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
	"bytes"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

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

// TestTemplateCarriesALabelInAnAnnotationReservedNamespace is M1's regression, at the level the
// estimator reads: the two reserved lists are not the same list, and a label under a namespace that
// is only reserved for annotations - Argo CD's - is a label a nodeSelector can match. Filtering the
// label overlay with the annotation list drops it from the template, and the pod that selects it is
// never placed on the pool: the silent failure the feature exists to remove.
func TestTemplateCarriesALabelInAnAnnotationReservedNamespace(t *testing.T) {
	server := newServer(t,
		slotWithMetadata("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady,
			map[string]string{"argocd.argoproj.io/team": "platform", "k3d.io/role": "agent"},
			map[string]string{"argocd.argoproj.io/sync-wave": "1"}),
		nodeWithShape("slot-on", true, "8", "16Gi"),
	)

	template := templateFor(t, server)

	assert.Equal(t, "platform", template.Labels["argocd.argoproj.io/team"],
		"the label overlay uses the label rules: a nodeSelector selecting this team must find this pool")
	assert.Equal(t, "agent", template.Labels["k3d.io/role"])
	assert.NotContains(t, template.Annotations, "argocd.argoproj.io/sync-wave",
		"as an annotation the same namespace is Argo CD's, and the annotation overlay says so")
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
// estimator has to match against - so the label has to be declared on every slot of the pool, which
// is what the docs tell an operator to do for a pool that sits in one zone.
func TestTemplateOfAnAllOffPoolCarriesTheDeclaredZone(t *testing.T) {
	server := newServer(t,
		slotWithMetadata("slot-off-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff,
			map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			map[string]string{"pool.homecluster.dev/power": "ipmi"}),
		slotWithMetadata("slot-off-2", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff,
			map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			map[string]string{"pool.homecluster.dev/power": "ipmi"}),
	)

	template := templateFor(t, server)

	assert.Equal(t, "fsn1", template.Labels["topology.kubernetes.io/zone"])
	assert.Equal(t, "ipmi", template.Annotations["pool.homecluster.dev/power"])
	assert.Equal(t, testGroup, template.Labels["infra.homecluster.dev/group"],
		"the group label is still on the placeholder")
	assert.Equal(t, int64(defaultTemplateMaxPods), allowedPodNumberOf(template.Status.Allocatable),
		"and it is a template a pod can be placed on")
}

// TestTemplateOfAnAllOffPoolOmitsAZoneSomeSlotDoesNotDeclare is the fail-closed half of
// scale-from-zero: a pool whose slots do not all declare the zone cannot promise it, because the
// scale-up the template authorises buys whichever slot is free. The placeholder of such a pool
// carries the keys the group agrees on, and not the disputed one.
func TestTemplateOfAnAllOffPoolOmitsAZoneSomeSlotDoesNotDeclare(t *testing.T) {
	server := newServer(t,
		slotWithMetadata("slot-off-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff,
			map[string]string{"topology.kubernetes.io/zone": "fsn1", "pool.homecluster.dev/rack": "3"}, nil),
		slotWithMetadata("slot-off-2", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff,
			map[string]string{"pool.homecluster.dev/rack": "3"}, nil),
	)

	template := templateFor(t, server)

	assert.NotContains(t, template.Labels, "topology.kubernetes.io/zone",
		"one slot of the pool has no zone, so the pool must not simulate one: the estimator would place "+
			"a zoned pod on a machine that is booted without the label, and the pod stays Pending on a "+
			"machine that was paid for")
	assert.Equal(t, "3", template.Labels["pool.homecluster.dev/rack"],
		"the pool is still exact for every key its slots agree on")
}

// TestTemplateOfAHeterogeneousGroupOmitsTheDisputedKey pins down the decision the API forces, and
// the way it is made honest: NodeGroupTemplateNodeInfo answers for a *group* with one node, while
// IncreaseSize buys whichever slot the provider has free (sorted by name), so a key the slots
// disagree on is left out of the template rather than answered from the representative slot. The
// consequence for an operator is the sentence in the docs: slots the scheduler must tell apart
// belong in different groups, and a group that pays for a machine a pod cannot use is the failure
// this removes.
func TestTemplateOfAHeterogeneousGroupOmitsTheDisputedKey(t *testing.T) {
	logs := captureProviderLogs(t)

	server := newServer(t,
		slotWithMetadata("slot-fsn1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady,
			map[string]string{"topology.kubernetes.io/zone": "fsn1", "pool.homecluster.dev/rack": "3"},
			map[string]string{"pool.homecluster.dev/serial": "SN-0001"}),
		nodeWithShape("slot-fsn1", true, "8", "16Gi"),
		slotWithMetadata("slot-hel1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff,
			map[string]string{"topology.kubernetes.io/zone": "hel1", "pool.homecluster.dev/rack": "3"}, nil),
		nodeWithShape("slot-hel1", false, "8", "16Gi"),
	)

	template := templateFor(t, server)

	assert.NotContains(t, template.Labels, "topology.kubernetes.io/zone",
		"two slots, two zones: the group guarantees neither, so the template promises neither. The pod "+
			"selecting fsn1 must not buy slot-hel1's machine")
	assert.NotContains(t, template.Labels, "fsn1")
	assert.NotContains(t, template.Labels, "hel1")
	assert.Equal(t, "3", template.Labels["pool.homecluster.dev/rack"],
		"the keys the group agrees on are still the group's shape")
	assert.NotContains(t, template.Annotations, "pool.homecluster.dev/serial",
		"the same rule for annotations: a template that advertises one machine's serial number for the "+
			"pool is a template that lies to whatever reads it")

	// The report: the key, the slots, the values. A group's disagreement is configuration, and the
	// line has to name what to change.
	contents := logs.String()
	assert.Contains(t, contents, "the slots of a group disagree on their declared node metadata")
	assert.Contains(t, contents, "topology.kubernetes.io/zone")
	assert.Contains(t, contents, "on slot-fsn1")
	assert.Contains(t, contents, "on slot-hel1")
	assert.Contains(t, contents, "fsn1", "the note says which slot declares which value")
	assert.Contains(t, contents, "pool.homecluster.dev/serial")

	// And once per group, not per call: cluster-autoscaler asks this question for every group on
	// every scan, and a warning per scan is the noise that hides the warning.
	templateFor(t, server)
	templateFor(t, server)
	assert.Equal(t, 1, strings.Count(logs.String(), "the slots of a group disagree"),
		"the disagreement is reported when it appears, not on every scan")
}

// captureProviderLogs installs a logger writing to a buffer for the duration of a test. The
// provider logs through controller-runtime's global logger (log.Log), so a test that asserts an
// operator-visible line has to install one. The previous logger is restored on cleanup.
func captureProviderLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	buffer := &bytes.Buffer{}

	log.SetLogger(zap.New(zap.WriteTo(buffer), zap.UseDevMode(true)))
	t.Cleanup(func() { log.SetLogger(logr.Discard()) })

	return buffer
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
