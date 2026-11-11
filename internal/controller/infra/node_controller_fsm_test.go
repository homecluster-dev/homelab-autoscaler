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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

func setupTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = infrav2alpha1.AddToScheme(scheme)

	return scheme
}

// createTestNode builds the shared test slot.
//
// The signature is the model: in v2alpha1 the spec of a slot carries no intent at all, so the only
// two power-ish fields a test can set are the observation (status.powerState) and the report
// (status.phase). What the machine is *wanted* to do is not a field of the slot: it is the
// NodeClaim the test puts next to it (see claimOnSlot).
func createTestNode(observed infrav2alpha1.PowerState, report infrav2alpha1.NodePhase) *infrav2alpha1.Node {
	return &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-node",
			Namespace: "default",
			Labels: map[string]string{
				"group": "test-group",
			},
		},
		Spec: infrav2alpha1.NodeSpec{
			KubernetesNodeName: "test-node-k8s",
			StartupPodSpec: infrav2alpha1.MinimalPodSpec{
				Image:   "test/startup:latest",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "echo 'starting node'"},
			},
			ShutdownPodSpec: infrav2alpha1.MinimalPodSpec{
				Image:   "test/shutdown:latest",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "echo 'shutting down node'"},
			},
		},
		Status: infrav2alpha1.NodeStatus{
			PowerState: observed,
			Phase:      report,
		},
	}
}

func createTestGroup() *infrav2alpha1.Group {
	return &infrav2alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-group",
			Namespace: "default",
		},
		Spec: infrav2alpha1.GroupSpec{
			ScaleDownUtilizationThreshold:    "0.5",
			ScaleDownGpuUtilizationThreshold: "0.5",
			ZeroOrMaxNodeScaling:             false,
			IgnoreDaemonSetsUtilization:      false,
		},
	}
}

func createTestKubernetesNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node-k8s",
		},
		Spec: corev1.NodeSpec{
			Unschedulable: false,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

// claimOnSlot is the live NodeClaim of a slot: the label the controllers list on, the phase the
// input law reads, and the attribution IncreaseSize stamps. mutate is the test's way to name the
// parts of the request it cares about (the deadline, the attempt, a deletion timestamp).
func claimOnSlot(slot *infrav2alpha1.Node, claimPhase infrav2alpha1.NodeClaimPhase,
	mutate func(*infrav2alpha1.NodeClaim),
) *infrav2alpha1.NodeClaim {
	nodeClaim := &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claim-" + slot.Name,
			Namespace: slot.Namespace,
			Labels: map[string]string{
				claimLabelSlot:  slot.Name,
				claimLabelGroup: "test-group",
			},
		},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef: corev1.ObjectReference{
				APIVersion: infrav2alpha1.GroupVersion.String(),
				Kind:       "Node",
				Name:       slot.Name,
			},
			RequestedBy: "cluster-autoscaler",
			Request: infrav2alpha1.ClaimRequest{
				Call:  infrav2alpha1.ClaimCallIncreaseSize,
				Delta: 1,
				Group: "test-group",
			},
		},
		Status: infrav2alpha1.NodeClaimStatus{
			Phase:   claimPhase,
			Attempt: 1,
		},
	}

	if mutate != nil {
		mutate(nodeClaim)
	}

	return nodeClaim
}

// The label keys of a claim, spelled out so a test that seeds the inventory does not import the
// claim package just to name them (it is imported where its arithmetic is the subject).
const (
	claimLabelSlot  = "infra.homecluster.dev/node"
	claimLabelGroup = "group"
)

// newFSMReconciler builds the slot reconciler over a fake client with the claim status subresource
// enabled: the transitions the reconciler runs move a claim through its Recorder, and a test that
// cannot read the phase back would be asserting nothing.
func newFSMReconciler(t *testing.T, objects ...client.Object) *NodeReconciler {
	t.Helper()

	scheme := setupTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&infrav2alpha1.Node{}, &infrav2alpha1.NodeClaim{}).
		Build()

	return &NodeReconciler{Client: fakeClient, Scheme: scheme}
}

// reconcileTestSlot runs one pass over the shared test slot.
func reconcileTestSlot(t *testing.T, reconciler *NodeReconciler) (ctrl.Result, error) {
	t.Helper()

	return reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"},
	})
}

// readClaim reads a claim back from the reconciler's client.
func readClaim(t *testing.T, reconciler *NodeReconciler, name string) *infrav2alpha1.NodeClaim {
	t.Helper()

	nodeClaim := &infrav2alpha1.NodeClaim{}
	require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, nodeClaim))

	return nodeClaim
}

// readSlot reads the slot back from the reconciler's client.
func readSlot(t *testing.T, reconciler *NodeReconciler) *infrav2alpha1.Node {
	t.Helper()

	slot := &infrav2alpha1.Node{}
	require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Name: "test-node", Namespace: "default"}, slot))

	return slot
}

// TestNodeReconciler_FSMIntegration is the input law driven through Reconcile:
//
//	(live claim phase, observed power, kubelet liveness) in, one lifecycle action out
//
// Every case names the shape of the slot and the claim it carries, and asserts what the pass did -
// which lifecycle Job it bought, which phase the claim landed in, what the slot reports. No case
// writes an intent onto the slot, because in v2alpha1 there is no field to write it to, and no case
// reads status.phase as an input, because that field is this pass's output.
func TestNodeReconciler_FSMIntegration(t *testing.T) {
	t.Run("a live pending claim with the machine off boots it", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimPending, nil)
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim)

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, result)

		jobs := listLifecycleJobs(t, reconciler.Client)
		require.Len(t, jobs, 1, "law 1: a live claim with the machine off buys the machine")

		job := jobs[0]
		assert.Contains(t, job.Name, "test-node-startup-")
		assert.Equal(t, "startup", job.Labels["type"])
		assert.Equal(t, "test/startup:latest", job.Spec.Template.Spec.Containers[0].Image)

		// The pending -> booting edge is the write that bought the machine, so the claim moved.
		assert.Equal(t, infrav2alpha1.NodeClaimBooting, readClaim(t, reconciler, nodeClaim.Name).Status.Phase)
		assert.Equal(t, infrav2alpha1.NodePhaseBooting, readSlot(t, reconciler).Status.Phase)
	})

	t.Run("no live claim with the machine observed on powers it off", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode())

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, result)

		jobs := listLifecycleJobs(t, reconciler.Client)
		require.Len(t, jobs, 1, "law 3: a machine nothing wants is shut down")
		assert.Contains(t, jobs[0].Name, "test-node-shutdown-")
		assert.Equal(t, "shutdown", jobs[0].Labels["type"])
		assert.Equal(t, "test/shutdown:latest", jobs[0].Spec.Template.Spec.Containers[0].Image)

		updated := getK8sNode(t, reconciler.Client)
		assert.True(t, updated.Spec.Unschedulable, "the shutdown cordons the machine it is powering off")

		slot := readSlot(t, reconciler)
		assert.Equal(t, infrav2alpha1.NodePhaseDraining, slot.Status.Phase)
		assert.Nil(t, slot.Status.LastClaim, "a shutdown with no claim behind it records no episode")
	})

	t.Run("a failed claim's machine is force-shut-down, because a terminal claim is not a want", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimFailed, func(c *infrav2alpha1.NodeClaim) {
			c.Status.Reason = infrav2alpha1.ClaimReasonDeadlineExceeded
		})
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim)

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, result)

		jobs := listLifecycleJobs(t, reconciler.Client)
		require.Len(t, jobs, 1, "law 4: an overdue boot lands on a machine the pool has given up on")
		assert.Contains(t, jobs[0].Name, "test-node-shutdown-")

		assert.Equal(t, infrav2alpha1.NodeClaimFailed, readClaim(t, reconciler, nodeClaim.Name).Status.Phase,
			"the claim stays failed: a terminal phase has no outgoing edge")
		assert.Equal(t, infrav2alpha1.NodePhaseDraining, readSlot(t, reconciler).Status.Phase)
	})

	t.Run("a ready claim on a usable node fires nothing", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimReady, nil)
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim)

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)
		assert.Equal(t, ctrl.Result{RequeueAfter: requeueFiveMinutes}, result, "law 2: capacity serving pods is left alone")

		assert.Empty(t, listLifecycleJobs(t, reconciler.Client))
		assert.Equal(t, infrav2alpha1.NodeClaimReady, readClaim(t, reconciler, nodeClaim.Name).Status.Phase)
	})

	t.Run("a startup Job already in flight owns the slot, so no second machine is bought", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimBooting, nil)
		job := startupJobInFlight(node)
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim, job)

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)
		assert.Greater(t, result.RequeueAfter, time.Duration(0), "a transitional slot is looked at again on the backoff")

		assert.Len(t, listLifecycleJobs(t, reconciler.Client), 1, "the Job in flight is the operation; nothing new starts")
		assert.Equal(t, infrav2alpha1.NodeClaimBooting, readClaim(t, reconciler, nodeClaim.Name).Status.Phase)
	})

	t.Run("a coordination lock held by the opposite operation defers the boot", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		node.Annotations = map[string]string{
			"homelab-autoscaler.dev/operation-lock": "scale-up",
			"homelab-autoscaler.dev/lock-owner":     "cluster-autoscaler",
			"homelab-autoscaler.dev/lock-timestamp": time.Now().Format(time.RFC3339),
			"homelab-autoscaler.dev/lock-timeout":   "300",
		}
		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimPending, nil)
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim)

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err, "a transition that lost the lock is a deferral, not a failure")
		assert.Greater(t, result.RequeueAfter, time.Duration(0), "the deferred operation is requeued at the lock's own deadline")
		assert.LessOrEqual(t, result.RequeueAfter, 5*time.Minute, "never later than the lock timeout")

		assert.Empty(t, listLifecycleJobs(t, reconciler.Client), "the refused operation bought nothing")

		// The refusal leaves nothing behind: not on the slot, and not on the claim. The intent
		// lives in the claim, so the requeue runs the same branch and the boot starts when the
		// lock is free - no second IncreaseSize, no operator intervention.
		slot := readSlot(t, reconciler)
		assert.Equal(t, infrav2alpha1.NodePhaseOff, slot.Status.Phase)
		assert.Equal(t, infrav2alpha1.NodeClaimPending, readClaim(t, reconciler, nodeClaim.Name).Status.Phase)
		assert.Equal(t, "scale-up", slot.Annotations[fsm.OperationLockAnnotation], "the lock of the operation in flight survived the refusal")
	})
}

// startupJobInFlight is a lifecycle Job of the slot that has not finished: the fact that separates
// "a boot is in flight" from "a claim exists and nothing has started", and a fact about an object,
// not about a string on the slot.
func startupJobInFlight(slot *infrav2alpha1.Node) *batchv1.Job {
	return lifecycleJob(slot, fsm.PhaseStartup)
}

// lifecycleJob builds a running startup/shutdown Job of a slot, with the labels the reconciler
// lists on (fsm.LabelNodeCR / fsm.LabelPhase).
func lifecycleJob(slot *infrav2alpha1.Node, jobPhase string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      slot.Name + "-" + jobPhase + "-inflight",
			Namespace: slot.Namespace,
			Labels: map[string]string{
				fsm.LabelNodeCR: slot.Name,
				fsm.LabelPhase:  jobPhase,
				"type":          jobPhase,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "lifecycle",
						Image:   "test/" + jobPhase + ":latest",
						Command: []string{"/bin/sh", "-c", "true"},
					}},
				},
			},
		},
	}
}

// TestNodeReconciler_ScaleDownAttemptBound is the bound on a scale-down that cannot land, read off
// the claim digest of the slot. A shutdown script that exits 0 while the machine stays up is the
// pool's bug, not a reason for the controller to fire the same shutdown every resync; and the count
// that decides it lives on the claim that asked for the scale-down, so it ends with that episode and
// needs no reset heuristic on the long-lived slot (docs/design/nodeclaim.md §4, the removal of the
// v1alpha1 ShutdownAttempts condition).
func TestNodeReconciler_ScaleDownAttemptBound(t *testing.T) {
	// scaleDownDigest is the record of an episode that left a machine up: the scale-down call, the
	// terminal phase, and the shutdown attempts it spent, with the clock the reset grace is measured
	// from. The boot attempts (Attempt) are deliberately not the bound: a shutdown that will not
	// land must not spend the record of the machines the request bought (M4).
	scaleDownDigest := func(shutdownAttempts int32, lastAttempt time.Duration) *infrav2alpha1.LastClaimSummary {
		stamp := metav1.NewTime(time.Now().Add(-lastAttempt))

		return &infrav2alpha1.LastClaimSummary{
			Name:                  "claim-test-node",
			Phase:                 infrav2alpha1.NodeClaimCompleted,
			Attempt:               1,
			ShutdownAttempts:      shutdownAttempts,
			LastShutdownAttemptAt: &stamp,
			Call:                  infrav2alpha1.ClaimCallDeleteNodes,
			Group:                 "test-group",
		}
	}

	t.Run("the shutdown attempts spent on this scale-down stop the retries", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
		node.Status.LastClaim = scaleDownDigest(claim.MaxConsecutiveAttempts, time.Minute)
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode())

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err, "the refusal is not an error: the machine is up, the pool does not want it, and the record is on the claim")
		assert.Equal(t, ctrl.Result{RequeueAfter: refusedResync}, result)

		assert.Empty(t, listLifecycleJobs(t, reconciler.Client), "no fourth shutdown Job for a script that keeps exiting 0")
	})

	t.Run("the bound spent on the boot attempts is not a shutdown bound", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
		digest := scaleDownDigest(0, time.Minute)
		digest.Attempt = 9
		node.Status.LastClaim = digest

		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode())

		_, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)

		assert.Len(t, listLifecycleJobs(t, reconciler.Client), 1,
			"the attempts a slot spent buying machines do not stop the shutdown of the machine it wants off")
	})

	t.Run("one attempt short of the bound fires", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
		node.Status.LastClaim = scaleDownDigest(claim.MaxConsecutiveAttempts-1, time.Minute)
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode())

		_, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)

		assert.Len(t, listLifecycleJobs(t, reconciler.Client), 1, "the bound is 3, so attempt 3 is fired")
	})

	t.Run("the grace of a spent streak passed, and the shutdown fires again", func(t *testing.T) {
		// M4: the refusal is a bounded pause, not a sentence. A pool whose shutdown script has been
		// fixed is picked up by a later pass, with no hand edit, and the bound keeps its teeth
		// against a script that fails every time (it is re-spent, and re-paused).
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
		node.Status.LastClaim = scaleDownDigest(claim.MaxConsecutiveAttempts, claim.ShutdownAttemptResetGrace+time.Minute)

		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode())

		_, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)

		assert.Len(t, listLifecycleJobs(t, reconciler.Client), 1,
			"past claim.ShutdownAttemptResetGrace the streak is spent no longer, so the shutdown of the fixed script runs")
	})

	t.Run("a live claim is never refused: its own attempts are counted on it", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
		node.Status.LastClaim = scaleDownDigest(claim.MaxConsecutiveAttempts, time.Minute)

		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimReady, func(c *infrav2alpha1.NodeClaim) {
			c.Spec.Request.Call = infrav2alpha1.ClaimCallDeleteNodes
			c.Spec.Request.Nodes = []string{"test-node-k8s"}
		})

		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim)
		markDeleted(t, reconciler.Client, nodeClaim)

		_, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)

		assert.Len(t, listLifecycleJobs(t, reconciler.Client), 1,
			"a scale-down that names a claim owns the machine, and its attempts are counted on that claim")
	})

	t.Run("a live claim that spent its own shutdown attempts is refused, for the grace at most", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)

		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimReady, func(c *infrav2alpha1.NodeClaim) {
			c.Spec.Request.Call = infrav2alpha1.ClaimCallDeleteNodes
			c.Spec.Request.Nodes = []string{"test-node-k8s"}
		})

		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim)
		markDeleted(t, reconciler.Client, nodeClaim)

		// The streak the claim controller counts (reconcileDeletion -> BumpScaleDownAttempt), landed
		// the way that writer lands it.
		spent := &infrav2alpha1.NodeClaim{}
		require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(nodeClaim), spent))
		spent.Status.ShutdownAttempts = claim.MaxConsecutiveAttempts
		stamp := metav1.NewTime(time.Now())
		spent.Status.LastShutdownAttemptAt = &stamp
		require.NoError(t, reconciler.Client.Status().Update(context.Background(), spent))

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)
		assert.Equal(t, refusedResync, result.RequeueAfter,
			"the refusal is paced: no later than the end of the reset grace, and the resync below it")

		assert.Empty(t, listLifecycleJobs(t, reconciler.Client),
			"a claim whose machine will not go off is not re-fired every 15s of its finalizer's poll")
	})
}

// markDeleted is the shape a NodeGroupDeleteNodes / DecreaseTargetSize cancel leaves behind on a
// claim that carries the controller's finalizer: the stamp arrives with the boot, and the delete
// the provider issues sets metadata.deletionTimestamp and leaves the object for the claim
// controller to finish. Deleting without the finalizer would make the object disappear, and the
// test would be asserting a shape the model never reaches.
func markDeleted(t *testing.T, c client.Client, nodeClaim *infrav2alpha1.NodeClaim) {
	t.Helper()

	fresh := &infrav2alpha1.NodeClaim{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(nodeClaim), fresh))

	if !contains(fresh.Finalizers, claimFinalizerName) {
		fresh.Finalizers = append(fresh.Finalizers, claimFinalizerName)
		require.NoError(t, c.Update(context.Background(), fresh))
	}

	require.NoError(t, c.Delete(context.Background(), fresh))
}

// claimFinalizerName is the finalizer the claim controller puts on a claim whose boot started; the
// constant lives in the controller package, which is this package, so the test spells nothing.
const claimFinalizerName = ClaimFinalizer

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}

	return false
}

// TestNodeReconciler_RecordedPhaseIsNotAnInput is the law of §6 at the reconcile level: the derived
// Node.status.phase is this pass's output, and writing a value that contradicts the claim and the
// observation changes nothing about what the pass does. The v1alpha1 reconciler gated every
// transition on the recorded progress (validateStateTransition), which is why a record that
// outlived its machine stranded a slot; the record has no such power here.
func TestNodeReconciler_RecordedPhaseIsNotAnInput(t *testing.T) {
	everyReport := []infrav2alpha1.NodePhase{
		infrav2alpha1.NodePhaseOff,
		infrav2alpha1.NodePhasePending,
		infrav2alpha1.NodePhaseBooting,
		infrav2alpha1.NodePhaseReady,
		infrav2alpha1.NodePhaseDraining,
	}

	t.Run("a pending claim boots the machine whatever the slot reports", func(t *testing.T) {
		for _, report := range everyReport {
			t.Run(string(report), func(t *testing.T) {
				node := createTestNode(infrav2alpha1.PowerStateOff, report)
				nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimPending, nil)
				reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim)

				_, err := reconcileTestSlot(t, reconciler)
				require.NoError(t, err)

				assert.Len(t, listLifecycleJobs(t, reconciler.Client), 1,
					"the claim decides, and it says a machine is wanted: startup runs whatever the record said")
				assert.Equal(t, infrav2alpha1.NodePhaseBooting, readSlot(t, reconciler).Status.Phase,
					"the report is rewritten from the inputs, not defended")
			})
		}
	})

	t.Run("an observed machine with no claim is powered off whatever the slot reports", func(t *testing.T) {
		for _, report := range everyReport {
			t.Run(string(report), func(t *testing.T) {
				node := createTestNode(infrav2alpha1.PowerStateOn, report)
				reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode())

				_, err := reconcileTestSlot(t, reconciler)
				require.NoError(t, err)

				jobs := listLifecycleJobs(t, reconciler.Client)
				require.Len(t, jobs, 1, "no live claim + observed on is law 3, whatever the record said")
				assert.Contains(t, jobs[0].Name, "test-node-shutdown-")
			})
		}
	})
}

// TestNodeReconciler_BackoffBehavior covers the pacing of a transitional slot: a boot and a
// shutdown in flight are operations, and the pass that finds one running owns the machine's next
// look. The transitional shape is a fact about objects - the claim's phase and the lifecycle Job -
// not about a string on the slot.
func TestNodeReconciler_BackoffBehavior(t *testing.T) {
	t.Run("a boot in flight is polled on the backoff, not on the idle resync", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		node.Status.LastStartupTime = ptr(metav1.Now())
		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimBooting, nil)
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim, startupJobInFlight(node))

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)
		assert.Greater(t, result.RequeueAfter, time.Duration(0))
		assert.Less(t, result.RequeueAfter, requeueFiveMinutes, "a transitional slot is not left to the idle resync")
		assert.Empty(t, listLifecycleJobs(t, reconciler.Client)[1:], "the pass adds no second startup Job")
	})

	t.Run("a shutdown in flight is polled on the backoff", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimReady, nil)
		job := lifecycleJob(node, fsm.PhaseShutdown)
		reconciler := newFSMReconciler(t, node, createTestGroup(), createTestKubernetesNode(), nodeClaim, job)
		markDeleted(t, reconciler.Client, nodeClaim)

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)
		assert.Greater(t, result.RequeueAfter, time.Duration(0), "the claim's finalizer waits for the machine to go off")
		assert.Len(t, listLifecycleJobs(t, reconciler.Client), 1, "the shutdown Job in flight owns the slot")
	})
}

// ptr is the address-of helper for the optional status fields a test stamps.
func ptr[T any](v T) *T { return &v }

// TestNodeReconciler_ErrorHandling covers the passes that do not reach the input law.
func TestNodeReconciler_ErrorHandling(t *testing.T) {
	t.Run("a slot with no group label is an error, not a silently skipped slot", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		node.Labels = nil
		reconciler := newFSMReconciler(t, node, createTestKubernetesNode())

		result, err := reconcileTestSlot(t, reconciler)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has no group label")
		assert.Equal(t, ctrl.Result{}, result)
	})

	t.Run("a slot whose Group is not readable is requeued, and buys nothing", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimPending, nil)
		reconciler := newFSMReconciler(t, node, createTestKubernetesNode(), nodeClaim)

		result, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)
		assert.Equal(t, ctrl.Result{RequeueAfter: 5 * time.Second}, result)
		assert.Empty(t, listLifecycleJobs(t, reconciler.Client), "a pass with no Group takes no lifecycle action")
	})

	t.Run("a machine that has not joined the cluster is still booted by a live claim", func(t *testing.T) {
		node := createTestNode(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimPending, nil)
		reconciler := newFSMReconciler(t, node, createTestGroup(), nodeClaim)

		_, err := reconcileTestSlot(t, reconciler)
		require.NoError(t, err)

		jobs := listLifecycleJobs(t, reconciler.Client)
		require.Len(t, jobs, 1, "the lifecycle Job is what boots the machine, so a missing corev1.Node is not a reason to skip it")
		assert.Contains(t, jobs[0].Name, "test-node-startup-")
	})
}

// TestNodeReconciler_InputLawStates is the state half of the input law, as a table: every shape of
// (live claim, observed power, usability, lifecycle Jobs) maps to exactly one state, and the
// slot's recorded report is not an input to that mapping. It is the phase state table of
// docs/architecture/state.md, asserted on the state the machine starts a pass in.
func TestNodeReconciler_InputLawStates(t *testing.T) {
	tests := []struct {
		name  string
		input phase.Input
		want  string
	}{
		{
			name:  "no claim, machine off: a free slot",
			input: phase.Input{ObservedPower: infrav2alpha1.PowerStateOff},
			want:  fsm.StateShutdown,
		},
		{
			name: "pending claim, machine off, no Job yet: boot pending, StartNode is the event",
			input: phase.Input{
				Claim:         claimOnSlot(&infrav2alpha1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}, infrav2alpha1.NodeClaimPending, nil),
				ObservedPower: infrav2alpha1.PowerStateOff,
			},
			want: fsm.StateShutdown,
		},
		{
			name: "booting claim: the startup is in flight, JobCompleted is reachable",
			input: phase.Input{
				Claim:              claimOnSlot(&infrav2alpha1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}, infrav2alpha1.NodeClaimBooting, nil),
				ObservedPower:      infrav2alpha1.PowerStateOff,
				StartupJobInFlight: true,
			},
			want: fsm.StateStartingUp,
		},
		{
			name: "ready claim, machine on, usable: capacity serving pods",
			input: phase.Input{
				Claim:         claimOnSlot(&infrav2alpha1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}, infrav2alpha1.NodeClaimReady, nil),
				ObservedPower: infrav2alpha1.PowerStateOn,
				NodeUsable:    true,
			},
			want: fsm.StateReady,
		},
		{
			name: "ready claim, machine on, not usable: still in the boot window",
			input: phase.Input{
				Claim:         claimOnSlot(&infrav2alpha1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}, infrav2alpha1.NodeClaimReady, nil),
				ObservedPower: infrav2alpha1.PowerStateOn,
				NodeUsable:    false,
			},
			want: fsm.StateStartingUp,
		},
		{
			name: "ready claim, machine off: the machine vanished under it, boot it again",
			input: phase.Input{
				Claim:         claimOnSlot(&infrav2alpha1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}, infrav2alpha1.NodeClaimReady, nil),
				ObservedPower: infrav2alpha1.PowerStateOff,
			},
			want: fsm.StateStartingUp,
		},
		{
			name: "failed claim, machine on: the machine is not wanted, and is observed up",
			input: phase.Input{
				Claim:         claimOnSlot(&infrav2alpha1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}, infrav2alpha1.NodeClaimFailed, nil),
				ObservedPower: infrav2alpha1.PowerStateOn,
			},
			want: fsm.StateReady,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := createTestNode(tc.input.ObservedPower, infrav2alpha1.NodePhaseReady)
			nodeClaim := claimOnSlot(node, infrav2alpha1.NodeClaimReady, nil)

			// The record of a ready slot is the deliberate contradiction: the input says
			// something else, and the state follows the input.
			stateMachine := fsm.NewNodeStateMachine(node, newFSMReconciler(t, node, nodeClaim).Client, setupTestScheme(),
				fsm.NewCoordinationManager(newFSMReconciler(t, node, nodeClaim).Client, fsm.NodeControllerOwner),
				fsm.WithInput(tc.input))

			assert.Equal(t, tc.want, stateMachine.GetCurrentState(), "the input law, not the record, picks the state")
			assert.Equal(t, phase.State(tc.input), stateMachine.GetCurrentState())
		})
	}
}
