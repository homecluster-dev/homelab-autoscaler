//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"testing"
)

// The one rule the labels spec's own writes rest on, checked where it can be checked in a
// millisecond: what `kubectl patch --type=merge` does to a map. It merges, per RFC 7386 — a key the
// patch does not name stays in the object, only a null removes one, and a null on the field drops the
// field with it. `patchSlotLabels` has to name the dropped keys as nulls, and the CI run of the labels
// spec is the evidence of what happens when it does not: the suite drops `pool.homecluster.dev/rack`
// from a slot's declaration, the patch leaves the key in the slot, the writer correctly keeps the
// label it is still told to declare, and the gate that asserts "a key out of Git comes off the
// machine" times out after a minute of a declaration that never changed.
//
// This is a plain Go test, not a Ginkgo spec: the suite it belongs to needs a k3d cluster, and the
// fact is about a pure function. `go test -tags e2e ./test/e2e/ -run TestDeclarationPatch` is the
// check, and it runs on a laptop.
func TestDeclarationPatch(t *testing.T) {
	cases := []struct {
		name     string
		current  map[string]string
		declared map[string]string
		want     any
	}{
		{
			name:     "a key dropped from the declaration is named as a null",
			current:  map[string]string{"topology.kubernetes.io/zone": "fsn1", "pool.homecluster.dev/rack": "rack-3"},
			declared: map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			want:     map[string]any{"topology.kubernetes.io/zone": "fsn1", "pool.homecluster.dev/rack": nil},
		},
		{
			name:     "a value rewritten is named with its new value",
			current:  map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			declared: map[string]string{"topology.kubernetes.io/zone": "hel1"},
			want:     map[string]any{"topology.kubernetes.io/zone": "hel1"},
		},
		{
			name:     "the whole field dropped",
			current:  map[string]string{"pool.homecluster.dev/serial": "SN-0001"},
			declared: nil,
			want:     nil,
		},
		{
			name:     "a declaration on a slot that had none",
			current:  nil,
			declared: map[string]string{"pool.homecluster.dev/rack": "rack-3"},
			want:     map[string]any{"pool.homecluster.dev/rack": "rack-3"},
		},
		{
			name:     "an empty declaration empties the field, and removes every key it had",
			current:  map[string]string{"topology.kubernetes.io/zone": "fsn1"},
			declared: map[string]string{},
			want:     map[string]any{"topology.kubernetes.io/zone": nil},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := declarationPatch(tc.current, tc.declared)

			if jsonEqual(t, got) != jsonEqual(t, tc.want) {
				t.Fatalf("patch for %q:\n got %s\nwant %s", tc.name, jsonEqual(t, got), jsonEqual(t, tc.want))
			}

			// The shape that matters is the one the API server sees, so the assertion is made on the
			// bytes of the payload: a Go nil in a map marshals to `null`, a nil field to `null`, and
			// an empty map to `{}`. This is the check that fails if the nulls are ever dropped on the
			// way to the command line.
			payload, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal the patch: %v", err)
			}

			if tc.declared == nil {
				if string(payload) != "null" {
					t.Fatalf("dropping the field sends %s, want null", payload)
				}

				// Nothing to name key by key: the field is the null, and it takes its keys with it.
				return
			}

			patchMap, ok := got.(map[string]any)
			if !ok {
				t.Fatalf("a declaration is patched with a map, got %T", got)
			}

			for key, value := range tc.declared {
				if named, isNamed := patchMap[key]; !isNamed || named != any(value) {
					t.Fatalf("the declared key %s is named with %v (named: %v), want %v",
						key, named, isNamed, value)
				}
			}

			for _, dropped := range droppedKeys(tc.current, tc.declared) {
				named, isNamed := patchMap[dropped]
				if !isNamed {
					t.Fatalf("the key %s is dropped from the declaration, so the patch must name it as a "+
						"null; a patch that is silent about it leaves it on the slot for the life of the machine",
						dropped)
				}

				if named != nil {
					t.Fatalf("the dropped key %s is named with %v, want null", dropped, named)
				}
			}
		})
	}
}

// droppedKeys is the set of keys a live declaration has and the new one does not: the keys the
// operator deleted from the file, and the only reason a dropped label ever comes off a machine.
func droppedKeys(current, declared map[string]string) []string {
	var dropped []string

	for key := range current {
		if _, kept := declared[key]; !kept {
			dropped = append(dropped, key)
		}
	}

	return dropped
}

// jsonEqual renders a patch for comparison, so a nil map, an empty map and a dropped field stay
// distinguishable in a failure message.
func jsonEqual(t *testing.T, value any) string {
	t.Helper()

	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %v: %v", value, err)
	}

	return string(payload)
}
