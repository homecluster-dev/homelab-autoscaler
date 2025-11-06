# Quick Start

Get homelab-autoscaler running in 5 minutes using k3d for local testing.

> **Note**: This guide uses k3d for testing. For production deployment, see the [Installation Guide](installation.md).

## Prerequisites

- Docker
- k3d
- kubectl
- Helm

### Install Tools (macOS)

```bash
brew install k3d kubectl helm
```

### Install Tools (Linux)

```bash
# Install k3d
curl -s https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | bash

# Install kubectl
curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl

# Install Helm
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

## Quick Start Steps

### 1. Create k3d Cluster

```bash
k3d cluster create homelab-autoscaler \
  --servers 1 \
  --agents 2 \
  --wait

export KUBECONFIG=$(k3d kubeconfig write homelab-autoscaler)
kubectl cluster-info
```

### 2. Install homelab-autoscaler

```bash
kubectl create namespace homelab-autoscaler-system
helm repo add homelab-autoscaler https://autoscaler.homecluster.dev/

helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --wait
```

### 3. Create Node Group

```bash
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
EOF
```

### 4. Create Example Nodes

```bash
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

### 5. Verify Installation

```bash
kubectl get all -n homelab-autoscaler-system
kubectl get groups -n homelab-autoscaler-system
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
```

## Test Autoscaling

### Create Test Workload

```bash
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
EOF
```

### Monitor Scaling

```bash
kubectl get pods -w
kubectl get nodes -w
kubectl logs -f deployment/homelab-autoscaler-cluster-autoscaler -n homelab-autoscaler-system
```

### Scale Down

```bash
kubectl scale deployment test-scale-up --replicas=1
```

## Cleanup

```bash
# Remove test resources
kubectl delete deployment test-scale-up
kubectl delete groups,nodes.infra.homecluster.dev --all -n homelab-autoscaler-system

# Uninstall
helm uninstall homelab-autoscaler -n homelab-autoscaler-system
kubectl delete namespace homelab-autoscaler-system

# Delete cluster
k3d cluster delete homelab-autoscaler
```

## Next Steps

- **Production use**: Follow the [Installation Guide](installation.md)
- **Learn more**: Read the [Architecture Overview](../architecture/overview.md)
- **Troubleshoot**: Check [Known Issues](../troubleshooting/known-issues.md)