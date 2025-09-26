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

package groupstore

import (
	"testing"

	v1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Constants for health status values used in tests
const (
	testStatusHealthy = "healthy"
	testStatusOffline = "offline"
	testStatusUnknown = "unknown"
)

func TestUpdateGroupHealth(t *testing.T) {
	store := NewGroupStore()

	// Create a test group
	group := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-group",
		},
		Spec: v1alpha1.GroupSpec{
			Name: "test-group",
		},
		Status: v1alpha1.GroupStatus{
			Health: "unknown",
		},
	}

	// Add the group to the store
	err := store.AddOrUpdate(group)
	if err != nil {
		t.Fatalf("Failed to add group to store: %v", err)
	}

	// Test updating health status
	err = store.UpdateGroupHealth("test-group", "healthy")
	if err != nil {
		t.Fatalf("Failed to update group health: %v", err)
	}

	// Verify the health status was updated
	updatedGroup, err := store.Get("test-group")
	if err != nil {
		t.Fatalf("Failed to get group from store: %v", err)
	}

	if updatedGroup.Status.Health != testStatusHealthy {
		t.Errorf("Expected health status 'healthy', got '%s'", updatedGroup.Status.Health)
	}

	// Test updating to a different health status
	err = store.UpdateGroupHealth("test-group", "offline")
	if err != nil {
		t.Fatalf("Failed to update group health: %v", err)
	}

	// Verify the health status was updated again
	updatedGroup, err = store.Get("test-group")
	if err != nil {
		t.Fatalf("Failed to get group from store: %v", err)
	}

	if updatedGroup.Status.Health != testStatusOffline {
		t.Errorf("Expected health status 'offline', got '%s'", updatedGroup.Status.Health)
	}

	// Test updating non-existent group
	err = store.UpdateGroupHealth("non-existent", "healthy")
	if err == nil {
		t.Error("Expected error when updating non-existent group, got nil")
	}
}

func TestSetHealthcheckStatus(t *testing.T) {
	store := NewGroupStore()

	// Test setting healthcheck status
	store.SetHealthcheckStatus("test-group", "Healthy")

	// Verify the healthcheck status was set
	status, found := store.GetHealthcheckStatus("test-group")
	if !found {
		t.Error("Expected to find healthcheck status, but it was not found")
	}

	if status != "Healthy" {
		t.Errorf("Expected healthcheck status 'Healthy', got '%s'", status)
	}

	// Test updating healthcheck status
	store.SetHealthcheckStatus("test-group", "Failed")

	// Verify the healthcheck status was updated
	status, found = store.GetHealthcheckStatus("test-group")
	if !found {
		t.Error("Expected to find healthcheck status, but it was not found")
	}

	if status != "Failed" {
		t.Errorf("Expected healthcheck status 'Failed', got '%s'", status)
	}

	// Test backward compatibility - verify that setting group health also works with node-level tracking
	// Create a group first
	group := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-group-compat",
		},
		Spec: v1alpha1.GroupSpec{
			Name: "test-group-compat",
		},
		Status: v1alpha1.GroupStatus{
			Health: "unknown",
		},
	}
	err := store.AddOrUpdate(group)
	if err != nil {
		t.Fatalf("Failed to add group to store: %v", err)
	}

	// Set healthcheck status and verify it works with new structure
	store.SetHealthcheckStatus("test-group-compat", "healthy")
	status, found = store.GetHealthcheckStatus("test-group-compat")
	if !found {
		t.Error("Expected to find healthcheck status for backward compatibility test")
	}
	if status != testStatusHealthy {
		t.Errorf("Expected healthcheck status 'healthy' for backward compatibility, got '%s'", status)
	}
}

func TestSetNodeHealthcheckStatus(t *testing.T) {
	store := NewGroupStore()

	// Test setting node healthcheck status
	store.SetNodeHealthcheckStatus("test-group", "node1", "healthy")
	store.SetNodeHealthcheckStatus("test-group", "node2", "healthy")
	store.SetNodeHealthcheckStatus("test-group", "node3", "offline")

	// Verify individual node statuses
	status, found := store.GetNodeHealthcheckStatus("test-group", "node1")
	if !found {
		t.Error("Expected to find healthcheck status for node1")
	}
	if status != testStatusHealthy {
		t.Errorf("Expected healthcheck status 'healthy' for node1, got '%s'", status)
	}

	status, found = store.GetNodeHealthcheckStatus("test-group", "node2")
	if !found {
		t.Error("Expected to find healthcheck status for node2")
	}
	if status != testStatusHealthy {
		t.Errorf("Expected healthcheck status 'healthy' for node2, got '%s'", status)
	}

	status, found = store.GetNodeHealthcheckStatus("test-group", "node3")
	if !found {
		t.Error("Expected to find healthcheck status for node3")
	}
	if status != testStatusOffline {
		t.Errorf("Expected healthcheck status 'offline' for node3, got '%s'", status)
	}

	// Test getting status for non-existent node
	_, found = store.GetNodeHealthcheckStatus("test-group", "non-existent-node")
	if found {
		t.Error("Expected not to find healthcheck status for non-existent node")
	}

	// Test with empty parameters
	store.SetNodeHealthcheckStatus("", "node1", "healthy")
	store.SetNodeHealthcheckStatus("test-group", "", "healthy")
	// These should not crash and should be handled gracefully
}

func TestGetNodeHealthcheckStatus(t *testing.T) {
	store := NewGroupStore()

	// Set up some node health statuses
	store.SetNodeHealthcheckStatus("test-group", "node1", "healthy")
	store.SetNodeHealthcheckStatus("test-group", "node2", "offline")

	// Test retrieving individual node statuses
	testCases := []struct {
		groupName string
		nodeName  string
		expected  string
		found     bool
	}{
		{"test-group", "node1", "healthy", true},
		{"test-group", "node2", "offline", true},
		{"test-group", "non-existent", "", false},
		{"non-existent-group", "node1", "", false},
	}

	for _, tc := range testCases {
		status, found := store.GetNodeHealthcheckStatus(tc.groupName, tc.nodeName)
		if found != tc.found {
			t.Errorf("Expected found=%v for group %s node %s, got %v", tc.found, tc.groupName, tc.nodeName, found)
		}
		if found && status != tc.expected {
			t.Errorf("Expected status '%s' for group %s node %s, got '%s'", tc.expected, tc.groupName, tc.nodeName, status)
		}
	}

	// Test with empty parameters
	_, found := store.GetNodeHealthcheckStatus("", "node1")
	if found {
		t.Error("Expected not to find status with empty group name")
	}

	_, found = store.GetNodeHealthcheckStatus("test-group", "")
	if found {
		t.Error("Expected not to find status with empty node name")
	}
}

func TestNodeLevelHealthCalculation(t *testing.T) {
	store := NewGroupStore()

	// Test case 1: All nodes healthy -> group should be healthy
	store.SetNodeHealthcheckStatus("group1", "node1", "healthy")
	store.SetNodeHealthcheckStatus("group1", "node2", "healthy")
	store.SetNodeHealthcheckStatus("group1", "node3", "healthy")

	status, found := store.GetHealthcheckStatus("group1")
	if !found {
		t.Error("Expected to find healthcheck status for group1")
	}
	if status != testStatusHealthy {
		t.Errorf("Expected group health 'healthy' when all nodes are healthy, got '%s'", status)
	}

	// Test case 2: One node offline -> group should be offline
	store.SetNodeHealthcheckStatus("group2", "node1", "healthy")
	store.SetNodeHealthcheckStatus("group2", "node2", "offline")
	store.SetNodeHealthcheckStatus("group2", "node3", "healthy")

	status, found = store.GetHealthcheckStatus("group2")
	if !found {
		t.Error("Expected to find healthcheck status for group2")
	}
	if status != testStatusOffline {
		t.Errorf("Expected group health 'offline' when one node is offline, got '%s'", status)
	}

	// Test case 3: Mixed statuses (no offline nodes) -> group should be unknown
	store.SetNodeHealthcheckStatus("group3", "node1", "healthy")
	store.SetNodeHealthcheckStatus("group3", "node2", "unknown")
	store.SetNodeHealthcheckStatus("group3", "node3", "healthy")

	status, found = store.GetHealthcheckStatus("group3")
	if !found {
		t.Error("Expected to find healthcheck status for group3")
	}
	if status != testStatusUnknown {
		t.Errorf("Expected group health 'unknown' when mixed statuses without offline nodes, got '%s'", status)
	}

	// Test case 4: Empty group -> should be unknown
	store.SetNodeHealthcheckStatus("group4", "node1", "unknown")
	store.SetNodeHealthcheckStatus("group4", "node1", "") // Remove node1 by setting empty status

	status, found = store.GetHealthcheckStatus("group4")
	if found && status != testStatusUnknown {
		t.Errorf("Expected group health 'unknown' for empty group, got '%s'", status)
	}

	// Test case 5: Backward compatibility - using old SetHealthcheckStatus method
	store.SetHealthcheckStatus("group5", "healthy")
	status, found = store.GetHealthcheckStatus("group5")
	if !found {
		t.Error("Expected to find healthcheck status for group5 (backward compatibility)")
	}
	if status != "healthy" {
		t.Errorf("Expected group health 'healthy' for backward compatibility test, got '%s'", status)
	}

	// Test case 6: Mixed old and new methods
	store.SetHealthcheckStatus("group6", "healthy")              // Old method
	store.SetNodeHealthcheckStatus("group6", "node1", "offline") // New method should override

	status, found = store.GetHealthcheckStatus("group6")
	if !found {
		t.Error("Expected to find healthcheck status for group6")
	}
	if status != testStatusOffline {
		t.Errorf("Expected group health 'offline' when node method overrides group method, got '%s'", status)
	}
}

func TestRemoveGroupWithNodeHealth(t *testing.T) {
	store := NewGroupStore()

	// Create a group and set some node health statuses
	group := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-group-remove",
		},
		Spec: v1alpha1.GroupSpec{
			Name: "test-group-remove",
		},
		Status: v1alpha1.GroupStatus{
			Health: "unknown",
		},
	}

	err := store.AddOrUpdate(group)
	if err != nil {
		t.Fatalf("Failed to add group to store: %v", err)
	}

	store.SetNodeHealthcheckStatus("test-group-remove", "node1", "healthy")
	store.SetNodeHealthcheckStatus("test-group-remove", "node2", "offline")

	// Verify the group and health data exist
	_, err = store.Get("test-group-remove")
	if err != nil {
		t.Fatalf("Group should exist before removal: %v", err)
	}

	_, found := store.GetHealthcheckStatus("test-group-remove")
	if !found {
		t.Error("Healthcheck status should exist before removal")
	}

	// Remove the group
	err = store.Remove("test-group-remove")
	if err != nil {
		t.Fatalf("Failed to remove group: %v", err)
	}

	// Verify the group is removed
	_, err = store.Get("test-group-remove")
	if err == nil {
		t.Error("Expected group to be removed, but it still exists")
	}

	// Verify health data is also removed
	_, found = store.GetHealthcheckStatus("test-group-remove")
	if found {
		t.Error("Healthcheck status should be removed along with the group")
	}

	_, found = store.GetNodeHealthcheckStatus("test-group-remove", "node1")
	if found {
		t.Error("Node healthcheck status should be removed along with the group")
	}
}
