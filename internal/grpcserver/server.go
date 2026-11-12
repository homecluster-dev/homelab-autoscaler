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
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	"github.com/homecluster-dev/homelab-autoscaler/internal/nodelabels"
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

	// templateConflicts remembers, per node group, the keys the group's slots disagreed on the last
	// time this provider answered for it. NodeGroupTemplateNodeInfo is asked about every group on
	// every scan of cluster-autoscaler, so a disagreement is reported when it appears and when it
	// changes, not on every scan (see recordTemplateConflicts). The memory is per process: a restart
	// re-states the report, and nothing is derived from it.
	templateConflicts sync.Map
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
func (s *HomeClusterProviderServer) groupNodes(ctx context.Context, groupName string) ([]infrav2alpha1.Node, error) {
	nodes := &infrav2alpha1.NodeList{}
	listOpts := &client.ListOptions{LabelSelector: labels.SelectorFromSet(map[string]string{"group": groupName})}
	if err := s.Client.List(ctx, nodes, listOpts); err != nil {
		return nil, err
	}

	return nodes.Items, nil
}

// groupClaims lists the claims created for one node group. The provider counts them, cancels them
// and creates them, and the group's label on the claim is what makes all three one list
// (`kubectl get nodeclaim -l group=<group>` is the same view).
func (s *HomeClusterProviderServer) groupClaims(ctx context.Context, groupName string) ([]infrav2alpha1.NodeClaim, error) {
	return claim.ListInGroup(ctx, s.Client, namespaceConfig.Get(), groupName)
}

// targetSizeOf is the count of live claims: the target size of a group, and the number
// cluster-autoscaler sizes the group from. No progress value, and no spec field of a slot, appears
// in the arithmetic (docs/development/nodeclaim.md §5).
//
// The v1alpha1 rule this replaces counted a slot when spec.desiredPowerState was on **or** its
// status.progress was not one of shuttingdown / shutdown / startingup, and the "on" half of it
// existed because the write that asked for a machine was the only record of the request: a slot the
// autoscaler had just claimed kept reporting "shut down" until the node controller got to it, so a
// target size read off the progress alone made cluster-autoscaler call IncreaseSize again in the same
// loop. A claim is the request and the record, so the count of claims is the number, and a failed
// claim stops being counted the moment it is recorded - which is what removes the state "the
// autoscaler is paying for a machine that never arrived, forever".
func targetSizeOf(claims []infrav2alpha1.NodeClaim) int32 {
	return claim.TargetSize(claims)
}

// effectiveMinSize returns the floor cluster-autoscaler must not scale the group below,
// clamped to the number of slots the provider has, and reports whether the clamp kicked
// in. maxSize is derived from the Node CR count rather than stored on the Group, so the
// CRD cannot express minSize <= maxSize; that invariant is asserted here, at the gRPC
// boundary. A floor the provider cannot fill is clamped instead of reported: telling
// cluster-autoscaler it needs more slots than exist makes it scale up and fail on "no
// powered off nodes found".
func effectiveMinSize(group *infrav2alpha1.Group, maxSize int32) (int32, bool) {
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

	group := &infrav2alpha1.Group{}
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

	groups := &infrav2alpha1.GroupList{}
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

	group := &infrav2alpha1.Group{}
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
		if node.Name == req.Node.Name && node.Status.PowerState == infrav2alpha1.PowerStateOn {
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

	node := &infrav2alpha1.Node{}
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

	nodes := &infrav2alpha1.NodeList{}
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

// NodeGroupTargetSize returns the current target size of the node group: the number of live claims
// (pending, booting, ready) of the group, at this instant.
//
// A claim marked for scale-down is not counted, and a failed claim is not counted, so the two states
// the v1alpha1 arithmetic could not express - "a startup the autoscaler has cancelled" and "a machine
// that never arrived, paid for forever" - have no place to exist in this number.
func (s *HomeClusterProviderServer) NodeGroupTargetSize(ctx context.Context, req *pb.NodeGroupTargetSizeRequest) (*pb.NodeGroupTargetSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupTargetSize called", "nodeGroup", req.Id)

	// Check for context cancellation
	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	// The slots are read for the same reason every entry point reads them: a group with no slots has
	// nothing to size, and a read that fails is the error the autoscaler should see.
	nodes, err := s.groupNodes(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "nodeGroup", req.Id)

		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	claims, err := s.groupClaims(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list the claims of group", "nodeGroup", req.Id)

		return nil, status.Errorf(codes.Internal, "failed to list the claims of node group %s: %v", req.Id, err)
	}

	targetSize := targetSizeOf(claims)

	logger.Info("NodeGroupTargetSize response completed", "nodeGroup", req.Id,
		"targetSize", targetSize, "liveClaims", targetSize, "claims", len(claims), "slots", len(nodes))

	return &pb.NodeGroupTargetSizeResponse{
		TargetSize: targetSize,
	}, nil
}

// NodeGroupIncreaseSize claims delta free slots of the group by creating delta NodeClaims, each
// carrying the request that made it: the call, the delta, the group, the deadline of the attempt, and
// the caller's identity.
//
// The v1alpha1 handler claimed the *first* slot matching a three-field scan and returned, one slot
// per call whatever delta said, and the whole record of the request was the scalar it wrote. Here a
// slot is free exactly when no live claim names it (no scan, no status seeding, no observation), the
// max-size check is liveClaims + delta > slots, and the request is attributable from the claim.
func (s *HomeClusterProviderServer) NodeGroupIncreaseSize(ctx context.Context, req *pb.NodeGroupIncreaseSizeRequest) (*pb.NodeGroupIncreaseSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupIncreaseSize called", "nodeGroup", req.Id, "delta", req.Delta)

	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	if req.Delta <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta must be positive, got %d", req.Delta)
	}

	nodes, err := s.groupNodes(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "nodeGroup", req.Id)

		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	claims, err := s.groupClaims(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list the claims of group", "nodeGroup", req.Id)

		return nil, status.Errorf(codes.Internal, "failed to list the claims of node group %s: %v", req.Id, err)
	}

	// The max size of the group is the number of slots committed to it, and the current size is the
	// live claims: slots that are claimed and not booted yet are already spent against the max, which
	// is what makes a burst of IncreaseSize calls in one autoscaler loop stop at the max.
	maxSize := int32(len(nodes))
	currentSize := targetSizeOf(claims)
	if currentSize+req.Delta > maxSize {
		logger.Info("Refusing to increase the size of the group beyond its max size",
			"nodeGroup", req.Id, "delta", req.Delta, "currentSize", currentSize, "maxSize", maxSize)

		return nil, status.Errorf(codes.FailedPrecondition,
			"increasing node group %s by %d would exceed its max size %d (current target size %d)",
			req.Id, req.Delta, maxSize, currentSize)
	}

	free := claim.FreeSlots(nodes, claims)
	if int32(len(free)) < req.Delta {
		logger.Info("Refusing to increase the size of the group, not enough free slots",
			"nodeGroup", req.Id, "delta", req.Delta, "freeSlots", len(free), "slots", len(nodes))

		return nil, status.Errorf(codes.FailedPrecondition,
			"no free node slot found in group %s: %d requested, %d of %d slots are free",
			req.Id, req.Delta, len(free), len(nodes))
	}

	group := s.groupOf(ctx, req.Id)
	created := make([]string, 0, req.Delta)

	for i := range int(req.Delta) {
		slot := free[i]

		nodeClaim, err := s.newClaim(ctx, &slot, group, infrav2alpha1.ClaimRequest{
			Call:  infrav2alpha1.ClaimCallIncreaseSize,
			Delta: req.Delta,
			Group: req.Id,
		}, claim.RequestedByAutoscaler)
		if err != nil {
			logger.Error(err, "failed to create the claim of a free slot", "node", slot.Name, "group", req.Id, "created", created)

			return nil, status.Errorf(codes.Internal, "failed to create a NodeClaim for node %s of group %s: %v", slot.Name, req.Id, err)
		}

		created = append(created, nodeClaim.Name)

		logger.Info("Created a NodeClaim for a free slot",
			"node", slot.Name, "claim", nodeClaim.Name, "group", req.Id,
			"deadline", nodeClaim.Spec.DeadlineAt, "maxNodeProvisionTime", nodeClaim.Spec.MaxNodeProvisionTime)
	}

	logger.Info("NodeGroupIncreaseSize completed", "nodeGroup", req.Id, "delta", req.Delta, "claims", created)

	return &pb.NodeGroupIncreaseSizeResponse{}, nil
}

// NodeGroupDeleteNodes cancels the claims of the slots the request names, after recording the request
// on each of them (spec.request.call: DeleteNodes, spec.request.nodes: the names).
//
// cluster-autoscaler evicts before it calls DeleteNodes, so the drain stays its work; what the
// provider owns is the machine. A pending claim is deleted, and the object goes: nothing is in
// flight. A booting or ready claim is deleted through the claim controller's finalizer, which keeps
// the request alive until the machine is observed off, so the boot is reversed (cordon, shutdown Job,
// power off) and never interrupted. A slot with no live claim has nothing to cancel, and the input
// law powers its machine off on the next pass (no live claim + machine on = shutdown).
//
// The floor of the group (spec.minSize) is judged against the target size that results from the
// request, using the same targetSizeOf that NodeGroupTargetSize reports. A request that deletes
// nothing takes no capacity away, so it is answered as a no-op success: the group can already be at
// or below its floor while a shutdown from an earlier request is still in flight, and a
// FailedPrecondition there is a refusal the autoscaler retries forever for an operation that changes
// nothing.
func (s *HomeClusterProviderServer) NodeGroupDeleteNodes(ctx context.Context, req *pb.NodeGroupDeleteNodesRequest) (*pb.NodeGroupDeleteNodesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupDeleteNodes called", "nodeGroup", req.Id, "nodes", len(req.Nodes))

	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	nodes, err := s.groupNodes(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "nodeGroup", req.Id)

		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	claims, err := s.groupClaims(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list the claims of group", "nodeGroup", req.Id)

		return nil, status.Errorf(codes.Internal, "failed to list the claims of node group %s: %v", req.Id, err)
	}

	requested := make(map[string]string, len(req.Nodes))
	for _, node := range req.Nodes {
		requested[node.Name] = node.Name
	}

	maxSize := int32(len(nodes))
	minSize := s.groupMinSize(ctx, req.Id, maxSize)
	currentSize := targetSizeOf(claims)

	// The slots the request names: a DeleteNodes name is a Kubernetes node name, and the slot is
	// found by its name (the identity cluster-autoscaler keys on, and what the providerID homelab://
	// <slot> is built from) or by spec.kubernetesNodeName, which can differ.
	slots := make([]infrav2alpha1.Node, 0, len(req.Nodes))

	for _, node := range nodes {
		if _, ok := requested[node.Name]; ok {
			slots = append(slots, node)

			continue
		}

		if node.Spec.KubernetesNodeName != "" {
			if _, ok := requested[node.Spec.KubernetesNodeName]; ok {
				slots = append(slots, node)
			}
		}
	}

	// Only a slot with a live claim is capacity that goes away: a slot whose machine is off, or whose
	// claim already ended, is off the books and can be "deleted" again.
	var deleting int32

	for i := range slots {
		if live := claim.LiveOfSlot(claimsOfSlot(claims, slots[i].Name)); live != nil && claim.CountsTowardTargetSize(live) {
			deleting++
		}
	}

	if projected := currentSize - deleting; deleting > 0 && projected < minSize {
		logger.Info("Refusing to delete nodes, the group would fall below its minSize",
			"nodeGroup", req.Id, "nodesRequested", len(req.Nodes), "deleting", deleting,
			"currentSize", currentSize, "projectedSize", projected, "minSize", minSize)

		return nil, status.Errorf(codes.FailedPrecondition,
			"deleting %d node(s) from node group %s would take it below its minSize %d (current target size %d)",
			deleting, req.Id, minSize, currentSize)
	}

	names := make([]string, 0, len(req.Nodes))
	for _, node := range req.Nodes {
		names = append(names, node.Name)
	}

	for i := range slots {
		if ctx.Err() != nil {
			return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded while processing nodes: %v", ctx.Err())
		}

		slot := &slots[i]
		live := claim.LiveOfSlot(claimsOfSlot(claims, slot.Name))

		// A claim the request already marked is the state the request asked for: its finalizer is
		// running the machine down. Re-deleting it is not a cancellation, it is a second write on
		// an object that is on its way out, so the request is idempotent by construction.
		if live != nil && !live.DeletionTimestamp.IsZero() {
			logger.V(1).Info("The claim of the slot is already cancelled and draining", "claim", live.Name, "node", slot.Name)

			continue
		}

		if live == nil {
			// Nothing to cancel. A machine that is standing on this slot is powered off by the input
			// law on the slot's next pass; a free slot is already off.
			logger.V(1).Info("Slot has no live claim, nothing to cancel", "node", slot.Name, "group", req.Id)

			continue
		}

		// Record the request on the claim, then cancel it. The write order matters: the finalizer path
		// reports the claim's end from spec.request, so the attribution has to be on the object before
		// the delete that marks it.
		if live.Spec.Request.Call != infrav2alpha1.ClaimCallDeleteNodes {
			live.Spec.Request.Call = infrav2alpha1.ClaimCallDeleteNodes
			live.Spec.Request.Nodes = names
			live.Spec.RequestedBy = claim.RequestedByAutoscaler

			if err := s.Client.Update(ctx, live); err != nil {
				logger.Error(err, "failed to record the DeleteNodes request on the claim", "claim", live.Name, "node", slot.Name)

				return nil, status.Errorf(codes.Internal, "failed to record the delete request on claim %s: %v", live.Name, err)
			}
		}

		if err := s.Client.Delete(ctx, live); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "failed to cancel the claim of a deleted node", "claim", live.Name, "node", slot.Name)

			return nil, status.Errorf(codes.Internal, "failed to cancel the claim %s of node %s: %v", live.Name, slot.Name, err)
		}

		logger.Info("Cancelled the claim of a node named for deletion", "node", slot.Name, "claim", live.Name,
			"phase", string(live.Status.Phase), "group", req.Id)
	}

	logger.Info("NodeGroupDeleteNodes completed successfully", "nodeGroup", req.Id, "nodesDeleted", len(req.Nodes))

	return &pb.NodeGroupDeleteNodesResponse{}, nil
}

// claimsOfSlot filters a group's claim list down to one slot. The provider already holds the group's
// claims, and a slot's live claim is one pass over that list: no second read per slot, and one
// consistent snapshot for the whole request.
func claimsOfSlot(claims []infrav2alpha1.NodeClaim, slotName string) []infrav2alpha1.NodeClaim {
	out := make([]infrav2alpha1.NodeClaim, 0, 1)

	for i := range claims {
		if claim.SlotNameOf(&claims[i]) == slotName {
			out = append(out, claims[i])
		}
	}

	return out
}

// NodeGroupDecreaseTargetSize cancels up to -delta claims that are in phase **pending**, by recording
// the request on each of them (spec.request.call: DecreaseTargetSize, spec.request.delta: the delta,
// spec.requestedBy: the autoscaler) and deleting them, and reports the size the request actually
// produced.
//
// pending is the only cancelable phase (docs/development/nodeclaim.md §3): nothing is in flight, the object
// goes, and its unit comes off the target size on that write. A booting claim is committed and is
// never a cancel target - the startup Job, the coordination lock, the cordon, the node object and the
// machine are all in flight, and a cancel leaves the Job in a state nothing owns, which is the residue
// the 0.1.19 fix removed. A ready claim's machine is running work; cluster-autoscaler scales that
// path with DeleteNodes, not with this one.
//
// The floor (spec.minSize) is judged against the target size this request can produce: the current
// size minus the pending claims it is able to cancel, capped to what exists.
//
// What the request cannot carry out is not answered with success. The response of the RPC carries no
// size, so a success is the provider asserting "the size you asked for is in force", and the
// autoscaler's next move is built on that: it stops asking, and its pending pods sit on a group that
// is one unit bigger than it believes, while the slot the same autoscaler wants to scale into answers
// IncreaseSize with FailedPrecondition, no free node slot, for as long as the committed boot takes -
// up to one boot deadline (12m10s), plus the TTL of the claim that carried it. A group that is
// already at the requested size is a no-op success; a group whose every live claim is past the
// cancelable phase is answered with codes.Unavailable, the retryable code: the state is transient by
// construction (the boot deadline closes every boot, and DeleteNodes closes every ready machine), and
// the autoscaler's own retry is what picks the decrease up when it becomes possible.
func (s *HomeClusterProviderServer) NodeGroupDecreaseTargetSize(ctx context.Context, req *pb.NodeGroupDecreaseTargetSizeRequest) (*pb.NodeGroupDecreaseTargetSizeResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupDecreaseTargetSize called", "nodeGroup", req.Id, "delta", req.Delta)

	if ctx.Err() != nil {
		return nil, status.Errorf(codes.DeadlineExceeded, "context deadline exceeded: %v", ctx.Err())
	}

	if req.Delta >= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta must be negative, got %d", req.Delta)
	}

	nodes, err := s.groupNodes(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list nodes for group", "nodeGroup", req.Id)

		return nil, status.Errorf(codes.Internal, "failed to list nodes for group %s: %v", req.Id, err)
	}

	claims, err := s.groupClaims(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list the claims of group", "nodeGroup", req.Id)

		return nil, status.Errorf(codes.Internal, "failed to list the claims of node group %s: %v", req.Id, err)
	}

	currentSize := targetSizeOf(claims)
	cancellable := claim.PendingOfGroup(claims, req.Id)

	// Cancelling one pending claim is one unit off the target size, so carrying out the delta means
	// cancelling -delta of them, and a request that asks for more than exist cancels all of them: the
	// provider reports success for what it actually did, and cluster-autoscaler reads the new target
	// size back from NodeGroupTargetSize.
	toCancel := -req.Delta
	if available := int32(len(cancellable)); toCancel > available {
		toCancel = available
	}

	minSize := s.groupMinSize(ctx, req.Id, int32(len(nodes)))
	resultingSize := currentSize - toCancel

	if resultingSize < minSize {
		logger.Info("Refusing to decrease the target size below the minSize of the group",
			"nodeGroup", req.Id, "delta", req.Delta, "cancellable", int32(len(cancellable)),
			"toCancel", toCancel, "currentSize", currentSize, "resultingSize", resultingSize, "minSize", minSize)

		return nil, status.Errorf(codes.FailedPrecondition,
			"decreasing node group %s by %d would take it below its minSize %d (current target size %d, %d claim(s) can be cancelled)",
			req.Id, req.Delta, minSize, currentSize, toCancel)
	}

	if len(cancellable) == 0 {
		if currentSize == 0 {
			logger.Info("Nothing to cancel, and the group is already at the size the request asks for",
				"nodeGroup", req.Id, "delta", req.Delta, "currentSize", currentSize)

			return &pb.NodeGroupDecreaseTargetSizeResponse{}, nil
		}

		booting, ready := countPhase(claims, infrav2alpha1.NodeClaimBooting), countPhase(claims, infrav2alpha1.NodeClaimReady)

		logger.Info("Nothing cancellable right now: every live claim of the group is past the pending phase",
			"nodeGroup", req.Id, "delta", req.Delta, "currentSize", currentSize,
			"bootingClaims", booting, "readyClaims", ready)

		return nil, status.Errorf(codes.Unavailable,
			"node group %s cannot be decreased by %d right now: its %d live claim(s) are past the cancelable pending phase (%d booting, %d ready) and every one of them is committed - a boot is reversed by its own deadline, a ready machine by DeleteNodes; the request is worth retrying, and the autoscaler's next loop is what picks it up",
			req.Id, req.Delta, currentSize, booting, ready)
	}

	cancelled := make([]string, 0, toCancel)

	for i := range int(toCancel) {
		nodeClaim := &cancellable[i]

		// Record the request on the claim, then cancel it, exactly as NodeGroupDeleteNodes does. The
		// write order is not decoration: the claim controller's finalizer ends the episode from
		// spec.request (reasonForDeletion maps the call to CancelledByDecreaseTargetSize), the digest
		// the slot keeps is built from the same field, and the pool's `down` column is rendered from
		// it - a delete that records nothing leaves the IncreaseSize attribution of the pending claim
		// as the story of the request, and the pool reports a shrink it calls `up`.
		//
		// A claim with a deletion timestamp already carries the state this request asked for, so the
		// write is skipped for it: re-attributing an object on its way out is a second write, not a
		// cancellation.
		if nodeClaim.DeletionTimestamp.IsZero() {
			nodeClaim.Spec.Request.Call = infrav2alpha1.ClaimCallDecreaseTargetSize
			nodeClaim.Spec.Request.Delta = req.Delta
			nodeClaim.Spec.RequestedBy = claim.RequestedByAutoscaler

			if err := s.Client.Update(ctx, nodeClaim); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "failed to record the DecreaseTargetSize request on the claim", "claim", nodeClaim.Name, "group", req.Id)

				return nil, status.Errorf(codes.Internal, "failed to record the decrease request on claim %s of group %s: %v", nodeClaim.Name, req.Id, err)
			}
		}

		if err := s.Client.Delete(ctx, nodeClaim); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "failed to cancel a pending claim", "claim", nodeClaim.Name, "group", req.Id)

			return nil, status.Errorf(codes.Internal, "failed to cancel the pending claim %s of group %s: %v", nodeClaim.Name, req.Id, err)
		}

		cancelled = append(cancelled, nodeClaim.Name)
	}

	logger.Info("NodeGroupDecreaseTargetSize completed", "nodeGroup", req.Id, "delta", req.Delta,
		"claimsCancelled", cancelled, "currentSize", currentSize, "resultingSize", currentSize-toCancel,
		"bootingClaimsUntouched", countPhase(claims, infrav2alpha1.NodeClaimBooting))

	return &pb.NodeGroupDecreaseTargetSizeResponse{}, nil
}

// countPhase is the log rendering of how many claims of a group sit in one phase, and exists so that
// a decrease that declined to cancel a boot says so: "the autoscaler asked for -2 and the provider
// cancelled 1" is the question an operator asks next.
func countPhase(claims []infrav2alpha1.NodeClaim, to infrav2alpha1.NodeClaimPhase) int32 {
	var count int32

	for i := range claims {
		if claims[i].Status.Phase == to {
			count++
		}
	}

	return count
}

// NodeGroupNodes returns a list of all nodes that belong to this node group.
//
// The instances mirror the machines the provider has, and the contract is unchanged from v1alpha1:
// a slot with a Kubernetes node object is reported running while a live claim wants it and deleting
// when nothing wants it any more, a slot a live claim names that has not joined yet is reported as
// creating, and a slot no claim names and with no machine behind it is not reported at all.
//
// The word "wanted" is what moved: v1alpha1 read it off spec.desiredPowerState, v2alpha1 reads it off
// the live claim of the slot (docs/development/nodeclaim.md §5). The states keep their names, and the
// boundary the autoscaler depends on - every corev1.Node maps to exactly one instance, and an instance
// exists exactly when a machine exists or is on its way - is the same boundary.
func (s *HomeClusterProviderServer) NodeGroupNodes(ctx context.Context, req *pb.NodeGroupNodesRequest) (*pb.NodeGroupNodesResponse, error) {
	logger := log.Log.WithName("grpc-server")
	logger.Info("NodeGroupNodes called", "nodeGroup", req.Id)

	labelSelector := labels.SelectorFromSet(map[string]string{"group": req.Id})
	listOpts := &client.ListOptions{LabelSelector: labelSelector}
	nodes := &infrav2alpha1.NodeList{}
	err := s.Client.List(ctx, nodes, listOpts)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, status.Errorf(codes.InvalidArgument, "group not found: %s", req.Id)
		}
	}

	claims, err := s.groupClaims(ctx, req.Id)
	if err != nil {
		logger.Error(err, "failed to list the claims of group", "nodeGroup", req.Id)

		return nil, status.Errorf(codes.Internal, "failed to list the claims of node group %s: %v", req.Id, err)
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
		// The slot is wanted exactly when a live claim names it. A claim marked for scale-down is not
		// a want: the machine is going away, which is the deleting state.
		live := claim.LiveOfSlot(claimsOfSlot(claims, node.Name))
		wanted := claim.WantsMachine(live)

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

		if !hasKubernetesNode && !wanted {
			// A slot no live claim names, with no machine behind it, is capacity the provider could
			// create, not an instance that exists. Reporting it inflates the current size of the group,
			// which makes the autoscaler provision extra machines.
			continue
		}

		// A claimed slot whose node has not joined yet is still provisioning; a slot whose machine is
		// left over with nothing wanting it (no claim, or a claim a scale-down marked) is being powered
		// off.
		instanceState := pb.InstanceStatus_instanceCreating
		if hasKubernetesNode {
			if wanted {
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
		//
		// An Instance carries an id and a status, and nothing else: the proto has no labels, no
		// annotations, no capacity. What cluster-autoscaler knows about a machine that already
		// registered it reads off that machine's corev1.Node, which the cluster registry lists, so
		// the labels a slot declares have to be on the node object to be visible to the autoscaler
		// for an existing node - that is nodelabels.ApplyToKubernetesNode's job, on join and on every
		// drift. The template is the other half: the shape of a machine that does not exist yet, and
		// the only place a scale-up can be matched against a label.
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
	nodes := &infrav2alpha1.NodeList{}
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

	// The labels and annotations the group can answer for: every slot's declaration, restricted to
	// the keys the slots agree on. Computed here, from every slot of the group, because the template
	// is one answer for the whole group and the scale-up it authorises buys any slot of it.
	consensusLabels, consensusAnnotations := s.groupTemplateConsensus(req.Id, nodes.Items)

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

	// Put the labels and annotations the slots of the group declare into the template, after the copy
	// and before the marshal, in the same position applyTemplateCapacity's call has, so that the rule
	// is the same for the node copied from a live slot and for the synthesized placeholder.
	//
	// This is the half of the feature cluster-autoscaler reads. The estimator matches a pending pod's
	// nodeSelector and node affinity against the labels of this template, so a pod that asks for
	// `topology.kubernetes.io/zone: fsn1` is placed on a group whose template carries that label and
	// on no other: with every slot reporting the same template the answer to "which slot?" is "any
	// slot", and a zoned pod is never placed on this pool at all, so no machine is ever bought for
	// it. Declaring the zone on the slots that have one is what makes the scale-up possible.
	//
	// The declaration goes on top of the copy, and only the declared keys: what the representative
	// node carries beyond them is the machine's own shape, which the template keeps. The predicates
	// are the ones of the kind being written (labels filtered by the label list, annotations by the
	// annotation list), so a label under a namespace that is only reserved for annotations -
	// `argocd.argoproj.io/team`, say - reaches the template a `nodeSelector` matches against.
	//
	// What is overlaid is the group's consensus, not the representative slot's declaration: a key the
	// slots of the group do not all declare with the same value is omitted, and the omission is
	// reported. See nodelabels.Consensus for the arithmetic and groupTemplateConsensus for the
	// consequence this avoids - a machine bought for a pod that cannot run on it.
	templateNode.Labels = nodelabels.OverlayLabels(templateNode.Labels, consensusLabels)
	templateNode.Annotations = nodelabels.OverlayAnnotations(templateNode.Annotations, consensusAnnotations)

	// Remove node-specific labels that shouldn't be in template. After the overlay, so a
	// declaration cannot put the representative's hostname back: the key is refused at admission
	// (kubernetes.io/ is reserved) and this is the write that proves it.
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

// groupTemplateConsensus is the template's fail-closed rule: of everything the slots of a group
// declare, the template carries only the keys every slot of the group declares with the same value.
//
// The reason is the shape of the two calls cluster-autoscaler makes. `NodeGroupTemplateNodeInfo`
// answers for a *group* with *one* node, and the scale-up it authorises is `IncreaseSize`, which
// buys whichever slot the provider has free (sorted by name). A template that carries a label only
// some slots carry therefore makes a promise the group cannot keep: the estimator places the pod on
// the template, the provider boots a slot whose machine will never carry the label, the pod is
// still `Pending` after the boot, the slot is Ready, and the money is spent. Omitting the key is
// the honest answer: the estimator matches the selector against nothing, plans nothing, buys no
// machine, and the pod stays `Pending` on a pool that is visibly not the one for it - which is what
// a pool whose slots differ *is*.
//
// The disagreement is not an error, and it does not fail the call: the group is exact and useful
// for every pod that does not select the disputed key, a call that returns an error makes
// cluster-autoscaler skip the group for *every* pod, and a pool that cannot grow is a worse
// outcome than a pool that grows only for the work it can host. It is reported, per group, when it
// appears and when it changes (see recordTemplateConflicts), naming the key and the slots, because
// the fix is configuration: declare the key on every slot of the group, or put the slots the
// scheduler has to tell apart in different groups.
func (s *HomeClusterProviderServer) groupTemplateConsensus(group string, slots []infrav2alpha1.Node) (map[string]string, map[string]string) {
	labelDeclarations := make([]nodelabels.SlotDeclaration, 0, len(slots))
	annotationDeclarations := make([]nodelabels.SlotDeclaration, 0, len(slots))

	for i := range slots {
		labelDeclarations = append(labelDeclarations,
			nodelabels.SlotDeclaration{Name: slots[i].Name, Declared: slots[i].Spec.NodeLabels})
		annotationDeclarations = append(annotationDeclarations,
			nodelabels.SlotDeclaration{Name: slots[i].Name, Declared: slots[i].Spec.NodeAnnotations})
	}

	agreedLabels, labelConflicts := nodelabels.Consensus(labelDeclarations)
	agreedAnnotations, annotationConflicts := nodelabels.Consensus(annotationDeclarations)

	s.recordTemplateConflicts(group, labelConflicts, annotationConflicts)

	return agreedLabels, agreedAnnotations
}

// recordTemplateConflicts reports a group's disagreements, once each: the provider answers this
// call for every group on every scan of cluster-autoscaler (tens of times an hour), and a log line
// per scan is the noise that hides the line that matters. The memory is per process (a restart
// re-states it), keyed by group, and it says nothing the CRs do not already say: it is the pointer
// to the configuration that has to change.
func (s *HomeClusterProviderServer) recordTemplateConflicts(group string, labelConflicts, annotationConflicts []nodelabels.GroupConflict) {
	logger := log.Log.WithName("grpc-server")

	rendered := ""
	if note := nodelabels.RenderConflicts(labelConflicts); note != "" {
		rendered = "labels [" + note + "]"
	}

	if note := nodelabels.RenderConflicts(annotationConflicts); note != "" {
		if rendered != "" {
			rendered += " annotations [" + note + "]"
		} else {
			rendered = "annotations [" + note + "]"
		}
	}

	previous, seen := s.templateConflicts.Load(group)

	if rendered == "" {
		if seen {
			s.templateConflicts.Delete(group)

			logger.Info("A group's slots agree on their declared node labels again, the template is exact",
				"nodeGroup", group)
		}

		return
	}

	if seen && previous.(string) == rendered {
		return
	}

	s.templateConflicts.Store(group, rendered)

	logger.Info("NodeGroupTemplateNodeInfo: the slots of a group disagree on their declared node metadata, "+
		"the keys named are left out of the group's template so cluster-autoscaler cannot simulate a label "+
		"the group does not guarantee",
		"nodeGroup", group, "disagreements", rendered,
		"fix", "declare the key with the same value on every slot of the group, or put the slots the "+
			"scheduler must tell apart in different groups")
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
//     the shape the slot declares in spec.nodeCapacity plus the kubelet's default pod ceiling, and
//     no size the provider guessed - and a lookup that failed for a reason other than "the node
//     does not exist" is reported instead of being turned into an empty template.
//
// The returned Node CR is never nil when the error is nil.
func (s *HomeClusterProviderServer) pickTemplateSource(ctx context.Context, items []infrav2alpha1.Node) (*infrav2alpha1.Node, *corev1.Node, error) {
	logger := log.Log.WithName("grpc-server")

	var (
		best       *infrav2alpha1.Node
		bestNode   *corev1.Node
		bestReady  bool
		poweredOff *infrav2alpha1.Node
		lastErr    error
	)

	for i := range items {
		candidate := &items[i]
		if poweredOff == nil && candidate.Status.PowerState == infrav2alpha1.PowerStateOff {
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
func kubernetesNodeNameOf(node *infrav2alpha1.Node) string {
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

// defaultTemplateMaxPods is the pod ceiling a template carries when nothing names it. It is the
// kubelet's own default (--max-pods, 110), and it is the number cluster-autoscaler's own node
// template builders put into the machines they hand the estimator: the gce, aws, azure and
// hetzner builders all set corev1.ResourcePods in the template's allocatable and capacity.
const defaultTemplateMaxPods = 110

// defaultTemplateCapacity is the starting shape of a template's resource lists.
//
// pods is in it because it is the one resource the scheduler needs that a declaration is not
// expected to name: cpu and memory are facts about one specific machine, copied off its kubelet,
// while the pod ceiling is a kubelet flag with a fixed default. It is also the resource the
// scheduler judges first. framework.NodeInfo derives AllowedPodNumber from v1.ResourcePods alone,
// and NodeResourcesFit.fitsRequest tests "Too many pods" *before* its zero-request early return,
// so a template whose allocatable has no pods gives AllowedPodNumber 0 and every pod is rejected -
// with requests, and without them. The estimator then finds no placement for the pending pod,
// NodeGroupIncreaseSize is never called, and a minSize: 0 pool stays at zero: the documented
// declaration names cpu and memory, so filling the pod ceiling in is the provider's job.
//
// Nothing else is defaulted. cpu, memory, ephemeral-storage, hugepages and device plugin
// resources describe a machine, and inventing them invents hardware the pool may not own: a pool
// that declares nothing keeps being told "this machine has no known size" for those.
var defaultTemplateCapacity = corev1.ResourceList{
	corev1.ResourcePods: *resource.NewQuantity(defaultTemplateMaxPods, resource.DecimalSI),
}

// applyTemplateCapacity puts the machine size into the template: the defaults a schedulable
// template needs, then what the copy came with, then the slot's declaration. The node copied from
// a live slot and the synthesized placeholder go through this one function, so the two paths
// cannot drift apart.
//
// Precedence, per resource, spelled out because it is the whole point of the field:
//
//   - spec.nodeCapacity, when the slot declares it, wins for every resource it names, over the
//     live node's status.capacity and status.allocatable and over the defaults. The template is
//     the shape of the machine the autoscaler is about to boot, and the declaration is that shape
//     on purpose; what a slot happens to report right now is an accident of which slot is Ready.
//     A group whose slots are not identical (a mixed pool, a slot mid-replacement, a node object
//     its powered off machine left behind) would otherwise hand the estimator a different capacity
//     per call, and a pool at zero has no live node at all, which is the case the field exists for;
//   - a resource the declaration does not name is left alone, not dropped. A real node reports
//     pods, ephemeral-storage, hugepages, nvidia.com/gpu, and whatever a device plugin adds, and
//     the estimator judges a pod against all of them. Declaring cpu and memory declares the shape
//     of the box, not the list of resources it has, so the overlay is per resource: replacing the
//     whole list with the declaration is what made the documented declaration unplaceable, because
//     the template lost pods and the scheduler answered every pod "Too many pods";
//   - with no declaration, the live node's own numbers stay in the template: the kubelet measured
//     them, and that is today's behaviour for every pool that declares nothing. On the placeholder
//     path there is no live node, so an undeclared pool carries the defaults and nothing else: the
//     pod ceiling is the kubelet's default, and the size of a machine that never registered stays
//     unknown rather than being guessed.
//
// The declaration is applied to status.capacity as well as status.allocatable: the estimator
// judges requests against allocatable, and cluster-autoscaler's registered-node bookkeeping
// (quota simulation, resource reporting) reads capacity, so a template that fills only one of the
// two is judged by the other. A template machine has no system daemons to subtract, so on the
// placeholder path the two lists end up identical; on the live path each keeps its own numbers,
// overlaid resource by resource.
func applyTemplateCapacity(templateNode *corev1.Node, slot *infrav2alpha1.Node) {
	overlayDeclaredCapacity(&templateNode.Status.Capacity, slot.Spec.NodeCapacity)
	overlayDeclaredCapacity(&templateNode.Status.Allocatable, slot.Spec.NodeCapacity)
}

// overlayDeclaredCapacity merges one of a template's resource lists: the defaults for any
// resource the list does not already carry, then the declaration, resource by resource. Nothing
// is removed, and nothing a live node reported is overwritten by a default.
//
// Every map written here is a new one and every quantity a copy: the Node CR the provider reads
// comes out of the manager's cache, so aliasing its map, or the package-level defaults, into the
// template is a mutation of shared state waiting to happen, and capacity and allocatable must not
// become one map under two names.
func overlayDeclaredCapacity(list *corev1.ResourceList, declared corev1.ResourceList) {
	merged := make(corev1.ResourceList, len(*list)+len(defaultTemplateCapacity)+len(declared))

	for name, quantity := range *list {
		merged[name] = quantity
	}

	for name, quantity := range defaultTemplateCapacity {
		if _, carried := merged[name]; !carried {
			merged[name] = quantity.DeepCopy()
		}
	}

	for name, quantity := range declared {
		merged[name] = quantity.DeepCopy()
	}

	*list = merged
}

// placeholderTemplateNode builds the object the template is derived from when no slot of the
// group has a corev1.Node: a fully powered off pool. It carries everything that can be read from
// the Node CR alone: the group label, the taints the slot declares, and the capacity the slot
// declares, on top of the defaults applyTemplateCapacity fills in. No machine size is invented:
// cpu, memory, ephemeral-storage of a machine that has never registered cannot be read, so an
// undeclared placeholder says "I do not know the size of this machine" by carrying none of them,
// and the pool that declared spec.nodeCapacity is answering the question from Git. The pod ceiling
// is the one number that is not a fact about the hardware, and it is defaulted so the scheduler can
// place a pod on the template at all.
//
// The declared labels and annotations are not overlaid here. One caller overlays them, on the
// template it builds from whatever source it picked - this placeholder or a live node - so the rule
// is stated once and the two paths cannot drift apart, and so the labels that reach the estimator
// are the group's consensus rather than one slot's declaration (groupTemplateConsensus).
//
// Those labels are the reason a pool at zero can be chosen at all: the estimator matches a pod's
// nodeSelector against this object, and the case that matters, a pool in a specific zone, is
// answered from Git, because a pool whose every machine is off has no node object to read the zone off.
func placeholderTemplateNode(slot *infrav2alpha1.Node) *corev1.Node {
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

	group := &infrav2alpha1.Group{}
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
