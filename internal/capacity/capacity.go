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

// Package capacity compares the machine shape a Node CR declares (spec.nodeCapacity) with the
// machine that actually registered.
//
// The declaration is what cluster-autoscaler simulates a scale-up on when the pool is at zero,
// and it is a copy of the kubelet's numbers, not something the controller can measure: a system
// reservation, a resized VM, a kernel that took a page table, all of them move what the node
// reports without touching the Node CR. A declaration that silently stops matching reality is
// worse than no declaration, because the autoscaler places work on a machine that cannot take it.
//
// This package makes the divergence visible and nothing else. It never writes a node: the
// kubelet owns status.allocatable, and a running machine is the truth about what booted.
package capacity

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// Reason and action recorded on the Node CR when a declaration stops matching the machine, so
// that `kubectl describe node.infra.homecluster.dev <name>` shows the drift. The reason is
// stable and greppable, the note carries the resources and both values.
const (
	// ReasonCapacityMismatch is recorded for a registered node whose status.allocatable
	// disagrees with spec.nodeCapacity beyond the tolerance.
	ReasonCapacityMismatch = "NodeCapacityMismatch"
	// ActionCheckCapacity is the action the warning belongs to.
	ActionCheckCapacity = "CheckDeclaredCapacity"
)

// driftTolerancePercent is how far a live node's allocatable may sit from the declaration before
// it is worth an operator's attention. It is not zero on purpose: the kubelet subtracts a system
// reservation that moves with the kernel, the container runtime and the image, so demanding an
// exact match would fire on healthy pools and teach people to ignore the event. Five percent is
// the point where a machine really changed shape - a 2 vCPU box reporting 1950m is inside it, a
// 2 vCPU box reporting 1 CPU is not.
const driftTolerancePercent = 5

// CheckDeclaredCapacity warns when the machine a slot booted is not the shape the slot declares.
//
// The node controller calls it on every reconcile. Semantics:
//   - no declaration, no check: a pool that declares nothing is on the live-node path and has
//     nothing declared to be stale about;
//   - a slot whose node object does not exist yet, and a node that is not Ready, are skipped:
//     status.allocatable is written by the kubelet, so before the kubelet reports it there is no
//     measurement to compare, and comparing an empty allocatable against a declaration would
//     warn about a machine that is booting;
//   - every declared resource is compared against the node's allocatable, and any that is
//     missing from it, zero in it, or more than driftTolerancePercent away from the declaration
//     is reported by name with both values;
//   - the result is one Warning event on the Node CR per reconcile that sees the drift, listing
//     every mismatched resource. Warning, not Normal: an operator scanning a pool that does not
//     scale reads the Type column of `kubectl describe`, and a stale declaration stops scaling.
//
// It is warn-only by design. status.allocatable belongs to the kubelet; patching it to a
// declaration is how you get a scheduler that believes a machine has memory it does not have.
// Nothing on the Node CR dedupes the event either: the event aggregation the API server applies
// (the same mechanism the SkippedProtectedTaintKey warning relies on) folds repeats of an
// identical reason on the same object, so a drift that persists for hours is one row with a
// count, not a page of events.
//
// recorder may be nil, which records nothing (unit tests and the bare-metal setups do that).
func CheckDeclaredCapacity(ctx context.Context, c client.Client, slot *infrav1alpha1.Node, recorder events.EventRecorder) error {
	logger := log.FromContext(ctx).WithValues("nodeCR", slot.Name)

	if len(slot.Spec.NodeCapacity) == 0 {
		return nil
	}

	if slot.Spec.KubernetesNodeName == "" {
		// The slot has never been provisioned, so there is nothing registered to compare against.
		return nil
	}

	kubernetesNode := &corev1.Node{}
	if err := c.Get(ctx, types.NamespacedName{Name: slot.Spec.KubernetesNodeName}, kubernetesNode); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("Kubernetes node not found, skipping declared capacity check",
				"kubernetesNode", slot.Spec.KubernetesNodeName)

			return nil
		}

		return fmt.Errorf("get kubernetes node %s: %w", slot.Spec.KubernetesNodeName, err)
	}

	if !nodeIsReady(kubernetesNode) {
		// Allocatable is the kubelet's number, and an unready kubelet has not posted a current
		// one. A machine on its way up must not be reported as a stale declaration.
		return nil
	}

	mismatched := Mismatches(slot.Spec.NodeCapacity, kubernetesNode.Status.Allocatable)
	if len(mismatched) == 0 {
		return nil
	}

	details := strings.Join(mismatched, "; ")

	// Two channels, on purpose, matching the declared-taints warning: V(1) because this runs on
	// every reconcile and an Info line per resync is spam, the Warning event because
	// `kubectl describe node.infra...` is where an operator looks when a pool does not scale.
	logger.V(1).Info("Node reports an allocatable that disagrees with the declared node capacity",
		"kubernetesNode", slot.Spec.KubernetesNodeName, "resources", details)

	if recorder != nil {
		recorder.Eventf(slot, nil, corev1.EventTypeWarning, ReasonCapacityMismatch, ActionCheckCapacity,
			"%s", mismatchNote(slot.Name, details))
	}

	return nil
}

// Mismatches compares a declaration against what a node reports and returns one entry per
// resource that drifted. Only declared resources are compared: the declaration is the contract
// the operator wrote, and a node reports a dozen resources (pods, ephemeral-storage, whatever a
// device plugin adds) that nobody declared, so walking the node's list would warn about the shape
// of every machine.
//
// A declared resource the node does not report at all, or reports as zero, is a mismatch: the
// autoscaler has been told the machine has it.
//
// The resources are named in sorted order, which is what keeps the event stream folded: the
// API server's aggregation merges events with an identical message, and a note built by ranging
// over a map renders the same drift as a different sentence on every pass.
func Mismatches(declared, live corev1.ResourceList) []string {
	names := make([]corev1.ResourceName, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}

	slices.Sort(names)

	var mismatched []string

	for _, name := range names {
		want := declared[name]
		got, reported := live[name]

		switch {
		case !reported:
			mismatched = append(mismatched, fmt.Sprintf("%s: declared %s, the node does not report it",
				name, want.String()))
		case !withinTolerance(want, got):
			mismatched = append(mismatched, fmt.Sprintf("%s: declared %s, the node reports %s",
				name, want.String(), got.String()))
		}
	}

	return mismatched
}

// withinTolerance reports whether a live quantity sits within driftTolerancePercent of the
// declared one, measured against the declared value, which is the reference the operator wrote.
//
// The comparison runs on milli values, an int64, so it is exact: turning a Quantity into a float
// is how 1950m starts looking like 1949.999999m and a tolerance check fires on rounding.
func withinTolerance(declared, live resource.Quantity) bool {
	want, got := declared.MilliValue(), live.MilliValue()

	if want < 0 {
		want = -want
	}

	if want == 0 {
		// Zero has no percentage of itself, so anything that is not also zero is a real
		// disagreement. Declaring zero is not a machine shape worth simulating anyway.
		return got == 0
	}

	difference := want - got
	if difference < 0 {
		difference = -difference
	}

	// the comparison stays in integers: difference*100 <= tolerance*|want| is the same
	// predicate as "the gap is at most five percent of the declared value", and it has no rounding
	return difference*100 <= int64(driftTolerancePercent)*want
}

// nodeIsReady reports whether the kubelet of a Kubernetes node is posting a ready status. The
// same rule the provider applies when it picks the template source, restated here because the
// two packages must not import each other: a node with no Ready condition at all is not ready.
func nodeIsReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

// mismatchNote renders the event message: which slot, which resources, both values, and what to
// do about it.
func mismatchNote(nodeName, details string) string {
	return fmt.Sprintf("status.allocatable of node %s disagrees with spec.nodeCapacity by more than %d%%: %s. "+
		"The declaration is the shape cluster-autoscaler simulates a scale-up on, so a stale one sizes the "+
		"machine wrong: you over-provision work the booted node cannot take, or leave it idle. The kubelet "+
		"owns the reported values: update spec.nodeCapacity to what the machine really reports, or fix the "+
		"machine.", nodeName, driftTolerancePercent, details)
}
