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

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/taints"
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
func taintStrings(items []corev1.Taint) []string {
	out := make([]string, 0, len(items))
	for _, taint := range items {
		out = append(out, taint.ToString())
	}

	return out
}

// assertResource compares a resource the template must carry against the quantity it should hold.
// Cmp, not assert.Equal on the Quantity: the template makes a round trip through protobuf, which
// re-renders a BinarySI number (3.2Gi) as its milli-decimal equivalent (3435973836800m), and the
// two Quantity values are the same number in two representations.
func assertResource(t *testing.T, list corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()

	got, ok := list[name]
	require.Truef(t, ok, "%s is missing from the template", name)

	expected := resource.MustParse(want)
	assert.Equalf(t, 0, expected.Cmp(got), "%s: want %s, got %s", name, want, got.String())
}

// nodeWithShape builds a Kubernetes node the way a machine that joined looks: the allocatable
// the kubelet reported, a Ready condition, and the taints the node carries. cpu and memory are the
// part a caller varies; pods and ephemeral-storage are filled in, because every kubelet reports
// them and a fixture that leaves them out is a node no scheduler can place a pod on.
func nodeWithShape(name string, ready bool, cpu, memory string, nodeTaints ...corev1.Taint) *corev1.Node {
	return nodeWithResources(name, ready, corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse(cpu),
		corev1.ResourceMemory:           resource.MustParse(memory),
		corev1.ResourcePods:             resource.MustParse("110"),
		corev1.ResourceEphemeralStorage: resource.MustParse("40Gi"),
	}, nodeTaints...)
}

// nodeWithResources builds a registered Kubernetes node with an exact resource list, shared by
// capacity and allocatable, plus a Ready condition and the taints the node carries. It is the
// fixture for what a real kubelet reports beyond cpu and memory: a pod ceiling, the local disk,
// hugepages, and whatever a device plugin adds.
func nodeWithResources(name string, ready bool, resources corev1.ResourceList, nodeTaints ...corev1.Taint) *corev1.Node {
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
			Capacity:    resources.DeepCopy(),
			Allocatable: resources.DeepCopy(),
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: status,
				Reason: reason,
			}},
		},
	}
}

// slotWithTaints builds a Node CR of the test group that declares spec.nodeTaints.
func slotWithTaints(name string, actual infrav2alpha1.PowerState, progress infrav2alpha1.NodePhase, nodeTaints ...corev1.Taint) *infrav2alpha1.Node {
	node := nodeCR(name, actual, progress)
	node.Spec.NodeTaints = nodeTaints

	return node
}

// slotWithCapacity builds a Node CR of the test group that declares spec.nodeCapacity: the shape
// the slot's machine boots, as the kubelet reports it. The declaration names cpu and memory only,
// which is the documented shape and the one a real operator writes.
func slotWithCapacity(name string, actual infrav2alpha1.PowerState, progress infrav2alpha1.NodePhase, cpu, memory string) *infrav2alpha1.Node {
	return slotWithCapacityList(name, actual, progress, corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(memory),
	})
}

// slotWithCapacityList builds a Node CR of the test group that declares an exact spec.nodeCapacity,
// the fixture for a declaration that names more, or fewer, resources than cpu and memory.
func slotWithCapacityList(name string, actual infrav2alpha1.PowerState, progress infrav2alpha1.NodePhase, declared corev1.ResourceList) *infrav2alpha1.Node {
	node := nodeCR(name, actual, progress)
	node.Spec.NodeCapacity = declared

	return node
}

// allowedPodNumberOf mirrors the one derivation of the scheduler framework that decides whether a
// template can host anything at all: framework.NodeInfo.SetNode reads v1.ResourcePods out of
// status.allocatable and sets AllowedPodNumber from nothing else, and NodeResourcesFit
// .fitsRequest answers "Too many pods" when len(pods)+1 exceeds it, before its zero-request early
// return. A template with no pods in its allocatable therefore has AllowedPodNumber 0 and rejects
// every pod, so the estimator finds no placement and NodeGroupIncreaseSize is never called - the
// failure spec.nodeCapacity exists to close, and the reason the provider defaults the pod ceiling.
func allowedPodNumberOf(list corev1.ResourceList) int64 {
	pods, found := list[corev1.ResourcePods]
	if !found {
		return 0
	}

	return pods.Value()
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
		nodeCR("slot-off-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		nodeCR("slot-off-2", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		slotWithTaints("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady, dedicated),
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
		nodeCR("slot-off", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
		nodeWithShape("slot-off", false, "1", "1Gi"),
		nodeCR("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		nodeWithShape("slot-on", true, "16", "32Gi"),
	)

	template := templateFor(t, server)

	assert.Equal(t, resource.MustParse("16"), template.Status.Allocatable[corev1.ResourceCPU])
}

// TestNodeGroupTemplateWithoutAnyNodeSynthesizesPlaceholder covers a pool in which no slot has
// a machine. The provider still has to answer, and the honest answer is the shape the Node CR
// declares (the group label and the declared taints) and nothing about size: the cpu, memory and
// disk of a machine that has never registered cannot be read, and inventing them lets the
// autoscaler place work the real machine cannot take. The pod ceiling is the one number that is
// not a fact about the hardware - every kubelet has one, and the default is a constant - so the
// placeholder carries it, and a pod with requests still fits nowhere.
func TestNodeGroupTemplateWithoutAnyNodeSynthesizesPlaceholder(t *testing.T) {
	dedicated := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}

	server := newServer(t,
		slotWithTaints("slot-off-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff, dedicated),
		nodeCR("slot-off-2", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)

	template := templateFor(t, server)

	assert.NotContains(t, template.Status.Allocatable, corev1.ResourceCPU, "a placeholder must not invent a machine size")
	assert.NotContains(t, template.Status.Allocatable, corev1.ResourceMemory, "a placeholder must not invent a machine size")
	assert.NotContains(t, template.Status.Capacity, corev1.ResourceCPU, "a placeholder must not invent a machine size")
	assert.NotContains(t, template.Status.Capacity, corev1.ResourceMemory, "a placeholder must not invent a machine size")
	assert.Equal(t, template.Status.Allocatable, template.Status.Capacity,
		"a template machine has no system daemons to subtract, so capacity and allocatable are the same numbers")
	assert.Greater(t, allowedPodNumberOf(template.Status.Allocatable), int64(0),
		"the pod ceiling is defaulted, so the template is a node the scheduler can place on at all")
	assert.Equal(t, []string{dedicated.ToString()}, taintStrings(template.Spec.Taints),
		"the placeholder carries the taints the slot declares")
	assert.Equal(t, testGroup, template.Labels["infra.homecluster.dev/group"])
	assert.Contains(t, template.Name, testGroup+"-template-")
}

// TestNodeGroupTemplatePlaceholderCarriesDeclaredCapacity is the scale-from-zero case the field
// exists for. A pool in which no slot has ever booted has no corev1.Node, so its placeholder had
// an empty status.allocatable: cluster-autoscaler found no pod that fits and never called
// NodeGroupIncreaseSize, which is why a minSize: 0 pool stays at zero and the workaround is a
// machine that never sleeps. The declaration is the shape the placeholder can honestly carry,
// because it is written in Git next to the taints of the same machine.
func TestNodeGroupTemplatePlaceholderCarriesDeclaredCapacity(t *testing.T) {
	dedicated := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}

	server := newServer(t,
		slotWithCapacity("slot-off-1", infrav2alpha1.PowerStateOff,
			infrav2alpha1.NodePhaseOff, "1950m", "3.2Gi"),
		slotWithTaints("slot-off-2", infrav2alpha1.PowerStateOff,
			infrav2alpha1.NodePhaseOff, dedicated),
	)

	template := templateFor(t, server)

	// a pod with cpu requests must fit the template, that is what makes the pool scale from zero
	assertResource(t, template.Status.Allocatable, corev1.ResourceCPU, "1950m")
	assertResource(t, template.Status.Allocatable, corev1.ResourceMemory, "3.2Gi")
	assertResource(t, template.Status.Capacity, corev1.ResourceCPU, "1950m")
	assertResource(t, template.Status.Capacity, corev1.ResourceMemory, "3.2Gi")
	// The shape is the declaration plus the defaulted pod ceiling. Locking the list to exactly the
	// two declared resources locks the broken shape: the scheduler derives AllowedPodNumber from
	// v1.ResourcePods alone, so that template answers every pod "Too many pods".
	assertResource(t, template.Status.Allocatable, corev1.ResourcePods, "110")
	assert.Len(t, template.Status.Allocatable, 3,
		"the declared resources plus the defaulted pod ceiling, and nothing invented")
	assert.Equal(t, template.Status.Allocatable, template.Status.Capacity,
		"a template machine has no system daemons to subtract, so capacity and allocatable are the same list")
}

// TestNodeGroupTemplateDeclaredCapacityWithoutPodsStillAcceptsPods is the blocking shape of the
// feature. The documented declaration is cpu and memory, because that is what an operator reads off
// a machine; pods is a kubelet flag. Applied as a whole list, that declaration leaves the template
// with no v1.ResourcePods, framework.NodeInfo.SetNode derives AllowedPodNumber 0 from it, and
// NodeResourcesFit.fitsRequest answers "Too many pods" for every pod, before its zero-request early
// return, so the estimator returns 0 nodes, NodeGroupIncreaseSize is never called and a minSize: 0
// pool still cannot scale from zero. The provider fills the kubelet's default pod ceiling in.
func TestNodeGroupTemplateDeclaredCapacityWithoutPodsStillAcceptsPods(t *testing.T) {
	server := newServer(t,
		slotWithCapacity("slot-off-1", infrav2alpha1.PowerStateOff,
			infrav2alpha1.NodePhaseOff, "1950m", "3.2Gi"),
	)

	template := templateFor(t, server)

	assert.Greater(t, allowedPodNumberOf(template.Status.Allocatable), int64(0),
		"a declaration without pods must not leave the template at a pod ceiling of zero")
	assert.Greater(t, allowedPodNumberOf(template.Status.Capacity), int64(0),
		"capacity is read by cluster-autoscaler too, so it needs the pod ceiling as well")
	assertResource(t, template.Status.Allocatable, corev1.ResourcePods, "110")
	// the default must not displace the declaration
	assertResource(t, template.Status.Allocatable, corev1.ResourceCPU, "1950m")
}

// TestNodeGroupTemplateDeclaredCapacityWinsOverTheLiveNode covers the precedence on the path that
// copies a registered node: the declaration is the shape of the machine the autoscaler is about
// to boot, the live node is what one slot happens to report right now. Reading the live node
// would make the whole group's template change with which slot is Ready, in a mixed pool it
// hands the estimator a different machine per call.
func TestNodeGroupTemplateDeclaredCapacityWinsOverTheLiveNode(t *testing.T) {
	server := newServer(t,
		slotWithCapacity("slot-on", infrav2alpha1.PowerStateOn,
			infrav2alpha1.NodePhaseReady, "1950m", "3.2Gi"),
		nodeWithShape("slot-on", true, "8", "16Gi"),
	)

	template := templateFor(t, server)

	// the declaration wins over the 8 CPU / 16 GiB the representative slot reports
	assertResource(t, template.Status.Allocatable, corev1.ResourceCPU, "1950m")
	assertResource(t, template.Status.Allocatable, corev1.ResourceMemory, "3.2Gi")
	assertResource(t, template.Status.Capacity, corev1.ResourceCPU, "1950m")
	assertResource(t, template.Status.Capacity, corev1.ResourceMemory, "3.2Gi")
	// and it wins resource by resource: what the slot reports that the declaration does not name
	// stays, which is what a cpu/memory declaration from the docs has to leave alone
	assertResource(t, template.Status.Allocatable, corev1.ResourcePods, "110")
	assertResource(t, template.Status.Allocatable, corev1.ResourceEphemeralStorage, "40Gi")
}

// TestNodeGroupTemplateLiveNodeResourcesSurviveTheDeclaration is the other half of "the declared
// values win": winning is per resource, the declaration does not replace the list. A real kubelet
// reports a pod ceiling, the local disk, hugepages, and every device plugin resource next to cpu
// and memory, and the estimator judges a pod against all of them. A 2 vCPU box that carries a GPU is
// still a box with a GPU after the declaration is applied, and a kubelet configured with --max-pods
// 250 keeps 250, so the provider does not lower a real machine to the default.
func TestNodeGroupTemplateLiveNodeResourcesSurviveTheDeclaration(t *testing.T) {
	gpu := corev1.ResourceName("nvidia.com/gpu")
	hugepages := corev1.ResourceName(corev1.ResourceHugePagesPrefix + "2Mi")

	server := newServer(t,
		slotWithCapacity("slot-on", infrav2alpha1.PowerStateOn,
			infrav2alpha1.NodePhaseReady, "1950m", "3.2Gi"),
		nodeWithResources("slot-on", true, corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("8"),
			corev1.ResourceMemory:           resource.MustParse("16Gi"),
			corev1.ResourcePods:             resource.MustParse("250"),
			corev1.ResourceEphemeralStorage: resource.MustParse("80Gi"),
			hugepages:                       resource.MustParse("1Gi"),
			gpu:                             resource.MustParse("2"),
		}),
	)

	template := templateFor(t, server)

	for _, list := range []struct {
		name  string
		items corev1.ResourceList
	}{
		{"status.allocatable", template.Status.Allocatable},
		{"status.capacity", template.Status.Capacity},
	} {
		// the two declared resources are the declaration
		assertResource(t, list.items, corev1.ResourceCPU, "1950m")
		assertResource(t, list.items, corev1.ResourceMemory, "3.2Gi")
		// everything the machine reports that the declaration does not name is the machine's
		assertResource(t, list.items, corev1.ResourcePods, "250")
		assertResource(t, list.items, corev1.ResourceEphemeralStorage, "80Gi")
		assertResource(t, list.items, hugepages, "1Gi")
		assertResource(t, list.items, gpu, "2")
		assert.Len(t, list.items, 6, "%s: the declared resources overlaid on the node's own list", list.name)
	}
}

// TestNodeGroupTemplateFallsBackToTheLiveNodeAllocatable keeps the today-behaviour for a pool that
// declares nothing: the kubelet's measured numbers are a better estimate than an empty template,
// and no existing pool is asked to write the field to keep working.
func TestNodeGroupTemplateFallsBackToTheLiveNodeAllocatable(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		nodeWithShape("slot-on", true, "8", "16Gi"),
	)

	template := templateFor(t, server)

	// with no declaration the live node's allocatable is used
	assertResource(t, template.Status.Allocatable, corev1.ResourceCPU, "8")
	assertResource(t, template.Status.Allocatable, corev1.ResourceMemory, "16Gi")
	assertResource(t, template.Status.Capacity, corev1.ResourceCPU, "8")
	assertResource(t, template.Status.Capacity, corev1.ResourceMemory, "16Gi")
}

// TestApplyTemplateCapacityDoesNotAliasTheDeclaration guards the copy the precedence needs. The
// Node CR the provider reads comes out of the manager's cache, so putting its map into the
// template would hand every later reader a template-shaped object, and capacity and allocatable
// would be one map: clearing one in a later step would clear the other.
func TestApplyTemplateCapacityDoesNotAliasTheDeclaration(t *testing.T) {
	slotCR := slotWithCapacity("slot-1", infrav2alpha1.PowerStateOff,
		infrav2alpha1.NodePhaseOff, "1950m", "3.2Gi")
	template := &corev1.Node{}

	applyTemplateCapacity(template, slotCR)

	template.Status.Allocatable[corev1.ResourceCPU] = resource.MustParse("1")
	template.Status.Capacity[corev1.ResourceMemory] = resource.MustParse("16Gi")

	assert.Equal(t, resource.MustParse("1950m"), slotCR.Spec.NodeCapacity[corev1.ResourceCPU],
		"the declaration is copied, not shared with the template")
	assert.Equal(t, resource.MustParse("3.2Gi"), slotCR.Spec.NodeCapacity[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("3.2Gi"), template.Status.Allocatable[corev1.ResourceMemory],
		"capacity and allocatable are two lists, not one map under two names")
}

// TestApplyTemplateCapacityLeavesAnUndeclaredSlotAlone covers the fallback half of the precedence
// at the unit level: the numbers the copy carried, from the live node, survive untouched.
func TestApplyTemplateCapacityLeavesAnUndeclaredSlotAlone(t *testing.T) {
	slotCR := nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	template := nodeWithShape("slot-1", true, "8", "16Gi")

	applyTemplateCapacity(template, slotCR)

	assert.Equal(t, resource.MustParse("8"), template.Status.Allocatable[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("16Gi"), template.Status.Capacity[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("110"), template.Status.Allocatable[corev1.ResourcePods],
		"the pod ceiling the node reports is kept, it is not lowered to the default")
}

// TestApplyTemplateCapacityOverlaysTheDeclarationPerResource is the unit level of the same rule:
// the declared cpu replaces the live cpu, the live pod ceiling, disk and GPU survive, and the
// declaration is applied to capacity as well as allocatable.
func TestApplyTemplateCapacityOverlaysTheDeclarationPerResource(t *testing.T) {
	gpu := corev1.ResourceName("nvidia.com/gpu")
	slotCR := slotWithCapacity("slot-1", infrav2alpha1.PowerStateOff,
		infrav2alpha1.NodePhaseOff, "3900m", "7.8Gi")
	template := nodeWithResources("slot-1", true, corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("8"),
		corev1.ResourceMemory:           resource.MustParse("16Gi"),
		corev1.ResourcePods:             resource.MustParse("250"),
		corev1.ResourceEphemeralStorage: resource.MustParse("80Gi"),
		gpu:                             resource.MustParse("2"),
	})

	applyTemplateCapacity(template, slotCR)

	for _, items := range []corev1.ResourceList{template.Status.Capacity, template.Status.Allocatable} {
		assert.Equal(t, resource.MustParse("3900m"), items[corev1.ResourceCPU], "the declaration wins for cpu")
		assert.Equal(t, resource.MustParse("7.8Gi"), items[corev1.ResourceMemory], "the declaration wins for memory")
		assert.Equal(t, resource.MustParse("250"), items[corev1.ResourcePods], "a resource the declaration omits stays")
		assert.Equal(t, resource.MustParse("80Gi"), items[corev1.ResourceEphemeralStorage], "the disk stays")
		assert.Equal(t, resource.MustParse("2"), items[gpu], "the GPU stays: a pod asking for it must fit")
	}
}

// TestApplyTemplateCapacityDefaultsThePodCeilingOnAnEmptyTemplate is the placeholder half: the
// object starts with nothing, so the pod ceiling comes from the default and no machine size is
// added. This is the shape the documented cpu/memory declaration lands on, and the one the
// scheduler can place a pod on at all.
func TestApplyTemplateCapacityDefaultsThePodCeilingOnAnEmptyTemplate(t *testing.T) {
	slotCR := slotWithCapacity("slot-1", infrav2alpha1.PowerStateOff,
		infrav2alpha1.NodePhaseOff, "1950m", "3.2Gi")
	template := &corev1.Node{}

	applyTemplateCapacity(template, slotCR)

	assertResource(t, template.Status.Allocatable, corev1.ResourcePods, "110")
	assertResource(t, template.Status.Capacity, corev1.ResourcePods, "110")
	assertResource(t, template.Status.Allocatable, corev1.ResourceCPU, "1950m")
	assert.NotContains(t, template.Status.Allocatable, corev1.ResourceEphemeralStorage,
		"the default is the pod ceiling only, the provider does not invent a disk")
}

// TestApplyTemplateCapacityDoesNotAliasTheDefaults guards the package level default list. It is
// shared by every call, and a Quantity carries a cached rendering and an optional pointer to its
// internal representation: putting the default value into a template, and letting a caller (or the
// protobuf round trip) touch it, would rewrite the default for every later pool.
func TestApplyTemplateCapacityDoesNotAliasTheDefaults(t *testing.T) {
	slotCR := nodeCR("slot-1", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	template := &corev1.Node{}

	applyTemplateCapacity(template, slotCR)

	pods := template.Status.Allocatable[corev1.ResourcePods]
	pods.Add(resource.MustParse("1"))
	template.Status.Allocatable[corev1.ResourcePods] = pods
	template.Status.Capacity[corev1.ResourcePods] = resource.MustParse("1")

	shared := defaultTemplateCapacity[corev1.ResourcePods]
	assert.Equal(t, int64(defaultTemplateMaxPods), shared.Value(),
		"the shared default is not reachable through the template")
	// capacity and allocatable are two lists, and each carries its own copy of the default
	assertResource(t, template.Status.Allocatable, corev1.ResourcePods, "111")
	assertResource(t, template.Status.Capacity, corev1.ResourcePods, "1")

	second := &corev1.Node{}
	applyTemplateCapacity(second, slotCR)
	assertResource(t, second.Status.Allocatable, corev1.ResourcePods, "110")
	assertResource(t, second.Status.Capacity, corev1.ResourcePods, "110")
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
		slotWithTaints("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady, declared),
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

	slotCR := slotWithTaints("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady, declared)
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
	slotCR := nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
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

// TestNodeGroupTemplateKeepsThePoolsOwnRoleTaint is the prefix rule's cost, measured on the
// template. The k3d e2e, examples/k3d and config/manager taint agent nodes with
// node.kubernetes.io/role=agent, and the deployment that runs on them tolerates exactly that.
// A prefix-based protected-key rule dropped the declaration from the template, so the estimator
// saw an untainted machine and placed pods the node rejects - the failure spec.nodeTaints
// removes. The transient lifecycle taints next to it stay filtered out.
func TestNodeGroupTemplateKeepsThePoolsOwnRoleTaint(t *testing.T) {
	role := corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}
	unschedulable := corev1.Taint{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule}

	server := newServer(t,
		slotWithTaints("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady, role),
		nodeWithShape("slot-on", true, "8", "16Gi", role, unschedulable),
	)

	template := templateFor(t, server)

	assert.Contains(t, taintStrings(template.Spec.Taints), role.ToString(),
		"the taint the pool declares and the workload tolerates must be in the template")
	assert.NotContains(t, taintStrings(template.Spec.Taints), unschedulable.ToString(),
		"the transient taint of a cordoned node is still stripped")
}

// TestNodeGroupTemplatePlaceholderKeepsThePoolsOwnRoleTaint covers the same rule on the
// placeholder path, a fully powered off pool: scale-from-zero simulations judge the pod against
// the declared taints, so the pool's own key must survive the protected-key filter there too.
func TestNodeGroupTemplatePlaceholderKeepsThePoolsOwnRoleTaint(t *testing.T) {
	role := corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}

	server := newServer(t,
		slotWithTaints("slot-off", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff, role),
	)

	template := templateFor(t, server)

	assert.Equal(t, []string{role.ToString()}, taintStrings(template.Spec.Taints))
}

// TestNodeGroupTemplateDropsClusterAutoscalerScaleDownTaints is the cost of reading the template
// from a live node. A Ready node that cluster-autoscaler is currently evaluating for scale-down
// carries DeletionCandidateOfClusterAutoscaler; copied into the template of the whole group, it
// is a taint no pod tolerates, so the estimator finds no placement and never scales up. Both CA
// keys are filtered out of the template, and the pools own taint is not, which is why this list
// is spelled out per key instead of reusing taints.IsProtectedKey.
func TestNodeGroupTemplateDropsClusterAutoscalerScaleDownTaints(t *testing.T) {
	role := corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}
	deletionCandidate := corev1.Taint{Key: taints.DeletionCandidateOfClusterAutoscaler, Effect: corev1.TaintEffectNoSchedule}
	toBeDeleted := corev1.Taint{Key: taints.ToBeDeletedByClusterAutoscaler, Effect: corev1.TaintEffectNoSchedule}
	notReady := corev1.Taint{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule}
	unreachable := corev1.Taint{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoSchedule}

	server := newServer(t,
		slotWithTaints("slot-on", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady, role),
		nodeWithShape("slot-on", true, "8", "16Gi", role, deletionCandidate, toBeDeleted, notReady, unreachable),
	)

	template := templateFor(t, server)
	got := taintStrings(template.Spec.Taints)

	assert.NotContains(t, got, deletionCandidate.ToString(),
		"a node CA is evaluating for scale-down must not taint the whole group's template")
	assert.NotContains(t, got, toBeDeleted.ToString())
	assert.NotContains(t, got, notReady.ToString())
	assert.NotContains(t, got, unreachable.ToString())
	assert.Equal(t, []string{role.ToString()}, got,
		"the pool's own taint survives, so the estimator still matches the toleration")
}

// nodeGetFailureClient fails the corev1.Node lookup of one name with a real error and lets the
// other lookups report "the node does not exist", the mix that hid a failure behind a placeholder.
type nodeGetFailureClient struct {
	client.Client
	failFor string
	err     error
}

func (c *nodeGetFailureClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, isNode := obj.(*corev1.Node); isNode && key.Name == c.failFor {
		return c.err
	}

	return c.Client.Get(ctx, key, obj, opts...)
}

// TestNodeGroupTemplateRealFailureSurvivesANotFoundSlot keeps pickTemplateSource honest when the
// pool mixes a slot the provider cannot read with a slot that has no node object: the last slot
// visited must not overwrite the only interesting error, because "this pool is powered off" is
// not a good answer for "the provider cannot talk to the API".
func TestNodeGroupTemplateRealFailureSurvivesANotFoundSlot(t *testing.T) {
	base := newServer(t,
		nodeCR("a-slot", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
		nodeCR("z-slot", infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff),
	)
	server := NewHomeClusterProviderServer(
		&nodeGetFailureClient{Client: base.Client, failFor: "a-slot", err: errors.New("connection refused")},
		base.Scheme,
	)

	_, err := server.NodeGroupTemplateNodeInfo(context.Background(), &pb.NodeGroupTemplateNodeInfoRequest{Id: testGroup})
	require.Error(t, err, "the z-slot's NotFound must not turn a real failure into a placeholder")
	assert.Contains(t, err.Error(), "failed to get kubernetes node")
	assert.Contains(t, err.Error(), "connection refused", "the reported error is the one that is worth reporting")
}

// TestNodeGroupTemplateSurfacesLookupFailures keeps a real API failure visible: the
// placeholder exists for "no machine is up", not for "the provider cannot read the cluster",
// which is what the old single Get reported as Internal.
func TestNodeGroupTemplateSurfacesLookupFailures(t *testing.T) {
	base := newServer(t, nodeCR("slot-1", infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady))
	server := NewHomeClusterProviderServer(
		&failingNodeClient{Client: base.Client, err: errors.New("connection refused")},
		base.Scheme,
	)

	_, err := server.NodeGroupTemplateNodeInfo(context.Background(), &pb.NodeGroupTemplateNodeInfoRequest{Id: testGroup})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get kubernetes node")
}
