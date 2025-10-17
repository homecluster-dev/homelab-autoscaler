---
title: Changelog
description: Release history and changes for Homelab Autoscaler
sidebar_position: 1
---

# Changelog

All notable changes to the Homelab Autoscaler project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Current Version

### [v0.1.10] - 2024-10-17

**Current stable release**

#### Installation

```bash
# Add the Helm repository
helm repo add homelab-autoscaler https://autoscaler.homecluster.dev
helm repo update

# Install the chart
helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler --version 0.1.10
```

#### What's New
- Latest stable release with improved functionality
- Enhanced documentation and examples
- Bug fixes and performance improvements

---

## Release History



---

## Installation Instructions

### Prerequisites
- Kubernetes cluster (v1.20+)
- Helm 3.x
- kubectl configured to access your cluster

### Quick Installation

1. **Add the Helm repository:**
   ```bash
   helm repo add homelab-autoscaler https://autoscaler.homecluster.dev
   helm repo update
   ```

2. **Install the latest version:**
   ```bash
   helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler
   ```

3. **Install a specific version:**
   ```bash
   helm install homelab-autoscaler homelab-autoscaler/homelab-autoscaler --version 0.1.10
   ```

### Configuration

For detailed configuration options, see the [Installation Guide](../getting-started/installation.md).

### Upgrade Instructions

To upgrade to the latest version:

```bash
helm repo update
helm upgrade homelab-autoscaler homelab-autoscaler/homelab-autoscaler
```

To upgrade to a specific version:

```bash
helm upgrade homelab-autoscaler homelab-autoscaler/homelab-autoscaler --version 0.1.10
```

## Breaking Changes

### v0.1.x Series
- No breaking changes in the current series
- All releases are backward compatible

## Links

- [GitHub Repository](https://github.com/homecluster-dev/homelab-autoscaler)
- [GitHub Releases](https://github.com/homecluster-dev/homelab-autoscaler/releases)
- [Helm Chart Repository](https://autoscaler.homecluster.dev)
- [Documentation](https://autoscaler.homecluster.dev/docs)

## Support

If you encounter any issues:

1. Check the [Troubleshooting Guide](../troubleshooting/debugging-guide.md)
2. Review [Known Issues](../troubleshooting/known-issues.md)
3. Search existing [GitHub Issues](https://github.com/homecluster-dev/homelab-autoscaler/issues)
4. Create a new issue if needed

---

*This changelog is automatically updated when new versions are released.*