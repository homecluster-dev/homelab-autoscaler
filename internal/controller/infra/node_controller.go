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
	"strings"
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

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	autoscalerpkg "github.com/homecluster-dev/homelab-autoscaler/internal/autoscaler"
	"github.com/homecluster-dev/homelab-autoscaler/internal/capacity"
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

	// EventRecorder publishes Kubernetes Events on the Node CR for the scale up/down transitions the
	// FSM runs, and for the corrections the model records on a slot whose report and whose
	// observation disagree. nil records nothing, which is what the unit tests use.
	// Built once in cmd/main.go with mgr.GetEventRecorder, the new events API.
	EventRecorder events.EventRecorder
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/finalizers,verbs=update

// The input law of this reconciler is the live claim of the slot, and the recovery of a finished
// lifecycle Job is correlated to the claim that owns the operation (claim.BootCommittedAt,
// claim.ScaleDownRequestedAt), so it lists and reads NodeClaims. The rule is not decoration: the
// aggregated role picks the verbs up from the claim controller's markers today, and a deployment that
// ships this controller's rules on their own would run a reconciler that cannot read its input.
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodeclaims,verbs=get;list;watch

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
	node := &infrav2alpha1.Node{}
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
	group := &infrav2alpha1.Group{}
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

	// The input law of the NodeClaim model: the state machine is driven by
	//
	//	(live claim phase, observed power, kubelet liveness / node readiness)
	//
	// and by nothing else. The slot's spec carries no intent in v2alpha1 (spec.powerState is gone),
	// and status.phase is this pass's output, never its input (see internal/phase).
	live, err := r.liveClaimOf(ctx, node)
	if err != nil {
		logger.Error(err, "Failed to read the live claim of the slot", "node", node.Name)

		return ctrl.Result{}, err
	}

	input := r.lifecycleInput(ctx, node, live)

	result, err := r.handleStateTransitionsWithFSM(ctx, node, group, input)

	return result, err
}

// readinessTaintKeys returns the taint keys the scale-up readiness gate of this slot's Group waits
// for (spec.nodeReadinessTaints), and records them on the slot (autoscaler.ConditionReadinessTaints)
// so the gate is legible from the slot itself: "the slot has been booting for ten minutes, waiting
// for what" is a question an operator asks from `kubectl describe`, and the answer lives in a Group
// they will not think to open.
//
// The write is status-only, on a fresh copy, and only when the record differs, so a resync of a slot
// whose Group says the same thing as the slot writes nothing. It is best-effort: the keys the FSM
// gets come from the Group read in this pass, so a status write that fails costs the record, never
// the gate.
//
// With no Group to read (the pass has none in hand), the slot's own record is the fallback, so the
// gate keeps the keys the Group configured for the slot instead of quietly becoming Ready-only.
func (r *NodeReconciler) readinessTaintKeys(ctx context.Context, node *infrav2alpha1.Node, group *infrav2alpha1.Group) []string {
	if group == nil {
		return autoscalerpkg.ReadinessTaintKeysOfSlot(node)
	}

	keys := autoscalerpkg.ReadinessTaintKeys(group)

	if !autoscalerpkg.StampReadinessTaints(node, keys) {
		return keys
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav2alpha1.Node{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
			return err
		}

		if !autoscalerpkg.StampReadinessTaints(fresh, keys) {
			return nil
		}

		return r.Status().Update(ctx, fresh)
	}); err != nil {
		log.FromContext(ctx).V(1).Info("Failed to record the readiness taints of the Group on the slot",
			"node", node.Name, "group", group.Name, "taints", strings.Join(keys, ","), "error", err.Error())

		return keys
	}

	// Keep the caller's copy in step, so the pass does not write the same condition again.
	autoscalerpkg.StampReadinessTaints(node, keys)

	return keys
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
		For(&infrav2alpha1.Node{}).
		// A NodeClaim is owned by the slot it asks for (the ownerReference internal/claim.New stamps),
		// so every request, every phase move and every collection enqueues the slot. The input law reads
		// the claim, so a pass must run when the claim changes: a claim created while the manager was
		// down boots when it is created and not when something incidentally writes the Node, and a claim
		// whose lifecycle Job finished during a restart gets the pass that re-fires its event (see
		// recoverFinishedLifecycleJob).
		Owns(&infrav2alpha1.NodeClaim{}).
		Named("node").
		Complete(r)
}

// handleNodeDeletion handles the deletion of a node
func (r *NodeReconciler) handleNodeDeletion(ctx context.Context, node *infrav2alpha1.Node, groupName, nodeName string) (ctrl.Result, error) {
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

func (r *NodeReconciler) setProviderIDForKubernetesNode(ctx context.Context, node *infrav2alpha1.Node) error {
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
//     /network-unavailable), the two cluster-autoscaler keys, and Cilium's
//     node.cilium.io/agent-not-ready, which its agent stamps on a node that has just joined and
//     removes when its datapath is programmed. A key that merely shares the node.kubernetes.io
//     namespace, role=agent for instance, is not protected: the e2e pools declare exactly that and
//     must be applied.
//
// It is idempotent: when the node already carries every declared taint nothing is patched.
//
// Spec.TaintsToRemove is unrelated to this field: it filters taints out of the node template
// the provider reports to the autoscaler, it never touches the live node. Do not conflate
// them.
func (r *NodeReconciler) applyNodeTaintsToKubernetesNode(ctx context.Context, node *infrav2alpha1.Node) error {
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
func (r *NodeReconciler) checkDeclaredNodeCapacity(ctx context.Context, node *infrav2alpha1.Node) error {
	return capacity.CheckDeclaredCapacity(ctx, r.Client, node, r.EventRecorder)
}
func (r *NodeReconciler) SetControllerReference(ctx context.Context, node *infrav2alpha1.Node, group *infrav2alpha1.Group) error {
	logger := log.FromContext(ctx)

	// Check if the Node already has an owner reference to this Group
	for _, ownerRef := range node.OwnerReferences {
		if ownerRef.UID == group.UID && ownerRef.Kind == "Group" && ownerRef.APIVersion == "infra.homecluster.dev/v2alpha1" {
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
func (r *NodeReconciler) checkOperationLock(ctx context.Context, node *infrav2alpha1.Node) (*OperationLock, bool) {
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
	nodes := &infrav2alpha1.NodeList{}
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
func (r *NodeReconciler) forceReleaseLock(ctx context.Context, node *infrav2alpha1.Node) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav2alpha1.Node{}
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
