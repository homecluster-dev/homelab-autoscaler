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

package fsm

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/looplab/fsm"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/phase"
	"github.com/homecluster-dev/homelab-autoscaler/internal/taints"
)

// NewNodeStateMachine creates a new FSM instance for node state management.
//
// Options are optional: with none the FSM behaves exactly as it did before the event
// recorder existed, which keeps every existing caller and test working. Pass
// WithEventRecorder(mgr.GetEventRecorder(...)) to have the lifecycle transitions
// recorded on the Node CR.
func NewNodeStateMachine(
	node *infrav2alpha1.Node,
	k8sClient client.Client,
	scheme *runtime.Scheme,
	coordMgr CoordinationManager,
	opts ...Option,
) *NodeStateMachine {
	nm := &NodeStateMachine{
		node:                 node,
		client:               k8sClient,
		scheme:               scheme,
		coordinationManager:  coordMgr,
		jobTimeout:           DefaultJobTimeout,
		uncordonTimeout:      DefaultUncordonTimeout,
		readinessGateTimeout: DefaultReadinessGateTimeout,
	}

	for _, opt := range opts {
		opt(nm)
	}

	// A caller that passes no input gets the observation alone: the power the cluster reports for the
	// machine, and no live claim. That is the shape of a slot with no claim, and it is what the
	// historical callers of this constructor - built from a Node read from the cache - mean.
	if nm.input.ObservedPower == "" {
		nm.input.ObservedPower = nm.node.Status.PowerState
	}

	// Initialize the FSM with the state the input law derives: (live claim phase, observed power,
	// node usability). The v1alpha1 machine seeded itself from status.progress, i.e. from a record
	// of what it last did, and every rule about which writers outrank which is a consequence of
	// that; the record is gone as an input.
	initialState := phase.State(nm.input)

	nm.fsm = fsm.NewFSM(
		initialState,
		GetFSMTransitions(),
		GetFSMCallbacks(nm),
	)

	return nm
}

// GetCurrentState returns the current FSM state
func (nm *NodeStateMachine) GetCurrentState() string {
	return nm.fsm.Current()
}

// CanTransition checks if an event can be triggered from the current state
func (nm *NodeStateMachine) CanTransition(event string) bool {
	return nm.fsm.Can(event)
}

// StartNode triggers the StartNode event to begin node startup.
//
// The FSM event runs first, because taking the coordination lock is the first thing a
// lifecycle operation does: cleanupPendingJobs deletes the pending lifecycle Job of the slot
// and the taint removal touches the Kubernetes node, and a startup that is about to be
// refused by a scale-down holding the slot has no business doing either. Everything after
// the event runs on a slot this operation owns.
func (nm *NodeStateMachine) StartNode() error {
	logger := log.Log.WithName("fsm")
	ctx := context.TODO()

	logger.Info("Triggering StartNode event", "node", nm.node.Name, "currentState", nm.fsm.Current())

	// The attempt bound of the model lives on the claim, not on the slot: a claim whose
	// status.attempt is past claim.MaxConsecutiveAttempts is failed by the claim controller before it
	// buys a machine, so this call is never reached for an exhausted slot, and the counter that ends
	// with the episode needs no reset heuristic (docs/design/nodeclaim.md §4).

	// Trigger FSM event. A cancellation from the coordination lock leaves status.progress on
	// the stable state the transition started from (see beforeStartNode) and returns a
	// LockConflictError, which the caller defers: the intent this call carries is in
	// spec.desiredPowerState already, so the request is retried, not dropped.
	if err := nm.fsm.Event(ctx, EventStartNode); err != nil {
		nm.recordTransitionRefusal(false, err)
		return fmt.Errorf("FSM transition failed: %w", err)
	}

	// Clean up any existing pending jobs now that this operation owns the slot
	if err := nm.cleanupPendingJobs(nm.node.Spec.KubernetesNodeName); err != nil {
		logger.Error(err, "Failed to cleanup pending jobs, continuing with startup", "node", nm.node.Name)
		// Continue with startup even if cleanup fails
	}

	// Remove cluster autoscaler taint if present
	if err := nm.removeClusterAutoscalerTaint(nm.node.Spec.KubernetesNodeName); err != nil {
		logger.Error(err, "Failed to remove cluster autoscaler taint, continuing with startup", "node", nm.node.Name)
		// Continue with startup even if taint removal fails
	}

	// Create and start Kubernetes Job
	job, err := nm.createStartupJob()
	if err != nil {
		// Record the failure before rolling the FSM back, and record it only here: the
		// JobFailed transition into shutdown suppresses its completion event, so a
		// failed scale up produces exactly one Warning.
		nm.recordScaleFailure(false, fmt.Sprintf("failed to create startup job: %s", errorDetail(err)))
		// Trigger failure event to rollback. The rollback moves the slot out of startingup,
		// and afterJobFailed releases the coordination lock: a failed operation never leaves
		// a transitional progress or a lock behind.
		if err := nm.fsm.Event(ctx, EventJobFailed); err != nil {
			logger.Error(err, "Failed to trigger JobFailed event", "node", nm.node.Name)
		}
		return fmt.Errorf("failed to create startup job: %w", err)
	}

	nm.currentJob = job.Name
	nm.jobStartTime = time.Now()

	// The lifecycle Job of this operation exists: the input records it, and the report the same
	// pass derives is a boot, not a pending claim.
	nm.input.ShutdownJobInFlight = false
	nm.input.StartupJobInFlight = true

	// The pending -> booting edge of the claim, on the same path that created the startup Job: an
	// attempt is a machine bought, not a reconcile pass, and from here the boot is committed -
	// nothing cancels it (docs/design/nodeclaim.md §3).
	nm.commitAttempt("")
	nm.reportPhase()

	// Start async job monitoring
	go nm.monitorJobCompletion(job.Name)

	logger.Info("Node startup initiated", "node", nm.node.Name, "job", job.Name)
	return nil
}

// ShutdownNode triggers the ShutdownNode event to begin node shutdown.
//
// The FSM event runs first, for the same reason as StartNode: cordoning the Kubernetes node
// and deleting the pending lifecycle Job of the slot are side effects this operation only
// owns once the coordination lock is taken. A shutdown requested while a scale-up holds the
// slot is refused by the lock, and the refusal now leaves the slot exactly as it was - the
// startup Job of the operation in flight is not deleted, the node is not cordoned, and
// status.progress is not moved to shuttingdown.
//
// The refused shutdown is not lost: spec.desiredPowerState (off, written by
// NodeGroupDeleteNodes) is the intent, and it is what the node controller reconciles from,
// so the next pass calls this method again and the shutdown runs when the scale-up releases
// the lock. The returned error wraps a LockConflictError carrying the remaining lifetime of
// that lock, so the caller can requeue for that deadline (see
// handleStateTransitionsWithFSM) instead of letting controller-runtime rate-limit a request
// that is known to be valid.
func (nm *NodeStateMachine) ShutdownNode() error {
	logger := log.Log.WithName("fsm")
	ctx := context.TODO()

	logger.Info("Triggering ShutdownNode event", "node", nm.node.Name, "currentState", nm.fsm.Current())

	// A scale-down is a claim (request.call: DeleteNodes), and its attempts are counted on that
	// claim and end when it ends: the v1alpha1 ShutdownAttempts condition on the long-lived slot,
	// with its two reset sites, is gone (§4). The count is stamped below, on the path that created
	// the shutdown Job, because an attempt is a machine the pool was asked to power off and not a
	// pass that looked at it.

	// Trigger FSM event, taking the coordination lock. See StartNode for what a cancellation
	// does and does not leave behind.
	if err := nm.fsm.Event(ctx, EventShutdownNode); err != nil {
		nm.recordTransitionRefusal(true, err)
		return fmt.Errorf("FSM transition failed: %w", err)
	}

	// Clean up any existing pending jobs now that this operation owns the slot
	if err := nm.cleanupPendingJobs(nm.node.Spec.KubernetesNodeName); err != nil {
		logger.Error(err, "Failed to cleanup pending jobs, continuing with shutdown", "node", nm.node.Name)
		// Continue with shutdown even if cleanup fails
	}

	// Cordon the Kubernetes node before shutdown
	if err := nm.setNodeUnschedulable(nm.node.Spec.KubernetesNodeName); err != nil {
		logger.Error(err, "Failed to cordon Kubernetes node, continuing with shutdown", "kubernetesNode", nm.node.Spec.KubernetesNodeName)
		// Continue with shutdown even if cordoning fails
	}

	// Create and start Kubernetes Job
	job, err := nm.createShutdownJob()
	if err != nil {
		// See StartNode: the failure is recorded once, here, and the rollback
		// transition suppresses its completion event.
		nm.recordScaleFailure(true, fmt.Sprintf("failed to create shutdown job: %s", errorDetail(err)))
		// Trigger failure event to rollback. The rollback moves the slot out of shuttingdown
		// and afterJobFailed releases the lock, so a shutdown that never got a Job leaves a
		// stable progress and no lock behind, ready for the next attempt.
		if err := nm.fsm.Event(ctx, EventJobFailed); err != nil {
			logger.Error(err, "Failed to trigger JobFailed event", "node", nm.node.Name)
		}
		return fmt.Errorf("failed to create shutdown job: %w", err)
	}

	nm.currentJob = job.Name
	nm.jobStartTime = time.Now()

	// The shutdown Job of this operation exists: the input records it, so the report the same pass
	// derives is draining - the machine is up and the pool wants it no longer - and the state the
	// machine derives is shuttingdown, where JobCompleted is reachable.
	nm.input.StartupJobInFlight = false
	nm.input.ShutdownJobInFlight = true

	// One shutdown attempt is one Job: the streak the bound (claim.MaxConsecutiveAttempts) reads is
	// counted here, on the claim that asked for the scale-down, and nowhere else. The finalizer's
	// poll of the observation is read-only, so 45s of polling a machine that stays up is three looks
	// and not three attempts.
	nm.recordShutdownAttempt()

	nm.reportPhase()

	// Start async job monitoring
	go nm.monitorJobCompletion(job.Name)

	logger.Info("Node shutdown initiated", "node", nm.node.Name, "job", job.Name)
	return nil
}

// JobCompleted triggers the JobCompleted event
func (nm *NodeStateMachine) JobCompleted(job *batchv1.Job) error {
	ctx := context.TODO()
	return nm.fsm.Event(ctx, EventJobCompleted, job)
}

// JobFailed triggers the JobFailed event
func (nm *NodeStateMachine) JobFailed() error {
	ctx := context.TODO()
	return nm.fsm.Event(ctx, EventJobFailed)
}

// JobTimeout triggers the JobTimeout event
func (nm *NodeStateMachine) JobTimeout() error {
	ctx := context.TODO()
	return nm.fsm.Event(ctx, EventJobTimeout)
}

// ForceCleanup triggers the ForceCleanup event
func (nm *NodeStateMachine) ForceCleanup() error {
	ctx := context.TODO()
	return nm.fsm.Event(ctx, EventForceCleanup)
}

// CalculateBackoff implements smart backoff strategy based on transition duration
func (nm *NodeStateMachine) CalculateBackoff() time.Duration {
	if nm.backoff == nil {
		return time.Minute // Default backoff
	}

	elapsed := time.Since(nm.backoff.transitionStart)

	switch {
	case elapsed <= 2*time.Minute:
		return 30 * time.Second // Early phase
	case elapsed <= 10*time.Minute:
		return 2 * time.Minute // Normal phase
	case elapsed <= 15*time.Minute:
		return 5 * time.Minute // Late phase
	default:
		// Force cleanup after 15 minutes
		logger := log.Log.WithName("fsm")
		if err := nm.ForceCleanup(); err != nil {
			logger.Error(err, "Failed to force cleanup", "node", nm.node.Name)
		}
		return 0
	}
}

// IsTransitionStuck checks if the current transition has been stuck for too long
func (nm *NodeStateMachine) IsTransitionStuck() bool {
	if nm.backoff == nil {
		return false
	}

	elapsed := time.Since(nm.backoff.transitionStart)
	return elapsed > StuckTransitionThreshold
}

// CheckStuckState checks for stuck transitions and forces cleanup if needed
func (nm *NodeStateMachine) CheckStuckState() {
	if nm.IsTransitionStuck() {
		logger := log.Log.WithName("fsm")
		logger.Info("Detected stuck state transition, forcing cleanup",
			"node", nm.node.Name,
			"state", nm.fsm.Current(),
			"duration", time.Since(nm.backoff.transitionStart))

		if err := nm.ForceCleanup(); err != nil {
			logger.Error(err, "Failed to force cleanup", "node", nm.node.Name)
		}
	}
}

// MonitorJobCompletion monitors a Kubernetes job and triggers appropriate FSM events
func (nm *NodeStateMachine) MonitorJobCompletion(jobName string) {
	nm.monitorJobCompletion(jobName)
}

// monitorJobCompletion is the internal implementation of job monitoring
func (nm *NodeStateMachine) monitorJobCompletion(jobName string) {
	logger := log.Log.WithName("fsm")
	ctx, cancel := context.WithTimeout(context.Background(), nm.jobTimeout)
	defer cancel()

	// Poll job status every 1 second for tests, 30 seconds for production
	pollInterval := JobMonitorPollInterval
	if nm.jobTimeout < 5*time.Second {
		pollInterval = 100 * time.Millisecond // Fast polling for tests
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	logger.Info("Starting job monitoring", "node", nm.node.Name, "job", jobName)

	// Check job status immediately first
	job := &batchv1.Job{}
	err := nm.client.Get(ctx, client.ObjectKey{Name: jobName, Namespace: nm.node.Namespace}, job)
	if err == nil {
		if job.Status.Succeeded > 0 {
			logger.Info("Job completed successfully", "node", nm.node.Name, "job", jobName)
			if err := nm.JobCompleted(job); err != nil {
				logger.Error(err, "Failed to trigger JobCompleted event", "node", nm.node.Name)
			}
			return
		}

		if job.Status.Failed > 0 {
			logger.Info("Job failed", "node", nm.node.Name, "job", jobName)
			nm.recordObservedJobFailure(job)
			if err := nm.JobFailed(); err != nil {
				logger.Error(err, "Failed to trigger JobFailed event", "node", nm.node.Name)
			}
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("Job monitoring nm.uncordonTimeout", "node", nm.node.Name, "job", jobName)
			if err := nm.JobTimeout(); err != nil {
				logger.Error(err, "Failed to trigger JobTimeout event", "node", nm.node.Name)
			}
			return

		case <-ticker.C:
			job := &batchv1.Job{}
			err := nm.client.Get(ctx, client.ObjectKey{Name: jobName, Namespace: nm.node.Namespace}, job)
			if err != nil {
				logger.Error(err, "Failed to get job status", "job", jobName)
				continue
			}

			if job.Status.Succeeded > 0 {
				logger.Info("Job completed successfully", "node", nm.node.Name, "job", jobName)
				if err := nm.JobCompleted(job); err != nil {
					logger.Error(err, "Failed to trigger JobCompleted event", "node", nm.node.Name)
				}
				return
			}

			if job.Status.Failed > 0 {
				logger.Info("Job failed", "node", nm.node.Name, "job", jobName)
				nm.recordObservedJobFailure(job)
				if err := nm.JobFailed(); err != nil {
					logger.Error(err, "Failed to trigger JobFailed event", "node", nm.node.Name)
				}
				return
			}

			logger.V(1).Info("Job still running", "node", nm.node.Name, "job", jobName)
		}
	}
}

// lifecycleObjectMeta builds the ObjectMeta shared by the startup and shutdown Jobs,
// so the two builders (and their pod templates) cannot drift apart.
func (nm *NodeStateMachine) lifecycleObjectMeta(name string, jobPhase string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   nm.node.Namespace,
		Labels:      nm.lifecycleLabels(jobPhase),
		Annotations: nm.lifecycleAnnotations(),
	}
}

// lifecycleLabels returns the label set for a lifecycle Job of the given phase
// (PhaseStartup or PhaseShutdown) and for its pod template.
//
// Provenance is carried by the labels below plus the ownerReferences that
// setOwnerReference() sets: with a live claim the controller ownerReference points at
// the NodeClaim - the Job is the attempt of that claim, and that is what makes Argo CD
// show it under the claim - with the Node CR kept as a secondary owner so the Job is
// also readable under the slot. Do NOT add `argocd.argoproj.io/instance` (or any other
// Argo tracking label) here: these Jobs are created by the controller, they are not
// Argo-managed children, and a tracking label would make Argo claim them and prune them
// for not existing in Git.
func (nm *NodeStateMachine) lifecycleLabels(jobPhase string) map[string]string {
	jobLabels := map[string]string{
		// Keys relied upon by cleanupPendingJobs, the gRPC server and the tests.
		LabelApp:        LifecycleAppName,
		LabelGroup:      nm.node.Labels[LabelGroup],
		LabelNodeLegacy: nm.node.Spec.KubernetesNodeName,
		LabelType:       jobPhase,

		// Standard labels, so the operator's Jobs are identifiable by convention.
		LabelK8sName:      LifecycleAppName,
		LabelK8sComponent: LifecycleComponentName,
		LabelK8sManagedBy: LifecycleAppName,

		// Explicit provenance: the Node CR a human / Argo CD sees is nm.node.Name,
		// which can differ from spec.kubernetesNodeName carried by LabelNodeLegacy.
		LabelNodeCR: nm.node.Name,
		LabelPhase:  jobPhase,
	}

	// The attempt is the claim's, so a Job of an attempt names it. No claim means the
	// shutdown of a machine the pool stopped wanting (law 3): nothing asked for it, so
	// the key is absent, and `kubectl get jobs -l infra.homecluster.dev/nodeclaim`
	// selects exactly the Jobs that have an attempt behind them.
	if nm.claim != nil {
		jobLabels[LabelNodeClaim] = nm.claim.Name
	}

	return jobLabels
}

// lifecycleAnnotations returns the annotations recording which Node CR, and which
// NodeClaim, created the Job.
func (nm *NodeStateMachine) lifecycleAnnotations() map[string]string {
	annotations := map[string]string{
		AnnotationNodeUID:   string(nm.node.UID),
		AnnotationNodeGroup: nm.node.Labels[LabelGroup],
		AnnotationCreatedBy: LifecycleAppName,
	}

	if nm.claim != nil {
		annotations[AnnotationClaimUID] = string(nm.claim.UID)
	}

	return annotations
}

// createStartupJob creates a Kubernetes Job for node startup
func (nm *NodeStateMachine) createStartupJob() (*batchv1.Job, error) {
	job := &batchv1.Job{
		ObjectMeta: nm.lifecycleObjectMeta(
			fmt.Sprintf("%s-startup-%d", nm.node.Name, time.Now().Unix()),
			PhaseStartup,
		),
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				// Keep the pod template labels in sync with the Job's own labels.
				ObjectMeta: metav1.ObjectMeta{
					Labels: nm.lifecycleLabels(PhaseStartup),
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ServiceAccountName: "default",
					Containers: []corev1.Container{
						{
							Name:         "startup",
							Image:        nm.node.Spec.StartupPodSpec.Image,
							Command:      nm.node.Spec.StartupPodSpec.Command,
							Args:         nm.node.Spec.StartupPodSpec.Args,
							VolumeMounts: []corev1.VolumeMount{},
						},
					},
					Volumes: []corev1.Volume{},
				},
			},
		},
	}

	if nm.node.Spec.StartupPodSpec.PriorityClassName != nil {
		job.Spec.Template.Spec.PriorityClassName = *nm.node.Spec.StartupPodSpec.PriorityClassName
	}

	// Set owner reference
	nm.setOwnerReference(job)

	// Apply ServiceAccount if specified
	if nm.node.Spec.StartupPodSpec.ServiceAccount != nil {
		job.Spec.Template.Spec.ServiceAccountName = *nm.node.Spec.StartupPodSpec.ServiceAccount
	}

	// Apply volumes and volume mounts
	startupContainerIndex := 0
	for _, volSpec := range nm.node.Spec.StartupPodSpec.Volumes {
		// Create volume
		vol := corev1.Volume{
			Name: volSpec.Name,
		}

		if volSpec.SecretName != "" {
			vol.VolumeSource = corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: volSpec.SecretName,
				},
			}
		} else if volSpec.ConfigMapName != "" {
			vol.VolumeSource = corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: volSpec.ConfigMapName,
					},
				},
			}
		}

		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, vol)

		// Create volume mount for the container
		volumeMount := corev1.VolumeMount{
			Name:      volSpec.Name,
			MountPath: volSpec.MountPath,
			ReadOnly:  true, // Secret/ConfigMap volumes are read-only
		}

		job.Spec.Template.Spec.Containers[startupContainerIndex].VolumeMounts =
			append(job.Spec.Template.Spec.Containers[startupContainerIndex].VolumeMounts, volumeMount)
	}

	// Apply TTL for automatic cleanup (default 600 seconds if not specified)
	if nm.node.Spec.StartupPodSpec.TTLSecondsAfterFinished != nil {
		job.Spec.TTLSecondsAfterFinished = nm.node.Spec.StartupPodSpec.TTLSecondsAfterFinished
	} else {
		defaultTTL := int32(600)
		job.Spec.TTLSecondsAfterFinished = &defaultTTL
	}

	// Create the job
	if err := nm.client.Create(context.TODO(), job); err != nil {
		return nil, err
	}

	return job, nil
}

// createShutdownJob creates a Kubernetes Job for node shutdown
func (nm *NodeStateMachine) createShutdownJob() (*batchv1.Job, error) {
	job := &batchv1.Job{
		ObjectMeta: nm.lifecycleObjectMeta(
			fmt.Sprintf("%s-shutdown-%d", nm.node.Name, time.Now().Unix()),
			PhaseShutdown,
		),
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				// Keep the pod template labels in sync with the Job's own labels.
				ObjectMeta: metav1.ObjectMeta{
					Labels: nm.lifecycleLabels(PhaseShutdown),
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ServiceAccountName: "default",
					Containers: []corev1.Container{
						{
							Name:         "shutdown",
							Image:        nm.node.Spec.ShutdownPodSpec.Image,
							Command:      nm.node.Spec.ShutdownPodSpec.Command,
							Args:         nm.node.Spec.ShutdownPodSpec.Args,
							VolumeMounts: []corev1.VolumeMount{},
						},
					},
					Volumes: []corev1.Volume{},
				},
			},
		},
	}

	if nm.node.Spec.ShutdownPodSpec.PriorityClassName != nil {
		job.Spec.Template.Spec.PriorityClassName = *nm.node.Spec.ShutdownPodSpec.PriorityClassName
	}

	// Set owner reference
	nm.setOwnerReference(job)

	// Apply ServiceAccount if specified
	if nm.node.Spec.ShutdownPodSpec.ServiceAccount != nil {
		job.Spec.Template.Spec.ServiceAccountName = *nm.node.Spec.ShutdownPodSpec.ServiceAccount
	}

	// Apply volumes and volume mounts
	shutdownContainerIndex := 0
	for _, volSpec := range nm.node.Spec.ShutdownPodSpec.Volumes {
		// Create volume
		vol := corev1.Volume{
			Name: volSpec.Name,
		}

		if volSpec.SecretName != "" {
			vol.VolumeSource = corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: volSpec.SecretName,
				},
			}
		} else if volSpec.ConfigMapName != "" {
			vol.VolumeSource = corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: volSpec.ConfigMapName,
					},
				},
			}
		}

		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, vol)

		// Create volume mount for the container
		volumeMount := corev1.VolumeMount{
			Name:      volSpec.Name,
			MountPath: volSpec.MountPath,
			ReadOnly:  true, // Secret/ConfigMap volumes are read-only
		}

		job.Spec.Template.Spec.Containers[shutdownContainerIndex].VolumeMounts =
			append(job.Spec.Template.Spec.Containers[shutdownContainerIndex].VolumeMounts, volumeMount)
	}

	// Apply TTL for automatic cleanup (default 600 seconds if not specified)
	if nm.node.Spec.ShutdownPodSpec.TTLSecondsAfterFinished != nil {
		job.Spec.TTLSecondsAfterFinished = nm.node.Spec.ShutdownPodSpec.TTLSecondsAfterFinished
	} else {
		defaultTTL := int32(600)
		job.Spec.TTLSecondsAfterFinished = &defaultTTL
	}

	// Create the job
	if err := nm.client.Create(context.TODO(), job); err != nil {
		return nil, err
	}

	return job, nil
}

// setOwnerReference stamps the ownership of a lifecycle Job.
//
// The Job is the attempt of a claim, so with a live claim the NodeClaim is the controller
// owner: the Job hangs under the claim in Argo CD's tree, and the machine work a claim
// asked for is readable on the claim.
//
// The Node CR stays a secondary, non-controller owner, and that is deliberate. The slot is
// the object an operator reads for a machine (`kubectl get jobs -l
// infra.homecluster.dev/node=<slot>`), and Kubernetes collects a dependent only once
// *every* owner is gone: the secondary ref keeps the Job and its provenance on a slot that
// outlives the claim the TTL has collected, and deleting the slot takes the claims and
// their Jobs in one cascade.
//
// The cancelled claim is the ordering that matters. A claim a scale-down marked is a live
// claim, so its shutdown Job is claim-owned from the moment ShutdownNode creates it, and it
// cannot be collected mid-shutdown: the claim's finalizer comes off only once the machine
// is observed off (NodeClaimReconciler.reconcileDeletion), which is the same window in
// which the shutdown has to land. A pending claim has no Job to own - the `pending ->
// booting` write is what creates the startup Job - and a pending Job a crash left behind on
// a slot that is shutting down is cleanupPendingJobs' to delete.
//
// With no live claim - the force-shutdown of law 3, a machine the pool stopped wanting and
// whose claim is already collected - the slot is the only object with an ownership to give,
// and the Job is owned by the Node as controller, exactly as before this change.
func (nm *NodeStateMachine) setOwnerReference(job *batchv1.Job) {
	nodeGVK := infrav2alpha1.GroupVersion.WithKind("Node")

	nodeRef := metav1.OwnerReference{
		APIVersion: nodeGVK.GroupVersion().String(),
		Kind:       nodeGVK.Kind,
		Name:       nm.node.Name,
		UID:        nm.node.UID,
	}

	if nm.claim == nil {
		nodeRef.Controller = &[]bool{true}[0]
		job.SetOwnerReferences([]metav1.OwnerReference{nodeRef})

		return
	}

	claimGVK := infrav2alpha1.GroupVersion.WithKind("NodeClaim")

	job.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: claimGVK.GroupVersion().String(),
			Kind:       claimGVK.Kind,
			Name:       nm.claim.Name,
			UID:        nm.claim.UID,
			// The claim is the owner the attempt answers to, and the deletion of a
			// committed boot is the shutdown this Job runs: the claim is not removed
			// ahead of it.
			Controller:         &[]bool{true}[0],
			BlockOwnerDeletion: &[]bool{true}[0],
		},
		nodeRef,
	})
}

// setNodeUnschedulable cordons the Kubernetes node to prevent new pods from being scheduled
func (nm *NodeStateMachine) setNodeUnschedulable(nodeName string) error {
	logger := log.Log.WithName("fsm").WithValues("node", nm.node.Name, "kubernetesNode", nodeName, "action", "cordon")

	if nodeName == "" {
		logger.V(1).Info("No Kubernetes node name specified, skipping cordon operation")
		return nil
	}

	ctx := context.TODO()

	// Get the Kubernetes node
	kubeNode := &corev1.Node{}
	if err := nm.client.Get(ctx, client.ObjectKey{Name: nodeName}, kubeNode); err != nil {
		logger.Info("Kubernetes node not found, skipping cordon operation", "error", err.Error())
		return nil // Don't fail the operation if the node doesn't exist
	}

	// Check if already unschedulable
	if kubeNode.Spec.Unschedulable {
		logger.V(1).Info("Kubernetes node is already unschedulable")
		return nil
	}

	// Set node as unschedulable
	kubeNode.Spec.Unschedulable = true

	if err := nm.client.Update(ctx, kubeNode); err != nil {
		logger.Error(err, "Failed to cordon Kubernetes node")
		return err
	}

	logger.Info("Successfully cordoned Kubernetes node")
	return nil
}

// setNodeSchedulableInternal performs the actual uncordon operation without retry, and returns the
// node object as the machine left it: the gate's answer is recorded from that object (see
// recordGateAnswer), so what the report of the same pass derives from is the state the uncordon
// wrote, not a read of it from before.
func (nm *NodeStateMachine) setNodeSchedulableInternal(nodeName string) (*corev1.Node, error) {
	logger := log.Log.WithName("fsm").WithValues("node", nm.node.Name, "kubernetesNode", nodeName, "action", "uncordon")

	if nodeName == "" {
		logger.V(1).Info("No Kubernetes node name specified, skipping uncordon operation")
		return nil, nil
	}

	ctx := context.TODO()

	// Get the Kubernetes node
	kubeNode := &corev1.Node{}
	if err := nm.client.Get(ctx, client.ObjectKey{Name: nodeName}, kubeNode); err != nil {
		logger.Info("Kubernetes node not found, skipping uncordon operation", "error", err.Error())
		return nil, nil // Don't fail the operation if the node doesn't exist
	}

	// Check if already schedulable and has no unschedulable taint
	needsUpdate := kubeNode.Spec.Unschedulable
	needsTaintRemoval := false

	for _, taint := range kubeNode.Spec.Taints {
		if taint.Key == "node.kubernetes.io/unschedulable" {
			needsTaintRemoval = true
			break
		}
	}

	if !needsUpdate && !needsTaintRemoval {
		logger.V(1).Info("Kubernetes node is already schedulable")
		return kubeNode, nil
	}

	// Set node as schedulable
	if needsUpdate {
		kubeNode.Spec.Unschedulable = false
	}

	// Remove unschedulable taint if present
	if needsTaintRemoval {
		newTaints := []corev1.Taint{}
		for _, taint := range kubeNode.Spec.Taints {
			if taint.Key != "node.kubernetes.io/unschedulable" {
				newTaints = append(newTaints, taint)
			}
		}

		kubeNode.Spec.Taints = newTaints
		logger.Info("Removed unschedulable taint", "taintKey", "node.kubernetes.io/unschedulable")
	}

	if err := nm.client.Update(ctx, kubeNode); err != nil {
		logger.Error(err, "Failed to uncordon Kubernetes node")
		return nil, err
	}

	logger.Info("Successfully uncordoned Kubernetes node")

	return kubeNode, nil
}

// TaintCiliumAgentNotReady is Cilium's bootstrap taint: its agent adds it to a node that has just
// joined, with a NoExecute effect, and removes it once its datapath is programmed for that node.
// The kubelet marks the node Ready long before that happens, so Ready is not evidence that the
// machine can take work.
//
// It is the documented default of the readiness gate, and the value the chart's Group CRs get by
// CRD default (api/infra/v2alpha1 GroupSpec.NodeReadinessTaints): the key belongs to the CNI of
// the cluster and is not verifiable from this repository, so the gate reads its list from the
// Group and this constant is only the default in the schema. Cilium stamps the key from v1.9
// onwards; the deployment this chart ships runs v1.16.x (Stable channel).
//
// The key is matched on its own, and no taint is ever written or removed here: Cilium owns the
// taint, exactly like taints.IsProtectedKey refuses to let spec.nodeTaints write it.
const TaintCiliumAgentNotReady = taints.CiliumAgentNotReady

// nodeUsability reports whether a node object is Ready and free of every taint the gate waits for,
// and returns the reason it is not, for the retry log and the timeout event. An empty reason means
// the machine can take the pool's pods. keys is the list the slot's Group declares
// (spec.nodeReadinessTaints); an empty list gates on the Ready condition alone, which is the
// behaviour of a CNI that stamps no bootstrap taint.
func nodeUsability(kubeNode *corev1.Node, keys []string) string {
	isReady := false

	for _, cond := range kubeNode.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			isReady = cond.Status == corev1.ConditionTrue

			break
		}
	}

	if !isReady {
		return "node is not Ready"
	}

	for _, taint := range kubeNode.Spec.Taints {
		for _, key := range keys {
			if taint.Key == key {
				return "node carries the readiness taint " + key
			}
		}
	}

	return ""
}

// schedulablePoll is the exponential backoff the uncordon / readiness loop sleeps with: 2s,
// doubling, capped at MaxUncordonRetryInterval. It is a type so every branch of the loop has one
// call (poll.wait()), and so the ceiling stays a single named number that
// claim.AttemptBudget is measured from.
type schedulablePoll struct {
	interval time.Duration
	max      time.Duration
}

func newSchedulablePoll() *schedulablePoll {
	return &schedulablePoll{interval: 2 * time.Second, max: MaxUncordonRetryInterval}
}

// wait sleeps for the current interval, then doubles it for the next pass up to the ceiling.
func (p *schedulablePoll) wait() {
	time.Sleep(p.interval)

	p.interval *= 2
	if p.interval > p.max {
		p.interval = p.max
	}
}

// setNodeSchedulableWithRetry is the scale-up gate: it holds the transition into ready open until
// the machine can take the pool's work, and then uncordons it.
//
// The gate has two halves and each has its own deadline, because they wait for two different things
// and a cold boot spends nearly all of its time in the first one:
//
//   - registration, bounded by nm.uncordonTimeout (DefaultUncordonTimeout, 90s): the time allowed
//     for the corev1.Node of a powered-on machine to appear, which is the kubelet joining;
//   - readiness, bounded by nm.readinessGateTimeout (DefaultReadinessGateTimeout, 4m) counted from
//     the pass on which the node object first appears: the node has to report Ready and be free of
//     every readiness taint the slot's Group declares (spec.nodeReadinessTaints, by default
//     Cilium's node.cilium.io/agent-not-ready, see TaintCiliumAgentNotReady). The CNI programs the
//     datapath minutes after the kubelet checks in and the pool's pods do not tolerate that taint,
//     so a slot declared ready at registration time is read by the autoscaler as capacity its
//     pending pod cannot land on, and the pool buys a second machine for a node that is about to
//     work.
//
// One budget for both halves is the bug this split closes. A machine that takes 80s to register has
// 10s left for a gate that needs minutes, so the gate times out on nearly every boot: the slot is
// declared ready with the bootstrap taint still on the node - the exact symptom the gate was added
// for - and the operator pays a 90s hook and an ERROR line per scale-up that worked.
//
// On every pass, once the node object exists, the taints the slot declares (spec.nodeTaints) are
// stamped before the node is uncordoned, so the machine is tainted before a pod can be bound to it
// (see the loop body for the ordering).
//
// The loop also re-stamps the coordination lock of the slot (see renewOperationLock), on every pass
// of both halves: the gate can outlast the lock's own timeout, and a lock that ages out stops
// protecting the slot it names, which is precisely the window in which the progress watchdog, the
// next acquisition and the expired-lock cleanup all conclude that the operation is over. Renewing
// only in the readiness half is the bug this closes: a startup Job longer than ~3m arrives at the
// gate with the lock already expired, RenewLock will not resurrect an expired lock, and the whole
// 4m gate then runs with nothing protecting the slot.
//
// A timeout does not wedge anything: the loop uncordons the node on the way out, records a Warning
// on the Node CR naming the gate it was waiting on, and returns an error the caller logs and
// continues past (see afterJobCompleted). That is deliberate, twice over. Teardown must not block on
// a readiness gate nothing can answer for, and a node left cordoned is worse than a node that is not
// ready yet: cluster-autoscaler skips cordoned nodes when it picks scale-down candidates, so a
// cordoned machine is never drained, never deleted, and keeps the pool paying for it, while the
// slot's lifecycle operation is over and there is nothing left to run the uncordon.
func (nm *NodeStateMachine) setNodeSchedulableWithRetry(nodeName string) error {
	logger := log.Log.WithName("fsm").WithValues("node", nm.node.Name, "kubernetesNode", nodeName, "action", "uncordon-retry")

	if nodeName == "" {
		logger.V(1).Info("No Kubernetes node name specified, skipping uncordon operation")
		return nil
	}

	start := time.Now()
	poll := newSchedulablePoll()

	// The two deadlines. The gate's starts on the first pass that sees the node object, so the
	// registration wait cannot spend it.
	registrationDeadline := start.Add(nm.uncordonTimeout)
	gateDeadline := start.Add(nm.readinessGateTimeout)
	nodeSeen := false

	// waitingOn records why the last pass refused the node, so a timeout names the gate that was
	// never satisfied instead of leaving the operator to guess.
	waitingOn := "the node object has not appeared"

	for {
		now := time.Now()

		if !nodeSeen && now.After(registrationDeadline) {
			return nm.abandonReadinessGate(logger, nodeName, waitingOn, nm.uncordonTimeout, time.Since(start), false)
		}

		if nodeSeen && now.After(gateDeadline) {
			return nm.abandonReadinessGate(logger, nodeName, waitingOn, nm.readinessGateTimeout, time.Since(start), true)
		}

		// Get current node status
		ctx := context.TODO()
		kubeNode := &corev1.Node{}
		if err := nm.client.Get(ctx, client.ObjectKey{Name: nodeName}, kubeNode); err != nil {
			logger.Info("Node not found yet, retrying", "error", err.Error(), "elapsed", time.Since(start).String())

			// The registration wait keeps the lock too, and this is the half that used to run with
			// none: a startup Job longer than ~3m means the lock - acquired when the transition into
			// startingup ran, with DefaultLockTimeout (5m) - has already aged out by the time the
			// node object appears, and RenewLock deliberately refuses to resurrect an expired lock.
			// Renewing here is what keeps the lock alive across the whole gate: the slot is held for
			// the registration wait as much as for the readiness half, and a watchdog that sees a stale
			// lock settles a slot whose hook is still running.
			nm.renewOperationLock()
			poll.wait()

			continue
		}

		if !nodeSeen {
			nodeSeen = true
			gateDeadline = time.Now().Add(nm.readinessGateTimeout)

			logger.V(1).Info("Kubernetes node registered, opening the readiness gate",
				"registrationElapsed", time.Since(start).String(),
				"gateTimeout", nm.readinessGateTimeout.String())

			// The slot is held for the whole gate: keep the lock that says so alive, from this pass
			// and from every pass that refuses the node below.
			nm.renewOperationLock()
		}

		// Stamp the taints the slot declares (spec.nodeTaints) before this node becomes
		// schedulable. Ordering is the whole point, twice over:
		//   - setNodeSchedulableInternal writes the whole node object it just read, so a taint
		//     applied after it would be wiped by that Update;
		//   - a schedulable, untainted node is bindable by a pod the machine rejects, which is
		//     the window spec.nodeTaints exists to close. A tainting startup Job cannot close
		//     it, the Job finishes after the node joined.
		// Called on every retry: it is idempotent (no write when the node already carries the
		// taints) and the node object may have appeared since the last iteration. A failure here
		// does not stop the uncordon, the next reconcile re-applies it.
		if err := taints.ApplyToKubernetesNode(ctx, nm.client, nm.node, nm.eventRecorder); err != nil {
			logger.Info("Failed to apply declared taints before uncordon", "error", err.Error(), "elapsed", time.Since(start).String())
		}

		// The gate: Ready, and free of every readiness taint the Group declares. Both are required
		// before the node is uncordoned and before the transition is treated as complete, because
		// completing the transition writes status.progress: ready, and the autoscaler reads that as
		// capacity it can place pods on. Declaring it early is what makes the pool buy a second
		// machine for a node that is about to work.
		if reason := nodeUsability(kubeNode, nm.readinessTaintKeys); reason != "" {
			waitingOn = reason

			logger.Info("Node cannot take the pool's pods yet, retrying",
				"reason", reason,
				"elapsed", time.Since(start).String(),
				"gateRemaining", time.Until(gateDeadline).String())

			nm.renewOperationLock()
			poll.wait()

			continue
		}

		// Try to uncordon using internal method (no retry)
		uncordoned, err := nm.setNodeSchedulableInternal(nodeName)
		if err == nil {
			// The gate has answered, and its answer is an input of the report: record it before the
			// caller moves the claim to ready and writes the slot's phase (see recordGateAnswer).
			nm.recordGateAnswer(uncordoned)

			logger.Info("Successfully uncordoned Kubernetes node",
				"elapsed", time.Since(start).String(),
				"gate", "satisfied")

			return nil
		}

		logger.Info("Retrying uncordon operation", "error", err.Error(), "elapsed", time.Since(start).String())
		nm.renewOperationLock()
		poll.wait()
	}
}

// abandonReadinessGate is the way out of the gate when a deadline expires: the operator-visible
// record, a best-effort uncordon, and the error the caller logs and continues past.
//
// gateOpened says which half of the loop ran out of budget: the registration wait (there is no
// node object at all, so nothing to uncordon) or the readiness gate itself.
func (nm *NodeStateMachine) abandonReadinessGate(logger logr.Logger, nodeName, waitingOn string, budget, elapsed time.Duration, gateOpened bool) error {
	gate := "registration"
	if gateOpened {
		gate = "readiness"
	}

	logger.Info("Timed out waiting for the node to become usable, uncordoning it anyway",
		"gate", gate,
		"elapsed", elapsed.String(),
		"waitingOn", waitingOn,
		"timeout", budget.String(),
		"why", "a cordoned node is skipped by cluster-autoscaler's scale-down, and the lifecycle "+
			"operation is over, so nothing would uncordon it again")

	// The event, not only the log. A machine that comes up unable to take the pool's work - or one
	// that never registers at all - is a fact the operator has to be able to see on the slot long
	// after the controller's log line rolled away, and this is the repo's convention for "go look":
	// a Warning on the Node CR, readable with `kubectl describe node.infra.homecluster.dev <name>`.
	nm.recordLifecycleEvent(corev1.EventTypeWarning, ReasonReadinessGateTimeout, ActionUncordon,
		readinessGateNote(nm.node.Name, nodeName, gate, waitingOn, budget))

	if _, err := nm.setNodeSchedulableInternal(nodeName); err != nil {
		logger.Error(err, "Best-effort uncordon after the readiness timeout failed, leaving the node cordoned",
			"elapsed", elapsed.String())
	}

	// The caller logs this and continues: the transition completes, the lock is released, the slot
	// is not left transitional. See afterJobCompleted.
	return fmt.Errorf("timeout after %v: node %s never became usable (%s), uncordoned it anyway",
		budget, nodeName, waitingOn)
}

// readinessGateNote renders the Warning's message: which half of the loop ran out of budget, what
// it was waiting for, and what the controller did about it.
func readinessGateNote(slotName, nodeName, gate, waitingOn string, budget time.Duration) string {
	return fmt.Sprintf(
		"Scale-up of node %s: the %s gate on Kubernetes node %s was not satisfied within %v (%s). "+
			"The node was uncordoned anyway and the slot is recorded ready, because a cordoned machine is invisible to "+
			"cluster-autoscaler's scale-down and nothing would uncordon it again: check the CNI of the pool.",
		slotName, gate, nodeName, budget, waitingOn)
}

// removeClusterAutoscalerTaint removes the ToBeDeletedByClusterAutoscaler taint from the Kubernetes node if present
func (nm *NodeStateMachine) removeClusterAutoscalerTaint(nodeName string) error {
	logger := log.Log.WithName("fsm").WithValues("node", nm.node.Name, "kubernetesNode", nodeName, "action", "removeTaint")

	if nodeName == "" {
		logger.V(1).Info("No Kubernetes node name specified, skipping taint removal operation")
		return nil
	}

	ctx := context.TODO()

	// Get the Kubernetes node
	kubeNode := &corev1.Node{}
	if err := nm.client.Get(ctx, client.ObjectKey{Name: nodeName}, kubeNode); err != nil {
		logger.Info("Kubernetes node not found, skipping taint removal operation", "error", err.Error())
		return nil // Don't fail the operation if the node doesn't exist
	}

	// Check if the taint exists
	taintKey := "ToBeDeletedByClusterAutoscaler"
	taintIndex := -1
	for i, taint := range kubeNode.Spec.Taints {
		if taint.Key == taintKey {
			taintIndex = i
			break
		}
	}

	// If taint doesn't exist, nothing to do
	if taintIndex == -1 {
		logger.V(1).Info("Cluster autoscaler taint not found on node")
		return nil
	}

	// Remove the taint
	kubeNode.Spec.Taints = append(kubeNode.Spec.Taints[:taintIndex], kubeNode.Spec.Taints[taintIndex+1:]...)

	if err := nm.client.Update(ctx, kubeNode); err != nil {
		logger.Error(err, "Failed to remove cluster autoscaler taint from Kubernetes node")
		return err
	}

	logger.Info("Successfully removed cluster autoscaler taint from Kubernetes node")
	return nil
}

// addNodeCondition adds a condition to the node status
func (nm *NodeStateMachine) addNodeCondition(condition metav1.Condition) {
	ctx := context.TODO()

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the node
		fresh := &infrav2alpha1.Node{}
		if err := nm.client.Get(ctx, client.ObjectKeyFromObject(nm.node), fresh); err != nil {
			return err
		}

		// Add condition
		fresh.Status.Conditions = append(fresh.Status.Conditions, condition)

		// Update the node status
		if err := nm.client.Status().Update(ctx, fresh); err != nil {
			return err
		}

		// Update our local copy
		nm.node = fresh
		return nil
	}); err != nil {
		logger := log.Log.WithName("fsm")
		logger.Error(err, "Failed to add node condition", "node", nm.node.Name, "condition", condition.Type)
	}
}

// cleanupPendingJobs finds and deletes any existing startup or shutdown jobs for the node
func (nm *NodeStateMachine) cleanupPendingJobs(nodeKubernetesName string) error {
	logger := log.Log.WithName("fsm").WithValues("node", nm.node.Name, "kubernetesNode", nodeKubernetesName, "action", "cleanup")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if nodeKubernetesName == "" {
		logger.V(1).Info("No Kubernetes node name specified, skipping job cleanup")
		return nil
	}

	// Create label selector for jobs belonging to this node
	labelSelector := labels.SelectorFromSet(labels.Set{
		LabelNodeLegacy: nodeKubernetesName,
	})

	// List all jobs with the node label
	jobList := &batchv1.JobList{}
	listOpts := &client.ListOptions{
		Namespace:     nm.node.Namespace,
		LabelSelector: labelSelector,
	}

	if err := nm.client.List(ctx, jobList, listOpts); err != nil {
		logger.Error(err, "Failed to list jobs for cleanup")
		return fmt.Errorf("failed to list jobs: %w", err)
	}

	// Delete pending startup/shutdown jobs to ensure clean state
	for _, job := range jobList.Items {
		// Check if this is a startup or shutdown job
		jobType, exists := job.Labels[LabelType]
		if !exists || (jobType != PhaseStartup && jobType != PhaseShutdown) {
			continue
		}

		// Check if job is still pending or running (not completed)
		if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
			// Delete pending job to ensure clean state
			if err := nm.client.Delete(ctx, &job); err != nil {
				logger.Error(err, "Failed to delete pending job", "job", job.Name)
				continue
			}
			logger.Info("Deleted pending job", "job", job.Name, "type", jobType)
		} else {
			logger.V(1).Info("Skipping completed job", "job", job.Name, "type", jobType, "succeeded", job.Status.Succeeded, "failed", job.Status.Failed)
		}
	}

	logger.Info("Job cleanup completed")

	return nil
}
