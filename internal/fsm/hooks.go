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

package fsm

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/looplab/fsm"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// Before hooks - executed before state transitions for lock acquisition

func (nm *NodeStateMachine) beforeStartNode(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Acquiring coordination lock for node startup", "node", nm.node.Name)

	// Acquire coordination lock for scale-up operation
	if err := nm.coordinationManager.AcquireLock(ctx, nm.node, OperationScaleUp, DefaultLockTimeout); err != nil {
		logger.Error(err, "Failed to acquire coordination lock for startup", "node", nm.node.Name)
		if e != nil {
			e.Cancel(err)
		}
		return
	}

	logger.Info("Successfully acquired coordination lock for node startup", "node", nm.node.Name)
}

func (nm *NodeStateMachine) beforeShutdownNode(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Acquiring coordination lock for node shutdown", "node", nm.node.Name)

	// Acquire coordination lock for scale-down operation
	if err := nm.coordinationManager.AcquireLock(ctx, nm.node, OperationScaleDown, DefaultLockTimeout); err != nil {
		logger.Error(err, "Failed to acquire coordination lock for shutdown", "node", nm.node.Name)
		if e != nil {
			e.Cancel(err)
		}
		return
	}

	logger.Info("Successfully acquired coordination lock for node shutdown", "node", nm.node.Name)
}

// After hooks - executed after state transitions for lock release and status updates

func (nm *NodeStateMachine) afterJobCompleted(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Job completed, releasing coordination lock", "node", nm.node.Name, "state", e.Dst)

	// If transitioning to Ready state (startup completed), uncordon the Kubernetes node
	if e.Dst == StateReady {
		if err := nm.setNodeSchedulable(nm.node.Spec.KubernetesNodeName); err != nil {
			logger.Error(err, "Failed to uncordon Kubernetes node after startup completion", "kubernetesNode", nm.node.Spec.KubernetesNodeName)
			// Continue with the operation even if uncordoning fails
		}
	}

	// Release coordination lock after successful completion
	if err := nm.coordinationManager.ReleaseLock(ctx, nm.node); err != nil {
		logger.Error(err, "Failed to release coordination lock after job completion", "node", nm.node.Name)
	} else {
		logger.Info("Successfully released coordination lock after job completion", "node", nm.node.Name)
	}

	// Update node status to reflect new state
	nm.updateNodeProgress(infrav1alpha1.Progress(e.Dst))
}

func (nm *NodeStateMachine) afterJobFailed(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Job failed, releasing coordination lock", "node", nm.node.Name, "state", e.Dst)

	// Release coordination lock after failure
	if err := nm.coordinationManager.ReleaseLock(ctx, nm.node); err != nil {
		logger.Error(err, "Failed to release coordination lock after job failure", "node", nm.node.Name)
	} else {
		logger.Info("Successfully released coordination lock after job failure", "node", nm.node.Name)
	}

	// Update node status to reflect failure and new state
	nm.updateNodeProgress(infrav1alpha1.Progress(e.Dst))

	// Add failure condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "JobFailed",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "AsyncJobFailed",
		Message:            fmt.Sprintf("Job failed, transitioned from %s to %s", e.Src, e.Dst),
	})
}

func (nm *NodeStateMachine) afterForceCleanup(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Force cleanup executed, releasing coordination lock", "node", nm.node.Name)

	// Force release coordination lock
	if err := nm.coordinationManager.ForceReleaseLock(ctx, nm.node); err != nil {
		logger.Error(err, "Failed to force release coordination lock", "node", nm.node.Name)
	} else {
		logger.Info("Successfully force released coordination lock", "node", nm.node.Name)
	}

	// Update node status to reflect cleanup
	nm.updateNodeProgress(infrav1alpha1.Progress(e.Dst))

	// Add cleanup condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "ForceCleanup",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "StuckTransitionCleanup",
		Message:            fmt.Sprintf("Force cleanup executed, transitioned from %s to %s", e.Src, e.Dst),
	})
}

// State entry hooks - executed when entering specific states

func (nm *NodeStateMachine) enterStartingUp(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Entering StartingUp state", "node", nm.node.Name)

	// Reset backoff strategy for new transition
	nm.backoff = &BackoffStrategy{
		transitionStart: time.Now(),
		retryCount:      0,
		maxRetries:      MaxRetries,
	}

	// Update node status
	nm.updateNodeProgress(infrav1alpha1.ProgressStartingUp)

	// Add progressing condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "StartupProgressing",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeStarting",
		Message:            "Node startup operation initiated",
	})
}

func (nm *NodeStateMachine) enterShuttingDown(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Entering ShuttingDown state", "node", nm.node.Name)

	// Reset backoff strategy for new transition
	nm.backoff = &BackoffStrategy{
		transitionStart: time.Now(),
		retryCount:      0,
		maxRetries:      MaxRetries,
	}

	// Update node status
	nm.updateNodeProgress(infrav1alpha1.ProgressShuttingDown)

	// Add progressing condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "ShutdownProgressing",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeShuttingDown",
		Message:            "Node shutdown operation initiated",
	})
}

func (nm *NodeStateMachine) enterReady(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Entering Ready state", "node", nm.node.Name)

	// Clear backoff strategy
	nm.backoff = nil

	// Update node status
	nm.updateNodeProgress(infrav1alpha1.ProgressReady)

	// Add ready condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeReady",
		Message:            "Node is ready and operational",
	})
}

func (nm *NodeStateMachine) enterShutdown(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Entering Shutdown state", "node", nm.node.Name)

	// Clear backoff strategy
	nm.backoff = nil

	// Update node status
	nm.updateNodeProgress(infrav1alpha1.ProgressShutdown)

	// Add shutdown condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "Shutdown",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeShutdown",
		Message:            "Node is shutdown and powered off",
	})
}

// Helper methods for status updates are implemented in node_state_machine.go
