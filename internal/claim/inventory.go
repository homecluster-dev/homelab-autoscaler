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

package claim

import (
	"context"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// ListInNamespace lists every claim in a namespace.
func ListInNamespace(ctx context.Context, c client.Client, namespace string) ([]infrav2alpha1.NodeClaim, error) {
	list := &infrav2alpha1.NodeClaimList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	return list.Items, nil
}

// ListInGroup lists every claim created for one node group. The claims carry the group as a label,
// so a pool's requests are one selector, and `kubectl get nodeclaim -l group=<group>` answers the
// same question an operator would otherwise reconstruct from three slot status fields.
func ListInGroup(ctx context.Context, c client.Client, namespace, group string) ([]infrav2alpha1.NodeClaim, error) {
	list := &infrav2alpha1.NodeClaimList{}
	if err := c.List(ctx, list, client.InNamespace(namespace),
		&client.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{LabelGroup: group})}); err != nil {
		return nil, err
	}

	return list.Items, nil
}

// CountsTowardTargetSize reports whether a claim is a unit of the target size cluster-autoscaler is
// paying for: a live claim (pending, booting, ready) that no scale-down has named.
//
// This replaces countsTowardsTargetSize of the v1alpha1 provider, which read a slot's
// spec.desiredPowerState and its status.progress, and which is the reason the arithmetic was
// auditable by nobody. No progress value appears here.
//
// A claim marked for deletion is not counted: it is capacity that is going away, and the number
// cluster-autoscaler reads back after a DeleteNodes must fall on the write that asked for it, the
// same instant a pending claim's deletion takes its unit off. A failed claim is not counted either,
// which is what removes the state "the autoscaler is paying for a machine that never arrived,
// forever" from the system: the claim hits its deadline, stops being counted, force-shuts the
// machine, and the slot is free.
func CountsTowardTargetSize(c *infrav2alpha1.NodeClaim) bool {
	if c == nil || !phase.IsLive(c.Status.Phase) {
		return false
	}

	return c.DeletionTimestamp.IsZero()
}

// TargetSize is the number of live claims: the target size of a group, and of the whole provider.
func TargetSize(claims []infrav2alpha1.NodeClaim) int32 {
	var size int32

	for i := range claims {
		if CountsTowardTargetSize(&claims[i]) {
			size++
		}
	}

	return size
}

// ClaimedSlots is the set of slots that carry a live claim, including one marked for deletion: a
// slot with a claim in flight is not free, whatever the claim's phase arithmetic says about the
// target size.
func ClaimedSlots(claims []infrav2alpha1.NodeClaim) map[string]bool {
	claimed := make(map[string]bool, len(claims))

	for i := range claims {
		c := &claims[i]
		if phase.IsLive(c.Status.Phase) {
			claimed[SlotNameOf(c)] = true
		}
	}

	return claimed
}

// IsFree reports whether a slot has no live claim, i.e. whether IncreaseSize may claim it. In v1
// the same test was a three-field scan ({powerState: off, status.powerState: off, status.progress:
// shutdown}) plus a controller seeding the status of slots that never had a machine; "no live
// claim" needs no seeding, no status, and no observation.
func IsFree(slotName string, claims []infrav2alpha1.NodeClaim) bool {
	for i := range claims {
		if SlotNameOf(&claims[i]) == slotName && phase.IsLive(claims[i].Status.Phase) {
			return false
		}
	}

	return true
}

// FreeSlots returns the slots no live claim names, sorted by name so that a burst of IncreaseSize
// calls in one autoscaler loop claims them in a stable order.
func FreeSlots(nodes []infrav2alpha1.Node, claims []infrav2alpha1.NodeClaim) []infrav2alpha1.Node {
	claimed := ClaimedSlots(claims)
	free := make([]infrav2alpha1.Node, 0, len(nodes))

	for _, node := range nodes {
		if claimed[node.Name] {
			continue
		}

		free = append(free, node)
	}

	sort.Slice(free, func(i, j int) bool { return free[i].Name < free[j].Name })

	return free
}

// LiveOfGroup returns the live claims of a group, sorted by name.
func LiveOfGroup(claims []infrav2alpha1.NodeClaim, group string) []infrav2alpha1.NodeClaim {
	live := make([]infrav2alpha1.NodeClaim, 0, len(claims))

	for i := range claims {
		c := &claims[i]
		if c.Spec.Request.Group != group || !phase.IsLive(c.Status.Phase) {
			continue
		}

		live = append(live, claims[i])
	}

	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })

	return live
}

// PendingOfGroup returns the pending claims of a group, oldest first: the claims
// NodeGroupDecreaseTargetSize may cancel. A booting claim is committed and is never a cancel
// target, so it is not in this list.
func PendingOfGroup(claims []infrav2alpha1.NodeClaim, group string) []infrav2alpha1.NodeClaim {
	pending := make([]infrav2alpha1.NodeClaim, 0, len(claims))

	for i := range claims {
		c := &claims[i]
		if c.Spec.Request.Group != group || !phase.IsPending(c.Status.Phase) ||
			!c.DeletionTimestamp.IsZero() {
			continue
		}

		pending = append(pending, claims[i])
	}

	sort.Slice(pending, func(i, j int) bool {
		if !pending[i].CreationTimestamp.Equal(&pending[j].CreationTimestamp) {
			return pending[i].CreationTimestamp.Before(&pending[j].CreationTimestamp)
		}

		return pending[i].Name < pending[j].Name
	})

	return pending
}

// OfSlotNamed returns the claims that name one Kubernetes node name in a DeleteNodes request, i.e.
// the claims of the slots the request names. The slot's name is its identity in the provider
// (NodeGroupDeleteNodes looks the CR up by name, and the providerID is homelab://<slot>), so the
// mapping from a request's node names to slots is: a slot whose name, or spec.kubernetesNodeName,
// matches.
func OfSlotNamed(claims []infrav2alpha1.NodeClaim, slot *infrav2alpha1.Node) []infrav2alpha1.NodeClaim {
	out := make([]infrav2alpha1.NodeClaim, 0, 1)

	for i := range claims {
		if SlotNameOf(&claims[i]) == slot.Name && phase.IsLive(claims[i].Status.Phase) {
			out = append(out, claims[i])
		}
	}

	return out
}

// ScaleDownStreakExhausted reports that a slot has used the scale-down attempts its digest carries,
// i.e. that the model stops firing the shutdown of a machine that will not go off, and returns how
// long is left of the reset grace. The bound is the same claim.MaxConsecutiveAttempts the boot
// attempts are bounded by, and it is read off the record of the last claim, which the claim
// controller keeps in step with the attempts it spent: the v1alpha1 ShutdownAttempts condition on the
// long-lived slot, and its two reset sites, are gone (docs/development/nodeclaim.md §4).
//
// This is the case with no claim left to carry the count: the machine was left up by an episode that
// has already been collected. The digest is the record, so the streak and its clock are read from it,
// and the grace is what makes the pause bounded here too.
func ScaleDownStreakExhausted(slot *infrav2alpha1.Node) (bool, time.Duration) {
	digest := slot.Status.LastClaim
	if digest == nil || digest.ShutdownAttempts < MaxConsecutiveAttempts {
		return false, 0
	}

	if digest.Call != infrav2alpha1.ClaimCallDeleteNodes && digest.Call != infrav2alpha1.ClaimCallDecreaseTargetSize {
		return false, 0
	}

	if !phase.IsTerminal(digest.Phase) {
		return false, 0
	}

	last := time.Time{}
	if digest.LastShutdownAttemptAt != nil {
		last = digest.LastShutdownAttemptAt.Time
	}

	if last.IsZero() {
		last = time.Now()
	}

	left := ShutdownAttemptResetGrace - time.Since(last)
	if left <= 0 {
		return false, 0
	}

	return true, left
}
