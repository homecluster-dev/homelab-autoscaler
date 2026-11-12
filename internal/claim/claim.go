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

// Package claim is the arithmetic of NodeClaim objects: what a request costs, which slots are
// free, how many units a group is paying for, and what the slot keeps after the claim is
// collected.
//
// It is the package that makes "desired power is derived, never stored" a computation. Every
// caller that needs to know whether a machine is wanted asks this package, and nothing asks a
// spec field of the slot: in v1alpha1 the same questions were answered by reading three fields
// written by four controllers (spec.powerState, status.powerState, status.progress, and the
// three-field scan NodeGroupIncreaseSize did to find a free slot).
package claim

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// Labels carried by every claim, so a slot's claims are one list away (`kubectl get nodeclaim
// -l infra.homecluster.dev/node=<slot>`) and a pool's are one selector
// (`-l group=<group>`). The label keys are the ones the lifecycle Jobs already use
// (internal/fsm/types.go), so a slot's claims and its Jobs are read with the same commands.
const (
	LabelSlot  = "infra.homecluster.dev/node"
	LabelGroup = "group"

	// RequestedByAutoscaler is the free-text identity the gRPC provider stamps on a claim it
	// creates. It is not parsed, it is read.
	RequestedByAutoscaler = "cluster-autoscaler"

	// DefaultTTLSecondsAfterFinished is how long a terminal claim survives. Claims are the record
	// of one scale-up episode; the slot keeps the digest, so the object is collectable, and a
	// homelab pool that scales twice a day must not accumulate objects forever.
	DefaultTTLSecondsAfterFinished = int32(3600)

	// MaxConsecutiveAttempts is the bound the v1alpha1 fsm.StartupAttemptLimit and
	// fsm.ShutdownAttemptLimit conditions carried on the long-lived slot. It lives on the request
	// now: a claim's status.attempt is seeded from the slot's digest, and a claim whose attempt
	// crosses this bound is failed before it buys a machine. The streak ends with the object that
	// carries it, which is why there is no reset heuristic (docs/design/nodeclaim.md §4).
	MaxConsecutiveAttempts = int32(3)
)

// AttemptBudget is the operator's budget for one attempt, measured from spec.requestedAt. It is
// the sum of every deadline one attempt can consume, plus one sleep at the ceiling: the deadline of
// a claim is what enforces it, and it has to be longer than the attempt it bounds.
//
//	startup Job timeout + job monitor poll + registration wait + readiness gate + margin
//	DefaultJobTimeout (5m) + JobMonitorPollInterval (30s) + DefaultUncordonTimeout (90s)
//	  + DefaultReadinessGateTimeout (4m) + MaxUncordonRetryInterval (10s) + 1m = 12m10s
//
// The startup Job's own timeout is in the sum because the clock runs from `requestedAt`, i.e. from
// before the Job exists: one attempt can spend the Job's whole 5m (JobTimeout), then the 90s it takes
// the node to register, then the 4m of the readiness gate - 10m30s of deadlines - and a budget
// of 7m10s kills the claim in the middle of the machine it is still buying. The 0.1.20 progress
// watchdog excluded the Job timeout for exactly this reason: its grace applied only to a Job that had
// already finished (internal/progress/watchdog.go, deleted with this model), and a claim's deadline
// has no such restriction to inherit.
//
// The test in this package computes the number from the fsm constants and pins the set they are, so
// growing a deadline, or dropping one out of the sum, fails.
func AttemptBudget() time.Duration {
	return fsm.DefaultJobTimeout +
		fsm.JobMonitorPollInterval +
		fsm.DefaultUncordonTimeout +
		fsm.DefaultReadinessGateTimeout +
		fsm.MaxUncordonRetryInterval +
		time.Minute
}

// BudgetConstants is the set AttemptBudget is built from, named: the test asserts the sum of exactly
// these, so a constant that quietly leaves the budget leaves the set with it.
func BudgetConstants() map[string]time.Duration {
	return map[string]time.Duration{
		"startup Job timeout":      fsm.DefaultJobTimeout,
		"job monitor poll":         fsm.JobMonitorPollInterval,
		"registration wait":        fsm.DefaultUncordonTimeout,
		"readiness gate":           fsm.DefaultReadinessGateTimeout,
		"uncordon retry ceiling":   fsm.MaxUncordonRetryInterval,
		"one sleep at the ceiling": time.Minute,
	}
}

// MachineGoneGrace is how long the observation "the machine of a ready claim is off" has to hold
// before the claim is failed with MachineGone. It is one boot window (AttemptBudget), deliberately:
// a reboot, a kubelet restarting after an upgrade, a machine a provider is slow to report, all show
// the same shape for minutes, and a model that fails a serving claim on a single observation would
// scale down a machine that is fine. A machine that stays off for longer than it takes to buy a new
// one is gone, and the claim that asked for it is the object that says so.
func MachineGoneGrace() time.Duration {
	return AttemptBudget()
}

// DeadlineAt is spec.deadlineAt for a request made at requestedAt for a group with the given
// spec.maxNodeProvisionTime: the operator's budget, floored by the group's number.
//
// The floor is not decoration. cluster-autoscaler is told the group's number as its own scale-up
// termination deadline (NodeGroupGetOptions -> MaxNodeProvisionDuration), so it gives up on the
// slot at that instant; enforcing the shorter of the two lets the provider declare a claim dead
// while the autoscaler is still waiting for it, and the autoscaler's answer to a group that
// stopped growing is another IncreaseSize.
func DeadlineAt(requestedAt time.Time, groupMaxNodeProvisionTime *metav1.Duration) time.Time {
	budget := AttemptBudget()
	if groupMaxNodeProvisionTime != nil && groupMaxNodeProvisionTime.Duration > budget {
		budget = groupMaxNodeProvisionTime.Duration
	}

	return requestedAt.Add(budget)
}

// Budget returns the group's maxNodeProvisionTime in force for a claim, i.e. the floor of the
// claim's deadline, so the number can be copied onto the claim for reporting.
func Budget(group *infrav2alpha1.Group) *metav1.Duration {
	if group == nil || group.Spec.MaxNodeProvisionTime == nil {
		return nil
	}

	d := *group.Spec.MaxNodeProvisionTime

	return &d
}

// ShutdownAttemptResetGrace is how long a spent scale-down streak keeps refusing the shutdown. Past
// it, the count restarts and the shutdown fires again: a pool whose shutdown script was broken and
// has been fixed is picked up by a later pass, without a hand edit, and the bound keeps its teeth
// against a script that fails every time (30m is 3 attempts of a machine the provider keeps reporting
// up, at the resync the refusal paces it at).
//
// The escape hatch for an operator who does not want to wait for the grace is documented in
// docs/design/nodeclaim.md §4: zero the counter on the claim's status subresource, and the next pass
// fires the shutdown.
const ShutdownAttemptResetGrace = 30 * time.Minute

// ScaleDownStreakSpent reports that a scale-down claim has used its shutdown attempts on a machine
// the provider still observes up, and returns how long is left of the reset grace. Past the grace the
// streak is not spent any more: the refusal was a bounded pause, not a sentence, and the machine is
// asked to go off again (docs/design/nodeclaim.md §4, docs/architecture/state.md).
//
// The count is status.shutdownAttempts, and it is a counter of its own: status.attempt counts the
// machines the request bought, and a shutdown that will not land must not spend the boot attempts of
// the episode it is draining.
func ScaleDownStreakSpent(c *infrav2alpha1.NodeClaim) (bool, time.Duration) {
	if c == nil || c.Status.ShutdownAttempts < MaxConsecutiveAttempts {
		return false, 0
	}

	last := time.Time{}
	if c.Status.LastShutdownAttemptAt != nil {
		last = c.Status.LastShutdownAttemptAt.Time
	}

	if last.IsZero() {
		// A counter with no clock is a counter that started now: refusing for the whole grace on the
		// strength of a writer that stamped no time is the wrong direction of safe.
		last = time.Now()
	}

	left := ShutdownAttemptResetGrace - time.Since(last)
	if left <= 0 {
		return false, 0
	}

	return true, left
}

// ScaleDownStreakResetDue reports that a scale-down streak is spent and its reset grace has passed:
// the pass that fires the shutdown again is the one the pause ends on, and the record of the pause
// ending belongs to it (the count itself restarts with the Job that pass creates, at
// BumpScaleDownAttempt). A counter with no clock is treated as started now, as in ScaleDownStreakSpent,
// so a streak that stamped no time is never reported as already reset.
func ScaleDownStreakResetDue(c *infrav2alpha1.NodeClaim) bool {
	if c == nil || c.Status.ShutdownAttempts < MaxConsecutiveAttempts {
		return false
	}

	spent, _ := ScaleDownStreakSpent(c)

	return !spent
}

// SeedAttempt is the attempt number a new claim of a slot carries: the slot's last attempt + 1
// while the slot keeps failing, and 1 again once an attempt has landed.
//
// "A bound on attempts is a bound on consecutive claims" (docs/design/nodeclaim.md §4), and the
// digest's phase is the record of what the last one did: an episode that ended in ready or
// completed bought a machine the pool could use, so the next request of that slot starts a new
// streak. That is the whole reset rule - it reads the object that ends with the episode, so
// nothing on the slot accumulates and no slot is sentenced for the failures of its life.
func SeedAttempt(last *infrav2alpha1.LastClaimSummary) int32 {
	if last == nil {
		return 1
	}

	if last.Attempt < 1 {
		return 1
	}

	switch last.Phase {
	case infrav2alpha1.NodeClaimReady, infrav2alpha1.NodeClaimCompleted:
		return 1
	default:
		return last.Attempt + 1
	}
}

// New builds a claim for a slot: attribution, deadline, seeded attempt, labels, and an
// ownerReference on the slot so that deleting a slot from Git takes its claims with it.
//
// attempt is not set here: the claim controller seeds status.attempt on the claim's first pass
// (see SeedAttempt) because the seeding reads the slot's digest, and a claim created from a stale
// cache must not bake a number in that the controller cannot correct.
func New(slot *infrav2alpha1.Node, group *infrav2alpha1.Group, req infrav2alpha1.ClaimRequest,
	requestedBy string, scheme *runtime.Scheme,
) (*infrav2alpha1.NodeClaim, error) {
	requestedAt := time.Now()
	ttl := DefaultTTLSecondsAfterFinished

	c := &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", slot.Name, rand.String(5)),
			Namespace: slot.Namespace,
			Labels: map[string]string{
				LabelSlot:  slot.Name,
				LabelGroup: req.Group,
			},
		},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef: corev1.ObjectReference{
				APIVersion: infrav2alpha1.GroupVersion.String(),
				Kind:       "Node",
				Namespace:  slot.Namespace,
				Name:       slot.Name,
				UID:        slot.UID,
			},
			RequestedAt:             metav1.NewTime(requestedAt),
			RequestedBy:             requestedBy,
			Request:                 req,
			DeadlineAt:              deadlineAt(requestedAt, Budget(group)),
			MaxNodeProvisionTime:    Budget(group),
			TTLSecondsAfterFinished: &ttl,
		},
	}

	if err := controllerutil.SetControllerReference(slot, c, scheme); err != nil {
		return nil, fmt.Errorf("setting the ownerReference of the claim of slot %s: %w", slot.Name, err)
	}

	return c, nil
}

// deadlineAt is spec.deadlineAt: the operator's budget, floored by the group's number, measured
// from the moment the request was made.
func deadlineAt(requestedAt time.Time, groupMax *metav1.Duration) *metav1.Time {
	out := metav1.NewTime(DeadlineAt(requestedAt, groupMax))

	return &out
}

// Digest is the one-line record of a claim, copied onto Node.status.lastClaim so that
// `kubectl describe node.infra.homecluster.dev <slot>` still tells the story after the claim has
// been collected by its TTL. The GC is safe because the summary survives it.
func Digest(c *infrav2alpha1.NodeClaim) *infrav2alpha1.LastClaimSummary {
	if c == nil {
		return nil
	}

	summary := &infrav2alpha1.LastClaimSummary{
		Name:                  c.Name,
		Phase:                 c.Status.Phase,
		Attempt:               c.Status.Attempt,
		ShutdownAttempts:      c.Status.ShutdownAttempts,
		LastShutdownAttemptAt: c.Status.LastShutdownAttemptAt,
		Call:                  c.Spec.Request.Call,
		Group:                 c.Spec.Request.Group,
		Reason:                c.Status.Reason,
		DeadlineAt:            c.Spec.DeadlineAt,
		FinishedAt:            c.Status.FinishedAt,
	}

	if !c.Spec.RequestedAt.IsZero() {
		requested := c.Spec.RequestedAt
		summary.RequestedAt = &requested
	}

	// The instant a scale-down named this claim, recorded next to the request's own: the two are not
	// the same moment, and the display of a scale-down has to date the shrink and not the boot it
	// reversed. DeleteNodes rewrites spec.request.call / nodes / requestedBy on a claim that can be
	// days old and leaves requestedAt alone, so a digest of a scale-down is (requestedAt of the
	// episode, scaledDownAt of the request) and the pool's LASTSCALE reads the second.
	if scaled := ScaleDownRequestedAt(c); !scaled.IsZero() {
		stamp := metav1.NewTime(scaled)
		summary.ScaledDownAt = &stamp
	}

	return summary
}

// IsOverdue reports whether a claim's deadline has passed.
func IsOverdue(c *infrav2alpha1.NodeClaim, now time.Time) bool {
	if c == nil || c.Spec.DeadlineAt == nil {
		return false
	}

	return c.Spec.DeadlineAt.Time.Before(now)
}

// SlotNameOf is the slot (Node CR name) a claim asks for. spec.slotRef.name is the field; the
// label is the index, and the two must agree, so the webhook reads this helper.
func SlotNameOf(c *infrav2alpha1.NodeClaim) string {
	return c.Spec.SlotRef.Name
}

// SlotLabel returns the label selector that lists the claims of one slot.
func SlotLabel(slotName string) client.ListOptions {
	return client.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{LabelSlot: slotName}),
	}
}

// SlotKey is the name of the field index the claim controller registers, so a watch-driven
// reconcile can list a slot's claims without scanning the namespace.
const SlotKeyField = "spec.slotRef.name"

// IndexSlot is the field indexer function for SlotKeyField.
func IndexSlot(rawObj client.Object) []string {
	c, ok := rawObj.(*infrav2alpha1.NodeClaim)
	if !ok {
		return nil
	}

	name := SlotNameOf(c)
	if name == "" {
		return nil
	}

	return []string{name}
}

// ListOfSlot returns the claims of one slot, newest first.
func ListOfSlot(ctx context.Context, c client.Client, namespace, slotName string) ([]infrav2alpha1.NodeClaim, error) {
	list := &infrav2alpha1.NodeClaimList{}
	if err := c.List(ctx, list, client.InNamespace(namespace),
		&client.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{LabelSlot: slotName})}); err != nil {
		return nil, fmt.Errorf("listing the claims of slot %s: %w", slotName, err)
	}

	return list.Items, nil
}

// LiveOfSlot returns the live claim of a slot: the newest claim whose phase is live (pending,
// booting, ready). A claim a scale-down marked for deletion is returned too, and it is the claim of
// the slot: the boot it started is committed, and deleting the claim is the request that reverses
// it, so the object stays on the slot until the machine is observed off (docs/design/nodeclaim.md §3).
//
// "Live" is the phase question - does an episode of this slot still own the machine. WantsMachine,
// below, is the stronger question the state machine asks before it boots anything. When more than
// one claim of a slot is live the newest is the one the slot answers for, and the rest are extras
// the claim controller fails: one live claim per slot is load-bearing, it is what makes "a live
// claim exists" a synonym for "the machine is wanted".
func LiveOfSlot(claims []infrav2alpha1.NodeClaim) *infrav2alpha1.NodeClaim {
	var best *infrav2alpha1.NodeClaim

	for i := range claims {
		c := &claims[i]
		if !phase.IsLive(c.Status.Phase) {
			continue
		}

		if best == nil || c.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = c
		}
	}

	return best
}

// WantsMachine reports whether a claim wants a machine on its slot: it is live, and no scale-down
// has named it. A claim marked for deletion is the record of the opposite - the machine is going
// off, and the claim's finalizer is what keeps the request from being forgotten - so a pass that
// would boot the slot must not read a marked claim as a want, and a pass that powers the slot off
// must not lose sight of it. The pair is the difference between "the startup is committed" and
// "the startup is the next thing to do" (docs/design/nodeclaim.md §3, §5).
func WantsMachine(c *infrav2alpha1.NodeClaim) bool {
	return c != nil && phase.IsLive(c.Status.Phase) && c.DeletionTimestamp.IsZero()
}
