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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
)

// transitionClaimWriter is the fsm.ClaimWriter the node state machine moves claims through: a thin
// adapter over claim.Recorder, which owns the phase table. It exists so that internal/fsm does not
// import internal/claim (the claim package imports the fsm constants its budget is measured from,
// and an edge in the other direction would be a cycle), and so that the legality of a move has
// exactly one enforcement point whatever the caller.
type transitionClaimWriter struct {
	recorder *claim.Recorder
	client   client.Client

	// slot is the slot this state machine is running on: the shutdown streak is counted on its
	// claim and mirrored onto its digest, and the writer is built per pass with the slot in hand.
	slot *infrav2alpha1.Node
}

// claimWriterFor builds the writer for a client, bound to the slot the state machine runs on.
func claimWriterFor(c client.Client, slot *infrav2alpha1.Node) *transitionClaimWriter {
	return &transitionClaimWriter{recorder: claim.NewRecorder(c), client: c, slot: slot}
}

// MoveClaim applies one phase move of the claim. The in-memory copy the caller passes is updated
// with the phase that landed, so the caller's next derivation sees the move it just made.
func (w *transitionClaimWriter) MoveClaim(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim, to infrav2alpha1.NodeClaimPhase, reason string) error {
	if nodeClaim == nil {
		return nil
	}

	moved, err := w.recorder.Move(ctx, client.ObjectKeyFromObject(nodeClaim), to, reason)
	if err != nil {
		return err
	}

	if moved || nodeClaim.Status.Phase == to {
		nodeClaim.Status.Phase = to
	}

	return nil
}

// RecordShutdownAttempt is the shutdown attempt of the scale-down this pass fired: the state machine
// calls it on the path that created the shutdown Job, so the streak the bound reads is a streak of
// shutdowns and not a streak of reconcile passes (see BumpScaleDownAttempt).
func (w *transitionClaimWriter) RecordShutdownAttempt(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) error {
	if nodeClaim == nil {
		return nil
	}

	_, _, err := BumpScaleDownAttempt(ctx, w.client, w.slot, nodeClaim)

	return err
}

// StampDigest copies a claim's summary onto the slot's status.lastClaim, so that
// `kubectl describe node.infra.homecluster.dev <slot>` answers "who asked, when, how many attempts,
// why did it end" after the claim has been collected (docs/design/nodeclaim.md §8).
//
// It writes only when the record differs, so a resync of a slot whose last claim has not moved
// writes nothing. It never stamps a summary of a claim that is not the newest of its slot: the digest
// is the story of the last episode, and a late write from a claim that has been superseded, or from
// an episode whose collection is landing after the next request was already made, would rewrite it
// with the older story. The ordering check is claim.LiveOfSlot's rule (newest creation timestamp,
// ties broken on the name) over every claim of the slot, live or not.
func StampDigest(ctx context.Context, c client.Client, slot *infrav2alpha1.Node, nodeClaim *infrav2alpha1.NodeClaim) error {
	if nodeClaim == nil {
		return nil
	}

	newest, err := newestClaimOfSlot(ctx, c, slot.Namespace, slot.Name)
	if err != nil {
		return err
	}

	if newest != nil && isNewerClaim(newest, nodeClaim) {
		// Some other claim of this slot is newer than the one being stamped: its episode is the
		// story the digest tells, and the pass that writes that one is the writer. A claim that is
		// itself the newest one is the writer, and stamps.
		return nil
	}

	digest := claim.Digest(nodeClaim)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.Node{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(slot), fresh); err != nil {
			return err
		}

		if sameDigest(fresh.Status.LastClaim, digest) {
			return nil
		}

		fresh.Status.LastClaim = digest

		return c.Status().Update(ctx, fresh)
	})
}

// newestClaimOfSlot returns the newest claim of a slot, by the ordering the model uses everywhere
// (isNewerClaim: creation timestamp, ties broken on the name so the rule is deterministic). nil when the
// slot has no claims at all, which is the case of a claim being deleted that no longer has siblings.
func newestClaimOfSlot(ctx context.Context, c client.Client, namespace, slotName string) (*infrav2alpha1.NodeClaim, error) {
	items, err := claim.ListOfSlot(ctx, c, namespace, slotName)
	if err != nil {
		return nil, err
	}

	if len(items) == 0 {
		return nil, nil
	}

	newest := &items[0]
	for i := range items[1:] {
		candidate := &items[i+1]
		if isNewerClaim(candidate, newest) {
			newest = candidate
		}
	}

	return newest, nil
}

// BumpScaleDownAttempt counts one more shutdown attempt on the scale-down claim of a slot whose
// machine the provider still observes up, and stamps the streak onto the slot's digest. The streak is
// what claim.ScaleDownStreakSpent bounds, and the count lives on the claim that asked for the
// scale-down, so it ends when the claim ends.
//
// Its one caller is the shutdown-firing path: the state machine's ShutdownNode, on the branch that
// created the shutdown Job (fsm.recordShutdownAttempt, reached through the claim writer in this
// package). That is the site where an attempt exists. The claim controller's finalizer poll of the
// observation (reconcileDeletion) reads the count and writes nothing: a streak counted per reconcile
// is a streak of polling - at the 15s poll, three "attempts" are 45s of looking at a machine with one
// Job behind it - and the bound then trips before it has bounded anything, starting the 30m pause
// early.
//
// The count is status.shutdownAttempts, a counter of its own, and never status.attempt: status.attempt
// is the number of machines this slot has been asked to buy, it is the seed of the next claim's
// attempt, and a shutdown that will not land must not spend the boot budget of the episode that is
// draining (docs/design/nodeclaim.md §4).
//
// It returns whether the count moved, and whether the move was a reset: a streak that is spent with
// its grace past starts from 1 again, so a pool whose shutdown script has been fixed is picked up by
// a later pass. The records of the bound - the refusal and the end of the pause - are published by
// the pass that applies them (scaleDownStreakRefusal), because that pass is the one that knows the
// machine is still up.
func BumpScaleDownAttempt(ctx context.Context, c client.Client, slot *infrav2alpha1.Node, nodeClaim *infrav2alpha1.NodeClaim) (bool, bool, error) {
	if nodeClaim == nil {
		return false, false, nil
	}

	var bumped, reset bool

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.NodeClaim{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(nodeClaim), fresh); err != nil {
			return err
		}

		if spent, _ := claim.ScaleDownStreakSpent(fresh); spent {
			// Inside the reset grace of a spent streak: there is nothing to count, and the refusal
			// (internal/controller/infra/lifecycle.go) is the record.
			return nil
		}

		if fresh.Status.ShutdownAttempts >= claim.MaxConsecutiveAttempts {
			reset = true
			fresh.Status.ShutdownAttempts = 0
		}

		fresh.Status.ShutdownAttempts++

		now := metav1.NewTime(time.Now())
		fresh.Status.LastShutdownAttemptAt = &now

		bumped = true

		return c.Status().Update(ctx, fresh)
	})
	if err != nil {
		return false, false, err
	}

	if !bumped {
		return false, false, nil
	}

	// Re-Get, so the caller's copy carries the count that landed and the digest stamped from it
	// names the attempt this pass bought.
	if err := c.Get(ctx, client.ObjectKeyFromObject(nodeClaim), nodeClaim); err != nil {
		return true, reset, err
	}

	return true, reset, StampDigest(ctx, c, slot, nodeClaim)
}

// sameDigest compares two digests on the fields an operator reads, so a resync writes nothing.
func sameDigest(a, b *infrav2alpha1.LastClaimSummary) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Name == b.Name &&
		a.Phase == b.Phase &&
		a.Attempt == b.Attempt &&
		a.ShutdownAttempts == b.ShutdownAttempts &&
		a.Call == b.Call &&
		a.Group == b.Group &&
		a.Reason == b.Reason &&
		sameTime(a.RequestedAt, b.RequestedAt) &&
		sameTime(a.DeadlineAt, b.DeadlineAt) &&
		sameTime(a.FinishedAt, b.FinishedAt) &&
		sameTime(a.LastShutdownAttemptAt, b.LastShutdownAttemptAt)
}

// sameTime compares two optional timestamps, nil included.
func sameTime(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Time.Equal(b.Time)
}

// requeueAt folds a deadline into a reconcile result: the earliest of what the caller already asked
// for and what the deadline asks for, so a claim's deadline is looked at on time and an earlier
// requeue is never delayed.
func requeueAt(result ctrl.Result, at time.Time) ctrl.Result {
	if at.IsZero() {
		return result
	}

	wait := time.Until(at)
	if wait <= 0 {
		wait = time.Second
	}

	//nolint:staticcheck // SA1019: reading Requeue is the point: controller-runtime refuses a Result
	// that sets Requeue and RequeueAfter together, so a caller that asked for "now" is left alone.
	if result.Requeue {
		return result
	}

	if result.RequeueAfter > 0 && result.RequeueAfter <= wait {
		return result
	}

	result.RequeueAfter = wait

	return result
}
