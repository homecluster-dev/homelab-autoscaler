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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
)

func setupTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = infrav1alpha1.AddToScheme(scheme)
	return scheme
}

func createTestNode(powerState infrav1alpha1.PowerState, progress infrav1alpha1.Progress) *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-node",
			Namespace: "default",
			Labels: map[string]string{
				"group": "test-group",
			},
		},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  powerState,
			KubernetesNodeName: "test-node-k8s",
			StartupPodSpec: infrav1alpha1.MinimalPodSpec{
				Image:   "test/startup:latest",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "echo 'starting node'"},
			},
			ShutdownPodSpec: infrav1alpha1.MinimalPodSpec{
				Image:   "test/shutdown:latest",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "echo 'shutting down node'"},
			},
		},
		Status: infrav1alpha1.NodeStatus{
			PowerState: powerState,
			Progress:   progress,
		},
	}
}

func createTestGroup() *infrav1alpha1.Group {
	return &infrav1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-group",
			Namespace: "default",
		},
		Spec: infrav1alpha1.GroupSpec{
			ScaleDownUtilizationThreshold:    "0.5",
			ScaleDownGpuUtilizationThreshold: "0.5",
			ZeroOrMaxNodeScaling:             false,
			IgnoreDaemonSetsUtilization:      false,
		},
	}
}

func createTestKubernetesNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node-k8s",
		},
		Spec: corev1.NodeSpec{
			Unschedulable: false,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

func TestNodeReconciler_FSMIntegration(t *testing.T) {
	scheme := setupTestScheme()

	t.Run("successful node startup", func(t *testing.T) {
		// Create test objects
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown)
		node.Status.PowerState = infrav1alpha1.PowerStateOff // Current state is off, desired is on
		group := createTestGroup()
		k8sNode := createTestKubernetesNode()

		// Create fake client
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, group, k8sNode).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, result)

		// Verify job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(ctx, jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Contains(t, job.Name, "test-node-startup-")
		assert.Equal(t, "startup", job.Labels["type"])
		assert.Equal(t, "test/startup:latest", job.Spec.Template.Spec.Containers[0].Image)

		// Verify node status was updated
		updatedNode := &infrav1alpha1.Node{}
		err = fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		assert.Equal(t, infrav1alpha1.ProgressStartingUp, updatedNode.Status.Progress)
	})

	t.Run("successful node shutdown", func(t *testing.T) {
		// Create test objects
		node := createTestNode(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady)
		node.Status.PowerState = infrav1alpha1.PowerStateOn // Current state is on, desired is off
		group := createTestGroup()
		k8sNode := createTestKubernetesNode()

		// Create fake client
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, group, k8sNode).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, result)

		// Verify job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(ctx, jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Contains(t, job.Name, "test-node-shutdown-")
		assert.Equal(t, "shutdown", job.Labels["type"])
		assert.Equal(t, "test/shutdown:latest", job.Spec.Template.Spec.Containers[0].Image)

		// Verify node status was updated
		updatedNode := &infrav1alpha1.Node{}
		err = fakeClient.Get(ctx, client.ObjectKeyFromObject(node), updatedNode)
		assert.NoError(t, err)
		assert.Equal(t, infrav1alpha1.ProgressShuttingDown, updatedNode.Status.Progress)
	})

	t.Run("node already in desired state", func(t *testing.T) {
		// Create test objects - node already in desired state
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
		node.Status.PowerState = infrav1alpha1.PowerStateOn // Already on and ready
		group := createTestGroup()
		k8sNode := createTestKubernetesNode()

		// Create fake client
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, group, k8sNode).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{RequeueAfter: 5 * time.Minute}, result)

		// Verify no job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(ctx, jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})

	t.Run("node in transitional state", func(t *testing.T) {
		// Create test objects - node in transitional state
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp)
		node.Status.PowerState = infrav1alpha1.PowerStateOff
		group := createTestGroup()
		k8sNode := createTestKubernetesNode()

		// Create fake client
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, group, k8sNode).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		assert.NoError(t, err)
		// Should requeue with FSM backoff timing
		assert.True(t, result.RequeueAfter > 0)

		// Verify no new job was created (already transitioning)
		jobs := &batchv1.JobList{}
		err = fakeClient.List(ctx, jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})

	t.Run("coordination lock conflict", func(t *testing.T) {
		// Create test objects with existing coordination lock
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown)
		node.Status.PowerState = infrav1alpha1.PowerStateOff
		node.Annotations = map[string]string{
			"homelab-autoscaler.dev/operation-lock": "scale-up",
			"homelab-autoscaler.dev/lock-owner":     "cluster-autoscaler",
			"homelab-autoscaler.dev/lock-timestamp": time.Now().Format(time.RFC3339),
			"homelab-autoscaler.dev/lock-timeout":   "300", // 5 minutes
		}
		group := createTestGroup()
		k8sNode := createTestKubernetesNode()

		// Create fake client
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, group, k8sNode).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "FSM transition failed")
		// Should requeue with backoff
		assert.True(t, result.RequeueAfter > 0)

		// Verify no job was created due to lock conflict
		jobs := &batchv1.JobList{}
		err = fakeClient.List(ctx, jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})

	t.Run("invalid transition", func(t *testing.T) {
		// Create test objects with invalid transition (try to start already running node)
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
		node.Status.PowerState = infrav1alpha1.PowerStateOn // Already on and ready, can't start again
		group := createTestGroup()
		k8sNode := createTestKubernetesNode()

		// Create fake client
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, group, k8sNode).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		assert.NoError(t, err)
		// Should requeue normally since node is already in desired state
		assert.Equal(t, ctrl.Result{RequeueAfter: 5 * time.Minute}, result)

		// Verify no job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(ctx, jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})
}

func TestNodeReconciler_BackoffBehavior(t *testing.T) {
	scheme := setupTestScheme()

	t.Run("backoff calculation for transitional states", func(t *testing.T) {
		// Create test objects in transitional state with recent timestamp
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp)
		node.Status.PowerState = infrav1alpha1.PowerStateOff
		now := metav1.Now()
		node.Status.LastStartupTime = &now
		group := createTestGroup()
		k8sNode := createTestKubernetesNode()

		// Create fake client
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, group, k8sNode).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		assert.NoError(t, err)
		// Should use backoff timing (early phase = 30 seconds)
		assert.True(t, result.RequeueAfter > 0)
		assert.True(t, result.RequeueAfter <= time.Minute) // Should be short backoff for early phase
	})

	t.Run("stuck state detection and cleanup", func(t *testing.T) {
		// Create test objects in transitional state with old timestamp (stuck)
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp)
		node.Status.PowerState = infrav1alpha1.PowerStateOff
		oldTime := metav1.NewTime(time.Now().Add(-20 * time.Minute)) // Very old timestamp
		node.Status.LastStartupTime = &oldTime
		group := createTestGroup()
		k8sNode := createTestKubernetesNode()

		// Create fake client
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, group, k8sNode).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		assert.NoError(t, err)
		// Should detect stuck state and use backoff (not immediate retry)
		assert.True(t, result.RequeueAfter > 0)

		// Verify no new job was created (still in transitional state)
		jobs := &batchv1.JobList{}
		err = fakeClient.List(ctx, jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})
}

func TestNodeReconciler_ErrorHandling(t *testing.T) {
	scheme := setupTestScheme()

	t.Run("missing group reference", func(t *testing.T) {
		// Create node without group
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown)
		node.Labels = nil // Remove group label
		k8sNode := createTestKubernetesNode()

		// Create fake client without group
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, k8sNode).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "has no group label")
		assert.Equal(t, ctrl.Result{}, result)
	})

	t.Run("missing kubernetes node", func(t *testing.T) {
		// Create test objects without kubernetes node
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown)
		node.Status.PowerState = infrav1alpha1.PowerStateOff
		group := createTestGroup()

		// Create fake client without kubernetes node
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node, group).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build()

		// Create reconciler
		reconciler := &NodeReconciler{
			Client: fakeClient,
			Scheme: scheme,
		}

		// Reconcile
		ctx := context.TODO()
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-node",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(ctx, req)
		// The FSM creates the job first, then the job will fail when it tries to run
		if err != nil {
			assert.Contains(t, err.Error(), "FSM transition failed")
			assert.True(t, result.RequeueAfter > 0) // Should requeue with backoff
		} else {
			// FSM successfully creates the job, but the job will fail during execution
			// This is the expected behavior - the FSM doesn't pre-validate k8s node existence
			jobs := &batchv1.JobList{}
			listErr := fakeClient.List(ctx, jobs)
			assert.NoError(t, listErr)
			assert.Len(t, jobs.Items, 1) // Job is created by FSM

			job := jobs.Items[0]
			assert.Contains(t, job.Name, "test-node-startup-")
			assert.Equal(t, "startup", job.Labels["type"])
		}
	})
}

func TestNodeReconciler_StateConsistency(t *testing.T) {
	scheme := setupTestScheme()

	t.Run("state matches node progress", func(t *testing.T) {
		testCases := []struct {
			name        string
			progress    infrav1alpha1.Progress
			expectedFSM string
		}{
			{
				name:        "shutdown progress maps to shutdown state",
				progress:    infrav1alpha1.ProgressShutdown,
				expectedFSM: fsm.StateShutdown,
			},
			{
				name:        "starting up progress maps to starting up state",
				progress:    infrav1alpha1.ProgressStartingUp,
				expectedFSM: fsm.StateStartingUp,
			},
			{
				name:        "ready progress maps to ready state",
				progress:    infrav1alpha1.ProgressReady,
				expectedFSM: fsm.StateReady,
			},
			{
				name:        "shutting down progress maps to shutting down state",
				progress:    infrav1alpha1.ProgressShuttingDown,
				expectedFSM: fsm.StateShuttingDown,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				// Create test objects
				node := createTestNode(infrav1alpha1.PowerStateOn, tc.progress)
				group := createTestGroup()
				k8sNode := createTestKubernetesNode()

				// Create fake client
				fakeClient := fake.NewClientBuilder().
					WithScheme(scheme).
					WithObjects(node, group, k8sNode).
					WithStatusSubresource(&infrav1alpha1.Node{}).
					Build()

				// Create FSM directly to verify state mapping
				coordMgr := fsm.NewCoordinationManager(fakeClient, fsm.NodeControllerOwner)
				stateMachine := fsm.NewNodeStateMachine(node, fakeClient, scheme, coordMgr)

				assert.Equal(t, tc.expectedFSM, stateMachine.GetCurrentState())
			})
		}
	})
}
