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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/homecluster-dev/homelab-autoscaler/internal/config"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

// drainEvents returns every event the fake recorder has accepted. The recorder is a
// buffered channel, so the tests must size it above the number of events they expect:
// a full channel blocks the writer, it does not drop.
func drainEvents(rec *events.FakeRecorder) []string {
	var recorded []string
	for {
		select {
		case event := <-rec.Events:
			recorded = append(recorded, event)
		default:
			return recorded
		}
	}
}

// findEvents returns the recorded events carrying the "<Type> <Reason>" prefix that
// `kubectl describe` prints, e.g. "Normal ScaleUpRequested".
func findEvents(recorded []string, typeAndReason string) []string {
	var found []string
	for _, event := range recorded {
		if strings.HasPrefix(event, typeAndReason+" ") {
			found = append(found, event)
		}
	}
	return found
}

// assertEventCount asserts the exact number of events recorded for a transition, which is
// how these tests prove there is no event storm: one event per transition, none per resync.
func assertEventCount(t *testing.T, recorded []string, typeAndReason string, want int) []string {
	t.Helper()

	found := findEvents(recorded, typeAndReason)
	require.Len(t, found, want, "expected %d %q events, got: %v", want, typeAndReason, recorded)

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

func newRecordingStateMachine(t *testing.T, node *infrav2alpha1.Node, mockCoord *MockCoordinationManager, objects ...client.Object) (*NodeStateMachine, *events.FakeRecorder) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	fakeClient := CreateFakeClient(scheme, append([]client.Object{node}, objects...)...)

	rec := events.NewFakeRecorder(20)
	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord, WithEventRecorder(rec))
	nm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	return nm, rec
}

func TestNodeStateMachine_Events_ScaleUp(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	mockCoord := NewMockCoordinationManager()
	nm, rec := newRecordingStateMachine(t, node, mockCoord)

	require.NoError(t, nm.StartNode())
	require.NoError(t, nm.JobCompleted(nil))
	assert.Equal(t, StateReady, nm.GetCurrentState())

	// Request on the shutdown -> startingup transition, completion when the startup Job
	// finishes: exactly one event per transition.
	recorded := drainEvents(rec)

	requested := assertEventCount(t, recorded, "Normal "+ReasonScaleUpRequested, 1)
	assert.Contains(t, requested[0], "test-node")
	assert.Contains(t, requested[0], "group test-group")
	assert.Contains(t, requested[0], "kubernetes node test-node-k8s")

	completed := assertEventCount(t, recorded, "Normal "+ReasonScaleUpCompleted, 1)
	assert.Contains(t, completed[0], "startup job finished")
	assert.Contains(t, completed[0], "slot is ready")

	assertEventCount(t, recorded, "Warning "+ReasonScaleUpFailed, 0)
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownRequested, 0)
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownCompleted, 0)

	// This fixture has no corev1.Node for the scale-up to gate on, so the registration deadline
	// expires and the loop records its timeout: the operator-visible half of "the machine never
	// registered". That is the third event and the last one: two lifecycle events, one gate event, and
	// no repeat.
	gate := assertEventCount(t, recorded, "Warning "+ReasonReadinessGateTimeout, 1)
	assert.Contains(t, gate[0], "has not appeared", "the event names the gate that ran out")

	assert.True(t, strings.HasPrefix(recorded[0], "Normal "+ReasonScaleUpRequested),
		"the request is recorded before the completion: %v", recorded)
	assert.Len(t, recorded, 3, "one event per transition, one for the gate, no storm: %v", recorded)
}

func TestNodeStateMachine_Events_ScaleDown(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	kubeNode := CreateTestKubernetesNode("test-node-k8s", false)
	mockCoord := NewMockCoordinationManager()
	nm, rec := newRecordingStateMachine(t, node, mockCoord, kubeNode)

	require.NoError(t, nm.ShutdownNode())
	require.NoError(t, nm.JobCompleted(nil))
	assert.Equal(t, StateShutdown, nm.GetCurrentState())

	recorded := drainEvents(rec)

	requested := assertEventCount(t, recorded, "Normal "+ReasonScaleDownRequested, 1)
	assert.Contains(t, requested[0], "test-node")
	assert.Contains(t, requested[0], "group test-group")
	assert.Contains(t, requested[0], "kubernetes node test-node-k8s")

	completed := assertEventCount(t, recorded, "Normal "+ReasonScaleDownCompleted, 1)
	assert.Contains(t, completed[0], "shutdown job finished")
	assert.Contains(t, completed[0], "slot is shut down")

	assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 0)
	assertEventCount(t, recorded, "Normal "+ReasonScaleUpRequested, 0)
	assert.Len(t, recorded, 2, "one event per transition, no storm: %v", recorded)
}

func TestNodeStateMachine_Events_ScaleUpFailure(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	mockCoord := NewMockCoordinationManager()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	// The API server refuses the startup Job; the FSM rolls back startingup -> shutdown.
	jobErr := errors.New("admission webhook denied the request: no free power slot")
	fakeClient := &createJobFailingClient{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(node).
			WithStatusSubresource(&infrav2alpha1.Node{}).
			Build(),
		err: jobErr,
	}

	rec := events.NewFakeRecorder(20)
	nm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord, WithEventRecorder(rec))
	nm.uncordonTimeout = 100 * time.Millisecond
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	err := nm.StartNode()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create startup job")

	recorded := drainEvents(rec)

	// The request happened, the failure is reported with the underlying error text...
	assertEventCount(t, recorded, "Normal "+ReasonScaleUpRequested, 1)
	failed := assertEventCount(t, recorded, "Warning "+ReasonScaleUpFailed, 1)
	assert.Contains(t, failed[0], "failed to create startup job")
	assert.Contains(t, failed[0], "no free power slot")
	assert.Contains(t, failed[0], "test-node")

	// ...and the rollback out of startingup is NOT reported as a completed scale down.
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownCompleted, 0)
	assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 0)
	assert.Len(t, recorded, 2, "a failed scale up records exactly one warning: %v", recorded)
}

func TestNodeStateMachine_Events_ScaleDownFailure(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	mockCoord := NewMockCoordinationManager()
	mockCoord.SetAcquireError(errors.New("cluster autoscaler holds the scale-down lock"))
	nm, rec := newRecordingStateMachine(t, node, mockCoord)

	err := nm.ShutdownNode()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FSM transition failed")

	recorded := drainEvents(rec)

	// The transition never happened, so no request event - only the failure, as a Warning.
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownRequested, 0)
	failed := assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 1)
	assert.Contains(t, failed[0], "FSM transition failed")
	assert.Contains(t, failed[0], "cluster autoscaler holds the scale-down lock")
	assert.Len(t, recorded, 1, "%v", recorded)
}

// TestNodeStateMachine_Events_LockConflictIsADeferral: a transition that lost the coordination
// lock is the lock doing its job, not a lifecycle operation that died. It records one Normal
// ScaleDownDeferred naming the holder and the deadline, and no Warning. The live trace this
// branch starts from opens with a Warning for exactly this case, on a slot that is fine, and
// the autoscaler emits one per collision on a busy pool.
func TestNodeStateMachine_Events_LockConflictIsADeferral(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	mockCoord := NewMockCoordinationManager()
	mockCoord.SetAcquireError(newLockConflict(OperationScaleDown, &OperationLock{
		Operation: OperationScaleUp,
		Owner:     NodeControllerOwner,
		Timestamp: time.Now().Add(-4 * time.Minute),
		Timeout:   DefaultLockTimeout,
	}, lockReasonAlreadyExists))

	nm, rec := newRecordingStateMachine(t, node, mockCoord)

	err := nm.ShutdownNode()
	require.Error(t, err)
	require.True(t, IsLockConflict(err))

	recorded := drainEvents(rec)
	deferred := assertEventCount(t, recorded, "Normal "+ReasonScaleDownDeferred, 1)
	assert.Contains(t, deferred[0], "coordination lock already exists")
	assert.Contains(t, deferred[0], "operation=scale-up")
	assert.Contains(t, deferred[0], "owner=node-controller")
	assert.Contains(t, deferred[0], "retrying in", "the event says when the operation comes back")

	assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 0)
	assertEventCount(t, recorded, "Normal "+ReasonScaleDownRequested, 0)
	assert.Len(t, recorded, 1, "a deferral is one Normal event, not a Warning: %v", recorded)
}

func TestNodeStateMachine_Events_ObservedJobFailure(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)

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

	recorded := drainEvents(rec)
	failed := assertEventCount(t, recorded, "Warning "+ReasonScaleUpFailed, 1)
	assert.Contains(t, failed[0], "BackoffLimitExceeded")
	assert.Contains(t, failed[0], failedJob.Name)

	assertEventCount(t, recorded, "Normal "+ReasonScaleDownCompleted, 0)
	assert.Len(t, recorded, 1, "a failed startup records exactly one warning: %v", recorded)
}

func TestNodeStateMachine_Events_ForceCleanupIsAFailure(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
	mockCoord := NewMockCoordinationManager()
	nm, rec := newRecordingStateMachine(t, node, mockCoord)

	require.NoError(t, nm.ForceCleanup())

	recorded := drainEvents(rec)
	failed := assertEventCount(t, recorded, "Warning "+ReasonScaleUpFailed, 1)
	assert.Contains(t, failed[0], "forced cleanup")
	assert.Contains(t, failed[0], StateStartingUp)
	assert.Contains(t, failed[0], StateShutdown)

	assertEventCount(t, recorded, "Normal "+ReasonScaleDownCompleted, 0)
	assert.Len(t, recorded, 1, "%v", recorded)
}

func TestNodeStateMachine_Events_NilRecorderIsSilent(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	mockCoord := NewMockCoordinationManager()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))
	fakeClient := CreateFakeClient(scheme, node)

	// No option at all, and an explicit nil recorder: both must be safe no-ops.
	for name, nm := range map[string]*NodeStateMachine{
		"no option":           NewNodeStateMachine(node, fakeClient, scheme, mockCoord),
		"nil recorder option": NewNodeStateMachine(node, fakeClient, scheme, mockCoord, WithEventRecorder(nil)),
	} {
		t.Run(name, func(t *testing.T) {
			nm.uncordonTimeout = 100 * time.Millisecond
			nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

			require.NoError(t, nm.StartNode())
			require.NoError(t, nm.JobCompleted(nil))
			assert.Equal(t, StateReady, nm.GetCurrentState())
		})
	}
}

// capturedEvent is one full Eventf call. The client-go fake only hands back the rendered
// "<type> <reason> <note>" string, so the arguments the new events API added - regarding,
// related, action - need a recorder that keeps the calls themselves.
type capturedEvent struct {
	regarding runtime.Object
	related   runtime.Object
	eventType string
	reason    string
	action    string
	note      string
	args      []interface{}
}

// message renders the note the way the real recorder does - fmt.Sprintf(note, args...) -
// so a note that was used as a format string shows up mangled here.
func (e capturedEvent) message() string {
	return fmt.Sprintf(e.note, e.args...)
}

// capturingRecorder is a test double for k8s.io/client-go/tools/events.EventRecorder.
type capturingRecorder struct {
	calls []capturedEvent
}

var _ events.EventRecorder = &capturingRecorder{}

func (c *capturingRecorder) Eventf(regarding runtime.Object, related runtime.Object,
	eventType, reason, action, note string, args ...interface{}) {
	c.calls = append(c.calls, capturedEvent{
		regarding: regarding,
		related:   related,
		eventType: eventType,
		reason:    reason,
		action:    action,
		note:      note,
		args:      args,
	})
}

// reasonCalls returns every captured call recorded for a reason.
func (c *capturingRecorder) reasonCalls(reason string) []capturedEvent {
	var found []capturedEvent
	for _, call := range c.calls {
		if call.reason == reason {
			found = append(found, call)
		}
	}
	return found
}

func newCapturingStateMachine(t *testing.T, node *infrav2alpha1.Node, mockCoord *MockCoordinationManager, objects ...client.Object) (*NodeStateMachine, *capturingRecorder) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	rec := &capturingRecorder{}
	nm := NewNodeStateMachine(node, CreateFakeClient(scheme, append([]client.Object{node}, objects...)...),
		scheme, mockCoord, WithEventRecorder(rec))
	nm.uncordonTimeout = 100 * time.Millisecond
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	return nm, rec
}

// TestNodeStateMachine_Events_NewAPICallShape pins the arguments of the new events API:
// the events.FakeRecorder renders them into one string, so this is what proves the
// migration is wiring the right objects and verbs, not just the right text.
func TestNodeStateMachine_Events_NewAPICallShape(t *testing.T) {
	t.Run("scale down is about the Node CR with the PowerOff action", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		kubeNode := CreateTestKubernetesNode("test-node-k8s", false)
		nm, rec := newCapturingStateMachine(t, node, NewMockCoordinationManager(), kubeNode)

		require.NoError(t, nm.ShutdownNode())
		require.NoError(t, nm.JobCompleted(nil))

		require.Len(t, rec.calls, 2, "one event per transition: %v", rec.calls)

		requested := rec.reasonCalls(ReasonScaleDownRequested)
		require.Len(t, requested, 1)

		// regarding is the Node CR, related stays nil: the lifecycle transition is about
		// the slot, the Job it ran is named in the note.
		regarding, ok := requested[0].regarding.(*infrav2alpha1.Node)
		require.True(t, ok, "regarding must be the Node CR, got %T", requested[0].regarding)
		assert.Equal(t, "test-node", regarding.Name)
		assert.Nil(t, requested[0].related)

		assert.Equal(t, corev1.EventTypeNormal, requested[0].eventType)
		assert.Equal(t, ActionPowerOff, requested[0].action)
		assert.Contains(t, requested[0].message(), "Scale down requested for node test-node")

		completed := rec.reasonCalls(ReasonScaleDownCompleted)
		require.Len(t, completed, 1)
		assert.Equal(t, ActionPowerOff, completed[0].action)
	})

	t.Run("scale up and its failure use the PowerOn action", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		nm, rec := newCapturingStateMachine(t, node, NewMockCoordinationManager())

		require.NoError(t, nm.StartNode())
		require.NoError(t, nm.JobCompleted(nil))

		requested := rec.reasonCalls(ReasonScaleUpRequested)
		require.Len(t, requested, 1)
		assert.Equal(t, ActionPowerOn, requested[0].action)
		completed := rec.reasonCalls(ReasonScaleUpCompleted)
		require.Len(t, completed, 1)
		assert.Equal(t, ActionPowerOn, completed[0].action)

		// A failed scale up is a Warning that names the operation that died, so it keeps
		// the PowerOn action even though the slot rolled back to shutdown.
		failedNode := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		failRec := &capturingRecorder{}

		scheme := runtime.NewScheme()
		require.NoError(t, clientgoscheme.AddToScheme(scheme))
		require.NoError(t, infrav2alpha1.AddToScheme(scheme))

		failing := &createJobFailingClient{
			Client: CreateFakeClient(scheme, failedNode),
			err:    errors.New("admission webhook denied the request: no free power slot"),
		}
		failNM := NewNodeStateMachine(failedNode, failing, scheme, NewMockCoordinationManager(), WithEventRecorder(failRec))
		failNM.uncordonTimeout = 100 * time.Millisecond
		failNM.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		require.Error(t, failNM.StartNode())

		failed := failRec.reasonCalls(ReasonScaleUpFailed)
		require.Len(t, failed, 1)
		assert.Equal(t, corev1.EventTypeWarning, failed[0].eventType)
		assert.Equal(t, ActionPowerOn, failed[0].action)
		assert.Contains(t, failed[0].message(), "no free power slot")
	})
}

// TestNodeStateMachine_Events_NoteIsNotAFormatString pins the escaping choice: the new API
// renders the note with Sprintf, so the already-rendered message must arrive as an
// argument to "%s". A webhook denial quoting usage carries a literal '%'.
func TestNodeStateMachine_Events_NoteIsNotAFormatString(t *testing.T) {
	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
	mockCoord := NewMockCoordinationManager()
	mockCoord.SetAcquireError(errors.New("admission webhook denied the request: 90% of pods must stay evictable"))
	nm, rec := newRecordingStateMachine(t, node, mockCoord)

	require.Error(t, nm.ShutdownNode())

	recorded := drainEvents(rec)
	failed := assertEventCount(t, recorded, "Warning "+ReasonScaleDownFailed, 1)
	assert.Contains(t, failed[0], "90% of pods must stay evictable", "the message is passed as an argument, never as the format string")
	assert.NotContains(t, failed[0], "%!", "no Sprintf error verbs leak into the note")
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
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("unnamed kubernetes node is called out", func(t *testing.T) {
		node := CreateTestNode("slot-3", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		node.Spec.KubernetesNodeName = ""

		nm := NewNodeStateMachine(node, CreateFakeClient(scheme, node), scheme, NewMockCoordinationManager())

		assert.Equal(t, `node slot-3 (group test-group, no kubernetes node)`, nm.lifecycleTarget())
	})

	t.Run("missing group label", func(t *testing.T) {
		node := CreateTestNode("slot-4", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		delete(node.Labels, "group")

		nm := NewNodeStateMachine(node, CreateFakeClient(scheme, node), scheme, NewMockCoordinationManager())

		assert.Equal(t, `node slot-4 (group unknown, kubernetes node slot-4-k8s)`, nm.lifecycleTarget())
	})
}
