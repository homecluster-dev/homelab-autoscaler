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

package testdata

import (
    "embed"
    "fmt"
    "strings"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/util/yaml"
    "sigs.k8s.io/controller-runtime/pkg/client"

    infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
    "github.com/homecluster-dev/homelab-autoscaler/internal/config"
    "github.com/homecluster-dev/homelab-autoscaler/test/integration/mocks"
)

//go:embed *.yaml
var testDataFiles embed.FS

// TestDataLoader provides functionality to load test data from YAML files
type TestDataLoader struct {
	scheme *runtime.Scheme
}

// NewTestDataLoader creates a new test data loader with the provided scheme
func NewTestDataLoader(scheme *runtime.Scheme) *TestDataLoader {
	return &TestDataLoader{
		scheme: scheme,
	}
}

// LoadGroups loads all Group CRs from groups.yaml
func (l *TestDataLoader) LoadGroups() ([]*infrav1alpha1.Group, error) {
    data, err := testDataFiles.ReadFile("groups.yaml")
    if err != nil {
        return nil, fmt.Errorf("failed to read groups.yaml: %w", err)
    }
    // Replace template placeholder with actual namespace used by the server.
    ns := config.NewNamespaceConfig().Get()
    data = []byte(strings.ReplaceAll(string(data), "{{ .Namespace }}", ns))

	var groups []*infrav1alpha1.Group
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)

	for {
		var group infrav1alpha1.Group
		if err := decoder.Decode(&group); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("failed to decode group: %w", err)
		}

		// Skip empty objects (from document separators)
		if group.Name == "" {
			continue
		}

		groups = append(groups, &group)
	}

	return groups, nil
}

// LoadNodes loads all Node CRs from nodes.yaml
func (l *TestDataLoader) LoadNodes() ([]*infrav1alpha1.Node, error) {
    data, err := testDataFiles.ReadFile("nodes.yaml")
    if err != nil {
        return nil, fmt.Errorf("failed to read nodes.yaml: %w", err)
    }
    ns := config.NewNamespaceConfig().Get()
    data = []byte(strings.ReplaceAll(string(data), "{{ .Namespace }}", ns))

	var nodes []*infrav1alpha1.Node
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)

	for {
		var node infrav1alpha1.Node
		if err := decoder.Decode(&node); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("failed to decode node: %w", err)
		}

		// Skip empty objects (from document separators)
		if node.Name == "" {
			continue
		}

		nodes = append(nodes, &node)
	}

	return nodes, nil
}

// LoadKubernetesNodes loads all Kubernetes Node objects from kubernetes_nodes.yaml
func (l *TestDataLoader) LoadKubernetesNodes() ([]*corev1.Node, error) {
    data, err := testDataFiles.ReadFile("kubernetes_nodes.yaml")
    if err != nil {
        return nil, fmt.Errorf("failed to read kubernetes_nodes.yaml: %w", err)
    }
    ns := config.NewNamespaceConfig().Get()
    data = []byte(strings.ReplaceAll(string(data), "{{ .Namespace }}", ns))

	var nodes []*corev1.Node
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)

	for {
		var node corev1.Node
		if err := decoder.Decode(&node); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("failed to decode kubernetes node: %w", err)
		}

		// Skip empty objects (from document separators)
		if node.Name == "" {
			continue
		}

		nodes = append(nodes, &node)
	}

	return nodes, nil
}

// LoadAllTestData loads all test data (Groups, Nodes, and Kubernetes Nodes)
func (l *TestDataLoader) LoadAllTestData() ([]*infrav1alpha1.Group, []*infrav1alpha1.Node, []*corev1.Node, error) {
	groups, err := l.LoadGroups()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load groups: %w", err)
	}

	nodes, err := l.LoadNodes()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load nodes: %w", err)
	}

	kubernetesNodes, err := l.LoadKubernetesNodes()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load kubernetes nodes: %w", err)
	}

	return groups, nodes, kubernetesNodes, nil
}

// CreateMockClientWithAllData creates a mock client pre-populated with all test data
func (l *TestDataLoader) CreateMockClientWithAllData() (client.Client, error) {
	groups, nodes, kubernetesNodes, err := l.LoadAllTestData()
	if err != nil {
		return nil, fmt.Errorf("failed to load test data: %w", err)
	}

	mockClient := mocks.NewMockClient(l.scheme)

	// Add all groups
	for _, group := range groups {
		if err := mockClient.AddObject(group); err != nil {
			return nil, fmt.Errorf("failed to add group %s: %w", group.Name, err)
		}
	}

	// Add all nodes
	for _, node := range nodes {
		if err := mockClient.AddObject(node); err != nil {
			return nil, fmt.Errorf("failed to add node %s: %w", node.Name, err)
		}
	}

	// Add all kubernetes nodes
	for _, node := range kubernetesNodes {
		if err := mockClient.AddObject(node); err != nil {
			return nil, fmt.Errorf("failed to add kubernetes node %s: %w", node.Name, err)
		}
	}

	return mockClient, nil
}

// CreateMockClientWithScenario creates a mock client with data for a specific test scenario
func (l *TestDataLoader) CreateMockClientWithScenario(scenario string) (client.Client, error) {
	groups, nodes, kubernetesNodes, err := l.LoadAllTestData()
	if err != nil {
		return nil, fmt.Errorf("failed to load test data: %w", err)
	}

	mockClient := mocks.NewMockClient(l.scheme)

	// Filter and add groups for the scenario
	for _, group := range groups {
		if matchesScenario(group.Labels, scenario) {
			if err := mockClient.AddObject(group); err != nil {
				return nil, fmt.Errorf("failed to add group %s: %w", group.Name, err)
			}
		}
	}

	// Filter and add nodes for the scenario
	for _, node := range nodes {
		if matchesScenario(node.Labels, scenario) {
			if err := mockClient.AddObject(node); err != nil {
				return nil, fmt.Errorf("failed to add node %s: %w", node.Name, err)
			}
		}
	}

	// Filter and add kubernetes nodes for the scenario
	for _, node := range kubernetesNodes {
		if matchesScenario(node.Labels, scenario) {
			if err := mockClient.AddObject(node); err != nil {
				return nil, fmt.Errorf("failed to add kubernetes node %s: %w", node.Name, err)
			}
		}
	}

	return mockClient, nil
}

// CreateMockClientWithGroup creates a mock client with data for a specific group
func (l *TestDataLoader) CreateMockClientWithGroup(groupName string) (client.Client, error) {
	groups, nodes, kubernetesNodes, err := l.LoadAllTestData()
	if err != nil {
		return nil, fmt.Errorf("failed to load test data: %w", err)
	}

	mockClient := mocks.NewMockClient(l.scheme)

	// Add the specific group
	for _, group := range groups {
		if group.Name == groupName {
			if err := mockClient.AddObject(group); err != nil {
				return nil, fmt.Errorf("failed to add group %s: %w", group.Name, err)
			}
			break
		}
	}

	// Add nodes belonging to the group
	for _, node := range nodes {
		if groupLabel, exists := node.Labels["group"]; exists && groupLabel == groupName {
			if err := mockClient.AddObject(node); err != nil {
				return nil, fmt.Errorf("failed to add node %s: %w", node.Name, err)
			}
		}
	}

	// Add kubernetes nodes belonging to the group
	for _, node := range kubernetesNodes {
		if groupLabel, exists := node.Labels["infra.homecluster.dev/group"]; exists && groupLabel == groupName {
			if err := mockClient.AddObject(node); err != nil {
				return nil, fmt.Errorf("failed to add kubernetes node %s: %w", node.Name, err)
			}
		}
	}

	return mockClient, nil
}

// ValidateDataConsistency validates that Node CRs and Kubernetes nodes have matching names and consistent group labels
func (l *TestDataLoader) ValidateDataConsistency() error {
	groups, nodes, kubernetesNodes, err := l.LoadAllTestData()
	if err != nil {
		return fmt.Errorf("failed to load test data: %w", err)
	}

	// Create maps for efficient lookup
	groupMap := make(map[string]*infrav1alpha1.Group)
	for _, group := range groups {
		groupMap[group.Name] = group
	}

	nodeMap := make(map[string]*infrav1alpha1.Node)
	for _, node := range nodes {
		nodeMap[node.Spec.KubernetesNodeName] = node
	}

	kubernetesNodeMap := make(map[string]*corev1.Node)
	for _, node := range kubernetesNodes {
		kubernetesNodeMap[node.Name] = node
	}

	// Validate that every Node CR has a corresponding Kubernetes Node
	for _, node := range nodes {
		kubernetesNodeName := node.Spec.KubernetesNodeName
		kubernetesNode, exists := kubernetesNodeMap[kubernetesNodeName]
		if !exists {
			return fmt.Errorf("Node CR %s references Kubernetes node %s which does not exist", node.Name, kubernetesNodeName)
		}

		// Validate group label consistency
		nodeGroupLabel := node.Labels["group"]
		kubernetesGroupLabel := kubernetesNode.Labels["infra.homecluster.dev/group"]

		// Both should have the same group label (or both should be empty for orphaned nodes)
		if nodeGroupLabel != kubernetesGroupLabel {
			return fmt.Errorf("Group label mismatch for node %s: Node CR has '%s', Kubernetes Node has '%s'",
				node.Name, nodeGroupLabel, kubernetesGroupLabel)
		}

		// If they have a group label, validate that the group exists
		if nodeGroupLabel != "" {
			if _, exists := groupMap[nodeGroupLabel]; !exists {
				return fmt.Errorf("Node %s references group %s which does not exist", node.Name, nodeGroupLabel)
			}
		}
	}

	// Validate that every Kubernetes Node with a group label has a corresponding Node CR
	for _, kubernetesNode := range kubernetesNodes {
		if groupLabel, hasGroupLabel := kubernetesNode.Labels["infra.homecluster.dev/group"]; hasGroupLabel {
			if _, exists := nodeMap[kubernetesNode.Name]; !exists {
				return fmt.Errorf("Kubernetes node %s has group label %s but no corresponding Node CR exists",
					kubernetesNode.Name, groupLabel)
			}
		}
	}

	return nil
}

// GetTestScenarios returns a list of available test scenarios
func (l *TestDataLoader) GetTestScenarios() []string {
	return []string{
		"basic",
		"gpu-workload",
		"cpu-workload",
		"offline-nodes",
		"transitioning-nodes",
		"scale-from-zero",
		"fast-scaling",
		"development",
		"shutting-down",
		"starting-up",
		"orphaned-node",
	}
}

// GetTestGroups returns a list of available test groups
func (l *TestDataLoader) GetTestGroups() []string {
	return []string{
		"test-group",
		"gpu-group",
		"cpu-group",
		"offline-group",
		"unknown-group",
		"empty-group",
		"fast-scale-group",
		"dev-group",
	}
}

// matchesScenario checks if the given labels match a test scenario
func matchesScenario(labels map[string]string, scenario string) bool {
	if labels == nil {
		return false
	}

	testScenario, exists := labels["test-scenario"]
	return exists && testScenario == scenario
}

// Convenience functions for common test setups

// CreateBasicTestClient creates a mock client with basic test data (test-group with 2 nodes)
func CreateBasicTestClient(scheme *runtime.Scheme) (client.Client, error) {
	loader := NewTestDataLoader(scheme)
	return loader.CreateMockClientWithGroup("test-group")
}

// CreateGPUTestClient creates a mock client with GPU test data
func CreateGPUTestClient(scheme *runtime.Scheme) (client.Client, error) {
	loader := NewTestDataLoader(scheme)
	return loader.CreateMockClientWithGroup("gpu-group")
}

// CreateCPUTestClient creates a mock client with CPU test data
func CreateCPUTestClient(scheme *runtime.Scheme) (client.Client, error) {
	loader := NewTestDataLoader(scheme)
	return loader.CreateMockClientWithGroup("cpu-group")
}

// CreateScaleUpTestClient creates a mock client with data suitable for scale-up testing
func CreateScaleUpTestClient(scheme *runtime.Scheme) (client.Client, error) {
	loader := NewTestDataLoader(scheme)
	return loader.CreateMockClientWithScenario("scale-up-candidate")
}

// CreateScaleDownTestClient creates a mock client with data suitable for scale-down testing
func CreateScaleDownTestClient(scheme *runtime.Scheme) (client.Client, error) {
	loader := NewTestDataLoader(scheme)
	return loader.CreateMockClientWithScenario("shutting-down")
}

// CreateOfflineTestClient creates a mock client with offline nodes
func CreateOfflineTestClient(scheme *runtime.Scheme) (client.Client, error) {
	loader := NewTestDataLoader(scheme)
	return loader.CreateMockClientWithGroup("offline-group")
}

// CreateEmptyGroupTestClient creates a mock client with an empty group (for scale-from-zero testing)
func CreateEmptyGroupTestClient(scheme *runtime.Scheme) (client.Client, error) {
	loader := NewTestDataLoader(scheme)
	return loader.CreateMockClientWithGroup("empty-group")
}

// CreateFullTestClient creates a mock client with all test data loaded
func CreateFullTestClient(scheme *runtime.Scheme) (client.Client, error) {
	loader := NewTestDataLoader(scheme)
	return loader.CreateMockClientWithAllData()
}

// ValidateTestDataConsistency validates the consistency of all test data
func ValidateTestDataConsistency(scheme *runtime.Scheme) error {
	loader := NewTestDataLoader(scheme)
	return loader.ValidateDataConsistency()
}
