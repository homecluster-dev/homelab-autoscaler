# Common Issues & Solutions

Quick reference for resolving frequently encountered problems with homelab-autoscaler.

## 🔍 Quick Diagnostics

### Controller Not Starting
```bash
# Check if CRDs are installed
kubectl get crds | grep homecluster

# Check pod status
kubectl get pods -n homelab-autoscaler-system

# Check logs for errors
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager
```

**Common Fixes:**
- Run `make install` to install CRDs
- Ensure you're using the correct namespace: `homelab-autoscaler-system`

### gRPC Connection Issues
```bash
# Check if gRPC service is running
kubectl get svc -n homelab-autoscaler-system | grep grpc

# Test gRPC connectivity
kubectl run grpc-test --rm -it --image=nicolaka/netshoot -- \
  grpc_cli call homelab-autoscaler-grpc:50051 NodeGroups ""
```

**Common Fixes:**
- Verify gRPC service is enabled in Helm values
- Check network policies if using in production

## 🛠️ Common Operations

### Manual Node Power Control

There is no field to declare. A request is an object, and a cancellation is the delete of one:

```bash
# Ask for a machine on a free slot (a slot with no live claim)
kubectl apply -f - <<'YAML'
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
YAML

# Watch it: pending -> booting -> ready, inside spec.deadlineAt
kubectl get nodeclaim -n homelab-autoscaler-system -w

# Power the machine off: cancel the request that owns it
kubectl delete nodeclaim claim-worker-01 -n homelab-autoscaler-system

# The slot's side of the same story
kubectl describe nodes.infra.homecluster.dev worker-01 -n homelab-autoscaler-system
```

A `booting`/`ready` claim carries the controller's finalizer, so deleting it is the shutdown request:
cordon, shutdown Job, power off, and the claim ends `completed` when the machine is observed off. A
`pending` claim is deleted outright.

### Stuck Job Cleanup
```bash
# List all jobs in the namespace
kubectl get jobs -n homelab-autoscaler-system

# Delete failed jobs
kubectl delete jobs -n homelab-autoscaler-system --field-selector=status.successful=0

# Clean up completed jobs
kubectl delete jobs -n homelab-autoscaler-system --field-selector=status.successful=1
```

### Resource Inspection
```bash
# Get detailed resource status
kubectl describe group <group-name> -n homelab-autoscaler-system
kubectl describe node.infra.homecluster.dev <node-name> -n homelab-autoscaler-system

# Check resource conditions
kubectl get group <group-name> -n homelab-autoscaler-system -o jsonpath='{.status.conditions}'
kubectl get node.infra.homecluster.dev <node-name> -n homelab-autoscaler-system -o jsonpath='{.status.conditions}'
```

## ⚡ Performance Issues

### High Resource Usage
```bash
# Check controller resource usage
kubectl top pods -n homelab-autoscaler-system

# Monitor API server calls
kubectl get --raw /metrics | grep apiserver_request_total
```

**Tuning Options:**
- Adjust reconciliation intervals in Helm values
- Increase resource limits for controller
- Reduce log verbosity if debug logging is enabled

### Slow Scaling Operations
```bash
# Check operation timing in logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep -i "duration\|timeout"

# Monitor job execution times
kubectl get jobs -n homelab-autoscaler-system -o wide
```

**Optimizations:**
- Tune job timeouts in Node CRD specifications
- Optimize power operation scripts for faster execution
- Consider higher resource limits for power operation jobs

## 🐛 Debugging Specific Scenarios

### Nodes Not Powering On/Off
1. Check job status: `kubectl describe job <job-name>`
2. Verify power operation scripts work manually
3. Check node power state: `kubectl describe node.infra.homecluster.dev <node-name>`

### A Fresh Node Takes No Pods (Cilium Bootstrap Taint)

A node that joins and is `Ready` is not yet able to run the pool's workloads: Cilium adds
`node.cilium.io/agent-not-ready` to a node that has just joined and removes it once its datapath is
programmed, which is minutes after the kubelet checked in. The controller waits for that taint to
clear before it uncordons the node and before the scale-up transition is treated as complete, so
**the pool's workloads do not need to tolerate the taint** - declare it nowhere, and do not add a
toleration to work that is supposed to land on the pool.

The key the gate waits for is the pool's configuration: `Group.spec.nodeReadinessTaints` is the list,
defaulted by the CRD to Cilium's `node.cilium.io/agent-not-ready` (the key has been Cilium's since
v1.9; this chart deploys 1.16.x). A pool on a CNI that stamps no bootstrap taint declares an empty
list, and the gate becomes the `Ready` condition alone. What a slot is gated on is recorded on the
slot, so a slot that has been booting for ten minutes says what it is waiting for:

```bash
kubectl get group <group> -o jsonpath='{.spec.nodeReadinessTaints}' ; echo
kubectl describe node.infra.homecluster.dev <node-name>   # Condition: NodeReadinessTaints
```

The gate has its own 4 minute deadline, counted from the moment the node object registers - the 90s
registration timeout is a separate budget, so a slow boot does not eat the gate. Past the deadline the
controller uncordons the node (a cordoned node is skipped by cluster-autoscaler's scale-down, so it
would never be drained, never deleted, and the pool would keep paying for it), records a
`ReadinessGateTimeout` **Warning** on the slot, and **fails the claim** with that reason: the machine
is powered off, the slot is free, and the next claim on it is attempt N+1. The taint is visible on the
node object:

```bash
kubectl get node <node> -o jsonpath='{.spec.taints}' ; echo
kubectl get node <node> -o jsonpath='{.spec.unschedulable}' ; echo
```

### A Machine Is Up and Nothing Wants It

A machine the pool no longer wants is a slot with **no live claim** and `status.powerState: on`. The
input law shuts it down; the report of that shape is `draining`. Read the triple, not one field:

```bash
# The slot: observation, report, and the digest of the last request
kubectl get nodes.infra.homecluster.dev <node-name> -n homelab-autoscaler-system \
  -o jsonpath='{.status.powerState} {.status.phase} {.status.lastClaim.phase} {.status.lastClaim.reason} {.status.lastClaim.attempt}' ; echo
# The claims of the slot: live ones (pending, booting, ready) own the machine
kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name> -o wide
# The machine, as the kubelet reports it. The corev1.Node is cluster-scoped: no -n.
kubectl get node <kubernetes-node-name> -o jsonpath='{.spec.unschedulable} {.status.conditions[?(@.type=="Ready")].status}' ; echo
kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name>
kubectl describe nodes.infra.homecluster.dev <node-name> -n homelab-autoscaler-system
```

What the answers mean:

- **No live claim, `status.powerState: on`** → the slot reconciler cordons the node and runs the
  shutdown Job. If it does not, check for a coordination lock held by the opposite operation
  (`homelab-autoscaler.dev/operation-lock` on the slot): a scale-down that loses the lock is deferred
  to the lock's deadline and records a Normal `ScaleDownDeferred`, and the intent is the claim, so
  nothing has to be asked for again.
- **A claim in `failed` with the machine up** → that is the force shutdown of an overdue boot, the
  same branch. `status.reason` says which attempt died and why.
- **A claim marked for deletion (`DeletionTimestamp` set, finalizer present) with the machine up** →
  the scale-down is running. The claim ends when the machine is observed off.
- **A claim in `ready`, the node cordoned** → nothing is shutting it down, because the claim wants
  the machine: `kubectl describe node <kubernetes-node-name>` shows who put the cordon there (see
  [Who Cordoned My Node](#who-cordoned-my-node)).

The v1alpha1 watchdog (`internal/progress`) that healed these shapes by rewriting
`status.progress` is gone. A slot is never written back to a stable pair, because it never held the
request: the claim has a deadline, and the deadline is the heal.

### The Shutdown Job Exits 0 and the Machine Stays Up

A shutdown Job is a script: it exits 0 when its own commands ran, which is not evidence that the
provider's power-off landed. The controller reads the machine, so the slot keeps reporting
`draining`, the scale-down claim keeps its finalizer, and the attempts are counted **on that claim**:

```bash
kubectl describe nodeclaim claim-<node-name> -n homelab-autoscaler-system
# Events: ShutdownAttemptLimitReached - the machine is still observed up after the bound (3)
kubectl get nodes.infra.homecluster.dev <node-name> -n homelab-autoscaler-system \
  -o jsonpath='{.status.lastClaim.attempt} {.status.lastClaim.reason} {.status.lastClaim.call}' ; echo
kubectl logs -n homelab-autoscaler-system job/<shutdown-job>
```

Past the bound the slot reconciler **stops firing the shutdown** — no fifth Job for a script that
keeps exiting 0 — and the machine stays up, cordoned, unwanted, and paid for. That is deliberate:
the fix is the script. Make `spec.shutdownPodSpec` call the provider's own power-off and exit
non-zero when the machine is still on; the next pass picks the loop back up, and no hand on a status
field is needed. A healthy scale-down pays no second Job either: the guard reads the kubelet's
heartbeat against the Job's `status.completionTime`, because a node object stays `Ready` for
`node-monitor-grace-period` (~40s) after a real power-off.

### A Claim Is Stuck Pending

`pending` means the provider answered cluster-autoscaler and **no lifecycle Job has been created
yet**. The claim is the request, so the whole question is "why has the slot not started booting it":

```bash
# 1. The claim, and its budget
kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name> -o wide
kubectl describe nodeclaim claim-<node-name> -n homelab-autoscaler-system
# 2. The slot's triple: the live claim, the observed power, the node
kubectl get nodes.infra.homecluster.dev <node-name> -n homelab-autoscaler-system \
  -o jsonpath='{.status.powerState} {.status.phase}' ; echo
kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name> \
  -o jsonpath='{range .items[*]}{.metadata.name} {.status.phase} {.status.reason}{"\n"}{end}'
# 3. Who owns the slot: the coordination lock, and any lifecycle Job in flight
kubectl get nodes.infra.homecluster.dev <node-name> -n homelab-autoscaler-system \
  -o jsonpath='{.metadata.annotations}' ; echo
kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name>
# 4. The slot reconciler's own words
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager \
  -l control-plane=controller-manager --tail=200 | grep <node-name>
```

The causes, in the order to check them:

- **The machine is already observed up** (`status.powerState: on`). Law 1 boots a machine that is
  off; a machine that is up goes through the Job monitor of the operation that started it. A claim
  in `pending` on a slot that is up is the shape a boot whose Job completed before the phase landed
  takes, and the report reads `booting`, not `pending`.
- **The slot reconciler is not running, or the pass is deferred.** A lock held by the opposite
  operation defers the boot to the lock's deadline (`ScaleUpDeferred` on the slot). Expired locks are
  cleaned up on every pass, so an annotation older than its own `lock-timeout` is not a cause.
- **The Group cannot be read.** A slot whose `group` label names no Group CR is requeued every 5s
  and buys nothing; the log says `Group referenced by node label not found`.
- **`spec.startupPodSpec` is broken** (image pull, command, RBAC). The Job exists and fails, the
  claim moves to `failed` with `reason: StartupJobFailed`, and the machine is powered off.
  `kubectl logs -n homelab-autoscaler-system job/<startup-job>` is the answer.
- **The deadline arrives.** `spec.deadlineAt` is the budget of the attempt; when it passes, the claim
  is recorded `failed` with `reason: DeadlineExceeded` and the slot is freed for attempt N+1. A claim
  cannot sit in `pending` past its deadline.

```bash
# The claim is overdue and the controller is healthy, and you want the machine now:
kubectl delete nodeclaim claim-<node-name> -n homelab-autoscaler-system   # cancel: nothing is in flight
kubectl scale deployment/homelab-autoscaler-cluster-autoscaler -n homelab-autoscaler-system --replicas=1
```

### A Claim Failed

`failed` is terminal and it is not capacity: the target size dropped when it was recorded, the
machine was force-shut-down, and the next claim on the slot is attempt N+1. Read the reason before
you touch anything:

```bash
kubectl get nodeclaim -n homelab-autoscaler-system --sort-by=.metadata.creationTimestamp | tail
kubectl describe nodeclaim claim-<node-name> -n homelab-autoscaler-system
kubectl get nodes.infra.homecluster.dev <node-name> -n homelab-autoscaler-system \
  -o jsonpath='{.status.lastClaim}' ; echo
kubectl get events -n homelab-autoscaler-system --field-selector involvedObject.name=claim-<node-name> \
  --sort-by=.lastTimestamp | tail
kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name>
```

| `status.reason` | What happened | What to do |
| --- | --- | --- |
| `DeadlineExceeded` | the boot outlived `spec.deadlineAt` (the operator budget, floored by the group's `spec.maxNodeProvisionTime`) | read the startup Job's logs and the machine's power API credentials; a pool whose boots are legitimately slow raises `spec.maxNodeProvisionTime` on the Group, which raises the claim's floor |
| `ReadinessGateTimeout` | the machine booted and never cleared the gate — not `Ready`, or a `spec.nodeReadinessTaints` key still on it | `kubectl get node <kubernetes-node-name> -o jsonpath='{.spec.taints}' ; echo`, check the CNI, and see [A Fresh Node Takes No Pods](#a-fresh-node-takes-no-pods-cilium-bootstrap-taint) |
| `StartupJobFailed` / `StartupJobTimeout` | the startup script exited non-zero, ran out of time, or could not be created | `kubectl logs -n homelab-autoscaler-system job/<startup-job>`; the image, the command, the credentials, the RBAC of its ServiceAccount |
| `AttemptLimitExceeded` | the slot spent its consecutive attempts (3), so no machine is bought for it | fix the cause of the streak, then create the next claim (an episode that lands `ready` or `completed` restarts the streak at 1); the record is on the claim and in `status.lastClaim` |
| `Superseded` | two live claims named one machine and this one is the older | see [One live claim per slot is rejected](#one-live-claim-per-slot-is-rejected) |
| `SlotDeleted` | the `Node` CR went away under the claim | nothing to fix: the slot is gone, and the claims of a slot are owned by it |
| `MachineGone` | the node object of a ready claim disappeared while the machine was observed off | the machine was deleted out of band; the slot becomes free and the next request claims it |

A failed claim is kept for `spec.ttlSecondsAfterFinished` (default 3600s) after `status.finishedAt`,
so `kubectl get nodeclaim -a` in the pool's namespace shows the episodes while they are readable, and
`Node.status.lastClaim` keeps the digest after collection.

### One Live Claim Per Slot Is Rejected

The refusal is the invariant working, and it is not a bug to work around: a slot answers for one
request at a time, which is what makes "a live claim exists" mean "the machine is wanted", keeps
`TargetSize` a count of claims, and keeps the model cancellable.

```bash
# Who owns the slot?
kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name> -o wide
# The refusal, verbatim, from the admission webhook
kubectl apply -f my-claim.yaml 2>&1 | grep -i "one live claim per slot"
```

The message names the slot and the claim that owns it:

```
error: metadata: Invalid value: "worker-01": slot "worker-01" already has the live NodeClaim
"worker-01-7f4b9c" in phase "booting": one live claim per slot
```

Decide what you meant, and do the matching thing:

```bash
# You wanted a second machine: claim a slot that has no live claim, not this one
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -l group=<group> -o wide

# You wanted this slot's request replaced: cancel the live claim, then create the new one.
# A pending claim is deleted outright; a booting/ready claim runs the shutdown path through
# its finalizer and ends when the machine is observed off.
kubectl delete nodeclaim <claim-name> -n homelab-autoscaler-system
kubectl get nodeclaim <claim-name> -n homelab-autoscaler-system   # gone, or draining with a DeletionTimestamp

# The claim you are creating is a terminal status write, not a second request: it is admitted.
kubectl get nodeclaim -n homelab-autoscaler-system -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,DELETED:.metadata.deletionTimestamp
```

Two things that are **not** rejections: a `NodeClaim` whose phase is `failed`/`completed` (a
terminal claim frees its slot), and a delete of any claim. If two live claims are ever found on one
slot — a webhook that was down, two providers racing, a claim applied by hand — the claim controller
fails the older one with `reason: Superseded`; the newest live claim is the one the slot answers for.

### Who Cordoned My Node

cluster-autoscaler marks the nodes it is about to remove with its own taints, and the operator side
records them on the Node CR, once per transition, so a cordon is attributable without raising the
autoscaler's verbosity:

```bash
kubectl describe node.infra.homecluster.dev <node-name>
# Events:  AutoscalerDeletionCandidate  (DeletionCandidateOfClusterAutoscaler, the soft marker)
#          AutoscalerDeletionRequested  (ToBeDeletedByClusterAutoscaler, draining for deletion)
# Condition: ClusterAutoscalerMarkedForDeletion, with the node's taints and spec.unschedulable
```

The controller reports the markers; it never adds, edits, or removes one, and it never writes
a slot's spec - the scale-down request itself is the gRPC provider's to make, and it makes it by
cancelling the `NodeClaim` that owns the machine.

**Date the decision from the condition's message, not from its `lastTransitionTime`.** A condition
timestamp only moves when the `Status` flips, and both markers are recorded with `Status: True`, so
the `DeletionCandidate` → `DeletionRequested` transition - the one an operator asks about - keeps the
candidate's time. The message carries the instant this controller read the node:

```
Type: ClusterAutoscalerMarkedForDeletion   Status: True   Reason: AutoscalerDeletionRequested
Message: node worker-1 carries the cluster-autoscaler marker ToBeDeletedByClusterAutoscaler
         (taints=[...], spec.unschedulable=true, observed at 2026-11-11T14:03:00Z)
```

### Autoscaling Not Triggering
1. Verify resource utilization metrics
2. Check group scaling thresholds
3. Ensure gRPC server is responding to Cluster Autoscaler

### Resource State Mismatch
1. Run manual sync: Annotate resource with `homelab-autoscaler.dev/force-reconcile`
2. Check Core controller logs for sync issues
3. Verify Kubernetes node exists and is healthy

## 📋 Emergency Procedures

### Complete System Reset
```bash
# Delete all custom resources (CAUTION: Destructive!)
kubectl delete groups --all -n homelab-autoscaler-system
kubectl delete nodes.infra.homecluster.dev --all -n homelab-autoscaler-system

# Restart controller
kubectl rollout restart deployment/homelab-autoscaler-controller-manager -n homelab-autoscaler-system
```

### Database Corruption Recovery
```bash
# Export current state for backup
kubectl get groups -n homelab-autoscaler-system -o yaml > groups-backup.yaml
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -o yaml > nodes-backup.yaml

# Restore from backup
kubectl apply -f groups-backup.yaml
kubectl apply -f nodes-backup.yaml
```

---

For more detailed debugging, see the [Debugging Guide](debugging-guide.md).
Report persistent issues on [GitHub Issues](https://github.com/homecluster-dev/homelab-autoscaler/issues).