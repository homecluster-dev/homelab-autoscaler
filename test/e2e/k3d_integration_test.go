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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/homecluster-dev/homelab-autoscaler/test/utils"
)

const (
	clusterName     = "homelab-autoscaler-k3d-e2e"
	grpcServerPort  = "50052"
	kubeconfigPath  = "./kubeconfig"
	serverPidFile   = "/tmp/homelab-autoscaler-server.pid"
	nodeName        = "k3d-homelab-autoscaler-agent-0"
	namespace       = "homelab-autoscaler-system"
	defaultTimeout  = 600 * time.Second
	defaultInterval = 5 * time.Second
)

var _ = Describe("K3d Integration", func() {

	BeforeEach(func() {
		By("Setting up test environment")

		// Set KUBECONFIG environment variable
		err := os.Setenv("KUBECONFIG", kubeconfigPath)
		Expect(err).NotTo(HaveOccurred())
	})

	Context("Full workflow test", func() {
		It("should complete the full scaling workflow", func() {

			By("Applying group and node CRs")
			applyTestManifests()

			By("Waiting for node to be scaled down")
			waitForNodeScaleDown()

			By("Applying hello-world deployment")
			applyHelloWorldDeployment()

			By("Verifying deployment runs successfully")
			verifyDeploymentRunning()

			By("Deleting the deployment")
			deleteHelloWorldDeployment()

			By("Waiting for node shutdown after cleanup period")
			waitForNodeShutdown()

			By("Waiting for node to be scaled down after deployment deleted")
			waitForNodeScaleDown()

			By("Test completed successfully")
		})
	})
})

func applyTestManifests() {
	By("Applying group and node CRs from example manifests")

	manifestFiles := []string{
		"./examples/k3d/group1.yaml",
		"./examples/k3d/nodes1.yaml",
	}

	for _, manifestFile := range manifestFiles {
		cmd := exec.Command("kubectl", "apply", "-f", manifestFile)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	}

	// Wait for resources to be created
	Eventually(func() error {
		cmd := exec.Command("kubectl", "get", "groups.infra.homecluster.dev", "group1", "-o", "name", "-n", "homelab-autoscaler-system")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		_, err := utils.Run(cmd)
		return err
	}, defaultTimeout, defaultInterval).Should(Succeed())

	Eventually(func() error {
		cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", nodeName, "-o", "name", "-n", "homelab-autoscaler-system")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		_, err := utils.Run(cmd)
		return err
	}, defaultTimeout, defaultInterval).Should(Succeed())
}

func waitForNodeScaleDown() {
	By("Waiting for node to be scaled down (checking for taint and SchedulingDisabled)")

	Eventually(func() bool {
		// Check if kubernetes node has the expected taint
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.taints}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			return false
		}

		// Look for scale-down taint or unschedulable status
		return strings.Contains(output, "ToBeDeletedByClusterAutoscaler") ||
			strings.Contains(output, "DeletionCandidateOfClusterAutoscaler")
	}, defaultTimeout, defaultInterval).Should(BeTrue())

	// Also check if node is marked as unschedulable
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.unschedulable}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			return false
		}
		return strings.TrimSpace(output) == "true"
	}, defaultTimeout, defaultInterval).Should(BeTrue())
}

func applyHelloWorldDeployment() {
	By("Applying hello-world deployment")

	cmd := exec.Command("kubectl", "apply", "-f", "examples/k3d/hello-world-deployment.yaml")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

func verifyDeploymentRunning() {
	By("Verifying deployment runs successfully")

	// Wait for deployment to be ready
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "deployment", "hello-world-deployment", "-o", "jsonpath={.status.readyReplicas}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			return false
		}
		readyReplicas := strings.TrimSpace(output)
		return readyReplicas == "1"
	}, defaultTimeout, defaultInterval).Should(BeTrue())

	// Verify pod is running
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "pods", "-l", "app=hello-world", "-o", "jsonpath={.items[0].status.phase}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			return false
		}
		return strings.TrimSpace(output) == "Running"
	}, defaultTimeout, defaultInterval).Should(BeTrue())
}

func deleteHelloWorldDeployment() {
	By("Deleting hello-world deployment")

	cmd := exec.Command("kubectl", "delete", "-f", "examples/k3d/hello-world-deployment.yaml")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	// Wait for deployment to be deleted
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "deployment", "hello-world-deployment")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		_, err := utils.Run(cmd)
		return err != nil // Should return error when deployment doesn't exist
	}, defaultTimeout, defaultInterval).Should(BeTrue())
}

func waitForNodeShutdown() {
	By("Waiting for node shutdown after cleanup period")

	// Wait for the node to be removed or marked for deletion
	// This might take some time based on the scaleDownUnneededTime setting (30s in our config)
	Eventually(func() bool {
		// Check if node CR still exists and has appropriate status
		cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", nodeName, "-o", "jsonpath={.status.powerState}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			// Node might be deleted, which is also a valid end state
			return true
		}

		powerState := strings.TrimSpace(output)
		// Node should be powered off or in process of shutting down
		return powerState == "off" || powerState == "shutting-down"
	}, 600*time.Second, 10*time.Second).Should(BeTrue())
}
