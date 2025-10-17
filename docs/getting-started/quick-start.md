# Quick Start Guide

Get up and running with homelab-autoscaler in 5 minutes using k3d for local testing.

> ℹ️ **NOTE**: This guide uses k3d for quick local testing and development. For production deployment, see the [Installation Guide](installation.md).

## Prerequisites

- **Docker** - For k3d cluster
- **k3d** - Kubernetes in Docker
- **kubectl** - Kubernetes CLI
- **Helm** - Package manager for Kubernetes

### Install Prerequisites (macOS)

```bash
# Install via Homebrew
brew install k3d kubectl helm
```

### Install Prerequisites (Linux)

```bash
# Install k3d
curl -s https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | bash

# Install kubectl
curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl

# Install Helm
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

## 5-Step Quick Start

### Step 1: Create k3d Cluster

```bash
# Create a k3d cluster with multiple nodes
k3d cluster create homelab-autoscaler \
  --servers 1 \
  --agents 2 \
  --wait

# Export kubeconfig
export KUBECONFIG=$(k3d kubeconfig write homelab-autoscaler)

# Verify cluster is running
kubectl cluster-info

```

### Step 2: Install homelab-autoscaler

```bash
# Create namespace
kubectl create namespace homelab-autoscaler-system

# Add Helm repository
helm repo add homelab-autoscaler https://autoscaler.homecluster.dev/

helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --wait
```

### Step 3: Create Example Node Group

```bash
# Create a node group for autoscaling
cat <<EOF | kubectl apply -f -
apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: k3d-workers
  namespace: homelab-autoscaler-system
spec:
  maxSize: 3
  scaleDownUtilizationThreshold: 0.5
  scaleDownUnneededTime: "2m"
  scaleDownUnreadyTime: "1m"
  maxNodeProvisionTime: "2m"
  ignoreDaemonSetsUtilization: true
  zeroOrMaxNodeScaling: false
EOF
```

### Step 4: Create Example Physical Nodes

```bash
# Create example nodes that simulate physical machines
cat <<EOF | kubectl apply -f -
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: k3d-worker-1
  namespace: homelab-autoscaler-system
  labels:
    group: k3d-workers
spec:
  kubernetesNodeName: k3d-homelab-autoscaler-agent-0
  powerState: "on"
  pricing:
    hourlyRate: "1.0"
    podRate: "0.0"
  startupPodSpec:
    image: curlimages/curl:latest
    command: ["/bin/sh"]
    args: ["-c", "echo 'Starting node k3d-worker-1' && sleep 10"]
  shutdownPodSpec:
    image: curlimages/curl:latest
    command: ["/bin/sh"]
    args: ["-c", "echo 'Stopping node k3d-worker-1' && sleep 5"]
---
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: k3d-worker-2
  namespace: homelab-autoscaler-system
  labels:
    group: k3d-workers
spec:
  kubernetesNodeName: k3d-homelab-autoscaler-agent-1
  powerState: "off"
  pricing:
    hourlyRate: "1.0"
    podRate: "0.0"
  startupPodSpec:
    image: curlimages/curl:latest
    command: ["/bin/sh"]
    args: ["-c", "echo 'Starting node k3d-worker-2' && sleep 10"]
  shutdownPodSpec:
    image: curlimages/curl:latest
    command: ["/bin/sh"]
    args: ["-c", "echo 'Stopping node k3d-worker-2' && sleep 5"]
EOF
```

### Step 5: Verify Installation

```bash
# Check all components are running
kubectl get all -n homelab-autoscaler-system

# Check custom resources
kubectl get groups -n homelab-autoscaler-system
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system

# Check cluster autoscaler logs
kubectl logs -f deployment/homelab-autoscaler-cluster-autoscaler \
  -n homelab-autoscaler-system
```

## Testing Autoscaling

### Test Scale-Up

Create a deployment that requires more resources:

```bash
# Create a resource-intensive deployment
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-scale-up
spec:
  replicas: 10
  selector:
    matchLabels:
      app: test-scale-up
  template:
    metadata:
      labels:
        app: test-scale-up
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        resources:
          requests:
            cpu: 500m
            memory: 512Mi
          limits:
            cpu: 1000m
            memory: 1Gi
EOF
```

### Monitor Scaling Activity

```bash
# Watch pods and nodes
kubectl get pods -w &
kubectl get nodes -w &

# Monitor autoscaler logs
kubectl logs -f deployment/homelab-autoscaler-cluster-autoscaler \
  -n homelab-autoscaler-system

# Check node status
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -w
```

### Test Scale-Down

```bash
# Reduce deployment size
kubectl scale deployment test-scale-up --replicas=1

# Watch for scale-down activity (may take several minutes)
kubectl logs -f deployment/homelab-autoscaler-cluster-autoscaler \
  -n homelab-autoscaler-system | grep -i "scale.*down"
```

## Common Commands

### Check System Status

```bash
# Overall system health
kubectl get all -n homelab-autoscaler-system

# Custom resources
kubectl get groups,nodes.infra.homecluster.dev -n homelab-autoscaler-system

# Recent events
kubectl get events -n homelab-autoscaler-system --sort-by='.lastTimestamp' | tail -10
```

### Debug Issues

```bash
# Controller logs
kubectl logs deployment/homelab-autoscaler-controller-manager \
  -n homelab-autoscaler-system

# Cluster autoscaler logs
kubectl logs deployment/homelab-autoscaler-cluster-autoscaler \
  -n homelab-autoscaler-system

# Check for failed jobs
kubectl get jobs -n homelab-autoscaler-system
```

### Test gRPC Interface

```bash
# Port forward to gRPC service
kubectl port-forward service/homelab-autoscaler-grpc 50051:50051 \
  -n homelab-autoscaler-system &

# Test with grpcurl (if available)
grpcurl -plaintext localhost:50051 list
grpcurl -plaintext -d '{}' localhost:50051 externalgrpc.CloudProvider/NodeGroups
```

## Expected Behavior

### What Should Work

✅ **Controller Installation** - Pods start and CRDs are created  
✅ **Resource Creation** - Groups and Nodes can be created  
✅ **Basic Monitoring** - Logs show controller activity  
✅ **Job Creation** - Power operations create Kubernetes Jobs  

### Known Limitations

ℹ️ **Configuration Limitations** - Some advanced features may require additional setup
ℹ️ **k3d Environment** - Simulated physical nodes for testing purposes
ℹ️ **Development Setup** - Optimized for learning and development workflows

See [Known Issues](../troubleshooting/known-issues.md) for current limitations and workarounds.

## Cleanup

### Remove Test Resources

```bash
# Delete test deployment
kubectl delete deployment test-scale-up

# Delete custom resources
kubectl delete groups,nodes.infra.homecluster.dev --all \
  -n homelab-autoscaler-system
```

### Uninstall homelab-autoscaler

```bash
# Uninstall Helm release
helm uninstall homelab-autoscaler -n homelab-autoscaler-system

# Remove CRDs (optional)
kubectl delete crd groups.infra.homecluster.dev nodes.infra.homecluster.dev

# Remove namespace
kubectl delete namespace homelab-autoscaler-system
```

### Delete k3d Cluster

```bash
# Delete the entire cluster
k3d cluster delete homelab-autoscaler
```

## Next Steps

### For Development

1. **Review Architecture** - Read [Architecture Overview](../architecture/overview.md)
2. **Setup Development** - Follow [Development Setup](../development/setup.md)
3. **Fix Known Issues** - Contribute to resolving [Known Issues](../troubleshooting/known-issues.md)

### For Production

1. **Install via Helm** - Use the [Installation Guide](installation.md)
2. **Configure Security** - Set up proper RBAC and network policies
3. **Monitor System** - Implement monitoring and alerting
4. **Review Production Guide** - Follow production-specific configuration recommendations

## Troubleshooting

### Common Issues

#### Pods Not Starting

```bash
# Check pod status and events
kubectl describe pods -n homelab-autoscaler-system

# Check resource constraints
kubectl top nodes
kubectl top pods -n homelab-autoscaler-system
```

#### CRDs Not Installing

```bash
# Check CRD status
kubectl get crds | grep homecluster

# Manually apply CRDs
kubectl apply -f config/crd/bases/
```

#### gRPC Connection Issues

```bash
# Check service status
kubectl get services -n homelab-autoscaler-system

# Test port forwarding
kubectl port-forward service/homelab-autoscaler-grpc 50051:50051 \
  -n homelab-autoscaler-system
```

### Getting Help

1. **Check Logs** - Review controller and cluster autoscaler logs
2. **Debug Guide** - Follow [Debugging Guide](../troubleshooting/debugging-guide.md)
3. **Known Issues** - Check [Known Issues](../troubleshooting/known-issues.md)
4. **GitHub Issues** - Report bugs with debug information

## Related Documentation

- [Installation Guide](installation.md) - Production Helm installation
- [Architecture Overview](../architecture/overview.md) - System design
- [Known Issues](../troubleshooting/known-issues.md) - Current limitations
- [API Reference](../api-reference/crds/group.md) - CRD specifications