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

	"github.com/looplab/fsm"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// FSM States - map directly to Progress enum values
const (
	StateShutdown     = "shutdown"
	StateStartingUp   = "startingup"
	StateReady        = "ready"
	StateShuttingDown = "shuttingdown"
)

// FSM Events
const (
	EventStartNode    = "StartNode"
	EventShutdownNode = "ShutdownNode"
	EventJobCompleted = "JobCompleted"
	EventJobFailed    = "JobFailed"
	EventJobTimeout   = "JobTimeout"
	EventForceCleanup = "ForceCleanup"
)

// Operation types for coordination locks
const (
	OperationScaleUp   = "scale-up"
	OperationScaleDown = "scale-down"
)

// Default timeouts and backoff settings
const (
	DefaultJobTimeout  = 5 * time.Minute
	DefaultLockTimeout = 5 * time.Minute
	MaxRetries         = 3
)

// NodeStateMachine represents the FSM for node state management
type NodeStateMachine struct {
	// FSM engine
	fsm *fsm.FSM

	// Kubernetes resources
	node   *infrav1alpha1.Node
	client client.Client
	scheme *runtime.Scheme

	// Coordination management
	coordinationManager CoordinationManager

	// Job tracking
	currentJob   string
	jobTimeout   time.Duration
	jobStartTime time.Time

	// Backoff strategy
	backoff *BackoffStrategy
}

// BackoffStrategy implements smart backoff for state transitions
type BackoffStrategy struct {
	transitionStart time.Time
	retryCount      int
	maxRetries      int
}

// FSMCallbacks defines the interface for FSM callbacks
type FSMCallbacks struct {
	// Job management
	CreateStartupJob  func(*infrav1alpha1.Node) (*batchv1.Job, error)
	CreateShutdownJob func(*infrav1alpha1.Node) (*batchv1.Job, error)

	// Status updates
	UpdateNodeStatus func(*infrav1alpha1.Node, string, metav1.Condition) error

	// Coordination
	AcquireLock func(string, time.Duration) error
	ReleaseLock func() error

	// Kubernetes operations
	UpdateKubernetesNode func(*corev1.Node) error
}

// CoordinationManager interface for managing coordination locks
type CoordinationManager interface {
	AcquireLock(ctx context.Context, node *infrav1alpha1.Node, operation string, timeout time.Duration) error
	ReleaseLock(ctx context.Context, node *infrav1alpha1.Node) error
	CheckLock(ctx context.Context, node *infrav1alpha1.Node) (*OperationLock, bool)
	IsLockExpired(lock *OperationLock) bool
	ForceReleaseLock(ctx context.Context, node *infrav1alpha1.Node) error
}

// OperationLock represents a coordination lock on a node
type OperationLock struct {
	Operation string
	Owner     string
	Timestamp time.Time
	Timeout   time.Duration
}

// NodeStateMachineInterface defines the interface for the FSM
type NodeStateMachineInterface interface {
	// State management
	GetCurrentState() string
	CanTransition(event string) bool

	// Event triggering
	StartNode() error
	ShutdownNode() error
	JobCompleted() error
	JobFailed() error
	JobTimeout() error
	ForceCleanup() error

	// Backoff calculation
	CalculateBackoff() time.Duration

	// State validation
	IsTransitionStuck() bool
	CheckStuckState()

	// Job monitoring
	MonitorJobCompletion(jobName string)
}
