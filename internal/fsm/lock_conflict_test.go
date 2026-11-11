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
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
)

// The tests in this file drive the collision the live trace showed: cluster-autoscaler asked
// to delete a slot about 4 minutes into its startup, and the delete came back as
//
//	ScaleDownFailed ... FSM transition failed: transition canceled with error:
//	  coordination lock already exists: operation=scale-up, owner=node-controller, age=4m0s
//
// because the scale-up held the coordination lock. Pre-fix that refusal was terminal - it
// reached the autoscaler as an error, controller-runtime's rate-limited queue owned the retry,
// and it had already torn the state of the operation in flight apart on the way to being
// refused: the startup Job of that scale-up deleted, the Kubernetes node cordoned.
//
// (The progress=shuttingdown and the "Late transition phase - extended backoff" of the same
// trace are not produced by this path: the before_ hooks cancelled the transition before
// looplab/fsm applied the state change, so the enter_ hook that writes the progress never
// ran. The writer of that progress is NodeGroupDecreaseTargetSize, in the gRPC server, and
// it is fixed in internal/grpcserver with these tests.)
//
// They use the real DefaultCoordinationManager against a fake client, because the lock
// annotations are the thing under test.

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
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

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
func (e *conflictTestEnv) machine(t *testing.T, node *infrav2alpha1.Node) *NodeStateMachine {
	t.Helper()

	nm := NewNodeStateMachine(node, e.fakeClient, e.scheme, e.coordMgr, WithEventRecorder(e.recorder))
	nm.uncordonTimeout = 100 * time.Millisecond
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	return nm
}

// stored reads the slot back from the API, so the assertions are about what is persisted and
// not about the copy a state machine happens to hold.
func (e *conflictTestEnv) stored(t *testing.T, node *infrav2alpha1.Node) *infrav2alpha1.Node {
	t.Helper()

	fresh := &infrav2alpha1.Node{}
	require.NoError(t, e.fakeClient.Get(context.TODO(), client.ObjectKeyFromObject(node), fresh))

	return fresh
}

// heldScaleUpLock takes the coordination lock of a scale-up on the slot, the way the startup
// path does, and returns the slot as it is stored with that lock on it. age is written into
// the lock timestamp: the live trace shows a lock 4m0s old, inside its 5m timeout.
func (e *conflictTestEnv) heldLock(t *testing.T, node *infrav2alpha1.Node, operation string, age time.Duration) *infrav2alpha1.Node {
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
// brought it up still 4 minutes old on the slot. The name is a parameter so a test can state
// which slot it drives; the coordination tests read it back under the same name.
func bootingSlot(name string) *infrav2alpha1.Node {
	node := CreateTestNode(name, config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseReady)
	node.Status.PowerState = infrav2alpha1.PowerStateOn

	return node
}

// TestShutdownNodeLockConflictLeavesNoTransitionalProgress is the stranding itself. The
// shutdown arrives while the scale-up owns the slot, loses the acquisition, and must leave the
// slot exactly as it was:
//
//   - status.phase is not left shuttingdown (a). A transitional progress is excluded from
//     the provider's target size, validateStateTransition refuses to start a transition from
//     one, and nothing in the controller heals it: the stuck-transition cleanup the docs
//     promised runs on the machine's own backoff record, which the reconciler builds fresh on
//     every pass, so ForceCleanup is never reached from a reconcile (see
//     docs/architecture/state.md);
//   - the refusal is reported as a deferral with a deadline, not a terminal cancellation (b);
//   - the lock of the operation in flight is untouched, and the shutdown performs none of its
//     side effects on a slot it does not own (c);
//   - the deferral is recorded once, as a Normal ScaleDownDeferred naming the lock and the
//     deadline, with no Warning and no ScaleDownRequested for a transition that never happened (d).
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
	assert.Equal(t, infrav2alpha1.NodePhaseReady, stored.Status.Phase,
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

	// (d) the deferral is recorded once, as a deferral, and there is no request event for a
	// transition that never happened
	recorded := drainEvents(env.recorder)
	deferred := assertEventCount(t, recorded, "Normal "+ReasonScaleDownDeferred, 1)
	assert.Contains(t, deferred[0], "coordination lock already exists")
	assert.Contains(t, deferred[0], "operation=scale-up")
	assert.Contains(t, deferred[0], "owner=node-controller")
	assert.Contains(t, deferred[0], "retrying in", "the event says when the shutdown comes back")
	assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 0)
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
	require.Equal(t, infrav2alpha1.NodePhaseReady, env.stored(t, node).Status.Phase)

	// 2. the startup finishes. The job monitor of the scale-up is a state machine primed from
	// the progress it started from - startingup - and reporting its Job completed is what
	// releases the coordination lock and records ready.
	monitorNode := env.stored(t, node).DeepCopy()
	monitorNode.Status.Phase = infrav2alpha1.NodePhaseBooting
	monitor := env.machine(t, monitorNode)

	startupJob := CreateTestJobWithNodeLabel("test-node-startup-1", node.Namespace, PhaseStartup, "test-node-k8s")
	require.NoError(t, monitor.JobCompleted(startupJob))

	stored := env.stored(t, node)
	assert.Equal(t, infrav2alpha1.NodePhaseReady, stored.Status.Phase)
	_, locked := env.coordMgr.CheckLock(context.TODO(), stored)
	assert.False(t, locked, "the completed scale-up must not leave its lock on the slot")

	// 3. the next reconcile of the same slot asks for the shutdown again, off the spec, with no
	// second DeleteNodes from cluster-autoscaler. It wins the lock now.
	retry := env.machine(t, env.stored(t, node))
	require.NoError(t, retry.ShutdownNode())

	stored = env.stored(t, node)
	assert.Equal(t, infrav2alpha1.NodePhaseDraining, stored.Status.Phase,
		"the retried shutdown runs, so the transitional progress it writes is driven by a real operation")
	assert.Equal(t, infrav2alpha1.PowerStateOff, stored.Spec.DesiredPowerState)

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
	assert.Equal(t, infrav2alpha1.NodePhaseOff, stored.Status.Phase, "the slot ends shut down")
	assert.Equal(t, infrav2alpha1.PowerStateOff, stored.Spec.DesiredPowerState)
	_, locked = env.coordMgr.CheckLock(context.TODO(), stored)
	assert.False(t, locked, "a completed shutdown releases its lock")

	// One ScaleDownDeferred for the whole story - the collision - plus the events of the
	// shutdown that ran. A collision is the lock doing its job, so nothing on this path is a
	// Warning.
	recorded := drainEvents(env.recorder)
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownDeferred, 1)
	assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 0)
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownRequested, 1)
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownCompleted, 1)
	assertEventCount(t, recorded, "Normal "+ReasonScaleUpCompleted, 1)
}

// TestStartNodeLockConflictLeavesSlotUntouched is the mirror: a scale-up that arrives while a
// scale-down owns the slot. It must not leave the slot in startingup, and it must not run any
// of its side effects on a machine it does not own - the startup Job, and clearing the
// ToBeDeletedByClusterAutoscaler taint off a node the shutdown is draining.
func TestStartNodeLockConflictLeavesSlotUntouched(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseOff)
	node.Status.PowerState = infrav2alpha1.PowerStateOff

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
	assert.Equal(t, infrav2alpha1.NodePhaseOff, stored.Status.Phase,
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
	assertEventCount(t, recorded, "Normal "+ReasonScaleUpDeferred, 1)
	assertEventCount(t, recorded, "Warning "+ReasonScaleUpFailed, 0)
	assertEventCount(t, recorded, "Normal "+ReasonScaleUpRequested, 0)
}

// TestJobTimeoutReleasesCoordinationLock covers the lock that outlives its operation. A
// lifecycle Job that runs out of time moves the slot back to a stable state, and before this
// hook existed the coordination lock stayed on the slot for the rest of its timeout: the next
// lifecycle operation on that slot then lost the acquisition to a lock nothing was running
// under, which is the shape of the live trace.
func TestJobTimeoutReleasesCoordinationLock(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	node.Status.PowerState = infrav2alpha1.PowerStateOff

	env := newConflictTestEnv(t, node)
	booting := env.heldLock(t, node, OperationScaleUp, 0)

	nm := env.machine(t, booting)
	require.NoError(t, nm.JobTimeout())

	stored := env.stored(t, node)
	assert.Equal(t, infrav2alpha1.NodePhaseOff, stored.Status.Phase)
	assert.Equal(t, StateShutdown, nm.GetCurrentState())

	_, locked := env.coordMgr.CheckLock(context.TODO(), stored)
	assert.False(t, locked, "a timed out operation must not leave its lock on the slot")

	// The failed scale up is recorded once, by the state the slot landed in.
	recorded := drainEvents(env.recorder)
	assertEventCount(t, recorded, "Warning "+ReasonScaleUpFailed, 1)
	assert.Len(t, recorded, 1, "%v", recorded)
}

// TestUpdateNodeProgressPersistsOnlyTheCurrentState pins the guard that keeps a state machine
// from writing a progress it is not in. Inside the FSM the writers of status.phase are the
// enter_ and after_ hooks, which looplab/fsm runs after applying the state change, so on every
// legitimate path the two agree; a disagreement can only come from a machine that is stale -
// the job monitor of an earlier reconcile, or one built from a Node read before a transition -
// and that is precisely how a slot ends up in a transitional progress nothing is driving.
// The guard only covers writes that go through it: the gRPC server and the core controllers
// write status.phase too, and they are not state machines (see docs/architecture/state.md).
func TestUpdateNodeProgressPersistsOnlyTheCurrentState(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseDraining)
	node.Status.PowerState = infrav2alpha1.PowerStateOn

	env := newConflictTestEnv(t, node)
	nm := env.machine(t, env.stored(t, node))

	// a machine in shuttingdown cannot record ready: that write is not its transition to make
	nm.updateNodeProgress(infrav2alpha1.NodePhaseReady)
	assert.Equal(t, infrav2alpha1.NodePhaseDraining, env.stored(t, node).Status.Phase)

	// the same machine, moved to the state a hook is entering, writes its own state
	nm.PrimeEnterState(StateShutdown)
	nm.updateNodeProgress(infrav2alpha1.NodePhaseOff)
	assert.Equal(t, infrav2alpha1.NodePhaseOff, env.stored(t, node).Status.Phase)
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
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	fake := CreateFakeClient(scheme, node)
	failing := &createJobFailingClient{Client: fake, err: assert.AnError}
	mockCoord := NewMockCoordinationManager()

	rec := events.NewFakeRecorder(20)
	nm := NewNodeStateMachine(node, failing, scheme, mockCoord, WithEventRecorder(rec))
	nm.uncordonTimeout = 100 * time.Millisecond
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	err := nm.ShutdownNode()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create shutdown job")

	stored := &infrav2alpha1.Node{}
	require.NoError(t, fake.Get(context.TODO(), client.ObjectKeyFromObject(node), stored))
	assert.Equal(t, infrav2alpha1.NodePhaseReady, stored.Status.Phase,
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

// slotWriteRace is an API server that loses a race on the slot: the first `conflicts` Update
// calls on a Node CR are refused with a plain resourceVersion conflict, the same error a real
// apiserver returns when a status write from the core controller bumps the object under this
// write. `racer`, when set, runs inside that window and puts a lock on the slot in the API,
// the way the operation that wins the race does.
//
// The point of the shape is that it separates the two things a write conflict can mean: a
// lock in the API, which is a deferral, and a slot that merely moved, which is a retry.
type slotWriteRace struct {
	client.Client

	mu        sync.Mutex
	attempts  int
	conflicts int
	racer     func() error
}

func (c *slotWriteRace) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, isNode := obj.(*infrav2alpha1.Node); !isNode {
		return c.Client.Update(ctx, obj, opts...)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.attempts++

	if c.attempts <= c.conflicts {
		if c.racer != nil {
			// A racer that cannot write its lock is a broken test setup: surface it as the
			// API error it is, rather than letting the test read a slot nobody filled in.
			if err := c.racer(); err != nil {
				return err
			}
		}

		return apierrors.NewConflict(
			schema.GroupResource{Group: infrav2alpha1.GroupVersion.Group, Resource: "nodes"},
			obj.GetName(),
			errors.New("the object has been modified; please apply your changes to the latest version and try again"),
		)
	}

	return c.Client.Update(ctx, obj, opts...)
}

// slotUpdates counts the Write attempts the race client was asked to make on a slot, so a
// test can tell "retried the acquisition" from "gave up on the first conflict".
func (c *slotWriteRace) slotUpdates() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.attempts
}

// foreignLockRacer writes a lock owned by somebody else onto the slot, through the
// unwrapped client, in the window the race opens. It returns the update error: the test
// asserts on the lock it wrote, so a racer that failed is caught by those assertions.
func (c *slotWriteRace) foreignLockRacer(operation, owner string, age time.Duration) func() error {
	return func() error {
		live := &infrav2alpha1.Node{}
		if err := c.Get(context.TODO(), client.ObjectKey{Name: "test-node", Namespace: config.NewNamespaceConfig().Get()}, live); err != nil {
			return err
		}

		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[OperationLockAnnotation] = operation
		live.Annotations[LockOwnerAnnotation] = owner
		live.Annotations[LockTimestampAnnotation] = time.Now().Add(-age).Format(time.RFC3339)
		live.Annotations[LockTimeoutAnnotation] = strconv.FormatFloat(DefaultLockTimeout.Seconds(), 'f', 0, 64)

		return c.Client.Update(context.TODO(), live)
	}
}

// TestAcquireLockPlainWriteConflictIsRetried hits the branch a benign race opens: the slot
// moved under the lock write (a status update from the core controller, a resync) and there is
// no lock in the API at all. The acquisition is retried on the new object and succeeds.
//
// Deciding from the copy the attempt stamped - the pre-fix shape - reports this acquisition's
// own annotations as the lock in the way, with a timestamp of time.Now(), and returns a typed
// conflict. RetryOnConflict stops on it, because the error is no longer a resourceVersion
// conflict, and a write race costs a Warning event plus a defer of the rest of the lock
// timeout:
//
//	coordination lock changed during retry: operation=scale-up, owner=node-controller, age=335ms
//	retryAfter=4m59.66s   lock really present in API: false
func TestAcquireLockPlainWriteConflictIsRetried(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseReady)
	node.Status.PowerState = infrav2alpha1.PowerStateOn
	env := newConflictTestEnv(t, node)

	racy := &slotWriteRace{Client: env.fakeClient, conflicts: 1}
	coord := NewCoordinationManager(racy, NodeControllerOwner)

	require.NoError(t, coord.AcquireLock(context.TODO(), env.stored(t, node), OperationScaleDown, DefaultLockTimeout))

	assert.Equal(t, 2, racy.slotUpdates(), "the lost write is retried, not reported as a lock conflict")

	lock, exists := coord.CheckLock(context.TODO(), env.stored(t, node))
	require.True(t, exists, "the retried acquisition takes the lock")
	assert.Equal(t, OperationScaleDown, lock.Operation)
	assert.Equal(t, NodeControllerOwner, lock.Owner)
}

// TestAcquireLockWriteConflictNamesTheLockThatWon is the other half of the same branch: a
// write conflict with a real lock in the API, written by the operation that beat this one.
// That is a deferral, and it has to name the holder that is actually in the slot - operation,
// owner, and the remaining time of that lock - because the caller schedules the retry from it.
func TestAcquireLockWriteConflictNamesTheLockThatWon(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseReady)
	node.Status.PowerState = infrav2alpha1.PowerStateOn
	env := newConflictTestEnv(t, node)

	racy := &slotWriteRace{Client: env.fakeClient, conflicts: 1}
	racy.racer = racy.foreignLockRacer(OperationScaleUp, "cluster-autoscaler", time.Minute)
	coord := NewCoordinationManager(racy, NodeControllerOwner)

	err := coord.AcquireLock(context.TODO(), env.stored(t, node), OperationScaleDown, DefaultLockTimeout)

	conflict := AsLockConflict(err)
	require.NotNil(t, conflict, "a lock in the API is a deferral: %v", err)
	assert.Equal(t, OperationScaleDown, conflict.Operation)
	assert.Equal(t, lockReasonChangedOnRetry, conflict.Reason)
	assert.Equal(t, OperationScaleUp, conflict.HolderOperation(), "the conflict names the lock that won, not this acquisition")
	assert.Equal(t, "cluster-autoscaler", conflict.HolderOwner())
	assert.Greater(t, conflict.HolderAge(), 50*time.Second)
	// a 5m lock one minute old leaves four minutes: the retry is scheduled from the holder,
	// not from a stamp this acquisition wrote itself
	assert.InDelta(t, 4*time.Minute, conflict.RetryAfter(), float64(10*time.Second))

	lock, exists := coord.CheckLock(context.TODO(), env.stored(t, node))
	require.True(t, exists, "a refused acquisition leaves the holder's lock alone")
	assert.Equal(t, OperationScaleUp, lock.Operation)
	assert.Equal(t, "cluster-autoscaler", lock.Owner)
	assert.Equal(t, 1, racy.slotUpdates(), "the conflict is decided from the first lost write, no retry needed")
}

// TestStartNodeSurvivesAPlainWriteConflictOnTheLock is the same race one level up, on the path
// a benign conflict reaches in production: the scale-up takes the lock, the apiserver refuses
// the write because the core controller updated the slot's status, and the acquisition is
// retried. The startup runs, and the slot records no lock event at all - pre-fix this is a
// ScaleUpFailed Warning, a startup that never happened, and a requeue at the lock timeout.
func TestStartNodeSurvivesAPlainWriteConflictOnTheLock(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseOff)
	node.Status.PowerState = infrav2alpha1.PowerStateOff
	kubeNode := CreateTestKubernetesNode("test-node-k8s", false)

	env := newConflictTestEnv(t, node, kubeNode)
	racy := &slotWriteRace{Client: env.fakeClient, conflicts: 1}
	coord := NewCoordinationManager(racy, NodeControllerOwner)

	nm := NewNodeStateMachine(env.stored(t, node), racy, env.scheme, coord, WithEventRecorder(env.recorder))
	nm.uncordonTimeout = 100 * time.Millisecond
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	err := nm.StartNode()
	require.NoError(t, err, "a write race with no lock in the API is retried, it is not a lock conflict")
	assert.Nil(t, AsLockConflict(err))

	assert.Greater(t, racy.slotUpdates(), 1, "the acquisition ran again after the lost write")

	stored := env.stored(t, node)
	assert.Equal(t, infrav2alpha1.NodePhaseBooting, stored.Status.Phase, "the startup ran")

	lock, exists := coord.CheckLock(context.TODO(), stored)
	require.True(t, exists, "the startup owns the slot while its job runs")
	assert.Equal(t, OperationScaleUp, lock.Operation)

	jobs := &batchv1.JobList{}
	require.NoError(t, env.fakeClient.List(context.TODO(), jobs))
	require.Len(t, jobs.Items, 1, "the startup created its job")
	assert.Equal(t, PhaseStartup, jobs.Items[0].Labels[LabelType])

	recorded := drainEvents(env.recorder)
	assertEventCount(t, recorded, "Warning "+ReasonScaleUpFailed, 0)
	assertEventCount(t, recorded, "Normal "+ReasonScaleUpRequested, 1)
	for _, event := range recorded {
		assert.NotContains(t, event, "coordination lock", "a plain write race records no lock event: %v", recorded)
	}
}
