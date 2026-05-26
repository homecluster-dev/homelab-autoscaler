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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

	// Node names - these must match the k3d cluster configuration
	agentNode0 = "k3d-homelab-autoscaler-agent-0"
	agentNode1 = "k3d-homelab-autoscaler-agent-1"

	// Taints - prevents workloads from being scheduled on agent nodes
	agentTaint = "node.kubernetes.io/role=agent:NoSchedule"

	// Timeouts - various timeouts for different operations
	caDetectionTimeout   = 60 * time.Second // Time for CA to detect need for scale
	vmShutdownTimeout    = 60 * time.Second // Time for VM to power off
	vmStartupTimeout     = 60 * time.Second // Time for VM to power on
	jobCompletionTimeout = 120 * time.Second // Time for startup/shutdown job to complete
	podScheduleTimeout   = 200 * time.Second // Time for pod to be scheduled after node is ready
	nodeStateTimeout     = 120 * time.Second // Time for node state changes

	// Check intervals - how often to poll for state changes
	checkInterval     = 5 * time.Second
	fastCheckInterval = 1 * time.Second
)

// Shared state between tests
// These variables persist across test contexts and are used to track state
var (
	vmControlServerPID int // PID of the VM control server process
	shutdownNodeName   string // Name of the node that was shut down (for scale-up test)
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
	// 4. Apply Node CRs that define our agent nodes
	// 5. Apply Group CR with aggressive scale-down settings for testing
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
			cmd = exec.Command("kubectl", "get", "service", "homelab-autoscaler-grpc-local",
				"-n", namespace)
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			// Service may not exist yet - that's okay, it will be created by the test
			_, _ = utils.Run(cmd)

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

			By("Applying Node CRs")
			// Node CRs define our agent nodes with startup/shutdown pod specs
			// These pods will curl the VM control server to power nodes on/off
			cmd = exec.Command("kubectl", "apply", "-f", "./examples/k3d/nodes1.yaml")
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Node CRs: %s", output)

			By("Verifying Node CRs are created")
			Eventually(func() error {
				cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev",
					agentNode0, "-n", namespace)
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				_, err := utils.Run(cmd)
				return err
			}, 30*time.Second, checkInterval).Should(Succeed(),
				"Node CRs should be created")

			// Apply Group CR with aggressive scale-down settings for testing
			// scaleDownUnneededTime: 30s - CA will consider node unneeded after 30s
			// scaleDownUtilizationThreshold: 0.1 - Node with <10% utilization is unneeded
			By("Applying Group CR with aggressive scale-down settings")
			groupYAML := `
apiVersion: infra.homecluster.dev/v1alpha1
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
	// 3. Controller updates Node CR powerState to "off"
	// 4. Controller creates shutdown job that curls VM control server /stop endpoint
	// 5. VM control server stops the k3d node
	// 6. Controller uncordons node and updates status
	//
	// Key validations:
	// - gRPC call was made
	// - Node CR powerState changed
	// - Shutdown job created with correct spec (ServiceAccount, volumes, TTL)
	// - VM control server received /stop request
	// - k3d node actually stopped
	// - Node CR progress reached "shutdown" state
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

			By("Verifying Node CR powerState is set to Off")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", agentNode0,
					"-n", namespace, "-o", "jsonpath={.spec.powerState }")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, _ := utils.Run(cmd)
				return strings.TrimSpace(output)
			}, 30*time.Second, checkInterval).Should(Equal("off"),
				"Node CR powerState should be off")

			By("Verifying shutdown job is created")
			Eventually(func() bool {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", "type=shutdown",
					"-n", namespace,
					"-o", "jsonpath={.items[*].metadata.name}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
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
			Eventually(func() bool {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", "type=shutdown",
					"-n", namespace,
					"-o", "jsonpath={.items[*].spec.ttlSecondsAfterFinished}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, err := utils.Run(cmd)
				return err == nil && len(strings.TrimSpace(output)) > 0 && output != "<no value>"
			}, 60*time.Second, checkInterval).Should(BeTrue(),
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

			By("Verifying Node CR progress becomes shutdown")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", agentNode0,
					"-n", namespace, "-o", "jsonpath={.status.progress}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, _ := utils.Run(cmd)
				return strings.TrimSpace(output)
			}, nodeStateTimeout, checkInterval).Should(Equal("shutdown"),
				"Node CR progress should be shutdown")

			By("Verifying Kubernetes node becomes NotReady")
			Eventually(func() bool {
				cmd := exec.Command("kubectl", "get", "node", agentNode0,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, _ := utils.Run(cmd)
				status := strings.TrimSpace(string(output))
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
	// 4. Controller updates Node CR powerState to "on"
	// 5. Controller creates startup job that curls VM control server /start endpoint
	// 6. VM control server starts the k3d node
	// 7. Controller uncordons the node when it's ready
	// 8. Deployment pod gets scheduled on the now-ready node
	//
	// Key validations:
	// - gRPC call was made
	// - Node CR powerState changed
	// - Startup job created with correct spec (ServiceAccount, volumes, TTL)
	// - VM control server received /start request
	// - k3d node actually started
	// - Node CR progress reached "ready" state
	// - Kubernetes node became Ready AND uncordoned (unschedulable=false)
	// - Deployment pod scheduled on the correct node
	//
	// IMPORTANT: The node must be BOTH Ready AND unschedulable=false before
	// pods can be scheduled. The controller uncordons the node in the
	// afterJobCompleted hook when transitioning to StateReady state.
	// =============================================================================
	Context("Scale-Up Workflow", func() {
		It("should verify Cluster Autoscaler scales up when pods need scheduling", func() {
			By("Verifying node is in shutdown state from Test 2")
			Expect(shutdownNodeName).ToNot(BeEmpty(),
				"shutdownNodeName should be set by Test 2")

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

			By("Verifying Node CR powerState is set to On")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", shutdownNodeName,
					"-n", namespace, "-o", "jsonpath={.spec.powerState}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, _ := utils.Run(cmd)
				return strings.TrimSpace(output)
			}, 30*time.Second, checkInterval).Should(Equal("on"),
				"Node CR powerState should be on")

			By("Verifying startup job is created")
			Eventually(func() bool {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", "type=startup",
					"-n", namespace,
					"-o", "jsonpath={.items[*].metadata.name}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, err := utils.Run(cmd)
				return err == nil && len(strings.TrimSpace(output)) > 0
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
			Eventually(func() bool {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-l", "type=startup",
					"-n", namespace,
					"-o", "jsonpath={.items[*].spec.ttlSecondsAfterFinished}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, err := utils.Run(cmd)
				return err == nil && len(strings.TrimSpace(output)) > 0 && output != "<no value>"
			}, 60*time.Second, checkInterval).Should(BeTrue(),
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

			By("Verifying Node CR progress becomes ready")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", shutdownNodeName,
					"-n", namespace, "-o", "jsonpath={.status.progress}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, _ := utils.Run(cmd)
				return strings.TrimSpace(output)
			}, nodeStateTimeout, checkInterval).Should(Equal("ready"),
				"Node CR progress should be ready")

			// CRITICAL: Wait for node to be BOTH Ready AND uncordoned (unschedulable=false)
			// The node is cordoned during shutdown to prevent scheduling.
			// The controller uncordons it in the afterJobCompleted hook when
			// transitioning to StateReady state.
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
apiVersion: infra.homecluster.dev/v1alpha1
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

			By("Verifying deployment pod is scheduled on the correct node")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app=hello-world",
					"-n", "default",
					"-o", "jsonpath={.items[*].spec.nodeName}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, _ := utils.Run(cmd)
				return strings.TrimSpace(output)
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
