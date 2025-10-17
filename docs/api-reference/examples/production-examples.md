# Production Examples

This page contains production-ready examples for homelab-autoscaler, demonstrating real-world configurations for physical nodes with various power management methods.

## Production Group Configuration

Conservative scaling settings suitable for production environments:

```yaml
# Production Group with conservative scaling settings
apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: worker-nodes
  namespace: homelab-autoscaler-system
spec:
  # Conservative maximum size for production
  maxSize: 5
  
  # Scale down when utilization is below 30% (conservative)
  scaleDownUtilizationThreshold: 0.3
  
  # Wait 30 minutes before scaling down (production safety)
  scaleDownUnneededTime: "30m"
  
  # Wait 20 minutes before scaling down unready nodes
  scaleDownUnreadyTime: "20m"
  
  # Allow up to 15 minutes for node provisioning
  maxNodeProvisionTime: "15m"
  
  # Ignore DaemonSet pods when calculating utilization
  ignoreDaemonSetsUtilization: true
  
  # Allow partial scaling
  zeroOrMaxNodeScaling: false
  
  # GPU utilization threshold (if using GPU nodes)
  scaleDownGpuUtilizationThreshold: "20"
```

## Wake-on-LAN Node

Physical node with Wake-on-LAN startup and SSH shutdown:

```yaml
# Physical node with Wake-on-LAN startup
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: worker-01
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
    node-type: worker
    zone: rack-1
spec:
  kubernetesNodeName: worker-01.homelab.local
  powerState: "off"
  
  pricing:
    hourlyRate: "2.50"  # Actual power cost calculation
    podRate: "0.01"     # Small per-pod cost
  
  # Wake-on-LAN startup using network broadcast
  startupPodSpec:
    image: homelab/wake-on-lan:latest
    command: ["wakeonlan"]
    args: ["00:11:22:33:44:55"]
    env:
    - name: TARGET_HOST
      value: "worker-01.homelab.local"
    - name: BROADCAST_IP
      value: "192.168.1.255"
    resources:
      limits:
        cpu: 100m
        memory: 64Mi
      requests:
        cpu: 10m
        memory: 32Mi
  
  # SSH-based graceful shutdown
  shutdownPodSpec:
    image: homelab/ssh-shutdown:latest
    command: ["ssh-shutdown"]
    args: ["worker-01.homelab.local"]
    env:
    - name: SSH_USER
      value: "automation"
    - name: SSH_KEY_PATH
      value: "/etc/ssh-keys/id_rsa"
    volumeMounts:
    - name: ssh-keys
      mountPath: /etc/ssh-keys
      readOnly: true
    resources:
      limits:
        cpu: 100m
        memory: 64Mi
      requests:
        cpu: 10m
        memory: 32Mi
    volumes:
    - name: ssh-keys
      secret:
        secretName: ssh-automation-keys
        defaultMode: 0600
```

## IPMI Node

Physical node with IPMI power management:

```yaml
# Physical node with IPMI power management
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: worker-02
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
    node-type: worker
    zone: rack-1
spec:
  kubernetesNodeName: worker-02.homelab.local
  powerState: "off"
  
  pricing:
    hourlyRate: "3.00"  # Higher power consumption
    podRate: "0.01"
  
  # IPMI power on
  startupPodSpec:
    image: gemtest/ipmi-tool:latest
    command: ["ipmitool"]
    args: [
      "-I", "lanplus",
      "-H", "worker-02-ipmi.homelab.local",
      "-U", "automation",
      "-P", "$(IPMI_PASSWORD)",
      "power", "on"
    ]
    env:
    - name: IPMI_PASSWORD
      valueFrom:
        secretKeyRef:
          name: ipmi-credentials
          key: password
    resources:
      limits:
        cpu: 100m
        memory: 64Mi
      requests:
        cpu: 10m
        memory: 32Mi
  
  # IPMI graceful shutdown
  shutdownPodSpec:
    image: gemtest/ipmi-tool:latest
    command: ["ipmitool"]
    args: [
      "-I", "lanplus",
      "-H", "worker-02-ipmi.homelab.local",
      "-U", "automation",
      "-P", "$(IPMI_PASSWORD)",
      "power", "soft"
    ]
    env:
    - name: IPMI_PASSWORD
      valueFrom:
        secretKeyRef:
          name: ipmi-credentials
          key: password
    resources:
      limits:
        cpu: 100m
        memory: 64Mi
      requests:
        cpu: 10m
        memory: 32Mi
```

## GPU Worker Node

GPU worker node with smart PDU power control:

```yaml
# GPU worker node with specialized configuration
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: gpu-worker-01
  namespace: homelab-autoscaler-system
  labels:
    group: gpu-workers
    node-type: gpu-worker
    gpu-type: nvidia-rtx-4090
spec:
  kubernetesNodeName: gpu-worker-01.homelab.local
  powerState: "off"
  
  pricing:
    hourlyRate: "5.00"  # High power consumption for GPU
    podRate: "0.05"     # Higher per-pod cost for GPU usage
  
  # Smart PDU power control
  startupPodSpec:
    image: homelab/pdu-control:latest
    command: ["pdu-control"]
    args: ["--action", "on", "--outlet", "3", "--host", "pdu-01.homelab.local"]
    env:
    - name: PDU_USERNAME
      valueFrom:
        secretKeyRef:
          name: pdu-credentials
          key: username
    - name: PDU_PASSWORD
      valueFrom:
        secretKeyRef:
          name: pdu-credentials
          key: password
    resources:
      limits:
        cpu: 100m
        memory: 64Mi
      requests:
        cpu: 10m
        memory: 32Mi
  
  shutdownPodSpec:
    image: homelab/pdu-control:latest
    command: ["pdu-control"]
    args: ["--action", "off", "--outlet", "3", "--host", "pdu-01.homelab.local", "--delay", "60"]
    env:
    - name: PDU_USERNAME
      valueFrom:
        secretKeyRef:
          name: pdu-credentials
          key: username
    - name: PDU_PASSWORD
      valueFrom:
        secretKeyRef:
          name: pdu-credentials
          key: password
    resources:
      limits:
        cpu: 100m
        memory: 64Mi
      requests:
        cpu: 10m
        memory: 32Mi
```

## Key Features

### Production Group
- **Conservative scaling**: 30% utilization threshold with long wait times
- **Safety margins**: 30-minute delays before scaling down
- **Moderate cluster size**: Up to 5 nodes for typical homelab setups

### Power Management Methods
- **Wake-on-LAN**: Network-based startup with SSH shutdown
- **IPMI**: Industry-standard server management interface
- **Smart PDU**: Intelligent power distribution unit control

### Cost Management
- **Realistic pricing**: Actual power consumption costs
- **Per-pod costs**: Additional charges for resource usage
- **GPU premium**: Higher costs for GPU-enabled nodes