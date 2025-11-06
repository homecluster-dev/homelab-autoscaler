# Overview

Homelab Autoscaler adds dynamic scaling to physical homelab Kubernetes clusters. Instead of creating VMs like cloud providers, it powers physical machines on and off based on workload demands.

## Key Features

- **Physical machine power management** - Wake-on-LAN, IPMI, BMC, smart PDU support
- **Kubernetes-native integration** - Custom Resource Definitions and controllers
- **Cluster Autoscaler compatible** - Standard gRPC CloudProvider interface
- **Cost-aware scaling** - Minimize power consumption during low usage

## How It Works

### Scale Up Process
1. Workload demand exceeds available resources
2. Cluster Autoscaler requests more nodes via gRPC
3. System finds powered-off nodes in target group
4. Startup job powers on physical machine
5. Machine boots and joins Kubernetes cluster
6. Pending pods schedule on new node

### Scale Down Process
1. Node utilization drops below thresholds
2. Cluster Autoscaler identifies unneeded nodes
3. Pods are drained from target node
4. Shutdown job powers off physical machine
5. Machine removed from Kubernetes cluster

## Core Components

### Custom Resources

**Group CRD** - Defines autoscaling policies:
```yaml
apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: worker-nodes
spec:
  maxSize: 5
  nodeSelector:
    node-type: worker
  scaleDownUtilizationThreshold: 0.5
```

**Node CRD** - Represents physical machines:
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
- **Group Controller** - Manages autoscaling policies
- **Node Controller** - Handles power operations
- **Core Controller** - Bridges Kubernetes nodes with Node CRDs

### gRPC Server
Implements Cluster Autoscaler CloudProvider interface for node management.

## Power Management

Supported power control methods:
- Wake-on-LAN packets
- IPMI/BMC interfaces
- Smart PDUs
- Custom scripts via SSH
- Hardware-specific protocols

## Getting Started

Choose your installation method:
- **[Quick Start](quick-start.md)** - k3d setup for testing
- **[Installation Guide](installation.md)** - Helm deployment for production

Review known limitations in [Known Issues](../troubleshooting/known-issues.md) before deployment.

## Use Cases

- **Development clusters** - Scale down overnight, up during work hours
- **Batch processing** - Scale up for large jobs, down when idle
- **Cost optimization** - Reduce power consumption during low usage
- **Resource bursting** - Handle temporary workload spikes