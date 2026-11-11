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

// The completion of a shutdown Job is the one write of status.progress: shutdown that this package
// does not verify, and it is how a slot comes to say it is shut down while its machine is up and
// serving pods. This file is the audit of that path, and the guard it produced.
//
// Every writer of status.progress: shutdown, and what each one is built on:
//
//  1. enterShutdown, on the JobCompleted transition shuttingdown -> shutdown (hooks.go). The
//     evidence is the exit code of the pod the slot's spec.shutdownPodSpec ran - a script the
//     operator writes, which calls the provider's power API, SSHes to the machine, pulls a plug. It
//     is trusted blindly: monitorJobCompletion reads job.Status.Succeeded, fires JobCompleted, and
//     the slot is recorded as off. A script that exits 0 on a path where the provider call did not
//     land - the provider returns 202 for a server that is not powered off, the script swallows an
//     error, `power-off` is issued as a graceful ACPI request and the machine comes back, a
//     reprovisioning leaves the same server running - writes the lie here. This is the cause of the
//     live slot: the Job succeeded, the machine did not go off, the FSM recorded shutdown.
//  2. enterShutdown, on JobFailed / JobTimeout / ForceCleanup of a *startup*. Each of those lands on
//     the stable off/shutdown pair by design, and each leaves the machine that booted in it powered
//     on: a startup Job that exits non-zero after the machine booted, a startup whose 5m timeout
//     expired on a machine that registered, a startup stuck past the 15m threshold that a force
//     cleanup moves. They are the same shape, and they are deliberate: the rollback into shutdown is
//     where a failed startup puts a slot, and writing "ready" for a machine that never became usable
//     is the churn the readiness gate closes. What has to be true is that the pair does not *rest*:
//     the observation controller writes the off when the machine is gone.
//  3. core.applyObservedPowerState, on an observed off (a node that stopped reporting Ready). That
//     is an observation and cannot contradict one.
//  4. NodeGroupDecreaseTargetSize used to write status.progress: shuttingdown directly - no
//     transition, no Job, no lock - and no longer does (see
//     docs/architecture/state.md). A writer outside the FSM is the shape this audit has to keep out
//     of the completion path, not a cause it can fix here.
//
// What turns one of those writes into a slot that cannot heal is the pair, not the write:
//
//   - updateNodeProgress writes status.progress and never status.powerState, so every writer above
//     leaves {status.powerState: on, status.progress: shutdown} standing until an observation lands;
//   - core.applyObservedPowerState, the writer that owns a slot with a node object, refuses to
//     correct a progress from an observed on unless spec.desiredPowerState is on - the
//     duplicate-shutdown race in the node-monitor-grace window is what that gate buys, and it is
//     the right trade - so on a slot claimed off the pair is stable;
//   - the FSM reads its state from status.progress, so from that pair the only legal event is
//     StartNode, and the claim is off: the reconcile logs "Node is in desired state or
//     transitioning" and does nothing, on a machine that is up, for as long as the slot lives.
//
// The guard, and the boundary of what can be verified from here. The provider client is *not*
// reachable from this package: the power operation is the lifecycle Job, a container the operator
// supplies, and the autoscaler's gRPC provider is the other side of this repository (it answers
// cluster-autoscaler about node groups; it does not own a machine's power state). The observation
// that is reachable is the one the reconcilers already write, so the hook's completion write is
// conditional on it, exactly as the two rules of internal/progress are:
//
//   - machineObservedUp reads the slot's last observed status.powerState and the Ready condition of
//     its corev1.Node. On a completed shutdown Job, that pair says the machine is up;
//   - the completion then records the observed progress (ready) instead of shutdown, and says so
//     (a ShutdownNotObserved Warning), leaving the slot in the one state the FSM can act on: from
//     ready, with the claim off, the next pass runs ShutdownNode - with its lock, its cordon and its
//     Job;
//   - the write is one write. refuseUnobservedShutdown returns the progress it recorded, and the
//     after_ hook of the same completion writes the progress of *its* transition (Progress(e.Dst)),
//     which updateNodeProgress then refuses because the machine is in ready: the guard of that
//     function stays a check and not a tautology, and a refused completion is not written twice;
//   - the heartbeat is the discriminator, and the bound it is measured against is the shutdown Job's
//     status.completionTime - the monitor passes the Job it read in the event's arguments, so the hook
//     has it (see jobFromArgs, shutdownObservationBound). That bound is what separates the two shapes:
//     a machine that went off during the script stops heartbeating *before* the Job finishes, a machine
//     the script did not power off keeps heartbeating *after* it. The stamp this shutdown left on the
//     slot (status.lastShutdownTime) cannot separate them, because it is written at the *start* of the
//     operation and the hook runs at most one JobMonitorPollInterval (30s) later: a genuinely live
//     machine presents a heartbeat ~30s after that stamp, and a window measured from it accepts the
//     incident's pair as "shut down". lastShutdownTime + shutdownObservationFreshness is kept as the
//     fallback for a completion with no Job object to read (a short spec.ttlSecondsAfterFinished),
//     which is the shape with no better evidence available;
//   - the retry the refusal releases is bounded. A pool whose shutdown script exits 0 without the
//     power-off landing cycles Job, refused completion, correction, ShutdownNode, Job, and every pass
//     of it re-cordons a node that is serving pods: the shutdown-attempt counter the FSM owns
//     (ShutdownAttemptLimit, internal/fsm/shutdown_attempts.go) stops it at 3 attempts with one
//     Warning naming the limit, and progress.ReconcileObservedPower paces the loop with an
//     exponentially growing requeue. Pre-fix this failure was static (a frozen slot); bounded, it is a
//     bounded number of shutdown Jobs, one Warning, and a slot an operator can read;
//   - what the hook still cannot do is wait. It runs inside the completion of an operation whose
//     monitor has already returned, so the precedence is enforced a second time, and with a horizon
//     that a hook does not have, by progress.ReconcileObservedPower
//     (internal/progress/observed_power.go): it applies the same rule once the slot's lifecycle Job
//     has aged out of SettleGracePeriod. That is the backstop, and the split is the one the watchdog
//     docs draw: this guard keeps a provable lie out of the completion write, the reconciler's rule
//     corrects a lie that rested.
//
// The trade accepted with the completion-time bound, stated because it is a real one: a provider whose
// power-off is asynchronous and lands *after* the script exits presents a heartbeat older than the
// completion, so the guard calls the machine "shut down" - no. It calls it "up", refuses the
// completion, and the machine goes off moments later: the cost is one retry (one attempt of the
// bound), not a lie in the record, because the slot is left at ready with a machine the observation
// says is up, and progress.ReconcileObservedPower settles the pair once the node object stops
// reporting Ready. The opposite direction - a machine that is up recorded off - is the failure that
// freezes a slot, and everything in this file is biased towards refusing on a doubt.

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/looplab/fsm"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// shutdownObservationFreshness is the fallback window: how much newer than the *start* of the
// shutdown operation the kubelet's last heartbeat has to be for "the machine is up" to be evidence
// that the shutdown did not land, for the completions whose Job object is already gone.
//
// It is a fallback because the bound that is measured against the start of the shutdown cannot
// separate the two shapes it has to. status.lastShutdownTime is stamped at the beginning of the
// transition - enterShuttingDown, in the same status write as progress: shuttingdown - and the Job
// monitor notices the Job is done at most one JobMonitorPollInterval (30s) later, so a shutdown script
// that finishes fast is completed, and judged, ~30s after the stamp. A machine that is genuinely up
// has heartbeated in exactly that interval, and a window of a minute therefore accepts it as "shut
// down": the incident's pair, a completed shutdown Job, a Ready node with a heartbeat 30s after the
// stamp, and the slot recorded off on a machine serving pods. The Job's own completion time is the
// bound that does separate them (see shutdownObservationBound), and it is what the guard uses whenever
// the Job is there - which it is for the default spec.ttlSecondsAfterFinished of 600s, the window in
// which this hook runs at all.
//
// The window is kept for the shape with no Job to read: a slot at progress: shutdown with no shutdown
// operation of its own (a record written by a rollback, a hand patch, a controller restart) has no
// completion time, and its lastShutdownTime - if any - is the only stamp there is. There the older
// rule is the defensible one: the node object of a machine that went off stays Ready for
// node-monitor-grace-period (kube-controller-manager's default, 40s) and the residue's last heartbeat
// lands inside it, so a heartbeat that is not more than this window after the stamp is not evidence.
const shutdownObservationFreshness = 60 * time.Second

// ReasonShutdownNotObserved is recorded on the Node CR when the shutdown Job of a slot completed and
// the machine it was supposed to power off is observed up: the slot is not recorded shut down,
// because a record of a machine that is up is the state that makes every further operation on the
// slot unreachable (see this file's audit).
const ReasonShutdownNotObserved = "ShutdownNotObserved"

// refuseUnobservedShutdown is the guard on the completion of a shutdown Job. It returns the progress
// it recorded - ready - when the machine the Job was supposed to power off is observed up, and the
// completion is then not recorded as a shutdown at all; it returns the empty progress when the
// observation allows the shutdown, and writes nothing, leaving the caller to record it as it always
// did.
//
// The return value is the contract with the after_ hook of the same completion. That hook writes the
// progress of the transition it closes (Progress(e.Dst)), which is what keeps updateNodeProgress's
// guard - "refuse a progress the state machine is not in" - a real check: a completion that was
// refused lands on a machine whose state is ready, so the hook's write of shutdown is refused and the
// slot is written exactly once, by this guard. A hook that wrote the machine's own state instead
// would write this value a second time, and the guard would be a tautology - a caller that can only
// pass the value the guard reads can never be refused by it.
//
// Moving the machine's own state (looplab/fsm's SetState) is what makes the write legal rather than
// an exception: updateNodeProgress persists only the progress of the state the machine is in, and the
// state it is in after a shutdown that did not land is ready, because the machine is up.
//
// From ready, with the claim off - the claim a completed shutdown always carries - the node
// controller's shutdown branch is reachable again and the next pass runs ShutdownNode, with its
// coordination lock, its cordon and its Job. That retry is bounded (fsm.ShutdownAttemptLimit: the
// attempts of one slot are counted by enterShuttingDown, and ShutdownNode refuses past the bound with
// one Warning), and progress.ReconcileObservedPower backs the precedence up with the horizon a hook
// does not have.
func (nm *NodeStateMachine) refuseUnobservedShutdown(ctx context.Context, job *batchv1.Job) infrav1alpha1.Progress {
	if !nm.machineObservedUp(ctx, job) {
		return ""
	}

	nm.fsm.SetState(StateReady)
	nm.updateNodeProgress(infrav1alpha1.ProgressReady)

	return infrav1alpha1.ProgressReady
}

// machineObservedUp reports whether the machine of the slot is observed up, by an observation current
// enough to be evidence against a completed shutdown: status.powerState on, the slot's corev1.Node
// reporting Ready, and that Ready condition's last heartbeat stamped after the bound the shutdown
// leaves (see shutdownObservationBound - the Job's completion time, and status.lastShutdownTime plus
// shutdownObservationFreshness when no Job object is left to read).
//
// The power-state readings are the two the observation controllers already publish
// (core.getNodeStatus maps NodeReady=True onto status.powerState: on). This repository has no provider
// client in this path - the power operation is the lifecycle Job the operator supplies, and the
// autoscaler's gRPC provider answers cluster-autoscaler about node groups, it does not own a
// machine's power state - so the hook's verification is conditional on the reconciler's observation,
// and progress.ReconcileObservedPower is the backstop that applies the same precedence once the
// observation has had time to settle.
//
// A read that fails is not evidence: the completion is recorded as it always was. A slot with no
// corev1.Node is not observed up: the machine is gone, and a provider that deletes the server (a
// scale-down that removes the machine) tears the node object down, so requiring the node object is
// what keeps this guard off the shape the progress watchdog exists for.
func (nm *NodeStateMachine) machineObservedUp(ctx context.Context, job *batchv1.Job) bool {
	if nm.node.Status.PowerState != infrav1alpha1.PowerStateOn {
		return false
	}

	if nm.node.Spec.KubernetesNodeName == "" {
		return false
	}

	kubeNode := &corev1.Node{}
	if err := nm.client.Get(ctx, client.ObjectKey{Name: nm.node.Spec.KubernetesNodeName}, kubeNode); err != nil {
		// Not knowing is not evidence. NotFound is the common case - the machine is gone, and a
		// shutdown that deletes the server tears the node object down with it - and a transport
		// failure says nothing about the machine either, so the completion is recorded as it always
		// was and the reconciler's rule is the one that looks again, on the far side of its grace.
		return false
	}

	bound, _ := shutdownObservationBound(nm.node, job)

	return kubeletIsLive(kubeNode, bound)
}

// shutdownObservationBound is the timestamp a kubelet heartbeat has to be after to say "this machine
// checked in after that shutdown finished", and a note naming the evidence it was measured from (for
// the log line: "the Job completed 5s ago" and "no Job, the shutdown stamp is 10 minutes old" are
// different readings of the same refusal).
//
// The shutdown Job's status.completionTime is the bound, and it is the one that separates the two
// shapes: a machine that went off during the script stops heartbeating before the Job finishes, and a
// machine the script did not power off keeps heartbeating past it. The monitor hands this hook the Job
// it read (see jobFromArgs), so the bound is available on every completion the default
// spec.ttlSecondsAfterFinished leaves an object behind for.
//
// With no Job, and no completion time on it, status.lastShutdownTime + shutdownObservationFreshness is
// the fallback, and the trade of the fallback is documented on that constant.
//
// What the bound deliberately does not try to cover: a provider whose power-off is asynchronous and
// lands *after* the script exits. The script has returned, the Job has completed, and the kubelet's
// last heartbeat is older than that completion only if the machine died after the Job finished - so
// the machine is reported "still up" and the completion is refused once. That costs one retry (and one
// attempt of the shutdown bound), and progress.ReconcileObservedPower is the backstop for a pair that
// rests: by the time the Job's grace has aged out, the machine is off, its node object is gone or
// NotReady, and there is nothing left to correct. The asymmetry is deliberate - a false "up" costs a
// bounded retry, a false "off" is the frozen slot this file exists to keep out of the record.
func shutdownObservationBound(node *infrav1alpha1.Node, job *batchv1.Job) (time.Time, string) {
	if job != nil && job.Status.CompletionTime != nil {
		completed := job.Status.CompletionTime.Time

		return completed, fmt.Sprintf("shutdown Job %s completed at %s", job.Name, completed.UTC().Format(time.RFC3339))
	}

	if node.Status.LastShutdownTime != nil {
		bound := node.Status.LastShutdownTime.Add(shutdownObservationFreshness)

		return bound, fmt.Sprintf("no lifecycle Job to measure against: the window is status.lastShutdownTime plus %v",
			shutdownObservationFreshness)
	}

	return time.Time{}, "the slot carries no shutdown stamp and no Job: the heartbeat is all the evidence there is"
}

// kubeletIsLive reports whether the Ready condition of a node is a live kubelet's observation and not
// the residue of one: the node must report NodeReady=True, carry a heartbeat, and - when the caller
// has a bound to measure against - have heartbeated after it. A zero bound (shutdownObservationBound
// spells out the shapes that produce one) leaves the heartbeat as the only evidence there is, and any
// heartbeat that says NodeReady=True is taken at face value.
//
// Clock caveat, because the comparison is across two clocks: lastHeartbeatTime is stamped by the
// *kubelet*, and the bound is stamped by the Job controller (completionTime) or by this manager
// (lastShutdownTime). On a node whose clock is skewed the comparison shifts by the skew - a machine
// running minutes behind can have its "up" read as "was off", and one minutes ahead can have a corpse
// read as "up". This repository cannot read the kubelet's clock, and the two directions are not equally
// expensive: the false "off" costs a bounded retry (the shutdown attempt bound, plus
// progress.ReconcileObservedPower's grace, which measures the same window on the manager's clock and
// the Job's), and the false "up" is what the guard exists to refuse. A cluster with real skew between
// its nodes and its control plane should be fixed at the source (the kubelet's own clock discipline),
// not worked around here.
func kubeletIsLive(kubeNode *corev1.Node, observedAfter time.Time) bool {
	for _, cond := range kubeNode.Status.Conditions {
		if cond.Type != corev1.NodeReady {
			continue
		}

		if cond.Status != corev1.ConditionTrue || cond.LastHeartbeatTime.IsZero() {
			return false
		}

		if observedAfter.IsZero() {
			return true
		}

		return cond.LastHeartbeatTime.After(observedAfter)
	}

	return false
}

// shutdownNotObservedNote renders the Warning: the Job that "succeeded", the machine that is up, the
// evidence the refusal was measured against, the progress the slot was recorded at, and what the
// controller will do next.
func shutdownNotObservedNote(nm *NodeStateMachine, job *batchv1.Job, boundNote string, attempts int) string {
	jobName := unknownField
	if job != nil {
		jobName = job.Name
	}

	retry := fmt.Sprintf(" This slot has spent %d of %d shutdown attempts; the controller stops firing the "+
		"shutdown at the limit and records ShutdownAttemptLimitReached, because a shutdown Job that keeps "+
		"exiting 0 on a machine that stays up is the pool's script, not the controller.",
		attempts, ShutdownAttemptLimit)

	return "Scale down of " + nm.lifecycleTarget() + " is NOT recorded as completed: shutdown Job " + jobName +
		" exited 0 while the machine is observed up (status.powerState on, Kubernetes node " + kubernetesNodeTarget(nm.node) +
		" Ready; measured against " + boundNote + "). The slot is recorded status.progress ready, not shutdown, because from " +
		"shutdown no operation on this slot is reachable - the FSM derives its state from the progress, so the pair " +
		"{powerState: on, progress: shutdown} is a slot that can only be healed by hand. The next reconcile runs the " +
		"shutdown again, on the backoff that refusal sets. Check that the Job's power-off call is the provider's own and " +
		"reports its failure: a script that exits 0 without the machine going off is the whole cause of this." + retry
}

// jobFromArgs returns the lifecycle Job a JobCompleted transition carried (the monitor passes the Job
// it read, and that Job's completionTime is the bound the guard measures the kubelet's heartbeat
// against), and nil when the caller had none.
func jobFromArgs(e *fsm.Event) *batchv1.Job {
	if e != nil {
		for _, arg := range e.Args {
			if job, ok := arg.(*batchv1.Job); ok && job != nil {
				return job
			}
		}
	}

	return nil
}

// jobNameFromArgs names that Job for a note, spelled out when the caller had none, so an event never
// quotes a blank field.
func jobNameFromArgs(e *fsm.Event) string {
	if job := jobFromArgs(e); job != nil {
		return job.Name
	}

	return unknownField
}

// kubernetesNodeTarget names the corev1.Node a slot maps to, spelled out when it has none.
func kubernetesNodeTarget(node *infrav1alpha1.Node) string {
	if node.Spec.KubernetesNodeName == "" {
		return "none"
	}

	return node.Spec.KubernetesNodeName
}
