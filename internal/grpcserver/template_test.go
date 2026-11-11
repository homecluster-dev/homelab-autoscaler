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
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

// templateFor calls NodeGroupTemplateNodeInfo and returns the node object cluster-autoscaler
// will simulate a scale-up on.
func templateFor(t *testing.T, server *HomeClusterProviderServer) *corev1.Node {
	t.Helper()

	resp, err := server.NodeGroupTemplateNodeInfo(context.Background(), &pb.NodeGroupTemplateNodeInfoRequest{Id: testGroup})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotEmpty(t, resp.NodeBytes)

	node := &corev1.Node{}
	require.NoError(t, node.Unmarshal(resp.NodeBytes))

	return node
}

// taintStrings renders a taint list as "key=value:effect", so a failing assertion shows the
// taints that came out instead of a diff of structs.
func taintStrings(taints []corev1.Taint) []string {
	out := make([]string, 0, len(taints))
	for _, taint := range taints {
		out = append(out, taint.ToString())
	}

	return out
}

// nodeWithShape builds a Kubernetes node the way a machine that joined looks: the allocatable
// the kubelet reported, a Ready condition, and the taints the node carries.
func nodeWithShape(name string, ready bool, cpu, memory string, nodeTaints ...corev1.Taint) *corev1.Node {
	status, reason := corev1.ConditionFalse, "KubeletNotReady"
	if ready {
		status, reason = corev1.ConditionTrue, "KubeletReady"
	}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"infra.homecluster.dev/group": testGroup, "kubernetes.io/hostname": name},
		},
		Spec: corev1.NodeSpec{ProviderID: "homelab://" + name, Taints: nodeTaints},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: status,
				Reason: reason,
			}},
		},
	}
}

// slotWithTaints builds a Node CR of the test group that declares spec.nodeTaints.
func slotWithTaints(name string, desired, actual infrav1alpha1.PowerState, progress infrav1alpha1.Progress, nodeTaints ...corev1.Taint) *infrav1alpha1.Node {
	node := nodeCR(name, desired, actual, progress)
	node.Spec.NodeTaints = nodeTaints

	return node
}

// TestNodeGroupTemplateBuiltFromSlotWithRealNode is the regression a live cluster produced.
// The pool has powered off slots (status off, no Kubernetes node) and one running slot whose
// node object exists. The old policy preferred a powered off slot, so the template was the
// synthesized placeholder: no taints and no allocatable, and cluster-autoscaler simulated an
// untainted, zero-capacity machine while the real one boots tainted
// (dedicated=remote:NoSchedule) and has 8 CPU.
func TestNodeGroupTemplateBuiltFromSlotWithRealNode(t *testing.T) {
	dedicated := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}
	storage := corev1.Taint{Key: "storage", Value: "slow", Effect: corev1.TaintEffectNoSchedule}
	unschedulable := corev1.Taint{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule}

	server := newServer(t,
		nodeCR("slot-off-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		nodeCR("slot-off-2", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		slotWithTaints("slot-on", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady, dedicated),
		nodeWithShape("slot-on", true, "8", "16Gi", storage, unschedulable),
	)

	template := templateFor(t, server)

	assert.Equal(t, resource.MustParse("8"), template.Status.Allocatable[corev1.ResourceCPU],
		"the template must carry the capacity of a real node: the autoscaler judges requests against it")
	assert.Contains(t, taintStrings(template.Spec.Taints), dedicated.ToString(),
		"a taint the slot declares must be on the template, otherwise CA places pods the node rejects")
	assert.Contains(t, taintStrings(template.Spec.Taints), storage.ToString(),
		"a taint the representative node carries must survive")
	assert.NotContains(t, taintStrings(template.Spec.Taints), unschedulable.ToString(),
		"the transient unschedulable taint must stay filtered out of the template")
	assert.Equal(t, testGroup, template.Labels["infra.homecluster.dev/group"],
		"the labels of the representative node are the labels the new machine gets")

	// the template stays a schedulable template: unique name, Ready, not unschedulable
	assert.Contains(t, template.Name, testGroup+"-template-")
	assert.False(t, template.Spec.Unschedulable)
	require.NotEmpty(t, template.Status.Conditions)
	assert.Equal(t, corev1.NodeReady, template.Status.Conditions[0].Type)
	assert.Equal(t, corev1.ConditionTrue, template.Status.Conditions[0].Status)
}

// TestNodeGroupTemplatePrefersReadyNode covers the preference inside the first pass: a slot
// whose node object is the leftover of a powered off machine (NotReady) is a usable source, a
// Ready one is a better estimate of the shape of a machine that carries work.
func TestNodeGroupTemplatePrefersReadyNode(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-off", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		nodeWithShape("slot-off", false, "1", "1Gi"),
		nodeCR("slot-on", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
		nodeWithShape("slot-on", true, "16", "32Gi"),
	)

	template := templateFor(t, server)

	assert.Equal(t, resource.MustParse("16"), template.Status.Allocatable[corev1.ResourceCPU])
}

// TestNodeGroupTemplateWithoutAnyNodeSynthesizesPlaceholder covers a pool in which no slot has
// a machine. The provider still has to answer, and the honest answer is the shape the Node CR
// declares (the group label and the declared taints) and nothing about capacity: the size of a
// machine that has never registered cannot be read, and inventing it lets the autoscaler place
// work the real machine cannot take.
func TestNodeGroupTemplateWithoutAnyNodeSynthesizesPlaceholder(t *testing.T) {
	dedicated := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}

	server := newServer(t,
		slotWithTaints("slot-off-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown, dedicated),
		nodeCR("slot-off-2", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
	)

	template := templateFor(t, server)

	assert.Empty(t, template.Status.Allocatable, "a placeholder must not invent capacity")
	assert.Equal(t, []string{dedicated.ToString()}, taintStrings(template.Spec.Taints),
		"the placeholder carries the taints the slot declares")
	assert.Equal(t, testGroup, template.Labels["infra.homecluster.dev/group"])
	assert.Contains(t, template.Name, testGroup+"-template-")
}

// TestNodeGroupTemplateDeclaredTaintsWin covers the overlay on the node copied from a live
// Node: the Node CR is the intent for the machine the autoscaler is about to boot, the live
// node of the representative slot is what exists right now and can lag behind. Matching is by
// key, which is the uniqueness Kubernetes enforces on node taints.
func TestNodeGroupTemplateDeclaredTaintsWin(t *testing.T) {
	declared := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}
	stale := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoExecute}
	kept := corev1.Taint{Key: "storage", Value: "slow", Effect: corev1.TaintEffectNoSchedule}

	server := newServer(t,
		slotWithTaints("slot-on", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady, declared),
		nodeWithShape("slot-on", true, "8", "16Gi", stale, kept),
	)

	template := templateFor(t, server)

	assert.Equal(t, []string{declared.ToString(), kept.ToString()}, taintStrings(template.Spec.Taints),
		"the declared taint replaces the taint with the same key, a taint that is not declared is kept")
}

// TestNodeGroupTemplateKeepsTheTwoTaintFieldsApart keeps the two fields from being conflated:
// spec.taintsToRemove filters taints out of the template, spec.nodeTaints puts them in. A key
// in both ends up on the template, because the declaration is what the machine boots with.
func TestNodeGroupTemplateKeepsTheTwoTaintFieldsApart(t *testing.T) {
	declared := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}
	filtered := corev1.Taint{Key: "temp", Value: "gone", Effect: corev1.TaintEffectNoSchedule}

	slotCR := slotWithTaints("slot-on", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady, declared)
	slotCR.Spec.TaintsToRemove = []corev1.Taint{filtered, declared}

	server := newServer(t, slotCR, nodeWithShape("slot-on", true, "8", "16Gi", filtered))

	template := templateFor(t, server)

	assert.NotContains(t, taintStrings(template.Spec.Taints), filtered.ToString(),
		"taintsToRemove strips the taint from the template")
	assert.Contains(t, taintStrings(template.Spec.Taints), declared.ToString(),
		"nodeTaints puts the declared taint on the template, even for a key listed in taintsToRemove")
}

// TestNodeGroupTemplateUsesKubernetesNodeName covers how the representative node is looked up:
// spec.kubernetesNodeName is the name of the machine, the Node CR name is only the fallback,
// the same rule NodeGroupNodes uses.
func TestNodeGroupTemplateUsesKubernetesNodeName(t *testing.T) {
	slotCR := nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	slotCR.Spec.KubernetesNodeName = "worker-9"

	server := newServer(t, slotCR, nodeWithShape("worker-9", true, "12", "24Gi"))

	template := templateFor(t, server)

	assert.Equal(t, resource.MustParse("12"), template.Status.Allocatable[corev1.ResourceCPU])
}

// failingNodeClient reports a non-NotFound failure for corev1.Node lookups, which is what a
// permissions problem or a broken cache looks like to the provider.
type failingNodeClient struct {
	client.Client
	err error
}

func (c *failingNodeClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, isNode := obj.(*corev1.Node); isNode {
		return c.err
	}

	return c.Client.Get(ctx, key, obj, opts...)
}

// TestNodeGroupTemplateSurfacesLookupFailures keeps a real API failure visible: the
// placeholder exists for "no machine is up", not for "the provider cannot read the cluster",
// which is what the old single Get reported as Internal.
func TestNodeGroupTemplateSurfacesLookupFailures(t *testing.T) {
	base := newServer(t, nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady))
	server := NewHomeClusterProviderServer(
		&failingNodeClient{Client: base.Client, err: errors.New("connection refused")},
		base.Scheme,
	)

	_, err := server.NodeGroupTemplateNodeInfo(context.Background(), &pb.NodeGroupTemplateNodeInfoRequest{Id: testGroup})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get kubernetes node")
}
