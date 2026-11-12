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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// claimsTestScheme is the scheme the fake client of these specs serves: the built-in types (the
// corev1.Node a machine registers) and the operator's own.
func claimsTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, infrav2alpha1.AddToScheme(scheme))

	return scheme
}

// TestReportPhaseCompareAndSetsOnTheObservation is the race the two writers of the report race for,
// driven on one write.
//
// The report of a machine is derived from one triple - the live claim, the observed power, the
// usability of the node object - and `status.powerState` is written by the observation reconciler
// from the node object, in the same write as the report it derives from it. A transition derives its
// report from the read of the slot its pass made, and the machine that answers for a lifecycle Job is
// the goroutine of the pass that created the Job, so that read is as old as the Job. When the
// observation path lands between the read and the write, the last writer is the one holding the older
// evidence.
//
// What that costs is not visible to the writer. A slot left reporting `draining` for a machine that is
// off, or `booting` for a machine that takes the pool's work, is what every reader of the pool reads
// (`Group.status.on`, the cluster view, `kubectl get nodes.infra.homecluster.dev`), and nothing is
// coming to undo it: the node object of a machine whose kubelet is dead has no kubelet left to move
// it, the claim has reached the phase the report names, the lifecycle Jobs are finished, and the next
// pass over a settled slot is its resync - `idleResync`, five minutes.
//
// The two objects of the race are the slot as a pass read it and the slot as the cluster stores it:
// the same name, the same namespace, and a different machine, which is what the race is.
func TestReportPhaseCompareAndSetsOnTheObservation(t *testing.T) {
	namespace := config.NewNamespaceConfig().Get()

	slot := func(power infrav2alpha1.PowerState, report infrav2alpha1.NodePhase) *infrav2alpha1.Node {
		return CreateTestNode("test-node", namespace, power, report)
	}

	stored := func(t *testing.T, nm *NodeStateMachine) *infrav2alpha1.Node {
		t.Helper()

		read := &infrav2alpha1.Node{}
		require.NoError(t, nm.client.Get(context.TODO(), client.ObjectKey{Name: "test-node", Namespace: namespace}, read))

		return read
	}

	t.Run("a machine built on the record, observed since, records nothing", func(t *testing.T) {
		scheme := claimsTestScheme(t)

		// The pass read the machine up: the shutdown of a machine the pool does not want, in the window
		// in which a kubelet that has just fallen silent is still reported Ready. The cluster has been
		// observed since: the machine is off, and the report written with that observation is `off`.
		passRead := slot(infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)

		nm := NewNodeStateMachine(passRead, CreateFakeClient(scheme, slot(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)),
			scheme, NewMockCoordinationManager(),
			WithInput(phase.Input{ObservedPower: infrav2alpha1.PowerStateOn}))

		require.Equal(t, infrav2alpha1.PowerStateOn, nm.readObservation,
			"the machine is stamped with the record its pass read")
		require.False(t, nm.observedLive, "the machine has not looked at the machine itself")

		nm.reportPhase()

		assert.Equal(t, infrav2alpha1.NodePhaseOff, stored(t, nm).Status.Phase,
			"the write of the older read is refused: the phase it derives, `draining`, names a machine the\n"+
				"cluster does not report, and the path that recorded the machine off wrote the report of\n"+
				"that observation in the same write")
	})

	t.Run("a machine that read the node itself writes", func(t *testing.T) {
		scheme := claimsTestScheme(t)

		// The boot started from a machine that is off and the slot is reporting the boot. The record has
		// not caught up with the machine the readiness gate is standing in front of: the node object
		// reports Ready, and the slot still records the machine off, because the observation path runs on
		// a node event and the registration is the event that has not been processed yet.
		passRead := slot(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		inCluster := slot(infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)

		kubeNode := CreateTestKubernetesNode("test-node-k8s", false)
		kubeNode.Labels["infra.homecluster.dev/node"] = "test-node"

		nm := NewNodeStateMachine(passRead, CreateFakeClient(scheme, inCluster, kubeNode), scheme,
			NewMockCoordinationManager(),
			WithInput(phase.Input{
				Claim:         TestClaim("test-node", "claim-live", infrav2alpha1.NodeClaimBooting, false),
				ObservedPower: infrav2alpha1.PowerStateOff,
			}))

		// The Job of the boot completed, so the machine is in the state its completion lands on; the
		// readiness gate read the node object it is answering for, and the claim's answer - the ready
		// move - is the gate's to write (afterJobCompleted: recordGateAnswer, moveClaim, reportPhase).
		nm.PrimeEnterState(StateReady)
		nm.recordGateAnswer(kubeNode)
		nm.moveClaim(infrav2alpha1.NodeClaimReady, "")

		require.True(t, nm.observedLive, "the machine has looked at the machine itself")

		nm.reportPhase()

		assert.Equal(t, infrav2alpha1.NodePhaseReady, stored(t, nm).Status.Phase,
			"a live read outranks the record on the slot, so the gate's report lands: a boot that cannot\n"+
				"report the machine it bought leaves the slot at `booting` for the life of the machine,\n"+
				"because a kubelet settled into Ready writes no further corev1.Node event")
	})
}
