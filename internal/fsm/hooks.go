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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/looplab/fsm"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// Before hooks - executed before state transitions for lock acquisition
//
// The hooks are the only place a transition takes the coordination lock, and they run before
// looplab/fsm applies the state change and calls the enter_ hooks, the writers of
// status.progress inside the FSM. That ordering is the invariant the lifecycle rests on: a
// transition refused by the lock never reaches a writer, so status.progress keeps the stable
// value it had (ready for a scale-down that lost to a scale-up, shutdown for a scale-up that
// lost to a scale-down). A progress left in startingup/shuttingdown is not cosmetic: it is
// excluded from the provider's target size, validateStateTransition gates every transition of
// the slot out, and nothing in a reconcile heals it - the stuck-transition cleanup the docs
// used to promise runs on the machine's own backoff record, which a reconciler builds fresh on
// every pass, so ForceCleanup is never reached from there. The progress of a transitional slot
// is moved by the operation that put it there, from its own job monitor, and by no one else.
// That is why a refusal has to happen before a writer, not after it. See
// cancelLockedTransition and docs/architecture/state.md.

func (nm *NodeStateMachine) beforeStartNode(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.V(1).Info("Acquiring coordination lock for node startup", "node", nm.node.Name)

	// Acquire coordination lock for scale-up operation
	if err := nm.coordinationManager.AcquireLock(ctx, nm.node, OperationScaleUp, DefaultLockTimeout); err != nil {
		nm.cancelLockedTransition(logger, e, OperationScaleUp, err)
		return
	}

	logger.Info("Successfully acquired coordination lock for node startup", "node", nm.node.Name)
}

func (nm *NodeStateMachine) beforeShutdownNode(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.V(1).Info("Acquiring coordination lock for node shutdown", "node", nm.node.Name)

	// Acquire coordination lock for scale-down operation
	if err := nm.coordinationManager.AcquireLock(ctx, nm.node, OperationScaleDown, DefaultLockTimeout); err != nil {
		nm.cancelLockedTransition(logger, e, OperationScaleDown, err)
		return
	}

	logger.Info("Successfully acquired coordination lock for node shutdown", "node", nm.node.Name)
}

// cancelLockedTransition cancels a transition that failed to take the coordination lock.
//
// It writes nothing, and the caller has already touched nothing. That is the fix for a
// shutdown colliding with a scale-up in flight: pre-fix the lifecycle entry points cleaned up
// the pending Job of the slot, cordoned the Kubernetes node and cleared the
// ToBeDeletedByClusterAutoscaler taint before the acquisition ran, so the operation that was
// about to be refused tore the state of the operation that owns the slot apart on the way out,
// and the refusal was an anonymous error the autoscaler treated as a failed scale down: the
// live trace
//
//	ScaleDownFailed ... FSM transition failed: transition canceled with error:
//	  coordination lock already exists: operation=scale-up, owner=node-controller, age=4m0s
//
// Cancelling in the before_ hook, where no enter_ hook has run, keeps the progress on the
// stable state the transition started from, so the same reconcile path can ask for the
// shutdown again as soon as the lock is free.
//
// A lock conflict is that case and is reported as a deferral, not as an error: the opposite
// operation owns the slot for a bounded time, the caller learns how long from
// LockConflictError.RetryAfter, and the request is not lost because the intent it carries
// (spec.desiredPowerState) was written before the transition and stays there. Any other
// acquisition failure - the API server is unreachable, the lock annotations are unreadable -
// is a real failure and is logged as one, with the same cancellation semantics.
func (nm *NodeStateMachine) cancelLockedTransition(logger logr.Logger, e *fsm.Event, operation string, err error) {
	if conflict := AsLockConflict(err); conflict != nil {
		logger.V(1).Info("Deferring transition, the coordination lock is held by the operation in flight",
			"node", nm.node.Name,
			"operation", operation,
			"heldOperation", conflict.HolderOperation(),
			"heldOwner", conflict.HolderOwner(),
			"heldAge", conflict.HolderAge().String(),
			"reason", conflict.Reason,
			"retryAfter", conflict.RetryAfter().String(),
			"detail", err.Error())
	} else {
		logger.Error(err, "Failed to acquire coordination lock", "node", nm.node.Name, "operation", operation)
	}

	if e != nil {
		e.Cancel(err)
	}
}

// After hooks - executed after state transitions for lock release and status updates
func (nm *NodeStateMachine) afterJobCompleted(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	// If transitioning to Ready state (startup completed), uncordon the Kubernetes node
	if e.Dst == StateReady {
		logger.Info("Transitioning to Ready state, uncordonning Kubernetes node", "kubernetesNode", nm.node.Spec.KubernetesNodeName)

		// Retry uncordon with timeout to handle node registration delays
		// Note: We do this BEFORE releasing the lock to ensure the node is schedulable
		// before other operations can proceed
		//
		// The retry loop also stamps the taints the slot declares (spec.nodeTaints) onto the
		// node, on every pass, before the node is uncordoned: the taint has to be on the machine
		// before a pod can be bound to it, and the loop is the first place this controller knows
		// the node object exists. See setNodeSchedulableWithRetry.
		if err := nm.setNodeSchedulableWithRetry(nm.node.Spec.KubernetesNodeName); err != nil {
			logger.Error(err, "Failed to uncordon Kubernetes node after startup completion", "kubernetesNode", nm.node.Spec.KubernetesNodeName)
			// Continue anyway - don't block the operation if uncordon fails
			// The node will remain cordoned until manually uncordoned or until next startup
		} else {
			logger.Info("Successfully uncordoned Kubernetes node", "kubernetesNode", nm.node.Spec.KubernetesNodeName)
		}
	}

	// Release coordination lock after job completion
	if err := nm.coordinationManager.ReleaseLock(ctx, nm.node); err != nil {
		logger.Error(err, "Failed to release coordination lock after job completion", "node", nm.node.Name)
	} else {
		logger.Info("Successfully released coordination lock after job completion", "node", nm.node.Name)
	}

	// Update node status to reflect new state
	nm.updateNodeProgress(infrav1alpha1.Progress(e.Dst))

	// Jobs are cleaned up automatically via TTL - no manual cleanup needed
}

// afterJobTimeout releases the coordination lock of an operation whose lifecycle Job ran out
// of time. JobTimeout moves the slot back to a stable state (ready for a shutdown that timed
// out, shutdown for a startup that timed out) and the enter_ hook records the failed scale
// up/down, so the operation is over - and the lock is the one thing that must not outlive it.
// With no hook here, the lock stayed on the slot for the rest of its timeout, the next
// lifecycle operation on that slot lost the acquisition, and that is the shape of the live
// trace this fix starts from: a scale-down cancelled by a scale-up lock that nothing was
// running any more.
//
// The lock is force-released: the operation it names has ended, whoever wrote it. Releasing
// only an owned lock would leave a lock written by an earlier state machine on a slot that is
// now stable, which is exactly what blocks the next transition. No event is recorded: the
// enter_ hook of the state the slot landed in records the failure of the timed-out operation,
// so this hook adds nothing the operator has to see.
func (nm *NodeStateMachine) afterJobTimeout(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Lifecycle job timed out, releasing the coordination lock", "node", nm.node.Name, "state", e.Dst)

	if err := nm.coordinationManager.ForceReleaseLock(ctx, nm.node); err != nil {
		logger.Error(err, "Failed to release coordination lock after job timeout", "node", nm.node.Name)
	}
}

func (nm *NodeStateMachine) afterJobFailed(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Job failed, releasing coordination lock", "node", nm.node.Name, "state", e.Dst)

	// Release coordination lock after failure
	if err := nm.coordinationManager.ReleaseLock(ctx, nm.node); err != nil {
		logger.Error(err, "Failed to release coordination lock after job failure", "node", nm.node.Name)
	} else {
		logger.Info("Successfully released coordination lock after job failure", "node", nm.node.Name)
	}

	// Update node status to reflect failure and new state
	nm.updateNodeProgress(infrav1alpha1.Progress(e.Dst))

	// Add failure condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "JobFailed",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "AsyncJobFailed",
		Message:            fmt.Sprintf("Job failed, transitioned from %s to %s", e.Src, e.Dst),
	})
}

func (nm *NodeStateMachine) afterForceCleanup(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Force cleanup executed, releasing coordination lock", "node", nm.node.Name)

	// A forced cleanup means the operation in flight got stuck, which is a failed
	// scale up/down from the operator's point of view. The enter_ hooks suppress their
	// completion event for this event, so exactly one Warning is recorded.
	if e != nil {
		nm.recordScaleFailure(e.Src == StateShuttingDown,
			fmt.Sprintf("transition was stuck for more than %v, forced cleanup moved %s to %s",
				StuckTransitionThreshold, e.Src, e.Dst))
	}

	// Force release coordination lock
	if err := nm.coordinationManager.ForceReleaseLock(ctx, nm.node); err != nil {
		logger.Error(err, "Failed to force release coordination lock", "node", nm.node.Name)
	} else {
		logger.Info("Successfully force released coordination lock", "node", nm.node.Name)
	}

	// Update node status to reflect cleanup
	nm.updateNodeProgress(infrav1alpha1.Progress(e.Dst))

	// Add cleanup condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "ForceCleanup",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "StuckTransitionCleanup",
		Message:            fmt.Sprintf("Force cleanup executed, transitioned from %s to %s", e.Src, e.Dst),
	})
}

// State entry hooks - executed when entering specific states

func (nm *NodeStateMachine) enterStartingUp(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Entering StartingUp state", "node", nm.node.Name)

	// Reset backoff strategy for new transition
	nm.backoff = &BackoffStrategy{
		transitionStart: time.Now(),
		retryCount:      0,
		maxRetries:      MaxRetries,
	}

	// Update node status
	nm.updateNodeProgress(infrav1alpha1.ProgressStartingUp)

	// Add progressing condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "StartupProgressing",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeStarting",
		Message:            "Node startup operation initiated",
	})

	// Reached only when the shutdown -> startingup transition actually happened, so this
	// records one event per scale-up request and never on a plain resync.
	nm.recordLifecycleEvent(corev1.EventTypeNormal, ReasonScaleUpRequested, ActionPowerOn,
		fmt.Sprintf("Scale up requested for %s: machine is being powered on", nm.lifecycleTarget()))
}

func (nm *NodeStateMachine) enterShuttingDown(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Entering ShuttingDown state", "node", nm.node.Name)

	// Reset backoff strategy for new transition
	nm.backoff = &BackoffStrategy{
		transitionStart: time.Now(),
		retryCount:      0,
		maxRetries:      MaxRetries,
	}

	// Update node status
	nm.updateNodeProgress(infrav1alpha1.ProgressShuttingDown)

	// Add progressing condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "ShutdownProgressing",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeShuttingDown",
		Message:            "Node shutdown operation initiated",
	})

	// See enterStartingUp: entered on the real ready -> shuttingdown transition only.
	nm.recordLifecycleEvent(corev1.EventTypeNormal, ReasonScaleDownRequested, ActionPowerOff,
		fmt.Sprintf("Scale down requested for %s: machine is being powered off", nm.lifecycleTarget()))
}

func (nm *NodeStateMachine) enterReady(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Entering Ready state", "node", nm.node.Name)

	// Clear backoff strategy
	nm.backoff = nil

	// Update node status
	nm.updateNodeProgress(infrav1alpha1.ProgressReady)

	// Add ready condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeReady",
		Message:            "Node is ready and operational",
	})

	// Arriving in ready is a completed scale up when the startup Job finished. A failed
	// rollback back into ready is recorded by the site that knows the error (StartNode,
	// ShutdownNode, the Job monitor), so it is not recorded twice here.
	if e == nil {
		return
	}

	switch e.Event {
	case EventJobCompleted:
		nm.recordLifecycleEvent(corev1.EventTypeNormal, ReasonScaleUpCompleted, ActionPowerOn,
			fmt.Sprintf("Scale up completed for %s: startup job finished, machine is on and the slot is ready",
				nm.lifecycleTarget()))
	case EventJobTimeout:
		nm.recordScaleFailure(true,
			fmt.Sprintf("shutdown job did not finish in time, slot stayed on and returned to %s", e.Dst))
	}
}

func (nm *NodeStateMachine) enterShutdown(ctx context.Context, e *fsm.Event) {
	logger := log.Log.WithName("fsm")

	logger.Info("Entering Shutdown state", "node", nm.node.Name)

	// Clear backoff strategy
	nm.backoff = nil

	// Update node status
	nm.updateNodeProgress(infrav1alpha1.ProgressShutdown)

	// Add shutdown condition
	nm.addNodeCondition(metav1.Condition{
		Type:               "Shutdown",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeShutdown",
		Message:            "Node is shutdown and powered off",
	})

	// See enterReady: entering shutdown is a completed scale down when the shutdown Job
	// finished; a startup that died on the way is recorded by the site that has the error.
	if e == nil {
		return
	}

	switch e.Event {
	case EventJobCompleted:
		nm.recordLifecycleEvent(corev1.EventTypeNormal, ReasonScaleDownCompleted, ActionPowerOff,
			fmt.Sprintf("Scale down completed for %s: shutdown job finished, machine is off and the slot is shut down",
				nm.lifecycleTarget()))
	case EventJobTimeout:
		nm.recordScaleFailure(false,
			fmt.Sprintf("startup job did not finish in time, slot returned to %s", e.Dst))
	}
}

// Helper methods for status updates are implemented in node_state_machine.go
