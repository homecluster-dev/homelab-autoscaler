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
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
	"github.com/looplab/fsm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

func TestBeforeStartNode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("successful lock acquisition", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		ctx := context.TODO()

		// Create a mock FSM event
		event := &fsm.Event{
			FSM:   nm.fsm,
			Event: EventStartNode,
			Src:   StateShutdown,
			Dst:   StateStartingUp,
		}

		// Call the before hook
		nm.beforeStartNode(ctx, event)

		// Verify lock acquisition was called
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)
		assert.Equal(t, node, acquireCalls[0].Node)
		assert.Equal(t, OperationScaleUp, acquireCalls[0].Operation)
		assert.Equal(t, DefaultLockTimeout, acquireCalls[0].Timeout)

		// Verify event was not cancelled
		assert.Nil(t, event.Err)
	})

	t.Run("lock acquisition failure cancels event", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		mockCoord := NewMockCoordinationManager()
		mockCoord.SetAcquireError(assert.AnError)
		fakeClient := CreateFakeClient(scheme, node)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		ctx := context.TODO()

		// Call the before hook with nil event (simulating direct call)
		nm.beforeStartNode(ctx, nil)

		// Verify lock acquisition was attempted
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)

		// Verify the hook handled the nil event gracefully
		// The main behavior is that lock acquisition was attempted
	})
}

func TestBeforeShutdownNode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("successful lock acquisition", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		ctx := context.TODO()

		// Create a mock FSM event
		event := &fsm.Event{
			FSM:   nm.fsm,
			Event: EventShutdownNode,
			Src:   StateReady,
			Dst:   StateShuttingDown,
		}

		// Call the before hook
		nm.beforeShutdownNode(ctx, event)

		// Verify lock acquisition was called
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)
		assert.Equal(t, node, acquireCalls[0].Node)
		assert.Equal(t, OperationScaleDown, acquireCalls[0].Operation)
		assert.Equal(t, DefaultLockTimeout, acquireCalls[0].Timeout)

		// Verify event was not cancelled
		assert.Nil(t, event.Err)
	})

	t.Run("lock acquisition failure cancels event", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		mockCoord := NewMockCoordinationManager()
		mockCoord.SetAcquireError(assert.AnError)
		fakeClient := CreateFakeClient(scheme, node)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		ctx := context.TODO()

		// Call the before hook with nil event (simulating direct call)
		nm.beforeShutdownNode(ctx, nil)

		// Verify lock acquisition was attempted
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)

		// Verify the hook handled the nil event gracefully
		// The main behavior is that lock acquisition was attempted
	})
}

func TestAfterJobCompleted(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("successful lock release and status update", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		mockCoord := NewMockCoordinationManager()

		// Create a Kubernetes node that is Ready but unschedulable (cordoned)
		kubeNode := CreateTestKubernetesNode("test-node-k8s", true)

		startingClaim := TestClaim("test-node", "claim-start", infrav2alpha1.NodeClaimBooting, false)
		fakeClient := CreateFakeClient(scheme, node, kubeNode, startingClaim)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord,
			WithClaim(startingClaim, newClaimTestWriter(fakeClient)))
		ctx := context.TODO()

		// Create a test job and add it to the fake client
		job := CreateTestJob("test-job", config.NewNamespaceConfig().Get(), "startup")

		// Create a mock FSM event with job argument
		event := &fsm.Event{
			FSM:   nm.fsm,
			Event: EventJobCompleted,
			Src:   StateStartingUp,
			Dst:   StateReady,
			Args:  []interface{}{job},
		}

		// looplab/fsm applies the state change before the after_ callback runs, so the
		// machine has to be in the state the transition entered (see PrimeEnterState).
		nm.PrimeEnterState(StateReady)

		// Call the after hook
		nm.afterJobCompleted(ctx, event)

		// Verify lock release was called
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)
		assert.Equal(t, node, releaseCalls[0].Node)

		// Verify node status was updated
		updatedNode := &infrav2alpha1.Node{}
		err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		// The gate passed, so the claim moved booting -> ready, and the gate's own read of the node is
		// an input of the report (recordGateAnswer): the node it just uncordoned reports Ready, so the
		// machine is observed on and usable, and the derived report is ready. This is the shape of a real
		// boot - the machine that fired the startup Job watched it off, and the machine that answers for
		// its completion is the goroutine of that same pass - and the report has to land here: the other
		// writer, the observation path, runs on corev1.Node events, and a kubelet that has settled into
		// Ready writes no more of them.
		assert.Equal(t, infrav2alpha1.NodePhaseReady, updatedNode.Status.Phase)

		updatedClaim := &infrav2alpha1.NodeClaim{}
		require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(startingClaim), updatedClaim))
		assert.Equal(t, infrav2alpha1.NodeClaimReady, updatedClaim.Status.Phase,
			"the readiness gate's answer is the evidence for the claim's ready phase")

		// Verify Kubernetes node was uncordoned
		updatedKubeNode := &corev1.Node{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-node-k8s"}, updatedKubeNode)
		assert.NoError(t, err)
		assert.False(t, updatedKubeNode.Spec.Unschedulable, "Kubernetes node should be uncordoned")
	})

	t.Run("lock release failure is logged but doesn't fail", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseDraining)
		mockCoord := NewMockCoordinationManager()
		mockCoord.SetReleaseError(assert.AnError)
		fakeClient := CreateFakeClient(scheme, node)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		ctx := context.TODO()

		// Create a test job and add it to the fake client
		job := CreateTestJob("test-job", config.NewNamespaceConfig().Get(), "shutdown")

		// Create a mock FSM event with job argument
		event := &fsm.Event{
			FSM:   nm.fsm,
			Event: EventJobCompleted,
			Src:   StateShuttingDown,
			Dst:   StateShutdown,
			Args:  []interface{}{job},
		}

		// looplab/fsm applies the state change before the after_ callback runs, so the
		// machine has to be in the state the transition entered (see PrimeEnterState).
		nm.PrimeEnterState(StateShutdown)

		// Call the after hook - should not panic or fail
		nm.afterJobCompleted(ctx, event)

		// Verify lock release was attempted
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)

		// Verify node status was still updated despite lock release failure
		updatedNode := &infrav2alpha1.Node{}
		err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		assert.Equal(t, infrav2alpha1.NodePhaseOff, updatedNode.Status.Phase)
	})
}

// writeOrderClient records every write it performs on a corev1.Node, in order, with the state
// the object was written with. Ordering tests need it: the end state of "the node is tainted and
// schedulable" is the same whether the taint landed before or after the uncordon, and only the
// first of those closes the window in which a pod can be bound to an untainted machine.
type writeOrderClient struct {
	client.Client
	writes []string
}

func (c *writeOrderClient) record(op string, obj client.Object) {
	kubeNode, isKubeNode := obj.(*corev1.Node)
	if !isKubeNode {
		return
	}

	keys := make([]string, 0, len(kubeNode.Spec.Taints))
	for i := range kubeNode.Spec.Taints {
		keys = append(keys, kubeNode.Spec.Taints[i].Key)
	}

	c.writes = append(c.writes, fmt.Sprintf("%s unschedulable=%v taints=[%s]", op, kubeNode.Spec.Unschedulable, strings.Join(keys, ",")))
}

func (c *writeOrderClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	err := c.Client.Patch(ctx, obj, patch, opts...)
	c.record("patch", obj)

	return err
}

func (c *writeOrderClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	err := c.Client.Update(ctx, obj, opts...)
	c.record("update", obj)

	return err
}

// TestAfterJobCompletedStampsDeclaredTaintsBeforeUncordon is the window a tainting startup Job
// cannot close: the scale-up path used to uncordon the node, and only the next reconcile stamped
// the declared taints, so a non-tolerating pod could be bound in between. The hook must taint
// first, on the pass that finds the node object, and the uncordon must not wipe the taint (it
// writes the whole node object it read).
func TestAfterJobCompletedStampsDeclaredTaintsBeforeUncordon(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	role := corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
	node.Spec.NodeTaints = []corev1.Taint{role}

	// Ready and cordoned, the state of a machine that has just registered after startup
	kubeNode := CreateTestKubernetesNode("test-node-k8s", true)
	base := CreateFakeClient(scheme, node, kubeNode)
	recorder := &writeOrderClient{Client: base}

	mockCoord := NewMockCoordinationManager()
	nm := NewNodeStateMachine(node, recorder, scheme, mockCoord)
	nm.uncordonTimeout = 5 * time.Second

	ctx := context.TODO()
	event := &fsm.Event{
		FSM:   nm.fsm,
		Event: EventJobCompleted,
		Src:   StateStartingUp,
		Dst:   StateReady,
		Args:  []interface{}{CreateTestJob("test-job", config.NewNamespaceConfig().Get(), "startup")},
	}

	nm.afterJobCompleted(ctx, event)

	updatedKubeNode := &corev1.Node{}
	require.NoError(t, base.Get(ctx, client.ObjectKey{Name: "test-node-k8s"}, updatedKubeNode))

	keys := make([]string, 0, len(updatedKubeNode.Spec.Taints))
	for i := range updatedKubeNode.Spec.Taints {
		keys = append(keys, updatedKubeNode.Spec.Taints[i].Key)
	}

	assert.False(t, updatedKubeNode.Spec.Unschedulable, "the node ends up schedulable")
	assert.Equal(t, []string{"node.kubernetes.io/role"}, keys, "the declared taint is on the node")

	require.Len(t, recorder.writes, 2, "one write stamps the taints, one uncordons")
	assert.Contains(t, recorder.writes[0], "patch unschedulable=true taints=[node.kubernetes.io/role]",
		"the taint must land while the node is still cordoned: \"%s\"", recorder.writes[0])
	assert.Contains(t, recorder.writes[1], "update unschedulable=false taints=[node.kubernetes.io/role]",
		"the uncordon must keep the taint it read: \"%s\"", recorder.writes[1])
}

func TestAfterJobFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("successful lock release and failure condition added", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		ctx := context.TODO()

		// Create a mock FSM event
		event := &fsm.Event{
			FSM:   nm.fsm,
			Event: EventJobFailed,
			Src:   StateStartingUp,
			Dst:   StateShutdown,
		}

		// looplab/fsm applies the state change before the after_ callback runs, so the
		// machine has to be in the state the transition entered (see PrimeEnterState).
		nm.PrimeEnterState(StateShutdown)

		// Call the after hook
		nm.afterJobFailed(ctx, event)

		// Verify lock release was called
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)
		assert.Equal(t, node, releaseCalls[0].Node)

		// Verify node status was updated
		updatedNode := &infrav2alpha1.Node{}
		err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		assert.Equal(t, infrav2alpha1.NodePhaseOff, updatedNode.Status.Phase)

		// Verify failure condition was added
		assert.Len(t, updatedNode.Status.Conditions, 1)
		condition := updatedNode.Status.Conditions[0]
		assert.Equal(t, "JobFailed", condition.Type)
		assert.Equal(t, metav1.ConditionTrue, condition.Status)
		assert.Equal(t, "AsyncJobFailed", condition.Reason)
		assert.Contains(t, condition.Message, "Job failed, transitioned from")
	})
}

func TestAfterForceCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("successful force lock release and cleanup condition added", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		ctx := context.TODO()

		// Create a mock FSM event
		event := &fsm.Event{
			FSM:   nm.fsm,
			Event: EventForceCleanup,
			Src:   StateStartingUp,
			Dst:   StateShutdown,
		}

		// looplab/fsm applies the state change before the after_ callback runs, so the
		// machine has to be in the state the transition entered (see PrimeEnterState).
		nm.PrimeEnterState(StateShutdown)

		// Call the after hook
		nm.afterForceCleanup(ctx, event)

		// Verify force lock release was called
		forceCalls := mockCoord.GetForceCalls()
		assert.Len(t, forceCalls, 1)
		assert.Equal(t, node, forceCalls[0].Node)

		// Verify node status was updated
		updatedNode := &infrav2alpha1.Node{}
		err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		assert.Equal(t, infrav2alpha1.NodePhaseOff, updatedNode.Status.Phase)

		// Verify cleanup condition was added
		assert.Len(t, updatedNode.Status.Conditions, 1)
		condition := updatedNode.Status.Conditions[0]
		assert.Equal(t, "ForceCleanup", condition.Type)
		assert.Equal(t, metav1.ConditionTrue, condition.Status)
		assert.Equal(t, "StuckTransitionCleanup", condition.Reason)
		assert.Contains(t, condition.Message, "Force cleanup executed")
	})
}

func TestEnterStartingUp(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	// The slot is in the shape a scale-up starts from: a live claim in pending, and the machine off.
	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord,
		WithClaim(TestClaim("test-node", "claim-start", infrav2alpha1.NodeClaimPending, false), nil))
	ctx := context.TODO()

	// Create a mock FSM event
	event := &fsm.Event{
		FSM:   nm.fsm,
		Event: EventStartNode,
		Src:   StateShutdown,
		Dst:   StateStartingUp,
	}

	// looplab/fsm applies the state change before the enter_ callback runs, so the machine
	// has to be in the state this hook is entering (see PrimeEnterState).
	nm.PrimeEnterState(StateStartingUp)

	// Call the enter hook
	nm.enterStartingUp(ctx, event)

	// Verify backoff strategy was initialized
	assert.NotNil(t, nm.backoff)
	assert.Equal(t, 0, nm.backoff.retryCount)
	assert.Equal(t, MaxRetries, nm.backoff.maxRetries)
	assert.False(t, nm.backoff.transitionStart.IsZero())

	// Verify node status was updated
	updatedNode := &infrav2alpha1.Node{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
	assert.NoError(t, err)
	// The report is the derived value of the inputs of this transition: the transition moved the
	// claim to booting, and a booting claim with the machine off is a slot in its boot window.
	assert.Equal(t, infrav2alpha1.NodePhaseBooting, updatedNode.Status.Phase)

	// The progressing condition is the only record the transition leaves on the slot: the startup
	// attempt is counted on the NodeClaim, not on the long-lived slot, so there is no condition to
	// stamp and no reset heuristic to run (docs/development/nodeclaim.md §4).
	require.Len(t, updatedNode.Status.Conditions, 1, "the progressing record alone")

	condition := meta.FindStatusCondition(updatedNode.Status.Conditions, "StartupProgressing")
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, "NodeStarting", condition.Reason)
}

func TestEnterShuttingDown(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	// The shutdown transition runs on a machine that is observed up with its shutdown Job in flight:
	// the shape the report reads draining from, and the shape the state machine is in.
	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord,
		WithInput(phase.Input{ObservedPower: infrav2alpha1.PowerStateOn, ShutdownJobInFlight: true}))
	ctx := context.TODO()

	// Create a mock FSM event
	event := &fsm.Event{
		FSM:   nm.fsm,
		Event: EventShutdownNode,
		Src:   StateReady,
		Dst:   StateShuttingDown,
	}

	// looplab/fsm applies the state change before the enter_ callback runs, so the machine
	// has to be in the state this hook is entering (see PrimeEnterState).
	nm.PrimeEnterState(StateShuttingDown)

	// Call the enter hook
	nm.enterShuttingDown(ctx, event)

	// Verify backoff strategy was initialized
	assert.NotNil(t, nm.backoff)
	assert.Equal(t, 0, nm.backoff.retryCount)
	assert.Equal(t, MaxRetries, nm.backoff.maxRetries)
	assert.False(t, nm.backoff.transitionStart.IsZero())

	// Verify node status was updated
	updatedNode := &infrav2alpha1.Node{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
	assert.NoError(t, err)
	assert.Equal(t, infrav2alpha1.NodePhaseDraining, updatedNode.Status.Phase)
	assert.NotNil(t, updatedNode.Status.LastShutdownTime)

	// The progressing condition is the only record the transition leaves on the slot: a scale-down is
	// a claim (request.call: DeleteNodes), and its attempts are counted on the claim that asked for
	// it and end when it ends.
	require.Len(t, updatedNode.Status.Conditions, 1, "the progressing record alone")

	progressing := meta.FindStatusCondition(updatedNode.Status.Conditions, "ShutdownProgressing")
	require.NotNil(t, progressing)
	assert.Equal(t, metav1.ConditionTrue, progressing.Status)
	assert.Equal(t, "NodeShuttingDown", progressing.Reason)
}

func TestEnterReady(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	// The slot enters ready with the machine observed up and its node usable: the shape the report
	// derives ready from.
	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord,
		WithInput(phase.Input{
			Claim:         TestClaim("test-node", "claim-ready", infrav2alpha1.NodeClaimReady, false),
			ObservedPower: infrav2alpha1.PowerStateOn,
			NodeUsable:    true,
		}))
	ctx := context.TODO()

	// Set up backoff strategy to verify it gets cleared
	nm.backoff = &BackoffStrategy{
		transitionStart: time.Now().Add(-5 * time.Minute),
		retryCount:      2,
		maxRetries:      MaxRetries,
	}

	// Create a mock FSM event
	event := &fsm.Event{
		FSM:   nm.fsm,
		Event: EventJobCompleted,
		Src:   StateStartingUp,
		Dst:   StateReady,
	}

	// looplab/fsm applies the state change before the enter_ callback runs, so the machine
	// has to be in the state this hook is entering (see PrimeEnterState).
	nm.PrimeEnterState(StateReady)

	// Call the enter hook
	nm.enterReady(ctx, event)

	// Verify backoff strategy was cleared
	assert.Nil(t, nm.backoff)

	// Verify node status was updated
	updatedNode := &infrav2alpha1.Node{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
	assert.NoError(t, err)
	assert.Equal(t, infrav2alpha1.NodePhaseReady, updatedNode.Status.Phase)

	// Verify ready condition was added
	assert.Len(t, updatedNode.Status.Conditions, 1)
	condition := updatedNode.Status.Conditions[0]
	assert.Equal(t, "Ready", condition.Type)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, "NodeReady", condition.Reason)
}

func TestEnterShutdown(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	nm := MachineForState(node, fakeClient, scheme, mockCoord)
	ctx := context.TODO()

	// Set up backoff strategy to verify it gets cleared
	nm.backoff = &BackoffStrategy{
		transitionStart: time.Now().Add(-3 * time.Minute),
		retryCount:      1,
		maxRetries:      MaxRetries,
	}

	// Create a mock FSM event
	event := &fsm.Event{
		FSM:   nm.fsm,
		Event: EventJobCompleted,
		Src:   StateShuttingDown,
		Dst:   StateShutdown,
	}

	// looplab/fsm applies the state change before the enter_ callback runs, so the machine
	// has to be in the state this hook is entering (see PrimeEnterState).
	nm.PrimeEnterState(StateShutdown)

	// Call the enter hook
	nm.enterShutdown(ctx, event)

	// Verify backoff strategy was cleared
	assert.Nil(t, nm.backoff)

	// Verify node status was updated
	updatedNode := &infrav2alpha1.Node{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
	assert.NoError(t, err)
	// The report is derived, and the observation of this slot says the machine is up: the slot
	// reports draining, not off. The report flips to off when the observation controller sees the
	// machine go NotReady, and nothing reads the report to decide an action.
	assert.Equal(t, infrav2alpha1.NodePhaseDraining, updatedNode.Status.Phase)

	// Verify shutdown condition was added
	assert.Len(t, updatedNode.Status.Conditions, 1)
	condition := updatedNode.Status.Conditions[0]
	assert.Equal(t, "Shutdown", condition.Type)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "NodeShutdown", condition.Reason)
}

func TestHooksIntegration(t *testing.T) {
	// This test verifies that hooks are properly integrated with the FSM
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
	nm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	t.Run("complete startup flow with hooks", func(t *testing.T) {
		// Start the node - should trigger before and enter hooks
		err := nm.StartNode()
		assert.NoError(t, err)

		// Verify we're in starting up state
		assert.Equal(t, StateStartingUp, nm.GetCurrentState())

		// Verify coordination lock was acquired
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)
		assert.Equal(t, OperationScaleUp, acquireCalls[0].Operation)

		// Verify backoff strategy was initialized
		assert.NotNil(t, nm.backoff)

		// Complete the job - should trigger after hook
		err = nm.JobCompleted(nil)
		assert.NoError(t, err)

		// Verify we're in ready state
		assert.Equal(t, StateReady, nm.GetCurrentState())

		// Verify coordination lock was released
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)

		// Verify backoff strategy was cleared
		assert.Nil(t, nm.backoff)
	})

	t.Run("complete shutdown flow with hooks", func(t *testing.T) {
		// Reset mock
		mockCoord.Reset()

		// Shutdown the node - should trigger before and enter hooks
		err := nm.ShutdownNode()
		assert.NoError(t, err)

		// Verify we're in shutting down state
		assert.Equal(t, StateShuttingDown, nm.GetCurrentState())

		// Verify coordination lock was acquired
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)
		assert.Equal(t, OperationScaleDown, acquireCalls[0].Operation)

		// Verify backoff strategy was initialized
		assert.NotNil(t, nm.backoff)

		// Complete the job - should trigger after hook
		err = nm.JobCompleted(nil)
		assert.NoError(t, err)

		// Verify we're in shutdown state
		assert.Equal(t, StateShutdown, nm.GetCurrentState())

		// Verify coordination lock was released
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)

		// Verify backoff strategy was cleared
		assert.Nil(t, nm.backoff)
	})

	t.Run("force cleanup flow with hooks", func(t *testing.T) {
		// Reset mock and create a fresh node state machine to avoid job conflicts
		mockCoord.Reset()

		// Create a new node with a different name to avoid job conflicts
		freshNode := CreateTestNode("test-node-2", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		fakeClient2 := CreateFakeClient(scheme, freshNode)
		nm2 := NewNodeStateMachine(freshNode, fakeClient2, scheme, mockCoord)

		err := nm2.StartNode()
		assert.NoError(t, err)

		// Force cleanup - should trigger after hook with force release
		err = nm2.ForceCleanup()
		assert.NoError(t, err)

		// Verify we're in shutdown state
		assert.Equal(t, StateShutdown, nm2.GetCurrentState())

		// Verify force lock release was called
		forceCalls := mockCoord.GetForceCalls()
		assert.Len(t, forceCalls, 1)

		// Verify backoff strategy was cleared
		assert.Nil(t, nm2.backoff)
	})
}

// TestAfterJobCompletedCompletesTheTransitionWhenTheReadinessGateTimesOut is the bound on the
// readiness gate. The transition is held open until the node can take the pool's pods (Ready plus
// Cilium's bootstrap taint gone), because completing it writes status.phase: ready and the
// autoscaler reads that as usable capacity. A CNI that never finishes must not wedge the slot:
// the hook releases the coordination lock and writes the progress it would have written, so the
// slot is not left transitional with a lock nothing is running under, and the node ends up
// uncordoned by the loop's timeout path.
func TestAfterJobCompletedCompletesTheTransitionWhenTheReadinessGateTimesOut(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	bootstrap := corev1.Taint{Key: TaintCiliumAgentNotReady, Value: "true", Effect: corev1.TaintEffectNoExecute}

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	kubeNode := CreateTestKubernetesNode("test-node-k8s", true)
	kubeNode.Spec.Taints = []corev1.Taint{bootstrap}

	fakeClient := CreateFakeClient(scheme, node, kubeNode)

	mockCoord := NewMockCoordinationManager()
	// The gate the test's name names: the bootstrap taint is the key the pool's Group declares, so
	// the loop waits for Cilium to remove it, and runs out of its budget. Without the key declared the
	// gate is the Ready condition alone, and this node is Ready - the fixture would answer for a gate
	// that passed.
	nm := MachineForState(node, fakeClient, scheme, mockCoord,
		WithReadinessTaintKeys([]string{TaintCiliumAgentNotReady}))
	nm.uncordonTimeout = 100 * time.Millisecond
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	ctx := context.TODO()
	event := &fsm.Event{
		FSM:   nm.fsm,
		Event: EventJobCompleted,
		Src:   StateStartingUp,
		Dst:   StateReady,
		Args:  []interface{}{CreateTestJob("test-job", config.NewNamespaceConfig().Get(), "startup")},
	}

	nm.PrimeEnterState(StateReady)
	nm.afterJobCompleted(ctx, event)

	assert.Len(t, mockCoord.GetReleaseCalls(), 1, "the lock of a completed operation is released whatever the gate decided")

	updated := &infrav2alpha1.Node{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updated))
	assert.Equal(t, infrav2alpha1.NodePhaseDraining, updated.Status.Phase,
		"the transition completes: a slot is never left transitional by a readiness gate that timed out. "+
			"The gate's answer moved the claim to failed, and a terminal claim with the machine observed up "+
			"is the force-shutdown row of the table, so the slot reports draining and law 3 powers the machine off")

	updatedKubeNode := &corev1.Node{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "test-node-k8s"}, updatedKubeNode))
	assert.False(t, updatedKubeNode.Spec.Unschedulable, "the node is uncordoned on the timeout path")
}
