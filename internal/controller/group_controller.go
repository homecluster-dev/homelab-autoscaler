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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrahomeclusterdevv1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
)

// Constants for healthcheck status values
const (
	HealthcheckStatusRunning      = "Running"
	HealthcheckStatusFailed       = "Failed"
	HealthcheckStatusUnknown      = "unknown"
	HealthcheckStatusHealthy      = "Healthy"
	HealthcheckStatusNotScheduled = "NotScheduled"
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
		// Remove from groupstore
		if err := r.GroupStore.Remove(req.Name); err != nil {
			logger.Error(err, "Failed to remove group from groupstore during deletion", "group", req.Name)
			return ctrl.Result{}, err
		}
		// Ensure finalizers are handled
		if len(group.Finalizers) > 0 {
			// Remove finalizers logic would go here
			logger.Info("Group has finalizers that need to be handled", "group", req.Name)
		}
		return ctrl.Result{}, nil
	}

	// Add/update the Group in the groupstore storage
	if err := r.GroupStore.AddOrUpdate(group); err != nil {
		logger.Error(err, "Failed to add/update group in groupstore", "group", req.Name)
		return ctrl.Result{}, err
	}
	logger.Info("Added/updated group in groupstore", "group", req.Name, "groupStoreAddr", fmt.Sprintf("%p", r.GroupStore), "groupCount", r.GroupStore.Count())

	// Link existing nodes that have the group label but no owner reference
	if err := r.linkExistingNodesToGroup(ctx, group); err != nil {
		logger.Error(err, "Failed to link existing nodes to Group", "group", req.Name)
		// Don't fail the reconciliation if we can't link nodes, just log the error
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrahomeclusterdevv1alpha1.Group{}).
		Named("group").
		Complete(r)
}

// linkExistingNodesToGroup scans for Node CRs with the group label and sets owner references
func (r *GroupReconciler) linkExistingNodesToGroup(ctx context.Context, group *infrahomeclusterdevv1alpha1.Group) error {
	logger := log.FromContext(ctx)

	// List all Node CRs with the group label matching this group
	nodeList := &infrahomeclusterdevv1alpha1.NodeList{}
	labelSelector := client.MatchingLabels{"group": group.Name}

	if err := r.List(ctx, nodeList, labelSelector, client.InNamespace(group.Namespace)); err != nil {
		logger.Error(err, "Failed to list nodes with group label", "group", group.Name)
		return err
	}

	logger.Info("Found nodes with group label", "group", group.Name, "nodeCount", len(nodeList.Items))

	// Process each node
	for _, node := range nodeList.Items {
		// Check if the node already has an owner reference to this group
		hasOwnerRef := false
		for _, ownerRef := range node.OwnerReferences {
			if ownerRef.UID == group.UID && ownerRef.Kind == "Group" && ownerRef.APIVersion == "infra.homecluster.dev/v1alpha1" {
				hasOwnerRef = true
				break
			}
		}

		// If the node already has the correct owner reference, skip it
		if hasOwnerRef {
			logger.Info("Node already has correct owner reference to Group", "node", node.Name, "group", group.Name)
			continue
		}

		// Set the owner reference using controllerutil.SetControllerReference
		if err := ctrl.SetControllerReference(group, &node, r.Scheme); err != nil {
			logger.Error(err, "Failed to set controller reference for node", "node", node.Name, "group", group.Name)
			continue // Continue with other nodes even if one fails
		}

		// Update the Node with the new owner reference
		if err := r.Update(ctx, &node); err != nil {
			logger.Error(err, "Failed to update Node with owner reference", "node", node.Name)
			continue // Continue with other nodes even if one fails
		}

		logger.Info("Successfully set owner reference for Node to Group", "node", node.Name, "group", group.Name)
	}

	return nil
}
