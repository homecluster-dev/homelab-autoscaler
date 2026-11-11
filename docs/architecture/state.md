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
| **Ready** | Node is powered on and operational |
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