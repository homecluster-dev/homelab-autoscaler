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
| `minSize` | `int32` | No | Floor cluster-autoscaler must not scale the group below. Defaults to `0` |

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
  # Keep one node of the pool up; omit for a group that may scale to nothing.
  minSize: 1
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

### Minimum Size

`minSize` is the floor of the group. The gRPC cloud provider reports it to
cluster-autoscaler as the min size of the node group, and refuses its own scale-down
entry points (`NodeGroupDeleteNodes`, `NodeGroupDecreaseTargetSize`) when the request
would take the group below it, so a pool that has to keep a machine up sets `minSize: 1`
and the autoscaler stops reclaiming that node. A pool that used a keepalive pod to pin an
always-on slot above the utilization threshold can drop the pod once the group declares
the floor.

Two things to know:

- `0` is the default and matches the behaviour of every release before the field existed:
  the group may scale down to nothing.
- `maxSize` is **not** a field. The provider derives it by counting the Node CRs labelled
  `group: <group name>`. The CRD therefore cannot assert `minSize <= maxSize`; the gRPC
  boundary does, clamping a `minSize` above the number of slots to that count and refusing
  a scale up that would exceed it.

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
