# Debugging Guide

This guide helps you debug issues with the homelab-autoscaler system.

> ⚠️ **IMPORTANT**: Many issues are due to [Known Issues](known-issues.md). Check there first.

## General Debugging Approach

### 1. Check System Status

```bash
# Check if controller is running
kubectl get pods -n homelab-autoscaler-system

# Check CRD installation
kubectl get crds | grep homecluster

# Check resource status
kubectl get groups -n homelab-autoscaler-system
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
```

### 2. Examine Logs

```bash
# Controller logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager

# Follow logs in real-time
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager -f

# Get logs from specific container
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager -c manager
```

### 3. Check Resource Details

```bash
# Describe resources for detailed information
kubectl describe group <group-name> -n homelab-autoscaler-system
kubectl describe node.infra.homecluster.dev <node-name> -n homelab-autoscaler-system

# Check events
kubectl get events -n homelab-autoscaler-system --sort-by='.lastTimestamp'
```

## Component-Specific Debugging

### Controller Issues

#### Symptoms
- Resources not being reconciled
- Status not updating
- Controllers crashing or restarting

#### Debugging Steps

```bash
# Check controller pod status
kubectl get pods -n homelab-autoscaler-system -l control-plane=controller-manager

# Get detailed pod information
kubectl describe pod -n homelab-autoscaler-system -l control-plane=controller-manager

# Check controller logs for errors
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep ERROR

# Check for reconciliation loops
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep "Reconciling"
```

#### Common Issues

1. **RBAC Permissions**
   ```bash
   # Check service account
   kubectl get serviceaccount -n homelab-autoscaler-system
   
   # Check role bindings
   kubectl get rolebinding,clusterrolebinding | grep homelab-autoscaler
   ```

2. **CRD Installation**
   ```bash
   # Verify CRDs are installed
   kubectl get crd infra.homecluster.dev
   kubectl get crd groups.infra.homecluster.dev
   kubectl get crd nodes.infra.homecluster.dev
   ```

3. **Resource Validation**
   ```bash
   # Check for validation errors
   kubectl apply -f config/samples/ --dry-run=server
   ```

### gRPC Server Issues

#### Symptoms
- Cluster Autoscaler cannot connect
- gRPC methods returning errors
- Scaling operations not working

#### Debugging Steps

```bash
# Check if gRPC server is running
kubectl get pods -n homelab-autoscaler-system
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep "gRPC"

# Port forward to gRPC server
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc 50051:50051 &

# Test gRPC connectivity
grpcurl -plaintext localhost:50051 list

# Test specific methods
grpcurl -plaintext -d '{}' localhost:50051 externalgrpc.CloudProvider/NodeGroups
```

#### Known gRPC Issues

1. **NodeGroupTargetSize Logic Bug**
   - **Symptom**: Incorrect target sizes returned
   - **Location**: `internal/grpcserver/server.go:306-311`
   - **Workaround**: Manual node management

2. **NodeGroupDecreaseTargetSize Not Persisting**
   - **Symptom**: Scale-down operations appear to work but don't persist
   - **Location**: `internal/grpcserver/server.go:461-473`
   - **Workaround**: Manual power state changes

### Node Power Management Issues

#### Symptoms
- Nodes stuck in transitional states
- Power operations failing
- Jobs not completing

#### Debugging Steps

```bash
# Check node status
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system

# Check power operation jobs
kubectl get jobs -n homelab-autoscaler-system

# Check job logs
kubectl logs -n homelab-autoscaler-system job/<job-name>

# Check job pod logs
kubectl get pods -n homelab-autoscaler-system -l job-name=<job-name>
kubectl logs -n homelab-autoscaler-system <pod-name>
```

#### Common Power Issues

1. **Startup Jobs Failing**
   ```bash
   # Check startup job logs
   kubectl logs -n homelab-autoscaler-system job/node-startup-<hash>
   
   # Common issues:
   # - Network connectivity to target node
   # - Incorrect credentials
   # - Wrong MAC address for WoL
   # - IPMI/BMC not accessible
   ```

2. **Shutdown Jobs Failing**
   ```bash
   # Check shutdown job logs
   kubectl logs -n homelab-autoscaler-system job/node-shutdown-<hash>
   
   # Common issues:
   # - SSH connectivity problems
   # - Insufficient permissions
   # - Node already powered off
   # - Network timeouts
   ```

3. **Nodes Stuck in Starting Up**
   ```bash
   # Check if node actually powered on
   ping <node-ip>
   
   # Check if kubelet is running on node
   ssh <node> "systemctl status kubelet"
   
   # Check node registration
   kubectl get nodes | grep <node-name>
   ```

### Group Management Issues

#### Symptoms
- Groups not managing nodes
- Health status not updating
- Autoscaling policies not working

#### Debugging Steps

```bash
# Check group status
kubectl get groups -n homelab-autoscaler-system -o wide

# Check group conditions
kubectl describe group <group-name> -n homelab-autoscaler-system

# Check nodes in group
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -l group=<group-name>
```

#### Known Group Issues

1. **Group Controller Incomplete**
   - **Symptom**: Groups only show "Loaded" condition
   - **Cause**: Controller only implements basic status setting
   - **Workaround**: Manual node management

2. **Health Status Not Updating**
   - **Symptom**: Group health always shows "unknown"
   - **Cause**: Health calculation logic not implemented
   - **Workaround**: Check individual node health

## Diagnostic Commands

### System Overview

```bash
#!/bin/bash
# homelab-autoscaler-debug.sh - Comprehensive system check

echo "=== Homelab Autoscaler Debug Report ==="
echo "Generated: $(date)"
echo

echo "=== Namespace and Pods ==="
kubectl get pods -n homelab-autoscaler-system
echo

echo "=== CRDs ==="
kubectl get crds | grep homecluster
echo

echo "=== Groups ==="
kubectl get groups -n homelab-autoscaler-system -o wide
echo

echo "=== Nodes ==="
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -o wide
echo

echo "=== Jobs ==="
kubectl get jobs -n homelab-autoscaler-system
echo

echo "=== Recent Events ==="
kubectl get events -n homelab-autoscaler-system --sort-by='.lastTimestamp' | tail -10
echo

echo "=== Controller Logs (last 20 lines) ==="
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager --tail=20
```

### gRPC Testing

```bash
#!/bin/bash
# grpc-test.sh - Test gRPC server functionality

# Port forward to gRPC server
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc 50051:50051 &
PF_PID=$!

sleep 2

echo "=== gRPC Server Test ==="
echo "Testing connectivity..."
grpcurl -plaintext localhost:50051 list

echo
echo "Testing NodeGroups..."
grpcurl -plaintext -d '{}' localhost:50051 externalgrpc.CloudProvider/NodeGroups

echo
echo "Testing NodeGroupTargetSize..."
grpcurl -plaintext -d '{"id": "test-group"}' localhost:50051 externalgrpc.CloudProvider/NodeGroupTargetSize

# Clean up
kill $PF_PID
```

## Performance Debugging

### Resource Usage

```bash
# Check controller resource usage
kubectl top pods -n homelab-autoscaler-system

# Check node resource usage
kubectl top nodes

# Check for resource limits
kubectl describe pod -n homelab-autoscaler-system -l control-plane=controller-manager | grep -A 5 -B 5 "Limits\|Requests"
```

### Memory and CPU Analysis

```bash
# Get detailed resource metrics
kubectl get --raw /apis/metrics.k8s.io/v1beta1/namespaces/homelab-autoscaler-system/pods

# Check for memory leaks in logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep -i "memory\|oom"
```

## Network Debugging

### Connectivity Issues

```bash
# Test cluster DNS
kubectl run debug-pod --image=busybox --rm -it -- nslookup kubernetes.default

# Test service connectivity
kubectl run debug-pod --image=busybox --rm -it -- wget -qO- http://homelab-autoscaler-grpc.homelab-autoscaler-system.svc.cluster.local:50051

# Check service endpoints
kubectl get endpoints -n homelab-autoscaler-system
```

### gRPC Connectivity

```bash
# Test gRPC from within cluster
kubectl run grpc-test --image=fullstorydev/grpcurl --rm -it -- \
  grpcurl -plaintext homelab-autoscaler-grpc.homelab-autoscaler-system.svc.cluster.local:50051 list
```

## Log Analysis

### Error Patterns

```bash
# Common error patterns to search for
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep -E "(ERROR|FATAL|panic|failed)"

# Reconciliation issues
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep "reconcile"

# gRPC errors
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | grep -i grpc
```

### Structured Log Analysis

```bash
# Extract JSON logs if using structured logging
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager | jq 'select(.level == "error")'
```

## Recovery Procedures

### Restart Components

```bash
# Restart controller
kubectl rollout restart deployment/homelab-autoscaler-controller-manager -n homelab-autoscaler-system

# Force pod recreation
kubectl delete pods -n homelab-autoscaler-system -l control-plane=controller-manager
```

### Reset Resource States

```bash
# Clear finalizers if resources are stuck
kubectl patch group <group-name> -n homelab-autoscaler-system -p '{"metadata":{"finalizers":[]}}' --type=merge

# Reset node power state
kubectl patch node.infra.homecluster.dev <node-name> -n homelab-autoscaler-system \
  --type='merge' -p='{"spec":{"powerState":"off"}}'
```

### Clean Up Failed Jobs

```bash
# Remove failed jobs
kubectl delete jobs -n homelab-autoscaler-system --field-selector=status.successful=0

# Clean up completed jobs
kubectl delete jobs -n homelab-autoscaler-system --field-selector=status.successful=1
```

## When to Escalate

### Critical Issues Requiring Code Fixes

1. **gRPC Logic Bugs**: Cannot be worked around, require code changes
2. **Controller Race Conditions**: May cause data corruption
3. **Missing Node Draining**: Risk of data loss

### Issues That Can Be Worked Around

1. **Incomplete Group Controller**: Use manual node management
2. **Missing Error Handling**: Monitor and restart manually
3. **Namespace Hardcoding**: Deploy in correct namespace

## Related Documentation

- [Known Issues](known-issues.md) - Critical bugs and limitations
- [Architecture Overview](../architecture/overview.md) - System design
- [Development Setup](../development/setup.md) - Setting up for debugging
- [API Reference](../api-reference/crds/group.md) - Resource specifications