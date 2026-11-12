# Debugging Guide

How to find out what the autoscaler is doing, and why. The symptom-first runbooks are in
[Common Issues & Solutions](common-issues.md); this page is the component-by-component method.

## General Debugging Approach

### 1. Check System Status

```bash
# Check if controller is running
kubectl get pods -n homelab-autoscaler-system

# Check CRD installation
kubectl get crds | grep homecluster

# Check resource status: the pool, its slots, and the requests that own them
kubectl get groups -n homelab-autoscaler-system
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
kubectl get nodeclaims -n homelab-autoscaler-system
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
kubectl describe nodeclaim <claim-name> -n homelab-autoscaler-system

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
   # Verify CRDs are installed. A CRD is named <plural>.<group>: there is no bare
   # `infra.homecluster.dev` resource to look up.
   kubectl get crd groups.infra.homecluster.dev
   kubectl get crd nodes.infra.homecluster.dev
   kubectl get crd nodeclaims.infra.homecluster.dev
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

# Port forward to gRPC service
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc-service 50051:50051 &

# Test gRPC connectivity
grpcurl -plaintext localhost:50051 list

# Test specific methods. The service is the proto's fully qualified name:
# package clusterautoscaler.cloudprovider.v1.externalgrpc, service CloudProvider (proto/cloud_provider.proto)
grpcurl -plaintext -d '{}' localhost:50051 clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider/NodeGroups
```

The gRPC server is one goroutine of the manager (`cmd/main.go`, `--enable-grpc-server`,
`--grpc-server-address=:50051`), so "is the gRPC server up" is "is the manager up": check
`kubectl logs … deployment/homelab-autoscaler-controller-manager | grep -i grpc` for the
`starting gRPC server` line and the per-call lines of each RPC.

### Node Power Management Issues

#### Symptoms
- Slots stuck in `pending` / `booting` / `draining` for longer than the claim's budget
- Power operations failing
- Lifecycle Jobs not completing

Every transition of a slot is an FSM pass
([State Management](../architecture/state.md)): the state is derived from the live claim, the
observed power and the usability of the node object, the coordination lock keeps the opposite
operation out of the same critical section, and the claim's deadline is the bound that ends an
attempt that lands nowhere.

#### Debugging Steps

```bash
# Check node status
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system

# Why a machine was powered on or off: the controller records every scale up/down
# transition on the Node CR (ScaleUpRequested, ScaleUpCompleted, ScaleDownRequested,
# ScaleDownCompleted; ScaleUpFailed / ScaleDownFailed as warnings; and a slot that lost
# the coordination lock to the opposite operation, which is a scheduled retry and not a
# failure, as ScaleUpDeferred / ScaleDownDeferred)
kubectl describe nodes.infra.homecluster.dev -n homelab-autoscaler-system <node-cr-name>
kubectl get events -n homelab-autoscaler-system --field-selector involvedObject.name=<node-cr-name>

# Check power operation jobs
kubectl get jobs -n homelab-autoscaler-system

# Only the jobs created for one Node CR (startup + shutdown), and their pods
kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-cr-name>
kubectl get pods -n homelab-autoscaler-system -l infra.homecluster.dev/node=<node-cr-name>

# Find the Node CR behind a job: check its labels/annotations, or its owner reference
kubectl get job -n homelab-autoscaler-system <job-name> -o jsonpath='{.metadata.labels}{"\n"}{.metadata.annotations}{"\n"}'

# Check job logs
kubectl logs -n homelab-autoscaler-system job/<job-name>

# Check job pod logs
kubectl get pods -n homelab-autoscaler-system -l job-name=<job-name>
kubectl logs -n homelab-autoscaler-system <pod-name>
```

#### Common Power Issues

1. **Startup Jobs Failing**
   ```bash
   # The Job of one slot, and its logs. A lifecycle Job is named <slot>-startup-<unix>.
   kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<slot>,infra.homecluster.dev/phase=startup
   kubectl logs -n homelab-autoscaler-system job/<slot>-startup-<unix>
   
   # What the claim recorded: reason StartupJobFailed / StartupJobTimeout, and the attempt number
   kubectl describe nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<slot>
   
   # Common issues:
   # - Network connectivity from the Job's pod to the power API (a Job runs in the cluster, so it
   #   reaches the BMC/PDU/SSH endpoint the same way a pod does)
   # - Credentials: a Secret volume that is not mounted, a Secret that is not in the slot's namespace,
   #   and a ServiceAccount without the RBAC the script needs
   # - Wrong MAC address for wake-on-LAN, no broadcast route out of the pod's network
   # - IPMI/BMC not accessible
   ```

2. **Shutdown Jobs Failing**
   ```bash
   kubectl get jobs -n homelab-autoscaler-system -l infra.homecluster.dev/node=<slot>,infra.homecluster.dev/phase=shutdown
   kubectl logs -n homelab-autoscaler-system job/<slot>-shutdown-<unix>
   ```
   A shutdown Job that exits 0 is not evidence the machine is off: the controller reads the machine,
   the slot keeps reporting `draining`, and the attempts are counted on the scale-down claim. That
   runbook, with the escape hatch for a spent streak, is
   [The Shutdown Job Exits 0 and the Machine Stays Up](common-issues.md#the-shutdown-job-exits-0-and-the-machine-stays-up).

3. **A Slot Stuck `booting`**
   ```bash
   # The claim owns the boot: its phase, reason and budget are the answer
   kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<slot> -o wide
   
   # Check if the machine actually powered on, and whether the kubelet registered
   ping <node-ip>
   ssh <node> "systemctl status kubelet"
   kubectl get node <kubernetes-node-name>
   
   # Check if the readiness gate is what is open: the key the slot is waiting for is a condition
   kubectl describe node.infra.homecluster.dev <slot> -n homelab-autoscaler-system
   kubectl get node <kubernetes-node-name> -o jsonpath='{.spec.taints}' ; echo
   ```
   The boot is bounded: `spec.deadlineAt` of the claim (12m10s, floored by the Group's
   `spec.maxNodeProvisionTime`) fails the attempt and powers the machine off, and the readiness gate
   has its own 4m. Nothing wedges a slot while you read it.

### Group Management Issues

#### Symptoms
- A pool whose counts do not move
- Health status `offline`
- Autoscaling policies not working

#### Debugging Steps

```bash
# Check group status: slots / on / off / last scale / health
kubectl get groups -n homelab-autoscaler-system

# Check group conditions, and the sentence of the last scale
kubectl describe group <group-name> -n homelab-autoscaler-system

# Check nodes in group
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -l group=<group-name>
```

#### Reading the Group's Report

1. **`HEALTH` is derived, not a probe**
   - `offline` means no slot of the pool has ever reported a phase: the pool has no slots (the
     `group` label matches no `Node` CR), or nothing has reconciled them
   - `healthy` means at least one slot has reported. A pool whose every machine is `off` is a healthy
     pool that is asleep, which is what a pool is for
   - `SLOTS` is the number of `Node` CRs labelled `group: <name>`: the pool's max size. If it is not
     the number of machines you declared, the label is the problem, not the controller

2. **The pool does not act on its status**
   - The counts are a display sum. What cluster-autoscaler sees, and what the model acts on, is the
     live claims and the slots: `kubectl get nodeclaims -n homelab-autoscaler-system -l group=<name>`
   - The `Loaded` condition says the group controller read the object; the pool's requests are
     objects, so "the policy is not doing anything" is a question about the autoscaler's simulation,
     and about what the group's template advertises: see
     [A Pod Needs A Slot With A Specific Label](common-issues.md#a-pod-needs-a-slot-with-a-specific-label)

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
kubectl get groups -n homelab-autoscaler-system
echo

echo "=== Slots ==="
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
echo

echo "=== Requests (claims) ==="
kubectl get nodeclaims -n homelab-autoscaler-system -o wide
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

# Port forward to the gRPC service
kubectl port-forward -n homelab-autoscaler-system service/homelab-autoscaler-grpc-service 50051:50051 &
PF_PID=$!

sleep 2

echo "=== gRPC Server Test ==="
echo "Testing connectivity..."
grpcurl -plaintext localhost:50051 list

echo
echo "Testing NodeGroups..."
grpcurl -plaintext -d '{}' localhost:50051 clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider/NodeGroups

echo
echo "Testing NodeGroupGetOptions..."
grpcurl -plaintext -d '{"id": "group1"}' localhost:50051 clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider/NodeGroupGetOptions

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

# Test a TCP path to the gRPC service (it speaks HTTP/2, so probe the port, do not GET it)
kubectl run debug-pod --image=busybox --rm -it -- \
  nc -zv homelab-autoscaler-grpc-service.homelab-autoscaler-system.svc.cluster.local 50051

# Check service endpoints: an empty ENDPOINTS list is a Service whose selector matches no ready pod
kubectl get endpoints -n homelab-autoscaler-system
```

### gRPC Connectivity

```bash
# Test gRPC from within the cluster
kubectl run grpc-test --image=fullstorydev/grpcurl --rm -it -- \
  grpcurl -plaintext homelab-autoscaler-grpc-service.homelab-autoscaler-system.svc.cluster.local:50051 list
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

### Power the Machine Off

There is no field to patch. A machine is wanted exactly when a live `NodeClaim` names its slot, so the
lever is the claim that owns it:

```bash
# 1. The claims of the slot: pending, booting and ready are the live ones
kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=<slot> -o wide

# 2. Delete the live claim. A pending claim is deleted outright - nothing is in flight. A booting or
#    ready claim carries the controller's finalizer, so the delete is the shutdown request: cordon,
#    shutdown Job, power off, and the claim ends `completed` when the machine is observed off.
kubectl delete nodeclaim <claim-name> -n homelab-autoscaler-system

# 3. Watch it land
kubectl get nodes.infra.homecluster.dev <slot> -n homelab-autoscaler-system
```

A shutdown that never lands - the Job exits 0 and the provider still observes the machine up - is
bounded: three shutdown Jobs, then the streak is spent and the controller pauses for
`claim.ShutdownAttemptResetGrace` (30 minutes) before firing again. Do not wait for the grace. Clear
the spent streak on the claim's status, and the next pass of the slot reconciler fires the shutdown:

```bash
kubectl patch nodeclaim <claim-name> -n homelab-autoscaler-system \
  --subresource=status --type=merge \
  -p '{"status":{"shutdownAttempts":0,"lastShutdownAttemptAt":null}}'
```

The same commands, with the causes listed in order, are in
[The Shutdown Job Exits 0 and the Machine Stays Up](common-issues.md#the-shutdown-job-exits-0-and-the-machine-stays-up).

### Clear a Stuck Slot

A slot is not a state to reset: its report is derived from the live claim, the observed power and the
node object, and every pass recomputes it. Ask for a pass, and read what the triple says:

```bash
kubectl annotate node.infra.homecluster.dev <slot> -n homelab-autoscaler-system \
  e2e.homecluster.dev/pass-requested-at=$(date -u +%FT%TZ) --overwrite
kubectl get nodes.infra.homecluster.dev <slot> -n homelab-autoscaler-system -o wide
kubectl describe node.infra.homecluster.dev <slot> -n homelab-autoscaler-system
```

### Clean Up Failed Jobs

```bash
# Remove failed jobs
kubectl delete jobs -n homelab-autoscaler-system --field-selector=status.successful=0

# Clean up completed jobs
kubectl delete jobs -n homelab-autoscaler-system --field-selector=status.successful=1
```

## When to Escalate

Open an issue with the output of the [diagnostic script](#system-overview) when the model's bounds
are not the answer:

- **A lifecycle Job that cannot be made to work.** `spec.startupPodSpec` / `spec.shutdownPodSpec` are
  your scripts and your image: the controller creates the Job and reads the machine. A script that
  cannot reach its power API is a network and credentials question, not an operator bug.
- **A CNI that never clears the taint the pool's readiness gate waits for.** The gate times out at 4m,
  uncordons, and fails the claim with `ReadinessGateTimeout`; a pool that loses every boot this way
  has a CNI, or a `spec.nodeReadinessTaints` list that names a key its CNI never removes.
- **A `kubernetesNodeName` collision.** Admission refuses a second slot that names the same
  `corev1.Node`, and names the holder. Two slots, one machine is a declaration to fix in Git.
- **A claim that will not end.** A claim whose machine is deleted out of band is bounded by
  `MachineGone` after one boot window; a claim that stays live forever, with the machine observed off
  and no node object, is a controller bug, and `kubectl describe nodeclaim` plus the manager's log for
  that claim is what an issue needs.

What is not an escalation: an attempt that failed at its deadline, a slot that stopped buying machines
after three attempts, a shutdown paused after three Jobs, a machine the input law powers off because
no claim wants it. Those are the bounds doing their job, and each has a runbook in
[Common Issues & Solutions](common-issues.md).

## Related Documentation

- [Common Issues & Solutions](common-issues.md) - the symptom-first runbooks
- [Known Issues](known-issues.md) - what is bounded, and what is not configurable
- [Architecture Overview](../architecture/overview.md) - System design
- [State Management](../architecture/state.md) - the input law, the phase table, the deadlines
- [Development Setup](../development/setup.md) - Setting up for debugging
- [API Reference](../api-reference/crds/group.md) - Resource specifications