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
	"strconv"
	"time"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// Coordination annotation keys for preventing race conditions with cluster autoscaler
const (
	OperationLockAnnotation = "homelab-autoscaler.dev/operation-lock"
	LockOwnerAnnotation     = "homelab-autoscaler.dev/lock-owner"
	LockTimestampAnnotation = "homelab-autoscaler.dev/lock-timestamp"
	LockTimeoutAnnotation   = "homelab-autoscaler.dev/lock-timeout"
	NodeControllerOwner     = "node-controller"
)

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
			return fmt.Errorf("coordination lock already exists: operation=%s, owner=%s, age=%v",
				existingLock.Operation, existingLock.Owner, time.Since(existingLock.Timestamp))
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
				return fmt.Errorf("coordination lock acquired by another process: operation=%s, owner=%s",
					existingLock.Operation, existingLock.Owner)
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
