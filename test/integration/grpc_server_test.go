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
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	infrav1alpha1 "github.com/homecluster-dev/homelab-autoscaler/api/infra/v1alpha1"
	"github.com/homecluster-dev/homelab-autoscaler/internal/grpcserver"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
	"github.com/homecluster-dev/homelab-autoscaler/test/integration/mocks"
	"github.com/homecluster-dev/homelab-autoscaler/test/integration/testdata"
)

var _ = Describe("gRPC Server Integration Tests", func() {
	var (
		testCtx    context.Context
		testCancel context.CancelFunc
		server     *grpcserver.HomeClusterProviderServer
		loader     *testdata.TestDataLoader
	)

	BeforeEach(func() {
		testCtx, testCancel = context.WithTimeout(ctx, 30*time.Second)
		loader = testdata.NewTestDataLoader(scheme.Scheme)
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

				// Verify specific groups
				groupNames := make([]string, len(resp.NodeGroups))
				for i, group := range resp.NodeGroups {
					groupNames[i] = group.Id
					Expect(group.MinSize).To(Equal(int32(0)))
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

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
				Expect(resp.TargetSize).To(BeNumerically(">=", 0))
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
		Context("when powered off nodes are available", func() {
			It("should power on nodes to increase size", func() {
				// Setup test data with gpu-group that has a powered off node
				mockClient, err := loader.CreateMockClientWithGroup("gpu-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupIncreaseSize endpoint
				req := &pb.NodeGroupIncreaseSizeRequest{
					Id:    "gpu-group",
					Delta: 1,
				}
				resp, err := server.NodeGroupIncreaseSize(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())

				// Verify node was updated to desired power state on
				node := &infrav1alpha1.Node{}
				err = mockClient.Get(testCtx, client.ObjectKey{Name: "gpu-node-2", Namespace: "homelab-autoscaler-system"}, node)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.Spec.DesiredPowerState).To(Equal(infrav1alpha1.PowerStateOn))
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
		Context("when nodes exist and are powered on", func() {
			It("should set nodes to desired power state off", func() {
				// Setup test data
				mockClient, err := loader.CreateMockClientWithGroup("test-group")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

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

				// Verify node was updated to desired power state off
				node := &infrav1alpha1.Node{}
				err = mockClient.Get(testCtx, client.ObjectKey{Name: "test-node-1", Namespace: "homelab-autoscaler-system"}, node)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.Spec.DesiredPowerState).To(Equal(infrav1alpha1.PowerStateOff))
			})
		})

		Context("when nodes do not exist", func() {
			It("should handle non-existent nodes gracefully", func() {
				// Setup empty mock client
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

				// Verify error
				Expect(err).To(HaveOccurred())
				st, ok := status.FromError(err)
				Expect(ok).To(BeTrue())
				Expect(st.Code()).To(Equal(codes.Internal))
			})
		})
	})

	Describe("NodeGroupDecreaseTargetSize endpoint", func() {
		Context("when group has running nodes", func() {
			It("should decrease target size by shutting down nodes", func() {
				// Setup test data with shutting down scenario
				mockClient, err := loader.CreateMockClientWithScenario("shutting-down")
				Expect(err).NotTo(HaveOccurred())
				server = grpcserver.NewHomeClusterProviderServer(mockClient, scheme.Scheme)

				// Call NodeGroupDecreaseTargetSize endpoint
				req := &pb.NodeGroupDecreaseTargetSizeRequest{
					Id:    "cpu-group",
					Delta: -1,
				}
				resp, err := server.NodeGroupDecreaseTargetSize(testCtx, req)

				// Verify response
				Expect(err).NotTo(HaveOccurred())
				Expect(resp).NotTo(BeNil())
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
				Expect(resp.NodeInfo).NotTo(BeNil())
				Expect(resp.NodeInfo.Name).To(ContainSubstring("test-group-template"))
				Expect(resp.NodeInfo.Spec.Unschedulable).To(BeFalse())

				// Verify template node has proper conditions
				Expect(resp.NodeInfo.Status.Conditions).To(HaveLen(5))
				readyCondition := resp.NodeInfo.Status.Conditions[0]
				Expect(readyCondition.Type).To(Equal(corev1.NodeReady))
				Expect(readyCondition.Status).To(Equal(corev1.ConditionTrue))
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
