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
	"errors"
	"fmt"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// NOTE: Namespace configuration is not needed in the coordination manager.
// The original variable caused an "unused" lint warning and has been removed.

// Coordination annotation keys for preventing race conditions with cluster autoscaler
const (
	OperationLockAnnotation = "homelab-autoscaler.dev/operation-lock"
	LockOwnerAnnotation     = "homelab-autoscaler.dev/lock-owner"
	LockTimestampAnnotation = "homelab-autoscaler.dev/lock-timestamp"
	LockTimeoutAnnotation   = "homelab-autoscaler.dev/lock-timeout"
	NodeControllerOwner     = "node-controller"
)

// Retry bounds applied to a transition that lost the coordination lock. RetryAfter never
// returns more than the lock timeout, because a lock older than its timeout is treated as
// expired and is taken over by the next acquisition.
const (
	MinLockRetryDelay = 5 * time.Second
	MaxLockRetryDelay = DefaultLockTimeout
)

// unknownLockValue is what a conflict reports for a lock that could not be read (the slot
// moved under a retry), so the log line keeps its shape instead of printing an empty field.
const unknownLockValue = "unknown"

// Why an acquisition was refused. Both checks are kept, and the message of the older
// one is preserved verbatim: it is what the operators grep for.
const (
	lockReasonAlreadyExists  = "already exists"
	lockReasonStolenByOther  = "acquired by another process"
	lockReasonChangedOnRetry = "changed during retry"
)

// LockConflictError reports that an operation lost the coordination lock of a slot to the
// operation that holds it. It is a typed error because a conflict is not a failed
// transition: nothing about the slot is wrong, the opposite lifecycle operation is in flight
// on the same slot and the lock is doing its job. The caller that sees it must defer the
// request (the intent it acts on is already recorded in spec.desiredPowerState, written by
// cluster-autoscaler's NodeGroupDeleteNodes / NodeGroupDecreaseTargetSize) and come back
// once the lock is free; treating it as a terminal failure is what strands a slot.
//
// The conflict is reported by AcquireLock only. The lock itself is never weakened by it: a
// refused operation takes nothing, writes nothing, and leaves the holder's lock in place,
// so scale-up and scale-down of one slot stay mutually exclusive.
type LockConflictError struct {
	// Operation is the operation that was refused (OperationScaleUp / OperationScaleDown).
	Operation string
	// Holder is the lock that is in the way, as it was read from the slot.
	Holder *OperationLock
	// Reason distinguishes the checks: the pre-check on the object in hand, the re-check
	// under optimistic concurrency, and a lock that moved between retries.
	Reason string
}

func (e *LockConflictError) Error() string {
	if e.Holder == nil {
		return fmt.Sprintf("coordination lock %s: operation=%s, owner=unknown, age=unknown",
			e.Reason, e.Operation)
	}

	return fmt.Sprintf("coordination lock %s: operation=%s, owner=%s, age=%v",
		e.Reason, e.Holder.Operation, e.Holder.Owner, time.Since(e.Holder.Timestamp))
}

// RetryAfter is how long the caller should wait before asking again: the remaining lifetime
// of the lock that holds the slot, clamped into [MinLockRetryDelay, MaxLockRetryDelay].
// Coming back earlier is a guaranteed conflict, coming back later than the timeout is
// pointless because an expired lock is taken over on the next acquisition.
func (e *LockConflictError) RetryAfter() time.Duration {
	if e.Holder == nil {
		return MinLockRetryDelay
	}

	remaining := e.Holder.Timeout - time.Since(e.Holder.Timestamp)
	if remaining < MinLockRetryDelay {
		return MinLockRetryDelay
	}
	if remaining > MaxLockRetryDelay {
		return MaxLockRetryDelay
	}

	return remaining
}

// IsLockConflict reports whether err is, or wraps, a LockConflictError: a transition that
// lost the coordination lock and has to be retried once the operation holding it is done.
func IsLockConflict(err error) bool {
	return AsLockConflict(err) != nil
}

// AsLockConflict returns the LockConflictError err is, or wraps, and nil for anything else.
// The caller that defers a transition needs the remaining lifetime of the lock to schedule
// the retry, which is what the typed error carries.
func AsLockConflict(err error) *LockConflictError {
	var conflict *LockConflictError
	if !errors.As(err, &conflict) {
		return nil
	}

	return conflict
}

// HolderOperation, HolderOwner and HolderAge describe the lock in the way, nil safely, for
// logs and events. They say which operation owns the slot, which is the half of a conflict an
// operator needs to follow: scale-up means the slot is booting, and the caller that defers the
// transition schedules the retry from HolderAge.
func (e *LockConflictError) HolderOperation() string {
	if e.Holder == nil {
		return unknownLockValue
	}

	return e.Holder.Operation
}

func (e *LockConflictError) HolderOwner() string {
	if e.Holder == nil {
		return unknownLockValue
	}

	return e.Holder.Owner
}

func (e *LockConflictError) HolderAge() time.Duration {
	if e.Holder == nil {
		return 0
	}

	return time.Since(e.Holder.Timestamp)
}

// newLockConflict builds the conflict error for a lock read off a slot.
func newLockConflict(operation string, holder *OperationLock, reason string) *LockConflictError {
	return &LockConflictError{Operation: operation, Holder: holder, Reason: reason}
}

// DefaultCoordinationManager implements the CoordinationManager interface
type DefaultCoordinationManager struct {
	client client.Client
	owner  string
}

// NewCoordinationManager creates a new coordination manager
func NewCoordinationManager(k8sClient client.Client, owner string) CoordinationManager {
	return &DefaultCoordinationManager{
		client: k8sClient,
		owner:  owner,
	}
}

// AcquireLock attempts to acquire an operation lock on a node using atomic operations
func (cm *DefaultCoordinationManager) AcquireLock(ctx context.Context, node *infrav1alpha1.Node, operation string, timeout time.Duration) error {
	logger := log.Log.WithName("coordination")

	if timeout == 0 {
		timeout = DefaultLockTimeout
	}

	// Check if lock already exists and is not expired
	if existingLock, exists := cm.CheckLock(ctx, node); exists {
		if !cm.IsLockExpired(existingLock) {
			return newLockConflict(operation, existingLock, lockReasonAlreadyExists)
		}
		logger.Info("Existing lock expired, proceeding with acquisition",
			"node", node.Name,
			"expiredOperation", existingLock.Operation,
			"expiredOwner", existingLock.Owner,
			"age", time.Since(existingLock.Timestamp))
	}

	// Use optimistic locking with retry to acquire the lock
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := cm.client.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
			return fmt.Errorf("failed to get fresh node: %w", err)
		}

		// Double-check lock doesn't exist on fresh copy
		if existingLock, exists := cm.CheckLock(ctx, fresh); exists {
			if !cm.IsLockExpired(existingLock) {
				return newLockConflict(operation, existingLock, lockReasonStolenByOther)
			}
		}

		// Initialize annotations if needed
		if fresh.Annotations == nil {
			fresh.Annotations = make(map[string]string)
		}

		// Set lock annotations
		fresh.Annotations[OperationLockAnnotation] = operation
		fresh.Annotations[LockOwnerAnnotation] = cm.owner
		fresh.Annotations[LockTimestampAnnotation] = time.Now().Format(time.RFC3339)
		fresh.Annotations[LockTimeoutAnnotation] = strconv.FormatFloat(timeout.Seconds(), 'f', 0, 64)

		// Attempt to update with optimistic locking
		if err := cm.client.Update(ctx, fresh); err != nil {
			if apierrors.IsConflict(err) {
				// The slot moved under this retry, which includes another operation taking
				// the lock: report it as a conflict, so the caller defers and comes back on
				// the lock's own schedule instead of treating the slot as broken.
				return newLockConflict(operation, cm.lockOn(fresh), lockReasonChangedOnRetry)
			}

			return err
		}

		logger.Info("Successfully acquired coordination lock",
			"node", node.Name,
			"operation", operation,
			"owner", cm.owner,
			"timeout", timeout)

		return nil
	})
}

// lockOn reports the live lock a slot carries, if any. It is the read behind a conflict
// reported from the update path, where the lock is discovered by losing the write.
func (cm *DefaultCoordinationManager) lockOn(node *infrav1alpha1.Node) *OperationLock {
	lock, exists := cm.CheckLock(context.TODO(), node)
	if !exists || cm.IsLockExpired(lock) {
		return nil
	}

	return lock
}

// ReleaseLock releases an operation lock from a node
func (cm *DefaultCoordinationManager) ReleaseLock(ctx context.Context, node *infrav1alpha1.Node) error {
	logger := log.Log.WithName("coordination")

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := cm.client.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
			return fmt.Errorf("failed to get fresh node: %w", err)
		}

		if fresh.Annotations == nil {
			logger.V(1).Info("No annotations to clean up", "node", node.Name)
			return nil
		}

		// Check if we own the lock before releasing
		currentOwner := fresh.Annotations[LockOwnerAnnotation]
		if currentOwner != cm.owner {
			if currentOwner == "" {
				logger.V(1).Info("No lock to release", "node", node.Name)
				return nil
			}
			logger.Info("Cannot release lock owned by different component",
				"node", node.Name,
				"currentOwner", currentOwner,
				"expectedOwner", cm.owner)
			return fmt.Errorf("cannot release lock owned by %s", currentOwner)
		}

		// Remove coordination annotations
		delete(fresh.Annotations, OperationLockAnnotation)
		delete(fresh.Annotations, LockOwnerAnnotation)
		delete(fresh.Annotations, LockTimestampAnnotation)
		delete(fresh.Annotations, LockTimeoutAnnotation)

		// Update the node
		if err := cm.client.Update(ctx, fresh); err != nil {
			return err
		}

		logger.Info("Successfully released coordination lock",
			"node", node.Name,
			"owner", cm.owner)

		return nil
	})
}

// CheckLock checks if a node has an active operation lock and returns the lock details
func (cm *DefaultCoordinationManager) CheckLock(ctx context.Context, node *infrav1alpha1.Node) (*OperationLock, bool) {
	logger := log.Log.WithName("coordination")

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

// IsLockExpired checks if an existing lock has expired based on timestamp and timeout
func (cm *DefaultCoordinationManager) IsLockExpired(lock *OperationLock) bool {
	if lock == nil {
		return true
	}

	age := time.Since(lock.Timestamp)
	return age > lock.Timeout
}

// ForceReleaseLock forcibly removes lock annotations from a node, regardless of ownership
func (cm *DefaultCoordinationManager) ForceReleaseLock(ctx context.Context, node *infrav1alpha1.Node) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav1alpha1.Node{}
		if err := cm.client.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
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
		return cm.client.Update(ctx, fresh)
	})
}
