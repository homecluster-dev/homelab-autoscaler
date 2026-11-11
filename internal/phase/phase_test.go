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

// TestDeriveIsThePhaseStateTable is the table of docs/architecture/state.md, asserted on the one
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
