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

// Package claim is the arithmetic of NodeClaim objects: what a request costs, which slots are
// free, how many units a group is paying for, and what the slot keeps after the claim is
// collected.
//
// It is the package that makes "desired power is derived, never stored" a computation. Every
// caller that needs to know whether a machine is wanted asks this package, and nothing asks a
// spec field of the slot: in v1alpha1 the same questions were answered by reading three fields
// written by four controllers (spec.powerState, status.powerState, status.progress, and the
// three-field scan NodeGroupIncreaseSize did to find a free slot).
package claim

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// Labels carried by every claim, so a slot's claims are one list away (`kubectl get nodeclaim
// -l infra.homecluster.dev/node=<slot>`) and a pool's are one selector
// (`-l group=<group>`). The label keys are the ones the lifecycle Jobs already use
// (internal/fsm/types.go), so a slot's claims and its Jobs are read with the same commands.
const (
	LabelSlot  = "infra.homecluster.dev/node"
	LabelGroup = "group"

	// RequestedByAutoscaler is the free-text identity the gRPC provider stamps on a claim it
	// creates. It is not parsed, it is read.
	RequestedByAutoscaler = "cluster-autoscaler"

	// DefaultTTLSecondsAfterFinished is how long a terminal claim survives. Claims are the record
	// of one scale-up episode; the slot keeps the digest, so the object is collectable, and a
	// homelab pool that scales twice a day must not accumulate objects forever.
	DefaultTTLSecondsAfterFinished = int32(3600)

	// MaxConsecutiveAttempts is the bound the v1alpha1 fsm.StartupAttemptLimit and
	// fsm.ShutdownAttemptLimit conditions carried on the long-lived slot. It lives on the request
	// now: a claim's status.attempt is seeded from the slot's digest, and a claim whose attempt
	// crosses this bound is failed before it buys a machine. The streak ends with the object that
	// carries it, which is why there is no reset heuristic (docs/design/nodeclaim.md §4).
	MaxConsecutiveAttempts = int32(3)
)

// AttemptBudget is the operator's budget for one attempt, measured from spec.requestedAt. It is
// the sum of every deadline one attempt can consume, plus one sleep at the ceiling - the same
// expression the 0.1.20 progress watchdog derived for the same reason, and the watchdog is gone
// with this: the budget of a claim is what its deadline enforces.
//
//	job monitor poll + registration wait + readiness gate + margin
//	JobMonitorPollInterval (30s) + DefaultUncordonTimeout (90s)
//	  + DefaultReadinessGateTimeout (4m) + MaxUncordonRetryInterval (10s) + 1m = 7m10s
//
// The test in this package computes the number from the fsm constants, so growing a deadline
// without growing the budget fails, and the constants are not copied.
func AttemptBudget() time.Duration {
	return fsm.JobMonitorPollInterval +
		fsm.DefaultUncordonTimeout +
		fsm.DefaultReadinessGateTimeout +
		fsm.MaxUncordonRetryInterval +
		time.Minute
}

// DeadlineAt is spec.deadlineAt for a request made at requestedAt for a group with the given
// spec.maxNodeProvisionTime: the operator's budget, floored by the group's number.
//
// The floor is not decoration. cluster-autoscaler is told the group's number as its own scale-up
// termination deadline (NodeGroupGetOptions -> MaxNodeProvisionDuration), so it gives up on the
// slot at that instant; enforcing the shorter of the two lets the provider declare a claim dead
// while the autoscaler is still waiting for it, and the autoscaler's answer to a group that
// stopped growing is another IncreaseSize.
func DeadlineAt(requestedAt time.Time, groupMaxNodeProvisionTime *metav1.Duration) time.Time {
	budget := AttemptBudget()
	if groupMaxNodeProvisionTime != nil && groupMaxNodeProvisionTime.Duration > budget {
		budget = groupMaxNodeProvisionTime.Duration
	}

	return requestedAt.Add(budget)
}

// Budget returns the group's maxNodeProvisionTime in force for a claim, i.e. the floor of the
// claim's deadline, so the number can be copied onto the claim for reporting.
func Budget(group *infrav2alpha1.Group) *metav1.Duration {
	if group == nil || group.Spec.MaxNodeProvisionTime == nil {
		return nil
	}

	d := *group.Spec.MaxNodeProvisionTime

	return &d
}

// SeedAttempt is the attempt number a new claim of a slot carries: the slot's last attempt + 1
// while the slot keeps failing, and 1 again once an attempt has landed.
//
// "A bound on attempts is a bound on consecutive claims" (docs/design/nodeclaim.md §4), and the
// digest's phase is the record of what the last one did: an episode that ended in ready or
// completed bought a machine the pool could use, so the next request of that slot starts a new
// streak. That is the whole reset rule - it reads the object that ends with the episode, so
// nothing on the slot accumulates and no slot is sentenced for the failures of its life.
func SeedAttempt(last *infrav2alpha1.LastClaimSummary) int32 {
	if last == nil {
		return 1
	}

	if last.Attempt < 1 {
		return 1
	}

	switch last.Phase {
	case infrav2alpha1.NodeClaimReady, infrav2alpha1.NodeClaimCompleted:
		return 1
	default:
		return last.Attempt + 1
	}
}

// New builds a claim for a slot: attribution, deadline, seeded attempt, labels, and an
// ownerReference on the slot so that deleting a slot from Git takes its claims with it.
//
// attempt is not set here: the claim controller seeds status.attempt on the claim's first pass
// (see SeedAttempt) because the seeding reads the slot's digest, and a claim created from a stale
// cache must not bake a number in that the controller cannot correct.
func New(slot *infrav2alpha1.Node, group *infrav2alpha1.Group, req infrav2alpha1.ClaimRequest,
	requestedBy string, scheme *runtime.Scheme,
) (*infrav2alpha1.NodeClaim, error) {
	requestedAt := time.Now()
	ttl := DefaultTTLSecondsAfterFinished

	c := &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", slot.Name, rand.String(5)),
			Namespace: slot.Namespace,
			Labels: map[string]string{
				LabelSlot:  slot.Name,
				LabelGroup: req.Group,
			},
		},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef: corev1.ObjectReference{
				APIVersion: infrav2alpha1.GroupVersion.String(),
				Kind:       "Node",
				Namespace:  slot.Namespace,
				Name:       slot.Name,
				UID:        slot.UID,
			},
			RequestedAt:             metav1.NewTime(requestedAt),
			RequestedBy:             requestedBy,
			Request:                 req,
			DeadlineAt:              deadlineAt(requestedAt, groupMaxOf(req, group)),
			MaxNodeProvisionTime:    Budget(group),
			TTLSecondsAfterFinished: &ttl,
		},
	}

	if err := controllerutil.SetControllerReference(slot, c, scheme); err != nil {
		return nil, fmt.Errorf("setting the ownerReference of the claim of slot %s: %w", slot.Name, err)
	}

	return c, nil
}

// deadlineAt is spec.deadlineAt: the operator's budget, floored by the group's number, measured
// from the moment the request was made.
func deadlineAt(requestedAt time.Time, groupMax *metav1.Duration) *metav1.Time {
	out := metav1.NewTime(DeadlineAt(requestedAt, groupMax))

	return &out
}

// groupMaxOf returns the group's spec.maxNodeProvisionTime. The group is looked up by the caller
// that needs it for arithmetic; a claim whose group cannot be read keeps the operator budget alone,
// which is the shorter of the two and the one the design floors with the group's value when it has
// one.
func groupMaxOf(req infrav2alpha1.ClaimRequest, group *infrav2alpha1.Group) *metav1.Duration {
	if group == nil {
		return nil
	}

	return Budget(group)
}

// Digest is the one-line record of a claim, copied onto Node.status.lastClaim so that
// `kubectl describe node.infra.homecluster.dev <slot>` still tells the story after the claim has
// been collected by its TTL. The GC is safe because the summary survives it.
func Digest(c *infrav2alpha1.NodeClaim) *infrav2alpha1.LastClaimSummary {
	if c == nil {
		return nil
	}

	summary := &infrav2alpha1.LastClaimSummary{
		Name:       c.Name,
		Phase:      c.Status.Phase,
		Attempt:    c.Status.Attempt,
		Call:       c.Spec.Request.Call,
		Group:      c.Spec.Request.Group,
		Reason:     c.Status.Reason,
		DeadlineAt: c.Spec.DeadlineAt,
		FinishedAt: c.Status.FinishedAt,
	}

	if !c.Spec.RequestedAt.IsZero() {
		requested := c.Spec.RequestedAt
		summary.RequestedAt = &requested
	}

	return summary
}

// IsOverdue reports whether a claim's deadline has passed.
func IsOverdue(c *infrav2alpha1.NodeClaim, now time.Time) bool {
	if c == nil || c.Spec.DeadlineAt == nil {
		return false
	}

	return c.Spec.DeadlineAt.Time.Before(now)
}

// SlotNameOf is the slot (Node CR name) a claim asks for. spec.slotRef.name is the field; the
// label is the index, and the two must agree, so the webhook reads this helper.
func SlotNameOf(c *infrav2alpha1.NodeClaim) string {
	return c.Spec.SlotRef.Name
}

// SlotLabel returns the label selector that lists the claims of one slot.
func SlotLabel(slotName string) client.ListOptions {
	return client.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{LabelSlot: slotName}),
	}
}

// SlotKey is the name of the field index the claim controller registers, so a watch-driven
// reconcile can list a slot's claims without scanning the namespace.
const SlotKeyField = "spec.slotRef.name"

// IndexSlot is the field indexer function for SlotKeyField.
func IndexSlot(rawObj client.Object) []string {
	c, ok := rawObj.(*infrav2alpha1.NodeClaim)
	if !ok {
		return nil
	}

	name := SlotNameOf(c)
	if name == "" {
		return nil
	}

	return []string{name}
}

// ListOfSlot returns the claims of one slot, newest first.
func ListOfSlot(ctx context.Context, c client.Client, namespace, slotName string) ([]infrav2alpha1.NodeClaim, error) {
	list := &infrav2alpha1.NodeClaimList{}
	if err := c.List(ctx, list, client.InNamespace(namespace),
		&client.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{LabelSlot: slotName})}); err != nil {
		return nil, fmt.Errorf("listing the claims of slot %s: %w", slotName, err)
	}

	return list.Items, nil
}

// LiveOfSlot returns the live claim of a slot: pending, booting or ready, and not deleted. When
// more than one is live the newest is the claim the slot answers for and the rest are extras the
// claim controller fails (one live claim per slot is load-bearing: it is what makes "a live claim
// exists" a synonym for "the machine is wanted").
func LiveOfSlot(claims []infrav2alpha1.NodeClaim) *infrav2alpha1.NodeClaim {
	var best *infrav2alpha1.NodeClaim

	for i := range claims {
		c := &claims[i]
		if !phase.IsLive(c.Status.Phase) || !c.DeletionTimestamp.IsZero() {
			continue
		}

		if best == nil || c.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = c
		}
	}

	return best
}
