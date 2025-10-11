# Architecture Overview

> ⚠️ **WARNING**: This describes the intended architecture. See [Known Issues](../troubleshooting/known-issues.md) for current implementation problems.

## System Design

The Homelab Autoscaler implements a Kubernetes operator pattern with external gRPC integration to provide cluster autoscaling for physical nodes.

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

### 1. Custom Resource Definitions (CRDs)

#### Group CRD
Defines autoscaling policies for a collection of physical nodes:
- **Scaling thresholds** (CPU, GPU utilization)
- **Timing parameters** (scale-down delays, provision timeouts)
- **Node selection** (labels, maximum size)
- **Health status** (healthy, offline, unknown)

#### Node CRD
Represents individual physical machines:
- **Power state management** (desired vs actual)
- **Startup/shutdown jobs** (container specs for power operations)
- **Health monitoring** (progress tracking)
- **Pricing information** (hourly rates, pod costs)

### 2. Controllers

#### Group Controller (`internal/controller/infra/group_controller.go`)
**Intended Function**: Manages Group CRDs and autoscaling policies
**Current Status**: ⚠️ Incomplete - only sets "Loaded" condition

**Responsibilities**:
- Monitor Group CRD changes
- Update group health status based on node states
- Enforce autoscaling policies
- Manage group-level conditions

#### Node Controller (`internal/controller/infra/node_controller.go`)
**Function**: Manages Node CRDs and power state transitions
**Current Status**: ✅ Race conditions fixed, node draining implemented

**Responsibilities**:
- Monitor Node CRD changes
- Execute startup/shutdown jobs when power state changes
- Update node status and progress
- Handle job completion and failures

#### Core Controller (`internal/controller/core/node_controller.go`)
**Function**: Bridges Kubernetes nodes with Node CRDs
**Current Status**: ⚠️ State management issues

**Responsibilities**:
- Monitor Kubernetes node events
- Sync node health status with Node CRDs
- Handle node registration/deregistration
- Maintain consistency between K8s nodes and CRDs

### 3. gRPC Server (`internal/grpcserver/server.go`)

Implements the Cluster Autoscaler CloudProvider interface:

**Current Status**: ⚠️ Critical logic bugs

#### Key Methods:
- `NodeGroups()` - List all autoscaling groups
- `NodeGroupTargetSize()` - Get current target size ⚠️ **BROKEN**
- `NodeGroupIncreaseSize()` - Scale up by powering on nodes
- `NodeGroupDecreaseTargetSize()` - Scale down ⚠️ **BROKEN**
- `NodeGroupDeleteNodes()` - Power off specific nodes
- `NodeGroupForNode()` - Find group for a given node

## Data Flow

### Intended Flow (Not Working)

1. **Group Creation**:
   ```
   User creates Group CRD → Group Controller processes → Updates status
   ```

2. **Node Management**:
   ```
   Node CRD created → Node Controller → Startup job → K8s node joins
   ```

3. **Autoscaling Decision**:
   ```
   Cluster Autoscaler → gRPC call → GroupStore lookup → Scaling action
   ```

4. **Scale Up**:
   ```
   gRPC IncreaseSize → Find powered-off node → Set DesiredPowerState=on
   → Node Controller → Startup job → Node joins cluster
   ```

5. **Scale Down**:
   ```
   gRPC DeleteNodes → Set DesiredPowerState=off → Node Controller
   → Drain node → Shutdown job → Node leaves cluster
   ```


## Key Design Patterns

### Controller Pattern
Each CRD has a dedicated controller following the Kubernetes operator pattern:
- Watch for resource changes
- Reconcile desired vs actual state
- Update status fields
- Handle errors and retries

### Job-Based Power Management
Physical power operations are executed via Kubernetes Jobs:
- **Startup jobs**: Execute scripts/commands to power on machines
- **Shutdown jobs**: Gracefully power off machines
- **Isolation**: Power operations isolated in containers
- **Monitoring**: Job status indicates operation success/failure

### Health Monitoring
Node health determined by multiple factors:
- **Power state**: Physical machine on/off status
- **Kubernetes node**: Node registered and ready in cluster
- **Progress tracking**: Startup/shutdown operation status
- **CronJob health checks**: Periodic health verification

## Integration Points

### Cluster Autoscaler Integration
The gRPC server implements the CloudProvider interface, allowing standard Cluster Autoscaler to manage physical nodes as if they were cloud instances.

### Kubernetes API Integration
Controllers use controller-runtime to:
- Watch CRD changes
- Update resource status
- Create/manage Jobs for power operations
- Monitor Kubernetes node events

### External Power Management
Startup/shutdown jobs can integrate with any power management system:
- IPMI/BMC interfaces
- Wake-on-LAN
- Smart PDUs
- Custom scripts

## Configuration

### Namespace
**Current**: Hardcoded to `homelab-autoscaler-system`
**Issue**: No configuration option for custom namespaces

### gRPC Server
- **Default**: Enabled on port 50051
- **Configurable**: Via command-line flags
- **TLS**: Not currently implemented

### Health Check Frequency
- **Default**: Every 10 seconds
- **Configurable**: Via CronJob schedule format

## Security Considerations

### RBAC
Controllers require permissions for:
- Reading/writing Group and Node CRDs
- Creating/monitoring Jobs
- Reading Kubernetes nodes
- Creating CronJobs for health checks

### Job Security
Startup/shutdown jobs run with configured ServiceAccount:
- Should have minimal required permissions
- Secrets/ConfigMaps for credentials
- Network policies for isolation

## Scalability

### Current Limitations
- Single controller instance (no leader election for some controllers)
- In-memory GroupStore (not persistent)
- No horizontal scaling of gRPC server

### Design Considerations
- Controller-runtime provides leader election
- GroupStore could be replaced with persistent storage
- gRPC server is stateless (can be horizontally scaled)

## Related Documentation

- [Components](components.md) - Detailed component breakdown
- [Controllers](controllers.md) - Controller implementation details
- [gRPC Interface](grpc-interface.md) - API specification
- [Data Flow](data-flow.md) - Detailed data flow diagrams