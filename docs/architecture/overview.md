# Architecture

Homelab Autoscaler is a Kubernetes operator with gRPC integration for physical node autoscaling.

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
                       │ (Groups, Nodes)  │
                       └──────────────────┘
```

## Core Components

### Custom Resources

**Group CRD** - Defines autoscaling policies:
- Scaling thresholds
- Timing parameters
- Node selection
- Health status

**Node CRD** - Physical machine representation:
- Power state management
- Startup/shutdown jobs
- Health monitoring
- Pricing information

### Controllers

**Group Controller** (`internal/controller/infra/group_controller.go`)
- Manages Group CRDs and autoscaling policies
- Updates group health status
- Enforces scaling policies

**Node Controller** (`internal/controller/infra/node_controller.go`)
- Manages Node CRDs and power state transitions
- Finite State Machine architecture
- Coordination locking for race prevention
- Power operation integration (in development)
- Progress watchdog (`internal/progress`): reconciles a `status.progress` that names a machine the
  cluster does not have (no `corev1.Node`, no lifecycle Job in flight, no such Job finished inside the
  ~7 minute settle grace, no live lock). A slot desired `off` is settled to `off`/`shutdown`
  (`ProgressReconciled` Warning). A **claim** - `spec.desiredPowerState: on`, the autoscaler asked for
  the machine - is never settled: it is **re-armed** to `shutdown` with the claim left on, so the FSM
  can start it again (`ClaimRearmed`). The retries are bounded by **one startup-attempt counter**
  (3 per claim episode, counted by the `StartNode` transition and cleared when a boot passes the
  readiness gate) that both failure loops pay into - the watchdog's re-arm, and a startup Job that
  exits non-zero and rolls the slot back to `shutdown`, which the watchdog never sees. Past the bound
  the watchdog settles the slot and `StartNode` refuses to start it, with one
  `ClaimRearmLimitReached` Warning. A ready, claimed slot whose node is cordoned with nothing running
  on it is uncordoned (`CordonReconciled`). It runs at the top of every pass, before any transition

**Core Controller** (`internal/controller/core/node_controller.go`)
- Bridges Kubernetes nodes with Node CRDs
- Syncs node health status
- Handles node registration/deregistration
- Records what cluster-autoscaler decided about the machine (`internal/autoscaler`): the
  `DeletionCandidateOfClusterAutoscaler` / `ToBeDeletedByClusterAutoscaler` markers become a
  `AutoscalerDeletionCandidate` / `AutoscalerDeletionRequested` event on the Node CR, once per
  transition, and the node's taints and `spec.unschedulable` are logged so a cordon is attributable
  without raising the autoscaler's verbosity. The moment of the observation is in the condition's
  message (`observed at ...`), because `lastTransitionTime` does not move on a
  candidate→requested transition (both are `Status: True`) and would date the record early

### Scale-up readiness

The **scale-up** transition into `ready` (`JobCompleted` → Ready, the one that writes
`status.progress: ready`) completes when the node is `Ready` **and** free of every readiness taint the
pool declares. Pool workloads therefore do not need to tolerate `node.cilium.io/agent-not-ready`: the
controller waits it out, because declaring a slot ready before its datapath works makes the autoscaler
look at a Ready node its pending pod cannot land on and buy another machine.

The key belongs to the CNI, so it is configuration: `Group.spec.nodeReadinessTaints` is the list the
gate waits for, defaulted by the CRD to Cilium's `node.cilium.io/agent-not-ready` (stamped by Cilium
since v1.9; this chart deploys 1.16.x). An empty list gates on `Ready` alone. The keys are recorded on
the slot as the `NodeReadinessTaints` condition.

The two deadlines are separate: 90s for the node object to register, then 4m for the gate, counted
from the moment the object appears. On a timeout the controller **uncordons the node**, records a
`ReadinessGateTimeout` Warning on the slot, and the transition completes - a cordoned machine is
invisible to scale-down and would be paid for forever. See [State Management](state.md).

### gRPC Server (`internal/grpcserver/server.go`)

Implements Cluster Autoscaler CloudProvider interface:

**Implemented**:
- `NodeGroups()` - Lists autoscaling groups
- `NodeGroupTargetSize()` - Gets current target size
- `NodeGroupNodes()` - Lists nodes in group
- `NodeGroupForNode()` - Finds group for node

**In Development**:
- `NodeGroupIncreaseSize()` - Scale up nodes
- `NodeGroupDeleteNodes()` - Scale down nodes
- `NodeGroupDecreaseTargetSize()` - Target size reduction

**Target size**

The one definition of the target size of a group (`targetSizeOf`, reported by
`NodeGroupTargetSize`) counts a slot when it is **claimed** - `spec.desiredPowerState: on`, its
machine booting or running - and does not count a slot that is genuinely being torn down
(`spec.desiredPowerState: off` with a progress of `shuttingdown`/`shutdown`/`startingup`: a
machine still up on its way out counts, a cancelled startup doesn't).

`spec.desiredPowerState` is the request and `status.progress` is what the machine is doing, and
the autoscaler reads the request: `NodeGroupIncreaseSize` claims a slot with a spec write, and
the FSM moves the progress seconds later, after the machine has taken minutes to boot. A target
size read off the progress alone reports the slot the autoscaler just asked for as shut down, so
it concludes the group never grew and calls `IncreaseSize` again in the same loop. Every
minSize/maxSize check in the provider is judged against this same number.

## Data Flow

1. **Group Creation**: User creates Group CRD → Group Controller → Updates status
2. **Node Management**: Node CRD created → Node Controller → Startup job → K8s node joins
3. **Autoscaling**: Cluster Autoscaler → gRPC call → Kubernetes API → Scaling action
4. **Scale Up**: gRPC IncreaseSize → Find powered-off node → Set DesiredPowerState=on → Startup job
5. **Scale Down**: gRPC DeleteNodes → Set DesiredPowerState=off → Drain node → Shutdown job

## Design Patterns

### Controller Pattern
Each CRD has dedicated controller:
- Watch resource changes
- Reconcile desired vs actual state
- Update status fields
- Handle errors and retries

### Job-Based Power Management
Power operations via Kubernetes Jobs:
- **Startup jobs**: Execute power-on commands
- **Shutdown jobs**: Graceful power-off
- **Isolation**: Operations in containers
- **Monitoring**: Job status indicates success/failure

### Health Monitoring
Node health determined by:
- Power state (on/off)
- Kubernetes node readiness
- Progress tracking
- Periodic health checks

## Integration Points

### Cluster Autoscaler
Standard gRPC CloudProvider interface for integration.

### Kubernetes API
Controllers use controller-runtime for:
- Watching CRD changes
- Updating resource status
- Creating/managing Jobs
- Monitoring node events

### Webhook Validation
Admission webhooks provide:
- Validation - Ensure CRD requirements
- Mutation - Set default values
- Security - Prevent invalid configurations

## Configuration

### Namespace
- Hardcoded to `homelab-autoscaler-system`
- No custom namespace option yet

### gRPC Server
- Enabled on port 50051 by default
- Configurable via command-line flags
- TLS not implemented

### Health Checks
- Default: Every 10 seconds
- Configurable via CronJob schedule

## Security

### RBAC
Controllers need permissions for:
- Group/Node CRD operations
- Job creation/monitoring
- Kubernetes node reading
- CronJob creation

### Job Security
Startup/shutdown jobs:
- Minimal required permissions
- Secrets/ConfigMaps for credentials
- Network policy isolation

## Scalability
- Controller-runtime leader election
- gRPC server is stateless (horizontal scaling)

## Related Documentation
- [Installation](../getting-started/installation.md) - Helm deployment
- [Quick Start](../getting-started/quick-start.md) - k3d testing
- [FSM Architecture](state.md) - State management
- [NodeClaim Design](../design/nodeclaim.md) - Where scale-up intent goes in v2alpha1
- [Known Issues](../troubleshooting/known-issues.md) - Current limitations
- [API Reference](../api-reference/crds/group.md) - CRD specs