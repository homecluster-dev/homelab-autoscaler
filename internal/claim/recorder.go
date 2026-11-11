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

package claim

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// Finalizer is the finalizer a claim carries from the moment its phase becomes booting. It is what
// makes "a boot that has started is never cancelled" true: deleting a booting or ready claim is not
// an abandonment of the machine, it is the scale-down request, and the finalizer runs the object to
// completed / failed once the machine is off (docs/design/nodeclaim.md §3).
//
// A pending claim carries no finalizer, so canceling a pending claim is a plain delete: nothing is
// in flight, the object goes, and the unit comes off the target size on that write.
//
// It lives here, with the recorder, because the recorder is its writer: the finalizer lands on the
// same write that records the pending -> booting move, so there is no pass boundary in which a
// DeleteNodes can delete a committed boot with no shutdown behind it.
const Finalizer = "infra.homecluster.dev/nodeclaim-finalizer"

// HasFinalizer reports whether a claim carries the scale-down finalizer.
func HasFinalizer(c *infrav2alpha1.NodeClaim) bool {
	return c != nil && controllerutil.ContainsFinalizer(c, Finalizer)
}

// ErrIllegalMove is returned by Recorder.Move for a phase transition the model does not allow, the
// one that matters being a cancel of a booting claim: "pending is cancelable, booting is
// committed" (docs/design/nodeclaim.md §3). A caller that asks for it gets an error naming both
// phases, and nothing is written.
var ErrIllegalMove = errors.New("illegal NodeClaim phase transition")

// LegalMoves is the phase table of the claim, §3 of the design as data:
//
//	pending  -> booting, completed, failed
//	booting  -> ready, failed
//	ready    -> completed, failed
//	failed, completed -> terminal, no outgoing edge
//
// pending -> completed is in the table because a pending claim deleted by DecreaseTargetSize is
// collected through its finalizer-less path, and the digest of an episode that ended before a
// machine was bought reads completed, not failed: the machine was never wanted again.
var LegalMoves = map[infrav2alpha1.NodeClaimPhase][]infrav2alpha1.NodeClaimPhase{
	infrav2alpha1.NodeClaimPending: {
		infrav2alpha1.NodeClaimBooting,
		infrav2alpha1.NodeClaimReady,
		infrav2alpha1.NodeClaimCompleted,
		infrav2alpha1.NodeClaimFailed,
	},
	infrav2alpha1.NodeClaimBooting: {
		infrav2alpha1.NodeClaimReady,
		infrav2alpha1.NodeClaimCompleted,
		infrav2alpha1.NodeClaimFailed,
	},
	infrav2alpha1.NodeClaimReady: {
		infrav2alpha1.NodeClaimCompleted,
		infrav2alpha1.NodeClaimFailed,
	},
	infrav2alpha1.NodeClaimFailed:    {},
	infrav2alpha1.NodeClaimCompleted: {},
}

// CanMove reports whether a phase transition is legal. The same phase is a no-op, not a move, and
// is reported as allowed so a re-apply of a completed write is idempotent.
func CanMove(from, to infrav2alpha1.NodeClaimPhase) bool {
	if from == to {
		return true
	}

	for _, allowed := range LegalMoves[from] {
		if allowed == to {
			return true
		}
	}

	return false
}

// Recorder writes the status of claims: the attempt seed, and every phase move. Both the claim
// controller (deadlines, the attempt bound, the finalizer of a scale-down, collection) and the
// lifecycle transition path (the pending -> booting write the startup Job hangs off, the
// booting -> ready write the readiness gate answers for) go through it, so the phase table above
// is enforced in one place and the claim has one set of legal moves whatever the caller.
type Recorder struct {
	Client client.Client
}

// NewRecorder returns a Recorder bound to a client.
func NewRecorder(c client.Client) *Recorder {
	return &Recorder{Client: c}
}

// Seed records the attempt number a fresh claim carries, from the slot's digest (see SeedAttempt).
// It writes once: a claim that already carries an attempt is left alone, so the number a slot's
// history produced is not recomputed on every pass.
func (r *Recorder) Seed(ctx context.Context, key client.ObjectKey, slot *infrav2alpha1.Node) (bool, error) {
	seeded := SeedAttempt(slot.Status.LastClaim)
	wrote := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.NodeClaim{}
		if err := r.Client.Get(ctx, key, fresh); err != nil {
			return err
		}

		if fresh.Status.Attempt != 0 {
			return nil
		}

		fresh.Status.Attempt = seeded

		if err := r.Client.Status().Update(ctx, fresh); err != nil {
			return err
		}

		wrote = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("seeding the attempt of claim %s: %w", key.Name, err)
	}

	return wrote, nil
}

// Move applies one phase transition, with its reason, and stamps the conditions and the finish time
// that go with it. It re-reads the claim, so it moves the object the cluster has, not a caller's
// stale copy, and it refuses a move the table refuses (see ErrIllegalMove).
//
// A move is one phase, and it can need two writes, because a claim carries two things that are not
// status (see closeBootWindow): the finalizer of a committed boot, and `spec.deadlineAt`, the budget
// of a boot that is still running. The metadata/spec write is applied first, so a manager that dies
// between the two leaves the boot committed and the deadline intact - the safe direction, the one
// where the claim controller lands the phase on its next pass.
func (r *Recorder) Move(ctx context.Context, key client.ObjectKey, to infrav2alpha1.NodeClaimPhase, reason string) (bool, error) {
	moved := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.NodeClaim{}
		if err := r.Client.Get(ctx, key, fresh); err != nil {
			return err
		}

		from := fresh.Status.Phase
		if from == "" {
			from = infrav2alpha1.NodeClaimPending
		}

		if !CanMove(from, to) {
			return fmt.Errorf("%w: %s -> %s (claim %s, slot %s)", ErrIllegalMove, from, to, fresh.Name, SlotNameOf(fresh))
		}

		idempotent := from == to && (reason == "" || fresh.Status.Reason == reason)

		restamped, err := r.closeBootWindow(ctx, fresh, to)
		if err != nil {
			return err
		}

		if idempotent && !restamped {
			return nil
		}

		fresh.Status.Phase = to
		if reason != "" {
			fresh.Status.Reason = reason
		}

		if phase.IsTerminal(to) {
			now := metav1.NewTime(time.Now())
			fresh.Status.FinishedAt = &now
			r.stampTerminalConditions(fresh, from, to, reason)
		} else {
			r.stampLiveConditions(fresh, to)
		}

		if err := r.Client.Status().Update(ctx, fresh); err != nil {
			return err
		}

		moved = true

		return nil
	})
	if err != nil {
		return false, err
	}

	return moved, nil
}

// closeBootWindow writes the non-status half of two moves, and reports whether it wrote:
//
//   - `-> booting`: the finalizer arrives with the boot, on the same write that records the phase.
//     A finalizer that lands on a later pass of the claim controller leaves a window in which a
//     DeleteNodes deletes a committed boot with no shutdown behind it (docs/design/nodeclaim.md §3);
//   - `-> ready`: `spec.deadlineAt` is cleared. The deadline is the budget of the boot window, not
//     of the machine's life: a claim that reached ready owns a machine, and a CRD whose deadline
//     field still names an instant in its past is a claim the digest, `kubectl get nodeclaim` and
//     any writer that re-reads the deadline all read as an overdue attempt. Clearing it is what makes
//     the object say what the model means: the attempt is over.
//
// A finalizer is never added to an object the API server has already marked for deletion (it is
// refused), and a claim being deleted has a shutdown running, so the finalizer question is moot there.
func (r *Recorder) closeBootWindow(ctx context.Context, fresh *infrav2alpha1.NodeClaim, to infrav2alpha1.NodeClaimPhase) (bool, error) {
	addFinalizer := to == infrav2alpha1.NodeClaimBooting &&
		fresh.DeletionTimestamp.IsZero() &&
		!controllerutil.ContainsFinalizer(fresh, Finalizer)
	clearDeadline := to == infrav2alpha1.NodeClaimReady && fresh.Spec.DeadlineAt != nil

	if !addFinalizer && !clearDeadline {
		return false, nil
	}

	if addFinalizer {
		controllerutil.AddFinalizer(fresh, Finalizer)
	}

	if clearDeadline {
		fresh.Spec.DeadlineAt = nil
	}

	if err := r.Client.Update(ctx, fresh); err != nil {
		return false, fmt.Errorf("writing the metadata of claim %s for its move to %s: %w", fresh.Name, to, err)
	}

	return true, nil
}

// MoveOf is Move for a claim the caller already has in hand.
func (r *Recorder) MoveOf(ctx context.Context, c *infrav2alpha1.NodeClaim, to infrav2alpha1.NodeClaimPhase, reason string) (bool, error) {
	return r.Move(ctx, client.ObjectKeyFromObject(c), to, reason)
}

// stampLiveConditions records the conditions of a live phase: MachineCommitted from booting on (the
// attempt bought a machine, and the claim is no longer cancelable), Ready at ready.
func (r *Recorder) stampLiveConditions(c *infrav2alpha1.NodeClaim, to infrav2alpha1.NodeClaimPhase) {
	if to != infrav2alpha1.NodeClaimPending {
		stampCondition(c, infrav2alpha1.ClaimConditionMachineCommitted, metav1.ConditionTrue, string(to),
			"the startup Job of the attempt exists: a boot that has started is never cancelled")
	}

	stampCondition(c, infrav2alpha1.ClaimConditionReady,
		BoolStatus(to == infrav2alpha1.NodeClaimReady), string(to),
		claimReadyMessage(to))
}

// stampTerminalConditions records the conditions of a terminal phase, read from the edge it moved
// along:
//
//   - Ready is False: the claim no longer asserts that a machine can take the pool's work, whatever
//     it asserted a pass ago;
//   - MachineCommitted is the answer to "did this attempt ever buy a machine", and on a terminal move
//     that is a question about the phase it moved *from*: a claim that ended from booting or ready
//     committed a machine, and the machine is the thing the input law now powers off; a claim that
//     ended from pending never bought one, and there is nothing to clean up. Writing the phase it
//     moved *to* here would erase the difference between the two, and the condition is the one an
//     operator reads to answer "is there a machine behind this dead claim".
//
// It is a record, never an instruction: the shutdown is decided from the observation, not from a
// condition (docs/architecture/state.md's authority table).
func (r *Recorder) stampTerminalConditions(c *infrav2alpha1.NodeClaim, from, to infrav2alpha1.NodeClaimPhase, reason string) {
	stampCondition(c, infrav2alpha1.ClaimConditionReady, metav1.ConditionFalse, string(to), reason)
	stampCondition(c, infrav2alpha1.ClaimConditionMachineCommitted,
		BoolStatus(phase.IsLive(from) && from != infrav2alpha1.NodeClaimPending), string(to), reason)
}

// claimReadyMessage is the message of the Ready condition of a live claim.
func claimReadyMessage(to infrav2alpha1.NodeClaimPhase) string {
	if to == infrav2alpha1.NodeClaimReady {
		return "the machine is up, joined, and free of the pool's readiness taints: it can take the pool's work"
	}

	return "the machine this claim asked for is not usable yet"
}

// BoolStatus maps a boolean onto a condition status.
func BoolStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}

	return metav1.ConditionFalse
}

// stampCondition writes one condition, keeping the LastTransitionTime stable while the Status does
// not flip (the api-conventions rule the rest of this repository follows).
func stampCondition(c *infrav2alpha1.NodeClaim, conditionType string, status metav1.ConditionStatus, reason, message string) {
	cond := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}

	existing := false
	for i := range c.Status.Conditions {
		if c.Status.Conditions[i].Type == conditionType {
			if c.Status.Conditions[i].Status == status {
				cond.LastTransitionTime = c.Status.Conditions[i].LastTransitionTime
			}

			c.Status.Conditions[i] = cond
			existing = true

			break
		}
	}

	if !existing {
		c.Status.Conditions = append(c.Status.Conditions, cond)
	}
}

// StampFinishedAt gives a terminal claim the finish time its TTL is measured from, and reports
// whether it wrote. It exists for the claim the collector found terminal with no clock: a phase
// landed on by a writer that stamps no time (a hand write, a status-only apply) is otherwise never
// collected, because the move that would stamp the time is an idempotent no-op, and the TTL of an
// object that never finished has no instant to start from. Nothing accumulates on a broken pool.
func (r *Recorder) StampFinishedAt(ctx context.Context, key client.ObjectKey) (bool, error) {
	wrote := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.NodeClaim{}
		if err := r.Client.Get(ctx, key, fresh); err != nil {
			return err
		}

		if !phase.IsTerminal(fresh.Status.Phase) {
			return nil
		}

		if fresh.Status.FinishedAt != nil {
			return nil
		}

		now := metav1.NewTime(time.Now())
		fresh.Status.FinishedAt = &now

		if err := r.Client.Status().Update(ctx, fresh); err != nil {
			return err
		}

		wrote = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("stamping the finish time of claim %s: %w", key.Name, err)
	}

	return wrote, nil
}

// RecordMachineLoss stamps the MachineObservedOff condition of a claim whose machine the provider
// observes off, and returns the instant the loss was first observed plus whether this call wrote. It
// is the clock of the MachineGone bound: the first pass that sees the machine off writes the
// condition and its lastTransitionTime, every later pass reads it back, and only an observation that
// outlasts a whole boot window ends the claim (docs/design/nodeclaim.md §4). A reboot of a healthy
// machine shows the same shape for minutes, which is why the case is bounded by an observation and
// not by a single pass.
func (r *Recorder) RecordMachineLoss(ctx context.Context, key client.ObjectKey) (time.Time, bool, error) {
	first := time.Time{}
	wrote := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.NodeClaim{}
		if err := r.Client.Get(ctx, key, fresh); err != nil {
			return err
		}

		if seen, ok := MachineLossObservedSince(fresh); ok {
			first = seen

			return nil
		}

		stampCondition(fresh, infrav2alpha1.ClaimConditionMachineObservedOff, metav1.ConditionTrue,
			infrav2alpha1.ClaimReasonMachineGone,
			"the provider observes the machine of a ready claim off: the loss is being watched, and a loss that outlasts one boot window fails the claim")

		first = time.Now()

		if err := r.Client.Status().Update(ctx, fresh); err != nil {
			return err
		}

		wrote = true

		return nil
	})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("recording the machine loss of claim %s: %w", key.Name, err)
	}

	return first, wrote, nil
}

// ClearMachineLoss flips the MachineObservedOff condition back to False when the machine of a claim
// is observed up again, and reports whether it wrote. A condition that is never cleared makes the
// next loss of the same machine instantaneously old, and a claim whose machine came back for three
// days would be failed by a five minute blip.
func (r *Recorder) ClearMachineLoss(ctx context.Context, key client.ObjectKey) (bool, error) {
	wrote := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.NodeClaim{}
		if err := r.Client.Get(ctx, key, fresh); err != nil {
			return err
		}

		if _, ok := MachineLossObservedSince(fresh); !ok {
			return nil
		}

		stampCondition(fresh, infrav2alpha1.ClaimConditionMachineObservedOff, metav1.ConditionFalse,
			string(fresh.Status.Phase), "the machine of the claim is observed up again: the loss is over")

		if err := r.Client.Status().Update(ctx, fresh); err != nil {
			return err
		}

		wrote = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("clearing the machine loss of claim %s: %w", key.Name, err)
	}

	return wrote, nil
}

// MachineLossObservedSince reads the instant the loss of a claim's machine was first recorded, and
// reports whether the claim carries one.
func MachineLossObservedSince(c *infrav2alpha1.NodeClaim) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}

	for _, cond := range c.Status.Conditions {
		if cond.Type != infrav2alpha1.ClaimConditionMachineObservedOff {
			continue
		}

		if cond.Status != metav1.ConditionTrue {
			return time.Time{}, false
		}

		return cond.LastTransitionTime.Time, true
	}

	return time.Time{}, false
}

// Get reads a claim by name, and reports "not found" as nil, nil: a claim that is gone is a claim
// that ended, and every caller of this is a path that has to be idempotent against collection.
func Get(ctx context.Context, c client.Client, key types.NamespacedName) (*infrav2alpha1.NodeClaim, error) {
	got := &infrav2alpha1.NodeClaim{}
	if err := c.Get(ctx, key, got); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}

		return nil, err
	}

	return got, nil
}
