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
	"strings"

	corev1 "k8s.io/api/core/v1"

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

// slotEventsFor renders the reasons of the events.k8s.io Events published on a slot, the way
// `kubectl describe` does, so a failing gate shows what the controller recorded about the write.
//
// The list is unfiltered and the selection is done on the decoded object: the field selectors of
// the events registry are a detail this suite has no business depending on, and the target of an
// Event is the `regarding` reference of the new events API, which is exactly what the recorder
// stamps for a CRD object.
func slotEventsFor(slot string) string {
	out, err := utils.Run(kubectl("get", "events.k8s.io", "-n", namespace, "-o", "json"))
	if err != nil {
		return ""
	}

	var listed struct {
		Items []struct {
			Reason    string `json:"reason"`
			Regarding struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"regarding"`
			Note string `json:"note"`
		} `json:"items"`
	}

	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		return ""
	}

	reasons := make([]string, 0, len(listed.Items))

	for _, event := range listed.Items {
		if event.Regarding.Name != slot {
			continue
		}

		reasons = append(reasons, event.Reason+" "+event.Note)
	}

	return strings.Join(reasons, "\n")
}
