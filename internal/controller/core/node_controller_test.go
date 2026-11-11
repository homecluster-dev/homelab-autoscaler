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

	. "github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

var _ = Describe("Node Controller", func() {
	Context("When reconciling a resource", func() {

		It("should successfully reconcile Kubernetes node status changes", func() {
			// Test that node status changes are properly synchronized to Homelab Node CRDs
			// and that power state transitions are handled correctly
		})
	})
})

// managedNode builds a Kubernetes node that points at a slot through the label the
// controller uses to find the Node CR, with the given Ready condition.
func managedNode(name string, ready bool) *corev1.Node {
	conditionStatus := corev1.ConditionFalse
	if ready {
		conditionStatus = corev1.ConditionTrue
	}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"infra.homecluster.dev/node": name},
		},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: conditionStatus},
		}},
	}
}

// unmanagedNode builds a Kubernetes node that carries no slot label.
func unmanagedNode(name string) *corev1.Node {
	node := managedNode(name, true)
	delete(node.Labels, "infra.homecluster.dev/node")

	return node
}

func newK8sNodeReconciler(t *testing.T, objects ...client.Object) *KubernetesNodeReconciler {
	t.Helper()

	scheme := slotScheme(t)

	return &KubernetesNodeReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objects...).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build(),
		Scheme: scheme,
	}
}

// slotFrom reads a Node CR (a slot) back from the client.
func slotFrom(t *testing.T, c client.Client, name string) *infrav1alpha1.Node {
	t.Helper()

	node := &infrav1alpha1.Node{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: slotNamespace}, node))

	return node
}

func reconcileK8sNode(t *testing.T, r *KubernetesNodeReconciler, name string) {
	t.Helper()

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	require.NoError(t, err)
}

// TestK8sNodeReconcilerCorrectsProgressWhenPowerStateUnchanged pins the live bug: a slot
// that was shut down and started again (desiredPowerState back to "on", node Ready) keeps
// status.powerState: "on", so the power-state short circuit used to return before the
// progress correction and status.progress stayed "shutdown" forever. That stale progress
// is not cosmetic: NodeGroupTargetSize excludes slots whose progress is
// shutdown/shuttingdown, so a group with one Ready node reports targetSize 0 and
// cluster-autoscaler refuses to scale.
func TestK8sNodeReconcilerCorrectsProgressWhenPowerStateUnchanged(t *testing.T) {
	r := newK8sNodeReconciler(t,
		slot("omni-remote-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown),
		managedNode("omni-remote-1", true),
	)

	reconcileK8sNode(t, r, "omni-remote-1")

	node := slotFrom(t, r.Client, "omni-remote-1")
	assert.Equal(t, infrav1alpha1.PowerStateOn, node.Status.PowerState, "power state stays as observed")
	assert.Equal(t, infrav1alpha1.ProgressReady, node.Status.Progress, "progress must follow the observed power state")
	assert.Equal(t, infrav1alpha1.PowerStateOn, node.Spec.DesiredPowerState, "spec must not be touched")
}

// TestK8sNodeReconcilerCorrectsProgressToShutdownForOffNode is the mirror case: the node
// went away (NotReady) while the recorded power state was already off, so only the stale
// "ready" progress has to be corrected.
func TestK8sNodeReconcilerCorrectsProgressToShutdownForOffNode(t *testing.T) {
	r := newK8sNodeReconciler(t,
		slot("omni-remote-2", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady),
		managedNode("omni-remote-2", false),
	)

	reconcileK8sNode(t, r, "omni-remote-2")

	node := slotFrom(t, r.Client, "omni-remote-2")
	assert.Equal(t, infrav1alpha1.PowerStateOff, node.Status.PowerState)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, node.Status.Progress)
}

// TestK8sNodeReconcilerLeavesTransitionalProgressAlone: startingup and shuttingdown are
// owned by the state machine, which is the only one that knows a power operation is in
// flight. The reconciler corrects the power state, never those values.
func TestK8sNodeReconcilerLeavesTransitionalProgressAlone(t *testing.T) {
	tests := []struct {
		name         string
		slot         *infrav1alpha1.Node
		ready        bool
		wantPower    infrav1alpha1.PowerState
		wantProgress infrav1alpha1.Progress
	}{
		{
			name:         "starting up slot with a not ready node keeps startingup",
			slot:         slot("slot-starting", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
			ready:        false,
			wantPower:    infrav1alpha1.PowerStateOff,
			wantProgress: infrav1alpha1.ProgressStartingUp,
		},
		{
			name:         "shutting down slot with a ready node keeps shuttingdown",
			slot:         slot("slot-stopping", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown),
			ready:        true,
			wantPower:    infrav1alpha1.PowerStateOn,
			wantProgress: infrav1alpha1.ProgressShuttingDown,
		},
		{
			name:         "starting up slot whose node joined keeps startingup",
			slot:         slot("slot-joined", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp),
			ready:        true,
			wantPower:    infrav1alpha1.PowerStateOn,
			wantProgress: infrav1alpha1.ProgressStartingUp,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newK8sNodeReconciler(t, tc.slot, managedNode(tc.slot.Name, tc.ready))

			reconcileK8sNode(t, r, tc.slot.Name)

			node := slotFrom(t, r.Client, tc.slot.Name)
			assert.Equal(t, tc.wantPower, node.Status.PowerState, "power state follows the observation")
			assert.Equal(t, tc.wantProgress, node.Status.Progress, "transitional progress is never overwritten")
		})
	}
}

// TestK8sNodeReconcilerSkipsStatusWriteWhenConsistent covers the fast path: when the
// recorded power state and the progress that corresponds to it already agree, the
// reconciler must not write the status. A write on every reconcile bumps the
// resourceVersion of the slot, re-triggers the watches and keeps the controller hot.
func TestK8sNodeReconcilerSkipsStatusWriteWhenConsistent(t *testing.T) {
	tests := []struct {
		name  string
		slot  *infrav1alpha1.Node
		ready bool
	}{
		{"on and ready, node ready", slot("slot-ready", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady), true},
		{"off and shutdown, node not ready", slot("slot-off", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown), false},
		{"on and startingup, node ready", slot("slot-starting", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp), true},
		{"off and shuttingdown, node not ready", slot("slot-stopping", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newK8sNodeReconciler(t, tc.slot, managedNode(tc.slot.Name, tc.ready))

			before := slotFrom(t, r.Client, tc.slot.Name)
			reconcileK8sNode(t, r, tc.slot.Name)
			after := slotFrom(t, r.Client, tc.slot.Name)

			assert.Equal(t, before.Status, after.Status)
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "no status write when power state and progress already agree")
		})
	}
}

// TestK8sNodeReconcilerProgressCorrectionIsIdempotent: the correction runs once and the
// following reconciles, which is what the controller does on every node event, are no-ops.
func TestK8sNodeReconcilerProgressCorrectionIsIdempotent(t *testing.T) {
	r := newK8sNodeReconciler(t,
		slot("omni-remote-3", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown),
		managedNode("omni-remote-3", true),
	)

	reconcileK8sNode(t, r, "omni-remote-3")

	after := slotFrom(t, r.Client, "omni-remote-3")
	require.Equal(t, infrav1alpha1.ProgressReady, after.Status.Progress)

	for range 3 {
		reconcileK8sNode(t, r, "omni-remote-3")
	}

	again := slotFrom(t, r.Client, "omni-remote-3")
	assert.Equal(t, after.Status, again.Status)
	assert.Equal(t, after.ResourceVersion, again.ResourceVersion, "a corrected slot must not be rewritten")
}

func TestK8sNodeReconcilerIgnoresUnmanagedAndMissingObjects(t *testing.T) {
	t.Run("kubernetes node without the slot label is skipped", func(t *testing.T) {
		r := newK8sNodeReconciler(t,
			slot("omni-remote-4", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown),
			unmanagedNode("omni-remote-4"),
		)

		reconcileK8sNode(t, r, "omni-remote-4")

		node := slotFrom(t, r.Client, "omni-remote-4")
		assert.Equal(t, infrav1alpha1.ProgressShutdown, node.Status.Progress, "an unmanaged node must not be touched")
	})

	t.Run("missing kubernetes node is not an error", func(t *testing.T) {
		r := newK8sNodeReconciler(t)
		reconcileK8sNode(t, r, "gone")
	})

	t.Run("missing node CR is not an error", func(t *testing.T) {
		r := newK8sNodeReconciler(t, managedNode("orphan", true))
		reconcileK8sNode(t, r, "orphan")
	})
}

func TestExpectedProgress(t *testing.T) {
	tests := []struct {
		powerState infrav1alpha1.PowerState
		progress   infrav1alpha1.Progress
		ok         bool
	}{
		{infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady, true},
		{infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown, true},
		{"", "", false},
	}

	for _, tc := range tests {
		progress, ok := expectedProgress(tc.powerState)
		assert.Equal(t, tc.progress, progress, "progress for power state %q", tc.powerState)
		assert.Equal(t, tc.ok, ok, "derived flag for power state %q", tc.powerState)
	}
}

func TestApplyObservedPowerState(t *testing.T) {
	tests := []struct {
		name         string
		powerState   infrav1alpha1.PowerState
		observed     infrav1alpha1.PowerState
		progress     infrav1alpha1.Progress
		wantChange   bool
		wantPower    infrav1alpha1.PowerState
		wantProgress infrav1alpha1.Progress
	}{
		{"power state change derives progress", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown, true, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady},
		{"progress corrected without a power state change", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown, true, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady},
		{"stale ready corrected for an off slot", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady, true, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown},
		{"empty progress is filled in", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, "", true, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown},
		{"startingup is owned by the state machine", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp, false, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp},
		{"shuttingdown is owned by the state machine", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown, false, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown},
		{"startingup survives a power state change", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp, true, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp},
		{"already consistent", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady, false, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := slot("n", infrav1alpha1.PowerStateOn, tc.powerState, tc.progress)

			changed := applyObservedPowerState(node, tc.observed)

			assert.Equal(t, tc.wantChange, changed)
			assert.Equal(t, tc.wantPower, node.Status.PowerState)
			assert.Equal(t, tc.wantProgress, node.Status.Progress)
		})
	}
}
