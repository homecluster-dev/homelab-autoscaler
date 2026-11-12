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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func labelsPath() *field.Path { return field.NewPath("spec").Child("nodeLabels") }

func annotationsPath() *field.Path {
	return field.NewPath("spec").Child("nodeAnnotations")
}

func TestValidateLabels(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		wantSome []string
	}{
		{
			name:   "a slot that declares nothing is valid",
			labels: nil,
		},
		{
			name: "the zone, the region and a key in the operator's own namespace are valid",
			labels: map[string]string{
				TopologyZoneLabel:           "fsn1",
				TopologyRegionLabel:         "eu-central-1",
				"pool.homecluster.dev/rack": "rack-3",
				"dedicated":                 "remote",
				"pool.homecluster.dev/ssd":  "",
			},
		},
		{
			name:   "a key with an uppercase prefix is not a qualified name",
			labels: map[string]string{"Pool.HomeCluster.Dev/rack": "3"},
			wantSome: []string{
				"spec.nodeLabels[Pool.HomeCluster.Dev/rack]",
				"lowercase RFC 1123 subdomain",
			},
		},
		{
			name:     "a key with a space is not a qualified name",
			labels:   map[string]string{"my rack": "3"},
			wantSome: []string{"spec.nodeLabels[my rack]"},
		},
		{
			name:     "a key with an empty prefix is not a qualified name",
			labels:   map[string]string{"/rack": "3"},
			wantSome: []string{"spec.nodeLabels[/rack]"},
		},
		{
			name:     "a key with an empty name is not a qualified name",
			labels:   map[string]string{"pool.homecluster.dev/": "3"},
			wantSome: []string{"spec.nodeLabels[pool.homecluster.dev/]"},
		},
		{
			name:     "a key with three segments is not a qualified name",
			labels:   map[string]string{"pool.homecluster.dev/a/b": "3"},
			wantSome: []string{"spec.nodeLabels[pool.homecluster.dev/a/b]"},
		},
		{
			name:     "a value with a space is not a label value",
			labels:   map[string]string{"rack": "rack three"},
			wantSome: []string{"a valid label must be an empty string"},
		},
		{
			name:     "a value longer than 63 characters is refused",
			labels:   map[string]string{"rack": strings.Repeat("x", 64)},
			wantSome: []string{"must be no more than 63 bytes"},
		},
		{
			// One case per reserved prefix, asserted through the validation path the webhook
			// calls, so the message an operator reads is the one under test.
			name:     "infra.homecluster.dev/ is the operator's",
			labels:   map[string]string{"infra.homecluster.dev/group": "workers"},
			wantSome: []string{"spec.nodeLabels[infra.homecluster.dev/group]", "reserved key, not writable from a slot"},
		},
		{
			name:     "node.kubernetes.io/ is the kubelet's and the node lifecycle controller's",
			labels:   map[string]string{"node.kubernetes.io/role": "agent"},
			wantSome: []string{"spec.nodeLabels[node.kubernetes.io/role]", "kubelet"},
		},
		{
			name:     "kubernetes.io/ is written by the kubelet and the cloud integrations",
			labels:   map[string]string{"kubernetes.io/hostname": "worker-01"},
			wantSome: []string{"spec.nodeLabels[kubernetes.io/hostname]", "hostname"},
		},
		{
			name:     "node-role.kubernetes.io/ belongs to the control plane tooling",
			labels:   map[string]string{"node-role.kubernetes.io/control-plane": ""},
			wantSome: []string{"spec.nodeLabels[node-role.kubernetes.io/control-plane]", "control plane"},
		},
		{
			name:     "topology.kubernetes.io/ is the topology machinery's, except zone and region",
			labels:   map[string]string{"topology.kubernetes.io/zone-labels": "fsn1"},
			wantSome: []string{"spec.nodeLabels[topology.kubernetes.io/zone-labels]", "topology.kubernetes.io/zone"},
		},
		{
			name:   "topology.kubernetes.io/zone is allowed, and it is the key the feature is about",
			labels: map[string]string{TopologyZoneLabel: "fsn1"},
		},
		{
			name:   "topology.kubernetes.io/region is allowed",
			labels: map[string]string{TopologyRegionLabel: "eu-central-1"},
		},
		{
			name:     "more labels than the bound is refused",
			labels:   entries(MaxEntries + 1),
			wantSome: []string{"spec.nodeLabels: Too many: 65: must have at most 64 items"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateLabels(tc.labels, labelsPath())

			if len(tc.wantSome) == 0 {
				assert.Empty(t, errs.ToAggregate())

				return
			}

			message := errs.ToAggregate().Error()
			for _, want := range tc.wantSome {
				assert.Contains(t, message, want)
			}
		})
	}
}

func TestValidateLabelsReportsEveryBadKeyAtOnce(t *testing.T) {
	// A manifest with one bad key out of twenty must say which one, and all of them: an operator
	// fixing a rejected slot applies it again, and a refusal that names one key per apply is a
	// loop of applies.
	errs := ValidateLabels(map[string]string{
		"kubernetes.io/hostname":  "worker-01",
		"node.kubernetes.io/role": "agent",
		"rack":                    "rack three",
	}, labelsPath())

	assert.Len(t, errs, 3)

	for _, key := range []string{"kubernetes.io/hostname", "node.kubernetes.io/role", "rack"} {
		assert.Contains(t, errs.ToAggregate().Error(), fmt.Sprintf("spec.nodeLabels[%s]", key))
	}
}

func TestValidateAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantSome    []string
	}{
		{
			name: "a slot that declares nothing is valid",
		},
		{
			name:        "a plain key in the operator's namespace is valid",
			annotations: map[string]string{"pool.homecluster.dev/serial": "SN-0001"},
		},
		{
			name:        "an annotation key with a space is refused",
			annotations: map[string]string{"not a key": "value"},
			wantSome:    []string{"spec.nodeAnnotations[not a key]"},
		},
		{
			name:        "an annotation key with two slashes is refused",
			annotations: map[string]string{"pool.homecluster.dev/a/b": "value"},
			wantSome:    []string{"spec.nodeAnnotations[pool.homecluster.dev/a/b]"},
		},
		{
			name:        "Argo CD's annotations belong to Argo CD",
			annotations: map[string]string{"argocd.argoproj.io/sync-wave": "1"},
			wantSome:    []string{"spec.nodeAnnotations[argocd.argoproj.io/sync-wave]", "Argo CD"},
		},
		{
			name:        "kubectl's annotations belong to kubectl",
			annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
			wantSome:    []string{"spec.nodeAnnotations[kubectl.kubernetes.io/last-applied-configuration]", "kubectl"},
		},
		{
			name:        "the operator's namespace belongs to the operator, records included",
			annotations: map[string]string{AppliedLabelsAnnotation: "rack"},
			wantSome:    []string{"spec.nodeAnnotations[infra.homecluster.dev/applied-node-labels]"},
		},
		{
			name:        "the count is bounded",
			annotations: entries(MaxEntries + 1),
			wantSome:    []string{"spec.nodeAnnotations: Too many: 65: must have at most 64 items"},
		},
		{
			name:        "the total size is bounded by what the node object may carry",
			annotations: map[string]string{"pool.homecluster.dev/notes": strings.Repeat("x", 256*1024+1)},
			wantSome:    []string{"larger than limit"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateAnnotations(tc.annotations, annotationsPath())

			if len(tc.wantSome) == 0 {
				assert.Empty(t, errs.ToAggregate())

				return
			}

			message := errs.ToAggregate().Error()
			for _, want := range tc.wantSome {
				assert.Contains(t, message, want)
			}
		})
	}
}

func TestValidateAnnotationsAcceptsWhatTheNodeObjectAccepts(t *testing.T) {
	// Annotation values are not syntax-checked: the API machinery checks nothing about them beyond
	// the size of the map, and a check stricter than the API server's refuses a note the node would
	// have taken. Keys are compared in lower case, the rule the API machinery applies.
	errs := ValidateAnnotations(map[string]string{
		"pool.homecluster.dev/last-poweroff": "2026-11-12T03:04:05Z (rack 3, IPMI, `ipmitool -H bmc`)",
		"pool.homecluster.dev/notes-json":    `{"rack":"3","unit":42,"owner":"filippo"}`,
		"POOL.HOMELAB-DEV/upper":             "a case-insensitive namespace, a valid key",
		"node.kubernetes.io/maintenance":     "true",
		"topology.kubernetes.io/note":        "top of frame",
		"beta.kubernetes.io/whatever":        "the deprecated namespace carries no owner for annotations",
	}, annotationsPath())

	assert.Empty(t, errs.ToAggregate())
}

func entries(n int) map[string]string {
	out := make(map[string]string, n)
	for i := range n {
		out[fmt.Sprintf("pool.homecluster.dev/k%d", i)] = "v"
	}

	return out
}
