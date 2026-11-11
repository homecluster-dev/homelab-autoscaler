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
}

// claimWriterFor builds the writer for a client.
func claimWriterFor(c client.Client) *transitionClaimWriter {
	return &transitionClaimWriter{recorder: claim.NewRecorder(c)}
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

// StampDigest copies a claim's summary onto the slot's status.lastClaim, so that
// `kubectl describe node.infra.homecluster.dev <slot>` answers "who asked, when, how many attempts,
// why did it end" after the claim has been collected (docs/design/nodeclaim.md §8).
//
// It writes only when the record differs, so a resync of a slot whose last claim has not moved
// writes nothing. It never stamps a summary of a claim that is not the newest of its slot: the
// digest is the story of the last episode, and a late write from a claim that has been superseded
// would rewrite it.
func StampDigest(ctx context.Context, c client.Client, slot *infrav2alpha1.Node, nodeClaim *infrav2alpha1.NodeClaim) error {
	if nodeClaim == nil {
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

// BumpScaleDownAttempt counts one more shutdown attempt on the scale-down claim of a slot whose
// machine the provider still observes up, and stamps the streak onto the slot's digest. The streak
// is what claim.ScaleDownStreakExhausted bounds, and the count lives on the claim that asked for
// the scale-down, so it ends when the claim ends.
func BumpScaleDownAttempt(ctx context.Context, c client.Client, slot *infrav2alpha1.Node, nodeClaim *infrav2alpha1.NodeClaim) (bool, error) {
	if nodeClaim == nil {
		return false, nil
	}

	var bumped bool

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.NodeClaim{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(nodeClaim), fresh); err != nil {
			return err
		}

		if fresh.Status.Attempt >= claim.MaxConsecutiveAttempts {
			return nil
		}

		fresh.Status.Attempt++
		bumped = true

		return c.Status().Update(ctx, fresh)
	})
	if err != nil {
		return false, err
	}

	if !bumped {
		return false, nil
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(nodeClaim), nodeClaim); err != nil {
		return true, err
	}

	return true, StampDigest(ctx, c, slot, nodeClaim)
}

// sameDigest compares two digests on the fields an operator reads, so a resync writes nothing.
func sameDigest(a, b *infrav2alpha1.LastClaimSummary) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Name == b.Name &&
		a.Phase == b.Phase &&
		a.Attempt == b.Attempt &&
		a.Call == b.Call &&
		a.Group == b.Group &&
		a.Reason == b.Reason &&
		sameTime(a.RequestedAt, b.RequestedAt) &&
		sameTime(a.DeadlineAt, b.DeadlineAt) &&
		sameTime(a.FinishedAt, b.FinishedAt)
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
