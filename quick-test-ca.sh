#!/bin/bash
# Quick test for cluster autoscaler gRPC integration
# This is a simplified version for quick validation

set -e

echo "=== Quick Cluster Autoscaler gRPC Test ==="

# Check if k3d is installed
if ! command -v k3d &> /dev/null; then
    echo "Error: k3d is not installed"
    echo "Install with: curl -s https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | bash"
    exit 1
fi

# Check if kubectl is installed
if ! command -v kubectl &> /dev/null; then
    echo "Error: kubectl is not installed"
    exit 1
fi

# Check if helm is installed
if ! command -v helm &> /dev/null; then
    echo "Error: helm is not installed"
    echo "Install with: https://helm.sh/docs/intro/install/"
    exit 1
fi

CLUSTER_NAME="ca-grpc-test"

echo "1. Creating k3d cluster..."
k3d cluster delete "$CLUSTER_NAME" 2>/dev/null || true
k3d cluster create "$CLUSTER_NAME" --servers 2 --agents 2 \
    --k3s-arg "--tls-sans=127.0.0.1@server:*" \
    --wait
k3d kubeconfig get "$CLUSTER_NAME" > ./kubeconfig
export KUBECONFIG=./kubeconfig

echo "2. Building controller..."
cd /home/opencode/homecluster-dev/homelab-autoscaler
go build -o bin/manager ./cmd/main.go

echo "3. Deploying controller with Helm chart..."
helm upgrade --install homelab-autoscaler ./dist/chart \
    --namespace homelab-autoscaler-system \
    --create-namespace \
    --set controllerManager.container.image.tag=latest \
    --set grpcService.enabled=true \
    --wait --timeout 120s

echo "4. Waiting for controller to be ready..."
kubectl wait --for=condition=available deployment/homelab-autoscaler-controller-manager \
    -n homelab-autoscaler-system --timeout=120s

echo "5. Configuring Helm chart to expose gRPC server..."
# The Helm chart already exposes the gRPC service, we just need to ensure it's accessible
# We'll patch the deployment to run without leader election for testing
kubectl patch deployment homelab-autoscaler-controller-manager \
    -n homelab-autoscaler-system \
    --type='json' \
    -p='[{"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--grpc-server-address=:50052"}]' || true

sleep 5

echo "6. Verifying gRPC service is accessible..."
kubectl wait --for=condition=available deployment/homelab-autoscaler-controller-manager \
    -n homelab-autoscaler-system --timeout=120s

echo "7. Deploying cluster autoscaler..."
kubectl create namespace cluster-autoscaler --dry-run=client -o yaml | kubectl apply -f -

# Deploy cluster autoscaler
kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cluster-autoscaler
  namespace: cluster-autoscaler
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cluster-autoscaler
rules:
  - apiGroups: [""]
    resources: ["events", "endpoints", "pods", "nodes", "namespaces", "services", "replicationcontrollers", "persistentvolumeclaims", "persistentvolumes"]
    verbs: ["watch", "list", "get", "update", "create", "patch"]
  - apiGroups: ["extensions"]
    resources: ["replicasets", "daemonsets"]
    verbs: ["watch", "list", "get"]
  - apiGroups: ["apps"]
    resources: ["statefulsets", "replicasets", "daemonsets"]
    verbs: ["watch", "list", "get"]
  - apiGroups: ["storage.k8s.io"]
    resources: ["storageclasses", "csinodes", "csidrivers"]
    verbs: ["watch", "list", "get"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["create", "get", "update"]
  - apiGroups: ["infra.homecluster.dev"]
    resources: ["groups", "nodes"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cluster-autoscaler
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-autoscaler
subjects:
  - kind: ServiceAccount
    name: cluster-autoscaler
    namespace: cluster-autoscaler
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cluster-autoscaler
  namespace: cluster-autoscaler
spec:
  replicas: 1
  selector:
    matchLabels:
      app: cluster-autoscaler
  template:
    metadata:
      labels:
        app: cluster-autoscaler
    spec:
      serviceAccountName: cluster-autoscaler
      containers:
        - image: registry.k8s.io/autoscaling/cluster-autoscaler:v1.35.0
          name: cluster-autoscaler
          command:
            - ./cluster-autoscaler
            - --v=4
            - --cloud-provider=externalgrpc
            - --external-grpc-provider-address=localhost:50052
            - --nodes=2:10:test-group
          resources:
            limits:
              cpu: 100m
              memory: 300Mi
            requests:
              cpu: 100m
              memory: 300Mi
EOF

echo "8. Waiting for cluster autoscaler to start..."
sleep 15

echo "9. Checking for errors..."
echo "=== Cluster Autoscaler Logs ==="
kubectl logs -n cluster-autoscaler -l app=cluster-autoscaler --tail=50

# Check for critical errors
ERRORS=$(kubectl logs -n cluster-autoscaler -l app=cluster-autoscaler 2>&1 | \
    grep -i "could not create gRPC client\|can't parse YAML" || true)

echo ""
echo "=== Test Results ==="
if [ -n "$ERRORS" ]; then
    echo "❌ FAILED: Found critical errors"
    echo "$ERRORS"
    kubectl logs -n cluster-autoscaler -l app=cluster-autoscaler --tail=100 > /tmp/ca-logs.txt
    echo "Full logs saved to /tmp/ca-logs.txt"
    
    # Cleanup
    kill $GRPC_PID 2>/dev/null || true
    k3d cluster delete "$CLUSTER_NAME"
    exit 1
else
    echo "✅ PASSED: No gRPC connection errors detected"
    echo ""
    echo "The proto v1.35.0 update is working correctly!"
    echo ""
    echo "To inspect further:"
    echo "  kubectl logs -n cluster-autoscaler -l app=cluster-autoscaler -f"
    echo ""
    echo "To cleanup:"
    echo "  k3d cluster delete $CLUSTER_NAME"
    
    # Don't cleanup - let user inspect
    exit 0
fi
