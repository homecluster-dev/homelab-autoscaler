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

// Package phase is the single place the state of a slot is computed.
//
// The law the whole NodeClaim model rests on (docs/design/nodeclaim.md §2, §6):
//
//	machine is wanted  ⇔  a live NodeClaim exists for the slot
//
// so the input of every decision is a triple, never a persisted record:
//
//	(live claim phase, observed power, kubelet liveness / node readiness)
//
// This package turns that triple into the two things the operator needs out of it:
//
//   - Derive: the value of Node.status.phase, the derived *report*. It is written by the
//     observation path and by the lifecycle transition path, and read to decide nothing. Both
//     writers call Derive, so they cannot disagree about a value that has one formula.
//   - State: the initial state of the node state machine. It is derived from the same triple,
//     which is what replaces reading the FSM's own state out of status.progress
//     (the v1alpha1 getCurrentState), the ambiguity that the 0.1.20 watchdog and the 0.1.22
//     observed-power precedence rule both existed to patch.
//
// The state names live here, and internal/fsm aliases them, so that the mapping from an
// observation to a state has one definition and no import cycle.
package phase

import (
	corev1 "k8s.io/api/core/v1"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// State names of the node state machine. They are the looplab/fsm state identifiers, and the
// mapping from a derived phase to a state (State) is the FSM's input law.
const (
	StateShutdown     = "shutdown"
	StateStartingUp   = "startingup"
	StateReady        = "ready"
	StateShuttingDown = "shuttingdown"
)

// Input is the whole input of the state machine, and the whole input of the derived report.
//
// It carries no field read from Node.status.phase, and no field read from a record of "what the
// FSM last did": those are the outputs of this package, never its inputs.
type Input struct {
	// Claim is the live NodeClaim of the slot, or nil. A claim in a terminal phase (failed,
	// completed) is not live and is reported as nil by LiveClaim: a terminal claim is not
	// capacity, and it stops being capacity the moment it is recorded.
	Claim *infrav2alpha1.NodeClaim

	// ObservedPower is what the provider observation reports for the machine
	// (Node.status.powerState): on when the corev1.Node reports NodeReady=True, off otherwise.
	// An empty value means "never observed", and is treated as off.
	ObservedPower infrav2alpha1.PowerState

	// NodeUsable is "the kubelet is live and the node can take the pool's work": the corev1.Node
	// exists, reports Ready, is not cordoned, and carries none of the Group's readiness taints
	// (see Unusable for the exact rules).
	NodeUsable bool

	// StartupJobInFlight / ShutdownJobInFlight record that a lifecycle Job of this slot exists and
	// has not finished. They are the difference between "a boot is in flight" (report booting, and
	// the state machine is already in startingup, where JobCompleted is reachable) and
	// "nothing has been asked for yet" (report pending, and StartNode is the event to fire).
	StartupJobInFlight  bool
	ShutdownJobInFlight bool
}

// LiveClaim returns the claim of the input when it is live (pending, booting, ready and not
// deleted), and nil otherwise.
func (in Input) LiveClaim() *infrav2alpha1.NodeClaim {
	if in.Claim == nil {
		return nil
	}

	if !IsLive(in.Claim.Status.Phase) {
		return nil
	}

	return in.Claim
}

// IsLive reports whether a claim phase means "a machine is wanted": pending, booting and ready are
// live; failed and completed are terminal, and are not capacity.
func IsLive(phase infrav2alpha1.NodeClaimPhase) bool {
	switch phase {
	case infrav2alpha1.NodeClaimPending, infrav2alpha1.NodeClaimBooting, infrav2alpha1.NodeClaimReady:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether a claim phase is an end (failed, completed).
func IsTerminal(phase infrav2alpha1.NodeClaimPhase) bool {
	return !IsLive(phase)
}

// ObservedOn reports whether the observation says the machine is up.
func (in Input) ObservedOn() bool {
	return in.ObservedPower == infrav2alpha1.PowerStateOn
}

// MarkedForScaleDown reports whether the live claim of the slot is being deleted, i.e. that a
// scale-down (NodeGroupDeleteNodes, a hand delete, an expired manual claim) has named the machine:
// the phase the slot reports while that drains is draining.
func (in Input) MarkedForScaleDown() bool {
	claim := in.LiveClaim()

	return claim != nil && !claim.DeletionTimestamp.IsZero()
}

// Derive computes the value of Node.status.phase from the input. It is a report, and the phase
// state table in the PR / docs/architecture/state.md is its specification:
//
//	live claim          observed   usable   phase      meaning
//	none                off        -        off        a free slot
//	none                on         -        draining   an unwanted machine: the shutdown path owns it
//	pending             off        -        pending    claimed, nothing started (the cancelable shape)
//	pending             on         -        booting    the machine is coming up before the stamp landed
//	booting             -          -        booting    a startup Job is in flight
//	ready               on         yes      ready      the machine takes the pool's work
//	ready               on         no       booting    not usable yet: the gate is not satisfied
//	ready               off        -        booting    the machine vanished under a live claim: boot it
//	any live, deleted   on         -        draining   a scale-down is draining the machine
//	any live, deleted   off        -        off        the machine is off, the claim is being collected
func Derive(in Input) infrav2alpha1.NodePhase {
	if in.ShutdownJobInFlight && in.ObservedOn() {
		return infrav2alpha1.NodePhaseDraining
	}

	claim := in.LiveClaim()
	if claim == nil {
		// No live claim: the machine is not wanted. Observed up, it is a machine to power off
		// (the case that includes an overdue, failed claim whose boot lands later); observed
		// off, it is a free slot.
		if in.ObservedOn() {
			return infrav2alpha1.NodePhaseDraining
		}

		return infrav2alpha1.NodePhaseOff
	}

	if !claim.DeletionTimestamp.IsZero() {
		// A scale-down has named the machine: while it is observed up the slot is draining, and once
		// it is off the claim is collected. This is the shape DeleteNodes puts the slot in, and it is
		// why a marked claim is never a boot target: the machine is wanted no longer.
		if in.ObservedOn() {
			return infrav2alpha1.NodePhaseDraining
		}

		return infrav2alpha1.NodePhaseOff
	}

	if !in.ObservedOn() {
		// A live claim with the machine off is booted. A claim that was already ready has lost its
		// machine, and its slot is being bought again: report booting, not ready.
		if claim.Status.Phase == infrav2alpha1.NodeClaimPending && !in.StartupJobInFlight {
			return infrav2alpha1.NodePhasePending
		}

		return infrav2alpha1.NodePhaseBooting
	}

	// Live claim, machine observed up.
	if claim.Status.Phase == infrav2alpha1.NodeClaimReady && in.NodeUsable {
		return infrav2alpha1.NodePhaseReady
	}

	// The machine is up but is not usable (registering, boot taint on it, cordoned) or the claim
	// has not been stamped ready: the slot is in its boot window.
	return infrav2alpha1.NodePhaseBooting
}

// State computes the state the node state machine starts a pass in, from the same input as
// Derive. The one place where the report and the state are not the same word is the
// unwanted-machine case: the report says draining (the slot is going away), and the machine is
// in fact Ready and reachable, so the state the shutdown transition starts from is ready.
//
// A shutdown Job in flight means the shuttingdown state, where JobCompleted / JobFailed /
// JobTimeout are reachable and ShutdownNode is not: exactly one operation may own a slot.
func State(in Input) string {
	derived := Derive(in)

	switch derived {
	case infrav2alpha1.NodePhaseOff:
		return StateShutdown
	case infrav2alpha1.NodePhasePending:
		// StartNode is the event to fire, and it fires from shutdown.
		return StateShutdown
	case infrav2alpha1.NodePhaseBooting:
		return StateStartingUp
	case infrav2alpha1.NodePhaseReady:
		return StateReady
	case infrav2alpha1.NodePhaseDraining:
		if in.ShutdownJobInFlight {
			return StateShuttingDown
		}

		// The machine is up and nothing wants it: the transition to fire is ShutdownNode, and it
		// fires from ready.
		return StateReady
	default:
		return StateShutdown
	}
}

// Unusable reports why a corev1.Node cannot take the pool's work, and returns "" when it can. It
// is the readiness gate's predicate, written once so that the gate of the scale-up
// (fsm.setNodeSchedulableWithRetry), the claim's ready phase, and this report all ask the same
// question the same way:
//
//   - the node must report NodeReady=True (a kubelet with a live heartbeat);
//   - the node must carry none of the readiness taints the Group declares
//     (spec.nodeReadinessTaints, by default Cilium's node.cilium.io/agent-not-ready): a node whose
//     CNI has not programmed its datapath cannot take the pool's pods;
//   - the node must not be cordoned (spec.unschedulable): a cordoned node is invisible to
//     cluster-autoscaler's scale-down and takes no pods.
func Unusable(k8sNode *corev1.Node, readinessTaintKeys []string) string {
	if k8sNode == nil {
		return "the Kubernetes node object does not exist"
	}

	isReady := false

	for _, cond := range k8sNode.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			isReady = cond.Status == corev1.ConditionTrue

			break
		}
	}

	if !isReady {
		return "node is not Ready"
	}

	for _, taint := range k8sNode.Spec.Taints {
		for _, key := range readinessTaintKeys {
			if taint.Key == key {
				return "node carries the readiness taint " + key
			}
		}
	}

	if k8sNode.Spec.Unschedulable {
		return "node is cordoned"
	}

	return ""
}
