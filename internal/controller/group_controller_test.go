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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrahomeclusterdevv1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
)

var _ = Describe("Group Controller", func() {
	Context("When reconciling a Group resource", func() {
		const (
			groupName      = "test-group"
			groupNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      groupName,
			Namespace: groupNamespace,
		}
		group := &infrahomeclusterdevv1alpha1.Group{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Group")
			group = &infrahomeclusterdevv1alpha1.Group{
				ObjectMeta: metav1.ObjectMeta{
					Name:      groupName,
					Namespace: groupNamespace,
				},
				Spec: infrahomeclusterdevv1alpha1.GroupSpec{
					Name:    "test-group-name",
					MaxSize: 5,
					NodeSelector: map[string]string{
						"node-type": "worker",
					},
					Pricing: infrahomeclusterdevv1alpha1.PricingSpec{
						HourlyRate:  "0.5",
						MonthlyRate: "300",
					},
				},
			}
			Expect(k8sClient.Create(ctx, group)).To(Succeed())
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance Group")
			err := k8sClient.Get(ctx, typeNamespacedName, group)
			if err == nil {
				Expect(k8sClient.Delete(ctx, group)).To(Succeed())
			}
		})

		It("should successfully reconcile a Group resource and store it in groupstore", func() {
			By("Reconciling the created resource")
			controllerReconciler := &GroupReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupstore.NewGroupStore(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Group was stored in groupstore")
			storedGroup, err := controllerReconciler.GroupStore.Get(groupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(storedGroup).NotTo(BeNil())
			Expect(storedGroup.Spec.Name).To(Equal("test-group-name"))
			Expect(storedGroup.Spec.MaxSize).To(Equal(5))

			By("Verifying the Group status was updated")
			updatedGroup := &infrahomeclusterdevv1alpha1.Group{}
			err = k8sClient.Get(ctx, typeNamespacedName, updatedGroup)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedGroup.Status.Conditions).To(HaveLen(1))
			Expect(updatedGroup.Status.Conditions[0].Type).To(Equal("Loaded"))
			Expect(updatedGroup.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
		})

		It("should handle non-existent Group resources gracefully", func() {
			By("Creating a reconciler with groupstore")
			controllerReconciler := &GroupReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupstore.NewGroupStore(),
			}

			By("Reconciling a non-existent Group")
			nonExistentName := types.NamespacedName{
				Name:      "non-existent-group",
				Namespace: groupNamespace,
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: nonExistentName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle Group deletion properly", func() {
			By("Creating a reconciler with groupstore")
			controllerReconciler := &GroupReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupstore.NewGroupStore(),
			}

			By("First reconciling the Group to ensure it's in groupstore")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Group exists in groupstore")
			_, err = controllerReconciler.GroupStore.Get(groupName)
			Expect(err).NotTo(HaveOccurred())

			By("Marking the Group for deletion")
			groupToDelete := &infrahomeclusterdevv1alpha1.Group{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, groupToDelete)).To(Succeed())

			// Delete the Group using Kubernetes client to properly set deletion timestamp
			Expect(k8sClient.Delete(ctx, groupToDelete)).To(Succeed())

			By("Reconciling the deleted Group")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			// The controller should handle the "not found" case gracefully
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Group was removed from groupstore")
			_, err = controllerReconciler.GroupStore.Get(groupName)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("should update existing Group resources in groupstore", func() {
			By("Creating a reconciler with groupstore")
			controllerReconciler := &GroupReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupstore.NewGroupStore(),
			}

			By("First reconciling the Group")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Updating the Group spec")
			updatedGroup := &infrahomeclusterdevv1alpha1.Group{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updatedGroup)).To(Succeed())
			updatedGroup.Spec.MaxSize = 10
			Expect(k8sClient.Update(ctx, updatedGroup)).To(Succeed())

			By("Reconciling the updated Group")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Group was updated in groupstore")
			storedGroup, err := controllerReconciler.GroupStore.Get(groupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(storedGroup.Spec.MaxSize).To(Equal(10))
		})

		It("should handle Group resources with edge case specifications gracefully", func() {
			By("Creating a Group with edge case specification")
			edgeCaseGroup := &infrahomeclusterdevv1alpha1.Group{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "edge-case-group",
					Namespace: groupNamespace,
				},
				Spec: infrahomeclusterdevv1alpha1.GroupSpec{
					Name:    "edge-case-group-name",
					MaxSize: 0, // Edge case: zero size (valid for CRD but may be problematic for business logic)
					NodeSelector: map[string]string{
						"node-type": "worker",
					},
					Pricing: infrahomeclusterdevv1alpha1.PricingSpec{
						HourlyRate:  "0.0", // Edge case: zero rate
						MonthlyRate: "0.0", // Edge case: zero rate
					},
				},
			}
			Expect(k8sClient.Create(ctx, edgeCaseGroup)).To(Succeed())

			defer func() {
				By("Cleaning up edge case Group")
				err := k8sClient.Delete(ctx, edgeCaseGroup)
				if err == nil || !errors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				}
			}()

			By("Reconciling the edge case Group")
			controllerReconciler := &GroupReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupstore.NewGroupStore(),
			}

			edgeCaseGroupName := types.NamespacedName{
				Name:      "edge-case-group",
				Namespace: groupNamespace,
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: edgeCaseGroupName,
			})
			// The controller should not error even with edge case specs
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the edge case Group was stored in groupstore")
			storedGroup, err := controllerReconciler.GroupStore.Get("edge-case-group")
			Expect(err).NotTo(HaveOccurred())
			Expect(storedGroup).NotTo(BeNil())
		})

		It("should correctly handle finalizers", func() {
			By("Creating a Group with finalizers")
			groupWithFinalizers := &infrahomeclusterdevv1alpha1.Group{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "group-with-finalizers",
					Namespace:  groupNamespace,
					Finalizers: []string{"test-finalizer"},
				},
				Spec: infrahomeclusterdevv1alpha1.GroupSpec{
					Name:    "test-group-with-finalizers",
					MaxSize: 3,
					NodeSelector: map[string]string{
						"node-type": "worker",
					},
					Pricing: infrahomeclusterdevv1alpha1.PricingSpec{
						HourlyRate:  "1.0",
						MonthlyRate: "600",
					},
				},
			}
			Expect(k8sClient.Create(ctx, groupWithFinalizers)).To(Succeed())

			defer func() {
				By("Cleaning up Group with finalizers")
				err := k8sClient.Delete(ctx, groupWithFinalizers)
				if err == nil || !errors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				}
			}()

			By("Reconciling the Group with finalizers")
			controllerReconciler := &GroupReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupstore.NewGroupStore(),
			}

			groupWithFinalizersName := types.NamespacedName{
				Name:      "group-with-finalizers",
				Namespace: groupNamespace,
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: groupWithFinalizersName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Group with finalizers was stored in groupstore")
			storedGroup, err := controllerReconciler.GroupStore.Get("group-with-finalizers")
			Expect(err).NotTo(HaveOccurred())
			Expect(storedGroup).NotTo(BeNil())

			By("Marking the Group with finalizers for deletion")
			groupToDelete := &infrahomeclusterdevv1alpha1.Group{}
			Expect(k8sClient.Get(ctx, groupWithFinalizersName, groupToDelete)).To(Succeed())

			// Delete the Group using Kubernetes client to properly set deletion timestamp
			Expect(k8sClient.Delete(ctx, groupToDelete)).To(Succeed())

			By("Reconciling the deleted Group with finalizers")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: groupWithFinalizersName,
			})
			// The controller should handle the "not found" case gracefully
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Group with finalizers was removed from groupstore")
			_, err = controllerReconciler.GroupStore.Get("group-with-finalizers")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		DescribeTable("should handle various Group specifications",
			func(groupName string, spec infrahomeclusterdevv1alpha1.GroupSpec, expectError bool) {
				By("Creating a Group with specific spec")
				testGroup := &infrahomeclusterdevv1alpha1.Group{
					ObjectMeta: metav1.ObjectMeta{
						Name:      groupName,
						Namespace: groupNamespace,
					},
					Spec: spec,
				}
				Expect(k8sClient.Create(ctx, testGroup)).To(Succeed())

				defer func() {
					By("Cleaning up test Group")
					err := k8sClient.Delete(ctx, testGroup)
					if err == nil || !errors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}()

				By("Reconciling the Group")
				controllerReconciler := &GroupReconciler{
					Client:     k8sClient,
					Scheme:     k8sClient.Scheme(),
					GroupStore: groupstore.NewGroupStore(),
				}

				testGroupName := types.NamespacedName{
					Name:      groupName,
					Namespace: groupNamespace,
				}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: testGroupName,
				})

				if expectError {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).NotTo(HaveOccurred())

					By("Verifying the Group was stored in groupstore")
					storedGroup, err := controllerReconciler.GroupStore.Get(groupName)
					Expect(err).NotTo(HaveOccurred())
					Expect(storedGroup).NotTo(BeNil())
				}
			},
			Entry("Minimal valid Group",
				"minimal-group",
				infrahomeclusterdevv1alpha1.GroupSpec{
					Name:    "minimal-group-name",
					MaxSize: 1,
					NodeSelector: map[string]string{
						"node-type": "worker",
					},
					Pricing: infrahomeclusterdevv1alpha1.PricingSpec{
						HourlyRate:  "0.1",
						MonthlyRate: "60",
					},
				},
				false),
			Entry("Group with complex node selector",
				"group-complex-selector",
				infrahomeclusterdevv1alpha1.GroupSpec{
					Name:    "complex-selector-group",
					MaxSize: 10,
					NodeSelector: map[string]string{
						"node-type":              "worker",
						"region":                 "us-west",
						"kubernetes.io/hostname": "node-1",
					},
					Pricing: infrahomeclusterdevv1alpha1.PricingSpec{
						HourlyRate:  "2.5",
						MonthlyRate: "1500",
					},
				},
				false),
		)

		It("should correctly update Group status conditions", func() {
			By("Reconciling the Group")
			controllerReconciler := &GroupReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				GroupStore: groupstore.NewGroupStore(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Group status conditions")
			updatedGroup := &infrahomeclusterdevv1alpha1.Group{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updatedGroup)).To(Succeed())

			Expect(updatedGroup.Status.Conditions).To(HaveLen(1))
			condition := updatedGroup.Status.Conditions[0]
			Expect(condition.Type).To(Equal("Loaded"))
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal("GroupLoaded"))
			Expect(condition.Message).To(Equal("Group has been successfully loaded and is ready for use"))
			Expect(condition.LastTransitionTime.Time).To(BeTemporally("~", time.Now(), time.Second*10))
		})
	})
})
