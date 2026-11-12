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
	"fmt"
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

// OverlayLabels returns current with the declared *labels* applied on top: declared keys win,
// everything else is kept, nothing is removed. It is the template's rule, not the node's: a node
// template is rebuilt from scratch on every call, there is no previous write to undo, and a key the
// slot does not declare is a key of the machine the template was copied from, which the template keeps.
//
// The predicate is the *label* one (ReservedLabelKey), and that is the whole reason labels and
// annotations have separate entry points: the reserved sets are not the same, and one shared
// predicate silently drops a key that is refused on one side only. A label under
// `argocd.argoproj.io/` is legal (nothing stamps an annotation there, so nothing stamps a label
// there either) and is exactly the key a `nodeSelector` matches: filtering the label overlay with
// the annotation list drops it from the template cluster-autoscaler simulates, and the pod that
// selects it is never placed - the failure the feature exists to remove, reproduced quietly.
//
// Keys in the label-reserved namespaces are skipped, so a declaration that predates the webhook's
// reserved-key list (an object created while the webhook was down, a cluster restored from an old
// etcd) cannot stamp a key the kubelet owns - `kubernetes.io/hostname` in particular - onto the
// template the estimator judges pods against.
func OverlayLabels(current, declared map[string]string) map[string]string {
	return overlay(current, declared, ReservedLabelKey)
}

// OverlayAnnotations is OverlayLabels for annotations, with the annotation predicate
// (ReservedAnnotationKey): this operator's domains, cluster-autoscaler's, Argo CD's, kubectl's.
func OverlayAnnotations(current, declared map[string]string) map[string]string {
	return overlay(current, declared, ReservedAnnotationKey)
}

func overlay(current, declared map[string]string, isReserved func(string) (bool, string)) map[string]string {
	if len(declared) == 0 {
		return current
	}

	next := make(map[string]string, len(current)+len(declared))

	for key, value := range current {
		next[key] = value
	}

	for _, key := range sortedKeys(declared) {
		if reserved, _ := isReserved(key); reserved {
			continue
		}

		next[key] = declared[key]
	}

	return next
}

// GroupConflict is one key the slots of a group do not agree on, and the evidence of the
// disagreement: which slot declares which value, and which slots declare nothing at all. It is
// rendered into the log line an operator has to be able to act on, which means naming the slots.
type GroupConflict struct {
	// Key is the declared key the group disagrees on, labels or annotations, whichever side asked.
	Key string
	// Values maps each declared value to the slots that declare it. More than one entry is the
	// plain disagreement: two slots, two values.
	Values map[string][]string
	// Silent lists the slots that do not declare the key. Their machines never carry it, so a
	// template that carries it promises a label the group cannot guarantee.
	Silent []string
}

// Note renders the conflict for a human, naming the key, the values, the slots, in that order.
func (c GroupConflict) Note() string {
	parts := make([]string, 0, len(c.Values))

	for _, value := range sortedMapKeys(c.Values) {
		parts = append(parts, fmt.Sprintf("%q on %s", value, strings.Join(c.Values[value], ", ")))
	}

	if len(c.Silent) > 0 {
		parts = append(parts, fmt.Sprintf("not declared at all on %s", strings.Join(c.Silent, ", ")))
	}

	return fmt.Sprintf("%s: %s", c.Key, strings.Join(parts, "; "))
}

// SlotDeclaration is one slot's declaration of labels, or of annotations, plus the name the
// conflict has to be reported with.
type SlotDeclaration struct {
	Name     string
	Declared map[string]string
}

// Consensus is the fail-closed rule of a group's node template: it returns the keys every slot of
// the group declares with the same value, and the keys it refuses to answer with.
//
// `NodeGroupTemplateNodeInfo` is asked about a *group* and answers with one node, while the
// scale-up it authorises buys whichever slot the provider has free, sorted by name. A key some
// slots declare and others do not is therefore a promise the group cannot keep: the estimator
// places a pod on the template because it carries `topology.kubernetes.io/zone: fsn1`,
// `IncreaseSize` boots the slot that declares `hel1`, or declares nothing, the machine is bought,
// the pod is still `Pending`, and the money is spent. Omitting the key is what makes the template
// a fact about the whole group: the estimator then matches nothing on it, plans nothing, and buys
// no machine for a pod this pool cannot host. Not an error: the group is still exactly what it is
// for every pod that does not select the disputed key, and a scale-up that fails an admission
// check for a reason the caller can report is a worse outcome than a scale-up that never starts.
//
// A group of one slot, and a group whose slots declare the same keys with the same values, have no
// conflicts, which is every pool the docs describe.
func Consensus(slots []SlotDeclaration) (map[string]string, []GroupConflict) {
	if len(slots) == 0 {
		return nil, nil
	}

	// key -> value -> the slots that declare it, plus the slots that do not declare it at all.
	declaredBy := map[string]map[string][]string{}
	missing := map[string][]string{}

	for _, slot := range slots {
		seen := make(map[string]bool, len(slot.Declared))

		for _, key := range sortedKeys(slot.Declared) {
			value := slot.Declared[key]

			if declaredBy[key] == nil {
				declaredBy[key] = map[string][]string{}
			}

			declaredBy[key][value] = append(declaredBy[key][value], slot.Name)
			seen[key] = true
		}

		for key := range declaredBy {
			if !seen[key] {
				missing[key] = append(missing[key], slot.Name)
			}
		}
	}

	agreed := make(map[string]string, len(declaredBy))

	// Every key that is not a promise the whole group makes is a conflict, so the two slices of this
	// loop add up to the keys that were declared: the capacity is the bound, and a group that agrees
	// on everything allocates nothing.
	conflicts := make([]GroupConflict, 0, len(declaredBy))

	for _, key := range sortedMapKeys(declaredBy) {
		values := declaredBy[key]

		// Sorted, because the note is the operator's evidence: the same pool renders the same line.
		silent := append([]string(nil), missing[key]...)
		sort.Strings(silent)

		if len(values) == 1 && len(silent) == 0 {
			for value := range values {
				agreed[key] = value
			}

			continue
		}

		conflicts = append(conflicts, GroupConflict{Key: key, Values: values, Silent: silent})
	}

	return agreed, conflicts
}

// RenderConflicts renders a group's conflicts onto one line, for a log field an operator can grep.
func RenderConflicts(conflicts []GroupConflict) string {
	notes := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		notes = append(notes, conflict.Note())
	}

	return strings.Join(notes, " | ")
}

// sortedMapKeys is sortedKeys for a map of any value type: every rendering of a set of keys, in a
// record annotation and in a conflict note alike, is sorted, so the same content is the same string
// and a resync that changes nothing writes and logs nothing new.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
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
