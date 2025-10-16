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
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// Coordination annotation keys for preventing race conditions with cluster autoscaler
const (
	OperationLockAnnotation = "homelab-autoscaler.dev/operation-lock"
	LockOwnerAnnotation     = "homelab-autoscaler.dev/lock-owner"
	LockTimestampAnnotation = "homelab-autoscaler.dev/lock-timestamp"
	LockTimeoutAnnotation   = "homelab-autoscaler.dev/lock-timeout"
	DefaultLockTimeout      = 5 * time.Minute
	NodeControllerOwner     = "node-controller"
)

// OperationLock represents a coordination lock on a node
type OperationLock struct {
	Operation string
	Owner     string
	Timestamp time.Time
	Timeout   time.Duration
}

// NodeReconciler reconciles a Node object
type NodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/finalizers,verbs=update

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Clean up expired locks at the beginning of reconciliation
	if err := r.cleanupExpiredLocks(ctx); err != nil {
		logger.Error(err, "Failed to cleanup expired locks, continuing with reconciliation")
		// Don't fail reconciliation due to cleanup errors, just log and continue
	}

	// Get the Node resource from the request
	node := &infrav1alpha1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// The object was not found, return early
		return ctrl.Result{}, err
	}

	if node.Labels["group"] != "" || node.Labels["infra.homecluster.dev/group"] != node.Labels["group"] {
		if err := r.addLabelToKubernetesNodeWithPatch(ctx, node.Spec.KubernetesNodeName, "infra.homecluster.dev/group", node.Labels["group"]); err != nil {
			logger.Error(err, "Failed to add group label to Kubernetes node", "node", node.Spec.KubernetesNodeName)
		}
		if err := r.addLabelToKubernetesNodeWithPatch(ctx, node.Spec.KubernetesNodeName, "infra.homecluster.dev/node", node.Name); err != nil {
			logger.Error(err, "Failed to add node label to Kubernetes node", "node", node.Spec.KubernetesNodeName)
		}
	}
	// Check if the Node has a group label
	groupName, hasGroupLabel := node.Labels["group"]
	if !hasGroupLabel {
		logger.Info("Node does not have a group label, skipping owner reference management", "node", node.Name)
		return ctrl.Result{}, fmt.Errorf("node %s has no group label, skipping", node.Name)
	}

	// Get the Group CR referenced by the label
	group := &infrav1alpha1.Group{}
	if err := r.Get(ctx, types.NamespacedName{Name: groupName, Namespace: node.Namespace}, group); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Group referenced by node label not found", "group", groupName, "node", node.Name)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Group", "group", groupName)
		return ctrl.Result{}, err
	}

	if err := r.SetControllerReference(ctx, node, group); err != nil {
		logger.Error(err, "Failed to set controller reference", "node", node.Name, "group", group.Name)
		return ctrl.Result{}, err
	}

	if !node.DeletionTimestamp.IsZero() {
		return r.handleNodeDeletion(ctx, node, groupName, req.Name)
	}

	// Validate state before attempting transitions with smart backoff
	if result, err := r.validateStateTransition(ctx, node); err != nil {
		logger.Error(err, "Invalid state transition", "node", node.Name)
		return ctrl.Result{}, err
	} else if result.RequeueAfter > 0 {
		// Smart backoff is requesting a requeue
		return result, nil
	}

	// Handle power state transitions with proper logic
	if node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOn &&
		(node.Status.PowerState == infrav1alpha1.PowerStateOff || node.Status.PowerState == "") &&
		node.Status.Progress != infrav1alpha1.ProgressStartingUp {

		err := r.startNode(ctx, node)
		if err != nil {
			logger.Error(err, "Failed to start Node", "node", node.Name)
			// Check if this is a coordination lock error - use longer backoff to avoid conflicts
			if fmt.Sprintf("%v", err) != "" && (fmt.Sprintf("%v", err) == "coordination lock" ||
				fmt.Sprintf("%v", err) == "lock already exists") {
				logger.Info("Coordination lock conflict during node start, backing off",
					"node", node.Name,
					"backoff", "2 minutes")
				return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
			}
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

	} else if node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOff &&
		node.Status.PowerState == infrav1alpha1.PowerStateOn &&
		node.Status.Progress != infrav1alpha1.ProgressShuttingDown {

		err := r.shutdownNode(ctx, node)
		if err != nil {
			logger.Error(err, "Failed to shutdown Node", "node", node.Name)
			// Check if this is a coordination lock error - use longer backoff to avoid conflicts
			if fmt.Sprintf("%v", err) != "" && (fmt.Sprintf("%v", err) == "coordination lock" ||
				fmt.Sprintf("%v", err) == "lock already exists") {
				logger.Info("Coordination lock conflict during node shutdown, backing off",
					"node", node.Name,
					"backoff", "2 minutes")
				return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
			}
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
	} else {
		// Node is already in desired state or transitioning
		logger.Info("Node is in desired state or transitioning",
			"node", node.Name,
			"desiredState", node.Spec.DesiredPowerState,
			"currentState", node.Status.PowerState,
			"progress", node.Status.Progress)
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil // Check again later
	}

	logger.Info("Successfully reconciled Node", "node", node.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Register field index for pods by node name
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{pod.Spec.NodeName}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.Node{}).
		Named("node").
		Complete(r)
}

// handleNodeDeletion handles the deletion of a node
func (r *NodeReconciler) handleNodeDeletion(ctx context.Context, node *infrav1alpha1.Node, groupName, nodeName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Node is being deleted", "node", nodeName)

	return ctrl.Result{}, nil
}

func (r *NodeReconciler) addLabelToKubernetesNodeWithPatch(ctx context.Context, nodeName, labelKey, labelValue string) error {
	logger := log.FromContext(ctx)

	// Get the Kubernetes node
	kubernetesNode := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, kubernetesNode); err != nil {
		logger.Error(err, "Failed to get Kubernetes node", "node", nodeName)
		return err
	}

	// Create a patch with the new label
	patch := client.MergeFrom(kubernetesNode.DeepCopy())

	// Initialize the Labels map if it's nil
	if kubernetesNode.Labels == nil {
		kubernetesNode.Labels = make(map[string]string)
	}

	// Add the label
	kubernetesNode.Labels[labelKey] = labelValue

	// Apply the patch
	if err := r.Patch(ctx, kubernetesNode, patch); err != nil {
		logger.Error(err, "Failed to patch Kubernetes node with label", "node", nodeName, "label", labelKey)
		return err
	}

	logger.Info("Successfully patched Kubernetes node with label", "node", nodeName, "label", labelKey, "value", labelValue)
	return nil
}

func (r *NodeReconciler) shutdownNode(ctx context.Context, nodeCR *infrav1alpha1.Node) error {
	logger := log.FromContext(ctx)

	// Acquire operation lock for scale-down operation
	if err := r.acquireOperationLock(ctx, nodeCR, "scale-down", DefaultLockTimeout); err != nil {
		logger.Info("Failed to acquire coordination lock for scale-down operation",
			"node", nodeCR.Name,
			"error", err)
		return fmt.Errorf("cannot shutdown node due to coordination lock: %w", err)
	}

	logger.Info("Acquired coordination lock for scale-down operation", "node", nodeCR.Name)

	// Mark the Kubernetes node as not schedulable before starting shutdown
	k8sNode := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: nodeCR.Spec.KubernetesNodeName}, k8sNode); err != nil {
		logger.Error(err, "failed to get Kubernetes node", "nodeName", nodeCR.Spec.KubernetesNodeName)
		// Release lock on error
		if releaseErr := r.releaseOperationLock(ctx, nodeCR); releaseErr != nil {
			logger.Error(releaseErr, "Failed to release coordination lock after error", "node", nodeCR.Name)
		}
		return err
	}

	// Mark the node as unschedulable
	k8sNode.Spec.Unschedulable = true
	if err := r.Update(ctx, k8sNode); err != nil {
		logger.Error(err, "failed to mark Kubernetes node as unschedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)
		// Release lock on error
		if releaseErr := r.releaseOperationLock(ctx, nodeCR); releaseErr != nil {
			logger.Error(releaseErr, "Failed to release coordination lock after error", "node", nodeCR.Name)
		}
		return err
	}

	logger.Info("Successfully marked Kubernetes node as unschedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)

	// // Drain the node by evicting all pods
	// if err := r.drainNode(ctx, nodeCR.Spec.KubernetesNodeName); err != nil {
	// 	logger.Error(err, "failed to drain node", "nodeName", nodeCR.Spec.KubernetesNodeName)
	// 	// Release lock on error
	// 	if releaseErr := r.releaseOperationLock(ctx, nodeCR); releaseErr != nil {
	// 		logger.Error(releaseErr, "Failed to release coordination lock after error", "node", nodeCR.Name)
	// 	}
	// 	return err
	// }

	// logger.Info("Successfully drained node", "nodeName", nodeCR.Spec.KubernetesNodeName)

	// Create a Job to shutdown the node
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-shutdown-%s", nodeCR.Name, fmt.Sprintf("%d", metav1.Now().Unix())),
			Namespace: nodeCR.Namespace,
			Labels: map[string]string{
				"app":   "homelab-autoscaler",
				"group": nodeCR.Labels["group"],
				"node":  nodeCR.Spec.KubernetesNodeName,
				"type":  "shutdown",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "homelab-autoscaler",
						"group": nodeCR.Labels["group"],
						"node":  nodeCR.Spec.KubernetesNodeName,
						"type":  "shutdown",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "shutdown",
							Image:   nodeCR.Spec.ShutdownPodSpec.Image,
							Command: nodeCR.Spec.ShutdownPodSpec.Command,
							Args:    nodeCR.Spec.ShutdownPodSpec.Args,
						},
					},
				},
			},
		},
	}

	// Set owner reference to the Node CR
	if err := ctrl.SetControllerReference(nodeCR, job, r.Scheme); err != nil {
		// Release lock on error
		if releaseErr := r.releaseOperationLock(ctx, nodeCR); releaseErr != nil {
			logger.Error(releaseErr, "Failed to release coordination lock after error", "node", nodeCR.Name)
		}
		return err
	}

	// Create the Job
	if err := r.Create(ctx, job); err != nil {
		// Release lock on error
		if releaseErr := r.releaseOperationLock(ctx, nodeCR); releaseErr != nil {
			logger.Error(releaseErr, "Failed to release coordination lock after error", "node", nodeCR.Name)
		}
		return err
	}

	logger.Info("Successfully created shutdown job", "job", job.Name, "node", nodeCR.Spec.KubernetesNodeName)

	// Update the Node status condition to "progressing" immediately for UI feedback
	nodeCR.Status.Conditions = append(nodeCR.Status.Conditions, metav1.Condition{
		Type:               "progressing",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeGroupDecreasingSize",
		Message:            fmt.Sprintf("Shutting down node via job %s", job.Name),
	})
	nodeCR.Status.LastShutdownTime = &metav1.Time{Time: time.Now().UTC()}
	nodeCR.Status.Progress = infrav1alpha1.ProgressShuttingDown

	// Update the Node with the new status info
	if err := r.Update(ctx, nodeCR); err != nil {
		logger.Error(err, "Failed to update Node with status condition", "node", nodeCR.Name)
		// Don't return error here as job is already created, let async function handle it
	}

	// Launch async goroutine to wait for job completion and release lock
	go r.shutdownNodeAsync(ctx, nodeCR, job.Name)

	return nil
}

func (r *NodeReconciler) startNode(ctx context.Context, nodeCR *infrav1alpha1.Node) error {
	logger := log.FromContext(ctx)

	// Acquire operation lock for scale-up operation
	if err := r.acquireOperationLock(ctx, nodeCR, "scale-up", DefaultLockTimeout); err != nil {
		logger.Info("Failed to acquire coordination lock for scale-up operation",
			"node", nodeCR.Name,
			"error", err)
		return fmt.Errorf("cannot start node due to coordination lock: %w", err)
	}

	logger.Info("Acquired coordination lock for scale-up operation", "node", nodeCR.Name)

	// Get the Kubernetes node object and set it as schedulable
	k8sNode := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: nodeCR.Spec.KubernetesNodeName}, k8sNode); err != nil {
		logger.Error(err, "failed to get Kubernetes node", "nodeName", nodeCR.Spec.KubernetesNodeName)
		// Release lock on error
		if releaseErr := r.releaseOperationLock(ctx, nodeCR); releaseErr != nil {
			logger.Error(releaseErr, "Failed to release coordination lock after error", "node", nodeCR.Name)
		}
		return err
	}

	// Set the node as schedulable
	k8sNode.Spec.Unschedulable = false

	// Remove cluster autoscaler taints by key
	var filteredTaints []corev1.Taint
	for _, taint := range k8sNode.Spec.Taints {
		if taint.Key != "ToBeDeletedByClusterAutoscaler" && taint.Key != "DeletionCandidateOfClusterAutoscaler" {
			filteredTaints = append(filteredTaints, taint)
		}
	}
	k8sNode.Spec.Taints = filteredTaints

	if err := r.Update(ctx, k8sNode); err != nil {
		logger.Error(err, "failed to mark Kubernetes node as schedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)
		// Release lock on error
		if releaseErr := r.releaseOperationLock(ctx, nodeCR); releaseErr != nil {
			logger.Error(releaseErr, "Failed to release coordination lock after error", "node", nodeCR.Name)
		}
		return err
	}

	logger.Info("Successfully marked Kubernetes node as schedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)

	// Create a Job to start the unhealthy node
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-startup-%s", nodeCR.Name, fmt.Sprintf("%d", metav1.Now().Unix())),
			Namespace: nodeCR.Namespace,
			Labels: map[string]string{
				"app":   "homelab-autoscaler",
				"group": nodeCR.Labels["group"],
				"node":  nodeCR.Spec.KubernetesNodeName,
				"type":  "startup",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "homelab-autoscaler",
						"group": nodeCR.Labels["group"],
						"node":  nodeCR.Spec.KubernetesNodeName,
						"type":  "startup",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "startup",
							Image:   nodeCR.Spec.StartupPodSpec.Image,
							Command: nodeCR.Spec.StartupPodSpec.Command,
							Args:    nodeCR.Spec.StartupPodSpec.Args,
						},
					},
				},
			},
		},
	}

	// Set owner reference to the Node CR
	if err := ctrl.SetControllerReference(nodeCR, job, r.Scheme); err != nil {
		// Release lock on error
		if releaseErr := r.releaseOperationLock(ctx, nodeCR); releaseErr != nil {
			logger.Error(releaseErr, "Failed to release coordination lock after error", "node", nodeCR.Name)
		}
		return err
	}

	// Create the Job
	if err := r.Create(ctx, job); err != nil {
		// Release lock on error
		if releaseErr := r.releaseOperationLock(ctx, nodeCR); releaseErr != nil {
			logger.Error(releaseErr, "Failed to release coordination lock after error", "node", nodeCR.Name)
		}
		return err
	}

	logger.Info("Successfully created startup job", "job", job.Name, "node", nodeCR.Name)

	// Update the Node status condition to "progressing" immediately for UI feedback
	nodeCR.Status.Conditions = append(nodeCR.Status.Conditions, metav1.Condition{
		Type:               "progressing",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeGroupIncreaseSize",
		Message:            fmt.Sprintf("Starting node via job %s", job.Name),
	})
	nodeCR.Status.LastStartupTime = &metav1.Time{Time: time.Now().UTC()}
	nodeCR.Status.Progress = infrav1alpha1.ProgressStartingUp

	// Update the Node with the new status info
	if err := r.Update(ctx, nodeCR); err != nil {
		logger.Error(err, "Failed to update Node with status condition", "node", nodeCR.Name)
		// Don't return error here as job is already created, let async function handle it
	}

	// Launch async goroutine to wait for job completion and release lock
	go r.startNodeAsync(ctx, nodeCR, job.Name)

	return nil
}

// waitForJobCompletion waits for a job to complete with polling and timeout
func (r *NodeReconciler) waitForJobCompletion(ctx context.Context, jobName, namespace string, timeout time.Duration) error {
	logger := log.FromContext(ctx)

	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Poll every 30 seconds
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	logger.Info("Waiting for job completion", "job", jobName, "timeout", timeout)

	for {
		select {
		case <-timeoutCtx.Done():
			logger.Error(timeoutCtx.Err(), "Timeout waiting for job completion", "job", jobName)
			return fmt.Errorf("timeout waiting for job %s to complete: %w", jobName, timeoutCtx.Err())
		case <-ticker.C:
			// Check job status
			job := &batchv1.Job{}
			if err := r.Get(timeoutCtx, types.NamespacedName{Name: jobName, Namespace: namespace}, job); err != nil {
				if errors.IsNotFound(err) {
					logger.Info("Job not found, may have been cleaned up", "job", jobName)
					return nil
				}
				logger.Error(err, "Failed to get job status", "job", jobName)
				continue
			}

			// Check if job completed successfully
			if job.Status.Succeeded > 0 {
				logger.Info("Job completed successfully", "job", jobName)
				return nil
			}

			// Check if job failed
			if job.Status.Failed > 0 {
				logger.Error(nil, "Job failed", "job", jobName, "failedPods", job.Status.Failed)
				return fmt.Errorf("job %s failed with %d failed pods", jobName, job.Status.Failed)
			}

			// Job is still running
			logger.Info("Job still running", "job", jobName, "active", job.Status.Active)
		}
	}
}

// startNodeAsync runs the node startup process asynchronously and waits for job completion before releasing the lock
func (r *NodeReconciler) startNodeAsync(ctx context.Context, nodeCR *infrav1alpha1.Node, jobName string) {
	logger := log.FromContext(ctx)

	// Ensure lock is always released
	defer func() {
		if err := r.releaseOperationLock(ctx, nodeCR); err != nil {
			logger.Error(err, "Failed to release coordination lock after startNodeAsync operation",
				"node", nodeCR.Name)
		} else {
			logger.Info("Successfully released coordination lock after startNodeAsync operation",
				"node", nodeCR.Name)
		}
	}()

	// Wait for job completion with 5-minute timeout
	if err := r.waitForJobCompletion(ctx, jobName, nodeCR.Namespace, 5*time.Minute); err != nil {
		logger.Error(err, "Startup job did not complete successfully", "node", nodeCR.Name, "job", jobName)

		// Update node status to reflect failure
		if updateErr := r.updateNodeStatusAfterJobFailure(ctx, nodeCR, "startup", err); updateErr != nil {
			logger.Error(updateErr, "Failed to update node status after job failure", "node", nodeCR.Name)
		}
		return
	}

	// Update node status to reflect successful completion
	if err := r.updateNodeStatusAfterJobSuccess(ctx, nodeCR, "startup"); err != nil {
		logger.Error(err, "Failed to update node status after job success", "node", nodeCR.Name)
	}

	logger.Info("Node startup completed successfully", "node", nodeCR.Name, "job", jobName)
}

// shutdownNodeAsync runs the node shutdown process asynchronously and waits for job completion before releasing the lock
func (r *NodeReconciler) shutdownNodeAsync(ctx context.Context, nodeCR *infrav1alpha1.Node, jobName string) {
	logger := log.FromContext(ctx)

	// Ensure lock is always released
	defer func() {
		if err := r.releaseOperationLock(ctx, nodeCR); err != nil {
			logger.Error(err, "Failed to release coordination lock after shutdownNodeAsync operation",
				"node", nodeCR.Name)
		} else {
			logger.Info("Successfully released coordination lock after shutdownNodeAsync operation",
				"node", nodeCR.Name)
		}
	}()

	// Wait for job completion with 5-minute timeout
	if err := r.waitForJobCompletion(ctx, jobName, nodeCR.Namespace, 5*time.Minute); err != nil {
		logger.Error(err, "Shutdown job did not complete successfully", "node", nodeCR.Name, "job", jobName)

		// Update node status to reflect failure
		if updateErr := r.updateNodeStatusAfterJobFailure(ctx, nodeCR, "shutdown", err); updateErr != nil {
			logger.Error(updateErr, "Failed to update node status after job failure", "node", nodeCR.Name)
		}
		return
	}

	// Update node status to reflect successful completion
	if err := r.updateNodeStatusAfterJobSuccess(ctx, nodeCR, "shutdown"); err != nil {
		logger.Error(err, "Failed to update node status after job success", "node", nodeCR.Name)
	}

	logger.Info("Node shutdown completed successfully", "node", nodeCR.Name, "job", jobName)
}

// updateNodeStatusAfterJobSuccess updates the node status after a job completes successfully
func (r *NodeReconciler) updateNodeStatusAfterJobSuccess(ctx context.Context, nodeCR *infrav1alpha1.Node, operation string) error {
	logger := log.FromContext(ctx)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(nodeCR), fresh); err != nil {
			return fmt.Errorf("failed to get fresh node: %w", err)
		}

		// Update status based on operation
		switch operation {
		case "startup":
			fresh.Status.Conditions = append(fresh.Status.Conditions, metav1.Condition{
				Type:               "StartupJobCompleted",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "NodeStartupJobCompleted",
				Message:            "Node startup job completed successfully",
			})
		case "shutdown":
			fresh.Status.Conditions = append(fresh.Status.Conditions, metav1.Condition{
				Type:               "ShutdownJobCompleted",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "NodeShutdownJobCompleted",
				Message:            "Node shutdown job completed successfully",
			})
		}

		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}

		logger.Info("Successfully updated node status after job success",
			"node", fresh.Name, "operation", operation, "powerState", fresh.Status.PowerState)
		return nil
	})
}

// updateNodeStatusAfterJobFailure updates the node status after a job fails
func (r *NodeReconciler) updateNodeStatusAfterJobFailure(ctx context.Context, nodeCR *infrav1alpha1.Node, operation string, jobErr error) error {
	logger := log.FromContext(ctx)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(nodeCR), fresh); err != nil {
			return fmt.Errorf("failed to get fresh node: %w", err)
		}

		// Clear progress and add failure condition
		// fresh.Status.Progress = ""
		fresh.Status.Conditions = append(fresh.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             fmt.Sprintf("Node%sJobFailed", operation),
			Message:            fmt.Sprintf("Node %s job failed: %v", operation, jobErr),
		})

		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}

		logger.Info("Successfully updated node status after job failure",
			"node", fresh.Name, "operation", operation, "error", jobErr)
		return nil
	})
}

// validateStateTransition validates that a state transition is valid and implements smart backoff strategy
// Returns ctrl.Result with appropriate requeue timing instead of errors for transitional states
func (r *NodeReconciler) validateStateTransition(ctx context.Context, node *infrav1alpha1.Node) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check for context cancellation
	if ctx.Err() != nil {
		return ctrl.Result{}, ctx.Err()
	}

	// Validate that we have required fields
	if node.Spec.DesiredPowerState == "" {
		return ctrl.Result{}, fmt.Errorf("node %s has empty DesiredPowerState", node.Name)
	}

	// Validate that DesiredPowerState is a valid value
	if node.Spec.DesiredPowerState != infrav1alpha1.PowerStateOn &&
		node.Spec.DesiredPowerState != infrav1alpha1.PowerStateOff {
		return ctrl.Result{}, fmt.Errorf("node %s has invalid DesiredPowerState: %s", node.Name, node.Spec.DesiredPowerState)
	}

	// Check if node is in a transitional state and apply smart backoff
	if node.Status.Progress == infrav1alpha1.ProgressStartingUp ||
		node.Status.Progress == infrav1alpha1.ProgressShuttingDown {

		// Check for existing operation locks before allowing transitions
		if existingLock, exists := r.checkOperationLock(ctx, node); exists {
			if !r.isLockExpired(existingLock) {
				// Respect locks from other controllers (like cluster autoscaler)
				if existingLock.Owner != NodeControllerOwner {
					logger.Info("Respecting coordination lock from other controller",
						"node", node.Name,
						"lockOwner", existingLock.Owner,
						"operation", existingLock.Operation,
						"age", time.Since(existingLock.Timestamp))
					return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
				}
			} else {
				// Clean up expired locks automatically
				logger.Info("Cleaning up expired coordination lock",
					"node", node.Name,
					"operation", existingLock.Operation,
					"owner", existingLock.Owner,
					"age", time.Since(existingLock.Timestamp))
				if err := r.forceReleaseLock(ctx, node); err != nil {
					logger.Error(err, "Failed to cleanup expired lock", "node", node.Name)
				}
			}
		}

		// Calculate transition duration based on the appropriate timestamp
		var transitionStart time.Time
		if node.Status.Progress == infrav1alpha1.ProgressStartingUp && node.Status.LastStartupTime != nil {
			transitionStart = node.Status.LastStartupTime.Time
		} else if node.Status.Progress == infrav1alpha1.ProgressShuttingDown && node.Status.LastShutdownTime != nil {
			transitionStart = node.Status.LastShutdownTime.Time
		} else {
			// Fallback to a reasonable default if timestamps are missing
			transitionStart = time.Now().Add(-1 * time.Minute)
		}

		transitionDuration := time.Since(transitionStart)

		// Implement progressive backoff strategy
		var backoffDuration time.Duration
		var logMessage string

		switch {
		case transitionDuration <= 2*time.Minute:
			// Early transitions (0-2 min): 30-second backoff, allow monitoring
			backoffDuration = 30 * time.Second
			logMessage = "Early transition phase - monitoring with short backoff"
		case transitionDuration <= 10*time.Minute:
			// Normal transitions (2-10 min): 2-minute backoff
			backoffDuration = 2 * time.Minute
			logMessage = "Normal transition phase - standard backoff"
		case transitionDuration <= 15*time.Minute:
			// Late transitions (10-15 min): 5-minute backoff
			backoffDuration = 5 * time.Minute
			logMessage = "Late transition phase - extended backoff"
		default:
			// Stuck transitions (>15 min): Force retry/cleanup
			logger.Info("Transition appears stuck, forcing cleanup and retry",
				"node", node.Name,
				"progress", node.Status.Progress,
				"transitionDuration", transitionDuration)

			// Clean up any existing locks and allow the transition to proceed
			if _, exists := r.checkOperationLock(ctx, node); exists {
				if err := r.forceReleaseLock(ctx, node); err != nil {
					logger.Error(err, "Failed to force release stuck lock", "node", node.Name)
				}
			}

			// Return no requeue to allow immediate retry
			return ctrl.Result{}, nil
		}

		logger.Info(logMessage,
			"node", node.Name,
			"progress", node.Status.Progress,
			"transitionDuration", transitionDuration,
			"backoffDuration", backoffDuration)

		return ctrl.Result{RequeueAfter: backoffDuration}, nil
	}

	// Node is not in transitional state, allow normal processing
	return ctrl.Result{}, nil
}
func (r *NodeReconciler) SetControllerReference(ctx context.Context, node *infrav1alpha1.Node, group *infrav1alpha1.Group) error {
	logger := log.FromContext(ctx)

	// Check if the Node already has an owner reference to this Group
	for _, ownerRef := range node.OwnerReferences {
		if ownerRef.UID == group.UID && ownerRef.Kind == "Group" && ownerRef.APIVersion == "infra.homecluster.dev/v1alpha1" {
			return nil
		}
	}

	// Set the owner reference using controllerutil.SetControllerReference
	if err := ctrl.SetControllerReference(group, node, r.Scheme); err != nil {
		logger.Error(err, "Failed to set controller reference", "node", node.Name, "group", group.Name)
		return err
	}

	// Update the Node with the new owner reference
	if err := r.Update(ctx, node); err != nil {
		logger.Error(err, "Failed to update Node with owner reference", "node", node.Name)
		return err
	}

	logger.Info("Successfully set owner reference for Node to Group", "node", node.Name, "group", group.Name)

	return nil
}

// acquireOperationLock attempts to acquire an operation lock on a node using atomic operations
// with optimistic locking (resource version conflicts). Returns error if lock cannot be acquired.
func (r *NodeReconciler) acquireOperationLock(ctx context.Context, node *infrav1alpha1.Node, operation string, timeout time.Duration) error {
	logger := log.FromContext(ctx)

	if timeout == 0 {
		timeout = DefaultLockTimeout
	}

	// Check if lock already exists and is not expired
	if existingLock, exists := r.checkOperationLock(ctx, node); exists {
		if !r.isLockExpired(existingLock) {
			return fmt.Errorf("coordination lock already exists: operation=%s, owner=%s, age=%v",
				existingLock.Operation, existingLock.Owner, time.Since(existingLock.Timestamp))
		}
		logger.Info("Existing lock expired, proceeding with acquisition",
			"node", node.Name,
			"expiredOperation", existingLock.Operation,
			"expiredOwner", existingLock.Owner,
			"age", time.Since(existingLock.Timestamp))
	}

	// Use optimistic locking with retry to acquire the lock
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
			return fmt.Errorf("failed to get fresh node: %w", err)
		}

		// Double-check lock doesn't exist on fresh copy
		if existingLock, exists := r.checkOperationLock(ctx, fresh); exists {
			if !r.isLockExpired(existingLock) {
				return fmt.Errorf("coordination lock acquired by another process: operation=%s, owner=%s",
					existingLock.Operation, existingLock.Owner)
			}
		}

		// Initialize annotations if needed
		if fresh.Annotations == nil {
			fresh.Annotations = make(map[string]string)
		}

		// Set lock annotations
		fresh.Annotations[OperationLockAnnotation] = operation
		fresh.Annotations[LockOwnerAnnotation] = NodeControllerOwner
		fresh.Annotations[LockTimestampAnnotation] = time.Now().Format(time.RFC3339)
		fresh.Annotations[LockTimeoutAnnotation] = strconv.FormatFloat(timeout.Seconds(), 'f', 0, 64)

		// Attempt to update with optimistic locking
		if err := r.Update(ctx, fresh); err != nil {
			return err
		}

		logger.Info("Successfully acquired coordination lock",
			"node", node.Name,
			"operation", operation,
			"owner", NodeControllerOwner,
			"timeout", timeout)

		return nil
	})
}

// checkOperationLock checks if a node has an active operation lock and returns the lock details.
// Returns (lock, true) if an active lock exists, (nil, false) otherwise.
func (r *NodeReconciler) checkOperationLock(ctx context.Context, node *infrav1alpha1.Node) (*OperationLock, bool) {
	logger := log.FromContext(ctx)

	if node.Annotations == nil {
		return nil, false
	}

	operation := node.Annotations[OperationLockAnnotation]
	owner := node.Annotations[LockOwnerAnnotation]
	timestampStr := node.Annotations[LockTimestampAnnotation]
	timeoutStr := node.Annotations[LockTimeoutAnnotation]

	// Check if required annotations exist
	if operation == "" || owner == "" || timestampStr == "" {
		return nil, false
	}

	// Parse timestamp
	timestamp, err := time.Parse(time.RFC3339, timestampStr)
	if err != nil {
		logger.V(1).Info("Invalid lock timestamp, treating as no lock",
			"node", node.Name,
			"timestamp", timestampStr,
			"error", err)
		return nil, false
	}

	// Parse timeout (default to 5 minutes if invalid)
	timeout := DefaultLockTimeout
	if timeoutStr != "" {
		if timeoutSeconds, err := strconv.ParseFloat(timeoutStr, 64); err == nil {
			timeout = time.Duration(timeoutSeconds) * time.Second
		} else {
			logger.V(1).Info("Invalid lock timeout, using default",
				"node", node.Name,
				"timeout", timeoutStr,
				"default", DefaultLockTimeout)
		}
	}

	lock := &OperationLock{
		Operation: operation,
		Owner:     owner,
		Timestamp: timestamp,
		Timeout:   timeout,
	}

	return lock, true
}

// releaseOperationLock releases an operation lock from a node. Only releases locks owned by this controller.
// Uses optimistic locking to prevent race conditions during release.
func (r *NodeReconciler) releaseOperationLock(ctx context.Context, node *infrav1alpha1.Node) error {
	logger := log.FromContext(ctx)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
			return fmt.Errorf("failed to get fresh node: %w", err)
		}

		if fresh.Annotations == nil {
			logger.V(1).Info("No annotations to clean up", "node", node.Name)
			return nil
		}

		// Check if we own the lock before releasing
		currentOwner := fresh.Annotations[LockOwnerAnnotation]
		if currentOwner != NodeControllerOwner {
			if currentOwner == "" {
				logger.V(1).Info("No lock to release", "node", node.Name)
				return nil
			}
			logger.Info("Cannot release lock owned by different component",
				"node", node.Name,
				"currentOwner", currentOwner,
				"expectedOwner", NodeControllerOwner)
			return fmt.Errorf("cannot release lock owned by %s", currentOwner)
		}

		// Remove coordination annotations
		delete(fresh.Annotations, OperationLockAnnotation)
		delete(fresh.Annotations, LockOwnerAnnotation)
		delete(fresh.Annotations, LockTimestampAnnotation)
		delete(fresh.Annotations, LockTimeoutAnnotation)

		// Update the node
		if err := r.Update(ctx, fresh); err != nil {
			return err
		}

		logger.Info("Successfully released coordination lock",
			"node", node.Name,
			"owner", NodeControllerOwner)

		return nil
	})
}

// isLockExpired checks if an existing lock has expired based on timestamp and timeout.
// Returns true if the lock has exceeded its timeout duration.
func (r *NodeReconciler) isLockExpired(lock *OperationLock) bool {
	if lock == nil {
		return true
	}

	age := time.Since(lock.Timestamp)
	return age > lock.Timeout
}

// cleanupExpiredLocks removes expired locks from all nodes in the cluster.
// This should be called periodically to prevent stale locks from blocking operations.
func (r *NodeReconciler) cleanupExpiredLocks(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// List all nodes to check for expired locks
	nodes := &infrav1alpha1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return fmt.Errorf("failed to list nodes for lock cleanup: %w", err)
	}

	cleanedCount := 0
	for _, node := range nodes.Items {
		if lock, exists := r.checkOperationLock(ctx, &node); exists {
			if r.isLockExpired(lock) {
				logger.Info("Cleaning up expired coordination lock",
					"node", node.Name,
					"operation", lock.Operation,
					"owner", lock.Owner,
					"age", time.Since(lock.Timestamp),
					"timeout", lock.Timeout)

				if err := r.forceReleaseLock(ctx, &node); err != nil {
					logger.Error(err, "Failed to cleanup expired lock", "node", node.Name)
					continue
				}
				cleanedCount++
			}
		}
	}

	if cleanedCount > 0 {
		logger.Info("Completed expired lock cleanup", "cleanedLocks", cleanedCount)
	}

	return nil
}

// forceReleaseLock forcibly removes lock annotations from a node, regardless of ownership.
// This is used for cleanup of expired locks and should be used with caution.
func (r *NodeReconciler) forceReleaseLock(ctx context.Context, node *infrav1alpha1.Node) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
			return fmt.Errorf("failed to get fresh node: %w", err)
		}

		if fresh.Annotations == nil {
			return nil // No annotations to clean up
		}

		// Remove all coordination lock annotations
		delete(fresh.Annotations, OperationLockAnnotation)
		delete(fresh.Annotations, LockOwnerAnnotation)
		delete(fresh.Annotations, LockTimestampAnnotation)
		delete(fresh.Annotations, LockTimeoutAnnotation)

		// Update the node
		return r.Update(ctx, fresh)
	})
}
