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

package v2alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeClaimPhase is the phase of a NodeClaim. pending and booting are the two phases a live claim
// can be in while the machine is being bought, ready is the claim's assertion that the machine it
// asked for can take the pool's work, and failed / completed are terminal.
type NodeClaimPhase string

const (
	// NodeClaimPending: the claim exists and no lifecycle Job has been created for it. This is the
	// only cancelable phase - deleting a pending claim cancels the request, because nothing is in
	// flight.
	NodeClaimPending NodeClaimPhase = "pending"
	// NodeClaimBooting: the startup Job exists and the power-on has been asked for. A boot that has
	// started is committed: no caller cancels it, and a delete of a booting claim runs the shutdown
	// path to completion (see the claim's finalizer), it does not abandon the machine.
	NodeClaimBooting NodeClaimPhase = "booting"
	// NodeClaimReady: the machine is up, joined, and free of the pool's readiness taints, so it can
	// take the pool's work. This is the gate the readiness gate of the scale-up answers for;
	// "the corev1.Node is Ready" alone is a report on the slot (Node.status.phase), not a claim.
	NodeClaimReady NodeClaimPhase = "ready"
	// NodeClaimFailed: the attempt did not produce a usable machine and never will. Terminal. The
	// machine is force-shut-down so an overdue boot cannot join and start serving capacity the
	// autoscaler has already given up on.
	NodeClaimFailed NodeClaimPhase = "failed"
	// NodeClaimCompleted: the claim ended with the machine it asked for available - a scale-down
	// landed on it, a manual claim's machine was handed over. Terminal.
	NodeClaimCompleted NodeClaimPhase = "completed"
)

// ClaimRequest.Call values: the gRPC entry point that created the claim. They are the enum of the
// spec field, spelled as constants so the provider, the claim controller and the docs quote one name.
const (
	ClaimCallIncreaseSize       = "IncreaseSize"
	ClaimCallDecreaseTargetSize = "DecreaseTargetSize"
	ClaimCallDeleteNodes        = "DeleteNodes"
)

// Claim reasons recorded on NodeClaim.status.reason, and mirrored into
// Node.status.lastClaim.reason. Free-form for an operator, and the set is open.
const (
	// ClaimReasonDeadlineExceeded: spec.deadlineAt passed while the claim was still pending/booting.
	ClaimReasonDeadlineExceeded = "DeadlineExceeded"
	// ClaimReasonStartupJobFailed: the startup Job of the attempt exited non-zero, or could not
	// be created.
	ClaimReasonStartupJobFailed = "StartupJobFailed"
	// ClaimReasonStartupJobTimeout: the startup Job of the attempt ran out of time.
	ClaimReasonStartupJobTimeout = "StartupJobTimeout"
	// ClaimReasonShutdownJobFailed: the shutdown Job of a scale-down exited non-zero, so the machine
	// it was supposed to power off is still up.
	ClaimReasonShutdownJobFailed = "ShutdownJobFailed"
	// ClaimReasonShutdownJobTimeout: the shutdown Job of a scale-down ran out of time.
	ClaimReasonShutdownJobTimeout = "ShutdownJobTimeout"
	// ClaimReasonStuckTransition: the lifecycle transition of the attempt was stuck past
	// fsm.StuckTransitionThreshold and was force-cleaned.
	ClaimReasonStuckTransition = "StuckTransition"
	// ClaimReasonReadinessGateTimeout: the machine booted but never cleared the readiness gate of
	// the Group (Ready plus free of spec.nodeReadinessTaints), so it is not a machine that can take
	// the pool's work.
	ClaimReasonReadinessGateTimeout = "ReadinessGateTimeout"
	// ClaimReasonAttemptLimitExceeded: the slot has used the consecutive-attempt bound, so no
	// further machine is bought for it until an attempt lands.
	ClaimReasonAttemptLimitExceeded = "AttemptLimitExceeded"
	// ClaimReasonCancelledByDecreaseTargetSize: NodeGroupDecreaseTargetSize deleted a pending claim.
	ClaimReasonCancelledByDecreaseTargetSize = "CancelledByDecreaseTargetSize"
	// ClaimReasonNodeDeleted: NodeGroupDeleteNodes named this slot's machine for deletion.
	ClaimReasonNodeDeleted = "NodeDeleted"
	// ClaimReasonMachineGone: the node object of a ready claim disappeared, and the provider
	// observes the machine off.
	ClaimReasonMachineGone = "MachineGone"
	// ClaimReasonSlotDeleted: the slot (Node CR) the claim asked for is going away.
	ClaimReasonSlotDeleted = "SlotDeleted"
	// ClaimReasonDeletedByHand: an operator deleted the claim, and the machine it asked for was
	// powered off by the finalizer path.
	ClaimReasonDeletedByHand = "DeletedByHand"
	// ClaimReasonSuperseded: a second live claim was found on one slot, and this one is the extra.
	// One live claim per slot is load-bearing, so the extra is failed, not merged.
	ClaimReasonSuperseded = "Superseded"
)

// Claim condition types recorded on NodeClaim.status.conditions.
const (
	// ClaimConditionReady is True while the claim's machine can take the pool's work.
	ClaimConditionReady = "Ready"
	// ClaimConditionMachineCommitted is True once the startup Job of the attempt exists, i.e. from
	// pending -> booting on. It is the record that the attempt bought a machine, and that the claim
	// is no longer cancelable.
	//
	// On a terminal move it is read off the edge the claim moved along, and it keeps its meaning:
	// True for an attempt that ended from booting or ready, because a machine was committed and is
	// the thing the input law powers off; False for an attempt that ended from pending, because
	// nothing was ever bought. It is a record for an operator, never an instruction to a
	// reconciler (claim.Recorder.stampTerminalConditions, docs/architecture/state.md).
	ClaimConditionMachineCommitted = "MachineCommitted"
	// ClaimConditionMachineObservedOff is True while the machine of a claim that reached ready is
	// observed off. Its lastTransitionTime is the instant the loss was first recorded, and it is what
	// the MachineGone bound is measured from: the observation has to outlast a whole boot window
	// before the model calls a machine gone, because a reboot shows the same shape and is not one
	// (docs/design/nodeclaim.md §4). Cleared when the machine comes back.
	ClaimConditionMachineObservedOff = "MachineObservedOff"
)

// NodeClaimSpec defines the desired state of NodeClaim: one request for one machine.
//
// The spec is written by the autoscaler (the gRPC provider creates claims on IncreaseSize and
// records requests on DeleteNodes) and by an operator making a manual claim. It is immutable in
// practice: a claim is a record of a request, and changing a request means making a new claim.
type NodeClaimSpec struct {
	// SlotRef names the Node CR (the slot) this claim asks for. Name is the only field that
	// matters; one live claim per slot is enforced by the admission webhook and by the claim
	// controller, because a CRD cannot express cross-object uniqueness.
	// +kubebuilder:validation:Required
	SlotRef corev1.ObjectReference `json:"slotRef"`

	// RequestedAt is when the request was made. The deadline budget is measured from it, so a
	// claim created in the past (a manual claim, a replay) gets the budget it would have had.
	// +optional
	RequestedAt metav1.Time `json:"requestedAt,omitempty"`

	// RequestedBy is free text: the identity of the caller, for an operator reading
	// `kubectl get nodeclaim`. "cluster-autoscaler", a manager's name, a human's kubectl in a
	// manual claim. It is not parsed.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	RequestedBy string `json:"requestedBy,omitempty"`

	// Request is structured attribution: which RPC asked for this machine, and with what
	// arguments. This is the field that answers "who asked, why, and for what", the question the
	// v1 scalar could not.
	// +kubebuilder:validation:Required
	Request ClaimRequest `json:"request"`

	// DeadlineAt is the budget for this attempt: the operator's budget
	// (job monitor poll + registration wait + readiness gate + margin), floored by the group's
	// spec.maxNodeProvisionTime. Overdue means failed, and failed means force shutdown.
	// +optional
	DeadlineAt *metav1.Time `json:"deadlineAt,omitempty"`

	// MaxNodeProvisionTime is the group's spec.maxNodeProvisionTime as it was when the claim was
	// created, copied onto the claim for reporting: a claim says what budget was in force, and a
	// pool that changes the number does not rewrite history. It is the floor of DeadlineAt, because
	// cluster-autoscaler is told the same number as its own scale-up termination deadline
	// (NodeGroupGetOptions -> MaxNodeProvisionDuration) and giving up on a slot before it gives up
	// is a claim the autoscaler is still waiting for.
	// +optional
	MaxNodeProvisionTime *metav1.Duration `json:"maxNodeProvisionTime,omitempty"`

	// TTLSecondsAfterFinished is how long a terminal claim (failed, completed) is kept after it
	// lands, before the claim controller collects it. The slot keeps the story in
	// Node.status.lastClaim, so collection is safe.
	// +optional
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// ClaimRequest is the attribution of a scale-up request.
type ClaimRequest struct {
	// Call is the gRPC entry point that created this claim.
	// +kubebuilder:validation:Enum=IncreaseSize;DecreaseTargetSize;DeleteNodes
	// +kubebuilder:validation:Required
	Call string `json:"call"`

	// Delta is the request's delta, signed: +N for IncreaseSize, -N for DecreaseTargetSize.
	// Empty for DeleteNodes, which names nodes.
	// +optional
	Delta int32 `json:"delta,omitempty"`

	// Group is the node group (the Group CR name) the request was for.
	// +kubebuilder:validation:Required
	Group string `json:"group"`

	// Nodes are the Kubernetes node names from a DeleteNodes request. Only set for
	// Call: DeleteNodes.
	// +optional
	// +listType=atomic
	Nodes []string `json:"nodes,omitempty"`
}

// NodeClaimStatus defines the observed state of NodeClaim.
type NodeClaimStatus struct {
	// Phase is the phase of the claim. See NodeClaimPhase for who writes each move.
	//
	// No status default: the status subresource strips `status` from a create, so a default here is
	// only stamped by the first status write and buys nothing in the window the model cares about. A
	// claim with no phase is read as pending by phase.IsLive / phase.IsPending, which is the single
	// place the reading lives (docs/design/nodeclaim.md §3).
	// +kubebuilder:validation:Enum=pending;booting;ready;failed;completed
	// +optional
	Phase NodeClaimPhase `json:"phase,omitempty"`

	// Attempt is 1..N for this slot: the number of machines the slot has been asked to buy in a
	// row. It is seeded from Node.status.lastClaim.attempt at creation, incremented by the
	// pending -> booting write (the attempt is a machine bought, not a reconcile pass), and bounded
	// per slot by the consecutive streak the digest records.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Attempt int32 `json:"attempt,omitempty"`

	// Reason is the one-line outcome, for `kubectl get nodeclaim`: DeadlineExceeded,
	// StartupJobFailed, CancelledByDecreaseTargetSize, NodeDeleted.
	// +optional
	Reason string `json:"reason,omitempty"`

	// ShutdownAttempts is how many times the scale-down that named this claim has fired the shutdown
	// of a machine the provider still observes up. It is a counter of its own, separate from
	// status.attempt: status.attempt counts machines the request bought (the boot bound of §4), and a
	// scale-down that cannot land must not spend the boot attempts of the machine it is draining -
	// that number is the record of the episode, and it seeds the next claim's attempt.
	//
	// The streak ends with the claim, and it resets: when the machine is observed off the claim ends
	// completed, and when a reset grace passes with the machine still up the count goes back to 0 and
	// the shutdown fires again, so a pool whose shutdown script has been fixed is picked up by the next
	// pass (docs/design/nodeclaim.md §4, docs/architecture/state.md).
	// +optional
	// +kubebuilder:validation:Minimum=0
	ShutdownAttempts int32 `json:"shutdownAttempts,omitempty"`

	// LastShutdownAttemptAt is when the last shutdown attempt of this claim was counted. It is the
	// clock of the reset grace above, and the timestamp an operator reads for "the controller stopped
	// firing this shutdown when".
	// +optional
	LastShutdownAttemptAt *metav1.Time `json:"lastShutdownAttemptAt,omitempty"`

	// FinishedAt is when the claim reached a terminal phase. The TTL is measured from it, and it is
	// one of the times the slot's digest carries.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:shortName=nc,categories=homelab-autoscaler
// +kubebuilder:printcolumn:name="Slot",type=string,JSONPath=`.spec.slotRef.name`,description="Slot (Node CR) this claim asks for"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="pending, booting, ready, failed or completed"
// +kubebuilder:printcolumn:name="Attempt",type=integer,JSONPath=`.status.attempt`,description="Attempt number for this slot"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`,description="One-line outcome"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`,description="Time the request was made"
// +kubebuilder:printcolumn:name="Deadline",type=date,JSONPath=`.spec.deadlineAt`,priority=1,description="Budget of the attempt"

// NodeClaim is one request for one machine: the scale-up intent of the autoscaler, with an
// identity, an attribution, a deadline and an end. It is the object the v1alpha1 scalar
// spec.powerState could not be. See docs/design/nodeclaim.md.
type NodeClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of NodeClaim
	// +required
	Spec NodeClaimSpec `json:"spec"`

	// status defines the observed state of NodeClaim
	// +optional
	Status NodeClaimStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// NodeClaimList contains a list of NodeClaim
type NodeClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeClaim{}, &NodeClaimList{})
}
