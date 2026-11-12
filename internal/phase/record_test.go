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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// t0 is the clock every record test injects. Record takes `at` for exactly this: a 24 h window
// cannot be tested against time.Now(), and the staleness of the count is the interesting case.
var t0 = time.Date(2025, 11, 12, 6, 0, 0, 0, time.UTC)

// slot is a slot with a report, in the shape the API server would hand back: free, powered off,
// and no history yet, which is the state every record test starts from.
func slot() *infrav2alpha1.Node {
	return &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-worker-1", Namespace: "homelab-autoscaler-system"},
		Status:     infrav2alpha1.NodeStatus{Phase: infrav2alpha1.NodePhaseOff, PowerState: infrav2alpha1.PowerStateOff},
	}
}

// TestRecordDatesAChangeAndNotAResync is the field an operator reads as "Since": it dates the
// change of status.phase, so a pass that records nothing must not move it, and a pass that records
// one must.
func TestRecordDatesAChangeAndNotAResync(t *testing.T) {
	node := slot()

	require.True(t, Record(node, infrav2alpha1.NodePhaseBooting, t0, "claim c1 pending, observed off"))

	require.NotNil(t, node.Status.LastTransitionTime)
	assert.True(t, node.Status.LastTransitionTime.Time.Equal(t0), "the record quotes the clock it was handed")
	assert.Equal(t, infrav2alpha1.NodePhaseOff, node.Status.PreviousPhase, "the phase it came from")
	assert.Equal(t, infrav2alpha1.NodePhaseBooting, node.Status.Phase)
	require.Len(t, node.Status.RecentTransitions, 1)
	assert.Equal(t, infrav2alpha1.NodePhaseOff, node.Status.RecentTransitions[0].From)
	assert.Equal(t, infrav2alpha1.NodePhaseBooting, node.Status.RecentTransitions[0].To)
	assert.Equal(t, "claim c1 pending, observed off", node.Status.RecentTransitions[0].Reason)

	// A resync of the same report: 40 more passes, and nothing about the record moves.
	for range 40 {
		assert.False(t, Record(node, infrav2alpha1.NodePhaseBooting, t0.Add(time.Minute), "same inputs"))
	}

	assert.True(t, node.Status.LastTransitionTime.Time.Equal(t0), "a resync does not date a change that did not happen")
	assert.Len(t, node.Status.RecentTransitions, 1, "a phase is recorded once, not once per pass")
	assert.Equal(t, int32(1), node.Status.ActivationsTotal, "and one boot, not forty")
}

// TestRecordCountsBootsOnceEach is the whole point of activationsTotal: a machine that goes
// off -> booting -> ready -> draining -> off -> booting has been woken up twice, whatever the
// resyncs in between did.
func TestRecordCountsBootsOnceEach(t *testing.T) {
	node := slot()

	episodes := []infrav2alpha1.NodePhase{
		infrav2alpha1.NodePhaseBooting, infrav2alpha1.NodePhaseReady, infrav2alpha1.NodePhaseDraining,
		infrav2alpha1.NodePhaseOff, infrav2alpha1.NodePhaseBooting, infrav2alpha1.NodePhaseReady,
	}

	at := t0
	for _, to := range episodes {
		Record(node, to, at, "episode")

		at = at.Add(time.Minute)
	}

	assert.Equal(t, int32(2), node.Status.ActivationsTotal, "two entries into booting, two activations")
	assert.Equal(t, int32(2), node.Status.ActivationsLast24H, "both inside the window")
	assert.Len(t, node.Status.RecentTransitions, 6)
	assert.Equal(t, infrav2alpha1.NodePhaseBooting, node.Status.PreviousPhase)
}

// TestIsActivationIsTheReportNotTheTransition: entering `booting` is the machine being turned on,
// from any phase, including the `ready -> booting` of a machine that vanished under a live claim
// and a `draining -> booting` of a cancelled scale-down. It is counted once per entry, and the
// resyncs inside the boot window count for nothing.
func TestIsActivationIsTheReportNotTheTransition(t *testing.T) {
	assert.True(t, IsActivation(infrav2alpha1.NodePhaseOff, infrav2alpha1.NodePhaseBooting))
	assert.True(t, IsActivation(infrav2alpha1.NodePhasePending, infrav2alpha1.NodePhaseBooting))
	assert.True(t, IsActivation(infrav2alpha1.NodePhaseReady, infrav2alpha1.NodePhaseBooting))
	assert.True(t, IsActivation(infrav2alpha1.NodePhaseDraining, infrav2alpha1.NodePhaseBooting))
	assert.True(t, IsActivation("", infrav2alpha1.NodePhaseBooting))

	assert.False(t, IsActivation(infrav2alpha1.NodePhaseBooting, infrav2alpha1.NodePhaseBooting))
	assert.False(t, IsActivation(infrav2alpha1.NodePhaseBooting, infrav2alpha1.NodePhaseReady))
	assert.False(t, IsActivation(infrav2alpha1.NodePhaseReady, infrav2alpha1.NodePhaseDraining))
	assert.False(t, IsActivation(infrav2alpha1.NodePhaseOff, infrav2alpha1.NodePhasePending))
}

// TestRingIsBounded: 40 changes of a churning slot leave the last 32, newest last. The bound is
// what keeps a day of a pool in etcd.
func TestRingIsBounded(t *testing.T) {
	node := slot()

	at := t0

	for range 40 {
		Record(node, infrav2alpha1.NodePhaseBooting, at, "boot")
		at = at.Add(time.Minute)

		Record(node, infrav2alpha1.NodePhaseOff, at, "off")
		at = at.Add(time.Minute)
	}

	require.Len(t, node.Status.RecentTransitions, TransitionRingCap)
	assert.Equal(t, infrav2alpha1.NodePhaseOff, node.Status.RecentTransitions[TransitionRingCap-1].To)
	assert.True(t, node.Status.RecentTransitions[TransitionRingCap-1].At.Time.Equal(at.Add(-time.Minute)))

	// The counter is a lifetime counter and is not bounded by the ring: all 40 boots are counted
	// in the total, and only the ones the ring still holds are counted in the 24 h window.
	assert.Equal(t, int32(40), node.Status.ActivationsTotal)
	assert.Equal(t, int32(16), node.Status.ActivationsLast24H, "the ring's newest 32 rows carry 16 boots")
}

// TestActivationsLast24HIsNotStale is the 24 h honesty case the design note asks for, with an
// injected clock: three activations, advance 25 hours, the count is 0 - with no new transition,
// on a pass that records nothing, and it says so by returning true (there is a write to do).
func TestActivationsLast24HIsNotStale(t *testing.T) {
	node := slot()

	at := t0

	for range 3 {
		Record(node, infrav2alpha1.NodePhaseBooting, at, "boot")

		at = at.Add(2 * time.Minute)

		Record(node, infrav2alpha1.NodePhaseOff, at, "off")

		at = at.Add(2 * time.Minute)
	}

	require.Equal(t, int32(3), node.Status.ActivationsLast24H)
	require.Equal(t, int32(3), node.Status.ActivationsTotal)

	// 25 hours later, on a pass whose report has not changed (the slot is still `off`).
	later := t0.Add(25 * time.Hour)

	changed := Record(node, infrav2alpha1.NodePhaseOff, later, "resync")

	assert.True(t, changed, "the window moved, so the caller has a write to land")
	assert.Equal(t, int32(0), node.Status.ActivationsLast24H, "the day that passed contains no boot")
	assert.Equal(t, int32(3), node.Status.ActivationsTotal, "and the lifetime counter is untouched: it is not a window")
	assert.Empty(t, node.Status.RecentTransitions, "the ring is pruned to the window")
	assert.True(t, node.Status.LastTransitionTime.Time.Equal(t0.Add(10*time.Minute)), "the last change is still the one that happened, not the pass that pruned")

	// A second pass at the same instant records and prunes nothing.
	assert.False(t, Record(node, infrav2alpha1.NodePhaseOff, later.Add(time.Minute), "resync"))
}

// TestActivationsLast24HIsAPartialWindow: a boot is counted while it is inside the window and
// stops being counted the minute it leaves, one row at a time.
func TestActivationsLast24HIsAPartialWindow(t *testing.T) {
	node := slot()

	Record(node, infrav2alpha1.NodePhaseBooting, t0, "boot 1")
	Record(node, infrav2alpha1.NodePhaseOff, t0.Add(time.Hour), "off 1")
	Record(node, infrav2alpha1.NodePhaseBooting, t0.Add(2*time.Hour), "boot 2")
	Record(node, infrav2alpha1.NodePhaseOff, t0.Add(3*time.Hour), "off 2")
	Record(node, infrav2alpha1.NodePhaseBooting, t0.Add(4*time.Hour), "boot 3")
	Record(node, infrav2alpha1.NodePhaseOff, t0.Add(5*time.Hour), "off 3")

	assert.Equal(t, int32(3), node.Status.ActivationsLast24H)

	// t0 + 24h + 1ns: boot 1 is out, boots 2 and 3 are in.
	Record(node, infrav2alpha1.NodePhaseOff, t0.Add(24*time.Hour+time.Second), "resync")

	assert.Equal(t, int32(2), node.Status.ActivationsLast24H)
	assert.Len(t, node.Status.RecentTransitions, 5)

	// t0 + 27h: boot 1 and boot 2 have left the window, boot 3 has not. The window slides one row
	// at a time, and the count follows it row by row.
	Record(node, infrav2alpha1.NodePhaseOff, t0.Add(27*time.Hour), "resync")

	assert.Equal(t, int32(1), node.Status.ActivationsLast24H)
	assert.Len(t, node.Status.RecentTransitions, 3)

	// t0 + 29h: the last boot is out too, and the ring holds only the row that is exactly at the
	// cutoff (an entry is dropped once it is strictly older than the window).
	Record(node, infrav2alpha1.NodePhaseOff, t0.Add(29*time.Hour), "resync")

	assert.Equal(t, int32(0), node.Status.ActivationsLast24H)
}

// TestNextTransitionExpiryIsTheInstantTheCountChanges is the other half of the honesty mechanism:
// the reconciler is told when to come back, and the answer is the expiry of the oldest row.
func TestNextTransitionExpiryIsTheInstantTheCountChanges(t *testing.T) {
	empty := slot()

	_, ok := NextTransitionExpiry(empty, t0)
	assert.False(t, ok, "a slot with no ring has no window to age: no requeue to schedule")

	node := slot()
	Record(node, infrav2alpha1.NodePhaseBooting, t0, "boot 1")
	Record(node, infrav2alpha1.NodePhaseOff, t0.Add(time.Hour), "off 1")
	Record(node, infrav2alpha1.NodePhaseBooting, t0.Add(2*time.Hour), "boot 2")

	expiry, ok := NextTransitionExpiry(node, t0.Add(3*time.Hour))
	require.True(t, ok)
	assert.True(t, expiry.Equal(t0.Add(24*time.Hour)), "the oldest row leaves the window at t0+24h: come back then")

	// A row that is already outside the window is "now": a pass must run, at once.
	late, ok := NextTransitionExpiry(node, t0.Add(30*time.Hour))
	require.True(t, ok)
	assert.True(t, late.Equal(t0.Add(30*time.Hour)), "an expired window asks for the next pass, not a delay")

	// After the pass that prunes, there is nothing left to schedule. The pass records no transition:
	// it hands in the phase the slot already reports, which is the refresh's shape.
	Record(node, infrav2alpha1.NodePhaseBooting, t0.Add(30*time.Hour), "resync")

	_, ok = NextTransitionExpiry(node, t0.Add(30*time.Hour))
	assert.False(t, ok, "pruned and empty: no requeue")
}

// TestRecordClipsAReason: a reason is composed from object names, and the CRD bounds it. A reason
// the API server refuses for being long would cost the whole report, so the writer clips it.
func TestRecordClipsAReason(t *testing.T) {
	node := slot()

	long := strings.Repeat("claim-", 500)

	Record(node, infrav2alpha1.NodePhaseBooting, t0, long)

	require.Len(t, node.Status.RecentTransitions, 1)
	assert.Len(t, node.Status.RecentTransitions[0].Reason, ReasonMaxLength)
}

// TestReasonNamesTheInputsItWasDerivedFrom: the row says what the value was computed from, so
// "why does this slot say draining" is answered without reading the claim.
func TestReasonNamesTheInputsItWasDerivedFrom(t *testing.T) {
	free := Reason(Input{ObservedPower: infrav2alpha1.PowerStateOff}, infrav2alpha1.NodePhaseOff)
	assert.Equal(t, "no live claim, observed off", free)

	booting := Reason(Input{Claim: claim(infrav2alpha1.NodeClaimBooting, false), ObservedPower: infrav2alpha1.PowerStateOff, StartupJobInFlight: true}, infrav2alpha1.NodePhaseBooting)
	assert.Equal(t, "claim claim-worker-01 booting, observed off, startup job in flight", booting)

	gated := Reason(Input{Claim: claim(infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOn}, infrav2alpha1.NodePhaseBooting)
	assert.Equal(t, "claim claim-worker-01 ready, observed on, node not usable", gated)

	draining := Reason(Input{Claim: claim(infrav2alpha1.NodeClaimReady, true), ObservedPower: infrav2alpha1.PowerStateOn}, infrav2alpha1.NodePhaseDraining)
	assert.Equal(t, "claim claim-worker-01 marked for scale-down, observed on", draining)

	never := Reason(Input{Claim: claim("", false)}, infrav2alpha1.NodePhasePending)
	assert.Equal(t, "claim claim-worker-01 pending, never observed", never)
}

// TestRecordIsIdempotentAcrossWriters: the observation path and the transition path call the same
// writer with the same derived value. Two writers, one history: a slot cannot end up with the row
// recorded twice because both paths saw the same change.
func TestRecordIsIdempotentAcrossWriters(t *testing.T) {
	first := slot()
	require.True(t, Record(first, infrav2alpha1.NodePhaseBooting, t0, "claim c1, observed off"))
	require.True(t, Record(first, infrav2alpha1.NodePhaseReady, t0.Add(3*time.Minute), "observed on, usable"))

	second := first.DeepCopy()

	// The other writer lands the same change on its own copy: nothing to add.
	require.False(t, Record(second, infrav2alpha1.NodePhaseReady, t0.Add(4*time.Minute), "observed on, usable"))

	assert.Equal(t, int32(1), second.Status.ActivationsTotal)
	assert.Equal(t, infrav2alpha1.NodePhaseReady, second.Status.Phase)
	assert.Len(t, second.Status.RecentTransitions, 2)
	assert.Equal(t, first.Status.RecentTransitions[1].Reason, second.Status.RecentTransitions[1].Reason)
}

// TestIsUpIsThePoolOnCount: the pool counts the phases that mean a machine is being paid for.
func TestIsUpIsThePoolOnCount(t *testing.T) {
	assert.True(t, IsUp(infrav2alpha1.NodePhaseBooting))
	assert.True(t, IsUp(infrav2alpha1.NodePhaseReady))
	assert.True(t, IsUp(infrav2alpha1.NodePhaseDraining))

	assert.False(t, IsUp(infrav2alpha1.NodePhaseOff))
	assert.False(t, IsUp(infrav2alpha1.NodePhasePending))
	assert.False(t, IsUp(""))
}
