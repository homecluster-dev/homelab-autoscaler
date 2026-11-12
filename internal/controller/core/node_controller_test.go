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
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	autoscalerpkg "github.com/homecluster-dev/homelab-autoscaler/internal/autoscaler"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
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

// slotClaim builds a live NodeClaim of a test slot. The claim is the first input of the derived
// report: a slot whose machine is up and that no claim wants is draining, and the same slot with a
// ready claim is ready. Tests that care about the report of a claimed slot pass a claim, exactly as
// production reads one.
func slotClaim(slotName string, claimPhase infrav2alpha1.NodeClaimPhase) *infrav2alpha1.NodeClaim {
	return &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      slotName + "-claim",
			Namespace: slotNamespace,
			Labels:    map[string]string{"infra.homecluster.dev/node": slotName},
		},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef: corev1.ObjectReference{Name: slotName, Namespace: slotNamespace},
			Request: infrav2alpha1.ClaimRequest{Call: infrav2alpha1.ClaimCallIncreaseSize, Delta: 1, Group: "remote-workers"},
		},
		Status: infrav2alpha1.NodeClaimStatus{Phase: claimPhase, Attempt: 1},
	}
}

func newK8sNodeReconciler(t *testing.T, objects ...client.Object) *KubernetesNodeReconciler {
	t.Helper()

	scheme := slotScheme(t)

	return &KubernetesNodeReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objects...).
			WithStatusSubresource(&infrav2alpha1.Node{}).
			Build(),
		Scheme: scheme,
	}
}

// slotFrom reads a Node CR (a slot) back from the client.
func slotFrom(t *testing.T, c client.Client, name string) *infrav2alpha1.Node {
	t.Helper()

	node := &infrav2alpha1.Node{}
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

// TestK8sNodeReconcilerCorrectsProgressWhenPowerStateUnchanged pins the live bug of v1alpha1, and
// shows what replaced its rule: a slot whose machine booted through a path this controller did not
// observe kept status.powerState: "on" while its recorded progress stayed "shutdown", and the
// provider's target size - which excluded a shutdown progress - reported 0 for a Ready node.
//
// v2alpha1 has no such rule to keep. The report is computed from (the live claim, the observed
// power, the node), so an observed on with a live claim is ready in the same write that records the
// power, and the target size counts the claim, not the report.
func TestK8sNodeReconcilerCorrectsProgressWhenPowerStateUnchanged(t *testing.T) {
	r := newK8sNodeReconciler(t,
		slot("omni-remote-1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseOff),
		slotClaim("omni-remote-1", infrav2alpha1.NodeClaimReady),
		managedNode("omni-remote-1", true),
	)

	reconcileK8sNode(t, r, "omni-remote-1")

	node := slotFrom(t, r.Client, "omni-remote-1")
	assert.Equal(t, infrav2alpha1.PowerStateOn, node.Status.PowerState, "power state stays as observed")
	assert.Equal(t, infrav2alpha1.NodePhaseReady, node.Status.Phase, "the report is derived from the live claim and the observed power")
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
		slot("omni-remote-2", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseReady),
		slotClaim("omni-remote-2", infrav2alpha1.NodeClaimReady),
		managedNode("omni-remote-2", false),
	)

	reconcileK8sNode(t, r, "omni-remote-2")

	node := slotFrom(t, r.Client, "omni-remote-2")
	// A live claim whose machine is not observed up is a slot in its boot window: the report says
	// booting, and the state machine's answer to the same inputs is "boot it". The NotReady node is
	// not turned into a free slot, and no rule protects a written value: the value is computed.
	assert.Equal(t, infrav2alpha1.NodePhaseBooting, node.Status.Phase, "a NotReady node under a live claim is a boot window")
	assert.Equal(t, infrav2alpha1.PowerStateOff, node.Status.PowerState)

	before := slotFrom(t, r.Client, "omni-remote-2")
	reconcileK8sNode(t, r, "omni-remote-2")
	after := slotFrom(t, r.Client, "omni-remote-2")
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "the report already matches the inputs, so nothing is written")
}

// TestK8sNodeReconcilerCorrectsProgressToShutdownWhenTheMachineTurnsOff is the mirror
// case, and the one the shutdown correction exists for: the recorded power state was on
// and the machine went away, so the observed off is a real change and the stale "ready"
// progress has to be corrected in the same write.
func TestK8sNodeReconcilerCorrectsProgressToShutdownWhenTheMachineTurnsOff(t *testing.T) {
	r := newK8sNodeReconciler(t,
		slot("omni-remote-5", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		managedNode("omni-remote-5", false),
	)

	reconcileK8sNode(t, r, "omni-remote-5")

	node := slotFrom(t, r.Client, "omni-remote-5")
	assert.Equal(t, infrav2alpha1.PowerStateOff, node.Status.PowerState, "the power state change is recorded")
	assert.Equal(t, infrav2alpha1.NodePhaseOff, node.Status.Phase, "a real power state change corrects the progress")
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
		slot("omni-remote-6", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
		managedNode("omni-remote-6", true),
	)

	before := slotFrom(t, r.Client, "omni-remote-6")
	reconcileK8sNode(t, r, "omni-remote-6")
	reconcileK8sNode(t, r, "omni-remote-6")

	node := slotFrom(t, r.Client, "omni-remote-6")
	assert.Equal(t, infrav2alpha1.NodePhaseDraining, node.Status.Phase,
		"the shutdown window reports draining: the machine is observed up and no claim wants it")
	assert.Equal(t, infrav2alpha1.PowerStateOn, node.Status.PowerState, "the machine is still observed up during the grace period")
	assert.Equal(t, before.Status, node.Status)
	assert.Equal(t, before.ResourceVersion, node.ResourceVersion, "nothing to record during the shutdown window, so no status write")
}

// TestK8sNodeReconcilerLeavesTransitionalProgressAlone: startingup and shuttingdown are
// owned by the state machine, which is the only one that knows a power operation is in
// flight. The reconciler corrects the power state, never those values.
func TestK8sNodeReconcilerLeavesTransitionalProgressAlone(t *testing.T) {
	tests := []struct {
		name         string
		slot         *infrav2alpha1.Node
		claim        *infrav2alpha1.NodeClaim
		ready        bool
		wantPower    infrav2alpha1.PowerState
		wantProgress infrav2alpha1.NodePhase
	}{
		{
			name:         "starting up slot with a not ready node keeps startingup",
			slot:         slot("slot-starting", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting),
			claim:        slotClaim("slot-starting", infrav2alpha1.NodeClaimBooting),
			ready:        false,
			wantPower:    infrav2alpha1.PowerStateOff,
			wantProgress: infrav2alpha1.NodePhaseBooting,
		},
		{
			name:         "shutting down slot with a ready node keeps shuttingdown",
			slot:         slot("slot-stopping", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining),
			ready:        true,
			wantPower:    infrav2alpha1.PowerStateOn,
			wantProgress: infrav2alpha1.NodePhaseDraining,
		},
		{
			name:         "starting up slot whose node joined keeps startingup",
			slot:         slot("slot-joined", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting),
			claim:        slotClaim("slot-joined", infrav2alpha1.NodeClaimBooting),
			ready:        true,
			wantPower:    infrav2alpha1.PowerStateOn,
			wantProgress: infrav2alpha1.NodePhaseBooting,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{tc.slot, managedNode(tc.slot.Name, tc.ready)}
			if tc.claim != nil {
				objects = append(objects, tc.claim)
			}

			r := newK8sNodeReconciler(t, objects...)

			reconcileK8sNode(t, r, tc.slot.Name)

			node := slotFrom(t, r.Client, tc.slot.Name)
			assert.Equal(t, tc.wantPower, node.Status.PowerState, "power state follows the observation")
			assert.Equal(t, tc.wantProgress, node.Status.Phase,
				"the report of a slot with a live claim is derived from the claim, not from the power alone")
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
		slot  *infrav2alpha1.Node
		claim *infrav2alpha1.NodeClaim
		ready bool
	}{
		{name: "on and ready, node ready, live claim ready", slot: slot("slot-ready", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady), claim: slotClaim("slot-ready", infrav2alpha1.NodeClaimReady), ready: true},
		{name: "off and off, node not ready, no claim", slot: slot("slot-off", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff), ready: false},
		{name: "on and booting, node ready, live claim booting", slot: slot("slot-starting", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting), claim: slotClaim("slot-starting", infrav2alpha1.NodeClaimBooting), ready: true},
		{name: "off and off, node not ready, no claim (repeat)", slot: slot("slot-stopping", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff), ready: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{tc.slot, managedNode(tc.slot.Name, tc.ready)}
			if tc.claim != nil {
				objects = append(objects, tc.claim)
			}

			r := newK8sNodeReconciler(t, objects...)

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
		slot("omni-remote-3", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseOff),
		slotClaim("omni-remote-3", infrav2alpha1.NodeClaimReady),
		managedNode("omni-remote-3", true),
	)

	reconcileK8sNode(t, r, "omni-remote-3")

	after := slotFrom(t, r.Client, "omni-remote-3")
	require.Equal(t, infrav2alpha1.NodePhaseReady, after.Status.Phase)

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
			slot("omni-remote-4", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseOff),
			unmanagedNode("omni-remote-4"),
		)

		reconcileK8sNode(t, r, "omni-remote-4")

		node := slotFrom(t, r.Client, "omni-remote-4")
		assert.Equal(t, infrav2alpha1.NodePhaseOff, node.Status.Phase, "an unmanaged node must not be touched")
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

// TestApplyObservation is the observation path of the v2alpha1 model: the power state is recorded
// from the node object, and the slot's report is *computed* from (the live claim, the observed power,
// the usability of the node) by phase.Derive. There is no "which writer outranks which" rule any
// more - the v1alpha1 table of four rules collapsed into one formula, so a row of this table is a row
// of the phase state table, and the record a caller starts with is irrelevant to the value written.
func TestApplyObservation(t *testing.T) {
	const slotName = "n"

	pending := &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{Phase: infrav2alpha1.NodeClaimPending}}
	booting := &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{Phase: infrav2alpha1.NodeClaimBooting}}
	ready := &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{Phase: infrav2alpha1.NodeClaimReady}}
	failed := &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{Phase: infrav2alpha1.NodeClaimFailed}}

	tests := []struct {
		name       string
		from       infrav2alpha1.NodePhase
		claim      *infrav2alpha1.NodeClaim
		observed   infrav2alpha1.PowerState
		usable     bool
		wantChange bool
		wantPower  infrav2alpha1.PowerState
		wantPhase  infrav2alpha1.NodePhase
	}{
		{
			name:       "the live bug of v1alpha1: an observed on on a slot recorded off, with a live claim",
			from:       infrav2alpha1.NodePhaseOff,
			claim:      ready,
			observed:   infrav2alpha1.PowerStateOn,
			usable:     true,
			wantChange: true,
			wantPower:  infrav2alpha1.PowerStateOn,
			wantPhase:  infrav2alpha1.NodePhaseReady,
		},
		{
			name:       "a machine observed up that no claim wants is draining, not ready",
			from:       infrav2alpha1.NodePhaseReady,
			observed:   infrav2alpha1.PowerStateOn,
			usable:     true,
			wantChange: true,
			wantPower:  infrav2alpha1.PowerStateOn,
			wantPhase:  infrav2alpha1.NodePhaseDraining,
		},
		{
			name:       "an overdue, failed claim whose machine booted is force-shut-down: the report says draining",
			from:       infrav2alpha1.NodePhaseBooting,
			claim:      failed,
			observed:   infrav2alpha1.PowerStateOn,
			usable:     true,
			wantChange: true,
			wantPower:  infrav2alpha1.PowerStateOn,
			wantPhase:  infrav2alpha1.NodePhaseDraining,
		},
		{
			name:       "a pending claim with the machine off is reported pending, the shape IncreaseSize can cancel",
			from:       infrav2alpha1.NodePhaseOff,
			claim:      pending,
			observed:   infrav2alpha1.PowerStateOff,
			wantChange: true,
			wantPower:  infrav2alpha1.PowerStateOff,
			wantPhase:  infrav2alpha1.NodePhasePending,
		},
		{
			name:       "a booting claim is booting whatever the record said",
			from:       infrav2alpha1.NodePhaseReady,
			claim:      booting,
			observed:   infrav2alpha1.PowerStateOff,
			wantChange: true,
			wantPower:  infrav2alpha1.PowerStateOff,
			wantPhase:  infrav2alpha1.NodePhaseBooting,
		},
		{
			name:       "the boot window: a live claim, the node not usable yet, is booting, not ready",
			from:       infrav2alpha1.NodePhaseOff,
			claim:      ready,
			observed:   infrav2alpha1.PowerStateOn,
			usable:     false,
			wantChange: true,
			wantPower:  infrav2alpha1.PowerStateOn,
			wantPhase:  infrav2alpha1.NodePhaseBooting,
		},
		{
			name:       "a free slot: no claim, machine off",
			from:       "",
			observed:   infrav2alpha1.PowerStateOff,
			wantChange: true,
			wantPower:  infrav2alpha1.PowerStateOff,
			wantPhase:  infrav2alpha1.NodePhaseOff,
		},
		{
			name:       "already consistent: a report that matches the inputs writes nothing",
			from:       infrav2alpha1.NodePhaseReady,
			claim:      ready,
			observed:   infrav2alpha1.PowerStateOn,
			usable:     true,
			wantChange: false,
			wantPower:  infrav2alpha1.PowerStateOn,
			wantPhase:  infrav2alpha1.NodePhaseReady,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The record the slot starts with is deliberately not part of the inputs: the value
			// written is computed from the claim, the observation and the node.
			node := slot(slotName, tc.observed, tc.from)

			in := phase.Input{Claim: tc.claim, ObservedPower: tc.observed, NodeUsable: tc.usable}

			at := time.Date(2025, 11, 12, 9, 0, 0, 0, time.UTC)
			changed := applyObservation(node, tc.observed, in, at)

			assert.Equal(t, tc.wantChange, changed)
			assert.Equal(t, tc.wantPower, node.Status.PowerState)
			assert.Equal(t, tc.wantPhase, node.Status.Phase)

			// The history is written with the report by the one writer of it (phase.Record): a pass
			// that moved the phase dates it and records the row, a pass that moved nothing does not.
			if tc.wantPhase != tc.from {
				require.NotNil(t, node.Status.LastTransitionTime, "a phase change dates itself")
				assert.True(t, node.Status.LastTransitionTime.Time.Equal(at))
				assert.Equal(t, tc.from, node.Status.PreviousPhase)
				require.Len(t, node.Status.RecentTransitions, 1)
				assert.Equal(t, tc.wantPhase, node.Status.RecentTransitions[0].To)
				assert.NotEmpty(t, node.Status.RecentTransitions[0].Reason, "the row names the inputs")
			} else {
				assert.Nil(t, node.Status.LastTransitionTime, "a resync does not date a change that did not happen")
				assert.Empty(t, node.Status.RecentTransitions)
				assert.Zero(t, node.Status.ActivationsLast24H)
			}
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

// lifecycleJobOf is a lifecycle Job of a slot that has not finished: the fact that separates "a boot
// was asked for" from "a boot is running". The labels are the ones the controller stamps on the Jobs
// it creates (fsm.LabelNodeCR is the slot, fsm.LabelPhase the direction), so the fixture is the shape
// the production list reads.
func lifecycleJobOf(slotName, jobPhase string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      slotName + "-" + jobPhase + "-inflight",
			Namespace: slotNamespace,
			Labels: map[string]string{
				fsm.LabelNodeCR: slotName,
				fsm.LabelPhase:  jobPhase,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "lifecycle", Image: "envtest/lifecycle:latest"}},
				},
			},
		},
	}
}

// TestObservationInputAndLifecycleInputDeriveOnePhase is half 1 of a pair (the observation path's
// half): the triple "a claim pending, the machine observed off, a startup Job in flight", assembled
// by this path, derives `booting`. Its pair, TestLifecycleInputAndObservationInputDeriveOnePhase in
// internal/controller/infra/node_controller_test.go, assembles the same world through the transition
// path and asserts the same value.
//
// The pair is the point: these are the two writers of the derived report, a phase has one formula
// (phase.Derive), and a formula with one value has one way to assemble its input. This path used to
// leave the two lifecycle-Job fields empty - nothing in the observation reads a Job - so it derived
// `pending` for a machine whose boot had already started while the transition path derived
// `booting`, and since the record of the report became a writer's job that disagreement is not a
// display hiccup: the two paths append two rows to one slot's ring and count one boot twice, once per
// pass, all day. Both paths fill the fields from fsm.JobsInFlightOfSlot now, and these two tests are
// the pin that they do.
func TestObservationInputAndLifecycleInputDeriveOnePhase(t *testing.T) {
	nodeCR := slot("worker-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhasePending)
	k8sNode := managedNode("worker-1", false)
	nodeClaim := slotClaim("worker-1", infrav2alpha1.NodeClaimPending)

	reconciler := newK8sNodeReconciler(t, nodeCR, k8sNode, nodeClaim, lifecycleJobOf("worker-1", fsm.PhaseStartup))

	in := reconciler.observationInput(context.Background(), nodeCR, k8sNode, infrav2alpha1.PowerStateOff)

	require.NotNil(t, in.Claim, "the live claim of the slot is the first input of the report")
	assert.True(t, in.StartupJobInFlight, "the observation path fills the lifecycle-Job fields, through the one helper the transition path calls")
	assert.False(t, in.ShutdownJobInFlight, "and answers the direction it did not find, not the one it did not ask")

	assert.Equal(t, infrav2alpha1.NodePhaseBooting, phase.Derive(in),
		"a pending claim, a machine observed off and a startup Job in flight is `booting` - the value of the same world, on both writers' paths")

	// The shape the field is the difference of: take the Job away and the same triple is `pending`,
	// the cancelable shape. Without the field filled in, this is the value the observation path
	// produced for a machine that was already booting - and the value the transition path overrode,
	// every pass, in the ring.
	noJob := newK8sNodeReconciler(t, nodeCR, k8sNode, nodeClaim)

	assert.Equal(t, infrav2alpha1.NodePhasePending, phase.Derive(noJob.observationInput(context.Background(), nodeCR, k8sNode, infrav2alpha1.PowerStateOff)))
}

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
		slot("omni-remote-7", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
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
		slot("omni-remote-8", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
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
		slot("omni-remote-9", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		slotClaim("omni-remote-9", infrav2alpha1.NodeClaimReady),
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

// statusConflictClient is the fake the two write-path tests need: it answers every status write with
// a Conflict (a concurrent writer that never loses), and the second one stamps a condition on the
// slot the retry loop reads back, the shape of a writer that recorded the same observation first.
type statusConflictClient struct {
	client.Client

	statusWrites int
	// stampOnReread is applied to the Node CR handed back by the second Get, which is the read
	// inside the retry loop.
	stampOnReread func(*infrav2alpha1.Node)
	gets          int
}

func (c *statusConflictClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}

	if slot, isSlot := obj.(*infrav2alpha1.Node); isSlot {
		c.gets++

		if c.stampOnReread != nil && c.gets >= 2 {
			c.stampOnReread(slot)
		}
	}

	return nil
}

func (c *statusConflictClient) Status() client.SubResourceWriter {
	return &conflictStatusWriter{client: c, owner: c}
}

type conflictStatusWriter struct {
	client client.Client
	owner  *statusConflictClient
}

func (w *conflictStatusWriter) Create(_ context.Context, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
	return fmt.Errorf("unexpected Create")
}

func (w *conflictStatusWriter) Update(_ context.Context, _ client.Object, _ ...client.SubResourceUpdateOption) error {
	w.owner.statusWrites++

	return apierrors.NewConflict(
		schema.GroupResource{Group: infrav2alpha1.GroupVersion.Group, Resource: "nodes"},
		"omni-remote-12", fmt.Errorf("the slot moved under the write"))
}

func (w *conflictStatusWriter) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
	return fmt.Errorf("unexpected Patch")
}

func (w *conflictStatusWriter) Apply(_ context.Context, _ runtime.ApplyConfiguration, _ ...client.SubResourceApplyOption) error {
	return fmt.Errorf("unexpected Apply")
}

// TestK8sNodeReconcilerEmitsNothingWhenTheWriteDoesNotLand is the half of the marker recording that
// keeps it honest: an Event is the claim "this is recorded on the slot", and publishing one for a
// status update the API server refused is a record of something that is not in the cluster. The write
// failing is also the case where the log line must stay silent: "Successfully updated Node status" is
// the line an operator dates a power-state change from, and a per-reconcile copy of a write that never
// landed is a log that says nothing.
func TestK8sNodeReconcilerEmitsNothingWhenTheWriteDoesNotLand(t *testing.T) {
	recorder := events.NewFakeRecorder(16)

	slotCR := slot("omni-remote-12", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	node := markNode(managedNode("omni-remote-12", true), true, poolRoleTaint, hardMarker)

	base := newK8sNodeReconciler(t, slotCR, node)
	c := &statusConflictClient{Client: base.Client}
	base.Client = c
	base.EventRecorder = recorder

	logs := &bytes.Buffer{}
	ctx := log.IntoContext(context.Background(), zap.New(zap.WriteTo(logs), zap.UseDevMode(true)))

	_, err := base.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "omni-remote-12"}})

	require.Error(t, err, "a write the API server refused is reported, not swallowed")
	assert.Greater(t, c.statusWrites, 0, "the retry loop tried, as it is designed to")

	assert.Zero(t, len(drainK8sEvents(recorder)),
		"and no marker event is published for a status update that never landed")

	contents := logs.String()
	assert.NotContains(t, contents, "Successfully updated Node status",
		"nothing was updated, so the audit line stays out of the log")
	assert.Contains(t, contents, "Failed to update Node status")

	// The slot carries no record of a marker the controller never managed to write.
	after := slotFrom(t, c, "omni-remote-12")
	assert.Nil(t, meta.FindStatusCondition(after.Status.Conditions, autoscalerpkg.ConditionMarkedForDeletion))
}

// TestK8sNodeReconcilerSilentWhenAnotherWriterGotThereFirst is the other shape of a write that does
// not land: the retry loop re-reads the slot, finds the observation it was about to record already
// recorded - by the state machine, by a resync of this same controller - and returns with nothing to
// write. No status write, no event, and no "Successfully updated" line claiming an update.
func TestK8sNodeReconcilerSilentWhenAnotherWriterGotThereFirst(t *testing.T) {
	recorder := events.NewFakeRecorder(16)

	slotCR := slot("omni-remote-13", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	// Not cordoned: the slot's live claim is ready, its machine is Ready, so the observation this
	// pass would record is the one the concurrent writer already stamped, and the loop must go quiet.
	node := markNode(managedNode("omni-remote-13", true), false, poolRoleTaint, hardMarker)

	base := newK8sNodeReconciler(t, slotCR, slotClaim("omni-remote-13", infrav2alpha1.NodeClaimReady), node)
	c := &statusConflictClient{
		Client: base.Client,
		stampOnReread: func(fresh *infrav2alpha1.Node) {
			meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
				Type:               autoscalerpkg.ConditionMarkedForDeletion,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             autoscalerpkg.ReasonDeletionRequested,
				Message:            "recorded by a concurrent pass",
			})
		},
	}
	base.Client = c
	base.EventRecorder = recorder

	logs := &bytes.Buffer{}
	ctx := log.IntoContext(context.Background(), zap.New(zap.WriteTo(logs), zap.UseDevMode(true)))

	_, err := base.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "omni-remote-13"}})
	require.NoError(t, err, "someone else recorded what we observed: that is success, not a failure")
	assert.Zero(t, c.statusWrites, "so the loop wrote nothing")

	assert.Zero(t, len(drainK8sEvents(recorder)), "and the event that would have announced it is not published")
	assert.NotContains(t, logs.String(), "Successfully updated Node status",
		"the audit line is only for a write that landed")
}
