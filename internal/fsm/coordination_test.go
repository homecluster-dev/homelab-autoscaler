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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
)

// lockScheme is every type a coordination test reads: the Node CR and the corev1.Node the
// coordination annotations live on.
func lockScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	return scheme
}

// lockedSlot is a slot carrying a coordination lock written the way AcquireLock writes one, aged by
// age, with the 5m timeout a scale-up takes.
func lockedSlot(operation, owner string, age time.Duration) *infrav1alpha1.Node {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp)
	node.Annotations = map[string]string{
		OperationLockAnnotation: operation,
		LockOwnerAnnotation:     owner,
		LockTimestampAnnotation: time.Now().Add(-age).Format(time.RFC3339),
		LockTimeoutAnnotation:   "300",
	}

	return node
}

// readSlot reads the slot back, so the assertions are about what is in the API and not about the
// copy a test holds.
func readSlot(t *testing.T, c client.Client) *infrav1alpha1.Node {
	t.Helper()

	node := &infrav1alpha1.Node{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "test-node", Namespace: config.NewNamespaceConfig().Get()}, node))

	return node
}

// TestRenewLockKeepsOurOwnLiveLock is the half of the API the readiness gate depends on: the
// timestamp moves, and nothing else about the lock does. The operation the lock names is what an
// operator reads and what the takeover path compares, and the timeout is what every reader of the
// lock computes its expiry from, so a re-stamp that rewrote either would be a different lock, not a
// younger one.
func TestRenewLockKeepsOurOwnLiveLock(t *testing.T) {
	slot := lockedSlot(OperationScaleUp, NodeControllerOwner, 4*time.Minute)
	c := CreateFakeClient(lockScheme(t), slot)

	coord := NewCoordinationManager(c, NodeControllerOwner)

	before := readSlot(t, c)
	renewed, err := coord.RenewLock(context.Background(), slot)
	require.NoError(t, err)
	require.True(t, renewed, "a live lock this component owns is renewed")

	after := readSlot(t, c)
	assert.NotEqual(t, before.ResourceVersion, after.ResourceVersion, "the stamp is a write")

	lock, exists := coord.CheckLock(context.Background(), after)
	require.True(t, exists)
	assert.Equal(t, OperationScaleUp, lock.Operation, "the lock still names the operation that owns the slot")
	assert.Equal(t, NodeControllerOwner, lock.Owner, "and its owner")
	assert.Less(t, time.Since(lock.Timestamp), time.Minute,
		"the timestamp moved: it is %v old, the slot was stamped 4m ago", time.Since(lock.Timestamp).String())
	assert.False(t, coord.IsLockExpired(lock), "so the lock is live again")
}

// TestRenewLockTakesNothingOver: renewal is a heartbeat, not an acquisition. A slot with no lock
// gets none (the operation that owns a slot acquired it in its before_ hook, and a gate loop that
// found the lock gone must not mint one), a lock owned by another component is untouched
// (cluster-autoscaler's provider writes locks of its own), and an expired lock is not resurrected -
// an expired lock names an operation that is over, and the takeover path, the node controller's
// cleanup and the progress watchdog all decide what to do from exactly that reading.
func TestRenewLockTakesNothingOver(t *testing.T) {
	tests := []struct {
		name  string
		slot  *infrav1alpha1.Node
		owner string
	}{
		{
			name:  "no lock at all",
			slot:  CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp),
			owner: NodeControllerOwner,
		},
		{
			name:  "a lock owned by someone else",
			slot:  lockedSlot(OperationScaleDown, "cluster-autoscaler", 10*time.Second),
			owner: NodeControllerOwner,
		},
		{
			name:  "an expired lock of ours",
			slot:  lockedSlot(OperationScaleUp, NodeControllerOwner, 10*time.Minute),
			owner: NodeControllerOwner,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := CreateFakeClient(lockScheme(t), tc.slot)
			coord := NewCoordinationManager(c, tc.owner)

			before := readSlot(t, c)

			renewed, err := coord.RenewLock(context.Background(), tc.slot)
			require.NoError(t, err)
			assert.False(t, renewed, "nothing was stamped")

			after := readSlot(t, c)
			assert.Equal(t, before.Annotations, after.Annotations, "the lock in the API is exactly as it was")
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "and a pass that renews nothing writes nothing")
		})
	}
}

// TestAcquireLockAndRenewLockShareTheLockShape is the reason both writers call one stamping
// helper: a lock written by an acquisition and a lock written by a re-stamp must age identically,
// because three readers compute an expiry from the timestamp and the timeout.
func TestAcquireLockAndRenewLockShareTheLockShape(t *testing.T) {
	slot := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressStartingUp)
	c := CreateFakeClient(lockScheme(t), slot)
	coord := NewCoordinationManager(c, NodeControllerOwner)

	require.NoError(t, coord.AcquireLock(context.Background(), slot, OperationScaleUp, DefaultLockTimeout))

	acquired := readSlot(t, c)

	renewed, err := coord.RenewLock(context.Background(), acquired)
	require.NoError(t, err)
	require.True(t, renewed)

	renewedSlot := readSlot(t, c)

	for _, key := range []string{OperationLockAnnotation, LockOwnerAnnotation, LockTimeoutAnnotation} {
		assert.Equal(t, acquired.Annotations[key], renewedSlot.Annotations[key],
			"%s is written by the same code on both paths", key)
	}

	_, hasTimestamp := renewedSlot.Annotations[LockTimestampAnnotation]
	require.True(t, hasTimestamp, "the timestamp annotation is the renewal")

	lock, exists := coord.CheckLock(context.Background(), renewedSlot)
	require.True(t, exists)
	assert.Equal(t, DefaultLockTimeout, lock.Timeout, "the timeout a reader computes is the acquired one")
}
