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
| `kubernetesNodeName` | `string` | Yes | Name of the corresponding Kubernetes node. One slot per machine: admission refuses a second slot that claims a node another slot already claims, and names the holder |
| `pricing` | `PricingSpec` | Yes | Cost information for the node |
| `nodeCapacity` | `corev1.ResourceList` | No | The shape of the machine this slot boots, as the kubelet reports it. Applied to the node template resource by resource, which is what lets a pool at `minSize: 0` scale back up; `pods` is defaulted for you |
| `nodeLabels` | `map[string]string` | No | Labels this machine must carry on its `corev1.Node`: applied on join - in the gate, before the node is uncordoned - re-asserted on drift, removed when the declaration drops the key, and carried into the node template so cluster-autoscaler can match a `nodeSelector` against this pool. A declared key reaches the template only when every slot of the group declares it with the same value |
| `nodeAnnotations` | `map[string]string` | No | The same, for annotations. Applied, re-asserted and removed by the same rules; annotations play no part in scheduling, and the group rule above decides what the template carries |
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
| `hourlyRate` | `string` | Yes | Cost per hour when running, as a decimal string (`"0.05"`). The CRD has no number type for it: `0.05` unquoted is refused at apply |
| `podRate` | `string` | Yes | Cost per pod scheduled on the node, as a decimal string (`"0.001"`) |

### NodeStatus

| Field | Type | Description |
|-------|------|-------------|
| `powerState` | `PowerState` | What the provider observes: `on` when the `corev1.Node` reports `NodeReady=True`, `off` otherwise. An observation, and one of the three inputs of every decision |
| `phase` | `NodePhase` | The **derived report** of the slot: `off`, `pending`, `booting`, `ready`, `draining`. Computed from (live claim, observed power, node usability) and read by nothing that decides. `status.progress` is removed with the model that needed it |
| `lastClaim` | `LastClaimSummary` | The digest of the slot's most recent claim: `{name, phase, attempt, call, group, reason, requestedAt, scaledDownAt, deadlineAt, finishedAt}`. It survives the claim's TTL collection, seeds the next claim's `status.attempt`, and answers "who asked, when, how many attempts, why did it end" from `kubectl describe`. The two times are the two moments of an episode: `requestedAt` is when the request that owned the slot was made, and `scaledDownAt` - the claim's deletion timestamp - is when a scale-down named it, which is the instant a shrink happened and not the age of the machine it took back ([the Group's `LASTSCALE`](group.md) reads it) |
| `conditions` | `[]metav1.Condition` | Standard Kubernetes conditions: the autoscaler markers (`AutoscalerDeletionCandidate`, `AutoscalerDeletionRequested`), `NodeReadinessTaints` (the gate this slot is waiting on), lifecycle conditions written by the transitions |
| `health` | `string` | Health status: `healthy`, `offline`, `unknown` |
| `lastStartupTime` | `*metav1.Time` | Last successful startup time |
| `lastShutdownTime` | `*metav1.Time` | Last successful shutdown time |
| `lastTransitionTime` | `*metav1.Time` | The instant `status.phase` last changed value. Written with the phase, by the one writer of it (`phase.Record`), so a resync that records nothing does not move it. The `SINCE` column |
| `previousPhase` | `NodePhase` | The phase the slot reported before this one, empty for the first report. "`off -> booting`" is a machine coming up, "`ready -> booting`" is a machine the pool lost, and the phase alone cannot tell them apart |
| `activationsTotal` | `int32` | Monotonic count of the slot's boots: every pass that recorded the report **entering** `booting` counted one. Never resets, so it is a fact about the machine, not about the clock |
| `activationsLast24H` | `int32` | How many of those boots are inside the last 24 hours: the length of `recentActivations`, recomputed on every write of the report and on the pass that watches the oldest boot leave the window. The `ON24H` column, the churn number. Defaults to `0`, so a slot written before the field existed renders a number and not `<none>` |
| `recentTransitions` | `[]NodeTransition` | The **display** ring of the slot's phase changes: at most 32, pruned to the last 24 hours, newest last. Each row is `{at, from, to, reason}`, the `reason` naming the inputs the report was derived from (live claim, observed power, node usability). The rows are appended by the pass that recorded each change, so their order is the order the writes landed: two writers racing inside a second can be listed out of chronological order, which is a display detail of a churn and not a fact any code reads. The 24 h count is deliberately **not** read through this cap |
| `recentActivations` | `[]metav1.Time` | The ledger the count is read from: the instant of every boot inside the window, oldest first, at most 512. A second list next to the ring because a bound a count is read through is a bound on the count - a slot that churns 40 times a day writes 80 ring rows and the ring keeps 32, so counting the ring flatlines `ON24H` at 16 exactly where the question starts to matter. A ledger row is one timestamp, so 512 of them cost less than 32 ring rows and the count is honest to 512 boots a day; past that the number is a floor and says so by sitting at the cap. Seeded from the ring on the first pass of a slot upgraded into this release, so an upgrade does not zero a churning pool's column |

#### NodeTransition

```go
type NodeTransition struct {
    At     *metav1.Time // when the report changed value
    From   NodePhase    // the phase it came from (empty for the first report)
    To     NodePhase    // the phase it went to
    Reason string       // the inputs it was derived from, e.g. "claim c1 ready, observed on, node usable"
}
```

The ring is bounded twice over, on purpose: 32 rows and 24 hours, whichever bites first. A day of a
churning slot is a list an operator can read and etcd can hold. The ring is the history; the boots of
the day are counted from `recentActivations`, which is pruned by the same cutoff on the same pass and
is bounded wider (512) because it holds nothing but the instants. It is written by the passes that
write the phase - the observation path, the lifecycle transition path, the seed of a
never-provisioned slot - and by nothing else: `phase.Record` is the single writer of every field
above, so a slot's history is the same list of changes whichever path got there first. Nothing in
the operator reads them, and
[the state doc](../../development/state.md#the-observability-fields) says why that is asserted by
a test and not by a promise.

### The Columns of `kubectl get`

```bash
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
```

```
NAME              POWERSTATE   PHASE     GROUP           ZONE   SINCE    ON24H
remote-worker-1   on           ready     remote-workers  fsn1   3h13m    3
remote-worker-2   on           booting   remote-workers  fsn1   4m       1
remote-worker-3   off          off       remote-workers  hel1   18h      2
remote-worker-4   on           draining  remote-workers         30m      0
```

| Column | Source |
| --- | --- |
| `NAME` | the built-in column of every Kubernetes table, not a printcolumn |
| `POWERSTATE` | `.status.powerState` |
| `PHASE` | `.status.phase` |
| `Group` | `.metadata.labels.group` |
| `Zone` | `.spec.nodeLabels['topology\.kubernetes\.io/zone']` - the key contains dots, so the JSONPath escapes them |
| `Since` | `.status.lastTransitionTime`, type `date` |
| `On24h` | `.status.activationsLast24H`, type `integer`, visible by default - the marker carries no `priority`, because `0` is the default and writing it out invites the reading that the column is optional |

The names and casing of `Powerstate` and `Phase` are the ones shipped before this work, and they are
treated as a published string: a printer column is a value nothing compiles, and the failure mode of
a typo is a silent column in front of an operator. The check that keeps the generated CRD and the
chart's copy identical is listed in
[What the tests keep honest](../../development/crd-sync.md#what-the-tests-keep-honest).

**There is no duration column, and there cannot be one.** A printer column is a JSONPath evaluated
on the object: it selects a value, it does not subtract one, and "up for 3h" is the difference
between `status.lastTransitionTime` and the time you ask the question. The API server prints what
the object holds, so the object holds the instant and the duration is computed at read time. The
columns and how to read them are in
[Read the Pool at a Glance](../../concepts/pool-at-a-glance.md), and the exact duration command, and
its checked output, are in the runbook:
[How long has this machine been up?](../../troubleshooting/common-issues.md#how-long-has-this-machine-been-up).

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

The table that computes it is in [State Management](../../development/state.md#the-input-law).
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

The operator-facing digest of these rules — the write order, the drift rule, the ownership record,
the reserved namespaces and the group's consensus rule — is
[Node labels](../../concepts/node-labels.md). This section is the reference form of the same rules.

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
  satisfy. That is a gate, not a best effort: for a slot that declares labels or annotations, a write
  that fails keeps the machine cordoned, records a `DeclaredNodeLabelsUnwritten` Warning on the slot,
  and is retried inside the readiness gate's deadline. The deadline still wins - at that point the
  node is uncordoned (`ReadinessGateTimeout`, naming what it was waiting on) and the slot reconciler
  keeps retrying, because a cordoned machine is one cluster-autoscaler's scale-down never removes.
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

Two rules are checked only at admission, and the writer repeats them for a slot object that predates
them (created while the webhook was down, a cluster restored from an older etcd): a key in a reserved
namespace is skipped with a Warning, and a slot may not claim the Kubernetes node of another slot.

Keys the controller refuses to write, with the reason in the admission message:

| Label keys | Owner, and why the refusal |
| --- | --- |
| `infra.homecluster.dev/*` | this operator's API domain: it stamps `group` and `node` on the node object, and keeps its records there (the keys a slot applied, the keys it refused) |
| `homelab-autoscaler.dev/*` | this project's domain: the coordination locks a lifecycle operation takes on a slot (`homelab-autoscaler.dev/operation-lock`, on the **Node CR**) live under it |
| `cluster-autoscaler.kubernetes.io/*` | cluster-autoscaler: it reads its per-machine controls off the node object (`cluster-autoscaler.kubernetes.io/scale-down-disabled`) |
| `node.kubernetes.io/*` | the kubelet and the node lifecycle controller: `role`, `os`, `arch`, `instance-type`, `exclude-home`, `drain-status` |
| `kubernetes.io/*` | the kubelet and the cloud integrations: `hostname`, `os`, `arch`, the version keys |
| `node-role.kubernetes.io/*` | the tooling that joins a machine: kubeadm, k3s, RKE2 |
| `topology.kubernetes.io/*` | the topology machinery - **except** `topology.kubernetes.io/zone` and `topology.kubernetes.io/region`, which are yours to declare |

| Annotation keys | Owner, and why the refusal |
| --- | --- |
| `infra.homecluster.dev/*` | this operator's API domain: the applied-keys and refused-keys records of the node object |
| `homelab-autoscaler.dev/*` | this project's domain: the coordination locks of a lifecycle operation, and the operator's own markers |
| `cluster-autoscaler.kubernetes.io/*` | cluster-autoscaler's per-machine controls, read off the node object. A slot that stamps `scale-down-disabled` removes its own machine from the pool's scale-down behind the pool's back; `spec.minSize` of the Group and the claim's lifecycle are the parts the model can see |
| `argocd.argoproj.io/*` | Argo CD's tracking and sync annotations (`tracking-id`, `sync-wave`, `sync-options`) |
| `kubectl.kubernetes.io/*` | kubectl's client-side records on a live object, first of them `last-applied-configuration`. (A cordon is `spec.unschedulable` and a drain adds a `node.kubernetes.io/*` *label*, so those are the label rules' business, not this one.) |

The two lists are not the same list, and that is deliberate. `node.kubernetes.io/`, `kubernetes.io/`
and `node-role.kubernetes.io/` are reserved for **labels** only: nothing stamps an annotation there
on its own, so a note under them is yours. `argocd.argoproj.io/` is reserved for **annotations**
only, for the same reason in reverse: Argo CD owns its annotations, and a *label* under that domain
(`argocd.argoproj.io/team: platform`) is a label like any other, and the label a `nodeSelector`
matches. The template path applies each list to the kind it writes, and
[the runbook entry](../../troubleshooting/common-issues.md) names the symptom when it does not.

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
  namespace: homelab-autoscaler-system
  labels:
    group: storage-nodes
spec:
  kubernetesNodeName: storage-01
  pricing:
    hourlyRate: "0.05"
    podRate: "0.001"
  nodeLabels:
    topology.kubernetes.io/zone: fsn1
  startupPodSpec:
    image: gemtest/ipmitool:latest
    command: ["ipmitool"]
    args: ["-I", "lanplus", "-H", "storage-01-ipmi", "power", "on"]
  shutdownPodSpec:
    image: gemtest/ipmitool:latest
    command: ["ipmitool"]
    args: ["-I", "lanplus", "-H", "storage-01-ipmi", "power", "soft"]
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
   against drift. On a scale-up the write happens **before** the node is uncordoned, and a machine
   whose labels could not be written stays cordoned until they land: the readiness gate refuses it,
   with a `DeclaredNodeLabelsUnwritten` Warning on the slot. The gate's deadline still wins - at that
   point the machine is uncordoned and the slot reconciler keeps retrying the write, because a
   cordoned machine is one cluster-autoscaler's scale-down never removes.
2. The provider puts the same labels into the group's node template, the object cluster-autoscaler
   simulates a placement on. A pool whose every slot is off gets a placeholder carrying the
   declaration, so the label is available before any machine has ever registered.
3. A pending pod with that `nodeSelector` now fits that group and no other, the estimator plans a
   scale-up on it, and `NodeGroupIncreaseSize` buys the machine.

Before the field, every slot of a pool reported the same template, so a pod selecting a label no
template carried was placed on no pool: the scale-up never happened, and the pod stayed Pending with
a message about a node affinity it never matched.

**A group answers with one template, and the template promises only what the group guarantees.**
`NodeGroupTemplateNodeInfo` is called per *group*, and one call returns one node, while the scale-up
it authorises is `IncreaseSize`, which buys whichever slot the provider has free (sorted by name).
So the template carries a declared key only when **every slot of the group declares it with the same
value**. Anything else is a promise the group cannot keep, and it is left out:

| the group's slots | the template |
| --- | --- |
| all declare `zone: fsn1` | carries `zone: fsn1` |
| one declares `zone: fsn1`, another `zone: hel1` | carries no `zone` at all |
| one declares `zone: fsn1`, another declares nothing | carries no `zone` at all |
| one slot, declaring `zone: fsn1` | carries `zone: fsn1` (one slot *is* the group) |
| nobody declares anything | carries nothing declared, exactly as before the field |

The omitted key is reported, per group, when it appears and when it changes (not on every scan):

```
INFO grpc-server NodeGroupTemplateNodeInfo: the slots of a group disagree on their declared node
metadata, the keys named are left out of the group's template ... {"nodeGroup": "storage-nodes",
"disagreements": "labels [topology.kubernetes.io/zone: \"fsn1\" on storage-01; \"hel1\" on storage-02]"}
```

A pod selecting a label the group disagrees on is then **not** placed on that group at all: the
estimator matches the selector against a template that does not carry it, plans nothing, and buys no
machine. That is the fail-closed direction. The alternative - simulating the representative slot's
label - spends the money and leaves the pod `Pending`: the boot runs, the machine registers without
the label, the estimator's plan does not fit, and the pool is one Ready machine larger with the work
still unscheduled. The fix is configuration: declare the key with the same value on every slot of the
group, or put the slots the scheduler must tell apart in **different groups**. A group whose slots
declare the same labels, a group of one slot, and a pool that declares nothing are all exact.

The disagreement is not an error and does not fail the call: the group stays exact and useful for
every pod that does not select the disputed key, and a call that returns an error makes
cluster-autoscaler skip the group for *every* pod.

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
   the autoscaler simulates a real machine. The `nodeLabels` and `nodeAnnotations` the group's slots
   declare are overlaid on the copy, declaration last, so the group answers with the label the pool
   is configured to carry and not with the value one live node happens to have. Only the keys every
   slot of the group declares with the same value are overlaid (see
   [Scheduling with labels](#scheduling-with-labels)), and `kubernetes.io/hostname` is stripped
   after the overlay, so no declaration can put the representative's hostname into the template.
2. If no slot has a node object (the whole pool is powered off), the provider synthesizes a
   placeholder from the Node CR. It carries the group label, the declared `nodeTaints` and the
   declared `nodeCapacity`, on top of the default pod ceiling, and the caller overlays the declared
   labels and annotations on it exactly as it does on a copy of a live node - one overlay, for both
   paths, so the two cannot state the rule differently. No machine size is invented: a pool that
   declared `nodeCapacity` is answering the question from Git, and a pool that declared nothing gets
   a template with no cpu, no memory and no disk, because the size of a machine that never registered
   can't be read.

One template per group. A key the slots of a group disagree on - two values, or a value some slots
never declare - is left out of the template, so the estimator never simulates a label the group
cannot guarantee, and the disagreement is reported in the provider's log. Slots the scheduler must
tell apart belong in different groups (see
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
    hourlyRate: "0.05"      # strings: 0.05 unquoted is refused at apply
    podRate: "0.001"
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
  powerState: "on"          # observed; quoted, because bare `on` is a YAML boolean
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
    reason: NodeReadinessTaints
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
  # Stamp the budget: the provider computes these two when it creates a claim, and nothing
  # fills them in for a hand-made one. A claim with no spec.deadlineAt never expires.
  requestedAt: "2026-03-04T09:38:15Z"
  deadlineAt: "2026-03-04T09:50:25Z"   # requestedAt + the 12m10s of one attempt
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
- [NodeClaim Design](../../development/nodeclaim.md) - why `spec.powerState` is gone
- [State Management](../../development/state.md) - the input law, and the report `status.phase` is
- [Architecture Overview](../../architecture/overview.md) - System design
