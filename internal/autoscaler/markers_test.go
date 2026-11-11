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

package autoscaler

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// observedAt is the clock the marker tests hand to ApplyMarker. The observation time is a parameter
// precisely so the record can be asserted against a fixed instant instead of against a clock the
// test cannot control (see TestApplyMarkerTimestampsTheObservationItMade).
var observedAt = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// slotCR is the Node CR of a running machine of a pool.
func slotCR() *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "homelab-autoscaler-system"},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  infrav1alpha1.PowerStateOn,
			KubernetesNodeName: "worker-1",
		},
		Status: infrav1alpha1.NodeStatus{
			PowerState: infrav1alpha1.PowerStateOn,
			Progress:   infrav1alpha1.ProgressReady,
		},
	}
}

// nodeOf is the corev1.Node of that slot, carrying the given taints, cordoned when asked.
func nodeOf(cordoned bool, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec: corev1.NodeSpec{
			Unschedulable: cordoned,
			Taints:        taints,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// candidate is cluster-autoscaler's soft marker, requested the hard one, role the pool's own.
var (
	candidate = corev1.Taint{Key: TaintDeletionCandidate, Effect: corev1.TaintEffectPreferNoSchedule}
	requested = corev1.Taint{Key: TaintDeletionRequested, Effect: corev1.TaintEffectNoSchedule}
	role      = corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}
)

// recordedCondition reads the marker condition back off the slot.
func recordedCondition(t *testing.T, slot *infrav1alpha1.Node) *metav1.Condition {
	t.Helper()

	condition := meta.FindStatusCondition(slot.Status.Conditions, ConditionMarkedForDeletion)

	return condition
}

// TestApplyMarkerRecordsEachTransitionOnce is the shape of the live question - "who cordoned this
// node, and when" - answered without an event storm. The marker is recorded on the slot, once per
// transition: the pass that sees the marker the slot already carries reports no change, so the
// controller writes nothing and records nothing on every resync.
func TestApplyMarkerRecordsEachTransitionOnce(t *testing.T) {
	slot := slotCR()

	// The marker appears: a candidate.
	decision, changed := ApplyMarker(slot, nodeOf(false, role, candidate), observedAt)
	require.True(t, changed, "a marker that was never recorded is a transition")
	require.True(t, decision.Recorded)
	assert.Equal(t, TaintDeletionCandidate, decision.Marker)
	assert.Empty(t, decision.PreviousReason)

	condition := recordedCondition(t, slot)
	require.NotNil(t, condition, "the record lives on the slot")
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, ReasonDeletionCandidate, condition.Reason)
	assert.Contains(t, condition.Message, "worker-1")
	assert.Contains(t, condition.Message, TaintDeletionCandidate)
	assert.Contains(t, condition.Message, "spec.unschedulable=false")

	// Every later pass over the same node: no change, nothing to write, nothing to record.
	for range 3 {
		decision, changed := ApplyMarker(slot, nodeOf(false, role, candidate), observedAt)
		assert.False(t, changed, "the marker is the one already recorded")
		assert.False(t, decision.Recorded)
		assert.Equal(t, ReasonDeletionCandidate, decision.PreviousReason)
	}

	// The candidate becomes a delete request: the hard marker, and a cordon.
	decision, changed = ApplyMarker(slot, nodeOf(true, role, requested), observedAt)
	require.True(t, changed)
	require.True(t, decision.Recorded)
	assert.Equal(t, ReasonDeletionCandidate, decision.PreviousReason, "the event says what it replaced")

	condition = recordedCondition(t, slot)
	assert.Equal(t, ReasonDeletionRequested, condition.Reason)
	assert.Contains(t, condition.Message, "spec.unschedulable=true", "the cordon is part of the record")

	// The marker is gone: recorded, no event (Emit decides that), and the slot is ready to
	// report the next appearance as a new transition.
	decision, changed = ApplyMarker(slot, nodeOf(false, role), observedAt)
	require.True(t, changed)
	require.True(t, decision.Recorded)
	assert.Empty(t, decision.Marker)

	condition = recordedCondition(t, slot)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, ReasonNotMarked, condition.Reason)

	decision, changed = ApplyMarker(slot, nodeOf(false, role), observedAt)
	assert.False(t, changed, "an unmarked slot that is recorded as unmarked writes nothing")

	// It comes back, and that is a new transition with a new event.
	decision, changed = ApplyMarker(slot, nodeOf(true, requested), observedAt)
	require.True(t, changed)
	require.True(t, decision.Recorded)
	assert.Equal(t, ReasonNotMarked, decision.PreviousReason)
}

// TestApplyMarkerLeavesAHealthySlotUntouched keeps the first pass over a clean pool quiet: a node
// cluster-autoscaler never marked and a slot with no marker condition get no status write, so no
// Node CR of every pool gains a condition, and the controller stays out of the hot path.
func TestApplyMarkerLeavesAHealthySlotUntouched(t *testing.T) {
	slot := slotCR()

	decision, changed := ApplyMarker(slot, nodeOf(false, role), observedAt)

	assert.False(t, changed)
	assert.False(t, decision.Recorded)
	assert.Empty(t, slot.Status.Conditions, "nothing observed, nothing recorded")
}

// TestMarkerKeyPrefersTheHardMarker: a node that is being drained is being deleted, whatever the
// evaluator still thinks of it.
func TestMarkerKeyPrefersTheHardMarker(t *testing.T) {
	assert.Equal(t, TaintDeletionRequested, markerKey(nodeOf(true, candidate, requested)))
	assert.Equal(t, TaintDeletionCandidate, markerKey(nodeOf(false, role, candidate)))
	assert.Equal(t, "", markerKey(nodeOf(false, role)))
	assert.Equal(t, "", markerKey(nodeOf(true)), "a cordon alone is not an autoscaler marker")
}

// TestEmitRecordsOneNormalEventNamingTheNode is the operator-facing half: the Event on the Node CR
// names the node, the marker, the cordon, and the evidence, and the log line carries the whole
// taint list, which is what makes a cordon attributable without raising cluster-autoscaler's
// verbosity.
func TestEmitRecordsOneNormalEventNamingTheNode(t *testing.T) {
	recorder := events.NewFakeRecorder(8)
	slot := slotCR()
	k8sNode := nodeOf(true, role, requested)

	decision, changed := ApplyMarker(slot, k8sNode, observedAt)
	require.True(t, changed)

	Emit(context.Background(), recorder, slot, k8sNode, decision)

	require.Len(t, recorder.Events, 1, "one transition, one event")

	event := <-recorder.Events
	assert.Contains(t, event, "Normal", "the autoscaler marking a node is its decision, not an incident")
	assert.Contains(t, event, ReasonDeletionRequested)
	assert.Contains(t, event, "worker-1", "the event names the node, not only the slot")
	assert.Contains(t, event, TaintDeletionRequested)
	assert.Contains(t, event, ConditionMarkedForDeletion)
}

// TestEmitRecordsNoEventWhenTheMarkerDisappears: the unmarking is the condition, not an event. The
// operator asking who cordoned a node is asking about the appearance.
func TestEmitRecordsNoEventWhenTheMarkerDisappears(t *testing.T) {
	recorder := events.NewFakeRecorder(8)
	slot := slotCR()

	ApplyMarker(slot, nodeOf(true, requested), observedAt)
	drainEvents(recorder)

	decision, changed := ApplyMarker(slot, nodeOf(false), observedAt)
	require.True(t, changed)

	Emit(context.Background(), recorder, slot, nodeOf(false), decision)
	assert.Zero(t, len(drainEvents(recorder)), "a marker that went away records no event")

	// A nil recorder is a supported shape, and the log line is still the audit trail.
	logs := &bytes.Buffer{}
	Emit(withTestLogger(context.Background(), logs), nil, slot, nodeOf(false), decision)
	assert.Contains(t, logs.String(), "Recorded the cluster-autoscaler decision for the slot")
	assert.Contains(t, logs.String(), "marker", "none")
}

// TestEmitLogsTheTaintsThatExplainACordon: the taint list and spec.unschedulable of the node go to
// the log with the marker, which is the record that closes "the node was cordoned and nothing
// said so".
func TestEmitLogsTheTaintsThatExplainACordon(t *testing.T) {
	slot := slotCR()
	k8sNode := nodeOf(true, role, requested)

	decision, _ := ApplyMarker(slot, k8sNode, observedAt)

	logs := &bytes.Buffer{}
	Emit(withTestLogger(context.Background(), logs), nil, slot, k8sNode, decision)

	contents := logs.String()
	assert.Contains(t, contents, TaintDeletionRequested)
	assert.Contains(t, contents, "node.kubernetes.io/role=agent:NoSchedule", "the full taint list, not only the marker")
	assert.Contains(t, contents, "unschedulable")
}

// withTestLogger puts a logger that writes to the buffer in the context, which is what the
// package's log.FromContext reads.
func withTestLogger(ctx context.Context, buffer *bytes.Buffer) context.Context {
	return log.IntoContext(ctx, zap.New(zap.WriteTo(buffer), zap.UseDevMode(true)))
}

// drainEvents empties the fake recorder's channel and returns everything it buffered.
func drainEvents(recorder *events.FakeRecorder) []string {
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

// TestApplyMarkerTimestampsTheObservationItMade is the M4 fix, and it pins a documented behaviour of
// meta.SetStatusCondition rather than fighting it: the supplied LastTransitionTime is only taken
// when the Status flips. Both deletion markers are recorded with Status True, so
// True/DeletionCandidate -> True/DeletionRequested keeps the *candidate's* time on a condition whose
// Reason is now the delete request - and LastTransitionTime is the only timestamp the condition has,
// so "the autoscaler marked my node at 13:58" is a sentence the field tells you to write, and it is
// minutes early by construction, on exactly the transition an operator asks about.
//
// The record that is true is the message, which carries the instant this pass read the node.
func TestApplyMarkerTimestampsTheObservationItMade(t *testing.T) {
	slot := slotCR()

	candidateSeen := time.Date(2026, 11, 11, 13, 40, 0, 0, time.UTC)
	requestedSeen := time.Date(2026, 11, 11, 14, 3, 0, 0, time.UTC)

	_, changed := ApplyMarker(slot, nodeOf(false, role, candidate), candidateSeen)
	require.True(t, changed)

	condition := recordedCondition(t, slot)
	require.NotNil(t, condition)
	assert.Equal(t, ReasonDeletionCandidate, condition.Reason)
	assert.True(t, condition.LastTransitionTime.Time.Equal(candidateSeen), "the first record is the first observation")
	assert.Contains(t, condition.Message, "observed at 2026-11-11T13:40:00Z")

	// The candidate becomes a delete request, 23 minutes later.
	decision, changed := ApplyMarker(slot, nodeOf(true, role, requested), requestedSeen)
	require.True(t, changed, "a new marker is a transition")
	assert.Equal(t, ReasonDeletionCandidate, decision.PreviousReason)

	after := recordedCondition(t, slot)
	assert.Equal(t, ReasonDeletionRequested, after.Reason, "the reason is the marker that is on the node now")
	assert.Equal(t, candidateSeen, after.LastTransitionTime.Time,
		"the condition's timestamp is the candidate's, and the test pins that: SetStatusCondition keeps it while Status stays True")

	assert.Contains(t, after.Message, "observed at 2026-11-11T14:03:00Z",
		"the message is the record that tells the operator when this controller saw the marker")
	assert.NotContains(t, after.Message, "13:40", "and it never carries an older pass's time")

	// An unmarking flips the Status, so the timestamp moves; the observation stays the true record.
	decision, changed = ApplyMarker(slot, nodeOf(false, role), requestedSeen.Add(90*time.Second))
	require.True(t, changed)

	cleared := recordedCondition(t, slot)
	assert.Equal(t, metav1.ConditionFalse, cleared.Status)
	assert.Equal(t, ReasonNotMarked, cleared.Reason)
	assert.True(t, cleared.LastTransitionTime.Time.Equal(requestedSeen.Add(90*time.Second)))
	assert.Contains(t, cleared.Message, "observed at 2026-11-11T14:04:30Z")
}

// TestApplyMarkerIgnoresTaintsThatAreNotMarkers: the record is cluster-autoscaler's decision, not the
// node's taint weather. A kubelet taint arriving, the pool's own taint changing, a cordon - none of
// them is a marker, so a pass that sees only those writes nothing and records nothing, on a slot that
// has already recorded the absence of a marker (the shape that is exposed: a condition whose message
// quotes the taint list and spec.unschedulable would otherwise be "stale" every time the node churns).
func TestApplyMarkerIgnoresTaintsThatAreNotMarkers(t *testing.T) {
	recorder := events.NewFakeRecorder(8)
	slot := slotCR()

	// Record the absence of a marker once, so the slot has the condition the deduplication reads.
	ApplyMarker(slot, nodeOf(true, requested), observedAt)
	decision, changed := ApplyMarker(slot, nodeOf(false, role), observedAt.Add(time.Minute))
	require.True(t, changed, "the marker disappeared, that is a transition")
	assert.Equal(t, ReasonDeletionRequested, decision.PreviousReason, "the pass reports what it replaced")

	before := slot.DeepCopy()
	drainEvents(recorder)

	// An unrelated taint change: the pool's role taint is replaced by the kubelet's, and the node is
	// cordoned.
	for _, node := range []*corev1.Node{
		nodeOf(false, corev1.Taint{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute}),
		nodeOf(true, poolCordon()),
		nodeOf(true),
	} {
		decision, changed = ApplyMarker(slot, node, observedAt.Add(10*time.Minute))
		assert.False(t, changed, "node %q is not a cluster-autoscaler marker", taintList(node))
		assert.False(t, decision.Recorded)
	}

	assert.Equal(t, before.Status.Conditions, slot.Status.Conditions, "the condition is untouched, so the caller writes nothing")
	assert.Zero(t, len(drainEvents(recorder)), "and records nothing")
}

// poolCordon is a node cordoned by hand, with the pool's own taint: unschedulable, and no marker.
func poolCordon() corev1.Taint {
	return corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}
}

// TestReadinessTaintKeysIsTheGroupsListAndNothingElse: the key the scale-up gate waits for belongs to
// the CNI of the cluster, so it is read from the Group, as declared, in order, deduplicated, blanks
// dropped. An empty list is the gate off (Ready only) and is returned as empty, not as a default:
// turning the gate off has to be expressible.
func TestReadinessTaintKeysIsTheGroupsListAndNothingElse(t *testing.T) {
	group := func(taints ...corev1.Taint) *infrav1alpha1.Group {
		return &infrav1alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: "remote-workers"},
			Spec:       infrav1alpha1.GroupSpec{NodeReadinessTaints: taints},
		}
	}

	assert.Nil(t, ReadinessTaintKeys(nil), "no Group, no keys: the caller decides the fallback")
	assert.Empty(t, ReadinessTaintKeys(group()), "an empty list is the gate off, not a default")

	assert.Equal(t, []string{ciliumTaintKey},
		ReadinessTaintKeys(group(ciliumBootstrap())), "the declared key, in the declared shape")

	assert.Equal(t, []string{ciliumTaintKey, "node.kubelet.io/startup"},
		ReadinessTaintKeys(group(
			ciliumBootstrap(),
			corev1.Taint{Key: " node.kubelet.io/startup ", Effect: corev1.TaintEffectNoSchedule},
			ciliumBootstrap(),
			corev1.Taint{Key: ""},
		)), "repeats collapse, blanks are dropped, and surrounding space is not part of a key")
}

// TestStampReadinessTaintsRecordsTheGateOnTheSlotOnce is the record's cost: a Group write on the first
// pass that needs it, and nothing on a resync. A condition stamped on every reconcile would make this
// controller hot on a pool at rest, which is the rule every status writer here follows.
func TestStampReadinessTaintsRecordsTheGateOnTheSlotOnce(t *testing.T) {
	slot := slotCR()

	require.True(t, StampReadinessTaints(slot, []string{ciliumTaintKey}), "the first record changes the slot")

	condition := meta.FindStatusCondition(slot.Status.Conditions, ConditionReadinessTaints)
	require.NotNil(t, condition)
	assert.Equal(t, ReasonReadinessTaints, condition.Reason)
	assert.Equal(t, ciliumTaintKey, condition.Message, "the message is the key list, so it can be read back")

	for range 3 {
		assert.False(t, StampReadinessTaints(slot, []string{ciliumTaintKey}), "a resync of the same Group writes nothing")
	}

	assert.Equal(t, []string{ciliumTaintKey}, ReadinessTaintKeysOfSlot(slot), "a slot whose Group cannot be read is reconciled from its own record")

	require.True(t, StampReadinessTaints(slot, nil), "the gate turned off is a change, and it is recorded")
	assert.Equal(t, "none", recordedReadiness(slot), "and it reads differently from a slot with no record at all")
	assert.Empty(t, ReadinessTaintKeysOfSlot(slot), "an off gate reads back as no keys")

	// A record this package did not write is not a key list.
	other := slotCR()
	meta.SetStatusCondition(&other.Status.Conditions, metav1.Condition{
		Type:               ConditionReadinessTaints,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "SomethingElse",
		Message:            ciliumTaintKey,
	})
	assert.Empty(t, ReadinessTaintKeysOfSlot(other), "only our own reason is read back as keys")
	assert.Empty(t, ReadinessTaintKeysOfSlot(nil))
}

// recordedReadiness returns the readiness condition's message.
func recordedReadiness(slot *infrav1alpha1.Node) string {
	condition := meta.FindStatusCondition(slot.Status.Conditions, ConditionReadinessTaints)
	if condition == nil {
		return ""
	}

	return condition.Message
}

// ciliumTaintKey is Cilium's bootstrap taint, spelled out here because the key belongs to the CNI of
// the cluster and the autoscaler package reads it from the Group: it is configuration, not a constant
// of this package. It is the value the chart's Group CRs get by CRD default.
const ciliumTaintKey = "node.cilium.io/agent-not-ready"

// ciliumBootstrap is the taint the chart's Group CRs get by CRD default.
func ciliumBootstrap() corev1.Taint {
	return corev1.Taint{Key: ciliumTaintKey, Value: "true", Effect: corev1.TaintEffectNoExecute}
}
