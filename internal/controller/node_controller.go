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
	"crypto/sha256"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
)

// Health status constants
const (
	HealthStatusHealthy      = "healthy"
	HealthStatusFailed       = "Failed"
	HealthStatusRunning      = "Running"
	HealthStatusNotScheduled = "NotScheduled"
)

// NodeReconciler reconciles a Node object
type NodeReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	GroupStore *groupstore.GroupStore
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/finalizers,verbs=update

// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Node object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the Node resource from the request
	node := &infrav1alpha1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// The object was not found, return early
		if errors.IsNotFound(err) {
			logger.Info("Node not found, will be removed from groupstore", "node", req.Name)
			// Remove from groupstore if it exists (ignore error if it doesn't exist)
			if err := r.GroupStore.RemoveNode(req.Name); err != nil {
				// Only log the error if it's not a "not found" error
				if !strings.Contains(err.Error(), "not found") {
					logger.Error(err, "Failed to remove node from groupstore", "node", req.Name)
					return ctrl.Result{}, err
				}
				// Node wasn't in groupstore, which is fine
				logger.Info("Node was not in groupstore, nothing to remove", "node", req.Name)
			}
			return ctrl.Result{}, nil
		}
		// Error reading the object - return with requeue
		logger.Error(err, "Failed to get Node", "node", req.Name)
		return ctrl.Result{}, err
	}

	// Check if this is a status-only update that should be skipped
	if shouldSkip, err := r.shouldSkipStatusOnlyUpdate(ctx, node); shouldSkip {
		return ctrl.Result{}, err
	}

	// Validate that the referenced Kubernetes node exists and is schedulable when adding a new Node resource
	// This validation only runs when the node is not already in the GroupStore (i.e., it's a new node)
	if err := r.validateNewNode(ctx, node); err != nil {
		return ctrl.Result{}, err
	}

	// Add or update the Node in the GroupStore
	if r.GroupStore != nil {
		if err := r.GroupStore.AddOrUpdateNode(node); err != nil {
			logger.Error(err, "Failed to add or update node in groupstore", "node", node.Name)
			return ctrl.Result{}, err
		}
	}
	logger.Info("Successfully added or updated node in groupstore", "node", node.Name)

	// Check if the Node has a group label
	groupName, hasGroupLabel := node.Labels["group"]
	if !hasGroupLabel {
		logger.Info("Node does not have a group label, skipping owner reference management", "node", node.Name)
		return ctrl.Result{}, nil
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

	// Check if the Node already has an owner reference to this Group
	hasOwnerRef := false
	for _, ownerRef := range node.OwnerReferences {
		if ownerRef.UID == group.UID && ownerRef.Kind == "Group" && ownerRef.APIVersion == "infra.homecluster.dev/v1alpha1" {
			hasOwnerRef = true
			break
		}
	}

	// Handle deletion
	if !node.DeletionTimestamp.IsZero() {
		return r.handleNodeDeletion(ctx, node, groupName, req.Name)
	}

	// If the Node doesn't have the correct owner reference, set it
	if !hasOwnerRef {
		// Set the owner reference using controllerutil.SetControllerReference
		if err := ctrl.SetControllerReference(group, node, r.Scheme); err != nil {
			logger.Error(err, "Failed to set controller reference", "node", node.Name, "group", groupName)
			return ctrl.Result{}, err
		}

		// Update the Node with the new owner reference
		if err := r.Update(ctx, node); err != nil {
			logger.Error(err, "Failed to update Node with owner reference", "node", node.Name)
			return ctrl.Result{}, err
		}

		logger.Info("Successfully set owner reference for Node to Group", "node", node.Name, "group", groupName)
	}

	// Create or update the healthcheck CronJob for this node
	if err := r.createOrUpdateHealthcheckCronJob(ctx, node, groupName); err != nil {
		logger.Error(err, "Failed to create or update healthcheck CronJob", "node", node.Name)
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled Node", "node", node.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Note: GroupStore should already be initialized from main.go
	// If it's nil, create a new one as fallback
	if r.GroupStore == nil {
		r.GroupStore = groupstore.NewGroupStore()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.Node{}).
		Named("node").
		Complete(r)
}

// generateShortCronJobName generates a shortened CronJob name that stays within Kubernetes limits
// while maintaining uniqueness through a hash suffix
func generateShortCronJobName(groupName, nodeName string) string {
	// Maximum length for Kubernetes resource names is 52 characters
	// We need to reserve space for: {prefix}-{hash}-healthcheck
	// Let's use: {truncatedGroup}-{truncatedNode}-{hash}-healthcheck
	// Reserve 12 chars for "-healthcheck", 9 chars for "-{hash}", and some buffer

	maxNameLength := 52 - 12 - 9 - 2 // 29 chars total for group + node names

	// Create a unique identifier by combining group and node names
	combined := fmt.Sprintf("%s-%s", groupName, nodeName)

	// Generate a short hash (8 characters) of the combined string
	hash := sha256.Sum256([]byte(combined))
	hashStr := fmt.Sprintf("%x", hash)[:8]

	// Truncate group and node names if they're too long
	truncatedGroup := groupName
	truncatedNode := nodeName

	// If the combined length is too long, truncate proportionally
	if len(groupName)+len(nodeName) > maxNameLength {
		// Split the available space proportionally
		totalLen := len(groupName) + len(nodeName)
		groupRatio := float64(len(groupName)) / float64(totalLen)
		nodeRatio := float64(len(nodeName)) / float64(totalLen)

		groupMaxLen := int(float64(maxNameLength) * groupRatio)
		nodeMaxLen := int(float64(maxNameLength) * nodeRatio)

		// Ensure we don't truncate to 0
		if groupMaxLen < 1 {
			groupMaxLen = 1
			nodeMaxLen = maxNameLength - 1
		}
		if nodeMaxLen < 1 {
			nodeMaxLen = 1
			groupMaxLen = maxNameLength - 1
		}

		if len(groupName) > groupMaxLen {
			truncatedGroup = groupName[:groupMaxLen]
		}
		if len(nodeName) > nodeMaxLen {
			truncatedNode = nodeName[:nodeMaxLen]
		}
	}

	return fmt.Sprintf("%s-%s-%s-healthcheck", truncatedGroup, truncatedNode, hashStr)
}

// extractNodeNameFromCronJob extracts the node name from a CronJob name
// This handles both the old format and the new hash-based format
func extractNodeNameFromCronJob(cronJobName string, cronJobLabels map[string]string) string {
	// First try to get the node name from labels (new approach)
	if nodeName, exists := cronJobLabels["node"]; exists {
		return nodeName
	}

	// Fallback to parsing the name for backward compatibility
	// Check if it ends with -healthcheck
	if !strings.HasSuffix(cronJobName, "-healthcheck") {
		return ""
	}

	// Remove the -healthcheck suffix
	nameWithoutSuffix := cronJobName[:len(cronJobName)-12]

	// Split by dash
	parts := strings.Split(nameWithoutSuffix, "-")

	// If we have a hash (8 chars) as the last part, it's the new format
	if len(parts) >= 3 && len(parts[len(parts)-1]) == 8 {
		// New format: {group}-{node}-{hash}
		// We need to find where the group ends and node begins
		// Since we don't know the exact group name, we use a heuristic:
		// The node name is everything before the hash, minus the first part (which is part of the group)
		if len(parts) >= 4 {
			// For cases like "test-group-test-node-hash", the node is "test-node"
			nodeParts := parts[2 : len(parts)-1]
			return strings.Join(nodeParts, "-")
		} else {
			// Simple case: "group-node-hash", the node is just "node"
			return parts[1]
		}
	}

	// Old format: {group}-{node}
	if len(parts) >= 2 {
		// For old format, we need to find where group ends and node begins
		// Since we don't know the exact group name, we use a heuristic:
		// The node name is everything after the first part
		if len(parts) >= 3 {
			// For cases like "test-group-test-node", the node is "test-node"
			nodeParts := parts[2:]
			return strings.Join(nodeParts, "-")
		} else {
			// Simple case: "group-node", the node is just "node"
			return parts[1]
		}
	}

	return ""
}

// generateHealthcheckCronJob creates a CronJob for healthchecking a specific node
func (r *NodeReconciler) generateHealthcheckCronJob(node *infrav1alpha1.Node, groupName string, scheme *runtime.Scheme) (*batchv1.CronJob, error) {
	// Convert healthcheck period to cron schedule format
	// Period is in seconds, convert to cron format: "*/{period} * * * *"
	cronSchedule := fmt.Sprintf("*/%d * * * *", node.Spec.HealthcheckPeriod)

	// Generate a shortened CronJob name
	cronJobName := generateShortCronJobName(groupName, node.Spec.KubernetesNodeName)

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronJobName,
			Namespace: node.Namespace,
			Labels: map[string]string{
				"app":        "homelab-autoscaler",
				"group":      groupName,
				"node":       node.Spec.KubernetesNodeName,
				"group-name": groupName,                    // Store full group name in label
				"node-name":  node.Spec.KubernetesNodeName, // Store full node name in label
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule: cronSchedule,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app":   "homelab-autoscaler",
								"group": groupName,
								"node":  node.Spec.KubernetesNodeName,
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							ServiceAccountName: func() string {
								if node.Spec.HealthcheckPodSpec.ServiceAccount != nil {
									return *node.Spec.HealthcheckPodSpec.ServiceAccount
								}
								return ""
							}(),
							Containers: []corev1.Container{
								{
									Name:    "healthcheck",
									Image:   node.Spec.HealthcheckPodSpec.Image,
									Command: node.Spec.HealthcheckPodSpec.Command,
									Args:    node.Spec.HealthcheckPodSpec.Args,
								},
							},
						},
					},
				},
			},
		},
	}

	// Set owner reference to the Node CR (not Group CR)
	if err := ctrl.SetControllerReference(node, cronJob, scheme); err != nil {
		return nil, err
	}

	return cronJob, nil
}

// createOrUpdateHealthcheckCronJob creates or updates a healthcheck CronJob for a specific node
func (r *NodeReconciler) createOrUpdateHealthcheckCronJob(ctx context.Context, node *infrav1alpha1.Node, groupName string) error {
	logger := log.FromContext(ctx)

	// Generate the desired CronJob
	desiredCronJob, err := r.generateHealthcheckCronJob(node, groupName, r.Scheme)
	if err != nil {
		logger.Error(err, "Failed to generate healthcheck CronJob", "node", node.Name)
		return err
	}

	// Try to get the existing CronJob
	existingCronJob := &batchv1.CronJob{}
	err = r.Get(ctx, types.NamespacedName{Name: desiredCronJob.Name, Namespace: desiredCronJob.Namespace}, existingCronJob)
	if err != nil {
		if errors.IsNotFound(err) {
			// Create the CronJob
			if err := r.Create(ctx, desiredCronJob); err != nil {
				logger.Error(err, "Failed to create healthcheck CronJob", "cronJob", desiredCronJob.Name)
				return err
			}
			logger.Info("Successfully created healthcheck CronJob", "cronJob", desiredCronJob.Name)
			return nil
		}
		logger.Error(err, "Failed to get existing CronJob", "cronJob", desiredCronJob.Name)
		return err
	}

	// Update the existing CronJob
	existingCronJob.Spec = desiredCronJob.Spec
	if err := r.Update(ctx, existingCronJob); err != nil {
		logger.Error(err, "Failed to update healthcheck CronJob", "cronJob", desiredCronJob.Name)
		return err
	}

	logger.Info("Successfully updated healthcheck CronJob", "cronJob", desiredCronJob.Name)
	return nil
}

// deleteHealthcheckCronJob deletes the healthcheck CronJob for a specific node
func (r *NodeReconciler) deleteHealthcheckCronJob(ctx context.Context, node *infrav1alpha1.Node, groupName string) error {
	logger := log.FromContext(ctx)

	// First try to find the CronJob using the new approach (by labels)
	cronJobList := &batchv1.CronJobList{}
	err := r.List(ctx, cronJobList,
		client.InNamespace(node.Namespace),
		client.MatchingLabels{
			"group": groupName,
			"node":  node.Spec.KubernetesNodeName,
		})
	if err != nil {
		logger.Error(err, "Failed to list CronJobs for deletion", "group", groupName, "node", node.Spec.KubernetesNodeName)
		return err
	}

	// If we found CronJobs with the new labels, delete them
	if len(cronJobList.Items) > 0 {
		for _, cronJob := range cronJobList.Items {
			if err := r.Delete(ctx, &cronJob); err != nil {
				logger.Error(err, "Failed to delete healthcheck CronJob", "cronJob", cronJob.Name)
				return err
			}
			logger.Info("Successfully deleted healthcheck CronJob", "cronJob", cronJob.Name)
		}
		return nil
	}

	// Fallback: try the old naming approach for backward compatibility
	cronJobName := fmt.Sprintf("%s-%s-healthcheck", groupName, node.Spec.KubernetesNodeName)
	cronJob := &batchv1.CronJob{}
	err = r.Get(ctx, types.NamespacedName{Name: cronJobName, Namespace: node.Namespace}, cronJob)
	if err != nil {
		if errors.IsNotFound(err) {
			// CronJob doesn't exist, nothing to delete
			logger.Info("Healthcheck CronJob not found, nothing to delete", "cronJob", cronJobName)
			return nil
		}
		logger.Error(err, "Failed to get healthcheck CronJob for deletion", "cronJob", cronJobName)
		return err
	}

	// Delete the CronJob
	if err := r.Delete(ctx, cronJob); err != nil {
		logger.Error(err, "Failed to delete healthcheck CronJob", "cronJob", cronJobName)
		return err
	}

	logger.Info("Successfully deleted healthcheck CronJob", "cronJob", cronJobName)
	return nil
}

// prependCondition prepends a condition to the node's conditions slice
func (r *NodeReconciler) prependCondition(node *infrav1alpha1.Node, newCondition metav1.Condition) {
	// Check if a condition of the same type already exists
	for i, existingCondition := range node.Status.Conditions {
		if existingCondition.Type == newCondition.Type {
			// Update the existing condition
			node.Status.Conditions[i] = newCondition
			return
		}
	}
	
	// If no condition of the same type exists, prepend the new condition
	node.Status.Conditions = append([]metav1.Condition{newCondition}, node.Status.Conditions...)
}

// updateNodeConditionProgressing updates the Node status condition to "Progressing" when scaling is initiated
func (r *NodeReconciler) updateNodeConditionProgressing(ctx context.Context, node *infrav1alpha1.Node, reason, message string) error {
	logger := log.FromContext(ctx)

	// Create or update the Progressing condition
	progressingCondition := metav1.Condition{
		Type:               "Progressing",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	// Update or add the condition manually since we don't have meta.SetStatusCondition
	r.prependCondition(node, progressingCondition)

	// Update the Node status in the cluster
	if err := r.Status().Update(ctx, node); err != nil {
		logger.Error(err, "Failed to update Node condition to Progressing", "node", node.Name)
		return err
	}

	logger.Info("Successfully updated Node condition to Progressing", "node", node.Name, "reason", reason, "message", message)
	return nil
}

// SetNodeConditionProgressingForScaling sets the Node condition to Progressing when scaling is initiated
func (r *NodeReconciler) SetNodeConditionProgressingForScaling(ctx context.Context, nodeName string) error {
	logger := log.FromContext(ctx)

	// Get the Node resource
	node := &infrav1alpha1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: "homelab-autoscaler-system"}, node); err != nil {
		logger.Error(err, "Failed to get Node for scaling condition update", "node", nodeName)
		return err
	}

	// Update the condition to Progressing for scaling
	return r.updateNodeConditionProgressing(ctx, node, "ScalingInitiated", "Node scaling operation initiated")
}

// SetNodeConditionTerminatingForShutdown sets the Node condition to Terminating when shutdown is initiated
func (r *NodeReconciler) SetNodeConditionTerminatingForShutdown(ctx context.Context, nodeName string) error {
	logger := log.FromContext(ctx)

	// Get the Node resource
	node := &infrav1alpha1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: "homelab-autoscaler-system"}, node); err != nil {
		logger.Error(err, "Failed to get Node for shutdown condition update", "node", nodeName)
		return err
	}

	// Create or update the Terminating condition
	terminatingCondition := metav1.Condition{
		Type:               "Terminating",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeGroupDeleteNodes",
		Message:            "Node shutdown operation initiated",
	}

	// Update or add the condition manually since we don't have meta.SetStatusCondition
	r.prependCondition(node, terminatingCondition)

	// Update the Node status in the cluster
	if err := r.Status().Update(ctx, node); err != nil {
		logger.Error(err, "Failed to update Node condition to Terminating", "node", node.Name)
		return err
	}

	logger.Info("Successfully updated Node condition to Terminating", "node", node.Name)
	return nil
}

// hasTerminatingCondition checks if the node has a Terminating condition with Status: "True"
func (r *NodeReconciler) hasTerminatingCondition(node *infrav1alpha1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Terminating" && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// validateNewNode validates that a new Node resource references an existing and schedulable Kubernetes node
func (r *NodeReconciler) validateNewNode(ctx context.Context, node *infrav1alpha1.Node) error {
	// This validation only runs when the node is not already in the GroupStore (i.e., it's a new node)
	if r.GroupStore == nil {
		return nil
	}

	_, err := r.GroupStore.GetNode(node.Name)
	if err != nil && strings.Contains(err.Error(), "not found") {
		// This is a new node, validate the referenced Kubernetes node
		return r.validateKubernetesNode(ctx, node, node.Name, node.Spec.KubernetesNodeName)
	}

	return nil
}

// validateKubernetesNode validates that the referenced Kubernetes node exists and is schedulable
func (r *NodeReconciler) validateKubernetesNode(ctx context.Context, node *infrav1alpha1.Node, nodeName, kubernetesNodeName string) error {
	logger := log.FromContext(ctx)

	// Get the referenced Kubernetes node
	kubernetesNode := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: kubernetesNodeName}, kubernetesNode); err != nil {
		if errors.IsNotFound(err) {
			logger.Error(err, "Referenced Kubernetes node does not exist",
				"node", nodeName, "kubernetesNodeName", kubernetesNodeName)
			// Set error condition on the Node CR
			errorCondition := metav1.Condition{
				Type:               "Error",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "InvalidKubernetesNode",
				Message:            "invalid kubernetesNodeName",
			}
			r.prependCondition(node, errorCondition)
			return fmt.Errorf("referenced Kubernetes node %q does not exist", kubernetesNodeName)
		}
		logger.Error(err, "Failed to get referenced Kubernetes node",
			"node", nodeName, "kubernetesNodeName", kubernetesNodeName)
		return err
	}

	// Check if the Kubernetes node is schedulable
	if kubernetesNode.Spec.Unschedulable {
		logger.Error(nil, "Referenced Kubernetes node is unschedulable",
			"node", nodeName, "kubernetesNodeName", kubernetesNodeName)
		// Set error condition on the Node CR
		errorCondition := metav1.Condition{
			Type:               "Error",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "InvalidKubernetesNode",
			Message:            "invalid kubernetesNodeName",
		}
		r.prependCondition(node, errorCondition)
		return fmt.Errorf("referenced Kubernetes node %q is unschedulable", kubernetesNodeName)
	}

	logger.Info("Successfully validated referenced Kubernetes node",
		"node", nodeName, "kubernetesNodeName", kubernetesNodeName)
	return nil
}

// shouldSkipStatusOnlyUpdate checks if this is a status-only update that should be skipped
func (r *NodeReconciler) shouldSkipStatusOnlyUpdate(ctx context.Context, node *infrav1alpha1.Node) (bool, error) {
	logger := log.FromContext(ctx)

	// Check if this is a status-only update by comparing Generation
	// If Generation hasn't changed, only status fields were updated
	// However, we should still reconcile if the GroupStore health status has changed
	if r.GroupStore != nil {
		storedNode, err := r.GroupStore.GetNode(node.Name)
		if err == nil && storedNode != nil && node.Generation == storedNode.Generation {
			// Check if the health status in GroupStore has changed
			groupName, hasGroupLabel := node.Labels["group"]
			if hasGroupLabel {
				groupStoreHealth, exists := r.GroupStore.GetNodeHealthcheckStatus(groupName, node.Spec.KubernetesNodeName)
				if exists && groupStoreHealth != node.Status.Health {
					// Health status has changed in GroupStore, allow reconciliation
					logger.Info("Allowing reconciliation due to GroupStore health status change", "node", node.Name, "oldHealth", node.Status.Health, "newHealth", groupStoreHealth)
				} else {
					// This is a status-only update with no health status change, skip reconciliation
					logger.Info("Skipping reconciliation for status-only update", "node", node.Name, "generation", node.Generation)
					return true, nil
				}
			} else {
				// No group label, skip reconciliation
				logger.Info("Skipping reconciliation for status-only update (no group label)", "node", node.Name, "generation", node.Generation)
				return true, nil
			}
		}
	}
	return false, nil
}

// handleNodeDeletion handles the deletion of a node
func (r *NodeReconciler) handleNodeDeletion(ctx context.Context, node *infrav1alpha1.Node, groupName, nodeName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Node is being deleted", "node", nodeName)
	// Remove from groupstore
	if r.GroupStore != nil {
		if err := r.GroupStore.RemoveNode(nodeName); err != nil {
			logger.Error(err, "Failed to remove node from groupstore during deletion", "node", nodeName)
			return ctrl.Result{}, err
		}
	}
	// Delete associated CronJob
	if err := r.deleteHealthcheckCronJob(ctx, node, groupName); err != nil {
		logger.Error(err, "Failed to delete healthcheck CronJob", "node", nodeName)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
