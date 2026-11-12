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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// claimPhasesInOrder is the phase set of the claim, in the order the design's table lists it
// (docs/design/nodeclaim.md §3): the three live phases, then the two terminal ones.
var claimPhasesInOrder = []infrav2alpha1.NodeClaimPhase{
	infrav2alpha1.NodeClaimPending,
	infrav2alpha1.NodeClaimBooting,
	infrav2alpha1.NodeClaimReady,
	infrav2alpha1.NodeClaimFailed,
	infrav2alpha1.NodeClaimCompleted,
}

// theImplementedPhaseTable is claim.LegalMoves as docs/architecture/state.md states it ("Claim
// phases, and who writes each move"): the design's §3 sketch, plus the two edges the implementation
// settled in §11 - a pending claim may be stamped ready when the machine it asked for is up before
// the boot stamp lands (a manual claim on a running slot), and a booting claim ends completed on
// the finalizer path of a scale-down (the boot is reversed, never interrupted).
var theImplementedPhaseTable = map[infrav2alpha1.NodeClaimPhase][]infrav2alpha1.NodeClaimPhase{
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

// TestLegalMovesIsThePhaseTable asserts the table row by row. A row of the table is a set of edges
// with a name attached to each one (the startup Job, the readiness gate, the deadline, the
// scale-down), so a new edge added to the map is a new writer of the claim, and this test is where
// that has to be said out loud.
func TestLegalMovesIsThePhaseTable(t *testing.T) {
	require.Len(t, LegalMoves, len(theImplementedPhaseTable), "the table covers every phase of the enum")

	for _, from := range claimPhasesInOrder {
		t.Run(string(from)+" has one row", func(t *testing.T) {
			assert.ElementsMatch(t, theImplementedPhaseTable[from], LegalMoves[from],
				"the outgoing edges of %s", from)
		})
	}
}

// TestCanMoveIsTheWholeMatrix runs every pair of the phase set, so the shape of the model is pinned:
// pending is cancelable, booting is committed, ready is the phase that serves pods, and failed and
// completed are ends.
func TestCanMoveIsTheWholeMatrix(t *testing.T) {
	for _, from := range claimPhasesInOrder {
		for _, to := range claimPhasesInOrder {
			name := string(from) + " -> " + string(to)

			want := from == to
			for _, allowed := range theImplementedPhaseTable[from] {
				if allowed == to {
					want = true

					break
				}
			}

			t.Run(name, func(t *testing.T) {
				assert.Equal(t, want, CanMove(from, to))
			})
		}
	}

	// The three properties the design's prose states, spelled as assertions over the matrix so a
	// row edit that breaks one of them fails here and not in production.
	t.Run("pending is the only cancelable phase", func(t *testing.T) {
		// Canceling a pending claim is deleting it: the request ends completed with no machine
		// bought, and the unit comes off the target size on that write.
		assert.True(t, CanMove(infrav2alpha1.NodeClaimPending, infrav2alpha1.NodeClaimCompleted))
		assert.True(t, CanMove(infrav2alpha1.NodeClaimPending, infrav2alpha1.NodeClaimFailed))
	})

	t.Run("a boot that has started is committed", func(t *testing.T) {
		// No phase moves back to pending: the request that bought a machine cannot be un-bought, and
		// a scale-down of a booting claim runs the shutdown path to completed.
		for _, from := range claimPhasesInOrder {
			if from == infrav2alpha1.NodeClaimPending {
				continue
			}

			assert.False(t, CanMove(from, infrav2alpha1.NodeClaimPending),
				"nothing returns a claim to the cancelable phase, and %s least of all", from)
		}
	})

	t.Run("failed and completed are ends", func(t *testing.T) {
		for _, terminal := range []infrav2alpha1.NodeClaimPhase{
			infrav2alpha1.NodeClaimFailed,
			infrav2alpha1.NodeClaimCompleted,
		} {
			for _, to := range claimPhasesInOrder {
				if to == terminal {
					continue
				}

				assert.False(t, CanMove(terminal, to), "%s is terminal, and %s is not its future", terminal, to)
			}
		}
	})

	t.Run("every live phase can be failed", func(t *testing.T) {
		// The deadline is the heal of every attempt that produced nothing, so it has to be reachable
		// from every live phase.
		for _, live := range []infrav2alpha1.NodeClaimPhase{
			infrav2alpha1.NodeClaimPending,
			infrav2alpha1.NodeClaimBooting,
			infrav2alpha1.NodeClaimReady,
		} {
			assert.True(t, CanMove(live, infrav2alpha1.NodeClaimFailed), "%s can hit its deadline", live)
		}
	})

	t.Run("the same phase is a no-op, not a move", func(t *testing.T) {
		for _, claimPhase := range claimPhasesInOrder {
			assert.True(t, CanMove(claimPhase, claimPhase), "re-applying a write of %s is idempotent", claimPhase)
		}
	})

	t.Run("an unstamped phase has no row", func(t *testing.T) {
		// A claim object built in Go has an empty status.phase; the CRD default stamps pending, and
		// the Recorder normalizes the empty value before it asks this question (see
		// TestRecorderMoveNormalizesTheCRDDefault).
		assert.False(t, CanMove("", infrav2alpha1.NodeClaimBooting))
	})
}

// theDesignPhaseTable is the phase table of docs/design/nodeclaim.md §3 as written: the sketch the
// design states, with the "Legal next phases" column verbatim.
var theDesignPhaseTable = map[infrav2alpha1.NodeClaimPhase][]infrav2alpha1.NodeClaimPhase{
	infrav2alpha1.NodeClaimPending: {
		infrav2alpha1.NodeClaimBooting,
		infrav2alpha1.NodeClaimCompleted,
		infrav2alpha1.NodeClaimFailed,
	},
	infrav2alpha1.NodeClaimBooting: {
		infrav2alpha1.NodeClaimReady,
		infrav2alpha1.NodeClaimFailed,
	},
	infrav2alpha1.NodeClaimReady: {
		infrav2alpha1.NodeClaimCompleted,
		infrav2alpha1.NodeClaimFailed,
	},
	infrav2alpha1.NodeClaimFailed:    {},
	infrav2alpha1.NodeClaimCompleted: {},
}

// TestTheDesignPhaseTableIsTheLowBoundOfTheRunningOne keeps the design's promise and the
// implementation's record in one place: every edge §3 states is legal (a move the design promised
// cannot be taken away silently), and the two edges the implementation added are named, because a
// phase table is the one contract of the model and an edge that appears without a sentence attached
// to it is a bug in the design, not in the map.
func TestTheDesignPhaseTableIsTheLowBoundOfTheRunningOne(t *testing.T) {
	for from, edges := range theDesignPhaseTable {
		for _, to := range edges {
			t.Run(string(from)+" -> "+string(to), func(t *testing.T) {
				assert.True(t, CanMove(from, to),
					"the phase table of §3 states this move, so the running table must allow it")
			})
		}
	}

	for _, from := range claimPhasesInOrder {
		t.Run(string(from)+" has no more terminal edges than the design states", func(t *testing.T) {
			assert.ElementsMatch(t, theImplementedPhaseTable[from], LegalMoves[from])
		})
	}

	sharpened := []struct {
		from, to infrav2alpha1.NodeClaimPhase
		why      string
	}{
		{
			from: infrav2alpha1.NodeClaimPending,
			to:   infrav2alpha1.NodeClaimReady,
			why: "a manual claim on a slot whose machine is already up: the readiness gate answers for it, and the report row " +
				"\"pending claim, machine observed up\" is the machine arriving before the boot stamp landed",
		},
		{
			from: infrav2alpha1.NodeClaimBooting,
			to:   infrav2alpha1.NodeClaimCompleted,
			why: "the finalizer path of a scale-down that named a booting claim ends it completed once the machine is observed off: " +
				"a boot is reversed, never interrupted (docs/design/nodeclaim.md §11)",
		},
	}

	for _, edge := range sharpened {
		t.Run(string(edge.from)+" -> "+string(edge.to)+" is an edge the implementation settled", func(t *testing.T) {
			assert.True(t, CanMove(edge.from, edge.to), edge.why)
			assert.NotContains(t, theDesignPhaseTable[edge.from], edge.to,
				"the edge is beyond the design's sketch, and the sentence that justifies it is: %s", edge.why)
		})
	}
}

// TestRecorderMoveIsTheTableEnforced runs the moves through the writer: the phase, the reason, the
// conditions and the finish time a move produces. Every caller - the claim controller and the
// lifecycle transition path - goes through here, so the table is enforced in one place.
func TestRecorderMoveIsTheTableEnforced(t *testing.T) {
	tests := []struct {
		name string
		from infrav2alpha1.NodeClaimPhase
		to   infrav2alpha1.NodeClaimPhase
	}{
		{name: "the startup Job is created", from: infrav2alpha1.NodeClaimPending, to: infrav2alpha1.NodeClaimBooting},
		{name: "the readiness gate answers", from: infrav2alpha1.NodeClaimBooting, to: infrav2alpha1.NodeClaimReady},
		{name: "a manual claim on a machine that is already up", from: infrav2alpha1.NodeClaimPending, to: infrav2alpha1.NodeClaimReady},
		{name: "DecreaseTargetSize cancels a pending claim", from: infrav2alpha1.NodeClaimPending, to: infrav2alpha1.NodeClaimCompleted},
		{name: "the finalizer of a scale-down ends a booting claim", from: infrav2alpha1.NodeClaimBooting, to: infrav2alpha1.NodeClaimCompleted},
		{name: "DeleteNodes lands on a ready machine", from: infrav2alpha1.NodeClaimReady, to: infrav2alpha1.NodeClaimCompleted},
		{name: "the deadline passes while the claim is pending", from: infrav2alpha1.NodeClaimPending, to: infrav2alpha1.NodeClaimFailed},
		{name: "the startup Job fails", from: infrav2alpha1.NodeClaimBooting, to: infrav2alpha1.NodeClaimFailed},
		{name: "the readiness gate times out", from: infrav2alpha1.NodeClaimBooting, to: infrav2alpha1.NodeClaimFailed},
		{name: "the machine vanished under a ready claim", from: infrav2alpha1.NodeClaimReady, to: infrav2alpha1.NodeClaimFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim := recordedClaim(tt.from, 1)
			recorder, cl := recorderWith(t, nodeClaim)

			moved, err := recorder.MoveOf(context.Background(), nodeClaim, tt.to, "Because")
			require.NoError(t, err)
			require.True(t, moved)

			got := getClaim(t, cl, nodeClaim)
			assert.Equal(t, tt.to, got.Status.Phase)
			assert.Equal(t, "Because", got.Status.Reason)

			if !phase.IsTerminal(tt.to) {
				assert.Nil(t, got.Status.FinishedAt, "a live claim has no finish time")
			}

			if phase.IsTerminal(tt.to) {
				require.NotNil(t, got.Status.FinishedAt, "a terminal move stamps the time the TTL measures from")
				assert.False(t, got.Status.FinishedAt.Time.Before(nodeClaim.CreationTimestamp.Time))
			}

			// MachineCommitted is the answer to "did this attempt ever buy a machine". On a live
			// move the phase that answers it is the one it moved to; on a terminal move it is the
			// one it moved *from*: a claim that ends from booting or ready leaves a real machine
			// behind for the input law to power off, a claim that ends from pending leaves
			// nothing. Recording the destination of a terminal move here would call both "no
			// machine", which is the difference an operator is asking about.
			committed := tt.to == infrav2alpha1.NodeClaimBooting || tt.to == infrav2alpha1.NodeClaimReady ||
				tt.from == infrav2alpha1.NodeClaimBooting || tt.from == infrav2alpha1.NodeClaimReady
			assert.Equal(t, committed,
				conditionStatus(t, got, infrav2alpha1.ClaimConditionMachineCommitted) == metav1.ConditionTrue,
				"the attempt bought a machine from booting on, and an end does not un-buy it")
			assert.Equal(t, tt.to == infrav2alpha1.NodeClaimReady,
				conditionStatus(t, got, infrav2alpha1.ClaimConditionReady) == metav1.ConditionTrue,
				"the claim asserts its machine is usable only at ready")
		})
	}
}

// TestRecorderMoveRefusesACancelOfABootingClaim is the one move the model has an error for: a
// caller that tries to un-boot a committed attempt gets ErrIllegalMove, and the object is not
// written. "A boot is reversed, never interrupted" (§3).
func TestRecorderMoveRefusesACancelOfABootingClaim(t *testing.T) {
	tests := []struct {
		name string
		from infrav2alpha1.NodeClaimPhase
		to   infrav2alpha1.NodeClaimPhase
	}{
		{name: "a cancel of a committed boot", from: infrav2alpha1.NodeClaimBooting, to: infrav2alpha1.NodeClaimPending},
		{name: "a boot that never happened", from: infrav2alpha1.NodeClaimReady, to: infrav2alpha1.NodeClaimPending},
		{name: "a claim that is over, restarted", from: infrav2alpha1.NodeClaimCompleted, to: infrav2alpha1.NodeClaimBooting},
		{name: "a failed attempt, revived", from: infrav2alpha1.NodeClaimFailed, to: infrav2alpha1.NodeClaimReady},
		{name: "an end, undone", from: infrav2alpha1.NodeClaimFailed, to: infrav2alpha1.NodeClaimCompleted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim := recordedClaim(tt.from, 2)
			recorder, cl := recorderWith(t, nodeClaim)

			moved, err := recorder.MoveOf(context.Background(), nodeClaim, tt.to, infrav2alpha1.ClaimReasonCancelledByDecreaseTargetSize)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrIllegalMove)
			assert.False(t, moved)
			assert.Contains(t, err.Error(), string(tt.from))
			assert.Contains(t, err.Error(), string(tt.to))
			assert.Contains(t, err.Error(), fixSlot, "the error names the slot an operator has to go look at")

			got := getClaim(t, cl, nodeClaim)
			assert.Equal(t, tt.from, got.Status.Phase, "a refused move writes nothing")
			assert.Empty(t, got.Status.Reason)
			assert.Nil(t, got.Status.FinishedAt)
			assert.Empty(t, got.Status.Conditions)
		})
	}
}

// TestRecorderMoveNormalizesTheCRDDefault: a claim object read from the API carries
// status.phase: pending (the CRD default), and a claim built in Go carries the empty value. The
// Recorder reads the empty value as pending, so the first move of a fresh claim is the pending ->
// booting edge and not an illegal move from a phase that does not exist.
func TestRecorderMoveNormalizesTheCRDDefault(t *testing.T) {
	nodeClaim := recordedClaim("", 0)
	nodeClaim.Status.Attempt = 0
	recorder, cl := recorderWith(t, nodeClaim)

	moved, err := recorder.Move(context.Background(), client.ObjectKeyFromObject(nodeClaim),
		infrav2alpha1.NodeClaimBooting, "")
	require.NoError(t, err)
	assert.True(t, moved)

	got := getClaim(t, cl, nodeClaim)
	assert.Equal(t, infrav2alpha1.NodeClaimBooting, got.Status.Phase)
	assert.Equal(t, metav1.ConditionTrue, conditionStatus(t, got, infrav2alpha1.ClaimConditionMachineCommitted))
}

// TestRecorderMoveIsIdempotent: a re-apply of the write that landed is a no-op. The claim controller
// re-reconciles on every watch event, and the deadline and the attempt bound are both enforced by a
// Move a previous pass already made.
func TestRecorderMoveIsIdempotent(t *testing.T) {
	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimBooting, 2)
	recorder, cl := recorderWith(t, nodeClaim)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(nodeClaim)

	moved, err := recorder.Move(ctx, key, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonDeadlineExceeded)
	require.NoError(t, err)
	require.True(t, moved)

	first := getClaim(t, cl, nodeClaim)
	require.NotNil(t, first.Status.FinishedAt)

	moved, err = recorder.Move(ctx, key, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonDeadlineExceeded)
	require.NoError(t, err)
	assert.False(t, moved, "the second pass of the same move writes nothing")

	second := getClaim(t, cl, nodeClaim)
	assert.Equal(t, first.Status.Phase, second.Status.Phase)
	assert.Equal(t, first.Status.Reason, second.Status.Reason)
	assert.Equal(t, first.Status.FinishedAt, second.Status.FinishedAt, "the finish time of an attempt is written once")
	assert.Equal(t, first.Status.Conditions, second.Status.Conditions)
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion, "a no-op move does not touch the object")
}

// TestRecorderMoveOverwritesAReasonIsTheRecordOfWhy: the same phase with a different reason is a
// write. A claim failed by its deadline and then read by the pass that records the attempt bound
// reports the reason the last writer saw, and the digest that follows it carries that one.
func TestRecorderMoveOverwritesAReason(t *testing.T) {
	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimBooting, 1)
	recorder, cl := recorderWith(t, nodeClaim)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(nodeClaim)

	_, err := recorder.Move(ctx, key, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonDeadlineExceeded)
	require.NoError(t, err)

	moved, err := recorder.Move(ctx, key, infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonAttemptLimitExceeded)
	require.NoError(t, err)
	assert.True(t, moved, "a new reason about the same end is a write, not a no-op")
	assert.Equal(t, infrav2alpha1.ClaimReasonAttemptLimitExceeded, getClaim(t, cl, nodeClaim).Status.Reason)
}

// TestRecorderIsIdempotentOverAnAlreadyMarkedClaim is the scale-down path of the claim controller:
// NodeGroupDeleteNodes marks the claim (the delete, with the request recorded on it first), the
// finalizer runs the machine down, and the claim ends completed. The controller re-reconciles while
// the machine is still observed up, so the second pass must land on the record the first one wrote,
// and must not move the phase, the reason, the finish time, or the attribution.
func TestRecorderIsIdempotentOverAnAlreadyMarkedClaim(t *testing.T) {
	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimReady, 1)
	nodeClaim.Finalizers = []string{"infra.homecluster.dev/nodeclaim-finalizer"}
	nodeClaim.Spec.Request = deleteNodes(fixK8sNode)
	nodeClaim.Spec.RequestedBy = RequestedByAutoscaler

	recorder, cl := recorderWith(t, nodeClaim)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(nodeClaim)

	// The provider's DeleteNodes: record the request, then delete. The finalizer keeps the object.
	require.NoError(t, cl.Delete(ctx, nodeClaim))

	marked := getClaim(t, cl, nodeClaim)
	require.False(t, marked.DeletionTimestamp.IsZero(), "a scale-down names the machine by marking its claim")

	// The first pass that sees the machine off ends the claim.
	moved, err := recorder.Move(ctx, key, infrav2alpha1.NodeClaimCompleted, infrav2alpha1.ClaimReasonNodeDeleted)
	require.NoError(t, err)
	require.True(t, moved)

	landed := getClaim(t, cl, nodeClaim)
	assert.Equal(t, infrav2alpha1.NodeClaimCompleted, landed.Status.Phase)
	assert.Equal(t, infrav2alpha1.ClaimReasonNodeDeleted, landed.Status.Reason)
	require.NotNil(t, landed.Status.FinishedAt)
	assert.Equal(t, metav1.ConditionFalse, conditionStatus(t, landed, infrav2alpha1.ClaimConditionReady))
	assert.Equal(t, metav1.ConditionTrue, conditionStatus(t, landed, infrav2alpha1.ClaimConditionMachineCommitted),
		"the scale-down ended a ready claim: there is a machine, and it is what the shutdown was for")

	// Every later pass of the same scale-down: the request is already recorded, and re-deleting it
	// is not a cancellation.
	for pass := 2; pass <= 4; pass++ {
		moved, err = recorder.Move(ctx, key, infrav2alpha1.NodeClaimCompleted, infrav2alpha1.ClaimReasonNodeDeleted)
		require.NoError(t, err, "pass %d", pass)
		assert.False(t, moved, "pass %d of a landed scale-down writes nothing", pass)
	}

	after := getClaim(t, cl, nodeClaim)
	assert.Equal(t, landed.Status, after.Status, "the record of the episode is written once")
	assert.Equal(t, infrav2alpha1.ClaimCallDeleteNodes, after.Spec.Request.Call)
	assert.Equal(t, []string{fixK8sNode}, after.Spec.Request.Nodes)
	assert.Equal(t, RequestedByAutoscaler, after.Spec.RequestedBy)
	assert.Equal(t, fixGroup, after.Spec.Request.Group)

	// The digest the slot keeps after the collection is the attribution of the request.
	digest := Digest(after)
	assert.Equal(t, infrav2alpha1.ClaimCallDeleteNodes, digest.Call)
	assert.Equal(t, fixGroup, digest.Group)
	assert.Equal(t, infrav2alpha1.NodeClaimCompleted, digest.Phase)
	assert.Equal(t, infrav2alpha1.ClaimReasonNodeDeleted, digest.Reason)
	assert.Equal(t, int32(1), digest.Attempt)
	assert.Equal(t, int32(1), SeedAttempt(digest), "a scale-down that landed ends the streak")
}

// TestRecorderSeedWritesTheAttemptOnce: the seed is a write of the number the slot's history
// produced, and it happens on the claim's first pass. A claim that already carries an attempt is
// left alone, so the streak is not recomputed on every reconcile.
func TestRecorderSeedWritesTheAttemptOnce(t *testing.T) {
	failing := &infrav2alpha1.LastClaimSummary{
		Name:    "remote-worker-2-old",
		Phase:   infrav2alpha1.NodeClaimFailed,
		Attempt: 2,
		Call:    infrav2alpha1.ClaimCallIncreaseSize,
		Group:   fixGroup,
	}

	tests := []struct {
		name         string
		digest       *infrav2alpha1.LastClaimSummary
		startAttempt int32
		wantSeed     int32
		wantWrote    bool
	}{
		{
			name:         "a fresh claim on a slot that never had a machine",
			digest:       nil,
			startAttempt: 0,
			wantSeed:     1,
			wantWrote:    true,
		},
		{
			name:         "a fresh claim on a slot whose last attempt failed",
			digest:       failing,
			startAttempt: 0,
			wantSeed:     3,
			wantWrote:    true,
		},
		{
			name:         "a fresh claim on a slot whose last attempt landed",
			digest:       &infrav2alpha1.LastClaimSummary{Phase: infrav2alpha1.NodeClaimReady, Attempt: 2},
			startAttempt: 0,
			wantSeed:     1,
			wantWrote:    true,
		},
		{
			name:         "a claim that already carries its number is not re-seeded",
			digest:       failing,
			startAttempt: 1,
			wantSeed:     1,
			wantWrote:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPending, tt.startAttempt)
			s := slot()
			s.Status.LastClaim = tt.digest

			recorder, cl := recorderWith(t, nodeClaim, s)
			ctx := context.Background()

			wrote, err := recorder.Seed(ctx, client.ObjectKeyFromObject(nodeClaim), s)
			require.NoError(t, err)
			assert.Equal(t, tt.wantWrote, wrote)

			got := getClaim(t, cl, nodeClaim)
			assert.Equal(t, tt.wantSeed, got.Status.Attempt)
			assert.Equal(t, infrav2alpha1.NodeClaimPending, got.Status.Phase, "the seed writes the number, not the phase")

			// The second pass of the same claim writes nothing, whatever the slot's digest says now:
			// the number of an attempt is fixed when the attempt is born.
			wrote, err = recorder.Seed(ctx, client.ObjectKeyFromObject(nodeClaim), s)
			require.NoError(t, err)
			assert.False(t, wrote, "the seed is written once")
			assert.Equal(t, tt.wantSeed, getClaim(t, cl, nodeClaim).Status.Attempt)
		})
	}
}

// TestRecorderSeedOfAMissingClaimIsAnError: a claim collected by its TTL is a claim that ended, and
// the caller that asks for its seed is a reconcile of an object that is gone. The error is the
// signal; the phase of a claim nobody has is not.
func TestRecorderSeedOfAMissingClaimIsAnError(t *testing.T) {
	recorder, _ := recorderWith(t)

	wrote, err := recorder.Seed(context.Background(),
		types.NamespacedName{Name: "gone-7f4b9c", Namespace: fixNamespace}, slot())
	require.Error(t, err)
	assert.False(t, wrote)
	assert.Contains(t, err.Error(), "gone-7f4b9c")
}

// TestARequestReadsBackWhatItAskedFor runs the life of one claim through the writer - created by
// the provider, seeded by the claim controller, moved by the lifecycle path - and reads every
// attribution field back after each write. The question the v1alpha1 scalar could not answer is
// "who asked, why, for which group, for which machines", and it is answerable only if the writes of
// the status never disturb the spec that carries it.
func TestARequestReadsBackWhatItAskedFor(t *testing.T) {
	s := scheme(t)
	srv := slot()

	nodeClaim, err := New(srv, group(nil), deleteNodes(fixK8sNode, "remote-worker-3"), RequestedByAutoscaler, s)
	require.NoError(t, err)

	recorder, cl := recorderWith(t, nodeClaim)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(nodeClaim)

	seeded, err := recorder.Seed(ctx, key, srv)
	require.NoError(t, err)
	require.True(t, seeded)

	// The pending -> booting write is the attempt boundary: the same write buys the machine.
	moved, err := recorder.Move(ctx, key, infrav2alpha1.NodeClaimBooting, "")
	require.NoError(t, err)
	require.True(t, moved)

	machine, err := recorder.Move(ctx, key, infrav2alpha1.NodeClaimReady, "")
	require.NoError(t, err)
	require.True(t, machine)

	got := getClaim(t, cl, nodeClaim)

	// The attribution, read off the object the cluster has.
	assert.Equal(t, RequestedByAutoscaler, got.Spec.RequestedBy, "requestedBy")
	assert.Equal(t, infrav2alpha1.ClaimCallDeleteNodes, got.Spec.Request.Call, "request.call")
	assert.Equal(t, fixGroup, got.Spec.Request.Group, "request.group")
	assert.Zero(t, got.Spec.Request.Delta, "DeleteNodes names machines, it does not count them")
	assert.Equal(t, []string{fixK8sNode, "remote-worker-3"}, got.Spec.Request.Nodes, "request.nodes")
	assert.Equal(t, fixSlot, SlotNameOf(got), "slotRef.name")
	assert.Equal(t, fixGroup, got.Labels[LabelGroup], "the group label")
	assert.Equal(t, fixSlot, got.Labels[LabelSlot], "the slot label")
	assert.NotEmpty(t, got.Spec.RequestedAt)
	// B1: the boot window closed at ready, and the recorder cleared spec.deadlineAt on that write.
	// A healthy machine's claim that carries a deadline in its past is a claim every later reader -
	// the controller, the digest, `kubectl get nodeclaim` - reads as an overdue attempt.
	assert.Nil(t, got.Spec.DeadlineAt, "the deadline is the budget of the boot, and the boot is over")
	assert.True(t, HasFinalizer(got), "the finalizer arrived with the boot, on the booting write (M5)")

	// The record of the attempt: the number the slot's history produced, and the phase the
	// lifecycle path reported.
	assert.Equal(t, int32(1), got.Status.Attempt, "the first claim of a slot with no history is attempt 1")
	assert.Equal(t, infrav2alpha1.NodeClaimReady, got.Status.Phase)
	assert.Empty(t, got.Status.Reason, "a move with no reason does not invent one")
	assert.Nil(t, got.Status.FinishedAt)

	digest := Digest(got)
	assert.Equal(t, infrav2alpha1.ClaimCallDeleteNodes, digest.Call)
	assert.Equal(t, fixGroup, digest.Group)
	assert.Equal(t, int32(1), digest.Attempt)
	assert.Equal(t, infrav2alpha1.NodeClaimReady, digest.Phase)
	assert.Nil(t, digest.DeadlineAt, "the digest and the CRD agree: the attempt is over, and it has no deadline")
}

// TestRecorderBootingMoveStampsTheFinalizer is M5: the finalizer of a scale-down arrives on the same
// write that records the pending -> booting move. A finalizer stamped by the claim controller on a
// later pass leaves a window in which a DeleteNodes deletes a committed boot with no shutdown behind
// it - the object is gone, the startup Job and the machine are not, and nothing owns them (§3: "a
// boot that has started is never cancelled" is only true if the object that says so cannot be deleted
// while the boot runs).
func TestRecorderBootingMoveStampsTheFinalizer(t *testing.T) {
	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPending, 1)
	require.Empty(t, nodeClaim.Finalizers, "a pending claim is cheap to cancel: it carries no finalizer")

	recorder, cl := recorderWith(t, nodeClaim)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(nodeClaim)

	moved, err := recorder.Move(ctx, key, infrav2alpha1.NodeClaimBooting, "")
	require.NoError(t, err)
	require.True(t, moved)

	got := getClaim(t, cl, nodeClaim)
	assert.Equal(t, infrav2alpha1.NodeClaimBooting, got.Status.Phase)
	assert.True(t, HasFinalizer(got), "the phase and the finalizer land on one write, so there is no pass boundary to delete into")

	// The finalizer of a pending claim's cancelation is not stamped: deleting a pending claim stays a
	// plain delete, which is the whole cancelation path of DecreaseTargetSize.
	pending := recordedClaim(infrav2alpha1.NodeClaimPending, 1)
	pending.Name = "stays-cancelable"
	recorder2, cl2 := recorderWith(t, pending)

	moved, err = recorder2.Move(ctx, client.ObjectKeyFromObject(pending), infrav2alpha1.NodeClaimCompleted,
		infrav2alpha1.ClaimReasonCancelledByDecreaseTargetSize)
	require.NoError(t, err)
	require.True(t, moved)
	assert.False(t, HasFinalizer(getClaim(t, cl2, pending)),
		"a move that never entered the boot window stamps no finalizer")
}

// TestRecorderReadyMoveClosesTheBootWindow is B1 in the writer: reaching ready ends the boot budget,
// and the deadline is cleared so the object says what the model means. The move is idempotent
// afterwards: the second pass writes nothing, because the deadline it would clear is already gone.
func TestRecorderReadyMoveClosesTheBootWindow(t *testing.T) {
	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimBooting, 1)
	deadline := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	nodeClaim.Spec.DeadlineAt = &deadline

	recorder, cl := recorderWith(t, nodeClaim)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(nodeClaim)

	moved, err := recorder.Move(ctx, key, infrav2alpha1.NodeClaimReady, "")
	require.NoError(t, err)
	require.True(t, moved)

	got := getClaim(t, cl, nodeClaim)
	assert.Equal(t, infrav2alpha1.NodeClaimReady, got.Status.Phase)
	assert.Nil(t, got.Spec.DeadlineAt, "the boot window is closed: the deadline of the attempt is finished")
	assert.False(t, IsOverdue(got, time.Now()), "a ready claim with an old deadline is not overdue: IsOverdue reads the field, and the field is gone")

	before := getClaim(t, cl, nodeClaim)
	moved, err = recorder.Move(ctx, key, infrav2alpha1.NodeClaimReady, "")
	require.NoError(t, err)
	assert.False(t, moved, "the ready move is idempotent once its window is closed")
	assert.Equal(t, before.ResourceVersion, getClaim(t, cl, nodeClaim).ResourceVersion,
		"an idempotent move touches neither the spec nor the status")
}

// TestScaleDownStreakSpentIsTheShutdownCounterOfItsOwn is M4: the bound on shutdown attempts lives on
// status.shutdownAttempts, the boot attempts are untouched by it, and a spent streak is a bounded
// pause - past claim.ShutdownAttemptResetGrace the shutdown fires again, so a pool whose script has
// been fixed is picked up without a hand edit.
func TestScaleDownStreakSpentIsTheShutdownCounterOfItsOwn(t *testing.T) {
	at := func(d time.Duration) *metav1.Time {
		now := metav1.NewTime(time.Now().Add(d))

		return &now
	}

	tests := []struct {
		name      string
		claim     *infrav2alpha1.NodeClaim
		wantSpent bool
		wantReset bool
	}{
		{
			name:      "no attempts spent",
			claim:     &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{Phase: infrav2alpha1.NodeClaimReady}},
			wantSpent: false,
		},
		{
			name: "one attempt short of the bound",
			claim: &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{
				Phase: infrav2alpha1.NodeClaimReady, ShutdownAttempts: MaxConsecutiveAttempts - 1,
				LastShutdownAttemptAt: at(0),
			}},
			wantSpent: false,
		},
		{
			name: "the bound spent, inside the grace: the refusal is in force",
			claim: &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{
				Phase: infrav2alpha1.NodeClaimReady, ShutdownAttempts: MaxConsecutiveAttempts,
				LastShutdownAttemptAt: at(0),
			}},
			wantSpent: true,
		},
		{
			name: "the bound spent, its grace past: a fixed script is picked up",
			claim: &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{
				Phase: infrav2alpha1.NodeClaimReady, ShutdownAttempts: MaxConsecutiveAttempts,
				LastShutdownAttemptAt: at(-(ShutdownAttemptResetGrace + time.Minute)),
			}},
			wantSpent: false,
			wantReset: true,
		},
		{
			name: "a spent streak with no clock is a streak that started now",
			claim: &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{
				Phase: infrav2alpha1.NodeClaimReady, ShutdownAttempts: MaxConsecutiveAttempts,
			}},
			wantSpent: true,
		},
		{
			name: "the boot attempts of the episode are not the shutdown bound",
			claim: &infrav2alpha1.NodeClaim{Status: infrav2alpha1.NodeClaimStatus{
				Phase: infrav2alpha1.NodeClaimReady, Attempt: 9, ShutdownAttempts: 0,
			}},
			wantSpent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spent, left := ScaleDownStreakSpent(tt.claim)
			assert.Equal(t, tt.wantSpent, spent)

			if tt.wantSpent {
				assert.Greater(t, left, time.Duration(0), "the refusal reports the grace it is pausing for")
				assert.LessOrEqual(t, left, ShutdownAttemptResetGrace)
			} else {
				assert.Zero(t, left, "a streak that is not spent has no grace to wait out")
			}

			assert.Equal(t, tt.wantReset, ScaleDownStreakResetDue(tt.claim),
				"the pass that fires a shutdown past the grace is the one that says the pause ended, and it is that pass's event")
		})
	}

	nilSpent, nilLeft := ScaleDownStreakSpent(nil)
	assert.False(t, nilSpent, "no claim spends no attempts")
	assert.Zero(t, nilLeft)
}

// TestGetIsNilForACollectedClaim: every caller of Get is a path that has to survive the TTL.
func TestGetIsNilForACollectedClaim(t *testing.T) {
	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimCompleted, 1)
	recorder, cl := recorderWith(t, nodeClaim)
	ctx := context.Background()

	tests := []struct {
		name    string
		key     types.NamespacedName
		wantNil bool
	}{
		{name: "a claim that is there", key: client.ObjectKeyFromObject(nodeClaim), wantNil: false},
		{name: "a claim the TTL collected", key: types.NamespacedName{Name: "remote-worker-2-00000", Namespace: fixNamespace}, wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Get(ctx, cl, tt.key)
			require.NoError(t, err)

			if tt.wantNil {
				assert.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			assert.Equal(t, nodeClaim.Name, got.Name)
			assert.Equal(t, infrav2alpha1.NodeClaimCompleted, got.Status.Phase)
		})
	}

	assert.NotNil(t, recorder.Client, "a Recorder built by NewRecorder carries its client")
}

// fixClaimName is the name of a claim of the fixture's slot: <slot>-<suffix>, the shape the
// provider creates, and never renamed.
const fixClaimName = fixSlot + "-7f4b9c"

// recordedClaim is a claim of the fixture's slot, in one phase, carrying one attempt.
func recordedClaim(claimPhase infrav2alpha1.NodeClaimPhase, attempt int32) *infrav2alpha1.NodeClaim {
	return &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: fixClaimName, Namespace: fixNamespace},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef:     corev1.ObjectReference{APIVersion: infrav2alpha1.GroupVersion.String(), Kind: "Node", Name: fixSlot},
			RequestedBy: RequestedByAutoscaler,
			Request:     increaseSize(1),
		},
		Status: infrav2alpha1.NodeClaimStatus{Phase: claimPhase, Attempt: attempt},
	}
}

// recorderClient is the fake cluster of the recorder's tests: the fixture's objects, with the status
// subresource of the claim and the slot enabled, because the Recorder writes status there and nothing
// else. A test that intercepts one write wraps this client, and reads the result off it.
func recorderClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&infrav2alpha1.NodeClaim{}, &infrav2alpha1.Node{}).
		Build()
}

// recorderWith is a Recorder over that cluster.
func recorderWith(t *testing.T, objs ...client.Object) (*Recorder, client.WithWatch) {
	t.Helper()

	cl := recorderClient(t, objs...)

	return NewRecorder(cl), cl
}

// getClaim reads a claim back off the cluster, so every assertion is about the write, not about the
// caller's copy.
func getClaim(t *testing.T, cl client.Client, nodeClaim *infrav2alpha1.NodeClaim) *infrav2alpha1.NodeClaim {
	t.Helper()

	got := &infrav2alpha1.NodeClaim{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(nodeClaim), got))

	return got
}

// conditionStatus reads one condition of a claim; a condition that is absent reads as Unknown, so a
// missing stamp is visible as a failure and not as a zero value.
func conditionStatus(t *testing.T, nodeClaim *infrav2alpha1.NodeClaim, conditionType string) metav1.ConditionStatus {
	t.Helper()

	for _, cond := range nodeClaim.Status.Conditions {
		if cond.Type == conditionType {
			return cond.Status
		}
	}

	return metav1.ConditionUnknown
}

// TestRecorderReadyMoveClearsTheDeadlineAfterTheStatusWrite is the order of the two writes of the
// `booting -> ready` move, and the direction a crash between them leaves the claim in.
//
// The deadline is the only writer that fails an attempt whose startup Job is gone, and the claim
// controller reads it inside the boot window only. Clearing it *before* the status write therefore
// leaves the shape nothing heals: a manager that dies between the two hands back a `booting` claim
// with no deadline, and once the Job's TTL removes the Job no path is left to end the attempt - the
// claim lives, its slot is counted as capacity, and nothing is watching it. Writing the status first
// leaves the other pair: a `ready` claim that still carries its deadline, a field every guard in the
// model scopes to the boot window and therefore reads as inert, and an idempotent re-apply of the
// move that clears it.
func TestRecorderReadyMoveClearsTheDeadlineAfterTheStatusWrite(t *testing.T) {
	ctx := context.Background()

	bootingWithDeadline := func() *infrav2alpha1.NodeClaim {
		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimBooting, 1)

		deadline := metav1.NewTime(time.Now().Add(-3 * time.Hour))
		nodeClaim.Spec.DeadlineAt = &deadline

		return nodeClaim
	}

	conflict := func(obj client.Object) error {
		return apierrors.NewConflict(
			schema.GroupResource{Group: infrav2alpha1.GroupVersion.Group, Resource: "nodeclaims"},
			obj.GetName(),
			context.DeadlineExceeded)
	}

	t.Run("the status lands and the deadline clear is lost: the pair is healable", func(t *testing.T) {
		nodeClaim := bootingWithDeadline()
		key := client.ObjectKeyFromObject(nodeClaim)

		// The metadata/spec write fails: the recorder is past its status write, and a manager that
		// dies here leaves the shape below.
		base := recorderClient(t, nodeClaim)
		lost := interceptor.NewClient(base, interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				return conflict(obj)
			},
		})

		moved, err := NewRecorder(lost).Move(ctx, key, infrav2alpha1.NodeClaimReady, "")
		require.Error(t, err, "a lost write is reported, not swallowed")
		assert.False(t, moved)

		got := getClaim(t, base, nodeClaim)
		assert.Equal(t, infrav2alpha1.NodeClaimReady, got.Status.Phase,
			"the phase the transition produced is recorded first: the readiness gate's answer is not lost with the spec write")
		assert.NotNil(t, got.Spec.DeadlineAt, "the deadline is the field that is still owed")
		assert.True(t, IsOverdue(got, time.Now()), "and it is overdue, which is what makes the pair legible: the boot is over")
		assert.False(t, phase.InBootWindow(got.Status.Phase) && IsOverdue(got, time.Now()),
			"no guard of the model reads a deadline outside the boot window: the claim is not destroyed, and it is watched")

		// The heal: the pass that re-lands the (idempotent) move is the one that closes the window.
		_, err = NewRecorder(lost).Move(ctx, key, infrav2alpha1.NodeClaimReady, "")
		require.Error(t, err, "the writer is still broken: the recorder never silently reports a clean move")

		repaired := NewRecorder(base)
		moved, err = repaired.Move(ctx, key, infrav2alpha1.NodeClaimReady, "")
		require.NoError(t, err)
		assert.True(t, moved, "the re-apply writes the half the lost write owed: it is the pass that closes the window")
		assert.Nil(t, getClaim(t, base, nodeClaim).Spec.DeadlineAt,
			"and the re-apply is the pass that clears the deadline the lost write owed")
	})

	t.Run("the status write is lost: the deadline stays on a booting claim", func(t *testing.T) {
		nodeClaim := bootingWithDeadline()
		key := client.ObjectKeyFromObject(nodeClaim)

		base := recorderClient(t, nodeClaim)
		lost := interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
				return conflict(obj)
			},
		})

		moved, err := NewRecorder(lost).Move(ctx, key, infrav2alpha1.NodeClaimReady, "")
		require.Error(t, err)
		assert.False(t, moved)

		got := getClaim(t, base, nodeClaim)
		assert.Equal(t, infrav2alpha1.NodeClaimBooting, got.Status.Phase, "the move did not land")
		assert.NotNil(t, got.Spec.DeadlineAt,
			"and the claim is not left booting without the deadline that ends it: the clear runs after the status write, never before it")
		assert.True(t, phase.InBootWindow(got.Status.Phase) && IsOverdue(got, time.Now()),
			"the deadline guard is armed on exactly this shape, and it is what fails the attempt")
	})
}

// TestBootCommittedAtAndScaleDownRequestedAtAreTheBoundsARecoveryCorrelatesTo is the lower bound of
// the two lifecycle recoveries (internal/controller/infra/lifecycle.go): a finished Job is evidence
// about the operation of this claim, and the operation cannot predate the claim's own commit.
func TestBootCommittedAtAndScaleDownRequestedAtAreTheBoundsARecoveryCorrelatesTo(t *testing.T) {
	made := time.Now().Add(-10 * time.Minute)
	committed := made.Add(90 * time.Second)
	marked := made.Add(20 * time.Minute)

	withCondition := func(status metav1.ConditionStatus, at time.Time) *infrav2alpha1.NodeClaim {
		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimBooting, 1)
		nodeClaim.CreationTimestamp = metav1.NewTime(made)
		nodeClaim.Status.Conditions = []metav1.Condition{{
			Type:               infrav2alpha1.ClaimConditionMachineCommitted,
			Status:             status,
			Reason:             string(infrav2alpha1.NodeClaimBooting),
			LastTransitionTime: metav1.NewTime(at),
		}}

		return nodeClaim
	}

	assert.Equal(t, committed, BootCommittedAt(withCondition(metav1.ConditionTrue, committed)),
		"a claim that records its commit answers with the instant it committed")
	unstamped := recordedClaim(infrav2alpha1.NodeClaimBooting, 1)
	unstamped.CreationTimestamp = metav1.NewTime(made)

	assert.Equal(t, made, BootCommittedAt(unstamped),
		"a claim with no condition falls back to its creation, the oldest instant its boot can have started")
	assert.Equal(t, made, BootCommittedAt(withCondition(metav1.ConditionFalse, committed)),
		"a MachineCommitted that is False records no commit: the fallback is the claim's creation")
	assert.True(t, BootCommittedAt(nil).IsZero(), "no claim, no bound: the caller does not refuse on an absence")

	markedClaim := recordedClaim(infrav2alpha1.NodeClaimReady, 1)
	deletion := metav1.NewTime(marked)
	markedClaim.CreationTimestamp = metav1.NewTime(made)
	markedClaim.DeletionTimestamp = &deletion

	assert.Equal(t, marked, ScaleDownRequestedAt(markedClaim),
		"the scale-down's clock is the deletionTimestamp the request landed, not the claim's creation")
	assert.True(t, ScaleDownRequestedAt(recordedClaim(infrav2alpha1.NodeClaimReady, 1)).IsZero(),
		"a claim no scale-down named owns no shutdown to recover")
	assert.True(t, ScaleDownRequestedAt(nil).IsZero())
}
