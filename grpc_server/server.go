/*
Copyright 2022 The Kubernetes Authors.

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

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

// MockCloudProviderServer implements the CloudProviderServer interface
type MockCloudProviderServer struct {
	pb.UnimplementedCloudProviderServer
	mu sync.RWMutex

	// Mock data storage
	nodeGroups       map[string]*pb.NodeGroup
	nodeGroupSizes   map[string]int32
	nodes            map[string]*pb.ExternalGrpcNode
	nodeToNodeGroup  map[string]string
	instances        map[string][]*pb.Instance
	gpuTypes         map[string]*anypb.Any
	pricingData      map[string]float64
}

// NewMockCloudProviderServer creates a new mock server with initialized data
func NewMockCloudProviderServer() *MockCloudProviderServer {
	server := &MockCloudProviderServer{
		nodeGroups:      make(map[string]*pb.NodeGroup),
		nodeGroupSizes:  make(map[string]int32),
		nodes:           make(map[string]*pb.ExternalGrpcNode),
		nodeToNodeGroup: make(map[string]string),
		instances:       make(map[string][]*pb.Instance),
		gpuTypes:        make(map[string]*anypb.Any),
		pricingData:     make(map[string]float64),
	}
	server.initializeMockData()
	return server
}

// initializeMockData sets up realistic mock data for testing
func (s *MockCloudProviderServer) initializeMockData() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create mock node groups
	s.nodeGroups["ng-1"] = &pb.NodeGroup{
		Id:      "ng-1",
		MinSize: 1,
		MaxSize: 10,
		Debug:   "Mock node group 1 - Standard compute",
	}
	s.nodeGroups["ng-2"] = &pb.NodeGroup{
		Id:      "ng-2",
		MinSize: 0,
		MaxSize: 5,
		Debug:   "Mock node group 2 - GPU enabled",
	}
	s.nodeGroups["ng-3"] = &pb.NodeGroup{
		Id:      "ng-3",
		MinSize: 2,
		MaxSize: 20,
		Debug:   "Mock node group 3 - High memory",
	}

	// Initialize node group sizes
	s.nodeGroupSizes["ng-1"] = 3
	s.nodeGroupSizes["ng-2"] = 1
	s.nodeGroupSizes["ng-3"] = 5

	// Create mock nodes
	s.nodes["node-1"] = &pb.ExternalGrpcNode{
		ProviderID: "mock://node-1",
		Name:       "node-1",
		Labels: map[string]string{
			"node.kubernetes.io/instance-type": "standard-2",
			"topology.kubernetes.io/zone":      "us-west-1a",
			"mock.io/nodegroup":                "ng-1",
		},
		Annotations: map[string]string{
			"mock.io/created-at": time.Now().Format(time.RFC3339),
		},
	}
	s.nodes["node-2"] = &pb.ExternalGrpcNode{
		ProviderID: "mock://node-2",
		Name:       "node-2",
		Labels: map[string]string{
			"node.kubernetes.io/instance-type": "standard-4",
			"topology.kubernetes.io/zone":      "us-west-1b",
			"mock.io/nodegroup":                "ng-1",
		},
		Annotations: map[string]string{
			"mock.io/created-at": time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		},
	}
	s.nodes["node-3"] = &pb.ExternalGrpcNode{
		ProviderID: "mock://node-3",
		Name:       "node-3",
		Labels: map[string]string{
			"node.kubernetes.io/instance-type": "gpu-v100",
			"topology.kubernetes.io/zone":      "us-west-1a",
			"mock.io/nodegroup":                "ng-2",
			"accelerator":                      "nvidia-tesla-v100",
		},
		Annotations: map[string]string{
			"mock.io/created-at": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
		},
	}

	// Map nodes to node groups
	s.nodeToNodeGroup["node-1"] = "ng-1"
	s.nodeToNodeGroup["node-2"] = "ng-1"
	s.nodeToNodeGroup["node-3"] = "ng-2"

	// Create mock instances
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
		{
			Id: "instance-2",
			Status: &pb.InstanceStatus{
				InstanceState: pb.InstanceStatus_instanceRunning,
				ErrorInfo: &pb.InstanceErrorInfo{
					ErrorCode:    "",
					ErrorMessage: "",
				},
			},
		},
	}
	s.instances["ng-2"] = []*pb.Instance{
		{
			Id: "instance-3",
			Status: &pb.InstanceStatus{
				InstanceState: pb.InstanceStatus_instanceRunning,
				ErrorInfo: &pb.InstanceErrorInfo{
					ErrorCode:    "",
					ErrorMessage: "",
				},
			},
		},
	}
	s.instances["ng-3"] = []*pb.Instance{
		{
			Id: "instance-4",
			Status: &pb.InstanceStatus{
				InstanceState: pb.InstanceStatus_instanceRunning,
				ErrorInfo: &pb.InstanceErrorInfo{
					ErrorCode:    "",
					ErrorMessage: "",
				},
			},
		},
		{
			Id: "instance-5",
			Status: &pb.InstanceStatus{
				InstanceState: pb.InstanceStatus_instanceRunning,
				ErrorInfo: &pb.InstanceErrorInfo{
					ErrorCode:    "",
					ErrorMessage: "",
				},
			},
		},
		{
			Id: "instance-6",
			Status: &pb.InstanceStatus{
				InstanceState: pb.InstanceStatus_instanceCreating,
				ErrorInfo: &pb.InstanceErrorInfo{
					ErrorCode:    "",
					ErrorMessage: "",
				},
			},
		},
	}

	// Create mock GPU types
	gpuData1, _ := anypb.New(&anypb.Any{
		TypeUrl: "type.googleapis.com/mock.GPUType",
		Value:   []byte(`{"name": "nvidia-tesla-v100", "memory": "16GB", "cores": 5120}`),
	})
	gpuData2, _ := anypb.New(&anypb.Any{
		TypeUrl: "type.googleapis.com/mock.GPUType",
		Value:   []byte(`{"name": "nvidia-tesla-t4", "memory": "16GB", "cores": 2560}`),
	})
	s.gpuTypes["nvidia-tesla-v100"] = gpuData1
	s.gpuTypes["nvidia-tesla-t4"] = gpuData2

	// Create mock pricing data
	s.pricingData["standard-2"] = 0.10
	s.pricingData["standard-4"] = 0.20
	s.pricingData["gpu-v100"] = 2.50
	s.pricingData["high-mem-8"] = 0.80
}

// NodeGroups returns all node groups configured for this cloud provider
func (s *MockCloudProviderServer) NodeGroups(ctx context.Context, req *pb.NodeGroupsRequest) (*pb.NodeGroupsResponse, error) {
	log.Printf("NodeGroups called")
	
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodeGroups := make([]*pb.NodeGroup, 0, len(s.nodeGroups))
	for _, ng := range s.nodeGroups {
		nodeGroups = append(nodeGroups, ng)
	}

	return &pb.NodeGroupsResponse{
		NodeGroups: nodeGroups,
	}, nil
}

// NodeGroupForNode returns the node group for the given node
func (s *MockCloudProviderServer) NodeGroupForNode(ctx context.Context, req *pb.NodeGroupForNodeRequest) (*pb.NodeGroupForNodeResponse, error) {
	log.Printf("NodeGroupForNode called for node: %s", req.Node.Name)
	
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodeGroupID, exists := s.nodeToNodeGroup[req.Node.Name]
	if !exists {
		return &pb.NodeGroupForNodeResponse{
			NodeGroup: &pb.NodeGroup{Id: ""},
		}, nil
	}

	nodeGroup, exists := s.nodeGroups[nodeGroupID]
	if !exists {
		return nil, status.Errorf(codes.Internal, "node group %s not found", nodeGroupID)
	}

	return &pb.NodeGroupForNodeResponse{
		NodeGroup: nodeGroup,
	}, nil
}

// PricingNodePrice returns a theoretical minimum price of running a node
func (s *MockCloudProviderServer) PricingNodePrice(ctx context.Context, req *pb.PricingNodePriceRequest) (*pb.PricingNodePriceResponse, error) {
	log.Printf("PricingNodePrice called for node: %s", req.Node.Name)
	
	// This is an optional method - return Unimplemented
	return nil, status.Errorf(codes.Unimplemented, "PricingNodePrice not implemented")
}

// PricingPodPrice returns a theoretical minimum price of running a pod
func (s *MockCloudProviderServer) PricingPodPrice(ctx context.Context, req *pb.PricingPodPriceRequest) (*pb.PricingPodPriceResponse, error) {
	log.Printf("PricingPodPrice called for pod: %s", req.Pod.Name)
	
	// This is an optional method - return Unimplemented
	return nil, status.Errorf(codes.Unimplemented, "PricingPodPrice not implemented")
}

// GPULabel returns the label added to nodes with GPU resource
func (s *MockCloudProviderServer) GPULabel(ctx context.Context, req *pb.GPULabelRequest) (*pb.GPULabelResponse, error) {
	log.Printf("GPULabel called")
	
	return &pb.GPULabelResponse{
		Label: "accelerator",
	}, nil
}

// GetAvailableGPUTypes return all available GPU types cloud provider supports
func (s *MockCloudProviderServer) GetAvailableGPUTypes(ctx context.Context, req *pb.GetAvailableGPUTypesRequest) (*pb.GetAvailableGPUTypesResponse, error) {
	log.Printf("GetAvailableGPUTypes called")
	
	s.mu.RLock()
	defer s.mu.RUnlock()

	return &pb.GetAvailableGPUTypesResponse{
		GpuTypes: s.gpuTypes,
	}, nil
}

// Cleanup cleans up open resources before the cloud provider is destroyed
func (s *MockCloudProviderServer) Cleanup(ctx context.Context, req *pb.CleanupRequest) (*pb.CleanupResponse, error) {
	log.Printf("Cleanup called")
	
	return &pb.CleanupResponse{}, nil
}

// Refresh is called before every main loop and can be used to dynamically update cloud provider state
func (s *MockCloudProviderServer) Refresh(ctx context.Context, req *pb.RefreshRequest) (*pb.RefreshResponse, error) {
	log.Printf("Refresh called")
	
	return &pb.RefreshResponse{}, nil
}

// NodeGroupTargetSize returns the current target size of the node group
func (s *MockCloudProviderServer) NodeGroupTargetSize(ctx context.Context, req *pb.NodeGroupTargetSizeRequest) (*pb.NodeGroupTargetSizeResponse, error) {
	log.Printf("NodeGroupTargetSize called for node group: %s", req.Id)
	
	s.mu.RLock()
	defer s.mu.RUnlock()

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
	log.Printf("NodeGroupIncreaseSize called for node group: %s, delta: %d", req.Id, req.Delta)
	
	s.mu.Lock()
	defer s.mu.Unlock()

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
	log.Printf("NodeGroupDeleteNodes called for node group: %s, nodes: %d", req.Id, len(req.Nodes))
	
	s.mu.Lock()
	defer s.mu.Unlock()

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
	log.Printf("NodeGroupDecreaseTargetSize called for node group: %s, delta: %d", req.Id, req.Delta)
	
	s.mu.Lock()
	defer s.mu.Unlock()

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
	log.Printf("NodeGroupNodes called for node group: %s", req.Id)
	
	s.mu.RLock()
	defer s.mu.RUnlock()

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
	log.Printf("NodeGroupTemplateNodeInfo called for node group: %s", req.Id)
	
	// This is an optional method - return Unimplemented
	return nil, status.Errorf(codes.Unimplemented, "NodeGroupTemplateNodeInfo not implemented")
}

// NodeGroupGetOptions returns NodeGroupAutoscalingOptions for the node group
func (s *MockCloudProviderServer) NodeGroupGetOptions(ctx context.Context, req *pb.NodeGroupAutoscalingOptionsRequest) (*pb.NodeGroupAutoscalingOptionsResponse, error) {
	log.Printf("NodeGroupGetOptions called for node group: %s", req.Id)
	
	// This is an optional method - return Unimplemented
	return nil, status.Errorf(codes.Unimplemented, "NodeGroupGetOptions not implemented")
}

func main() {
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	mockServer := NewMockCloudProviderServer()
	pb.RegisterCloudProviderServer(grpcServer, mockServer)

	log.Printf("Mock CloudProvider gRPC server starting on :50051")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}