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
		fsm.WithClaim(live, claimWriterFor(r.Client)))

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
	// shuttingdown is driven by its own Job. Nothing fires here.
	stateMachine.CheckStuckState()

	if fsm.IsTransitionalState(state) {
		return ctrl.Result{RequeueAfter: stateMachine.CalculateBackoff()}, nil
	}

	return ctrl.Result{RequeueAfter: idleResync}, nil
}

// scaleDownStreakRefusal reports that the model has stopped retrying the shutdown of a machine that
// will not go off, and the requeue that paces it. The bound is the streak the claim digest carries
// (claim.MaxConsecutiveAttempts), counted on the scale-down claim of the slot and stamped onto the
// slot by the claim controller: it replaces the v1alpha1 ShutdownAttempts condition on the long-lived
// slot, with its two reset sites (docs/design/nodeclaim.md §4).
//
// The refusal is not an error: the slot is up, the pool does not want it, and the pool's shutdown
// script is what fails. The record is the Warning the claim controller stamps when the streak
// reaches the bound, and a fixed script is picked up by the next pass, no hand intervention.
func (r *NodeReconciler) scaleDownStreakRefusal(ctx context.Context, node *infrav2alpha1.Node, live *infrav2alpha1.NodeClaim) (bool, ctrl.Result) {
	// A scale-down that names a claim spends its attempts on that claim, and the claim controller
	// counts them (BumpScaleDownAttempt); the bound applies here, so a shutdown script that exits 0
	// without powering the machine off is not re-fired every resync.
	if live != nil {
		if live.Status.Attempt < claim.MaxConsecutiveAttempts || live.DeletionTimestamp.IsZero() {
			return false, ctrl.Result{}
		}
	} else if !claim.ScaleDownStreakExhausted(node) {
		return false, ctrl.Result{}
	}

	// The record the refusal points an operator at: the claim, when the scale-down named one, and
	// the slot's digest when it did not (the machine was left up by an episode that is already
	// collected).
	attempts, claimName, reason := int32(0), "", ""

	if live != nil {
		attempts, claimName, reason = live.Status.Attempt, live.Name, live.Status.Reason
	} else if node.Status.LastClaim != nil {
		attempts, claimName, reason = node.Status.LastClaim.Attempt, node.Status.LastClaim.Name, node.Status.LastClaim.Reason
	}

	log.FromContext(ctx).Info("Not firing the shutdown: this slot has used its scale-down attempts",
		"node", node.Name,
		"attempts", attempts,
		"limit", claim.MaxConsecutiveAttempts,
		"claim", claimName,
		"reason", reason,
		"state", "the machine stays up, the slot keeps its report, and the record is on the claim")

	return true, ctrl.Result{RequeueAfter: refusedResync}
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
