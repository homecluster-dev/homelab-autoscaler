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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
)

// shutdownCompletionSlot is a slot in the middle of a scale-down: claimed off, the machine it is
// being shut down observed up (status.powerState on), and the transitional progress of the shutdown
// operation whose Job is about to complete.
func shutdownCompletionSlot() *infrav1alpha1.Node {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
	node.Status.PowerState = infrav1alpha1.PowerStateOn

	// The stamp the transition into shuttingdown leaves, well before the freshness window: the
	// shutdown has been under way for ten minutes.
	stamp := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	node.Status.LastShutdownTime = &stamp

	return node
}

// kubeNodeWithHeartbeat is the corev1.Node of the slot's machine, Ready, with the Ready condition's
// last heartbeat at beat. A heartbeat after the shutdown's stamp is a kubelet checking in now; one
// before it is the node object of a machine that has just been powered off, which stays Ready for
// node-monitor-grace-period.
func kubeNodeWithHeartbeat(t *testing.T, beat time.Time, ready corev1.ConditionStatus) *corev1.Node {
	t.Helper()

	kubeNode := CreateTestKubernetesNode("test-node-k8s", false)
	kubeNode.Status.Conditions[0].Status = ready
	kubeNode.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(beat)

	return kubeNode
}

// completionHook builds the state machine, its recorder, and the JobCompleted transition of the
// shutdown Job, with the machine primed into StateShutdown the way looplab/fsm primes it before the
// enter_ hook of the destination runs.
func completionHook(t *testing.T, objects ...client.Object) (*NodeStateMachine, *events.FakeRecorder, *fsm.Event) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	node := shutdownCompletionSlot()
	objects = append([]client.Object{node}, objects...)

	fakeClient := CreateFakeClient(scheme, objects...)

	rec := events.NewFakeRecorder(16)
	nm := NewNodeStateMachine(node, fakeClient, scheme, NewMockCoordinationManager(), WithEventRecorder(rec))
	nm.PrimeEnterState(StateShutdown)

	return nm, rec, newJobCompletedEvent(nm, StateShuttingDown, StateShutdown, "test-node-shutdown-1700000000")
}

// newJobCompletedEvent is the transition the lifecycle Job monitor fires when its Job reports
// success: JobCompleted, from the transitional state into the stable one, carrying the Job. The
// monitor passes the Job it read as the event's argument, which is what the refusal quotes.
func newJobCompletedEvent(nm *NodeStateMachine, src, dst, jobName string) *fsm.Event {
	return &fsm.Event{
		FSM:   nm.fsm,
		Event: EventJobCompleted,
		Src:   src,
		Dst:   dst,
		Args:  []interface{}{CreateTestJob(jobName, config.NewNamespaceConfig().Get(), PhaseShutdown)},
	}
}

// findCondition returns the status.conditions entry of the given type, and nil when the slot has
// none: a test that asserts a condition was not stamped cannot use FindStatusCondition's zero value
// and stay readable.
func findCondition(node *infrav1alpha1.Node, condType string) *metav1.Condition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == condType {
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
// made the live slot unhealable for 40 minutes and what left an operator patching status.progress by
// hand. The slot is recorded at the progress the observation says, ready, so the claim that is
// already off reaches ShutdownNode again on the next pass, and the Warning says which Job claimed
// the machine was off.
func TestEnterShutdownDoesNotRecordAShutdownTheObservationContradicts(t *testing.T) {
	nm, rec, event := completionHook(t, kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue))

	nm.enterShutdown(context.TODO(), event)

	updated := &infrav1alpha1.Node{}
	require.NoError(t, nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), updated))

	assert.Equal(t, infrav1alpha1.ProgressReady, updated.Status.Progress,
		"the shutdown is not recorded while the machine is observed up")
	assert.Equal(t, infrav1alpha1.PowerStateOn, updated.Status.PowerState, "the observation is left to the observation controllers")
	assert.Equal(t, StateReady, nm.GetCurrentState(), "the machine is moved to the state the observation says, so its own writer agrees")

	// The Shutdown condition says "Node is shutdown and powered off": it is not stamped on a slot
	// whose machine is up.
	assert.Nil(t, findCondition(updated, "Shutdown"), "no condition may claim a machine that is up is off")

	recorded := drainEvents(rec)
	require.Len(t, recorded, 1, "one Warning, and the ScaleDownCompleted the slot did not earn: %v", recorded)
	assert.Contains(t, recorded[0], "Warning "+ReasonShutdownNotObserved)
	assert.Contains(t, recorded[0], "test-node-shutdown-1700000000", "the event names the Job that claimed the machine was off")
	assert.Contains(t, recorded[0], "test-node-k8s", "the event names the machine that is up")
	assert.NotContains(t, recorded, "Normal "+ReasonScaleDownCompleted)
}

// TestAfterJobCompletedDoesNotWriteTheShutdownTheEnterHookRefused: looplab/fsm runs the enter_ hook of
// the destination before the after_ hook, and both write the progress of a JobCompleted transition.
// A completion the observation refused must not be written by the second hook: two hooks of one
// completion, one slot.
func TestAfterJobCompletedDoesNotWriteTheShutdownTheEnterHookRefused(t *testing.T) {
	nm, rec, event := completionHook(t, kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue))

	nm.enterShutdown(context.TODO(), event)
	nm.afterJobCompleted(context.TODO(), event)

	updated := &infrav1alpha1.Node{}
	require.NoError(t, nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), updated))

	assert.Equal(t, infrav1alpha1.ProgressReady, updated.Status.Progress,
		"the after_ hook writes the state the machine is in, and the machine is in ready")
	assert.Equal(t, StateReady, nm.GetCurrentState())
	assert.Len(t, drainEvents(rec), 1, "the refusal is recorded once")
}

// TestEnterShutdownRecordsTheCompletionTheObservationAllows keeps the healthy path exactly as it was:
// a machine the cluster does not observe up is recorded shut down, with its condition and its
// completion event. The three shapes are the ones every scale-down lands on, and none of them may
// pay a second shutdown Job:
//
//   - the node object is gone (a provider that deletes the server takes it down);
//   - the node is Ready with a heartbeat older than the shutdown's stamp: the ~40s window in which a
//     powered-off machine's node object is still Ready, which is why the guard reads the heartbeat
//     and not the condition;
//   - the node is NotReady.
func TestEnterShutdownRecordsTheCompletionTheObservationAllows(t *testing.T) {
	t.Run("no node object", func(t *testing.T) {
		nm, rec, event := completionHook(t)

		assertCompletionRecorded(t, nm, rec, event)
	})

	t.Run("Ready with a heartbeat the shutdown predates", func(t *testing.T) {
		// The shutdown started ten minutes before this heartbeat? No - the heartbeat is 5s after the
		// shutdown's stamp, inside the freshness window: a machine that died when the Job ran leaves
		// exactly this residue.
		stamp := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		nm, rec, event := completionHook(t, kubeNodeWithHeartbeat(t, stamp.Add(5*time.Second), corev1.ConditionTrue))
		nm.node.Status.LastShutdownTime = &stamp

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

	updated := &infrav1alpha1.Node{}
	require.NoError(t, nm.client.Get(context.TODO(), client.ObjectKeyFromObject(nm.node), updated))

	assert.Equal(t, infrav1alpha1.ProgressShutdown, updated.Status.Progress)
	assert.Equal(t, StateShutdown, nm.GetCurrentState())
	require.NotNil(t, findCondition(updated, "Shutdown"), "the shutdown condition is stamped as it always was")

	recorded := drainEvents(rec)
	require.Len(t, recorded, 1, "%v", recorded)
	assert.Contains(t, recorded[0], "Normal "+ReasonScaleDownCompleted)
	assert.NotContains(t, recorded[0], ReasonShutdownNotObserved)
}

// TestKubeletIsLive is the discriminator of the guard, on its own: the node has to report Ready,
// carry a heartbeat, and - when the slot carries the stamp of the shutdown under way - have
// heartbeated more than shutdownObservationFreshness after it.
func TestKubeletIsLive(t *testing.T) {
	stamp := metav1.NewTime(time.Now().Add(-10 * time.Minute))

	tests := []struct {
		name     string
		node     *corev1.Node
		shutdown *metav1.Time
		want     bool
	}{
		{
			name:     "Ready, current heartbeat, shutdown stamped 10 minutes ago",
			node:     kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue),
			shutdown: &stamp,
			want:     true,
		},
		{
			name:     "heartbeat 5s after the shutdown: the node of a machine that just went off",
			node:     kubeNodeWithHeartbeat(t, stamp.Add(5*time.Second), corev1.ConditionTrue),
			shutdown: &stamp,
			want:     false,
		},
		{
			name:     "heartbeat 90s after the shutdown: past the node-monitor-grace window",
			node:     kubeNodeWithHeartbeat(t, stamp.Add(90*time.Second), corev1.ConditionTrue),
			shutdown: &stamp,
			want:     true,
		},
		{
			name:     "no shutdown stamp: the progress has no operation behind it, the heartbeat is all there is",
			node:     kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue),
			shutdown: nil,
			want:     true,
		},
		{
			name:     "NotReady",
			node:     kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionFalse),
			shutdown: &stamp,
			want:     false,
		},
		{
			name:     "no heartbeat stamped: not positive evidence",
			node:     CreateTestKubernetesNode("test-node-k8s", false),
			shutdown: &stamp,
			want:     false,
		},
		{
			name:     "no Ready condition at all",
			node:     &corev1.Node{Status: corev1.NodeStatus{}},
			shutdown: &stamp,
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, kubeletIsLive(tc.node, tc.shutdown))
		})
	}
}

// TestShutdownCompletionObservations documents the half of the guard that is not about the node
// object: a slot whose recorded power state is off, and a slot that has no Kubernetes node name, are
// never "observed up", so a completion is recorded normally for them.
func TestShutdownCompletionObservations(t *testing.T) {
	t.Run("powerState off is not an observed on", func(t *testing.T) {
		nm, _, _ := completionHook(t, kubeNodeWithHeartbeat(t, time.Now(), corev1.ConditionTrue))
		nm.node.Status.PowerState = infrav1alpha1.PowerStateOff

		assert.False(t, nm.machineObservedUp(context.TODO()))
	})

	t.Run("a slot with no Kubernetes node name has no machine to observe", func(t *testing.T) {
		nm, _, _ := completionHook(t)
		nm.node.Spec.KubernetesNodeName = ""

		assert.False(t, nm.machineObservedUp(context.TODO()))
	})

	t.Run("a node object that cannot be read is not evidence", func(t *testing.T) {
		nm, _, _ := completionHook(t)

		assert.False(t, nm.machineObservedUp(context.TODO()), "the fake client has no corev1.Node for the slot")
	})
}
