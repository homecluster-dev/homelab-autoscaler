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

package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

const slotNamespace = "homelab-autoscaler-system"

func slotScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	return scheme
}

// slot builds a Node CR (a slot) with the given observed power and reported phase.
func slot(name string, powerState infrav2alpha1.PowerState, nodePhase infrav2alpha1.NodePhase) *infrav2alpha1.Node {
	return &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: slotNamespace,
			Labels:    map[string]string{"group": "remote-workers"},
		},
		Spec: infrav2alpha1.NodeSpec{
			KubernetesNodeName: name,
		},
		Status: infrav2alpha1.NodeStatus{
			PowerState: powerState,
			Phase:      nodePhase,
		},
	}
}

func newSlotReconciler(t *testing.T, objects ...client.Object) *NodeSlotReconciler {
	t.Helper()

	scheme := slotScheme(t)
	return &NodeSlotReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objects...).
			WithStatusSubresource(&infrav2alpha1.Node{}).
			Build(),
		Scheme: scheme,
	}
}

func reconcileSlot(t *testing.T, r *NodeSlotReconciler, name string) {
	t.Helper()

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: slotNamespace},
	})
	require.NoError(t, err)
}

func getNodeCR(t *testing.T, r *NodeSlotReconciler, name string) *infrav2alpha1.Node {
	t.Helper()

	node := &infrav2alpha1.Node{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: slotNamespace}, node))

	return node
}

// TestNodeSlotReconcilerSeedsFreshPool covers the report of a committed pool: every slot has an
// empty status, and `kubectl get nodes.infra.homecluster.dev` on a pool that is idle by design must
// read as idle slots, not as broken ones.
//
// Under v1alpha1 this seeding was load-bearing for the arithmetic - the provider counted every slot
// whose progress was not shuttingdown/shutdown/startingup, so an unseeded pool reported targetSize ==
// the number of slots and cluster-autoscaler never asked for a scale up. v2alpha1 counts live claims,
// so nothing here is load-bearing: "no live claim" is the free slot, and this controller only writes
// the report.
func TestNodeSlotReconcilerSeedsFreshPool(t *testing.T) {
	r := newSlotReconciler(t,
		slot("omni-remote-1", infrav2alpha1.PowerStateOff, ""),
		slot("omni-remote-2", infrav2alpha1.PowerStateOff, ""),
	)

	reconcileSlot(t, r, "omni-remote-1")
	reconcileSlot(t, r, "omni-remote-2")

	for _, name := range []string{"omni-remote-1", "omni-remote-2"} {
		node := getNodeCR(t, r, name)
		assert.Equal(t, infrav2alpha1.PowerStateOff, node.Status.PowerState, "slot %s power state", name)
		assert.Equal(t, infrav2alpha1.NodePhaseOff, node.Status.Phase, "slot %s progress", name)
	}

	// Re-running must be a no-op (idempotent).
	before := getNodeCR(t, r, "omni-remote-1")
	reconcileSlot(t, r, "omni-remote-1")
	after := getNodeCR(t, r, "omni-remote-1")
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "status must not be rewritten when already seeded")
}

func TestNodeSlotReconcilerLeavesRealStatesAlone(t *testing.T) {
	readyK8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "omni-remote-3"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	}

	tests := []struct {
		name     string
		node     *infrav2alpha1.Node
		extra    []client.Object
		expected infrav2alpha1.NodeStatus
	}{
		{
			name:     "slot whose machine is observed on keeps its status",
			node:     slot("slot-on", infrav2alpha1.PowerStateOn, ""),
			expected: infrav2alpha1.NodeStatus{PowerState: infrav2alpha1.PowerStateOn, Phase: ""},
		},
		{
			name:     "slot that is on keeps its real status",
			node:     slot("slot-powered-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
			expected: infrav2alpha1.NodeStatus{PowerState: infrav2alpha1.PowerStateOn, Phase: infrav2alpha1.NodePhaseReady},
		},
		{
			name:     "starting up slot is not touched",
			node:     slot("slot-starting", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
			expected: infrav2alpha1.NodeStatus{PowerState: infrav2alpha1.PowerStateOff, Phase: infrav2alpha1.NodePhaseBooting},
		},
		{
			name:     "shutting down slot is not touched",
			node:     slot("slot-stopping", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
			expected: infrav2alpha1.NodeStatus{PowerState: infrav2alpha1.PowerStateOn, Phase: infrav2alpha1.NodePhaseDraining},
		},
		{
			name:     "slot with a matching kubernetes node is left to the node controller",
			node:     slot("omni-remote-3", infrav2alpha1.PowerStateOff, ""),
			extra:    []client.Object{readyK8sNode},
			expected: infrav2alpha1.NodeStatus{PowerState: infrav2alpha1.PowerStateOff, Phase: ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := append([]client.Object{tc.node}, tc.extra...)
			r := newSlotReconciler(t, objects...)

			reconcileSlot(t, r, tc.node.Name)

			node := getNodeCR(t, r, tc.node.Name)
			assert.Equal(t, tc.expected, infrav2alpha1.NodeStatus{PowerState: node.Status.PowerState, Phase: node.Status.Phase})
		})
	}
}

// TestNodeSlotReconcilerSkipsSeedAfterFSMRan guards against hiding a machine that is
// actually running. status.lastStartupTime is set by the state machine when it starts a
// slot, so a slot that carries it has been provisioned at least once: its corev1.Node object
// can be absent because the machine was deleted, because it never joined, or because an
// earlier status write failed. Seeding off/shutdown would drop a running machine from the
// target size, so the seeder leaves such a slot alone.
func TestNodeSlotReconcilerSkipsSeedAfterFSMRan(t *testing.T) {
	emptyStatusSlot := slot("slot-ever-started", "", "")
	started := metav1.Now()
	emptyStatusSlot.Status.LastStartupTime = &started

	r := newSlotReconciler(t, emptyStatusSlot)

	reconcileSlot(t, r, "slot-ever-started")

	node := getNodeCR(t, r, "slot-ever-started")
	assert.Empty(t, node.Status.PowerState, "a slot the state machine has started must not be seeded")
	assert.Empty(t, node.Status.Phase)
	assert.NotNil(t, node.Status.LastStartupTime)
}

// TestNodeSlotReconcilerSeedsSlotNeverStarted pins the intended behaviour: a slot without a
// recorded startup and without a Kubernetes node is a never-provisioned placeholder and is
// seeded.
func TestNodeSlotReconcilerSeedsSlotNeverStarted(t *testing.T) {
	r := newSlotReconciler(t, slot("slot-never-started", infrav2alpha1.PowerStateOff, ""))

	reconcileSlot(t, r, "slot-never-started")

	node := getNodeCR(t, r, "slot-never-started")
	assert.Equal(t, infrav2alpha1.PowerStateOff, node.Status.PowerState)
	assert.Equal(t, infrav2alpha1.NodePhaseOff, node.Status.Phase)
}

func TestNodeSlotReconcilerCompletesPartialStatus(t *testing.T) {
	// progress already shutdown, power state never observed: only the power state is filled in.
	r := newSlotReconciler(t, slot("slot-partial", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff))

	reconcileSlot(t, r, "slot-partial")

	node := getNodeCR(t, r, "slot-partial")
	assert.Equal(t, infrav2alpha1.PowerStateOff, node.Status.PowerState)
	assert.Equal(t, infrav2alpha1.NodePhaseOff, node.Status.Phase)
}

func TestNodeSlotReconcilerDeletedNodeCR(t *testing.T) {
	r := newSlotReconciler(t, slot("slot-gone", infrav2alpha1.PowerStateOff, ""))

	require.NoError(t, r.Delete(context.Background(), &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "slot-gone", Namespace: slotNamespace},
	}))

	reconcileSlot(t, r, "slot-gone")
}

func TestNeedsStatusSeed(t *testing.T) {
	tests := []struct {
		name     string
		node     *infrav2alpha1.Node
		expected bool
	}{
		{"fresh off slot", slot("n", infrav2alpha1.PowerStateOff, ""), true},
		{"off slot, empty progress, power state off", slot("n", infrav2alpha1.PowerStateOff, ""), true},
		{"already seeded", slot("n", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff), false},
		{"desired on", slot("n", infrav2alpha1.PowerStateOn, ""), false},
		{"desired on and off status", slot("n", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff), false},
		{"power state on", slot("n", infrav2alpha1.PowerStateOn, ""), false},
		{"progress ready", slot("n", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseReady), false},
		{"progress starting up", slot("n", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting), false},
		{"progress shutting down", slot("n", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, needsStatusSeed(tc.node))
		})
	}
}
