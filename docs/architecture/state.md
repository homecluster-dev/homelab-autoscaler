# State Management

Homelab Autoscaler uses a finite state machine (FSM) for reliable node power state management.

## FSM Overview

The FSM manages node power transitions using the `looplab/fsm` library with coordination locks to prevent race conditions.

```
Shutdown ←→ StartingUp ←→ Ready ←→ ShuttingDown
                      \\              /
                       \\            /
                        JobFailed
```

## States

| State | Description |
|-------|-------------|
| **Shutdown** | Node is powered off, ready for startup |
| **StartingUp** | Node startup operation in progress |
| **Ready** | Node is powered on and operational. The scale-up transition that lands here (`JobCompleted` → Ready) additionally requires the node to be Ready and free of the pool's readiness taints - see [Ready means the machine can take the pool's work](#ready-means-the-machine-can-take-the-pools-work) |
| **ShuttingDown** | Node shutdown operation in progress |

## State Transitions

### Startup Flow
1. **StartNode** event → Shutdown → StartingUp
2. Startup job creates Kubernetes Job
3. **JobCompleted** → StartingUp → Ready
4. **JobFailed** → StartingUp → Shutdown (retry)

### Shutdown Flow
1. **ShutdownNode** event → Ready → ShuttingDown
2. Shutdown job creates Kubernetes Job
3. **JobCompleted** → ShuttingDown → Shutdown
4. **JobFailed** → ShuttingDown → Ready (retry)

## Coordination Locks

The FSM uses Kubernetes annotations for coordination:

```yaml
annotations:
  homelab-autoscaler.dev/operation-lock: "scale-up"
  homelab-autoscaler.dev/lock-owner: "node-controller"
  homelab-autoscaler.dev/lock-timestamp: "2024-01-01T00:00:00Z"
  homelab-autoscaler.dev/lock-timeout: "5m"
```

### Lock Lifecycle
- Acquired in `before_` hooks before state transitions
- Released in `after_` hooks after successful transitions
- Prevents race conditions between controllers
- 5-minute timeout for stuck operations

### What a refused transition leaves behind

A transition that loses the lock is a deferral, not a failure of the slot, and the rules below
are what keep it from stranding one:

- The acquisition happens in the `before_` hook, before `looplab/fsm` applies the state change
  and before the `enter_` hooks, the writers of `status.progress` inside the FSM. A refused
  transition writes nothing: the slot keeps the stable progress it had (`ready` for a
  scale-down that lost to a scale-up, `shutdown` the other way round).
- The lifecycle entry points (`StartNode`, `ShutdownNode`) trigger the event, and so take the
  lock, before touching anything: cordoning the Kubernetes node, clearing the
  `ToBeDeletedByClusterAutoscaler` taint and deleting the pending lifecycle Job all happen
  only on a slot the operation owns. A refused operation takes nothing apart on a slot it
  doesn't own.
- The refusal is reported as a `LockConflictError` carrying the remaining lifetime of the lock.
  The node controller requeues for that deadline and records a `ScaleUpDeferred` /
  `ScaleDownDeferred` event, a Normal one: a collision is the lock doing its job, not an
  incident. The intent of the refused operation is `spec.desiredPowerState` - written by
  cluster-autoscaler's `DeleteNodes` / `DecreaseTargetSize` - so the transition runs on the next
  pass without a second request from the autoscaler.
- Every transition that ends an operation releases the lock, including `JobTimeout`: a lock
  must not outlive the operation that wrote it, or the next operation on the slot is refused
  by a lock nothing is running under.

### Why a transitional progress has to come from a transition

`status.progress: startingup`/`shuttingdown` says a power operation is in flight, and that isn't
cosmetic: the provider leaves those slots out of the target size (`countsTowardsTargetSize`),
and `validateStateTransition` in the node controller gates every transition of a transitional
slot out, so no lifecycle operation starts on one.

Nothing in a reconcile fixes a transitional slot that nothing is driving, either:

- `applyObservedPowerState` (core node controller) never overwrites a transitional progress;
- the FSM can't fire `ShutdownNode`/`StartNode` from `startingup`/`shuttingdown`;
- the stuck-transition cleanup (`ForceCleanup` at `StuckTransitionThreshold`, 15m) reads the
  backoff record of the state machine that ran the transition. The reconciler builds a fresh
  machine on every pass, so `IsTransitionStuck()` is false there and `ForceCleanup` is never
  reached from a reconcile. Past 15m `validateStateTransition` force-releases the coordination
  lock and lets the pass run - it doesn't move the progress.

The writer that moves a transitional progress is the operation that wrote it: the lifecycle Job
monitor of the machine that started it fires `JobCompleted`, `JobFailed` or `JobTimeout`. That's
the whole recovery story, and it's why the rules above matter:

- a transition refused by the lock must not write a progress it will never drive;
- a writer outside the FSM must not write one at all. `NodeGroupDecreaseTargetSize` cancels a
  starting slot with a `spec.desiredPowerState: off` write and leaves `status.progress` to the
  FSM, so the shutdown runs through `ShutdownNode` with its Job, its lock and its timestamps.
  It used to write `status.progress: shuttingdown` directly - no transition, no Job, no lock -
  and the slot was only freed by the monitor of the startup it collided with, a goroutine a
  manager restart takes away.

`updateNodeProgress` guards the writes that go through the FSM: it persists only the progress of
the state the machine is in. `status.progress` has writers outside the FSM (the gRPC server, the
core node controller, the node slot controller provisioning a slot), and none of them runs under
that check.

### The authority order for status.progress

`status.progress` has several writers, and they are ordered. From most to least authoritative:

1. **The state machine owns a transition.** The FSM writes the progress of the state it moves
   into, and only the operation that wrote a transitional progress moves it again
   (`updateNodeProgress` refuses the progress of any state the machine is not in). A slot with a
   lifecycle Job in flight, a live coordination lock, or a Kubernetes node behind it is owned by
   its operation, and nothing else writes it.
2. **The progress watchdog reconciles a slot that nothing is driving** (`internal/progress`, run
   by the node controller at the top of every pass, before any FSM branch). One gate, three
   outcomes: the slot has no `corev1.Node`, no startup/shutdown Job in flight, no such Job finished
   in the last `SettleGracePeriod` (~7 minutes, see below), carries no live coordination lock, and
   its progress implies a machine (`ready`, `startingup`, `shuttingdown`). Then
   - `spec.desiredPowerState: off` -> **settle** to the stable `off`/`shutdown` pair, with a
     `ProgressReconciled` Warning naming the progress it replaced;
   - `spec.desiredPowerState: on` (a claim) -> **re-arm**: the same pair, and the claim left alone,
     which is the shape `StartNode` is legal from, so the pool retries the machine. A `ClaimRearmed`
     Normal event names the attempt the arming leads to. The retries are bounded by the
     **startup-attempt counter** the FSM owns (`fsm.StartupAttemptLimit`, 3): it counts startup
     attempts, not watchdog passes, and past the bound the re-arming stops and the slot is **settled**
     to the stable pair with one `ClaimRearmLimitReached` Warning, so a slot whose machine will not
     boot is never left at `ready` with nothing behind it;
   - the slot has a node object, is at `ready`, is claimed on, is cordoned, and carries no
     cluster-autoscaler marker -> **uncordon** it, with a `CordonReconciled` Warning. That is the
     residue of the paths that land in `ready` without the uncordon hook (`JobTimeout`, `JobFailed`).
3. **Observation updates the stable values.** `core.KubernetesNodeReconciler.applyObservedPowerState`
   corrects `powerState`/`progress` to what the node object reports, and
   `core.NodeSlotReconciler` seeds a slot that never had a machine. Neither writes a transitional
   value, and neither runs on a slot that has no node object at all - which is exactly the case
   rule 2 exists for.

The order is the design. Rule 1 is what can strand a slot; rule 2 exists because of rule 1, and
because rules 3 cannot reach a slot with no node object; rule 3 is the writer that was never
allowed to fix the transitional values, and still is not.

The two gates that are not obvious:

- **The claim.** A slot with `spec.desiredPowerState: on` is never settled *out of the claim*: the
  claim is the request, and dropping it, or telling the provider its requested capacity does not
  exist, is the autoscaler's arithmetic (`NodeGroupDecreaseTargetSize`) and not this controller's
  answer to a scale-up. A claim whose machine never booted is not left pointing at one either: it is
  re-armed (progress and power state made true, `spec.desiredPowerState` untouched), because the
  alternative is a slot that `StartNode` cannot leave, that `NodeGroupTargetSize` counts forever, that
  `NodeGroupDecreaseTargetSize` cannot cancel, and that no other writer in the system heals. Past the
  startup-attempt bound the same pair is written as a **settlement** - the progress is made true and
  the claim is still left on - because the bound is the point where retrying stops and an operator has
  to say why. What the settlement changes is the progress, which is what every other writer reads: from
  `shutdown` the FSM can start the machine again the moment an operator fixes the template, and a
  scale-down lands on a slot that is already off instead of building a shutdown Job for a server that is
  gone. What it does not change is the arithmetic - a claimed slot counts toward the target size whatever
  its progress says, because the request is what the autoscaler asked for, and dropping it stays
  `NodeGroupDecreaseTargetSize` / `NodeGroupDeleteNodes`, never the watchdog's. The
  `ClaimRearmLimitReached` Warning is what tells an operator the request is standing and will not be
  filled.
- **The startup-attempt bound.** A re-arm is a machine the pool is about to pay for, so the retries
  are bounded - by one counter, shared by both failure loops, owned by the FSM
  (`internal/fsm/startup_attempts.go`):
  - it counts **startup attempts**, incremented where an attempt begins: the `StartNode` transition
    (`enterStartingUp`, `shutdown -> startingup`), which is where the lifecycle Job and the power-on
    hang off, and the one place the watchdog's re-arm and a `JobFailed` rollback both lead into;
  - it is cleared where an attempt **demonstrably succeeded**: the readiness gate of
    `afterJobCompleted`, the same place that decides the slot may be recorded `ready`. A completed
    startup Job is not the proof - a machine that boots, registers and never clears its CNI bootstrap
    taint has spent its attempts and keeps them counted - and a slot is reused across claim episodes,
    so a count that is never cleared is a lifetime bound, not a bound on the failure;
  - it is enforced on **both** paths: the watchdog refuses to re-arm past the limit, and
    `fsm.NodeStateMachine.StartNode` refuses to start (the loud loop - a startup Job that exits
    non-zero - leaves the slot at the stable `off`/`shutdown` pair, which implies no machine, so the
    watchdog never sees it at all and the FSM is the only writer in front of it). Past the bound the
    slot is settled to the stable pair, its claim is kept, and exactly one `ClaimRearmLimitReached`
    Warning is recorded.
- **The grace.** `SettleGracePeriod` (~7 minutes) is how long a *finished* lifecycle Job keeps its
  slot out of reach. The Job is the operation and the state machine that started it is the writer
  that settles the progress it moved, and the hook that settles it is not instantaneous: it runs
  `setNodeSchedulableWithRetry`, which can spend one monitor poll (`fsm.JobMonitorPollInterval`,
  30s) noticing the Job, the whole registration budget (`fsm.DefaultUncordonTimeout`, 90s) waiting
  for the node object, the whole readiness gate (`fsm.DefaultReadinessGateTimeout`, 4m) waiting for
  it to become usable, and one sleep (`fsm.MaxUncordonRetryInterval`, 10s) past its own deadline.
  The grace is that sum plus a minute, and a test computes the same expression, so growing any of
  those four constants without growing the grace fails. Stomping the progress inside that window
  writes over a transition its owner is about to record, and the FSM reads its state from the
  progress, so the write is how a duplicate lifecycle Job is born. The grace is *not* measured
  against the 5m Job timeout: it applies only to a finished Job, and a Job that is pending or
  running is never acted on at all, at any length. The bound it does respect is the 15m
  `StuckTransitionThreshold`: a slot whose monitor died with its manager - the shape the watchdog
  exists for - is healed well inside the horizon an operator calls stuck. With no Job object left at
  all - the TTL collected it, which a short `spec.ttlSecondsAfterFinished` makes routine - the same
  window is measured from `status.lastStartupTime`, the stamp the FSM's own progress write leaves on
  the transition into `startingup`: the absence of a Job is not evidence that the operation is over,
  and a grace that only exists while the Job object does would open inside the hook that owns the
  progress.

### Ready means the machine can take the pool's work

The **scale-up transition** into `ready` - `JobCompleted` → Ready, the one that writes
`status.progress: ready` - completes when the node object is `Ready` **and** free of every readiness
taint the pool declares. `fsm.setNodeSchedulableWithRetry` waits for both, and the transition's
`after_` hook runs after it.

The gate is the startup path's only: a slot can also reach `ready` through `JobFailed` and
`JobTimeout`, which are rollbacks of a shutdown, and through the observation controller's correction
of a machine that is Ready. Those paths do not run the gate. The cordon safety net in
`internal/progress` is what covers the residue they leave (a ready, claimed slot whose node is
cordoned with no operation on it).

The kubelet marks a node Ready minutes before the CNI has programmed its datapath, and the pool's
workloads do not tolerate the bootstrap taint, so **a pool's workloads do not need to tolerate
`node.cilium.io/agent-not-ready`**: the operator declares the taint nowhere, the controller waits it
out. Declaring the slot ready while the taint is on the node tells the autoscaler the slot is
capacity, the pending pod still cannot be placed on it, and the pool buys a second machine for a node
that is about to work - the churn this gate closes.

Which key to wait for belongs to the CNI, so it is configuration, not a constant:
`Group.spec.nodeReadinessTaints` is the exact list the gate waits for, and the CRD defaults it to
`{key: node.cilium.io/agent-not-ready, effect: NoExecute}` (Cilium has stamped that key since v1.9;
the pool this chart deploys runs Cilium 1.16.x, Stable channel). A pool on a CNI that stamps no
bootstrap taint declares an empty list, and the gate is the `Ready` condition alone. The keys a slot
is gated on are recorded on it as the `NodeReadinessTaints` condition, so a slot that has been
booting for ten minutes says what it is waiting for.

The two halves of the wait have their own deadlines, because a cold boot spends nearly all of its
time in the first: registration (the node object appearing) is bounded by 90s
(`fsm.DefaultUncordonTimeout`), and the readiness gate by 4m (`fsm.DefaultReadinessGateTimeout`) -
started when the node object first appears, not when the loop starts. One shared budget is the bug
this split closes: a machine that registers in 80s has 10s left for a gate that needs minutes, so the
gate times out on the boot it exists to protect, the slot is declared ready with the taint on it, and
the churn survives with a 90s hook and an ERROR line per scale-up.

A timeout does not wedge anything. The loop uncordons the node, records a **`ReadinessGateTimeout`
Warning** on the slot naming the half that ran out and the taint it was waiting for, logs the same,
and the transition completes as it always has: a readiness gate nothing can answer for must not block
teardown, must not leave the slot transitional, and must not leave the machine cordoned, because
cluster-autoscaler skips cordoned nodes when it picks scale-down candidates - a cordoned node is
never drained, never deleted, and keeps the pool paying for it. Because the gate can outlast the
coordination lock's 5m timeout, the loop re-stamps the lock on every pass (`RenewLock`): a lock that
ages out mid-gate protects nothing, and the watchdog, the next acquisition and the expired-lock
cleanup all conclude from it that the operation is over.

### What the autoscaler decided, recorded on the slot

cluster-autoscaler marks the nodes it is about to remove with its own taints:
`DeletionCandidateOfClusterAutoscaler` (soft, a candidate under evaluation) then
`ToBeDeletedByClusterAutoscaler` (hard, a delete requested and the node draining). The core node
controller watches the node objects, so it records each transition of those markers on the Node CR
as a Normal event - `AutoscalerDeletionCandidate` / `AutoscalerDeletionRequested` - and logs the
node's full taint list with `spec.unschedulable`, which is what makes a cordon attributable without
raising the autoscaler's verbosity. The last marker observed is kept on the slot as the
`ClusterAutoscalerMarkedForDeletion` condition, and that record is the deduplication: a pass that
sees the marker it already recorded writes nothing and records nothing, an unmarking is a status
update with no event, and a reappearance is a new event. The controller only reports the markers, it
never adds, edits, or removes one, and it never writes `spec.desiredPowerState`: the scale-down
request is the gRPC provider's to make.

Read the observation from the condition's **message**, not from its `lastTransitionTime`:
`meta.SetStatusCondition` only takes the supplied timestamp when the `Status` flips, and both
deletion markers are recorded with `Status: True`. `True/DeletionCandidate` becoming
`True/DeletionRequested` therefore keeps the *candidate's* time on a condition whose reason is now
the delete request, and the interval between the two markers - minutes, in a real drain - is missing
from the one field that reads like a timestamp. The message carries the instant the pass read the
node (`observed at <RFC3339>`), and that is the record that is true.

## Async Operations

Power operations use Kubernetes Jobs for async execution:

### Startup Job
```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: node-startup-{node-name}
spec:
  template:
    spec:
      containers:
      - name: power-manager
        image: homelab/power-manager:latest
        command: ["wake-on-lan"]
        args: ["00:11:22:33:44:55"]
```

### Shutdown Job
```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: node-shutdown-{node-name}
spec:
  template:
    spec:
      containers:
      - name: power-manager
        image: homelab/power-manager:latest
        command: ["ssh-shutdown"]
        args: ["worker-01.local"]
```

## Error Handling

The FSM provides robust error recovery:

### Job Failures
- Failed startup jobs → Shutdown state (ready for retry)
- Failed shutdown jobs → Ready state (ready for retry)
- Automatic retry with backoff strategy

### Timeout Handling
- 5-minute timeout for job completion
- State-aware backoff (30s → 2m → 5m)
- A transition older than 15m gets its coordination lock force-released, and the next pass runs
  (see [Why a transitional progress has to come from a transition](#why-a-transitional-progress-has-to-come-from-a-transition))

### Coordination Conflicts
- FSM prevents invalid concurrent transitions
- A lost lock is a deferral: a `ScaleUpDeferred`/`ScaleDownDeferred` event and a requeue at the
  lock's remaining lifetime
- Clear error reporting and retry logic
- Automatic lock cleanup

### Slots With No Machine
- The progress watchdog settles a slot whose `status.progress` names a machine the cluster does not
  have, with a `ProgressReconciled` Warning naming the progress it replaced (see
  [The authority order for status.progress](#the-authority-order-for-statusprogress))
- A **claim** (a slot with `spec.desiredPowerState: on`) whose machine never booted is re-armed, not
  settled: the progress and the power state are made true, the claim stays, and the pool retries the
  machine (`ClaimRearmed`, naming the attempt)
- The retries are bounded by **one startup-attempt counter** (`fsm.StartupAttemptLimit`, 3) that both
  failure loops pay into, because they spend the same thing - a machine:
  - it is incremented where an attempt begins, the `StartNode` transition
    (`enterStartingUp` -> `shutdown -> startingup`), which is what the lifecycle Job and the power-on
    hang off, and what both the watchdog's re-arm and a `JobFailed` rollback lead back into;
  - it is cleared where an attempt demonstrably succeeded: the **readiness gate** of
    `afterJobCompleted`, the place that decides the slot may be recorded `ready`. A completed startup
    Job is not the proof - a machine that boots, registers and never clears its CNI bootstrap taint
    has spent its attempts and keeps them counted - and slots are reused across claim episodes, so a
    count that never resets is a lifetime sentence;
  - it is enforced on both paths: the watchdog refuses to re-arm, and `StartNode` refuses to start, so
    the loud loop (a startup Job that exits non-zero, which lands at the stable `off`/`shutdown` pair
    the watchdog never looks at) is bounded too. Past the bound the slot is settled to that pair, the
    claim is kept, and exactly one `ClaimRearmLimitReached` Warning is recorded, so a claim that cannot
    boot buys a bounded number of machines and an operator sees it
- A ready, claimed slot whose node is cordoned with no Job, no lock and no autoscaler marker on it is
  uncordoned (`CordonReconciled`): the paths that roll back into `ready` never call the uncordon hook,
  and a cordoned node is invisible to scale-down, so it is never removed and never stops costing
- A lifecycle Job in flight, a Job that finished inside the settle grace - measured from
  `status.lastStartupTime` when no Job object is left, so a short `ttlSecondsAfterFinished` cannot open
  the gate inside a running hook - a live coordination lock, or a node object (whose status belongs to
  the observation controllers) keeps the watchdog away from the slot: the operation owns it

## Integration Points

### Controller Integration
```go
// NodeReconciler uses FSM for state management
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) {
    fsm := fsm.NewNodeStateMachine(node, r.Client)

    if node.Spec.DesiredPowerState == "on" {
        fsm.StartNode()
    } else {
        fsm.ShutdownNode()
    }
}
```

### gRPC Server Integration
```go
// Cluster Autoscaler triggers FSM events
func (s *Server) NodeGroupIncreaseSize(ctx context.Context, req *pb.Request) {
    fsm := fsm.NewNodeStateMachine(node, s.Client)
    fsm.StartNode()
}
```

## Benefits

### Reliability
- Formal state transitions prevent invalid operations
- Coordination locks prevent race conditions
- Automatic error recovery and retry

### Observability
- Clear current state and valid transitions
- State transition logging and metrics
- Easy debugging of stuck operations

### Flexibility
- Extensible FSM architecture
- Customizable timeout and backoff
- Support for various power management methods

## Configuration

### Timeout Settings
- Job timeout: 5 minutes (configurable)
- Lock timeout: 5 minutes (configurable)
- Backoff strategy: 30s → 2m → 5m

### Power Methods
Supported power control methods:
- Wake-on-LAN
- IPMI/BMC interfaces
- Smart PDUs
- Custom scripts via SSH
- Hardware-specific protocols

## Related Documentation
- [Architecture Overview](overview.md) - System design
- [Node CRD](../api-reference/crds/node.md) - Node resource definition
- [Development Setup](../development/setup.md) - FSM implementation details