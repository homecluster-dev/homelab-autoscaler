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

package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/capacity"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/progress"
	"github.com/homecluster-dev/homelab-autoscaler/internal/taints"
)

// Coordination annotation keys for preventing race conditions with cluster autoscaler
const (
	OperationLockAnnotation = "homelab-autoscaler.dev/operation-lock"
	LockOwnerAnnotation     = "homelab-autoscaler.dev/lock-owner"
	LockTimestampAnnotation = "homelab-autoscaler.dev/lock-timestamp"
	LockTimeoutAnnotation   = "homelab-autoscaler.dev/lock-timeout"
	DefaultLockTimeout      = 5 * time.Minute
	NodeControllerOwner     = "node-controller"
)

// OperationLock represents a coordination lock on a node
type OperationLock struct {
	Operation string
	Owner     string
	Timestamp time.Time
	Timeout   time.Duration
}

// NodeReconciler reconciles a Node object
type NodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// EventRecorder publishes Kubernetes Events on the Node CR for the scale up/down
	// transitions the FSM runs and for the progress settlements the watchdog makes (see
	// internal/progress). nil records nothing, which is what the unit tests use.
	// Built once in cmd/main.go with mgr.GetEventRecorder, the new events API.
	EventRecorder events.EventRecorder
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/finalizers,verbs=update

// Needed to record why a slot was powered on/off on the Node CR, so that
// `kubectl describe node.infra.homecluster.dev <name>` shows the lifecycle transitions.
// The controller records through the new events API (k8s.io/client-go/tools/events), so the
// events it creates live in the events.k8s.io group. The core "" rule stays: the client-go
// broadcaster mirrors to it in most setups, and that is what `kubectl describe` shows on
// older clusters.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Clean up expired locks at the beginning of reconciliation
	if err := r.cleanupExpiredLocks(ctx); err != nil {
		logger.Error(err, "Failed to cleanup expired locks, continuing with reconciliation")
		// Don't fail reconciliation due to cleanup errors, just log and continue
	}

	// Get the Node resource from the request
	node := &infrav1alpha1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// The object was not found, return early
		return ctrl.Result{}, err
	}

	if node.Labels["group"] != "" && node.Labels["infra.homecluster.dev/group"] != node.Labels["group"] {
		if err := r.addLabelToKubernetesNodeWithPatch(ctx, node.Spec.KubernetesNodeName, "infra.homecluster.dev/group", node.Labels["group"]); err != nil {
			logger.Error(err, "Failed to add group label to Kubernetes node", "node", node.Spec.KubernetesNodeName)
		}
		if err := r.addLabelToKubernetesNodeWithPatch(ctx, node.Spec.KubernetesNodeName, "infra.homecluster.dev/node", node.Name); err != nil {
			logger.Error(err, "Failed to add node label to Kubernetes node", "node", node.Spec.KubernetesNodeName)
		}
	}
	// Check if the Node has a group label
	groupName, hasGroupLabel := node.Labels["group"]
	if !hasGroupLabel {
		logger.Info("Node does not have a group label, skipping owner reference management", "node", node.Name)
		return ctrl.Result{}, fmt.Errorf("node %s has no group label, skipping", node.Name)
	}

	// Get the Group CR referenced by the label
	group := &infrav1alpha1.Group{}
	if err := r.Get(ctx, types.NamespacedName{Name: groupName, Namespace: node.Namespace}, group); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Group referenced by node label not found, requeuing", "group", groupName, "node", node.Name)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		logger.Error(err, "Failed to get Group", "group", groupName)
		return ctrl.Result{}, err
	}

	if err := r.SetControllerReference(ctx, node, group); err != nil {
		logger.Error(err, "Failed to set controller reference", "node", node.Name, "group", group.Name)
		return ctrl.Result{}, err
	}

	// Set providerID for Cluster Autoscaler integration
	if err := r.setProviderIDForKubernetesNode(ctx, node); err != nil {
		logger.Error(err, "Failed to set providerID", "node", node.Name)
		// Don't fail reconciliation, just log and continue
	}

	// Stamp the taints the slot declares (spec.nodeTaints) onto the Kubernetes node
	if err := r.applyNodeTaintsToKubernetesNode(ctx, node); err != nil {
		logger.Error(err, "Failed to apply declared taints", "node", node.Name)
		// Don't fail reconciliation, just log and continue
	}

	// Compare the machine that registered with the shape the slot declares (spec.nodeCapacity),
	// and warn when they disagree
	if err := r.checkDeclaredNodeCapacity(ctx, node); err != nil {
		logger.Error(err, "Failed to check the declared node capacity", "node", node.Name)
		// Don't fail reconciliation, just log and continue
	}

	if !node.DeletionTimestamp.IsZero() {
		return r.handleNodeDeletion(ctx, node, groupName, req.Name)
	}

	// Settle a status.progress that outlived the machine behind it, before any FSM branch can act
	// on it. This runs first on purpose: the slot below is the live failure, a progress of ready
	// (or a transitional one) with no corev1.Node and no lifecycle Job, and every branch that sees
	// it wants to act on a machine that is not there. From ready the FSM fires a ShutdownNode and
	// builds a shutdown Job for a server that was already deleted; a transitional progress is
	// gated out of every transition by validateStateTransition, so it is simply frozen. Neither
	// shape heals itself - see internal/progress for why not.
	//
	// The wrapper returns the deadline the watchdog asked for when it declined to settle (a Job in
	// flight, a Job that just finished inside the grace), which is folded into the result below so
	// the settlement is scheduled and not left to the next resync.
	watchdogRequeue := r.reconcileObservedProgress(ctx, node)

	// Validate state before attempting transitions with smart backoff
	if result, err := r.validateStateTransition(ctx, node); err != nil {
		logger.Error(err, "Invalid state transition", "node", node.Name)
		return ctrl.Result{}, err
	} else if result.RequeueAfter > 0 {
		// Smart backoff is requesting a requeue
		return shortenRequeue(result, watchdogRequeue), nil
	}

	// Handle power state transitions using FSM
	result, err := r.handleStateTransitionsWithFSM(ctx, node)

	return shortenRequeue(result, watchdogRequeue), err
}

// reconcileObservedProgress settles a slot whose status.progress names a machine the cluster does
// not have, and returns how long the watchdog wants to be looked at again (0 = no deadline). See
// internal/progress for the rule, the grace period it is built on, and the event it records.
//
// A failure of the watchdog is never fatal to the pass: it means the settlement did not happen,
// and the next pass is the next attempt. Failing a reconcile because a slot that is already
// broken could not be repaired would hide the reason it is broken.
func (r *NodeReconciler) reconcileObservedProgress(ctx context.Context, node *infrav1alpha1.Node) time.Duration {
	outcome, err := progress.Settle(ctx, r.Client, node, r.EventRecorder, progress.SettleGracePeriod)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to reconcile status.progress against observed reality",
			"node", node.Name, "progress", string(node.Status.Progress))

		return progress.SettleGracePeriod
	}

	return outcome.RequeueAfter
}

// shortenRequeue moves a reconcile result forward to an earlier deadline the caller was asked to
// honour, and never delays one: a requeue the FSM asked for, a transition that finished and wants
// to run now, wins over the watchdog's deadline.
func shortenRequeue(result ctrl.Result, requeueAfter time.Duration) ctrl.Result {
	if requeueAfter <= 0 {
		return result
	}

	if result.RequeueAfter > 0 && result.RequeueAfter <= requeueAfter {
		return result
	}

	result.RequeueAfter = requeueAfter

	return result
}

// handleStateTransitionsWithFSM uses the FSM for state management
func (r *NodeReconciler) handleStateTransitionsWithFSM(ctx context.Context, node *infrav1alpha1.Node) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Create coordination manager
	coordMgr := fsm.NewCoordinationManager(r.Client, fsm.NodeControllerOwner)

	// Create FSM instance, recording the lifecycle transitions on the Node CR.
	stateMachine := fsm.NewNodeStateMachine(node, r.Client, r.Scheme, coordMgr,
		fsm.WithEventRecorder(r.EventRecorder))

	logger.Info("Using FSM for state management",
		"node", node.Name,
		"currentState", stateMachine.GetCurrentState(),
		"desiredPowerState", node.Spec.DesiredPowerState)

	// Handle desired state transitions
	if node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOn &&
		(node.Status.PowerState == infrav1alpha1.PowerStateOff || node.Status.PowerState == "") &&
		stateMachine.CanTransition(fsm.EventStartNode) {

		if err := stateMachine.StartNode(); err != nil {
			if result, deferred := deferTransitionOnLockConflict(ctx, node, err); deferred {
				return result, nil
			}
			// controller-runtime discards the Result when the error is non-nil, the requeue
			// backoff is decided by the controller, so only the error is returned here.
			return ctrl.Result{}, err
		}

	} else if node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOff &&
		node.Status.PowerState == infrav1alpha1.PowerStateOn &&
		stateMachine.CanTransition(fsm.EventShutdownNode) {

		if err := stateMachine.ShutdownNode(); err != nil {
			if result, deferred := deferTransitionOnLockConflict(ctx, node, err); deferred {
				return result, nil
			}
			// controller-runtime discards the Result when the error is non-nil, the requeue
			// backoff is decided by the controller, so only the error is returned here.
			return ctrl.Result{}, err
		}

	} else {
		// Check for stuck transitions
		stateMachine.CheckStuckState()

		// Node is already in desired state or transitioning
		logger.Info("Node is in desired state or transitioning",
			"node", node.Name,
			"desiredState", node.Spec.DesiredPowerState,
			"currentState", node.Status.PowerState,
			"progress", node.Status.Progress,
			"fsmState", stateMachine.GetCurrentState())

		// Use FSM backoff if in transitional state, otherwise standard requeue
		if fsm.IsTransitionalState(stateMachine.GetCurrentState()) {
			return ctrl.Result{RequeueAfter: stateMachine.CalculateBackoff()}, nil
		}
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
	}

	logger.Info("Successfully processed FSM state transition", "node", node.Name)
	return ctrl.Result{}, nil
}

// deferTransitionOnLockConflict turns a lifecycle transition that lost the coordination lock
// into a requeue at the deadline the lock itself names, and reports whether it did.
//
// The refused operation is not lost. Its intent lives in spec.desiredPowerState, which is what
// cluster-autoscaler writes through NodeGroupDeleteNodes and NodeGroupDecreaseTargetSize, so
// the very same branch that produced the transition runs again on the requeue and the shutdown
// fires as soon as the scale-up in flight releases the slot - no second DeleteNodes from the
// autoscaler, no operator intervention. Returning the error instead hands that retry to
// controller-runtime's rate-limited queue, and the only record that a shutdown was asked for
// at all is a warning on the Node CR: the live shape this closes.
//
// Only a lock conflict is deferred this way. Any other failure of a transition is a real
// failure and is returned as an error, so it keeps the controller's backoff, its log line and
// its metrics.
func deferTransitionOnLockConflict(ctx context.Context, node *infrav1alpha1.Node, err error) (ctrl.Result, bool) {
	conflict := fsm.AsLockConflict(err)
	if conflict == nil {
		return ctrl.Result{}, false
	}

	// The lifecycle transitions in fsm record the deferral as a Normal Scale*Deferred event
	// on the Node CR; the reason is the coordination lock, so the log says which operation
	// owns the slot and when the retry runs, at V(1): a collision between a scale-up and a
	// scale-down is a scheduled event on a homelab, not an incident.
	log.FromContext(ctx).V(1).Info("FSM transition deferred by the coordination lock",
		"node", node.Name,
		"deferredOperation", conflict.Operation,
		"heldOperation", conflict.HolderOperation(),
		"heldOwner", conflict.HolderOwner(),
		"requeueAfter", conflict.RetryAfter().String(),
		"reason", err.Error())

	return ctrl.Result{RequeueAfter: conflict.RetryAfter()}, true
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Register field index for pods by node name
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{pod.Spec.NodeName}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.Node{}).
		Named("node").
		Complete(r)
}

// handleNodeDeletion handles the deletion of a node
func (r *NodeReconciler) handleNodeDeletion(ctx context.Context, node *infrav1alpha1.Node, groupName, nodeName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Node is being deleted", "node", nodeName)

	return ctrl.Result{}, nil
}

func (r *NodeReconciler) addLabelToKubernetesNodeWithPatch(ctx context.Context, nodeName, labelKey, labelValue string) error {
	logger := log.FromContext(ctx)

	// Get the Kubernetes node
	kubernetesNode := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, kubernetesNode); err != nil {
		logger.Error(err, "Failed to get Kubernetes node", "node", nodeName)
		return err
	}

	// Create a patch with the new label
	patch := client.MergeFrom(kubernetesNode.DeepCopy())

	// Initialize the Labels map if it's nil
	if kubernetesNode.Labels == nil {
		kubernetesNode.Labels = make(map[string]string)
	}

	// Add the label
	kubernetesNode.Labels[labelKey] = labelValue

	// Apply the patch
	if err := r.Patch(ctx, kubernetesNode, patch); err != nil {
		logger.Error(err, "Failed to patch Kubernetes node with label", "node", nodeName, "label", labelKey)
		return err
	}

	logger.Info("Successfully patched Kubernetes node with label", "node", nodeName, "label", labelKey, "value", labelValue)
	return nil
}

func (r *NodeReconciler) setProviderIDForKubernetesNode(ctx context.Context, node *infrav1alpha1.Node) error {
	logger := log.FromContext(ctx)

	// Get the Kubernetes node
	kubernetesNode := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Spec.KubernetesNodeName}, kubernetesNode); err != nil {
		logger.Error(err, "Failed to get Kubernetes node", "node", node.Spec.KubernetesNodeName)
		return err
	}

	// Check if providerID is already set
	if kubernetesNode.Spec.ProviderID != "" {
		return nil
	}

	// Create a patch to set the providerID
	patch := client.MergeFrom(kubernetesNode.DeepCopy())

	// Set the providerID using the homelab scheme
	kubernetesNode.Spec.ProviderID = fmt.Sprintf("homelab://%s", node.Name)

	// Apply the patch
	if err := r.Patch(ctx, kubernetesNode, patch); err != nil {
		logger.Error(err, "Failed to patch Kubernetes node with providerID", "node", node.Spec.KubernetesNodeName)
		return err
	}

	logger.Info("Successfully set providerID for Kubernetes node", "node", node.Spec.KubernetesNodeName, "providerID", kubernetesNode.Spec.ProviderID)
	return nil
}

// applyNodeTaintsToKubernetesNode stamps the taints the Node CR declares (spec.nodeTaints)
// onto the Kubernetes node. It is a thin wrapper over taints.ApplyToKubernetesNode, the single
// implementation the FSM's uncordon path calls too, so the two writers cannot drift.
//
// Semantics, in short (taints.ApplyToKubernetesNode and taints.Merge hold the rules):
//   - a declared taint is added, an existing taint with the same key is overwritten;
//   - taints that are not declared are never removed, and a declared taint an operator deleted
//     with `kubectl taint <node> ...-` is put back on the next reconcile: the declaration is
//     the intent, a hand removal is not;
//   - the keys a Kubernetes component owns are skipped, and the skip is surfaced with a Warning
//     event on the Node CR. The set is explicit in taints.IsProtectedKey - the lifecycle and
//     kubelet keys (node.kubernetes.io/not-ready, /unreachable, /unschedulable,
//     /out-of-service, /memory-pressure, /disk-pressure, /pid-pressure,
//     /network-unavailable) and the two cluster-autoscaler keys. A key that merely shares the
//     node.kubernetes.io namespace, role=agent for instance, is not protected: the e2e pools
//     declare exactly that and must be applied.
//
// It is idempotent: when the node already carries every declared taint nothing is patched.
//
// Spec.TaintsToRemove is unrelated to this field: it filters taints out of the node template
// the provider reports to the autoscaler, it never touches the live node. Do not conflate
// them.
func (r *NodeReconciler) applyNodeTaintsToKubernetesNode(ctx context.Context, node *infrav1alpha1.Node) error {
	return taints.ApplyToKubernetesNode(ctx, r.Client, node, r.EventRecorder)
}

// checkDeclaredNodeCapacity compares the Kubernetes node of a Ready slot with the capacity that
// slot declares (spec.nodeCapacity), and records a Warning event on the Node CR when the
// allocatable the kubelet reports is more than five percent away from the declaration on any
// declared resource. The event names the resource and both values, so a declaration that went
// stale - the VM was resized, the system reservation moved, the numbers came from the datasheet
// instead of from a booted machine - is visible from `kubectl describe` instead of showing up as
// a pool that never scales up.
//
// It is a check, not a reconcile. The node is never patched: the kubelet owns
// status.allocatable, and a running machine is the truth about what booted. Nothing is written on
// the Node CR to remember that the warning fired; the API server's event aggregation folds the
// repeats of an identical reason into a count.
//
// Thin wrapper over capacity.CheckDeclaredCapacity, which holds the rules (no declaration, no
// node object, and a node that is not Ready all record nothing).
func (r *NodeReconciler) checkDeclaredNodeCapacity(ctx context.Context, node *infrav1alpha1.Node) error {
	return capacity.CheckDeclaredCapacity(ctx, r.Client, node, r.EventRecorder)
}

// validateStateTransition validates that a state transition is valid and implements smart backoff strategy
// Returns ctrl.Result with appropriate requeue timing instead of errors for transitional states
func (r *NodeReconciler) validateStateTransition(ctx context.Context, node *infrav1alpha1.Node) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check for context cancellation
	if ctx.Err() != nil {
		return ctrl.Result{}, ctx.Err()
	}

	// Validate that we have required fields
	if node.Spec.DesiredPowerState == "" {
		return ctrl.Result{}, fmt.Errorf("node %s has empty DesiredPowerState", node.Name)
	}

	// Validate that DesiredPowerState is a valid value
	if node.Spec.DesiredPowerState != infrav1alpha1.PowerStateOn &&
		node.Spec.DesiredPowerState != infrav1alpha1.PowerStateOff {
		return ctrl.Result{}, fmt.Errorf("node %s has invalid DesiredPowerState: %s", node.Name, node.Spec.DesiredPowerState)
	}

	// Check if node is in a transitional state and apply smart backoff
	if node.Status.Progress == infrav1alpha1.ProgressStartingUp ||
		node.Status.Progress == infrav1alpha1.ProgressShuttingDown {

		// Check for existing operation locks before allowing transitions
		if existingLock, exists := r.checkOperationLock(ctx, node); exists {
			if !r.isLockExpired(existingLock) {
				// Respect locks from other controllers (like cluster autoscaler)
				if existingLock.Owner != NodeControllerOwner {
					logger.Info("Respecting coordination lock from other controller",
						"node", node.Name,
						"lockOwner", existingLock.Owner,
						"operation", existingLock.Operation,
						"age", time.Since(existingLock.Timestamp))
					return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
				}
			} else {
				// Clean up expired locks automatically
				logger.Info("Cleaning up expired coordination lock",
					"node", node.Name,
					"operation", existingLock.Operation,
					"owner", existingLock.Owner,
					"age", time.Since(existingLock.Timestamp))
				if err := r.forceReleaseLock(ctx, node); err != nil {
					logger.Error(err, "Failed to cleanup expired lock", "node", node.Name)
				}
			}
		}

		// Calculate transition duration based on the appropriate timestamp
		var transitionStart time.Time
		if node.Status.Progress == infrav1alpha1.ProgressStartingUp && node.Status.LastStartupTime != nil {
			transitionStart = node.Status.LastStartupTime.Time
		} else if node.Status.Progress == infrav1alpha1.ProgressShuttingDown && node.Status.LastShutdownTime != nil {
			transitionStart = node.Status.LastShutdownTime.Time
		} else {
			// Fallback to a reasonable default if timestamps are missing
			transitionStart = time.Now().Add(-1 * time.Minute)
		}

		transitionDuration := time.Since(transitionStart)

		// Implement progressive backoff strategy
		var backoffDuration time.Duration
		var logMessage string

		switch {
		case transitionDuration <= 2*time.Minute:
			// Early transitions (0-2 min): 30-second backoff, allow monitoring
			backoffDuration = 30 * time.Second
			logMessage = "Early transition phase - monitoring with short backoff"
		case transitionDuration <= 10*time.Minute:
			// Normal transitions (2-10 min): 2-minute backoff
			backoffDuration = 2 * time.Minute
			logMessage = "Normal transition phase - standard backoff"
		case transitionDuration <= 15*time.Minute:
			// Late transitions (10-15 min): 5-minute backoff
			backoffDuration = 5 * time.Minute
			logMessage = "Late transition phase - extended backoff"
		default:
			// Stuck transitions (>15 min): Force retry/cleanup
			logger.Info("Transition appears stuck, forcing cleanup and retry",
				"node", node.Name,
				"progress", node.Status.Progress,
				"transitionDuration", transitionDuration)

			// Clean up any existing locks and allow the transition to proceed
			if _, exists := r.checkOperationLock(ctx, node); exists {
				if err := r.forceReleaseLock(ctx, node); err != nil {
					logger.Error(err, "Failed to force release stuck lock", "node", node.Name)
				}
			}

			// Return no requeue to allow immediate retry
			return ctrl.Result{}, nil
		}

		logger.Info(logMessage,
			"node", node.Name,
			"progress", node.Status.Progress,
			"transitionDuration", transitionDuration,
			"backoffDuration", backoffDuration)

		return ctrl.Result{RequeueAfter: backoffDuration}, nil
	}

	// Node is not in transitional state, allow normal processing
	return ctrl.Result{}, nil
}
func (r *NodeReconciler) SetControllerReference(ctx context.Context, node *infrav1alpha1.Node, group *infrav1alpha1.Group) error {
	logger := log.FromContext(ctx)

	// Check if the Node already has an owner reference to this Group
	for _, ownerRef := range node.OwnerReferences {
		if ownerRef.UID == group.UID && ownerRef.Kind == "Group" && ownerRef.APIVersion == "infra.homecluster.dev/v1alpha1" {
			return nil
		}
	}

	// Set the owner reference using controllerutil.SetControllerReference
	if err := ctrl.SetControllerReference(group, node, r.Scheme); err != nil {
		logger.Error(err, "Failed to set controller reference", "node", node.Name, "group", group.Name)
		return err
	}

	// Update the Node with the new owner reference
	if err := r.Update(ctx, node); err != nil {
		logger.Error(err, "Failed to update Node with owner reference", "node", node.Name)
		return err
	}

	logger.Info("Successfully set owner reference for Node to Group", "node", node.Name, "group", group.Name)

	return nil
}

// checkOperationLock checks if a node has an active operation lock and returns the lock details.
// Returns (lock, true) if an active lock exists, (nil, false) otherwise.
func (r *NodeReconciler) checkOperationLock(ctx context.Context, node *infrav1alpha1.Node) (*OperationLock, bool) {
	logger := log.FromContext(ctx)

	if node.Annotations == nil {
		return nil, false
	}

	operation := node.Annotations[OperationLockAnnotation]
	owner := node.Annotations[LockOwnerAnnotation]
	timestampStr := node.Annotations[LockTimestampAnnotation]
	timeoutStr := node.Annotations[LockTimeoutAnnotation]

	// Check if required annotations exist
	if operation == "" || owner == "" || timestampStr == "" {
		return nil, false
	}

	// Parse timestamp
	timestamp, err := time.Parse(time.RFC3339, timestampStr)
	if err != nil {
		logger.V(1).Info("Invalid lock timestamp, treating as no lock",
			"node", node.Name,
			"timestamp", timestampStr,
			"error", err)
		return nil, false
	}

	// Parse timeout (default to 5 minutes if invalid)
	timeout := DefaultLockTimeout
	if timeoutStr != "" {
		if timeoutSeconds, err := strconv.ParseFloat(timeoutStr, 64); err == nil {
			timeout = time.Duration(timeoutSeconds) * time.Second
		} else {
			logger.V(1).Info("Invalid lock timeout, using default",
				"node", node.Name,
				"timeout", timeoutStr,
				"default", DefaultLockTimeout)
		}
	}

	lock := &OperationLock{
		Operation: operation,
		Owner:     owner,
		Timestamp: timestamp,
		Timeout:   timeout,
	}

	return lock, true
}

// isLockExpired checks if an existing lock has expired based on timestamp and timeout.
// Returns true if the lock has exceeded its timeout duration.
func (r *NodeReconciler) isLockExpired(lock *OperationLock) bool {
	if lock == nil {
		return true
	}

	age := time.Since(lock.Timestamp)
	return age > lock.Timeout
}

// cleanupExpiredLocks removes expired locks from all nodes in the cluster.
// This should be called periodically to prevent stale locks from blocking operations.
func (r *NodeReconciler) cleanupExpiredLocks(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// List all nodes to check for expired locks
	nodes := &infrav1alpha1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return fmt.Errorf("failed to list nodes for lock cleanup: %w", err)
	}

	cleanedCount := 0
	for _, node := range nodes.Items {
		if lock, exists := r.checkOperationLock(ctx, &node); exists {
			if r.isLockExpired(lock) {
				logger.Info("Cleaning up expired coordination lock",
					"node", node.Name,
					"operation", lock.Operation,
					"owner", lock.Owner,
					"age", time.Since(lock.Timestamp),
					"timeout", lock.Timeout)

				if err := r.forceReleaseLock(ctx, &node); err != nil {
					logger.Error(err, "Failed to cleanup expired lock", "node", node.Name)
					continue
				}
				cleanedCount++
			}
		}
	}

	if cleanedCount > 0 {
		logger.Info("Completed expired lock cleanup", "cleanedLocks", cleanedCount)
	}

	return nil
}

// forceReleaseLock forcibly removes lock annotations from a node, regardless of ownership.
// This is used for cleanup of expired locks and should be used with caution.
func (r *NodeReconciler) forceReleaseLock(ctx context.Context, node *infrav1alpha1.Node) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
			return fmt.Errorf("failed to get fresh node: %w", err)
		}

		if fresh.Annotations == nil {
			return nil // No annotations to clean up
		}

		// Remove all coordination lock annotations
		delete(fresh.Annotations, OperationLockAnnotation)
		delete(fresh.Annotations, LockOwnerAnnotation)
		delete(fresh.Annotations, LockTimestampAnnotation)
		delete(fresh.Annotations, LockTimeoutAnnotation)

		// Update the node
		return r.Update(ctx, fresh)
	})
}
