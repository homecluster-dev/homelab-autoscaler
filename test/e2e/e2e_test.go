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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/homecluster-dev/homelab-autoscaler/test/utils"
)

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
	SetDefaultEventuallyPollingInterval(time.Second)

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

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=homelab-autoscaler-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		It("should create cronjobs for each NodeSpec in Group CRs", func() {
			By("verifying that cronjobs are created for each NodeSpec")
			verifyCronJobsCreated := func(g Gomega) {
				// Check if group1-worker-node-1-healthcheck cronjob exists (from group1.yaml)
				cmd := exec.Command("kubectl", "get", "cronjob", "group1-worker-node-1-healthcheck", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "group1-worker-node-1-healthcheck cronjob does not exist")

				// Check if group1-worker-node-2-healthcheck cronjob exists (from group1.yaml)
				cmd = exec.Command("kubectl", "get", "cronjob", "group1-worker-node-2-healthcheck", "-n", namespace)
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "group1-worker-node-2-healthcheck cronjob does not exist")

				// Check if group2-worker-node-3-healthcheck cronjob exists (from group2.yaml)
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-3-healthcheck", "-n", namespace)
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "group2-worker-node-3-healthcheck cronjob does not exist")

				// Check if group2-worker-node-4-healthcheck cronjob exists (from group2.yaml)
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-4-healthcheck", "-n", namespace)
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "group2-worker-node-4-healthcheck cronjob does not exist")
			}
			Eventually(verifyCronJobsCreated).Should(Succeed())

			By("verifying that cronjobs are in the correct namespace")
			verifyCronJobsNamespace := func(g Gomega) {
				// Get group1-worker-node-1-healthcheck cronjob details
				cmd := exec.Command("kubectl", "get", "cronjob", "group1-worker-node-1-healthcheck", "-o", "jsonpath={.metadata.namespace}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group1-worker-node-1-healthcheck namespace")
				g.Expect(output).To(Equal(namespace), "group1-worker-node-1-healthcheck is not in the correct namespace")

				// Get group1-worker-node-2-healthcheck cronjob details
				cmd = exec.Command("kubectl", "get", "cronjob", "group1-worker-node-2-healthcheck", "-o", "jsonpath={.metadata.namespace}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group1-worker-node-2-healthcheck namespace")
				g.Expect(output).To(Equal(namespace), "group1-worker-node-2-healthcheck is not in the correct namespace")

				// Get group2-worker-node-3-healthcheck cronjob details
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-3-healthcheck", "-o", "jsonpath={.metadata.namespace}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group2-worker-node-3-healthcheck namespace")
				g.Expect(output).To(Equal(namespace), "group2-worker-node-3-healthcheck is not in the correct namespace")

				// Get group2-worker-node-4-healthcheck cronjob details
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-4-healthcheck", "-o", "jsonpath={.metadata.namespace}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group2-worker-node-4-healthcheck namespace")
				g.Expect(output).To(Equal(namespace), "group2-worker-node-4-healthcheck is not in the correct namespace")
			}
			Eventually(verifyCronJobsNamespace).Should(Succeed())

			By("verifying that cronjobs have the correct schedule")
			verifyCronJobsSchedule := func(g Gomega) {
				// Get group1-worker-node-1-healthcheck cronjob schedule (should be "*/1 * * * *" based on healthcheckPeriod: 1)
				cmd := exec.Command("kubectl", "get", "cronjob", "group1-worker-node-1-healthcheck", "-o", "jsonpath={.spec.schedule}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group1-worker-node-1-healthcheck schedule")
				g.Expect(output).To(Equal("*/1 * * * *"), "group1-worker-node-1-healthcheck has incorrect schedule")

				// Get group1-worker-node-2-healthcheck cronjob schedule (should be "*/2 * * * *" based on healthcheckPeriod: 2)
				cmd = exec.Command("kubectl", "get", "cronjob", "group1-worker-node-2-healthcheck", "-o", "jsonpath={.spec.schedule}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group1-worker-node-2-healthcheck schedule")
				g.Expect(output).To(Equal("*/2 * * * *"), "group1-worker-node-2-healthcheck has incorrect schedule")

				// Get group2-worker-node-3-healthcheck cronjob schedule (should be "*/1 * * * *" based on healthcheckPeriod: 1)
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-3-healthcheck", "-o", "jsonpath={.spec.schedule}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group2-worker-node-3-healthcheck schedule")
				g.Expect(output).To(Equal("*/1 * * * *"), "group2-worker-node-3-healthcheck has incorrect schedule")

				// Get group2-worker-node-4-healthcheck cronjob schedule (should be "*/3 * * * *" based on healthcheckPeriod: 3)
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-4-healthcheck", "-o", "jsonpath={.spec.schedule}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group2-worker-node-4-healthcheck schedule")
				g.Expect(output).To(Equal("*/3 * * * *"), "group2-worker-node-4-healthcheck has incorrect schedule")
			}
			Eventually(verifyCronJobsSchedule).Should(Succeed())

			By("verifying that cronjobs have the correct container configuration")
			verifyCronJobsContainers := func(g Gomega) {
				// Get group1-worker-node-1-healthcheck cronjob containers array
				cmd := exec.Command("kubectl", "get", "cronjob", "group1-worker-node-1-healthcheck", "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group1-worker-node-1-healthcheck containers")
				g.Expect(output).To(ContainSubstring("busybox:latest"), "group1-worker-node-1-healthcheck has incorrect container image")
				g.Expect(output).To(ContainSubstring("/bin/sh"), "group1-worker-node-1-healthcheck has incorrect command")
				g.Expect(output).To(ContainSubstring("-c"), "group1-worker-node-1-healthcheck has incorrect args")
				g.Expect(output).To(ContainSubstring("Health check for group1 node1"), "group1-worker-node-1-healthcheck has incorrect args")

				// Get group1-worker-node-2-healthcheck cronjob containers array
				cmd = exec.Command("kubectl", "get", "cronjob", "group1-worker-node-2-healthcheck", "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group1-worker-node-2-healthcheck containers")
				g.Expect(output).To(ContainSubstring("busybox:latest"), "group1-worker-node-2-healthcheck has incorrect container image")
				g.Expect(output).To(ContainSubstring("/bin/sh"), "group1-worker-node-2-healthcheck has incorrect command")
				g.Expect(output).To(ContainSubstring("-c"), "group1-worker-node-2-healthcheck has incorrect args")
				g.Expect(output).To(ContainSubstring("Health check for group1 node2"), "group1-worker-node-2-healthcheck has incorrect args")

				// Get group2-worker-node-3-healthcheck cronjob containers array
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-3-healthcheck", "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group2-worker-node-3-healthcheck containers")
				g.Expect(output).To(ContainSubstring("alpine:latest"), "group2-worker-node-3-healthcheck has incorrect container image")
				g.Expect(output).To(ContainSubstring("/bin/sh"), "group2-worker-node-3-healthcheck has incorrect command")
				g.Expect(output).To(ContainSubstring("-c"), "group2-worker-node-3-healthcheck has incorrect args")
				g.Expect(output).To(ContainSubstring("Health check for group2 node3"), "group2-worker-node-3-healthcheck has incorrect args")

				// Get group2-worker-node-4-healthcheck cronjob containers array
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-4-healthcheck", "-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group2-worker-node-4-healthcheck containers")
				g.Expect(output).To(ContainSubstring("alpine:latest"), "group2-worker-node-4-healthcheck has incorrect container image")
				g.Expect(output).To(ContainSubstring("/bin/sh"), "group2-worker-node-4-healthcheck has incorrect command")
				g.Expect(output).To(ContainSubstring("-c"), "group2-worker-node-4-healthcheck has incorrect args")
				g.Expect(output).To(ContainSubstring("Health check for group2 node4"), "group2-worker-node-4-healthcheck has incorrect args")
			}
			Eventually(verifyCronJobsContainers).Should(Succeed())

			By("verifying that cronjobs are properly labeled to associate them with their respective Group CRs")
			verifyCronJobsLabels := func(g Gomega) {
				// Get group1-worker-node-1-healthcheck cronjob owner references array
				cmd := exec.Command("kubectl", "get", "cronjob", "group1-worker-node-1-healthcheck", "-o", "jsonpath={.metadata.ownerReferences}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group1-worker-node-1-healthcheck owner references")
				g.Expect(output).To(ContainSubstring("Group"), "group1-worker-node-1-healthcheck does not have Group as owner reference")
				g.Expect(output).To(ContainSubstring("group1"), "group1-worker-node-1-healthcheck does not have group1 as owner reference")

				// Get group1-worker-node-2-healthcheck cronjob owner references array
				cmd = exec.Command("kubectl", "get", "cronjob", "group1-worker-node-2-healthcheck", "-o", "jsonpath={.metadata.ownerReferences}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group1-worker-node-2-healthcheck owner references")
				g.Expect(output).To(ContainSubstring("Group"), "group1-worker-node-2-healthcheck does not have Group as owner reference")
				g.Expect(output).To(ContainSubstring("group1"), "group1-worker-node-2-healthcheck does not have group1 as owner reference")

				// Get group2-worker-node-3-healthcheck cronjob owner references array
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-3-healthcheck", "-o", "jsonpath={.metadata.ownerReferences}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group2-worker-node-3-healthcheck owner references")
				g.Expect(output).To(ContainSubstring("Group"), "group2-worker-node-3-healthcheck does not have Group as owner reference")
				g.Expect(output).To(ContainSubstring("group2"), "group2-worker-node-3-healthcheck does not have group2 as owner reference")

				// Get group2-worker-node-4-healthcheck cronjob owner references array
				cmd = exec.Command("kubectl", "get", "cronjob", "group2-worker-node-4-healthcheck", "-o", "jsonpath={.metadata.ownerReferences}", "-n", namespace)
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get group2-worker-node-4-healthcheck owner references")
				g.Expect(output).To(ContainSubstring("Group"), "group2-worker-node-4-healthcheck does not have Group as owner reference")
				g.Expect(output).To(ContainSubstring("group2"), "group2-worker-node-4-healthcheck does not have group2 as owner reference")
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

// deployGroupCRs deploys the Group CRs using kubectl apply command.
func deployGroupCRs() {
	By("waiting for Group CRD to be established")
	verifyCRDEstablished := func(g Gomega) {
		// Wait for the Group CRD to be established
		cmd := exec.Command("kubectl", "wait", "--for=condition=established",
			"crd/groups.infra.homecluster.dev",
			"--timeout=60s")
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Group CRD was not established within timeout")
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

	By("waiting for Group CRs to be properly applied")
	verifyGroupCRsApplied := func(g Gomega) {
		// Check if group1 CR is applied
		cmd := exec.Command("kubectl", "get", "group", "group1", "-n", namespace)
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Group1 CR is not applied")

		// Check if group2 CR is applied
		cmd = exec.Command("kubectl", "get", "group", "group2", "-n", namespace)
		_, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Group2 CR is not applied")
	}
	Eventually(verifyGroupCRsApplied).Should(Succeed())
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
