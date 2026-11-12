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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
)

// The claim's own timeline, published through a real event recorder against a real API server.
//
// The unit tests of internal/claim assert the set on a recording double: which reason, which type,
// which note. What they cannot assert is that the object the recorder builds reaches the cluster:
// that the `regarding` reference resolves for a CRD type, that the action the new events API
// aggregates on is carried, and that `kubectl describe nodeclaim` therefore has something to show.
// This suite is that assertion, and the recorder is built the way the manager builds it
// (`mgr.GetEventRecorder("nodeclaim-controller")`: a broadcaster over events.k8s.io, a recorder on
// the scheme that knows this repository's types).
var _ = Describe("NodeClaim events", func() {
	const group = "envtest-claims"

	var broadcaster events.EventBroadcaster

	var slotSeq int

	slotName := func() string {
		slotSeq++

		return fmt.Sprintf("events-slot-%d", slotSeq)
	}

	BeforeEach(func() {
		clientset, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		broadcaster = events.NewBroadcaster(&events.EventSinkImpl{Interface: clientset.EventsV1()})
		Expect(broadcaster.StartRecordingToSinkWithContext(ctx)).To(Succeed())

		groupCR := &infrav2alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: group, Namespace: NamespaceConfig.Get()},
			Spec: infrav2alpha1.GroupSpec{
				MinSize:                       0,
				ScaleDownUtilizationThreshold: "0.5",
				ScaleDownUnneededTime:         &metav1.Duration{Duration: 10 * time.Minute},
				ScaleDownUnreadyTime:          &metav1.Duration{Duration: 20 * time.Minute},
				MaxNodeProvisionTime:          &metav1.Duration{Duration: 15 * time.Minute},
			},
		}

		createIgnoreExists(groupCR)
	})

	AfterEach(func() {
		if broadcaster != nil {
			broadcaster.Shutdown()
		}
	})

	It("publishes one Event per phase move on the claim, and nothing for a move that did not land", func() {
		name := slotName()

		slot := &infrav2alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: NamespaceConfig.Get(),
				Labels:    map[string]string{"group": group},
			},
			Spec: infrav2alpha1.NodeSpec{
				KubernetesNodeName: name + "-k8s",
				Pricing:            infrav2alpha1.PricingSpec{HourlyRate: "0.05", PodRate: "0.001"},
				StartupPodSpec:     infrav2alpha1.MinimalPodSpec{Image: "envtest/startup:latest"},
				ShutdownPodSpec:    infrav2alpha1.MinimalPodSpec{Image: "envtest/shutdown:latest"},
			},
		}
		createIgnoreExists(slot)

		// The provider's own write, so the attribution the create event quotes is the one a real
		// IncreaseSize stamps.
		nodeClaim := createProviderClaim(slot)
		key := client.ObjectKeyFromObject(nodeClaim)

		r := claim.NewRecorderWithEvents(k8sClient, broadcaster.NewRecorder(scheme.Scheme, "nodeclaim-controller"))

		By("publishing the create on the claim, with the attribution the request carried")
		_, err := r.Seed(ctx, key, slot)
		Expect(err).NotTo(HaveOccurred())

		created := awaitClaimEvent(nodeClaim, claim.EventClaimCreated)
		Expect(created.Type).To(Equal(corev1.EventTypeNormal))
		Expect(created.Action).To(Equal(claim.ActionClaim), "the action is the key the new events API aggregates on")
		Expect(created.Regarding.Kind).To(Equal("NodeClaim"), "the timeline is on the claim, where describe reads it")
		Expect(created.Regarding.Name).To(Equal(nodeClaim.Name))
		Expect(created.Note).To(ContainSubstring(infrav2alpha1.ClaimCallIncreaseSize))
		Expect(created.Note).To(ContainSubstring("delta +1"))
		Expect(created.Note).To(ContainSubstring(claim.RequestedByAutoscaler))

		By("publishing the commit, then the readiness, and nothing for a move that changed nothing")
		_, err = r.Move(ctx, key, infrav2alpha1.NodeClaimBooting, "")
		Expect(err).NotTo(HaveOccurred())

		booting := awaitClaimEvent(nodeClaim, claim.EventClaimBooting)
		Expect(booting.Type).To(Equal(corev1.EventTypeNormal))
		Expect(booting.Note).To(ContainSubstring("no longer cancelable"))

		_, err = r.Move(ctx, key, infrav2alpha1.NodeClaimReady, "")
		Expect(err).NotTo(HaveOccurred())

		ready := awaitClaimEvent(nodeClaim, claim.EventClaimReady)
		Expect(ready.Note).To(ContainSubstring(slot.Spec.KubernetesNodeName),
			"the readiness gate's answer names the node object that cleared")

		// The pass that re-applies a move that is already true - the recorder is called on every pass
		// of a ready claim's slot - writes nothing, and narrates nothing.
		_, err = r.Move(ctx, key, infrav2alpha1.NodeClaimReady, "")
		Expect(err).NotTo(HaveOccurred())

		// A move the phase table refuses is an error, and no Event: a boot that has started is never
		// cancelled, and the timeline does not claim it was.
		_, err = r.Move(ctx, key, infrav2alpha1.NodeClaimPending, "")
		Expect(err).To(MatchError(claim.ErrIllegalMove))

		Consistently(func() []string {
			return claimEventReasons(nodeClaim)
		}, 3*time.Second, 500*time.Millisecond).Should(HaveLen(3),
			"the create, the commit and the readiness are three events; a re-apply and a refused move add none")

		By("publishing the cancel, naming the request the attempt was")
		_, err = r.Move(ctx, key, infrav2alpha1.NodeClaimCompleted, infrav2alpha1.ClaimReasonDeletedByHand)
		Expect(err).NotTo(HaveOccurred())

		cancelled := awaitClaimEvent(nodeClaim, claim.EventClaimCancelled)
		Expect(cancelled.Type).To(Equal(corev1.EventTypeNormal), "a cancel that landed is not a fault")
		Expect(cancelled.Note).To(ContainSubstring(infrav2alpha1.ClaimReasonDeletedByHand))
		Expect(cancelled.Note).To(ContainSubstring(infrav2alpha1.ClaimCallIncreaseSize))
	})
})

// claimEvents reads the Events published on one claim. The list is the namespace's, filtered in Go
// on the regarding reference: the field selectors of the events API are the API server's business,
// and a read a suite depends on should not be. The recipe with the selectors - the one an operator
// runs - is in docs/troubleshooting/common-issues.md.
func claimEvents(nodeClaim *infrav2alpha1.NodeClaim) []eventsv1.Event {
	list := &eventsv1.EventList{}
	Expect(k8sClient.List(ctx, list, client.InNamespace(nodeClaim.Namespace))).To(Succeed())

	found := []eventsv1.Event{}

	for i := range list.Items {
		nodeEvent := list.Items[i]
		if nodeEvent.Regarding.Kind == "NodeClaim" && nodeEvent.Regarding.Name == nodeClaim.Name {
			found = append(found, nodeEvent)
		}
	}

	return found
}

// claimEventReasons is the reason of every Event published on a claim, for a count assertion.
func claimEventReasons(nodeClaim *infrav2alpha1.NodeClaim) []string {
	published := claimEvents(nodeClaim)
	reasons := make([]string, 0, len(published))

	for _, nodeEvent := range published {
		reasons = append(reasons, nodeEvent.Reason)
	}

	return reasons
}

// awaitClaimEvent waits for one Event of a reason on a claim and returns it: the broadcaster writes
// asynchronously, so a move and the object it produces are a poll apart.
func awaitClaimEvent(nodeClaim *infrav2alpha1.NodeClaim, reason string) eventsv1.Event {
	GinkgoHelper()

	var found *eventsv1.Event

	Eventually(func() string {
		published := claimEvents(nodeClaim)
		for i := range published {
			if published[i].Reason == reason {
				found = &published[i]

				return ""
			}
		}

		return fmt.Sprintf("claim %s carries %v, and no %s", nodeClaim.Name, claimEventReasons(nodeClaim), reason)
	}, 10*time.Second, 250*time.Millisecond).Should(BeEmpty())

	Expect(found).NotTo(BeNil())

	return *found
}
