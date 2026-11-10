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

package grpcserver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

const testGroup = "remote-workers"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	return scheme
}

// nodeCR builds a Node CR (a slot) of the given group and power state.
func nodeCR(name string, desired infrav1alpha1.PowerState, status infrav1alpha1.PowerState, progress infrav1alpha1.Progress) *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"group": testGroup},
		},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  desired,
			KubernetesNodeName: name,
		},
		Status: infrav1alpha1.NodeStatus{
			PowerState: status,
			Progress:   progress,
		},
	}
}

// k8sNode builds a Kubernetes node, optionally with a providerID set by a
// cloud-controller-manager.
func k8sNode(name, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

func newServer(t *testing.T, objects ...client.Object) *HomeClusterProviderServer {
	scheme := testScheme(t)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1alpha1.Node{}).
		Build()

	return NewHomeClusterProviderServer(k8sClient, scheme)
}

func TestNodeGroupNodes(t *testing.T) {
	tests := []struct {
		name string
		// objects to seed the fake client with
		objects []client.Object
		// expected instance id -> instance state
		expected map[string]pb.InstanceStatus_InstanceState
	}{
		{
			name: "powered off slots are not instances",
			objects: []client.Object{
				nodeCR("slot-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{},
		},
		{
			name: "slot that is on without a kubernetes node is provisioning",
			objects: []client.Object{
				nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"slot-1": pb.InstanceStatus_instanceCreating,
			},
		},
		{
			name: "slot that is on with a kubernetes node is running",
			objects: []client.Object{
				nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
				k8sNode("slot-1", "homelab://slot-1"),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"homelab://slot-1": pb.InstanceStatus_instanceRunning,
			},
		},
		{
			name: "empty providerID falls back to the kubernetes node name",
			objects: []client.Object{
				nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
				k8sNode("slot-1", ""),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"slot-1": pb.InstanceStatus_instanceRunning,
			},
		},
		{
			name: "mixed pool reports only existing and provisioning capacity",
			objects: []client.Object{
				// fresh, never provisioned slot of a 15 slot pool
				nodeCR("slot-off", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
				// slot requested by IncreaseSize, machine not joined yet
				nodeCR("slot-new", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
				// running slot without a cloud-controller-manager
				nodeCR("omni-remote-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
				k8sNode("omni-remote-1", ""),
			},
			expected: map[string]pb.InstanceStatus_InstanceState{
				"slot-new":      pb.InstanceStatus_instanceCreating,
				"omni-remote-1": pb.InstanceStatus_instanceRunning,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer(t, tc.objects...)

			resp, err := server.NodeGroupNodes(context.Background(), &pb.NodeGroupNodesRequest{Id: testGroup})
			require.NoError(t, err)
			require.NotNil(t, resp)

			got := make(map[string]pb.InstanceStatus_InstanceState, len(resp.Instances))
			for _, instance := range resp.Instances {
				require.NotEmpty(t, instance.Id, "instance id must never be empty")
				require.NotNil(t, instance.Status)
				got[instance.Id] = instance.Status.InstanceState
			}

			assert.Len(t, resp.Instances, len(tc.expected))
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestNodeGroupTargetSizeIgnoresSeededSlots(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		nodeCR("slot-2", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		nodeCR("slot-3", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
		nodeCR("slot-4", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressStartingUp),
		nodeCR("slot-5", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown),
	)

	resp, err := server.NodeGroupTargetSize(context.Background(), &pb.NodeGroupTargetSizeRequest{Id: testGroup})
	require.NoError(t, err)
	// only the ready and the starting up slot count towards the target size
	assert.Equal(t, int32(2), resp.TargetSize)
}

func TestNodeGroupIncreaseSizePicksSeededOffSlot(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOff, infrav1alpha1.ProgressShutdown),
		nodeCR("slot-2", infrav1alpha1.PowerStateOff, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressShuttingDown),
	)

	_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.NoError(t, err)

	updated := &infrav1alpha1.Node{}
	require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: "slot-1", Namespace: "default"}, updated))
	assert.Equal(t, infrav1alpha1.PowerStateOn, updated.Spec.DesiredPowerState)

	// the shutting down slot must not be touched
	stillOff := &infrav1alpha1.Node{}
	require.NoError(t, server.Client.Get(context.Background(), client.ObjectKey{Name: "slot-2", Namespace: "default"}, stillOff))
	assert.Equal(t, infrav1alpha1.PowerStateOff, stillOff.Spec.DesiredPowerState)
}

func TestNodeGroupIncreaseSizeFailsWithoutFreeSlot(t *testing.T) {
	server := newServer(t,
		nodeCR("slot-1", infrav1alpha1.PowerStateOn, infrav1alpha1.PowerStateOn, infrav1alpha1.ProgressReady),
	)

	_, err := server.NodeGroupIncreaseSize(context.Background(), &pb.NodeGroupIncreaseSizeRequest{Id: testGroup, Delta: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no powered off nodes found")
}
