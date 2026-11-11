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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
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
func taintK8sNode(taints ...corev1.Taint) *corev1.Node {
	k8sNode := createTestKubernetesNode()
	k8sNode.Spec.Taints = taints

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
			name:     "a standard node lifecycle taint is not written",
			declared: []corev1.Taint{{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoSchedule}},
			existing: nil,
			want:     nil,
			patched:  false,
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
