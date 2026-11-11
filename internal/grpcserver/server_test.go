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
	"slices"
	"testing"

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
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

const testGroup = "remote-workers"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	return scheme
}

// nodeCR builds a Node CR (a slot) of the given group and power state.
func nodeCR(name string, desired infrav2alpha1.PowerState, status infrav2alpha1.PowerState, progress infrav2alpha1.NodePhase) *infrav2alpha1.Node {
	return &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"group": testGroup},
		},
		Spec: infrav2alpha1.NodeSpec{
			DesiredPowerState:  desired,
			KubernetesNodeName: name,
		},
		Status: infrav2alpha1.NodeStatus{
			PowerState: status,
			Phase:      progress,
		},
	}
}

// k8sNode builds a Kubernetes node, optionally with a providerID set by a
// cloud-controller-manager or by the operator.
func k8sNode(name, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

// slotWithoutKubernetesNodeName builds a Node CR whose spec.kubernetesNodeName is empty.
func slotWithoutKubernetesNodeName(name string, desired infrav2alpha1.PowerState) *infrav2alpha1.Node {
	node := nodeCR(name, desired, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
	node.Spec.KubernetesNodeName = ""

	return node
}

func newServer(t *testing.T, objects ...client.Object) *HomeClusterProviderServer {
	scheme := testScheme(t)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&infrav2alpha1.Node{}).
		Build()

	return NewHomeClusterProviderServer(k8sClient, scheme)
}

func TestNodeGroupNodes(t *testing.T) {
	tests := []struct {
		name string
		// objects to seed the fake client with
		objects []client.Object
		// expected instance id -> instance state
		expected map[string]pb.InstanceStatus_InstanceState
	}{
		{
			// A slot with no machine and no node object is spare capacity, not an instance.
			// Reporting it is what made the autoscaler provision extra machines.
			name: "off slot without a kubernetes node is skipped",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{},
		},
		{
			// The machine is off but the kubelet node object is still there (NotReady), so the
			// slot is an instance that is going away. Cluster-autoscaler matches the node to it
			// by spec.providerID, so it must not look like an unregistered node.
			name: "off slot with a kubernetes node is deleting",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
				k8sNode("slot-1", "homelab://slot-1"),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceDeleting,
			},
		},
		{
			name: "off slot with a kubernetes node without providerID falls back to the node CR identity",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
				k8sNode("slot-1", ""),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceDeleting,
			},
		},
		{
			name: "off slot whose kubernetes node name is empty is skipped",
			objects: []client.Object{
				slotWithoutKubernetesNodeName("slot-1", infrav2alpha1.PowerStateOff),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{},
		},
		{
			name: "slot that is on without a kubernetes node is provisioning",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceCreating,
			},
		},
		{
			name: "slot that is on with a kubernetes node is running",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
				k8sNode("slot-1", "homelab://slot-1"),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceRunning,
			},
		},
		{
			name: "empty providerID falls back to the provider id the operator writes",
			objects: []client.Object{
				nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
				k8sNode("slot-1", ""),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceRunning,
			},
		},
		{
			name: "slot without a kubernetes node name is provisioning and keeps an id",
			objects: []client.Object{
				slotWithoutKubernetesNodeName("slot-1", infrav2alpha1.PowerStateOn),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceCreating,
			},
		},
		{
			name: "mixed pool reports every slot with a machine and skips spare capacity",
			objects: []client.Object{
				// fresh, never provisioned slot of a 15 slot pool, no machine and no node object
				nodeCR("slot-seeded", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
				// scaled down slot, the machine is off and the node object is left over
				nodeCR("slot-off", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
				k8sNode("slot-off", "homelab://slot-off"),
				// slot requested by IncreaseSize, machine not joined yet
				nodeCR("slot-new", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
				// running slot without a cloud-controller-manager
				nodeCR("omni-remote-1", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
				k8sNode("omni-remote-1", ""),
			},
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

// TestNodeGroupNodesStableIdentityAcrossProvisioning covers the identity of a slot while it
// crosses the provisioning boundary. Cluster-autoscaler keys instances by id, and the
// operator stamps the Kubernetes node with providerID "homelab://<Node CR name>" when the
// machine joins. The id reported before the node exists must be the same, otherwise the
// in-flight instance is invisible and a single pending pod produces two scale ups.
func TestNodeGroupNodesStableIdentityAcrossProvisioning(t *testing.T) {
	server := newServer(t, nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting))

	ctx := context.Background()

	resp, err := server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	require.Len(t, resp.Instances, 1)
	provisioningID := resp.Instances[0].Id
	assert.Equal(t, pb.InstanceStatus_instanceCreating, resp.Instances[0].Status.InstanceState)

	// The machine joins and the operator patches the providerID onto the Kubernetes node.
	require.NoError(t, server.Client.Create(ctx, k8sNode("slot-1", "homelab://slot-1")))

	resp, err = server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	require.Len(t, resp.Instances, 1)
	assert.Equal(t, pb.InstanceStatus_instanceRunning, resp.Instances[0].Status.InstanceState)
	assert.Equal(t, provisioningID, resp.Instances[0].Id, "instance id must not change once the node joins")
}

// TestNodeGroupNodesIgnoresEmptyKubernetesNodeName covers a slot that has no
// spec.kubernetesNodeName yet: the Get must be skipped so the call keeps returning a
// stable provisioning instance instead of a non-NotFound error every time.
func TestNodeGroupNodesIgnoresEmptyKubernetesNodeName(t *testing.T) {
	server := newServer(t, slotWithoutKubernetesNodeName("slot-1", infrav2alpha1.PowerStateOn))

	for range 3 {
		resp, err := server.NodeGroupNodes(context.Background(), &pb.NodeGroupNodesRequest{Id: testGroup})
		require.NoError(t, err)
		require.Len(t, resp.Instances, 1)
		assert.Equal(t, "homelab://slot-1", resp.Instances[0].Id)
		assert.Equal(t, pb.InstanceStatus_instanceCreating, resp.Instances[0].Status.InstanceState)
	}
}

// TestNodeGroupNodesInstancePerMachine covers the contract the autoscaler relies on: one
// instance per slot that has a machine, and the state of that instance follows the desired power
// state. Cluster-autoscaler keys Kubernetes nodes to instances by spec.providerID, so the row
// that matters is the shut down slot - its node object is still there and must map to an
// instance, marked deleting, while a never provisioned slot must stay invisible.
func TestNodeGroupNodesInstancePerMachine(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-running", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		k8sNode("slot-running", "homelab://slot-running"),
		nodeCR("slot-creating", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
		nodeCR("slot-deleting", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		k8sNode("slot-deleting", "homelab://slot-deleting"),
		nodeCR("slot-spare", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

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
	}, got, "a slot with no machine must not be reported")
}

// TestNodeGroupNodesStableIdentityAcrossShutdown covers the identity of a slot while it is
// scaled down. The instance id is the providerID cluster-autoscaler matches the Kubernetes node
// against, so the id must not change when the slot moves from running to deleting.
func TestNodeGroupNodesStableIdentityAcrossShutdown(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		k8sNode("slot-1", "homelab://slot-1"),
	)

	ctx := context.Background()

	resp, err := server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	require.Len(t, resp.Instances, 1)
	assert.Equal(t, pb.InstanceStatus_instanceRunning, resp.Instances[0].Status.InstanceState)

	// NodeGroupDeleteNodes sets the desired state to off; the machine powers off and the node
	// object stays behind, NotReady.
	slot := &infrav2alpha1.Node{}
	require.NoError(t, server.Client.Get(ctx, client.ObjectKey{Name: "slot-1", Namespace: "default"}, slot))
	slot.Spec.DesiredPowerState = infrav2alpha1.PowerStateOff
	require.NoError(t, server.Client.Update(ctx, slot))

	resp, err = server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	require.Len(t, resp.Instances, 1)
	assert.Equal(t, pb.InstanceStatus_instanceDeleting, resp.Instances[0].Status.InstanceState)
	assert.Equal(t, "homelab://slot-1", resp.Instances[0].Id, "instance id must not change when the slot is scaled down")
}

func TestNodeGroupTargetSizeIgnoresSeededSlots(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		nodeCR("slot-2", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		nodeCR("slot-3", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		nodeCR("slot-4", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
		nodeCR("slot-5", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
	)

	resp, err := server.NodeGroupTargetSize(context.Background(), &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	// only the ready and the starting up slot count towards the target size
	assert.Equal(t, int32(2), resp.TargetSize)
}

func TestNodeGroupIncreaseSizePicksSeededOffSlot(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		nodeCR("slot-2", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
	)

	_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.NoError(t, err)

	updated := &infrav2alpha1.Node{}
	require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: "slot-1", Namespace: "default"}, updated))
	assert.Equal(t, infrav2alpha1.PowerStateOn, updated.Spec.DesiredPowerState)

	// the shutting down slot must not be touched
	stillOff := &infrav2alpha1.Node{}
	require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: "slot-2", Namespace: "default"}, stillOff))
	assert.Equal(t, infrav2alpha1.PowerStateOff, stillOff.Spec.DesiredPowerState)
}

func TestNodeGroupIncreaseSizeFailsWithoutFreeSlot(t *testing.T) {
	// Two slots, so the max size check does not fire: the only spare slot is mid-shutdown
	// (progress shuttingdown) and cannot be started, and running slots are not free.
	server := newServer(t,
		nodeCRInGroup(testGroup, "slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		nodeCRInGroup(testGroup, "slot-2", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
	)

	_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no powered off nodes found")
}

// testNamespace is the namespace the gRPC server resolves Group CRs in
// (internal/config.NamespaceConfig default).
const testNamespace = "homelab-autoscaler-system"

// groupCR builds a Group CR with a per-group minimum size.
func groupCR(name string, minSize int32) *infrav2alpha1.Group {
	return &infrav2alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       infrav2alpha1.GroupSpec{MinSize: minSize},
	}
}

// nodeCRInGroup builds a Node CR (a slot) of an arbitrary group.
func nodeCRInGroup(group, name string, desired, status infrav2alpha1.PowerState, progress infrav2alpha1.NodePhase) *infrav2alpha1.Node {
	node := nodeCR(name, desired, status, progress)
	node.Labels["group"] = group

	return node
}

// TestNodeGroupsReportsGroupMinSize: the provider used to hardcode MinSize 0, so a pool
// could never express "keep at least one node" and cluster-autoscaler was free to
// reclaim the always-on slot (the gitops repo worked around that with a keepalive pod).
// The Group CR is now the source of the floor, and a floor above the number of slots the
// provider has is clamped to it: maxSize is derived from the Node CR count, so the CRD
// cannot assert minSize <= maxSize.
func TestNodeGroupsReportsGroupMinSize(t *testing.T) {
	server := newServer(t,
		groupCR(testGroup, 1),
		nodeCRInGroup(testGroup, "slot-running", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		nodeCRInGroup(testGroup, "slot-spare", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		nodeCRInGroup(testGroup, "slot-off", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),

		groupCR("group-no-min", 0),
		nodeCRInGroup("group-no-min", "spare-1", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),

		groupCR("group-over-min", 5),
		nodeCRInGroup("group-over-min", "only-1", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		nodeCRInGroup("group-over-min", "only-2", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),

		groupCR("group-empty", 1),
	)

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
	server := newServer(t,
		groupCR(testGroup, 2),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   "slot-off",
			Labels: map[string]string{"infra.homecluster.dev/group": testGroup},
		}},
		nodeCRInGroup(testGroup, "slot-off", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		nodeCRInGroup(testGroup, "slot-spare", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	resp, err := server.NodeGroupForNode(context.Background(), &pb.NodeGroupForNodeRequest{
		Node: &pb.ExternalGrpcNode{Name: "slot-off"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.NodeGroup)

	assert.Equal(t, testGroup, resp.NodeGroup.Id)
	assert.Equal(t, int32(2), resp.NodeGroup.MinSize)
	assert.Equal(t, int32(2), resp.NodeGroup.MaxSize)
}

// slotInSystemNamespace builds a slot of the test group in the namespace the server
// resolves Node CRs in, which is what NodeGroupDeleteNodes looks the slot up by.
func slotInSystemNamespace(name string, desired, status infrav2alpha1.PowerState, progress infrav2alpha1.NodePhase) *infrav2alpha1.Node {
	node := nodeCRInGroup(testGroup, name, desired, status, progress)
	node.Namespace = testNamespace

	return node
}

func externalNodes(names ...string) []*pb.ExternalGrpcNode {
	nodes := make([]*pb.ExternalGrpcNode, 0, len(names))
	for _, name := range names {
		nodes = append(nodes, &pb.ExternalGrpcNode{Name: name})
	}

	return nodes
}

func desiredPowerStates(t *testing.T, server *HomeClusterProviderServer, names ...string) map[string]infrav2alpha1.PowerState {
	t.Helper()

	got := make(map[string]infrav2alpha1.PowerState, len(names))
	for _, name := range names {
		node := &infrav2alpha1.Node{}
		require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: testNamespace}, node))
		got[name] = node.Spec.DesiredPowerState
	}

	return got
}

// TestNodeGroupDeleteNodesRefusesBelowMinSize: the provider must not power machines off
// below the floor of the group. The refusal is explicit and logged, not a silent accept
// that leaves the always-on slot destroyed. A request that deletes nothing takes no
// capacity away, so it is a no-op success even when the group is already at or below its
// floor.
func TestNodeGroupDeleteNodesRefusesBelowMinSize(t *testing.T) {
	running := func(name string) *infrav2alpha1.Node {
		return slotInSystemNamespace(name, infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	}

	tests := []struct {
		name    string
		objects []client.Object
		nodes   []string
		refuse  bool
	}{
		{
			name:    "deleting the last node of a minSize 1 group is refused",
			objects: []client.Object{groupCR(testGroup, 1), running("slot-1")},
			nodes:   []string{"slot-1"},
			refuse:  true,
		},
		{
			name:    "deleting both nodes of a minSize 1 group is refused",
			objects: []client.Object{groupCR(testGroup, 1), running("slot-1"), running("slot-2")},
			nodes:   []string{"slot-1", "slot-2"},
			refuse:  true,
		},
		{
			name:    "deleting one of two nodes of a minSize 1 group is allowed",
			objects: []client.Object{groupCR(testGroup, 1), running("slot-1"), running("slot-2")},
			nodes:   []string{"slot-2"},
			refuse:  false,
		},
		{
			// The group is below its floor while a shutdown from an earlier request is still
			// in flight, and this request deletes a slot that is already shut down, so it
			// takes no capacity away. Refusing it asks the autoscaler to do something the
			// provider cannot act on and locks the delete loop on a no-op.
			name: "a request that deletes nothing is allowed while the group is below its floor",
			objects: []client.Object{
				groupCR(testGroup, 2),
				slotInSystemNamespace("slot-powering-off", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
				slotInSystemNamespace("slot-shut", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
			},
			nodes:  []string{"slot-shut"},
			refuse: false,
		},
		{
			name:    "a group without a Group CR keeps the old, unbounded behaviour",
			objects: []client.Object{running("slot-1")},
			nodes:   []string{"slot-1"},
			refuse:  false,
		},
		{
			name: "slots that are already shut down do not count against the floor",
			objects: []client.Object{
				groupCR(testGroup, 1),
				running("slot-1"),
				slotInSystemNamespace("slot-2", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
			},
			nodes:  []string{"slot-2"},
			refuse: false,
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

				// nothing was touched
				for _, name := range tc.nodes {
					assert.Equal(t, infrav2alpha1.PowerStateOn, desiredPowerStates(t, server, name)[name],
						"a refused delete must leave %s desired on", name)
				}

				return
			}

			require.NoError(t, err)
			for _, name := range tc.nodes {
				assert.Equal(t, infrav2alpha1.PowerStateOff, desiredPowerStates(t, server, name)[name])
			}
		})
	}
}

// TestNodeGroupDecreaseTargetSizeRefusesBelowMinSize mirrors the same floor on the
// decrease path: cancelling slots that are still starting must not take the group below
// its minimum size. The floor is judged against the target size the request can actually
// produce (targetSizeOf minus the slots it is able to cancel), and a request that cannot
// take any capacity away is a no-op success, because cluster-autoscaler retries a refusal
// forever.
func TestNodeGroupDecreaseTargetSizeRefusesBelowMinSize(t *testing.T) {
	startingUp := func(name string) *infrav2alpha1.Node {
		return slotInSystemNamespace(name, infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
	}
	running := func(name string) *infrav2alpha1.Node {
		return slotInSystemNamespace(name, infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	}
	// midShutdown is a slot being powered off: desired off, the machine still on, progress
	// not yet a shutdown, so it still counts towards the target size.
	midShutdown := func(name string) *infrav2alpha1.Node {
		return slotInSystemNamespace(name, infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	}

	tests := []struct {
		name     string
		objects  []client.Object
		delta    int32
		refuse   bool
		wantSize int32    // what NodeGroupTargetSize reports after the request
		wantShed []string // slots the request must cancel: spec off, progress left to the FSM
		wantKeep []string // slots this request must not cancel
	}{
		{
			// The reported bug: the floor has to be judged against the target size that
			// results from the request. Cancelling the starting slot takes the target size
			// from 2 to 1, which is the floor, not below it, and refusing that leaves
			// cluster-autoscaler retrying the refusal forever with a target size it can
			// never correct (the pre-fix arithmetic counted the running slots only, so it
			// compared 1 + (-1) = 0 against the floor).
			name:     "minSize 1, one running + one starting: delta -1 lands on the floor and is carried out",
			objects:  []client.Object{groupCR(testGroup, 1), running("slot-1"), startingUp("slot-2")},
			delta:    -1,
			refuse:   false,
			wantSize: 1,
			wantShed: []string{"slot-2"},
			wantKeep: []string{"slot-1"},
		},
		{
			// Nothing to cancel means no capacity goes away, so there is nothing to refuse:
			// a FailedPrecondition here is retried forever.
			name:     "minSize 1 with one running node and nothing to cancel is a no-op success",
			objects:  []client.Object{groupCR(testGroup, 1), running("slot-1")},
			delta:    -1,
			refuse:   false,
			wantSize: 1,
			wantKeep: []string{"slot-1"},
		},
		{
			// The only capacity this entry point owns is the starting slot, so a delta that is
			// bigger than the slots it can cancel cannot cross the floor either: cancelling the
			// one starting slot takes the target size from 3 to 2, which is the floor.
			name:     "minSize 2, two running + one starting: delta -2 cancels the one starting slot and lands on the floor",
			objects:  []client.Object{groupCR(testGroup, 2), running("slot-1"), running("slot-2"), startingUp("slot-3")},
			delta:    -2,
			refuse:   false,
			wantSize: 2,
			wantShed: []string{"slot-3"},
			wantKeep: []string{"slot-1", "slot-2"},
		},
		{
			// The case that genuinely crosses the floor: one running slot and one starting
			// slot, so the only cancellation this request can carry out takes the target size
			// from 2 to 1, below the floor. It is refused, nothing is cancelled.
			name:     "minSize 2, one running + one starting: delta -2 crosses the floor and is refused",
			objects:  []client.Object{groupCR(testGroup, 2), running("slot-1"), startingUp("slot-2")},
			delta:    -2,
			refuse:   true,
			wantSize: 2,
			wantKeep: []string{"slot-1", "slot-2"},
		},
		{
			name:     "minSize 2, two running + one starting: delta -1 lands on the floor",
			objects:  []client.Object{groupCR(testGroup, 2), running("slot-1"), running("slot-2"), startingUp("slot-3")},
			delta:    -1,
			refuse:   false,
			wantSize: 2,
			wantShed: []string{"slot-3"},
			wantKeep: []string{"slot-1", "slot-2"},
		},
		{
			// The floor is compared against targetSizeOf, the number NodeGroupTargetSize
			// reports, not against a count of the running slots. A slot that is mid shutdown
			// is still part of the target size, so cancelling the starting slot here lands on
			// the floor; counting only the running slots would refuse it.
			name:     "minSize 2, one running + one starting + one mid shutdown: delta -1 lands on the floor",
			objects:  []client.Object{groupCR(testGroup, 2), running("slot-1"), startingUp("slot-2"), midShutdown("slot-3")},
			delta:    -1,
			refuse:   false,
			wantSize: 2,
			wantShed: []string{"slot-2"},
			wantKeep: []string{"slot-1", "slot-3"},
		},
		{
			// The floor has to be judged against the size the request can actually produce,
			// not against the uncapped delta: the target size is 3 (one running + two starting),
			// two slots can be cancelled, so the achievable result is 1, exactly the floor. The
			// uncapped arithmetic compared 3 - 3 = 0 against the floor and refused it, the same
			// class of unrecoverable FailedPrecondition as the case above, one level down.
			name:     "minSize 1, one running + two starting: delta -3 cancels both starting slots and lands on the floor",
			objects:  []client.Object{groupCR(testGroup, 1), running("slot-1"), startingUp("slot-2"), startingUp("slot-3")},
			delta:    -3,
			refuse:   false,
			wantSize: 1,
			wantShed: []string{"slot-2", "slot-3"},
			wantKeep: []string{"slot-1"},
		},
		{
			// Cancelling one starting slot is one unit off the target size, so the delta is
			// carried out on the slots that can be cancelled and the running machines, which
			// this entry point does not own, stay up.
			name:     "minSize 1 with two running nodes and one starting cancels the starting slot",
			objects:  []client.Object{groupCR(testGroup, 1), running("slot-1"), running("slot-2"), startingUp("slot-3")},
			delta:    -1,
			refuse:   false,
			wantSize: 2,
			wantShed: []string{"slot-3"},
			wantKeep: []string{"slot-1", "slot-2"},
		},
		{
			// A delta larger than the slots that can be cancelled cancels all of them and
			// still succeeds: cluster-autoscaler reads the resulting target size back from
			// NodeGroupTargetSize, it does not track the arithmetic itself.
			name:     "no floor, delta larger than the starting slots cancels all of them",
			objects:  []client.Object{groupCR(testGroup, 0), running("slot-1"), startingUp("slot-2")},
			delta:    -2,
			refuse:   false,
			wantSize: 1,
			wantShed: []string{"slot-2"},
			wantKeep: []string{"slot-1"},
		},
		{
			name:     "no floor keeps the old behaviour",
			objects:  []client.Object{groupCR(testGroup, 0), running("slot-1"), startingUp("slot-2")},
			delta:    -1,
			refuse:   false,
			wantSize: 1,
			wantShed: []string{"slot-2"},
			wantKeep: []string{"slot-1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer(t, tc.objects...)

			_, err := server.NodeGroupDecreaseTargetSize(context.Background(), &pb.NodeGroupDecreaseTargetSizeRequest{
				Id:    testGroup,
				Delta: tc.delta,
			})

			if tc.refuse {
				require.Error(t, err)
				st, ok := grpcstatus.FromError(err)
				require.True(t, ok)
				assert.Equal(t, codes.FailedPrecondition, st.Code())
				assert.Contains(t, st.Message(), "minSize")
			} else {
				require.NoError(t, err)
			}

			all := append(append([]string{}, tc.wantShed...), tc.wantKeep...)
			for _, name := range all {
				if slices.Contains(tc.wantShed, name) {
					desired, progress := slotState(t, server, name)
					assert.Equal(t, infrav2alpha1.PowerStateOff, desired, "cancelled slot %s must be desired off", name)
					// The progress is the FSM's to write. The cancellation is the spec write,
					// and countsTowardsTargetSize excludes the desired-off + startingup shape it
					// produces, so the slot leaves the target size on that write alone - the
					// node controller's ShutdownNode, with its Job and its lock, lands the
					// progress on shuttingdown afterwards.
					assert.Equal(t, infrav2alpha1.NodePhaseBooting, progress,
						"cancelled slot %s keeps the progress the FSM wrote", name)

					continue
				}

				_, progress := slotState(t, server, name)
				assert.NotEqual(t, infrav2alpha1.NodePhaseDraining, progress, "slot %s must not be cancelled", name)
			}

			// The request lands exactly on the size it asked for, judged by the number
			// cluster-autoscaler reads back: current target size minus the cancellations the
			// request carried out.
			resp, err := server.NodeGroupTargetSize(context.Background(), &pb.NodeGroupTargetSizeRequest{Id: testGroup})
			require.NoError(t, err)
			assert.Equal(t, tc.wantSize, resp.TargetSize, "the target size that lands is the one NodeGroupTargetSize reports")
		})
	}
}

// slotState reads the desired power state and the recorded progress of a slot.
func slotState(t *testing.T, server *HomeClusterProviderServer, name string) (infrav2alpha1.PowerState, infrav2alpha1.NodePhase) {
	t.Helper()

	node := &infrav2alpha1.Node{}
	require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: testNamespace}, node))

	return node.Spec.DesiredPowerState, node.Status.Phase
}

// TestNodeGroupIncreaseSizeRefusesBeyondMaxSize: maxSize is the number of slots committed
// to the group, so a scale up that would exceed it is refused by the provider too.
func TestNodeGroupIncreaseSizeRefusesBeyondMaxSize(t *testing.T) {
	server := newServer(t,
		groupCR(testGroup, 1),
		slotInSystemNamespace("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
	)

	_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.Error(t, err)

	st, ok := grpcstatus.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "max size")
	assert.Equal(t, infrav2alpha1.PowerStateOn, desiredPowerStates(t, server, "slot-1")["slot-1"])

	// within the max size, the scale up still powers a spare slot on
	server = newServer(t,
		groupCR(testGroup, 1),
		slotInSystemNamespace("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		slotInSystemNamespace("slot-2", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	_, err = server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.NoError(t, err)
	assert.Equal(t, infrav2alpha1.PowerStateOn, desiredPowerStates(t, server, "slot-2")["slot-2"])
}

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

func TestTargetSizeOf(t *testing.T) {
	nodes := []infrav2alpha1.Node{
		*nodeCRInGroup(testGroup, "ready", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		*nodeCRInGroup(testGroup, "starting", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
		*nodeCRInGroup(testGroup, "stopping", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
		*nodeCRInGroup(testGroup, "off", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		*nodeCRInGroup(testGroup, "seeded", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	}

	assert.Equal(t, int32(2), targetSizeOf(nodes))
	assert.Equal(t, int32(0), targetSizeOf(nil))
}

// TestCountsTowardsTargetSize pins the definition of a slot the provider is committed to
// serve. The claimed rows are the burst fix: NodeGroupIncreaseSize claims a slot by writing
// spec.desiredPowerState = on, and status.phase - owned by the FSM - keeps saying
// "shutdown" until the node controller picks the slot up seconds later and the machine then
// takes minutes to boot. A target size read off progress alone reports the slot as gone for
// that whole window, so cluster-autoscaler asks for the same slot again in the same loop.
func TestCountsTowardsTargetSize(t *testing.T) {
	tests := []struct {
		name     string
		desired  infrav2alpha1.PowerState
		status   infrav2alpha1.PowerState
		progress infrav2alpha1.NodePhase
		want     bool
	}{
		{"a running slot counts", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady, true},
		{"a slot that is starting up counts", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting, true},
		// (a) the claimed slot: this is the row the pre-fix helper got wrong.
		{"a claimed slot whose FSM has not moved yet counts", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff, true},
		{"a claimed slot with no progress recorded yet counts", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, "", true},
		// (b) a slot that is genuinely being torn down stays excluded, the exclusion the
		// autoscaler needs for its headroom.
		{"a slot mid shutdown does not count", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining, false},
		{"a shut down slot does not count", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff, false},
		// A machine that is up and desired off is capacity the provider is serving: only
		// NodeGroupDeleteNodes owns it, and the autoscaler must keep seeing it until the
		// shutdown job has run.
		{"a machine still running while desired off counts", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady, true},
		// The shape a cancelled startup lands in: NodeGroupDecreaseTargetSize cleared the spec
		// and left the progress to the FSM. The request is off and the machine never came up, so
		// it is not capacity - and this is the row that keeps the target size drop immediate now
		// that the handler no longer writes progress: shuttingdown itself.
		{"a cancelled startup does not count", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := nodeCRInGroup(testGroup, "slot-1", tc.desired, tc.status, tc.progress)
			assert.Equal(t, tc.want, countsTowardsTargetSize(*node))
		})
	}
}

// TestNodeGroupTargetSizeCountsClaimedSlot is the same rule at the gRPC boundary, where
// cluster-autoscaler reads it: the two slots the operator committed are 1 running machine and
// 1 claim that has not been picked up by the FSM yet, and the target size has to be 2, not 1.
// With 1 the autoscaler concludes the group did not grow and calls IncreaseSize again.
func TestNodeGroupTargetSizeCountsClaimedSlot(t *testing.T) {
	server := newServer(t,
		slotInSystemNamespace("slot-running", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		slotInSystemNamespace("slot-claimed", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		slotInSystemNamespace("slot-stopping", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
		slotInSystemNamespace("slot-spare", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	resp, err := server.NodeGroupTargetSize(context.Background(), &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	// the running machine and the claim; the slot that is being torn down and the spare do not
	assert.Equal(t, int32(2), resp.TargetSize)
}

// TestNodeGroupIncreaseSizeRefusesWhenClaimsReachMaxSize is the burst itself (test c): one
// pending pod made cluster-autoscaler call IncreaseSize 5 times in 15 minutes, because every
// call was answered with a target size that ignored the slot it had just claimed. Once claims
// count, the group is at its max size as soon as its slots are claimed, and the repeat
// requests are refused instead of powering on machines over what the operator committed.
func TestNodeGroupIncreaseSizeRefusesWhenClaimsReachMaxSize(t *testing.T) {
	server := newServer(t,
		groupCR(testGroup, 1),
		slotInSystemNamespace("slot-running", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		slotInSystemNamespace("slot-spare", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	ctx := context.Background()

	// The first scale up is served: the group is at 1 of 2 slots.
	_, err := server.NodeGroupIncreaseSize(ctx, &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.NoError(t, err)
	assert.Equal(t, infrav2alpha1.PowerStateOn, desiredPowerStates(t, server, "slot-spare")["slot-spare"])

	// The claim is capacity now, so the group reads full at the max size. Pre-fix this
	// answered 1 - the running machine only - which is the number that made the autoscaler
	// ask for the slot it had just claimed, over and over in the same loop.
	resp, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.TargetSize, "a claimed slot is part of the target size")

	// cluster-autoscaler has not refreshed its view yet and asks again, and again: every
	// repeat is refused, and it is refused as a max size refusal, not as "no powered off
	// nodes found" reached by walking past the max size check.
	for i := 0; i < 3; i++ {
		_, err = server.NodeGroupIncreaseSize(ctx, &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
		require.Error(t, err, "a scale up that crosses the max size must be refused")

		st, ok := grpcstatus.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.FailedPrecondition, st.Code())
		assert.Contains(t, st.Message(), "max size")
	}

	// Nothing changed on the way: the group is still at 2 of 2, and the second slot stayed
	// the only claimed one.
	resp, err = server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.TargetSize)
}

// TestNodeGroupDecreaseTargetSizeClaimIsNotACancelTarget is the knock-on on the decrease path
// (test d). A claim in flight is a scale-up the provider has not started, not a startup to
// abort, and it does count towards the target size. The floor arithmetic has to land on the
// floor with the claim counted: the target size here is 3 (running + starting + claimed) and
// the only cancellable slot is the starting one, so delta -1 produces exactly the floor of 2
// and must be carried out.
func TestNodeGroupDecreaseTargetSizeClaimIsNotACancelTarget(t *testing.T) {
	server := newServer(t,
		groupCR(testGroup, 2),
		slotInSystemNamespace("slot-running", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		slotInSystemNamespace("slot-starting", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
		slotInSystemNamespace("slot-claimed", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	ctx := context.Background()

	_, err := server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -1})
	require.NoError(t, err)

	// the starting slot is the one that is cancelled: the spec write is the whole cancellation,
	// and a cancelled startup (desired off, progress still the startingup the FSM wrote) is off
	// the target size, while a claimed slot counts on spec alone
	desired, progress := slotState(t, server, "slot-starting")
	assert.Equal(t, infrav2alpha1.PowerStateOff, desired)
	assert.Equal(t, infrav2alpha1.NodePhaseBooting, progress, "the cancellation does not write status.phase")

	// the claim is untouched: cancelling it is not this entry point's, and a claim whose spec
	// was cleared would silently drop out of the target size
	desired, progress = slotState(t, server, "slot-claimed")
	assert.Equal(t, infrav2alpha1.PowerStateOn, desired, "a claim in flight is not a cancel target")
	assert.Equal(t, infrav2alpha1.NodePhaseOff, progress)

	desired, progress = slotState(t, server, "slot-running")
	assert.Equal(t, infrav2alpha1.PowerStateOn, desired)
	assert.Equal(t, infrav2alpha1.NodePhaseReady, progress)

	// the size that landed on the floor is the one NodeGroupTargetSize reports: running + claim
	resp, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.TargetSize)

	// With the claim counted, the group sits exactly on its floor and has one unit of
	// headroom, which is the truth about the pool: the next decrease has nothing left to
	// cancel, so it is a no-op success and the claim survives. A FailedPrecondition here is
	// the refusal cluster-autoscaler retries forever.
	_, err = server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -1})
	require.NoError(t, err)
	desired, _ = slotState(t, server, "slot-claimed")
	assert.Equal(t, infrav2alpha1.PowerStateOn, desired, "the claim is not a cancel target")

	// and a request that asks for more than the cancellable slots cannot cross the floor
	// either: 3 of them, of which one is cancellable, produce 2, exactly the floor.
	server = newServer(t,
		groupCR(testGroup, 2),
		slotInSystemNamespace("slot-running", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		slotInSystemNamespace("slot-starting", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
		slotInSystemNamespace("slot-claimed", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	_, err = server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -2})
	require.NoError(t, err)
	desired, progress = slotState(t, server, "slot-starting")
	assert.Equal(t, infrav2alpha1.PowerStateOff, desired)
	assert.Equal(t, infrav2alpha1.NodePhaseBooting, progress)
	desired, _ = slotState(t, server, "slot-claimed")
	assert.Equal(t, infrav2alpha1.PowerStateOn, desired)

	// A decrease that would take the pool under the floor is still refused with the claim
	// counted: the target size is 4, the two starting slots are all this entry point can
	// cancel, so delta -3 produces 2, below the floor of 3. The floor is judged on the same
	// number NodeGroupTargetSize reports, on both sides of the fix.
	server = newServer(t,
		groupCR(testGroup, 3),
		slotInSystemNamespace("slot-running", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		slotInSystemNamespace("slot-starting", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
		slotInSystemNamespace("slot-starting-2", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
		slotInSystemNamespace("slot-claimed", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	_, err = server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -3})
	require.Error(t, err)
	st, ok := grpcstatus.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "minSize")
	desired, _ = slotState(t, server, "slot-claimed")
	assert.Equal(t, infrav2alpha1.PowerStateOn, desired, "a refused decrease cancels nothing")
}

// TestNodeGroupDecreaseTargetSizeCancelsWithTheSpecNotTheProgress is the behaviour change on
// the cancel path. Cancelling a starting slot is a spec write, the shape NodeGroupDeleteNodes
// produces, and never a write of status.phase. The handler used to set
// progress: shuttingdown itself - no transition, no lifecycle Job, no coordination lock, no
// lastShutdownTime - which makes it the one writer of a transitional progress that is not a
// transition: the slot is excluded from the target size, validateStateTransition gates every
// further transition out of it, and no reconcile heals it, because the stuck-transition
// cleanup runs on the state machine's own backoff record and the reconciler builds that
// machine fresh on every pass. The only writer left is the job monitor of the startup the
// cancel collided with, and a manager restart takes that goroutine away.
//
// What the call is asked to do is unchanged: cancelling N starting slots takes exactly N units
// off the target size, on the write that asks for it, and the shutdown runs through
// ShutdownNode with its Job and its lock.
func TestNodeGroupDecreaseTargetSizeCancelsWithTheSpecNotTheProgress(t *testing.T) {
	startingUp := func(name string) *infrav2alpha1.Node {
		return slotInSystemNamespace(name, infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
	}
	running := func(name string) *infrav2alpha1.Node {
		return slotInSystemNamespace(name, infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	}

	server := newServer(t,
		groupCR(testGroup, 1),
		running("slot-1"), running("slot-2"),
		startingUp("slot-3"), startingUp("slot-4"),
	)

	ctx := context.Background()

	resp, err := server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	require.Equal(t, int32(4), resp.TargetSize, "two running + two starting")

	_, err = server.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{Id: testGroup, Delta: -2})
	require.NoError(t, err)

	for _, name := range []string{"slot-3", "slot-4"} {
		node := &infrav2alpha1.Node{}
		require.NoError(t, server.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, node))

		assert.Equal(t, infrav2alpha1.PowerStateOff, node.Spec.DesiredPowerState, "cancelled slot %s is desired off", name)
		assert.Equal(t, infrav2alpha1.NodePhaseBooting, node.Status.Phase,
			"cancelled slot %s keeps the progress the FSM wrote: this handler does not own status.phase", name)
		assert.Nil(t, node.Status.LastShutdownTime,
			"cancelled slot %s records no shutdown: the FSM stamps it when its ShutdownNode runs", name)
	}

	for _, name := range []string{"slot-1", "slot-2"} {
		desired, progress := slotState(t, server, name)
		assert.Equal(t, infrav2alpha1.PowerStateOn, desired, "running machine %s is left to NodeGroupDeleteNodes", name)
		assert.Equal(t, infrav2alpha1.NodePhaseReady, progress)
	}

	// The size lands exactly on what the request asked for, on the spec write alone: 4 - 2,
	// read back from the endpoint cluster-autoscaler queries.
	resp, err = server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.TargetSize, "cancelling N starting slots takes exactly N off the target size")

	// The shutdown the FSM runs on the next reconcile keeps the slot off the target size: the
	// progress moves to shuttingdown with its timestamp, and the number does not move again.
	for _, name := range []string{"slot-3", "slot-4"} {
		node := &infrav2alpha1.Node{}
		require.NoError(t, server.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: testNamespace}, node))

		node.Status.Phase = infrav2alpha1.NodePhaseDraining
		now := metav1.Now()
		node.Status.LastShutdownTime = &now
		require.NoError(t, server.Client.Status().Update(ctx, node))
	}

	resp, err = server.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.TargetSize, "the FSM-driven shutdown lands on the same target size")
}

// TestNodeGroupDeleteNodesCountsClaimedSlotAsCapacity covers the floor arithmetic on the
// delete path: a claim is capacity the group is committed to, so deleting the only claimed
// slot of a minSize 1 group crosses the floor and is refused. Before the fix the claim was
// invisible to the floor as well, and the delete silently cancelled a scale-up that the pool
// needs to reach its minimum.
func TestNodeGroupDeleteNodesCountsClaimedSlotAsCapacity(t *testing.T) {
	server := newServer(t,
		groupCR(testGroup, 1),
		slotInSystemNamespace("slot-claimed", infrav2alpha1.PowerStateOn, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		slotInSystemNamespace("slot-spare", infrav2alpha1.PowerStateOff, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	_, err := server.NodeGroupDeleteNodes(context.Background(), &pb.NodeGroupDeleteNodesRequest{
		Id:    testGroup,
		Nodes: externalNodes("slot-claimed"),
	})
	require.Error(t, err)

	st, ok := grpcstatus.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "minSize")

	desired, progress := slotState(t, server, "slot-claimed")
	assert.Equal(t, infrav2alpha1.PowerStateOn, desired, "a refused delete must leave the claim in place")
	assert.Equal(t, infrav2alpha1.NodePhaseOff, progress)
}
