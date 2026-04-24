#!/bin/bash

CLUSTER_NAME="homelab-autoscaler"
# Pinned to a k8s >=1.34 image so that CA 1.35's DRA reflectors
# (ResourceSlice, DeviceClass, ResourceClaim on resource.k8s.io/v1)
# can list/watch successfully. Older k3s ships k8s <1.34 where those APIs
# are missing and CA floods the logs with failed watches.
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.35.3-k3s1}"

echo "Creating k3d cluster: ${CLUSTER_NAME} (image: ${K3S_IMAGE})"
k3d cluster create ${CLUSTER_NAME} \
  --image "${K3S_IMAGE}" \
  --servers 3 \
  --agents 2 \
  --port "8080:8080@loadbalancer" \
  --wait

echo "Exporting kubeconfig..."
k3d kubeconfig get ${CLUSTER_NAME} > ./kubeconfig
export KUBECONFIG=$(pwd)/kubeconfig
echo "KUBECONFIG exported to: ${KUBECONFIG}"

echo "Cluster created successfully!"
kubectl cluster-info
