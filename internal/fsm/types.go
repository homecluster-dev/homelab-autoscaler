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
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// FSM States. The names are the identifiers of the internal state machine and live in
// internal/phase, which owns the mapping from (live claim, observed power, node usability) to a
// state (phase.State). They used to be the values of the Node's status.progress enum, because the
// state machine read its own state out of that field; v2alpha1 has no such field, and the input of
// a pass is derived, not recorded (docs/design/nodeclaim.md §6).
const (
	StateShutdown     = phase.StateShutdown
	StateStartingUp   = phase.StateStartingUp
	StateReady        = phase.StateReady
	StateShuttingDown = phase.StateShuttingDown
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

// Lifecycle phases, used as the value of the "type" label and of
// LabelPhase on the startup/shutdown Jobs the controller creates.
const (
	PhaseStartup  = "startup"
	PhaseShutdown = "shutdown"
)

// Label and annotation keys carried by every startup/shutdown Job the
// controller creates, and by its pod template (labels only), so that
// kubectl, Argo CD and anyone reading the cluster can tell which Node CR
// a Job belongs to. See (NodeStateMachine).lifecycleLabels for the rules
// about which keys may be added.
const (
	// Legacy keys. cleanupPendingJobs() selects on LabelNodeLegacy and filters on
	// LabelType, the gRPC server groups resources by LabelGroup, and tests assert on
	// all four - keep the values exactly as they are.
	LabelApp   = "app"
	LabelGroup = "group"
	// LabelNodeLegacy holds spec.kubernetesNodeName, which can differ from the
	// Node CR name - see LabelNodeCR for the CR name.
	LabelNodeLegacy = "node"
	LabelType       = "type"

	// Standard Kubernetes recommended labels.
	LabelK8sName      = "app.kubernetes.io/name"
	LabelK8sComponent = "app.kubernetes.io/component"
	LabelK8sManagedBy = "app.kubernetes.io/managed-by"

	// Explicit provenance: the Node CR that caused this Job.
	LabelNodeCR = "infra.homecluster.dev/node"
	LabelPhase  = "infra.homecluster.dev/phase"

	// Annotations repeat the provenance so it stays legible in `kubectl describe`
	// and survives a dropped ownerReference.
	AnnotationNodeUID   = "infra.homecluster.dev/node-uid"
	AnnotationNodeGroup = "infra.homecluster.dev/node-group"
	AnnotationCreatedBy = "infra.homecluster.dev/created-by"
)

// Label and annotation values used for the lifecycle Jobs.
const (
	LifecycleAppName       = "homelab-autoscaler"
	LifecycleComponentName = "node-lifecycle"
)

// Default timeouts and backoff settings
const (
	DefaultJobTimeout      = 5 * time.Minute
	DefaultLockTimeout     = 5 * time.Minute
	MaxRetries             = 3
	DefaultUncordonTimeout = 90 * time.Second

	// DefaultReadinessGateTimeout bounds the readiness gate of the scale-up: the wait for the node
	// object to be Ready and free of the CNI's bootstrap taint, counted from the moment the node
	// object first appears (see setNodeSchedulableWithRetry). It is deliberately not the
	// registration wait, which stays DefaultUncordonTimeout: a cold boot spends most of 90s
	// registering the kubelet, and a gate sharing that budget times out on the machine it exists to
	// protect - the slot is declared ready with the bootstrap taint still on the node, the pool's
	// pods still cannot land, and the autoscaler buys a second machine. That is the symptom the
	// gate was added for, plus 90s of a hook held open and an operator-visible failure per boot.
	//
	// 4 minutes is the shape of a Cilium bootstrap on the pool this controller runs on: the agent
	// pod is scheduled once the node is Ready, pulls its image, and programs the datapath. The gate
	// is a deadline, not a poll of a known duration: on a timeout the node is uncordoned, a Warning
	// is recorded on the slot, and the transition completes, so a longer value only delays the
	// signal that the CNI is really broken.
	DefaultReadinessGateTimeout = 4 * time.Minute

	// MaxUncordonRetryInterval is the ceiling of the exponential backoff the uncordon / readiness
	// loop sleeps between passes (see setNodeSchedulableWithRetry). It is a named constant because
	// claim.AttemptBudget is built from it: the budget of one attempt has to outlast every deadline
	// that loop can consume, plus one sleep at this ceiling, or a claim is failed at the instant its
	// own writer is about to record the machine it bought.
	MaxUncordonRetryInterval = 10 * time.Second

	// StuckTransitionThreshold is how long a transitional state (startingup,
	// shuttingdown) may last before CheckStuckState forces a cleanup. The event recorded
	// for that cleanup quotes it, so it is a named constant rather than a literal.
	StuckTransitionThreshold = 15 * time.Minute

	// JobMonitorPollInterval is how often the lifecycle Job monitor of a state machine reads
	// its Job (see monitorJobCompletion), and so how long after a Job finishes the
	// JobCompleted transition can be. It is a named constant because claim.AttemptBudget is built
	// from it: the deadline of a claim has to outlast the polls of the writer it defers to, or it
	// fails a claim whose transition is about to be recorded by its owner.
	JobMonitorPollInterval = 30 * time.Second
)

// NodeStateMachine represents the FSM for node state management
type NodeStateMachine struct {
	// FSM engine
	fsm *fsm.FSM

	// Kubernetes resources
	node   *infrav2alpha1.Node
	client client.Client
	scheme *runtime.Scheme

	// eventRecorder publishes Kubernetes Events on the Node CR for the lifecycle
	// transitions the FSM performs. Optional: nil records nothing (see WithEventRecorder).
	// It is the new events API (k8s.io/client-go/tools/events, i.e. events.k8s.io).
	eventRecorder events.EventRecorder

	// Coordination management
	coordinationManager CoordinationManager

	// Job tracking
	currentJob   string
	jobTimeout   time.Duration
	jobStartTime time.Time

	// Backoff strategy
	backoff *BackoffStrategy

	// Configuration
	uncordonTimeout time.Duration

	// readinessGateTimeout bounds the readiness gate of the scale-up, counted from the moment the
	// node object first appears (see setNodeSchedulableWithRetry and DefaultReadinessGateTimeout).
	readinessGateTimeout time.Duration

	// input is the whole input of this state machine, per the input law of the NodeClaim model:
	// (live claim phase, observed power, kubelet liveness / node readiness). It is never seeded
	// from a record of what the FSM last did - the derived Node.status.phase is this machine's
	// output, and reading it back as an input is the ambiguity the 0.1.20 watchdog and the 0.1.22
	// observed-power precedence rule existed to patch.
	input phase.Input

	// claim is the live NodeClaim the transition is buying a machine for, and the object whose
	// phase the transition moves. nil means the slot has no live claim: the transitions that run
	// with none are the shutdowns of a machine the pool no longer wants.
	claim *infrav2alpha1.NodeClaim

	// claimWriter moves the phase of the claim. nil writes nothing, which is what the unit tests
	// use; the controllers pass claim.Recorder through the adapter in internal/controller/infra.
	claimWriter ClaimWriter

	// readinessTaintKeys are the taint keys the readiness gate waits for the node to be free of, on
	// top of the Ready condition. They come from the slot's Group (spec.nodeReadinessTaints): the
	// key belongs to the CNI of the cluster and is not verifiable from this repository, so an empty
	// list gates on Ready alone.
	readinessTaintKeys []string
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
	CreateStartupJob  func(*infrav2alpha1.Node) (*batchv1.Job, error)
	CreateShutdownJob func(*infrav2alpha1.Node) (*batchv1.Job, error)

	// Status updates
	UpdateNodeStatus func(*infrav2alpha1.Node, string, metav1.Condition) error

	// Coordination
	AcquireLock func(string, time.Duration) error
	ReleaseLock func() error

	// Kubernetes operations
	UpdateKubernetesNode func(*corev1.Node) error
}

// CoordinationManager interface for managing coordination locks
type CoordinationManager interface {
	AcquireLock(ctx context.Context, node *infrav2alpha1.Node, operation string, timeout time.Duration) error
	ReleaseLock(ctx context.Context, node *infrav2alpha1.Node) error
	CheckLock(ctx context.Context, node *infrav2alpha1.Node) (*OperationLock, bool)
	IsLockExpired(lock *OperationLock) bool
	ForceReleaseLock(ctx context.Context, node *infrav2alpha1.Node) error
	// RenewLock re-stamps the timestamp of the live lock this manager owns, so an operation that
	// runs longer than the lock's timeout keeps the slot it already holds (see the implementation
	// for the exact rules). It acquires nothing and takes nothing over; it reports whether a stamp
	// was written.
	RenewLock(ctx context.Context, node *infrav2alpha1.Node) (bool, error)
}

// ClaimWriter moves the phase of a NodeClaim, with the reason that goes with the move. The
// state machine is the one that knows a startup Job exists, a readiness gate passed, a Job failed
// or a transition was force-cleaned, and those are the evidence for the claim's moves
// (pending -> booting, booting -> ready, -> failed). It writes nothing itself: the claim's phase
// table (internal/claim.LegalMoves) and its status writer live behind this interface, so the
// state machine does not import the claim package and the table has one enforcement point.
type ClaimWriter interface {
	MoveClaim(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim, to infrav2alpha1.NodeClaimPhase, reason string) error

	// RecordShutdownAttempt counts one shutdown attempt on the claim a scale-down named. It is a
	// method of this interface and not a write the state machine makes, because the claim's status
	// writer (and the bound the count feeds) lives behind it, and it is called where the shutdown is
	// fired: the path that created the shutdown Job. A reconcile pass that only looked at the machine
	// is not an attempt, and a streak counted per pass spends the bound on polling the observation
	// (docs/design/nodeclaim.md §4).
	RecordShutdownAttempt(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) error
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
