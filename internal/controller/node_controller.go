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
			logger.Info("Node not found", "node", req.Name)
			return ctrl.Result{}, nil
		}
		// Error reading the object - return with requeue
		logger.Error(err, "Failed to get Node", "node", req.Name)
		return ctrl.Result{}, err
	}

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
		logger.Info("Node is being deleted", "node", req.Name)
		// Delete associated CronJob
		if err := r.deleteHealthcheckCronJob(ctx, node, groupName); err != nil {
			logger.Error(err, "Failed to delete healthcheck CronJob", "node", req.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
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

	// Start the healthcheck status sync goroutine
	// Pass a context that will be cancelled when the manager stops
	// to avoid the "close of closed channel" panic from duplicate signal handler setup
	ctx := context.Background()
	go r.syncHealthcheckStatuses(ctx)

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.Node{}).
		Named("node").
		Complete(r)
}

// generateHealthcheckCronJob creates a CronJob for healthchecking a specific node
func (r *NodeReconciler) generateHealthcheckCronJob(node *infrav1alpha1.Node, groupName string) *batchv1.CronJob {
	// Convert healthcheck period to cron schedule format
	// Period is in seconds, convert to cron format: "*/{period} * * * *"
	cronSchedule := fmt.Sprintf("*/%d * * * *", node.Spec.HealthcheckPeriod)

	// Create the CronJob name using the pattern: {groupName}-{kubernetesNodeName}-healthcheck
	cronJobName := fmt.Sprintf("%s-%s-healthcheck", groupName, node.Spec.KubernetesNodeName)

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronJobName,
			Namespace: node.Namespace,
			Labels: map[string]string{
				"app":   "homelab-autoscaler",
				"group": groupName,
				"node":  node.Spec.KubernetesNodeName,
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
}

// createOrUpdateHealthcheckCronJob creates or updates a healthcheck CronJob for a specific node
func (r *NodeReconciler) createOrUpdateHealthcheckCronJob(ctx context.Context, node *infrav1alpha1.Node, groupName string) error {
	logger := log.FromContext(ctx)

	// Generate the desired CronJob
	desiredCronJob := r.generateHealthcheckCronJob(node, groupName)

	// Set owner reference to the Node CR (not Group CR)
	if err := ctrl.SetControllerReference(node, desiredCronJob, r.Scheme); err != nil {
		logger.Error(err, "Failed to set controller reference for CronJob", "cronJob", desiredCronJob.Name)
		return err
	}

	// Try to get the existing CronJob
	existingCronJob := &batchv1.CronJob{}
	err := r.Get(ctx, types.NamespacedName{Name: desiredCronJob.Name, Namespace: desiredCronJob.Namespace}, existingCronJob)
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

	// Construct the CronJob name
	cronJobName := fmt.Sprintf("%s-%s-healthcheck", groupName, node.Spec.KubernetesNodeName)

	// Try to get the CronJob
	cronJob := &batchv1.CronJob{}
	err := r.Get(ctx, types.NamespacedName{Name: cronJobName, Namespace: node.Namespace}, cronJob)
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

// getHealthcheckStatus determines the healthcheck status from CronJob status
func (r *NodeReconciler) getHealthcheckStatus(cronJob *batchv1.CronJob) string {
	if cronJob == nil {
		return "Unknown"
	}

	// Check if the CronJob has been scheduled
	if cronJob.Status.LastScheduleTime == nil {
		return "NotScheduled"
	}

	// Check if there are any active jobs
	if len(cronJob.Status.Active) > 0 {
		return HealthcheckStatusRunning
	}

	// Check for failed jobs in the last schedule
	// If there are recent failures, mark as offline
	if cronJob.Status.LastSuccessfulTime == nil {
		// If never successful and was scheduled, it might have failed
		// Check if enough time has passed since last schedule to consider it failed
		timeSinceLastSchedule := time.Since(cronJob.Status.LastScheduleTime.Time)
		if cronJob.Spec.StartingDeadlineSeconds != nil && timeSinceLastSchedule > time.Duration(*cronJob.Spec.StartingDeadlineSeconds)*time.Second {
			return HealthcheckStatusFailed
		}
		// If no deadline set or still within deadline, consider it running
		return HealthcheckStatusRunning
	}

	// Check if the last successful time is recent enough
	timeSinceLastSuccess := time.Since(cronJob.Status.LastSuccessfulTime.Time)

	// Parse the schedule to determine the expected interval (simplified)
	// For now, assume a reasonable default based on typical healthcheck periods
	if timeSinceLastSuccess > 5*time.Minute {
		return HealthcheckStatusFailed // Too long since last success
	}

	return HealthcheckStatusHealthy
}

// listHealthcheckCronJobsForNode lists all CronJobs for a specific node
func (r *NodeReconciler) listHealthcheckCronJobsForNode(ctx context.Context, nodeName string) (*batchv1.CronJob, error) {
	logger := log.FromContext(ctx)

	var cronJobList batchv1.CronJobList
	if err := r.List(ctx, &cronJobList, client.InNamespace("")); err != nil {
		logger.Error(err, "Failed to list CronJobs")
		return nil, err
	}

	for i := range cronJobList.Items {
		cronJob := &cronJobList.Items[i]
		// Check if the CronJob name follows the pattern *-{nodename}-healthcheck
		if len(cronJob.Name) > 12 && cronJob.Name[len(cronJob.Name)-12:] == "-healthcheck" {
			// Extract the node name from the cronjob name
			// Format: {groupname}-{nodename}-healthcheck
			parts := strings.Split(cronJob.Name, "-")
			if len(parts) >= 3 && parts[len(parts)-2] == nodeName {
				return cronJob, nil
			}
		}
	}

	return nil, nil // Not found
}

// updateNodeHealthStatus updates the health status of a node based on its CronJob status
func (r *NodeReconciler) updateNodeHealthStatus(ctx context.Context, node *infrav1alpha1.Node, groupName string) error {
	logger := log.FromContext(ctx)

	// Get the CronJob for this node
	cronJob, err := r.listHealthcheckCronJobsForNode(ctx, node.Spec.KubernetesNodeName)
	if err != nil {
		logger.Error(err, "Failed to get CronJob for node", "node", node.Spec.KubernetesNodeName)
		return err
	}

	if cronJob == nil {
		// No CronJob found, set status to unknown
		node.Status.Health = HealthcheckStatusUnknown
	} else {
		// Get the healthcheck status from the CronJob
		healthcheckStatus := r.getHealthcheckStatus(cronJob)

		// Convert healthcheck status to lowercase for consistency with CRD enum values
		var nodeHealthStatus string
		switch healthcheckStatus {
		case "Healthy":
			nodeHealthStatus = "healthy"
		case "Failed":
			nodeHealthStatus = "offline"
		case HealthcheckStatusRunning, HealthcheckStatusNotScheduled:
			nodeHealthStatus = HealthcheckStatusUnknown
		default:
			nodeHealthStatus = HealthcheckStatusUnknown
		}

		node.Status.Health = nodeHealthStatus
	}

	// Update the Node status in the cluster
	if err := r.Status().Update(ctx, node); err != nil {
		logger.Error(err, "Failed to update Node health status", "node", node.Name)
		return err
	}

	// Also update the groupstore to keep it synchronized
	if cronJob != nil {
		r.GroupStore.SetNodeHealthcheckStatus(groupName, node.Spec.KubernetesNodeName, node.Status.Health)
		logger.Info("Updated node healthcheck status in groupstore", "group", groupName, "node", node.Spec.KubernetesNodeName, "status", node.Status.Health)
	}

	return nil
}

// syncHealthcheckStatuses periodically checks status of all healthcheck CronJobs and updates node status
func (r *NodeReconciler) syncHealthcheckStatuses(ctx context.Context) {
	logger := log.FromContext(ctx)

	// Create a ticker that runs every 10 seconds for more responsive health checks
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Run immediately on startup
	logger.Info("Starting initial healthcheck status sync")
	if err := r.updateAllNodeHealthStatuses(ctx); err != nil {
		logger.Error(err, "Failed to update node health statuses during initial sync")
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("Stopping healthcheck status sync")
			return
		case <-ticker.C:
			logger.Info("Starting periodic healthcheck status sync")

			if err := r.updateAllNodeHealthStatuses(ctx); err != nil {
				logger.Error(err, "Failed to update node health statuses during periodic sync")
			}

			logger.Info("Completed healthcheck status sync")
		}
	}
}

// updateAllNodeHealthStatuses updates health status for all nodes
func (r *NodeReconciler) updateAllNodeHealthStatuses(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// List all Node CRs
	nodeList := &infrav1alpha1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		logger.Error(err, "Failed to list nodes")
		return err
	}

	for _, node := range nodeList.Items {
		// Check if the node has a group label
		groupName, hasGroupLabel := node.Labels["group"]
		if !hasGroupLabel {
			continue
		}

		// Update the health status for this node
		if err := r.updateNodeHealthStatus(ctx, &node, groupName); err != nil {
			logger.Error(err, "Failed to update node health status", "node", node.Name)
			// Continue with other nodes even if one fails
		}
	}

	return nil
}
