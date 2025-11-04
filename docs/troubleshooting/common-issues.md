# Common Issues & Solutions

Quick reference for resolving frequently encountered problems with homelab-autoscaler.

## 🔍 Quick Diagnostics

### Controller Not Starting
```bash
# Check if CRDs are installed
kubectl get crds | grep homecluster

# Check pod status
kubectl get pods -n homelab-autoscaler-system

# Check logs for errors
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager
```

**Common Fixes:**
- Run `make install` to install CRDs
- Ensure you're using the correct namespace: `homelab-autoscaler-system`

### gRPC Connection Issues
```bash
# Check if gRPC service is running
kubectl get svc -n homelab-autoscaler-system | grep grpc

# Test gRPC connectivity
kubectl run grpc-test --rm -it --image=nicolaka/netshoot -- \
  grpc_cli call homelab-autoscaler-grpc:50051 NodeGroups ""
```

**Common Fixes:**
- Verify gRPC service is enabled in Helm values
- Check network policies if using in production

## 🛠️ Common Operations

### Manual Node Power Control
```bash
# Power on a specific node
kubectl patch node.infra.homecluster.dev <node-name> \
  --type='merge' -p='{"spec":{"powerState":"on"}}'

# Power off a specific node
kubectl patch node.infra.homecluster.dev <node-name> \
  --type='merge' -p='{"spec":{"powerState":"off"}}'

# Force status refresh
kubectl annotate node.infra.homecluster.dev <node-name> \
  homelab-autoscaler.dev/force-reconcile=$(date +%s)
```

### Stuck Job Cleanup
```bash
# List all jobs in the namespace
kubectl get jobs -n homelab-autoscaler-system

# Delete failed jobs
kubectl delete jobs -n homelab-autoscaler-system --field-selector=status.successful=0

# Clean up completed jobs
kubectl delete jobs -n homelab-autoscaler-system --field-selector=status.successful=1
```

### Resource Inspection
```bash
# Get detailed resource status
kubectl describe group <group-name> -n homelab-autoscaler-system
kubectl describe node.infra.homecluster.dev <node-name> -n homelab-autoscaler-system

# Check resource conditions
kubectl get group <group-name> -n homelab-autoscaler-system -o jsonpath='{.status.conditions}'
kubectl get node.infra.homecluster.dev <node-name> -n homelab-autoscaler-system -o jsonpath='{.status.conditions}'
```

## ⚡ Performance Issues

### High Resource Usage
```bash
# Check controller resource usage
kubectl top pods -n homelab-autoscaler-system

# Monitor API server calls
kubectl get --raw /metrics | grep apiserver_request_total
```

**Tuning Options:**
- Adjust reconciliation intervals in Helm values
- Increase resource limits for controller
- Reduce log verbosity if debug logging is enabled

### Slow Scaling Operations
```bash
# Check operation timing in logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep -i "duration\|timeout"

# Monitor job execution times
kubectl get jobs -n homelab-autoscaler-system -o wide
```

**Optimizations:**
- Tune job timeouts in Node CRD specifications
- Optimize power operation scripts for faster execution
- Consider higher resource limits for power operation jobs

## 🐛 Debugging Specific Scenarios

### Nodes Not Powering On/Off
1. Check job status: `kubectl describe job <job-name>`
2. Verify power operation scripts work manually
3. Check node power state: `kubectl describe node.infra.homecluster.dev <node-name>`

### Autoscaling Not Triggering
1. Verify resource utilization metrics
2. Check group scaling thresholds
3. Ensure gRPC server is responding to Cluster Autoscaler

### Resource State Mismatch
1. Run manual sync: Annotate resource with `homelab-autoscaler.dev/force-reconcile`
2. Check Core controller logs for sync issues
3. Verify Kubernetes node exists and is healthy

## 📋 Emergency Procedures

### Complete System Reset
```bash
# Delete all custom resources (CAUTION: Destructive!)
kubectl delete groups --all -n homelab-autoscaler-system
kubectl delete nodes.infra.homecluster.dev --all -n homelab-autoscaler-system

# Restart controller
kubectl rollout restart deployment/homelab-autoscaler-controller-manager -n homelab-autoscaler-system
```

### Database Corruption Recovery
```bash
# Export current state for backup
kubectl get groups -n homelab-autoscaler-system -o yaml > groups-backup.yaml
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -o yaml > nodes-backup.yaml

# Restore from backup
kubectl apply -f groups-backup.yaml
kubectl apply -f nodes-backup.yaml
```

---

For more detailed debugging, see the [Debugging Guide](debugging-guide.md).
Report persistent issues on [GitHub Issues](https://github.com/homecluster-dev/homelab-autoscaler/issues).