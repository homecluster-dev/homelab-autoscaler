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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	autoscalerpkg "github.com/homecluster-dev/homelab-autoscaler/internal/autoscaler"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	corecontroller "github.com/homecluster-dev/homelab-autoscaler/internal/controller/core"
	"github.com/homecluster-dev/homelab-autoscaler/internal/fsm"
)

// The envtest coverage of the claim model, against a real API server: the CRDs' defaults and enums
// are in force, the status subresources are real, and a claim is an object the reconcilers read
// back. Every spec names the shape of a slot (the claim it carries, the power its machine is
// observed at, the node object it presents) and asserts what the operator ends up with: the
// claim's phase, the slot's derived report, and the lifecycle Job the pass bought.
//
// The specs share one API server, so each names its own slot and its own Kubernetes node.
var _ = Describe("NodeClaim lifecycle", func() {
	const group = "envtest-claims"

	var slotSeq int

	// slotName mints a unique slot name per spec, so the specs do not collide on a shared server.
	slotName := func(kind string) string {
		slotSeq++

		return fmt.Sprintf("%s-%d", kind, slotSeq)
	}

	// ensureGroup creates the Group the slots label. The Node reconciler refuses a slot whose
	// Group it cannot read, so every spec needs it, and it carries the readiness taint key the
	// pool's CNI stamps: the gate for phase ready is the node being free of it.
	ensureGroup := func() *infrav2alpha1.Group {
		groupCR := &infrav2alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: group, Namespace: NamespaceConfig.Get()},
			Spec: infrav2alpha1.GroupSpec{
				MinSize:                          0,
				ScaleDownUtilizationThreshold:    "0.5",
				ScaleDownGpuUtilizationThreshold: "0.5",
				ScaleDownUnneededTime:            &metav1.Duration{Duration: 10 * time.Minute},
				ScaleDownUnreadyTime:             &metav1.Duration{Duration: 20 * time.Minute},
				MaxNodeProvisionTime:             &metav1.Duration{Duration: 15 * time.Minute},
			},
		}

		createIgnoreExists(groupCR)

		return groupCR
	}

	// newSlot registers a slot: the Group, the label, and the observation/report the spec starts
	// from. spec.powerState does not exist in v2alpha1, so there is no intent to declare here.
	newSlot := func(name string, observed infrav2alpha1.PowerState, report infrav2alpha1.NodePhase) *infrav2alpha1.Node {
		ensureGroup()

		slot := &infrav2alpha1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: NamespaceConfig.Get(),
				Labels:    map[string]string{"group": group},
			},
			Spec: infrav2alpha1.NodeSpec{
				KubernetesNodeName: name + "-k8s",
				Pricing:            infrav2alpha1.PricingSpec{HourlyRate: "0.05", PodRate: "0.001"},
				StartupPodSpec:     infrav2alpha1.MinimalPodSpec{Image: "envtest/startup:latest", Command: []string{"/bin/sh"}, Args: []string{"-c", "true"}},
				ShutdownPodSpec:    infrav2alpha1.MinimalPodSpec{Image: "envtest/shutdown:latest", Command: []string{"/bin/sh"}, Args: []string{"-c", "true"}},
			},
		}

		Expect(k8sClient.Create(ctx, slot)).To(Succeed())
		setSlotStatus(slot, observed, report)

		return slot
	}

	// newClaim registers a live claim for a slot through the API server, and lands its phase on
	// the status subresource. The defaults of the CRD (phase pending, ttlSecondsAfterFinished
	// 3600) are the API server's, not a fake client's, so a spec that omits a phase gets the one
	// the schema declares.
	newClaim := func(slot *infrav2alpha1.Node, claimPhase infrav2alpha1.NodeClaimPhase, mutate func(*infrav2alpha1.NodeClaim)) *infrav2alpha1.NodeClaim {
		nodeClaim := &infrav2alpha1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      slot.Name + "-claim",
				Namespace: slot.Namespace,
				Labels: map[string]string{
					claimLabelSlot:  slot.Name,
					claimLabelGroup: group,
				},
			},
			Spec: infrav2alpha1.NodeClaimSpec{
				SlotRef:     corev1ObjectRef(slot),
				RequestedBy: "cluster-autoscaler",
				Request: infrav2alpha1.ClaimRequest{
					Call:  infrav2alpha1.ClaimCallIncreaseSize,
					Delta: 1,
					Group: group,
				},
			},
		}

		if mutate != nil {
			mutate(nodeClaim)
		}

		Expect(k8sClient.Create(ctx, nodeClaim)).To(Succeed())

		if claimPhase != "" {
			setClaimPhase(nodeClaim, claimPhase, "")
		}

		return nodeClaim
	}

	// k8sNode registers the Kubernetes node of a slot: the machine, as the kubelet presents it.
	// The label is what the observation path matches on, so a spec that asserts the derived
	// report needs it: the machine is found by the label, not by a name convention.
	k8sNode := func(slotName string, ready bool, unschedulable bool, taints []corev1.Taint) *corev1.Node {
		name := slotName + "-k8s"

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{"infra.homecluster.dev/node": slotName},
			},
			Spec: corev1.NodeSpec{Unschedulable: unschedulable, Taints: taints},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				}},
			},
		}

		if !ready {
			node.Status.Conditions[0].Status = corev1.ConditionFalse
		}

		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		return node
	}

	// "a slot with no live claim and the machine off is idle"
	It("leaves a free slot with a powered off machine alone", func() {
		name := slotName("idle")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)

		result := reconcileSlot(slot)
		Expect(result.RequeueAfter).To(Equal(idleResync), "the idle resync is what notices a machine powered on out of band")

		Expect(jobsOfSlot(name)).To(BeEmpty(), "a slot no claim names buys nothing")

		live, err := (&NodeReconciler{Client: k8sClient, Scheme: scheme.Scheme}).liveClaimOf(ctx, slot)
		Expect(err).NotTo(HaveOccurred())
		Expect(live).To(BeNil(), "no claim, no live claim")

		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseOff),
			"no live claim + no machine is reported off, the idle shape of a homelab pool")
	})

	// "a live pending claim boots the machine"
	It("boots the machine of a live pending claim", func() {
		name := slotName("boot")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimPending, nil)

		reconcileSlot(slot)

		jobs := jobsOfSlot(name)
		Expect(jobs).To(HaveLen(1), "law 1: the claim wants a machine, so the startup Job runs")
		Expect(jobs[0].Labels["type"]).To(Equal(fsm.PhaseStartup))

		Expect(readClaimObject(nodeClaim.Name).Status.Phase).To(Equal(infrav2alpha1.NodeClaimBooting),
			"the pending -> booting write is the one that created the startup Job: an attempt is a machine bought")
		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseBooting),
			"the slot reports the boot the claim asked for")
	})

	// "booting + observed on + node usable -> claim ready, and the derived Node.status.phase: ready"
	It("lands a booting claim on ready once the machine is up and usable", func() {
		name := slotName("ready")
		slot := newSlot(name, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimBooting, nil)
		k8sNode(name, true, false, nil)
		job := createLifecycleJob(slot, fsm.PhaseStartup)

		// The state the input law puts the machine in, before the gate answers: the claim is
		// booting, the machine is observed on and the node is usable, so the startup is in flight
		// and JobCompleted is the event that is reachable.
		Expect(openReadinessGateState(slot)).To(Equal(fsm.StateStartingUp))

		completeStartupGate(slot, job)

		ready := readClaimObject(nodeClaim.Name)
		Expect(ready.Status.Phase).To(Equal(infrav2alpha1.NodeClaimReady),
			"the readiness gate is the writer of the claim's ready: the machine can take the pool's work")
		Expect(ready.Status.Reason).To(BeEmpty())
		Expect(ready.Status.Conditions).To(ContainElement(HaveField("Type", infrav2alpha1.ClaimConditionReady)))

		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseReady),
			"the slot's report is derived from the claim and the observation, by the same formula")

		// And the pass that follows the gate takes no further action on a ready unit.
		result := reconcileSlot(readSlotObject(name))
		Expect(result.RequeueAfter).To(Equal(idleResync))
		Expect(len(jobsOfSlot(name))).To(Equal(1), "the startup Job in flight is the only lifecycle Job")
	})

	// "spec.deadlineAt in the past on a booting claim -> failed, and the machine is force-shut down"
	It("fails an overdue booting claim and powers its machine off", func() {
		name := slotName("deadline")
		slot := newSlot(name, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimBooting, func(c *infrav2alpha1.NodeClaim) {
			overdue := metav1.NewTime(time.Now().Add(-time.Minute))
			c.Spec.DeadlineAt = &overdue
		})

		k8sNode(name, true, false, nil)
		createLifecycleJob(slot, fsm.PhaseStartup)

		reconcileClaim(nodeClaim)

		failed := readClaimObject(nodeClaim.Name)
		Expect(failed.Status.Phase).To(Equal(infrav2alpha1.NodeClaimFailed), "overdue means failed")
		Expect(failed.Status.Reason).To(Equal(infrav2alpha1.ClaimReasonDeadlineExceeded))
		Expect(failed.Status.FinishedAt).NotTo(BeNil(), "the TTL of a terminal claim is measured from it")

		Expect(readSlotObject(name).Status.LastClaim).NotTo(BeNil(), "the slot keeps the story the claim will not survive")
		Expect(readSlotObject(name).Status.LastClaim.Reason).To(Equal(infrav2alpha1.ClaimReasonDeadlineExceeded))

		// The force shutdown is the point of the deadline: a boot that overran is a machine that
		// may join and start serving capacity the autoscaler has already given up on.
		reconcileSlot(readSlotObject(name))

		shutdownJobs := jobsOfSlot(name, fsm.PhaseShutdown)
		Expect(shutdownJobs).To(HaveLen(1), "a terminal claim is not a want: the machine goes off")

		Expect(readK8sNode(name+"-k8s").Spec.Unschedulable).To(BeTrue(), "the machine is cordoned before it is powered off")
		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseDraining))
	})

	// "status.attempt seeds from Node.status.lastClaim.attempt and increments per new claim"
	It("seeds the attempt of a claim from the slot's digest, and counts each new claim", func() {
		name := slotName("attempt")
		newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		ensureGroup()

		// The slot has failed twice in a row. The digest is the only record of that: the claims
		// that carried it are collected after their TTL.
		stampDigest(name, &infrav2alpha1.LastClaimSummary{
			Name: "claim-of-the-episode-before", Phase: infrav2alpha1.NodeClaimFailed, Attempt: 2,
			Call: infrav2alpha1.ClaimCallIncreaseSize, Group: group, Reason: infrav2alpha1.ClaimReasonStartupJobFailed,
		})

		first := createProviderClaim(readSlotObject(name))
		Expect(first.Status.Attempt).To(BeZero(), "a created claim bakes no number in: the controller seeds it from the slot")

		reconcileClaim(first)
		Expect(readClaimObject(first.Name).Status.Attempt).To(Equal(int32(3)),
			"the third consecutive attempt of this slot, seeded from the digest the slot keeps")

		// The next episode of the same slot is attempt 4, and the bound (3) is what fails it:
		// a bound on attempts is a bound on the consecutive claims of a slot, and it lives on the
		// request, not on the long-lived slot.
		stampDigest(name, claim.Digest(readClaimObject(first.Name)))
		second := createProviderClaim(readSlotObject(name))
		reconcileClaim(second)

		secondAfter := readClaimObject(second.Name)
		Expect(secondAfter.Status.Attempt).To(Equal(int32(4)))
		Expect(secondAfter.Status.Phase).To(Equal(infrav2alpha1.NodeClaimFailed),
			"the slot has used its consecutive attempts, so no machine is bought for this claim")
		Expect(secondAfter.Status.Reason).To(Equal(infrav2alpha1.ClaimReasonAttemptLimitExceeded))

		// An episode that landed restarts the streak: the reset rule reads the object that ends
		// with the episode, which is why the v1alpha1 condition on the slot and its reset sites
		// are gone.
		stampDigest(name, &infrav2alpha1.LastClaimSummary{Name: "a-landed-claim", Phase: infrav2alpha1.NodeClaimReady, Attempt: 4, Call: infrav2alpha1.ClaimCallIncreaseSize})
		third := createProviderClaim(readSlotObject(name))
		reconcileClaim(third)
		Expect(readClaimObject(third.Name).Status.Attempt).To(Equal(int32(1)), "an attempt that bought a usable machine ends the streak")
	})

	// "the derived Node.status.phase never causes an action on its own"
	It("acts on the claim and the observation, never on the phase the slot reports", func() {
		// A report of ready, on a slot with no claim and a machine observed off. Reading the record
		// as an input is what stranded v1alpha1 slots: a shutdown for a machine that is not there,
		// or a boot the model has no request for. Here the record is a report, so the pass is inert.
		name := slotName("lie-off")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseReady)
		// The machine is off, and the kubelet left its node object behind NotReady: that is the
		// observation the report is recomputed from.
		k8sNode(name, false, false, nil)

		reconcileSlot(slot)
		Expect(jobsOfSlot(name)).To(BeEmpty(), "a phase the slot reports is not a request: nothing is bought, nothing is powered off")

		// The report is this pass's output. The observation path recomputes it from
		// (no live claim, observed off) through phase.Derive, and that is the only writer that
		// touches it.
		reconcileObservation(name + "-k8s")
		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseOff),
			"the report follows the inputs; the inputs never follow the report")

		// The same record, contradicted the other way: a slot reporting off, with a live claim that
		// wants a machine. The claim decides, and the boot runs.
		booting := slotName("lie-boot")
		bootSlot := newSlot(booting, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		claimOn := newClaim(bootSlot, infrav2alpha1.NodeClaimPending, nil)

		reconcileSlot(bootSlot)
		Expect(jobsOfSlot(booting, fsm.PhaseStartup)).To(HaveLen(1),
			"the live claim decides the boot; the report of the slot is not consulted")
		Expect(readClaimObject(claimOn.Name).Status.Phase).To(Equal(infrav2alpha1.NodeClaimBooting))

		// A slot reporting draining, with a live ready claim and a usable machine: the report says
		// the machine is going away, the claim says it is wanted, and the claim wins. No shutdown,
		// no cordon, and the report is recomputed to ready.
		serving := slotName("lie-draining")
		servingSlot := newSlot(serving, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
		newClaim(servingSlot, infrav2alpha1.NodeClaimReady, nil)
		k8sNode(serving, true, false, nil)

		result := reconcileSlot(readSlotObject(serving))
		Expect(result.RequeueAfter).To(Equal(idleResync), "law 2: capacity serving pods is left alone")
		Expect(jobsOfSlot(serving, fsm.PhaseShutdown)).To(BeEmpty(), "the phase the slot reports starts no shutdown")
		Expect(readK8sNode(serving + "-k8s").Spec.Unschedulable).To(BeFalse())

		reconcileObservation(serving + "-k8s")
		Expect(readSlotObject(serving).Status.Phase).To(Equal(infrav2alpha1.NodePhaseReady))
	})
})

// ---------------------------------------------------------------------------
// envtest helpers: the shape of a slot, and the passes over it.
// ---------------------------------------------------------------------------

// createIgnoreExists creates an object and tolerates a pre-existing one, so the shared Group of
// the specs can be ensured by every spec.
func createIgnoreExists(obj client.Object) {
	if err := k8sClient.Create(ctx, obj); err != nil {
		Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
	}
}

// corev1ObjectRef is the spec.slotRef of a claim: name is the field the controllers read.
func corev1ObjectRef(slot *infrav2alpha1.Node) corev1.ObjectReference {
	return corev1.ObjectReference{
		APIVersion: infrav2alpha1.GroupVersion.String(),
		Kind:       "Node",
		Namespace:  slot.Namespace,
		Name:       slot.Name,
		UID:        slot.UID,
	}
}

// setSlotStatus writes the observation and the report of a slot through the status subresource,
// the way the observation path does. There is no spec field for an intent in v2alpha1, so this is
// the only way a test puts a slot into a shape.
func setSlotStatus(slot *infrav2alpha1.Node, observed infrav2alpha1.PowerState, report infrav2alpha1.NodePhase) {
	fresh := readSlotObject(slot.Name)
	fresh.Status.PowerState = observed
	fresh.Status.Phase = report
	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}

// setClaimPhase lands a phase on a claim's status subresource. The claim controller, and the
// transitions that move a claim, write through internal/claim.Recorder; a test that seeds a shape
// writes the same field the same way.
func setClaimPhase(nodeClaim *infrav2alpha1.NodeClaim, claimPhase infrav2alpha1.NodeClaimPhase, reason string) {
	fresh := readClaimObject(nodeClaim.Name)
	fresh.Status.Phase = claimPhase
	fresh.Status.Reason = reason
	fresh.Status.Attempt = 1
	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}

// stampDigest records the slot's digest of its last claim, which is what seeds the next attempt.
func stampDigest(slotName string, digest *infrav2alpha1.LastClaimSummary) {
	fresh := readSlotObject(slotName)
	fresh.Status.LastClaim = digest
	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}

// createProviderClaim is the provider's write: internal/claim.New, the constructor the gRPC
// handlers call, run against the env API server, so attribution, deadline, labels and the
// ownerReference are the ones the real request carries.
func createProviderClaim(slot *infrav2alpha1.Node) *infrav2alpha1.NodeClaim {
	groupCR := &infrav2alpha1.Group{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "envtest-claims", Namespace: slot.Namespace}, groupCR)).To(Succeed())

	nodeClaim, err := claim.New(slot, groupCR, infrav2alpha1.ClaimRequest{
		Call:  infrav2alpha1.ClaimCallIncreaseSize,
		Delta: 1,
		Group: groupCR.Name,
	}, claim.RequestedByAutoscaler, scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient.Create(ctx, nodeClaim)).To(Succeed())

	return nodeClaim
}

// createLifecycleJob registers a running lifecycle Job of a slot, with the labels the reconciler
// lists on. It is the fact that separates "a boot is in flight" from "a claim exists".
func createLifecycleJob(slot *infrav2alpha1.Node, jobPhase string) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      slot.Name + "-" + jobPhase + "-envtest",
			Namespace: slot.Namespace,
			Labels: map[string]string{
				fsm.LabelNodeCR: slot.Name,
				fsm.LabelPhase:  jobPhase,
				"type":          jobPhase,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "lifecycle", Image: "envtest/" + jobPhase + ":latest", Command: []string{"/bin/sh", "-c", "true"}}},
				},
			},
		},
	}

	Expect(k8sClient.Create(ctx, job)).To(Succeed())

	return job
}

// reconcileSlot runs one pass of the slot reconciler over a slot.
func reconcileSlot(slot *infrav2alpha1.Node) ctrl.Result {
	GinkgoHelper()

	reconciler := &NodeReconciler{Client: k8sClient, Scheme: scheme.Scheme}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: slot.Name, Namespace: slot.Namespace,
	}})
	Expect(err).NotTo(HaveOccurred())

	return result
}

// reconcileClaim runs one pass of the claim reconciler over a claim: the deadline, the attempt
// bound, the finalizer of a scale-down, the digest, the collection.
func reconcileClaim(nodeClaim *infrav2alpha1.NodeClaim) ctrl.Result {
	GinkgoHelper()

	reconciler := &NodeClaimReconciler{Client: k8sClient, Scheme: scheme.Scheme}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: nodeClaim.Name, Namespace: nodeClaim.Namespace,
	}})
	Expect(err).NotTo(HaveOccurred())

	return result
}

// reconcileObservation runs the observation path over the Kubernetes node of a slot: the writer of
// status.powerState and of the derived status.phase.
func reconcileObservation(k8sNodeName string) {
	GinkgoHelper()

	reconciler := &corecontroller.KubernetesNodeReconciler{Client: k8sClient, Scheme: scheme.Scheme}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: k8sNodeName}})
	Expect(err).NotTo(HaveOccurred())
	Expect(result).To(Equal(ctrl.Result{}))
}

// openReadinessGateState is the state the input law puts a slot in, read off the state machine the
// slot reconciler would build: the claim, the observation, the node, the Jobs in flight.
func openReadinessGateState(slot *infrav2alpha1.Node) string {
	GinkgoHelper()

	reconciler := &NodeReconciler{Client: k8sClient, Scheme: scheme.Scheme}

	live, err := reconciler.liveClaimOf(ctx, slot)
	Expect(err).NotTo(HaveOccurred())
	requireLiveClaim(live)

	machine := startupGateMachine(reconciler, slot, live)

	return machine.GetCurrentState()
}

// completeStartupGate runs the writer of the booting -> ready move: the Job monitor of the machine
// that started it reports the startup Job completed, the readiness gate answers for the node, and
// the gate's answer is what the claim's phase records.
func completeStartupGate(slot *infrav2alpha1.Node, job *batchv1.Job) {
	GinkgoHelper()

	reconciler := &NodeReconciler{Client: k8sClient, Scheme: scheme.Scheme}

	live, err := reconciler.liveClaimOf(ctx, slot)
	Expect(err).NotTo(HaveOccurred())
	requireLiveClaim(live)

	Expect(startupGateMachine(reconciler, slot, live).JobCompleted(job)).To(Succeed())
}

// startupGateMachine builds the state machine exactly as the slot reconciler does, so the gate the
// spec exercises is the production gate: the coordination lock, the readiness taint keys of the
// Group, the input law, and the claim writer.
func startupGateMachine(reconciler *NodeReconciler, slot *infrav2alpha1.Node, live *infrav2alpha1.NodeClaim) *fsm.NodeStateMachine {
	GinkgoHelper()

	fresh := readSlotObject(slot.Name)
	input := reconciler.lifecycleInput(ctx, fresh, live)
	coord := fsm.NewCoordinationManager(k8sClient, fsm.NodeControllerOwner)

	return fsm.NewNodeStateMachine(fresh, k8sClient, scheme.Scheme, coord,
		fsm.WithReadinessTaintKeys(readyTaintKeysOf(fresh)),
		fsm.WithInput(input),
		fsm.WithClaim(live, claimWriterFor(k8sClient)))
}

// readyTaintKeysOf is the gate the slot records for its Group: the keys the observation path reads
// too, so the readiness the claim asserts and the readiness the slot reports are one question.
func readyTaintKeysOf(slot *infrav2alpha1.Node) []string {
	return autoscalerpkg.ReadinessTaintKeysOfSlot(slot)
}

// requireLiveClaim fails the spec when the slot has no live claim to drive.
func requireLiveClaim(live *infrav2alpha1.NodeClaim) {
	GinkgoHelper()
	Expect(live).NotTo(BeNil(), "the spec seeded a live claim for the slot")
}

// readSlotObject reads a slot back from the API server.
func readSlotObject(name string) *infrav2alpha1.Node {
	slot := &infrav2alpha1.Node{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: NamespaceConfig.Get()}, slot)).To(Succeed())

	return slot
}

// readClaimObject reads a claim back from the API server.
func readClaimObject(name string) *infrav2alpha1.NodeClaim {
	nodeClaim := &infrav2alpha1.NodeClaim{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: NamespaceConfig.Get()}, nodeClaim)).To(Succeed())

	return nodeClaim
}

// readK8sNode reads the Kubernetes node of a slot back from the API server.
func readK8sNode(name string) *corev1.Node {
	node := &corev1.Node{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)).To(Succeed())

	return node
}

// jobsOfSlot lists the lifecycle Jobs the controller created for a slot, optionally filtered by
// lifecycle phase. The label the Jobs carry is the same one the reconciler reads, so the assertion
// is about the objects, not about a bookkeeping field.
func jobsOfSlot(slotName string, phases ...string) []batchv1.Job {
	jobs := &batchv1.JobList{}
	Expect(k8sClient.List(ctx, jobs, client.InNamespace(NamespaceConfig.Get()))).To(Succeed())

	matches := make([]batchv1.Job, 0, len(jobs.Items))

	for _, job := range jobs.Items {
		if job.Labels[fsm.LabelNodeCR] != slotName {
			continue
		}

		if len(phases) == 0 {
			matches = append(matches, job)

			continue
		}

		for _, phase := range phases {
			if job.Labels[fsm.LabelPhase] == phase {
				matches = append(matches, job)

				break
			}
		}
	}

	return matches
}
