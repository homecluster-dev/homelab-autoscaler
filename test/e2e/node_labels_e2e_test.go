//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/homecluster-dev/homelab-autoscaler/internal/nodelabels"
	"github.com/homecluster-dev/homelab-autoscaler/test/utils"
)

// The helpers of the "Scheduling With Slot Labels" spec, which lives in k3d_integration_test.go
// inside the Ordered K3d Integration container: the spec has to run after the scale-up spec,
// because that is where the pool has a Ready, uncordoned, served machine, and Ginkgo randomizes the
// order of top-level containers (a container of its own boots on a pool the suite left powered off,
// and paying for a boot in a second container does not fit the suite's `go test` budget).
//
// Everything here is a read or a write of a slot's declaration and of the node object it lands on,
// typed against the API rather than against kubectl output strings, and silent on failure paths: a
// helper that returns the zero value lets an Eventually poll, and the gate above it is the sentence
// about the feature.

// patchSlotLabels writes spec.nodeLabels and spec.nodeAnnotations of a slot, the way a Git apply
// does: a merge patch that replaces the two maps. A nil map is sent as null, which removes the
// field, which is the "operator deleted the key from the file" case the apply has to follow.
func patchSlotLabels(slot string, labels, annotations map[string]string) error {
	body := map[string]any{}
	body["spec"] = map[string]any{"nodeLabels": labels, "nodeAnnotations": annotations}

	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	_, err = utils.Run(kubectl("patch", "nodes.infra.homecluster.dev", slot, "-n", namespace,
		"--type=merge", "-p", string(payload)))

	return err
}

// kubeNodeLabels reads the labels of the Kubernetes node of a slot.
func kubeNodeLabels(nodeName string) map[string]string {
	out, err := utils.Run(kubectl("get", "node", nodeName, "-o", "json"))
	if err != nil {
		return nil
	}

	node := &corev1.Node{}
	if err := json.Unmarshal([]byte(out), node); err != nil {
		return nil
	}

	return node.Labels
}

// kubeNodeAnnotations reads the annotations of the Kubernetes node of a slot, including the
// operator's record of the keys a slot wrote.
func kubeNodeAnnotations(nodeName string) map[string]string {
	out, err := utils.Run(kubectl("get", "node", nodeName, "-o", "json"))
	if err != nil {
		return nil
	}

	node := &corev1.Node{}
	if err := json.Unmarshal([]byte(out), node); err != nil {
		return nil
	}

	return node.Annotations
}

// slotEventsFor renders the Events published on one slot, the way this suite reads every cluster
// fact: `kubectl get events`, one list, and the selection done on the decoded object.
//
// The list is the namespace's, filtered on `involvedObject.kind == Node` and its name in Go, for
// the two reasons the claim reader states (claimEventReasons): the operator publishes
// events.k8s.io objects and the core/v1 Event that `kubectl get events` shows is the API server's
// projection of one, and the field selectors of the events registry are the API server's business,
// not something a suite should depend on. Reading `kubectl get events.k8s.io` is *not* the same
// read: kubectl parses that argument as a resource named `events` in version `k8s.io`, not as the
// `events.k8s.io` API group, and the new API's `regarding` reference is not a field of the core
// Event, so a decoder written against it sees nothing. The read returns its error: a helper that
// answers the zero value on a failed read turns a broken read into an assertion that can never
// pass, and the first CI run of this spec is the evidence.
//
// The rendered line is `reason message`, which is what `kubectl describe` prints for an Event, so a
// failing gate shows what the controller recorded about the write.
func slotEventsFor(slot string) (string, error) {
	out, err := utils.Run(kubectl("get", "events", "-n", namespace, "-o", "json"))
	if err != nil {
		return "", fmt.Errorf("list the events in %s: %w", namespace, err)
	}

	list := &corev1.EventList{}
	if err := json.Unmarshal([]byte(out), list); err != nil {
		return "", fmt.Errorf("decode the event list of slot %s: %w", slot, err)
	}

	lines := make([]string, 0, len(list.Items))

	for _, nodeEvent := range list.Items {
		// Kind "Node" and the slot's name is the slot's own record: every Event the operator
		// publishes about a slot is filed against it, and the reasons asserted here
		// (NodeLabelsApplied, SkippedReservedNodeLabelKey) are the operator's, not the kubelet's. The
		// Kubernetes node of the k3d pool shares its name with its slot, so the machine's own Events
		// can ride along in this text; they carry other reasons, and a gate that reads a reason never
		// mistakes them. Filtering on a field selector would depend on the events registry, which the
		// suite's claim reader already declines to do.
		if nodeEvent.InvolvedObject.Kind != "Node" || nodeEvent.InvolvedObject.Name != slot {
			continue
		}

		lines = append(lines, nodeEvent.Reason+" "+nodeEvent.Message)
	}

	return strings.Join(lines, "\n"), nil
}

// controllerLogFile is where `make test-e2e` starts the local manager: the controller runs on this
// host, with nohup redirecting its output here, exactly like the VM control server's request log the
// suite already reads. It is the record of every decision the operator made, and the only way a
// failing gate can say what the reconciler did to the object it asserts on.
const controllerLogFile = "/tmp/homelab-autoscaler.log"

// controllerLogNote renders the controller's recent lines about one slot, newest last. A gate that
// asserts a consequence of a reconcile has to show the reconcile when it fails: the labels the writer
// decided to apply, the record it read, the pass it skipped, all of it is in these lines.
func controllerLogNote(slot string) string {
	data, err := os.ReadFile(controllerLogFile)
	if err != nil {
		return "\n  controller log unreadable: " + err.Error()
	}

	var kept []string

	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, slot) {
			continue
		}

		kept = append(kept, "  "+line)
	}

	if len(kept) == 0 {
		return "\n  no controller log line mentions " + slot
	}

	const tail = 40
	if len(kept) > tail {
		kept = kept[len(kept)-tail:]
	}

	return "\n  controller, newest last:\n" + strings.Join(kept, "\n")
}

// describeNodeMetadata renders what the writer based its decision on, for the gates that assert the
// record and the removals built on it: the declaration the slot carries, and the three records on the
// node object - the keys a slot applied, the keys it annotated, and the keys it refused. A removal is
// keyed by the record, so a gate that asserts a key came off has to show the record when it fails.
func describeNodeMetadata(nodeName string) string {
	lines := []string{"node " + nodeName}

	labels := kubeNodeLabels(nodeName)
	if labels == nil {
		lines = append(lines, "  node object unreadable")
	} else {
		lines = append(lines, "  node label keys: "+strings.Join(sortedKeysOf(labels), ","))
	}

	// Every record of the writer is an annotation on the node object, and the labels it wrote are
	// listed in one of them. Reading a record out of the *labels* map is the kind of mistake that
	// turns a diagnostic into a lie, so all three are read from the annotations, once.
	annotations := kubeNodeAnnotations(nodeName)
	if annotations != nil {
		lines = append(lines, "  node annotation keys: "+strings.Join(sortedKeysOf(annotations), ","))

		for _, record := range []string{nodelabels.AppliedLabelsAnnotation,
			nodelabels.AppliedAnnotationsAnnotation, nodelabels.SkippedKeysAnnotation} {
			lines = append(lines, "  record "+record+"="+annotations[record])
		}
	}

	out, err := utils.Run(kubectl("get", "nodes.infra.homecluster.dev", nodeName, "-n", namespace,
		"-o", `jsonpath={.spec.nodeLabels} {.spec.nodeAnnotations}`))
	if err != nil {
		lines = append(lines, "  slot declaration unreadable: "+err.Error())
	} else {
		lines = append(lines, "  slot declares: "+strings.TrimSpace(out))
	}

	return "\n" + strings.Join(lines, "\n")
}

// slotDeclarationNote renders the slot's own declaration, the input of every write the suite asserts.
func slotDeclarationNote(slot string) string {
	out, err := utils.Run(kubectl("get", "nodes.infra.homecluster.dev", slot, "-n", namespace,
		"-o", `jsonpath={.spec.nodeLabels} {.spec.nodeAnnotations}`))
	if err != nil {
		return "  slot declaration unreadable: " + err.Error()
	}

	return "  slot declares: " + strings.TrimSpace(out)
}

// sortedKeysOf renders a map's keys in order, for a message that has to be comparable between runs.
func sortedKeysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// nodeLabelKeys reads the *keys* of a slot's Kubernetes node, and reports the failure of the read
// instead of answering with a nil map. It exists for the removal assertions: "the label is gone" is
// only a fact about a node object that was read, and a helper that returns a nil map on failure lets
// an unparseable, deleted or unreadable node object satisfy "it is not there".
func nodeLabelKeys(nodeName string) ([]string, error) {
	return nodeMetadataKeys(nodeName, "labels")
}

// nodeAnnotationKeys is nodeLabelKeys for the annotations of the same object, the record of the keys
// a slot wrote included.
func nodeAnnotationKeys(nodeName string) ([]string, error) {
	return nodeMetadataKeys(nodeName, "annotations")
}

func nodeMetadataKeys(nodeName, field string) ([]string, error) {
	out, err := utils.Run(kubectl("get", "node", nodeName, "-o", "json"))
	if err != nil {
		return nil, fmt.Errorf("read the %s of Kubernetes node %s: %w", field, nodeName, err)
	}

	node := &corev1.Node{}
	if err := json.Unmarshal([]byte(out), node); err != nil {
		return nil, fmt.Errorf("decode Kubernetes node %s: %w", nodeName, err)
	}

	metadata := node.Labels
	if field == "annotations" {
		metadata = node.Annotations
	}

	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys, nil
}
