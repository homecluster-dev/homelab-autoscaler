# Events, Reasons and Conditions

What the operator publishes about one episode, on two objects: the **claim** reports the request, the
**slot** reports the machine. Read the claim for the timeline of an attempt and the slot for what the
machine did; `kubectl describe` is the entry point for both.

The durable record is not the event stream. Events expire with the cluster's Event TTL — about one
hour by default — while `NodeClaim.status.conditions`, `NodeClaim.status.reason` and
`Node.status.lastClaim` live as long as their object does, and the slot's digest outlives the claim
that was collected by its TTL. So:

| You want | Read | It lasts |
| --- | --- | --- |
| what just happened, in order | `kubectl describe nodeclaim <claim>` | about an hour |
| what the machine did | `kubectl describe node.infra.homecluster.dev <slot>` | about an hour |
| what is true about the last attempt | `status.reason`, `status.conditions`, `Node.status.lastClaim` | as long as the objects do |
| the shape of the pool a day later | `kubectl get groups`, `kubectl describe node.infra.homecluster.dev` (`recentTransitions`) | the field's own bound |

```bash
# One attempt's timeline, oldest first
kubectl describe nodeclaim worker-01-7f4b9c -n homelab-autoscaler-system

# The claim stream of the pool
kubectl get events -n homelab-autoscaler-system \
  --field-selector involvedObject.kind=NodeClaim --sort-by=.lastTimestamp

# One claim's events
kubectl get events -n homelab-autoscaler-system \
  --field-selector involvedObject.kind=NodeClaim,involvedObject.name=worker-01-7f4b9c

# The slot's events: what the machine was asked to do
kubectl describe nodes.infra.homecluster.dev worker-01 -n homelab-autoscaler-system
```

## Claim events

Every one of the `Claim*` rows below is published on the `NodeClaim`, carries the action `nodeclaim`,
and is written at the one chokepoint that owns the phase writes — after the status write landed. A
move the phase table refuses, a write the API server refused, and a re-apply that changed nothing
publish nothing, so a timeline never reports a transition the cluster does not have. Repeats of one
fact patch a single Event's `count` and `lastTimestamp` instead of adding an object per pass, so a
resync loop cannot storm the stream; `kubectl get events -o wide` shows the action. The last two rows
are the slot reconciler's: published on the claim that carries the count, with the action `PowerOff`.

| Reason | Type | When | The note carries |
| --- | --- | --- | --- |
| `ClaimCreated` | Normal | the claim exists — the first write of a fresh claim, for every creator: the provider, a hand-applied claim, a replay | `requestedBy`, the call and its `delta` / `nodes`, the group, the attempt number, the deadline and the budget |
| `ClaimBooting` | Normal | `pending → booting` | the startup Job of the attempt (or the label selector that finds it), and that the boot is committed and no longer cancelable |
| `ClaimReady` | Normal | `booting → ready` | the Kubernetes node that passed the readiness gate, and the time from `spec.requestedAt`; the same write cleared `spec.deadlineAt` |
| `ClaimFailed` | Warning | any move to `failed` | `status.reason` **and the number behind it**: the deadline that passed, the Job that died, the attempts spent, the next attempt's number |
| `ClaimCancelled` | Normal | a live claim a caller cancelled reached its end (`CancelledByDecreaseTargetSize`, `NodeDeleted`, `DeletedByHand`) | which call asked, its delta / nodes, whether a machine had been bought, and how long the attempt lasted |
| `ClaimCompleted` | Normal | a move to `completed` with no cancel behind it | that the machine is observed off, the duration, and the shutdown Job that powered it off when one is readable |
| `ClaimSuperseded` | Normal | a move to `failed` with `reason: Superseded` | the newer claim's name — the request that owns the slot now |
| `ShutdownAttemptLimitReached` | Warning | a scale-down has spent its 3 shutdown attempts on a machine still observed up, and the controller stops firing it | the attempts, the bound, and how long the pause lasts |
| `ShutdownAttemptStreakReset` | Warning | the reset grace passed and the controller fires the shutdown again | the bound spent, the grace, and that a fixed script is picked up without a hand edit |

`ClaimCancelled` and `ClaimSuperseded` are Normal, not Warning: a cancel is a request landing, and a
superseded extra claim is the one-live-claim-per-slot rule holding.

## Slot events

These are published on the `Node` CR — the slot — and say what the machine was asked to do and what
happened to it. The `Action` column is what `kubectl get events -o wide` prints.

| Reason | Type | Action | When |
| --- | --- | --- | --- |
| `ScaleUpRequested` | Normal | `PowerOn` | the startup transition started: the slot was claimed and the power-on was asked for |
| `ScaleUpCompleted` | Normal | `PowerOn` | the startup Job completed and the transition into `ready` finished |
| `ScaleUpFailed` | Warning | `PowerOn` | the startup Job failed, timed out, or the transition was force-cleaned |
| `ScaleUpDeferred` | Normal | `PowerOn` | the boot is deferred: the coordination lock is held by the opposite operation, and the requeue is paced to the lock's own deadline |
| `ScaleDownRequested` | Normal | `PowerOff` | the shutdown transition started: the node was cordoned and the power-off was asked for |
| `ScaleDownCompleted` | Normal | `PowerOff` | the shutdown Job completed and the machine is observed off |
| `ScaleDownFailed` | Warning | `PowerOff` | the shutdown Job failed, timed out, or the transition was force-cleaned |
| `ScaleDownDeferred` | Normal | `PowerOff` | the shutdown is deferred behind a lock; the intent stays on the live claim, so nothing has to be asked for again |
| `ReadinessGateTimeout` | Warning | `Uncordon` | the gate's 4 minutes expired: the node was uncordoned anyway (a cordoned machine is invisible to scale-down and is paid for forever) and the claim failed with that reason |
| `DeclaredNodeLabelsUnwritten` | Warning | `Uncordon` | the gate refused the node because the labels and annotations its slot declares could not be written, so the machine stays cordoned while the write is retried. Recorded once per gate, not per retry |
| `NodeLabelsApplied` | Normal | `ApplyNodeLabels` | a write of the slot's declared keys landed on the node object: `+` added, `~` rewritten, `-` removed |
| `SkippedReservedNodeLabelKey` | Warning | `ApplyNodeLabels` | a declared key in a namespace its owner controls was skipped at write time, for a slot that predates the admission rule. Fired when the set of refused keys changes |
| `NodeCapacityMismatch` | Warning | `CheckDeclaredCapacity` | the registered machine's `status.allocatable` sits more than 5% from `spec.nodeCapacity` on a declared resource, naming the resource and both values. Only declared resources are compared |
| `AutoscalerDeletionCandidate` | Normal | `AutoscalerDecision` | the node carries `DeletionCandidateOfClusterAutoscaler`: cluster-autoscaler is evaluating it for scale-down |
| `AutoscalerDeletionRequested` | Normal | `AutoscalerDecision` | the node carries `ToBeDeletedByClusterAutoscaler`: it was marked for deletion and is draining |

The last two are reports of a decision the autoscaler made, not of an action this operator took: it
never adds, edits, or removes a cluster-autoscaler marker. Date the decision from the condition's
message (`observed at ...`), not from its `lastTransitionTime` — both markers are recorded with
`Status: True`, so the candidate→requested transition keeps the candidate's time.

Messages are flattened onto one line and cut at 512 characters: a Job failure or a webhook denial
carries a multi-line payload, and an Event is a real object kept in etcd.

## Claim reasons

`NodeClaim.status.reason` is the one-line outcome, mirrored into `Node.status.lastClaim.reason`. The
set is open, and the names are stable:

| Reason | What happened |
| --- | --- |
| `DeadlineExceeded` | `spec.deadlineAt` passed while the claim was `pending` or `booting`; the machine is force-shut-down |
| `StartupJobFailed` | the startup Job of the attempt exited non-zero, or could not be created |
| `StartupJobTimeout` | the startup Job of the attempt ran out of time |
| `ShutdownJobFailed` | the shutdown Job of a scale-down exited non-zero, so the machine it was to power off is still up |
| `ShutdownJobTimeout` | the shutdown Job of a scale-down ran out of time |
| `StuckTransition` | the lifecycle transition was stuck past 15m and was force-cleaned |
| `ReadinessGateTimeout` | the machine booted and joined but never cleared the pool's readiness gate |
| `AttemptLimitExceeded` | the slot spent its 3 consecutive attempts; no machine is bought for it until one lands |
| `CancelledByDecreaseTargetSize` | `DecreaseTargetSize` deleted a `pending` claim |
| `NodeDeleted` | `DeleteNodes` named this slot's machine for deletion |
| `MachineGone` | the node object of a `ready` claim disappeared and the machine has been observed off for a boot window |
| `SlotDeleted` | the `Node` CR the claim asked for is going away |
| `DeletedByHand` | an operator deleted the claim, and the finalizer powered its machine off |
| `Superseded` | a second live claim was found on one slot, and this one is the extra |

Each is explained, with what to do, in
[A Claim Failed](../troubleshooting/common-issues.md#a-claim-failed).

## Conditions

Conditions are the durable half. They are stamped by the same writes as the events, and they answer
"what is true" rather than "what happened".

On the claim:

| Condition | True when |
| --- | --- |
| `Ready` | the claim's machine can take the pool's work |
| `MachineCommitted` | the startup Job exists, i.e. from `pending → booting` on. On a terminal move it is read off the phase the claim moved **from**, so a dead claim says whether there is a machine to clean up. It is a record for you, never an instruction to a reconciler |
| `MachineObservedOff` | the machine of a `ready` claim is observed off. Its `lastTransitionTime` is the instant the loss was first recorded, and the `MachineGone` bound is measured from it; a reboot clears it and the claim stays `ready` |

On the slot, written by the lifecycle transitions:

| Condition | True when |
| --- | --- |
| `StartupProgressing` | a startup transition started and the machine is being powered on |
| `ShutdownProgressing` | a shutdown transition started and the machine is being powered off |
| `Ready` | the slot entered the ready state of the state machine: the machine is on and operational |
| `Shutdown` | `False`, with reason `NodeShutdown`, when the slot entered the shut-down state: the machine is off |
| `JobFailed` | a lifecycle Job of the attempt exited non-zero, naming the transition it moved the slot on |
| `ForceCleanup` | a stuck transition was force-cleaned past 15m |

Plus the two the observation writes:

| Condition | What it records |
| --- | --- |
| `ClusterAutoscalerMarkedForDeletion` | the last cluster-autoscaler marker this operator read on the node, with the node's taints, `spec.unschedulable` and the instant it was observed |
| `NodeReadinessTaints` | the taint keys this slot's readiness gate is waiting for, from the Group's `spec.nodeReadinessTaints` |

The Group carries one condition of its own, `Loaded`: the pool is published to the provider. Its
`lastTransitionTime` dates the moment that status last changed, not the last reconcile pass.

## Related Documentation

- [The NodeClaim model](../concepts/nodeclaim-model.md) — the phases the events report
- [Read the Pool at a Glance](../concepts/pool-at-a-glance.md) — the fields that outlive the stream
- [Node CRD](../api-reference/crds/node.md), [NodeClaim CRD](../api-reference/crds/nodeclaim.md)
- [Debugging Guide](../troubleshooting/debugging-guide.md) — the component-by-component method
