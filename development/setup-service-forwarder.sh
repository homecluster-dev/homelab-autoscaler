#!/bin/bash
# Setup local service forwarder with the correct Docker bridge IP
# This script should be run after the k3d cluster is created

set -e

NAMESPACE="${NAMESPACE:-homelab-autoscaler-system}"
CLUSTER_NAME="${CLUSTER_NAME:-homelab-autoscaler}"

# Get the Docker bridge gateway IP for the k3d network
GATEWAY_IP=$(docker network inspect k3d-${CLUSTER_NAME} --format='{{range .IPAM.Config}}{{.Gateway}}{{end}}' 2>/dev/null)

if [ -z "$GATEWAY_IP" ]; then
    echo "Warning: Could not determine k3d network gateway, using default Docker bridge (172.17.0.1)"
    GATEWAY_IP="172.17.0.1"
fi

echo "Setting up service forwarder to use gateway IP: $GATEWAY_IP"

# Create or update the service forwarder
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: homelab-autoscaler-grpc-local
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: homelab-autoscaler
    app.kubernetes.io/component: grpc-forwarder
    app.kubernetes.io/part-of: homelab-autoscaler-dev
spec:
  ports:
  - port: 50051
    targetPort: 50052
    protocol: TCP
---
apiVersion: v1
kind: Endpoints
metadata:
  name: homelab-autoscaler-grpc-local
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: homelab-autoscaler
    app.kubernetes.io/component: grpc-forwarder
    app.kubernetes.io/part-of: homelab-autoscaler-dev
subsets:
- addresses:
  - ip: ${GATEWAY_IP}
  ports:
  - port: 50052
    protocol: TCP
---
apiVersion: v1
kind: Service
metadata:
  name: homelab-autoscaler-webhook-local
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: homelab-autoscaler
    app.kubernetes.io/component: webhook-forwarder
    app.kubernetes.io/part-of: homelab-autoscaler-dev
spec:
  type: ExternalName
  externalName: host.k3d.internal
  ports:
  - port: 443
    targetPort: 9443
EOF

echo "Service forwarder configured successfully"
