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

package integration

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	infrav2alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v2alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/claim"
	"github.com/homecluster-dev/homelab-autoscaler/internal/config"
	"github.com/homecluster-dev/homelab-autoscaler/internal/grpcserver"
	"github.com/homecluster-dev/homelab-autoscaler/internal/taints"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
	"github.com/homecluster-dev/homelab-autoscaler/test/integration/mocks"
	"github.com/homecluster-dev/homelab-autoscaler/test/integration/testdata"
)

// cmpResource compares a declared quantity against what a template carries. Cmp, not a struct
// comparison: the template round-trips through protobuf, which re-renders a BinarySI number
// (3.2Gi) as its milli-decimal equivalent (3435973836800m), so the two Quantity values are the
// same number in two representations.
func cmpResource(list corev1.ResourceList, name corev1.ResourceName, declared string) int {
	want, got := resource.MustParse(declared), list[name]

	return want.Cmp(got)
}

var _ = Describe("gRPC Server Integration Tests", func() {
	var (
		testCtx    context.Context
		testCancel context.CancelFunc
		server     *grpcserver.HomeClusterProviderServer
		loader     *testdata.TestDataLoader
		namespace  *config.NamespaceConfig
	)

	BeforeEach(func() {
		testCtx, testCancel = context.WithTimeout(ctx, 30*time.Second)
		loader = testdata.NewTestDataLoader(scheme.Scheme)
		namespace = config.NewNamespaceConfig()
	})

	AfterEach(func() {
		testCancel()
	})

	Describe("NodeGroups endpoint", func() {
		Context("when groups exist in the cluster", func() {
			It("should return all node groups", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithAllData()
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroups endpoint
				req := &pb.NodeGroupsRequest{}
				resp, err := server.NodeGroups(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.NodeGroups).To(HaveLen(8)) // Based on test data: test-group, gpu-group, cpu-group,
				// offline-group, unknown-group, empty-group, fast-scale-group, dev-group

				// Verify specific groups. dev-group is the group that declares a per-group
				// minimum size in the test data; every other group keeps the default floor of 0.
				groupNames := make([]string, 0, len(resp.NodeGroups))
				for _, group := range resp.NodeGroups {
					groupNames = append(groupNames, group.Id)

					expectedMinSize := int32(0)
					if group.Id == "dev-group" {
						expectedMinSize = 1
					}
					Expect(group.MinSize).To(Equal(expectedMinSize), "minSize of group %s", group.Id)
					Expect(group.MinSize).To(BeNumerically("<=", group.MaxSize),
						"minSize of group %s must not exceed its slots", group.Id)
					Expect(group.MaxSize).To(BeNumerically(">=", 0))
				}
				Expect(groupNames).To(ContainElements("test-group", "gpu-group", "cpu-group", "offline-group",
					"unknown-group", "empty-group", "fast-scale-group", "dev-group"))
			})
		})

		Context("when no groups exist", func() {
			It("should return empty list", func() {
				// Setup empty mock client
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroups endpoint
				req := &pb.NodeGroupsRequest{}
				resp, err := server.NodeGroups(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.NodeGroups).To(BeEmpty())
			})
		})
	})

	Describe("NodeGroupForNode endpoint", func() {
		Context("when node belongs to a group", func() {
			It("should return the correct node group", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithGroup("test-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupForNode endpoint
				req := &pb.NodeGroupForNodeRequest{
					Node: &pb.ExternalGrpcNode{
						Name: "test-node-1",
					},
				}
				resp, err := server.NodeGroupForNode(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.NodeGroup).NotTo(BeNil())
				Expect(resp.NodeGroup.Id).To(Equal("test-group"))
			})
		})

		Context("when node does not belong to any group", func() {
			It("should return empty node group", func() {
				// Setup test data with orphaned node
				mockClient, err := loader.CreateMockClientWithScenario("orphaned-node")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupForNode endpoint
				req := &pb.NodeGroupForNodeRequest{
					Node: &pb.ExternalGrpcNode{
						Name: "orphaned-node-1",
					},
				}
				resp, err := server.NodeGroupForNode(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.NodeGroup).NotTo(BeNil())
				Expect(resp.NodeGroup.Id).To(Equal(""))
			})
		})

		Context("when node does not exist", func() {
			It("should return empty node group", func() {
				// Setup empty mock client
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupForNode endpoint
				req := &pb.NodeGroupForNodeRequest{
					Node: &pb.ExternalGrpcNode{
						Name: "non-existent-node",
					},
				}
				resp, err := server.NodeGroupForNode(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.NodeGroup).NotTo(BeNil())
				Expect(resp.NodeGroup.Id).To(Equal(""))
			})
		})
	})

	Describe("NodeGroupTargetSize endpoint", func() {
		Context("when group has nodes", func() {
			It("should return correct target size", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithGroup("test-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupTargetSize endpoint
				req := &pb.NodeGroupTargetSizeRequest{
					Id: "test-group",
				}
				resp, err := server.NodeGroupTargetSize(testCtx, req)

				// Verify response: the target size is the count of the group's live claims,
				// and the fixture gives test-group two of them.
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.TargetSize).To(Equal(int32(2)))
			})
		})

		Context("when group does not exist", func() {
			It("should handle non-existent group gracefully", func() {
				// Setup empty mock client
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupTargetSize endpoint
				req := &pb.NodeGroupTargetSizeRequest{
					Id: "non-existent-group",
				}
				resp, err := server.NodeGroupTargetSize(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.TargetSize).To(Equal(int32(0)))
			})
		})
	})

	Describe("NodeGroupIncreaseSize endpoint", func() {
		Context("when a free slot is available", func() {
			It("should create a NodeClaim for it, attributed to the request", func() {
				// Setup test data with gpu-group, whose second slot has no live claim: the slot
				// a failed claim freed (claim-gpu-node-2, DeadlineExceeded), which is the only
				// shape IncreaseSize may claim in v2alpha1.
				mockClient, err := loader.CreateMockClientWithAllData()
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				before, err := server.NodeGroupTargetSize(testCtx, &pb.NodeGroupTargetSizeRequest{Id: "gpu-group"})
				Expect(err).NotTo(HaveOccurred())
				Expect(before.TargetSize).To(Equal(int32(1)), "only gpu-node-1 has a live claim")

				// Call NodeGroupIncreaseSize endpoint
				req := &pb.NodeGroupIncreaseSizeRequest{
					Id:    "gpu-group",
					Delta: 1,
				}
				resp, err := server.NodeGroupIncreaseSize(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())

				// The request is an object now, and the object says who asked and for what.
				claims := &infrav2alpha1.NodeClaimList{}
				Expect(mockClient.List(testCtx, claims, &client.ListOptions{Namespace: namespace.Get()})).To(Succeed())

				var created *infrav2alpha1.NodeClaim

				for i := range claims.Items {
					candidate := &claims.Items[i]
					if candidate.Spec.SlotRef.Name == "gpu-node-2" &&
						candidate.Spec.Request.Call == infrav2alpha1.ClaimCallIncreaseSize &&
						candidate.Status.Phase == "" {
						created = candidate

						break
					}
				}

				requireClaimsFor(created, "gpu-node-2", "gpu-group", int32(1))

				// A created claim carries no phase until the claim controller's first write:
				// the status subresource strips `status` from a create, and the CRD declares
				// no default there. The provider reads that empty phase as pending
				// (phase.IsLive), so the claim is live and counted from its create - the
				// fixture lands the controller's write here to assert the arithmetic on the
				// phase the episode runs at, not to make the claim count.
				created.Status.Phase = infrav2alpha1.NodeClaimPending
				Expect(mockClient.Update(testCtx, created)).To(Succeed())

				after, err := server.NodeGroupTargetSize(testCtx, &pb.NodeGroupTargetSizeRequest{Id: "gpu-group"})
				Expect(err).NotTo(HaveOccurred())
				Expect(after.TargetSize).To(Equal(int32(2)),
					"the count of live claims is the target size, and the created claim is one")

				// No field of the slot carries the request: the spec is Git-owned configuration.
				node := &infrav2alpha1.Node{}
				gpuKey := client.ObjectKey{Name: "gpu-node-2", Namespace: namespace.Get()}
				Expect(mockClient.Get(testCtx, gpuKey, node)).To(Succeed())
				Expect(node.Spec.KubernetesNodeName).To(Equal("gpu-node-2"), "the handler wrote no field of the slot")
			})
		})

		Context("when no powered off nodes are available", func() {
			It("should return appropriate error", func() {
				// Setup test data with all nodes powered on
				mockClient, err := loader.CreateMockClientWithGroup("test-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupIncreaseSize endpoint
				req := &pb.NodeGroupIncreaseSizeRequest{
					Id:    "test-group",
					Delta: 1,
				}
				_, err = server.NodeGroupIncreaseSize(testCtx, req)

				// Verify error
				Expect(err).To(HaveOccurred())
				st, ok := status.FromError(err)
				Expect(ok).To(BeTrue())
				Expect(st.Code()).To(Equal(codes.FailedPrecondition))
			})
		})

		Context("when delta is invalid", func() {
			It("should return validation error", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithGroup("test-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupIncreaseSize endpoint with invalid delta
				req := &pb.NodeGroupIncreaseSizeRequest{
					Id:    "test-group",
					Delta: -1,
				}
				_, err = server.NodeGroupIncreaseSize(testCtx, req)

				// Verify error
				Expect(err).To(HaveOccurred())
				st, ok := status.FromError(err)
				Expect(ok).To(BeTrue())
				Expect(st.Code()).To(Equal(codes.InvalidArgument))
			})
		})
	})

	Describe("NodeGroupDeleteNodes endpoint", func() {
		Context("when a slot's machine is named for deletion", func() {
			It("should cancel the claim that owns it", func() {
				// Setup test data: test-group has two running slots, each owned by a live
				// ready claim. cluster-autoscaler drains first, then calls DeleteNodes.
				mockClient, err := loader.CreateMockClientWithGroup("test-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				before, err := server.NodeGroupTargetSize(testCtx, &pb.NodeGroupTargetSizeRequest{Id: "test-group"})
				Expect(err).NotTo(HaveOccurred())
				Expect(before.TargetSize).To(Equal(int32(2)))

				// Call NodeGroupDeleteNodes endpoint
				req := &pb.NodeGroupDeleteNodesRequest{
					Id: "test-group",
					Nodes: []*pb.ExternalGrpcNode{
						{Name: "test-node-1"},
					},
				}
				resp, err := server.NodeGroupDeleteNodes(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())

				// The claim of the named slot is cancelled: its unit comes off the target size
				// on the write that asked for it, and the slot reconciler powers the machine off
				// because nothing wants it any more.
				claims := &infrav2alpha1.NodeClaimList{}
				Expect(mockClient.List(testCtx, claims, &client.ListOptions{Namespace: namespace.Get()})).To(Succeed())

				for _, candidate := range claims.Items {
					Expect(candidate.Spec.SlotRef.Name).NotTo(Equal("test-node-1"),
						"the request that owned the named slot is cancelled, not left live")
				}

				after, err := server.NodeGroupTargetSize(testCtx, &pb.NodeGroupTargetSizeRequest{Id: "test-group"})
				Expect(err).NotTo(HaveOccurred())
				Expect(after.TargetSize).To(Equal(int32(1)), "the autoscaler reads the size the request produced")

				// The slot itself is untouched: DeleteNodes records and cancels a claim, it does
				// not write a slot.
				node := &infrav2alpha1.Node{}
				nodeKey := client.ObjectKey{Name: "test-node-1", Namespace: namespace.Get()}
				Expect(mockClient.Get(testCtx, nodeKey, node)).To(Succeed())
				Expect(node.Spec.KubernetesNodeName).To(Equal("test-node-1"))
			})
		})

		Context("when the named machine has no slot", func() {
			It("answers the request as the no-op it is", func() {
				// The v1alpha1 handler looked each name up as a slot and answered the miss
				// with codes.Internal, which cluster-autoscaler retried. Under the claim model
				// the request cancels the claim of a slot; a name the provider has no slot for
				// cancels nothing, takes no capacity away, and is answered as a success so the
				// autoscaler moves on.
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupDeleteNodes endpoint
				req := &pb.NodeGroupDeleteNodesRequest{
					Id: "test-group",
					Nodes: []*pb.ExternalGrpcNode{
						{Name: "non-existent-node"},
					},
				}
				_, err := server.NodeGroupDeleteNodes(testCtx, req)
				Expect(err).NotTo(HaveOccurred())

				claims := &infrav2alpha1.NodeClaimList{}
				Expect(mockClient.List(testCtx, claims, &client.ListOptions{Namespace: namespace.Get()})).To(Succeed())
				Expect(claims.Items).To(BeEmpty(), "a request that names no slot creates nothing")

				size, err := server.NodeGroupTargetSize(testCtx, &pb.NodeGroupTargetSizeRequest{Id: "test-group"})
				Expect(err).NotTo(HaveOccurred())
				Expect(size.TargetSize).To(Equal(int32(0)))
			})
		})
	})

	Describe("NodeGroupDecreaseTargetSize endpoint", func() {
		Context("when group has running nodes", func() {
			It("refuses a decrease of a group whose live claims are all committed, retryably", func() {
				// The scenario's machine is a ready claim: the machine is running work, and
				// this entry point cancels pending claims only (docs/development/nodeclaim.md §5).
				mockClient, err := loader.CreateMockClientWithScenario("shutting-down")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupDecreaseTargetSize endpoint
				req := &pb.NodeGroupDecreaseTargetSizeRequest{
					Id:    "cpu-group",
					Delta: -1,
				}
				_, err = server.NodeGroupDecreaseTargetSize(testCtx, req)

				// M6: the provider does not answer a request it did not carry out with success.
				// Success is the autoscaler's cue to stop asking, and it is then left paying for a
				// unit it asked away while IncreaseSize for the same slot answers "no free node
				// slot" - for up to a boot deadline plus the claim's TTL. The state is transient
				// (DeleteNodes closes a ready machine), so the code is the retryable one.
				Expect(err).To(HaveOccurred())

				st, ok := status.FromError(err)
				Expect(ok).To(BeTrue())
				Expect(st.Code()).To(Equal(codes.Unavailable))
				Expect(st.Message()).To(ContainSubstring("ready"))

				size, err := server.NodeGroupTargetSize(testCtx, &pb.NodeGroupTargetSizeRequest{Id: "cpu-group"})
				Expect(err).NotTo(HaveOccurred())
				Expect(size.TargetSize).To(BeNumerically(">", 0),
					"nothing was cancelled, so the size the autoscaler reads back is the size it asked against")
			})
		})

		Context("when delta is invalid", func() {
			It("should return validation error", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithGroup("test-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupDecreaseTargetSize endpoint with invalid delta
				req := &pb.NodeGroupDecreaseTargetSizeRequest{
					Id:    "test-group",
					Delta: 1,
				}
				_, err = server.NodeGroupDecreaseTargetSize(testCtx, req)

				// Verify error
				Expect(err).To(HaveOccurred())
				st, ok := status.FromError(err)
				Expect(ok).To(BeTrue())
				Expect(st.Code()).To(Equal(codes.InvalidArgument))
			})
		})
	})

	Describe("NodeGroupNodes endpoint", func() {
		Context("when group has nodes", func() {
			It("should return all instances in the group", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithGroup("test-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupNodes endpoint
				req := &pb.NodeGroupNodesRequest{
					Id: "test-group",
				}
				resp, err := server.NodeGroupNodes(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.Instances).To(HaveLen(2)) // test-node-1, test-node-2

				// Verify instance properties
				for _, instance := range resp.Instances {
					Expect(instance.Id).NotTo(BeEmpty())
					Expect(instance.Status).NotTo(BeNil())
					Expect(instance.Status.InstanceState).To(Equal(pb.InstanceStatus_instanceRunning))
				}
			})
		})

		Context("when group does not exist", func() {
			It("should handle non-existent group gracefully", func() {
				// Setup empty mock client
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupNodes endpoint
				req := &pb.NodeGroupNodesRequest{
					Id: "non-existent-group",
				}
				resp, err := server.NodeGroupNodes(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.Instances).To(BeEmpty())
			})
		})
	})

	Describe("NodeGroupTemplateNodeInfo endpoint", func() {
		Context("when group has nodes", func() {
			It("should return template node info", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithGroup("test-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupTemplateNodeInfo endpoint
				req := &pb.NodeGroupTemplateNodeInfoRequest{
					Id: "test-group",
				}
				resp, err := server.NodeGroupTemplateNodeInfo(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.NodeBytes).NotTo(BeEmpty())

				// Unmarshal the node bytes to verify contents
				templateNode := &corev1.Node{}
				err = templateNode.Unmarshal(resp.NodeBytes)
				Expect(err).NotTo(HaveOccurred())
				Expect(templateNode.Name).To(ContainSubstring("test-group-template"))
				Expect(templateNode.Spec.Unschedulable).To(BeFalse())

				// Verify template node has proper conditions
				Expect(templateNode.Status.Conditions).To(HaveLen(5))
				readyCondition := templateNode.Status.Conditions[0]
				Expect(readyCondition.Type).To(Equal(corev1.NodeReady))
				Expect(readyCondition.Status).To(Equal(corev1.ConditionTrue))
			})
		})

		Context("when node defines custom taints to remove", func() {
			It("filters those taints from the template node", func() {
				// Build a mock client with a group and a node that defines custom taints to remove
				customTaint := corev1.Taint{Key: "custom/taint", Effect: corev1.TaintEffectNoSchedule}
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).
					WithGroup("test-group").
					WithNode(
						"test-node-1",
						mocks.WithNodeGroup("test-group"),
						mocks.WithNodeTaintsToRemove([]corev1.Taint{customTaint}),
					).
					Build()

				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call the endpoint
				req := &pb.NodeGroupTemplateNodeInfoRequest{Id: "test-group"}
				resp, err := server.NodeGroupTemplateNodeInfo(testCtx, req)

				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.NodeBytes).NotTo(BeEmpty())

				// Unmarshal the node bytes to verify contents
				templateNode := &corev1.Node{}
				err = templateNode.Unmarshal(resp.NodeBytes)
				Expect(err).NotTo(HaveOccurred())

				// Ensure the custom taint is not present in the template node's taints
				for _, t := range templateNode.Spec.Taints {
					Expect(t.Key).NotTo(Equal("custom/taint"))
				}
			})
		})

		Context("when a pool mixes powered off slots with a slot that has a Kubernetes node", func() {
			It("builds the template from the slot with a node, and carries the declared taints", func() {
				// The shape of the machine (allocatable, taints, labels) can only be read from a
				// corev1.Node, so the off slots, which have none, must not be the representative.
				// The declared spec.nodeTaints must reach the template, so a pod that does not
				// tolerate dedicated=remote:NoSchedule is never simulated onto this group.
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).
					WithGroup("test-group").
					WithNode("test-node-1", mocks.WithNodeGroup("test-group"),
						mocks.WithNodeTaints([]corev1.Taint{{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}})).
					WithNode("test-node-2", mocks.WithNodeGroup("test-group"),
						mocks.WithNodePowerState(infrav2alpha1.PowerStateOff)).
					WithKubernetesNode("test-node-1", mocks.WithKubernetesNodeGroupLabel("test-group")).
					Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				resp, err := server.NodeGroupTemplateNodeInfo(testCtx, &pb.NodeGroupTemplateNodeInfoRequest{Id: "test-group"})
				Expect(err).NotTo(HaveOccurred())

				templateNode := &corev1.Node{}
				Expect(templateNode.Unmarshal(resp.NodeBytes)).To(Succeed())

				Expect(templateNode.Status.Allocatable).To(HaveKey(corev1.ResourceCPU),
					"the template must come from the node object, not from a placeholder")
				Expect(templateNode.Spec.Taints).To(ContainElement(corev1.Taint{
					Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule,
				}))
			})
		})

		Context("when no slot of the group has a Kubernetes node", func() {
			It("answers with a placeholder that carries the declared taints and no machine size", func() {
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).
					WithGroup("test-group").
					WithNode("test-node-1", mocks.WithNodeGroup("test-group"),
						mocks.WithNodePowerState(infrav2alpha1.PowerStateOff),
						mocks.WithNodeTaints([]corev1.Taint{{Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule}})).
					Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				resp, err := server.NodeGroupTemplateNodeInfo(testCtx, &pb.NodeGroupTemplateNodeInfoRequest{Id: "test-group"})
				Expect(err).NotTo(HaveOccurred())

				templateNode := &corev1.Node{}
				Expect(templateNode.Unmarshal(resp.NodeBytes)).To(Succeed())

				// the cpu, memory and disk of a machine that never registered cannot be read, so they
				// are not invented. The pod ceiling is the kubelet's own default and is not a fact about
				// the hardware, and a template without it is a node the scheduler answers "Too many
				// pods" for, so the estimator never calls NodeGroupIncreaseSize.
				Expect(templateNode.Status.Allocatable).NotTo(HaveKey(corev1.ResourceCPU))
				Expect(templateNode.Status.Allocatable).NotTo(HaveKey(corev1.ResourceMemory))
				Expect(templateNode.Status.Allocatable).NotTo(HaveKey(corev1.ResourceEphemeralStorage))
				Expect(templateNode.Status.Allocatable).To(HaveKey(corev1.ResourcePods))
				Expect(cmpResource(templateNode.Status.Allocatable, corev1.ResourcePods, "110")).To(BeZero())
				Expect(templateNode.Status.Capacity).To(Equal(templateNode.Status.Allocatable))
				Expect(templateNode.Spec.Taints).To(ConsistOf(corev1.Taint{
					Key: "dedicated", Value: "remote", Effect: corev1.TaintEffectNoSchedule,
				}))
				Expect(templateNode.Labels).To(HaveKeyWithValue("infra.homecluster.dev/group", "test-group"))
			})
		})

		Context("when a powered off slot declares the capacity of its machine", func() {
			It("builds the placeholder at that size, so the pool can scale from zero", func() {
				// The template is the machine cluster-autoscaler simulates a placement on, and a
				// pool at zero has no corev1.Node to read its shape from. With no declaration the
				// placeholder carries an empty allocatable, no pod with requests fits, and
				// NodeGroupIncreaseSize is never called: the pool stays at zero. The declaration is
				// the shape the placeholder can honestly carry, because it is in Git.
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).
					WithGroup("test-group").
					WithNode("test-node-1", mocks.WithNodeGroup("test-group"),
						mocks.WithNodePowerState(infrav2alpha1.PowerStateOff),
						mocks.WithNodeCapacity("1950m", "3.2Gi")).
					Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				resp, err := server.NodeGroupTemplateNodeInfo(testCtx, &pb.NodeGroupTemplateNodeInfoRequest{Id: "test-group"})
				Expect(err).NotTo(HaveOccurred())

				templateNode := &corev1.Node{}
				Expect(templateNode.Unmarshal(resp.NodeBytes)).To(Succeed())

				_, hasCPU := templateNode.Status.Allocatable[corev1.ResourceCPU]
				Expect(hasCPU).To(BeTrue(), "a pod with cpu requests must fit the template")
				Expect(cmpResource(templateNode.Status.Allocatable, corev1.ResourceCPU, "1950m")).To(BeZero(),
					"the template is the declared size")
				Expect(cmpResource(templateNode.Status.Allocatable, corev1.ResourceMemory, "3.2Gi")).To(BeZero())
				Expect(cmpResource(templateNode.Status.Capacity, corev1.ResourceCPU, "1950m")).To(BeZero(),
					"capacity and allocatable are the same list on a template machine")
				// the pod ceiling of the template is what lets the scheduler place a pod on it at all:
				// NodeResourcesFit checks "Too many pods" before it looks at any request
				Expect(cmpResource(templateNode.Status.Allocatable, corev1.ResourcePods, "110")).To(BeZero())
				Expect(cmpResource(templateNode.Status.Allocatable, corev1.ResourceEphemeralStorage, "40Gi")).To(BeZero())
			})
		})

		Context("when the slot declares a capacity and a live node reports another one", func() {
			It("uses the declaration, so the template does not depend on which slot is Ready", func() {
				// The declaration is the shape of the machine the autoscaler is about to boot;
				// what one slot reports right now is an accident of the pool's current state, and
				// in a mixed pool it would hand the estimator a different machine per call.
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).
					WithGroup("test-group").
					WithNode("test-node-1", mocks.WithNodeGroup("test-group"),
						mocks.WithNodePowerState(infrav2alpha1.PowerStateOn),
						mocks.WithNodeCapacity("1950m", "3.2Gi")).
					// the builder's node reports 3.8 CPU / 7.5 GiB allocatable
					WithKubernetesNode("test-node-1", mocks.WithKubernetesNodeGroupLabel("test-group")).
					Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				resp, err := server.NodeGroupTemplateNodeInfo(testCtx, &pb.NodeGroupTemplateNodeInfoRequest{Id: "test-group"})
				Expect(err).NotTo(HaveOccurred())

				templateNode := &corev1.Node{}
				Expect(templateNode.Unmarshal(resp.NodeBytes)).To(Succeed())

				Expect(cmpResource(templateNode.Status.Allocatable, corev1.ResourceCPU, "1950m")).To(BeZero(),
					"the declaration wins over the 3.8 CPU the representative slot reports")
				Expect(cmpResource(templateNode.Status.Allocatable, corev1.ResourceMemory, "3.2Gi")).To(BeZero())
				Expect(cmpResource(templateNode.Status.Allocatable, corev1.ResourceEphemeralStorage, "40Gi")).To(BeZero(),
					"the declaration wins for the resources it names, over the 38Gi the node reports")
				Expect(cmpResource(templateNode.Status.Allocatable, corev1.ResourcePods, "110")).To(BeZero(),
					"the template keeps a pod ceiling: the scheduler rejects every pod without one")
			})
		})

		Context("when the representative node is being evaluated for scale-down", func() {
			It("keeps the autoscaler taints out of the template and keeps the pool's own taint", func() {
				// The template is copied from a live node, so the taints cluster-autoscaler stamped
				// on that node describe its scale-down state, not the shape of a new machine. Left
				// in, the estimator sees a taint no pod tolerates and never scales the group up.
				// The pool's own node.kubernetes.io/role taint, the one the k3d e2e uses, must stay:
				// it is declared in spec.nodeTaints and the workload tolerates it.
				role := corev1.Taint{Key: "node.kubernetes.io/role", Value: "agent", Effect: corev1.TaintEffectNoSchedule}

				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).
					WithGroup("test-group").
					WithNode("test-node-1", mocks.WithNodeGroup("test-group"),
						mocks.WithNodePowerState(infrav2alpha1.PowerStateOn),
						mocks.WithNodeTaints([]corev1.Taint{role})).
					WithKubernetesNode("test-node-1", mocks.WithKubernetesNodeGroupLabel("test-group"),
						mocks.WithKubernetesNodeTaint("node.kubernetes.io/role", "agent", corev1.TaintEffectNoSchedule),
						mocks.WithKubernetesNodeTaint(taints.DeletionCandidateOfClusterAutoscaler, "", corev1.TaintEffectNoSchedule),
						mocks.WithKubernetesNodeTaint(taints.ToBeDeletedByClusterAutoscaler, "1763000000", corev1.TaintEffectNoSchedule)).
					Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				resp, err := server.NodeGroupTemplateNodeInfo(testCtx, &pb.NodeGroupTemplateNodeInfoRequest{Id: "test-group"})
				Expect(err).NotTo(HaveOccurred())

				templateNode := &corev1.Node{}
				Expect(templateNode.Unmarshal(resp.NodeBytes)).To(Succeed())

				keys := make([]string, 0, len(templateNode.Spec.Taints))
				for _, t := range templateNode.Spec.Taints {
					keys = append(keys, t.ToString())
				}

				Expect(keys).To(ConsistOf("node.kubernetes.io/role=agent:NoSchedule"),
					"both cluster-autoscaler taints are stripped, the pool's own taint is not")
				Expect(templateNode.Status.Allocatable).To(HaveKey(corev1.ResourceCPU))
			})
		})

		Context("when group has no nodes", func() {
			It("should return unimplemented error for scale from zero", func() {
				// Setup test data with empty group
				mockClient, err := loader.CreateMockClientWithGroup("empty-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupTemplateNodeInfo endpoint
				req := &pb.NodeGroupTemplateNodeInfoRequest{
					Id: "empty-group",
				}
				_, err = server.NodeGroupTemplateNodeInfo(testCtx, req)

				// Verify error
				Expect(err).To(HaveOccurred())
				st, ok := status.FromError(err)
				Expect(ok).To(BeTrue())
				Expect(st.Code()).To(Equal(codes.Unimplemented))
			})
		})
	})

	Describe("NodeGroupGetOptions endpoint", func() {
		Context("when group exists with autoscaling options", func() {
			It("should return correct autoscaling options", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithGroup("gpu-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupGetOptions endpoint
				req := &pb.NodeGroupAutoscalingOptionsRequest{
					Id: "gpu-group",
				}
				resp, err := server.NodeGroupGetOptions(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.NodeGroupAutoscalingOptions).NotTo(BeNil())

				options := resp.NodeGroupAutoscalingOptions
				Expect(options.ScaleDownUtilizationThreshold).To(Equal(0.3))
				Expect(options.ScaleDownGpuUtilizationThreshold).To(Equal(0.2))
				Expect(options.ZeroOrMaxNodeScaling).To(BeTrue())
				Expect(options.IgnoreDaemonSetsUtilization).To(BeFalse())
			})
		})

		Context("when group does not exist", func() {
			It("should handle non-existent group gracefully", func() {
				// Setup empty mock client
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupGetOptions endpoint
				req := &pb.NodeGroupAutoscalingOptionsRequest{
					Id: "non-existent-group",
				}
				resp, err := server.NodeGroupGetOptions(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
			})
		})
	})

	Describe("PricingNodePrice endpoint", func() {
		Context("when node exists with pricing info", func() {
			It("should return correct node price", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithGroup("gpu-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call PricingNodePrice endpoint
				req := &pb.PricingNodePriceRequest{
					Node: &pb.ExternalGrpcNode{
						Name: "gpu-node-1",
					},
				}
				resp, err := server.PricingNodePrice(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.Price).To(Equal(0.85)) // Based on test data
			})
		})

		Context("when node does not exist", func() {
			It("should return zero price", func() {
				// Setup empty mock client
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call PricingNodePrice endpoint
				req := &pb.PricingNodePriceRequest{
					Node: &pb.ExternalGrpcNode{
						Name: "non-existent-node",
					},
				}
				resp, err := server.PricingNodePrice(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.Price).To(Equal(0.0))
			})
		})

	})

	Describe("PricingPodPrice endpoint", func() {
		Context("when nodes have PodRate set", func() {
			It("should return the minimum PodRate across nodes", func() {
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).
					WithGroup("group-a").
					WithGroup("group-b").
					WithNode(
						"node-a-1",
						mocks.WithNodeGroup("group-a"),
						mocks.WithNodePricing("0.10", "0.02"),
					).
					WithNode(
						"node-b-1",
						mocks.WithNodeGroup("group-b"),
						mocks.WithNodePricing("0.05", "0.005"),
					).
					Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				resp, err := server.PricingPodPrice(testCtx, &pb.PricingPodPriceRequest{})

				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.Price).To(Equal(0.005))
			})
		})

		Context("when nodes have unparseable PodRate", func() {
			It("should skip those nodes and use the next cheapest valid rate", func() {
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).
					WithGroup("group-a").
					WithNode(
						"node-bad",
						mocks.WithNodeGroup("group-a"),
						mocks.WithNodePricing("0.10", "not-a-number"),
					).
					WithNode(
						"node-good",
						mocks.WithNodeGroup("group-a"),
						mocks.WithNodePricing("0.10", "0.03"),
					).
					Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				resp, err := server.PricingPodPrice(testCtx, &pb.PricingPodPriceRequest{})

				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Price).To(Equal(0.03))
			})
		})

		Context("when no nodes are configured", func() {
			It("should return zero price without error", func() {
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				resp, err := server.PricingPodPrice(testCtx, &pb.PricingPodPriceRequest{})

				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.Price).To(Equal(0.0))
			})
		})
	})

	Describe("GPULabel endpoint", func() {
		It("should return the GPU label", func() {
			// Setup mock client
			mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
			server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

			// Call GPULabel endpoint
			req := &pb.GPULabelRequest{}
			resp, err := server.GPULabel(testCtx, req)

			// Verify response
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Label).To(Equal("accelerator"))
		})
	})

	Describe("GetAvailableGPUTypes endpoint", func() {
		It("should return available GPU types", func() {
			// Setup mock client
			mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
			server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

			// Call GetAvailableGPUTypes endpoint
			req := &pb.GetAvailableGPUTypesRequest{}
			resp, err := server.GetAvailableGPUTypes(testCtx, req)

			// Verify response
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.GpuTypes).To(HaveLen(2)) // nvidia-tesla-v100, nvidia-tesla-t4
			Expect(resp.GpuTypes).To(HaveKey("nvidia-tesla-v100"))
			Expect(resp.GpuTypes).To(HaveKey("nvidia-tesla-t4"))
		})
	})

	Describe("Cleanup endpoint", func() {
		It("should perform cleanup successfully", func() {
			// Setup mock client
			mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
			server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

			// Call Cleanup endpoint
			req := &pb.CleanupRequest{}
			resp, err := server.Cleanup(testCtx, req)

			// Verify response
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
		})
	})

	Describe("Refresh endpoint", func() {
		It("should refresh state successfully", func() {
			// Setup mock client
			mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
			server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

			// Call Refresh endpoint
			req := &pb.RefreshRequest{}
			resp, err := server.Refresh(testCtx, req)

			// Verify response
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
		})
	})

	Describe("Context cancellation", func() {
		It("should handle context cancellation gracefully", func() {
			// Setup test data
			mockClient, err := loader.CreateMockClientWithGroup("test-group")
			Expect(err).NotTo(HaveOccurred())
			server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

			// Create cancelled context
			cancelledCtx, cancel := context.WithCancel(context.Background())
			cancel()

			// Call NodeGroupTargetSize with cancelled context
			req := &pb.NodeGroupTargetSizeRequest{
				Id: "test-group",
			}
			_, err = server.NodeGroupTargetSize(cancelledCtx, req)

			// Verify error
			Expect(err).To(HaveOccurred())
			st, ok := status.FromError(err)
			Expect(ok).To(BeTrue())
			Expect(st.Code()).To(Equal(codes.DeadlineExceeded))
		})
	})

	Describe("Error scenarios", func() {
		Context("when client operations fail", func() {
			It("should handle client errors gracefully", func() {
				// This would require a mock client that returns errors
				// For now, we test with empty client which should handle gracefully
				mockClient := mocks.NewMockClientBuilder(scheme.Scheme).Build()
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call with non-existent group
				req := &pb.NodeGroupTargetSizeRequest{
					Id: "non-existent-group",
				}
				resp, err := server.NodeGroupTargetSize(testCtx, req)

				// Should not error, but return 0 target size
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.TargetSize).To(Equal(int32(0)))
			})
		})
	})
})

// requireClaimsFor asserts the attribution a scale-up request leaves on the object it created:
// the slot it asks for, the call that asked, the delta it asked for, the group, and the caller.
// This is the question the v1alpha1 scalar could not answer - who asked, why, for what.
func requireClaimsFor(nodeClaim *infrav2alpha1.NodeClaim, slotName, groupName string, delta int32) {
	GinkgoHelper()

	Expect(nodeClaim).NotTo(BeNil(), "no claim was created for slot %s", slotName)
	Expect(nodeClaim.Spec.SlotRef.Name).To(Equal(slotName))
	Expect(nodeClaim.Spec.Request.Call).To(Equal(infrav2alpha1.ClaimCallIncreaseSize))
	Expect(nodeClaim.Spec.Request.Group).To(Equal(groupName))
	Expect(nodeClaim.Spec.Request.Delta).To(Equal(delta))
	Expect(nodeClaim.Spec.RequestedBy).NotTo(BeEmpty(), "the claim says who asked")
	Expect(nodeClaim.Spec.RequestedAt.Time).NotTo(BeZero())
	Expect(nodeClaim.Spec.DeadlineAt).NotTo(BeNil(), "the claim is born with a budget")
	Expect(nodeClaim.Labels[claim.LabelSlot]).To(Equal(slotName), "the slot's claims are one label away")
	Expect(nodeClaim.Labels[claim.LabelGroup]).To(Equal(groupName))
}
