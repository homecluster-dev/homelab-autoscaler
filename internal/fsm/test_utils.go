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

package fsm

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// MockCoordinationManager implements CoordinationManager for testing
type MockCoordinationManager struct {
	locks        map[string]*OperationLock
	acquireError error
	releaseError error
	checkError   error
	forceError   error
	acquireCalls []AcquireCall
	releaseCalls []ReleaseCall
	checkCalls   []CheckCall
	forceCalls   []ForceCall
}

type AcquireCall struct {
	Node      *infrav1alpha1.Node
	Operation string
	Timeout   time.Duration
}

type ReleaseCall struct {
	Node *infrav1alpha1.Node
}

type CheckCall struct {
	Node *infrav1alpha1.Node
}

type ForceCall struct {
	Node *infrav1alpha1.Node
}

// NewMockCoordinationManager creates a new mock coordination manager
func NewMockCoordinationManager() *MockCoordinationManager {
	return &MockCoordinationManager{
		locks:        make(map[string]*OperationLock),
		acquireCalls: make([]AcquireCall, 0),
		releaseCalls: make([]ReleaseCall, 0),
		checkCalls:   make([]CheckCall, 0),
		forceCalls:   make([]ForceCall, 0),
	}
}

func (m *MockCoordinationManager) AcquireLock(ctx context.Context, node *infrav1alpha1.Node, operation string, timeout time.Duration) error {
	m.acquireCalls = append(m.acquireCalls, AcquireCall{
		Node:      node,
		Operation: operation,
		Timeout:   timeout,
	})

	if m.acquireError != nil {
		return m.acquireError
	}

	// Simulate successful lock acquisition
	m.locks[node.Name] = &OperationLock{
		Operation: operation,
		Owner:     NodeControllerOwner,
		Timestamp: time.Now(),
		Timeout:   timeout,
	}

	return nil
}

func (m *MockCoordinationManager) ReleaseLock(ctx context.Context, node *infrav1alpha1.Node) error {
	m.releaseCalls = append(m.releaseCalls, ReleaseCall{Node: node})

	if m.releaseError != nil {
		return m.releaseError
	}

	// Simulate successful lock release
	delete(m.locks, node.Name)
	return nil
}

func (m *MockCoordinationManager) CheckLock(ctx context.Context, node *infrav1alpha1.Node) (*OperationLock, bool) {
	m.checkCalls = append(m.checkCalls, CheckCall{Node: node})

	if m.checkError != nil {
		return nil, false
	}

	lock, exists := m.locks[node.Name]
	return lock, exists
}

func (m *MockCoordinationManager) IsLockExpired(lock *OperationLock) bool {
	if lock == nil {
		return true
	}
	return time.Since(lock.Timestamp) > lock.Timeout
}

func (m *MockCoordinationManager) ForceReleaseLock(ctx context.Context, node *infrav1alpha1.Node) error {
	m.forceCalls = append(m.forceCalls, ForceCall{Node: node})

	if m.forceError != nil {
		return m.forceError
	}

	// Simulate successful force release
	delete(m.locks, node.Name)
	return nil
}

// Test helper methods
func (m *MockCoordinationManager) SetAcquireError(err error) {
	m.acquireError = err
}

func (m *MockCoordinationManager) SetReleaseError(err error) {
	m.releaseError = err
}

func (m *MockCoordinationManager) SetCheckError(err error) {
	m.checkError = err
}

func (m *MockCoordinationManager) SetForceError(err error) {
	m.forceError = err
}

func (m *MockCoordinationManager) GetAcquireCalls() []AcquireCall {
	return m.acquireCalls
}

func (m *MockCoordinationManager) GetReleaseCalls() []ReleaseCall {
	return m.releaseCalls
}

func (m *MockCoordinationManager) GetCheckCalls() []CheckCall {
	return m.checkCalls
}

func (m *MockCoordinationManager) GetForceCalls() []ForceCall {
	return m.forceCalls
}

func (m *MockCoordinationManager) HasLock(nodeName string) bool {
	_, exists := m.locks[nodeName]
	return exists
}

func (m *MockCoordinationManager) GetLock(nodeName string) *OperationLock {
	return m.locks[nodeName]
}

func (m *MockCoordinationManager) Reset() {
	m.locks = make(map[string]*OperationLock)
	m.acquireError = nil
	m.releaseError = nil
	m.checkError = nil
	m.forceError = nil
	m.acquireCalls = make([]AcquireCall, 0)
	m.releaseCalls = make([]ReleaseCall, 0)
	m.checkCalls = make([]CheckCall, 0)
	m.forceCalls = make([]ForceCall, 0)
}

// CreateTestNode creates a test node with the given parameters
func CreateTestNode(name, namespace string, powerState infrav1alpha1.PowerState, progress infrav1alpha1.Progress) *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"group": "test-group",
			},
		},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  powerState,
			KubernetesNodeName: name + "-k8s",
			StartupPodSpec: infrav1alpha1.MinimalPodSpec{
				Image:   "test/startup:latest",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "echo 'starting node'"},
			},
			ShutdownPodSpec: infrav1alpha1.MinimalPodSpec{
				Image:   "test/shutdown:latest",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "echo 'shutting down node'"},
			},
		},
		Status: infrav1alpha1.NodeStatus{
			PowerState: powerState,
			Progress:   progress,
		},
	}
}

// CreateTestJob creates a test job for testing purposes
func CreateTestJob(name, namespace, jobType string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":  "homelab-autoscaler",
				"type": jobType,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    jobType,
							Image:   "test/" + jobType + ":latest",
							Command: []string{"/bin/sh"},
							Args:    []string{"-c", "echo 'test job'"},
						},
					},
				},
			},
		},
	}
}

// CreateTestJobWithNodeLabel creates a test job with node label for cleanup testing
func CreateTestJobWithNodeLabel(name, namespace, jobType, nodeName string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":   "homelab-autoscaler",
				"type":  jobType,
				"node":  nodeName,
				"group": "test-group",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "homelab-autoscaler",
						"type":  jobType,
						"node":  nodeName,
						"group": "test-group",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    jobType,
							Image:   "test/" + jobType + ":latest",
							Command: []string{"/bin/sh"},
							Args:    []string{"-c", "echo 'test job'"},
						},
					},
				},
			},
		},
	}
}

// CreateFakeClient creates a fake Kubernetes client for testing
func CreateFakeClient(scheme *runtime.Scheme, objects ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1alpha1.Node{}).
		Build()
}

// WaitForCondition waits for a condition to be true with timeout
func WaitForCondition(condition func() bool, timeout time.Duration, interval time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

// AssertEventuallyTrue asserts that a condition becomes true within a timeout
func AssertEventuallyTrue(condition func() bool, timeout time.Duration, message string) bool {
	return WaitForCondition(condition, timeout, 100*time.Millisecond)
}

// CreateTestKubernetesNode creates a test Kubernetes node for testing scheduling operations
func CreateTestKubernetesNode(name string, unschedulable bool) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/hostname": name,
			},
		},
		Spec: corev1.NodeSpec{
			Unschedulable: unschedulable,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}
