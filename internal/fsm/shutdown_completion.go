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
//     Job - and the shutdown is retried until it lands;
//   - the window a hook cannot judge on the node object alone is the one the reconcilers' grace
//     exists for: a corev1.Node stays Ready for node-monitor-grace-period (~40s) after its kubelet
//     stops heartbeating, and the Job monitor polls every JobMonitorPollInterval (30s), so "Ready"
//     seconds after a power-off landed is not evidence that the machine is up - refusing the
//     completion on that reading would fire a second shutdown Job on every scale-down. The hook
//     therefore reads the heartbeat, not the condition: the observation only counts when the kubelet
//     checked in more than shutdownObservationFreshness after the stamp this shutdown left on the
//     slot (see machineObservedUp). A slot whose machine has just been powered off has no such
//     heartbeat and is recorded shut down, as it always was;
//   - what the hook still cannot do is wait. It runs inside the completion of an operation whose
//     monitor has already returned, so the precedence is enforced a second time, and with a horizon
//     that a hook does not have, by progress.ReconcileObservedPower
//     (internal/progress/observed_power.go): it applies the same rule once the slot's lifecycle Job
//     has aged out of SettleGracePeriod. That is the backstop, and the split is the one the watchdog
//     docs draw: this guard keeps a provable lie out of the completion write, the reconciler's rule
//     corrects a lie that rested.

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/looplab/fsm"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// shutdownObservationFreshness is how much newer than the start of the shutdown operation the
// kubelet's last heartbeat has to be for "the machine is up" to be evidence that the shutdown did
// not land.
//
// It exists because the hook cannot see what the reconciler's rule can. A corev1.Node stays Ready for
// node-monitor-grace-period (kube-controller-manager's default, 40s) after its kubelet stops checking
// in, and the lifecycle Job monitor polls every JobMonitorPollInterval (30s), so a shutdown that
// landed is, for up to that long, recorded against a node object that still says Ready. Refusing the
// completion on that reading would fire a second shutdown Job on every scale-down. The kubelet's
// heartbeat is the field that separates the two: it stops with the machine, so a heartbeat stamped
// more than this window after status.lastShutdownTime - the stamp the shutdown transition leaves - is
// a machine checking in now, and a heartbeat older than it is a corpse the node controller has not
// noticed yet.
const shutdownObservationFreshness = 60 * time.Second

// ReasonShutdownNotObserved is recorded on the Node CR when the shutdown Job of a slot completed and
// the machine it was supposed to power off is observed up: the slot is not recorded shut down,
// because a record of a machine that is up is the state that makes every further operation on the
// slot unreachable (see this file's audit).
const ReasonShutdownNotObserved = "ShutdownNotObserved"

// refuseUnobservedShutdown is the guard on the completion of a shutdown Job. It returns true when
// the machine the Job was supposed to power off is observed up: the completion is then not recorded
// as a shutdown, and the slot is moved to the state the observation says - ready - so the FSM can
// act on it again.
//
// Moving the machine's own state (looplab/fsm's SetState) is what makes the write legal rather than
// an exception: updateNodeProgress persists only the progress of the state the machine is in, and the
// state it is in after a shutdown that did not land is ready, because the machine is up. The after_
// hook of the same completion writes the state the machine is in for the same reason, so the two
// hooks of one completion cannot disagree, and every later writer of this slot reads a progress the
// cluster agrees with.
//
// From ready, with the claim off - the claim a completed shutdown always carries - the node
// controller's shutdown branch is reachable again and the next pass runs ShutdownNode, with its
// coordination lock, its cordon and its Job. The shutdown is retried until it lands, and the
// Warning names the Job that claimed it had.
func (nm *NodeStateMachine) refuseUnobservedShutdown(ctx context.Context) bool {
	if !nm.machineObservedUp(ctx) {
		return false
	}

	nm.fsm.SetState(StateReady)
	nm.updateNodeProgress(infrav1alpha1.ProgressReady)

	return true
}

// machineObservedUp reports whether the machine of the slot is observed up, by an observation current
// enough to be evidence against a completed shutdown: status.powerState on, the slot's corev1.Node
// reporting Ready, and that Ready condition's last heartbeat stamped more than
// shutdownObservationFreshness after the shutdown this completion is recording.
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
func (nm *NodeStateMachine) machineObservedUp(ctx context.Context) bool {
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

	return kubeletIsLive(kubeNode, nm.node.Status.LastShutdownTime)
}

// kubeletIsLive reports whether the Ready condition of a node is a live kubelet's observation and
// not the residue of one: the node must report NodeReady=True, carry a heartbeat, and - when the
// slot carries the stamp of the shutdown under way - have heartbeated more than
// shutdownObservationFreshness after it. A slot with no shutdown stamp has no operation to bound the
// observation against, and a status.progress: shutdown with no shutdown ever started is a record with
// nothing behind it: the heartbeat is all the evidence there is.
func kubeletIsLive(kubeNode *corev1.Node, shutdownStarted *metav1.Time) bool {
	for _, cond := range kubeNode.Status.Conditions {
		if cond.Type != corev1.NodeReady {
			continue
		}

		if cond.Status != corev1.ConditionTrue || cond.LastHeartbeatTime.IsZero() {
			return false
		}

		if shutdownStarted == nil {
			return true
		}

		bound := shutdownStarted.Add(shutdownObservationFreshness)

		return cond.LastHeartbeatTime.After(bound)
	}

	return false
}

// shutdownNotObservedNote renders the Warning: the Job that "succeeded", the machine that is up, the
// progress the slot was recorded at, and what the controller will do next.
func shutdownNotObservedNote(nm *NodeStateMachine, jobName string) string {
	return "Scale down of " + nm.lifecycleTarget() + " is NOT recorded as completed: shutdown Job " + jobName +
		" exited 0 while the machine is observed up (status.powerState on, Kubernetes node " + kubernetesNodeTarget(nm.node) +
		" Ready). The slot is recorded status.progress ready, not shutdown, because from shutdown no operation on this slot " +
		"is reachable - the FSM derives its state from the progress, so the pair {powerState: on, progress: shutdown} is a " +
		"slot that can only be healed by hand. The next reconcile runs the shutdown again. Check that the Job's power-off " +
		"call is the provider's own and reports its failure: a script that exits 0 without the machine going off is the " +
		"whole cause of this."
}

// jobNameFromArgs names the lifecycle Job a JobCompleted transition carried (the monitor passes the
// Job it read), spelled out when the caller had none, so an event never quotes a blank field.
func jobNameFromArgs(e *fsm.Event) string {
	if e != nil {
		for _, arg := range e.Args {
			if job, ok := arg.(*batchv1.Job); ok && job != nil {
				return job.Name
			}
		}
	}

	return "unknown"
}

// kubernetesNodeTarget names the corev1.Node a slot maps to, spelled out when it has none.
func kubernetesNodeTarget(node *infrav1alpha1.Node) string {
	if node.Spec.KubernetesNodeName == "" {
		return "none"
	}

	return node.Spec.KubernetesNodeName
}
