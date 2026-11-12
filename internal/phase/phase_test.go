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

package phase

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// claim is the fixture's live claim of the slot.
func claim(phase infrav2alpha1.NodeClaimPhase, marked bool) *infrav2alpha1.NodeClaim {
	nodeClaim := &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-worker-01", Namespace: "homelab-autoscaler-system"},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef: corev1.ObjectReference{Name: "worker-01"},
			Request: infrav2alpha1.ClaimRequest{Call: infrav2alpha1.ClaimCallIncreaseSize, Group: "workers"},
		},
		Status: infrav2alpha1.NodeClaimStatus{Phase: phase, Attempt: 1},
	}

	if marked {
		now := metav1.Now()
		nodeClaim.DeletionTimestamp = &now
	}

	return nodeClaim
}

// TestDeriveIsThePhaseStateTable is the table of docs/development/state.md, asserted on the one
// function that computes the report. Every row is (live claim, observed power, node usability) in,
// a reported phase out; no row reads a record.
func TestDeriveIsThePhaseStateTable(t *testing.T) {
	tests := []struct {
		name  string
		input Input
		want  infrav2alpha1.NodePhase
	}{
		{"no claim, machine off", Input{ObservedPower: infrav2alpha1.PowerStateOff}, infrav2alpha1.NodePhaseOff},
		{"no claim, no observation yet", Input{}, infrav2alpha1.NodePhaseOff},
		{"no claim, machine observed up is unwanted, not idle", Input{ObservedPower: infrav2alpha1.PowerStateOn}, infrav2alpha1.NodePhaseDraining},
		{"pending claim, machine off", Input{Claim: claim(infrav2alpha1.NodeClaimPending, false), ObservedPower: infrav2alpha1.PowerStateOff}, infrav2alpha1.NodePhasePending},
		// M1: the status subresource strips `status` from a create, and status.phase carries no CRD
		// default, so a brand-new claim has no phase. It is the live claim of its slot, in the
		// pending shape, from its create.
		{"claim with no phase is live and pending", Input{Claim: claim("", false), ObservedPower: infrav2alpha1.PowerStateOff}, infrav2alpha1.NodePhasePending},
		{"pending claim with a startup Job is booting", Input{Claim: claim(infrav2alpha1.NodeClaimPending, false), ObservedPower: infrav2alpha1.PowerStateOff, StartupJobInFlight: true}, infrav2alpha1.NodePhaseBooting},
		{"pending claim, machine already up", Input{Claim: claim(infrav2alpha1.NodeClaimPending, false), ObservedPower: infrav2alpha1.PowerStateOn}, infrav2alpha1.NodePhaseBooting},
		{"booting claim", Input{Claim: claim(infrav2alpha1.NodeClaimBooting, false), ObservedPower: infrav2alpha1.PowerStateOff, StartupJobInFlight: true}, infrav2alpha1.NodePhaseBooting},
		{"booting claim, machine up, node not usable", Input{Claim: claim(infrav2alpha1.NodeClaimBooting, false), ObservedPower: infrav2alpha1.PowerStateOn}, infrav2alpha1.NodePhaseBooting},
		{"ready claim, machine up, node usable", Input{Claim: claim(infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true}, infrav2alpha1.NodePhaseReady},
		{"ready claim, machine up, node not usable is still booting", Input{Claim: claim(infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: false}, infrav2alpha1.NodePhaseBooting},
		{"ready claim whose machine vanished is booting, not ready", Input{Claim: claim(infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOff}, infrav2alpha1.NodePhaseBooting},
		{"failed claim, machine up", Input{Claim: claim(infrav2alpha1.NodeClaimFailed, false), ObservedPower: infrav2alpha1.PowerStateOn}, infrav2alpha1.NodePhaseDraining},
		{"failed claim, machine off", Input{Claim: claim(infrav2alpha1.NodeClaimFailed, false), ObservedPower: infrav2alpha1.PowerStateOff}, infrav2alpha1.NodePhaseOff},
		{"completed claim is not capacity", Input{Claim: claim(infrav2alpha1.NodeClaimCompleted, false), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true}, infrav2alpha1.NodePhaseDraining},
		{"a marked claim with the machine up drains", Input{Claim: claim(infrav2alpha1.NodeClaimReady, true), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true}, infrav2alpha1.NodePhaseDraining},
		{"a marked claim with the machine off is reported off", Input{Claim: claim(infrav2alpha1.NodeClaimBooting, true), ObservedPower: infrav2alpha1.PowerStateOff}, infrav2alpha1.NodePhaseOff},
		{"a shutdown Job in flight on an observed-up machine is draining", Input{Claim: claim(infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true, ShutdownJobInFlight: true}, infrav2alpha1.NodePhaseDraining},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Derive(tc.input))
		})
	}
}

// TestStateIsTheInputLawOfTheMachine asserts the state a pass starts in, including the one place
// where the report and the state are not the same word: the unwanted machine reports draining and
// starts in ready, because ShutdownNode is reachable from ready.
func TestStateIsTheInputLawOfTheMachine(t *testing.T) {
	tests := []struct {
		name  string
		input Input
		want  string
	}{
		{"free slot", Input{ObservedPower: infrav2alpha1.PowerStateOff}, StateShutdown},
		{"boot pending fires StartNode, so it starts in shutdown", Input{Claim: claim(infrav2alpha1.NodeClaimPending, false), ObservedPower: infrav2alpha1.PowerStateOff}, StateShutdown},
		{"booting is the transitional state", Input{Claim: claim(infrav2alpha1.NodeClaimBooting, false), ObservedPower: infrav2alpha1.PowerStateOff, StartupJobInFlight: true}, StateStartingUp},
		{"ready claim, usable node", Input{Claim: claim(infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true}, StateReady},
		{"ready claim, gate not satisfied", Input{Claim: claim(infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: false}, StateStartingUp},
		{"unwanted machine: the report says draining, the state is ready", Input{ObservedPower: infrav2alpha1.PowerStateOn}, StateReady},
		{"shutdown Job in flight owns the slot", Input{ObservedPower: infrav2alpha1.PowerStateOn, ShutdownJobInFlight: true}, StateShuttingDown},
		{"terminal claim with the machine up is the force-shutdown row", Input{Claim: claim(infrav2alpha1.NodeClaimFailed, false), ObservedPower: infrav2alpha1.PowerStateOn}, StateReady},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, State(tc.input))
		})
	}
}

// TestLiveClaimIsThePhaseQuestion separates "live" (the phase) from "wanted" (not marked): the
// input reports its claim as nil exactly when the phase is terminal, and keeps a marked claim, so
// the report can say draining and the shutdown has an owner.
func TestLiveClaimIsThePhaseQuestion(t *testing.T) {
	assert.Nil(t, Input{}.LiveClaim())
	assert.Nil(t, Input{Claim: claim(infrav2alpha1.NodeClaimFailed, false)}.LiveClaim())
	assert.Nil(t, Input{Claim: claim(infrav2alpha1.NodeClaimCompleted, false)}.LiveClaim())
	assert.NotNil(t, Input{Claim: claim(infrav2alpha1.NodeClaimPending, false)}.LiveClaim())
	assert.NotNil(t, Input{Claim: claim(infrav2alpha1.NodeClaimReady, true)}.LiveClaim())

	assert.True(t, IsLive(infrav2alpha1.NodeClaimBooting))
	assert.False(t, IsLive(infrav2alpha1.NodeClaimFailed))
	assert.True(t, IsTerminal(infrav2alpha1.NodeClaimCompleted))

	assert.True(t, Input{Claim: claim(infrav2alpha1.NodeClaimReady, true)}.MarkedForScaleDown())
	assert.False(t, Input{Claim: claim(infrav2alpha1.NodeClaimReady, false)}.MarkedForScaleDown())
	assert.False(t, Input{Claim: claim(infrav2alpha1.NodeClaimFailed, true)}.MarkedForScaleDown(), "a terminal claim is not live, so it marks nothing")

	assert.False(t, Input{}.ObservedOn())
	assert.False(t, Input{ObservedPower: infrav2alpha1.PowerStateOff}.ObservedOn(), "an empty observation is treated as off, never as up")
	assert.True(t, Input{ObservedPower: infrav2alpha1.PowerStateOn}.ObservedOn())
}

// TestUnusableIsTheReadinessGate is the one predicate the gate, the claim's ready phase and the
// derived report share. The reason string is what an operator reads, so it names the cause.
// TestObservedPowerIsTheKubeletsHeartbeat pins the one definition of the observation: the Ready
// condition of the node object. The observation path writes status.powerState from it, the report is
// derived over it, and the state machine's writer of the report reads the machine through the same
// function - so "the machine is up" cannot mean one thing to the writer of the record and another to
// the writer of the report. That divergence is the ambiguity the 0.1.22 observed-power precedence
// rule was a patch over, and the law internal/phase states is that the record is not an input.
func TestObservedPowerIsTheKubeletsHeartbeat(t *testing.T) {
	ready := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-01"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		}}},
	}

	assert.Equal(t, infrav2alpha1.PowerStateOn, ObservedPower(ready), "a kubelet with a live heartbeat is an up machine")

	assert.Equal(t, infrav2alpha1.PowerStateOff, ObservedPower(nil),
		"no node object is no kubelet: the machine is not there")

	notReady := ready.DeepCopy()
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	assert.Equal(t, infrav2alpha1.PowerStateOff, ObservedPower(notReady),
		"a node that reports NotReady is off: the object is what a powered-off machine leaves behind")

	unknown := ready.DeepCopy()
	unknown.Status.Conditions[0].Status = corev1.ConditionUnknown
	assert.Equal(t, infrav2alpha1.PowerStateOff, ObservedPower(unknown),
		"a silent kubelet is declared Unknown by the node lifecycle controller, and that is the off")

	noCondition := ready.DeepCopy()
	noCondition.Status.Conditions = nil
	assert.Equal(t, infrav2alpha1.PowerStateOff, ObservedPower(noCondition),
		"a node that never reported Ready has no beating kubelet")

	assert.True(t, Input{ObservedPower: ObservedPower(ready)}.ObservedOn(),
		"the report is derived over the answer this definition gives, not over a copy of it")
	assert.False(t, Input{ObservedPower: ObservedPower(unknown)}.ObservedOn(),
		"and a silent kubelet is off for the report, as it is for the record")
}

func TestUnusableIsTheReadinessGate(t *testing.T) {
	ready := func() *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-01"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}}},
		}
	}

	keys := []string{"node.cilium.io/agent-not-ready"}

	assert.Empty(t, Unusable(ready(), keys), "Ready, gate-clear, uncordoned: the machine can take the pool's work")
	assert.Contains(t, Unusable(nil, keys), "does not exist")

	notReady := ready()
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	assert.Contains(t, Unusable(notReady, keys), "not Ready")

	noCondition := ready()
	noCondition.Status.Conditions = nil
	assert.Contains(t, Unusable(noCondition, keys), "not Ready", "a node that never reported Ready is not Ready")

	tainted := ready()
	tainted.Spec.Taints = []corev1.Taint{{Key: "node.cilium.io/agent-not-ready", Effect: corev1.TaintEffectNoExecute}}
	assert.Contains(t, Unusable(tainted, keys), "node.cilium.io/agent-not-ready")
	assert.Empty(t, Unusable(tainted, nil), "an empty key list gates on Ready alone")

	cordoned := ready()
	cordoned.Spec.Unschedulable = true
	assert.Contains(t, Unusable(cordoned, keys), "cordoned")
}

// TestServingSlotIsTheServingShape is the diagnosis of the k3d e2e failure written as a table: the
// state the setup spec puts its machine in - a live claim stamped ready, the machine observed on, a
// corev1.Node whose kubelet is beating, Ready, uncordoned and carrying only the pool's own role taint
// - derives `ready`, and nothing else in the shape moves the answer.
//
// The rows are the suspects the failure had to be read against. Each is the serving triple with one
// field added that a controller *could* have read (the digest of a past episode, a condition on the
// claim, a boot deadline that has run out, an in-flight startup Job of the boot that landed) and the
// answer stays `ready`: the report has exactly three inputs, and a slot whose report says `booting`
// in this shape is a writer that handed in a stale triple, not a formula that disagrees (§6, and the
// table in TestDeriveIsThePhaseStateTable).
func TestServingSlotIsTheServingShape(t *testing.T) {
	now := metav1.Now()

	// servingNode is a machine a kubelet is running: Ready with a current heartbeat, uncordoned, and
	// carrying the pool's role taint, which is not one of the readiness keys the Group declares.
	servingNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "k3d-homelab-autoscaler-agent-0"},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}},
		},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
		}}},
	}

	// The triple of the spec, with the readiness question asked the way the controllers ask it.
	serving := Input{
		Claim:         claim(infrav2alpha1.NodeClaimReady, false),
		ObservedPower: infrav2alpha1.PowerStateOn,
		NodeUsable:    Unusable(servingNode, nil) == "",
	}

	assert.True(t, serving.NodeUsable, "the role taint of the pool is not a readiness taint, and a Ready, uncordoned node is usable")
	assert.Equal(t, infrav2alpha1.NodePhaseReady, Derive(serving), "a live ready claim, a machine observed on and a usable node is ready")
	assert.Equal(t, StateReady, State(serving), "and the state machine starts in ready, where ShutdownNode is reachable and StartNode is not")

	// The suspects, each added to the same triple.
	t.Run("a boot deadline in the past says nothing about a machine that is serving", func(t *testing.T) {
		overdue := serving
		overdue.Claim = claim(infrav2alpha1.NodeClaimReady, false)
		stamped := metav1.NewTime(time.Now().Add(-3 * time.Hour))
		overdue.Claim.Spec.DeadlineAt = &stamped

		assert.Equal(t, infrav2alpha1.NodePhaseReady, Derive(overdue))
		assert.False(t, InBootWindow(overdue.Claim.Status.Phase), "the deadline bounded the boot, and the boot is over")
	})

	t.Run("the record of an episode is not an input", func(t *testing.T) {
		withDigest := serving
		withDigest.Claim = claim(infrav2alpha1.NodeClaimReady, false)
		withDigest.Claim.Status.Conditions = []metav1.Condition{{
			Type:               infrav2alpha1.ClaimConditionMachineObservedOff,
			Status:             metav1.ConditionTrue,
			Reason:             infrav2alpha1.ClaimReasonMachineGone,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
		}}

		assert.Equal(t, infrav2alpha1.NodePhaseReady, Derive(withDigest),
			"a condition is the record of an observation, and the observation is the input")
	})

	t.Run("the startup Job of the boot that landed is not a boot in flight", func(t *testing.T) {
		// The report of a serving slot with its consumed startup Job still counted would be booting, so
		// the answer is not the presence of a Job on the slot: it is a Job of this operation, in flight.
		jobFinished := serving

		assert.Equal(t, infrav2alpha1.NodePhaseReady, Derive(jobFinished))
	})

	// The one suspect that does move the answer, kept honest: a claim a scale-down has named is not a
	// serving slot, and a machine nobody observed on is not a machine that is up.
	assert.Equal(t, infrav2alpha1.NodePhaseDraining, Derive(Input{
		Claim:         claim(infrav2alpha1.NodeClaimReady, true),
		ObservedPower: serving.ObservedPower,
		NodeUsable:    serving.NodeUsable,
	}))

	for _, observed := range []infrav2alpha1.PowerState{infrav2alpha1.PowerStateOff, ""} {
		assert.Equal(t, infrav2alpha1.NodePhaseBooting, Derive(Input{
			Claim:         claim(infrav2alpha1.NodeClaimReady, false),
			ObservedPower: observed,
			NodeUsable:    true,
		}), "a ready claim whose machine is not observed on is a machine being bought again, not a serving one")
	}
}

// TestIsLiveIsTheWantsMachineReading is the law's predicate, asserted on the phase set: the three
// live phases are capacity, the two terminal ones are not, and the empty phase a created claim
// carries before the first status write is pending (M1). A reader that calls an empty phase terminal
// - and IsTerminal said exactly that, before the reading moved here - makes a brand-new request
// invisible to TargetSize, IsFree, ClaimedSlots and the webhook, so a second IncreaseSize can claim
// the same slot and two requests buy one machine.
func TestIsLiveIsTheWantsMachineReading(t *testing.T) {
	tests := []struct {
		phase       infrav2alpha1.NodeClaimPhase
		wantLive    bool
		wantPending bool
		wantWindow  bool
	}{
		{phase: "", wantLive: true, wantPending: true, wantWindow: true},
		{phase: infrav2alpha1.NodeClaimPending, wantLive: true, wantPending: true, wantWindow: true},
		{phase: infrav2alpha1.NodeClaimBooting, wantLive: true, wantPending: false, wantWindow: true},
		{phase: infrav2alpha1.NodeClaimReady, wantLive: true, wantPending: false, wantWindow: false},
		{phase: infrav2alpha1.NodeClaimFailed, wantLive: false, wantPending: false, wantWindow: false},
		{phase: infrav2alpha1.NodeClaimCompleted, wantLive: false, wantPending: false, wantWindow: false},
	}

	for _, tt := range tests {
		name := string(tt.phase)
		if name == "" {
			name = "(no phase yet)"
		}

		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.wantLive, IsLive(tt.phase), "IsLive is the machine-is-wanted reading")
			assert.Equal(t, !tt.wantLive, IsTerminal(tt.phase), "IsTerminal is its complement, not a third answer")
			assert.Equal(t, tt.wantPending, IsPending(tt.phase), "IsPending is the cancelable reading")
			assert.Equal(t, tt.wantWindow, InBootWindow(tt.phase),
				"InBootWindow is the range spec.deadlineAt and the attempt bound apply to: ready ends it (B1)")
		})
	}
}

// TestLiveClaimReturnsAMarkedClaim keeps the comment on Input.LiveClaim honest: the mark a
// scale-down leaves is not the phase question. The marked claim owns its slot, and MarkedForScaleDown
// is this predicate conjoined with the deletion timestamp; WantsMachine (internal/claim) is the
// stronger question a boot must not answer to it.
func TestLiveClaimReturnsAMarkedClaim(t *testing.T) {
	marked := claim(infrav2alpha1.NodeClaimReady, true)

	in := Input{Claim: marked, ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true}

	assert.NotNil(t, in.LiveClaim(), "a claim a scale-down marked is still the claim of the slot")
	assert.True(t, in.MarkedForScaleDown(), "and it is the request that reverses the machine")
	assert.Equal(t, infrav2alpha1.NodePhaseDraining, Derive(in), "the slot reports the drain it is in")

	assert.Nil(t, Input{Claim: claim(infrav2alpha1.NodeClaimFailed, false)}.LiveClaim(),
		"a terminal claim is not capacity, and stops being capacity the moment it is recorded")
	assert.Nil(t, Input{Claim: nil}.LiveClaim(), "no claim, no live claim")
}
