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

package progress

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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/taints"
)

const testNamespace = "homelab-autoscaler-system"

// watchdogScheme is every type the watchdog reads: the Node CR, the corev1.Node and the
// batchv1.Job.
func watchdogScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	return scheme
}

// newClient builds the fake client the watchdog runs against.
func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(watchdogScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1alpha1.Node{}).
		Build()
}

// slot builds a Node CR of a pool that is being churned: the desired power state and the
// progress under test, with a Kubernetes node name the slot can be matched against.
func slot(desired infrav1alpha1.PowerState, progress infrav1alpha1.Progress) *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-1",
			Namespace: testNamespace,
			Labels:    map[string]string{"group": "remote-workers"},
		},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  desired,
			KubernetesNodeName: "worker-1-k8s",
		},
		Status: infrav1alpha1.NodeStatus{
			// powerState: on, the state a slot whose machine was deleted out of band is left
			// in: the last thing recorded about it was that it was up.
			PowerState: infrav1alpha1.PowerStateOn,
			Progress:   progress,
		},
	}
}

// liveNode is the corev1.Node of a slot, Ready.
func liveNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1-k8s"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// lifecycleJob builds a lifecycle Job of the slot, labelled the way the FSM labels one, in the
// given phase, and finished when finishedAt is non-zero.
func lifecycleJob(phase string, finishedAt time.Time) *batchv1.Job {
	name := "worker-1-" + phase + "-1700000000"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				fsm.LabelApp:        fsm.LifecycleAppName,
				fsm.LabelGroup:      "remote-workers",
				fsm.LabelNodeLegacy: "worker-1-k8s",
				fsm.LabelType:       phase,
				fsm.LabelNodeCR:     "worker-1",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{{Name: phase, Image: "busybox"}},
				},
			},
		},
	}

	if finishedAt.IsZero() {
		// Pending / running: the monitor of the machine that started it is the writer.
		started := metav1.NewTime(time.Now().Add(-30 * time.Second))
		job.Status.StartTime = &started

		return job
	}

	completed := metav1.NewTime(finishedAt)
	job.Status.StartTime = &completed
	job.Status.CompletionTime = &completed

	if phase == fsm.PhaseShutdown {
		job.Status.Failed = 1
	} else {
		job.Status.Succeeded = 1
	}

	return job
}

// readSlot reads the slot back from the client, so the assertions are about what is in the
// cluster and not about a pointer the watchdog holds.
func readSlot(t *testing.T, c client.Client) *infrav1alpha1.Node {
	t.Helper()

	node := &infrav1alpha1.Node{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "worker-1", Namespace: testNamespace}, node))

	return node
}

// settle runs one pass of the watchdog against the fake client.
func settle(t *testing.T, c client.Client, node *infrav1alpha1.Node, recorder events.EventRecorder) Outcome {
	t.Helper()

	outcome, err := Settle(context.Background(), c, node, recorder, SettleGracePeriod)
	require.NoError(t, err)

	return outcome
}

// TestSettleLeavesTheMachinelessSlotNoMachineNoJob is the live failure: a slot at
// ready / startingup / shuttingdown, with the Hetzner server deleted, so no corev1.Node points
// at it and no lifecycle Job is behind it, and spec.desiredPowerState: off. Nothing in a
// reconcile heals that state today, and every FSM branch that looks at it either builds a Job
// for a machine that does not exist (from ready) or is gated out (from a transitional progress).
//
// The settlement writes the pair the observation controllers agree on, off/shutdown, and records
// exactly one Warning naming the progress it replaced.
func TestSettleSettlesMachinelessSlot(t *testing.T) {
	for _, progress := range []infrav1alpha1.Progress{
		infrav1alpha1.ProgressReady,
		infrav1alpha1.ProgressStartingUp,
		infrav1alpha1.ProgressShuttingDown,
	} {
		t.Run(string(progress), func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)
			node := slot(infrav1alpha1.PowerStateOff, progress)
			c := newClient(t, node)

			outcome := settle(t, c, node, recorder)

			assert.True(t, outcome.Settled, "progress %q implies a machine the cluster does not have", progress)
			assert.Equal(t, progress, outcome.Previous, "the outcome names the progress it replaced")
			assert.Zero(t, outcome.RequeueAfter, "nothing is in flight, so no look is scheduled")

			after := readSlot(t, c)
			assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress, "the slot is settled to shutdown")
			assert.Equal(t, infrav1alpha1.PowerStateOff, after.Status.PowerState, "and to the power state the slot is desired in")
			assert.Equal(t, infrav1alpha1.PowerStateOff, after.Spec.DesiredPowerState, "spec is never written")

			require.Equal(t, 1, len(recorder.Events), "one settlement records one event")

			event := <-recorder.Events
			assert.Contains(t, event, corev1.EventTypeWarning, "a slot pointing at a machine that is gone is not a Normal event")
			assert.Contains(t, event, ReasonProgressReconciled)
			assert.Contains(t, event, string(progress), "the event names the progress it replaced")
			assert.Contains(t, event, "worker-1")
			assert.Contains(t, event, "worker-1-k8s", "the event names the node that is not there")
		})
	}
}

// TestSettleNeverOverridesTheClaimIsTheRearm is the M3 rule. A slot that was claimed
// (spec.desiredPowerState: on), whose startup Job completed, and whose node never registered ends at
// {desired: on, powerState: off, progress: ready}. Nothing in the system can heal that: the
// settlement refuses it (the claim gate), CanTransition(StartNode) is false from ready,
// NodeGroupTargetSize counts it forever, DecreaseTargetSize cannot cancel it (isStartingUpSlot wants
// progress: startingup), and the autoscaler believes the group has capacity that never materialises.
//
// So the slot is re-armed, not settled: the progress and the power state are the lie about the
// machine, and they are removed; spec.desiredPowerState is the request, and it stays. That is the
// pair the FSM's StartNode is legal from, so the pool retries the machine.
func TestSettleNeverOverridesTheClaimIsTheRearm(t *testing.T) {
	recorder := events.NewFakeRecorder(8)
	node := slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	node.Status.PowerState = infrav1alpha1.PowerStateOff
	c := newClient(t, node)

	outcome := settle(t, c, node, recorder)

	require.True(t, outcome.Rearmed, "a claimed slot whose machine is gone is re-armed, not left pointing at one")
	assert.Equal(t, ActionRearm, outcome.Action)
	assert.Equal(t, infrav1alpha1.ProgressReady, outcome.Previous, "the outcome names the progress it replaced")
	assert.Zero(t, outcome.Attempts, "no attempt has been spent yet: the count is the FSMs, and this pass arms the first")

	after := readSlot(t, c)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress, "progress: shutdown is the state StartNode is legal from")
	assert.Equal(t, infrav1alpha1.PowerStateOff, after.Status.PowerState, "and the power state the machine is really in")
	assert.Equal(t, infrav1alpha1.PowerStateOn, after.Spec.DesiredPowerState,
		"spec.desiredPowerState is the claim and is never dropped here: that is the provider's arithmetic")

	// The FSM consequence, asserted on the machine the node controller builds from this shape.
	stateMachine := fsm.NewNodeStateMachine(after, c, watchdogScheme(t),
		fsm.NewCoordinationManager(c, fsm.NodeControllerOwner))
	assert.Equal(t, fsm.StateShutdown, stateMachine.GetCurrentState(), "the FSM reads shutdown from the progress")
	assert.True(t, stateMachine.CanTransition(fsm.EventStartNode),
		"and StartNode is legal again, which is what makes the pool retry the machine")

	require.Len(t, recorder.Events, 1, "one re-arm, one event")

	event := <-recorder.Events
	assert.Contains(t, event, corev1.EventTypeNormal, "a re-arm is the system healing itself, not an incident")
	assert.Contains(t, event, ReasonClaimRearmed)
	assert.Contains(t, event, "startup attempt 1 of 3", "the event names the attempt the arming leads to, so a pool that keeps buying machines is legible")
	assert.Contains(t, event, string(infrav1alpha1.ProgressReady), "and names the progress it replaced")
}

// TestSettleRearmIsTheSameGateAsTheSettlement: the actions share every gate, so the evidence that
// lets a settlement is the evidence that lets a re-arm and the cordon fix. Each row is one gate: a
// claimed slot with the gate open gets its action, and the same slot with the gate closed gets
// nothing at all - no write, no event, no drift between the three.
func TestSettleRearmIsTheSameGateAsTheSettlement(t *testing.T) {
	tests := []struct {
		name       string
		object     client.Object
		mutate     func(*infrav1alpha1.Node)
		wantAction Action
		wantReason string
	}{
		{
			name:       "no machine, no job, no lock: the claim is re-armed",
			wantAction: ActionRearm,
		},
		{
			name:   "the slot has a Kubernetes node: the observation controllers own it",
			object: liveNode(),
		},
		{
			name:       "a node object that is cordoned: the safety net uncordons it",
			object:     cordonedNode(),
			wantAction: ActionUncordon,
		},
		{
			name:   "a cordoned node cluster-autoscaler marked: its decision stands",
			object: markedNode(),
		},
		{
			name:   "a lifecycle job in flight",
			object: lifecycleJob(fsm.PhaseStartup, time.Time{}),
		},
		{
			name:   "a lifecycle job that just finished",
			object: lifecycleJob(fsm.PhaseStartup, time.Now().Add(-time.Minute)),
		},
		{
			name: "a live coordination lock",
			mutate: func(node *infrav1alpha1.Node) {
				node.Annotations = map[string]string{
					fsm.OperationLockAnnotation: fsm.OperationScaleUp,
					fsm.LockOwnerAnnotation:     fsm.NodeControllerOwner,
					fsm.LockTimestampAnnotation: time.Now().Format(time.RFC3339),
					fsm.LockTimeoutAnnotation:   "300",
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)

			node := slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
			node.Status.PowerState = infrav1alpha1.PowerStateOff

			if tc.mutate != nil {
				tc.mutate(node)
			}

			objects := []client.Object{node}
			if tc.object != nil {
				objects = append(objects, tc.object)
			}

			c := newClient(t, objects...)
			before := readSlot(t, c)

			outcome := settle(t, c, node, recorder)

			assert.Equal(t, tc.wantAction, outcome.Action)

			after := readSlot(t, c)

			if tc.wantAction == ActionNone {
				assert.NotEmpty(t, outcome.Reason, "a refusal always says why")
				assert.Equal(t, before.Status, after.Status, "the gate owns the slot, so nothing is written")
				assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
				assert.Zero(t, len(recorder.Events))

				return
			}

			assert.Empty(t, outcome.Reason, "an action is reported by its event, not by a refusal reason")

			switch tc.wantAction {
			case ActionRearm:
				assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress)
				assert.Equal(t, infrav1alpha1.PowerStateOn, after.Spec.DesiredPowerState)
			case ActionUncordon:
				assert.Equal(t, infrav1alpha1.ProgressReady, after.Status.Progress, "the cordon fix writes no progress")
				assert.False(t, readKubernetesNode(t, c).Spec.Unschedulable, "it clears the cordon, on the node object")
			}
		})
	}
}

// cordonedNode is the residue of an operation that ended: the machine is Ready, the slot is claimed
// on, and spec.unschedulable is still set from the shutdown that did not finish.
func cordonedNode() *corev1.Node {
	node := liveNode()
	node.Spec.Unschedulable = true

	return node
}

// markedNode is the same machine cordoned by cluster-autoscaler, with its deletion marker on it.
func markedNode() *corev1.Node {
	node := cordonedNode()
	node.Spec.Taints = []corev1.Taint{
		{Key: taints.ToBeDeletedByClusterAutoscaler, Effect: corev1.TaintEffectNoSchedule},
	}

	return node
}

// readKubernetesNode reads the corev1.Node of the slot back from the client.
func readKubernetesNode(t *testing.T, c client.Client) *corev1.Node {
	t.Helper()

	node := &corev1.Node{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "worker-1-k8s"}, node))

	return node
}

// stampAttempts writes the startup-attempt record the FSM owns onto the slot in the client, in the
// wire shape the bound reads (fsm.ReasonStartupAttempt + the count). The watchdog never writes that
// record, so a test of its bound has to put it there the way the cluster does: an attempt counted by
// the transition into startingup, not by a pass of this package.
func stampAttempts(t *testing.T, c client.Client, attempts int) {
	t.Helper()

	current := readSlot(t, c)

	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:    ConditionClaimRearmed,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonClaimRearmAttempt + fmt.Sprint(attempts),
		Message: fmt.Sprintf("startup attempt %d recorded by the FSM", attempts),
	})

	require.NoError(t, c.Status().Update(context.Background(), current))
}

// TestSettleSpentAttemptsRearmsUntilTheBoundThenSettlesAndSaysSoOnce is the bound and the phantom
// closed. Each re-arm is a startup the pool is about to make, and the count that bounds it is the
// FSM's startup-attempt record: the watchdog reads it, never writes it. Until the bound the slot is
// re-armed so the pool retries the machine; at the bound the re-arming stops and the slot is settled
// to the stable off/shutdown pair, with the claim kept and exactly one Warning naming it.
//
// The settlement at the bound is the point. Refusing to re-arm and leaving the progress where it was
// leaves a slot at ready with no machine: a slot the FSM cannot start (StartNode is illegal from
// ready), cannot shut down without building a shutdown Job for a server that is already gone, and
// cannot be healed by any other writer - the bound would have traded an unbounded loop for a frozen
// phantom. The claim and its target-size unit stay in both shapes, by design: the request is the
// provider's to drop (countsTowardsTargetSize counts a claimed slot whatever its progress says), and
// what the settlement changes is that the progress stops lying, which is what makes the slot
// healable when an operator fixes the template.
func TestSettleSpentAttemptsRearmsUntilTheBoundThenSettlesAndSaysSoOnce(t *testing.T) {
	node := slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	node.Status.PowerState = infrav1alpha1.PowerStateOff
	c := newClient(t, node)

	for spent := 0; spent < ClaimRearmLimit; spent++ {
		recorder := events.NewFakeRecorder(8)

		// Each pass is the shape the previous attempt left: the slot is at a progress that names a
		// machine, the startup that ran from the last re-arm finished, and the FSM counted that
		// attempt on the transition into startingup.
		current := readSlot(t, c)
		current.Status.Progress = infrav1alpha1.ProgressReady
		current.Status.PowerState = infrav1alpha1.PowerStateOff
		require.NoError(t, c.Status().Update(context.Background(), current))

		if spent > 0 {
			stampAttempts(t, c, spent)
		}

		outcome := settle(t, c, readSlot(t, c), recorder)

		require.True(t, outcome.Rearmed, "%d attempts spent of %d: the slot is still armed", spent, ClaimRearmLimit)
		assert.Equal(t, spent, outcome.Attempts, "the count the pass reports is the FSM's, read not written")

		after := readSlot(t, c)
		assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress)
		assert.Equal(t, infrav1alpha1.PowerStateOn, after.Spec.DesiredPowerState, "the claim stands")
		assert.Equal(t, spent, fsm.StartupAttemptsOf(after),
			"a re-arm arms an attempt, it does not spend one: the counter belongs to the transition that starts the machine, so the loud loop (a Job that exits non-zero) pays into the same account")

		event := <-recorder.Events
		assert.Contains(t, event, ReasonClaimRearmed)
		assert.Contains(t, event, fmt.Sprintf("startup attempt %d of %d", spent+1, ClaimRearmLimit),
			"the event names the attempt the arming leads to")
	}

	// The bound is used up: the third attempt booted nothing, and the FSM counted it. The slot
	// settles, keeps its claim, and says so - once.
	recorder := events.NewFakeRecorder(8)

	stampAttempts(t, c, ClaimRearmLimit)

	current := readSlot(t, c)
	current.Status.Progress = infrav1alpha1.ProgressReady
	current.Status.PowerState = infrav1alpha1.PowerStateOff
	require.NoError(t, c.Status().Update(context.Background(), current))

	outcome := settle(t, c, readSlot(t, c), recorder)

	assert.Equal(t, ActionSettle, outcome.Action, "the re-arming stopped, and the slot is settled in its place")
	assert.True(t, outcome.Settled)
	assert.Zero(t, outcome.RequeueAfter, "and it is not scheduled to resume: the bound is the end of the loop")
	assert.Empty(t, outcome.Reason, "the action is reported by its event; the refusal reason is for the no-action path")
	assert.Equal(t, ClaimRearmLimit, outcome.Attempts, "the Warning names the attempts that were spent")

	after := readSlot(t, c)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress,
		"no machine, so no ready: the phantom is closed, not renamed")
	assert.Equal(t, infrav1alpha1.PowerStateOff, after.Status.PowerState)
	assert.Equal(t, infrav1alpha1.PowerStateOn, after.Spec.DesiredPowerState, "and its claim is untouched")

	require.Len(t, recorder.Events, 1, "the bound is an operator-visible Warning")
	event := <-recorder.Events
	assert.Contains(t, event, corev1.EventTypeWarning)
	assert.Contains(t, event, ReasonClaimRearmLimitReached)
	assert.Contains(t, event, fmt.Sprint(ClaimRearmLimit), "the Warning names the bound that was hit")

	assert.Equal(t, ReasonClaimRearmLimitReached, rearmCondition(t, c).Reason,
		"the record is the note, so the passes after it have nothing left to write")

	before := readSlot(t, c)

	for range 3 {
		quiet := events.NewFakeRecorder(8)
		again := settle(t, c, readSlot(t, c), quiet)

		assert.Equal(t, ActionNone, again.Action)
		assert.Zero(t, len(quiet.Events), "a slot that will not boot is reported once, not on every resync")
	}

	assert.Equal(t, before.ResourceVersion, readSlot(t, c).ResourceVersion, "and the note is written exactly once")
}

// TestSettleRearmLeavesTheAttemptRecordAlone: the counter is the FSM's. A re-arm that stamped the
// record is a count that charges the watchdog's passes, which is the shape that made a bound of 3 a
// lifetime sentence - slots are reused across claim episodes, and the record never went away.
func TestSettleRearmLeavesTheAttemptRecordAlone(t *testing.T) {
	node := slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	node.Status.PowerState = infrav1alpha1.PowerStateOff
	c := newClient(t, node)

	settle(t, c, node, events.NewFakeRecorder(8))

	after := readSlot(t, c)
	assert.Zero(t, fsm.StartupAttemptsOf(after), "the watchdog arms attempts and counts none")
	assert.Nil(t, meta.FindStatusCondition(after.Status.Conditions, ConditionClaimRearmed),
		"and writes no record at all")
}

// TestSettleRearmIsIdempotent: a slot that was re-armed and has not run a startup since is at
// shutdown, which implies no machine, so the passes after the re-arm write nothing and record
// nothing - the same rule that keeps a healthy pool quiet.
func TestSettleRearmIsIdempotent(t *testing.T) {
	node := slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	node.Status.PowerState = infrav1alpha1.PowerStateOff
	c := newClient(t, node)

	settle(t, c, node, events.NewFakeRecorder(8))

	before := readSlot(t, c)

	for range 3 {
		recorder := events.NewFakeRecorder(8)
		outcome := settle(t, c, readSlot(t, c), recorder)

		assert.False(t, outcome.Rearmed, "a re-armed slot is re-armed once per startup that dies")
		assert.Zero(t, outcome.RequeueAfter)
		assert.Zero(t, len(recorder.Events), "nothing to do, nothing recorded")
	}

	after := readSlot(t, c)
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
}

// rearmCondition reads the re-arm record back off the slot in the client.
func rearmCondition(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()

	condition := meta.FindStatusCondition(readSlot(t, c).Status.Conditions, ConditionClaimRearmed)
	require.NotNil(t, condition, "the re-arm record lives on the slot")

	return condition
}

// TestSettleLeavesASlotWithAJobAlone is the FSM's ownership: the lifecycle Job is the operation
// and the state machine that started it is the writer that settles its progress. The watchdog
// must not touch any slot that has one, in any of the shapes a Job in flight takes.
func TestSettleLeavesASlotWithAJobAlone(t *testing.T) {
	tests := []struct {
		name  string
		job   *batchv1.Job
		rejob bool
	}{
		{
			name:  "a pending startup job",
			job:   lifecycleJob(fsm.PhaseStartup, time.Time{}),
			rejob: true,
		},
		{
			name:  "a running shutdown job",
			job:   lifecycleJob(fsm.PhaseShutdown, time.Time{}),
			rejob: true,
		},
		{
			name: "a startup job that finished seconds ago",
			// (h) The window the grace exists for: the Job completed, the monitor of the
			// machine that started it has not fired JobCompleted yet.
			job: lifecycleJob(fsm.PhaseStartup, time.Now().Add(-15*time.Second)),
		},
		{
			name: "a shutdown job that failed seconds ago",
			job:  lifecycleJob(fsm.PhaseShutdown, time.Now().Add(-5*time.Second)),
		},
		{
			name: "a finished job with no completion timestamp, measured from its start",
			job: func() *batchv1.Job {
				job := lifecycleJob(fsm.PhaseStartup, time.Now().Add(-10*time.Second))
				job.Status.CompletionTime = nil

				return job
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)
			node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
			c := newClient(t, node, tc.job)

			before := readSlot(t, c)
			outcome := settle(t, c, node, recorder)

			after := readSlot(t, c)
			assert.False(t, outcome.Settled, "the operation owns the progress")
			assert.Equal(t, before.Status, after.Status)
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "a pass that settles nothing writes nothing")
			assert.Zero(t, len(recorder.Events))

			if tc.rejob {
				assert.Equal(t, SettleGracePeriod, outcome.RequeueAfter, "a Job in flight is looked at again on the grace")
			} else {
				assert.Greater(t, outcome.RequeueAfter, time.Duration(0), "the grace expiry is scheduled, not forgotten")
				assert.LessOrEqual(t, outcome.RequeueAfter, SettleGracePeriod)
			}
		})
	}
}

// TestSettleLeavesASlotWithAMachineAlone: a corev1.Node object - Ready or NotReady - is a
// machine, and the status of a slot with one is owned by the observation controllers. The
// watchdog settles slots with no machine, nothing else.
func TestSettleLeavesASlotWithAMachineAlone(t *testing.T) {
	for _, ready := range []bool{true, false} {
		t.Run(map[bool]string{true: "Ready", false: "NotReady"}[ready], func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)
			node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady)
			kubeNode := liveNode()

			if !ready {
				kubeNode.Status.Conditions[0].Status = corev1.ConditionFalse
			}

			c := newClient(t, node, kubeNode)

			before := readSlot(t, c)
			outcome := settle(t, c, node, recorder)

			after := readSlot(t, c)
			assert.False(t, outcome.Settled)
			assert.Contains(t, outcome.Reason, "observation controllers")
			assert.Equal(t, before.Status, after.Status)
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
			assert.Zero(t, len(recorder.Events))
		})
	}
}

// TestSettleIsIdempotent covers the steady state of a healthy pool: a slot that is already
// settled, and a slot that was settled by the pass before this one. A write on every pass bumps
// the slot, re-triggers the watches and keeps the controller hot, so nothing may be written and
// nothing recorded.
func TestSettleIsIdempotent(t *testing.T) {
	tests := []struct {
		name         string
		progress     infrav1alpha1.Progress
		wantProgress infrav1alpha1.Progress
	}{
		{"already shutdown", infrav1alpha1.ProgressShutdown, infrav1alpha1.ProgressShutdown},
		{"never seeded", "", ""},
		{"settled by the previous pass", infrav1alpha1.ProgressReady, infrav1alpha1.ProgressShutdown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)
			node := slot(infrav1alpha1.PowerStateOff, tc.progress)
			c := newClient(t, node)

			settle(t, c, node, recorder)

			for range 3 {
				outcome := settle(t, c, node, recorder)
				assert.False(t, outcome.Settled, "a settled slot is settled once")
				assert.Zero(t, outcome.RequeueAfter)
			}

			after := readSlot(t, c)
			assert.Equal(t, tc.wantProgress, after.Status.Progress)

			if tc.progress == infrav1alpha1.ProgressShutdown || tc.progress == "" {
				assert.Zero(t, len(recorder.Events), "nothing to settle, nothing recorded")
			}
		})
	}
}

// TestSettleLeavesASlotBehindALiveLockAlone: the coordination lock says an operation owns the
// slot right now. The window this closes is the one inside a transition - enterShuttingDown has
// written the progress, the Job has not been created yet - and the scale-down the provider is
// running under its own lock. An expired lock is not a lock: nothing is running under it, and a
// slot frozen behind one is what the watchdog exists for.
func TestSettleLeavesASlotBehindALiveLockAlone(t *testing.T) {
	node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
	node.Annotations = map[string]string{
		fsm.OperationLockAnnotation: fsm.OperationScaleDown,
		fsm.LockOwnerAnnotation:     fsm.NodeControllerOwner,
		fsm.LockTimestampAnnotation: time.Now().Format(time.RFC3339),
		fsm.LockTimeoutAnnotation:   "300",
	}

	c := newClient(t, node)
	before := readSlot(t, c)

	outcome := settle(t, c, node, events.NewFakeRecorder(8))

	after := readSlot(t, c)
	assert.False(t, outcome.Settled)
	assert.Contains(t, outcome.Reason, "coordination lock")
	assert.Equal(t, before.Status, after.Status)

	t.Run("an expired lock does not hold the slot", func(t *testing.T) {
		expired := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
		expired.Annotations = map[string]string{
			fsm.OperationLockAnnotation: fsm.OperationScaleDown,
			fsm.LockOwnerAnnotation:     fsm.NodeControllerOwner,
			fsm.LockTimestampAnnotation: time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
			fsm.LockTimeoutAnnotation:   "300",
		}

		c := newClient(t, expired)
		outcome := settle(t, c, expired, events.NewFakeRecorder(8))
		assert.True(t, outcome.Settled, "a lock nothing is running under must not freeze the slot forever")
	})
}

// TestDecideIsTheRule keeps the rule itself readable without any client in the way: the table is the
// contract, and each row says which gate stopped the action, or which action the evidence allows.
func TestDecideIsTheRule(t *testing.T) {
	now := time.Now()

	claimed := func(progress infrav1alpha1.Progress) *infrav1alpha1.Node {
		node := slot(infrav1alpha1.PowerStateOn, progress)
		node.Status.PowerState = infrav1alpha1.PowerStateOff

		return node
	}

	tests := []struct {
		name         string
		node         *infrav1alpha1.Node
		obs          observations
		wantAction   Action
		wantRequeue  time.Duration
		wantReasonIs string
	}{
		{
			name:         "ready, off, nothing behind it",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady),
			wantAction:   ActionSettle,
			wantReasonIs: "no Kubernetes node",
		},
		{
			name:         "claimed, no node: re-arm, never settle",
			node:         claimed(infrav1alpha1.ProgressReady),
			wantAction:   ActionRearm,
			wantReasonIs: "no Kubernetes node behind it",
		},
		{
			name:         "claimed, startingup, no node: the same shape, the same action",
			node:         claimed(infrav1alpha1.ProgressStartingUp),
			wantAction:   ActionRearm,
			wantReasonIs: "startup finished",
		},
		{
			// The bound closes the slot, it does not freeze it: a claimed slot whose startup
			// attempts are spent is settled to the stable pair, because leaving it at ready with no
			// machine is the phantom the whole path exists to remove, and the FSM refuses the start
			// a re-arm would lead to.
			name:         "claimed, at the startup-attempt bound: settle, and say the reason is the bound",
			node:         claimed(infrav1alpha1.ProgressReady),
			obs:          observations{startupAttempts: ClaimRearmLimit},
			wantAction:   ActionSettle,
			wantReasonIs: "limit",
		},
		{
			name:         "progress that implies no machine",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
			wantReasonIs: "does not imply a machine",
		},
		{
			name:         "machine present, desired off: the observation controllers own it",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady),
			obs:          observations{nodePresent: true},
			wantReasonIs: "observation controllers",
		},
		{
			name:         "machine present, claimed, cordoned, no marker: the safety net",
			node:         claimed(infrav1alpha1.ProgressReady),
			obs:          observations{nodePresent: true, nodeCordoned: true},
			wantAction:   ActionUncordon,
			wantReasonIs: "cordoned with no Job",
		},
		{
			name:         "machine present, claimed, cordoned by cluster-autoscaler: its decision stands",
			node:         claimed(infrav1alpha1.ProgressReady),
			obs:          observations{nodePresent: true, nodeCordoned: true, nodeMarked: true},
			wantReasonIs: "observation controllers",
		},
		{
			name:         "machine present, claimed, cordoned, transitional progress: the operation owns it",
			node:         claimed(infrav1alpha1.ProgressStartingUp),
			obs:          observations{nodePresent: true, nodeCordoned: true},
			wantReasonIs: "observation controllers",
		},
		{
			name:         "lock held",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown),
			obs:          observations{lockHeld: true},
			wantReasonIs: "owns the slot",
		},
		{
			name:         "lock held, claimed: the lock gate is in front of the re-arm too",
			node:         claimed(infrav1alpha1.ProgressStartingUp),
			obs:          observations{lockHeld: true},
			wantReasonIs: "owns the slot",
		},
		{
			name:         "job in flight",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
			obs:          observations{jobs: []batchv1.Job{*lifecycleJob(fsm.PhaseStartup, time.Time{})}},
			wantAction:   ActionNone,
			wantRequeue:  SettleGracePeriod,
			wantReasonIs: "in flight",
		},
		{
			name:         "job in flight, claimed: the grace is the settlement's gate and the re-arm's",
			node:         claimed(infrav1alpha1.ProgressStartingUp),
			obs:          observations{jobs: []batchv1.Job{*lifecycleJob(fsm.PhaseStartup, time.Time{})}},
			wantAction:   ActionNone,
			wantRequeue:  SettleGracePeriod,
			wantReasonIs: "in flight",
		},
		{
			name: "job finished inside the grace",
			node: slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
			obs: observations{
				jobs: []batchv1.Job{*lifecycleJob(fsm.PhaseStartup, now.Add(-90*time.Second))},
				now:  now,
			},
			// The grace is SettleGracePeriod, 90s of it spent: the look is scheduled for the rest.
			wantRequeue:  SettleGracePeriod - 90*time.Second,
			wantReasonIs: "inside the settle grace",
		},
		{
			name: "job finished past the grace",
			node: slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
			obs: observations{
				jobs: []batchv1.Job{*lifecycleJob(fsm.PhaseStartup, now.Add(-SettleGracePeriod-time.Minute))},
				now:  now,
			},
			wantAction:   ActionSettle,
			wantReasonIs: "no Kubernetes node",
		},
		{
			name: "the tightest deadline of several jobs wins",
			node: slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown),
			obs: observations{
				jobs: []batchv1.Job{
					*lifecycleJob(fsm.PhaseStartup, now.Add(-5*time.Minute)),
					*lifecycleJob(fsm.PhaseShutdown, now.Add(-110*time.Second)),
				},
				now: now,
			},
			wantRequeue:  SettleGracePeriod - 5*time.Minute,
			wantReasonIs: "inside the settle grace",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := tc.obs
			obs.grace = SettleGracePeriod

			if obs.now.IsZero() {
				obs.now = now
			}

			d := decide(tc.node, obs)

			assert.Equal(t, tc.wantAction, d.action)
			assert.Equal(t, tc.wantRequeue, d.requeueAfter)
			assert.Contains(t, d.reason, tc.wantReasonIs)

			if tc.wantAction == ActionSettle || tc.wantAction == ActionRearm {
				assert.Equal(t, tc.node.Status.Progress, d.previous, "the action records what it replaces")
			}

			if tc.wantAction == ActionUncordon {
				assert.Empty(t, d.previous, "the cordon fix writes no progress, so it replaces none")
			}
		})
	}
}

// TestSettleGracePeriodOutlivesTheWritersItDefersTo is the M1 invariant. The grace exists so a
// lifecycle Job that finished, whose after_JobCompleted has not finished running, is not stomped - and
// that hook is not instantaneous: it runs setNodeSchedulableWithRetry, which can spend one monitor poll
// noticing the Job, the whole registration budget waiting for the node object, the whole readiness gate
// waiting for it to be usable, and one sleep at the ceiling of its backoff. The grace must be strictly
// longer than that sum, which is what the expression computes: a value that only covers the monitor
// poll (the 2m this constant used to be) settles a slot whose hook has minutes of gate left, and the
// hook then writes ready on top of the settlement.
//
// The bound it has to respect is the stuck-transition threshold, not the Job timeout: the grace only
// applies to a *finished* Job, so a Job of a live monitor is protected by the Job, at any length.
func TestSettleGracePeriodOutlivesTheWritersItDefersTo(t *testing.T) {
	writersWindow := fsm.JobMonitorPollInterval +
		fsm.DefaultUncordonTimeout +
		fsm.DefaultReadinessGateTimeout +
		fsm.MaxUncordonRetryInterval

	assert.Greater(t, SettleGracePeriod, writersWindow,
		"the grace must be longer than the whole path a completed Job's hook can take: %v of writers, %v of grace",
		writersWindow.String(), SettleGracePeriod.String())

	assert.Greater(t, SettleGracePeriod, fsm.JobMonitorPollInterval,
		"a Job that finished is polled by the writer it defers to, so the grace is more than one poll")
	assert.Greater(t, SettleGracePeriod, time.Minute, "a reconcile is seconds; the grace is minutes")
	assert.Less(t, SettleGracePeriod, fsm.StuckTransitionThreshold,
		"a slot whose monitor died with its manager heals well inside the horizon an operator calls stuck")

	// The gate and the grace are measured against the same loop: a gate with no deadline of its own is
	// the bug this round closed, and the sum above silently stops meaning anything if the two budgets
	// are merged again.
	assert.Greater(t, fsm.DefaultReadinessGateTimeout, fsm.DefaultUncordonTimeout,
		"the readiness gate needs a longer budget than the registration wait it used to share")
	assert.Greater(t, fsm.DefaultReadinessGateTimeout+fsm.DefaultUncordonTimeout, fsm.DefaultLockTimeout,
		"the gate can outlast the coordination lock, which is why the loop renews it")
}

// TestFinishedAtCoversEveryShapeAFinishedJobTakes: a lifecycle Job that cannot be measured must
// still age out of the grace, or it freezes its slot forever. The fallbacks are ordered newest
// first, and an unfinished Job reports "not finished" whatever it carries.
func TestFinishedAtCoversEveryShapeAFinishedJobTakes(t *testing.T) {
	at := time.Now().Add(-4 * time.Minute)
	stamp := metav1.NewTime(at)

	tests := []struct {
		name     string
		job      *batchv1.Job
		wantOK   bool
		wantTime time.Time
	}{
		{
			name:     "completion time",
			job:      lifecycleJob(fsm.PhaseStartup, at),
			wantOK:   true,
			wantTime: at,
		},
		{
			name: "condition, no completion time",
			job: func() *batchv1.Job {
				job := lifecycleJob(fsm.PhaseStartup, at)
				job.Status.CompletionTime = nil
				job.Status.Conditions = []batchv1.JobCondition{{
					Type:               batchv1.JobComplete,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: stamp,
				}}

				return job
			}(),
			wantOK:   true,
			wantTime: at,
		},
		{
			name: "start time, nothing else",
			job: func() *batchv1.Job {
				job := lifecycleJob(fsm.PhaseStartup, at)
				job.Status.CompletionTime = nil

				return job
			}(),
			wantOK:   true,
			wantTime: at,
		},
		{
			name: "creation timestamp, nothing else",
			job: func() *batchv1.Job {
				job := lifecycleJob(fsm.PhaseStartup, time.Time{})
				job.Status.Succeeded = 1
				job.Status.StartTime = nil
				job.CreationTimestamp = stamp

				return job
			}(),
			wantOK:   true,
			wantTime: at,
		},
		{
			name:   "running job is not finished",
			job:    lifecycleJob(fsm.PhaseStartup, time.Time{}),
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := finishedAt(tc.job)
			assert.Equal(t, tc.wantOK, ok)

			if tc.wantOK {
				assert.WithinDuration(t, tc.wantTime, got, time.Second)
			}
		})
	}
}

// TestSettleIgnoresJobsOfAnotherSlot: the selector matches the slot's provenance labels, so a
// neighbouring slot's lifecycle Job - the common shape in a churning pool - must not hold this
// slot's settlement.
func TestSettleIgnoresJobsOfAnotherSlot(t *testing.T) {
	other := lifecycleJob(fsm.PhaseShutdown, time.Time{})
	other.Name = "worker-2-shutdown-1700000000"
	other.Labels[fsm.LabelNodeCR] = "worker-2"
	other.Labels[fsm.LabelNodeLegacy] = "worker-2-k8s"

	node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady)
	c := newClient(t, node, other)

	outcome := settle(t, c, node, events.NewFakeRecorder(8))
	assert.True(t, outcome.Settled)

	jobs, err := lifecycleJobs(context.Background(), c, node)
	require.NoError(t, err)
	assert.Empty(t, jobs, "only the slot's own Jobs are considered")
}

// TestSettlementNoteNamesTheEvidence keeps the operator-facing half of the fix readable: the
// progress that was replaced, the machine that is not there, and the values that were written.
func TestSettlementNoteNamesTheEvidence(t *testing.T) {
	note := settlementNote(slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown), infrav1alpha1.ProgressShuttingDown)

	for _, want := range []string{"shuttingdown", "shutdown", "worker-1", "worker-1-k8s", "desiredPowerState"} {
		assert.Contains(t, note, want)
	}

	assert.False(t, strings.Contains(note, "\n"), "an Event message is one line")
}

// rerereadClient is the fake the two write-path tests need: it intercepts the reads of a pass and
// changes what the caller sees, so the write path is exercised against an object that moved under it.
// The watchdog re-derives its whole rule from a freshly read copy inside the retry, and these are the
// tests that prove it does.
type rerereadClient struct {
	client.Client

	// onNodeGet rewrites the Node CR handed back by a Get, the first time it is asked for. That is
	// the read inside the write path: the decision was made on the copy the pass started with, and
	// this is the shape the slot has turned into in the meantime.
	onNodeGet func(*infrav1alpha1.Node)

	// nodeGetCount counts the reads of the Node CR, so "the first time" is precise.
	nodeGetCount int

	// onKubernetesNodeGet is the same hook for the corev1.Node read: it returns the error that
	// read answers with.
	onKubernetesNodeGet func() error
}

func (c *rerereadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}

	if slot, isSlot := obj.(*infrav1alpha1.Node); isSlot {
		c.nodeGetCount++

		if c.onNodeGet != nil && c.nodeGetCount == 1 {
			c.onNodeGet(slot)
		}

		return nil
	}

	if _, isKubeNode := obj.(*corev1.Node); isKubeNode && c.onKubernetesNodeGet != nil {
		return c.onKubernetesNodeGet()
	}

	return nil
}

// TestSettleStopsQualifyingOnRereadWritesNothing is the write path's own gate. The pass decided on the
// slot it was handed; the write re-reads it, and an operation, a claim, or a machine that arrived in
// between owns the slot. A settlement that goes through on a stale decision is a controller stomping
// an operation - the one failure mode of this package that matters - so the re-derived answer wins.
func TestSettleStopsQualifyingOnRereadWritesNothing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*infrav1alpha1.Node)
		hint   string
	}{
		{
			name: "a coordination lock appeared",
			mutate: func(node *infrav1alpha1.Node) {
				node.Annotations = map[string]string{
					fsm.OperationLockAnnotation: fsm.OperationScaleUp,
					fsm.LockOwnerAnnotation:     fsm.NodeControllerOwner,
					fsm.LockTimestampAnnotation: time.Now().Format(time.RFC3339),
					fsm.LockTimeoutAnnotation:   "300",
				}
			},
		},
		{
			name:   "the claim was dropped",
			mutate: func(node *infrav1alpha1.Node) { node.Spec.DesiredPowerState = infrav1alpha1.PowerStateOff },
		},
		{
			name:   "the progress already moved",
			mutate: func(node *infrav1alpha1.Node) { node.Status.Progress = infrav1alpha1.ProgressShutdown },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)

			node := slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
			node.Status.PowerState = infrav1alpha1.PowerStateOff

			base := newClient(t, node)
			c := &rerereadClient{Client: base, onNodeGet: tc.mutate}

			before := readSlot(t, base)
			outcome := settle(t, c, node, recorder)

			assert.Equal(t, ActionNone, outcome.Action, "the write path re-decided and stopped")
			assert.False(t, outcome.Rearmed)
			assert.Contains(t, outcome.Reason, "re-read")

			after := readSlot(t, base)
			assert.Equal(t, infrav1alpha1.ProgressReady, after.Status.Progress, "the slot is exactly as the operation left it")
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "nothing was written")
			assert.Zero(t, len(recorder.Events), "and nothing was recorded for a decision that no longer holds")
		})
	}
}

// TestSettleAnAPIErrorOnTheNodeReadIsNotEvidence: not knowing whether the machine is there is not
// knowing that it is not. A read that fails - the API server is unreachable, the RBAC is missing, the
// request timed out - must surface as the error it is, with no write and no event, so the controller's
// backoff brings the pass back. A settlement written from a failed read is a settlement written
// against a live machine, and it is the churn this package exists to stop.
func TestSettleAnAPIErrorOnTheNodeReadIsNotEvidence(t *testing.T) {
	recorder := events.NewFakeRecorder(8)

	node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady)
	base := newClient(t, node, liveNode())
	c := &rerereadClient{
		Client: base,
		onKubernetesNodeGet: func() error {
			return fmt.Errorf("connection refused")
		},
	}

	before := readSlot(t, base)

	outcome, err := Settle(context.Background(), c, node, recorder, SettleGracePeriod)
	require.Error(t, err, "the caller reports the read it could not make")
	assert.Contains(t, err.Error(), "connection refused", "the reason travels, it is not flattened into a refusal")
	assert.Contains(t, err.Error(), "worker-1-k8s", "and it names the node the pass could not read")
	assert.Empty(t, outcome, "nothing was decided, so nothing is reported")

	after := readSlot(t, base)
	assert.Equal(t, before.Status, after.Status, "a slot with a machine is not settled on a failed read")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
	assert.Zero(t, len(recorder.Events))
}

// TestSettleUncordonsTheResidueOfAnOperationThatEnded is the safety net. The paths that roll a slot
// back into ready - JobTimeout, JobFailed - never call the uncordon hook, so a machine cordoned by the
// shutdown that did not finish stays cordoned under a slot the autoscaler believes is running. A
// cordoned node is skipped by cluster-autoscaler when it picks scale-down candidates: it is never
// drained, never deleted, and paid for forever.
func TestSettleUncordonsTheResidueOfAnOperationThatEnded(t *testing.T) {
	recorder := events.NewFakeRecorder(8)

	node := slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	node.Status.PowerState = infrav1alpha1.PowerStateOn

	kubeNode := cordonedNode()
	kubeNode.Spec.Taints = []corev1.Taint{
		{Key: taints.NodeUnschedulable, Effect: corev1.TaintEffectNoSchedule},
	}

	c := newClient(t, node, kubeNode)
	before := readSlot(t, c)

	outcome := settle(t, c, node, recorder)

	require.True(t, outcome.Uncordoned, "a ready slot that is claimed on and cordoned with nothing running is uncordoned")
	assert.Equal(t, ActionUncordon, outcome.Action)
	assert.Empty(t, outcome.Previous, "the progress of this slot is the truth, so no progress is replaced")

	updated := readKubernetesNode(t, c)
	assert.False(t, updated.Spec.Unschedulable, "the cordon is gone")
	assert.Empty(t, updated.Spec.Taints, "and so is the kubelet's unschedulable taint that goes with it")

	after := readSlot(t, c)
	assert.Equal(t, before.Status, after.Status, "the slot's status is untouched: the cordon was the fact that was wrong")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "so the slot is not even written")

	require.Len(t, recorder.Events, 1)
	event := <-recorder.Events
	assert.Contains(t, event, corev1.EventTypeWarning, "an operation that ended and left its machine cordoned is worth a look")
	assert.Contains(t, event, ReasonCordonReconciled)
	assert.Contains(t, event, "worker-1-k8s", "the event names the node it made schedulable")

	// The pass after it: the node is uncordoned, so the safety net is a no-op, and a resync of a
	// healthy pool writes and records nothing.
	quiet := events.NewFakeRecorder(8)
	again := settle(t, c, readSlot(t, c), quiet)

	assert.Equal(t, ActionNone, again.Action)
	assert.Zero(t, len(quiet.Events), "an uncordoned slot is uncordoned once")
}

// TestSettleLeavesACordonThatMeansSomethingAlone: the safety net is narrow, and every gate it shares
// with the settlement applies. A slot whose node cluster-autoscaler has marked is the autoscaler's
// decision; a slot that is transitioning has an operation running under its lock; a slot whose
// lifecycle Job is in flight is mid-operation, and its cordon is the shutdown doing its work.
func TestSettleLeavesACordonThatMeansSomethingAlone(t *testing.T) {
	tests := []struct {
		name       string
		desired    infrav1alpha1.PowerState
		progress   infrav1alpha1.Progress
		node       *corev1.Node
		lock       bool
		job        *batchv1.Job
		wantReason string
	}{
		{
			name:       "cluster-autoscaler marked the node it is draining",
			desired:    infrav1alpha1.PowerStateOn,
			progress:   infrav1alpha1.ProgressReady,
			node:       markedNode(),
			wantReason: "observation controllers",
		},
		{
			name:       "the slot is shutting down, so the cordon is the operation",
			desired:    infrav1alpha1.PowerStateOff,
			progress:   infrav1alpha1.ProgressShuttingDown,
			node:       cordonedNode(),
			wantReason: "observation controllers",
		},
		{
			name:       "a lifecycle job is in flight",
			desired:    infrav1alpha1.PowerStateOn,
			progress:   infrav1alpha1.ProgressReady,
			node:       cordonedNode(),
			job:        lifecycleJob(fsm.PhaseShutdown, time.Time{}),
			wantReason: "in flight",
		},
		{
			name:       "a coordination lock owns the slot",
			desired:    infrav1alpha1.PowerStateOn,
			progress:   infrav1alpha1.ProgressReady,
			node:       cordonedNode(),
			lock:       true,
			wantReason: "coordination lock",
		},
		{
			name:       "the slot is not wanted on, so its cordon is the shutdown's intent",
			desired:    infrav1alpha1.PowerStateOff,
			progress:   infrav1alpha1.ProgressReady,
			node:       cordonedNode(),
			wantReason: "observation controllers",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)

			node := slot(tc.desired, tc.progress)

			if tc.lock {
				node.Annotations = map[string]string{
					fsm.OperationLockAnnotation: fsm.OperationScaleDown,
					fsm.LockOwnerAnnotation:     fsm.NodeControllerOwner,
					fsm.LockTimestampAnnotation: time.Now().Format(time.RFC3339),
					fsm.LockTimeoutAnnotation:   "300",
				}
			}

			objects := []client.Object{node, tc.node}
			if tc.job != nil {
				objects = append(objects, tc.job)
			}

			c := newClient(t, objects...)
			beforeNode := readKubernetesNode(t, c)
			beforeSlot := readSlot(t, c)

			outcome := settle(t, c, node, recorder)

			assert.Equal(t, ActionNone, outcome.Action)
			assert.Contains(t, outcome.Reason, tc.wantReason)

			assert.Equal(t, beforeNode.Spec.Unschedulable, readKubernetesNode(t, c).Spec.Unschedulable,
				"the node keeps the cordon that means something")
			assert.Equal(t, beforeSlot.ResourceVersion, readSlot(t, c).ResourceVersion, "and the slot is not written")
			assert.Zero(t, len(recorder.Events))
		})
	}
}

// TestSettleGraceIsMeasuredFromTheStartupStampWhenNoJobObjectIsGone closes the gap the Job gate
// leaves. The grace is measured from the lifecycle Job, and a slot very often has no Job object to
// measure: a startup Job whose spec.ttlSecondsAfterFinished is short is collected minutes after it
// finished, and its after_JobCompleted hook - registration wait, readiness gate - can be minutes
// from writing the progress that hook owns. With no Job the gate opened immediately, so the watchdog
// settled the slot mid-hook and the hook wrote ready on top of it: two writers, one slot, a
// progress that named a machine for a second time, and the duplicate scale-up the grace exists to
// prevent. status.lastStartupTime is the stamp the FSM's own progress write leaves on the transition
// into startingup, so it carries the window across the gap.
func TestSettleGraceIsMeasuredFromTheStartupStampWhenNoJobObjectIsGone(t *testing.T) {
	t.Run("a startup stamped inside the grace is left alone", func(t *testing.T) {
		for _, desired := range []infrav1alpha1.PowerState{infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn} {
			t.Run(string(desired), func(t *testing.T) {
				recorder := events.NewFakeRecorder(8)

				node := slot(desired, infrav1alpha1.ProgressStartingUp)
				node.Status.PowerState = infrav1alpha1.PowerStateOn
				stamp := metav1.NewTime(time.Now().Add(-time.Minute))
				node.Status.LastStartupTime = &stamp

				c := newClient(t, node)
				before := readSlot(t, c)

				outcome := settle(t, c, node, recorder)

				assert.Equal(t, ActionNone, outcome.Action,
					"a startup younger than the grace is an operation whose writer may still be running, with or without a Job object")
				assert.Greater(t, outcome.RequeueAfter, time.Duration(0), "and the look is scheduled, not forgotten")
				assert.Less(t, outcome.RequeueAfter, SettleGracePeriod, "on the rest of the grace, not on a fresh one")
				assert.Contains(t, outcome.Reason, "lastStartupTime", "the log says what the window was measured from")

				assert.Equal(t, before.ResourceVersion, readSlot(t, c).ResourceVersion, "nothing is written")
				assert.Zero(t, len(recorder.Events), "and nothing is recorded")
			})
		}
	})

	t.Run("a startup stamped past the grace is the shape this package acts on", func(t *testing.T) {
		recorder := events.NewFakeRecorder(8)

		node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
		stamp := metav1.NewTime(time.Now().Add(-SettleGracePeriod - time.Minute))
		node.Status.LastStartupTime = &stamp

		c := newClient(t, node)
		outcome := settle(t, c, node, recorder)

		assert.True(t, outcome.Settled, "the slot whose startup ran out of its grace is settled: the stamp is a floor, not a freeze")
		assert.Equal(t, infrav1alpha1.ProgressShutdown, readSlot(t, c).Status.Progress)
	})

	t.Run("a slot with no startup stamp is not held by a deadline it has no evidence for", func(t *testing.T) {
		recorder := events.NewFakeRecorder(8)

		node := slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
		node.Status.PowerState = infrav1alpha1.PowerStateOff

		c := newClient(t, node)
		outcome := settle(t, c, node, recorder)

		assert.True(t, outcome.Rearmed, "a slot that never stamped a startup has no window to wait out")
		assert.Zero(t, outcome.RequeueAfter)
	})

	t.Run("the Job object stays the evidence when there is one", func(t *testing.T) {
		recorder := events.NewFakeRecorder(8)

		node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
		stamp := metav1.NewTime(time.Now().Add(-SettleGracePeriod - time.Minute))
		node.Status.LastStartupTime = &stamp

		// A Job that finished 30s ago: the stamp is older than the grace, and the Job is not. The
		// tightest deadline wins, and the Job is the one that says a writer is still running.
		c := newClient(t, node, lifecycleJob(fsm.PhaseStartup, time.Now().Add(-30*time.Second)))
		outcome := settle(t, c, node, recorder)

		assert.Equal(t, ActionNone, outcome.Action)
		assert.Greater(t, outcome.RequeueAfter, time.Duration(0))
		assert.Contains(t, outcome.Reason, "settle grace")
	})
}

// TestSettleSteadyStateWritesNothingAndRecordsNothing is the idempotency rule as one table: every
// shape of a pool at rest is passed over with no status write and no event, on every resync. It
// includes the two states the bound leaves behind - a slot that was re-armed (so its progress is the
// stable shutdown, and the machine of the attempt is on its way), and a slot whose startup attempts
// are spent (settled, noted, and reported once) - because the passes after an action are where an
// event storm is born.
func TestSettleSteadyStateWritesNothingAndRecordsNothing(t *testing.T) {
	tests := []struct {
		name     string
		desired  infrav1alpha1.PowerState
		progress infrav1alpha1.Progress
		attempts int
		object   client.Object
	}{
		{name: "no claim, already shutdown", desired: infrav1alpha1.PowerStateOff, progress: infrav1alpha1.ProgressShutdown},
		{name: "no claim, never seeded", desired: infrav1alpha1.PowerStateOff, progress: ""},
		{name: "claimed, shutdown: the re-arm's own steady state", desired: infrav1alpha1.PowerStateOn, progress: infrav1alpha1.ProgressShutdown},
		{
			name:     "claimed, shutdown, attempts spent: the bound's steady state",
			desired:  infrav1alpha1.PowerStateOn,
			progress: infrav1alpha1.ProgressShutdown,
			attempts: ClaimRearmLimit,
		},
		{
			name:     "claimed, ready, with a machine behind it",
			desired:  infrav1alpha1.PowerStateOn,
			progress: infrav1alpha1.ProgressReady,
			object:   liveNode(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := slot(tc.desired, tc.progress)
			node.Status.PowerState = infrav1alpha1.PowerStateOff

			objects := []client.Object{node}
			if tc.object != nil {
				objects = append(objects, tc.object)
			}

			c := newClient(t, objects...)

			if tc.attempts > 0 {
				stampAttempts(t, c, tc.attempts)
			}

			if tc.attempts >= ClaimRearmLimit {
				// The settled-and-noted slot is what the bound's own pass leaves behind.
				current := readSlot(t, c)
				fsm.StampStartupAttemptLimit(current, tc.attempts, settleLimitCause(current))
				require.NoError(t, c.Status().Update(context.Background(), current))
			}

			before := readSlot(t, c)

			for range 3 {
				recorder := events.NewFakeRecorder(8)

				outcome := settle(t, c, readSlot(t, c), recorder)

				assert.Equal(t, ActionNone, outcome.Action)
				assert.Zero(t, outcome.RequeueAfter, "nothing in flight, so nothing scheduled")
				assert.NotEmpty(t, outcome.Reason)
				assert.Zero(t, len(recorder.Events), "a pool at rest records nothing")
			}

			assert.Equal(t, before.ResourceVersion, readSlot(t, c).ResourceVersion, "and writes nothing")
		})
	}
}
