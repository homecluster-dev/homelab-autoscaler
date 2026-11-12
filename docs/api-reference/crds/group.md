# Group CRD Reference

The Group Custom Resource Definition (CRD) defines autoscaling policies for collections of physical nodes.

## API Version
- **Group**: `infra.homecluster.dev`
- **Version**: `v2alpha1` (served and storage; `v1alpha1` is removed in this release)
- **Kind**: `Group`

## Specification

### GroupSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `scaleDownUtilizationThreshold` | `string` | Yes | CPU utilization threshold for scale-down, a decimal string (`"0.5"`) |
| `scaleDownGpuUtilizationThreshold` | `string` | Yes | GPU utilization threshold for scale-down, a decimal string (`"0.5"`) |
| `scaleDownUnneededTime` | `*metav1.Duration` | Yes | Time a node must be unneeded before scale-down |
| `scaleDownUnreadyTime` | `*metav1.Duration` | Yes | Time an unready node waits before scale-down |
| `maxNodeProvisionTime` | `*metav1.Duration` | Yes | Maximum time to wait for node provisioning. Reported to cluster-autoscaler as its own scale-up termination deadline (`GetOptions` → `MaxNodeProvisionDuration`), and the **floor of a claim's `spec.deadlineAt`**: a claim never dies while the autoscaler is still waiting for it |
| `zeroOrMaxNodeScaling` | `bool` | Yes | Whether to scale to zero or max nodes only |
| `ignoreDaemonSetsUtilization` | `bool` | Yes | Ignore DaemonSet pods in utilization calculations |
| `minSize` | `int32` | No | Floor cluster-autoscaler must not scale the group below. Defaults to `0` |
| `nodeReadinessTaints` | `[]corev1.Taint` | No | Taints a machine of this group must be free of before the controller calls the slot ready and uncordons it, on top of the node reporting `Ready`. Defaults to Cilium's `node.cilium.io/agent-not-ready` (`NoExecute`); an empty list gates on `Ready` alone |

### GroupStatus

| Field | Type | Description |
|-------|------|-------------|
| `health` | `string` | `healthy`, `offline`, `unknown` - derived on the same pass: `offline` when no slot of the pool has ever reported a phase (no slots committed, or a pool whose slot reconciler has not run on them), `healthy` when at least one has. A pool whose every machine is `off` is a pool that is asleep, which is what a pool is for, and not an outage; `unknown` is a value an operator stamps by hand, and nothing in the controller writes it |
| `conditions` | `[]metav1.Condition` | The `Loaded` condition the group controller maintains. Its `lastTransitionTime` dates the moment the condition's status last changed, not the last reconcile pass |
| `slots` | `int32` | The `Node` CRs labelled `group: <name>`: the machines the pool can have, i.e. its max size, the same number the provider reports to cluster-autoscaler as `MaxSize` |
| `on` | `int32` | The slots reporting a machine the pool is paying for: `booting`, `ready`, `draining` |
| `off` | `int32` | The rest: `off`, `pending`, and a slot that has never reported. `slots == on + off` always |
| `lastScaleAt` | `*metav1.Time` | When the pool last grew or shrank. A scale-up is dated by the request that made it (`lastClaim.requestedAt`); a scale-down by the instant the scale-down landed (`lastClaim.scaledDownAt`, the deletionTimestamp the request put on the claim), because the request that shrinks a pool names a machine booted by an older request and does not rewrite that request's time |
| `lastScaleAction` | `string` | That scale in one line, naming the slot and the request: `up remote-worker-2`, `down remote-worker-4`, `ca:DeleteNodes remote-worker-3`. Clipped to the field's bound: the line is composed from a slot name, and a status write the API server refuses for a long name costs the pool's whole report |

The group controller writes all of it, in one pass over the slots of the pool, and the pool
reconciles when a slot changes (`Owns(&Node{})`, via the controller reference the slot's reconciler
stamps). The counts are a sum over the slots' `status.phase`; they are a display sum, and no
decision reads them - the provider's arithmetic counts live claims and slots directly
([NodeClaim design](../../design/nodeclaim.md)). The pass compares what it computed with what it
read and writes nothing when they agree: the slot's own write is what woke the pool, so a
pass that repeats the pool's numbers is the common case, and a controller that regenerates the
event it is subscribed to is a pool that never settles in `kubectl get groups -w`.

`lastScaleAt` / `lastScaleAction` are **materialized, not read on demand**, and that is the point.
The same story is a Kubernetes Event on a slot, Events are garbage-collected in about an hour, and
the question - "did this pool thrash last night?" - is asked the next morning. The per-slot record
that outlives a claim's TTL collection is `Node.status.lastClaim` (the digest the claim controller
stamps), so the newest of those over the pool is copied onto the pool: a scale is legible on the
Group a day later, with no Event to have kept and nothing to correlate.

The digest carries **two instants and the controller uses the one that matches the verb**, which is
the whole story of `LASTSCALE`. `requestedAt` is when the request that owned the slot was made;
`scaledDownAt` is when a scale-down named it. `NodeGroupDeleteNodes` rewrites `spec.request`
(the call, the nodes, the requester) on a claim that can be days old and never touches
`spec.requestedAt`, and `NodeGroupDecreaseTargetSize` records its own call and delta on the pending
claim it is about to delete - so the shrink's instant is the deletion timestamp, and a column that
read the episode's request time would report a pool that shrank 24 minutes ago as `LASTSCALE = 3d`.

### The Columns of `kubectl get groups`

```
NAME             SLOTS   ON   OFF   LASTSCALE   HEALTH   AGE
remote-workers   5       2    3     24m         healthy  41d
```

`NAME` and `AGE` are the built-in columns of a Kubernetes table; the five that come between them are
this CRD's. A pool that used to answer "it exists" now answers "five slots, two machines up, the
last thing it did was take one back 24 minutes ago". The columns are asserted on the generated CRD
and on the chart's copy by `internal/controller/infra/crd_columns_test.go`, and their values are
asserted against a live API server in `internal/controller/infra/group_columns_envtest_test.go`.

The read-time companion, for one slot's history and for durations, is the runbook's
[Read the Pool at a Glance](../../troubleshooting/common-issues.md#read-the-pool-at-a-glance).

## Readiness Gate

`nodeReadinessTaints` is the list the gate for a claim's **`ready`** phase waits for: the
`JobCompleted` → Ready transition of the scale-up, which is the only writer of
`NodeClaim.status.phase: ready`. The kubelet marks a node Ready long before the CNI has programmed its datapath, and the
pool's workloads do not tolerate the bootstrap taint the CNI stamps while it works, so a slot declared
ready in that window is read by cluster-autoscaler as capacity its pending pod cannot land on - and
the pool buys a second machine for a node that is about to work. The controller holds the scale-up
open until the taint is gone, so pool workloads need no toleration for it.

- The **key** is what the gate matches; Kubernetes keeps one taint per key on a node object, so a
  declaration's `value` and `effect` are documentation and are not compared.
- The CRD defaults the list to Cilium's key (Cilium has stamped `node.cilium.io/agent-not-ready`
  since v1.9; the pool this chart deploys runs 1.16.x). A Group written before the field existed gets
  the gate it was already getting.
- `nodeReadinessTaints: []` turns the gate off: a pool on a CNI that stamps no bootstrap taint gates
  on `Ready` alone.
- These are the taints the gate **reads**. The taints to stamp on the machine are
  `spec.nodeTaints` of the Node CR, and a key must not be in both: the gate's key is stamped and
  removed by its owner, so a declaration that stamps it makes the slot's own scale-up wait for a
  taint the slot keeps putting back. `taints.IsProtectedKey` refuses Cilium's key in `nodeTaints`.
- The gate is bounded: 90s for the node object to register, then 4m for readiness, counted from the
  moment the object appears. A timeout **uncordons** the node, records a `ReadinessGateTimeout`
  Warning on the slot, and **fails the claim** with that reason, because a cordoned node is skipped
  by cluster-autoscaler's scale-down - never drained, never deleted, and paid for forever. The
  input law then powers the machine off, the slot becomes free, and the next claim on it is
  attempt N+1. Under `v1alpha1` a timed-out gate completed the transition and recorded the slot
  `ready`; the claim model refuses to call capacity that cannot take the pool's work ready.

```yaml
spec:
  # The pool boots on a CNI that stamps its own startup taint.
  nodeReadinessTaints:
  - key: node.cilium.io/agent-not-ready
    effect: NoExecute
---
spec:
  # flannel / a cloud CNI: no bootstrap taint, so no gate beyond Ready.
  nodeReadinessTaints: []
```

The keys a slot is gated on are recorded on the slot as the `NodeReadinessTaints` condition, so a slot
that has been booting for ten minutes says what it is waiting for. See
[State Management](../../architecture/state.md).

## Example

```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: worker-nodes
  namespace: homelab-autoscaler-system
spec:
  # Keep one node of the pool up; omit for a group that may scale to nothing.
  minSize: 1
  scaleDownUtilizationThreshold: "0.5"
  scaleDownGpuUtilizationThreshold: "0.5"
  scaleDownUnneededTime: "10m"
  scaleDownUnreadyTime: "20m"
  maxNodeProvisionTime: "15m"
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: true
```

## Usage

### Creating a Group

1. **Set scaling thresholds** for CPU and GPU utilization
2. **Configure timing parameters** for scale-down behavior
3. **Apply the Group resource** to your cluster

The size of a group is not part of the spec beyond `minSize`: its max size is the number
of Node CRs labelled `group: <group name>`, so a pool grows and shrinks by committing
slots, not by editing the Group.

```bash
kubectl apply -f group.yaml
```

### Monitoring Group Status

```bash
# Check group status
kubectl get groups -n homelab-autoscaler-system

# Get detailed group information
kubectl describe group worker-nodes -n homelab-autoscaler-system

# Watch group status changes
kubectl get groups -n homelab-autoscaler-system -w
```

`kubectl get groups` answers the shape of the pool (slots, up, down, last scale); `kubectl describe
group` adds the sentence of the last scale (`lastScaleAction: ca:DeleteNodes remote-worker-3`) and
the `Loaded` condition.

## Scaling Behavior

### Scale-Up Triggers
- Pod scheduling failures due to insufficient resources
- CPU/memory pressure on existing nodes
- External scaling requests via gRPC API

### Scale-Down Triggers
- Node utilization below `scaleDownUtilizationThreshold`
- GPU utilization below `scaleDownGpuUtilizationThreshold`
- Node unneeded for longer than `scaleDownUnneededTime`
- Node unready for longer than `scaleDownUnreadyTime`

### Minimum Size

`minSize` is the floor of the group. The gRPC cloud provider reports it to
cluster-autoscaler as the min size of the node group, and refuses its own scale-down
entry points (`NodeGroupDeleteNodes`, `NodeGroupDecreaseTargetSize`) when the request
would take the group below it, so a pool that has to keep a machine up sets `minSize: 1`
and the autoscaler stops reclaiming that node. A pool that used a keepalive pod to pin an
always-on slot above the utilization threshold can drop the pod once the group declares
the floor.

Two things to know:

- `0` is the default and matches the behaviour of every release before the field existed:
  the group may scale down to nothing.
- `maxSize` is **not** a field. The provider derives it by counting the Node CRs labelled
  `group: <group name>`. The CRD therefore cannot assert `minSize <= maxSize`; the gRPC
  boundary does, clamping a `minSize` above the number of slots to that count and refusing
  a scale up that would exceed it.

`minSize: 0` is a supported floor, and a pool that uses it answers one more question: with no slot
registered, cluster-autoscaler sizes the group's node template from
[`spec.nodeCapacity` on the Node CRs](node.md#node-capacity). A pool at zero that declares nothing
has a template with no cpu and no memory, no pod with requests fits it, and `NodeGroupIncreaseSize`
is never called, so the group stays at zero and the floor that works is `minSize: 1`.

### Timing Parameters

| Parameter | Purpose | Typical Value |
|-----------|---------|---------------|
| `scaleDownUnneededTime` | Prevents flapping by requiring sustained low utilization | `10m` |
| `scaleDownUnreadyTime` | Removes unresponsive nodes | `20m` |
| `maxNodeProvisionTime` | Timeout for node startup operations | `15m` |

## Best Practices

### Resource Planning
- Account for startup time in `maxNodeProvisionTime`

### Threshold Tuning
- Start with conservative thresholds (0.5-0.7) and adjust based on workload patterns
- Monitor actual utilization patterns before optimizing thresholds
- Consider workload characteristics (CPU vs memory intensive)

## Troubleshooting

### Common Issues

1. **Nodes not joining the group**
   - Check that the `group` label is set on the Node CRs: the group's slots are exactly the Node
     CRs labelled `group: <name>`, and that count is the group's `maxSize`

2. **Scaling not working**
   - Verify gRPC server is running and accessible
   - Check controller logs for errors
   - The group's requests are objects: `kubectl get nodeclaim -n homelab-autoscaler-system
     -l group=<group>` — `pending` means the provider answered the autoscaler and nothing has
     started, a `failed` claim says why in `status.reason`

3. **The pool grows past its floor, or refuses to shrink**
   - `spec.minSize` is enforced by the provider at `DeleteNodes` and `DecreaseTargetSize`, judged
     against the target size the request can actually produce (the count of live claims)

### Debugging Commands

```bash
# Check group controller logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager

# List the slots of the pool (the Node CRs are the pool's members)
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -l group=worker-nodes

# The machines of the pool that have registered, as the kubelet reports them. The operator stamps
# `infra.homecluster.dev/group` on the corev1.Node; the bare `group` label is the Node CR's.
kubectl get nodes -l infra.homecluster.dev/group=worker-nodes

# Check the requests of the group, and the budget they carry
kubectl get nodeclaim -n homelab-autoscaler-system -l group=worker-nodes -o wide

# Verify the gRPC service, and that its selector resolves to a ready pod
kubectl get service,endpoints -n homelab-autoscaler-system -l control-plane=controller-manager
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc-service 50051:50051
```

## Related Documentation

- [Node CRD](node.md) - Individual node management
- [NodeClaim CRD](nodeclaim.md) - the requests the pool's capacity is made of, and where
  `maxNodeProvisionTime` lands: `spec.deadlineAt`, floored by this field
- [Architecture Overview](../../architecture/overview.md) - System design
