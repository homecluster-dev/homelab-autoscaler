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
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

func TestDataConsistency(t *testing.T) {
	// Create a scheme with our custom resources
	testScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(testScheme); err != nil {
		t.Fatalf("Failed to add core types to scheme: %v", err)
	}
	if err := infrav1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("Failed to add infra types to scheme: %v", err)
	}

	// Validate data consistency
	if err := ValidateTestDataConsistency(testScheme); err != nil {
		t.Errorf("Test data consistency validation failed: %v", err)
	}
}

func TestLoadAllTestData(t *testing.T) {
	// Create a scheme with our custom resources
	testScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(testScheme); err != nil {
		t.Fatalf("Failed to add core types to scheme: %v", err)
	}
	if err := infrav1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("Failed to add infra types to scheme: %v", err)
	}

	loader := NewTestDataLoader(testScheme)

	// Test loading groups
	groups, err := loader.LoadGroups()
	if err != nil {
		t.Errorf("Failed to load groups: %v", err)
	}
	if len(groups) == 0 {
		t.Error("No groups loaded")
	}
	t.Logf("Loaded %d groups", len(groups))

	// Test loading nodes
	nodes, err := loader.LoadNodes()
	if err != nil {
		t.Errorf("Failed to load nodes: %v", err)
	}
	if len(nodes) == 0 {
		t.Error("No nodes loaded")
	}
	t.Logf("Loaded %d nodes", len(nodes))

	// Test loading kubernetes nodes
	kubernetesNodes, err := loader.LoadKubernetesNodes()
	if err != nil {
		t.Errorf("Failed to load kubernetes nodes: %v", err)
	}
	if len(kubernetesNodes) == 0 {
		t.Error("No kubernetes nodes loaded")
	}
	t.Logf("Loaded %d kubernetes nodes", len(kubernetesNodes))

	// Verify that we have matching counts for nodes with groups
	nodesByGroup := make(map[string]int)
	kubernetesNodesByGroup := make(map[string]int)

	for _, node := range nodes {
		if groupLabel, exists := node.Labels["infra.homecluster.dev/group"]; exists {
			nodesByGroup[groupLabel]++
		}
	}

	for _, node := range kubernetesNodes {
		if groupLabel, exists := node.Labels["infra.homecluster.dev/group"]; exists {
			kubernetesNodesByGroup[groupLabel]++
		}
	}

	// Check that each group has matching node counts
	for group, nodeCount := range nodesByGroup {
		kubernetesNodeCount, exists := kubernetesNodesByGroup[group]
		if !exists {
			t.Errorf("Group %s has %d Node CRs but no Kubernetes nodes", group, nodeCount)
		} else if nodeCount != kubernetesNodeCount {
			t.Errorf("Group %s has %d Node CRs but %d Kubernetes nodes", group, nodeCount, kubernetesNodeCount)
		}
	}
}

func TestCreateMockClients(t *testing.T) {
	// Create a scheme with our custom resources
	testScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(testScheme); err != nil {
		t.Fatalf("Failed to add core types to scheme: %v", err)
	}
	if err := infrav1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("Failed to add infra types to scheme: %v", err)
	}

	// Test creating various mock clients
	testCases := []struct {
		name        string
		createFunc  func(*runtime.Scheme) (interface{}, error)
		expectError bool
	}{
		{
			name: "BasicTestClient",
			createFunc: func(s *runtime.Scheme) (interface{}, error) {
				return CreateBasicTestClient(s)
			},
		},
		{
			name: "GPUTestClient",
			createFunc: func(s *runtime.Scheme) (interface{}, error) {
				return CreateGPUTestClient(s)
			},
		},
		{
			name: "CPUTestClient",
			createFunc: func(s *runtime.Scheme) (interface{}, error) {
				return CreateCPUTestClient(s)
			},
		},
		{
			name: "FullTestClient",
			createFunc: func(s *runtime.Scheme) (interface{}, error) {
				return CreateFullTestClient(s)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := tc.createFunc(testScheme)
			if tc.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if client == nil {
					t.Error("Client is nil")
				}
			}
		})
	}
}

func TestGetTestScenarios(t *testing.T) {
	testScheme := runtime.NewScheme()
	loader := NewTestDataLoader(testScheme)

	scenarios := loader.GetTestScenarios()
	if len(scenarios) == 0 {
		t.Error("No test scenarios returned")
	}

	expectedScenarios := []string{
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

	scenarioMap := make(map[string]bool)
	for _, scenario := range scenarios {
		scenarioMap[scenario] = true
	}

	for _, expected := range expectedScenarios {
		if !scenarioMap[expected] {
			t.Errorf("Expected scenario %s not found in returned scenarios", expected)
		}
	}

	t.Logf("Available test scenarios: %v", scenarios)
}

func TestGetTestGroups(t *testing.T) {
	testScheme := runtime.NewScheme()
	loader := NewTestDataLoader(testScheme)

	groups := loader.GetTestGroups()
	if len(groups) == 0 {
		t.Error("No test groups returned")
	}

	expectedGroups := []string{
		"test-group",
		"gpu-group",
		"cpu-group",
		"offline-group",
		"unknown-group",
		"empty-group",
		"fast-scale-group",
		"dev-group",
	}

	groupMap := make(map[string]bool)
	for _, group := range groups {
		groupMap[group] = true
	}

	for _, expected := range expectedGroups {
		if !groupMap[expected] {
			t.Errorf("Expected group %s not found in returned groups", expected)
		}
	}

	t.Logf("Available test groups: %v", groups)
}
