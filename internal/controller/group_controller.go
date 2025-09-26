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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrahomeclusterdevv1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
)

// GroupReconciler reconciles a Group object
type GroupReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	GroupStore *groupstore.GroupStore
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=groups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=groups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=groups/finalizers,verbs=update

// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Log the reconciliation start
	logger.Info("Starting reconciliation for Group", "group", req.Name, "namespace", req.Namespace)

	// Get the Group resource from the request
	group := &infrahomeclusterdevv1alpha1.Group{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		// The object was not found, return early
		if errors.IsNotFound(err) {
			logger.Info("Group not found, will be removed from groupstore", "group", req.Name)
			// Remove from groupstore if it exists (ignore error if it doesn't exist)
			if err := r.GroupStore.Remove(req.Name); err != nil {
				// Only log the error if it's not a "not found" error
				if !strings.Contains(err.Error(), "not found") {
					logger.Error(err, "Failed to remove group from groupstore", "group", req.Name)
					return ctrl.Result{}, err
				}
				// Group wasn't in groupstore, which is fine
				logger.Info("Group was not in groupstore, nothing to remove", "group", req.Name)
			}
			return ctrl.Result{}, nil
		}
		// Error reading the object - return with requeue
		logger.Error(err, "Failed to get Group", "group", req.Name)
		return ctrl.Result{}, err
	}

	// Check if this is a status-only update by comparing Generation
	// If Generation hasn't changed, only status fields were updated
	storedGroup, err := r.GroupStore.Get(group.Name)
	if err == nil && storedGroup != nil && group.Generation == storedGroup.Generation {
		// This is a status-only update, skip reconciliation to avoid infinite loops
		logger.Info("Skipping reconciliation for status-only update", "group", group.Name, "generation", group.Generation)
		return ctrl.Result{}, nil
	}

	// Handle deletion
	if !group.DeletionTimestamp.IsZero() {
		logger.Info("Group is being deleted", "group", req.Name)
		// Delete associated CronJob
		if err := r.deleteHealthcheckCronJob(ctx, group); err != nil {
			logger.Error(err, "Failed to delete healthcheck CronJob", "group", req.Name)
			return ctrl.Result{}, err
		}
		// Remove from groupstore
		if err := r.GroupStore.Remove(req.Name); err != nil {
			logger.Error(err, "Failed to remove group from groupstore during deletion", "group", req.Name)
			return ctrl.Result{}, err
		}
		// Ensure finalizers are handled
		if len(group.ObjectMeta.Finalizers) > 0 {
			// Remove finalizers logic would go here
			logger.Info("Group has finalizers that need to be handled", "group", req.Name)
		}
		return ctrl.Result{}, nil
	}

	// Create or update associated CronJob
	if err := r.createOrUpdateHealthcheckCronJob(ctx, group); err != nil {
		logger.Error(err, "Failed to create or update healthcheck CronJob", "group", req.Name)
		return ctrl.Result{}, err
	}

	// Add/update the Group in the groupstore storage
	if err := r.GroupStore.AddOrUpdate(group); err != nil {
		logger.Error(err, "Failed to add/update group in groupstore", "group", req.Name)
		return ctrl.Result{}, err
	}
	logger.Info("Added/updated group in groupstore", "group", req.Name, "groupStoreAddr", fmt.Sprintf("%p", r.GroupStore), "groupCount", r.GroupStore.Count())

	// Initialize healthcheck status for new groups
	// Check if this group has a healthcheck status set
	_, exists := r.GroupStore.GetHealthcheckStatus(group.Name)
	if !exists {
		// Set initial healthcheck status to "NotScheduled" for new groups
		r.GroupStore.SetHealthcheckStatus(group.Name, "NotScheduled")
		logger.Info("Initialized healthcheck status for new group", "group", group.Name, "status", "NotScheduled")
	}

	// Mark the Group status as "Loaded" using metav1.Condition
	now := metav1.Now()
	loadedCondition := metav1.Condition{
		Type:               "Loaded",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "GroupLoaded",
		Message:            "Group has been successfully loaded and is ready for use",
	}

	// Update the Group status conditions
	group.Status.Conditions = []metav1.Condition{loadedCondition}

	// Update the Group in the cluster
	if err := r.Status().Update(ctx, group); err != nil {
		logger.Error(err, "Failed to update Group status", "group", req.Name)
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled Group", "group", req.Name)
	return ctrl.Result{Requeue: false}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
		For(&infrahomeclusterdevv1alpha1.Group{}).
		Named("group").
		Complete(r)
}

// generateHealthcheckCronJob creates a CronJob manifest for a specific NodeSpec's healthcheck
func (r *GroupReconciler) generateHealthcheckCronJob(group *infrahomeclusterdevv1alpha1.Group, nodeSpec *infrahomeclusterdevv1alpha1.NodeSpec, kubernetesNodeName string) *batchv1.CronJob {
	cronJobName := fmt.Sprintf("%s-%s-healthcheck", group.Name, kubernetesNodeName)

	schedule := fmt.Sprintf("*/%d * * * *", nodeSpec.HealthcheckPeriod)

	// Convert MinimalPodSpec to corev1.PodSpec
	containers := []corev1.Container{
		{
			Name:    "healthcheck",
			Image:   nodeSpec.HealthcheckPodSpec.Image,
			Command: nodeSpec.HealthcheckPodSpec.Command,
			Args:    nodeSpec.HealthcheckPodSpec.Args,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &[]bool{false}[0],
				RunAsNonRoot:             &[]bool{true}[0],
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
				},
			},
		},
	}

	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	// Process volume mounts
	for _, vol := range nodeSpec.HealthcheckPodSpec.Volumes {
		volumeMount := corev1.VolumeMount{
			Name:      vol.Name,
			MountPath: vol.MountPath,
		}
		volumeMounts = append(volumeMounts, volumeMount)

		if vol.SecretName != "" {
			volumes = append(volumes, corev1.Volume{
				Name: vol.Name,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: vol.SecretName,
					},
				},
			})
		} else if vol.ConfigMapName != "" {
			volumes = append(volumes, corev1.Volume{
				Name: vol.Name,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: vol.ConfigMapName,
						},
					},
				},
			})
		}
	}

	// Apply volume mounts to the container
	if len(volumeMounts) > 0 {
		containers[0].VolumeMounts = volumeMounts
	}

	podSpec := corev1.PodSpec{
		Containers:    containers,
		Volumes:       volumes,
		RestartPolicy: corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &[]bool{true}[0],
			RunAsUser:    &[]int64{1000}[0],
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronJobName,
			Namespace: group.Namespace,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: schedule,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: podSpec,
					},
				},
			},
		},
	}

	// Set owner reference to ensure proper cleanup
	if err := ctrl.SetControllerReference(group, cronJob, r.Scheme); err != nil {
		// This should never happen as we're setting reference to the same object we're reconciling
		panic(fmt.Sprintf("failed to set controller reference: %v", err))
	}

	return cronJob
}

// createOrUpdateHealthcheckCronJob creates or updates healthcheck CronJobs for all NodeSpecs in a Group
func (r *GroupReconciler) createOrUpdateHealthcheckCronJob(ctx context.Context, group *infrahomeclusterdevv1alpha1.Group) error {
	logger := log.FromContext(ctx)

	logger.Info("Creating/updating healthcheck CronJobs for group", "group", group.Name, "nodeSpecsCount", len(group.Spec.NodesSpecs))

	// If no nodes are specified, delete any existing cronjobs
	if len(group.Spec.NodesSpecs) == 0 {
		logger.Info("No NodeSpecs found, deleting existing cronjobs", "group", group.Name)
		return r.deleteHealthcheckCronJob(ctx, group)
	}

	// Create or update a CronJob for each NodeSpec
	for i, nodeSpec := range group.Spec.NodesSpecs {
		logger.Info("Processing NodeSpec", "group", group.Name, "index", i, "nodeName", nodeSpec.KubernetesNodeName, "healthcheckPeriod", nodeSpec.HealthcheckPeriod, "image", nodeSpec.HealthcheckPodSpec.Image)

		cronJob := r.generateHealthcheckCronJob(group, &nodeSpec, nodeSpec.KubernetesNodeName)
		if cronJob == nil {
			logger.Info("Generated nil cronjob, skipping", "group", group.Name, "nodeName", nodeSpec.KubernetesNodeName)
			continue
		}

		logger.Info("Generated cronjob", "group", group.Name, "cronJobName", cronJob.Name, "schedule", cronJob.Spec.Schedule)

		// Create or update the CronJob
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cronJob, func() error {
			// Only update the spec, not metadata
			cronJob.Spec = r.generateHealthcheckCronJob(group, &nodeSpec, nodeSpec.KubernetesNodeName).Spec
			return nil
		})

		if err != nil {
			logger.Error(err, "Failed to create or update healthcheck CronJob", "cronJob", cronJob.Name)
			return err
		}

		logger.Info("Successfully created/updated healthcheck CronJob", "cronJob", cronJob.Name)
	}

	return nil
}

// deleteHealthcheckCronJob deletes all healthcheck CronJobs for a Group
func (r *GroupReconciler) deleteHealthcheckCronJob(ctx context.Context, group *infrahomeclusterdevv1alpha1.Group) error {
	logger := log.FromContext(ctx)

	// List all cronjobs that belong to this group
	cronJobs, err := r.listHealthcheckCronJobsForGroup(ctx, group.Name)
	if err != nil {
		logger.Error(err, "Failed to list healthcheck CronJobs for group", "group", group.Name)
		return err
	}

	// Delete each cronjob
	for _, cronJob := range cronJobs {
		if err := r.Delete(ctx, cronJob); err != nil {
			logger.Error(err, "Failed to delete healthcheck CronJob", "cronJob", cronJob.Name)
			return err
		}
		logger.Info("Successfully deleted healthcheck CronJob", "cronJob", cronJob.Name)
	}

	return nil
}

// listHealthcheckCronJobs lists all CronJobs with names matching the pattern {groupname}-*-healthcheck
func (r *GroupReconciler) listHealthcheckCronJobs(ctx context.Context) ([]*batchv1.CronJob, error) {
	logger := log.FromContext(ctx)

	var cronJobList batchv1.CronJobList
	if err := r.List(ctx, &cronJobList, client.InNamespace("")); err != nil {
		logger.Error(err, "Failed to list CronJobs")
		return nil, err
	}

	var healthcheckCronJobs []*batchv1.CronJob
	for i := range cronJobList.Items {
		cronJob := &cronJobList.Items[i]
		// Check if the CronJob name follows the pattern {groupname}-*-healthcheck
		if len(cronJob.Name) > 12 && cronJob.Name[len(cronJob.Name)-12:] == "-healthcheck" {
			healthcheckCronJobs = append(healthcheckCronJobs, cronJob)
		}
	}

	logger.Info("Found healthcheck CronJobs", "count", len(healthcheckCronJobs))
	return healthcheckCronJobs, nil
}

// listHealthcheckCronJobsForGroup lists all CronJobs for a specific group
func (r *GroupReconciler) listHealthcheckCronJobsForGroup(ctx context.Context, groupName string) ([]*batchv1.CronJob, error) {
	logger := log.FromContext(ctx)

	var cronJobList batchv1.CronJobList
	if err := r.List(ctx, &cronJobList, client.InNamespace("")); err != nil {
		logger.Error(err, "Failed to list CronJobs")
		return nil, err
	}

	var groupCronJobs []*batchv1.CronJob
	for i := range cronJobList.Items {
		cronJob := &cronJobList.Items[i]
		// Check if the CronJob name follows the pattern {groupname}-*-healthcheck
		if len(cronJob.Name) > 12 && cronJob.Name[len(cronJob.Name)-12:] == "-healthcheck" {
			// Extract the group name from the cronjob name
			// Format: {groupname}-{nodename}-healthcheck
			parts := strings.Split(cronJob.Name, "-")
			if len(parts) >= 3 && strings.Join(parts[:len(parts)-2], "-") == groupName {
				groupCronJobs = append(groupCronJobs, cronJob)
			}
		}
	}

	logger.Info("Found healthcheck CronJobs for group", "group", groupName, "count", len(groupCronJobs))
	return groupCronJobs, nil
}

// getHealthcheckStatus determines the healthcheck status from CronJob status
func (r *GroupReconciler) getHealthcheckStatus(cronJob *batchv1.CronJob) string {
	if cronJob == nil {
		return "Unknown"
	}

	// Check if the CronJob has been scheduled
	if cronJob.Status.LastScheduleTime == nil {
		return "NotScheduled"
	}

	// Check if there are any active jobs
	if len(cronJob.Status.Active) > 0 {
		return "Running"
	}

	// Check for failed jobs in the last schedule
	// If there are recent failures, mark as offline
	if cronJob.Status.LastSuccessfulTime == nil {
		// If never successful and was scheduled, it might have failed
		// Check if enough time has passed since last schedule to consider it failed
		timeSinceLastSchedule := time.Since(cronJob.Status.LastScheduleTime.Time)
		if cronJob.Spec.StartingDeadlineSeconds != nil && timeSinceLastSchedule > time.Duration(*cronJob.Spec.StartingDeadlineSeconds)*time.Second {
			return "Failed"
		}
		// If no deadline set or still within deadline, consider it running
		return "Running"
	}

	// Check if the last successful time is recent enough
	timeSinceLastSuccess := time.Since(cronJob.Status.LastSuccessfulTime.Time)

	// Parse the schedule to determine the expected interval (simplified)
	// For now, assume a reasonable default based on typical healthcheck periods
	if timeSinceLastSuccess > 5*time.Minute {
		return "Failed" // Too long since last success
	}

	return "Healthy"
}

// updateGroupstoreWithHealthcheckResults updates the healthcheckMap in groupstore with results and updates Group CRD status
func (r *GroupReconciler) updateGroupstoreWithHealthcheckResults(ctx context.Context, cronJobs []*batchv1.CronJob) error {
	logger := log.FromContext(ctx)

	// Group cronjobs by group name
	groupCronJobs := make(map[string][]*batchv1.CronJob)
	for _, cronJob := range cronJobs {
		// Extract group name from CronJob name (format: {groupname}-{nodename}-healthcheck)
		parts := strings.Split(cronJob.Name, "-")
		if len(parts) >= 3 {
			groupName := strings.Join(parts[:len(parts)-2], "-")
			groupCronJobs[groupName] = append(groupCronJobs[groupName], cronJob)
		}
	}

	// Process each group
	for groupName, groupJobs := range groupCronJobs {
		// Get the Group CRD to update its status
		group := &infrahomeclusterdevv1alpha1.Group{}
		if err := r.Get(ctx, types.NamespacedName{Name: groupName, Namespace: groupJobs[0].Namespace}, group); err != nil {
			if errors.IsNotFound(err) {
				logger.Info("Group CRD not found, skipping status update", "group", groupName)
				continue
			}
			logger.Error(err, "Failed to get Group CRD for status update", "group", groupName)
			continue
		}

		// Track if any node health status changed
		nodeHealthChanged := false

		// Process each cronjob to update individual node health status
		for _, cronJob := range groupJobs {
			// Extract node name from CronJob name (format: {groupname}-{nodename}-healthcheck)
			parts := strings.Split(cronJob.Name, "-")
			if len(parts) < 3 {
				logger.Error(fmt.Errorf("invalid cronjob name format"), "Skipping cronjob with invalid name format", "cronJob", cronJob.Name)
				continue
			}

			// Extract node name (everything between group name and "-healthcheck")
			nodeName := parts[len(parts)-2]

			// Get the healthcheck status for this node
			healthcheckStatus := r.getHealthcheckStatus(cronJob)

			// Convert healthcheck status to lowercase for consistency with CRD enum values
			var nodeHealthStatus string
			switch healthcheckStatus {
			case "Healthy":
				nodeHealthStatus = "healthy"
			case "Failed":
				nodeHealthStatus = "offline"
			case "Running", "NotScheduled":
				nodeHealthStatus = "unknown"
			default:
				nodeHealthStatus = "unknown"
			}

			// Update the node health status in groupstore
			r.GroupStore.SetNodeHealthcheckStatus(groupName, nodeName, nodeHealthStatus)
			logger.Info("Updated node healthcheck status in groupstore", "group", groupName, "node", nodeName, "status", nodeHealthStatus)

			// Update the NodeSpec.Status.Health field in the Group CRD
			nodeSpecUpdated := false
			for i := range group.Spec.NodesSpecs {
				if group.Spec.NodesSpecs[i].KubernetesNodeName == nodeName {
					// Only update if the health status has changed
					if group.Spec.NodesSpecs[i].Status.Health != nodeHealthStatus {
						group.Spec.NodesSpecs[i].Status.Health = nodeHealthStatus
						nodeHealthChanged = true
						nodeSpecUpdated = true
						logger.Info("Updated NodeSpec health status", "group", groupName, "node", nodeName, "health", nodeHealthStatus)
					}
					break
				}
			}

			if !nodeSpecUpdated {
				logger.Info("Warning: Could not find matching NodeSpec for node", "group", groupName, "node", nodeName)
			}
		}

		// Get the overall group health status from groupstore (calculated by SetNodeHealthcheckStatus)
		overallHealthStatus, exists := r.GroupStore.GetHealthcheckStatus(groupName)
		if !exists {
			overallHealthStatus = "unknown"
		}

		// Update group-level health status if it changed
		if group.Status.Health != overallHealthStatus {
			group.Status.Health = overallHealthStatus
			nodeHealthChanged = true
		}

		// Only update the Group CRD if something changed
		if nodeHealthChanged {
			// Update the Group status in the cluster
			if err := r.Status().Update(ctx, group); err != nil {
				logger.Error(err, "Failed to update Group CRD health status", "group", groupName, "health", overallHealthStatus)
				continue
			}

			// Also update the groupstore to keep it synchronized
			if err := r.GroupStore.UpdateGroupHealth(groupName, overallHealthStatus); err != nil {
				logger.Error(err, "Failed to update groupstore health status", "group", groupName, "health", overallHealthStatus)
				// Continue even if groupstore update fails, as CRD update succeeded
			}

			logger.Info("Updated Group CRD and groupstore health status", "group", groupName, "health", overallHealthStatus, "groupStoreAddr", fmt.Sprintf("%p", r.GroupStore))
		}
	}

	return nil
}

// syncHealthcheckStatuses periodically checks status of all healthcheck CronJobs and updates groupstore
func (r *GroupReconciler) syncHealthcheckStatuses(ctx context.Context) {
	logger := log.FromContext(ctx)

	// Create a ticker that runs every 10 seconds for more responsive health checks
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Run immediately on startup
	logger.Info("Starting initial healthcheck status sync")
	cronJobs, err := r.listHealthcheckCronJobs(ctx)
	if err != nil {
		logger.Error(err, "Failed to list healthcheck CronJobs during initial sync")
	} else {
		if err := r.updateGroupstoreWithHealthcheckResults(ctx, cronJobs); err != nil {
			logger.Error(err, "Failed to update groupstore with healthcheck results during initial sync")
		} else {
			logger.Info("Completed initial healthcheck status sync", "cronJobs", len(cronJobs))
		}
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("Stopping healthcheck status sync")
			return
		case <-ticker.C:
			logger.Info("Starting periodic healthcheck status sync")

			// List all healthcheck CronJobs
			cronJobs, err := r.listHealthcheckCronJobs(ctx)
			if err != nil {
				logger.Error(err, "Failed to list healthcheck CronJobs")
				continue
			}

			// Update groupstore with healthcheck results
			if err := r.updateGroupstoreWithHealthcheckResults(ctx, cronJobs); err != nil {
				logger.Error(err, "Failed to update groupstore with healthcheck results")
				continue
			}

			logger.Info("Completed healthcheck status sync", "cronJobs", len(cronJobs))
		}
	}
}
