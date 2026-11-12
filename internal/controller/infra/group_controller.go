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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrahomeclusterdevv2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// GroupReconciler reconciles a Group object
type GroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=groups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=groups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=groups/finalizers,verbs=update

// The shape of a pool is read off its slots, so this reconciler lists Node CRs. It never writes
// one: the slots are the input of a count, and the group's status is its only output.
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Log the reconciliation start
	logger.Info("Starting reconciliation for Group", "group", req.Name, "namespace", req.Namespace)

	// Get the Group resource from the request
	group := &infrahomeclusterdevv2alpha1.Group{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		// The object was not found, return early
		logger.Error(err, "Failed to get Group", "group", req.Name)
		return ctrl.Result{}, err
	}

	// The slots of the pool: the same list the provider's max-size arithmetic reads (label `group`),
	// and the only source of every number this controller writes.
	slots := &infrahomeclusterdevv2alpha1.NodeList{}
	selector := labels.SelectorFromSet(labels.Set{fsm.LabelGroup: group.Name})

	if err := r.List(ctx, slots, &client.ListOptions{Namespace: group.Namespace, LabelSelector: selector}); err != nil {
		logger.Error(err, "Failed to list the slots of the group", "group", group.Name)

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

	// The pool at a glance: how many slots it has, how many report a machine that is up, and the
	// last scale it made.
	recordPoolShape(group, slots.Items)

	// Update the Group in the cluster
	if err := r.Status().Update(ctx, group); err != nil {
		logger.Error(err, "Failed to update Group status", "group", group.Name)
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled Group",
		"group", group.Name,
		"slots", group.Status.Slots,
		"on", group.Status.On,
		"off", group.Status.Off,
		"lastScale", group.Status.LastScaleAction)

	return ctrl.Result{Requeue: false}, nil
}

// recordPoolShape materializes the shape of a pool from its slots, in one pass over the list the
// reconciler already has and by the one writer of these fields.
//
// The counts are derived from the slots' reports (phase.IsUp over status.phase), which is what
// keeps them honest without a second record of the same fact: a slot's phase is computed from (the
// live claim, the observed power, the node's usability), and the pool's number is the sum of its
// slots' values. It is a display sum, and the group controller decides nothing from it - the
// provider's arithmetic reads the live claims and the slots directly (docs/design/nodeclaim.md §5).
//
// The last scale is the one field that is **materialized rather than read on demand**, and it has
// to be: the same story is a Kubernetes Event on a slot, Events are garbage-collected in about an
// hour, and "what did this pool do last night" is asked the next morning. Node.status.lastClaim is
// the per-slot record that outlives a claim's TTL collection, so the newest of them, over the pool,
// is materialized onto the pool itself: a scale is visible a day later, on the object an operator
// opens, with nothing to correlate and no Event to have kept.
func recordPoolShape(group *infrahomeclusterdevv2alpha1.Group, slots []infrahomeclusterdevv2alpha1.Node) {
	group.Status.Slots = int32(len(slots))

	var on int32

	for i := range slots {
		if phase.IsUp(slots[i].Status.Phase) {
			on++
		}
	}

	group.Status.On = on
	group.Status.Off = group.Status.Slots - on

	at, action, ok := lastScaleOf(slots)
	if !ok {
		// No slot of this pool has ever had a request recorded on it: the pool has never scaled,
		// which is a different sentence from "the last scale was long ago".
		group.Status.LastScaleAt = nil
		group.Status.LastScaleAction = ""

		return
	}

	stamp := metav1.NewTime(at)
	group.Status.LastScaleAt = &stamp
	group.Status.LastScaleAction = action
}

// lastScaleOf returns the newest scale the slots remember: when it was asked for, and the one line
// naming the slot and the request that moved it (`up remote-worker-3`, `down remote-worker-7`,
// `ca:DeleteNodes remote-worker-2`).
//
// "Newest" is the request's own instant (status.lastClaim.requestedAt, falling back to finishedAt
// for a digest written by a path that had no request time), because the question is when the pool
// grew, not when its episode closed. The list the API server returns is name-ordered, so a tie is
// resolved deterministically by keeping the first newest slot.
func lastScaleOf(slots []infrahomeclusterdevv2alpha1.Node) (time.Time, string, bool) {
	var (
		newest  time.Time
		action  string
		found   bool
		bestKey string
	)

	for i := range slots {
		digest := slots[i].Status.LastClaim
		if digest == nil {
			continue
		}

		at := time.Time{}
		if digest.RequestedAt != nil {
			at = digest.RequestedAt.Time
		} else if digest.FinishedAt != nil {
			at = digest.FinishedAt.Time
		}

		if at.IsZero() {
			// A digest with no time cannot date a scale; a slot whose claim never carried one is
			// not evidence about when the pool moved.
			continue
		}

		key := slots[i].Name

		if !found || at.After(newest) || (at.Equal(newest) && key < bestKey) {
			newest = at
			action = fmt.Sprintf("%s %s", scaleVerbOf(digest.Call), key)
			bestKey = key
			found = true
		}
	}

	return newest, action, found
}

// scaleVerbOf renders the request a slot carries into the verb an operator reads. The three calls
// are the three ways a pool moves in this model: the autoscaler asking for a machine, the
// autoscaler taking one back, and the autoscaler naming the machines to remove. Anything else (a
// call a future version adds) is rendered by its own name, so the column never silently lies about
// a verb it does not know.
func scaleVerbOf(call string) string {
	switch call {
	case infrahomeclusterdevv2alpha1.ClaimCallIncreaseSize:
		return "up"
	case infrahomeclusterdevv2alpha1.ClaimCallDecreaseTargetSize:
		return "down"
	case infrahomeclusterdevv2alpha1.ClaimCallDeleteNodes:
		return "ca:DeleteNodes"
	case "":
		return "scale"
	default:
		return call
	}
}

// SetupWithManager sets up the manager for the Group reconciler.
//
// The pool's shape is a sum over its slots, so a slot is what has to enqueue the pool: the
// controller reference the infra node reconciler stamps on every slot (SetControllerReference) makes
// `Owns(&Node{})` the mapping, so committing a slot to the pool, and every phase change of one,
// re-renders the pool's numbers on the instant instead of on the next resync.
func (r *GroupReconciler) SetupWithManager(mgr ctrl.Manager) error {

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrahomeclusterdevv2alpha1.Group{}).
		Owns(&infrahomeclusterdevv2alpha1.Node{}).
		Named("group").
		Complete(r)
}
