# Homelab Autoscaler Documentation

## Overview

The Homelab Autoscaler is a Kubernetes operator designed to provide cluster autoscaling capabilities for homelab environments with physical node s. It manages the power state of physical machines based on workload demands, similar to how cloud providers scale virtual instances, without actually provisioning/deprovissioning nodes for the cluster.

## Current Status

**🚨 DEVELOPMENT PHASE 🚨**

## Documentation Structure

### Getting Started
- [Overview](docs/getting-started/overview.md) - System concepts and design
- [Quick Start](docs/getting-started/quick-start.md) - Fast setup for testing
- [Installation](docs/getting-started/installation.md) - Complete installation guide
- [First Deployment](docs/getting-started/first-deployment.md) - Deploy your first autoscaling group

### Architecture
- [Overview](docs/architecture/overview.md) - High-level system design
- [Components](docs/architecture/components.md) - Detailed component breakdown
- [Controllers](docs/architecture/controllers.md) - Controller responsibilities
- [gRPC Interface](docs/architecture/grpc-interface.md) - External API specification
- [Data Flow](docs/architecture/data-flow.md) - How data moves through the system

### API Reference
- [Group CRD](docs/api-reference/crds/group.md) - Autoscaling group configuration
- [Node CRD](docs/api-reference/crds/node.md) - Physical node management
- [Examples](docs/api-reference/examples/) - Sample configurations

### Troubleshooting
- [Debugging Guide](docs/troubleshooting/debugging-guide.md) - How to debug the system

### Development
- [Setup](docs/development/setup.md) - Development environment setup
- [Architecture Deep Dive](docs/development/architecture-deep-dive.md) - Internal implementation details
- [Contributing](docs/development/contributing.md) - How to contribute

## Key Concepts

### Groups
Define autoscaling policies and node selection criteria. Groups specify:
- Maximum node count
- Scaling thresholds
- Node selection labels
- Timing parameters

### Nodes
Represent physical machines with:
- Power state management
- Startup/shutdown job specifications
- Health monitoring
- Pricing information

### Controllers
- **Group Controller**: Manages Group CRDs (currently incomplete)
- **Node Controller**: Manages Node CRDs and power states
- **Core Controller**: Handles Kubernetes node lifecycle

### gRPC Server
Implements the Cluster Autoscaler CloudProvider interface for external integration.

## Quick Navigation

| I want to... | Go to... |
|--------------|----------|
| Understand the system | [Architecture Overview](docs/architecture/overview.md) |
| Try it out | [Quick Start](docs/getting-started/quick-start.md) |
| Configure groups | [Group CRD](docs/api-reference/crds/group.md) |
| Debug problems | [Debugging Guide](docs/troubleshooting/debugging-guide.md) |
| Contribute fixes | [Development Setup](docs/development/setup.md) |

## License

Copyright 2025. Licensed under the Apache License, Version 2.0.