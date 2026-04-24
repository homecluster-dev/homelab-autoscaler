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

package utils

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}

// VMRequest represents a request to the VM control server
type VMRequest struct {
	Endpoint  string    `json:"endpoint"`
	VMName    string    `json:"vm_name"`
	Timestamp time.Time `json:"timestamp"`
}

// NodeTainting utilities

// TaintNode adds a taint to a Kubernetes node
func TaintNode(nodeName, taint string) error {
	cmd := exec.Command("kubectl", "taint", "nodes", nodeName, taint, "--overwrite")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to taint node %s: %w, output: %s", nodeName, err, string(output))
	}
	return nil
}

// UntaintNode removes a taint from a Kubernetes node
func UntaintNode(nodeName, taintKey string) error {
	taintKeyWithMinus := taintKey + "-"
	cmd := exec.Command("kubectl", "taint", "nodes", nodeName, taintKeyWithMinus)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to untaint node %s: %w, output: %s", nodeName, err, string(output))
	}
	return nil
}

// NodeExists checks if a node exists in the cluster
func NodeExists(nodeName string) bool {
	cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "name")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "node/"+nodeName)
}

// VerifyNodeTaint checks if a node has a specific taint
func VerifyNodeTaint(nodeName, taintKey string) bool {
	cmd := exec.Command("kubectl", "get", "node", nodeName, "-o", "jsonpath={.spec.taints}")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), taintKey)
}

// VM Control Server utilities

// VerifyVMControlServer checks if the VM control server is responding
func VerifyVMControlServer(host string, port int) bool {
	url := fmt.Sprintf("http://%s:%d/", host, port)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()

	// Server might return 404 for root path but that's okay - it's responding
	return resp.StatusCode < 500
}

// VerifyVMControlServerRequest checks if a specific request was received by the VM control server
// This reads from a request log file that the server should maintain
func VerifyVMControlServerRequest(logFile, endpoint, vmName string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logFile)
		if err == nil {
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				if strings.Contains(line, endpoint) && strings.Contains(line, vmName) {
					return true
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	return false
}

// GetVMControlServerRequests reads all requests from the VM control server log
func GetVMControlServerRequests(logFile string) ([]VMRequest, error) {
	data, err := os.ReadFile(logFile)
	if err != nil {
		return nil, err
	}

	var requests []VMRequest
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var req VMRequest
		if err := json.Unmarshal([]byte(line), &req); err == nil {
			requests = append(requests, req)
		}
	}

	return requests, nil
}

// gRPC Verification utilities

// GetClusterAutoscalerLogs retrieves logs from the cluster autoscaler
func GetClusterAutoscalerLogs(namespace string, tailLines int) (string, error) {
	cmd := exec.Command("kubectl", "logs",
		"-l", "app.kubernetes.io/component=cluster-autoscaler",
		"-n", namespace,
		fmt.Sprintf("--tail=%d", tailLines))
	cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// GetHomeClusterAutoscalerLogs retrieves logs from the homelab-autoscaler gRPC server log file
func GetHomeClusterAutoscalerLogs(namespace string, tailLines int) (string, error) {
	logFile := "/tmp/homelab-autoscaler.log"
	data, err := os.ReadFile(logFile)
	if err != nil {
		return "", err
	}
	
	lines := strings.Split(string(data), "\n")
	start := 0
	if len(lines) > tailLines {
		start = len(lines) - tailLines
	}
	
	return strings.Join(lines[start:], "\n"), nil
}

// VerifyGRPCCall checks if a specific gRPC method was called by searching homelab-autoscaler logs
func VerifyGRPCCall(namespace, method string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		logs, err := GetHomeClusterAutoscalerLogs(namespace, 500)
		if err == nil && strings.Contains(logs, method) {
			return true
		}
		time.Sleep(2 * time.Second)
	}

	return false
}

// Job Verification utilities

// VerifyJobHasServiceAccount checks if a job has the correct ServiceAccount
func VerifyJobHasServiceAccount(namespace, jobLabel, serviceAccount string) bool {
	cmd := exec.Command("kubectl", "get", "jobs",
		"-l", jobLabel,
		"-n", namespace,
		"-o", "jsonpath={.items[*].spec.template.spec.serviceAccountName}")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), serviceAccount)
}

// VerifyJobHasVolume checks if a job has a specific volume
func VerifyJobHasVolume(namespace, jobLabel, volumeName string) bool {
	cmd := exec.Command("kubectl", "get", "jobs",
		"-l", jobLabel,
		"-n", namespace,
		"-o", "jsonpath={.items[*].spec.template.spec.volumes}")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}

	return strings.Contains(string(output), volumeName)
}

// WaitForJobCompletion waits for a job to complete
func WaitForJobCompletion(namespace string, jobLabel string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		cmd := exec.Command("kubectl", "get", "jobs",
			"-l", jobLabel,
			"-n", namespace,
			"-o", "jsonpath={.items[*].status.succeeded}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
		output, err := cmd.CombinedOutput()
		if err == nil && strings.Contains(string(output), "1") {
			return true
		}
		time.Sleep(2 * time.Second)
	}

	return false
}

// Node Verification utilities

// WaitForNodeState waits for a Node CR to reach a specific progress state
func WaitForNodeState(namespace string, nodeName string, state string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		cmd := exec.Command("kubectl", "get", "nodes.infra.homecluster.dev", nodeName,
			"-n", namespace, "-o", "jsonpath={.status.progress}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
		output, err := cmd.CombinedOutput()
		if err == nil && strings.TrimSpace(string(output)) == state {
			return true
		}
		time.Sleep(2 * time.Second)
	}

	return false
}

// WaitForNodeReady waits for a Kubernetes node to be in a specific Ready state
func WaitForNodeReady(nodeName string, ready bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	expected := "True"
	if !ready {
		expected = "False"
	}

	for time.Now().Before(deadline) {
		cmd := exec.Command("kubectl", "get", "node", nodeName,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
		output, err := cmd.CombinedOutput()
		if err == nil && strings.TrimSpace(string(output)) == expected {
			return true
		}
		time.Sleep(2 * time.Second)
	}

	return false
}

// WaitForK3dNodeState waits for a k3d node to be in a specific running state
func WaitForK3dNodeState(vmName string, running bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		cmd := exec.Command("k3d", "node", "list", vmName)
		output, err := cmd.CombinedOutput()
		if err != nil {
			continue
		}

		outputStr := string(output)
		isRunning := strings.Contains(strings.ToLower(outputStr), "running") ||
			strings.Contains(strings.ToLower(outputStr), "up")

		if running && isRunning {
			return true
		}
		if !running && !isRunning {
			return true
		}
		time.Sleep(2 * time.Second)
	}

	return false
}

// WaitForDeploymentRunning waits for a deployment to have ready replicas
func WaitForDeploymentRunning(namespace string, deploymentName string, readyReplicas int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		cmd := exec.Command("kubectl", "get", "deployment", deploymentName,
			"-n", namespace, "-o", "jsonpath={.status.readyReplicas}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
		output, err := cmd.CombinedOutput()
		if err == nil {
			ready := strings.TrimSpace(string(output))
			if readyReplicas == 0 {
				return ready == "" || ready == "0"
			}
			readyInt := 0
			fmt.Sscanf(ready, "%d", &readyInt)
			if readyInt >= readyReplicas {
				return true
			}
		}
		time.Sleep(2 * time.Second)
	}

	return false
}

// WaitForPodScheduled waits for a pod to be scheduled and running
func WaitForPodScheduled(namespace string, labelSelector string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		cmd := exec.Command("kubectl", "get", "pods",
			"-l", labelSelector,
			"-n", namespace,
			"-o", "jsonpath={.items[*].status.phase}")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+os.Getenv("KUBECONFIG"))
		output, err := cmd.CombinedOutput()
		if err == nil && strings.Contains(string(output), "Running") {
			return true
		}
		time.Sleep(2 * time.Second)
	}

	return false
}

// GetK3dGatewayIP returns the gateway IP for the k3d network
func GetK3dGatewayIP(clusterName string) (string, error) {
	cmd := exec.Command("docker", "network", "inspect", "k3d-"+clusterName,
		"-f", "{{(index .IPAM.Config 0).Gateway}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// File utilities

// WriteToLogFile appends a line to a log file
func WriteToLogFile(logFile, message string) error {
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(message + "\n")
	return err
}

// ReadLogFile reads the entire contents of a log file
func ReadLogFile(logFile string) (string, error) {
	data, err := os.ReadFile(logFile)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ClearLogFile clears a log file
func ClearLogFile(logFile string) error {
	return os.WriteFile(logFile, []byte{}, 0644)
}
