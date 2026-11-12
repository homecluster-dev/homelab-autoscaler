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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
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

	// harnessRequestedBy is the attribution the harness stamps on a claim it writes for one of its own
	// simulated machines. The harness owns the machines (they are k3d containers it starts and stops),
	// so it is the caller for them, and the model says so on the object: a machine is up exactly when a
	// live NodeClaim names its slot, and the claim that says so names who asked.
	harnessRequestedBy = "e2e"

	// Timeouts - various timeouts for different operations
	vmShutdownTimeout    = 60 * time.Second  // Time for VM to power off
	vmStartupTimeout     = 60 * time.Second  // Time for VM to power on
	jobCompletionTimeout = 120 * time.Second // Time for startup/shutdown job to complete
	podScheduleTimeout   = 200 * time.Second // Time for pod to be scheduled after node is ready
	nodeStateTimeout     = 120 * time.Second // Time for the slot's report to follow an observation

	// The two gates cluster-autoscaler is asked to pass are decisions of a process outside this suite,
	// so their clocks are the autoscaler's, not a reconcile's.

	// caDecisionTimeout is how long the autoscaler may take to decide: its loop period, the group's
	// scaleDownUnneededTime window measured from the moment the machine became Ready and the
	// autoscaler finished registering it, and - for a scale-up - the simulation of the pending pod.
	caDecisionTimeout = 5 * time.Minute
	// deleteNodesRPCTimeout is how long the autoscaler may take, once it has logged the decision, to
	// have the call reach the provider: the taint delay (--node-delete-delay-after-taint) and the drain.
	deleteNodesRPCTimeout = 2 * time.Minute
	// machineStateTimeout is how long an observation of a machine - a k3d container coming up or down,
	// and the kubelet's heartbeat following it - may take to reach the slot's report. The node lifecycle
	// controller declares a silent kubelet only after its monitor grace period, so an off-report is
	// minutes late by construction, and a shutdown Job has to run before the machine is even asked to
	// stop.
	machineStateTimeout = 4 * time.Minute

	// The window of the "a served claim stays ready past its boot deadline" spec is measured from the
	// claim's own deadline, and bounded: minDeadlineWatch is the shortest honest observation, and
	// maxDeadlineWatch keeps one spec from eating the suite. The floor is not decoration: a claim's
	// budget is the operator's claim.AttemptBudget (12m10s) and the group's spec.maxNodeProvisionTime
	// is a floor that only ever raises it (claim.DeadlineAt), so no pool number shortens a deadline
	// below one boot window, and a window shorter than that says nothing about a machine that outlives
	// its boot budget.
	minDeadlineWatch = 3 * time.Minute
	maxDeadlineWatch = 14 * time.Minute
	// deadlineWatchMargin is how long past the claim's deadline the observation keeps going: the passes
	// that would act on an overdue claim are a reconcile apart from the deadline itself.
	deadlineWatchMargin = 2 * time.Minute

	// logTailLines is how much of a log the gates read. cluster-autoscaler writes its loop bookkeeping
	// every pass, so a tail of 1000 lines is a couple of minutes of history: a decision made before the
	// suite started watching has to be visible, and a decision that made the suite's gate has to be
	// findable.
	logTailLines = 5000

	// bootRequestWindow is the window the VM control server's request log is read over for one power
	// transition. Each spec watches its own boot and its own shutdown, so the window covers the request
	// rate of a transition the suite starts from the claim: a second, not a whole run. It is wide enough
	// for a Job pod that starts slowly and narrow enough that the /stop of the setup's power-off, minutes
	// earlier, is not the request the scale-down spec reads.
	bootRequestWindow = 90 * time.Second

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
	// machine is no longer wanted". It is measured from the machine being observed off, so it is
	// pinned to machineStateTimeout, not to the autoscaler's clocks.
	claimCollectedTimeout = 3 * time.Minute

	// Check intervals - how often to poll for state changes
	checkInterval = 5 * time.Second
)

// claimCancelled is the answer of the read that asks "has the scale-down named this machine": it is
// returned by a slot whose claim carries the DeleteNodes request and the mark, and by a slot that has
// no live claim left to cancel, because both are the shape the request asked for.
const claimCancelled = "cancelled"

// Shared state between tests
// These variables persist across test contexts and are used to track state
var (
	shutdownNodeName string // Name of the node that was shut down (for scale-up test)

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
//
// The harness is the machine. The pool's machines are k3d containers the suite starts and stops
// through the VM control server, so the suite is the provider's other half: a simulated machine that
// is up has a live NodeClaim naming its slot, and a slot nobody claims is a machine the pool does not
// want, which the operator powers off. A suite that left its machines unclaimed would be exercising
// cluster-autoscaler on a group of machines that are already going down, and the autoscaler has
// nothing to scale down. The lifecycle helpers claimSlot and releaseSlot are the harness playing its
// half of the model, and they are the only writes this suite makes to a claim.
// =============================================================================
var _ = Describe("K3d Integration", Serial, func() {
	// The harness stops wanting its machines when the run is over: the claims it wrote for the k3d
	// containers are deleted, and the model powers the machines down. This is housekeeping, not a gate -
	// what a release does is asserted by the spec that reads the slot, not here.
	AfterAll(func() {
		By("Releasing the machines the harness claimed")
		releaseSlot(agentNode0)
		releaseSlot(agentNode1)
	})

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
	// 6. Verify the slots start free: no live NodeClaim names them, and the machines nobody claimed
	//    are observed coming down
	// 7. Claim the slot of the machine the scale-down spec runs on, and follow the claim to ready:
	//    the harness is the machine, so a simulated machine in service is a claimed slot
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
			// A slot is configuration: its identity, its machine, the Jobs that power it on and off, and
			// no field of it carries a scale-up, so applying this file cannot contradict a claim and there
			// is nothing for selfHeal to revert.
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
			applyGroupCR(groupYAML)

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
			// Nothing is seeded, no status is scanned and no observation is needed to know it, so the
			// slots are readable as free the instant they are applied.
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

			By("Verifying the machines nobody claimed are coming down")
			// The k3d agents booted with the cluster, so their machines are observed up while no claim
			// names them. A machine is wanted exactly when a live claim names its slot, so these two are
			// unwanted capacity: the shutdown path owns them, and the slot's report is draining on the way
			// down and off once the machine is down. The pair is one shape - a machine the pool does not
			// want - a Job and a reconcile apart, so the suite reads the pair and then the off end of it.
			for _, slot := range []string{agentNode0, agentNode1} {
				Eventually(func() bool {
					return slotIsUnwanted(slot)
				}, machineStateTimeout, checkInterval).Should(BeTrue(),
					"A slot no claim names reports off, or draining while its machine comes down")
			}

			// The off end is the shape the scale-down spec starts from, and the shape the harness's claim
			// boots a machine out of: a live claim on a slot whose machine is observed off is a request
			// the input law answers by booting it. Claiming a slot whose machine is already observed up
			// is the shape nothing boots - the machine is up and the claim has not been stamped ready, so
			// the slot reports the boot window and no startup Job is owed - so the suite waits for the
			// machines to be down before it asks for one.
			Eventually(func() []infrav2alpha1.NodePhase {
				return []infrav2alpha1.NodePhase{slotPhase(agentNode0), slotPhase(agentNode1)}
			}, machineStateTimeout, checkInterval).Should(Equal([]infrav2alpha1.NodePhase{
				infrav2alpha1.NodePhaseOff,
				infrav2alpha1.NodePhaseOff,
			}), "A machine the pool does not want ends off: the pool reads as idle, and every slot is free")

			By("Claiming the slot of the machine the scale-down spec runs on")
			// The harness is the machine: the pool's machines are k3d containers it starts and stops, and
			// nothing powers them for the pool. So a simulated machine that is in service is up because a
			// live NodeClaim names its slot, and the claim is written by the harness with the attribution
			// of a scale-up (call: IncreaseSize, the group, a delta) and a deadline. From there the model
			// runs: law 1 boots the machine the claim wants, the startup Job asks the VM control server
			// for it, the readiness gate answers for the claim's ready phase.
			//
			// The scale-down spec needs a machine the autoscaler can find Ready, empty and long enough in
			// service to call unneeded. The other slot stays unclaimed on purpose: it is the idle pool of
			// a homelab, readable as off, and the free slot the scale-up of the next spec asks for.
			claimSlot(agentNode0, harnessRequestedBy)

			By("Verifying the claimed machine is in service")
			// The VM control server was asked for the machine: the startup Job of the attempt is what
			// powers a k3d container, so the request in its log is the boot, not the cluster creation.
			Eventually(func() bool {
				return utils.VerifyVMControlServerRequest(vmControlLogFile, "/start", agentNode0, bootRequestWindow)
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"the machine of a claimed slot is booted by the provider's startup Job, so the VM control server saw /start for it")

			// The claim's ready phase is the sentence "the machine it asked for can take the pool's
			// work", and the slot's report follows it: ready, with the machine observed on.
			Eventually(func() infrav2alpha1.NodePhase {
				return slotPhase(agentNode0)
			}, nodeStateTimeout, checkInterval).Should(Equal(infrav2alpha1.NodePhaseReady),
				"The report of a slot with a ready claim, a Ready node and a live kubelet is ready")

			Eventually(func() infrav2alpha1.PowerState {
				return slotPowerState(agentNode0)
			}, nodeStateTimeout, checkInterval).Should(Equal(infrav2alpha1.PowerStateOn),
				"The observation of the machine the claim asked for is on")

			By("Setup & Environment Validation completed successfully")
		})
	})

	// =============================================================================
	// TEST 2: Scale-Down Workflow
	// =============================================================================
	// Purpose: Verify that Cluster Autoscaler decides to remove a machine the pool no longer needs,
	// and that the model powers it off.
	//
	// Flow:
	// 1. The machine is in service because a live NodeClaim names its slot (the setup's claim): the node
	//    is Ready, the slot reports ready, and the group has an empty node to consider.
	// 2. CA decides: it writes "Scale-down: removing empty node <name>" as it taints the node and starts
	//    the deletion, then calls the provider's NodeGroupDeleteNodes.
	// 3. The provider cancels the machine: the live claim of the slot is recorded with the request
	//    (call: DeleteNodes, nodes: the names) and deleted. A booting or ready claim keeps its
	//    finalizer until the machine is observed off; a slot with no live claim has nothing to cancel,
	//    and the input law powers its machine off on the slot's next pass.
	// 4. The slot's shutdown transition runs: cordon, shutdown Job, power off.
	// 5. The shutdown Job curls the VM control server's /stop endpoint, and the VM control server stops
	//    the k3d node.
	// 6. The machine is observed off: the slot's report is off, the marked claim ends completed and is
	//    released, and the slot's digest keeps the episode.
	//
	// Key validations:
	// - CA's decision is in its own log naming this machine, and the RPC reached the provider: two gates
	//   in that order, each on a line a pass that decided nothing does not write
	// - the claim that wanted the machine is cancelled: the request recorded on it (call: DeleteNodes,
	//   the nodes named) and the mark that runs the shutdown
	// - the slot's report is the machine coming down, and then off, and the observed power is off
	// - the shutdown Job of this machine, from this decision, is the one asserted on: ServiceAccount,
	//   Secret volume, TTL, and a completion read off job.status.succeeded
	// - VM control server received /stop for this machine, and the k3d node actually stopped
	// - the claim stops being live, and the slot's digest of the episode records an ended claim
	// - Kubernetes node became NotReady
	// =============================================================================
	Context("Scale-Down Workflow", func() {
		It("should verify Cluster Autoscaler detects unneeded nodes and triggers shutdown", func() {
			// The minute the suite starts watching from. Every lifecycle Job asserted below is one this
			// decision created: the setup's shutdown of the same slot left a completed shutdown Job
			// behind, and a Job of a past episode is not the evidence that this scale-down ran.
			scaleDownStarted := time.Now()

			By("Waiting for Cluster Autoscaler to decide this machine is unneeded")
			// The word "unneeded" is bookkeeping the autoscaler writes on every pass: it echoes its own
			// --scale-down-unneeded-time at startup, and its tracker prints "<node> was unneeded for 3s"
			// while a node is only a candidate. The sentence the actuator writes when it commits to a
			// machine names the machine:
			//
			//	Scale-down: removing empty node "k3d-homelab-autoscaler-agent-0"
			//	Scale-down: removing node k3d-homelab-autoscaler-agent-0, utilization: ...
			//
			// It is written as the node is tainted and the deletion starts, so the gate is a decision, and
			// it is about this machine.
			Eventually(func() bool {
				logs, _ := utils.GetClusterAutoscalerLogs(namespace, logTailLines)

				return caLoggedScaleDownOf(logs, agentNode0)
			}, caDecisionTimeout, checkInterval).Should(BeTrue(),
				"Cluster Autoscaler must log its scale-down decision for %s. Check CA logs.", agentNode0)

			By("Verifying gRPC NodeGroupDeleteNodes call was made")
			// The autoscaler taints, waits --node-delete-delay-after-taint, drains, and only then calls
			// the provider, so this follows the decision by seconds and is asserted after it, not beside it.
			Eventually(func() bool {
				logs, _ := utils.GetHomeClusterAutoscalerLogs(namespace, logTailLines)

				return strings.Contains(logs, "NodeGroupDeleteNodes called")
			}, deleteNodesRPCTimeout, checkInterval).Should(BeTrue(),
				"homelab-autoscaler should receive NodeGroupDeleteNodes: the provider logs the call it answers")

			By("Verifying the provider cancelled the claim of this machine")
			// The RPC line names the group and a count, so a scale-down of the pool's other machine answers
			// it. The provider's cancellation line names the slot and the claim, and it is written on the
			// write that takes the machine out of the target size.
			Eventually(func() bool {
				logs, _ := utils.GetHomeClusterAutoscalerLogs(namespace, logTailLines)

				return providerCancelledClaimOf(logs, agentNode0)
			}, deleteNodesRPCTimeout, checkInterval).Should(BeTrue(),
				"the provider must answer the scale-down of %s by cancelling the claim that wanted it",
				agentNode0)

			By("Verifying the claim of the machine is marked, with the request on it")
			// A machine is wanted exactly when a live claim names its slot, so this is where a scale-down
			// is readable: the provider records the request on the live claim (call: DeleteNodes, nodes:
			// the names) and deletes it. A booting or ready claim keeps its finalizer while the machine
			// drains, so at this instant the claim is live and marked; the slot with no live claim at all
			// is pinned below, once the machine is observed off.
			Eventually(func() string {
				live, err := liveClaimOfSlot(agentNode0)
				if err != nil {
					return "the claims of the slot are unreadable: " + err.Error()
				}

				if live == nil {
					// Nothing left to cancel: a pending claim has no finalizer, so it goes outright and the
					// machine comes down by the input law.
					return claimCancelled
				}

				if live.DeletionTimestamp.IsZero() {
					return fmt.Sprintf("claim %s is live and unmarked: the scale-down has not named it", live.Name)
				}

				if live.Spec.Request.Call != infrav2alpha1.ClaimCallDeleteNodes {
					return fmt.Sprintf("claim %s is marked, but its request is call %q, not DeleteNodes",
						live.Name, live.Spec.Request.Call)
				}

				if !requestNamesNode(live.Spec.Request.Nodes, agentNode0) {
					return fmt.Sprintf("claim %s records a DeleteNodes request for %v, which does not name %s",
						live.Name, live.Spec.Request.Nodes, agentNode0)
				}

				return claimCancelled
			}, deleteNodesRPCTimeout, checkInterval).Should(Equal(claimCancelled),
				"A scale-down is a request on an object: the claim of the slot carries the call that ended it,\n"+
					"the nodes it named, and the mark that runs the shutdown")

			By("Verifying the slot reports the machine coming down")
			// The machine is observed up and its claim is named for deletion: that row of the input table
			// reports draining - cordoned, with the shutdown path owning the slot. The off end of the
			// sequence is pinned below, after the Job has run and the machine is down.
			Eventually(func() infrav2alpha1.NodePhase {
				return slotPhase(agentNode0)
			}, machineStateTimeout, checkInterval).Should(Equal(infrav2alpha1.NodePhaseDraining),
				"The report of a slot whose machine is observed up while a scale-down names its claim is draining")

			var shutdownJob string

			By("Verifying the shutdown job of this machine is created")
			// The selector is the slot, and the Job is the one that started after the autoscaler decided.
			// "a shutdown Job exists in the namespace" is answered by the Job the setup's shutdown of the
			// same slot left behind, which is a true sentence about nothing this spec did.
			Eventually(func() string {
				shutdownJob = newestLifecycleJobOfSlot(fsm.PhaseShutdown, agentNode0, scaleDownStarted)

				return shutdownJob
			}, 90*time.Second, checkInterval).ShouldNot(BeEmpty(),
				"the shutdown transition of the scale-down creates a shutdown Job for %s",
				agentNode0)

			By("Verifying the shutdown job carries the spec the slot declares")
			// One read of the typed object, and one sentence: the controller's ServiceAccount, the Secret
			// volume the slot declares for its shutdown hook, and a TTL so a pool that scales twice a day
			// does not accumulate Jobs.
			Eventually(func() string {
				job, err := readJob(shutdownJob)
				if err != nil {
					return err.Error()
				}

				return lifecycleJobSpecProblem(job, "controller-manager", "auth-secrets")
			}, 60*time.Second, checkInterval).Should(BeEmpty(),
				"the shutdown Job of the scale-down is the Job the slot's shutdownPodSpec declares")

			By("Verifying VM control server receives /stop request")
			// The request log is read over a window: a /stop for this machine in the last bootRequestWindow.
			// The setup stopped this machine once already, and a gate that read the whole file is answered by
			// that older request.
			Eventually(func() bool {
				return utils.VerifyVMControlServerRequest(vmControlLogFile, "/stop", agentNode0, bootRequestWindow)
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"VM control server should receive /stop request for %s", agentNode0)

			By("Verifying shutdown job completes")
			// The count is read off job.status.succeeded. The suite used to accept any Job in the namespace
			// whose status string contained a "1", which a Job of another slot, or of a past episode, answers.
			Eventually(func() string {
				job, err := readJob(shutdownJob)
				if err != nil {
					return err.Error()
				}

				if job.Status.Succeeded >= 1 {
					return ""
				}

				if job.Status.Failed >= 1 {
					logs, logErr := utils.GetJobLogs(namespace, lifecycleJobSelector(fsm.PhaseShutdown, agentNode0))

					return fmt.Sprintf("job %s failed: %s", job.Name, logs+" "+logErr.Error())
				}

				return fmt.Sprintf("job %s is running: active=%d succeeded=%d failed=%d",
					job.Name, job.Status.Active, job.Status.Succeeded, job.Status.Failed)
			}, jobCompletionTimeout, checkInterval).Should(BeEmpty(),
				"the shutdown Job of the scale-down runs the power-off the slot declares")

			By("Verifying k3d node stops")
			Eventually(func() bool {
				cmd := exec.Command("k3d", "node", "list", agentNode0)
				output, _ := cmd.CombinedOutput()
				outputStr := strings.ToLower(string(output))
				return !strings.Contains(outputStr, "running") && !strings.Contains(outputStr, "up")
			}, vmShutdownTimeout, checkInterval).Should(BeTrue(),
				"k3d node %s should stop", agentNode0)

			// The slot's report is derived from (the live claim, the observed power, the usability of the
			// node). The machine is off and its claim has ended: nothing is wanted and nothing is running,
			// which is the off row of the table, and the power the provider observes is off with it.
			By("Verifying the slot's report becomes off")
			Eventually(func() infrav2alpha1.NodePhase {
				return slotPhase(agentNode0)
			}, machineStateTimeout, checkInterval).Should(Equal(infrav2alpha1.NodePhaseOff),
				"The derived phase of a free slot with no machine is off")

			By("Verifying the machine is observed off")
			Eventually(func() infrav2alpha1.PowerState {
				return slotPowerState(agentNode0)
			}, machineStateTimeout, checkInterval).Should(Equal(infrav2alpha1.PowerStateOff),
				"The observed power state of the slot is off")

			By("Verifying the scale-down left no claim owning the slot")
			// A claim a scale-down marked keeps its finalizer until the machine is observed off, then ends
			// completed and is released. The target size of the group counts live claims, so the unit came
			// off on the write that named the machine: from here the slot is free, and the next IncreaseSize
			// may claim it.
			Eventually(func() int {
				live, err := liveClaimsOfSlot(agentNode0)
				if err != nil {
					return -1
				}

				return len(live)
			}, claimCollectedTimeout, checkInterval).Should(Equal(0),
				"The claim of the episode is not live: the slot is free, and the next IncreaseSize may claim it")

			By("Verifying the slot's digest records an episode that ended")
			// The claims are collected by their TTL and the story survives in Node.status.lastClaim. A slot
			// whose machines were never claimed has no digest at all, which is the empty case of this read;
			// what must never happen is a digest that reports a live phase on a slot whose machine is off.
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
			// The kubelet of a stopped machine stops beating, and the node lifecycle controller declares the
			// node NotReady after its monitor grace period: an observation, minutes late by construction.
			Eventually(func() bool {
				out, _ := utils.Run(kubectl("get", "node", agentNode0,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}"))
				status := strings.TrimSpace(out)

				return status == "False" || status == "Unknown" || status == ""
			}, machineStateTimeout, checkInterval).Should(BeTrue(),
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
	// 2. CA decides to grow the group: it writes "Scale-up: setting group group1 size to N" as it calls
	//    the provider, because the pending pod fits no existing node
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
	// - CA's decision is in its own log naming the pool, and the RPC reached the provider
	// - a NodeClaim exists for the slot, and it is the record of the request: attribution, deadline,
	//   attempt, TTL, ownerReference
	// - the claim's phases are the phases of the model: pending -> booting (committed) -> ready
	// - the slot's derived phase follows the claim: off -> booting -> ready
	// - the observed power state is on
	// - the startup Job of this attempt, on this slot, carries the spec the slot declares
	//   (ServiceAccount, volume, TTL) and its status.succeeded says it ran
	// - VM control server received /start for this machine, and the k3d node actually started
	// - the slot keeps the digest of the claim that served the request, and a startup timestamp
	// - Kubernetes node became Ready AND uncordoned (unschedulable=false)
	// - a ready claim that crosses its boot deadline keeps its machine: the observation outlives the
	//   budget the claim names, because a machine that serves the pool is past its boot budget by any
	//   measure, and nothing may read that as a reason to power it off
	// - Deployment pod scheduled on the correct node
	//
	// IMPORTANT: The node must be BOTH Ready AND unschedulable=false before
	// pods can be scheduled. The readiness gate of the scale-up uncordons the node and answers for
	// the claim's ready phase; a node that clears neither is not reported ready, and its claim fails
	// at the gate's timeout (reason ReadinessGateTimeout).
	// =============================================================================
	Context("Scale-Up Workflow", func() {
		It("should verify Cluster Autoscaler scales up when pods need scheduling", func() {
			// The minute this spec starts watching from, for the same reason as in the scale-down spec: the
			// lifecycle Jobs of earlier episodes of this slot exist, and they are not this attempt's.
			scaleUpStarted := time.Now()

			By("Verifying node is off from Test 2")
			Expect(shutdownNodeName).ToNot(BeEmpty(),
				"shutdownNodeName should be set by Test 2")
			Expect(slotPhase(shutdownNodeName)).To(Equal(infrav2alpha1.NodePhaseOff),
				"The slot of the scale-up starts free: off, and no live claim names it")

			// The shape IncreaseSize reads as "claimable": the scale-down of the last spec ended the claim
			// that wanted this machine, so nothing names the slot and the group has a free slot to hand out.
			Eventually(func() int {
				live, err := liveClaimsOfSlot(shutdownNodeName)
				if err != nil {
					return -1
				}

				return len(live)
			}, 30*time.Second, checkInterval).Should(Equal(0),
				"The slot the scale-up starts from is free: no live NodeClaim names it")

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

			By("Waiting for Cluster Autoscaler to decide to grow the group")
			// "scale up" is in the autoscaler's log on every idle pass - "Nothing to do: no pods and no need
			// to scale up" is the sentence of a scale-up that did not happen. The orchestrator writes its
			// decision, naming the pool, as it calls the provider:
			//
			//	Scale-up: setting group group1 size to 2
			//
			// which is the gate: a plan for this pool, on this pass.
			Eventually(func() bool {
				logs, _ := utils.GetClusterAutoscalerLogs(namespace, logTailLines)

				return caLoggedScaleUpOf(logs, groupName)
			}, caDecisionTimeout, checkInterval).Should(BeTrue(),
				"Cluster Autoscaler must log its scale-up decision for group %s. Check CA logs.", groupName)

			By("Verifying gRPC NodeGroupIncreaseSize call was made")
			Eventually(func() bool {
				logs, _ := utils.GetHomeClusterAutoscalerLogs(namespace, logTailLines)

				return strings.Contains(logs, "NodeGroupIncreaseSize called")
			}, deleteNodesRPCTimeout, checkInterval).Should(BeTrue(),
				"homelab-autoscaler should receive NodeGroupIncreaseSize: the provider logs the call it answers")

			// A scale-up is an object. The provider creates it on a free slot, and on it is the whole
			// request: who asked, through which call, for which pool, by how much, with what budget, and
			// which attempt of that slot it is.
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
				"The deadline is the budget of one attempt (startup Job timeout + job monitor poll +\n"+
					"registration wait + readiness gate + margin), floored by the group's number")

			By("Verifying the budget outlasts the deadlines one attempt can spend in series")
			// The deadline runs from spec.requestedAt, i.e. from before the startup Job exists, so one
			// attempt can spend the Job's own timeout, then the registration wait, then the readiness gate -
			// about 10m30s of deadlines. A budget that omits the Job's timeout fails a claim in the middle
			// of the machine it is still buying, and the machine that boots an instant later is
			// force-shut-down as overdue.
			Eventually(func() bool {
				fresh, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || fresh == nil || fresh.Spec.DeadlineAt == nil {
					return false
				}

				budget := fresh.Spec.DeadlineAt.Time.Sub(fresh.Spec.RequestedAt.Time)

				return budget >= claimSerialDeadlines()
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"The claim's budget is at least the Job timeout + the registration wait + the readiness gate")

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

			var startupJob string

			By("Verifying the startup job of this machine is created")
			// The pending -> booting write of the claim is what creates the startup Job, so the Job the
			// scale-up owes is one this attempt created. Scoped to the slot, and to the minute this spec
			// started watching: the setup's boot of the same machine left a completed startup Job behind.
			Eventually(func() string {
				startupJob = newestLifecycleJobOfSlot(fsm.PhaseStartup, shutdownNodeName, scaleUpStarted)

				return startupJob
			}, 90*time.Second, checkInterval).ShouldNot(BeEmpty(),
				"the startup transition of the claim creates a startup Job for %s", shutdownNodeName)

			By("Verifying the startup job carries the spec the slot declares")
			// One read of the typed object, and one sentence: the controller's ServiceAccount, the ConfigMap
			// the slot's startupPodSpec mounts, and a TTL so a pool that scales twice a day does not
			// accumulate Jobs.
			Eventually(func() string {
				job, err := readJob(startupJob)
				if err != nil {
					return err.Error()
				}

				return lifecycleJobSpecProblem(job, "controller-manager", "startup-config")
			}, 60*time.Second, checkInterval).Should(BeEmpty(),
				"the startup Job of the scale-up is the Job the slot's startupPodSpec declares")

			By("Verifying VM control server receives /start request")
			// The request log is read over the last 30s, so this is the /start of this boot: the setup's
			// claim booted the same machine earlier, and its /start is in the log too.
			Eventually(func() bool {
				return utils.VerifyVMControlServerRequest(vmControlLogFile, "/start", shutdownNodeName, bootRequestWindow)
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"VM control server should receive /start request for %s", shutdownNodeName)

			By("Verifying k3d node starts")
			Eventually(func() bool {
				cmd := exec.Command("k3d", "node", "list", shutdownNodeName)
				output, _ := cmd.CombinedOutput()
				outputStr := strings.ToLower(string(output))

				return strings.Contains(outputStr, "running")
			}, vmStartupTimeout, checkInterval).Should(BeTrue(),
				"k3d node %s should start", shutdownNodeName)

			By("Verifying startup job completes")
			// The count is read off job.status.succeeded, for this attempt's Job.
			Eventually(func() string {
				job, err := readJob(startupJob)
				if err != nil {
					return err.Error()
				}

				if job.Status.Succeeded >= 1 {
					return ""
				}

				if job.Status.Failed >= 1 {
					logs, logErr := utils.GetJobLogs(namespace, lifecycleJobSelector(fsm.PhaseStartup, shutdownNodeName))

					return fmt.Sprintf("job %s failed: %s %s", job.Name, logs, logErr)
				}

				return fmt.Sprintf("job %s is running: active=%d succeeded=%d failed=%d",
					job.Name, job.Status.Active, job.Status.Succeeded, job.Status.Failed)
			}, jobCompletionTimeout, checkInterval).Should(BeEmpty(),
				"the startup Job of the scale-up asks the machine for power")

			// The claim's ready phase is the claim's assertion that the machine it asked for can take the
			// pool's work - the node is Ready, uncordoned, and free of the pool's readiness taints - which is
			// a stronger sentence than "the corev1.Node is Ready", and it is the sentence the autoscaler's
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
			// After the node becomes ready, CA might see it as unneeded (only 1 small pod) and try to scale
			// it down again, so the unneeded window is relaxed well past the observation this spec makes
			// below: the machine the claim bought stays in service while the suite watches it, and the
			// autoscaler's scale-down of a machine is the scale-down spec's subject, not this one's. The
			// pool's maxNodeProvisionTime is raised in the same write because it is the number
			// cluster-autoscaler is told as its own scale-up termination deadline (NodeGroupGetOptions): a
			// k3d machine boots in minutes, and an autoscaler that gives up at 2m sends a second IncreaseSize
			// for the pod that is already on its way - onto the pool's other slot, which is the machine the
			// pod must not land on. The operator's budget for one attempt is longer than 5m, so the claim
			// deadlines of this run are unchanged (claim.DeadlineAt takes the larger of the two).
			By("Updating Group CR to prevent immediate scale-down during pod startup")
			groupYAML := `
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: group1
  namespace: homelab-autoscaler-system
spec:
  ignoreDaemonSetsUtilization: true
  maxNodeProvisionTime: 5m
  scaleDownGpuUtilizationThreshold: "30"
  scaleDownUnneededTime: 20m
  scaleDownUnreadyTime: 30s
  scaleDownUtilizationThreshold: "0.05"
  zeroOrMaxNodeScaling: false
`
			applyGroupCR(groupYAML)

			By("Verifying the scale-up of the claim is still standing")
			Consistently(func() infrav2alpha1.NodePhase {
				return slotPhase(shutdownNodeName)
			}, 15*time.Second, checkInterval).Should(Equal(infrav2alpha1.NodePhaseReady),
				"The relaxed scale-down settings keep the machine the claim bought in service")

			By("Verifying a boot deadline in the past leaves a ready claim alone")
			// The deadline is the budget of the boot window, and a machine that serves the pool for an hour
			// is past it by any measure. A controller that applies the deadline to a ready claim destroys
			// working capacity on a timer: the claim lands failed/DeadlineExceeded, a shutdown Job runs, the
			// node is cordoned, and the pod on it is evicted. The write below manufactures the shape every
			// healthy machine reaches - a ready claim with a deadline in its past - and the window below
			// outlives the budget the claim names, so the same shape is watched as the claim crosses its own
			// deadline inside the observation, and not only as a stamped fake.
			serving, err := newestClaimOfSlot(shutdownNodeName)
			Expect(err).NotTo(HaveOccurred())
			Expect(serving).NotTo(BeNil())

			// The natural deadline of the attempt, read off the object before it is stamped: the window is
			// measured from it, so the observation crosses the deadline the provider wrote.
			naturalDeadline := serving.Spec.DeadlineAt

			// The moment the served claim is in service: the shutdown Jobs asserted on below are the ones
			// that start now, not the Job the scale-down of the previous spec left on the same slot.
			servingSince := time.Now()

			Expect(stampClaimDeadline(serving.Name, time.Now().Add(-3*time.Hour))).To(Succeed(),
				"The deadline of a served claim can be pushed into the past by anything that writes the object")

			watch := deadlineWatchWindow(naturalDeadline)

			Consistently(func() string {
				served, err := newestClaimOfSlot(shutdownNodeName)
				if err != nil || served == nil {
					return "claim-gone"
				}

				if string(served.Status.Phase) != string(infrav2alpha1.NodeClaimReady) {
					return "claim-" + string(served.Status.Phase) + "-" + served.Status.Reason
				}

				if slotPhase(shutdownNodeName) != infrav2alpha1.NodePhaseReady {
					return "slot-" + string(slotPhase(shutdownNodeName))
				}

				// A shutdown Job is the force shutdown of a machine the model has stopped wanting: the first
				// visible write of it, behind a claim that is serving.
				if job := newestLifecycleJobOfSlot(fsm.PhaseShutdown, shutdownNodeName, servingSince); job != "" {
					return "shutdown-job: " + job
				}

				if !nodeSchedulable(shutdownNodeName) {
					return "cordoned"
				}

				return "ready"
			}, watch, checkInterval).Should(Equal("ready"),
				"A ready claim past its boot deadline stays ready: the machine, its node object, and the pod on it are left alone")

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
// The harness as the machine
// =============================================================================
// The pool's machines are k3d containers, and the only thing that powers them is the VM control server
// the suite runs. So the suite is the provider's counterpart: it writes the claim that says a machine
// is wanted, and it stops wanting the ones it takes out of service. These are the only writes this
// suite makes to a claim, and they are the shape of the machine lifecycle the provider drives: a claim
// created on a free slot boots the machine, a claim deleted stops wanting it.
// =============================================================================

// harnessClaimName is the name of the claim the harness writes for one of its machines. The name is
// deterministic, so claiming a slot twice is one apply that replaces the request, not two objects
// competing for one slot - which is the rule the model enforces on every writer.
func harnessClaimName(slotName string) string {
	return slotName + "-e2e"
}

// claimSlot is the harness asking for a machine on a slot: it writes the NodeClaim with the
// attribution a scale-up carries (request.call: IncreaseSize, the group, a delta, the requester) and
// its budget, and then waits for the claim to report ready. Everything after the write is the model:
// a live claim on a slot whose machine is observed off is a request the input law answers by booting
// it, the startup Job asks the VM control server for the machine, and the readiness gate stamps the
// claim ready once the node is Ready, uncordoned and free of the pool's readiness taints.
//
// A caller must claim a slot whose machine is observed off. A claim on a slot whose machine is already
// up is a request nothing boots - the law starts no startup Job for a machine that is running - so the
// suite brings its machines down first, exactly as a pool whose machines are off starts.
func claimSlot(slotName, requestedBy string) *infrav2alpha1.NodeClaim {
	GinkgoHelper()

	slot, err := readSlot(slotName)
	Expect(err).NotTo(HaveOccurred(), "the slot of a machine the harness boots must be readable")

	out, err := applyObject(newHarnessClaim(slot, requestedBy))
	Expect(err).NotTo(HaveOccurred(), "Failed to apply the harness's NodeClaim for slot %s: %s", slotName, out)

	// The phase, with the reason beside it: a claim that ends failed names why, and the suite says so.
	Eventually(func() string {
		fresh, err := newestClaimOfSlot(slotName)
		if err != nil {
			return "claims-unreadable"
		}

		if fresh == nil {
			return "no-claim"
		}

		return string(fresh.Status.Phase) + "/" + fresh.Status.Reason
	}, claimReadyTimeout, checkInterval).Should(Equal(string(infrav2alpha1.NodeClaimReady)+"/"),
		"the claim the harness wrote for slot %s reaches ready: the machine is up, joined, and usable",
		slotName)

	fresh, err := newestClaimOfSlot(slotName)
	Expect(err).NotTo(HaveOccurred())
	Expect(fresh).NotTo(BeNil())

	return fresh
}

// newHarnessClaim is the claim of a simulated machine: the same object the provider's IncreaseSize
// writes (internal/claim.New), built by the harness for a machine it owns. The attribution is the
// provider's - the call that made the request, the pool it was for, the delta - and the requestedBy
// names the caller, so `kubectl get nodeclaim` says which requests came from the autoscaler and which
// from the harness's own machines.
func newHarnessClaim(slot *infrav2alpha1.Node, requestedBy string) *infrav2alpha1.NodeClaim {
	requestedAt := time.Now()
	ttl := claim.DefaultTTLSecondsAfterFinished
	owner := true
	floor := groupFloor(groupName)
	deadline := metav1.NewTime(claim.DeadlineAt(requestedAt, floor))

	return &infrav2alpha1.NodeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infrav2alpha1.GroupVersion.String(),
			Kind:       "NodeClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      harnessClaimName(slot.Name),
			Namespace: slot.Namespace,
			Labels: map[string]string{
				claim.LabelSlot:  slot.Name,
				claim.LabelGroup: groupName,
			},
			// The claim is owned by the slot it asks for, so deleting a slot takes its claims with it.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         infrav2alpha1.GroupVersion.String(),
				Kind:               "Node",
				Name:               slot.Name,
				UID:                slot.UID,
				Controller:         &owner,
				BlockOwnerDeletion: &owner,
			}},
		},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef: corev1.ObjectReference{
				APIVersion: infrav2alpha1.GroupVersion.String(),
				Kind:       "Node",
				Namespace:  slot.Namespace,
				Name:       slot.Name,
				UID:        slot.UID,
			},
			RequestedAt: metav1.NewTime(requestedAt),
			RequestedBy: requestedBy,
			Request: infrav2alpha1.ClaimRequest{
				Call:  infrav2alpha1.ClaimCallIncreaseSize,
				Delta: 1,
				Group: groupName,
			},
			DeadlineAt:              &deadline,
			MaxNodeProvisionTime:    floor,
			TTLSecondsAfterFinished: &ttl,
		},
	}
}

// releaseSlot is the harness stopping wanting one of its machines: it deletes the live claim of the
// slot. A pending claim goes outright, and the input law powers the machine down; a booting or ready
// claim is deleted through its finalizer, so the shutdown path runs to the end and the object is
// released once the machine is observed off.
//
// It reports to the Ginkgo writer rather than gating on the observation: its callers are the specs that
// hand a machine back at the end of a run, and a cleanup that fails a suite for a slow power-off would
// be a suite that fails on its own housekeeping. What the suite asserts about a release is asserted by
// the spec that reads the slot.
func releaseSlot(slotName string) {
	GinkgoHelper()

	live, err := liveClaimOfSlot(slotName)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "releaseSlot(%s): the claims of the slot are unreadable: %v\n", slotName, err)

		return
	}

	if live == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "releaseSlot(%s): no live claim names the slot\n", slotName)

		return
	}

	out, err := utils.Run(kubectl("delete", "nodeclaim", live.Name, "-n", namespace, "--ignore-not-found"))
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "releaseSlot(%s): deleting claim %s: %s\n", slotName, live.Name, out)
	}
}

// applyGroupCR writes a Group CR and applies it. The suite changes the pool's numbers mid-run - the
// aggressive scale-down window the scale-down spec needs, the relaxed one the scale-up spec needs - and
// writes them through the pool's own API, on a temp file that is closed and removed by this helper.
func applyGroupCR(body string) {
	GinkgoHelper()

	tmpFile, err := os.CreateTemp("", "group-*.yaml")
	Expect(err).NotTo(HaveOccurred())

	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}()

	_, err = tmpFile.WriteString(body)
	Expect(err).NotTo(HaveOccurred())

	out, err := utils.Run(kubectl("apply", "-f", tmpFile.Name()))
	Expect(err).NotTo(HaveOccurred(), "Failed to apply the Group: %s", out)
}

// applyObject writes one typed object through the API server: the suite's own claims go through the
// same admission and the same storage as the provider's writes, with no client set and no YAML string
// to keep in step with the API.
func applyObject(obj client.Object) (string, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}

	cmd := kubectl("apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(data))

	return utils.Run(cmd)
}

// =============================================================================
// Reads of the claim model
// =============================================================================
// Every assertion of this suite goes through the API the model exposes, and is typed against
// api/infra/v2alpha1: a slot (Node CR) is read for its derived report and its observation, a NodeClaim
// is read for its phases, its attribution and its attempt, and a lifecycle Job is read for the spec the
// slot declares. Nothing here reads a field a slot's spec carries as runtime intent, because the model
// has none: a scale-up is a claim, a scale-down is the end of one, and the machine is what the
// observation says it is. The claims are written by the provider through the gRPC calls
// cluster-autoscaler makes, and by the harness for the machines the harness owns (see above).

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

// claimSerialDeadlines is the chain of deadlines one attempt can consume in series: the startup Job's
// own timeout, then the registration wait, then the readiness gate. The claim's budget is measured from
// spec.requestedAt, before the Job exists, so it has to outlast all three.
func claimSerialDeadlines() time.Duration {
	return fsm.DefaultJobTimeout + fsm.DefaultUncordonTimeout + fsm.DefaultReadinessGateTimeout
}

// stampClaimDeadline writes spec.deadlineAt of a claim: the shape every healthy machine's claim reaches
// when its boot budget has run out, manufactured on the live object so a controller that reads the
// deadline as a lifetime is caught in one poll. The observation that follows it is measured from the
// claim's natural deadline, so the same assertion is also made as the claim crosses the budget the
// provider wrote.
func stampClaimDeadline(name string, at time.Time) error {
	stamp := fmt.Sprintf(`{"spec":{"deadlineAt":%q}}`, at.UTC().Format(time.RFC3339))
	_, err := utils.Run(kubectl("patch", "nodeclaim", name, "-n", namespace,
		"--type", "merge", "-p", stamp))

	return err
}

// nodeSchedulable reports whether the Kubernetes node of a slot is still schedulable: a cordon is
// the first write of a shutdown, so a serving machine that is cordoned is the symptom, visible
// seconds before the power-off lands.
func nodeSchedulable(slotName string) bool {
	out, err := utils.Run(kubectl("get", "node", slotName, "-o", "jsonpath={.spec.unschedulable}"))
	if err != nil {
		return true
	}

	return strings.TrimSpace(out) != "true"
}

// newestClaimOfSlot returns the newest claim of a slot in any phase, live or terminal: the episode the
// slot answers for, and the object the suite follows from pending to its end. It returns nil, nil for a
// slot that has never been claimed.
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

// =============================================================================
// The gates: a decision in a log, a Job of this attempt, a window past a deadline
// =============================================================================
// A gate is only a gate if the sentence it matches cannot be true when the thing it is watching for
// did not happen. The three helpers below are the reads that hold that line: cluster-autoscaler's
// decision, the lifecycle Job of the attempt that is running, and the window that outlives the budget
// a claim names.
// =============================================================================

// caLoggedScaleDownOf reports that cluster-autoscaler logged a scale-down decision for a node. The
// actuator writes one of these two lines as it taints the node and starts the deletion, at V(0):
//
//	Scale-down: removing empty node "k3d-homelab-autoscaler-agent-0"
//	Scale-down: removing node k3d-homelab-autoscaler-agent-0, utilization: ...
//
// The word "unneeded" is deliberately not consulted: the autoscaler prints it as bookkeeping on every
// pass (its flag echo, and "<node> was unneeded for 3s" from the tracker that counts the candidate's
// timer), so a gate built on it is true on a run where the autoscaler decided nothing.
func caLoggedScaleDownOf(logs, nodeName string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "Scale-down: removing") && strings.Contains(line, nodeName) {
			return true
		}
	}

	return false
}

// caLoggedScaleUpOf reports that cluster-autoscaler logged the decision to grow a node group. The
// orchestrator writes it, naming the pool and the size, as it calls the provider:
//
//	Scale-up: setting group group1 size to 2
//
// "scale up" alone is not a decision: the autoscaler's idle pass prints "no need to scale up", and a
// line that is true when nothing happened cannot gate on it.
func caLoggedScaleUpOf(logs, group string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "Scale-up: setting group "+group+" size to") {
			return true
		}
	}

	return false
}

// requestNamesNode reports that a DeleteNodes request names a Kubernetes node: the attribution the
// provider records on the claim it cancels, read back as the machine it was about.
func requestNamesNode(nodes []string, name string) bool {
	for _, n := range nodes {
		if n == name {
			return true
		}
	}

	return false
}

// providerCancelledClaimOf reports that the provider's log records the cancellation of the claim of one
// slot. NodeGroupDeleteNodes writes the line on the write that records the request on a live claim and
// marks it, so it is the RPC and the machine it was for, read from the same place: the bare
// "NodeGroupDeleteNodes called" line names the node group and a count of nodes, which a scale-down of
// the pool's other machine answers.
func providerCancelledClaimOf(logs, slotName string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "Cancelled the claim of a node named for deletion") &&
			strings.Contains(line, slotName) {
			return true
		}
	}

	return false
}

// lifecycleJobSelector is the selector of one slot's lifecycle Jobs: the phase label the suite has
// always read (type=startup, type=shutdown) and the slot the Job belongs to, so a Job of the other
// slot cannot answer for this machine.
func lifecycleJobSelector(jobPhase, slotName string) string {
	return fmt.Sprintf("type=%s,%s=%s", jobPhase, fsm.LabelNodeCR, slotName)
}

// newestLifecycleJobOfSlot is the name of the newest lifecycle Job of a slot in the given phase created
// at or after `since`, and "" when there is none. The cut is the point: a slot that has been booted and
// shut down twice has a Job of every past episode, and the evidence that this transition ran is the
// Job this transition created.
func newestLifecycleJobOfSlot(jobPhase, slotName string, since time.Time) string {
	out, err := utils.Run(kubectl("get", "jobs", "-n", namespace,
		"-l", lifecycleJobSelector(jobPhase, slotName), "-o", "json"))
	if err != nil {
		return ""
	}

	list := &batchv1.JobList{}
	if err := json.Unmarshal([]byte(out), list); err != nil {
		return ""
	}

	var newest *batchv1.Job

	for i := range list.Items {
		job := &list.Items[i]
		if job.CreationTimestamp.Time.Before(since) {
			continue
		}

		if newest == nil || job.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = job
		}
	}

	if newest == nil {
		return ""
	}

	return newest.Name
}

// readJob reads one Job by name, typed: the assertions about a lifecycle Job are made on the fields
// Kubernetes defines, not on a jsonpath string compared to "<no value>".
func readJob(name string) (*batchv1.Job, error) {
	if name == "" {
		return nil, fmt.Errorf("no lifecycle Job name to read")
	}

	out, err := utils.Run(kubectl("get", "job", name, "-n", namespace, "-o", "json"))
	if err != nil {
		return nil, err
	}

	job := &batchv1.Job{}
	if err := json.Unmarshal([]byte(out), job); err != nil {
		return nil, err
	}

	return job, nil
}

// lifecycleJobSpecProblem is the shape a lifecycle Job of this pool must have, read off the typed
// object: the controller's ServiceAccount, the volume the slot declares for its hook, and a TTL so the
// Jobs of a pool that scales twice a day are collected. "" means the Job is as declared; anything else
// is the sentence an operator reads.
func lifecycleJobSpecProblem(job *batchv1.Job, serviceAccount, volumeName string) string {
	if job == nil {
		return "the Job is not readable"
	}

	if got := job.Spec.Template.Spec.ServiceAccountName; got != serviceAccount {
		return fmt.Sprintf("job %s runs with serviceAccount %q, not %q", job.Name, got, serviceAccount)
	}

	mounted := make([]string, 0, len(job.Spec.Template.Spec.Volumes))
	found := false

	for _, volume := range job.Spec.Template.Spec.Volumes {
		mounted = append(mounted, volume.Name)

		if volume.Name == volumeName {
			found = true
		}
	}

	if !found {
		return fmt.Sprintf("job %s mounts %v, which has no %q volume", job.Name, mounted, volumeName)
	}

	if job.Spec.TTLSecondsAfterFinished == nil {
		return fmt.Sprintf("job %s has no ttlSecondsAfterFinished: a pool that scales twice a day accumulates Jobs", job.Name)
	}

	return ""
}

// deadlineWatchWindow is how long the "a ready machine stays ready past its deadline" observation has
// to run: from now until the claim's own deadline, plus a margin for the passes that would act on it
// once it has passed. A window that stops short of the deadline has watched a claim that is still
// inside its boot budget, which is the shape the assertion is not about.
//
// The window is measured, not hardcoded, because the budget is a sum of the operator's constants
// (claim.AttemptBudget: the startup Job timeout, the job monitor poll, the registration wait, the
// readiness gate and a margin) and the group's spec.maxNodeProvisionTime is a floor that only raises
// it (claim.DeadlineAt). No pool number brings a deadline under one boot window, so the only honest
// choice is a window that outlives it.
func deadlineWatchWindow(deadline *metav1.Time) time.Duration {
	watch := minDeadlineWatch

	if deadline != nil {
		if until := time.Until(deadline.Add(deadlineWatchMargin)); until > watch {
			watch = until
		}
	}

	if watch > maxDeadlineWatch {
		watch = maxDeadlineWatch
	}

	return watch
}
