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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
)

// capturingRecorder is the test double of k8s.io/client-go/tools/events.EventRecorder: it records
// the calls, and nothing else. The claim's event set is asserted on this, because the fact under test
// is "which Event did the move publish, on which object, with what note" - the API server's
// aggregation, the Event TTL, and `kubectl describe`'s rendering are Kubernetes' behaviour, not this
// package's.
type capturingRecorder struct {
	calls []capturedEvent
}

type capturedEvent struct {
	regarding runtime.Object
	related   runtime.Object
	eventType string
	reason    string
	action    string
	note      string
	args      []interface{}
}

// message renders the note the way the recorder will: the note is eventNoteFormat, and the rendered
// message travels as its argument (so a message with a literal '%' in it is data, and this test can
// say so).
func (e capturedEvent) message() string {
	return fmt.Sprintf(e.note, e.args...)
}

func (c *capturingRecorder) Eventf(regarding runtime.Object, related runtime.Object,
	eventType, reason, action, note string, args ...interface{}) {
	c.calls = append(c.calls, capturedEvent{
		regarding: regarding,
		related:   related,
		eventType: eventType,
		reason:    reason,
		action:    action,
		note:      note,
		args:      args,
	})
}

func (c *capturingRecorder) Event(regarding runtime.Object, related runtime.Object,
	eventType, reason, action, note string) {
	c.calls = append(c.calls, capturedEvent{
		regarding: regarding,
		related:   related,
		eventType: eventType,
		reason:    reason,
		action:    action,
		note:      note,
	})
}

func (c *capturingRecorder) AnnotatedEventf(regarding runtime.Object, annotations map[string]string,
	eventType, reason, action, note string, args ...interface{}) {
	c.Eventf(regarding, nil, eventType, reason, action, note, args...)
}

var _ events.EventRecorder = &capturingRecorder{}

// reasonsOf is the reason of every event a run published, in order: the set is the assertion, and a
// move that published two events, or none, fails on it.
func reasonsOf(rec *capturingRecorder) []string {
	reasons := make([]string, 0, len(rec.calls))

	for _, call := range rec.calls {
		reasons = append(reasons, call.reason)
	}

	return reasons
}

// oneEvent is the whole point of the set: one move, one Event. It returns the event, and fails the
// test if the count is anything else.
func oneEvent(t *testing.T, rec *capturingRecorder) capturedEvent {
	t.Helper()

	require.Len(t, rec.calls, 1, "one transition publishes exactly one Event, and the reasons published were %v", reasonsOf(rec))

	return rec.calls[0]
}

// eventsScheme is the fixture's cluster with the kinds the claim events read: the CRDs, and the
// batch Job the lifecycle Jobs are (the startup and shutdown Job names are quoted in the messages,
// so the events test needs a cluster that can answer that list).
func eventsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, infrav2alpha1.AddToScheme(s))
	require.NoError(t, batchv1.AddToScheme(s))

	return s
}

// eventsClient is the fake cluster of the event tests: the same objects and status subresources the
// recorder's tests build, with batch added.
func eventsClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()

	return fakeclient.NewClientBuilder().
		WithScheme(eventsScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&infrav2alpha1.NodeClaim{}, &infrav2alpha1.Node{}).
		Build()
}

// recorderWithEvents is a Recorder that publishes, over that cluster.
func recorderWithEvents(t *testing.T, rec *capturingRecorder, objs ...client.Object) *Recorder {
	t.Helper()

	return NewRecorderWithEvents(eventsClient(t, objs...), rec)
}

// labelled stamps the slot label every claim of the model carries (claim.LabelSlot, the index
// ListOfSlot reads): the fixture's claim is findable by the slot the way a real one is.
func labelled(nodeClaim *infrav2alpha1.NodeClaim) {
	if nodeClaim.Labels == nil {
		nodeClaim.Labels = map[string]string{}
	}

	nodeClaim.Labels[LabelSlot] = SlotNameOf(nodeClaim)
	nodeClaim.Labels[LabelGroup] = nodeClaim.Spec.Request.Group
}

// lifecycleJobOfClaim is the Job a claim's attempt created: the label the transition stamps
// (fsm.LabelNodeClaim) is the only link between a Job and the claim it is the attempt of, and it is
// what the recorder reads to name it in an event.
func lifecycleJobOfClaim(claimName, slotName, jobPhase, suffix string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s", slotName, jobPhase, suffix),
			Namespace: fixNamespace,
			Labels: map[string]string{
				fsm.LabelNodeCR:    slotName,
				fsm.LabelPhase:     jobPhase,
				fsm.LabelNodeClaim: claimName,
			},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
	}
}

// TestSeedPublishesClaimCreatedWithTheAttribution is the create of an attempt, on the claim: the
// request exists, and the event says who asked, through which RPC, with what arguments, which attempt
// of the slot it is, and what budget it got. The seed of the attempt is the first write the claim
// controller performs on a fresh claim, and it happens once, so this is one ClaimCreated per claim
// and it lands for every writer of a claim - the provider, a manual claim, a replay.
func TestSeedPublishesClaimCreatedWithTheAttribution(t *testing.T) {
	ctx := context.Background()
	rec := &capturingRecorder{}

	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPhase(""), 0)

	requested := metav1.NewTime(time.Now().Add(-90 * time.Second))
	nodeClaim.Spec.RequestedAt = requested

	deadline := metav1.NewTime(requested.Add(AttemptBudget()))
	nodeClaim.Spec.DeadlineAt = &deadline

	slotCR := slot()
	r := recorderWithEvents(t, rec, nodeClaim, slotCR)

	wrote, err := r.Seed(ctx, client.ObjectKeyFromObject(nodeClaim), slotCR)
	require.NoError(t, err)
	require.True(t, wrote, "the first seed of a fresh claim writes")

	call := oneEvent(t, rec)
	assert.Equal(t, EventClaimCreated, call.reason)
	assert.Equal(t, corev1.EventTypeNormal, call.eventType)
	assert.Equal(t, ActionClaim, call.action,
		"the action is part of the new events API's aggregation key, so repeats of one fact coalesce")

	claimed, ok := call.regarding.(*infrav2alpha1.NodeClaim)
	require.True(t, ok, "the event is published on the claim, so `kubectl describe nodeclaim` shows it")
	assert.Equal(t, fixClaimName, claimed.Name)

	message := call.message()
	assert.Contains(t, message, fixClaimName, "the event names the claim it is about")
	assert.Contains(t, message, fixSlot, "the event names the slot the request asks for")
	assert.Contains(t, message, infrav2alpha1.ClaimCallIncreaseSize, "the event names the RPC that asked")
	assert.Contains(t, message, "delta +1", "the event carries the request's delta, signed")
	assert.Contains(t, message, fixGroup, "the event carries the group")
	assert.Contains(t, message, RequestedByAutoscaler, "the event carries who asked")
	assert.Contains(t, message, "attempt 1", "the event carries the seeded attempt number")
	assert.Contains(t, message, "deadline", "the event carries the budget")
	assert.Contains(t, message, deadline.Time.UTC().Format(time.RFC3339))
	assert.NotContains(t, message, "%!", "the note is data, not a format string")

	// The seed writes once, and so does the event: a claim that already carries an attempt is left
	// alone, and the pass that comes back publishes nothing.
	wrote, err = r.Seed(ctx, client.ObjectKeyFromObject(nodeClaim), slotCR)
	require.NoError(t, err)
	require.False(t, wrote)
	assert.Len(t, rec.calls, 1, "a re-seed is not a creation, and publishes no event")
}

// TestClaimCreatedNamesTheCallAndWhatItNamed: the attribution is the point of the object, so the
// event spells the call out, and the arguments that call carries - the delta of a size change, the
// node names of a DeleteNodes.
func TestClaimCreatedNamesTheCallAndWhatItNamed(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		request infrav2alpha1.ClaimRequest
		want    []string
		deny    []string
	}{
		{
			name:    "IncreaseSize names its delta",
			request: increaseSize(3),
			want:    []string{infrav2alpha1.ClaimCallIncreaseSize, "delta +3", fixGroup},
			deny:    []string{"nodes "},
		},
		{
			name:    "DecreaseTargetSize names its delta",
			request: decreaseTargetSize(-2),
			want:    []string{infrav2alpha1.ClaimCallDecreaseTargetSize, "delta -2", fixGroup},
			deny:    []string{"nodes "},
		},
		{
			name:    "DeleteNodes names the machines",
			request: deleteNodes(fixK8sNode, "remote-worker-3"),
			want:    []string{infrav2alpha1.ClaimCallDeleteNodes, "nodes " + fixK8sNode + ",remote-worker-3", fixGroup},
			deny:    []string{"delta "},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &capturingRecorder{}

			nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPhase(""), 0)
			nodeClaim.Spec.Request = tt.request
			nodeClaim.Spec.RequestedBy = "filippo"

			slotCR := slot()
			r := recorderWithEvents(t, rec, nodeClaim, slotCR)

			_, err := r.Seed(ctx, client.ObjectKeyFromObject(nodeClaim), slotCR)
			require.NoError(t, err)

			call := oneEvent(t, rec)
			assert.Equal(t, EventClaimCreated, call.reason)

			message := call.message()
			for _, want := range tt.want {
				assert.Contains(t, message, want)
			}

			for _, deny := range tt.deny {
				assert.NotContains(t, message, deny, "an event quotes the arguments the call carries, and no others")
			}

			assert.Contains(t, message, "by filippo", "the free-text identity of the caller is part of the record")
		})
	}
}

// TestPhaseMovesPublishOneEventEach is the event set: every edge of the claim's phase table
// publishes the one fact it is the writer of, on the claim, with the attempt-side detail the slot's
// events do not carry (the attempt number, the Job, the deadline, the duration).
func TestPhaseMovesPublishOneEventEach(t *testing.T) {
	ctx := context.Background()

	t.Run("pending -> booting commits the boot and names the startup Job", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPending, 2)

		deadline := metav1.NewTime(time.Now().Add(AttemptBudget()))
		nodeClaim.Spec.DeadlineAt = &deadline

		job := lifecycleJobOfClaim(fixClaimName, fixSlot, fsm.PhaseStartup, "1772000000")

		r := recorderWithEvents(t, rec, nodeClaim, slot(), job)

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), infrav2alpha1.NodeClaimBooting, "")
		require.NoError(t, err)
		require.True(t, moved)

		call := oneEvent(t, rec)
		assert.Equal(t, EventClaimBooting, call.reason)
		assert.Equal(t, corev1.EventTypeNormal, call.eventType, "a boot that committed is the request working, not a fault")
		assert.Equal(t, ActionClaim, call.action)

		message := call.message()
		assert.Contains(t, message, job.Name, "the event names the startup Job of the attempt")
		assert.Contains(t, message, "committed")
		assert.Contains(t, message, "no longer cancelable", "the fact the edge carries: a boot that started is reversed, never cancelled")
		assert.Contains(t, message, "attempt 2")
		assert.Contains(t, message, fixClaimName)
	})

	t.Run("a committed boot whose Job is not readable quotes the selector, not a name", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPending, 1)
		r := recorderWithEvents(t, rec, nodeClaim, slot())

		// The normal order of the transition: enterStartingUp records the commit, and StartNode
		// creates the startup Job a few lines later (internal/fsm/node_state_machine.go). The event
		// cannot quote a name that does not exist yet, so it points at the label that resolves it.
		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), infrav2alpha1.NodeClaimBooting, "")
		require.NoError(t, err)
		require.True(t, moved)

		call := oneEvent(t, rec)
		assert.Equal(t, EventClaimBooting, call.reason)
		assert.Contains(t, call.message(), fsm.LabelNodeClaim+"="+fixClaimName,
			"the event says where to read the Job of this attempt")
	})

	t.Run("booting -> ready reports the gate's answer for the attempt", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimBooting, 1)

		deadline := metav1.NewTime(time.Now().Add(-30 * time.Second))
		nodeClaim.Spec.DeadlineAt = &deadline

		requested := metav1.NewTime(time.Now().Add(-4 * time.Minute))
		nodeClaim.Spec.RequestedAt = requested

		r := recorderWithEvents(t, rec, nodeClaim, slot())

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), infrav2alpha1.NodeClaimReady, "")
		require.NoError(t, err)
		require.True(t, moved)

		call := oneEvent(t, rec)
		assert.Equal(t, EventClaimReady, call.reason)
		assert.Equal(t, corev1.EventTypeNormal, call.eventType)

		message := call.message()
		assert.Contains(t, message, fixK8sNode, "the event names the kubernetes node the machine joined as")
		assert.Contains(t, message, "readiness gate", "the event names the gate that passed")
		assert.Contains(t, message, "after the request", "the event carries the time from spec.requestedAt")
		assert.Contains(t, message, "4m", "the duration of the attempt, to the second: 4m0s / 4m1s")
		assert.Contains(t, message, "attempt 1")
	})

	t.Run("a scale-down that lands is a ClaimCancelled naming the call that asked", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimBooting, 3)
		nodeClaim.Spec.Request = deleteNodes(fixK8sNode)

		requested := metav1.NewTime(time.Now().Add(-6 * time.Minute))
		nodeClaim.Spec.RequestedAt = requested

		job := lifecycleJobOfClaim(fixClaimName, fixSlot, fsm.PhaseShutdown, "1772000600")

		r := recorderWithEvents(t, rec, nodeClaim, slot(), job)

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim),
			infrav2alpha1.NodeClaimCompleted, infrav2alpha1.ClaimReasonNodeDeleted)
		require.NoError(t, err)
		require.True(t, moved)

		call := oneEvent(t, rec)
		assert.Equal(t, EventClaimCancelled, call.reason)
		assert.Equal(t, corev1.EventTypeNormal, call.eventType,
			"the cancel is the request landing, so it is not a Warning")

		message := call.message()
		assert.Contains(t, message, infrav2alpha1.ClaimCallDeleteNodes, "the event says which call asked")
		assert.Contains(t, message, "nodes "+fixK8sNode, "and which machines it named")
		assert.Contains(t, message, infrav2alpha1.ClaimReasonNodeDeleted)
		assert.Contains(t, message, "attempt 3")
		assert.Contains(t, message, job.Name, "the Job that ran the power-off is the record of the ending")
		assert.Contains(t, message, "observed off")
	})

	t.Run("a pending claim DecreaseTargetSize took away says no machine was bought", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPending, 1)
		nodeClaim.Spec.Request = decreaseTargetSize(-1)

		r := recorderWithEvents(t, rec, nodeClaim, slot())

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim),
			infrav2alpha1.NodeClaimCompleted, infrav2alpha1.ClaimReasonCancelledByDecreaseTargetSize)
		require.NoError(t, err)
		require.True(t, moved)

		call := oneEvent(t, rec)
		assert.Equal(t, EventClaimCancelled, call.reason)

		message := call.message()
		assert.Contains(t, message, infrav2alpha1.ClaimCallDecreaseTargetSize)
		assert.Contains(t, message, "delta -1")
		assert.Contains(t, message, "no machine was bought")
	})

	t.Run("a delete by hand is a ClaimCancelled too", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimReady, 1)

		r := recorderWithEvents(t, rec, nodeClaim, slot())

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim),
			infrav2alpha1.NodeClaimCompleted, infrav2alpha1.ClaimReasonDeletedByHand)
		require.NoError(t, err)
		require.True(t, moved)

		call := oneEvent(t, rec)
		assert.Equal(t, EventClaimCancelled, call.reason)
		assert.Contains(t, call.message(), infrav2alpha1.ClaimReasonDeletedByHand)
	})

	t.Run("an end with no cancel behind it is ClaimCompleted", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimReady, 1)

		requested := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		nodeClaim.Spec.RequestedAt = requested

		r := recorderWithEvents(t, rec, nodeClaim, slot())

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), infrav2alpha1.NodeClaimCompleted, "")
		require.NoError(t, err)
		require.True(t, moved)

		call := oneEvent(t, rec)
		assert.Equal(t, EventClaimCompleted, call.reason)
		assert.Equal(t, corev1.EventTypeNormal, call.eventType)

		message := call.message()
		assert.Contains(t, message, "observed off", "the fact a move to completed reports")
		assert.Contains(t, message, "10m", "and the total duration of the episode, to the second")
		assert.Contains(t, message, infrav2alpha1.ClaimCallIncreaseSize, "the request the episode was")
	})

	t.Run("the extra live claim of a slot is superseded, and the event names the claim that won", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimBooting, 1)

		// The owner of the slot is the newest live claim of the slot - creation timestamp, name
		// breaking the tie (claim.LiveOfSlot) - so the fixture says which of the two is newer.
		older := metav1.NewTime(time.Now().Add(-5 * time.Minute))
		nodeClaim.CreationTimestamp = older
		labelled(nodeClaim)

		newer := recordedClaim(infrav2alpha1.NodeClaimPending, 2)
		newer.Name = fixSlot + "-newest"
		newer.CreationTimestamp = metav1.NewTime(time.Now())
		labelled(newer)

		r := recorderWithEvents(t, rec, nodeClaim, slot(), newer)

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim),
			infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonSuperseded)
		require.NoError(t, err)
		require.True(t, moved)

		call := oneEvent(t, rec)
		assert.Equal(t, EventClaimSuperseded, call.reason)
		assert.Equal(t, corev1.EventTypeNormal, call.eventType,
			"the model is not broken: one live claim per slot is load-bearing, and the extra ends")

		message := call.message()
		assert.Contains(t, message, newer.Name, "the newer claim's name is the fact an operator acts on")
		assert.Contains(t, message, "superseded")
	})
}

// TestNothingIsPublishedForAMoveThatDidNotLand is the other half of the count rule: an Event is a
// record of a write, so a move the phase table refuses, a move the API server refused, and a re-apply
// of a move that changed nothing publish nothing. A timeline that reports a transition the cluster
// does not have is worse than no timeline: it is the first thing an operator trusts.
func TestNothingIsPublishedForAMoveThatDidNotLand(t *testing.T) {
	ctx := context.Background()

	t.Run("an illegal move is refused, writes nothing, and says nothing", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimBooting, 1)
		r := recorderWithEvents(t, rec, nodeClaim, slot())

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), infrav2alpha1.NodeClaimPending, "")
		require.ErrorIs(t, err, ErrIllegalMove, "a boot that has started is never cancelled")
		assert.False(t, moved)
		assert.Empty(t, rec.calls, "a refused move is not a fact, so it is not an event")
	})

	t.Run("a status write the API server refused publishes nothing", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPending, 1)

		cl := eventsClient(t, nodeClaim, slot())

		blocked := interceptor.NewClient(cl, interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(
					schema.GroupResource{Group: infrav2alpha1.GroupVersion.Group, Resource: "nodeclaims"},
					obj.GetName(),
					context.DeadlineExceeded)
			},
		})

		r := NewRecorderWithEvents(blocked, rec)

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), infrav2alpha1.NodeClaimBooting, "")
		require.Error(t, err, "the write failed, and the recorder says so")
		assert.False(t, moved)
		assert.Empty(t, rec.calls, "the phase did not move, so the claim's timeline does not claim it did")
	})

	t.Run("an idempotent re-write of a move publishes nothing", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimReady, 1)
		nodeClaim.Status.Reason = infrav2alpha1.ClaimReasonMachineGone

		r := recorderWithEvents(t, rec, nodeClaim, slot())

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim),
			infrav2alpha1.NodeClaimReady, infrav2alpha1.ClaimReasonMachineGone)
		require.NoError(t, err)
		assert.False(t, moved, "the phase was already what the caller asked for")
		assert.Empty(t, rec.calls, "the recorder is called on every pass of a ready claim's slot; it does not narrate them")
	})

	t.Run("an idempotent re-write that clears a deadline publishes nothing", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimReady, 1)

		deadline := metav1.NewTime(time.Now().Add(-time.Minute))
		nodeClaim.Spec.DeadlineAt = &deadline

		r := recorderWithEvents(t, rec, nodeClaim, slot())

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), infrav2alpha1.NodeClaimReady, "")
		require.NoError(t, err)
		require.True(t, moved, "the ready move owed the deadline clear, and this pass lands it")

		assert.Empty(t, rec.calls,
			"the write changed spec.deadlineAt, not the phase: the ready event was published by the move that made the claim ready")
	})

	t.Run("a terminal claim is not re-narrated", func(t *testing.T) {
		rec := &capturingRecorder{}

		nodeClaim := recordedClaim(infrav2alpha1.NodeClaimFailed, 2)
		nodeClaim.Status.Reason = infrav2alpha1.ClaimReasonDeadlineExceeded

		r := recorderWithEvents(t, rec, nodeClaim, slot())

		moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim),
			infrav2alpha1.NodeClaimFailed, infrav2alpha1.ClaimReasonDeadlineExceeded)
		require.NoError(t, err)
		assert.False(t, moved)
		assert.Empty(t, rec.calls)
	})
}

// TestRecorderWithoutAnEventRecorderIsNilSafe is the pattern the repository already follows
// (internal/controller/infra/lifecycle.go's eventf): a Recorder built by NewRecorder has no event
// recorder, and every phase write still lands. The unit tests of the phase model, the fsm's test
// writer, and any caller with no manager to build a recorder from depend on that.
func TestRecorderWithoutAnEventRecorderIsNilSafe(t *testing.T) {
	ctx := context.Background()

	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPending, 1)

	r, cl := recorderWith(t, nodeClaim, slot())
	require.Nil(t, r.Events, "NewRecorder wires no event recorder")

	moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), infrav2alpha1.NodeClaimBooting, "")
	require.NoError(t, err)
	require.True(t, moved)

	got := getClaim(t, cl, nodeClaim)
	assert.Equal(t, infrav2alpha1.NodeClaimBooting, got.Status.Phase)

	_, err = r.Seed(ctx, client.ObjectKeyFromObject(nodeClaim), slot())
	require.NoError(t, err)
}

// TestEveryEventOfTheSetCarriesTheAggregatingAction pins the dedup mechanism: the new events API
// keys its aggregation on the object, the type, the reason and the action, so repeats of one fact on
// one claim patch one Event (count, lastTimestamp) instead of adding an object per pass. An event
// published with no action is an event that storms.
func TestEveryEventOfTheSetCarriesTheAggregatingAction(t *testing.T) {
	ctx := context.Background()

	rec := &capturingRecorder{}

	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPending, 0)
	slotCR := slot()

	r := recorderWithEvents(t, rec, nodeClaim, slotCR)

	_, err := r.Seed(ctx, client.ObjectKeyFromObject(nodeClaim), slotCR)
	require.NoError(t, err)

	for _, to := range []infrav2alpha1.NodeClaimPhase{
		infrav2alpha1.NodeClaimBooting,
		infrav2alpha1.NodeClaimReady,
		infrav2alpha1.NodeClaimCompleted,
	} {
		_, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), to, "")
		require.NoError(t, err)
	}

	require.Len(t, rec.calls, 4, "the create and three moves")

	for _, call := range rec.calls {
		assert.Equal(t, ActionClaim, call.action)
		assert.NotEmpty(t, call.reason, "every fact of the set is a named reason")
		assert.Equal(t, eventNoteFormat, call.note, "the rendered message is an argument, never a format string")
		assert.IsType(t, &infrav2alpha1.NodeClaim{}, call.regarding)
		assert.Nil(t, call.related, "the claim is the subject; its Job is named in the note")
	}
}

// decreaseTargetSize is the request NodeGroupDecreaseTargetSize records: a signed delta, no nodes.
func decreaseTargetSize(delta int32) infrav2alpha1.ClaimRequest {
	return infrav2alpha1.ClaimRequest{
		Call:  infrav2alpha1.ClaimCallDecreaseTargetSize,
		Delta: delta,
		Group: fixGroup,
	}
}

// TestClaimFailedQuotesTheNumberBehindEveryReason is the reason table of the model, as an event: a
// move to failed publishes one Warning that names status.reason and the concrete value behind it -
// the deadline that passed, the Job that died, the bound the streak hit, the observation that
// outlasted its grace. An operator reading `kubectl describe nodeclaim` after one bad boot gets the
// number to act on, not a label to search the docs for.
//
// The reasons are the constants the phase writers can record (the claim controller's bounds, the
// transition's Jobs, the gate, the supersede, the slot's deletion), spelled by name so a reason that
// gains a writer, or a detail that disappears, fails here.
func TestClaimFailedQuotesTheNumberBehindEveryReason(t *testing.T) {
	ctx := context.Background()

	observed := metav1.NewTime(time.Now().Add(-2 * time.Hour))

	tests := []struct {
		name       string
		from       infrav2alpha1.NodeClaimPhase
		attempt    int32
		reason     string
		wantReason string
		wantType   string
		job        string
		stamp      func(*infrav2alpha1.NodeClaim)
		want       []string
		deny       []string
	}{
		{
			name:    "DeadlineExceeded",
			from:    infrav2alpha1.NodeClaimBooting,
			attempt: 2,
			reason:  infrav2alpha1.ClaimReasonDeadlineExceeded,
			stamp: func(nodeClaim *infrav2alpha1.NodeClaim) {
				deadline := metav1.NewTime(time.Now().Add(-3 * time.Minute))
				nodeClaim.Spec.DeadlineAt = &deadline
				max := metav1.Duration{Duration: 15 * time.Minute}
				nodeClaim.Spec.MaxNodeProvisionTime = &max
			},
			want: []string{"the boot window closed at", "15m0s", "force-shut-down", "the next claim of the slot is attempt 3"},
			deny: []string{"unset"},
		},
		{
			name:    "StartupJobFailed",
			from:    infrav2alpha1.NodeClaimBooting,
			attempt: 1,
			reason:  infrav2alpha1.ClaimReasonStartupJobFailed,
			job:     fsm.PhaseStartup,
			want:    []string{"startup Job", fixSlot + "-startup-1772000000", "exited non-zero"},
		},
		{
			name:    "StartupJobTimeout",
			from:    infrav2alpha1.NodeClaimBooting,
			attempt: 1,
			reason:  infrav2alpha1.ClaimReasonStartupJobTimeout,
			job:     fsm.PhaseStartup,
			want:    []string{"startup Job", fixSlot + "-startup-1772000000", "ran out of time", "5m0s"},
		},
		{
			name:    "ShutdownJobFailed",
			from:    infrav2alpha1.NodeClaimReady,
			attempt: 1,
			reason:  infrav2alpha1.ClaimReasonShutdownJobFailed,
			job:     fsm.PhaseShutdown,
			stamp: func(nodeClaim *infrav2alpha1.NodeClaim) {
				nodeClaim.Status.ShutdownAttempts = 2
			},
			want: []string{"shutdown Job", fixSlot + "-shutdown-1772000600", "exited non-zero", "2 so far"},
		},
		{
			name:    "ShutdownJobTimeout",
			from:    infrav2alpha1.NodeClaimReady,
			attempt: 1,
			reason:  infrav2alpha1.ClaimReasonShutdownJobTimeout,
			job:     fsm.PhaseShutdown,
			want:    []string{"shutdown Job", "ran out of time"},
		},
		{
			name:    "ReadinessGateTimeout",
			from:    infrav2alpha1.NodeClaimBooting,
			attempt: 1,
			reason:  infrav2alpha1.ClaimReasonReadinessGateTimeout,
			want:    []string{fixK8sNode, "readiness gate", "4m0s", "uncordoned"},
		},
		{
			name:    "AttemptLimitExceeded",
			from:    infrav2alpha1.NodeClaimBooting,
			attempt: 4,
			reason:  infrav2alpha1.ClaimReasonAttemptLimitExceeded,
			want:    []string{"spent 4 consecutive attempts", "the bound is 3", "no machine is bought"},
		},
		{
			name:    "MachineGone",
			from:    infrav2alpha1.NodeClaimReady,
			attempt: 1,
			reason:  infrav2alpha1.ClaimReasonMachineGone,
			stamp: func(nodeClaim *infrav2alpha1.NodeClaim) {
				nodeClaim.Status.Conditions = []metav1.Condition{{
					Type:               infrav2alpha1.ClaimConditionMachineObservedOff,
					Status:             metav1.ConditionTrue,
					Reason:             infrav2alpha1.ClaimReasonMachineGone,
					Message:            "the provider observes the machine of a ready claim off",
					LastTransitionTime: observed,
				}}
			},
			want: []string{"off for 2h", "one boot window", "12m10s"},
		},
		{
			name:    "StuckTransition",
			from:    infrav2alpha1.NodeClaimBooting,
			attempt: 1,
			reason:  infrav2alpha1.ClaimReasonStuckTransition,
			want:    []string{"stuck past 15m0s", "force-cleaned"},
		},
		{
			name:    "SlotDeleted",
			from:    infrav2alpha1.NodeClaimBooting,
			attempt: 1,
			reason:  infrav2alpha1.ClaimReasonSlotDeleted,
			want:    []string{"the slot " + fixSlot + " is gone", "finalizer is released"},
		},
		{
			name:       "Superseded",
			from:       infrav2alpha1.NodeClaimBooting,
			attempt:    1,
			reason:     infrav2alpha1.ClaimReasonSuperseded,
			wantReason: EventClaimSuperseded,
			wantType:   corev1.EventTypeNormal,
			want:       []string{"superseded", fixSlot + "-newest"},
		},
		{
			name:    "a reason the api has no constant for",
			from:    infrav2alpha1.NodeClaimBooting,
			attempt: 1,
			reason:  "HandWritten",
			want:    []string{"reason HandWritten", "the slot is free"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &capturingRecorder{}

			nodeClaim := recordedClaim(tt.from, tt.attempt)

			requested := metav1.NewTime(time.Now().Add(-5 * time.Minute))
			nodeClaim.Spec.RequestedAt = requested

			if tt.stamp != nil {
				tt.stamp(nodeClaim)
			}

			objs := []client.Object{nodeClaim, slot()}

			if tt.job != "" {
				suffix := "1772000000"
				if tt.job == fsm.PhaseShutdown {
					suffix = "1772000600"
				}

				objs = append(objs, lifecycleJobOfClaim(fixClaimName, fixSlot, tt.job, suffix))
			}

			if tt.reason == infrav2alpha1.ClaimReasonSuperseded {
				nodeClaim.CreationTimestamp = metav1.NewTime(time.Now().Add(-5 * time.Minute))
				labelled(nodeClaim)

				newer := recordedClaim(infrav2alpha1.NodeClaimPending, 2)
				newer.Name = fixSlot + "-newest"
				newer.CreationTimestamp = metav1.NewTime(time.Now())
				labelled(newer)
				objs = append(objs, newer)
			}

			r := recorderWithEvents(t, rec, objs...)

			moved, err := r.Move(ctx, client.ObjectKeyFromObject(nodeClaim), infrav2alpha1.NodeClaimFailed, tt.reason)
			require.NoError(t, err)
			require.True(t, moved)

			call := oneEvent(t, rec)

			wantReason := tt.wantReason
			if wantReason == "" {
				wantReason = EventClaimFailed
			}

			assert.Equal(t, wantReason, call.reason)

			wantType := tt.wantType
			if wantType == "" {
				wantType = corev1.EventTypeWarning
			}

			assert.Equal(t, wantType, call.eventType)

			message := call.message()
			for _, want := range tt.want {
				assert.Contains(t, message, want)
			}

			for _, deny := range tt.deny {
				assert.NotContains(t, message, deny)
			}
		})
	}
}

// TestEventNoteCarriesTheMessageAsData is the rule internal/fsm/recorder.go states for the slot's
// events, applied here: the rendered message goes to the recorder as the argument of "%s", so a node
// name with a literal '%' in it - a provider payload, a URL-escaped name - is quoted, not
// re-interpreted as a format verb.
func TestEventNoteCarriesTheMessageAsData(t *testing.T) {
	ctx := context.Background()
	rec := &capturingRecorder{}

	nodeClaim := recordedClaim(infrav2alpha1.NodeClaimPhase(""), 0)
	nodeClaim.Spec.Request = deleteNodes("worker%2f-1", "5%-cpu")

	r := recorderWithEvents(t, rec, nodeClaim, slot())

	_, err := r.Seed(ctx, client.ObjectKeyFromObject(nodeClaim), slot())
	require.NoError(t, err)

	call := oneEvent(t, rec)
	assert.Equal(t, eventNoteFormat, call.note)

	message := call.message()
	assert.Contains(t, message, "nodes worker%2f-1,5%-cpu")
	assert.NotContains(t, message, "%!", "a literal percent is data, not a verb Sprintf lost")
}

// TestEventMessageIsBounded: an Event is an API object etcd keeps for the Event TTL, so a note is
// flattened onto one line and cut at the same bound the slot's events use.
func TestEventMessageIsBounded(t *testing.T) {
	long := strings.Repeat("a machine the pool wants ", 100)

	truncated := truncateEventMessage(long)
	assert.LessOrEqual(t, len([]rune(truncated)), maxEventMessageLength)
	assert.True(t, strings.HasSuffix(truncated, truncatedSuffix), "a cut note says it was cut")
	assert.NotContains(t, truncated, "\n")

	short := truncateEventMessage("attempt 1 of slot remote-worker-2   is  committed")
	assert.Equal(t, "attempt 1 of slot remote-worker-2 is committed", short, "whitespace is collapsed onto one line")
}
