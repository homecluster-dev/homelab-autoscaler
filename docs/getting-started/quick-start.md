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

The chart's defaults enable the validating webhooks and serve their certificates with cert-manager, so
install that first, and ask for cluster-autoscaler in the same release: nothing scales a pool on its
own.

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait -n cert-manager --for=condition=available --timeout=300s \
  deployment/cert-manager deployment/cert-manager-webhook deployment/cert-manager-cainjector

kubectl create namespace homelab-autoscaler-system

helm repo add homelab-autoscaler https://autoscaler.homecluster.dev
helm repo update

helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system \
  --set clusterAutoscaler.enabled=true \
  --wait
```

### 3. Create Node Group

Every field of the spec except `minSize` and `nodeReadinessTaints` is required. The utilization
thresholds are strings; `0.5` unquoted is rejected. The pool's ceiling is the number of `Node` CRs
you label `group: k3d-workers` — there is no `maxSize`.

```bash
cat <<EOF | kubectl apply -f -
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: k3d-workers
  namespace: homelab-autoscaler-system
spec:
  minSize: 0
  scaleDownUtilizationThreshold: "0.5"
  scaleDownGpuUtilizationThreshold: "0.5"
  scaleDownUnneededTime: "2m"
  scaleDownUnreadyTime: "1m"
  maxNodeProvisionTime: "2m"
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: true
EOF
```

### 4. Create Example Nodes

A slot declares how its machine is powered, and the shape it accepts is
`image` / `command` / `args` / `volumes` / `serviceAccount` — anything else is pruned. These two
simulate power operations with `echo`, so you can watch the whole loop without a BMC: the Jobs run,
the claim moves `pending → booting → ready`, and the machines never actually move, which is why the
scale-down in this quick start is the part that needs a real power API.

```bash
cat <<EOF | kubectl apply -f -
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: k3d-worker-1
  namespace: homelab-autoscaler-system
  labels:
    group: k3d-workers
spec:
  kubernetesNodeName: k3d-homelab-autoscaler-agent-0
  pricing:
    hourlyRate: "1.5"
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
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: k3d-worker-2
  namespace: homelab-autoscaler-system
  labels:
    group: k3d-workers
spec:
  kubernetesNodeName: k3d-homelab-autoscaler-agent-1
  pricing:
    hourlyRate: "1.5"
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

### 5. Verify Installation

```bash
kubectl get groups -n homelab-autoscaler-system
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
kubectl get nodeclaims -n homelab-autoscaler-system
```

`SLOTS 2 / ON 0 / OFF 2` and two slots reading `off off k3d-workers` is a pool at rest. Enable
cluster-autoscaler (`clusterAutoscaler.enabled: true` in your values, see
[Installation](installation.md)) before the next step: the pool only grows when the autoscaler fits a
pending pod on the group's node template.

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
kubectl get nodeclaims -n homelab-autoscaler-system -w
kubectl logs -f deployment/homelab-autoscaler-cluster-autoscaler -n homelab-autoscaler-system
```

The claim is the request: `pending` is the autoscaler's answer, `booting` is the startup Job, `ready`
is the machine that takes the pod. The scale-down is the half this simulated pool cannot show you —
the scripts only `echo`, so the machines stay up and the slot sits at `draining` while the provider
keeps observing the machine on.

### Scale Down

```bash
kubectl scale deployment test-scale-up --replicas=1
```

## Cleanup

```bash
# Remove test resources
kubectl delete deployment test-scale-up
kubectl delete nodeclaims --all -n homelab-autoscaler-system
kubectl delete nodes.infra.homecluster.dev --all -n homelab-autoscaler-system
kubectl delete groups --all -n homelab-autoscaler-system

# Uninstall. The chart keeps the CRDs by default (`crd.keep: true`): delete all three to go clean.
helm uninstall homelab-autoscaler -n homelab-autoscaler-system
kubectl delete crd groups.infra.homecluster.dev nodes.infra.homecluster.dev nodeclaims.infra.homecluster.dev
kubectl delete namespace homelab-autoscaler-system

# Delete cluster
k3d cluster delete homelab-autoscaler
```

## Next Steps

- **Production use**: Follow the [Installation Guide](installation.md)
- **Learn more**: Read the [Architecture Overview](../architecture/overview.md)
- **Troubleshoot**: Check [Known Issues](../troubleshooting/known-issues.md)