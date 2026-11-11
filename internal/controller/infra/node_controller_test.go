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
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/capacity"
	"github.com/homecluster-dev/homelab-autoscaler/internal/taints"
)

// taintNodeCR returns the shared test Node CR with spec.nodeTaints declared.
func taintNodeCR(declared ...corev1.Taint) *infrav1alpha1.Node {
	node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	node.Status.PowerState = infrav1alpha1.PowerStateOn
	node.Spec.NodeTaints = declared

	return node
}

// taintK8sNode returns the Kubernetes node the shared test Node CR points at, with the given
// taints already on it.
func taintK8sNode(existing ...corev1.Taint) *corev1.Node {
	k8sNode := createTestKubernetesNode()
	k8sNode.Spec.Taints = existing

	return k8sNode
}

// requeueFiveMinutes is the steady-state requeue of the node controller, asserted here so the
// reconcile-level test states what it expects from the loop.
const requeueFiveMinutes = 5 * time.Minute

func newTaintReconciler(t *testing.T, objects ...client.Object) *NodeReconciler {
	t.Helper()

	scheme := setupTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1alpha1.Node{}).
		Build()

	return &NodeReconciler{Client: fakeClient, Scheme: scheme}
}

// getK8sNode reads the Kubernetes node the shared test Node CR points at.
func getK8sNode(t *testing.T, c client.Client) *corev1.Node {
	t.Helper()

	node := &corev1.Node{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "test-node-k8s"}, node))

	return node
}

// TestApplyNodeTaints covers the semantics of spec.nodeTaints on the live node: a declared
// taint is added, an existing taint with the same key is overwritten, taints that are not
// declared survive, taints owned by cluster-autoscaler and by the node lifecycle controllers
// are never written, and a reconcile that changes nothing does not patch.
func TestApplyNodeTaints(t *testing.T) {
	dedicated := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}
	storage := corev1.Taint{Key: "storage", Value: "slow", Effect: corev1.TaintEffectNoSchedule}

	tests := []struct {
		name string
		// declared is spec.nodeTaints of the Node CR
		declared []corev1.Taint
		// existing is what the Kubernetes node carries before the reconcile
		existing []corev1.Taint
		// want is the taint list the node must carry afterwards
		want []corev1.Taint
		// patched is whether a write to the node is expected
		patched bool
	}{
		{
			name:     "a declared taint is added",
			declared: []corev1.Taint{dedicated},
			existing: []corev1.Taint{storage},
			want:     []corev1.Taint{storage, dedicated},
			patched:  true,
		},
		{
			name:     "an unrelated taint is kept",
			declared: []corev1.Taint{dedicated},
			existing: []corev1.Taint{{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule}, storage},
			want:     []corev1.Taint{{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule}, storage, dedicated},
			patched:  true,
		},
		{
			name:     "nothing is patched when the declared taint is already there",
			declared: []corev1.Taint{dedicated},
			existing: []corev1.Taint{dedicated},
			want:     []corev1.Taint{dedicated},
			patched:  false,
		},
		{
			name:     "a declared taint overwrites the taint with the same key",
			declared: []corev1.Taint{dedicated},
			existing: []corev1.Taint{{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoExecute}},
			want:     []corev1.Taint{dedicated},
			patched:  true,
		},
		{
			name:     "a declared taint overwrites the value of the taint with the same key",
			declared: []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}},
			existing: []corev1.Taint{{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}},
			want:     []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}},
			patched:  true,
		},
		{
			name:     "a taint owned by cluster-autoscaler is not written",
			declared: []corev1.Taint{{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule}},
			existing: []corev1.Taint{storage},
			want:     []corev1.Taint{storage},
			patched:  false,
		},
		{
			// node.kubernetes.io/role is the taint this repository's k3d e2e, examples/k3d and
			// config/manager use, and the key the docs tell operators to migrate into this field.
			// A prefix rule on node.kubernetes.io/ swallowed it, which is the "CA simulates the
			// node without the taint" failure this PR removes.
			name:     "the pool's own node.kubernetes.io/role taint is applied",
			declared: []corev1.Taint{{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}},
			existing: []corev1.Taint{storage},
			want:     []corev1.Taint{storage, {Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}},
			patched:  true,
		},
		{
			name:     "an old-beta key is applied, nothing owns a taint in that namespace",
			declared: []corev1.Taint{{Key: "beta.kubernetes.io/arch", Value: "arm64", Effect: corev1.TaintEffectNoSchedule}},
			existing: nil,
			want:     []corev1.Taint{{Key: "beta.kubernetes.io/arch", Value: "arm64", Effect: corev1.TaintEffectNoSchedule}},
			patched:  true,
		},
		{
			name:     "a standard node lifecycle taint is not written",
			declared: []corev1.Taint{{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoSchedule}},
			existing: nil,
			want:     nil,
			patched:  false,
		},
		{
			// The inverse of the additive rule: removing a declared taint by hand is not a
			// removal, the next reconcile puts it back, because the declaration is the intent.
			name:     "a taint removed by hand is re-added",
			declared: []corev1.Taint{dedicated},
			existing: nil,
			want:     []corev1.Taint{dedicated},
			patched:  true,
		},
		{
			name:     "nothing is declared, so the node is left alone",
			declared: nil,
			existing: []corev1.Taint{storage},
			want:     []corev1.Taint{storage},
			patched:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := taintNodeCR(tc.declared...)
			k8sNode := taintK8sNode(tc.existing...)
			reconciler := newTaintReconciler(t, node, createTestGroup(), k8sNode)

			before := getK8sNode(t, reconciler.Client)
			require.NoError(t, reconciler.applyNodeTaintsToKubernetesNode(context.Background(), node))
			after := getK8sNode(t, reconciler.Client)

			assert.Equal(t, tc.want, after.Spec.Taints)
			assert.Equal(t, tc.patched, before.ResourceVersion != after.ResourceVersion,
				"patched must be %v", tc.patched)

			// running the same reconcile twice must not write either way, so the
			// controller does not churn the node object every loop
			twice := getK8sNode(t, reconciler.Client)
			require.NoError(t, reconciler.applyNodeTaintsToKubernetesNode(context.Background(), node))
			thrice := getK8sNode(t, reconciler.Client)
			assert.Equal(t, twice.ResourceVersion, thrice.ResourceVersion, "applying taints must be idempotent")
			assert.Equal(t, tc.want, thrice.Spec.Taints)
		})
	}
}

// TestApplyNodeTaintsWithoutKubernetesNode covers a slot whose machine has not joined the
// cluster: the node object does not exist yet, and that is the normal state of a fresh slot,
// so it must not be reported as a failure.
func TestApplyNodeTaintsWithoutKubernetesNode(t *testing.T) {
	dedicated := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}

	// the node object is missing
	node := taintNodeCR(dedicated)
	reconciler := newTaintReconciler(t, node, createTestGroup())
	require.NoError(t, reconciler.applyNodeTaintsToKubernetesNode(context.Background(), node))

	// spec.kubernetesNodeName is empty, so there is nothing to look up
	fresh := taintNodeCR(dedicated)
	fresh.Spec.KubernetesNodeName = ""
	require.NoError(t, reconciler.applyNodeTaintsToKubernetesNode(context.Background(), fresh))
}

// TestNodeReconcileAppliesDeclaredTaints wires the field through Reconcile: the taint must
// land on the node on the same pass that stamps the providerID, so the autoscaler's template
// (which reads the node) and the node agree.
func TestNodeReconcileAppliesDeclaredTaints(t *testing.T) {
	dedicated := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}
	storage := corev1.Taint{Key: "storage", Value: "slow", Effect: corev1.TaintEffectNoSchedule}

	node := taintNodeCR(dedicated)
	k8sNode := taintK8sNode(storage)
	reconciler := newTaintReconciler(t, node, createTestGroup(), k8sNode)

	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: requeueFiveMinutes}, result)

	assert.Equal(t, []corev1.Taint{storage, dedicated}, getK8sNode(t, reconciler.Client).Spec.Taints)
}

// TestApplyNodeTaintsSurfacesSkippedKeys covers what happens to a declaration the controller
// refuses to write. Skipping it silently is the failure mode this field exists to remove: the
// Node CR says the machine is tainted, the node is not, and cluster-autoscaler simulates a node
// the pool does not boot. So the skip must be visible from outside the controller's logs - a
// Warning event on the Node CR, readable with `kubectl describe` - while the keys the operator
// does own, node.kubernetes.io/role first among them, are applied normally.
func TestApplyNodeTaintsSurfacesSkippedKeys(t *testing.T) {
	dedicated := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}
	role := corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}
	notReady := corev1.Taint{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule}
	deletionCandidate := corev1.Taint{Key: "DeletionCandidateOfClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule}

	recorder := events.NewFakeRecorder(8)

	node := taintNodeCR(notReady, role, dedicated, deletionCandidate)
	reconciler := newTaintReconciler(t, node, createTestGroup(), taintK8sNode())
	reconciler.EventRecorder = recorder

	require.NoError(t, reconciler.applyNodeTaintsToKubernetesNode(context.Background(), node))

	// the owned keys landed, the owned-by-Kubernetes keys did not
	assert.Equal(t, []corev1.Taint{role, dedicated}, getK8sNode(t, reconciler.Client).Spec.Taints)

	require.NotEmpty(t, recorder.Events, "the fake recorder must expose its channel")
	require.Equal(t, 1, len(recorder.Events), "one reconcile records one event, not one per key")

	event := <-recorder.Events
	assert.Contains(t, event, "Warning", "a drop that stops the pool from scaling must not be a Normal event")
	assert.Contains(t, event, taints.ReasonSkippedProtectedKey)
	assert.Contains(t, event, "node.kubernetes.io/not-ready", "the event names the key that was dropped")
	assert.Contains(t, event, "DeletionCandidateOfClusterAutoscaler")
	assert.NotContains(t, event, "node.kubernetes.io/role", "the event must not claim a key it applied")
	assert.Contains(t, event, "test-node", "the event says which slot dropped the declaration")
}

// TestApplyNodeTaintsRecordsNoEventWhenNothingIsSkipped keeps the event stream clean: a healthy
// declaration must not put an Event on every Node CR on every reconcile.
func TestApplyNodeTaintsRecordsNoEventWhenNothingIsSkipped(t *testing.T) {
	dedicated := corev1.Taint{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}

	recorder := events.NewFakeRecorder(8)

	node := taintNodeCR(dedicated)
	reconciler := newTaintReconciler(t, node, createTestGroup(), taintK8sNode())
	reconciler.EventRecorder = recorder

	require.NoError(t, reconciler.applyNodeTaintsToKubernetesNode(context.Background(), node))
	assert.Equal(t, []corev1.Taint{dedicated}, getK8sNode(t, reconciler.Client).Spec.Taints)
	assert.Zero(t, len(recorder.Events), "nothing was skipped, so nothing was recorded")
}

// capacityNodeCR returns the shared test Node CR that declares spec.nodeCapacity: the shape a
// 2 vCPU / 4 GiB machine reports once the kubelet subtracted its system reservation, 1950m of cpu
// and 3.2Gi of memory.
func capacityNodeCR() *infrav1alpha1.Node {
	node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	node.Status.PowerState = infrav1alpha1.PowerStateOn
	node.Spec.NodeCapacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1950m"),
		corev1.ResourceMemory: resource.MustParse("3.2Gi"),
	}

	return node
}

// capacityK8sNode returns the shared test Kubernetes node with the allocatable a kubelet reported,
// Ready unless ready is false.
func capacityK8sNode(cpu, memory string, ready bool) *corev1.Node {
	node := createTestKubernetesNode()
	node.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(memory),
	}

	if !ready {
		node.Status.Conditions[0].Status = corev1.ConditionFalse
		node.Status.Conditions[0].Reason = "KubeletNotReady"
	}

	return node
}

// TestDeclaredNodeCapacityDriftWarns covers a declaration that stopped matching the machine: the
// VM was resized, the system reservation moved, the numbers came off the datasheet instead of off
// a booted node. A stale declaration makes the pool unplaceable, cluster-autoscaler simulates a
// machine the pool does not boot, and nothing in the cluster says so. The check is a Warning event
// naming the drifted resource and both values, and nothing else.
func TestDeclaredNodeCapacityDriftWarns(t *testing.T) {
	recorder := events.NewFakeRecorder(8)

	node := capacityNodeCR()
	reconciler := newTaintReconciler(t, node, createTestGroup(), capacityK8sNode("1", "3.2Gi", true))
	reconciler.EventRecorder = recorder

	require.NoError(t, reconciler.checkDeclaredNodeCapacity(context.Background(), node))

	require.Equal(t, 1, len(recorder.Events), "one reconcile records one event for the drift it saw")

	event := <-recorder.Events
	assert.Contains(t, event, "Warning", "a stale declaration stops the pool from scaling, so it is not a Normal event")
	assert.Contains(t, event, capacity.ReasonCapacityMismatch)
	assert.Contains(t, event, "cpu", "the event names the resource that drifted")
	assert.Contains(t, event, "1950m", "the event carries the declared value")
	assert.Contains(t, event, "spec.nodeCapacity", "the event says which field to fix")
	assert.Contains(t, event, "test-node", "the event says which slot declares it")
	assert.NotContains(t, event, "memory", "the resource that matches is not named")

	// the node keeps the kubelet's numbers: the declaration is a warning, never a write
	allocatable := getK8sNode(t, reconciler.Client).Status.Allocatable
	require.Len(t, allocatable, 2)

	reported := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1"),
		corev1.ResourceMemory: resource.MustParse("3.2Gi"),
	}

	for name, want := range reported {
		got, ok := allocatable[name]
		require.Truef(t, ok, "%s is still reported by the kubelet", name)
		assert.Equalf(t, 0, want.Cmp(got), "%s: the drift is warned about, not written away", name)
	}
}

// TestDeclaredNodeCapacityRecordsNothing covers the passes that must not put an Event on the Node
// CR: the healthy declaration, the pool that declares nothing, and the machine whose kubelet has
// not reported allocatable yet.
func TestDeclaredNodeCapacityRecordsNothing(t *testing.T) {
	tests := []struct {
		name     string
		declared *infrav1alpha1.Node
		live     *corev1.Node
	}{
		{
			name:     "allocatable matches the declaration",
			declared: capacityNodeCR(),
			live:     capacityK8sNode("1950m", "3.2Gi", true),
		},
		{
			name:     "allocatable is within the tolerance of the declaration",
			declared: capacityNodeCR(),
			live:     capacityK8sNode("2", "3.1Gi", true),
		},
		{
			name:     "the slot declares nothing, so nothing can be stale",
			declared: createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
			live:     capacityK8sNode("1", "1Gi", true),
		},
		{
			name:     "the node is not Ready, so its allocatable is not a measurement",
			declared: capacityNodeCR(),
			live:     capacityK8sNode("1", "1Gi", false),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)

			reconciler := newTaintReconciler(t, tc.declared, createTestGroup(), tc.live)
			reconciler.EventRecorder = recorder

			require.NoError(t, reconciler.checkDeclaredNodeCapacity(context.Background(), tc.declared))
			assert.Zero(t, len(recorder.Events), "no drift, no event")
		})
	}
}

// TestNodeReconcileChecksDeclaredCapacity wires the check through Reconcile, so the drift is
// reported on the same pass that stamps the providerID and the declared taints: an operator
// reading `kubectl describe node.infra.homecluster.dev <name>` sees the shape the autoscaler is
// simulating, the taints it simulates, and the reason the pool does not scale, in one place.
func TestNodeReconcileChecksDeclaredCapacity(t *testing.T) {
	recorder := events.NewFakeRecorder(16)

	node := capacityNodeCR()
	reconciler := newTaintReconciler(t, node, createTestGroup(), capacityK8sNode("1", "3.2Gi", true))
	reconciler.EventRecorder = recorder

	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: requeueFiveMinutes}, result)

	require.NotEmpty(t, recorder.Events, "Reconcile must run the capacity check, not only the taint one")

	var found bool

	for _, event := range drainEvents(recorder) {
		if strings.Contains(event, capacity.ReasonCapacityMismatch) {
			assert.Contains(t, event, "Warning")
			assert.Contains(t, event, "cpu")
			found = true
		}
	}

	assert.True(t, found, "the reconcile pass records the drift between spec.nodeCapacity and the node")
}

// drainEvents empties the fake recorder's channel and returns everything it buffered. The fake
// recorder is a fixed-size channel, so a test that reads one event has to leave the rest.
func drainEvents(recorder *events.FakeRecorder) []string {
	var collected []string

	for {
		select {
		case event := <-recorder.Events:
			collected = append(collected, event)
		default:
			return collected
		}
	}
}
