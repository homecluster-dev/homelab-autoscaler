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

package progress

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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
)

const testNamespace = "homelab-autoscaler-system"

// watchdogScheme is every type the watchdog reads: the Node CR, the corev1.Node and the
// batchv1.Job.
func watchdogScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	return scheme
}

// newClient builds the fake client the watchdog runs against.
func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(watchdogScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1alpha1.Node{}).
		Build()
}

// slot builds a Node CR of a pool that is being churned: the desired power state and the
// progress under test, with a Kubernetes node name the slot can be matched against.
func slot(desired infrav1alpha1.PowerState, progress infrav1alpha1.Progress) *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-1",
			Namespace: testNamespace,
			Labels:    map[string]string{"group": "remote-workers"},
		},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  desired,
			KubernetesNodeName: "worker-1-k8s",
		},
		Status: infrav1alpha1.NodeStatus{
			// powerState: on, the state a slot whose machine was deleted out of band is left
			// in: the last thing recorded about it was that it was up.
			PowerState: infrav1alpha1.PowerStateOn,
			Progress:   progress,
		},
	}
}

// liveNode is the corev1.Node of a slot, Ready.
func liveNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1-k8s"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// lifecycleJob builds a lifecycle Job of the slot, labelled the way the FSM labels one, in the
// given phase, and finished when finishedAt is non-zero.
func lifecycleJob(phase string, finishedAt time.Time) *batchv1.Job {
	name := "worker-1-" + phase + "-1700000000"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				fsm.LabelApp:        fsm.LifecycleAppName,
				fsm.LabelGroup:      "remote-workers",
				fsm.LabelNodeLegacy: "worker-1-k8s",
				fsm.LabelType:       phase,
				fsm.LabelNodeCR:     "worker-1",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{{Name: phase, Image: "busybox"}},
				},
			},
		},
	}

	if finishedAt.IsZero() {
		// Pending / running: the monitor of the machine that started it is the writer.
		started := metav1.NewTime(time.Now().Add(-30 * time.Second))
		job.Status.StartTime = &started

		return job
	}

	completed := metav1.NewTime(finishedAt)
	job.Status.StartTime = &completed
	job.Status.CompletionTime = &completed

	if phase == fsm.PhaseShutdown {
		job.Status.Failed = 1
	} else {
		job.Status.Succeeded = 1
	}

	return job
}

// readSlot reads the slot back from the client, so the assertions are about what is in the
// cluster and not about a pointer the watchdog holds.
func readSlot(t *testing.T, c client.Client) *infrav1alpha1.Node {
	t.Helper()

	node := &infrav1alpha1.Node{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "worker-1", Namespace: testNamespace}, node))

	return node
}

// settle runs one pass of the watchdog against the fake client.
func settle(t *testing.T, c client.Client, node *infrav1alpha1.Node, recorder events.EventRecorder) Outcome {
	t.Helper()

	outcome, err := Settle(context.Background(), c, node, recorder, SettleGracePeriod)
	require.NoError(t, err)

	return outcome
}

// TestSettleLeavesTheMachinelessSlotNoMachineNoJob is the live failure: a slot at
// ready / startingup / shuttingdown, with the Hetzner server deleted, so no corev1.Node points
// at it and no lifecycle Job is behind it, and spec.desiredPowerState: off. Nothing in a
// reconcile heals that state today, and every FSM branch that looks at it either builds a Job
// for a machine that does not exist (from ready) or is gated out (from a transitional progress).
//
// The settlement writes the pair the observation controllers agree on, off/shutdown, and records
// exactly one Warning naming the progress it replaced.
func TestSettleSettlesMachinelessSlot(t *testing.T) {
	for _, progress := range []infrav1alpha1.Progress{
		infrav1alpha1.ProgressReady,
		infrav1alpha1.ProgressStartingUp,
		infrav1alpha1.ProgressShuttingDown,
	} {
		t.Run(string(progress), func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)
			node := slot(infrav1alpha1.PowerStateOff, progress)
			c := newClient(t, node)

			outcome := settle(t, c, node, recorder)

			assert.True(t, outcome.Settled, "progress %q implies a machine the cluster does not have", progress)
			assert.Equal(t, progress, outcome.Previous, "the outcome names the progress it replaced")
			assert.Zero(t, outcome.RequeueAfter, "nothing is in flight, so no look is scheduled")

			after := readSlot(t, c)
			assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress, "the slot is settled to shutdown")
			assert.Equal(t, infrav1alpha1.PowerStateOff, after.Status.PowerState, "and to the power state the slot is desired in")
			assert.Equal(t, infrav1alpha1.PowerStateOff, after.Spec.DesiredPowerState, "spec is never written")

			require.Equal(t, 1, len(recorder.Events), "one settlement records one event")

			event := <-recorder.Events
			assert.Contains(t, event, corev1.EventTypeWarning, "a slot pointing at a machine that is gone is not a Normal event")
			assert.Contains(t, event, ReasonProgressReconciled)
			assert.Contains(t, event, string(progress), "the event names the progress it replaced")
			assert.Contains(t, event, "worker-1")
			assert.Contains(t, event, "worker-1-k8s", "the event names the node that is not there")
		})
	}
}

// TestSettleNeverOverridesAClaim is the gate that keeps the watchdog out of the claim: a slot
// that is wanted on and reports ready with no machine is a startup that has to run, not a slot
// to settle. Settling it would tell the provider its requested capacity does not exist, which is
// the failure the target-size fix removed.
func TestSettleNeverOverridesAClaim(t *testing.T) {
	recorder := events.NewFakeRecorder(8)
	node := slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	c := newClient(t, node)

	before := readSlot(t, c)
	outcome := settle(t, c, node, recorder)

	assert.False(t, outcome.Settled)
	assert.Contains(t, outcome.Reason, "claim")

	after := readSlot(t, c)
	assert.Equal(t, before.Status, after.Status, "a claimed slot is untouched")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "and nothing is written")
	assert.Zero(t, len(recorder.Events))
}

// TestSettleLeavesASlotWithAJobAlone is the FSM's ownership: the lifecycle Job is the operation
// and the state machine that started it is the writer that settles its progress. The watchdog
// must not touch any slot that has one, in any of the shapes a Job in flight takes.
func TestSettleLeavesASlotWithAJobAlone(t *testing.T) {
	tests := []struct {
		name  string
		job   *batchv1.Job
		rejob bool
	}{
		{
			name:  "a pending startup job",
			job:   lifecycleJob(fsm.PhaseStartup, time.Time{}),
			rejob: true,
		},
		{
			name:  "a running shutdown job",
			job:   lifecycleJob(fsm.PhaseShutdown, time.Time{}),
			rejob: true,
		},
		{
			name: "a startup job that finished seconds ago",
			// (h) The window the grace exists for: the Job completed, the monitor of the
			// machine that started it has not fired JobCompleted yet.
			job: lifecycleJob(fsm.PhaseStartup, time.Now().Add(-15*time.Second)),
		},
		{
			name: "a shutdown job that failed seconds ago",
			job:  lifecycleJob(fsm.PhaseShutdown, time.Now().Add(-5*time.Second)),
		},
		{
			name: "a finished job with no completion timestamp, measured from its start",
			job: func() *batchv1.Job {
				job := lifecycleJob(fsm.PhaseStartup, time.Now().Add(-10*time.Second))
				job.Status.CompletionTime = nil

				return job
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)
			node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
			c := newClient(t, node, tc.job)

			before := readSlot(t, c)
			outcome := settle(t, c, node, recorder)

			after := readSlot(t, c)
			assert.False(t, outcome.Settled, "the operation owns the progress")
			assert.Equal(t, before.Status, after.Status)
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "a pass that settles nothing writes nothing")
			assert.Zero(t, len(recorder.Events))

			if tc.rejob {
				assert.Equal(t, SettleGracePeriod, outcome.RequeueAfter, "a Job in flight is looked at again on the grace")
			} else {
				assert.Greater(t, outcome.RequeueAfter, time.Duration(0), "the grace expiry is scheduled, not forgotten")
				assert.LessOrEqual(t, outcome.RequeueAfter, SettleGracePeriod)
			}
		})
	}
}

// TestSettleLeavesASlotWithAMachineAlone: a corev1.Node object - Ready or NotReady - is a
// machine, and the status of a slot with one is owned by the observation controllers. The
// watchdog settles slots with no machine, nothing else.
func TestSettleLeavesASlotWithAMachineAlone(t *testing.T) {
	for _, ready := range []bool{true, false} {
		t.Run(map[bool]string{true: "Ready", false: "NotReady"}[ready], func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)
			node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady)
			kubeNode := liveNode()

			if !ready {
				kubeNode.Status.Conditions[0].Status = corev1.ConditionFalse
			}

			c := newClient(t, node, kubeNode)

			before := readSlot(t, c)
			outcome := settle(t, c, node, recorder)

			after := readSlot(t, c)
			assert.False(t, outcome.Settled)
			assert.Contains(t, outcome.Reason, "observation controllers")
			assert.Equal(t, before.Status, after.Status)
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
			assert.Zero(t, len(recorder.Events))
		})
	}
}

// TestSettleIsIdempotent covers the steady state of a healthy pool: a slot that is already
// settled, and a slot that was settled by the pass before this one. A write on every pass bumps
// the slot, re-triggers the watches and keeps the controller hot, so nothing may be written and
// nothing recorded.
func TestSettleIsIdempotent(t *testing.T) {
	tests := []struct {
		name         string
		progress     infrav1alpha1.Progress
		wantProgress infrav1alpha1.Progress
	}{
		{"already shutdown", infrav1alpha1.ProgressShutdown, infrav1alpha1.ProgressShutdown},
		{"never seeded", "", ""},
		{"settled by the previous pass", infrav1alpha1.ProgressReady, infrav1alpha1.ProgressShutdown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)
			node := slot(infrav1alpha1.PowerStateOff, tc.progress)
			c := newClient(t, node)

			settle(t, c, node, recorder)

			for range 3 {
				outcome := settle(t, c, node, recorder)
				assert.False(t, outcome.Settled, "a settled slot is settled once")
				assert.Zero(t, outcome.RequeueAfter)
			}

			after := readSlot(t, c)
			assert.Equal(t, tc.wantProgress, after.Status.Progress)

			if tc.progress == infrav1alpha1.ProgressShutdown || tc.progress == "" {
				assert.Zero(t, len(recorder.Events), "nothing to settle, nothing recorded")
			}
		})
	}
}

// TestSettleLeavesASlotBehindALiveLockAlone: the coordination lock says an operation owns the
// slot right now. The window this closes is the one inside a transition - enterShuttingDown has
// written the progress, the Job has not been created yet - and the scale-down the provider is
// running under its own lock. An expired lock is not a lock: nothing is running under it, and a
// slot frozen behind one is what the watchdog exists for.
func TestSettleLeavesASlotBehindALiveLockAlone(t *testing.T) {
	node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
	node.Annotations = map[string]string{
		fsm.OperationLockAnnotation: fsm.OperationScaleDown,
		fsm.LockOwnerAnnotation:     fsm.NodeControllerOwner,
		fsm.LockTimestampAnnotation: time.Now().Format(time.RFC3339),
		fsm.LockTimeoutAnnotation:   "300",
	}

	c := newClient(t, node)
	before := readSlot(t, c)

	outcome := settle(t, c, node, events.NewFakeRecorder(8))

	after := readSlot(t, c)
	assert.False(t, outcome.Settled)
	assert.Contains(t, outcome.Reason, "coordination lock")
	assert.Equal(t, before.Status, after.Status)

	t.Run("an expired lock does not hold the slot", func(t *testing.T) {
		expired := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
		expired.Annotations = map[string]string{
			fsm.OperationLockAnnotation: fsm.OperationScaleDown,
			fsm.LockOwnerAnnotation:     fsm.NodeControllerOwner,
			fsm.LockTimestampAnnotation: time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
			fsm.LockTimeoutAnnotation:   "300",
		}

		c := newClient(t, expired)
		outcome := settle(t, c, expired, events.NewFakeRecorder(8))
		assert.True(t, outcome.Settled, "a lock nothing is running under must not freeze the slot forever")
	})
}

// TestDecideIsTheRule keeps the rule itself readable without any client in the way: the table is
// the contract, and each row says which gate stopped the settlement.
func TestDecideIsTheRule(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name         string
		node         *infrav1alpha1.Node
		nodePresent  bool
		lockHeld     bool
		jobs         []batchv1.Job
		wantSettle   bool
		wantRequeue  time.Duration
		wantReasonIs string
	}{
		{
			name:         "ready, off, nothing behind it",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady),
			wantSettle:   true,
			wantReasonIs: "no Kubernetes node",
		},
		{
			name:         "claimed slot",
			node:         slot(infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
			wantReasonIs: "claim is never overridden",
		},
		{
			name:         "progress that implies no machine",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
			wantReasonIs: "does not imply a machine",
		},
		{
			name:         "machine present",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady),
			nodePresent:  true,
			wantReasonIs: "observation controllers",
		},
		{
			name:         "lock held",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown),
			lockHeld:     true,
			wantReasonIs: "owns the slot",
		},
		{
			name:         "job in flight",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
			jobs:         []batchv1.Job{*lifecycleJob(fsm.PhaseStartup, time.Time{})},
			wantRequeue:  SettleGracePeriod,
			wantReasonIs: "in flight",
		},
		{
			name: "job finished inside the grace",
			node: slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
			jobs: []batchv1.Job{*lifecycleJob(fsm.PhaseStartup, now.Add(-90*time.Second))},
			// 2m of grace, 90s since the Job finished: the look is scheduled for the rest.
			wantRequeue:  30 * time.Second,
			wantReasonIs: "inside the settle grace",
		},
		{
			name:         "job finished past the grace",
			node:         slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
			jobs:         []batchv1.Job{*lifecycleJob(fsm.PhaseStartup, now.Add(-3*time.Minute))},
			wantSettle:   true,
			wantReasonIs: "no Kubernetes node",
		},
		{
			name: "the tightest deadline of several jobs wins",
			node: slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown),
			jobs: []batchv1.Job{
				*lifecycleJob(fsm.PhaseStartup, now.Add(-5*time.Minute)),
				*lifecycleJob(fsm.PhaseShutdown, now.Add(-110*time.Second)),
			},
			wantRequeue:  10 * time.Second,
			wantReasonIs: "inside the settle grace",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(tc.node, tc.nodePresent, tc.lockHeld, tc.jobs, now, SettleGracePeriod)

			assert.Equal(t, tc.wantSettle, d.settle)
			assert.Equal(t, tc.wantRequeue, d.requeueAfter)
			assert.Contains(t, d.reason, tc.wantReasonIs)

			if tc.wantSettle {
				assert.Equal(t, tc.node.Status.Progress, d.previous, "the settlement records what it replaces")
			}
		})
	}
}

// TestSettleGracePeriodIsLongerThanTheWritersItWaitsFor pins the reasoning behind the value: the
// grace exists so a Job that finished, whose after_JobCompleted has not run, is not stomped. The
// monitor that runs it polls every 30s, so the grace must be several polls, and it has to stay
// well under the Job timeout and the stuck-transition threshold, so a slot whose monitor really
// died is settled long before an operator notices it.
func TestSettleGracePeriodIsLongerThanTheWritersItWaitsFor(t *testing.T) {
	assert.GreaterOrEqual(t, SettleGracePeriod, 4*fsm.JobMonitorPollInterval,
		"the grace must cover several monitor polls, the writer it waits for")
	assert.Greater(t, SettleGracePeriod, 30*time.Second, "a reconcile is seconds, the grace must be minutes")
	assert.Less(t, SettleGracePeriod, fsm.DefaultJobTimeout, "a Job of a live monitor is not settled away")
	assert.Less(t, SettleGracePeriod, fsm.StuckTransitionThreshold, "a slot heals long before the stuck threshold")
}

// TestFinishedAtCoversEveryShapeAFinishedJobTakes: a lifecycle Job that cannot be measured must
// still age out of the grace, or it freezes its slot forever. The fallbacks are ordered newest
// first, and an unfinished Job reports "not finished" whatever it carries.
func TestFinishedAtCoversEveryShapeAFinishedJobTakes(t *testing.T) {
	at := time.Now().Add(-4 * time.Minute)
	stamp := metav1.NewTime(at)

	tests := []struct {
		name     string
		job      *batchv1.Job
		wantOK   bool
		wantTime time.Time
	}{
		{
			name:     "completion time",
			job:      lifecycleJob(fsm.PhaseStartup, at),
			wantOK:   true,
			wantTime: at,
		},
		{
			name: "condition, no completion time",
			job: func() *batchv1.Job {
				job := lifecycleJob(fsm.PhaseStartup, at)
				job.Status.CompletionTime = nil
				job.Status.Conditions = []batchv1.JobCondition{{
					Type:               batchv1.JobComplete,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: stamp,
				}}

				return job
			}(),
			wantOK:   true,
			wantTime: at,
		},
		{
			name: "start time, nothing else",
			job: func() *batchv1.Job {
				job := lifecycleJob(fsm.PhaseStartup, at)
				job.Status.CompletionTime = nil

				return job
			}(),
			wantOK:   true,
			wantTime: at,
		},
		{
			name: "creation timestamp, nothing else",
			job: func() *batchv1.Job {
				job := lifecycleJob(fsm.PhaseStartup, time.Time{})
				job.Status.Succeeded = 1
				job.Status.StartTime = nil
				job.CreationTimestamp = stamp

				return job
			}(),
			wantOK:   true,
			wantTime: at,
		},
		{
			name:   "running job is not finished",
			job:    lifecycleJob(fsm.PhaseStartup, time.Time{}),
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := finishedAt(tc.job)
			assert.Equal(t, tc.wantOK, ok)

			if tc.wantOK {
				assert.WithinDuration(t, tc.wantTime, got, time.Second)
			}
		})
	}
}

// TestSettleIgnoresJobsOfAnotherSlot: the selector matches the slot's provenance labels, so a
// neighbouring slot's lifecycle Job - the common shape in a churning pool - must not hold this
// slot's settlement.
func TestSettleIgnoresJobsOfAnotherSlot(t *testing.T) {
	other := lifecycleJob(fsm.PhaseShutdown, time.Time{})
	other.Name = "worker-2-shutdown-1700000000"
	other.Labels[fsm.LabelNodeCR] = "worker-2"
	other.Labels[fsm.LabelNodeLegacy] = "worker-2-k8s"

	node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady)
	c := newClient(t, node, other)

	outcome := settle(t, c, node, events.NewFakeRecorder(8))
	assert.True(t, outcome.Settled)

	jobs, err := lifecycleJobs(context.Background(), c, node)
	require.NoError(t, err)
	assert.Empty(t, jobs, "only the slot's own Jobs are considered")
}

// TestSettlementNoteNamesTheEvidence keeps the operator-facing half of the fix readable: the
// progress that was replaced, the machine that is not there, and the values that were written.
func TestSettlementNoteNamesTheEvidence(t *testing.T) {
	note := settlementNote(slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown), infrav1alpha1.ProgressShuttingDown)

	for _, want := range []string{"shuttingdown", "shutdown", "worker-1", "worker-1-k8s", "desiredPowerState"} {
		assert.Contains(t, note, want)
	}

	assert.False(t, strings.Contains(note, "\n"), "an Event message is one line")
}
