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
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/taints"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

// HomeClusterProviderServer implements the CloudProviderServer interface
type HomeClusterProviderServer struct {
	pb.UnimplementedCloudProviderServer

	// Kubernetes client for creating resources
	Client client.Client
	Scheme *runtime.Scheme

	gpuTypes map[string]*anypb.Any
}

// NewHomeClusterProviderServer creates a new server
func NewHomeClusterProviderServer(k8sClient client.Client, scheme *runtime.Scheme) *HomeClusterProviderServer {
	server := &HomeClusterProviderServer{
		Client:   k8sClient,
		Scheme:   scheme,
		gpuTypes: make(map[string]*anypb.Any),
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

	logger.Info("Data initialization completed", "gpuTypes", len(s.gpuTypes))
}

// groupNodes lists the slots (Node CRs) committed to a group. The number of slots is the
// max size of the group: maxSize is not a field of the Group CR, it is how many Node CRs
// carry the group label.
func (s *HomeClusterProviderServer) groupNodes(ctx context.Context, groupName string) ([]infrav1alpha1.Node, error) {
	nodes := &infrav1alpha1.NodeList{}
	listOpts := &client.ListOptions{LabelSelector: labels.SelectorFromSet(map[string]string{"group": groupName})}
	if err := s.Client.List(ctx, nodes, listOpts); err != nil {
		return nil, err
	}

	return nodes.Items, nil
}

// countsTowardsTargetSize reports whether a slot counts as capacity the provider is
// serving: a slot whose progress is a shutdown (shutdown/shuttingdown) is not counted,
// which is what leaves the group headroom for cluster-autoscaler.
func countsTowardsTargetSize(node infrav1alpha1.Node) bool {
	return node.Status.Progress != infrav1alpha1.ProgressShuttingDown &&
		node.Status.Progress != infrav1alpha1.ProgressShutdown
}

// targetSizeOf counts the slots that make up the target size of a group. This is the one
// definition of "target size" in the provider: it is what NodeGroupTargetSize reports to
// cluster-autoscaler, and every minSize/maxSize check must be compared against this same
// number, because the autoscaler sizes the group from it. Do not derive the size from a
// subset of the slots (powered on plus starting up, say): that is a different quantity and
// a floor judged against it refuses requests that do not cross the floor, which
// cluster-autoscaler then retries forever.
func targetSizeOf(nodes []infrav1alpha1.Node) int32 {
	var targetSize int32
	for _, node := range nodes {
		if countsTowardsTargetSize(node) {
			targetSize++
		}
	}

	return targetSize
}

// isStartingUpSlot reports the one slot shape NodeGroupDecreaseTargetSize can cancel: a
// slot that is desired on, has not powered on yet and is mid startup. Cancelling it takes
// it out of the target size (its progress becomes shuttingdown), which is what makes
// starting up slots the only capacity this entry point can decrease.
func isStartingUpSlot(node infrav1alpha1.Node) bool {
	return node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOn &&
		node.Status.PowerState == infrav1alpha1.PowerStateOff &&
		node.Status.Progress == infrav1alpha1.ProgressStartingUp
}

// effectiveMinSize returns the floor cluster-autoscaler must not scale the group below,
// clamped to the number of slots the provider has, and reports whether the clamp kicked
// in. maxSize is derived from the Node CR count rather than stored on the Group, so the
// CRD cannot express minSize <= maxSize; that invariant is asserted here, at the gRPC
// boundary. A floor the provider cannot fill is clamped instead of reported: telling
// cluster-autoscaler it needs more slots than exist makes it scale up and fail on "no
// powered off nodes found".
func effectiveMinSize(group *infrav1alpha1.Group, maxSize int32) (int32, bool) {
	if group == nil || group.Spec.MinSize <= 0 {
		return 0, false
	}
	if group.Spec.MinSize > maxSize {
		return maxSize, true
	}

	return group.Spec.MinSize, false
}

// groupMinSize resolves the floor of a group from its Group CR. A group that cannot be
// read has no floor (0) and is reported in the log: scale requests for it are answered by
// the node lookups, which surface the real error, and refusing the request on a transient
// API failure would stall the autoscaler.
func (s *HomeClusterProviderServer) groupMinSize(ctx context.Context, groupName string, maxSize int32) int32 {
	logger := log.Log.WithName("grpc-server")

	group := &infrav1alpha1.Group{}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: groupName, Namespace: namespaceConfig.Get()}, group); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.V(1).Info("Group not found, no minimum size to enforce", "group", groupName)
		} else {
			logger.Error(err, "Failed to get group, no minimum size to enforce", "group", groupName)
		}

		return 0
	}

	minSize, clamped := effectiveMinSize(group, maxSize)
	if clamped {
		logger.Info("Clamping the minSize of the group to the slots it has",
			"group", groupName, "specMinSize", group.Spec.MinSize, "maxSize", maxSize, "minSize", minSize)
	}

	return minSize
}

// NodeGroups returns all node groups
func (s *HomeClusterProviderServer) NodeGroups(ctx context.Context, req *pb.NodeGroupsRequest) (*pb.NodeGroupsResponse, error) {
	logger := log.Log.WithName("grpc-server")

	groups := &infrav1alpha1.GroupList{}
	err := s.Client.List(ctx, groups, &client.ListOptions{})
	if err != nil {
		logger.Error(err, "Failed to list groups")
		return nil, status.Errorf(codes.Internal, "failed to list groups: %v", err)
	}

	logger.Info("Retrieved groups", "count", len(groups.Items))

	// Create NodeGroup messages for all groups
	nodeGroups := make([]*pb.NodeGroup, 0, len(groups.Items))

	// Process each group
	for _, group := range groups.Items {

		var maxSize int32
		nodes, err := s.groupNodes(ctx, group.Name)
		if err != nil {
			logger.Error(err, "Failed to list nodes for group", "group", group.Name)
			maxSize = 0
		} else {
			maxSize = int32(len(nodes))
		}

		minSize, clamped := effectiveMinSize(&group, maxSize)
		if clamped {
			logger.Info("Clamping the minSize of the group to the slots it has",
				"group", group.Name, "specMinSize", group.Spec.MinSize, "maxSize", maxSize, "minSize", minSize)
		}

		// Create NodeGroup with: minSize=GroupSpec.MinSize, maxSize=<slots in the group>, id=group name
		nodeGroup := &pb.NodeGroup{
			Id:      group.Name,
			MinSize: minSize,
			MaxSize: maxSize,
			Debug:   fmt.Sprintf("Group %s - MinSize: %d, MaxSize: %d", group.Name, minSize, maxSize),
		}

		nodeGroups = append(nodeGroups, nodeGroup)
		logger.Info("Added group to NodeGroups response", "group", group.Name, "minSize", minSize, "nodeCount", maxSize)
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

	if groupName == "" {
		return &pb.NodeGroupForNodeResponse{
			NodeGroup: &pb.NodeGroup{Id: ""},
		}, nil
	}

	group := &infrav1alpha1.Group{}
	err = s.Client.Get(ctx, client.ObjectKey{Name: groupName, Namespace: namespaceConfig.Get()}, group)
	if err != nil {
		// Node not found in any group, return empty string
		logger.Error(err, "Group not found", "group", groupName)
		return &pb.NodeGroupForNodeResponse{
			NodeGroup: &pb.NodeGroup{Id: ""},
		}, nil
	}

	var maxSize int32 = 0
	nodes, err := s.groupNodes(ctx, groupName)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			maxSize = 0
		} else {
			logger.Error(err, "error listing nodes", "group", groupName)
		}
	} else {
		maxSize = int32(len(nodes))
	}

	for _, node := range nodes {
		if node.Name == req.Node.Name && node.Status.PowerState == infrav1alpha1.PowerStateOn {
			return &pb.NodeGroupForNodeResponse{
				NodeGroup: &pb.NodeGroup{Id: groupName},
			}, nil
		}
	}

	minSize, clamped := effectiveMinSize(group, maxSize)
	if clamped {
		logger.Info("Clamping the minSize of the group to the slots it has",
			"group", groupName, "specMinSize", group.Spec.MinSize, "maxSize", maxSize, "minSize", minSize)
	}

	nodeGroup := &pb.NodeGroup{
		Id:      groupName,
		MinSize: minSize,
		MaxSize: maxSize,
		Debug:   fmt.Sprintf("Group %s - MinSize: %d, MaxSize: %d", group.Name, minSize, len(nodes)),
	}

	logger.Info("Found node group for node", "node", req.Node.Name, "group", group.Name)
	return &pb.NodeGroupForNodeResponse{
		NodeGroup: nodeGroup,
	}, nil
}

// PricingNodePrice returns a theoretical minimum price of running a node
func (s *HomeClusterProviderServer) PricingNodePrice(ctx context.Context, req *pb.PricingNodePriceRequest) (*pb.PricingNodePriceResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("PricingNodePrice called", "node", req.Node.Name)

	node := &infrav1alpha1.Node{}
	err := s.Client.Get(ctx, client.ObjectKey{Name: req.Node.Name, Namespace: namespaceConfig.Get()}, node)
	if err != nil {
		// Node not found in any group
		logger.Info("Node not found", "node", req.Node.Name)
		return &pb.PricingNodePriceResponse{
			Price: 0,
		}, nil
	}

	price, err := strconv.ParseFloat(node.Spec.Pricing.HourlyRate, 64)
	if err != nil {
		logger.Info("error converting node.Spec.Pricing.HourlyRate", "node", node.Name, "HourlyRate", node.Spec.Pricing.HourlyRate)
		price = 0
	}

	return &pb.PricingNodePriceResponse{
		Price: price,
	}, nil
}

// PricingPodPrice returns a theoretical minimum price of running a pod.
//
// Returning Unimplemented here silently kills cluster-autoscaler's price expander:
// the expander loops over candidate node groups, drops any whose PodPrice errors,
// and ends up with an empty set — which then bypasses any chained tiebreaker
// (e.g. price,least-waste). See externalgrpc PricingModel + expander/price.
//
// The model mirrors PricingNodePrice: each Node CR carries PricingSpec.PodRate
// as a per-pod hourly rate; we return the cheapest configured rate as a
// "theoretical minimum" baseline that's stable across node groups. When no
// rates are configured we return 0, which is still a valid finite price.
func (s *HomeClusterProviderServer) PricingPodPrice(ctx context.Context, req *pb.PricingPodPriceRequest) (*pb.PricingPodPriceResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("PricingPodPrice called", "podBytes", len(req.PodBytes))

	nodes := &infrav1alpha1.NodeList{}
	if err := s.Client.List(ctx, nodes); err != nil {
		logger.Error(err, "failed to list nodes for pod pricing")
		return nil, status.Errorf(codes.Internal, "failed to list nodes: %v", err)
	}

	var price float64
	haveRate := false
	for _, n := range nodes.Items {
		rate, err := strconv.ParseFloat(n.Spec.Pricing.PodRate, 64)
		if err != nil {
			logger.Info("skipping unparseable PodRate", "node", n.Name, "podRate", n.Spec.Pricing.PodRate)
			continue
		}
		if !haveRate || rate < price {
			price = rate
			haveRate = true
		}
	}

	return &pb.PricingPodPriceResponse{Price: price}, nil
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

	nodes, err := s.groupNodes(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "group", req.Id)
		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	// Count nodes that should be running (target size = nodes not shutting down or shutdown)
	// This represents the desired number of active nodes in the group
	targetSize := targetSizeOf(nodes)

	logger.Info("NodeGroupTargetSize response completed", "nodeGroup", req.Id, "targetSize", targetSize, "totalNodes", len(nodes))
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

	nodes, err := s.groupNodes(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "group", req.Id)
		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	// The max size of the group is the number of slots committed to it. Refuse a scale up
	// that would exceed it explicitly: cluster-autoscaler sizes the group from this same
	// count, so a request that crosses it means its view is stale and powering a slot on
	// anyway would take the pool over the machines the operator committed.
	maxSize := int32(len(nodes))
	currentSize := targetSizeOf(nodes)
	if currentSize+req.Delta > maxSize {
		logger.Info("Refusing to increase the size of the group beyond its max size",
			"nodeGroup", req.Id, "delta", req.Delta, "currentSize", currentSize, "maxSize", maxSize)
		return nil, status.Errorf(codes.FailedPrecondition,
			"increasing node group %s by %d would exceed its max size %d (current target size %d)",
			req.Id, req.Delta, maxSize, currentSize)
	}

	// Find an unhealthy node
	var node *infrav1alpha1.Node
	var nodeName string

	for _, nodeCR := range nodes {
		desiredPowerState := nodeCR.Spec.DesiredPowerState
		powerState := nodeCR.Status.PowerState
		progress := nodeCR.Status.Progress

		// Look for nodes that are desired to be off and are actually off and shutdown
		if desiredPowerState == infrav1alpha1.PowerStateOff && powerState == infrav1alpha1.PowerStateOff && progress == infrav1alpha1.ProgressShutdown {
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

// NodeGroupDeleteNodes deletes nodes from this node group. The floor of the group
// (spec.minSize) is judged against the target size that results from the request, using
// targetSizeOf, the same number NodeGroupTargetSize reports. A request that deletes
// nothing takes no capacity away, so it is answered as a no-op success: the group can
// already be at or below its floor while a shutdown from an earlier request is still in
// flight, and a FailedPrecondition there is a refusal the autoscaler retries forever for
// an operation that would change nothing.
func (s *HomeClusterProviderServer) NodeGroupDeleteNodes(ctx context.Context, req *pb.NodeGroupDeleteNodesRequest) (*pb.NodeGroupDeleteNodesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupDeleteNodes called", "nodeGroup", req.Id, "nodes", len(req.Nodes))

	// Check for context cancellation
	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	// Enforce the floor of the group before touching anything. cluster-autoscaler keeps its
	// own min-size check, but a provider that accepts a scale down below minSize silently
	// powers machines off, so refuse here and say why in the log.
	nodes, err := s.groupNodes(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "group", req.Id)
		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	maxSize := int32(len(nodes))
	minSize := s.groupMinSize(ctx, req.Id, maxSize)
	currentSize := targetSizeOf(nodes)

	requested := make(map[string]bool, len(req.Nodes))
	for _, node := range req.Nodes {
		requested[node.Name] = true
	}

	// Only the slots that count towards the target size are capacity that goes away; a slot
	// that is already shut down is off the books and can be "deleted" again.
	var deleting int32
	for _, node := range nodes {
		if requested[node.Name] && countsTowardsTargetSize(node) {
			deleting++
		}
	}

	// Only a request that actually takes capacity away can cross the floor. A slot that is
	// already shut down is off the target size, so a request that deletes nothing projects
	// the current size: when the group is already at or below its floor (a shutdown from an
	// earlier delete is still in flight) that is not a request crossing the floor and
	// refusing it would wedge the autoscaler over a no-op.
	if projected := currentSize - deleting; deleting > 0 && projected < minSize {
		logger.Info("Refusing to delete nodes, the group would fall below its minSize",
			"nodeGroup", req.Id, "nodesRequested", len(req.Nodes), "deleting", deleting,
			"currentSize", currentSize, "projectedSize", projected, "minSize", minSize)
		return nil, status.Errorf(codes.FailedPrecondition,
			"deleting %d node(s) from node group %s would take it below its minSize %d (current target size %d)",
			deleting, req.Id, minSize, currentSize)
	}

	// Process each node to be deleted
	for _, node := range req.Nodes {
		// Check for context cancellation before processing each node
		if ctx.Err() != nil {
			return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded while processing nodes: %v", ctx.Err())
		}

		// Get the Node CR
		nodeCR := &infrav1alpha1.Node{}
		err := s.Client.Get(ctx, client.ObjectKey{Name: node.Name, Namespace: namespaceConfig.Get()}, nodeCR)
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

// NodeGroupDecreaseTargetSize decreases the target size of the node group by cancelling
// the slots that are still starting. The floor of the group (spec.minSize) is judged
// against the target size that this request can actually produce, using the same
// targetSizeOf that NodeGroupTargetSize reports, and a request that cannot take any
// capacity away is a no-op success: cluster-autoscaler retries a FailedPrecondition
// forever and never corrects its own view of the target size.
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

	nodes, err := s.groupNodes(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "group", req.Id)
		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	// The only capacity this entry point can take away is slots that are still starting:
	// a running machine is left to NodeGroupDeleteNodes, and a slot whose progress is
	// already a shutdown is off the target size. Cancelling one moves its progress to
	// shuttingdown, which is exactly one unit off targetSizeOf.
	var startingUpNodes []infrav1alpha1.Node
	for _, node := range nodes {
		if isStartingUpSlot(node) {
			startingUpNodes = append(startingUpNodes, node)
		}
	}

	// The target size is the single definition from targetSizeOf, the number
	// NodeGroupTargetSize reports. cluster-autoscaler calls this entry point precisely when
	// its view of the target size is stale, so a request that lands on the floor has to be
	// accepted. The arithmetic that decides the floor has been wrong twice: it first
	// compared the floor against the running slots only, dropping the ones that are starting
	// (minSize 1, one running + one starting, delta -1 lands on the floor, was refused), and
	// then against the uncapped delta, so a request that can only reach the floor was
	// refused as well (see the nodesToShutdown cap below).
	currentSize := targetSizeOf(nodes)

	// Nothing to cancel means the request cannot take any capacity away: answer it as a
	// no-op success rather than an error, so the autoscaler is not stuck retrying a
	// request the provider can never carry out, and moves on to reconcile its view.
	if len(startingUpNodes) == 0 {
		logger.Info("Nothing to cancel, the target size of the group cannot be decreased by this request",
			"nodeGroup", req.Id, "delta", req.Delta, "currentSize", currentSize, "resultingSize", currentSize)
		return &pb.NodeGroupDecreaseTargetSizeResponse{}, nil
	}

	minSize := s.groupMinSize(ctx, req.Id, int32(len(nodes)))

	// How many slots this request will actually cancel. Cancelling one starting slot is one
	// unit off the target size, so carrying out the delta means cancelling -delta of them,
	// and a request that asks for more than exist cancels all of them: the provider reports
	// success for what it actually did, and cluster-autoscaler reads the new target size back
	// from NodeGroupTargetSize.
	nodesToShutdown := -req.Delta
	if available := int32(len(startingUpNodes)); nodesToShutdown > available {
		nodesToShutdown = available
	}

	// The floor is judged against the size the request will produce, which is the current
	// target size minus the slots it can cancel, not minus the delta it was asked for. With
	// minSize 1, one running + two starting slots and delta -3 the achievable result is 1,
	// exactly the floor: refusing that (the uncapped arithmetic produced 0) is the same class
	// of unrecoverable FailedPrecondition as the case above, one level down.
	resultingSize := currentSize - nodesToShutdown
	if resultingSize < minSize {
		logger.Info("Refusing to decrease the target size below the minSize of the group",
			"nodeGroup", req.Id, "delta", req.Delta, "cancellable", int32(len(startingUpNodes)),
			"nodesToShutdown", nodesToShutdown, "currentSize", currentSize,
			"resultingSize", resultingSize, "minSize", minSize)
		return nil, status.Errorf(codes.FailedPrecondition,
			"decreasing node group %s by %d would take it below its minSize %d (current target size %d, %d slot(s) can be cancelled)",
			req.Id, req.Delta, minSize, currentSize, nodesToShutdown)
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

	// The size reported here is the target size after the cancellations that were actually
	// carried out (the cap above), not the one the delta asked for.
	logger.Info("NodeGroupDecreaseTargetSize completed", "nodeGroup", req.Id, "delta", req.Delta,
		"nodesShutdown", nodesToShutdown, "currentSize", currentSize, "newSize", currentSize-nodesToShutdown)

	return &pb.NodeGroupDecreaseTargetSizeResponse{}, nil
}

// NodeGroupNodes returns a list of all nodes that belong to this node group.
//
// The instances mirror the machines the provider has: a slot with a Kubernetes node object is
// reported running when it is desired to be on and deleting when it is desired to be off, a slot
// that is desired to be on but has not joined yet is reported as creating, and a slot that is
// desired to be off without a machine behind it is not reported at all.
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

	// Convert nodes to instances.
	//
	// A slot has a machine behind it exactly when it has a Kubernetes Node object: the kubelet
	// registers the node when the machine boots and the object stays behind, NotReady, while the
	// machine is powered off. Cluster-autoscaler matches every Kubernetes Node to an instance by
	// spec.providerID, so a slot with a node object must produce an instance (marked deleting
	// when the machine is desired off) and a slot without one must not.
	instances := make([]*pb.Instance, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		desiredOn := node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOn

		// The Kubernetes node only exists once the machine has joined the cluster. Slots
		// without spec.kubernetesNodeName have never been provisioned, so the lookup is
		// skipped and they are treated as having no node.
		kubernetesNode := &corev1.Node{}
		hasKubernetesNode := false
		if name := node.Spec.KubernetesNodeName; name != "" {
			err = s.Client.Get(ctx, client.ObjectKey{Name: name}, kubernetesNode)
			switch {
			case err == nil:
				hasKubernetesNode = true
			case !errors.IsNotFound(err):
				logger.Error(err, "Failed to get Kubernetes node", "node", name, "nodeCR", node.Name)
			}
		}

		if !hasKubernetesNode && !desiredOn {
			// A slot that is not desired to be on and has no machine is capacity the
			// provider could create, not an instance that exists. Reporting it inflates
			// the current size of the group, which makes the autoscaler provision extra
			// machines.
			continue
		}

		// A desired-on slot whose node has not joined yet is still provisioning; a desired-off
		// slot whose node object is left over is a machine that is being powered off.
		instanceState := pb.InstanceStatus_instanceCreating
		if hasKubernetesNode {
			if desiredOn {
				instanceState = pb.InstanceStatus_instanceRunning
			} else {
				instanceState = pb.InstanceStatus_instanceDeleting
			}
		}

		// Cluster-autoscaler keys instances by id, and the operator stamps the Kubernetes node
		// with providerID "homelab://<Node CR name>" once the machine joins. Use that same
		// identity as the fallback so the id does not change when the slot crosses the
		// provisioning boundary (an empty providerID, i.e. no cloud-controller-manager, would
		// otherwise report the bare node name and then the providerID). Keep the providerID of
		// the Kubernetes node when it is set, it is the authoritative identity.
		instanceID := kubernetesNode.Spec.ProviderID
		if instanceID == "" {
			instanceID = fmt.Sprintf("homelab://%s", node.Name)
		}

		instance := &pb.Instance{
			Id: instanceID,
			Status: &pb.InstanceStatus{
				InstanceState: instanceState,
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
		return nil, status.Error(codes.Unimplemented, "scale from zero not supported")
	}

	// Pick the slot the template is built from, and the Kubernetes node that describes it.
	representativeNodeCR, k8sNode, err := s.pickTemplateSource(ctx, nodes.Items)
	if err != nil {
		return nil, err
	}
	if k8sNode == nil {
		// No slot of the group has a Kubernetes node object, so the whole pool is powered off.
		// Synthesize a placeholder so the call keeps answering, and carry what the Node CR
		// knows about the slot.
		k8sNode = placeholderTemplateNode(representativeNodeCR)
	}

	// Create template node by copying the representative node
	templateNode := k8sNode.DeepCopy()

	// Put the capacity cluster-autoscaler will judge the pod's requests against into the template.
	// The declaration is read here, after the copy and before the marshal, so that the rule below
	// is the same for the node copied from a live slot and for the synthesized placeholder.
	applyTemplateCapacity(templateNode, representativeNodeCR)

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
	// Build a set of taint keys that should be removed. Start with the static list: the
	// transient node.kubernetes.io/* taints a booted template must not carry, and both
	// cluster-autoscaler taints. The DeletionCandidate key matters because the template is now
	// copied from a live node: a Ready node that cluster-autoscaler is currently evaluating for
	// scale-down carries that taint, and copying it into the whole group's template gives the
	// estimator a taint no pod tolerates, so nothing ever scales up. Do not replace this list
	// with taints.IsProtectedKey: that set protects lifecycle keys from being *written*, and
	// using it here would strip node.kubernetes.io/role=agent, the taint the e2e pools declare
	// and tolerate, from the template too.
	removeKeys := map[string]bool{
		"node.kubernetes.io/unschedulable":          true,
		"node.kubernetes.io/not-ready":              true,
		"node.kubernetes.io/unreachable":            true,
		taints.ToBeDeletedByClusterAutoscaler:       true,
		taints.DeletionCandidateOfClusterAutoscaler: true,
	}
	// Extend with any custom taints configured on the Node CR. TaintsToRemove only filters
	// taints out of the template; spec.nodeTaints, which adds taints to it, is applied below.
	for _, t := range representativeNodeCR.Spec.TaintsToRemove {
		removeKeys[t.Key] = true
	}
	// Filter out taints based on the combined set
	filtered := make([]corev1.Taint, 0, len(templateNode.Spec.Taints))
	for _, taint := range templateNode.Spec.Taints {
		if removeKeys[taint.Key] {
			continue
		}
		filtered = append(filtered, taint)
	}
	templateNode.Spec.Taints = filtered

	// Overlay the taints the slot declares. They are applied last, so they win over what the
	// representative node happens to carry: the Node CR is the intent for the machine the
	// autoscaler is about to boot, and the live node of the representative slot can lag behind
	// it. Applying them here is what lets CA see dedicated=remote:NoSchedule before the
	// machine has registered, and it is why a consumer no longer has to taint from a startup
	// Job.
	templateNode.Spec.Taints = taints.Overlay(templateNode.Spec.Taints, representativeNodeCR.Spec.NodeTaints)

	// Ensure node is schedulable
	templateNode.Spec.Unschedulable = false

	// Serialize the node to bytes using protobuf Marshal
	// This format is expected by the cluster autoscaler which will use Unmarshal() to deserialize
	nodeBytes, err := templateNode.Marshal()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to marshal node: %v", err)
	}

	return &pb.NodeGroupTemplateNodeInfoResponse{
		NodeBytes: nodeBytes,
	}, nil
}

// pickTemplateSource chooses the slot the node template is built from, and returns the
// Kubernetes node that describes it (nil when no slot of the group has one).
//
// The template is the machine shape cluster-autoscaler simulates a scale-up on, and only a
// corev1.Node carries that shape: status.allocatable, spec.taints, and the labels a pod needs
// in order to schedule. The old policy was "prefer a slot whose status.powerState is off",
// which is exactly what made the template wrong: a powered off machine has no corev1.Node
// object, so the lookup fell into the NotFound branch and the template was a placeholder with
// no taints and no capacity. CA then simulated an untainted, zero-capacity node while the
// machine boots tainted (dedicated=remote:NoSchedule), so it placed pods on a node that never
// accepts them and had no capacity to judge the requests against. It only worked before
// status.powerState started being seeded: the off slots had an empty status, so the loop fell
// through to the running slot and the template was accurate by accident.
//
// Policy, in order:
//  1. a slot whose corev1.Node exists, a Ready one preferred. Any live node object is a real
//     source of allocatable, taints and labels, which is what the template needs; a Ready one
//     is the shape of a machine that actually carries work, and a NotReady leftover (a node
//     object its powered off machine left behind) is the next best thing.
//  2. no slot has a corev1.Node at all, i.e. the whole pool is powered off: keep the previous
//     pick (prefer a powered off slot, then the first slot) and return a nil node, so the
//     caller synthesizes a placeholder. Capacity is not invented there - the placeholder carries
//     the shape the slot declares in spec.nodeCapacity, and nothing else - and a lookup that
//     failed for a reason other than "the node does not exist" is reported instead of being
//     turned into an empty template.
//
// The returned Node CR is never nil when the error is nil.
func (s *HomeClusterProviderServer) pickTemplateSource(ctx context.Context, items []infrav1alpha1.Node) (*infrav1alpha1.Node, *corev1.Node, error) {
	logger := log.Log.WithName("grpc-server")

	var (
		best       *infrav1alpha1.Node
		bestNode   *corev1.Node
		bestReady  bool
		poweredOff *infrav1alpha1.Node
		lastErr    error
	)

	for i := range items {
		candidate := &items[i]
		if poweredOff == nil && candidate.Status.PowerState == infrav1alpha1.PowerStateOff {
			poweredOff = candidate
		}

		candidateNode := &corev1.Node{}
		if err := s.Client.Get(ctx, client.ObjectKey{Name: kubernetesNodeNameOf(candidate)}, candidateNode); err != nil {
			// "There is no node object for this slot" is the normal state of a powered off
			// machine, and it says nothing about the other slots. Only a real failure is worth
			// remembering: recording NotFound here meant a later slot's missing node overwrote
			// an earlier slot's genuine API error, and the call answered with a placeholder for
			// a cluster the provider cannot read at all.
			if errors.IsNotFound(err) {
				continue
			}

			logger.Error(err, "Failed to get Kubernetes node", "node", kubernetesNodeNameOf(candidate), "nodeCR", candidate.Name)
			lastErr = err

			continue
		}

		ready := kubernetesNodeIsReady(candidateNode)
		if bestNode == nil || (ready && !bestReady) {
			best, bestNode, bestReady = candidate, candidateNode, ready
		}
	}

	if bestNode != nil {
		return best, bestNode, nil
	}

	// No slot has a node object. Any failure that is not "the node does not exist" (a
	// permissions problem, a broken cache) is reported, it must not be hidden behind a
	// placeholder: lastErr only ever holds such a failure, so one slot's NotFound cannot mask
	// another slot's real error.
	if lastErr != nil {
		return nil, nil, status.Errorf(codes.Internal, "failed to get kubernetes node: %v", lastErr)
	}

	fallback := poweredOff
	if fallback == nil {
		fallback = &items[0]
	}

	return fallback, nil, nil
}

// kubernetesNodeNameOf returns the name of the corev1.Node object that backs a slot. The
// operator fills spec.kubernetesNodeName in when the machine is provisioned; the Node CR name
// is the fallback, which is the rule NodeGroupNodes uses too.
func kubernetesNodeNameOf(node *infrav1alpha1.Node) string {
	if node.Spec.KubernetesNodeName != "" {
		return node.Spec.KubernetesNodeName
	}

	return node.Name
}

// kubernetesNodeIsReady reports whether the kubelet of a Kubernetes node is posting a ready
// status. A node without a Ready condition at all is not ready.
func kubernetesNodeIsReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

// applyTemplateCapacity stamps the machine size of the template from the slot's declaration.
//
// Precedence, spelled out because it is the whole point of the field:
//
//   - spec.nodeCapacity, when the slot declares it, wins over the live node's
//     status.capacity and status.allocatable. The template is the shape of the machine the
//     autoscaler is about to boot, and the declaration is that shape on purpose; what a slot
//     happens to report right now is an accident of which slot is Ready. A group whose slots
//     are not identical (a mixed pool, a slot mid-replacement, a node object its powered off
//     machine left behind) would otherwise hand the estimator a different capacity per call,
//     and a pool at zero has no live node at all, which is the case the field exists for;
//   - with no declaration, the live node's own numbers stay in the template: the kubelet
//     measured them, and that is today's behaviour for every pool that declares nothing. On the
//     placeholder path there is no live node, so an undeclared pool gets an empty capacity, and
//     the autoscaler is told "this machine has no known size" rather than being handed a guess.
//
// Both status.capacity and status.allocatable get the same list, each its own copy: the
// autoscaler reads allocatable, and a template machine has no system daemons to subtract from
// capacity, so the two are the same number. Declaring the pair, not one of them, is why the
// field is a single ResourceList.
func applyTemplateCapacity(templateNode *corev1.Node, slot *infrav1alpha1.Node) {
	if len(slot.Spec.NodeCapacity) == 0 {
		return
	}

	// DeepCopy, so the Node CR's own map is never aliased into the template: the caller marshals
	// the template, and a shared map is a mutation of the cached API object waiting to happen.
	templateNode.Status.Capacity = slot.Spec.NodeCapacity.DeepCopy()
	templateNode.Status.Allocatable = slot.Spec.NodeCapacity.DeepCopy()
}

// placeholderTemplateNode builds the object the template is derived from when no slot of the
// group has a corev1.Node: a fully powered off pool. It carries everything that can be read
// from the Node CR, the group label, the taints the slot declares, and the capacity the slot
// declares. Nothing about capacity is invented: status.allocatable of a machine that has never
// registered cannot be read, so an undeclared placeholder says "I do not know the size of this
// machine" with an empty list, and the pool that declared spec.nodeCapacity is answering the
// question from Git. applyTemplateCapacity applies the declaration to both paths; the call here
// is what makes the placeholder a complete object on its own.
func placeholderTemplateNode(slot *infrav1alpha1.Node) *corev1.Node {
	placeholder := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: slot.Name,
			Labels: map[string]string{
				"infra.homecluster.dev/group": slot.Labels["group"],
			},
		},
		Spec: corev1.NodeSpec{
			Taints: taints.Overlay(nil, slot.Spec.NodeTaints),
		},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
			Reason: "KubeletReady",
		}}},
	}

	applyTemplateCapacity(placeholder, slot)

	return placeholder
}

// NodeGroupGetOptions returns NodeGroupAutoscalingOptions for the node group
func (s *HomeClusterProviderServer) NodeGroupGetOptions(ctx context.Context, req *pb.NodeGroupAutoscalingOptionsRequest) (*pb.NodeGroupAutoscalingOptionsResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupGetOptions called", "nodeGroup", req.Id)

	group := &infrav1alpha1.Group{}
	err := s.Client.Get(ctx, client.ObjectKey{Name: req.Id, Namespace: namespaceConfig.Get()}, group)
	if err != nil {
		logger.Info("Group not found, returning nil to use CA defaults", "group", req.Id)
		return &pb.NodeGroupAutoscalingOptionsResponse{
			NodeGroupAutoscalingOptions: nil,
		}, nil
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

	// Convert *metav1.Duration to *durationpb.Duration
	toDurationPB := func(d *metav1.Duration) *durationpb.Duration {
		if d == nil || d.Duration == 0 {
			return nil
		}
		return durationpb.New(d.Duration)
	}

	return &pb.NodeGroupAutoscalingOptionsResponse{
		NodeGroupAutoscalingOptions: &pb.NodeGroupAutoscalingOptions{
			ScaleDownUtilizationThreshold:    scaleDownUtilizationThreshold,
			ScaleDownGpuUtilizationThreshold: scaleDownGpuUtilizationThreshold,
			ScaleDownUnneededDuration:        toDurationPB(group.Spec.ScaleDownUnneededTime),
			ScaleDownUnreadyDuration:         toDurationPB(group.Spec.ScaleDownUnreadyTime),
			MaxNodeProvisionDuration:         toDurationPB(group.Spec.MaxNodeProvisionTime),
			ZeroOrMaxNodeScaling:             group.Spec.ZeroOrMaxNodeScaling,
			IgnoreDaemonSetsUtilization:      group.Spec.IgnoreDaemonSetsUtilization,
		},
	}, nil
}
