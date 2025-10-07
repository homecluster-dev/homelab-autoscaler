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
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
)

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	GroupStore *groupstore.GroupStore
}

// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the CronJob resource from the request
	cronJob := &batchv1.CronJob{}
	if err := r.Get(ctx, req.NamespacedName, cronJob); err != nil {
		// The object was not found, return early
		if errors.IsNotFound(err) {
			logger.Info("CronJob not found, skipping reconciliation", "cronJob", req.Name)
			return ctrl.Result{}, nil
		}
		// Error reading the object - return with requeue
		logger.Error(err, "Failed to get CronJob", "cronJob", req.Name)
		return ctrl.Result{}, err
	}

	// Extract group and node information from the CronJob
	groupName, nodeName, err := r.extractGroupAndNodeNames(cronJob)
	if err != nil {
		logger.Info("Skipping CronJob reconciliation - not a healthcheck CronJob", "cronJob", req.Name, "error", err)
		return ctrl.Result{}, nil
	}

	logger.Info("Processing healthcheck CronJob", "cronJob", req.Name, "group", groupName, "node", nodeName)

	// Calculate health status based on CronJob status
	healthStatus := r.calculateHealthStatus(cronJob)
	logger.Info("Calculated health status", "cronJob", req.Name, "health", healthStatus)

	// Update the GroupStore with the health status
	if r.GroupStore != nil {
		r.GroupStore.SetNodeHealthcheckStatus(groupName, nodeName, healthStatus)
		logger.Info("Updated node healthcheck status in groupstore", "group", groupName, "node", nodeName, "status", healthStatus)
	}

	// Trigger reconciliation of the corresponding Node CR to update its status
	if err := r.triggerNodeReconciliation(ctx, groupName, nodeName); err != nil {
		logger.Error(err, "Failed to trigger Node reconciliation", "group", groupName, "node", nodeName)
		// Don't fail the CronJob reconciliation if Node reconciliation fails
	}

	// Update the Node health status from GroupStore
	if err := r.updateNodeHealthStatusFromGroupStore(ctx, groupName, nodeName); err != nil {
		logger.Error(err, "Failed to update Node health status from GroupStore", "group", groupName, "node", nodeName)
		// Don't fail the CronJob reconciliation if Node health status update fails
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Note: GroupStore should already be initialized from main.go
	// If it's nil, create a new one as fallback
	if r.GroupStore == nil {
		r.GroupStore = groupstore.NewGroupStore()
	}

	// Create a predicate to filter CronJobs by label
	labelPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		labels := obj.GetLabels()
		if labels == nil {
			return false
		}
		// Only reconcile CronJobs with the "app": "homelab-autoscaler" label
		return labels["app"] == "homelab-autoscaler"
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.CronJob{}).
		WithEventFilter(labelPredicate).
		Named("cronjob").
		Complete(r)
}

// extractGroupAndNodeNames extracts group and node names from CronJob labels or name
func (r *CronJobReconciler) extractGroupAndNodeNames(cronJob *batchv1.CronJob) (string, string, error) {
	if cronJob == nil {
		return "", "", fmt.Errorf("cronJob cannot be nil")
	}

	// First try to get the group and node names from labels (new approach)
	if groupName, exists := cronJob.Labels["group"]; exists {
		if nodeName, exists := cronJob.Labels["node"]; exists {
			return groupName, nodeName, nil
		}
	}

	// Fallback to parsing the name for backward compatibility
	// Use the same logic as the existing extractNodeNameFromCronJob function
	nodeName := extractNodeNameFromCronJob(cronJob.Name, cronJob.Labels)
	if nodeName == "" {
		return "", "", fmt.Errorf("could not extract node name from CronJob")
	}

	// Try to extract group name from the CronJob name
	// For format like "{group}-{node}-healthcheck" or "{group}-{node}-{hash}-healthcheck"
	if strings.HasSuffix(cronJob.Name, "-healthcheck") {
		nameWithoutSuffix := cronJob.Name[:len(cronJob.Name)-12]
		parts := strings.Split(nameWithoutSuffix, "-")

		if len(parts) >= 2 {
			// Check if we have a hash (8 chars) as the last part (new format)
			if len(parts) >= 3 && len(parts[len(parts)-1]) == 8 {
				// New format: {group}-{node}-{hash}
				// The group is everything before the node parts
				if len(parts) >= 4 {
					// For cases like "test-group-test-node-hash", the group is "test-group"
					groupParts := parts[:len(parts)-2]
					return strings.Join(groupParts, "-"), nodeName, nil
				} else {
					// Simple case: "group-node-hash", the group is just "group"
					return parts[0], nodeName, nil
				}
			} else {
				// Old format: {group}-{node}
				// The group is everything before the node parts
				if len(parts) >= 3 {
					// For cases like "test-group-test-node", the group is "test-group"
					groupParts := parts[:len(parts)-1]
					return strings.Join(groupParts, "-"), nodeName, nil
				} else {
					// Simple case: "group-node", the group is just "group"
					return parts[0], nodeName, nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("could not extract group name from CronJob name")
}

// calculateHealthStatus determines the healthcheck status from CronJob status
func (r *CronJobReconciler) calculateHealthStatus(cronJob *batchv1.CronJob) string {
	if cronJob == nil {
		return "unknown"
	}

	// Map CronJob status to node health status
	// Based on the requirements:
	// "Healthy" → "healthy"
	// "Failed" → "offline"
	// "Running"/"NotScheduled" → "unknown"

	// Check if there are active jobs (Running)
	if len(cronJob.Status.Active) > 0 {
		return "unknown"
	}

	// If there's no last schedule time, it's not scheduled yet
	if cronJob.Status.LastScheduleTime == nil {
		return "unknown"
	}

	// If there's a last successful time, check if it's recent enough
	if cronJob.Status.LastSuccessfulTime != nil {
		timeSinceLastSuccess := time.Since(cronJob.Status.LastSuccessfulTime.Time)
		// If the last success was recent enough, it's healthy
		if timeSinceLastSuccess <= 5*time.Minute {
			return "healthy"
		}
	}

	// If we got here, it means either:
	// 1. There was never a successful run, or
	// 2. The last successful run was too long ago
	// In either case, we consider it failed/offline
	return "offline"
}

// triggerNodeReconciliation triggers reconciliation of the corresponding Node CR
func (r *CronJobReconciler) triggerNodeReconciliation(ctx context.Context, groupName, nodeName string) error {
	logger := log.FromContext(ctx)

	// List all Node CRs with the group label and matching kubernetes node name
	nodeList := &infrav1alpha1.NodeList{}
	if err := r.List(ctx, nodeList,
		client.MatchingLabels{
			"group": groupName,
		}); err != nil {
		logger.Error(err, "Failed to list Nodes for group", "group", groupName)
		return err
	}

	// Find the Node CR that matches the kubernetes node name
	for _, node := range nodeList.Items {
		if node.Spec.KubernetesNodeName == nodeName {
			// Trigger reconciliation by updating the Node status
			// This will cause the Node controller to reconcile and update the health status
			logger.Info("Triggering Node reconciliation", "node", node.Name, "group", groupName, "kubernetesNode", nodeName)

			// Get the current Node to ensure we have the latest version
			currentNode := &infrav1alpha1.Node{}
			if err := r.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, currentNode); err != nil {
				if errors.IsNotFound(err) {
					logger.Info("Node not found, skipping reconciliation trigger", "node", node.Name)
					continue
				}
				logger.Error(err, "Failed to get Node for reconciliation trigger", "node", node.Name)
				continue
			}

			// Update the status to trigger reconciliation
			// We don't need to change the health status here, just trigger the reconciliation
			// The Node controller will pick up the updated GroupStore health status
			if err := r.Status().Update(ctx, currentNode); err != nil {
				logger.Error(err, "Failed to update Node status to trigger reconciliation", "node", node.Name)
				continue
			}

			logger.Info("Successfully triggered Node reconciliation", "node", node.Name)
			return nil
		}
	}

	logger.Info("No matching Node CR found for reconciliation trigger", "group", groupName, "kubernetesNode", nodeName)
	return nil
}

// updateNodeHealthStatusFromGroupStore updates the health status of a node based on GroupStore data
func (r *CronJobReconciler) updateNodeHealthStatusFromGroupStore(ctx context.Context, groupName, nodeName string) error {
	logger := log.FromContext(ctx)

	// List all Node CRs with the group label and matching kubernetes node name
	nodeList := &infrav1alpha1.NodeList{}
	if err := r.List(ctx, nodeList,
		client.MatchingLabels{
			"group": groupName,
		}); err != nil {
		logger.Error(err, "Failed to list Nodes for group", "group", groupName)
		return err
	}

	// Find the Node CR that matches the kubernetes node name
	for _, node := range nodeList.Items {
		if node.Spec.KubernetesNodeName == nodeName {
			// Get the current Node to ensure we have the latest version
			currentNode := &infrav1alpha1.Node{}
			if err := r.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, currentNode); err != nil {
				if errors.IsNotFound(err) {
					logger.Info("Node not found, skipping health status update", "node", node.Name)
					continue
				}
				logger.Error(err, "Failed to get Node for health status update", "node", node.Name)
				continue
			}

			// Store the previous health status to detect changes
			previousHealthStatus := currentNode.Status.Health

			// Get the health status from GroupStore
			groupStoreHealthStatus := ""
			if r.GroupStore != nil {
				if status, exists := r.GroupStore.GetNodeHealthcheckStatus(groupName, nodeName); exists {
					groupStoreHealthStatus = status
				}
			}

			// If no health status in GroupStore, default to unknown
			if groupStoreHealthStatus == "" {
				groupStoreHealthStatus = HealthcheckStatusUnknown
			}

			// Update the node health status
			currentNode.Status.Health = groupStoreHealthStatus

			// Set conditions based on health status
			// Check if the health status is "unknown" (scaling scenario)
			if currentNode.Status.Health == "unknown" {
				// Set the Progressing condition when node is scaling (unknown health)
				progressingCondition := metav1.Condition{
					Type:               "Progressing",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "NodeScaling",
					Message:            "Node is being scaled",
				}
				r.prependCondition(currentNode, progressingCondition)
				logger.Info("Set Node condition to Progressing", "node", currentNode.Name, "health", currentNode.Status.Health)
			} else {
				// Clear the Progressing condition when health is no longer unknown
				progressingCondition := metav1.Condition{
					Type:               "Progressing",
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.Now(),
					Reason:             "NodeStable",
					Message:            "Node is no longer scaling",
				}
				r.prependCondition(currentNode, progressingCondition)
			}

			// Check if the health status changed to "healthy"
			if previousHealthStatus != currentNode.Status.Health && currentNode.Status.Health == HealthStatusHealthy {
				// Set the Available condition when node becomes healthy
				availableCondition := metav1.Condition{
					Type:               "Available",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "NodeHealthy",
					Message:            "Node is healthy and available",
				}
				r.prependCondition(currentNode, availableCondition)
				logger.Info("Set Node condition to Available", "node", currentNode.Name, "health", currentNode.Status.Health)
			} else if currentNode.Status.Health != HealthStatusHealthy {
				// Clear the Available condition when health is not healthy
				availableCondition := metav1.Condition{
					Type:               "Available",
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.Now(),
					Reason:             "NodeUnhealthy",
					Message:            fmt.Sprintf("Node is %s", currentNode.Status.Health),
				}
				r.prependCondition(currentNode, availableCondition)
			}

			// Update the Node status in the cluster
			if err := r.Status().Update(ctx, currentNode); err != nil {
				logger.Error(err, "Failed to update Node health status", "node", currentNode.Name)
				return err
			}

			logger.Info("Successfully updated Node health status from GroupStore", "node", currentNode.Name, "health", currentNode.Status.Health)
			return nil
		}
	}

	logger.Info("No matching Node CR found for health status update", "group", groupName, "kubernetesNode", nodeName)
	return nil
}

// prependCondition prepends a condition to the node's conditions slice
func (r *CronJobReconciler) prependCondition(node *infrav1alpha1.Node, newCondition metav1.Condition) {
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
