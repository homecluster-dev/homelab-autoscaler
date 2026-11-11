//go:build e2e
// +build e2e

/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
	"github.com/homecluster-dev/homelab-autoscaler/test/utils"
)

const (
	clusterName      = "homelab-autoscaler"
	grpcServerPort   = "50052"
	kubeconfigPath   = "./kubeconfig"
	namespace        = "homelab-autoscaler-system"
	vmControlPort    = 9052
	vmControlHost    = "localhost"
	vmControlLogFile = "/tmp/vm-control-requests.log"

	// groupName is the Group CR of the two agent slots. A claim records it
	// (spec.request.group), which is what makes the pool of a scale-up readable.
	groupName = "group1"

	// Node names - these must match the k3d cluster configuration
	agentNode0 = "k3d-homelab-autoscaler-agent-0"
	agentNode1 = "k3d-homelab-autoscaler-agent-1"

	// Taints - prevents workloads from being scheduled on agent nodes
	agentTaint = "node.kubernetes.io/role=agent:NoSchedule"

	// Timeouts - various timeouts for different operations
	caDetectionTimeout   = 60 * time.Second  // Time for CA to detect need for scale
	vmShutdownTimeout    = 60 * time.Second  // Time for VM to power off
	vmStartupTimeout     = 60 * time.Second  // Time for VM to power on
	jobCompletionTimeout = 120 * time.Second // Time for startup/shutdown job to complete
	podScheduleTimeout   = 200 * time.Second // Time for pod to be scheduled after node is ready
	nodeStateTimeout     = 120 * time.Second // Time for the slot's report to follow an observation

	// Claim timeouts - the NodeClaim is the object a scale-up is, so the phases of the model are
	// what the suite waits on: a phase is a fact about one request, with a name, an attempt number
	// and a deadline attached, where a progress value was a fact about a slot's record.

	// claimCreatedTimeout is how long the suite waits for the provider's IncreaseSize to have
	// produced the claim: the gRPC handler creates it on the request, so this is a call's latency.
	claimCreatedTimeout = 60 * time.Second
	// claimPhaseTimeout is how long one phase move of a claim may take: pending -> booting is the
	// write that creates the startup Job, so this is Job creation plus a reconcile.
	claimPhaseTimeout = 90 * time.Second
	// claimReadyTimeout is how long the claim may take to reach ready. The readiness gate of the
	// attempt has its own budget (fsm.DefaultReadinessGateTimeout), and the gate is what holds a
	// claim at booting until the node is Ready and free of the pool's readiness taints.
	claimReadyTimeout = 5 * time.Minute
	// claimCollectedTimeout is how long a scale-down's claim may keep its finalizer: the finalizer
	// is the request that reverses a committed boot, and it is released once the machine is
	// observed off. The TTL of a terminal claim is much longer (the suite never waits for it);
	// what the suite asserts is that the claim stops being live, which is the sentence "the
	// machine is no longer wanted".
	claimCollectedTimeout = 2 * time.Minute

	// Check intervals - how often to poll for state changes
	checkInterval     = 5 * time.Second
	fastCheckInterval = 1 * time.Second
)

// Shared state between tests
// These variables persist across test contexts and are used to track state
var (
	vmControlServerPID int    // PID of the VM control server process
	shutdownNodeName   string // Name of the node that was shut down (for scale-up test)

	// scaleUpClaimName is the NodeClaim the provider created for the scale-up of Test 3. The claim
	// is the request, so the suite names it and follows it across its phases, and its digest on the
	// slot is what survives the collection of the object.
	scaleUpClaimName string
)

// BeforeSuite runs once before all tests in this suite
// It sets up the test environment by verifying prerequisites
var _ = BeforeSuite(func() {
	By("Setting up test environment")

	// Set KUBECONFIG environment variable so kubectl commands use our test cluster
	err := os.Setenv("KUBECONFIG", filepath.Join(getProjectDir(), kubeconfigPath))
	Expect(err).NotTo(HaveOccurred())

	// Verify k3d cluster exists
	cmd := exec.Command("k3d", "cluster", "list")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	cmd = exec.Command("k3d", "cluster", "list")
	output, _ := utils.Run(cmd)
	Expect(strings.Contains(output, clusterName)).To(BeTrue(),
		"k3d cluster '%s' not found. Please run 'make test-e2e' to set up the test environment.", clusterName)

	// Clear VM control server log from previous test runs
	err = utils.ClearLogFile(vmControlLogFile)
	Expect(err).NotTo(HaveOccurred())
})

// =============================================================================
// TEST SUITE: K3d Integration
// =============================================================================
// This test suite validates the complete autoscaling workflow with k3d nodes:
// 1. Setup & Environment Validation - Prepare the test environment
// 2. Scale-Down Workflow - Verify nodes can be powered off when not needed
// 3. Scale-Up Workflow - Verify nodes can be powered on when needed
//
// The model the suite drives is the claim model of docs/design/nodeclaim.md: a slot (Node CR) is
// configuration only, a NodeClaim is one request for one machine, and Node.status is the report of
// what the cluster observes. Desired power is derived, never stored, so nothing here reads or writes
// a spec field of a slot: a scale-up is a claim the provider creates, a scale-down is the end of one,
// and every wait in the suite is on a phase - of the claim, of the slot's report, of the machine.
// =============================================================================
var _ = Describe("K3d Integration", Serial, func() {

	// =============================================================================
	// TEST 1: Setup & Environment Validation
	// =============================================================================
	// Purpose: Prepare and validate the test environment before running scale tests
	//
	// Steps:
	// 1. Verify k3d cluster is running with agent nodes
	// 2. Taint agent nodes to prevent workload scheduling (they're for testing only)
	// 3. Set up VM control server service for node power management
	// 4. Apply Node CRs (v2alpha1) that define our agent slots
	// 5. Apply Group CR with aggressive scale-down settings for testing
	// 6. Verify the slots start free: no live NodeClaim names them
	// =============================================================================
	Context("Setup & Environment Validation", func() {
		It("should verify prerequisites and prepare environment", func() {
			By("Verifying k3d cluster is running")
			cmd := exec.Command("k3d", "cluster", "list", clusterName)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying agent nodes exist")
			Expect(utils.NodeExists(agentNode0)).To(BeTrue(),
				"Agent node 0 should exist")
			Expect(utils.NodeExists(agentNode1)).To(BeTrue(),
				"Agent node 1 should exist")

			// Taint agent nodes to prevent workload scheduling.
			// This is critical: we want control over which node gets scaled,
			// so workloads can't accidentally schedule on them before we're ready.
			By("Tainting agent nodes to prevent workload scheduling")
			err = utils.TaintNode(agentNode0, agentTaint)
			Expect(err).NotTo(HaveOccurred())
			err = utils.TaintNode(agentNode1, agentTaint)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying nodes are tainted")
			Eventually(func() bool {
				return utils.VerifyNodeTaint(agentNode0, "role") &&
					utils.VerifyNodeTaint(agentNode1, "role")
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"Agent nodes should be tainted with role taint")

			By("Verifying gRPC service forwarder exists")
			// Service may not exist yet - that's okay, it will be created by the test
			_, _ = utils.Run(kubectl("get", "service", "homelab-autoscaler-grpc-local", "-n", namespace))

			By("Verifying VM control server is accessible")
			Eventually(func() bool {
				return utils.VerifyVMControlServer(vmControlHost, vmControlPort)
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"VM control server should be responding on %s:%d", vmControlHost, vmControlPort)

			// Set up service to make VM control server accessible from inside the cluster
			By("Setting up VM control host service")
			cmd = exec.Command("bash", "./examples/k3d/setup-vm-control-service.sh")
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to setup VM control service: %s", output)

			By("Applying secrets and configmaps")
			cmd = exec.Command("kubectl", "apply", "-f", "./examples/k3d/secrets.yaml")
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply secrets/configmaps: %s", output)

			// Node CRs define our agent slots with startup/shutdown pod specs.
			// These pods will curl the VM control server to power nodes on/off.
			// The v2alpha1 spec carries no runtime intent: there is no spec.powerState in it, so
			// applying this file can never contradict a scale-up, and selfHeal has nothing to revert.
			By("Applying Node CRs")
			cmd = exec.Command("kubectl", "apply", "-f", "./examples/k3d/nodes1.yaml")
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Node CRs: %s", output)

			By("Verifying Node CRs are created")
			Eventually(func() error {
				_, err := readSlot(agentNode0)

				return err
			}, 30*time.Second, checkInterval).Should(Succeed(),
				"Node CRs should be created")

			By("Verifying the applied slots declare configuration only")
			Eventually(func() string {
				slot, err := readSlot(agentNode0)
				if err != nil {
					return ""
				}

				return slot.Spec.KubernetesNodeName
			}, 30*time.Second, checkInterval).Should(Equal(agentNode0),
				"The slot's identity is its name and its kubernetesNodeName: the provider looks a slot up by name")

			// Apply Group CR with aggressive scale-down settings for testing
			// scaleDownUnneededTime: 30s - CA will consider node unneeded after 30s
			// scaleDownUtilizationThreshold: 0.1 - Node with <10% utilization is unneeded
			// maxNodeProvisionTime: 2m - the floor of a claim's deadline. The operator's budget of one
			// attempt (claim.AttemptBudget) is longer, so the claims of this run carry the operator's
			// budget: the floor never enforces a deadline shorter than the number cluster-autoscaler
			// is itself told (NodeGroupGetOptions -> MaxNodeProvisionDuration).
			By("Applying Group CR with aggressive scale-down settings")
			groupYAML := `
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: group1
  namespace: homelab-autoscaler-system
spec:
  ignoreDaemonSetsUtilization: true
  maxNodeProvisionTime: 2m
  scaleDownGpuUtilizationThreshold: "30"
  scaleDownUnneededTime: 30s
  scaleDownUnreadyTime: 30s
  scaleDownUtilizationThreshold: "0.1"
  zeroOrMaxNodeScaling: false
`
			tmpFile, err := os.CreateTemp("", "group-*.yaml")
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(tmpFile.Name())

			_, err = tmpFile.WriteString(groupYAML)
			Expect(err).NotTo(HaveOccurred())
			tmpFile.Close()

			cmd = exec.Command("kubectl", "apply", "-f", tmpFile.Name())
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Group: %s", output)

			By("Verifying the Group CR is served at v2alpha1")
			Eventually(func() string {
				out, err := utils.Run(kubectl("get", "group.infra.homecluster.dev", groupName,
					"-n", namespace, "-o", "jsonpath={.apiVersion}"))
				if err != nil {
					return ""
				}

				return strings.TrimSpace(out)
			}, 30*time.Second, checkInterval).Should(Equal(infrav2alpha1.GroupVersion.String()),
				"The readback must name the version the operator serves")

			// The whole model in one line: a slot is free exactly when no live NodeClaim names it.
			// No status seeding, no three-field scan, no observation is needed to know that, which is
			// why the slots are readable as free the instant they are applied.
			By("Verifying the slots are free: no live NodeClaim names them")
			Eventually(func() int {
				live, err := liveClaimsOfSlot(agentNode0)
				if err != nil {
					return -1
				}

				other, err := liveClaimsOfSlot(agentNode1)
				if err != nil {
					return -1
				}

				return len(live) + len(other)
			}, 30*time.Second, checkInterval).Should(Equal(0),
				"A slot nobody asked for has no live claim: that is the free shape IncreaseSize claims")

			By("Verifying the slots report the machines the pool does not want")
			// The k3d agents booted with the cluster, so their machines are observed up while no
			// claim names them. Law 1 of the input makes them unwanted capacity: the shutdown path
			// owns the machine, and the slot's derived report says draining on the way down and off
			// once the machine is down. The two are one shape, a machine the pool has stopped
			// wanting, a Job and a reconcile apart, so the suite reads the pair.
			Eventually(func() bool {
				return slotIsUnwanted(agentNode0)
			}, nodeStateTimeout, checkInterval).Should(BeTrue(),
				"A slot no claim names reports off, or draining while its machine comes down")

			By("Setup & Environment Validation completed successfully")
		})
	})

	// =============================================================================
	// TEST 2: Scale-Down Workflow
	// =============================================================================
	// Purpose: Verify that Cluster Autoscaler can detect unneeded nodes and
	// our controller can power them off.
	//
	// Flow:
	// 1. CA detects node is unneeded (no workloads, below utilization threshold)
	// 2. CA calls our gRPC server's NodeGroupDeleteNodes method
	// 3. The provider cancels the machine: a live claim of the slot is recorded with the request
	//    (call: DeleteNodes, nodes: the names) and deleted; a slot with no live claim has nothing to
	//    cancel, and the input law powers its machine off on the slot's next pass
	// 4. The slot's shutdown transition runs: cordon, shutdown Job, power off
	// 5. The controller creates shutdown job that curls VM control server /stop endpoint
	// 6. VM control server stops the k3d node
	// 7. The machine is observed off: the slot's report is off, and a marked claim ends completed
	//
	// Key validations:
	// - gRPC call was made
	// - the slot is not wanted: no live NodeClaim names it (the sentence spec.powerState: off used
	//   to carry, now with an owner, a call and a reason attached)
	// - the slot's derived phase reports the machine coming down, then off
	// - the observed power state is off
	// - Shutdown job created with correct spec (ServiceAccount, volumes, TTL)
	// - VM control server received /stop request
	// - k3d node actually stopped
	// - the slot's digest of the episode, if the slot ever had one, records an ended claim
	// - Kubernetes node became NotReady
	// =============================================================================
	Context("Scale-Down Workflow", func() {
		It("should verify Cluster Autoscaler detects unneeded nodes and triggers shutdown", func() {
			By("Waiting for Cluster Autoscaler to detect unneeded nodes")
			Eventually(func() bool {
				logs, _ := utils.GetClusterAutoscalerLogs(namespace, 1000)
				return strings.Contains(logs, "unneeded") ||
					strings.Contains(logs, "DeleteNodes")
			}, caDetectionTimeout, checkInterval).Should(BeTrue(),
				"Cluster Autoscaler should detect unneeded nodes. Check CA logs.")

			By("Verifying gRPC NodeGroupDeleteNodes call was made")
			Eventually(func() bool {
				logs, _ := utils.GetHomeClusterAutoscalerLogs(namespace, 1000)
				return strings.Contains(logs, "NodeGroupDeleteNodes")
			}, caDetectionTimeout, checkInterval).Should(BeTrue(),
				"homelab-autoscaler should receive NodeGroupDeleteNodes call")

			// The replacement for "Node CR powerState is set to off": in v2alpha1 there is no spec
			// field that carries a scale-down. The machine is not wanted exactly when no live claim
			// names the slot, and that is the read the provider's NodeGroupDeleteNodes and the
			// input law agree on.
			By("Verifying the slot is not wanted: no live NodeClaim names it")
			Eventually(func() int {
				live, err := liveClaimsOfSlot(agentNode0)
				if err != nil {
					return -1
				}

				return len(live)
			}, 30*time.Second, checkInterval).Should(Equal(0),
				"A scale-down leaves no live claim on the slot: a pending one is deleted outright,\n"+
					"a booting or ready one is deleted through its finalizer")

			By("Verifying the slot reports the machine coming down")
			// The report of a machine the pool has stopped wanting is draining while it is observed
			// up (cordoned, shutdown Job in flight) and off once it is down. Which of the two this
			// pass sees depends on how far the shutdown got, so the pair is the assertion, and the
			// off end of it is pinned below, after the Job completes.
			Eventually(func() bool {
				return slotIsUnwanted(agentNode0)
			}, nodeStateTimeout, checkInterval).Should(BeTrue(),
				"The report of a slot whose machine is unwanted is off, or draining while the machine comes down")

			By("Verifying shutdown job is created")
			Eventually(func() bool {
				cmd := kubectl("get", "jobs",
					"-l", "type=shutdown",
					"-n", namespace,
					"-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				return err == nil && len(strings.TrimSpace(output)) > 0
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"Shutdown job should be created")

			By("Verifying shutdown job has correct ServiceAccount")
			Eventually(func() bool {
				return utils.VerifyJobHasServiceAccount(namespace, "type=shutdown", "controller-manager")
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"Shutdown job should use controller-manager ServiceAccount")

			By("Verifying shutdown job has Secret volume")
			Eventually(func() bool {
				return utils.VerifyJobHasVolume(namespace, "type=shutdown", "auth-secrets")
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"Shutdown job should have auth-secrets volume")

			// Verify TTL is set for automatic job cleanup
			// Jobs have ttlSecondsAfterFinished: 600 (10 minutes) by default
			By("Verifying shutdown job has TTL set")
			Eventually(func() string {
				out, _ := utils.Run(kubectl("get", "jobs",
					"-l", "type=shutdown",
					"-n", namespace,
					"-o", "jsonpath={.items[*].spec.ttlSecondsAfterFinished}"))

				return strings.TrimSpace(out)
			}, 60*time.Second, checkInterval).Should(
				And(Not(BeEmpty()), Not(Equal("<no value>"))),
				"Shutdown job should have ttlSecondsAfterFinished set")

			By("Verifying VM control server receives /stop request")
			Eventually(func() bool {
				return utils.VerifyVMControlServerRequest(vmControlLogFile, "/stop", agentNode0, 30*time.Second)
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"VM control server should receive /stop request for %s", agentNode0)

			By("Verifying shutdown job completes")
			success, jobLogs, jobEvents := utils.WaitForJobCompletionWithError(namespace, "type=shutdown", jobCompletionTimeout)
			Expect(success).To(BeTrue(),
				"Shutdown job should complete successfully. Job Logs: %s. Job Events: %s", jobLogs, jobEvents)

			By("Verifying k3d node stops")
			Eventually(func() bool {
				cmd := exec.Command("k3d", "node", "list", agentNode0)
				output, _ := cmd.CombinedOutput()
				outputStr := strings.ToLower(string(output))
				return !strings.Contains(outputStr, "running") && !strings.Contains(outputStr, "up")
			}, vmShutdownTimeout, checkInterval).Should(BeTrue(),
				"k3d node %s should stop", agentNode0)

			// The replacement for "Node CR progress becomes shutdown". The report of the slot is
			// derived from (the live claim, the observed power, the usability of the node) by
			// phase.Derive, and the machine is off with no claim: the slot is off, and the power the
			// provider observes is off.
			By("Verifying the slot's report becomes off")
			Eventually(func() infrav2alpha1.NodePhase {
				return slotPhase(agentNode0)
			}, nodeStateTimeout, checkInterval).Should(Equal(infrav2alpha1.NodePhaseOff),
				"The derived phase of a free slot with no machine is off")

			By("Verifying the machine is observed off")
			Eventually(func() infrav2alpha1.PowerState {
				return slotPowerState(agentNode0)
			}, nodeStateTimeout, checkInterval).Should(Equal(infrav2alpha1.PowerStateOff),
				"The observed power state of the slot is off")

			By("Verifying the scale-down left no claim owning the slot")
			// A claim a scale-down marked keeps its finalizer until the machine is observed off, then
			// ends completed and is released. The target size of the group counts live claims, so the
			// unit came off on the write that named the machine.
			Eventually(func() int {
				live, err := liveClaimsOfSlot(agentNode0)
				if err != nil {
					return -1
				}

				return len(live)
			}, claimCollectedTimeout, checkInterval).Should(Equal(0),
				"The claim of the episode is not live: the slot is free, and the next IncreaseSize may claim it")

			By("Verifying the slot's digest records an episode that ended")
			// The claims are collected by their TTL and the story survives in Node.status.lastClaim.
			// A slot whose machines were never claimed has no digest at all, which is the empty case
			// of this read; what must never happen is a digest that reports a live phase on a slot
			// whose machine is off.
			Eventually(func() bool {
				digest := slotDigest(agentNode0)
				if digest == nil {
					return true
				}

				return phase.IsTerminal(digest.Phase)
			}, nodeStateTimeout, checkInterval).Should(BeTrue(),
				"The digest of the last claim of an off slot is a terminal phase (failed, completed),\n"+
					"or there is no claim to record")

			By("Verifying Kubernetes node becomes NotReady")
			Eventually(func() bool {
				out, _ := utils.Run(kubectl("get", "node", agentNode0,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}"))
				status := strings.TrimSpace(out)
				return status == "False" || status == "Unknown"
			}, nodeStateTimeout, checkInterval).Should(BeTrue(),
				"Kubernetes node should be NotReady or Unknown")

			// Store node name for Test 3 (Scale-Up Workflow)
			shutdownNodeName = agentNode0

			By("Scale-Down Workflow completed successfully")
		})
	})

	// =============================================================================
	// TEST 3: Scale-Up Workflow
	// =============================================================================
	// Purpose: Verify that Cluster Autoscaler can detect need for more capacity
	// and our controller can power on a previously shutdown node.
	//
	// Flow:
	// 1. Apply deployment targeting the shutdown node (it's tainted, so pod stays pending)
	// 2. CA detects need for scale-up (pending pod can't be scheduled)
	// 3. CA calls our gRPC server's NodeGroupIncreaseSize method
	// 4. The provider creates a NodeClaim on the free slot: the request, with its attribution
	//    (call, delta, group, requestedBy), its deadline, its TTL and an ownerReference on the slot
	// 5. The claim controller seeds status.attempt from the slot's digest; the slot's startup
	//    transition writes the pending -> booting move and creates the startup Job on the same write
	// 6. The startup Job curls the VM control server /start endpoint, the VM control server starts
	//    the k3d node, the node registers
	// 7. The readiness gate answers: the node is Ready and free of the pool's readiness taints, so
	//    the claim moves to ready, and the slot's report and observation follow
	// 8. Deployment pod gets scheduled on the now-ready node
	//
	// Key validations:
	// - gRPC call was made
	// - a NodeClaim exists for the slot, and it is the record of the request: attribution, deadline,
	//   attempt, TTL, ownerReference
	// - the claim's phases are the phases of the model: pending -> booting (committed) -> ready
	// - the slot's derived phase follows the claim: off -> booting -> ready
	// - the observed power state is on
	// - Startup job created with correct spec (ServiceAccount, volumes, TTL)
	// - VM control server received /start request
	// - k3d node actually started
	// - the slot keeps the digest of the claim that served the request, and a startup timestamp
	// - Kubernetes node became Ready AND uncordoned (unschedulable=false)
	// - Deployment pod scheduled on the correct node
	//
	// IMPORTANT: The node must be BOTH Ready AND unschedulable=false before
	// pods can be scheduled. The readiness gate of the scale-up uncordons the node and answers for
	// the claim's ready phase; a node that clears neither is not reported ready, and its claim fails
	// at the gate's timeout (reason ReadinessGateTimeout).
	// =============================================================================
	Context("Scale-Up Workflow", func() {
		It("should verify Cluster Autoscaler scales up when pods need scheduling", func() {
			By("Verifying node is off from Test 2")
			Expect(shutdownNodeName).ToNot(BeEmpty(),
				"shutdownNodeName should be set by Test 2")
			Expect(slotPhase(shutdownNodeName)).To(Equal(infrav2alpha1.NodePhaseOff),
				"The slot of the scale-up starts free: off, and no live claim names it")

			// Untaint the shutdown node to allow scheduling
			// This is necessary because we taint nodes in Test 1 to prevent
			// workloads from scheduling before we're ready
			By("Untainting the shutdown node to allow scheduling")
			err := utils.UntaintNode(shutdownNodeName, "node.kubernetes.io/role")
			Expect(err).NotTo(HaveOccurred())

			By("Verifying node is untainted")
			Eventually(func() bool {
				return !utils.VerifyNodeTaint(shutdownNodeName, "role")
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"Node should be untainted")

			// Verify the other node is still tainted
			// This ensures pods will be scheduled on the node we just powered on,
			// not on the other agent node
			By("Verifying other agent node is still tainted")
			otherNode := agentNode1
			if shutdownNodeName == agentNode1 {
				otherNode = agentNode0
			}
			Eventually(func() bool {
				return utils.VerifyNodeTaint(otherNode, "role")
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"Other node %s should still be tainted", otherNode)

			// Apply deployment targeting the agent node
			// The pod will be pending because the node is down
			// This triggers CA to detect need for scale-up
			By("Applying hello-world deployment targeting the agent node")
			cmd := exec.Command("kubectl", "apply", "-f", "examples/k3d/hello-world-deployment.yaml")
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply deployment: %s", output)

			By("Waiting for CA to detect need for scale-up")
			Eventually(func() bool {
				logs, _ := utils.GetClusterAutoscalerLogs(namespace, 1000)
				return strings.Contains(logs, "IncreaseSize") ||
					strings.Contains(logs, "scale up")
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"CA should detect need for scale-up")

			By("Verifying gRPC NodeGroupIncreaseSize call was made")
			Eventually(func() bool {
				logs, _ := utils.GetHomeClusterAutoscalerLogs(namespace, 1000)
				return strings.Contains(logs, "NodeGroupIncreaseSize")
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"homelab-autoscaler should receive NodeGroupIncreaseSize call")

			// The replacement for "Node CR powerState is set to On": the scale-up is an object. The
			// provider creates it on a free slot, and everything the scalar could not say - who
			// asked, which call, for which pool, with what budget, which attempt - is on it.
			By("Verifying the provider created a NodeClaim for the slot")
			Eventually(func() error {
				fresh, err := liveClaimOfSlot(shutdownNodeName)
				if err != nil {
					return err
				}

				if fresh == nil {
					return fmt.Errorf("no live NodeClaim names slot %s yet", shutdownNodeName)
				}

				scaleUpClaimName = fresh.Name

				return nil
			}, claimCreatedTimeout, checkInterval).Should(Succeed(),
				"A scale-up is a live NodeClaim on the slot: pending, booting or ready is the claim's "+
					"answer that a machine is wanted; a claim that ended in failed, completed or under a "+
					"deletion timestamp is not")

			By("Verifying the claim carries the attribution of the request")
			Eventually(func() infrav2alpha1.NodeClaimSpec {
				fresh, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || fresh == nil {
					return infrav2alpha1.NodeClaimSpec{}
				}

				return fresh.Spec
			}, 30*time.Second, checkInterval).Should(And(
				HaveField("Request.Call", Equal(infrav2alpha1.ClaimCallIncreaseSize)),
				HaveField("Request.Group", Equal(groupName)),
				HaveField("Request.Delta", BeNumerically(">=", 1)),
				HaveField("RequestedBy", Equal(claim.RequestedByAutoscaler)),
				HaveField("SlotRef.Name", Equal(shutdownNodeName)),
			), "The claim says who asked, through which call, for which pool, by how much, for which slot")

			By("Verifying the claim carries the budget of one attempt")
			Eventually(func() bool {
				fresh, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || fresh == nil || fresh.Spec.DeadlineAt == nil {
					return false
				}

				// The operator's budget, floored by the group's maxNodeProvisionTime: the same
				// computation the provider ran when it created the claim.
				want := claim.DeadlineAt(fresh.Spec.RequestedAt.Time, groupFloor(groupName))

				return fresh.Spec.DeadlineAt.Time.Equal(want)
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"The deadline is the budget of one attempt (job monitor poll + registration wait +\n"+
					"readiness gate + margin), floored by the group's number")

			By("Verifying the claim copies the group's number in force")
			// spec.maxNodeProvisionTime is the number the pool declared when the claim was created:
			// a claim says what budget was in force, and a pool that changes the number does not
			// rewrite the history of an attempt.
			Eventually(func() *metav1.Duration {
				fresh, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || fresh == nil {
					return nil
				}

				return fresh.Spec.MaxNodeProvisionTime
			}, 30*time.Second, checkInterval).Should(
				Equal(groupFloor(groupName)),
				"The claim carries the group's spec.maxNodeProvisionTime, the floor of its deadline")

			By("Verifying the claim carries the TTL that collects it")
			// Every scale-up is an object, so a pool that scales twice a day must not accumulate
			// them: a terminal claim is collected after its TTL, and the slot keeps the digest.
			Eventually(func() bool {
				fresh, err := newestClaimOfSlot(shutdownNodeName)

				return err == nil && fresh != nil && fresh.Spec.TTLSecondsAfterFinished != nil &&
					*fresh.Spec.TTLSecondsAfterFinished >= 0
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"The claim carries spec.ttlSecondsAfterFinished, so nothing accumulates on the pool")

			By("Verifying the claim is owned by the slot it asks for")
			Eventually(func() []string {
				fresh, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || fresh == nil {
					return nil
				}

				owners := make([]string, 0, len(fresh.OwnerReferences))
				for _, ref := range fresh.OwnerReferences {
					if ref.Controller != nil && *ref.Controller {
						owners = append(owners, ref.Kind+"/"+ref.Name)
					}
				}

				return owners
			}, 30*time.Second, checkInterval).Should(Equal([]string{"Node/" + shutdownNodeName}),
				"Deleting the slot from Git takes its claims with it")

			By("Verifying the claim carries a seeded attempt")
			Eventually(func() int32 {
				fresh, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || fresh == nil {
					return 0
				}

				return fresh.Status.Attempt
			}, 30*time.Second, checkInterval).Should(BeNumerically(">=", 1),
				"The claim controller seeds status.attempt from the slot's digest: the bound\n"+
					"on attempts is a bound on consecutive claims")

			By("Verifying the claim is committed: pending -> booting with the startup Job")
			// The pending -> booting edge is the attempt boundary: the same write creates the startup
			// Job, so an attempt is a machine bought. From here the boot is committed - the claim
			// controller's finalizer is on the object, and a cancel is not available to any caller.
			Eventually(func() infrav2alpha1.NodeClaimPhase {
				fresh, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || fresh == nil {
					return ""
				}

				return fresh.Status.Phase
			}, claimPhaseTimeout, checkInterval).Should(Equal(infrav2alpha1.NodeClaimBooting),
				"A live claim moves from pending to booting when the startup Job of the attempt exists")

			By("Verifying the slot reports booting")
			Eventually(func() infrav2alpha1.NodePhase {
				return slotPhase(shutdownNodeName)
			}, nodeStateTimeout, checkInterval).Should(Equal(infrav2alpha1.NodePhaseBooting),
				"The report of a slot with a claim whose startup operation is in flight is booting")

			By("Verifying startup job is created")
			Eventually(func() bool {
				out, err := utils.Run(kubectl("get", "jobs",
					"-l", "type=startup",
					"-n", namespace,
					"-o", "jsonpath={.items[*].metadata.name}"))

				return err == nil && len(strings.TrimSpace(out)) > 0
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"Startup job should be created")

			By("Verifying startup job has correct ServiceAccount")
			Eventually(func() bool {
				return utils.VerifyJobHasServiceAccount(namespace, "type=startup", "controller-manager")
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"Startup job should use controller-manager ServiceAccount")

			By("Verifying startup job has ConfigMap volume")
			Eventually(func() bool {
				return utils.VerifyJobHasVolume(namespace, "type=startup", "startup-config")
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"Startup job should have startup-config volume")

			// Verify TTL is set for automatic job cleanup
			By("Verifying startup job has TTL set")
			Eventually(func() string {
				out, _ := utils.Run(kubectl("get", "jobs",
					"-l", "type=startup",
					"-n", namespace,
					"-o", "jsonpath={.items[*].spec.ttlSecondsAfterFinished}"))

				return strings.TrimSpace(out)
			}, 60*time.Second, checkInterval).Should(
				And(Not(BeEmpty()), Not(Equal("<no value>"))),
				"Startup job should have ttlSecondsAfterFinished set")

			By("Verifying VM control server receives /start request")
			Eventually(func() bool {
				return utils.VerifyVMControlServerRequest(vmControlLogFile, "/start", shutdownNodeName, 30*time.Second)
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"VM control server should receive /start request for %s", shutdownNodeName)

			By("Verifying k3d node starts")
			Eventually(func() bool {
				cmd := exec.Command("k3d", "node", "list", shutdownNodeName)
				output, _ := cmd.CombinedOutput()
				outputStr := strings.ToLower(string(output))
				return strings.Contains(outputStr, "running") || strings.Contains(outputStr, "up")
			}, vmStartupTimeout, checkInterval).Should(BeTrue(),
				"k3d node %s should start", shutdownNodeName)

			By("Verifying startup job completes")
			success, jobLogs, jobEvents := utils.WaitForJobCompletionWithError(namespace, "type=startup", jobCompletionTimeout)
			Expect(success).To(BeTrue(),
				"Startup job should complete successfully. Job Logs: %s. Job Events: %s", jobLogs, jobEvents)

			// The replacement for "Node CR progress becomes ready". The claim's ready phase is the
			// claim's assertion that the machine it asked for can take the pool's work - the node is
			// Ready, uncordoned, and free of the pool's readiness taints - which is a stronger
			// sentence than "the corev1.Node is Ready", and it is the sentence the autoscaler's
			// placement simulation is answered by.
			By("Verifying the claim reaches ready")
			Eventually(func() infrav2alpha1.NodeClaimPhase {
				fresh, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || fresh == nil {
					return ""
				}

				return fresh.Status.Phase
			}, claimReadyTimeout, checkInterval).Should(Equal(infrav2alpha1.NodeClaimReady),
				"The claim of the scale-up reports the machine it asked for is usable")

			By("Verifying the claim's attempt is the number it was seeded with")
			Eventually(func() int32 {
				fresh, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || fresh == nil {
					return 0
				}

				return fresh.Status.Attempt
			}, 30*time.Second, checkInterval).Should(BeNumerically(">=", 1),
				"An attempt is counted by the claim that bought the machine, and ends with it")

			By("Verifying the slot's report is ready, and the machine is observed on")
			Eventually(func() infrav2alpha1.NodePhase {
				return slotPhase(shutdownNodeName)
			}, nodeStateTimeout, checkInterval).Should(Equal(infrav2alpha1.NodePhaseReady),
				"The report of a slot with a ready claim, a Ready node and a live kubelet is ready")

			By("Verifying the observed power state is on")
			Eventually(func() infrav2alpha1.PowerState {
				return slotPowerState(shutdownNodeName)
			}, nodeStateTimeout, checkInterval).Should(Equal(infrav2alpha1.PowerStateOn),
				"The observation of the machine is on, and it is an observation, not an intent")

			By("Verifying the slot keeps the digest of the claim that served the request")
			// The claims are collected after they land; the digest is what `kubectl describe` reads.
			Eventually(func() []string {
				digest := slotDigest(shutdownNodeName)
				if digest == nil {
					return nil
				}

				return []string{digest.Name, string(digest.Phase), digest.Call, digest.Group}
			}, nodeStateTimeout, checkInterval).Should(Equal([]string{
				scaleUpClaimName,
				string(infrav2alpha1.NodeClaimReady),
				infrav2alpha1.ClaimCallIncreaseSize,
				groupName,
			}), "The digest of the slot names the claim that served the request, its phase, its call and its group")

			By("Verifying the digest carries the attempt that landed, and that the streak restarts")
			// The bound on attempts is a bound on consecutive claims. A landed attempt ends the
			// episode, so the next request of this slot is attempt 1: the reset rule is a read of the
			// digest, not a heuristic on the long-lived slot.
			Eventually(func() int32 {
				digest := slotDigest(shutdownNodeName)
				if digest == nil {
					return 0
				}

				return digest.Attempt
			}, nodeStateTimeout, checkInterval).Should(BeNumerically(">=", 1),
				"The digest carries the attempt number of the claim that ended")

			Expect(claim.SeedAttempt(slotDigest(shutdownNodeName))).To(Equal(int32(1)),
				"The next claim of a slot whose last attempt landed starts at 1")

			By("Verifying the slot records the startup of the machine")
			Eventually(func() bool {
				slot, err := readSlot(shutdownNodeName)

				return err == nil && slot.Status.LastStartupTime != nil
			}, nodeStateTimeout, checkInterval).Should(BeTrue(),
				"The slot records when its machine last came up")

			// CRITICAL: Wait for node to be BOTH Ready AND uncordoned (unschedulable=false)
			// The node is cordoned during shutdown to prevent scheduling.
			// The readiness gate uncordons the node as it answers for the claim's ready phase.
			// Pods cannot be scheduled until BOTH conditions are met.
			By("Verifying controller uncordonned the Kubernetes node")
			Eventually(func() error {
				uncordoned, err := utils.VerifyNodeUncordon(shutdownNodeName)
				if err != nil {
					return err
				}
				if !uncordoned {
					utils.PrintNodeStatus(shutdownNodeName)
					return fmt.Errorf("node %s is still cordoned (unschedulable=true)", shutdownNodeName)
				}
				return nil
			}, 120*time.Second, checkInterval).Should(Succeed(),
				"Node %s should be uncordoned (schedulable=true)", shutdownNodeName)

			By("Verifying Kubernetes node is Ready")
			Eventually(func() error {
				ready, err := utils.VerifyNodeReady(shutdownNodeName)
				if err != nil {
					return err
				}
				if !ready {
					utils.PrintNodeStatus(shutdownNodeName)
					return fmt.Errorf("node %s is not Ready", shutdownNodeName)
				}
				return nil
			}, 120*time.Second, checkInterval).Should(Succeed(),
				"Node %s should be Ready", shutdownNodeName)

			// Update Group CR to prevent immediate scale-down during pod startup
			// After the node becomes ready, CA might see it as unneeded (only 1 small pod)
			// and try to scale it down again. We relax the settings to give the pod time.
			By("Updating Group CR to prevent immediate scale-down during pod startup")
			groupYAML := `
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: group1
  namespace: homelab-autoscaler-system
spec:
  ignoreDaemonSetsUtilization: true
  maxNodeProvisionTime: 2m
  scaleDownGpuUtilizationThreshold: "30"
  scaleDownUnneededTime: 2m
  scaleDownUnreadyTime: 30s
  scaleDownUtilizationThreshold: "0.05"
  zeroOrMaxNodeScaling: false
`
			tmpFile, err := os.CreateTemp("", "group-scaleup-*.yaml")
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(tmpFile.Name())
			_, err = tmpFile.WriteString(groupYAML)
			Expect(err).NotTo(HaveOccurred())
			tmpFile.Close()
			cmd = exec.Command("kubectl", "apply", "-f", tmpFile.Name())
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to update Group: %s", output)

			By("Verifying the scale-up of the claim is still standing")
			Consistently(func() infrav2alpha1.NodePhase {
				return slotPhase(shutdownNodeName)
			}, 15*time.Second, checkInterval).Should(Equal(infrav2alpha1.NodePhaseReady),
				"The relaxed scale-down settings keep the machine the claim bought in service")

			By("Verifying deployment pod is scheduled on the correct node")
			Eventually(func() string {
				out, _ := utils.Run(kubectl("get", "pods",
					"-l", "app=hello-world",
					"-n", "default",
					"-o", "jsonpath={.items[*].spec.nodeName}"))

				return strings.TrimSpace(out)
			}, podScheduleTimeout, checkInterval).Should(Equal(shutdownNodeName),
				"Deployment pod should be scheduled on %s", shutdownNodeName)

			By("Scale-Up Workflow completed successfully")
		})
	})
})

// getProjectDir returns the project directory
func getProjectDir() string {
	wd, _ := os.Getwd()
	return strings.Replace(wd, "/test/e2e", "", 1)
}

// =============================================================================
// Reads of the claim model
// =============================================================================
// Every assertion of this suite goes through the API the model exposes, and is typed against
// api/infra/v2alpha1: a slot (Node CR) is read for its derived report and its observation, a
// NodeClaim is read for its phases, its attribution and its attempt. Nothing here reads a field the
// v1alpha1 API had and v2alpha1 removed, and nothing writes a spec field of a slot: the claims are
// created and cancelled by the provider, through the gRPC calls cluster-autoscaler makes, and the
// suite only watches what the model reports.

// kubectl returns a kubectl command wired to the test cluster's kubeconfig.
func kubectl(args ...string) *exec.Cmd {
	cmd := exec.Command("kubectl", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(getProjectDir(), kubeconfigPath))

	return cmd
}

// readSlot reads the Node CR (the slot) named name.
func readSlot(name string) (*infrav2alpha1.Node, error) {
	out, err := utils.Run(kubectl("get", "nodes.infra.homecluster.dev", name, "-n", namespace, "-o", "json"))
	if err != nil {
		return nil, err
	}

	slot := &infrav2alpha1.Node{}
	if err := json.Unmarshal([]byte(out), slot); err != nil {
		return nil, err
	}

	return slot, nil
}

// slotPhase is the derived report of the slot: off, pending, booting, ready or draining. A slot that
// cannot be read reports the empty value, so an Eventually over it polls rather than failing a suite
// on one bad pass.
func slotPhase(name string) infrav2alpha1.NodePhase {
	slot, err := readSlot(name)
	if err != nil {
		return ""
	}

	return slot.Status.Phase
}

// slotIsUnwanted reports the two reports of a slot that no live claim wants a machine on: draining,
// while the machine is observed up and the shutdown path owns it, and off, once the machine is down.
// The scale-down of a slot runs through both, a lifecycle Job apart, so a suite that polled one
// exact value would be asserting a timing, not the model.
func slotIsUnwanted(name string) bool {
	switch slotPhase(name) {
	case infrav2alpha1.NodePhaseOff, infrav2alpha1.NodePhaseDraining:
		return true
	default:
		return false
	}
}

// slotPowerState is the power the provider observes for the slot's machine: an observation, never an
// intent, and the input the state machine reads next to the live claim.
func slotPowerState(name string) infrav2alpha1.PowerState {
	slot, err := readSlot(name)
	if err != nil {
		return ""
	}

	return slot.Status.PowerState
}

// slotDigest is the record of the slot's last claim, the one that survives the claim's collection.
func slotDigest(name string) *infrav2alpha1.LastClaimSummary {
	slot, err := readSlot(name)
	if err != nil {
		return nil
	}

	return slot.Status.LastClaim
}

// readClaims lists the claims of one slot, by the label every claim carries
// (claim.LabelSlot, the key the lifecycle Jobs use too, so a slot's claims and its Jobs are read
// with the same selector).
func readClaims(slotName string) ([]infrav2alpha1.NodeClaim, error) {
	out, err := utils.Run(kubectl("get", "nodeclaims.infra.homecluster.dev", "-n", namespace,
		"-l", claim.LabelSlot+"="+slotName, "-o", "json"))
	if err != nil {
		return nil, err
	}

	list := &infrav2alpha1.NodeClaimList{}
	if err := json.Unmarshal([]byte(out), list); err != nil {
		return nil, err
	}

	return list.Items, nil
}

// liveClaimsOfSlot returns the claims of a slot that mean "a machine is wanted": pending, booting,
// ready - including one a scale-down has marked, which is the record of the request that reverses
// the boot. It is the provider's own predicate (phase.IsLive), so the suite and the arithmetic of
// NodeGroupTargetSize answer the same question.
func liveClaimsOfSlot(slotName string) ([]infrav2alpha1.NodeClaim, error) {
	claims, err := readClaims(slotName)
	if err != nil {
		return nil, err
	}

	live := make([]infrav2alpha1.NodeClaim, 0, len(claims))

	for i := range claims {
		if phase.IsLive(claims[i].Status.Phase) {
			live = append(live, claims[i])
		}
	}

	return live, nil
}

// liveClaimOfSlot is claim.LiveOfSlot over what the API returns: the newest live claim of a slot -
// the one the slot answers for, including a claim a scale-down has marked. The suite asks the
// provider's own question, so a green suite and a green arithmetic are the same statement.
func liveClaimOfSlot(slotName string) (*infrav2alpha1.NodeClaim, error) {
	claims, err := readClaims(slotName)
	if err != nil {
		return nil, err
	}

	return claim.LiveOfSlot(claims), nil
}

// newestClaimOfSlot returns the newest claim of a slot in any phase, live or terminal: the episode
// the slot answers for, and the object the suite follows from pending to its end. It returns nil,
// nil for a slot that has never been claimed.
func newestClaimOfSlot(slotName string) (*infrav2alpha1.NodeClaim, error) {
	claims, err := readClaims(slotName)
	if err != nil {
		return nil, err
	}

	if len(claims) == 0 {
		return nil, nil
	}

	newest := claims[0]
	for _, c := range claims[1:] {
		if c.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = c
		}
	}

	return &newest, nil
}

// groupFloor is the group's spec.maxNodeProvisionTime: the floor of a claim's deadline, and the
// number cluster-autoscaler is told as its own scale-up termination deadline.
func groupFloor(name string) *metav1.Duration {
	out, err := utils.Run(kubectl("get", "group.infra.homecluster.dev", name, "-n", namespace,
		"-o", "jsonpath={.spec.maxNodeProvisionTime}"))
	if err != nil {
		return nil
	}

	value := strings.TrimSpace(out)
	if value == "" || value == "<no value>" {
		return nil
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return nil
	}

	return &metav1.Duration{Duration: parsed}
}
