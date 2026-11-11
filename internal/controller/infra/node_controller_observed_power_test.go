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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/progress"
)

// observedPowerNode is the live slot: the claim is off (cluster-autoscaler dropped it), the machine
// is observed up (status.powerState on, its corev1.Node Ready), and status.progress records the
// shutdown that did not take it off. The FSM derives StateShutdown from that progress, so
// ShutdownNode - the operation the claim wants - is not reachable from it, and the pass logs
// "Node is in desired state or transitioning".
func observedPowerNode(desired infrav1alpha1.PowerState, recorded infrav1alpha1.Progress) *infrav1alpha1.Node {
	node := createTestNode(desired, recorded)
	node.Status.PowerState = infrav1alpha1.PowerStateOn

	return node
}

// newObservedPowerReconciler wires the node controller with an event recorder, so a test can count
// what the precedence rule published.
func newObservedPowerReconciler(t *testing.T, recorder events.EventRecorder, objects ...client.Object) *NodeReconciler {
	t.Helper()

	scheme := setupTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1alpha1.Node{}).
		Build()

	return &NodeReconciler{Client: fakeClient, Scheme: scheme, EventRecorder: recorder}
}

// readObservedPowerSlot reads the slot back, so the assertions are about the cluster.
func readObservedPowerSlot(t *testing.T, c client.Client) *infrav1alpha1.Node {
	t.Helper()

	node := &infrav1alpha1.Node{}
	require.NoError(t, c.Get(context.Background(), reconcileRequest().NamespacedName, node))

	return node
}

// reconcileRequest is the request for the shared test slot.
func reconcileRequest() reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-node", Namespace: "default"}}
}

// withReason returns the recorded events carrying a reason, in the "<Type> <Reason> <message>"
// rendering the fake recorder produces, which is what `kubectl describe` shows.
func withReason(recorded []string, reason string) []string {
	var found []string

	for _, event := range recorded {
		if strings.HasPrefix(event, "Warning "+reason+" ") || strings.HasPrefix(event, "Normal "+reason+" ") {
			found = append(found, event)
		}
	}

	return found
}

// completedShutdownJob is the lifecycle Job of the shutdown that just finished, stamped the way the
// FSM stamps it (fsm.lifecycleLabels) and completed inside the settle grace. It is the evidence that
// an operation owns the slot: its state machine is the writer of the progress this rule would
// overwrite, and the node object of a machine that has just been powered off stays Ready for
// node-monitor-grace-period.
func completedShutdownJob() *batchv1.Job {
	completed := metav1.NewTime(time.Now().Add(-30 * time.Second))

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-node-shutdown-1700000000",
			Namespace: "default",
			Labels: map[string]string{
				fsm.LabelApp:        fsm.LifecycleAppName,
				fsm.LabelGroup:      "test-group",
				fsm.LabelNodeLegacy: "test-node-k8s",
				fsm.LabelType:       fsm.PhaseShutdown,
				fsm.LabelNodeCR:     "test-node",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{{Name: "shutdown", Image: "test/shutdown:latest"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			StartTime:      &completed,
			CompletionTime: &completed,
			Succeeded:      1,
		},
	}
}

// TestNodeReconcileObservedOnFiresTheShutdownItOutranks is the fix for the 40-minute stuck slot, run
// through the reconciler: the observed on outranks the recorded progress, the correction lands in the
// caller's copy, and the FSM branch of the very same pass fires ShutdownNode. No grace is waited
// for - the claim is already off and the machine is burning money - and the correction is recorded
// once, naming the progress it replaced.
func TestNodeReconcileObservedOnFiresTheShutdownItOutranks(t *testing.T) {
	recorder := events.NewFakeRecorder(8)

	node := observedPowerNode(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	reconciler := newObservedPowerReconciler(t, recorder, node, createTestGroup(), createTestKubernetesNode())

	result, err := reconciler.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result, "the shutdown is running, so the pass has nothing to wait for")

	// The shutdown Job went out on this pass.
	jobs := &batchv1.JobList{}
	require.NoError(t, reconciler.List(context.Background(), jobs))
	require.Len(t, jobs.Items, 1, "one shutdown Job, in the same pass as the correction")
	assert.Contains(t, jobs.Items[0].Name, "test-node-shutdown-")
	assert.Equal(t, "shutdown", jobs.Items[0].Labels["type"])

	// The progress moved: the correction, then the FSM's own write of the transition it fired.
	after := readObservedPowerSlot(t, reconciler.Client)
	assert.Equal(t, infrav1alpha1.ProgressShuttingDown, after.Status.Progress)
	assert.Equal(t, infrav1alpha1.PowerStateOn, after.Status.PowerState, "the machine is still up: the observation stands")
	assert.Equal(t, infrav1alpha1.PowerStateOff, after.Spec.DesiredPowerState, "the claim is the request, nothing dropped it")

	recorded := drainEvents(recorder)

	corrections := withReason(recorded, progress.ReasonMachineObservedUp)
	require.Len(t, corrections, 1, "one correction, one event: %v", recorded)
	assert.Contains(t, corrections[0], "Warning", "an unwanted machine is the shape worth a Warning")
	assert.Contains(t, corrections[0], "\"shutdown\"", "the event names the progress the machine outlived: %s", corrections[0])
	assert.Contains(t, corrections[0], "test-node-k8s", "the event names the machine that was observed up")

	// The only other event of the pass is the shutdown the correction released: the rule records a
	// correction, it does not narrate the operation the FSM then ran.
	assert.Len(t, recorded, 2, "the correction, and the ScaleDownRequested of the shutdown it released: %v", recorded)
	assert.Len(t, withReason(recorded, "ScaleDownRequested"), 1)
}

// TestNodeReconcileObservedOnOfAWantedMachineCorrectsTheProgressOnly: the machine is up and is the
// one the claim asks for, so the correction is the whole fix - no lifecycle Job, no power
// operation - and the second pass writes nothing and records nothing.
func TestNodeReconcileObservedOnOfAWantedMachineCorrectsTheProgressOnly(t *testing.T) {
	recorder := events.NewFakeRecorder(8)

	node := observedPowerNode(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown)
	reconciler := newObservedPowerReconciler(t, recorder, node, createTestGroup(), createTestKubernetesNode())

	result, err := reconciler.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: requeueFiveMinutes}, result,
		"the slot is at the pair the observation says, so the idle resync is the answer")

	after := readObservedPowerSlot(t, reconciler.Client)
	assert.Equal(t, infrav1alpha1.ProgressReady, after.Status.Progress)
	assert.Equal(t, infrav1alpha1.PowerStateOn, after.Status.PowerState)
	assert.Equal(t, infrav1alpha1.PowerStateOn, after.Spec.DesiredPowerState)

	jobs := &batchv1.JobList{}
	require.NoError(t, reconciler.List(context.Background(), jobs))
	assert.Empty(t, jobs.Items, "a wanted machine that is up needs no operation")

	recorded := drainEvents(recorder)

	corrections := withReason(recorded, progress.ReasonMachineObservedUp)
	require.Len(t, corrections, 1, "one correction, one event: %v", recorded)
	assert.Contains(t, corrections[0], "Normal", "a machine that is up and wanted is a corrected record, not an incident")
	assert.Len(t, recorded, 1, "and nothing else: no lifecycle operation was asked for")

	// The second pass: nothing to do, nothing written, nothing recorded.
	written := readObservedPowerSlot(t, reconciler.Client)
	_, err = reconciler.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	again := readObservedPowerSlot(t, reconciler.Client)
	assert.Equal(t, written.ResourceVersion, again.ResourceVersion, "the correction is one write, and the next pass writes nothing")
	assert.Empty(t, drainEvents(recorder), "no event storm")
}

// TestNodeReconcileLeavesAShutdownInFlightToItsOperation: the completion of a shutdown Job records
// progress: shutdown while the machine's node object is still Ready (node-monitor-grace-period,
// ~40s), so a pass in that window must not "correct" the slot to ready and fire a second shutdown.
// The lifecycle Job's own gate is what defers the rule, and the deferral is scheduled.
func TestNodeReconcileLeavesAShutdownInFlightToItsOperation(t *testing.T) {
	recorder := events.NewFakeRecorder(8)

	node := observedPowerNode(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	reconciler := newObservedPowerReconciler(t, recorder, node, createTestGroup(), createTestKubernetesNode(),
		completedShutdownJob())

	_, err := reconciler.Reconcile(context.Background(), reconcileRequest())
	require.NoError(t, err)

	after := readObservedPowerSlot(t, reconciler.Client)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress, "the operation's progress is untouched")

	jobs := &batchv1.JobList{}
	require.NoError(t, reconciler.List(context.Background(), jobs))
	assert.Len(t, jobs.Items, 1, "no second shutdown Job in the window the gate exists for")

	assert.Empty(t, drainEvents(recorder), "the gate records nothing")
}
