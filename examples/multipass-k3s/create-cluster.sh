#!/bin/bash


# Launch 3 nodes
multipass launch 22.04 --name k3s-master --cpus 2 --memory 2G --disk 8G
multipass launch 22.04 --name k3s-worker1 --cpus 2 --memory 1.5G --disk 8G
multipass launch 22.04 --name k3s-worker2 --cpus 2 --memory 1.5G --disk 8G

# Install k3s on master
K3S_SERVER=$(multipass info k3s-master | grep IPv4 | awk '{print $2}')
multipass exec k3s-master -- bash -c "curl -sfL https://get.k3s.io | sh -"

# Get token for workers
K3S_TOKEN=$(multipass exec k3s-master sudo cat /var/lib/rancher/k3s/server/node-token)

# Join workers to cluster
multipass exec k3s-worker1 -- \
  bash -c "curl -sfL https://get.k3s.io | K3S_URL=https://$K3S_SERVER:6443 K3S_TOKEN=$K3S_TOKEN sh -"

multipass exec k3s-worker2 -- \
  bash -c "curl -sfL https://get.k3s.io | K3S_URL=https://$K3S_SERVER:6443 K3S_TOKEN=$K3S_TOKEN sh -"

# Verify cluster
multipass exec k3s-master -- sudo kubectl get nodes

multipass exec k3s-master -- sudo cat /etc/rancher/k3s/k3s.yaml > kubeconfig
sed -i '' "s/127.0.0.1/$K3S_SERVER/g" kubeconfig
export KUBECONFIG=$(pwd)/kubeconfig
kubectl get nodes
