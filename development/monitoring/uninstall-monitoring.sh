#!/bin/bash

set -e

echo "🗑️  Uninstalling Monitoring Stack..."

# Check prerequisites
if ! command -v kubectl &> /dev/null; then
    echo "❌ kubectl is not installed or not in PATH"
    exit 1
fi

if ! command -v helm &> /dev/null; then
    echo "❌ helm is not installed or not in PATH"
    exit 1
fi

if ! kubectl cluster-info &> /dev/null; then
    echo "❌ No Kubernetes cluster connection found"
    exit 1
fi

# Check if monitoring namespace exists
if ! kubectl get namespace monitoring &> /dev/null; then
    echo "⚠️  Namespace 'monitoring' does not exist. Nothing to uninstall."
    exit 0
fi

echo "⚠️  This will remove all monitoring components and data!"
echo "   - kube-prometheus-stack (Prometheus + Grafana + AlertManager)"
echo "   - Loki"
echo "   - Tempo"
echo "   - All associated PVCs and stored data"
echo ""
read -p "Are you sure you want to continue? (yes/N): " -r
echo
if [[ ! $REPLY =~ ^[Yy][Ee][Ss]$ ]]; then
    echo "Uninstall cancelled"
    exit 0
fi

# Uninstall Helm releases
echo "📦 Uninstalling Helm releases..."

if helm list -n monitoring | grep -q "^tempo"; then
    echo "  Removing Tempo..."
    helm uninstall tempo -n monitoring --wait
else
    echo "  Tempo not found, skipping..."
fi

if helm list -n monitoring | grep -q "^loki"; then
    echo "  Removing Loki..."
    helm uninstall loki -n monitoring --wait
else
    echo "  Loki not found, skipping..."
fi

if helm list -n monitoring | grep -q "^monitoring"; then
    echo "  Removing kube-prometheus-stack..."
    helm uninstall monitoring -n monitoring --wait
else
    echo "  kube-prometheus-stack not found, skipping..."
fi

# Wait for pods to terminate
echo "⏳ Waiting for pods to terminate..."
kubectl wait --for=delete pod --all -n monitoring --timeout=120s 2>/dev/null || true

# Delete CRDs created by kube-prometheus-stack
echo "🧹 Cleaning up Custom Resource Definitions (CRDs)..."
kubectl delete crd alertmanagerconfigs.monitoring.coreos.com 2>/dev/null || true
kubectl delete crd alertmanagers.monitoring.coreos.com 2>/dev/null || true
kubectl delete crd podmonitors.monitoring.coreos.com 2>/dev/null || true
kubectl delete crd probes.monitoring.coreos.com 2>/dev/null || true
kubectl delete crd prometheusagents.monitoring.coreos.com 2>/dev/null || true
kubectl delete crd prometheuses.monitoring.coreos.com 2>/dev/null || true
kubectl delete crd prometheusrules.monitoring.coreos.com 2>/dev/null || true
kubectl delete crd scrapeconfigs.monitoring.coreos.com 2>/dev/null || true
kubectl delete crd servicemonitors.monitoring.coreos.com 2>/dev/null || true
kubectl delete crd thanosrulers.monitoring.coreos.com 2>/dev/null || true

# Delete PVCs (this will also trigger PV deletion)
echo "🗑️  Deleting Persistent Volume Claims (PVCs)..."
if kubectl get pvc -n monitoring --no-headers 2>/dev/null | grep -q .; then
    kubectl delete pvc --all -n monitoring --wait=true
    echo "  PVCs deleted"
else
    echo "  No PVCs found"
fi

# Clean up any leftover cluster roles and bindings
echo "🧹 Cleaning up ClusterRoles and ClusterRoleBindings..."
kubectl delete clusterrole -l app.kubernetes.io/part-of=kube-prometheus-stack 2>/dev/null || true
kubectl delete clusterrolebinding -l app.kubernetes.io/part-of=kube-prometheus-stack 2>/dev/null || true

# Clean up specific leftovers that might remain
kubectl delete clusterrole monitoring-grafana-clusterrole 2>/dev/null || true
kubectl delete clusterrole monitoring-kube-prometheus-prometheus 2>/dev/null || true
kubectl delete clusterrole monitoring-kube-state-metrics 2>/dev/null || true
kubectl delete clusterrolebinding monitoring-grafana-clusterrolebinding 2>/dev/null || true
kubectl delete clusterrolebinding monitoring-kube-prometheus-prometheus 2>/dev/null || true
kubectl delete clusterrolebinding monitoring-kube-state-metrics 2>/dev/null || true

# Delete the namespace
echo "🗑️  Deleting monitoring namespace..."
kubectl delete namespace monitoring --wait=true --timeout=120s

echo "✅ Monitoring stack completely removed!"
echo ""
echo "📝 Note: All data including metrics, logs, and traces have been deleted."
echo "   To reinstall, run: ./install-monitoring.sh"
