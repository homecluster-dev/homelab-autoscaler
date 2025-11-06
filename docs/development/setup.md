# Development Setup

Set up a development environment for the homelab-autoscaler project.

## Requirements

- Go 1.24.0+
- Docker 17.03+
- kubectl 1.11.3+
- kind or minikube (local cluster)
- make

### Optional Tools
- golangci-lint (code linting)
- grpcurl (gRPC testing)
- k9s (Kubernetes TUI)

## Quick Start

```bash
# Clone and setup
git clone https://github.com/homecluster-dev/homelab-autoscaler.git
cd homelab-autoscaler

go mod download

# Install tools
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest

# Run development setup
make help               # Show all targets
make install            # Install CRDs
make run               # Start controller
```

## Local Cluster Setup

### Using kind
```bash
kind create cluster --name homelab-autoscaler
kubectl cluster-info --context kind-homelab-autoscaler
```

### Using minikube
```bash
minikube start --driver=docker
minikube addons enable metrics-server
```

## Common Development Tasks

### Build and Test
```bash
make build          # Build binary
make test           # Run unit tests
make docker-build   # Build Docker image
```

### Deployment
```bash
make install        # Install CRDs to cluster
make deploy         # Deploy controller
make undeploy       # Remove controller
```

### Running Locally
```bash
# Run controller with development settings
make run

# Or with custom flags
go run cmd/main.go \\
  --metrics-bind-address=:8080 \\
  --leader-elect=false \\
  --enable-grpc-server=true \\
  --grpc-server-address=:50051
```

### Testing gRPC Interface
```bash
kubectl port-forward service/homelab-autoscaler-grpc 50051:50051 &
grpcurl -plaintext localhost:50051 list
grpcurl -plaintext localhost:50051 externalgrpc.CloudProvider/NodeGroups
```

## Project Structure

```
homelab-autoscaler/
├── api/                    # CRD definitions
├── cmd/                    # Main application
├── config/                 # Kubernetes manifests
├── internal/              # Internal packages
│   ├── controller/        # Controllers
│   ├── grpcserver/       # gRPC server
│   └── webhook/          # Admission webhooks
├── proto/                # Protocol buffers
└── test/                 # Test utilities
```

## Key Files

| File | Purpose |
|------|---------|
| `cmd/main.go` | Application entry point |
| `api/infra/v1alpha1/group_types.go` | Group CRD definition |
| `api/infra/v1alpha1/node_types.go` | Node CRD definition |
| `internal/grpcserver/server.go` | gRPC server implementation |
| `Makefile` | Build automation |

## Development Tips

### Debugging
```bash
# Enable verbose logging
go run cmd/main.go --zap-log-level=debug

# Check controller logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager
```

### Testing gRPC
```bash
grpcurl -plaintext localhost:50051 list
grpcurl -plaintext -d '{}' localhost:50051 externalgrpc.CloudProvider/NodeGroups
```

### Working with CRDs
```bash
# Apply CRD changes
make install

# Create test resources
kubectl apply -k config/samples/

# Watch resources
kubectl get groups,nodes.infra.homecluster.dev -w
```

## Known Issues

### Critical Bugs
1. **gRPC Logic Bugs** (HIGH)
   - File: `internal/grpcserver/server.go:306-311`
   - File: `internal/grpcserver/server.go:461-473`

2. **Controller Race Conditions** (HIGH)
   - Use FSM architecture for state management with coordination locks

### Testing Limitations
- Some tests hardcode namespace `homelab-autoscaler-system`

## Contributing

1. Fix existing bugs before adding features
2. Write tests for new functionality
3. Update documentation with changes
4. Follow Go conventions and `gofmt` formatting

## Next Steps

1. Start with debugging critical gRPC bugs
2. Test controller locally with `make run`
3. Create sample resources to test functionality