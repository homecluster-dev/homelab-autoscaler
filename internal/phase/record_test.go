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

// TestRingIsBoundedAndTheCountIsNot: 40 boots of a churning slot leave the ring with its last 32
// rows, newest last - the bound is what keeps a day of a pool in etcd - and the 24 h count with all
// 40. A count read through a display bound is a count capped at half the display bound: the ring is
// 32 rows and a boot is two of them (in and out), so counting the ring flatlines the churn column at
// 16 exactly where the question starts to matter. The ledger of boots is a second, cheaper list
// (one timestamp per row) and is what activationsLast24H is the length of.
func TestRingIsBoundedAndTheCountIsNot(t *testing.T) {
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

	// The lifetime counter is not bounded by anything, and the window count is not bounded by the
	// ring: all 40 boots are in both, and the ledger that counts them holds exactly 40 rows.
	assert.Equal(t, int32(40), node.Status.ActivationsTotal)
	assert.Equal(t, int32(40), node.Status.ActivationsLast24H, "the count is the ledger, not the ring")
	require.Len(t, node.Status.RecentActivations, 40)

	// The two lists are pruned by the same cutoff. The churn spans 80 minutes, so a pass 26 hours
	// later has a cutoff past every row of both, and the count goes with them.
	Record(node, infrav2alpha1.NodePhaseOff, t0.Add(26*time.Hour), "resync")

	assert.Equal(t, int32(0), node.Status.ActivationsLast24H)
	assert.Empty(t, node.Status.RecentActivations, "the ledger ages out row by row, like the ring")
	assert.Equal(t, int32(40), node.Status.ActivationsTotal, "the lifetime counter is not a window")
}

// TestActivationWindowIsBoundedIsTheHonestBound: 600 boots in a day are 600 rows in a list of
// timestamps, and the ledger's own cap (ActivationWindowCap) is where the count stops - a floor the
// column states by sitting at the cap, not a silent flatline at 16.
func TestActivationWindowIsBoundedIsTheHonestBound(t *testing.T) {
	node := slot()

	at := t0

	for range 600 {
		Record(node, infrav2alpha1.NodePhaseBooting, at, "boot")
		at = at.Add(time.Second)

		Record(node, infrav2alpha1.NodePhaseOff, at, "off")
		at = at.Add(time.Second)
	}

	assert.Equal(t, int32(600), node.Status.ActivationsTotal, "the lifetime count is unbounded")
	require.Len(t, node.Status.RecentTransitions, TransitionRingCap, "the display ring keeps its bound")
	require.Len(t, node.Status.RecentActivations, ActivationWindowCap, "the ledger keeps its own, 16x wider")
	assert.Equal(t, int32(ActivationWindowCap), node.Status.ActivationsLast24H, "the count is the ledger's length: a stated floor")

	// The rows the cap dropped are the oldest: the newest boot of all is in the ledger, and the
	// window's next expiry is the oldest row it kept.
	expiry, ok := NextTransitionExpiry(node, at)
	require.True(t, ok)
	assert.True(t, expiry.After(at), "the count still has boots to lose, and the requeue says when")
}

// TestLedgerIsSeededFromTheRingOnUpgrade: a slot written before the ledger existed has a ring and no
// activation rows. Recomputing the count from an empty ledger would zero a churning pool's column on
// the upgrade and for a day after it, so the boots the ring still names become the ledger's rows.
func TestLedgerIsSeededFromTheRingOnUpgrade(t *testing.T) {
	node := slot()

	at := metav1.NewTime(t0)
	node.Status.RecentTransitions = []infrav2alpha1.NodeTransition{
		{At: &at, From: infrav2alpha1.NodePhaseOff, To: infrav2alpha1.NodePhaseBooting},
		{At: &at, From: infrav2alpha1.NodePhaseBooting, To: infrav2alpha1.NodePhaseReady},
	}
	node.Status.Phase = infrav2alpha1.NodePhaseReady
	node.Status.ActivationsLast24H = 1

	// A stable pass: the slot reports what the inputs derive, so nothing is appended and the window
	// is what moves - and the seeding is what keeps the count at 1 instead of 0.
	require.True(t, Record(node, infrav2alpha1.NodePhaseReady, t0.Add(time.Hour), "resync"))

	require.Len(t, node.Status.RecentActivations, 1, "the boot the ring remembers became a ledger row")
	assert.True(t, node.Status.RecentActivations[0].Time.Equal(t0))
	assert.Equal(t, int32(1), node.Status.ActivationsLast24H)
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

// TestClipIsTheBoundOfBothComposedStrings: the ring's reason and the pool's last-scale sentence are
// composed from object names, and Clip is their one bound. A Kubernetes name is 253 characters, the
// verb `ca:DeleteNodes` is 14 plus a space, and the field is bounded at 256 - so the composition is
// over the bound by 12 characters on a legal name, and a writer that does not clip produces a status
// the API server refuses on every pass, for an object nobody can fix without deleting it.
func TestClipIsTheBoundOfBothComposedStrings(t *testing.T) {
	long := "ca:DeleteNodes " + strings.Repeat("w", 253)
	require.Greater(t, len([]rune(long)), ScaleActionMaxLength)

	clipped := Clip(long, ScaleActionMaxLength)

	assert.Len(t, []rune(clipped), ScaleActionMaxLength, "exactly the bound, ellipsis included")
	assert.True(t, strings.HasPrefix(clipped, "ca:DeleteNodes "), "the verb survives the clip: the sentence starts with what the pool did")
	assert.True(t, strings.HasSuffix(clipped, "..."), "and the truncation is visible")

	assert.Equal(t, "up remote-worker-2", Clip("up remote-worker-2", ScaleActionMaxLength), "a string inside the bound is returned untouched")
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
