# Homelab Autoscaler E2E Test Flow

## Overview

The `make test-e2e` target runs comprehensive end-to-end integration tests against a real k3d Kubernetes cluster. It validates the complete autoscaling workflow where the homelab-autoscaler controller communicates with Kubernetes Cluster Autoscaler (CA) via gRPC to provision and deprovision nodes based on workload demand.

## Test Structure

The e2e tests consist of 3 sequential Ginkgo tests that verify the complete scale-down and scale-up workflows:

1. **Setup & Environment Validation** - Verifies prerequisites and prepares the environment
2. **Scale-Down Workflow** - Verifies CA detects unneeded nodes and triggers shutdown
3. **Scale-Up Workflow** - Verifies CA scales up when pods need scheduling

All tests share state via Ginkgo's `BeforeSuite`/`AfterSuite` and global variables.

## Architecture Components

### 1. **VM Control Server** (`examples/k3d/vm_control_server.py`)
- Simple HTTP server running on port 8080
- Exposes `/start?vm=VM_NAME` and `/stop?vm=VM_NAME` endpoints
- Executes `k3d node start/stop` commands to power k3d nodes on/off
- Simulates real VM provisioning/shutdown for k3d nodes
- Logs all requests to `/tmp/vm-control-requests.log` for test verification

### 2. **Homelab Autoscaler Controller** (`cmd/main.go`)
- Runs locally outside the cluster
- Exposes gRPC server on port 50052
- Implements the Cluster Autoscaler integrations API
- Monitors node groups and communicates scaling decisions to CA

### 3. **Kubernetes Cluster Autoscaler** (Helm chart)
- Runs in-cluster in `homelab-autoscaler-system` namespace
- Connects to local gRPC server via service forwarder
- Makes scaling decisions based on pending pods and node utilization
- Communicates with homelab-autoscaler for node lifecycle management
- Logs all gRPC interactions for test verification

### 4. **Node Custom Resources** (`examples/k3d/nodes1.yaml`)
- Define individual nodes with startup/shutdown hooks
- `startupPodSpec`: Executes curl to VM control server's `/start` endpoint when node is provisioned
- `shutdownPodSpec`: Executes curl to VM control server's `/stop` endpoint when node is decommissioned
- Organized into groups (e.g., `group1`) for collective scaling

### 5. **Service Forwarders** (created during test setup)
- **homelab-autoscaler-grpc-local**: Routes cluster traffic on port 50052 to the local gRPC server
- **host-internal**: Routes traffic on port 8080 to the VM control server running on the host

### 6. **Test Utilities** (`test/utils/utils.go`)
- Node tainting utilities (`TaintNode`, `UntaintNode`, `VerifyNodeTaint`)
- VM control server utilities (`VerifyVMControlServer`, `VerifyVMControlServerRequest`)
- gRPC verification utilities (`GetClusterAutoscalerLogs`, `VerifyGRPCCall`)
- Job verification utilities (`VerifyJobHasServiceAccount`, `VerifyJobHasVolume`, `WaitForJobCompletion`)
- Node verification utilities (`WaitForNodeState`, `WaitForNodeReady`, `WaitForK3dNodeState`)
- Pod verification utilities (`WaitForPodScheduled`)

## Test Execution Flow

### Phase 1: Cleanup (Lines 114-115 in Makefile)
```bash
lsof -i :8081 | awk '{print $$2}' | tail -n 1 | xargs kill -9 || echo "ok"
lsof -i :8080 | awk '{print $$2}' | tail -n 1 | xargs kill -9 || echo "ok"
```
- Kills any lingering processes from previous test runs on ports 8080/8081
- Ensures clean state before starting

### Phase 2: Environment Setup (Line 118)
```bash
./development/install-dev.sh
```
This script performs:
1. **Prerequisites Check**: Validates k3d, helm, kubectl are installed
2. **Cluster Creation**: Creates fresh k3d cluster named `homelab-autoscaler`
3. **Kubeconfig Setup**: Exports `./kubeconfig` for cluster access
4. **Namespace Creation**: Creates `homelab-autoscaler-system` namespace
5. **Helm Installation**: Installs cluster autoscaler with development values
6. **Service Forwarder**: Applies local service forwarder for gRPC communication

### Phase 3: Start VM Control Server (Lines 120-121)
```bash
cd examples/k3d && python3 vm_control_server.py &
```
- Starts HTTP server on port 8080
- Listens for start/stop commands for k3d nodes
- Stores PID in `/tmp/vm-control.pid` for cleanup

### Phase 4: Start Local Controller (Lines 123-127)
```bash
KUBECONFIG=./kubeconfig ENABLE_WEBHOOKS=false go run cmd/main.go --grpc-server-address :50052 &
```
- Launches homelab-autoscaler controller locally
- Binds gRPC server to port 50052
- Uses local kubeconfig to communicate with k3d cluster
- Stores PID in `/tmp/local-server.pid` for cleanup
- Webhooks disabled for local testing

### Phase 5: Run E2E Tests (Line 130)
```bash
KUBECONFIG=./kubeconfig go test -tags=e2e ./test/e2e/ -v -ginkgo.v -ginkgo.focus="K3d Integration"
```

#### Test 1: Setup & Environment Validation

**Purpose**: Verify prerequisites and prepare environment

**Steps**:
1. Verify k3d cluster exists
2. Set KUBECONFIG environment variable
3. Clear VM control server request log
4. Taint agent nodes with `node.kubernetes.io/role=agent:NoSchedule`
   - Prevents regular workloads from scheduling on agent nodes
   - Ensures clean scale-down detection
5. Verify gRPC service forwarder exists
6. Verify VM control server is accessible
7. Apply Node CRs from `examples/k3d/nodes1.yaml`
8. Verify Node CRs are created

**Verification Points**:
- k3d cluster is running
- Agent nodes exist
- Agent nodes are tainted
- gRPC service forwarder exists
- VM control server is responding
- Node CRs are created

---

#### Test 2: Scale-Down Workflow

**Purpose**: Verify Cluster Autoscaler detects unneeded nodes and triggers shutdown

**Preconditions**:
- Node CRs already applied (from Test 1)
- Agent nodes tainted (no workloads can schedule)

**Steps**:
1. Apply Group CR with aggressive scale-down settings:
   - `scaleDownUnneededTime: 30s`
   - `scaleDownUtilizationThreshold: 0.1`
   - `ignoreDaemonSetsUtilization: true`
2. Wait for Cluster Autoscaler to detect unneeded nodes (~60s)
   - Monitor CA logs for "unneeded" messages
3. Verify gRPC `NodeGroupDeleteNodes` call was made
   - Check CA logs for "DeleteNodes" call
4. Verify Node CR `spec.desiredPowerState` set to `off`
5. Verify shutdown job is created
6. Verify shutdown job has correct ServiceAccount (`controller-manager`)
7. Verify shutdown job has Secret volume (`auth-secrets`)
8. Verify VM control server receives `/stop` HTTP request
   - Requests logged to `/tmp/vm-control-requests.log`
9. Verify k3d node is stopped
10. Verify Node CR `status.progress` becomes `shutdown`
11. Verify Kubernetes node becomes NotReady

**Verification Points**:
- CA detects node as unneeded
- gRPC DeleteNodes call is made
- Shutdown job is created with correct ServiceAccount
- Shutdown job has ConfigMap and Secret volumes
- VM control server receives `/stop` request
- k3d node stops
- Node CR reaches `shutdown` state
- Kubernetes node becomes NotReady

**Shared State**: Stores shutdown node name for Test 3

---

#### Test 3: Scale-Up Workflow

**Purpose**: Verify Cluster Autoscaler scales up when pods need scheduling

**Preconditions**:
- Test 2 completed (node is shutdown)
- Shutdown state preserved (Node CR with `progress=shutdown`)

**Steps**:
1. Untaint the shutdown node to allow scheduling
2. Verify node is untainted
3. Apply hello-world deployment targeting the agent node
4. Wait for CA to detect need for scale-up
   - Check CA logs for "IncreaseSize" or "scale up"
5. Verify gRPC `NodeGroupIncreaseSize` call was made
6. Verify Node CR `spec.desiredPowerState` set to `on`
7. Verify startup job is created
8. Verify startup job has correct ServiceAccount (`controller-manager`)
9. Verify startup job has ConfigMap volume (`startup-config`)
10. Verify VM control server receives `/start` HTTP request
11. Verify k3d node starts
12. Verify startup job completes
13. Verify Node CR `status.progress` becomes `ready`
14. Verify Kubernetes node becomes Ready
15. Verify deployment pod is scheduled and running

**Verification Points**:
- Pod scheduling triggers scale-up need
- gRPC IncreaseSize call is made
- Startup job is created with correct ServiceAccount
- Startup job has ConfigMap volume
- VM control server receives `/start` request
- k3d node starts
- Startup job completes successfully
- Node CR reaches `ready` state
- Kubernetes node Ready
- Deployment pod scheduled and running

---

### Phase 6: Cleanup (Lines 132-155)

**Stop Local Controller:**
```bash
if [ -f /tmp/local-server.pid ]; then
    source /tmp/local-server.pid
    kill $SERVER_PID 2>/dev/null || true
    rm -f /tmp/local-server.pid
fi
```

**Stop VM Control Server:**
```bash
if [ -f /tmp/vm-control.pid ]; then
    source /tmp/vm-control.pid
    kill $VM_CONTROL_PID 2>/dev/null || true
    rm -f /tmp/vm-control.pid
fi
```

**Delete k3d Cluster:**
```bash
./examples/k3d/delete-cluster.sh
```
- Removes all k3d nodes and cluster resources
- Cleans up Docker containers

**Final Port Cleanup:**
```bash
lsof -i :8081 | awk '{print $$2}' | tail -n 1 | xargs kill -9 || echo "ok"
lsof -i :8080 | awk '{print $$2}' | tail -n 1 | xargs kill -9 || echo "ok"
```
- Ensures no lingering processes

## Key Integration Points

### gRPC Communication Flow
```
┌─────────────────────┐     gRPC      ┌─────────────────────────┐
│  Cluster Autoscaler │◄──────────────┤  Local Controller       │
│  (in-cluster)       │   Port 50052  │  (outside cluster)      │
└─────────┬───────────┘               └─────────────────────────┘
          │
          │ Service Forwarder
          ▼
┌─────────────────────┐
│  homelab-autoscaler │
│  grpc-local:50052   │
└─────────────────────┘
```

### Node Lifecycle Flow
```
┌────────────┐    HTTP       ┌──────────────────┐    k3d CLI    ┌─────────────┐
│ Startup    │──────────────►│ VM Control       │──────────────►│ k3d Node    │
│ Pod        │  /start?vm=X  │ Server           │  k3d node     │ (powered on)│
└────────────┘               │ (port 8080)      │    start X    └─────────────┘
                             └──────────────────┘

┌────────────┐    HTTP       ┌──────────────────┐    k3d CLI    ┌─────────────┐
│ Shutdown   │──────────────►│ VM Control       │──────────────►│ k3d Node    │
│ Pod        │   /stop?vm=X  │ Server           │   k3d node    │ (powered    │
└────────────┘               │ (port 8080)      │    stop X     └─────────────┘
                                                            (stopped)
```

## Test Validation Points

### Test 1: Setup & Environment Validation
1. **k3d cluster running**: Cluster exists and is accessible
2. **Agent nodes exist**: Both agent nodes are present
3. **Node tainting**: Agent nodes tainted with `node.kubernetes.io/role=agent:NoSchedule`
4. **gRPC forwarder exists**: Service forwarder for local controller is configured
5. **VM control server responding**: HTTP server accessible on localhost:8080
6. **Node CRs created**: Node custom resources applied successfully

### Test 2: Scale-Down Workflow
7. **CA detects unneeded nodes**: CA logs show "unneeded" detection
8. **gRPC DeleteNodes call**: CA calls `NodeGroupDeleteNodes` via gRPC
9. **desiredPowerState set to off**: Node CR spec updated correctly
10. **Shutdown job created**: Kubernetes job with correct labels
11. **Shutdown job ServiceAccount**: Uses `controller-manager` ServiceAccount
12. **Shutdown job volumes**: Has `auth-secrets` Secret volume
13. **VM control server receives /stop**: Request logged to `/tmp/vm-control-requests.log`
14. **k3d node stops**: Node status is not "running" or "up"
15. **Node CR progress=shutdown**: Status reflects shutdown completion
16. **Kubernetes node NotReady**: Node condition is False or Unknown

### Test 3: Scale-Up Workflow
17. **Node untainted**: Taint removed, allowing scheduling
18. **CA detects scale-up need**: CA logs show "IncreaseSize" or "scale up"
19. **gRPC IncreaseSize call**: CA calls `NodeGroupIncreaseSize` via gRPC
20. **desiredPowerState set to on**: Node CR spec updated correctly
21. **Startup job created**: Kubernetes job with correct labels
22. **Startup job ServiceAccount**: Uses `controller-manager` ServiceAccount
23. **Startup job volumes**: Has `startup-config` ConfigMap volume
24. **VM control server receives /start**: Request logged to `/tmp/vm-control-requests.log`
25. **k3d node starts**: Node status is "running" or "up"
26. **Startup job completes**: Job reaches succeeded state
27. **Node CR progress=ready**: Status reflects ready completion
28. **Kubernetes node Ready**: Node condition is True
29. **Deployment pod running**: hello-world pod scheduled and running

## Environment Variables

- `KUBECONFIG=./kubeconfig`: Path to k3d cluster kubeconfig
- `ENABLE_WEBHOOKS=false`: Disable webhooks for local testing

## Test Constants

Defined in `test/e2e/k3d_integration_test.go`:

```go
const (
    clusterName      = "homelab-autoscaler"
    grpcServerPort   = "50052"
    kubeconfigPath   = "./kubeconfig"
    namespace        = "homelab-autoscaler-system"
    vmControlPort    = 8080
    vmControlHost    = "localhost"
    vmControlLogFile = "/tmp/vm-control-requests.log"

    // Node names
    agentNode0 = "k3d-homelab-autoscaler-agent-0"
    agentNode1 = "k3d-homelab-autoscaler-agent-1"

    // Taints
    agentTaint = "node.kubernetes.io/role=agent:NoSchedule"

    // Timeouts
    caDetectionTimeout   = 60 * time.Second
    vmShutdownTimeout    = 60 * time.Second
    vmStartupTimeout     = 60 * time.Second
    jobCompletionTimeout = 120 * time.Second
    podScheduleTimeout   = 60 * time.Second
    nodeStateTimeout     = 120 * time.Second

    // Check intervals
    checkInterval     = 5 * time.Second
    fastCheckInterval = 1 * time.Second
)
```

## Expected Runtime

| Test | Duration | Notes |
|------|----------|-------|
| Test 1: Setup | 30s | Quick validation |
| Test 2: Scale-Down | 2.5m | CA detection (60s) + shutdown (60s) + verification |
| Test 3: Scale-Up | 2.5m | Scale-up (60s) + startup (60s) + verification |
| **Total** | **~6 minutes** | With overhead, ~8 minutes |

## Test Tags

- `-tags=e2e`: Build tag to include e2e tests
- `-ginkgo.focus="K3d Integration"`: Filter to run only K3d integration tests
- `Serial`: Tests run sequentially (not in parallel)

## Comparison: `test-e2e` vs `run-e2e`

| Aspect | `test-e2e` | `run-e2e` |
|--------|-----------|-----------|
| Cleanup | Automatic | Manual |
| Post-test state | Cluster deleted | Cluster running for inspection |
| Use case | CI/CD, full validation | Debugging, manual testing |

## Files Involved

### Test Orchestration
- `Makefile`: Test orchestration (`test-e2e`, `run-e2e` targets)
- `development/install-dev.sh`: Environment setup (cluster creation, Helm installation)
- `examples/k3d/create-cluster.sh`: Cluster creation script
- `examples/k3d/delete-cluster.sh`: Cluster deletion script

### Test Implementation
- `test/e2e/k3d_integration_test.go`: Main test file with 3 sequential tests
- `test/utils/utils.go`: Helper functions for node management, VM control, job verification

### Test Fixtures
- `examples/k3d/nodes1.yaml`: Node CR definitions (agent-0, agent-1)
- `examples/k3d/hello-world-deployment.yaml`: Test workload (nginx deployment)
- `examples/k3d/vm_control_server.py`: HTTP server for node power control

### Runtime Files
- `/tmp/vm-control-requests.log`: VM control server request log
- `/tmp/local-server.pid`: Local server PID file
- `/tmp/vm-control.pid`: VM control server PID file
- `./kubeconfig`: Cluster kubeconfig (created by install-dev.sh)
