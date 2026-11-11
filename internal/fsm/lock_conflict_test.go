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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
)

// The tests in this file drive the collision the live trace showed: cluster-autoscaler asked
// to delete a slot about 4 minutes into its startup, and the slot ended up in
//
//	ScaleDownFailed ... FSM transition failed: transition canceled with error:
//	  coordination lock already exists: operation=scale-up, owner=node-controller, age=4m0s
//	then: "Late transition phase - extended backoff"  progress=shuttingdown
//	      transitionDuration=10m28s  backoffDuration=5m
//
// with its machine and its Job already gone. They use the real DefaultCoordinationManager
// against a fake client, because the lock annotations are the thing under test.

// conflictTestEnv is the wiring the collision tests share: a slot, its Kubernetes node and a
// coordination manager reading and writing the lock annotations of the fake client.
type conflictTestEnv struct {
	scheme     *runtime.Scheme
	fakeClient client.Client
	coordMgr   CoordinationManager
	recorder   *events.FakeRecorder
}

func newConflictTestEnv(t *testing.T, objects ...client.Object) *conflictTestEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	fakeClient := CreateFakeClient(scheme, objects...)

	return &conflictTestEnv{
		scheme:     scheme,
		fakeClient: fakeClient,
		coordMgr:   NewCoordinationManager(fakeClient, NodeControllerOwner),
		recorder:   events.NewFakeRecorder(30),
	}
}

// machine returns a state machine reading the slot through the env's client, primed from the
// slot as it is stored, with the test-sized uncordon timeout the uncordon paths need.
func (e *conflictTestEnv) machine(t *testing.T, node *infrav1alpha1.Node) *NodeStateMachine {
	t.Helper()

	nm := NewNodeStateMachine(node, e.fakeClient, e.scheme, e.coordMgr, WithEventRecorder(e.recorder))
	nm.uncordonTimeout = 100 * time.Millisecond

	return nm
}

// stored reads the slot back from the API, so the assertions are about what is persisted and
// not about the copy a state machine happens to hold.
func (e *conflictTestEnv) stored(t *testing.T, node *infrav1alpha1.Node) *infrav1alpha1.Node {
	t.Helper()

	fresh := &infrav1alpha1.Node{}
	require.NoError(t, e.fakeClient.Get(context.TODO(), client.ObjectKeyFromObject(node), fresh))

	return fresh
}

// heldScaleUpLock takes the coordination lock of a scale-up on the slot, the way the startup
// path does, and returns the slot as it is stored with that lock on it. age is written into
// the lock timestamp: the live trace shows a lock 4m0s old, inside its 5m timeout.
func (e *conflictTestEnv) heldLock(t *testing.T, node *infrav1alpha1.Node, operation string, age time.Duration) *infrav1alpha1.Node {
	t.Helper()

	ctx := context.TODO()
	stored := e.stored(t, node)
	require.NoError(t, e.coordMgr.AcquireLock(ctx, stored, operation, DefaultLockTimeout))

	if age > 0 {
		fresh := e.stored(t, node)
		fresh.Annotations[LockTimestampAnnotation] = time.Now().Add(-age).Format(time.RFC3339)
		require.NoError(t, e.fakeClient.Update(ctx, fresh))
		stored = e.stored(t, node)
	}

	return stored
}

// bootingSlot is the slot of the live trace: desired off because cluster-autoscaler asked for
// it back, the machine up, the progress ready, and the scale-up lock of the startup that
// brought it up still 4 minutes old on the slot.
func bootingSlot(name string) *infrav1alpha1.Node {
	node := CreateTestNode(name, config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady)
	node.Status.PowerState = infrav1alpha1.PowerStateOn

	return node
}

// TestShutdownNodeLockConflictLeavesNoTransitionalProgress is the stranding itself. The
// shutdown arrives while the scale-up owns the slot, loses the acquisition, and must leave the
// slot exactly as it was:
//
//   - status.progress is not left shuttingdown (a). A transitional progress is excluded from
//     the provider's target size, validateStateTransition refuses to start a transition from
//     one, and the slot is only freed by the stuck cleanup at StuckTransitionThreshold, 15
//     minutes later: that is the 10m28s of the trace;
//   - the refusal is reported as a deferral with a deadline, not a terminal cancellation (b);
//   - the lock of the operation in flight is untouched, and the shutdown performs none of its
//     side effects on a slot it does not own (c);
//   - exactly one ScaleDownFailed warning is recorded, and no ScaleDownRequested, because the
//     transition never happened (d).
func TestShutdownNodeLockConflictLeavesNoTransitionalProgress(t *testing.T) {
	node := bootingSlot("test-node")
	startupJob := CreateTestJobWithNodeLabel("test-node-startup-1", node.Namespace, PhaseStartup, "test-node-k8s")
	kubeNode := CreateTestKubernetesNode("test-node-k8s", false)

	env := newConflictTestEnv(t, node, startupJob, kubeNode)
	booting := env.heldLock(t, node, OperationScaleUp, 4*time.Minute)

	nm := env.machine(t, booting)
	require.Equal(t, StateReady, nm.GetCurrentState())

	err := nm.ShutdownNode()

	// (b) the refusal is a lock conflict with a deadline, not a terminal failure
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FSM transition failed")
	conflict := AsLockConflict(err)
	require.NotNil(t, conflict, "a lost acquisition must be reported as a lock conflict: %v", err)
	assert.True(t, IsLockConflict(err))
	assert.Equal(t, OperationScaleDown, conflict.Operation, "the scale-down is the operation that was refused")
	assert.Equal(t, OperationScaleUp, conflict.HolderOperation())
	assert.Equal(t, NodeControllerOwner, conflict.HolderOwner())
	assert.Greater(t, conflict.HolderAge(), 3*time.Minute)
	assert.Greater(t, conflict.RetryAfter(), time.Duration(0))
	assert.LessOrEqual(t, conflict.RetryAfter(), DefaultLockTimeout)

	// (a) nothing transitional was written, in the cluster or in the machine
	stored := env.stored(t, node)
	assert.Equal(t, infrav1alpha1.ProgressReady, stored.Status.Progress,
		"a cancelled transition must not leave the slot in a transitional progress")
	assert.Equal(t, StateReady, nm.GetCurrentState())
	assert.Nil(t, nm.backoff, "a transition that never ran has no backoff to track")

	// (c) the slot is left to the operation that owns it: no shutdown Job, the startup Job of
	// the scale-up is not deleted, the node is not cordoned, and the lock is intact
	jobs := &batchv1.JobList{}
	require.NoError(t, env.fakeClient.List(context.TODO(), jobs))
	require.Len(t, jobs.Items, 1, "a refused shutdown must not create a job, and must not delete the startup job")
	assert.Equal(t, startupJob.Name, jobs.Items[0].Name)

	storedKubeNode := &corev1.Node{}
	require.NoError(t, env.fakeClient.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, storedKubeNode))
	assert.False(t, storedKubeNode.Spec.Unschedulable, "a refused shutdown must not cordon a node it does not own")

	lock, exists := env.coordMgr.CheckLock(context.TODO(), stored)
	require.True(t, exists, "the refused operation must not touch the lock of the operation in flight")
	assert.Equal(t, OperationScaleUp, lock.Operation)

	// (d) one warning, exactly as the operators read it, and no request event
	recorded := drainEvents(env.recorder)
	failed := assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 1)
	assert.Contains(t, failed[0], "coordination lock already exists")
	assert.Contains(t, failed[0], "operation=scale-up")
	assert.Contains(t, failed[0], "owner=node-controller")
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownRequested, 0)
	assert.Len(t, recorded, 1, "a refused shutdown records exactly one event: %v", recorded)
}

// TestShutdownNodeCompletesAfterTheScaleUpReleasesTheLock is the second half of the live
// trace: the shutdown that collided with the startup has to be retried once that operation
// finishes, and the slot has to end up off / shutdown without cluster-autoscaler issuing a
// second DeleteNodes. It walks the three actors in order - the refused shutdown, the scale-up
// job monitor that releases the lock, and the next reconcile of the same slot - on one slot,
// one coordination manager and one recorder.
func TestShutdownNodeCompletesAfterTheScaleUpReleasesTheLock(t *testing.T) {
	node := bootingSlot("test-node")
	kubeNode := CreateTestKubernetesNode("test-node-k8s", false)
	env := newConflictTestEnv(t, node, kubeNode)

	ctx := context.TODO()
	booting := env.heldLock(t, node, OperationScaleUp, 4*time.Minute)

	// 1. the shutdown arrives and loses the lock; the slot stays ready and the intent (desired
	// off) is left where cluster-autoscaler wrote it.
	first := env.machine(t, booting)
	err := first.ShutdownNode()
	require.Error(t, err)
	require.NotNil(t, AsLockConflict(err))
	require.Equal(t, infrav1alpha1.ProgressReady, env.stored(t, node).Status.Progress)

	// 2. the startup finishes. The job monitor of the scale-up is a state machine primed from
	// the progress it started from - startingup - and reporting its Job completed is what
	// releases the coordination lock and records ready.
	monitorNode := env.stored(t, node).DeepCopy()
	monitorNode.Status.Progress = infrav1alpha1.ProgressStartingUp
	monitor := env.machine(t, monitorNode)

	startupJob := CreateTestJobWithNodeLabel("test-node-startup-1", node.Namespace, PhaseStartup, "test-node-k8s")
	require.NoError(t, monitor.JobCompleted(startupJob))

	stored := env.stored(t, node)
	assert.Equal(t, infrav1alpha1.ProgressReady, stored.Status.Progress)
	_, locked := env.coordMgr.CheckLock(context.TODO(), stored)
	assert.False(t, locked, "the completed scale-up must not leave its lock on the slot")

	// 3. the next reconcile of the same slot asks for the shutdown again, off the spec, with no
	// second DeleteNodes from cluster-autoscaler. It wins the lock now.
	retry := env.machine(t, env.stored(t, node))
	require.NoError(t, retry.ShutdownNode())

	stored = env.stored(t, node)
	assert.Equal(t, infrav1alpha1.ProgressShuttingDown, stored.Status.Progress,
		"the retried shutdown runs, so the transitional progress it writes is driven by a real operation")
	assert.Equal(t, infrav1alpha1.PowerStateOff, stored.Spec.DesiredPowerState)

	jobs := &batchv1.JobList{}
	require.NoError(t, env.fakeClient.List(ctx, jobs))
	require.Len(t, jobs.Items, 1, "the retried shutdown owns the slot and creates its job")
	assert.Equal(t, PhaseShutdown, jobs.Items[0].Labels[LabelType])

	lock, locked := env.coordMgr.CheckLock(context.TODO(), stored)
	require.True(t, locked)
	assert.Equal(t, OperationScaleDown, lock.Operation)

	// 4. the shutdown job finishes: the slot ends off / shutdown, and no lock is left behind.
	shutdownJob := jobs.Items[0]
	require.NoError(t, retry.JobCompleted(&shutdownJob))

	stored = env.stored(t, node)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, stored.Status.Progress, "the slot ends shut down")
	assert.Equal(t, infrav1alpha1.PowerStateOff, stored.Spec.DesiredPowerState)
	_, locked = env.coordMgr.CheckLock(context.TODO(), stored)
	assert.False(t, locked, "a completed shutdown releases its lock")

	// Exactly one ScaleDownFailed for the whole story - the deferral - plus the events of the
	// shutdown that ran.
	recorded := drainEvents(env.recorder)
	assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 1)
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownRequested, 1)
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownCompleted, 1)
	assertEventCount(t, recorded, "Normal "+ReasonScaleUpCompleted, 1)
}

// TestStartNodeLockConflictLeavesSlotUntouched is the mirror: a scale-up that arrives while a
// scale-down owns the slot. It must not leave the slot in startingup, and it must not run any
// of its side effects on a machine it does not own - the startup Job, and clearing the
// ToBeDeletedByClusterAutoscaler taint off a node the shutdown is draining.
func TestStartNodeLockConflictLeavesSlotUntouched(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown)
	node.Status.PowerState = infrav1alpha1.PowerStateOff

	kubeNode := CreateTestKubernetesNode("test-node-k8s", true)
	kubeNode.Spec.Taints = []corev1.Taint{{
		Key:    "ToBeDeletedByClusterAutoscaler",
		Effect: corev1.TaintEffectNoSchedule,
	}}

	env := newConflictTestEnv(t, node, kubeNode)
	shuttingDown := env.heldLock(t, node, OperationScaleDown, 0)

	nm := env.machine(t, shuttingDown)
	err := nm.StartNode()
	require.Error(t, err)

	conflict := AsLockConflict(err)
	require.NotNil(t, conflict)
	assert.Equal(t, OperationScaleUp, conflict.Operation)
	assert.Equal(t, OperationScaleDown, conflict.HolderOperation())

	stored := env.stored(t, node)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, stored.Status.Progress,
		"a cancelled scale-up must not leave the slot in startingup")
	assert.Equal(t, StateShutdown, nm.GetCurrentState())

	jobs := &batchv1.JobList{}
	require.NoError(t, env.fakeClient.List(context.TODO(), jobs))
	assert.Empty(t, jobs.Items, "a refused scale-up creates no job")

	storedKubeNode := &corev1.Node{}
	require.NoError(t, env.fakeClient.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, storedKubeNode))
	require.Len(t, storedKubeNode.Spec.Taints, 1)
	assert.Equal(t, "ToBeDeletedByClusterAutoscaler", storedKubeNode.Spec.Taints[0].Key,
		"a refused scale-up must not clear the taint of the deletion in flight")

	recorded := drainEvents(env.recorder)
	assertEventCount(t, recorded, "Warning "+ReasonScaleUpFailed, 1)
	assertEventCount(t, recorded, "Normal "+ReasonScaleUpRequested, 0)
}

// TestJobTimeoutReleasesCoordinationLock covers the lock that outlives its operation. A
// lifecycle Job that runs out of time moves the slot back to a stable state, and before this
// hook existed the coordination lock stayed on the slot for the rest of its timeout: the next
// lifecycle operation on that slot then lost the acquisition to a lock nothing was running
// under, which is the shape of the live trace.
func TestJobTimeoutReleasesCoordinationLock(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp)
	node.Status.PowerState = infrav1alpha1.PowerStateOff

	env := newConflictTestEnv(t, node)
	booting := env.heldLock(t, node, OperationScaleUp, 0)

	nm := env.machine(t, booting)
	require.NoError(t, nm.JobTimeout())

	stored := env.stored(t, node)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, stored.Status.Progress)
	assert.Equal(t, StateShutdown, nm.GetCurrentState())

	_, locked := env.coordMgr.CheckLock(context.TODO(), stored)
	assert.False(t, locked, "a timed out operation must not leave its lock on the slot")

	// The failed scale up is recorded once, by the state the slot landed in.
	recorded := drainEvents(env.recorder)
	assertEventCount(t, recorded, "Warning "+ReasonScaleUpFailed, 1)
	assert.Len(t, recorded, 1, "%v", recorded)
}

// TestUpdateNodeProgressPersistsOnlyTheCurrentState pins the guard that keeps a state machine
// from writing a progress it is not in. The writers of status.progress are the enter_ and
// after_ hooks, which looplab/fsm runs after applying the state change, so on every
// legitimate path the two agree; a disagreement can only come from a machine that is stale -
// the job monitor of an earlier reconcile, or one built from a Node read before a transition -
// and that is precisely how a slot ends up in a transitional progress nothing is driving.
func TestUpdateNodeProgressPersistsOnlyTheCurrentState(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
	node.Status.PowerState = infrav1alpha1.PowerStateOn

	env := newConflictTestEnv(t, node)
	nm := env.machine(t, env.stored(t, node))

	// a machine in shuttingdown cannot record ready: that write is not its transition to make
	nm.updateNodeProgress(infrav1alpha1.ProgressReady)
	assert.Equal(t, infrav1alpha1.ProgressShuttingDown, env.stored(t, node).Status.Progress)

	// the same machine, moved to the state a hook is entering, writes its own state
	nm.PrimeEnterState(StateShutdown)
	nm.updateNodeProgress(infrav1alpha1.ProgressShutdown)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, env.stored(t, node).Status.Progress)
}

// TestShutdownJobCreationFailureRollsTheSlotBackOutOfShuttingDown covers the other half of the
// requirement: a transition that *ran* and then failed - here an API server that refuses the
// shutdown Job - must not leave the slot in shuttingdown either. The rollback (JobFailed)
// moves it back to ready and afterJobFailed releases the coordination lock, so the next
// reconcile can ask for the shutdown again, and the provider's target size keeps counting a
// machine that is still up.
func TestShutdownJobCreationFailureRollsTheSlotBackOutOfShuttingDown(t *testing.T) {
	node := bootingSlot("test-node")

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	fake := CreateFakeClient(scheme, node)
	failing := &createJobFailingClient{Client: fake, err: assert.AnError}
	mockCoord := NewMockCoordinationManager()

	rec := events.NewFakeRecorder(20)
	nm := NewNodeStateMachine(node, failing, scheme, mockCoord, WithEventRecorder(rec))
	nm.uncordonTimeout = 100 * time.Millisecond

	err := nm.ShutdownNode()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create shutdown job")

	stored := &infrav1alpha1.Node{}
	require.NoError(t, fake.Get(context.TODO(), client.ObjectKeyFromObject(node), stored))
	assert.Equal(t, infrav1alpha1.ProgressReady, stored.Status.Progress,
		"a shutdown whose job could not be created leaves the slot ready, not shuttingdown")
	assert.Equal(t, StateReady, nm.GetCurrentState())
	assert.False(t, mockCoord.HasLock("test-node"), "the lock of a failed operation is released")

	recorded := drainEvents(rec)
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownRequested, 1)
	failed := assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 1)
	assert.Contains(t, failed[0], "failed to create shutdown job")
	// rolling back into ready is not a completed scale up
	assertEventCount(t, recorded, "Normal "+ReasonScaleUpCompleted, 0)
	assert.Len(t, recorded, 2, "%v", recorded)
}
