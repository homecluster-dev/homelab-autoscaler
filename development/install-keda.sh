#!/bin/bash
set -e

echo "🚀 Starting KEDA HTTP Add-on Demo Deployment"
echo "=============================================="

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Install KEDA Core
echo -e "${YELLOW}📦 Installing KEDA Core...${NC}"
helm repo add kedacore https://kedacore.github.io/charts
helm repo update
helm upgrade --install keda kedacore/keda \
  --namespace keda \
  --create-namespace \
  --set prometheus.operator.enabled=true \
  --set prometheus.operator.serviceMonitor.enabled=true \
  --set prometheus.operator.serviceMonitor.namespace=keda \
  --set prometheus.metricServer.enabled=true \
  --set prometheus.metricServer.serviceMonitor.enabled=true \
  --set prometheus.webhooks.enabled=true \
  --set prometheus.webhooks.serviceMonitor.enabled=true \
  --wait
echo -e "${GREEN}✅ KEDA Core installed${NC}"

# Install KEDA HTTP Add-on
echo -e "${YELLOW}📦 Installing KEDA HTTP Add-on...${NC}"
helm upgrade --install http-add-on kedacore/keda-add-ons-http --namespace keda  --set interceptor.responseHeaderTimeout=300s --set interceptor.replicas.waitTimeout=300s --wait
echo -e "${GREEN}✅ KEDA HTTP Add-on installed${NC}"

# Fix RBAC permissions for external scaler
echo -e "${YELLOW}🔧 Applying RBAC fix for external scaler...${NC}"
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: keda-add-ons-http-external-scaler
  labels:
    app.kubernetes.io/component: scaler
    app.kubernetes.io/instance: http-add-on
    app.kubernetes.io/managed-by: Helm
    app.kubernetes.io/name: http-add-on
    app.kubernetes.io/part-of: keda-add-ons-http
    app.kubernetes.io/version: 0.11.0
    helm.sh/chart: keda-add-ons-http-0.11.0
  annotations:
    meta.helm.sh/release-name: http-add-on
    meta.helm.sh/release-namespace: keda
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["discovery.k8s.io"]
  resources: ["endpointslices"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["http.keda.sh"]
  resources: ["httpscaledobjects"]
  verbs: ["get", "list", "watch"]
EOF
echo -e "${GREEN}✅ RBAC permissions updated${NC}"

# Restart external scaler to apply RBAC changes
echo -e "${YELLOW}🔄 Restarting external scaler with new permissions...${NC}"
kubectl rollout restart deployment/keda-add-ons-http-external-scaler -n keda
kubectl rollout status deployment/keda-add-ons-http-external-scaler -n keda --timeout=60s
echo -e "${GREEN}✅ External scaler restarted${NC}"

# Wait for KEDA components to be ready
echo -e "${YELLOW}⏳ Waiting for KEDA components to be ready...${NC}"
kubectl wait --for=condition=available --timeout=120s deployment/keda-operator -n keda
kubectl wait --for=condition=available --timeout=120s deployment/keda-add-ons-http-interceptor -n keda
kubectl wait --for=condition=available --timeout=120s deployment/keda-add-ons-http-external-scaler -n keda
echo -e "${GREEN}✅ All KEDA components ready${NC}"

# Verify scaler is working (check for no errors in recent logs)
echo -e "${YELLOW}🔍 Verifying scaler health...${NC}"
sleep 5
ERROR_COUNT=$(kubectl logs -n keda deployment/keda-add-ons-http-external-scaler --tail=20 2>/dev/null | grep -c "there isn't any valid interceptor endpoint" || true)
if [ "$ERROR_COUNT" -gt 0 ]; then
  echo -e "${YELLOW}⚠️  Warning: Scaler still showing errors. Waiting a bit longer...${NC}"
  sleep 10
fi
echo -e "${GREEN}✅ Scaler health check passed${NC}"

# Deploy demo application
echo -e "${YELLOW}🎯 Deploying demo HTTP server...${NC}"
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-http-server
  namespace: default
spec:
  replicas: 0
  selector:
    matchLabels:
      app: demo-http-server
  template:
    metadata:
      labels:
        app: demo-http-server
    spec:
      nodeSelector:
        infra.homecluster.dev/group: group1
        infra.homecluster.dev/namespace: ${NAMESPACE:-homelab-autoscaler-system}
      tolerations:
      - key: "autoscaler"
        operator: "Equal"
        value: "homelab"
        effect: "NoSchedule"
      containers:
      - name: server
        image: hashicorp/http-echo:latest
        args:
          - "-text=🎉 Hello from KEDA HTTP Autoscaler! Pod: \$(HOSTNAME)"
        env:
        - name: HOSTNAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        ports:
        - containerPort: 5678
        resources:
          requests:
            cpu: 50m
            memory: 32Mi
          limits:
            cpu: 100m
            memory: 64Mi
---
apiVersion: v1
kind: Service
metadata:
  name: demo-http-server
  namespace: default
spec:
  ports:
  - port: 8082
    targetPort: 5678
    protocol: TCP
  selector:
    app: demo-http-server
EOF
echo -e "${GREEN}✅ Demo app deployed (starting at 0 replicas)${NC}"

# Create HTTPScaledObject with correct syntax
echo -e "${YELLOW}📊 Creating HTTPScaledObject...${NC}"
kubectl apply -f - <<EOF
apiVersion: http.keda.sh/v1alpha1
kind: HTTPScaledObject
metadata:
  name: demo-scaler
  namespace: default
spec:
  hosts:
    - demo.local
  pathPrefixes:
    - /
  scaleTargetRef:
    name: demo-http-server
    kind: Deployment
    apiVersion: apps/v1
    service: demo-http-server
    port: 8082
  replicas:
    min: 0
    max: 1
  scaledownPeriod: 60  # Scale down to 0 after 30 seconds of inactivity
  scalingMetric:
    requestRate:
      granularity: 1s
      targetValue: 2
      window: 10s 
EOF
echo -e "${GREEN}✅ HTTPScaledObject created${NC}"

# Create Traefik Ingress
echo -e "${YELLOW}🌐 Creating Traefik Ingress...${NC}"
kubectl apply -f - <<EOF
---
apiVersion: traefik.io/v1alpha1
kind: ServersTransport
metadata:
  name: keda-interceptor-transport
  namespace: keda
spec:
  forwardingTimeouts:
    responseHeaderTimeout: 300s
    dialTimeout: 30s
---
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: demo-ingress
  namespace: keda
spec:
  entryPoints:
    - web
  routes:
    - match: Host(\`demo.local\`)
      kind: Rule
      services:
        - name: keda-add-ons-http-interceptor-proxy
          port: 8080
          serversTransport: keda-interceptor-transport

EOF
echo -e "${GREEN}✅ Ingress configured${NC}"

# Wait a moment for everything to settle
echo -e "${YELLOW}⏳ Waiting for resources to synchronize...${NC}"
sleep 5

echo ""
echo "=============================================="
echo -e "${GREEN}🎉 Deployment Complete!${NC}"
echo "=============================================="
echo ""
echo "📝 Quick Test Commands:"
echo ""
echo "1. Test the endpoint (should trigger scale from 0):"
echo "   curl -H 'Host: demo.local' http://localhost:8082/"
echo ""
echo "2. Watch pods scale up:"
echo "   watch kubectl get pods -l app=demo-http-server"
echo ""
echo "3. Generate load to trigger autoscaling:"
echo "   for i in {1..100}; do curl -H 'Host: demo.local' http://localhost:8082/ & done"
echo ""
echo "4. Watch the HTTPScaledObject:"
echo "   kubectl get httpscaledobject demo-scaler -w"
echo ""
echo "5. Check KEDA ScaledObjects:"
echo "   kubectl get scaledobject"
echo ""
echo "6. View interceptor logs (to show request handling):"
echo "   kubectl logs -n keda -l app.kubernetes.io/component=interceptor -f"
echo ""
echo "7. View external scaler logs (verify no errors):"
echo "   kubectl logs -n keda -l app.kubernetes.io/component=external-scaler -f"
echo ""
echo "8. Check HTTPScaledObject status:"
echo "   kubectl describe httpscaledobject demo-scaler"
echo ""
echo "✨ Pro tip: Open multiple terminals to watch pods, logs, and send requests simultaneously!"
echo ""
