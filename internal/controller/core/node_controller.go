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
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
)

// Namespace configuration instance
var namespaceConfig = config.NewNamespaceConfig()

// NodeReconciler reconciles a Node object
type KubernetesNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes/finalizers,verbs=update

// Reconcile synchronizes Kubernetes node status with Homelab Node CRDs
// This controller ensures Homelab Node resources reflect the actual state
// of corresponding Kubernetes nodes for power management and autoscaling.
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
	if err := r.Get(ctx, client.ObjectKey{Name: nodeLabel, Namespace: namespaceConfig.Get()}, nodeCR); err != nil {
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

	// Only update when the recorded status disagrees with what is observed. A status
	// write on every reconcile would keep this controller hot, so the fast path covers
	// "power state unchanged AND progress already consistent with it".
	if !applyObservedPowerState(nodeCR.DeepCopy(), nodeStatus) {
		logger.V(1).Info("Node status unchanged, skipping update", "node", node.Name, "status", nodeStatus)
		return ctrl.Result{}, nil
	}

	// The state machine writes the transitional progress values concurrently, so re-read
	// the CR and re-derive the correction on every attempt. Same status-update and
	// conflict-retry pattern as fsm.NodeStateMachine.updateNodeProgress.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(nodeCR), fresh); err != nil {
			return err
		}

		if !applyObservedPowerState(fresh, nodeStatus) {
			// Someone (usually the state machine) already recorded what we observed.
			return nil
		}

		// Use status subresource to avoid conflicts with spec updates
		return r.Status().Update(ctx, fresh)
	}); err != nil {
		if apierrors.IsNotFound(err) {
			// The Node CR disappeared underneath us, nothing left to record.
			logger.V(1).Info("Node CR not found, skipping status update", "node", node.Name, "nodeCR", nodeCR.Name)
			return ctrl.Result{}, nil
		}

		logger.Error(err, "Failed to update Node status", "node", node.Name, "powerstate", nodeStatus)
		// controller-runtime discards the Result when the error is non-nil, the requeue
		// backoff is decided by the controller, so only the error is returned here.
		return ctrl.Result{}, err
	}

	logger.Info("Successfully updated Node status", "node", node.Name, "powerstate", nodeStatus)

	return ctrl.Result{}, nil

}

// expectedProgress returns the progress value that corresponds to an observed power
// state, and reports whether such a value exists. Only the steady states have one:
// startingup and shuttingdown are transitional values owned by the state machine and
// are never derived from an observation.
func expectedProgress(powerState infrav1alpha1.PowerState) (infrav1alpha1.Progress, bool) {
	switch powerState {
	case infrav1alpha1.PowerStateOn:
		return infrav1alpha1.ProgressReady, true
	case infrav1alpha1.PowerStateOff:
		return infrav1alpha1.ProgressShutdown, true
	default:
		return "", false
	}
}

// applyObservedPowerState records the observed power state on the Node CR and corrects
// status.progress to the value that corresponds to it. It returns true when the object
// was changed, so the caller can skip the status write when there is nothing to do.
//
// The progress correction is deliberately not gated on the power state changing. A slot
// that was shut down and started again through a path this controller did not observe
// (desiredPowerState flipped back to on, machine booted) keeps status.powerState: on
// while status.progress stays "shutdown". That stale progress is not cosmetic:
// NodeGroupTargetSize excludes slots whose progress is shutdown/shuttingdown, so a group
// with one Ready node reports targetSize 0 and cluster-autoscaler refuses to scale.
//
// The transitional progress values (startingup/shuttingdown) are never overwritten: they
// are owned by the state machine, which is the only one that knows a power operation is
// in flight.
func applyObservedPowerState(node *infrav1alpha1.Node, powerState infrav1alpha1.PowerState) bool {
	changed := false

	if node.Status.PowerState != powerState {
		node.Status.PowerState = powerState
		changed = true
	}

	desiredProgress, derived := expectedProgress(powerState)
	if !derived {
		return changed
	}

	switch node.Status.Progress {
	case infrav1alpha1.ProgressStartingUp, infrav1alpha1.ProgressShuttingDown:
		// Transition in flight, the state machine owns status.progress.
	default:
		if node.Status.Progress != desiredProgress {
			node.Status.Progress = desiredProgress
			changed = true
		}
	}

	return changed
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
