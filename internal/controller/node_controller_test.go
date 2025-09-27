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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrahomeclusterdevv1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
)

var _ = Describe("Node Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		node := &infrahomeclusterdevv1alpha1.Node{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Node")
			err := k8sClient.Get(ctx, typeNamespacedName, node)
			if err != nil && errors.IsNotFound(err) {
				resource := &infrahomeclusterdevv1alpha1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					// TODO(user): Specify other spec details if needed.
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &infrahomeclusterdevv1alpha1.Node{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Node")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &NodeReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})

	Context("When updating Node conditions", func() {
		ctx := context.Background()
		var groupStore *groupstore.GroupStore
		var resourceName string
		var groupName string
		var nodeName string

		BeforeEach(func() {
			groupStore = groupstore.NewGroupStore()
			// Use unique names for each test to avoid conflicts
			resourceName = fmt.Sprintf("test-node-conditions-%d", time.Now().UnixNano())
			groupName = fmt.Sprintf("test-group-conditions-%d", time.Now().UnixNano())
			nodeName = fmt.Sprintf("test-k8s-node-conditions-%d", time.Now().UnixNano())

			// Create the test group first
			testGroup := &infrahomeclusterdevv1alpha1.Group{
				ObjectMeta: metav1.ObjectMeta{
					Name:      groupName,
					Namespace: "homelab-autoscaler-system",
				},
				Spec: infrahomeclusterdevv1alpha1.GroupSpec{
					Name:         groupName,
					MaxSize:      5,
					NodeSelector: map[string]string{"group": groupName},
					Pricing: infrahomeclusterdevv1alpha1.PricingSpec{
						MonthlyRate: "100.0",
						HourlyRate:  "0.1",
					},
				},
			}
			Expect(k8sClient.Create(ctx, testGroup)).To(Succeed())
		})

		AfterEach(func() {
			// Clean up the group
			testGroup := &infrahomeclusterdevv1alpha1.Group{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: groupName, Namespace: "homelab-autoscaler-system"}, testGroup)
			if err == nil {
				_ = k8sClient.Delete(ctx, testGroup)
			}
		})

		It("should set Node condition to 'progressing' when scaling", func() {
			By("Creating the referenced Kubernetes node")
			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: corev1.NodeSpec{
					Unschedulable: false,
				},
			}
			Expect(k8sClient.Create(ctx, k8sNode)).To(Succeed())

			By("Creating a Node resource")
			node := &infrahomeclusterdevv1alpha1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "homelab-autoscaler-system",
					Labels: map[string]string{
						"group": groupName,
					},
				},
				Spec: infrahomeclusterdevv1alpha1.NodeSpec{
					KubernetesNodeName: nodeName,
					HealthcheckPeriod:  60,
					StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "startup"},
					},
					ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "shutdown"},
					},
					HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "healthcheck"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			By("Creating a NodeReconciler with GroupStore")
			controllerReconciler := &NodeReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupStore,
			}

			By("Simulating a scaling operation by setting node health to 'unknown'")
			groupStore.SetNodeHealthcheckStatus(groupName, nodeName, "unknown")

			By("Reconciling the Node")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Node condition is set to 'progressing'")
			updatedNode := &infrahomeclusterdevv1alpha1.Node{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"}, updatedNode)
			Expect(err).NotTo(HaveOccurred())

			progressingCondition := getNodeCondition(updatedNode.Status.Conditions, "Progressing")
			Expect(progressingCondition).NotTo(BeNil())
			Expect(progressingCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(progressingCondition.Reason).To(Equal("NodeScaling"))
			Expect(progressingCondition.Message).To(ContainSubstring("Node is being scaled"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, k8sNode)).To(Succeed())
		})

		It("should set Node condition to 'available' when healthcheck status is 'healthy'", func() {
			By("Creating the referenced Kubernetes node")
			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: corev1.NodeSpec{
					Unschedulable: false,
				},
			}
			Expect(k8sClient.Create(ctx, k8sNode)).To(Succeed())

			By("Creating a Node resource")
			node := &infrahomeclusterdevv1alpha1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "homelab-autoscaler-system",
					Labels: map[string]string{
						"group": groupName,
					},
				},
				Spec: infrahomeclusterdevv1alpha1.NodeSpec{
					KubernetesNodeName: nodeName,
					HealthcheckPeriod:  60,
					StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "startup"},
					},
					ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "shutdown"},
					},
					HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "healthcheck"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			By("Creating a NodeReconciler with GroupStore")
			controllerReconciler := &NodeReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupStore,
			}

			By("Simulating a healthy node by setting node health to 'healthy'")
			groupStore.SetNodeHealthcheckStatus(groupName, nodeName, "healthy")

			By("Reconciling the Node")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Node condition is set to 'available'")
			updatedNode := &infrahomeclusterdevv1alpha1.Node{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"}, updatedNode)
			Expect(err).NotTo(HaveOccurred())

			availableCondition := getNodeCondition(updatedNode.Status.Conditions, "Available")
			Expect(availableCondition).NotTo(BeNil())
			Expect(availableCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(availableCondition.Reason).To(Equal("NodeHealthy"))
			Expect(availableCondition.Message).To(ContainSubstring("Node is healthy and available"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, k8sNode)).To(Succeed())
		})

		It("should handle multiple condition updates correctly", func() {
			By("Creating the referenced Kubernetes node")
			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: corev1.NodeSpec{
					Unschedulable: false,
				},
			}
			Expect(k8sClient.Create(ctx, k8sNode)).To(Succeed())

			By("Creating a Node resource")
			node := &infrahomeclusterdevv1alpha1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "homelab-autoscaler-system",
					Labels: map[string]string{
						"group": groupName,
					},
				},
				Spec: infrahomeclusterdevv1alpha1.NodeSpec{
					KubernetesNodeName: nodeName,
					HealthcheckPeriod:  60,
					StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "startup"},
					},
					ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "shutdown"},
					},
					HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "healthcheck"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			By("Creating a NodeReconciler with GroupStore")
			controllerReconciler := &NodeReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupStore,
			}

			By("First reconciliation with unknown health (scaling)")
			groupStore.SetNodeHealthcheckStatus(groupName, nodeName, "unknown")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying 'progressing' condition is set")
			updatedNode := &infrahomeclusterdevv1alpha1.Node{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"}, updatedNode)
			Expect(err).NotTo(HaveOccurred())

			progressingCondition := getNodeCondition(updatedNode.Status.Conditions, "Progressing")
			Expect(progressingCondition).NotTo(BeNil())
			Expect(progressingCondition.Status).To(Equal(metav1.ConditionTrue))

			By("Second reconciliation with healthy health")
			groupStore.SetNodeHealthcheckStatus(groupName, nodeName, "healthy")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying 'available' condition is set and 'progressing' is updated")
			err = k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"}, updatedNode)
			Expect(err).NotTo(HaveOccurred())

			availableCondition := getNodeCondition(updatedNode.Status.Conditions, "Available")
			Expect(availableCondition).NotTo(BeNil())
			Expect(availableCondition.Status).To(Equal(metav1.ConditionTrue))

			progressingCondition = getNodeCondition(updatedNode.Status.Conditions, "Progressing")
			Expect(progressingCondition).NotTo(BeNil())
			Expect(progressingCondition.Status).To(Equal(metav1.ConditionFalse))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, k8sNode)).To(Succeed())
		})

		It("should handle offline health status correctly", func() {
			By("Creating the referenced Kubernetes node")
			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: corev1.NodeSpec{
					Unschedulable: false,
				},
			}
			Expect(k8sClient.Create(ctx, k8sNode)).To(Succeed())

			By("Creating a Node resource")
			node := &infrahomeclusterdevv1alpha1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "homelab-autoscaler-system",
					Labels: map[string]string{
						"group": groupName,
					},
				},
				Spec: infrahomeclusterdevv1alpha1.NodeSpec{
					KubernetesNodeName: nodeName,
					HealthcheckPeriod:  60,
					StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "startup"},
					},
					ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "shutdown"},
					},
					HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "healthcheck"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			By("Creating a NodeReconciler with GroupStore")
			controllerReconciler := &NodeReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupStore,
			}

			By("Simulating an offline node")
			groupStore.SetNodeHealthcheckStatus(groupName, nodeName, "offline")

			By("Reconciling the Node")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Node conditions for offline status")
			updatedNode := &infrahomeclusterdevv1alpha1.Node{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"}, updatedNode)
			Expect(err).NotTo(HaveOccurred())

			// Should not have available condition when offline
			availableCondition := getNodeCondition(updatedNode.Status.Conditions, "Available")
			if availableCondition != nil {
				Expect(availableCondition.Status).To(Equal(metav1.ConditionFalse))
			}

			// Should not have progressing condition when offline
			progressingCondition := getNodeCondition(updatedNode.Status.Conditions, "Progressing")
			if progressingCondition != nil {
				Expect(progressingCondition.Status).To(Equal(metav1.ConditionFalse))
			}

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, k8sNode)).To(Succeed())
		})

		It("should handle SetNodeConditionProgressingForScaling method", func() {
			By("Creating the referenced Kubernetes node")
			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: corev1.NodeSpec{
					Unschedulable: false,
				},
			}
			Expect(k8sClient.Create(ctx, k8sNode)).To(Succeed())

			By("Creating a Node resource")
			node := &infrahomeclusterdevv1alpha1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "homelab-autoscaler-system",
					Labels: map[string]string{
						"group": groupName,
					},
				},
				Spec: infrahomeclusterdevv1alpha1.NodeSpec{
					KubernetesNodeName: nodeName,
					HealthcheckPeriod:  60,
					StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "startup"},
					},
					ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "shutdown"},
					},
					HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "healthcheck"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			By("Creating a NodeReconciler with GroupStore")
			controllerReconciler := &NodeReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupStore,
			}

			By("Calling SetNodeConditionProgressingForScaling")
			err := controllerReconciler.SetNodeConditionProgressingForScaling(ctx, resourceName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Node condition is set to 'progressing'")
			updatedNode := &infrahomeclusterdevv1alpha1.Node{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"}, updatedNode)
			Expect(err).NotTo(HaveOccurred())

			progressingCondition := getNodeCondition(updatedNode.Status.Conditions, "Progressing")
			Expect(progressingCondition).NotTo(BeNil())
			Expect(progressingCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(progressingCondition.Reason).To(Equal("ScalingInitiated"))
			Expect(progressingCondition.Message).To(ContainSubstring("Node scaling operation initiated"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, k8sNode)).To(Succeed())
		})

		It("should handle status-only update detection", func() {
			By("Creating the referenced Kubernetes node")
			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: corev1.NodeSpec{
					Unschedulable: false,
				},
			}
			Expect(k8sClient.Create(ctx, k8sNode)).To(Succeed())

			By("Creating a Node resource")
			node := &infrahomeclusterdevv1alpha1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "homelab-autoscaler-system",
					Labels: map[string]string{
						"group": groupName,
					},
				},
				Spec: infrahomeclusterdevv1alpha1.NodeSpec{
					KubernetesNodeName: nodeName,
					HealthcheckPeriod:  60,
					StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "startup"},
					},
					ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "shutdown"},
					},
					HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "healthcheck"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			By("Creating a NodeReconciler with GroupStore")
			controllerReconciler := &NodeReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupStore,
			}

			By("First reconciliation to add node to GroupStore")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Updating only the status (simulating status-only update)")
			updatedNode := &infrahomeclusterdevv1alpha1.Node{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"}, updatedNode)
			Expect(err).NotTo(HaveOccurred())

			// Update status but not spec (Generation should remain the same)
			updatedNode.Status.Health = "healthy"
			err = k8sClient.Status().Update(ctx, updatedNode)
			Expect(err).NotTo(HaveOccurred())

			By("Second reconciliation should be skipped due to status-only update")
			// This should be skipped due to status-only update detection
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, k8sNode)).To(Succeed())
		})

		Context("When handling shutdown functionality", func() {
			var (
				shutdownNodeName    string
				shutdownGroupName   string
				shutdownK8sNodeName string
				groupStore          *groupstore.GroupStore
			)

			BeforeEach(func() {
				// Use unique names for each test to avoid conflicts
				// Keep names short to avoid CronJob name length issues (max 52 characters)
				shutdownNodeName = fmt.Sprintf("node-shutdown-%d", time.Now().UnixNano()/1000000) // Remove nanoseconds to shorten
				shutdownGroupName = fmt.Sprintf("group-shutdown-%d", time.Now().UnixNano()/1000000)
				shutdownK8sNodeName = fmt.Sprintf("k8s-node-shutdown-%d", time.Now().UnixNano()/1000000)

				// Create a fresh GroupStore for each test
				groupStore = groupstore.NewGroupStore()

				// Create the test group first
				testGroup := &infrahomeclusterdevv1alpha1.Group{
					ObjectMeta: metav1.ObjectMeta{
						Name:      shutdownGroupName,
						Namespace: "homelab-autoscaler-system",
					},
					Spec: infrahomeclusterdevv1alpha1.GroupSpec{
						Name:         shutdownGroupName,
						MaxSize:      5,
						NodeSelector: map[string]string{"group": shutdownGroupName},
						Pricing: infrahomeclusterdevv1alpha1.PricingSpec{
							MonthlyRate: "100.0",
							HourlyRate:  "0.1",
						},
					},
				}
				Expect(k8sClient.Create(ctx, testGroup)).To(Succeed())
			})

			AfterEach(func() {
				// Clean up the group
				testGroup := &infrahomeclusterdevv1alpha1.Group{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: shutdownGroupName, Namespace: "homelab-autoscaler-system"}, testGroup)
				if err == nil {
					_ = k8sClient.Delete(ctx, testGroup)
				}
			})

			It("should set Node condition to 'terminating' when shutdown is initiated", func() {
				By("Creating the referenced Kubernetes node")
				k8sNode := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: shutdownK8sNodeName,
					},
					Spec: corev1.NodeSpec{
						Unschedulable: false,
					},
				}
				Expect(k8sClient.Create(ctx, k8sNode)).To(Succeed())

				By("Creating a Node resource")
				node := &infrahomeclusterdevv1alpha1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:      shutdownNodeName,
						Namespace: "homelab-autoscaler-system",
						Labels: map[string]string{
							"group": shutdownGroupName,
						},
					},
					Spec: infrahomeclusterdevv1alpha1.NodeSpec{
						KubernetesNodeName: shutdownK8sNodeName,
						HealthcheckPeriod:  60,
						StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
							Image:   "test-image",
							Command: []string{"echo", "startup"},
						},
						ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
							Image:   "test-image",
							Command: []string{"echo", "shutdown"},
						},
						HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
							Image:   "test-image",
							Command: []string{"echo", "healthcheck"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, node)).To(Succeed())

				By("Creating a NodeReconciler with GroupStore")
				controllerReconciler := &NodeReconciler{
					Client:     k8sClient,
					Scheme:     k8sClient.Scheme(),
					GroupStore: groupStore,
				}

				By("Calling SetNodeConditionTerminatingForShutdown")
				err := controllerReconciler.SetNodeConditionTerminatingForShutdown(ctx, shutdownNodeName)
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the Node condition is set to 'terminating'")
				updatedNode := &infrahomeclusterdevv1alpha1.Node{}
				// Retry getting the updated node to ensure status is updated
				Eventually(func() bool {
					err = k8sClient.Get(ctx, types.NamespacedName{Name: shutdownNodeName, Namespace: "homelab-autoscaler-system"}, updatedNode)
					if err != nil {
						return false
					}
					// Debug: Print all conditions
					fmt.Printf("DEBUG: Node conditions: %+v\n", updatedNode.Status.Conditions)
					terminatingCondition := getNodeCondition(updatedNode.Status.Conditions, "Terminating")
					fmt.Printf("DEBUG: Terminating condition: %+v\n", terminatingCondition)
					return terminatingCondition != nil && terminatingCondition.Status == metav1.ConditionTrue
				}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())

				terminatingCondition := getNodeCondition(updatedNode.Status.Conditions, "Terminating")
				Expect(terminatingCondition.Reason).To(Equal("NodeGroupDeleteNodes"))
				Expect(terminatingCondition.Message).To(ContainSubstring("Node shutdown operation initiated"))

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, node)).To(Succeed())
				Expect(k8sClient.Delete(ctx, k8sNode)).To(Succeed())
			})

			It("should set Node condition to 'offline' when healthy to unhealthy transition detected for terminating nodes", func() {
				By("Creating the referenced Kubernetes node")
				k8sNode := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: shutdownK8sNodeName,
					},
					Spec: corev1.NodeSpec{
						Unschedulable: false,
					},
				}
				Expect(k8sClient.Create(ctx, k8sNode)).To(Succeed())

				By("Creating a Node resource")
				node := &infrahomeclusterdevv1alpha1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:      shutdownNodeName,
						Namespace: "homelab-autoscaler-system",
						Labels: map[string]string{
							"group": shutdownGroupName,
						},
					},
					Spec: infrahomeclusterdevv1alpha1.NodeSpec{
						KubernetesNodeName: shutdownK8sNodeName,
						HealthcheckPeriod:  60,
						StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
							Image:   "test-image",
							Command: []string{"echo", "startup"},
						},
						ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
							Image:   "test-image",
							Command: []string{"echo", "shutdown"},
						},
						HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
							Image:   "test-image",
							Command: []string{"echo", "healthcheck"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, node)).To(Succeed())

				By("Creating a NodeReconciler with GroupStore")
				controllerReconciler := &NodeReconciler{
					Client:     k8sClient,
					Scheme:     k8sClient.Scheme(),
					GroupStore: groupStore,
				}

				By("First setting node to healthy status")
				groupStore.SetNodeHealthcheckStatus(shutdownGroupName, shutdownK8sNodeName, "healthy")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: shutdownNodeName, Namespace: "homelab-autoscaler-system"},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Setting terminating condition")
				err = controllerReconciler.SetNodeConditionTerminatingForShutdown(ctx, shutdownNodeName)
				Expect(err).NotTo(HaveOccurred())

				By("Simulating health status change from healthy to offline for terminating node")
				groupStore.SetNodeHealthcheckStatus(shutdownGroupName, shutdownK8sNodeName, "offline")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: shutdownNodeName, Namespace: "homelab-autoscaler-system"},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the Node condition transitions correctly")
				updatedNode := &infrahomeclusterdevv1alpha1.Node{}
				err = k8sClient.Get(ctx, types.NamespacedName{Name: shutdownNodeName, Namespace: "homelab-autoscaler-system"}, updatedNode)
				Expect(err).NotTo(HaveOccurred())

				// Should have terminating condition set
				terminatingCondition := getNodeCondition(updatedNode.Status.Conditions, "Terminating")
				Expect(terminatingCondition).NotTo(BeNil())
				Expect(terminatingCondition.Status).To(Equal(metav1.ConditionTrue))

				// Should not have available condition when offline
				availableCondition := getNodeCondition(updatedNode.Status.Conditions, "Available")
				if availableCondition != nil {
					Expect(availableCondition.Status).To(Equal(metav1.ConditionFalse))
				}

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, node)).To(Succeed())
				Expect(k8sClient.Delete(ctx, k8sNode)).To(Succeed())
			})

			It("should handle error cases for shutdown condition methods", func() {
				By("Creating a NodeReconciler with GroupStore")
				controllerReconciler := &NodeReconciler{
					Client:     k8sClient,
					Scheme:     k8sClient.Scheme(),
					GroupStore: groupStore,
				}

				By("Calling SetNodeConditionTerminatingForShutdown with non-existent node")
				err := controllerReconciler.SetNodeConditionTerminatingForShutdown(ctx, "non-existent-node")
				Expect(err).To(HaveOccurred())

				By("Calling SetNodeConditionProgressingForScaling with non-existent node")
				err = controllerReconciler.SetNodeConditionProgressingForScaling(ctx, "non-existent-node")
				Expect(err).To(HaveOccurred())
			})

			It("should handle condition transitions for terminating nodes correctly", func() {
				By("Creating the referenced Kubernetes node")
				k8sNode := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: shutdownK8sNodeName,
					},
					Spec: corev1.NodeSpec{
						Unschedulable: false,
					},
				}
				Expect(k8sClient.Create(ctx, k8sNode)).To(Succeed())

				By("Creating a Node resource")
				node := &infrahomeclusterdevv1alpha1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:      shutdownNodeName,
						Namespace: "homelab-autoscaler-system",
						Labels: map[string]string{
							"group": shutdownGroupName,
						},
					},
					Spec: infrahomeclusterdevv1alpha1.NodeSpec{
						KubernetesNodeName: shutdownK8sNodeName,
						HealthcheckPeriod:  60,
						StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
							Image:   "test-image",
							Command: []string{"echo", "startup"},
						},
						ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
							Image:   "test-image",
							Command: []string{"echo", "shutdown"},
						},
						HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
							Image:   "test-image",
							Command: []string{"echo", "healthcheck"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, node)).To(Succeed())

				By("Creating a NodeReconciler with GroupStore")
				controllerReconciler := &NodeReconciler{
					Client:     k8sClient,
					Scheme:     k8sClient.Scheme(),
					GroupStore: groupStore,
				}

				By("Setting initial healthy status")
				groupStore.SetNodeHealthcheckStatus(shutdownGroupName, shutdownK8sNodeName, "healthy")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: shutdownNodeName, Namespace: "homelab-autoscaler-system"},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial available condition")
				updatedNode := &infrahomeclusterdevv1alpha1.Node{}
				err = k8sClient.Get(ctx, types.NamespacedName{Name: shutdownNodeName, Namespace: "homelab-autoscaler-system"}, updatedNode)
				Expect(err).NotTo(HaveOccurred())

				availableCondition := getNodeCondition(updatedNode.Status.Conditions, "Available")
				Expect(availableCondition).NotTo(BeNil())
				Expect(availableCondition.Status).To(Equal(metav1.ConditionTrue))

				By("Initiating shutdown (setting terminating condition)")
				err = controllerReconciler.SetNodeConditionTerminatingForShutdown(ctx, shutdownNodeName)
				Expect(err).NotTo(HaveOccurred())

				By("Simulating node going offline during shutdown")
				groupStore.SetNodeHealthcheckStatus(shutdownGroupName, shutdownK8sNodeName, "offline")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: shutdownNodeName, Namespace: "homelab-autoscaler-system"},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying final condition state")
				err = k8sClient.Get(ctx, types.NamespacedName{Name: shutdownNodeName, Namespace: "homelab-autoscaler-system"}, updatedNode)
				Expect(err).NotTo(HaveOccurred())

				// Should have terminating condition
				terminatingCondition := getNodeCondition(updatedNode.Status.Conditions, "Terminating")
				Expect(terminatingCondition).NotTo(BeNil())
				Expect(terminatingCondition.Status).To(Equal(metav1.ConditionTrue))

				// Should not have available condition when offline
				availableCondition = getNodeCondition(updatedNode.Status.Conditions, "Available")
				if availableCondition != nil {
					Expect(availableCondition.Status).To(Equal(metav1.ConditionFalse))
				}

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, node)).To(Succeed())
				Expect(k8sClient.Delete(ctx, k8sNode)).To(Succeed())
			})
		})
	})

	Context("When validating referenced Kubernetes nodes", func() {
		ctx := context.Background()
		var groupStore *groupstore.GroupStore
		var resourceName string
		var groupName string

		BeforeEach(func() {
			groupStore = groupstore.NewGroupStore()
			// Use unique names for each test to avoid conflicts
			resourceName = fmt.Sprintf("test-node-validation-%d", time.Now().UnixNano())
			groupName = fmt.Sprintf("test-group-validation-%d", time.Now().UnixNano())

			// Create the test group first
			testGroup := &infrahomeclusterdevv1alpha1.Group{
				ObjectMeta: metav1.ObjectMeta{
					Name:      groupName,
					Namespace: "homelab-autoscaler-system",
				},
				Spec: infrahomeclusterdevv1alpha1.GroupSpec{
					Name:         groupName,
					MaxSize:      5,
					NodeSelector: map[string]string{"group": groupName},
					Pricing: infrahomeclusterdevv1alpha1.PricingSpec{
						MonthlyRate: "100.0",
						HourlyRate:  "0.1",
					},
				},
			}
			Expect(k8sClient.Create(ctx, testGroup)).To(Succeed())
		})

		AfterEach(func() {
			// Clean up the group
			testGroup := &infrahomeclusterdevv1alpha1.Group{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: groupName, Namespace: "homelab-autoscaler-system"}, testGroup)
			if err == nil {
				_ = k8sClient.Delete(ctx, testGroup)
			}
		})

		It("should reject Node creation when referenced Kubernetes node does not exist", func() {
			By("Creating a Node resource with non-existent Kubernetes node")
			node := &infrahomeclusterdevv1alpha1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "homelab-autoscaler-system",
					Labels: map[string]string{
						"group": groupName,
					},
				},
				Spec: infrahomeclusterdevv1alpha1.NodeSpec{
					KubernetesNodeName: "non-existent-node",
					HealthcheckPeriod:  60,
					StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "startup"},
					},
					ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "shutdown"},
					},
					HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "healthcheck"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			By("Creating a NodeReconciler with GroupStore")
			controllerReconciler := &NodeReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupStore,
			}

			By("Reconciling the Node")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("referenced Kubernetes node \"non-existent-node\" does not exist"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		})

		It("should reject Node creation when referenced Kubernetes node is unschedulable", func() {
			By("Creating an unschedulable Kubernetes node")
			unschedulableNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "unschedulable-node",
				},
				Spec: corev1.NodeSpec{
					Unschedulable: true,
				},
			}
			Expect(k8sClient.Create(ctx, unschedulableNode)).To(Succeed())

			By("Creating a Node resource referencing the unschedulable node")
			node := &infrahomeclusterdevv1alpha1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "homelab-autoscaler-system",
					Labels: map[string]string{
						"group": groupName,
					},
				},
				Spec: infrahomeclusterdevv1alpha1.NodeSpec{
					KubernetesNodeName: "unschedulable-node",
					HealthcheckPeriod:  60,
					StartupPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "startup"},
					},
					ShutdownPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "shutdown"},
					},
					HealthcheckPodSpec: infrahomeclusterdevv1alpha1.MinimalPodSpec{
						Image:   "test-image",
						Command: []string{"echo", "healthcheck"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			By("Creating a NodeReconciler with GroupStore")
			controllerReconciler := &NodeReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupStore,
			}

			By("Reconciling the Node")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "homelab-autoscaler-system"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("referenced Kubernetes node \"unschedulable-node\" is unschedulable"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, unschedulableNode)).To(Succeed())
		})
	})
})

// Helper function to get a specific condition from a list of conditions
func getNodeCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
