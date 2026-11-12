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

// Package autoscaler records, on the Node CR, what cluster-autoscaler decided about the slot's
// machine.
//
// The two markers are cluster-autoscaler's own: DeletionCandidateOfClusterAutoscaler says a node
// is a scale-down candidate (the soft marker, written while it is evaluated), and
// ToBeDeletedByClusterAutoscaler says a delete was requested and the node is being drained (the
// hard marker). The operator that watches a pool lose a machine wants to know who asked for it.
// The live shape this closes is a cordon that could not be attributed at all: a node was
// cordoned about a minute after it joined, nothing in the operator's window carried a CA
// "Scale-down: removing node" line, and the operator had to raise CA's verbosity to find out what
// the autoscaler had decided.
//
// The controller side has the same evidence for free, because the markers are on the node object
// this controller already watches. So a marker is recorded as an Event on the Node CR - the place
// `kubectl describe node.infra.homecluster.dev <name>` reads - and the node's taint list and
// spec.unschedulable are logged with it, which is enough to attribute a cordon without touching
// cluster-autoscaler's verbosity.
//
// It is recorded once per transition, never per reconcile: the last marker observed is kept as the
// status.conditions entry of the slot (ConditionMarkedForDeletion), and a pass that sees the
// marker it already recorded writes nothing and records nothing. The slot's own power intent is
// untouched: cluster-autoscaler asks for a scale-down by calling the gRPC provider, which is what
// writes spec.desiredPowerState, and this package only reports what the node says.
//
// The package also carries the reader of the Group's readiness taint list (spec.nodeReadinessTaints
// - the taints the scale-up gate waits for, see ReadinessTaintKeys) and the condition that records
// it on the slot. Those keys are the CNI's, and the list is configuration: the one place that reads
// cluster-autoscaler's node keys is the right place to read the gate's, and a slot stuck booting has
// to say on itself what it is waiting for.
package autoscaler

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// The cluster-autoscaler node markers this controller reports. They are cluster-autoscaler's keys
// (it owns them, which is why taints.IsProtectedKey refuses to let spec.nodeTaints write them);
// this package never adds, changes, or removes one.
const (
	// TaintDeletionCandidate is the soft marker: the autoscaler considers the node a scale-down
	// candidate and is evaluating it.
	TaintDeletionCandidate = "DeletionCandidateOfClusterAutoscaler"
	// TaintDeletionRequested is the hard marker: the delete was requested and the node is being
	// drained for removal.
	TaintDeletionRequested = "ToBeDeletedByClusterAutoscaler"
)

// Reason, action and condition type used to record a marker on the Node CR.
const (
	// ReasonDeletionCandidate is recorded for the soft marker.
	ReasonDeletionCandidate = "AutoscalerDeletionCandidate"
	// ReasonDeletionRequested is recorded for the hard marker.
	ReasonDeletionRequested = "AutoscalerDeletionRequested"
	// ReasonNotMarked records that the marker is gone again, which is what makes the next
	// appearance a transition and not a repeat.
	ReasonNotMarked = "AutoscalerNotMarked"
	// ReasonReadinessTaints is the reason the readiness-taint condition carries, so the condition can
	// be told apart from a stale record of an older Group: only a condition with this reason is read
	// back as a key list.
	ReasonReadinessTaints = "NodeReadinessTaints"
	// ActionAutoscalerDecision is the action the recorded events belong to.
	ActionAutoscalerDecision = "AutoscalerDecision"
	// ConditionMarkedForDeletion is the condition that carries the last marker this controller
	// observed. Its Reason is the stored value the deduplication compares against, so the record
	// lives on the slot, survives a manager restart, and is visible to the operator.
	ConditionMarkedForDeletion = "ClusterAutoscalerMarkedForDeletion"
)

// ConditionReadinessTaints is the status condition the node controller stamps on a slot with the
// taint keys its Group declares as the readiness gate (spec.nodeReadinessTaints). It is the
// fallback the controller uses for a slot whose Group cannot be read, and it is the operator's
// record of which keys the gate is holding the slot open on: the keys belong to the CNI, which is
// not verifiable from this repository, so they are configuration, and a slot that is stuck booting
// has to say what it is waiting for.
const ConditionReadinessTaints = "NodeReadinessTaints"

// ReadinessTaintKeys returns the taint keys the scale-up readiness gate waits for the slot's node to
// be free of, read off the Group the slot belongs to (spec.nodeReadinessTaints). The list is
// returned as declared, so an empty list - a CNI that stamps no bootstrap taint, the documented way
// to turn the gate off - means the gate is the Ready condition alone. The keys are deduplicated and
// blank entries dropped, so a Group that repeats a key does not make the gate log twice.
func ReadinessTaintKeys(group *infrav2alpha1.Group) []string {
	if group == nil {
		return nil
	}

	return taintKeys(group.Spec.NodeReadinessTaints)
}

// StampReadinessTaints records the gate's keys on the slot (ConditionReadinessTaints) and reports
// whether the slot's conditions changed, so the caller writes only when there is something to write.
// The record is what makes "the slot has been booting for 10 minutes" answerable from the slot: the
// keys belong to the Group and to the CNI, and a slot whose Group cannot be read is reconciled from
// this record (see ReadinessTaintKeysOfSlot).
func StampReadinessTaints(slot *infrav2alpha1.Node, keys []string) bool {
	message := renderKeys(keys)

	stored := meta.FindStatusCondition(slot.Status.Conditions, ConditionReadinessTaints)
	if stored != nil && stored.Message == message && stored.Reason == ReasonReadinessTaints {
		// Steady state: the Group says the same thing as the record, so a resync writes nothing.
		return false
	}

	meta.SetStatusCondition(&slot.Status.Conditions, metav1.Condition{
		Type:               ConditionReadinessTaints,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: slot.Generation,
		Reason:             ReasonReadinessTaints,
		Message:            message,
	})

	return true
}

// ReadinessTaintKeysOfSlot reads the keys back off the slot's own condition
// (ConditionReadinessTaints), whose message lists them comma-separated. It is the fallback for a
// pass that could not read its Group, so the gate keeps the budget the Group configured for the
// slot rather than silently becoming Ready-only for one pass.
func ReadinessTaintKeysOfSlot(slot *infrav2alpha1.Node) []string {
	if slot == nil {
		return nil
	}

	condition := meta.FindStatusCondition(slot.Status.Conditions, ConditionReadinessTaints)
	if condition == nil || condition.Reason != ReasonReadinessTaints {
		return nil
	}

	return splitKeys(condition.Message)
}

// taintKeys maps a taint list to its keys, in order, skipping blanks and repeats. The gate matches
// on the key, which is the identity of a taint on a node object (Kubernetes keeps one entry per
// key), so the value and the effect of the declaration are documentation for the operator.
func taintKeys(taints []corev1.Taint) []string {
	keys := make([]string, 0, len(taints))
	seen := map[string]bool{}

	for _, taint := range taints {
		key := strings.TrimSpace(taint.Key)
		if key == "" || seen[key] {
			continue
		}

		seen[key] = true

		keys = append(keys, key)
	}

	if len(keys) == 0 {
		// No keys, spelled as nothing, so an empty list and a list of blanks are the same answer.
		return nil
	}

	return keys
}

// noReadinessTaints is the message an off gate is recorded with. It is a constant because the
// deduplication of the record compares messages, and because splitKeys reads the same word back.
const noReadinessTaints = "none"

// renderKeys renders the keys for the condition's message, and says so explicitly when the list is
// empty: a slot whose gate is Ready-only has to read differently from a slot with no gate record at
// all.
func renderKeys(keys []string) string {
	if len(keys) == 0 {
		return noReadinessTaints
	}

	return strings.Join(keys, ",")
}

// splitKeys reads the keys back out of a condition message written by renderKeys.
func splitKeys(message string) []string {
	if message == "" || message == noReadinessTaints {
		return nil
	}

	parts := strings.Split(message, ",")

	keys := make([]string, 0, len(parts))

	for _, part := range parts {
		key := strings.TrimSpace(part)
		if key == "" {
			continue
		}

		keys = append(keys, key)
	}

	return keys
}

// Decision reports what one pass of RecordDecision did.
type Decision struct {
	// Recorded is true when the slot's status was written, which is exactly when a marker
	// transition was observed.
	Recorded bool
	// Marker is the key that is now recorded on the slot, empty when the marker is gone.
	Marker string
	// PreviousReason is the condition reason recorded before this pass, empty when nothing was
	// recorded: it is what makes the event say that a candidate became a delete request.
	PreviousReason string
}

// ApplyMarker records the marker observed on the slot's node into the slot's status, and reports
// what it decided and whether the object changed. It is the marker equivalent of
// core.applyObservedPowerState: a pure mutation of the copy the caller is about to write, so the
// observation and the power state land in one status update, and it never writes on its own.
//
// observedAt is the moment the node object was read, and it is a parameter because the answer to
// "when did the autoscaler mark my node" is the question this record exists for: the condition's
// LastTransitionTime is not that answer, and the message is (see markerMessage).
//
// The deduplication record is the condition itself: its Reason is the marker that was last
// observed, so a pass that sees the marker it already recorded reports no change and writes
// nothing. A slot with no marker and no condition is left untouched, so the first pass over a
// pool cluster-autoscaler never touched adds no status noise.
func ApplyMarker(slot *infrav2alpha1.Node, k8sNode *corev1.Node, observedAt time.Time) (Decision, bool) {
	marker := markerKey(k8sNode)
	reason, status := markerReason(marker)

	stored := meta.FindStatusCondition(slot.Status.Conditions, ConditionMarkedForDeletion)

	if stored == nil {
		if marker == "" {
			return Decision{}, false
		}
	} else if stored.Status == status && stored.Reason == reason {
		// The steady-state branch: this is what every pass over an unchanged node takes.
		return Decision{Marker: marker, PreviousReason: stored.Reason}, false
	}

	previousReason := ""
	if stored != nil {
		previousReason = stored.Reason
	}

	meta.SetStatusCondition(&slot.Status.Conditions, metav1.Condition{
		Type:   ConditionMarkedForDeletion,
		Status: status,
		// The time of the write, not the time of the observation: see markerMessage for why the
		// message carries the observation, and the test that pins it.
		LastTransitionTime: metav1.NewTime(observedAt),
		Reason:             reason,
		Message:            markerMessage(k8sNode, marker, observedAt),
	})

	return Decision{Recorded: true, Marker: marker, PreviousReason: previousReason}, true
}

// Emit publishes what ApplyMarker decided: the log line naming the marker, the node's full taint
// list and its spec.unschedulable, and the Event on the Node CR. Call it only after the status
// write succeeded, and only for a Decision whose Recorded is true: an event for a write that did
// not land is a record of something that is not in the cluster, and an event per reconcile is an
// event storm.
//
// A marker that disappeared is not an event: the condition records it, the autoscaler's own
// log has the unmarking, and the operator asking "who cordoned my node" is asking about the
// appearance.
func Emit(ctx context.Context, recorder events.EventRecorder, slot *infrav2alpha1.Node, k8sNode *corev1.Node, decision Decision) {
	logger := log.FromContext(ctx).WithValues("node", slot.Name, "kubernetesNode", k8sNode.Name)

	logger.Info("Recorded the cluster-autoscaler decision for the slot",
		"marker", markerLabel(decision.Marker),
		"previous", decision.PreviousReason,
		"taints", taintList(k8sNode),
		"unschedulable", k8sNode.Spec.Unschedulable)

	if decision.Marker == "" || recorder == nil {
		return
	}

	recorder.Eventf(slot, nil, corev1.EventTypeNormal, reasonFor(decision.Marker), ActionAutoscalerDecision,
		"%s", markerNote(slot, k8sNode, decision))
}

// markerKey returns the cluster-autoscaler marker a node carries, preferring the hard one: a
// node that is being drained is being deleted, whatever the evaluator still thinks of it.
func markerKey(k8sNode *corev1.Node) string {
	found := ""

	for _, taint := range k8sNode.Spec.Taints {
		switch taint.Key {
		case TaintDeletionRequested:
			return TaintDeletionRequested
		case TaintDeletionCandidate:
			found = TaintDeletionCandidate
		}
	}

	return found
}

// markerReason maps a marker to the condition reason and status that record it.
func markerReason(marker string) (string, metav1.ConditionStatus) {
	switch marker {
	case TaintDeletionCandidate:
		return ReasonDeletionCandidate, metav1.ConditionTrue
	case TaintDeletionRequested:
		return ReasonDeletionRequested, metav1.ConditionTrue
	default:
		return ReasonNotMarked, metav1.ConditionFalse
	}
}

// reasonFor maps a marker to the Event reason recorded on the Node CR.
func reasonFor(marker string) string {
	reason, _ := markerReason(marker)

	return reason
}

// markerMessage renders the condition's message: the evidence the decision was read from, in the
// place an operator looks at, and the time this controller observed it.
//
// The timestamp in the message is the record, the condition's LastTransitionTime is not, and that
// is `meta.SetStatusCondition`'s documented behaviour, not a bug to work around: it keeps the
// existing LastTransitionTime whenever the Status does not flip. Both deletion markers are recorded
// with Status True, so the transition an operator asks about - a candidate that became a delete
// request, "the node was cordoned at 14:03, what did the autoscaler decide before that" - keeps the
// candidate's time on the hard marker and is dated minutes to hours early by the field that reads
// like a timestamp. The message therefore carries the observation time of this pass, and it is the
// half of the record that is true.
func markerMessage(k8sNode *corev1.Node, marker string, observedAt time.Time) string {
	observed := "observed at " + observedAt.UTC().Format(time.RFC3339)

	if marker == "" {
		return fmt.Sprintf("no cluster-autoscaler deletion marker on node %s (taints=[%s], spec.unschedulable=%v, %s)",
			k8sNode.Name, taintList(k8sNode), k8sNode.Spec.Unschedulable, observed)
	}

	return fmt.Sprintf("node %s carries the cluster-autoscaler marker %s (taints=[%s], spec.unschedulable=%v, %s)",
		k8sNode.Name, marker, taintList(k8sNode), k8sNode.Spec.Unschedulable, observed)
}

// markerNote renders the Event's message. It names the node, the marker, and the schedulability
// the autoscaler is enforcing with it, because the cordon that goes with a scale-down is the half
// of the story an operator is missing when a node stops taking pods.
func markerNote(slot *infrav2alpha1.Node, k8sNode *corev1.Node, decision Decision) string {
	note := fmt.Sprintf(
		"cluster-autoscaler marked node %s (slot %s) as %s: taint %s on the node, spec.unschedulable=%v, node taints=[%s]. "+
			"Recorded on the slot as condition %s; the scale-down request itself is spec.desiredPowerState, which this controller does not write.",
		k8sNode.Name, slot.Name, decision.Marker, decision.Marker, k8sNode.Spec.Unschedulable,
		taintList(k8sNode), ConditionMarkedForDeletion)

	if decision.PreviousReason != "" {
		note = fmt.Sprintf("%s (previous marker recorded: %s)", note, decision.PreviousReason)
	}

	return note
}

// markerLabel names a marker for the log, spelled out when there is none so a line is never
// printed with an empty field.
func markerLabel(marker string) string {
	if marker == "" {
		return "none"
	}

	return marker
}

// taintList renders the node's taints as key=value:effect pairs, the full list rather than only
// the marker: a cordon has to be attributable from one line, and the list is the evidence.
func taintList(k8sNode *corev1.Node) string {
	if len(k8sNode.Spec.Taints) == 0 {
		return ""
	}

	parts := make([]string, 0, len(k8sNode.Spec.Taints))

	for _, taint := range k8sNode.Spec.Taints {
		parts = append(parts, fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect))
	}

	return strings.Join(parts, ",")
}
