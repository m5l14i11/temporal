// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package frontend

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/exp/maps"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	namespacepb "go.temporal.io/api/namespace/v1"
	"go.temporal.io/api/operatorservice/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"

	"go.temporal.io/server/api/adminservice/v1"
	"go.temporal.io/server/api/historyservice/v1"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	svc "go.temporal.io/server/client"
	"go.temporal.io/server/client/admin"
	"go.temporal.io/server/client/frontend"
	"go.temporal.io/server/common"
	clustermetadata "go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/visibility/manager"
	"go.temporal.io/server/common/persistence/visibility/store/elasticsearch"
	esclient "go.temporal.io/server/common/persistence/visibility/store/elasticsearch/client"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/searchattribute"
	"go.temporal.io/server/common/util"
	"go.temporal.io/server/service/worker"
	"go.temporal.io/server/service/worker/addsearchattributes"
	"go.temporal.io/server/service/worker/deletenamespace"
	"go.temporal.io/server/service/worker/deletenamespace/deleteexecutions"
)

var _ OperatorHandler = (*OperatorHandlerImpl)(nil)

type (
	// OperatorHandlerImpl - gRPC handler interface for operator service
	OperatorHandlerImpl struct {
		status int32

		logger                 log.Logger
		config                 *Config
		esClient               esclient.Client
		sdkClientFactory       sdk.ClientFactory
		metricsHandler         metrics.Handler
		visibilityMgr          manager.VisibilityManager
		saProvider             searchattribute.Provider
		saManager              searchattribute.Manager
		healthServer           *health.Server
		historyClient          historyservice.HistoryServiceClient
		namespaceRegistry      namespace.Registry
		clusterMetadataManager persistence.ClusterMetadataManager
		clusterMetadata        clustermetadata.Metadata
		clientFactory          svc.Factory
	}

	NewOperatorHandlerImplArgs struct {
		config                 *Config
		EsClient               esclient.Client
		Logger                 log.Logger
		sdkClientFactory       sdk.ClientFactory
		MetricsHandler         metrics.Handler
		VisibilityMgr          manager.VisibilityManager
		SaProvider             searchattribute.Provider
		SaManager              searchattribute.Manager
		healthServer           *health.Server
		historyClient          historyservice.HistoryServiceClient
		namespaceRegistry      namespace.Registry
		clusterMetadataManager persistence.ClusterMetadataManager
		clusterMetadata        clustermetadata.Metadata
		clientFactory          svc.Factory
	}
)

// NewOperatorHandlerImpl creates a gRPC handler for operatorservice
func NewOperatorHandlerImpl(
	args NewOperatorHandlerImplArgs,
) *OperatorHandlerImpl {

	handler := &OperatorHandlerImpl{
		logger:                 args.Logger,
		status:                 common.DaemonStatusInitialized,
		config:                 args.config,
		esClient:               args.EsClient,
		sdkClientFactory:       args.sdkClientFactory,
		metricsHandler:         args.MetricsHandler,
		visibilityMgr:          args.VisibilityMgr,
		saProvider:             args.SaProvider,
		saManager:              args.SaManager,
		healthServer:           args.healthServer,
		historyClient:          args.historyClient,
		namespaceRegistry:      args.namespaceRegistry,
		clusterMetadataManager: args.clusterMetadataManager,
		clusterMetadata:        args.clusterMetadata,
		clientFactory:          args.clientFactory,
	}

	return handler
}

// Start starts the handler
func (h *OperatorHandlerImpl) Start() {
	if atomic.CompareAndSwapInt32(
		&h.status,
		common.DaemonStatusInitialized,
		common.DaemonStatusStarted,
	) {
		h.healthServer.SetServingStatus(OperatorServiceName, healthpb.HealthCheckResponse_SERVING)
	}
}

// Stop stops the handler
func (h *OperatorHandlerImpl) Stop() {
	if atomic.CompareAndSwapInt32(
		&h.status,
		common.DaemonStatusStarted,
		common.DaemonStatusStopped,
	) {
		h.healthServer.SetServingStatus(OperatorServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	}
}

func (h *OperatorHandlerImpl) AddSearchAttributes(
	ctx context.Context,
	request *operatorservice.AddSearchAttributesRequest,
) (_ *operatorservice.AddSearchAttributesResponse, retError error) {
	defer log.CapturePanic(h.logger, &retError)

	scope, startTime := h.startRequestProfile(metrics.OperatorAddSearchAttributesScope)
	defer func() { scope.Timer(metrics.ServiceLatency.GetMetricName()).Record(time.Since(startTime)) }()

	// validate request
	if request == nil {
		return nil, errRequestNotSet
	}

	if len(request.GetSearchAttributes()) == 0 {
		return nil, errSearchAttributesNotSet
	}

	indexName := h.visibilityMgr.GetIndexName()
	currentSearchAttributes, err := h.saProvider.GetSearchAttributes(indexName, true)
	if err != nil {
		return nil, serviceerror.NewUnavailable(fmt.Sprintf(errUnableToGetSearchAttributesMessage, err))
	}

	for saName, saType := range request.GetSearchAttributes() {
		if searchattribute.IsReserved(saName) {
			return nil, serviceerror.NewInvalidArgument(fmt.Sprintf(errSearchAttributeIsReservedMessage, saName))
		}
		if currentSearchAttributes.IsDefined(saName) {
			return nil, serviceerror.NewAlreadyExist(fmt.Sprintf(errSearchAttributeAlreadyExistsMessage, saName))
		}
		if _, ok := enumspb.IndexedValueType_name[int32(saType)]; !ok {
			return nil, serviceerror.NewInvalidArgument(fmt.Sprintf(errUnknownSearchAttributeTypeMessage, saType))
		}
	}

	if h.visibilityMgr.GetName() == elasticsearch.PersistenceName {
		err = h.addSearchAttributesElasticsearch(ctx, request, indexName)
	} else {
		err = h.addSearchAttributesSQL(ctx, request, currentSearchAttributes)
	}

	if err != nil {
		scope.Counter(metrics.AddSearchAttributesWorkflowFailuresCount.GetMetricName()).Record(1)
		return nil, err
	}
	scope.Counter(metrics.AddSearchAttributesWorkflowSuccessCount.GetMetricName()).Record(1)
	return &operatorservice.AddSearchAttributesResponse{}, nil
}

func (h *OperatorHandlerImpl) addSearchAttributesElasticsearch(
	ctx context.Context,
	request *operatorservice.AddSearchAttributesRequest,
	indexName string,
) error {
	// Execute workflow.
	wfParams := addsearchattributes.WorkflowParams{
		CustomAttributesToAdd: request.GetSearchAttributes(),
		IndexName:             indexName,
		SkipSchemaUpdate:      false,
	}

	sdkClient := h.sdkClientFactory.GetSystemClient()
	run, err := sdkClient.ExecuteWorkflow(
		ctx,
		sdkclient.StartWorkflowOptions{
			TaskQueue: worker.DefaultWorkerTaskQueue,
			ID:        addsearchattributes.WorkflowName,
		},
		addsearchattributes.WorkflowName,
		wfParams,
	)
	if err != nil {
		return serviceerror.NewUnavailable(
			fmt.Sprintf(errUnableToStartWorkflowMessage, addsearchattributes.WorkflowName, err),
		)
	}

	// Wait for workflow to complete.
	err = run.Get(ctx, nil)
	if err != nil {
		execution := &commonpb.WorkflowExecution{
			WorkflowId: addsearchattributes.WorkflowName,
			RunId:      run.GetRunID(),
		}
		return serviceerror.NewSystemWorkflow(execution, err)
	}
	return nil
}

func (h *OperatorHandlerImpl) addSearchAttributesSQL(
	ctx context.Context,
	request *operatorservice.AddSearchAttributesRequest,
	currentSearchAttributes searchattribute.NameTypeMap,
) error {
	client, err := h.clientFactory.NewLocalFrontendClientWithTimeout(
		frontend.DefaultTimeout,
		frontend.DefaultLongPollTimeout,
	)
	if err != nil {
		return serviceerror.NewUnavailable(fmt.Sprintf(errUnableToCreateFrontendClientMessage, err))
	}

	nsName := request.GetNamespace()
	if nsName == "" {
		return errNamespaceNotSet
	}
	ns, err := h.namespaceRegistry.GetNamespace(namespace.Name(nsName))
	if err != nil {
		return serviceerror.NewUnavailable(fmt.Sprintf(errUnableToGetNamespaceInfoMessage, nsName))
	}

	customSearchAttributes := currentSearchAttributes.Custom()
	mapper := ns.CustomSearchAttributesMapper()
	fieldToAliasMap := util.CloneMapNonNil(mapper.FieldToAliasMap())
	for saName, saType := range request.GetSearchAttributes() {
		// check if alias is already in use
		if _, err := mapper.GetFieldName(saName, nsName); err == nil {
			return serviceerror.NewAlreadyExist(
				fmt.Sprintf(errSearchAttributeAlreadyExistsMessage, saName),
			)
		}
		// find the first available field for the given type
		targetFieldName := ""
		cntUsed := 0
		for fieldName, fieldType := range customSearchAttributes {
			if fieldType != saType {
				continue
			}
			if _, ok := fieldToAliasMap[fieldName]; !ok {
				targetFieldName = fieldName
				break
			}
			cntUsed++
		}
		if targetFieldName == "" {
			return serviceerror.NewInvalidArgument(
				fmt.Sprintf(errTooManySearchAttributesMessage, cntUsed, saType.String()),
			)
		}
		fieldToAliasMap[targetFieldName] = saName
	}

	_, err = client.UpdateNamespace(ctx, &workflowservice.UpdateNamespaceRequest{
		Namespace: nsName,
		Config: &namespacepb.NamespaceConfig{
			CustomSearchAttributeAliases: fieldToAliasMap,
		},
	})
	return err
}

func (h *OperatorHandlerImpl) RemoveSearchAttributes(
	ctx context.Context,
	request *operatorservice.RemoveSearchAttributesRequest,
) (_ *operatorservice.RemoveSearchAttributesResponse, retError error) {
	defer log.CapturePanic(h.logger, &retError)

	// validate request
	if request == nil {
		return nil, errRequestNotSet
	}

	if len(request.GetSearchAttributes()) == 0 {
		return nil, errSearchAttributesNotSet
	}

	var err error
	indexName := h.visibilityMgr.GetIndexName()
	if h.visibilityMgr.GetName() == elasticsearch.PersistenceName {
		err = h.removeSearchAttributesElasticsearch(ctx, request, indexName)
	} else {
		err = h.removeSearchAttributesSQL(ctx, request)
	}

	if err != nil {
		return nil, err
	}
	return &operatorservice.RemoveSearchAttributesResponse{}, nil
}

func (h *OperatorHandlerImpl) removeSearchAttributesElasticsearch(
	ctx context.Context,
	request *operatorservice.RemoveSearchAttributesRequest,
	indexName string,
) error {
	currentSearchAttributes, err := h.saProvider.GetSearchAttributes(indexName, true)
	if err != nil {
		return serviceerror.NewUnavailable(fmt.Sprintf(errUnableToGetSearchAttributesMessage, err))
	}

	newCustomSearchAttributes := maps.Clone(currentSearchAttributes.Custom())
	for _, saName := range request.GetSearchAttributes() {
		if !currentSearchAttributes.IsDefined(saName) {
			return serviceerror.NewNotFound(fmt.Sprintf(errSearchAttributeDoesntExistMessage, saName))
		}
		if _, ok := newCustomSearchAttributes[saName]; !ok {
			return serviceerror.NewInvalidArgument(
				fmt.Sprintf(errUnableToRemoveNonCustomSearchAttributesMessage, saName),
			)
		}
		delete(newCustomSearchAttributes, saName)
	}

	err = h.saManager.SaveSearchAttributes(ctx, indexName, newCustomSearchAttributes)
	if err != nil {
		return serviceerror.NewUnavailable(fmt.Sprintf(errUnableToSaveSearchAttributesMessage, err))
	}
	return nil
}

func (h *OperatorHandlerImpl) removeSearchAttributesSQL(
	ctx context.Context,
	request *operatorservice.RemoveSearchAttributesRequest,
) error {
	client, err := h.clientFactory.NewLocalFrontendClientWithTimeout(
		frontend.DefaultTimeout,
		frontend.DefaultLongPollTimeout,
	)
	if err != nil {
		return serviceerror.NewUnavailable(fmt.Sprintf(errUnableToCreateFrontendClientMessage, err))
	}

	nsName := request.GetNamespace()
	if nsName == "" {
		return errNamespaceNotSet
	}
	ns, err := h.namespaceRegistry.GetNamespace(namespace.Name(nsName))
	if err != nil {
		return serviceerror.NewUnavailable(fmt.Sprintf(errUnableToGetNamespaceInfoMessage, nsName))
	}

	mapper := ns.CustomSearchAttributesMapper()
	fieldToAliasMap := maps.Clone(mapper.FieldToAliasMap())
	for _, saName := range request.GetSearchAttributes() {
		fieldName, err := mapper.GetFieldName(saName, nsName)
		if err != nil {
			return serviceerror.NewNotFound(fmt.Sprintf(errSearchAttributeDoesntExistMessage, saName))
		}
		delete(fieldToAliasMap, fieldName)
	}

	_, err = client.UpdateNamespace(ctx, &workflowservice.UpdateNamespaceRequest{
		Namespace: nsName,
		Config: &namespacepb.NamespaceConfig{
			CustomSearchAttributeAliases: fieldToAliasMap,
		},
	})
	return err
}

func (h *OperatorHandlerImpl) ListSearchAttributes(
	ctx context.Context,
	request *operatorservice.ListSearchAttributesRequest,
) (_ *operatorservice.ListSearchAttributesResponse, retError error) {
	defer log.CapturePanic(h.logger, &retError)

	if request == nil {
		return nil, errRequestNotSet
	}

	indexName := h.visibilityMgr.GetIndexName()
	searchAttributes, err := h.saProvider.GetSearchAttributes(indexName, true)
	if err != nil {
		return nil, serviceerror.NewUnavailable(
			fmt.Sprintf("unable to read custom search attributes: %v", err),
		)
	}

	if h.visibilityMgr.GetName() == elasticsearch.PersistenceName {
		return h.listSearchAttributesElasticsearch(ctx, indexName, searchAttributes)
	}
	return h.listSearchAttributesSQL(request, searchAttributes)
}

func (h *OperatorHandlerImpl) listSearchAttributesElasticsearch(
	ctx context.Context,
	indexName string,
	searchAttributes searchattribute.NameTypeMap,
) (*operatorservice.ListSearchAttributesResponse, error) {
	var storageSchema map[string]string
	if h.esClient != nil {
		var err error
		storageSchema, err = h.esClient.GetMapping(ctx, indexName)
		if err != nil {
			return nil, serviceerror.NewUnavailable(
				fmt.Sprintf("unable to get mapping from Elasticsearch: %v", err),
			)
		}
	}
	return &operatorservice.ListSearchAttributesResponse{
		CustomAttributes: searchAttributes.Custom(),
		SystemAttributes: searchAttributes.System(),
		StorageSchema:    storageSchema,
	}, nil
}

func (h *OperatorHandlerImpl) listSearchAttributesSQL(
	request *operatorservice.ListSearchAttributesRequest,
	searchAttributes searchattribute.NameTypeMap,
) (*operatorservice.ListSearchAttributesResponse, error) {
	nsName := request.GetNamespace()
	if nsName == "" {
		return nil, errNamespaceNotSet
	}
	ns, err := h.namespaceRegistry.GetNamespace(namespace.Name(nsName))
	if err != nil {
		return nil, serviceerror.NewUnavailable(
			fmt.Sprintf(errUnableToGetNamespaceInfoMessage, nsName),
		)
	}
	mapper := ns.CustomSearchAttributesMapper()
	customSearchAttributes := make(map[string]enumspb.IndexedValueType)
	for field, tp := range searchAttributes.Custom() {
		if alias, err := mapper.GetAlias(field, nsName); err == nil {
			customSearchAttributes[alias] = tp
		}
	}
	return &operatorservice.ListSearchAttributesResponse{
		CustomAttributes: customSearchAttributes,
		SystemAttributes: searchAttributes.System(),
		StorageSchema:    nil,
	}, nil
}

func (h *OperatorHandlerImpl) DeleteNamespace(
	ctx context.Context,
	request *operatorservice.DeleteNamespaceRequest,
) (_ *operatorservice.DeleteNamespaceResponse, retError error) {
	defer log.CapturePanic(h.logger, &retError)

	scope, startTime := h.startRequestProfile(metrics.OperatorDeleteNamespaceScope)
	defer func() { scope.Timer(metrics.ServiceLatency.GetMetricName()).Record(time.Since(startTime)) }()

	// validate request
	if request == nil {
		return nil, errRequestNotSet
	}

	if request.GetNamespace() == primitives.SystemLocalNamespace {
		return nil, errUnableDeleteSystemNamespace
	}

	// Execute workflow.
	wfParams := deletenamespace.DeleteNamespaceWorkflowParams{
		Namespace: namespace.Name(request.GetNamespace()),
		DeleteExecutionsConfig: deleteexecutions.DeleteExecutionsConfig{
			DeleteActivityRPS:                    h.config.DeleteNamespaceDeleteActivityRPS(),
			PageSize:                             h.config.DeleteNamespacePageSize(),
			PagesPerExecution:                    h.config.DeleteNamespacePagesPerExecution(),
			ConcurrentDeleteExecutionsActivities: h.config.DeleteNamespaceConcurrentDeleteExecutionsActivities(),
		},
		NamespaceDeleteDelay: h.config.DeleteNamespaceNamespaceDeleteDelay(),
	}

	sdkClient := h.sdkClientFactory.GetSystemClient()
	run, err := sdkClient.ExecuteWorkflow(
		ctx,
		sdkclient.StartWorkflowOptions{
			TaskQueue: worker.DefaultWorkerTaskQueue,
			ID:        fmt.Sprintf("%s/%s", deletenamespace.WorkflowName, request.GetNamespace()),
		},
		deletenamespace.WorkflowName,
		wfParams,
	)
	if err != nil {
		return nil, serviceerror.NewUnavailable(fmt.Sprintf(errUnableToStartWorkflowMessage, deletenamespace.WorkflowName, err))
	}

	// Wait for workflow to complete.
	var wfResult deletenamespace.DeleteNamespaceWorkflowResult
	err = run.Get(ctx, &wfResult)
	if err != nil {
		scope.Counter(metrics.DeleteNamespaceWorkflowFailuresCount.GetMetricName()).Record(1)
		execution := &commonpb.WorkflowExecution{WorkflowId: deletenamespace.WorkflowName, RunId: run.GetRunID()}
		return nil, serviceerror.NewSystemWorkflow(execution, err)
	}
	scope.Counter(metrics.DeleteNamespaceWorkflowSuccessCount.GetMetricName()).Record(1)

	return &operatorservice.DeleteNamespaceResponse{
		DeletedNamespace: wfResult.DeletedNamespace.String(),
	}, nil
}

// AddOrUpdateRemoteCluster adds or updates the connection config to a remote cluster.
func (h *OperatorHandlerImpl) AddOrUpdateRemoteCluster(
	ctx context.Context,
	request *operatorservice.AddOrUpdateRemoteClusterRequest,
) (_ *operatorservice.AddOrUpdateRemoteClusterResponse, retError error) {
	defer log.CapturePanic(h.logger, &retError)
	scope, startTime := h.startRequestProfile(metrics.OperatorAddOrUpdateRemoteClusterScope)
	defer func() { scope.Timer(metrics.ServiceLatency.GetMetricName()).Record(time.Since(startTime)) }()

	adminClient := h.clientFactory.NewRemoteAdminClientWithTimeout(
		request.GetFrontendAddress(),
		admin.DefaultTimeout,
		admin.DefaultLargeTimeout,
	)

	// Fetch cluster metadata from remote cluster
	resp, err := adminClient.DescribeCluster(ctx, &adminservice.DescribeClusterRequest{})
	if err != nil {
		scope.Counter(metrics.ServiceFailures.GetMetricName()).Record(1)
		return nil, serviceerror.NewUnavailable(fmt.Sprintf(
			errUnableConnectRemoteClusterMessage,
			request.GetFrontendAddress(),
			err,
		))
	}

	err = h.validateRemoteClusterMetadata(resp)
	if err != nil {
		scope.Counter(metrics.ServiceFailures.GetMetricName()).Record(1)
		return nil, serviceerror.NewInvalidArgument(fmt.Sprintf(errInvalidRemoteClusterInfo, err))
	}

	var updateRequestVersion int64 = 0
	clusterData, err := h.clusterMetadataManager.GetClusterMetadata(
		ctx,
		&persistence.GetClusterMetadataRequest{ClusterName: resp.GetClusterName()},
	)
	switch err.(type) {
	case nil:
		updateRequestVersion = clusterData.Version
	case *serviceerror.NotFound:
		updateRequestVersion = 0
	default:
		scope.Counter(metrics.ServiceFailures.GetMetricName()).Record(1)
		return nil, serviceerror.NewInternal(fmt.Sprintf(errUnableToStoreClusterInfo, err))
	}

	applied, err := h.clusterMetadataManager.SaveClusterMetadata(ctx, &persistence.SaveClusterMetadataRequest{
		ClusterMetadata: persistencespb.ClusterMetadata{
			ClusterName:              resp.GetClusterName(),
			HistoryShardCount:        resp.GetHistoryShardCount(),
			ClusterId:                resp.GetClusterId(),
			ClusterAddress:           request.GetFrontendAddress(),
			FailoverVersionIncrement: resp.GetFailoverVersionIncrement(),
			InitialFailoverVersion:   resp.GetInitialFailoverVersion(),
			IsGlobalNamespaceEnabled: resp.GetIsGlobalNamespaceEnabled(),
			IsConnectionEnabled:      request.GetEnableRemoteClusterConnection(),
		},
		Version: updateRequestVersion,
	})
	if err != nil {
		scope.Counter(metrics.ServiceFailures.GetMetricName()).Record(1)
		return nil, serviceerror.NewInternal(fmt.Sprintf(errUnableToStoreClusterInfo, err))
	}
	if !applied {
		scope.Counter(metrics.ServiceFailures.GetMetricName()).Record(1)
		return nil, serviceerror.NewInvalidArgument(fmt.Sprintf(errUnableToStoreClusterInfo, err))
	}
	return &operatorservice.AddOrUpdateRemoteClusterResponse{}, nil
}

func (h *OperatorHandlerImpl) RemoveRemoteCluster(
	ctx context.Context,
	request *operatorservice.RemoveRemoteClusterRequest,
) (_ *operatorservice.RemoveRemoteClusterResponse, retError error) {
	defer log.CapturePanic(h.logger, &retError)
	scope, startTime := h.startRequestProfile(metrics.OperatorRemoveRemoteClusterScope)
	defer func() { scope.Timer(metrics.ServiceLatency.GetMetricName()).Record(time.Since(startTime)) }()

	var isClusterNameExist bool
	for clusterName := range h.clusterMetadata.GetAllClusterInfo() {
		if clusterName == request.GetClusterName() {
			isClusterNameExist = true
			break
		}
	}
	if !isClusterNameExist {
		return nil, serviceerror.NewNotFound("The cluster to be deleted cannot be found in clusters cache.")
	}

	if err := h.clusterMetadataManager.DeleteClusterMetadata(
		ctx,
		&persistence.DeleteClusterMetadataRequest{ClusterName: request.GetClusterName()},
	); err != nil {
		scope.Counter(metrics.ServiceFailures.GetMetricName()).Record(1)
		return nil, serviceerror.NewInternal(fmt.Sprintf(errUnableToDeleteClusterInfo, err))
	}
	return &operatorservice.RemoveRemoteClusterResponse{}, nil
}

func (h *OperatorHandlerImpl) ListClusters(
	ctx context.Context,
	request *operatorservice.ListClustersRequest,
) (_ *operatorservice.ListClustersResponse, retError error) {
	defer log.CapturePanic(h.logger, &retError)
	scope, startTime := h.startRequestProfile(metrics.OperatorListClustersScope)
	defer func() { scope.Timer(metrics.ServiceLatency.GetMetricName()).Record(time.Since(startTime)) }()

	if request == nil {
		return nil, errRequestNotSet
	}
	if request.GetPageSize() <= 0 {
		request.PageSize = listClustersPageSize
	}

	resp, err := h.clusterMetadataManager.ListClusterMetadata(ctx, &persistence.ListClusterMetadataRequest{
		PageSize:      int(request.GetPageSize()),
		NextPageToken: request.GetNextPageToken(),
	})
	if err != nil {
		scope.Counter(metrics.ServiceFailures.GetMetricName()).Record(1)
		return nil, err
	}

	var clusterMetadataList []*operatorservice.ClusterMetadata
	for _, clusterResp := range resp.ClusterMetadata {
		clusterMetadataList = append(clusterMetadataList, &operatorservice.ClusterMetadata{
			ClusterName:            clusterResp.GetClusterName(),
			ClusterId:              clusterResp.GetClusterId(),
			Address:                clusterResp.GetClusterAddress(),
			InitialFailoverVersion: clusterResp.GetInitialFailoverVersion(),
			HistoryShardCount:      clusterResp.GetHistoryShardCount(),
			IsConnectionEnabled:    clusterResp.GetIsConnectionEnabled(),
		})
	}
	return &operatorservice.ListClustersResponse{
		Clusters:      clusterMetadataList,
		NextPageToken: resp.NextPageToken,
	}, nil
}

func (h *OperatorHandlerImpl) validateRemoteClusterMetadata(metadata *adminservice.DescribeClusterResponse) error {
	// Verify remote cluster config
	currentClusterInfo := h.clusterMetadata
	if metadata.GetClusterName() == currentClusterInfo.GetCurrentClusterName() {
		// cluster name conflict
		return serviceerror.NewInvalidArgument("Cannot update current cluster metadata from rpc calls")
	}
	if metadata.GetFailoverVersionIncrement() != currentClusterInfo.GetFailoverVersionIncrement() {
		// failover version increment is mismatch with current cluster config
		return serviceerror.NewInvalidArgument("Cannot add remote cluster due to failover version increment mismatch")
	}
	if metadata.GetHistoryShardCount() != h.config.NumHistoryShards {
		remoteShardCount := metadata.GetHistoryShardCount()
		large := remoteShardCount
		small := h.config.NumHistoryShards
		if large < small {
			small, large = large, small
		}
		if large%small != 0 {
			return serviceerror.NewInvalidArgument("Remote cluster shard number and local cluster shard number are not multiples.")
		}
	}
	if !metadata.IsGlobalNamespaceEnabled {
		// remote cluster doesn't support global namespace
		return serviceerror.NewInvalidArgument("Cannot add remote cluster as global namespace is not supported")
	}
	for clusterName, cluster := range currentClusterInfo.GetAllClusterInfo() {
		if clusterName != metadata.ClusterName && cluster.InitialFailoverVersion == metadata.GetInitialFailoverVersion() {
			// initial failover version conflict
			// best effort: race condition if a concurrent write to db with the same version.
			return serviceerror.NewInvalidArgument("Cannot add remote cluster due to initial failover version conflict")
		}
	}
	return nil
}

// startRequestProfile initiates recording of request metrics
func (h *OperatorHandlerImpl) startRequestProfile(operation string) (metrics.Handler, time.Time) {
	metricsScope := h.metricsHandler.WithTags(metrics.OperationTag(operation))
	metricsScope.Counter(metrics.ServiceRequests.GetMetricName()).Record(1)
	return metricsScope, time.Now().UTC()
}
