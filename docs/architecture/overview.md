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

**Core Controller** (`internal/controller/core/node_controller.go`)
- Bridges Kubernetes nodes with Node CRDs
- Syncs node health status
- Handles node registration/deregistration

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
- [Known Issues](../troubleshooting/known-issues.md) - Current limitations
- [API Reference](../api-reference/crds/group.md) - CRD specs