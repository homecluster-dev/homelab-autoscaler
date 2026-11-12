# NodeClaim CRD Reference

A NodeClaim is one request for one machine: scale-up intent as an object, with an identity, an
attribution, a deadline and an end. It replaces the `spec.powerState` scalar `v1alpha1` put on the
slot, and it is the object the operator's decisions are derived from.

## API Version
- **Group**: `infra.homecluster.dev`
- **Version**: `v2alpha1` (served and storage)
- **Kind**: `NodeClaim` (short name `nc`, category `homelab-autoscaler`)

## The law it exists for

```
machine is wanted  ⇔  a live NodeClaim (pending, booting, ready) names the slot
```

The slot (`Node`) is configuration, the claim is a request, `Node.status` is what the cluster
observes. One live claim per slot, enforced at admission and in the controller.

## Specification

### NodeClaimSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `slotRef` | `corev1.ObjectReference` | Yes | The `Node` CR (the slot) this claim asks for. `name` is the field every controller reads; the `infra.homecluster.dev/node` label is the index, and the two must agree |
| `request` | `ClaimRequest` | Yes | Structured attribution: which RPC asked for this machine, and with what arguments |
| `requestedAt` | `metav1.Time` | No | When the request was made. The deadline budget is measured from it, so a replayed or hand-made claim gets the budget it would have had |
| `requestedBy` | `string` | No | Free text, for an operator reading `kubectl get nodeclaim`: `cluster-autoscaler`, a human's name on a manual claim. Never parsed. Max 256 chars |
| `deadlineAt` | `*metav1.Time` | No | The budget of the **boot**: `requestedAt + max(12m10s, group.spec.maxNodeProvisionTime)` — the startup Job's own timeout, the Job monitor poll, the registration wait, the readiness gate and a margin. Overdue means `failed`, and `failed` means the machine is force-shut-down. It bounds `pending` and `booting` only, and the `booting → ready` write **clears it**: a claim that owns a machine is not destroyed by a budget it already spent |
| `maxNodeProvisionTime` | `*metav1.Duration` | No | The group's `spec.maxNodeProvisionTime` as it was when the claim was created: a claim says what budget was in force, and a pool that changes the number does not rewrite history |
| `ttlSecondsAfterFinished` | `*int32` | No | How long a terminal claim (`failed`, `completed`) survives `status.finishedAt` before the controller deletes it. Default `3600`. The slot keeps the digest, so collection is safe |

The spec is written by the autoscaler (the gRPC provider) and by an operator making a manual
claim. Treat it as immutable: a claim is the record of a request, and changing a request means
making a new claim.

### ClaimRequest

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `call` | `string` | Yes | The gRPC entry point that created the claim: `IncreaseSize`, `DecreaseTargetSize`, `DeleteNodes` |
| `group` | `string` | Yes | The node group (the `Group` CR name) the request was for. It is also the `group` label of the claim |
| `delta` | `int32` | No | The request's delta, signed: `+N` for `IncreaseSize`, `-N` for `DecreaseTargetSize`. Empty for `DeleteNodes`, which names nodes |
| `nodes` | `[]string` | No | The Kubernetes node names a `DeleteNodes` request named |

### NodeClaimStatus

| Field | Type | Description |
|-------|------|-------------|
| `phase` | `NodeClaimPhase` | `pending`, `booting`, `ready`, `failed`, `completed`. **No CRD default**: the status subresource strips `status` from a create, so a default on a subresource field is only stamped by the first status write. An empty phase is read as `pending` by `phase.IsLive` / `phase.IsPending` — the one place that reading lives — so a request is capacity from its create |
| `attempt` | `int32` | `1..N` for the slot: stamped once, on the claim's first pass, from `Node.status.lastClaim.attempt` (`claim.Recorder.Seed`, and only while the field is still 0). Nothing writes it again — not the `pending → booting` write, not a reconcile pass — so one claim is one attempt |
| `shutdownAttempts` | `int32` | How many times the scale-down that named this claim fired the shutdown of a machine the provider still observes up. A counter of its own, separate from `attempt`: the boot attempts are the record of the machines the request bought, and a shutdown that will not land does not spend them. Past `claim.MaxConsecutiveAttempts` (3) the slot reconciler stops firing the shutdown, and the streak resets after `claim.ShutdownAttemptResetGrace` (30m) or with the claim |
| `lastShutdownAttemptAt` | `*metav1.Time` | When the last of those attempts was counted: the clock of the reset grace, and the timestamp that answers "since when has the controller paused this shutdown" |
| `reason` | `string` | The one-line outcome: `DeadlineExceeded`, `StartupJobFailed`, `StartupJobTimeout`, `ShutdownJobFailed`, `ShutdownJobTimeout`, `ReadinessGateTimeout`, `AttemptLimitExceeded`, `CancelledByDecreaseTargetSize`, `NodeDeleted`, `MachineGone`, `SlotDeleted`, `DeletedByHand`, `Superseded`, `StuckTransition` |
| `finishedAt` | `*metav1.Time` | When the claim reached a terminal phase. The TTL is measured from it |
| `conditions` | `[]metav1.Condition` | `MachineCommitted` (True from `booting` on: the attempt bought a machine, the claim is no longer cancelable; on a terminal move it is read off the phase the claim moved **from**, so a dead claim says whether there is a machine to clean up), `Ready` (True at `ready`), and `MachineObservedOff` (True while the machine of a ready claim is observed off; its `lastTransitionTime` is the clock of the `MachineGone` bound) |

### Phases, and who writes each move

| Phase | Meaning | Legal next | Written by |
|-------|---------|----------|-----------|
| `pending` | Claimed, no lifecycle Job yet. **The only cancelable phase.** | `booting`, `ready`, `completed`, `failed` | created by the provider, and live from the create: the claim carries no phase at all in that window, and an empty phase is read as `pending` |
| `booting` | The startup Job exists and the power-on has been asked for. **Committed.** | `ready`, `completed`, `failed` | the `StartNode` transition of the slot, in the same write that created the startup Job. `attempt` is not written here: the claim carries the number it was seeded with |
| `ready` | The machine is up, joined, and free of the pool's readiness taints, so it can take the pool's work | `completed`, `failed` | the readiness gate at the end of the startup transition (`fsm.setNodeSchedulableWithRetry`) — the only writer of the move, and the write that closes the boot window by clearing `spec.deadlineAt` |
| `failed` | The attempt produced no usable machine and never will. Terminal | — | the claim controller (deadline, attempt bound, superseded, slot gone) and the transition path (Job failed / timed out, readiness gate timed out, force cleanup) |
| `completed` | The claim ended with the machine available. Terminal | — | the claim controller's finalizer path, once the machine is observed off |

`pending → booting` is also the attempt boundary. A move the phase table refuses
(`internal/claim/recorder.go`) is an `ErrIllegalMove`, and nothing is written: **a boot that has
started is never cancelled**, it is reversed — deleting a `booting` claim runs cordon, shutdown
Job, power off, and the claim ends `completed`.

## Events

The claim is the attempt, so the attempt's timeline is on the claim: `kubectl describe nodeclaim
<name>` shows when the request was created, when it committed to a boot, when it became ready, and
how it ended. Every one of these Events has `involvedObject.kind: NodeClaim`, carries the action
`nodeclaim`, and is published by the one chokepoint every phase write of the model passes through
(`internal/claim/recorder.go`, which calls the table in `internal/claim/events.go`), so "exactly one
Event per transition" is a property of the structure, not a rule each caller has to remember. The
table of the set — with the two scale-down streak records and the slot's own events — is in
[Events](../../reference/events.md).

Three rules the set keeps:

- **Nothing is published for a move that did not land.** An illegal move (`ErrIllegalMove`), a
  re-apply of a move that changed nothing, and the metadata half of a move (a finalizer, a cleared
  `deadlineAt`) write nothing, so they say nothing. A timeline that reports a transition the cluster
  does not have is worse than no timeline.
- **Repeats coalesce.** Every event of the set carries the action `nodeclaim`, and the new events API
  (`events.k8s.io`) keys its aggregation on object + type + reason + action: a fact that happens
  again patches one Event's count and `lastTimestamp` instead of adding an object per pass. No
  resync loop storms the event stream.
- **The slot's events and the claim's events are two sentences, not one sentence twice.**
  `ScaleUpRequested` / `ScaleUpCompleted` on the `Node` say what the slot's machine did;
  `ClaimBooting` / `ClaimReady` on the claim say what happened to this attempt, with the attempt's
  own numbers (attempt number, Job, deadline, duration).

Events are the timeline, conditions are the durable record. A `NodeClaim` is collected after
`spec.ttlSecondsAfterFinished` and an Event after the cluster's Event TTL (one hour by default), and
the slot keeps the episode in `Node.status.lastClaim` — so `status.conditions` and the digest are
what you read the day after, and the events are what you read while it is happening.

```bash
# One claim's timeline
kubectl describe nodeclaim worker-01-7f4b9c -n homelab-autoscaler-system

# The pool's claim events, newest first
kubectl get events -n homelab-autoscaler-system \
  --field-selector involvedObject.kind=NodeClaim --sort-by=.lastTimestamp

# One claim's events
kubectl get events -n homelab-autoscaler-system \
  --field-selector involvedObject.kind=NodeClaim,involvedObject.name=worker-01-7f4b9c
```

## How cluster-autoscaler's calls become claims

| RPC | What the provider does |
|-----|------------------------|
| `NodeGroupTargetSize` | Returns the count of **live** claims: `pending` + `booting` + `ready`. A `failed`, `completed`, or scale-down-marked claim is not counted |
| `NodeGroupIncreaseSize(delta)` | Creates `delta` claims on `delta` free slots (free = no live claim names it), each with `request.call: IncreaseSize`, the request's `delta`, the `group`, `requestedBy: cluster-autoscaler`, `requestedAt`, `deadlineAt`, and the group's `maxNodeProvisionTime`. `maxSize` is the slot count, checked as `liveClaims + delta > slots` |
| `NodeGroupDecreaseTargetSize(delta)` | Deletes `\|delta\|` claims in phase **`pending`**, oldest first, capped to what can be cancelled. **Never a `booting` claim, never a `ready` one.** The floor is judged against the resulting size the request can produce. A group already at the requested size is a no-op success; a group whose every live claim is past `pending` is answered with `codes.Unavailable` (retryable) naming how many boots and machines are committed, because success here is the autoscaler's cue to stop asking and to keep paying for a unit it asked away, while its next `IncreaseSize` for the same slot answers "no free node slot" for up to a boot deadline plus the claim's TTL |
| `NodeGroupDeleteNodes(nodes)` | Records the request on the claim of each named slot (`request.call: DeleteNodes`, `request.nodes`) and cancels it: a `pending` claim is deleted, a `booting`/`ready` claim is deleted through its finalizer, which runs the shutdown. The drain stays cluster-autoscaler's |

No RPC writes a field of a `Node` spec. `NodeGroupNodes` reports an instance per machine:
`creating` (a live claim, no node object), `running` (a live claim that wants the machine, node
object present), `deleting` (a node object no live claim wants).

## Labels and ownership

```yaml
metadata:
  labels:
    group: remote-workers                    # the pool: kubectl get nodeclaim -l group=...
    infra.homecluster.dev/node: worker-01    # the slot: -l infra.homecluster.dev/node=worker-01
  ownerReferences:
  - kind: Node
    name: worker-01
    controller: true
  finalizers:
  - infra.homecluster.dev/nodeclaim-finalizer  # stamped when the phase becomes booting
```

The ownerReference is what makes deleting a slot from Git take its claims with it, and the
finalizer is what makes deleting a committed boot a scale-down request instead of an abandonment.

## Examples

The claim a scale-up produces, seen from `kubectl get nodeclaim -o wide`:

```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: NodeClaim
metadata:
  name: worker-01-7f4b9c
  namespace: homelab-autoscaler-system
spec:
  slotRef:
    name: worker-01
  requestedAt: "2026-03-04T09:38:15Z"
  requestedBy: cluster-autoscaler
  request:
    call: IncreaseSize
    delta: 1
    group: worker-nodes
  deadlineAt: "2026-03-04T09:53:15Z"   # requestedAt + max(12m10s, the group's number)
  maxNodeProvisionTime: "15m0s"        # the group's number, copied at creation
  ttlSecondsAfterFinished: 3600
status:
  phase: booting
  attempt: 2
  conditions:
  - type: MachineCommitted
    status: "True"
    reason: booting
    message: "startup Job worker-01-startup-1772185095 exists: the boot is committed"
    lastTransitionTime: "2026-03-04T09:39:02Z"
```

A manual scale-up, with the identity of whoever asked, and the budget stamped by hand: the provider
computes `spec.requestedAt` and `spec.deadlineAt` when it creates a claim, and nothing fills them in
for one you write. A claim with no `spec.deadlineAt` never expires, so an attempt that never starts
holds its slot open with nothing left to time it out.

```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: NodeClaim
metadata:
  name: worker-01-manual
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
    infra.homecluster.dev/node: worker-01
spec:
  slotRef:
    name: worker-01
  requestedBy: filippo
  request:
    call: IncreaseSize
    group: worker-nodes
  requestedAt: "2026-03-04T09:38:15Z"   # now, RFC3339
  deadlineAt: "2026-03-04T09:50:25Z"    # requestedAt + 12m10s, the budget of one attempt
```

## Usage

```bash
# The pool's requests, with their deadlines
kubectl get nodeclaim -n homelab-autoscaler-system -o wide

# One slot's history, newest first
kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=worker-01

# The record of the last episode, after the claims have been collected
kubectl describe nodes.infra.homecluster.dev worker-01 -n homelab-autoscaler-system

# Watch a scale-up land
kubectl get nodeclaim -n homelab-autoscaler-system -w

# Cancel a request. A pending claim is deleted; a booting/ready claim runs the shutdown path.
kubectl delete nodeclaim worker-01-7f4b9c -n homelab-autoscaler-system
```

## Validation

- `spec.slotRef.name` is required, and the slot must exist in the claim's namespace
- **one live claim per slot.** A second live claim is refused at admission, and the message names
  the slot and the claim that owns it
- a status write that ends a claim (`→ failed`, `→ completed`) is never refused, and a delete is
  never refused
- the claim controller enforces the same rule where admission cannot see it (two providers racing,
  a hand-applied claim, a webhook that was down): the newest live claim owns the slot, the extras
  fail themselves with `reason: Superseded`

## Troubleshooting

1. **A claim is stuck `pending`** — the provider answered the autoscaler, nothing has started.
   `kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<slot>`
   The slot reconciler boots it on its next pass; a coordination lock held by the opposite
   operation defers it. See [Common issues](../../troubleshooting/common-issues.md#a-claim-is-stuck-pending).
2. **A claim is `failed`** — read `status.reason`, then the `ClaimFailed` event in
   [Events](#events) (it quotes the deadline, the Job and the attempts behind the reason), then
   `Node.status.lastClaim`. `DeadlineExceeded` and `ReadinessGateTimeout` mean the machine did not
   become usable; the machine is powered off and the slot is free. See
   [Common issues](../../troubleshooting/common-issues.md#a-claim-failed).
3. **"one live claim per slot" is rejected** — the refusal is the invariant working. See
   [Common issues](../../troubleshooting/common-issues.md#one-live-claim-per-slot-is-rejected).
4. **The pool never scales up** — a claim is only created if the estimator fits a pod on the
   group's template, which for a pool at zero means
   [`spec.nodeCapacity`](node.md#node-capacity) must be declared on the slots.

## Related Documentation

- [The NodeClaim model](../../concepts/nodeclaim-model.md) - the law, the phases and the bounds, in
  operator's words
- [Events](../../reference/events.md) - every reason this object and the slot publish, and what lasts
- [NodeClaim Design](../../development/nodeclaim.md) - why this object, and what it replaced
- [State Management](../../development/state.md) - the input law, the phase table, the deadlines
- [Node CRD](node.md) - the slot a claim asks for
- [Group CRD](group.md) - `minSize`, `maxNodeProvisionTime`, the readiness taints
- [Common issues](../../troubleshooting/common-issues.md) - the runbooks
