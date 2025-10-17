# System Overview

## What is Homelab Autoscaler?

The Homelab Autoscaler brings cloud-like autoscaling capabilities to physical homelab environments. Instead of creating/destroying virtual machines, it powers physical nodes on/off based on workload demands.

## Core Concepts

### Physical Node Management
- **Power Control**: Manages physical machine power states (on/off)
- **Job-Based Operations**: Uses Kubernetes Jobs for startup/shutdown procedures
- **Health Monitoring**: Tracks node health and availability
- **Cost Awareness**: Includes power consumption and operational costs

### Autoscaling Groups
- **Group Policies**: Define scaling behavior and thresholds
- **Node Selection**: Use labels to group similar physical machines
- **Scaling Limits**: Set minimum and maximum node counts
- **Timing Controls**: Configure scale-down delays and timeouts

### Integration Points
- **Cluster Autoscaler**: Compatible with standard Kubernetes autoscaling
- **gRPC Interface**: Implements CloudProvider API for external integration
- **Custom Resources**: Kubernetes-native configuration via CRDs

## How It Works

### Intended Workflow (Currently Broken)

1. **Workload Demand**: Pods cannot be scheduled due to resource constraints
2. **Scale-Up Decision**: Cluster Autoscaler requests more nodes via gRPC
3. **Node Selection**: System finds powered-off nodes in the target group
4. **Power-On**: Startup job executes to power on physical machine
5. **Node Join**: Physical machine boots and joins Kubernetes cluster
6. **Pod Scheduling**: Pending pods are scheduled on new node

### Scale-Down Process

1. **Low Utilization**: Node utilization falls below thresholds
2. **Scale-Down Decision**: Cluster Autoscaler identifies unneeded nodes
3. **Node Drain**: Pods are evicted from the target node (⚠️ Not implemented)
4. **Power-Off**: Shutdown job executes to power off physical machine
5. **Node Leave**: Machine is removed from Kubernetes cluster

## System Components

### Custom Resource Definitions (CRDs)

#### Group CRD
Defines autoscaling policies:
```yaml
apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: worker-nodes
spec:
  name: worker-nodes
  maxSize: 5
  nodeSelector:
    node-type: worker
  scaleDownUtilizationThreshold: 0.5
  scaleDownUnneededTime: "10m"
```

#### Node CRD
Represents physical machines:
```yaml
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: worker-01
  labels:
    group: worker-nodes
spec:
  powerState: on
  kubernetesNodeName: worker-01
  startupPodSpec:
    image: homelab/power-manager:latest
    command: ["wake-on-lan", "00:11:22:33:44:55"]
```

### Controllers

- **Group Controller**: Manages Group CRDs (⚠️ Currently incomplete)
- **Node Controller**: Handles Node CRDs and power operations
- **Core Controller**: Bridges Kubernetes nodes with Node CRDs

### gRPC Server

Implements Cluster Autoscaler CloudProvider interface:
- `NodeGroups()` - List autoscaling groups
- `NodeGroupTargetSize()` - Get current group size (⚠️ Broken)
- `NodeGroupIncreaseSize()` - Scale up group
- `NodeGroupDecreaseTargetSize()` - Scale down group (⚠️ Broken)

## Power Management

### Startup Operations
Physical machines are powered on using Kubernetes Jobs that can:
- Send Wake-on-LAN packets
- Use IPMI/BMC interfaces
- Control smart PDUs
- Execute custom scripts

### Shutdown Operations
Graceful shutdown via Jobs that can:
- SSH to machines for clean shutdown
- Use IPMI power control
- Control smart PDUs
- Execute custom power-off procedures

### Health Monitoring
Node health is determined by:
- Physical power state
- Kubernetes node readiness
- Periodic health checks via CronJobs
- Job completion status

## Current Limitations

### Critical Issues
- **gRPC Logic Bugs**: Core scaling methods have incorrect logic
- **Missing Node Draining**: Pods not evicted before shutdown
- **Controller Race Conditions**: State management issues
- **Incomplete Group Controller**: Only sets basic status

### Missing Features
- Proper error handling and recovery
- Job monitoring and timeout handling
- Comprehensive logging and metrics
- Multi-tenancy support

## Use Cases

### Ideal Scenarios (When Fixed)
- **Development Clusters**: Scale down overnight, scale up during work hours
- **Batch Processing**: Scale up for large jobs, scale down when idle
- **Cost Optimization**: Minimize power consumption during low usage
- **Resource Bursting**: Handle temporary workload spikes

### Current Reality
- **Development/Testing Only**: System has critical bugs
- **Manual Operations**: Bypass autoscaler for reliable operations
- **Learning Platform**: Understand autoscaling concepts

## Getting Started

1. **Read Known Issues**: Understand current limitations
2. **Set Up Development Environment**: Follow [Development Setup](../development/setup.md)
3. **Deploy Sample Configuration**: Use provided examples
4. **Monitor System Behavior**: Watch logs and resource states
5. **Contribute Fixes**: Help resolve critical issues

## Architecture Benefits

### When Working Properly
- **Cost Efficiency**: Only run machines when needed
- **Energy Savings**: Reduce power consumption
- **Kubernetes Native**: Standard autoscaling integration
- **Flexible Power Management**: Support various power control methods
- **Scalable Design**: Handle multiple node groups and policies

### Current State
- **Educational Value**: Learn about Kubernetes operators and autoscaling
- **Foundation**: Good architectural base for future development
- **Integration Ready**: gRPC interface compatible with Cluster Autoscaler

## Next Steps

To make this system production-ready:

1. **Fix Critical Bugs**: Resolve gRPC logic errors
2. **Implement Node Draining**: Graceful pod eviction
3. **Add Error Handling**: Comprehensive error recovery
4. **Complete Controllers**: Finish Group controller implementation
5. **Add Monitoring**: Metrics and observability
6. **Security Hardening**: RBAC and secret management
7. **Testing**: Comprehensive test coverage

## Related Documentation

- [Architecture Overview](../architecture/overview.md) - Detailed system design
- [Known Issues](../troubleshooting/known-issues.md) - Current bugs and limitations
- [API Reference](../api-reference/crds/group.md) - CRD specifications
- [Development Setup](../development/setup.md) - Getting started with development