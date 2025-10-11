# Homelab Autoscaler Documentation

> ⚠️ **CRITICAL WARNING: NOT PRODUCTION READY** ⚠️
> 
> This system contains critical bugs and missing functionality. Do NOT use in production environments.
> See [Known Issues](troubleshooting/known-issues.md) for details.

## Overview

The Homelab Autoscaler is a Kubernetes operator designed to provide cluster autoscaling capabilities for homelab environments with physical nodes. It manages the power state of physical machines based on workload demands, similar to how cloud providers scale virtual instances.

## Current Status

**🚨 DEVELOPMENT PHASE 🚨**

## Documentation Structure

### Getting Started
- [Overview](getting-started/overview.md) - System concepts and design
- [Quick Start](getting-started/quick-start.md) - Fast setup for testing
- [Installation](getting-started/installation.md) - Complete installation guide
- [First Deployment](getting-started/first-deployment.md) - Deploy your first autoscaling group

### Architecture
- [Overview](architecture/overview.md) - High-level system design
- [Components](architecture/components.md) - Detailed component breakdown
- [Controllers](architecture/controllers.md) - Controller responsibilities
- [gRPC Interface](architecture/grpc-interface.md) - External API specification
- [Data Flow](architecture/data-flow.md) - How data moves through the system

### API Reference
- [Group CRD](api-reference/crds/group.md) - Autoscaling group configuration
- [Node CRD](api-reference/crds/node.md) - Physical node management
- [Examples](api-reference/examples/) - Sample configurations

### Troubleshooting
- [Known Issues](troubleshooting/known-issues.md) - **READ THIS FIRST**
- [Common Problems](troubleshooting/common-problems.md) - Frequent issues and solutions
- [Debugging Guide](troubleshooting/debugging-guide.md) - How to debug the system

### Development
- [Setup](development/setup.md) - Development environment setup
- [Architecture Deep Dive](development/architecture-deep-dive.md) - Internal implementation details
- [Contributing](development/contributing.md) - How to contribute

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
| Understand the system | [Architecture Overview](architecture/overview.md) |
| See what's broken | [Known Issues](troubleshooting/known-issues.md) |
| Try it out | [Quick Start](getting-started/quick-start.md) |
| Configure groups | [Group CRD](api-reference/crds/group.md) |
| Debug problems | [Debugging Guide](troubleshooting/debugging-guide.md) |
| Contribute fixes | [Development Setup](development/setup.md) |

## License

Copyright 2025. Licensed under the Apache License, Version 2.0.