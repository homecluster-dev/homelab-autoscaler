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
	"time"

	"github.com/looplab/fsm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// reportPhase records the derived phase of the slot: phase.Derive(input), the report of
// (live claim, observed power, node usability).
//
// The value is computed, never passed in by the caller. The v1alpha1 writer took the progress the
// caller "believed it was writing" (every hook passed Progress(e.Dst)) because status.phase was
// both the state machine's own state and the world's report, and the guard that refused a value the
// machine was not in existed to keep those two readings from being written by four controllers.
// v2alpha1 has one formula, so there is nothing for a caller to pass, and the two readings cannot
// disagree by construction.
//
// The one remnant of the guard that is still meaningful for a derived value is kept: if the inputs
// of this machine no longer describe the state it is in, the machine is stale - the async job
// monitor goroutine of a transition that already finished, a state machine built from a Node read
// before a transition ran - and the pass that owns the inputs writes the report. A stale machine
// writing over a live one is how a slot used to be stranded in a transitional value with nothing
// driving it.
//
// It is a report: nothing in this repository reads Node.status.phase to decide an action, and the
// state machine's own input is never the record of a past pass.
func (nm *NodeStateMachine) reportPhase() {
	ctx := context.TODO()
	logger := log.Log.WithName("fsm")

	derived := phase.Derive(nm.input)

	if state := phase.State(nm.input); state != nm.fsm.Current() {
		logger.V(1).Info("Not recording the derived phase: the inputs of this machine no longer describe the state it is in",
			"node", nm.node.Name,
			"derivedPhase", string(derived),
			"derivedState", state,
			"machineState", nm.fsm.Current())

		return
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.Node{}
		if err := nm.client.Get(ctx, client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		if !stampDerivedPhase(fresh, derived) {
			// The slot already reports what the inputs say: nothing to write.
			nm.node = fresh

			return nil
		}

		if err := nm.client.Status().Update(ctx, fresh); err != nil {
			return err
		}

		nm.node = fresh

		return nil
	}); err != nil {
		logger.Error(err, "Failed to record the derived phase of the slot", "node", nm.node.Name, "phase", string(derived))
	}
}

// stampDerivedPhase writes the derived phase onto a fresh copy of the slot and keeps the two
// timestamps an operator reads ("when did this machine last boot / go off") in step with it. It
// returns true when the object changed, so a pass whose report already matches writes nothing.
func stampDerivedPhase(node *infrav2alpha1.Node, derived infrav2alpha1.NodePhase) bool {
	if node.Status.Phase == derived {
		return false
	}

	node.Status.Phase = derived

	now := metav1.NewTime(time.Now())

	switch derived {
	case infrav2alpha1.NodePhaseBooting:
		node.Status.LastStartupTime = &now
	case infrav2alpha1.NodePhaseDraining:
		node.Status.LastShutdownTime = &now
	}

	return true
}

// claimOf returns the live claim this machine moves, and nil when the transition has no claim to
// report on (the shutdown of a machine the pool no longer wants).
func (nm *NodeStateMachine) claimOf() *infrav2alpha1.NodeClaim {
	return nm.claim
}

// moveClaim records a phase move of the claim this transition is the evidence for, and keeps the
// machine's own input in step with it, so the report written after the move is derived from the
// claim's new phase.
//
// It never fails the transition. The lifecycle operation is what it is - a startup Job exists, a
// readiness gate passed, a Job failed - and a claim status write that the API server refused costs
// the record, not the machine: the claim controller reads the same evidence on its next pass (the
// Job, the deadline, the observation) and lands the phase there. A claim that cannot be moved is
// therefore logged at the level of a failed write and left to the controller, and a claim in
// pending whose boot did start is corrected by the same path.
func (nm *NodeStateMachine) moveClaim(to infrav2alpha1.NodeClaimPhase, reason string) {
	if nm.claim == nil {
		// No claim: nothing to move, and the report of the slot is derived from the observation
		// alone. This is the shape of a shutdown of an unwanted machine, and of every unit test.
		nm.setInputPhase(to)

		return
	}

	if nm.claimWriter == nil {
		nm.setInputPhase(to)

		return
	}

	if err := nm.claimWriter.MoveClaim(context.TODO(), nm.claim, to, reason); err != nil {
		log.Log.WithName("fsm").Error(err, "Failed to record the phase of the claim; the claim controller lands it from the same evidence",
			"node", nm.node.Name, "claim", nm.claim.Name, "phase", string(to), "reason", reason)

		return
	}

	nm.setInputPhase(to)
}

// setInputPhase keeps the machine's cached claim (and so its input) in step with a phase the
// transition just produced, so the derived report of the same pass names the machine the
// transition bought and not the one the pass started with.
func (nm *NodeStateMachine) setInputPhase(to infrav2alpha1.NodeClaimPhase) {
	if nm.claim == nil {
		return
	}

	nm.claim.Status.Phase = to
	nm.input.Claim = nm.claim

	if to == infrav2alpha1.NodeClaimBooting {
		nm.input.StartupJobInFlight = true
	}
}

// commitAttempt is the pending -> booting edge: the write that says this claim bought a machine.
// It runs on the same path that created the startup Job (StartNode), so an attempt is a machine and
// not a reconcile pass, and a claim that never got a Job keeps its number without spending a
// machine on it.
func (nm *NodeStateMachine) commitAttempt(reason string) {
	if nm.claim == nil {
		return
	}

	if nm.claim.Status.Phase != infrav2alpha1.NodeClaimPending {
		return
	}

	nm.moveClaim(infrav2alpha1.NodeClaimBooting, reason)
}

// jobFailureReason names the reason a failed or timed-out lifecycle Job records on the claim, told
// apart by the direction of the transition that died: a startup Job is the attempt of a scale-up, a
// shutdown Job is the attempt of a scale-down.
func jobFailureReason(e *fsm.Event, timedOut bool) string {
	isShutdown := e != nil && (e.Src == StateShuttingDown || e.Dst == StateReady)

	switch {
	case isShutdown && timedOut:
		return infrav2alpha1.ClaimReasonShutdownJobTimeout
	case isShutdown:
		return infrav2alpha1.ClaimReasonShutdownJobFailed
	case timedOut:
		return infrav2alpha1.ClaimReasonStartupJobTimeout
	default:
		return infrav2alpha1.ClaimReasonStartupJobFailed
	}
}

// completeScaleDownClaim ends the claim of a scale-down whose machine went off: the claim
// NodeGroupDeleteNodes named and marked for deletion. A slot with no claim (a machine the pool
// stopped wanting, powered off by the input law) has nothing to complete, and a live claim that no
// scale-down marked is left alone: the shutdown of an unwanted machine is not the end of a boot
// request that already landed.
func (nm *NodeStateMachine) completeScaleDownClaim(reason string) {
	if nm.claim == nil || nm.claim.DeletionTimestamp.IsZero() {
		return
	}

	nm.moveClaim(infrav2alpha1.NodeClaimCompleted, reason)
}
