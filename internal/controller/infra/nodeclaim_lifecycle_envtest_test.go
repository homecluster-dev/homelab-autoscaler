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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	ensureGroup := func() {
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
	// k8sNode registers the machine of a slot as the kubelet presents it. A fixture never cordons:
	// the cordon is a transition's, so the assertion "the shutdown cordoned it" means something.
	k8sNode := func(slotName string, ready bool) {
		name := slotName + "-k8s"

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{"infra.homecluster.dev/node": slotName},
			},
			Spec: corev1.NodeSpec{},
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
		Expect(readClaimObject(nodeClaim.Name).Finalizers).To(ContainElements(ClaimFinalizer),
			"the finalizer lands on the same write as the booting phase (M5): there is no pass boundary in which a DeleteNodes deletes a committed boot with no shutdown behind it")
		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseBooting),
			"the slot reports the boot the claim asked for")
	})

	// "booting + observed on + node usable -> claim ready, and the derived Node.status.phase: ready"
	It("lands a booting claim on ready once the machine is up and usable", func() {
		name := slotName("ready")
		slot := newSlot(name, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimBooting, nil)
		k8sNode(name, true)
		job := createStartupJobFixture(slot, name+"-startup-envtest")

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
		Expect(jobsOfSlot(name)).To(HaveLen(1), "the startup Job in flight is the only lifecycle Job")
	})

	// "a boot that started from an off observation lands its report ready"
	//
	// This is the shape the k3d e2e drives and the shape every real boot is: the pass that fires
	// StartNode reads the machine observed off, and the Job monitor that reports the boot completed is
	// the goroutine of *that* pass, so the input of the machine that answers for the boot is the
	// snapshot of the boot's start - off, with no usable node. The machine came up while the Job ran:
	// the kubelet joined, the observation path recorded the power, and the readiness gate has just read
	// the node object it is answering for. The report the same pass writes is derived from that
	// evidence, not from the snapshot the boot started in.
	//
	// The spec above fixes the observation at `on` before the gate runs, which is the shape a fixture
	// can seed and a boot cannot reach. Here nothing re-reads the slot for the monitor: a settled
	// kubelet writes no further corev1.Node event (its heartbeats go to its Lease), so the observation
	// path - the other writer of the report - never runs again, and a report the gate refuses to write
	// stays `booting` for the life of the machine.
	It("lands a boot that started from an off observation on a ready report", func() {
		name := slotName("boot-from-off")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimPending, nil)

		// Law 1 boots it: the startup Job, the pending -> booting commit, and the report of the pass.
		reconcileSlot(readSlotObject(name))

		Expect(readClaimObject(nodeClaim.Name).Status.Phase).To(Equal(infrav2alpha1.NodeClaimBooting))
		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseBooting))

		job := newestJobOf(jobsOfSlot(name, fsm.PhaseStartup))

		// The state machine of the pass that started the boot, built from the inputs that pass read:
		// the machine observed off, and no node object, the kubelet of a powered-off machine not having
		// joined yet. The Job monitor of the boot is the goroutine of this machine.
		reconciler := &NodeReconciler{Client: k8sClient, Scheme: scheme.Scheme}

		bootingSlot := readSlotObject(name)

		live, err := reconciler.liveClaimOf(ctx, bootingSlot)
		Expect(err).NotTo(HaveOccurred())
		requireLiveClaim(live)

		monitor := startupGateMachine(reconciler, bootingSlot, live)
		Expect(monitor.GetCurrentState()).To(Equal(fsm.StateStartingUp),
			"the boot in flight is the state its completion is reachable from")

		// The machine comes up, and the observation follows it.
		k8sNode(name, true)
		reconcileObservation(name + "-k8s")

		Expect(readSlotObject(name).Status.PowerState).To(Equal(infrav2alpha1.PowerStateOn),
			"the kubelet is beating, so the machine is observed on")
		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseBooting),
			"the claim has not been stamped ready, and an unstamped claim on a machine that is up is booting")

		// The startup Job finishes, and the monitor of the pass that started it reports the completion.
		completeJob(job)

		Expect(monitor.JobCompleted(job)).To(Succeed())

		ready := readClaimObject(nodeClaim.Name)
		Expect(ready.Status.Phase).To(Equal(infrav2alpha1.NodeClaimReady),
			"the readiness gate answered for the claim: the node is Ready, gate-clear and uncordoned")

		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseReady),
			"the gate's own read of the node is an input, so the report of the same evidence is ready: a slot\n"+
				"that reports booting here has no writer left to fix it, the node object is quiet")
	})

	// "spec.deadlineAt in the past on a booting claim -> failed, and the machine is force-shut down"
	It("fails an overdue booting claim and powers its machine off", func() {
		name := slotName("deadline")
		slot := newSlot(name, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimBooting, func(c *infrav2alpha1.NodeClaim) {
			overdue := metav1.NewTime(time.Now().Add(-time.Minute))
			c.Spec.DeadlineAt = &overdue
		})

		k8sNode(name, true)
		createStartupJobFixture(slot, name+"-startup-envtest")

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
		// Each episode gets its own slot: a slot carries one live claim, and the spec is about the
		// number the digest of a slot seeds, not about two requests wanting one machine (that pair
		// has its own spec below).
		name := slotName("attempt")
		newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)

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

		// The next episode of the same slot is attempt 4, and the bound (3) is what fails it: a
		// bound on attempts is a bound on the consecutive claims of a slot, and it lives on the
		// request, not on the long-lived slot. The previous claim has to be out of the way for the
		// slot to be free again - the provider claims a free slot, and one live claim per slot is
		// enforced where the API cannot see it.
		// The digest of the episode is what the next slot's first request reads, and the object that
		// carried it is out of the way: one live claim per slot.
		firstDigest := claim.Digest(readClaimObject(first.Name))
		collectClaim(first.Name)

		secondName := slotName("attempt-next")
		newSlot(secondName, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		stampDigest(secondName, firstDigest)

		second := createProviderClaim(readSlotObject(secondName))
		reconcileClaim(second)

		secondAfter := readClaimObject(second.Name)
		Expect(secondAfter.Status.Attempt).To(Equal(int32(4)))
		Expect(secondAfter.Status.Phase).To(Equal(infrav2alpha1.NodeClaimFailed),
			"the slot has used its consecutive attempts, so no machine is bought for this claim")
		Expect(secondAfter.Status.Reason).To(Equal(infrav2alpha1.ClaimReasonAttemptLimitExceeded))

		// An episode that landed restarts the streak: the reset rule reads the object that ends
		// with the episode, which is why the v1alpha1 condition on the slot and its reset sites
		// are gone.
		thirdName := slotName("attempt-landed")
		newSlot(thirdName, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		stampDigest(thirdName, &infrav2alpha1.LastClaimSummary{Name: "a-landed-claim", Phase: infrav2alpha1.NodeClaimReady, Attempt: 4, Call: infrav2alpha1.ClaimCallIncreaseSize})

		third := createProviderClaim(readSlotObject(thirdName))
		reconcileClaim(third)
		Expect(readClaimObject(third.Name).Status.Attempt).To(Equal(int32(1)), "an attempt that bought a usable machine ends the streak")
	})

	// "a terminal claim is collected after its TTL, and the slot keeps the story"
	It("collects a terminal claim after its TTL, and leaves the story on the slot", func() {
		// §8's collection: a pool that scales twice a day writes claims forever, so the TTL is what
		// keeps `kubectl get nodeclaim` a command an operator wants to run. The slot's digest is the
		// record that survives the delete.
		name := slotName("collect")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimFailed, func(c *infrav2alpha1.NodeClaim) {
			ttl := int32(0)
			c.Spec.TTLSecondsAfterFinished = &ttl
		})

		// The fixture landed the phase on the status subresource, so the claim carries no finishedAt.
		// The first pass stamps the clock the TTL measures from - a terminal phase reached by a
		// writer that stamped no time has to get one, or the object is never collected and the pool
		// accumulates - and the collection runs on the pass after it.
		reconcileClaim(nodeClaim)

		stamped := readClaimObject(nodeClaim.Name)
		Expect(stamped.Status.FinishedAt).NotTo(BeNil(), "a terminal claim with no clock is given one, so its TTL has a start")

		reconcileClaim(stamped)

		Eventually(func() bool {
			return claimIsGone(stamped.Namespace, stamped.Name)
		}, "30s", "200ms").Should(BeTrue(), "a terminal claim past its TTL is deleted")

		digest := readSlotObject(name).Status.LastClaim
		Expect(digest).NotTo(BeNil(), "the GC is safe because the digest of the episode survives it")
		Expect(digest.Phase).To(Equal(infrav2alpha1.NodeClaimFailed))
		Expect(digest.Name).To(Equal(stamped.Name))

		// A claim whose TTL has not passed is kept, and the pass that will collect it is scheduled
		// off status.finishedAt: the collection is timed, not polled.
		later := slotName("collect-later")
		newSlot(later, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		kept := newClaim(readSlotObject(later), infrav2alpha1.NodeClaimCompleted, func(c *infrav2alpha1.NodeClaim) {
			ttl := int32(3600)
			c.Spec.TTLSecondsAfterFinished = &ttl
		})

		result := reconcileClaim(kept)
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "the collection is scheduled, not polled")
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Hour), "and it is scheduled for the TTL, not for a resync loop")
		Expect(readClaimObject(kept.Name).Status.Phase).To(Equal(infrav2alpha1.NodeClaimCompleted),
			"a terminal claim inside its TTL is still readable, which is the point of the TTL")
	})

	// "a scale-down whose shutdown never lands is counted, bounded, and paused"
	It("keeps a scale-down that never lands, bounded by the shutdown Jobs it fired", func() {
		// M4. A shutdown Job that exits 0 without the machine going off is the pool's script, not the
		// controller's bug. The finalizer cannot release while the machine is up, so the loop is
		// permanent unless the attempts are counted, bounded, and the bound is a pause with an end.
		//
		// The bound is status.shutdownAttempts, and it counts shutdowns, not reconciles: the claim
		// controller's finalizer poll runs every 15s, so a streak written on that path reaches 3 in 45s
		// with zero lifecycle Jobs behind it, trips the bound after one real attempt, and starts the 30m
		// pause early. The count is stamped where a shutdown is fired - the state machine's ShutdownNode,
		// on the branch that created the shutdown Job - and the poll reads.
		//
		// The boot attempts of the episode are a separate counter (status.attempt), and a shutdown that
		// will not land must not spend them.
		name := slotName("stuck-shutdown")
		slot := newSlot(name, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseDraining)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimReady, func(c *infrav2alpha1.NodeClaim) {
			c.Spec.Request.Call = infrav2alpha1.ClaimCallDeleteNodes
			c.Spec.Request.Nodes = []string{name + "-k8s"}
		})

		k8sNode(name, true)
		markForScaleDown(nodeClaim)

		// One cycle is one attempt: the slot pass fires the shutdown, the script exits 0, the machine
		// the provider observes is still up, and the completion is refused (fsm.refuseUnobservedShutdown)
		// so the slot stays draining and the scale-down has not landed.
		for attempt := 1; attempt <= int(claim.MaxConsecutiveAttempts); attempt++ {
			reconcileSlot(readSlotObject(name))

			jobs := jobsOfSlot(name, fsm.PhaseShutdown)
			Expect(jobs).To(HaveLen(attempt), "attempt %d is one shutdown Job: the machine is up and a marked claim is not a want", attempt)

			Expect(readClaimObject(nodeClaim.Name).Status.ShutdownAttempts).To(Equal(int32(attempt)),
				"the streak counts the shutdowns this scale-down fired, not the passes that looked at the machine")

			fired := newestJobOf(jobs)
			monitorLifecycleJob(readSlotObject(name), fired)

			// The finalizer polls the observation. Three looks at a machine that stays up are three
			// looks: the streak they leave behind is the streak of the Jobs that were fired.
			for pass := 1; pass <= 3; pass++ {
				result := reconcileClaim(readClaimObject(nodeClaim.Name))
				Expect(result.RequeueAfter).To(Equal(shutdownPollResync), "pass %d polls the observation", pass)
			}

			Expect(readClaimObject(nodeClaim.Name).Status.ShutdownAttempts).To(Equal(int32(attempt)),
				"the poll of the observation counts nothing: it is read-only, and an attempt is a shutdown Job")
			Expect(readClaimObject(nodeClaim.Name).Status.Attempt).To(BeNumerically("<", claim.MaxConsecutiveAttempts),
				"a shutdown that will not land does not spend the boot attempts of the episode (M4)")

			// The shutdown Job is named per second (fsm.createShutdownJob), so the next attempt is a
			// second later. A spec that fires two shutdowns in one second asks for a name collision.
			waitForNextSecond()
		}

		stuck := readClaimObject(nodeClaim.Name)
		Expect(stuck.Status.Phase).To(Equal(infrav2alpha1.NodeClaimReady),
			"a scale-down that has not landed does not end the claim: the machine is still up")
		Expect(stuck.Finalizers).To(ContainElements(ClaimFinalizer),
			"the request to power the machine off is kept: a scale-down is reversed, never forgotten")
		Expect(stuck.Status.ShutdownAttempts).To(Equal(claim.MaxConsecutiveAttempts),
			"the bound is spent on the shutdown counter, and the refusal is the record")
		Expect(stuck.Status.LastShutdownAttemptAt).NotTo(BeNil(),
			"the streak carries the clock its reset grace is measured from")

		// Past the bound the model stops firing a shutdown that will not land: the slot reconciler
		// runs the law again, finds the streak spent, and buys no fourth Job.
		before := len(jobsOfSlot(name, fsm.PhaseShutdown))

		result := reconcileSlot(readSlotObject(name))
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "the pause is a requeue, not a silent stop")
		Expect(result.RequeueAfter).To(BeNumerically("<=", claim.ShutdownAttemptResetGrace),
			"and it is paced to the end of the reset grace, so the pause ends on a pass")
		Expect(jobsOfSlot(name, fsm.PhaseShutdown)).To(HaveLen(before),
			"no fourth shutdown Job for a script that keeps exiting 0")

		// The observation flipping to off is what ends the loop: the scale-down landed, the claim ends
		// completed, the finalizer comes off, and the object - with the streak it counted - goes away.
		// The slot's digest is the record that survives it.
		powerOff(name)

		reconcileClaim(readClaimObject(nodeClaim.Name))

		Eventually(func() bool {
			return claimIsGone(nodeClaim.Namespace, nodeClaim.Name)
		}, "30s", "200ms").Should(BeTrue(),
			"a landed scale-down releases the finalizer, and the marked object is deleted on that write")

		digest := readSlotObject(name).Status.LastClaim
		Expect(digest).NotTo(BeNil(), "the episode is the slot's story after the claim is gone")
		Expect(digest.Phase).To(Equal(infrav2alpha1.NodeClaimCompleted))
		Expect(digest.Reason).To(Equal(infrav2alpha1.ClaimReasonNodeDeleted))
		Expect(digest.ShutdownAttempts).To(Equal(claim.MaxConsecutiveAttempts),
			"the attempts the stuck shutdown spent are on the record, and the streak ends with the object")
	})

	// "a slot deleted while its claim holds the finalizer takes the claim with it"
	It("releases a claim whose slot is deleted under its finalizer", func() {
		// B3. The documented rollout deletes the Node CRs (§9), and garbage collection of a namespace
		// does the same. A claim that holds the finalizer blocks its own deletion with the slot gone:
		// leaving it there is a claim stuck in Terminating forever, never retried, with the machine it
		// asked for running behind it. The claim controller has nothing left to run - the record lives
		// on a slot that does not exist - so the finalizer comes off, and GC finishes the object.
		name := slotName("slot-deleted")
		newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		nodeClaim := createProviderClaim(readSlotObject(name))
		setClaimPhase(nodeClaim, infrav2alpha1.NodeClaimBooting, "")

		// The claim is marked (the scale-down that named it, or the ownerReference's garbage
		// collection when the slot goes; envtest runs no garbage collector, so the mark is written
		// here) and the slot is deleted: the pair the rollout produces.
		markForScaleDown(nodeClaim)

		deleted := readSlotObject(name)
		Expect(k8sClient.Delete(ctx, deleted)).To(Succeed())

		for pass := 1; pass <= 6; pass++ {
			current, found := readClaimIfThere(nodeClaim.Namespace, nodeClaim.Name)
			if !found {
				break
			}

			reconcileClaim(current)

			after, still := readClaimIfThere(nodeClaim.Namespace, nodeClaim.Name)
			if still {
				Expect(after.Finalizers).NotTo(ContainElements(ClaimFinalizer),
					"pass %d removes the finalizer: nothing is left that could satisfy it", pass)
				Expect(after.Status.Reason).To(Equal(infrav2alpha1.ClaimReasonSlotDeleted))
			}
		}

		Eventually(func() bool {
			return claimIsGone(nodeClaim.Namespace, nodeClaim.Name)
		}, "30s", "200ms").Should(BeTrue(),
			"a marked object whose finalizer was released is deleted, and the claim is not stuck in Terminating")
	})

	// "one live claim per slot, enforced by the controller and not only by admission"
	It("fails the extra live claim of a slot and keeps the newest one", func() {
		// The webhook refuses a second live claim at admission; this is the same rule where the API
		// cannot see it - two providers racing, a claim applied by hand, a provider that listed a
		// stale inventory. The newest live claim owns the slot, and every older one is failed with
		// reason Superseded, naming the slot and the claim that owns it.
		name := slotName("two-claims")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)

		older := newClaimNamed(slot, name+"-claim-a", infrav2alpha1.NodeClaimBooting, nil)
		time.Sleep(1100 * time.Millisecond) // a second of creation-timestamp resolution, so "newest" is decidable
		newClaimNamed(slot, name+"-claim-b", infrav2alpha1.NodeClaimBooting, nil)

		reconcileClaim(older)

		superseded := readClaimObject(older.Name)
		Expect(superseded.Status.Phase).To(Equal(infrav2alpha1.NodeClaimFailed),
			"two live claims want one machine, and that is an operator-visible bug, not a supported state")
		Expect(superseded.Status.Reason).To(Equal(infrav2alpha1.ClaimReasonSuperseded))
		Expect(superseded.Status.FinishedAt).NotTo(BeNil(), "the extra claim is terminal, and its TTL starts now")

		kept := readClaimObject(name + "-claim-b")
		Expect(kept.Status.Phase).To(Equal(infrav2alpha1.NodeClaimBooting),
			"the newest live claim is the one the slot answers for")

		// The slot's digest is the newest episode's story, not the late write of the claim that was
		// just failed as superseded (StampDigest's ordering rule).
		reconcileClaim(kept)
		reconcileClaim(readClaimObject(older.Name))
		Expect(readSlotObject(name).Status.LastClaim.Name).NotTo(Equal(older.Name),
			"a superseded claim's collection does not rewrite the digest with the older episode")
	})

	// "a created claim with no phase is live: the window before the first status write"
	It("counts a created claim with no phase as the live claim of its slot", func() {
		// M1. The status subresource strips `status` from a create, and the CRD declares no phase
		// default (a default on a subresource field is only stamped by the first status write), so a
		// brand-new claim carries no phase at all. phase.IsLive reads that window as pending, and
		// every reader that decides whether a machine is wanted asks IsLive: TargetSize, IsFree,
		// ClaimedSlots, the webhook, and these two reconcilers.
		name := slotName("phase-empty")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseOff)
		nodeClaim := newClaim(slot, "", nil)

		fresh := readClaimObject(nodeClaim.Name)
		Expect(fresh.Status.Phase).To(BeEmpty(), "the create carried no status: the controller's first write has not landed")

		// The claim controller treats it as a live claim in the boot window: it seeds the attempt,
		// the deadline and the bound apply to it, and its digest is stamped.
		reconcileClaim(fresh)

		seeded := readClaimObject(nodeClaim.Name)
		Expect(seeded.Status.Attempt).To(Equal(int32(1)),
			"a phaseless claim is a request the controller seeds, not an object it ignores")
		Expect(readSlotObject(name).Status.LastClaim.Name).To(Equal(nodeClaim.Name),
			"the slot's digest is stamped by the pass that seeded the brand-new request")

		// A second live claim on the slot enters the same rule, and the phaseless claim is in the
		// set the rule reads: an empty phase is live, so the older of the pair - the one with no
		// phase - is the extra, named and failed by the code that names the slot.
		time.Sleep(1100 * time.Millisecond) // a second of creation-timestamp resolution, so "newest" is decidable
		extra := newClaimNamed(slot, name+"-claim-extra", infrav2alpha1.NodeClaimPending, nil)

		reconcileClaim(extra)
		reconcileClaim(readClaimObject(nodeClaim.Name))

		Expect(readClaimObject(extra.Name).Status.Phase).To(Equal(infrav2alpha1.NodeClaimPending),
			"the newest live claim is the one the slot answers for")
		Expect(readClaimObject(nodeClaim.Name).Status.Reason).To(Equal(infrav2alpha1.ClaimReasonSuperseded),
			"the phaseless claim was in the live set, and lost the ordering: an empty phase is live")

		// The claim the slot answers for boots: law 1 asks the live question, and a request the API
		// server accepted wants a machine immediately.
		reconcileSlot(readSlotObject(name))
		Expect(jobsOfSlot(name, fsm.PhaseStartup)).To(HaveLen(1),
			"a live claim the API server accepted boots on the pass that reads it: the phase of the claim is not a record a pass has to be handed")
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
		k8sNode(name, false)

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
		k8sNode(serving, true)

		result := reconcileSlot(readSlotObject(serving))
		Expect(result.RequeueAfter).To(Equal(idleResync), "law 2: capacity serving pods is left alone")
		Expect(jobsOfSlot(serving, fsm.PhaseShutdown)).To(BeEmpty(), "the phase the slot reports starts no shutdown")
		Expect(readK8sNode(serving + "-k8s").Spec.Unschedulable).To(BeFalse())

		reconcileObservation(serving + "-k8s")
		Expect(readSlotObject(serving).Status.Phase).To(Equal(infrav2alpha1.NodePhaseReady))
	})

	// "the boot deadline of a ready claim is closed: the machine it bought is not destroyed"
	It("leaves a ready claim alone when its boot deadline has passed", func() {
		// B1. The deadline is the budget of the boot window, measured from spec.requestedAt. Every
		// healthy machine in the pool is requested more than 12m10s before it serves its first pod,
		// so a deadline that applies to a ready claim is a controller that destroys capacity on a
		// timer: the machine is force-shut-down, cordoned, and the pool buys it again.
		name := slotName("ready-overdue")
		slot := newSlot(name, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseReady)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimReady, func(c *infrav2alpha1.NodeClaim) {
			overdue := metav1.NewTime(time.Now().Add(-3 * time.Hour))
			c.Spec.DeadlineAt = &overdue
		})

		k8sNode(name, true)

		reconcileClaim(nodeClaim)

		ready := readClaimObject(nodeClaim.Name)
		Expect(ready.Status.Phase).To(Equal(infrav2alpha1.NodeClaimReady),
			"the deadline bounds the boot: a claim that reached ready owns a machine, and the boot budget says nothing about it")
		Expect(ready.Status.Reason).To(BeEmpty(), "no DeadlineExceeded on a healthy machine")

		reconcileSlot(readSlotObject(name))
		reconcileClaim(readClaimObject(nodeClaim.Name))

		Expect(jobsOfSlot(name, fsm.PhaseShutdown)).To(BeEmpty(), "no shutdown Job: the machine is wanted")
		Expect(readK8sNode(name+"-k8s").Spec.Unschedulable).To(BeFalse(), "the serving node is not cordoned")
		Expect(readClaimObject(nodeClaim.Name).Status.Phase).To(Equal(infrav2alpha1.NodeClaimReady))
		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseReady),
			"the slot keeps reporting the capacity it has")
	})

	// "a ready claim whose machine is observed off past a boot window is failed: MachineGone"
	It("fails a ready claim whose machine has been observed off past the grace", func() {
		// M2. phase.Derive reports booting for a ready claim with the machine off, law 1 needs the
		// shutdown state to fire a boot and law 3 needs the machine observed up to fire a shutdown:
		// the case has no owner, and ClaimReasonMachineGone is declared in the API and written by
		// nobody. The claim controller owns it, bounded by an observation, so a reboot (which shows
		// the same shape for minutes) is not an attempt that ends.
		name := slotName("machine-gone")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimReady, nil)

		// The first pass that sees the machine off records the loss and ends nothing.
		reconcileClaim(nodeClaim)

		watched := readClaimObject(nodeClaim.Name)
		Expect(watched.Status.Phase).To(Equal(infrav2alpha1.NodeClaimReady),
			"a machine that is off for one pass is a machine that may be rebooting")
		observed, seen := claim.MachineLossObservedSince(watched)
		Expect(seen).To(BeTrue(), "the loss is recorded on the object, so the bound survives a manager restart")
		Expect(observed).To(BeTemporally("~", time.Now(), time.Minute), "the first pass starts the clock it will outlive")

		// The loss is old: the observation has outlasted a whole boot window.
		stampMachineLossAge(nodeClaim.Name, claim.MachineGoneGrace()+time.Minute)

		reconcileClaim(readClaimObject(nodeClaim.Name))

		gone := readClaimObject(nodeClaim.Name)
		Expect(gone.Status.Phase).To(Equal(infrav2alpha1.NodeClaimFailed),
			"a machine that stays off longer than it takes to buy one is gone")
		Expect(gone.Status.Reason).To(Equal(infrav2alpha1.ClaimReasonMachineGone))
		Expect(gone.Status.FinishedAt).NotTo(BeNil(), "a terminal claim carries the time the TTL measures from")
		Expect(readSlotObject(name).Status.LastClaim.Reason).To(Equal(infrav2alpha1.ClaimReasonMachineGone),
			"the slot keeps the story of why it is free")

		// A terminal claim is not a want: the slot is free, and the next request of the slot is a
		// new attempt. The report is the slot pass's output, and the machine is off, so the slot
		// lands at off and buys nothing.
		reconcileSlot(readSlotObject(name))
		Expect(jobsOfSlot(name, fsm.PhaseStartup)).To(BeEmpty(),
			"a slot whose claim ended is free: nothing boots from a record, and nothing boots for a machine the pool has stopped wanting")
		Expect(jobsOfSlot(name, fsm.PhaseShutdown)).To(BeEmpty(),
			"the machine is observed off: there is nothing left to power off")
	})

	// "a lifecycle Job that finished with nobody watching is picked up by the next pass"
	It("lands a claim whose startup Job finished before the reconciler looked at it", func() {
		// M3. The Job monitor is a goroutine of the process that ran the transition. A manager that
		// restarts in that window loses the goroutine and keeps the objects: a booting claim whose
		// startup Job completed during the restart has no event left to fire, sits at booting until
		// its deadline fails it, and the machine that booted fine is force-shut-down. The Job is the
		// fact, so the reconcile reads it.
		name := slotName("job-recovered")
		slot := newSlot(name, infrav2alpha1.PowerStateOn, infrav2alpha1.NodePhaseBooting)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimBooting, nil)
		k8sNode(name, true)

		job := createStartupJobFixture(slot, name+"-startup-envtest")
		completeJob(job)

		reconcileSlot(readSlotObject(name))

		landed := readClaimObject(nodeClaim.Name)
		Expect(landed.Status.Phase).To(Equal(infrav2alpha1.NodeClaimReady),
			"the finished startup Job of a booting claim is the evidence the readiness gate answers for")
		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseReady),
			"and the slot's report is derived from the claim the recovery moved")
		Expect(jobsOfSlot(name, fsm.PhaseShutdown)).To(BeEmpty(), "a recovered boot is not punished with a shutdown")
	})

	// "the recovery of a finished Job answers for the boot window, not for a machine that is serving"
	It("leaves a ready claim alone when the only Job on its slot is the completed startup of its own boot", func() {
		// M3, the correlated half. A ready claim whose machine is momentarily observed off derives
		// `startingup` - the same report a real boot derives - and a recovery gated on the derived state
		// alone re-fires the startup Job this claim's readiness gate consumed long ago. The gate then
		// runs again on a machine that is already serving: 96s without a node object, ~4m30s with one
		// that is NotReady, and the claim ends failed/ReadinessGateTimeout instead of the MachineGone the
		// 12m10s observation bound is there to record. When the machine comes back, the slot sees "no
		// live claim + on" and powers it off: the reboot churn the grace exists to close.
		//
		// The Job is the claim's own - created after the claim, so nothing but the boot-window gate
		// refuses it - and that gate is the fix: the recovery answers for an attempt that is running.
		name := slotName("ready-blip")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimReady, nil)

		// The machine is off and its kubelet left its node object behind NotReady: the blip shape a
		// reboot, a slow provider and a lost machine all show.
		k8sNode(name, false)

		job := createStartupJobFixture(slot, name+"-startup-envtest")
		completeJob(job)

		result := reconcileSlot(readSlotObject(name))

		ready := readClaimObject(nodeClaim.Name)
		Expect(ready.Status.Phase).To(Equal(infrav2alpha1.NodeClaimReady),
			"a consumed startup Job is not the evidence of a boot: the claim owns a machine, and the boot window is closed")
		Expect(ready.Status.Reason).To(BeEmpty(),
			"no ReadinessGateTimeout: the gate of the attempt that bought this machine ran once, and it passed")
		Expect(ready.Status.FinishedAt).To(BeNil(), "the episode is not over: a machine that is off for a pass is rebooting")

		Expect(result.RequeueAfter).To(BeNumerically(">", 0),
			"the blip is a transitional report, and the pass that looks again is scheduled")
		Expect(jobsOfSlot(name, fsm.PhaseShutdown)).To(BeEmpty(), "a momentary blip powers nothing off")
		Expect(jobsOfSlot(name)).To(HaveLen(1), "the recovery creates nothing: the completed startup Job is the only Job on the slot")
		Expect(readK8sNode(name+"-k8s").Spec.Unschedulable).To(BeFalse(), "the serving node is not cordoned by a recovery")

		// The owner of the shape is the claim controller, bounded by an observation of the loss: the
		// first pass records it and ends nothing, which is the sentence the machine-gone spec makes.
		reconcileClaim(readClaimObject(nodeClaim.Name))

		watched := readClaimObject(nodeClaim.Name)
		Expect(watched.Status.Phase).To(Equal(infrav2alpha1.NodeClaimReady), "the loss is being watched, not spent")
		_, seen := claim.MachineLossObservedSince(watched)
		Expect(seen).To(BeTrue(), "the bound that ends this claim is the observation, and it started on this pass")
	})

	// "the recovery reads the newest Job of the phase, not the newest finished one"
	It("leaves a booting claim to the startup Job that is running, not to an older finished one", func() {
		// M3, the in-flight half. A booting claim with a finished failed startup Job of an earlier
		// attempt on the same slot is failed on the next pass by the older Job while its own Job is
		// still running: the newest *finished* Job is not the operation, and firing its event steals the
		// verdict of an attempt that has not been decided yet. The newest Job of the wanted phase is the
		// operation; if it is unfinished, the monitor owns its event and this pass has nothing to say.
		name := slotName("boot-in-flight")
		slot := newSlot(name, infrav2alpha1.PowerStateOff, infrav2alpha1.NodePhaseBooting)
		nodeClaim := newClaim(slot, infrav2alpha1.NodeClaimBooting, nil)
		k8sNode(name, false)

		// The record of the attempt before: finished, failed, and older than the boot running now.
		older := createStartupJobFixture(slot, name+"-startup-older")
		failJob(older)

		waitForNextSecond()

		// The attempt in flight: the newest startup Job of the slot, unfinished.
		createStartupJobFixture(slot, name+"-startup-running")

		reconcileSlot(readSlotObject(name))

		booting := readClaimObject(nodeClaim.Name)
		Expect(booting.Status.Phase).To(Equal(infrav2alpha1.NodeClaimBooting),
			"the running Job is the operation, and an older finished one cannot end the attempt it is running")
		Expect(booting.Status.Reason).To(BeEmpty(), "no StartupJobFailed from somebody else's attempt")
		Expect(booting.Status.FinishedAt).To(BeNil(), "the attempt is still running, and its deadline is intact")

		Expect(jobsOfSlot(name, fsm.PhaseShutdown)).To(BeEmpty(), "a boot in flight is not shut down by a recovery")
		Expect(jobsOfSlot(name, fsm.PhaseStartup)).To(HaveLen(2), "the pass creates no third Job: the boot it is watching is the one that exists")
		Expect(readSlotObject(name).Status.Phase).To(Equal(infrav2alpha1.NodePhaseBooting),
			"the slot keeps reporting the boot its claim asked for")
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
// createStartupJobFixture registers a running startup Job under a name the spec chooses, so a slot can
// carry two Jobs of the phase - the finished record of an earlier attempt and the running Job of the
// one in flight - and "the newest" is a question the spec can answer. The shutdown Jobs of the specs
// are created by the controller: law 3 is what fires a shutdown.
func createStartupJobFixture(slot *infrav2alpha1.Node, name string) *batchv1.Job {
	jobPhase := fsm.PhaseStartup

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
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

// newestJobOf is the Job a spec's next step acts on: a slot that carries several lifecycle Jobs of
// one phase has one current operation, and it is the newest - the same rule the recovery of a
// finished Job applies to a slot (internal/controller/infra/lifecycle.go).
func newestJobOf(jobs []batchv1.Job) *batchv1.Job {
	Expect(jobs).NotTo(BeEmpty(), "the spec has a Job of this phase to act on")

	newest := &jobs[0]
	for i := range jobs[1:] {
		candidate := &jobs[i+1]
		if candidate.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = candidate
		}
	}

	return newest
}

// monitorLifecycleJob plays the Job monitor of a lifecycle Job that has finished. In production the
// monitor is a goroutine of the pass that fired the operation: it holds the coordination lock, reads
// the Job, and reports the completion - which is what releases the lock. envtest runs no Job
// controller, so the spec is the goroutine. The state machine is built while the Job is still in
// flight, because that is the state its event is reachable from, and the Job is stamped finished
// before the event runs.
func monitorLifecycleJob(slot *infrav2alpha1.Node, job *batchv1.Job) {
	GinkgoHelper()

	reconciler := &NodeReconciler{Client: k8sClient, Scheme: scheme.Scheme}

	live, err := reconciler.liveClaimOf(ctx, slot)
	Expect(err).NotTo(HaveOccurred())

	machine := startupGateMachine(reconciler, slot, live)
	Expect(machine.GetCurrentState()).To(Equal(fsm.StateShuttingDown),
		"the shutdown Job in flight is the state its completion is reachable from")

	completeJob(job)

	// The machine the script did not power off keeps heartbeating *after* the Job finished: that is
	// the pair fsm.machineObservedUp reads to refuse the completion, and the bound it measures
	// against is the Job's completion time (fsm.shutdownObservationBound). The second the Job
	// completed is the same second as its completion time, so the heartbeat the spec stamps is one
	// later - the kubelet's own cadence is 5s, and a spec that stamps both in one second asserts a
	// machine that stopped heartbeating at the instant the script exited.
	waitForNextSecond()
	heartbeatK8sNode(slot.Name)

	Expect(machine.JobCompleted(job)).To(Succeed())
}

// waitForNextSecond moves a spec past the resolution of the name the controller gives a lifecycle Job
// (<slot>-shutdown-<unix second>): two shutdowns fired inside one second ask for one name, and the
// second create fails on it. The Jobs the fixtures write by hand are named by the spec, so only the
// Jobs the controller creates are affected.
func waitForNextSecond() {
	time.Sleep(1100 * time.Millisecond)
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
		fsm.WithClaim(live, claimWriterFor(k8sClient, fresh, reconciler.EventRecorder)))
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

// ---------------------------------------------------------------------------
// envtest helpers: the shapes a spec seeds that the API server will not stamp.
// ---------------------------------------------------------------------------

// newClaimNamed registers a live claim of a slot under a name the spec chooses, so two claims of one
// slot can be created in a known order (the "newest wins" rule of one-live-claim-per-slot is about
// creation timestamps, and a name is how a spec says which is which).
func newClaimNamed(slot *infrav2alpha1.Node, name string, claimPhase infrav2alpha1.NodeClaimPhase, mutate func(*infrav2alpha1.NodeClaim)) *infrav2alpha1.NodeClaim {
	nodeClaim := &infrav2alpha1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: slot.Namespace,
			Labels: map[string]string{
				claimLabelSlot:  slot.Name,
				claimLabelGroup: readSlotObject(slot.Name).Labels["group"],
			},
		},
		Spec: infrav2alpha1.NodeClaimSpec{
			SlotRef:     corev1ObjectRef(slot),
			RequestedBy: "cluster-autoscaler",
			Request: infrav2alpha1.ClaimRequest{
				Call:  infrav2alpha1.ClaimCallIncreaseSize,
				Delta: 1,
				Group: readSlotObject(slot.Name).Labels["group"],
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

// addFinalizer stamps the scale-down finalizer the way the recorder's pending -> booting write does,
// for a spec that lands the phase on the status subresource by hand and needs the metadata half of
// the write present.
func addFinalizer(name string) {
	fresh := readClaimObject(name)
	fresh.Finalizers = append(fresh.Finalizers, ClaimFinalizer)
	Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
}

// markForScaleDown is NodeGroupDeleteNodes' write on a claim that carries the finalizer: the delete
// lands the deletionTimestamp, and the object stays for the claim controller to finish. A delete
// without the finalizer would make the object disappear, and the spec would assert a shape the model
// never reaches.
func markForScaleDown(nodeClaim *infrav2alpha1.NodeClaim) {
	GinkgoHelper()

	addFinalizer(nodeClaim.Name)
	Expect(k8sClient.Delete(ctx, readClaimObject(nodeClaim.Name))).To(Succeed())

	fresh := readClaimObject(nodeClaim.Name)
	Expect(fresh.DeletionTimestamp).NotTo(BeNil(), "a scale-down names the machine by marking its claim")
}

// readClaimIfThere reads a claim, and reports "gone" as not-found without failing the spec: the
// collection and the garbage-collection paths delete the object, and a spec that polls for the end
// has to be able to look.
func readClaimIfThere(namespace, name string) (*infrav2alpha1.NodeClaim, bool) {
	nodeClaim := &infrav2alpha1.NodeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, nodeClaim); err != nil {
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the only read failure a spec tolerates on a claim is that it is gone")

		return nil, false
	}

	return nodeClaim, true
}

// collectClaim takes a claim out of the way of the next episode of its slot. A pending claim carries
// no finalizer, so the delete is the cancellation the model prescribes: the object goes, and the slot
// is free to be claimed again.
func collectClaim(name string) {
	GinkgoHelper()

	nodeClaim := readClaimObject(name)
	Expect(k8sClient.Delete(ctx, nodeClaim)).To(Succeed())

	Eventually(func() bool {
		return claimIsGone(nodeClaim.Namespace, nodeClaim.Name)
	}, "30s", "200ms").Should(BeTrue(), "a claim with no finalizer is deleted by the delete")
}

// claimIsGone is the collection assertion: the object is not there any more.
func claimIsGone(namespace, name string) bool {
	_, found := readClaimIfThere(namespace, name)

	return !found
}

// stampMachineLossAge records the machine-loss condition of a claim with a lastTransitionTime in the
// past: the shape a claim carries when the loss was first observed long ago. The bound is measured
// off the object, not off a controller's memory, so a manager restart does not restart the clock,
// and a spec that wants an old observation writes it the way the writer does.
func stampMachineLossAge(name string, age time.Duration) {
	fresh := readClaimObject(name)

	condition := metav1.Condition{
		Type:               infrav2alpha1.ClaimConditionMachineObservedOff,
		Status:             metav1.ConditionTrue,
		Reason:             infrav2alpha1.ClaimReasonMachineGone,
		Message:            "the spec observed the machine off, and it has been off for the whole grace",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
	}

	replaced := false

	for i := range fresh.Status.Conditions {
		if fresh.Status.Conditions[i].Type == condition.Type {
			fresh.Status.Conditions[i] = condition
			replaced = true

			break
		}
	}

	if !replaced {
		fresh.Status.Conditions = append(fresh.Status.Conditions, condition)
	}

	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}

// completeJob stamps a lifecycle Job succeeded, the way the cluster does when the pod the operator's
// script runs exits 0. It is the fact the in-process Job monitor reads, and the fact a reconcile
// re-reads after a restart.
func completeJob(job *batchv1.Job) {
	GinkgoHelper()

	fresh := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), fresh)).To(Succeed())

	now := metav1.NewTime(time.Now())

	fresh.Status.StartTime = &now
	fresh.Status.Succeeded = 1
	fresh.Status.CompletionTime = &now
	fresh.Status.Conditions = []batchv1.JobCondition{
		{
			Type:               batchv1.JobSuccessCriteriaMet,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
		},
		{
			Type:               batchv1.JobComplete,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
		},
	}
	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}

// heartbeatK8sNode stamps the kubelet's last heartbeat on the node object of a slot, at "now", and
// keeps the node Ready. It is the fact an observation of "the machine is up" is made of: the Ready
// condition with a heartbeat newer than the instant the shutdown Job completed (see
// monitorLifecycleJob, and fsm.shutdownObservationBound for the bound).
func heartbeatK8sNode(slotName string) {
	GinkgoHelper()

	node := readK8sNode(slotName + "-k8s")
	now := metav1.NewTime(time.Now())

	found := false

	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type != corev1.NodeReady {
			continue
		}

		node.Status.Conditions[i].Status = corev1.ConditionTrue
		node.Status.Conditions[i].LastHeartbeatTime = now
		node.Status.Conditions[i].LastTransitionTime = now
		found = true
	}

	Expect(found).To(BeTrue(), "the slot's node object carries its Ready condition")
	Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
}

// failJob stamps a lifecycle Job failed, the way the cluster does when the pod the operator's script
// runs exhausts its retries. A finished Job of a bygone attempt is the record the recovery of a
// running one must not mistake for evidence.
func failJob(job *batchv1.Job) {
	GinkgoHelper()

	fresh := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), fresh)).To(Succeed())

	now := metav1.NewTime(time.Now())

	fresh.Status.StartTime = &now
	fresh.Status.Failed = 1
	fresh.Status.Conditions = []batchv1.JobCondition{
		{
			Type:               batchv1.JobFailureTarget,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "BackoffLimitExceeded",
			Message:            "the spec's lifecycle script exited non-zero",
		},
		{
			Type:               batchv1.JobFailed,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "BackoffLimitExceeded",
			Message:            "the spec's lifecycle script exited non-zero",
		},
	}
	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}

// powerOff lands the observation of a machine that went off: the provider's power-off, the kubelet
// stopping its heartbeat, and the observation path writing the slot. The spec's shutdown script is a
// container envtest will not run, so this is how a spec says "the machine is off now".
func powerOff(slotName string) {
	GinkgoHelper()

	fresh := readSlotObject(slotName)
	fresh.Status.PowerState = infrav2alpha1.PowerStateOff
	Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}
