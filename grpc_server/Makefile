# Makefile for gRPC Server

.PHONY: build run clean test tidy example-client run-example run-server

# Build the gRPC server
build:
	go build -o bin/grpc-server server.go

# Run the gRPC server
run:
	go run server.go

# Run the server using the script
run-server:
	./run_server.sh

# Build and run the example client
example-client:
	go build -o bin/example-client example_client.go

# Run the example client (server must be running)
run-example:
	go run example_client.go

# Clean build artifacts
clean:
	rm -rf bin/

# Run go mod tidy
tidy:
	go mod tidy

# Test the server (basic compilation test)
test:
	go build -o /tmp/grpc-server-test server.go
	rm -f /tmp/grpc-server-test

# Download dependencies
deps:
	go mod download

# Format code
fmt:
	go fmt ./...

# Lint code
lint:
	golangci-lint run

# Help
help:
	@echo "Available targets:"
	@echo "  build         - Build the gRPC server"
	@echo "  run           - Run the gRPC server"
	@echo "  run-server    - Run server using script"
	@echo "  example-client- Build the example client"
	@echo "  run-example   - Run the example client (server must be running)"
	@echo "  clean         - Clean build artifacts"
	@echo "  tidy          - Run go mod tidy"
	@echo "  test          - Test compilation"
	@echo "  deps          - Download dependencies"
	@echo "  fmt           - Format code"
	@echo "  lint          - Run linter"
	@echo "  help          - Show this help"