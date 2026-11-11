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
	decision, changed := ApplyMarker(slot, nodeOf(false, role, candidate))
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
		decision, changed := ApplyMarker(slot, nodeOf(false, role, candidate))
		assert.False(t, changed, "the marker is the one already recorded")
		assert.False(t, decision.Recorded)
		assert.Equal(t, ReasonDeletionCandidate, decision.PreviousReason)
	}

	// The candidate becomes a delete request: the hard marker, and a cordon.
	decision, changed = ApplyMarker(slot, nodeOf(true, role, requested))
	require.True(t, changed)
	require.True(t, decision.Recorded)
	assert.Equal(t, ReasonDeletionCandidate, decision.PreviousReason, "the event says what it replaced")

	condition = recordedCondition(t, slot)
	assert.Equal(t, ReasonDeletionRequested, condition.Reason)
	assert.Contains(t, condition.Message, "spec.unschedulable=true", "the cordon is part of the record")

	// The marker is gone: recorded, no event (Emit decides that), and the slot is ready to
	// report the next appearance as a new transition.
	decision, changed = ApplyMarker(slot, nodeOf(false, role))
	require.True(t, changed)
	require.True(t, decision.Recorded)
	assert.Empty(t, decision.Marker)

	condition = recordedCondition(t, slot)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, ReasonNotMarked, condition.Reason)

	decision, changed = ApplyMarker(slot, nodeOf(false, role))
	assert.False(t, changed, "an unmarked slot that is recorded as unmarked writes nothing")

	// It comes back, and that is a new transition with a new event.
	decision, changed = ApplyMarker(slot, nodeOf(true, requested))
	require.True(t, changed)
	require.True(t, decision.Recorded)
	assert.Equal(t, ReasonNotMarked, decision.PreviousReason)
}

// TestApplyMarkerLeavesAHealthySlotUntouched keeps the first pass over a clean pool quiet: a node
// cluster-autoscaler never marked and a slot with no marker condition get no status write, so no
// Node CR of every pool gains a condition, and the controller stays out of the hot path.
func TestApplyMarkerLeavesAHealthySlotUntouched(t *testing.T) {
	slot := slotCR()

	decision, changed := ApplyMarker(slot, nodeOf(false, role))

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

	decision, changed := ApplyMarker(slot, k8sNode)
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

	ApplyMarker(slot, nodeOf(true, requested))
	drainEvents(recorder)

	decision, changed := ApplyMarker(slot, nodeOf(false))
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

	decision, _ := ApplyMarker(slot, k8sNode)

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
