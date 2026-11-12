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

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
	"github.com/homecluster-dev/homelab-autoscaler/internal/nodelabels"
)

// createSlotWithNode declares a slot and creates the corev1.Node its machine registered: a node
// object with the labels a kubelet stamps, and no Ready condition, which is the state of a machine
// that joined a moment ago. NotReady keeps the lifecycle of the slot inert (the machine is observed
// off, no claim names it, so the pass writes the node object and decides nothing).
func createSlotWithNode(name string, labels, annotations map[string]string) *infrav2alpha1.Node {
	GinkgoHelper()

	slot := &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: NamespaceConfig.Get(),
			Labels:    map[string]string{"group": "envtest-node-labels"},
		},
		Spec: infrav2alpha1.NodeSpec{
			KubernetesNodeName: name,
			Pricing:            infrav2alpha1.PricingSpec{HourlyRate: "0.05", PodRate: "0.001"},
			StartupPodSpec:     infrav2alpha1.MinimalPodSpec{Image: "envtest/startup:latest"},
			ShutdownPodSpec:    infrav2alpha1.MinimalPodSpec{Image: "envtest/shutdown:latest"},
			NodeLabels:         labels,
			NodeAnnotations:    annotations,
		},
	}
	createIgnoreExists(slot)

	createIgnoreExists(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"kubernetes.io/hostname": name, "node.kubernetes.io/role": "agent"},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionFalse,
				Reason: "KubeletNotReady",
			}},
		},
	})

	return slot
}

func readKubeNode(name string) *corev1.Node {
	GinkgoHelper()

	node := &corev1.Node{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)).To(Succeed())

	return node
}

// pass runs one reconcile of the slot: the whole reconciler, so the assertion is that the label
// apply is on the path a slot's resync takes, and not on a helper nobody calls.
func pass(reconciler *NodeReconciler, slot *infrav2alpha1.Node) {
	GinkgoHelper()

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(slot)})
	Expect(err).NotTo(HaveOccurred())
}

// slotEvents reads the events.k8s.io Events the recorder published on the slot. The recorder is the
// manager's, so this is the assertion that the Event of a real write reaches a real cluster, and that
// `kubectl describe node.infra.homecluster.dev <slot>` has it.
func slotEvents(slot *infrav2alpha1.Node) []eventsv1.Event {
	GinkgoHelper()

	clientset, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())

	list, err := clientset.EventsV1().Events(slot.Namespace).List(context.Background(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	var out []eventsv1.Event

	for _, event := range list.Items {
		if event.Regarding.Name == slot.Name && event.Regarding.Kind == "Node" {
			out = append(out, event)
		}
	}

	return out
}

func awaitSlotEvent(slot *infrav2alpha1.Node, reason string) eventsv1.Event {
	GinkgoHelper()

	var found *eventsv1.Event

	Eventually(func() string {
		published := slotEvents(slot)

		for i := range published {
			if published[i].Reason == reason {
				found = &published[i]

				return ""
			}
		}

		return fmt.Sprintf("slot %s carries %v, and no %s", slot.Name, eventReasons(published), reason)
	}, 20*time.Second, 250*time.Millisecond).Should(BeEmpty())

	Expect(found).NotTo(BeNil())

	return *found
}

func eventReasons(published []eventsv1.Event) []string {
	reasons := make([]string, 0, len(published))
	for _, event := range published {
		reasons = append(reasons, event.Reason)
	}

	return reasons
}

func appliedEventCount(slot *infrav2alpha1.Node) int {
	GinkgoHelper()

	count := 0

	for _, event := range slotEvents(slot) {
		if event.Reason == nodelabels.ReasonApplied {
			count++
		}
	}

	return count
}

// The apply of spec.nodeLabels / spec.nodeAnnotations, driven through a real API server.
//
// The unit tests of internal/nodelabels hold the merge rules and assert them on a fake client. What
// they cannot assert is the write against a live object: that a JSON merge patch carrying a null
// removes a key from a real node (the fake client folds it), that the record annotation survives
// the write that reads it back, that the Event the recorder builds for a CRD `regarding` reaches the
// cluster, and that the node reconciler calls this path at all. Those are the four things this suite
// is for, because each is a failure an operator sees as "my zone label is not on the node".
var _ = Describe("Node labels applied on join", func() {
	const group = "envtest-node-labels"

	var broadcaster events.EventBroadcaster

	var slotSeq int

	BeforeEach(func() {
		clientset, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		broadcaster = events.NewBroadcaster(&events.EventSinkImpl{Interface: clientset.EventsV1()})
		Expect(broadcaster.StartRecordingToSinkWithContext(ctx)).To(Succeed())

		createIgnoreExists(&infrav2alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: group, Namespace: NamespaceConfig.Get()},
			Spec: infrav2alpha1.GroupSpec{
				MinSize:                       0,
				ScaleDownUtilizationThreshold: "0.5",
				ScaleDownUnneededTime:         &metav1.Duration{Duration: 10 * time.Minute},
				ScaleDownUnreadyTime:          &metav1.Duration{Duration: 20 * time.Minute},
				MaxNodeProvisionTime:          &metav1.Duration{Duration: 15 * time.Minute},
			},
		})
	})

	AfterEach(func() {
		if broadcaster != nil {
			broadcaster.Shutdown()
		}
	})

	It("stamps the declaration on the node that just joined, re-asserts drift, and takes back a dropped key", func() {
		slotSeq++
		name := fmt.Sprintf("labels-slot-%d", slotSeq)

		slot := createSlotWithNode(name,
			map[string]string{"topology.kubernetes.io/zone": "fsn1", "pool.homecluster.dev/rack": "3"},
			map[string]string{"pool.homecluster.dev/serial": "SN-0001"})

		reconciler := &NodeReconciler{
			Client:        k8sClient,
			Scheme:        scheme.Scheme,
			EventRecorder: broadcaster.NewRecorder(scheme.Scheme, "node-controller"),
		}

		By("writing the labels and annotations the slot declares onto the node object")
		pass(reconciler, slot)

		node := readKubeNode(name)
		Expect(node.Labels).To(HaveKeyWithValue("topology.kubernetes.io/zone", "fsn1"),
			"the label a pod's nodeSelector matches must be on the node, and it must be there before the scheduler binds a pod to the machine")
		Expect(node.Labels).To(HaveKeyWithValue("pool.homecluster.dev/rack", "3"))
		Expect(node.Annotations).To(HaveKeyWithValue("pool.homecluster.dev/serial", "SN-0001"))
		Expect(node.Labels).To(HaveKeyWithValue("kubernetes.io/hostname", name),
			"a label the kubelet owns, and this slot never declared, is untouched")
		Expect(node.Annotations).To(HaveKeyWithValue(nodelabels.AppliedLabelsAnnotation,
			"pool.homecluster.dev/rack,topology.kubernetes.io/zone"),
			"the node carries the record of the keys the slot wrote, which is what makes removal possible")

		event := awaitSlotEvent(slot, nodelabels.ReasonApplied)
		Expect(event.Type).To(Equal(corev1.EventTypeNormal))
		Expect(event.Action).To(Equal(nodelabels.ActionApplyNodeLabels))
		Expect(event.Note).To(ContainSubstring("+pool.homecluster.dev/rack"))
		Expect(event.Note).To(ContainSubstring("+topology.kubernetes.io/zone"))
		Expect(event.Note).To(ContainSubstring("+pool.homecluster.dev/serial"))

		By("writing nothing on a pass that finds the node already matching")
		before := appliedEventCount(slot)
		pass(reconciler, slot)
		pass(reconciler, slot)

		Expect(appliedEventCount(slot)).To(Equal(before),
			"an unchanged resync of a settled slot patches the node object, and an Event per pass buries the one that reported a real change")

		By("re-asserting the declaration after drift on the node object")
		// The two shapes drift takes: an operator's `kubectl label node <node> zone=hel1
		// --overwrite`, and a key removed - a rebooted kubelet that stopped stamping it, a node
		// object recreated by a re-registration. The declaration is the intent, so the next pass puts
		// both back.
		node = readKubeNode(name)
		delete(node.Labels, "pool.homecluster.dev/rack")
		node.Labels["topology.kubernetes.io/zone"] = "hel1"
		Expect(k8sClient.Update(ctx, node)).To(Succeed())

		pass(reconciler, slot)

		after := readKubeNode(name)
		Expect(after.Labels).To(HaveKeyWithValue("topology.kubernetes.io/zone", "fsn1"),
			"a value changed by hand on the node is put back on the next reconcile")
		Expect(after.Labels).To(HaveKeyWithValue("pool.homecluster.dev/rack", "3"),
			"a label the slot declares and the node lost is put back on the next reconcile")
		Expect(appliedEventCount(slot)).To(Equal(before+1), "the write that fixed the drift is one Event")

		By("removing a key from the node when the declaration drops it")
		// The Git apply: the key leaves the file, so it leaves the machine. Only that key: the
		// kubelet's hostname, and every label the slot never wrote, stay.
		fresh := &infrav2alpha1.Node{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(slot), fresh)).To(Succeed())
		fresh.Spec.NodeLabels = map[string]string{"topology.kubernetes.io/zone": "fsn1"}
		fresh.Spec.NodeAnnotations = nil
		Expect(k8sClient.Update(ctx, fresh)).To(Succeed())

		pass(reconciler, fresh)

		removed := readKubeNode(name)
		Expect(removed.Labels).NotTo(HaveKey("pool.homecluster.dev/rack"),
			"a key the declaration dropped comes off the node on the next reconcile")
		Expect(removed.Labels).To(HaveKeyWithValue("topology.kubernetes.io/zone", "fsn1"))
		Expect(removed.Labels).To(HaveKeyWithValue("kubernetes.io/hostname", name))
		Expect(removed.Annotations).NotTo(HaveKey("pool.homecluster.dev/serial"),
			"the same rule for annotations")
		Expect(removed.Annotations).To(HaveKeyWithValue(nodelabels.AppliedLabelsAnnotation,
			"topology.kubernetes.io/zone"), "the record follows the declaration")
		Expect(removed.Annotations).NotTo(HaveKey(nodelabels.AppliedAnnotationsAnnotation),
			"a slot that declares no annotations leaves no record behind")

		By("leaving the lifecycle of the slot alone: the label pass is a write to the node object, not a transition")
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(NamespaceConfig.Get()))).To(Succeed())

		for i := range jobs.Items {
			Expect(jobs.Items[i].Labels[fsm.LabelNodeCR]).NotTo(Equal(name),
				"a slot with no live claim and a machine observed off must not start a lifecycle Job because a label was applied")
		}
	})
})
