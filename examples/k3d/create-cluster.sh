#!/bin/bash

CLUSTER_NAME="homelab-autoscaler"

echo "Creating k3d cluster: ${CLUSTER_NAME}"
k3d cluster create ${CLUSTER_NAME} \
  --servers 3 \
  --agents 2 \
  --wait

echo "Exporting kubeconfig..."
k3d kubeconfig get ${CLUSTER_NAME} > ./kubeconfig
export KUBECONFIG=$(pwd)/kubeconfig
echo "KUBECONFIG exported to: ${KUBECONFIG}"

echo "Cluster created successfully!"
kubectl cluster-info
