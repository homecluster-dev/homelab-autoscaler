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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	autoscalerpkg "github.com/homecluster-dev/homelab-autoscaler/internal/autoscaler"
	"github.com/homecluster-dev/homelab-autoscaler/internal/capacity"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
	"github.com/homecluster-dev/homelab-autoscaler/internal/taints"
)

// taintNodeCR returns the shared test Node CR with spec.nodeTaints declared.
func taintNodeCR(declared ...corev1.Taint) *infrav2alpha1.Node {
	// An idle slot: observed off, and no live claim names it. That is the shape a pass over it
	// leaves alone (law: a machine is wanted exactly when a live claim wants it), so the reconcile
	// level tests below assert the plumbing they are about, and not a lifecycle action.
	node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
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
		WithStatusSubresource(&infrav2alpha1.Node{}).
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
func capacityNodeCR() *infrav2alpha1.Node {
	// An idle slot again: the declaration is read on a pass that takes no lifecycle action, so the
	// reconcile-level assertion below is about the check, not about a shutdown.
	node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
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
// a booted node. A stale declaration silently sizes the estimator's simulated machine wrong, so the
// pool over-provisions work the booted node cannot take or leaves it idle, and nothing in the
// cluster says so. The check is a Warning event naming the drifted resource and both values, and
// nothing else.
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
		declared *infrav2alpha1.Node
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
			declared: createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady),
			// A live claim of this slot is what makes the machine it reports wanted; the check
			// itself reads the declaration and the node object only.
			live: capacityK8sNode("1", "1Gi", true),
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

// listLifecycleJobs returns every startup/shutdown Job the controller created.
func listLifecycleJobs(t *testing.T, c client.Client) []batchv1.Job {
	t.Helper()

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(context.Background(), jobs))

	return jobs.Items
}

// TestReadinessTaintKeysStampedFromTheGroup is the wiring of the configurable gate key: the key belongs
// to the CNI of the cluster, so it is read from the slot's Group and recorded on the slot, where an
// operator looking at a slot that has been booting for ten minutes can read what it is waiting for. An
// empty list is a real answer (the gate is off), and it is recorded as one.
func TestReadinessTaintKeysStampedFromTheGroup(t *testing.T) {
	tests := []struct {
		name string
		spec []corev1.Taint
		want []string
	}{
		{
			name: "the group declares a CNI key",
			spec: []corev1.Taint{{Key: "node.cilium.io/agent-not-ready", Effect: corev1.TaintEffectNoExecute}},
			want: []string{"node.cilium.io/agent-not-ready"},
		},
		{
			name: "the gate is off",
			spec: []corev1.Taint{},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
			group := createTestGroup()
			group.Spec.NodeReadinessTaints = tc.spec

			reconciler := newTaintReconciler(t, node, group, createTestKubernetesNode())

			keys := reconciler.readinessTaintKeys(context.Background(), node, group)
			assert.Equal(t, tc.want, keys, "the FSM gets exactly the list the Group declares")

			stamped := &infrav2alpha1.Node{}
			require.NoError(t, reconciler.Get(context.Background(),
				types.NamespacedName{Name: "test-node", Namespace: "default"}, stamped))

			condition := meta.FindStatusCondition(stamped.Status.Conditions, autoscalerpkg.ConditionReadinessTaints)
			require.NotNil(t, condition, "the gate is legible from the slot")

			if len(tc.want) == 0 {
				assert.Equal(t, "none", condition.Message, "an off gate reads differently from no record at all")

				return
			}

			assert.Equal(t, tc.want[0], condition.Message)
			assert.Equal(t, keys, autoscalerpkg.ReadinessTaintKeysOfSlot(stamped), "the record reads back as the gate's keys")

			before := stamped.DeepCopy()

			// The steady state: the same Group, the same record, no write.
			assert.Equal(t, tc.want, reconciler.readinessTaintKeys(context.Background(), stamped, group))

			after := &infrav2alpha1.Node{}
			require.NoError(t, reconciler.Get(context.Background(),
				types.NamespacedName{Name: "test-node", Namespace: "default"}, after))
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "a resync of an unchanged Group writes nothing")
		})
	}

	t.Run("no group to read falls back to the slot's record", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
		autoscalerpkg.StampReadinessTaints(node, []string{"node.cilium.io/agent-not-ready"})

		reconciler := newTaintReconciler(t, node, createTestGroup())

		assert.Equal(t, []string{"node.cilium.io/agent-not-ready"}, reconciler.readinessTaintKeys(context.Background(), node, nil),
			"a pass without its Group keeps the gate the Group configured, it does not quietly become Ready-only")
	})
}

// TestNodeReconcileIdleSlotIsIdleAndDoesNothing is the shape a homelab pool rests in: a slot with no
// live claim and a machine the provider observes off. Nothing is wanted, nothing is in flight, so
// the pass fires no lifecycle transition, buys no Job, and comes back on the idle resync - the
// resync being what notices a machine that was powered on out of band.
func TestNodeReconcileIdleSlotIsIdleAndDoesNothing(t *testing.T) {
	node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode())

	result, err := reconcileTestSlot(t, reconciler)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: requeueFiveMinutes}, result)

	assert.Empty(t, listLifecycleJobs(t, reconciler.Client), "a free slot buys nothing")
	assert.Equal(t, infrav2alpha1.NodePhaseOff, readSlot(t, reconciler).Status.Phase,
		"the report of a slot with no claim and no machine is off")
}

// TestLiveClaimOfIgnoresTerminalClaims is the arithmetic of "the machine is wanted" at the reconcile:
// pending, booting and ready are live, failed and completed are not. A terminal claim whose machine
// is observed up is the force-shutdown case of the input law, so reading it as live would be the
// model keeping a machine the pool has given up on.
func TestLiveClaimOfIgnoresTerminalClaims(t *testing.T) {
	node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)

	tests := []struct {
		claimPhase infrav2alpha1.NodeClaimPhase
		wantLive   bool
	}{
		{claimPhase: infrav2alpha1.NodeClaimPending, wantLive: true},
		{claimPhase: infrav2alpha1.NodeClaimBooting, wantLive: true},
		{claimPhase: infrav2alpha1.NodeClaimReady, wantLive: true},
		{claimPhase: infrav2alpha1.NodeClaimFailed, wantLive: false},
		{claimPhase: infrav2alpha1.NodeClaimCompleted, wantLive: false},
	}

	for _, tc := range tests {
		t.Run(string(tc.claimPhase), func(t *testing.T) {
			nodeClaim := claimOnSlot(node, tc.claimPhase, nil)
			reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim)

			live, err := reconciler.liveClaimOf(context.Background(), node)
			require.NoError(t, err)

			if tc.wantLive {
				require.NotNil(t, live, "a %s claim names a machine that is wanted", tc.claimPhase)
				assert.Equal(t, nodeClaim.Name, live.Name)

				return
			}

			assert.Nil(t, live, "a %s claim is terminal: it is not capacity, and it stops being capacity the moment it is recorded", tc.claimPhase)
		})
	}
}

// TestLifecycleInputIsTheTriple is the input of one pass, assembled: the live claim, the observed
// power, the usability of the node object, and the lifecycle Jobs of the slot. The assertion that
// matters for the model is the last one: two slots whose records disagree, with the same claim, the
// same observation and the same node, produce the same input, so the record cannot reach a decision.
func TestLifecycleInputIsTheTriple(t *testing.T) {
	node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimBooting, nil)
	reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim, startupJobInFlight(node))

	input := reconciler.lifecycleInput(context.Background(), node, nodeClaim)
	require.NotNil(t, input.Claim)
	assert.Equal(t, infrav2alpha1.NodeClaimBooting, input.Claim.Status.Phase)
	assert.Equal(t, infrav2alpha1.PowerStateOff, input.ObservedPower)
	assert.True(t, input.StartupJobInFlight, "the startup Job of the slot is a fact about an object that exists")
	assert.False(t, input.ShutdownJobInFlight)
	assert.True(t, input.NodeUsable, "the shared test node is Ready, uncordoned and carries no readiness taint")

	// The record, contradicted five ways: the input does not change, because the input has no field
	// that a record can fill.
	for _, report := range []infrav2alpha1.NodePhase{
		infrav2alpha1.NodePhaseReady,
		infrav2alpha1.NodePhaseDraining,
		infrav2alpha1.NodePhasePending,
		infrav2alpha1.NodePhaseBooting,
		infrav2alpha1.NodePhaseOff,
	} {
		contradicted := node.DeepCopy()
		contradicted.Status.Phase = report

		assert.Equal(t, input, reconciler.lifecycleInput(context.Background(), contradicted, nodeClaim),
			"the report of a slot is never an input to the pass that writes it")
	}
}

// TestLifecycleInputAndObservationInputDeriveOnePhase is half 2 of a pair (the transition path's
// half): the triple "a claim pending, the machine observed off, a startup Job in flight", assembled
// by this path, derives `booting`. Its pair,
// TestObservationInputAndLifecycleInputDeriveOnePhase in
// internal/controller/core/node_controller_test.go, assembles the same world through the observation
// path and asserts the same value.
//
// The pair is the point: these are the two writers of the derived report, a phase has one formula
// (phase.Derive), and a formula with one value has one way to assemble its input. An observation that
// left the lifecycle-Job fields empty derived `pending` for a machine whose boot had already started,
// the transition path derived `booting` for the same machine, and since the record of the report
// became a writer's job that disagreement is not a display hiccup - the two paths append two rows to
// one slot's ring and count one boot twice, once per pass, all day. Both paths fill the fields from
// fsm.JobsInFlightOfSlot now, and the two tests are the pin that they do.
func TestLifecycleInputAndObservationInputDeriveOnePhase(t *testing.T) {
	node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhasePending)
	nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimPending, nil)
	reconciler := newFSMReconciler(t, node, createTestGroup(), nodeClaim, startupJobInFlight(node))

	input := reconciler.lifecycleInput(context.Background(), node, nodeClaim)

	require.NotNil(t, input.Claim, "the claim is the first input of the report")
	assert.True(t, input.StartupJobInFlight, "the startup Job is read here, and not left for the other path to notice")
	assert.False(t, input.ShutdownJobInFlight)

	assert.Equal(t, infrav2alpha1.NodePhaseBooting, phase.Derive(input),
		"a pending claim, a machine observed off and a startup Job in flight is `booting` - the value of the same world, on both writers' paths")

	// The shape the field is the difference of: with no Job in flight the same triple is `pending`,
	// the cancelable shape, which is what the provider's DecreaseTargetSize loop is allowed to take
	// away - and takes away with its own attribution recorded on the claim (see the gRPC test).
	noJob := newFSMReconciler(t, node, createTestGroup(), nodeClaim)

	assert.Equal(t, infrav2alpha1.NodePhasePending, phase.Derive(noJob.lifecycleInput(context.Background(), node, nodeClaim)))
}

// TestNodeReconcileClaimMarkedForScaleDownIsDrained is DeleteNodes seen from the slot: the provider
// recorded the request on the claim and deleted it, so the claim carries a deletion timestamp and
// the controller's finalizer. The machine is wanted no longer, which is law 3 with an owner: the
// shutdown runs, the node is cordoned, and the claim stays live and marked until the claim controller
// sees the machine off (the boot is reversed, never interrupted).
func TestNodeReconcileClaimMarkedForScaleDownIsDrained(t *testing.T) {
	node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimReady, func(c *infrav2alpha1.NodeClaim) {
		c.Spec.Request.Call = infrav2alpha1.ClaimCallDeleteNodes
		c.Spec.Request.Nodes = []string{"test-node-k8s"}
	})
	reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim)
	markDeleted(t, reconciler.Client, nodeClaim)

	_, err := reconcileTestSlot(t, reconciler)
	require.NoError(t, err)

	jobs := listLifecycleJobs(t, reconciler.Client)
	require.Len(t, jobs, 1, "a marked claim is the scale-down request that owns this machine")
	assert.Contains(t, jobs[0].Name, "test-node-shutdown-")

	assert.True(t, getK8sNode(t, reconciler.Client).Spec.Unschedulable, "the machine is cordoned before it is powered off")
	assert.Equal(t, infrav2alpha1.NodePhaseDraining, readSlot(t, reconciler).Status.Phase)

	live, err := reconciler.liveClaimOf(context.Background(), node)
	require.NoError(t, err)
	require.NotNil(t, live, "a live claim marked for deletion is still the claim of the slot: it ends when the machine is off")
	assert.Equal(t, infrav2alpha1.NodeClaimReady, live.Status.Phase)
}
