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

package fsm

// The shutdown-attempt bound: the mirror of the startup bound (internal/fsm/startup_attempts.go), for
// the loop the shutdown-completion guard opens.
//
// A shutdown Job that exits 0 while the machine is observed up is refused (refuseUnobservedShutdown):
// the slot is recorded ready, not shutdown, and the claim that is already off makes the next pass fire
// ShutdownNode again. That is the correct response to a lie in the record, and unbounded it is a loop
// with a price: Job, machine still up, refused completion, correction, ShutdownNode, Job - and every
// pass of it re-cordons a node that is serving pods and spends a real power operation on a real
// machine. Before this PR the same broken script produced a *static* failure
// (a frozen slot, no Job at all), so the loop is a cost this fix introduces and has to bound.
//
// The bound is one counter, on the slot, in the same shape as the startup one, and for the same three
// reasons that shape was chosen there:
//
//   - it counts *attempts*, incremented where an attempt begins - enterShuttingDown, the
//     ready -> shuttingdown transition the lock, the cordon and the shutdown Job hang off, and the one
//     place every caller of ShutdownNode passes through (the autoscaler's scale-down, the shutdown an
//     observed-power correction releases, the retry a refused completion leads back into);
//   - it is cleared where an attempt demonstrably succeeded: a shutdown completion the observation
//     agreed with (enterShutdown records the shutdown), and the readiness gate of a boot
//     (afterJobCompleted) - so the bound prices one scale-down episode and not the life of a slot that
//     is reused across episodes;
//   - it is enforced in front of the path that spends the machine: fsm.NodeStateMachine.ShutdownNode
//     refuses the transition past the bound, before the lock, the cordon and the Job, and records
//     exactly one Warning naming the limit (the record is the deduplication).
//
// What the counter is not: a rate limiter, and not a way to give up on a claim. Past the bound the
// slot stays at the pair the observation says - status.progress ready, status.powerState on,
// spec.desiredPowerState off - which is a machine that is up, cordoned, unwanted, and reported. That
// is the documented hole of this fix: the pool stops hammering a machine it cannot power off, and only
// an operator fixing spec.shutdownPodSpec (or powering the machine off out of band) closes it. The
// pair is not the frozen pair this repository exists to heal: from ready, ShutdownNode is legal, so the
// moment the script is fixed the next pass powers the machine off with no human touching the slot.
//
// The record is a status.conditions entry, so the bound needs no CRD field: it lives on the slot,
// survives a manager restart, and is what `kubectl describe node.infra.homecluster.dev <name>` shows.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

const (
	// ShutdownAttemptLimit is how many shutdown attempts one slot may make for one scale-down episode
	// before the controller stops and tells an operator. Each attempt is a lifecycle Job, a cordon and
	// a power operation on a real machine, so the number is deliberately small and matches the startup
	// bound it mirrors: three attempts is three shutdown Jobs on a node that is serving pods, and the
	// failure that needs a human (a shutdown script that exits 0 without the provider's power-off
	// landing, a provider API that accepts the request and ignores it) is not fixed by a fourth.
	ShutdownAttemptLimit = 3

	// ConditionShutdownAttempts is the status.conditions entry that carries the count.
	ConditionShutdownAttempts = "ShutdownAttempts"
	// ReasonShutdownAttempt prefixes the Reason that carries the count
	// (ReasonShutdownAttempt + the number, e.g. ShutdownAttempt2).
	ReasonShutdownAttempt = "ShutdownAttempt"
	// ReasonShutdownAttemptLimit is the Reason of a slot that has used its shutdown attempts up. Its
	// stability is what makes the deduplication of the Warning work: the record is the bound and the
	// receipt at once, so the passes after the note write nothing and record nothing.
	ReasonShutdownAttemptLimit = "ShutdownAttemptLimitReached"
	// ReasonShutdownAttemptLimitReached is the Event reason recorded once for a slot whose shutdown
	// attempts are spent. It is exported because internal/progress reads and quotes the bound, and the
	// two writers must not spell it differently.
	ReasonShutdownAttemptLimitReached = ReasonShutdownAttemptLimit
)

// ShutdownAttemptsOf returns how many shutdown attempts this slot has spent in its current
// scale-down episode. The count is read off the condition's Reason, so the bound needs no CRD field; a
// slot with no record has never started a shutdown in this episode, and a record that says the bound is
// used up reads as the bound, so the count cannot be escaped by a Reason this package does not write.
//
// The record is cleared by clearShutdownAttempts, which the two places that prove a scale-down episode
// is over call: a shutdown completion the observation agreed with, and the readiness gate of a boot.
func ShutdownAttemptsOf(node *infrav1alpha1.Node) int {
	condition := shutdownAttemptCondition(node)
	if condition == nil {
		return 0
	}

	if condition.Reason == ReasonShutdownAttemptLimit {
		return ShutdownAttemptLimit
	}

	count, err := strconv.Atoi(strings.TrimPrefix(condition.Reason, ReasonShutdownAttempt))
	if err != nil || count < 0 {
		return 0
	}

	return count
}

// ShutdownAttemptsExhausted reports whether the slot may not run another shutdown in this scale-down
// episode. The consumer is NodeStateMachine.ShutdownNode, and internal/progress reads the same count
// for the backoff it paces the loop with (progress.observedPowerRetryBackoff).
func ShutdownAttemptsExhausted(node *infrav1alpha1.Node) bool {
	return ShutdownAttemptsOf(node) >= ShutdownAttemptLimit
}

// ShutdownAttemptLimitRecorded reports whether the slot already carries the "attempts used up" record.
// It is the deduplication of the Warning: a pool that stopped powering machines off is reported once,
// not on every resync, and the writer that sees this skips its status write.
func ShutdownAttemptLimitRecorded(node *infrav1alpha1.Node) bool {
	condition := shutdownAttemptCondition(node)

	return condition != nil && condition.Reason == ReasonShutdownAttemptLimit
}

// ShutdownAttemptRecord returns the slot's attempt record for a note or a log line, and nil when the
// slot has none.
func ShutdownAttemptRecord(node *infrav1alpha1.Node) *metav1.Condition {
	return shutdownAttemptCondition(node)
}

// shutdownAttemptCondition is the single reader of the record, so the count, the bound and the
// deduplication all look at the same field the same way.
func shutdownAttemptCondition(node *infrav1alpha1.Node) *metav1.Condition {
	return meta.FindStatusCondition(node.Status.Conditions, ConditionShutdownAttempts)
}

// recordShutdownAttempt counts one shutdown attempt on the copy of the slot it is given and returns the
// attempt number it recorded. It mutates the conditions only: the caller owns the status write.
//
// The caller must check ShutdownAttemptsExhausted first: a bound that is incremented past its limit by
// a path that refused to enforce it is a counter an operator cannot read.
func recordShutdownAttempt(node *infrav1alpha1.Node) int {
	attempts := ShutdownAttemptsOf(node) + 1

	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               ConditionShutdownAttempts,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: node.Generation,
		Reason:             ReasonShutdownAttempt + strconv.Itoa(attempts),
		Message: fmt.Sprintf(
			"shutdown attempt %d of %d for %s: the ready -> shuttingdown transition ran, so a coordination lock, a "+
				"cordon and a shutdown lifecycle Job hang off this record. It is cleared when the shutdown lands (the "+
				"completion is recorded with the observation agreeing) and when the slot boots a machine that passes the "+
				"readiness gate, and at %d attempts the controller stops firing shutdowns for this slot.",
			attempts, ShutdownAttemptLimit, shutdownAttemptTarget(node), ShutdownAttemptLimit),
	})

	return attempts
}

// clearShutdownAttempts removes the attempt record from the copy of the slot it is given, and reports
// whether there was anything to remove. A caller that only writes status when this returns true is
// idempotent: the passes after a completed shutdown write nothing.
//
// This is the reset rule, and it is why the bound prices one scale-down episode and not the life of the
// slot: slots are long-lived and are reused across claims, so a count that is never cleared makes the
// fourth scale-down of a machine over its life the last one the controller ever runs.
func clearShutdownAttempts(node *infrav1alpha1.Node) bool {
	return meta.RemoveStatusCondition(&node.Status.Conditions, ConditionShutdownAttempts)
}

// StampShutdownAttemptLimit records that the shutdown attempts of this scale-down episode are used up,
// and that the controller stopped spending power operations on the slot. The Reason is stable, because
// it is the deduplication key; the Message names the attempts, the evidence, and what an operator can
// do, because this is the record that outlives every log line the controller writes about the slot.
func StampShutdownAttemptLimit(node *infrav1alpha1.Node, attempts int, cause string) {
	if cause == "" {
		cause = "each shutdown Job exited 0 while the machine kept reporting Ready"
	}

	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               ConditionShutdownAttempts,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: node.Generation,
		Reason:             ReasonShutdownAttemptLimit,
		Message: fmt.Sprintf(
			"%d of %d shutdown attempts used for %s (%s). The controller will not fire another shutdown for this "+
				"slot: it stays at status.progress %q with status.powerState %q while spec.desiredPowerState stays %q, "+
				"so the machine is up, cordoned, unwanted, and reported. Fix what keeps the shutdown from landing "+
				"(spec.shutdownPodSpec must call the provider's own power-off and exit non-zero when the machine is "+
				"still on), or power the machine off out of band: a shutdown that lands, or a boot that passes the "+
				"readiness gate, clears this record and the %d attempts with it.",
			attempts, ShutdownAttemptLimit, shutdownAttemptTarget(node), cause,
			string(node.Status.Progress), string(node.Status.PowerState), string(node.Spec.DesiredPowerState),
			ShutdownAttemptLimit),
	})
}

// shutdownAttemptTarget names the slot in a record: the CR, and the Kubernetes node its machine runs as.
func shutdownAttemptTarget(node *infrav1alpha1.Node) string {
	if node.Spec.KubernetesNodeName == "" {
		return fmt.Sprintf("node %s (no kubernetes node name)", node.Name)
	}

	return fmt.Sprintf("node %s (kubernetes node %s)", node.Name, node.Spec.KubernetesNodeName)
}

// shutdownAttemptRefusalCause renders the evidence a shutdown-attempt refusal quotes, read off the slot
// itself. The shape the loop leaves behind is recorded on the slot - the Shutdown condition the
// completion stamps, the last shutdown stamp, and the power state the observation publishes - so the
// Warning names what happened to this slot's machine instead of guessing at a generic cause.
func shutdownAttemptRefusalCause(node *infrav1alpha1.Node) string {
	if node.Status.LastShutdownTime != nil {
		return fmt.Sprintf("each shutdown Job of this slot exited 0 with Kubernetes node %q still reporting Ready "+
			"(last shutdown started at %s, status.powerState %q)",
			node.Spec.KubernetesNodeName, node.Status.LastShutdownTime.Time.UTC().Format("2006-01-02T15:04:05Z"),
			string(node.Status.PowerState))
	}

	return fmt.Sprintf("each shutdown Job of this slot exited 0 with status.powerState %q and Kubernetes node %q "+
		"observed up, and no shutdown of this slot is stamped on it",
		string(node.Status.PowerState), node.Spec.KubernetesNodeName)
}

// shutdownAttemptLimitNote renders the Warning of a refusal: the attempts, the evidence, what the
// controller stopped doing, and the one thing an operator can do about it. It is the shape an operator
// sees in the event stream, where a pool that cannot power machines off has to be visible without
// opening an object.
func shutdownAttemptLimitNote(node *infrav1alpha1.Node, attempts int, cause string) string {
	return fmt.Sprintf(
		"Scale down refused for %s: %d of %d shutdown attempts used (%s). The controller will not fire another "+
			"shutdown for this slot, so it stays at status.progress %q with status.powerState %q while "+
			"spec.desiredPowerState stays %q - the machine is up, cordoned, unwanted by its own claim, and paid for. "+
			"Fix spec.shutdownPodSpec so it calls the provider's own power-off and exits non-zero when the machine is "+
			"still on, or power the machine off out of band: the shutdown lands on the next pass and the record, and "+
			"the %d attempts with it.",
		shutdownAttemptTarget(node), attempts, ShutdownAttemptLimit, cause,
		string(node.Status.Progress), string(node.Status.PowerState), string(node.Spec.DesiredPowerState),
		ShutdownAttemptLimit)
}

// ShutdownAttemptLimitError reports a shutdown that was refused because the slot has used up the
// shutdown attempts of its scale-down episode. It is a typed error for the same reason
// StartupAttemptLimitError is one: the caller has to tell a refusal that is the system protecting a
// serving node from an endless re-cordon (stop, tell an operator, do not retry) from a transition that
// failed (keep controller-runtime's backoff, the request may succeed). The slot is left exactly as the
// refusal found it, which for the refused-completion loop is the observed pair - progress: ready,
// powerState: on - that the correction wrote.
type ShutdownAttemptLimitError struct {
	// Node is the slot the refusal is about.
	Node string
	// Attempts is how many shutdown attempts the slot has recorded.
	Attempts int
	// Limit is ShutdownAttemptLimit, quoted so a caller can print the bound it hit.
	Limit int
}

func (e *ShutdownAttemptLimitError) Error() string {
	return fmt.Sprintf("shutdown refused for node %s: %d of %d shutdown attempts used, "+
		"the controller will not fire another shutdown for this slot",
		e.Node, e.Attempts, e.Limit)
}

// AsShutdownAttemptLimit returns the shutdown-attempt refusal behind an error, and nil when the error
// is not one.
func AsShutdownAttemptLimit(err error) *ShutdownAttemptLimitError {
	var limit *ShutdownAttemptLimitError

	if errors.As(err, &limit) {
		return limit
	}

	return nil
}

// ShutdownAttemptsSpent reports the slot's shutdown-attempt count as this state machine sees it, and
// whether that count refuses the next shutdown. It is the mirror of StartupAttemptsSpent, and the
// count is read off this machine's copy of the slot, which every caller has just read from the API:
// the node controller's pass, and the observed-power rule that runs in front of it.
func (nm *NodeStateMachine) ShutdownAttemptsSpent() (int, bool) {
	attempts := ShutdownAttemptsOf(nm.node)

	return attempts, attempts >= ShutdownAttemptLimit
}

// recordShutdownAttempt persists the shutdown attempt this machine is making, and returns the attempt
// number recorded. It is the writer enterShuttingDown calls: the ready -> shuttingdown transition is
// where a shutdown attempt begins, and every caller of ShutdownNode passes through it.
//
// It is a status write of its own, on a freshly read copy, in the shape recordStartupAttempt uses.
// That is one extra write per scale-down, and it is the price of putting the increment where an
// attempt actually begins: a count kept only in the refusal would bound the loop the guard starts and
// leave the plain one - a scale-down whose Job runs, the machine stays up, the completion is refused -
// with nothing on the slot at all.
//
// A write that fails is logged, not fatal: the shutdown proceeds, and under-counting on an API server
// that is refusing writes is the lesser failure. The bound errs towards spending one more power
// operation, never towards refusing a scale-down of a machine that is genuinely off.
func (nm *NodeStateMachine) recordShutdownAttempt() int {
	logger := log.Log.WithName("fsm")
	recorded := 0

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		recorded = recordShutdownAttempt(fresh)

		if err := nm.client.Status().Update(context.TODO(), fresh); err != nil {
			return err
		}

		nm.node = fresh

		return nil
	})
	if err != nil {
		logger.Error(err, "Failed to record the shutdown attempt on the slot",
			"node", nm.node.Name, "attempt", recorded)
	}

	return recorded
}

// clearShutdownAttemptRecord removes the attempt record from the slot: a shutdown landed with the
// observation agreeing, or the slot booted a machine that passed the readiness gate, so this
// scale-down episode is over and the bound starts over. See clearShutdownAttempts for why the episode
// is the unit the bound prices.
//
// It writes only when there is a record to remove, so the passes after a completed shutdown - and
// every scale-down of a slot that never spent an attempt - write nothing.
func (nm *NodeStateMachine) clearShutdownAttemptRecord() {
	logger := log.Log.WithName("fsm")

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		if !clearShutdownAttempts(fresh) {
			// Nothing to reset: no write, and no status churn on a healthy pool.
			return nil
		}

		if err := nm.client.Status().Update(context.TODO(), fresh); err != nil {
			return err
		}

		nm.node = fresh

		logger.V(1).Info("Reset the shutdown-attempt record of a slot whose scale-down episode ended",
			"node", fresh.Name)

		return nil
	})
	if err != nil {
		// The record stays, so the bound keeps the attempts it counted. That is the safe direction:
		// the slot is not frozen (the count only refuses new shutdowns, and the slot is at a stable
		// progress), and the next completed shutdown tries again.
		logger.Error(err, "Failed to reset the shutdown-attempt record of a slot that shut down",
			"node", nm.node.Name)
	}
}

// noteShutdownAttemptLimit records, exactly once, that this slot's shutdown attempts are used up, and
// reports the count and whether the note landed. The caller records its Warning only when the note
// landed, which is what makes the event one-per-episode instead of one-per-resync.
//
// The cause names the evidence the refusal read, so `kubectl describe` says which loop was closed.
func (nm *NodeStateMachine) noteShutdownAttemptLimit(cause string) (int, bool) {
	logger := log.Log.WithName("fsm")

	attempts := 0
	noted := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		current := ShutdownAttemptsOf(fresh)

		if current < ShutdownAttemptLimit {
			// The slot is not out of attempts after all: a shutdown that landed, or a boot that
			// cleared the record, arrived between the read that decided the refusal and this one.
			attempts = current

			return nil
		}

		attempts = current

		if ShutdownAttemptLimitRecorded(fresh) {
			// Steady state of a slot that cannot be powered off: no write, no event.
			return nil
		}

		StampShutdownAttemptLimit(fresh, current, cause)

		if err := nm.client.Status().Update(context.TODO(), fresh); err != nil {
			return err
		}

		nm.node = fresh
		noted = true

		return nil
	})
	if err != nil {
		logger.Error(err, "Failed to record the shutdown-attempt limit on the slot",
			"node", nm.node.Name, "attempts", attempts)

		return attempts, false
	}

	return attempts, noted
}
