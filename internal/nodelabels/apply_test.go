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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// applyEnv is one slot, one Kubernetes node, and a client that counts the writes, which is the
// only way to assert "nothing was patched" - the state of every resync of a settled slot.
type applyEnv struct {
	slot       *infrav2alpha1.Node
	node       *corev1.Node
	recorder   *capturingRecorder
	patchCalls int

	// c is built once, on first use: a fake client built from the fixtures again would answer
	// from the fixtures, and every assertion about what the apply wrote would be a lie.
	c client.Client
}

func (e *applyEnv) client(t *testing.T) client.Client {
	t.Helper()

	if e.c != nil {
		return e.c
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	if e.node != nil {
		builder = builder.WithObjects(e.node)
	}

	if e.slot != nil {
		builder = builder.WithObjects(e.slot)
	}

	e.c = builder.WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			e.patchCalls++

			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()

	return e.c
}

func newApplyEnv(labels, annotations map[string]string, nodeLabels map[string]string) *applyEnv {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-01", Labels: nodeLabels},
	}

	slot := &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "slot-1", Namespace: "default"},
		Spec: infrav2alpha1.NodeSpec{
			KubernetesNodeName: "worker-01",
			NodeLabels:         labels,
			NodeAnnotations:    annotations,
		},
	}

	return &applyEnv{slot: slot, node: node, recorder: &capturingRecorder{}}
}

// applied reads the Kubernetes node back out of the cluster the apply wrote to.
func (e *applyEnv) applied(t *testing.T) *corev1.Node {
	t.Helper()

	k8sNode := &corev1.Node{}
	require.NoError(t, e.client(t).Get(t.Context(), types.NamespacedName{Name: "worker-01"}, k8sNode))

	return k8sNode
}

func (e *applyEnv) run(t *testing.T) {
	t.Helper()

	require.NoError(t, ApplyToKubernetesNode(t.Context(), e.client(t), e.slot, e.recorder))
}

func TestApplyWritesTheDeclaration(t *testing.T) {
	env := newApplyEnv(
		map[string]string{TopologyZoneLabel: "fsn1", "rack": "3"},
		map[string]string{"pool.homecluster.dev/serial": "SN-0001"},
		map[string]string{"kubernetes.io/hostname": "worker-01"},
	)
	env.run(t)

	node := env.applied(t)

	assert.Equal(t, "fsn1", node.Labels[TopologyZoneLabel], "the zone a nodeSelector matches must be on the node")
	assert.Equal(t, "3", node.Labels["rack"])
	assert.Equal(t, "SN-0001", node.Annotations["pool.homecluster.dev/serial"])
	assert.Equal(t, "worker-01", node.Labels["kubernetes.io/hostname"], "the kubelet's own label is untouched")
	assert.Equal(t, "rack,"+TopologyZoneLabel, node.Annotations[AppliedLabelsAnnotation],
		"the record lists the keys the slot wrote, so the next pass can take them back")
	assert.Equal(t, "pool.homecluster.dev/serial", node.Annotations[AppliedAnnotationsAnnotation])

	require.Len(t, env.recorder.calls, 1)
	assert.Equal(t, corev1.EventTypeNormal, env.recorder.calls[0].eventType)
	assert.Equal(t, ReasonApplied, env.recorder.calls[0].reason)
	assert.Equal(t, ActionApplyNodeLabels, env.recorder.calls[0].action)
	assert.Contains(t, env.recorder.calls[0].message(), "+rack +"+TopologyZoneLabel)
	assert.Contains(t, env.recorder.calls[0].message(), "+pool.homecluster.dev/serial")
	assert.Equal(t, 1, env.patchCalls)
}

func TestApplyIsIdempotent(t *testing.T) {
	// Every resync of every settled slot runs this path. A patch per pass churns a hot object and
	// an Event per pass buries the one that mattered.
	env := newApplyEnv(map[string]string{TopologyZoneLabel: "fsn1"}, nil, map[string]string{})
	env.run(t)

	firstCalls, firstEvents := env.patchCalls, len(env.recorder.calls)

	env.run(t)
	env.run(t)

	assert.Equal(t, firstCalls, env.patchCalls, "a node that already matches the declaration is not patched")
	assert.Equal(t, firstEvents, len(env.recorder.calls), "a pass that wrote nothing records nothing")
}

func TestApplyReassertsDrift(t *testing.T) {
	// The two shapes drift takes: a value changed by hand, and a key that disappeared - a kubelet
	// that stopped stamping it, a node object recreated by a re-registration, an operator's
	// `kubectl label node ... key-`.
	env := newApplyEnv(map[string]string{TopologyZoneLabel: "fsn1", "rack": "3"}, nil, map[string]string{})
	env.run(t)

	// The hand edit, written the way kubectl writes it: remove one key, overwrite the other.
	patchClient := env.client(t)

	node := env.applied(t)
	labels := map[string]string{"rack": "9", "node.kubernetes.io/role": "agent"}

	for key, value := range node.Labels {
		if key == "rack" {
			continue
		}

		labels[key] = value
	}

	node.Labels = labels
	require.NoError(t, patchClient.Update(t.Context(), node))

	env.run(t)

	after := env.applied(t)
	assert.Equal(t, "fsn1", after.Labels[TopologyZoneLabel], "a label the slot declares and the node lost is put back")
	assert.Equal(t, "3", after.Labels["rack"], "a value changed on the node is put back")
	assert.Equal(t, 2, env.patchCalls, "the drift is one write")
	assert.Len(t, env.recorder.calls, 2, "the write that fixed the drift is the event an operator reads")
	assert.Equal(t, "agent", after.Labels["node.kubernetes.io/role"],
		"a label the slot does not declare, and never wrote, is left alone")
}

// capturedEvent and capturingRecorder are the test double of the events API recorder: what the
// apply recorded, in order, with the note rendered the way the real recorder renders it.
type capturedEvent struct {
	eventType string
	reason    string
	action    string
	note      string
	args      []interface{}
}

func (e capturedEvent) message() string {
	return fmt.Sprintf(e.note, e.args...)
}

type capturingRecorder struct {
	calls []capturedEvent
}

var _ events.EventRecorder = &capturingRecorder{}

func (c *capturingRecorder) Eventf(regarding runtime.Object, related runtime.Object,
	eventType, reason, action, note string, args ...interface{}) {
	c.calls = append(c.calls, capturedEvent{
		eventType: eventType,
		reason:    reason,
		action:    action,
		note:      note,
		args:      args,
	})
}

func TestApplyRemovesADroppedKey(t *testing.T) {
	// The rule that makes the declaration true of a machine that lives for months: taking the key
	// out of Git takes it off the node.
	env := newApplyEnv(map[string]string{TopologyZoneLabel: "fsn1", "rack": "3"},
		map[string]string{"pool.homecluster.dev/serial": "SN-0001"},
		map[string]string{"kubernetes.io/hostname": "worker-01"})
	env.run(t)

	delete(env.slot.Spec.NodeLabels, "rack")
	delete(env.slot.Spec.NodeAnnotations, "pool.homecluster.dev/serial")
	env.run(t)

	node := env.applied(t)

	_, rackPresent := node.Labels["rack"]
	assert.False(t, rackPresent, "a key the declaration dropped is removed from the node")

	_, serialPresent := node.Annotations["pool.homecluster.dev/serial"]
	assert.False(t, serialPresent, "the same rule for annotations")

	assert.Equal(t, "fsn1", node.Labels[TopologyZoneLabel], "a key still declared stays")
	assert.Equal(t, "worker-01", node.Labels["kubernetes.io/hostname"], "a key the slot never wrote is untouched")
	assert.Equal(t, TopologyZoneLabel, node.Annotations[AppliedLabelsAnnotation],
		"the record follows the declaration: it is what the next pass removes")

	assert.Equal(t, 2, env.patchCalls)
	assert.Contains(t, env.recorder.calls[1].message(), "-rack")
	assert.Contains(t, env.recorder.calls[1].message(), "-pool.homecluster.dev/serial")
}

func TestApplyDropsTheRecordWhenNothingIsDeclared(t *testing.T) {
	// The last key of a slot whose field was emptied: the record's job was to say what to remove,
	// it removed it, and an empty annotation on every node of every idle pool is litter.
	env := newApplyEnv(map[string]string{"rack": "3"}, map[string]string{"pool.homecluster.dev/n": "1"}, nil)
	env.run(t)

	env.slot.Spec.NodeLabels = nil
	env.slot.Spec.NodeAnnotations = nil
	env.run(t)

	node := env.applied(t)

	_, rackPresent := node.Labels["rack"]
	assert.False(t, rackPresent)

	assert.NotContains(t, node.Annotations, AppliedLabelsAnnotation)
	assert.NotContains(t, node.Annotations, AppliedAnnotationsAnnotation)
	assert.NotContains(t, node.Annotations, "pool.homecluster.dev/n")
}

func TestApplySkipsReservedKeys(t *testing.T) {
	// The webhook refuses these keys. This is the write for an object that predates the rule, or
	// one created on a cluster with the webhook switched off: the operator's and Kubernetes'
	// namespaces are not written, and the refusal is recorded, not swallowed.
	env := newApplyEnv(
		map[string]string{
			"kubernetes.io/hostname":             "spoofed",
			"infra.homecluster.dev/group":        "spoofed",
			"node.kubernetes.io/role":            "spoofed",
			"topology.kubernetes.io/zone-labels": "spoofed",
			TopologyZoneLabel:                    "fsn1",
		},
		map[string]string{
			"argocd.argoproj.io/sync-wave":                     "1",
			"kubectl.kubernetes.io/last-applied-configuration": "{}",
			AppliedLabelsAnnotation:                            "spoofed",
		},
		map[string]string{"kubernetes.io/hostname": "worker-01", "node.kubernetes.io/role": "agent"},
	)
	env.run(t)

	node := env.applied(t)

	assert.Equal(t, "worker-01", node.Labels["kubernetes.io/hostname"], "the kubelet's value stands")
	assert.Equal(t, "agent", node.Labels["node.kubernetes.io/role"], "the node lifecycle namespace is not this writer's")
	assert.NotContains(t, node.Labels, "infra.homecluster.dev/group")
	assert.NotContains(t, node.Labels, "topology.kubernetes.io/zone-labels")
	assert.Equal(t, "fsn1", node.Labels[TopologyZoneLabel], "the allowed exception is written")
	assert.NotContains(t, node.Annotations, "argocd.argoproj.io/sync-wave")
	assert.NotContains(t, node.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
	assert.Equal(t, TopologyZoneLabel, node.Annotations[AppliedLabelsAnnotation],
		"the record lists only the keys the controller wrote")

	require.Len(t, env.recorder.calls, 2,
		"the refusal of the reserved keys, and the write of the one key the slot may declare")

	warning := env.recorder.calls[0]
	assert.Equal(t, corev1.EventTypeWarning, warning.eventType)
	assert.Equal(t, ReasonSkippedReservedKey, warning.reason)

	note := warning.message()
	for _, key := range []string{
		"kubernetes.io/hostname",
		"infra.homecluster.dev/group",
		"node.kubernetes.io/role",
		"topology.kubernetes.io/zone-labels",
		"argocd.argoproj.io/sync-wave",
		"kubectl.kubernetes.io/last-applied-configuration",
		AppliedLabelsAnnotation,
	} {
		assert.Contains(t, note, key)
	}
}

func TestApplyToleratesANodeThatHasNotJoined(t *testing.T) {
	// The machine has not registered, which is the normal state of a slot of an idle pool and of a
	// boot in flight. The labels land when the node object appears.
	env := newApplyEnv(map[string]string{TopologyZoneLabel: "fsn1"}, nil, nil)
	env.node = nil
	env.run(t)

	assert.Zero(t, env.patchCalls)
	assert.Empty(t, env.recorder.calls)
}

func TestApplySkipsASlotWithNoKubernetesNodeName(t *testing.T) {
	env := newApplyEnv(map[string]string{TopologyZoneLabel: "fsn1"}, nil, nil)
	env.slot.Spec.KubernetesNodeName = ""
	env.run(t)

	assert.Zero(t, env.patchCalls)
}

func TestApplyWithoutARecorderWritesTheNode(t *testing.T) {
	// The unit tests of the callers pass nil, and a nil recorder must not panic on the write path
	// that records the skips and the applied change.
	env := newApplyEnv(map[string]string{TopologyZoneLabel: "fsn1", "kubernetes.io/hostname": "spoofed"}, nil, nil)
	env.recorder = nil

	require.NoError(t, ApplyToKubernetesNode(t.Context(), env.client(t), env.slot, nil))

	node := env.applied(t)
	assert.Equal(t, "fsn1", node.Labels[TopologyZoneLabel])
}
