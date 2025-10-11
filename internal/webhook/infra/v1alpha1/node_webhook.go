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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var nodelog = logf.Log.WithName("node-resource")

// SetupNodeWebhookWithManager registers the webhook for Node in the manager.
func SetupNodeWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&infrav1alpha1.Node{}).
		WithValidator(&NodeCustomValidator{
			Client: mgr.GetClient(),
		}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-infra-homecluster-dev-v1alpha1-node,mutating=false,failurePolicy=fail,sideEffects=None,groups=infra.homecluster.dev,resources=nodes,verbs=create;update,versions=v1alpha1,name=vnode-v1alpha1.kb.io,admissionReviewVersions=v1

// NodeCustomValidator struct is responsible for validating the Node resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type NodeCustomValidator struct {
	Client client.Client
}

var _ webhook.CustomValidator = &NodeCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Node.
func (v *NodeCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	node, ok := obj.(*infrav1alpha1.Node)
	if !ok {
		return nil, fmt.Errorf("expected a Node object but got %T", obj)
	}
	nodelog.Info("Validation for Node upon creation", "name", node.GetName())

	if err := v.validateGroupLabel(ctx, node); err != nil {
		return nil, err
	}
	return nil, v.validateKubernetesNode(ctx, node)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Node.
func (v *NodeCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	node, ok := newObj.(*infrav1alpha1.Node)
	if !ok {
		return nil, fmt.Errorf("expected a Node object for the newObj but got %T", newObj)
	}
	nodelog.Info("Validation for Node upon update", "name", node.GetName())

	if err := v.validateGroupLabel(ctx, node); err != nil {
		return nil, err
	}
	return nil, v.validateKubernetesNode(ctx, node)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Node.
func (v *NodeCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	node, ok := obj.(*infrav1alpha1.Node)
	if !ok {
		return nil, fmt.Errorf("expected a Node object but got %T", obj)
	}
	nodelog.Info("Validation for Node upon deletion", "name", node.GetName())

	return nil, nil
}

// validateKubernetesNode checks if the referenced Kubernetes node exists
func (v *NodeCustomValidator) validateKubernetesNode(ctx context.Context, node *infrav1alpha1.Node) error {
	var k8sNode corev1.Node
	err := v.Client.Get(ctx, types.NamespacedName{Name: node.Spec.KubernetesNodeName}, &k8sNode)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return field.Invalid(
				field.NewPath("spec").Child("kubernetesNodeName"),
				node.Spec.KubernetesNodeName,
				fmt.Sprintf("kubernetes node '%s' does not exist in the cluster", node.Spec.KubernetesNodeName),
			)
		}
		return fmt.Errorf("failed to verify kubernetes node existence: %w", err)
	}

	nodelog.Info("Validated kubernetes node exists", "nodeName", node.Spec.KubernetesNodeName)
	return nil
}

// validateGroupLabel checks if the "group" label exists and references a valid Group CRD
func (v *NodeCustomValidator) validateGroupLabel(ctx context.Context, node *infrav1alpha1.Node) error {
	// Check if the "group" label exists
	groupName, exists := node.Labels["group"]
	if !exists {
		return field.Required(
			field.NewPath("metadata").Child("labels").Child("group"),
			"Node must have a 'group' label",
		)
	}

	// Verify that the referenced Group CRD exists in the same namespace
	var group infrav1alpha1.Group
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
