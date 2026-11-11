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
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/autoscaler"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// Namespace configuration instance
var namespaceConfig = config.NewNamespaceConfig()

// NodeReconciler reconciles a Node object
type KubernetesNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// EventRecorder publishes the cluster-autoscaler decisions this controller observes on a
	// node (see internal/autoscaler) on the Node CR, so `kubectl describe
	// node.infra.homecluster.dev <name>` shows what the autoscaler decided about the machine.
	// nil records nothing, which is what the unit tests use.
	EventRecorder events.EventRecorder
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
	nodeCR := &infrav2alpha1.Node{}
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

	// The moment this pass read the node object. It is a single value on purpose: the marker record
	// quotes it in its message, and internal/autoscaler's condition timestamp is not the observation
	// (SetStatusCondition keeps the old one whenever the Status does not flip, and both deletion
	// markers are Status True), so the time an operator asks about has to come from here.
	observedAt := time.Now()

	// What cluster-autoscaler decided about this node is an observation of the same node object, so
	// it is derived and written together with the power state. It is deduplicated on the slot: a
	// marker that is already recorded is no change, and a pass that records nothing writes nothing.
	_, markerChanged := autoscaler.ApplyMarker(nodeCR.DeepCopy(), node, observedAt)

	// The live claim of the slot is an input of the report, and is read here: the report of a slot
	// is (live claim, observed power, node usability), and this is the path that sees the machine.
	input := r.observationInput(ctx, nodeCR, node, nodeStatus)

	// Only update when the recorded status disagrees with what is observed. A status
	// write on every reconcile would keep this controller hot, so the fast path covers
	// "power state and derived phase unchanged, and no new autoscaler marker".
	if !applyObservation(nodeCR.DeepCopy(), nodeStatus, input) && !markerChanged {
		logger.V(1).Info("Node status unchanged, skipping update", "node", node.Name, "status", nodeStatus)
		return ctrl.Result{}, nil
	}

	// The state machine writes the transitional progress values concurrently, so re-read
	// the CR and re-derive the correction on every attempt. Same status-update and
	// conflict-retry pattern as fsm.NodeStateMachine.updateNodeProgress.
	var (
		recorded autoscaler.Decision
		wrote    bool
	)

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.Node{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(nodeCR), fresh); err != nil {
			return err
		}

		powerChanged := applyObservation(fresh, nodeStatus, input)

		decision, markerChanged := autoscaler.ApplyMarker(fresh, node, observedAt)
		if !powerChanged && !markerChanged {
			// Someone (usually the state machine) already recorded what we observed.
			return nil
		}

		// Use status subresource to avoid conflicts with spec updates
		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}

		wrote = true

		// Remember what landed, so the event is published for a write that succeeded and not
		// for one that a concurrent writer answered first.
		if decision.Recorded {
			recorded = decision
		}

		return nil
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

	// The audit line for a write that landed. A pass that the retry loop resolved with no write - a
	// concurrent writer recorded the same observation first - must not claim one: this is the line an
	// operator reads to date a power-state change of a slot, and a per-resync copy of it is a log that
	// says nothing.
	if wrote {
		logger.Info("Successfully updated Node status", "node", node.Name, "powerstate", nodeStatus)
	} else {
		logger.V(1).Info("Node status was recorded by another writer, nothing to update", "node", node.Name, "status", nodeStatus)
	}

	// Publish the autoscaler decision the write above recorded: the Event on the Node CR, the
	// node's taints and spec.unschedulable in the log. After the write, so a record is never
	// published for a status update that did not land.
	if recorded.Recorded {
		autoscaler.Emit(ctx, r.EventRecorder, nodeCR, node, recorded)
	}

	return ctrl.Result{}, nil

}

// applyObservation records what the cluster observes about a slot: the power of its machine
// (status.powerState) and the derived report of the slot (status.phase).
//
// powerState keeps its meaning: on when the corev1.Node reports NodeReady=True, off otherwise. It is
// the observation half of the state machine's input, and the strongest evidence in the system about
// a machine - which is what the 0.1.22 precedence rule said, and what the NodeClaim model makes the
// general law instead of a tie-break between writers of one field.
//
// phase is computed, not decided: phase.Derive over (the live claim of the slot, the power state
// observed here, the usability of the node object). That is why this path can write it without
// knowing anything about a transition: the same formula runs in internal/fsm, so the two writers of
// the report cannot produce different values from the same inputs, and "an observed on outranks a
// recorded shutdown" is no longer a rule - the record is not an input.
//
// It returns true when the object changed, so the caller skips the status write when there is
// nothing to do.
func applyObservation(node *infrav2alpha1.Node, powerState infrav2alpha1.PowerState, in phase.Input) bool {
	changed := false

	if node.Status.PowerState != powerState {
		node.Status.PowerState = powerState
		changed = true
	}

	in.ObservedPower = powerState

	derived := phase.Derive(in)
	if node.Status.Phase != derived {
		node.Status.Phase = derived
		changed = true
	}

	return changed
}

// nodeUsable reports whether the corev1.Node of a slot can take the pool's work, and is the third
// input of the state machine: the node is Ready, carries none of the readiness taints the slot's
// Group declares, and is not cordoned. The slot's own record of the taint keys
// (autoscalerpkg.ReadinessTaintKeysOfSlot) is read, so this path needs no Group: the infra
// reconciler stamps the keys on the slot from the Group it reads.
func nodeUsable(k8sNode *corev1.Node, nodeCR *infrav2alpha1.Node) bool {
	return phase.Unusable(k8sNode, autoscaler.ReadinessTaintKeysOfSlot(nodeCR)) == ""
}

// observationInput assembles the state machine's input for one observation of one slot: the live
// claim the slot carries, the power the node object shows, and whether the node can take work.
//
// A claim list that fails is not fatal: a slot with no claim read is a slot reported from the
// observation alone, which is the value of the report before any claim existed, and the next pass
// re-reads. Failing the observation of a machine because a claim could not be listed would leave
// the report of a whole pool stale.
func (r *KubernetesNodeReconciler) observationInput(ctx context.Context, nodeCR *infrav2alpha1.Node, k8sNode *corev1.Node, powerState infrav2alpha1.PowerState) phase.Input {
	claims, err := claim.ListOfSlot(ctx, r.Client, nodeCR.Namespace, nodeCR.Name)
	live := (*infrav2alpha1.NodeClaim)(nil)

	if err == nil {
		live = claim.LiveOfSlot(claims)
	}

	return phase.Input{
		Claim:         live,
		ObservedPower: powerState,
		NodeUsable:    nodeUsable(k8sNode, nodeCR),
	}
}

// getNodeStatus maps the Ready condition of the Kubernetes node onto a power state. A
// node that is not Ready is reported as off: the kubelet registers the node when the
// machine boots and the object is left behind, NotReady, when it is powered off. NotReady
// is therefore a weak observation, which is why applyObservedPowerState only corrects
// progress for it when the recorded power state actually changes.
func (r *KubernetesNodeReconciler) getNodeStatus(node *corev1.Node) infrav2alpha1.PowerState {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return infrav2alpha1.PowerStateOn
		}
	}
	return infrav2alpha1.PowerStateOff
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubernetesNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Named("core-node").
		Complete(r)
}
