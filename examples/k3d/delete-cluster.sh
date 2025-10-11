#!/bin/bash

CLUSTER_NAME="homelab-autoscaler"

echo "Deleting k3d cluster: ${CLUSTER_NAME}"
k3d cluster delete ${CLUSTER_NAME}

echo "Cluster deleted successfully!"