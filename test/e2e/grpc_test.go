//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

var _ = Describe("gRPC Server", Ordered, func() {
	var (
		grpcClient pb.CloudProviderClient
		grpcConn   *grpc.ClientConn
	)

	BeforeAll(func() {
		By("creating gRPC client for test suite")
		// Assume port forwarding is handled externally by Makefile
		// Connect to localhost:50051 where the gRPC server should be accessible
		conn, err := grpc.Dial("localhost:50051",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
			grpc.WithTimeout(10*time.Second))
		Expect(err).NotTo(HaveOccurred(), "Failed to connect to gRPC server")
		grpcConn = conn
		grpcClient = pb.NewCloudProviderClient(conn)

		// Verify the gRPC service is responding
		By("verifying gRPC server is accessible")
		Eventually(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, err := grpcClient.GPULabel(ctx, &pb.GPULabelRequest{})
			if err != nil {
				return err
			}

			GinkgoWriter.Printf("gRPC server verified as ready\n")
			return nil
		}, 60*time.Second, 2*time.Second).Should(Succeed(), "gRPC server should be accessible")
	})

	AfterAll(func() {
		By("cleaning up gRPC client connection")
		if grpcConn != nil {
			grpcConn.Close()
		}
	})

	Context("gRPC Server Endpoints", func() {
		It("should respond to NodeGroups request with real Group data from separate Node CRs", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Wait for groups to be processed and health checks to be established
			// The new architecture uses separate Node CRs that are linked to Groups via labels
			By("waiting for at least 1 healthy group to be available via gRPC")
			Eventually(func() int {
				resp, err := grpcClient.NodeGroups(ctx, &pb.NodeGroupsRequest{})
				if err != nil {
					GinkgoWriter.Printf("gRPC NodeGroups call failed: %v\n", err)
					return 0
				}
				GinkgoWriter.Printf("gRPC NodeGroups returned %d groups\n", len(resp.NodeGroups))
				return len(resp.NodeGroups)
			}, 2*time.Minute, 5*time.Second).Should(BeNumerically(">=", 1), "Expected at least 1 healthy group")

			// Get the final response
			resp, err := grpcClient.NodeGroups(ctx, &pb.NodeGroupsRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.NodeGroups).NotTo(BeEmpty())

			// Verify that the NodeGroups have the correct format
			for _, ng := range resp.NodeGroups {
				Expect(ng.Id).NotTo(BeEmpty(), "NodeGroup ID should not be empty")
				Expect(ng.MinSize).To(Equal(int32(0)), "NodeGroup MinSize should be 0")
				Expect(ng.MaxSize).To(BeNumerically(">", 0), "NodeGroup MaxSize should be greater than 0")

				// Verify that the NodeGroup corresponds to one of our test groups
				Expect(ng.Id).To(BeElementOf([]string{"group1"}),
					fmt.Sprintf("NodeGroup ID %s should be one of our test groups", ng.Id))

				// Verify MaxSize matches the Group spec
				if ng.Id == "group1" {
					Expect(ng.MaxSize).To(Equal(int32(5)), "group1 should have MaxSize=5")
				}
			}
		})

		It("should return all groups regardless of health status with separate Node CRs", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// First, verify we have groups available
			Eventually(func() int {
				resp, err := grpcClient.NodeGroups(ctx, &pb.NodeGroupsRequest{})
				if err != nil {
					return 0
				}
				return len(resp.NodeGroups)
			}, 2*time.Minute, 5*time.Second).Should(BeNumerically(">=", 1), "Expected at least 1 group initially")

			// Get the current response
			resp, err := grpcClient.NodeGroups(ctx, &pb.NodeGroupsRequest{})
			Expect(err).NotTo(HaveOccurred())

			// Verify we have the expected groups (the gRPC server now returns all groups without health filtering)
			groupIDs := []string{}
			for _, ng := range resp.NodeGroups {
				groupIDs = append(groupIDs, ng.Id)
			}
			Expect(groupIDs).To(ContainElement("group1"), "group1 should be present")

		})

		It("should respond to GPULabel request", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			resp, err := grpcClient.GPULabel(ctx, &pb.GPULabelRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Label).To(Equal("accelerator"))
		})

		It("should respond to GetAvailableGPUTypes request", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			resp, err := grpcClient.GetAvailableGPUTypes(ctx, &pb.GetAvailableGPUTypesRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GpuTypes).NotTo(BeEmpty())
			Expect(resp.GpuTypes).To(HaveKey("nvidia-tesla-v100"))
			Expect(resp.GpuTypes).To(HaveKey("nvidia-tesla-t4"))
		})

		It("should respond to NodeGroupTargetSize request", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			resp, err := grpcClient.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: "ng-1"})
			Expect(err).NotTo(HaveOccurred())
			// TargetSize can be 0, just verify the response is valid
			Expect(resp.TargetSize).To(BeNumerically(">=", 0))
		})

		It("should respond to NodeGroupNodes request", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			resp, err := grpcClient.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: "ng-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			// Instances can be empty or nil, just verify we get a response
		})

		It("should handle NodeGroupDeleteNodes correctly", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Call NodeGroupDeleteNodes with group1 and group1-worker-node-1
			_, err := grpcClient.NodeGroupDeleteNodes(ctx, &pb.NodeGroupDeleteNodesRequest{
				Id: "group1",
				Nodes: []*pb.ExternalGrpcNode{
					{
						Name: "group1-worker-node-1",
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify that the Kubernetes node "worker1.k8s.local" is marked as unschedulable
			Eventually(func() bool {
				// Use kubectl to check if the node is unschedulable
				cmd := exec.Command("kubectl", "get", "node", "worker1.k8s.local", "-o", "jsonpath={.spec.unschedulable}")
				output, err := cmd.CombinedOutput()
				if err != nil {
					GinkgoWriter.Printf("Failed to get node status: %v, output: %s\n", err, string(output))
					return false
				}
				return string(output) == "true"
			}, 30*time.Second, 2*time.Second).Should(BeTrue(), "Kubernetes node worker1.k8s.local should be unschedulable")
		})

		It("should handle NodeGroupIncreaseSize correctly", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Increase size of group1 which had a node deleted in the previous test
			_, err := grpcClient.NodeGroupIncreaseSize(ctx, &pb.NodeGroupIncreaseSizeRequest{
				Id:    "group1",
				Delta: 1,
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify that the Kubernetes node "worker1.k8s.local" becomes schedulable again
			Eventually(func() bool {
				// Use kubectl to check if the node is schedulable
				cmd := exec.Command("kubectl", "get", "node", "worker1.k8s.local", "-o", "jsonpath={.spec.unschedulable}")
				output, err := cmd.CombinedOutput()
				if err != nil {
					GinkgoWriter.Printf("Failed to get node status: %v, output: %s\n", err, string(output))
					return false
				}
				// If unschedulable is empty or false, the node is schedulable
				return string(output) == "" || string(output) == "false"
			}, 30*time.Second, 2*time.Second).Should(BeTrue(), "Kubernetes node worker1.k8s.local should be schedulable")
		})

		It("should handle NodeGroupDecreaseTargetSize correctly", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Try to decrease size of non-existent node group - expect NotFound error
			_, err := grpcClient.NodeGroupDecreaseTargetSize(ctx, &pb.NodeGroupDecreaseTargetSizeRequest{
				Id:    "ng-1",
				Delta: -1,
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("node group ng-1 not found"))
		})

		It("should handle NodeGroupForNode correctly", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Create a test node
			testNode := &pb.ExternalGrpcNode{
				Name: "node-1",
			}

			// Try to find node group for non-existent node - expect empty response
			resp, err := grpcClient.NodeGroupForNode(ctx, &pb.NodeGroupForNodeRequest{Node: testNode})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.NodeGroup).NotTo(BeNil())
			// Node group ID can be empty since the node doesn't exist in any group
			Expect(resp.NodeGroup.Id).To(BeEmpty())
		})

	})
})
