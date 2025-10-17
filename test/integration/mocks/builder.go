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

package mocks

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// MockClientBuilder provides a fluent interface for building mock clients with predefined data
type MockClientBuilder struct {
	client *MockClient
	scheme *runtime.Scheme
}

// NewMockClientBuilder creates a new builder for mock clients
func NewMockClientBuilder(scheme *runtime.Scheme) *MockClientBuilder {
	return &MockClientBuilder{
		client: NewMockClient(scheme),
		scheme: scheme,
	}
}

// WithGroup adds a Group CR to the mock client
func (b *MockClientBuilder) WithGroup(name string, opts ...GroupOption) *MockClientBuilder {
	group := &infrav1alpha1.Group{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.homecluster.dev/v1alpha1",
			Kind:       "Group",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "homelab-autoscaler-system",
			UID:       types.UID(fmt.Sprintf("group-%s-uid", name)),
			Labels:    make(map[string]string),
		},
		Spec: infrav1alpha1.GroupSpec{
			ScaleDownUtilizationThreshold:    "0.5",
			ScaleDownGpuUtilizationThreshold: "0.5",
			ScaleDownUnneededTime:            &metav1.Duration{Duration: 10 * time.Minute},
			ScaleDownUnreadyTime:             &metav1.Duration{Duration: 20 * time.Minute},
			MaxNodeProvisionTime:             &metav1.Duration{Duration: 15 * time.Minute},
			ZeroOrMaxNodeScaling:             false,
			IgnoreDaemonSetsUtilization:      true,
		},
		Status: infrav1alpha1.GroupStatus{
			Health: "healthy",
		},
	}

	// Apply options
	for _, opt := range opts {
		opt(group)
	}

	_ = b.client.AddObject(group)
	return b
}

// WithNode adds a Node CR to the mock client
func (b *MockClientBuilder) WithNode(name string, opts ...NodeOption) *MockClientBuilder {
	node := &infrav1alpha1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.homecluster.dev/v1alpha1",
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "homelab-autoscaler-system",
			UID:       types.UID(fmt.Sprintf("node-%s-uid", name)),
			Labels:    make(map[string]string),
		},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  infrav1alpha1.PowerStateOn,
			KubernetesNodeName: name,
			StartupPodSpec: infrav1alpha1.MinimalPodSpec{
				Image:   "startup:latest",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "echo 'starting up'"},
			},
			ShutdownPodSpec: infrav1alpha1.MinimalPodSpec{
				Image:   "shutdown:latest",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "echo 'shutting down'"},
			},
			Pricing: infrav1alpha1.PricingSpec{
				HourlyRate: "0.10",
				PodRate:    "0.01",
			},
		},
		Status: infrav1alpha1.NodeStatus{
			Health:     "healthy",
			PowerState: infrav1alpha1.PowerStateOn,
			Progress:   infrav1alpha1.ProgressReady,
		},
	}

	// Apply options
	for _, opt := range opts {
		opt(node)
	}

	_ = b.client.AddObject(node)
	return b
}

// WithKubernetesNode adds a Kubernetes Node to the mock client
func (b *MockClientBuilder) WithKubernetesNode(name string, opts ...KubernetesNodeOption) *MockClientBuilder {
	node := &corev1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(fmt.Sprintf("k8s-node-%s-uid", name)),
			Labels: map[string]string{
				"kubernetes.io/hostname": name,
			},
		},
		Spec: corev1.NodeSpec{
			ProviderID: fmt.Sprintf("homelab://%s", name),
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3.8"),
				corev1.ResourceMemory: resource.MustParse("7.5Gi"),
			},
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
					Reason: "KubeletReady",
				},
			},
		},
	}

	// Apply options
	for _, opt := range opts {
		opt(node)
	}

	_ = b.client.AddObject(node)
	return b
}

// Build returns the configured mock client
func (b *MockClientBuilder) Build() client.Client {
	return b.client
}

// GroupOption is a function that modifies a Group CR
type GroupOption func(*infrav1alpha1.Group)

// WithGroupHealth sets the health status of a Group
func WithGroupHealth(health string) GroupOption {
	return func(g *infrav1alpha1.Group) {
		g.Status.Health = health
	}
}

// WithGroupLabel adds a label to a Group
func WithGroupLabel(key, value string) GroupOption {
	return func(g *infrav1alpha1.Group) {
		if g.Labels == nil {
			g.Labels = make(map[string]string)
		}
		g.Labels[key] = value
	}
}

// WithGroupScaleDownThreshold sets the scale down utilization threshold
func WithGroupScaleDownThreshold(threshold string) GroupOption {
	return func(g *infrav1alpha1.Group) {
		g.Spec.ScaleDownUtilizationThreshold = threshold
	}
}

// NodeOption is a function that modifies a Node CR
type NodeOption func(*infrav1alpha1.Node)

// WithNodeGroup assigns a Node to a Group
func WithNodeGroup(groupName string) NodeOption {
	return func(n *infrav1alpha1.Node) {
		if n.Labels == nil {
			n.Labels = make(map[string]string)
		}
		n.Labels["group"] = groupName
	}
}

// WithNodePowerState sets the power state of a Node
func WithNodePowerState(desired, actual infrav1alpha1.PowerState) NodeOption {
	return func(n *infrav1alpha1.Node) {
		n.Spec.DesiredPowerState = desired
		n.Status.PowerState = actual
	}
}

// WithNodeProgress sets the progress status of a Node
func WithNodeProgress(progress infrav1alpha1.Progress) NodeOption {
	return func(n *infrav1alpha1.Node) {
		n.Status.Progress = progress
	}
}

// WithNodeHealth sets the health status of a Node
func WithNodeHealth(health string) NodeOption {
	return func(n *infrav1alpha1.Node) {
		n.Status.Health = health
	}
}

// WithNodePricing sets the pricing information for a Node
func WithNodePricing(hourlyRate, podRate string) NodeOption {
	return func(n *infrav1alpha1.Node) {
		n.Spec.Pricing.HourlyRate = hourlyRate
		n.Spec.Pricing.PodRate = podRate
	}
}

// WithNodeLabel adds a label to a Node
func WithNodeLabel(key, value string) NodeOption {
	return func(n *infrav1alpha1.Node) {
		if n.Labels == nil {
			n.Labels = make(map[string]string)
		}
		n.Labels[key] = value
	}
}

// WithNodeLastStartupTime sets the last startup time
func WithNodeLastStartupTime(t time.Time) NodeOption {
	return func(n *infrav1alpha1.Node) {
		metaTime := metav1.NewTime(t)
		n.Status.LastStartupTime = &metaTime
	}
}

// KubernetesNodeOption is a function that modifies a Kubernetes Node
type KubernetesNodeOption func(*corev1.Node)

// WithKubernetesNodeLabel adds a label to a Kubernetes Node
func WithKubernetesNodeLabel(key, value string) KubernetesNodeOption {
	return func(n *corev1.Node) {
		if n.Labels == nil {
			n.Labels = make(map[string]string)
		}
		n.Labels[key] = value
	}
}

// WithKubernetesNodeGroupLabel adds the group label to a Kubernetes Node
func WithKubernetesNodeGroupLabel(groupName string) KubernetesNodeOption {
	return func(n *corev1.Node) {
		if n.Labels == nil {
			n.Labels = make(map[string]string)
		}
		n.Labels["infra.homecluster.dev/group"] = groupName
	}
}

// WithKubernetesNodeCondition sets a condition on a Kubernetes Node
func WithKubernetesNodeCondition(conditionType corev1.NodeConditionType, status corev1.ConditionStatus,
	reason string) KubernetesNodeOption {
	return func(n *corev1.Node) {
		// Find existing condition or add new one
		found := false
		for i, condition := range n.Status.Conditions {
			if condition.Type == conditionType {
				n.Status.Conditions[i].Status = status
				n.Status.Conditions[i].Reason = reason
				found = true
				break
			}
		}
		if !found {
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
				Type:   conditionType,
				Status: status,
				Reason: reason,
			})
		}
	}
}

// WithKubernetesNodeResources sets the capacity and allocatable resources
func WithKubernetesNodeResources(cpu, memory string) KubernetesNodeOption {
	return func(n *corev1.Node) {
		n.Status.Capacity = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}
		// Set allocatable to slightly less than capacity
		cpuQuantity := resource.MustParse(cpu)
		memQuantity := resource.MustParse(memory)

		// Reduce by 5% for allocatable
		cpuMillis := cpuQuantity.MilliValue() * 95 / 100
		memBytes := memQuantity.Value() * 95 / 100

		n.Status.Allocatable = corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(memBytes, resource.BinarySI),
		}
	}
}

// WithKubernetesNodeTaint adds a taint to a Kubernetes Node
func WithKubernetesNodeTaint(key, value string, effect corev1.TaintEffect) KubernetesNodeOption {
	return func(n *corev1.Node) {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
			Key:    key,
			Value:  value,
			Effect: effect,
		})
	}
}

// WithKubernetesNodeUnschedulable sets the unschedulable flag
func WithKubernetesNodeUnschedulable(unschedulable bool) KubernetesNodeOption {
	return func(n *corev1.Node) {
		n.Spec.Unschedulable = unschedulable
	}
}

// Validation helpers

// ValidateNodeGroupConsistency validates that Node CRs have matching Kubernetes nodes
func ValidateNodeGroupConsistency(c client.Client) error {
	// This would be implemented to validate that:
	// 1. Every Node CR has a corresponding Kubernetes Node
	// 2. Group labels are consistent between Node CRs and Kubernetes Nodes
	// 3. Node names match between CRs and Kubernetes Nodes
	// For now, this is a placeholder for future implementation
	return nil
}

// Common test scenarios

// NewBasicTestScenario creates a mock client with a basic test scenario
func NewBasicTestScenario(scheme *runtime.Scheme) client.Client {
	return NewMockClientBuilder(scheme).
		WithGroup("test-group").
		WithNode("test-node-1", WithNodeGroup("test-group")).
		WithNode("test-node-2", WithNodeGroup("test-group"),
			WithNodePowerState(infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff)).
		WithKubernetesNode("test-node-1", WithKubernetesNodeGroupLabel("test-group")).
		WithKubernetesNode("test-node-2", WithKubernetesNodeGroupLabel("test-group")).
		Build()
}

// NewMultiGroupScenario creates a mock client with multiple groups
func NewMultiGroupScenario(scheme *runtime.Scheme) client.Client {
	return NewMockClientBuilder(scheme).
		WithGroup("group-1").
		WithGroup("group-2").
		WithNode("node-1-1", WithNodeGroup("group-1")).
		WithNode("node-1-2", WithNodeGroup("group-1")).
		WithNode("node-2-1", WithNodeGroup("group-2")).
		WithKubernetesNode("node-1-1", WithKubernetesNodeGroupLabel("group-1")).
		WithKubernetesNode("node-1-2", WithKubernetesNodeGroupLabel("group-1")).
		WithKubernetesNode("node-2-1", WithKubernetesNodeGroupLabel("group-2")).
		Build()
}

// NewScaleUpScenario creates a mock client with nodes ready for scale up
func NewScaleUpScenario(scheme *runtime.Scheme) client.Client {
	return NewMockClientBuilder(scheme).
		WithGroup("scale-group").
		WithNode("powered-on-node", WithNodeGroup("scale-group"),
			WithNodePowerState(infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn)).
		WithNode("powered-off-node", WithNodeGroup("scale-group"),
			WithNodePowerState(infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff),
			WithNodeProgress(infrav1alpha1.ProgressShutdown)).
		WithKubernetesNode("powered-on-node", WithKubernetesNodeGroupLabel("scale-group")).
		WithKubernetesNode("powered-off-node", WithKubernetesNodeGroupLabel("scale-group")).
		Build()
}

// NewScaleDownScenario creates a mock client with nodes ready for scale down
func NewScaleDownScenario(scheme *runtime.Scheme) client.Client {
	return NewMockClientBuilder(scheme).
		WithGroup("scale-group").
		WithNode("running-node-1", WithNodeGroup("scale-group"),
			WithNodePowerState(infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn),
			WithNodeProgress(infrav1alpha1.ProgressReady)).
		WithNode("running-node-2", WithNodeGroup("scale-group"),
			WithNodePowerState(infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn),
			WithNodeProgress(infrav1alpha1.ProgressReady)).
		WithNode("starting-node", WithNodeGroup("scale-group"),
			WithNodePowerState(infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff),
			WithNodeProgress(infrav1alpha1.ProgressStartingUp),
			WithNodeLastStartupTime(time.Now().Add(-5*time.Minute))).
		WithKubernetesNode("running-node-1", WithKubernetesNodeGroupLabel("scale-group")).
		WithKubernetesNode("running-node-2", WithKubernetesNodeGroupLabel("scale-group")).
		WithKubernetesNode("starting-node", WithKubernetesNodeGroupLabel("scale-group")).
		Build()
}
