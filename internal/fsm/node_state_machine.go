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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/looplab/fsm"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// NewNodeStateMachine creates a new FSM instance for node state management
func NewNodeStateMachine(
	node *infrav1alpha1.Node,
	k8sClient client.Client,
	scheme *runtime.Scheme,
	coordMgr CoordinationManager,
) *NodeStateMachine {
	nm := &NodeStateMachine{
		node:                node,
		client:              k8sClient,
		scheme:              scheme,
		coordinationManager: coordMgr,
		jobTimeout:          DefaultJobTimeout,
	}

	// Initialize FSM with current state from node progress
	initialState := nm.getCurrentState()

	nm.fsm = fsm.NewFSM(
		initialState,
		GetFSMTransitions(),
		GetFSMCallbacks(nm),
	)

	return nm
}

// GetCurrentState returns the current FSM state based on node progress
func (nm *NodeStateMachine) getCurrentState() string {
	switch nm.node.Status.Progress {
	case infrav1alpha1.ProgressShutdown:
		return StateShutdown
	case infrav1alpha1.ProgressStartingUp:
		return StateStartingUp
	case infrav1alpha1.ProgressReady:
		return StateReady
	case infrav1alpha1.ProgressShuttingDown:
		return StateShuttingDown
	default:
		// Default to shutdown if progress is empty or unknown
		return StateShutdown
	}
}

// GetCurrentState returns the current FSM state
func (nm *NodeStateMachine) GetCurrentState() string {
	return nm.fsm.Current()
}

// CanTransition checks if an event can be triggered from the current state
func (nm *NodeStateMachine) CanTransition(event string) bool {
	return nm.fsm.Can(event)
}

// StartNode triggers the StartNode event to begin node startup
func (nm *NodeStateMachine) StartNode() error {
	logger := log.Log.WithName("fsm")
	ctx := context.TODO()

	logger.Info("Triggering StartNode event", "node", nm.node.Name, "currentState", nm.fsm.Current())

	// Clean up any existing pending jobs before starting new operation
	if err := nm.cleanupPendingJobs(nm.node.Spec.KubernetesNodeName); err != nil {
		logger.Error(err, "Failed to cleanup pending jobs, continuing with startup", "node", nm.node.Name)
		// Continue with startup even if cleanup fails
	}

	// Remove cluster autoscaler taint if present
	if err := nm.removeClusterAutoscalerTaint(nm.node.Spec.KubernetesNodeName); err != nil {
		logger.Error(err, "Failed to remove cluster autoscaler taint, continuing with startup", "node", nm.node.Name)
		// Continue with startup even if taint removal fails
	}

	// Trigger FSM event
	if err := nm.fsm.Event(ctx, EventStartNode); err != nil {
		return fmt.Errorf("FSM transition failed: %w", err)
	}

	// Create and start Kubernetes Job
	job, err := nm.createStartupJob()
	if err != nil {
		// Trigger failure event to rollback
		if err := nm.fsm.Event(ctx, EventJobFailed); err != nil {
			logger.Error(err, "Failed to trigger JobFailed event", "node", nm.node.Name)
		}
		return fmt.Errorf("failed to create startup job: %w", err)
	}

	nm.currentJob = job.Name
	nm.jobStartTime = time.Now()

	// Start async job monitoring
	go nm.monitorJobCompletion(job.Name)

	logger.Info("Node startup initiated", "node", nm.node.Name, "job", job.Name)
	return nil
}

// ShutdownNode triggers the ShutdownNode event to begin node shutdown
func (nm *NodeStateMachine) ShutdownNode() error {
	logger := log.Log.WithName("fsm")
	ctx := context.TODO()

	logger.Info("Triggering ShutdownNode event", "node", nm.node.Name, "currentState", nm.fsm.Current())

	// Clean up any existing pending jobs before starting new operation
	if err := nm.cleanupPendingJobs(nm.node.Spec.KubernetesNodeName); err != nil {
		logger.Error(err, "Failed to cleanup pending jobs, continuing with shutdown", "node", nm.node.Name)
		// Continue with shutdown even if cleanup fails
	}

	// Cordon the Kubernetes node before shutdown
	if err := nm.setNodeUnschedulable(nm.node.Spec.KubernetesNodeName); err != nil {
		logger.Error(err, "Failed to cordon Kubernetes node, continuing with shutdown", "kubernetesNode", nm.node.Spec.KubernetesNodeName)
		// Continue with shutdown even if cordoning fails
	}

	// Trigger FSM event
	if err := nm.fsm.Event(ctx, EventShutdownNode); err != nil {
		return fmt.Errorf("FSM transition failed: %w", err)
	}

	// Create and start Kubernetes Job
	job, err := nm.createShutdownJob()
	if err != nil {
		// Trigger failure event to rollback
		if err := nm.fsm.Event(ctx, EventJobFailed); err != nil {
			logger.Error(err, "Failed to trigger JobFailed event", "node", nm.node.Name)
		}
		return fmt.Errorf("failed to create shutdown job: %w", err)
	}

	nm.currentJob = job.Name
	nm.jobStartTime = time.Now()

	// Start async job monitoring
	go nm.monitorJobCompletion(job.Name)

	logger.Info("Node shutdown initiated", "node", nm.node.Name, "job", job.Name)
	return nil
}

// JobCompleted triggers the JobCompleted event
func (nm *NodeStateMachine) JobCompleted(job *batchv1.Job) error {
	ctx := context.TODO()
	return nm.fsm.Event(ctx, EventJobCompleted, job)
}

// JobFailed triggers the JobFailed event
func (nm *NodeStateMachine) JobFailed() error {
	ctx := context.TODO()
	return nm.fsm.Event(ctx, EventJobFailed)
}

// JobTimeout triggers the JobTimeout event
func (nm *NodeStateMachine) JobTimeout() error {
	ctx := context.TODO()
	return nm.fsm.Event(ctx, EventJobTimeout)
}

// ForceCleanup triggers the ForceCleanup event
func (nm *NodeStateMachine) ForceCleanup() error {
	ctx := context.TODO()
	return nm.fsm.Event(ctx, EventForceCleanup)
}

// CalculateBackoff implements smart backoff strategy based on transition duration
func (nm *NodeStateMachine) CalculateBackoff() time.Duration {
	if nm.backoff == nil {
		return time.Minute // Default backoff
	}

	elapsed := time.Since(nm.backoff.transitionStart)

	switch {
	case elapsed <= 2*time.Minute:
		return 30 * time.Second // Early phase
	case elapsed <= 10*time.Minute:
		return 2 * time.Minute // Normal phase
	case elapsed <= 15*time.Minute:
		return 5 * time.Minute // Late phase
	default:
		// Force cleanup after 15 minutes
		logger := log.Log.WithName("fsm")
		if err := nm.ForceCleanup(); err != nil {
			logger.Error(err, "Failed to force cleanup", "node", nm.node.Name)
		}
		return 0
	}
}

// IsTransitionStuck checks if the current transition has been stuck for too long
func (nm *NodeStateMachine) IsTransitionStuck() bool {
	if nm.backoff == nil {
		return false
	}

	elapsed := time.Since(nm.backoff.transitionStart)
	return elapsed > 15*time.Minute
}

// CheckStuckState checks for stuck transitions and forces cleanup if needed
func (nm *NodeStateMachine) CheckStuckState() {
	if nm.IsTransitionStuck() {
		logger := log.Log.WithName("fsm")
		logger.Info("Detected stuck state transition, forcing cleanup",
			"node", nm.node.Name,
			"state", nm.fsm.Current(),
			"duration", time.Since(nm.backoff.transitionStart))

		if err := nm.ForceCleanup(); err != nil {
			logger.Error(err, "Failed to force cleanup", "node", nm.node.Name)
		}
	}
}

// MonitorJobCompletion monitors a Kubernetes job and triggers appropriate FSM events
func (nm *NodeStateMachine) MonitorJobCompletion(jobName string) {
	nm.monitorJobCompletion(jobName)
}

// monitorJobCompletion is the internal implementation of job monitoring
func (nm *NodeStateMachine) monitorJobCompletion(jobName string) {
	logger := log.Log.WithName("fsm")
	ctx, cancel := context.WithTimeout(context.Background(), nm.jobTimeout)
	defer cancel()

	// Poll job status every 1 second for tests, 30 seconds for production
	pollInterval := 30 * time.Second
	if nm.jobTimeout < 5*time.Second {
		pollInterval = 100 * time.Millisecond // Fast polling for tests
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	logger.Info("Starting job monitoring", "node", nm.node.Name, "job", jobName)

	// Check job status immediately first
	job := &batchv1.Job{}
	err := nm.client.Get(ctx, client.ObjectKey{Name: jobName, Namespace: nm.node.Namespace}, job)
	if err == nil {
		if job.Status.Succeeded > 0 {
			logger.Info("Job completed successfully", "node", nm.node.Name, "job", jobName)
			if err := nm.JobCompleted(job); err != nil {
				logger.Error(err, "Failed to trigger JobCompleted event", "node", nm.node.Name)
			}
			return
		}

		if job.Status.Failed > 0 {
			logger.Info("Job failed", "node", nm.node.Name, "job", jobName)
			if err := nm.JobFailed(); err != nil {
				logger.Error(err, "Failed to trigger JobFailed event", "node", nm.node.Name)
			}
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("Job monitoring timeout", "node", nm.node.Name, "job", jobName)
			if err := nm.JobTimeout(); err != nil {
				logger.Error(err, "Failed to trigger JobTimeout event", "node", nm.node.Name)
			}
			return

		case <-ticker.C:
			job := &batchv1.Job{}
			err := nm.client.Get(ctx, client.ObjectKey{Name: jobName, Namespace: nm.node.Namespace}, job)
			if err != nil {
				logger.Error(err, "Failed to get job status", "job", jobName)
				continue
			}

			if job.Status.Succeeded > 0 {
				logger.Info("Job completed successfully", "node", nm.node.Name, "job", jobName)
				if err := nm.JobCompleted(job); err != nil {
					logger.Error(err, "Failed to trigger JobCompleted event", "node", nm.node.Name)
				}
				return
			}

			if job.Status.Failed > 0 {
				logger.Info("Job failed", "node", nm.node.Name, "job", jobName)
				if err := nm.JobFailed(); err != nil {
					logger.Error(err, "Failed to trigger JobFailed event", "node", nm.node.Name)
				}
				return
			}

			logger.V(1).Info("Job still running", "node", nm.node.Name, "job", jobName)
		}
	}
}

// createStartupJob creates a Kubernetes Job for node startup
func (nm *NodeStateMachine) createStartupJob() (*batchv1.Job, error) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-startup-%d", nm.node.Name, time.Now().Unix()),
			Namespace: nm.node.Namespace,
			Labels: map[string]string{
				"app":   "homelab-autoscaler",
				"group": nm.node.Labels["group"],
				"node":  nm.node.Spec.KubernetesNodeName,
				"type":  "startup",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "homelab-autoscaler",
						"group": nm.node.Labels["group"],
						"node":  nm.node.Spec.KubernetesNodeName,
						"type":  "startup",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "startup",
							Image:   nm.node.Spec.StartupPodSpec.Image,
							Command: nm.node.Spec.StartupPodSpec.Command,
							Args:    nm.node.Spec.StartupPodSpec.Args,
						},
					},
				},
			},
		},
	}

	// Set owner reference
	nm.setOwnerReference(job)

	// Create the job
	if err := nm.client.Create(context.TODO(), job); err != nil {
		return nil, err
	}

	return job, nil
}

// createShutdownJob creates a Kubernetes Job for node shutdown
func (nm *NodeStateMachine) createShutdownJob() (*batchv1.Job, error) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-shutdown-%d", nm.node.Name, time.Now().Unix()),
			Namespace: nm.node.Namespace,
			Labels: map[string]string{
				"app":   "homelab-autoscaler",
				"group": nm.node.Labels["group"],
				"node":  nm.node.Spec.KubernetesNodeName,
				"type":  "shutdown",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "homelab-autoscaler",
						"group": nm.node.Labels["group"],
						"node":  nm.node.Spec.KubernetesNodeName,
						"type":  "shutdown",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "shutdown",
							Image:   nm.node.Spec.ShutdownPodSpec.Image,
							Command: nm.node.Spec.ShutdownPodSpec.Command,
							Args:    nm.node.Spec.ShutdownPodSpec.Args,
						},
					},
				},
			},
		},
	}

	// Set owner reference
	nm.setOwnerReference(job)

	// Create the job
	if err := nm.client.Create(context.TODO(), job); err != nil {
		return nil, err
	}

	return job, nil
}

// setOwnerReference sets the owner reference for a job
func (nm *NodeStateMachine) setOwnerReference(job *batchv1.Job) {
	gvk := infrav1alpha1.GroupVersion.WithKind("Node")
	job.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Name:       nm.node.Name,
			UID:        nm.node.UID,
			Controller: &[]bool{true}[0],
		},
	})
}

// updateNodeProgress updates the node progress status
func (nm *NodeStateMachine) updateNodeProgress(progress infrav1alpha1.Progress) {
	ctx := context.TODO()

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := nm.client.Get(ctx, client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		// Update progress
		fresh.Status.Progress = progress

		// Update timestamps based on progress
		now := metav1.Now()
		switch progress {
		case infrav1alpha1.ProgressStartingUp:
			fresh.Status.LastStartupTime = &now
		case infrav1alpha1.ProgressShuttingDown:
			fresh.Status.LastShutdownTime = &now
		}

		// Update the node status
		if err := nm.client.Status().Update(ctx, fresh); err != nil {
			return err
		}

		// Update our local copy
		nm.node = fresh
		return nil
	}); err != nil {
		logger := log.Log.WithName("fsm")
		logger.Error(err, "Failed to update node progress", "node", nm.node.Name, "progress", progress)
	}
}

// setNodeUnschedulable cordons the Kubernetes node to prevent new pods from being scheduled
func (nm *NodeStateMachine) setNodeUnschedulable(nodeName string) error {
	logger := log.Log.WithName("fsm").WithValues("node", nm.node.Name, "kubernetesNode", nodeName, "action", "cordon")

	if nodeName == "" {
		logger.V(1).Info("No Kubernetes node name specified, skipping cordon operation")
		return nil
	}

	ctx := context.TODO()

	// Get the Kubernetes node
	kubeNode := &corev1.Node{}
	if err := nm.client.Get(ctx, client.ObjectKey{Name: nodeName}, kubeNode); err != nil {
		logger.Info("Kubernetes node not found, skipping cordon operation", "error", err.Error())
		return nil // Don't fail the operation if the node doesn't exist
	}

	// Check if already unschedulable
	if kubeNode.Spec.Unschedulable {
		logger.V(1).Info("Kubernetes node is already unschedulable")
		return nil
	}

	// Set node as unschedulable
	kubeNode.Spec.Unschedulable = true

	if err := nm.client.Update(ctx, kubeNode); err != nil {
		logger.Error(err, "Failed to cordon Kubernetes node")
		return err
	}

	logger.Info("Successfully cordoned Kubernetes node")
	return nil
}

// setNodeSchedulable uncordons the Kubernetes node to allow new pods to be scheduled
func (nm *NodeStateMachine) setNodeSchedulable(nodeName string) error {
	logger := log.Log.WithName("fsm").WithValues("node", nm.node.Name, "kubernetesNode", nodeName, "action", "uncordon")

	if nodeName == "" {
		logger.V(1).Info("No Kubernetes node name specified, skipping uncordon operation")
		return nil
	}

	ctx := context.TODO()

	// Get the Kubernetes node
	kubeNode := &corev1.Node{}
	if err := nm.client.Get(ctx, client.ObjectKey{Name: nodeName}, kubeNode); err != nil {
		logger.Info("Kubernetes node not found, skipping uncordon operation", "error", err.Error())
		return nil // Don't fail the operation if the node doesn't exist
	}

	// Check if already schedulable
	if !kubeNode.Spec.Unschedulable {
		logger.V(1).Info("Kubernetes node is already schedulable")
		return nil
	}

	// Set node as schedulable
	kubeNode.Spec.Unschedulable = false

	if err := nm.client.Update(ctx, kubeNode); err != nil {
		logger.Error(err, "Failed to uncordon Kubernetes node")
		return err
	}

	logger.Info("Successfully uncordoned Kubernetes node")
	return nil
}

// removeClusterAutoscalerTaint removes the ToBeDeletedByClusterAutoscaler taint from the Kubernetes node if present
func (nm *NodeStateMachine) removeClusterAutoscalerTaint(nodeName string) error {
	logger := log.Log.WithName("fsm").WithValues("node", nm.node.Name, "kubernetesNode", nodeName, "action", "removeTaint")

	if nodeName == "" {
		logger.V(1).Info("No Kubernetes node name specified, skipping taint removal operation")
		return nil
	}

	ctx := context.TODO()

	// Get the Kubernetes node
	kubeNode := &corev1.Node{}
	if err := nm.client.Get(ctx, client.ObjectKey{Name: nodeName}, kubeNode); err != nil {
		logger.Info("Kubernetes node not found, skipping taint removal operation", "error", err.Error())
		return nil // Don't fail the operation if the node doesn't exist
	}

	// Check if the taint exists
	taintKey := "ToBeDeletedByClusterAutoscaler"
	taintIndex := -1
	for i, taint := range kubeNode.Spec.Taints {
		if taint.Key == taintKey {
			taintIndex = i
			break
		}
	}

	// If taint doesn't exist, nothing to do
	if taintIndex == -1 {
		logger.V(1).Info("Cluster autoscaler taint not found on node")
		return nil
	}

	// Remove the taint
	kubeNode.Spec.Taints = append(kubeNode.Spec.Taints[:taintIndex], kubeNode.Spec.Taints[taintIndex+1:]...)

	if err := nm.client.Update(ctx, kubeNode); err != nil {
		logger.Error(err, "Failed to remove cluster autoscaler taint from Kubernetes node")
		return err
	}

	logger.Info("Successfully removed cluster autoscaler taint from Kubernetes node")
	return nil
}

// addNodeCondition adds a condition to the node status
func (nm *NodeStateMachine) addNodeCondition(condition metav1.Condition) {
	ctx := context.TODO()

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := nm.client.Get(ctx, client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		// Add condition
		fresh.Status.Conditions = append(fresh.Status.Conditions, condition)

		// Update the node status
		if err := nm.client.Status().Update(ctx, fresh); err != nil {
			return err
		}

		// Update our local copy
		nm.node = fresh
		return nil
	}); err != nil {
		logger := log.Log.WithName("fsm")
		logger.Error(err, "Failed to add node condition", "node", nm.node.Name, "condition", condition.Type)
	}
}

// cleanupPendingJobs finds and deletes any existing startup or shutdown jobs for the node
func (nm *NodeStateMachine) cleanupPendingJobs(nodeKubernetesName string) error {
	logger := log.Log.WithName("fsm").WithValues("node", nm.node.Name, "kubernetesNode", nodeKubernetesName, "action", "cleanup")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if nodeKubernetesName == "" {
		logger.V(1).Info("No Kubernetes node name specified, skipping job cleanup")
		return nil
	}

	// Create label selector for jobs belonging to this node
	labelSelector := labels.SelectorFromSet(labels.Set{
		"node": nodeKubernetesName,
	})

	// List all jobs with the node label
	jobList := &batchv1.JobList{}
	listOpts := &client.ListOptions{
		Namespace:     nm.node.Namespace,
		LabelSelector: labelSelector,
	}

	if err := nm.client.List(ctx, jobList, listOpts); err != nil {
		logger.Error(err, "Failed to list jobs for cleanup")
		return fmt.Errorf("failed to list jobs: %w", err)
	}

	var deletedJobs []string
	var errors []error

	for _, job := range jobList.Items {
		// Check if this is a startup or shutdown job
		jobType, exists := job.Labels["type"]
		if !exists || (jobType != "startup" && jobType != "shutdown") {
			continue
		}

		// Check if job is still pending or running (not completed)
		if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
			logger.Info("Deleting pending job", "job", job.Name, "type", jobType)

			// Delete the job with proper cleanup
			deletePolicy := metav1.DeletePropagationForeground
			deleteOpts := &client.DeleteOptions{
				PropagationPolicy: &deletePolicy,
			}

			if err := nm.client.Delete(ctx, &job, deleteOpts); err != nil {
				logger.Error(err, "Failed to delete pending job", "job", job.Name, "type", jobType)
				errors = append(errors, fmt.Errorf("failed to delete job %s: %w", job.Name, err))
			} else {
				deletedJobs = append(deletedJobs, job.Name)
			}
		} else {
			logger.V(1).Info("Skipping completed job", "job", job.Name, "type", jobType, "succeeded", job.Status.Succeeded, "failed", job.Status.Failed)
		}
	}

	if len(deletedJobs) > 0 {
		logger.Info("Successfully cleaned up pending jobs", "deletedJobs", deletedJobs)
	} else {
		logger.V(1).Info("No pending jobs found to cleanup")
	}

	// Return combined errors if any occurred, but don't fail the operation
	if len(errors) > 0 {
		logger.Error(fmt.Errorf("some job cleanup operations failed"), "Job cleanup completed with errors", "errorCount", len(errors))
		// Return the first error for logging purposes, but operation continues
		return errors[0]
	}

	return nil
}
