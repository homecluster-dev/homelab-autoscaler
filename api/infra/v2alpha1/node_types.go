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
	"encoding/json"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PowerState string

const (
	PowerStateOn  PowerState = "on"
	PowerStateOff PowerState = "off"
)

// UnmarshalJSON implements custom JSON unmarshaling for PowerState
func (p *PowerState) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	switch s {
	case "on", "true":
		*p = PowerStateOn
	case "off", "false":
		*p = PowerStateOff
	default:
		*p = PowerState(s) // fallback to original value
	}
	return nil
}

// MarshalJSON implements custom JSON marshaling for PowerState
func (p PowerState) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(p))
}

// UnmarshalYAML implements custom YAML unmarshaling for PowerState
func (p *PowerState) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}

	switch s {
	case "on", "true":
		*p = PowerStateOn
	case "off", "false":
		*p = PowerStateOff
	default:
		*p = PowerState(s) // fallback to original value
	}
	return nil
}

// MarshalYAML implements custom YAML marshaling for PowerState
func (p PowerState) MarshalYAML() (interface{}, error) {
	return string(p), nil
}

// NodePhase is the derived, reported phase of a slot: a summary of (the live NodeClaim of the
// slot, the power the provider observes, the liveness of the kubelet). It is written by the
// observation path and by the lifecycle transition path, both of which compute it from those three
// inputs with the one function phase.Derive, and it is never read to decide an action: the state
// machine's input is the live claim and the observation, not this record. See
// docs/design/nodeclaim.md §6 and docs/architecture/state.md.
type NodePhase string

const (
	// NodePhaseOff: no live claim, no observed machine.
	NodePhaseOff NodePhase = "off"
	// NodePhasePending: a live claim in pending, and no lifecycle Job for it yet.
	NodePhasePending NodePhase = "pending"
	// NodePhaseBooting: a live claim whose startup operation is in flight.
	NodePhaseBooting NodePhase = "booting"
	// NodePhaseReady: a live claim in ready, and a node that is observed Ready and gate-clear.
	NodePhaseReady NodePhase = "ready"
	// NodePhaseDraining: a claim marked for scale-down, or a machine the pool no longer wants,
	// with the node cordoned and the machine still observed up.
	NodePhaseDraining NodePhase = "draining"
)

// NodeSpec defines the desired state of Node: what a slot *is*, and nothing about what anyone
// wants it to do next.
//
// In v1alpha1 this struct carried spec.powerState, a scalar that served as the scale-up request,
// the lock, the target-size arithmetic and the history at once. It is gone in v2alpha1, and there is
// no field in the spec for any of that: desired power is derived, never stored, and a slot is
// wanted exactly when a live NodeClaim names it (docs/design/nodeclaim.md §2). Git is
// the only writer of this spec, so an apply can never contradict a scale-up, and a scale-up can
// never overwrite a declaration.
type NodeSpec struct {
	StartupPodSpec     MinimalPodSpec `json:"startupPodSpec"`
	ShutdownPodSpec    MinimalPodSpec `json:"shutdownPodSpec"`
	KubernetesNodeName string         `json:"kubernetesNodeName"`
	Pricing            PricingSpec    `json:"pricing"`
	// TaintsToRemove is a list of taints that should be stripped from the node when generating a template.
	// It allows users to declare custom taints that the autoscaler must ignore.
	//
	// It is unrelated to NodeTaints: this field filters taints out of the node template,
	// NodeTaints puts them in. Do not conflate them.
	// +optional
	TaintsToRemove []corev1.Taint `json:"taintsToRemove,omitempty"`

	// NodeTaints are the taints this slot's machine must carry.
	//
	// What the field gives you: what cluster-autoscaler simulates matches what the pool boots
	// with, the taints are declared in Git, and they survive node recreation. The provider puts
	// them in the node template it reports for scale-up simulation, so the autoscaler sees
	// dedicated=remote:NoSchedule before the machine has registered and stops placing pods on a
	// node that rejects them.
	//
	// The controller applies them to the Kubernetes node as soon as the node object exists: the
	// scale-up path stamps them in the same hook that uncordons the node, so the machine is
	// tainted before it becomes schedulable, and every later reconcile re-applies them. A taint
	// you remove by hand with `kubectl taint <node> <key>-` is put back on the next reconcile:
	// the field only adds, and the declaration is the intent.
	//
	// Keys a Kubernetes component owns are skipped, and the skip is recorded as a Warning event
	// on the Node CR: the node lifecycle controller and the kubelet own node.kubernetes.io/
	// not-ready, unreachable, unschedulable, out-of-service, memory-pressure, disk-pressure,
	// pid-pressure and network-unavailable, cluster-autoscaler owns
	// ToBeDeletedByClusterAutoscaler and DeletionCandidateOfClusterAutoscaler, and Cilium owns
	// node.cilium.io/agent-not-ready (the bootstrap taint the scale-up readiness gate waits for,
	// see the Group's spec.nodeReadinessTaints - declaring it here makes the slot's own scale-up
	// wait for a taint the slot keeps putting back). The node prefix is a namespace, not an
	// ownership claim: node.kubernetes.io/role=agent, the taint the k3d setups use, is yours to
	// declare.
	//
	// Declare taints here instead of tainting from a startup Job. A Job can only taint a node
	// that already joined, which is after the autoscaler simulated a placement on an untainted
	// template and after the node was schedulable, so pods get bound to a machine that rejects
	// them.
	//
	// The window this closes is the one the controller drives. It can only patch a node that
	// exists, so a machine you booted by hand, or one that registered while the controller was
	// down, can be Ready and schedulable for a pass before the taint lands. Keep the tolerations
	// of your own startup tooling correct.
	// +optional
	NodeTaints []corev1.Taint `json:"nodeTaints,omitempty"`

	// NodeCapacity is the shape of the machine this slot boots: the resources the kubelet reports
	// as allocatable once the machine joins, not the nominal numbers the hardware is sold as.
	//
	// What the field gives you: a pool whose every slot is powered off can scale up. The node
	// template cluster-autoscaler simulates a placement on is built from a corev1.Node, and a pool
	// at zero has none, so the provider synthesizes a placeholder. Capacity is the one part of the
	// machine's shape a Node CR can supply for that placeholder: with the field unset the template
	// carries no cpu and no memory, no pod with resource requests fits on it, and the autoscaler
	// never calls NodeGroupIncreaseSize, which is why a minSize: 0 pool never comes back up and the
	// workaround is minSize: 1 with a machine that never sleeps.
	//
	// It is declared in the spec rather than copied into the status because the template is needed
	// before a slot has ever booted: a status field fed from a live node is empty in exactly the
	// case it would be read for, and keeping it filled means a pipeline that watches nodes and
	// writes status. One field next to the taints of the same machine is reviewable in Git, and it
	// describes a machine that does not exist yet.
	//
	// Precedence is per resource, not per list: every resource you name replaces what the live node
	// reports in status.capacity and status.allocatable, on the path that copies a registered node
	// as well as on the placeholder, so the shape the autoscaler simulates does not change with
	// which slot of the group happens to be Ready. A resource you do not name keeps the live node's
	// number, which is how pods, ephemeral-storage, hugepages and any GPU or device plugin resource
	// of the real machine survive a cpu-and-memory declaration. Declare every resource your
	// workloads request, because a resource you leave out is not checked (see below) and an
	// incomplete declaration is an incomplete machine to the estimator. You do not have to declare
	// pods: a template that carries none gets the kubelet's default pod ceiling (110) filled in, the
	// scheduler rejects every pod on a node with no pods in its allocatable, and the estimator then
	// finds no placement and never calls NodeGroupIncreaseSize. When the field is unset the live
	// node's own numbers are used, which is the behaviour of every pool that declares nothing.
	//
	// The values must match what the kubelet reports, which is not what the datasheet says: system
	// reservations make a 2 vCPU / 4 GiB machine, a Hetzner CX23 for instance, report 1950m and
	// 3.2Gi. Declare what the node reports, not what the box is sold as. The autoscaler judges pod
	// requests against this list, so a declaration larger than reality places work the machine
	// cannot take and a smaller one leaves capacity unused.
	//
	// The controller compares the declaration with the machine once it registers: a
	// status.allocatable that differs from spec.nodeCapacity by more than five percent on any
	// resource puts a Warning event on the Node CR, reason NodeCapacityMismatch, naming the
	// resource and both values. It warns only, and it compares only the resources you declared: a
	// resource you omitted, memory for instance, is never reported as drift, so a workload that
	// asks for it is placed on a template whose size for it is unknown.
	// +optional
	NodeCapacity corev1.ResourceList `json:"nodeCapacity,omitempty"`
}

type PricingSpec struct {
	HourlyRate string `json:"hourlyRate"`
	PodRate    string `json:"podRate"`
}

type MinimalPodSpec struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Each volume mounts a Secret or ConfigMap
	Volumes        []VolumeMountSpec `json:"volumes,omitempty"`
	ServiceAccount *string           `json:"serviceAccount,omitempty"`
	// PriorityClassName is assigned to the Pod created by the startup/shutdown Job.
	// Use it to keep power-management Pods schedulable (and non-preemptible) under
	// resource pressure, e.g. "system-cluster-critical". Empty means the cluster
	// default (priority 0), which is today's behaviour.
	// +optional
	PriorityClassName *string `json:"priorityClassName,omitempty"`
	// TTLSecondsAfterFinished limits the lifetime of the Job.
	// Defaults to 600 seconds (10 minutes) if not specified.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

type VolumeMountSpec struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	// Either SecretName or ConfigMapName should be set
	SecretName    string `json:"secretName,omitempty"`
	ConfigMapName string `json:"configMapName,omitempty"`
}

// NodeStatus defines the observed state of Node.
type NodeStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Node resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +kubebuilder:validation:Enum=healthy;offline;unknown
	// +optional
	Health string `json:"health,omitempty"`

	// PowerState is the power the provider observes for this slot's machine: on when the
	// corev1.Node reports NodeReady=True, off otherwise. It is an observation, not an intent, and
	// it is the input the state machine reads ("observed power") next to the live claim.
	// +kubebuilder:validation:Enum=on;off
	// +optional
	PowerState PowerState `json:"powerState,omitempty"` // "on" or "off"

	// Phase is the derived report of the slot: see NodePhase. Written from
	// (live claim, observed power, node usability), read by nobody in the operator, and
	// present so that `kubectl get nodes.infra.homecluster.dev` keeps one column that tells an
	// operator what the slot is doing.
	// +kubebuilder:validation:Enum=off;pending;booting;ready;draining
	// +optional
	Phase NodePhase `json:"phase,omitempty"`

	// LastClaim is the digest of the most recent NodeClaim of this slot. Claims are collected
	// after they land (TTL), and this is what survives the collection, so that
	// `kubectl describe node.infra.homecluster.dev <slot>` still answers "who asked, when, how
	// many attempts, why did it end". Written by the claim controller, read by nobody to decide
	// an action; the next claim's status.attempt is seeded from it.
	// +optional
	LastClaim *LastClaimSummary `json:"lastClaim,omitempty"`

	// LastStartupTime is the last time the node was successfully started up.
	// +optional
	LastStartupTime *metav1.Time `json:"lastStartupTime,omitempty"`

	// LastShutdownTime is the last time the node was successfully shut down.
	// +optional
	LastShutdownTime *metav1.Time `json:"lastShutdownTime,omitempty"`
}

// LastClaimSummary is the digest of the last NodeClaim of a slot. Its field set is the design's
// (docs/design/nodeclaim.md §8): the story of the episode that ended, without a second command.
type LastClaimSummary struct {
	// Name is the name of the claim, so an operator can find it while it still exists.
	// +optional
	Name string `json:"name,omitempty"`

	// Phase is the phase the claim ended in: failed and completed are terminal, and this is the
	// sentence "the last attempt of this slot ended like this".
	// +kubebuilder:validation:Enum=pending;booting;ready;failed;completed
	// +optional
	Phase NodeClaimPhase `json:"phase,omitempty"`

	// Attempt is the attempt number the claim carried (1..N for the slot). Seeding the next
	// claim reads it; see the reset rule in internal/claim.SeedAttempt.
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// ShutdownAttempts is the scale-down attempts the claim had spent when the episode ended. It is
	// the record the slot reconciler reads to decide whether to keep firing a shutdown on a machine
	// that stays up when no claim of the slot is left to carry the count (see
	// claim.ScaleDownStreakSpent), and it is a counter of its own: the boot attempts of the episode are
	// in Attempt, and a shutdown that will not land must not spend them.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ShutdownAttempts int32 `json:"shutdownAttempts,omitempty"`

	// LastShutdownAttemptAt is when the last of those attempts was counted. The streak reset's grace is
	// measured from it, so a slot whose pool script has been fixed is not sentenced by the attempts of
	// the episodes that ended (docs/design/nodeclaim.md §4).
	// +optional
	LastShutdownAttemptAt *metav1.Time `json:"lastShutdownAttemptAt,omitempty"`

	// Call is the gRPC entry point that created the claim (IncreaseSize, DecreaseTargetSize,
	// DeleteNodes) - the attribution of the request that owned this slot.
	// +optional
	Call string `json:"call,omitempty"`

	// Group is the node group the request was for.
	// +optional
	Group string `json:"group,omitempty"`

	// Reason is the one-line outcome: DeadlineExceeded, StartupJobFailed, ReadinessGateTimeout,
	// CancelledByDecreaseTargetSize, NodeDeleted, AttemptLimitExceeded.
	// +optional
	Reason string `json:"reason,omitempty"`

	// RequestedAt is when the request was made.
	// +optional
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`

	// DeadlineAt is the budget the claim carried.
	// +optional
	DeadlineAt *metav1.Time `json:"deadlineAt,omitempty"`

	// FinishedAt is when the claim reached its terminal phase.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Powerstate",type=string,JSONPath=".status.powerState",description="Current power state"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase",description="Derived phase of the slot (live claim + observed power + node usability)"

// Node is the Schema for the nodes API
type Node struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Node
	// +required
	Spec NodeSpec `json:"spec"`

	// status defines the observed state of Node
	// +optional
	Status NodeStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// NodeList contains a list of Node
type NodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Node `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Node{}, &NodeList{})
}
