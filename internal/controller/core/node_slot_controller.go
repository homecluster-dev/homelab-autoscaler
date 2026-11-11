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

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
)

// NodeSlotReconciler seeds the report of Node CRs (slots) that have never been observed: a freshly
// committed pool has status.powerState "" and status.phase "", and `kubectl get
// nodes.infra.homecluster.dev` on a pool that is idle by design reads as a pool of broken slots.
//
// What changed under the NodeClaim model: the seeding is no longer load-bearing for the arithmetic.
// In v1alpha1 the provider counted every slot whose status.progress was not one of
// shuttingdown/shutdown/startingup, so an unseeded pool reported targetSize == number of slots,
// cluster-autoscaler read "max size reached" and never asked for a scale up - the seeding existed to
// make that arithmetic come out. v2alpha1 counts live claims, and "no live claim" needs no seeding,
// no status and no observation (docs/design/nodeclaim.md §2). What is left here is the report: one
// column that tells an operator the slot is free.
//
// KubernetesNodeReconciler is driven by corev1.Node objects, so a slot with no machine never gets a
// pass from it. This controller runs on the slot object itself, and writes nothing a controller that
// observes a machine owns: it refuses any slot with a live claim, a non-empty status, a machine
// observed up, a node object, or a slot the state machine has already run for.
type NodeSlotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

// Reconcile seeds the report of a never-provisioned slot. It is idempotent and never touches a slot
// with a claim, a machine, or a report someone else wrote.
func (r *NodeSlotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	nodeCR := &infrav2alpha1.Node{}
	if err := r.Get(ctx, req.NamespacedName, nodeCR); err != nil {
		// Node CR not found - possibly deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !needsStatusSeed(nodeCR) {
		return ctrl.Result{}, nil
	}

	// A live claim means the slot is wanted: its report belongs to the controllers that read the
	// claim, and seeding off here would report a slot that is being booted as free.
	claims, err := claim.ListOfSlot(ctx, r.Client, nodeCR.Namespace, nodeCR.Name)
	if err != nil {
		logger.Error(err, "Failed to list the claims of the slot", "node", nodeCR.Name)

		return ctrl.Result{}, err
	}

	if claim.LiveOfSlot(claims) != nil {
		return ctrl.Result{}, nil
	}

	// A slot whose state machine has run at least once may have a real machine behind it: the
	// Kubernetes node object can be missing because the machine was never created, because it was
	// removed from the cluster, or because an earlier status write failed. Seeding off in that case
	// reports a running machine as free, so leave the status to the controllers that observe it.
	if nodeCR.Status.LastStartupTime != nil {
		logger.Info("Skipping the status seed, the state machine has already run for this slot; "+
			"the machine may still be running while its Kubernetes node object is absent, so its "+
			"state must be verified out of band",
			"node", nodeCR.Name,
			"lastStartupTime", nodeCR.Status.LastStartupTime)

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
	nodeCRCopy.Status.PowerState = infrav2alpha1.PowerStateOff
	nodeCRCopy.Status.Phase = infrav2alpha1.NodePhaseOff

	// Use status subresource to avoid conflicts with spec updates
	if err := r.Status().Update(ctx, nodeCRCopy); err != nil {
		logger.Error(err, "Failed to seed Node status", "node", nodeCR.Name)
		// controller-runtime discards the Result when the error is non-nil, the requeue
		// backoff is decided by the controller, so only the error is returned here.
		return ctrl.Result{}, err
	}

	logger.Info("Seeded the report of a never-provisioned slot",
		"node", nodeCR.Name,
		"powerstate", string(nodeCRCopy.Status.PowerState),
		"phase", string(nodeCRCopy.Status.Phase))

	return ctrl.Result{}, nil
}

// needsStatusSeed reports whether the slot's report must be seeded as off. It returns false for a
// slot that carries a report already, a slot with a claim digest (an episode ran on it), and a slot
// whose machine is observed up.
func needsStatusSeed(node *infrav2alpha1.Node) bool {
	if node.Status.PowerState == infrav2alpha1.PowerStateOn {
		// A real observation. Never overwrite it.
		return false
	}

	if node.Status.LastClaim != nil {
		// The slot has a claim history; its report is written by the observation and the claim
		// paths, which read the history.
		return false
	}

	switch node.Status.Phase {
	case infrav2alpha1.NodePhaseOff:
		// Already seeded: complete an empty power state, and nothing else.
		return node.Status.PowerState != infrav2alpha1.PowerStateOff
	case infrav2alpha1.NodePhasePending, infrav2alpha1.NodePhaseBooting,
		infrav2alpha1.NodePhaseReady, infrav2alpha1.NodePhaseDraining:
		// Someone who reads the claim, or observes the machine, owns this report.
		return false
	default:
		// Empty: a slot of a freshly committed pool, idle by design.
		return true
	}
}

// SetupWithManager sets up the controller with the Manager.
//
// What has to be seeded is decided by the spec, so the watch uses GenerationChangedPredicate: it
// lets create events and spec changes through, and ignores updates that only touch fields written by
// the other reconcilers. In v2alpha1 the spec of a slot carries no runtime intent at all
// (spec.desiredPowerState is gone with the claim model, docs/design/nodeclaim.md §2), so "the spec
// changed" is the only question this predicate answers, and it is the right one: a status write bumps
// the resourceVersion, never the generation, so status-only updates from KubernetesNodeReconciler and
// the FSM do not re-trigger this reconciler.
func (r *NodeSlotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav2alpha1.Node{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// A claim is owned by its slot, so the claims of a slot are watched through the slot: a claim
		// that appears, moves, and is collected never enqueues this reconciler, which owns a report and
		// not a machine.
		Named("core-node-slot").
		Complete(r)
}
