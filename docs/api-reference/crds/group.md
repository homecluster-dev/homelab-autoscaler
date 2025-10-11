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
| `maxSize` | `int` | Yes | Maximum number of nodes in the group |
| `nodeSelector` | `map[string]string` | Yes | Labels to select nodes for this group |
| `scaleDownUtilizationThreshold` | `float64` | Yes | CPU utilization threshold for scale-down (0.0-1.0) |
| `scaleDownGpuUtilizationThreshold` | `float64` | Yes | GPU utilization threshold for scale-down (0.0-1.0) |
| `scaleDownUnneededTime` | `*metav1.Duration` | Yes | Time a node must be unneeded before scale-down |
| `scaleDownUnreadyTime` | `*metav1.Duration` | Yes | Time an unready node waits before scale-down |
| `maxNodeProvisionTime` | `*metav1.Duration` | Yes | Maximum time to wait for node provisioning |
| `zeroOrMaxNodeScaling` | `bool` | Yes | Whether to scale to zero or max nodes only |
| `ignoreDaemonSetsUtilization` | `bool` | Yes | Ignore DaemonSet pods in utilization calculations |

### GroupStatus

| Field | Type | Description |
|-------|------|-------------|
| `health` | `string` | Overall health status: `healthy`, `offline`, `unknown` |
| `conditions` | `[]metav1.Condition` | Standard Kubernetes conditions |

#### Health Status Values

- **`healthy`**: All nodes in the group are healthy
- **`offline`**: At least one node in the group is offline
- **`unknown`**: Health status cannot be determined

#### Standard Conditions

- **`Available`**: The group is fully functional
- **`Progressing`**: The group is being created or updated
- **`Degraded`**: The group failed to reach or maintain desired state

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
status:
  health: healthy
  conditions:
  - type: Available
    status: "True"
    reason: GroupReady
    message: "Group is ready and managing nodes"
    lastTransitionTime: "2025-01-01T12:00:00Z"
```

## Usage

### Creating a Group

1. **Define node selection criteria** using `nodeSelector`
2. **Set scaling thresholds** for CPU and GPU utilization
3. **Configure timing parameters** for scale-down behavior
4. **Apply the Group resource** to your cluster

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

### Node Selection

Nodes are selected for a group based on the `nodeSelector` labels. Ensure your Node CRDs have matching labels:

```yaml
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: worker-01
  labels:
    node-type: worker    # Matches group nodeSelector
    zone: homelab        # Matches group nodeSelector
    group: worker-nodes  # Required for group association
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
- Set `maxSize` based on your physical infrastructure capacity
- Consider power consumption and cooling when setting maximum sizes
- Account for startup time in `maxNodeProvisionTime`

### Threshold Tuning
- Start with conservative thresholds (0.5-0.7) and adjust based on workload patterns
- Monitor actual utilization patterns before optimizing thresholds
- Consider workload characteristics (CPU vs memory intensive)

### Label Strategy
- Use consistent labeling across nodes and groups
- Include zone/rack information for placement awareness
- Add node capability labels (GPU, storage type, etc.)

## Troubleshooting

### Common Issues

1. **Nodes not joining group**
   - Verify `nodeSelector` labels match Node CRD labels
   - Check that `group` label is set on Node CRDs

2. **Scaling not working**
   - See [Known Issues](../../troubleshooting/known-issues.md) for critical bugs
   - Verify gRPC server is running and accessible
   - Check controller logs for errors

3. **Health status stuck**
   - Group controller may not be functioning (see known issues)
   - Check individual node health status
   - Verify CronJob health checks are running

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
- [Known Issues](../../troubleshooting/known-issues.md) - Current limitations