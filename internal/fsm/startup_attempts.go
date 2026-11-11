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

// The startup-attempt bound: one counter, one owner, one reset rule.
//
// A slot that is asked for (spec.desiredPowerState: on) whose machine does not boot is a request
// the pool keeps paying for. The shape is a loop, and it has two known entries:
//
//   - the loud one: a startup Job that exits non-zero. afterJobFailed moves the slot
//     startingup -> shutdown and writes the progress, so the next pass sees
//     desired on && powerState off && CanTransition(StartNode), and builds another Job and another
//     machine;
//   - the quiet one: a startup Job that completes with no corev1.Node behind it. The slot lands at
//     {desired: on, powerState: off, progress: ready}, the progress watchdog re-arms it to
//     shutdown, and the pass that follows starts the machine again.
//
// Both entries spend the same thing - a startup attempt, which is a real machine bought - and only
// one bound stops both, and it can only be one counter, because a bound that counts one entry is a
// bound that the other one drives straight through. This file is that counter. It is owned here
// because the FSM is the writer of the slot's lifecycle, and because the two consumers of the count
// are the FSM's StartNode and the watchdog's re-arm: the increment lives where an attempt actually
// begins (enterStartingUp, the transition both entries pass through, and the only place a lifecycle
// startup Job is created), the reset lives where an attempt demonstrably succeeded (the readiness
// gate of afterJobCompleted), and the enforcement lives in front of every path that would spend
// another machine (NodeStateMachine.StartNode, and progress.Settle).
//
// What the counter is not: a rate limiter. It bounds one claim episode of one slot, and the episode
// ends where the boot succeeds. Slots are long-lived and are reused across claim episodes
// (internal/grpcserver picks an existing powered-off Node CR for a new claim), so a count that is
// never cleared is a lifetime bound: three unrelated failures over the life of a machine would leave
// a slot that never retries a machine again, which is exactly what the first version of this counter
// did - it counted the watchdog's re-arms, and nothing ever removed the record.
//
// The record is a status.conditions entry, so the bound needs no CRD field: it lives on the slot,
// survives a manager restart, and is what `kubectl describe node.infra.homecluster.dev <name>`
// shows. Its Type keeps the spelling the first version shipped (ClaimRearmed) so a slot that
// already carries one is read correctly across an upgrade; what the Reason counts is a startup
// attempt.
const (
	// StartupAttemptLimit is how many startup attempts one slot may make for one claim episode
	// before the controller stops and tells an operator. Each attempt is a lifecycle Job and a
	// power-on of a real machine, so the number is deliberately small - three attempts is three
	// machines bought for one slot, and the failure that needs a human (a bad startup command, a
	// bad image, dead power-API credentials) is not fixed by a fourth.
	StartupAttemptLimit = 3

	// ConditionStartupAttempts is the status.conditions entry that carries the count.
	ConditionStartupAttempts = "ClaimRearmed"
	// ReasonStartupAttempt prefixes the Reason that carries the count
	// (ReasonStartupAttempt + the number, e.g. ClaimRearmAttempt2).
	ReasonStartupAttempt = "ClaimRearmAttempt"
	// ReasonStartupAttemptLimit is the Reason of a slot that has used its attempts up. Its
	// stability is what makes the deduplication of the Warning work: the record is the bound and
	// the receipt at once, so the passes after the note write nothing and record nothing.
	ReasonStartupAttemptLimit = "ClaimRearmLimitReached"

	// ReasonStartupAttemptLimitReached is the Event reason recorded once for a slot whose attempts
	// are spent. It is exported because progress (the watchdog) records the same event, and the two
	// writers must not spell it differently.
	ReasonStartupAttemptLimitReached = ReasonStartupAttemptLimit
)

// StartupAttemptsOf returns how many startup attempts this slot has made in its current claim
// episode. The count is read off the condition's Reason, so the bound needs no CRD field; a slot
// with no record has never started a machine in this episode, and a record that says the bound is
// used up reads as the bound, so the count cannot be escaped by a Reason this package does not
// write.
//
// The record is cleared by clearStartupAttempts, which the readiness gate calls: an episode that
// booted a working machine starts the count over, so a slot is not disqualified for the life of the
// cluster by failures that are behind it.
func StartupAttemptsOf(node *infrav1alpha1.Node) int {
	condition := startupAttemptCondition(node)
	if condition == nil {
		return 0
	}

	if condition.Reason == ReasonStartupAttemptLimit {
		return StartupAttemptLimit
	}

	count, err := strconv.Atoi(strings.TrimPrefix(condition.Reason, ReasonStartupAttempt))
	if err != nil || count < 0 {
		return 0
	}

	return count
}

// StartupAttemptsExhausted reports whether the slot may not start another machine in this claim
// episode. Both consumers ask it: NodeStateMachine.StartNode refuses the attempt, and
// progress.Settle refuses to re-arm the slot.
func StartupAttemptsExhausted(node *infrav1alpha1.Node) bool {
	return StartupAttemptsOf(node) >= StartupAttemptLimit
}

// StartupAttemptLimitRecorded reports whether the slot already carries the "attempts used up"
// record. It is the deduplication of the Warning: a slot that is permanently broken is reported
// once, not on every resync, and the writer that sees this skips its status write.
func StartupAttemptLimitRecorded(node *infrav1alpha1.Node) bool {
	condition := startupAttemptCondition(node)

	return condition != nil && condition.Reason == ReasonStartupAttemptLimit
}

// StartupAttemptRecord returns the slot's attempt record for a note or a log line, and nil when the
// slot has none.
func StartupAttemptRecord(node *infrav1alpha1.Node) *metav1.Condition {
	return startupAttemptCondition(node)
}

// startupAttemptCondition is the single reader of the record, so the count, the bound and the
// deduplication all look at the same field the same way.
func startupAttemptCondition(node *infrav1alpha1.Node) *metav1.Condition {
	return meta.FindStatusCondition(node.Status.Conditions, ConditionStartupAttempts)
}

// recordStartupAttempt counts one startup attempt on the copy of the slot it is given and returns
// the attempt number it recorded. It mutates the conditions only: the caller owns the status write,
// which keeps it next to the progress write of the same transition.
//
// The caller must check StartupAttemptsExhausted first. The count is a bound, and a bound that is
// incremented past its limit by a path that refused to enforce it is a counter an operator cannot
// read.
func recordStartupAttempt(node *infrav1alpha1.Node) int {
	attempts := StartupAttemptsOf(node) + 1

	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               ConditionStartupAttempts,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: node.Generation,
		Reason:             ReasonStartupAttempt + strconv.Itoa(attempts),
		Message: fmt.Sprintf(
			"startup attempt %d of %d for %s: the shutdown -> startingup transition ran, so a lifecycle Job and a "+
				"power-on of a real machine hang off this record. It is cleared when the readiness gate of a startup "+
				"completes, and at %d attempts the controller stops starting machines for this slot.",
			attempts, StartupAttemptLimit, startupAttemptTarget(node), StartupAttemptLimit),
	})

	return attempts
}

// clearStartupAttempts removes the attempt record from the copy of the slot it is given, and reports
// whether there was anything to remove. A caller that only writes status when this returns true is
// idempotent: the passes after a completed boot write nothing.
//
// This is the reset rule, and it is the reason the bound is a bound on a claim episode and not on
// the life of the slot. It is called by the readiness gate of afterJobCompleted - the same place
// that decides the slot may be recorded ready - and not by the JobCompleted transition alone: a
// machine that boots, registers and never clears its CNI bootstrap taint has cost the pool a machine
// and has to keep the attempts it spent counted against it.
func clearStartupAttempts(node *infrav1alpha1.Node) bool {
	return meta.RemoveStatusCondition(&node.Status.Conditions, ConditionStartupAttempts)
}

// StampStartupAttemptLimit records that the attempts of this claim episode are used up, and that
// the controller stopped spending machines on the slot. The Reason is stable, because it is the
// deduplication key; the Message names the attempts, the evidence, and what an operator can do,
// because this is the record that outlives every log line the controller writes about the slot.
func StampStartupAttemptLimit(node *infrav1alpha1.Node, attempts int, cause string) {
	if cause == "" {
		cause = "no Kubernetes node ever registered for the machines that were started"
	}

	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               ConditionStartupAttempts,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: node.Generation,
		Reason:             ReasonStartupAttemptLimit,
		Message: fmt.Sprintf(
			"%d of %d startup attempts used for %s (%s). The controller will not start another machine for this slot: "+
				"the claim is kept in spec.desiredPowerState and the slot is settled to status.progress %q with "+
				"status.powerState %q, so it is counted as requested capacity with no machine behind it. Scale the group "+
				"down to drop the claim, or fix what keeps the machine from booting (startup command, image, power API "+
				"credentials) - a boot that passes the readiness gate clears this record and the %d attempts with it.",
			attempts, StartupAttemptLimit, startupAttemptTarget(node), cause,
			string(infrav1alpha1.ProgressShutdown), string(infrav1alpha1.PowerStateOff), StartupAttemptLimit),
	})
}

// startupAttemptTarget names the slot in a record: the CR, and the Kubernetes node its machines are
// supposed to register as. Spelled out rather than printed empty, because "this slot has no node
// name" is part of the diagnosis.
func startupAttemptTarget(node *infrav1alpha1.Node) string {
	if node.Spec.KubernetesNodeName == "" {
		return fmt.Sprintf("node %s (no kubernetes node name)", node.Name)
	}

	return fmt.Sprintf("node %s (kubernetes node %s)", node.Name, node.Spec.KubernetesNodeName)
}

// StartupAttemptLimitError reports a startup that was refused because the slot has used up the
// startup attempts of its claim episode. It is a typed error for the same reason
// LockConflictError is one: the caller has to tell a refusal that is the system protecting the pool
// from a machine that will not boot (stop, tell an operator, do not retry) from a transition that
// failed (keep controller-runtime's backoff, the request may succeed). The slot is left exactly as
// the refusal found it, which for both entries of the loop is the stable
// powerState: off / progress: shutdown pair the claim sits on while it is off.
type StartupAttemptLimitError struct {
	// Node is the slot the refusal is about.
	Node string
	// Attempts is how many startup attempts the slot has recorded.
	Attempts int
	// Limit is StartupAttemptLimit, quoted so a caller can print the bound it hit.
	Limit int
}

func (e *StartupAttemptLimitError) Error() string {
	return fmt.Sprintf("startup refused for node %s: %d of %d startup attempts used, "+
		"the controller will not start another machine for this slot",
		e.Node, e.Attempts, e.Limit)
}

// AsStartupAttemptLimit returns the startup-attempt refusal behind an error, and nil when the error
// is not one. See StartupAttemptLimitError for what a caller that got one must do (and must not).
func AsStartupAttemptLimit(err error) *StartupAttemptLimitError {
	var limit *StartupAttemptLimitError

	if errors.As(err, &limit) {
		return limit
	}

	return nil
}

// StartupAttemptsSpent reports the slot's startup-attempt count as this state machine sees it, and
// whether that count refuses the next attempt. It is exported for the node controller, which asks
// before it fires the transition so the refusal is reported as the settled state it is and not as a
// reconcile failure.
//
// The count is read off this machine's copy of the slot, which every caller has just read from the
// API: the node controller's pass, and the watchdog pass that runs in front of it.
func (nm *NodeStateMachine) StartupAttemptsSpent() (int, bool) {
	attempts := StartupAttemptsOf(nm.node)

	return attempts, attempts >= StartupAttemptLimit
}

// recordStartupAttempt persists the startup attempt this machine is making, and returns the attempt
// number recorded. It is the writer enterStartingUp calls: the shutdown -> startingup transition is
// where a startup attempt begins, and both entries of the failure loop pass through it.
//
// It is a status write of its own, on a freshly read copy, in the shape every other writer of the
// slot's status uses (read-fresh, mutate, write, re-sync). That is one extra write per scale-up,
// and it is the price of putting the increment where an attempt actually begins: the alternative is
// a count that only one of the two loops pays, which is the bug this file removes. The write happens
// before the lifecycle Job of the attempt is created, so an attempt that dies on the way to its Job
// is counted too - it is an attempt the operator has to be able to see.
//
// A write that fails is logged, not fatal: the startup proceeds, the slot is transitional, and
// under-counting on an API server that is refusing writes is the lesser failure. The gate that
// clears the record is the same kind of write, and the bound errs towards spending one more machine,
// never towards freezing a slot.
func (nm *NodeStateMachine) recordStartupAttempt() int {
	logger := log.Log.WithName("fsm")
	recorded := 0

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		recorded = recordStartupAttempt(fresh)

		if err := nm.client.Status().Update(context.TODO(), fresh); err != nil {
			return err
		}

		nm.node = fresh

		return nil
	})
	if err != nil {
		logger.Error(err, "Failed to record the startup attempt on the slot",
			"node", nm.node.Name, "attempt", recorded)
	}

	return recorded
}

// clearStartupAttemptRecord removes the attempt record from the slot: the readiness gate of a
// startup completed, so the claim episode booted a machine that can take the pool's work and the
// bound starts over. See clearStartupAttempts for why the gate is the reset rule and a finished Job
// is not.
//
// It writes only when there is a record to remove, so the passes after a completed boot - and every
// boot of a slot that never spent an attempt - write nothing.
func (nm *NodeStateMachine) clearStartupAttemptRecord() {
	logger := log.Log.WithName("fsm")

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		if !clearStartupAttempts(fresh) {
			// Nothing to reset: no write, and no status churn on a healthy pool.
			return nil
		}

		if err := nm.client.Status().Update(context.TODO(), fresh); err != nil {
			return err
		}

		nm.node = fresh

		logger.V(1).Info("Reset the startup-attempt record of a slot whose readiness gate completed",
			"node", fresh.Name)

		return nil
	})
	if err != nil {
		// The record stays, so the bound keeps the attempts it counted. That is the safe direction:
		// the slot is not frozen (the count only refuses new machines, and the slot is at a stable
		// progress), and the next completed boot tries again.
		logger.Error(err, "Failed to reset the startup-attempt record of a slot that booted",
			"node", nm.node.Name)
	}
}

// noteStartupAttemptLimit records, exactly once, that this slot's startup attempts are used up, and
// reports the count and whether the note landed. The caller records its Warning only when the note
// landed, which is what makes the event one-per-episode instead of one-per-resync: a pool that
// stopped booting is a fact an operator has to see, and an event storm on the API server is not how
// it gets seen.
//
// The cause names the path that refused, so `kubectl describe` says which loop was closed
// ("the startup Job failed 3 times" and "the slot's machines never registered" are different
// failures with the same bound).
func (nm *NodeStateMachine) noteStartupAttemptLimit(cause string) (int, bool) {
	logger := log.Log.WithName("fsm")

	attempts := 0
	noted := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		current := StartupAttemptsOf(fresh)

		if current < StartupAttemptLimit {
			// The slot is not out of attempts after all: a boot cleared the record between the read
			// that decided the refusal and this one. Nothing to note, and the caller records nothing.
			attempts = current

			return nil
		}

		attempts = current

		if StartupAttemptLimitRecorded(fresh) {
			// Steady state of a slot that will not boot: no write, no event.
			return nil
		}

		StampStartupAttemptLimit(fresh, current, cause)

		if err := nm.client.Status().Update(context.TODO(), fresh); err != nil {
			return err
		}

		nm.node = fresh
		noted = true

		return nil
	})
	if err != nil {
		logger.Error(err, "Failed to record the startup-attempt limit on the slot",
			"node", nm.node.Name, "attempts", attempts)

		return attempts, false
	}

	return attempts, noted
}
