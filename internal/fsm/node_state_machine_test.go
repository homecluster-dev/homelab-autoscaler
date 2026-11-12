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
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// inputLawCases is the phase state table of the NodeClaim model, as a test: every row is
// (live claim, observed power, node usability, Jobs in flight) in, and the derived report and the
// derived state machine state out. It is the table the PR and docs/architecture/state.md carry, and
// the same rows are driven end to end in internal/controller/infra's input-law test.
//
// The four laws of docs/design/nodeclaim.md §6 are rows 1, 2, 3 and 4 (boot / ready / shut down /
// force shut down); the rest are the shapes between them.
func TestFSMInputLaw(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	const slot = "test-node"

	pending := TestClaim(slot, "claim-pending", infrav2alpha1.NodeClaimPending, false)
	booting := TestClaim(slot, "claim-booting", infrav2alpha1.NodeClaimBooting, false)
	ready := TestClaim(slot, "claim-ready", infrav2alpha1.NodeClaimReady, false)
	failed := TestClaim(slot, "claim-failed", infrav2alpha1.NodeClaimFailed, false)
	marked := TestClaim(slot, "claim-marked", infrav2alpha1.NodeClaimReady, true)

	off := infrav2alpha1.PowerStateOff
	on := infrav2alpha1.PowerStateOn

	tests := []struct {
		name          string
		in            phase.Input
		expectedPhase infrav2alpha1.NodePhase
		expectedState string
	}{
		{
			name:          "law 1: a live claim with the machine off boots it",
			in:            phase.Input{Claim: pending, ObservedPower: off},
			expectedPhase: infrav2alpha1.NodePhasePending,
			expectedState: StateShutdown,
		},
		{
			name:          "law 2: live claim, machine on, node usable is ready",
			in:            phase.Input{Claim: ready, ObservedPower: on, NodeUsable: true},
			expectedPhase: infrav2alpha1.NodePhaseReady,
			expectedState: StateReady,
		},
		{
			name:          "law 3: no live claim, machine on, shuts it down",
			in:            phase.Input{ObservedPower: on, NodeUsable: true},
			expectedPhase: infrav2alpha1.NodePhaseDraining,
			expectedState: StateReady,
		},
		{
			name:          "law 4: a failed claim with the machine on force-shuts it down",
			in:            phase.Input{Claim: failed, ObservedPower: on, NodeUsable: true},
			expectedPhase: infrav2alpha1.NodePhaseDraining,
			expectedState: StateReady,
		},
		{
			name:          "a free slot: no claim, machine off",
			in:            phase.Input{ObservedPower: off},
			expectedPhase: infrav2alpha1.NodePhaseOff,
			expectedState: StateShutdown,
		},
		{
			name:          "a startup Job in flight for a pending claim is a boot, not a cancel target",
			in:            phase.Input{Claim: pending, ObservedPower: off, StartupJobInFlight: true},
			expectedPhase: infrav2alpha1.NodePhaseBooting,
			expectedState: StateStartingUp,
		},
		{
			name:          "a booting claim with the machine off is starting up",
			in:            phase.Input{Claim: booting, ObservedPower: off},
			expectedPhase: infrav2alpha1.NodePhaseBooting,
			expectedState: StateStartingUp,
		},
		{
			name:          "a ready claim whose node cannot take the pool's work is not ready",
			in:            phase.Input{Claim: ready, ObservedPower: on, NodeUsable: false},
			expectedPhase: infrav2alpha1.NodePhaseBooting,
			expectedState: StateStartingUp,
		},
		{
			name:          "a machine that vanished under a live claim is booted again",
			in:            phase.Input{Claim: ready, ObservedPower: off},
			expectedPhase: infrav2alpha1.NodePhaseBooting,
			expectedState: StateStartingUp,
		},
		{
			name:          "a claim marked for scale-down, machine on, with its shutdown Job running",
			in:            phase.Input{Claim: marked, ObservedPower: on, NodeUsable: true, ShutdownJobInFlight: true},
			expectedPhase: infrav2alpha1.NodePhaseDraining,
			expectedState: StateShuttingDown,
		},
		{
			name:          "a claim marked for scale-down, machine on, no Job yet: ShutdownNode is the event",
			in:            phase.Input{Claim: marked, ObservedPower: on, NodeUsable: true},
			expectedPhase: infrav2alpha1.NodePhaseDraining,
			expectedState: StateReady,
		},
		{
			name:          "a claim marked for scale-down, machine off: the slot is free again",
			in:            phase.Input{Claim: marked, ObservedPower: off},
			expectedPhase: infrav2alpha1.NodePhaseOff,
			expectedState: StateShutdown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expectedPhase, phase.Derive(tt.in), "the report the model records")
			assert.Equal(t, tt.expectedState, phase.State(tt.in), "the state the machine starts in")

			// The same derivation through a built machine, so the looplab wiring is covered and not
			// only the formula.
			node := CreateTestNode(slot, config.NewNamespaceConfig().Get(), tt.in.ObservedPower, "")
			fakeClient := CreateFakeClient(scheme, node)
			machine := NewNodeStateMachine(node, fakeClient, scheme, NewMockCoordinationManager(), WithInput(tt.in))

			assert.Equal(t, tt.expectedState, machine.GetCurrentState())
		})
	}
}

func TestNewNodeStateMachine(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
	fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
	fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	assert.NotNil(t, fsm)
	assert.Equal(t, StateShutdown, fsm.GetCurrentState())
	assert.Equal(t, node, fsm.node)
	assert.Equal(t, fakeClient, fsm.client)
	assert.Equal(t, scheme, fsm.scheme)
	assert.Equal(t, mockCoord, fsm.coordinationManager)
	assert.Equal(t, DefaultJobTimeout, fsm.jobTimeout)
}

func TestNodeStateMachine_CanTransition(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	// Each case names the claim and the observation that put the machine in the state the event is
	// asked for: the state is derived, so a test cannot write it.
	const slot = "test-node"

	tests := []struct {
		name          string
		in            phase.Input
		event         string
		canTransition bool
	}{
		{
			name:          "can start node from a pending claim with the machine off",
			in:            phase.Input{Claim: TestClaim(slot, "c1", infrav2alpha1.NodeClaimPending, false), ObservedPower: infrav2alpha1.PowerStateOff},
			event:         EventStartNode,
			canTransition: true,
		},
		{
			name:          "can shutdown node from an unwanted machine",
			in:            phase.Input{ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true},
			event:         EventShutdownNode,
			canTransition: true,
		},
		{
			name:          "can complete job from starting up",
			in:            phase.Input{Claim: TestClaim(slot, "c2", infrav2alpha1.NodeClaimBooting, false), ObservedPower: infrav2alpha1.PowerStateOff},
			event:         EventJobCompleted,
			canTransition: true,
		},
		{
			name:          "can complete job from shutting down",
			in:            phase.Input{ObservedPower: infrav2alpha1.PowerStateOn, ShutdownJobInFlight: true},
			event:         EventJobCompleted,
			canTransition: true,
		},
		{
			name:          "can fail job from starting up",
			in:            phase.Input{Claim: TestClaim(slot, "c3", infrav2alpha1.NodeClaimBooting, false), ObservedPower: infrav2alpha1.PowerStateOff},
			event:         EventJobFailed,
			canTransition: true,
		},
		{
			name:          "can fail job from shutting down",
			in:            phase.Input{ObservedPower: infrav2alpha1.PowerStateOn, ShutdownJobInFlight: true},
			event:         EventJobFailed,
			canTransition: true,
		},
		{
			name:          "can timeout from starting up",
			in:            phase.Input{Claim: TestClaim(slot, "c4", infrav2alpha1.NodeClaimBooting, false), ObservedPower: infrav2alpha1.PowerStateOff},
			event:         EventJobTimeout,
			canTransition: true,
		},
		{
			name:          "can timeout from shutting down",
			in:            phase.Input{ObservedPower: infrav2alpha1.PowerStateOn, ShutdownJobInFlight: true},
			event:         EventJobTimeout,
			canTransition: true,
		},
		{
			name:          "can force cleanup from starting up",
			in:            phase.Input{Claim: TestClaim(slot, "c5", infrav2alpha1.NodeClaimBooting, false), ObservedPower: infrav2alpha1.PowerStateOff},
			event:         EventForceCleanup,
			canTransition: true,
		},
		{
			name:          "can force cleanup from shutting down",
			in:            phase.Input{ObservedPower: infrav2alpha1.PowerStateOn, ShutdownJobInFlight: true},
			event:         EventForceCleanup,
			canTransition: true,
		},
		{
			name:          "cannot start node from a ready claim on a usable node",
			in:            phase.Input{Claim: TestClaim(slot, "c6", infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true},
			event:         EventStartNode,
			canTransition: false,
		},
		{
			name:          "cannot shutdown node from a free slot",
			in:            phase.Input{ObservedPower: infrav2alpha1.PowerStateOff},
			event:         EventShutdownNode,
			canTransition: false,
		},
		{
			name:          "cannot complete job from ready",
			in:            phase.Input{Claim: TestClaim(slot, "c7", infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true},
			event:         EventJobCompleted,
			canTransition: false,
		},
		{
			name:          "cannot complete job from shutdown",
			in:            phase.Input{ObservedPower: infrav2alpha1.PowerStateOff},
			event:         EventJobCompleted,
			canTransition: false,
		},
		{
			name:          "cannot force cleanup from ready",
			in:            phase.Input{Claim: TestClaim(slot, "c8", infrav2alpha1.NodeClaimReady, false), ObservedPower: infrav2alpha1.PowerStateOn, NodeUsable: true},
			event:         EventForceCleanup,
			canTransition: false,
		},
		{
			name:          "cannot force cleanup from shutdown",
			in:            phase.Input{ObservedPower: infrav2alpha1.PowerStateOff},
			event:         EventForceCleanup,
			canTransition: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := CreateTestNode(slot, config.NewNamespaceConfig().Get(), tt.in.ObservedPower, "")
			mockCoord := NewMockCoordinationManager()
			fakeClient := CreateFakeClient(scheme, node)

			fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord, WithInput(tt.in))

			assert.Equal(t, tt.canTransition, fsm.CanTransition(tt.event),
				"event %s from the state %q derives", tt.event, fsm.GetCurrentState())
		})
	}
}

func TestNodeStateMachine_StartNode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("successful node start", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.StartNode()
		assert.NoError(t, err)

		// Verify FSM state changed
		assert.Equal(t, StateStartingUp, fsm.GetCurrentState())

		// Verify coordination lock was acquired
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)
		assert.Equal(t, OperationScaleUp, acquireCalls[0].Operation)
		assert.Equal(t, DefaultLockTimeout, acquireCalls[0].Timeout)

		// Verify job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Contains(t, job.Name, "test-node-startup-")
		assert.Equal(t, "homelab-autoscaler-system", job.Namespace)
		assert.Equal(t, "startup", job.Labels["type"])
		assert.Equal(t, "test/startup:latest", job.Spec.Template.Spec.Containers[0].Image)

		// The startup Job must be attributable to the Node CR that caused it.
		assert.Equal(t, "test-node", job.Labels["infra.homecluster.dev/node"])
		assert.Equal(t, "startup", job.Labels["infra.homecluster.dev/phase"])
		assert.Equal(t, "homelab-autoscaler", job.Labels["app.kubernetes.io/name"])
		assert.Equal(t, job.Labels, job.Spec.Template.Labels, "pod template labels must match the job labels")

		// Verify job tracking
		assert.Equal(t, job.Name, fsm.currentJob)
		assert.False(t, fsm.jobStartTime.IsZero())
	})

	t.Run("coordination lock failure", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		mockCoord := NewMockCoordinationManager()
		mockCoord.SetAcquireError(assert.AnError)
		fakeClient := CreateFakeClient(scheme, node)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.StartNode()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "FSM transition failed")

		// Verify FSM state didn't change
		assert.Equal(t, StateShutdown, fsm.GetCurrentState())

		// Verify no job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})

	t.Run("invalid state transition", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.StartNode()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "FSM transition failed")

		// Verify FSM state didn't change
		assert.Equal(t, StateReady, fsm.GetCurrentState())

		// Verify no coordination calls were made
		assert.Len(t, mockCoord.GetAcquireCalls(), 0)
	})
}

func TestNodeStateMachine_ShutdownNode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("successful node shutdown with kubernetes node cordoning", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		kubeNode := CreateTestKubernetesNode("test-node-k8s", false) // Initially schedulable
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node, kubeNode)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.ShutdownNode()
		assert.NoError(t, err)

		// Verify FSM state changed
		assert.Equal(t, StateShuttingDown, fsm.GetCurrentState())

		// Verify coordination lock was acquired
		acquireCalls := mockCoord.GetAcquireCalls()
		assert.Len(t, acquireCalls, 1)
		assert.Equal(t, OperationScaleDown, acquireCalls[0].Operation)
		assert.Equal(t, DefaultLockTimeout, acquireCalls[0].Timeout)

		// Verify Kubernetes node was cordoned
		updatedKubeNode := &corev1.Node{}
		err = fakeClient.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updatedKubeNode)
		assert.NoError(t, err)
		assert.True(t, updatedKubeNode.Spec.Unschedulable, "Kubernetes node should be cordoned during shutdown")

		// Verify job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Contains(t, job.Name, "test-node-shutdown-")
		assert.Equal(t, "homelab-autoscaler-system", job.Namespace)
		assert.Equal(t, "shutdown", job.Labels["type"])
		assert.Equal(t, "test/shutdown:latest", job.Spec.Template.Spec.Containers[0].Image)

		// The shutdown Job must be attributable to the Node CR that caused it.
		assert.Equal(t, "test-node", job.Labels["infra.homecluster.dev/node"])
		assert.Equal(t, "shutdown", job.Labels["infra.homecluster.dev/phase"])
		assert.Equal(t, "homelab-autoscaler", job.Labels["app.kubernetes.io/managed-by"])
		assert.Equal(t, job.Labels, job.Spec.Template.Labels, "pod template labels must match the job labels")

		// Verify job tracking
		assert.Equal(t, job.Name, fsm.currentJob)
		assert.False(t, fsm.jobStartTime.IsZero())
	})

	t.Run("successful node shutdown without kubernetes node", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node) // No Kubernetes node

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.ShutdownNode()
		assert.NoError(t, err) // Should not fail even if Kubernetes node doesn't exist

		// Verify FSM state changed
		assert.Equal(t, StateShuttingDown, fsm.GetCurrentState())

		// Verify job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)
	})

	t.Run("coordination lock failure", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		mockCoord := NewMockCoordinationManager()
		mockCoord.SetAcquireError(assert.AnError)
		fakeClient := CreateFakeClient(scheme, node)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.ShutdownNode()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "FSM transition failed")

		// Verify FSM state didn't change
		assert.Equal(t, StateReady, fsm.GetCurrentState())

		// Verify no job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})
}

func TestNodeStateMachine_JobEvents(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("startup job completed uncordons kubernetes node", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		kubeNode := CreateTestKubernetesNode("test-node-k8s", true) // Initially unschedulable
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node, kubeNode)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.JobCompleted(nil)
		assert.NoError(t, err)

		// Verify FSM state changed to Ready
		assert.Equal(t, StateReady, fsm.GetCurrentState())

		// Verify Kubernetes node was uncordoned
		updatedKubeNode := &corev1.Node{}
		err = fakeClient.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updatedKubeNode)
		assert.NoError(t, err)
		assert.False(t, updatedKubeNode.Spec.Unschedulable, "Kubernetes node should be uncordoned after startup completion")

		// Verify coordination lock was released
		releaseCalls := mockCoord.GetReleaseCalls()
		assert.Len(t, releaseCalls, 1)
	})

	t.Run("shutdown job completed does not uncordon kubernetes node", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
		kubeNode := CreateTestKubernetesNode("test-node-k8s", true) // Initially unschedulable
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node, kubeNode)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.JobCompleted(nil)
		assert.NoError(t, err)

		// Verify FSM state changed to Shutdown
		assert.Equal(t, StateShutdown, fsm.GetCurrentState())

		// Verify Kubernetes node remains cordoned
		updatedKubeNode := &corev1.Node{}
		err = fakeClient.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updatedKubeNode)
		assert.NoError(t, err)
		assert.True(t, updatedKubeNode.Spec.Unschedulable, "Kubernetes node should remain cordoned after shutdown completion")
	})

	tests := []struct {
		name          string
		initialState  infrav2alpha1.NodePhase
		event         func(*NodeStateMachine) error
		expectedState string
	}{
		{
			name:         "job completed from starting up",
			initialState: infrav2alpha1.NodePhaseBooting,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobCompleted(nil)
			},
			expectedState: StateReady,
		},
		{
			name:         "job completed from shutting down",
			initialState: infrav2alpha1.NodePhaseDraining,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobCompleted(nil)
			},
			expectedState: StateShutdown,
		},
		{
			name:         "job failed from starting up",
			initialState: infrav2alpha1.NodePhaseBooting,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobFailed()
			},
			expectedState: StateShutdown,
		},
		{
			name:         "job failed from shutting down",
			initialState: infrav2alpha1.NodePhaseDraining,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobFailed()
			},
			expectedState: StateReady,
		},
		{
			name:         "job timeout from starting up",
			initialState: infrav2alpha1.NodePhaseBooting,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobTimeout()
			},
			expectedState: StateShutdown, // Timeout from startup goes to shutdown
		},
		{
			name:         "job timeout from shutting down",
			initialState: infrav2alpha1.NodePhaseDraining,
			event: func(fsm *NodeStateMachine) error {
				return fsm.JobTimeout()
			},
			expectedState: StateReady, // Timeout from shutdown goes to ready
		},
		{
			name:         "force cleanup from starting up",
			initialState: infrav2alpha1.NodePhaseBooting,
			event: func(fsm *NodeStateMachine) error {
				return fsm.ForceCleanup()
			},
			expectedState: StateShutdown,
		},
		{
			name:         "force cleanup from shutting down",
			initialState: infrav2alpha1.NodePhaseDraining,
			event: func(fsm *NodeStateMachine) error {
				return fsm.ForceCleanup()
			},
			expectedState: StateShutdown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, tt.initialState)
			mockCoord := NewMockCoordinationManager()
			fakeClient := CreateFakeClient(scheme, node)

			fsm := MachineForState(node, fakeClient, scheme, mockCoord)
			fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
			fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline
			fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
			fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

			err := tt.event(fsm)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedState, fsm.GetCurrentState())
		})
	}
}

func TestNodeStateMachine_CalculateBackoff(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
	fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
	fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	t.Run("no backoff strategy returns default", func(t *testing.T) {
		backoff := fsm.CalculateBackoff()
		assert.Equal(t, time.Minute, backoff)
	})

	t.Run("early phase backoff", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-1 * time.Minute),
			retryCount:      0,
			maxRetries:      MaxRetries,
		}

		backoff := fsm.CalculateBackoff()
		assert.Equal(t, 30*time.Second, backoff)
	})

	t.Run("normal phase backoff", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-5 * time.Minute),
			retryCount:      1,
			maxRetries:      MaxRetries,
		}

		backoff := fsm.CalculateBackoff()
		assert.Equal(t, 2*time.Minute, backoff)
	})

	t.Run("late phase backoff", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-12 * time.Minute),
			retryCount:      2,
			maxRetries:      MaxRetries,
		}

		backoff := fsm.CalculateBackoff()
		assert.Equal(t, 5*time.Minute, backoff)
	})

	t.Run("stuck transition triggers force cleanup", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-20 * time.Minute),
			retryCount:      3,
			maxRetries:      MaxRetries,
		}

		backoff := fsm.CalculateBackoff()
		assert.Equal(t, time.Duration(0), backoff)
		// Note: ForceCleanup would be called but we can't easily test that here
	})
}

func TestNodeStateMachine_IsTransitionStuck(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
	mockCoord := NewMockCoordinationManager()
	fakeClient := CreateFakeClient(scheme, node)

	fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
	fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
	fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	t.Run("no backoff strategy is not stuck", func(t *testing.T) {
		assert.False(t, fsm.IsTransitionStuck())
	})

	t.Run("recent transition is not stuck", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-5 * time.Minute),
			retryCount:      1,
			maxRetries:      MaxRetries,
		}

		assert.False(t, fsm.IsTransitionStuck())
	})

	t.Run("old transition is stuck", func(t *testing.T) {
		fsm.backoff = &BackoffStrategy{
			transitionStart: time.Now().Add(-20 * time.Minute),
			retryCount:      3,
			maxRetries:      MaxRetries,
		}

		assert.True(t, fsm.IsTransitionStuck())
	})
}

func TestNodeStateMachine_MonitorJobCompletion(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("job completes successfully", func(t *testing.T) {
		node := CreateTestNode("test-node-monitor-1", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		// Create a job that will be marked as successful
		job := CreateTestJob("test-job-1", config.NewNamespaceConfig().Get(), "startup")
		job.Status.Succeeded = 1
		err := fakeClient.Create(context.TODO(), job)
		require.NoError(t, err)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline
		fsm.jobTimeout = 2 * time.Second                  // Short timeout for testing

		// Start monitoring in a goroutine
		done := make(chan bool)
		go func() {
			fsm.monitorJobCompletion("test-job-1")
			done <- true
		}()

		// Wait for monitoring to complete
		select {
		case <-done:
			// Success - job monitoring completed
		case <-time.After(5 * time.Second):
			t.Fatal("Job monitoring did not complete in time")
		}

		// Give a small delay for the FSM event to process
		time.Sleep(100 * time.Millisecond)

		// Verify FSM transitioned to ready state
		assert.Equal(t, StateReady, fsm.GetCurrentState())
	})

	t.Run("job fails", func(t *testing.T) {
		node := CreateTestNode("test-node-monitor-2", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		// Create a job that will be marked as failed
		job := CreateTestJob("test-job-2", config.NewNamespaceConfig().Get(), "startup")
		job.Status.Failed = 1
		err := fakeClient.Create(context.TODO(), job)
		require.NoError(t, err)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline
		fsm.jobTimeout = 2 * time.Second                  // Short timeout for testing

		// Start monitoring in a goroutine
		done := make(chan bool)
		go func() {
			fsm.monitorJobCompletion("test-job-2")
			done <- true
		}()

		// Wait for monitoring to complete
		select {
		case <-done:
			// Success - job monitoring completed
		case <-time.After(5 * time.Second):
			t.Fatal("Job monitoring did not complete in time")
		}

		// Give a small delay for the FSM event to process
		time.Sleep(100 * time.Millisecond)

		// Verify FSM transitioned to shutdown state (failure from starting up)
		assert.Equal(t, StateShutdown, fsm.GetCurrentState())
	})

	t.Run("job times out", func(t *testing.T) {
		node := CreateTestNode("test-node-monitor-3", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		// Create a job that remains running
		job := CreateTestJob("test-job-3", config.NewNamespaceConfig().Get(), "startup")
		job.Status.Active = 1
		err := fakeClient.Create(context.TODO(), job)
		require.NoError(t, err)

		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline
		fsm.jobTimeout = 100 * time.Millisecond           // Very short timeout for testing

		// Start monitoring in a goroutine
		done := make(chan bool)
		go func() {
			fsm.monitorJobCompletion("test-job-3")
			done <- true
		}()

		// Wait for monitoring to complete
		select {
		case <-done:
			// Success - job monitoring completed with timeout
		case <-time.After(2 * time.Second):
			t.Fatal("Job monitoring did not complete in time")
		}

		// Give a small delay for the FSM event to process
		time.Sleep(100 * time.Millisecond)

		// Verify FSM transitioned to shutdown state (timeout from starting up goes to shutdown)
		assert.Equal(t, StateShutdown, fsm.GetCurrentState())
	})
}

func TestNodeStateMachine_CleanupPendingJobs(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("cleanup multiple pending jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		mockCoord := NewMockCoordinationManager()

		// Create multiple pending jobs for the same node
		pendingStartupJob := CreateTestJobWithNodeLabel("test-node-startup-old", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		pendingShutdownJob := CreateTestJobWithNodeLabel("test-node-shutdown-old", config.NewNamespaceConfig().Get(), "shutdown", "test-node-k8s")

		// Create a completed job that should not be deleted
		completedJob := CreateTestJobWithNodeLabel("test-node-startup-completed", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		completedJob.Status.Succeeded = 1

		// Create a job for a different node that should not be deleted
		otherNodeJob := CreateTestJobWithNodeLabel("other-node-startup", config.NewNamespaceConfig().Get(), "startup", "other-node-k8s")

		fakeClient := CreateFakeClient(scheme, node, pendingStartupJob, pendingShutdownJob, completedJob, otherNodeJob)
		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		// Call cleanup
		err := fsm.cleanupPendingJobs("test-node-k8s")
		assert.NoError(t, err)

		// Verify pending jobs were deleted
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)

		// Should have 2 jobs remaining: completed job and other node job
		assert.Len(t, jobs.Items, 2)

		jobNames := make(map[string]bool)
		for _, job := range jobs.Items {
			jobNames[job.Name] = true
		}

		assert.True(t, jobNames["test-node-startup-completed"], "Completed job should not be deleted")
		assert.True(t, jobNames["other-node-startup"], "Other node job should not be deleted")
		assert.False(t, jobNames["test-node-startup-old"], "Pending startup job should be deleted")
		assert.False(t, jobNames["test-node-shutdown-old"], "Pending shutdown job should be deleted")
	})

	t.Run("cleanup with no pending jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		// Call cleanup when no jobs exist
		err := fsm.cleanupPendingJobs("test-node-k8s")
		assert.NoError(t, err)

		// Verify no jobs exist
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 0)
	})

	t.Run("cleanup with empty kubernetes node name", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		node.Spec.KubernetesNodeName = "" // Empty node name
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		// Call cleanup with empty node name
		err := fsm.cleanupPendingJobs("")
		assert.NoError(t, err) // Should not error
	})

	t.Run("cleanup only affects startup and shutdown jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		mockCoord := NewMockCoordinationManager()

		// Create jobs with different types
		startupJob := CreateTestJobWithNodeLabel("test-node-startup", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		shutdownJob := CreateTestJobWithNodeLabel("test-node-shutdown", config.NewNamespaceConfig().Get(), "shutdown", "test-node-k8s")

		// Create a job with different type that should not be deleted
		otherTypeJob := CreateTestJobWithNodeLabel("test-node-other", config.NewNamespaceConfig().Get(), "maintenance", "test-node-k8s")

		fakeClient := CreateFakeClient(scheme, node, startupJob, shutdownJob, otherTypeJob)
		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		// Call cleanup
		err := fsm.cleanupPendingJobs("test-node-k8s")
		assert.NoError(t, err)

		// Verify only startup/shutdown jobs were deleted
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)

		assert.Len(t, jobs.Items, 1)
		assert.Equal(t, "test-node-other", jobs.Items[0].Name)
		assert.Equal(t, "maintenance", jobs.Items[0].Labels["type"])
	})
}

func TestNodeStateMachine_StartNodeWithCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("startup cleans up existing pending jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		mockCoord := NewMockCoordinationManager()

		// Create existing pending jobs
		oldStartupJob := CreateTestJobWithNodeLabel("test-node-startup-old", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		oldShutdownJob := CreateTestJobWithNodeLabel("test-node-shutdown-old", config.NewNamespaceConfig().Get(), "shutdown", "test-node-k8s")

		fakeClient := CreateFakeClient(scheme, node, oldStartupJob, oldShutdownJob)
		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		// Start node
		err := fsm.StartNode()
		assert.NoError(t, err)

		// Verify old jobs were cleaned up and new job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)

		// Should have only 1 job (the new startup job)
		assert.Len(t, jobs.Items, 1)

		newJob := jobs.Items[0]
		assert.Contains(t, newJob.Name, "test-node-startup-")
		assert.Equal(t, "startup", newJob.Labels["type"])
		assert.NotEqual(t, "test-node-startup-old", newJob.Name) // Should be a new job
	})
}

func TestNodeStateMachine_ShutdownNodeWithCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("shutdown cleans up existing pending jobs", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		mockCoord := NewMockCoordinationManager()

		// Create existing pending jobs
		oldStartupJob := CreateTestJobWithNodeLabel("test-node-startup-old", config.NewNamespaceConfig().Get(), "startup", "test-node-k8s")
		oldShutdownJob := CreateTestJobWithNodeLabel("test-node-shutdown-old", config.NewNamespaceConfig().Get(), "shutdown", "test-node-k8s")

		fakeClient := CreateFakeClient(scheme, node, oldStartupJob, oldShutdownJob)
		fsm := MachineForState(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		// Shutdown node
		err := fsm.ShutdownNode()
		assert.NoError(t, err)

		// Verify old jobs were cleaned up and new job was created
		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)

		// Should have only 1 job (the new shutdown job)
		assert.Len(t, jobs.Items, 1)

		newJob := jobs.Items[0]
		assert.Contains(t, newJob.Name, "test-node-shutdown-")
		assert.Equal(t, "shutdown", newJob.Labels["type"])
		assert.NotEqual(t, "test-node-shutdown-old", newJob.Name) // Should be a new job
	})
}

func TestNodeStateMachine_VolumesAndServiceAccount(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	t.Run("startup job uses default ServiceAccount when not specified", func(t *testing.T) {
		node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.StartNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Equal(t, "default", job.Spec.Template.Spec.ServiceAccountName)
	})

	t.Run("startup job uses specified ServiceAccount", func(t *testing.T) {
		serviceAccountName := "custom-service-account"
		node := CreateTestNode("test-node-sa", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		node.Spec.StartupPodSpec.ServiceAccount = &serviceAccountName

		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.StartNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Equal(t, "custom-service-account", job.Spec.Template.Spec.ServiceAccountName)
	})

	t.Run("startup job mounts Secret volume", func(t *testing.T) {
		node := CreateTestNode("test-node-secret", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		node.Spec.StartupPodSpec.Volumes = []infrav2alpha1.VolumeMountSpec{
			{
				Name:       "credentials",
				MountPath:  "/etc/credentials",
				SecretName: "my-secret",
			},
		}

		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.StartNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]

		// Verify volume is created
		assert.Len(t, job.Spec.Template.Spec.Volumes, 1)
		assert.Equal(t, "credentials", job.Spec.Template.Spec.Volumes[0].Name)
		assert.NotNil(t, job.Spec.Template.Spec.Volumes[0].Secret)
		assert.Equal(t, "my-secret", job.Spec.Template.Spec.Volumes[0].Secret.SecretName)

		// Verify volume mount is created
		assert.Len(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, 1)
		assert.Equal(t, "credentials", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name)
		assert.Equal(t, "/etc/credentials", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath)
		assert.True(t, job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly)
	})

	t.Run("startup job mounts ConfigMap volume", func(t *testing.T) {
		node := CreateTestNode("test-node-configmap", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		node.Spec.StartupPodSpec.Volumes = []infrav2alpha1.VolumeMountSpec{
			{
				Name:          "config",
				MountPath:     "/etc/config",
				ConfigMapName: "my-configmap",
			},
		}

		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.StartNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]

		// Verify volume is created
		assert.Len(t, job.Spec.Template.Spec.Volumes, 1)
		assert.Equal(t, "config", job.Spec.Template.Spec.Volumes[0].Name)
		assert.NotNil(t, job.Spec.Template.Spec.Volumes[0].ConfigMap)
		assert.Equal(t, "my-configmap", job.Spec.Template.Spec.Volumes[0].ConfigMap.Name)

		// Verify volume mount is created
		assert.Len(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, 1)
		assert.Equal(t, "config", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name)
		assert.Equal(t, "/etc/config", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath)
		assert.True(t, job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly)
	})

	t.Run("startup job mounts multiple volumes", func(t *testing.T) {
		node := CreateTestNode("test-node-multi", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		node.Spec.StartupPodSpec.Volumes = []infrav2alpha1.VolumeMountSpec{
			{
				Name:       "credentials",
				MountPath:  "/credentials",
				SecretName: "hetzner-credentials",
			},
			{
				Name:          "scripts",
				MountPath:     "/scripts",
				ConfigMapName: "hcloud-scripts",
			},
		}

		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.StartNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]

		// Verify both volumes are created
		assert.Len(t, job.Spec.Template.Spec.Volumes, 2)

		volumeNames := make(map[string]bool)
		for _, vol := range job.Spec.Template.Spec.Volumes {
			volumeNames[vol.Name] = true
		}
		assert.True(t, volumeNames["credentials"])
		assert.True(t, volumeNames["scripts"])

		// Verify both volume mounts are created
		assert.Len(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, 2)

		mountPaths := make(map[string]bool)
		for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
			mountPaths[mount.MountPath] = true
		}
		assert.True(t, mountPaths["/credentials"])
		assert.True(t, mountPaths["/scripts"])
	})

	t.Run("shutdown job uses default ServiceAccount when not specified", func(t *testing.T) {
		node := CreateTestNode("test-node-shutdown-sa", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.ShutdownNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Equal(t, "default", job.Spec.Template.Spec.ServiceAccountName)
	})

	t.Run("shutdown job uses specified ServiceAccount", func(t *testing.T) {
		serviceAccountName := "shutdown-service-account"
		node := CreateTestNode("test-node-shutdown-sa2", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		node.Spec.ShutdownPodSpec.ServiceAccount = &serviceAccountName

		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.ShutdownNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]
		assert.Equal(t, "shutdown-service-account", job.Spec.Template.Spec.ServiceAccountName)
	})

	t.Run("shutdown job mounts Secret volume", func(t *testing.T) {
		node := CreateTestNode("test-node-shutdown-secret", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		node.Spec.ShutdownPodSpec.Volumes = []infrav2alpha1.VolumeMountSpec{
			{
				Name:       "credentials",
				MountPath:  "/etc/credentials",
				SecretName: "shutdown-secret",
			},
		}

		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.ShutdownNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]

		// Verify volume is created
		assert.Len(t, job.Spec.Template.Spec.Volumes, 1)
		assert.Equal(t, "credentials", job.Spec.Template.Spec.Volumes[0].Name)
		assert.NotNil(t, job.Spec.Template.Spec.Volumes[0].Secret)
		assert.Equal(t, "shutdown-secret", job.Spec.Template.Spec.Volumes[0].Secret.SecretName)

		// Verify volume mount is created
		assert.Len(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, 1)
		assert.Equal(t, "credentials", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name)
		assert.Equal(t, "/etc/credentials", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath)
		assert.True(t, job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly)
	})

	t.Run("shutdown job mounts ConfigMap volume", func(t *testing.T) {
		node := CreateTestNode("test-node-shutdown-configmap", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		node.Spec.ShutdownPodSpec.Volumes = []infrav2alpha1.VolumeMountSpec{
			{
				Name:          "config",
				MountPath:     "/etc/config",
				ConfigMapName: "shutdown-configmap",
			},
		}

		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.ShutdownNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]

		// Verify volume is created
		assert.Len(t, job.Spec.Template.Spec.Volumes, 1)
		assert.Equal(t, "config", job.Spec.Template.Spec.Volumes[0].Name)
		assert.NotNil(t, job.Spec.Template.Spec.Volumes[0].ConfigMap)
		assert.Equal(t, "shutdown-configmap", job.Spec.Template.Spec.Volumes[0].ConfigMap.Name)

		// Verify volume mount is created
		assert.Len(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, 1)
		assert.Equal(t, "config", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name)
		assert.Equal(t, "/etc/config", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath)
		assert.True(t, job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly)
	})

	t.Run("shutdown job uses volumes and ServiceAccount together", func(t *testing.T) {
		serviceAccountName := "shutdown-sa"
		node := CreateTestNode("test-node-shutdown-combined", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		node.Spec.ShutdownPodSpec.ServiceAccount = &serviceAccountName
		node.Spec.ShutdownPodSpec.Volumes = []infrav2alpha1.VolumeMountSpec{
			{
				Name:       "credentials",
				MountPath:  "/credentials",
				SecretName: "shutdown-creds",
			},
			{
				Name:          "scripts",
				MountPath:     "/scripts",
				ConfigMapName: "shutdown-scripts",
			},
		}

		mockCoord := NewMockCoordinationManager()
		fakeClient := CreateFakeClient(scheme, node)

		fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
		fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
		fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

		err := fsm.ShutdownNode()
		assert.NoError(t, err)

		jobs := &batchv1.JobList{}
		err = fakeClient.List(context.TODO(), jobs)
		assert.NoError(t, err)
		assert.Len(t, jobs.Items, 1)

		job := jobs.Items[0]

		// Verify ServiceAccount is set
		assert.Equal(t, "shutdown-sa", job.Spec.Template.Spec.ServiceAccountName)

		// Verify volumes are created
		assert.Len(t, job.Spec.Template.Spec.Volumes, 2)

		// Verify volume mounts are created
		assert.Len(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, 2)

		// Verify all volume mounts are read-only
		for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
			assert.True(t, mount.ReadOnly)
		}
	})
}

// ownerRefOfKind is the ownerReference of the given Kind, and nil when the object has
// none. A lifecycle Job has two owners once a claim exists, so the assertions read the
// ref they are talking about by kind instead of by position.
func ownerRefOfKind(refs []metav1.OwnerReference, kind string) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Kind == kind {
			return &refs[i]
		}
	}

	return nil
}

// isControllerRef is the reading Kubernetes itself uses for the controller owner: a ref
// with no Controller field is a plain, non-controller owner.
func isControllerRef(ref *metav1.OwnerReference) bool {
	return ref.Controller != nil && *ref.Controller
}

// TestNodeStateMachine_LifecycleJobProvenance asserts that the startup/shutdown Jobs the
// controller creates carry the label + annotation set that makes them attributable to the
// Node CR that caused them and - when the transition has one - to the NodeClaim whose
// attempt they are, that the pod template labels stay in sync with the Job's own labels,
// and that the ownerReferences tie the Job to both objects: the claim is the controller
// owner of its attempt and the Node stays a secondary owner, and with no claim at all the
// Node stays exactly the controller owner it was (Argo CD reads those ownerReferences; no
// Argo tracking label is added).
func TestNodeStateMachine_LifecycleJobProvenance(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	const nodeUID = types.UID("11111111-2222-3333-4444-555555555555")
	const claimUID = types.UID("66666666-7777-8888-9999-000000000000")

	tests := []struct {
		name     string
		phase    string
		power    infrav2alpha1.PowerState
		progress infrav2alpha1.NodePhase
		action   func(*NodeStateMachine) error
		// claim is the live claim of the transition, and nil is the shape of a shutdown of a
		// machine the pool stopped wanting: no claim, nothing asking, law 3.
		claim *infrav2alpha1.NodeClaim
	}{
		{
			name:     "startup job",
			phase:    PhaseStartup,
			power:    infrav2alpha1.PowerStateOff,
			progress: infrav2alpha1.NodePhaseOff,
			action:   func(f *NodeStateMachine) error { return f.StartNode() },
		},
		{
			name:     "shutdown job",
			phase:    PhaseShutdown,
			power:    infrav2alpha1.PowerStateOn,
			progress: infrav2alpha1.NodePhaseReady,
			action:   func(f *NodeStateMachine) error { return f.ShutdownNode() },
		},
		{
			name:   "startup job of a live claim",
			phase:  PhaseStartup,
			power:  infrav2alpha1.PowerStateOff,
			action: func(f *NodeStateMachine) error { return f.StartNode() },
			claim:  TestClaim("test-node", "test-node-claim-a", infrav2alpha1.NodeClaimPending, false),
		},
		{
			name:   "shutdown job of a claim a scale-down marked",
			phase:  PhaseShutdown,
			power:  infrav2alpha1.PowerStateOn,
			action: func(f *NodeStateMachine) error { return f.ShutdownNode() },
			claim:  TestClaim("test-node", "test-node-claim-b", infrav2alpha1.NodeClaimReady, true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), tt.power, tt.progress)
			node.UID = nodeUID
			mockCoord := NewMockCoordinationManager()
			fakeClient := CreateFakeClient(scheme, node)

			opts := []Option{}
			if tt.claim != nil {
				tt.claim.UID = claimUID
				opts = append(opts, WithClaim(tt.claim, nil))
			}

			fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord, opts...)
			fsm.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
			fsm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

			require.NoError(t, tt.action(fsm))

			jobs := &batchv1.JobList{}
			require.NoError(t, fakeClient.List(context.TODO(), jobs))
			require.Len(t, jobs.Items, 1)

			job := jobs.Items[0]

			// Expected label set, written out literally so an accidental change to the
			// shared helper shows up here.
			expectedLabels := map[string]string{
				// existing keys, kept as-is for the cleanup selector and the gRPC server
				"app":   "homelab-autoscaler",
				"group": "test-group",
				"node":  "test-node-k8s", // spec.kubernetesNodeName, deliberately not the CR name
				"type":  tt.phase,
				// standard Kubernetes recommended labels
				"app.kubernetes.io/name":       "homelab-autoscaler",
				"app.kubernetes.io/component":  "node-lifecycle",
				"app.kubernetes.io/managed-by": "homelab-autoscaler",
				// explicit provenance: the Node CR a human / Argo CD sees
				"infra.homecluster.dev/node":  "test-node",
				"infra.homecluster.dev/phase": tt.phase,
			}
			expectedAnnotations := map[string]string{
				"infra.homecluster.dev/node-uid":   string(nodeUID),
				"infra.homecluster.dev/node-group": "test-group",
				"infra.homecluster.dev/created-by": "homelab-autoscaler",
			}

			if tt.claim != nil {
				// The attempt is the claim's, so the Job of an attempt names it, and
				// `kubectl get jobs -l infra.homecluster.dev/nodeclaim=<claim>` answers "which
				// attempt did this?". With no claim - the shutdown of a machine the pool stopped
				// wanting - both keys stay absent, and the selector answers exactly the Jobs that
				// have an attempt behind them.
				expectedLabels["infra.homecluster.dev/nodeclaim"] = tt.claim.Name
				expectedAnnotations["infra.homecluster.dev/nodeclaim-uid"] = string(claimUID)
			}

			assert.Equal(t, expectedLabels, job.Labels, "job labels")
			assert.Equal(t, expectedLabels, job.Spec.Template.Labels,
				"pod template labels must stay in sync with the job labels")
			assert.Equal(t, expectedAnnotations, job.Annotations, "job annotations")

			// The Jobs are controller-owned, not Argo-managed children: an Argo tracking
			// label would make Argo claim and prune them.
			assert.NotContains(t, job.Labels, "argocd.argoproj.io/instance")
			assert.NotContains(t, job.Spec.Template.Labels, "argocd.argoproj.io/instance")

			// The ownerReferences are the provenance Argo CD's resource tree reads. With a live
			// claim the Job hangs under the claim that asked for it - the controller owner - and
			// under the slot as a secondary owner; with no claim, under the slot alone, as before.
			refs := job.GetOwnerReferences()

			nodeRef := ownerRefOfKind(refs, "Node")
			require.NotNil(t, nodeRef, "the Job stays attributable to the slot")
			assert.Equal(t, infrav2alpha1.GroupVersion.String(), nodeRef.APIVersion)
			assert.Equal(t, "test-node", nodeRef.Name)
			assert.Equal(t, nodeUID, nodeRef.UID)

			claimRef := ownerRefOfKind(refs, "NodeClaim")

			if tt.claim == nil {
				require.Len(t, refs, 1, "no claim means the slot is the only owner")
				assert.Nil(t, claimRef, "no claim means no claim ownerReference")
				assert.True(t, isControllerRef(nodeRef), "with no claim the slot is the controller owner")

				return
			}

			require.NotNil(t, claimRef, "the Job of an attempt is owned by the claim that asked for it")
			assert.Equal(t, infrav2alpha1.GroupVersion.String(), claimRef.APIVersion)
			assert.Equal(t, tt.claim.Name, claimRef.Name)
			assert.Equal(t, claimUID, claimRef.UID)
			assert.True(t, isControllerRef(claimRef), "the claim is the controller owner of its Job")
			require.NotNil(t, claimRef.BlockOwnerDeletion)
			assert.True(t, *claimRef.BlockOwnerDeletion,
				"a committed boot is not removed ahead of the shutdown Job that reverses it")
			assert.False(t, isControllerRef(nodeRef), "the slot is a secondary, non-controller owner")
		})
	}
}

// TestNodeStateMachine_LifecycleJobSelectors asserts that every selector that reads the lifecycle
// Jobs of a slot keeps finding them now that the Job is claim-owned and carries the claim label:
// cleanupPendingJobs (node label + type filter), the slot reconciler's view (slot label + phase
// label), the group view the provider's label keys give (`group` + `type`), and the claim selector
// that answers "which attempt did this?". A label that stopped matching would strand a pending Job
// in the slot, and the recovery that re-fires a finished Job reads the same labels.
func TestNodeStateMachine_LifecycleJobSelectors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	tests := []struct {
		name   string
		power  infrav2alpha1.PowerState
		phase  string
		action func(*NodeStateMachine) error
		claim  *infrav2alpha1.NodeClaim
	}{{
		name:   "startup Job of a live claim",
		power:  infrav2alpha1.PowerStateOff,
		phase:  PhaseStartup,
		action: func(f *NodeStateMachine) error { return f.StartNode() },
		claim:  TestClaim("test-node", "test-node-claim-sel", infrav2alpha1.NodeClaimPending, false),
	}, {
		name:   "shutdown Job with no claim",
		power:  infrav2alpha1.PowerStateOn,
		phase:  PhaseShutdown,
		action: func(f *NodeStateMachine) error { return f.ShutdownNode() },
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), tt.power, "")
			mockCoord := NewMockCoordinationManager()
			fakeClient := CreateFakeClient(scheme, node)

			opts := []Option{}
			if tt.claim != nil {
				opts = append(opts, WithClaim(tt.claim, nil))
			}

			fsm := NewNodeStateMachine(node, fakeClient, scheme, mockCoord, opts...)
			fsm.uncordonTimeout = 100 * time.Millisecond
			fsm.readinessGateTimeout = 100 * time.Millisecond

			require.NoError(t, tt.action(fsm))

			// The reconciler's view: the slot label, filtered by the phase label
			// (lifecycleJobsInFlight, newestLifecycleJob).
			found := listJobsWithLabels(t, fakeClient, node.Namespace, map[string]string{LabelNodeCR: node.Name})
			require.Len(t, found, 1, "the slot selector must find the Job it created")
			assert.Equal(t, tt.phase, found[0].Labels[LabelPhase], "the phase label the reconciler filters on")

			// The provider's grouping key, and the type filter.
			grouped := listJobsWithLabels(t, fakeClient, node.Namespace, map[string]string{
				LabelGroup: node.Labels[LabelGroup], LabelType: tt.phase,
			})
			require.Len(t, grouped, 1, "the group + type selector must find the Job")

			// The attempt's view: the claim the Job belongs to. A Job the pool owes no claim to -
			// the shutdown of a machine nobody wants - is not selectable by claim at all.
			if tt.claim == nil {
				claimJobs := listJobsWithLabels(t, fakeClient, node.Namespace, map[string]string{LabelNodeClaim: "any"})
				require.Len(t, claimJobs, 0, "a Job with no claim carries no claim label")
			} else {
				claimJobs := listJobsWithLabels(t, fakeClient, node.Namespace, map[string]string{LabelNodeClaim: tt.claim.Name})
				require.Len(t, claimJobs, 1, "the claim selector answers which attempt made this Job")
				assert.Equal(t, tt.claim.Name, claimJobs[0].Labels[LabelNodeClaim])
			}

			// cleanupPendingJobs selects on the legacy node label and filters on the type
			// label: it must still find the pending Job, claim-owned or not, and delete it.
			require.NoError(t, fsm.cleanupPendingJobs(node.Spec.KubernetesNodeName))

			remaining := listJobsWithLabels(t, fakeClient, node.Namespace, map[string]string{LabelNodeLegacy: node.Spec.KubernetesNodeName})
			assert.Empty(t, remaining, "cleanupPendingJobs found the Job through its selectors and deleted it")
		})
	}
}

// listJobsWithLabels lists the Jobs of a namespace through a label selector, the way the callers
// that read the lifecycle Jobs do.
func listJobsWithLabels(t *testing.T, c client.Client, namespace string, set map[string]string) []batchv1.Job {
	t.Helper()

	jobs := &batchv1.JobList{}
	err := c.List(context.TODO(), jobs, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: labels.SelectorFromSet(labels.Set(set)),
	})
	require.NoError(t, err)

	return jobs.Items
}

// TestNodeStateMachine_PriorityClassName asserts that spec.startupPodSpec.priorityClassName /
// spec.shutdownPodSpec.priorityClassName land on the Pod template of the Job the FSM creates,
// and that leaving the field unset keeps the historical empty value (backwards compatibility).
func TestNodeStateMachine_PriorityClassName(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	tests := []struct {
		name     string
		priority *string
		podSpec  func(*infrav2alpha1.Node, *string)
		power    infrav2alpha1.PowerState
		progress infrav2alpha1.NodePhase
		action   func(*NodeStateMachine) error
		expected string
	}{
		{
			name:     "startup job applies priorityClassName",
			priority: ptrToString("system-cluster-critical"),
			podSpec:  func(n *infrav2alpha1.Node, p *string) { n.Spec.StartupPodSpec.PriorityClassName = p },
			power:    infrav2alpha1.PowerStateOff,
			progress: infrav2alpha1.NodePhaseOff,
			action:   func(f *NodeStateMachine) error { return f.StartNode() },
			expected: "system-cluster-critical",
		},
		{
			name:     "shutdown job applies priorityClassName",
			priority: ptrToString("power-management"),
			podSpec:  func(n *infrav2alpha1.Node, p *string) { n.Spec.ShutdownPodSpec.PriorityClassName = p },
			power:    infrav2alpha1.PowerStateOn,
			progress: infrav2alpha1.NodePhaseReady,
			action:   func(f *NodeStateMachine) error { return f.ShutdownNode() },
			expected: "power-management",
		},
		{
			name:     "startup job without priorityClassName stays empty",
			priority: nil,
			podSpec:  func(n *infrav2alpha1.Node, p *string) { n.Spec.StartupPodSpec.PriorityClassName = p },
			power:    infrav2alpha1.PowerStateOff,
			progress: infrav2alpha1.NodePhaseOff,
			action:   func(f *NodeStateMachine) error { return f.StartNode() },
			expected: "",
		},
		{
			name:     "shutdown job without priorityClassName stays empty",
			priority: nil,
			podSpec:  func(n *infrav2alpha1.Node, p *string) { n.Spec.ShutdownPodSpec.PriorityClassName = p },
			power:    infrav2alpha1.PowerStateOn,
			progress: infrav2alpha1.NodePhaseReady,
			action:   func(f *NodeStateMachine) error { return f.ShutdownNode() },
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := CreateTestNode("test-node-priority", config.NewNamespaceConfig().Get(), tt.power, tt.progress)
			tt.podSpec(node, tt.priority)

			mockCoord := NewMockCoordinationManager()
			fakeClient := CreateFakeClient(scheme, node)

			f := NewNodeStateMachine(node, fakeClient, scheme, mockCoord)
			f.uncordonTimeout = 100 * time.Millisecond      // Short timeout for tests
			f.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

			require.NoError(t, tt.action(f))

			jobs := &batchv1.JobList{}
			require.NoError(t, fakeClient.List(context.TODO(), jobs))
			require.Len(t, jobs.Items, 1)

			assert.Equal(t, tt.expected, jobs.Items[0].Spec.Template.Spec.PriorityClassName)
		})
	}
}

func ptrToString(s string) *string {
	return &s
}

// taintLiftClient reads a corev1.Node and, from the nth read on, reports it without
// Cilium's bootstrap taint: that is what Cilium does when its agent finishes with a node that
// has just joined. It also records every write of a node object, in order, so a test can tell
// uncordoning the node from refusing it.
type taintLiftClient struct {
	client.Client
	liftFrom int
	// readyFrom reports the node NotReady until the readyFrom-th read, the shape of a kubelet that
	// has registered the object and is not Ready yet - the window the registration deadline is for.
	readyFrom int
	key       string
	gets      int
	writes    []string
}

func (c *taintLiftClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}

	kubeNode, isKubeNode := obj.(*corev1.Node)
	if !isKubeNode {
		return nil
	}

	c.gets++

	if c.readyFrom > 0 && c.gets < c.readyFrom {
		kubeNode.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
		}
	}

	if c.key != "" && c.liftFrom > 0 && c.gets >= c.liftFrom {
		kept := make([]corev1.Taint, 0, len(kubeNode.Spec.Taints))

		for _, taint := range kubeNode.Spec.Taints {
			if taint.Key == c.key {
				continue
			}

			kept = append(kept, taint)
		}

		kubeNode.Spec.Taints = kept
	}

	return nil
}

func (c *taintLiftClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if kubeNode, isKubeNode := obj.(*corev1.Node); isKubeNode {
		keys := make([]string, 0, len(kubeNode.Spec.Taints))
		for _, taint := range kubeNode.Spec.Taints {
			keys = append(keys, taint.Key)
		}

		c.writes = append(c.writes, fmt.Sprintf("update unschedulable=%v taints=[%s]",
			kubeNode.Spec.Unschedulable, strings.Join(keys, ",")))
	}

	return c.Client.Update(ctx, obj, opts...)
}

// ciliumNode builds the node object of a machine that has just joined: cordoned by the scale-down
// that preceded it, Ready, and carrying Cilium's bootstrap taint because the agent has not
// finished programming it.
func ciliumNode(taints ...corev1.Taint) *corev1.Node {
	node := CreateTestKubernetesNode("test-node-k8s", true)
	node.Spec.Taints = taints

	return node
}

// TestSetNodeSchedulableWaitsForTheCiliumBootstrapTaint is the live symptom of a pool that
// churns: the kubelet marks a fresh machine Ready minutes before Cilium clears
// node.cilium.io/agent-not-ready, and the pool's pods do not tolerate that taint. Completing the
// scale-up on Ready tells the autoscaler the slot is capacity, the pending pod still cannot land,
// and the pool buys a second machine for a node that is about to work.
//
// The gate has to be passed, not read: the node is uncordoned on the pass where the taint is
// gone, and the pass that saw the taint wrote nothing.
func TestSetNodeSchedulableWaitsForTheCiliumBootstrapTaint(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	bootstrap := corev1.Taint{
		Key:    TaintCiliumAgentNotReady,
		Value:  "true",
		Effect: corev1.TaintEffectNoExecute,
	}

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	base := CreateFakeClient(scheme, node, ciliumNode(bootstrap))
	recorder := &taintLiftClient{Client: base, key: TaintCiliumAgentNotReady, liftFrom: 2}

	nm := NewNodeStateMachine(node, recorder, scheme, NewMockCoordinationManager(),
		WithReadinessTaintKeys([]string{TaintCiliumAgentNotReady}))
	nm.uncordonTimeout = 20 * time.Second

	start := time.Now()

	err := nm.setNodeSchedulableWithRetry("test-node-k8s")
	elapsed := time.Since(start)

	require.NoError(t, err, "the gate is satisfied on a later pass, so the uncordon succeeds")

	assert.GreaterOrEqual(t, recorder.gets, 3, "the loop refused the node while Cilium was still bootstrapping")
	assert.GreaterOrEqual(t, elapsed, 2*time.Second, "each refusal waits a retry interval, it is not a busy loop")

	require.Len(t, recorder.writes, 1, "the node object is written once, when the gate is satisfied")
	assert.NotContains(t, recorder.writes[0], TaintCiliumAgentNotReady,
		"the uncordon never carries the bootstrap taint back onto the node: \"%s\"", recorder.writes[0])
	assert.Contains(t, recorder.writes[0], "unschedulable=false")

	updated := &corev1.Node{}
	require.NoError(t, base.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updated))
	assert.False(t, updated.Spec.Unschedulable, "the node ends up schedulable")
}

// TestSetNodeSchedulableDoesNotWaitForACiliumItDoesNotHave: a pool on a CNI that does not add the
// bootstrap taint must not pay for the gate. Ready and clean is uncordoned on the first pass,
// with no retry, and unrelated taints (the pool's own, the kubelet's) never hold the transition
// open.
func TestSetNodeSchedulableDoesNotWaitForACiliumItDoesNotHave(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	role := corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}
	unreachable := corev1.Taint{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute}

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	base := CreateFakeClient(scheme, node, ciliumNode(role, unreachable))
	recorder := &taintLiftClient{Client: base}

	nm := NewNodeStateMachine(node, recorder, scheme, NewMockCoordinationManager(),
		WithReadinessTaintKeys([]string{TaintCiliumAgentNotReady}))
	nm.uncordonTimeout = 20 * time.Second

	start := time.Now()

	require.NoError(t, nm.setNodeSchedulableWithRetry("test-node-k8s"))

	assert.Less(t, time.Since(start), time.Second, "no Cilium, no waiting")
	// Three reads, and no retry. The gate reads the node, the label apply reads it (a slot that
	// declares nothing still has to be reconciled: the record of the keys the slot wrote, which is
	// what removes a dropped key, lives on the node object, so it has to be read), and the uncordon
	// reads it before it writes. spec.nodeTaints is empty on this fixture, so the taint apply
	// returns before its own read - the reason the count was 2 before the labels field arrived.
	assert.Equal(t, 3, recorder.gets, "one read for the gate, one for the declared labels, one for the uncordon, no retry")
	require.Len(t, recorder.writes, 1)
	assert.Contains(t, recorder.writes[0], "node.kubernetes.io/role", "the taints of the slot are kept by the uncordon")
}

// captureLogs installs a logger writing to a buffer for the duration of a test and returns it.
// The FSM logs through controller-runtime's global logger (log.Log), so a test that asserts an
// operator-visible line has to install one. The previous logger is restored on cleanup.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	buffer := &bytes.Buffer{}

	log.SetLogger(zap.New(zap.WriteTo(buffer), zap.UseDevMode(true)))
	t.Cleanup(func() { log.SetLogger(logr.Discard()) })

	return buffer
}

// TestSetNodeSchedulableTimeoutUncordonsAndLogs is the bounded half of the gate. A machine whose
// CNI never finishes must not wedge the slot: the loop uncordons the node on the way out, names
// the gate it was waiting on, and returns an error the caller logs and continues past. The
// uncordon matters more than the gate - a cordoned node is skipped by cluster-autoscaler's
// scale-down, so it is never drained, never deleted, and nothing would ever uncordon it again.
func TestSetNodeSchedulableTimeoutUncordonsAndLogs(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	logs := captureLogs(t)

	bootstrap := corev1.Taint{Key: TaintCiliumAgentNotReady, Value: "true", Effect: corev1.TaintEffectNoExecute}

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	base := CreateFakeClient(scheme, node, ciliumNode(bootstrap))
	recorder := &taintLiftClient{Client: base}

	nm := NewNodeStateMachine(node, recorder, scheme, NewMockCoordinationManager(),
		WithReadinessTaintKeys([]string{TaintCiliumAgentNotReady}))
	nm.uncordonTimeout = 100 * time.Millisecond
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	err := nm.setNodeSchedulableWithRetry("test-node-k8s")

	require.Error(t, err, "the caller is told the gate was never satisfied")
	assert.Contains(t, err.Error(), "never became usable")
	assert.Contains(t, err.Error(), TaintCiliumAgentNotReady, "the error names the gate that timed out")

	updated := &corev1.Node{}
	require.NoError(t, base.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updated))
	assert.False(t, updated.Spec.Unschedulable, "the timeout path uncordons: a cordoned machine is invisible to scale-down")

	require.Len(t, recorder.writes, 1, "one write, the best-effort uncordon")
	assert.Contains(t, recorder.writes[0], "unschedulable=false")

	// The operator-facing half: the line that names the gate and the reason it is passed over.
	contents := logs.String()
	assert.Contains(t, contents, "Timed out waiting for the node to become usable, uncordoning it anyway")
	assert.Contains(t, contents, TaintCiliumAgentNotReady, "the log says what the loop was waiting on")
}

// TestSetNodeSchedulableOnMissingNodeTimesOutWithoutWriting is the shape of a startup whose
// machine never registers: there is no node object at all. The loop times out, and the
// best-effort uncordon must not invent a node object.
func TestSetNodeSchedulableOnMissingNodeTimesOutWithoutWriting(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	base := CreateFakeClient(scheme, node)
	recorder := &taintLiftClient{Client: base}

	nm := NewNodeStateMachine(node, recorder, scheme, NewMockCoordinationManager())
	nm.uncordonTimeout = 100 * time.Millisecond
	nm.readinessGateTimeout = 100 * time.Millisecond // the gate has its own deadline

	require.Error(t, nm.setNodeSchedulableWithRetry("test-node-k8s"))
	assert.Empty(t, recorder.writes, "nothing to uncordon, so nothing is written")
}

// TestNodeUsability is the gate itself: Ready is necessary and not sufficient, Cilium's bootstrap
// taint is what holds the transition open, and the taints of the slot and of the kubelet are none
// of its business.
func TestNodeUsability(t *testing.T) {
	bootstrap := corev1.Taint{Key: TaintCiliumAgentNotReady, Value: "true", Effect: corev1.TaintEffectNoExecute}
	role := corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}

	notReady := CreateTestKubernetesNode("test-node-k8s", true)
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse

	noCondition := CreateTestKubernetesNode("test-node-k8s", true)
	noCondition.Status.Conditions = nil

	tests := []struct {
		name       string
		node       *corev1.Node
		wantReason string
	}{
		{"Ready and clean", CreateTestKubernetesNode("test-node-k8s", true), ""},
		{"Ready with the pool's own taint", CreateTestKubernetesNode("test-node-k8s", true), ""},
		{"not Ready", notReady, "node is not Ready"},
		{"no Ready condition at all", noCondition, "node is not Ready"},
		{"Ready, Cilium still bootstrapping", ciliumNode(bootstrap), TaintCiliumAgentNotReady},
	}

	tests[1].node.Spec.Taints = []corev1.Taint{role}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason := nodeUsability(tc.node, []string{TaintCiliumAgentNotReady})
			assert.Contains(t, reason, tc.wantReason)
		})
	}

	// The keys are the configuration the Group carries. With no keys the gate is the Ready condition
	// alone - which is the explicit "this CNI stamps no bootstrap taint" declaration - and a key the
	// node does not carry never holds the transition open.
	assert.Equal(t, "", nodeUsability(ciliumNode(bootstrap), nil), "no keys, no gate: Ready is enough")
	// A declared key the node carries is the reason to wait, and the key is configuration: the gate is
	// not Cilium-specific.
	other := ciliumNode(corev1.Taint{Key: "some.other/cni", Effect: corev1.TaintEffectNoExecute})
	assert.Contains(t, nodeUsability(other, []string{"some.other/cni"}), "some.other/cni",
		"any declared key the node carries holds the transition open")
	assert.Equal(t, "", nodeUsability(other, []string{"node.kubelet.io/startup"}),
		"a declared key the node does not carry is not a reason to wait")
}

// newGateMachine is the loop with both budgets spelled out, so a test says which half it is
// exercising: registration is the wait for the node object, the gate is the wait for that object to
// be Ready and free of the readiness taints, counted from the pass it appears on.
func newGateMachine(node *infrav2alpha1.Node, c client.Client, scheme *runtime.Scheme, coord CoordinationManager, keys []string, registration, gate time.Duration) *NodeStateMachine {
	nm := NewNodeStateMachine(node, c, scheme, coord, WithReadinessTaintKeys(keys))
	nm.uncordonTimeout = registration
	nm.readinessGateTimeout = gate

	return nm
}

// TestSetNodeSchedulableGateHasItsOwnDeadline is the M2 fix, and it fails against a single shared
// budget: the registration wait must not spend the readiness gate's time.
//
// The shape is a cold boot. The node object registers on the first read (registration is satisfied
// in milliseconds), and the kubelet reports NotReady for two seconds while it finishes joining. With
// one 100ms budget the loop times out on the machine it exists to protect: it uncordons a NotReady
// node, logs an operator-visible failure, holds the hook open for the whole budget, and the slot is
// declared ready anyway - which is exactly the symptom the gate was added for, on every boot. With
// the gate's own deadline, the same boot passes.
func TestSetNodeSchedulableGateHasItsOwnDeadline(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	base := CreateFakeClient(scheme, node, ciliumNode())

	// Ready on the 3rd read: the loop must still be waiting for the gate, not out of budget.
	recorder := &taintLiftClient{Client: base, readyFrom: 3}

	nm := newGateMachine(node, recorder, scheme, NewMockCoordinationManager(),
		[]string{TaintCiliumAgentNotReady}, 100*time.Millisecond, 20*time.Second)

	start := time.Now()
	err := nm.setNodeSchedulableWithRetry("test-node-k8s")
	elapsed := time.Since(start)

	require.NoError(t, err, "the registration budget is spent, the gate budget is not")
	assert.Greater(t, elapsed, 100*time.Millisecond,
		"a shared budget of 100ms would have returned an error by now: it took %v", elapsed.String())
	assert.GreaterOrEqual(t, recorder.gets, 3, "the loop kept polling the gate after the registration deadline passed")

	updated := &corev1.Node{}
	require.NoError(t, base.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updated))
	assert.False(t, updated.Spec.Unschedulable, "and the node is uncordoned on the pass the gate is satisfied")
}

// TestSetNodeSchedulableRegistrationDeadlineIsTheNodeObject: the registration budget is the other
// half, and it is the one that applies while there is no node object at all. A machine that never
// registers fails fast, writes no node object, and says what it was waiting for.
func TestSetNodeSchedulableRegistrationDeadlineIsTheNodeObject(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	base := CreateFakeClient(scheme, node)
	recorder := &taintLiftClient{Client: base}

	nm := newGateMachine(node, recorder, scheme, NewMockCoordinationManager(), nil, 100*time.Millisecond, 20*time.Second)

	start := time.Now()
	err := nm.setNodeSchedulableWithRetry("test-node-k8s")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has not appeared", "the timeout names the half of the loop that ran out")
	assert.NotContains(t, err.Error(), TaintCiliumAgentNotReady, "the readiness gate was never opened, so it is not the reason")
	assert.Less(t, time.Since(start), 5*time.Second, "the registration budget, not the gate, decided: %v", time.Since(start).String())
	assert.Empty(t, recorder.writes, "there is no node object to uncordon, so nothing is written")
}

// TestReadinessGateKeepsTheCoordinationLockAlive is why the gate loop re-stamps the lock. The gate is
// bounded by 4m and the registration wait by 90s - together longer than DefaultLockTimeout (5m). A
// lock that ages out mid-gate is not a lock: the progress watchdog's lock gate stops protecting the
// slot, validateStateTransition cleans the lock up, and the next acquisition takes the slot over,
// while the operation that acquired it is still running. So the loop that holds the slot open renews
// the lock that says the slot is held.
//
// The lock's timeout is 5s and the loop runs for 6s with sleeps of 2s and 4s, so an un-renewed lock
// is expired before the loop finishes: the assertion is about the lock's age at the end, not about a
// call count alone.
func TestReadinessGateKeepsTheCoordinationLockAlive(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	base := CreateFakeClient(scheme, node, ciliumNode())
	recorder := &taintLiftClient{Client: base, readyFrom: 3}

	coord := NewMockCoordinationManager()
	require.NoError(t, coord.AcquireLock(context.TODO(), node, OperationScaleUp, 5*time.Second))

	nm := newGateMachine(node, recorder, scheme, coord, nil, 20*time.Second, 20*time.Second)

	require.NoError(t, nm.setNodeSchedulableWithRetry("test-node-k8s"))

	assert.NotEmpty(t, coord.GetRenewCalls(), "the gate re-stamps the lock it runs under")

	lock := coord.GetLock("test-node")
	require.NotNil(t, lock, "the operation still holds the lock it acquired")
	assert.Equal(t, OperationScaleUp, lock.Operation, "renewal keeps the operation the lock names")
	assert.Equal(t, 5*time.Second, lock.Timeout, "and its timeout")
	assert.False(t, coord.IsLockExpired(lock),
		"the lock is live at the end of a gate that outlasted its timeout: age %v, timeout %v",
		time.Since(lock.Timestamp).String(), lock.Timeout.String())
}

// TestReadinessGateTimeoutRecordsAWarningOnTheSlot is the operator-visible half of the gate timeout.
// The log line rolls away; a pool whose nodes come up unable to take the pool's work has to say so on
// the slot, which is the repo's convention for "go and look". The uncordon still happens: the slot is
// recorded ready, and a cordoned machine is one the autoscaler will never remove.
func TestReadinessGateTimeoutRecordsAWarningOnTheSlot(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	bootstrap := corev1.Taint{Key: TaintCiliumAgentNotReady, Value: "true", Effect: corev1.TaintEffectNoExecute}

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	base := CreateFakeClient(scheme, node, ciliumNode(bootstrap))
	recorder := &taintLiftClient{Client: base}
	rec := events.NewFakeRecorder(16)

	nm := NewNodeStateMachine(node, recorder, scheme, NewMockCoordinationManager(),
		WithEventRecorder(rec),
		WithReadinessTaintKeys([]string{TaintCiliumAgentNotReady}),
		WithReadinessGateTimeout(100*time.Millisecond))
	nm.uncordonTimeout = 20 * time.Second

	err := nm.setNodeSchedulableWithRetry("test-node-k8s")
	require.Error(t, err, "the caller is told the gate timed out, and continues past it")

	// The registration budget is 20s, so the timeout below can only be the gate's: the deadline that
	// started when the node object appeared.
	assert.Contains(t, err.Error(), "timeout after 100ms", "the error quotes the gate's own budget")

	warnings := drainEvents(rec)

	require.Len(t, warnings, 1, "one gate, one event")
	assert.Contains(t, warnings[0], corev1.EventTypeWarning)
	assert.Contains(t, warnings[0], ReasonReadinessGateTimeout)
	assert.Contains(t, warnings[0], TaintCiliumAgentNotReady, "the event names the taint the gate was waiting for")
	assert.Contains(t, warnings[0], "readiness gate", "and which half of the loop timed out")

	updated := &corev1.Node{}
	require.NoError(t, base.Get(context.TODO(), client.ObjectKey{Name: "test-node-k8s"}, updated))
	assert.False(t, updated.Spec.Unschedulable, "the timeout path uncordons: a cordoned machine is invisible to scale-down")
}

// TestWithReadinessGateTimeoutZeroKeepsTheDefault: an option that zeroes the gate budget would make
// the loop time out on the pass the node appears, which is the failure the gate exists to close. A
// non-positive override is ignored, so only a real budget changes the default.
func TestWithReadinessGateTimeoutZeroKeepsTheDefault(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	node := CreateTestNode("test-node", config.NewNamespaceConfig().Get(), infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
	nm := NewNodeStateMachine(node, CreateFakeClient(scheme, node), scheme, NewMockCoordinationManager(),
		WithReadinessGateTimeout(0))

	assert.Equal(t, DefaultReadinessGateTimeout, nm.readinessGateTimeout)
	assert.Empty(t, nm.readinessTaintKeys, "no option, no keys: the gate is Ready-only until the Group configures it")
}
