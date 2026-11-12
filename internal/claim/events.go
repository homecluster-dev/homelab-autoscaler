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
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// The claim's own timeline, as Kubernetes Events.
//
// A NodeClaim is one attempt: it has a create, a commit, a ready, an end. Before this set, every
// Event the operator published landed on the `Node` (the slot's scale up/down transitions), and
// `kubectl describe nodeclaim` showed a spec, a status and conditions with no timeline: the moment
// an attempt was created, the moment it committed to a boot, and the moment it ended were readable
// only from the timestamps of the conditions, which carry no narrative and no detail.
//
// The set is deliberately small and one-event-per-fact, and it is written at the one chokepoint that
// owns the phase writes (Recorder.Move), so "one event per transition" is a property of the
// structure and not a discipline every caller has to keep:
//
//   - the event is emitted after the status write succeeded, so a move the API server refused, and a
//     re-apply of a move that changed nothing, publish nothing;
//   - an illegal move (ErrIllegalMove) publishes nothing, because it writes nothing;
//   - the reason is the fact, the note carries the claim-side detail (attempt number, Job name,
//     deadline, duration) that makes the event readable on its own.
//
// Events and conditions are two records of the same moves, and they are not interchangeable: the
// conditions are the durable record (`status.conditions` survives as long as the claim does, and the
// slot's digest survives the claim), the Events expire with the cluster's Event TTL (one hour by
// default) and are the timeline you read when the claim is still there. `kubectl describe nodeclaim`
// is the "what just happened" view; the conditions are the "what is true" view.
//
// The reason names are stable and greppable, and every one of them is prefixed Claim, so
//
//	kubectl get events -n <ns> --field-selector involvedObject.kind=NodeClaim
//
// reads as the claim stream, and `kubectl describe nodeclaim <name>` shows the attempt's life.
const (
	// EventClaimCreated: the request exists. Written by the recorder's attempt seed (the first
	// write the claim controller performs on a fresh claim), so it lands for every creator - the
	// gRPC provider, a hand-applied manual claim, a replay - and carries the attribution the spec
	// was created with.
	EventClaimCreated = "ClaimCreated"
	// EventClaimBooting: pending -> booting, the boot is committed and the claim is no longer
	// cancelable.
	EventClaimBooting = "ClaimBooting"
	// EventClaimReady: booting -> ready, the readiness gate answered for this attempt.
	EventClaimReady = "ClaimReady"
	// EventClaimFailed: any move to failed, quoting status.reason and the number behind it.
	EventClaimFailed = "ClaimFailed"
	// EventClaimCancelled: a live claim a caller cancelled (DecreaseTargetSize, DeleteNodes, a
	// delete by hand) reached its end. Normal, not Warning: the cancel is the request landing.
	EventClaimCancelled = "ClaimCancelled"
	// EventClaimCompleted: a move to completed with no cancel behind it: the machine the attempt
	// asked for is observed off, and the episode is over.
	EventClaimCompleted = "ClaimCompleted"
	// EventClaimSuperseded: a move to failed for the reason Superseded. It is not a Warning: the
	// model has not broken, one live claim per slot is load-bearing and the extra one is the claim
	// that ends (docs/development/nodeclaim.md §3).
	EventClaimSuperseded = "ClaimSuperseded"

	// ActionClaim is the action every event of this set carries. The new events API
	// (k8s.io/client-go/tools/events) keys its aggregation on the object, the type, the reason and
	// the action, so repeats of the same fact on the same claim coalesce into one Event object with
	// a count and a lastTimestamp instead of a new object per pass: no event storm out of a resync
	// loop, and `kubectl get events -o wide` names what the claim controller did.
	ActionClaim = "nodeclaim"
)

// eventNoteFormat is the note handed to the new events API: the rendered message goes in as an
// argument, so a Job name, a node name, or a provider payload with a literal '%' in it is never
// re-read as a format string (the same rule internal/fsm/recorder.go states for the slot's events).
const eventNoteFormat = "%s"

// maxEventMessageLength bounds a note. An Event is a real API object kept in etcd for the Event TTL,
// and the detail this set quotes (Job names, node lists, deadlines) is bounded by construction, but
// the truncation is the cheap insurance the slot's events already carry.
const maxEventMessageLength = 512

// eventf publishes one Event on the claim, and is a no-op without a recorder: the nil-safe pattern
// the repository already uses (internal/controller/infra/lifecycle.go's eventf), so every unit test
// that builds a Recorder with NewRecorder - which wires no recorder - records nothing and the phase
// writes it asserts are unaffected.
func (r *Recorder) eventf(nodeClaim *infrav2alpha1.NodeClaim, eventType, reason, message string) {
	if r == nil || r.Events == nil || nodeClaim == nil {
		return
	}

	r.Events.Eventf(nodeClaim, nil, eventType, reason, ActionClaim, eventNoteFormat,
		truncateEventMessage(message))
}

// truncateEventMessage flattens a note onto one line and cuts it at maxEventMessageLength, on rune
// boundaries (the same rule, and the same bound, internal/fsm/recorder.go applies to the slot's
// events: a node name or a provider payload is not ASCII).
func truncateEventMessage(message string) string {
	oneLine := strings.Join(strings.Fields(message), " ")

	runes := []rune(oneLine)
	if len(runes) <= maxEventMessageLength {
		return oneLine
	}

	suffix := []rune(truncatedSuffix)

	return string(runes[:maxEventMessageLength-len(suffix)]) + truncatedSuffix
}

// truncatedSuffix marks a note the recorder cut.
const truncatedSuffix = "... (truncated)"

// recordMove publishes the one Event of one phase move. It runs after the status write landed (see
// Recorder.Move), on the object the write produced, so the attempt number, the reason, and the
// conditions the message quotes are the record as the cluster has it.
//
// The caller has already established that the phase changed: an idempotent re-write, a refused move,
// and a metadata-only write (a finalizer, a cleared deadline) reach no event.
func (r *Recorder) recordMove(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim,
	from, to infrav2alpha1.NodeClaimPhase, reason string,
) {
	if r == nil || r.Events == nil || nodeClaim == nil {
		return
	}

	effective := reason
	if effective == "" {
		effective = nodeClaim.Status.Reason
	}

	eventReason, eventType, message := moveEvent(nodeClaim, from, to, effective, r.factsFor(ctx, nodeClaim, to, effective))

	r.eventf(nodeClaim, eventType, eventReason, message)

	log.FromContext(ctx).V(1).Info("Recorded the claim's event of its move",
		"claim", nodeClaim.Name, "reason", eventReason, "from", string(from), "to", string(to))
}

// recordCreation publishes the ClaimCreated event of a fresh claim, on the write that seeded its
// attempt. That seed is the first thing the claim controller does with a claim object, and it happens
// once, so it is the create the claim stream reports for every writer: the gRPC provider's
// IncreaseSize, a manual claim applied by hand, a replayed request. The attribution it quotes is the
// spec the object was created with, which is the point of the object.
func (r *Recorder) recordCreation(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) {
	if r == nil || r.Events == nil || nodeClaim == nil {
		return
	}

	r.eventf(nodeClaim, corev1.EventTypeNormal, EventClaimCreated, createdEvent(nodeClaim))

	log.FromContext(ctx).V(1).Info("Recorded the claim's creation event", "claim", nodeClaim.Name,
		"attempt", nodeClaim.Status.Attempt)
}

// claimFacts is the detail a claim event quotes that is not a field of the claim: the lifecycle Job
// of the attempt, the Kubernetes node the machine joined as, the newer claim that superseded this
// one. Each is read best-effort - the event is a report, and a fact that cannot be read is quoted as
// the selector that finds it, never as a reason to publish nothing.
type claimFacts struct {
	startupJob   string
	shutdownJob  string
	kubernetes   string
	supersededBy string
}

// factsFor reads only what the event being built quotes, so the chokepoint adds no list to a move
// that needs none (the ready and failed paths, which are the ones an operator reads, are the ones
// that pay for a read).
func (r *Recorder) factsFor(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim,
	to infrav2alpha1.NodeClaimPhase, reason string,
) claimFacts {
	facts := claimFacts{}

	switch to {
	case infrav2alpha1.NodeClaimBooting:
		facts.startupJob = r.lifecycleJobName(ctx, nodeClaim, fsm.PhaseStartup)
	case infrav2alpha1.NodeClaimReady:
		facts.kubernetes = r.kubernetesNodeName(ctx, nodeClaim)
	case infrav2alpha1.NodeClaimCompleted:
		facts.shutdownJob = r.lifecycleJobName(ctx, nodeClaim, fsm.PhaseShutdown)
	case infrav2alpha1.NodeClaimFailed:
		switch reason {
		case infrav2alpha1.ClaimReasonStartupJobFailed, infrav2alpha1.ClaimReasonStartupJobTimeout:
			facts.startupJob = r.lifecycleJobName(ctx, nodeClaim, fsm.PhaseStartup)
		case infrav2alpha1.ClaimReasonShutdownJobFailed, infrav2alpha1.ClaimReasonShutdownJobTimeout:
			facts.shutdownJob = r.lifecycleJobName(ctx, nodeClaim, fsm.PhaseShutdown)
		case infrav2alpha1.ClaimReasonReadinessGateTimeout:
			facts.kubernetes = r.kubernetesNodeName(ctx, nodeClaim)
		case infrav2alpha1.ClaimReasonSuperseded:
			facts.supersededBy = r.supersedingClaimName(ctx, nodeClaim)
		}
	}

	return facts
}

// lifecycleJobName names the lifecycle Job of this attempt: the Jobs the transition creates carry
// the claim's name as a label (fsm.LabelNodeClaim), so the attempt's Job is one list away, and the
// newest one is the Job of this attempt. "" says the Job is not there to read - which is the normal
// shape of the pending -> booting write, because the transition records the commit and creates the
// Job a few lines later (internal/fsm StartNode): the event then quotes the selector that finds it,
// and does not invent a name.
func (r *Recorder) lifecycleJobName(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim, jobPhase string) string {
	if r.Client == nil {
		return ""
	}

	list := &batchv1.JobList{}

	selector := labels.SelectorFromSet(labels.Set{
		fsm.LabelNodeClaim: nodeClaim.Name,
		fsm.LabelPhase:     jobPhase,
	})

	if err := r.Client.List(ctx, list, &client.ListOptions{
		Namespace:     nodeClaim.Namespace,
		LabelSelector: selector,
	}); err != nil {
		// A cluster this recorder cannot read Jobs from (a unit test's client, a cache with no
		// Informer for the kind) is not a reason to publish no event.
		log.FromContext(ctx).V(1).Info("Cannot read the lifecycle Jobs of the claim",
			"claim", nodeClaim.Name, "phase", jobPhase, "error", err.Error())

		return ""
	}

	var newest *batchv1.Job

	for i := range list.Items {
		job := &list.Items[i]
		if newest == nil || job.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = job
		}
	}

	if newest == nil {
		return ""
	}

	return newest.Name
}

// kubernetesNodeName is the node object the attempt's machine joined as, read off the slot it asks
// for (spec.kubernetesNodeName is the slot's field). The fallback is the slot's name, which is what
// the pool's slots are named by in every example of the docs, and it is labelled as such.
func (r *Recorder) kubernetesNodeName(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) string {
	slotName := SlotNameOf(nodeClaim)

	if r.Client == nil {
		return slotName
	}

	slot := &infrav2alpha1.Node{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: nodeClaim.Namespace, Name: slotName}, slot); err != nil {
		return slotName
	}

	if slot.Spec.KubernetesNodeName == "" {
		return slotName
	}

	return slot.Spec.KubernetesNodeName
}

// supersedingClaimName names the live claim that owns the slot this claim is the extra of. The
// Superseded reason says "one live claim per slot"; the name of the claim that won is the fact an
// operator acts on, and it is not on this object.
func (r *Recorder) supersedingClaimName(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) string {
	if r.Client == nil {
		return ""
	}

	claims, err := ListOfSlot(ctx, r.Client, nodeClaim.Namespace, SlotNameOf(nodeClaim))
	if err != nil {
		return ""
	}

	owner := LiveOfSlot(claims)
	if owner == nil || owner.Name == nodeClaim.Name {
		return ""
	}

	return owner.Name
}

// moveEvent is the event table as data: the move, and the one fact it reports. It is a pure function
// over the claim, the edge, the reason and the read-out facts, so the set of events the model
// publishes is testable without a cluster, and a new edge has to be said out loud here before it can
// publish anything.
//
// The messages are written for the claim, not for the slot. The slot's own events
// (internal/fsm: ScaleUpRequested, ScaleUpCompleted, PowerOn / PowerOff) say what the machine of the
// slot did; a claim event says what happened to this attempt, and quotes the attempt-side detail -
// its number, its Job, its deadline, its duration - so the pair reads as two sentences and not as one
// sentence twice.
func moveEvent(nodeClaim *infrav2alpha1.NodeClaim, from, to infrav2alpha1.NodeClaimPhase,
	reason string, facts claimFacts,
) (string, string, string) {
	switch to {
	case infrav2alpha1.NodeClaimBooting:
		return EventClaimBooting, corev1.EventTypeNormal, bootingEvent(nodeClaim, facts)
	case infrav2alpha1.NodeClaimReady:
		return EventClaimReady, corev1.EventTypeNormal, readyEvent(nodeClaim, facts)
	case infrav2alpha1.NodeClaimFailed:
		if reason == infrav2alpha1.ClaimReasonSuperseded {
			return EventClaimSuperseded, corev1.EventTypeNormal, supersededEvent(nodeClaim, facts)
		}

		return EventClaimFailed, corev1.EventTypeWarning, failedEvent(nodeClaim, from, reason, facts)
	case infrav2alpha1.NodeClaimCompleted:
		if isCancellation(nodeClaim, reason) {
			return EventClaimCancelled, corev1.EventTypeNormal, cancelledEvent(nodeClaim, from, reason, facts)
		}

		return EventClaimCompleted, corev1.EventTypeNormal, completedEvent(nodeClaim, from, reason, facts)
	default:
		// A write of a live phase that is not a move (a re-applied pending stamp, a hand edit) has no
		// fact of its own to report, and publishes nothing.
		return "", corev1.EventTypeNormal, ""
	}
}

// isCancellation says a claim that ended completed ended because a caller asked for the machine back:
// the three reasons the scale-down paths write (the provider's DecreaseTargetSize and DeleteNodes,
// and the finalizer path of a claim deleted by hand). Every other completed end is the episode
// closing on its own - a manual claim whose machine was handed over - and reads as ClaimCompleted.
func isCancellation(nodeClaim *infrav2alpha1.NodeClaim, reason string) bool {
	if reason == "" {
		// No reason on the write, and the request that made the claim is a scale-down: the provider
		// recorded the call on the spec, and that is the caller this event names.
		return nodeClaim.Spec.Request.Call == infrav2alpha1.ClaimCallDeleteNodes ||
			nodeClaim.Spec.Request.Call == infrav2alpha1.ClaimCallDecreaseTargetSize
	}

	switch reason {
	case infrav2alpha1.ClaimReasonCancelledByDecreaseTargetSize,
		infrav2alpha1.ClaimReasonNodeDeleted,
		infrav2alpha1.ClaimReasonDeletedByHand:
		return true
	default:
		return false
	}
}

// createdEvent is the record of a request existing: who asked, through which RPC, for what, with what
// budget, and which attempt of the slot it is.
func createdEvent(nodeClaim *infrav2alpha1.NodeClaim) string {
	return fmt.Sprintf("claim %s created for slot %s: %s (attempt %d, %s)",
		nodeClaim.Name, SlotNameOf(nodeClaim), requestSummary(nodeClaim),
		nodeClaim.Status.Attempt, deadlineSummary(nodeClaim))
}

// bootingEvent is the pending -> booting edge: the commit. The startup Job is named when it exists to
// be read (a re-applied move, a Job created by an earlier pass); at the write that records the commit
// in the normal path the Job is created a few lines later by StartNode, so the event quotes the
// label selector that resolves it, and does not invent a name.
func bootingEvent(nodeClaim *infrav2alpha1.NodeClaim, facts claimFacts) string {
	job := facts.startupJob
	if job == "" {
		job = fmt.Sprintf("its startup Job (kubectl get jobs -n %s -l %s=%s)",
			nodeClaim.Namespace, fsm.LabelNodeClaim, nodeClaim.Name)
	} else {
		job = fmt.Sprintf("startup Job %s", job)
	}

	return fmt.Sprintf("%s is committed: %s exists and the power-on has been asked for, so this boot is no longer cancelable - a delete reverses it through the finalizer, it does not interrupt it (%s)",
		attemptSummary(nodeClaim), job, deadlineSummary(nodeClaim))
}

// readyEvent is the readiness gate answering for the attempt: the node it asked for is usable, and
// the boot window is closed (the same write cleared spec.deadlineAt).
func readyEvent(nodeClaim *infrav2alpha1.NodeClaim, facts claimFacts) string {
	return fmt.Sprintf("%s is ready: kubernetes node %s passed the readiness gate - Ready, free of the pool's readiness taints, uncordoned - %s after the request; the boot window is closed and the machine can take the pool's work",
		attemptSummary(nodeClaim), facts.kubernetes, sinceRequest(nodeClaim))
}

// failedEvent is any move to failed, quoting the reason the model recorded and the number behind it.
func failedEvent(nodeClaim *infrav2alpha1.NodeClaim, from infrav2alpha1.NodeClaimPhase,
	reason string, facts claimFacts,
) string {
	return fmt.Sprintf("%s failed from %s, reason %s: %s",
		attemptSummary(nodeClaim), string(from), namedReason(reason), failureDetail(nodeClaim, reason, facts))
}

// cancelledEvent is a live claim a caller asked back, and the end of the attempt that request bought.
func cancelledEvent(nodeClaim *infrav2alpha1.NodeClaim, from infrav2alpha1.NodeClaimPhase,
	reason string, facts claimFacts,
) string {
	committed := phase.IsLive(from) && from != infrav2alpha1.NodeClaimPending

	end := "the machine is observed off and the slot is free"
	if !committed {
		end = "no machine was bought, and the slot is free"
	}

	return fmt.Sprintf("%s was cancelled from %s by %s (reason %s): %s, the attempt lasted %s%s",
		attemptSummary(nodeClaim), string(from), requestSummary(nodeClaim), namedReason(reason), end,
		sinceRequest(nodeClaim), shutdownDetail(facts))
}

// completedEvent is a claim whose episode ended with the machine it asked for available: the
// observation says it is off, and the record says how long the attempt lasted.
func completedEvent(nodeClaim *infrav2alpha1.NodeClaim, from infrav2alpha1.NodeClaimPhase,
	reason string, facts claimFacts,
) string {
	return fmt.Sprintf("%s ended completed from %s: the machine it asked for is observed off, %s after the request (%s, reason %s)%s",
		attemptSummary(nodeClaim), string(from), sinceRequest(nodeClaim), requestSummary(nodeClaim),
		namedReason(reason), shutdownDetail(facts))
}

// supersededEvent is the extra live claim of a slot ending. The newer claim's name is the fact the
// operator acts on: it is the request that owns the machine now.
func supersededEvent(nodeClaim *infrav2alpha1.NodeClaim, facts claimFacts) string {
	owner := facts.supersededBy
	if owner == "" {
		owner = fmt.Sprintf("a newer live claim of slot %s", SlotNameOf(nodeClaim))
	}

	return fmt.Sprintf("%s is superseded: %s owns the slot, one live claim per slot is load-bearing, so this attempt ends failed and anything it started is shut down",
		attemptSummary(nodeClaim), owner)
}

// failureDetail is the concrete number behind each reason the FSM and the claim controller can
// record on a move to failed. The reason alone is a label; an operator reading
// `kubectl describe nodeclaim` to answer "why did this attempt fail" needs the value that closed it
// - the deadline that passed, the Job that died, the bound the streak hit, the observation that
// outlasted its grace.
func failureDetail(nodeClaim *infrav2alpha1.NodeClaim, reason string, facts claimFacts) string {
	next := fmt.Sprintf("the next claim of the slot is attempt %d", nodeClaim.Status.Attempt+1)

	switch reason {
	case infrav2alpha1.ClaimReasonDeadlineExceeded:
		return fmt.Sprintf("the boot window closed at %s, %s of budget from the request; the machine is force-shut-down, the slot is free, and %s",
			renderTime(nodeClaim.Spec.DeadlineAt), budgetOf(nodeClaim).String(), next)

	case infrav2alpha1.ClaimReasonStartupJobFailed:
		return fmt.Sprintf("the startup Job %s exited non-zero: the machine this attempt bought is shut down, the slot is free, and %s",
			namedJob(facts.startupJob, fsm.PhaseStartup, nodeClaim), next)

	case infrav2alpha1.ClaimReasonStartupJobTimeout:
		return fmt.Sprintf("the startup Job %s ran out of time (the Job's own timeout is %s, inside the boot window of %s): the machine is shut down, the slot is free, and %s",
			namedJob(facts.startupJob, fsm.PhaseStartup, nodeClaim),
			fsm.DefaultJobTimeout.String(), budgetOf(nodeClaim).String(), next)

	case infrav2alpha1.ClaimReasonShutdownJobFailed:
		return fmt.Sprintf("the shutdown Job %s of the scale-down exited non-zero, so the machine this claim owns is still up: the input law keeps powering it off, and the streak is counted on this claim (%d so far)",
			namedJob(facts.shutdownJob, fsm.PhaseShutdown, nodeClaim), nodeClaim.Status.ShutdownAttempts)

	case infrav2alpha1.ClaimReasonShutdownJobTimeout:
		return fmt.Sprintf("the shutdown Job %s of the scale-down ran out of time, so the machine this claim owns is still up: the input law keeps powering it off, and the streak is counted on this claim (%d so far)",
			namedJob(facts.shutdownJob, fsm.PhaseShutdown, nodeClaim), nodeClaim.Status.ShutdownAttempts)

	case infrav2alpha1.ClaimReasonReadinessGateTimeout:
		return fmt.Sprintf("the machine booted and joined, but kubernetes node %s never cleared the readiness gate within %s (Ready, and free of the pool's readiness taints): it is uncordoned on the way out and shut down, and %s",
			facts.kubernetes, fsm.DefaultReadinessGateTimeout.String(), next)

	case infrav2alpha1.ClaimReasonAttemptLimitExceeded:
		return fmt.Sprintf("the slot has spent %d consecutive attempts and the bound is %d, so no machine is bought for it until one lands; no Job is created, and %s",
			nodeClaim.Status.Attempt, MaxConsecutiveAttempts, next)

	case infrav2alpha1.ClaimReasonMachineGone:
		return fmt.Sprintf("the provider has observed the machine off for %s, longer than one boot window %s, so the machine is gone and not rebooting: the claim is released, the slot is free, and %s",
			observedOffFor(nodeClaim), MachineGoneGrace().String(), next)

	case infrav2alpha1.ClaimReasonStuckTransition:
		return fmt.Sprintf("the lifecycle transition of the attempt was stuck past %s and was force-cleaned: the slot is back in a stable phase, and %s",
			fsm.StuckTransitionThreshold.String(), next)

	case infrav2alpha1.ClaimReasonSlotDeleted:
		return fmt.Sprintf("the slot %s is gone and the machine it named is going with it: the finalizer is released and this claim is its own record for as long as its TTL keeps it",
			SlotNameOf(nodeClaim))

	case infrav2alpha1.ClaimReasonSuperseded:
		// moveEvent routes this reason to supersededEvent; the row exists so a caller that asks for
		// the detail of the reason never gets an empty string.
		return fmt.Sprintf("%s is the live claim of the slot, and one live claim per slot is load-bearing",
			SlotNameOf(nodeClaim))

	case "":
		return fmt.Sprintf("no reason was recorded on the move; the machine this attempt started is shut down, the slot is free, and %s", next)

	default:
		return fmt.Sprintf("the attempt ends here: the machine it started is shut down, the slot is free, and %s", next)
	}
}

// shutdownDetail is the scale-down half of an ending that landed: the Job that powered the machine
// off, when a Job exists to name. It is appended to the completed and cancelled events, where "the
// machine is observed off" is a claim about an observation and the Job is the object that ran it.
func shutdownDetail(facts claimFacts) string {
	if facts.shutdownJob == "" {
		return ""
	}

	return fmt.Sprintf(", powered off by shutdown Job %s", facts.shutdownJob)
}

// namedJob is a Job name, or the selector that finds the Jobs of this claim when the Job is not
// readable from here (a Job the TTL already collected, a cache that cannot answer).
func namedJob(name, jobPhase string, nodeClaim *infrav2alpha1.NodeClaim) string {
	if name != "" {
		return name
	}

	return fmt.Sprintf("(the %s Job of the attempt: kubectl get jobs -n %s -l %s=%s)",
		jobPhase, nodeClaim.Namespace, fsm.LabelNodeClaim, nodeClaim.Name)
}

// attemptSummary is the claim-side subject of every event: which attempt of which slot this is. The
// slot's events name the machine; these name the request.
func attemptSummary(nodeClaim *infrav2alpha1.NodeClaim) string {
	return fmt.Sprintf("attempt %d of slot %s (claim %s)",
		nodeClaim.Status.Attempt, SlotNameOf(nodeClaim), nodeClaim.Name)
}

// requestSummary is the attribution of the claim: the RPC that asked, the arguments it asked with,
// the group, and the caller's identity. It is spec.request read out loud, and it is the answer to
// "who asked for this, and with what".
func requestSummary(nodeClaim *infrav2alpha1.NodeClaim) string {
	req := nodeClaim.Spec.Request

	parts := []string{"call " + namedCall(req.Call)}

	switch {
	case len(req.Nodes) > 0:
		parts = append(parts, fmt.Sprintf("nodes %s", strings.Join(req.Nodes, ",")))
	case req.Delta != 0:
		parts = append(parts, fmt.Sprintf("delta %+d", req.Delta))
	}

	parts = append(parts, "group "+req.Group)

	if nodeClaim.Spec.RequestedBy != "" {
		parts = append(parts, "by "+nodeClaim.Spec.RequestedBy)
	}

	return strings.Join(parts, ", ")
}

// deadlineSummary is the budget of the attempt as the claim carries it.
func deadlineSummary(nodeClaim *infrav2alpha1.NodeClaim) string {
	if nodeClaim.Spec.DeadlineAt == nil {
		return "no deadline recorded"
	}

	return fmt.Sprintf("deadline %s, budget %s from the request",
		renderTime(nodeClaim.Spec.DeadlineAt), budgetOf(nodeClaim).String())
}

// budgetOf is the budget in force for the attempt: the group's spec.maxNodeProvisionTime as copied
// onto the claim, and the operator's claim.AttemptBudget for a pool that declared no number.
func budgetOf(nodeClaim *infrav2alpha1.NodeClaim) time.Duration {
	if nodeClaim.Spec.MaxNodeProvisionTime != nil {
		return nodeClaim.Spec.MaxNodeProvisionTime.Duration
	}

	return AttemptBudget()
}

// sinceRequest is how long the attempt has been running, measured from spec.requestedAt: the number
// an operator compares against the budget.
func sinceRequest(nodeClaim *infrav2alpha1.NodeClaim) string {
	if nodeClaim.Spec.RequestedAt.IsZero() {
		return "an unrecorded time"
	}

	elapsed := time.Since(nodeClaim.Spec.RequestedAt.Time)
	if elapsed < 0 {
		elapsed = 0
	}

	return elapsed.Round(time.Second).String()
}

// observedOffFor is how long the loss of the claim's machine has been observed - the clock of the
// MachineGone bound, read off the MachineObservedOff condition the recorder stamps.
func observedOffFor(nodeClaim *infrav2alpha1.NodeClaim) string {
	first, ok := MachineLossObservedSince(nodeClaim)
	if !ok {
		return "at least one boot window"
	}

	age := time.Since(first)
	if age < 0 {
		age = 0
	}

	return age.Round(time.Second).String()
}

// namedCall is the RPC of a request, spelled when the spec carries one.
func namedCall(call string) string {
	if call == "" {
		return "no recorded call"
	}

	return call
}

// namedReason is a status.reason, spelled when a writer stamped none.
func namedReason(reason string) string {
	if reason == "" {
		return "no recorded reason"
	}

	return reason
}

// renderTime is a metav1.Time in the form an operator reads in an event: UTC, RFC 3339.
func renderTime(at *metav1.Time) string {
	if at == nil || at.IsZero() {
		return "unset"
	}

	return at.Time.UTC().Format(time.RFC3339)
}
