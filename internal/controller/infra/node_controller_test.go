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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	autoscalerpkg "github.com/homecluster-dev/homelab-autoscaler/internal/autoscaler"
	"github.com/homecluster-dev/homelab-autoscaler/internal/capacity"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/progress"
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

// machinelessSlot is a slot of a pool that churns: the Hetzner server behind it is deleted, so no
// corev1.Node points at it and no lifecycle Job is behind it, and cluster-autoscaler has asked for
// it to be off. The status still says the machine is up and the progress names one.
func machinelessSlot(previous infrav1alpha1.Progress) *infrav1alpha1.Node {
	node := createTestNode(infrav1alpha1.PowerStateOff, previous)
	node.Status.PowerState = infrav1alpha1.PowerStateOn

	return node
}

// listLifecycleJobs returns every startup/shutdown Job the controller created.
func listLifecycleJobs(t *testing.T, c client.Client) []batchv1.Job {
	t.Helper()

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(context.Background(), jobs))

	return jobs.Items
}

// TestNodeReconcileSettlesAProgressThatOutlivedItsMachine is the live failure wired through
// Reconcile: a slot at ready, desired off, no Kubernetes node and no lifecycle Job. Pre-fix this
// pass fired ShutdownNode on a machine that does not exist - a shutdown Job for a deleted server,
// a cordon of a node object that is not there, and a slot that lands back at ready when the Job
// fails, so the pool pays for the loop. The watchdog settles the slot before any FSM branch looks
// at it, and the settlement is the one Warning that says so.
func TestNodeReconcileSettlesAProgressThatOutlivedItsMachine(t *testing.T) {
	for _, previous := range []infrav1alpha1.Progress{
		infrav1alpha1.ProgressReady,
		infrav1alpha1.ProgressStartingUp,
		infrav1alpha1.ProgressShuttingDown,
	} {
		t.Run(string(previous), func(t *testing.T) {
			recorder := events.NewFakeRecorder(16)

			node := machinelessSlot(previous)

			// The stamp of a transition that is over. A slot with no lifecycle Job object left -
			// the Job was collected by its TTL - has its grace measured from status.lastStartupTime
			// (progress.startupGrace), so a startup stamped now is a startup whose hook may still be
			// running and is requeued, not settled. A slot of a pool that churned is stamped long
			// past the grace, and that is what this pass acts on.
			stale := metav1.NewTime(time.Now().Add(-progress.SettleGracePeriod - time.Minute))

			if previous == infrav1alpha1.ProgressStartingUp {
				node.Status.LastStartupTime = &stale
			} else {
				stamp := metav1.Now()
				node.Status.LastShutdownTime = &stamp
			}

			reconciler := newTaintReconciler(t, node, createTestGroup())
			reconciler.EventRecorder = recorder

			_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"},
			})
			require.NoError(t, err)

			after := &infrav1alpha1.Node{}
			require.NoError(t, reconciler.Get(context.Background(),
				types.NamespacedName{Name: "test-node", Namespace: "default"}, after))

			assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress,
				"a progress naming a machine the cluster does not have is settled")
			assert.Equal(t, infrav1alpha1.PowerStateOff, after.Status.PowerState)
			assert.Empty(t, listLifecycleJobs(t, reconciler.Client),
				"no lifecycle Job is built for a machine that is gone, that is the churn this closes")

			var settled []string

			for _, event := range drainEvents(recorder) {
				if strings.Contains(event, progress.ReasonProgressReconciled) {
					settled = append(settled, event)
				}
			}

			require.Len(t, settled, 1, "one settlement, one Warning")
			assert.Contains(t, settled[0], "Warning")
			assert.Contains(t, settled[0], string(previous), "the event names the progress it replaced")
		})
	}
}

// TestNodeReconcileSettlementIsIdempotent runs the reconcile on a slot that is already settled:
// nothing is written, nothing is recorded. A slot of a healthy off pool is reconciled on every
// resync, and a settlement that fires every time is an event storm on a pool at rest.
func TestNodeReconcileSettlementIsIdempotent(t *testing.T) {
	recorder := events.NewFakeRecorder(16)

	node := machinelessSlot(infrav1alpha1.ProgressReady)
	reconciler := newTaintReconciler(t, node, createTestGroup())
	reconciler.EventRecorder = recorder

	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"}}

	require.NoError(t, firstReconcile(reconciler, request))
	drainEvents(recorder)

	for range 3 {
		require.NoError(t, firstReconcile(reconciler, request))
	}

	after := &infrav1alpha1.Node{}
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "test-node", Namespace: "default"}, after))

	assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress)
	assert.Zero(t, len(drainEvents(recorder)), "a settled slot records nothing on the passes after the settlement")
}

// firstReconcile is the Reconcile call of the tests above, with the error surfaced.
func firstReconcile(reconciler *NodeReconciler, request reconcile.Request) error {
	_, err := reconciler.Reconcile(context.Background(), request)

	return err
}

// TestNodeReconcileLeavesASlotWithALiveJobAlone is the FSM's ownership, seen from the reconcile:
// a slot whose lifecycle Job is pending is an operation in flight, and the watchdog must not
// write over it. The FSM's own transitional progress is what the pass leaves behind.
func TestNodeReconcileLeavesASlotWithALiveJobAlone(t *testing.T) {
	recorder := events.NewFakeRecorder(16)

	node := machinelessSlot(infrav1alpha1.ProgressShuttingDown)
	stamp := metav1.Now()
	node.Status.LastShutdownTime = &stamp

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-node-shutdown-1700000000",
			Namespace: "default",
			Labels: map[string]string{
				fsm.LabelApp:        fsm.LifecycleAppName,
				fsm.LabelType:       fsm.PhaseShutdown,
				fsm.LabelNodeCR:     "test-node",
				fsm.LabelNodeLegacy: "test-node-k8s",
			},
		},
	}

	reconciler := newTaintReconciler(t, node, createTestGroup(), job)
	reconciler.EventRecorder = recorder

	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"},
	})
	require.NoError(t, err)

	after := &infrav1alpha1.Node{}
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "test-node", Namespace: "default"}, after))

	assert.Equal(t, infrav1alpha1.ProgressShuttingDown, after.Status.Progress, "the operation owns the progress")
	assert.Zero(t, len(drainEvents(recorder)), "a pass that settles nothing records nothing")
	assert.LessOrEqual(t, result.RequeueAfter, progress.SettleGracePeriod, "the slot is looked at again, not forgotten")
}

// TestNodeReconcileHonoursTheWatchdogDeadline: a transitional slot is on the FSM's progressive
// backoff (2 minutes at 5 minutes of transition age). When the watchdog has a tighter deadline -
// a lifecycle Job that finished 90s ago, so it leaves the grace in 30s - the pass comes back on
// the earlier one, and the settlement is scheduled rather than left to the next resync.
func TestNodeReconcileHonoursTheWatchdogDeadline(t *testing.T) {
	node := machinelessSlot(infrav1alpha1.ProgressShuttingDown)
	stamp := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	node.Status.LastShutdownTime = &stamp

	// The Job finished 30s before the settle grace runs out, so the watchdog asks for 30s. Derived from
	// progress.SettleGracePeriod: the row says "30s before the grace", not a number that only made
	// sense next to the value the constant used to carry.
	completed := metav1.NewTime(time.Now().Add(-(progress.SettleGracePeriod - 30*time.Second)))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-node-shutdown-1700000000",
			Namespace: "default",
			Labels: map[string]string{
				fsm.LabelApp:        fsm.LifecycleAppName,
				fsm.LabelType:       fsm.PhaseShutdown,
				fsm.LabelNodeCR:     "test-node",
				fsm.LabelNodeLegacy: "test-node-k8s",
			},
		},
		Status: batchv1.JobStatus{
			Failed:         1,
			CompletionTime: &completed,
		},
	}

	reconciler := newTaintReconciler(t, node, createTestGroup(), job)

	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"},
	})
	require.NoError(t, err)

	assert.InDelta(t, (30 * time.Second).Seconds(), result.RequeueAfter.Seconds(), 2,
		"the grace expires in 30s, so the pass comes back then, not on the 2m transition backoff")
	assert.Less(t, result.RequeueAfter, 2*time.Minute, "earlier than the FSM's backoff for this transition")

	after := &infrav1alpha1.Node{}
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "test-node", Namespace: "default"}, after))
	assert.Equal(t, infrav1alpha1.ProgressShuttingDown, after.Status.Progress, "the Job is still inside its grace")
}

// TestShortenRequeue is the caller-side rule of the watchdog, tested directly: the pass honours a
// deadline the watchdog asked for, and it never becomes later, never empty, and never a hot loop.
//
// The three properties that matter:
//   - a deadline the FSM asked for (a lock to come back on, a transition backoff) is earlier, so it
//     wins - the watchdog's grace is a look, not a mandate;
//   - a result that asks to requeue *now* (Requeue: true, no RequeueAfter) is never turned into a
//     delayed one: that is how a settlement makes a controller slow;
//   - a non-positive watchdog deadline changes nothing, so the common path (no deadline) cannot
//     produce a zero RequeueAfter with Requeue: true, which is an immediate re-dispatch.
func TestShortenRequeue(t *testing.T) {
	const (
		watchdog = progress.SettleGracePeriod
		fsmSoon  = 30 * time.Second
	)

	tests := []struct {
		name         string
		result       ctrl.Result
		requeueAfter time.Duration
		want         ctrl.Result
	}{
		{
			name:         "no watchdog deadline: the result passes through untouched",
			result:       ctrl.Result{RequeueAfter: 5 * time.Minute},
			requeueAfter: 0,
			want:         ctrl.Result{RequeueAfter: 5 * time.Minute},
		},
		{
			name:         "a negative deadline is not a deadline",
			result:       ctrl.Result{RequeueAfter: 5 * time.Minute},
			requeueAfter: -time.Second,
			want:         ctrl.Result{RequeueAfter: 5 * time.Minute},
		},
		{
			name:         "the result requeues now: the watchdog does not delay it",
			result:       ctrl.Result{Requeue: true},
			requeueAfter: watchdog,
			want:         ctrl.Result{Requeue: true},
		},
		{
			name:         "an empty result takes the watchdog's deadline",
			result:       ctrl.Result{},
			requeueAfter: watchdog,
			want:         ctrl.Result{RequeueAfter: watchdog},
		},
		{
			name:         "a tighter FSM deadline wins",
			result:       ctrl.Result{RequeueAfter: fsmSoon},
			requeueAfter: watchdog,
			want:         ctrl.Result{RequeueAfter: fsmSoon},
		},
		{
			name:         "an equal deadline is left as the caller wrote it",
			result:       ctrl.Result{RequeueAfter: watchdog},
			requeueAfter: watchdog,
			want:         ctrl.Result{RequeueAfter: watchdog},
		},
		{
			name:         "a looser FSM deadline is shortened",
			result:       ctrl.Result{RequeueAfter: 15 * time.Minute},
			requeueAfter: watchdog,
			want:         ctrl.Result{RequeueAfter: watchdog},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shortenRequeue(tc.result, tc.requeueAfter)

			assert.Equal(t, tc.want, got)

			if tc.result.RequeueAfter > 0 {
				assert.LessOrEqual(t, got.RequeueAfter, tc.result.RequeueAfter,
					"the watchdog never extends a deadline the caller already asked for")
			}

			//nolint:staticcheck // SA1019: Requeue is deprecated, and the property under test is about
			// the pair of fields: shortening must never leave a result that asks to requeue now *and*
			// names a deadline, which controller-runtime refuses.
			assert.False(t, got.Requeue && got.RequeueAfter > 0,
				"a shortened result is not also an immediate requeue: %v", got)
		})
	}
}

// claimedMachinelessSlot is the M3 shape seen from the reconcile: cluster-autoscaler asked for this
// slot (spec.desiredPowerState: on), its startup Job completed, and its node never registered, so it
// sits at {desired: on, powerState: off, progress: ready}. The FSM cannot StartNode from ready, the
// provider counts it as capacity forever, and the autoscaler plans against a machine that does not
// exist.
func claimedMachinelessSlot(previous infrav1alpha1.Progress) *infrav1alpha1.Node {
	node := createTestNode(infrav1alpha1.PowerStateOn, previous)
	node.Status.PowerState = infrav1alpha1.PowerStateOff

	return node
}

// TestNodeReconcileRearmsAClaimAndRetriesTheMachine is the phantom-claim hole closed at the reconcile
// level: the watchdog re-arms the slot (progress: shutdown, powerState: off, desired: on) and the very
// same pass then runs the FSM, which can StartNode from shutdown, so the pool retries the machine.
// The claim is not dropped, and the retry is bounded: the startup Job of the attempt is the operation
// that owns the slot on the next pass.
func TestNodeReconcileRearmsAClaimAndRetriesTheMachine(t *testing.T) {
	recorder := events.NewFakeRecorder(16)

	node := claimedMachinelessSlot(infrav1alpha1.ProgressReady)
	reconciler := newTaintReconciler(t, node, createTestGroup())
	reconciler.EventRecorder = recorder

	_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"},
	})
	require.NoError(t, err)

	after := &infrav1alpha1.Node{}
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "test-node", Namespace: "default"}, after))

	assert.Equal(t, infrav1alpha1.PowerStateOn, after.Spec.DesiredPowerState, "the claim stands: it is the request")
	assert.Equal(t, infrav1alpha1.ProgressStartingUp, after.Status.Progress,
		"the pass re-armed the slot (shutdown) and the FSM then started it again (startingup)")
	assert.Equal(t, infrav1alpha1.PowerStateOff, after.Status.PowerState, "the machine is off, and the status says so")

	jobs := listLifecycleJobs(t, reconciler.Client)
	require.Len(t, jobs, 1, "the retry is a real startup: one Job for one attempt")
	assert.Equal(t, fsm.PhaseStartup, jobs[0].Labels[fsm.LabelType], "the pool retries the machine, it does not delete one")

	var rearmed []string

	for _, event := range drainEvents(recorder) {
		if strings.Contains(event, progress.ReasonClaimRearmed) {
			rearmed = append(rearmed, event)
		}
	}

	require.Len(t, rearmed, 1, "one re-arm, one event")
	assert.Contains(t, rearmed[0], "Normal", "a re-arm is the system healing itself")
	assert.Contains(t, rearmed[0], "startup attempt 1 of "+fmt.Sprint(progress.ClaimRearmLimit),
		"and the event names the attempt the arming leads to")
	assert.NotContains(t, strings.Join(rearmed, "\n"), progress.ReasonProgressReconciled,
		"a re-arm is not a settlement: the events distinguish the two, so an operator can tell a claim that is being retried from a machine that is gone")

	condition := meta.FindStatusCondition(after.Status.Conditions, progress.ConditionClaimRearmed)
	require.NotNil(t, condition, "the attempt count lives on the slot, so the bound survives a manager restart")
	assert.Equal(t, progress.ReasonClaimRearmAttempt+"1", condition.Reason,
		"the attempt is counted by the startup the pass started, not by the re-arm that armed it")
}

// TestNodeReconcileRearmIsBoundedAtTheReconcileLevel walks the whole shared mechanism through
// Reconcile: each pass re-arms a claimed slot whose machine never registered, the FSM then starts
// one machine for it, and the attempt is counted by the transition the pass ran. At
// progress.ClaimRearmLimit the re-arming stops, the slot settles to the stable pair, one Warning goes
// out, and the pass builds nothing. A claim whose machine cannot boot must not buy a machine per
// resync, forever.
//
// The bound is one counter for both failure loops, and the test asserts whose counter it is: the
// number on the slot is written by the startup (fsm.enterStartingUp), not by the watchdog's re-arm,
// so the loud loop - a startup Job that exits non-zero, which this package never sees - pays into the
// same account. A counter the watchdog kept for its own passes left that loop unbounded.
func TestNodeReconcileRearmIsBoundedAtTheReconcileLevel(t *testing.T) {
	node := claimedMachinelessSlot(infrav1alpha1.ProgressReady)
	reconciler := newTaintReconciler(t, node, createTestGroup())

	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"}}
	recorder := events.NewFakeRecorder(64)
	reconciler.EventRecorder = recorder

	stale := metav1.NewTime(time.Now().Add(-progress.SettleGracePeriod - time.Minute))

	for attempt := 1; attempt <= progress.ClaimRearmLimit; attempt++ {
		require.NoErrorf(t, firstReconcile(reconciler, request), "attempt %d of %d is inside the bound",
			attempt, progress.ClaimRearmLimit)

		current := readReconciledSlot(t, reconciler)
		assert.Equal(t, infrav1alpha1.ProgressStartingUp, current.Status.Progress,
			"the pass re-armed the slot and the FSM started the machine again")
		assert.Equal(t, attempt, fsm.StartupAttemptsOf(current),
			"the startup counts the attempt, not the re-arm that armed it")
		assert.Len(t, listLifecycleJobs(t, reconciler.Client), 1, "one machine per attempt")

		// The startup of the attempt finished and its machine never registered: the Job is gone (the
		// TTL collects it), the operation's lock is released, and the slot is left at a progress that
		// names a machine. All three are what the watchdog's gates read.
		for i := range listLifecycleJobs(t, reconciler.Client) {
			require.NoError(t, reconciler.Delete(context.Background(), &listLifecycleJobs(t, reconciler.Client)[i]))
		}

		releaseLock(t, reconciler, current)
		current.Status.Progress = infrav1alpha1.ProgressReady
		current.Status.PowerState = infrav1alpha1.PowerStateOff

		// The attempt is over: its Job was collected by the TTL, and the startup stamp the FSM left
		// on the slot is what the watchdog measures its grace from when no Job object is left
		// (progress.startupGrace). An attempt that finished a while ago is stamped past the grace,
		// which is the shape between two attempts of a pool that churns; the test says so in
		// milliseconds, so the stamp is moved by hand.
		current.Status.LastStartupTime = &stale

		require.NoError(t, reconciler.Status().Update(context.Background(), current))
	}

	// One more pass: the bound is used up. The slot settles to the stable pair with its claim kept, one
	// Warning goes out, the FSM refuses the start the arming would have led to, and no server is built.
	require.NoError(t, firstReconcile(reconciler, request))

	current := readReconciledSlot(t, reconciler)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, current.Status.Progress,
		"no machine, so no ready: the phantom is closed, not reported")
	assert.Equal(t, infrav1alpha1.PowerStateOff, current.Status.PowerState)
	assert.Equal(t, infrav1alpha1.PowerStateOn, current.Spec.DesiredPowerState, "the claim is the provider's to drop")

	condition := meta.FindStatusCondition(current.Status.Conditions, progress.ConditionClaimRearmed)
	require.NotNil(t, condition)
	assert.Equal(t, progress.ReasonClaimRearmLimitReached, condition.Reason)

	assert.Empty(t, listLifecycleJobs(t, reconciler.Client),
		"at the bound the controller creates no further server for the slot")

	var warnings []string

	for _, event := range drainEvents(recorder) {
		if strings.Contains(event, progress.ReasonClaimRearmLimitReached) {
			warnings = append(warnings, event)
		}
	}

	require.Len(t, warnings, 1, "the bound is reported once, not on every resync")
	assert.Contains(t, warnings[0], "Warning")
	assert.Contains(t, warnings[0], fmt.Sprint(progress.ClaimRearmLimit), "the Warning names the attempts it spent")

	require.NoError(t, firstReconcile(reconciler, request))
	assert.Zero(t, len(drainEvents(recorder)), "and the passes after it are silent")
	assert.Equal(t, current.ResourceVersion, readReconciledSlot(t, reconciler).ResourceVersion,
		"a settled slot that will not boot writes nothing, so a broken pool costs no API traffic")
}

// TestNodeReconcileRefusesAStartOnASlotThatUsedItsAttempts is the second entry of the bound at the
// reconcile level: the loud loop. A startup Job that exits non-zero leaves the slot at the stable
// {desired: on, powerState: off, progress: shutdown} pair (afterJobFailed writes the progress and
// nothing else), which is exactly the state the controller's StartNode branch fires from, and the
// progress watchdog never looks at it: a progress of shutdown implies no machine. The bound is
// therefore enforced by the FSM's StartNode, and the pass that finds the attempts spent must build no
// Job, leave the slot at the stable pair, and record one Warning.
//
// It is also the steady-state case: the refusal is not a retry, so a slot that is permanently broken
// writes and records nothing after the first pass, and the reconcile does not fail (the operator's
// signal is the event, not controller-runtime's rate limiter).
func TestNodeReconcileRefusesAStartOnASlotThatUsedItsAttempts(t *testing.T) {
	node := claimedMachinelessSlot(infrav1alpha1.ProgressShutdown)
	stamp := metav1.Now()
	node.Status.LastStartupTime = &stamp

	reconciler := newTaintReconciler(t, node, createTestGroup())

	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"}}
	recorder := events.NewFakeRecorder(32)
	reconciler.EventRecorder = recorder

	current := readReconciledSlot(t, reconciler)
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:   progress.ConditionClaimRearmed,
		Status: metav1.ConditionTrue,
		Reason: progress.ReasonClaimRearmAttempt + fmt.Sprint(progress.ClaimRearmLimit),
		Message: fmt.Sprintf("startup attempt %d recorded by the FSM: every startup Job of this slot exited non-zero",
			progress.ClaimRearmLimit),
	})
	require.NoError(t, reconciler.Status().Update(context.Background(), current))

	require.NoError(t, firstReconcile(reconciler, request),
		"a refusal is not a reconcile failure: the slot is stable and the record is the event")

	after := readReconciledSlot(t, reconciler)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress, "the slot stays at the stable pair")
	assert.Equal(t, infrav1alpha1.PowerStateOff, after.Status.PowerState)
	assert.Equal(t, infrav1alpha1.PowerStateOn, after.Spec.DesiredPowerState, "and keeps the claim")
	assert.Empty(t, listLifecycleJobs(t, reconciler.Client), "the fourth StartNode is refused, so no machine is bought")

	condition := meta.FindStatusCondition(after.Status.Conditions, progress.ConditionClaimRearmed)
	require.NotNil(t, condition)
	assert.Equal(t, progress.ReasonClaimRearmLimitReached, condition.Reason,
		"the refusal stamps the note, which is what the deduplication reads")

	recorded := drainEvents(recorder)
	require.Len(t, recorded, 1, "one Warning for the bound")
	assert.Contains(t, recorded[0], "Warning")
	assert.Contains(t, recorded[0], progress.ReasonClaimRearmLimitReached)

	before := readReconciledSlot(t, reconciler)

	require.NoError(t, firstReconcile(reconciler, request))
	assert.Zero(t, len(drainEvents(recorder)), "and every later pass is silent")
	assert.Equal(t, before.ResourceVersion, readReconciledSlot(t, reconciler).ResourceVersion,
		"with no status write at all")
	assert.Empty(t, listLifecycleJobs(t, reconciler.Client))
}

// readReconciledSlot reads the slot the reconciler's client holds.
func readReconciledSlot(t *testing.T, reconciler *NodeReconciler) *infrav1alpha1.Node {
	t.Helper()

	node := &infrav1alpha1.Node{}
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "test-node", Namespace: "default"}, node))

	return node
}

// releaseLock removes the coordination lock annotations of a slot, which is what the completion of an
// operation does: the lock is not the watchdog's to clear, and a test that leaves one on the slot
// tests the lock gate instead of the attempt bound.
func releaseLock(t *testing.T, reconciler *NodeReconciler, node *infrav1alpha1.Node) {
	t.Helper()

	for _, key := range []string{
		fsm.OperationLockAnnotation, fsm.LockOwnerAnnotation,
		fsm.LockTimestampAnnotation, fsm.LockTimeoutAnnotation,
	} {
		delete(node.Annotations, key)
	}

	if len(node.Annotations) == 0 {
		node.Annotations = nil
	}

	require.NoError(t, reconciler.Update(context.Background(), node))
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
			node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp)
			group := createTestGroup()
			group.Spec.NodeReadinessTaints = tc.spec

			reconciler := newTaintReconciler(t, node, group, createTestKubernetesNode())

			keys := reconciler.readinessTaintKeys(context.Background(), node, group)
			assert.Equal(t, tc.want, keys, "the FSM gets exactly the list the Group declares")

			stamped := &infrav1alpha1.Node{}
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

			after := &infrav1alpha1.Node{}
			require.NoError(t, reconciler.Get(context.Background(),
				types.NamespacedName{Name: "test-node", Namespace: "default"}, after))
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "a resync of an unchanged Group writes nothing")
		})
	}

	t.Run("no group to read falls back to the slot's record", func(t *testing.T) {
		node := createTestNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp)
		autoscalerpkg.StampReadinessTaints(node, []string{"node.cilium.io/agent-not-ready"})

		reconciler := newTaintReconciler(t, node, createTestGroup())

		assert.Equal(t, []string{"node.cilium.io/agent-not-ready"}, reconciler.readinessTaintKeys(context.Background(), node, nil),
			"a pass without its Group keeps the gate the Group configured, it does not quietly become Ready-only")
	})
}
