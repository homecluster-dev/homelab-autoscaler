# Node CRD Reference

The Node Custom Resource Definition (CRD) represents individual physical machines in the homelab autoscaler system.

## API Version
- **Group**: `infra.homecluster.dev`
- **Version**: `v1alpha1`
- **Kind**: `Node`

## Specification

### NodeSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `powerState` | `PowerState` | Yes | Desired power state: `on` or `off` |
| `startupPodSpec` | `MinimalPodSpec` | Yes | Pod specification for startup operations |
| `shutdownPodSpec` | `MinimalPodSpec` | Yes | Pod specification for shutdown operations |
| `kubernetesNodeName` | `string` | Yes | Name of the corresponding Kubernetes node |
| `pricing` | `PricingSpec` | Yes | Cost information for the node |

#### PowerState

```go
type PowerState string

const (
    PowerStateOn  PowerState = "on"
    PowerStateOff PowerState = "off"
)
```

#### MinimalPodSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `image` | `string` | Yes | Container image for the operation |
| `command` | `[]string` | No | Command to execute |
| `args` | `[]string` | No | Arguments for the command |
| `volumes` | `[]VolumeMountSpec` | No | Volume mounts for secrets/configmaps |
| `serviceAccount` | `*string` | No | ServiceAccount for the pod |

#### VolumeMountSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Volume name |
| `mountPath` | `string` | Yes | Path to mount the volume |
| `secretName` | `string` | No | Secret to mount (mutually exclusive with configMapName) |
| `configMapName` | `string` | No | ConfigMap to mount (mutually exclusive with secretName) |

#### PricingSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `hourlyRate` | `float64` | Yes | Cost per hour when running |
| `podRate` | `float64` | Yes | Cost per pod scheduled on the node |

### NodeStatus

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Standard Kubernetes conditions |
| `health` | `string` | Health status: `healthy`, `offline`, `unknown` |
| `powerState` | `PowerState` | Current power state: `on` or `off` |
| `progress` | `Progress` | Current operation progress |
| `lastStartupTime` | `*metav1.Time` | Last successful startup time |
| `lastShutdownTime` | `*metav1.Time` | Last successful shutdown time |

#### Progress

```go
type Progress string

const (
    ProgressShuttingDown Progress = "shuttingdown"
    ProgressShutdown     Progress = "shutdown"
    ProgressStartingUp   Progress = "startingup"
    ProgressReady        Progress = "ready"
)
```

## Example

```yaml
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: worker-01
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
    node-type: worker
    zone: homelab
spec:
  powerState: on
  kubernetesNodeName: worker-01
  startupPodSpec:
    image: homelab/power-manager:latest
    command: ["/bin/sh"]
    args: ["-c", "wake-on-lan 00:11:22:33:44:55"]
    volumes:
    - name: credentials
      mountPath: /etc/credentials
      secretName: power-credentials
    serviceAccount: power-manager
  shutdownPodSpec:
    image: homelab/power-manager:latest
    command: ["/bin/sh"]
    args: ["-c", "ssh root@worker-01 'shutdown -h now'"]
    volumes:
    - name: ssh-keys
      mountPath: /root/.ssh
      secretName: ssh-keys
    serviceAccount: power-manager
  pricing:
    hourlyRate: 0.05
    podRate: 0.001
status:
  health: healthy
  powerState: on
  progress: ready
  lastStartupTime: "2025-01-01T12:00:00Z"
  conditions:
  - type: Available
    status: "True"
    reason: NodeReady
    message: "Node is powered on and ready"
    lastTransitionTime: "2025-01-01T12:00:00Z"
```

## Power Management

### Startup Process

1. **Trigger**: `spec.powerState` set to `on`
2. **Job Creation**: Node controller creates Job from `startupPodSpec`
3. **Progress Tracking**: Status updated to `startingup`
4. **Completion**: Node joins cluster, status becomes `ready`

### Shutdown Process

1. **Trigger**: `spec.powerState` set to `off`
2. **Node Drain**: Pods evicted from Kubernetes node (⚠️ Not implemented)
3. **Job Creation**: Node controller creates Job from `shutdownPodSpec`
4. **Progress Tracking**: Status updated to `shuttingdown`
5. **Completion**: Node leaves cluster, status becomes `shutdown`

## Power Operation Examples

### Wake-on-LAN Startup

```yaml
startupPodSpec:
  image: alpine/socat:latest
  command: ["/bin/sh"]
  args:
  - "-c"
  - |
    echo -ne '\xFF\xFF\xFF\xFF\xFF\xFF\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55\x00\x11\x22\x33\x44\x55' | socat - UDP-DATAGRAM:255.255.255.255:9,broadcast
```

### IPMI Power Control

```yaml
startupPodSpec:
  image: homelab/ipmitool:latest
  command: ["ipmitool"]
  args: ["-I", "lanplus", "-H", "worker-01-ipmi", "-U", "admin", "-P", "password", "power", "on"]
  volumes:
  - name: ipmi-credentials
    mountPath: /etc/ipmi
    secretName: ipmi-credentials

shutdownPodSpec:
  image: homelab/ipmitool:latest
  command: ["ipmitool"]
  args: ["-I", "lanplus", "-H", "worker-01-ipmi", "-U", "admin", "-P", "password", "power", "soft"]
  volumes:
  - name: ipmi-credentials
    mountPath: /etc/ipmi
    secretName: ipmi-credentials
```

### SSH-based Shutdown

```yaml
shutdownPodSpec:
  image: alpine/ssh:latest
  command: ["ssh"]
  args: ["-i", "/root/.ssh/id_rsa", "-o", "StrictHostKeyChecking=no", "root@worker-01", "shutdown -h now"]
  volumes:
  - name: ssh-keys
    mountPath: /root/.ssh
    secretName: ssh-keys
```

## Usage

### Creating a Node

```bash
kubectl apply -f node.yaml
```

### Monitoring Node Status

```bash
# List all nodes
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system

# Get detailed node information
kubectl describe node.infra.homecluster.dev worker-01 -n homelab-autoscaler-system

# Watch node status changes
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -w
```

### Manual Power Operations

```bash
# Power on a node
kubectl patch node.infra.homecluster.dev worker-01 -n homelab-autoscaler-system \
  --type='merge' -p='{"spec":{"powerState":"on"}}'

# Power off a node
kubectl patch node.infra.homecluster.dev worker-01 -n homelab-autoscaler-system \
  --type='merge' -p='{"spec":{"powerState":"off"}}'
```

## Best Practices

### Security
- Use ServiceAccounts with minimal required permissions
- Store credentials in Secrets, not in pod specs
- Use SSH keys instead of passwords where possible
- Implement network policies to restrict power management pod access

### Reliability
- Test startup and shutdown procedures thoroughly
- Implement timeouts in power operation scripts
- Use idempotent operations (safe to retry)
- Monitor job completion and implement retry logic

### Labeling
- Always include `group` label for autoscaling group association
- Add descriptive labels for node capabilities and location
- Use consistent labeling across your infrastructure

### Pricing
- Set realistic hourly rates based on actual power consumption
- Include infrastructure costs (cooling, networking) in hourly rate
- Use pod rate to encourage efficient resource utilization

## Troubleshooting

### Common Issues

1. **Node stuck in starting up**
   - Check startup job logs: `kubectl logs job/node-startup-<hash>`
   - Verify network connectivity to target node
   - Check credentials and permissions

2. **Node not joining cluster**
   - Verify `kubernetesNodeName` matches actual node name
   - Check node configuration and kubelet status
   - Ensure network connectivity between node and cluster

3. **Power operations failing**
   - Check job logs for error messages
   - Verify credentials and network access
   - Test power commands manually

### Debugging Commands

```bash
# Check node controller logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager

# List power operation jobs
kubectl get jobs -n homelab-autoscaler-system

# Check job logs
kubectl logs -n homelab-autoscaler-system job/<job-name>

# Describe node for detailed status
kubectl describe node.infra.homecluster.dev <node-name> -n homelab-autoscaler-system
```

## Related Documentation

- [Group CRD](group.md) - Autoscaling group configuration
- [Architecture Overview](../../architecture/overview.md) - System design
- [Known Issues](../../troubleshooting/known-issues.md) - Current limitations