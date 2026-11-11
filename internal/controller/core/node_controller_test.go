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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	autoscalerpkg "github.com/homecluster-dev/homelab-autoscaler/internal/autoscaler"
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

// TestK8sNodeReconcilerRecordsABootWindowWithoutTouchingProgress is the regression test
// for the boot-window stomp. enterReady runs when the startup Job succeeds, and the Job
// is only the power-on command: the node object appears later, when the kubelet
// registers (setNodeSchedulableWithRetry retries for 90s because the node is not Ready
// yet). So {desired: on, status.powerState: off, progress: ready} plus a NotReady node is
// the normal state of every boot. A NotReady node is not evidence that the machine is
// off, so the slot must come out of the reconcile exactly as it went in: rewriting
// progress to shutdown here makes the state machine, which reads its state from progress,
// start the machine again (second startup Job, second scale-up event, autoscaler taint
// removed from a node that has not joined, targetSize dipping to 0).
func TestK8sNodeReconcilerRecordsABootWindowWithoutTouchingProgress(t *testing.T) {
	r := newK8sNodeReconciler(t,
		slot("omni-remote-2", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady),
		managedNode("omni-remote-2", false),
	)

	before := slotFrom(t, r.Client, "omni-remote-2")
	reconcileK8sNode(t, r, "omni-remote-2")
	reconcileK8sNode(t, r, "omni-remote-2")

	node := slotFrom(t, r.Client, "omni-remote-2")
	assert.Equal(t, infrav1alpha1.ProgressReady, node.Status.Progress, "a NotReady node during boot must not stomp ready")
	assert.Equal(t, infrav1alpha1.PowerStateOff, node.Status.PowerState)
	assert.Equal(t, before.Status, node.Status)
	assert.Equal(t, before.ResourceVersion, node.ResourceVersion, "nothing to record during the boot window, so no status write")
}

// TestK8sNodeReconcilerCorrectsProgressToShutdownWhenTheMachineTurnsOff is the mirror
// case, and the one the shutdown correction exists for: the recorded power state was on
// and the machine went away, so the observed off is a real change and the stale "ready"
// progress has to be corrected in the same write.
func TestK8sNodeReconcilerCorrectsProgressToShutdownWhenTheMachineTurnsOff(t *testing.T) {
	r := newK8sNodeReconciler(t,
		slot("omni-remote-5", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
		managedNode("omni-remote-5", false),
	)

	reconcileK8sNode(t, r, "omni-remote-5")

	node := slotFrom(t, r.Client, "omni-remote-5")
	assert.Equal(t, infrav1alpha1.PowerStateOff, node.Status.PowerState, "the power state change is recorded")
	assert.Equal(t, infrav1alpha1.ProgressShutdown, node.Status.Progress, "a real power state change corrects the progress")
}

// TestK8sNodeReconcilerLeavesShutdownWindowAlone is the mirror of the boot-window test and
// the reason the observed-on correction is gated on spec.desiredPowerState too:
// fsm.hooks.enterShutdown writes progress=shutdown when the shutdown Job submits the
// power-off, and the corev1.Node keeps NodeReady=True for the node-monitor-grace-period
// (~40s). A Node event in that window observes {desired: off, status.powerState: on,
// progress: shutdown} with no power state change. Correcting that to ready makes the FSM,
// which reads its state from progress, derive StateReady, and the infra NodeController
// then fires a second ShutdownNode for a machine that is already being powered off: a
// duplicate shutdown Job.
func TestK8sNodeReconcilerLeavesShutdownWindowAlone(t *testing.T) {
	r := newK8sNodeReconciler(t,
		slot("omni-remote-6", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown),
		managedNode("omni-remote-6", true),
	)

	before := slotFrom(t, r.Client, "omni-remote-6")
	reconcileK8sNode(t, r, "omni-remote-6")
	reconcileK8sNode(t, r, "omni-remote-6")

	node := slotFrom(t, r.Client, "omni-remote-6")
	assert.Equal(t, infrav1alpha1.ProgressShutdown, node.Status.Progress, "a shutdown that is already recorded must not be flipped back to ready")
	assert.Equal(t, infrav1alpha1.PowerStateOn, node.Status.PowerState, "the machine is still observed up during the grace period")
	assert.Equal(t, infrav1alpha1.PowerStateOff, node.Spec.DesiredPowerState, "spec must not be touched")
	assert.Equal(t, before.Status, node.Status)
	assert.Equal(t, before.ResourceVersion, node.ResourceVersion, "nothing to record during the shutdown window, so no status write")
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
		desired      infrav1alpha1.PowerState
		powerState   infrav1alpha1.PowerState
		observed     infrav1alpha1.PowerState
		progress     infrav1alpha1.Progress
		wantChange   bool
		wantPower    infrav1alpha1.PowerState
		wantProgress infrav1alpha1.Progress
	}{
		{"power state change derives progress", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown, true, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady},
		{"progress corrected without a power state change", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown, true, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady},
		{"observed on fills in an empty progress", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, "", true, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady},
		{"power state change to off corrects ready to shutdown", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady, true, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown},
		{"boot window: not ready node, slot already recorded off, keeps ready", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady, false, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady},
		{"boot window: unrecorded progress of a slot that is already off is left alone", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, "", false, infrav1alpha1.PowerStateOff, ""},
		{
			// The mirror of the live bug, gated by spec.desiredPowerState: enterShutdown has
			// already recorded progress=shutdown when the shutdown Job submitted the power-off,
			// and the corev1.Node stays Ready for the node-monitor-grace-period. Flipping that
			// to ready makes the FSM derive StateReady and the infra controller fires a second
			// ShutdownNode, i.e. a duplicate shutdown Job.
			name:         "shutdown window: ready node of a slot that is desired off keeps shutdown",
			desired:      infrav1alpha1.PowerStateOff,
			powerState:   infrav1alpha1.PowerStateOn,
			observed:     infrav1alpha1.PowerStateOn,
			progress:     infrav1alpha1.ProgressShutdown,
			wantChange:   false,
			wantPower:    infrav1alpha1.PowerStateOn,
			wantProgress: infrav1alpha1.ProgressShutdown,
		},
		{"startingup is owned by the state machine", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp, false, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp},
		{"shuttingdown is owned by the state machine", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown, false, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown},
		{"startingup survives a power state change", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp, true, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp},
		{"already consistent", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady, false, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := slot("n", tc.desired, tc.powerState, tc.progress)

			changed := applyObservedPowerState(node, tc.observed)

			assert.Equal(t, tc.wantChange, changed)
			assert.Equal(t, tc.wantPower, node.Status.PowerState)
			assert.Equal(t, tc.wantProgress, node.Status.Progress)
			assert.Equal(t, tc.desired, node.Spec.DesiredPowerState, "the request must never touch spec.desiredPowerState")
		})
	}
}

// markNode puts cluster-autoscaler's markers, and the cordon that goes with them, on a managed
// Kubernetes node: the evidence the operator could not attribute in the live trace.
func markNode(node *corev1.Node, cordoned bool, taints ...corev1.Taint) *corev1.Node {
	node.Spec.Unschedulable = cordoned
	node.Spec.Taints = taints

	return node
}

// The two markers are cluster-autoscaler's own keys; internal/autoscaler owns the constants.
var (
	softMarker    = corev1.Taint{Key: autoscalerpkg.TaintDeletionCandidate, Effect: corev1.TaintEffectPreferNoSchedule}
	hardMarker    = corev1.Taint{Key: autoscalerpkg.TaintDeletionRequested, Effect: corev1.TaintEffectNoSchedule}
	poolRoleTaint = corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}
)

// TestK8sNodeReconcilerRecordsTheAutoscalerMarkerOnce is the observability fix at the reconcile
// level. Live, a node of a freshly started slot was cordoned about a minute after it joined and
// nothing on the operator side said who did it: cluster-autoscaler's own markers -
// DeletionCandidateOfClusterAutoscaler, then ToBeDeletedByClusterAutoscaler - are on the node
// object this controller already watches, so they are recorded on the Node CR, once per
// transition.
//
// "Once" is the constraint. This reconciler runs on every event of every managed node, and an
// event per pass is an event storm on a pool that is being scaled down.
func TestK8sNodeReconcilerRecordsTheAutoscalerMarkerOnce(t *testing.T) {
	recorder := events.NewFakeRecorder(16)

	node := markNode(managedNode("omni-remote-7", true), true, poolRoleTaint, hardMarker)
	r := newK8sNodeReconciler(t,
		slot("omni-remote-7", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
		node,
	)
	r.EventRecorder = recorder

	reconcileK8sNode(t, r, "omni-remote-7")

	recorded := drainK8sEvents(recorder)
	require.Len(t, recorded, 1, "one transition, one event")
	assert.Contains(t, recorded[0], "Normal", "the autoscaler marking a node is a decision, not an incident")
	assert.Contains(t, recorded[0], autoscalerpkg.ReasonDeletionRequested)
	assert.Contains(t, recorded[0], "omni-remote-7", "the event names the node")
	assert.Contains(t, recorded[0], autoscalerpkg.TaintDeletionRequested)
	assert.Contains(t, recorded[0], "node.kubernetes.io/role", "the event carries the taints it read, so a cordon is attributable")

	condition := meta.FindStatusCondition(slotFrom(t, r.Client, "omni-remote-7").Status.Conditions,
		autoscalerpkg.ConditionMarkedForDeletion)
	require.NotNil(t, condition, "the record lives on the slot")
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, autoscalerpkg.ReasonDeletionRequested, condition.Reason)

	// The steady state: the controller sees the same node on every event, and must neither write
	// the slot nor record anything.
	before := slotFrom(t, r.Client, "omni-remote-7")

	for range 3 {
		reconcileK8sNode(t, r, "omni-remote-7")
	}

	after := slotFrom(t, r.Client, "omni-remote-7")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "a marker that is already recorded writes nothing")
	assert.Zero(t, len(drainK8sEvents(recorder)), "and records nothing")
}

// TestK8sNodeReconcilerMarkerTransitionsAreAllRecorded walks the sequence the live pool went
// through: candidate, then delete request, then unmarked, then marked again. Each transition is
// recorded, the unmarking is a status write without an event, and the reappearance is a new event
// - which is the answer to "did the autoscaler decide twice about this node".
func TestK8sNodeReconcilerMarkerTransitionsAreAllRecorded(t *testing.T) {
	recorder := events.NewFakeRecorder(16)

	node := markNode(managedNode("omni-remote-8", true), false, poolRoleTaint, softMarker)
	r := newK8sNodeReconciler(t,
		slot("omni-remote-8", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
		node,
	)
	r.EventRecorder = recorder

	reconcileK8sNode(t, r, "omni-remote-8")

	recorded := drainK8sEvents(recorder)
	require.Len(t, recorded, 1)
	assert.Contains(t, recorded[0], autoscalerpkg.ReasonDeletionCandidate)

	// The candidate becomes a delete request: a new marker, a new event.
	live := &corev1.Node{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "omni-remote-8"}, live))
	markNode(live, true, poolRoleTaint, hardMarker)
	require.NoError(t, r.Update(context.Background(), live))

	reconcileK8sNode(t, r, "omni-remote-8")

	recorded = drainK8sEvents(recorder)
	require.Len(t, recorded, 1, "the hard marker is a transition from the soft one")
	assert.Contains(t, recorded[0], autoscalerpkg.ReasonDeletionRequested)

	// The autoscaler backs off: recorded on the slot, no event.
	live = &corev1.Node{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "omni-remote-8"}, live))
	markNode(live, false, poolRoleTaint)
	require.NoError(t, r.Update(context.Background(), live))

	reconcileK8sNode(t, r, "omni-remote-8")

	assert.Zero(t, len(drainK8sEvents(recorder)), "a marker that disappeared is not an event")

	condition := meta.FindStatusCondition(slotFrom(t, r.Client, "omni-remote-8").Status.Conditions,
		autoscalerpkg.ConditionMarkedForDeletion)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, autoscalerpkg.ReasonNotMarked, condition.Reason)

	// It comes back, and that is a new transition with a new event.
	live = &corev1.Node{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "omni-remote-8"}, live))
	markNode(live, true, poolRoleTaint, hardMarker)
	require.NoError(t, r.Update(context.Background(), live))

	reconcileK8sNode(t, r, "omni-remote-8")

	recorded = drainK8sEvents(recorder)
	require.Len(t, recorded, 1, "a reappearance is a new decision, not a repeat")
	assert.Contains(t, recorded[0], autoscalerpkg.ReasonDeletionRequested)
}

// TestK8sNodeReconcilerRecordsNothingForACleanNode is the fast path of a healthy pool: no marker,
// no condition, no status write, no event. The recording must cost a pool that cluster-autoscaler
// never touched nothing at all.
func TestK8sNodeReconcilerRecordsNothingForACleanNode(t *testing.T) {
	recorder := events.NewFakeRecorder(16)

	r := newK8sNodeReconciler(t,
		slot("omni-remote-9", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
		markNode(managedNode("omni-remote-9", true), false, poolRoleTaint),
	)
	r.EventRecorder = recorder

	before := slotFrom(t, r.Client, "omni-remote-9")
	reconcileK8sNode(t, r, "omni-remote-9")
	reconcileK8sNode(t, r, "omni-remote-9")

	after := slotFrom(t, r.Client, "omni-remote-9")
	assert.Equal(t, before.Status, after.Status, "nothing to record, so the status is untouched")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
	assert.Empty(t, after.Status.Conditions, "no condition added to a slot the autoscaler never marked")
	assert.Zero(t, len(drainK8sEvents(recorder)))
}

// drainK8sEvents empties the fake recorder's channel and returns everything it buffered.
func drainK8sEvents(recorder *events.FakeRecorder) []string {
	var collected []string

	for {
		select {
		case event := <-recorder.Events:
			collected = append(collected, event)
		default:
			return collected
		}
	}
}
