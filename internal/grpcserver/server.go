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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

// MockCloudProviderServer implements the CloudProviderServer interface
type MockCloudProviderServer struct {
	pb.UnimplementedCloudProviderServer

	// GroupStore for managing Group resources
	groupStore *groupstore.GroupStore

	// Kubernetes client for creating resources
	Client client.Client
	Scheme *runtime.Scheme

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
func NewMockCloudProviderServer(groupStore *groupstore.GroupStore, k8sClient client.Client, scheme *runtime.Scheme) *MockCloudProviderServer {
	server := &MockCloudProviderServer{
		groupStore:      groupStore,
		Client:          k8sClient,
		Scheme:          scheme,
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

		// Get nodes for this group using the new GetNodesForGroup method
		nodeNames := s.groupStore.GetNodesForGroup(group.Name)
		nodeCount := len(nodeNames)

		// Create NodeGroup with: minSize=0, maxSize=GroupSpec.MaxSize, id=GroupSpec.Name
		nodeGroup := &pb.NodeGroup{
			Id:      group.Spec.Name,
			MinSize: 0,
			MaxSize: int32(group.Spec.MaxSize),
			Debug:   fmt.Sprintf("Group %s - MaxSize: %d, Nodes: %d, Health: %s", group.Spec.Name, group.Spec.MaxSize, nodeCount, group.Status.Health),
		}

		nodeGroups = append(nodeGroups, nodeGroup)
		logger.Info("Added group to NodeGroups response", "group", group.Name, "nodeCount", nodeCount)
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

	// Use the new GetGroupForNode method to find the group for this node
	groupName, exists := s.groupStore.GetGroupForNode(req.Node.Name)
	if !exists {
		// Node not found in any group, return empty string
		logger.Info("Node not found in any group", "node", req.Node.Name)
		return &pb.NodeGroupForNodeResponse{
			NodeGroup: &pb.NodeGroup{Id: ""},
		}, nil
	}

	// Get the group details
	group, err := s.groupStore.Get(groupName)
	if err != nil {
		logger.Error(err, "failed to get group from GroupStore", "group", groupName)
		return nil, status.Errorf(codes.Internal, "failed to get group %s: %v", groupName, err)
	}

	// Found the node in this group, return the associated group
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
			logger.Info("Cleanup would remove group resources", "group", group.Name)
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

	// Get nodes for this group using the new GetNodesForGroup method
	nodeNames := s.groupStore.GetNodesForGroup(req.Id)
	targetSize := int32(len(nodeNames))

	logger.Info("NodeGroupTargetSize response completed", "nodeGroup", req.Id, "targetSize", targetSize)
	return &pb.NodeGroupTargetSizeResponse{
		TargetSize: targetSize,
	}, nil
}

// NodeGroupIncreaseSize increases the size of the node group by finding an unhealthy node and starting a new job
func (s *MockCloudProviderServer) NodeGroupIncreaseSize(ctx context.Context, req *pb.NodeGroupIncreaseSizeRequest) (*pb.NodeGroupIncreaseSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupIncreaseSize called", "nodeGroup", req.Id, "delta", req.Delta)

	// Check for context cancellation
	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	// Validate delta is positive
	if req.Delta <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta must be positive, got %d", req.Delta)
	}

	// Get the group from GroupStore
	_, err := s.groupStore.Get(req.Id)
	if err != nil {
		logger.Error(err, "failed to get group from GroupStore", "group", req.Id)
		return nil, status.Errorf(codes.NotFound, "node group %s not found", req.Id)
	}

	// Get all nodes for this group from the nodeToGroupMap
	kubernetesNodeNames := s.groupStore.GetNodesForGroup(req.Id)
	if len(kubernetesNodeNames) == 0 {
		return nil, status.Errorf(codes.NotFound, "no nodes found for group %s", req.Id)
	}

	// Find an unhealthy node
	var unhealthyNode *infrav1alpha1.Node
	var unhealthyNodeName string

	for _, k8sNodeName := range kubernetesNodeNames {
		// Get the health status from healthcheckMap using the Kubernetes node name
		healthStatus, exists := s.groupStore.GetNodeHealthcheckStatus(req.Id, k8sNodeName)
		if !exists {
			logger.Info("No health status found for node, treating as unknown", "k8sNode", k8sNodeName)
			healthStatus = "unknown"
		}

		// Check if node is unhealthy (offline or unknown)
		if healthStatus == "offline" || healthStatus == "unknown" {
			// Find the Node CR that corresponds to this Kubernetes node name
			// We need to iterate through all nodes to find the one with matching KubernetesNodeName
			nodes, err := s.groupStore.ListNode()
			if err != nil {
				logger.Error(err, "Failed to list nodes from groupstore")
				continue
			}

			for _, node := range nodes {
				if node.Spec.KubernetesNodeName == k8sNodeName {
					unhealthyNode = node
					unhealthyNodeName = node.Name
					break
				}
			}

			if unhealthyNode != nil {
				break
			}
		}
	}

	if unhealthyNode == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "no unhealthy nodes found in group %s", req.Id)
	}

	logger.Info("Found unhealthy node, starting job", "node", unhealthyNodeName, "group", req.Id)

	// Get the Kubernetes node object and set it as schedulable
	k8sNode := &corev1.Node{}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: unhealthyNode.Spec.KubernetesNodeName}, k8sNode); err != nil {
		logger.Error(err, "failed to get Kubernetes node", "nodeName", unhealthyNode.Spec.KubernetesNodeName)
		return nil, status.Errorf(codes.Internal, "failed to get Kubernetes node %s: %v", unhealthyNode.Spec.KubernetesNodeName, err)
	}

	// Set the node as schedulable
	k8sNode.Spec.Unschedulable = false
	if err := s.Client.Update(ctx, k8sNode); err != nil {
		logger.Error(err, "failed to mark Kubernetes node as schedulable", "nodeName", unhealthyNode.Spec.KubernetesNodeName)
		return nil, status.Errorf(codes.Internal, "failed to mark node %s as schedulable: %v", unhealthyNode.Spec.KubernetesNodeName, err)
	}

	logger.Info("Successfully marked Kubernetes node as schedulable", "nodeName", unhealthyNode.Spec.KubernetesNodeName)

	// Create a Job to start the unhealthy node
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-startup-%s", unhealthyNode.Name, fmt.Sprintf("%d", metav1.Now().Unix())),
			Namespace: unhealthyNode.Namespace,
			Labels: map[string]string{
				"app":   "homelab-autoscaler",
				"group": req.Id,
				"node":  unhealthyNode.Spec.KubernetesNodeName,
				"type":  "startup",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "homelab-autoscaler",
						"group": req.Id,
						"node":  unhealthyNode.Spec.KubernetesNodeName,
						"type":  "startup",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "startup",
							Image:   unhealthyNode.Spec.StartupPodSpec.Image,
							Command: unhealthyNode.Spec.StartupPodSpec.Command,
							Args:    unhealthyNode.Spec.StartupPodSpec.Args,
						},
					},
				},
			},
		},
	}

	// Set owner reference to the Node CR
	if err := ctrl.SetControllerReference(unhealthyNode, job, s.Scheme); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to set controller reference for job: %v", err)
	}

	// Create the Job
	if err := s.Client.Create(ctx, job); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create startup job: %v", err)
	}

	logger.Info("Successfully created startup job", "job", job.Name, "node", unhealthyNodeName)

	// Update the Node status condition to "progressing"
	unhealthyNode.Status.Conditions = []metav1.Condition{
		{
			Type:               "progressing",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "NodeGroupIncreaseSize",
			Message:            fmt.Sprintf("Starting node via job %s", job.Name),
		},
	}

	// Update the Node status
	if err := s.Client.Status().Update(ctx, unhealthyNode); err != nil {
		logger.Error(err, "Failed to update node status", "node", unhealthyNodeName)
		// Don't fail the entire operation if status update fails
	}

	logger.Info("Successfully updated node status to progressing", "node", unhealthyNodeName)

	return &pb.NodeGroupIncreaseSizeResponse{}, nil
}

// NodeGroupDeleteNodes deletes nodes from this node group
func (s *MockCloudProviderServer) NodeGroupDeleteNodes(ctx context.Context, req *pb.NodeGroupDeleteNodesRequest) (*pb.NodeGroupDeleteNodesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupDeleteNodes called", "nodeGroup", req.Id, "nodes", len(req.Nodes))

	// Check for context cancellation
	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	// Get the group to check if it exists
	_, err := s.groupStore.Get(req.Id)
	if err != nil {
		logger.Error(err, "failed to get group from GroupStore", "group", req.Id)
		return nil, status.Errorf(codes.NotFound, "node group %s not found", req.Id)
	}

	// Get current node count
	currentNodes := s.groupStore.GetNodesForGroup(req.Id)
	currentSize := int32(len(currentNodes))

	// Check if we're trying to delete more nodes than exist
	if int32(len(req.Nodes)) > currentSize {
		logger.Info("Attempting to delete more nodes than exist", "nodeGroup", req.Id, "requested", len(req.Nodes), "current", currentSize)
		return nil, status.Errorf(codes.InvalidArgument, "cannot delete %d nodes when only %d exist", len(req.Nodes), currentSize)
	}

	// Process each node to be deleted
	for _, node := range req.Nodes {
		// Check for context cancellation before processing each node
		if ctx.Err() != nil {
			return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded while processing nodes: %v", ctx.Err())
		}

		// Get the Node CR from the GroupStore
		nodeCR, err := s.groupStore.GetNode(node.Name)
		if err != nil {
			logger.Error(err, "failed to get node from GroupStore", "node", node.Name)
			return nil, status.Errorf(codes.Internal, "failed to get node %s: %v", node.Name, err)
		}

		logger.Info("Found node to delete", "node", node.Name, "group", req.Id)

		// Mark the Kubernetes node as not schedulable before starting shutdown
		k8sNode := &corev1.Node{}
		if err := s.Client.Get(ctx, client.ObjectKey{Name: nodeCR.Spec.KubernetesNodeName}, k8sNode); err != nil {
			logger.Error(err, "failed to get Kubernetes node", "nodeName", nodeCR.Spec.KubernetesNodeName)
			return nil, status.Errorf(codes.Internal, "failed to get Kubernetes node %s: %v", nodeCR.Spec.KubernetesNodeName, err)
		}

		// Mark the node as unschedulable
		k8sNode.Spec.Unschedulable = true
		if err := s.Client.Update(ctx, k8sNode); err != nil {
			logger.Error(err, "failed to mark Kubernetes node as unschedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)
			return nil, status.Errorf(codes.Internal, "failed to mark node %s as unschedulable: %v", nodeCR.Spec.KubernetesNodeName, err)
		}

		logger.Info("Successfully marked Kubernetes node as unschedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)

		// Create a Job to shutdown the node
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-shutdown-%s", nodeCR.Name, fmt.Sprintf("%d", metav1.Now().Unix())),
				Namespace: nodeCR.Namespace,
				Labels: map[string]string{
					"app":   "homelab-autoscaler",
					"group": req.Id,
					"node":  nodeCR.Spec.KubernetesNodeName,
					"type":  "shutdown",
				},
			},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":   "homelab-autoscaler",
							"group": req.Id,
							"node":  nodeCR.Spec.KubernetesNodeName,
							"type":  "shutdown",
						},
					},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyOnFailure,
						Containers: []corev1.Container{
							{
								Name:    "shutdown",
								Image:   nodeCR.Spec.ShutdownPodSpec.Image,
								Command: nodeCR.Spec.ShutdownPodSpec.Command,
								Args:    nodeCR.Spec.ShutdownPodSpec.Args,
							},
						},
					},
				},
			},
		}

		// Set owner reference to the Node CR
		if err := ctrl.SetControllerReference(nodeCR, job, s.Scheme); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to set controller reference for shutdown job: %v", err)
		}

		// Create the Job
		if err := s.Client.Create(ctx, job); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to create shutdown job: %v", err)
		}

		logger.Info("Successfully created shutdown job", "job", job.Name, "node", node.Name)

		// Note: The Node status condition update to "terminating" should be handled by the node controller
		// via the SetNodeConditionTerminatingForShutdown method. The gRPC server should not directly
		// update node status to avoid conflicts with the controller's reconciliation loop.
	}

	logger.Info("NodeGroupDeleteNodes completed successfully", "nodeGroup", req.Id, "nodesDeleted", len(req.Nodes))
	return &pb.NodeGroupDeleteNodesResponse{}, nil
}

// NodeGroupDecreaseTargetSize decreases the target size of the node group
func (s *MockCloudProviderServer) NodeGroupDecreaseTargetSize(ctx context.Context, req *pb.NodeGroupDecreaseTargetSizeRequest) (*pb.NodeGroupDecreaseTargetSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupDecreaseTargetSize called", "nodeGroup", req.Id, "delta", req.Delta)

	// Check if the group exists
	_, err := s.groupStore.Get(req.Id)
	if err != nil {
		logger.Error(err, "failed to get group from GroupStore", "group", req.Id)
		return nil, status.Errorf(codes.NotFound, "node group %s not found", req.Id)
	}

	if req.Delta >= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta must be negative, got %d", req.Delta)
	}

	// Get current node count
	currentNodes := s.groupStore.GetNodesForGroup(req.Id)
	currentSize := int32(len(currentNodes))
	newSize := currentSize + req.Delta // req.Delta is negative

	if newSize < 0 {
		newSize = 0
	}

	logger.Info("NodeGroupDecreaseTargetSize would decrease target size", "nodeGroup", req.Id, "currentSize", currentSize, "newSize", newSize, "delta", req.Delta)

	return &pb.NodeGroupDecreaseTargetSizeResponse{}, nil
}

// NodeGroupNodes returns a list of all nodes that belong to this node group
func (s *MockCloudProviderServer) NodeGroupNodes(ctx context.Context, req *pb.NodeGroupNodesRequest) (*pb.NodeGroupNodesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupNodes called", "nodeGroup", req.Id)

	// Get nodes for this group using the new GetNodesForGroup method
	nodeNames := s.groupStore.GetNodesForGroup(req.Id)

	// Convert node names to instances
	instances := make([]*pb.Instance, 0, len(nodeNames))
	for _, nodeName := range nodeNames {
		instance := &pb.Instance{
			Id: nodeName,
			Status: &pb.InstanceStatus{
				InstanceState: pb.InstanceStatus_instanceRunning,
				ErrorInfo: &pb.InstanceErrorInfo{
					ErrorCode:    "",
					ErrorMessage: "",
				},
			},
		}
		instances = append(instances, instance)
	}

	logger.Info("NodeGroupNodes response completed", "nodeGroup", req.Id, "nodeCount", len(instances))
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
