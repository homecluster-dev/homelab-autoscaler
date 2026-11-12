# Architecture

Homelab Autoscaler is a Kubernetes operator with gRPC integration for physical node autoscaling.
Scale-up intent is an object: `Node` is what a slot *is*, `NodeClaim` is one request for one
machine, and `Node.status` is what the cluster observes about it.

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────┐
│ Cluster         │    │ Homelab          │    │ Physical        │
│ Autoscaler      │◄──►│ Autoscaler       │◄──►│ Nodes           │
│                 │gRPC│                  │Jobs│                 │
└─────────────────┘    └──────────────────┘    └─────────────────┘
                              │
                              ▼
                       ┌──────────────────┐
                       │ Kubernetes API   │
                       │ (Groups, Nodes,  │
                       │  NodeClaims)     │
                       └──────────────────┘
```

## Core Components

### Custom Resources

**Group CRD** (`infra.homecluster.dev/v2alpha1`) - autoscaling policy for a pool:
- scaling thresholds and timing (`minSize`, `maxNodeProvisionTime`, scale-down windows)
- **no node selection, and no `maxSize`**: a pool is the set of `Node` CRs labelled
  `group: <name>`, and the labels a machine carries are declared on the slot
  (`spec.nodeLabels`). The pool's ceiling is its slot count
- the readiness taints a machine must be free of before it counts as capacity
- the pool's report: `slots`, `on`, `off`, `health`, and the last scale

**Node CRD** (`infra.homecluster.dev/v2alpha1`) - a slot: the configuration of one machine, and
nothing about what anyone wants it to do next:
- `spec.startupPodSpec` / `spec.shutdownPodSpec`, `spec.kubernetesNodeName`, `spec.nodeTaints`,
  `spec.nodeCapacity`, `spec.pricing`
- `status.powerState` (observed), `status.phase` (derived report), `status.lastClaim` (the digest
  of the last request)
- Git is the only writer of the spec, so an apply can never contradict a scale-up

**NodeClaim CRD** (`infra.homecluster.dev/v2alpha1`) - one request for one machine:
- `spec.slotRef`, `spec.requestedBy`, `spec.request` (`call` / `delta` / `group` / `nodes`),
  `spec.deadlineAt`, `spec.maxNodeProvisionTime`, `spec.ttlSecondsAfterFinished`
- `status.phase` (`pending` / `booting` / `ready` / `failed` / `completed`), `status.attempt`,
  `status.reason`, `status.finishedAt`, conditions
- created by the gRPC provider, owned by the slot, collected after its TTL

### Controllers

**Group Controller** (`internal/controller/infra/group_controller.go`)
- Reads the policy object, publishes it to the provider, and keeps the `Loaded` condition
- Counts the pool in one pass over its slots and writes the report `kubectl get groups` prints:
  `slots`, `on`, `off`, and `health` derived from the slots' reports (`offline` when no slot has
  ever reported, `healthy` once one has)
- Materializes the last scale (`lastScaleAt` / `lastScaleAction`) off each slot's `status.lastClaim`
  digest, so the pool's history outlives the Events and the claim's TTL
- Decides nothing: no reconciler reads these counts, and the provider's arithmetic counts live
  claims and slots directly

**Slot reconciler** (`internal/controller/infra/node_controller.go`, `lifecycle.go`, `claims.go`)
- Reads the live claim of the slot, the observed power and the node's usability, and fires the one
  lifecycle transition that input asks for
- Finite State Machine architecture (`internal/fsm`), seeded from that triple and never from a
  record
- Coordination locking for race prevention (kept as a belt over the claim, see
  [State Management](state.md))
- Owns the claim moves a transition is the evidence for (`pending → booting` with the startup Job,
  `booting → ready` with the readiness gate, `→ failed` with a Job failure, a timeout, a stuck
  transition)
- ProviderID, declared taints, declared node labels and annotations (applied on join, re-asserted
  on drift, removed when the declaration drops the key), capacity drift: the slot-level plumbing

**Claim controller** (`internal/controller/infra/nodeclaim_controller.go`)
- Seeds `status.attempt` from the slot's digest, and enforces the consecutive-attempt bound
- Enforces one live claim per slot where admission cannot see it (`reason: Superseded`)
- Stamps the finalizer with the boot, runs the scale-down path, and ends the claim when the
  machine is observed off
- Fails an overdue claim (`reason: DeadlineExceeded`) — the deadline is the heal the v1alpha1
  watchdog performed
- Stamps `Node.status.lastClaim`, and collects a terminal claim after its TTL

**Observation reconciler** (`internal/controller/core/node_controller.go`)
- Bridges Kubernetes nodes with Node CRDs: `status.powerState`, the derived `status.phase`, health
- Records what cluster-autoscaler decided about the machine (`internal/autoscaler`): the
  `DeletionCandidateOfClusterAutoscaler` / `ToBeDeletedByClusterAutoscaler` markers become an
  `AutoscalerDeletionCandidate` / `AutoscalerDeletionRequested` event on the Node CR, once per
  transition, and the node's taints and `spec.unschedulable` are logged so a cordon is attributable
  without raising the autoscaler's verbosity. The moment of the observation is in the condition's
  message (`observed at ...`), because `lastTransitionTime` does not move on a
  candidate→requested transition (both are `Status: True`) and would date the record early

**Node slot controller** (`internal/controller/core/node_slot_controller.go`)
- Reports the shape of slots that have never had a machine, so a pool's arithmetic and an
  operator's `kubectl get` describe them. "Is this slot free" is a claim question
  (`internal/claim`), so the v1alpha1 job of seeding a status so the target-size arithmetic
  excludes a never-provisioned slot is gone

### Scale-up readiness

The **claim** reaches `ready` when the node is `Ready` **and** free of every readiness taint the
pool declares. Pool workloads therefore do not need to tolerate `node.cilium.io/agent-not-ready`:
the controller waits it out, because declaring capacity before its datapath works makes the
autoscaler look at a Ready node its pending pod cannot land on and buy another machine.

The key belongs to the CNI, so it is configuration: `Group.spec.nodeReadinessTaints` is the list the
gate waits for, defaulted by the CRD to Cilium's `node.cilium.io/agent-not-ready` (stamped by Cilium
since v1.9; this chart deploys 1.16.x). An empty list gates on `Ready` alone. The keys are recorded on
the slot as the `NodeReadinessTaints` condition.

The two deadlines are separate: 90s for the node object to register, then 4m for the gate, counted
from the moment the object appears. On a timeout the controller **uncordons the node** (a cordoned
machine is invisible to scale-down and would be paid for forever), records a `ReadinessGateTimeout`
Warning on the slot, and moves the claim to **`failed`** with that reason: the input law then powers
the machine off, the slot becomes free, and the next claim on it is attempt N+1. See
[State Management](state.md).

### gRPC Server (`internal/grpcserver/server.go`)

Implements the Cluster Autoscaler CloudProvider interface:

- `NodeGroups()`, `NodeGroupForNode()`, `NodeGroupNodes()`, `NodeGroupTemplateNodeInfo()`,
  `NodeGroupGetOptions()`, pricing
- `NodeGroupTargetSize()` - the count of the group's **live claims**
- `NodeGroupIncreaseSize(delta)` - creates `delta` claims on `delta` free slots, each carrying the
  attribution: `requestedBy`, `request.call`, `request.delta`, `request.group`, and a deadline
- `NodeGroupDeleteNodes(nodes)` - records the request on the claim of each named slot and cancels
  it (a `pending` claim is deleted; a `booting`/`ready` claim is deleted through its finalizer,
  which runs the shutdown)
- `NodeGroupDecreaseTargetSize(delta)` - cancels `pending` claims, never a `booting` one, and never
  below the group's `minSize`

No handler writes a field of a `Node` spec. That is the point: `spec.powerState` was the request,
the lock, the arithmetic and the history at once, and a GitOps apply with `selfHeal` re-applied it
over the autoscaler's write on every sync.

**Target size**

`targetSizeOf` is the count of live claims: `pending` + `booting` + `ready`, excluding a claim a
scale-down has marked. A `failed` claim stops being counted the moment it is recorded, which
removes the state "the autoscaler is paying for a machine that never arrived, forever". No progress
value, and no slot spec field, appears in the arithmetic, and every minSize/maxSize check in the
provider is judged against this same number.

**Instances**

The `NodeGroupNodes` contract is unchanged: a slot with a `corev1.Node` is `running` while a live
claim wants it and `deleting` when nothing wants it any more, a slot a live claim names that has
not joined is `creating`, and a slot no claim names with no machine behind it is not reported. The
word "wanted" moved from a spec field to the claim. The node template
(`NodeGroupTemplateNodeInfo`) reads `Node.spec` only, which is what makes it correct in v2: the
config object is a config object.

## Data Flow

1. **Declaration**: `kubectl apply` of a `Node` → the slot reconciler stamps providerID, taints,
   the Group ownerRef, and checks the declared capacity
2. **Scale up**: Cluster Autoscaler → `NodeGroupIncreaseSize` → a `NodeClaim` on a free slot →
   the slot reconciler takes the lock, creates the startup Job, moves the claim `pending → booting`
   → the machine boots, the node joins → the readiness gate moves the claim `booting → ready`
3. **Serving**: a ready claim plus an observed-up, usable node is capacity; the slot reports
   `ready`
4. **Scale down**: Cluster Autoscaler drains, then `NodeGroupDeleteNodes` → the request is
   recorded on the claim and the claim is cancelled → the slot reconciler cordons and runs the
   shutdown Job → the machine is observed off → the claim ends `completed` and is collected, and
   the slot keeps the digest
5. **A stalled attempt**: the claim passes `spec.deadlineAt` → `failed` (`DeadlineExceeded`) → the
   machine is force-shut-down → the slot is free, and the next claim is attempt N+1

## Design Patterns

### Controller Pattern
Each CRD has one owner:
- the claim controller owns the claim and its lifecycle Jobs' outcome
- the slot reconciler owns the lifecycle operation on the slot
- the observation reconciler owns the report of what the cluster sees

### Job-Based Power Management
Power operations via Kubernetes Jobs:
- **Startup jobs**: Execute power-on commands, created by the `StartNode` transition
- **Shutdown jobs**: Graceful power-off, created by the `ShutdownNode` transition
- **Isolation**: Operations in containers
- **Monitoring**: the Job monitor of the operation fires the transition, and the claim's phase
  records its outcome

### Health Monitoring
Node health determined by:
- observed power (`status.powerState`, from the `corev1.Node` Ready condition)
- the derived report (`status.phase`)
- the last episode (`status.lastClaim`)
- periodic resyncs (5 minutes for a stable slot, at the deadline for a claim)

## Integration Points

### Cluster Autoscaler
Standard gRPC CloudProvider interface. The provider answers its four mutation calls with claims.

### Kubernetes API
Controllers use controller-runtime for watching CRDs, writing status, creating and managing Jobs,
and monitoring node events. The claim controller's field index is `spec.slotRef.name`.

### Webhook Validation
- `Node`: the `group` label must reference a Group CR; a slot whose `corev1.Node` has not joined is
  admitted with a warning, never denied
- `NodeClaim`: the slot must exist, and **one live claim per slot**. The message names the slot and
  the claim that owns it. A terminal status write and a delete are never refused

## Configuration

### Namespace
- Defaults to `homelab-autoscaler-system`, overridable with `HOMELAB_AUTOSCALER_NAMESPACE`

### gRPC Server
- Enabled on port 50051 by default, configurable via command-line flags

### Health Checks
- The manager's own liveness and readiness probes, on `:8081` (`/healthz`, `/readyz`), defined in
  the chart (`controllerManager.container.livenessProbe` / `.readinessProbe`). There is no scheduled
  health job: nothing runs on a CronJob
- A pool's health is a field on the objects, not a probe result: `kubectl get groups` answers it from
  the slots' reports, and a slot's health is on the slot

## Security

### RBAC
Controllers need permissions for the Group, Node and **NodeClaim** CRDs (including
`nodeclaims/status` and `nodeclaims/finalizers`), Jobs, Kubernetes nodes, and events.

### Job Security
Startup/shutdown jobs run with minimal permissions, Secrets/ConfigMaps for credentials, and
network policy isolation.

## Scalability
- controller-runtime leader election
- the gRPC server is stateless; the claims make the scale-up history an object, not a field

## Related Documentation
- [Installation](../getting-started/installation.md) - Helm deployment
- [Quick Start](../getting-started/quick-start.md) - k3d testing
- [State Management](state.md) - the input law, the claim phases, the lock
- [NodeClaim CRD](../api-reference/crds/nodeclaim.md) - the request object
- [NodeClaim Design](../design/nodeclaim.md) - why this model
- [Known Issues](../troubleshooting/known-issues.md) - Current limitations
