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

package capacity

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// slot returns a Node CR that declares spec.nodeCapacity, the shared fixture of this package.
func slot(declared corev1.ResourceList) *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-01", Namespace: "default"},
		Spec: infrav1alpha1.NodeSpec{
			KubernetesNodeName: "worker-01",
			NodeCapacity:       declared,
		},
	}
}

// registered returns the corev1.Node a booted machine reports, Ready unless stated otherwise.
func registered(cpu, memory string, ready bool) *corev1.Node {
	status, reason := corev1.ConditionFalse, "KubeletNotReady"
	if ready {
		status, reason = corev1.ConditionTrue, "KubeletReady"
	}

	allocatable := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)}
	if cpu != "" {
		allocatable[corev1.ResourceCPU] = resource.MustParse(cpu)
	}

	conditions := []corev1.NodeCondition{{
		Type:   corev1.NodeReady,
		Status: status,
		Reason: reason,
	}}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-01"},
		Status: corev1.NodeStatus{
			// capacity is deliberately larger than allocatable: the kubelet subtracts a system
			// reservation, and the check compares the declaration against allocatable.
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			Allocatable: allocatable,
			Conditions:  conditions,
		},
	}
}

func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, infrav1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

// driftedLine and missingLine render one Mismatches entry the way the package does, so the table
// below asserts which resources are named and with which values, and not how a Quantity
// canonicalizes: resource.MustParse("3.2Gi").String() is "3435973836800m", because 3.2Gi is not a
// whole number of bytes, and spelling that out per expectation is noise.
func driftedLine(name, declared, live string) string {
	want, got := quantity(declared), quantity(live)

	return fmt.Sprintf("%s: declared %s, the node reports %s", name, want, got)
}

func missingLine(name, declared string) string {
	return fmt.Sprintf("%s: declared %s, the node does not report it", name, quantity(declared))
}

// quantity is the String() of a parsed quantity, the rendering Mismatches uses.
func quantity(value string) string {
	parsed := resource.MustParse(value)

	return parsed.String()
}

// declaredCX23 is the shape a 2 vCPU / 4 GiB machine is sold as, after the kubelet's system
// reservation: the number a working declaration carries, and the reason the tolerance is not zero.
func declaredCX23() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1950m"),
		corev1.ResourceMemory: resource.MustParse("3.2Gi"),
	}
}

// TestMismatches is the tolerance table: what counts as a stale declaration and what does not.
func TestMismatches(t *testing.T) {
	tests := []struct {
		name     string
		declared corev1.ResourceList
		live     corev1.ResourceList
		want     []string
	}{
		{
			name:     "matching values",
			declared: declaredCX23(),
			live:     declaredCX23(),
			want:     nil,
		},
		{
			name:     "the datasheet number, 2.5% away, is inside the tolerance",
			declared: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1950m")},
			live:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			want:     nil,
		},
		{
			name:     "exactly the tolerance is not beyond it",
			declared: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")},
			live:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("950m")},
			want:     nil,
		},
		{
			name:     "one percent further is drift",
			declared: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")},
			live:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("940m")},
			want:     []string{driftedLine("cpu", "1000m", "940m")},
		},
		{
			name:     "a machine that grew is drift, not only one that shrank",
			declared: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1950m")},
			live:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			want:     []string{driftedLine("cpu", "1950m", "4")},
		},
		{
			name:     "a declared resource the node does not report at all",
			declared: declaredCX23(),
			live:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1950m")},
			want:     []string{missingLine("memory", "3.2Gi")},
		},
		{
			name:     "a declared resource the node reports as zero",
			declared: declaredCX23(),
			live: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1950m"),
				corev1.ResourceMemory: resource.MustParse("0"),
			},
			want: []string{driftedLine("memory", "3.2Gi", "0")},
		},
		{
			name:     "the resources the node reports and nobody declared are not compared",
			declared: declaredCX23(),
			live: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("1950m"),
				corev1.ResourceMemory:           resource.MustParse("3.2Gi"),
				corev1.ResourcePods:             resource.MustParse("110"),
				corev1.ResourceEphemeralStorage: resource.MustParse("40Gi"),
			},
			want: nil,
		},
		{
			name:     "every drifted resource is named, in sorted order",
			declared: declaredCX23(),
			live: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
			want: []string{
				driftedLine("cpu", "1950m", "1"),
				driftedLine("memory", "3.2Gi", "16Gi"),
			},
		},
		{
			name:     "declaring zero means the node must report zero",
			declared: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")},
			live:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			want:     []string{driftedLine("cpu", "0", "1")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Mismatches(tc.declared, tc.live))
		})
	}
}

// TestCheckDeclaredCapacityRecordsWarning is the point of the field's drift check: a stale
// declaration must be visible from outside the controller's logs, because the pool it describes
// silently stops being placeable.
func TestCheckDeclaredCapacityRecordsWarning(t *testing.T) {
	recorder := &capturingRecorder{}
	nodeCR := slot(declaredCX23())

	// the machine was resized under the declaration
	c := newClient(t, nodeCR, registered("4", "3.2Gi", true))
	require.NoError(t, CheckDeclaredCapacity(context.Background(), c, nodeCR, recorder))

	require.Equal(t, 1, len(recorder.calls), "one reconcile records one event, naming every drifted resource")

	call := recorder.calls[0]
	assert.Equal(t, corev1.EventTypeWarning, call.eventType,
		"a stale declaration stops the pool from scaling, so it is not a Normal event")
	assert.Equal(t, ReasonCapacityMismatch, call.reason)
	assert.Equal(t, ActionCheckCapacity, call.action)
	assert.Same(t, nodeCR, call.regarding, "the event goes on the Node CR, where `kubectl describe` shows it")

	message := call.message()
	assert.Contains(t, message, "cpu", "the event names the resource that drifted")
	assert.Contains(t, message, "1950m", "the event carries the declared value")
	assert.Contains(t, message, "4", "the event carries the value the node reports")
	assert.Contains(t, message, "worker-01", "the event says which slot declares it")
	assert.Contains(t, message, "spec.nodeCapacity", "the event says which field to fix")
	assert.NotContains(t, message, "memory", "only the drifted resource is named")
	assert.NotContains(t, message, "%!", "the note is passed as an argument, so its 5% stays a 5%")
}

// TestCheckDeclaredCapacityStaysSilent covers the cases that must not put an Event on the Node
// CR: a healthy declaration, a pool that declares nothing, a machine that has not booted, and a
// machine whose kubelet has not reported allocatable yet.
func TestCheckDeclaredCapacityStaysSilent(t *testing.T) {
	tests := []struct {
		name     string
		nodeCR   *infrav1alpha1.Node
		objects  []client.Object
		warnings int
	}{
		{
			name:   "allocatable matches the declaration",
			nodeCR: slot(declaredCX23()),
			objects: []client.Object{
				registered("1950m", "3.2Gi", true),
			},
			warnings: 0,
		},
		{
			name: "allocatable is within the tolerance",
			// 3.2Gi reported as 3.0Gi is 6.25% away, 3.1Gi is 3.1% away: the second is a
			// reservation that moved, the first is a machine that changed.
			nodeCR: slot(declaredCX23()),
			objects: []client.Object{
				registered("1950m", "3.1Gi", true),
			},
			warnings: 0,
		},
		{
			name:   "no declaration, so nothing to be stale about",
			nodeCR: slot(nil),
			objects: []client.Object{
				registered("1", "1Gi", true),
			},
			warnings: 0,
		},
		{
			name:   "the machine has not registered",
			nodeCR: slot(declaredCX23()),
			objects: []client.Object{
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "someone-else"}},
			},
			warnings: 0,
		},
		{
			name:   "the node is not Ready, so its allocatable is not a measurement",
			nodeCR: slot(declaredCX23()),
			objects: []client.Object{
				registered("1", "1Gi", false),
			},
			warnings: 0,
		},
		{
			name: "the slot has never been provisioned",
			nodeCR: func() *infrav1alpha1.Node {
				s := slot(declaredCX23())
				s.Spec.KubernetesNodeName = ""

				return s
			}(),
			objects:  []client.Object{},
			warnings: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(8)

			objects := append([]client.Object{tc.nodeCR}, tc.objects...)
			c := newClient(t, objects...)

			require.NoError(t, CheckDeclaredCapacity(context.Background(), c, tc.nodeCR, recorder))
			assert.Zero(t, len(recorder.Events), "no drift, no event")
		})
	}
}

// TestCheckDeclaredCapacityNeverWritesTheNode keeps the check a check. The kubelet owns
// status.allocatable; a controller that patched it to a declaration would give the scheduler a
// machine that does not exist.
func TestCheckDeclaredCapacityNeverWritesTheNode(t *testing.T) {
	recorder := events.NewFakeRecorder(8)
	nodeCR := slot(declaredCX23())
	live := registered("1", "1Gi", true)

	c := newClient(t, nodeCR, live)
	require.NoError(t, CheckDeclaredCapacity(context.Background(), c, nodeCR, recorder))

	after := &corev1.Node{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "worker-01"}, after))

	assert.Equal(t,
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		after.Status.Allocatable, "the node is left exactly as the kubelet reported it")
	assert.Equal(t, 1, len(recorder.Events))
}

// TestCheckDeclaredCapacityToleratesANilRecorder covers the unit-test and library use of the
// reconciler, where no recorder is wired.
func TestCheckDeclaredCapacityToleratesANilRecorder(t *testing.T) {
	nodeCR := slot(declaredCX23())
	c := newClient(t, nodeCR, registered("1", "1Gi", true))

	require.NoError(t, CheckDeclaredCapacity(context.Background(), c, nodeCR, nil))
}

// TestCheckDeclaredCapacityPassesTheNoteAsAnArgument keeps the tolerance sentence readable. The
// recorder renders fmt.Sprintf(note, args...), and the note carries a literal "5%", so the note
// has to arrive as an argument: as a format string it shows up to the operator mangled.
func TestCheckDeclaredCapacityPassesTheNoteAsAnArgument(t *testing.T) {
	recorder := &capturingRecorder{}
	nodeCR := slot(declaredCX23())
	c := newClient(t, nodeCR, registered("1", "1Gi", true))

	require.NoError(t, CheckDeclaredCapacity(context.Background(), c, nodeCR, recorder))
	require.Equal(t, 1, len(recorder.calls))

	assert.Equal(t, "%s", recorder.calls[0].note, "the note is data, not a format string")

	message := recorder.calls[0].message()
	assert.Contains(t, message, "more than 5%")
	assert.Contains(t, message, "worker-01")
	assert.NotContains(t, message, "%!")
}

// capturedEvent is one Eventf call, so a test can assert the reason, the action and the rendered
// message instead of a string the fake recorder concatenates.
type capturedEvent struct {
	regarding runtime.Object
	related   runtime.Object
	eventType string
	reason    string
	action    string
	note      string
	args      []interface{}
}

// message renders the note the way the real recorder does, fmt.Sprintf(note, args...), so a note
// used as a format string shows up mangled here.
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
