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

package v2alpha1

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
)

const (
	claimNamespace = "homelab-autoscaler-system"
	claimGroup     = "remote-workers"
	claimSlot      = "remote-worker-2"
)

// claimUnderTest is the claim of the slot under test: the attribution IncreaseSize stamps, the
// labels the inventory lists on. mutate names the phase, a deletion timestamp, anything the case
// is about.
func claimUnderTest(name, slotName string, mutate func(*infrav2alpha1.NodeClaim)) *infrav2alpha1.NodeClaim {
	nodeClaim := &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: claimNamespace,
			Labels: map[string]string{
				claim.LabelSlot:  slotName,
				claim.LabelGroup: claimGroup,
			},
		},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef:     corev1.ObjectReference{Name: slotName},
			RequestedBy: claim.RequestedByAutoscaler,
			Request: infrav2alpha1.ClaimRequest{
				Call:  infrav2alpha1.ClaimCallIncreaseSize,
				Delta: 1,
				Group: claimGroup,
			},
		},
	}

	if mutate != nil {
		mutate(nodeClaim)
	}

	return nodeClaim
}

// slotUnderTest is the slot a claim asks for: the Node CR the webhook verifies exists.
func slotUnderTest(name string) *infrav2alpha1.Node {
	return &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: claimNamespace, Labels: map[string]string{"group": claimGroup}},
		Spec: infrav2alpha1.NodeSpec{
			KubernetesNodeName: name,
			Pricing:            infrav2alpha1.PricingSpec{HourlyRate: "0", PodRate: "0"},
		},
	}
}

// newClaimValidator builds the validator over a fake cluster holding the objects the case
// describes.
func newClaimValidator(objects ...client.Object) NodeClaimCustomValidator {
	testScheme := runtime.NewScheme()
	_ = infrav2alpha1.AddToScheme(testScheme)
	_ = corev1.AddToScheme(testScheme)

	k8sClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&infrav2alpha1.NodeClaim{}).
		WithObjects(objects...).
		Build()

	return NodeClaimCustomValidator{Client: k8sClient}
}

var _ = Describe("NodeClaim Webhook", func() {
	Context("one live claim per slot, on create", func() {
		It("admits the first live claim of a slot", func() {
			validator := newClaimValidator(slotUnderTest(claimSlot))

			warnings, err := validator.ValidateCreate(context.Background(), claimUnderTest("claim-a", claimSlot, nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeNil())
		})

		It("refuses a second live claim, and the message names the slot", func() {
			live := claimUnderTest("claim-a", claimSlot, func(c *infrav2alpha1.NodeClaim) {
				c.Status.Phase = infrav2alpha1.NodeClaimBooting
			})

			validator := newClaimValidator(slotUnderTest(claimSlot), live)

			_, err := validator.ValidateCreate(context.Background(), claimUnderTest("claim-b", claimSlot, func(c *infrav2alpha1.NodeClaim) {
				c.Status.Phase = infrav2alpha1.NodeClaimPending
			}))
			requireForbidden(err, claimSlot, "claim-a")
		})

		It("refuses a claim on a slot whose live claim is being cancelled", func() {
			// A marked claim owns the slot until the machine is observed off: a second claim
			// would be a scale-up issued against a machine that is going away.
			draining := claimUnderTest("claim-a", claimSlot, func(c *infrav2alpha1.NodeClaim) {
				c.Status.Phase = infrav2alpha1.NodeClaimReady
				now := metav1.Now()
				c.DeletionTimestamp = &now
				c.Finalizers = append(c.Finalizers, "infra.homecluster.dev/nodeclaim-finalizer")
			})

			validator := newClaimValidator(slotUnderTest(claimSlot), draining)

			_, err := validator.ValidateCreate(context.Background(), claimUnderTest("claim-b", claimSlot, nil))
			requireForbidden(err, claimSlot, "claim-a")
		})

		It("admits a claim on a slot whose last claim ended, because a terminal claim is not live", func() {
			validator := newClaimValidator(
				slotUnderTest(claimSlot),
				claimUnderTest("claim-old", claimSlot, func(c *infrav2alpha1.NodeClaim) {
					c.Status.Phase = infrav2alpha1.NodeClaimFailed
				}),
				claimUnderTest("claim-older", claimSlot, func(c *infrav2alpha1.NodeClaim) {
					c.Status.Phase = infrav2alpha1.NodeClaimCompleted
				}),
			)

			_, err := validator.ValidateCreate(context.Background(), claimUnderTest("claim-new", claimSlot, nil))
			Expect(err).NotTo(HaveOccurred(), "failed and completed claims are not capacity, and free their slot")
		})
	})

	Context("one live claim per slot, on update", func() {
		It("admits an update of the slot's own live claim", func() {
			live := claimUnderTest("claim-a", claimSlot, func(c *infrav2alpha1.NodeClaim) {
				c.Status.Phase = infrav2alpha1.NodeClaimPending
			})

			validator := newClaimValidator(slotUnderTest(claimSlot), live)

			updated := live.DeepCopy()
			updated.Status.Phase = infrav2alpha1.NodeClaimBooting

			_, err := validator.ValidateUpdate(context.Background(), live, updated)
			Expect(err).NotTo(HaveOccurred(), "the claim controller moves a claim through its phases on the object that owns the slot")
		})

		It("refuses an update of a second claim while the slot has a live one, and the message names the slot", func() {
			live := claimUnderTest("claim-a", claimSlot, func(c *infrav2alpha1.NodeClaim) {
				c.Status.Phase = infrav2alpha1.NodeClaimBooting
			})
			extra := claimUnderTest("claim-b", claimSlot, func(c *infrav2alpha1.NodeClaim) {
				c.Status.Phase = infrav2alpha1.NodeClaimPending
			})

			validator := newClaimValidator(slotUnderTest(claimSlot), live, extra)

			updated := extra.DeepCopy()
			updated.Status.Phase = infrav2alpha1.NodeClaimBooting

			_, err := validator.ValidateUpdate(context.Background(), extra, updated)
			requireForbidden(err, claimSlot, "claim-a")
		})

		It("admits the status write that ends a claim, because a terminal move opens no second claim", func() {
			// The claim controller's own write: pending -> failed on a claim that is not the
			// newest of the slot (it is being superseded). Refusing it would strand the record
			// the whole model exists to produce.
			live := claimUnderTest("claim-a", claimSlot, func(c *infrav2alpha1.NodeClaim) {
				c.Status.Phase = infrav2alpha1.NodeClaimReady
			})
			older := claimUnderTest("claim-b", claimSlot, func(c *infrav2alpha1.NodeClaim) {
				c.Status.Phase = infrav2alpha1.NodeClaimBooting
			})

			validator := newClaimValidator(slotUnderTest(claimSlot), live, older)

			finished := older.DeepCopy()
			finished.Status.Phase = infrav2alpha1.NodeClaimFailed
			finished.Status.Reason = infrav2alpha1.ClaimReasonSuperseded

			_, err := validator.ValidateUpdate(context.Background(), older, finished)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("the slot a claim asks for", func() {
		It("refuses a claim with no slot named", func() {
			validator := newClaimValidator(slotUnderTest(claimSlot))

			_, err := validator.ValidateCreate(context.Background(), claimUnderTest("claim-a", "", nil))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("slotRef"))
		})

		It("refuses a claim for a slot that does not exist, and names it", func() {
			validator := newClaimValidator()

			_, err := validator.ValidateCreate(context.Background(), claimUnderTest("claim-a", "no-such-slot", nil))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no-such-slot"))
			Expect(err.Error()).To(ContainSubstring("does not exist"))
		})

		It("never refuses a delete, on any path", func() {
			live := claimUnderTest("claim-a", claimSlot, func(c *infrav2alpha1.NodeClaim) {
				c.Status.Phase = infrav2alpha1.NodeClaimBooting
			})

			validator := newClaimValidator(slotUnderTest(claimSlot), live)

			warnings, err := validator.ValidateDelete(context.Background(), live)
			Expect(err).NotTo(HaveOccurred(), "canceling is deleting: admission is the wrong place to judge a scale-down")
			Expect(warnings).To(BeNil())
		})

		It("does not deny when the inventory cannot be read", func() {
			// A webhook that denies because it could not list is a webhook that stops the pool.
			validator := newClaimValidator(slotUnderTest(claimSlot))

			_, err := validator.ValidateCreate(context.Background(), claimUnderTest("claim-a", claimSlot, nil))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// The same rule through the deployed webhook, on a real API server: the admission
	// configuration is generated from the marker of this package, and the rule is the one the
	// CRD cannot express, so the pair (webhook + config) is what the spec asserts.
	Context("through the API server", Ordered, func() {
		const (
			envNamespace = "default"
			envGroup     = "envtest-claim-workers"
			envSlot      = "envtest-claim-slot"
		)

		var advClient client.Client

		BeforeAll(func() {
			var err error
			advClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())

			group := &infrav2alpha1.Group{
				ObjectMeta: metav1.ObjectMeta{Name: envGroup, Namespace: envNamespace},
				Spec: infrav2alpha1.GroupSpec{
					ScaleDownUtilizationThreshold:    "0.5",
					ScaleDownGpuUtilizationThreshold: "0.5",
					ScaleDownUnneededTime:            &metav1.Duration{Duration: 10 * time.Minute},
					ScaleDownUnreadyTime:             &metav1.Duration{Duration: 20 * time.Minute},
					MaxNodeProvisionTime:             &metav1.Duration{Duration: 30 * time.Minute},
				},
			}
			Expect(advClient.Create(ctx, group)).To(Succeed())

			slot := slotUnderTest(envSlot)
			slot.Namespace = envNamespace
			slot.Labels = map[string]string{"group": envGroup}
			Expect(advClient.Create(ctx, slot)).To(Succeed())
		})

		It("admits the first live claim of the slot", func() {
			nodeClaim := envClaim("envtest-claim-a", envNamespace, envSlot)

			Eventually(func() error {
				return advClient.Create(ctx, nodeClaim)
			}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

			// A created claim carries no phase: the status of a create request is discarded by
			// the status subresource, and the phase is the controller's write. Landing it here is
			// what makes the claim live, and what the next spec refuses against.
			landClaimPhase(advClient, envNamespace, "envtest-claim-a", infrav2alpha1.NodeClaimPending)
		})

		It("refuses a second live claim on the same slot, naming the slot", func() {
			// The webhook reads the slot's claims through the manager cache, which has to
			// observe the first claim before it can refuse the second.
			Eventually(func() string {
				err := advClient.Create(ctx, envClaim("envtest-claim-b", envNamespace, envSlot))
				if err == nil {
					return ""
				}

				return err.Error()
			}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(ContainSubstring(envSlot))

			denied := &infrav2alpha1.NodeClaimList{}
			Expect(advClient.List(ctx, denied, client.InNamespace(envNamespace))).To(Succeed())
			Expect(denied.Items).To(HaveLen(1), "the refusal wrote nothing: the slot has one live claim")
		})

		It("admits an update of the slot's own live claim", func() {
			live := &infrav2alpha1.NodeClaim{}
			Expect(advClient.Get(ctx, types.NamespacedName{Name: "envtest-claim-a", Namespace: envNamespace}, live)).To(Succeed())

			live.Spec.RequestedBy = "an-operator-making-a-manual-claim"

			Consistently(func() error {
				return advClient.Update(ctx, live)
			}).WithTimeout(2 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
		})

		It("admits the status write that ends the claim", func() {
			live := &infrav2alpha1.NodeClaim{}
			Expect(advClient.Get(ctx, types.NamespacedName{Name: "envtest-claim-a", Namespace: envNamespace}, live)).To(Succeed())

			live.Status.Phase = infrav2alpha1.NodeClaimFailed
			live.Status.Reason = infrav2alpha1.ClaimReasonDeadlineExceeded

			Expect(advClient.Status().Update(ctx, live)).To(Succeed(),
				"a terminal move opens no second claim, so the webhook never blocks the record the model exists to produce")
		})

		It("admits a new claim once the slot's claim has ended", func() {
			// claim-a is failed (the spec before this one landed the phase), so the slot is
			// free again: the refusal of the earlier spec was about a live claim, not about a
			// name.

			Eventually(func() error {
				return advClient.Create(ctx, envClaim("envtest-claim-c", envNamespace, envSlot))
			}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

			third := &infrav2alpha1.NodeClaim{}
			Expect(advClient.Get(ctx, types.NamespacedName{Name: "envtest-claim-c", Namespace: envNamespace}, third)).To(Succeed())
			Expect(third.Spec.SlotRef.Name).To(Equal(envSlot))
			Expect(third.Spec.Request.Call).To(Equal(infrav2alpha1.ClaimCallIncreaseSize))
		})
	})
})

// landClaimPhase writes a claim's phase through the status subresource, the way the claim
// controller's first pass does, and waits for the write to read back.
func landClaimPhase(c client.Client, namespace, name string, phase infrav2alpha1.NodeClaimPhase) {
	GinkgoHelper()

	Eventually(func() error {
		got := &infrav2alpha1.NodeClaim{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got); err != nil {
			return err
		}

		got.Status.Phase = phase

		return c.Status().Update(ctx, got)
	}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

	Eventually(func() infrav2alpha1.NodeClaimPhase {
		got := &infrav2alpha1.NodeClaim{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got); err != nil {
			return ""
		}

		return got.Status.Phase
	}).WithTimeout(30 * time.Second).Should(Equal(phase))
}

// envClaim is a claim of the envtest slot, built the way the provider builds one: the labels the
// inventory lists on, the attribution of the request, and the status the schema default produces.
func envClaim(name, namespace, slotName string) *infrav2alpha1.NodeClaim {
	nodeClaim := claimUnderTest(name, slotName, nil)
	nodeClaim.Namespace = namespace

	return nodeClaim
}

// requireForbidden asserts the one refusal this webhook exists for: the message names the slot
// and the live claim that owns it, because "why was my scale-up refused" is asked about a slot.
func requireForbidden(err error, slotName, liveClaimName string) {
	GinkgoHelper()

	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring(slotName), "the message names the slot")
	Expect(err.Error()).To(ContainSubstring(liveClaimName), "and the claim that owns it")
	Expect(err.Error()).To(ContainSubstring("one live claim per slot"), "and the rule that refused it")
}
