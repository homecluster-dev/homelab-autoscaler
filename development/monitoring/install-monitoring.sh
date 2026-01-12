#!/bin/bash

set -e

echo "🚀 Installing Monitoring Stack (Prometheus + Grafana + Loki + Tempo)..."

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

# Verify k3d cluster (optional)
CLUSTER_NAME=$(kubectl config current-context 2>/dev/null || echo "unknown")
if [[ ! "$CLUSTER_NAME" =~ k3d ]]; then
    echo "⚠️  Warning: Current cluster context doesn't appear to be k3d: $CLUSTER_NAME"
    read -p "Continue anyway? (y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "Installation cancelled"
        exit 1
    fi
fi

# Create namespace
echo "📦 Creating monitoring namespace..."
kubectl create namespace monitoring --dry-run=client -o yaml | kubectl apply -f -

# Add Helm repositories
echo "📦 Adding Helm repositories..."
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo add grafana https://grafana.github.io/helm-charts
helm repo update

# Create values files
echo "📝 Creating values files..."

# Prometheus + Grafana values
cat > /tmp/prometheus-values.yaml <<'EOF'
# kube-prometheus-stack values for k3d development
prometheus:
  prometheusSpec:
    serviceMonitorSelectorNilUsesHelmValues: false
    replicas: 1
    resources:
      requests:
        cpu: 200m
        memory: 512Mi
      limits:
        cpu: 1000m
        memory: 1Gi
    storageSpec:
      volumeClaimTemplate:
        spec:
          storageClassName: local-path
          accessModes: ["ReadWriteOnce"]
          resources:
            requests:
              storage: 5Gi
    retention: 7d
    retentionSize: 4GB
    
    # Custom scrape configs for cluster autoscaler
    additionalScrapeConfigs:
      - job_name: 'cluster-autoscaler'
        kubernetes_sd_configs:
          - role: pod
            namespaces:
              names:
                - kube-system
                - default
        relabel_configs:
          - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
            action: keep
            regex: true
          - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_path]
            action: replace
            target_label: __metrics_path__
            regex: (.+)
          - source_labels: [__address__, __meta_kubernetes_pod_annotation_prometheus_io_port]
            action: replace
            regex: ([^:]+)(?::\d+)?;(\d+)
            replacement: $1:$2
            target_label: __address__
          - action: labelmap
            regex: __meta_kubernetes_pod_label_(.+)
          - source_labels: [__meta_kubernetes_namespace]
            action: replace
            target_label: kubernetes_namespace
          - source_labels: [__meta_kubernetes_pod_name]
            action: replace
            target_label: kubernetes_pod_name

grafana:
  enabled: true
  adminPassword: admin
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      cpu: 500m
      memory: 512Mi
  persistence:
    enabled: true
    size: 2Gi
    storageClassName: local-path
  
  # Pre-configure Loki and Tempo datasources
  additionalDataSources:
    - name: Loki
      type: loki
      access: proxy
      url: http://loki-gateway.monitoring.svc.cluster.local:80
      jsonData:
        maxLines: 1000
    - name: Tempo
      type: tempo
      access: proxy
      url: http://tempo.monitoring.svc.cluster.local:3100
      jsonData:
        tracesToLogsV2:
          datasourceUid: 'Loki'
        tracesToMetrics:
          datasourceUid: 'Prometheus'
        serviceMap:
          datasourceUid: 'Prometheus'

alertmanager:
  enabled: true
  alertmanagerSpec:
    replicas: 1
    resources:
      requests:
        cpu: 50m
        memory: 128Mi
      limits:
        cpu: 200m
        memory: 256Mi
    storage:
      volumeClaimTemplate:
        spec:
          storageClassName: local-path
          accessModes: ["ReadWriteOnce"]
          resources:
            requests:
              storage: 1Gi

# Disable node-exporter for k3d
nodeExporter:
  enabled: false

# Enable kube-state-metrics
kubeStateMetrics:
  enabled: true
  resources:
    requests:
      cpu: 50m
      memory: 128Mi
    limits:
      cpu: 200m
      memory: 256Mi
EOF

# Loki values
cat > /tmp/loki-values.yaml <<'EOF'
# Loki values for k3d development
deploymentMode: SingleBinary

loki:
  auth_enabled: false
  commonConfig:
    replication_factor: 1
  storage:
    type: 'filesystem'
    bucketNames: {}
  schemaConfig:
    configs:
      - from: 2024-01-01
        store: tsdb
        object_store: filesystem
        schema: v13
        index:
          prefix: loki_index_
          period: 24h
  
  limits_config:
    retention_period: 168h
    ingestion_rate_mb: 10
    ingestion_burst_size_mb: 20
  
  compactor:
    retention_enabled: true
    delete_request_store: filesystem

singleBinary:
  replicas: 1
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      cpu: 500m
      memory: 512Mi
  persistence:
    enabled: true
    size: 5Gi
    storageClass: local-path

# Gateway for unified endpoint
gateway:
  enabled: true
  replicas: 1
  resources:
    requests:
      cpu: 50m
      memory: 64Mi
    limits:
      cpu: 200m
      memory: 128Mi

# Enable basic monitoring
monitoring:
  selfMonitoring:
    enabled: false
  lokiCanary:
    enabled: false

# Disable unnecessary components
backend:
  replicas: 0
read:
  replicas: 0
write:
  replicas: 0
EOF

# Tempo values
cat > /tmp/tempo-values.yaml <<'EOF'
# Tempo values for k3d development
replicas: 1

tempo:
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      cpu: 500m
      memory: 512Mi
  
  storage:
    trace:
      backend: local
      local:
        path: /var/tempo/traces
  
  retention: 168h
  
  metricsGenerator:
    enabled: true
    remoteWriteUrl: "http://monitoring-kube-prometheus-prometheus.monitoring.svc:9090/api/v1/write"

persistence:
  enabled: true
  size: 5Gi
  storageClassName: local-path

service:
  type: ClusterIP

# Enable service monitors for Prometheus scraping
serviceMonitor:
  enabled: true
EOF

# Install kube-prometheus-stack
echo "🔧 Installing kube-prometheus-stack (Prometheus + Grafana)..."
helm upgrade --install monitoring prometheus-community/kube-prometheus-stack \
  --namespace monitoring \
  --values /tmp/prometheus-values.yaml \
  --wait \
  --timeout 10m

# Install Loki
echo "🔧 Installing Loki..."
helm upgrade --install loki grafana/loki \
  --version 6.33.0 \
  --namespace monitoring \
  --values /tmp/loki-values.yaml \
  --wait \
  --timeout 10m

# Install Tempo
echo "🔧 Installing Tempo..."
helm upgrade --install tempo grafana/tempo \
  --namespace monitoring \
  --values /tmp/tempo-values.yaml \
  --wait \
  --timeout 10m

# Promtail values
# Promtail values
cat > /tmp/promtail-values.yaml <<'EOF'
config:
  clients:
    - url: http://loki-gateway.monitoring.svc.cluster.local:80/loki/api/v1/push

  snippets:
    # Use the default scrapeConfigs which already handle kubernetes-pods
    # The chart provides excellent defaults for Kubernetes pod log collection
    extraRelabelConfigs:
      # Add custom labels if needed
      - source_labels: [__meta_kubernetes_pod_label_app]
        action: replace
        target_label: app

resources:
  requests:
    cpu: 50m
    memory: 128Mi
  limits:
    cpu: 200m
    memory: 256Mi

# Service account with proper permissions
serviceAccount:
  create: true

rbac:
  create: true
  pspEnabled: false

# Tolerations to run on all nodes
tolerations:
  - effect: NoSchedule
    operator: Exists
EOF

# Install Promtail
echo "🔧 Installing Promtail..."
helm upgrade --install promtail grafana/promtail \
  --namespace monitoring \
  --values /tmp/promtail-values.yaml \
  --wait \
  --timeout 5m

# Clean up promtail values
rm -f /tmp/promtail-values.yaml

# Clean up temp files
rm -f /tmp/prometheus-values.yaml /tmp/loki-values.yaml /tmp/tempo-values.yaml


echo "Installing Cluster autoscaler service monitor"
NAMESPACE="${NAMESPACE:-homelab-autoscaler-system}" kubectl apply -f ./development/monitoring/autoscalerservicemonitor.yaml

echo "✅ Monitoring stack installed successfully!"
echo ""
echo "📊 Access your services:"
echo ""
echo "Grafana Dashboard:"
echo "  kubectl port-forward -n monitoring svc/monitoring-grafana 3000:80"
echo "  URL: http://localhost:3000"
echo "  Credentials: admin / admin"
echo ""
echo "Prometheus UI:"
echo "  kubectl port-forward -n monitoring svc/monitoring-kube-prometheus-prometheus 9090:9090"
echo "  URL: http://localhost:9090"
echo ""
echo "Loki (via Grafana):"
echo "  Already configured as datasource in Grafana"
echo ""
echo "Tempo (via Grafana):"
echo "  Already configured as datasource in Grafana"
echo ""
echo "🔍 Check pod status:"
echo "  kubectl get pods -n monitoring"
echo ""
echo "📝 Note: Datasources are pre-configured in Grafana"
echo "   - Prometheus (default)"
echo "   - Loki (logs)"
echo "   - Tempo (traces)"
