# E2E Test Refactoring Plan

## Executive Summary

Rewrite `/test/e2e/k3d_integration_test.go` to properly test the gRPC-based Cluster Autoscaler integration with three sequential tests that verify the complete scale-down and scale-up workflows.

## Current Test Problems

1. **Test 1 (Volumes)** - Too isolated; doesn't test real workflow
2. **Test 2 (Full workflow)** - Missing critical verifications:
   - No node tainting (workloads can interfere)
   - No gRPC request verification to VM control server
   - No verification that pods actually run successfully
   - Missing scale-up workflow (only tests shutdown)
   - Tests run independently without shared state

## New Test Structure

### Test 1: Setup & Environment Validation (30 seconds)
**Purpose:** Verify prerequisites and prepare environment

**Steps:**
1. Verify k3d cluster exists
2. Set KUBECONFIG environment variable
3. Taint agent nodes with `node.kubernetes.io/role=agent:NoSchedule`
   - Prevents regular workloads from scheduling on agent nodes
   - Ensures clean scale-down detection
4. Verify gRPC forwarder connectivity
5. Start VM control server (if not running)
6. Verify VM control server responds to health check

**Verification Points:**
- k3d cluster is running
- Agent nodes are tainted
- gRPC service forwarder exists
- VM control server is responding

---

### Test 2: Scale-Down Workflow (2.5 minutes)
**Purpose:** Verify Cluster Autoscaler detects unneeded nodes and triggers shutdown

**Preconditions:**
- Node CRs already applied (from setup)
- Agent nodes tainted (no workloads can schedule)

**Steps:**
1. Apply Group CR with aggressive scale-down settings:
   - `scaleDownUnneededTime: 30s`
   - `scaleDownUtilizationThreshold: 0.1`
2. Wait for Cluster Autoscaler to detect unneeded nodes (~60s)
   - Monitor CA logs for "unneeded" messages
3. Verify gRPC `NodeGroupDeleteNodes` call was made
   - Check CA logs for "DeleteNodes" call
4. Verify Node CR `spec.desiredPowerState` set to `Off`
5. Verify shutdown job is created
6. Verify VM control server receives `/stop` HTTP request
   - Track requests in a log file
7. Verify k3d node is stopped
8. Verify Node CR `status.progress` becomes `shutdown`
9. Verify Kubernetes node becomes NotReady

**Verification Points:**
- CA detects node as unneeded
- gRPC DeleteNodes call is made
- Shutdown job is created with correct ServiceAccount and volumes
- VM control server receives `/stop` request
- k3d node stops
- Node CR reaches `shutdown` state
- Kubernetes node becomes NotReady

---

### Test 3: Scale-Up Workflow (2.5 minutes)
**Purpose:** Verify Cluster Autoscaler scales up when pods need scheduling

**Preconditions:**
- Test 2 completed (node is shutdown)
- Shutdown state preserved (Node CR with `progress=shutdown`)

**Steps:**
1. Untaint agent node (allow scheduling)
2. Apply hello-world deployment targeting the agent node
3. Wait for pod to be scheduled (should fail initially - node is down)
4. Verify CA detects need for scale-up
5. Verify gRPC `NodeGroupIncreaseSize` call was made
   - Check CA logs for "IncreaseSize" call
6. Verify Node CR `spec.desiredPowerState` set to `On`
7. Verify startup job is created
8. Verify VM control server receives `/start` HTTP request
   - Check request log from Test 2
9. Verify k3d node starts
10. Verify startup job completes
11. Verify Node CR `status.progress` becomes `ready`
12. Verify Kubernetes node becomes Ready
13. Verify deployment pod is scheduled and running

**Verification Points:**
- Pod scheduling triggers scale-up
- gRPC IncreaseSize call is made
- Startup job is created with correct ServiceAccount and volumes
- VM control server receives `/start` request
- k3d node starts
- Startup job completes
- Node CR reaches `ready` state
- Kubernetes node becomes Ready
- Deployment pod is running on the node

---

## Implementation Details

### Helper Functions to Add

**In `/test/utils/utils.go`:**

```go
// NodeTainting
func TaintNode(nodeName, taint string) error
func UntaintNode(nodeName, taintKey string) error
func VerifyNodeTaint(nodeName, taint string) bool

// VM Control Server
func StartVMControlServer(port int) (*exec.Cmd, int, error)
func StopVMControlServer(pid int) error
func VerifyVMControlServerRequest(endpoint string, vmName string) bool
func GetVMControlServerRequests() ([]VMRequest, error)

// gRPC Verification
func VerifyGRPCCall(method string) bool
func GetClusterAutoscalerLogs(filter string) string

// Job Verification
func VerifyJobHasServiceAccount(jobLabel, serviceAccount string) bool
func VerifyJobHasVolume(jobLabel, volumeType, volumeName string) bool
func WaitForJobCompletion(jobLabel, timeout time.Duration) bool

// Node Verification
func WaitForNodeState(nodeName, state string, timeout time.Duration) bool
func WaitForNodeReady(nodeName string, ready bool, timeout time.Duration) bool
func WaitForK3dNodeState(vmName string, running bool, timeout time.Duration) bool
```

### Test Constants

```go
const (
    // Timeouts
    caDetectionTimeout     = 60 * time.Second  // CA detecting unneeded nodes
    vmShutdownTimeout      = 60 * time.Second  // VM shutdown (k3d node stop)
    vmStartupTimeout       = 60 * time.Second  // VM startup (k3d node start)
    jobCompletionTimeout   = 120 * time.Second // Job completion
    podScheduleTimeout     = 60 * time.Second  // Pod scheduling
    
    // Intervals
    checkInterval          = 5 * time.Second
    fastCheckInterval      = 1 * time.Second
    
    // Node names
    agentNode0             = "k3d-homelab-autoscaler-agent-0"
    agentNode1             = "k3d-homelab-autoscaler-agent-1"
    
    // Taints
    agentTaint             = "node.kubernetes.io/role=agent:NoSchedule"
    
    // VM Control Server
    vmControlServerPort    = 8080
    vmControlServerHost    = "localhost"
)
```

### Shared State Management

Use Ginkgo's `BeforeSuite`/`AfterSuite` and `var` declarations to share state between tests:

```go
var (
    vmControlServerPID int
    vmRequestsLog      []VMRequest
    shutdownNodeName   string  // Set in Test 2, used in Test 3
)

var _ = BeforeSuite(func() {
    // Start VM control server
    // Taint nodes
    // Apply Node CRs
})

var _ = AfterSuite(func() {
    // Stop VM control server
    // Untaint nodes
    // Cleanup Node CRs
})
```

---

## Files to Modify

### 1. `/test/e2e/k3d_integration_test.go` (COMPLETE REWRITE)
- Replace existing test structure
- Implement 3 new tests as described above
- Add comprehensive verification logic

### 2. `/test/utils/utils.go` (ADD HELPER FUNCTIONS)
- Add tainting utilities
- Add VM control server utilities
- Add gRPC verification utilities
- Add job verification utilities
- Add node state verification utilities

### 3. `/examples/k3d/create-cluster.sh` (OPTIONAL)
- Add node tainting after cluster creation
- OR keep tainting in test setup for flexibility

### 4. `/Makefile` (MINOR UPDATE)
- Update test runtime estimates in comments
- Ensure VM control server cleanup is robust

---

## Test Runtime Estimates

| Test | Duration | Notes |
|------|----------|-------|
| Test 1: Setup | 30s | Quick validation |
| Test 2: Scale-Down | 2.5m | CA detection (60s) + shutdown (60s) + verification |
| Test 3: Scale-Up | 2.5m | Scale-up (60s) + startup (60s) + verification |
| **Total** | **~6 minutes** | With overhead, ~8 minutes |

---

## Verification Checklist

### Scale-Down Verification
- [ ] CA logs show "unneeded" detection
- [ ] CA logs show "DeleteNodes" gRPC call
- [ ] Node CR `spec.desiredPowerState` = `Off`
- [ ] Shutdown job created with correct ServiceAccount
- [ ] Shutdown job has ConfigMap and Secret volumes
- [ ] VM control server receives `/stop` request
- [ ] k3d node stops (verified via `k3d node list`)
- [ ] Node CR `status.progress` = `shutdown`
- [ ] Kubernetes node NotReady

### Scale-Up Verification
- [ ] Pod scheduling triggers scale-up need
- [ ] CA logs show "IncreaseSize" gRPC call
- [ ] Node CR `spec.desiredPowerState` = `On`
- [ ] Startup job created with correct ServiceAccount
- [ ] Startup job has ConfigMap volume
- [ ] VM control server receives `/start` request
- [ ] k3d node starts (verified via `k3d node list`)
- [ ] Startup job completes successfully
- [ ] Node CR `status.progress` = `ready`
- [ ] Kubernetes node Ready
- [ ] Deployment pod scheduled and running

---

## Edge Cases to Handle

1. **VM Control Server Not Running**
   - Start server in BeforeSuite
   - Verify server is responding before tests
   - Handle server crashes gracefully

2. **Node Taint Already Present**
   - Check for existing taints before applying
   - Don't fail if taint already exists

3. **Stuck Transitions**
   - Timeout waiting for state changes
   - Log CA logs and node status for debugging
   - Provide clear error messages

4. **gRPC Connection Issues**
   - Verify gRPC forwarder connectivity
   - Check CA logs for connection errors
   - Retry failed verifications

5. **k3d Node State Issues**
   - Verify node state via `k3d node list`
   - Handle nodes in inconsistent states
   - Provide cleanup instructions

---

## Success Criteria

1. **All 3 tests pass consistently** (>90% pass rate)
2. **Comprehensive verification** of all workflow steps
3. **Clear error messages** when tests fail
4. **Proper cleanup** of all resources
5. **Reasonable runtime** (<10 minutes total)
6. **No flaky tests** (deterministic behavior)

---

## Implementation Order

1. **Phase 1: Helper Functions** (`/test/utils/utils.go`)
   - Node tainting utilities
   - VM control server utilities
   - Job verification utilities
   - Node state verification utilities

2. **Phase 2: Test Infrastructure** (`/test/e2e/k3d_integration_test.go`)
   - New test structure with BeforeSuite/AfterSuite
   - Shared state management
   - Common setup/cleanup logic

3. **Phase 3: Test Implementation**
   - Test 1: Setup & Environment Validation
   - Test 2: Scale-Down Workflow
   - Test 3: Scale-Up Workflow

4. **Phase 4: Testing & Refinement**
   - Run tests multiple times
   - Fix flaky behavior
   - Improve error messages
   - Optimize timeouts

5. **Phase 5: Documentation**
   - Update README with test instructions
   - Document expected runtime
   - Add troubleshooting guide

---

## Notes

- **Node Tainting**: Critical for clean scale-down detection. Without tainting, workloads can schedule on agent nodes and prevent CA from detecting them as "unneeded".
- **Shared State**: Test 2 sets up shutdown state that Test 3 uses. This ensures the scale-up workflow tests actually powering on a node.
- **VM Control Server**: Must be running on host (port 8080) to receive HTTP requests from startup/shutdown jobs.
- **gRPC Verification**: Primary verification is through CA logs, as we can't directly intercept gRPC calls.
- **k3d Node Verification**: Use `k3d node list` command to verify node running/stopped state.
