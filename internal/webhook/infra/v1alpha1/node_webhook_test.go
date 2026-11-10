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

package v1alpha1

import (
	"context"
	"fmt"
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

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

const (
	testNamespace = "homelab-autoscaler-system"
	testGroup     = "remote-workers"
	testNodeName  = "omni-remote-2"
)

// nodeUnderTest returns a Node CR for a slot of the given power state. The referenced
// Kubernetes node is deliberately one that does not exist in the fake cluster, which is
// the state of a powered off slot that has never been provisioned.
func nodeUnderTest(powerState infrav1alpha1.PowerState, labels map[string]string) *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNodeName,
			Namespace: testNamespace,
			Labels:    labels,
		},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  powerState,
			KubernetesNodeName: testNodeName,
			Pricing: infrav1alpha1.PricingSpec{
				HourlyRate: "0",
				PodRate:    "0",
			},
		},
	}
}

func groupLabels() map[string]string {
	return map[string]string{"group": testGroup}
}

var _ = Describe("Node Webhook", func() {
	var (
		obj          *infrav1alpha1.Node
		oldObj       *infrav1alpha1.Node
		validator    NodeCustomValidator
		k8sClient    client.Client
		testScheme   *runtime.Scheme
		clusterGroup *infrav1alpha1.Group
	)

	BeforeEach(func() {
		testScheme = runtime.NewScheme()
		Expect(infrav1alpha1.AddToScheme(testScheme)).To(Succeed())
		Expect(corev1.AddToScheme(testScheme)).To(Succeed())

		// The cluster contains the Group CR and a Kubernetes node for a slot that is
		// already running, but no Kubernetes node for the slot under test.
		clusterGroup = &infrav1alpha1.Group{
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

		obj = nodeUnderTest(infrav1alpha1.PowerStateOff, groupLabels())
		oldObj = obj.DeepCopy()
	})

	Context("When creating a Node with a power state of off", func() {
		It("Should admit a slot whose Kubernetes node does not exist yet", func() {
			warnings, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).NotTo(HaveOccurred(), "powered off slots must be declarable before the machine exists")
			Expect(warnings).To(BeNil())
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

	Context("When creating a Node with a power state of on", func() {
		It("Should admit a slot whose Kubernetes node does not exist, warning about it", func() {
			obj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn

			warnings, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).NotTo(HaveOccurred(),
				"the power-on path writes powerState on before the machine joins, so a missing node must never deny")
			Expect(warnings).To(HaveLen(1))
			Expect(warnings[0]).To(ContainSubstring(
				"kubernetes node \"" + testNodeName + "\" has not joined the cluster yet"))
			Expect(warnings[0]).To(ContainSubstring("kubelet registers it when the machine boots"))
		})

		// The guarantee behind the fix: a webhook that cannot read the cluster must not turn
		// that into a denial, otherwise every power-on write fails with a transport error.
		It("Should admit a slot whose node lookup fails, without warnings", func() {
			obj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn

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
			obj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn
			obj.Spec.KubernetesNodeName = "omni-remote-1"

			warnings, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeNil())
		})
	})

	Context("When updating a Node", func() {
		It("Should admit an update that keeps a never-provisioned slot powered off", func() {
			warnings, err := validator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeNil())
		})

		It("Should admit powering off a slot whose Kubernetes node is gone", func() {
			oldObj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn
			obj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOff

			_, err := validator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should still require the group label", func() {
			obj.Labels = map[string]string{}

			_, err := validator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("group"))
		})

		It("Should admit powering on a slot whose Kubernetes node does not exist, warning about it", func() {
			oldObj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOff
			obj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn

			warnings, err := validator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).NotTo(HaveOccurred(),
				"the operator powers slots on before the Kubernetes node joins, admission must not deny")
			Expect(warnings).To(HaveLen(1))
			Expect(warnings[0]).To(ContainSubstring(
				"kubernetes node \"" + testNodeName + "\" has not joined the cluster yet"))
		})
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

		group := &infrav1alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: envtestGroup, Namespace: envtestNamespace},
			Spec: infrav1alpha1.GroupSpec{
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

	It("admits a never-provisioned slot powered off, without warnings", func() {
		node := &infrav1alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      envtestSlot,
				Namespace: envtestNamespace,
				Labels:    map[string]string{"group": envtestGroup},
			},
			Spec: infrav1alpha1.NodeSpec{
				DesiredPowerState:  infrav1alpha1.PowerStateOff,
				KubernetesNodeName: envtestSlot,
				Pricing: infrav1alpha1.PricingSpec{
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

		Expect(rec.drain()).To(BeEmpty(), "a powered off slot must not produce warnings")
	})

	It("admits powering on a slot without a Kubernetes node and returns the warning", func() {
		rec.drain()

		// This drives the real admission path: k8sClient -> API server -> deployed validating
		// webhook. Denying this write is what blocked the operator power-on path.
		Eventually(func() error {
			fresh := &infrav1alpha1.Node{}
			if err := advClient.Get(ctx, nodeKey, fresh); err != nil {
				return err
			}

			fresh.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn

			return advClient.Update(ctx, fresh)
		}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

		// Confirm the slot really is powered on and no corev1.Node exists for it.
		updated := &infrav1alpha1.Node{}
		Expect(advClient.Get(ctx, nodeKey, updated)).To(Succeed())
		Expect(updated.Spec.DesiredPowerState).To(Equal(infrav1alpha1.PowerStateOn))

		k8sNode := &corev1.Node{}
		Expect(apierrors.IsNotFound(advClient.Get(ctx, client.ObjectKey{Name: envtestSlot}, k8sNode))).To(BeTrue())

		warnings := rec.drain()
		Expect(warnings).To(ContainElement(
			ContainSubstring("kubernetes node \""+envtestSlot+"\" has not joined the cluster yet")),
			"the missing Kubernetes node must be reported as an admission warning")
		Expect(warnings).To(ContainElement(
			ContainSubstring("kubelet registers it when the machine boots")))
	})
})
