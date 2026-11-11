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
| `powerState` | `PowerState` | Yes | Desired power state: `on` or `off` (see [FSM Architecture](../../architecture/state.md) for formal state management) |
| `startupPodSpec` | `MinimalPodSpec` | Yes | Pod specification for startup operations |
| `shutdownPodSpec` | `MinimalPodSpec` | Yes | Pod specification for shutdown operations |
| `kubernetesNodeName` | `string` | Yes | Name of the corresponding Kubernetes node |
| `pricing` | `PricingSpec` | Yes | Cost information for the node |
| `nodeTaints` | `[]corev1.Taint` | No | Taints this machine must carry, applied to the Kubernetes node and included in the node template |
| `taintsToRemove` | `[]corev1.Taint` | No | Taints stripped from the node template so the autoscaler ignores them |

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
| `priorityClassName` | `*string` | No | PriorityClass assigned to the job pod, e.g. `system-cluster-critical` |

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

### Node Taints

Declare the taints a machine must carry in `spec.nodeTaints`:

```yaml
spec:
  nodeTaints:
  - key: dedicated
    value: remote
    effect: NoSchedule
```

What the field gives you:

- cluster-autoscaler simulates the machine your pool boots. The provider puts the declared taints
  in the node template, so `dedicated=remote:NoSchedule` is there before the machine registers,
  and a pod that doesn't tolerate it is never placed on this group.
- The taints are in Git with the rest of the slot, and they survive node recreation. Nothing
  depends on a startup script running.

The controller applies them to the Kubernetes node as soon as the node object exists: the
scale-up path stamps them in the hook that uncordons the node, so the machine carries them before
it becomes schedulable, and every later reconcile re-applies them.

A tainting startup Job can't do that. It runs after the node joined, which is after the
autoscaler simulated a placement on an untainted template and after the node was schedulable, so
pods land on a machine that rejects them.

The window this closes is the one the controller drives. It can only patch a node that exists, so
a machine you booted by hand, or one that registered while the controller was down, can be Ready
and schedulable for a pass before the taint lands. Keep the tolerations of your own startup
tooling correct.

The field only adds, and it keeps adding:

- A taint you don't declare is never removed.
- A taint you declare and then remove by hand with `kubectl taint <node> <key>-` is put back on
  the next reconcile. The declaration is the intent, the `kubectl` run isn't.

Keys the controller won't write, because a Kubernetes component owns them and re-applies them on
its own sync loop:

| Keys | Owner |
| --- | --- |
| `node.kubernetes.io/not-ready`, `node.kubernetes.io/unreachable`, `node.kubernetes.io/unschedulable`, `node.kubernetes.io/out-of-service` | node lifecycle controller |
| `node.kubernetes.io/memory-pressure`, `node.kubernetes.io/disk-pressure`, `node.kubernetes.io/pid-pressure`, `node.kubernetes.io/network-unavailable` | kubelet |
| `ToBeDeletedByClusterAutoscaler`, `DeletionCandidateOfClusterAutoscaler` | cluster-autoscaler |

A declaration on one of those keys is dropped, and the drop is visible: a Warning event on the
Node CR (`kubectl describe node.infra.homecluster.dev <name>`, reason
`SkippedProtectedTaintKey`) and a `V(1)` log line. Nothing is dropped silently.

The `node.kubernetes.io/` prefix is a namespace, not an ownership claim. The
`node.kubernetes.io/role=agent` taint your k3d agents carry is yours to declare, and the
controller applies it.

`spec.taintsToRemove` is the opposite field and is unrelated: it filters taints out of the node
template so the autoscaler ignores taints your startup process leaves behind. It never touches
the live node. A key listed in both fields ends up on the template, because `nodeTaints` is what
the machine boots with.

If you migrate a pool from a tainting startup Job, drop the `kubectl taint` step from the Job and
set `nodeTaints` on the Node CRs. The template carries the taint as soon as the CR is applied;
the node gets it the next time the controller touches it, which on a scale-up is before the node
is uncordoned.

### Node Template

The provider reports one machine shape per group, built from a slot of that group:

1. A slot whose Kubernetes node object exists, a Ready one preferred. That node object is the
   only source of `status.allocatable`, `spec.taints` and the labels a pod needs to schedule, so
   the autoscaler simulates a real machine.
2. If no slot has a node object (the whole pool is powered off), the provider synthesizes a
   placeholder from the Node CR. It carries the group label and the declared `nodeTaints`, and no
   capacity, because the size of a machine that never registered can't be read and shouldn't be
   invented.

Because the template is copied from a live node, the autoscaler's own taints are filtered out of
it: `ToBeDeletedByClusterAutoscaler` and `DeletionCandidateOfClusterAutoscaler` on the
representative node describe that machine's scale-down state, not the shape of a new one, and
copying them into the group's template leaves the estimator with a taint no pod tolerates. The
transient `node.kubernetes.io/unschedulable`, `not-ready` and `unreachable` taints are filtered
out the same way. Keys the pool declares, `node.kubernetes.io/role` included, are not.

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
  nodeTaints:
  - key: dedicated
    value: remote
    effect: NoSchedule
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
  image: gemtest/ipmitool:latest
  command: ["ipmitool"]
  args: ["-I", "lanplus", "-H", "worker-01-ipmi", "-U", "admin", "-P", "password", "power", "on"]
  volumes:
  - name: ipmi-credentials
    mountPath: /etc/ipmi
    secretName: ipmi-credentials

shutdownPodSpec:
  image: gemtest/ipmitool:latest
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
