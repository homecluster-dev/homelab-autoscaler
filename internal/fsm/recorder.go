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
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// Reasons recorded on the Node CR, so that `kubectl describe node.infra.homecluster.dev
// <name>` explains why a machine was powered on or off. They are read by humans and are
// greppable in the event stream - keep them stable, the messages carry the detail.
const (
	ReasonScaleUpRequested   = "ScaleUpRequested"
	ReasonScaleUpCompleted   = "ScaleUpCompleted"
	ReasonScaleUpFailed      = "ScaleUpFailed"
	ReasonScaleUpDeferred    = "ScaleUpDeferred"
	ReasonScaleDownRequested = "ScaleDownRequested"
	ReasonScaleDownCompleted = "ScaleDownCompleted"
	ReasonScaleDownFailed    = "ScaleDownFailed"
	ReasonScaleDownDeferred  = "ScaleDownDeferred"

	// ReasonReadinessGateTimeout is recorded when the scale-up gate (Ready, and free of the
	// readiness taints the Group declares) ran out of its deadline: the machine is on and was
	// uncordoned, and the slot is recorded ready, but the gate the pool's pods depend on was never
	// satisfied. It is a Warning because the operator has to go and look at the CNI - the churn the
	// gate closes is a machine paid for and unusable - and because the FSM has already done the only
	// safe thing it can (uncordon, complete the transition) and will not try again.
	ReasonReadinessGateTimeout = "ReadinessGateTimeout"
)

// Actions recorded on the Node CR. The new events API keeps `reason` (what happened,
// UpperCamelCase, stable, greppable) apart from `action` (what the controller did to the
// machine), so `kubectl get events -o wide` shows PowerOn/PowerOff without parsing notes.
const (
	ActionPowerOn  = "PowerOn"
	ActionPowerOff = "PowerOff"
	// ActionUncordon is the action the readiness gate and its timeout belong to: the controller
	// made the machine schedulable, which is the half of the scale-up that follows the power-on.
	ActionUncordon = "Uncordon"
)

// Bounds on recorded messages. An Event is a real API object: `kubectl describe` shows it,
// and etcd keeps it. Startup/shutdown Job failures and webhook denials can carry long,
// multi-line payloads, so they are flattened onto one line and cut off.
const (
	maxEventMessageLength = 512
	truncatedSuffix       = "... (truncated)"
)

// eventNoteFormat is the note passed to the new events API. The new API renders the note as
// a format string, and the lifecycle messages are already rendered: a Job failure or a
// webhook denial can carry a literal '%' in its payload, which Sprintf would then try to
// interpret. So the message always goes through as an argument to "%s".
const eventNoteFormat = "%s"

// Option customises a NodeStateMachine. Options exist so that the recorder is optional:
// callers that pass none (every existing caller, and the unit tests) get the historical
// behaviour of recording nothing.
type Option func(*NodeStateMachine)

// WithEventRecorder records Kubernetes Events on the Node CR for the lifecycle transitions
// the FSM performs: scale up/down requested, completed, and failed. A nil recorder - the
// default - records nothing, so the FSM stays usable in tests and in callers that have no
// manager to build a recorder from.
func WithEventRecorder(recorder events.EventRecorder) Option {
	return func(nm *NodeStateMachine) { nm.eventRecorder = recorder }
}

// WithReadinessGateTimeout overrides DefaultReadinessGateTimeout, the deadline of the gate that
// holds the transition into ready open until the node is Ready and free of the readiness taints.
// Tests use it to exercise a gate timeout in milliseconds; a caller with a CNI that is known to be
// slower (a large cluster, a pulled image, an encrypted overlay) can widen the budget for the whole
// controller.
func WithReadinessGateTimeout(timeout time.Duration) Option {
	return func(nm *NodeStateMachine) {
		if timeout > 0 {
			nm.readinessGateTimeout = timeout
		}
	}
}

// WithReadinessTaintKeys declares the taint keys the readiness gate waits for the node to be free
// of, on top of the Ready condition. The caller reads them from the slot's Group
// (spec.nodeReadinessTaints, whose CRD default is Cilium's node.cilium.io/agent-not-ready): the
// key is stamped by the CNI of the cluster, which this repository cannot verify, so it is
// configuration and not a constant. An empty list gates on Ready alone - the behaviour of a CNI
// that stamps no bootstrap taint, and the explicit way to turn the gate off.
func WithReadinessTaintKeys(keys []string) Option {
	return func(nm *NodeStateMachine) { nm.readinessTaintKeys = keys }
}

// WithInput is the input law: the triple (live claim phase, observed power, node usability) the
// state machine starts a pass in. A caller that passes none gets the observation alone - the slot's
// status.powerState, and no live claim - which is what a pass over a slot with no claim is.
//
// The input is never seeded from Node.status.phase. The report is this package's output, and reading
// it back as an input is the v1alpha1 ambiguity (a field that is both the machine's state and the
// world's report) the 0.1.20 watchdog and the 0.1.22 precedence rule existed to patch.
func WithInput(in phase.Input) Option {
	return func(nm *NodeStateMachine) {
		nm.input = in
		if in.Claim != nil {
			nm.claim = in.Claim
		}
	}
}

// WithClaim binds a lifecycle transition to the NodeClaim it is buying a machine for, and the writer
// that records the claim's phase moves. The claim is part of the input (the FSM's state is derived
// from it), so this option implies the claim half of WithInput.
func WithClaim(nodeClaim *infrav2alpha1.NodeClaim, writer ClaimWriter) Option {
	return func(nm *NodeStateMachine) {
		nm.claim = nodeClaim
		nm.claimWriter = writer
		nm.input.Claim = nodeClaim
	}
}

// recordLifecycleEvent attaches an Event to the Node CR. It is a no-op without a recorder.
//
// Callers must only invoke it from code that runs on an actual transition (the enter_/
// after_ hooks, and the failure sites in StartNode/ShutdownNode/monitorJobCompletion):
// it is deliberately not called from Reconcile, which runs on every resync.
//
// regarding is the Node CR and related stays nil: every lifecycle event is about the CR
// itself, the Job it acted on is named in the note. The note is eventNoteFormat, so the
// message is passed as an argument (see eventNoteFormat).
func (nm *NodeStateMachine) recordLifecycleEvent(eventType, reason, action, message string) {
	if nm.eventRecorder == nil {
		return
	}

	nm.eventRecorder.Eventf(nm.node, nil, eventType, reason, action,
		eventNoteFormat, truncateEventMessage(message))
}

// recordScaleFailure records a Warning event for a failed lifecycle operation, naming the
// underlying failure in the message. isScaleDown says which operation died: a scale down
// (ready -> shuttingdown) that dies rolls the slot back to ready, a scale up that dies
// rolls it back to shutdown, so the caller - not the resulting state - decides.
//
// A transition that lost the coordination lock is not a failure and does not come here: the
// caller reports it with recordScaleDeferral (see recordTransitionRefusal).
func (nm *NodeStateMachine) recordScaleFailure(isScaleDown bool, detail string) {
	if isScaleDown {
		nm.recordLifecycleEvent(corev1.EventTypeWarning, ReasonScaleDownFailed, ActionPowerOff,
			fmt.Sprintf("Scale down failed for %s: %s", nm.lifecycleTarget(), detail))
		return
	}

	nm.recordLifecycleEvent(corev1.EventTypeWarning, ReasonScaleUpFailed, ActionPowerOn,
		fmt.Sprintf("Scale up failed for %s: %s", nm.lifecycleTarget(), detail))
}

// recordScaleDeferral records a Normal event for a lifecycle operation that lost the
// coordination lock to the opposite operation on the same slot, and says when it comes back.
//
// It is not a Warning. Two operations colliding on one slot is the coordination lock doing
// what it is for: the slot is fine, the refused request is recorded in
// spec.desiredPowerState, and the controller requeues at the deadline the lock names. A
// Warning on that path is what a homelab drowns in - the live trace of this bug starts with
// one - and it tells the operator nothing the deferred operation has not already done.
// Real failures keep their Warning (see recordScaleFailure).
func (nm *NodeStateMachine) recordScaleDeferral(isScaleDown bool, detail string) {
	if isScaleDown {
		nm.recordLifecycleEvent(corev1.EventTypeNormal, ReasonScaleDownDeferred, ActionPowerOff,
			fmt.Sprintf("Scale down deferred for %s: %s", nm.lifecycleTarget(), detail))
		return
	}

	nm.recordLifecycleEvent(corev1.EventTypeNormal, ReasonScaleUpDeferred, ActionPowerOn,
		fmt.Sprintf("Scale up deferred for %s: %s", nm.lifecycleTarget(), detail))
}

// recordTransitionRefusal records a lifecycle transition that did not run, and it is the only
// writer of its event: the state of the slot says nothing about which operation was refused,
// so the caller says. A lost coordination lock is a deferral - the slot is fine, the request
// is in spec.desiredPowerState and the controller requeues at the lock's deadline - and is
// recorded as one. Everything else (the API server is unreachable, the lock annotations are
// unreadable) is a failure.
func (nm *NodeStateMachine) recordTransitionRefusal(isScaleDown bool, err error) {
	if conflict := AsLockConflict(err); conflict != nil {
		nm.recordScaleDeferral(isScaleDown, conflict.Deferral())
		return
	}

	nm.recordScaleFailure(isScaleDown, fmt.Sprintf("FSM transition failed: %s", errorDetail(err)))
}

// recordObservedJobFailure records the Warning for a lifecycle Job that the monitor saw
// fail. Call it before the JobFailed event: the state the FSM is still in (startingup vs
// shuttingdown) is what says which operation died, and the rollback changes it.
func (nm *NodeStateMachine) recordObservedJobFailure(job *batchv1.Job) {
	nm.recordScaleFailure(nm.fsm.Current() == StateShuttingDown, jobFailureDetail(job))
}

// lifecycleTarget names the machine a transition acts on: the Node CR, the group it
// belongs to, and the corev1.Node the slot maps to when that is known. A slot that has
// never been provisioned has no Kubernetes node yet, which is exactly the case operators
// ask about, so it is spelled out instead of printed empty.
func (nm *NodeStateMachine) lifecycleTarget() string {
	group := nm.node.Labels[LabelGroup]
	if group == "" {
		group = nm.node.Labels["infra.homecluster.dev/group"]
	}
	if group == "" {
		group = unknownField
	}

	if nm.node.Spec.KubernetesNodeName == "" {
		return fmt.Sprintf("node %s (group %s, no kubernetes node)", nm.node.Name, group)
	}

	return fmt.Sprintf("node %s (group %s, kubernetes node %s)",
		nm.node.Name, group, nm.node.Spec.KubernetesNodeName)
}

// errorDetail renders an error for an event message.
func errorDetail(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// jobFailureDetail renders why a lifecycle Job failed. The Job's own conditions carry the
// reason the controller manager recorded (BackoffLimitExceeded, PodFailed, ...); fall back
// to the failed-pod counter when there is nothing to quote.
func jobFailureDetail(job *batchv1.Job) string {
	if job == nil {
		return "lifecycle job failed"
	}

	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		if cond.Reason == "" && cond.Message == "" {
			continue
		}
		return fmt.Sprintf("job %s: %s %s", job.Name, cond.Reason, cond.Message)
	}

	return fmt.Sprintf("job %s reported %d failed pod(s)", job.Name, job.Status.Failed)
}

// truncateEventMessage flattens message onto a single line and cuts it to
// maxEventMessageLength, so a long Job error cannot make `kubectl describe` unreadable.
func truncateEventMessage(message string) string {
	// strings.Fields splits on every whitespace incl. newlines, tabs and CRs.
	oneLine := strings.Join(strings.Fields(message), " ")

	runes := []rune(oneLine)
	if len(runes) <= maxEventMessageLength {
		return oneLine
	}

	suffixRunes := []rune(truncatedSuffix)

	return string(runes[:maxEventMessageLength-len(suffixRunes)]) + truncatedSuffix
}
