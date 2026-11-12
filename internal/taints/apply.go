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

package taints

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// Reason and action recorded on the Node CR when a declaration is dropped, so that
// `kubectl describe node.infra.homecluster.dev <name>` shows the taint the controller refused
// to write. The reason is stable and greppable, the note carries the keys.
const (
	// ReasonSkippedProtectedKey is recorded for a taint in spec.nodeTaints whose key is owned
	// by a Kubernetes component, so the declaration was dropped.
	ReasonSkippedProtectedKey = "SkippedProtectedTaintKey"
	// ActionApplyTaints is the action the skipped write belongs to.
	ActionApplyTaints = "ApplyTaints"
)

// ApplyToKubernetesNode stamps the taints a Node CR declares (spec.nodeTaints) onto the
// corev1.Node of that slot, with the Get + merge patch shape the label and providerID helpers
// use. The node controller calls it on every reconcile and the FSM calls it on the scale-up
// path before it uncordons the node, so the machine is tainted before a pod can be bound to it.
// It is the only implementation of that write: the two paths must not drift apart, because a
// taint that lands differently per path is a taint the autoscaler cannot predict.
//
// Semantics (Merge holds the merge rules):
//   - a declared taint is added, and an existing taint with the same key is overwritten;
//   - taints that are not declared are never removed;
//   - a key a Kubernetes component owns (IsProtectedKey) is skipped, and the skip is surfaced,
//     not swallowed: a V(1) log plus a Warning event on the Node CR. A declaration that
//     disappears silently is the failure this field exists to remove.
//
// It is idempotent: when the node already carries every declared taint nothing is patched, so
// the controller does not churn the node object on every resync, and the FSM can call it on
// every retry of the uncordon loop.
//
// A missing node object is not a failure: the machine has not registered yet, and the taints
// land when it does. recorder may be nil, which records nothing (unit tests do that).
func ApplyToKubernetesNode(ctx context.Context, c client.Client, slot *infrav2alpha1.Node, recorder events.EventRecorder) error {
	logger := log.FromContext(ctx).WithValues("nodeCR", slot.Name)

	if len(slot.Spec.NodeTaints) == 0 {
		return nil
	}

	if slot.Spec.KubernetesNodeName == "" {
		// The slot has never been provisioned, so there is no node to taint.
		return nil
	}

	kubernetesNode := &corev1.Node{}
	if err := c.Get(ctx, types.NamespacedName{Name: slot.Spec.KubernetesNodeName}, kubernetesNode); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("Kubernetes node not found, skipping declared taints",
				"kubernetesNode", slot.Spec.KubernetesNodeName)

			return nil
		}

		return fmt.Errorf("get kubernetes node %s: %w", slot.Spec.KubernetesNodeName, err)
	}

	desired, changed, skipped := Merge(kubernetesNode.Spec.Taints, slot.Spec.NodeTaints)

	if len(skipped) > 0 {
		// Two channels, on purpose. V(1) because this runs on every reconcile and an Info line
		// per resync is spam; the Warning event because `kubectl describe node.infra...` is
		// where an operator looks when a pool does not behave.
		keys := strings.Join(skipped, ", ")
		logger.V(1).Info("Ignoring declared taints with keys their owner controls",
			"kubernetesNode", slot.Spec.KubernetesNodeName, "taints", keys)

		if recorder != nil {
			recorder.Eventf(slot, nil, corev1.EventTypeWarning, ReasonSkippedProtectedKey, ActionApplyTaints,
				"%s", skippedKeysNote(slot.Name, keys))
		}
	}

	if !changed {
		return nil
	}

	patch := client.MergeFrom(kubernetesNode.DeepCopy())

	kubernetesNode.Spec.Taints = desired

	if err := c.Patch(ctx, kubernetesNode, patch); err != nil {
		return fmt.Errorf("patch kubernetes node %s with declared taints: %w", slot.Spec.KubernetesNodeName, err)
	}

	// Info, not V(1): this line only fires when a write actually happened, so it is the audit
	// trail of "when did the controller taint this machine", not per-resync noise.
	logger.Info("Successfully patched Kubernetes node with declared taints",
		"kubernetesNode", slot.Spec.KubernetesNodeName, "taints", len(slot.Spec.NodeTaints))

	return nil
}

// skippedKeysNote renders the event message: which slot, which keys, and why they were dropped.
func skippedKeysNote(nodeName, keys string) string {
	return fmt.Sprintf("spec.nodeTaints of node %s declares keys owned by Kubernetes, they are not "+
		"applied: %s. Their owner (the node lifecycle controllers, the kubelet, cluster-autoscaler) "+
		"adds and removes them on its own; declare a key you own instead.", nodeName, keys)
}
