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

package fsm

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
)

// startupAttemptScheme is the type set every startup-attempt test drives: the Node CR, its
// corev1.Node, and the lifecycle Jobs the FSM builds.
func startupAttemptScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	return scheme
}

// unbootableSlot is a slot of a pool whose machine will not come up: claimed (desired on), off, and
// at the stable progress the last attempt rolled back to. That is the shape both failure loops hand
// to the next pass, and the shape the bound has to stop:
//
//   - the loud one, a startup Job that exits non-zero, lands here from afterJobFailed, which writes
//     the progress and nothing else;
//   - the quiet one, a startup Job that completes with no node behind it, lands here from the
//     watchdog's re-arm.
func unbootableSlot() *infrav1alpha1.Node {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShutdown)
	node.Status.PowerState = infrav1alpha1.PowerStateOff

	return node
}

// readSlotCR reads the Node CR back from the client, so the assertions are about what a controller
// would read on the next pass and not about a pointer a hook holds.
func readSlotCR(t *testing.T, c client.Client) *infrav1alpha1.Node {
	t.Helper()

	node := &infrav1alpha1.Node{}
	require.NoError(t, c.Get(context.TODO(),
		client.ObjectKey{Name: "test-node", Namespace: config.NewNamespaceConfig().Get()}, node))

	return node
}

// startupJobCount counts the startup Jobs the FSM created: the machines the pool paid for. It is the
// number a bound has to be measured in, because an attempt that is counted and refused has to leave
// one behind, and an attempt that is refused late must not add one.
func startupJobCount(t *testing.T, c client.Client) int {
	t.Helper()

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(context.TODO(), jobs))

	startups := 0

	for i := range jobs.Items {
		if jobs.Items[i].Labels[LabelType] == PhaseStartup {
			startups++
		}
	}

	return startups
}

// finishStartupJobs marks every startup Job of the slot the way a Job that ran looks to the FSM: a
// succeeded or a failed pod counter, plus a completion time. Two things depend on that. It is the
// evidence monitorJobCompletion reads before it fires JobCompleted / JobFailed, and it is what keeps
// the next StartNode's cleanupPendingJobs from deleting the Job: a finished Job is the record of an
// attempt the pool paid for, a pending one is the residue a new attempt cleans up.
func finishStartupJobs(t *testing.T, c client.Client, failed bool) {
	t.Helper()

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(context.TODO(), jobs))

	now := metav1.Now()

	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Labels[LabelType] != PhaseStartup || job.Labels[LabelNodeCR] != "test-node" {
			continue
		}

		job.Status.StartTime = &now
		job.Status.CompletionTime = &now

		if failed {
			job.Status.Failed = 1
		} else {
			job.Status.Succeeded = 1
		}

		require.NoError(t, c.Status().Update(context.TODO(), job))
	}
}

// rearmTheSlot writes the shape the progress watchdog's re-arm leaves behind - status.progress
// shutdown, status.powerState off, spec.desiredPowerState on - and moves the state machine to the
// state that progress spells. progress.Settle is the writer in a cluster; importing it from the fsm
// tests would be a cycle, and the shape is the contract: it is the state StartNode is legal from.
func rearmTheSlot(t *testing.T, c client.Client, nm *NodeStateMachine) {
	t.Helper()

	fresh := readSlotCR(t, c)
	fresh.Status.Progress = infrav1alpha1.ProgressShutdown
	fresh.Status.PowerState = infrav1alpha1.PowerStateOff
	require.NoError(t, c.Status().Update(context.TODO(), fresh))

	nm.node = fresh
	nm.PrimeEnterState(StateShutdown)
}

// waitOutTheJobNameSecond sleeps past the unix-second that the FSM's lifecycle Job name is built
// from (createStartupJob names it <slot>-startup-<unix seconds>). A test that starts a machine three
// times in a row lands every attempt in the same second, and the fake client then refuses the create
// for a name collision - a test artifact a real pool never hits, because an attempt is a Job that
// takes seconds to fail.
func waitOutTheJobNameSecond() {
	time.Sleep(1100 * time.Millisecond)
}

// TestStartupJobFailureLoopIsBoundedOnTheFourthStart is B2, the loop the bound exists for and that
// the pre-fix counter did not cover. A startup Job that exits non-zero drives
// startingup -> JobFailed -> shutdown, and afterJobFailed writes the *progress* only, so the next
// pass of the controller sees desired on && powerState off && CanTransition(StartNode) and creates
// another Job and another machine. Unbounded, that is one machine per pass forever, with no counter
// and no event; the slot is at a stable progress the whole time, so the progress watchdog - which
// acts on a progress that implies a machine - never sees it at all.
//
// Three attempts are allowed, each counted by the transition that began it. The fourth StartNode is
// refused, records exactly one ClaimRearmLimitReached Warning, leaves the slot at the stable
// off/shutdown pair with its claim, and creates no Job. Every later pass is silent.
func TestStartupJobFailureLoopIsBoundedOnTheFourthStart(t *testing.T) {
	scheme := startupAttemptScheme(t)

	node := unbootableSlot()
	fakeClient := CreateFakeClient(scheme, node)

	rec := events.NewFakeRecorder(64)
	coord := NewMockCoordinationManager()

	nm := NewNodeStateMachine(node, fakeClient, scheme, coord, WithEventRecorder(rec))

	for attempt := 1; attempt <= StartupAttemptLimit; attempt++ {
		waitOutTheJobNameSecond()

		require.NoErrorf(t, nm.StartNode(), "attempt %d of %d is inside the bound", attempt, StartupAttemptLimit)

		assert.Equal(t, attempt, StartupAttemptsOf(readSlotCR(t, fakeClient)),
			"the attempt is counted where it begins, before its Job can fail: the Job failing is not the proof of a working machine")

		// The loud failure: the startup Job exited non-zero, the FSM rolls the slot back to shutdown.
		finishStartupJobs(t, fakeClient, true)
		require.NoError(t, nm.JobFailed())

		current := readSlotCR(t, fakeClient)
		assert.Equal(t, infrav1alpha1.ProgressShutdown, current.Status.Progress,
			"the rollback leaves the stable progress - which is why the watchdog never sees this loop and the FSM has to bound it")
		assert.Equal(t, infrav1alpha1.PowerStateOff, current.Status.PowerState)
		assert.Equal(t, StateShutdown, nm.GetCurrentState())
		assert.False(t, coord.HasLock("test-node"), "a rolled-back operation releases the lock it took")
	}

	require.Equal(t, StartupAttemptLimit, startupJobCount(t, fakeClient), "one machine per attempt")
	drainEvents(rec)

	err := nm.StartNode()
	require.Error(t, err, "the 4th StartNode is refused")

	refusal := AsStartupAttemptLimit(err)
	require.NotNil(t, refusal, "and it is reported as the bound, not as a failed transition")
	assert.Equal(t, StartupAttemptLimit, refusal.Attempts)
	assert.Equal(t, StartupAttemptLimit, refusal.Limit)

	assert.Equal(t, StartupAttemptLimit, startupJobCount(t, fakeClient), "no further Job, so no further machine")

	current := readSlotCR(t, fakeClient)
	assert.Equal(t, infrav1alpha1.ProgressShutdown, current.Status.Progress, "the slot settles at the stable pair")
	assert.Equal(t, infrav1alpha1.PowerStateOff, current.Status.PowerState)
	assert.Equal(t, infrav1alpha1.PowerStateOn, current.Spec.DesiredPowerState, "the claim is the autoscaler's to drop, never this refusal's")
	assert.False(t, coord.HasLock("test-node"), "a refusal takes no lock: it starts nothing")

	record := StartupAttemptRecord(current)
	require.NotNil(t, record)
	assert.Equal(t, ReasonStartupAttemptLimit, record.Reason, "the record is the receipt and the deduplication key")

	recorded := drainEvents(rec)
	require.Len(t, recorded, 1, "exactly one event for the bound")
	assert.Contains(t, recorded[0], corev1.EventTypeWarning, "a slot that will not boot and is counted as requested capacity is a Warning")
	assert.Contains(t, recorded[0], ReasonStartupAttemptLimitReached)
	assert.Contains(t, recorded[0], fmt.Sprint(StartupAttemptLimit), "the event names the attempts it spent")
	assert.Contains(t, recorded[0], "exited non-zero", "and the evidence the slot carries: this loop dies in its Job")

	// Steady state: the refusal is not a retry. The passes after it write nothing and record nothing,
	// so a slot that is permanently broken costs one event, not one per resync.
	before := readSlotCR(t, fakeClient).ResourceVersion

	require.Error(t, nm.StartNode())
	require.Error(t, nm.StartNode())

	assert.Empty(t, drainEvents(rec), "the bound is reported once")
	assert.Equal(t, before, readSlotCR(t, fakeClient).ResourceVersion, "and the steady state writes nothing")
	assert.Equal(t, StartupAttemptLimit, startupJobCount(t, fakeClient), "no machine is bought by a refusal")
}

// TestStartupAttemptsResetWhenTheReadinessGateCompletes is B1's reset rule, and the reason the bound
// is a bound on a claim episode rather than on the life of a slot. Slots are long-lived and are
// reused: internal/grpcserver picks an existing powered-off Node CR for a new claim, so a count that
// is never cleared turns three unrelated failures over a machine's life into a slot that never
// retries a machine again - which is exactly what the pre-fix counter did, because nothing removed
// its record.
//
// The gate is what makes status.progress: ready mean something, so it is what resets: two attempts
// die, the third boots a machine that registers, reports Ready, is free of the readiness taint and
// is uncordoned - and the count goes to zero. Three more attempts are then allowed.
func TestStartupAttemptsResetWhenTheReadinessGateCompletes(t *testing.T) {
	scheme := startupAttemptScheme(t)

	node := unbootableSlot()

	// The machine of a successful boot: cordoned by the scale-down that preceded it, Ready, and with
	// no bootstrap taint on it, so the gate is satisfied on the first pass.
	kubeNode := CreateTestKubernetesNode("test-node-k8s", true)
	fakeClient := CreateFakeClient(scheme, node, kubeNode)

	rec := events.NewFakeRecorder(64)

	nm := NewNodeStateMachine(node, fakeClient, scheme, NewMockCoordinationManager(),
		WithEventRecorder(rec),
		WithReadinessTaintKeys([]string{TaintCiliumAgentNotReady}))
	nm.uncordonTimeout = 20 * time.Second
	nm.readinessGateTimeout = 20 * time.Second

	for attempt := 1; attempt <= 2; attempt++ {
		waitOutTheJobNameSecond()

		require.NoErrorf(t, nm.StartNode(), "attempt %d is inside the bound", attempt)
		finishStartupJobs(t, fakeClient, true)
		require.NoError(t, nm.JobFailed())
	}

	require.Equal(t, 2, StartupAttemptsOf(readSlotCR(t, fakeClient)), "the failures are counted")

	waitOutTheJobNameSecond()

	require.NoError(t, nm.StartNode())
	require.Equal(t, 3, StartupAttemptsOf(readSlotCR(t, fakeClient)),
		"the third attempt is the one the bound is about: the slot is one machine away from being frozen")

	finishStartupJobs(t, fakeClient, false)
	require.NoError(t, nm.JobCompleted(CreateTestJob("test-node-startup-1", config.NewNamespaceConfig().Get(), PhaseStartup)))

	after := readSlotCR(t, fakeClient)
	assert.Equal(t, infrav1alpha1.ProgressReady, after.Status.Progress, "the boot completed")
	assert.Zero(t, StartupAttemptsOf(after), "a boot that passes the gate clears the record")
	assert.Nil(t, StartupAttemptRecord(after), "and the slot carries no stale count for the next episode")

	// The pool scales the slot back down, and a new claim episode starts on the same slot. The count
	// starts with it: three attempts are allowed, and only the fourth of that episode is refused.
	// Without the reset this is where a slot that booted a working machine once would be frozen for
	// the rest of its life.
	require.NoError(t, nm.ShutdownNode())
	require.NoError(t, nm.JobCompleted(CreateTestJob("test-node-shutdown-1", config.NewNamespaceConfig().Get(), PhaseShutdown)))
	require.Equal(t, StateShutdown, nm.GetCurrentState())

	for attempt := 1; attempt <= StartupAttemptLimit; attempt++ {
		waitOutTheJobNameSecond()

		require.NoErrorf(t, nm.StartNode(),
			"attempt %d of the new episode is allowed: the bound is per claim episode, not per slot", attempt)
		assert.Equal(t, attempt, StartupAttemptsOf(readSlotCR(t, fakeClient)))

		finishStartupJobs(t, fakeClient, true)
		require.NoError(t, nm.JobFailed())
	}

	refusal := AsStartupAttemptLimit(nm.StartNode())
	require.NotNil(t, refusal, "the fourth attempt of the new episode is the one the bound stops")
	assert.Equal(t, StartupAttemptLimit, refusal.Attempts)
}

// TestACompletedStartupJobWithTheGatePendingDoesNotResetTheCount is the reset rule's boundary. The
// gate is what makes ready mean something, so a startup whose Job succeeded and whose machine never
// clears the CNI bootstrap taint has spent its attempts and must keep them counted: resetting on
// JobCompleted alone clears the bound by the failure it bounds, and the loop that buys machines for a
// node that takes no pods starts over every time.
func TestACompletedStartupJobWithTheGatePendingDoesNotResetTheCount(t *testing.T) {
	scheme := startupAttemptScheme(t)

	node := unbootableSlot()

	bootstrap := corev1.Taint{Key: TaintCiliumAgentNotReady, Value: "true", Effect: corev1.TaintEffectNoExecute}
	kubeNode := CreateTestKubernetesNode("test-node-k8s", true)
	kubeNode.Spec.Taints = []corev1.Taint{bootstrap}

	fakeClient := CreateFakeClient(scheme, node, kubeNode)

	rec := events.NewFakeRecorder(64)

	nm := NewNodeStateMachine(node, fakeClient, scheme, NewMockCoordinationManager(),
		WithEventRecorder(rec),
		WithReadinessTaintKeys([]string{TaintCiliumAgentNotReady}),
		WithReadinessGateTimeout(100*time.Millisecond))
	nm.uncordonTimeout = 20 * time.Second

	for attempt := 1; attempt <= 2; attempt++ {
		waitOutTheJobNameSecond()

		require.NoErrorf(t, nm.StartNode(), "attempt %d is inside the bound", attempt)
		finishStartupJobs(t, fakeClient, true)
		require.NoError(t, nm.JobFailed())
	}

	waitOutTheJobNameSecond()

	require.NoError(t, nm.StartNode())
	require.Equal(t, StartupAttemptLimit, StartupAttemptsOf(readSlotCR(t, fakeClient)))

	finishStartupJobs(t, fakeClient, false)
	require.NoError(t, nm.JobCompleted(CreateTestJob("test-node-startup-1", config.NewNamespaceConfig().Get(), PhaseStartup)))

	after := readSlotCR(t, fakeClient)
	assert.Equal(t, infrav1alpha1.ProgressReady, after.Status.Progress,
		"a gate timeout completes the transition as it always has: the slot is not wedged")
	assert.Equal(t, StartupAttemptLimit, StartupAttemptsOf(after),
		"and the gate that timed out does not reset the attempts: the machine is up and takes no pods, which is the failure the bound prices")

	warnings := drainEvents(rec)
	require.NotEmpty(t, warnings)

	gated := false
	for _, event := range warnings {
		if strings.Contains(event, ReasonReadinessGateTimeout) {
			gated = true
		}
	}

	assert.True(t, gated, "the record of a gate that timed out is the Warning the count is kept for")

	// The slot's machines never became usable, so the pool re-arms it (the watchdog writes the stable
	// pair, claim kept) and the next start is refused by the count the gate did not clear.
	rearmTheSlot(t, fakeClient, nm)

	refusal := AsStartupAttemptLimit(nm.StartNode())
	require.NotNil(t, refusal, "the bound is what stops the next machine, not the gate")
	assert.Equal(t, StartupAttemptLimit, refusal.Attempts)
	assert.Equal(t, StartupAttemptLimit, startupJobCount(t, fakeClient), "the attempts spent are the machines bought, and no more")
}

// TestTheRegistrationWaitKeepsTheCoordinationLockAlive is the first nit. The loop re-stamped the lock
// only once the node object appeared, so a startup Job longer than ~3m arrives at the gate with its
// lock already aged out (DefaultLockTimeout is 5m, and RenewLock will not resurrect an expired lock):
// the whole 4m readiness gate then runs with nothing protecting the slot, which is the window in
// which the next acquisition, the expired-lock cleanup and the progress watchdog all conclude the
// operation is over.
func TestTheRegistrationWaitKeepsTheCoordinationLockAlive(t *testing.T) {
	scheme := startupAttemptScheme(t)

	node := unbootableSlot()
	node.Status.Progress = infrav1alpha1.ProgressStartingUp

	// No corev1.Node at all: the whole budget is the registration wait.
	fakeClient := CreateFakeClient(scheme, node)

	coord := NewMockCoordinationManager()
	require.NoError(t, coord.AcquireLock(context.TODO(), node, OperationScaleUp, DefaultLockTimeout))

	nm := NewNodeStateMachine(node, fakeClient, scheme, coord)
	nm.uncordonTimeout = 2 * time.Second
	nm.readinessGateTimeout = 20 * time.Second

	err := nm.setNodeSchedulableWithRetry("test-node-k8s")
	require.Error(t, err, "a machine that never registers times out the registration wait")

	renews := coord.GetRenewCalls()
	require.NotEmpty(t, renews,
		"the registration wait re-stamps the lock it runs under, so the gate that follows starts on a live lock")
	assert.Equal(t, OperationScaleUp, coord.GetLock("test-node").Operation, "renewal keeps the operation the lock names")
}

// TestStartupAttemptRecordShape pins the record the bound, the reset and the deduplication all read:
// the count is in the Reason, an unreadable Reason reads as no attempts (never as a freeze), and the
// limit Reason reads as the limit. It is the wire contract between the two writers (the FSM's
// enterStartingUp) and the two readers (StartNode, progress.Settle).
func TestStartupAttemptRecordShape(t *testing.T) {
	node := unbootableSlot()

	assert.Zero(t, StartupAttemptsOf(node), "no record, no attempts")
	assert.False(t, StartupAttemptsExhausted(node))
	assert.False(t, StartupAttemptLimitRecorded(node))

	assert.Equal(t, 1, recordStartupAttempt(node))
	assert.Equal(t, 2, recordStartupAttempt(node))
	assert.Equal(t, 2, StartupAttemptsOf(node))
	assert.Equal(t, ReasonStartupAttempt+"2", StartupAttemptRecord(node).Reason)

	require.Equal(t, StartupAttemptLimit, recordStartupAttempt(node))
	assert.True(t, StartupAttemptsExhausted(node), "the limit-th attempt is the last one allowed")

	assert.True(t, clearStartupAttempts(node), "the gate removes the record")
	assert.False(t, clearStartupAttempts(node), "and a second pass has nothing to remove, so its caller writes nothing")
	assert.Zero(t, StartupAttemptsOf(node))

	StampStartupAttemptLimit(node, StartupAttemptLimit, "the startup Jobs exited non-zero")
	assert.Equal(t, ReasonStartupAttemptLimit, StartupAttemptRecord(node).Reason)
	assert.True(t, StartupAttemptLimitRecorded(node))
	assert.Equal(t, StartupAttemptLimit, StartupAttemptsOf(node), "a spent budget reads as the budget")

	assert.True(t, clearStartupAttempts(node), "the limit note is cleared by the same rule as the count")

	node.Status.Conditions = []metav1.Condition{
		{Type: ConditionStartupAttempts, Status: metav1.ConditionTrue, Reason: "SomethingElse"},
	}

	assert.Zero(t, StartupAttemptsOf(node), "a Reason this package does not write is not a count, and never a freeze")
	assert.False(t, StartupAttemptsExhausted(node))
}
