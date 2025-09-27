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
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	server := NewMockCloudProviderServer(groupStore, nil, nil)

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
	server := NewMockCloudProviderServer(groupStore, nil, nil)

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

	server := NewMockCloudProviderServer(groupStore, nil, nil)

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

	server := NewMockCloudProviderServer(groupStore, nil, nil)

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

func TestNodeGroupIncreaseSize(t *testing.T) {
	// Create a GroupStore and add test data
	groupStore := groupstore.NewGroupStore()

	// Create a test group
	group := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-group",
		},
		Spec: v1alpha1.GroupSpec{
			Name:    "test-group",
			MaxSize: 5,
		},
	}
	err := groupStore.AddOrUpdate(group)
	if err != nil {
		t.Fatalf("Failed to add group to store: %v", err)
	}

	// Create test nodes with different health statuses
	healthyNode := &v1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "healthy-node",
			Namespace: "default",
		},
		Spec: v1alpha1.NodeSpec{
			KubernetesNodeName: "healthy-k8s-node",
		},
	}
	unhealthyNode := &v1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unhealthy-node",
			Namespace: "default",
		},
		Spec: v1alpha1.NodeSpec{
			KubernetesNodeName: "unhealthy-k8s-node",
		},
	}

	err = groupStore.AddOrUpdateNode(healthyNode)
	if err != nil {
		t.Fatalf("Failed to add healthy node: %v", err)
	}
	err = groupStore.AddOrUpdateNode(unhealthyNode)
	if err != nil {
		t.Fatalf("Failed to add unhealthy node: %v", err)
	}

	// Set node health statuses
	groupStore.SetNodeHealthcheckStatus("test-group", "healthy-k8s-node", "healthy")
	groupStore.SetNodeHealthcheckStatus("test-group", "unhealthy-k8s-node", "offline")

	// Create a mock Kubernetes client
	mockClient := &mockK8sClient{}

	// Create a scheme for the test
	scheme := runtime.NewScheme()
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	// Create the gRPC server
	server := NewMockCloudProviderServer(groupStore, mockClient, scheme)

	// Test 1: Successful increase size with unhealthy node
	t.Run("SuccessfulIncreaseSize", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := &pb.NodeGroupIncreaseSizeRequest{
			Id:    "test-group",
			Delta: 1,
		}

		resp, err := server.NodeGroupIncreaseSize(ctx, req)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		if resp == nil {
			t.Fatal("Expected non-nil response")
		}

		// Verify that a Job was created
		if len(mockClient.createdJobs) != 1 {
			t.Fatalf("Expected 1 job to be created, got %d", len(mockClient.createdJobs))
		}

		job := mockClient.createdJobs[0]
		if job.Name == "" {
			t.Error("Expected job to have a name")
		}
		if job.Namespace != "default" {
			t.Errorf("Expected job namespace 'default', got '%s'", job.Namespace)
		}
		if job.Labels["type"] != "startup" {
			t.Errorf("Expected job label 'type=startup', got '%s'", job.Labels["type"])
		}
		if job.Labels["group"] != "test-group" {
			t.Errorf("Expected job label 'group=test-group', got '%s'", job.Labels["group"])
		}
		if job.Labels["node"] != "unhealthy-k8s-node" {
			t.Errorf("Expected job label 'node=unhealthy-k8s-node', got '%s'", job.Labels["node"])
		}

		// Verify the job spec
		if len(job.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("Expected 1 container in job, got %d", len(job.Spec.Template.Spec.Containers))
		}
		if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
			t.Error("Expected RestartPolicyOnFailure")
		}
	})

	// Test 2: No unhealthy nodes found
	t.Run("NoUnhealthyNodes", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Reset mock client
		mockClient.createdJobs = nil

		// Set all nodes to healthy
		groupStore.SetNodeHealthcheckStatus("test-group", "healthy-k8s-node", "healthy")
		groupStore.SetNodeHealthcheckStatus("test-group", "unhealthy-k8s-node", "healthy")

		req := &pb.NodeGroupIncreaseSizeRequest{
			Id:    "test-group",
			Delta: 1,
		}

		_, err := server.NodeGroupIncreaseSize(ctx, req)
		if err == nil {
			t.Fatal("Expected error when no unhealthy nodes found, got nil")
		}
		if !strings.Contains(err.Error(), "no unhealthy nodes found") {
			t.Fatalf("Expected 'no unhealthy nodes found' error, got: %v", err)
		}

		// Verify no job was created
		if len(mockClient.createdJobs) != 0 {
			t.Fatalf("Expected 0 jobs to be created, got %d", len(mockClient.createdJobs))
		}
	})

	// Test 3: Non-existent group
	t.Run("NonExistentGroup", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := &pb.NodeGroupIncreaseSizeRequest{
			Id:    "non-existent-group",
			Delta: 1,
		}

		_, err := server.NodeGroupIncreaseSize(ctx, req)
		if err == nil {
			t.Fatal("Expected error for non-existent group, got nil")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Fatalf("Expected 'not found' error, got: %v", err)
		}
	})

	// Test 4: Invalid delta (zero or negative)
	t.Run("InvalidDelta", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Test delta = 0
		req := &pb.NodeGroupIncreaseSizeRequest{
			Id:    "test-group",
			Delta: 0,
		}

		_, err := server.NodeGroupIncreaseSize(ctx, req)
		if err == nil {
			t.Fatal("Expected error for delta=0, got nil")
		}
		if !strings.Contains(err.Error(), "delta must be positive") {
			t.Fatalf("Expected 'delta must be positive' error, got: %v", err)
		}

		// Test delta < 0
		req.Delta = -1
		_, err = server.NodeGroupIncreaseSize(ctx, req)
		if err == nil {
			t.Fatal("Expected error for delta=-1, got nil")
		}
		if !strings.Contains(err.Error(), "delta must be positive") {
			t.Fatalf("Expected 'delta must be positive' error, got: %v", err)
		}
	})

	// Test 5: Context cancellation
	t.Run("ContextCancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		req := &pb.NodeGroupIncreaseSizeRequest{
			Id:    "test-group",
			Delta: 1,
		}

		_, err := server.NodeGroupIncreaseSize(ctx, req)
		if err == nil {
			t.Fatal("Expected error for cancelled context, got nil")
		}
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("Expected 'context deadline exceeded' error, got: %v", err)
		}
	})

	// Test 6: No nodes in group
	t.Run("NoNodesInGroup", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Create a new group with no nodes
		emptyGroup := &v1alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{
				Name: "empty-group",
			},
			Spec: v1alpha1.GroupSpec{
				Name:    "empty-group",
				MaxSize: 5,
			},
		}
		err := groupStore.AddOrUpdate(emptyGroup)
		if err != nil {
			t.Fatalf("Failed to add empty group: %v", err)
		}

		req := &pb.NodeGroupIncreaseSizeRequest{
			Id:    "empty-group",
			Delta: 1,
		}

		_, err = server.NodeGroupIncreaseSize(ctx, req)
		if err == nil {
			t.Fatal("Expected error when no nodes in group, got nil")
		}
		if !strings.Contains(err.Error(), "no nodes found for group") {
			t.Fatalf("Expected 'no nodes found for group' error, got: %v", err)
		}
	})
}

// Mock Kubernetes client for testing
type mockK8sClient struct {
	createdJobs []batchv1.Job
}

func (m *mockK8sClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if job, ok := obj.(*batchv1.Job); ok {
		m.createdJobs = append(m.createdJobs, *job)
		return nil
	}
	return nil
}

func (m *mockK8sClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return nil
}

func (m *mockK8sClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return nil
}

func (m *mockK8sClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return nil
}

func (m *mockK8sClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return nil
}

func (m *mockK8sClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return nil
}

func (m *mockK8sClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return nil
}

func (m *mockK8sClient) Status() client.StatusWriter {
	return &mockStatusWriter{client: m}
}

func (m *mockK8sClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	return nil
}

func (m *mockK8sClient) Scheme() *runtime.Scheme {
	return runtime.NewScheme()
}

func (m *mockK8sClient) RESTMapper() meta.RESTMapper {
	return meta.NewDefaultRESTMapper([]schema.GroupVersion{})
}

func (m *mockK8sClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, nil
}

func (m *mockK8sClient) IsObjectNamespaced(obj runtime.Object) (bool, error) {
	return true, nil
}

func (m *mockK8sClient) SubResource(subResource string) client.SubResourceClient {
	return &mockSubResourceClient{client: m}
}

type mockSubResourceClient struct {
	client *mockK8sClient
}

func (s *mockSubResourceClient) Get(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceGetOption) error {
	return nil
}

func (s *mockSubResourceClient) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return nil
}

func (s *mockSubResourceClient) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return nil
}

func (s *mockSubResourceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return nil
}

type mockStatusWriter struct {
	client *mockK8sClient
}

func (s *mockStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return nil
}

func (s *mockStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return nil
}

func (s *mockStatusWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return nil
}

func TestNodeGroupDeleteNodes_Success(t *testing.T) {
	// Create a GroupStore and add test data
	groupStore := groupstore.NewGroupStore()

	// Create a mock Kubernetes client
	scheme := runtime.NewScheme()
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	mockClient := &mockK8sClient{
		createdJobs: []batchv1.Job{},
	}

	// Add a test group
	testGroup := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-group",
			Namespace: "homelab-autoscaler-system",
		},
		Spec: v1alpha1.GroupSpec{
			Name:    "test-group",
			MaxSize: 3,
		},
	}
	err := groupStore.AddOrUpdate(testGroup)
	if err != nil {
		t.Fatalf("Failed to add test group: %v", err)
	}

	// Add a test node with shutdown pod spec
	testNode := &v1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-node",
			Namespace: "homelab-autoscaler-system",
		},
		Spec: v1alpha1.NodeSpec{
			KubernetesNodeName: "test-k8s-node",
			ShutdownPodSpec: v1alpha1.MinimalPodSpec{
				Image:   "busybox:latest",
				Command: []string{"sh", "-c", "echo 'shutting down'"},
				Args:    []string{},
			},
		},
	}
	err = groupStore.AddOrUpdateNode(testNode)
	if err != nil {
		t.Fatalf("Failed to add test node: %v", err)
	}

	// Associate the node with the test group
	groupStore.SetNodeHealthcheckStatus("test-group", "test-k8s-node", "healthy")

	// Create the gRPC server
	server := NewMockCloudProviderServer(groupStore, mockClient, scheme)

	// Test successful deletion
	t.Run("SuccessfulDeletion", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := &pb.NodeGroupDeleteNodesRequest{
			Id: "test-group",
			Nodes: []*pb.ExternalGrpcNode{
				{Name: "test-node"},
			},
		}

		resp, err := server.NodeGroupDeleteNodes(ctx, req)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		if resp == nil {
			t.Fatal("Expected non-nil response")
		}

		// Verify that a shutdown job was created
		if len(mockClient.createdJobs) != 1 {
			t.Fatalf("Expected 1 job to be created, got %d", len(mockClient.createdJobs))
		}

		job := mockClient.createdJobs[0]
		if job.Name == "" {
			t.Fatal("Expected job to have a name")
		}
		if job.Namespace != "homelab-autoscaler-system" {
			t.Fatalf("Expected job namespace to be 'homelab-autoscaler-system', got '%s'", job.Namespace)
		}

		// Verify job labels
		expectedLabels := map[string]string{
			"app":   "homelab-autoscaler",
			"group": "test-group",
			"node":  "test-k8s-node",
			"type":  "shutdown",
		}
		for key, expectedValue := range expectedLabels {
			if job.Labels[key] != expectedValue {
				t.Fatalf("Expected job label '%s' to be '%s', got '%s'", key, expectedValue, job.Labels[key])
			}
		}

		// Verify job spec
		if len(job.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("Expected 1 container in job, got %d", len(job.Spec.Template.Spec.Containers))
		}

		container := job.Spec.Template.Spec.Containers[0]
		if container.Name != "shutdown" {
			t.Fatalf("Expected container name to be 'shutdown', got '%s'", container.Name)
		}
		if container.Image != "busybox:latest" {
			t.Fatalf("Expected container image to be 'busybox:latest', got '%s'", container.Image)
		}
	})
}

func TestNodeGroupDeleteNodes_GroupNotFound(t *testing.T) {
	// Create a GroupStore with no groups
	groupStore := groupstore.NewGroupStore()

	// Create the gRPC server
	server := NewMockCloudProviderServer(groupStore, nil, nil)

	t.Run("GroupNotFound", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := &pb.NodeGroupDeleteNodesRequest{
			Id: "non-existent-group",
			Nodes: []*pb.ExternalGrpcNode{
				{Name: "test-node"},
			},
		}

		resp, err := server.NodeGroupDeleteNodes(ctx, req)
		if err == nil {
			t.Fatal("Expected error for non-existent group, got nil")
		}
		if resp != nil {
			t.Fatal("Expected nil response for non-existent group")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Fatalf("Expected 'not found' error, got: %v", err)
		}
	})
}

func TestNodeGroupDeleteNodes_NodeNotFound(t *testing.T) {
	// Create a GroupStore and add test data
	groupStore := groupstore.NewGroupStore()

	// Add a test group
	testGroup := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-group",
		},
		Spec: v1alpha1.GroupSpec{
			Name:    "test-group",
			MaxSize: 3,
		},
	}
	err := groupStore.AddOrUpdate(testGroup)
	if err != nil {
		t.Fatalf("Failed to add test group: %v", err)
	}

	// Add one test node
	testNode := &v1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: v1alpha1.NodeSpec{
			KubernetesNodeName: "test-k8s-node",
			ShutdownPodSpec: v1alpha1.MinimalPodSpec{
				Image: "busybox:latest",
			},
		},
	}
	err = groupStore.AddOrUpdateNode(testNode)
	if err != nil {
		t.Fatalf("Failed to add test node: %v", err)
	}

	// Associate the node with the test group
	groupStore.SetNodeHealthcheckStatus("test-group", "test-k8s-node", "healthy")

	// Create the gRPC server
	server := NewMockCloudProviderServer(groupStore, nil, nil)

	t.Run("NodeNotFound", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := &pb.NodeGroupDeleteNodesRequest{
			Id: "test-group",
			Nodes: []*pb.ExternalGrpcNode{
				{Name: "non-existent-node"},
			},
		}

		resp, err := server.NodeGroupDeleteNodes(ctx, req)
		if err == nil {
			t.Fatal("Expected error for non-existent node, got nil")
		}
		if resp != nil {
			t.Fatal("Expected nil response for non-existent node")
		}
		if !strings.Contains(err.Error(), "failed to get node") {
			t.Fatalf("Expected 'failed to get node' error, got: %v", err)
		}
	})
}

func TestNodeGroupDeleteNodes_TooManyNodes(t *testing.T) {
	// Create a GroupStore and add test data
	groupStore := groupstore.NewGroupStore()

	// Add a test group
	testGroup := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-group",
		},
		Spec: v1alpha1.GroupSpec{
			Name:    "test-group",
			MaxSize: 3,
		},
	}
	err := groupStore.AddOrUpdate(testGroup)
	if err != nil {
		t.Fatalf("Failed to add test group: %v", err)
	}

	// Add one test node
	testNode := &v1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: v1alpha1.NodeSpec{
			KubernetesNodeName: "test-k8s-node",
			ShutdownPodSpec: v1alpha1.MinimalPodSpec{
				Image: "busybox:latest",
			},
		},
	}
	err = groupStore.AddOrUpdateNode(testNode)
	if err != nil {
		t.Fatalf("Failed to add test node: %v", err)
	}

	// Associate the node with the test group
	groupStore.SetNodeHealthcheckStatus("test-group", "test-k8s-node", "healthy")

	// Create the gRPC server
	server := NewMockCloudProviderServer(groupStore, nil, nil)

	t.Run("TooManyNodes", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := &pb.NodeGroupDeleteNodesRequest{
			Id: "test-group",
			Nodes: []*pb.ExternalGrpcNode{
				{Name: "test-node"},
				{Name: "test-node-2"}, // This node doesn't exist
			},
		}

		resp, err := server.NodeGroupDeleteNodes(ctx, req)
		if err == nil {
			t.Fatal("Expected error for too many nodes, got nil")
		}
		if resp != nil {
			t.Fatal("Expected nil response for too many nodes")
		}
		if !strings.Contains(err.Error(), "cannot delete") {
			t.Fatalf("Expected 'cannot delete' error, got: %v", err)
		}
	})
}

func TestNodeGroupDeleteNodes_ContextCancelled(t *testing.T) {
	// Create a GroupStore and add test data
	groupStore := groupstore.NewGroupStore()

	// Add a test group
	testGroup := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-group",
		},
		Spec: v1alpha1.GroupSpec{
			Name:    "test-group",
			MaxSize: 3,
		},
	}
	err := groupStore.AddOrUpdate(testGroup)
	if err != nil {
		t.Fatalf("Failed to add test group: %v", err)
	}

	// Add a test node
	testNode := &v1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: v1alpha1.NodeSpec{
			KubernetesNodeName: "test-k8s-node",
			ShutdownPodSpec: v1alpha1.MinimalPodSpec{
				Image: "busybox:latest",
			},
		},
	}
	err = groupStore.AddOrUpdateNode(testNode)
	if err != nil {
		t.Fatalf("Failed to add test node: %v", err)
	}

	// Associate the node with the test group
	groupStore.SetNodeHealthcheckStatus("test-group", "test-k8s-node", "healthy")

	// Create the gRPC server
	server := NewMockCloudProviderServer(groupStore, nil, nil)

	t.Run("ContextCancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		req := &pb.NodeGroupDeleteNodesRequest{
			Id: "test-group",
			Nodes: []*pb.ExternalGrpcNode{
				{Name: "test-node"},
			},
		}

		resp, err := server.NodeGroupDeleteNodes(ctx, req)
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

func TestNodeGroupDeleteNodes_MultipleNodes(t *testing.T) {
	// Create a GroupStore and add test data
	groupStore := groupstore.NewGroupStore()

	// Create a mock Kubernetes client
	scheme := runtime.NewScheme()
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	mockClient := &mockK8sClient{
		createdJobs: []batchv1.Job{},
	}

	// Add a test group
	testGroup := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-group",
			Namespace: "homelab-autoscaler-system",
		},
		Spec: v1alpha1.GroupSpec{
			Name:    "test-group",
			MaxSize: 5,
		},
	}
	err := groupStore.AddOrUpdate(testGroup)
	if err != nil {
		t.Fatalf("Failed to add test group: %v", err)
	}

	// Add multiple test nodes
	nodes := []string{"node1", "node2", "node3"}
	for _, nodeName := range nodes {
		k8sNodeName := fmt.Sprintf("test-k8s-%s", nodeName)
		testNode := &v1alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nodeName,
				Namespace: "homelab-autoscaler-system",
			},
			Spec: v1alpha1.NodeSpec{
				KubernetesNodeName: k8sNodeName,
				ShutdownPodSpec: v1alpha1.MinimalPodSpec{
					Image:   "busybox:latest",
					Command: []string{"sh", "-c", fmt.Sprintf("echo 'shutting down %s'", nodeName)},
					Args:    []string{},
				},
			},
		}
		err = groupStore.AddOrUpdateNode(testNode)
		if err != nil {
			t.Fatalf("Failed to add test node %s: %v", nodeName, err)
		}
		// Associate the node with the test group
		groupStore.SetNodeHealthcheckStatus("test-group", k8sNodeName, "healthy")
	}

	// Create the gRPC server
	server := NewMockCloudProviderServer(groupStore, mockClient, scheme)

	t.Run("MultipleNodes", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := &pb.NodeGroupDeleteNodesRequest{
			Id: "test-group",
			Nodes: []*pb.ExternalGrpcNode{
				{Name: "node1"},
				{Name: "node2"},
				{Name: "node3"},
			},
		}

		resp, err := server.NodeGroupDeleteNodes(ctx, req)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		if resp == nil {
			t.Fatal("Expected non-nil response")
		}

		// Verify that 3 shutdown jobs were created
		if len(mockClient.createdJobs) != 3 {
			t.Fatalf("Expected 3 jobs to be created, got %d", len(mockClient.createdJobs))
		}

		// Verify each job has correct properties
		for i, job := range mockClient.createdJobs {
			if job.Name == "" {
				t.Fatalf("Expected job %d to have a name", i)
			}
			if job.Namespace != "homelab-autoscaler-system" {
				t.Fatalf("Expected job %d namespace to be 'homelab-autoscaler-system', got '%s'", i, job.Namespace)
			}

			// Verify job labels
			expectedLabels := map[string]string{
				"app":   "homelab-autoscaler",
				"group": "test-group",
				"type":  "shutdown",
			}
			for key, expectedValue := range expectedLabels {
				if job.Labels[key] != expectedValue {
					t.Fatalf("Expected job %d label '%s' to be '%s', got '%s'", i, key, expectedValue, job.Labels[key])
				}
			}
		}
	})
}
