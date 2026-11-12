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

package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	autoscalerpkg "github.com/homecluster-dev/homelab-autoscaler/internal/autoscaler"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// idleResync is how long a stable slot waits before the slot reconciler looks at it again: a slot
// that is doing nothing (free, or serving) has no deadline of its own, and the resync is what
// notices a machine that was powered off out of band.
const idleResync = 5 * time.Minute

// refusedResync is the pacing of a scale-down the model has stopped retrying: the bound has been
// reached, the record is on the slot, and the pass that comes back finds the same machine.
const refusedResync = 5 * time.Minute

// liveClaimOf reads the live claim of a slot: the one claim in pending, booting or ready that names
// it. A claim in a terminal phase is not live, and a terminal claim whose machine is observed up is
// the "failed claim + machine on" case of the input law, not a live claim.
func (r *NodeReconciler) liveClaimOf(ctx context.Context, node *infrav2alpha1.Node) (*infrav2alpha1.NodeClaim, error) {
	claims, err := claim.ListOfSlot(ctx, r.Client, node.Namespace, node.Name)
	if err != nil {
		return nil, err
	}

	return claim.LiveOfSlot(claims), nil
}

// lifecycleInput assembles the state machine's input for one pass over one slot:
//
//	(live claim phase, observed power, kubelet liveness / node readiness)
//
// plus the two lifecycle-Job facts that separate "claimed, nothing started" from "a startup is in
// flight". It reads the claim, the corev1.Node, and the Jobs labelled with the slot; it never reads
// Node.status.phase, which is what this pass writes.
func (r *NodeReconciler) lifecycleInput(ctx context.Context, node *infrav2alpha1.Node, live *infrav2alpha1.NodeClaim) phase.Input {
	input := phase.Input{Claim: live, ObservedPower: node.Status.PowerState}

	if name := node.Spec.KubernetesNodeName; name != "" {
		kubeNode := &corev1.Node{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, kubeNode); err == nil {
			input.NodeUsable = phase.Unusable(kubeNode, autoscalerpkg.ReadinessTaintKeysOfSlot(node)) == ""
		}
	}

	input.StartupJobInFlight, input.ShutdownJobInFlight = r.lifecycleJobsInFlight(ctx, node)

	return input
}

// lifecycleJobsInFlight reports whether a startup Job and whether a shutdown Job of this slot is
// running. The lifecycle Jobs carry the slot's name and the lifecycle phase as labels
// (fsm.LabelNodeCR, fsm.LabelPhase), so this is one list, and the answer is a fact about objects
// that exist, not about a string someone wrote on a slot.
func (r *NodeReconciler) lifecycleJobsInFlight(ctx context.Context, node *infrav2alpha1.Node) (bool, bool) {
	jobList := &batchv1.JobList{}
	selector := labels.SelectorFromSet(labels.Set{fsm.LabelNodeCR: node.Name})

	if err := r.List(ctx, jobList, &client.ListOptions{Namespace: node.Namespace, LabelSelector: selector}); err != nil {
		log.FromContext(ctx).V(1).Info("Failed to list the lifecycle Jobs of the slot",
			"node", node.Name, "error", err.Error())

		return false, false
	}

	var startup, shutdown bool

	for i := range jobList.Items {
		job := &jobList.Items[i]
		if !job.DeletionTimestamp.IsZero() || job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			continue
		}

		switch job.Labels[fsm.LabelPhase] {
		case fsm.PhaseStartup:
			startup = true
		case fsm.PhaseShutdown:
			shutdown = true
		}
	}

	return startup, shutdown
}

// recoverFinishedLifecycleJob re-fires the lifecycle event of a Job that finished with nobody
// watching, and reports whether it did.
//
// The monitor of a lifecycle Job is a goroutine of the process that ran the transition: it holds the
// coordination lock, reads the Job, and fires JobCompleted / JobFailed. A manager that restarts in
// that window loses the goroutine and keeps the objects, and nothing re-attaches - a booting claim
// whose startup Job completed during the restart has no event left to fire, sits at booting until its
// boot deadline fails it, and the machine that booted successfully is then force-shut-down. Same for
// a shutdown Job that completed: the scale-down's finalizer never releases.
//
// The Job is the fact, so the reconcile reads it - and the Job is read *as this claim's Job*. A
// finished Job of the right phase on the right slot is not evidence about the operation running
// now, and firing its event is: a `ready` claim whose machine is momentarily observed off derives
// `startingup` exactly like a real boot, and the recovery of its long-consumed startup Job runs the
// readiness gate again on a machine that is already serving, failing the claim with
// ReadinessGateTimeout; a `booting` claim whose own Job is still running is failed by an older
// finished Job of the same phase. So the recovery is gated on the claim and on the Job:
//
//  1. a live claim exists, and the operation is one it owns: the startup recovery runs only inside
//     the boot window (phase.InBootWindow - the window the deadline bounds and the readiness gate
//     answers for), and the shutdown recovery only for a claim a scale-down has named;
//  2. the *newest* Job of the wanted phase is finished: a newer unfinished one means the operation
//     is in flight, and its monitor owns its event. The newest, not the newest finished - an older
//     finished Job of the same phase is the record of an earlier attempt;
//  3. that Job does not predate the operation: a startup Job is not older than the moment the claim
//     committed its boot (claim.BootCommittedAt), a shutdown Job not older than the moment the
//     scale-down named the claim (claim.ScaleDownRequestedAt);
//  4. no coordination lock is held and unexpired: a live lock means an in-process monitor owns this
//     operation, and stealing its event is the race the lock exists to close. A lock the restart left
//     behind expires by itself (fsm.DefaultLockTimeout, cleaned at the top of the pass), so the
//     recovery runs within one lock timeout of the restart - far inside the claim's deadline.
//
// Gates 1-3 are why the shutdown branch is inert in the normal shape: phase.State derives
// shuttingdown only while a shutdown Job is in flight, and gate 2 then refuses - the pass has no
// evidence to re-fire, and law 3 owns the restart case by firing the shutdown again. It stays
// because the branch is the one that lands the completion of a shutdown this process started.
func (r *NodeReconciler) recoverFinishedLifecycleJob(
	ctx context.Context,
	node *infrav2alpha1.Node,
	stateMachine *fsm.NodeStateMachine,
	state string,
	live *infrav2alpha1.NodeClaim,
) (bool, error) {
	var wantPhase string

	switch state {
	case phase.StateStartingUp:
		wantPhase = fsm.PhaseStartup
	case phase.StateShuttingDown:
		wantPhase = fsm.PhaseShutdown
	default:
		// ready and shutdown are stable states: nothing is waiting on a Job, and a finished Job of a
		// bygone operation is not evidence about this one.
		return false, nil
	}

	// Gate 1: the recovery answers for the operation of this slot's live claim. No live claim means
	// no operation is running to re-attach to - law 1 boots the slot and law 3 powers it off, and
	// neither is waiting on an event.
	if live == nil {
		return false, nil
	}

	if wantPhase == fsm.PhaseStartup && !phase.InBootWindow(live.Status.Phase) {
		// The claim is ready: its boot was consumed by the readiness gate long ago, and the shape the
		// law derived (booting, from a machine momentarily observed off) is a machine being *kept*,
		// not a machine being bought. The case belongs to claim.watchMachine, bounded by an
		// observation of the loss, not to a Job of an attempt that ended.
		return false, nil
	}

	if wantPhase == fsm.PhaseShutdown && live.DeletionTimestamp.IsZero() {
		// No scale-down has named this claim, so no scale-down owns a shutdown Job to recover.
		return false, nil
	}

	if lock, held := r.checkOperationLock(ctx, node); held && !r.isLockExpired(lock) {
		// An operation owns this slot in this process. Its own monitor fires its event.
		return false, nil
	}

	// Gates 2 and 3: the newest Job of the wanted phase, and only it, is this operation's.
	job := r.newestLifecycleJob(ctx, node, wantPhase)
	if job == nil {
		return false, nil
	}

	if !lifecycleJobFinished(job) {
		// The operation is in flight. Its monitor owns its event; an older finished Job of the same
		// phase is somebody else's attempt.
		return false, nil
	}

	if start := lifecycleEpisodeStart(wantPhase, live); !start.IsZero() && job.CreationTimestamp.Time.Before(start) {
		log.FromContext(ctx).V(1).Info("Not re-firing a finished lifecycle Job that predates the operation this slot is running",
			"node", node.Name, "job", job.Name, "phase", wantPhase,
			"jobCreated", job.CreationTimestamp.String(), "operationStarted", start.String())

		return false, nil
	}

	logger := log.FromContext(ctx)

	if job.Status.Succeeded > 0 {
		logger.Info("Re-firing the completion of a lifecycle Job that finished with nobody watching",
			"node", node.Name, "job", job.Name, "phase", wantPhase)

		return true, stateMachine.JobCompleted(job)
	}

	logger.Info("Re-firing the failure of a lifecycle Job that finished with nobody watching",
		"node", node.Name, "job", job.Name, "phase", wantPhase)

	return true, stateMachine.JobFailed()
}

// lifecycleEpisodeStart is the instant the operation a recovery answers for began, read off the
// claim that owns it: the boot commit for a startup, the scale-down request for a shutdown. The zero
// time says the claim carries no bound to compare against, and the caller does not refuse on it.
func lifecycleEpisodeStart(wantPhase string, live *infrav2alpha1.NodeClaim) time.Time {
	if wantPhase == fsm.PhaseShutdown {
		return claim.ScaleDownRequestedAt(live)
	}

	return claim.BootCommittedAt(live)
}

// lifecycleJobFinished reports whether the cluster stamped a terminal status on a lifecycle Job. A
// Job it did not is in flight, and its event belongs to the monitor that is watching it.
func lifecycleJobFinished(job *batchv1.Job) bool {
	return job != nil && (job.Status.Succeeded > 0 || job.Status.Failed > 0)
}

// newestLifecycleJob returns the newest lifecycle Job of a slot in the given phase
// (fsm.PhaseStartup / fsm.PhaseShutdown), finished or not, and nil when the slot has none. The
// newest is the operation: the recovery asks whether *it* has finished, so a Job that is running
// hides an older finished one instead of being ignored for it.
func (r *NodeReconciler) newestLifecycleJob(ctx context.Context, node *infrav2alpha1.Node, wantPhase string) *batchv1.Job {
	jobList := &batchv1.JobList{}
	selector := labels.SelectorFromSet(labels.Set{fsm.LabelNodeCR: node.Name})

	if err := r.List(ctx, jobList, &client.ListOptions{Namespace: node.Namespace, LabelSelector: selector}); err != nil {
		log.FromContext(ctx).V(1).Info("Failed to list the lifecycle Jobs of the slot",
			"node", node.Name, "error", err.Error())

		return nil
	}

	var newest *batchv1.Job

	for i := range jobList.Items {
		job := &jobList.Items[i]

		if job.Labels[fsm.LabelPhase] != wantPhase {
			continue
		}

		if !job.DeletionTimestamp.IsZero() {
			continue
		}

		if newest == nil || job.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = job
		}
	}

	return newest
}

// handleStateTransitionsWithFSM fires the lifecycle transition the input law asks for. The law, in
// the order it is applied (docs/design/nodeclaim.md §6):
//
//  1. a live claim with the machine off boots it;
//  2. a live claim, the machine on, and the node usable is ready: nothing to fire;
//  3. no live claim with the machine on shuts the machine down;
//  4. a failed claim with the machine on force-shuts the machine down (the same branch as 3:
//     a terminal claim is not live, which is what makes the case fall out of the law instead of
//     needing a rule of its own).
//
// A claim marked for scale-down (DeleteNodes named it) is in the shutdown branch too: the machine
// is wanted no longer, and the claim's finalizer is what keeps the request from being forgotten.
//
// The transitions run on the state machine, which takes the coordination lock in its before_ hook,
// so a scale-up and a scale-down of one slot cannot both take the slot apart (see
// internal/fsm/coordination.go, and docs/design/nodeclaim.md §7 on why the lock stays one more
// release as a belt over the claim).
func (r *NodeReconciler) handleStateTransitionsWithFSM(
	ctx context.Context,
	node *infrav2alpha1.Node,
	group *infrav2alpha1.Group,
	input phase.Input,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	live := input.LiveClaim()

	coordMgr := fsm.NewCoordinationManager(r.Client, fsm.NodeControllerOwner)

	stateMachine := fsm.NewNodeStateMachine(node, r.Client, r.Scheme, coordMgr,
		fsm.WithEventRecorder(r.EventRecorder),
		fsm.WithReadinessTaintKeys(r.readinessTaintKeys(ctx, node, group)),
		fsm.WithInput(input),
		fsm.WithClaim(live, claimWriterFor(r.Client, node)))

	state := stateMachine.GetCurrentState()

	logger.Info("Reconciling the lifecycle of a slot from its claim and its observation",
		"node", node.Name,
		"claim", claimNameOf(live),
		"claimPhase", phaseNameOf(live),
		"observedPower", string(input.ObservedPower),
		"nodeUsable", input.NodeUsable,
		"startupJob", input.StartupJobInFlight,
		"shutdownJob", input.ShutdownJobInFlight,
		"fsmState", state)

	// Law 1: a live claim with the machine off boots it. The state machine's StartNode transition
	// is the one that creates the startup Job, and it is reachable from shutdown, which is the
	// state a slot in that shape derives to.
	//
	// The want is the claim's, and a claim a scale-down marked is not a want: its machine is going
	// off, and booting it again is the loop the claim's finalizer exists to close (law 3 owns that
	// shape).
	if claim.WantsMachine(live) && !input.ObservedOn() && state == phase.StateShutdown {
		if err := stateMachine.StartNode(); err != nil {
			if result, deferred := deferTransitionOnLockConflict(ctx, node, err); deferred {
				return result, nil
			}

			// controller-runtime discards the Result when the error is non-nil, the requeue
			// backoff is decided by the controller, so only the error is returned here.
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// Laws 3 and 4, and the scale-down of a marked claim: the machine is observed up and nothing
	// wants it. state == ready is the state such a slot derives to, and ShutdownNode is the
	// transition reachable from it.
	if input.ObservedOn() && state == phase.StateReady && (live == nil || !live.DeletionTimestamp.IsZero()) {
		if exhausted, requeue := r.scaleDownStreakRefusal(ctx, node, live); exhausted {
			return requeue, nil
		}

		if err := stateMachine.ShutdownNode(); err != nil {
			if result, deferred := deferTransitionOnLockConflict(ctx, node, err); deferred {
				return result, nil
			}

			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// Law 2, and every transitional state: ready with a live claim and a usable node is capacity
	// serving pods; startingup is driven by the Job monitor of the machine that started it;
	// shuttingdown is driven by its own Job. Nothing fires here - except for the Job that finished
	// while the manager was not looking (recoverFinishedLifecycleJob), which is the same evidence
	// read off the same object.
	if handled, err := r.recoverFinishedLifecycleJob(ctx, node, stateMachine, state, live); err != nil {
		return ctrl.Result{}, err
	} else if handled {
		return ctrl.Result{}, nil
	}

	stateMachine.CheckStuckState()

	if fsm.IsTransitionalState(state) {
		return ctrl.Result{RequeueAfter: stateMachine.CalculateBackoff()}, nil
	}

	return ctrl.Result{RequeueAfter: idleResync}, nil
}

// scaleDownStreakRefusal reports that the model has stopped retrying the shutdown of a machine that
// will not go off, and the requeue that paces it. The bound is the streak of shutdown attempts
// (claim.MaxConsecutiveAttempts) counted on the scale-down claim of the slot - status.shutdownAttempts,
// a counter of its own, not the boot attempts of the episode - and stamped onto the slot's digest, so
// it survives the claim's collection. It replaces the v1alpha1 ShutdownAttempts condition on the
// long-lived slot, with its two reset sites (docs/design/nodeclaim.md §4).
//
// The streak is counted where a shutdown is fired: the state machine's ShutdownNode, on the path that
// created the shutdown Job (fsm.recordShutdownAttempt -> BumpScaleDownAttempt). A pass that only
// looked at the machine is not an attempt - the claim controller's finalizer poll is read-only - so
// three attempts are three shutdown Jobs, which is what makes the bound bound something.
//
// The refusal is a bounded pause, not a sentence: past claim.ShutdownAttemptResetGrace the streak is
// spent no longer and the shutdown fires again, so a pool whose shutdown script has been fixed is
// picked up without a hand edit, and the requeue below is paced to the grace so the resync is what
// notices. The two records of the pause are published on the claim that carries the count: the
// ShutdownAttemptLimitReached warning when this pass refuses, the ShutdownAttemptStreakReset warning
// on the pass that ends the pause and fires again.
//
// The refusal is not an error: the slot is up, the pool does not want it, and the pool's shutdown
// script is what fails. An operator who wants the next attempt now, and not at the grace, has the
// documented escape hatch of §4: zero status.shutdownAttempts on the claim, and the next pass fires.
func (r *NodeReconciler) scaleDownStreakRefusal(ctx context.Context, node *infrav2alpha1.Node, live *infrav2alpha1.NodeClaim) (bool, ctrl.Result) {
	// A scale-down that names a claim spends its attempts on that claim, and the shutdown path counts
	// them; the bound is applied here, so a shutdown script that exits 0 without powering the machine
	// off is not re-fired every resync. With no claim left to carry the count, the slot's digest is
	// the record of the episode that left the machine up.
	spent, left := false, time.Duration(0)

	if live != nil {
		spent, left = claim.ScaleDownStreakSpent(live)
	} else {
		spent, left = claim.ScaleDownStreakExhausted(node)
	}

	if !spent {
		// The pass that fires a shutdown whose streak was spent is the pass the pause ends on, and the
		// count restarts with the Job this pass creates. The record of the pause ending is published
		// once, here: the bump that lands in the same pass moves the counter off the bound.
		if live != nil && claim.ScaleDownStreakResetDue(live) {
			r.eventf(live, corev1.EventTypeWarning, "ShutdownAttemptStreakReset", fsm.ActionPowerOff,
				"the scale-down of slot %s spent its %d shutdown attempts and the reset grace of %s has passed: the machine is still observed up, so the controller fires the shutdown again - a pool whose script has been fixed is picked up without a hand edit",
				node.Name, claim.MaxConsecutiveAttempts, claim.ShutdownAttemptResetGrace.String())
		}

		return false, ctrl.Result{}
	}

	// The record the refusal points an operator at: the claim, when the scale-down named one, and
	// the slot's digest when it did not (the machine was left up by an episode that is already
	// collected).
	attempts, claimName, reason := int32(0), "", ""

	if live != nil {
		attempts, claimName, reason = live.Status.ShutdownAttempts, live.Name, live.Status.Reason
	} else if node.Status.LastClaim != nil {
		attempts, claimName, reason = node.Status.LastClaim.ShutdownAttempts, node.Status.LastClaim.Name, node.Status.LastClaim.Reason
	}

	log.FromContext(ctx).Info("Not firing the shutdown: this slot has used its scale-down attempts",
		"node", node.Name,
		"attempts", attempts,
		"limit", claim.MaxConsecutiveAttempts,
		"resetIn", left.String(),
		"claim", claimName,
		"reason", reason,
		"state", "the machine stays up, the slot keeps its report, and the record is on the claim")

	// The record an operator is pointed at, on the object that carries the count. A scale-down with no
	// claim to name has no object to record on: its record is the slot's digest, read above.
	if live != nil {
		r.eventf(live, corev1.EventTypeWarning, "ShutdownAttemptLimitReached", fsm.ActionPowerOff,
			"scale-down of slot %s has spent %d of %d shutdown attempts and the machine is still observed up: the controller stops firing the shutdown for %s, because a shutdown Job that exits 0 without the machine going off is the pool's script, not the controller",
			node.Name, claim.MaxConsecutiveAttempts, claim.MaxConsecutiveAttempts, claim.ShutdownAttemptResetGrace.String())
	}

	// Come back no later than the end of the reset grace, so the pause ends on a pass and not on a
	// resync that happens to land after it.
	requeue := refusedResync
	if left > 0 && left < requeue {
		requeue = left
	}

	return true, ctrl.Result{RequeueAfter: requeue}
}

// eventf publishes an Event on an object this reconciler has to report on, and is a no-op without a
// recorder (nil is what the unit tests build). The scale-down streak's records are published on the
// claim that carries the count, because the claim is the object an operator opens to read an episode.
func (r *NodeReconciler) eventf(obj runtime.Object, eventType, reason, action, format string, args ...interface{}) {
	if r.EventRecorder == nil || obj == nil {
		return
	}

	r.EventRecorder.Eventf(obj, nil, eventType, reason, action, format, args...)
}

// claimNameOf / phaseNameOf are the log renderings of a claim that may be nil.
func claimNameOf(c *infrav2alpha1.NodeClaim) string {
	if c == nil {
		return ""
	}

	return c.Name
}

func phaseNameOf(c *infrav2alpha1.NodeClaim) string {
	if c == nil {
		return ""
	}

	return string(c.Status.Phase)
}

// deferTransitionOnLockConflict turns a lifecycle transition that lost the coordination lock into a
// requeue at the deadline the lock itself names, and reports whether it did.
//
// The refused operation is not lost. Its intent lives in the live NodeClaim of the slot, which is
// what this reconciler drives the state machine from, so the very same branch that produced the
// transition runs again on the requeue and the shutdown fires as soon as the scale-up in flight
// releases the slot - no second DeleteNodes from the autoscaler, no operator intervention.
// Returning the error instead hands that retry to controller-runtime's rate-limited queue, and the
// only record that a shutdown was asked for at all is a warning on the Node CR.
//
// Only a lock conflict is deferred this way. Any other failure of a transition is a real failure and
// is returned as an error, so it keeps the controller's backoff, its log line and its metrics.
func deferTransitionOnLockConflict(ctx context.Context, node *infrav2alpha1.Node, err error) (ctrl.Result, bool) {
	conflict := fsm.AsLockConflict(err)
	if conflict == nil {
		return ctrl.Result{}, false
	}

	// The lifecycle transitions in fsm record the deferral as a Normal Scale*Deferred event on the
	// Node CR; the reason is the coordination lock, so the log says which operation owns the slot and
	// when the retry runs, at V(1): a collision between a scale-up and a scale-down is a scheduled
	// event on a homelab, not an incident.
	log.FromContext(ctx).V(1).Info("FSM transition deferred by the coordination lock",
		"node", node.Name,
		"deferredOperation", conflict.Operation,
		"heldOperation", conflict.HolderOperation(),
		"heldOwner", conflict.HolderOwner(),
		"requeueAfter", conflict.RetryAfter().String(),
		"reason", err.Error())

	return ctrl.Result{RequeueAfter: conflict.RetryAfter()}, true
}
