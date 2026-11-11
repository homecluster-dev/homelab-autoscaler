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

package taints

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func taint(key, value string, effect corev1.TaintEffect) corev1.Taint {
	return corev1.Taint{Key: key, Value: value, Effect: effect}
}

func TestIsProtectedKey(t *testing.T) {
	protected := []string{
		"node.kubernetes.io/not-ready",
		"node.kubernetes.io/unreachable",
		"node.kubernetes.io/unschedulable",
		"node.kubernetes.io/out-of-service",
		"beta.kubernetes.io/arch",
		ToBeDeletedByClusterAutoscaler,
		DeletionCandidateOfClusterAutoscaler,
	}
	for _, key := range protected {
		assert.True(t, IsProtectedKey(key), "%s must be protected", key)
	}

	for _, key := range []string{"dedicated", "storage", "pool.homecluster.dev/gpu", "node-homecluster.dev/keep"} {
		assert.False(t, IsProtectedKey(key), "%s must not be protected", key)
	}
}

// TestMerge covers the rules both the node controller and the node template use: add what is
// declared, overwrite by key, keep what is not declared, never write a key someone else owns,
// and report changed=false for a merge that would be a no-op so the caller skips the write.
func TestMerge(t *testing.T) {
	dedicated := taint("dedicated", "remote", corev1.TaintEffectNoSchedule)
	storage := taint("storage", "slow", corev1.TaintEffectNoSchedule)

	tests := []struct {
		name        string
		current     []corev1.Taint
		declared    []corev1.Taint
		want        []corev1.Taint
		wantChanged bool
		wantSkipped []string
	}{
		{
			name:        "a declared taint is added",
			current:     []corev1.Taint{storage},
			declared:    []corev1.Taint{dedicated},
			want:        []corev1.Taint{storage, dedicated},
			wantChanged: true,
		},
		{
			name:        "an existing taint with the same key is overwritten",
			current:     []corev1.Taint{taint("dedicated", "remote", corev1.TaintEffectNoExecute)},
			declared:    []corev1.Taint{dedicated},
			want:        []corev1.Taint{dedicated},
			wantChanged: true,
		},
		{
			name:        "an undeclared taint is kept",
			current:     []corev1.Taint{storage, taint("gpu", "true", corev1.TaintEffectNoSchedule)},
			declared:    []corev1.Taint{dedicated},
			want:        []corev1.Taint{storage, taint("gpu", "true", corev1.TaintEffectNoSchedule), dedicated},
			wantChanged: true,
		},
		{
			name:        "nothing changes when the declared taint is already there",
			current:     []corev1.Taint{dedicated},
			declared:    []corev1.Taint{dedicated},
			want:        []corev1.Taint{dedicated},
			wantChanged: false,
		},
		{
			name:        "nothing is declared",
			current:     []corev1.Taint{storage},
			declared:    nil,
			want:        []corev1.Taint{storage},
			wantChanged: false,
		},
		{
			name:        "a protected key is skipped",
			current:     []corev1.Taint{storage},
			declared:    []corev1.Taint{taint("node.kubernetes.io/not-ready", "", corev1.TaintEffectNoSchedule), taint(ToBeDeletedByClusterAutoscaler, "123", corev1.TaintEffectNoSchedule), dedicated},
			want:        []corev1.Taint{storage, dedicated},
			wantChanged: true,
			wantSkipped: []string{"node.kubernetes.io/not-ready", ToBeDeletedByClusterAutoscaler},
		},
		{
			name:        "the timeAdded of the node is kept when the taint is overwritten",
			current:     []corev1.Taint{{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoExecute, TimeAdded: &metav1.Time{}}},
			declared:    []corev1.Taint{dedicated},
			want:        []corev1.Taint{{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule, TimeAdded: &metav1.Time{}}},
			wantChanged: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next, changed, skipped := Merge(tc.current, tc.declared)

			assert.Equal(t, tc.want, next)
			assert.Equal(t, tc.wantChanged, changed)
			assert.Equal(t, tc.wantSkipped, skipped)

			// Overlay is the same rules without the change flag, used by the template
			assert.Equal(t, next, Overlay(tc.current, tc.declared))
		})
	}
}

// TestMergeDoesNotAliasingInputs checks that a declared taint is copied, not aliased, into the
// result: the caller must not be able to mutate the Node CR spec through the returned list.
func TestMergeDoesNotAliasDeclaredTaints(t *testing.T) {
	declared := []corev1.Taint{{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}}
	current := []corev1.Taint{{Key: "storage", Value: "slow", Effect: corev1.TaintEffectNoSchedule}}

	next, _, _ := Merge(current, declared)
	next[1].Value = "mutated"

	assert.Equal(t, "remote", declared[0].Value, "the declared slice must not be aliased into the result")
	assert.Equal(t, "slow", current[0].Value, "the current slice must not be aliased into the result")
}
