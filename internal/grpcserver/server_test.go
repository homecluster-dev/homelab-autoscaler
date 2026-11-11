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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

const testGroup = "remote-workers"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	return scheme
}

// nodeCR builds a Node CR (a slot) of the given group and power state.
func nodeCR(name string, desired infrav1alpha1.PowerState, status infrav1alpha1.PowerState, progress infrav1alpha1.Progress) *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"group": testGroup},
		},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  desired,
			KubernetesNodeName: name,
		},
		Status: infrav1alpha1.NodeStatus{
			PowerState: status,
			Progress:   progress,
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
func slotWithoutKubernetesNodeName(name string, desired infrav1alpha1.PowerState) *infrav1alpha1.Node {
	node := nodeCR(name, desired, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
	node.Spec.KubernetesNodeName = ""

	return node
}

func newServer(t *testing.T, objects ...client.Object) *HomeClusterProviderServer {
	scheme := testScheme(t)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1alpha1.Node{}).
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
				nodeCR("slot-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{},
		},
		{
			// The machine is off but the kubelet node object is still there (NotReady), so the
			// slot is an instance that is going away. Cluster-autoscaler matches the node to it
			// by spec.providerID, so it must not look like an unregistered node.
			name: "off slot with a kubernetes node is deleting",
			objects: []client.Object{
				nodeCR("slot-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
				k8sNode("slot-1", "homelab://slot-1"),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceDeleting,
			},
		},
		{
			name: "off slot with a kubernetes node without providerID falls back to the node CR identity",
			objects: []client.Object{
				nodeCR("slot-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
				k8sNode("slot-1", ""),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceDeleting,
			},
		},
		{
			name: "off slot whose kubernetes node name is empty is skipped",
			objects: []client.Object{
				slotWithoutKubernetesNodeName("slot-1", infrav1alpha1.PowerStateOff),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{},
		},
		{
			name: "slot that is on without a kubernetes node is provisioning",
			objects: []client.Object{
				nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceCreating,
			},
		},
		{
			name: "slot that is on with a kubernetes node is running",
			objects: []client.Object{
				nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
				k8sNode("slot-1", "homelab://slot-1"),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceRunning,
			},
		},
		{
			name: "empty providerID falls back to the provider id the operator writes",
			objects: []client.Object{
				nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
				k8sNode("slot-1", ""),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceRunning,
			},
		},
		{
			name: "slot without a kubernetes node name is provisioning and keeps an id",
			objects: []client.Object{
				slotWithoutKubernetesNodeName("slot-1", infrav1alpha1.PowerStateOn),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceCreating,
			},
		},
		{
			name: "mixed pool reports every slot with a machine and skips spare capacity",
			objects: []client.Object{
				// fresh, never provisioned slot of a 15 slot pool, no machine and no node object
				nodeCR("slot-seeded", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
				// scaled down slot, the machine is off and the node object is left over
				nodeCR("slot-off", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
				k8sNode("slot-off", "homelab://slot-off"),
				// slot requested by IncreaseSize, machine not joined yet
				nodeCR("slot-new", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
				// running slot without a cloud-controller-manager
				nodeCR("omni-remote-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
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
	server := newServer(t, nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp))

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
	server := newServer(t, slotWithoutKubernetesNodeName("slot-1", infrav1alpha1.PowerStateOn))

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
		nodeCR("slot-running", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
		k8sNode("slot-running", "homelab://slot-running"),
		nodeCR("slot-creating", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
		nodeCR("slot-deleting", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		k8sNode("slot-deleting", "homelab://slot-deleting"),
		nodeCR("slot-spare", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
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
		nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
		k8sNode("slot-1", "homelab://slot-1"),
	)

	ctx := context.Background()

	resp, err := server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	require.Len(t, resp.Instances, 1)
	assert.Equal(t, pb.InstanceStatus_instanceRunning, resp.Instances[0].Status.InstanceState)

	// NodeGroupDeleteNodes sets the desired state to off; the machine powers off and the node
	// object stays behind, NotReady.
	slot := &infrav1alpha1.Node{}
	require.NoError(t, server.Client.Get(ctx, client.ObjectKey{Name: "slot-1", Namespace: "default"}, slot))
	slot.Spec.DesiredPowerState = infrav1alpha1.PowerStateOff
	require.NoError(t, server.Client.Update(ctx, slot))

	resp, err = server.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	require.Len(t, resp.Instances, 1)
	assert.Equal(t, pb.InstanceStatus_instanceDeleting, resp.Instances[0].Status.InstanceState)
	assert.Equal(t, "homelab://slot-1", resp.Instances[0].Id, "instance id must not change when the slot is scaled down")
}

func TestNodeGroupTargetSizeIgnoresSeededSlots(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		nodeCR("slot-2", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		nodeCR("slot-3", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
		nodeCR("slot-4", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
		nodeCR("slot-5", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown),
	)

	resp, err := server.NodeGroupTargetSize(context.Background(), &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	// only the ready and the starting up slot count towards the target size
	assert.Equal(t, int32(2), resp.TargetSize)
}

func TestNodeGroupIncreaseSizePicksSeededOffSlot(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		nodeCR("slot-2", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown),
	)

	_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.NoError(t, err)

	updated := &infrav1alpha1.Node{}
	require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: "slot-1", Namespace: "default"}, updated))
	assert.Equal(t, infrav1alpha1.PowerStateOn, updated.Spec.DesiredPowerState)

	// the shutting down slot must not be touched
	stillOff := &infrav1alpha1.Node{}
	require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: "slot-2", Namespace: "default"}, stillOff))
	assert.Equal(t, infrav1alpha1.PowerStateOff, stillOff.Spec.DesiredPowerState)
}

func TestNodeGroupIncreaseSizeFailsWithoutFreeSlot(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
	)

	_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no powered off nodes found")
}
