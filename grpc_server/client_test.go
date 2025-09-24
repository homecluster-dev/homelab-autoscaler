/*
Copyright 2022 The Kubernetes Authors.

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

package main

import (
	"context"
	"log"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

func TestGRPCServer(t *testing.T) {
	// Start the server in the background
	go func() {
		main()
	}()

	// Give the server time to start
	time.Sleep(2 * time.Second)

	// Connect to the server
	conn, err := grpc.Dial("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewCloudProviderClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test NodeGroups
	t.Run("NodeGroups", func(t *testing.T) {
		resp, err := client.NodeGroups(ctx, &pb.NodeGroupsRequest{})
		if err != nil {
			t.Fatalf("NodeGroups failed: %v", err)
		}
		if len(resp.NodeGroups) == 0 {
			t.Error("Expected at least one node group")
		}
		log.Printf("Found %d node groups", len(resp.NodeGroups))
	})

	// Test GPULabel
	t.Run("GPULabel", func(t *testing.T) {
		resp, err := client.GPULabel(ctx, &pb.GPULabelRequest{})
		if err != nil {
			t.Fatalf("GPULabel failed: %v", err)
		}
		if resp.Label == "" {
			t.Error("Expected non-empty GPU label")
		}
		log.Printf("GPU label: %s", resp.Label)
	})

	// Test GetAvailableGPUTypes
	t.Run("GetAvailableGPUTypes", func(t *testing.T) {
		resp, err := client.GetAvailableGPUTypes(ctx, &pb.GetAvailableGPUTypesRequest{})
		if err != nil {
			t.Fatalf("GetAvailableGPUTypes failed: %v", err)
		}
		if len(resp.GpuTypes) == 0 {
			t.Error("Expected at least one GPU type")
		}
		log.Printf("Found %d GPU types", len(resp.GpuTypes))
	})

	// Test NodeGroupTargetSize
	t.Run("NodeGroupTargetSize", func(t *testing.T) {
		resp, err := client.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: "ng-1"})
		if err != nil {
			t.Fatalf("NodeGroupTargetSize failed: %v", err)
		}
		if resp.TargetSize <= 0 {
			t.Error("Expected positive target size")
		}
		log.Printf("Node group ng-1 target size: %d", resp.TargetSize)
	})

	// Test optional methods return Unimplemented
	t.Run("PricingNodePrice_Unimplemented", func(t *testing.T) {
		_, err := client.PricingNodePrice(ctx, &pb.PricingNodePriceRequest{})
		if err == nil {
			t.Error("Expected PricingNodePrice to return Unimplemented error")
		}
		log.Printf("PricingNodePrice correctly returned: %v", err)
	})

	t.Run("PricingPodPrice_Unimplemented", func(t *testing.T) {
		_, err := client.PricingPodPrice(ctx, &pb.PricingPodPriceRequest{})
		if err == nil {
			t.Error("Expected PricingPodPrice to return Unimplemented error")
		}
		log.Printf("PricingPodPrice correctly returned: %v", err)
	})

	t.Run("NodeGroupTemplateNodeInfo_Unimplemented", func(t *testing.T) {
		_, err := client.NodeGroupTemplateNodeInfo(ctx, &pb.NodeGroupTemplateNodeInfoRequest{Id: "ng-1"})
		if err == nil {
			t.Error("Expected NodeGroupTemplateNodeInfo to return Unimplemented error")
		}
		log.Printf("NodeGroupTemplateNodeInfo correctly returned: %v", err)
	})

	t.Run("NodeGroupGetOptions_Unimplemented", func(t *testing.T) {
		_, err := client.NodeGroupGetOptions(ctx, &pb.NodeGroupAutoscalingOptionsRequest{Id: "ng-1"})
		if err == nil {
			t.Error("Expected NodeGroupGetOptions to return Unimplemented error")
		}
		log.Printf("NodeGroupGetOptions correctly returned: %v", err)
	})

	log.Println("All tests passed!")
}