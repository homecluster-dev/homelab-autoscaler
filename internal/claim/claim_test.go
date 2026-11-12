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

package claim

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
)

// The fixtures every test of this package reads: one slot, one group, one request. The names are
// the ones the design's example uses (docs/development/nodeclaim.md §3), so a failure reads as the doc.
const (
	fixNamespace = "homelab-autoscaler-system"
	fixSlot      = "remote-worker-2"
	fixGroup     = "remote-workers"
	fixK8sNode   = "remote-worker-2"
)

// slot returns the Node CR (the slot) a claim asks for.
func slot() *infrav2alpha1.Node {
	return &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fixSlot,
			Namespace: fixNamespace,
			UID:       types.UID("uid-" + fixSlot),
			Labels:    map[string]string{LabelGroup: fixGroup},
		},
		Spec: infrav2alpha1.NodeSpec{KubernetesNodeName: fixK8sNode},
	}
}

// group returns the Group CR whose spec.maxNodeProvisionTime floors a claim's deadline. A nil
// maxNodeProvisionTime is the pool that declares nothing.
func group(maxNodeProvisionTime *metav1.Duration) *infrav2alpha1.Group {
	return &infrav2alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: fixGroup, Namespace: fixNamespace},
		Spec:       infrav2alpha1.GroupSpec{MaxNodeProvisionTime: maxNodeProvisionTime},
	}
}

// increaseSize is the request the provider's NodeGroupIncreaseSize handler makes.
func increaseSize(delta int32) infrav2alpha1.ClaimRequest {
	return infrav2alpha1.ClaimRequest{
		Call:  infrav2alpha1.ClaimCallIncreaseSize,
		Delta: delta,
		Group: fixGroup,
	}
}

// deleteNodes is the request the provider's NodeGroupDeleteNodes handler records on a claim.
func deleteNodes(names ...string) infrav2alpha1.ClaimRequest {
	return infrav2alpha1.ClaimRequest{
		Call:  infrav2alpha1.ClaimCallDeleteNodes,
		Group: fixGroup,
		Nodes: names,
	}
}

// TestAttemptBudgetIsTheSumOfTheDeadlines pins §4 of the design: the budget of one attempt is the
// sum of every deadline an attempt can consume, plus one sleep at the ceiling. The expected value is
// computed from the fsm's own constants, so growing a deadline without growing the budget fails.
//
// The set is what the test asserts, not just the total. The budget is measured from
// spec.requestedAt, i.e. from before the startup Job exists, so one attempt can spend the Job's own
// timeout, then the registration wait, then the readiness gate, in series: a sum that quietly drops
// one of the constants is a claim killed in the middle of the machine it is still buying, and an
// assertion that only compares the budget against the *smallest* member (a poll interval) cannot see
// that. So the set is asserted three ways: the sum of the named set equals the budget, no proper
// subset of it does, and the budget outlasts the serial chain of the deadlines it bounds.
func TestAttemptBudgetIsTheSumOfTheDeadlines(t *testing.T) {
	constants := BudgetConstants()

	names := make([]string, 0, len(constants))
	for name := range constants {
		names = append(names, name)
	}
	sort.Strings(names)

	// The set the budget is built from, spelled out. A constant leaving the budget has to leave this
	// list too, and the sum assertion below is what catches it.
	assert.Equal(t, []string{
		"job monitor poll",
		"one sleep at the ceiling",
		"readiness gate",
		"registration wait",
		"startup Job timeout",
		"uncordon retry ceiling",
	}, names, "the set of constants the budget is built from")

	assert.Equal(t, map[string]time.Duration{
		"startup Job timeout":      fsm.DefaultJobTimeout,
		"job monitor poll":         fsm.JobMonitorPollInterval,
		"registration wait":        fsm.DefaultUncordonTimeout,
		"readiness gate":           fsm.DefaultReadinessGateTimeout,
		"uncordon retry ceiling":   fsm.MaxUncordonRetryInterval,
		"one sleep at the ceiling": time.Minute,
	}, constants, "each member of the set is the fsm constant of §4, not a copy of a number")

	total := time.Duration(0)
	for _, name := range names {
		total += constants[name]
	}

	assert.Equal(t, total, AttemptBudget(),
		"the budget is the sum of the named set: startup Job timeout + job monitor poll + registration wait + readiness gate + margin")

	// No proper subset of the set adds up to the budget: dropping one member, in particular the
	// startup Job's own timeout, moves the total and fails here.
	for _, name := range names {
		assert.NotEqual(t, AttemptBudget(), total-constants[name],
			"the budget is built from %s too: without it a claim is killed at the boundary of a deadline it is still inside", name)
	}

	// The design names the number the set adds up to (docs/development/nodeclaim.md §4:
	// 5m + 30s + 90s + 4m + 10s + 1m = 12m10s). Changing a deadline is a deliberate act, and this
	// assertion is the one that says so in the release notes.
	assert.Equal(t, 12*time.Minute+10*time.Second, AttemptBudget(),
		"the budget the design states is 12m10s")

	// The budget is longer than each deadline it covers, and longer than the chain they form in
	// series: a Job that burns its whole 5m, a node that takes the whole 90s to register, and a gate
	// that runs its whole 4m is 10m30s of attempt, and a 7m10s budget failed the claim inside it.
	// That is the arithmetic the 0.1.20 watchdog's grace got by only applying to a Job that had
	// already finished, and a claim's deadline has no such restriction to inherit.
	serial := fsm.DefaultJobTimeout + fsm.DefaultUncordonTimeout + fsm.DefaultReadinessGateTimeout
	assert.Greater(t, AttemptBudget(), serial,
		"the budget must outlast a Job that spends its whole timeout and then the registration wait and then the gate (%s)", serial)

	for name, deadline := range constants {
		assert.Greater(t, AttemptBudget(), deadline, "the budget must outlast the %s it covers", name)
	}
}

// TestMachineGoneGraceIsABootWindow is M2's bound: the observation that a ready claim's machine is
// off has to outlast a whole boot window before the model calls the machine gone, so a reboot is not
// an attempt that ends, and a machine that is really gone is not counted as capacity forever.
func TestMachineGoneGraceIsABootWindow(t *testing.T) {
	assert.Equal(t, AttemptBudget(), MachineGoneGrace(),
		"the grace is one boot window: the same horizon the deadline of the attempt is measured by")
}

// TestDeadlineAtIsTheBudgetFlooredByTheGroupNumber is the floor rule of §4:
//
//	deadlineAt = requestedAt + max(budget, group.spec.maxNodeProvisionTime)
//
// The floor exists because cluster-autoscaler is told the group's number as its own scale-up
// termination deadline: a claim that dies before that instant is one the autoscaler is still
// waiting for, and its answer to a group that stopped growing is another IncreaseSize.
func TestDeadlineAtIsTheBudgetFlooredByTheGroupNumber(t *testing.T) {
	requestedAt := time.Date(2026, 3, 4, 9, 38, 15, 0, time.UTC)
	budget := AttemptBudget()

	tests := []struct {
		name     string
		groupMax *metav1.Duration
		// wantOffset is how far past requestedAt the deadline lands.
		wantOffset time.Duration
	}{
		{
			name:       "a pool that declares no number: the operator's budget alone",
			groupMax:   nil,
			wantOffset: budget,
		},
		{
			name:       "a zero group number is no floor",
			groupMax:   &metav1.Duration{Duration: 0},
			wantOffset: budget,
		},
		{
			name:       "a negative group number is no floor",
			groupMax:   &metav1.Duration{Duration: -time.Minute},
			wantOffset: budget,
		},
		{
			// The k3d and the website examples declare 2m. The shorter number never wins: enforcing
			// it would fail a claim while cluster-autoscaler is still waiting for the machine.
			name:       "a group number shorter than the budget does not shorten the deadline",
			groupMax:   &metav1.Duration{Duration: 2 * time.Minute},
			wantOffset: budget,
		},
		{
			name:       "a group number equal to the budget is the deadline",
			groupMax:   &metav1.Duration{Duration: budget},
			wantOffset: budget,
		},
		{
			name:       "a longer group number extends the deadline to the autoscaler's own horizon",
			groupMax:   &metav1.Duration{Duration: 20 * time.Minute},
			wantOffset: 20 * time.Minute,
		},
		{
			name:       "a pool that never gives up",
			groupMax:   &metav1.Duration{Duration: 24 * time.Hour},
			wantOffset: 24 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeadlineAt(requestedAt, tt.groupMax)

			assert.Equal(t, requestedAt.Add(tt.wantOffset), got)
			assert.Greater(t, got, requestedAt, "a claim is never born overdue")

			// The floor, stated as an inequality so a longer budget and a longer group number are
			// both covered by every case: the deadline is never earlier than either number.
			assert.False(t, got.Before(requestedAt.Add(budget)),
				"the deadline must not cut the operator's budget short")

			if tt.groupMax != nil && tt.groupMax.Duration > 0 {
				assert.False(t, got.Before(requestedAt.Add(tt.groupMax.Duration)),
					"the deadline must not cut the group's maxNodeProvisionTime short")
			}
		})
	}
}

// TestIsOverdueIsTheClockOfTheDeadline: overdue means failed (§4), so the predicate that decides
// it is the one place a claim's end is timed.
func TestIsOverdueIsTheClockOfTheDeadline(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 45, 25, 0, time.UTC)

	tests := []struct {
		name  string
		claim *infrav2alpha1.NodeClaim
		want  bool
	}{
		{name: "no claim asks for nothing", claim: nil, want: false},
		{
			name:  "a claim with no deadline is never overdue",
			claim: &infrav2alpha1.NodeClaim{},
			want:  false,
		},
		{
			name:  "the deadline passed",
			claim: &infrav2alpha1.NodeClaim{Spec: infrav2alpha1.NodeClaimSpec{DeadlineAt: ptrTime(now.Add(-time.Second))}},
			want:  true,
		},
		{
			name:  "the deadline is still ahead",
			claim: &infrav2alpha1.NodeClaim{Spec: infrav2alpha1.NodeClaimSpec{DeadlineAt: ptrTime(now.Add(time.Second))}},
			want:  false,
		},
		{
			name:  "the instant of the deadline is not past it",
			claim: &infrav2alpha1.NodeClaim{Spec: infrav2alpha1.NodeClaimSpec{DeadlineAt: ptrTime(now)}},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsOverdue(tt.claim, now))
		})
	}
}

// TestBudgetIsTheNumberInForceForReporting: the group's spec.maxNodeProvisionTime is copied onto
// the claim, so a pool that changes the number does not rewrite the history of a claim (§4).
func TestBudgetIsTheNumberInForceForReporting(t *testing.T) {
	long := 20 * time.Minute

	tests := []struct {
		name  string
		group *infrav2alpha1.Group
		want  *metav1.Duration
	}{
		{name: "no group, no number", group: nil, want: nil},
		{
			name:  "a pool that declares nothing carries no number",
			group: group(nil),
			want:  nil,
		},
		{
			name:  "the declared number is the one in force",
			group: group(&metav1.Duration{Duration: long}),
			want:  &metav1.Duration{Duration: long},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Budget(tt.group))
		})
	}

	// The value handed out is a copy: a claim that carries it must not alias the Group's spec, or
	// writing the claim writes the pool.
	g := group(&metav1.Duration{Duration: long})
	copied := Budget(g)
	require.NotNil(t, copied)
	require.NotSame(t, g.Spec.MaxNodeProvisionTime, copied)

	copied.Duration = time.Second
	assert.Equal(t, long, g.Spec.MaxNodeProvisionTime.Duration, "the Group's spec is not writable through a claim")
}

// TestSeedAttemptIsTheStreakOfTheSlot is §4's attempt rule as data: the next claim of a slot carries
// the last attempt + 1 while the slot keeps failing, and 1 again once an attempt has landed. The
// digest's phase is the record of what the last one did, so the reset rule is a read of an object
// that ends with the episode - nothing accumulates on the slot, and there is no reset heuristic.
func TestSeedAttemptIsTheStreakOfTheSlot(t *testing.T) {
	tests := []struct {
		name string
		last *infrav2alpha1.LastClaimSummary
		want int32
	}{
		{
			name: "a slot that never had a claim buys its first machine",
			last: nil,
			want: 1,
		},
		{
			name: "a digest with no attempt number is the first attempt",
			last: &infrav2alpha1.LastClaimSummary{Name: "remote-worker-2-7f4b9c", Phase: infrav2alpha1.NodeClaimFailed},
			want: 1,
		},
		{
			name: "a digest with a zero attempt number is the first attempt",
			last: &infrav2alpha1.LastClaimSummary{Attempt: 0, Phase: infrav2alpha1.NodeClaimFailed},
			want: 1,
		},
		{
			// The shape the deadline leaves behind: attempt 1 bought nothing, so the next claim is
			// attempt 2, and the bound is counted on the claims, not on the slot.
			name: "a failed attempt is followed by the next one",
			last: &infrav2alpha1.LastClaimSummary{Attempt: 1, Phase: infrav2alpha1.NodeClaimFailed, Reason: infrav2alpha1.ClaimReasonDeadlineExceeded},
			want: 2,
		},
		{
			name: "the streak grows with every attempt that did not land",
			last: &infrav2alpha1.LastClaimSummary{Attempt: 2, Phase: infrav2alpha1.NodeClaimFailed, Reason: infrav2alpha1.ClaimReasonStartupJobFailed},
			want: 3,
		},
		{
			name: "the streak crosses the bound, and the claim controller refuses the machine",
			last: &infrav2alpha1.LastClaimSummary{Attempt: MaxConsecutiveAttempts, Phase: infrav2alpha1.NodeClaimFailed},
			want: MaxConsecutiveAttempts + 1,
		},
		{
			// ready and completed are the two phases that say the attempt produced a machine the pool
			// could use, and both end the streak. A scale-down that landed on a slot is a completed
			// claim: the next request for that machine starts counting again at 1.
			name: "a landed attempt resets the streak",
			last: &infrav2alpha1.LastClaimSummary{Attempt: MaxConsecutiveAttempts, Phase: infrav2alpha1.NodeClaimCompleted, Call: infrav2alpha1.ClaimCallDeleteNodes},
			want: 1,
		},
		{
			name: "a claim that reached ready resets the streak",
			last: &infrav2alpha1.LastClaimSummary{Attempt: MaxConsecutiveAttempts, Phase: infrav2alpha1.NodeClaimReady, Call: infrav2alpha1.ClaimCallIncreaseSize},
			want: 1,
		},
		{
			// A digest whose phase is empty is not a record of a landed attempt, so the streak is
			// kept: the seed never invents history, it reads what the last claim reported.
			name: "a claim whose digest never stamped a phase keeps the streak",
			last: &infrav2alpha1.LastClaimSummary{Attempt: 1},
			want: 2,
		},
		{
			name: "a negative digest is read as no history",
			last: &infrav2alpha1.LastClaimSummary{Attempt: -4, Phase: infrav2alpha1.NodeClaimFailed},
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SeedAttempt(tt.last))
		})
	}
}

// TestSeedAttemptIsTheStreakOfOneEpisode runs the streak the way the claims of a slot run it: the
// seed of each claim is taken from the digest of the one before it. The bound is a bound on
// consecutive claims, and a landed attempt ends the episode, so the next episode counts from 1 and
// the failures of the old one are not resurrected.
func TestSeedAttemptIsTheStreakOfOneEpisode(t *testing.T) {
	var digest *infrav2alpha1.LastClaimSummary

	// A machine that never arrives: attempt 1, 2, 3, and the claim past the bound the controller
	// refuses before it buys a machine.
	for want := int32(1); want <= MaxConsecutiveAttempts+1; want++ {
		seed := SeedAttempt(digest)
		require.Equal(t, want, seed, "attempt %d of a slot that keeps failing", want)

		digest = &infrav2alpha1.LastClaimSummary{
			Name:    fmt.Sprintf("%s-attempt-%d", fixSlot, want),
			Phase:   infrav2alpha1.NodeClaimFailed,
			Attempt: seed,
			Reason:  infrav2alpha1.ClaimReasonDeadlineExceeded,
			Call:    infrav2alpha1.ClaimCallIncreaseSize,
			Group:   fixGroup,
		}
	}

	assert.Greater(t, SeedAttempt(digest), MaxConsecutiveAttempts,
		"the streak is what the bound reads: the seed crosses claim.MaxConsecutiveAttempts, and the claim controller fails that claim (ClaimReasonAttemptLimitExceeded)")

	// The machine arrives: the attempt lands, and the episode ends with the object that counted it.
	// Two more episodes, each starting at 1, are the "no counter on the slot to accumulate" of §4.
	for episode := 1; episode <= 3; episode++ {
		digest = &infrav2alpha1.LastClaimSummary{
			Name:    fmt.Sprintf("%s-episode-%d", fixSlot, episode),
			Phase:   infrav2alpha1.NodeClaimReady,
			Attempt: MaxConsecutiveAttempts,
			Call:    infrav2alpha1.ClaimCallIncreaseSize,
			Group:   fixGroup,
		}

		require.Equal(t, int32(1), SeedAttempt(digest), "episode %d starts a new streak", episode)

		// The first claim of the new episode fails: the next one is 2, not 4 (the number the old
		// episode ended at is gone with its object).
		digest = &infrav2alpha1.LastClaimSummary{Name: digest.Name, Phase: infrav2alpha1.NodeClaimFailed, Attempt: 1}
		assert.Equal(t, int32(2), SeedAttempt(digest))
	}
}

// TestNewIsTheRequestTheScalarCouldNot: a claim is created with its attribution, its deadline, its
// TTL, its labels and its ownerReference, and with no attempt number - the claim controller seeds
// status.attempt on the first pass, because the seeding reads the slot's digest and a claim created
// from a stale cache must not bake in a number the controller cannot correct.
func TestNewIsTheRequestTheScalarCouldNot(t *testing.T) {
	scheme := scheme(t)

	// A group number longer than the operator's budget (12m10s, §4): the floor is what the deadline
	// lands on, so the fixture has to be above it to test the floor.
	long := 20 * time.Minute

	tests := []struct {
		name        string
		group       *infrav2alpha1.Group
		req         infrav2alpha1.ClaimRequest
		requestedBy string
		// wantOffset is the budget the deadline carries: the operator's, floored by the group's.
		wantOffset time.Duration
	}{
		{
			name:        "a scale-up from cluster-autoscaler",
			group:       group(nil),
			req:         increaseSize(1),
			requestedBy: RequestedByAutoscaler,
			wantOffset:  AttemptBudget(),
		},
		{
			name:        "a scale-up of a delta, on a pool with a long provision horizon",
			group:       group(&metav1.Duration{Duration: long}),
			req:         increaseSize(3),
			requestedBy: RequestedByAutoscaler,
			wantOffset:  long,
		},
		{
			name:        "a scale-down that names the machines it takes away",
			group:       group(&metav1.Duration{Duration: 2 * time.Minute}),
			req:         deleteNodes(fixK8sNode, "remote-worker-3"),
			requestedBy: RequestedByAutoscaler,
			wantOffset:  AttemptBudget(),
		},
		{
			// The manual claim of §10: the operator names themselves, because requestedBy is read
			// by a human and parsed by nobody.
			name:        "a manual claim",
			group:       group(nil),
			req:         increaseSize(1),
			requestedBy: "filippo@kubectl",
			wantOffset:  AttemptBudget(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim, err := New(slot(), tt.group, tt.req, tt.requestedBy, scheme)
			require.NoError(t, err)

			// Identity: <slot>-<suffix>, in the slot's namespace, and never renamed.
			assert.True(t, strings.HasPrefix(nodeClaim.Name, fixSlot+"-"),
				"the claim's name says which slot it asks for, got %q", nodeClaim.Name)
			assert.NotEqual(t, fixSlot, nodeClaim.Name, "the suffix makes two claims of one slot two objects")
			assert.Equal(t, fixNamespace, nodeClaim.Namespace)

			// The labels are the list command of the design: -l infra.homecluster.dev/node=<slot>.
			assert.Equal(t, fixSlot, nodeClaim.Labels[LabelSlot])
			assert.Equal(t, tt.req.Group, nodeClaim.Labels[LabelGroup])

			// The slot it asks for, by name and by uid.
			assert.Equal(t, infrav2alpha1.GroupVersion.String(), nodeClaim.Spec.SlotRef.APIVersion)
			assert.Equal(t, "Node", nodeClaim.Spec.SlotRef.Kind)
			assert.Equal(t, fixNamespace, nodeClaim.Spec.SlotRef.Namespace)
			assert.Equal(t, fixSlot, SlotNameOf(nodeClaim))
			assert.NotEmpty(t, nodeClaim.Spec.SlotRef.UID)

			// The attribution: who asked, which call, with what arguments, for which pool.
			assert.Equal(t, tt.requestedBy, nodeClaim.Spec.RequestedBy)
			assert.Equal(t, tt.req, nodeClaim.Spec.Request)

			// The deadline and the number in force, and the TTL that collects the object after it
			// lands.
			requestedAt := nodeClaim.Spec.RequestedAt.Time
			require.NotNil(t, nodeClaim.Spec.DeadlineAt)
			assert.WithinDuration(t, requestedAt.Add(tt.wantOffset), nodeClaim.Spec.DeadlineAt.Time, time.Second)
			assert.False(t, IsOverdue(nodeClaim, requestedAt), "a claim is born inside its budget")
			assert.Equal(t, tt.group.Spec.MaxNodeProvisionTime, nodeClaim.Spec.MaxNodeProvisionTime)
			require.NotNil(t, nodeClaim.Spec.TTLSecondsAfterFinished)
			assert.Equal(t, DefaultTTLSecondsAfterFinished, *nodeClaim.Spec.TTLSecondsAfterFinished)

			// Deleting the slot from Git takes its claims with it (§8).
			require.Len(t, nodeClaim.OwnerReferences, 1)
			assert.Equal(t, fixSlot, nodeClaim.OwnerReferences[0].Name)
			assert.Equal(t, "Node", nodeClaim.OwnerReferences[0].Kind)
			require.NotNil(t, nodeClaim.OwnerReferences[0].Controller)
			assert.True(t, *nodeClaim.OwnerReferences[0].Controller)

			// The attempt is seeded by the controller, not baked in at creation.
			assert.Zero(t, nodeClaim.Status.Attempt)
			assert.Empty(t, nodeClaim.Status.Phase)
		})
	}
}

// TestDigestIsTheStoryThatOutlivesTheClaim: claims are collected by their TTL, and Node.status.
// lastClaim is what survives the collection, so that `kubectl describe node.infra.homecluster.dev
// <slot>` answers "who asked, when, how many attempts, why did it end" (§8).
func TestDigestIsTheStoryThatOutlivesTheClaim(t *testing.T) {
	requested := time.Date(2026, 3, 4, 9, 38, 15, 0, time.UTC)
	deadline := time.Date(2026, 3, 4, 9, 45, 25, 0, time.UTC)
	finished := time.Date(2026, 3, 4, 9, 45, 26, 0, time.UTC)
	// The scale-down's own instant: three days after the request that created the machine it takes
	// back, which is the point of keeping both.
	scaledDown := requested.Add(3 * 24 * time.Hour)

	overdue := &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: fixSlot + "-7f4b9c", Namespace: fixNamespace},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef:     corev1.ObjectReference{Name: fixSlot},
			RequestedAt: metav1.NewTime(requested),
			RequestedBy: RequestedByAutoscaler,
			Request:     increaseSize(1),
			DeadlineAt:  ptrTime(deadline),
		},
		Status: infrav2alpha1.NodeClaimStatus{
			Phase:      infrav2alpha1.NodeClaimFailed,
			Attempt:    2,
			Reason:     infrav2alpha1.ClaimReasonDeadlineExceeded,
			FinishedAt: ptrTime(finished),
		},
	}

	tests := []struct {
		name  string
		claim *infrav2alpha1.NodeClaim
		want  *infrav2alpha1.LastClaimSummary
		// wantSeed is the attempt number the next claim of the slot carries, computed from the
		// record: the digest is the seed's only input, so the story and the streak are one object.
		wantSeed int32
	}{
		{
			name:     "no claim, no story",
			claim:    nil,
			want:     nil,
			wantSeed: 1,
		},
		{
			name:     "an overdue attempt, recorded with its reason",
			claim:    overdue,
			wantSeed: 3,
			want: &infrav2alpha1.LastClaimSummary{
				Name:        overdue.Name,
				Phase:       infrav2alpha1.NodeClaimFailed,
				Attempt:     2,
				Call:        infrav2alpha1.ClaimCallIncreaseSize,
				Group:       fixGroup,
				Reason:      infrav2alpha1.ClaimReasonDeadlineExceeded,
				RequestedAt: ptrTime(requested),
				DeadlineAt:  ptrTime(deadline),
				FinishedAt:  ptrTime(finished),
			},
		},
		{
			// The shape a scale-down leaves behind: the digest says which call ended the slot, which
			// is what SeedAttempt reads to restart the streak.
			name:     "a scale-down that landed",
			wantSeed: 1,
			claim: &infrav2alpha1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: fixSlot + "-a1b2c3"},
				Spec: infrav2alpha1.NodeClaimSpec{
					SlotRef: corev1.ObjectReference{Name: fixSlot},
					Request: deleteNodes(fixK8sNode),
				},
				Status: infrav2alpha1.NodeClaimStatus{
					Phase:      infrav2alpha1.NodeClaimCompleted,
					Attempt:    1,
					Reason:     infrav2alpha1.ClaimReasonNodeDeleted,
					FinishedAt: ptrTime(finished),
				},
			},
			want: &infrav2alpha1.LastClaimSummary{
				Name:       fixSlot + "-a1b2c3",
				Phase:      infrav2alpha1.NodeClaimCompleted,
				Attempt:    1,
				Call:       infrav2alpha1.ClaimCallDeleteNodes,
				Group:      fixGroup,
				Reason:     infrav2alpha1.ClaimReasonNodeDeleted,
				FinishedAt: ptrTime(finished),
			},
		},
		{
			// The pair of instants a scale-down leaves on a claim, and the reason the digest carries
			// both. A DeleteNodes rewrites spec.request of a claim that can be days old and never
			// touches spec.requestedAt, so the episode's request time is the age of the machine and
			// the deletionTimestamp is the instant the pool shrank: a display that reads the first for
			// a shrink dates "a machine taken back 24 minutes ago" by its boot, three days ago.
			name:     "a scale-down that named a claim days after the request that made it",
			wantSeed: 1,
			claim: &infrav2alpha1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              fixSlot + "-9d8e7f",
					DeletionTimestamp: ptrTime(scaledDown),
				},
				Spec: infrav2alpha1.NodeClaimSpec{
					SlotRef:     corev1.ObjectReference{Name: fixSlot},
					RequestedAt: metav1.NewTime(requested),
					RequestedBy: RequestedByAutoscaler,
					Request:     deleteNodes(fixK8sNode),
				},
				Status: infrav2alpha1.NodeClaimStatus{
					Phase:      infrav2alpha1.NodeClaimCompleted,
					Attempt:    1,
					Reason:     infrav2alpha1.ClaimReasonNodeDeleted,
					FinishedAt: ptrTime(finished),
				},
			},
			want: &infrav2alpha1.LastClaimSummary{
				Name:         fixSlot + "-9d8e7f",
				Phase:        infrav2alpha1.NodeClaimCompleted,
				Attempt:      1,
				Call:         infrav2alpha1.ClaimCallDeleteNodes,
				Group:        fixGroup,
				Reason:       infrav2alpha1.ClaimReasonNodeDeleted,
				RequestedAt:  ptrTime(requested),
				ScaledDownAt: ptrTime(scaledDown),
				FinishedAt:   ptrTime(finished),
			},
		},
		{
			// The claim controller keeps the digest in step with a live claim, so a slot whose
			// controller died still reports the attempt it was on.
			name:     "a live claim, stamped while it runs",
			wantSeed: 4,
			claim: &infrav2alpha1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: fixSlot + "-d4e5f6"},
				Spec:       infrav2alpha1.NodeClaimSpec{Request: increaseSize(1)},
				Status:     infrav2alpha1.NodeClaimStatus{Phase: infrav2alpha1.NodeClaimBooting, Attempt: 3},
			},
			want: &infrav2alpha1.LastClaimSummary{
				Name:    fixSlot + "-d4e5f6",
				Phase:   infrav2alpha1.NodeClaimBooting,
				Attempt: 3,
				Call:    infrav2alpha1.ClaimCallIncreaseSize,
				Group:   fixGroup,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Digest(tt.claim)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantSeed, SeedAttempt(got),
				"the record of the episode that ended is the whole input of the next attempt")
		})
	}
}

// ptrTime is a metav1.Time pointer, for the spec and status fields that are optional times.
func ptrTime(t time.Time) *metav1.Time {
	out := metav1.NewTime(t)

	return &out
}

// scheme is the scheme of the v2alpha1 types, the one the provider and the controllers run with.
func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, infrav2alpha1.AddToScheme(s))

	return s
}
