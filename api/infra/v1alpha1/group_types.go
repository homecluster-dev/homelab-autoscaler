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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Group CRD specification for homelab autoscaling groups
// JSON tags are required for all fields to ensure proper serialization

// GroupSpec defines the desired state of Group
type GroupSpec struct {
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// NodeSelector                     map[string]string `json:"nodeSelector"`

	// MinSize is the floor that cluster-autoscaler must not scale this group below. It is
	// reported to cluster-autoscaler as the min size of the node group, so a pool that has
	// to keep a machine up sets minSize: 1 and the autoscaler stops reclaiming that node
	// (no keepalive pod pinning the slot is needed any more).
	//
	// 0 is the default and the previous behaviour: the group may scale down to nothing.
	//
	// maxSize is not a field, it is derived from the number of Node CRs labelled with this
	// group, so the invariant minSize <= maxSize cannot be expressed in the CRD. It is
	// asserted at the gRPC boundary: a minSize larger than the number of slots is clamped
	// to it when the group is reported, and scale operations that would cross the floor are
	// refused.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinSize int32 `json:"minSize,omitempty"`

	ScaleDownUtilizationThreshold    string           `json:"scaleDownUtilizationThreshold"`
	ScaleDownGpuUtilizationThreshold string           `json:"scaleDownGpuUtilizationThreshold"`
	ScaleDownUnneededTime            *metav1.Duration `json:"scaleDownUnneededTime"`
	ScaleDownUnreadyTime             *metav1.Duration `json:"scaleDownUnreadyTime"`
	MaxNodeProvisionTime             *metav1.Duration `json:"maxNodeProvisionTime"`
	ZeroOrMaxNodeScaling             bool             `json:"zeroOrMaxNodeScaling"`
	IgnoreDaemonSetsUtilization      bool             `json:"ignoreDaemonSetsUtilization"`

	// NodeReadinessTaints are the taints a machine of this group must be free of before the
	// controller calls the slot ready and uncordons it, on top of the node reporting Ready.
	//
	// Why the field exists: the kubelet marks a node Ready long before the CNI has programmed its
	// datapath. Cilium's agent stamps node.cilium.io/agent-not-ready (NoExecute) on a node that has
	// just joined and removes it minutes later, and the pool's workloads do not tolerate it, so a
	// slot declared ready while the taint is on the node is read by cluster-autoscaler as capacity
	// its pending pod cannot land on: the autoscaler buys a second machine for a node that is about
	// to work. The controller holds the scale-up open until the taint is gone, so pool workloads do
	// not need a toleration for it.
	//
	// The list is the exact set the gate waits for, which makes it the knob for a CNI that stamps a
	// different key and the way to turn the gate off: an empty list gates on the Ready condition
	// alone, which is what a pool on a CNI that stamps no bootstrap taint declares. The CRD defaults
	// the field to Cilium's taint - Cilium has stamped this key since v1.9, and the pool this chart
	// deploys runs v1.16.x - so a Group written before the field existed keeps the gate it was
	// already getting.
	//
	// The key is what the gate matches: Kubernetes keeps one taint per key on a node object, so the
	// value and the effect of a declaration are documentation for the operator and are not compared.
	//
	// These are not the taints to apply to the machine, the gate only reads them. The taints to stamp
	// are spec.nodeTaints of the Node CR, and a key must not be in both: the taint this gate waits
	// for is stamped and removed by its owner, so a declaration that stamps it makes the slot's own
	// scale-up wait for a taint the slot keeps putting back. taints.IsProtectedKey refuses Cilium's
	// key in spec.nodeTaints for that reason.
	//
	// The gate is bounded: its deadline is 4m from the moment the node object appears
	// (fsm.DefaultReadinessGateTimeout), and a timeout uncordons the node, records a
	// ReadinessGateTimeout Warning on the slot and completes the transition, so a CNI that never
	// finishes never wedges the state machine and never leaves a cordoned machine that
	// cluster-autoscaler will not scale down.
	// +optional
	// +listType=atomic
	// +kubebuilder:default={{key: "node.cilium.io/agent-not-ready", effect: "NoExecute"}}
	NodeReadinessTaints []corev1.Taint `json:"nodeReadinessTaints,omitempty"`
}

// GroupStatus defines the observed state of Group.
type GroupStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// Health status of the group (healthy, offline, unknown)
	// +kubebuilder:validation:Enum=healthy;offline;unknown
	// +optional
	Health string `json:"health,omitempty"`

	// conditions represent the current state of the Group resource.
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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Group is the Schema for the groups API
type Group struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Group
	// +required
	Spec GroupSpec `json:"spec"`

	// status defines the observed state of Group
	// +optional
	Status GroupStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// GroupList contains a list of Group
type GroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Group `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Group{}, &GroupList{})
}
