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
// This package is the one writer that settles such a slot, and it is deliberately narrow. It
// acts only on a slot that claims to have a machine (ready / startingup / shuttingdown), has no
// corev1.Node, has no lifecycle Job in flight, and whose spec.desiredPowerState asks for the
// machine to be off. It writes the stable off / shutdown pair the observation controllers agree
// on, and records a Warning event naming the progress it replaced. It never overrides a claim:
// settling a claimed slot that never booted is a different decision (abandoning the claim) and
// is not made here.
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
)

// SettleGracePeriod is how long a *finished* lifecycle Job keeps its slot out of reach of the
// watchdog.
//
// The Job is the operation and the state machine that started it is the writer that settles the
// progress it moved: fsm.monitorJobCompletion polls the Job and fires JobCompleted, whose
// after_ hook writes the stable progress. The monitor polls every 30s in production, and it
// runs in the goroutine StartNode / ShutdownNode spawned, so a Job that completed a second ago
// has not necessarily been acted on yet. Stomping the progress in that window writes over a
// transition its owner is about to record, and the FSM reads its state from progress, so the
// write is how a duplicate lifecycle Job - and a second scale-up - is born.
//
// The value is therefore a multiple of the monitor's 30s poll, and far longer than one reconcile
// (seconds), while staying well under the 5m Job timeout and the 15m stuck-transition threshold.
// A slot whose monitor is really gone - the manager restarted and the goroutine died with it -
// is settled within ~2 minutes, which is the point of having the watchdog at all.
const SettleGracePeriod = 2 * time.Minute

// Reason and action recorded on the Node CR when the watchdog settles a slot, so that
// `kubectl describe node.infra.homecluster.dev <name>` shows who moved status.progress and why.
const (
	// ReasonProgressReconciled is recorded for a status.progress that named a machine the
	// cluster does not have.
	ReasonProgressReconciled = "ProgressReconciled"
	// ActionSettleProgress is the action the settlement belongs to.
	ActionSettleProgress = "SettleProgress"
)

// Outcome reports what one pass of the watchdog decided, so the caller can log it and, where it
// has a say, come back at the deadline the watchdog names.
type Outcome struct {
	// Settled is true when status.progress was written.
	Settled bool

	// Previous is the progress the slot carried before the write; empty when nothing was
	// written.
	Previous infrav1alpha1.Progress

	// RequeueAfter is how long the caller should wait before looking at this slot again: > 0
	// when the slot was left alone because a lifecycle Job is in flight or is inside the grace
	// of finishing, so the settlement is not forgotten, it is scheduled.
	RequeueAfter time.Duration

	// Reason explains, for the log line, why nothing was settled.
	Reason string
}

// Settle reconciles status.progress of a slot with observed reality and reports what it did.
//
// The rules, and why each one is there:
//
//   - spec.desiredPowerState must be off. The claim is the request and the watchdog never
//     overrides it: a slot that is wanted on and reports ready with no machine is a startup that
//     has to be retried, not a slot to settle, and settling it would tell the provider the
//     requested capacity does not exist (see the package doc for the follow-up this leaves open).
//   - the progress must be one that implies a machine: ready, startingup, shuttingdown.
//     shutdown and the empty progress of a never-seeded slot are already the settled answer, so
//     a pass over them writes nothing, which is what keeps this idempotent.
//   - there must be no corev1.Node for the slot. A node object - Ready or not - is a machine, and
//     the observation controllers own the status of a slot that has one: core.
//     applyObservedPowerState is the writer for the NotReady and Ready windows of a real machine.
//   - there must be no lifecycle Job in flight, and a Job that finished must be older than grace.
//     The Job is the operation; the state machine that started it settles its progress, and the
//     grace covers the gap between a Job finishing and that machine acting on it.
//   - the slot must carry no live coordination lock. A lock says an operation owns the slot right
//     now - including the window in which the FSM has written the transitional progress and has
//     not created the Job yet, and the scale-down the provider is running.
//
// On a settlement the slot's off/shutdown pair is written through the status subresource, using
// the same read-fresh, re-decide, retry-on-conflict shape as fsm.updateNodeProgress, and a
// Warning event names the progress it replaced. The caller's copy of the slot is updated in
// place, so the pass that settled it does not act on the progress it just replaced.
//
// A nil recorder records nothing, which is what the unit tests use. grace <= 0 selects
// SettleGracePeriod.
func Settle(ctx context.Context, c client.Client, slot *infrav1alpha1.Node, recorder events.EventRecorder, grace time.Duration) (Outcome, error) {
	if grace <= 0 {
		grace = SettleGracePeriod
	}

	logger := log.FromContext(ctx).WithValues("node", slot.Name)
	coord := fsm.NewCoordinationManager(c, fsm.NodeControllerOwner)

	// Cheap gates first: the two reads below are only paid for a slot that is a candidate.
	if slot.Spec.DesiredPowerState != infrav1alpha1.PowerStateOff {
		return Outcome{Reason: "the slot is claimed (spec.desiredPowerState is not off), a claim is never overridden"}, nil
	}

	if !impliesMachine(slot.Status.Progress) {
		return Outcome{Reason: fmt.Sprintf("status.progress %q does not imply a machine", slot.Status.Progress)}, nil
	}

	nodePresent, err := kubernetesNodePresent(ctx, c, slot)
	if err != nil {
		return Outcome{}, err
	}

	jobs, err := lifecycleJobs(ctx, c, slot)
	if err != nil {
		return Outcome{}, err
	}

	lockHeld, lockNote := lockIsHeld(ctx, coord, slot)

	decision := decide(slot, nodePresent, lockHeld, jobs, time.Now(), grace)
	if !decision.settle {
		if lockHeld {
			logger = logger.WithValues("lock", lockNote)
		}

		if decision.requeueAfter > 0 {
			logger.V(1).Info("Leaving status.progress to the operation in flight",
				"progress", string(slot.Status.Progress),
				"reason", decision.reason,
				"requeueAfter", decision.requeueAfter.String())

			return Outcome{RequeueAfter: decision.requeueAfter, Reason: decision.reason}, nil
		}

		logger.V(1).Info("Nothing to settle", "progress", string(slot.Status.Progress), "reason", decision.reason)

		return Outcome{Reason: decision.reason}, nil
	}

	settled, previous, err := writeSettlement(ctx, c, coord, slot, grace)
	if err != nil {
		// The slot is gone, the API server is unreachable, ... Nothing was written, so the
		// caller reports the error and the normal controller backoff brings the pass back.
		return Outcome{}, err
	}

	if !settled {
		// The object moved between the decision and the write and no longer qualifies: the
		// machine of a lifecycle Job, an observation controller, a claim, got there first.
		logger.V(1).Info("status.progress was settled by someone else while writing",
			"progress", string(slot.Status.Progress))

		return Outcome{Reason: "the slot stopped qualifying on re-read"}, nil
	}

	note := settlementNote(slot, previous)

	// Two channels, on purpose: the Warning event is what `kubectl describe` shows for a slot
	// that was left pointing at a machine that is gone, the log line is what the controller's
	// own trail is. A settlement is not routine: it means a machine was deleted behind the
	// operator's back, or an operation died with its manager, and that is worth a Warning.
	logger.Info("Settled status.progress of a slot with no machine and no lifecycle Job",
		"previousProgress", string(previous),
		"progress", string(infrav1alpha1.ProgressShutdown),
		"powerstate", string(infrav1alpha1.PowerStateOff),
		"kubernetesNode", slot.Spec.KubernetesNodeName)

	if recorder != nil {
		recorder.Eventf(slot, nil, corev1.EventTypeWarning, ReasonProgressReconciled, ActionSettleProgress,
			"%s", note)
	}

	return Outcome{Settled: true, Previous: previous}, nil
}

// settlementNote renders the Warning event's message: the progress that was replaced, the
// evidence that settled it, and the values it was settled to.
func settlementNote(slot *infrav1alpha1.Node, previous infrav1alpha1.Progress) string {
	kubernetesNode := slot.Spec.KubernetesNodeName
	if kubernetesNode == "" {
		kubernetesNode = "none"
	}

	return fmt.Sprintf(
		"Reconciled status.progress of node %s from %q to %q (status.powerState %q): spec.desiredPowerState is %q and "+
			"the cluster has no Kubernetes node %q and no startup/shutdown Job for the slot, so nothing is running "+
			"the transition the progress describes",
		slot.Name, string(previous), string(infrav1alpha1.ProgressShutdown), string(infrav1alpha1.PowerStateOff),
		string(infrav1alpha1.PowerStateOff), kubernetesNode)
}

// decision is the rule with no I/O in it, so the rule is testable on its own.
type decision struct {
	// settle says the slot must be moved to off/shutdown.
	settle bool
	// requeueAfter is > 0 when the slot was left alone for a reason with a deadline (a
	// lifecycle Job in flight, or one that finished inside the grace), so the caller schedules
	// the look that settles it.
	requeueAfter time.Duration
	// reason is the human-readable explanation for the log line.
	reason string
	// previous is the progress being replaced, set when settle is true.
	previous infrav1alpha1.Progress
}

// decide applies the settlement rule to what the caller observed. See Settle for the rules and
// for why each gate exists.
func decide(slot *infrav1alpha1.Node, nodePresent, lockHeld bool, jobs []batchv1.Job, now time.Time, grace time.Duration) decision {
	if slot.Spec.DesiredPowerState != infrav1alpha1.PowerStateOff {
		return decision{reason: "spec.desiredPowerState is not off, a claim is never overridden"}
	}

	if !impliesMachine(slot.Status.Progress) {
		return decision{reason: fmt.Sprintf("status.progress %q does not imply a machine", slot.Status.Progress)}
	}

	if nodePresent {
		return decision{reason: fmt.Sprintf("the slot has a Kubernetes node %q, its status belongs to the observation controllers",
			slot.Spec.KubernetesNodeName)}
	}

	if lockHeld {
		return decision{reason: "a coordination lock names an operation that owns the slot"}
	}

	// The grace window is measured from the tightest deadline of the Jobs of the slot: the
	// earliest one that is about to leave it.
	var wait time.Duration

	for i := range jobs {
		job := &jobs[i]

		completed, finished := finishedAt(job)
		if !finished {
			// Pending or running: the operation is alive and its monitor is the writer. Look
			// again on the grace, so a monitor that died with its manager is still caught.
			return decision{requeueAfter: grace, reason: fmt.Sprintf("lifecycle Job %s is in flight", job.Name)}
		}

		remaining := grace - now.Sub(completed)
		if remaining <= 0 {
			continue
		}

		if wait == 0 || remaining < wait {
			wait = remaining
		}
	}

	if wait > 0 {
		return decision{requeueAfter: wait, reason: "a lifecycle Job of the slot finished inside the settle grace"}
	}

	return decision{
		settle:   true,
		previous: slot.Status.Progress,
		reason:   "no Kubernetes node, no lifecycle Job, no coordination lock",
	}
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

// writeSettlement writes the settled pair, re-deriving the whole rule from the object read back.
// A write that goes through on a slot someone else changed in the meantime is how a controller
// stomps an operation, so the rule is evaluated against the fresh copy and the write is skipped
// when the slot no longer qualifies. It reports whether it wrote, and the progress it replaced.
func writeSettlement(ctx context.Context, c client.Client, coord fsm.CoordinationManager, slot *infrav1alpha1.Node, grace time.Duration) (bool, infrav1alpha1.Progress, error) {
	var (
		settled  bool
		previous infrav1alpha1.Progress
	)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(slot), fresh); err != nil {
			return err
		}

		nodePresent, err := kubernetesNodePresent(ctx, c, fresh)
		if err != nil {
			return err
		}

		jobs, err := lifecycleJobs(ctx, c, fresh)
		if err != nil {
			return err
		}

		lockHeld, _ := lockIsHeld(ctx, coord, fresh)

		d := decide(fresh, nodePresent, lockHeld, jobs, time.Now(), grace)
		if !d.settle {
			// The slot stopped qualifying: an operation, a claim, or a machine appeared.
			return nil
		}

		fresh.Status.Progress = infrav1alpha1.ProgressShutdown
		fresh.Status.PowerState = infrav1alpha1.PowerStateOff

		if err := c.Status().Update(ctx, fresh); err != nil {
			return err
		}

		settled = true
		previous = d.previous

		// Keep the caller's copy in step, so the pass that settled the slot acts on the
		// settled values: the FSM reads its state from status.progress, and the node
		// controller has just run this pass with this object in hand.
		slot.Status.Progress = fresh.Status.Progress
		slot.Status.PowerState = fresh.Status.PowerState

		return nil
	})
	if err != nil {
		return false, "", err
	}

	return settled, previous, nil
}

// kubernetesNodePresent reports whether the slot has a Kubernetes node object. Any object counts,
// Ready or not: a node object is a machine, and the status of a slot with one is owned by the
// observation controllers (core.applyObservedPowerState spells out why NotReady is a weak
// observation of a machine, and never evidence that a machine is gone).
func kubernetesNodePresent(ctx context.Context, c client.Client, slot *infrav1alpha1.Node) (bool, error) {
	if slot.Spec.KubernetesNodeName == "" {
		// The slot has never been provisioned, so there is no machine to look for.
		return false, nil
	}

	kubeNode := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: slot.Spec.KubernetesNodeName}, kubeNode); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("get kubernetes node %s: %w", slot.Spec.KubernetesNodeName, err)
	}

	return true, nil
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
