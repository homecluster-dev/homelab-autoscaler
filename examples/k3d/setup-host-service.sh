#!/bin/bash
# Setup script for k3d e2e test environment
# Creates a service that resolves to the Docker gateway IP

set -e

CLUSTER_NAME="homelab-autoscaler"
NAMESPACE="homelab-autoscaler-system"
SERVICE_NAME="host-internal"

echo "Getting Docker gateway IP for cluster: k3d-${CLUSTER_NAME}"
GATEWAY_IP=$(docker network inspect "k3d-${CLUSTER_NAME}" -f '{{(index .IPAM.Config 0).Gateway}}')

if [ -z "$GATEWAY_IP" ]; then
    echo "Error: Could not get gateway IP for k3d-${CLUSTER_NAME}"
    exit 1
fi

echo "Gateway IP: ${GATEWAY_IP}"

echo "Creating service to resolve host-internal.${NAMESPACE}.svc.cluster.local to ${GATEWAY_IP}"

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: ${SERVICE_NAME}
  namespace: ${NAMESPACE}
spec:
  clusterIP: None
  ports:
  - port: 9052
    targetPort: 9052
    protocol: TCP
---
apiVersion: v1
kind: Endpoints
metadata:
  name: ${SERVICE_NAME}
  namespace: ${NAMESPACE}
subsets:
- addresses:
  - ip: ${GATEWAY_IP}
  ports:
  - port: 9052
    protocol: TCP
EOF

echo "Service created successfully"
echo "Pods can now use: http://${SERVICE_NAME}.${NAMESPACE}.svc.cluster.local:9052"
