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
	"sort"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

// HomeClusterProviderServer implements the CloudProviderServer interface
type HomeClusterProviderServer struct {
	pb.UnimplementedCloudProviderServer

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

// NewHomeClusterProviderServer creates a new mock server with GroupStore integration
func NewHomeClusterProviderServer(k8sClient client.Client, scheme *runtime.Scheme) *HomeClusterProviderServer {
	server := &HomeClusterProviderServer{
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
func (s *HomeClusterProviderServer) initializeMockData() {
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
func (s *HomeClusterProviderServer) NodeGroups(ctx context.Context, req *pb.NodeGroupsRequest) (*pb.NodeGroupsResponse, error) {
	logger := log.Log.WithName("grpc-server")

	groups := &infrav1alpha1.GroupList{}
	err := s.Client.List(ctx, groups, &client.ListOptions{})
	if err != nil {
		logger.Error(err, "failed to list groups")
		return nil, status.Errorf(codes.Internal, "failed to list groups: %v", err)
	}

	logger.Info("Retrieved groups from GroupStore", "count", len(groups.Items))

	// Create NodeGroup messages for all groups
	nodeGroups := make([]*pb.NodeGroup, 0, len(groups.Items))

	// Process each group
	for _, group := range groups.Items {

		var maxSize int32 = 0
		labelSelector := labels.SelectorFromSet(map[string]string{"group": group.Name})
		listOpts := &client.ListOptions{LabelSelector: labelSelector}
		nodes := &infrav1alpha1.NodeList{}
		err = s.Client.List(ctx, nodes, listOpts)
		if err != nil {
			maxSize = 0
		} else {
			maxSize = int32(len(nodes.Items))
		}

		// Create NodeGroup with: minSize=0, maxSize=GroupSpec.MaxSize, id=GroupSpec.Name
		nodeGroup := &pb.NodeGroup{
			Id:      group.Spec.Name,
			MinSize: 0,
			MaxSize: maxSize,
			Debug:   fmt.Sprintf("Group %s - MaxSize: %d, Nodes: %d", group.Spec.Name, group.Spec.MaxSize, maxSize),
		}

		nodeGroups = append(nodeGroups, nodeGroup)
		logger.Info("Added group to NodeGroups response", "group", group.Name, "nodeCount", maxSize)
	}

	logger.Info("NodeGroups response completed", "totalGroups", len(nodeGroups))
	return &pb.NodeGroupsResponse{
		NodeGroups: nodeGroups,
	}, nil
}

// NodeGroupForNode returns the node group for the given node
func (s *HomeClusterProviderServer) NodeGroupForNode(ctx context.Context, req *pb.NodeGroupForNodeRequest) (*pb.NodeGroupForNodeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupForNode called", "node", req.Node.Name)

	node := &corev1.Node{}
	err := s.Client.Get(ctx, client.ObjectKey{Name: req.Node.Name}, node)
	if err != nil {
		// Node not found in any group, return empty string
		logger.Info("Kubernetes node not found", "node", req.Node.Name)
		return &pb.NodeGroupForNodeResponse{
			NodeGroup: &pb.NodeGroup{Id: ""},
		}, nil
	}

	groupName := node.Labels["infra.homecluster.dev/group"]
	group := &infrav1alpha1.Group{}
	err = s.Client.Get(ctx, client.ObjectKey{Name: groupName}, group)
	if err != nil {
		// Node not found in any group, return empty string
		logger.Info("Group not found", "group", groupName)
		return &pb.NodeGroupForNodeResponse{
			NodeGroup: &pb.NodeGroup{Id: ""},
		}, nil
	}

	var maxSize int32 = 0
	labelSelector := labels.SelectorFromSet(map[string]string{"group": groupName})
	listOpts := &client.ListOptions{LabelSelector: labelSelector}
	nodes := &infrav1alpha1.NodeList{}
	err = s.Client.List(ctx, nodes, listOpts)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			maxSize = 0
		}
	}
	nodeGroup := &pb.NodeGroup{
		Id:      groupName,
		MinSize: 0,
		MaxSize: maxSize,
		Debug:   fmt.Sprintf("Group %s - MaxSize: %d", group.Spec.Name, group.Spec.MaxSize),
	}

	logger.Info("Found node group for node", "node", req.Node.Name, "group", group.Spec.Name)
	return &pb.NodeGroupForNodeResponse{
		NodeGroup: nodeGroup,
	}, nil
}

// PricingNodePrice returns a theoretical minimum price of running a node
func (s *HomeClusterProviderServer) PricingNodePrice(ctx context.Context, req *pb.PricingNodePriceRequest) (*pb.PricingNodePriceResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("PricingNodePrice called", "node", req.Node.Name)

	node := &infrav1alpha1.Node{}
	err := s.Client.Get(ctx, client.ObjectKey{Name: req.Node.Name}, node)
	if err != nil {
		// Node not found in any group
		logger.Info("Node not found", "node", req.Node.Name)
		return &pb.PricingNodePriceResponse{
			Price: 0,
		}, nil
	}

	price := node.Spec.Pricing.HourlyRate

	return &pb.PricingNodePriceResponse{
		Price: price,
	}, nil
}

// PricingPodPrice returns a theoretical minimum price of running a pod
func (s *HomeClusterProviderServer) PricingPodPrice(ctx context.Context, req *pb.PricingPodPriceRequest) (*pb.PricingPodPriceResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("PricingPodPrice called", "pod", req.Pod.Name)

	// This is an optional method - return Unimplemented
	return nil, status.Errorf(codes.Unimplemented, "PricingPodPrice not implemented")
}

// GPULabel returns the label added to nodes with GPU resource
func (s *HomeClusterProviderServer) GPULabel(ctx context.Context, req *pb.GPULabelRequest) (*pb.GPULabelResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("GPULabel called")

	return &pb.GPULabelResponse{
		Label: "accelerator",
	}, nil
}

// GetAvailableGPUTypes return all available GPU types cloud provider supports
func (s *HomeClusterProviderServer) GetAvailableGPUTypes(ctx context.Context, req *pb.GetAvailableGPUTypesRequest) (*pb.GetAvailableGPUTypesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("GetAvailableGPUTypes called")

	return &pb.GetAvailableGPUTypesResponse{
		GpuTypes: s.gpuTypes,
	}, nil
}

// Cleanup cleans up open resources before the cloud provider is destroyed
func (s *HomeClusterProviderServer) Cleanup(ctx context.Context, req *pb.CleanupRequest) (*pb.CleanupResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("Cleanup called")

	// In a real implementation, this would clean up resources like:
	// - Clean up any external resources
	// - Clear caches and temporary data

	return &pb.CleanupResponse{}, nil
}

// Refresh is called before every main loop and can be used to dynamically update cloud provider state
func (s *HomeClusterProviderServer) Refresh(ctx context.Context, req *pb.RefreshRequest) (*pb.RefreshResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("Refresh called")

	return &pb.RefreshResponse{}, nil
}

// NodeGroupTargetSize returns the current target size of the node group
func (s *HomeClusterProviderServer) NodeGroupTargetSize(ctx context.Context, req *pb.NodeGroupTargetSizeRequest) (*pb.NodeGroupTargetSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupTargetSize called", "nodeGroup", req.Id)

	// Check for context cancellation
	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	labelSelector := labels.SelectorFromSet(map[string]string{"group": req.Id})
	listOpts := &client.ListOptions{LabelSelector: labelSelector}
	nodes := &infrav1alpha1.NodeList{}
	err := s.Client.List(ctx, nodes, listOpts)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "group", req.Id)
		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	// Count nodes that should be running (target size = nodes not shutting down or shutdown)
	// This represents the desired number of active nodes in the group
	var targetSize int32 = 0
	for _, node := range nodes.Items {
		// Count nodes that are not in shutdown states
		if node.Status.Progress != infrav1alpha1.ProgressShuttingDown &&
			node.Status.Progress != infrav1alpha1.ProgressShutdown {
			targetSize++
		}
	}

	logger.Info("NodeGroupTargetSize response completed", "nodeGroup", req.Id, "targetSize", targetSize, "totalNodes", len(nodes.Items))
	return &pb.NodeGroupTargetSizeResponse{
		TargetSize: targetSize,
	}, nil
}

// NodeGroupIncreaseSize increases the size of the node group by finding an unhealthy node and starting a new job
func (s *HomeClusterProviderServer) NodeGroupIncreaseSize(ctx context.Context, req *pb.NodeGroupIncreaseSizeRequest) (*pb.NodeGroupIncreaseSizeResponse, error) {
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

	labelSelector := labels.SelectorFromSet(map[string]string{"group": req.Id})
	listOpts := &client.ListOptions{LabelSelector: labelSelector}
	nodes := &infrav1alpha1.NodeList{}
	err := s.Client.List(ctx, nodes, listOpts)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "group", req.Id)
		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	// Find an unhealthy node
	var node *infrav1alpha1.Node
	var nodeName string

	for _, nodeCR := range nodes.Items {
		powerState := nodeCR.Status.PowerState
		progress := nodeCR.Status.Progress

		if powerState == infrav1alpha1.PowerStateOff && progress == infrav1alpha1.ProgressShutdown {
			node = &nodeCR
			nodeName = nodeCR.Name
			break
		}
	}

	if node == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "no powered off nodes found in group %s", req.Id)
	}

	logger.Info("Found powered off node, setting desired state", "node", nodeName, "group", req.Id)

	node.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn

	if err := s.Client.Update(ctx, node); err != nil {
		logger.Error(err, "failed to update node DesiredPowerState", "node", nodeName)
		return nil, status.Errorf(codes.Internal, "failed to update node %s DesiredPowerState to on: %v", nodeName, err)
	}

	logger.Info("Successfully updated node DesiredPowerState to PowerStateOn", "node", nodeName)

	return &pb.NodeGroupIncreaseSizeResponse{}, nil
}

// NodeGroupDeleteNodes deletes nodes from this node group
func (s *HomeClusterProviderServer) NodeGroupDeleteNodes(ctx context.Context, req *pb.NodeGroupDeleteNodesRequest) (*pb.NodeGroupDeleteNodesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupDeleteNodes called", "nodeGroup", req.Id, "nodes", len(req.Nodes))

	// Check for context cancellation
	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	// Process each node to be deleted
	for _, node := range req.Nodes {
		// Check for context cancellation before processing each node
		if ctx.Err() != nil {
			return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded while processing nodes: %v", ctx.Err())
		}

		// Get the Node CR from the GroupStore
		nodeCR := &infrav1alpha1.Node{}
		err := s.Client.Get(ctx, client.ObjectKey{Name: node.Name}, nodeCR)
		if err != nil {
			logger.Error(err, "failed to get nodeCR", "node", node.Name)
			return nil, status.Errorf(codes.Internal, "failed to get node %s: %v", node.Name, err)
		}

		logger.Info("Found node to delete", "node", node.Name, "group", req.Id)
		nodeCR.Spec.DesiredPowerState = infrav1alpha1.PowerStateOff

		if err := s.Client.Update(ctx, nodeCR); err != nil {
			logger.Error(err, "failed to update node DesiredPowerState", "node", node.Name)
			return nil, status.Errorf(codes.Internal, "failed to update node %s DesiredPowerState to off: %v", node.Name, err)
		}

	}

	logger.Info("NodeGroupDeleteNodes completed successfully", "nodeGroup", req.Id, "nodesDeleted", len(req.Nodes))
	return &pb.NodeGroupDeleteNodesResponse{}, nil
}

// NodeGroupDecreaseTargetSize decreases the target size of the node group
func (s *HomeClusterProviderServer) NodeGroupDecreaseTargetSize(ctx context.Context, req *pb.NodeGroupDecreaseTargetSizeRequest) (*pb.NodeGroupDecreaseTargetSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupDecreaseTargetSize called", "nodeGroup", req.Id, "delta", req.Delta)

	// Check for context cancellation
	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	// Validate delta is negative
	if req.Delta >= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta must be negative, got %d", req.Delta)
	}

	labelSelector := labels.SelectorFromSet(map[string]string{"group": req.Id})
	listOpts := &client.ListOptions{LabelSelector: labelSelector}
	nodes := &infrav1alpha1.NodeList{}
	err := s.Client.List(ctx, nodes, listOpts)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "group", req.Id)
		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	// Separate nodes by their current state
	var poweredOnNodes []infrav1alpha1.Node
	var startingUpNodes []infrav1alpha1.Node

	for _, node := range nodes.Items {
		if node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOn &&
			node.Status.PowerState == infrav1alpha1.PowerStateOn &&
			node.Status.Progress == infrav1alpha1.ProgressReady {
			poweredOnNodes = append(poweredOnNodes, node)
		} else if node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOn &&
			node.Status.PowerState == infrav1alpha1.PowerStateOff &&
			node.Status.Progress == infrav1alpha1.ProgressStartingUp {
			startingUpNodes = append(startingUpNodes, node)
		}
	}

	currentSize := int32(len(poweredOnNodes) + len(startingUpNodes))
	newDesiredSize := int32(len(poweredOnNodes)) + req.Delta // req.Delta is negative

	if newDesiredSize < 0 {
		newDesiredSize = 0
	}

	// Sort starting up nodes by last startup time (oldest first)
	sort.Slice(startingUpNodes, func(i, j int) bool {
		if startingUpNodes[i].Status.LastStartupTime == nil {
			return false
		}
		if startingUpNodes[j].Status.LastStartupTime == nil {
			return true
		}
		return startingUpNodes[i].Status.LastStartupTime.Before(startingUpNodes[j].Status.LastStartupTime)
	})

	// Calculate how many starting up nodes to shut down
	nodesToShutdown := int32(len(startingUpNodes)) - newDesiredSize
	if nodesToShutdown < 0 {
		nodesToShutdown = 0
	}

	// Shut down the required number of starting up nodes
	for i := 0; i < int(nodesToShutdown) && i < len(startingUpNodes); i++ {
		node := &startingUpNodes[i]

		// Update spec (desired state)
		node.Spec.DesiredPowerState = infrav1alpha1.PowerStateOff
		if err := s.Client.Update(ctx, node); err != nil {
			logger.Error(err, "failed to update node spec", "node", node.Name)
			return nil, status.Errorf(codes.Internal, "failed to update node %s spec: %v", node.Name, err)
		}

		// Update status separately
		node.Status.Progress = infrav1alpha1.ProgressShuttingDown
		if err := s.Client.Status().Update(ctx, node); err != nil {
			logger.Error(err, "failed to update node status", "node", node.Name)
			return nil, status.Errorf(codes.Internal, "failed to update node %s status: %v", node.Name, err)
		}

		logger.Info("Successfully marked node for shutdown", "node", node.Name, "group", req.Id)
	}

	logger.Info("NodeGroupDecreaseTargetSize completed", "nodeGroup", req.Id, "currentSize", currentSize, "newSize", newDesiredSize, "delta", req.Delta, "nodesShutdown", nodesToShutdown)

	return &pb.NodeGroupDecreaseTargetSizeResponse{}, nil
}

// NodeGroupNodes returns a list of all nodes that belong to this node group
func (s *HomeClusterProviderServer) NodeGroupNodes(ctx context.Context, req *pb.NodeGroupNodesRequest) (*pb.NodeGroupNodesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupNodes called", "nodeGroup", req.Id)

	labelSelector := labels.SelectorFromSet(map[string]string{"group": req.Id})
	listOpts := &client.ListOptions{LabelSelector: labelSelector}
	nodes := &infrav1alpha1.NodeList{}
	err := s.Client.List(ctx, nodes, listOpts)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, status.Errorf(codes.InvalidArgument, "group not found: %s", req.Id)
		}
	}

	// Convert nodes to instances
	instances := make([]*pb.Instance, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		instance := &pb.Instance{
			Id: node.Name,
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

func (s *HomeClusterProviderServer) NodeGroupTemplateNodeInfo(ctx context.Context, req *pb.NodeGroupTemplateNodeInfoRequest) (*pb.NodeGroupTemplateNodeInfoResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupTemplateNodeInfo called", "nodeGroup", req.Id)

	// Get nodes from this group
	labelSelector := labels.SelectorFromSet(map[string]string{"group": req.Id})
	listOpts := &client.ListOptions{LabelSelector: labelSelector}
	nodes := &infrav1alpha1.NodeList{}
	err := s.Client.List(ctx, nodes, listOpts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list nodes: %v", err)
	}

	if len(nodes.Items) == 0 {
		// No nodes in group - return nil (valid response for scale-from-zero)
		return &pb.NodeGroupTemplateNodeInfoResponse{
			NodeInfo: nil,
		}, nil
	}

	// Find a representative node (prefer powered off nodes)
	var representativeNodeCR *infrav1alpha1.Node
	for i := range nodes.Items {
		if nodes.Items[i].Status.PowerState == infrav1alpha1.PowerStateOff {
			representativeNodeCR = &nodes.Items[i]
			break
		}
	}
	if representativeNodeCR == nil {
		// No powered on nodes, use first node
		representativeNodeCR = &nodes.Items[0]
	}

	// Get the corresponding Kubernetes node
	k8sNode := &corev1.Node{}
	err = s.Client.Get(ctx, client.ObjectKey{Name: representativeNodeCR.Name}, k8sNode)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get kubernetes node: %v", err)
	}

	// Create template node by copying the representative node
	templateNode := k8sNode.DeepCopy()

	// Generate a unique template name
	templateNode.Name = fmt.Sprintf("%s-template-%d", req.Id, time.Now().Unix())
	templateNode.UID = ""
	templateNode.ResourceVersion = ""

	// Remove node-specific labels that shouldn't be in template
	delete(templateNode.Labels, "kubernetes.io/hostname")

	// Set Ready conditions (remove NotReady status)
	templateNode.Status.Conditions = []corev1.NodeCondition{
		{
			Type:    corev1.NodeReady,
			Status:  corev1.ConditionTrue,
			Reason:  "KubeletReady",
			Message: "kubelet is posting ready status",
		},
		{
			Type:   corev1.NodeMemoryPressure,
			Status: corev1.ConditionFalse,
			Reason: "KubeletHasSufficientMemory",
		},
		{
			Type:   corev1.NodeDiskPressure,
			Status: corev1.ConditionFalse,
			Reason: "KubeletHasNoDiskPressure",
		},
		{
			Type:   corev1.NodePIDPressure,
			Status: corev1.ConditionFalse,
			Reason: "KubeletHasSufficientPID",
		},
		{
			Type:   corev1.NodeNetworkUnavailable,
			Status: corev1.ConditionFalse,
			Reason: "RouteCreated",
		},
	}

	// Remove unschedulable taints
	var sanitizedTaints []corev1.Taint
	for _, taint := range templateNode.Spec.Taints {
		// Filter out common unschedulable taints
		if taint.Key == "node.kubernetes.io/unschedulable" ||
			taint.Key == "node.kubernetes.io/not-ready" ||
			taint.Key == "node.kubernetes.io/unreachable" ||
			taint.Key == "ToBeDeletedByClusterAutoscaler" {
			continue
		}
		sanitizedTaints = append(sanitizedTaints, taint)
	}
	templateNode.Spec.Taints = sanitizedTaints

	// Ensure node is schedulable
	templateNode.Spec.Unschedulable = false

	return &pb.NodeGroupTemplateNodeInfoResponse{
		NodeInfo: templateNode,
	}, nil
}

// NodeGroupGetOptions returns NodeGroupAutoscalingOptions for the node group
func (s *HomeClusterProviderServer) NodeGroupGetOptions(ctx context.Context, req *pb.NodeGroupAutoscalingOptionsRequest) (*pb.NodeGroupAutoscalingOptionsResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupGetOptions called", "nodeGroup", req.Id)

	group := &infrav1alpha1.Group{}
	err := s.Client.Get(ctx, client.ObjectKey{Name: req.Id}, group)
	if err != nil {
		// Node not found in any group, return empty string
		logger.Info("Group not found", "group", req.Id)
		return &pb.NodeGroupAutoscalingOptionsResponse{}, nil

	}

	scaleDownUtilizationThreshold, err := strconv.ParseFloat(group.Spec.ScaleDownUtilizationThreshold, 64)
	if err != nil {
		logger.Info("error converting ScaleDownUtilizationThreshold", "group", group.Name, "ScaleDownUtilizationThreshold", group.Spec.ScaleDownUtilizationThreshold)
		scaleDownUtilizationThreshold = 0
	}

	scaleDownGpuUtilizationThreshold, err := strconv.ParseFloat(group.Spec.ScaleDownGpuUtilizationThreshold, 64)
	if err != nil {
		logger.Info("error converting ScaleDownGpuUtilizationThreshold", "group", group.Name, "ScaleDownGpuUtilizationThreshold", group.Spec.ScaleDownGpuUtilizationThreshold)
		scaleDownGpuUtilizationThreshold = 0
	}

	return &pb.NodeGroupAutoscalingOptionsResponse{
		NodeGroupAutoscalingOptions: &pb.NodeGroupAutoscalingOptions{
			ScaleDownUtilizationThreshold:    scaleDownUtilizationThreshold,
			ScaleDownGpuUtilizationThreshold: scaleDownGpuUtilizationThreshold,
			ScaleDownUnneededTime:            group.Spec.ScaleDownUnneededTime,
			ScaleDownUnreadyTime:             group.Spec.ScaleDownUnreadyTime,
			MaxNodeProvisionTime:             group.Spec.MaxNodeProvisionTime,
			ZeroOrMaxNodeScaling:             group.Spec.ZeroOrMaxNodeScaling,
			IgnoreDaemonSetsUtilization:      group.Spec.IgnoreDaemonSetsUtilization,
		},
	}, nil
}
