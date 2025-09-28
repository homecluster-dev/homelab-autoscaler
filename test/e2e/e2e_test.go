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
	"crypto/sha256"
	"encoding/json"
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

// generateExpectedCronJobName generates the expected CronJob name using the same logic as the controller
func generateExpectedCronJobName(groupName, nodeName string) string {
	// This matches the logic in internal/controller/node_controller.go generateShortCronJobName function
	// Maximum length for Kubernetes resource names is 52 characters
	// We need to reserve space for: {prefix}-{hash}-healthcheck
	// Let's use: {truncatedGroup}-{truncatedNode}-{hash}-healthcheck
	// Reserve 12 chars for "-healthcheck", 9 chars for "-{hash}", and some buffer

	maxNameLength := 52 - 12 - 9 - 2 // 29 chars total for group + node names

	// Create a unique identifier by combining group and node names
	combined := fmt.Sprintf("%s-%s", groupName, nodeName)

	// Generate a short hash (8 characters) of the combined string
	hash := sha256.Sum256([]byte(combined))
	hashStr := fmt.Sprintf("%x", hash)[:8]

	// Truncate group and node names if they're too long
	truncatedGroup := groupName
	truncatedNode := nodeName

	// If the combined length is too long, truncate proportionally
	if len(groupName)+len(nodeName) > maxNameLength {
		// Split the available space proportionally
		totalLen := len(groupName) + len(nodeName)
		groupRatio := float64(len(groupName)) / float64(totalLen)
		nodeRatio := float64(len(nodeName)) / float64(totalLen)

		groupMaxLen := int(float64(maxNameLength) * groupRatio)
		nodeMaxLen := int(float64(maxNameLength) * nodeRatio)

		// Ensure we don't truncate to 0
		if groupMaxLen < 1 {
			groupMaxLen = 1
			nodeMaxLen = maxNameLength - 1
		}
		if nodeMaxLen < 1 {
			nodeMaxLen = 1
			groupMaxLen = maxNameLength - 1
		}

		if len(groupName) > groupMaxLen {
			truncatedGroup = groupName[:groupMaxLen]
		}
		if len(nodeName) > nodeMaxLen {
			truncatedNode = nodeName[:nodeMaxLen]
		}
	}

	return fmt.Sprintf("%s-%s-%s-healthcheck", truncatedGroup, truncatedNode, hashStr)
}

// namespace where the project is deployed in
const namespace = "homelab-autoscaler-system"

// serviceAccountName created for the project
const serviceAccountName = "homelab-autoscaler-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "homelab-autoscaler-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "homelab-autoscaler-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			// Get the controller pod name dynamically if not already set
			currentControllerPodName := controllerPodName
			if currentControllerPodName == "" {
				By("Fetching controller manager pod name")
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)
				podOutput, err := utils.Run(cmd)
				if err == nil {
					podNames := utils.GetNonEmptyLines(podOutput)
					if len(podNames) > 0 {
						currentControllerPodName = podNames[0]
					}
				}
			}

			if currentControllerPodName != "" {
				By("Fetching controller manager pod logs")
				cmd := exec.Command("kubectl", "logs", currentControllerPodName, "-n", namespace)
				controllerLogs, err := utils.Run(cmd)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
				}

				By("Fetching controller manager pod description")
				cmd = exec.Command("kubectl", "describe", "pod", currentControllerPodName, "-n", namespace)
				podDescription, err := utils.Run(cmd)
				if err == nil {
					fmt.Println("Pod description:\n", podDescription)
				} else {
					fmt.Println("Failed to describe controller pod")
				}
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Could not determine controller pod name for log collection\n")
			}

			By("Fetching Kubernetes events")
			cmd := exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(5 * time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should create Node CRs and their associated cronjobs", func() {
			By("verifying that Node CRs are created")
			verifyNodeCRsCreated := func(g Gomega) {
				// Check if group1-worker-node-1 CR exists (from nodes1.yaml)
				cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", "group1-worker-node-1", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "group1-worker-node-1 CR does not exist")

				// Check if group1-worker-node-2 CR exists (from nodes1.yaml)
				cmd = exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", "group1-worker-node-2", "-n", namespace)
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "group1-worker-node-2 CR does not exist")

				// Check if group2-worker-node-3 CR exists (from nodes2.yaml)
				cmd = exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", "group2-worker-node-3", "-n", namespace)
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "group2-worker-node-3 CR does not exist")

				// Check if group2-worker-node-4 CR exists (from nodes2.yaml)
				cmd = exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", "group2-worker-node-4", "-n", namespace)
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "group2-worker-node-4 CR does not exist")
			}
			Eventually(verifyNodeCRsCreated).Should(Succeed())

			By("verifying that cronjobs are created for each Node CR")
			verifyCronJobsCreated := func(g Gomega) {
				// Get all cronjobs in the namespace
				cmd := exec.Command("kubectl", "get", "cronjob", "-n", namespace, "-o", "jsonpath={.items[*].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get cronjobs list")

				// Parse the cronjob names from the output (space-separated)
				var cronjobNames []string
				if output != "" {
					cronjobNames = strings.Fields(output)
				}

				// Generate expected cronjob names
				expectedCronJobs := []string{
					generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker"),
					generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker2"),
					generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker3"),
					generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker4"),
				}

				// Verify all expected cronjobs are present
				for _, expectedCronJob := range expectedCronJobs {
					g.Expect(cronjobNames).To(ContainElement(expectedCronJob), fmt.Sprintf("Expected cronjob %s not found in namespace", expectedCronJob))
				}
			}
			Eventually(verifyCronJobsCreated).Should(Succeed())

			By("verifying that cronjobs are in the correct namespace")
			verifyCronJobsNamespace := func(g Gomega) {
				// Get group1-worker-node-1-healthcheck cronjob details
				cronjobName1 := generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker")
				cmd := exec.Command("kubectl", "get", "cronjob", cronjobName1, "-o", "jsonpath={.metadata.namespace}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s namespace", cronjobName1))
				g.Expect(output).To(Equal(namespace), fmt.Sprintf("%s is not in the correct namespace", cronjobName1))

				// Get group1-worker-node-2-healthcheck cronjob details
				cronjobName2 := generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker2")
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName2, "-o", "jsonpath={.metadata.namespace}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s namespace", cronjobName2))
				g.Expect(output).To(Equal(namespace), fmt.Sprintf("%s is not in the correct namespace", cronjobName2))

				// Get group2-worker-node-3-healthcheck cronjob details
				cronjobName3 := generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker3")
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName3, "-o", "jsonpath={.metadata.namespace}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s namespace", cronjobName3))
				g.Expect(output).To(Equal(namespace), fmt.Sprintf("%s is not in the correct namespace", cronjobName3))

				// Get group2-worker-node-4-healthcheck cronjob details
				cronjobName4 := generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker4")
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName4, "-o", "jsonpath={.metadata.namespace}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s namespace", cronjobName4))
				g.Expect(output).To(Equal(namespace), fmt.Sprintf("%s is not in the correct namespace", cronjobName4))
			}
			Eventually(verifyCronJobsNamespace).Should(Succeed())

			By("verifying that cronjobs have the correct schedule")
			verifyCronJobsSchedule := func(g Gomega) {
				// Get group1-worker-node-1-healthcheck cronjob schedule (should be "*/1 * * * *" based on healthcheckPeriod: 1)
				cronjobName1 := generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker")
				cmd := exec.Command("kubectl", "get", "cronjob", cronjobName1, "-o", "jsonpath={.spec.schedule}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s schedule", cronjobName1))
				g.Expect(output).To(Equal("*/1 * * * *"), fmt.Sprintf("%s has incorrect schedule", cronjobName1))

				// Get group1-worker-node-2-healthcheck cronjob schedule (should be "*/1 * * * *" based on healthcheckPeriod: 1)
				cronjobName2 := generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker2")
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName2, "-o", "jsonpath={.spec.schedule}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s schedule", cronjobName2))
				g.Expect(output).To(Equal("*/1 * * * *"), fmt.Sprintf("%s has incorrect schedule", cronjobName2))

				// Get group2-worker-node-3-healthcheck cronjob schedule (should be "*/2 * * * *" based on healthcheckPeriod: 2)
				cronjobName3 := generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker3")
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName3, "-o", "jsonpath={.spec.schedule}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s schedule", cronjobName3))
				g.Expect(output).To(Equal("*/2 * * * *"), fmt.Sprintf("%s has incorrect schedule", cronjobName3))

				// Get group2-worker-node-4-healthcheck cronjob schedule (should be "*/2 * * * *" based on healthcheckPeriod: 2)
				cronjobName4 := generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker4")
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName4, "-o", "jsonpath={.spec.schedule}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s schedule", cronjobName4))
				g.Expect(output).To(Equal("*/2 * * * *"), fmt.Sprintf("%s has incorrect schedule", cronjobName4))
			}
			Eventually(verifyCronJobsSchedule).Should(Succeed())

			By("verifying that cronjobs have the correct container configuration")
			verifyCronJobsContainers := func(g Gomega) {
				// Get group1-worker-node-1-healthcheck cronjob container details
				cronjobName1 := generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker")
				
				// Verify container image
				cmd := exec.Command("kubectl", "get", "cronjob", cronjobName1, "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers[0].image}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s container image", cronjobName1))
				g.Expect(output).To(Equal("alpine/k8s:1.34.1"), fmt.Sprintf("%s has incorrect container image", cronjobName1))

				// Verify service account
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName1, "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.serviceAccountName}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s service account", cronjobName1))
				g.Expect(output).To(Equal("kubectl-sa"), fmt.Sprintf("%s has incorrect service account", cronjobName1))

				// Verify group1-worker-node-2-healthcheck cronjob container details
				cronjobName2 := generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker2")
				
				// Verify container image
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName2, "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers[0].image}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s container image", cronjobName2))
				g.Expect(output).To(Equal("alpine/k8s:1.34.1"), fmt.Sprintf("%s has incorrect container image", cronjobName2))

				// Verify service account
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName2, "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.serviceAccountName}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s service account", cronjobName2))
				g.Expect(output).To(Equal("kubectl-sa"), fmt.Sprintf("%s has incorrect service account", cronjobName2))

				// Verify group2-worker-node-3-healthcheck cronjob container details
				cronjobName3 := generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker3")
				
				// Verify container image
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName3, "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers[0].image}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s container image", cronjobName3))
				g.Expect(output).To(Equal("alpine/k8s:1.34.1"), fmt.Sprintf("%s has incorrect container image", cronjobName3))

				// Verify service account
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName3, "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.serviceAccountName}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s service account", cronjobName3))
				g.Expect(output).To(Equal("kubectl-sa"), fmt.Sprintf("%s has incorrect service account", cronjobName3))

				// Verify group2-worker-node-4-healthcheck cronjob container details
				cronjobName4 := generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker4")
				
				// Verify container image
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName4, "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers[0].image}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s container image", cronjobName4))
				g.Expect(output).To(Equal("alpine/k8s:1.34.1"), fmt.Sprintf("%s has incorrect container image", cronjobName4))

				// Verify service account
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName4, "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.serviceAccountName}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s service account", cronjobName4))
				g.Expect(output).To(Equal("kubectl-sa"), fmt.Sprintf("%s has incorrect service account", cronjobName4))

			}
			Eventually(verifyCronJobsContainers).Should(Succeed())

			By("verifying that cronjobs are properly labeled to associate them with their respective Node CRs")
			verifyCronJobsLabels := func(g Gomega) {
				// Get group1-worker-node-1-healthcheck cronjob owner references array
				cronjobName1 := generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker")
				cmd := exec.Command("kubectl", "get", "cronjob", cronjobName1, "-o", "jsonpath={.metadata.ownerReferences}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s owner references", cronjobName1))
				g.Expect(output).To(ContainSubstring("Node"), fmt.Sprintf("%s does not have Node as owner reference", cronjobName1))
				g.Expect(output).To(ContainSubstring("group1-worker-node-1"), fmt.Sprintf("%s does not have group1-worker-node-1 as owner reference", cronjobName1))

				// Get group1-worker-node-2-healthcheck cronjob owner references array
				cronjobName2 := generateExpectedCronJobName("group1", "homelab-autoscaler-test-e2e-worker2")
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName2, "-o", "jsonpath={.metadata.ownerReferences}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s owner references", cronjobName2))
				g.Expect(output).To(ContainSubstring("Node"), fmt.Sprintf("%s does not have Node as owner reference", cronjobName2))
				g.Expect(output).To(ContainSubstring("group1-worker-node-2"), fmt.Sprintf("%s does not have group1-worker-node-2 as owner reference", cronjobName2))

				// Get group2-worker-node-3-healthcheck cronjob owner references array
				cronjobName3 := generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker3")
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName3, "-o", "jsonpath={.metadata.ownerReferences}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s owner references", cronjobName3))
				g.Expect(output).To(ContainSubstring("Node"), fmt.Sprintf("%s does not have Node as owner reference", cronjobName3))
				g.Expect(output).To(ContainSubstring("group2-worker-node-3"), fmt.Sprintf("%s does not have group2-worker-node-3 as owner reference", cronjobName3))

				// Get group2-worker-node-4-healthcheck cronjob owner references array
				cronjobName4 := generateExpectedCronJobName("group2", "homelab-autoscaler-test-e2e-worker4")
				cmd = exec.Command("kubectl", "get", "cronjob", cronjobName4, "-o", "jsonpath={.metadata.ownerReferences}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get %s owner references", cronjobName4))
				g.Expect(output).To(ContainSubstring("Node"), fmt.Sprintf("%s does not have Node as owner reference", cronjobName4))
				g.Expect(output).To(ContainSubstring("group2-worker-node-4"), fmt.Sprintf("%s does not have group2-worker-node-4 as owner reference", cronjobName4))
			}
			Eventually(verifyCronJobsLabels).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})
})

// deployGroupCRs deploys the Group and Node CRs using kubectl apply command.
func deployGroupCRs() {
	By("waiting for Group and Node CRDs to be established")
	verifyCRDEstablished := func(g Gomega) {
		// Wait for the Group CRD to be established
		cmd := exec.Command("kubectl", "wait", "--for=condition=established",
			"crd/groups.infra.homecluster.dev",
			"--timeout=60s")
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Group CRD was not established within timeout")

		// Wait for the Node CRD to be established
		cmd = exec.Command("kubectl", "wait", "--for=condition=established",
			"crd/nodes.infra.homecluster.dev",
			"--timeout=60s")
		_, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Node CRD was not established within timeout")
	}
	Eventually(verifyCRDEstablished).Should(Succeed())

	By("deploying group1 CR")
	cmd := exec.Command("kubectl", "apply", "-f", "test/e2e/manifests/group1.yaml", "-n", namespace)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy group1 CR")

	By("deploying group2 CR")
	cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/manifests/group2.yaml", "-n", namespace)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy group2 CR")

	By("deploying nodes1 CRs")
	cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/manifests/nodes1.yaml", "-n", namespace)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy nodes1 CRs")

	By("deploying nodes2 CRs")
	cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/manifests/nodes2.yaml", "-n", namespace)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy nodes2 CRs")

	By("deploying unhealthy group Node CR")
	cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/manifests/unhealthy.yaml", "-n", namespace)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy unhealthy group Node CR")

	By("waiting for Group and Node CRs to be properly applied")
	verifyCRsApplied := func(g Gomega) {
		// Check if group1 CR is applied
		cmd := exec.Command("kubectl", "get", "group", "group1", "-n", namespace)
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Group1 CR is not applied")

		// Check if group2 CR is applied
		cmd = exec.Command("kubectl", "get", "group", "group2", "-n", namespace)
		_, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Group2 CR is not applied")

		// Check if Node CRs are applied
		cmd = exec.Command("kubectl", "get", "node", "group1-worker-node-1", "-n", namespace)
		_, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "group1-worker-node-1 CR is not applied")

		cmd = exec.Command("kubectl", "get", "node", "group1-worker-node-2", "-n", namespace)
		_, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "group1-worker-node-2 CR is not applied")

		cmd = exec.Command("kubectl", "get", "node", "group2-worker-node-3", "-n", namespace)
		_, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "group2-worker-node-3 CR is not applied")

		cmd = exec.Command("kubectl", "get", "node", "group2-worker-node-4", "-n", namespace)
		_, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "group2-worker-node-4 CR is not applied")

		// Check if unhealthy group Node CR is applied
		cmd = exec.Command("kubectl", "get", "node", "unhealthy-group-unhealthy-node", "-n", namespace)
		_, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "unhealthy-group-unhealthy-node CR is not applied")
	}
	Eventually(verifyCRsApplied).Should(Succeed())
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
