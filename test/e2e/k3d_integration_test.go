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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/homecluster-dev/homelab-autoscaler/test/utils"
)

const (
	clusterName     = "homelab-autoscaler"
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

	Context("Volumes and ServiceAccount support", func() {
		It("should verify jobs have ServiceAccount, ConfigMap and Secret volumes", func() {

			By("Setting up test ConfigMaps and Secrets")
			setupTestConfigMapsAndSecrets()

			By("Applying test node manifests with volumes and ServiceAccount")
			applyTestNodeManifestsWithVolumes()

			By("Starting the node to trigger startup job creation")
			startTestNode()

			By("Verifying startup job has correct ServiceAccount")
			verifyStartupJobServiceAccount()

			By("Verifying startup job has ConfigMap volume")
			verifyStartupJobConfigMapVolume()

			By("Waiting for startup job to complete")
			waitForStartupJobCompletion()

			By("Verifying shutdown job has Secret volume")
			verifyShutdownJobSecretVolume()

			By("Cleaning up test resources")
			cleanupTestResources()
		})
	})

	Context("Full workflow test", func() {
		It("should complete the full scaling workflow", func() {

			By("Setting up gRPC service forwarder")
			setupServiceForwarder()

			By("Setting up host-internal service")
			setupHostInternalService()

			By("Waiting for cluster autoscaler to be ready")
			waitForClusterAutoscaler()

			By("Applying group with aggressive scale-down settings")
			applyTestGroupWithAggressiveScaleDown()

			By("Applying node CRs")
			applyTestNodeManifests()

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

func getK3dGatewayIP() string {
	cmd := exec.Command("docker", "network", "inspect", "k3d-"+clusterName, "-f", "{{(index .IPAM.Config 0).Gateway}}")
	output, err := utils.Run(cmd)
	if err != nil {
		By(fmt.Sprintf("Warning: docker network inspect failed: %v", err))
		return ""
	}
	gatewayIP := strings.TrimSpace(output)
	By(fmt.Sprintf("Found k3d gateway IP: %s", gatewayIP))
	return gatewayIP
}

func setupServiceForwarder() {
	By("Setting up gRPC service forwarder to localhost:50052")

	gatewayIP := getK3dGatewayIP()
	Expect(gatewayIP).NotTo(BeEmpty(), "Failed to get k3d gateway IP. "+
		"Ensure k3d cluster '%s' is running: k3d cluster create '%s'", clusterName, clusterName)
	By(fmt.Sprintf("Using k3d gateway IP: %s", gatewayIP))

	fwdYAML := fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: homelab-autoscaler-grpc-local
  namespace: %s
  labels:
    app: homelab-autoscaler
    type: grpc-service-forwarder
spec:
  ports:
  - port: 50051
    targetPort: 50052
    protocol: TCP
    name: grpc
  selector:
    app: cluster-autoscaler
---
apiVersion: v1
kind: Endpoints
metadata:
  name: homelab-autoscaler-grpc-local
  namespace: %s
subsets:
- addresses:
  - ip: %s
  ports:
  - port: 50052
    protocol: TCP
`, namespace, namespace, gatewayIP)

	tmpFile, err := os.CreateTemp("", "grpc-fwd-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(fwdYAML)
	Expect(err).NotTo(HaveOccurred())
	tmpFile.Close()

	cmd := exec.Command("kubectl", "apply", "-f", tmpFile.Name())
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply gRPC service forwarder: %s", output)
}

func setupHostInternalService() {
	By("Setting up host-internal service for startup/shutdown pods")

	gatewayIP := getK3dGatewayIP()
	Expect(gatewayIP).NotTo(BeEmpty(), "Failed to get k3d gateway IP")

	hostSvcYAML := fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: host-internal
  namespace: %s
spec:
  clusterIP: None
  ports:
  - port: 8080
    targetPort: 8080
    protocol: TCP
---
apiVersion: v1
kind: Endpoints
metadata:
  name: host-internal
  namespace: %s
subsets:
- addresses:
  - ip: %s
  ports:
  - port: 8080
    protocol: TCP
`, namespace, namespace, gatewayIP)

	tmpFile, err := os.CreateTemp("", "host-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(hostSvcYAML)
	Expect(err).NotTo(HaveOccurred())
	tmpFile.Close()

	cmd := exec.Command("kubectl", "apply", "-f", tmpFile.Name())
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply host-internal service: %s", output)
}

func applyTestGroupWithAggressiveScaleDown() {
	By("Applying test group with aggressive scale-down settings")

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
	Expect(err).NotTo(HaveOccurred(), "Failed to apply group: %s", output)
}

func waitForClusterAutoscaler() {
	By("Waiting for cluster autoscaler pod to be Ready")
	Eventually(func() string {
		cmd := exec.Command("kubectl", "get", "pods",
			"-l", "app.kubernetes.io/component=cluster-autoscaler",
			"-n", namespace,
			"-o", `jsonpath={.items[*].status.conditions[?(@.type=="Ready")].status}`)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, _ := utils.Run(cmd)
		return strings.TrimSpace(output)
	}, caTimeout, caInterval).Should(Equal("True"), "Cluster autoscaler pod should be Ready")

	By("Waiting for cluster autoscaler to acquire leader lease")
	Eventually(func() string {
		cmd := exec.Command("kubectl", "get", "lease", "cluster-autoscaler",
			"-n", "kube-system",
			"-o", "jsonpath={.spec.holderIdentity}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, _ := utils.Run(cmd)
		return strings.TrimSpace(output)
	}, caTimeout, caInterval).ShouldNot(BeEmpty(), "Cluster autoscaler should hold leader lease")
}

func applyTestNodeManifests() {
	By("Applying node CRs from example manifests")

	cmd := exec.Command("kubectl", "apply", "-f", "./examples/k3d/nodes1.yaml")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	// Wait for node CR to be created
	Eventually(func() error {
		cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", nodeName, "-o", "name", "-n", "homelab-autoscaler-system")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		_, err := utils.Run(cmd)
		return err
	}, defaultTimeout, defaultInterval).Should(Succeed())
}

func waitForNodeScaleDown() {
	By("Waiting for node to be scaled down")

	// Wait for ToBeDeletedByClusterAutoscaler taint
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.taints}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get node taints: %v", err))
			return false
		}
		hasTaint := strings.Contains(output, "ToBeDeletedByClusterAutoscaler")
		if hasTaint {
			By("Node has ToBeDeletedByClusterAutoscaler taint")
		}
		return hasTaint
	}, 300*time.Second, 5*time.Second).Should(BeTrue(),
		"Node should get ToBeDeletedByClusterAutoscaler taint")

	// Wait for DeletionCandidateOfClusterAutoscaler taint
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.taints}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get node taints: %v", err))
			return false
		}
		hasTaint := strings.Contains(output, "DeletionCandidateOfClusterAutoscaler")
		if hasTaint {
			By("Node has DeletionCandidateOfClusterAutoscaler taint")
		}
		return hasTaint
	}, 300*time.Second, 5*time.Second).Should(BeTrue(),
		"Node should get DeletionCandidateOfClusterAutoscaler taint")

	// Wait for SchedulingDisabled (spec.unschedulable = true)
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.unschedulable}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get node unschedulable status: %v", err))
			return false
		}
		isUnschedulable := strings.TrimSpace(output) == "true"
		if isUnschedulable {
			By("Node is marked unschedulable (cordoned)")
		}
		return isUnschedulable
	}, 300*time.Second, 5*time.Second).Should(BeTrue(),
		"Node should be marked unschedulable (cordoned)")
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
			By(fmt.Sprintf("Warning: failed to get deployment status: %v", err))
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
			By(fmt.Sprintf("Warning: failed to get pod status: %v", err))
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
	By("Waiting for node shutdown sequence")

	// Step 1: Wait for Node CR progress to become shuttingdown
	By("Waiting for Node CR progress to become shuttingdown")
	Eventually(func() string {
		cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", nodeName,
			"-n", namespace, "-o", "jsonpath={.status.progress}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get Node CR progress: %v", err))
			return ""
		}
		progress := strings.TrimSpace(output)
		By(fmt.Sprintf("Node CR progress: %s", progress))
		return progress
	}, 120*time.Second, 5*time.Second).Should(Equal("shuttingdown"),
		"Node CR should have progress='shuttingdown'")

	// Step 2: Verify Kubernetes node is cordoned
	By("Verifying Kubernetes node is cordoned")
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.unschedulable}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get node unschedulable: %v", err))
			return false
		}
		isCordoned := strings.TrimSpace(output) == "true"
		if isCordoned {
			By("Kubernetes node is confirmed cordoned (unschedulable=true)")
		}
		return isCordoned
	}, 60*time.Second, 5*time.Second).Should(BeTrue(),
		"Kubernetes node should be cordoned")

	// Step 3: Verify shutdown job is created
	By("Verifying shutdown job is created")
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", "type=shutdown",
			"-n", namespace,
			"-o", "jsonpath={.items[*].metadata.name}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get shutdown jobs: %v", err))
			return false
		}
		hasJob := len(strings.TrimSpace(output)) > 0
		if hasJob {
			By(fmt.Sprintf("Shutdown job created: %s", output))
		}
		return hasJob
	}, 60*time.Second, 5*time.Second).Should(BeTrue(),
		"Shutdown job should be created")

	// Step 4: Wait for shutdown job to complete
	By("Waiting for shutdown job to complete")
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", "type=shutdown",
			"-n", namespace,
			"-o", "jsonpath={.items[*].status.succeeded}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get shutdown job status: %v", err))
			return false
		}
		completed := strings.Contains(output, "1")
		if completed {
			By("Shutdown job completed successfully")
		}
		return completed
	}, 180*time.Second, 5*time.Second).Should(BeTrue(),
		"Shutdown job should complete successfully")

	// Step 5: Wait for Node CR progress to become shutdown
	By("Waiting for Node CR progress to become shutdown")
	Eventually(func() string {
		cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", nodeName,
			"-n", namespace, "-o", "jsonpath={.status.progress}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get Node CR progress: %v", err))
			return ""
		}
		progress := strings.TrimSpace(output)
		By(fmt.Sprintf("Node CR progress: %s", progress))
		return progress
	}, 120*time.Second, 5*time.Second).Should(Equal("shutdown"),
		"Node CR should have progress='shutdown'")

	// Step 6: Verify Kubernetes node is NotReady (VM is powered off)
	By("Verifying Kubernetes node is NotReady (VM powered off)")
	Eventually(func() string {
		cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get node Ready status: %v", err))
			return ""
		}
		status := strings.TrimSpace(output)
		By(fmt.Sprintf("Kubernetes node Ready status: %s", status))
		return status
	}, 120*time.Second, 5*time.Second).Should(SatisfyAny(
		Equal("False"),
		Equal("Unknown")),
		"Kubernetes node should be NotReady or Unknown (VM is powered off)")
}

func setupTestConfigMapsAndSecrets() {
	// Create test ConfigMap for startup config
	startupConfigCM := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: startup-config
  namespace: homelab-autoscaler-system
data:
  startup-config.sh: |
    echo "Starting node..."
`
	tmpFile, err := os.CreateTemp("", "startup-cm-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(startupConfigCM)
	Expect(err).NotTo(HaveOccurred())
	tmpFile.Close()

	cmd := exec.Command("kubectl", "apply", "-f", tmpFile.Name())
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply startup ConfigMap: %s", output)

	// Create test ConfigMap for shutdown config
	shutdownConfigCM := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: shutdown-config
  namespace: homelab-autoscaler-system
data:
  shutdown-config.sh: |
    echo "Shutting down node..."
`
	tmpFile, err = os.CreateTemp("", "shutdown-cm-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(shutdownConfigCM)
	Expect(err).NotTo(HaveOccurred())
	tmpFile.Close()

	cmd = exec.Command("kubectl", "apply", "-f", tmpFile.Name())
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	output, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply shutdown ConfigMap: %s", output)

	// Create test Secret for authentication
	authSecret := `
apiVersion: v1
kind: Secret
metadata:
  name: auth-secrets
  namespace: homelab-autoscaler-system
type: Opaque
stringData:
  token: test-token-12345
`
	tmpFile, err = os.CreateTemp("", "auth-secret-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(authSecret)
	Expect(err).NotTo(HaveOccurred())
	tmpFile.Close()

	cmd = exec.Command("kubectl", "apply", "-f", tmpFile.Name())
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	output, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply auth Secret: %s", output)
}

func applyTestNodeManifestsWithVolumes() {
	By("Applying node CR with volumes and ServiceAccount from nodes1.yaml")

	cmd := exec.Command("kubectl", "apply", "-f", "./examples/k3d/nodes1.yaml")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	// Wait for node CR to be created
	Eventually(func() error {
		cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", nodeName, "-o", "name", "-n", "homelab-autoscaler-system")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		_, err := utils.Run(cmd)
		return err
	}, defaultTimeout, defaultInterval).Should(Succeed())
}

func startTestNode() {
	By("Patching node to trigger startup")

	// Patch the node to powerState: on if it's not already
	patch := `{"spec":{"powerState":"on"}}`
	cmd := exec.Command("kubectl", "patch", "nodes.infra.homecluster.dev", nodeName,
		"-n", "homelab-autoscaler-system", "--type=merge", "-p", patch)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

func verifyStartupJobServiceAccount() {
	By("Verifying startup job has correct ServiceAccount")

	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", "type=startup",
			"-n", "homelab-autoscaler-system",
			"-o", "jsonpath={.items[*].spec.template.spec.serviceAccountName}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get startup job ServiceAccount: %v", err))
			return false
		}
		hasServiceAccount := strings.Contains(output, "homelab-autoscaler")
		if hasServiceAccount {
			By(fmt.Sprintf("Startup job has ServiceAccount: %s", output))
		}
		return hasServiceAccount
	}, 60*time.Second, 5*time.Second).Should(BeTrue(),
		"Startup job should have ServiceAccount 'homelab-autoscaler'")
}

func verifyStartupJobConfigMapVolume() {
	By("Verifying startup job has ConfigMap volume mounted")

	Eventually(func() bool {
		// Check for ConfigMap volume
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", "type=startup",
			"-n", "homelab-autoscaler-system",
			"-o", "jsonpath={.items[*].spec.template.spec.volumes}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get startup job volumes: %v", err))
			return false
		}

		// Check for volume mount
		cmd = exec.Command("kubectl", "get", "jobs",
			"-l", "type=startup",
			"-n", "homelab-autoscaler-system",
			"-o", "jsonpath={.items[*].spec.template.spec.containers[*].volumeMounts}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		volumeMountOutput, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get startup job volume mounts: %v", err))
			return false
		}

		hasConfigMap := strings.Contains(output, "configMap") && strings.Contains(volumeMountOutput, "config")
		if hasConfigMap {
			By("Startup job has ConfigMap volume and mount")
		}
		return hasConfigMap
	}, 60*time.Second, 5*time.Second).Should(BeTrue(),
		"Startup job should have ConfigMap volume mounted")
}

func waitForStartupJobCompletion() {
	By("Waiting for startup job to complete")

	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", "type=startup",
			"-n", "homelab-autoscaler-system",
			"-o", "jsonpath={.items[*].status.succeeded}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get startup job status: %v", err))
			return false
		}
		completed := strings.Contains(output, "1")
		if completed {
			By("Startup job completed successfully")
		}
		return completed
	}, 120*time.Second, 5*time.Second).Should(BeTrue(),
		"Startup job should complete successfully")
}

func verifyShutdownJobSecretVolume() {
	By("Verifying shutdown job has Secret volume mounted")

	// Trigger shutdown first
	patch := `{"spec":{"powerState":"off"}}`
	cmd := exec.Command("kubectl", "patch", "nodes.infra.homecluster.dev", nodeName,
		"-n", "homelab-autoscaler-system", "--type=merge", "-p", patch)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	// Wait for shutdown job and verify Secret volume
	Eventually(func() bool {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", "type=shutdown",
			"-n", "homelab-autoscaler-system",
			"-o", "jsonpath={.items[*].spec.template.spec.volumes}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := utils.Run(cmd)
		if err != nil {
			By(fmt.Sprintf("Warning: failed to get shutdown job volumes: %v", err))
			return false
		}

		hasSecret := strings.Contains(output, "secret") && strings.Contains(output, "auth-secrets")
		if hasSecret {
			By("Shutdown job has Secret volume 'auth-secrets'")
		}
		return hasSecret
	}, 120*time.Second, 5*time.Second).Should(BeTrue(),
		"Shutdown job should have Secret volume 'auth-secrets' mounted")
}

func cleanupTestResources() {
	By("Cleaning up test ConfigMaps and Secrets")

	cmd := exec.Command("kubectl", "delete", "configmap", "startup-config", "shutdown-config",
		"-n", "homelab-autoscaler-system", "--ignore-not-found")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, _ = utils.Run(cmd)

	cmd = exec.Command("kubectl", "delete", "secret", "auth-secrets",
		"-n", "homelab-autoscaler-system", "--ignore-not-found")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	_, _ = utils.Run(cmd)
}
