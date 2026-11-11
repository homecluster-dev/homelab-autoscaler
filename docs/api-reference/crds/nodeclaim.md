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
| `deadlineAt` | `*metav1.Time` | No | The budget of this attempt: `requestedAt + max(7m10s, group.spec.maxNodeProvisionTime)`. Overdue means `failed`, and `failed` means the machine is force-shut-down |
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
| `phase` | `NodeClaimPhase` | `pending`, `booting`, `ready`, `failed`, `completed`. Defaulted to `pending` by the CRD. The phase the operator's decisions derive from |
| `attempt` | `int32` | `1..N` for the slot: seeded from `Node.status.lastClaim.attempt`, incremented by the `pending → booting` write, so it counts machines bought, not reconcile passes |
| `reason` | `string` | The one-line outcome: `DeadlineExceeded`, `StartupJobFailed`, `StartupJobTimeout`, `ShutdownJobFailed`, `ShutdownJobTimeout`, `ReadinessGateTimeout`, `AttemptLimitExceeded`, `CancelledByDecreaseTargetSize`, `NodeDeleted`, `MachineGone`, `SlotDeleted`, `DeletedByHand`, `Superseded`, `StuckTransition` |
| `finishedAt` | `*metav1.Time` | When the claim reached a terminal phase. The TTL is measured from it |
| `conditions` | `[]metav1.Condition` | `MachineCommitted` (True from `booting` on: the attempt bought a machine, the claim is no longer cancelable) and `Ready` (True at `ready`) |

### Phases, and who writes each move

| Phase | Meaning | Legal next | Written by |
|-------|---------|----------|-----------|
| `pending` | Claimed, no lifecycle Job yet. **The only cancelable phase.** | `booting`, `ready`, `completed`, `failed` | created by the provider; live by the schema default |
| `booting` | The startup Job exists and the power-on has been asked for. **Committed.** | `ready`, `completed`, `failed` | the `StartNode` transition of the slot, in the same write that created the startup Job and incremented `attempt` |
| `ready` | The machine is up, joined, and free of the pool's readiness taints, so it can take the pool's work | `completed`, `failed` | the readiness gate at the end of the startup transition (`fsm.setNodeSchedulableWithRetry`) |
| `failed` | The attempt produced no usable machine and never will. Terminal | — | the claim controller (deadline, attempt bound, superseded, slot gone) and the transition path (Job failed / timed out, readiness gate timed out, force cleanup) |
| `completed` | The claim ended with the machine available. Terminal | — | the claim controller's finalizer path, once the machine is observed off |

`pending → booting` is also the attempt boundary. A move the phase table refuses
(`internal/claim/recorder.go`) is an `ErrIllegalMove`, and nothing is written: **a boot that has
started is never cancelled**, it is reversed — deleting a `booting` claim runs cordon, shutdown
Job, power off, and the claim ends `completed`.

## How cluster-autoscaler's calls become claims

| RPC | What the provider does |
|-----|------------------------|
| `NodeGroupTargetSize` | Returns the count of **live** claims: `pending` + `booting` + `ready`. A `failed`, `completed`, or scale-down-marked claim is not counted |
| `NodeGroupIncreaseSize(delta)` | Creates `delta` claims on `delta` free slots (free = no live claim names it), each with `request.call: IncreaseSize`, the request's `delta`, the `group`, `requestedBy: cluster-autoscaler`, `requestedAt`, `deadlineAt`, and the group's `maxNodeProvisionTime`. `maxSize` is the slot count, checked as `liveClaims + delta > slots` |
| `NodeGroupDecreaseTargetSize(delta)` | Deletes `\|delta\|` claims in phase **`pending`**, oldest first, capped to what can be cancelled. **Never a `booting` claim, never a `ready` one.** The floor is judged against the resulting size the request can produce; a request that can cancel nothing is a no-op success |
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
  deadlineAt: "2026-03-04T09:45:25Z"
  maxNodeProvisionTime: 15m0s
  ttlSecondsAfterFinished: 3600
status:
  phase: booting
  attempt: 2
  conditions:
  - type: MachineCommitted
    status: "True"
    reason: booting
```

A manual scale-up, with the identity of whoever asked:

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
2. **A claim is `failed`** — read `status.reason`, then `Node.status.lastClaim`. `DeadlineExceeded`
   and `ReadinessGateTimeout` mean the machine did not become usable; the machine is powered off
   and the slot is free. See
   [Common issues](../../troubleshooting/common-issues.md#a-claim-failed).
3. **"one live claim per slot" is rejected** — the refusal is the invariant working. See
   [Common issues](../../troubleshooting/common-issues.md#one-live-claim-per-slot-is-rejected).
4. **The pool never scales up** — a claim is only created if the estimator fits a pod on the
   group's template, which for a pool at zero means
   [`spec.nodeCapacity`](node.md#node-capacity) must be declared on the slots.

## Related Documentation

- [NodeClaim Design](../../design/nodeclaim.md) - why this object, and what it replaced
- [State Management](../../architecture/state.md) - the input law, the phase table, the deadlines
- [Node CRD](node.md) - the slot a claim asks for
- [Group CRD](group.md) - `minSize`, `maxNodeProvisionTime`, the readiness taints
- [Common issues](../../troubleshooting/common-issues.md) - the runbooks
