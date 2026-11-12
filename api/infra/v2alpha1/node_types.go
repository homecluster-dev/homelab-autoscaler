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
	"encoding/json"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PowerState string

const (
	PowerStateOn  PowerState = "on"
	PowerStateOff PowerState = "off"
)

// UnmarshalJSON implements custom JSON unmarshaling for PowerState
func (p *PowerState) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	switch s {
	case "on", "true":
		*p = PowerStateOn
	case "off", "false":
		*p = PowerStateOff
	default:
		*p = PowerState(s) // fallback to original value
	}
	return nil
}

// MarshalJSON implements custom JSON marshaling for PowerState
func (p PowerState) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(p))
}

// UnmarshalYAML implements custom YAML unmarshaling for PowerState
func (p *PowerState) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}

	switch s {
	case "on", "true":
		*p = PowerStateOn
	case "off", "false":
		*p = PowerStateOff
	default:
		*p = PowerState(s) // fallback to original value
	}
	return nil
}

// MarshalYAML implements custom YAML marshaling for PowerState
func (p PowerState) MarshalYAML() (interface{}, error) {
	return string(p), nil
}

// NodePhase is the derived, reported phase of a slot: a summary of (the live NodeClaim of the
// slot, the power the provider observes, the liveness of the kubelet). It is written by the
// observation path and by the lifecycle transition path, both of which compute it from those three
// inputs with the one function phase.Derive, and it is never read to decide an action: the state
// machine's input is the live claim and the observation, not this record. See
// docs/development/nodeclaim.md §6 and docs/development/state.md.
type NodePhase string

const (
	// NodePhaseOff: no live claim, no observed machine.
	NodePhaseOff NodePhase = "off"
	// NodePhasePending: a live claim in pending, and no lifecycle Job for it yet.
	NodePhasePending NodePhase = "pending"
	// NodePhaseBooting: a live claim whose startup operation is in flight.
	NodePhaseBooting NodePhase = "booting"
	// NodePhaseReady: a live claim in ready, and a node that is observed Ready and gate-clear.
	NodePhaseReady NodePhase = "ready"
	// NodePhaseDraining: a claim marked for scale-down, or a machine the pool no longer wants,
	// with the node cordoned and the machine still observed up.
	NodePhaseDraining NodePhase = "draining"
)

// NodeSpec defines the desired state of Node: what a slot *is*, and nothing about what anyone
// wants it to do next.
//
// In v1alpha1 this struct carried spec.powerState, a scalar that served as the scale-up request,
// the lock, the target-size arithmetic and the history at once. It is gone in v2alpha1, and there is
// no field in the spec for any of that: desired power is derived, never stored, and a slot is
// wanted exactly when a live NodeClaim names it (docs/development/nodeclaim.md §2). Git is
// the only writer of this spec, so an apply can never contradict a scale-up, and a scale-up can
// never overwrite a declaration.
type NodeSpec struct {
	StartupPodSpec  MinimalPodSpec `json:"startupPodSpec"`
	ShutdownPodSpec MinimalPodSpec `json:"shutdownPodSpec"`
	// KubernetesNodeName is the name of the corev1.Node object this slot's machine registers as:
	// the identity every write on that node object is keyed by - the operator's labels, the taints
	// the slot declares, the labels and annotations it declares and the record of the keys it applied
	// there, the capacity check, the readiness gate.
	//
	// One slot per machine. Admission refuses a second slot that names a node another slot already
	// claims, and the refusal names the holder, because the collision is not cosmetic: the record of
	// the keys a slot applied is keyed by the node object, so two slots claiming one node overwrite
	// each other's record and then remove each other's labels on every pass, patch-ping-pong on a
	// live object with an event each time, and the autoscaler's slot arithmetic cannot say which of
	// the two answers for the machine. A list failure is a warning and never a denial, the rule the
	// webhook already states for the node lookup.
	KubernetesNodeName string      `json:"kubernetesNodeName"`
	Pricing            PricingSpec `json:"pricing"`
	// TaintsToRemove is a list of taints that should be stripped from the node when generating a template.
	// It allows users to declare custom taints that the autoscaler must ignore.
	//
	// It is unrelated to NodeTaints: this field filters taints out of the node template,
	// NodeTaints puts them in. Do not conflate them.
	// +optional
	TaintsToRemove []corev1.Taint `json:"taintsToRemove,omitempty"`

	// NodeTaints are the taints this slot's machine must carry.
	//
	// What the field gives you: what cluster-autoscaler simulates matches what the pool boots
	// with, the taints are declared in Git, and they survive node recreation. The provider puts
	// them in the node template it reports for scale-up simulation, so the autoscaler sees
	// dedicated=remote:NoSchedule before the machine has registered and stops placing pods on a
	// node that rejects them.
	//
	// The controller applies them to the Kubernetes node as soon as the node object exists: the
	// scale-up path stamps them in the same hook that uncordons the node, so the machine is
	// tainted before it becomes schedulable, and every later reconcile re-applies them. A taint
	// you remove by hand with `kubectl taint <node> <key>-` is put back on the next reconcile:
	// the field only adds, and the declaration is the intent.
	//
	// Keys a Kubernetes component owns are skipped, and the skip is recorded as a Warning event
	// on the Node CR: the node lifecycle controller and the kubelet own node.kubernetes.io/
	// not-ready, unreachable, unschedulable, out-of-service, memory-pressure, disk-pressure,
	// pid-pressure and network-unavailable, cluster-autoscaler owns
	// ToBeDeletedByClusterAutoscaler and DeletionCandidateOfClusterAutoscaler, and Cilium owns
	// node.cilium.io/agent-not-ready (the bootstrap taint the scale-up readiness gate waits for,
	// see the Group's spec.nodeReadinessTaints - declaring it here makes the slot's own scale-up
	// wait for a taint the slot keeps putting back). The node prefix is a namespace, not an
	// ownership claim: node.kubernetes.io/role=agent, the taint the k3d setups use, is yours to
	// declare.
	//
	// Declare taints here instead of tainting from a startup Job. A Job can only taint a node
	// that already joined, which is after the autoscaler simulated a placement on an untainted
	// template and after the node was schedulable, so pods get bound to a machine that rejects
	// them.
	//
	// The window this closes is the one the controller drives. It can only patch a node that
	// exists, so a machine you booted by hand, or one that registered while the controller was
	// down, can be Ready and schedulable for a pass before the taint lands. Keep the tolerations
	// of your own startup tooling correct.
	// +optional
	NodeTaints []corev1.Taint `json:"nodeTaints,omitempty"`

	// NodeLabels are the labels this slot's machine must carry on its corev1.Node.
	//
	// What the field gives you is a slot the scheduler, and cluster-autoscaler's simulation of
	// this pool, can pick for a pod that asks for it. A pod with
	// `nodeSelector: {topology.kubernetes.io/zone: fsn1}` is placed by label matching, and
	// cluster-autoscaler decides which group to grow by matching the same selector against the
	// group's node template. A pool whose slots all report the same template cannot answer "which
	// slot?", so a zoned pod is never placed on that pool at all: declaring the zone on the slots
	// that have one is what makes the scale-up possible.
	//
	// The controller applies them to the Kubernetes node as soon as the node object exists - the
	// scale-up path stamps them in the same hook that uncordons the node, so the machine carries
	// them before it is schedulable - and every later reconcile re-asserts them. A label you
	// change by hand on the node is put back, a label the spec drops is removed from the node on
	// the next reconcile, and a label the slot never declared is never touched: the declaration is
	// the intent, and the controller only writes the keys it owns. The writes are idempotent (a
	// node that already matches is not patched) and one Normal event with reason
	// `NodeLabelsApplied` is recorded on the Node CR when a write happens.
	//
	// cluster-autoscaler sees them twice over. What reaches the group's node template is the group's
	// consensus, not one slot's declaration: a key counts only when every slot of the group declares
	// it with the same value, and a key the slots disagree on is left out of the template and reported
	// in the provider's log (nodelabels.Consensus, groupTemplateConsensus). A slot at zero whose pool
	// has no node object at all gets a placeholder carrying that same consensus set, so the estimator
	// can place a zoned pod on a machine that does not exist yet, and places it only where every slot
	// of the pool will carry the label. Existing machines are read from their corev1.Node, which is
	// the object the controller stamps.
	//
	// Keys a Kubernetes component, the autoscaler, a GitOps tool, or this operator owns are refused
	// at admission, with the reason in the message: `infra.homecluster.dev/*` and
	// `homelab-autoscaler.dev/*` (this operator's own domains - the API group's, and the project's,
	// where the coordination locks of a lifecycle operation live),
	// `cluster-autoscaler.kubernetes.io/*` (cluster-autoscaler reads its per-machine controls off the
	// node object), `node.kubernetes.io/*`, `kubernetes.io/*` and `node-role.kubernetes.io/*`
	// (kubelet and control plane), and `topology.kubernetes.io/*` except the two keys that describe
	// where the machine is, `topology.kubernetes.io/zone` and `topology.kubernetes.io/region`. Keys
	// under the allowed topology namespace are the kubelet's to stamp on a machine that knows its own
	// zone; declaring them here is how a pool of boxes in a rack, a site, or a Hetzner FSN1 declares
	// it before the machine boots. A slot object that predates the rule, on a cluster that ran with
	// the webhook switched off, has the same keys skipped at apply with a `SkippedReservedNodeLabelKey`
	// Warning - recorded once, when the set of refused keys changes, and written on the node object
	// under `infra.homecluster.dev/skipped-node-keys`, beside the label that is not there.
	//
	// The rule is enforced twice more, for the same reason: a label in the annotation-reserved
	// namespaces is not refused here (nothing stamps a label under `argocd.argoproj.io/`, so a label
	// there is a label like any other), and the template path filters labels with the label list and
	// annotations with the annotation list, so a label that a `nodeSelector` can match is never
	// silently dropped from the machine shape cluster-autoscaler judges a pod against.
	//
	// A group answers cluster-autoscaler with one template, and the scale-up that template authorises
	// buys whichever slot of the group is free. So a declared key reaches the template only when every
	// slot of the group declares it with the same value: a key they disagree on is left out, and
	// reported in the provider's log, because a template carrying a label some slots will not have buys
	// a machine the pod cannot use. Put the slots you need to tell apart into different groups.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`

	// NodeAnnotations are the annotations this slot's machine must carry on its corev1.Node.
	//
	// They are for the same audience as NodeLabels, on the side of the object that is not part of
	// identity: notes for a scheduler plugin, a cost tool, an operator's own automation, and the
	// metadata a downstream controller reads off a node. Annotations never take part in
	// `nodeSelector` or affinity matching, so they do not influence where cluster-autoscaler
	// places a pod; they are carried into the node template so a consumer reading the template of
	// a pool sees the same metadata it will see on the machine.
	//
	// The apply, drift and removal semantics are the ones described on NodeLabels: stamped on
	// join, re-asserted on drift, removed when the spec drops the key, never written for a key the
	// slot does not declare.
	//
	// Reserved annotation keys are refused at admission: `infra.homecluster.dev/*` and
	// `homelab-autoscaler.dev/*` (this operator's own domains: the records of the keys a slot applied,
	// and the coordination locks of a lifecycle operation), `cluster-autoscaler.kubernetes.io/*`
	// (cluster-autoscaler reads its per-machine controls off the node object - a slot stamping
	// `scale-down-disabled` takes its own machine out of the pool's scale-down behind the pool's
	// back), `argocd.argoproj.io/*` (the sync engine of Argo CD owns its tracking and hook
	// annotations), and `kubectl.kubernetes.io/*` (kubectl owns the client-side records it writes on a
	// live object, first of them `last-applied-configuration`). The label-reserved namespaces
	// `node.kubernetes.io/*`, `kubernetes.io/*` and `node-role.kubernetes.io/*` are not refused here:
	// nothing stamps an annotation in them, so there is no owner to defer to.
	//
	// The group rule is the one described on NodeLabels, and it applies here too: a key the slots of a
	// group disagree on is left out of the template, so the template of a pool does not advertise one
	// machine's serial number to every consumer that reads it.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	NodeAnnotations map[string]string `json:"nodeAnnotations,omitempty"`

	// NodeCapacity is the shape of the machine this slot boots: the resources the kubelet reports
	// as allocatable once the machine joins, not the nominal numbers the hardware is sold as.
	//
	// What the field gives you: a pool whose every slot is powered off can scale up. The node
	// template cluster-autoscaler simulates a placement on is built from a corev1.Node, and a pool
	// at zero has none, so the provider synthesizes a placeholder. Capacity is the one part of the
	// machine's shape a Node CR can supply for that placeholder: with the field unset the template
	// carries no cpu and no memory, no pod with resource requests fits on it, and the autoscaler
	// never calls NodeGroupIncreaseSize, which is why a minSize: 0 pool never comes back up and the
	// workaround is minSize: 1 with a machine that never sleeps.
	//
	// It is declared in the spec rather than copied into the status because the template is needed
	// before a slot has ever booted: a status field fed from a live node is empty in exactly the
	// case it would be read for, and keeping it filled means a pipeline that watches nodes and
	// writes status. One field next to the taints of the same machine is reviewable in Git, and it
	// describes a machine that does not exist yet.
	//
	// Precedence is per resource, not per list: every resource you name replaces what the live node
	// reports in status.capacity and status.allocatable, on the path that copies a registered node
	// as well as on the placeholder, so the shape the autoscaler simulates does not change with
	// which slot of the group happens to be Ready. A resource you do not name keeps the live node's
	// number, which is how pods, ephemeral-storage, hugepages and any GPU or device plugin resource
	// of the real machine survive a cpu-and-memory declaration. Declare every resource your
	// workloads request, because a resource you leave out is not checked (see below) and an
	// incomplete declaration is an incomplete machine to the estimator. You do not have to declare
	// pods: a template that carries none gets the kubelet's default pod ceiling (110) filled in, the
	// scheduler rejects every pod on a node with no pods in its allocatable, and the estimator then
	// finds no placement and never calls NodeGroupIncreaseSize. When the field is unset the live
	// node's own numbers are used, which is the behaviour of every pool that declares nothing.
	//
	// The values must match what the kubelet reports, which is not what the datasheet says: system
	// reservations make a 2 vCPU / 4 GiB machine, a Hetzner CX23 for instance, report 1950m and
	// 3.2Gi. Declare what the node reports, not what the box is sold as. The autoscaler judges pod
	// requests against this list, so a declaration larger than reality places work the machine
	// cannot take and a smaller one leaves capacity unused.
	//
	// The controller compares the declaration with the machine once it registers: a
	// status.allocatable that differs from spec.nodeCapacity by more than five percent on any
	// resource puts a Warning event on the Node CR, reason NodeCapacityMismatch, naming the
	// resource and both values. It warns only, and it compares only the resources you declared: a
	// resource you omitted, memory for instance, is never reported as drift, so a workload that
	// asks for it is placed on a template whose size for it is unknown.
	// +optional
	NodeCapacity corev1.ResourceList `json:"nodeCapacity,omitempty"`
}

type PricingSpec struct {
	HourlyRate string `json:"hourlyRate"`
	PodRate    string `json:"podRate"`
}

type MinimalPodSpec struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Each volume mounts a Secret or ConfigMap
	Volumes        []VolumeMountSpec `json:"volumes,omitempty"`
	ServiceAccount *string           `json:"serviceAccount,omitempty"`
	// PriorityClassName is assigned to the Pod created by the startup/shutdown Job.
	// Use it to keep power-management Pods schedulable (and non-preemptible) under
	// resource pressure, e.g. "system-cluster-critical". Empty means the cluster
	// default (priority 0), which is today's behaviour.
	// +optional
	PriorityClassName *string `json:"priorityClassName,omitempty"`
	// TTLSecondsAfterFinished limits the lifetime of the Job.
	// Defaults to 600 seconds (10 minutes) if not specified.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

type VolumeMountSpec struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	// Either SecretName or ConfigMapName should be set
	SecretName    string `json:"secretName,omitempty"`
	ConfigMapName string `json:"configMapName,omitempty"`
}

// NodeStatus defines the observed state of Node.
type NodeStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Node resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +kubebuilder:validation:Enum=healthy;offline;unknown
	// +optional
	Health string `json:"health,omitempty"`

	// PowerState is the power the provider observes for this slot's machine: on when the
	// corev1.Node reports NodeReady=True, off otherwise. It is an observation, not an intent, and
	// it is the input the state machine reads ("observed power") next to the live claim.
	// +kubebuilder:validation:Enum=on;off
	// +optional
	PowerState PowerState `json:"powerState,omitempty"` // "on" or "off"

	// Phase is the derived report of the slot: see NodePhase. Written from
	// (live claim, observed power, node usability), read by nobody in the operator, and
	// present so that `kubectl get nodes.infra.homecluster.dev` keeps one column that tells an
	// operator what the slot is doing.
	// +kubebuilder:validation:Enum=off;pending;booting;ready;draining
	// +optional
	Phase NodePhase `json:"phase,omitempty"`

	// LastClaim is the digest of the most recent NodeClaim of this slot. Claims are collected
	// after they land (TTL), and this is what survives the collection, so that
	// `kubectl describe node.infra.homecluster.dev <slot>` still answers "who asked, when, how
	// many attempts, why did it end". Written by the claim controller, read by nobody to decide
	// an action; the next claim's status.attempt is seeded from it.
	// +optional
	LastClaim *LastClaimSummary `json:"lastClaim,omitempty"`

	// LastStartupTime is the last time the node was successfully started up.
	// +optional
	LastStartupTime *metav1.Time `json:"lastStartupTime,omitempty"`

	// LastShutdownTime is the last time the node was successfully shut down.
	// +optional
	LastShutdownTime *metav1.Time `json:"lastShutdownTime,omitempty"`

	// LastTransitionTime is the instant status.phase last changed value. It is the "since" an
	// operator reads ("this slot has been booting for 12 minutes"), and it is written with the
	// phase, by the one writer of the report (phase.Record), so a pass that records nothing
	// leaves it alone: it dates the change, not the resync.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// PreviousPhase is the phase the slot reported before status.phase, empty when the slot has
	// reported exactly one phase. It is the other half of the sentence a phase cannot say on its
	// own: "off -> booting" is a machine coming up, "ready -> booting" is a machine the pool lost.
	// +kubebuilder:validation:Enum="";off;pending;booting;ready;draining
	// +optional
	PreviousPhase NodePhase `json:"previousPhase,omitempty"`

	// ActivationsTotal is the monotonic count of the slot's boots: every pass that recorded the
	// report entering `booting` counted one. It is the slot's lifetime counter ("how many times has
	// this machine been woken up"), and it never resets, so it is comparable to the machine, not
	// to the clock. It carries no `omitempty`: a zero is a number an operator reads ("this slot has
	// never been booted"), and a field omitted at zero renders an empty column, which is a different
	// and worse sentence. The default is what gives that zero to a slot written before the field
	// existed: without it its column renders `<none>`.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	ActivationsTotal int32 `json:"activationsTotal"`

	// ActivationsLast24H is how many of those boots are inside the last 24 hours. It is a
	// materialized count, not a third record of the same fact: it is the sum of
	// status.recentActivations - the ledger of the boots inside the window - recomputed on every
	// write of the report, and pruned and recomputed on the pass that watches the oldest boot expire
	// (see phase.NextTransitionExpiry). Churn - the pool that boots a machine for one pod and powers
	// it off ten minutes later - is visible here and nowhere else in the API.
	//
	// It is **not** counted from status.recentTransitions: the ring is the display history, is capped
	// at 32 rows, and a slot that churns writes more rows a day than the ring keeps, so a pool that
	// booted 40 times today would report 16 and the column would go flat exactly when the question
	// gets interesting. The ledger's rows are one timestamp each, so the count is honest to
	// ActivationWindowCap boots a day, and past that bound it is a floor, not a guess: a slot
	// reporting 512 is a slot the pool woke up at least 512 times.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	ActivationsLast24H int32 `json:"activationsLast24H"`

	// RecentTransitions is the bounded, time-boxed ring of the slot's phase changes: the last
	// TransitionRingCap of them, pruned to TransitionWindow. It is the shape of the pool over a
	// day (`off -> booting -> ready -> draining -> off` at 03:00, and again at 09:00), readable
	// with no second command.
	//
	// It is observability and nothing else: no reconciler, hook, provider call or state machine
	// reads it, and it is bounded on purpose - a day of a slot is 32 rows, not a growing list in
	// etcd. The rows are appended by the pass that records each change, so their order is the order
	// the writes landed and not the order the changes happened: two writers racing inside one second
	// (the observation path and the transition path) can be listed out of chronological order, which
	// is a display detail of a churn and not a fact any code reads.
	//
	// It is not the ledger activationsLast24H is counted from - that is status.recentActivations,
	// which has no display cap to trade against.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	RecentTransitions []NodeTransition `json:"recentTransitions,omitempty"`

	// RecentActivations is the ledger of the slot's boots inside the 24 h window: the instant of
	// every entry into `booting` the window still contains, oldest first, capped at
	// ActivationWindowCap. activationsLast24H is its length.
	//
	// Why a second ledger next to the ring: the ring is bounded for display (32 rows), and a bound
	// that a count is read through is a bound on the count - a slot that churns 40 times a day has
	// 80 rows a day and the ring keeps 32 of them, so the honest number and the displayed number
	// cannot be the same field. This ledger holds nothing but the instants, so 512 of them cost less
	// than 32 ring rows do, and the count is honest to 512 boots a day. Past the cap the count is a
	// floor, and it says so by sitting at the cap.
	//
	// It is pruned by the same cutoff, on the same pass, by the one writer of the record
	// (phase.Record). Like the ring it is observability: nothing in the operator reads it to decide
	// anything, and internal/phase/hygiene_test.go enforces that.
	// +optional
	// +kubebuilder:validation:MaxItems=512
	// +listType=atomic
	RecentActivations []metav1.Time `json:"recentActivations,omitempty"`
}

// NodeTransition is one recorded change of Node.status.phase: the moment, the phase it came from,
// the phase it went to, and one line saying why. `from` is empty for the first phase a slot ever
// reported, which is a seeding, not a transition of a machine.
type NodeTransition struct {
	// At is the instant the report changed value.
	// +optional
	At *metav1.Time `json:"at,omitempty"`

	// From is the phase the slot reported before this one.
	// +kubebuilder:validation:Enum="";off;pending;booting;ready;draining
	// +optional
	From NodePhase `json:"from,omitempty"`

	// To is the phase the slot reports since.
	// +kubebuilder:validation:Enum=off;pending;booting;ready;draining
	To NodePhase `json:"to"`

	// Reason is the one line that says what the report was derived from: the live claim of the
	// slot, the power the provider observed, the usability of the node object (see
	// phase.Reason). It names the inputs, it does not narrate a transition.
	// +optional
	// +kubebuilder:validation:MaxLength=320
	Reason string `json:"reason,omitempty"`
}

// LastClaimSummary is the digest of the last NodeClaim of a slot. Its field set is the design's
// (docs/development/nodeclaim.md §8): the story of the episode that ended, without a second command.
type LastClaimSummary struct {
	// Name is the name of the claim, so an operator can find it while it still exists.
	// +optional
	Name string `json:"name,omitempty"`

	// Phase is the phase the claim ended in: failed and completed are terminal, and this is the
	// sentence "the last attempt of this slot ended like this".
	// +kubebuilder:validation:Enum=pending;booting;ready;failed;completed
	// +optional
	Phase NodeClaimPhase `json:"phase,omitempty"`

	// Attempt is the attempt number the claim carried (1..N for the slot). Seeding the next
	// claim reads it; see the reset rule in internal/claim.SeedAttempt.
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// ShutdownAttempts is the scale-down attempts the claim had spent when the episode ended. It is
	// the record the slot reconciler reads to decide whether to keep firing a shutdown on a machine
	// that stays up when no claim of the slot is left to carry the count (see
	// claim.ScaleDownStreakSpent), and it is a counter of its own: the boot attempts of the episode are
	// in Attempt, and a shutdown that will not land must not spend them.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ShutdownAttempts int32 `json:"shutdownAttempts,omitempty"`

	// LastShutdownAttemptAt is when the last of those attempts was counted. The streak reset's grace is
	// measured from it, so a slot whose pool script has been fixed is not sentenced by the attempts of
	// the episodes that ended (docs/development/nodeclaim.md §4).
	// +optional
	LastShutdownAttemptAt *metav1.Time `json:"lastShutdownAttemptAt,omitempty"`

	// Call is the gRPC entry point that created the claim (IncreaseSize, DecreaseTargetSize,
	// DeleteNodes) - the attribution of the request that owned this slot.
	// +optional
	Call string `json:"call,omitempty"`

	// Group is the node group the request was for.
	// +optional
	Group string `json:"group,omitempty"`

	// Reason is the one-line outcome: DeadlineExceeded, StartupJobFailed, ReadinessGateTimeout,
	// CancelledByDecreaseTargetSize, NodeDeleted, AttemptLimitExceeded.
	// +optional
	Reason string `json:"reason,omitempty"`

	// RequestedAt is when the request was made.
	// +optional
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`

	// ScaledDownAt is the instant a scale-down named this claim: the deletionTimestamp
	// NodeGroupDeleteNodes, NodeGroupDecreaseTargetSize, a hand delete or the TTL collection of an
	// expired claim landed on it (claim.ScaleDownRequestedAt is the same reading). Nil says no
	// scale-down ended this episode, which is what tells a scale-up's digest from a scale-down's.
	//
	// It exists because the instant a scale-down asked is **not** the instant the episode's request
	// was made. DeleteNodes rewrites spec.request.call, spec.request.nodes and spec.requestedBy on a
	// claim that can be days old and does not touch spec.requestedAt, so "RequestedAt three days ago
	// with Call DeleteNodes" is the normal shape of a scale-down and not a bug - and a machine taken
	// back 24 minutes ago was being dated three days ago. The pool's `LASTSCALE` reads this field for
	// a scale-down episode (group_controller.lastScaleOf), so the column is the instant the pool
	// shrank.
	// +optional
	ScaledDownAt *metav1.Time `json:"scaledDownAt,omitempty"`

	// DeadlineAt is the budget the claim carried.
	// +optional
	DeadlineAt *metav1.Time `json:"deadlineAt,omitempty"`

	// FinishedAt is when the claim reached its terminal phase.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// The columns of `kubectl get nodes.infra.homecluster.dev`: the shape of a slot at a glance,
// without a second command. NAME is the built-in column of every Kubernetes table, not a
// printcolumn. The two that were there first (Powerstate, Phase) keep their names and casing.
//
// A duration column is not possible: a printer column is a JSONPath read off the object, so it can
// select a value and cannot subtract one. "up for 3h" is therefore a read-time command over
// status.phase + status.lastTransitionTime, documented in the runbook
// (docs/troubleshooting/common-issues.md, "Read the pool at a glance").
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Powerstate",type=string,JSONPath=".status.powerState",description="Current power state"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase",description="Derived phase of the slot (live claim + observed power + node usability)"
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=".metadata.labels.group",description="The pool this slot belongs to"
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=".spec.nodeLabels['topology\\.kubernetes\\.io/zone']",description="The zone the slot declares (spec.nodeLabels), empty for a pool that does not zone its slots"
// +kubebuilder:printcolumn:name="Since",type=date,JSONPath=".status.lastTransitionTime",description="When status.phase last changed value"
// +kubebuilder:printcolumn:name="On24h",type=integer,JSONPath=".status.activationsLast24H",description="Boots of this machine inside the last 24h"

// Node is the Schema for the nodes API
type Node struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Node
	// +required
	Spec NodeSpec `json:"spec"`

	// status defines the observed state of Node
	// +optional
	Status NodeStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// NodeList contains a list of Node
type NodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Node `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Node{}, &NodeList{})
}
