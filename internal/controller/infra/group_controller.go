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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrahomeclusterdevv1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// GroupReconciler reconciles a Group object
type GroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=groups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=groups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=groups/finalizers,verbs=update

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
		logger.Error(err, "Failed to get Group", "group", req.Name)
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrahomeclusterdevv1alpha1.Group{}).
		Named("group").
		Complete(r)
}
