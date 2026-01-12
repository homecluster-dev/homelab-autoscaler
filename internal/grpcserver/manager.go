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

package grpcserver

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

// ServerManager manages the lifecycle of the gRPC server
type ServerManager struct {
	server   *grpc.Server
	listener net.Listener
	address  string
	wg       sync.WaitGroup
	client   client.Client
	scheme   *runtime.Scheme
}

// NewServerManager creates a new gRPC server manager
func NewServerManager(address string, k8sClient client.Client, scheme *runtime.Scheme) *ServerManager {
	return &ServerManager{
		address: address,
		client:  k8sClient,
		scheme:  scheme,
	}
}

// Start starts the gRPC server in a goroutine
func (sm *ServerManager) Start(ctx context.Context) error {
	if sm.server != nil {
		return fmt.Errorf("server is already running in namespace %s", namespaceConfig.Get())
	}

	// Create TCP listener
	lis, err := net.Listen("tcp", sm.address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", sm.address, err)
	}
	sm.listener = lis

	// Create gRPC server
	sm.server = grpc.NewServer()

	// Register the reflection service to enable grpcurl and other tools
	reflection.Register(sm.server)

	// Register the cloud provider service
	server := NewHomeClusterProviderServer(sm.client, sm.scheme)
	pb.RegisterCloudProviderServer(sm.server, server)

	log.Printf("Starting CloudProvider gRPC server on %s", sm.address)

	// Start server in a goroutine
	sm.wg.Add(1)
	go func() {
		defer sm.wg.Done()
		log.Printf("gRPC server goroutine starting to serve on %s", sm.address)
		if err := sm.server.Serve(lis); err != nil {
			log.Printf("gRPC server stopped with error: %v", err)
		} else {
			log.Printf("gRPC server stopped normally")
		}
	}()

	return nil
}

// Stop gracefully stops the gRPC server
func (sm *ServerManager) Stop() error {
	if sm.server == nil {
		return fmt.Errorf("server is not running")
	}

	log.Printf("Stopping CloudProvider gRPC server")

	// Graceful stop
	sm.server.GracefulStop()

	// Wait for the server goroutine to finish
	sm.wg.Wait()

	sm.server = nil
	sm.listener = nil

	log.Printf("CloudProvider gRPC server stopped")
	return nil
}

// GetAddress returns the address the server is listening on
func (sm *ServerManager) GetAddress() string {
	if sm.listener != nil {
		return sm.listener.Addr().String()
	}
	return sm.address
}

// IsRunning returns true if the server is currently running
func (sm *ServerManager) IsRunning() bool {
	return sm.server != nil
}
