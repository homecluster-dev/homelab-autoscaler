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
