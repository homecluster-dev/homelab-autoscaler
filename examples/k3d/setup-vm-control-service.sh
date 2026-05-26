#!/bin/bash
# Setup VM control host service with the correct Docker bridge IP
# This script should be run after the k3d cluster is created and before e2e tests

set -e

NAMESPACE="${NAMESPACE:-homelab-autoscaler-system}"
CLUSTER_NAME="${CLUSTER_NAME:-homelab-autoscaler}"

# Get the Docker bridge gateway IP for the k3d network
# This works for the default Docker bridge network (172.17.0.1)
# or the k3d-specific network if it uses a different subnet
GATEWAY_IP=$(docker network inspect k3d-${CLUSTER_NAME} --format='{{range .IPAM.Config}}{{.Gateway}}{{end}}' 2>/dev/null)

if [ -z "$GATEWAY_IP" ]; then
    echo "Warning: Could not determine k3d network gateway, using default Docker bridge (172.17.0.1)"
    GATEWAY_IP="172.17.0.1"
fi

echo "Setting VM control host service to use gateway IP: $GATEWAY_IP"

# Create or update the ExternalName service with the gateway IP
# ExternalName doesn't support IPs directly, so we use an Endpoints object
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: vm-control-host
  namespace: ${NAMESPACE}
spec:
  type: ClusterIP
  clusterIP: None
  ports:
  - port: 9052
    targetPort: 9052
---
apiVersion: v1
kind: Endpoints
metadata:
  name: vm-control-host
  namespace: ${NAMESPACE}
subsets:
- addresses:
  - ip: ${GATEWAY_IP}
  ports:
  - port: 9052
EOF

echo "VM control host service configured successfully"
