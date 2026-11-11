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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
// names no machine and a machine the cluster observes up). One slot is one of the two shapes, and a
// pass of both rules on that slot is at most one write.
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
}
