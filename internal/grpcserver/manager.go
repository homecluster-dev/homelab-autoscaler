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
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/homecluster-dev/homelab-autoscaler/internal/groupstore"
	pb "github.com/homecluster-dev/homelab-autoscaler/proto"
)

// ServerManager manages the lifecycle of the gRPC server
type ServerManager struct {
	server     *grpc.Server
	listener   net.Listener
	address    string
	wg         sync.WaitGroup
	groupStore *groupstore.GroupStore
	client     client.Client
	scheme     *runtime.Scheme
}

// NewServerManager creates a new gRPC server manager
func NewServerManager(address string, groupStore *groupstore.GroupStore, k8sClient client.Client, scheme *runtime.Scheme) *ServerManager {
	return &ServerManager{
		address:    address,
		groupStore: groupStore,
		client:     k8sClient,
		scheme:     scheme,
	}
}

// Start starts the gRPC server in a goroutine
func (sm *ServerManager) Start(ctx context.Context) error {
	if sm.server != nil {
		return fmt.Errorf("server is already running")
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

	// Register the mock cloud provider service
	mockServer := NewMockCloudProviderServer(sm.groupStore, sm.client, sm.scheme)
	pb.RegisterCloudProviderServer(sm.server, mockServer)

	log.Printf("Starting Mock CloudProvider gRPC server on %s", sm.address)

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

	// Wait for server to be ready with better error handling
	// Try to establish a connection to verify the server is ready
	maxRetries := 20              // Increased retries for better reliability
	retryDelay := 1 * time.Second // Increased delay

	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			log.Printf("gRPC server startup cancelled due to context: %v", ctx.Err())
			return ctx.Err()
		default:
			// Try to establish a connection to verify server is ready
			log.Printf("Attempting to verify gRPC server readiness (attempt %d/%d)", i+1, maxRetries)
			conn, err := net.DialTimeout("tcp", sm.listener.Addr().String(), 500*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				log.Printf("Mock CloudProvider gRPC server TCP connection verified as ready on %s", sm.address)

				// Additional verification: wait a bit more to ensure gRPC service is fully registered
				time.Sleep(2 * time.Second)
				log.Printf("Mock CloudProvider gRPC server should be fully ready on %s", sm.address)
				break
			}

			if i < maxRetries-1 {
				log.Printf("gRPC server not ready yet, retrying in %v (attempt %d/%d): %v", retryDelay, i+1, maxRetries, err)
				time.Sleep(retryDelay)
			} else {
				return fmt.Errorf("gRPC server failed to become ready after %d attempts: %v", maxRetries, err)
			}
		}
	}

	log.Printf("Mock CloudProvider gRPC server started successfully on %s", sm.address)
	return nil
}

// Stop gracefully stops the gRPC server
func (sm *ServerManager) Stop() error {
	if sm.server == nil {
		return fmt.Errorf("server is not running")
	}

	log.Printf("Stopping Mock CloudProvider gRPC server")

	// Graceful stop
	sm.server.GracefulStop()

	// Wait for the server goroutine to finish
	sm.wg.Wait()

	sm.server = nil
	sm.listener = nil

	log.Printf("Mock CloudProvider gRPC server stopped")
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
