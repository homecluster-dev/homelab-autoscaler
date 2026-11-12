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
// pool's last scale is materialized from, and the one that outlives the claim's TTL collection.
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

// reportSlot creates a slot of a pool and stamps the report the controllers would have written on
// it: the phase, the power that goes with it, the digest of the last request, and the observability
// fields the ring work adds.
func reportSlot(name, group string, slotPhase infrav2alpha1.NodePhase, digest *infrav2alpha1.LastClaimSummary) *infrav2alpha1.Node {
	GinkgoHelper()

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

	at := metav1.NewTime(time.Now().Add(-90 * time.Minute))
	fresh.Status.PreviousPhase = infrav2alpha1.NodePhaseBooting
	fresh.Status.LastTransitionTime = &at
	fresh.Status.ActivationsTotal = 7
	fresh.Status.ActivationsLast24H = 3
	fresh.Status.RecentTransitions = []infrav2alpha1.NodeTransition{
		{At: &at, From: infrav2alpha1.NodePhaseBooting, To: slotPhase, Reason: "claim c1 ready, observed on, node usable"},
	}

	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())

	return fresh
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

		// Two machines the pool is paying for (ready, booting), two that are not (off, pending),
		// and three recorded scales: the newest of them is the DeleteNodes on worker-3, an hour ago.
		reportSlot(pool+"-worker-1", pool, infrav2alpha1.NodePhaseReady, scaleDigest(pool, infrav2alpha1.ClaimCallIncreaseSize, 5*time.Hour))
		reportSlot(pool+"-worker-2", pool, infrav2alpha1.NodePhaseBooting, scaleDigest(pool, infrav2alpha1.ClaimCallIncreaseSize, 4*time.Hour))
		reportSlot(pool+"-worker-3", pool, infrav2alpha1.NodePhaseOff, scaleDigest(pool, infrav2alpha1.ClaimCallDeleteNodes, 1*time.Hour))
		reportSlot(pool+"-worker-4", pool, infrav2alpha1.NodePhasePending, nil)

		group := reconcilePool(pool)

		Expect(group.Status.Slots).To(BeNumerically("==", 4), "four committed slots")
		Expect(group.Status.On).To(BeNumerically("==", 2), "booting and ready are the machines being paid for")
		Expect(group.Status.Off).To(BeNumerically("==", 2), "off and pending are the machines that are not")
		Expect(group.Status.Slots).To(BeNumerically("==", group.Status.On+group.Status.Off), "the pool is always fully partitioned")

		Expect(group.Status.LastScaleAt).NotTo(BeNil())
		Expect(group.Status.LastScaleAction).To(Equal("ca:DeleteNodes " + pool + "-worker-3"))
		Expect(group.Status.LastScaleAt.Time).To(BeTemporally("~", time.Now().Add(-1*time.Hour), time.Minute))

		// The Loaded condition this controller wrote before this work is still written.
		Expect(group.Status.Conditions).NotTo(BeEmpty())
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
