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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// key of the namespace the *labels* belong to is never written, whatever the spec of an old
	// object claims.
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
		"homelab-autoscaler.dev/role": "spoofed",
		// refused as an annotation (Argo CD owns it there), and a label a nodeSelector can match:
		// the label overlay must keep it, or the pod selecting it is never placed
		"argocd.argoproj.io/team": "platform",
	}

	assert.Equal(t, map[string]string{
		"kubernetes.io/hostname":      "worker-01",
		"topology.kubernetes.io/zone": "fsn1",
		"infra.homecluster.dev/group": "workers",
		"node.kubernetes.io/role":     "agent",
		"pool.homecluster.dev/tier":   "storage",
		"argocd.argoproj.io/team":     "platform",
	}, OverlayLabels(current, declared),
		"the label overlay filters the label-reserved namespaces and nothing else: dropping "+
			"argocd.argoproj.io/team is the silent failure this feature exists to remove, because the "+
			"template the estimator judges the pod against would not carry the label the selector asks for")

	assert.Equal(t, current, OverlayLabels(current, nil), "a slot that declares nothing leaves the template as copied")
	assert.Nil(t, OverlayLabels(nil, nil))

	// The same key in the other direction: refused as an annotation, where Argo CD owns it.
	assert.NotContains(t, OverlayAnnotations(nil, map[string]string{"argocd.argoproj.io/team": "platform"}),
		"argocd.argoproj.io/team", "the annotation overlay uses the annotation list")
}

func TestOverlayAnnotationsSkipsReservedKeys(t *testing.T) {
	overlaid := OverlayAnnotations(map[string]string{"pool.homecluster.dev/note": "keep"}, map[string]string{
		"argocd.argoproj.io/sync-wave":                         "1",
		"kubectl.kubernetes.io/last-applied-configuration":     "{}",
		"infra.homecluster.dev/applied-node-labels":            "rack",
		"homelab-autoscaler.dev/operation-lock":                "scale-up",
		"cluster-autoscaler.kubernetes.io/scale-down-disabled": "true",
		"pool.homecluster.dev/power":                           "ipmi",
		// the namespaces reserved for labels only are open for annotations
		"kubernetes.io/maintenance-window": "sat 03:00",
	})

	assert.Equal(t, map[string]string{
		"pool.homecluster.dev/note":        "keep",
		"pool.homecluster.dev/power":       "ipmi",
		"kubernetes.io/maintenance-window": "sat 03:00",
	}, overlaid)
}

func TestConsensus(t *testing.T) {
	// The fail-closed rule of a group's template: a key is answered for the group only when every
	// slot of the group declares it, with the same value. Anything less is a promise the scale-up
	// cannot keep, because IncreaseSize buys whichever slot is free.
	slots := []SlotDeclaration{
		{Name: "slot-a", Declared: map[string]string{
			TopologyZoneLabel:                "fsn1",
			"pool.homecluster.dev/rack":      "3",
			"pool.homecluster.dev/tier":      "storage",
			"pool.homecluster.dev/contested": "nvme",
		}},
		{Name: "slot-b", Declared: map[string]string{
			TopologyZoneLabel:                "fsn1",
			"pool.homecluster.dev/rack":      "3",
			"pool.homecluster.dev/contested": "sata",
			"pool.homecluster.dev/power":     "ipmi",
		}},
		{Name: "slot-c", Declared: map[string]string{
			TopologyZoneLabel:                "fsn1",
			"pool.homecluster.dev/rack":      "3",
			"pool.homecluster.dev/contested": "nvme",
		}},
	}

	agreed, conflicts := Consensus(slots)

	assert.Equal(t, map[string]string{TopologyZoneLabel: "fsn1", "pool.homecluster.dev/rack": "3"}, agreed,
		"a key every slot declares with the same value is the group's shape")

	require.Len(t, conflicts, 3, "one key with two values, and two keys some slots do not declare at all")

	notes := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		notes = append(notes, conflict.Note())
	}

	joined := strings.Join(notes, "\n")

	assert.Contains(t, joined, "pool.homecluster.dev/contested")
	assert.Contains(t, joined, `"nvme" on slot-a, slot-c`, "the note names the value and the slots that declare it")
	assert.Contains(t, joined, `"sata" on slot-b`)
	assert.Contains(t, joined, `pool.homecluster.dev/tier: "storage" on slot-a; not declared at all on slot-b, slot-c`,
		"a key some slots never declare is not the group's shape either: the machine IncreaseSize buys "+
			"may be one of them, and the pod would stay Pending on a machine that was paid for")
	assert.Contains(t, joined, `pool.homecluster.dev/power: "ipmi" on slot-b; not declared at all on slot-c`)

	// A group of one slot is exact, which is every pool of one machine, and a pool whose slots
	// declare the same keys is exact, which is every pool the docs describe.
	single, conflicts := Consensus([]SlotDeclaration{{Name: "slot-a", Declared: map[string]string{TopologyZoneLabel: "fsn1"}}})
	assert.Equal(t, map[string]string{TopologyZoneLabel: "fsn1"}, single)
	assert.Empty(t, conflicts)

	// A slot that declares nothing declares no opinion, so nothing is guaranteed for the group.
	zero, conflicts := Consensus([]SlotDeclaration{
		{Name: "slot-a", Declared: map[string]string{TopologyZoneLabel: "fsn1"}},
		{Name: "slot-b"},
	})
	assert.Empty(t, zero, "one slot of the pool has no zone, so the pool cannot promise one")
	require.Len(t, conflicts, 1)
	assert.Equal(t, []string{"slot-b"}, conflicts[0].Silent)

	// Nothing declared anywhere, and no slots at all: the answer is "no promises, no conflicts".
	empty, conflicts := Consensus(nil)
	assert.Nil(t, empty)
	assert.Empty(t, conflicts)

	empty, conflicts = Consensus([]SlotDeclaration{{Name: "slot-a"}, {Name: "slot-b"}})
	assert.Empty(t, empty)
	assert.Empty(t, conflicts, "a pool that declares nothing is exactly what it is")
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
