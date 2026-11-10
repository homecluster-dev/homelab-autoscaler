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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/homecluster-dev/homelab-autoscaler/internal/config"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// drainEvents returns every event the fake recorder has accepted. The recorder is a
// buffered channel, so the tests must size it above the number of events they expect:
// a full channel blocks the writer, it does not drop.
func drainEvents(rec *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case event := <-rec.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

// findEvents returns the recorded events carrying the "<Type> <Reason>" prefix that
// `kubectl describe` prints, e.g. "Normal ScaleUpRequested".
func findEvents(events []string, typeAndReason string) []string {
	var found []string
	for _, event := range events {
		if strings.HasPrefix(event, typeAndReason+" ") {
			found = append(found, event)
		}
	}
	return found
}

// assertEventCount asserts the exact number of events recorded for a transition, which is
// how these tests prove there is no event storm: one event per transition, none per resync.
func assertEventCount(t *testing.T, events []string, typeAndReason string, want int) []string {
	t.Helper()

	found := findEvents(events, typeAndReason)
	require.Len(t, found, want, "expected %d %q events, got: %v", want, typeAndReason, events)

	return found
}

// createJobFailingClient fails every Create, standing in for an API server that refuses
// the startup/shutdown Job (webhook denial, quota, RBAC).
type createJobFailingClient struct {
	client.Client
	err error
}

func (c *createJobFailingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*batchv1.Job); ok {
		return c.err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func newRecordingStateMachine(t *testing.T, node *infrav1alpha1.Node, mockCoord *MockCoordinationManager, objects ...client.Object) (*NodeStateMachine, *record.FakeRecorder) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	fakeClient := CreateFakeClient(scheme, append([]client.Object{node}, objects...)...)

	rec := record.NewFakeRecorder(20)
	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord, WithEventRecorder(rec))
	nm.uncordonTimeout = 100 * time.Millisecond // Short timeout for tests

	return nm, rec
}

func TestNodeStateMachine_Events_ScaleUp(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	mockCoord := NewMockCoordinationManager()
	nm, rec := newRecordingStateMachine(t, node, mockCoord)

	require.NoError(t, nm.StartNode())
	require.NoError(t, nm.JobCompleted(nil))
	assert.Equal(t, StateReady, nm.GetCurrentState())

	// Request on the shutdown -> startingup transition, completion when the startup Job
	// finishes: exactly one event per transition.
	events := drainEvents(rec)

	requested := assertEventCount(t, events, "Normal "+ReasonScaleUpRequested, 1)
	assert.Contains(t, requested[0], "test-node")
	assert.Contains(t, requested[0], "group test-group")
	assert.Contains(t, requested[0], "kubernetes node test-node-k8s")

	completed := assertEventCount(t, events, "Normal "+ReasonScaleUpCompleted, 1)
	assert.Contains(t, completed[0], "startup job finished")
	assert.Contains(t, completed[0], "slot is ready")

	assertEventCount(t, events, "Warning "+ReasonScaleUpFailed, 0)
	assertEventCount(t, events, "Normal "+ReasonScaleDownRequested, 0)
	assertEventCount(t, events, "Normal "+ReasonScaleDownCompleted, 0)
	assert.True(t, strings.HasPrefix(events[0], "Normal "+ReasonScaleUpRequested),
		"the request is recorded before the completion: %v", events)
	assert.Len(t, events, 2, "one event per transition, no storm: %v", events)
}

func TestNodeStateMachine_Events_ScaleDown(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	kubeNode := CreateTestKubernetesNode("test-node-k8s", false)
	mockCoord := NewMockCoordinationManager()
	nm, rec := newRecordingStateMachine(t, node, mockCoord, kubeNode)

	require.NoError(t, nm.ShutdownNode())
	require.NoError(t, nm.JobCompleted(nil))
	assert.Equal(t, StateShutdown, nm.GetCurrentState())

	events := drainEvents(rec)

	requested := assertEventCount(t, events, "Normal "+ReasonScaleDownRequested, 1)
	assert.Contains(t, requested[0], "test-node")
	assert.Contains(t, requested[0], "group test-group")
	assert.Contains(t, requested[0], "kubernetes node test-node-k8s")

	completed := assertEventCount(t, events, "Normal "+ReasonScaleDownCompleted, 1)
	assert.Contains(t, completed[0], "shutdown job finished")
	assert.Contains(t, completed[0], "slot is shut down")

	assertEventCount(t, events, "Warning "+ReasonScaleDownFailed, 0)
	assertEventCount(t, events, "Normal "+ReasonScaleUpRequested, 0)
	assert.Len(t, events, 2, "one event per transition, no storm: %v", events)
}

func TestNodeStateMachine_Events_ScaleUpFailure(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	mockCoord := NewMockCoordinationManager()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	// The API server refuses the startup Job; the FSM rolls back startingup -> shutdown.
	jobErr := errors.New("admission webhook denied the request: no free power slot")
	fakeClient := &createJobFailingClient{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node).
			WithStatusSubresource(&infrav1alpha1.Node{}).
			Build(),
		err: jobErr,
	}

	rec := record.NewFakeRecorder(20)
	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord, WithEventRecorder(rec))
	nm.uncordonTimeout = 100 * time.Millisecond

	err := nm.StartNode()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create startup job")

	events := drainEvents(rec)

	// The request happened, the failure is reported with the underlying error text...
	assertEventCount(t, events, "Normal "+ReasonScaleUpRequested, 1)
	failed := assertEventCount(t, events, "Warning "+ReasonScaleUpFailed, 1)
	assert.Contains(t, failed[0], "failed to create startup job")
	assert.Contains(t, failed[0], "no free power slot")
	assert.Contains(t, failed[0], "test-node")

	// ...and the rollback out of startingup is NOT reported as a completed scale down.
	assertEventCount(t, events, "Normal "+ReasonScaleDownCompleted, 0)
	assertEventCount(t, events, "Warning "+ReasonScaleDownFailed, 0)
	assert.Len(t, events, 2, "a failed scale up records exactly one warning: %v", events)
}

func TestNodeStateMachine_Events_ScaleDownFailure(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady)
	mockCoord := NewMockCoordinationManager()
	mockCoord.SetAcquireError(errors.New("cluster autoscaler holds the scale-down lock"))
	nm, rec := newRecordingStateMachine(t, node, mockCoord)

	err := nm.ShutdownNode()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FSM transition failed")

	events := drainEvents(rec)

	// The transition never happened, so no request event - only the failure, as a Warning.
	assertEventCount(t, events, "Normal "+ReasonScaleDownRequested, 0)
	failed := assertEventCount(t, events, "Warning "+ReasonScaleDownFailed, 1)
	assert.Contains(t, failed[0], "FSM transition failed")
	assert.Contains(t, failed[0], "cluster autoscaler holds the scale-down lock")
	assert.Len(t, events, 1, "%v", events)
}

func TestNodeStateMachine_Events_ObservedJobFailure(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)

	failedJob := CreateTestJob("test-node-startup-1", node.Namespace, PhaseStartup)
	failedJob.Status.Failed = 1
	failedJob.Status.Conditions = []batchv1.JobCondition{{
		Type:    batchv1.JobFailed,
		Status:  corev1.ConditionTrue,
		Reason:  "BackoffLimitExceeded",
		Message: "Job has reached the specified backoff limit",
	}}

	mockCoord := NewMockCoordinationManager()
	nm, rec := newRecordingStateMachine(t, node, mockCoord, failedJob)

	// The monitor sees the Job fail, records the warning, then rolls the FSM back.
	nm.monitorJobCompletion(failedJob.Name)
	assert.Equal(t, StateShutdown, nm.GetCurrentState())

	events := drainEvents(rec)
	failed := assertEventCount(t, events, "Warning "+ReasonScaleUpFailed, 1)
	assert.Contains(t, failed[0], "BackoffLimitExceeded")
	assert.Contains(t, failed[0], failedJob.Name)

	assertEventCount(t, events, "Normal "+ReasonScaleDownCompleted, 0)
	assert.Len(t, events, 1, "a failed startup records exactly one warning: %v", events)
}

func TestNodeStateMachine_Events_ForceCleanupIsAFailure(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp)
	mockCoord := NewMockCoordinationManager()
	nm, rec := newRecordingStateMachine(t, node, mockCoord)

	require.NoError(t, nm.ForceCleanup())

	events := drainEvents(rec)
	failed := assertEventCount(t, events, "Warning "+ReasonScaleUpFailed, 1)
	assert.Contains(t, failed[0], "forced cleanup")
	assert.Contains(t, failed[0], StateStartingUp)
	assert.Contains(t, failed[0], StateShutdown)

	assertEventCount(t, events, "Normal "+ReasonScaleDownCompleted, 0)
	assert.Len(t, events, 1, "%v", events)
}

func TestNodeStateMachine_Events_NilRecorderIsSilent(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
	mockCoord := NewMockCoordinationManager()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))
	fakeClient := CreateFakeClient(scheme, node)

	// No option at all, and an explicit nil recorder: both must be safe no-ops.
	for name, nm := range map[string]*NodeStateMachine{
		"no option":           NewNodeStateMachine(node, fakeClient, scheme, mockCoord),
		"nil recorder option": NewNodeStateMachine(node, fakeClient, scheme, mockCoord, WithEventRecorder(nil)),
	} {
		t.Run(name, func(t *testing.T) {
			nm.uncordonTimeout = 100 * time.Millisecond

			require.NoError(t, nm.StartNode())
			require.NoError(t, nm.JobCompleted(nil))
			assert.Equal(t, StateReady, nm.GetCurrentState())
		})
	}
}

func TestTruncateEventMessage(t *testing.T) {
	long := "startup job failed\n" + strings.Repeat("boom ", 300)

	got := truncateEventMessage(long)

	assert.NotContains(t, got, "\n", "event messages stay on one line for kubectl describe")
	assert.LessOrEqual(t, len([]rune(got)), maxEventMessageLength)
	assert.True(t, strings.HasSuffix(got, truncatedSuffix), "truncation is visible: %q", got)

	assert.Equal(t, "one line message", truncateEventMessage("one line\tmessage\n"))
	assert.Equal(t, "short", truncateEventMessage("short"))
}

func TestLifecycleTarget(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	t.Run("unnamed kubernetes node is called out", func(t *testing.T) {
		node := CreateTestNode("slot-3", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		node.Spec.KubernetesNodeName = ""

		nm := NewNodeStateMachine(node, CreateFakeClient(scheme, node), scheme, NewMockCoordinationManager())

		assert.Equal(t, `node slot-3 (group test-group, no kubernetes node)`, nm.lifecycleTarget())
	})

	t.Run("missing group label", func(t *testing.T) {
		node := CreateTestNode("slot-4", config.NewNamespaceConfig().Get(), infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown)
		delete(node.Labels, "group")

		nm := NewNodeStateMachine(node, CreateFakeClient(scheme, node), scheme, NewMockCoordinationManager())

		assert.Equal(t, `node slot-4 (group unknown, kubernetes node slot-4-k8s)`, nm.lifecycleTarget())
	})
}
