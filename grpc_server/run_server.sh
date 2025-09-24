#!/bin/bash

# Script to run the mock gRPC server

echo "Starting Mock Cloud Provider gRPC Server..."
echo "Server will listen on port 50051"
echo ""
echo "To test the server, you can:"
echo "1. Use the example client: go run example_client.go"
echo "2. Use grpcurl commands:"
echo "   grpcurl -plaintext localhost:50051 list"
echo "   grpcurl -plaintext localhost:50051 clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider/NodeGroups"
echo ""
echo "Press Ctrl+C to stop the server"
echo ""

go run server.go