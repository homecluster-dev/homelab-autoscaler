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

package nodelabels

import (
	"context"
	"encoding/json"
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

// The reason and action recorded on the Node CR around this write, so that
// `kubectl describe node.infra.homecluster.dev <name>` answers "did the labels of this slot land,
// and what did the controller change" without reading the node.
const (
	// ReasonApplied is the Normal event of one write that happened: which keys were added,
	// rewritten and removed.
	ReasonApplied = "NodeLabelsApplied"
	// ReasonSkippedReservedKey is the Warning event for a declared key in a namespace an
	// operator owns, which the controller refused to write. The webhook refuses the same keys at
	// admission; this is the record for an object that predates the rule, or that was created on a
	// cluster with the webhook switched off.
	ReasonSkippedReservedKey = "SkippedReservedNodeLabelKey"
	// ActionApplyNodeLabels is the action both events belong to.
	ActionApplyNodeLabels = "ApplyNodeLabels"
)

// ApplyToKubernetesNode writes the labels and annotations a Node CR declares
// (spec.nodeLabels, spec.nodeAnnotations) onto the corev1.Node of that slot.
//
// It is the one implementation of that write, and it is called twice over, deliberately:
// the node reconciler on every pass, and the scale-up readiness hook immediately before the node
// is uncordoned. The second call is the ordering that matters: a node that becomes schedulable
// before its zone label exists is a node a zoned pod is bound to and then evicted from, and the
// autoscaler's simulation placed that pod on a template that carries the label.
//
// The rules are Reconcile's, restated for the caller:
//   - a declared key lands on the node, and drift is put back: a value changed by hand, a label
//     removed by a reboot of a kubelet that stamps its own set, a node object recreated by a
//     re-registration - the next pass re-asserts it;
//   - a key the slot's declaration drops is removed from the node, because the controller records
//     the keys it wrote (AppliedLabelsAnnotation, AppliedAnnotationsAnnotation) and owns exactly
//     those;
//   - a key the slot never declared is never touched. The kubelet's labels, a cloud
//     integration's, another tool's, are not this field's business;
//   - a declared key in a reserved namespace is skipped, and the skip is surfaced with a Warning
//     event, not swallowed;
//   - the write is one JSON merge patch carrying only the keys that differ, so an
//     unchanged resync sends nothing, and a patch that names only its own keys cannot clobber a
//     concurrent write of a key it did not touch.
//
// One Normal Event with reason ReasonApplied is recorded per write that happened, naming the keys
// in each direction, and one Info log line says the same. A write that changed nothing records
// nothing.
//
// A missing node object is not a failure: the machine has not registered yet, and the labels land
// when it does. recorder may be nil, which records nothing (the unit tests do that).
func ApplyToKubernetesNode(ctx context.Context, c client.Client, slot *infrav2alpha1.Node, recorder events.EventRecorder) error {
	logger := log.FromContext(ctx).WithValues("nodeCR", slot.Name)

	if slot.Spec.KubernetesNodeName == "" {
		// The slot has never been provisioned, so there is no node object to write to.
		return nil
	}

	kubernetesNode := &corev1.Node{}
	if err := c.Get(ctx, types.NamespacedName{Name: slot.Spec.KubernetesNodeName}, kubernetesNode); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("Kubernetes node not found, skipping declared labels",
				"kubernetesNode", slot.Spec.KubernetesNodeName)

			return nil
		}

		return fmt.Errorf("get kubernetes node %s: %w", slot.Spec.KubernetesNodeName, err)
	}

	declaredLabels, skippedLabels := dropReserved(slot.Spec.NodeLabels, ReservedLabelKey)
	declaredAnnotations, skippedAnnotations := dropReserved(slot.Spec.NodeAnnotations, ReservedAnnotationKey)

	if len(skippedLabels)+len(skippedAnnotations) > 0 && recorder != nil {
		note := skippedKeysNote(slot.Name, skippedLabels, skippedAnnotations)

		// V(1) plus an Event, the same two channels as the taints package: this runs on every
		// reconcile, and the Event is where an operator looks.
		logger.V(1).Info("Ignoring declared keys with namespaces their owner controls",
			"kubernetesNode", slot.Spec.KubernetesNodeName, "note", note)

		recorder.Eventf(slot, nil, corev1.EventTypeWarning, ReasonSkippedReservedKey, ActionApplyNodeLabels, "%s", note)
	}

	ownedLabels := DecodeOwnerRecord(kubernetesNode.Annotations[AppliedLabelsAnnotation])
	ownedAnnotations := DecodeOwnerRecord(kubernetesNode.Annotations[AppliedAnnotationsAnnotation])

	labels, labelChange := Reconcile(kubernetesNode.Labels, declaredLabels, ownedLabels)
	annotations, annotationChange := Reconcile(kubernetesNode.Annotations, declaredAnnotations, ownedAnnotations)

	// The owner records are part of the write: they are what the next pass removes.
	recordLabels := EncodeOwnerRecord(DeclaredKeys(declaredLabels))
	recordAnnotations := EncodeOwnerRecord(DeclaredKeys(declaredAnnotations))

	recordChange := recordDiff(kubernetesNode.Annotations, AppliedLabelsAnnotation, recordLabels) +
		recordDiff(kubernetesNode.Annotations, AppliedAnnotationsAnnotation, recordAnnotations)

	if labelChange.Empty() && annotationChange.Empty() && recordChange == 0 {
		// The node is what the slot declares. Write nothing, record nothing: this is the state of
		// every pass of every settled slot, and a patch per resync is churn on a hot object.
		return nil
	}

	patch, err := buildPatch(labels, labelChange, annotations, annotationChange,
		kubernetesNode.Annotations, recordLabels, recordAnnotations)
	if err != nil {
		return fmt.Errorf("build node labels patch: %w", err)
	}

	if err := c.Patch(ctx, kubernetesNode, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("patch kubernetes node %s with declared labels: %w", slot.Spec.KubernetesNodeName, err)
	}

	note := ChangeNote(labelChange, annotationChange)

	// Info, not V(1): this line fires only when a write happened, so it is the audit trail of
	// "when did the controller stamp this machine", not per-resync noise.
	logger.Info("Successfully patched Kubernetes node with declared labels",
		"kubernetesNode", slot.Spec.KubernetesNodeName,
		"labels", len(declaredLabels),
		"annotations", len(declaredAnnotations),
		"change", note)

	if recorder != nil {
		recorder.Eventf(slot, nil, corev1.EventTypeNormal, ReasonApplied, ActionApplyNodeLabels, "%s",
			fmt.Sprintf("node %s: %s", slot.Spec.KubernetesNodeName, note))
	}

	return nil
}

// dropReserved filters a declaration of the keys in a namespace an operator owns, and reports what
// was dropped. The webhook refuses the same keys; this is the second line of defence for an object
// that predates the rule, and the reason the reserved list is a property of the write and not only
// of admission.
func dropReserved(declared map[string]string, isReserved func(string) (bool, string)) (map[string]string, []string) {
	if len(declared) == 0 {
		return nil, nil
	}

	kept := make(map[string]string, len(declared))

	var skipped []string

	for _, key := range sortedKeys(declared) {
		if reserved, _ := isReserved(key); reserved {
			skipped = append(skipped, key)

			continue
		}

		kept[key] = declared[key]
	}

	return kept, skipped
}

// recordDiff counts a record annotation that has to change: 1 when the record on the node object
// and the record the declaration implies differ, 0 when they agree. It is counted, not returned as
// a Change, because the records are the controller's bookkeeping and not the operator's keys: an
// Event that announced "annotations: ~infra.homecluster.dev/applied-node-labels" would report the
// controller's own housekeeping as if the slot had asked for it.
func recordDiff(annotations map[string]string, key, want string) int {
	if annotations[key] == want {
		return 0
	}

	return 1
}

// metadataPatch is the merge-patch shape of the write. A nil *string renders as JSON null, which is
// how a JSON merge patch deletes a key, and only the keys that differ are in the body, so a node
// object other controllers are writing concurrently is untouched everywhere else.
type metadataPatch struct {
	Labels      map[string]*string `json:"labels,omitempty"`
	Annotations map[string]*string `json:"annotations,omitempty"`
}

type nodePatch struct {
	Metadata metadataPatch `json:"metadata"`
}

// buildPatch renders the write from the two Reconcile results.
//
// The values come from the merged maps, not from the declaration, so a key the node already
// carried with the declared value is never in the body, and the record annotations are set here
// (to null when the slot declares nothing, which is the state after the last of a slot's keys was
// removed).
func buildPatch(
	labels map[string]string, labelChange Change,
	annotations map[string]string, annotationChange Change,
	existingAnnotations map[string]string,
	recordLabels, recordAnnotations string,
) ([]byte, error) {
	patch := nodePatch{Metadata: metadataPatch{
		Labels:      map[string]*string{},
		Annotations: map[string]*string{},
	}}

	for key := range labelChange.addedAndUpdated() {
		value := labels[key]
		patch.Metadata.Labels[key] = &value
	}

	for _, key := range labelChange.Removed {
		patch.Metadata.Labels[key] = nil
	}

	for key := range annotationChange.addedAndUpdated() {
		value := annotations[key]
		patch.Metadata.Annotations[key] = &value
	}

	for _, key := range annotationChange.Removed {
		patch.Metadata.Annotations[key] = nil
	}

	for _, record := range []struct {
		key  string
		want string
	}{{AppliedLabelsAnnotation, recordLabels}, {AppliedAnnotationsAnnotation, recordAnnotations}} {
		if existingAnnotations[record.key] == record.want {
			continue
		}

		if record.want == "" {
			// Nothing is declared any more: the record's only job was to say what to remove, it
			// has removed it, and an empty annotation on every node of every idle pool is litter.
			patch.Metadata.Annotations[record.key] = nil

			continue
		}

		value := record.want
		patch.Metadata.Annotations[record.key] = &value
	}

	if len(patch.Metadata.Labels) == 0 {
		patch.Metadata.Labels = nil
	}

	if len(patch.Metadata.Annotations) == 0 {
		patch.Metadata.Annotations = nil
	}

	return json.Marshal(patch)
}

// addedAndUpdated is the set of keys a write puts a value on the object for, as a set so a caller
// iterates each key once.
func (c Change) addedAndUpdated() map[string]struct{} {
	keys := make(map[string]struct{}, len(c.Added)+len(c.Updated))
	for _, key := range c.Added {
		keys[key] = struct{}{}
	}

	for _, key := range c.Updated {
		keys[key] = struct{}{}
	}

	return keys
}

// skippedKeysNote renders the Warning event: which slot, which keys, and why they were dropped.
func skippedKeysNote(nodeName string, labels, annotations []string) string {
	return fmt.Sprintf("spec.nodeLabels / spec.nodeAnnotations of node %s declare keys in namespaces "+
		"their owner controls, they are not applied: %s. Kubernetes, a CNI, Argo CD, kubectl or this "+
		"operator adds and removes them; declare a key you own, or topology.kubernetes.io/zone and "+
		"topology.kubernetes.io/region.",
		nodeName, joinKeys(labels, annotations))
}

func joinKeys(labels, annotations []string) string {
	joined := append(append([]string(nil), labels...), annotations...)

	return strings.Join(joined, ", ")
}
