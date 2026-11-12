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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// ClaimFinalizer is the finalizer the claim model puts on a claim the moment its phase becomes
// booting. It is the claim.Finalizer constant, spelled here so the controller that removes it on a
// landed scale-down names the same string the recorder writes; see internal/claim/recorder.go for
// the rules and docs/development/nodeclaim.md §3.
const ClaimFinalizer = claim.Finalizer

// NodeClaimReconciler owns the lifecycle of a NodeClaim: the attempt seed, one live claim per slot,
// the deadline, the attempt bound, the finalizer of a scale-down, the digest the slot keeps, and the
// collection of a terminal claim after its TTL.
//
// It does not boot or power off machines. The transition that buys the machine runs on the slot
// (NodeReconciler, internal/controller/infra/lifecycle.go), because the coordination lock, the
// lifecycle Job and the readiness gate are per-slot machinery; this controller moves the claim's
// phase from what that path reports (via the fsm's claim writer), from the deadline, and from the
// observation of the machine.
type NodeClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// EventRecorder is the recorder the claim's timeline is published through. The timeline itself is
	// written by the claim.Recorder (internal/claim/events.go: one Event per phase move, plus the
	// creation and the endings), because that is the chokepoint every phase write of the model passes
	// through - so this reconciler's job is to hand that recorder an event recorder, which
	// SetupWithManager does with the manager's. nil records nothing, and the phase writes are
	// unaffected.
	EventRecorder events.EventRecorder

	// Recorder writes claim status, and publishes the claim's timeline through it. Set to
	// claim.NewRecorderWithEvents(r.Client, mgr.GetEventRecorder("nodeclaim-controller")) in
	// SetupWithManager when nil, so tests can inject their own.
	Recorder *claim.Recorder

	// APIReader reads the slot of a claim straight from the API server, past the cache. The one path
	// that needs it is the release of a finalizer on a slot that is gone: that write is
	// irreversible, so the fact behind it is never a read that can lag (see slotIsGone). Set from
	// mgr.GetAPIReader() in SetupWithManager when nil.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodeclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodeclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/status,verbs=get;update;patch

// Reconcile runs one pass over one claim.
func (r *NodeClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	nodeClaim := &infrav2alpha1.NodeClaim{}
	if err := r.Get(ctx, req.NamespacedName, nodeClaim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	slot, err := r.slotOf(ctx, nodeClaim)
	if err != nil {
		if errors.IsNotFound(err) {
			// The slot is gone. The machine it named is going with it (the claim's ownerReference
			// makes the slot's deletion the claim's deletion), so the claim has nothing left to
			// run: the finalizer comes off and the GC finishes the object. Leaving it on is a claim
			// stuck in Terminating forever, never retried, with a machine running behind it - and
			// it is reachable through the documented rollout, which deletes the Node CRs
			// (docs/development/nodeclaim.md §9), and through garbage collection of a namespace.
			//
			// The read that says so is the cached one, and the write behind it cannot be undone: a
			// cache that has not caught up with a slot that exists - a slot recreated by the rollout,
			// a claim reconciled from a watch of its own create - would release the finalizer of a
			// claim whose machine is still wanted. The fact is re-read from the API server before
			// anything is given up (slotIsGone), and a pass that disagrees requeues.
			//
			// No digest is stamped: the record lives on the slot, and the slot is not there. The
			// phase move is best-effort for the same reason - it is the story an operator reads when
			// the claim survives the finalizer removal, not a step the model depends on.
			if !r.slotIsGone(ctx, nodeClaim) {
				logger.Info("The cache says the slot is gone and the API server says it is there; re-reading on the next pass",
					"claim", nodeClaim.Name, "slot", claim.SlotNameOf(nodeClaim))

				return ctrl.Result{RequeueAfter: slotReadDisagreementResync}, nil
			}

			return r.releaseDeletedSlot(ctx, nodeClaim)
		}

		logger.Error(err, "Failed to read the slot of the claim", "claim", nodeClaim.Name)

		return ctrl.Result{}, err
	}

	// The attempt number the claim carries: seeded once, from the slot's digest, so the bound on
	// attempts is a bound on the consecutive claims of a slot and needs no counter, and no reset
	// heuristic, on the slot itself (§4).
	if nodeClaim.Status.Attempt == 0 {
		if _, err := r.recorder().Seed(ctx, client.ObjectKeyFromObject(nodeClaim), slot); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.Get(ctx, req.NamespacedName, nodeClaim); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	// One live claim per slot. The webhook refuses a second one at admission; this is the same rule
	// enforced where the API cannot see it (two providers racing, a claim applied by hand, a webhook
	// that was down). The extra claim fails itself, and the error names the slot.
	handled, err := r.refuseExtraLiveClaims(ctx, nodeClaim)
	if err != nil {
		return ctrl.Result{}, err
	}

	if handled {
		// This claim is the extra one, and failing it is the whole pass: the object is terminal now,
		// its TTL collects it, and the copy this pass read is out of date - the pass that writes a
		// finalizer from it would write one on a dead claim.
		return ctrl.Result{RequeueAfter: idleResync}, nil
	}

	// The finalizer arrives with the boot: from the moment the phase is booting, deleting this claim
	// is a scale-down request and not an abandonment.
	if err := r.ensureFinalizer(ctx, nodeClaim); err != nil {
		return ctrl.Result{}, err
	}

	if !nodeClaim.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, nodeClaim, slot)
	}

	return r.reconcileLive(ctx, nodeClaim, slot)
}

// releaseDeletedSlot ends a claim whose slot (Node CR) is gone: the reason is recorded, the finalizer
// comes off, and the claim's own TTL is kept from here (finishReleasedSlot). The SlotDeleted digest
// is deliberately not stamped - the record lives on the slot, and the slot does not exist - so the
// claim object is the record, for as long as its TTL keeps it: the collection path this controller
// uses for every other terminal claim is reached through the slot, and the slot is gone.
//
// "For as long as its TTL keeps it" is exact, and the wording matters. A claim the scale-down already
// marked is deleted by the API server on the write that removes its finalizer, and a claim an
// ownerReference points at can be taken by the slot's garbage collection at the same instant: neither
// survives to its TTL. An unmarked claim of a deleted slot - the rollout of §9, which deletes the
// Node CRs and leaves the claims - is the one that reads as the record, and it is kept for
// spec.ttlSecondsAfterFinished from the moment it was failed.
func (r *NodeClaimReconciler) releaseDeletedSlot(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if phase.IsLive(nodeClaim.Status.Phase) {
		if _, err := r.recorder().MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonSlotDeleted); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if !claim.HasFinalizer(nodeClaim) {
		return r.finishReleasedSlot(ctx, nodeClaim)
	}

	// Re-Get: the recorder's move bumped the resourceVersion, and a finalizer removal that lands on
	// a caller's stale copy is a conflict on the pass that has nothing else left to do.
	fresh := &infrav2alpha1.NodeClaim{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !controllerutil.ContainsFinalizer(fresh, ClaimFinalizer) {
		return r.finishReleasedSlot(ctx, nodeClaim)
	}

	controllerutil.RemoveFinalizer(fresh, ClaimFinalizer)

	if err := r.Update(ctx, fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Released the finalizer of a claim whose slot is gone",
		"claim", nodeClaim.Name, "slot", claim.SlotNameOf(nodeClaim))

	return r.finishReleasedSlot(ctx, nodeClaim)
}

// finishReleasedSlot keeps the TTL of a claim that outlived its slot. Every other collection path of
// this controller is reached through the slot - it reads the slot, and stamps the digest the slot
// keeps - so with the slot gone that path never runs again, and the only record of the episode is the
// object itself (§8, §9).
//
// A claim the provider created names its slot by an ownerReference on the slot, so the slot's
// deletion is its deletion and the garbage collector finishes it. A claim applied by hand - a
// manifest, a provider that stamped no ownerReference - has no collector at all: nothing deletes it,
// and its spec.ttlSecondsAfterFinished is a field nothing reads. The release path is the one pass
// this controller runs for such an object, so the TTL is honoured here: deleted once it has passed,
// and a requeue scheduled for the instant it passes when it has not.
func (r *NodeClaimReconciler) finishReleasedSlot(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) (ctrl.Result, error) {
	fresh := &infrav2alpha1.NodeClaim{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), fresh); err != nil {
		// The object is gone: the finalizer removal above finished a marked claim, and its owner's
		// garbage collection finished an unmarked one. Nothing is left to schedule.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if phase.IsLive(fresh.Status.Phase) {
		// The move to failed did not land (a conflict, a write the API server refused). The next pass
		// re-reads the slot's absence, lands it, and reaches the collection below.
		return ctrl.Result{RequeueAfter: idleResync}, nil
	}

	if fresh.Status.FinishedAt == nil {
		if _, err := r.recorder().StampFinishedAt(ctx, client.ObjectKeyFromObject(fresh)); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}

		return ctrl.Result{RequeueAfter: idleResync}, nil
	}

	ttl := ttlOfClaim(fresh)

	if expiry := fresh.Status.FinishedAt.Add(ttl); expiry.After(time.Now()) {
		return ctrl.Result{RequeueAfter: time.Until(expiry)}, nil
	}

	if err := r.Delete(ctx, fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.FromContext(ctx).Info("Collected a terminal claim whose slot is gone", "claim", fresh.Name,
		"slot", claim.SlotNameOf(fresh), "phase", string(fresh.Status.Phase), "ttl", ttl.String())

	return ctrl.Result{}, nil
}

// slotIsGone asks the API server, past the cache, whether the slot of a claim is gone. The cached
// read in slotOf is the one that drives every other decision of this controller, and it is the one
// that can be behind: a slot the rollout deleted and recreated, a claim reconciled from a watch of
// its own create. Releasing a finalizer cannot be undone, so the irreversible branch re-reads the
// fact from the API server and gives up only when both readers agree the slot is not there.
//
// With no direct reader wired - a unit test whose client is the only client there is - the cached
// read is not a lagging one, and the answer is the cached one.
func (r *NodeClaimReconciler) slotIsGone(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) bool {
	if r.APIReader == nil {
		return true
	}

	slot := &infrav2alpha1.Node{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: nodeClaim.Namespace, Name: claim.SlotNameOf(nodeClaim)}, slot); err != nil {
		return errors.IsNotFound(err)
	}

	return false
}

// slotReadDisagreementResync is how long a pass that cannot tell whether its slot is gone waits
// before asking again: the cache catches up in seconds, and the question is only ever asked once.
const slotReadDisagreementResync = 5 * time.Second

// reconcileLive enforces the two bounds that end an attempt without a machine - the boot deadline
// and the streak of attempts - owns the machine that vanished under a ready claim, keeps the slot's
// digest in step, and asks to be looked at again at the deadline.
//
// Both bounds are bounds on the boot window, and they stop applying at ready: the deadline is the
// budget for buying a machine, and the attempt bound is the bound on how many machines a slot may be
// asked to buy in a row, so a claim that has its machine is not destroyed by a budget it already
// spent, and is not failed by the number it landed on.
//
// The deadline is the whole replacement for the v1alpha1 progress watchdog: settle/re-arm/uncordon
// existed to heal a long-lived slot left pointing at a machine that is not there, and a claim that
// produced nothing has a deadline that heals it - failed, force shutdown (the input law shuts a
// machine down when its claim is terminal), slot free, next claim attempt N+1.
func (r *NodeClaimReconciler) reconcileLive(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim, slot *infrav2alpha1.Node) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	recorder := r.recorder()

	if phase.InBootWindow(nodeClaim.Status.Phase) && claim.IsOverdue(nodeClaim, time.Now()) {
		moved, err := recorder.MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonDeadlineExceeded)
		if err != nil {
			return ctrl.Result{}, err
		}

		if moved {
			// The move is the record this pass reports on: the digest the slot keeps, the terminal
			// branch below and the events all read the claim's phase, reason and finish time, and a
			// caller's copy written before the move names the attempt that just ended as the one
			// still running.
			if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), nodeClaim); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}

		logger.Info("Claim passed its deadline", "claim", nodeClaim.Name, "slot", slot.Name,
			"phase", string(nodeClaim.Status.Phase), "deadline", nodeClaim.Spec.DeadlineAt)
	}

	// The attempt bound, read off the claim: a slot whose streak of attempts is spent buys no further
	// machine until an attempt lands (SeedAttempt restarts the streak at 1 after one does). This
	// replaces fsm.StartupAttemptLimit, the ClaimRearmed condition on the slot and its refusal site.
	if phase.InBootWindow(nodeClaim.Status.Phase) && nodeClaim.Status.Attempt > claim.MaxConsecutiveAttempts {
		moved, err := recorder.MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonAttemptLimitExceeded)
		if err != nil {
			return ctrl.Result{}, err
		}

		if moved {
			if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), nodeClaim); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
	}

	if phase.IsTerminal(nodeClaim.Status.Phase) {
		return r.collect(ctx, nodeClaim, slot)
	}

	// A ready claim whose machine has vanished has no owner in the input law: the report it derives
	// is booting, law 1 needs the shutdown state to fire a boot and law 3 needs the machine observed
	// up to fire a shutdown, and the boot deadline closed on the write that made the claim ready. The
	// claim controller owns the case, bounded by an observation (see watchMachine).
	lost, requeue, err := r.watchMachine(ctx, nodeClaim, slot)
	if err != nil {
		return ctrl.Result{}, err
	}

	if lost {
		// Re-Get, so the collection below digests the episode that just ended and not the claim as
		// this pass found it, and hand the terminal claim to the collector: the digest, the finish
		// time and the TTL are its job, and one pass that records an end is one pass that records it
		// everywhere.
		if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), nodeClaim); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}

		return r.collect(ctx, nodeClaim, slot)
	}

	if err := StampDigest(ctx, r.Client, slot, nodeClaim); err != nil {
		logger.V(1).Info("Failed to stamp the slot's digest of the claim", "claim", nodeClaim.Name, "error", err.Error())
	}

	res := ctrl.Result{RequeueAfter: idleResync}
	if nodeClaim.Spec.DeadlineAt != nil {
		res = requeueAt(res, nodeClaim.Spec.DeadlineAt.Time)
	}

	if requeue != nil {
		res = requeueAt(res, time.Now().Add(*requeue))
	}

	return res, nil
}

// watchMachine owns the machine that disappears under a claim that already reached ready.
//
// Nothing else does. phase.Derive reports booting for "ready claim, machine off" and the state
// machine starts in startingup, where law 1's StartNode is not reachable and law 3's ShutdownNode
// needs the machine observed up: a ready claim whose machine was deleted in a Hetzner console, by a
// `kubectl delete node` of a deprovisioned server, or by a pool that was taken away, sits live
// forever, counted as capacity the autoscaler is paying for, with the reason ClaimReasonMachineGone
// named in the API and written by nobody.
//
// The bound is an observation, not a pass: the first pass that sees the machine off stamps
// MachineObservedOff, a loss that heals leaves the claim ready and clears the condition, and only a
// loss that outlasts one boot window (claim.MachineGoneGrace) ends the attempt. The move is the
// whole cleanup: a terminal claim is not a want, so the input law powers anything that is running
// off, the slot becomes free, and the next request of the slot is attempt N+1.
func (r *NodeClaimReconciler) watchMachine(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim, slot *infrav2alpha1.Node) (bool, *time.Duration, error) {
	logger := log.FromContext(ctx)

	if nodeClaim.Status.Phase != infrav2alpha1.NodeClaimReady {
		return false, nil, nil
	}

	key := client.ObjectKeyFromObject(nodeClaim)

	if slot.Status.PowerState == infrav2alpha1.PowerStateOn {
		if _, err := r.recorder().ClearMachineLoss(ctx, key); err != nil && client.IgnoreNotFound(err) != nil {
			return false, nil, err
		}

		return false, nil, nil
	}

	first, _, err := r.recorder().RecordMachineLoss(ctx, key)
	if err != nil && client.IgnoreNotFound(err) != nil {
		return false, nil, err
	}

	if first.IsZero() {
		// The condition write did not land, or the claim is gone: the next pass re-reads the
		// observation, and a loss that is real is old by then.
		return false, nil, nil
	}

	grace := claim.MachineGoneGrace()

	if age := time.Since(first); age < grace {
		left := grace - age

		logger.V(1).Info("The machine of a ready claim is observed off; the loss is being watched",
			"claim", nodeClaim.Name, "slot", slot.Name, "observedSince", first.String(), "bound", grace.String())

		return false, &left, nil
	}

	moved, err := r.recorder().MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonMachineGone)
	if err != nil {
		return false, nil, err
	}

	logger.Info("The machine of a ready claim is gone",
		"claim", nodeClaim.Name, "slot", slot.Name, "observedSince", first.String(), "grace", grace.String())

	return moved, nil, nil
}

// reconcileDeletion runs the finalizer of a claim a scale-down named.
//
// A pending claim has no finalizer, so it never reaches this path: deleting it is the cancellation,
// and the unit comes off the target size on the delete. A booting or ready claim does reach it, and
// the rule is that a boot is reversed, never interrupted: the machine is powered off by the shutdown
// path (the slot reconciler fires ShutdownNode for a claim marked for scale-down), and only once the
// machine is observed off does the claim end and the finalizer come off.
//
// While the machine is still up this pass is read-only. The attempts of the scale-down are counted by
// the path that fires them (internal/fsm ShutdownNode -> recordShutdownAttempt -> BumpScaleDownAttempt),
// not here: this path polls the observation every shutdownPollResync, and a poll is not an attempt.
// The bound it would spend on polls is the bound the slot reconciler applies, so the streak stays the
// streak of shutdown Jobs - which is the number the 30m pause is measured against.
func (r *NodeClaimReconciler) reconcileDeletion(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim, slot *infrav2alpha1.Node) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	recorder := r.recorder()

	if slot.Status.PowerState == infrav2alpha1.PowerStateOn {
		// The machine is up. The slot reconciler owns powering it off and counting the shutdowns it
		// fires; this pass keeps the finalizer, reads the streak for the log, and comes back.
		logger.V(1).Info("Claim is being deleted and the machine is still up; keeping the finalizer",
			"claim", nodeClaim.Name, "slot", slot.Name, "phase", string(nodeClaim.Status.Phase),
			"shutdownAttempts", nodeClaim.Status.ShutdownAttempts,
			"shutdownsFired", "the shutdown Jobs this slot's scale-down created, counted where they fire")

		return ctrl.Result{RequeueAfter: shutdownPollResync}, nil
	}

	// The machine is off. The scale-down landed: a live claim ends completed, and the digest the slot
	// keeps says which request it was. The streak ends with the object that carries it, which is the
	// reset the v1alpha1 shutdown counter needed a site for and does not exist here.
	if phase.IsLive(nodeClaim.Status.Phase) {
		if _, err := recorder.MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimCompleted, reasonForDeletion(nodeClaim)); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Re-Get: the recorder's move above wrote the status subresource, and a digest, or a finalizer
	// removal, built on the copy this pass started with is a guaranteed conflict on the first pass of
	// every scale-down - and a digest built on the stale copy is the story of the claim as it was
	// before the scale-down landed, phase and reason both. The removal is the last thing the claim
	// controller has to do, so the write that lands is the one the GC finishes on.
	fresh := &infrav2alpha1.NodeClaim{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := StampDigest(ctx, r.Client, slot, fresh); err != nil {
		logger.V(1).Info("Failed to stamp the slot's digest of the deleted claim", "claim", nodeClaim.Name, "error", err.Error())
	}

	if !controllerutil.ContainsFinalizer(fresh, ClaimFinalizer) {
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(fresh, ClaimFinalizer)

	if err := r.Update(ctx, fresh); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return ctrl.Result{}, nil
}

// shutdownPollResync is how long a claim marked for scale-down waits between looks at a machine that
// is still observed up. It is the poll of a lifecycle operation, not a retry: the shutdown Job is
// what powers the machine off, and this path only notices when it did.
const shutdownPollResync = 15 * time.Second

// refuseExtraLiveClaims enforces one live claim per slot, in the place the API cannot see
// (admission races, a hand-applied claim, a provider that listed a stale inventory). The newest live
// claim of a slot is the one the slot answers for; every older live claim on the same slot is failed
// with reason Superseded, and the error names the slot, because "two claims want one machine" is an
// operator-visible bug and not a supported state (docs/development/nodeclaim.md §8). The event of the fact
// is the recorder's ClaimSuperseded on that move, and it names the claim that won the slot
// (internal/claim/events.go).
func (r *NodeClaimReconciler) refuseExtraLiveClaims(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) (bool, error) {
	if !phase.IsLive(nodeClaim.Status.Phase) {
		return false, nil
	}

	claims, err := claim.ListOfSlot(ctx, r.Client, nodeClaim.Namespace, claim.SlotNameOf(nodeClaim))
	if err != nil {
		return false, err
	}

	extras := make([]*infrav2alpha1.NodeClaim, 0, len(claims))

	for i := range claims {
		existing := &claims[i]
		if !phase.IsLive(existing.Status.Phase) || existing.Name == nodeClaim.Name {
			continue
		}

		// The newest live claim owns the slot; ties break on the name, so the rule is deterministic
		// and matches claim.LiveOfSlot's choice of the slot's claim.
		if isNewerClaim(existing, nodeClaim) {
			extras = append(extras, nodeClaim)

			break
		}

		extras = append(extras, existing)
	}

	if len(extras) == 0 {
		return false, nil
	}

	for _, extra := range extras {
		if extra.Name == nodeClaim.Name {
			// The claim fails itself. The event of the fact - which claim owns the slot now - is the
			// recorder's ClaimSuperseded on the move below: one event per transition, and the detail
			// (the newer claim's name) is in it.
			if _, err := r.recorder().MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonSuperseded); err != nil {
				return false, err
			}

			return true, nil
		}

		if _, err := r.recorder().Move(ctx, client.ObjectKeyFromObject(extra), infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonSuperseded); err != nil {
			return false, err
		}
	}

	return false, nil
}

// isNewerClaim is the ordering the model uses everywhere: creation timestamp, with the name breaking
// a tie so a slot has one newest claim by one formula (claim.LiveOfSlot, StampDigest's digest rule,
// and this refusal of an extra live claim all ask the same question the same way).
func isNewerClaim(a, b *infrav2alpha1.NodeClaim) bool {
	if a.Name == b.Name {
		return false
	}

	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.After(b.CreationTimestamp.Time)
	}

	return a.Name < b.Name
}

// ensureFinalizer is the belt of the scale-down finalizer. The writer is the recorder: the
// pending -> booting move stamps it on the same write that records the phase, so a DeleteNodes in
// the pass boundary cannot delete a committed boot with no shutdown behind it (M5, and
// docs/development/nodeclaim.md §3). This pass covers what that writer cannot see: a claim whose phase
// was landed on by a hand write, or by a move whose metadata write the API server refused.
func (r *NodeClaimReconciler) ensureFinalizer(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) error {
	if claim.HasFinalizer(nodeClaim) {
		return nil
	}

	// Re-Get: the writers that ran before this pass's metadata write (the attempt seed, the
	// supersede, a move) all bump the resourceVersion, and a finalizer added on a caller's stale
	// copy is a conflict on a path that has to succeed for the scale-down to be seen at all. The
	// phase question is asked of the object the cluster has, so a claim the recorder just failed is
	// never stamped a finalizer.
	fresh := &infrav2alpha1.NodeClaim{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), fresh); err != nil {
		return client.IgnoreNotFound(err)
	}

	if phase.IsPending(fresh.Status.Phase) || !phase.IsLive(fresh.Status.Phase) {
		return nil
	}

	if !fresh.DeletionTimestamp.IsZero() {
		// The API server refuses a finalizer on an object that is already being deleted, and a claim
		// that is being deleted has its scale-down running from the finalizer it does have.
		return nil
	}

	if controllerutil.ContainsFinalizer(fresh, ClaimFinalizer) {
		nodeClaim.Finalizers = append([]string(nil), fresh.Finalizers...)
		nodeClaim.ResourceVersion = fresh.ResourceVersion

		return nil
	}

	controllerutil.AddFinalizer(fresh, ClaimFinalizer)

	if err := r.Update(ctx, fresh); err != nil {
		return client.IgnoreNotFound(err)
	}

	// Keep the caller's copy in step with the write, so a later write of the same pass lands.
	nodeClaim.Finalizers = append([]string(nil), fresh.Finalizers...)
	nodeClaim.ResourceVersion = fresh.ResourceVersion

	return nil
}

// collect is the garbage collection of a terminal claim: after its TTL from status.finishedAt the
// object is deleted, which is safe because the slot keeps the digest of the episode
// (Node.status.lastClaim). A claim with no finishedAt is stamped now, so a terminal phase reached by
// a writer that did not stamp the time is still collected, and nothing accumulates on a broken pool.
func (r *NodeClaimReconciler) collect(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim, slot *infrav2alpha1.Node) (ctrl.Result, error) {
	if err := StampDigest(ctx, r.Client, slot, nodeClaim); err != nil {
		log.FromContext(ctx).V(1).Info("Failed to stamp the slot's digest of the collected claim",
			"claim", nodeClaim.Name, "error", err.Error())
	}

	if nodeClaim.Status.FinishedAt == nil {
		moved, err := r.recorder().MoveOf(ctx, nodeClaim, nodeClaim.Status.Phase, nodeClaim.Status.Reason)
		if err != nil {
			return ctrl.Result{}, err
		}

		if !moved {
			// The claim already carries the phase it is being moved to, so the recorder's move is a
			// no-op and writes no clock. A terminal claim with no finishedAt has no instant for its
			// TTL to start from, and would live forever: stamp the time the collection measures, so a
			// phase landed on by a writer that stamped no time (a hand write, an apply of a status)
			// is collected like one the recorder wrote.
			if _, err := r.recorder().StampFinishedAt(ctx, client.ObjectKeyFromObject(nodeClaim)); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}

		return ctrl.Result{RequeueAfter: idleResync}, nil
	}

	ttl := ttlOfClaim(nodeClaim)

	expiry := nodeClaim.Status.FinishedAt.Add(ttl)
	if now := time.Now(); now.Before(expiry) {
		return ctrl.Result{RequeueAfter: expiry.Sub(now)}, nil
	}

	if err := r.Delete(ctx, nodeClaim); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("Collected a terminal claim", "claim", nodeClaim.Name,
		"slot", claim.SlotNameOf(nodeClaim), "phase", string(nodeClaim.Status.Phase), "ttl", ttl.String())

	return ctrl.Result{}, nil
}

// slotOf reads the slot (Node CR) the claim asks for. spec.slotRef.name is the field; the lookup is
// by name in the claim's namespace, which is the namespace of the slot.
func (r *NodeClaimReconciler) slotOf(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) (*infrav2alpha1.Node, error) {
	slot := &infrav2alpha1.Node{}

	if err := r.Get(ctx, client.ObjectKey{Namespace: nodeClaim.Namespace, Name: claim.SlotNameOf(nodeClaim)}, slot); err != nil {
		return nil, err
	}

	return slot, nil
}

// reasonForDeletion names why a scale-down claim ended, from the request that made it.
func reasonForDeletion(nodeClaim *infrav2alpha1.NodeClaim) string {
	switch nodeClaim.Spec.Request.Call {
	case infrav2alpha1.ClaimCallDeleteNodes:
		return infrav2alpha1.ClaimReasonNodeDeleted
	case infrav2alpha1.ClaimCallDecreaseTargetSize:
		return infrav2alpha1.ClaimReasonCancelledByDecreaseTargetSize
	default:
		return infrav2alpha1.ClaimReasonDeletedByHand
	}
}

// recorder returns the status writer, defaulting to one bound to this reconciler's client and to the
// reconciler's event recorder, so the claim's timeline is published at the chokepoint that writes the
// phase whatever built the Recorder.
func (r *NodeClaimReconciler) recorder() *claim.Recorder {
	if r.Recorder != nil {
		return r.Recorder
	}

	r.Recorder = claim.NewRecorderWithEvents(r.Client, r.EventRecorder)

	return r.Recorder
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		// The claim's timeline is the recorder's, and the manager is the source of the event recorder
		// it publishes through. A recorder named for this controller is the record in
		// `kubectl get events -o wide`: the reporting controller says which controller spoke, and the
		// action (claim.ActionClaim) is the key the new events API aggregates repeats on.
		r.Recorder = claim.NewRecorderWithEvents(r.Client, mgr.GetEventRecorder("nodeclaim-controller"))
	}

	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav2alpha1.NodeClaim{}).
		Named("nodeclaim").
		Complete(r)
}

// ttlOfClaim is spec.ttlSecondsAfterFinished of a terminal claim, defaulted to
// claim.DefaultTTLSecondsAfterFinished. Both collection paths read it here: the one that runs with a
// slot (collect, which stamps the digest and deletes), and the one that runs when the slot is gone
// (finishReleasedSlot, which has no digest to stamp and no garbage collector to finish the object).
func ttlOfClaim(nodeClaim *infrav2alpha1.NodeClaim) time.Duration {
	if nodeClaim.Spec.TTLSecondsAfterFinished != nil {
		return time.Duration(*nodeClaim.Spec.TTLSecondsAfterFinished) * time.Second
	}

	return time.Duration(claim.DefaultTTLSecondsAfterFinished) * time.Second
}
