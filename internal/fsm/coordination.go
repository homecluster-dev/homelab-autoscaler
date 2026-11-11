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

// unknownField is what a note reports for a value this path could not read - the holder of a
// lock that moved under a retry, the group label of a slot, the lifecycle Job of a completion
// whose event carried none - so a message an operator reads keeps its shape instead of printing
// an empty field. One constant, because a note that spells the same absence three ways is three
// things to grep for.
const unknownField = "unknown"

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

// Deferral renders the refusal for an operator: who owns the slot, and when this operation
// comes back. It is the message of the Scale*Deferred event, so it says in one line that
// nothing is wrong with the slot and that the request is not lost.
func (e *LockConflictError) Deferral() string {
	return fmt.Sprintf("%s, the opposite operation owns the slot, retrying in %v", e.Error(), e.RetryAfter())
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
		return unknownField
	}

	return e.Holder.Operation
}

func (e *LockConflictError) HolderOwner() string {
	if e.Holder == nil {
		return unknownField
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
		stampLock(fresh, cm.owner, operation, time.Now(), timeout)

		// Attempt to update with optimistic locking
		if err := cm.client.Update(ctx, fresh); err != nil {
			if !apierrors.IsConflict(err) {
				return err
			}

			// The write lost a race. Decide from what the API server holds, never from
			// `fresh`: this attempt has already stamped our own lock annotations onto that
			// copy, so a lock read off it is this acquisition reported as the holder, aged
			// 0s. The retry loop stops on it too, because the error returned is no longer a
			// conflict. A benign status/spec write race - no lock in the API at all - therefore
			// cost a failure event and a defer of nearly the whole lock timeout, verified live:
			//
			//   coordination lock changed during retry: operation=scale-up,
			//   owner=node-controller, age=335ms     ->  retryAfter=4m59.66s
			//   lock really present in the API: false
			//
			// Re-reading the slot is the only way to tell the two shapes apart.
			live := &infrav1alpha1.Node{}
			if getErr := cm.client.Get(ctx, client.ObjectKeyFromObject(node), live); getErr != nil {
				// Nothing is known about the lock and the fact in hand is the lost write, so
				// return it: RetryOnConflict retries a conflict, and anything else - the slot
				// is gone, the API server is unreachable - surfaces as the error it is.
				logger.V(1).Info("Lost the lock write and cannot re-read the slot, retrying the acquisition",
					"node", node.Name, "error", getErr.Error())

				return err
			}

			if holder := cm.lockOn(live); holder != nil {
				// The racer took the lock: report the real holder, so the caller defers and
				// comes back on the deadline that lock actually has left.
				return newLockConflict(operation, holder, lockReasonChangedOnRetry)
			}

			// No lock in the API. The slot moved under this attempt - a status write from
			// the core controller, a resync, a spec write from the provider - which is all a
			// resourceVersion conflict says. Return the original conflict and let
			// RetryOnConflict run the acquisition again against the new object.
			//
			// A read that goes through a cache can lag behind the API and miss a lock the
			// racer wrote milliseconds ago; the cost is one more pass of this loop, and the
			// double-check at its top reports the lock as stolen on the next one.
			logger.V(1).Info("Lost the lock write to a plain update, retrying the acquisition",
				"node", node.Name, "operation", operation)

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

// stampLock writes the coordination lock annotations of a slot. AcquireLock and RenewLock share
// it so the shape of a lock - and the expiry every reader of it computes - is written in one
// place: a re-stamp that ages a lock differently from an acquisition is a lock the next
// acquisition, the node controller's cleanup and the progress watchdog all read at odds.
func stampLock(node *infrav1alpha1.Node, owner, operation string, at time.Time, timeout time.Duration) {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	node.Annotations[OperationLockAnnotation] = operation
	node.Annotations[LockOwnerAnnotation] = owner
	node.Annotations[LockTimestampAnnotation] = at.Format(time.RFC3339)
	node.Annotations[LockTimeoutAnnotation] = strconv.FormatFloat(timeout.Seconds(), 'f', 0, 64)
}

// lockOn reports the live lock a slot carries, if any, and nil for an expired one. It is
// the read behind a conflict reported from the update path, where the lock is discovered by
// losing the write, so it takes the object re-read from the API: the copy an acquisition
// attempt stamped with its own annotations is not evidence about the lock, it is the
// acquisition.
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

// RenewLock re-stamps the timestamp of the coordination lock this manager owns on a slot, and
// reports whether it did. It is the heartbeat of a long-running operation: the acquisition writes
// a lock with DefaultLockTimeout (5m), and every reader of that lock - the next acquisition, the
// node controller's expired-lock cleanup, and the progress watchdog's lock gate - treats an older
// one as free. An operation that holds a slot for longer than the timeout (the scale-up readiness
// gate alone is bounded by 90s of registration plus 4m of gate) therefore loses the protection it
// acquired, mid-wait, and the slot is open to the writers the lock exists to keep out.
//
// The rules, all of them refusals to take anything:
//   - no lock on the slot: nothing to renew (returns false, no write). Renewal never acquires -
//     acquiring is the before_ hook's job and it is the operation that owns the slot that renews;
//   - a lock owned by someone else (cluster-autoscaler's provider, another controller): untouched,
//     renewing a lock you do not own is claiming it;
//   - an expired lock: not renewed. An expired lock names an operation that is over; re-stamping it
//     would resurrect a lock nothing is running under, which is the shape the watchdog, the
//     takeover path and cleanupExpiredLocks all exist to heal.
//
// The operation and the timeout of the holder are kept verbatim: only the timestamp moves, so the
// lock an operator sees in the annotations still names the operation that owns the slot.
func (cm *DefaultCoordinationManager) RenewLock(ctx context.Context, node *infrav1alpha1.Node) (bool, error) {
	logger := log.Log.WithName("coordination")

	renewed := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infrav1alpha1.Node{}
		if err := cm.client.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
			return fmt.Errorf("failed to get fresh node: %w", err)
		}

		lock, exists := cm.CheckLock(ctx, fresh)
		if !exists {
			logger.V(1).Info("No coordination lock to renew",
				"node", node.Name, "owner", cm.owner)

			return nil
		}

		if lock.Owner != cm.owner {
			logger.V(1).Info("Not renewing a coordination lock this component does not own",
				"node", node.Name,
				"owner", cm.owner,
				"lockOwner", lock.Owner,
				"operation", lock.Operation)

			return nil
		}

		if cm.IsLockExpired(lock) {
			logger.Info("Not renewing an expired coordination lock, the operation that wrote it is over",
				"node", node.Name,
				"operation", lock.Operation,
				"owner", cm.owner,
				"age", time.Since(lock.Timestamp),
				"timeout", lock.Timeout)

			return nil
		}

		stampLock(fresh, cm.owner, lock.Operation, time.Now(), lock.Timeout)

		if err := cm.client.Update(ctx, fresh); err != nil {
			return err
		}

		renewed = true

		logger.V(1).Info("Re-stamped the coordination lock of a long-running operation",
			"node", node.Name,
			"operation", lock.Operation,
			"owner", cm.owner,
			"timeout", lock.Timeout)

		return nil
	})
	if err != nil {
		return false, err
	}

	return renewed, nil
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
