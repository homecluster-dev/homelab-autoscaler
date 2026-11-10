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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
		obj       *infrav1alpha1.Node
		oldObj    *infrav1alpha1.Node
		validator NodeCustomValidator
		k8sClient client.Client
	)

	BeforeEach(func() {
		scheme := runtime.NewScheme()
		Expect(infrav1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())

		// The cluster contains the Group CR and a Kubernetes node for a slot that is
		// already running, but no Kubernetes node for the slot under test.
		clusterGroup := &infrav1alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: testGroup, Namespace: testNamespace},
		}
		runningNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "omni-remote-1"},
		}
		k8sClient = fake.NewClientBuilder().
			WithScheme(scheme).
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
		It("Should reject a slot whose Kubernetes node does not exist", func() {
			obj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn

			_, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("kubernetes node '" + testNodeName + "' does not exist"))
		})

		It("Should admit a slot whose Kubernetes node exists", func() {
			obj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn
			obj.Spec.KubernetesNodeName = "omni-remote-1"

			_, err := validator.ValidateCreate(context.Background(), obj)
			Expect(err).NotTo(HaveOccurred())
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

		It("Should reject powering on a slot whose Kubernetes node does not exist", func() {
			oldObj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOff
			obj.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn

			_, err := validator.ValidateUpdate(context.Background(), oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("kubernetes node '" + testNodeName + "' does not exist"))
		})
	})
})
