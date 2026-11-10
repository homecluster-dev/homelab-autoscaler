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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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
		return ctrl.Result{RequeueAfter: time.Second * 30}, err
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
func (r *NodeSlotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.Node{}).
		Named("core-node-slot").
		Complete(r)
}
