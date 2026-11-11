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

// Package taints merges the taints a Node CR declares (spec.nodeTaints) with the taints a
// Kubernetes node carries, and with the taints of the node template the provider reports to
// cluster-autoscaler. Both sides need the same rules, so they live here once: the node
// controller stamps the declared taints onto the live node, and the gRPC provider overlays
// them on the template so the autoscaler sees them before the machine has registered.
package taints

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Cluster-autoscaler owns the taints it stamps when it marks a node for deletion. They are
// spelled out here because they are not exported by the k8s.io API packages; see
// cluster-autoscaler/utils/taints.
const (
	// ToBeDeletedByClusterAutoscaler marks a node cluster-autoscaler has decided to delete.
	ToBeDeletedByClusterAutoscaler = "ToBeDeletedByClusterAutoscaler"
	// DeletionCandidateOfClusterAutoscaler marks a node cluster-autoscaler is considering.
	DeletionCandidateOfClusterAutoscaler = "DeletionCandidateOfClusterAutoscaler"
)

// IsProtectedKey reports whether a taint key belongs to someone else, and must therefore not
// be written by the operator:
//
//   - node.kubernetes.io/* and beta.kubernetes.io/* are stamped by the node lifecycle
//     controllers (not-ready, unreachable, unschedulable, out-of-service) and removed by them;
//   - the two cluster-autoscaler keys are added and removed by the autoscaler itself.
//
// A declaration that collides with a protected key is skipped instead of applied: two writers
// fighting over one key never converges, and the owner always wins the last write.
func IsProtectedKey(key string) bool {
	if strings.HasPrefix(key, "node.kubernetes.io/") || strings.HasPrefix(key, "beta.kubernetes.io/") {
		return true
	}

	switch key {
	case ToBeDeletedByClusterAutoscaler, DeletionCandidateOfClusterAutoscaler:
		return true
	default:
		return false
	}
}

// Overlay returns current with every declared taint applied to it: a declared key that is not
// present is added, a declared key that is present is overwritten. A taint that is not
// declared is left alone, and the result keeps one entry per key, which is the invariant
// Kubernetes enforces on node taints.
//
// The declared taint wins over the existing one because it is the intent: the Node CR says
// what the machine must look like, the node object says what it happens to look like right
// now. The input slices are not modified.
//
// Protected keys are skipped here too, so the template and the live node agree: a taint the
// operator will not write to a node is not one the autoscaler should simulate either. The
// transient node.kubernetes.io/not-ready, /unreachable and /unschedulable taints are also
// stripped from the template, and a declaration cannot put them back.
func Overlay(current, declared []corev1.Taint) []corev1.Taint {
	next, changed, _ := Merge(current, declared)
	if !changed {
		return current
	}

	return next
}

// Merge is Overlay with the answer to "did anything change?", so a caller can skip a write
// that would be a no-op (the node controller must not patch a node every reconcile).
//
// It also reports the declared keys that were skipped because they are protected, so the
// caller can surface them.
func Merge(current, declared []corev1.Taint) (next []corev1.Taint, changed bool, skipped []string) {
	if len(declared) == 0 {
		return current, false, nil
	}

	next = make([]corev1.Taint, len(current))
	copy(next, current)

	for _, want := range declared {
		if IsProtectedKey(want.Key) {
			skipped = append(skipped, want.Key)

			continue
		}

		found := false

		for i := range next {
			if next[i].Key != want.Key {
				continue
			}

			found = true

			if next[i].Value != want.Value || next[i].Effect != want.Effect {
				update := *want.DeepCopy()
				// Keep when the existing taint was stamped: spec has no timestamp, and
				// rewriting it on every patch would make the write never settle.
				if update.TimeAdded == nil {
					update.TimeAdded = next[i].TimeAdded
				}

				next[i] = update
				changed = true
			}

			break
		}

		if !found {
			next = append(next, *want.DeepCopy())
			changed = true
		}
	}

	return next, changed, skipped
}
