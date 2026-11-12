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

package grpcserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

// slotWithMetadata builds a slot of the test group that declares spec.nodeLabels and
// spec.nodeAnnotations: the labels cluster-autoscaler matches a pod's nodeSelector against, and
// the metadata a consumer of the template reads.
func slotWithMetadata(name string, observed infrav2alpha1.PowerState, report infrav2alpha1.NodePhase,
	labels, annotations map[string]string) *infrav2alpha1.Node {
	node := nodeCR(name, observed, report)
	node.Spec.NodeLabels = labels
	node.Spec.NodeAnnotations = annotations

	return node
}

// TestTemplateCarriesTheDeclaredLabels is the half of the feature the estimator reads: a pod with
// `nodeSelector: {topology.kubernetes.io/zone: fsn1}` is placed on the group whose template carries
// that label, and on no other. Before the field, every slot of a pool reported the same template,
// so a zoned pod was never placed on this pool and no machine was ever bought for it.
func TestTemplateCarriesTheDeclaredLabels(t *testing.T) {
	server := newServer(t,
		slotWithMetadata("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady,
			map[string]string{"topology.kubernetes.io/zone": "fsn1", "pool.homecluster.dev/rack": "3"},
			map[string]string{"pool.homecluster.dev/serial": "SN-0001"}),
		nodeWithShape("slot-on", true, "8", "16Gi"),
	)

	template := templateFor(t, server)

	assert.Equal(t, "fsn1", template.Labels["topology.kubernetes.io/zone"],
		"the zone is what the simulator matches a nodeSelector against: without it a zoned pod never triggers this pool")
	assert.Equal(t, "3", template.Labels["pool.homecluster.dev/rack"])
	assert.Equal(t, "SN-0001", template.Annotations["pool.homecluster.dev/serial"],
		"the annotations ride along, so a consumer reading the template sees what it will see on the machine")

	// the template is still a template: the representative node's hostname is a fact about one
	// machine, so it is stripped from the copy, after the overlay, by the template builder.
	assert.NotContains(t, template.Labels, "kubernetes.io/hostname")
}

// TestTemplateLabelWinsOverTheRepresentativeNode states the precedence, which is the rule
// spec.nodeCapacity states for the machine's size: the template is the shape of the machine the
// autoscaler is about to boot, so the declaration is the value, and a label the representative node
// carries with a different value is drift on a live machine, not the shape of a new one.
func TestTemplateLabelWinsOverTheRepresentativeNode(t *testing.T) {
	source := nodeWithShape("slot-on", true, "8", "16Gi")
	source.Labels["topology.kubernetes.io/zone"] = "hel1"

	server := newServer(t,
		slotWithMetadata("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady,
			map[string]string{"topology.kubernetes.io/zone": "fsn1"}, nil),
		source,
	)

	template := templateFor(t, server)

	assert.Equal(t, "fsn1", template.Labels["topology.kubernetes.io/zone"],
		"the declaration answers for every slot of the group, not for whichever node is Ready")
}

// TestTemplateOfAnAllOffPoolCarriesTheDeclaredZone is the case that has no node object to copy:
// a pool whose every machine is off, which is exactly when a zoned pod has to trigger a scale-up.
// The placeholder is built from the Node CR, and the label it carries is the only thing the
// estimator has to match against.
func TestTemplateOfAnAllOffPoolCarriesTheDeclaredZone(t *testing.T) {
	server := newServer(t,
		slotWithMetadata("slot-off-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff,
			map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			map[string]string{"pool.homecluster.dev/power": "ipmi"}),
		nodeCR("slot-off-2", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	template := templateFor(t, server)

	assert.Equal(t, "fsn1", template.Labels["topology.kubernetes.io/zone"])
	assert.Equal(t, "ipmi", template.Annotations["pool.homecluster.dev/power"])
	assert.Equal(t, testGroup, template.Labels["infra.homecluster.dev/group"],
		"the group label is still on the placeholder")
	assert.Equal(t, int64(defaultTemplateMaxPods), allowedPodNumberOf(template.Status.Allocatable),
		"and it is a template a pod can be placed on")
}

// TestTemplateOfAHeterogeneousGroupAnswersFromOneSlot pins down the decision the API forces:
// NodeGroupTemplateNodeInfo answers for a *group*, and one call returns one node, so a group whose
// slots declare different labels is simulated from the one slot the provider picked - a Ready
// node's slot first, a slot of a fully off pool otherwise. The consequence for an operator is the
// sentence in the docs: slots the scheduler must tell apart belong in different groups.
func TestTemplateOfAHeterogeneousGroupAnswersFromOneSlot(t *testing.T) {
	server := newServer(t,
		slotWithMetadata("slot-fsn1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady,
			map[string]string{"topology.kubernetes.io/zone": "fsn1"}, nil),
		nodeWithShape("slot-fsn1", true, "8", "16Gi"),
		slotWithMetadata("slot-hel1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff,
			map[string]string{"topology.kubernetes.io/zone": "hel1"}, nil),
		nodeWithShape("slot-hel1", false, "8", "16Gi"),
	)

	template := templateFor(t, server)

	assert.Equal(t, "fsn1", template.Labels["topology.kubernetes.io/zone"],
		"one template per group, from the representative slot: the Ready one")
	assert.NotContains(t, template.Labels, "hel1")
	assert.NotEqual(t, "hel1", template.Labels["topology.kubernetes.io/zone"],
		"the other slot's zone is not merged in: a pod selecting hel1 is answered with this machine")
}

// TestTemplateRefusesASpoofedHostnameKey is the reserved-key rule on the CA side. The webhook
// refuses kubernetes.io/hostname in spec.nodeLabels; a slot object that predates the rule, or one
// created while the webhook was down, must still not hand the estimator a template with a fixed
// hostname, nor with a key the kubelet owns.
func TestTemplateRefusesASpoofedHostnameKey(t *testing.T) {
	server := newServer(t,
		slotWithMetadata("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady,
			map[string]string{
				"kubernetes.io/hostname":      "spoofed",
				"node.kubernetes.io/role":     "spoofed",
				"infra.homecluster.dev/group": "spoofed",
				"topology.kubernetes.io/zone": "fsn1",
			},
			map[string]string{"argocd.argoproj.io/sync-wave": "1"}),
		nodeWithShape("slot-on", true, "8", "16Gi"),
	)

	template := templateFor(t, server)

	assert.NotEqual(t, "spoofed", template.Labels["kubernetes.io/hostname"])
	assert.NotEqual(t, "spoofed", template.Labels["node.kubernetes.io/role"])
	assert.Equal(t, testGroup, template.Labels["infra.homecluster.dev/group"])
	assert.NotContains(t, template.Annotations, "argocd.argoproj.io/sync-wave")
	assert.Equal(t, "fsn1", template.Labels["topology.kubernetes.io/zone"],
		"the allowed key is applied, the reserved ones are not")
}

// TestNodeGroupNodesReportsIDsNotLabels records the contract of the other call cluster-autoscaler
// makes, checked against the proto rather than assumed: an Instance carries an id and a status, and
// nothing else. The labels of a machine that already registered are read by the autoscaler off that
// machine's corev1.Node - which is why the slot's declaration has to be stamped on the node object,
// not only into the template. Declaring labels must not change what this call reports.
func TestNodeGroupNodesReportsIDsNotLabels(t *testing.T) {
	server := newServer(t,
		slotWithMetadata("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady,
			map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			map[string]string{"pool.homecluster.dev/serial": "SN-0001"}),
		nodeWithShape("slot-on", true, "8", "16Gi"),
		// a machine is running for the autoscaler when a live claim wants it
		claimCR("slot-on", infrav2alpha1.NodeClaimReady, nil),
	)

	resp, err := server.NodeGroupNodes(t.Context(), &pb.NodeGroupNodesRequest{Id: testGroup})
	require.NoError(t, err)
	require.Len(t, resp.Instances, 1)

	instance := resp.Instances[0]
	assert.Equal(t, "homelab://slot-on", instance.Id)
	assert.Equal(t, pb.InstanceStatus_instanceRunning, instance.Status.InstanceState)

	// The proto's own field set, read off the descriptor, so the assertion says what the contract
	// is: an Instance is an id and a status, with no labels, no annotations and no capacity.
	fields := instance.ProtoReflect().Descriptor().Fields()
	assert.Equal(t, 2, fields.Len(), "an Instance is an id and a status, and nothing more")
	for _, name := range []string{"labels", "annotations", "capacity", "allocatable"} {
		assert.Nil(t, fields.ByName(protoreflect.Name(name)),
			"the autoscaler cannot read %s off an instance, so the labels of a registered machine "+
				"are the ones on its corev1.Node", name)
	}
}
