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
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// NodeReconciler reconciles a Node object
type KubernetesNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Node object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *KubernetesNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the Node object
	node := &corev1.Node{}
	err := r.Get(ctx, req.NamespacedName, node)
	if err != nil {
		// Node not found - possibly deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if this Kubernetes node is managed by our controller
	nodeLabel, hasNodeLabel := node.Labels["infra.homecluster.dev/node"]
	if !hasNodeLabel {
		// Node is not managed by our controller, skip
		return ctrl.Result{}, nil
	}

	// Get the corresponding Node CR
	nodeCR := &infrav1alpha1.Node{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: nodeLabel, Namespace: "homelab-autoscaler-system"}, nodeCR); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("Node CR not found for Kubernetes node", "nodeName", node.Name, "nodeCR", nodeLabel)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Node CR", "nodeName", node.Name, "nodeCR", nodeLabel)
		return ctrl.Result{}, err
	}

	// Get current node status from Kubernetes
	nodeStatus := r.getNodeStatus(node)
	logger.Info("Node status retrieved", "name", node.Name, "status", nodeStatus)

	// Only update if the status has actually changed to avoid conflicts
	if nodeCR.Status.PowerState == nodeStatus {
		logger.V(1).Info("Node status unchanged, skipping update", "node", node.Name, "status", nodeStatus)
		return ctrl.Result{}, nil
	}

	// Create a copy for status update to avoid race conditions
	nodeCRCopy := nodeCR.DeepCopy()
	nodeCRCopy.Status.PowerState = nodeStatus
	
	// Update progress based on power state, but only if not already in a transitional state
	if nodeCRCopy.Status.Progress != infrav1alpha1.ProgressStartingUp &&
		nodeCRCopy.Status.Progress != infrav1alpha1.ProgressShuttingDown {
		switch nodeStatus {
		case infrav1alpha1.PowerStateOff:
			nodeCRCopy.Status.Progress = infrav1alpha1.ProgressShutdown
		case infrav1alpha1.PowerStateOn:
			nodeCRCopy.Status.Progress = infrav1alpha1.ProgressReady
		}
	}

	// Use status subresource to avoid conflicts with spec updates
	if err := r.Status().Update(ctx, nodeCRCopy); err != nil {
		logger.Error(err, "Failed to update Node status", "node", node.Name, "powerstate", nodeCRCopy.Status.PowerState)
		return ctrl.Result{RequeueAfter: time.Second * 30}, err
	}

	logger.Info("Successfully updated Node status", "node", node.Name, "powerstate", nodeCRCopy.Status.PowerState, "progress", nodeCRCopy.Status.Progress)

	return ctrl.Result{}, nil

}

func (r *KubernetesNodeReconciler) getNodeStatus(node *corev1.Node) infrav1alpha1.PowerState {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return infrav1alpha1.PowerStateOn
		}
	}
	return infrav1alpha1.PowerStateOff
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubernetesNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Named("core-node").
		Complete(r)
}
