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
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
)

// observedSlot is the live shape: the slot is claimed off (the autoscaler dropped it), its machine
// is observed up (status.powerState on, and the corev1.Node Ready - the same observation), and
// status.progress says there is no machine. progress is the value under test.
func observedSlot(desired infrav1alpha1.PowerState, progress infrav1alpha1.Progress) *infrav1alpha1.Node {
	node := slot(desired, progress)
	node.Status.PowerState = infrav1alpha1.PowerStateOn

	return node
}

// newClientWithInterceptor builds the same fake client newClient builds, with injected read failures,
// so a test can put the rule in front of a partially-failed observation - a lifecycle Job List the API
// server refused, a corev1.Node this pass could not get - and assert what it writes (nothing) and what
// it returns (the error).
func newClientWithInterceptor(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(watchdogScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1alpha1.Node{}).
		WithInterceptorFuncs(funcs).
		Build()
}

// reconcileObservedPower runs one pass of the rule against the fake client.
func reconcileObservedPower(t *testing.T, c client.Client, node *infrav1alpha1.Node, recorder events.EventRecorder) Outcome {
	t.Helper()

	outcome, err := ReconcileObservedPower(context.Background(), c, node, recorder, SettleGracePeriod)
	require.NoError(t, err)

	return outcome
}

// drainEvents empties the fake recorder's buffered channel. The recorder is a channel, so a test
// that expects one event must not leave the rest in the buffer when it asserts "nothing was
// recorded": the size of the channel is the test's, and draining it is how the count is the truth.
func drainEvents(rec *events.FakeRecorder) []string {
	var recorded []string

	for {
		select {
		case event := <-rec.Events:
			recorded = append(recorded, event)
		default:
			return recorded
		}
	}
}

// TestObservedPowerCorrectsAProgressTheObservedOnContradicts is the live failure: a slot whose
// machine is observed up, whose claim is off, and whose status.progress is one of the values the FSM
// reads as StateShutdown. From that progress ShutdownNode is unreachable, so the reconcile logs
// "Node is in desired state or transitioning" and the machine keeps running for as long as the slot
// lives - 40 minutes in the live trace, and only a hand-patched status ended it.
//
// The rule writes the progress the observation says (ready), records exactly one event naming the
// progress it replaced, and never touches the claim: acting on the claim is the FSM's branch.
func TestObservedPowerCorrectsAProgressTheObservedOnContradicts(t *testing.T) {
	for _, progress := range []infrav1alpha1.Progress{
		infrav1alpha1.ProgressShutdown,
		infrav1alpha1.ProgressShuttingDown,
		"", // the empty progress of a slot that was never seeded is StateShutdown too
	} {
		for _, desired := range []infrav1alpha1.PowerState{infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn} {
			name := "progress " + string(progress) + ", claim " + string(desired)

			t.Run(name, func(t *testing.T) {
				rec := events.NewFakeRecorder(8)
				node := observedSlot(desired, progress)
				c := newClient(t, node, liveNode())

				outcome := reconcileObservedPower(t, c, node, rec)

				assert.Equal(t, ActionCorrectToReady, outcome.Action, "the observation outranks the progress")
				assert.True(t, outcome.Corrected, "the correction wrote")
				assert.Equal(t, progress, outcome.Previous, "the outcome names the progress it replaced")
				assert.Zero(t, outcome.RequeueAfter, "the correction is now, not on a deadline")

				after := readSlot(t, c)
				assert.Equal(t, infrav1alpha1.ProgressReady, after.Status.Progress)
				assert.Equal(t, infrav1alpha1.PowerStateOn, after.Status.PowerState, "the observation is not rewritten")
				assert.Equal(t, desired, after.Spec.DesiredPowerState, "the claim is the request, this rule never answers it")

				// The caller's copy is updated in place, so the FSM branch of the same pass reads
				// the corrected progress and not the value it replaced.
				assert.Equal(t, infrav1alpha1.ProgressReady, node.Status.Progress)

				recorded := drainEvents(rec)
				require.Len(t, recorded, 1, "one event for one correction")
				assert.Contains(t, recorded[0], ReasonMachineObservedUp,
					"the reason is what `kubectl describe` and `kubectl get events` select on")

				want := "Warning"
				if desired == infrav1alpha1.PowerStateOn {
					// A machine that is up and wanted: the progress was the only thing wrong.
					want = "Normal"
				}

				assert.Contains(t, recorded[0], want, "an unwanted machine is the shape worth a Warning")
				assert.Contains(t, recorded[0], "worker-1-k8s", "the event names the machine that was observed")
				assert.Contains(t, recorded[0], "ready", "the event names the progress it wrote")
			})
		}
	}
}

// TestObservedPowerNamesTheProgressItReplaced pins the message the operator reads: a shutdown that
// did not land and a shuttingdown whose monitor died are different failures, and the note has to say
// which record the machine outlived.
func TestObservedPowerNamesTheProgressItReplaced(t *testing.T) {
	for _, progress := range []infrav1alpha1.Progress{infrav1alpha1.ProgressShutdown, infrav1alpha1.ProgressShuttingDown} {
		rec := events.NewFakeRecorder(8)
		node := observedSlot(infrav1alpha1.PowerStateOff, progress)
		c := newClient(t, node, liveNode())

		reconcileObservedPower(t, c, node, rec)

		recorded := drainEvents(rec)
		require.Len(t, recorded, 1)
		assert.Contains(t, recorded[0], string(progress),
			"the event names the previous progress: %s", recorded[0])
		assert.NotContains(t, recorded[0], "spec.desiredPowerState \"on\"", "the claim is off, and the note says so")
	}
}

// TestObservedPowerIsIdempotent: the progress the rule writes (ready) is outside the set the rule
// reads, so the next pass over the same slot writes nothing and records nothing. No event storm, no
// hot loop: a slot that is at ready with a machine behind it is the normal case.
func TestObservedPowerIsIdempotent(t *testing.T) {
	rec := events.NewFakeRecorder(8)
	node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	c := newClient(t, node, liveNode())

	first := reconcileObservedPower(t, c, node, rec)
	require.Equal(t, ActionCorrectToReady, first.Action)

	written := readSlot(t, c)
	require.Len(t, drainEvents(rec), 1)

	second := reconcileObservedPower(t, c, node, rec)
	assert.Equal(t, ActionNone, second.Action)
	assert.False(t, second.Corrected)
	assert.Contains(t, second.Reason, "does not contradict")

	after := readSlot(t, c)
	assert.Equal(t, written.ResourceVersion, after.ResourceVersion, "the second pass wrote nothing")
	assert.Empty(t, drainEvents(rec), "the second pass recorded nothing")
}

// TestObservedPowerLeavesTheNormalCasesAlone covers the shapes this rule must not touch: the healthy
// pair, the settled pair, and the transitional startup.
func TestObservedPowerLeavesTheNormalCasesAlone(t *testing.T) {
	tests := []struct {
		name     string
		desired  infrav1alpha1.PowerState
		on       infrav1alpha1.PowerState
		progress infrav1alpha1.Progress
		ready    bool
		// want is the substring of the reason the pass reports
		want string
	}{
		{
			name:     "observed on and progress ready: the normal case",
			desired:  infrav1alpha1.PowerStateOn,
			on:       infrav1alpha1.PowerStateOn,
			progress: infrav1alpha1.ProgressReady,
			ready:    true,
			want:     "does not contradict",
		},
		{
			name:     "the stable off / shutdown pair",
			desired:  infrav1alpha1.PowerStateOff,
			on:       infrav1alpha1.PowerStateOff,
			progress: infrav1alpha1.ProgressShutdown,
			ready:    false,
			want:     "powerState is",
		},
		{
			name:     "a startup in flight is the operation's",
			desired:  infrav1alpha1.PowerStateOn,
			on:       infrav1alpha1.PowerStateOn,
			progress: infrav1alpha1.ProgressStartingUp,
			ready:    true,
			want:     "does not contradict",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := events.NewFakeRecorder(8)

			node := observedSlot(tc.desired, tc.progress)
			node.Status.PowerState = tc.on

			kubeNode := liveNode()
			if !tc.ready {
				kubeNode.Status.Conditions[0].Status = corev1.ConditionFalse
			}

			c := newClient(t, node, kubeNode)

			outcome := reconcileObservedPower(t, c, node, rec)
			assert.Equal(t, ActionNone, outcome.Action)
			assert.Contains(t, outcome.Reason, tc.want)
			assert.Equal(t, tc.progress, readSlot(t, c).Status.Progress, "nothing was written")
			assert.Empty(t, drainEvents(rec), "nothing was recorded")
		})
	}
}

// TestObservedPowerRefusesTheShapesAnOperationOwns: a lifecycle Job in flight, a Job that finished
// inside the settle grace, and a live coordination lock all say an operation owns the slot, and its
// state machine is the writer of its progress. The Job gate is the one that matters for this rule:
// the completion of a shutdown Job writes progress: shutdown while the machine's node object is
// Ready for node-monitor-grace-period (~40s), the window in which a "Ready" reading is not evidence
// that the machine stayed up. Correcting there is the duplicate shutdown Job
// core.applyObservedPowerState refuses to write for.
func TestObservedPowerRefusesTheShapesAnOperationOwns(t *testing.T) {
	tests := []struct {
		name string
		// prepare mutates the slot / client for the case
		prepare func(t *testing.T, node *infrav1alpha1.Node, extra []client.Object) []client.Object
		// wantRequeue is > 0 when the pass has to ask for a deadline
		wantRequeue time.Duration
		wantReason  string
	}{
		{
			name: "a shutdown Job in flight",
			prepare: func(t *testing.T, node *infrav1alpha1.Node, extra []client.Object) []client.Object {
				return append(extra, lifecycleJob(fsm.PhaseShutdown, time.Time{}))
			},
			wantRequeue: SettleGracePeriod,
			wantReason:  "is in flight",
		},
		{
			name: "a shutdown Job that finished inside the grace",
			prepare: func(t *testing.T, node *infrav1alpha1.Node, extra []client.Object) []client.Object {
				return append(extra, lifecycleJob(fsm.PhaseShutdown, time.Now().Add(-time.Minute)))
			},
			wantRequeue: SettleGracePeriod - time.Minute,
			wantReason:  "finished inside the settle grace",
		},
		{
			name: "a live coordination lock",
			prepare: func(t *testing.T, node *infrav1alpha1.Node, extra []client.Object) []client.Object {
				node.Annotations = map[string]string{
					fsm.OperationLockAnnotation: fsm.OperationScaleDown,
					fsm.LockOwnerAnnotation:     fsm.NodeControllerOwner,
					fsm.LockTimestampAnnotation: time.Now().Format(time.RFC3339),
					fsm.LockTimeoutAnnotation:   "300",
				}

				return extra
			},
			wantReason: "coordination lock",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := events.NewFakeRecorder(8)

			node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
			extra := []client.Object{liveNode()}
			objects := append(extra, tc.prepare(t, node, nil)...)

			c := newClient(t, append([]client.Object{node}, objects...)...)

			before := readSlot(t, c)
			outcome := reconcileObservedPower(t, c, readSlot(t, c), rec)

			assert.Equal(t, ActionNone, outcome.Action)
			assert.Contains(t, outcome.Reason, tc.wantReason)

			if tc.wantRequeue > 0 {
				assert.InDelta(t, tc.wantRequeue.Seconds(), outcome.RequeueAfter.Seconds(), 5,
					"the look that acts is scheduled, not left to the next resync")
			} else {
				assert.Zero(t, outcome.RequeueAfter)
			}

			after := readSlot(t, c)
			assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress, "the operation's progress is untouched")
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "the gate writes nothing")
			assert.Empty(t, drainEvents(rec), "the gate records nothing")
		})
	}
}

// TestObservedPowerNeedsTheMachineToBeObservedUp: a slot whose node object is gone, or is NotReady,
// is not a machine that is up. The first is the watchdog's shape (settle / re-arm), and NotReady is
// the weak observation core.applyObservedPowerState spells out - a machine that is booting, a machine
// that is going off, and a machine that is up with a dead kubelet all spell it. Requiring the Ready
// node object is what makes the two rules of this package structurally exclusive.
func TestObservedPowerNeedsTheMachineToBeObservedUp(t *testing.T) {
	t.Run("no node object at all", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)
		node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		c := newClient(t, node)

		outcome := reconcileObservedPower(t, c, node, rec)

		assert.Equal(t, ActionNone, outcome.Action)
		assert.Contains(t, outcome.Reason, "no Kubernetes node")
		assert.Empty(t, drainEvents(rec))
		assert.Equal(t, infrav1alpha1.ProgressShutdown, readSlot(t, c).Status.Progress)
	})

	t.Run("node object present and NotReady", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)

		kubeNode := liveNode()
		kubeNode.Status.Conditions[0].Status = corev1.ConditionFalse

		node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		c := newClient(t, node, kubeNode)

		outcome := reconcileObservedPower(t, c, node, rec)

		assert.Equal(t, ActionNone, outcome.Action)
		assert.Contains(t, outcome.Reason, "does not report Ready")
		assert.Empty(t, drainEvents(rec))
	})
}

// TestObservedPowerAndTheWatchdogHaveDistinctJobs pins the split the two rules are documented by:
// the watchdog settles a claim that produced nothing (a progress that names a machine and no machine
// in the cluster), this rule corrects a machine that outlived its recorded state (a progress that
// names no machine and a machine the cluster observes up). A slot that is one of the two shapes is
// acted on by one rule, and a pass of both rules on it is at most one write.
//
// The two shapes are not the whole space, and this test does not claim they are. There is a third
// shape neither rule acts on: {status.progress: shuttingdown, a corev1.Node that is present and
// NotReady, no lifecycle Job, no live lock} - a shutdown whose operation died, on a machine whose node
// object is there and reporting down. Settle refuses it (its gates want a slot with no node object at
// all: a slot with a node belongs to the observation controllers), ReconcileObservedPower refuses it
// (NotReady is not evidence that a machine is up, so nothing outranks the progress), and
// core.applyObservedPowerState - the writer that does own a slot with a node - writes the off and never
// touches a transitional progress. Nothing in this package moves that slot, and it is frozen the way
// the shapes this package exists for are: validateStateTransition gates every transition of a
// transitional slot out, and the stuck-transition cleanup is not reached from a reconcile (a fresh
// state machine per pass, so IsTransitionStuck is false). It is a documented hole, not an oversight:
// settling it means reading "NotReady" as "the machine is off", the reading the observation controller
// refuses to make because it is also every boot window. The pair is stated in internal/progress's
// package doc and in docs/architecture/state.md.
func TestObservedPowerAndTheWatchdogHaveDistinctJobs(t *testing.T) {
	t.Run("the machine the cluster observes is this rule's, not the watchdog's", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)
		node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		c := newClient(t, node, liveNode())

		outcome := reconcileObservedPower(t, c, readSlot(t, c), rec)
		require.Equal(t, ActionCorrectToReady, outcome.Action)

		watched, err := Settle(context.Background(), c, readSlot(t, c), rec, SettleGracePeriod)
		require.NoError(t, err)

		assert.Equal(t, ActionNone, watched.Action, "the watchdog has nothing to settle: the slot has a machine")
		assert.Contains(t, watched.Reason, "belongs to the observation controllers")

		require.Len(t, drainEvents(rec), 1, "one rule acted, so one event")
		assert.Equal(t, infrav1alpha1.ProgressReady, readSlot(t, c).Status.Progress, "one write, the correction")
	})

	t.Run("the machine the cluster does not have is the watchdog's, not this rule's", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)

		// powerState: on is the residue of a machine that was deleted out of band: the last thing
		// recorded about it was that it was up. The FSM's state is shutdown, and the pair is the
		// stable off/shutdown pair - the watchdog's shape, and not evidence of a machine.
		node := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		c := newClient(t, node)

		outcome := reconcileObservedPower(t, c, readSlot(t, c), rec)
		assert.Equal(t, ActionNone, outcome.Action, "no node object, so no observed machine to outrank the progress")

		watched, err := Settle(context.Background(), c, readSlot(t, c), rec, SettleGracePeriod)
		require.NoError(t, err)
		assert.Equal(t, ActionNone, watched.Action, "the settled pair needs no settlement either")

		assert.Empty(t, drainEvents(rec), "neither rule touched the stable pair")
	})

	t.Run("a transitional progress with a Ready machine is this rule's, with a machine gone the watchdog's", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)
		node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
		c := newClient(t, node, liveNode())

		outcome := reconcileObservedPower(t, c, readSlot(t, c), rec)
		require.Equal(t, ActionCorrectToReady, outcome.Action, "a shuttingdown nothing is driving, on a machine that is up")

		stale := metav1.NewTime(time.Now().Add(-SettleGracePeriod - time.Minute))

		machineless := slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShuttingDown)
		machineless.Status.LastShutdownTime = &stale
		c2 := newClient(t, machineless)

		outcome2 := reconcileObservedPower(t, c2, readSlot(t, c2), rec)
		assert.Equal(t, ActionNone, outcome2.Action, "the machine is gone: this rule has nothing to outrank")

		watched, err := Settle(context.Background(), c2, readSlot(t, c2), rec, SettleGracePeriod)
		require.NoError(t, err)
		assert.Equal(t, ActionSettle, watched.Action, "and the watchdog settles it, as its own tests cover")
	})
}

// TestDecideObservedPowerIsTheRule exercises the rule with no I/O, the way the watchdog's decide is
// exercised: every gate, in order, and the action only when all of them are behind the slot.
func TestDecideObservedPowerIsTheRule(t *testing.T) {
	up := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)

	base := observations{nodePresent: true, nodeReady: true, now: time.Now(), grace: SettleGracePeriod}

	t.Run("the shape it exists for", func(t *testing.T) {
		d := decideObservedPower(up, base)
		assert.Equal(t, ActionCorrectToReady, d.action)
		assert.Equal(t, infrav1alpha1.ProgressShutdown, d.previous)
		assert.Zero(t, d.requeueAfter)
		assert.Contains(t, d.reason, "observed on and up")
	})

	t.Run("progress implies a machine", func(t *testing.T) {
		slot := up.DeepCopy()
		slot.Status.Progress = infrav1alpha1.ProgressReady

		d := decideObservedPower(slot, base)
		assert.Equal(t, ActionNone, d.action)
		assert.Contains(t, d.reason, "does not contradict")
	})

	t.Run("powerState is not on", func(t *testing.T) {
		slot := up.DeepCopy()
		slot.Status.PowerState = infrav1alpha1.PowerStateOff

		d := decideObservedPower(slot, base)
		assert.Equal(t, ActionNone, d.action)
		assert.Contains(t, d.reason, "not on")
	})

	t.Run("no node object", func(t *testing.T) {
		d := decideObservedPower(up, observations{nodeReady: true, now: time.Now(), grace: SettleGracePeriod})
		assert.Equal(t, ActionNone, d.action)
		assert.Contains(t, d.reason, "no Kubernetes node")
	})

	t.Run("node object is NotReady", func(t *testing.T) {
		d := decideObservedPower(up, observations{nodePresent: true, nodeReady: false, now: time.Now(), grace: SettleGracePeriod})
		assert.Equal(t, ActionNone, d.action)
		assert.Contains(t, d.reason, "does not report Ready")
	})

	t.Run("a Job in flight, and one that just finished", func(t *testing.T) {
		running := observations{nodePresent: true, nodeReady: true, now: time.Now(), grace: SettleGracePeriod}
		running.jobs = []batchv1.Job{*lifecycleJob(fsm.PhaseShutdown, time.Time{})}

		d := decideObservedPower(up, running)
		assert.Equal(t, ActionNone, d.action)
		assert.Contains(t, d.reason, "is in flight")

		fresh := observations{nodePresent: true, nodeReady: true, now: time.Now(), grace: SettleGracePeriod}
		fresh.jobs = []batchv1.Job{*lifecycleJob(fsm.PhaseShutdown, time.Now().Add(-time.Minute))}

		d = decideObservedPower(up, fresh)
		assert.Equal(t, ActionNone, d.action)
		assert.Contains(t, d.reason, "inside the settle grace")
		assert.Greater(t, d.requeueAfter, time.Duration(0))

		aged := observations{nodePresent: true, nodeReady: true, now: time.Now(), grace: SettleGracePeriod}
		aged.jobs = []batchv1.Job{*lifecycleJob(fsm.PhaseShutdown, time.Now().Add(-SettleGracePeriod-time.Minute))}

		assert.Equal(t, ActionCorrectToReady, decideObservedPower(up, aged).action,
			"a Job older than the grace no longer owns the slot")
	})

	t.Run("a live lock", func(t *testing.T) {
		d := decideObservedPower(up, observations{nodePresent: true, nodeReady: true, lockHeld: true, now: time.Now(), grace: SettleGracePeriod})
		assert.Equal(t, ActionNone, d.action)
		assert.Contains(t, d.reason, "coordination lock")
	})

	t.Run("no Job object: the window is the shutdown stamp", func(t *testing.T) {
		// The reproduced failure, as a decision: the Job the shutdown ran is gone (a short
		// spec.ttlSecondsAfterFinished collected it), the machine went off, and its node object is
		// still Ready for node-monitor-grace-period. The stamp is the only deadline in the evidence.
		young := up.DeepCopy()
		stamp := metav1.NewTime(time.Now().Add(-20 * time.Second))
		young.Status.LastShutdownTime = &stamp

		d := decideObservedPower(young, observations{nodePresent: true, nodeReady: true, now: time.Now(), grace: SettleGracePeriod})
		assert.Equal(t, ActionNone, d.action, "a shutdown younger than the grace owns its slot")
		assert.Contains(t, d.reason, "lastShutdownTime")
		assert.InDelta(t, (SettleGracePeriod - 20*time.Second).Seconds(), d.requeueAfter.Seconds(), 5,
			"and the look is on the rest of the grace")

		aged := up.DeepCopy()
		old := metav1.NewTime(time.Now().Add(-SettleGracePeriod - time.Minute))
		aged.Status.LastShutdownTime = &old

		assert.Equal(t, ActionCorrectToReady, decideObservedPower(aged,
			observations{nodePresent: true, nodeReady: true, now: time.Now(), grace: SettleGracePeriod}).action,
			"a stamp older than the grace is a floor, not a freeze")

		// The Job object, when there is one, stays the evidence: its deadline is the tighter one and
		// the stamp is not consulted.
		withJob := up.DeepCopy()
		youngStamp := metav1.NewTime(time.Now().Add(-SettleGracePeriod - time.Minute))
		withJob.Status.LastShutdownTime = &youngStamp

		obs := observations{
			nodePresent: true, nodeReady: true, now: time.Now(), grace: SettleGracePeriod,
			jobs: []batchv1.Job{*lifecycleJob(fsm.PhaseShutdown, time.Now().Add(-30*time.Second))},
		}

		d = decideObservedPower(withJob, obs)
		assert.Equal(t, ActionNone, d.action)
		assert.Contains(t, d.reason, "finished inside the settle grace", "the Job, not the stamp, is what deferred this slot")
	})

	t.Run("the pace of a slot whose completions keep being refused", func(t *testing.T) {
		// The count is read on the shared observation, the same field the FSM's bound reads.
		obs := observations{
			nodePresent: true, nodeReady: true, now: time.Now(), grace: SettleGracePeriod,
			shutdownAttempts: 2,
		}

		d := decideObservedPower(up, obs)
		assert.Equal(t, ActionCorrectToReady, d.action, "the correction is still the right write")
		assert.Zero(t, d.requeueAfter,
			"the backoff is not stamped on the decision: the correction releases a shutdown now, and the "+
				"delay is set on the outcome, from the count the write read back")
	})
}

// stampShutdownAttempts writes the shutdown-attempt record the FSM owns onto the slot in the client,
// in the wire shape the bound reads (fsm.ReasonShutdownAttempt + the count). This package never writes
// that record, so a test of the pace it sets has to put it there the way the cluster does: an attempt
// counted by the transition into shuttingdown, not by a pass of this rule.
func stampShutdownAttempts(t *testing.T, c client.Client, attempts int) {
	t.Helper()

	current := readSlot(t, c)

	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:    fsm.ConditionShutdownAttempts,
		Status:  metav1.ConditionTrue,
		Reason:  fsm.ReasonShutdownAttempt + fmt.Sprint(attempts),
		Message: fmt.Sprintf("shutdown attempt %d recorded by the FSM", attempts),
	})

	require.NoError(t, c.Status().Update(context.Background(), current))
}

// shutdownStamped puts the slot's status.lastShutdownTime at stamp: the timestamp the transition into
// shuttingdown leaves, in the same status write as progress: shuttingdown. It is the evidence this rule
// measures its grace against when no lifecycle Job object is left to measure against.
func shutdownStamped(t *testing.T, c client.Client, stamp time.Time) {
	t.Helper()

	current := readSlot(t, c)

	stamped := metav1.NewTime(stamp)
	current.Status.LastShutdownTime = &stamped

	require.NoError(t, c.Status().Update(context.Background(), current))
}

// TestObservedPowerLeavesACollectedShutdownJobToItsStamp closes the gap the Job gate leaves, and is the
// reproduced failure. The grace is measured from the lifecycle Job, and a slot whose shutdown Job was
// collected has no Job object to measure: spec.ttlSecondsAfterFinished: 0 removes the object the moment
// the pod finishes, and a short one does it minutes later. With no Job the gate opened immediately, so
// on a machine that *did* go off - whose node object is still Ready for node-monitor-grace-period
// (~40s), whose status.powerState is still the on that observation wrote - the rule corrected the slot
// to ready and the same pass fired a second ShutdownNode. Every scale-down of a pool with a short TTL
// paid for it, in the window the gate exists to cover.
//
// The fix is the watchdog's fallback, mirrored (startupGrace): with no Job object, the same grace is
// measured from status.lastShutdownTime.
func TestObservedPowerLeavesACollectedShutdownJobToItsStamp(t *testing.T) {
	t.Run("a shutdown stamped 20s ago, no Job object: no correction, no event, no second shutdown", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)

		node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		c := newClient(t, node, liveNode())

		shutdownStamped(t, c, time.Now().Add(-20*time.Second))

		before := readSlot(t, c)
		outcome := reconcileObservedPower(t, c, before, rec)

		assert.Equal(t, ActionNone, outcome.Action,
			"a shutdown younger than the grace, with no Job object, is an operation whose writer may still be running")
		assert.False(t, outcome.Corrected)
		assert.Greater(t, outcome.RequeueAfter, time.Duration(0), "and the look is scheduled, not forgotten")
		assert.Less(t, outcome.RequeueAfter, SettleGracePeriod, "on the rest of the grace, not on a fresh one")
		assert.Contains(t, outcome.Reason, "lastShutdownTime", "the log says what the window was measured from")

		after := readSlot(t, c)
		assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress, "nothing was written")
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "and the slot was not touched")
		assert.Empty(t, drainEvents(rec), "and nothing was recorded: no MachineObservedUp, so no second shutdown")
	})

	t.Run("the shape a ttlSecondsAfterFinished of 0 leaves is the shape the stamp covers", func(t *testing.T) {
		// The FSM stamps spec.ttlSecondsAfterFinished on the shutdown Job it creates
		// (fsm.createShutdownJob), and the internal/fsm test that pins a 0 TTL pins the shape this
		// subtest reads: the Job completed 20s ago, the Job object is gone, the node object is still
		// Ready. That is the whole of the evidence, and the stamp is the only deadline in it.
		for _, ttl := range []int32{0, 1, 60} {
			t.Run(fmt.Sprintf("ttlSecondsAfterFinished %d", ttl), func(t *testing.T) {
				rec := events.NewFakeRecorder(8)

				node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
				node.Spec.ShutdownPodSpec.TTLSecondsAfterFinished = &ttl

				c := newClient(t, node, liveNode())
				shutdownStamped(t, c, time.Now().Add(-20*time.Second))

				outcome := reconcileObservedPower(t, c, readSlot(t, c), rec)

				assert.Equal(t, ActionNone, outcome.Action, "the collected Job is not evidence that the operation is over")
				assert.Empty(t, drainEvents(rec))
			})
		}
	})

	t.Run("a stamp older than the grace is the shape this rule acts on", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)

		node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		c := newClient(t, node, liveNode())

		shutdownStamped(t, c, time.Now().Add(-SettleGracePeriod-time.Minute))

		outcome := reconcileObservedPower(t, c, readSlot(t, c), rec)

		assert.Equal(t, ActionCorrectToReady, outcome.Action,
			"a shutdown that ran out of its grace, on a machine observed up, is the live failure: the stamp is a floor, not a freeze")
		require.Len(t, drainEvents(rec), 1)
	})

	t.Run("a slot with no shutdown stamp is not held by a deadline it has no evidence for", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)

		node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		c := newClient(t, node, liveNode())

		outcome := reconcileObservedPower(t, c, readSlot(t, c), rec)

		assert.Equal(t, ActionCorrectToReady, outcome.Action,
			"the 40-minute slot had a progress: shutdown and no shutdown operation behind it: that is the shape, and no stamp to wait out")
		assert.Zero(t, outcome.RequeueAfter)
	})

	t.Run("the Job object stays the evidence when there is one", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)

		node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		c := newClient(t, node, liveNode(), lifecycleJob(fsm.PhaseShutdown, time.Now().Add(-30*time.Second)))

		// The stamp is older than the grace; the Job finished 30s ago. The tightest deadline wins, and
		// the Job is the one that says a writer is still running.
		shutdownStamped(t, c, time.Now().Add(-SettleGracePeriod-time.Minute))

		outcome := reconcileObservedPower(t, c, readSlot(t, c), rec)

		assert.Equal(t, ActionNone, outcome.Action)
		assert.Contains(t, outcome.Reason, "finished inside the settle grace")
		assert.Greater(t, outcome.RequeueAfter, time.Duration(0))
		assert.Less(t, outcome.RequeueAfter, SettleGracePeriod)
	})
}

// TestObservedPowerPacesARefusedShutdownLoop is the backoff on the loop this rule's correction
// releases. A slot whose shutdown script exits 0 without the provider's power-off landing cycles:
// shutdown Job, refused completion (fsm.refuseUnobservedShutdown), correction to ready, ShutdownNode,
// Job - and every pass re-cordons a node that is serving pods. The bound is the FSM's
// (fsm.ShutdownAttemptLimit stops the firing), and the pace is this rule's: a slot with refused
// completions is looked at again on an exponentially growing deadline, so the loop slows down as it
// repeats, capped, and a slot that has never had a completion refused is not delayed at all.
func TestObservedPowerPacesARefusedShutdownLoop(t *testing.T) {
	tests := []struct {
		name string
		// attempts is the shutdown-attempt record the slot carries
		attempts int
		want     time.Duration
	}{
		{name: "no refused completion: no delay, an unwanted machine is shut down now", attempts: 0, want: 0},
		{name: "one attempt spent", attempts: 1, want: ObservedPowerRetryBase},
		{name: "two attempts spent: doubled", attempts: 2, want: 2 * ObservedPowerRetryBase},
		{name: "three attempts spent: doubled again", attempts: 3, want: 4 * ObservedPowerRetryBase},
		{name: "a count past the bound is capped, and the slot stays in the loop", attempts: 12, want: ObservedPowerRetryCap},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := events.NewFakeRecorder(8)

			node := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
			c := newClient(t, node, liveNode())

			if tc.attempts > 0 {
				stampShutdownAttempts(t, c, tc.attempts)
			}

			outcome := reconcileObservedPower(t, c, readSlot(t, c), rec)

			require.Equal(t, ActionCorrectToReady, outcome.Action, "the correction is still the right write")
			assert.Equal(t, tc.attempts, outcome.ShutdownAttempts, "the pass reports the count it paced on")
			assert.Equal(t, tc.want, outcome.RequeueAfter, "the next look is on the backoff of this slot")

			recorded := drainEvents(rec)
			require.Len(t, recorded, 1)

			if tc.attempts > 0 {
				assert.Contains(t, recorded[0], fmt.Sprintf("%d of %d shutdown attempts", tc.attempts, fsm.ShutdownAttemptLimit),
					"the event says how many machines this pool has already tried to power off: %s", recorded[0])
			} else {
				assert.NotContains(t, recorded[0], "shutdown attempts", "a first correction is a healed record, not a loop")
			}
		})
	}
}

// TestObservedPowerRetryBackoffIsMonotonicAndCapped pins the arithmetic of the backoff on its own: the
// delay never shrinks as the attempts grow, never exceeds the cap, and is 0 only for a slot that has
// never had a completion refused.
func TestObservedPowerRetryBackoffIsMonotonicAndCapped(t *testing.T) {
	assert.Zero(t, observedPowerRetryBackoff(0), "no refused completion, no reason to wait")
	assert.Zero(t, observedPowerRetryBackoff(-1), "a count that cannot be negative is not a delay")

	previous := time.Duration(0)

	for attempts := 1; attempts <= 20; attempts++ {
		backoff := observedPowerRetryBackoff(attempts)

		assert.Greater(t, backoff, time.Duration(0))
		assert.LessOrEqual(t, backoff, ObservedPowerRetryCap, "attempts %d", attempts)
		assert.GreaterOrEqual(t, backoff, previous, "the backoff never shrinks as the attempts grow (attempts %d)", attempts)

		previous = backoff
	}

	assert.Equal(t, ObservedPowerRetryCap, previous, "a loop that will not end is on a fixed look, not invisible")
}

// TestObservedPowerSkipsTheObservationForAPoweredOffSlot is the cheap gate in front of the reads. A pool
// at rest is slots whose status.powerState is off - every machine that is genuinely down - and reading
// their corev1.Node and listing the namespace's lifecycle Jobs for each of them, on every reconcile,
// to conclude "powerState is off" is a Get and a List per slot per pass. The gate is checked on the
// caller's copy, before observe(), and the write path keeps re-deriving the same check from the copy it
// had to read anyway.
func TestObservedPowerSkipsTheObservationForAPoweredOffSlot(t *testing.T) {
	// A client whose namespace-wide Job List always fails: if the rule pays for that List, it returns
	// an error; if it decides on the slot's own fields first, it returns the reason.
	reads := 0

	// The steady state of most slots in a pool: a machine that is genuinely off.
	down := observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	down.Status.PowerState = infrav1alpha1.PowerStateOff

	c := newClientWithInterceptor(t,
		interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*batchv1.JobList); ok {
					reads++

					return fmt.Errorf("injected: cannot list lifecycle Jobs")
				}

				return cl.List(ctx, list, opts...)
			},
		},
		down, liveNode())

	outcome, err := ReconcileObservedPower(context.Background(), c, readSlot(t, c), nil, SettleGracePeriod)
	require.NoError(t, err, "the pass decided on the fields the caller already holds, and read nothing")
	assert.Equal(t, ActionNone, outcome.Action)
	assert.Contains(t, outcome.Reason, "powerState is")
	assert.Zero(t, reads, "no lifecycle Job List, and no corev1.Node Get, was paid for a slot that is off")

	// The same client, the same slot with the observed on: the reads are paid for, and their failure is
	// reported. Only the order of the gates changed; the behaviour did not.
	up := readSlot(t, c)
	up.Status.PowerState = infrav1alpha1.PowerStateOn

	result, err := ReconcileObservedPower(context.Background(), c, up, nil, SettleGracePeriod)
	require.Error(t, err, "an observed on needs the observation, and a failed observation is an error")
	assert.Equal(t, ActionNone, result.Action)
	assert.Greater(t, reads, 0, "and the read was attempted, so the gate above it is a short-circuit and not a skip")
}

// TestObservedPowerWritesNothingWhenTheObservationFails is the rule both rules of this package write
// under: a correction, a settlement, a re-arm and an uncordon are all decided from one shared read, so
// a partially-failed observation - a Job List the API server refused, a node object this pass could not
// get - is never evidence to write over a slot. The slot keeps the progress it had, and the caller
// reports the error and comes back.
func TestObservedPowerWritesNothingWhenTheObservationFails(t *testing.T) {
	t.Run("the correction", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)

		c := newClientWithInterceptor(t,
			interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*batchv1.JobList); ok {
						return fmt.Errorf("injected: cannot list lifecycle Jobs")
					}

					return cl.List(ctx, list, opts...)
				},
			},
			observedSlot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown), liveNode())

		before := readSlot(t, c)

		outcome, err := ReconcileObservedPower(context.Background(), c, before, rec, SettleGracePeriod)
		require.Error(t, err)
		assert.Equal(t, ActionNone, outcome.Action, "nothing was decided, so nothing is reported as done")
		assert.Zero(t, outcome.RequeueAfter, "a failed read asks for no deadline: the controller's own backoff is the schedule")

		after := readSlot(t, c)
		assert.Equal(t, infrav1alpha1.ProgressShutdown, after.Status.Progress, "the slot kept the progress it had")
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "and was not written")
		assert.Empty(t, drainEvents(rec), "and nothing was recorded")
	})

	t.Run("the settlement", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)

		c := newClientWithInterceptor(t,
			interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Node); ok {
						return fmt.Errorf("injected: cannot read the Kubernetes node")
					}

					return cl.Get(ctx, key, obj, opts...)
				},
			},
			slot(infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressReady))

		before := readSlot(t, c)

		outcome, err := Settle(context.Background(), c, before, rec, SettleGracePeriod)
		require.Error(t, err, "the shared read failed, so the settlement is reported, not guessed")
		assert.Equal(t, ActionNone, outcome.Action)
		assert.False(t, outcome.Settled)

		after := readSlot(t, c)
		assert.Equal(t, infrav1alpha1.ProgressReady, after.Status.Progress,
			"a settlement is never written from a partially-failed observation: not knowing the machine is not evidence that it is gone")
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "and the slot was not written")
		assert.Empty(t, drainEvents(rec))
	})
}
