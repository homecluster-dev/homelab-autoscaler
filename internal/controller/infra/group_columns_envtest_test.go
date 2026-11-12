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

package controller

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
)

// newPool commits a uniquely-named Group and returns its name. Every spec of the pool-shape
// describe needs its own pool: the counts are a sum over the slots that label the pool, the envtest
// cluster is shared by the specs, and ginkgo randomizes their order.
var poolSeq int

func newPool(prefix string) string {
	GinkgoHelper()

	poolSeq++

	name := fmt.Sprintf("%s-%d", prefix, poolSeq)
	createIgnoreExists(poolGroup(name))

	return name
}

// poolGroup is a Group with the spec the CRD requires: the scale-down timings are mandatory
// fields, and the pool's shape is read from the slots that label it.
func poolGroup(name string) *infrav2alpha1.Group {
	return &infrav2alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: NamespaceConfig.Get()},
		Spec: infrav2alpha1.GroupSpec{
			ScaleDownUtilizationThreshold:    "0.5",
			ScaleDownGpuUtilizationThreshold: "0.5",
			ScaleDownUnneededTime:            &metav1.Duration{Duration: 10 * time.Minute},
			ScaleDownUnreadyTime:             &metav1.Duration{Duration: 20 * time.Minute},
			MaxNodeProvisionTime:             &metav1.Duration{Duration: 15 * time.Minute},
		},
	}
}

// scaleDigest is the digest of a request a slot remembers (status.lastClaim): the record the
// pool's last scale is materialized from, and the one that outlives the claim's TTL collection. It
// is a scale-up's digest - the episode's request time is the instant the pool grew.
func scaleDigest(group, call string, age time.Duration) *infrav2alpha1.LastClaimSummary {
	at := metav1.NewTime(time.Now().Add(-age))

	return &infrav2alpha1.LastClaimSummary{
		Name:        fmt.Sprintf("claim-%s", call),
		Call:        call,
		Group:       group,
		Phase:       infrav2alpha1.NodeClaimCompleted,
		RequestedAt: &at,
	}
}

// scaleDownDigest is the digest of a scale-down, and the shape its two instants have in the real
// system: the request time of the episode it ends (a machine booted three days ago, the claim
// DeleteNodes / DecreaseTargetSize was given) and the scale-down's own instant (24 minutes ago, the
// deletionTimestamp the request landed on the claim).
//
// This is the digest the pool's `LASTSCALE` is dated by, and the pair is the point of the fixture:
// the two fields differ, the older one is the one a naive read reaches for (it is the field named
// "when the request was made"), and dating a shrink by the boot it reversed is the bug the second
// field exists to close.
func scaleDownDigest(group, call, reason string, episodeAge, scaleAge time.Duration) *infrav2alpha1.LastClaimSummary {
	digest := scaleDigest(group, call, episodeAge)
	scaled := metav1.NewTime(time.Now().Add(-scaleAge))

	digest.Phase = infrav2alpha1.NodeClaimCompleted
	digest.Reason = reason
	digest.ScaledDownAt = &scaled

	return digest
}

// slotReport is the report a slot's controllers would have written, spelled out: a spec that cares
// about one field of it does not have to fake the rest, and the fields that render as columns are
// the ones a spec that asserts a cell has to name.
type slotReport struct {
	phase infrav2alpha1.NodePhase

	// digest is the slot's status.lastClaim, nil for a slot that never had a claim.
	digest *infrav2alpha1.LastClaimSummary

	// activations / last24H are the two counters, `since` the instant the phase last changed, and
	// transitions the rows of the ring the counters are read from.
	activations int32
	last24H     int32
	transitions int
	since       time.Duration

	// bare stamps nothing the spec did not name: a slot that has never reported, with no counters,
	// no ring and no transition time, which is the shape of a pool whose slot reconciler has not run
	// on it - and the only shape `HEALTH` reads as offline.
	bare bool
}

// defaults fills the report with the shape every spec that does not care about a field expects: a
// slot with a history, a phase that changed 90 minutes ago, and three boots inside the day.
func (r *slotReport) defaults() {
	if r.bare {
		return
	}

	if r.activations == 0 {
		r.activations = 7
	}

	if r.last24H == 0 && r.transitions == 0 {
		r.last24H = 3
	}

	if r.transitions == 0 {
		r.transitions = int(r.last24H)
	}

	if r.since == 0 {
		r.since = -90 * time.Minute
	}
}

// reportSlot creates a slot of a pool and stamps the default report on it.
func reportSlot(name, group string, slotPhase infrav2alpha1.NodePhase, digest *infrav2alpha1.LastClaimSummary) *infrav2alpha1.Node {
	GinkgoHelper()

	return reportSlotShape(name, group, slotReport{phase: slotPhase, digest: digest})
}

// reportSlotShape creates a slot of a pool and stamps the report the controllers would have written
// on it: the phase, the power that goes with it, the digest of the last request, and the
// observability fields the ring work adds - the counters, the ring, the activation ledger, the
// instant the phase last changed. The API server is what enforces the bounds of these fields, so the
// fixtures that exercise a bound (a clipped scale sentence, a capped ledger) are stamped here.
func reportSlotShape(name, group string, report slotReport) *infrav2alpha1.Node {
	GinkgoHelper()

	report.defaults()
	slotPhase := report.phase
	digest := report.digest

	slot := &infrav2alpha1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: NamespaceConfig.Get(),
			Labels:    map[string]string{"group": group},
		},
		Spec: infrav2alpha1.NodeSpec{
			KubernetesNodeName: name,
			Pricing:            infrav2alpha1.PricingSpec{HourlyRate: "0.05", PodRate: "0.001"},
			StartupPodSpec:     infrav2alpha1.MinimalPodSpec{Image: "envtest/startup:latest"},
			ShutdownPodSpec:    infrav2alpha1.MinimalPodSpec{Image: "envtest/shutdown:latest"},
			NodeLabels:         map[string]string{"topology.kubernetes.io/zone": "fsn1"},
		},
	}
	createIgnoreExists(slot)

	fresh := &infrav2alpha1.Node{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(slot), fresh)).To(Succeed())

	fresh.Status.Phase = slotPhase
	fresh.Status.PowerState = infrav2alpha1.PowerStateOff
	fresh.Status.LastClaim = digest

	if phase.IsUp(slotPhase) {
		fresh.Status.PowerState = infrav2alpha1.PowerStateOn
	}

	at := metav1.NewTime(time.Now().Add(report.since))

	fresh.Status.ActivationsTotal = report.activations
	fresh.Status.ActivationsLast24H = report.last24H
	fresh.Status.RecentTransitions = ringOf(report.transitions, slotPhase, at)
	fresh.Status.RecentActivations = ledgerOf(report.last24H, at)

	if !report.bare {
		fresh.Status.PreviousPhase = infrav2alpha1.NodePhaseBooting
		fresh.Status.LastTransitionTime = &at
	}

	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())

	return fresh
}

// ringOf / ledgerOf stamp the two histories the counters are read from, at the one instant the phase
// last changed: a fixture whose count is 3 is a fixture whose ring and ledger say 3, so a spec that
// reads the count reads something the object supports.
func ringOf(rows int, to infrav2alpha1.NodePhase, at metav1.Time) []infrav2alpha1.NodeTransition {
	ring := make([]infrav2alpha1.NodeTransition, 0, rows)

	for range rows {
		ring = append(ring, infrav2alpha1.NodeTransition{
			At:     &at,
			From:   infrav2alpha1.NodePhaseBooting,
			To:     to,
			Reason: "claim c1 ready, observed on, node usable",
		})
	}

	return ring
}

func ledgerOf(boots int32, at metav1.Time) []metav1.Time {
	ledger := make([]metav1.Time, 0, boots)

	for range int(boots) {
		ledger = append(ledger, at)
	}

	return ledger
}

// poolSlots lists the slots of a pool: the same list the group controller counts.
func poolSlots(group string) []infrav2alpha1.Node {
	GinkgoHelper()

	slots := &infrav2alpha1.NodeList{}
	Expect(k8sClient.List(ctx,
		slots,
		client.InNamespace(NamespaceConfig.Get()),
		client.MatchingLabels{"group": group})).To(Succeed())

	return slots.Items
}

// The pool at a glance, asserted against a real API server: the group controller's counts, and the
// server-printed table `kubectl get groups` renders.
var _ = Describe("Group pool shape", func() {
	var reconciler *GroupReconciler

	reconcilePool := func(name string) *infrav2alpha1.Group {
		GinkgoHelper()

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: NamespaceConfig.Get()}})
		Expect(err).NotTo(HaveOccurred())

		group := &infrav2alpha1.Group{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: NamespaceConfig.Get()}, group)).To(Succeed())

		return group
	}

	BeforeEach(func() {
		reconciler = &GroupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	It("counts the pool from its slots' phases, and materializes the last scale off their digests", func() {
		pool := newPool("envtest-shape")

		// Two machines the pool is paying for (ready, booting), three that are not (off, pending,
		// off), and four recorded scales. The newest is the DeleteNodes on worker-3, an hour ago:
		// worker-5's shrink is two hours old, so it is a `down` the verb rendering covers and not the
		// scale the pool last made - the spec that pins the newest scale wins has its own fixture.
		reportSlot(pool+"-worker-1", pool, infrav2alpha1.NodePhaseReady, scaleDigest(pool, infrav2alpha1.ClaimCallIncreaseSize, 5*time.Hour))
		reportSlot(pool+"-worker-2", pool, infrav2alpha1.NodePhaseBooting, scaleDigest(pool, infrav2alpha1.ClaimCallIncreaseSize, 4*time.Hour))
		reportSlot(pool+"-worker-3", pool, infrav2alpha1.NodePhaseOff, scaleDigest(pool, infrav2alpha1.ClaimCallDeleteNodes, 1*time.Hour))
		reportSlot(pool+"-worker-4", pool, infrav2alpha1.NodePhasePending, nil)
		reportSlot(pool+"-worker-5", pool, infrav2alpha1.NodePhaseOff, scaleDownDigest(pool, infrav2alpha1.ClaimCallDecreaseTargetSize, infrav2alpha1.ClaimReasonCancelledByDecreaseTargetSize, 6*time.Hour, 2*time.Hour))

		group := reconcilePool(pool)

		Expect(group.Status.Slots).To(BeNumerically("==", 5), "five committed slots")
		Expect(group.Status.On).To(BeNumerically("==", 2), "booting and ready are the machines being paid for")
		Expect(group.Status.Off).To(BeNumerically("==", 3), "off and pending are the machines that are not")
		Expect(group.Status.Slots).To(BeNumerically("==", group.Status.On+group.Status.Off), "the pool is always fully partitioned")

		Expect(group.Status.LastScaleAt).NotTo(BeNil())
		Expect(group.Status.LastScaleAction).To(Equal("ca:DeleteNodes " + pool + "-worker-3"))
		Expect(group.Status.LastScaleAt.Time).To(BeTemporally("~", time.Now().Add(-1*time.Hour), time.Minute))

		// The Loaded condition this controller wrote before this work is still written.
		Expect(group.Status.Conditions).NotTo(BeEmpty())
	})

	It("renders a cancelled pending request as a shrink, and the newest scale wins", func() {
		pool := newPool("envtest-down")

		// The `down` verb has one writer in the model: NodeGroupDecreaseTargetSize records its call on
		// the claim it is about to delete, the claim controller's finalizer ends the episode with
		// ClaimReasonCancelledByDecreaseTargetSize, and the digest the slot keeps is built from the
		// same write. A cancel that recorded nothing leaves the IncreaseSize attribution of the
		// pending claim as the story, and the pool reports its own shrink as `up`.
		reportSlot(pool+"-up", pool, infrav2alpha1.NodePhaseOff, scaleDigest(pool, infrav2alpha1.ClaimCallIncreaseSize, 30*time.Minute))
		cancelled := reportSlotShape(pool+"-down", pool, slotReport{
			phase: infrav2alpha1.NodePhaseOff,
			digest: scaleDownDigest(pool, infrav2alpha1.ClaimCallDecreaseTargetSize,
				infrav2alpha1.ClaimReasonCancelledByDecreaseTargetSize, 6*time.Hour, 5*time.Minute),
		})

		group := reconcilePool(pool)

		Expect(group.Status.LastScaleAction).To(Equal("down "+pool+"-down"), "the newest scale is the shrink, and it is named as one")
		Expect(group.Status.LastScaleAt.Time).To(BeTemporally("~", time.Now().Add(-5*time.Minute), time.Minute))

		// The digest that produced it is the record of the episode, and carries the reason the
		// finalizer wrote: the slot can answer "who cancelled this, and why" after the claim is gone.
		stamped := &infrav2alpha1.Node{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cancelled), stamped)).To(Succeed())
		Expect(stamped.Status.LastClaim.Reason).To(Equal(infrav2alpha1.ClaimReasonCancelledByDecreaseTargetSize))
	})

	It("dates a scale-down by the scale-down, not by the episode it ends", func() {
		pool := newPool("envtest-scaledown")

		// The shape a DeleteNodes leaves behind, and the case the column exists for: the claim of a
		// machine booted three days ago is marked 24 minutes ago, and its digest keeps both instants.
		// Dating the shrink by the request time of the episode - the field literally named
		// "when the request was made" - reports LASTSCALE = 3d for a pool that shrank 24 minutes ago.
		fresh := reportSlotShape(pool+"-worker-1", pool, slotReport{
			phase: infrav2alpha1.NodePhaseOff,
			digest: scaleDownDigest(pool, infrav2alpha1.ClaimCallDeleteNodes,
				infrav2alpha1.ClaimReasonNodeDeleted, 3*24*time.Hour, 24*time.Minute),
		})

		digest := fresh.Status.LastClaim
		Expect(digest).NotTo(BeNil())
		Expect(digest.RequestedAt).NotTo(BeNil(), "the episode's request time is three days old")
		Expect(digest.ScaledDownAt).NotTo(BeNil(), "the scale-down's own instant is 24 minutes old")
		Expect(digest.ScaledDownAt.Time).To(BeTemporally(">", digest.RequestedAt.Time), "and the scale-down is the newer of the two")

		group := reconcilePool(pool)

		Expect(group.Status.LastScaleAction).To(Equal("ca:DeleteNodes " + pool + "-worker-1"))
		Expect(group.Status.LastScaleAt.Time).To(BeTemporally("~", time.Now().Add(-24*time.Minute), time.Minute),
			"LASTSCALE is the instant the pool shrank, not the age of the machine it took back")
	})

	It("is healthy while something in the pool reports, and offline when nothing does", func() {
		asleep := newPool("envtest-healthy")

		// A pool whose every machine is off is a pool that is doing its job, not an outage: the column
		// answers "is the control loop alive for this pool", and an `off` report is that loop running.
		reportSlot(asleep+"-worker-1", asleep, infrav2alpha1.NodePhaseOff, nil)
		reportSlot(asleep+"-worker-2", asleep, infrav2alpha1.NodePhaseReady, nil)

		group := reconcilePool(asleep)

		Expect(group.Status.On).To(BeNumerically("==", 1))
		Expect(group.Status.Health).To(Equal(infrav2alpha1.GroupHealthHealthy), "every machine off is a pool asleep, not a pool dead")

		quiet := newPool("envtest-offline")

		reportSlotShape(quiet+"-worker-1", quiet, slotReport{bare: true})

		offline := reconcilePool(quiet)

		Expect(offline.Status.Slots).To(BeNumerically("==", 1))
		Expect(offline.Status.On).To(BeNumerically("==", 0))
		Expect(offline.Status.Health).To(Equal(infrav2alpha1.GroupHealthOffline), "a slot that has never reported is a slot nobody is watching")
	})

	It("clips a last-scale sentence the field cannot hold", func() {
		pool := newPool("envtest-clip")

		// The sentence is `verb <slot name>` and the field is bounded at 256: a verb of 14 characters
		// plus a Kubernetes name of 253 is 268, so a writer that composes without clipping produces a
		// status the API server refuses - on every pass, forever, for a pool whose operator cannot fix
		// it without deleting the object. The slot below is a legal name at its DNS bound.
		long := "worker-" + strings.Repeat("x", 246)
		Expect(long).To(HaveLen(253), "a legal Kubernetes object name, at the DNS bound")

		reportSlotShape(long, pool, slotReport{
			phase:  infrav2alpha1.NodePhaseOff,
			digest: scaleDownDigest(pool, infrav2alpha1.ClaimCallDeleteNodes, infrav2alpha1.ClaimReasonNodeDeleted, 24*time.Hour, 5*time.Minute),
		})

		group := reconcilePool(pool)

		Expect(group.Status.LastScaleAt).NotTo(BeNil(), "the pass landed: a clipped sentence is a written status")
		Expect(group.Status.LastScaleAction).To(HaveLen(phase.ScaleActionMaxLength))
		Expect(group.Status.LastScaleAction).To(HavePrefix("ca:DeleteNodes worker-xxx"), "the verb and the head of the name survive")
		Expect(group.Status.LastScaleAction).To(HaveSuffix("..."), "and the cut is visible")
	})

	It("writes nothing on a pass that computes what the pool already says", func() {
		pool := newPool("envtest-still")

		reportSlot(pool+"-worker-1", pool, infrav2alpha1.NodePhaseReady, scaleDigest(pool, infrav2alpha1.ClaimCallIncreaseSize, time.Hour))

		first := reconcilePool(pool)
		second := reconcilePool(pool)

		// The pool is woken by its own slots (Owns(&Node{})), so a pass that rewrites what it just
		// read is a controller generating the event it is subscribed to, and a `kubectl get groups -w`
		// that never settles. The comparison is the whole fix: compute, compare, write on a difference.
		Expect(second.ResourceVersion).To(Equal(first.ResourceVersion), "no difference, no write")

		// And the `Loaded` condition's timestamp dates the last change of its status, not the last
		// pass: stamping now every pass turns a condition's timestamp into a reconcile counter.
		Expect(second.Status.Conditions).To(HaveLen(1))
		Expect(second.Status.Conditions[0].LastTransitionTime).To(Equal(first.Status.Conditions[0].LastTransitionTime))
	})

	It("follows a slot that moves, with no second write to the group", func() {
		pool := newPool("envtest-mover")

		moved := reportSlot(pool+"-mover", pool, infrav2alpha1.NodePhaseReady, scaleDigest(pool, infrav2alpha1.ClaimCallIncreaseSize, time.Minute))

		before := reconcilePool(pool)

		up := 0

		for _, slot := range poolSlots(pool) {
			if phase.IsUp(slot.Status.Phase) {
				up++
			}
		}

		Expect(before.Status.On).To(BeNumerically("==", int32(up)), "the count is the sum of the slots' reports")

		fresh := &infrav2alpha1.Node{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(moved), fresh)).To(Succeed())

		fresh.Status.Phase = infrav2alpha1.NodePhaseOff
		fresh.Status.PowerState = infrav2alpha1.PowerStateOff
		Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())

		after := reconcilePool(pool)

		Expect(after.Status.On).To(BeNumerically("==", before.Status.On-1), "the pool's On follows the slot's report")
		Expect(after.Status.Off).To(BeNumerically("==", before.Status.Off+1))
		Expect(after.Status.Slots).To(BeNumerically("==", before.Status.Slots), "and the pool did not grow")
	})

	It("says the pool never scaled when no slot has a recorded request", func() {
		quiet := newPool("envtest-quiet")

		reportSlot(quiet+"-worker-1", quiet, infrav2alpha1.NodePhaseOff, nil)

		group := reconcilePool(quiet)

		Expect(group.Status.Slots).To(BeNumerically("==", 1))
		Expect(group.Status.LastScaleAt).To(BeNil(), `"never scaled" is not "scaled long ago"`)
		Expect(group.Status.LastScaleAction).To(BeEmpty())
	})

	It("serves the table kubectl get groups renders, with the new columns in order", func() {
		pool := newPool("envtest-table")

		reportSlot(pool+"-worker-1", pool, infrav2alpha1.NodePhaseReady, scaleDigest(pool, infrav2alpha1.ClaimCallIncreaseSize, 2*time.Hour))
		reportSlot(pool+"-worker-2", pool, infrav2alpha1.NodePhaseBooting, scaleDigest(pool, infrav2alpha1.ClaimCallIncreaseSize, time.Hour))
		reportSlot(pool+"-worker-3", pool, infrav2alpha1.NodePhaseOff, nil)

		reconcilePool(pool)

		table := fetchTable(schema.GroupVersion{Group: "infra.homecluster.dev", Version: "v2alpha1"}, "groups", pool)

		// The names are exactly the ones the CRD declares: kubectl renders them upper-case and adds
		// AGE from the metadata, and a rename here is a rename of a column operators read.
		Expect(columnNames(table)).To(Equal([]string{"Name", "Slots", "On", "Off", "LastScale", "Health"}))

		row := rowNamed(table, pool)
		Expect(cellString(row, 1)).To(Equal("3"), "three committed slots")
		Expect(cellString(row, 2)).To(Equal("2"), "two of them up")
		Expect(cellString(row, 3)).To(Equal("1"), "one of them not")
		// The pool's two newest columns, as cells and not as headers: LASTSCALE is the age of the
		// scale the digest dated, and HEALTH is the column this controller derives on the same pass.
		Expect(cellString(row, 4)).To(MatchRegexp(`^[0-9]+[mhd]$`), "the age of the pool's last scale, printed by the server")
		Expect(cellString(row, 5)).To(Equal(infrav2alpha1.GroupHealthHealthy), "the pool reports itself healthy because its slots report")
	})
})

// The slots' own columns: `kubectl get nodes.infra.homecluster.dev` answering the shape of a slot
// with no second command. The Zone column is the one worth asserting against a real API server: its
// JSONPath selects a key with dots in it, and a typo in the escaping renders an empty column and
// nothing else - a green build, and a silent column.
var _ = Describe("Node slot columns", func() {
	const zonePool = "envtest-columns-pool"

	BeforeEach(func() {
		createIgnoreExists(poolGroup(zonePool))
	})

	It("renders Group, Zone, Since and On24h from the object the API server has", func() {
		name := zonePool + "-worker-1"

		reportSlot(name, zonePool, infrav2alpha1.NodePhaseReady, nil)

		table := fetchTable(schema.GroupVersion{Group: "infra.homecluster.dev", Version: "v2alpha1"}, "nodes", name)

		Expect(columnNames(table)).To(Equal([]string{"Name", "Powerstate", "Phase", "Group", "Zone", "Since", "On24h"}))

		row := rowNamed(table, name)
		Expect(cellString(row, 1)).To(Equal("on"))
		Expect(cellString(row, 2)).To(Equal("ready"))
		Expect(cellString(row, 3)).To(Equal(zonePool), "the pool, off the label")
		Expect(cellString(row, 4)).To(Equal("fsn1"), "the zone, off spec.nodeLabels, through the escaped JSONPath")
		Expect(cellString(row, 6)).To(Equal("3"), "the 24h count, materialized")

		// `Since` is a date column, and the server prints a date column as the human age of the
		// instant it selects - the rendering kubectl gives AGE - so the cell names the age of the
		// change ("90m" for a phase that changed 90 minutes ago) and the object keeps the instant the
		// runbook's duration command subtracts. Asserting the cell and not the header is the point: a
		// JSONPath that selects a field the schema prunes is a green build and an empty column.
		Expect(cellString(row, 5)).To(MatchRegexp(`^[0-9]+m$`), "the age of the instant status.phase last changed")
	})

	It("renders a zero as a zero: a slot that has never booted reports 0, not <none>", func() {
		name := zonePool + "-worker-zero"

		// A slot with no report history at all: no phase change, no counters, no ledger. The two
		// counters carry +kubebuilder:default=0 so that a slot written before this release renders a
		// number, and `never booted` and `nothing recorded` are the same readable zero - which is the
		// sentence a churn column has to answer for a machine that sleeps.
		reportSlotShape(name, zonePool, slotReport{bare: true, phase: infrav2alpha1.NodePhaseOff})

		table := fetchTable(schema.GroupVersion{Group: "infra.homecluster.dev", Version: "v2alpha1"}, "nodes", name)
		row := rowNamed(table, name)

		Expect(cellString(row, 2)).To(Equal("off"))
		Expect(cellString(row, 5)).To(BeEmpty(), "no phase has ever changed, so there is no instant to print")
		Expect(cellString(row, 6)).To(Equal("0"), "a zero count renders 0, not <none>")
	})
})

// fetchTable asks the API server for the server-printed table of a resource: the exact response
// `kubectl get` renders, with include=columns turning the additionalPrinterColumns on.
func fetchTable(gv schema.GroupVersion, resource, name string) *metav1.Table {
	GinkgoHelper()

	out := rest.CopyConfig(cfg)
	out.GroupVersion = &gv
	out.APIPath = "/apis"
	out.NegotiatedSerializer = serializer.NewCodecFactory(scheme.Scheme).WithoutConversion()

	restClient, err := rest.RESTClientFor(out)
	Expect(err).NotTo(HaveOccurred())

	table := &metav1.Table{}

	req := restClient.Get().
		Namespace(NamespaceConfig.Get()).
		Resource(resource).
		Name(name).
		Param("include", "columns").
		SetHeader("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")

	Expect(req.Do(ctx).Into(table)).To(Succeed())

	return table
}

func columnNames(table *metav1.Table) []string {
	names := make([]string, 0, len(table.ColumnDefinitions))

	for _, col := range table.ColumnDefinitions {
		names = append(names, col.Name)
	}

	return names
}

func rowNamed(table *metav1.Table, name string) metav1.TableRow {
	GinkgoHelper()

	for _, row := range table.Rows {
		if len(row.Cells) > 0 && cellString(row, 0) == name {
			return row
		}
	}

	Fail("the table has no row named " + name)

	return metav1.TableRow{}
}

// cellString renders a table cell the way the server printed it: a cell is a decoded JSON value, so
// a string cell is a string, a number cell is a number, and an unset field is nil - which is what
// an empty column of `kubectl get` is.
func cellString(row metav1.TableRow, i int) string {
	GinkgoHelper()

	if i >= len(row.Cells) {
		Fail(fmt.Sprintf("row has %d cells, wanted index %d", len(row.Cells), i))
	}

	cell := row.Cells[i]
	if cell == nil {
		return ""
	}

	if s, ok := cell.(string); ok {
		return s
	}

	return fmt.Sprintf("%v", cell)
}
