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

// Shared state between tests
var (
	vmControlServerPID int
	shutdownNodeName   string
)

var _ = BeforeSuite(func() {
	By("Setting up test environment")

	// Set KUBECONFIG environment variable
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

	// Clear VM control server log
	err = utils.ClearLogFile(vmControlLogFile)
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	By("Cleaning up test environment")

	// Untaint nodes
	By("Untainting agent nodes")
	_ = utils.UntaintNode(agentNode0, "node.kubernetes.io/role")
	_ = utils.UntaintNode(agentNode1, "node.kubernetes.io/role")

	// Cleanup Node CRs
	By("Cleaning up Node CRs")
	cmd := exec.Command("kubectl", "delete", "-f", "./examples/k3d/nodes1.yaml", "--ignore-not-found")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, _ = utils.Run(cmd)

	// Cleanup test deployment
	By("Cleaning up test deployment")
	cmd = exec.Command("kubectl", "delete", "-f", "examples/k3d/hello-world-deployment.yaml", "--ignore-not-found")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, _ = utils.Run(cmd)

	// Cleanup Group CR
	By("Cleaning up Group CR")
	cmd = exec.Command("kubectl", "delete", "group", "group1", "-n", namespace, "--ignore-not-found")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, _ = utils.Run(cmd)
})

var _ = Describe("K3d Integration", Serial, func() {

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

			By("Applying secrets and configmaps")
			cmd = exec.Command("kubectl", "apply", "-f", "./examples/k3d/secrets.yaml")
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply secrets/configmaps: %s", output)

			By("Applying Node CRs")
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

			By("Setup & Environment Validation completed successfully")
		})
	})

	Context("Scale-Down Workflow", func() {
		It("should verify Cluster Autoscaler detects unneeded nodes and triggers shutdown", func() {
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

			cmd := exec.Command("kubectl", "apply", "-f", tmpFile.Name())
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Group: %s", output)

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

			By("Verifying Node CR desiredPowerState is set to Off")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", agentNode0,
					"-n", namespace, "-o", "jsonpath={.spec.desiredPowerState}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, _ := utils.Run(cmd)
				return strings.TrimSpace(output)
			}, 30*time.Second, checkInterval).Should(Equal("off"),
				"Node CR desiredPowerState should be off")

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

			By("Verifying VM control server receives /stop request")
			Eventually(func() bool {
				return utils.VerifyVMControlServerRequest(vmControlLogFile, "/stop", agentNode0, 30*time.Second)
			}, 60*time.Second, checkInterval).Should(BeTrue(),
				"VM control server should receive /stop request for %s", agentNode0)

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

			// Store node name for Test 3
			shutdownNodeName = agentNode0

			By("Scale-Down Workflow completed successfully")
		})
	})

	Context("Scale-Up Workflow", func() {
		It("should verify Cluster Autoscaler scales up when pods need scheduling", func() {
			By("Verifying node is in shutdown state from Test 2")
			Expect(shutdownNodeName).ToNot(BeEmpty(),
				"shutdownNodeName should be set by Test 2")

			By("Untainting the shutdown node to allow scheduling")
			err := utils.UntaintNode(shutdownNodeName, "node.kubernetes.io/role")
			Expect(err).NotTo(HaveOccurred())

			By("Verifying node is untainted")
			Eventually(func() bool {
				return !utils.VerifyNodeTaint(shutdownNodeName, "role")
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"Node should be untainted")

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
				logs, _ := utils.GetClusterAutoscalerLogs(namespace, 1000)
				return strings.Contains(logs, "IncreaseSize")
			}, 30*time.Second, checkInterval).Should(BeTrue(),
				"CA should call NodeGroupIncreaseSize")

			By("Verifying Node CR desiredPowerState is set to On")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", shutdownNodeName,
					"-n", namespace, "-o", "jsonpath={.spec.desiredPowerState}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, _ := utils.Run(cmd)
				return strings.TrimSpace(output)
			}, 30*time.Second, checkInterval).Should(Equal("on"),
				"Node CR desiredPowerState should be on")

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
			Eventually(func() bool {
				return utils.WaitForJobCompletion(namespace, "type=startup", jobCompletionTimeout)
			}, jobCompletionTimeout, checkInterval).Should(BeTrue(),
				"Startup job should complete")

			By("Verifying Node CR progress becomes ready")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", shutdownNodeName,
					"-n", namespace, "-o", "jsonpath={.status.progress}")
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				output, _ := utils.Run(cmd)
				return strings.TrimSpace(output)
			}, nodeStateTimeout, checkInterval).Should(Equal("ready"),
				"Node CR progress should be ready")

			By("Verifying Kubernetes node becomes Ready")
			Eventually(func() bool {
				return utils.WaitForNodeReady(shutdownNodeName, true, nodeStateTimeout)
			}, nodeStateTimeout, checkInterval).Should(BeTrue(),
				"Kubernetes node should be Ready")

			By("Verifying deployment pod is scheduled and running")
			Eventually(func() bool {
				return utils.WaitForPodScheduled("default", "app=hello-world", podScheduleTimeout)
			}, podScheduleTimeout, checkInterval).Should(BeTrue(),
				"Deployment pod should be scheduled and running")

			By("Scale-Up Workflow completed successfully")
		})
	})
})

// getProjectDir returns the project directory
func getProjectDir() string {
	wd, _ := os.Getwd()
	return strings.Replace(wd, "/test/e2e", "", 1)
}
