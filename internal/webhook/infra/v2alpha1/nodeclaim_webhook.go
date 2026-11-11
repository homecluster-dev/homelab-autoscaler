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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// nolint:unused
// claimlog is for logging in this package.
var claimlog = logf.Log.WithName("nodeclaim-resource")

// SetupNodeClaimWebhookWithManager registers the webhook for NodeClaim in the manager.
func SetupNodeClaimWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav2alpha1.NodeClaim{}).
		WithValidator(&NodeClaimCustomValidator{
			Client: mgr.GetClient(),
		}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-infra-homecluster-dev-v2alpha1-nodeclaim,mutating=false,failurePolicy=fail,sideEffects=None,groups=infra.homecluster.dev,resources=nodeclaims,verbs=create;update,versions=v2alpha1,name=nodeclaim-v2alpha1.kb.io,admissionReviewVersions=v1

// NodeClaimCustomValidator validates NodeClaim requests.
//
// Its one job is the model's load-bearing invariant: one live claim per slot. It is what makes "a
// live claim exists" a synonym for "the machine is wanted", what keeps the provider's target size a
// count of claims, and what keeps the model cancellable. A CRD cannot express cross-object
// uniqueness, so the rule is stated here, at admission, and enforced again by the claim controller
// for the requests admission cannot see (a webhook that was down, a cached list that raced).
//
// Both halves name the slot in the message, because the question an operator asks is about the slot.
type NodeClaimCustomValidator struct {
	Client client.Client
}

// ValidateCreate implements webhook.CustomValidator.
func (v *NodeClaimCustomValidator) ValidateCreate(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) (admission.Warnings, error) {
	claimlog.Info("Validation for NodeClaim upon creation", "name", nodeClaim.GetName())

	return nil, v.validate(ctx, nodeClaim)
}

// ValidateUpdate implements webhook.CustomValidator.
func (v *NodeClaimCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *infrav2alpha1.NodeClaim) (admission.Warnings, error) {
	claimlog.Info("Validation for NodeClaim upon update", "name", newObj.GetName())

	return nil, v.validate(ctx, newObj)
}

// ValidateDelete implements webhook.CustomValidator.
//
// Deleting a claim is never refused: canceling a pending claim is a delete, and a booting claim is
// deleted through its finalizer, which runs the shutdown. Admission is the wrong place to judge a
// scale-down.
func (v *NodeClaimCustomValidator) ValidateDelete(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) (admission.Warnings, error) {
	return nil, nil
}

// validate checks the slot of a claim and the one-live-per-slot rule.
func (v *NodeClaimCustomValidator) validate(ctx context.Context, nodeClaim *infrav2alpha1.NodeClaim) error {
	slotName := claim.SlotNameOf(nodeClaim)
	if slotName == "" {
		return field.Required(field.NewPath("spec").Child("slotRef").Child("name"),
			"a NodeClaim names the slot (Node CR) it asks for")
	}

	// The slot must exist in the claim's namespace: a claim for a slot that is not there can never
	// be satisfied, and the claim controller would fail it, so the admission answer is the shorter
	// path with the better message.
	var slot infrav2alpha1.Node
	if err := v.Client.Get(ctx, types.NamespacedName{Name: slotName, Namespace: nodeClaim.Namespace}, &slot); err != nil {
		if apierrors.IsNotFound(err) {
			return field.Invalid(field.NewPath("spec").Child("slotRef").Child("name"), slotName,
				fmt.Sprintf("slot %q does not exist in namespace %q", slotName, nodeClaim.Namespace))
		}

		// Never deny on a lookup failure that is not "it does not exist".
		claimlog.Error(err, "Failed to verify the slot of the claim", "slot", slotName)

		return nil
	}

	if !phase.IsLive(nodeClaim.Status.Phase) && nodeClaim.Status.Phase != "" {
		// A write that does not open a live claim (a status move to a terminal phase) is never the
		// second claim on the slot.
		return nil
	}

	claims, err := claim.ListOfSlot(ctx, v.Client, nodeClaim.Namespace, slotName)
	if err != nil {
		claimlog.Error(err, "Failed to list the claims of the slot", "slot", slotName)

		return nil
	}

	if live := claim.LiveOfSlot(claims); live != nil && live.Name != nodeClaim.Name {
		return field.Forbidden(field.NewPath("spec").Child("slotRef"),
			fmt.Sprintf("slot %q already has the live NodeClaim %q in phase %q: one live claim per slot",
				slotName, live.Name, string(live.Status.Phase)))
	}

	return nil
}
