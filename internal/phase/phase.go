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
// The law the whole NodeClaim model rests on (docs/development/nodeclaim.md §2, §6):
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
//   - Record: the history of that report - when it last changed value, what it changed from, the
//     bounded ring of its changes, and the counts of the boots inside the last 24 h. Both writers
//     call Record too, and nothing else writes those fields, so a slot's phase and its history
//     cannot be told apart by which path got there first (see record.go).
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
	// Claim is the claim of the slot as its caller read it - the newest live claim of the slot,
	// including one a scale-down has marked for deletion - or nil. Filtering by phase is LiveClaim's
	// job, so a caller that hands in a terminal claim is answered as nil: a terminal claim is not
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

// LiveClaim returns the claim of the input when its phase is live - pending, counting the empty
// phase of a claim the controller has not written yet, booting, ready - and nil for a terminal claim
// and for no claim at all.
//
// A claim a scale-down has marked IS returned, and that is the point: the mark is not the phase
// question. The marked claim owns its slot, its boot is committed, and its finalizer is the request
// that reverses it, which is what the report row "a live claim, marked, observed up -> draining"
// needs. MarkedForScaleDown below is this question conjoined with the deletion timestamp, and
// WantsMachine (internal/claim) is the stronger question a boot must not answer to a marked claim.
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
//
// An empty phase is live, and is read as pending. The status subresource strips `status` from a
// create, so a claim that has just been created has no phase at all until the claim controller's
// first write lands: a brand-new request is capacity from the instant it exists, and TargetSize,
// IsFree, ClaimedSlots and the webhook all have to say so, or a second IncreaseSize claims the same
// slot. `+kubebuilder:default=pending` on the status does not fix this - a default on a subresource
// field is only stamped by the first status write - so the reading lives here, where
// claim.Recorder.Move already defaults an empty phase to pending.
func IsLive(phase infrav2alpha1.NodeClaimPhase) bool {
	switch phase {
	case "", infrav2alpha1.NodeClaimPending, infrav2alpha1.NodeClaimBooting, infrav2alpha1.NodeClaimReady:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether a claim phase is an end (failed, completed). A claim with no phase yet
// is not an end: it is a request the API server has accepted and the controller has not moved.
func IsTerminal(phase infrav2alpha1.NodeClaimPhase) bool {
	return !IsLive(phase)
}

// IsPending reports whether a claim is in the cancelable phase, counting the phase a created claim
// carries before the first status write (empty) as pending, for the same reason IsLive does.
func IsPending(phase infrav2alpha1.NodeClaimPhase) bool {
	return phase == "" || phase == infrav2alpha1.NodeClaimPending
}

// InBootWindow reports whether a claim is in the phase range `spec.deadlineAt` bounds: the machine
// is being bought, and has not been bought. A claim that reached `ready` owns a machine, and the
// boot budget says nothing about it, so the deadline, and the attempt bound that rides on it, stop
// applying there (docs/development/nodeclaim.md §4).
func InBootWindow(phase infrav2alpha1.NodeClaimPhase) bool {
	return IsPending(phase) || phase == infrav2alpha1.NodeClaimBooting
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
// state table in the PR / docs/development/state.md is its specification:
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
		if IsPending(claim.Status.Phase) && !in.StartupJobInFlight {
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

// IsUp reports whether a slot's phase means the pool is paying for a machine: `booting` (the boot
// window: a machine being bought), `ready` (it takes the pool's work) and `draining` (up, unwanted,
// cordoned and not yet off) are the three; `off`, `pending` and a slot that has never reported are
// the rest.
//
// It is the summing rule of the pool's `On` count (Group.status.on), and a display rule only: no
// decision in this repository branches on it. `booting` counts as up because it is the phase a slot
// reports while its machine is being bought, and a boot that never lands is money spent - the count
// is about what the pool is paying for, not about what a machine is doing this second.
func IsUp(p infrav2alpha1.NodePhase) bool {
	switch p {
	case infrav2alpha1.NodePhaseBooting, infrav2alpha1.NodePhaseReady, infrav2alpha1.NodePhaseDraining:
		return true
	default:
		return false
	}
}

// ObservedPower maps the Ready condition of a Kubernetes node onto the power the provider observes
// for the machine: on when the kubelet is beating, off otherwise. A node that is not Ready, and a
// node object that does not exist, are off: the kubelet registers the machine when it boots and the
// object is left behind, NotReady, when the machine is powered off, so "not Ready" is the weak
// observation of a machine that is not there, and it is the same question in the same words for
// every reader of the report.
//
// This is the definition the observation path writes `status.powerState` from, and the one the
// report of a slot is derived over. A writer of the report that reads the machine through anything
// else - a record of what an earlier pass saw, a copy of the condition with its own name - is a
// writer of two formulas for one field, which is the ambiguity the observed-power precedence rule
// of 0.1.22 existed to patch.
func ObservedPower(k8sNode *corev1.Node) infrav2alpha1.PowerState {
	if k8sNode == nil {
		return infrav2alpha1.PowerStateOff
	}

	for _, cond := range k8sNode.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			if cond.Status == corev1.ConditionTrue {
				return infrav2alpha1.PowerStateOn
			}

			return infrav2alpha1.PowerStateOff
		}
	}

	return infrav2alpha1.PowerStateOff
}

// Unusable reports why a corev1.Node cannot take the pool's work, and returns "" when it can. It
// is the readiness gate's predicate, written once so that the gate of the scale-up
// (fsm.setNodeSchedulableWithRetry), the claim's ready phase, and this report all ask the same
// question the same way: the node must report NodeReady=True (a kubelet with a live heartbeat),
// carry none of the readiness taints the Group declares (spec.nodeReadinessTaints, by default
// Cilium's node.cilium.io/agent-not-ready: a node whose CNI has not programmed its datapath cannot
// take the pool's pods), and not be cordoned (spec.unschedulable: a cordoned node is invisible to
// cluster-autoscaler's scale-down and takes no pods).
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
