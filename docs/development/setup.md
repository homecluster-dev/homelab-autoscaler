# Development Setup

This guide helps you set up a development environment for the homelab-autoscaler project.

> ⚠️ **WARNING**: This system has critical bugs. See [Known Issues](../troubleshooting/known-issues.md) before starting development.

## Prerequisites

### Required Tools
- **Go 1.24.0+** - Programming language
- **Docker 17.03+** - Container runtime
- **kubectl 1.11.3+** - Kubernetes CLI
- **kind** or **minikube** - Local Kubernetes cluster
- **make** - Build automation

### Optional Tools
- **golangci-lint** - Code linting
- **grpcurl** - gRPC testing
- **k9s** - Kubernetes TUI

## Environment Setup

### 1. Clone Repository

```bash
git clone https://github.com/homecluster-dev/homelab-autoscaler.git
cd homelab-autoscaler
```

### 2. Install Dependencies

```bash
# Download Go modules
go mod download

# Install development tools
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest
```

### 3. Set Up Local Kubernetes Cluster

#### Using kind

```bash
# Create cluster with custom configuration
kind create cluster --config hack/kind-config.yaml --name homelab-autoscaler

# Verify cluster
kubectl cluster-info --context kind-homelab-autoscaler
```

#### Using minikube

```bash
# Start minikube
minikube start --driver=docker

# Enable required addons
minikube addons enable metrics-server
```

## Build and Test

### Available Make Targets

```bash
# Show all available targets
make help

# Common development tasks
make test           # Run unit tests
make build          # Build manager binary
make docker-build   # Build Docker image
make install        # Install CRDs
make deploy         # Deploy to cluster
make undeploy       # Remove from cluster
make uninstall      # Remove CRDs
```

### Running Tests

```bash
# Unit tests
make test

# Integration tests (requires cluster)
make test-integration

# E2E tests (requires cluster and setup)
make test-e2e

# Lint code
golangci-lint run
```

### Building

```bash
# Build manager binary
make build

# Build and push Docker image
make docker-build docker-push IMG=your-registry/homelab-autoscaler:dev
```

## Development Workflow

### 1. Install CRDs

```bash
make install
```

### 2. Run Controller Locally

```bash
# Run with development settings
make run

# Or run with custom flags
go run cmd/main.go \
  --metrics-bind-address=:8080 \
  --leader-elect=false \
  --enable-grpc-server=true \
  --grpc-server-address=:50051
```

### 3. Deploy Sample Resources

```bash
# Apply sample configurations
kubectl apply -k config/samples/
```

### 4. Test gRPC Interface

```bash
# Port forward to gRPC server
kubectl port-forward service/homelab-autoscaler-grpc 50051:50051 &

# Test gRPC methods
grpcurl -plaintext localhost:50051 list
grpcurl -plaintext localhost:50051 externalgrpc.CloudProvider/NodeGroups
```

## Project Structure

```
homelab-autoscaler/
├── api/                    # CRD definitions
│   └── infra/v1alpha1/    # API version v1alpha1
├── cmd/                   # Main application
├── config/                # Kubernetes manifests
│   ├── crd/              # CRD installations
│   ├── default/          # Default deployment
│   ├── manager/          # Controller manager
│   ├── rbac/             # RBAC configurations
│   └── samples/          # Example resources
├── docs/                 # Documentation
├── hack/                 # Development scripts
├── internal/             # Internal packages
│   ├── controller/       # Controllers
│   │   ├── core/        # Core Kubernetes controllers
│   │   └── infra/       # Infrastructure controllers
│   ├── grpcserver/      # gRPC server implementation
│   └── webhook/         # Admission webhooks
├── proto/               # Protocol buffer definitions
└── test/                # Test utilities and E2E tests
```

## Key Files

| File | Purpose |
|------|---------|
| [`cmd/main.go`](../../cmd/main.go) | Application entry point |
| [`api/infra/v1alpha1/group_types.go`](../../api/infra/v1alpha1/group_types.go) | Group CRD definition |
| [`api/infra/v1alpha1/node_types.go`](../../api/infra/v1alpha1/node_types.go) | Node CRD definition |
| [`internal/grpcserver/server.go`](../../internal/grpcserver/server.go) | gRPC server implementation |
| [`Makefile`](../../Makefile) | Build automation |
| [`PROJECT`](../../PROJECT) | Kubebuilder project file |

## Development Tips

### Debugging Controllers

```bash
# Enable debug logging
export KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use --print path)

# Run with verbose logging
go run cmd/main.go --zap-log-level=debug
```

### Testing gRPC Methods

```bash
# List available services
grpcurl -plaintext localhost:50051 list

# Call specific methods
grpcurl -plaintext -d '{}' localhost:50051 externalgrpc.CloudProvider/NodeGroups

# Test with data
grpcurl -plaintext -d '{"id": "test-group"}' localhost:50051 externalgrpc.CloudProvider/NodeGroupTargetSize
```

### Working with CRDs

```bash
# Apply CRD changes
make install

# Create test resources
kubectl apply -f config/samples/

# Watch resource changes
kubectl get groups -w
kubectl get nodes.infra.homecluster.dev -w
```

## Known Development Issues

### Critical Bugs to Fix First

1. **gRPC Logic Bugs** (Priority: CRITICAL)
   - File: [`internal/grpcserver/server.go:306-311`](../../internal/grpcserver/server.go:306)
   - File: [`internal/grpcserver/server.go:461-473`](../../internal/grpcserver/server.go:461)

2. **GroupStore Not Used** (Priority: HIGH)
   - File: [`cmd/main.go:186`](../../cmd/main.go:186)
   - GroupStore created but never passed to gRPC server

3. **Controller Race Conditions** (Priority: HIGH)
   - Multiple controllers accessing same resources without proper coordination

### Testing Limitations

- E2E tests require VirtualBox setup (see [`test/virtualbox/`](../../test/virtualbox/))
- Some tests hardcode namespace `homelab-autoscaler-system`
- gRPC tests may fail due to logic bugs

### Debugging Commands

```bash
# Check controller logs
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager

# Check CRD status
kubectl get crds | grep homecluster

# Describe resources
kubectl describe group <group-name>
kubectl describe node.infra.homecluster.dev <node-name>

# Check gRPC server status
kubectl get pods -n homelab-autoscaler-system
kubectl port-forward -n homelab-autoscaler-system pod/<pod-name> 50051:50051
```

## Contributing

### Before Making Changes

1. **Read Known Issues**: Understand current limitations
2. **Write Tests**: Add tests for new functionality
3. **Fix Critical Bugs**: Prioritize fixing existing bugs
4. **Update Documentation**: Keep docs in sync with changes

### Code Style

- Follow Go conventions and `gofmt` formatting
- Use `golangci-lint` for code quality
- Add comments for exported functions
- Include error handling and logging

### Submitting Changes

1. Create feature branch from main
2. Make changes with tests
3. Run full test suite
4. Update documentation
5. Submit pull request

## Useful Resources

- [Kubebuilder Documentation](https://book.kubebuilder.io/)
- [Controller Runtime](https://pkg.go.dev/sigs.k8s.io/controller-runtime)
- [Kubernetes API Conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
- [gRPC Go Documentation](https://grpc.io/docs/languages/go/)

## Related Documentation

- [Architecture Overview](../architecture/overview.md) - System design
- [Known Issues](../troubleshooting/known-issues.md) - Current bugs and limitations
- [API Reference](../api-reference/crds/group.md) - CRD specifications