# Node CRD Reference

The Node Custom Resource Definition (CRD) is the declaration of one machine slot in the pool: what
the slot *is* (its lifecycle pods, its shape, its taints, its price), and nothing about what anyone
wants it to do next.

## API Version
- **Group**: `infra.homecluster.dev`
- **Version**: `v2alpha1` (served and storage; `v1alpha1` is removed in this release)
- **Kind**: `Node`

## Specification

### NodeSpec

The spec is Git-owned configuration. **`spec.powerState` is removed in `v2alpha1`**: there is no
field for a runtime intent, so an apply can never contradict a scale-up and a scale-up can never
overwrite a declaration. A machine is wanted exactly when a live
[`NodeClaim`](nodeclaim.md) names this slot.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `startupPodSpec` | `MinimalPodSpec` | Yes | Pod specification for startup operations |
| `shutdownPodSpec` | `MinimalPodSpec` | Yes | Pod specification for shutdown operations |
| `kubernetesNodeName` | `string` | Yes | Name of the corresponding Kubernetes node |
| `pricing` | `PricingSpec` | Yes | Cost information for the node |
| `nodeCapacity` | `corev1.ResourceList` | No | The shape of the machine this slot boots, as the kubelet reports it. Applied to the node template resource by resource, which is what lets a pool at `minSize: 0` scale back up; `pods` is defaulted for you |
| `nodeLabels` | `map[string]string` | No | Labels this machine must carry on its `corev1.Node`: applied on join, re-asserted on drift, removed when the declaration drops the key, and carried into the node template so cluster-autoscaler can match a `nodeSelector` against this pool |
| `nodeAnnotations` | `map[string]string` | No | The same, for annotations. Applied, re-asserted and removed by the same rules; annotations play no part in scheduling |
| `nodeTaints` | `[]corev1.Taint` | No | Taints this machine must carry, applied to the Kubernetes node and included in the node template |
| `taintsToRemove` | `[]corev1.Taint` | No | Taints stripped from the node template so the autoscaler ignores them |

#### MinimalPodSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `image` | `string` | Yes | Container image for the operation |
| `command` | `[]string` | No | Command to execute |
| `args` | `[]string` | No | Arguments for the command |
| `volumes` | `[]VolumeMountSpec` | No | Volume mounts for secrets/configmaps |
| `serviceAccount` | `*string` | No | ServiceAccount for the pod |
| `priorityClassName` | `*string` | No | PriorityClass assigned to the job pod, e.g. `system-cluster-critical` |

#### VolumeMountSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Volume name |
| `mountPath` | `string` | Yes | Path to mount the volume |
| `secretName` | `string` | No | Secret to mount (mutually exclusive with configMapName) |
| `configMapName` | `string` | No | ConfigMap to mount (mutually exclusive with secretName) |

#### PricingSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `hourlyRate` | `float64` | Yes | Cost per hour when running |
| `podRate` | `float64` | Yes | Cost per pod scheduled on the node |

### NodeStatus

| Field | Type | Description |
|-------|------|-------------|
| `powerState` | `PowerState` | What the provider observes: `on` when the `corev1.Node` reports `NodeReady=True`, `off` otherwise. An observation, and one of the three inputs of every decision |
| `phase` | `NodePhase` | The **derived report** of the slot: `off`, `pending`, `booting`, `ready`, `draining`. Computed from (live claim, observed power, node usability) and read by nothing that decides. `status.progress` is removed with the model that needed it |
| `lastClaim` | `LastClaimSummary` | The digest of the slot's most recent claim: `{name, phase, attempt, call, group, reason, requestedAt, deadlineAt, finishedAt}`. It survives the claim's TTL collection, seeds the next claim's `status.attempt`, and answers "who asked, when, how many attempts, why did it end" from `kubectl describe` |
| `conditions` | `[]metav1.Condition` | Standard Kubernetes conditions: the autoscaler markers (`AutoscalerDeletionCandidate`, `AutoscalerDeletionRequested`), `NodeReadinessTaints` (the gate this slot is waiting on), lifecycle conditions written by the transitions |
| `health` | `string` | Health status: `healthy`, `offline`, `unknown` |
| `lastStartupTime` | `*metav1.Time` | Last successful startup time |
| `lastShutdownTime` | `*metav1.Time` | Last successful shutdown time |

#### PowerState

```go
type PowerState string

const (
    PowerStateOn  PowerState = "on"   // the node object reports NodeReady=True
    PowerStateOff PowerState = "off"  // everything else, including a NotReady leftover
)
```

`on`/`off` accept `true`/`false` on the way in, for manifests written against the old field.

#### Phase (derived, reported)

```go
type NodePhase string

const (
    NodePhaseOff       NodePhase = "off"       // no live claim, no observed machine
    NodePhasePending   NodePhase = "pending"   // a live pending claim, no lifecycle Job yet
    NodePhaseBooting   NodePhase = "booting"   // a live claim whose boot is in flight
    NodePhaseReady     NodePhase = "ready"     // a ready claim, node observed Ready and gate-clear
    NodePhaseDraining  NodePhase = "draining"  // a machine the pool no longer wants, observed up
)
```

The table that computes it is in [State Management](../../architecture/state.md#the-input-law).
It is a report: write a value that contradicts the claim and the observation and nothing about the
operator's behaviour changes — the next pass overwrites the value from its inputs. The phase the
model acts on is the **claim's** (`NodeClaim.status.phase`).

### Node Taints

Declare the taints a machine must carry in `spec.nodeTaints`:

```yaml
spec:
  nodeTaints:
  - key: dedicated
    value: remote
    effect: NoSchedule
```

What the field gives you:

- cluster-autoscaler simulates the machine your pool boots. The provider puts the declared taints
  in the node template, so `dedicated=remote:NoSchedule` is there before the machine registers,
  and a pod that doesn't tolerate it is never placed on this group.
- The taints are in Git with the rest of the slot, and they survive node recreation. Nothing
  depends on a startup script running.

The controller applies them to the Kubernetes node as soon as the node object exists: the
scale-up path stamps them in the hook that uncordons the node, so the machine carries them before
it becomes schedulable, and every later reconcile re-applies them.

A tainting startup Job can't do that. It runs after the node joined, which is after the
autoscaler simulated a placement on an untainted template and after the node was schedulable, so
pods land on a machine that rejects them.

The window this closes is the one the controller drives. It can only patch a node that exists, so
a machine you booted by hand, or one that registered while the controller was down, can be Ready
and schedulable for a pass before the taint lands. Keep the tolerations of your own startup
tooling correct.

The field only adds, and it keeps adding:

- A taint you don't declare is never removed.
- A taint you declare and then remove by hand with `kubectl taint <node> <key>-` is put back on
  the next reconcile. The declaration is the intent, the `kubectl` run isn't.

Keys the controller won't write, because a Kubernetes component owns them and re-applies them on
its own sync loop:

| Keys | Owner |
| --- | --- |
| `node.kubernetes.io/not-ready`, `node.kubernetes.io/unreachable`, `node.kubernetes.io/unschedulable`, `node.kubernetes.io/out-of-service` | node lifecycle controller |
| `node.kubernetes.io/memory-pressure`, `node.kubernetes.io/disk-pressure`, `node.kubernetes.io/pid-pressure`, `node.kubernetes.io/network-unavailable` | kubelet |
| `ToBeDeletedByClusterAutoscaler`, `DeletionCandidateOfClusterAutoscaler` | cluster-autoscaler |
| `node.cilium.io/agent-not-ready` | Cilium (the bootstrap taint the scale-up readiness gate waits for - see the Group's `nodeReadinessTaints`) |

A declaration on one of those keys is dropped, and the drop is visible: a Warning event on the
Node CR (`kubectl describe node.infra.homecluster.dev <name>`, reason
`SkippedProtectedTaintKey`) and a `V(1)` log line. Nothing is dropped silently.

The `node.kubernetes.io/` prefix is a namespace, not an ownership claim. The
`node.kubernetes.io/role=agent` taint your k3d agents carry is yours to declare, and the
controller applies it.

`spec.taintsToRemove` is the opposite field and is unrelated: it filters taints out of the node
template so the autoscaler ignores taints your startup process leaves behind. It never touches
the live node. A key listed in both fields ends up on the template, because `nodeTaints` is what
the machine boots with.

If you migrate a pool from a tainting startup Job, drop the `kubectl taint` step from the Job and
set `nodeTaints` on the Node CRs. The template carries the taint as soon as the CR is applied;
the node gets it the next time the controller touches it, which on a scale-up is before the node
is uncordoned.

### Node Labels and Annotations

Declare the labels and annotations a machine must carry in `spec.nodeLabels` and
`spec.nodeAnnotations`:

```yaml
spec:
  nodeLabels:
    topology.kubernetes.io/zone: fsn1
    pool.homecluster.dev/rack: rack-3
  nodeAnnotations:
    pool.homecluster.dev/serial: SN-0001
```

The labels are the half that changes behaviour. Scheduling is label matching, and
cluster-autoscaler decides which pool to grow by matching the same selector against the pool's node
template, so a label on a slot is the answer to "which machine takes this pod". Annotations are
metadata for whatever reads the node, and for the template that stands in for it; they never take
part in a placement.

The controller writes them onto the `corev1.Node`, and keeps the node at the declaration:

- The labels land when the node object exists. On a scale-up they are stamped in the same hook that
  uncordons the node, so a node is never schedulable for a pod whose selector it does not yet
  satisfy.
- Drift is put back. Change the value with `kubectl label --overwrite`, delete a key, reboot a
  machine whose kubelet stamps its own set, let a re-registration recreate the node object: the next
  reconcile re-asserts the declaration.
- A key you drop from the declaration is removed from the node on the next reconcile. The controller
  records the keys it wrote on the node object
  (`infra.homecluster.dev/applied-node-labels`, `/applied-node-annotations`), and it owns exactly
  those.
- A key the slot never declared is never touched: the kubelet's `kubernetes.io/hostname`,
  a cloud integration's topology labels, anything another tool added, all stay as they are. Two
  writers on one key never converges, and the owner wins the last write.
- The writes are idempotent: a node that already matches is not patched, so a resync of a settled
  slot writes nothing. One `Normal` event, reason `NodeLabelsApplied`, names the keys that were
  added (`+`), rewritten (`~`) and removed (`-`) on the write that happened:

```bash
kubectl describe node.infra.homecluster.dev <node-name>
# NodeLabelsApplied  node worker-01: labels: +topology.kubernetes.io/zone ~rack -old; annotations: +pool.homecluster.dev/serial
kubectl get node worker-01 -o jsonpath='{.metadata.annotations.infra\.homecluster\.dev/applied-node-labels}'
```

Both fields are bounded to 64 entries (`maxProperties` on the CRD), and the webhook checks what a
CRD pattern cannot: that every key is a valid label key, every label value a valid label value, the
annotations fit the 256 kB the API server allows on the node object, and no key is in a namespace
someone else owns. A slot that fails any of it is refused at `kubectl apply`, naming the key.

Keys the controller refuses to write, with the reason in the admission message:

| Label keys | Owner, and why the refusal |
| --- | --- |
| `infra.homecluster.dev/*` | this operator: it stamps `group` and `node` on the node object, and keeps the applied-keys records there |
| `node.kubernetes.io/*` | the kubelet and the node lifecycle controller: `role`, `os`, `arch`, `instance-type`, `exclude-home`, `drain-status` |
| `kubernetes.io/*` | the kubelet and the cloud integrations: `hostname`, `os`, `arch`, the version keys |
| `node-role.kubernetes.io/*` | the tooling that joins a machine: kubeadm, k3s, RKE2 |
| `topology.kubernetes.io/*` | the topology machinery - **except** `topology.kubernetes.io/zone` and `topology.kubernetes.io/region`, which are yours to declare |

| Annotation keys | Owner, and why the refusal |
| --- | --- |
| `infra.homecluster.dev/*` | this operator: the applied-keys records and the coordination locks |
| `argocd.argoproj.io/*` | Argo CD's tracking and sync annotations (`tracking-id`, `sync-wave`, `sync-options`) |
| `kubectl.kubernetes.io/*` | kubectl's own records on a live object (`last-applied-configuration`, the cordon/drain entries) |

The `node.kubernetes.io/` and `kubernetes.io/` namespaces are reserved for **labels** only. Nothing
stamps an annotation there on its own, so a note under them is yours, and admission says so.

`topology.kubernetes.io/zone` and `topology.kubernetes.io/region` are the exception because a
homelab has no cloud node controller to ask where a box is: the slot's declaration in Git is the
only source of that fact, and the only thing the autoscaler can match a zoned pod against.

### Scheduling with labels

The case the field exists for. A rack of machines in one site, and a stateful workload that may only
run in that site:

```yaml
# the slot: the zone is in Git with the rest of the machine's declaration
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: storage-01
  labels:
    group: storage-nodes
spec:
  kubernetesNodeName: storage-01
  nodeLabels:
    topology.kubernetes.io/zone: fsn1
---
# the pod: it asks for that zone, and only a pool whose template carries it is a candidate
apiVersion: v1
kind: Pod
metadata:
  name: database
spec:
  nodeSelector:
    topology.kubernetes.io/zone: fsn1
```

What happens, in order:

1. The slot's declaration lands on the `corev1.Node` when the machine joins, and is kept there
   against drift.
2. The provider puts the same labels into the group's node template, the object cluster-autoscaler
   simulates a placement on. A pool whose every slot is off gets a placeholder carrying the
   declaration, so the label is available before any machine has ever registered.
3. A pending pod with that `nodeSelector` now fits that group and no other, the estimator plans a
   scale-up on it, and `NodeGroupIncreaseSize` buys the machine.

Before the field, every slot of a pool reported the same template, so a pod selecting a label no
template carried was placed on no pool: the scale-up never happened, and the pod stayed Pending with
a message about a node affinity it never matched.

**A group answers with one template.** `NodeGroupTemplateNodeInfo` is called per *group*, and one
call returns one node, so the provider builds the template from the representative slot it already
picks - a slot whose node object exists, a Ready one preferred, and any slot of a fully powered off
pool. Slots of the same group that declare **different** labels are therefore simulated from one of
them, and a pod selecting a label only some slots carry can be answered with a machine that does not
have it. The consequence is configuration advice, not a bug to work around: put the slots you need
the scheduler to tell apart in different groups, and let every slot of a group declare the same
labels. A group whose slots declare the same labels, and a pool that declares nothing, are both
exact.

**For machines that already exist the node object is the source.** `NodeGroupNodes` reports an
instance as an id and a status, and the proto carries no labels, no annotations and no capacity, so
what cluster-autoscaler knows about a registered machine's labels it reads off that machine's
`corev1.Node`. That is why the declaration is stamped on the node and not only handed to the
autoscaler: the apply-on-join half is what the autoscaler sees about the machines it already has.


### Node Capacity

Declare the shape of the machine a slot boots in `spec.nodeCapacity`:

```yaml
spec:
  nodeCapacity:
    cpu: 1950m
    memory: 3.2Gi
```

What the field gives you is a pool that can come back up from zero. cluster-autoscaler decides
whether a pod fits on a machine by simulating it on the group's node template, and that template
is built from a `corev1.Node`. A pool whose every slot is powered off has no `corev1.Node` at all,
so the provider synthesizes a placeholder, and a placeholder it has no size for carries no cpu and
no memory. A pod with any resource request then fits nowhere, `NodeGroupIncreaseSize` is never
called, and the pool stays at zero. That is why a `minSize: 0` pool needs an always-on machine
today.

Capacity is the one part of the machine's shape a Node CR can answer for, because you know the
machine you are going to boot. It is declared in the spec and not copied into the status for the
same reason: the template is needed before a slot has ever booted, so a status field fed from a
live node is empty in exactly the case it would be read for, and keeping it filled means a pipeline
that watches nodes and writes status. One field next to the `nodeTaints` of the same machine is
reviewable in Git, and it describes a machine that does not exist yet.

Declare what the kubelet reports, not what the box is sold as. The system reservation moves the
number: a 2 vCPU / 4 GiB machine, a Hetzner CX23 for instance, reports `1950m` and `3.2Gi`.

| Declared | Reported by the kubelet | Verdict |
| --- | --- | --- |
| `1950m` | `2` | 2.5% away, fine |
| `1950m` | `1` | the machine is not the one you described |

The autoscaler judges the pod's requests against the template, so a declaration larger than reality
places work the machine cannot take, and a smaller one leaves capacity unused.

Precedence in the template, resource by resource:

- Every resource you declare wins over the live node's `status.capacity` and
  `status.allocatable`, on the path that copies a registered node as well as on the placeholder.
  The declaration is the shape of the machine the pool boots, and it is one value for every slot of
  the group. Reading the live node would make the group's template change with which slot happens to
  be Ready, and in a mixed pool it hands the estimator a different machine per call.
- A resource you don't declare keeps the live node's number: `pods`, `ephemeral-storage`, hugepages,
  `nvidia.com/gpu`, anything a device plugin adds. The estimator judges a pod against all of them,
  so declaring cpu and memory declares the shape of the box, not the list of resources it has.
  Replacing the whole list with a declaration is what makes a documented cpu-and-memory declaration
  unplaceable: the template loses `pods`, and the scheduler answers every pod "Too many pods".
- `pods` is defaulted for you. A template whose lists carry none gets `pods: 110`, the kubelet's
  default `--max-pods` and the number cluster-autoscaler's own template builders put in. Declare it
  only to move off that default, and a live node's own pod ceiling is never lowered to it. Nothing
  else is defaulted: cpu, memory and disk are facts about one machine, and the provider doesn't
  invent machines.
- With the field unset, the live node's own numbers are used. A pool that declares nothing keeps
  working exactly as before, and a pool that declares nothing and is fully powered off gets a
  template with no cpu, no memory and no disk: the honest "this machine has no known size", never a
  guess.

The declaration goes into `status.capacity` as well as `status.allocatable`, because
cluster-autoscaler reads both. Allocatable is what a pod's requests are judged against; capacity
feeds the bookkeeping over the group's machines, the quota simulation and the resources it reports
for registered nodes. A template that fills only one of the two is judged by the other. A template
machine has no system daemons to subtract, so a placeholder's two lists end up identical; on a node
copied from a live slot each keeps its own numbers, overlaid resource by resource.

The controller checks the declaration against the machine once it registers:

- A `status.allocatable` more than five percent away from the declaration, on any declared
  resource, puts a `Warning` event on the Node CR with reason `NodeCapacityMismatch`, naming the
  resource and both values. Read it with
  `kubectl describe node.infra.homecluster.dev <name> -n <namespace>`.
- Only the resources you declared are compared, so an incomplete declaration is not detected. Leave
  `memory` out and no warning ever mentions it, while a pod that asks for memory is judged against a
  template with no size for it. Declare every resource your workloads ask for; `pods` is the one
  resource the provider fills in for you.
- It warns only. The node is never reconciled to the declared values: the kubelet owns
  `status.allocatable`, and a machine that registered is the truth about what booted. Fix it by
  updating `spec.nodeCapacity`, or by fixing the machine.
- Nothing is compared until the node is Ready, because before the kubelet reports allocatable there
  is no measurement to compare. A pool that declares nothing is never warned about.

### Node Template

The provider reports one machine shape per group, built from a slot of that group:

1. A slot whose Kubernetes node object exists, a Ready one preferred. That node object is the
   only source of `status.allocatable`, `spec.taints` and the labels a pod needs to schedule, so
   the autoscaler simulates a real machine. The slot's declared `nodeLabels` and `nodeAnnotations`
   are overlaid on the copy, declaration last, so the group answers with the label the pool is
   configured to carry and not with the value one live node happens to have.
2. If no slot has a node object (the whole pool is powered off), the provider synthesizes a
   placeholder from the Node CR. It carries the group label, the declared `nodeLabels` and
   `nodeAnnotations`, the declared `nodeTaints` and the declared `nodeCapacity`, on top of the
   default pod ceiling. No machine size is invented: a pool that declared `nodeCapacity` is
   answering the question from Git, and a pool that declared nothing gets a template with no cpu,
   no memory and no disk, because the size of a machine that never registered can't be read.

One template per group, from one slot: a group whose slots declare different labels is simulated
from the slot that was picked. Slots the scheduler must tell apart belong in different groups (see
[Scheduling with labels](#scheduling-with-labels)).

Because the template is copied from a live node, the autoscaler's own taints are filtered out of
it: `ToBeDeletedByClusterAutoscaler` and `DeletionCandidateOfClusterAutoscaler` on the
representative node describe that machine's scale-down state, not the shape of a new one, and
copying them into the group's template leaves the estimator with a taint no pod tolerates. The
transient `node.kubernetes.io/unschedulable`, `not-ready` and `unreachable` taints are filtered
out the same way. Keys the pool declares, `node.kubernetes.io/role` included, are not.

## Example

```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: worker-01
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
    node-type: worker
    zone: homelab
spec:
  kubernetesNodeName: worker-01
  startupPodSpec:
    image: homelab/power-manager:latest
    command: ["/bin/sh"]
    args: ["-c", "wake-on-lan 00:11:22:33:44:55"]
    volumes:
    - name: credentials
      mountPath: /etc/credentials
      secretName: power-credentials
    serviceAccount: power-manager
  shutdownPodSpec:
    image: homelab/power-manager:latest
    command: ["/bin/sh"]
    args: ["-c", "ssh root@worker-01 'shutdown -h now'"]
    volumes:
    - name: ssh-keys
      mountPath: /root/.ssh
      secretName: ssh-keys
    serviceAccount: power-manager
  pricing:
    hourlyRate: 0.05
    podRate: 0.001
  nodeCapacity:
    cpu: 1950m
    memory: 3.2Gi
  nodeLabels:
    topology.kubernetes.io/zone: fsn1
    pool.homecluster.dev/rack: rack-3
  nodeAnnotations:
    pool.homecluster.dev/serial: SN-0001
  nodeTaints:
  - key: dedicated
    value: remote
    effect: NoSchedule
status:
  health: healthy
  powerState: on          # observed
  phase: ready            # derived from (live claim, observed power, node usability)
  lastClaim:              # the digest of the request that owns this machine
    name: worker-01-7f4b9c
    phase: ready
    attempt: 2
    call: IncreaseSize
    group: worker-nodes
    requestedAt: "2025-01-01T11:52:00Z"
    deadlineAt: "2025-01-01T11:59:10Z"
  lastStartupTime: "2025-01-01T12:00:00Z"
  conditions:
  - type: NodeReadinessTaints
    status: "True"
    reason: ReadinessTaints
    message: "node.cilium.io/agent-not-ready"
    lastTransitionTime: "2025-01-01T11:52:00Z"
```

The claim behind that status is [the NodeClaim CRD](nodeclaim.md).

## Power Management

### Startup Process

1. **Trigger**: a live `NodeClaim` names the slot and the machine is off
2. **Job Creation**: the slot reconciler takes the coordination lock and creates the Job from
   `spec.startupPodSpec`, in the same write that moves the claim `pending → booting`
3. **Report**: the slot reports `booting` while the startup is in flight
4. **Completion**: the node registers, passes the readiness gate, the claim moves to `ready`, and
   the slot reports `ready`

### Shutdown Process

1. **Trigger**: no live claim names the slot while its machine is observed up — a scale-down
   cancelled the claim, or the claim hit its deadline
2. **Node Drain**: cluster-autoscaler evicts the pods before it calls `DeleteNodes`; the
   provider observes and records the marker, it does not evict
3. **Job Creation**: the slot reconciler cordons the node and creates the Job from
   `spec.shutdownPodSpec`
4. **Report**: the slot reports `draining` while the machine is still observed up — a shutdown Job
   that exits 0 is not evidence that the machine is off
5. **Completion**: the machine is observed off, the scale-down claim ends `completed`, and the slot
   reports `off`

## Power Operation Examples

### Wake-on-LAN Startup

```yaml
startupPodSpec:
  image: alpine/socat:latest
  command: ["/bin/sh"]
  args:
  - "-c"
  - |
    echo -ne '\xFF\xFF\xFF\xFF\xFF\xFF\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55' | socat - UDP-DATAGRAM:255.255.255.255:9,broadcast
```

### IPMI Power Control

```yaml
startupPodSpec:
  image: gemtest/ipmitool:latest
  command: ["ipmitool"]
  args: ["-I", "lanplus", "-H", "worker-01-ipmi", "-U", "admin", "-P", "password", "power", "on"]
  volumes:
  - name: ipmi-credentials
    mountPath: /etc/ipmi
    secretName: ipmi-credentials

shutdownPodSpec:
  image: gemtest/ipmitool:latest
  command: ["ipmitool"]
  args: ["-I", "lanplus", "-H", "worker-01-ipmi", "-U", "admin", "-P", "password", "power", "soft"]
  volumes:
  - name: ipmi-credentials
    mountPath: /etc/ipmi
    secretName: ipmi-credentials
```

### SSH-based Shutdown

```yaml
shutdownPodSpec:
  image: alpine/ssh:latest
  command: ["ssh"]
  args: ["-i", "/root/.ssh/id_rsa", "-o", "StrictHostKeyChecking=no", "root@worker-01", "shutdown -h now"]
  volumes:
  - name: ssh-keys
    mountPath: /root/.ssh
    secretName: ssh-keys
```

## Usage

### Creating a Node

```bash
kubectl apply -f node.yaml
```

### Monitoring Node Status

```bash
# List all nodes
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system

# Get detailed node information
kubectl describe node.infra.homecluster.dev worker-01 -n homelab-autoscaler-system

# Watch node status changes
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -w
```

### Manual power operations

There is no field to declare. A manual scale-up is a claim, and a manual scale-down is the
cancellation of one — the same two objects the autoscaler's RPCs produce, with `requestedBy`
naming who did it:

```yaml
# power on: ask for the machine
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
    call: IncreaseSize      # the closest label for a hand-made request
    group: worker-nodes
```

```bash
# power on
kubectl apply -f worker-01-manual.yaml
kubectl get nodeclaim -n homelab-autoscaler-system -w

# power off: cancel it. A pending claim is deleted outright; a booting/ready claim runs the
# shutdown path through its finalizer, and ends when the machine is observed off.
kubectl delete nodeclaim worker-01-manual -n homelab-autoscaler-system
```

A second live claim on one slot is refused by the webhook, naming the slot. See
[the NodeClaim CRD](nodeclaim.md).

## Best Practices

### Security
- Use ServiceAccounts with minimal required permissions
- Store credentials in Secrets, not in pod specs
- Use SSH keys instead of passwords where possible
- Implement network policies to restrict power management pod access

### Reliability
- Test startup and shutdown procedures thoroughly
- Implement timeouts in power operation scripts
- Use idempotent operations (safe to retry)
- Monitor job completion and implement retry logic

### Labeling
- Always include `group` label for autoscaling group association
- Add descriptive labels for node capabilities and location
- Use consistent labeling across your infrastructure

### Pricing
- Set realistic hourly rates based on actual power consumption
- Include infrastructure costs (cooling, networking) in hourly rate
- Use pod rate to encourage efficient resource utilization

## Troubleshooting

### Common Issues

1. **A slot that never leaves `booting`**
   - The boot is the claim's: `kubectl get nodeclaim -n homelab-autoscaler-system -l
     infra.homecluster.dev/node=<slot>` and read `status.phase`, `status.reason`,
     `spec.deadlineAt`
   - Check startup job logs: `kubectl logs -n homelab-autoscaler-system job/<startup-job>`
   - Verify network connectivity to the target node, and the credentials it uses
   - If the gate is what is slow, `kubectl describe node.infra.homecluster.dev <slot>
     -n homelab-autoscaler-system` shows the `NodeReadinessTaints` condition: the key the
     slot is waiting for. Past the gate's 4m the claim fails with `ReadinessGateTimeout`

2. **Node not joining cluster**
   - Verify `kubernetesNodeName` matches actual node name
   - Check node configuration and kubelet status
   - Ensure network connectivity between node and cluster

3. **Power operations failing**
   - Check job logs for error messages
   - Verify credentials and network access
   - Test power commands manually

4. **A pool at `minSize: 0` never scales up**
   - With no slot registered, the provider can only report the capacity the slots declare:
     `kubectl get node.infra.homecluster.dev <slot> -o jsonpath='{.spec.nodeCapacity}'` returning
     nothing means the group's template has no cpu and no memory, and no pod with requests fits it.
     Declare the field, and declare every resource your workloads ask for
   - `kubectl describe node.infra.homecluster.dev <slot>` shows a `NodeCapacityMismatch` warning
     when the declaration and the booted machine disagree. That drift doesn't stop placement: it
     means the estimator is sizing the machine wrong, so you over-provision work the node can't take
     (the pod stays Pending after the machine boots) or leave a booted machine idle. The
     unplaceable case is the missing declaration above
   - A declaration that names no `pods` is not the problem it looks like: the provider defaults the
     pod ceiling to the kubelet's 110, so a cpu-and-memory declaration scales from zero fine

### Debugging Commands

```bash
# Check node controller logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager

# List power operation jobs
kubectl get jobs -n homelab-autoscaler-system

# Check job logs
kubectl logs -n homelab-autoscaler-system job/<job-name>

# Describe node for detailed status
kubectl describe node.infra.homecluster.dev <node-name> -n homelab-autoscaler-system
```

## Related Documentation

- [Group CRD](group.md) - Autoscaling group configuration
- [NodeClaim CRD](nodeclaim.md) - the request for a machine: the field this Node CRD no longer has
- [NodeClaim Design](../../design/nodeclaim.md) - why `spec.powerState` is gone
- [State Management](../../architecture/state.md) - the input law, and the report `status.phase` is
- [Architecture Overview](../../architecture/overview.md) - System design
