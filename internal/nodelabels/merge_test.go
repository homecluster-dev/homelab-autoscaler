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

package nodelabels

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReconcile(t *testing.T) {
	tests := []struct {
		name     string
		current  map[string]string
		declared map[string]string
		owned    map[string]string
		want     map[string]string
		change   Change
	}{
		{
			name:    "nothing declared and nothing owned writes nothing",
			current: map[string]string{"kubernetes.io/hostname": "worker-01"},
			want:    map[string]string{"kubernetes.io/hostname": "worker-01"},
		},
		{
			name:     "a declared key the node does not carry is added",
			current:  map[string]string{"kubernetes.io/hostname": "worker-01"},
			declared: map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			owned:    map[string]string{},
			want:     map[string]string{"kubernetes.io/hostname": "worker-01", "topology.kubernetes.io/zone": "fsn1"},
			change:   Change{Added: []string{"topology.kubernetes.io/zone"}},
		},
		{
			name:     "a declared key the node carries with the same value changes nothing",
			current:  map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			declared: map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			owned:    map[string]string{"topology.kubernetes.io/zone": ""},
			want:     map[string]string{"topology.kubernetes.io/zone": "fsn1"},
		},
		{
			// The drift of a reboot, of a kubelet that stopped stamping the label, of an operator's
			// `kubectl label --overwrite`: the declaration is the intent, so the value is put back.
			name:     "drift on a key the slot owns is re-asserted",
			current:  map[string]string{"topology.kubernetes.io/zone": "hel1"},
			declared: map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			owned:    map[string]string{"topology.kubernetes.io/zone": ""},
			want:     map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			change:   Change{Updated: []string{"topology.kubernetes.io/zone"}},
		},
		{
			// The rule a label-only writer could not implement: dropping the key from Git is
			// dropping it from the machine, and only for the keys the slot wrote.
			name:     "an owned key the declaration dropped is removed",
			current:  map[string]string{"topology.kubernetes.io/zone": "fsn1", "rack": "3"},
			declared: map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			owned:    map[string]string{"topology.kubernetes.io/zone": "", "rack": ""},
			want:     map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			change:   Change{Removed: []string{"rack"}},
		},
		{
			// The other half of the same rule: the kubelet's labels, and anyone else's, are not
			// this controller's to remove.
			name:     "a key the slot never declared and never wrote is left alone",
			current:  map[string]string{"kubernetes.io/hostname": "worker-01", "node.kubernetes.io/role": "agent"},
			declared: map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			owned:    map[string]string{},
			want: map[string]string{
				"kubernetes.io/hostname":      "worker-01",
				"node.kubernetes.io/role":     "agent",
				"topology.kubernetes.io/zone": "fsn1",
			},
			change: Change{Added: []string{"topology.kubernetes.io/zone"}},
		},
		{
			name: "one pass can add, re-assert and remove at once",
			current: map[string]string{
				"rack":                        "3",
				"topology.kubernetes.io/zone": "hel1",
				"kubernetes.io/hostname":      "worker-01",
			},
			declared: map[string]string{"topology.kubernetes.io/zone": "fsn1", "pool.homecluster.dev/tier": "storage"},
			owned:    map[string]string{"rack": "", "topology.kubernetes.io/zone": ""},
			want: map[string]string{
				"topology.kubernetes.io/zone": "fsn1",
				"pool.homecluster.dev/tier":   "storage",
				"kubernetes.io/hostname":      "worker-01",
			},
			change: Change{
				Added:   []string{"pool.homecluster.dev/tier"},
				Updated: []string{"topology.kubernetes.io/zone"},
				Removed: []string{"rack"},
			},
		},
		{
			name:     "a key that is owned and declared keeps the declared value",
			current:  map[string]string{"rack": "3"},
			declared: map[string]string{"rack": "4"},
			owned:    map[string]string{"rack": ""},
			want:     map[string]string{"rack": "4"},
			change:   Change{Updated: []string{"rack"}},
		},
		{
			name:     "an owned key the node does not carry is not reported as removed",
			current:  map[string]string{},
			declared: map[string]string{},
			owned:    map[string]string{"rack": ""},
			want:     map[string]string{},
		},
		{
			name:     "a nil current map is a node with no labels at all",
			current:  nil,
			declared: map[string]string{"rack": "3"},
			owned:    nil,
			want:     map[string]string{"rack": "3"},
			change:   Change{Added: []string{"rack"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next, change := Reconcile(tc.current, tc.declared, tc.owned)

			assert.Equal(t, tc.want, next)
			assert.Equal(t, tc.change.Added, change.Added, "added keys")
			assert.Equal(t, tc.change.Updated, change.Updated, "updated keys")
			assert.Equal(t, tc.change.Removed, change.Removed, "removed keys")
			assert.Equal(t, tc.change.Empty(), change.Empty())
		})
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	// Applying the same declaration to the node the previous apply produced must change nothing:
	// every resync of a settled slot runs this path, and a write per pass is churn on a hot object.
	current := map[string]string{"kubernetes.io/hostname": "worker-01"}
	declared := map[string]string{"topology.kubernetes.io/zone": "fsn1", "rack": "3"}

	next, first := Reconcile(current, declared, DecodeOwnerRecord(""))
	assert.False(t, first.Empty())

	again, second := Reconcile(next, declared, DecodeOwnerRecord(EncodeOwnerRecord(DeclaredKeys(declared))))
	assert.True(t, second.Empty(), "the second pass over the same declaration writes nothing: %+v", second)
	assert.Equal(t, next, again)
}

func TestReconcileDoesNotAliasItsInputs(t *testing.T) {
	// The node object and the Node CR both come out of the manager's cache. A merge that wrote
	// into them would mutate shared state.
	current := map[string]string{"rack": "3"}
	declared := map[string]string{"topology.kubernetes.io/zone": "fsn1"}
	owned := map[string]string{"old": ""}

	next, _ := Reconcile(current, declared, owned)
	next["injected"] = "x"

	assert.Equal(t, map[string]string{"rack": "3"}, current, "the node's own map is untouched")
	assert.Equal(t, map[string]string{"topology.kubernetes.io/zone": "fsn1"}, declared, "the declaration is untouched")
	assert.Len(t, owned, 1)
}

func TestOverlay(t *testing.T) {
	// The template's rule: declared keys win, everything else is kept, nothing is removed, and a
	// reserved key is never written, whatever the spec of an old object claims.
	current := map[string]string{
		"kubernetes.io/hostname":      "worker-01",
		"topology.kubernetes.io/zone": "hel1",
		"infra.homecluster.dev/group": "workers",
		"node.kubernetes.io/role":     "agent",
	}
	declared := map[string]string{
		"topology.kubernetes.io/zone": "fsn1",
		"pool.homecluster.dev/tier":   "storage",
		// refused at admission; a declaration that predates the rule must still not land
		"kubernetes.io/hostname":      "spoofed",
		"node.kubernetes.io/role":     "spoofed",
		"infra.homecluster.dev/group": "spoofed",
	}

	assert.Equal(t, map[string]string{
		"kubernetes.io/hostname":      "worker-01",
		"topology.kubernetes.io/zone": "fsn1",
		"infra.homecluster.dev/group": "workers",
		"node.kubernetes.io/role":     "agent",
		"pool.homecluster.dev/tier":   "storage",
	}, Overlay(current, declared))

	assert.Equal(t, current, Overlay(current, nil), "a slot that declares nothing leaves the template as copied")
	assert.Nil(t, Overlay(nil, nil))
}

func TestOverlayAnnotationsSkipsReservedKeys(t *testing.T) {
	overlaid := Overlay(map[string]string{"pool.homecluster.dev/note": "keep"}, map[string]string{
		"argocd.argoproj.io/sync-wave":                     "1",
		"kubectl.kubernetes.io/last-applied-configuration": "{}",
		"infra.homecluster.dev/applied-node-labels":        "rack",
		"pool.homecluster.dev/power":                       "ipmi",
	})

	assert.Equal(t, map[string]string{
		"pool.homecluster.dev/note":  "keep",
		"pool.homecluster.dev/power": "ipmi",
	}, overlaid)
}

func TestChangeNote(t *testing.T) {
	note := ChangeNote(
		Change{Added: []string{"topology.kubernetes.io/zone"}, Updated: []string{"rack"}, Removed: []string{"old"}},
		Change{Added: []string{"pool.homecluster.dev/serial"}},
	)

	assert.Equal(t, "labels: +topology.kubernetes.io/zone ~rack -old; annotations: +pool.homecluster.dev/serial", note)
	assert.Empty(t, ChangeNote(Change{}, Change{}))
	assert.Equal(t, "annotations: +a", ChangeNote(Change{}, Change{Added: []string{"a"}}))
}
