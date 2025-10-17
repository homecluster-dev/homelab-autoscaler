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
	"github.com/looplab/fsm"
)

// GetFSMTransitions returns the FSM transition matrix according to the architecture specification
func GetFSMTransitions() []fsm.EventDesc {
	return []fsm.EventDesc{
		// StartNode: Shutdown -> StartingUp
		{Name: EventStartNode, Src: []string{StateShutdown}, Dst: StateStartingUp},

		// ShutdownNode: Ready -> ShuttingDown
		{Name: EventShutdownNode, Src: []string{StateReady}, Dst: StateShuttingDown},

		// JobCompleted: StartingUp -> Ready, ShuttingDown -> Shutdown
		{Name: EventJobCompleted, Src: []string{StateStartingUp}, Dst: StateReady},
		{Name: EventJobCompleted, Src: []string{StateShuttingDown}, Dst: StateShutdown},

		// JobFailed: StartingUp -> Shutdown, ShuttingDown -> Ready
		{Name: EventJobFailed, Src: []string{StateStartingUp}, Dst: StateShutdown},
		{Name: EventJobFailed, Src: []string{StateShuttingDown}, Dst: StateReady},

		// JobTimeout: Transition to shutdown on timeout from starting up, stay in same state for shutting down
		{Name: EventJobTimeout, Src: []string{StateStartingUp}, Dst: StateShutdown},
		{Name: EventJobTimeout, Src: []string{StateShuttingDown}, Dst: StateReady},

		// ForceCleanup: Force transition to Shutdown from any transitional state
		{Name: EventForceCleanup, Src: []string{StateStartingUp, StateShuttingDown}, Dst: StateShutdown},
	}
}

// GetFSMCallbacks returns the FSM callbacks for hook integration
func GetFSMCallbacks(nm *NodeStateMachine) fsm.Callbacks {
	return fsm.Callbacks{
		// Before hooks for lock acquisition
		"before_" + EventStartNode:    nm.beforeStartNode,
		"before_" + EventShutdownNode: nm.beforeShutdownNode,

		// After hooks for lock release and status updates
		"after_" + EventJobCompleted: nm.afterJobCompleted,
		"after_" + EventJobFailed:    nm.afterJobFailed,
		"after_" + EventForceCleanup: nm.afterForceCleanup,

		// State entry hooks
		"enter_" + StateStartingUp:   nm.enterStartingUp,
		"enter_" + StateShuttingDown: nm.enterShuttingDown,
		"enter_" + StateReady:        nm.enterReady,
		"enter_" + StateShutdown:     nm.enterShutdown,
	}
}

// ValidateEventTransition checks if an event can be triggered from the current state
func ValidateEventTransition(currentState, event string) bool {
	transitions := GetFSMTransitions()

	for _, transition := range transitions {
		if transition.Name == event {
			for _, srcState := range transition.Src {
				if srcState == currentState {
					return true
				}
			}
		}
	}

	return false
}

// GetValidEventsForState returns all valid events that can be triggered from the given state
func GetValidEventsForState(state string) []string {
	transitions := GetFSMTransitions()
	var validEvents []string

	for _, transition := range transitions {
		for _, srcState := range transition.Src {
			if srcState == state {
				// Check if event is already in the list to avoid duplicates
				found := false
				for _, event := range validEvents {
					if event == transition.Name {
						found = true
						break
					}
				}
				if !found {
					validEvents = append(validEvents, transition.Name)
				}
				break
			}
		}
	}

	return validEvents
}

// GetDestinationState returns the destination state for a given event from a source state
func GetDestinationState(srcState, event string) (string, bool) {
	transitions := GetFSMTransitions()

	for _, transition := range transitions {
		if transition.Name == event {
			for _, src := range transition.Src {
				if src == srcState {
					return transition.Dst, true
				}
			}
		}
	}

	return "", false
}

// IsTransitionalState returns true if the state represents an ongoing operation
func IsTransitionalState(state string) bool {
	return state == StateStartingUp || state == StateShuttingDown
}

// IsStableState returns true if the state represents a stable, non-transitional state
func IsStableState(state string) bool {
	return state == StateReady || state == StateShutdown
}

// GetOperationForEvent returns the coordination lock operation type for an event
func GetOperationForEvent(event string) string {
	switch event {
	case EventStartNode:
		return OperationScaleUp
	case EventShutdownNode:
		return OperationScaleDown
	default:
		return ""
	}
}
