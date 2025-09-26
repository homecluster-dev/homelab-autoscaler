/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package grpcserver

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

// MockCloudProviderServer implements the CloudProviderServer interface
type MockCloudProviderServer struct {
	pb.UnimplementedCloudProviderServer

	// GroupStore for managing Group resources
	groupStore *groupstore.GroupStore

	// Mock data storage (kept for compatibility with other methods)
	nodeGroups      map[string]*pb.NodeGroup
	nodeGroupSizes  map[string]int32
	nodes           map[string]*pb.ExternalGrpcNode
	nodeToNodeGroup map[string]string
	instances       map[string][]*pb.Instance
	gpuTypes        map[string]*anypb.Any
	pricingData     map[string]float64
}

// NewMockCloudProviderServer creates a new mock server with GroupStore integration
func NewMockCloudProviderServer(groupStore *groupstore.GroupStore) *MockCloudProviderServer {
	server := &MockCloudProviderServer{
		groupStore:      groupStore,
		nodeGroups:      make(map[string]*pb.NodeGroup),
		nodeGroupSizes:  make(map[string]int32),
		nodes:           make(map[string]*pb.ExternalGrpcNode),
		nodeToNodeGroup: make(map[string]string),
		instances:       make(map[string][]*pb.Instance),
		gpuTypes:        make(map[string]*anypb.Any),
		pricingData:     make(map[string]float64),
	}

	// Initialize with some default mock data to ensure basic functionality
	server.initializeMockData()

	return server
}

// initializeMockData sets up basic mock data for testing
func (s *MockCloudProviderServer) initializeMockData() {
	logger := log.Log.WithName("grpc-server")
	logger.Info("Initializing mock data for CloudProvider server")

	// Initialize some default GPU types
	s.gpuTypes["nvidia-tesla-v100"] = &anypb.Any{}
	s.gpuTypes["nvidia-tesla-t4"] = &anypb.Any{}

	// Initialize a default node group for testing
	defaultNodeGroup := &pb.NodeGroup{
		Id:      "ng-1",
		MinSize: 0,
		MaxSize: 5,
		Debug:   "Default test node group",
	}
	s.nodeGroups["ng-1"] = defaultNodeGroup
	s.nodeGroupSizes["ng-1"] = 2

	// Initialize some mock nodes and instances
	mockNode := &pb.ExternalGrpcNode{
		Name: "node-1",
	}
	s.nodes["node-1"] = mockNode
	s.nodeToNodeGroup["node-1"] = "ng-1"

	// Initialize some mock instances
	s.instances["ng-1"] = []*pb.Instance{
		{
			Id: "instance-1",
			Status: &pb.InstanceStatus{
				InstanceState: pb.InstanceStatus_instanceRunning,
				ErrorInfo: &pb.InstanceErrorInfo{
					ErrorCode:    "",
					ErrorMessage: "",
				},
			},
		},
	}

	logger.Info("Mock data initialization completed", "nodeGroups", len(s.nodeGroups), "gpuTypes", len(s.gpuTypes))
}

// NodeGroups returns all node groups from the GroupStore
func (s *MockCloudProviderServer) NodeGroups(ctx context.Context, req *pb.NodeGroupsRequest) (*pb.NodeGroupsResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroups called", "groupStoreAddr", fmt.Sprintf("%p", s.groupStore))

	// Check if context is already cancelled
	select {
	case <-ctx.Done():
		logger.Error(ctx.Err(), "Context cancelled before starting NodeGroups operation")
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	default:
	}

	// Get all groups from the GroupStore - this is a simple sync.Map iteration
	groups, err := s.groupStore.List()
	if err != nil {
		logger.Error(err, "failed to list groups from GroupStore")
		return nil, status.Errorf(codes.Internal, "failed to list groups: %v", err)
	}

	logger.Info("Retrieved groups from GroupStore", "count", len(groups))

	// Create NodeGroup messages for all groups
	nodeGroups := make([]*pb.NodeGroup, 0, len(groups))

	// Process each group - no health filtering, include all groups
	for _, group := range groups {
		// Check if context is cancelled before processing each group
		select {
		case <-ctx.Done():
			logger.Error(ctx.Err(), "Context cancelled while processing groups")
			return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded while processing groups: %v", ctx.Err())
		default:
		}

		// Create NodeGroup with: minSize=0, maxSize=GroupSpec.MaxSize, id=GroupSpec.Name
		nodeGroup := &pb.NodeGroup{
			Id:      group.Spec.Name,
			MinSize: 0,
			MaxSize: int32(group.Spec.MaxSize),
			Debug:   fmt.Sprintf("Group %s - MaxSize: %d", group.Spec.Name, group.Spec.MaxSize),
		}

		nodeGroups = append(nodeGroups, nodeGroup)
		logger.Info("Added group to NodeGroups response", "group", group.Name)
	}

	logger.Info("NodeGroups response completed", "totalGroups", len(nodeGroups))
	return &pb.NodeGroupsResponse{
		NodeGroups: nodeGroups,
	}, nil
}

// NodeGroupForNode returns the node group for the given node
func (s *MockCloudProviderServer) NodeGroupForNode(ctx context.Context, req *pb.NodeGroupForNodeRequest) (*pb.NodeGroupForNodeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupForNode called", "node", req.Node.Name)

	// Check if context is already cancelled
	select {
	case <-ctx.Done():
		logger.Error(ctx.Err(), "Context cancelled before starting NodeGroupForNode operation")
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	default:
	}

	// Get all groups from the GroupStore
	groups, err := s.groupStore.List()
	if err != nil {
		logger.Error(err, "failed to list groups from GroupStore")
		return nil, status.Errorf(codes.Internal, "failed to list groups: %v", err)
	}

	// Search for the node in all groups
	for _, group := range groups {
		// Check if context is cancelled before processing each group
		select {
		case <-ctx.Done():
			logger.Error(ctx.Err(), "Context cancelled while processing groups")
			return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded while processing groups: %v", ctx.Err())
		default:
		}

		// Check each node's KubernetesNodeName against the requested node name
		for _, nodeSpec := range group.Spec.NodesSpecs {
			if nodeSpec.KubernetesNodeName == req.Node.Name {
				// Found the node, return the associated group
				nodeGroup := &pb.NodeGroup{
					Id:      group.Spec.Name,
					MinSize: 0,
					MaxSize: int32(group.Spec.MaxSize),
					Debug:   fmt.Sprintf("Group %s - MaxSize: %d", group.Spec.Name, group.Spec.MaxSize),
				}

				logger.Info("Found node group for node", "node", req.Node.Name, "group", group.Spec.Name)
				return &pb.NodeGroupForNodeResponse{
					NodeGroup: nodeGroup,
				}, nil
			}
		}
	}

	// Node not found in any group, return empty string
	logger.Info("Node not found in any group", "node", req.Node.Name)
	return &pb.NodeGroupForNodeResponse{
		NodeGroup: &pb.NodeGroup{Id: ""},
	}, nil
}

// PricingNodePrice returns a theoretical minimum price of running a node
func (s *MockCloudProviderServer) PricingNodePrice(ctx context.Context, req *pb.PricingNodePriceRequest) (*pb.PricingNodePriceResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("PricingNodePrice called", "node", req.Node.Name)

	// This is an optional method - return Unimplemented
	return nil, status.Errorf(codes.Unimplemented, "PricingNodePrice not implemented")
}

// PricingPodPrice returns a theoretical minimum price of running a pod
func (s *MockCloudProviderServer) PricingPodPrice(ctx context.Context, req *pb.PricingPodPriceRequest) (*pb.PricingPodPriceResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("PricingPodPrice called", "pod", req.Pod.Name)

	// This is an optional method - return Unimplemented
	return nil, status.Errorf(codes.Unimplemented, "PricingPodPrice not implemented")
}

// GPULabel returns the label added to nodes with GPU resource
func (s *MockCloudProviderServer) GPULabel(ctx context.Context, req *pb.GPULabelRequest) (*pb.GPULabelResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("GPULabel called", "groupStoreAddr", fmt.Sprintf("%p", s.groupStore))

	// Simple health check for the service
	if s.groupStore == nil {
		logger.Error(nil, "GroupStore is nil in GPULabel call")
		return nil, status.Errorf(codes.Internal, "GroupStore not initialized")
	}

	return &pb.GPULabelResponse{
		Label: "accelerator",
	}, nil
}

// GetAvailableGPUTypes return all available GPU types cloud provider supports
func (s *MockCloudProviderServer) GetAvailableGPUTypes(ctx context.Context, req *pb.GetAvailableGPUTypesRequest) (*pb.GetAvailableGPUTypesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("GetAvailableGPUTypes called")

	return &pb.GetAvailableGPUTypesResponse{
		GpuTypes: s.gpuTypes,
	}, nil
}

// Cleanup cleans up open resources before the cloud provider is destroyed
func (s *MockCloudProviderServer) Cleanup(ctx context.Context, req *pb.CleanupRequest) (*pb.CleanupResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("Cleanup called", "groupStoreAddr", fmt.Sprintf("%p", s.groupStore))

	// Check if context is already cancelled
	select {
	case <-ctx.Done():
		logger.Error(ctx.Err(), "Context cancelled before starting Cleanup operation")
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	default:
	}

	// In a real implementation, this would clean up resources like:
	// - Delete all CronJobs associated with groups
	// - Clean up any external resources
	// - Clear caches and temporary data

	// For this mock implementation, we'll clear our local mock data
	// and log what would be cleaned up

	// Get all groups to log what would be cleaned up
	groups, err := s.groupStore.List()
	if err != nil {
		logger.Error(err, "failed to list groups for cleanup logging")
		// Continue with cleanup even if we can't list groups
	} else {
		logger.Info("Cleanup would remove resources for groups", "groupCount", len(groups))
		for _, group := range groups {
			logger.Info("Cleanup would remove group resources", "group", group.Name, "nodeCount", len(group.Spec.NodesSpecs))
		}
	}

	// Clear mock data
	s.nodeGroups = make(map[string]*pb.NodeGroup)
	s.nodeGroupSizes = make(map[string]int32)
	s.nodes = make(map[string]*pb.ExternalGrpcNode)
	s.nodeToNodeGroup = make(map[string]string)
	s.instances = make(map[string][]*pb.Instance)
	s.gpuTypes = make(map[string]*anypb.Any)
	s.pricingData = make(map[string]float64)

	logger.Info("Cleanup completed - mock data cleared")
	return &pb.CleanupResponse{}, nil
}

// Refresh is called before every main loop and can be used to dynamically update cloud provider state
func (s *MockCloudProviderServer) Refresh(ctx context.Context, req *pb.RefreshRequest) (*pb.RefreshResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("Refresh called")

	return &pb.RefreshResponse{}, nil
}

// NodeGroupTargetSize returns the current target size of the node group
func (s *MockCloudProviderServer) NodeGroupTargetSize(ctx context.Context, req *pb.NodeGroupTargetSizeRequest) (*pb.NodeGroupTargetSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupTargetSize called", "nodeGroup", req.Id)

	size, exists := s.nodeGroupSizes[req.Id]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "node group %s not found", req.Id)
	}

	return &pb.NodeGroupTargetSizeResponse{
		TargetSize: size,
	}, nil
}

// NodeGroupIncreaseSize increases the size of the node group
func (s *MockCloudProviderServer) NodeGroupIncreaseSize(ctx context.Context, req *pb.NodeGroupIncreaseSizeRequest) (*pb.NodeGroupIncreaseSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupIncreaseSize called", "nodeGroup", req.Id, "delta", req.Delta)

	nodeGroup, exists := s.nodeGroups[req.Id]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "node group %s not found", req.Id)
	}

	currentSize := s.nodeGroupSizes[req.Id]
	newSize := currentSize + req.Delta

	if newSize > nodeGroup.MaxSize {
		return nil, status.Errorf(codes.InvalidArgument, "new size %d exceeds max size %d", newSize, nodeGroup.MaxSize)
	}

	s.nodeGroupSizes[req.Id] = newSize

	// Simulate adding new instances
	for i := int32(0); i < req.Delta; i++ {
		instanceID := fmt.Sprintf("instance-%s-%d", req.Id, time.Now().UnixNano())
		s.instances[req.Id] = append(s.instances[req.Id], &pb.Instance{
			Id: instanceID,
			Status: &pb.InstanceStatus{
				InstanceState: pb.InstanceStatus_instanceCreating,
				ErrorInfo: &pb.InstanceErrorInfo{
					ErrorCode:    "",
					ErrorMessage: "",
				},
			},
		})
	}

	return &pb.NodeGroupIncreaseSizeResponse{}, nil
}

// NodeGroupDeleteNodes deletes nodes from this node group
func (s *MockCloudProviderServer) NodeGroupDeleteNodes(ctx context.Context, req *pb.NodeGroupDeleteNodesRequest) (*pb.NodeGroupDeleteNodesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupDeleteNodes called", "nodeGroup", req.Id, "nodes", len(req.Nodes))

	_, exists := s.nodeGroups[req.Id]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "node group %s not found", req.Id)
	}

	// Remove nodes from the node group
	for _, node := range req.Nodes {
		delete(s.nodeToNodeGroup, node.Name)
		delete(s.nodes, node.Name)
	}

	// Update node group size
	currentSize := s.nodeGroupSizes[req.Id]
	newSize := currentSize - int32(len(req.Nodes))
	if newSize < 0 {
		newSize = 0
	}
	s.nodeGroupSizes[req.Id] = newSize

	// Remove instances
	instances := s.instances[req.Id]
	newInstances := make([]*pb.Instance, 0, len(instances)-len(req.Nodes))
	for i, instance := range instances {
		if i < len(instances)-len(req.Nodes) {
			newInstances = append(newInstances, instance)
		}
	}
	s.instances[req.Id] = newInstances

	return &pb.NodeGroupDeleteNodesResponse{}, nil
}

// NodeGroupDecreaseTargetSize decreases the target size of the node group
func (s *MockCloudProviderServer) NodeGroupDecreaseTargetSize(ctx context.Context, req *pb.NodeGroupDecreaseTargetSizeRequest) (*pb.NodeGroupDecreaseTargetSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupDecreaseTargetSize called", "nodeGroup", req.Id, "delta", req.Delta)

	nodeGroup, exists := s.nodeGroups[req.Id]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "node group %s not found", req.Id)
	}

	if req.Delta >= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta must be negative, got %d", req.Delta)
	}

	currentSize := s.nodeGroupSizes[req.Id]
	newSize := currentSize + req.Delta // req.Delta is negative

	if newSize < nodeGroup.MinSize {
		return nil, status.Errorf(codes.InvalidArgument, "new size %d is below min size %d", newSize, nodeGroup.MinSize)
	}

	s.nodeGroupSizes[req.Id] = newSize

	return &pb.NodeGroupDecreaseTargetSizeResponse{}, nil
}

// NodeGroupNodes returns a list of all nodes that belong to this node group
func (s *MockCloudProviderServer) NodeGroupNodes(ctx context.Context, req *pb.NodeGroupNodesRequest) (*pb.NodeGroupNodesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupNodes called", "nodeGroup", req.Id)

	instances, exists := s.instances[req.Id]
	if !exists {
		return &pb.NodeGroupNodesResponse{
			Instances: []*pb.Instance{},
		}, nil
	}

	return &pb.NodeGroupNodesResponse{
		Instances: instances,
	}, nil
}

// NodeGroupTemplateNodeInfo returns a structure of an empty node
func (s *MockCloudProviderServer) NodeGroupTemplateNodeInfo(ctx context.Context, req *pb.NodeGroupTemplateNodeInfoRequest) (*pb.NodeGroupTemplateNodeInfoResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupTemplateNodeInfo called", "nodeGroup", req.Id)

	// This is an optional method - return Unimplemented
	return nil, status.Errorf(codes.Unimplemented, "NodeGroupTemplateNodeInfo not implemented")
}

// NodeGroupGetOptions returns NodeGroupAutoscalingOptions for the node group
func (s *MockCloudProviderServer) NodeGroupGetOptions(ctx context.Context, req *pb.NodeGroupAutoscalingOptionsRequest) (*pb.NodeGroupAutoscalingOptionsResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupGetOptions called", "nodeGroup", req.Id)

	// This is an optional method - return Unimplemented
	return nil, status.Errorf(codes.Unimplemented, "NodeGroupGetOptions not implemented")
}
