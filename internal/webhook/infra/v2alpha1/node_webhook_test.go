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
	"fmt"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
)

const (
	testNamespace = "homelab-autoscaler-system"
	testGroup     = "remote-workers"
	testNodeName  = "omni-remote-2"
)

// nodeUnderTest returns a Node CR: a slot, declared by Git. In v2alpha1 its spec carries no
// runtime intent at all, so there is no power state for a test to set - the referenced
// Kubernetes node is deliberately one that does not exist in the fake cluster, which is the
// state of every slot of an idle pool, and of a slot whose machine has not joined yet.
func nodeUnderTest(labels map[string]string) *infrav2alpha1.Node {
	return &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNodeName,
			Namespace: testNamespace,
			Labels:    labels,
		},
		Spec: infrav2alpha1.NodeSpec{
			KubernetesNodeName: testNodeName,
			Pricing: infrav2alpha1.PricingSpec{
				HourlyRate: "0",
				PodRate:    "0",
			},
		},
	}
}

func groupLabels() map[string]string {
	return map[string]string{"group": testGroup}
}

// entries builds a map of n distinct, valid keys, for the rows that assert the bound on how many
// labels, and how many annotations, one slot may declare.
func entries(n int) map[string]string {
	out := make(map[string]string, n)
	for i := range n {
		out[fmt.Sprintf("pool.homecluster.dev/k%d", i)] = "v"
	}

	return out
}

var _ = Describe("Node Webhook", func() {
	var (
		obj          *infrav2alpha1.Node
		oldObj       *infrav2alpha1.Node
		validator    NodeCustomValidator
		k8sClient    client.Client
		testScheme   *runtime.Scheme
		clusterGroup *infrav2alpha1.Group
	)

	BeforeEach(func() {
		testScheme = runtime.NewScheme()
		Expect(infrav2alpha1.AddToScheme(testScheme)).To(Succeed())
		Expect(corev1.AddToScheme(testScheme)).To(Succeed())

		// The cluster contains the Group CR and a Kubernetes node for a slot that is
		// already running, but no Kubernetes node for the slot under test.
		clusterGroup = &infrav2alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: testGroup, Namespace: testNamespace},
		}
		runningNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "omni-remote-1"},
		}
		k8sClient = fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(clusterGroup, runningNode).
			Build()

		validator = NodeCustomValidator{Client: k8sClient}
		Expect(validator.Client).NotTo(BeNil(), "Expected validator client to be initialized")

		obj = nodeUnderTest(groupLabels())
		oldObj = obj.DeepCopy()
	})

	Context("When creating a Node, the declaration of a slot", func() {
		It("Should admit a slot whose Kubernetes node does not exist yet", func() {
			warnings, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).NotTo(HaveOccurred(), "a slot of an idle pool has no machine and no node object, and it must be declarable")
			Expect(warnings).NotTo(BeEmpty(), "the missing node is reported, never denied")
		})

		It("Should still require the group label", func() {
			obj.Labels = nil

			_, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("group"))
		})

		It("Should still reject an unknown group", func() {
			obj.Labels = map[string]string{"group": "does-not-exist"}

			_, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does-not-exist"))
		})
	})

	Context("When creating a Node whose machine has not joined", func() {
		It("Should admit the slot and warn about the missing Kubernetes node", func() {
			warnings, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).NotTo(HaveOccurred(),
				"a scale-up boots the machine and the kubelet registers the node minutes later, so a missing node must never deny the declaration")
			Expect(warnings).To(HaveLen(1))
			Expect(warnings[0]).To(ContainSubstring(
				"kubernetes node \"" + testNodeName + "\" has not joined the cluster yet"))
			Expect(warnings[0]).To(ContainSubstring("kubelet registers it when the machine boots"))
		})

		// The guarantee behind the fix: a webhook that cannot read the cluster must not turn
		// that into a denial, otherwise every apply of a pool fails with a transport error.
		It("Should admit a slot whose node lookup fails, without warnings", func() {
			lookupFailed := false
			failingClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithObjects(clusterGroup).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(callCtx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
						// Only the corev1.Node lookup under test fails, the Group lookup succeeds.
						if _, ok := o.(*corev1.Node); ok {
							lookupFailed = true
							return apierrors.NewInternalError(fmt.Errorf("injected lookup failure for %q", key.Name))
						}
						return c.Get(callCtx, key, o, opts...)
					},
				}).
				Build()

			failingValidator := NodeCustomValidator{Client: failingClient}

			warnings, err := failingValidator.ValidateCreate(context.Background(), obj)
			Expect(lookupFailed).To(BeTrue(), "expected the corev1.Node lookup to be intercepted")
			Expect(err).NotTo(HaveOccurred(),
				"a lookup failure must never deny admission, the operator reconciles the state anyway")
			Expect(warnings).To(BeNil(), "a lookup failure must not fabricate warnings about the node")

			warnings, err = failingValidator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"a lookup failure must never deny admission on the update path either")
			Expect(warnings).To(BeNil())
		})

		It("Should admit a slot whose Kubernetes node exists, without warnings", func() {
			obj.Spec.KubernetesNodeName = "omni-remote-1"

			warnings, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeNil())
		})
	})

	Context("When updating a Node, which is a Git apply", func() {
		It("Should admit an apply that changes the configuration of a never-provisioned slot", func() {
			obj.Spec.Pricing.HourlyRate = "0.07"

			_, err := validator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit an apply over a slot whose Kubernetes node is gone", func() {
			// The scale-down path: the machine is powered off, the node object is deleted,
			// and the declaration in Git is unchanged. An apply must never be denied for it.
			_, err := validator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should still require the group label", func() {
			obj.Labels = map[string]string{}

			_, err := validator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("group"))
		})

		It("Should admit a re-apply whose Kubernetes node does not exist, warning about it", func() {
			warnings, err := validator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"the autoscaler never writes a Node spec in v2alpha1, and a slot is declared before its machine ever joins: admission must not deny")
			Expect(warnings).To(HaveLen(1))
			Expect(warnings[0]).To(ContainSubstring(
				"kubernetes node \"" + testNodeName + "\" has not joined the cluster yet"))
		})
	})

	// The table of what a slot may declare about its node object.
	//
	// Every refusal is asserted to name the offending key, because the manifest an operator applies
	// carries many keys and the only useful admission message is the one that points at the entry.
	// The reserved rows are one per prefix on purpose: each prefix has its own owner and its own
	// reason, and a rule that refuses the whole list is only trustworthy if each entry is shown to
	// be in it.
	Context("When a slot declares nodeLabels and nodeAnnotations", func() {
		DescribeTable("the declaration is admitted, or refused naming the key",
			func(labels, annotations map[string]string, wantDenied string) {
				obj.Spec.NodeLabels = labels
				obj.Spec.NodeAnnotations = annotations

				// The create path is a Git apply of a new slot, the update path is a Git apply over
				// an existing one: both carry the same declaration, so both must answer the same.
				_, createErr := validator.ValidateCreate(context.Background(), obj)
				_, updateErr := validator.ValidateUpdate(context.Background(), oldObj, obj)

				if wantDenied == "" {
					Expect(createErr).NotTo(HaveOccurred())
					Expect(updateErr).NotTo(HaveOccurred())

					return
				}

				for _, err := range []error{createErr, updateErr} {
					Expect(err).To(HaveOccurred(), "the declaration must be refused at admission")
					Expect(err.Error()).To(ContainSubstring(wantDenied))
				}
			},

			// The motivating case, and the shape of a working declaration: the zone CA matches a
			// zoned pod against, a region, a rack in the operator's own namespace, and a plain key.
			Entry("admits the labels that make a zoned pod schedulable",
				map[string]string{
					"topology.kubernetes.io/zone":   "fsn1",
					"topology.kubernetes.io/region": "eu-central-1",
					"pool.homecluster.dev/rack":     "rack-3",
					"dedicated":                     "remote",
				}, nil, ""),

			Entry("admits annotations in any namespace Kubernetes does not own",
				nil,
				map[string]string{
					"power.homecluster.dev/serial": "SN-0001",
					// node.kubernetes.io/ and topology.kubernetes.io/ are reserved for *labels*,
					// where a Kubernetes component stamps and rotates them. As annotations they
					// have no owner writing them, so a note there is the operator's: the row pins
					// that judgement down.
					"node.kubernetes.io/maintenance": "true",
					"topology.kubernetes.io/note":    "rack 3, top of frame",
				}, ""),

			Entry("admits a slot that declares nothing, which is every slot written before this field",
				nil, nil, ""),

			Entry("admits an empty label value, which is a valid label value",
				map[string]string{"pool.homecluster.dev/ssd": ""}, nil, ""),

			Entry("refuses a label key that is not a qualified name",
				map[string]string{"Bad Key": "value"}, nil, "spec.nodeLabels[Bad Key]"),

			Entry("refuses a label key with an empty prefix",
				map[string]string{"/bare": "value"}, nil, "spec.nodeLabels[/bare]"),

			Entry("refuses a label key with two slashes",
				map[string]string{"pool.homecluster.dev/a/b": "value"}, nil, "spec.nodeLabels[pool.homecluster.dev/a/b]"),

			Entry("refuses a label value that is not a valid label value",
				map[string]string{"rack": "not a value!"}, nil, "a valid label must be an empty string"),

			Entry("refuses a label value longer than 63 characters",
				map[string]string{"rack": strings.Repeat("x", 64)}, nil, "must be no more than 63 bytes"),

			// One row per reserved prefix, with the owner in the message.
			Entry("refuses the operator's own namespace",
				map[string]string{"infra.homecluster.dev/group": "worker"}, nil,
				"spec.nodeLabels[infra.homecluster.dev/group]"),

			Entry("refuses the kubelet and node lifecycle namespace",
				map[string]string{"node.kubernetes.io/role": "agent"}, nil,
				"spec.nodeLabels[node.kubernetes.io/role]"),

			Entry("refuses the kubernetes.io namespace",
				map[string]string{"kubernetes.io/hostname": "worker-01"}, nil,
				"spec.nodeLabels[kubernetes.io/hostname]"),

			Entry("refuses the node role namespace",
				map[string]string{"node-role.kubernetes.io/control-plane": ""}, nil,
				"spec.nodeLabels[node-role.kubernetes.io/control-plane]"),

			Entry("refuses the rest of the topology namespace",
				map[string]string{"topology.kubernetes.io/zone-labels": "fsn1"}, nil,
				"spec.nodeLabels[topology.kubernetes.io/zone-labels]"),

			// The exceptions, asserted as entries of their own: the refusal above must not swallow
			// the two keys the whole feature is about.
			Entry("allows topology.kubernetes.io/zone, the key a zone nodeSelector matches",
				map[string]string{"topology.kubernetes.io/zone": "fsn1"}, nil, ""),

			Entry("allows topology.kubernetes.io/region",
				map[string]string{"topology.kubernetes.io/region": "eu-central-1"}, nil, ""),

			Entry("says who owns a reserved key, not just that it is refused",
				map[string]string{"kubernetes.io/arch": "amd64"}, nil, "reserved key, not writable from a slot"),

			Entry("refuses the operator's annotation namespace, records included",
				nil, map[string]string{"infra.homecluster.dev/applied-node-labels": "rack"},
				"spec.nodeAnnotations[infra.homecluster.dev/applied-node-labels]"),

			Entry("refuses Argo CD's annotations",
				nil, map[string]string{"argocd.argoproj.io/sync-wave": "1"},
				"spec.nodeAnnotations[argocd.argoproj.io/sync-wave]"),

			Entry("refuses kubectl's annotations",
				nil, map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
				"spec.nodeAnnotations[kubectl.kubernetes.io/last-applied-configuration]"),

			Entry("refuses an annotation key that is not a qualified name",
				nil, map[string]string{"not a key": "value"}, "spec.nodeAnnotations[not a key]"),

			// A label key is case-sensitive and its prefix must be a lowercase subdomain, so a
			// capitalised namespace is refused as a malformed key before the reserved list is
			// consulted. Annotation keys are the other way round: compared in lower case.
			Entry("refuses a label key whose prefix is not a lowercase subdomain",
				map[string]string{"Kubernetes.io/hostname": "worker"}, nil,
				"spec.nodeLabels[Kubernetes.io/hostname]"),

			// The bounds. A CRD expresses maxProperties for the count; the size of the annotation
			// map is the API server's own limit on the node object, and a Node CR that exceeds it is
			// a slot whose labels never land.
			Entry("refuses more labels than a slot may declare",
				entries(65), nil, "spec.nodeLabels: Too many: 65: must have at most 64 items"),

			Entry("refuses more annotations than a slot may declare",
				nil, entries(65), "spec.nodeAnnotations: Too many: 65: must have at most 64 items"),

			Entry("refuses an annotation map larger than the node object may carry",
				nil, map[string]string{"pool.homecluster.dev/notes": strings.Repeat("x", 256*1024+1)},
				"larger than limit"),
		)
	})
})

// warningRecorder captures the warning headers the API server returns to a client. The
// warnings produced by a validating webhook are surfaced to clients as HTTP Warning
// headers with code 299, which is how the real admission path is asserted on.
type warningRecorder struct {
	mu       sync.Mutex
	warnings []string
}

func (w *warningRecorder) HandleWarningHeader(code int, agent, text string) {
	if code != 299 {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	w.warnings = append(w.warnings, text)
}

func (w *warningRecorder) drain() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	got := w.warnings
	w.warnings = nil

	return got
}

// newWarningCapturingClient builds a client that records the warnings returned by the API
// server, so that tests can assert on the warnings emitted by the deployed webhook.
func newWarningCapturingClient(recorder *warningRecorder) (client.Client, error) {
	testCfg := rest.CopyConfig(cfg)
	testCfg.WarningHandler = recorder

	return client.New(testCfg, client.Options{Scheme: scheme.Scheme})
}

var _ = Describe("Node Webhook through the API server", Ordered, func() {
	const (
		envtestGroup     = "envtest-workers"
		envtestSlot      = "envtest-slot-1"
		envtestNamespace = "default"
	)

	var (
		rec       *warningRecorder
		advClient client.Client
		nodeKey   client.ObjectKey
	)

	BeforeAll(func() {
		rec = &warningRecorder{}

		var err error
		advClient, err = newWarningCapturingClient(rec)
		Expect(err).NotTo(HaveOccurred())

		group := &infrav2alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: envtestGroup, Namespace: envtestNamespace},
			Spec: infrav2alpha1.GroupSpec{
				ScaleDownUtilizationThreshold:    "0.5",
				ScaleDownGpuUtilizationThreshold: "0.5",
				ScaleDownUnneededTime:            &metav1.Duration{Duration: 10 * time.Minute},
				ScaleDownUnreadyTime:             &metav1.Duration{Duration: 20 * time.Minute},
				MaxNodeProvisionTime:             &metav1.Duration{Duration: 30 * time.Minute},
			},
		}
		Expect(advClient.Create(ctx, group)).To(Succeed())

		nodeKey = client.ObjectKey{Name: envtestSlot, Namespace: envtestNamespace}
	})

	It("admits the declaration of a never-provisioned slot, and warns about the node", func() {
		node := &infrav2alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      envtestSlot,
				Namespace: envtestNamespace,
				Labels:    map[string]string{"group": envtestGroup},
			},
			Spec: infrav2alpha1.NodeSpec{
				KubernetesNodeName: envtestSlot,
				Pricing: infrav2alpha1.PricingSpec{
					HourlyRate: "0",
					PodRate:    "0",
				},
			},
		}

		// The validator reads the Group through the manager cache, which needs to observe
		// it before the request is admitted, so retry until the webhook admits the create.
		Eventually(func() error {
			err := advClient.Create(ctx, node)
			if apierrors.IsAlreadyExists(err) {
				return nil
			}

			return err
		}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

		warnings := rec.drain()
		Expect(warnings).To(ContainElement(
			ContainSubstring("kubernetes node \""+envtestSlot+"\" has not joined the cluster yet")),
			"a slot of an idle pool has no machine: that is the normal shape, so it is a warning, never a denial")
	})

	It("admits a re-apply of the slot, which writes no intent, and warns again", func() {
		rec.drain()

		// The v1alpha1 failure this model removes: an apply that re-wrote the runtime intent
		// under the autoscaler's write, and a webhook that could deny it. In v2alpha1 a spec
		// apply is configuration only, and the admission answer to a slot whose machine has not
		// joined is a warning.
		Eventually(func() error {
			fresh := &infrav2alpha1.Node{}
			if err := advClient.Get(ctx, nodeKey, fresh); err != nil {
				return err
			}

			fresh.Spec.Pricing.HourlyRate = "0.07"

			return advClient.Update(ctx, fresh)
		}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

		updated := &infrav2alpha1.Node{}
		Expect(advClient.Get(ctx, nodeKey, updated)).To(Succeed())
		Expect(updated.Spec.Pricing.HourlyRate).To(Equal("0.07"))

		k8sNode := &corev1.Node{}
		Expect(apierrors.IsNotFound(advClient.Get(ctx, client.ObjectKey{Name: envtestSlot}, k8sNode))).To(BeTrue())

		warnings := rec.drain()
		Expect(warnings).To(ContainElement(
			ContainSubstring("kubelet registers it when the machine boots")),
			"the missing Kubernetes node is reported on the update path too")
	})

	It("denies a slot with no group label through the deployed webhook", func() {
		rec.drain()

		node := &infrav2alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "envtest-slot-no-group",
				Namespace: envtestNamespace,
			},
			Spec: infrav2alpha1.NodeSpec{
				KubernetesNodeName: "envtest-slot-no-group",
				Pricing:            infrav2alpha1.PricingSpec{HourlyRate: "0", PodRate: "0"},
			},
		}

		err := advClient.Create(ctx, node)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("group"))
	})

	// The reserved-key rules, on the admission path an operator actually takes: a deployed webhook,
	// a real API server, a client that reads the message `kubectl apply` prints. The table above
	// asserts the validator's decisions; this asserts that a refusal reaches the operator, naming
	// the key, and that the key a zoned pool needs is not caught by the same rule.
	It("denies a slot whose nodeLabels declare a key Kubernetes owns", func() {
		rec.drain()

		node := &infrav2alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "envtest-slot-reserved-labels",
				Namespace: envtestNamespace,
				Labels:    map[string]string{"group": envtestGroup},
			},
			Spec: infrav2alpha1.NodeSpec{
				KubernetesNodeName: "envtest-slot-reserved-labels",
				Pricing:            infrav2alpha1.PricingSpec{HourlyRate: "0", PodRate: "0"},
				NodeLabels: map[string]string{
					"kubernetes.io/hostname":  "spoofed",
					"node.kubernetes.io/role": "agent",
				},
				NodeAnnotations: map[string]string{"argocd.argoproj.io/sync-wave": "1"},
			},
		}

		err := advClient.Create(ctx, node)
		Expect(err).To(HaveOccurred(), "a slot that declares a reserved key is refused at apply time")

		// Every refused key is reported, in one message: a manifest with twenty labels names the
		// offender, it does not hide one behind another.
		for _, key := range []string{
			"spec.nodeLabels[kubernetes.io/hostname]",
			"spec.nodeLabels[node.kubernetes.io/role]",
			"spec.nodeAnnotations[argocd.argoproj.io/sync-wave]",
		} {
			Expect(err.Error()).To(ContainSubstring(key))
		}

		Expect(err.Error()).To(ContainSubstring("reserved key, not writable from a slot"))
		Expect(err.Error()).To(ContainSubstring("topology.kubernetes.io/zone"),
			"the refusal says what to declare instead, not only what is forbidden")
	})

	It("admits a slot that declares a zone, the key the whole feature is about", func() {
		rec.drain()

		node := &infrav2alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "envtest-slot-zoned",
				Namespace: envtestNamespace,
				Labels:    map[string]string{"group": envtestGroup},
			},
			Spec: infrav2alpha1.NodeSpec{
				KubernetesNodeName: "envtest-slot-zoned",
				Pricing:            infrav2alpha1.PricingSpec{HourlyRate: "0", PodRate: "0"},
				NodeLabels: map[string]string{
					"topology.kubernetes.io/zone":   "fsn1",
					"topology.kubernetes.io/region": "eu-central-1",
					"pool.homecluster.dev/rack":     "rack-3",
				},
				NodeAnnotations: map[string]string{"pool.homecluster.dev/serial": "SN-0001"},
			},
		}

		// The validator reads the Group through the manager's cache, which has to observe it first.
		Eventually(func() error {
			err := advClient.Create(ctx, node)
			if apierrors.IsAlreadyExists(err) {
				return nil
			}

			return err
		}).WithTimeout(30*time.Second).WithPolling(500*time.Millisecond).Should(Succeed(),
			"a zoned slot is the configuration the scheduler and cluster-autoscaler need: admission must take it")

		stored := &infrav2alpha1.Node{}
		Expect(advClient.Get(ctx, client.ObjectKey{Name: node.Name, Namespace: envtestNamespace}, stored)).To(Succeed())
		Expect(stored.Spec.NodeLabels).To(HaveKeyWithValue("topology.kubernetes.io/zone", "fsn1"),
			"the CRD schema carries the field, so the declaration survives the API server")
		Expect(stored.Spec.NodeAnnotations).To(HaveKeyWithValue("pool.homecluster.dev/serial", "SN-0001"))
	})
})
