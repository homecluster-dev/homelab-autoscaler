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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

const testGroup = "remote-workers"

// testNamespace is the namespace the gRPC server resolves Group and NodeClaim CRs in
// (internal/config.NamespaceConfig default). Slots of the test group live in "default", the
// fixtures that exercise a Group CR live here, because that is where the provider looks.
const testNamespace = "homelab-autoscaler-system"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	return scheme
}

// nodeCR builds a Node CR: a slot, and a slot only.
//
// The signature is the model. In v2alpha1 the spec has no runtime intent in it, so the two
// power-ish arguments a fixture can name are the observation (status.powerState) and the report
// (status.phase), and neither is what an RPC acts on: what makes a machine wanted is the
// NodeClaim next to it (see claimCR).
func nodeCR(name string, observed infrav2alpha1.PowerState, report infrav2alpha1.NodePhase) *infrav2alpha1.Node {
	return &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{"group": testGroup},
		},
		Spec: infrav2alpha1.NodeSpec{
			KubernetesNodeName: name,
		},
		Status: infrav2alpha1.NodeStatus{
			PowerState: observed,
			Phase:      report,
		},
	}
}

// nodeCRInGroup builds a slot of an arbitrary group.
func nodeCRInGroup(group, name string, observed infrav2alpha1.PowerState, report infrav2alpha1.NodePhase) *infrav2alpha1.Node {
	node := nodeCR(name, observed, report)
	node.Labels["group"] = group

	return node
}

// slotWithoutKubernetesNodeName builds a slot whose spec.kubernetesNodeName is empty: a slot
// that has never been provisioned.
func slotWithoutKubernetesNodeName(name string) *infrav2alpha1.Node {
	node := nodeCR(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	node.Spec.KubernetesNodeName = ""

	return node
}

// claimCR is the claim of a slot: the object the autoscaler's request becomes. It carries the
// labels the provider lists on, and the attribution IncreaseSize stamps.
func claimCR(slotName string, claimPhase infrav2alpha1.NodeClaimPhase, mutate func(*infrav2alpha1.NodeClaim)) *infrav2alpha1.NodeClaim {
	nodeClaim := &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claim-" + slotName,
			Namespace: testNamespace,
			Labels: map[string]string{
				claim.LabelSlot:  slotName,
				claim.LabelGroup: testGroup,
			},
		},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef: corev1.ObjectReference{
				APIVersion: infrav2alpha1.GroupVersion.String(),
				Kind:       "Node",
				Name:       slotName,
			},
			RequestedBy: claim.RequestedByAutoscaler,
			Request: infrav2alpha1.ClaimRequest{
				Call:  infrav2alpha1.ClaimCallIncreaseSize,
				Delta: 1,
				Group: testGroup,
			},
		},
		Status: infrav2alpha1.NodeClaimStatus{
			Phase:   claimPhase,
			Attempt: 1,
		},
	}

	// A live claim carries the controller's finalizer from the moment its boot starts: deleting it
	// is then the scale-down request, not an abandonment (docs/development/nodeclaim.md §3). A pending
	// claim carries none, which is what makes cancelling it a plain delete.
	if claimPhase == infrav2alpha1.NodeClaimBooting || claimPhase == infrav2alpha1.NodeClaimReady {
		nodeClaim.Finalizers = append(nodeClaim.Finalizers, claimFinalizer)
	}

	if mutate != nil {
		mutate(nodeClaim)
	}

	return nodeClaim
}

// claimFinalizer is the finalizer the claim controller stamps on a committed boot.
const claimFinalizer = "infra.homecluster.dev/nodeclaim-finalizer"

// runningSlot is a slot whose machine is up and takes work: the observation says on, and a live
// claim in ready is the request that put it there. The pair is what a v1alpha1 fixture wrote as
// one scalar.
func runningSlot(t *testing.T, name string) []client.Object {
	t.Helper()

	return []client.Object{nodeCR(name, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady), claimCR(name, infrav2alpha1.NodeClaimReady, nil)}
}

// bootingSlot is a slot whose startup has started: a live claim in booting, the machine not up
// yet. It is capacity, it is counted, and it is committed: no caller cancels it.
func bootingSlot(name string) []client.Object {
	return []client.Object{
		nodeCR(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
		claimCR(name, infrav2alpha1.NodeClaimBooting, nil),
	}
}

// pendingSlot is a slot a request has claimed and nothing has started: the cancelable shape.
func pendingSlot(name string) []client.Object {
	return []client.Object{
		nodeCR(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhasePending),
		claimCR(name, infrav2alpha1.NodeClaimPending, nil),
	}
}

// idleSlot is a free slot: no claim names it, its machine is off.
func idleSlot(name string) []client.Object {
	return []client.Object{nodeCR(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)}
}

// k8sNode builds a Kubernetes node, optionally with a providerID set by a
// cloud-controller-manager or by the operator.
func k8sNode(name, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

// groupCR builds a Group CR with a per-group minimum size.
func groupCR(name string, minSize int32) *infrav2alpha1.Group {
	return &infrav2alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       infrav2alpha1.GroupSpec{MinSize: minSize},
	}
}

// groupWithMaxProvision builds a Group CR that also declares spec.maxNodeProvisionTime, the
// floor of a claim's deadline.
func groupWithMaxProvision(name string, minSize int32, maxNodeProvisionTime time.Duration) *infrav2alpha1.Group {
	group := groupCR(name, minSize)
	group.Spec.MaxNodeProvisionTime = &metav1.Duration{Duration: maxNodeProvisionTime}

	return group
}

func externalNodes(names ...string) []*pb.ExternalGrpcNode {
	nodes := make([]*pb.ExternalGrpcNode, 0, len(names))
	for _, name := range names {
		nodes = append(nodes, &pb.ExternalGrpcNode{Name: name})
	}

	return nodes
}

func newServer(t *testing.T, objects ...client.Object) *HomeClusterProviderServer {
	t.Helper()

	scheme := testScheme(t)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&infrav2alpha1.Node{}, &infrav2alpha1.NodeClaim{}).
		Build()

	return NewHomeClusterProviderServer(k8sClient, scheme)
}

// flatten is the fixtures' way of seeding a pool out of slot shapes.
func flatten(groups ...[]client.Object) []client.Object {
	out := make([]client.Object, 0, len(groups))
	for _, group := range groups {
		out = append(out, group...)
	}

	return out
}

// claimsOf lists the claims the provider has produced, by name.
func claimsOf(t *testing.T, server *HomeClusterProviderServer) []infrav2alpha1.NodeClaim {
	t.Helper()

	list := &infrav2alpha1.NodeClaimList{}
	require.NoError(t, server.Client.List(context.Background(), list))

	return list.Items
}

// slotsOf reads the slots the provider has, by name, so a test can assert what an RPC left
// behind on a slot.
func slotsOf(t *testing.T, server *HomeClusterProviderServer, names ...string) map[string]*infrav2alpha1.Node {
	t.Helper()

	got := make(map[string]*infrav2alpha1.Node, len(names))

	for _, name := range names {
		node := &infrav2alpha1.Node{}
		require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: testNamespace}, node))
		got[name] = node
	}

	return got
}

// TestNodeGroupNodes is the instance contract of §5 of the design, unchanged in its shape and
// moved in its input: a slot is "wanted" when a live claim names it, not when a spec field says
// so. The rows are the ones cluster-autoscaler's registration and scale-down loops depend on:
// every corev1.Node maps to exactly one instance, and an instance exists exactly when a machine
// exists or is on its way.
func TestNodeGroupNodes(t *testing.T) {
	tests := []struct {
		name string
		// objects to seed the fake client with
		objects []client.Object
		// expected instance id -> instance state
		expected map[string]pb.InstanceStatus_InstanceState
	}{
		{
			// A slot with no machine, no node object and no claim is spare capacity, not an
			// instance. Reporting it inflates the current size of the group, which makes the
			// autoscaler provision extra machines.
			name: "a free slot without a kubernetes node is skipped",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{},
		},
		{
			// The machine is off, the kubelet's node object is left behind NotReady, and no
			// claim wants it: the instance is going away. Cluster-autoscaler matches the node
			// to it by spec.providerID, so it must not look like an unregistered node.
			name: "an unclaimed slot with a kubernetes node is deleting",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
				k8sNode("slot-1", "homelab://slot-1"),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceDeleting,
			},
		},
		{
			name: "an unclaimed slot whose kubernetes node has no providerID falls back to the slot identity",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
				k8sNode("slot-1", ""),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceDeleting,
			},
		},
		{
			name: "a slot with an empty kubernetesNodeName is skipped",
			objects: []client.Object{
				slotWithoutKubernetesNodeName("slot-1"),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{},
		},
		{
			// creating: a live claim in the boot window, and no node object yet.
			name: "a booting claim with no kubernetes node is creating",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
				claimCR("slot-1", infrav2alpha1.NodeClaimBooting, nil),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceCreating,
			},
		},
		{
			// A pending claim names a machine that is wanted and has not started: the same
			// creating state, and the row v1alpha1 could not express at all (the shape was
			// {desired: on, progress: shutdown}, indistinguishable from a free slot).
			name: "a pending claim with no kubernetes node is creating",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhasePending),
				claimCR("slot-1", infrav2alpha1.NodeClaimPending, nil),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceCreating,
			},
		},
		{
			name: "a ready claim with a kubernetes node is running",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
				claimCR("slot-1", infrav2alpha1.NodeClaimReady, nil),
				k8sNode("slot-1", "homelab://slot-1"),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceRunning,
			},
		},
		{
			name: "a ready claim and an empty providerID keep the id the operator writes",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
				claimCR("slot-1", infrav2alpha1.NodeClaimReady, nil),
				k8sNode("slot-1", ""),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceRunning,
			},
		},
		{
			// A slot a claim ended (failed) with its node object left behind: the machine is
			// not wanted, so the instance is deleting, and the claim is not counted by
			// TargetSize. The two readings are the same fact about the same object.
			name: "a failed claim with a kubernetes node is deleting",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
				claimCR("slot-1", infrav2alpha1.NodeClaimFailed, nil),
				k8sNode("slot-1", "homelab://slot-1"),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceDeleting,
			},
		},
		{
			name: "a mixed pool reports every machine and skips spare capacity",
			objects: flatten(
				idleSlot("slot-seeded"),
				[]client.Object{
					nodeCR("slot-off", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
					k8sNode("slot-off", "homelab://slot-off"),
				},
				[]client.Object{
					nodeCR("slot-new", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
					claimCR("slot-new", infrav2alpha1.NodeClaimBooting, nil),
				},
				[]client.Object{
					nodeCR("omni-remote-1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
					claimCR("omni-remote-1", infrav2alpha1.NodeClaimReady, nil),
					k8sNode("omni-remote-1", ""),
				},
			),
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-off":      pb.InstanceStatus_instanceDeleting,
				"homelab://slot-new":      pb.InstanceStatus_instanceCreating,
				"homelab://omni-remote-1": pb.InstanceStatus_instanceRunning,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer(t, tc.objects...)

			resp, err := server.NodeGroupNodes(context.Background(), &pb.NodeGroupNodesRequest{Id: testGroup})
			require.NoError(t, err)
			require.NotNil(t, resp)

			got := make(map[string]pb.InstanceStatus_InstanceState, len(resp.Instances))
			for _, instance := range resp.Instances {
				require.NotEmpty(t, instance.Id, "instance id must never be empty")
				require.NotNil(t, instance.Status)
				got[instance.Id] = instance.Status.InstanceState
			}

			assert.Len(t, resp.Instances, len(tc.expected))
			assert.Equal(t, tc.expected, got)
		})
	}
}

// TestNodeGroupNodesStableIdentityAcrossProvisioning: cluster-autoscaler keys instances by id,
// and the operator stamps providerID "homelab://<slot>" when the machine joins. The id reported
// before the node object exists must be the same, or a single pending pod produces two scale-ups.
// The identity crosses the boundary while the claim moves pending -> booting -> ready, which is
// the sequence a scale-up now writes.
func TestNodeGroupNodesStableIdentityAcrossProvisioning(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhasePending),
		claimCR("slot-1", infrav2alpha1.NodeClaimPending, nil),
	)

	ctx := context.Background()

	resp, err := server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	require.Len(t, resp.Instances, 1)
	provisioningID := resp.Instances[0].Id
	assert.Equal(t, pb.InstanceStatus_instanceCreating, resp.Instances[0].Status.InstanceState)

	// The boot is committed, the machine joins, the providerID is stamped.
	setClaimPhase(t, server, "slot-1", infrav2alpha1.NodeClaimBooting)
	require.NoError(t, server.Client.Create(ctx, k8sNode("slot-1", "homelab://slot-1")))

	resp, err = server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	require.Len(t, resp.Instances, 1)
	assert.Equal(t, pb.InstanceStatus_instanceRunning, resp.Instances[0].Status.InstanceState,
		"the node object is the machine: while the claim is still booting the instance is reported running, which is the contract unchanged from v1alpha1 (every corev1.Node maps to exactly one instance)")
	assert.Equal(t, provisioningID, resp.Instances[0].Id, "instance id must not change once the node joins")
}

// TestNodeGroupNodesIgnoresEmptyKubernetesNodeName: a slot with no spec.kubernetesNodeName has
// never been provisioned, so the Get is skipped and the call keeps returning one stable
// provisioning instance instead of a non-NotFound error every time.
func TestNodeGroupNodesIgnoresEmptyKubernetesNodeName(t *testing.T) {
	slot := slotWithoutKubernetesNodeName("slot-1")
	server := newServer(t, slot, claimCR("slot-1", infrav2alpha1.NodeClaimPending, nil))

	for range 3 {
		resp, err := server.NodeGroupNodes(context.Background(), &pb.NodeGroupNodesRequest{Id: testGroup})
		require.NoError(t, err)
		require.Len(t, resp.Instances, 1)
		assert.Equal(t, "homelab://slot-1", resp.Instances[0].Id)
		assert.Equal(t, pb.InstanceStatus_instanceCreating, resp.Instances[0].Status.InstanceState)
	}
}

// TestNodeGroupNodesInstancePerMachine is the contract one instance per machine, told by the
// claims: running (a live claim, node present), creating (a live claim, no node yet), deleting
// (a machine no live claim wants), and nothing at all for a free slot.
func TestNodeGroupNodesInstancePerMachine(t *testing.T) {
	server := newServer(t, flatten(
		[]client.Object{
			nodeCR("slot-running", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
			claimCR("slot-running", infrav2alpha1.NodeClaimReady, nil),
			k8sNode("slot-running", "homelab://slot-running"),
		},
		[]client.Object{
			nodeCR("slot-creating", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
			claimCR("slot-creating", infrav2alpha1.NodeClaimBooting, nil),
		},
		[]client.Object{
			nodeCR("slot-deleting", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
			k8sNode("slot-deleting", "homelab://slot-deleting"),
		},
		idleSlot("slot-spare"),
	)...)

	resp, err := server.NodeGroupNodes(context.Background(), &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)

	got := make(map[string]pb.InstanceStatus_InstanceState, len(resp.Instances))
	for _, instance := range resp.Instances {
		require.NotEmpty(t, instance.Id, "instance id must never be empty")
		require.NotNil(t, instance.Status)
		require.NotNil(t, instance.Status.ErrorInfo, "error info must stay present and empty")
		require.Empty(t, instance.Status.ErrorInfo.ErrorCode)
		got[instance.Id] = instance.Status.InstanceState
	}

	assert.Equal(t, map[string]pb.InstanceStatus_InstanceState{
		"homelab://slot-running":  pb.InstanceStatus_instanceRunning,
		"homelab://slot-creating": pb.InstanceStatus_instanceCreating,
		"homelab://slot-deleting": pb.InstanceStatus_instanceDeleting,
	}, got, "a slot with no machine and no claim must not be reported")
}

// TestNodeGroupsReportsGroupMinSize: the Group CR is the source of the floor, and a floor above
// the number of slots the provider has is clamped to it: maxSize is derived from the Node CR
// count, so the CRD cannot assert minSize <= maxSize.
func TestNodeGroupsReportsGroupMinSize(t *testing.T) {
	server := newServer(t, flatten(
		[]client.Object{groupCR(testGroup, 1)},
		runningSlot(t, "slot-running"),
		idleSlot("slot-spare"),
		idleSlot("slot-off"),
		[]client.Object{groupCR("group-no-min", 0)},
		[]client.Object{nodeCRInGroup("group-no-min", "spare-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)},
		[]client.Object{groupCR("group-over-min", 5)},
		[]client.Object{nodeCRInGroup("group-over-min", "only-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)},
		[]client.Object{nodeCRInGroup("group-over-min", "only-2", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)},
		[]client.Object{groupCR("group-empty", 1)},
	)...)

	resp, err := server.NodeGroups(context.Background(), &pb.NodeGroupsRequest{})
	require.NoError(t, err)

	got := make(map[string][2]int32, len(resp.NodeGroups))
	for _, group := range resp.NodeGroups {
		got[group.Id] = [2]int32{group.MinSize, group.MaxSize}
	}

	assert.Equal(t, map[string][2]int32{
		testGroup:        {1, 3},
		"group-no-min":   {0, 1},
		"group-over-min": {2, 2}, // clamped: there is no floor the provider cannot fill
		"group-empty":    {0, 0},
	}, got)
}

// TestNodeGroupForNodeReportsGroupMinSize covers the same floor on the per-node path.
func TestNodeGroupForNodeReportsGroupMinSize(t *testing.T) {
	server := newServer(t, flatten(
		[]client.Object{groupCR(testGroup, 2)},
		[]client.Object{&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   "slot-off",
			Labels: map[string]string{"infra.homecluster.dev/group": testGroup},
		}}},
		idleSlot("slot-off"),
		idleSlot("slot-spare"),
	)...)

	resp, err := server.NodeGroupForNode(context.Background(), &pb.NodeGroupForNodeRequest{
		Node: &pb.ExternalGrpcNode{Name: "slot-off"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.NodeGroup)

	assert.Equal(t, testGroup, resp.NodeGroup.Id)
	assert.Equal(t, int32(2), resp.NodeGroup.MinSize)
	assert.Equal(t, int32(2), resp.NodeGroup.MaxSize)
}

// setClaimPhase moves a claim's phase on the status subresource, the way the claim controller and
// the lifecycle transitions do. A fixture that needs a pool in a shape writes the shape, and the
// phase table itself is internal/claim's own test subject.
func setClaimPhase(t *testing.T, server *HomeClusterProviderServer, slotName string, to infrav2alpha1.NodeClaimPhase) {
	t.Helper()

	nodeClaim := &infrav2alpha1.NodeClaim{}
	require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: "claim-" + slotName, Namespace: testNamespace}, nodeClaim))

	nodeClaim.Status.Phase = to
	require.NoError(t, server.Client.Status().Update(context.Background(), nodeClaim))
}

// TestTargetSizeOfCountsLiveClaims is the arithmetic of §5 as a unit: the target size is the
// count of live claims, and the only rows that count are pending, booting and ready. No progress
// value, and no slot spec field, appears in it.
func TestTargetSizeOfCountsLiveClaims(t *testing.T) {
	claims := []infrav2alpha1.NodeClaim{
		*claimCR("slot-pending", infrav2alpha1.NodeClaimPending, nil),
		*claimCR("slot-booting", infrav2alpha1.NodeClaimBooting, nil),
		*claimCR("slot-ready", infrav2alpha1.NodeClaimReady, nil),
		*claimCR("slot-failed", infrav2alpha1.NodeClaimFailed, nil),
		*claimCR("slot-completed", infrav2alpha1.NodeClaimCompleted, nil),
	}

	assert.Equal(t, int32(3), targetSizeOf(claims))
	assert.Equal(t, int32(0), targetSizeOf(nil))

	// A claim a scale-down marked is capacity that is going away: it comes off the number on the
	// write that names it, the same instant a pending claim's deletion does.
	marked := claimCR("slot-marked", infrav2alpha1.NodeClaimReady, nil)
	now := metav1.Now()
	marked.DeletionTimestamp = &now

	assert.Equal(t, int32(1), targetSizeOf([]infrav2alpha1.NodeClaim{*claimCR("slot-ready", infrav2alpha1.NodeClaimReady, nil), *marked}))
}

// TestNodeGroupTargetSizeCountsLiveClaims is the same rule at the gRPC boundary, and the two
// states the v1alpha1 arithmetic could not express: a machine that never arrived is not paid for
// once its claim is failed, and a cancelled startup is off the number on the write that cancels
// it.
func TestNodeGroupTargetSizeCountsLiveClaims(t *testing.T) {
	server := newServer(t, flatten(
		pendingSlot("slot-1"),
		bootingSlot("slot-2"),
		runningSlot(t, "slot-3"),
		// The machine that never arrived: {desired: on, progress: ready} counted forever under
		// v1alpha1, and off the number the moment its claim is recorded failed.
		[]client.Object{
			nodeCR("slot-4", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
			claimCR("slot-4", infrav2alpha1.NodeClaimFailed, nil),
		},
		// The episode that landed and was released: not capacity, and not counted.
		[]client.Object{
			nodeCR("slot-5", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
			claimCR("slot-5", infrav2alpha1.NodeClaimCompleted, nil),
		},
		// A free slot of the pool: no claim, no machine, nothing counted.
		idleSlot("slot-6"),
	)...)

	resp, err := server.NodeGroupTargetSize(context.Background(), &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(3), resp.TargetSize, "pending + booting + ready; failed, completed and free slots are not counted")
}

// TestNodeGroupIncreaseSizeCreatesClaimsWithTheAttributionFilledIn is the point of the whole
// model at the provider boundary: a delta of N creates N claims on N free slots, and each one
// carries the request - who asked (requestedBy), which call, with what delta, for which group -
// with a deadline and an attempt seeded by the claim controller. The v1alpha1 handler claimed the
// first slot a three-field scan found and returned, one slot per call whatever delta said, and
// the entire record of the request was the scalar it wrote.
func TestNodeGroupIncreaseSizeCreatesClaimsWithTheAttributionFilledIn(t *testing.T) {
	server := newServer(t, flatten(
		[]client.Object{groupWithMaxProvision(testGroup, 0, 15*time.Minute)},
		idleSlot("slot-1"),
		idleSlot("slot-2"),
		idleSlot("slot-3"),
	)...)

	_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 2})
	require.NoError(t, err)

	claims := claimsOf(t, server)
	require.Len(t, claims, 2, "delta 2 creates two claims, one per free slot")

	slots := make([]string, 0, len(claims))
	for _, nodeClaim := range claims {
		assert.Equal(t, claim.RequestedByAutoscaler, nodeClaim.Spec.RequestedBy, "the caller's identity is on the claim")
		assert.Equal(t, infrav2alpha1.ClaimCallIncreaseSize, nodeClaim.Spec.Request.Call, "the RPC that created it")
		assert.Equal(t, int32(2), nodeClaim.Spec.Request.Delta, "the request's own delta, signed")
		assert.Equal(t, testGroup, nodeClaim.Spec.Request.Group, "the group the request was for")
		assert.Equal(t, testGroup, nodeClaim.Labels[claim.LabelGroup], "the label the pool's claims are listed by")
		assert.NotEmpty(t, nodeClaim.Spec.SlotRef.Name)
		assert.NotEmpty(t, nodeClaim.Spec.RequestedAt.Time, "the deadline budget is measured from it")
		assert.Empty(t, nodeClaim.Spec.Request.Nodes, "IncreaseSize names no nodes")

		require.NotNil(t, nodeClaim.Spec.DeadlineAt, "a claim is born with a budget")
		budget := nodeClaim.Spec.DeadlineAt.Sub(nodeClaim.Spec.RequestedAt.Time)
		assert.Equal(t, 15*time.Minute, budget,
			"the deadline is the operator's budget floored by the group's spec.maxNodeProvisionTime, which is the number cluster-autoscaler is told as its own termination deadline")
		require.NotNil(t, nodeClaim.Spec.MaxNodeProvisionTime, "the claim records the budget that was in force when it was created")
		assert.Equal(t, 15*time.Minute, nodeClaim.Spec.MaxNodeProvisionTime.Duration)
		assert.GreaterOrEqual(t, len(nodeClaim.OwnerReferences), 1, "the claim is owned by its slot: deleting a slot takes its claims")
		assert.Zero(t, nodeClaim.Status.Attempt, "the attempt is seeded by the claim controller, not baked in at creation")

		slots = append(slots, nodeClaim.Spec.SlotRef.Name)
	}

	assert.ElementsMatch(t, []string{"slot-1", "slot-2"}, slots, "the free slots are claimed in a stable order")

	// The slots themselves are untouched: no field of a slot spec carries an intent in v2alpha1,
	// so there is nothing for the handler to write.
	for _, name := range []string{"slot-1", "slot-2", "slot-3"} {
		assert.Equal(t, infrav2alpha1.NodePhaseOff, slotsOf(t, server, name)[name].Status.Phase,
			"the report of a slot is not a provider's to write")
	}
}

// TestNodeGroupIncreaseSizeClaimsOnlyFreeSlots: a slot with a live claim is not free, whatever
// its observation says, and the max-size check counts the live claims, so a burst of IncreaseSize
// calls in one autoscaler loop stops at the ceiling.
func TestNodeGroupIncreaseSizeClaimsOnlyFreeSlots(t *testing.T) {
	server := newServer(t, flatten(
		bootingSlot("slot-1"),
		runningSlot(t, "slot-2"),
		idleSlot("slot-3"),
		idleSlot("slot-4"),
	)...)

	_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 2})
	require.NoError(t, err)

	claims := claimsOf(t, server)
	require.Len(t, claims, 4, "two claims existed, two more were created")

	newSlots := make([]string, 0, 2)
	for _, nodeClaim := range claims {
		if nodeClaim.Status.Phase == "" {
			newSlots = append(newSlots, nodeClaim.Spec.SlotRef.Name)
		}
	}

	assert.ElementsMatch(t, []string{"slot-3", "slot-4"}, newSlots,
		"the slots a live claim already names are not claimed a second time")

	landCreatedClaims(t, server)

	// 4 slots, 4 live claims: the pool is at its max size, and the next request is refused.
	_, err = server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.Error(t, err)

	st, ok := grpcstatus.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "max size")
}

// TestNodeGroupIncreaseSizeFailsWithoutFreeSlot keeps the provider's own refusal legible: a pool
// whose every slot carries a live claim cannot grow, and the message says so in the terms the
// operator reads (free slots of the group). A slot whose claim is only draining is not free
// either: the episode has not ended, so the same slot is not claimed twice while its machine is
// being powered off, and the max-size arithmetic does not count it (a scale-down has taken its
// unit off the number).
func TestNodeGroupIncreaseSizeFailsWithoutFreeSlot(t *testing.T) {
	server := newServer(t, flatten(
		runningSlot(t, "slot-1"),
		[]client.Object{
			nodeCR("slot-2", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
			claimCR("slot-2", infrav2alpha1.NodeClaimReady, func(c *infrav2alpha1.NodeClaim) {
				now := metav1.Now()
				c.DeletionTimestamp = &now
			}),
		},
		bootingSlot("slot-3"),
	)...)

	ctx := context.Background()

	size, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(2), size.TargetSize, "the draining claim is not counted")

	_, err = server.NodeGroupIncreaseSize(ctx, &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "free node slot")
	assert.Contains(t, err.Error(), "0 of 3 slots are free")
}

// TestCreatedClaimIsLiveBeforeTheFirstStatusWrite is the ordering M1 settles: the status
// subresource strips `status` from a create, and `status.phase` carries no CRD default (a default
// on a subresource field is only stamped by the first status write), so a claim the provider has
// just created has no phase at all. In that window the provider must already see the request:
// TargetSize counts it, its slot is not free, and DecreaseTargetSize can cancel it. If an empty
// phase is read as "not live" - and IsTerminal said exactly that, before the reading moved into
// phase.IsLive - a second IncreaseSize claims the same slot, two requests buy one machine, and the
// autoscaler's arithmetic is short by the unit it is paying for.
func TestCreatedClaimIsLiveBeforeTheFirstStatusWrite(t *testing.T) {
	server := newServer(t, flatten(
		[]client.Object{groupCR(testGroup, 0)},
		idleSlot("slot-fresh"),
		idleSlot("slot-idle"),
	)...)

	ctx := context.Background()

	_, err := server.NodeGroupIncreaseSize(ctx, &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.NoError(t, err)

	created := claimsOf(t, server)
	require.Len(t, created, 1)
	require.Empty(t, created[0].Status.Phase, "the create carried no status: the phase the controller lands has not landed yet")

	size, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(1), size.TargetSize, "a request the API server accepted is capacity from its create")

	nodes, err := server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Len(t, nodes.Instances, 1, "the slot a phaseless claim names is reported, not invisible")

	// The same slot is not claimed a second time: one live claim per slot is what makes "a live
	// claim exists" a synonym for "the machine is wanted".
	_, err = server.NodeGroupIncreaseSize(ctx, &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.NoError(t, err, "the second request takes the other free slot; the first slot is not offered twice")

	claims := claimsOf(t, server)
	require.Len(t, claims, 2)
	slots := map[string]bool{}
	for _, nodeClaim := range claims {
		assert.False(t, slots[nodeClaim.Spec.SlotRef.Name], "slot %s is claimed twice", nodeClaim.Spec.SlotRef.Name)
		slots[nodeClaim.Spec.SlotRef.Name] = true
	}

	// A claim with no phase is in the cancelable phase, so the decrease that asks for the unit back
	// cancels the request the controller has not moved yet.
	_, err = server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -2})
	require.NoError(t, err, "both claims are pending, one of them by the empty phase, and both are cancelable")

	after, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(0), after.TargetSize, "the units came off on the deletes that made them")
	assert.Empty(t, claimsOf(t, server), "a phaseless claim is a pending claim, and a pending claim is deleted by a decrease")
}

// landCreatedClaims lands the claims a handler just created on their pending phase, for the tests
// that need the claim controller's first write to have happened (a move out of pending, a digest
// seeded from the phase).
//
// It is not needed for the provider's arithmetic. `status.phase` carries no CRD default - a default
// on a status field is not stamped on create, because the status subresource strips `status` from
// the create - and a claim with no phase is read as pending by phase.IsLive / phase.IsPending, which
// is the single place that reading lives. A brand-new request is capacity from the instant the API
// server accepts it: TargetSize counts it, its slot is not free, and DecreaseTargetSize can cancel
// it. TestProviderSeesACreatedClaimLive pins that ordering; a fixture that seeds pending here is
// stating the phase the controller's first pass lands, not making the claim live.
func landCreatedClaims(t *testing.T, server *HomeClusterProviderServer) {
	t.Helper()

	for _, nodeClaim := range claimsOf(t, server) {
		if nodeClaim.Status.Phase != "" {
			continue
		}

		fresh := nodeClaim.DeepCopy()
		fresh.Status.Phase = infrav2alpha1.NodeClaimPending
		require.NoError(t, server.Client.Status().Update(context.Background(), fresh))
	}
}

// TestNodeGroupIncreaseSizeRefusesNonPositiveDelta: delta is the request, and a request that
// asks for nothing is a client bug, not a write to be carried out.
func TestNodeGroupIncreaseSizeRefusesNonPositiveDelta(t *testing.T) {
	server := newServer(t, idleSlot("slot-1")...)

	for _, delta := range []int32{0, -1} {
		_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: delta})
		require.Error(t, err)

		st, ok := grpcstatus.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.InvalidArgument, st.Code())
	}

	assert.Empty(t, claimsOf(t, server), "a refused request creates nothing")
}

// TestNodeGroupDecreaseTargetSizeCancelsOnlyPendingClaims is §3 and §5 at the boundary: pending
// is the only cancelable phase, a booting claim is committed, and the number the provider ends up
// reporting is the size the request actually produced.
//
// "No silent lying" is the property under test. The response of the RPC carries no size
// (NodeGroupDecreaseTargetSizeResponse is intentionally empty), so success is the provider
// asserting "the size you asked for is in force", and cluster-autoscaler reads the group's size
// back from NodeGroupTargetSize. A request for -3 that can cancel 2 units takes 2 units away and
// reports success for exactly that; a request that can cancel nothing is answered with a code that
// says so (TestNodeGroupDecreaseTargetSizeWithNothingToCancelIsRetryable).
func TestNodeGroupDecreaseTargetSizeCancelsOnlyPendingClaims(t *testing.T) {
	server := newServer(t, flatten(
		pendingSlot("slot-pending-1"),
		pendingSlot("slot-pending-2"),
		bootingSlot("slot-booting"),
		runningSlot(t, "slot-ready"),
		idleSlot("slot-free"),
	)...)

	ctx := context.Background()

	before, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	require.Equal(t, int32(4), before.TargetSize, "two pending, one booting, one ready")

	_, err = server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -3})
	require.NoError(t, err, "the provider carries out what it can, and reports success for exactly that")

	phases := claimPhases(t, server)
	assert.Empty(t, phases["claim-slot-pending-1"], "the oldest pending claim is cancelled: the object goes, nothing is in flight")
	assert.Empty(t, phases["claim-slot-pending-2"], "and the second, until the request runs out of cancelable claims")
	assert.Equal(t, infrav2alpha1.NodeClaimBooting, phases["claim-slot-booting"],
		"a booting claim is never a cancel target: the startup Job, the lock, the cordon and the machine are in flight, and a cancel leaves the Job in a state nothing owns")
	assert.Equal(t, infrav2alpha1.NodeClaimReady, phases["claim-slot-ready"],
		"a ready claim's machine runs work: cluster-autoscaler scales that path with DeleteNodes")

	after, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(2), after.TargetSize,
		"the achievable resulting size is what the provider reports: the two cancelled units are off the number, the committed boot is not")
}

// TestNodeGroupDecreaseTargetSizeRecordsItsAttribution is the write the cancel loop owes and used to
// skip: the request is recorded on the claim - the call, the delta, the requester - and only then is
// the object deleted, exactly as NodeGroupDeleteNodes does it.
//
// The delete alone is a request nobody can see afterwards. The claim controller's finalizer ends the
// episode from spec.request (reasonForDeletion maps the call to CancelledByDecreaseTargetSize), the
// digest the slot keeps is built from the same field, and the pool's `down` column is rendered from
// that digest - so a cancel that records nothing leaves the IncreaseSize attribution of the pending
// claim as the story of the request, and the pool reports its own shrink as `up`, with the timestamp
// of the boot it cancelled. Two consumers already read this field and nothing wrote it.
//
// The finalizer is what keeps the object readable after the delete: a pending claim with no finalizer
// is simply gone, and the record this test asserts is the one the collector reads.
func TestNodeGroupDecreaseTargetSizeRecordsItsAttribution(t *testing.T) {
	cancelable := claimCR("slot-pending-1", infrav2alpha1.NodeClaimPending, func(c *infrav2alpha1.NodeClaim) {
		c.Finalizers = append(c.Finalizers, claimFinalizer)
		c.Spec.RequestedAt = metav1.NewTime(time.Now().Add(-3 * time.Hour))
	})

	// The slot of `pendingSlot`, with the claim of the same slot carrying the finalizer: a pending
	// claim has none in the model, and the finalizer is here to keep the cancelled object readable.
	server := newServer(t, nodeCR("slot-pending-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhasePending), cancelable)
	ctx := context.Background()

	_, err := server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -1})
	require.NoError(t, err)

	live := &infrav2alpha1.NodeClaim{}
	require.NoError(t, server.Client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "claim-slot-pending-1"}, live),
		"the finalizer keeps the object until the scale-down lands, which is the request's own record")

	assert.False(t, live.DeletionTimestamp.IsZero(), "the claim is cancelled: the delete that follows the write is the request")
	assert.Equal(t, infrav2alpha1.ClaimCallDecreaseTargetSize, live.Spec.Request.Call,
		"the call is recorded before the delete, so the collector can name the request that ended the episode")
	assert.Equal(t, int32(-1), live.Spec.Request.Delta, "and so is the delta, the signed size the request asked for")
	assert.Equal(t, claim.RequestedByAutoscaler, live.Spec.RequestedBy)

	// The digest the collector stamps is built from the same object, and carries both facts the pool
	// renders: the verb, and the scale-down's own instant (never the request time of the boot).
	digest := claim.Digest(live)
	assert.Equal(t, infrav2alpha1.ClaimCallDecreaseTargetSize, digest.Call)
	assert.NotNil(t, digest.ScaledDownAt, "the instant the scale-down named the claim, next to the episode's request time")
	assert.True(t, digest.ScaledDownAt.After(digest.RequestedAt.Time), "the scale-down is the newer of the two instants")

	// A claim the request already marked is left alone: the attribution is on the object, and a second
	// write on a claim that is on its way out is not a cancellation.
	before := live.DeepCopy()
	_, err = server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -1})
	require.NoError(t, err)

	after := &infrav2alpha1.NodeClaim{}
	require.NoError(t, server.Client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "claim-slot-pending-1"}, after))
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "the request is idempotent on a claim it already marked")
}

// TestNodeGroupDecreaseTargetSizeWithNothingToCancelIsRetryable is M6, the messaging half of the
// cancelation contract. A group whose every live claim is past the pending phase cannot be
// decreased by this entry point: a booting claim is committed and a ready machine runs work, so
// the request has nothing to cancel.
//
// Answering that with success is a lie the autoscaler acts on. It stops asking for the decrease,
// and it keeps paying for a unit it asked away - and for the same slot its next IncreaseSize is
// answered "no free node slot" (FailedPrecondition), a permanent-looking error that lasts as long
// as the committed boot takes: up to one boot deadline, plus the TTL of the claim that carried it.
// The state is transient by construction (the boot deadline closes a boot, DeleteNodes closes a
// ready machine), so the code is codes.Unavailable: retryable, and naming what is committed and by
// what.
func TestNodeGroupDecreaseTargetSizeWithNothingToCancelIsRetryable(t *testing.T) {
	server := newServer(t, flatten(
		bootingSlot("slot-booting"),
		runningSlot(t, "slot-ready"),
		// A terminal claim is not a live one, and there is nothing to cancel there either.
		[]client.Object{
			nodeCR("slot-failed", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
			claimCR("slot-failed", infrav2alpha1.NodeClaimFailed, nil),
		},
	)...)

	ctx := context.Background()

	_, err := server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -2})
	require.Error(t, err, "a decrease that cancelled nothing does not report success")

	st, ok := grpcstatus.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code(), "the state is transient: a boot ends at its deadline, a ready machine ends at DeleteNodes")
	assert.Contains(t, st.Message(), "booting")
	assert.Contains(t, st.Message(), "ready")

	phases := claimPhases(t, server)
	assert.Len(t, phases, 3, "nothing was cancelled, so no claim disappeared")
	assert.Equal(t, infrav2alpha1.NodeClaimBooting, phases["claim-slot-booting"])
	assert.Equal(t, infrav2alpha1.NodeClaimReady, phases["claim-slot-ready"])

	after, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(2), after.TargetSize, "the size the provider reports is unchanged, because the request changed nothing")
}

// TestNodeGroupDecreaseTargetSizeAtZeroIsANoOpSuccess: a group with no live claim is already at
// every size the request can ask for. That is not a failure to carry out the request, it is the
// request satisfied, and the autoscaler is told so with a success it can act on.
func TestNodeGroupDecreaseTargetSizeAtZeroIsANoOpSuccess(t *testing.T) {
	server := newServer(t, flatten(
		idleSlot("slot-free-1"),
		idleSlot("slot-free-2"),
		[]client.Object{
			nodeCR("slot-done", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
			claimCR("slot-done", infrav2alpha1.NodeClaimCompleted, nil),
		},
	)...)

	ctx := context.Background()

	_, err := server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -1})
	require.NoError(t, err, "the requested size is already in force: the answer is success, not a retry")

	after, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(0), after.TargetSize)
}

// TestNodeGroupDecreaseTargetSizeRefusesBelowMinSize: the floor is judged against the target
// size the request can actually produce - the current size minus the pending claims it is able
// to cancel, capped to what exists - and a refusal cancels nothing.
func TestNodeGroupDecreaseTargetSizeRefusesBelowMinSize(t *testing.T) {
	tests := []struct {
		name    string
		objects []client.Object
		delta   int32
		refuse  bool
	}{
		{
			name: "a decrease that would take the group under its floor is refused",
			objects: flatten([]client.Object{groupCR(testGroup, 2)},
				runningSlot(t, "slot-running"), pendingSlot("slot-pending")),
			delta:  -1,
			refuse: true,
		},
		{
			name: "landing exactly on the floor is allowed",
			objects: flatten([]client.Object{groupCR(testGroup, 1)},
				runningSlot(t, "slot-running"), pendingSlot("slot-pending-1"), pendingSlot("slot-pending-2")),
			delta: -1,
		},
		{
			// The cap is the point: the request asks for -2, one of the two claims it names is
			// booting, so the achievable result is the floor, and refusing it is the arithmetic
			// bug this handler has had twice.
			name: "a request that can only reach the floor is allowed",
			objects: flatten([]client.Object{groupCR(testGroup, 1)},
				runningSlot(t, "slot-running"), pendingSlot("slot-pending"), bootingSlot("slot-booting")),
			delta: -2,
		},
		{
			name: "a group with no Group CR keeps the old, unbounded behaviour",
			objects: flatten(
				runningSlot(t, "slot-running"), pendingSlot("slot-pending")),
			delta: -1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer(t, tc.objects...)

			ctx := context.Background()

			_, err := server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: tc.delta})

			if tc.refuse {
				require.Error(t, err)

				st, ok := grpcstatus.FromError(err)
				require.True(t, ok)
				assert.Equal(t, codes.FailedPrecondition, st.Code())
				assert.Contains(t, st.Message(), "minSize")

				phases := claimPhases(t, server)
				assert.Equal(t, infrav2alpha1.NodeClaimPending, phases["claim-slot-pending"], "a refused decrease cancels nothing")

				return
			}

			require.NoError(t, err)
		})
	}
}

// TestNodeGroupDecreaseTargetSizeRejectsNonNegativeDelta: the entry point is a decrease, and a
// delta that is not one is a client bug.
func TestNodeGroupDecreaseTargetSizeRejectsNonNegativeDelta(t *testing.T) {
	server := newServer(t, flatten(pendingSlot("slot-pending"))...)

	for _, delta := range []int32{0, 1} {
		_, err := server.NodeGroupDecreaseTargetSize(context.Background(), &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: delta})
		require.Error(t, err)

		st, ok := grpcstatus.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.InvalidArgument, st.Code())
	}
}

// claimPhases is the pool's inventory as a map from claim name to phase: the shape after a
// request, read back from the API. A claim that is gone has no entry.
func claimPhases(t *testing.T, server *HomeClusterProviderServer) map[string]infrav2alpha1.NodeClaimPhase {
	t.Helper()

	got := make(map[string]infrav2alpha1.NodeClaimPhase)

	for _, nodeClaim := range claimsOf(t, server) {
		got[nodeClaim.Name] = nodeClaim.Status.Phase
	}

	return got
}

// TestNodeGroupDeleteNodesRecordsTheRequestAndCancelsTheClaim is DeleteNodes as the model
// defines it: the request is written onto the claim that owns the machine (call, nodes, caller),
// and the claim is cancelled. The machine is not the provider's to evict - cluster-autoscaler
// drains before it calls - and no field of a slot is written.
func TestNodeGroupDeleteNodesRecordsTheRequestAndCancelsTheClaim(t *testing.T) {
	server := newServer(t, flatten(
		runningSlot(t, "slot-ready"),
		bootingSlot("slot-booting"),
		pendingSlot("slot-pending"),
		idleSlot("slot-free"),
	)...)

	ctx := context.Background()

	_, err := server.NodeGroupDeleteNodes(ctx, &pb.NodeGroupDeleteNodesRequest{
		Id:    testGroup,
		Nodes: externalNodes("slot-ready", "slot-booting", "slot-pending", "slot-free", "node-the-provider-has-no-slot-for"),
	})
	require.NoError(t, err)

	phases := claimPhases(t, server)
	assert.Equal(t, infrav2alpha1.NodeClaimReady, phases["claim-slot-ready"],
		"a ready claim's cancel runs through its finalizer: the shutdown is asked for, and the claim ends when the machine is observed off")
	assert.Equal(t, infrav2alpha1.NodeClaimBooting, phases["claim-slot-booting"],
		"a booting claim is cancelled the same way: the boot is reversed, never interrupted")
	assert.NotContains(t, phases, "claim-slot-pending",
		"a pending claim has nothing in flight, so cancelling it is deleting it, and its unit comes off the target size on that write")

	recorded := claimByName(t, server, "claim-slot-ready")
	assert.Equal(t, infrav2alpha1.ClaimCallDeleteNodes, recorded.Spec.Request.Call, "the request that ended this claim is on the claim")
	assert.Equal(t, []string{"slot-ready", "slot-booting", "slot-pending", "slot-free", "node-the-provider-has-no-slot-for"}, recorded.Spec.Request.Nodes)
	assert.Equal(t, claim.RequestedByAutoscaler, recorded.Spec.RequestedBy)
	assert.False(t, recorded.DeletionTimestamp.IsZero(), "the claim is marked: its finalizer is the record that a scale-down is running")

	booting := claimByName(t, server, "claim-slot-booting")
	assert.Equal(t, infrav2alpha1.ClaimCallDeleteNodes, booting.Spec.Request.Call)
	assert.False(t, booting.DeletionTimestamp.IsZero())

	// The number the autoscaler reads back falls on the write that asked for it: all three live
	// claims were named, the free slot had nothing to cancel.
	after, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(0), after.TargetSize, "a marked claim is not counted, and a deleted claim is gone")
}

// TestNodeGroupDeleteNodesIsIdempotent: a claim the request already marked is the state the
// request asked for, so the handler does not write over an object that is on its way out.
func TestNodeGroupDeleteNodesIsIdempotent(t *testing.T) {
	server := newServer(t, flatten(runningSlot(t, "slot-ready"))...)

	ctx := context.Background()

	for range 3 {
		_, err := server.NodeGroupDeleteNodes(ctx, &pb.NodeGroupDeleteNodesRequest{
			Id:    testGroup,
			Nodes: externalNodes("slot-ready"),
		})
		require.NoError(t, err)
	}

	recorded := claimByName(t, server, "claim-slot-ready")
	assert.Equal(t, infrav2alpha1.ClaimCallDeleteNodes, recorded.Spec.Request.Call)
	assert.False(t, recorded.DeletionTimestamp.IsZero())
	assert.Len(t, claimsOf(t, server), 1, "no second claim, no extra write")
}

// TestNodeGroupDeleteNodesRefusesBelowMinSize: the provider must not power machines off below the
// floor of the group. A request that deletes nothing takes no capacity away, so it is a no-op
// success even when the group is at or below its floor: a FailedPrecondition there is a refusal
// the autoscaler retries forever for an operation that changes nothing.
func TestNodeGroupDeleteNodesRefusesBelowMinSize(t *testing.T) {
	tests := []struct {
		name    string
		objects []client.Object
		nodes   []string
		refuse  bool
	}{
		{
			name:    "deleting the last node of a minSize 1 group is refused",
			objects: flatten([]client.Object{groupCR(testGroup, 1)}, runningSlot(t, "slot-1")),
			nodes:   []string{"slot-1"},
			refuse:  true,
		},
		{
			name:    "deleting both nodes of a minSize 1 group is refused",
			objects: flatten([]client.Object{groupCR(testGroup, 1)}, runningSlot(t, "slot-1"), runningSlot(t, "slot-2")),
			nodes:   []string{"slot-1", "slot-2"},
			refuse:  true,
		},
		{
			name:    "deleting one of two nodes of a minSize 1 group is allowed",
			objects: flatten([]client.Object{groupCR(testGroup, 1)}, runningSlot(t, "slot-1"), runningSlot(t, "slot-2")),
			nodes:   []string{"slot-2"},
		},
		{
			// The group is below its floor while a shutdown from an earlier request is still
			// draining, and this request names a slot that has nothing live to cancel.
			name: "a request that deletes nothing is allowed while the group is below its floor",
			objects: flatten([]client.Object{groupCR(testGroup, 2)},
				[]client.Object{
					nodeCR("slot-powering-off", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
					claimCR("slot-powering-off", infrav2alpha1.NodeClaimReady, func(c *infrav2alpha1.NodeClaim) {
						now := metav1.Now()
						c.DeletionTimestamp = &now
					}),
				},
				idleSlot("slot-shut")),
			nodes:  []string{"slot-shut"},
			refuse: false,
		},
		{
			name:    "a group without a Group CR keeps the old, unbounded behaviour",
			objects: flatten(runningSlot(t, "slot-1")),
			nodes:   []string{"slot-1"},
		},
		{
			name: "slots that are already shut down do not count against the floor",
			objects: flatten([]client.Object{groupCR(testGroup, 1)},
				runningSlot(t, "slot-1"),
				idleSlot("slot-2")),
			nodes: []string{"slot-2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer(t, tc.objects...)

			_, err := server.NodeGroupDeleteNodes(context.Background(), &pb.NodeGroupDeleteNodesRequest{
				Id:    testGroup,
				Nodes: externalNodes(tc.nodes...),
			})

			if tc.refuse {
				require.Error(t, err)

				st, ok := grpcstatus.FromError(err)
				require.True(t, ok)
				assert.Equal(t, codes.FailedPrecondition, st.Code())
				assert.Contains(t, st.Message(), "minSize")

				for _, name := range tc.nodes {
					claimOfSlot := claimByName(t, server, "claim-"+name)
					assert.True(t, claimOfSlot.DeletionTimestamp.IsZero(),
						"a refused delete marks nothing: %s keeps its live unmarked claim", name)
					assert.Equal(t, infrav2alpha1.ClaimCallIncreaseSize, claimOfSlot.Spec.Request.Call,
						"a refused delete records nothing on the claim either")
				}

				return
			}

			require.NoError(t, err)
		})
	}
}

// TestNodeGroupDeleteNodesNamesTheKubernetesNodeName: a DeleteNodes request carries Kubernetes
// node names, and the slot is found by its name (the identity the providerID homelab://<slot> is
// built from) or by spec.kubernetesNodeName, which can differ.
func TestNodeGroupDeleteNodesNamesTheKubernetesNodeName(t *testing.T) {
	slot := nodeCR("slot-cr-name", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	slot.Spec.KubernetesNodeName = "worker-9"

	nodeClaim := claimCR("slot-cr-name", infrav2alpha1.NodeClaimReady, nil)

	server := newServer(t, slot, nodeClaim)

	_, err := server.NodeGroupDeleteNodes(context.Background(), &pb.NodeGroupDeleteNodesRequest{
		Id:    testGroup,
		Nodes: externalNodes("worker-9"),
	})
	require.NoError(t, err)

	recorded := claimByName(t, server, "claim-slot-cr-name")
	assert.Equal(t, infrav2alpha1.ClaimCallDeleteNodes, recorded.Spec.Request.Call)
	assert.Equal(t, []string{"worker-9"}, recorded.Spec.Request.Nodes)
	assert.False(t, recorded.DeletionTimestamp.IsZero(), "the slot named by its Kubernetes node name is the slot whose claim is cancelled")
}

// claimByName reads one claim back from the provider's inventory by name. A claim marked for
// deletion is read like any other: the finalizer is what keeps it readable, and that is the
// record a scale-down leaves behind.
func claimByName(t *testing.T, server *HomeClusterProviderServer, name string) *infrav2alpha1.NodeClaim {
	t.Helper()

	nodeClaim := &infrav2alpha1.NodeClaim{}
	require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: testNamespace}, nodeClaim),
		"no claim with that name in the provider's inventory")

	return nodeClaim
}

// slotSpecOf is a slot's whole spec: the field set an RPC must never touch, because in v2alpha1
// a slot spec is Git-owned configuration and carries no runtime intent.
func slotSpecOf(t *testing.T, server *HomeClusterProviderServer, names ...string) map[string]infrav2alpha1.NodeSpec {
	t.Helper()

	got := make(map[string]infrav2alpha1.NodeSpec, len(names))
	for _, name := range names {
		got[name] = *slotsOf(t, server, name)[name].Spec.DeepCopy()
	}

	return got
}

// TestNoRPCWritesASlotSpec is the law of §2 asserted on all four entry points at once: the
// provider expresses intent by creating, recording and cancelling claims, and never by writing a
// field of a slot. That is the whole reason GitOps stopped reverting scale-ups - an apply can no
// longer contradict a request, because no request lives in an applied field.
func TestNoRPCWritesASlotSpec(t *testing.T) {
	names := []string{"slot-pending", "slot-booting", "slot-ready", "slot-free"}

	seed := func() *HomeClusterProviderServer {
		return newServer(t, flatten(
			[]client.Object{groupCR(testGroup, 0)},
			pendingSlot("slot-pending"),
			bootingSlot("slot-booting"),
			runningSlot(t, "slot-ready"),
			idleSlot("slot-free"),
		)...)
	}

	rpcs := map[string]func(ctx context.Context, server *HomeClusterProviderServer) error{
		"NodeGroupTargetSize": func(ctx context.Context, server *HomeClusterProviderServer) error {
			_, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})

			return err
		},
		"NodeGroupIncreaseSize": func(ctx context.Context, server *HomeClusterProviderServer) error {
			_, err := server.NodeGroupIncreaseSize(ctx, &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})

			return err
		},
		"NodeGroupDecreaseTargetSize": func(ctx context.Context, server *HomeClusterProviderServer) error {
			_, err := server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -1})

			return err
		},
		"NodeGroupDeleteNodes": func(ctx context.Context, server *HomeClusterProviderServer) error {
			_, err := server.NodeGroupDeleteNodes(ctx, &pb.NodeGroupDeleteNodesRequest{Id: testGroup, Nodes: externalNames(names...)})

			return err
		},
		"NodeGroupNodes": func(ctx context.Context, server *HomeClusterProviderServer) error {
			_, err := server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})

			return err
		},
		"NodeGroupTemplateNodeInfo": func(ctx context.Context, server *HomeClusterProviderServer) error {
			_, err := server.NodeGroupTemplateNodeInfo(ctx, &pb.NodeGroupTemplateNodeInfoRequest{Id: testGroup})

			return err
		},
		"NodeGroups": func(ctx context.Context, server *HomeClusterProviderServer) error {
			_, err := server.NodeGroups(ctx, &pb.NodeGroupsRequest{})

			return err
		},
		"NodeGroupForNode": func(ctx context.Context, server *HomeClusterProviderServer) error {
			_, err := server.NodeGroupForNode(ctx, &pb.NodeGroupForNodeRequest{Node: &pb.ExternalGrpcNode{Name: "slot-ready"}})

			return err
		},
	}

	for name, run := range rpcs {
		t.Run(name, func(t *testing.T) {
			server := seed()
			ctx := context.Background()

			before := slotSpecOf(t, server, names...)

			require.NoError(t, run(ctx, server))

			assert.Equal(t, before, slotSpecOf(t, server, names...),
				"%s wrote a field of a slot spec: the spec of a slot is configuration, and intent lives in the claims", name)
		})
	}
}

// externalNames is the request's node list, spelled from slot names.
func externalNames(names ...string) []*pb.ExternalGrpcNode {
	return externalNodes(names...)
}

// TestEffectiveMinSize keeps the clamp of a floor the provider cannot fill.
func TestEffectiveMinSize(t *testing.T) {
	tests := []struct {
		name        string
		group       *infrav2alpha1.Group
		maxSize     int32
		want        int32
		wantClamped bool
	}{
		{"nil group", nil, 3, 0, false},
		{"zero is the default", groupCR("g", 0), 3, 0, false},
		{"negative is treated as zero", groupCR("g", -1), 3, 0, false},
		{"floor below the max size is reported as is", groupCR("g", 2), 3, 2, false},
		{"floor equal to the max size is reported as is", groupCR("g", 3), 3, 3, false},
		{"floor above the max size is clamped", groupCR("g", 4), 3, 3, true},
		{"floor with no slots is clamped to zero", groupCR("g", 1), 0, 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			minSize, clamped := effectiveMinSize(tc.group, tc.maxSize)
			assert.Equal(t, tc.want, minSize)
			assert.Equal(t, tc.wantClamped, clamped)
		})
	}
}

// TestGetOptionsReportsTheGroupBudget: cluster-autoscaler is told the group's
// spec.maxNodeProvisionTime as its own scale-up termination deadline, and that number floors a
// claim's deadline. The two have to stay the same number, or the provider declares a claim dead
// while the autoscaler is still waiting for it - and the autoscaler's answer to a group that
// stopped growing is another IncreaseSize.
func TestGetOptionsReportsTheGroupBudget(t *testing.T) {
	server := newServer(t, flatten(
		[]client.Object{groupWithMaxProvision(testGroup, 0, 15*time.Minute)},
		idleSlot("slot-1"),
	)...)

	resp, err := server.NodeGroupGetOptions(context.Background(), &pb.NodeGroupAutoscalingOptionsRequest{Id: testGroup})
	require.NoError(t, err)
	require.NotNil(t, resp.NodeGroupAutoscalingOptions)
	require.NotNil(t, resp.NodeGroupAutoscalingOptions.MaxNodeProvisionDuration)

	told := resp.NodeGroupAutoscalingOptions.MaxNodeProvisionDuration.AsDuration()
	assert.Equal(t, 15*time.Minute, told, "the number the autoscaler is told is the group's spec.maxNodeProvisionTime")

	requestedAt := time.Now()
	deadline := claim.DeadlineAt(requestedAt, &metav1.Duration{Duration: told})
	assert.Equal(t, told, deadline.Sub(requestedAt),
		"the claim's deadline is floored by that same number, so a claim never dies before the autoscaler gives up on it")
	assert.Greater(t, told, claim.AttemptBudget(), "the group's floor is above the operator's budget in this fixture, which is the case the floor exists for")
}
