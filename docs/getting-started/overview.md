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

### Current Implementation

1. **Workload Demand**: Pods cannot be scheduled due to resource constraints
2. **Scale-Up Decision**: Cluster Autoscaler requests more nodes via gRPC
3. **Node Selection**: System finds powered-off nodes in the target group
4. **Power-On**: Startup job executes to power on physical machine
5. **Node Join**: Physical machine boots and joins Kubernetes cluster
6. **Pod Scheduling**: Pending pods are scheduled on new node

### Scale-Down Process

1. **Low Utilization**: Node utilization falls below thresholds
2. **Scale-Down Decision**: Cluster Autoscaler identifies unneeded nodes
3. **Node Drain**: Pods are evicted from the target node
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

- **Group Controller**: Manages Group CRDs and autoscaling policies
- **Node Controller**: Handles Node CRDs and power operations
- **Core Controller**: Bridges Kubernetes nodes with Node CRDs

### gRPC Server

Implements Cluster Autoscaler CloudProvider interface:
- `NodeGroups()` - List autoscaling groups
- `NodeGroupTargetSize()` - Get current group size
- `NodeGroupIncreaseSize()` - Scale up group
- `NodeGroupDecreaseTargetSize()` - Scale down group

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

## Known Limitations

### Current Constraints
- **Configuration Flexibility**: Limited customization options for timeouts and thresholds
- **Namespace Support**: Currently optimized for single-namespace deployment
- **Monitoring**: Basic metrics available, comprehensive observability in development
- **Multi-tenancy**: Single-tenant design, multi-tenant support planned

### Planned Enhancements
- Enhanced error handling and recovery mechanisms
- Advanced job monitoring and timeout handling
- Comprehensive logging and metrics collection
- Multi-tenancy support for enterprise deployments

## Use Cases

### Production Scenarios
- **Development Clusters**: Scale down overnight, scale up during work hours
- **Batch Processing**: Scale up for large jobs, scale down when idle
- **Cost Optimization**: Minimize power consumption during low usage
- **Resource Bursting**: Handle temporary workload spikes

### Current Capabilities
- **Production Deployment**: Stable autoscaling for physical infrastructure
- **Automated Operations**: Reliable power management and node lifecycle
- **Integration Ready**: Compatible with standard Kubernetes autoscaling tools

## Getting Started

1. **Choose Installation Method**:
   - **Quick Testing**: Follow [Quick Start Guide](quick-start.md) for k3d setup
   - **Production**: Use [Installation Guide](installation.md) for Helm deployment
2. **Review Known Issues**: Check [Known Issues](../troubleshooting/known-issues.md) for current limitations
3. **Set Up Development Environment**: Follow [Development Setup](../development/setup.md) for contributing
4. **Deploy Sample Configuration**: Use provided [examples](../api-reference/examples/k3d-group.md)
5. **Monitor System Behavior**: Watch logs and resource states

## Architecture Benefits

### Production Benefits
- **Cost Efficiency**: Only run machines when needed
- **Energy Savings**: Reduce power consumption
- **Kubernetes Native**: Standard autoscaling integration
- **Flexible Power Management**: Support various power control methods
- **Scalable Design**: Handle multiple node groups and policies

### Current Capabilities
- **Production Ready**: Stable operation for physical infrastructure autoscaling
- **Robust Architecture**: Well-tested foundation with comprehensive state management
- **Integration Complete**: Full compatibility with Cluster Autoscaler

## Next Steps

To enhance the system further:

1. **Advanced Configuration**: Expand customization options
2. **Enhanced Monitoring**: Additional metrics and observability features
3. **Multi-tenancy**: Support for multiple tenant deployments
4. **Performance Optimization**: Fine-tune scaling algorithms
5. **Security Enhancements**: Advanced RBAC and secret management
6. **Extended Testing**: Broader test coverage for edge cases

## Related Documentation

- [Quick Start Guide](quick-start.md) - Get started quickly with k3d
- [Installation Guide](installation.md) - Production Helm deployment
- [Architecture Overview](../architecture/overview.md) - Detailed system design
- [Known Issues](../troubleshooting/known-issues.md) - Current bugs and limitations
- [API Reference](../api-reference/crds/group.md) - CRD specifications
- [Examples](../api-reference/examples/) - Configuration examples
- [Development Setup](../development/setup.md) - Getting started with development