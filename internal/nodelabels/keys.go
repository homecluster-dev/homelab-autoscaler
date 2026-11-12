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

import "strings"

// The keys of the corev1.Node a slot must not declare in spec.nodeLabels /
// spec.nodeAnnotations, because something else adds and removes them on its own sync loop. Two
// writers fighting over one key never converges, and the owner always wins the last write: the
// label the scheduler matches is the one the kubelet stamped, and the declaration is what the
// autoscaler simulated. The admission webhook refuses them with the reason in the message, so the
// failure is at `kubectl apply` time, not in a pool that never scales up.
//
// They are prefix rules, unlike the protected *taint* keys in internal/taints, which are a set of
// exact keys: a taint namespace has a handful of stamped keys and one key that shares the namespace
// (node.kubernetes.io/role) is the operator's to declare, while a label namespace is full of keys
// the kubelet and the controllers write continuously and rotates (kubernetes.io/hostname,
// /os, /arch, the version keys).

// The domain of this project, and the domain of its API group. Both are reserved, for labels and
// for annotations, because both are where this operator writes its own bookkeeping:
// `infra.homecluster.dev/*` is the API group's domain (the labels the operator stamps on a node
// object, the records of the keys it applied), and `homelab-autoscaler.dev/*` is the project's
// domain (the coordination locks the state machine takes on a slot, and the force-reconcile marker
// an operator sets). Neither is a namespace a slot's declaration has anything to add to.
const (
	operatorDomainPrefix = "infra.homecluster.dev/"
	projectDomainPrefix  = "homelab-autoscaler.dev/"

	// autoscalerPrefix is cluster-autoscaler's own namespace: it reads its per-machine controls off
	// the node object (cluster-autoscaler.kubernetes.io/scale-down-disabled), and stamps nothing
	// there for a slot to declare.
	autoscalerPrefix = "cluster-autoscaler.kubernetes.io/"
)

// reservedLabelPrefixes is the label-key namespaces spec.nodeLabels refuses to write, each with
// the reason an operator reads in the admission message. `exceptions` lists the keys inside a
// prefix that are allowed anyway.
//
// topology.kubernetes.io is the one prefix with exceptions, and the two allowed keys are the
// motivating case for the whole field: a pod with
//
//	nodeSelector: {topology.kubernetes.io/zone: fsn1}
//
// is placed by label matching, and cluster-autoscaler grows the pool whose *template* carries that
// label. Nothing in a homelab writes the zone of a machine - there is no cloud node controller to
// ask - so the slot's declaration is the only source of the fact, and the only thing the estimator
// can match against. The rest of the namespace (topology.kubernetes.io/zone-labels,
// /failures-domain, /region of a machine the cloud controller already knows) stays with its owner.
var reservedLabelPrefixes = []reservedPrefix{
	{
		prefix: operatorDomainPrefix,
		reason: "the homelab-autoscaler operator owns this namespace: it stamps infra.homecluster.dev/group " +
			"and /node on the node object, and records the keys it applies and the keys it refused there",
	},
	{
		prefix: projectDomainPrefix,
		reason: "this project's own domain: the coordination locks a lifecycle operation takes on a slot " +
			"(homelab-autoscaler.dev/operation-lock, /lock-owner, /lock-timestamp, /lock-timeout) live under it",
	},
	{
		prefix: autoscalerPrefix,
		reason: "cluster-autoscaler owns this namespace: it reads its per-machine controls off the node " +
			"object (cluster-autoscaler.kubernetes.io/scale-down-disabled), so a slot declaring a key here is " +
			"a slot rewriting the autoscaler's settings on its own machine",
	},
	{
		prefix: "node.kubernetes.io/",
		reason: "the kubelet and the node lifecycle controller own this namespace: role, os, arch, " +
			"instance-type, exclude-home, drain-status and the keys of the lifecycle conditions are " +
			"written and rotated by them",
	},
	{
		prefix: "kubernetes.io/",
		reason: "the kubelet, the node lifecycle controller and the cloud integrations own this " +
			"namespace: hostname, os, arch, and the version and out-of-service keys",
	},
	{
		prefix: "node-role.kubernetes.io/",
		reason: "the role labels are written by the control plane and by the tooling that joins a " +
			"machine (kubeadm, k3s, RKE2), not by a slot's declaration",
	},
	{
		prefix: topologyPrefix,
		reason: "the topology of a machine is stamped by the kubelet and by the cloud node controller " +
			"when a cluster has one; declare topology.kubernetes.io/zone or topology.kubernetes.io/region " +
			"of a box whose location no controller knows, never the rest",
		exceptions: []string{TopologyZoneLabel, TopologyRegionLabel},
	},
}

// reservedAnnotationPrefixes is the annotation-key namespaces spec.nodeAnnotations refuses to
// write. The prefixes, in plain words:
//
//   - infra.homecluster.dev/ and homelab-autoscaler.dev/: this operator's own domains. The first
//     carries the records this package writes on the node object (the keys a slot applied, the keys
//     it refused); the second carries the coordination locks a lifecycle operation takes on a slot
//     (homelab-autoscaler.dev/operation-lock, on the Node CR, not on the node object). A slot
//     declaring a key under either fights, or forges, the write that tracks it.
//   - cluster-autoscaler.kubernetes.io/: cluster-autoscaler reads its per-machine controls off the
//     node object, scale-down-disabled above all. A slot stamping it is a machine that removes
//     itself from the pool's scale-down behind the pool's back - the pool has spec.minSize and the
//     claim's lifecycle for that, and they are the parts the model can see.
//   - argocd.argoproj.io/: Argo CD owns its tracking and hook annotations
//     (argocd.argoproj.io/tracking-id, /sync-wave, /sync-options). A slot stamping them onto a
//     node object is a slot rewriting the sync engine's metadata on an object it does not manage.
//   - kubectl.kubernetes.io/: kubectl owns the client-side records it writes on a live object,
//     first of them kubectl.kubernetes.io/last-applied-configuration. It does *not* write
//     annotations when it cordons or drains a node - a cordon is spec.unschedulable, a drain adds
//     the node.kubernetes.io/exclude-from-external-load-balancers *label* - and those are covered
//     by the label rules, not by this one.
//
// The node.kubernetes.io/ and kubernetes.io/ annotation namespaces are deliberately not listed:
// they carry no annotation that a Kubernetes component adds and removes on a node object, so
// there is no owner to defer to, and a note under them is the operator's own.
var reservedAnnotationPrefixes = []reservedPrefix{
	{
		prefix: operatorDomainPrefix,
		reason: "the homelab-autoscaler operator owns this namespace: the records of the keys a slot " +
			"applied, and of the keys it refused, live on the node object under it",
	},
	{
		prefix: projectDomainPrefix,
		reason: "this project's own domain: the coordination locks a lifecycle operation takes on a slot " +
			"(homelab-autoscaler.dev/operation-lock, /lock-owner, /lock-timestamp, /lock-timeout) live under it",
	},
	{
		prefix: autoscalerPrefix,
		reason: "cluster-autoscaler owns this namespace: it reads its per-machine controls off the node " +
			"object (cluster-autoscaler.kubernetes.io/scale-down-disabled), and the pool's scale-down " +
			"behaviour is the Group's configuration, not a slot's annotation",
	},
	{
		prefix: "argocd.argoproj.io/",
		reason: "Argo CD owns its tracking and sync annotations on the objects it manages",
	},
	{
		prefix: "kubectl.kubernetes.io/",
		reason: "kubectl owns the client-side records it writes on a live object, " +
			"kubectl.kubernetes.io/last-applied-configuration above all",
	},
}

// reservedPrefix is one refused namespace, with the reason it is refused and the keys inside it
// that are allowed anyway.
type reservedPrefix struct {
	prefix     string
	reason     string
	exceptions []string
}

const (
	// topologyPrefix is the topology label namespace. Everything under it belongs to the
	// topology machinery except the two keys below.
	topologyPrefix = "topology.kubernetes.io/"

	// TopologyZoneLabel is the zone a machine sits in: topology.kubernetes.io/zone. A slot
	// declares it, the controller stamps it on the node, and it is what a pod's
	// nodeSelector and a topologySpreadConstraints' `key` match against.
	TopologyZoneLabel = topologyPrefix + "zone"

	// TopologyRegionLabel is the region a machine sits in:
	// topology.kubernetes.io/region. Same argument as the zone, one level up.
	TopologyRegionLabel = topologyPrefix + "region"
)

// AppliedLabelsAnnotation and AppliedAnnotationsAnnotation are the operator's records, written on
// the corev1.Node, of which keys of that node a slot's declaration put there.
//
// The record is what makes removal possible, and removal is what makes the declaration true: a
// key the spec drops must come off the node, and the only honest way to know a key is the slot's
// is to have written it down. Without it the controller would have to guess - "delete every node
// label that is not in the spec" deletes the kubelet's own labels, and "delete nothing" leaves a
// label the operator removed from Git on a machine forever.
//
// The value is the declared keys, sorted and comma-separated, so the record is stable (a resync
// that changes nothing writes nothing) and readable with `kubectl get node -o jsonpath` for the
// question "which labels on this node does the slot own?".
const (
	AppliedLabelsAnnotation      = "infra.homecluster.dev/applied-node-labels"
	AppliedAnnotationsAnnotation = "infra.homecluster.dev/applied-node-annotations"

	// SkippedKeysAnnotation records the declared keys this writer refused, on the node object, so
	// the answer to "the slot declares it, why is it not on the node" is readable where the label
	// is missing. It is also what keeps the Warning event honest: the event fires when this string
	// changes, not on every resync of a slot that keeps declaring the same key it may not write.
	SkippedKeysAnnotation = "infra.homecluster.dev/skipped-node-keys"
)

// ReservedLabelKey reports whether a label key is in a namespace this operator must not write,
// and returns the reason an operator can read in the admission message.
func ReservedLabelKey(key string) (bool, string) {
	return reserved(key, reservedLabelPrefixes)
}

// ReservedAnnotationKey reports whether an annotation key is owned by something that stamps it
// itself (this operator, Argo CD, kubectl), and returns the reason.
//
// The comparison is case-insensitive, which is the rule the API machinery applies to annotation
// keys (they are qualified names, and the domain part is case-insensitive).
func ReservedAnnotationKey(key string) (bool, string) {
	return reserved(strings.ToLower(key), reservedAnnotationPrefixes)
}

func reserved(key string, prefixes []reservedPrefix) (bool, string) {
	for _, p := range prefixes {
		if !strings.HasPrefix(key, p.prefix) {
			continue
		}

		for _, allowed := range p.exceptions {
			if key == allowed {
				return false, ""
			}
		}

		return true, p.reason
	}

	return false, ""
}
