# Homelab Autoscaler

A Kubernetes operator that manages physical node power states for energy-efficient homelab clusters. Powers nodes on during high demand, shuts them down during idle periods.

## 📦 Current Status

**Core Infrastructure Ready** - Partial autoscaling functionality operational

✅ **What Works:**
- Group management and autoscaling policies
- Kubernetes node ↔ custom resource synchronization
- Basic gRPC CloudProvider interface methods
- Complete deployment via Helm with automated CRDs

🚧 **In Development:**
- Full Node power operations (startup/shutdown jobs)
- Complete Cluster Autoscaler integration
- Advanced error recovery and monitoring

📋 **See [Implementation Status](docs/IMPLEMENTATION-STATUS.md) for detailed status**

## 🚀 Quick Start

### Prerequisites
- Kubernetes cluster (k3d, kind, or production)
- kubectl and helm installed
- Physical nodes with power management (IPMI, WoL, etc.)

### Installation
```bash
# Install CRDs
make install

# Deploy to cluster
make deploy

# Or run locally for development
make run
```

### Basic Configuration
```yaml
# Example Group CRD - manages physical node autoscaling
apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: worker-nodes
spec:
  maxSize: 5
  scaleUpThreshold: 70    # CPU% to trigger scale-up
  scaleDownThreshold: 30  # CPU% to trigger scale-down
  scaleDownDelay: 10m     # Wait before scaling down
```

```yaml
# Example Node CRD - represents a physical machine
apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: node-1
  labels:
    group: worker-nodes
spec:
  powerState: off        # Desired state (on/off)
  startupJob:
    template:
      spec:
        containers:
        - name: power-on
          image: ipmitool-image
          command: ["ipmitool", "-H", "bmc-host", "power", "on"]
```

## 🏗️ Architecture

```
Cluster Autoscaler ↔ gRPC Server (CloudProvider) ↔ Kubernetes API ↔ Controllers ↔ Physical Nodes
                      │
                      └── Custom Resources (Groups + Nodes)
```

### Key Components
- **Group Controller**: Manages autoscaling policies and group health
- **Node Controller**: Handles power state transitions via Kubernetes Jobs
- **Core Controller**: Syncs Kubernetes nodes with custom resources
- **gRPC Server**: Implements standard Cluster Autoscaler interface

## 🔧 Development

### Build and Test
```bash
make build    # Build manager binary
make test     # Run unit tests
make lint     # Run golangci-lint
make fmt      # Format code

# End-to-end testing with k3d
make test-e2e
```

### Code Structure
```
├── api/                 # CRD definitions
├── cmd/                 # Main entry point
├── config/              # Kubernetes manifests
├── internal/
│   ├── controller/      # Group, Node, Core controllers
│   ├── grpcserver/      # CloudProvider interface
│   ├── fsm/             # Finite state machine
│   └── webhook/         # Admission webhooks
├── proto/               # gRPC protocol definitions
└── test/                # Test utilities
```

## 📚 Documentation

- **[Implementation Status](docs/IMPLEMENTATION-STATUS.md)** - Authoritative feature status
- **[Quick Start](docs/getting-started/quick-start.md)** - k3d testing setup
- **[Architecture](docs/architecture/overview.md)** - System design and components
- **[API Reference](docs/api-reference/crds/group.md)** - CRD specifications
- **[Troubleshooting](docs/troubleshooting/debugging-guide.md)** - Debugging guide

## ⚠️ Known Limitations

- Currently requires manual intervention for failed operations
- Limited configuration options for advanced use cases
- Basic monitoring and metrics only
- Single namespace support (homelab-autoscaler-system)

## 🤝 Contributing

1. Check [open issues](https://github.com/homecluster-dev/homelab-autoscaler/issues)
2. Follow the [development guide](docs/development/setup.md)
3. Run pre-commit validation: `make pre-commit`
4. Ensure tests pass before submitting PRs

## 📄 License

Apache License 2.0 - See [LICENSE](LICENSE) for details.