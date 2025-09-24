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
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

func runExampleClient() {
	// Connect to the mock server
	conn, err := grpc.Dial("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewCloudProviderClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Println("=== Mock Cloud Provider gRPC Client Example ===")
	fmt.Println()

	// 1. Get all node groups
	fmt.Println("1. Getting all node groups...")
	nodeGroupsResp, err := client.NodeGroups(ctx, &pb.NodeGroupsRequest{})
	if err != nil {
		log.Fatalf("NodeGroups failed: %v", err)
	}
	fmt.Printf("   Found %d node groups:\n", len(nodeGroupsResp.NodeGroups))
	for _, ng := range nodeGroupsResp.NodeGroups {
		fmt.Printf("   - %s (min: %d, max: %d)\n", ng.Id, ng.MinSize, ng.MaxSize)
	}
	fmt.Println()

	// 2. Get GPU label
	fmt.Println("2. Getting GPU label...")
	gpuLabelResp, err := client.GPULabel(ctx, &pb.GPULabelRequest{})
	if err != nil {
		log.Fatalf("GPULabel failed: %v", err)
	}
	fmt.Printf("   GPU label: %s\n", gpuLabelResp.Label)
	fmt.Println()

	// 3. Get available GPU types
	fmt.Println("3. Getting available GPU types...")
	gpuTypesResp, err := client.GetAvailableGPUTypes(ctx, &pb.GetAvailableGPUTypesRequest{})
	if err != nil {
		log.Fatalf("GetAvailableGPUTypes failed: %v", err)
	}
	fmt.Printf("   Found %d GPU types:\n", len(gpuTypesResp.GpuTypes))
	for gpuType := range gpuTypesResp.GpuTypes {
		fmt.Printf("   - %s\n", gpuType)
	}
	fmt.Println()

	// 4. Get node group target sizes
	fmt.Println("4. Getting node group target sizes...")
	for _, ng := range nodeGroupsResp.NodeGroups {
		targetSizeResp, err := client.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: ng.Id})
		if err != nil {
			log.Printf("   Error getting target size for %s: %v", ng.Id, err)
			continue
		}
		fmt.Printf("   - %s: target size = %d\n", ng.Id, targetSizeResp.TargetSize)
	}
	fmt.Println()

	// 5. Get nodes in a node group
	if len(nodeGroupsResp.NodeGroups) > 0 {
		firstNG := nodeGroupsResp.NodeGroups[0]
		fmt.Printf("5. Getting nodes in node group %s...\n", firstNG.Id)
		nodesResp, err := client.NodeGroupNodes(ctx, &pb.NodeGroupNodesRequest{Id: firstNG.Id})
		if err != nil {
			log.Fatalf("NodeGroupNodes failed: %v", err)
		}
		fmt.Printf("   Found %d instances:\n", len(nodesResp.Instances))
		for _, instance := range nodesResp.Instances {
			fmt.Printf("   - %s (state: %v)\n", instance.Id, instance.Status.InstanceState)
		}
	}
	fmt.Println()

	// 6. Test optional methods (should return Unimplemented)
	fmt.Println("6. Testing optional methods (should return Unimplemented)...")
	
	_, err = client.PricingNodePrice(ctx, &pb.PricingNodePriceRequest{})
	if err != nil {
		fmt.Printf("   PricingNodePrice: %v ✓\n", err)
	} else {
		fmt.Println("   PricingNodePrice: Expected error but got success ✗")
	}

	_, err = client.PricingPodPrice(ctx, &pb.PricingPodPriceRequest{})
	if err != nil {
		fmt.Printf("   PricingPodPrice: %v ✓\n", err)
	} else {
		fmt.Println("   PricingPodPrice: Expected error but got success ✗")
	}

	_, err = client.NodeGroupTemplateNodeInfo(ctx, &pb.NodeGroupTemplateNodeInfoRequest{Id: "ng-1"})
	if err != nil {
		fmt.Printf("   NodeGroupTemplateNodeInfo: %v ✓\n", err)
	} else {
		fmt.Println("   NodeGroupTemplateNodeInfo: Expected error but got success ✗")
	}

	_, err = client.NodeGroupGetOptions(ctx, &pb.NodeGroupAutoscalingOptionsRequest{Id: "ng-1"})
	if err != nil {
		fmt.Printf("   NodeGroupGetOptions: %v ✓\n", err)
	} else {
		fmt.Println("   NodeGroupGetOptions: Expected error but got success ✗")
	}
	fmt.Println()

	// 7. Test scaling operations
	fmt.Println("7. Testing scaling operations...")
	
	// Increase size
	if len(nodeGroupsResp.NodeGroups) > 0 {
		firstNG := nodeGroupsResp.NodeGroups[0]
		fmt.Printf("   Increasing %s size by 1...\n", firstNG.Id)
		_, err = client.NodeGroupIncreaseSize(ctx, &pb.NodeGroupIncreaseSizeRequest{
			Id:    firstNG.Id,
			Delta: 1,
		})
	if err != nil {
		fmt.Printf("   Error: %v\n", err)
	} else {
		fmt.Println("   Success ✓")
		
		// Check new size
			newSizeResp, err := client.NodeGroupTargetSize(ctx, &pb.NodeGroupTargetSizeRequest{Id: firstNG.Id})
			if err != nil {
				fmt.Printf("   Error checking new size: %v\n", err)
			} else {
				fmt.Printf("   New target size: %d\n", newSizeResp.TargetSize)
			}
		}
	}
	fmt.Println()

	fmt.Println("=== Example completed successfully! ===")
}