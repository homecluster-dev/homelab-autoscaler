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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

func TestNewNodeStateMachine(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	tests := []struct {
		name          string
		nodeProgress  infrav1alpha1.Progress
		expectedState string
	}{
		{
			name:          "shutdown node creates FSM in shutdown state",
			nodeProgress:  infrav1alpha1.ProgressShutdown,
			expectedState: StateShutdown,
		},
		{
			name:          "starting up node creates FSM in startingup state",
			nodeProgress:  infrav1alpha1.ProgressStartingUp,
			expectedState: StateStartingUp,
		},
		{
			name:          "ready node creates FSM in ready state",
			nodeProgress:  infrav1alpha1.ProgressReady,
			expectedState: StateReady,
		},
		{
			name:          "shutting down node creates FSM in shuttingdown state",
			nodeProgress:  infrav1alpha1.ProgressShuttingDown,
			expectedState: StateShuttingDown,
		},
		{
			name:          "empty progress defaults to shutdown state",
			nodeProgress:  "",
			expectedState: StateShutdown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, tt.nodeProgress)
			mockCoord := NewMockCoordinationManager()
			fakeClient := CreateFakeClient(scheme, node)

			fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

			assert.NotNil(t, fsm)
			assert.Equal(t, tt.expectedState, fsm.GetCurrentState())
			assert.Equal(t, node, fsm.node)
			assert.Equal(t, fakeClient, fsm.client)
			assert.Equal(t, scheme, fsm.scheme)
			assert.Equal(t, mockCoord, fsm.coordinationManager)
			assert.Equal(t, DefaultJobTimeout, fsm.jobTimeout)
		})
	}
}

func TestNodeStateMachine_CanTransition(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	tests := []struct {
		name          string
		currentState  infrav1alpha1.Progress
		event         string
		canTransition bool
	}{
		// Valid transitions
		{
			name:          "can start node from shutdown",
			currentState:  infrav1alpha1.ProgressShutdown,
			event:         EventStartNode,
			canTransition: true,
		},
		{
			name:          "can shutdown node from ready",
			currentState:  infrav1alpha1.ProgressReady,
			event:         EventShutdownNode,
			canTransition: true,
		},
		{
			name:          "can complete job from starting up",
			currentState:  infrav1alpha1.ProgressStartingUp,
			event:         EventJobCompleted,
			canTransition: true,
		},
		{
			name:          "can complete job from shutting down",
			currentState:  infrav1alpha1.ProgressShuttingDown,
			event:         EventJobCompleted,
			canTransition: true,
		},
		{
			name:          "can fail job from starting up",
			currentState:  infrav1alpha1.ProgressStartingUp,
			event:         EventJobFailed,
			canTransition: true,
		},
		{
			name:          "can fail job from shutting down",
			currentState:  infrav1alpha1.ProgressShuttingDown,
			event:         EventJobFailed,
			canTransition: true,
		},
		{
			name:          "can timeout from starting up",
			currentState:  infrav1alpha1.ProgressStartingUp,
			event:         EventJobTimeout,
			canTransition: true,
		},
		{
			name:          "can timeout from shutting down",
			currentState:  infrav1alpha1.ProgressShuttingDown,
			event:         EventJobTimeout,
			canTransition: true,
		},
		{
			name:          "can force cleanup from starting up",
			currentState:  infrav1alpha1.ProgressStartingUp,
			event:         EventForceCleanup,
			canTransition: true,
		},
		{
			name:          "can force cleanup from shutting down",
			currentState:  infrav1alpha1.ProgressShuttingDown,
			event:         EventForceCleanup,
			canTransition: true,
		},

		// Invalid transitions
		{
			name:          "cannot start node from ready",
			currentState:  infrav1alpha1.ProgressReady,
			event:         EventStartNode,
			canTransition: false,
		},
		{
			name:          "cannot shutdown node from shutdown",
			currentState:  infrav1alpha1.ProgressShutdown,
			event:         EventShutdownNode,
			canTransition: false,
		},
		{
			name:          "cannot complete job from ready",
			currentState:  infrav1alpha1.ProgressReady,
			event:         EventJobCompleted,
			canTransition: false,
		},
		{
			name:          "cannot complete job from shutdown",
			currentState:  infrav1alpha1.ProgressShutdown,
			event:         EventJobCompleted,
			canTransition: false,
		},
		{
			name:          "cannot force cleanup from ready",
			currentState:  infrav1alpha1.ProgressReady,
			event:         EventForceCleanup,
			canTransition: false,
		},
		{
			name:          "cannot force cleanup from shutdown",
			currentState:  infrav1alpha1.ProgressShutdown,
			event:         EventForceCleanup,
			canTransition: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, tt.currentState)
			mockCoord := NewMockCoordinationManager()
			fakeClient := CreateFakeClient(scheme, node)

			fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

			assert.Equal(t, tt.canTransition, fsm.CanTransition(tt.event))
		})
	}
}

func TestNodeStateMachine_StartNode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("successful node start", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		err := fsm.StartNode()
		assert.NoError(t, err)

		// Verify FSM state changed
		assert.Equal(t, StateStartingUp, fsm.GetCurrentState())

		// Verify coordination lock was acquired
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)
		assert.Equal(t, OperationScaleUp, acquireCalls[0].Operation)
		assert.Equal(t, DefaultLockTimeout, acquireCalls[0].Timeout)

		// Verify job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Contains(t, job.Name, "test-node-startup-")
		assert.Equal(t, "homelab-autoscaler-system", job.Namespace)
		assert.Equal(t, "startup", job.Labels["type"])
		assert.Equal(t, "test/startup:latest", job.Spec.Template.Spec.Containers[0].Image)

		// Verify job tracking
		assert.Equal(t, job.Name, fsm.currentJob)
		assert.False(t, fsm.jobStartTime.IsZero())
	})

	t.Run("coordination lock failure", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		mockCoord := NewMockCoordinationManager()
		mockCoord.SetAcquireError(assert.AnError)
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		err := fsm.StartNode()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "FSM transition failed")

		// Verify FSM state didn't change
		assert.Equal(t, StateShutdown, fsm.GetCurrentState())

		// Verify no job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})

	t.Run("invalid state transition", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		err := fsm.StartNode()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "FSM transition failed")

		// Verify FSM state didn't change
		assert.Equal(t, StateReady, fsm.GetCurrentState())

		// Verify no coordination calls were made
		assert.Len(t, mockCoord.GetAcquireCalls(), 0)
	})
}

func TestNodeStateMachine_ShutdownNode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("successful node shutdown with kubernetes node cordoning", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
		kubeNode := CreateTestKubernetesNode("test-node-k8s", false) // Initially schedulable
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node, kubeNode)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		err := fsm.ShutdownNode()
		assert.NoError(t, err)

		// Verify FSM state changed
		assert.Equal(t, StateShuttingDown, fsm.GetCurrentState())

		// Verify coordination lock was acquired
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)
		assert.Equal(t, OperationScaleDown, acquireCalls[0].Operation)
		assert.Equal(t, DefaultLockTimeout, acquireCalls[0].Timeout)

		// Verify Kubernetes node was cordoned
		updatedKubeNode := &corev1.Node{}
		err = fakeClient.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updatedKubeNode)
		assert.NoError(t, err)
		assert.True(t, updatedKubeNode.Spec.Unschedulable, "Kubernetes node should be cordoned during shutdown")

		// Verify job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Contains(t, job.Name, "test-node-shutdown-")
		assert.Equal(t, "homelab-autoscaler-system", job.Namespace)
		assert.Equal(t, "shutdown", job.Labels["type"])
		assert.Equal(t, "test/shutdown:latest", job.Spec.Template.Spec.Containers[0].Image)

		// Verify job tracking
		assert.Equal(t, job.Name, fsm.currentJob)
		assert.False(t, fsm.jobStartTime.IsZero())
	})

	t.Run("successful node shutdown without kubernetes node", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node) // No Kubernetes node

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		err := fsm.ShutdownNode()
		assert.NoError(t, err) // Should not fail even if Kubernetes node doesn't exist

		// Verify FSM state changed
		assert.Equal(t, StateShuttingDown, fsm.GetCurrentState())

		// Verify job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)
	})

	t.Run("coordination lock failure", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
		mockCoord := NewMockCoordinationManager()
		mockCoord.SetAcquireError(assert.AnError)
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		err := fsm.ShutdownNode()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "FSM transition failed")

		// Verify FSM state didn't change
		assert.Equal(t, StateReady, fsm.GetCurrentState())

		// Verify no job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})
}

func TestNodeStateMachine_JobEvents(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("startup job completed uncordons kubernetes node", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
		kubeNode := CreateTestKubernetesNode("test-node-k8s", true) // Initially unschedulable
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node, kubeNode)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		err := fsm.JobCompleted(nil)
		assert.NoError(t, err)

		// Verify FSM state changed to Ready
		assert.Equal(t, StateReady, fsm.GetCurrentState())

		// Verify Kubernetes node was uncordoned
		updatedKubeNode := &corev1.Node{}
		err = fakeClient.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updatedKubeNode)
		assert.NoError(t, err)
		assert.False(t, updatedKubeNode.Spec.Unschedulable, "Kubernetes node should be uncordoned after startup completion")

		// Verify coordination lock was released
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)
	})

	t.Run("shutdown job completed does not uncordon kubernetes node", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown)
		kubeNode := CreateTestKubernetesNode("test-node-k8s", true) // Initially unschedulable
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node, kubeNode)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		err := fsm.JobCompleted(nil)
		assert.NoError(t, err)

		// Verify FSM state changed to Shutdown
		assert.Equal(t, StateShutdown, fsm.GetCurrentState())

		// Verify Kubernetes node remains cordoned
		updatedKubeNode := &corev1.Node{}
		err = fakeClient.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updatedKubeNode)
		assert.NoError(t, err)
		assert.True(t, updatedKubeNode.Spec.Unschedulable, "Kubernetes node should remain cordoned after shutdown completion")
	})

	tests := []struct {
		name          string
		initialState  infrav1alpha1.Progress
		event         func(*NodeStateMachine) error
		expectedState string
	}{
		{
			name:         "job completed from starting up",
			initialState: infrav1alpha1.ProgressStartingUp,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobCompleted(nil)
			},
			expectedState: StateReady,
		},
		{
			name:         "job completed from shutting down",
			initialState: infrav1alpha1.ProgressShuttingDown,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobCompleted(nil)
			},
			expectedState: StateShutdown,
		},
		{
			name:         "job failed from starting up",
			initialState: infrav1alpha1.ProgressStartingUp,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobFailed()
			},
			expectedState: StateShutdown,
		},
		{
			name:         "job failed from shutting down",
			initialState: infrav1alpha1.ProgressShuttingDown,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobFailed()
			},
			expectedState: StateReady,
		},
		{
			name:         "job timeout from starting up",
			initialState: infrav1alpha1.ProgressStartingUp,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobTimeout()
			},
			expectedState: StateShutdown, // Timeout from startup goes to shutdown
		},
		{
			name:         "job timeout from shutting down",
			initialState: infrav1alpha1.ProgressShuttingDown,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobTimeout()
			},
			expectedState: StateReady, // Timeout from shutdown goes to ready
		},
		{
			name:         "force cleanup from starting up",
			initialState: infrav1alpha1.ProgressStartingUp,
			event: func(fsm *NodeStateMachine) error {
				return fsm.ForceCleanup()
			},
			expectedState: StateShutdown,
		},
		{
			name:         "force cleanup from shutting down",
			initialState: infrav1alpha1.ProgressShuttingDown,
			event: func(fsm *NodeStateMachine) error {
				return fsm.ForceCleanup()
			},
			expectedState: StateShutdown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, tt.initialState)
			mockCoord := NewMockCoordinationManager()
			fakeClient := CreateFakeClient(scheme, node)

			fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

			err := tt.event(fsm)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedState, fsm.GetCurrentState())
		})
	}
}

func TestNodeStateMachine_CalculateBackoff(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

	t.Run("no backoff strategy returns default", func(t *testing.T) {
		backoff := fsm.CalculateBackoff()
		assert.Equal(t, time.Minute, backoff)
	})

	t.Run("early phase backoff", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-1 * time.Minute),
			retryCount:      0,
			maxRetries:      MaxRetries,
		}

		backoff := fsm.CalculateBackoff()
		assert.Equal(t, 30*time.Second, backoff)
	})

	t.Run("normal phase backoff", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-5 * time.Minute),
			retryCount:      1,
			maxRetries:      MaxRetries,
		}

		backoff := fsm.CalculateBackoff()
		assert.Equal(t, 2*time.Minute, backoff)
	})

	t.Run("late phase backoff", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-12 * time.Minute),
			retryCount:      2,
			maxRetries:      MaxRetries,
		}

		backoff := fsm.CalculateBackoff()
		assert.Equal(t, 5*time.Minute, backoff)
	})

	t.Run("stuck transition triggers force cleanup", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-20 * time.Minute),
			retryCount:      3,
			maxRetries:      MaxRetries,
		}

		backoff := fsm.CalculateBackoff()
		assert.Equal(t, time.Duration(0), backoff)
		// Note: ForceCleanup would be called but we can't easily test that here
	})
}

func TestNodeStateMachine_IsTransitionStuck(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

	t.Run("no backoff strategy is not stuck", func(t *testing.T) {
		assert.False(t, fsm.IsTransitionStuck())
	})

	t.Run("recent transition is not stuck", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-5 * time.Minute),
			retryCount:      1,
			maxRetries:      MaxRetries,
		}

		assert.False(t, fsm.IsTransitionStuck())
	})

	t.Run("old transition is stuck", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-20 * time.Minute),
			retryCount:      3,
			maxRetries:      MaxRetries,
		}

		assert.True(t, fsm.IsTransitionStuck())
	})
}

func TestNodeStateMachine_MonitorJobCompletion(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("job completes successfully", func(t *testing.T) {
		node := CreateTestNode("test-node-monitor-1", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		// Create a job that will be marked as successful
		job := CreateTestJob("test-job-1", config.NewNamespaceConfig().Get(), "startup")
		job.Status.Succeeded = 1
		err := fakeClient.Create(context.TODO(), job)
		require.NoError(t, err)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.jobTimeout = 2 * time.Second // Short timeout for testing

		// Start monitoring in a goroutine
		done := make(chan bool)
		go func() {
			fsm.monitorJobCompletion("test-job-1")
			done <- true
		}()

		// Wait for monitoring to complete
		select {
		case <-done:
			// Success - job monitoring completed
		case <-time.After(5 * time.Second):
			t.Fatal("Job monitoring did not complete in time")
		}

		// Give a small delay for the FSM event to process
		time.Sleep(100 * time.Millisecond)

		// Verify FSM transitioned to ready state
		assert.Equal(t, StateReady, fsm.GetCurrentState())
	})

	t.Run("job fails", func(t *testing.T) {
		node := CreateTestNode("test-node-monitor-2", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		// Create a job that will be marked as failed
		job := CreateTestJob("test-job-2", config.NewNamespaceConfig().Get(), "startup")
		job.Status.Failed = 1
		err := fakeClient.Create(context.TODO(), job)
		require.NoError(t, err)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.jobTimeout = 2 * time.Second // Short timeout for testing

		// Start monitoring in a goroutine
		done := make(chan bool)
		go func() {
			fsm.monitorJobCompletion("test-job-2")
			done <- true
		}()

		// Wait for monitoring to complete
		select {
		case <-done:
			// Success - job monitoring completed
		case <-time.After(5 * time.Second):
			t.Fatal("Job monitoring did not complete in time")
		}

		// Give a small delay for the FSM event to process
		time.Sleep(100 * time.Millisecond)

		// Verify FSM transitioned to shutdown state (failure from starting up)
		assert.Equal(t, StateShutdown, fsm.GetCurrentState())
	})

	t.Run("job times out", func(t *testing.T) {
		node := CreateTestNode("test-node-monitor-3", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		// Create a job that remains running
		job := CreateTestJob("test-job-3", config.NewNamespaceConfig().Get(), "startup")
		job.Status.Active = 1
		err := fakeClient.Create(context.TODO(), job)
		require.NoError(t, err)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.jobTimeout = 100 * time.Millisecond // Very short timeout for testing

		// Start monitoring in a goroutine
		done := make(chan bool)
		go func() {
			fsm.monitorJobCompletion("test-job-3")
			done <- true
		}()

		// Wait for monitoring to complete
		select {
		case <-done:
			// Success - job monitoring completed with timeout
		case <-time.After(2 * time.Second):
			t.Fatal("Job monitoring did not complete in time")
		}

		// Give a small delay for the FSM event to process
		time.Sleep(100 * time.Millisecond)

		// Verify FSM transitioned to shutdown state (timeout from starting up goes to shutdown)
		assert.Equal(t, StateShutdown, fsm.GetCurrentState())
	})
}

func TestNodeStateMachine_CleanupPendingJobs(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("cleanup multiple pending jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		mockCoord := NewMockCoordinationManager()

		// Create multiple pending jobs for the same node
		pendingStartupJob := CreateTestJobWithNodeLabel("test-node-startup-old", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		pendingShutdownJob := CreateTestJobWithNodeLabel("test-node-shutdown-old", config.NewNamespaceConfig().Get(), "shutdown", "test-node-k8s")

		// Create a completed job that should not be deleted
		completedJob := CreateTestJobWithNodeLabel("test-node-startup-completed", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		completedJob.Status.Succeeded = 1

		// Create a job for a different node that should not be deleted
		otherNodeJob := CreateTestJobWithNodeLabel("other-node-startup", config.NewNamespaceConfig().Get(), "startup", "other-node-k8s")

		fakeClient := CreateFakeClient(scheme, node, pendingStartupJob, pendingShutdownJob, completedJob, otherNodeJob)
		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		// Call cleanup
		err := fsm.cleanupPendingJobs("test-node-k8s")
		assert.NoError(t, err)

		// Verify pending jobs were deleted
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)

		// Should have 2 jobs remaining: completed job and other node job
		assert.Len(t, jobs.Items, 2)

		jobNames := make(map[string]bool)
		for _, job := range jobs.Items {
			jobNames[job.Name] = true
		}

		assert.True(t, jobNames["test-node-startup-completed"], "Completed job should not be deleted")
		assert.True(t, jobNames["other-node-startup"], "Other node job should not be deleted")
		assert.False(t, jobNames["test-node-startup-old"], "Pending startup job should be deleted")
		assert.False(t, jobNames["test-node-shutdown-old"], "Pending shutdown job should be deleted")
	})

	t.Run("cleanup with no pending jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		// Call cleanup when no jobs exist
		err := fsm.cleanupPendingJobs("test-node-k8s")
		assert.NoError(t, err)

		// Verify no jobs exist
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})

	t.Run("cleanup with empty kubernetes node name", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		node.Spec.KubernetesNodeName = "" // Empty node name
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		// Call cleanup with empty node name
		err := fsm.cleanupPendingJobs("")
		assert.NoError(t, err) // Should not error
	})

	t.Run("cleanup only affects startup and shutdown jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		mockCoord := NewMockCoordinationManager()

		// Create jobs with different types
		startupJob := CreateTestJobWithNodeLabel("test-node-startup", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		shutdownJob := CreateTestJobWithNodeLabel("test-node-shutdown", config.NewNamespaceConfig().Get(), "shutdown", "test-node-k8s")

		// Create a job with different type that should not be deleted
		otherTypeJob := CreateTestJobWithNodeLabel("test-node-other", config.NewNamespaceConfig().Get(), "maintenance", "test-node-k8s")

		fakeClient := CreateFakeClient(scheme, node, startupJob, shutdownJob, otherTypeJob)
		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		// Call cleanup
		err := fsm.cleanupPendingJobs("test-node-k8s")
		assert.NoError(t, err)

		// Verify only startup/shutdown jobs were deleted
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)

		assert.Len(t, jobs.Items, 1)
		assert.Equal(t, "test-node-other", jobs.Items[0].Name)
		assert.Equal(t, "maintenance", jobs.Items[0].Labels["type"])
	})
}

func TestNodeStateMachine_StartNodeWithCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("startup cleans up existing pending jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		mockCoord := NewMockCoordinationManager()

		// Create existing pending jobs
		oldStartupJob := CreateTestJobWithNodeLabel("test-node-startup-old", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		oldShutdownJob := CreateTestJobWithNodeLabel("test-node-shutdown-old", config.NewNamespaceConfig().Get(), "shutdown", "test-node-k8s")

		fakeClient := CreateFakeClient(scheme, node, oldStartupJob, oldShutdownJob)
		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		// Start node
		err := fsm.StartNode()
		assert.NoError(t, err)

		// Verify old jobs were cleaned up and new job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)

		// Should have only 1 job (the new startup job)
		assert.Len(t, jobs.Items, 1)

		newJob := jobs.Items[0]
		assert.Contains(t, newJob.Name, "test-node-startup-")
		assert.Equal(t, "startup", newJob.Labels["type"])
		assert.NotEqual(t, "test-node-startup-old", newJob.Name) // Should be a new job
	})
}

func TestNodeStateMachine_ShutdownNodeWithCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("shutdown cleans up existing pending jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
		mockCoord := NewMockCoordinationManager()

		// Create existing pending jobs
		oldStartupJob := CreateTestJobWithNodeLabel("test-node-startup-old", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		oldShutdownJob := CreateTestJobWithNodeLabel("test-node-shutdown-old", config.NewNamespaceConfig().Get(), "shutdown", "test-node-k8s")

		fakeClient := CreateFakeClient(scheme, node, oldStartupJob, oldShutdownJob)
		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

		// Shutdown node
		err := fsm.ShutdownNode()
		assert.NoError(t, err)

		// Verify old jobs were cleaned up and new job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)

		// Should have only 1 job (the new shutdown job)
		assert.Len(t, jobs.Items, 1)

		newJob := jobs.Items[0]
		assert.Contains(t, newJob.Name, "test-node-shutdown-")
		assert.Equal(t, "shutdown", newJob.Labels["type"])
		assert.NotEqual(t, "test-node-shutdown-old", newJob.Name) // Should be a new job
	})
}
