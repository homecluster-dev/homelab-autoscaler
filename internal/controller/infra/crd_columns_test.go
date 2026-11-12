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
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/yaml"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// The columns of the two CRDs, asserted on the **generated manifests**, in both the places they
// ship: config/crd/bases (kustomize install) and dist/chart/templates/crd (the Helm chart).
//
// A printer column is a string in a YAML file that nothing compiles: a typo in a JSONPath, a rename
// of a header, a dropped column, all ship green and say so only as an empty column in front of an
// operator. So the expectation is written here, in full, ordered, and the same assertion runs
// against the chart's copy, because the chart is what the cluster gets.
//
// The values of the columns - that the escaped Zone path selects a real key, that the group's
// counts render - are asserted against a live API server in group_columns_envtest_test.go. This
// file is the cheaper half: it fails on the manifest, before anything runs.
const (
	nodeCRD  = "infra.homecluster.dev_nodes.yaml"
	groupCRD = "infra.homecluster.dev_groups.yaml"

	basesDir = "../../../config/crd/bases"
	chartDir = "../../../dist/chart/templates/crd"
)

// wantNodeColumns is `kubectl get nodes.infra.homecluster.dev`, column by column. Powerstate and
// Phase are the columns that existed before this work: their names, casing and paths are frozen
// here, so the two that came first cannot be renamed by the ones that came after.
var wantNodeColumns = []columnSpec{
	{name: "Powerstate", kind: "string", jsonPath: ".status.powerState"},
	{name: "Phase", kind: "string", jsonPath: ".status.phase"},
	{name: "Group", kind: "string", jsonPath: ".metadata.labels.group"},
	{name: "Zone", kind: "string", jsonPath: `.spec.nodeLabels['topology\.kubernetes\.io/zone']`},
	{name: "Since", kind: "date", jsonPath: ".status.lastTransitionTime"},
	{name: "On24h", kind: "integer", jsonPath: ".status.activationsLast24H", priority: 0},
}

// wantGroupColumns is `kubectl get groups`: NAME and AGE were all it had.
var wantGroupColumns = []columnSpec{
	{name: "Slots", kind: "integer", jsonPath: ".status.slots"},
	{name: "On", kind: "integer", jsonPath: ".status.on", priority: 0},
	{name: "Off", kind: "integer", jsonPath: ".status.off", priority: 0},
	{name: "LastScale", kind: "date", jsonPath: ".status.lastScaleAt", priority: 0},
	{name: "Health", kind: "string", jsonPath: ".status.health", priority: 0},
}

type columnSpec struct {
	name     string
	kind     string
	jsonPath string
	priority int32
}

func (c columnSpec) String() string {
	return c.name + "|" + c.kind + "|" + c.jsonPath
}

func TestNodeCRDPrinterColumns(t *testing.T) {
	assertColumns(t, nodeCRD, wantNodeColumns)
}

func TestGroupCRDPrinterColumns(t *testing.T) {
	assertColumns(t, groupCRD, wantGroupColumns)
}

// TestZoneJSONPathParses: the Zone column selects a map key that contains dots, which JSONPath
// wants escaped inside brackets. The API server validates the path when the CRD is applied, so a
// broken escaping is a CRD the chart cannot install; the path is compiled here as well, against the
// same parser, so the failure is a test line and not an install error.
func TestZoneJSONPathParses(t *testing.T) {
	var zone string

	for _, col := range wantNodeColumns {
		if col.name == "Zone" {
			zone = col.jsonPath
		}
	}

	require.NotEmpty(t, zone)

	path := jsonpath.New("zone").AllowMissingKeys(true)
	// The printer wraps a CRD jsonPath in braces before feeding it the parser, and so does here.
	require.NoError(t, path.Parse("{"+zone+"}"))

	slot := &infrav2alpha1.Node{
		Spec: infrav2alpha1.NodeSpec{
			NodeLabels: map[string]string{"topology.kubernetes.io/zone": "fsn1", "pool.homecluster.dev/rack": "r7"},
		},
	}

	var rendered bytes.Buffer

	require.NoError(t, path.Execute(&rendered, slot))
	assert.Equal(t, "fsn1", rendered.String(), "the escaped path selects the zone, and only it")
}

// TestNodeStatusFieldsAreInTheCRD: the new fields are the columns' source, and a field the schema
// does not carry is a field the API server prunes on write - the controller would write a ring and
// read none back.
func TestNodeStatusFieldsAreInTheCRD(t *testing.T) {
	status := statusProperties(t, nodeCRD)

	for _, name := range []string{
		"lastTransitionTime", "previousPhase", "activationsTotal", "activationsLast24H", "recentTransitions",
	} {
		field, ok := status[name]
		require.True(t, ok, "status.%s is missing from the generated Node CRD", name)

		switch name {
		case "recentTransitions":
			assert.Equal(t, "array", field.Type)
			require.NotNil(t, field.Items)

			for _, key := range []string{"at", "from", "to", "reason"} {
				assert.Contains(t, field.Items.Schema.Properties, key, "the ring row carries %s", key)
			}

			assert.Equal(t, int64(32), *field.MaxItems, "the ring is bounded")
		case "activationsTotal", "activationsLast24H":
			assert.Equal(t, "integer", field.Type)
			assert.InDelta(t, 0.0, *field.Minimum, 0)
		case "lastTransitionTime":
			assert.Equal(t, "string", field.Type)
			assert.Equal(t, "date-time", field.Format)
		case "previousPhase":
			assert.Equal(t, "string", field.Type)
			assert.ElementsMatch(t, []any{"", "off", "pending", "booting", "ready", "draining"}, enumValues(field.Enum))
		}
	}
}

func TestGroupStatusFieldsAreInTheCRD(t *testing.T) {
	status := statusProperties(t, groupCRD)

	for _, name := range []string{"slots", "on", "off", "lastScaleAt", "lastScaleAction"} {
		require.Contains(t, status, name, "status.%s is missing from the generated Group CRD", name)
	}

	assert.Equal(t, "integer", status["slots"].Type)
	assert.Equal(t, "integer", status["on"].Type)
	assert.Equal(t, "integer", status["off"].Type)
	assert.Equal(t, "date-time", status["lastScaleAt"].Format)
	assert.Equal(t, "string", status["lastScaleAction"].Type)
	assert.Equal(t, int64(256), *status["lastScaleAction"].MaxLength)
}

// --- the machinery ---------------------------------------------------------------------------

func assertColumns(t *testing.T, file string, want []columnSpec) {
	t.Helper()

	// The generated manifest, and the chart's templated copy: the chart is what a cluster installs,
	// so a column that exists only in one of the two is a column that ships broken.
	crd := readCRD(t, filepath.Join(basesDir, file))

	assertColumnSpecs(t, crd, want)
	assertColumnSpecs(t, readCRD(t, filepath.Join(chartDir, file)), want)
}

func assertColumnSpecs(t *testing.T, crd *apiextv1.CustomResourceDefinition, want []columnSpec) {
	t.Helper()

	stored := storedVersion(t, crd)

	got := make([]columnSpec, 0, len(stored.AdditionalPrinterColumns))

	for _, col := range stored.AdditionalPrinterColumns {
		priority := col.Priority

		got = append(got, columnSpec{name: col.Name, kind: col.Type, jsonPath: col.JSONPath, priority: priority})
	}

	assert.Equal(t, render(want), render(got), "additionalPrinterColumns of %s (name|type|jsonPath, in order)", crd.Name)

	for _, col := range got {
		compiled := jsonpath.New(col.name).AllowMissingKeys(true)
		assert.NoError(t, compiled.Parse("{"+col.jsonPath+"}"), "column %s has a JSONPath the printer's parser accepts", col.name)
	}
}

func render(cols []columnSpec) []string {
	out := make([]string, 0, len(cols))
	for _, col := range cols {
		out = append(out, col.String())
	}

	return out
}

// readCRD unmarshals a generated CRD, stripping the Helm templating of the chart's copy (every
// templated line is a whole line in the generated files, so dropping them leaves valid YAML).
func readCRD(t *testing.T, path string) *apiextv1.CustomResourceDefinition {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "the generated CRD must exist: run `make manifests helm-sync-crds`")

	lines := strings.Split(string(raw), "\n")
	clean := make([]string, 0, len(lines))

	for _, line := range lines {
		if strings.Contains(line, "{{") || strings.Contains(line, "}}") {
			continue
		}

		clean = append(clean, line)
	}

	doc := &apiextv1.CustomResourceDefinition{}
	require.NoError(t, yaml.UnmarshalStrict([]byte(strings.Join(clean, "\n")), doc), path)

	return doc
}

func storedVersion(t *testing.T, crd *apiextv1.CustomResourceDefinition) *apiextv1.CustomResourceDefinitionVersion {
	t.Helper()

	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			return &crd.Spec.Versions[i]
		}
	}

	require.Fail(t, "the CRD has no storage version", crd.Name)

	return nil
}

func statusProperties(t *testing.T, file string) map[string]apiextv1.JSONSchemaProps {
	t.Helper()

	stored := storedVersion(t, readCRD(t, filepath.Join(basesDir, file)))

	status, ok := stored.Schema.OpenAPIV3Schema.Properties["status"]
	require.True(t, ok, "%s has no status schema", file)

	return status.Properties
}

func enumValues(en []apiextv1.JSON) []any {
	out := make([]any, 0, len(en))

	for _, e := range en {
		var v any

		if err := json.Unmarshal(e.Raw, &v); err != nil {
			continue
		}

		out = append(out, v)
	}

	return out
}
