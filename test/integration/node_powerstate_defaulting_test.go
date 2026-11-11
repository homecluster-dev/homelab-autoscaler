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

package integration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

const (
	// nodeCRDName is the CRD whose schema these specs pin: the default and the optionality of
	// spec.powerState live in the schema, so the assertions read the schema the API server
	// installed from config/crd/bases, which is the artifact a cluster actually runs.
	nodeCRDName = "nodes.infra.homecluster.dev"

	gitOpsFieldManager = "argocd"
	slotImage          = "busybox"
	slotNamespace      = "default"
)

// gitOpsManifest renders a Node CR the way a GitOps repository carries it: unstructured, so the
// apply configuration that reaches the API server is exactly the YAML in git. A powerState of ""
// means the manifest does not declare the field - the key is absent from the request, which is
// the state the churn fix asks for.
func gitOpsManifest(name, powerState string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(infrav1alpha1.GroupVersion.WithKind("Node"))
	u.SetName(name)
	u.SetNamespace(slotNamespace)

	spec := map[string]any{
		"kubernetesNodeName": name,
		"pricing":            map[string]any{"hourlyRate": "0", "podRate": "0"},
		"startupPodSpec":     map[string]any{"image": slotImage},
		"shutdownPodSpec":    map[string]any{"image": slotImage},
	}
	if powerState != "" {
		spec["powerState"] = powerState
	}
	if err := unstructured.SetNestedField(u.Object, spec, "spec"); err != nil {
		Fail("failed to build the gitops manifest: " + err.Error())
	}
	return u
}

// declaredSlot returns a typed Node as a controller builds one, with an explicit claim. Passing
// "" leaves spec.powerState unset, which is the Go zero value and what a client sends when the
// field is omitted: the json tag carries omitempty, so the key is absent from the request.
func declaredSlot(name string, powerState infrav1alpha1.PowerState) *infrav1alpha1.Node {
	return &infrav1alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: slotNamespace},
		Spec: infrav1alpha1.NodeSpec{
			DesiredPowerState:  powerState,
			KubernetesNodeName: name,
			Pricing:            infrav1alpha1.PricingSpec{HourlyRate: "0", PodRate: "0"},
			StartupPodSpec:     infrav1alpha1.MinimalPodSpec{Image: slotImage},
			ShutdownPodSpec:    infrav1alpha1.MinimalPodSpec{Image: slotImage},
		},
	}
}

// storedSlot reads back what the API server persisted for a slot.
func storedSlot(name string) *infrav1alpha1.Node {
	stored := &infrav1alpha1.Node{}
	key := client.ObjectKey{Name: name, Namespace: slotNamespace}
	Expect(k8sClient.Get(context.Background(), key, stored)).To(Succeed())
	return stored
}

// storedClaim reads back the persisted claim of a slot.
func storedClaim(name string) infrav1alpha1.PowerState {
	return storedSlot(name).Spec.DesiredPowerState
}

// applyFromGit patches the slot the way Argo CD does with a ServerSideApply policy: the manifest
// in git is the whole apply configuration, sent under its own field manager.
func applyFromGit(name, powerState string) {
	err := k8sClient.Apply(context.Background(),
		client.ApplyConfigurationFromUnstructured(gitOpsManifest(name, powerState)),
		client.FieldOwner(gitOpsFieldManager), client.ForceOwnership)
	Expect(err).NotTo(HaveOccurred(), "the gitops apply must be accepted")
}

// claimSlot writes the claim the way the autoscaler does: read-modify-write Update. The read
// matters - a full-object Update that omits the field is defaulted by the API server, see the
// last spec, so a writer of the claim has to send the object it read.
func claimSlot(name string, powerState infrav1alpha1.PowerState) {
	slot := &infrav1alpha1.Node{}
	key := client.ObjectKey{Name: name, Namespace: slotNamespace}
	Expect(k8sClient.Get(context.Background(), key, slot)).To(Succeed())

	slot.Spec.DesiredPowerState = powerState
	Expect(k8sClient.Update(context.Background(), slot)).To(Succeed())
}

var _ = Describe("Node spec.powerState: CRD default, GitOps apply, autoscaler claim", func() {
	var testCtx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		testCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
		DeferCleanup(cancel)
	})

	// slot registers the deletion of the slot it names, so the spec leaves nothing behind in a
	// cluster that the rest of the suite shares.
	slot := func(name string) string {
		DeferCleanup(func() {
			node := &infrav1alpha1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: slotNamespace},
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, node))).To(Succeed())
		})
		return name
	}

	It("installs a CRD in which spec.powerState is optional and defaulted to off", func() {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition",
		})
		Expect(k8sClient.Get(testCtx, client.ObjectKey{Name: nodeCRDName}, crd)).To(Succeed())

		versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(), "the installed Node CRD must carry a version")

		version, ok := versions[0].(map[string]any)
		Expect(ok).To(BeTrue(), "the Node CRD version must be a map")

		specSchema, found, err := unstructured.NestedMap(
			version, "schema", "openAPIV3Schema", "properties", "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())

		required, _, err := unstructured.NestedSlice(specSchema, "required")
		Expect(err).NotTo(HaveOccurred())
		for _, field := range required {
			Expect(field).NotTo(Equal("powerState"),
				"spec.powerState must stay out of the required list: a required field forces every "+
					"GitOps manifest to declare it, and a declared field is re-applied over the autoscaler's claim")
		}

		powerState, found, err := unstructured.NestedMap(
			specSchema, "properties", "powerState")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(powerState["default"]).To(Equal("off"),
			"spec.powerState must keep default: \"off\", the value that lets a manifest omit the field")
	})

	It("stores off for a Node created through the API server without the field", func() {
		name := slot("defaulting-create-omitted")

		Expect(k8sClient.Create(testCtx, declaredSlot(name, ""))).To(Succeed())

		Expect(storedClaim(name)).To(Equal(infrav1alpha1.PowerStateOff),
			"a Node that omits spec.powerState is claimed off by the API server, so an undeclared slot is off")
	})

	It("keeps the claim it was given for a Node created with powerState on", func() {
		name := slot("defaulting-create-on")

		Expect(k8sClient.Create(testCtx, declaredSlot(name, infrav1alpha1.PowerStateOn))).To(Succeed())

		Expect(storedClaim(name)).To(Equal(infrav1alpha1.PowerStateOn),
			"the default must not overwrite a claim that is present in the request")
	})

	It("leaves a claim of on standing when a gitops apply that omits the field is re-applied", func() {
		name := slot("defaulting-gitops-apply-omits-field")

		// git declares the slot without the field, the autoscaler claims the machine on, and
		// then git syncs again: the apply that carries no powerState must not touch the claim.
		applyFromGit(name, "")
		Expect(storedClaim(name)).To(Equal(infrav1alpha1.PowerStateOff))

		claimSlot(name, infrav1alpha1.PowerStateOn)
		Expect(storedClaim(name)).To(Equal(infrav1alpha1.PowerStateOn))

		applyFromGit(name, "")
		Expect(storedClaim(name)).To(Equal(infrav1alpha1.PowerStateOn),
			"this is the property that ends the churn: an applied configuration that does not carry "+
				"spec.powerState cannot revert the autoscaler's claim, so the machine is not shut down "+
				"mid-use and the pending pod does not trigger the next scale-up")
	})

	It("reverts a claim of on when git declares powerState off, which is why the manifests must drop it", func() {
		name := slot("defaulting-gitops-apply-declares-off")

		// The live failure, reproduced against a real API server: a manifest that declares
		// powerState: off owns the field, and every sync of selfHeal writes it back. The
		// operator cannot out-write that, so the field must not be declared in git at all.
		applyFromGit(name, string(infrav1alpha1.PowerStateOff))
		claimSlot(name, infrav1alpha1.PowerStateOn)

		applyFromGit(name, string(infrav1alpha1.PowerStateOff))
		Expect(storedClaim(name)).To(Equal(infrav1alpha1.PowerStateOff),
			"a manifest that declares the field wins the claim - keep it out of git")
	})

	It("defaults a full-object update whose body omits the field, so a claim writer must send what it read", func() {
		name := slot("defaulting-update-omitting-field")

		Expect(k8sClient.Create(testCtx, declaredSlot(name, infrav1alpha1.PowerStateOn))).To(Succeed())

		// A typed Update sends the struct: an unset DesiredPowerState is omitted from the body,
		// and the API server applies the default to that body. Read-modify-write is the contract
		// for every writer of the claim (the gRPC provider does exactly that), and the
		// empty-claim guard in the Node controller stays as the net for a hand-built literal.
		send := declaredSlot(name, "")
		send.SetResourceVersion(storedSlot(name).GetResourceVersion())
		Expect(k8sClient.Update(testCtx, send)).To(Succeed())

		Expect(storedClaim(name)).To(Equal(infrav1alpha1.PowerStateOff),
			"a PUT that omits spec.powerState is defaulted to off, it does not preserve the stored claim")
	})
})
