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
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
)

// NodeReconciler reconciles a Node object
type NodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.homecluster.dev,resources=nodes/finalizers,verbs=update

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the Node resource from the request
	node := &infrav1alpha1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// The object was not found, return early
		return ctrl.Result{}, err
	}

	if node.Labels["group"] != "" || node.Labels["infra.homecluster.dev/group"] != node.Labels["group"] {
		r.addLabelToKubernetesNodeWithPatch(ctx, node.Spec.KubernetesNodeName, "infra.homecluster.dev/group", node.Labels["group"])
		r.addLabelToKubernetesNodeWithPatch(ctx, node.Spec.KubernetesNodeName, "infra.homecluster.dev/node", node.Name)
	}
	// Check if the Node has a group label
	groupName, hasGroupLabel := node.Labels["group"]
	if !hasGroupLabel {
		logger.Info("Node does not have a group label, skipping owner reference management", "node", node.Name)
		return ctrl.Result{}, fmt.Errorf("Node %s has no group label, skipping", node.Name)
	}

	// Get the Group CR referenced by the label
	group := &infrav1alpha1.Group{}
	if err := r.Get(ctx, types.NamespacedName{Name: groupName, Namespace: node.Namespace}, group); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Group referenced by node label not found", "group", groupName, "node", node.Name)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Group", "group", groupName)
		return ctrl.Result{}, err
	}

	r.SetControllerReference(ctx, node, group)

	if !node.DeletionTimestamp.IsZero() {
		return r.handleNodeDeletion(ctx, node, groupName, req.Name)
	}

	// Validate state before attempting transitions
	if err := r.validateStateTransition(ctx, node); err != nil {
		logger.Error(err, "Invalid state transition", "node", node.Name)
		return ctrl.Result{RequeueAfter: time.Minute}, nil // Requeue to retry later
	}

	// Handle power state transitions with proper logic
	if node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOn &&
		(node.Status.PowerState == infrav1alpha1.PowerStateOff || node.Status.PowerState == "") &&
		node.Status.Progress != infrav1alpha1.ProgressStartingUp {

		err := r.startNode(ctx, node)
		if err != nil {
			logger.Error(err, "Failed to start Node", "node", node.Name)
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

	} else if node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOff &&
		node.Status.PowerState == infrav1alpha1.PowerStateOn &&
		node.Status.Progress != infrav1alpha1.ProgressShuttingDown {

		err := r.shutdownNode(ctx, node)
		if err != nil {
			logger.Error(err, "Failed to shutdown Node", "node", node.Name)
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
	} else {
		// Node is already in desired state or transitioning
		logger.Info("Node is in desired state or transitioning",
			"node", node.Name,
			"desiredState", node.Spec.DesiredPowerState,
			"currentState", node.Status.PowerState,
			"progress", node.Status.Progress)
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil // Check again later
	}

	logger.Info("Successfully reconciled Node", "node", node.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.Node{}).
		Named("node").
		Complete(r)
}

// validateKubernetesNode validates that the referenced Kubernetes node exists and is schedulable
func (r *NodeReconciler) validateKubernetesNode(ctx context.Context, node *infrav1alpha1.Node, nodeName, kubernetesNodeName string) (*corev1.Node, error) {
	logger := log.FromContext(ctx)

	// Get the referenced Kubernetes node
	kubernetesNode := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: kubernetesNodeName}, kubernetesNode); err != nil {
		if errors.IsNotFound(err) {
			logger.Error(err, "Referenced Kubernetes node does not exist",
				"node", nodeName, "kubernetesNodeName", kubernetesNodeName)
			return nil, fmt.Errorf("referenced Kubernetes node %q does not exist", kubernetesNodeName)
		}
		logger.Error(err, "Failed to get referenced Kubernetes node",
			"node", nodeName, "kubernetesNodeName", kubernetesNodeName)
		return nil, err
	}

	logger.Info("Successfully validated referenced Kubernetes node",
		"node", nodeName, "kubernetesNodeName", kubernetesNodeName)
	return kubernetesNode, nil
}

// handleNodeDeletion handles the deletion of a node
func (r *NodeReconciler) handleNodeDeletion(ctx context.Context, node *infrav1alpha1.Node, groupName, nodeName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Node is being deleted", "node", nodeName)

	return ctrl.Result{}, nil
}

func (r *NodeReconciler) addLabelToKubernetesNodeWithPatch(ctx context.Context, nodeName, labelKey, labelValue string) error {
	logger := log.FromContext(ctx)

	// Get the Kubernetes node
	kubernetesNode := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, kubernetesNode); err != nil {
		logger.Error(err, "Failed to get Kubernetes node", "node", nodeName)
		return err
	}

	// Create a patch with the new label
	patch := client.MergeFrom(kubernetesNode.DeepCopy())

	// Initialize the Labels map if it's nil
	if kubernetesNode.Labels == nil {
		kubernetesNode.Labels = make(map[string]string)
	}

	// Add the label
	kubernetesNode.Labels[labelKey] = labelValue

	// Apply the patch
	if err := r.Patch(ctx, kubernetesNode, patch); err != nil {
		logger.Error(err, "Failed to patch Kubernetes node with label", "node", nodeName, "label", labelKey)
		return err
	}

	logger.Info("Successfully patched Kubernetes node with label", "node", nodeName, "label", labelKey, "value", labelValue)
	return nil
}

func (r *NodeReconciler) shutdownNode(ctx context.Context, nodeCR *infrav1alpha1.Node) error {
	logger := log.FromContext(ctx)

	// Mark the Kubernetes node as not schedulable before starting shutdown
	k8sNode := &corev1.Node{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: nodeCR.Spec.KubernetesNodeName}, k8sNode); err != nil {
		logger.Error(err, "failed to get Kubernetes node", "nodeName", nodeCR.Spec.KubernetesNodeName)
		return err
	}

	// Mark the node as unschedulable
	k8sNode.Spec.Unschedulable = true
	if err := r.Client.Update(ctx, k8sNode); err != nil {
		logger.Error(err, "failed to mark Kubernetes node as unschedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)
		return err
	}

	logger.Info("Successfully marked Kubernetes node as unschedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)

	// Drain the node by evicting all pods
	if err := r.drainNode(ctx, nodeCR.Spec.KubernetesNodeName); err != nil {
		logger.Error(err, "failed to drain node", "nodeName", nodeCR.Spec.KubernetesNodeName)
		return err
	}

	logger.Info("Successfully drained node", "nodeName", nodeCR.Spec.KubernetesNodeName)

	// Create a Job to shutdown the node
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-shutdown-%s", nodeCR.Name, fmt.Sprintf("%d", metav1.Now().Unix())),
			Namespace: nodeCR.Namespace,
			Labels: map[string]string{
				"app":   "homelab-autoscaler",
				"group": nodeCR.Labels["group"],
				"node":  nodeCR.Spec.KubernetesNodeName,
				"type":  "shutdown",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "homelab-autoscaler",
						"group": nodeCR.Labels["group"],
						"node":  nodeCR.Spec.KubernetesNodeName,
						"type":  "shutdown",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "shutdown",
							Image:   nodeCR.Spec.ShutdownPodSpec.Image,
							Command: nodeCR.Spec.ShutdownPodSpec.Command,
							Args:    nodeCR.Spec.ShutdownPodSpec.Args,
						},
					},
				},
			},
		},
	}

	// Create the Job
	if err := r.Client.Create(ctx, job); err != nil {
		return err
	}

	// Set owner reference to the Node CR
	if err := ctrl.SetControllerReference(nodeCR, job, r.Scheme); err != nil {
		return err
	}

	// Update the Node status condition to "progressing"
	nodeCR.Status.Conditions = append(nodeCR.Status.Conditions, metav1.Condition{
		Type:               "progressing",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeGroupDecreasingSize",
		Message:            fmt.Sprintf("Shutting down node via job %s", job.Name),
	},
	)
	nodeCR.Status.LastStartupTime = &metav1.Time{Time: time.Now().UTC()}
	nodeCR.Status.Progress = infrav1alpha1.ProgressShuttingDown
	// Update the Node with the new status info
	if err := r.Update(ctx, nodeCR); err != nil {
		logger.Error(err, "Failed to update Node with status condition", "node", nodeCR.Name)
		return err
	}

	logger.Info("Successfully created shutdown job", "job", job.Name, "node", nodeCR.Spec.KubernetesNodeName)

	return nil
}

func (r *NodeReconciler) startNode(ctx context.Context, nodeCR *infrav1alpha1.Node) error {
	logger := log.FromContext(ctx)

	// Get the Kubernetes node object and set it as schedulable
	k8sNode := &corev1.Node{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: nodeCR.Spec.KubernetesNodeName}, k8sNode); err != nil {
		logger.Error(err, "failed to get Kubernetes node", "nodeName", nodeCR.Spec.KubernetesNodeName)
		return err
	}

	// Set the node as schedulable
	k8sNode.Spec.Unschedulable = false
	if err := r.Client.Update(ctx, k8sNode); err != nil {
		logger.Error(err, "failed to mark Kubernetes node as schedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)
		return err
	}

	logger.Info("Successfully marked Kubernetes node as schedulable", "nodeName", nodeCR.Spec.KubernetesNodeName)

	// Create a Job to start the unhealthy node
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-startup-%s", nodeCR.Name, fmt.Sprintf("%d", metav1.Now().Unix())),
			Namespace: nodeCR.Namespace,
			Labels: map[string]string{
				"app":   "homelab-autoscaler",
				"group": nodeCR.Labels["group"],
				"node":  nodeCR.Spec.KubernetesNodeName,
				"type":  "startup",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "homelab-autoscaler",
						"group": nodeCR.Labels["group"],
						"node":  nodeCR.Spec.KubernetesNodeName,
						"type":  "startup",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "startup",
							Image:   nodeCR.Spec.StartupPodSpec.Image,
							Command: nodeCR.Spec.StartupPodSpec.Command,
							Args:    nodeCR.Spec.StartupPodSpec.Args,
						},
					},
				},
			},
		},
	}

	// Set owner reference to the Node CR
	if err := ctrl.SetControllerReference(nodeCR, job, r.Scheme); err != nil {
		return err
	}

	// Create the Job
	if err := r.Client.Create(ctx, job); err != nil {
		return err
	}

	logger.Info("Successfully created startup job", "job", job.Name, "node", nodeCR)

	// Update the Node status condition to "progressing"
	nodeCR.Status.Conditions = append(nodeCR.Status.Conditions, metav1.Condition{
		Type:               "progressing",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeGroupIncreaseSize",
		Message:            fmt.Sprintf("Starting node via job %s", job.Name),
	},
	)
	nodeCR.Status.LastStartupTime = &metav1.Time{Time: time.Now().UTC()}
	nodeCR.Status.Progress = infrav1alpha1.ProgressStartingUp
	// Update the Node with the new status info
	if err := r.Update(ctx, nodeCR); err != nil {
		logger.Error(err, "Failed to update Node with status condition", "node", nodeCR.Name)
		return err
	}

	return nil

}

// validateStateTransition validates that a state transition is valid
func (r *NodeReconciler) validateStateTransition(ctx context.Context, node *infrav1alpha1.Node) error {
	logger := log.FromContext(ctx)

	// Check for context cancellation
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Validate that we have required fields
	if node.Spec.DesiredPowerState == "" {
		return fmt.Errorf("node %s has empty DesiredPowerState", node.Name)
	}

	// Validate that DesiredPowerState is a valid value
	if node.Spec.DesiredPowerState != infrav1alpha1.PowerStateOn &&
		node.Spec.DesiredPowerState != infrav1alpha1.PowerStateOff {
		return fmt.Errorf("node %s has invalid DesiredPowerState: %s", node.Name, node.Spec.DesiredPowerState)
	}

	// Check if node is already in a transitional state
	if node.Status.Progress == infrav1alpha1.ProgressStartingUp ||
		node.Status.Progress == infrav1alpha1.ProgressShuttingDown {
		logger.Info("Node is already transitioning",
			"node", node.Name,
			"progress", node.Status.Progress)
		return fmt.Errorf("node %s is already transitioning with progress: %s", node.Name, node.Status.Progress)
	}

	return nil

}

// drainNode drains all pods from the specified node using the Kubernetes eviction API
func (r *NodeReconciler) drainNode(ctx context.Context, nodeName string) error {
	logger := log.FromContext(ctx)
	
	// Set a timeout for the entire drain operation (5 minutes)
	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Get all pods running on the node
	podList := &corev1.PodList{}
	if err := r.Client.List(drainCtx, podList); err != nil {
		logger.Error(err, "failed to list pods", "nodeName", nodeName)
		return err
	}

	// Filter pods by node name
	var nodePodsItems []corev1.Pod
	for _, pod := range podList.Items {
		if pod.Spec.NodeName == nodeName {
			nodePodsItems = append(nodePodsItems, pod)
		}
	}
	podList.Items = nodePodsItems

	logger.Info("Found pods to evict", "nodeName", nodeName, "podCount", len(podList.Items))

	// Track pods that need eviction
	var podsToEvict []corev1.Pod
	for _, pod := range podList.Items {
		// Skip pods that are already terminating
		if pod.DeletionTimestamp != nil {
			continue
		}

		// Skip DaemonSet pods (they shouldn't be evicted)
		if r.isDaemonSetPod(&pod) {
			logger.Info("Skipping DaemonSet pod", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}

		// Skip static pods (managed by kubelet)
		if r.isStaticPod(&pod) {
			logger.Info("Skipping static pod", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}

		// Skip pods in kube-system namespace that are critical
		if r.isCriticalSystemPod(&pod) {
			logger.Info("Skipping critical system pod", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}

		podsToEvict = append(podsToEvict, pod)
	}

	if len(podsToEvict) == 0 {
		logger.Info("No pods to evict", "nodeName", nodeName)
		return nil
	}

	// Evict pods using the eviction API
	for _, pod := range podsToEvict {
		if err := r.evictPod(drainCtx, &pod); err != nil {
			logger.Error(err, "failed to evict pod", "pod", pod.Name, "namespace", pod.Namespace)
			// Continue with other pods even if one fails
		} else {
			logger.Info("Successfully evicted pod", "pod", pod.Name, "namespace", pod.Namespace)
		}
	}

	// Wait for pods to be terminated (with timeout)
	return r.waitForPodsToTerminate(drainCtx, nodeName, podsToEvict)
}

// evictPod evicts a single pod using the Kubernetes eviction API
func (r *NodeReconciler) evictPod(ctx context.Context, pod *corev1.Pod) error {
	eviction := &policyv1beta1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}

	// Create the eviction (this is a special subresource)
	return r.Client.SubResource("eviction").Create(ctx, pod, eviction)
}

// waitForPodsToTerminate waits for evicted pods to be terminated
func (r *NodeReconciler) waitForPodsToTerminate(ctx context.Context, nodeName string, evictedPods []corev1.Pod) error {
	logger := log.FromContext(ctx)
	
	// Create a map for quick lookup
	evictedPodMap := make(map[string]bool)
	for _, pod := range evictedPods {
		evictedPodMap[pod.Namespace+"/"+pod.Name] = true
	}

	// Poll every 10 seconds to check if pods are terminated
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Timeout waiting for pods to terminate", "nodeName", nodeName)
			return ctx.Err()
		case <-ticker.C:
			// Check if any evicted pods are still running
			podList := &corev1.PodList{}
			if err := r.Client.List(ctx, podList); err != nil {
				logger.Error(err, "failed to list pods while waiting for termination", "nodeName", nodeName)
				continue
			}

			// Filter pods by node name
			var nodePodsItems []corev1.Pod
			for _, pod := range podList.Items {
				if pod.Spec.NodeName == nodeName {
					nodePodsItems = append(nodePodsItems, pod)
				}
			}

			stillRunning := 0
			for _, pod := range nodePodsItems {
				podKey := pod.Namespace + "/" + pod.Name
				if evictedPodMap[podKey] && pod.DeletionTimestamp == nil {
					stillRunning++
				}
			}

			if stillRunning == 0 {
				logger.Info("All evicted pods have terminated", "nodeName", nodeName)
				return nil
			}

			logger.Info("Waiting for pods to terminate", "nodeName", nodeName, "stillRunning", stillRunning)
		}
	}
}

// isDaemonSetPod checks if a pod is managed by a DaemonSet
func (r *NodeReconciler) isDaemonSetPod(pod *corev1.Pod) bool {
	for _, ownerRef := range pod.OwnerReferences {
		if ownerRef.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// isStaticPod checks if a pod is a static pod (managed by kubelet)
func (r *NodeReconciler) isStaticPod(pod *corev1.Pod) bool {
	// Static pods have a specific annotation
	if _, exists := pod.Annotations["kubernetes.io/config.source"]; exists {
		return true
	}
	// Also check for mirror pod annotation
	if _, exists := pod.Annotations["kubernetes.io/config.mirror"]; exists {
		return true
	}
	return false
}

// isCriticalSystemPod checks if a pod is a critical system pod that shouldn't be evicted
func (r *NodeReconciler) isCriticalSystemPod(pod *corev1.Pod) bool {
	// Skip pods in kube-system namespace that are critical
	if pod.Namespace == "kube-system" {
		// List of critical system components that shouldn't be evicted
		criticalComponents := []string{
			"kube-proxy",
			"kube-dns",
			"coredns",
			"calico-node",
			"flannel",
			"weave-net",
		}
		
		for _, component := range criticalComponents {
			if pod.Name == component ||
			   (len(pod.Labels) > 0 && pod.Labels["k8s-app"] == component) ||
			   (len(pod.Labels) > 0 && pod.Labels["app"] == component) {
				return true
			}
		}
	}
	return false
}
func (r *NodeReconciler) SetControllerReference(ctx context.Context, node *infrav1alpha1.Node, group *infrav1alpha1.Group) error {
	logger := log.FromContext(ctx)

	// Check if the Node already has an owner reference to this Group
	for _, ownerRef := range node.OwnerReferences {
		if ownerRef.UID == group.UID && ownerRef.Kind == "Group" && ownerRef.APIVersion == "infra.homecluster.dev/v1alpha1" {
			return nil
		}
	}

	// Set the owner reference using controllerutil.SetControllerReference
	if err := ctrl.SetControllerReference(group, node, r.Scheme); err != nil {
		logger.Error(err, "Failed to set controller reference", "node", node.Name, "group", group.Name)
		return err
	}

	// Update the Node with the new owner reference
	if err := r.Update(ctx, node); err != nil {
		logger.Error(err, "Failed to update Node with owner reference", "node", node.Name)
		return err
	}

	logger.Info("Successfully set owner reference for Node to Group", "node", node.Name, "group", group.Name)

	return nil

}
