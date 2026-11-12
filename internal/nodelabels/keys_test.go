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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReservedLabelKey(t *testing.T) {
	// One case per reserved prefix, each naming the owner: the list is only trustworthy if every
	// entry can be pointed at.
	reserved := []string{
		// the operator's own namespace, including the records this package writes
		AppliedLabelsAnnotation,
		AppliedAnnotationsAnnotation,
		"infra.homecluster.dev/group",
		"infra.homecluster.dev/node",
		// kubelet and node lifecycle controller
		"node.kubernetes.io/role",
		"node.kubernetes.io/not-ready",
		"node.kubernetes.io/unreachable",
		"node.kubernetes.io/exclude-from-external-load-balancers",
		// kubelet, node lifecycle controller, cloud integrations
		"kubernetes.io/hostname",
		"kubernetes.io/os",
		"kubernetes.io/arch",
		"kubernetes.io/keep-out-of-service",
		// control plane join tooling
		"node-role.kubernetes.io/control-plane",
		"node-role.kubernetes.io/master",
		// topology, except the two allowed keys
		"topology.kubernetes.io/zone-labels",
		"topology.kubernetes.io/distribution",
	}

	for _, key := range reserved {
		isReserved, reason := ReservedLabelKey(key)
		assert.Truef(t, isReserved, "%s must be a reserved label key", key)
		assert.NotEmptyf(t, reason, "%s must be refused with a reason an operator can read", key)
	}

	// The keys a slot declares in practice, and the two exceptions of the topology namespace.
	// The zone is the whole motivating case of the feature: refusing it would refuse the feature.
	for _, key := range []string{
		TopologyZoneLabel,
		TopologyRegionLabel,
		"pool.homecluster.dev/rack",
		"dedicated",
		"storage",
		"k3d.io/role/agent",
		"beta.kubernetes.io/os",
	} {
		isReserved, reason := ReservedLabelKey(key)
		assert.Falsef(t, isReserved, "%s must be the operator's to declare (reason: %s)", key, reason)
	}
}

func TestReservedAnnotationKey(t *testing.T) {
	reserved := []string{
		AppliedLabelsAnnotation,
		"infra.homecluster.dev/operation-lock",
		"argocd.argoproj.io/tracking-id",
		"argocd.argoproj.io/sync-wave",
		"argocd.argoproj.io/sync-options",
		"kubectl.kubernetes.io/last-applied-configuration",
		"kubectl.kubernetes.io/cordon",
	}

	for _, key := range reserved {
		isReserved, reason := ReservedAnnotationKey(key)
		assert.Truef(t, isReserved, "%s must be a reserved annotation key", key)
		assert.NotEmptyf(t, reason, "%s must be refused with a reason", key)
	}

	// The namespaces reserved for *labels* are not reserved for annotations: nothing stamps an
	// annotation there on its own, so there is no owner to defer to. And the label exceptions are
	// not annotation rules: annotation keys carry no topology contract.
	for _, key := range []string{
		"power.homecluster.dev/serial",
		"node.kubernetes.io/maintenance",
		"kubernetes.io/maintenance-window",
		"topology.kubernetes.io/note",
		"topology.kubernetes.io/zone",
		"dedicated",
	} {
		isReserved, reason := ReservedAnnotationKey(key)
		assert.Falsef(t, isReserved, "%s must be the operator's to declare (reason: %s)", key, reason)
	}
}

func TestReservedAnnotationKeyIsCaseInsensitive(t *testing.T) {
	// Annotation keys are compared case-insensitively, which is the rule the API machinery applies
	// to them (the domain part of a qualified name is a case-insensitive DNS name).
	for _, key := range []string{
		"ArgoCD.ArgoProj.io/sync-wave",
		"Kubectl.Kubernetes.io/last-applied-configuration",
		"Infra.HomeCluster.Dev/group",
	} {
		isReserved, _ := ReservedAnnotationKey(key)
		assert.Truef(t, isReserved, "%s must be reserved whatever its case", key)
	}
}

func TestOwnerRecordRoundTrip(t *testing.T) {
	// The record is what makes removal possible, so the same set of keys must always render the
	// same string: a resync that changes nothing has to write nothing.
	encoded := EncodeOwnerRecord([]string{"topology.kubernetes.io/zone", "rack", "dedicated"})
	assert.Equal(t, "dedicated,rack,topology.kubernetes.io/zone", encoded)
	assert.Equal(t, encoded, EncodeOwnerRecord([]string{"rack", "dedicated", "topology.kubernetes.io/zone"}),
		"the record of one set of keys is one string, so an unchanged resync writes nothing")

	owned := DecodeOwnerRecord(encoded)
	assert.ElementsMatch(t, []string{"dedicated", "rack", "topology.kubernetes.io/zone"}, keysOf(owned))

	// Every shape of "this slot owns nothing here", which is every node written before the field
	// existed and every slot whose declaration is empty.
	for _, record := range []string{"", "  ", ",", "a, ,b", "a,,b"} {
		decoded := DecodeOwnerRecord(record)
		for _, key := range keysOf(decoded) {
			assert.NotEmpty(t, key, "a record never lists an empty key")
		}
	}

	assert.Empty(t, DecodeOwnerRecord(""))
	assert.Empty(t, DecodeOwnerRecord("  "))
	assert.Empty(t, DecodeOwnerRecord(","))
	assert.ElementsMatch(t, []string{"a", "b"}, keysOf(DecodeOwnerRecord("a, ,b")))
}

func keysOf(m map[string]string) []string {
	return sortedKeys(m)
}
