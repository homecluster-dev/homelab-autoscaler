# Installation Guide

This guide covers installing the homelab-autoscaler using Helm, the recommended production deployment method.

> ℹ️ **NOTE**: Review [Known Issues](../troubleshooting/known-issues.md) for current limitations and configuration recommendations before production deployment.

## Prerequisites

### Required Tools

- **Kubernetes cluster** (v1.19+)
- **Helm** (v3.0+)
- **kubectl** configured for your cluster

### Cluster Requirements

- **Minimum nodes**: 2 (1 control plane, 1 worker)
- **RBAC enabled**: Required for controller permissions
- **Custom Resource Definitions**: Cluster must support CRDs

## Installation Methods

### Method 1: Helm Chart (Recommended)

#### 1. Add Helm Repository

```bash
# Add the homelab-autoscaler Helm repository
helm repo add homelab-autoscaler https://homecluster-dev.github.io/homelab-autoscaler
helm repo update
```

#### 2. Create Namespace

```bash
kubectl create namespace homelab-autoscaler-system
```

#### 3. Install with Default Values

```bash
helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --wait
```

#### 4. Verify Installation

```bash
# Check pods are running
kubectl get pods -n homelab-autoscaler-system

# Check CRDs are installed
kubectl get crds | grep homecluster

# Check services
kubectl get services -n homelab-autoscaler-system
```

### Method 2: Local Chart Installation

If you're building from source or using a local chart:

#### 1. Build the Chart

```bash
# From the project root directory
make helm-package
```

#### 2. Install Local Chart

```bash
helm install homelab-autoscaler ./dist/chart \
  --namespace homelab-autoscaler-system \
  --create-namespace \
  --wait
```

## Configuration Options

### Basic Configuration

Create a `values.yaml` file to customize the installation:

```yaml
# values.yaml
controller:
  # Controller manager configuration
  replicas: 1
  image:
    repository: ghcr.io/homecluster-dev/homelab-autoscaler
    tag: "latest"
    pullPolicy: IfNotPresent
  
  # Resource limits
  resources:
    limits:
      cpu: 500m
      memory: 128Mi
    requests:
      cpu: 10m
      memory: 64Mi

grpcServer:
  # Enable gRPC server for cluster autoscaler integration
  enabled: true
  port: 50051
  
  # Service configuration
  service:
    type: ClusterIP
    port: 50051

clusterAutoscaler:
  # Install cluster autoscaler alongside controller
  enabled: true
  image:
    repository: registry.k8s.io/autoscaling/cluster-autoscaler
    tag: "v1.27.0"
  
  # Cluster autoscaler configuration
  config:
    scaleDownDelayAfterAdd: "10m"
    scaleDownUnneededTime: "10m"
    scaleDownUtilizationThreshold: 0.5

# Webhook configuration (if using admission webhooks)
webhook:
  enabled: false
  port: 9443
```

#### Install with Custom Values

```bash
helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --values values.yaml \
  --wait
```

### Advanced Configuration

#### Production Values Example

```yaml
# production-values.yaml
controller:
  replicas: 2  # High availability
  image:
    tag: "v0.1.0"  # Use specific version
    pullPolicy: Always
  
  resources:
    limits:
      cpu: 1000m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
  
  # Enable leader election for HA
  leaderElection:
    enabled: true

grpcServer:
  enabled: true
  service:
    type: LoadBalancer  # External access
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-type: "nlb"

clusterAutoscaler:
  enabled: true
  config:
    # Production-ready settings
    scaleDownDelayAfterAdd: "30m"
    scaleDownUnneededTime: "30m"
    scaleDownUtilizationThreshold: 0.3
    maxNodeProvisionTime: "15m"
    
  resources:
    limits:
      cpu: 500m
      memory: 256Mi
    requests:
      cpu: 100m
      memory: 128Mi

# Enable monitoring
monitoring:
  enabled: true
  serviceMonitor:
    enabled: true
    namespace: monitoring

# Security settings
rbac:
  create: true
serviceAccount:
  create: true
  annotations:
    eks.amazonaws.com/role-arn: "arn:aws:iam::ACCOUNT:role/homelab-autoscaler"

# Pod security
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  fsGroup: 65532

securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop:
    - ALL
  readOnlyRootFilesystem: true
```

## Post-Installation Setup

### 1. Create Node Groups

Create your first autoscaling group:

```yaml
# group-example.yaml
apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: worker-nodes
  namespace: homelab-autoscaler-system
spec:
  maxSize: 5
  scaleDownUtilizationThreshold: 0.5
  scaleDownUnneededTime: "10m"
  nodeSelector:
    node-type: worker
```

```bash
kubectl apply -f group-example.yaml
```

### 2. Create Physical Nodes

Define your physical machines:

```yaml
# node-example.yaml
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: worker-01
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
    node-type: worker
spec:
  kubernetesNodeName: worker-01
  powerState: "off"
  pricing:
    hourlyRate: "1.5"
    podRate: "0.0"
  startupPodSpec:
    image: homelab/power-manager:latest
    command: ["wake-on-lan"]
    args: ["00:11:22:33:44:55"]
  shutdownPodSpec:
    image: homelab/power-manager:latest
    command: ["ssh-shutdown"]
    args: ["worker-01.local"]
```

```bash
kubectl apply -f node-example.yaml
```

### 3. Configure Cluster Autoscaler

The cluster autoscaler should automatically discover your node groups. Verify the configuration:

```bash
# Check cluster autoscaler logs
kubectl logs -f deployment/homelab-autoscaler-cluster-autoscaler \
  -n homelab-autoscaler-system

# Check discovered node groups
kubectl logs deployment/homelab-autoscaler-cluster-autoscaler \
  -n homelab-autoscaler-system | grep "node group"
```

## Verification

### Check System Status

```bash
# Verify all components are running
kubectl get all -n homelab-autoscaler-system

# Check CRD installation
kubectl get crds | grep homecluster

# Verify node groups are loaded
kubectl get groups -n homelab-autoscaler-system

# Check physical nodes
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
```

### Test gRPC Connectivity

```bash
# Port forward to gRPC service
kubectl port-forward service/homelab-autoscaler-grpc 50051:50051 \
  -n homelab-autoscaler-system &

# Test with grpcurl (if available)
grpcurl -plaintext localhost:50051 list
```

## Troubleshooting

### Common Installation Issues

#### 1. CRD Installation Failures

```bash
# Check CRD status
kubectl get crds | grep homecluster

# Manually install CRDs if needed
kubectl apply -f https://raw.githubusercontent.com/homecluster-dev/homelab-autoscaler/main/config/crd/bases/
```

#### 2. RBAC Permission Issues

```bash
# Check service account
kubectl get serviceaccount -n homelab-autoscaler-system

# Check role bindings
kubectl get rolebinding,clusterrolebinding | grep homelab-autoscaler

# Describe pod for permission errors
kubectl describe pod -n homelab-autoscaler-system -l control-plane=controller-manager
```

#### 3. Image Pull Issues

```bash
# Check image pull secrets
kubectl get secrets -n homelab-autoscaler-system

# Check pod events
kubectl get events -n homelab-autoscaler-system --sort-by='.lastTimestamp'
```

### Getting Help

1. **Check Known Issues**: Review [Known Issues](../troubleshooting/known-issues.md)
2. **Debug Guide**: Follow the [Debugging Guide](../troubleshooting/debugging-guide.md)
3. **Logs**: Collect logs using the debug scripts in the troubleshooting guide
4. **Community**: Open an issue on GitHub with debug information

## Upgrading

### Helm Upgrade

```bash
# Update repository
helm repo update

# Upgrade to latest version
helm upgrade homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --values values.yaml
```

### Version-Specific Upgrades

```bash
# Upgrade to specific version
helm upgrade homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --version 0.2.0 \
  --values values.yaml
```

## Uninstallation

### Remove Helm Release

```bash
# Uninstall the release
helm uninstall homelab-autoscaler -n homelab-autoscaler-system

# Remove CRDs (optional - will delete all custom resources)
kubectl delete crd groups.infra.homecluster.dev
kubectl delete crd nodes.infra.homecluster.dev

# Remove namespace
kubectl delete namespace homelab-autoscaler-system
```

## Next Steps

1. **Configure Node Groups**: Set up your physical machine groups
2. **Test Scaling**: Create workloads to test autoscaling behavior
3. **Monitor System**: Set up monitoring and alerting
4. **Review Security**: Implement proper RBAC and network policies

## Related Documentation

- [Quick Start Guide](quick-start.md) - Get started quickly with k3d
- [Architecture Overview](../architecture/overview.md) - Understand the system design
- [API Reference](../api-reference/crds/group.md) - CRD specifications
- [Troubleshooting](../troubleshooting/debugging-guide.md) - Debug common issues