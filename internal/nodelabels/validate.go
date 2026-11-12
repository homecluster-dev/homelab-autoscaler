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

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// MaxEntries bounds how many labels, and how many annotations, one slot may declare.
//
// The number is far above any honest use - a slot carries a zone, a rack, a pool name, a handful
// of notes - and it is there for the two ways a slot spec is written: by hand, and by a pipeline
// that templates it out of a CMDB. A map with no bound is a manifest that can grow into a Node
// object the API server rejects on the *node* (the 256 kB annotations limit) while the Node CR was
// accepted, which is a failure that shows up as a machine that joined and is never reconciled.
// Bounding it at admission, where the message names the field, is the cheap version of the same
// story. The CRD carries the same bound as `maxProperties: 64`, so a cluster with the webhook
// switched off still cannot exceed it.
const MaxEntries = 64

// ValidateLabels validates spec.nodeLabels: every key a valid Kubernetes label key, every value a
// valid label value, the count bounded, and no key in a namespace an operator owns (ReservedLabelKey).
//
// The syntax rules are the API machinery's own (`k8s.io/apimachinery/pkg/util/validation`), so the
// webhook refuses exactly what the API server would refuse on the corev1.Node the controller
// patches later. That is the point of validating here: a slot whose label key is malformed is
// accepted as a Node CR, and its label never lands on the node, so the pod that selects on it
// never schedules and the pool never grows - a silent failure at the far end of a long chain.
// Here it fails at `kubectl apply`, naming the key.
//
// The path is the field's own (`spec.nodeLabels`), and a keyed error names the entry, so a
// manifest with one bad key out of twenty says which one.
func ValidateLabels(labels map[string]string, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	if len(labels) > MaxEntries {
		errs = append(errs, field.TooMany(path, len(labels), MaxEntries))
	}

	for _, key := range sortedKeys(labels) {
		value := labels[key]

		for _, msg := range validation.IsQualifiedName(key) {
			errs = append(errs, field.Invalid(path.Key(key), key, msg))
		}

		for _, msg := range validation.IsValidLabelValue(value) {
			errs = append(errs, field.Invalid(path.Key(key), value, msg))
		}

		if reserved, reason := ReservedLabelKey(key); reserved {
			errs = append(errs, field.Invalid(path.Key(key), key, reservedKeyNote(reason)))
		}
	}

	return errs
}

// ValidateAnnotations validates spec.nodeAnnotations: every key a valid annotation key (a
// qualified name, compared case-insensitively, which is the rule the API machinery applies to
// annotation keys), the total size under the limit the API server enforces on the node object, the
// count bounded, and no key in a namespace an operator owns (ReservedAnnotationKey).
//
// Annotation values are not syntax-checked: the API machinery checks nothing about them beyond the
// total size of the map, and a check stricter than the API server's would refuse a note the node
// would have accepted.
func ValidateAnnotations(annotations map[string]string, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	if len(annotations) > MaxEntries {
		errs = append(errs, field.TooMany(path, len(annotations), MaxEntries))
	}

	for _, key := range sortedKeys(annotations) {
		for _, msg := range validation.IsQualifiedName(strings.ToLower(key)) {
			errs = append(errs, field.Invalid(path.Key(key), key, msg))
		}

		if reserved, reason := ReservedAnnotationKey(key); reserved {
			errs = append(errs, field.Invalid(path.Key(key), key, reservedKeyNote(reason)))
		}
	}

	// The same limit, and the same helper, the API server applies to the annotations of the
	// node object this declaration is written onto.
	if err := apivalidation.ValidateAnnotationsSize(annotations); err != nil {
		errs = append(errs, field.Invalid(path, len(annotations), err.Error()))
	}

	return errs
}

// reservedKeyNote renders the refusal of a reserved key so the message answers "why" and not
// just "no".
func reservedKeyNote(reason string) string {
	return "reserved key, not writable from a slot: " + reason +
		". Declare a key in a namespace you own (a pool name, a rack, a site), " +
		"or topology.kubernetes.io/zone and topology.kubernetes.io/region for placement."
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
