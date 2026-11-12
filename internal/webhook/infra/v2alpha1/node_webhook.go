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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/nodelabels"
)

// nolint:unused
// log is for logging in this package.
var nodelog = logf.Log.WithName("node-resource")

// SetupNodeWebhookWithManager registers the webhook for Node in the manager.
func SetupNodeWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav2alpha1.Node{}).
		WithValidator(&NodeCustomValidator{
			Client: mgr.GetClient(),
		}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-infra-homecluster-dev-v2alpha1-node,mutating=false,failurePolicy=fail,sideEffects=None,groups=infra.homecluster.dev,resources=nodes,verbs=create;update,versions=v2alpha1,name=vnode-v2alpha1.kb.io,admissionReviewVersions=v1

// NodeCustomValidator struct is responsible for validating the Node resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type NodeCustomValidator struct {
	Client client.Client
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Node.
func (v *NodeCustomValidator) ValidateCreate(ctx context.Context, node *infrav2alpha1.Node) (admission.Warnings, error) {
	nodelog.Info("Validation for Node upon creation", "name", node.GetName())

	if err := validateSlotMetadata(node); err != nil {
		return nil, err
	}

	if err := v.validateGroupLabel(ctx, node); err != nil {
		return nil, err
	}

	return v.kubernetesNodeWarnings(ctx, node), nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Node.
func (v *NodeCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav2alpha1.Node) (admission.Warnings, error) {
	nodelog.Info("Validation for Node upon update", "name", newObj.GetName())

	if err := validateSlotMetadata(newObj); err != nil {
		return nil, err
	}

	if err := v.validateGroupLabel(ctx, newObj); err != nil {
		return nil, err
	}

	return v.kubernetesNodeWarnings(ctx, newObj), nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Node.
func (v *NodeCustomValidator) ValidateDelete(ctx context.Context, node *infrav2alpha1.Node) (admission.Warnings, error) {
	nodelog.Info("Validation for Node upon deletion", "name", node.GetName())

	return nil, nil
}

// kubernetesNodeWarnings reports informational warnings about the referenced Kubernetes node. It
// never denies a request.
//
// A slot legitimately has no corev1.Node: in v2alpha1 the spec carries no runtime intent at all, so
// a slot of an idle pool is a slot with no machine, nothing creates the Kubernetes node, and the
// kubelet registers it when a NodeClaim boots the machine. Denying a Node whose node has not joined
// would block the operator's own declarations (the autoscaler never writes a Node spec any more), so
// the missing node is surfaced as an admission warning only.
//
// A failed lookup is never turned into a denial either: the admission webhook must not fail a request
// because it could not read the cluster.
func (v *NodeCustomValidator) kubernetesNodeWarnings(ctx context.Context, node *infrav2alpha1.Node) admission.Warnings {
	var k8sNode corev1.Node

	err := v.Client.Get(ctx, types.NamespacedName{Name: node.Spec.KubernetesNodeName}, &k8sNode)
	if err != nil {
		if apierrors.IsNotFound(err) {
			nodelog.V(1).Info("Kubernetes node has not joined the cluster yet",
				"nodeName", node.Name, "kubernetesNodeName", node.Spec.KubernetesNodeName)

			return admission.Warnings{fmt.Sprintf(
				"kubernetes node %q has not joined the cluster yet; kubelet registers it when the machine boots",
				node.Spec.KubernetesNodeName,
			)}
		}

		// Never deny on a lookup failure: the operator reconciles the state anyway.
		nodelog.Error(err, "Failed to verify kubernetes node existence",
			"nodeName", node.Name, "kubernetesNodeName", node.Spec.KubernetesNodeName)

		return nil
	}

	nodelog.V(1).Info("Validated kubernetes node exists", "nodeName", node.Spec.KubernetesNodeName)

	return nil
}

// validateSlotMetadata validates what the slot declares for its node object: spec.nodeLabels and
// spec.nodeAnnotations.
//
// The rules live in internal/nodelabels, the package that also writes them, so admission and the
// apply cannot disagree about which keys exist and which are refused. Every problem is reported at
// once, on one field path each, because the manifest an operator applies is a file with twenty
// labels in it and a refusal that names one of them is a two-second fix while a refusal that names
// none is a bisect.
//
// This is the only place the shape of the declaration is checked. The CRD bounds the count
// (maxProperties: 64 on both fields); the syntax of a key, the syntax of a value, and the reserved
// namespaces are here, because a CRD pattern cannot express a qualified name with its exceptions,
// and because the message an operator needs is "node.kubernetes.io/role is written by the kubelet",
// not a regex.
//
// Why it matters that this fails at admission: a slot whose label key the API server will not take
// is a slot whose label never lands on the node, which is a pod that never schedules, on a pool
// that never grows, discovered by reading a scheduler event five minutes later. Refusing the slot
// at `kubectl apply` puts the failure next to the edit that caused it.
func validateSlotMetadata(node *infrav2alpha1.Node) error {
	spec := field.NewPath("spec")

	errs := nodelabels.ValidateLabels(node.Spec.NodeLabels, spec.Child("nodeLabels"))
	errs = append(errs, nodelabels.ValidateAnnotations(node.Spec.NodeAnnotations, spec.Child("nodeAnnotations"))...)

	if len(errs) == 0 {
		return nil
	}

	nodelog.Info("Refusing a slot with node labels or annotations it may not declare",
		"name", node.GetName(), "errors", len(errs))

	return apierrors.NewInvalid(infrav2alpha1.GroupVersion.WithKind("Node").GroupKind(), node.GetName(), errs)
}

// validateGroupLabel checks if the "group" label exists and references a valid Group CRD
func (v *NodeCustomValidator) validateGroupLabel(ctx context.Context, node *infrav2alpha1.Node) error {
	// Check if the "group" label exists
	groupName, exists := node.Labels["group"]
	if !exists {
		return field.Required(
			field.NewPath("metadata").Child("labels").Child("group"),
			"Node must have a 'group' label",
		)
	}

	// Verify that the referenced Group CRD exists in the same namespace
	var group infrav2alpha1.Group
	err := v.Client.Get(ctx, types.NamespacedName{
		Name:      groupName,
		Namespace: node.Namespace,
	}, &group)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return field.Invalid(
				field.NewPath("metadata").Child("labels").Child("group"),
				groupName,
				fmt.Sprintf("Group '%s' does not exist in namespace '%s'", groupName, node.Namespace),
			)
		}
		return fmt.Errorf("failed to verify Group existence: %w", err)
	}

	nodelog.Info("Validated group label references existing Group", "groupName", groupName, "namespace", node.Namespace)
	return nil
}
