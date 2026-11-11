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

// Package progress settles status.progress of a Node CR against what is in the cluster.
//
// A power transition has one writer, the FSM, and its rule is that the operation which wrote
// a progress is the only one that moves it (see fsm.NodeStateMachine.updateNodeProgress). That
// rule is what stops a stale state machine - the job monitor of an earlier reconcile, a machine
// built from a Node read before a transition - from stomping a slot. Its cost is the shape this
// package closes: a slot left at ready / startingup / shuttingdown with no machine and no
// lifecycle Job behind it, which is what a machine deleted out of band (a Hetzner server gone,
// so no corev1.Node ever pointed at the slot) leaves behind. Nothing in a reconcile heals such
// a slot:
//
//   - core.KubernetesNodeReconciler.applyObservedPowerState is driven by a corev1.Node, and the
//     slot has none, so the reconciler never runs for it;
//   - the FSM cannot fire ShutdownNode from shuttingdown, and firing it from ready would create
//     a shutdown Job for a machine that does not exist - the churn that same pool lived through;
//   - the stuck-transition cleanup (ForceCleanup at StuckTransitionThreshold) reads the backoff
//     record of the state machine that ran the transition, and the reconciler builds a fresh
//     machine on every pass, so IsTransitionStuck() is false there and the cleanup is never
//     reached from a reconcile.
//
// Such a progress is not cosmetic. shuttingdown/shutdown are excluded from the provider's target
// size and ready is counted as a running machine, so the group reports a size the cluster does
// not have; and validateStateTransition gates every transition of a transitional slot out, so
// the slot itself is frozen: no lifecycle operation can start on it and none can finish it.
//
// This package is the one writer that settles such a slot, and it is deliberately narrow. It acts
// only on a slot that claims to have a machine (ready / startingup / shuttingdown), has no
// corev1.Node, has no lifecycle Job in flight and no live coordination lock, and whose grace has
// elapsed. Three outcomes come out of that one gate, and they share it so they cannot drift:
//
//   - a slot whose spec.desiredPowerState is off is settled: the stable off / shutdown pair the
//     observation controllers agree on, with a Warning naming the progress it replaced;
//   - a slot whose spec.desiredPowerState is on - a claim - is never settled, and never was, but
//     it is not left pointing at a machine that does not exist either: it is re-armed. The slot
//     moves to off / shutdown and keeps spec.desiredPowerState: on, which is the shape the FSM's
//     StartNode is legal from, so the pool retries the machine. Re-arming is bounded by the
//     startup-attempt counter the FSM owns (fsm.StartupAttemptLimit, counted where an attempt
//     actually begins and cleared where a boot passes the readiness gate - see
//     internal/fsm/startup_attempts.go), because a claim whose startup keeps failing must not
//     recreate a server in a loop. Past the bound the re-arming stops and the slot is settled to the
//     stable pair with one Warning naming it; see ClaimRearmLimit for why the claim cannot simply be
//     given up on;
//   - a slot that has a node object, is at ready, is claimed on, and is cordoned with nothing
//     running on it is uncordoned. That is the safety net for the paths into ready that never call
//     the uncordon hook (JobTimeout and JobFailed roll a slot back to ready), where a cordoned
//     machine is invisible to cluster-autoscaler's scale-down and is therefore paid for forever.
//
// It records a Warning for each, and writes nothing else: the claim is the request, and this package
// does not answer it, it only makes the slot able to answer it again.
//
// The package holds two rules, and they have distinct jobs. Read them as the two directions a slot
// can lie in:
//
//   - Settle settles a *claim that produced nothing*: the progress names a machine (ready /
//     startingup / shuttingdown) and the cluster has none - no corev1.Node, no lifecycle Job, no
//     live lock. Its write is the stable off / shutdown pair.
//   - ReconcileObservedPower (see observed_power.go) corrects a *machine that outlived its recorded
//     state*: the progress says there is no machine (shutdown / shuttingdown, the two values the
//     FSM reads as StateShutdown) and the cluster has positively observed one up - status.powerState
//     on, and the slot's corev1.Node reporting Ready, which is the same observation that wrote the
//     on. Its write is the observed pair, progress: ready.
//
// They are disjoint on the two shapes they own, and that is structural, not a coordination: Settle's
// settle and re-arm arms require a slot with no corev1.Node at all, and ReconcileObservedPower
// requires a corev1.Node that reports Ready. The two rules share the gates that are not about the
// machine - observe, the lifecycle Job grace (jobWindow, plus the stamp-measured fallback the Job
// gate falls through to when no Job object is left: startupGrace for the watchdog, shutdownGrace for
// this rule) and the coordination lock - so they can never disagree about whether an operation owns
// the slot, and both run in the node controller's FSM path, above every branch that reads
// status.progress.
//
// Disjoint is not exhaustive. There is a third shape neither rule acts on, and it is stated here
// because a reader of "the two rules cover every pair" would assume it: {status.progress:
// shuttingdown, a corev1.Node that is present and NotReady, no lifecycle Job, no live lock} - the
// transitional progress of a shutdown whose operation died, on a machine whose node object is there
// and reporting down. Settle refuses it: its gates require a slot with no node object at all, because
// the status of a slot that has a node belongs to the observation controllers. ReconcileObservedPower
// refuses it: NotReady is not positive evidence that a machine is up, so there is nothing for the
// observation to outrank the progress with. And the observation controller that does own a slot with a
// node object - core.applyObservedPowerState - writes the off and never touches a transitional
// progress, which is the rule that keeps the boot window from being read as a shutdown.
//
// So nothing in this package moves that slot, and the pair is frozen in the same way the shapes this
// package exists for are: validateStateTransition gates every transition of a transitional slot out,
// and the stuck-transition cleanup is not reached from a reconcile (the reconciler builds a fresh
// state machine per pass, so IsTransitionStuck is false on it). It is a known hole, deliberately not
// closed here: settling it means reading "NotReady" as "the machine is off", and that reading is what
// the observation controller refuses to make for the boot window. The operator's way out is what it
// was before this package existed - a status.progress patch - and docs/architecture/state.md states
// the shape next to the two rules that own the other two.
//
// The two rules also share the bound and the pace of the loop they can start: a correction to ready
// is what makes ShutdownNode legal, and a pool whose shutdown script exits 0 without the power-off
// landing loops (Job, refused completion, correction, shutdown, Job). The bound is the shutdown-attempt
// counter the FSM owns (fsm.ShutdownAttemptLimit, internal/fsm/shutdown_attempts.go), and
// ReconcileObservedPower sets the pace: a slot with refused completions is looked at again on an
// exponentially growing deadline (observedPowerRetryBackoff). See observed_power.go.
package progress

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/taints"
)

// SettleGracePeriod is how long a *finished* lifecycle Job keeps its slot out of reach of the
// watchdog.
//
// The Job is the operation and the state machine that started it is the writer that settles the
// progress it moved: fsm.monitorJobCompletion polls the Job and fires JobCompleted, whose
// after_ hook writes the stable progress. The monitor polls every fsm.JobMonitorPollInterval in
// production, and it runs in the goroutine StartNode / ShutdownNode spawned, so a Job that
// completed a second ago has not necessarily been acted on yet. Stomping the progress in that
// window writes over a transition its owner is about to record, and the FSM reads its state from
// progress, so the write is how a duplicate lifecycle Job - and a second scale-up - is born.
//
// That window is not one poll long, and this value is derived from it rather than picked, because
// the after_JobCompleted hook of a completed startup Job runs setNodeSchedulableWithRetry *before*
// it writes the progress. That loop can consume, in order:
//
//   - one monitor poll to notice the Job is done (fsm.JobMonitorPollInterval, 30s);
//   - the registration wait for the corev1.Node to appear (fsm.DefaultUncordonTimeout, 90s);
//   - the readiness gate for that node to be Ready and free of the readiness taints, counted from
//     the pass the node object appears on (fsm.DefaultReadinessGateTimeout, 4m);
//   - one sleep at the ceiling of the loop's backoff, which overshoots the deadline it checks
//     (fsm.MaxUncordonRetryInterval, 10s).
//
// The grace is that sum plus a minute of slack: strictly longer than the path it has to be longer
// than. TestSettleGracePeriodOutlivesTheWritersItDefersTo computes the same sum and fails when any
// of those four numbers grows without the grace growing with it. The value this constant used to
// carry (2m) covered the monitor poll alone, so a scale-up whose gate was still running was
// settled to shutdown, and the hook then wrote ready on top of it: two writers, one slot, and a
// startup Job that had just been paid for.
//
// What the grace is *not* measured against: the 5m Job timeout. The grace only ever applies to a
// finished Job - a Job that is pending or running is an operation in flight, never settled on any
// deadline, and is merely looked at again - so a Job of a live monitor is protected by the Job
// itself, not by this number. The bound the value has to respect is fsm.StuckTransitionThreshold
// (15m), the horizon past which a transitional slot is "stuck" from an operator's point of view: a
// slot whose monitor died with its manager - the shape this package exists for, a manager restart
// that takes the goroutine away - is healed inside the grace, well inside that horizon, and not on
// every resync.
//
// A slot with no Job object at all is measured against the same window, from status.lastStartupTime
// (see startupGrace): a startup Job collected by a short spec.ttlSecondsAfterFinished leaves the slot
// in the position of a startup in flight, and a grace that only exists while the Job object does
// would open inside the hook that owns the progress.
const SettleGracePeriod = fsm.JobMonitorPollInterval +
	fsm.DefaultUncordonTimeout +
	fsm.DefaultReadinessGateTimeout +
	fsm.MaxUncordonRetryInterval +
	time.Minute

// Reason and action recorded on the Node CR when the watchdog acts, so that
// `kubectl describe node.infra.homecluster.dev <name>` shows who moved status.progress, from what
// evidence, and why.
const (
	// ReasonProgressReconciled is recorded for a status.progress that named a machine the
	// cluster does not have.
	ReasonProgressReconciled = "ProgressReconciled"
	// ReasonClaimRearmed is recorded when a claimed slot whose machine never arrived is sent back
	// through the startup path.
	ReasonClaimRearmed = "ClaimRearmed"
	// ReasonClaimRearmLimitReached is recorded when a slot has used its startup attempts and this
	// package stopped re-arming it. The name is the FSM's, because the counter is the FSM's: the
	// watchdog's limit pass and the FSM's refusal of a StartNode record the same reason, and the
	// record they deduplicate on is shared (see fsm.ReasonStartupAttemptLimitReached).
	ReasonClaimRearmLimitReached = fsm.ReasonStartupAttemptLimitReached
	// ReasonCordonReconciled is recorded when the safety net uncordons a ready slot whose node is
	// cordoned with no operation running on it.
	ReasonCordonReconciled = "CordonReconciled"

	// ActionSettleProgress is the action the settlement of a desired-off slot belongs to.
	ActionSettleProgress = "SettleProgress"
	// ActionRearmProgress is the action a re-arm of a claimed slot belongs to.
	ActionRearmProgress = "RearmProgress"
	// ActionUncordonNode is the action the cordon safety net belongs to.
	ActionUncordonNode = "UncordonNode"
)

// The claim re-arm, and the bound on it. A slot cluster-autoscaler asked for
// (spec.desiredPowerState: on) whose startup Job completed and whose machine never registered ends at
// {desired: on, powerState: off, progress: ready}. Nothing in the system heals that shape: the
// watchdog's settlement refuses it (the claim gate), CanTransition(StartNode) is false from ready,
// NodeGroupTargetSize counts the slot forever so the autoscaler believes the group has capacity that
// never materialises, and NodeGroupDecreaseTargetSize cannot cancel it (isStartingUpSlot wants
// progress: startingup). So the slot is re-armed rather than settled.
//
// The bound on the re-arm is not this package's to keep. It counts *startup attempts*, it is
// incremented where an attempt actually begins (fsm.enterStartingUp, the transition both the
// watchdog's re-arm and a JobFailed rollback lead back into), it is cleared where an attempt
// demonstrably succeeded (the readiness gate of fsm.afterJobCompleted), and it is enforced by both
// consumers: here, and by fsm.NodeStateMachine.StartNode, which refuses a StartNode on a slot that
// has used its attempts. A counter this package kept for its own re-arms bounded one loop and left
// the loud one - a startup Job that exits non-zero - running free, and never reset, so it was a
// lifetime bound on a slot that is reused across claim episodes. The record is a status.conditions
// entry (fsm.ConditionStartupAttempts), so the bound needs no CRD field, and both consumers read the
// same field they deduplicate their Warning on. See internal/fsm/startup_attempts.go for the whole
// rule and why it lives there.
const (
	// ClaimRearmLimit is how many startup attempts one slot may make for one claim episode before
	// the controller stops starting machines for it and tells the operator. Each attempt is a real
	// startup (the FSM's StartNode, a lifecycle Job, a power-on of a real machine), so the number is
	// deliberately small - three attempts is three machines bought for one slot - and the operator
	// sees the limit as a Warning rather than discovering it from the bill. The value and the record
	// are the FSM's; this is the name the watchdog's tests and docs use.
	ClaimRearmLimit = fsm.StartupAttemptLimit

	// ConditionClaimRearmed is the status.conditions entry that carries the startup-attempt count of
	// the slot. Its Reason carries the counter (ReasonClaimRearmAttempt + the count, e.g.
	// ClaimRearmAttempt2), which is why no CRD field is needed to bound the loop: the record lives on
	// the slot, survives a manager restart, shows in `kubectl describe`, and the deduplication of the
	// limit Warning reads the same field the bound does.
	ConditionClaimRearmed = fsm.ConditionStartupAttempts
	// ReasonClaimRearmAttempt prefixes the reason that carries the count.
	ReasonClaimRearmAttempt = fsm.ReasonStartupAttempt
)

// Action is what one pass of the watchdog decided to do about the slot. The three actions share one
// gate (see decide), so the evidence that allows a settlement is the evidence that allows a re-arm
// and the cordon fix: they cannot drift apart, and a slot is never in the position of being acted on
// by one and refused by another for a reason the first one did not check.
type Action string

const (
	// ActionNone leaves the slot alone: an operation owns it, a machine is behind it, or its
	// progress already says what the cluster looks like.
	ActionNone Action = ""

	// ActionSettle writes the stable off / shutdown pair of a slot whose machine is gone, keeping
	// spec.desiredPowerState as it is. It is the settlement of a desired-off slot, and - with
	// decision.limited set - the closing of a claimed slot whose startup attempts are spent: the same
	// pair, the claim kept, and the note that says the controller will not start another machine.
	ActionSettle Action = "settle"

	// ActionRearm writes progress: shutdown and powerState: off for a *claimed* slot whose machine
	// is gone, and leaves spec.desiredPowerState: on: the request stands, only the lie about the
	// machine is removed, which is what makes the FSM's StartNode legal again. It records no counter:
	// the attempt is counted by the FSM where it begins, and the arm that is refused is the one past
	// the bound (see ClaimRearmLimit).
	ActionRearm Action = "rearm"

	// ActionUncordon clears spec.unschedulable of the node of a ready slot that is claimed on and
	// has no operation running on it. It writes no status at all: the cordon is the fact that is
	// wrong, not the progress.
	ActionUncordon Action = "uncordon"
)

// Outcome reports what one pass of the watchdog decided, so the caller can log it and, where it
// has a say, come back at the deadline the watchdog names.
type Outcome struct {
	// Action is what the pass did, ActionNone when it did nothing.
	Action Action

	// Settled is true when a desired-off slot was written: Action == ActionSettle. It is kept as
	// the spelling the callers and the tests of the original settlement use.
	Settled bool

	// Rearmed is true when a claimed slot was sent back through the startup path:
	// Action == ActionRearm.
	Rearmed bool

	// Uncordoned is true when the cordon safety net cleared spec.unschedulable:
	// Action == ActionUncordon.
	Uncordoned bool

	// Corrected is true when the observed power of the slot overruled its recorded progress and the
	// progress was written: Action == ActionCorrectToReady (see observed_power.go).
	Corrected bool

	// ShutdownAttempts is the shutdown-attempt count of the slot, read off the record the FSM owns
	// (fsm.ShutdownAttemptsOf): the shutdowns whose completion the observation refused. Only the
	// observed-power rule reports it, and only reads it: the counter is incremented where a shutdown
	// attempt begins (fsm.enterShuttingDown) and cleared where one lands. It is the number the operator
	// sees in the event, the number the bound (fsm.ShutdownAttemptLimit) is enforced on, and the number
	// the exponential RequeueAfter below is computed from. This pass writes none of it.
	ShutdownAttempts int

	// Previous is the progress the slot carried before the write; empty when nothing was
	// written.
	Previous infrav1alpha1.Progress

	// Attempts is the startup-attempt count of the slot's claim episode, read off the record the FSM
	// owns (fsm.StartupAttemptsOf): 0 for a desired-off settlement, the attempts already spent for the
	// limit path, the attempts spent for a re-arm. It is the number the operator sees in the event, and
	// it is bounded by ClaimRearmLimit. This pass writes none of it.
	Attempts int

	// RequeueAfter is how long the caller should wait before looking at this slot again: > 0
	// when the slot was left alone because a lifecycle Job is in flight or is inside the grace
	// of finishing, so the settlement is not forgotten, it is scheduled; and - for the observed-power
	// rule - > 0 after it corrected a slot whose recent shutdown completions were refused, on the
	// exponential backoff of that loop (observedPowerRetryBackoff), so a pool whose shutdown script
	// never powers off slows down instead of firing a shutdown per pass.
	RequeueAfter time.Duration

	// Reason explains, for the log line, why nothing was settled.
	Reason string
}

// Settle reconciles status.progress of a slot with observed reality and reports what it did.
//
// The rules, and why each one is there:
//
//   - the progress must be one that implies a machine: ready, startingup, shuttingdown.
//     shutdown and the empty progress of a never-seeded slot are already the settled answer, so
//     a pass over them writes nothing, which is what keeps this idempotent.
//   - there must be no lifecycle Job in flight, and a Job that finished must be older than grace.
//     The Job is the operation; the state machine that started it settles its progress, and the
//     grace covers the gap between a Job finishing and that machine acting on it. With no Job object
//     at all - a Job collected by a short spec.ttlSecondsAfterFinished is the common shape - the
//     grace is measured from status.lastStartupTime, the stamp the FSM's own progress write leaves,
//     so a collected Job cannot open the gate inside a hook that is still running (see startupGrace).
//   - the slot must carry no live coordination lock. A lock says an operation owns the slot right
//     now - including the window in which the FSM has written the transitional progress and has
//     not created the Job yet, and the scale-down the provider is running.
//
// What the slot's claim and its node object then decide:
//
//   - spec.desiredPowerState off, no node object -> settle: the stable off / shutdown pair the
//     observation controllers agree on, one Warning naming the progress it replaced.
//   - spec.desiredPowerState on, no node object -> re-arm: the same pair, with
//     spec.desiredPowerState left on. The claim is the request and is never dropped here -
//     dropping it is the provider's arithmetic (NodeGroupDecreaseTargetSize), and a watchdog that
//     answered a scale-up request by declaring the capacity wanted is the failure the target-size
//     fix removed. What is removed is the progress that names a machine that does not exist,
//     because from ready the FSM cannot StartNode, and from shutdown it can: the pool retries the
//     machine. The retries are bounded by the startup-attempt counter the FSM owns
//     (ClaimRearmLimit, counted where an attempt begins and cleared where a boot passes the
//     readiness gate), and the FSM refuses the StartNode a re-arm leads to once the attempts are
//     spent. Past the bound this pass stops re-arming and settles the slot to the stable pair with
//     one Warning (ReasonClaimRearmLimitReached): the progress stops naming a machine the cluster does
//     not have - which is what makes the slot healable by every other writer - while the claim, and
//     with it the target-size unit the autoscaler asked for, stays.
//   - spec.desiredPowerState on, a node object that is cordoned, progress ready, and no marker of
//     cluster-autoscaler on the node -> uncordon. The paths that roll a slot back into ready
//     (JobTimeout, JobFailed) never call the uncordon hook, so a machine cordoned by the shutdown
//     that did not finish stays cordoned: cluster-autoscaler skips cordoned nodes when it picks
//     scale-down candidates, so it is never drained, never deleted, and paid for forever. A node
//     cluster-autoscaler has marked for deletion is its decision and is left alone.
//
// A node object - Ready or not - is a machine, and the status of a slot that has one belongs to the
// observation controllers: core.applyObservedPowerState is the writer for the NotReady and Ready
// windows of a real machine, so nothing about a slot with a node is settled here except the cordon
// above.
//
// Every write goes through the status subresource, using the same read-fresh, re-decide,
// retry-on-conflict shape as fsm.updateNodeProgress. The caller's copy of the slot is updated in
// place, so the pass that acted does not then run a transition on the progress it replaced.
//
// A nil recorder records nothing, which is what the unit tests use. grace <= 0 selects
// SettleGracePeriod.
func Settle(ctx context.Context, c client.Client, slot *infrav1alpha1.Node, recorder events.EventRecorder, grace time.Duration) (Outcome, error) {
	if grace <= 0 {
		grace = SettleGracePeriod
	}

	logger := log.FromContext(ctx).WithValues("node", slot.Name)
	coord := fsm.NewCoordinationManager(c, fsm.NodeControllerOwner)

	if !impliesMachine(slot.Status.Progress) {
		return Outcome{Reason: fmt.Sprintf("status.progress %q does not imply a machine", slot.Status.Progress)}, nil
	}

	obs, err := observe(ctx, c, coord, slot, grace)
	if err != nil {
		// The API server is unreachable, the node object read failed, the Job List failed, ... Nothing
		// is written and the caller reports the error, like it does for every other failure of the pass.
		// That is the rule this package writes under: a settlement, a re-arm, an uncordon, and the
		// observed-power correction are all decided from this one read, so a partially-failed
		// observation - a machine this pass could not see, Jobs this pass could not list - is never
		// evidence to write over a slot. The decide below is reached only with the whole evidence set.
		return Outcome{}, err
	}

	decision := decide(slot, obs)
	if decision.action == ActionNone {
		if obs.lockHeld {
			logger = logger.WithValues("lock", obs.lockNote)
		}

		if decision.requeueAfter > 0 {
			logger.V(1).Info("Leaving status.progress to the operation in flight",
				"progress", string(slot.Status.Progress),
				"reason", decision.reason,
				"requeueAfter", decision.requeueAfter.String())

			return Outcome{RequeueAfter: decision.requeueAfter, Reason: decision.reason}, nil
		}

		logger.V(1).Info("Nothing to reconcile", "progress", string(slot.Status.Progress), "reason", decision.reason)

		return Outcome{Reason: decision.reason}, nil
	}

	wrote, previous, attempts, err := apply(ctx, c, coord, slot, decision, grace)
	if err != nil {
		return Outcome{}, err
	}

	if !wrote {
		// The object moved between the decision and the write and no longer qualifies: the
		// machine of a lifecycle Job, an observation controller, a claim, got there first.
		logger.V(1).Info("The slot stopped qualifying while writing",
			"progress", string(slot.Status.Progress),
			"action", string(decision.action))

		return Outcome{Reason: "the slot stopped qualifying on re-read"}, nil
	}

	// Two channels, on purpose: the event is what `kubectl describe` shows for a slot the cluster
	// disagrees with, the log line is the controller's own trail. None of the three actions is
	// routine: a settlement means a machine was deleted behind the operator's back or an operation
	// died with its manager, a re-arm means a machine the autoscaler paid for never booted, and a
	// cordon left on a ready slot means a lifecycle operation rolled back and left the machine
	// unschedulable.
	outcome := Outcome{Action: decision.action, Previous: previous, Attempts: attempts}

	switch decision.action {
	case ActionSettle:
		outcome.Settled = true

		if decision.limited {
			// The slot has used the startup attempts of its claim episode, so it is not re-armed: an
			// armed slot is a slot the next pass starts, and the FSM refuses that start, so re-arming
			// would leave the progress pointing at a machine again for nothing. It is settled to the
			// stable off/shutdown pair the desired-off branch writes, with the claim kept, and the note
			// the same write stamped says the controller will not start another machine for it. That
			// closes the lie the progress tells: it now says what the cluster is - no machine, and no
			// operation. The claim is untouched, so the slot is still counted as requested capacity
			// (countsTowardsTargetSize counts a claimed slot whatever its progress says, and dropping a
			// claim is the provider's arithmetic, never this pass's); what changes is that every reader of
			// the progress now reads the truth, which is what makes the slot healable at all. Exactly one
			// Warning - the record is the deduplication, so every later pass over this slot writes nothing
			// and records nothing.
			logger.Info("Settled a claimed slot that has used its startup attempts",
				"previousProgress", string(previous),
				"progress", string(infrav1alpha1.ProgressShutdown),
				"powerstate", string(infrav1alpha1.PowerStateOff),
				"desiredPowerState", string(slot.Spec.DesiredPowerState),
				"attempts", attempts,
				"limit", ClaimRearmLimit,
				"kubernetesNode", slot.Spec.KubernetesNodeName)

			if recorder != nil {
				recorder.Eventf(slot, nil, corev1.EventTypeWarning, ReasonClaimRearmLimitReached, ActionSettleProgress,
					"%s", rearmLimitNote(slot, attempts))
			}

			return outcome, nil
		}

		logger.Info("Settled status.progress of a slot with no machine and no lifecycle Job",
			"previousProgress", string(previous),
			"progress", string(infrav1alpha1.ProgressShutdown),
			"powerstate", string(infrav1alpha1.PowerStateOff),
			"kubernetesNode", slot.Spec.KubernetesNodeName)

		if recorder != nil {
			recorder.Eventf(slot, nil, corev1.EventTypeWarning, ReasonProgressReconciled, ActionSettleProgress,
				"%s", settlementNote(slot, previous))
		}

	case ActionRearm:
		outcome.Rearmed = true

		// attempts is the count the slot carries - the attempts already spent, written by the FSM's
		// enterStartingUp hook, not by this pass. The event names the attempt the arming leads to
		// (attempts+1), because that is the machine the pool is about to buy and the number the
		// operator has to be able to count to.
		logger.Info("Re-armed a claimed slot whose machine never arrived",
			"previousProgress", string(previous),
			"progress", string(infrav1alpha1.ProgressShutdown),
			"powerstate", string(infrav1alpha1.PowerStateOff),
			"desiredPowerState", string(slot.Spec.DesiredPowerState),
			"attemptsUsed", attempts,
			"nextAttempt", attempts+1,
			"limit", ClaimRearmLimit,
			"kubernetesNode", slot.Spec.KubernetesNodeName)

		if recorder != nil {
			recorder.Eventf(slot, nil, corev1.EventTypeNormal, ReasonClaimRearmed, ActionRearmProgress,
				"%s", rearmNote(slot, previous, attempts+1))
		}

	case ActionUncordon:
		outcome.Uncordoned = true

		logger.Info("Uncordoned the node of a ready slot with no operation running on it",
			"kubernetesNode", slot.Spec.KubernetesNodeName,
			"progress", string(slot.Status.Progress),
			"desiredPowerState", string(slot.Spec.DesiredPowerState))

		if recorder != nil {
			recorder.Eventf(slot, nil, corev1.EventTypeWarning, ReasonCordonReconciled, ActionUncordonNode,
				"%s", uncordonNote(slot))
		}
	}

	return outcome, nil
}

// observations is what one pass measured about a slot. It is a struct so the rule (decide) takes its
// evidence in one place, the evidence cannot be passed in the wrong order, and the write path
// re-measures it with the same call (see observe).
type observations struct {
	// nodePresent is true when the slot has a corev1.Node, Ready or not: a node object is a
	// machine, and the status of a slot with one belongs to the observation controllers.
	nodePresent bool

	// nodeCordoned is spec.unschedulable of that node, and nodeMarked says whether
	// cluster-autoscaler has put one of its deletion markers on it. Both are false with no node.
	nodeCordoned bool
	nodeMarked   bool

	// nodeReady is the Ready condition of that node, and is the strong half of the evidence the
	// observed-power rule acts on: NodeReady=True is what core.getNodeStatus maps onto
	// status.powerState: on, so a slot whose node object reports Ready is a slot the cluster has
	// positively observed as powered on. NotReady is deliberately not evidence of anything here -
	// it is the weak observation core.applyObservedPowerState spells out (a machine that is booting,
	// a machine that is going off, and a machine that is up with a dead kubelet all spell it) - so
	// the rule that corrects a progress on the strength of an observed on only reads the Ready side.
	nodeReady bool

	// lockHeld says a live coordination lock names an operation that owns the slot; lockNote
	// describes it for the log.
	lockHeld bool
	lockNote string

	// jobs are the slot's lifecycle Jobs.
	jobs []batchv1.Job

	// startupAttempts is how many startup attempts this slot has made in its current claim episode,
	// read off the record the FSM owns (fsm.StartupAttemptsOf). This package never writes that number:
	// it is incremented where an attempt begins and cleared where a boot passes the readiness gate.
	startupAttempts int

	// shutdownAttempts is how many shutdown attempts this slot has spent, read off the record the FSM
	// owns (fsm.ShutdownAttemptsOf). Only the observed-power rule reads it: it counts the shutdowns
	// whose completion this pool's script claimed and the observation refused, and it is what that
	// rule's bound and its exponential backoff are measured in (observedPowerRetryBackoff, see
	// observed_power.go). Like the startup count, this package never writes it.
	shutdownAttempts int

	// now and grace are the clock and the window the Job gates are measured against.
	now   time.Time
	grace time.Duration
}

// observe pays for the reads of one pass: the slot's node object, its lifecycle Jobs, its
// coordination lock, and its startup- and shutdown-attempt records. The cheap in-memory gates live in
// decide, so this is only ever called for a slot that is a candidate. Every read is paid for once and
// reported: an error means the observation is incomplete, and neither rule writes on an observation
// it could not finish (see Settle and ReconcileObservedPower).
func observe(ctx context.Context, c client.Client, coord fsm.CoordinationManager, slot *infrav1alpha1.Node, grace time.Duration) (observations, error) {
	obs := observations{now: time.Now(), grace: grace}

	kubeNode, err := kubernetesNode(ctx, c, slot)
	if err != nil {
		return observations{}, err
	}

	if kubeNode != nil {
		obs.nodePresent = true
		obs.nodeCordoned = kubeNode.Spec.Unschedulable
		obs.nodeMarked = hasAutoscalerMarker(kubeNode)
		obs.nodeReady = isReady(kubeNode)
	}

	jobs, err := lifecycleJobs(ctx, c, slot)
	if err != nil {
		return observations{}, err
	}

	obs.jobs = jobs
	obs.lockHeld, obs.lockNote = lockIsHeld(ctx, coord, slot)
	obs.startupAttempts = fsm.StartupAttemptsOf(slot)
	obs.shutdownAttempts = fsm.ShutdownAttemptsOf(slot)

	return obs, nil
}

// settlementNote renders the Warning's message for a settled slot: the progress that was replaced,
// the evidence that settled it, and the values it was settled to.
func settlementNote(slot *infrav1alpha1.Node, previous infrav1alpha1.Progress) string {
	return fmt.Sprintf(
		"Reconciled status.progress of node %s from %q to %q (status.powerState %q): spec.desiredPowerState is %q and "+
			"the cluster has no Kubernetes node %q and no startup/shutdown Job for the slot, so nothing is running "+
			"the transition the progress describes",
		slot.Name, string(previous), string(infrav1alpha1.ProgressShutdown), string(infrav1alpha1.PowerStateOff),
		string(infrav1alpha1.PowerStateOff), kubernetesNodeName(slot))
}

// rearmNote renders the Normal event of a re-arm, naming the startup attempt the arming leads to: the
// operator sees that the pool is retrying a machine that never booted, which attempt of the bound this
// is, and that the claim is untouched. Normal, not Warning: a re-arm is the system healing itself, and
// the shape worth a Warning is the one where it stops (see rearmLimitNote). The count is the FSM's
// record (fsm.StartupAttemptsOf), read here, not written.
func rearmNote(slot *infrav1alpha1.Node, previous infrav1alpha1.Progress, attempt int) string {
	return fmt.Sprintf(
		"Re-armed the claim on node %s (this starts startup attempt %d of %d): status.progress %q named a machine the "+
			"cluster does not have, so it is recorded %q with status.powerState %q while spec.desiredPowerState stays %q "+
			"- the startup Job of this slot finished and no Kubernetes node %q ever registered, so the machine has to be "+
			"started again and the slot is now in the state that allows it. The attempt count is recorded on the slot and "+
			"is cleared by a boot that passes the readiness gate.",
		slot.Name, attempt, ClaimRearmLimit, string(previous), string(infrav1alpha1.ProgressShutdown),
		string(infrav1alpha1.PowerStateOff), string(infrav1alpha1.PowerStateOn), kubernetesNodeName(slot))
}

// rearmLimitNote renders the Warning that closes the loop: a claimed slot whose startup attempts are
// used up. The slot is settled to the stable off/shutdown pair, so the group stops reporting a machine
// the cluster does not have, and the claim stays, so the request the autoscaler made is not answered by
// a watchdog. What is left is a human looking at why this slot cannot boot, and the message says so,
// with the attempts it spent and the rule that would reset them.
func rearmLimitNote(slot *infrav1alpha1.Node, attempts int) string {
	return fmt.Sprintf(
		"Claim on node %s is not recoverable by re-arming: %d of %d startup attempts used, no Kubernetes node %q ever "+
			"registered. The slot is settled to status.progress %q with status.powerState %q, so it no longer reports a "+
			"machine the cluster does not have, and spec.desiredPowerState stays %q, so the request is not dropped here. "+
			"The controller will not start another machine for this slot: scale the group down to drop the claim, or fix "+
			"what keeps the machine from booting (startup command, image, power API credentials) - a boot that passes the "+
			"readiness gate clears the record and the %d attempts with it.",
		slot.Name, attempts, ClaimRearmLimit, kubernetesNodeName(slot), string(infrav1alpha1.ProgressShutdown),
		string(infrav1alpha1.PowerStateOff), string(slot.Spec.DesiredPowerState), ClaimRearmLimit)
}

// uncordonNote renders the event of the cordon safety net: which node was made schedulable, and why
// the controller - which does not normally touch a machine's schedulability outside a lifecycle
// operation - did it.
func uncordonNote(slot *infrav1alpha1.Node) string {
	return fmt.Sprintf(
		"Uncordoned Kubernetes node %s: the slot is at status.progress %q with spec.desiredPowerState %q and no "+
			"startup/shutdown Job, coordination lock, or deletion marker on it, so the cordon is the residue of an "+
			"operation that ended (a shutdown that timed out, a startup that rolled back). A cordoned node is skipped "+
			"by cluster-autoscaler's scale-down, so it is never drained, never deleted, and paid for.",
		kubernetesNodeName(slot), string(slot.Status.Progress), string(slot.Spec.DesiredPowerState))
}

// kubernetesNodeName renders spec.kubernetesNodeName for a note, spelled out when empty so a message
// never carries a blank field.
func kubernetesNodeName(slot *infrav1alpha1.Node) string {
	if slot.Spec.KubernetesNodeName == "" {
		return "none"
	}

	return slot.Spec.KubernetesNodeName
}

// decision is the rule with no I/O in it, so the rule is testable on its own.
type decision struct {
	// action is what the pass must do, ActionNone when it must do nothing.
	action Action
	// requeueAfter is > 0 when the slot was left alone for a reason with a deadline (a
	// lifecycle Job in flight, or one that finished inside the grace), so the caller schedules
	// the look that acts on it.
	requeueAfter time.Duration
	// reason is the human-readable explanation for the log line.
	reason string
	// limited is true when the settlement is the startup-attempt bound: the claim of this slot has
	// used its attempts, so it is settled rather than re-armed and the write stamps the note. It is a
	// field, not a string match on the reason, because the limit path has to stamp its note exactly
	// once and has to re-derive the shape from the object read back - a comparison against a formatted
	// sentence is how a reworded log line breaks a deduplication.
	limited bool
	// previous is the progress being replaced, set when the action writes a progress.
	previous infrav1alpha1.Progress
}

// decide applies the rule to what the caller observed. See Settle for the rules and for why each
// gate exists. Every action is behind every gate: the Job gates and the lock gate own the
// transitional progress of an operation in flight, and the node-object gate decides between the
// claim, the settlement and the cordon fix, so the three cannot drift apart.
func decide(slot *infrav1alpha1.Node, obs observations) decision {
	if !impliesMachine(slot.Status.Progress) {
		return decision{reason: fmt.Sprintf("status.progress %q does not imply a machine", slot.Status.Progress)}
	}

	if obs.lockHeld {
		return decision{reason: "a coordination lock names an operation that owns the slot"}
	}

	inFlight, wait := jobWindow(obs)
	if inFlight != "" {
		// Pending or running: the operation is alive and its monitor is the writer. Look again on
		// the grace, so a monitor that died with its manager is still caught.
		return decision{requeueAfter: obs.grace, reason: fmt.Sprintf("lifecycle Job %s is in flight", inFlight)}
	}

	if wait > 0 {
		return decision{requeueAfter: wait, reason: "a lifecycle Job of the slot finished inside the settle grace"}
	}

	if len(obs.jobs) == 0 {
		// No lifecycle Job object at all. That is the normal shape of a slot whose startup Job was
		// collected by its TTL (spec.ttlSecondsAfterFinished, 600s by default) with its machine never
		// registered, and of the window in which the FSM has written the transitional progress and is
		// on its way to create the Job. With no Job, the grace has nothing to be measured against, and
		// the slot carries the one timestamp a startup in flight stamps: status.lastStartupTime, written
		// by the FSM's own progress write on the transition into startingup. Measuring from it is what
		// keeps a short ttlSecondsAfterFinished from pulling the gate inside a hook that is still
		// running: a slot whose startup is younger than the grace is looked at again on the rest of the
		// grace, and a slot older than it is the shape this package acts on. A slot with no stamp at all
		// (it never started a machine) is not held by a deadline it has no evidence for.
		if remaining, ok := startupGrace(slot, obs); ok {
			return decision{requeueAfter: remaining,
				reason: "the slot's startup is inside the settle grace, measured from status.lastStartupTime: no lifecycle Job object is left to measure against"}
		}
	}

	// Nothing is running on the slot. What the claim and the node object say decides the action.
	if slot.Spec.DesiredPowerState == infrav1alpha1.PowerStateOff {
		if obs.nodePresent {
			return decision{reason: fmt.Sprintf("the slot has a Kubernetes node %q, its status belongs to the observation controllers",
				slot.Spec.KubernetesNodeName)}
		}

		return decision{
			action:   ActionSettle,
			previous: slot.Status.Progress,
			reason:   "no Kubernetes node, no lifecycle Job, no coordination lock",
		}
	}

	// A claim. The progress is not the request and is not the machine, so it is the thing that is
	// wrong: from ready the FSM cannot StartNode, and from shutdown it can.
	if !obs.nodePresent {
		if obs.startupAttempts >= ClaimRearmLimit {
			// The attempts of this claim episode are spent. The slot is not re-armed: re-arming makes
			// StartNode legal, and the FSM refuses that start, so the arming would buy nothing. It is
			// settled to the same stable off/shutdown pair the desired-off branch writes, claim kept, and
			// the write carries the note that says the controller stopped.
			//
			// What that closes is the progress, and it matters because the progress is what every other
			// writer reads: a slot at ready with no machine is a slot the FSM cannot start (StartNode is
			// illegal from ready), cannot shut down without building a shutdown Job for a server that is
			// already gone, and cannot be healed by any other writer in the system. From the settled pair
			// the FSM can start again the moment an operator fixes the template, and a scale-down lands on
			// a slot that is already off.
			//
			// What it does not change is the arithmetic: spec.desiredPowerState is the request, and
			// countsTowardsTargetSize counts a claimed slot whatever its progress says, so the slot stays
			// counted as capacity the pool has been asked for. Dropping that is the provider's
			// NodeGroupDecreaseTargetSize / DeleteNodes, never a watchdog's answer to a scale-up; the one
			// Warning is what tells an operator the request is standing and will not be filled.
			return decision{
				action:   ActionSettle,
				limited:  true,
				previous: slot.Status.Progress,
				reason: fmt.Sprintf("the slot has used its %d startup attempts (limit %d): the claim is settled, not re-armed, and only an operator can say why the machine will not boot",
					obs.startupAttempts, ClaimRearmLimit),
			}
		}

		return decision{
			action:   ActionRearm,
			previous: slot.Status.Progress,
			reason:   "a claim whose startup finished with no Kubernetes node behind it",
		}
	}

	// The slot has a machine. Its status belongs to the observation controllers, with one
	// exception: a ready slot that is wanted on and is cordoned with nothing running on it. The
	// paths that land in ready without the uncordon hook (JobTimeout, JobFailed) leave exactly
	// that, and a cordoned node is skipped by cluster-autoscaler's scale-down forever.
	if slot.Status.Progress == infrav1alpha1.ProgressReady && obs.nodeCordoned && !obs.nodeMarked {
		return decision{
			action: ActionUncordon,
			reason: "a claimed ready slot whose node is cordoned with no Job, lock, or autoscaler marker behind it",
		}
	}

	return decision{reason: fmt.Sprintf("the slot has a Kubernetes node %q, its status belongs to the observation controllers",
		slot.Spec.KubernetesNodeName)}
}

// impliesMachine reports whether a progress value says there is a machine behind the slot. The
// settled answer for a slot that is off (shutdown, and the empty progress of a slot that was
// never seeded) is not one, which is what makes a pass over an already settled slot a no-op.
func impliesMachine(progress infrav1alpha1.Progress) bool {
	switch progress {
	case infrav1alpha1.ProgressReady, infrav1alpha1.ProgressStartingUp, infrav1alpha1.ProgressShuttingDown:
		return true
	default:
		return false
	}
}

// jobWindow is the lifecycle-Job half of the gate the two rules of this package share. It reports
// the name of a Job that is pending or running - an operation in flight, whose monitor is the
// writer of the slot's progress - and the time left of the settle grace of the finished Job whose
// grace expires soonest, the tightest deadline of the Jobs of the slot.
//
// It is one function because both rules have to defer to the same evidence: the rule that settles a
// progress that outlived its machine (Settle) and the rule that corrects a progress that the
// observed power contradicts (ReconcileObservedPower) must never disagree about whether an
// operation owns the slot, and a grace period that one rule measures and the other does not is how
// a controller writes over a transition its owner is about to record.
func jobWindow(obs observations) (inFlight string, graceRemaining time.Duration) {
	for i := range obs.jobs {
		job := &obs.jobs[i]

		completed, finished := finishedAt(job)
		if !finished {
			return job.Name, 0
		}

		remaining := obs.grace - obs.now.Sub(completed)
		if remaining <= 0 {
			continue
		}

		if graceRemaining == 0 || remaining < graceRemaining {
			graceRemaining = remaining
		}
	}

	return "", graceRemaining
}

// startupGrace measures the settle grace of a slot that has no lifecycle Job object to measure
// against, from status.lastStartupTime: the timestamp the FSM stamps on the transition into
// startingup, in the same status write that records the transitional progress. It is a fallback, not
// a second gate: the Job object, when there is one, is the evidence the grace is measured from, and
// the absence of one is not evidence that the operation is over - a Job collected by a short
// spec.ttlSecondsAfterFinished leaves the slot exactly as a startup in flight leaves it, with the
// after_JobCompleted hook of that Job minutes from writing the progress the hook owns. Stomping that
// progress is the duplicate scale-up the grace exists to prevent, so a slot with a startup stamp
// inside the grace is looked at again on the rest of it, and only a slot with no stamp at all (it
// never started a machine) or a stamp older than the grace is acted on now.
func startupGrace(slot *infrav1alpha1.Node, obs observations) (time.Duration, bool) {
	if slot.Status.LastStartupTime == nil {
		return 0, false
	}

	remaining := obs.grace - obs.now.Sub(slot.Status.LastStartupTime.Time)
	if remaining <= 0 {
		return 0, false
	}

	return remaining, true
}

// finishedAt reports when a finished lifecycle Job finished, and whether it is finished at all.
// "Finished" is defined the way fsm.monitorJobCompletion defines it (a succeeded or a failed pod
// counter), so the watchdog and the monitor agree on what a Job that is done looks like.
//
// The completion time is preferred, then the Job's own conditions, then its start time, then its
// creation timestamp. The last two are fallbacks, not noise: a Job whose controller never stamped
// a completion time still has to age out of the grace, and the creation timestamp is the oldest
// defensible bound, so a slot is never frozen by a Job it cannot measure.
func finishedAt(job *batchv1.Job) (time.Time, bool) {
	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		return time.Time{}, false
	}

	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time, true
	}

	for _, cond := range job.Status.Conditions {
		if cond.Type != batchv1.JobComplete && cond.Type != batchv1.JobFailed {
			continue
		}

		if cond.Status == corev1.ConditionTrue && !cond.LastTransitionTime.IsZero() {
			return cond.LastTransitionTime.Time, true
		}
	}

	if job.Status.StartTime != nil {
		return job.Status.StartTime.Time, true
	}

	return job.CreationTimestamp.Time, true
}

// apply writes the decision, re-deriving the whole rule from the object read back. A write that
// goes through on a slot someone else changed in the meantime is how a controller stomps an
// operation, so the rule is evaluated against the fresh copy and the write is skipped when the slot
// no longer qualifies, or qualifies for a different action - or a different limit - than the one
// decided. It reports whether it wrote, the progress it replaced, and the startup-attempt count the
// slot carries. That count is read off the record the FSM owns and is never written here: the
// watchdog arms attempts, it does not spend them, and a count this package incremented for its own
// passes is the lifetime bound that froze healthy slots.
//
// One function for every action on purpose. The settlement of a desired-off slot, the settlement of a
// claimed slot whose attempts are spent (the same pair, plus the note that says the controller
// stopped), the re-arm (the same pair, no note, the claim kept) and the cordon fix (a write on the
// node object of a slot whose progress is already the truth) share one body, so the gates of the four
// cannot drift apart. Splitting them is the failure this package exists to avoid.
func apply(ctx context.Context, c client.Client, coord fsm.CoordinationManager, slot *infrav1alpha1.Node, d decision, grace time.Duration) (bool, infrav1alpha1.Progress, int, error) {
	var (
		wrote    bool
		previous infrav1alpha1.Progress
		attempts int
	)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(slot), fresh); err != nil {
			return err
		}

		obs, err := observe(ctx, c, coord, fresh, grace)
		if err != nil {
			return err
		}

		current := decide(fresh, obs)
		if current.action != d.action || current.limited != d.limited {
			// The slot stopped qualifying, became a different action, or its attempts are not spent
			// any more: an operation, a claim, a machine, a cordon, or a boot that reset the
			// startup-attempt record arrived between the decision and this write.
			return nil
		}

		if d.action == ActionUncordon {
			// The progress of this slot is the truth, the cordon on its node is not: the write is on
			// the node object, and the slot is not touched at all.
			wrote, err = uncordonNode(ctx, c, fresh.Spec.KubernetesNodeName)
			if err != nil {
				return err
			}

			syncLocalCopy(slot, fresh)

			return nil
		}

		fresh.Status.Progress = infrav1alpha1.ProgressShutdown
		fresh.Status.PowerState = infrav1alpha1.PowerStateOff

		if d.limited {
			// The note goes on in the same status update as the settlement it belongs to. It is the
			// bound, the deduplication of the Warning, and the explanation an operator reads on
			// `kubectl describe` a week later; a slot settled without it is a slot whose next pass
			// notes it again, which is the event storm the record exists to prevent, and a note on a
			// slot that was never settled is a record that lies about the progress.
			fsm.StampStartupAttemptLimit(fresh, obs.startupAttempts, settleLimitCause(fresh))
		}

		if err := c.Status().Update(ctx, fresh); err != nil {
			return err
		}

		// spec is never written: the claim of a settled, re-armed or limited slot stays on, and that
		// is the request the provider made. Dropping a claim is the autoscaler's arithmetic
		// (NodeGroupDecreaseTargetSize), never a watchdog's answer to a scale-up.

		wrote = true
		previous = current.previous
		attempts = obs.startupAttempts

		syncLocalCopy(slot, fresh)

		return nil
	})
	if err != nil {
		return false, "", 0, err
	}

	return wrote, previous, attempts, nil
}

// settleLimitCause renders the evidence the note quotes when the watchdog is the writer. It is a
// different sentence from the one the FSM's refusal quotes ("the startup Jobs exited non-zero" and
// "no node ever registered" are different failures with the same bound), and the record has to say
// which loop was closed: this one is the quiet loop, a claim whose machines were started and never
// registered.
func settleLimitCause(slot *infrav1alpha1.Node) string {
	return fmt.Sprintf("no Kubernetes node %q ever registered for the machines those attempts started",
		kubernetesNodeName(slot))
}

// syncLocalCopy keeps the caller's copy of the slot in step with what was written, so the pass that
// acted does not act again on the values it replaced: the FSM reads its state from status.progress,
// the limit note is read back as the deduplication of the Warning, and the node controller has just
// run this pass with this object in hand. Conditions are carried over for the same reason the
// progress is - a caller holding a copy without the note the write stamped is a caller that writes
// the note again.
func syncLocalCopy(slot *infrav1alpha1.Node, fresh *infrav1alpha1.Node) {
	slot.Status.Progress = fresh.Status.Progress
	slot.Status.PowerState = fresh.Status.PowerState
	slot.Status.Conditions = fresh.Status.Conditions
}

// hasAutoscalerMarker reports whether cluster-autoscaler has put one of its deletion markers on a
// node. A marker is a decision the autoscaler made and owns: it cordons the node it is draining, so
// the cordon safety net leaves that alone. The keys are the autoscaler's (taints.IsProtectedKey
// refuses to let spec.nodeTaints write them), and they are matched here rather than imported from
// the marker recorder so this package does not depend on what that one records.
func hasAutoscalerMarker(kubeNode *corev1.Node) bool {
	for _, taint := range kubeNode.Spec.Taints {
		if taint.Key == taints.ToBeDeletedByClusterAutoscaler ||
			taint.Key == taints.DeletionCandidateOfClusterAutoscaler {
			return true
		}
	}

	return false
}

// uncordonNode clears spec.unschedulable of a Kubernetes node, and the node.kubernetes.io/
// unschedulable taint that goes with a cordon, through a merge patch: a write that changes nothing
// sends nothing. It reports whether it wrote, so a slot whose node was un-cordoned by the kubelet
// between the read and the patch records no event.
//
// This is the safety net's own write, deliberately narrow: the scale-up path's uncordon is
// fsm.setNodeSchedulableInternal, which also runs inside an operation that owns the slot.
func uncordonNode(ctx context.Context, c client.Client, nodeName string) (bool, error) {
	if nodeName == "" {
		return false, nil
	}

	kubeNode := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, kubeNode); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("get kubernetes node %s: %w", nodeName, err)
	}

	if !kubeNode.Spec.Unschedulable && !hasUnschedulableTaint(kubeNode) {
		return false, nil
	}

	patch := client.MergeFrom(kubeNode.DeepCopy())

	kubeNode.Spec.Unschedulable = false

	if hasUnschedulableTaint(kubeNode) {
		kept := make([]corev1.Taint, 0, len(kubeNode.Spec.Taints))

		for _, taint := range kubeNode.Spec.Taints {
			if taint.Key == taints.NodeUnschedulable {
				continue
			}

			kept = append(kept, taint)
		}

		kubeNode.Spec.Taints = kept
	}

	if err := c.Patch(ctx, kubeNode, patch); err != nil {
		return false, fmt.Errorf("uncordon kubernetes node %s: %w", nodeName, err)
	}

	return true, nil
}

// isReady reports the Ready condition of a Kubernetes node: true only when the node says
// NodeReady=True. It is the same reading core.KubernetesNodeReconciler.getNodeStatus uses to write
// status.powerState: on, so the two rules observe the machine identically: a node that is not Ready
// is not evidence that a machine is up, and a node object that is not Ready is not evidence that a
// machine is gone either (see kubernetesNode).
func isReady(kubeNode *corev1.Node) bool {
	for _, cond := range kubeNode.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}

	return false
}

// hasUnschedulableTaint reports whether the kubelet's cordon taint is on the node.
func hasUnschedulableTaint(kubeNode *corev1.Node) bool {
	for _, taint := range kubeNode.Spec.Taints {
		if taint.Key == taints.NodeUnschedulable {
			return true
		}
	}

	return false
}

// kubernetesNode returns the Kubernetes node of a slot, and nil when it has none. The slot has none
// when it was never provisioned (no spec.kubernetesNodeName) and when the object is gone, which is
// the evidence the watchdog acts on.
//
// Any object counts, Ready or not: a node object is a machine, and the status of a slot with one is
// owned by the observation controllers (core.applyObservedPowerState spells out why NotReady is a
// weak observation of a machine, and never evidence that a machine is gone). An API error other than
// NotFound is returned: not knowing whether the machine is there is not evidence that it is not, and
// a settlement written from a failed read is a settlement written against a live machine.
func kubernetesNode(ctx context.Context, c client.Client, slot *infrav1alpha1.Node) (*corev1.Node, error) {
	if slot.Spec.KubernetesNodeName == "" {
		// The slot has never been provisioned, so there is no machine to look for.
		return nil, nil
	}

	kubeNode := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: slot.Spec.KubernetesNodeName}, kubeNode); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get kubernetes node %s: %w", slot.Spec.KubernetesNodeName, err)
	}

	return kubeNode, nil
}

// lifecycleJobs lists the startup and shutdown Jobs of the slot.
//
// Provenance is matched on both labels the FSM stamps - the CR name (fsm.LabelNodeCR) and the
// legacy Kubernetes node name (fsm.LabelNodeLegacy) - because the two can differ and because
// Jobs written before the CR-name label existed only carry the legacy one. A Job that cannot be
// found is a slot settled in front of its own operation, which is the one failure mode of this
// package that matters, so the match is deliberately generous: same app label, same namespace,
// and one of the two names.
func lifecycleJobs(ctx context.Context, c client.Client, slot *infrav1alpha1.Node) ([]batchv1.Job, error) {
	list := &batchv1.JobList{}

	opts := &client.ListOptions{
		Namespace:     slot.Namespace,
		LabelSelector: labels.SelectorFromSet(labels.Set{fsm.LabelApp: fsm.LifecycleAppName}),
	}

	if err := c.List(ctx, list, opts); err != nil {
		return nil, fmt.Errorf("list lifecycle jobs in namespace %s: %w", slot.Namespace, err)
	}

	var owned []batchv1.Job

	for i := range list.Items {
		job := &list.Items[i]

		if phase := job.Labels[fsm.LabelType]; phase != fsm.PhaseStartup && phase != fsm.PhaseShutdown {
			continue
		}

		if job.Labels[fsm.LabelNodeCR] == slot.Name ||
			(slot.Spec.KubernetesNodeName != "" && job.Labels[fsm.LabelNodeLegacy] == slot.Spec.KubernetesNodeName) {
			owned = append(owned, *job)
		}
	}

	return owned, nil
}

// lockIsHeld reports whether a live coordination lock names an operation that owns the slot, and
// returns a note describing it. An expired lock is not held: the acquisition path takes it over
// and the node controller cleans it up, so a slot frozen behind a lock nothing is running under
// is one of the shapes the watchdog exists for.
func lockIsHeld(ctx context.Context, coord fsm.CoordinationManager, slot *infrav1alpha1.Node) (bool, string) {
	lock, exists := coord.CheckLock(ctx, slot)
	if !exists || coord.IsLockExpired(lock) {
		return false, ""
	}

	return true, fmt.Sprintf("operation=%s owner=%s age=%v", lock.Operation, lock.Owner, time.Since(lock.Timestamp))
}
