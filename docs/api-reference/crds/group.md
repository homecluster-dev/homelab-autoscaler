# Group CRD Reference

The Group Custom Resource Definition (CRD) defines autoscaling policies for collections of physical nodes.

## API Version
- **Group**: `infra.homecluster.dev`
- **Version**: `v1alpha1`
- **Kind**: `Group`

## Specification

### GroupSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Unique identifier for the group |
| `scaleDownUtilizationThreshold` | `float64` | Yes | CPU utilization threshold for scale-down (0.0-1.0) |
| `scaleDownGpuUtilizationThreshold` | `float64` | Yes | GPU utilization threshold for scale-down (0.0-1.0) |
| `scaleDownUnneededTime` | `*metav1.Duration` | Yes | Time a node must be unneeded before scale-down |
| `scaleDownUnreadyTime` | `*metav1.Duration` | Yes | Time an unready node waits before scale-down |
| `maxNodeProvisionTime` | `*metav1.Duration` | Yes | Maximum time to wait for node provisioning |
| `zeroOrMaxNodeScaling` | `bool` | Yes | Whether to scale to zero or max nodes only |
| `ignoreDaemonSetsUtilization` | `bool` | Yes | Ignore DaemonSet pods in utilization calculations |

## Example

```yaml
apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: worker-nodes
  namespace: homelab-autoscaler-system
spec:
  name: worker-nodes
  maxSize: 5
  nodeSelector:
    node-type: worker
    zone: homelab
  scaleDownUtilizationThreshold: 0.5
  scaleDownGpuUtilizationThreshold: 0.5
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

1. **Nodes not joining group**
   - Check that `group` label is set on Node CRDs

2. **Scaling not working**
   - Verify gRPC server is running and accessible
   - Check controller logs for errors

### Debugging Commands

```bash
# Check group controller logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager

# List nodes in group
kubectl get nodes -l group=worker-nodes

# Check node CRDs
kubectl get nodes.infra.homecluster.dev -l group=worker-nodes

# Verify gRPC server status
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc 50051:50051
```

## Related Documentation

- [Node CRD](node.md) - Individual node management
- [Architecture Overview](../../architecture/overview.md) - System design
