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
	caTimeout       = 120 * time.Second
	caInterval      = 5 * time.Second
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

			By("Setting up gRPC service forwarder")
			setupServiceForwarder()

			By("Waiting for cluster autoscaler to be ready")
			waitForClusterAutoscaler()

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

func setupServiceForwarder() {
	By("Setting up gRPC service forwarder to localhost:50052")

	hostIP := "host.k3d.internal"

	fwdYAML := fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: homelab-autoscaler-grpc-local
  namespace: %s
  labels:
    app.kubernetes.io/name: homelab-autoscaler
    app.kubernetes.io/component: grpc-forwarder
spec:
  ports:
  - port: 50051
    targetPort: 50052
    protocol: TCP
---
apiVersion: v1
kind: Endpoints
metadata:
  name: homelab-autoscaler-grpc-local
  namespace: %s
  labels:
    app.kubernetes.io/name: homelab-autoscaler
    app.kubernetes.io/component: grpc-forwarder
subsets:
- addresses:
  - ip: %s
  ports:
  - port: 50052
    protocol: TCP
`, namespace, namespace, hostIP)

	tmpFile, err := os.CreateTemp("", "fwd-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(fwdYAML)
	Expect(err).NotTo(HaveOccurred())
	tmpFile.Close()

	cmd := exec.Command("kubectl", "apply", "-f", tmpFile.Name())
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply service forwarder: %s", output)

	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "service", "homelab-autoscaler-grpc-local",
			"-n", namespace, "-o", "name")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		_, err := utils.Run(cmd)
		return err == nil
	}, caTimeout, caInterval).Should(BeTrue(), "Service forwarder should be created")
}

func waitForClusterAutoscaler() {
	By("Waiting for cluster autoscaler to be ready")

	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "pods", "-l", "app.kubernetes.io/component=cluster-autoscaler",
			"-o", "jsonpath={.items[*].status.phase}", "-n", namespace)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			return false
		}
		return strings.Contains(output, "Running")
	}, caTimeout, caInterval).Should(BeTrue(), "Cluster autoscaler pod should be running")

	Eventually(func() bool {
		cmd := exec.Command("kubectl", "logs", "-l", "app.kubernetes.io/component=cluster-autoscaler",
			"-n", namespace, "--tail=100")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		return err == nil && strings.Contains(output, "Cluster Autoscaler")
	}, caTimeout, caInterval).Should(BeTrue(), "Cluster autoscaler should be initialized")
}

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
	By("Waiting for node to be scaled down in complete sequence: ToBeDeletedByClusterAutoscaler -> DeletionCandidateOfClusterAutoscaler -> SchedulingDisabled -> NotReady")

	// Step 1: Wait for ToBeDeletedByClusterAutoscaler taint
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.taints}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			return false
		}
		return strings.Contains(output, "ToBeDeletedByClusterAutoscaler")
	}, 300*time.Second, 5*time.Second).Should(BeTrue(), "Node should get ToBeDeletedByClusterAutoscaler taint")

	// Step 2: Wait for DeletionCandidateOfClusterAutoscaler taint
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.taints}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			return false
		}
		return strings.Contains(output, "DeletionCandidateOfClusterAutoscaler")
	}, 300*time.Second, 5*time.Second).Should(BeTrue(), "Node should get DeletionCandidateOfClusterAutoscaler taint")

	// Step 3: Wait for SchedulingDisabled (spec.unschedulable = true)
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.unschedulable}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			return false
		}
		return strings.TrimSpace(output) == "true"
	}, 300*time.Second, 5*time.Second).Should(BeTrue(), "Node should be marked unschedulable")

	// Step 4: Wait for node status to become NotReady
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			return false
		}
		status := strings.TrimSpace(output)
		return status == "False" || status == "Unknown"
	}, 300*time.Second, 5*time.Second).Should(BeTrue(), "Node should be marked NotReady")
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
