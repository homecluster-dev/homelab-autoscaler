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

package core

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// NodeSlotReconciler seeds the status of Node CRs (slots) that have no matching
// Kubernetes node, i.e. slots that never had a machine behind them.
//
// KubernetesNodeReconciler is driven by corev1.Node objects, so a freshly committed
// pool (all slots powerState: "off") never gets its status written and stays with
// status.powerState: "" / status.progress: "". The gRPC cloud provider counts every
// Node CR whose status.progress is not "shuttingdown"/"shutdown" as part of the
// target size of the node group, so such a pool reports targetSize == number of slots,
// the cluster-autoscaler sees "max size reached" and never asks for a scale up.
//
// This controller runs the same seeding for Node CRs that have no corev1.Node: a slot
// that is desired to be off, and whose status was never observed, is reported as
// off/shutdown so that it is excluded from the target size and can be picked up by
// NodeGroupIncreaseSize.
//
// Slots whose status records a startup (status.lastStartupTime) are never seeded: the
// state machine ran for them at least once, so the machine may well be running while its
// Kubernetes node object is absent (for example after a failed status write).
type NodeSlotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

// Reconcile seeds the status of a never-provisioned slot. It is idempotent and never
// touches a slot that is on, transitioning (startingup/shuttingdown) or ready.
func (r *NodeSlotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	nodeCR := &infrav1alpha1.Node{}
	if err := r.Get(ctx, req.NamespacedName, nodeCR); err != nil {
		// Node CR not found - possibly deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !needsStatusSeed(nodeCR) {
		return ctrl.Result{}, nil
	}

	// A slot whose FSM has run at least once may have a real machine behind it: the
	// Kubernetes node object can be missing because the machine was never created, because
	// it was removed from the cluster, or because an earlier status write failed. Seeding
	// off/shutdown in that case excludes a running machine from the target size, so leave
	// the status to the controllers that observe the machine.
	if nodeCR.Status.LastStartupTime != nil {
		logger.Info("Skipping status seed for a slot the state machine has already started",
			"node", nodeCR.Name,
			"lastStartupTime", nodeCR.Status.LastStartupTime,
			"warning", "the machine may still be running while its Kubernetes node object is absent; "+
				"verify it out of band, the operator will not re-report its state")

		return ctrl.Result{}, nil
	}

	// A slot that has a matching Kubernetes node is owned by KubernetesNodeReconciler,
	// which reports the real state. Never seed over it.
	if name := nodeCR.Spec.KubernetesNodeName; name != "" {
		k8sNode := &corev1.Node{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, k8sNode); err == nil {
			logger.V(1).Info("Kubernetes node exists, leaving status to the node controller",
				"node", nodeCR.Name, "kubernetesNode", name)
			return ctrl.Result{}, nil
		} else if !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to get Kubernetes node", "node", nodeCR.Name, "kubernetesNode", name)
			return ctrl.Result{}, err
		}
	}

	// Create a copy for the status update to avoid conflicts with spec updates
	nodeCRCopy := nodeCR.DeepCopy()
	nodeCRCopy.Status.PowerState = infrav1alpha1.PowerStateOff
	nodeCRCopy.Status.Progress = infrav1alpha1.ProgressShutdown

	// Use status subresource to avoid conflicts with spec updates
	if err := r.Status().Update(ctx, nodeCRCopy); err != nil {
		logger.Error(err, "Failed to seed Node status", "node", nodeCR.Name)
		// controller-runtime discards the Result when the error is non-nil, the requeue
		// backoff is decided by the controller, so only the error is returned here.
		return ctrl.Result{}, err
	}

	logger.Info("Seeded status of never-provisioned slot",
		"node", nodeCR.Name,
		"powerstate", nodeCRCopy.Status.PowerState,
		"progress", nodeCRCopy.Status.Progress)

	return ctrl.Result{}, nil
}

// needsStatusSeed reports whether the slot's status must be seeded as off/shutdown.
// It returns false for slots that are on, for transitional progress values
// (startingup/shuttingdown), for ready slots, and for slots that are already seeded.
func needsStatusSeed(node *infrav1alpha1.Node) bool {
	// Only slots that are desired to be off are capacity that is not in use.
	if node.Spec.DesiredPowerState != infrav1alpha1.PowerStateOff {
		return false
	}

	// A real status (the machine is on) is never overwritten.
	if node.Status.PowerState == infrav1alpha1.PowerStateOn {
		return false
	}

	switch node.Status.Progress {
	case infrav1alpha1.ProgressStartingUp, infrav1alpha1.ProgressShuttingDown, infrav1alpha1.ProgressReady:
		// Transition in flight, or the node is serving: leave it alone.
		return false
	case infrav1alpha1.ProgressShutdown:
		// Already excluded from the target size, only complete an empty power state.
		return node.Status.PowerState != infrav1alpha1.PowerStateOff
	default:
		// Empty progress: the slot was never observed by the node controllers, so it is
		// counted as part of the target size of the node group.
		return true
	}
}

// SetupWithManager sets up the controller with the Manager.
//
// The seeding only depends on spec and on the fields written by the other reconcilers, so
// the watch is narrowed to create events and spec changes (generation bumps). Status-only
// updates from KubernetesNodeReconciler and the FSM bump the resourceVersion without
// changing the generation and must not re-trigger this reconciler.
func (r *NodeSlotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.Node{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("core-node-slot").
		Complete(r)
}
