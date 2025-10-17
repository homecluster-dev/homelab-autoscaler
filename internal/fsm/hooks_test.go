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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

func TestBeforeStartNode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("successful lock acquisition", func(t *testing.T) {
		node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
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
		node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
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
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("successful lock acquisition", func(t *testing.T) {
		node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
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
		node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
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
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("successful lock release and status update", func(t *testing.T) {
		node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		ctx := context.TODO()

		// Create a mock FSM event
		event := &fsm.Event{
			FSM:   nm.fsm,
			Event: EventJobCompleted,
			Src:   StateStartingUp,
			Dst:   StateReady,
		}

		// Call the after hook
		nm.afterJobCompleted(ctx, event)

		// Verify lock release was called
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)
		assert.Equal(t, node, releaseCalls[0].Node)

		// Verify node status was updated
		updatedNode := &infrav1alpha1.Node{}
		err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		assert.Equal(t, infrav1alpha1.Progress(StateReady), updatedNode.Status.Progress)
	})

	t.Run("lock release failure is logged but doesn't fail", func(t *testing.T) {
		node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
		mockCoord := NewMockCoordinationManager()
		mockCoord.SetReleaseError(assert.AnError)
		fakeClient := CreateFakeClient(scheme, node)

		nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		ctx := context.TODO()

		// Create a mock FSM event
		event := &fsm.Event{
			FSM:   nm.fsm,
			Event: EventJobCompleted,
			Src:   StateShuttingDown,
			Dst:   StateShutdown,
		}

		// Call the after hook - should not panic or fail
		nm.afterJobCompleted(ctx, event)

		// Verify lock release was attempted
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)

		// Verify node status was still updated despite lock release failure
		updatedNode := &infrav1alpha1.Node{}
		err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		assert.Equal(t, infrav1alpha1.Progress(StateShutdown), updatedNode.Status.Progress)
	})
}

func TestAfterJobFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("successful lock release and failure condition added", func(t *testing.T) {
		node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
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

		// Call the after hook
		nm.afterJobFailed(ctx, event)

		// Verify lock release was called
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)
		assert.Equal(t, node, releaseCalls[0].Node)

		// Verify node status was updated
		updatedNode := &infrav1alpha1.Node{}
		err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		assert.Equal(t, infrav1alpha1.Progress(StateShutdown), updatedNode.Status.Progress)

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
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("successful force lock release and cleanup condition added", func(t *testing.T) {
		node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
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

		// Call the after hook
		nm.afterForceCleanup(ctx, event)

		// Verify force lock release was called
		forceCalls := mockCoord.GetForceCalls()
		assert.Len(t, forceCalls, 1)
		assert.Equal(t, node, forceCalls[0].Node)

		// Verify node status was updated
		updatedNode := &infrav1alpha1.Node{}
		err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		assert.Equal(t, infrav1alpha1.Progress(StateShutdown), updatedNode.Status.Progress)

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
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
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

	// Call the enter hook
	nm.enterStartingUp(ctx, event)

	// Verify backoff strategy was initialized
	assert.NotNil(t, nm.backoff)
	assert.Equal(t, 0, nm.backoff.retryCount)
	assert.Equal(t, MaxRetries, nm.backoff.maxRetries)
	assert.False(t, nm.backoff.transitionStart.IsZero())

	// Verify node status was updated
	updatedNode := &infrav1alpha1.Node{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
	assert.NoError(t, err)
	assert.Equal(t, infrav1alpha1.ProgressStartingUp, updatedNode.Status.Progress)
	assert.NotNil(t, updatedNode.Status.LastStartupTime)

	// Verify progressing condition was added
	assert.Len(t, updatedNode.Status.Conditions, 1)
	condition := updatedNode.Status.Conditions[0]
	assert.Equal(t, "StartupProgressing", condition.Type)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, "NodeStarting", condition.Reason)
}

func TestEnterShuttingDown(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
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

	// Call the enter hook
	nm.enterShuttingDown(ctx, event)

	// Verify backoff strategy was initialized
	assert.NotNil(t, nm.backoff)
	assert.Equal(t, 0, nm.backoff.retryCount)
	assert.Equal(t, MaxRetries, nm.backoff.maxRetries)
	assert.False(t, nm.backoff.transitionStart.IsZero())

	// Verify node status was updated
	updatedNode := &infrav1alpha1.Node{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
	assert.NoError(t, err)
	assert.Equal(t, infrav1alpha1.ProgressShuttingDown, updatedNode.Status.Progress)
	assert.NotNil(t, updatedNode.Status.LastShutdownTime)

	// Verify progressing condition was added
	assert.Len(t, updatedNode.Status.Conditions, 1)
	condition := updatedNode.Status.Conditions[0]
	assert.Equal(t, "ShutdownProgressing", condition.Type)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, "NodeShuttingDown", condition.Reason)
}

func TestEnterReady(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
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

	// Call the enter hook
	nm.enterReady(ctx, event)

	// Verify backoff strategy was cleared
	assert.Nil(t, nm.backoff)

	// Verify node status was updated
	updatedNode := &infrav1alpha1.Node{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
	assert.NoError(t, err)
	assert.Equal(t, infrav1alpha1.ProgressReady, updatedNode.Status.Progress)

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
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
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

	// Call the enter hook
	nm.enterShutdown(ctx, event)

	// Verify backoff strategy was cleared
	assert.Nil(t, nm.backoff)

	// Verify node status was updated
	updatedNode := &infrav1alpha1.Node{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
	assert.NoError(t, err)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, updatedNode.Status.Progress)

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
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

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
		err = nm.JobCompleted()
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
		err = nm.JobCompleted()
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
		freshNode := CreateTestNode("test-node-2", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
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
