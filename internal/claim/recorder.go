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

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

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

		if from == to && (reason == "" || fresh.Status.Reason == reason) {
			return nil
		}

		fresh.Status.Phase = to
		if reason != "" {
			fresh.Status.Reason = reason
		}

		if phase.IsTerminal(to) {
			now := metav1.NewTime(time.Now())
			fresh.Status.FinishedAt = &now
			r.stampTerminalConditions(fresh, to, reason)
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

// stampTerminalConditions records the conditions of a terminal phase.
func (r *Recorder) stampTerminalConditions(c *infrav2alpha1.NodeClaim, to infrav2alpha1.NodeClaimPhase, reason string) {
	stampCondition(c, infrav2alpha1.ClaimConditionReady, metav1.ConditionFalse, string(to), reason)
	stampCondition(c, infrav2alpha1.ClaimConditionMachineCommitted,
		BoolStatus(phase.IsLive(c.Status.Phase)), string(to), reason)
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
