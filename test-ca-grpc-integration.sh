#!/bin/bash
# Test script for cluster autoscaler gRPC integration with k3d
# This script tests the externalgrpc proto v1.35.0 changes

set -e

CLUSTER_NAME="homelab-ca-test"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
KUBECONFIG_FILE="$SCRIPT_DIR/kubeconfig"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

cleanup() {
    log_info "Cleaning up test environment..."
    cd "$PROJECT_ROOT"
    
    # Delete k3d cluster
    if command -v k3d &> /dev/null; then
        log_info "Deleting k3d cluster: $CLUSTER_NAME"
        k3d cluster delete "$CLUSTER_NAME" 2>/dev/null || true
    fi
    
    # Remove kubeconfig
    rm -f "$KUBECONFIG_FILE"
    
    log_info "Cleanup completed"
}

# Set trap for cleanup on exit
trap cleanup EXIT

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."
    
    local missing=0
    
    for cmd in k3d kubectl go python3; do
        if ! command -v $cmd &> /dev/null; then
            log_error "$cmd is not installed"
            missing=1
        fi
    done
    
    if [ $missing -eq 1 ]; then
        log_error "Please install the missing prerequisites"
        exit 1
    fi
    
    log_info "All prerequisites satisfied"
}

# Create k3d cluster
create_cluster() {
    log_info "Creating k3d cluster: $CLUSTER_NAME"
    
    # Delete existing cluster if present
    k3d cluster list | grep -q "$CLUSTER_NAME" && k3d cluster delete "$CLUSTER_NAME"
    
    k3d cluster create "$CLUSTER_NAME" \
        --servers 3 \
        --agents 2 \
        -p "80:80@loadbalancer" \
        -p "443:443@loadbalancer" \
        --wait
    
    # Export kubeconfig
    k3d kubeconfig get "$CLUSTER_NAME" > "$KUBECONFIG_FILE"
    export KUBECONFIG="$KUBECONFIG_FILE"
    
    log_info "Cluster created successfully"
    kubectl cluster-info
}

# Build and deploy the controller
deploy_controller() {
    log_info "Building and deploying controller..."
    
    cd "$PROJECT_ROOT"
    
    # Build the manager binary
    log_info "Building manager binary..."
    go build -o bin/manager ./cmd/main.go
    
    # Install CRDs
    log_info "Installing CRDs..."
    make install
    
    # Deploy controller
    log_info "Deploying controller..."
    make deploy
    
    # Wait for controller to be ready
    log_info "Waiting for controller to be ready..."
    kubectl wait --for=condition=available deployment/homelab-autoscaler-controller-manager \
        -n homelab-autoscaler-system \
        --timeout=120s
    
    log_info "Controller deployed successfully"
}

# Start gRPC server in background
start_grpc_server() {
    log_info "Starting gRPC server..."
    
    cd "$PROJECT_ROOT"
    
    # Start the manager with gRPC server enabled
    ENABLE_WEBHOOKS=false \
    GRPC_SERVER_ADDRESS=":50052" \
    KUBECONFIG="$KUBECONFIG_FILE" \
    go run ./cmd/main.go \
        --grpc-server-address=":50052" \
        --metrics-bind-address=":8443" \
        --health-probe-bind-address=":8081" \
        --leader-elect=false &
    
    GRPC_PID=$!
    log_info "gRPC server started with PID: $GRPC_PID"
    
    # Wait for gRPC server to be ready
    sleep 5
    
    # Check if gRPC server is listening
    if lsof -i :50052 > /dev/null; then
        log_info "gRPC server is listening on port 50052"
    else
        log_error "gRPC server failed to start"
        kill $GRPC_PID 2>/dev/null || true
        exit 1
    fi
    
    echo $GRPC_PID > /tmp/grpc-server.pid
}

# Deploy cluster autoscaler with externalgrpc provider
deploy_cluster_autoscaler() {
    log_info "Deploying cluster autoscaler with externalgrpc provider..."
    
    # Create namespace
    kubectl create namespace cluster-autoscaler --dry-run=client -o yaml | kubectl apply -f -
    
    # Create ConfigMap for cluster autoscaler configuration
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-autoscaler-config
  namespace: cluster-autoscaler
data:
  scale-down-delay-after-add: "10m"
  scale-down-unneeded-time: "10m"
  scale-down-unready-time: "10m"
  scan-interval: "10s"
  balancing-ignore-label_1: "topology.kubernetes.io/zone"
  balancing-ignore-label_2: "kubernetes.io/hostname"
EOF

    # Create ServiceAccount and RBAC
    cat <<EOF | kubectl apply -f -
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
    resources: ["events", "endpoints"]
    verbs: ["create", "patch"]
  - apiGroups: [""]
    resources: ["pods/eviction"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["pods/status"]
    verbs: ["update"]
  - apiGroups: [""]
    resources: ["endpoints"]
    resourceNames: ["cluster-autoscaler"]
    verbs: ["get", "update"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["watch", "list", "get", "update"]
  - apiGroups: [""]
    resources: ["namespaces", "pods", "services", "replicationcontrollers", "persistentvolumeclaims", "persistentvolumes"]
    verbs: ["watch", "list", "get"]
  - apiGroups: ["extensions"]
    resources: ["replicasets", "daemonsets"]
    verbs: ["watch", "list", "get"]
  - apiGroups: ["policy"]
    resources: ["poddisruptionbudgets"]
    verbs: ["watch", "list"]
  - apiGroups: ["apps"]
    resources: ["statefulsets", "replicasets", "daemonsets"]
    verbs: ["watch", "list", "get"]
  - apiGroups: ["storage.k8s.io"]
    resources: ["storageclasses", "csinodes", "csidrivers", "csistoragecapacities"]
    verbs: ["watch", "list", "get"]
  - apiGroups: ["batch", "extensions"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["create"]
  - apiGroups: ["coordination.k8s.io"]
    resourceNames: ["cluster-autoscaler"]
    resources: ["leases"]
    verbs: ["get", "update"]
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
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cluster-autoscaler
  namespace: cluster-autoscaler
rules:
  - apiGroups: [""]
    resources: ["endpoints"]
    verbs: ["create"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["create", "get", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cluster-autoscaler
  namespace: cluster-autoscaler
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: cluster-autoscaler
subjects:
  - kind: ServiceAccount
    name: cluster-autoscaler
    namespace: cluster-autoscaler
EOF

    # Create gRPC provider deployment
    cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cluster-autoscaler
  namespace: cluster-autoscaler
  labels:
    app: cluster-autoscaler
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
            - --stderrthreshold=info
            - --cloud-provider=externalgrpc
            - --external-grpc-provider-config=/config/grpc-provider-config.yaml
            - --external-grpc-provider-address=localhost:50052
            - --nodes=2:10:homelab-autoscaler-worker
          volumeMounts:
            - name: grpc-config
              mountPath: /config
              readOnly: true
          resources:
            limits:
              cpu: 100m
              memory: 300Mi
            requests:
              cpu: 100m
              memory: 300Mi
      volumes:
        - name: grpc-config
          configMap:
            name: cluster-autoscaler-config
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                labelSelector:
                  matchExpressions:
                    - key: app
                      operator: In
                      values:
                        - cluster-autoscaler
                topologyKey: kubernetes.io/hostname
EOF

    log_info "Waiting for cluster autoscaler deployment..."
    kubectl wait --for=condition=available deployment/cluster-autoscaler \
        -n cluster-autoscaler \
        --timeout=120s || true
    
    log_info "Cluster autoscaler deployment created"
}

# Monitor cluster autoscaler logs
monitor_ca_logs() {
    log_info "Monitoring cluster autoscaler logs for errors..."
    
    echo "=== Cluster Autoscaler Logs (first 60 seconds) ==="
    kubectl logs -n cluster-autoscaler -l app=cluster-autoscaler \
        --tail=100 \
        -f &
    LOG_PID=$!
    
    sleep 60
    
    # Check for critical errors
    log_info "Checking for critical errors..."
    CA_ERRORS=$(kubectl logs -n cluster-autoscaler -l app=cluster-autoscaler 2>&1 | \
        grep -i "error\|panic\|fatal\|could not create gRPC client" || true)
    
    kill $LOG_PID 2>/dev/null || true
    
    if [ -n "$CA_ERRORS" ]; then
        log_error "Errors found in cluster autoscaler logs:"
        echo "$CA_ERRORS"
        return 1
    else
        log_info "No critical errors found in cluster autoscaler logs"
        return 0
    fi
}

# Verify gRPC connection
verify_grpc_connection() {
    log_info "Verifying gRPC connection..."
    
    # Check if gRPC server is responding
    if lsof -i :50052 > /dev/null; then
        log_info "✓ gRPC server is listening"
    else
        log_error "✗ gRPC server is not listening"
        return 1
    fi
    
    # Check cluster autoscaler pod status
    CA_STATUS=$(kubectl get pod -n cluster-autoscaler -l app=cluster-autoscaler -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "Unknown")
    
    if [ "$CA_STATUS" = "Running" ]; then
        log_info "✓ Cluster autoscaler pod is running"
    else
        log_warn "⚠ Cluster autoscaler pod status: $CA_STATUS"
        return 1
    fi
    
    # Check for "Could not create gRPC client" error
    if kubectl logs -n cluster-autoscaler -l app=cluster-autoscaler 2>&1 | \
        grep -q "Could not create gRPC client"; then
        log_error "✗ gRPC client creation failed"
        return 1
    else
        log_info "✓ No gRPC client creation errors"
    fi
    
    # Check for YAML parsing errors
    if kubectl logs -n cluster-autoscaler -l app=cluster-autoscaler 2>&1 | \
        grep -q "can't parse YAML"; then
        log_error "✗ YAML parsing error detected"
        return 1
    else
        log_info "✓ No YAML parsing errors"
    fi
    
    return 0
}

# Create test Group CRD
create_test_group() {
    log_info "Creating test Group CRD..."
    
    cat <<EOF | kubectl apply -f -
apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: test-worker-group
  namespace: homelab-autoscaler-system
spec:
  maxSize: 5
  minSize: 2
  scaleUpThreshold: 70
  scaleDownThreshold: 30
  scaleDownDelay: "10m"
  scaleDownUnneededTime: "10m"
  scaleDownUnreadyTime: "10m"
  maxNodeProvisionTime: "15m"
  scaleDownUtilizationThreshold: "0.5"
  scaleDownGpuUtilizationThreshold: "0.5"
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: false
EOF

    log_info "Test Group CRD created"
}

# Main test execution
main() {
    log_info "=========================================="
    log_info "Cluster Autoscaler gRPC Integration Test"
    log_info "=========================================="
    
    # Step 1: Check prerequisites
    check_prerequisites
    
    # Step 2: Create k3d cluster
    create_cluster
    
    # Step 3: Deploy controller
    deploy_controller
    
    # Step 4: Create test Group
    create_test_group
    
    # Step 5: Start gRPC server
    start_grpc_server
    
    # Step 6: Deploy cluster autoscaler
    deploy_cluster_autoscaler
    
    # Step 7: Wait and monitor
    log_info "Waiting for cluster autoscaler to stabilize..."
    sleep 30
    
    # Step 8: Verify gRPC connection
    verify_grpc_connection
    VERIFICATION_RESULT=$?
    
    # Step 9: Monitor logs
    monitor_ca_logs
    MONITOR_RESULT=$?
    
    # Final result
    log_info "=========================================="
    if [ $VERIFICATION_RESULT -eq 0 ] && [ $MONITOR_RESULT -eq 0 ]; then
        log_info "✓ All tests passed!"
        log_info "The gRPC proto v1.35.0 update is working correctly"
    else
        log_error "✗ Some tests failed"
        log_info "Check the logs above for details"
    fi
    log_info "=========================================="
    
    # Keep environment running for manual inspection
    log_info "Test environment is still running for manual inspection"
    log_info "To inspect logs: kubectl logs -n cluster-autoscaler -l app=cluster-autoscaler -f"
    log_info "To cleanup manually: ./examples/k3d/delete-cluster.sh"
    
    # Read GRPC_PID and cleanup
    if [ -f /tmp/grpc-server.pid ]; then
        GRPC_PID=$(cat /tmp/grpc-server.pid)
        kill $GRPC_PID 2>/dev/null || true
        rm -f /tmp/grpc-server.pid
    fi
    
    # Return appropriate exit code
    if [ $VERIFICATION_RESULT -eq 0 ] && [ $MONITOR_RESULT -eq 0 ]; then
        exit 0
    else
        exit 1
    fi
}

# Run main function
main "$@"
