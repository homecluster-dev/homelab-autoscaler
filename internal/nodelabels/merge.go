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
	"sort"
	"strings"
)

// Change is what one reconcile of a declaration did to the node object: the keys it added, the
// keys it overwrote, the keys it removed. It is the audit trail of the write, in the log line and
// in the Event, and it answers "why is this label on my node, and who put it there" without a
// diff of two YAML files.
type Change struct {
	Added   []string
	Updated []string
	Removed []string
}

// Empty reports whether nothing changed, which is the case the controller must not write, and
// must not record an Event for.
func (c Change) Empty() bool {
	return len(c.Added) == 0 && len(c.Updated) == 0 && len(c.Removed) == 0
}

// ChangeNote renders the two changes of one apply for a human: "labels: +a ~c -e; annotations: +f".
// Compact enough for one log field and one Event message, and empty when nothing changed.
//
// The syntax is deliberate: `+` wrote a key that was not there, `~` rewrote a key that was, `-`
// removed a key the declaration dropped. `kubectl describe` shows the same string the log line
// shows, so "what did the controller do to that node" is one read of the slot.
func ChangeNote(labels, annotations Change) string {
	parts := make([]string, 0, 2)

	for _, section := range []struct {
		name string
		ch   Change
	}{{"labels", labels}, {"annotations", annotations}} {
		if section.ch.Empty() {
			continue
		}

		tokens := make([]string, 0, len(section.ch.Added)+len(section.ch.Updated)+len(section.ch.Removed))

		for _, key := range section.ch.Added {
			tokens = append(tokens, "+"+key)
		}

		for _, key := range section.ch.Updated {
			tokens = append(tokens, "~"+key)
		}

		for _, key := range section.ch.Removed {
			tokens = append(tokens, "-"+key)
		}

		parts = append(parts, section.name+": "+strings.Join(tokens, " "))
	}

	return strings.Join(parts, "; ")
}

// Reconcile is the whole rule of the apply, over one map (labels in one call, annotations in the
// other):
//
//   - every declared key is present in the result with the declared value. A key the node already
//     carries with a different value is overwritten: the declaration is the intent, the node
//     object says what the machine happens to look like, and the drift an operator introduced with
//     `kubectl label` is put back on the next pass;
//   - a key the slot applied before, in owned, and no longer declares is removed. This is what
//     makes "delete the key from Git" the same action as "delete the label from the node", which
//     is the only way a declaration stays true of a machine that lives for months;
//   - a key that is neither declared nor recorded as owned is left alone, whatever its value. The
//     controller has no claim on a label it did not write: the kubelet's own labels, a
//     topology label a cloud integration stamped, and any label another tool added are not this
//     field's business. Touching them is the two-writers-fighting failure the reserved-key list
//     exists to prevent, and the ownership record is what stops the same failure from the other
//     side;
//   - the result is a new map. The Node CR read out of the manager's cache, and the corev1.Node
//     read out of it, are never aliased into a write.
//
// The return reports what happened, so a caller that must not churn the API (the reconciler, on
// every resync) writes only when something differs.
func Reconcile(current, declared, owned map[string]string) (next map[string]string, change Change) {
	next = make(map[string]string, len(current)+len(declared))

	for key, value := range current {
		next[key] = value
	}

	for _, key := range sortedKeys(declared) {
		value := declared[key]

		previous, present := next[key]

		switch {
		case !present:
			change.Added = append(change.Added, key)
		case previous != value:
			change.Updated = append(change.Updated, key)
		}

		next[key] = value
	}

	for _, key := range sortedKeys(owned) {
		if _, declared := declared[key]; declared {
			continue
		}

		if _, present := next[key]; !present {
			continue
		}

		delete(next, key)
		change.Removed = append(change.Removed, key)
	}

	return next, change
}

// Overlay returns current with the declared map applied on top: declared keys win, everything else
// is kept, nothing is removed. It is the template's rule, not the node's: a node template is
// rebuilt from scratch on every call, there is no previous write to undo, and a key the slot does
// not declare is a key of the machine the template was copied from, which the template keeps.
//
// Reserved keys are skipped, so a declaration that predates the webhook's reserved-key list (an
// object created while the webhook was down, a cluster restored from an old etcd) cannot stamp a
// key the operator owns onto the template cluster-autoscaler simulates.
func Overlay(current, declared map[string]string) map[string]string {
	if len(declared) == 0 {
		return current
	}

	next := make(map[string]string, len(current)+len(declared))

	for key, value := range current {
		next[key] = value
	}

	for _, key := range sortedKeys(declared) {
		if reserved, _ := ReservedLabelKey(key); reserved {
			continue
		}

		if reserved, _ := ReservedAnnotationKey(key); reserved {
			continue
		}

		next[key] = declared[key]
	}

	return next
}

// EncodeOwnerRecord renders the keys a slot applied, for the record annotation the controller
// leaves on the node object (AppliedLabelsAnnotation, AppliedAnnotationsAnnotation). Sorted and
// comma-separated, so the same set of keys is always the same string and a resync that changes
// nothing writes nothing.
func EncodeOwnerRecord(keys []string) string {
	encoded := append([]string(nil), keys...)
	sort.Strings(encoded)

	return strings.Join(encoded, ",")
}

// DeclaredKeys returns the keys of a declaration, sorted. It is the content of the owner record
// after a write (the apply hands it the declaration it filtered of reserved keys), and sorting
// keeps the record stable: the same set of keys is the same string, so a resync writes nothing.
func DeclaredKeys(declared map[string]string) []string {
	return sortedKeys(declared)
}

// DecodeOwnerRecord parses a record annotation back into the keys it lists. An empty, missing or
// whitespace-only value means "this slot owns nothing on this node", which is the state of every
// node written before this field existed, and of every slot whose declaration is empty.
func DecodeOwnerRecord(record string) map[string]string {
	owned := map[string]string{}

	for _, key := range strings.Split(record, ",") {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}

		owned[key] = ""
	}

	return owned
}
