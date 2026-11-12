# Homelab Autoscaler E2E Test Flow

## Overview

The `make test-e2e` target runs comprehensive end-to-end integration tests against a real k3d Kubernetes cluster. It validates the complete autoscaling workflow where the homelab-autoscaler controller communicates with Kubernetes Cluster Autoscaler (CA) via gRPC to provision and deprovision nodes based on workload demand.

## Test Structure

The e2e tests consist of 3 sequential Ginkgo tests that verify the complete scale-down and scale-up workflows:

1. **Setup & Environment Validation** - Verifies prerequisites, prepares the environment, and claims
   the machine the scale-down spec runs on
2. **Scale-Down Workflow** - Verifies CA decides to remove a machine and the model powers it off
3. **Scale-Up Workflow** - Verifies CA scales up when pods need scheduling

All tests share state via Ginkgo's `BeforeSuite`/`AfterSuite` and global variables.

The suite is the provider's counterpart: the pool's machines are k3d containers that only the VM control
server powers, so a simulated machine that is in service has a live `NodeClaim` naming its slot, and a
slot nobody claims is a machine the pool does not want. `claimSlot` and `releaseSlot` in
`test/e2e/k3d_integration_test.go` are the harness playing that half of the model.

## Architecture Components

### 1. **VM Control Server** (`examples/k3d/vm_control_server.py`)
- Simple HTTP server running on port 9052
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
- **host-internal**: Routes traffic on port 9052 to the VM control server running on the host

### 6. **Test Utilities** (`test/utils/utils.go`)
- Node tainting utilities (`TaintNode`, `UntaintNode`, `VerifyNodeTaint`)
- VM control server utilities (`VerifyVMControlServer`, `VerifyVMControlServerRequest`)
- gRPC verification utilities (`GetClusterAutoscalerLogs`, `VerifyGRPCCall`)
- Job verification utilities (`VerifyJobHasServiceAccount`, `VerifyJobHasVolume`, `WaitForJobCompletion`)
- Slot and node verification utilities (`WaitForSlotPhase`, `WaitForNodeReady`, `WaitForK3dNodeState`)
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
- Starts HTTP server on port 9052
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
7. Apply Node CRs from `examples/k3d/nodes1.yaml`, and the Group CR with the pool's scale-down numbers
8. Verify the slots are free: no live `NodeClaim` names them, and the machines nobody claimed are
   observed coming down (`draining`, then `off`) - a machine is wanted exactly when a live claim names
   its slot
9. **Claim** the slot of the machine the scale-down spec runs on: the harness writes a `NodeClaim`
   (`request.call: IncreaseSize`, the group, a delta, `requestedBy: e2e`) and follows it to
   `status.phase: ready`. The pool's machines are k3d containers the suite controls, so the suite is the
   provider's counterpart: it asks for a machine the way the autoscaler does, and the operator boots it

**Verification Points**:
- k3d cluster is running
- Agent nodes exist
- Agent nodes are tainted
- gRPC service forwarder exists
- VM control server is responding
- Node CRs are created, and they declare configuration only
- The slots are free, and the unclaimed machines report `draining`/`off`
- The claimed slot reaches `ready`: the claim is stamped ready by the readiness gate, the VM control
  server was asked for the machine, the slot reports `ready` and the machine is observed `on`

---

#### Test 2: Scale-Down Workflow

**Purpose**: Verify Cluster Autoscaler detects unneeded nodes and triggers shutdown

**Preconditions**:
- The machine the spec runs on is in service because the setup's `NodeClaim` names its slot: the node is
  Ready, the slot reports `ready`, and the pool has an empty node for the autoscaler to consider
- Agent nodes are tainted, so no workload lands on them

**Steps**:
1. Wait for Cluster Autoscaler to *decide*, reading the line its actuator writes as it taints the node
   and starts the deletion: `Scale-down: removing empty node "<name>"` (or the drain line), naming this
   machine. The word "unneeded" is not consulted: the autoscaler prints it as bookkeeping on every pass
   (`--scale-down-unneeded-time` in its flag echo, `"<node> was unneeded for 3s"` from the candidate
   tracker), so a gate built on it is true when nothing was decided
2. Verify gRPC `NodeGroupDeleteNodes` reached the provider, and that the provider's log records the
   cancellation of the claim of *this* machine
3. Verify the claim is cancelled: the request is recorded on it (`call: DeleteNodes`, `nodes:` naming the
   machine) and it carries the mark that runs the shutdown
4. Verify the slot reports `draining`: the machine is observed up and a scale-down names its claim
5. Verify the shutdown job of this machine, created after the decision, is the job the slot declares:
   ServiceAccount `controller-manager`, the `auth-secrets` Secret volume, a
   `ttlSecondsAfterFinished`, and a completion read off `job.status.succeeded`
6. Verify VM control server receives `/stop` for this machine within a window, not somewhere in the
   history of the run
7. Verify k3d node is stopped
8. Verify the slot's derived `status.phase` is `off` and `status.powerState` is `off`
9. Verify the claim stops being live (its finalizer is released once the machine is observed off), and
   the slot's digest of the episode records a terminal phase
10. Verify Kubernetes node becomes NotReady

**Verification Points**:
- CA's decision is in its own log, naming this machine, and the RPC reached the provider
- The claim that wanted the machine carries the DeleteNodes request and the mark
- The slot's report is the machine coming down, and then off; the observed power is off
- The shutdown job of this machine, from this decision, has the ServiceAccount, the Secret volume and
  the TTL the slot declares, and it completed
- VM control server receives `/stop` request; k3d node stops
- Slot reports `off`, and no live claim owns it; the digest is a terminal phase
- Kubernetes node becomes NotReady

**Shared State**: Stores shutdown node name for Test 3

---

#### Test 3: Scale-Up Workflow

**Purpose**: Verify Cluster Autoscaler scales up when pods need scheduling

**Preconditions**:
- Test 2 completed (node is shutdown)
- Slot is off (`Node.status.phase: off`) and free: no live `NodeClaim` names it

**Steps**:
1. Untaint the shutdown node to allow scheduling
2. Verify node is untainted
3. Apply hello-world deployment targeting the agent node
4. Wait for CA to *decide* to grow the pool, reading the line the orchestrator writes as it calls the
   provider: `Scale-up: setting group group1 size to N`. "scale up" alone is printed on every idle pass
   ("no need to scale up"), so it cannot gate on a decision
5. Verify gRPC `NodeGroupIncreaseSize` call was made
6. Verify the provider created a `NodeClaim` for the slot, carrying the request (call, delta, group,
    `requestedBy`), the deadline (the budget of one attempt, floored by the group's
    `maxNodeProvisionTime`), the seeded `status.attempt`, the TTL and the ownerReference
7. Verify the startup job of this attempt, on this slot, is the job the slot declares:
   ServiceAccount `controller-manager`, the `startup-config` volume, a TTL, and `job.status.succeeded`
8. Verify VM control server receives `/start` for this machine, and the k3d node starts
9. Verify the claim moves `pending` -> `booting` -> `ready`, and the slot reports `ready` with
    `status.powerState: on`
10. Relax the pool's numbers (a long `scaleDownUnneededTime`, and a `maxNodeProvisionTime` that outlasts
    a k3d boot) so the autoscaler keeps the machine it just bought in service while the spec observes it
11. Watch the served claim across its own boot deadline: the claim's deadline is stamped into the past,
    and the window is measured from the claim's natural deadline so the observation crosses it. The
    claim stays `ready`, the slot stays `ready`, no shutdown Job appears, the node stays schedulable
12. Verify the deployment pod is scheduled on the machine the claim bought

**Verification Points**:
- Pod scheduling triggers the autoscaler's scale-up decision, naming the pool
- gRPC IncreaseSize call is made, and the provider's claim is the record of the request
- The startup job of this attempt has the ServiceAccount and the ConfigMap volume the slot declares,
  carries a TTL, and completed
- VM control server receives `/start` request; k3d node starts
- The claim reaches `ready` and the slot reports `ready`
- A ready claim that crosses its boot deadline keeps its machine (the window outlives the budget:
  the operator's `claim.AttemptBudget`, 12m10s, and the group's number only raises it)
- Kubernetes node Ready, uncordoned
- Deployment pod scheduled and running on the claimed machine

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
└────────────┘               │ (port 9052)      │    start X    └─────────────┘
                             └──────────────────┘

┌────────────┐    HTTP       ┌──────────────────┐    k3d CLI    ┌─────────────┐
│ Shutdown   │──────────────►│ VM Control       │──────────────►│ k3d Node    │
│ Pod        │   /stop?vm=X  │ Server           │   k3d node    │ (powered    │
└────────────┘               │ (port 9052)      │    stop X     └─────────────┘
                                                            (stopped)
```

## Test Validation Points

### Test 1: Setup & Environment Validation
1. **k3d cluster running**: Cluster exists and is accessible
2. **Agent nodes exist**: Both agent nodes are present
3. **Node tainting**: Agent nodes tainted with `node.kubernetes.io/role=agent:NoSchedule`
4. **gRPC forwarder exists**: Service forwarder for local controller is configured
5. **VM control server responding**: HTTP server accessible on the forwarded port
6. **Node CRs created**: Node custom resources (`v2alpha1`) applied successfully, declaring
   configuration only
7. **Slots are free**: no live `NodeClaim` names a slot, and an agent machine that is up with no claim
   is reported `draining`, then `off` - the law "a machine is wanted exactly when a live claim names the
   slot" is what makes the idle pool of a homelab readable
8. **The harness claims a machine**: a `NodeClaim` written by the suite (`requestedBy: e2e`,
   `request.call: IncreaseSize`) boots its k3d machine through the operator and reaches `ready`; the
   slot reports `ready`, the machine is observed `on`, and the VM control server saw `/start` for it

### Test 2: Scale-Down Workflow
7. **CA decided, and said so**: the autoscaler's actuator line `Scale-down: removing empty node
   "<machine>"` is in its log - not the word "unneeded", which it prints on every pass
8. **gRPC DeleteNodes call**: CA calls `NodeGroupDeleteNodes`, and the provider logs cancelling the
   claim of this machine
9. **The claim is cancelled**: it carries `request.call: DeleteNodes` with the machine named, and the
   mark that runs the shutdown; the slot with no live claim is asserted after the machine is off
10. **Slot reports draining**: the machine is observed up while its claim is named for deletion
11. **Shutdown job of this machine, from this decision**: scoped by the slot label and by the minute the
    spec started watching
12. **Shutdown job ServiceAccount / volume / TTL**: `controller-manager`, `auth-secrets`, and a
    `ttlSecondsAfterFinished`, read off the typed Job
13. **VM control server receives /stop**: request logged within a window, not somewhere in the history
14. **k3d node stops**: node status is not "running"
15. **Slot phase=off, observed off**: `Node.status.phase` is `off` and `status.powerState` is `off`,
    and the slot's digest of the last claim records a terminal phase
16. **Kubernetes node NotReady**: Node condition is False or Unknown

### Test 3: Scale-Up Workflow

17. **Node untainted**: Taint removed, allowing scheduling
18. **CA decided to grow the pool**: the orchestrator's line `Scale-up: setting group group1 size to N`
    is in its log - not "scale up", which the idle pass prints
19. **gRPC IncreaseSize call**: CA calls `NodeGroupIncreaseSize` via gRPC
20. **NodeClaim created for the slot**: the scale-up is an object, with its attribution
    (`request.call: IncreaseSize`, `request.group`, `request.delta`, `spec.requestedBy`), its
    deadline, its seeded `status.attempt`, its TTL and its ownerReference on the slot
21. **Startup job of this attempt, on this slot**: scoped by the slot label and by the minute the spec
    started watching
22. **Startup job ServiceAccount / volume / TTL**: `controller-manager` and `startup-config`, read off
    the typed Job
23. **VM control server receives /start**: within a window, so the boot of the setup's machine is not
    the request this spec reads
24. **k3d node starts**: node status is "running"
25. **Startup job completes**: `job.status.succeeded` is at least 1
26. **Claim phase=ready, slot phase=ready**: the claim asserts the machine it asked for can take the
    pool's work, the slot reports `ready`, `status.powerState` is `on`, and the slot keeps the digest
27. **A ready claim past its boot deadline keeps its machine**: the observation is measured from the
    claim's own deadline (the operator's budget is 12m10s, and the group's `maxNodeProvisionTime` is a
    floor that only raises it), and the claim, the slot, the node and the pod are left alone
28. **Kubernetes node Ready**: Node condition is True, and uncordoned
29. **Deployment pod running**: hello-world pod scheduled on the machine the claim bought

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
    vmControlPort    = 9052
    vmControlHost    = "localhost"
    vmControlLogFile = "/tmp/vm-control-requests.log"

    // Node names
    agentNode0 = "k3d-homelab-autoscaler-agent-0"
    agentNode1 = "k3d-homelab-autoscaler-agent-1"

    // Taints
    agentTaint = "node.kubernetes.io/role=agent:NoSchedule"

    // Timeouts
    // Gates the autoscaler is asked to pass: a decision, then the call it leads to
    caDecisionTimeout     = 5 * time.Minute  // CA's loop + the unneeded window + the drain
    deleteNodesRPCTimeout = 2 * time.Minute  // the taint delay, then the call

    // The machine's own clock: a container stops, the kubelet goes silent, the node lifecycle
    // controller declares it NotReady after its monitor grace period
    vmShutdownTimeout    = 60 * time.Second
    vmStartupTimeout     = 60 * time.Second
    jobCompletionTimeout = 120 * time.Second
    podScheduleTimeout   = 200 * time.Second
    nodeStateTimeout     = 120 * time.Second
    machineStateTimeout  = 4 * time.Minute

    // The claim's phases: created, committed, ready, collected
    claimCreatedTimeout   = 60 * time.Second
    claimPhaseTimeout     = 90 * time.Second
    claimReadyTimeout     = 5 * time.Minute
    claimCollectedTimeout = 3 * time.Minute

    // The window of the deadline spec, and the VM control request window
    minDeadlineWatch  = 3 * time.Minute
    maxDeadlineWatch  = 14 * time.Minute
    bootRequestWindow = 90 * time.Second

    // Check intervals
    checkInterval = 5 * time.Second
)
```

## Expected Runtime

| Test | Duration | Notes |
|------|----------|-------|
| Test 1: Setup | 3-5m | The unclaimed machines come down, then the harness's claim boots one and follows it to `ready` |
| Test 2: Scale-Down | 3-6m | The autoscaler's decision window, the drain, the shutdown Job, the machine observed off |
| Test 3: Scale-Up | 15-20m | The boot to `ready`, then the observation that outlives the claim's 12m10s budget |
| **Total** | **~25 minutes** | The long spec is the deadline observation, which cannot be shortened: the budget is a sum of the operator's constants |

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
