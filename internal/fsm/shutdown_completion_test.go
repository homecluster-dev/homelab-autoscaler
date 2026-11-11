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

package fsm

import (
	"context"
	"testing"
	"time"

	"github.com/looplab/fsm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
)

// shutdownCompletionSlot is a slot in the middle of a scale-down: claimed off, the machine it is
// being shut down observed up (status.powerState on), and the transitional progress of the shutdown
// operation whose Job is about to complete.
//
// stamp is the shutdown's own stamp - status.lastShutdownTime, written by enterShuttingDown in the
// same status write as progress: shuttingdown, so at the *start* of the operation. The tests pass
// both shapes on purpose: a shutdown that has been under way for ten minutes, and the fast script
// that finishes in seconds, which is the one the completion window used to misread.
func shutdownCompletionSlot(stamp time.Time) *infrav2alpha1.Node {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseDraining)
	node.Status.PowerState = infrav2alpha1.PowerStateOn

	stamped := metav1.NewTime(stamp)
	node.Status.LastShutdownTime = &stamped

	return node
}

// kubeNodeWithHeartbeat is the corev1.Node of the slot's machine, Ready, with the Ready condition's
// last heartbeat at beat. A heartbeat after the bound the shutdown leaves is a kubelet checking in
// now; one before it is the node object of a machine that has just been powered off, which stays Ready
// for node-monitor-grace-period.
func kubeNodeWithHeartbeat(t *testing.T, beat time.Time, ready corev1.ConditionStatus) *corev1.Node {
	t.Helper()

	kubeNode := CreateTestKubernetesNode("test-node-k8s", false)
	kubeNode.Status.Conditions[0].Status = ready
	kubeNode.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(beat)

	return kubeNode
}

// completedShutdownJob is the shutdown Job the lifecycle Job monitor hands the completion as the
// event's argument, finished at completedAt. Its status.completionTime is the bound the guard
// measures the kubelet's heartbeat against, and it is the reason the guard does not measure against
// status.lastShutdownTime: the stamp is written when the shutdown *starts*, the completion time when
// the script finishes, and a fast script closes that gap in seconds.
// shutdownJobName is the lifecycle Job every completion in this file carries: the refusal quotes
// it, and a test that asserts the note names it reads the same string.
const shutdownJobName = "test-node-shutdown-1700000000"

func completedShutdownJob(completedAt time.Time) *batchv1.Job {
	job := CreateTestJob(shutdownJobName, config.NewNamespaceConfig().Get(), PhaseShutdown)

	completed := metav1.NewTime(completedAt)
	job.Status.StartTime = &completed
	job.Status.CompletionTime = &completed
	job.Status.Succeeded = 1

	return job
}

// completionHook is the slow-shutdown shape: the shutdown has been under way for ten minutes, and the
// Job that ran is not in the cluster (its object was collected), so the guard falls back to the
// stamp + shutdownObservationFreshness window.
func completionHook(t *testing.T, objects ...client.Object) (*NodeStateMachine, *events.FakeRecorder, *fsm.Event) {
	t.Helper()

	return completionHookAt(t, time.Now().Add(-10*time.Minute), nil, objects...)
}

// completionHookAt builds the state machine, its recorder, and the JobCompleted transition of the
// shutdown Job, with the machine primed into StateShutdown the way looplab/fsm primes it before the
// enter_ hook of the destination runs.
//
// stamp is the slot's status.lastShutdownTime, jobCompletedAt the shutdown Job's
// status.completionTime (nil for a Job with no completion time, and job == nil for the shape a short
// spec.ttlSecondsAfterFinished leaves: no Job object to read at all).
func completionHookAt(
	t *testing.T,
	stamp time.Time,
	job *batchv1.Job,
	objects ...client.Object,
) (*NodeStateMachine, *events.FakeRecorder, *fsm.Event) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := shutdownCompletionSlot(stamp)
	objects = append([]client.Object{node}, objects...)

	fakeClient := CreateFakeClient(scheme, objects...)

	rec := events.NewFakeRecorder(16)
	nm := NewNodeStateMachine(node, fakeClient, scheme, NewMockCoordinationManager(), WithEventRecorder(rec))
	nm.PrimeEnterState(StateShutdown)

	return nm, rec, newShutdownCompletedEvent(nm, job)
}

// newShutdownCompletedEvent is the transition the lifecycle Job monitor fires when its Job reports
// success: JobCompleted, shuttingdown -> shutdown, carrying the Job it read. The Job in the arguments
// is what the guard measures against and what the refusal quotes.
func newShutdownCompletedEvent(nm *NodeStateMachine, job *batchv1.Job) *fsm.Event {
	if job == nil {
		job = CreateTestJob(shutdownJobName, config.NewNamespaceConfig().Get(), PhaseShutdown)
	}

	return &fsm.Event{
		FSM:   nm.fsm,
		Event: EventJobCompleted,
		Src:   StateShuttingDown,
		Dst:   StateShutdown,
		Args:  []interface{}{job},
	}
}

// shutdownCondition returns the slot's Shutdown status.conditions entry, and nil when it has none:
// a test that asserts a condition was not stamped cannot use FindStatusCondition's zero value and
// stay readable. The condition is the one that says "Node is shutdown and powered off", so the
// refusal of a shutdown the observation contradicts is what its absence pins.
func shutdownCondition(node *infrav2alpha1.Node) *metav1.Condition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == "Shutdown" {
			return &node.Status.Conditions[i]
		}
	}

	return nil
}

// TestEnterShutdownDoesNotRecordAShutdownTheObservationContradicts is the shutdown-completion audit
// as a failing test: the shutdown Job of a slot exits 0 - the script the operator supplies is the
// only evidence the completion has, and a script that exits 0 without the provider's power-off
// landing is the cause of the live slot - while the machine is observed up (status.powerState on,
// its node object Ready with a heartbeat current after the shutdown started).
//
// The completion must NOT be recorded as a shutdown. The pair {powerState: on, progress: shutdown}
// is the state the FSM reads as StateShutdown, from which ShutdownNode is unreachable, which is what
// made the live slot unhealable for 40 minutes and what left an operator patching status.phase by
// hand. The slot is recorded at the progress the observation says, ready, so the claim that is
// already off reaches ShutdownNode again on the next pass, and the Warning says which Job claimed
// the machine was off.
func TestEnterShutdownDoesNotRecordAShutdownTheObservationContradicts(t *testing.T) {
	nm, rec, event := completionHook(t, kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue))

	nm.enterShutdown(context.TODO(), event)

	updated := &infrav2alpha1.Node{}
	require.NoError(t, nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), updated))

	assert.Equal(t, infrav2alpha1.NodePhaseDraining, updated.Status.Phase,
		"the shutdown is not recorded while the machine is observed up: the report says draining")
	assert.Equal(t, infrav2alpha1.PowerStateOn, updated.Status.PowerState, "the observation is left to the observation controllers")
	assert.Equal(t, StateReady, nm.GetCurrentState(), "the machine is moved to the state the observation says, so its own writer agrees")

	// The Shutdown condition says "Node is shutdown and powered off": it is not stamped on a slot
	// whose machine is up.
	assert.Nil(t, shutdownCondition(updated), "no condition may claim a machine that is up is off")

	recorded := drainEvents(rec)
	require.Len(t, recorded, 1, "one Warning, and not the ScaleDownCompleted the slot did not earn: %v", recorded)
	assert.Contains(t, recorded[0], "Warning "+ReasonShutdownNotObserved)
	assert.Contains(t, recorded[0], "test-node-shutdown-1700000000", "the event names the Job that claimed the machine was off")
	assert.Contains(t, recorded[0], "test-node-k8s", "the event names the machine that is up")
	assert.NotContains(t, recorded, "Normal "+ReasonScaleDownCompleted)
}

// TestEnterShutdownRefusesAFastShutdownScript is the pair the completion window used to get backwards,
// and the incident's exact shape. status.lastShutdownTime is stamped at the *start* of the shutdown,
// and the Job monitor notices the finished Job at most one JobMonitorPollInterval (30s) later, so a
// shutdown script that exits 0 fast is completed ~30s after the stamp. A machine that is genuinely up
// has heartbeated in exactly that interval, and the window the guard used to measure -
// lastShutdownTime + shutdownObservationFreshness (60s) - rejects that heartbeat: the live machine was
// accepted as "shut down", the slot was recorded off, and the pair was on its way to the 40-minute
// stuck shape this PR is about.
//
// The bound that separates the two shapes is the shutdown Job's own status.completionTime, which the
// monitor hands this hook in the event's arguments: a machine that died during the script stops
// heartbeating before the Job finishes, a machine the script did not power off keeps heartbeating after
// it. Both subtests share the same stamp (30s ago) and the same Job completion (5s ago), and differ
// only in where the heartbeat falls relative to the completion - which is the whole discriminator.
func TestEnterShutdownRefusesAFastShutdownScript(t *testing.T) {
	shutdownStarted := time.Now().Add(-30 * time.Second)
	jobCompleted := time.Now().Add(-5 * time.Second)

	t.Run("heartbeat now, Job completed 5s ago: the machine is up, the completion is refused", func(t *testing.T) {
		nm, rec, event := completionHookAt(t, shutdownStarted,
			completedShutdownJob(jobCompleted),
			kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue))

		nm.enterShutdown(context.TODO(), event)

		updated := &infrav2alpha1.Node{}
		require.NoError(t, nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), updated))

		assert.Equal(t, infrav2alpha1.NodePhaseDraining, updated.Status.Phase,
			"a heartbeat newer than the shutdown Job's completion is a kubelet alive after the script finished")
		assert.Equal(t, StateReady, nm.GetCurrentState(), "the machine's own state moves with the write")
		assert.Nil(t, shutdownCondition(updated), "no condition may claim a machine that is up is off")

		recorded := drainEvents(rec)
		require.Len(t, recorded, 1, "%v", recorded)
		assert.Contains(t, recorded[0], "Warning "+ReasonShutdownNotObserved,
			"the refusal is what an operator sees, and it names the Job that claimed the machine was off")
		assert.Contains(t, recorded[0], "test-node-shutdown-1700000000")
		assert.NotContains(t, recorded, "Normal "+ReasonScaleDownCompleted)
	})

	t.Run("heartbeat inside the script, Job completed 5s ago: the machine is off, shutdown is recorded", func(t *testing.T) {
		// The heartbeat is 5s after the shutdown's stamp - inside the old freshness window, and before
		// the Job finished. It is the residue of a machine that died while the script ran, and the
		// completion is recorded exactly as every healthy scale-down has always recorded it.
		nm, rec, event := completionHookAt(t, shutdownStarted,
			completedShutdownJob(jobCompleted),
			kubeNodeWithHeartbeat(t, shutdownStarted.Add(5*time.Second), corev1.ConditionTrue))

		assertCompletionRecorded(t, nm, rec, event)
	})
}

// TestAfterJobCompletedDoesNotWriteTheShutdownTheEnterHookRefused: looplab/fsm runs the enter_ hook of
// the destination before the after_ hook, and both write the progress of a JobCompleted transition.
// A completion the observation refused must not be written by the second hook: two hooks of one
// completion, one slot.
//
// The hooks pass Progress(e.Dst) - the progress of the transition they close - and that is what makes
// updateNodeProgress's guard ("refuse a progress the state machine is not in") a check: enterShutdown
// refused the completion, moved the machine to ready and wrote ready, so the after_ hook's write of
// shutdown is refused and the slot is written exactly once. Passing nm.GetCurrentState() from these
// hooks asks the guard a question whose answer it reads from the same field, so it can never fire and
// the refused completion is written twice.
func TestAfterJobCompletedDoesNotWriteTheShutdownTheEnterHookRefused(t *testing.T) {
	nm, rec, event := completionHook(t, kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue))

	nm.enterShutdown(context.TODO(), event)

	written := &infrav2alpha1.Node{}
	require.NoError(t, nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), written))
	require.Equal(t, infrav2alpha1.NodePhaseDraining, written.Status.Phase)

	nm.afterJobCompleted(context.TODO(), event)

	updated := &infrav2alpha1.Node{}
	require.NoError(t, nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), updated))

	assert.Equal(t, infrav2alpha1.NodePhaseDraining, updated.Status.Phase,
		"the after_ hook writes the report of the machine it is in, and the guard refuses the write of "+
			"a machine the inputs say is up")
	assert.Equal(t, written.ResourceVersion, updated.ResourceVersion,
		"the refused completion is one status write, not two")
	assert.Equal(t, StateReady, nm.GetCurrentState())
	assert.Len(t, drainEvents(rec), 1, "the refusal is recorded once")
}

// TestEnterShutdownRecordsTheCompletionTheObservationAllows keeps the healthy path exactly as it was:
// a machine the cluster does not observe up is recorded shut down, with its condition and its
// completion event. The shapes are the ones every scale-down lands on, and none of them may pay a
// second shutdown Job:
//
//   - the node object is gone (a provider that deletes the server takes it down);
//   - the node is Ready with a heartbeat inside the freshness window: the ~40s in which a powered-off
//     machine's node object is still Ready, which is why the guard reads the heartbeat and not the
//     condition. Measured against the shutdown Job's completion when there is one (see
//     TestEnterShutdownRefusesAFastShutdownScript), and against lastShutdownTime +
//     shutdownObservationFreshness when the Job object is already gone;
//   - the node is NotReady.
func TestEnterShutdownRecordsTheCompletionTheObservationAllows(t *testing.T) {
	t.Run("no node object", func(t *testing.T) {
		nm, rec, event := completionHook(t)

		assertCompletionRecorded(t, nm, rec, event)
	})

	t.Run("Ready with a heartbeat inside the freshness window", func(t *testing.T) {
		// The shutdown started ten minutes ago and the heartbeat is 5s after that stamp: inside the
		// fallback window, and no Job object is left to measure against. A machine that died when the
		// script ran leaves exactly this residue - a node object that is still Ready because
		// node-monitor-grace-period has not elapsed - and recording it as up would fire a second
		// shutdown Job on every scale-down whose Job was collected.
		stamp := time.Now().Add(-10 * time.Minute)
		nm, rec, event := completionHookAt(t, stamp, nil,
			kubeNodeWithHeartbeat(t, stamp.Add(5*time.Second), corev1.ConditionTrue))

		assertCompletionRecorded(t, nm, rec, event)
	})

	t.Run("NotReady node object", func(t *testing.T) {
		nm, rec, event := completionHook(t, kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionFalse))

		assertCompletionRecorded(t, nm, rec, event)
	})
}

// assertCompletionRecorded is the shape a shutdown completion has always had, and must keep: the
// progress is shutdown, the condition is stamped, one Normal completion event, and no refusal.
func assertCompletionRecorded(t *testing.T, nm *NodeStateMachine, rec *events.FakeRecorder, event *fsm.Event) {
	t.Helper()

	nm.enterShutdown(context.TODO(), event)

	updated := &infrav2alpha1.Node{}
	require.NoError(t, nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), updated))

	assert.Equal(t, infrav2alpha1.NodePhaseDraining, updated.Status.Phase,
		"the report is derived from the observation, which is still on in the window the guard answers for")
	assert.Equal(t, StateShutdown, nm.GetCurrentState())
	require.NotNil(t, shutdownCondition(updated), "the shutdown condition is stamped as it always was")

	recorded := drainEvents(rec)
	require.Len(t, recorded, 1, "%v", recorded)
	assert.Contains(t, recorded[0], "Normal "+ReasonScaleDownCompleted)
	assert.NotContains(t, recorded[0], ReasonShutdownNotObserved)
}

// TestKubeletIsLive is the discriminator of the guard, on its own: the node has to report Ready, carry
// a heartbeat, and - when the caller has a bound to measure against - have heartbeated after it. The
// bound is what the shutdown leaves: the Job's completion time, or lastShutdownTime +
// shutdownObservationFreshness with no Job to read, or nothing at all.
func TestKubeletIsLive(t *testing.T) {
	stamp := time.Now().Add(-10 * time.Minute)
	fallback := stamp.Add(shutdownObservationFreshness)

	tests := []struct {
		name string
		node *corev1.Node
		// observedAfter is the bound shutdownObservationBound produced
		observedAfter time.Time
		want          bool
	}{
		{
			name:          "Ready, current heartbeat, bound 10 minutes in the past",
			node:          kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue),
			observedAfter: stamp,
			want:          true,
		},
		{
			name:          "heartbeat 5s after the shutdown stamp, fallback window 60s: the node of a machine that just went off",
			node:          kubeNodeWithHeartbeat(t, stamp.Add(5*time.Second), corev1.ConditionTrue),
			observedAfter: fallback,
			want:          false,
		},
		{
			name:          "heartbeat 90s after the shutdown stamp: past the fallback window, past node-monitor-grace-period",
			node:          kubeNodeWithHeartbeat(t, stamp.Add(90*time.Second), corev1.ConditionTrue),
			observedAfter: fallback,
			want:          true,
		},
		{
			name:          "heartbeat 5s after the Job completed: the machine is alive after the script finished",
			node:          kubeNodeWithHeartbeat(t, time.Now().Add(-5*time.Second), corev1.ConditionTrue),
			observedAfter: time.Now().Add(-10 * time.Second),
			want:          true,
		},
		{
			name:          "heartbeat before the Job completed: the machine stopped during the script",
			node:          kubeNodeWithHeartbeat(t, time.Now().Add(-30*time.Second), corev1.ConditionTrue),
			observedAfter: time.Now().Add(-10 * time.Second),
			want:          false,
		},
		{
			name:          "no bound: the progress has no operation behind it, the heartbeat is all there is",
			node:          kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue),
			observedAfter: time.Time{},
			want:          true,
		},
		{
			name:          "NotReady",
			node:          kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionFalse),
			observedAfter: stamp,
			want:          false,
		},
		{
			name:          "no heartbeat stamped: not positive evidence",
			node:          CreateTestKubernetesNode("test-node-k8s", false),
			observedAfter: stamp,
			want:          false,
		},
		{
			name:          "no Ready condition at all",
			node:          &corev1.Node{Status: corev1.NodeStatus{}},
			observedAfter: stamp,
			want:          false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, kubeletIsLive(tc.node, tc.observedAfter))
		})
	}
}

// TestShutdownObservationBound pins which evidence the guard measures the heartbeat against, in the
// order the evidence is worth trusting: the shutdown Job's completion time, status.lastShutdownTime
// plus the freshness window when the Job object is gone (a short spec.ttlSecondsAfterFinished, the
// shape with no better timestamp available), and nothing at all when the slot has neither.
func TestShutdownObservationBound(t *testing.T) {
	stamp := time.Now().Add(-30 * time.Second)
	completed := time.Now().Add(-5 * time.Second)

	node := shutdownCompletionSlot(stamp)

	t.Run("the Job's completion time wins, with no freshness window added", func(t *testing.T) {
		bound, note := shutdownObservationBound(node, completedShutdownJob(completed))

		assert.True(t, bound.Equal(completed), "the bound is the completion time itself: %s", bound)
		assert.Contains(t, note, "test-node-shutdown-1700000000", "the note names the evidence the refusal read")
	})

	t.Run("no Job object: lastShutdownTime plus the freshness window", func(t *testing.T) {
		bound, note := shutdownObservationBound(node, nil)

		assert.True(t, bound.Equal(stamp.Add(shutdownObservationFreshness)), "bound=%s", bound)
		assert.Contains(t, note, "lastShutdownTime")
	})

	t.Run("a Job with no completion time: the same fallback", func(t *testing.T) {
		job := CreateTestJob(shutdownJobName, config.NewNamespaceConfig().Get(), PhaseShutdown)

		bound, note := shutdownObservationBound(node, job)

		assert.True(t, bound.Equal(stamp.Add(shutdownObservationFreshness)), "bound=%s", bound)
		assert.Contains(t, note, "lastShutdownTime")
	})

	t.Run("a completed Job beats a shutdown stamp that is older", func(t *testing.T) {
		// The pair the incident produced: the stamp is 10 minutes old, so the fallback window has long
		// expired and any heartbeat would be taken as "up" - and the Job, which says the script only
		// finished 5s ago, is the evidence that separates a live machine from a corpse.
		aged := shutdownCompletionSlot(time.Now().Add(-10 * time.Minute))

		bound, _ := shutdownObservationBound(aged, completedShutdownJob(completed))

		assert.True(t, bound.Equal(completed), "bound=%s", bound)
	})

	t.Run("no stamp and no Job: no bound", func(t *testing.T) {
		slot := shutdownCompletionSlot(stamp)
		slot.Status.LastShutdownTime = nil

		bound, note := shutdownObservationBound(slot, nil)

		assert.True(t, bound.IsZero())
		assert.Contains(t, note, "no shutdown stamp")
	})
}

// TestShutdownCompletionObservations documents the half of the guard that is not about the node
// object: a slot whose recorded power state is off, and a slot that has no Kubernetes node name, are
// never "observed up", so a completion is recorded normally for them.
func TestShutdownCompletionObservations(t *testing.T) {
	t.Run("powerState off is not an observed on", func(t *testing.T) {
		nm, _, _ := completionHook(t, kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue))
		nm.node.Status.PowerState = infrav2alpha1.PowerStateOff

		assert.False(t, nm.machineObservedUp(context.TODO(), nil))
	})

	t.Run("a slot with no Kubernetes node name has no machine to observe", func(t *testing.T) {
		nm, _, _ := completionHook(t)
		nm.node.Spec.KubernetesNodeName = ""

		assert.False(t, nm.machineObservedUp(context.TODO(), nil))
	})

	t.Run("a node object that cannot be read is not evidence", func(t *testing.T) {
		nm, _, _ := completionHook(t)

		assert.False(t, nm.machineObservedUp(context.TODO(), nil), "the fake client has no corev1.Node for the slot")
	})
}
