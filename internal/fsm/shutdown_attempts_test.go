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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
)

// unshuttableSlot is a slot of a pool whose machine will not go off: claimed off (the autoscaler
// dropped it, or a scale-down asked for it), observed up, and at the progress the last completion was
// refused back into. That is the shape the refused-completion loop hands to the next pass, and the
// shape the shutdown bound has to stop.
func unshuttableSlot() *infrav1alpha1.Node {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady)
	node.Status.PowerState = infrav1alpha1.PowerStateOn

	stamp := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	node.Status.LastShutdownTime = &stamp

	return node
}

// shutdownJobCount counts the shutdown Jobs the FSM created: the power operations the pool paid for.
// It is the number a shutdown bound has to be measured in, because a refused attempt must not add one.
func shutdownJobCount(t *testing.T, c client.Client) int {
	t.Helper()

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(context.TODO(), jobs))

	shutdowns := 0

	for i := range jobs.Items {
		if jobs.Items[i].Labels[LabelType] == PhaseShutdown {
			shutdowns++
		}
	}

	return shutdowns
}

// finishLatestShutdownJob marks the newest shutdown Job of the slot the way a Job that ran looks to
// the FSM: a succeeded counter and a completion time at completedAt. It returns that Job, which is the
// object the lifecycle Job monitor hands the completion - and whose completionTime the guard measures
// the kubelet's heartbeat against.
//
// The Jobs of the earlier attempts stay in the cluster (a fake client does not run the TTL
// controller), and that is the shape the loop has in a real cluster too while a Job of the same slot
// is inside its TTL: the newest one is the attempt that just ran.
func finishLatestShutdownJob(t *testing.T, c client.Client, completedAt time.Time) *batchv1.Job {
	t.Helper()

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(context.TODO(), jobs))

	var latest *batchv1.Job

	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Labels[LabelType] != PhaseShutdown || job.Labels[LabelNodeCR] != "test-node" {
			continue
		}

		if latest == nil || job.Name > latest.Name {
			// The FSM names lifecycle Jobs "<slot>-shutdown-<unix seconds>", so the name orders them.
			latest = job
		}
	}

	require.NotNil(t, latest, "the attempt produced no shutdown Job")

	finished := latest.DeepCopy()

	completed := metav1.NewTime(completedAt)
	finished.Status.StartTime = &completed
	finished.Status.CompletionTime = &completed
	finished.Status.Succeeded = 1

	require.NoError(t, c.Status().Update(context.TODO(), finished))

	return finished
}

// TestTheRefusedShutdownCompletionLoopIsBoundedOnTheFourthAttempt is the bound on the loop Fix B
// opens, driven end to end: a shutdown Job that exits 0 while the machine is observed up (a live
// kubelet heartbeating after the Job completed), the completion refused, the slot back at ready with
// its claim off, and the next pass firing ShutdownNode again. Unbounded that is one shutdown Job, one
// cordon of a node that is serving pods, and one power operation on a real machine, per pass, forever
// - and pre-PR this failure was static, so the loop is a cost the fix introduces and has to bound.
//
// Three attempts are allowed, each counted where it begins (the transition into shuttingdown). The
// fourth ShutdownNode is refused: it builds no Job, takes no lock, cordons nothing, leaves the slot at
// the observed pair, and records exactly one ShutdownAttemptLimitReached Warning. Every later pass is
// silent.
func TestTheRefusedShutdownCompletionLoopIsBoundedOnTheFourthAttempt(t *testing.T) {
	scheme := startupAttemptScheme(t)

	node := unshuttableSlot()

	// The machine is up and says so: Ready, with a heartbeat stamped now, after every completion below.
	live := CreateTestKubernetesNode("test-node-k8s", false)
	live.Status.Conditions[0].LastHeartbeatTime = metav1.Now()

	fakeClient := CreateFakeClient(scheme, node, live)

	rec := events.NewFakeRecorder(64)
	coord := NewMockCoordinationManager()

	nm := NewNodeStateMachine(node, fakeClient, scheme, coord, WithEventRecorder(rec))

	for attempt := 1; attempt <= ShutdownAttemptLimit; attempt++ {
		waitOutTheJobNameSecond()

		require.NoErrorf(t, nm.ShutdownNode(), "attempt %d of %d is inside the bound", attempt, ShutdownAttemptLimit)

		assert.Equal(t, attempt, ShutdownAttemptsOf(readSlotCR(t, fakeClient)),
			"the attempt is counted where it begins, on the transition the lock, the cordon and the Job hang off")
		assert.Equal(t, StateShuttingDown, nm.GetCurrentState())

		// The script exits 0 (the Job completed) and the machine is observed up: the completion is
		// refused, the slot goes back to ready, and the claim reaches ShutdownNode again.
		job := finishLatestShutdownJob(t, fakeClient, time.Now().Add(-5*time.Second))
		require.NoError(t, nm.JobCompleted(job))

		current := readSlotCR(t, fakeClient)
		assert.Equal(t, infrav1alpha1.ProgressReady, current.Status.Progress,
			"the refusal records what the observation sees, which is what makes the next shutdown legal - and what the bound has to stop")
		assert.Equal(t, infrav1alpha1.PowerStateOn, current.Status.PowerState)
		assert.False(t, coord.HasLock("test-node"), "the refused operation released the lock it took")
	}

	require.Equal(t, ShutdownAttemptLimit, shutdownJobCount(t, fakeClient), "one shutdown Job per attempt")

	recorded := drainEvents(rec)
	assert.NotEmpty(t, withReasonOf(recorded, ReasonShutdownNotObserved),
		"the refusals are what an operator saw: %v", recorded)

	drainEvents(rec)

	err := nm.ShutdownNode()
	require.Error(t, err, "the 4th shutdown is refused")

	refusal := AsShutdownAttemptLimit(err)
	require.NotNil(t, refusal, "and it is reported as the bound, not as a failed transition")
	assert.Equal(t, ShutdownAttemptLimit, refusal.Attempts)
	assert.Equal(t, ShutdownAttemptLimit, refusal.Limit)

	assert.Equal(t, ShutdownAttemptLimit, shutdownJobCount(t, fakeClient), "no further Job, so no further power operation")
	assert.False(t, coord.HasLock("test-node"), "a refusal takes no lock: it starts nothing")

	current := readSlotCR(t, fakeClient)
	assert.Equal(t, infrav1alpha1.ProgressReady, current.Status.Progress,
		"the slot rests at the pair the observation says: ready, on, claimed off. That is the documented hole - the machine is up and paid for - and it is healable: from ready ShutdownNode is legal the moment the pool's script is fixed")
	assert.Equal(t, infrav1alpha1.PowerStateOn, current.Status.PowerState)
	assert.Equal(t, infrav1alpha1.PowerStateOff, current.Spec.DesiredPowerState, "the claim is the autoscaler's to drop, never this refusal's")

	record := ShutdownAttemptRecord(current)
	require.NotNil(t, record)
	assert.Equal(t, ReasonShutdownAttemptLimit, record.Reason, "the record is the receipt and the deduplication key")

	eventsOfRefusal := drainEvents(rec)
	require.Len(t, eventsOfRefusal, 1, "exactly one event for the bound: %v", eventsOfRefusal)
	assert.Contains(t, eventsOfRefusal[0], corev1.EventTypeWarning,
		"a machine that cannot be powered off, cordoned and paid for, is a Warning")
	assert.Contains(t, eventsOfRefusal[0], ReasonShutdownAttemptLimitReached)
	assert.Contains(t, eventsOfRefusal[0], fmt.Sprint(ShutdownAttemptLimit), "the event names the attempts it spent")
	assert.Contains(t, eventsOfRefusal[0], "test-node-k8s", "and the machine that stayed up")

	// Steady state: the refusal is not a retry. Later passes find the same count, write nothing and
	// record nothing, so a pool that cannot power off costs one event and not one per resync.
	before := readSlotCR(t, fakeClient).ResourceVersion

	require.Error(t, nm.ShutdownNode())
	require.Error(t, nm.ShutdownNode())

	assert.Empty(t, drainEvents(rec), "the bound is reported once")
	assert.Equal(t, before, readSlotCR(t, fakeClient).ResourceVersion, "and the steady state writes nothing")
	assert.Equal(t, ShutdownAttemptLimit, shutdownJobCount(t, fakeClient), "no Job is built by a refusal")
}

// withReasonOf returns the recorded events carrying an event reason, the way `kubectl describe`
// shows them: "<Type> <Reason> <note>".
func withReasonOf(recorded []string, reason string) []string {
	var found []string

	for _, event := range recorded {
		if strings.Contains(event, reason) {
			found = append(found, event)
		}
	}

	return found
}

// TestAShutdownTheObservationAgreesWithClearsTheRecord is the bound's reset rule, the first of the two:
// a shutdown completion the observation agreed with is an attempt that landed, so the scale-down
// episode is over and the count starts over. Without it, three shutdowns that failed in one episode of
// a machine's life would be the last three the controller ever runs on a slot that is reused across
// claims - the lifetime-sentence failure the startup counter's first version had.
func TestAShutdownTheObservationAgreesWithClearsTheRecord(t *testing.T) {
	scheme := startupAttemptScheme(t)

	node := unshuttableSlot()

	// The machine is off: the node object is Ready - it stays so for node-monitor-grace-period - with
	// a heartbeat that stopped when the shutdown started, and no Job completion to beat.
	kubeNode := CreateTestKubernetesNode("test-node-k8s", false)
	dead := metav1.NewTime(node.Status.LastShutdownTime.Add(5 * time.Second))
	kubeNode.Status.Conditions[0].LastHeartbeatTime = dead

	fakeClient := CreateFakeClient(scheme, node, kubeNode)

	rec := events.NewFakeRecorder(64)

	nm := NewNodeStateMachine(node, fakeClient, scheme, NewMockCoordinationManager(), WithEventRecorder(rec))

	// Two attempts that were refused, then the one that lands.
	stampShutdownAttempts(t, fakeClient, 2)
	require.Equal(t, 2, ShutdownAttemptsOf(readSlotCR(t, fakeClient)))

	waitOutTheJobNameSecond()
	require.NoError(t, nm.ShutdownNode())

	job := finishLatestShutdownJob(t, fakeClient, time.Now().Add(-5*time.Second))
	require.NoError(t, nm.JobCompleted(job))

	after := readSlotCR(t, fakeClient)

	assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress, "the shutdown is recorded: the observation agrees")
	require.NotNil(t, shutdownCondition(after), "with its condition, as it always was")
	assert.Zero(t, ShutdownAttemptsOf(after), "and the attempts of this episode are cleared by the shutdown that landed")
	assert.Nil(t, ShutdownAttemptRecord(after), "the slot carries no stale count for the next scale-down")

	recorded := drainEvents(rec)
	assert.NotEmpty(t, withReasonOf(recorded, ReasonScaleDownCompleted))
	assert.Empty(t, withReasonOf(recorded, ReasonShutdownNotObserved), "nothing was refused: %v", recorded)

	// The slot is claimed again, boots, and is scaled down again: the next episode starts unspent. The
	// machine came back up (ready), and the scale-down of that episode counts from one.
	nm.PrimeEnterState(StateReady)

	waitOutTheJobNameSecond()

	require.NoError(t, nm.ShutdownNode())
	assert.Equal(t, 1, ShutdownAttemptsOf(readSlotCR(t, fakeClient)), "a new episode counts from one")
}

// TestABootThatPassesTheGateClearsTheShutdownRecord is the second reset rule: a machine that booted,
// registered, cleared its bootstrap taint and is uncordoned has ended the episode in which its
// scale-downs were refused, so the shutdown bound starts over at the same gate that resets the startup
// bound. The gate is the place that makes status.progress: ready mean something, and it is the only
// place either counter is cleared by success.
func TestABootThatPassesTheGateClearsTheShutdownRecord(t *testing.T) {
	scheme := startupAttemptScheme(t)

	node := unbootableSlot()

	// Two refused scale-downs are recorded on the slot, in the wire shape the bound reads.
	recordShutdownAttempt(node)
	recordShutdownAttempt(node)

	kubeNode := CreateTestKubernetesNode("test-node-k8s", true)
	fakeClient := CreateFakeClient(scheme, node, kubeNode)

	require.Equal(t, 2, ShutdownAttemptsOf(readSlotCR(t, fakeClient)), "the refusals are counted")

	rec := events.NewFakeRecorder(64)

	nm := NewNodeStateMachine(node, fakeClient, scheme, NewMockCoordinationManager(),
		WithEventRecorder(rec),
		WithReadinessTaintKeys([]string{TaintCiliumAgentNotReady}))
	nm.uncordonTimeout = 20 * time.Second
	nm.readinessGateTimeout = 20 * time.Second

	waitOutTheJobNameSecond()
	require.NoError(t, nm.StartNode())

	finishStartupJobs(t, fakeClient, false)
	require.NoError(t, nm.JobCompleted(CreateTestJob("test-node-startup-1", config.NewNamespaceConfig().Get(), PhaseStartup)))

	after := readSlotCR(t, fakeClient)

	assert.Equal(t, infrav1alpha1.ProgressReady, after.Status.Progress, "the boot completed through the gate")
	assert.Nil(t, ShutdownAttemptRecord(after), "and the gate clears the shutdown record with the startup one")
	assert.Zero(t, ShutdownAttemptsOf(after))
}

// TestShutdownAttemptRecordShape pins the record the bound, the reset and the deduplication all read:
// the count rides on the Reason, a slot with no record has spent nothing, the limit Reason reads as the
// bound (so the count cannot be escaped by a Reason this package does not write), and the limit note is
// a stable deduplication key.
func TestShutdownAttemptRecordShape(t *testing.T) {
	node := unshuttableSlot()

	assert.Zero(t, ShutdownAttemptsOf(node), "no record, no attempts")
	assert.False(t, ShutdownAttemptsExhausted(node))
	assert.False(t, ShutdownAttemptLimitRecorded(node))

	assert.Equal(t, 1, recordShutdownAttempt(node))
	assert.Equal(t, 2, recordShutdownAttempt(node))

	assert.Equal(t, 2, ShutdownAttemptsOf(node))
	assert.Equal(t, ReasonShutdownAttempt+"2", ShutdownAttemptRecord(node).Reason)
	assert.False(t, ShutdownAttemptsExhausted(node), "the third attempt is inside the bound")

	assert.Equal(t, ShutdownAttemptLimit, recordShutdownAttempt(node))
	assert.True(t, ShutdownAttemptsExhausted(node), "the fourth is not")

	require.True(t, clearShutdownAttempts(node), "a record that is there is removable")
	assert.False(t, clearShutdownAttempts(node), "and removing it twice writes nothing on the second pass")
	assert.Zero(t, ShutdownAttemptsOf(node))

	StampShutdownAttemptLimit(node, ShutdownAttemptLimit, "each shutdown Job exited 0 on a machine that stayed up")

	assert.Equal(t, ShutdownAttemptLimit, ShutdownAttemptsOf(node), "the limit record reads as the bound, not as a parse failure")
	assert.True(t, ShutdownAttemptsExhausted(node))
	assert.True(t, ShutdownAttemptLimitRecorded(node), "the note is the deduplication key")
	assert.Contains(t, ShutdownAttemptRecord(node).Message, "each shutdown Job exited 0 on a machine that stayed up")
	assert.Contains(t, ShutdownAttemptRecord(node).Message, "test-node-k8s", "the record names the machine")

	// A Reason this package does not write is not a way out of the bound.
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:   ConditionShutdownAttempts,
		Status: metav1.ConditionTrue,
		Reason: "SomethingElse",
	})

	assert.Zero(t, ShutdownAttemptsOf(node), "an unreadable count is 0, not a frozen slot")
	assert.False(t, ShutdownAttemptLimitRecorded(node))
}

// TestShutdownJobCarriesTheTTLTheSlotDeclares pins the shape the observed-power rule's stamp fallback
// exists for: spec.shutdownPodSpec.ttlSecondsAfterFinished is copied onto the lifecycle Job, and a pool
// that declares 0 has no Job object to measure a grace against seconds after the script finished.
func TestShutdownJobCarriesTheTTLTheSlotDeclares(t *testing.T) {
	scheme := startupAttemptScheme(t)

	node := unshuttableSlot()

	ttl := int32(0)
	node.Spec.ShutdownPodSpec.TTLSecondsAfterFinished = &ttl

	fakeClient := CreateFakeClient(scheme, node, CreateTestKubernetesNode("test-node-k8s", false))

	nm := NewNodeStateMachine(node, fakeClient, scheme, NewMockCoordinationManager())

	require.NoError(t, nm.ShutdownNode())

	job := finishLatestShutdownJob(t, fakeClient, time.Now())
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished, "the FSM stamps the TTL on the Job it creates")
	assert.Zero(t, *job.Spec.TTLSecondsAfterFinished,
		"a TTL of 0 is the collection-immediately shape: the Job object is gone, and only status.lastShutdownTime carries the window")

	// The default, for the pools that declare nothing: the Job object is there for ten minutes, so the
	// completion time is available to the guard for every completion it runs.
	defaulted := unshuttableSlot()
	defaultClient := CreateFakeClient(scheme, defaulted, CreateTestKubernetesNode("test-node-k8s", false))

	require.NoError(t, NewNodeStateMachine(defaulted, defaultClient, scheme, NewMockCoordinationManager()).ShutdownNode())

	created := finishLatestShutdownJob(t, defaultClient, time.Now())
	require.NotNil(t, created.Spec.TTLSecondsAfterFinished)
	assert.EqualValues(t, 600, *created.Spec.TTLSecondsAfterFinished, "the default keeps the Job - and its completionTime - readable")
}

// stampShutdownAttempts writes the shutdown-attempt record onto the slot in the client, in the wire
// shape the bound reads: the record the FSM writes on the transition into shuttingdown, not one a test
// of a consumer invents.
func stampShutdownAttempts(t *testing.T, c client.Client, attempts int) {
	t.Helper()

	current := readSlotCR(t, c)

	for i := 0; i < attempts; i++ {
		recordShutdownAttempt(current)
	}

	require.NoError(t, c.Status().Update(context.TODO(), current))
}
