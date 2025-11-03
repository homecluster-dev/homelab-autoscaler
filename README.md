# Homelab Autoscaler

## Overview

The Homelab Autoscaler is a Kubernetes operator that provides sophisticated cluster autoscaling capabilities for homelab environments with physical nodes. It manages the power state of physical machines based on workload demands, featuring advanced FSM-based state management, comprehensive testing infrastructure, and complete Cluster Autoscaler CloudProvider interface implementation.

## Current Status

**under active development**

### Key Features

- **Advanced FSM Architecture**: Sophisticated finite state machine using looplab/fsm library for robust node state management with coordination locks
- **Complete CloudProvider Interface**: Full implementation of all 15 Cluster Autoscaler gRPC methods for seamless integration
- **Webhook Validation System**: Comprehensive validation webhooks for CRDs ensuring data integrity
- **Helm Chart Deployment**: Helm chart with automated CRD synchronization
- **Comprehensive Testing**: Unit, integration, and e2e tests with k3d for reliable operation
- **Coordination Locks**: Advanced locking mechanisms preventing race conditions during state transitions

## Documentation Structure

### Getting Started
- [Overview](docs/getting-started/overview.md) - System concepts and design

### Architecture
- [Overview](docs/architecture/overview.md) - High-level system design
- [FSM State Management](docs/architecture/state.md) - Comprehensive finite state machine architecture and implementation

### API Reference
- [Group CRD](docs/api-reference/crds/group.md) - Autoscaling group configuration
- [Node CRD](docs/api-reference/crds/node.md) - Physical node management
- [Examples](docs/api-reference/examples/) - Sample configurations

### Troubleshooting
- [Debugging Guide](docs/troubleshooting/debugging-guide.md) - How to debug the system

### Development
- [Setup](docs/development/setup.md) - Development environment setup
- [CRD Sync](docs/development/crd-sync.md) - Helm chart CRD synchronization process

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
- **Group Controller**: Manages Group CRDs and autoscaling policies
- **Node Controller**: Manages Node CRDs with sophisticated FSM-based power state transitions
- **Core Controller**: Handles Kubernetes node lifecycle events

### gRPC Server
Complete implementation of the Cluster Autoscaler CloudProvider interface with all 15 required methods for seamless integration with the Kubernetes Cluster Autoscaler.

### Finite State Machine
Advanced state management using looplab/fsm library with:
- Formal state transitions (Shutdown → StartingUp → Ready → ShuttingDown)
- Coordination lock integration preventing race conditions
- Smart backoff strategies with timeout handling
- Comprehensive error recovery mechanisms

### Webhook Validation
Validation webhooks ensuring:
- Group label validation for Node CRDs
- Kubernetes node existence verification
- Data integrity across all custom resources

### Deployment Options
- **Helm Chart**: deployment with automated CRD sync
- **Development Mode**: Local development setup with k3d integration
- **Manual Deployment**: Direct kubectl application of manifests

## Quick Navigation

| I want to... | Go to... |
|--------------|----------|
| Understand the system | [Architecture Overview](docs/architecture/overview.md) |
| Learn about FSM architecture | [State Management](docs/architecture/state.md) |
| Configure groups | [Group CRD](docs/api-reference/crds/group.md) |
| Configure nodes | [Node CRD](docs/api-reference/crds/node.md) |
| Debug problems | [Debugging Guide](docs/troubleshooting/debugging-guide.md) |
| Set up development | [Development Setup](docs/development/setup.md) |
| Deploy with Helm | [Examples](examples/) |

## License

Copyright 2025. Licensed under the Apache License, Version 2.0.