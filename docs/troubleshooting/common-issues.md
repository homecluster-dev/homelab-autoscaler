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
```bash
# Power on a specific node
kubectl patch node.infra.homecluster.dev <node-name> \
  --type='merge' -p='{"spec":{"powerState":"on"}}'

# Power off a specific node
kubectl patch node.infra.homecluster.dev <node-name> \
  --type='merge' -p='{"spec":{"powerState":"off"}}'

# Force status refresh
kubectl annotate node.infra.homecluster.dev <node-name> \
  homelab-autoscaler.dev/force-reconcile=$(date +%s)
```

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
controller uncordons the node, records a `ReadinessGateTimeout` **Warning** on the slot, and completes
the transition (a cordoned node is skipped by cluster-autoscaler's scale-down, so it would never be
drained, never deleted, and the pool would keep paying for it). The taint is visible on the node
object:

```bash
kubectl get node <node> -o jsonpath='{.spec.taints}' ; echo
kubectl get node <node> -o jsonpath='{.spec.unschedulable}' ; echo
```

### The Slot Reports a Machine That Is Not There

`status.progress: ready`, `startingup` or `shuttingdown` on a slot with no `corev1.Node` and no
lifecycle Job is what a machine deleted out of band leaves behind (a Hetzner server removed from the
console, a startup whose manager restarted mid-flight). What the node controller does depends on the
slot's own request, and it waits out a ~7 minute grace around a lifecycle Job that just finished
(`progress.SettleGracePeriod`, derived from the deadlines the completed Job's own hook can consume):

- `spec.desiredPowerState: off` -> **settled** to `off`/`shutdown`, one `ProgressReconciled` Warning
  naming the progress it replaced.
- `spec.desiredPowerState: on` (the autoscaler asked for this machine) -> **re-armed**: same pair, and
  the claim left on, so the pool starts the machine again. The `ClaimRearmed` event names the attempt
  the arming leads to. The retries are bounded by one **startup-attempt counter** (3 per claim episode,
  `progress.ClaimRearmLimit`): it counts the startup attempts of the slot, it is recorded by the
  `StartNode` transition, and it is cleared when a boot passes the readiness gate. Both loops pay into
  it - a startup whose machine never registers, and a startup Job that exits non-zero (that one leaves
  the slot at the stable `off`/`shutdown` pair, so the watchdog never looks at it: the FSM's `StartNode`
  is what refuses). Past the bound the watchdog **settles** the slot, `StartNode` refuses to start it,
  and one `ClaimRearmLimitReached` **Warning** is recorded: the slot is counted as requested capacity
  with no machine behind it, and nothing will retry it again on its own. Scale the group down, or fix
  what keeps the machine from booting (startup command, image, power API credentials); a boot that
  passes the readiness gate clears the counter and the 3 attempts with it.
- `spec.desiredPowerState: on`, at `ready`, and the node is cordoned with no Job, lock, or
  autoscaler marker on it -> **uncordoned**, with a `CordonReconciled` Warning. This is the residue of
  a shutdown that timed out or failed: the paths that roll back into `ready` never reach the uncordon
  hook, and a cordoned node is never a scale-down candidate, so it is never removed.

```bash
kubectl describe node.infra.homecluster.dev <node-name>
# Events: ProgressReconciled | ClaimRearmed | ClaimRearmLimitReached | CordonReconciled
# Conditions: ClaimRearmed (ClaimRearmAttempt<N> while the attempts are being spent, then
#            ClaimRearmLimitReached) - the startup-attempt record, readable from the slot
kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name>
kubectl get node <kubernetes-node-name> 2>&1              # no machine behind the slot
```

A slot the watchdog leaves alone is a slot an operation owns: a `corev1.Node` (its status belongs to
the observation controllers), a startup/shutdown Job in flight, a Job that finished inside the grace,
a startup stamped inside the grace by `status.lastStartupTime` when the Job object is already gone
(the TTL collected it), or a live coordination lock.

### The Slot Says Shutdown But The Machine Is Up

The mirror of the section above: there the progress named a machine the cluster did not have, here the
progress **denies** a machine the cluster can see. Read the pair:

```bash
kubectl get node.infra.homecluster.dev <node-name> -o jsonpath='{.spec.powerState} {.status.powerState} {.status.progress}' ; echo
kubectl get node <kubernetes-node-name> -o jsonpath='{.status.conditions[?(@.type=="Ready")].status} {.status.conditions[?(@.type=="Ready")].lastHeartbeatTime}' ; echo
kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-name>
```

`{status.powerState: on, status.progress: shutdown}` on a slot with `spec.powerState: off` is a
machine that outlived its record: a shutdown Job exited 0 and the provider's power-off did not land
(the request was accepted for a server that kept running, the script swallowed the error, a graceful
ACPI request came back up). The record is not cosmetic. The FSM derives its state from
`status.progress`, so the slot is `shutdown`; the only event legal from there is `StartNode`, the
claim is `off`, and `ShutdownNode` - the operation that would satisfy it - is unreachable. Before this
rule the reconcile logged "Node is in desired state or transitioning" every pass and the machine ran
for 40 minutes until `status.progress` was patched by hand.

The controller now closes it in two places, and both say so on the slot:

- the shutdown completion is **conditional** (`internal/fsm/shutdown_completion.go`): a completed
  shutdown Job on a machine the cluster observes up is not recorded as `shutdown` at all. The slot is
  recorded `ready`, which is the state the FSM can act on, and one `ShutdownNotObserved` **Warning**
  names the Job that claimed the machine was off. The next reconcile runs the shutdown again, with its
  Job, its lock and its cordon. The guard reads the kubelet's **heartbeat** and not the `Ready`
  condition, so a healthy scale-down - whose node object stays `Ready` for
  `node-monitor-grace-period`, ~40s, after the machine goes off - pays no second shutdown Job;
- the precedence is enforced again in the reconcile (`progress.ReconcileObservedPower`): with
  `status.powerState: on`, a `corev1.Node` reporting `Ready`, and a progress that implies no machine
  (`shutdown`, `shuttingdown`, or empty), the observation outranks the record, the progress is
  corrected to `ready`, and - for a claim of `off` - the **same pass** fires `ShutdownNode`. Nothing
  waits for a grace period: the claim is already off and the machine is burning money. One
  `MachineObservedUp` **Warning** names the progress it replaced; on a claimed-`on` slot the same
  correction is a **Normal**, with no Job, because the machine is the one the pool asked for.

```bash
kubectl describe node.infra.homecluster.dev <node-name>
# Events: ShutdownNotObserved | MachineObservedUp  (each names the progress / Job it is talking about)
```

Both rules share the lifecycle-Job and coordination-lock gates, so a shutdown that is genuinely in
flight is left to its own operation. What they cannot fix is the pool's own shutdown script: if
`spec.shutdownPodSpec` exits 0 without the provider's power-off landing, the slot loops - shutdown
Job, machine still up, `ShutdownNotObserved`, retry - and the visible symptom moves from a frozen slot
to a Warning per attempt. Make the script call the provider's own power-off and exit non-zero when the
machine is still on.

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
`spec.desiredPowerState` - the scale-down request itself is the gRPC provider's to make.

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