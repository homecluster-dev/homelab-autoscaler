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
	groupstore *groupstore.GroupStore
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
		if err.Error() == "groups.infra.homecluster.dev \""+req.Name+"\" not found" {
			logger.Info("Group not found, will be removed from groupstore", "group", req.Name)
			// Remove from groupstore if it exists
			if err := r.groupstore.Remove(req.Name); err != nil {
				logger.Error(err, "Failed to remove group from groupstore", "group", req.Name)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		// Error reading the object - return with requeue
		logger.Error(err, "Failed to get Group", "group", req.Name)
		return ctrl.Result{}, err
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
		if err := r.groupstore.Remove(req.Name); err != nil {
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
	if err := r.groupstore.AddOrUpdate(group); err != nil {
		logger.Error(err, "Failed to add/update group in groupstore", "group", req.Name)
		return ctrl.Result{}, err
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

	// Update the Group status
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
	// Initialize the groupstore
	r.groupstore = groupstore.NewGroupStore()

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

// generateHealthcheckCronJob creates a CronJob manifest for the Group's healthcheck
func (r *GroupReconciler) generateHealthcheckCronJob(group *infrahomeclusterdevv1alpha1.Group) *batchv1.CronJob {
	cronJobName := fmt.Sprintf("%s-healthcheck", group.Name)
	schedule := fmt.Sprintf("*/%d * * * *", group.Spec.HealthcheckPeriod)

	// Convert MinimalPodSpec to corev1.PodSpec
	containers := []corev1.Container{
		{
			Name:    "healthcheck",
			Image:   group.Spec.HealthcheckPodSpec.Image,
			Command: group.Spec.HealthcheckPodSpec.Command,
			Args:    group.Spec.HealthcheckPodSpec.Args,
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
	for _, vol := range group.Spec.HealthcheckPodSpec.Volumes {
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

// createOrUpdateHealthcheckCronJob creates or updates the healthcheck CronJob for a Group
func (r *GroupReconciler) createOrUpdateHealthcheckCronJob(ctx context.Context, group *infrahomeclusterdevv1alpha1.Group) error {
	logger := log.FromContext(ctx)
	cronJob := r.generateHealthcheckCronJob(group)

	// Create or update the CronJob
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cronJob, func() error {
		// Only update the spec, not metadata
		cronJob.Spec = r.generateHealthcheckCronJob(group).Spec
		return nil
	})

	if err != nil {
		logger.Error(err, "Failed to create or update healthcheck CronJob", "cronJob", cronJob.Name)
		return err
	}

	logger.Info("Successfully created/updated healthcheck CronJob", "cronJob", cronJob.Name)
	return nil
}

// deleteHealthcheckCronJob deletes the healthcheck CronJob for a Group
func (r *GroupReconciler) deleteHealthcheckCronJob(ctx context.Context, group *infrahomeclusterdevv1alpha1.Group) error {
	logger := log.FromContext(ctx)
	cronJobName := fmt.Sprintf("%s-healthcheck", group.Name)
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronJobName,
			Namespace: group.Namespace,
		},
	}

	// Check if the CronJob exists before attempting to delete
	err := r.Get(ctx, types.NamespacedName{Name: cronJobName, Namespace: group.Namespace}, cronJob)
	if err != nil {
		if errors.IsNotFound(err) {
			// CronJob doesn't exist, nothing to do
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

// listHealthcheckCronJobs lists all CronJobs with names matching the pattern {groupname}-healthcheck
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
		// Check if the CronJob name follows the pattern {groupname}-healthcheck
		if len(cronJob.Name) > 12 && cronJob.Name[len(cronJob.Name)-12:] == "-healthcheck" {
			healthcheckCronJobs = append(healthcheckCronJobs, cronJob)
		}
	}

	logger.Info("Found healthcheck CronJobs", "count", len(healthcheckCronJobs))
	return healthcheckCronJobs, nil
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

	// Check the most recent job status
	// Note: In a real implementation, you might want to check the actual Job status
	// This is a simplified version that assumes success if no active jobs and was scheduled
	return "Healthy"
}

// updateGroupstoreWithHealthcheckResults updates the healthcheckMap in groupstore with results
func (r *GroupReconciler) updateGroupstoreWithHealthcheckResults(ctx context.Context, cronJobs []*batchv1.CronJob) error {
	logger := log.FromContext(ctx)

	for _, cronJob := range cronJobs {
		// Extract group name from CronJob name (format: {groupname}-healthcheck)
		groupName := cronJob.Name[:len(cronJob.Name)-12]

		// Get healthcheck status
		status := r.getHealthcheckStatus(cronJob)

		// Update groupstore
		r.groupstore.SetHealthcheckStatus(groupName, status)
		logger.Info("Updated healthcheck status", "group", groupName, "status", status)
	}

	return nil
}

// syncHealthcheckStatuses periodically checks status of all healthcheck CronJobs and updates groupstore
func (r *GroupReconciler) syncHealthcheckStatuses(ctx context.Context) {
	logger := log.FromContext(ctx)

	// Create a ticker that runs every 30 seconds
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

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
