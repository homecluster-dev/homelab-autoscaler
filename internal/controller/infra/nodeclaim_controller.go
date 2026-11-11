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

	corev1 "k8s.io/api/core/v1"
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

// ClaimFinalizer is the finalizer the claim controller puts on a claim the moment its phase becomes
// booting. It is what makes "a boot that has started is never cancelled" true: deleting a booting or
// ready claim is not an abandonment of the machine, it is the scale-down request, and the finalizer
// runs the object to completed / failed once the machine is off (docs/design/nodeclaim.md §3).
//
// A pending claim carries no finalizer, so canceling a pending claim is a plain delete: nothing is in
// flight, the object goes, and the unit comes off the target size on that write.
const ClaimFinalizer = "infra.homecluster.dev/nodeclaim-finalizer"

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

	// EventRecorder publishes Events on the NodeClaim: the deadline that closed an attempt, the
	// attempt bound that stopped one, the extra live claim that was refused. nil records nothing.
	EventRecorder events.EventRecorder

	// Recorder writes claim status. Set to claim.NewRecorder(r.Client) in SetupWithManager when
	// nil, so tests can inject their own.
	Recorder *claim.Recorder
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
			// The slot is gone, and so is the machine this claim asked for.
			if _, err := r.recorder().MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonSlotDeleted); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
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
	if err := r.refuseExtraLiveClaims(ctx, nodeClaim); err != nil {
		return ctrl.Result{}, err
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

// reconcileLive enforces the two bounds that end an attempt without a machine - the deadline and the
// streak of attempts - keeps the slot's digest in step, and asks to be looked at again at the
// deadline.
//
// The deadline is the whole replacement for the v1alpha1 progress watchdog: settle/re-arm/uncordon
// existed to heal a long-lived slot left pointing at a machine that is not there, and a claim that
// produced nothing has a deadline that heals it - failed, force shutdown (the input law shuts a
// machine down when its claim is terminal), slot free, next claim attempt N+1.
func (r *NodeClaimReconciler) reconcileLive(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim, slot *infrav2alpha1.Node) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	recorder := r.recorder()

	if phase.IsLive(nodeClaim.Status.Phase) && claim.IsOverdue(nodeClaim, time.Now()) {
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

		r.eventf(nodeClaim, "ClaimDeadlineExceeded",
			"claim %s of slot %s is overdue (deadline %s, phase %s): recorded failed, the machine is force-shut-down and the next claim of the slot is attempt %d",
			nodeClaim.Name, slot.Name, nodeClaim.Spec.DeadlineAt, nodeClaim.Status.Phase, nodeClaim.Status.Attempt+1)

		logger.Info("Claim passed its deadline", "claim", nodeClaim.Name, "slot", slot.Name,
			"phase", string(nodeClaim.Status.Phase), "deadline", nodeClaim.Spec.DeadlineAt)
	}

	// The attempt bound, read off the claim: a slot whose streak of attempts is spent buys no further
	// machine until an attempt lands (SeedAttempt restarts the streak at 1 after one does). This
	// replaces fsm.StartupAttemptLimit, the ClaimRearmed condition on the slot and its refusal site.
	if phase.IsLive(nodeClaim.Status.Phase) && nodeClaim.Status.Attempt > claim.MaxConsecutiveAttempts {
		moved, err := recorder.MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonAttemptLimitExceeded)
		if err != nil {
			return ctrl.Result{}, err
		}

		if moved {
			if err := r.Get(ctx, client.ObjectKeyFromObject(nodeClaim), nodeClaim); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}

		r.eventf(nodeClaim, "ClaimAttemptLimitExceeded",
			"claim %s of slot %s is attempt %d of %d: the slot has used its consecutive attempts, no machine is bought for it, and the record lives on this claim",
			nodeClaim.Name, slot.Name, nodeClaim.Status.Attempt, claim.MaxConsecutiveAttempts)
	}

	if phase.IsTerminal(nodeClaim.Status.Phase) {
		return r.collect(ctx, nodeClaim, slot)
	}

	if err := StampDigest(ctx, r.Client, slot, nodeClaim); err != nil {
		logger.V(1).Info("Failed to stamp the slot's digest of the claim", "claim", nodeClaim.Name, "error", err.Error())
	}

	res := ctrl.Result{RequeueAfter: idleResync}
	if nodeClaim.Spec.DeadlineAt != nil {
		res = requeueAt(res, nodeClaim.Spec.DeadlineAt.Time)
	}

	return res, nil
}

// reconcileDeletion runs the finalizer of a claim a scale-down named.
//
// A pending claim has no finalizer, so it never reaches this path: deleting it is the cancellation,
// and the unit comes off the target size on the delete. A booting or ready claim does reach it, and
// the rule is that a boot is reversed, never interrupted: the machine is powered off by the shutdown
// path (the slot reconciler fires ShutdownNode for a claim marked for scale-down), and only once the
// machine is observed off does the claim end and the finalizer come off.
func (r *NodeClaimReconciler) reconcileDeletion(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim, slot *infrav2alpha1.Node) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	recorder := r.recorder()

	if slot.Status.PowerState == infrav2alpha1.PowerStateOn {
		// The machine is up. The slot reconciler owns powering it off; this pass keeps the finalizer,
		// counts the attempt for the bound, and comes back. The count runs for a live claim too:
		// a scale-down that named a ready claim spends its shutdown attempts on that claim, which is
		// what the bound of internal/claim.MaxConsecutiveAttempts bounds (the v1alpha1
		// ShutdownAttempts condition on the slot, and its two reset sites, are gone with §4).
		bumped, err := BumpScaleDownAttempt(ctx, r.Client, slot, nodeClaim)
		if err != nil {
			return ctrl.Result{}, err
		}

		if bumped && nodeClaim.Status.Attempt == claim.MaxConsecutiveAttempts {
			r.eventf(nodeClaim, "ShutdownAttemptLimitReached",
				"scale-down of slot %s has spent %d of %d attempts and the machine is still observed up: the controller stops firing the shutdown and records it here, because a shutdown Job that exits 0 without the machine going off is the pool's script, not the controller",
				slot.Name, claim.MaxConsecutiveAttempts, claim.MaxConsecutiveAttempts)
		}

		logger.V(1).Info("Claim is being deleted and the machine is still up; keeping the finalizer",
			"claim", nodeClaim.Name, "slot", slot.Name, "phase", string(nodeClaim.Status.Phase))

		return ctrl.Result{RequeueAfter: shutdownPollResync}, nil
	}

	// The machine is off. The scale-down landed: a live claim ends completed, and the digest the slot
	// keeps says which request it was.
	if phase.IsLive(nodeClaim.Status.Phase) {
		if _, err := recorder.MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimCompleted, reasonForDeletion(nodeClaim)); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := StampDigest(ctx, r.Client, slot, nodeClaim); err != nil {
		logger.V(1).Info("Failed to stamp the slot's digest of the deleted claim", "claim", nodeClaim.Name, "error", err.Error())
	}

	if controllerutil.ContainsFinalizer(nodeClaim, ClaimFinalizer) {
		controllerutil.RemoveFinalizer(nodeClaim, ClaimFinalizer)
		if err := r.Update(ctx, nodeClaim); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
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
// operator-visible bug and not a supported state (docs/design/nodeclaim.md §8).
func (r *NodeClaimReconciler) refuseExtraLiveClaims(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) error {
	if !phase.IsLive(nodeClaim.Status.Phase) {
		return nil
	}

	claims, err := claim.ListOfSlot(ctx, r.Client, nodeClaim.Namespace, claim.SlotNameOf(nodeClaim))
	if err != nil {
		return err
	}

	extras := make([]*infrav2alpha1.NodeClaim, 0, len(claims))

	for i := range claims {
		existing := &claims[i]
		if !phase.IsLive(existing.Status.Phase) || existing.Name == nodeClaim.Name {
			continue
		}

		// The newest live claim owns the slot; ties break on the name, so the rule is deterministic.
		if existing.CreationTimestamp.After(nodeClaim.CreationTimestamp.Time) ||
			(existing.CreationTimestamp.Equal(&nodeClaim.CreationTimestamp) && existing.Name < nodeClaim.Name) {
			extras = append(extras, nodeClaim)

			break
		}

		extras = append(extras, existing)
	}

	for _, extra := range extras {
		if extra.Name == nodeClaim.Name {
			r.eventf(nodeClaim, "SlotAlreadyClaimed",
				"slot %s already carries the live claim %s: one live claim per slot, this claim is failed as superseded",
				claim.SlotNameOf(nodeClaim), extra.Name)

			if _, err := r.recorder().MoveOf(ctx, nodeClaim, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonSuperseded); err != nil {
				return err
			}

			return nil
		}

		r.eventf(extra, "SlotAlreadyClaimed",
			"slot %s already carries the live claim %s: one live claim per slot, this claim is failed as superseded",
			claim.SlotNameOf(extra), nodeClaim.Name)

		if _, err := r.recorder().Move(ctx, client.ObjectKeyFromObject(extra), infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonSuperseded); err != nil {
			return err
		}
	}

	return nil
}

// ensureFinalizer puts the finalizer on a claim whose boot has started, and only then: the whole
// point of the finalizer is that a delete of a committed boot is not an abandonment, and a pending
// claim must stay cheap to cancel (docs/design/nodeclaim.md §3).
func (r *NodeClaimReconciler) ensureFinalizer(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) error {
	if !controllerutil.ContainsFinalizer(nodeClaim, ClaimFinalizer) {
		if nodeClaim.Status.Phase == infrav2alpha1.NodeClaimPending {
			return nil
		}

		if !phase.IsLive(nodeClaim.Status.Phase) {
			return nil
		}

		controllerutil.AddFinalizer(nodeClaim, ClaimFinalizer)

		if err := r.Update(ctx, nodeClaim); err != nil {
			return err
		}
	}

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
		if _, err := r.recorder().MoveOf(ctx, nodeClaim, nodeClaim.Status.Phase, nodeClaim.Status.Reason); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: idleResync}, nil
	}

	ttl := time.Duration(claim.DefaultTTLSecondsAfterFinished) * time.Second
	if nodeClaim.Spec.TTLSecondsAfterFinished != nil {
		ttl = time.Duration(*nodeClaim.Spec.TTLSecondsAfterFinished) * time.Second
	}

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

// recorder returns the status writer, defaulting to one bound to this reconciler's client.
func (r *NodeClaimReconciler) recorder() *claim.Recorder {
	if r.Recorder != nil {
		return r.Recorder
	}

	r.Recorder = claim.NewRecorder(r.Client)

	return r.Recorder
}

// eventf publishes an Event on the claim, and is a no-op without a recorder.
func (r *NodeClaimReconciler) eventf(nodeClaim *infrav2alpha1.NodeClaim, reason, format string, args ...interface{}) {
	if r.EventRecorder == nil || nodeClaim == nil {
		return
	}

	// Every record the claim controller publishes is a Warning: each one is an attempt that
	// ended without a usable machine, which is what an operator has to be told to go look.
	r.EventRecorder.Eventf(nodeClaim, nil, corev1.EventTypeWarning, reason, "nodeclaim", format, args...)
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav2alpha1.NodeClaim{}).
		Named("nodeclaim").
		Complete(r)
}
