# State Management

The operator decides nothing from a record. A machine is wanted exactly when a live `NodeClaim`
names its slot, and every power decision reads the same three facts:

```
machine is wanted  ⇔  a live NodeClaim (pending, booting, ready) exists for the slot

input of every decision = (live claim phase, observed power, node usability)
```

`Node.spec` has no field for an intent in `v2alpha1`, and `Node.status.phase` is a report computed
from that triple. The model and the reasoning behind it are in
[the NodeClaim design](nodeclaim.md); this file is what the running code does.

## The authority table

Two writers, with disjoint jobs. Nothing else decides anything.

| Question | Who answers | Where | What it may write |
| --- | --- | --- | --- |
| Is a machine wanted on this slot, and how did the request end? | the **claim** | the claim controller (`internal/controller/infra/nodeclaim_controller.go`) and the lifecycle transitions, both through `internal/claim/recorder.go` | `NodeClaim.status` (phase, reason, attempt, conditions, finishedAt) |
| What is the cluster observing about that machine? | the **observation** | `internal/controller/core/node_controller.go` (and the transition path, through `phase.Derive`) | `Node.status.powerState`, `Node.status.phase`, the report's own history (`lastTransitionTime`, `previousPhase`, the ring and the activation counts, all through `phase.Record`), the autoscaler marker conditions |
| What lifecycle operation runs on the slot next? | the **input law**, derived, never stored | `internal/phase/phase.go` + `internal/controller/infra/lifecycle.go` | lifecycle Jobs, cordons, the coordination lock, the claim moves listed above |

The claim decides. The observation reports. A report is never an input: `Node.status.phase`,
`Node.status.lastClaim` and every `Node.status.conditions` entry are outputs, and no reconciler
reads them to choose an action. That is the whole answer to the class of bug the 0.1.20 watchdog
and the 0.1.22 observed-power precedence rule existed to patch — a field that was both the state
machine's own state and the world's report, written by four controllers. There is one such field
left (`status.phase`), it has one formula, and it decides nothing.

## The observability fields

`Node.status` carries six fields that exist to be read and are inputs to nothing:
`lastTransitionTime`, `previousPhase`, `recentTransitions` (the bounded display ring: 32 rows,
24 hours, in the order the writes landed), `recentActivations` (the ledger of the boots inside the
window, 512 rows of one timestamp each), `activationsTotal` and `activationsLast24H`. `Group.status`
carries the same kind of thing one level up: `slots`, `on`, `off`, `health`, `lastScaleAt`,
`lastScaleAction`. They are **derived and materialized**:
derived, because each is a function of facts the model already computes - the phase,
the live claim, the observation - and materialized, because the answer has to be on the object when
an operator asks, hours after the Events that recorded it were garbage-collected and after the claim
that caused it was collected by its TTL. `activationsLast24H` is the pure case of the pattern, and
the case that decides where the bound goes: it is the length of a ledger of the window's boots, not a
count read through the display ring's cap, because a bound a count is read through is a bound on the
count - a slot that churns 40 times a day writes 80 ring rows, keeps 32 of them, and would report 16.

A slot's phase and its history have one writer,
`phase.Record`, called by the passes that already own a phase write (the observation, the transition,
the seed of a never-provisioned slot) and by the pass the window's expiry schedules. Nothing reads
them to decide: the state machine's input stays the triple, `Derive`'s inputs are the triple, and
`internal/phase/hygiene_test.go` asserts on the source that no code outside the writer touches the
ring, the counts, `previousPhase` or `lastTransitionTime`. That is the law this repository keeps
paying for - a field that is both the machine's state and the world's report is how a slot gets
stranded - and the new fields are the shape of that bug in its most tempting form, a history you can
consult "just to know what happened last".

## The input law

`phase.Input` is the whole input. `phase.State` turns it into the state the node state machine
starts a pass in; `phase.Derive` turns it into the value of `Node.status.phase`. Both are pure
functions of the same triple, so the state and the report cannot disagree, and a test of one is a
test of the other.

| Live claim | Observed power | Node usable | Report (`status.phase`) | State | Fires |
| --- | --- | --- | --- | --- | --- |
| none | off | — | `off` | `shutdown` | nothing |
| none | on | — | `draining` | `ready` | `ShutdownNode` |
| `pending` | off | — | `pending` | `shutdown` | `StartNode` |
| `pending` | on | — | `booting` | `startingup` | nothing (the Job monitor owns it) |
| `booting` | any | any | `booting` | `startingup` | nothing (the Job monitor owns it) |
| `ready` | on | yes | `ready` | `ready` | nothing |
| `ready` | on | no | `booting` | `startingup` | nothing (the readiness gate owns it) |
| `ready` | off | — | `booting` | `startingup` | nothing fires here; the claim controller ends the claim `failed` / `MachineGone` past one boot window ("A ready claim whose machine is gone") |
| live, marked for deletion | on | — | `draining` | `ready` / `shuttingdown` | `ShutdownNode`, then the claim's finalizer releases |
| live, marked for deletion | off | — | `off` | `shutdown` | the claim ends `completed`, the finalizer comes off |
| `failed` / `completed` | on | — | `draining` | `ready` | `ShutdownNode` (a terminal claim is not a want) |
| `failed` / `completed` | off | — | `off` | `shutdown` | nothing |

Three rules fall out of the table, and they are the rules the v1alpha1 docs stated as a precedence
order:

- **A record never outranks an observation.** "Observed `on` outranks a recorded `shutdown`" was
  rule 2 of four. Now the record is not an input at all, so there is nothing to rank.
- **A transitional slot is identified by objects, not by a string.** "An operation owns this slot"
  is a claim in `booting` plus a lifecycle Job with the slot's label
  (`lifecycleJobsInFlight`), so the old `validateStateTransition` gate on a transitional
  `status.progress` is gone.
- **A machine with no claim is unwanted**, including a machine whose claim just failed, and
  including one the provider observation found after its claim was collected. The force shutdown
  of an overdue boot is not a special case: a terminal claim is not live, which is the same row.

`phase.Unusable` is the single predicate for "the node can take the pool's work": the
`corev1.Node` exists, reports `NodeReady=True`, carries none of the Group's
`spec.nodeReadinessTaints`, and is not cordoned. The readiness gate, the claim's `ready` phase and
the derived report all ask it the same way.

## The node state machine

The states and events are unchanged; what changed is where the machine starts.

| State | Meaning |
| --- | --- |
| `shutdown` | no machine: a free slot, a claim in `pending` that has not started, or a machine that just went off |
| `startingup` | the startup operation is in flight (`JobCompleted` / `JobFailed` / `JobTimeout` are reachable) |
| `ready` | the machine is up, and the transition to fire from here is `ShutdownNode` |
| `shuttingdown` | the shutdown operation is in flight |

```
StartNode:     shutdown     -> startingup      (creates the startup Job, moves the claim pending -> booting)
ShutdownNode:  ready        -> shuttingdown    (cordons, creates the shutdown Job)
JobCompleted:  startingup   -> ready           (the readiness gate answers for the claim)
JobCompleted:  shuttingdown -> shutdown        (the machine is observed off; the scale-down claim ends)
JobFailed:     startingup   -> shutdown        (the claim ends failed, reason StartupJobFailed)
JobFailed:     shuttingdown -> ready           (the claim stays, and its attempt is counted)
JobTimeout:    startingup   -> shutdown        (reason StartupJobTimeout)
JobTimeout:    shuttingdown -> ready           (reason ShutdownJobTimeout)
ForceCleanup:  transitional -> shutdown        (reason StuckTransition)
```

`NewNodeStateMachine` seeds the state from `phase.State(input)`
(`internal/fsm/node_state_machine.go`).
A caller that passes no input gets the observation alone — a slot with no claim. The v1alpha1
`getCurrentState`, which read `status.progress` back off the slot, is gone.

The transitions run in `handleStateTransitionsWithFSM`, in the order of the law:

1. a live claim that wants a machine, the machine off → `StartNode`;
2. a live claim, the machine on, the node usable → nothing (capacity serving pods);
3. no live claim (or a marked one), the machine observed on → `ShutdownNode`;
4. a terminal claim with the machine on → the same branch as 3.

Law 1 uses `claim.WantsMachine`: a claim a scale-down marked is live and owns the slot, and it is
not a want. That is the difference between "the boot is committed and is being reversed" and "the
next thing to do is boot".

## Claim phases, and who writes each move

The phase table of the design, as the running code enforces it
(`claim.LegalMoves`, `internal/claim/recorder.go`
— a move the table refuses is `ErrIllegalMove`, and nothing is written):

| Phase | Meaning | Legal next | Written by |
| --- | --- | --- | --- |
| `pending` | claimed, no lifecycle Job yet. The only cancelable phase. | `booting`, `ready`, `completed`, `failed` | created by the provider (the gRPC handler) with **no phase at all** — the status subresource strips `status` from a create, and there is no default on the field — and read as `pending` by `phase.IsLive` / `phase.IsPending` |
| `booting` | the startup Job exists and the power-on has been asked for. Committed. | `ready`, `completed`, `failed` | the `StartNode` transition: the `pending → booting` write is the same write that created the startup Job. It does **not** touch `status.attempt`: that was stamped on the claim's first pass (`claim.Recorder.Seed`) and never rewritten |
| `ready` | the machine is up, joined, and free of the pool's readiness taints | `completed`, `failed` | `afterJobCompleted` in the startup direction, i.e. the readiness gate (`fsm.setNodeSchedulableWithRetry`) |
| `failed` | the attempt produced no usable machine and never will. Terminal. | — | the claim controller (deadline, attempt bound, superseded, slot deleted), the Job monitor (`JobFailed` / `JobTimeout`), the readiness-gate timeout, a force cleanup |
| `completed` | the claim ended with the machine available. Terminal. | — | the claim controller's finalizer path, once the machine is observed off |

Two consequences worth stating plainly:

- **Canceling a `pending` claim is deleting it.** Nothing is in flight, no finalizer is on the
  object, and the unit comes off the target size on that write.
- **Canceling a `booting` claim is not possible.** The finalizer arrives with the boot, so a
  delete is the request that reverses it: cordon, shutdown Job, power off, then `completed`. A
  boot is reversed, never interrupted. `NodeGroupDecreaseTargetSize` therefore cancels `pending`
  claims only, and `NodeGroupDeleteNodes` cancels a `booting`/`ready` claim through its finalizer.

Both records of a move are written at that one chokepoint, on the claim. The **conditions**
(`Ready`, `MachineCommitted`, `MachineObservedOff`) are the durable record: they live on
`status.conditions` as long as the claim does, and the slot's digest outlives the claim, so they are
what answers "what is true about this attempt" the day after. The **Events** (`ClaimCreated`,
`ClaimBooting`, `ClaimReady`, `ClaimFailed`, `ClaimCancelled`, `ClaimCompleted`,
`ClaimSuperseded` — `internal/claim/events.go`) are the timeline: one Event per transition, published
on the claim after the status write landed, and nothing published for a move the table refused, for a
write the API server refused, or for a re-apply that changed nothing. They carry the attempt's
numbers (attempt, Job, deadline, duration), they coalesce on repeats, and they expire with the
cluster's Event TTL (one hour by default) — so `kubectl describe nodeclaim` is the "what just
happened, in order" view, the conditions and the digest are the "what is true" view, and both are on
the claim.

## Deadlines and attempts

`spec.deadlineAt` is born with the claim, measured from `spec.requestedAt`
(`claim.AttemptBudget` / `claim.DeadlineAt`):

```
budget = DefaultJobTimeout (5m) + JobMonitorPollInterval (30s)
       + DefaultUncordonTimeout (90s) + DefaultReadinessGateTimeout (4m)
       + MaxUncordonRetryInterval (10s) + 1m
       = 12m10s
deadlineAt = requestedAt + max(budget, group.spec.maxNodeProvisionTime)
```

The startup Job's own timeout is in the sum because the clock runs from `spec.requestedAt`, i.e.
from before the Job exists: one attempt can spend the Job's whole 5m, then the 90s it takes the
node to register, then the 4m of the readiness gate — 10m30s of deadlines, in series. A
budget of 7m10s (the first sketch of §4) fails the claim in the middle of the machine it is still
buying, and the machine that boots an instant later is force-shut-down as overdue. The 0.1.20
progress watchdog excluded the Job timeout for exactly this reason — its grace applied only to a
Job that had already finished — and a claim's deadline has no such restriction to inherit.

The floor is not decoration: cluster-autoscaler is told the group's number as its own scale-up
termination deadline (`NodeGroupGetOptions` → `MaxNodeProvisionDuration`), so a claim that died
before that instant is a slot the autoscaler is still waiting for, and its answer is another
`IncreaseSize`. The number in force is copied onto the claim (`spec.maxNodeProvisionTime`) so a
pool that changes it does not rewrite history.

**Overdue means `failed`, and `failed` means force shutdown — inside the boot window.** The claim
controller records `failed` with `reason: DeadlineExceeded`; the next pass of the slot reads "no
live claim, machine observed on" and powers the machine off. The bound applies to `pending` and
`booting` (`phase.InBootWindow`), and the `booting → ready` write clears `spec.deadlineAt`: a
claim that reached `ready` owns a machine, and a boot budget measured 12 minutes earlier is not a
licence to destroy capacity that is serving. The same window bounds the attempt check; a claim
that landed on `ready` with attempt 4 is a success, not a fifth attempt. This is the whole
replacement for the v1alpha1 progress watchdog: the settle / re-arm / uncordon trio healed a
long-lived slot left pointing at a machine that is not there, and a claim that produced nothing
has a deadline that does the same thing with a record attached.

Attempts live on the request, not on the slot:

- `status.attempt` is seeded once, on the claim's first pass, from `Node.status.lastClaim.attempt`
  (`claim.SeedAttempt`): +1 while the slot keeps failing, and back to 1 after an episode that
  ended `ready` or `completed`.
- nothing writes the field after that seed — not the `pending → booting` write, not a reconcile pass —
  so one claim is one attempt, and the number names the machine that claim buys;
- `claim.MaxConsecutiveAttempts` (3) is the bound. Past it, the claim controller fails the claim
  with `reason: AttemptLimitExceeded` before a machine is bought.
- a scale-down's attempts are counted on the claim that asked for the scale-down, on
  `status.shutdownAttempts`, by the transition that fires the shutdown (`fsm.ShutdownNode` →
  `recordShutdownAttempt` → `BumpScaleDownAttempt`) — one per shutdown Job, never per reconcile
  pass, because the claim controller's finalizer poll of the observation runs every 15s and a
  streak counted there is 45s of polling with one Job behind it. It is a counter of its own, so a
  shutdown script that never lands does not spend the boot attempts of the episode it is
  draining. The slot
  reconciler stops firing a shutdown that will not land once the streak is spent, and the streak
  resets when the machine is observed off (the claim ends with the observation) or after
  `claim.ShutdownAttemptResetGrace` (30m), which is what picks up a pool whose script has been
  fixed. See "A shutdown Job that succeeded is not a machine that is off".

The v1alpha1 machinery for the same two bounds — the `ClaimRearmed` and `ShutdownAttempts`
conditions on the long-lived slot, `fsm.StartupAttemptLimit`, `fsm.ShutdownAttemptLimit`, and the
reset heuristics each of them needed (the readiness gate of `afterJobCompleted`, and two more
sites for the shutdown counter) — is gone. The object that carries the count dies with the
episode, which is why there is nothing to reset.

## A ready claim whose machine is gone

`ready` with the machine observed off is the one row of the table that fires nothing:
`phase.Derive` reports `booting` (law 1's `StartNode` is not reachable from `startingup`), law 3
needs the machine observed up, and the boot deadline closed on the write that made the claim
ready. A machine deleted in a provider console, a deprovisioned server, or a `kubectl delete node`
therefore leaves a live claim, a slot reported `booting` forever, and a unit cluster-autoscaler is
paying for.

The claim controller owns the case, bounded by an observation, not by a pass
(`NodeClaimReconciler.watchMachine`):

- the first pass that sees the machine off stamps `MachineObservedOff=True` on the claim, and its
  `lastTransitionTime` is the clock;
- the machine coming back flips the condition off, and the claim stays `ready` — a reboot shows
  the same shape for minutes, and is not an attempt that ends;
- an observation that outlasts `claim.MachineGoneGrace` — one boot window, the same horizon as the
  deadline — ends the claim `failed` with `reason: MachineGone`, and the input law makes the slot
  free again.

The bound lives on the object, so a manager restart does not restart the clock.

## The coordination lock is a belt

A live claim is intended to be the mutual exclusion; it is not that yet, so the annotations stay as
a belt
(`internal/fsm/coordination.go`,
`fsm.DefaultLockTimeout` 5m, `RenewLock` across the readiness gate, `cleanupExpiredLocks`, force
release at `fsm.StuckTransitionThreshold` 15m):

```yaml
annotations:
  homelab-autoscaler.dev/operation-lock: "scale-up"
  homelab-autoscaler.dev/lock-owner: "node-controller"
  homelab-autoscaler.dev/lock-timestamp: "2024-01-01T00:00:00Z"
  homelab-autoscaler.dev/lock-timeout: "300"
```

Two guarantees the lock provides and a claim does not yet:

- **Atomicity of the handover.** The lock is taken in the `before_` hook, before the state change
  and before the `enter_` hooks. The claim equivalent is "create the claim and stamp the finalizer
  atomically with taking the slot", which needs one-live-claim-per-slot to be airtight under
  concurrent writes.
- **A bounded ownership window.** A lock has a timeout and is force-released at 15m. A claim has a
  deadline, and a claim whose controller died is collected only when something reads its deadline.

### What a refused transition leaves behind

- The acquisition runs in the `before_` hook, so a refused transition writes nothing: no lifecycle
  Job, no cordon, no claim move, no report.
- The refused operation is not lost. Its intent is the live claim of the slot, which is what the
  reconciler drives the machine from, so the same branch runs again on the requeue
  (`deferTransitionOnLockConflict` requeues at the deadline the lock itself names) and records a
  Normal `ScaleUpDeferred` / `ScaleDownDeferred` event. In v1alpha1 the intent was
  `spec.desiredPowerState`; the scalar is gone and the claim replaced it.
- Every transition that ends an operation releases the lock, `JobTimeout` included: a lock must
  not outlive the operation that wrote it.

## Ready means the machine can take the pool's work

`Group.spec.nodeReadinessTaints` (defaulted to Cilium's `node.cilium.io/agent-not-ready:NoExecute`)
gates the claim's `ready` phase. `fsm.setNodeSchedulableWithRetry` holds the transition into
`ready` open with two separate budgets — `fsm.DefaultUncordonTimeout` (90s) for the node object to
register, `fsm.DefaultReadinessGateTimeout` (4m) for the gate from the moment it appears — and
renews the coordination lock on every pass, because the gate can outlast the lock's timeout.

The gate's answer is the claim's move (`internal/fsm/hooks.go`):

- satisfied → the node is uncordoned, the declared `spec.nodeTaints` are stamped, and the claim
  moves `booting → ready`;
- timed out → the node is uncordoned anyway (a cordoned node is invisible to cluster-autoscaler's
  scale-down and is paid for forever), a `ReadinessGateTimeout` warning is recorded on the slot,
  and **the claim moves to `failed` with that reason**. The machine is then force-shut-down by the
  input law, the slot becomes free, and the next claim on it is attempt N+1.

The satisfied branch records its own read of the node into the machine's input
(`fsm.recordGateAnswer`), and that is not a detail: the machine that answers for a boot is the Job
monitor of the pass that **started** it, and the triple that pass read is the machine observed
**off** with no node object - the shape every real boot starts in. The claim move is recorded into
the input, and so must the other two inputs the gate spent its time establishing: the node object
the uncordon left behind, asked with the one predicate the report uses (`phase.Unusable`), and its
`Ready` condition, which is the definition `status.powerState` is written from.

Deriving the report from the boot's snapshot instead is the bug this closes. The staleness guard of
`reportPhase` refuses the write - correctly, that machine's inputs describe a machine that is not
up - and the writer that would correct it is the observation reconciler, which runs on
`corev1.Node` events, and a kubelet that has settled into `Ready` writes no more of them (its
heartbeats go to its Lease, its status posts every five minutes). What is left is a live `ready`
claim, a machine observed on, a `Ready` uncordoned node, and a slot reporting `booting` for the
life of the machine - the autoscaler reading the pool as buying a machine it already has, and the
pass that reads the true triple (`claimPhase=ready observed=on usable=true state=ready`) logging
that it is in a stable state and writing nothing, which is what a stable state does.

Under v1alpha1 a gate that timed out completed the transition and recorded the slot `ready`. Under
the claim model a slot is not reported as capacity that can take the pool's work when the request
that bought it failed to produce it.

## The report has two writers, and the write is a compare-and-set on the observation

`Node.status.phase` is written by two paths, and both call the one formula:

- the **observation reconciler** (`internal/controller/core`) is woken by the `corev1.Node`, writes
  `status.powerState` from it, and records the report that triple derives in the same write;
- the **slot reconciler** (`internal/controller/infra`) drives the lifecycle transitions, and every
  pass it takes over a slot records the report its read of the triple derives - transition or no
  transition (`fsm.ReportPhase`).

The second is not a redundancy. A change of the triple with no operation behind it - a claim that
ended on its deadline, a machine powered out of band, a scale-down's mark landing on a claim - has
no transition to fire, and the pass that reads it is the only writer holding its evidence. The other
writer runs on `corev1.Node` events, and the shapes that need the report corrected are exactly the
ones that have stopped producing them: the node object of a machine whose kubelet is dead has no
kubelet left to move it, and a kubelet settled into `Ready` sends its heartbeats to its Lease and
posts no further status. A pass that read a true triple and wrote nothing would leave the slot
reporting the last thing that happened to have an operation behind it, for as long as the slot's
resync (`idleResync`, 5m) says.

A transition's own report write (`fsm.reportPhase`) is a **compare-and-set on the observation**: it
reads the slot again, and records nothing when the slot's recorded power has moved past the record
the machine was built from - the observation path has been there, and wrote the report of that
observation with it. The exception is a machine that has read the node object itself: the readiness
gate stands in front of the machine it is answering for, and its live read outranks the record on the
slot (`observedLive`). Without that exception, a boot that lands while its own pass watched the
machine off reports `booting` for the life of the machine; without the compare-and-set, a shutdown
that read the machine up stamps `draining` over the `off` a silent kubelet has just produced, and
nothing is coming after it to undo the stamp.

The consequence for an operator is the one the k3d suite met as a flake: a slot can report a machine
the pool does not have, and the read that says so is a pass over the slot. Any write to the slot
wakes one, and the suite uses an annotation the operator reads nothing into
(`e2e.homecluster.dev/pass-requested-at`, `requestSlotPass` in `test/e2e`) so that asking for the
pass is never mistaken for asserting the answer.

Pinned by `TestReportPhaseCompareAndSetsOnTheObservation` (the two directions, on one write) and by
the specs "keeps the report the observation landed when a slower transition writes after it" and
"records the end of a claim on a slot whose machine is down" in the claim-model envtest suite.

## A shutdown Job that succeeded is not a machine that is off

The shutdown Job is a script. It exits 0 when its own commands ran, which is not evidence about a
machine: the power-off can be a no-op, the BMC can be unreachable behind a wrapper that swallows
it, and the node object stays `Ready` for `node-monitor-grace-period` (~40s) after a real
`shutdown -h now`.

So the shutdown completion (`internal/fsm/shutdown_completion.go`) reads the machine, and the
report follows the machine:

- the shutdown Job completed and the machine is observed **up** → the slot keeps reporting
  `draining`, the scale-down claim keeps its finalizer, and the transition that fired the Job
  counts the attempt on `status.shutdownAttempts` (`fsm.recordShutdownAttempt`) — a counter of its
  own, counted on shutdown Jobs, because `status.attempt` is the record of the machines the request
  bought and a shutdown that will not land must not spend it. The claim controller's poll of the
  observation is read-only.
  Past `claim.MaxConsecutiveAttempts` the slot reconciler stops firing a shutdown that will not
  land, the record is the `ShutdownAttemptLimitReached` warning on the claim, and the refusal
  requeues.

  The pause is bounded, not a sentence: past `claim.ShutdownAttemptResetGrace` (30m) the streak is
  spent no longer, the count restarts, and the shutdown fires again. A pool whose shutdown script
  has been fixed is therefore picked up by a later pass, with no hand intervention — **30 minutes
  later**, not on the next one, and the next pass after the bound is paced to that deadline. The
  escape hatch for an operator who will not wait for the grace is the one the claim model makes
  possible, the counter living on the request:

  ```bash
  kubectl patch nodeclaim <claim> -n homelab-autoscaler-system --subresource=status --type=merge \
    -p '{"status":{"shutdownAttempts":0,"lastShutdownAttemptAt":null}}'
  ```

  The next pass of the slot reconciler fires the shutdown again.
- the machine is observed **off** → the scale-down claim ends `completed` (reason
  `NodeDeleted` / `CancelledByDecreaseTargetSize` / `DeletedByHand`, from the request that made
  it), the finalizer comes off, and the slot reports `off`.

That is the row the v1alpha1 docs stated as "a `shuttingdown` progress is moved only by the
operation that wrote it", with the addition the model makes possible: the episode has an object to
say which attempt this was, and an end.

## One live claim per slot

The invariant the whole model rests on, enforced twice:

- at admission, `NodeClaimCustomValidator`
  (`internal/webhook/infra/v2alpha1/nodeclaim_webhook.go`):
  a second live claim on a slot is refused, and the message names the slot and the live claim. A
  terminal write is never refused, and a delete is never refused;
- in the controller, `refuseExtraLiveClaims`: the newest live claim owns the slot, every older one
  fails itself with `reason: Superseded`. This is the path admission cannot see — two providers
  racing, a hand-applied claim, a webhook that was down.

Relaxing it means reconciling a *set* of requests against one machine, which is the ambiguity this
model removes.

## Collection

A claim is owned by its slot, so deleting a slot from Git takes its claims with it. A terminal
claim is deleted after `spec.ttlSecondsAfterFinished` (default 3600s) from `status.finishedAt`.
The story survives the collection in `Node.status.lastClaim`: `{name, phase, attempt, call, group,
reason, requestedAt, deadlineAt, finishedAt}`.

```bash
kubectl get nodeclaim -n homelab-autoscaler-system -o wide
kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<slot>
kubectl describe nodes.infra.homecluster.dev <slot> -n homelab-autoscaler-system   # status.lastClaim
```

## Async operations

Both lifecycle Jobs are created by the transition that takes the lock, carry the slot's name and
the lifecycle phase as labels, and are cleaned up by `spec.ttlSecondsAfterFinished` of the
`MinimalPodSpec` the slot declares. A Job is owned by the claim that asked for it — the controller
ownerReference is the `NodeClaim`, with the slot kept as a secondary, non-controller owner so the
Job is also readable under the slot — and a shutdown with no claim (law 3) is owned by the slot
alone.

```bash
kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<slot>
kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/nodeclaim=<claim>
kubectl get jobs -n homelab-autoscaler-system -l type=startup
```

`monitorJobCompletion` is the goroutine that fires `JobCompleted` / `JobFailed` / `JobTimeout` for
the operation that started it, and it is the writer of the claim's `booting → ready` and
`booting → failed` moves. It is the only writer of a transitional state's exit, which is why a
transition refused by the lock may not write one.

## Integration points

| Component | Reads | Writes |
| --- | --- | --- |
| gRPC provider (`internal/grpcserver`) | the claims of a group, the slots of a group | claims: create, record, cancel. **Nothing in a slot spec, ever.** |
| Slot reconciler (`internal/controller/infra`) | the live claim, `Node.status.powerState`, the `corev1.Node`, the lifecycle Jobs | lifecycle Jobs, cordons, the lock, the report of every pass (compare-and-set on the observation), the claim moves of a transition |
| Claim controller (`internal/controller/infra`) | the claim, its slot, the clock | `NodeClaim.status`, `Node.status.lastClaim`, the finalizer, collection |
| Observation reconciler (`internal/controller/core`) | the `corev1.Node`, the live claim | `Node.status.powerState`, `Node.status.phase`, the autoscaler conditions |
| Cluster-autoscaler | the gRPC provider | the gRPC provider |

## Related Documentation

- [The NodeClaim design](nodeclaim.md) — why this model, and what it replaced
- [Architecture overview](../architecture/overview.md) — the components
- [Node CRD](../api-reference/crds/node.md), [NodeClaim CRD](../api-reference/crds/nodeclaim.md),
  [Group CRD](../api-reference/crds/group.md)
- [Common issues](../troubleshooting/common-issues.md) — a claim stuck pending, a claim failed
