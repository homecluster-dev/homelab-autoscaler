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
	corev1 "k8s.io/api/core/v1"
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

		// The staleness guard, applied to the object this attempt read. The check above compares the
		// machine with its own inputs; this one compares the inputs with the slot, and it is the one
		// that matters for the *history*: a slot that already reports the phase these inputs derive
		// (the observation path wrote it, a pass of a machine that replaced this one wrote it) has its
		// change recorded, and appending the same change again would push the oldest real row out of
		// the ring and count a boot twice. A slot that reports something else is a slot this machine
		// no longer describes, and the writer of that report is the pass that owns the inputs.
		if fresh.Status.Phase == derived {
			nm.node = fresh

			return nil
		}

		if !stampDerivedPhase(fresh, derived, time.Now(), phase.Reason(nm.input, derived)) {
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

// stampDerivedPhase writes the derived phase onto a fresh copy of the slot, together with the
// record of the change (phase.Record: lastTransitionTime, previousPhase, the ring, the activation
// counts), and keeps the two timestamps an operator reads ("when did this machine last boot / go
// off") in step with it. It returns true when the object changed, so a pass whose report already
// matches writes nothing.
//
// The history is not written here: phase.Record is its only writer, and the observation path calls
// it with the same derived value, so a slot's history is the same list of changes whichever path
// got there first - and the caller's guard ("the slot already reports what my inputs derive") is
// what keeps the two from each appending the same change.
func stampDerivedPhase(node *infrav2alpha1.Node, derived infrav2alpha1.NodePhase, at time.Time, reason string) bool {
	moved := node.Status.Phase != derived

	changed := phase.Record(node, derived, at, reason)

	if !moved {
		return changed
	}

	now := metav1.NewTime(at)

	switch derived {
	case infrav2alpha1.NodePhaseBooting:
		node.Status.LastStartupTime = &now
	case infrav2alpha1.NodePhaseDraining:
		node.Status.LastShutdownTime = &now
	}

	return true
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

// recordGateAnswer is the readiness gate writing its own evidence into the machine's input, so the
// report the transition derives next describes the machine the gate is standing in front of.
//
// The machine that answers for a boot is the goroutine of the pass that fired it (`monitorJobCompletion`),
// and the input of that machine is the triple the pass read: for a real boot, the machine observed
// **off**, with no node object. The gate then spends its time turning that into a machine that is up
// and usable - registration, Ready, free of the pool's readiness taints, uncordoned - and the claim
// move it writes is recorded into the input (`setInputPhase`). The other two inputs of the triple are
// the evidence the gate read for itself, and are recorded here: the node object the uncordon just left
// behind, asked with the one predicate the report uses (`phase.Unusable`), and its Ready condition,
// which is the definition `status.powerState` is written from.
//
// Without this, the `booting -> ready` report is derived from the boot's starting snapshot, the
// staleness guard of `reportPhase` refuses the write - correctly, that machine's inputs describe a
// machine that is not up - and nothing writes the ready report afterwards. The other writer of the
// report is the observation path, and it runs on `corev1.Node` events: a kubelet that has settled into
// Ready is quiet (its heartbeats go to its Lease, and its status posts every five minutes), so a slot
// whose machine booted while the pass that started it watched it off reports `booting` for the life of
// the machine, and the autoscaler reads the pool as having no usable capacity.
//
// A node object that cannot be handed over (no name on the slot, a Get the API refused, the gate
// abandoned on its timeout) records nothing: the input keeps the observation it was built from, and
// the report is the observation reconciler's to write when the node object next moves. A gate that
// timed out moves the claim to `failed` anyway, so that shape has an owner: the input law shuts the
// machine down, and law 3 writes the report of the shutdown it fires.
func (nm *NodeStateMachine) recordGateAnswer(kubeNode *corev1.Node) {
	if kubeNode == nil {
		return
	}

	nm.input.NodeUsable = phase.Unusable(kubeNode, nm.readinessTaintKeys) == ""

	for _, cond := range kubeNode.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			// The gate's own definition of the machine being up, the same one the observation path
			// writes status.powerState from (core.KubernetesNodeReconciler.getNodeStatus).
			nm.input.ObservedPower = infrav2alpha1.PowerStateOn

			break
		}
	}
}

// recordShutdownAttempt counts the shutdown this machine fired on the claim a scale-down named. It
// runs on the path that created the shutdown Job (ShutdownNode, after the create), so the streak the
// bound reads is a streak of attempts: three shutdown Jobs on a machine the provider keeps observing
// up, and the pause that follows them.
//
// It never fails the transition, for the same reason moveClaim does not: the shutdown is running,
// and a claim status write the API server refused costs the record of one attempt, not the machine.
// A slot whose shutdown never lands is re-fired by the next pass and counted by the one that
// creates the next Job, so the bound closes in either way; a streak that cannot be written is a
// bound that is one attempt looser, which is the direction that does not destroy a machine.
//
// With no claim (the shutdown of a machine the pool stopped wanting, an episode whose claim is
// already collected) there is nothing to count on: the slot's digest is the record, and
// claim.ScaleDownStreakExhausted reads it.
func (nm *NodeStateMachine) recordShutdownAttempt() {
	if nm.claim == nil || nm.claimWriter == nil {
		return
	}

	if err := nm.claimWriter.RecordShutdownAttempt(context.TODO(), nm.claim); err != nil {
		log.Log.WithName("fsm").Error(err, "Failed to record the shutdown attempt on the claim; the scale-down bound is one attempt looser",
			"node", nm.node.Name, "claim", nm.claim.Name)
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

	if !phase.IsPending(nm.claim.Status.Phase) {
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

// releaseJobInputs drops the lifecycle-Job flags of the input when the operation that owned them is
// over: a Job that failed, timed out, or was force-cleaned no longer has anything in flight, and the
// report and the state the same pass derives must describe the slot the rollback landed on, not the
// transition that died.
func (nm *NodeStateMachine) releaseJobInputs(e *fsm.Event) {
	if e != nil && e.Src == StateShuttingDown {
		nm.input.ShutdownJobInFlight = false

		return
	}

	nm.input.StartupJobInFlight = false
}
