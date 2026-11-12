# NodeClaim Design

Scale-up intent gets its own object. `Node` keeps the configuration of a slot, `NodeClaim` carries
one request for one machine, and `Node.status` carries what the cluster observes. The design
replaces the scalar `spec.powerState` claim with a short-lived, attributed, deadline-carrying
object, and settles the questions the 0.1.19–0.1.22 fixes kept re-opening.

Every statement about current behaviour names the file that implements it, so the model can be
checked against the code.

**Status: implemented, in chart `0.2.0`** (`dist/chart/Chart.yaml`). `v2alpha1` is the served and
storage version of `nodes`, `groups` and the new `nodeclaims`, and `v1alpha1` is removed
(§9). Where the running code is not exactly the sketch above, §11 says so.

## 1. The Problem This Replaces

One object, `Node`, carries three kinds of state with three different writers and three different
lifetimes:

| Kind | Field | Written by | Lifetime |
| --- | --- | --- | --- |
| Declarative config | `spec.startupPodSpec`, `spec.shutdownPodSpec`, `spec.nodeTaints`, `spec.nodeCapacity`, `spec.pricing`, `spec.kubernetesNodeName` | Git, at `kubectl apply` | years |
| Runtime intent | `spec.powerState` (Go field `DesiredPowerState`, `api/infra/v1alpha1/node_types.go`) | cluster-autoscaler, through the gRPC provider | minutes, and it is not a request, it is a level |
| Recorded history | `status.progress`, `status.powerState`, `status.conditions` (`ClaimRearmed`, `ShutdownAttempts`, `NodeReadinessTaints`, `ClusterAutoscalerMarkedForDeletion`) | the FSM, the observation controllers, the progress watchdog, gRPC | the slot's whole life |

The consequences are all live, and each one is a fix:

- **GitOps reverts every scale-up.** `spec.powerState` is a spec field, so Argo CD with
  `selfHeal` + SSA re-applies the value in git over the autoscaler's write on every sync —
  ~2.5 minutes. The machine goes down under a running pod, the pending pod triggers the next
  scale-up, and the slot churns forever. 0.1.21 (commit `c491835`, released in `1b01dec`) answered
  it by defaulting the field (`+kubebuilder:default=off`) and by deleting it from the GitOps
  manifests.
  That is a mitigation, not a model: the field is still in the API, any applier that writes it
  wins the last-write race, and the only defence is a comment telling operators not to declare it.
- **A claim has no identity.** `NodeGroupIncreaseSize` picks the first slot at
  `{powerState: off, status.powerState: off, status.progress: shutdown}` and writes
  `spec.desiredPowerState = on` (`internal/grpcserver/server.go`,
  `NodeGroupIncreaseSize`). Nothing records who asked, why, for which node group, or that this is
  attempt #3. The whole request is one scalar, so "the last write stands" is the entire history.
- **Retry counters live on the long-lived slot.** `fsm.StartupAttemptLimit` (3) is carried by the
  `status.conditions` entry `ClaimRearmed` (`internal/fsm/startup_attempts.go`) and
  `fsm.ShutdownAttemptLimit` (3) by `ShutdownAttempts` (`internal/fsm/shutdown_attempts.go`).
  Neither counter has a natural end, because the slot never ends, so each needs a reset heuristic:
  the startup counter is cleared "where an attempt demonstrably succeeded", the readiness gate of
  `afterJobCompleted`, and the shutdown counter in two more places. The first version of the
  startup counter counted the watchdog's re-arms and nothing removed the record, which made it a
  lifetime sentence on a slot that is reused across claims — the file's own comment says so.
- **`status.progress` means two things.** It is "what the FSM last did" (the FSM reads its own
  state out of it, `getCurrentState`, `internal/fsm/node_state_machine.go`) and "what the world
  looks like" (the observation controllers write it from the `corev1.Node`,
  `applyObservedPowerState`, `internal/controller/core/node_controller.go`). Both readings are
  load-bearing: the provider's target size is computed from it
  (`countsTowardsTargetSize`, `internal/grpcserver/server.go`), and every transition of a
  transitional slot is gated out (`validateStateTransition`,
  `internal/controller/infra/node_controller.go`).
- **So the ambiguity has been patched twice, correctly.** 0.1.20 added the progress watchdog
  (`internal/progress/watchdog.go`): settle a slot whose progress names a machine the cluster does
  not have, re-arm a claim whose machine never arrived, bounded by the attempt counter. 0.1.22
  added the observation precedence rule (`internal/progress/observed_power.go`,
  `ReconcileObservedPower`): an observed `on` outranks a recorded `shutdown`, because a record of
  what the FSM last did is not evidence about a machine. The four-rule authority order that
  documents both is in [State Management](../architecture/state.md). It is a disambiguation of one
  field with four writers, and every rule in it exists because a claim has no object of its own.

The last consequence is the structural one: the scalar is the lock, the request, the history, and
the report at once, so nothing in the system can be said about a scale-up without reading three
fields written by four controllers.

## 2. The Model

**The law: desired power is derived, never stored.**

```
machine is wanted  ⇔  a live NodeClaim exists for the slot
```

`spec.powerState` is removed from `Node` in `v2alpha1`. There is no field to revert, no last-write
race with Git, and no scalar that has to mean three things.

### The three objects

| Object | Spec owner | Status owner | Lifetime | Carries |
| --- | --- | --- | --- | --- |
| `Node` (slot) | Git | the operator's observation reconcilers | as long as the machine is committed to the pool | what a slot **is**: lifecycle pod specs, `kubernetesNodeName`, taints, capacity, pricing. Config only — in v2 its spec has no runtime intent in it |
| `NodeClaim` | the autoscaler (the gRPC handlers create it) | the claim controller | one scale-up attempt: minutes, then collected by TTL | one request for one machine: what was asked, by which call, with what deadline, and how it ended |
| `Node.status` | — | the operator's observation reconcilers | with the slot | `powerState`, `progress` (derived and reported, never read to decide), and a digest of the last claim (`lastClaim`) |

One live claim per slot, and that is the whole model:

- **No claim** → the slot is free. It is not capacity, and it is the only shape
  `IncreaseSize` may claim. Today the equivalent test is a three-field scan
  (`{powerState: off, status.powerState: off, status.progress: shutdown}`) plus a controller whose
  only job is to seed the status of slots that never had a machine so the arithmetic excludes them
  (`NodeSlotReconciler`, `internal/controller/core/node_slot_controller.go`). "No live claim"
  needs no seeding, no status, and no observation.
- **A claim in `pending`/`booting`/`ready`** → the slot is capacity, in use, counted.
- **A claim in `failed`/`completed`** → not capacity, and it stops being capacity the moment it is
  recorded, not when a status write catches up.

### The analogy, and the one place it diverges

cert-manager has this shape. `Certificate` is what the user says they want and is the user's
object; the controller creates one `CertificateRequest` per issuance attempt, owns its spec and
its lifecycle, and the `Certificate`'s status reports the outcome of the last one. No field of
`Certificate.spec` says "issue now"; issuing is derived from the existence of a request object.

The divergence is the writer of intent. In cert-manager the user writes the `Certificate` and the
controller derives the request. Here **the autoscaler expresses the intent** — cluster-autoscaler
calls `IncreaseSize` and the provider creates the claim — and **git expresses only what a slot
is**. That is the point: config and intent stop sharing an object, so an apply can never contradict
a scale-up, and a scale-up can never overwrite a declaration.

## 3. API Sketch (`v2alpha1`)

```go
// api/infra/v2alpha1/nodeclaim_types.go
type NodeClaimSpec struct {
    // SlotRef names the Node CR this claim asks for. One live claim per slot is
    // enforced by the claim controller, not by the API.
    SlotRef corev1.ObjectReference `json:"slotRef"`

    // RequestedAt is when the request was made. The deadline budget is measured from it.
    RequestedAt metav1.Time `json:"requestedAt"`

    // RequestedBy is free text: the identity of the caller, for an operator reading
    // `kubectl get nodeclaim`. "cluster-autoscaler", a manager's name, a human's kubectl
    // in a manual claim. It is not parsed.
    // +optional
    RequestedBy string `json:"requestedBy,omitempty"`

    // Request is structured attribution: which RPC asked for this machine, and with
    // what arguments. This is the field that answers "who asked, why, and for what".
    Request ClaimRequest `json:"request"`

    // DeadlineAt is the budget for this attempt. Overdue means failed.
    // +optional
    DeadlineAt *metav1.Time `json:"deadlineAt,omitempty"`
}

type ClaimRequest struct {
    // Call is the gRPC entry point that created this claim:
    // IncreaseSize | DecreaseTargetSize | DeleteNodes.
    // +kubebuilder:validation:Enum=IncreaseSize;DecreaseTargetSize;DeleteNodes
    Call string `json:"call"`

    // Delta is the request's delta, signed: +N for IncreaseSize, -N for
    // DecreaseTargetSize. Empty for DeleteNodes, which names nodes.
    // +optional
    Delta int32 `json:"delta,omitempty"`

    // Group is the node group (the Group CR name) the request was for.
    Group string `json:"group"`

    // Nodes are the Kubernetes node names from a DeleteNodes request.
    // Only set for Call: DeleteNodes.
    // +optional
    // +listType=atomic
    Nodes []string `json:"nodes,omitempty"`
}

type NodeClaimStatus struct {
    // +kubebuilder:validation:Enum=pending;booting;ready;failed;completed
    Phase NodeClaimPhase `json:"phase,omitempty"`

    // Attempt is 1..N for this slot. Seeded from Node.status.lastClaim.attempt + 1, so
    // the bound on attempts lives on the claim and needs no condition on the slot.
    Attempt int32 `json:"attempt,omitempty"`

    // Reason is the one-line outcome, for `kubectl get nodeclaim`:
    // DeadlineExceeded, StartupJobFailed, CancelledByDecreaseTargetSize, NodeDeleted.
    // +optional
    Reason string `json:"reason,omitempty"`

    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: NodeClaim
metadata:
  name: remote-worker-2-7f4b9c   # <slot>-<suffix>: created, never renamed
  ownerReferences:
    - apiVersion: infra.homecluster.dev/v2alpha1
      kind: Node
      name: remote-worker-2
      controller: true
spec:
  slotRef:
    name: remote-worker-2
  requestedAt: "2026-03-04T09:38:15Z"
  requestedBy: cluster-autoscaler
  request:
    call: IncreaseSize
    delta: 1
    group: remote-workers
  deadlineAt: "2026-03-04T09:50:25Z"   # 12m10s budget, see §4
status:
  phase: booting
  attempt: 2
  reason: ""
```

### Phase table

The `ready` phase is the **claim's** assertion that the machine it asked for is usable. That is
not the same sentence as "the `corev1.Node` is Ready", which is an observation with no request
behind it, and is recorded on the slot, not on the claim (§6).

| Phase | Meaning | Legal next phases | Who writes the move | What the slot does |
| --- | --- | --- | --- | --- |
| `pending` | Claim exists, no lifecycle Job has been created. The only cancelable phase. | `booting`, `completed`, `failed` | claim controller | Nothing yet. The slot is capacity and is counted. `StartNode` runs on the next pass: coordination lock, startup Job, power-on. |
| `booting` | The startup Job exists; the power-on has been asked for. Committed. | `ready`, `failed` | The Job monitor of the machine that started it (`monitorJobCompletion` → `JobCompleted` / `JobFailed` / `JobTimeout`) | The machine is booting. The readiness gate holds the claim at `booting` until the node is Ready and free of the pool's readiness taints. |
| `ready` | The machine is up, joined, and can take the pool's work. | `completed`, `failed` | the readiness gate of the startup transition (`afterJobCompleted`, the only writer of the move), the provider (a scale-down lands on it), the claim controller's machine-loss bound (§4) | The slot serves pods. A scale-down names it in `DeleteNodes`, which moves it to `completed` and powers the machine off. |
| `failed` | The attempt did not produce a usable machine, and never will. Terminal. | — | the lifecycle transition (Job failed / timed out, readiness-gate timeout), the claim controller (deadline, attempt limit, machine gone, `Superseded` for a second live claim on the slot) | Not capacity. The machine is force-shut-down so an overdue boot cannot join and start serving. The next claim on the slot is attempt N+1. |
| `completed` | The claim ended with the machine it asked for available. Terminal. | — | provider (`DecreaseTargetSize`, `DeleteNodes`), claim controller (a hand-deleted machine, a manual claim) | Not capacity. The machine is off, or is being drained and powered off. |

A claim created by the provider carries **no phase at all**: the status subresource strips
`status` from a create, and a default on a subresource field is only stamped by the first status
write, so `+kubebuilder:default=pending` buys nothing in that window. The window is read as
`pending` by the one predicate every reader asks (`phase.IsLive`, `phase.IsPending`), so a
brand-new request is capacity from its create: `TargetSize` counts it, `IsFree` refuses the slot
to a second `IncreaseSize`, and `DecreaseTargetSize` can cancel it. A claim whose phase is read
as "not live" in that window is a slot two requests can claim.

**`pending` is cancelable and `booting` is committed.** A boot that has started is never
cancelled: the startup Job, the coordination lock, the cordon, the node object and the machine are
all in flight, and a cancel leaves the Job in a state nothing owns — the exact residue
`docs/architecture/state.md` documents under "Why a transitional progress has to come from a
transition" (the 0.1.19 fix, commit `553e190`, removed the one out-of-band canceller:
`NodeGroupDecreaseTargetSize` writing `status.progress: shuttingdown` with no transition, no Job
and no lock). Under this model:

- Canceling a `pending` claim is deleting it. Nothing is in flight, the controller drops it, the
  unit comes off the target size on the same write.
- Canceling a `booting` claim is not possible. The finalizer lands on the **same write** that
  records the `booting` phase (`claim.Recorder.openBootWindow`), not on a later pass of the claim
  controller, so there is no window in which a `DeleteNodes` deletes a committed boot with no
  shutdown behind it. A delete is therefore not an abandonment: the claim runs to `ready` or
  `failed`, and the machine is powered off by the shutdown path. A boot is reversed, never
  interrupted. The claim controller's own `ensureFinalizer` is the belt for a phase a hand write
  landed, and it never stamps a finalizer on a claim that is terminal, pending, or already being
  deleted.

The shutdown Job that reverses a committed boot is a child of the claim that asked for it:
`setOwnerReference` makes the `NodeClaim` the controller owner of the lifecycle Job, and keeps the
`Node` as a secondary, non-controller owner so the Job is also readable under the slot in Argo CD's
tree. The ordering is what makes the reversal safe: the claim's finalizer comes off only once the
machine is observed off (`NodeClaimReconciler.reconcileDeletion`), so the claim cannot go away - and
its shutdown Job cannot be garbage-collected - while the shutdown is still running.

The `pending → booting` edge is also the attempt boundary: incrementing `status.attempt` is part
of the same write that creates the startup Job, so an attempt is a machine bought and nothing
else.

**The events of these edges land on the claim.** A phase is the claim's, so its timeline is
readable on it: `ClaimCreated`, `ClaimBooting`, `ClaimReady`, `ClaimFailed`, `ClaimCancelled`,
`ClaimCompleted`, `ClaimSuperseded`, one Event per transition, written by the same
`claim.Recorder` that writes the phase (`internal/claim/recorder.go`, `internal/claim/events.go`) —
so a move the table refused, a write the API server refused, and a re-apply that changed nothing
publish nothing. The table of the set, and the commands that read it, are in
[the CRD reference](../api-reference/crds/nodeclaim.md#events).

## 4. Deadlines and Attempts

### `spec.deadlineAt`

A claim is born with a budget. The budget is the operator's, measured from
`spec.requestedAt`:

```
budget = startup Job timeout + job monitor poll + registration wait + readiness gate + margin
       = DefaultJobTimeout (5m) + JobMonitorPollInterval (30s) + DefaultUncordonTimeout (90s)
         + DefaultReadinessGateTimeout (4m) + MaxUncordonRetryInterval (10s) + 1m
       = 12m10s
```

It is the same *kind* of expression the 0.1.20 progress watchdog derived for the same reason —
the sum of every deadline one attempt can consume, plus one sleep at the ceiling — and the
watchdog is gone with this model: the budget of a claim is what its deadline enforces. The
startup Job's own timeout is in the sum, because the clock runs from `spec.requestedAt`, i.e.
from **before the Job exists**. One attempt can spend the Job's whole 5m (the Job's timeout is
`DefaultJobTimeout`, and `JobTimeout` is a reachable event), then the 90s it takes the node to
register, then the 4m of the readiness gate: 10m30s of deadlines in series. A budget of
7m10s — the first sketch of this section, and the number the watchdog's grace used, which was
safe there because the grace applied only to a Job that had **already finished**
(`internal/progress/watchdog.go`, deleted) — fails a claim in the middle of the machine it is
still buying, and the machine that boots an instant later is force-shut-down as overdue.

A test computes the number from the fsm constants, and asserts the **set** the sum is built from
by name (`claim.BudgetConstants`), so growing a deadline without growing the budget fails, and so
does quietly dropping a member of the set: the assertion that a budget minus one constant is not
the budget is the one that catches the 7m10s arithmetic.

It is **floored by `Group.spec.maxNodeProvisionTime`**:

```
deadlineAt = requestedAt + max(budget, group.spec.maxNodeProvisionTime)
```

The floor is not decoration. cluster-autoscaler is told the group's number as its own scale-up
termination deadline (`NodeGroupGetOptions` → `MaxNodeProvisionDuration`,
`internal/grpcserver/server.go`), so it gives up on the slot at that instant. Enforcing the
shorter of the two would let the provider declare a claim dead while the autoscaler is still
waiting for it, and the autoscaler's answer to a group that stopped growing is another
`IncreaseSize`. `spec.request`'s sibling field for reporting is the number itself: the group's
`maxNodeProvisionTime` is copied onto the claim when it is created, so a claim says what budget
was in force, and a pool that changes the number does not rewrite history.

**Overdue means `failed`, and `failed` means force shutdown — for a claim in the boot window.**
When the deadline passes with the claim at `pending` (including the empty phase of a claim whose
first status write has not landed) or `booting`, the claim controller records `failed` with
`reason: DeadlineExceeded` and runs the shutdown path on the machine: cordon, shutdown Job, power
off. The force shutdown is the point of the deadline — a boot that has overrun is a machine that
may boot into a cluster that no longer wants it and starts serving pods on capacity the
autoscaler has already given up on. The machine goes off, the slot becomes free, the next claim is
attempt N+1.

The deadline bounds the **boot**, not the machine's life. `phase.InBootWindow` is the range the
deadline, and the attempt bound that rides on it, apply to, and the `booting → ready` write
clears `spec.deadlineAt` (`claim.Recorder.clearBootDeadline`, on the write *after* the status one):
a claim that reached `ready` owns a
machine, every healthy machine in a homelab pool is requested more than 12 minutes before it
serves its first pod, and a CRD whose deadline field still names an instant in its past is a
healthy unit that every later reader — the controller, the digest, `kubectl get nodeclaim` — reads
as an overdue attempt. Destroying capacity on a timer is the bug this sentence closes.

### The two bounds that outlive the boot window

**A ready claim whose machine is gone.** `ready` with the machine observed off is the one shape
the input law has no transition for: the report it derives is `booting`, law 1's `StartNode` is
not reachable from `startingup`, law 3 needs the machine observed up, and the boot deadline
closed on the write that made the claim ready. A machine deleted in a Hetzner console, a
deprovisioned server, or a `kubectl delete node` leaves a live claim, a slot reported `booting`,
and a unit the autoscaler pays for, with `reason: MachineGone` declared in the API and written by
nobody. The claim controller owns the case, bounded by an **observation** and not by a pass
(`NodeClaimReconciler.watchMachine`): the first pass that sees the machine off stamps
`MachineObservedOff` on the claim, a machine that comes back clears it and the claim stays
`ready`, and an observation that outlasts `claim.MachineGoneGrace` — one boot window, the same
horizon as the deadline, deliberately: a reboot is not an attempt that ends — records
`failed` / `MachineGone`. The clock is on the object, so a manager restart does not restart it.

**A scale-down whose shutdown never lands.** The shutdown Job is a script, and it can exit 0 with
the machine observed up (the pool's wrapper swallows the power-off). The claim's finalizer cannot
release, so the poll is permanent unless the attempts are counted and bounded. They are counted on
`status.shutdownAttempts`, **where the shutdown fires** — the state machine's `ShutdownNode`, on the
branch that created the shutdown Job (`recordShutdownAttempt` → `BumpScaleDownAttempt`) — so three
attempts are three shutdown Jobs and not three passes of a poll: the claim controller's
`reconcileDeletion` reads the machine every 15s and writes nothing, and a streak written there
reaches the bound in 45s with one Job behind it, starting the pause early. It is a counter of its
own, never `status.attempt`, which is the record of the machines the request bought and the seed of
the next claim's number. Past
`claim.MaxConsecutiveAttempts` the slot reconciler stops firing a shutdown that will not land, and
the claim carries the `ShutdownAttemptLimitReached` warning.

The refusal is a bounded pause: past `claim.ShutdownAttemptResetGrace` (30m) the streak is spent
no longer, the count restarts, and the shutdown fires again, so a pool whose script has been fixed
is picked up by a later pass with no hand edit, and the refusal's requeue is paced to the grace so
the pause ends on a pass. The streak also resets when the observation flips — a machine observed
off ends the claim `completed`, and the counter dies with the object that carried it, which is why
there is no reset heuristic on the slot. The manual escape hatch, for an operator who wants the
next attempt now, is one command because the count lives on the request:

```bash
kubectl patch nodeclaim <claim> -n homelab-autoscaler-system --subresource=status --type=merge \
  -p '{"status":{"shutdownAttempts":0,"lastShutdownAttemptAt":null}}'
```

### Attempts

Attempts are a property of the request, not of the slot:

- `status.attempt` is `1..N` for a slot, seeded from `Node.status.lastClaim.attempt + 1` when a
  claim is created.
- It is incremented by the `pending → booting` write, the same write that creates the startup Job
  (§3), so it counts machines bought, not reconcile passes.
- A bound on attempts is a bound on consecutive claims; there is no counter on the slot to
  accumulate, and no reset heuristic, because the object that carries the count dies with the
  episode. The bound is enforced on a claim in the boot window: a claim that has its machine
  (`ready`) is not failed by the number it landed on.
- A scale-down's attempts are a **separate** count, `status.shutdownAttempts`, on the claim that
  asked for the scale-down, incremented by the transition that fires the shutdown — one per
  shutdown Job, never per reconcile pass — with the same bound and a bounded pause (§4).

| Today | Under NodeClaim |
| --- | --- |
| `fsm.StartupAttemptLimit` (3) on the `ClaimRearmed` condition of the slot, reset at the readiness gate of `afterJobCompleted` (`internal/fsm/startup_attempts.go`) | Subsumed: `status.attempt` on the claim, bounded per slot by `status.lastClaim.attempt`. The reset rule disappears with the object — nothing on the slot to clear, and no lifetime sentence for a slot that is reused across claims. |
| `fsm.ShutdownAttemptLimit` (3) on the `ShutdownAttempts` condition of the slot, reset in two places (`internal/fsm/shutdown_attempts.go`) | Subsumed: a scale-down is a claim (`request.call: DeleteNodes`), so its attempts are counted on the claim that asked for it — one per shutdown Job, at the transition that fires it — and end when it ends. |
| The progress watchdog's re-arm (`progress.Settle`, `ClaimRearmed` / `ProgressReconciled`, `internal/progress/watchdog.go`) | Subsumed: re-arming a long-lived slot becomes "create the next claim (attempt N+1)"; settling a claim that produced nothing becomes "record `failed` at the deadline". The slot is never written back to a stable pair, because it never held the request. |
| The observed-power precedence rule (`ReconcileObservedPower`, 0.1.22) | Kept, and promoted to the general input law (§6). It stops being a writer of the FSM's state. |
| `Node.status.conditions` entries `ClaimRearmed`, `ShutdownAttempts` | Replaced by the one-line `Node.status.lastClaim` digest plus the claims themselves. The events (`ScaleUpRequested`, `ReadinessGateTimeout`, `ShutdownNotObserved`, `internal/fsm/recorder.go`) stay events, and gain the claim's name in their message. |

## 5. How cluster-autoscaler's RPCs Map

| RPC | Today | Under NodeClaim |
| --- | --- | --- |
| `NodeGroupTargetSize` | `targetSizeOf` counts a slot when `spec.desiredPowerState: on` **or** its progress is not one of `shuttingdown`/`shutdown`/`startingup` (`countsTowardsTargetSize`, `internal/grpcserver/server.go`) | The count of **live claims** for the group: `pending` + `booting` + `ready`. No progress values appear in the arithmetic. |
| `NodeGroupIncreaseSize(delta)` | Validates `delta > 0`, refuses a request that crosses `maxSize` (the slot count), then claims the **first** free slot it finds and returns: one slot per call, whatever `delta` says (`internal/grpcserver/server.go`) | Creates `delta` claims on `delta` free slots (free = no live claim), each with the request attributed, `attempt` seeded, `deadlineAt` set. The `maxSize` check is `liveClaims + delta > slots`. |
| `NodeGroupDecreaseTargetSize(delta)` | Cancels slots matching `isStartingUpSlot` (`on` + observed `off` + `progress: startingup`) by writing `spec.desiredPowerState: off`, oldest startup first, capped to what exists, floor judged on the achievable size | Cancels `|delta|` claims in phase **`pending`** (including the empty phase of a claim whose first status write has not landed) — deletes them, no finalizer, the unit comes off the target size on that write. **Never a `booting` claim** (§3). A request that can cancel nothing is **not** answered with success (§5.1). The cancellation is typed and attributed: it says which call, which delta, which group. |
| `NodeGroupDeleteNodes(nodes)` | Writes `spec.desiredPowerState: off` on each named slot, floor judged against `targetSizeOf` | Records the request on each named slot's claim (`request.call: DeleteNodes`, `request.nodes`) and cancels it: a `pending` claim is deleted, a `booting`/`ready` claim is deleted through its finalizer, which runs the shutdown path (cordon, shutdown Job) and then releases. Draining stays cluster-autoscaler's — it evicts before calling `DeleteNodes`, and `ToBeDeletedByClusterAutoscaler` is observed and recorded by
`internal/autoscaler/markers.go`. |
| `NodeGroupNodes` | Instances are derived from the presence of a `corev1.Node` plus the spec: `creating` (claim, no node object), `running` (node object, claimed on), `deleting` (node object, claimed off); a slot with no node object and no claim is not reported | **Contract unchanged.** `creating` = a live claim in `pending`/`booting` with no `corev1.Node`; `running` = `ready`; `deleting` = a claim marked for scale-down. |
| `NodeGroupTemplateNodeInfo` | Built from a live node or a placeholder fed by `spec.nodeCapacity` (`placeholderTemplateNode`, `applyTemplateCapacity`) | **Contract unchanged.** Template synthesis reads `Node.spec` only, which is what makes it correct in v2: the config object is a config object. |
| `NodeGroupForNode`, `NodeGroups`, pricing, `GetOptions` | Unchanged | Unchanged. |

### 5.1 The decrease that can cancel nothing

`NodeGroupDecreaseTargetSize`'s response carries no size, so a success is the provider asserting
"the size you asked for is in force". The entry point cancels `pending` claims only, and a group
whose live claims are all `booting` or `ready` has nothing to cancel. Answering that with success
is a lie with a long tail: cluster-autoscaler stops asking for the decrease, keeps paying for a
unit it asked away, and its next `IncreaseSize` for the same slot is answered
`FailedPrecondition … no free node slot` — a permanent-looking error for as long as the committed
boot takes, up to one boot deadline (12m10s) plus the TTL of the claim that carried it.

So the provider answers what it did:

- a group with no live claim is already at the requested size: a no-op **success**;
- a group whose every live claim is past the cancelable phase: **`codes.Unavailable`**, naming how
  many claims are booting and how many are ready. The state is transient by construction — a boot
  is reversed by its own deadline, a ready machine by `DeleteNodes` — and `Unavailable` is the
  code cluster-autoscaler retries, so the decrease is carried out when it becomes possible;
- a decrease that would take the group under `spec.minSize`: `codes.FailedPrecondition`, as before.
  That one is not retryable, and it should not be: the floor is a declaration.

`DecreaseTargetSize` is never the way to take a `ready` machine away. That path is
`DeleteNodes`, which drains first (cluster-autoscaler evicts the pods, then calls), and the
claim's finalizer is what makes the request non-forgettable (§3).

**A `failed` or overdue claim is not counted.** The target size counts live claims, so the state
"the autoscaler is paying for a machine that never arrived, forever" has no place to exist in.
Today that state is real and documented: a slot at `{desired: on, progress: ready}` with no
machine is counted by `NodeGroupTargetSize` forever, cannot be cancelled by
`NodeGroupDecreaseTargetSize` (its gate wants `progress: startingup`), and is kept alive on
purpose by the watchdog's claim gate — "the autoscaler's arithmetic … is not this controller's
answer to a scale-up" (`internal/progress/watchdog.go`). Under this model the claim hits its
deadline, becomes `failed`, stops being counted, force-shuts the machine, and the slot is free.

## 6. What `status.progress` Becomes

`status.progress` stays on `Node`, and its meaning changes: it is a **derived, reported phase**,
written by the observation reconciler, and **never read to decide anything**.

| Reported phase | Derived from | Today's value |
| --- | --- | --- |
| `off` | no live claim, no observed machine | `shutdown` |
| `boot pending` | a live claim in `pending`, no lifecycle Job yet | *(new — today the shape is `{desired: on, progress: shutdown}` and is indistinguishable from a free slot)* |
| `booting` | a live claim in `booting` | `startingup` |
| `ready` | a live claim in `ready`, node observed Ready and gate-clear | `ready` |
| `draining` | a claim marked for scale-down, node cordoned, machine still observed up | `shuttingdown` |

`status.powerState` keeps its current meaning: what the provider observation reports.

The FSM's input is no longer a recorded phase. Every decision reads:

```
(live claim phase, observed power, kubelet liveness / node readiness)
```

Three consequences, each of which is a rule in `docs/architecture/state.md` today:

- **The 0.1.22 precedence rule becomes the general input law, not a rule 2.**
  `progress.ReconcileObservedPower` exists because a record of what the FSM last did is not
  evidence about a machine, and a record that outlives the machine freezes a slot the FSM cannot
  leave. Once the FSM never reads a record for its input, "observed outranks recorded" is not a
  tie-break between writers of one field: it is the only reading, and the field is a report. The
  four-rule authority order collapses to two writers with disjoint jobs — the claim controller
  moves claims, the observation reconciler moves reports.
- **The progress watchdog's job is exactly "the claim passed its deadline".** The
  settle / re-arm / uncordon trio in `internal/progress/watchdog.go` (deleted with this model)
  existed to heal a long-lived slot left pointing at a machine that is not there. A claim that produced nothing has a deadline,
  and the deadline is the heal: `failed`, force shutdown, slot free, attempt N+1. The cordon is
  read, not reconciled from: a cordoned node is `phase.Unusable`, so a claim whose machine is
  cordoned is never stamped `ready`, and the readiness gate's loop is the writer that uncordons
  (`fsm.setNodeSchedulableWithRetry`, `abandonReadinessGate`). A cordoned node left standing is
  invisible to cluster-autoscaler's scale-down, so it is paid for forever — which is why the
  uncordon runs on the way out of a gate that timed out, and why a claim that is failed with a
  cordoned node behind it is force-shut-down rather than left serving.
- **`validateStateTransition`'s gate on transitional slots goes away.** Today no transition may
  start on a slot whose progress is `startingup`/`shuttingdown`, and a transitional progress may
  only be written by the operation that wrote it, because the field is both the FSM's state and
  the world's report. With claims, "an operation is in flight on this slot" is the existence of a
  claim in `booting`, which is a fact about an object, not about a string.

The same split runs through the reporting: the slot keeps its **events of the machine**
(`ScaleUpRequested`, `ScaleUpCompleted`, `ScaleDownRequested`, … on the `Node`, written by the
transition path), and the claim gets its **events of the attempt** (`ClaimCreated`,
`ClaimBooting`, `ClaimReady`, `ClaimFailed`, `ClaimCancelled`, `ClaimCompleted`,
`ClaimSuperseded` on the `NodeClaim`, written at the phase-write chokepoint). They are one episode
read from two objects, not the same sentence twice: the slot's event says what the machine did,
the claim's event says what happened to this request and carries the request's numbers — attempt,
startup Job, deadline, duration. The reported phase itself stays a report; the story of a request
has an object, and the object has a timeline.

## 7. What Is Deliberately Kept

**The coordination lock stays, for one more release, as a belt.** A live claim is *intended* to
be the lock — one claim per slot, created by the same code path, is the mutual exclusion the
annotations provide (`OperationLockAnnotation`, `LockOwnerAnnotation`, `LockTimestampAnnotation`,
`LockTimeoutAnnotation`, `internal/fsm/coordination.go`, `fsm.DefaultLockTimeout` 5m, `RenewLock`
across the readiness gate, expired-lock cleanup, force release at
`fsm.StuckTransitionThreshold` 15m). It is not the lock yet, because two things the lock currently
guarantees are not yet guaranteed by a claim:

- **Atomicity of the handover.** Today the lock is acquired in the `before_` hook, before the
  state change and before the `enter_` hooks that write progress
  (`docs/architecture/state.md`, "What a refused transition leaves behind"), so a scale-up and a
  scale-down on one slot cannot both take the slot apart. The claim equivalent is "create the
  claim and stamp the finalizer, and no other writer can get the slot in between", which needs the
  one-live-claim-per-slot enforcement to be airtight under concurrent writes.
- **A bounded ownership window.** A lock has a timeout and is force-released at 15m. A claim has a
  deadline, and a claim whose controller died is collected only when something reads its deadline.
  One release of running both is the cheap proof.

Removing the lock is listed in §10 as the first follow-up once claims are proven.

**The readiness gate stays, and becomes the gate for `phase: ready`.** Nothing about
`Group.spec.nodeReadinessTaints` (defaulted to Cilium's
`node.cilium.io/agent-not-ready:NoExecute`, `api/infra/v1alpha1/group_types.go`) changes:
`fsm.setNodeSchedulableWithRetry` waits for the node to be Ready **and** free of every declared
taint, with the two budgets separate — 90s for the node object to register
(`fsm.DefaultUncordonTimeout`), 4m for the gate from the moment the object appears
(`fsm.DefaultReadinessGateTimeout`) — and a timeout uncordons, records `ReadinessGateTimeout`, and
completes. The only change is what it gates: the scale-up transition's progress write becomes the
`booting → ready` move of a claim, so "this claim's machine can take the pool's work" is what the
phase asserts. The churn it closes is unchanged: a slot declared ready with the bootstrap taint
on it is read by the autoscaler as capacity its pending pod cannot land on, and the pool buys a
second machine for a node that is about to work.

## 8. Trade-offs and Costs

**More objects, and their collection.** Every scale-up is an object, so a pool that scales twice a
day writes claims forever. The cost is paid in three places, all of them explicit:

- **TTL.** Claims carry a time-to-live after they land: `failed`/`completed` are terminal, and the
  controller collects them after the TTL (the same knob the lifecycle Jobs already use,
  `spec.ttlSecondsAfterFinished` on `MinimalPodSpec`). A claim that never lands is collected by its
  deadline plus the TTL, so nothing accumulates on a broken pool.
- **OwnerRefs.** A claim is owned by its `Node`, so deleting a slot from Git takes its claims with
  it, and deleting a pool cleans up after itself with no reconciler. The lifecycle Jobs of an attempt
  are owned by the claim that asked for them, with the slot as a secondary owner: deleting the slot
  takes the claims and their Jobs, and while the slot lives a Job keeps its provenance
  (`infra.homecluster.dev/nodeclaim`, and the ownerReference behind it) after the claim is collected.
- **A digest on the slot, so the story is still one command.** `Node.status.lastClaim` keeps
  `{slot, phase, attempt, request.call, request.group, reason, deadlineAt, finishedAt}`.
  `kubectl describe node.infra.homecluster.dev <slot>` answers "who asked, when, how many
  attempts, why did it end" without a second command; `kubectl get nodeclaim -o wide` answers it
  for the whole pool. The GC is safe because the summary survives it.

**A new ownership boundary.** The claim controller owns the claim and the lifecycle Jobs that
hang off it (create, monitor, deadline, force shutdown). The slot reconcilers
(`internal/controller/infra/node_controller.go` today) keep the slot-level plumbing: the providerID,
the declared taints, the capacity-drift warning, the labels, the observation of the machine.
Two controllers, one object each, is the point — but the seam is new and it is where the first
bugs will live: the handover from "claim controller created a startup Job" to "observation
reconciler saw the node" is the same window the 0.1.20 `SettleGracePeriod` existed to cover, now
bounded by the claim's deadline instead of by a grace (§4).

**One claim per slot is load-bearing.** It is what makes "a live claim exists" a synonym for
"the machine is wanted", what keeps `TargetSize` a count of claims, and what keeps the model
cancellable. The API cannot express it (no cross-object uniqueness in a CRD), so it is enforced by
the controller: pick-a-free-slot and create-its-claim happen under the same lock as today, and a
second live claim on one slot is an operator-visible bug, not a supported state. Relaxing it to N
claims per slot returns the design to reconciling a *set* of requests against one machine — which
is the whole ambiguity this document is removing.

**It is a breaking API change.** `spec.powerState` is gone from a served version, and `v1alpha1`
goes away in the same release (§9). Every Node CR in every GitOps repo is edited by hand. The
alternative — keep the field, ignore it — leaves a field operators declare and nothing reads,
which is how the churn came back.

## 9. Migration

Direct cut. No deprecation, no compatibility shim, no conversion webhook.

- **Chart `0.2.0`** (`dist/chart/Chart.yaml`, today `0.1.22`). A minor bump is the release the
  breaking API lands in.
- **Served and storage version: `infra.homecluster.dev/v2alpha1`.** `v1alpha1` is removed from the
  served set and as a storage version in the same release, for `nodes`, `groups`, and the new
  `nodeclaims`.
- **No conversion webhook.** `v1alpha1` → `v2alpha1` is not a field rename: a scalar is replaced by
  an object, and there is no honest conversion for `spec.powerState: on` — it is a claim with no
  request, no attribution, no deadline, and no attempt. A webhook that invents those is worse than
  a rollout that says "the pool goes to zero, then comes back".

### Rollout order for an existing cluster

The CRDs, the operator and the manifests are applied in that order, and the pool is taken to zero
first. Nothing in the order is optional: each step's risk is the previous step's gap.

0. **Quiesce.** Scale cluster-autoscaler to 0, and record what is in flight:

   ```bash
   kubectl get nodes.infra.homecluster.dev -o custom-columns=\
   SLOT:.metadata.name,CLAIM:.spec.powerState,OBSERVED:.status.powerState,PROGRESS:.status.progress
   ```

   Every slot that reads `on` is a machine the autoscaler asked for. Note them: after step 4 the
   pool is empty by design, and the claims that refill it come from workload demand.

1. **CRDs.** Apply the `0.2.0` CRD set: `nodes` and `groups` at `v2alpha1`, plus `nodeclaims`.
   This repo renders CRDs from `config/crd/bases` into `dist/chart/templates/crd/`, so a
   template-rendered apply (`kubectl apply`, `argocd sync --replace`) moves the storage version and
   drops `v1alpha1` in one write. A `helm upgrade` that carries CRDs in `crds/` does **not**
   remove a served version — check how the release is applied before trusting this step.
   - *Risk:* the running `0.1.22` operator loses its watches the instant `v1alpha1` stops being
     served. Its informers fail with `no matches for kind Node in version
     infra.homecluster.dev/v1alpha1` and the manager restarts. The gRPC server runs in the same
     binary, so scale-up requests for the pool fail until step 2 lands. A machine that is up keeps
     serving pods; nothing is powered off in this window. Keep the window short, and do steps 1-2
     in one command.

2. **The operator.** Deploy `0.2.0`: the claim controller, the observation reconcilers, the RBAC
   for `nodeclaims`, and the gRPC provider that creates claims.
   - *Risk:* it reads the existing slots through the `v2alpha1` schema, sees no claims, and applies
     the law: **a machine observed up with no claim is unwanted.** A slot that was `powerState: on`
     in step 0 is cordoned and shut down. That is correct behaviour and it is why step 0 exists.
   - *Risk:* the old stored objects are read with a schema that has no `powerState`. The field is
     dropped from the read, not from etcd — see step 3.

3. **The GitOps manifests.** Re-apply every `Node` as `v2alpha1`, with no `spec.powerState`.
   Because `v1alpha1` is gone and there is no conversion, the objects are **recreated, not
   patched**: a value written under a removed field stays in etcd and reappears if the field is
   ever added back to a schema.

   ```bash
   kubectl delete nodes.infra.homecluster.dev -l group=remote-workers -n homelab-autoscaler-system
   kubectl apply -f apps/homelab-autoscaler/remote-workers/nodes.yaml   # apiVersion: .../v2alpha1
   ```

   - *Risk:* the slot's identity is its name and its labels. `metadata.name ==
     spec.kubernetesNodeName` is load-bearing (`NodeGroupDeleteNodes` looks the CR up by name, and
     `spec.providerID` is empty without a cloud-controller-manager), the `group` label is what
     `groupNodes` lists on, and **`maxSize` is the count of Node CRs in the group**
     (`internal/grpcserver/server.go`). A partial re-apply lowers the ceiling of the pool; a
     re-apply that renames a slot leaves its `corev1.Node` and its `providerID`
     (`homelab://<slot>`) matching nothing.
   - *Risk:* Argo CD keeps the pruned v1 objects alive if the sync policy is not `Replace` and
     prune is off, and a slot that exists in git and not in the cluster is capacity that will
     never be claimed.

4. **The pool reconciles to zero, then scales on demand.** With `v2` slots, no claims and no
   autoscaler, every slot is free and every machine is off — the idle state of a homelab pool.
   Scale cluster-autoscaler back up and watch the first request:

   ```bash
   kubectl get nodeclaim -n homelab-autoscaler-system -w
   ```

   `pending` → `booting` → `ready` inside `deadlineAt`, then the pending pod binds. If a claim
   reaches `failed`, read `status.reason` and `Node.status.lastClaim` before touching anything:
   that pair is the record this design exists to produce.

**Roll-forward is the supported path.** Rolling a cluster back to `0.1.22` means restoring the
`v1alpha1` CRD by hand (Helm does not downgrade CRDs), re-adding `spec.powerState` to every
manifest, and living with the claims the operator left behind. Do the rollout on a pool that can
be empty for ten minutes.

## 10. Open Questions / Future Work

**Claim-side cancellation for `DeleteNodes`, or a finalizer on the node.** Settled: the finalizer
goes on the **claim** (§11). The machine-deleted-by-hand case is read as the design says — the
deadline fails the claim, and the claim controller's finalizer path ends it once the slot's
observation reports the machine off — and deleting a claim by hand is a supported cancel
(`reason: DeletedByHand`). What stays open is the operator's view of the case the finalizer cannot
satisfy on a machine that is gone: today the deadline is what frees the slot. The design puts the
finalizer on the claim: deleting a `booting`/`ready` claim *is* the shutdown request, which keeps
"the boot is committed" true and gives the scale-down one object to report on. The cost is that a
claim then outlives the machine it asked for, and a machine deleted by hand — a Hetzner console, a
`kubectl delete node`, a deprovisioned server — leaves a finalizer nothing can satisfy unless the
controller reads "no node object, no lifecycle Job, past the deadline" as completion. The
alternative is a finalizer on the `Node`, where today's `handleNodeDeletion` already runs
(`internal/controller/infra/node_controller.go`), and a claim that is deleted with no cleanup at
all. Pick one before the CRD ships: a finalizer is not a field you remove later.

**Should `status.progress` be persisted at all?** Settled: it is not (§11). The derived report is
persisted, under a new name and a new vocabulary — `Node.status.phase` — because the field it
replaced was the FSM's state and the report at once. The `kubectl get` column survives, and
`kubectl get nodeclaim` is now the other command. What stays open is whether a derived field should
be persisted at all, and the answer is the same precedent the design cites: keep what an operator
reads in a crisis. §6 makes it derived and read by nobody, and a
value that is computed from `(live claim, observation)` can be computed on read instead of written
on every observation — no status write, no stale report, no watchdog grace to protect it. It
cannot be a computed field, so the choices are: keep persisting it (cheap for the writer, and
`kubectl get nodes.infra.homecluster.dev` keeps its `Progress` column), or drop the field and
report the phase in the claim only (`kubectl get nodeclaim` becomes the one command). The
precedent in this repo is the `NodeReadinessTaints` condition: a record of what a slot is gated
on, kept because an operator reads it in a crisis.

**Removing the coordination lock.** The first follow-up (§7). The work is the two guarantees a
claim does not yet provide: making the create-claim-and-stamp-finalizer write atomic with taking
the slot, and giving a claim whose controller died a collection path that does not need a lock
timeout. When both hold, the lock is a second mutex over the same critical section, and every
`LockConflictError`, `ScaleUpDeferred` / `ScaleDownDeferred` requeue and expired-lock cleanup
(`internal/fsm/coordination.go`, `checkOperationLock`, `cleanupExpiredLocks`,
`forceReleaseLock`) goes away with it.

**`Node.status.lastClaim`: one digest, or the last N attempts.** One digest is bounded, survives
GC, and answers "why is this slot like this". A list of the last N attempts makes
`kubectl describe` tell the retry story the claims no longer exist to tell, and puts a pruning rule
back on the slot — the same class of bookkeeping this design removes. Start with the digest, and
add the list when an operator asks for a history that outlives the TTL.

**Manual scale-up, and the hand-deleted machine.** There is no field an operator declares to get a
machine up: the manual action is creating a `NodeClaim` by hand, with `requestedBy` naming who did
it. The mirror case — a machine deleted out of the pool — is the shape the watchdog handles today
and the claim's deadline handles here; what is not settled is whether an operator is allowed to
delete a claim, or has to cancel it through the autoscaler.

## 11. Implementation Notes

Four places where the code diverges from, or sharpens, the sketch. Each is the model holding, with
one detail settled by what the tests showed.

- **`status.progress` is removed, not mirrored.** §6 said the field stays, derived and read by
  nobody. The implementation deletes it and ships the derived value as **`Node.status.phase`**, with
  the new vocabulary (`off`, `pending`, `booting`, `ready`, `draining`) and one formula
  (`phase.Derive`, `internal/phase/phase.go`). Keeping a field named `progress` alongside a claim
  phase named `booting` would have left two words for one fact, and the ambiguity this design
  removes is a vocabulary ambiguity first: `progress` had to be both "what the FSM last did" and
  "what the world looks like" because it was the FSM's state. A derived report has no such history,
  so it is a new field with a name that says what it is. `internal/progress` — the watchdog and the
  observed-power precedence rule — is deleted with it: the deadline of a claim and the input law
  are their jobs.
- **A readiness gate that times out fails the claim.** §3 describes the gate as holding a claim at
  `booting` until the node is Ready and free of the pool's readiness taints, and §4's budget includes
  that gate. The implementation goes one step further: `afterJobCompleted` moves the claim to
  **`failed` with `reason: ReadinessGateTimeout`** when the gate's 4m expires
  (`internal/fsm/hooks.go`). A machine that booted but never cleared the gate is exactly the
  attempt the model has a word for, and the row "a slot is not reported `ready` with the bootstrap
  taint on it" is enforced by the phase, not by a comment: the input law powers the machine off, the
  slot becomes free, and the next claim is attempt N+1. The node is still uncordoned on the way out,
  because a cordoned node is invisible to scale-down and is paid for forever.
- **The gate's read of the node is an input of the report it writes.** §3 says the readiness gate
  holds a claim at `booting` until the node is Ready and free of the pool's readiness taints, and §6
  says `ready` is "a live claim in `ready`, node observed Ready and gate-clear". The transition that
  answers for both runs in the Job monitor of the pass that started the boot, and that pass read the
  machine **off**: so the gate records the node object it just uncordoned into the machine's input
  (`fsm.recordGateAnswer`) next to the claim move, and the report of the same pass derives `ready`.
  Without it the transition's own write is refused by the staleness guard, the observation reconciler
  that would correct it never sees another event for a node that has settled into `Ready`, and the
  slot reports `booting` for a machine that is serving - with a claim, an observation and a node
  object that all say otherwise.
- **A shutdown whose machine is observed up reports `draining`, and counts its attempts on the
  claim.** §6 lists `draining` as "a claim marked for scale-down, node cordoned, machine still
  observed up". The implementation covers the wider case, and states it in
  `docs/architecture/state.md`: a shutdown Job that completed while the machine is still observed up
  leaves the slot at `draining` (`phase.Derive` reads `ShutdownJobInFlight` first), the claim keeps
  its finalizer, and the transition that fired the Job counts the attempt on the claim that asked
  for the scale-down (`fsm.recordShutdownAttempt` → `BumpScaleDownAttempt`) — one count per shutdown
  Job, and a claim controller poll of the observation that counts nothing. Past `claim.MaxConsecutiveAttempts` the slot reconciler stops firing a shutdown that
  will not land and the record is the `ShutdownAttemptLimitReached` warning on the claim. This is
  the design's own rule — the bound lives on the request — applied to the scale-down, and it is what
  the v1alpha1 `ShutdownAttempts` condition did on the slot, with its two reset sites.
- **"Live" and "wanted" are two questions, and the code names them.** §3's "a live claim per slot"
  and "the only shape `IncreaseSize` may claim" need the pair. `claim.LiveOfSlot` returns the slot's
  newest live claim, **including one a scale-down has marked** — that claim owns the slot, its boot
  is committed, and the finalizer is the request that reverses it, which is what makes the report
  row "a live claim, marked, observed up → `draining`" reachable. `claim.WantsMachine` is the
  stronger question, and law 1 of the input law asks it: a marked claim is not a boot target. The
  provider's `TargetSize` excludes a marked claim (its unit comes off on the write that named it)
  while `ClaimedSlots` includes it (the slot is not free), and `NodeGroupDeleteNodes` is idempotent
  over an already-marked claim.
- **The deadline bounds the boot, and the boot ends at `ready`.** The first sketch applied
  `spec.deadlineAt` to any live claim, and `IsLive` includes `ready`, so every healthy machine was
  destroyed 12m10s after its request: `failed/DeadlineExceeded`, a shutdown Job, a cordon. The
  bound is `phase.InBootWindow` (`pending`, counting the empty phase, and `booting`), the
  `booting → ready` write clears `spec.deadlineAt` so the CRD and the digest agree the attempt is
  over, and the same window bounds the attempt check — a claim that landed on `ready` as attempt 4
  is a success, not a fifth attempt.
- **The budget is the set of deadlines an attempt can spend, in series.** `claim.AttemptBudget`
  includes `fsm.DefaultJobTimeout`: the clock starts at `spec.requestedAt`, before the startup Job
  exists, so an attempt can spend 5m of Job + 90s of registration + 4m of gate (10m30s) and a
  7m10s budget kills it inside the machine it is buying. `claim.BudgetConstants` is the set, and
  the test asserts the sum, the membership of each constant, and that no proper subset adds up to
  the budget.
- **A deleted slot releases its claim.** A claim whose `Node` is gone has nothing left to run: the
  reason `SlotDeleted` is recorded, the finalizer comes off, and the pass that released it keeps
  the claim's TTL (`finishReleasedSlot`): the collection path every other terminal claim goes
  through is reached *through the slot*, and the slot is gone. Leaving the finalizer on is a claim
  stuck in `Terminating` forever, never retried, with the machine still running — and it is
  reachable through the documented rollout, which deletes the Node CRs (§9). The fact is re-read
  with `mgr.GetAPIReader()` before anything is given up: releasing a finalizer cannot be undone, and
  a cached `NotFound` on a slot the rollout recreated is a claim released while its machine is
  wanted. No digest is stamped — the record lives on the slot, and the slot does not exist, so the
  claim object is the record, and it survives for `spec.ttlSecondsAfterFinished` from the moment it
  was failed. A claim the scale-down had **already** marked is deleted by the API server on the
  write that removes its finalizer, and a claim an ownerReference points at can be taken by the
  slot's garbage collection at the same instant: neither reads its TTL, which is why the release
  path is the one that honours it, and why a hand-applied claim with no ownerReference is not left
  to accumulate on a cluster with no collector for it.
- **The digest is the newest episode's, and the collector never loses a claim.** `StampDigest`
  stamps only the newest claim of the slot (the ordering rule `claim.LiveOfSlot` uses), so a late
  write from a superseded episode cannot rewrite the story with the older one. A terminal claim
  with no `status.finishedAt` gets one stamped (`Recorder.StampFinishedAt`), because a phase landed
  on by a writer that stamped no time would otherwise never start its TTL and never be collected.
- **`ready` with the machine off has an owner: the claim controller, bounded by an observation**
  (§4). `ClaimReasonMachineGone` is written by `NodeClaimReconciler.watchMachine`, on
  `claim.MachineGoneGrace` (one boot window), with the clock on the claim
  (`MachineObservedOff`), so a reboot heals and a manager restart does not restart the bound.
- **A restarted manager re-attaches to its Jobs, correlated to the claim.** The lifecycle Job
  monitor is a goroutine of the process that ran the transition, and the coordination lock is its
  ownership marker. A restart loses the goroutine and keeps the objects, leaving a booting claim
  whose Job completed with no event left to fire — and it sits at `booting` until its deadline
  fails it, destroying a machine that booted successfully. The slot reconciler therefore watches
  the claims of its slot (`Owns(&NodeClaim{})`, so a claim created while the manager was down boots
  when it is created) and reads the Job objects — as **this claim's Job**, because a finished Job
  of the right phase on the right slot is not evidence about the operation running now:
  `startingup` is the state a `ready` claim with a machine momentarily observed off derives, exactly
  like a real boot, and re-firing the startup Job its readiness gate consumed long ago runs that
  gate again on a serving machine (`failed/ReadinessGateTimeout` in ~96s, instead of the
  `MachineGone` the 12m10s observation bound is there to record, and a power-off of the machine
  when it comes back). So the recovery is gated four ways: a live claim, in the boot window for a
  startup and named by a scale-down for a shutdown; the **newest** Job of the wanted phase
  (finished, because an unfinished newer one means the operation is in flight and its monitor owns
  its event — the newest *finished* Job is somebody else's attempt); a Job that does not predate
  the operation (`claim.BootCommittedAt`, `claim.ScaleDownRequestedAt`); and no live coordination
  lock. The shutdown branch is inert in the normal shape by those gates — `shuttingdown` derives
  only while a shutdown Job is in flight — and law 3 owns the restart case by firing the shutdown
  again.
- **A scale-down counts its own attempts** on `status.shutdownAttempts`, one per shutdown Job, at
  the transition that fires it (`fsm.recordShutdownAttempt` → `BumpScaleDownAttempt`), with a
  bounded pause (`claim.ShutdownAttemptResetGrace`) and a documented manual escape hatch (§4). The
  claim controller's finalizer poll of the observation is read-only: counting it counts reconciles,
  and at 15s a poll the bound arrives in 45s with zero Jobs, which trips it after one real attempt.
  Spending the *boot* `status.attempt` on a shutdown that never lands is the bug the separate
  counter closes: the boot attempts are the record of the machines the request bought, and the seed
  of the next claim's number.
- **The finalizer arrives with the boot, the deadline leaves at ready.** `claim.Recorder.Move`
  stamps the finalizer on the metadata write that precedes the `booting` status write — a manager
  that dies between the two leaves the boot committed with its deadline intact — and clears
  `spec.deadlineAt` on the write that **follows** the `ready` status write. The two orders are
  opposite because the two shapes a crash between them leaves behind are not equally healable: a
  `ready` claim that still carries its deadline has an inert field (every guard reads the deadline
  inside the boot window) and an idempotent re-apply of the move that clears it, while a `booting`
  claim with no deadline has nothing left to fail it once the Job's TTL removes the Job.
  `ensureFinalizer` in the claim controller is the belt for a phase a hand write landed, re-Gets
  before it writes, and refuses a finalizer on a pending, terminal, or already-deleted claim.
- **`status.phase` has no CRD default, on purpose.** The status subresource strips `status` from a
  create, so a default there is only stamped by the first status write, and the window it appears
  to cover is the window the model has to survive: `phase.IsLive` reads an empty phase as pending,
  and every reader that decides whether a machine is wanted asks `IsLive` (§3).
- **The decrease that can cancel nothing says so** with `codes.Unavailable` (§5.1).
- Two smaller settlements: `spec.maxNodeProvisionTime` is the claim's field for the number in force
  (§4 called it "the sibling field for reporting"), and one-live-claim-per-slot is enforced at
  admission as well as in the claim controller (`NodeClaimCustomValidator`), so the rule is refused
  where it can be seen and repaired where it cannot.

The annotations above are the record of where the implementation settled a question the sketch left
open. Everything else in this document is the model as it runs.

## Related Documentation

- [Architecture Overview](../architecture/overview.md) — the components this design changes
- [State Management](../architecture/state.md) — the FSM, the authority order for
  `status.progress`, and the rules §6 collapses
- [Node CRD](../api-reference/crds/node.md) — the v1alpha1 fields this design removes and keeps
- [Group CRD](../api-reference/crds/group.md) — `maxNodeProvisionTime`, `minSize`,
  `nodeReadinessTaints`
- [Known Issues](../troubleshooting/known-issues.md) — the symptoms the 0.1.19–0.1.22 fixes were
  patches on
- [Implementation Status](../development/IMPLEMENTATION-STATUS.md) — what is built today
