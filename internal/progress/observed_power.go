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

// This file is the second rule of the package, and it is the mirror of the one in watchdog.go.
//
// watchdog.go settles a *claim that produced nothing*: status.progress names a machine and the
// cluster has none. This file corrects a *machine that outlived its recorded state*: the cluster has
// positively observed a machine up and status.progress says there is none.
//
// The shape comes from a live cluster (0.1.21): a slot at {spec.desiredPowerState: off,
// status.powerState: on, status.progress: shutdown}, its Hetzner server up and serving a pod, its
// claim dropped. Nothing in a reconcile heals it.
//
//   - The FSM derives its current state from status.progress (getCurrentState), so the slot is
//     StateShutdown; the only event legal from there is StartNode, and the claim is off, so the
//     node controller's branch is dead. It logs "Node is in desired state or transitioning" and
//     does nothing - 40 minutes of it, on a machine that is burning money. ShutdownNode is
//     StateReady -> StateShuttingDown, so the one operation that would satisfy the claim is
//     unreachable from the progress the slot carries.
//   - core.applyObservedPowerState, the writer that owns a slot with a node object, refuses to
//     correct a progress from an observed on unless spec.desiredPowerState is on. That refusal is
//     deliberate (the doc on it spells out the duplicate-shutdown race in the node-monitor-grace
//     window), and it makes the pair stable: an operator, or GitOps, can leave it there forever.
//   - internal/progress.Settle, the watchdog, never looks at it: its gate wants a progress that
//     implies a machine and no corev1.Node, and this slot has a Ready node.
//
// So a record of what the FSM *last did* (progress: shutdown, written by the completion of a
// shutdown Job whose exit code was the only evidence) outranked what the provider *observes*
// (status.powerState: on, written from the node's Ready condition). The invariant this rule
// enforces: a slot must never rest with an observed power that contradicts its FSM state.
//
// The rule, in one sentence: when status.powerState is on, the slot's corev1.Node reports Ready,
// and status.progress implies no machine (shutdown, shuttingdown, and the empty progress of a slot
// that was never seeded, all of which getCurrentState reads as StateShutdown), then the observation
// wins and the progress is corrected to ready. What happens next is the FSM's, not this rule's:
//
//   - spec.desiredPowerState off - the machine is up and unwanted: the slot is now StateReady, so
//     the very same pass fires ShutdownNode, with its coordination lock, its cordon and its
//     shutdown Job. No grace is waited for here, because the claim is already off and every minute
//     of the slot's life is a billed minute; the deferral this rule does honour is the one the
//     watchdog honours (see jobWindow), which is what keeps it from stomping the ~40s window in
//     which a shutdown that just landed leaves the node object Ready.
//   - spec.desiredPowerState on - the machine is up and wanted: the progress is a lie about a
//     machine that is not there, and the correction is the observed value. No lifecycle operation
//     follows from it (the FSM does not start a machine that is already on), and
//     core.applyObservedPowerState writes the same value on its own pass; this rule is what makes
//     the precedence explicit, in one place, and runs it before the FSM branch reads the progress.
//
// It writes one status field, on the subresource, through the same read-fresh, re-decide,
// retry-on-conflict shape as the watchdog, and it updates the caller's copy in place, so the pass
// that corrected the slot runs its FSM branch on the corrected value and not on the one it
// replaced. The correction is self-limiting twice over: the progress it writes is outside the set
// the rule reads, so a second pass writes nothing and records nothing, and a shutdown it releases
// moves the slot to shuttingdown behind a coordination lock the rule refuses.

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
)

// ReasonMachineObservedUp is recorded on the Node CR when the observed power of a slot overruled
// its recorded progress. One reason for both claims: what the operator has to read is that the
// machine and the slot's own record disagreed, and the note names the progress it replaced, the
// observation that outranked it, and what the controller did about it.
const ReasonMachineObservedUp = "MachineObservedUp"

// ActionCorrectProgress is the event action of that correction: the controller changed what the
// slot says about the machine, it did not power anything itself.
const ActionCorrectProgress = "CorrectProgress"

// ActionCorrectToReady is what one pass of this rule decides to do: write the progress the
// observation says (ready), leaving status.powerState - the observation itself - untouched.
const ActionCorrectToReady Action = "correcttoready"

// ReconcileObservedPower corrects a status.progress that the observed power of the slot
// contradicts, and reports what it did. It runs in the node controller's FSM path, before any
// branch reads status.progress, next to Settle (see internal/progress/watchdog.go for the rule
// that settles the mirror shape, and the file's doc comment for the split between the two).
//
// The gate, in the order the evidence is read:
//
//   - status.progress must imply no machine: shutdown, shuttingdown, or the empty progress of a
//     slot that was never seeded. Those are exactly the values getCurrentState maps to StateShutdown,
//     which is the state that makes ShutdownNode unreachable. ready and startingup are the
//     watchdog's and the operation's, and are not touched here.
//   - status.powerState must be on, and the slot's corev1.Node must report Ready. Both are the same
//     observation (core.getNodeStatus maps NodeReady=True onto on), and requiring the live node
//     object is what keeps this rule off the shapes the watchdog owns: a slot whose node object is
//     gone is a slot with no machine, whatever its last recorded power state says, and NotReady is a
//     weak observation of a machine (see kubernetesNode).
//   - no lifecycle Job of the slot may be in flight, and one that finished must be older than the
//     settle grace. This is the watchdog's gate, shared verbatim (jobWindow), for the same reason:
//     a shutdown Job that completed seconds ago has a state machine that is about to record the
//     progress it owns, and the node object of a machine that has just been powered off stays Ready
//     for node-monitor-grace-period (~40s). Correcting in that window is the duplicate shutdown Job
//     core.applyObservedPowerState refuses to write for.
//   - no live coordination lock: an operation owns the slot.
//
// A write that goes through records exactly one event, Warning for a slot that is claimed off (a
// machine is up, wanted by nobody, and paid for: the shutdown the correction releases is the fix,
// and the operator should see the contradiction) and Normal for a slot that is claimed on (the
// progress was the only thing wrong, the machine is the one the pool asked for, and the system
// corrected a record). The claim is never written: spec.desiredPowerState is the request, and
// dropping it stays the autoscaler's arithmetic, as it is for the watchdog.
//
// A nil recorder records nothing, which is what the unit tests use. grace <= 0 selects
// SettleGracePeriod.
func ReconcileObservedPower(ctx context.Context, c client.Client, slot *infrav1alpha1.Node, recorder events.EventRecorder, grace time.Duration) (Outcome, error) {
	if grace <= 0 {
		grace = SettleGracePeriod
	}

	logger := log.FromContext(ctx).WithValues("node", slot.Name)
	coord := fsm.NewCoordinationManager(c, fsm.NodeControllerOwner)

	if !impliesNoMachine(slot.Status.Progress) {
		return Outcome{Reason: fmt.Sprintf("status.progress %q does not contradict an observed on", slot.Status.Progress)}, nil
	}

	obs, err := observe(ctx, c, coord, slot, grace)
	if err != nil {
		// The API server is unreachable, the slot is gone, ... Nothing was written, and the caller
		// reports the error like it does for the watchdog: not knowing the state of the machine is
		// not evidence to write over a progress.
		return Outcome{}, err
	}

	decision := decideObservedPower(slot, obs)
	if decision.action == ActionNone {
		if obs.lockHeld {
			logger = logger.WithValues("lock", obs.lockNote)
		}

		if decision.requeueAfter > 0 {
			logger.V(1).Info("Leaving a progress that an observed on contradicts to the operation in flight",
				"progress", string(slot.Status.Progress),
				"powerstate", string(slot.Status.PowerState),
				"reason", decision.reason,
				"requeueAfter", decision.requeueAfter.String())

			return Outcome{RequeueAfter: decision.requeueAfter, Reason: decision.reason}, nil
		}

		logger.V(1).Info("Nothing to correct",
			"progress", string(slot.Status.Progress),
			"powerstate", string(slot.Status.PowerState),
			"reason", decision.reason)

		return Outcome{Reason: decision.reason}, nil
	}

	wrote, previous, err := applyObservedPower(ctx, c, coord, slot, decision, grace)
	if err != nil {
		return Outcome{}, err
	}

	if !wrote {
		// The object moved between the decision and the write and no longer qualifies: an
		// operation, an observation, a machine, got there first.
		logger.V(1).Info("The slot stopped qualifying while writing",
			"progress", string(slot.Status.Progress),
			"action", string(decision.action))

		return Outcome{Reason: "the slot stopped qualifying on re-read"}, nil
	}

	outcome := Outcome{Action: decision.action, Previous: previous, Corrected: true}

	// Warning for a machine that is up and unwanted, Normal for one that is up and wanted. The
	// watchdog's split is the same one (a settlement of a slot nobody asked for is a Warning, a
	// re-arm that heals a claim is a Normal), and the shape worth an operator's attention is the
	// one where the pool is paying for a machine its own request says it does not want.
	wanted := slot.Spec.DesiredPowerState == infrav1alpha1.PowerStateOn

	eventType := corev1.EventTypeWarning
	if wanted {
		eventType = corev1.EventTypeNormal
	}

	logger.Info("Corrected a status.progress the observed power of the slot contradicts",
		"previousProgress", string(previous),
		"progress", string(infrav1alpha1.ProgressReady),
		"powerstate", string(slot.Status.PowerState),
		"desiredPowerState", string(slot.Spec.DesiredPowerState),
		"kubernetesNode", kubernetesNodeName(slot),
		"recorded", eventType)

	if recorder != nil {
		recorder.Eventf(slot, nil, eventType, ReasonMachineObservedUp, ActionCorrectProgress,
			"%s", observedUpNote(slot, previous))
	}

	return outcome, nil
}

// impliesNoMachine reports whether a progress value says the slot has no machine, and is the mirror
// of impliesMachine over the same set: shutdown, shuttingdown, and the empty progress of a slot that
// was never seeded. The three are what fsm.getCurrentState maps to StateShutdown, which is the state
// from which ShutdownNode - the only operation that can satisfy a claim of off on a machine that is
// up - is not reachable. shuttingdown is in the set because a slot whose shutdown operation died with
// its manager, on a machine that is observably up, is frozen exactly as hard: validateStateTransition
// gates every transition of a transitional slot out, and the watchdog refuses to settle a slot that
// has a node object.
//
// The set is disjoint from impliesMachine over ready and startingup, and the only value both rules
// read is shuttingdown: they cannot both act on it, because the watchdog's gate requires a slot with
// no corev1.Node and this rule requires one that reports Ready.
func impliesNoMachine(progress infrav1alpha1.Progress) bool {
	switch progress {
	case infrav1alpha1.ProgressShutdown, infrav1alpha1.ProgressShuttingDown, "":
		return true
	default:
		return false
	}
}

// decideObservedPower is the rule with no I/O in it, so it is testable on its own and the write path
// re-derives it from the object it is about to write. See ReconcileObservedPower for the gates and
// for why each one is there.
func decideObservedPower(slot *infrav1alpha1.Node, obs observations) decision {
	if !impliesNoMachine(slot.Status.Progress) {
		return decision{reason: fmt.Sprintf("status.progress %q does not contradict an observed on", slot.Status.Progress)}
	}

	if slot.Status.PowerState != infrav1alpha1.PowerStateOn {
		return decision{reason: fmt.Sprintf("status.powerState is %q, not on: no observation contradicts the progress",
			string(slot.Status.PowerState))}
	}

	// The recorded on has to be the current observation of a machine, not the residue of one. A slot
	// whose node object is gone is the watchdog's shape (settle / re-arm), and a NotReady node is a
	// weak observation of a machine - both are spelled out on kubernetesNode and
	// core.applyObservedPowerState.
	if !obs.nodePresent {
		return decision{reason: fmt.Sprintf("the slot has no Kubernetes node %q: status.powerState %q is a record, not an observation",
			kubernetesNodeName(slot), string(slot.Status.PowerState))}
	}

	if !obs.nodeReady {
		return decision{reason: fmt.Sprintf("the Kubernetes node %q does not report Ready: the machine is not observed up",
			kubernetesNodeName(slot))}
	}

	if obs.lockHeld {
		return decision{reason: "a coordination lock names an operation that owns the slot"}
	}

	// The same lifecycle-Job gate the watchdog applies, for the same reason: the completed Job's own
	// state machine is the writer of the progress this rule would overwrite, and a machine that was
	// just powered off keeps its node object Ready for node-monitor-grace-period.
	inFlight, wait := jobWindow(obs)
	if inFlight != "" {
		return decision{requeueAfter: obs.grace, reason: fmt.Sprintf("lifecycle Job %s is in flight", inFlight)}
	}

	if wait > 0 {
		return decision{requeueAfter: wait, reason: "a lifecycle Job of the slot finished inside the settle grace"}
	}

	return decision{
		action:   ActionCorrectToReady,
		previous: slot.Status.Progress,
		reason: fmt.Sprintf("the machine is observed on and up, and status.progress %q says there is none",
			string(slot.Status.Progress)),
	}
}

// applyObservedPower writes the correction, re-deriving the whole rule from the object read back, in
// the shape fsm.updateNodeProgress and the watchdog's apply use: read fresh, re-decide, write the
// status subresource, retry on conflict, skip the write when the slot no longer qualifies.
//
// progress is the only field written. status.powerState is the observation this rule acted on and is
// left to the observation controllers, and spec is never written: the claim of a corrected slot
// stands, and acting on it is the FSM's branch, which runs on the caller's copy in the same pass.
func applyObservedPower(ctx context.Context, c client.Client, coord fsm.CoordinationManager, slot *infrav1alpha1.Node, d decision, grace time.Duration) (bool, infrav1alpha1.Progress, error) {
	var (
		wrote    bool
		previous infrav1alpha1.Progress
	)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(slot), fresh); err != nil {
			return err
		}

		obs, err := observe(ctx, c, coord, fresh, grace)
		if err != nil {
			return err
		}

		current := decideObservedPower(fresh, obs)
		if current.action != d.action {
			// The slot stopped qualifying: an operation took it, the machine went away, an
			// observation controller wrote the progress that is already the truth.
			return nil
		}

		fresh.Status.Progress = infrav1alpha1.ProgressReady

		if err := c.Status().Update(ctx, fresh); err != nil {
			return err
		}

		wrote = true
		previous = current.previous

		syncLocalCopy(slot, fresh)

		return nil
	})
	if err != nil {
		return false, "", err
	}

	return wrote, previous, nil
}

// observedUpNote renders the event's message: the progress that was replaced, the observation that
// outranked it, the claim, and what the controller expects to happen now. It is one sentence per
// claim because the two claims end in different places, and an operator reading `kubectl describe`
// has to be able to tell a machine that is about to be shut down from a record that was simply
// stale.
func observedUpNote(slot *infrav1alpha1.Node, previous infrav1alpha1.Progress) string {
	if slot.Spec.DesiredPowerState == infrav1alpha1.PowerStateOff {
		return fmt.Sprintf(
			"Observed Kubernetes node %s Ready (status.powerState %q) while status.progress said %q: the machine is up and "+
				"spec.desiredPowerState is %q, so the progress - a record of what the FSM last did, not of what the provider "+
				"sees - is corrected to %q. The shutdown the claim asks for is legal from there and runs on this pass; nothing "+
				"is waiting for a grace period, the machine is paid for now. Check why the shutdown that recorded %q did not "+
				"take the machine off (its Job exits 0 without the power-off landing, or the machine booted out of band).",
			kubernetesNodeName(slot), string(infrav1alpha1.PowerStateOn), string(previous),
			string(infrav1alpha1.PowerStateOff), string(infrav1alpha1.ProgressReady), string(previous))
	}

	return fmt.Sprintf(
		"Observed Kubernetes node %s Ready (status.powerState %q) while status.progress said %q: the machine is up and is the "+
			"one spec.desiredPowerState %q asks for, so the progress is corrected to %q. It implied no machine, which is the "+
			"state the FSM reads as shutdown, so a scale-down of this slot could not have been recorded at all. Nothing else "+
			"is done - no lifecycle Job, no power operation: the machine is wanted and up.",
		kubernetesNodeName(slot), string(infrav1alpha1.PowerStateOn), string(previous),
		string(infrav1alpha1.PowerStateOn), string(infrav1alpha1.ProgressReady))
}
