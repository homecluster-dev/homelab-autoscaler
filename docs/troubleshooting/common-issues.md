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
`draining`, the scale-down claim keeps its finalizer, and the attempts are counted **on that claim**,
one per shutdown Job — the transition that fires the shutdown writes the count, and the claim
controller's 15s poll of the observation only reads it:

```bash
kubectl describe nodeclaim claim-<node-name> -n homelab-autoscaler-system
# Events: ShutdownAttemptLimitReached - the machine is still observed up after the bound (3)
kubectl get nodes.infra.homecluster.dev <node-name> -n homelab-autoscaler-system \
  -o jsonpath='{.status.lastClaim.shutdownAttempts} {.status.lastClaim.reason} {.status.lastClaim.call}' ; echo
kubectl logs -n homelab-autoscaler-system job/<shutdown-job>
```

The attempts are `status.shutdownAttempts`, a counter of its own: `status.attempt` is the record of
the machines the request bought, and a shutdown that never lands must not spend it.

Past the bound the slot reconciler **stops firing the shutdown** — no fourth Job for a script that
has had its three — and the machine stays up, cordoned, unwanted, and paid for. That is deliberate:
the fix is the script. Make `spec.shutdownPodSpec` call the provider's own power-off and exit
non-zero when the machine is still on.

The pause is bounded, not a sentence: past `claim.ShutdownAttemptResetGrace` (30 minutes from
`status.lastShutdownAttemptAt`) the streak is spent no longer, the count restarts, and the shutdown
fires again — so a fixed script is picked up by a pass within 30 minutes, with no hand on a status
field. Waiting 30 minutes is not the only option: the count lives on the request, and the claim is
the object that reports it, so the escape hatch is one command, and the next pass of the slot
reconciler fires.

```bash
kubectl patch nodeclaim claim-<node-name> -n homelab-autoscaler-system --subresource=status --type=merge \
  -p '{"status":{"shutdownAttempts":0,"lastShutdownAttemptAt":null}}'
``` A healthy scale-down pays no second Job either: the guard reads the kubelet's
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
- **The deadline arrives.** `spec.deadlineAt` is the budget of the **boot** (12m10s: the startup
  Job's own timeout, the monitor poll, the registration wait, the readiness gate and a margin —
  floored by the group's `spec.maxNodeProvisionTime`). When it passes with the claim at `pending`
  or `booting`, the claim is recorded `failed` with `reason: DeadlineExceeded` and the slot is
  freed for attempt N+1. A `ready` claim is past the boot window, and the `booting → ready` write
  clears `spec.deadlineAt`: a healthy machine is never destroyed by a budget it already spent. A
  claim that reaches `ready` with a deadline in its past (an old object, a hand write) is left
  alone, and the deadline is cleared by the model, not by a timer.

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
kubectl get events -n homelab-autoscaler-system \
  --field-selector involvedObject.kind=NodeClaim,involvedObject.name=claim-<node-name> \
  --sort-by=.lastTimestamp
kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name>
```

`kubectl describe nodeclaim` is where the attempt's timeline is, because the claim is the attempt and
every Event about it is published on it (`internal/claim/events.go`). Read the stream oldest first
and the answer to "why did this attempt fail" is one line long:

```bash
# The whole life of one attempt: ClaimCreated -> ClaimBooting -> ClaimFailed
kubectl describe nodeclaim claim-<node-name> -n homelab-autoscaler-system

# Every claim event in the pool, so a slot's bad episode is visible next to the ones that landed
kubectl get events -n homelab-autoscaler-system \
  --field-selector involvedObject.kind=NodeClaim --sort-by=.lastTimestamp | tail -20

# The same stream for one slot: the claims of a slot share its label
kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name>
```

- `ClaimCreated` names the RPC that asked, its `delta` / `nodes`, the group, the caller, and the
  deadline the attempt got.
- `ClaimBooting` is the commit: the startup Job of the attempt, and the fact the boot is no longer
  cancelable.
- `ClaimFailed` is a Warning that quotes `status.reason` **and the number behind it** — the deadline
  that passed, the Job that died, the attempts spent — so the reason table below is a lookup you
  already have.
- `ClaimCancelled` names the call that asked for the machine back; `ClaimCompleted` says the machine
  is observed off and how long the episode lasted; `ClaimSuperseded` names the claim that won the
  slot.
- The slot's own events (`ScaleUpRequested`, `ScaleUpCompleted`, …) stay on the
  `Node` and say what the machine did. They are the same episode from the other object, not a second
  copy of the sentence.

The reason, and what to do with it:

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
so `kubectl get nodeclaim -a` in the pool's namespace shows the episodes while they are readable. The
Events of the attempt expire with the cluster's Event TTL (one hour by default), and the claim goes
with its TTL: from there `status.conditions`, `status.reason` and `Node.status.lastClaim` are the
record, which is why the conditions are stamped by the same write as the event and are the durable
half of the story.

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

### A Pod Needs A Slot With A Specific Label

A pod with `nodeSelector: {topology.kubernetes.io/zone: fsn1}` (or a label affinity, or a
topology spread over a label) that stays `Pending` on a pool that is powered off is a labels
question, and it has three answers: the label is on the machine, the label is on the pool's
template, or the pool cannot be the one that answers.

```bash
kubectl describe pod <pod> | sed -n '/Events:/,$p'
# 0/5 nodes are available: 1 node(s) didn't match Pod's node affinity/selector. preemption: ...
```

**1. Is the label on the node?**

```bash
kubectl get node <node> -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep -i zone
kubectl get node <node> -o jsonpath='{.metadata.annotations.infra\.homecluster\.dev/applied-node-labels}'
```

The record annotation lists the keys the slot wrote. A label that is on the node and not in that
list was put there by something else - the kubelet, a cloud integration, a hand `kubectl label` -
and the operator will not maintain it.

**2. Is the label declared on the slot?**

```bash
kubectl get nodes.infra.homecluster.dev <slot> -n <ns> -o jsonpath='{.spec.nodeLabels}' ; echo
kubectl describe node.infra.homecluster.dev <slot> -n <ns>
#  Normal  NodeLabelsApplied  ApplyNodeLabels  node <node>: labels: +topology.kubernetes.io/zone ...
```

No declaration, no label: the apply-on-join path writes the keys `spec.nodeLabels` declares, and
nothing else. No event, and the declaration present, means the node object the write targets does
not exist (the machine never registered, or `spec.kubernetesNodeName` names the wrong node) - the
apply is skipped until it does, and re-asserted every reconcile after that.

If the slot was refused at `kubectl apply`, the message names the key. `kubernetes.io/hostname`,
`node.kubernetes.io/*`, `node-role.kubernetes.io/*`, `infra.homecluster.dev/*`,
`homelab-autoscaler.dev/*`, `cluster-autoscaler.kubernetes.io/*` and `topology.kubernetes.io/*`
other than `zone` and `region` are keys their owner writes and re-writes; declare a key in your own
namespace (`pool.example.com/rack`), or the zone of a box no cloud controller knows.

The writer refuses the same keys for a slot object that predates the webhook rule, and says so
without swallowing it:

```bash
kubectl describe node.infra.homecluster.dev <slot> -n <ns>
#  Warning  SkippedReservedNodeLabelKey  ApplyNodeLabels  ... declare keys in namespaces their owner controls, they are not applied: kubernetes.io/hostname
kubectl get node <node> -o jsonpath='{.metadata.annotations.infra\.homecluster\.dev/skipped-node-keys}'
```

The Warning fires when the set of refused keys *changes*, and the node object's record says the key
is still being refused on every pass after that. A label that is declared, warned about, and not on
the node is this: take the key out of the declaration, and declare one you own.

**3. Is the pool the one the autoscaler can answer with?**

A pending pod is matched against each pool's **node template**, not against live machines, and a
pool at zero has no machine to read: the template is built from one representative slot, and what
the group's slots agree on is what the estimator sees.

```bash
# the slots of the pool, and what each declares
kubectl get nodes.infra.homecluster.dev -n <ns> -l group=<group> \
  -o custom-columns=NAME:.metadata.name,ZONE:.spec.nodeLabels.topology\.kubernetes\.io/zone

# the provider's own answer, when a group's slots disagree
kubectl logs deployment/homelab-autoscaler-controller-manager -n homelab-autoscaler-system \
  | grep "the slots of a group disagree"
```

- No slot of the group declares the label: no scale-up is planned for this group, and the autoscaler
  is right. Declare it on the slots.
- The slots declare **different** values, or only some of them declare it: the key is **left out of
  the template**, so a pod selecting it is not placed on this group at all. The group cannot guarantee
  the label, because `NodeGroupTemplateNodeInfo` answers for a group with one node while the scale-up
  it authorises buys whichever slot is free (sorted by name).
- The label is on the template and the scale-up runs, and the pod still lands elsewhere: check the
  taints. A node that is a selector match and rejects the pod on a taint is reported by
  `kubectl describe pod` as the same "didn't match" line, and the taint is the pool's
  `spec.nodeTaints` (see [Node Taints](../api-reference/crds/node.md#node-taints)).

**The direction the template is fail-closed in, and the money it saves.** Omitting a disputed key
means the estimator plans nothing for that pod on that pool: the pod stays `Pending`, no machine is
bought, and the pool keeps answering for every pod that does not select the disputed label. The
alternative - answering the group with the representative slot's label - is the expensive failure:
the estimator finds a fit, `NodeGroupIncreaseSize` boots a slot whose machine will never carry the
label, the boot is paid for, the node registers Ready and unusable for that pod, and the pod is still
`Pending` minutes later with the pool one machine up. A group whose slots differ is a configuration
mistake, and the fix is configuration: **declare the key with the same value on every slot of the
group, or put the slots the scheduler must tell apart in separate groups.** A group of one slot, and
a group whose slots declare the same keys with the same values, are exact.

**4. A label that keeps coming back after you removed it, and vice versa**

The declaration is the intent. A value you change on the node with `kubectl label --overwrite` is
put back on the next reconcile, and a key you delete from the slot's `spec.nodeLabels` is removed
from the node on the next reconcile. Fix Git, not the node, and the node follows.

Nothing in the operator watches the `corev1.Node` for a slot's sake: after a hand edit, the pass that
puts the declaration back is the slot's next pass, which for a settled slot is the five-minute idle
resync. Ask for it: patch any annotation on the Node CR (`kubectl annotate node.infra.homecluster.dev
<slot> -n <ns> e2e.homecluster.dev/pass-requested-at=$(date -u +%FT%TZ) --overwrite`) and the write
lands within seconds.

**5. A key that left the spec and will not come off the node**

Removal is keyed by the record the controller keeps on the node object
(`infra.homecluster.dev/applied-node-labels`, `/applied-node-annotations`): the controller removes the
keys it wrote, and never touches a key it did not. If you delete the record annotation by hand and
then delete the key from `spec.nodeLabels`, the controller no longer knows it wrote that label, and it
leaves it on the node - correctly, because from then on that label is one of "the keys the slot never
declared", and the operator has no claim on it. Two ways out, and both are honest: remove the label
yourself (`kubectl label node <node> <key>-`), or put the key back in the spec for one pass so the
record is rewritten, then remove it from the spec again. Do not widen the record by hand to make a
label removable: the record is the ownership claim, and a key listed in it is a key the controller
will overwrite and delete.

**6. A machine that comes up cordoned and stays cordoned, with a label warning**

On a scale-up the labels are written **before** the node is uncordoned, and the write is part of the
readiness gate: a slot that declares labels or annotations whose write to the node object fails keeps
the machine cordoned, and records

```
Warning  DeclaredNodeLabelsUnwritten  Uncordon  Scale-up of node <slot>: the labels and annotations ... could not be written ...
```

while the gate retries. This is deliberate: the group's template carries the label the estimator
placed the pod on, and a machine that becomes schedulable before it carries that label is a machine a
zoned pod is bound to and then evicted from. The gate's deadline still wins - when it expires the
machine is uncordoned (`ReadinessGateTimeout`, and the note repeats what it was waiting on), because
a cordoned machine is one cluster-autoscaler's scale-down never removes. Read the error in the
Warning: it is an API failure on the node object (the node was deleted mid-write, the manager cannot
patch cluster-scoped objects, the API server was unreachable), and the slot reconciler keeps retrying
the same write on every pass.

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