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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

func TestGetFSMTransitions(t *testing.T) {
	transitions := GetFSMTransitions()

	// Verify we have the expected number of transitions
	assert.Len(t, transitions, 9) // Based on the actual FSM implementation

	// Create a map for easier lookup
	transitionMap := make(map[string]map[string]string)
	for _, transition := range transitions {
		if transitionMap[transition.Name] == nil {
			transitionMap[transition.Name] = make(map[string]string)
		}
		for _, src := range transition.Src {
			transitionMap[transition.Name][src] = transition.Dst
		}
	}

	// Test StartNode transitions
	assert.Equal(t, StateStartingUp, transitionMap[EventStartNode][StateShutdown])

	// Test ShutdownNode transitions
	assert.Equal(t, StateShuttingDown, transitionMap[EventShutdownNode][StateReady])

	// Test JobCompleted transitions
	assert.Equal(t, StateReady, transitionMap[EventJobCompleted][StateStartingUp])
	assert.Equal(t, StateShutdown, transitionMap[EventJobCompleted][StateShuttingDown])

	// Test JobFailed transitions
	assert.Equal(t, StateShutdown, transitionMap[EventJobFailed][StateStartingUp])
	assert.Equal(t, StateReady, transitionMap[EventJobFailed][StateShuttingDown])

	// Test JobTimeout transitions (timeout goes to different states)
	assert.Equal(t, StateShutdown, transitionMap[EventJobTimeout][StateStartingUp])
	assert.Equal(t, StateReady, transitionMap[EventJobTimeout][StateShuttingDown])

	// Test ForceCleanup transitions
	assert.Equal(t, StateShutdown, transitionMap[EventForceCleanup][StateStartingUp])
	assert.Equal(t, StateShutdown, transitionMap[EventForceCleanup][StateShuttingDown])
}

func TestValidateEventTransition(t *testing.T) {
	tests := []struct {
		name         string
		currentState string
		event        string
		expected     bool
	}{
		// Valid transitions
		{
			name:         "start node from shutdown",
			currentState: StateShutdown,
			event:        EventStartNode,
			expected:     true,
		},
		{
			name:         "shutdown node from ready",
			currentState: StateReady,
			event:        EventShutdownNode,
			expected:     true,
		},
		{
			name:         "job completed from starting up",
			currentState: StateStartingUp,
			event:        EventJobCompleted,
			expected:     true,
		},
		{
			name:         "job completed from shutting down",
			currentState: StateShuttingDown,
			event:        EventJobCompleted,
			expected:     true,
		},
		{
			name:         "job failed from starting up",
			currentState: StateStartingUp,
			event:        EventJobFailed,
			expected:     true,
		},
		{
			name:         "job failed from shutting down",
			currentState: StateShuttingDown,
			event:        EventJobFailed,
			expected:     true,
		},
		{
			name:         "job timeout from starting up",
			currentState: StateStartingUp,
			event:        EventJobTimeout,
			expected:     true,
		},
		{
			name:         "job timeout from shutting down",
			currentState: StateShuttingDown,
			event:        EventJobTimeout,
			expected:     true,
		},
		{
			name:         "force cleanup from starting up",
			currentState: StateStartingUp,
			event:        EventForceCleanup,
			expected:     true,
		},
		{
			name:         "force cleanup from shutting down",
			currentState: StateShuttingDown,
			event:        EventForceCleanup,
			expected:     true,
		},

		// Invalid transitions
		{
			name:         "start node from ready",
			currentState: StateReady,
			event:        EventStartNode,
			expected:     false,
		},
		{
			name:         "start node from starting up",
			currentState: StateStartingUp,
			event:        EventStartNode,
			expected:     false,
		},
		{
			name:         "start node from shutting down",
			currentState: StateShuttingDown,
			event:        EventStartNode,
			expected:     false,
		},
		{
			name:         "shutdown node from shutdown",
			currentState: StateShutdown,
			event:        EventShutdownNode,
			expected:     false,
		},
		{
			name:         "shutdown node from starting up",
			currentState: StateStartingUp,
			event:        EventShutdownNode,
			expected:     false,
		},
		{
			name:         "shutdown node from shutting down",
			currentState: StateShuttingDown,
			event:        EventShutdownNode,
			expected:     false,
		},
		{
			name:         "job completed from ready",
			currentState: StateReady,
			event:        EventJobCompleted,
			expected:     false,
		},
		{
			name:         "job completed from shutdown",
			currentState: StateShutdown,
			event:        EventJobCompleted,
			expected:     false,
		},
		{
			name:         "job failed from ready",
			currentState: StateReady,
			event:        EventJobFailed,
			expected:     false,
		},
		{
			name:         "job failed from shutdown",
			currentState: StateShutdown,
			event:        EventJobFailed,
			expected:     false,
		},
		{
			name:         "job timeout from ready",
			currentState: StateReady,
			event:        EventJobTimeout,
			expected:     false,
		},
		{
			name:         "job timeout from shutdown",
			currentState: StateShutdown,
			event:        EventJobTimeout,
			expected:     false,
		},
		{
			name:         "force cleanup from ready",
			currentState: StateReady,
			event:        EventForceCleanup,
			expected:     false,
		},
		{
			name:         "force cleanup from shutdown",
			currentState: StateShutdown,
			event:        EventForceCleanup,
			expected:     false,
		},
		{
			name:         "invalid event",
			currentState: StateShutdown,
			event:        "InvalidEvent",
			expected:     false,
		},
		{
			name:         "invalid state",
			currentState: "InvalidState",
			event:        EventStartNode,
			expected:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidateEventTransition(tt.currentState, tt.event)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetValidEventsForState(t *testing.T) {
	tests := []struct {
		name           string
		state          string
		expectedEvents []string
	}{
		{
			name:           "shutdown state events",
			state:          StateShutdown,
			expectedEvents: []string{EventStartNode},
		},
		{
			name:           "ready state events",
			state:          StateReady,
			expectedEvents: []string{EventShutdownNode},
		},
		{
			name:  "starting up state events",
			state: StateStartingUp,
			expectedEvents: []string{
				EventJobCompleted,
				EventJobFailed,
				EventJobTimeout,
				EventForceCleanup,
			},
		},
		{
			name:  "shutting down state events",
			state: StateShuttingDown,
			expectedEvents: []string{
				EventJobCompleted,
				EventJobFailed,
				EventJobTimeout,
				EventForceCleanup,
			},
		},
		{
			name:           "invalid state",
			state:          "InvalidState",
			expectedEvents: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := GetValidEventsForState(tt.state)
			assert.ElementsMatch(t, tt.expectedEvents, events)
		})
	}
}

func TestGetDestinationState(t *testing.T) {
	tests := []struct {
		name        string
		srcState    string
		event       string
		expectedDst string
		expectedOk  bool
	}{
		// Valid transitions
		{
			name:        "start node from shutdown",
			srcState:    StateShutdown,
			event:       EventStartNode,
			expectedDst: StateStartingUp,
			expectedOk:  true,
		},
		{
			name:        "shutdown node from ready",
			srcState:    StateReady,
			event:       EventShutdownNode,
			expectedDst: StateShuttingDown,
			expectedOk:  true,
		},
		{
			name:        "job completed from starting up",
			srcState:    StateStartingUp,
			event:       EventJobCompleted,
			expectedDst: StateReady,
			expectedOk:  true,
		},
		{
			name:        "job completed from shutting down",
			srcState:    StateShuttingDown,
			event:       EventJobCompleted,
			expectedDst: StateShutdown,
			expectedOk:  true,
		},
		{
			name:        "job failed from starting up",
			srcState:    StateStartingUp,
			event:       EventJobFailed,
			expectedDst: StateShutdown,
			expectedOk:  true,
		},
		{
			name:        "job failed from shutting down",
			srcState:    StateShuttingDown,
			event:       EventJobFailed,
			expectedDst: StateReady,
			expectedOk:  true,
		},
		{
			name:        "job timeout from starting up",
			srcState:    StateStartingUp,
			event:       EventJobTimeout,
			expectedDst: StateShutdown,
			expectedOk:  true,
		},
		{
			name:        "job timeout from shutting down",
			srcState:    StateShuttingDown,
			event:       EventJobTimeout,
			expectedDst: StateReady,
			expectedOk:  true,
		},
		{
			name:        "force cleanup from starting up",
			srcState:    StateStartingUp,
			event:       EventForceCleanup,
			expectedDst: StateShutdown,
			expectedOk:  true,
		},
		{
			name:        "force cleanup from shutting down",
			srcState:    StateShuttingDown,
			event:       EventForceCleanup,
			expectedDst: StateShutdown,
			expectedOk:  true,
		},

		// Invalid transitions
		{
			name:        "start node from ready",
			srcState:    StateReady,
			event:       EventStartNode,
			expectedDst: "",
			expectedOk:  false,
		},
		{
			name:        "shutdown node from shutdown",
			srcState:    StateShutdown,
			event:       EventShutdownNode,
			expectedDst: "",
			expectedOk:  false,
		},
		{
			name:        "invalid event",
			srcState:    StateShutdown,
			event:       "InvalidEvent",
			expectedDst: "",
			expectedOk:  false,
		},
		{
			name:        "invalid state",
			srcState:    "InvalidState",
			event:       EventStartNode,
			expectedDst: "",
			expectedOk:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst, ok := GetDestinationState(tt.srcState, tt.event)
			assert.Equal(t, tt.expectedDst, dst)
			assert.Equal(t, tt.expectedOk, ok)
		})
	}
}

func TestIsTransitionalState(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		expected bool
	}{
		{
			name:     "starting up is transitional",
			state:    StateStartingUp,
			expected: true,
		},
		{
			name:     "shutting down is transitional",
			state:    StateShuttingDown,
			expected: true,
		},
		{
			name:     "ready is not transitional",
			state:    StateReady,
			expected: false,
		},
		{
			name:     "shutdown is not transitional",
			state:    StateShutdown,
			expected: false,
		},
		{
			name:     "invalid state is not transitional",
			state:    "InvalidState",
			expected: false,
		},
		{
			name:     "empty state is not transitional",
			state:    "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsTransitionalState(tt.state)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsStableState(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		expected bool
	}{
		{
			name:     "ready is stable",
			state:    StateReady,
			expected: true,
		},
		{
			name:     "shutdown is stable",
			state:    StateShutdown,
			expected: true,
		},
		{
			name:     "starting up is not stable",
			state:    StateStartingUp,
			expected: false,
		},
		{
			name:     "shutting down is not stable",
			state:    StateShuttingDown,
			expected: false,
		},
		{
			name:     "invalid state is not stable",
			state:    "InvalidState",
			expected: false,
		},
		{
			name:     "empty state is not stable",
			state:    "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsStableState(tt.state)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetOperationForEvent(t *testing.T) {
	tests := []struct {
		name     string
		event    string
		expected string
	}{
		{
			name:     "start node event maps to scale up",
			event:    EventStartNode,
			expected: OperationScaleUp,
		},
		{
			name:     "shutdown node event maps to scale down",
			event:    EventShutdownNode,
			expected: OperationScaleDown,
		},
		{
			name:     "job completed event has no operation",
			event:    EventJobCompleted,
			expected: "",
		},
		{
			name:     "job failed event has no operation",
			event:    EventJobFailed,
			expected: "",
		},
		{
			name:     "job timeout event has no operation",
			event:    EventJobTimeout,
			expected: "",
		},
		{
			name:     "force cleanup event has no operation",
			event:    EventForceCleanup,
			expected: "",
		},
		{
			name:     "invalid event has no operation",
			event:    "InvalidEvent",
			expected: "",
		},
		{
			name:     "empty event has no operation",
			event:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetOperationForEvent(tt.event)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetFSMCallbacks(t *testing.T) {
	// Create a minimal NodeStateMachine for testing
	node := CreateTestNode("test-node", "default", infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	mockCoord := NewMockCoordinationManager()

	// Set up proper scheme
	scheme := runtime.NewScheme()
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))
	fakeClient := CreateFakeClient(scheme, node)

	fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)

	callbacks := GetFSMCallbacks(fsm)

	// Verify that all expected callbacks are present
	expectedCallbacks := []string{
		"before_" + EventStartNode,
		"before_" + EventShutdownNode,
		"after_" + EventJobCompleted,
		"after_" + EventJobFailed,
		"after_" + EventForceCleanup,
		"enter_" + StateStartingUp,
		"enter_" + StateShuttingDown,
		"enter_" + StateReady,
		"enter_" + StateShutdown,
	}

	for _, expectedCallback := range expectedCallbacks {
		assert.Contains(t, callbacks, expectedCallback, "Missing callback: %s", expectedCallback)
		assert.NotNil(t, callbacks[expectedCallback], "Callback %s is nil", expectedCallback)
	}

	// Verify we have the expected number of callbacks
	assert.Len(t, callbacks, len(expectedCallbacks))
}

func TestTransitionMatrix_Completeness(t *testing.T) {
	// This test ensures that our transition matrix covers all expected state transitions
	// according to the architecture specification

	transitions := GetFSMTransitions()

	// Create a map to track covered transitions
	covered := make(map[string]map[string]bool)
	for _, transition := range transitions {
		for _, src := range transition.Src {
			if covered[src] == nil {
				covered[src] = make(map[string]bool)
			}
			covered[src][transition.Name] = true
		}
	}

	// Verify shutdown state transitions
	assert.True(t, covered[StateShutdown][EventStartNode], "Missing transition: shutdown -> StartNode")

	// Verify ready state transitions
	assert.True(t, covered[StateReady][EventShutdownNode], "Missing transition: ready -> ShutdownNode")

	// Verify starting up state transitions
	assert.True(t, covered[StateStartingUp][EventJobCompleted], "Missing transition: startingup -> JobCompleted")
	assert.True(t, covered[StateStartingUp][EventJobFailed], "Missing transition: startingup -> JobFailed")
	assert.True(t, covered[StateStartingUp][EventJobTimeout], "Missing transition: startingup -> JobTimeout")
	assert.True(t, covered[StateStartingUp][EventForceCleanup], "Missing transition: startingup -> ForceCleanup")

	// Verify shutting down state transitions
	assert.True(t, covered[StateShuttingDown][EventJobCompleted], "Missing transition: shuttingdown -> JobCompleted")
	assert.True(t, covered[StateShuttingDown][EventJobFailed], "Missing transition: shuttingdown -> JobFailed")
	assert.True(t, covered[StateShuttingDown][EventJobTimeout], "Missing transition: shuttingdown -> JobTimeout")
	assert.True(t, covered[StateShuttingDown][EventForceCleanup], "Missing transition: shuttingdown -> ForceCleanup")

	// Verify no invalid transitions exist
	// These should NOT be covered
	assert.False(t, covered[StateReady][EventStartNode], "Invalid transition exists: ready -> StartNode")
	assert.False(t, covered[StateShutdown][EventShutdownNode], "Invalid transition exists: shutdown -> ShutdownNode")
	assert.False(t, covered[StateReady][EventJobCompleted], "Invalid transition exists: ready -> JobCompleted")
	assert.False(t, covered[StateShutdown][EventJobCompleted], "Invalid transition exists: shutdown -> JobCompleted")
}
