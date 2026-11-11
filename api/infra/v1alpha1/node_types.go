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

type Progress string

const (
	ProgressShuttingDown Progress = "shuttingdown"
	ProgressShutdown     Progress = "shutdown"
	ProgressStartingUp   Progress = "startingup"
	ProgressReady        Progress = "ready"
)

// NodeSpec defines the desired state of Node
type NodeSpec struct {
	// +kubebuilder:validation:Enum=on;off
	DesiredPowerState  PowerState     `json:"powerState"` // "on" or "off"
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
	// ToBeDeletedByClusterAutoscaler and DeletionCandidateOfClusterAutoscaler. The node prefix is
	// a namespace, not an ownership claim: node.kubernetes.io/role=agent, the taint the k3d
	// setups use, is yours to declare.
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

	PowerState PowerState `json:"powerState"` // "on" or "off"
	Progress   Progress   `json:"progress"`

	// LastStartupTime is the last time the node was successfully started up.
	// +optional
	LastStartupTime *metav1.Time `json:"lastStartupTime,omitempty"`

	// LastShutdownTime is the last time the node was successfully shut down.
	// +optional
	LastShutdownTime *metav1.Time `json:"lastShutdownTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Powerstate",type=string,JSONPath=".status.powerState",description="Current power state"
// +kubebuilder:printcolumn:name="Progress",type=string,JSONPath=".status.progress",description="Current power progress"

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
