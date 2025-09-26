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

package grpcserver

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeGroups_SimpleOperations(t *testing.T) {
	// Create a GroupStore and add some test data
	groupStore := groupstore.NewGroupStore()

	// Add a healthy group
	healthyGroup := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "healthy-group",
		},
		Spec: v1alpha1.GroupSpec{
			Name:    "healthy-group",
			MaxSize: 5,
		},
	}
	err := groupStore.AddOrUpdate(healthyGroup)
	if err != nil {
		t.Fatalf("Failed to add healthy group: %v", err)
	}
	groupStore.SetHealthcheckStatus("healthy-group", "Healthy")

	// Add an unhealthy group
	unhealthyGroup := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unhealthy-group",
		},
		Spec: v1alpha1.GroupSpec{
			Name:    "unhealthy-group",
			MaxSize: 3,
		},
	}
	err = groupStore.AddOrUpdate(unhealthyGroup)
	if err != nil {
		t.Fatalf("Failed to add unhealthy group: %v", err)
	}
	groupStore.SetHealthcheckStatus("unhealthy-group", "Unhealthy")

	// Create the gRPC server
	server := NewMockCloudProviderServer(groupStore)

	// Test 1: Normal context - should return all groups regardless of health status
	t.Run("NormalContext", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, err := server.NodeGroups(ctx, &pb.NodeGroupsRequest{})
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		if resp == nil {
			t.Fatal("Expected non-nil response")
		}
		if len(resp.NodeGroups) != 2 {
			t.Fatalf("Expected 2 groups (both healthy and unhealthy), got %d", len(resp.NodeGroups))
		}

		// Verify both groups are returned
		groupIDs := make(map[string]bool)
		for _, ng := range resp.NodeGroups {
			groupIDs[ng.Id] = true
		}
		if !groupIDs["healthy-group"] {
			t.Fatal("Expected 'healthy-group' to be included in response")
		}
		if !groupIDs["unhealthy-group"] {
			t.Fatal("Expected 'unhealthy-group' to be included in response")
		}

		// Verify healthy group details
		var healthyGroup *pb.NodeGroup
		var unhealthyGroup *pb.NodeGroup
		for _, ng := range resp.NodeGroups {
			switch ng.Id {
			case "healthy-group":
				healthyGroup = ng
			case "unhealthy-group":
				unhealthyGroup = ng
			}
		}

		if healthyGroup == nil || healthyGroup.MaxSize != 5 {
			t.Fatalf("Expected healthy group with MaxSize 5, got healthyGroup=%v", healthyGroup)
		}
		if unhealthyGroup == nil || unhealthyGroup.MaxSize != 3 {
			t.Fatalf("Expected unhealthy group with MaxSize 3, got unhealthyGroup=%v", unhealthyGroup)
		}
	})

	// Test 2: Already cancelled context - should return deadline exceeded
	t.Run("CancelledContext", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		resp, err := server.NodeGroups(ctx, &pb.NodeGroupsRequest{})
		if err == nil {
			t.Fatal("Expected error for cancelled context, got nil")
		}
		if resp != nil {
			t.Fatal("Expected nil response for cancelled context")
		}
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("Expected 'context deadline exceeded' error, got: %v", err)
		}
	})

	// Test 3: Context cancelled during processing - should handle gracefully
	t.Run("ContextCancelledDuringProcessing", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		defer cancel()

		// Give it a tiny bit of time to start processing
		time.Sleep(2 * time.Millisecond)

		resp, err := server.NodeGroups(ctx, &pb.NodeGroupsRequest{})
		if err == nil {
			t.Fatal("Expected error for cancelled context, got nil")
		}
		if resp != nil {
			t.Fatal("Expected nil response for cancelled context")
		}
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("Expected 'context deadline exceeded' error, got: %v", err)
		}
	})
}

func TestNodeGroups_EmptyStore(t *testing.T) {
	// Test with empty store
	groupStore := groupstore.NewGroupStore()
	server := NewMockCloudProviderServer(groupStore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := server.NodeGroups(ctx, &pb.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("Expected no error for empty store, got: %v", err)
	}
	if resp == nil {
		t.Fatal("Expected non-nil response for empty store")
	}
	if len(resp.NodeGroups) != 0 {
		t.Fatalf("Expected 0 groups for empty store, got %d", len(resp.NodeGroups))
	}
}

func TestNodeGroups_GroupWithoutHealthStatus(t *testing.T) {
	// Test with group that has no health status - should now be included
	groupStore := groupstore.NewGroupStore()

	group := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "no-health-status",
		},
		Spec: v1alpha1.GroupSpec{
			Name:    "no-health-status",
			MaxSize: 2,
		},
	}
	err := groupStore.AddOrUpdate(group)
	if err != nil {
		t.Fatalf("Failed to add group: %v", err)
	}
	// Don't set health status - should now be included regardless

	server := NewMockCloudProviderServer(groupStore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := server.NodeGroups(ctx, &pb.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if resp == nil {
		t.Fatal("Expected non-nil response")
	}
	if len(resp.NodeGroups) != 1 {
		t.Fatalf("Expected 1 group (no health status should now be included), got %d", len(resp.NodeGroups))
	}
	if resp.NodeGroups[0].Id != "no-health-status" {
		t.Fatalf("Expected group ID 'no-health-status', got '%s'", resp.NodeGroups[0].Id)
	}
	if resp.NodeGroups[0].MaxSize != 2 {
		t.Fatalf("Expected MaxSize 2, got %d", resp.NodeGroups[0].MaxSize)
	}
}

func TestNodeGroups_UnhealthyGroupsIncluded(t *testing.T) {
	// Test that unhealthy groups are now included in the response
	groupStore := groupstore.NewGroupStore()

	// Add multiple groups with different health statuses
	groups := []*v1alpha1.Group{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "healthy-1"},
			Spec:       v1alpha1.GroupSpec{Name: "healthy-1", MaxSize: 5},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "unhealthy-1"},
			Spec:       v1alpha1.GroupSpec{Name: "unhealthy-1", MaxSize: 3},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "healthy-2"},
			Spec:       v1alpha1.GroupSpec{Name: "healthy-2", MaxSize: 7},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "unhealthy-2"},
			Spec:       v1alpha1.GroupSpec{Name: "unhealthy-2", MaxSize: 4},
		},
	}

	// Add all groups to the store
	for _, group := range groups {
		err := groupStore.AddOrUpdate(group)
		if err != nil {
			t.Fatalf("Failed to add group %s: %v", group.Name, err)
		}
	}

	// Set health statuses
	groupStore.SetHealthcheckStatus("healthy-1", "Healthy")
	groupStore.SetHealthcheckStatus("unhealthy-1", "Unhealthy")
	groupStore.SetHealthcheckStatus("healthy-2", "Healthy")
	groupStore.SetHealthcheckStatus("unhealthy-2", "Unhealthy")

	server := NewMockCloudProviderServer(groupStore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := server.NodeGroups(ctx, &pb.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if resp == nil {
		t.Fatal("Expected non-nil response")
	}
	if len(resp.NodeGroups) != 4 {
		t.Fatalf("Expected 4 groups (all groups regardless of health), got %d", len(resp.NodeGroups))
	}

	// Verify all groups are present
	expectedGroups := map[string]int32{
		"healthy-1":   5,
		"unhealthy-1": 3,
		"healthy-2":   7,
		"unhealthy-2": 4,
	}

	actualGroups := make(map[string]int32)
	for _, ng := range resp.NodeGroups {
		actualGroups[ng.Id] = ng.MaxSize
	}

	for expectedID, expectedMaxSize := range expectedGroups {
		actualMaxSize, exists := actualGroups[expectedID]
		if !exists {
			t.Fatalf("Expected group %s to be included in response", expectedID)
		}
		if actualMaxSize != expectedMaxSize {
			t.Fatalf("Expected group %s to have MaxSize %d, got %d", expectedID, expectedMaxSize, actualMaxSize)
		}
	}
}
