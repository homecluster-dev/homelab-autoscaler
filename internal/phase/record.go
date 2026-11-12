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

package phase

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// The bounds of the observability a slot keeps about its own report.
const (
	// TransitionRingCap is the length of Node.status.recentTransitions: the last N phase changes
	// of a slot, newest last. It is a cap and not a window: a slot that churns records more than
	// the cap and the oldest rows fall off, so a day of a slot is bounded in etcd by this number
	// and in time by TransitionWindow, whichever bites first.
	TransitionRingCap = 32

	// TransitionWindow is how far back the ring reaches: 24 hours, the horizon of "did this pool
	// thrash yesterday". An entry older than the window is pruned on the next write, and its
	// removal is what decrements activationsLast24H.
	TransitionWindow = 24 * time.Hour

	// ReasonMaxLength bounds NodeTransition.reason, and matches the +kubebuilder:validation:
	// MaxLength of the field: a reason is composed from object names, so the code clips it rather
	// than trusting the names to be short. A write the API server refused for a long reason would
	// cost the whole report.
	ReasonMaxLength = 320
)

// Record is the single writer of the observability that rides on Node.status.phase:
// lastTransitionTime, previousPhase, recentTransitions, activationsTotal, activationsLast24H.
//
// It is called by the places that already own a phase write and by nobody else:
//
//   - the observation path (`applyObservation` in internal/controller/core/node_controller.go),
//     which is the pass that sees the machine;
//   - the lifecycle transition path (`stampDerivedPhase` in internal/fsm/claims.go), which is the
//     pass that bought the machine;
//   - the seeding of a never-provisioned slot (internal/controller/core/node_slot_controller.go),
//     the first report of a freshly committed pool;
//   - the expiry pass of the slot reconciler (`refreshTransitionWindow` in
//     internal/controller/infra/lifecycle.go), which calls it with the phase the slot already
//     reports so the window is pruned on time - see NextTransitionExpiry.
//
// That is the rule the model kept from v1alpha1: the value of status.phase has one formula
// (Derive), and now the record of its changes has one writer, so the phase and its history cannot
// be told apart by which controller got there first. `at` is a parameter for the same reason
// Derive is a function: the tests inject the clock, and production passes time.Now().
//
// Record returns true when the object changed, so the caller writes nothing on a pass that records
// nothing. A change is:
//
//   - the phase changing value: lastTransitionTime, previousPhase, one ring row, and (for a boot)
//     activationsTotal;
//   - the window moving: an entry that left it, which changes recentTransitions and
//     activationsLast24H with no transition to record. That is the whole reason Record prunes on
//     every call and the slot reconciler requeues at the expiry instant: a count of the last 24 h
//     that is only recomputed on a transition is a number that goes stale while the pool sleeps,
//     and the question "how often does this machine boot in a day" is asked a day later.
func Record(node *infrav2alpha1.Node, derived infrav2alpha1.NodePhase, at time.Time, reason string) bool {
	from := node.Status.Phase
	moved := from != derived

	if moved {
		stamp := metav1.NewTime(at)

		node.Status.PreviousPhase = from
		node.Status.Phase = derived
		node.Status.LastTransitionTime = &stamp
		node.Status.RecentTransitions = append(node.Status.RecentTransitions, infrav2alpha1.NodeTransition{
			At:     &stamp,
			From:   from,
			To:     derived,
			Reason: clip(reason, ReasonMaxLength),
		})

		if IsActivation(from, derived) {
			node.Status.ActivationsTotal++
		}
	}

	// Both halves are evaluated: the window moves on the same call that records a transition, and
	// the answer is "did anything about the record change".
	pruned := pruneWindow(node, at)

	return moved || pruned
}

// IsActivation reports whether a phase change is a machine being turned on.
//
// The rule is the report's, not the transition's: entering `booting` is the shape the slot is in
// while a machine is being brought up, and phase.Derive only reports it for a machine that is not
// up and is wanted (from `off` / `pending` for a fresh claim, from `ready` for a live claim whose
// machine vanished, from `draining` for a scale-down that was cancelled). Counting the entries
// into `booting` - and an entry is recorded only on a change of value, never on a resync - is
// therefore one count per boot, and it is the same event the ring row names.
func IsActivation(from, to infrav2alpha1.NodePhase) bool {
	return to == infrav2alpha1.NodePhaseBooting && from != infrav2alpha1.NodePhaseBooting
}

// NextTransitionExpiry reports the instant the oldest entry of the ring leaves the window, and
// whether the slot has any entry to expire at all.
//
// It is the honest half of activationsLast24H: the count changes when an entry ages out, and
// nothing about the pool triggers a reconcile at that instant (a slot that is `off` has no node
// object, no claim, no Job, so no event). The slot reconciler folds it into its own
// `RequeueAfter`, so the pass that prunes runs at the moment the number changes, and not one
// minute later. An entry that is already outside the window reports "now", which is a pass to run.
func NextTransitionExpiry(node *infrav2alpha1.Node, now time.Time) (time.Time, bool) {
	var earliest time.Time

	for i := range node.Status.RecentTransitions {
		at := node.Status.RecentTransitions[i].At
		if at == nil {
			// A row with no time cannot age out; Record always stamps one, so this is a
			// hand-written object, and the next write prunes it.
			continue
		}

		expiry := at.Add(TransitionWindow)
		if !expiry.After(now) {
			// Already outside the window: the very next pass is the one that prunes it.
			return now, true
		}

		if earliest.IsZero() || expiry.Before(earliest) {
			earliest = expiry
		}
	}

	if earliest.IsZero() {
		return time.Time{}, false
	}

	return earliest, true
}

// pruneWindow drops the rows that left the window and re-materializes the count from what is
// left. It returns true when it changed anything.
//
// The count is never its own record of a fact: activationsLast24H is recomputed from the ring on
// every write, so the two cannot disagree, and the pruning is the only place the window moves.
// Rows with no timestamp are kept (a hand-written row is not this writer's to judge), and the cap
// is applied last, keeping the newest rows.
func pruneWindow(node *infrav2alpha1.Node, at time.Time) bool {
	changed := false

	ring := node.Status.RecentTransitions
	if len(ring) == 0 {
		if node.Status.ActivationsLast24H != 0 {
			node.Status.ActivationsLast24H = 0

			return true
		}

		return false
	}

	cutoff := at.Add(-TransitionWindow)
	kept := make([]infrav2alpha1.NodeTransition, 0, len(ring))

	for _, entry := range ring {
		if entry.At != nil && entry.At.Time.Before(cutoff) {
			changed = true

			continue
		}

		kept = append(kept, entry)
	}

	if len(kept) > TransitionRingCap {
		kept = kept[len(kept)-TransitionRingCap:]

		changed = true
	}

	count := activationsInWindow(kept)
	if node.Status.ActivationsLast24H != count {
		node.Status.ActivationsLast24H = count

		changed = true
	}

	if len(kept) != len(ring) {
		node.Status.RecentTransitions = kept

		changed = true
	}

	return changed
}

// activationsInWindow counts the boots the rows record: every entry into `booting`, which is the
// row IsActivation names, counted on the pruned ring, which is the window.
func activationsInWindow(ring []infrav2alpha1.NodeTransition) int32 {
	var count int32

	for _, entry := range ring {
		if entry.To == infrav2alpha1.NodePhaseBooting {
			count++
		}
	}

	return count
}

// Reason turns the triple a report was derived from into the one line NodeTransition.reason
// carries: the live claim, the observed power, the usability of the node object. It is composed
// from the same Input Derive consumed, so a reason is a restatement of the inputs of the value and
// cannot drift from it, and it names the claim, because the claim is the object an operator opens
// after this one.
//
// It is display text and nothing reads it: no decision in this repository parses a reason.
func Reason(in Input, derived infrav2alpha1.NodePhase) string {
	parts := make([]string, 0, 4)

	claim := in.LiveClaim()

	switch {
	case claim == nil:
		parts = append(parts, "no live claim")
	case !claim.DeletionTimestamp.IsZero():
		parts = append(parts, fmt.Sprintf("claim %s marked for scale-down", claim.Name))
	default:
		phase := claim.Status.Phase
		if phase == "" {
			phase = infrav2alpha1.NodeClaimPending
		}

		parts = append(parts, fmt.Sprintf("claim %s %s", claim.Name, phase))
	}

	switch in.ObservedPower {
	case infrav2alpha1.PowerStateOn:
		parts = append(parts, "observed on")
	case infrav2alpha1.PowerStateOff:
		parts = append(parts, "observed off")
	default:
		parts = append(parts, "never observed")
	}

	if in.ShutdownJobInFlight {
		parts = append(parts, "shutdown job in flight")
	}

	if in.StartupJobInFlight {
		parts = append(parts, "startup job in flight")
	}

	if derived == infrav2alpha1.NodePhaseBooting && in.ObservedOn() && !in.NodeUsable {
		parts = append(parts, "node not usable")
	}

	if derived == infrav2alpha1.NodePhaseReady {
		parts = append(parts, "node usable")
	}

	return strings.Join(parts, ", ")
}

// clip bounds a composed string to the CRD's MaxLength, so a long object name cannot make a status
// write the API server refuses. The bound is in runes, which is what the CRD's maxLength counts, and
// the ellipsis is ASCII, so the result is valid UTF-8 and exactly `max` long.
func clip(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}

	return string(runes[:max-3]) + "..."
}
