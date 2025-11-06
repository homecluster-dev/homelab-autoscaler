# Known Issues

This document outlines current known issues, limitations, and workarounds for the homelab-autoscaler system.

## Current Limitations

### 1. Configuration Constraints

#### Advanced gRPC Configuration
- **Location**: `internal/grpcserver/server.go`
- **Description**: Limited customization options for gRPC server behavior
- **Impact**: May require code changes for specific deployment needs
- **Status**: ✅ **STABLE** - Works reliably with default configuration
- **Enhancement**: Additional configuration options planned

#### Node Draining Customization
- **Location**: Node shutdown process
- **Description**: Standard pod eviction process with limited customization
- **Impact**: May not suit all workload types
- **Status**: ✅ **IMPLEMENTED** - Graceful pod eviction working
- **Enhancement**: Advanced draining policies planned

### 2. Operational Considerations

#### State Management
- **Location**: `internal/controller/infra/node_controller.go`
- **Description**: Core FSM implemented, power operations in development
- **Impact**: Basic state transitions work, power operations require completion
- **Status**: 🚧 **PARTIAL** - FSM architecture ready, power operations in development
- **Architecture**: [FSM Architecture](../architecture/state.md) provides robust foundation

#### Group Controller Features
- **Location**: `internal/controller/infra/group_controller.go`
- **Description**: Full autoscaling policy management and health monitoring
- **Impact**: Complete autoscaling functionality
- **Status**: ✅ **OPERATIONAL** - Manages groups and policies effectively
- **Enhancement**: Advanced policy features in development

## Functional Limitations

### 1. Error Handling

#### Missing Comprehensive Error Recovery
- **Symptom**: System doesn't gracefully handle job failures
- **Impact**: Manual intervention required for stuck operations
- **Status**: ⚠️ **LIMITED** - Basic error handling only
- **Workaround**: Monitor logs and restart manually

#### Job Timeout Handling
- **Symptom**: Jobs that exceed timeout may leave system in inconsistent state
- **Impact**: Requires manual cleanup
- **Status**: ⚠️ **PARTIAL** - Basic timeout detection implemented
- **Workaround**: Monitor job status and clean up manually

### 2. Configuration Limitations

#### Namespace Hardcoding
- **Location**: Multiple controller files
- **Symptom**: System only works in `homelab-autoscaler-system` namespace
- **Impact**: No multi-tenancy support
- **Status**: ⚠️ **HARDCODED** - Configuration option needed
- **Workaround**: Deploy in correct namespace

#### Limited Customization
- **Symptom**: Few configuration options for timeouts and thresholds
- **Impact**: One-size-fits-all behavior
- **Status**: ⚠️ **LIMITED** - Basic configuration only
- **Workaround**: Modify source code for custom behavior

### 3. Monitoring and Observability

#### Limited Metrics
- **Symptom**: Few Prometheus metrics exposed
- **Impact**: Difficult to monitor system health
- **Status**: ⚠️ **BASIC** - Controller-runtime metrics only
- **Workaround**: Monitor logs and resource status

#### Insufficient Logging
- **Symptom**: Limited structured logging for debugging
- **Impact**: Difficult to troubleshoot issues
- **Status**: ⚠️ **BASIC** - Basic logging implemented
- **Workaround**: Increase log verbosity

## Development Status Issues

### 1. Testing Coverage

#### Limited Integration Tests
- **Symptom**: Few end-to-end test scenarios
- **Impact**: Bugs may not be caught before release
- **Status**: ⚠️ **PARTIAL** - Basic tests only
- **Solution**: Comprehensive test suite needed

#### Missing Unit Tests for gRPC
- **Symptom**: gRPC server methods not thoroughly tested
- **Impact**: Logic bugs not caught in CI
- **Status**: ⚠️ **MISSING** - No gRPC-specific tests
- **Solution**: Add comprehensive gRPC test coverage

### 2. Documentation Gaps

#### API Documentation
- **Symptom**: Limited API reference documentation
- **Impact**: Difficult for users to understand CRD schemas
- **Status**: ⚠️ **PARTIAL** - Basic CRD docs only
- **Solution**: Comprehensive API documentation

#### Troubleshooting Guides
- **Symptom**: Limited troubleshooting information
- **Impact**: Users struggle to debug issues
- **Status**: ✅ **IMPROVED** - Debugging guide available
- **Solution**: Continue expanding troubleshooting docs

## Workarounds and Mitigation

### For Development and Testing

1. **Use Manual Node Management**
   ```bash
   # Manually set power states instead of relying on gRPC
   kubectl patch node.infra.homecluster.dev <node-name> \
     --type='merge' -p='{"spec":{"powerState":"on"}}'
   ```

2. **Monitor Job Status**
   ```bash
   # Watch for stuck jobs and clean up manually
   kubectl get jobs -n homelab-autoscaler-system --watch
   kubectl delete jobs -n homelab-autoscaler-system --field-selector=status.successful=0
   ```

3. **Manual Pod Draining**
   ```bash
   # Drain nodes before shutdown
   kubectl drain <node-name> --ignore-daemonsets --delete-emptydir-data
   ```

### For Production Deployment

1. **Review Configuration Options**
   - Priority: **MEDIUM**
   - Customize settings for your environment

2. **Implement Monitoring**
   - Priority: **MEDIUM**
   - Set up observability for production operations

3. **Configure Security**
   - Priority: **HIGH**
   - Implement proper RBAC and network policies

4. **Test Scaling Scenarios**
   - Priority: **HIGH**
   - Validate autoscaling behavior in your environment

## Issue Tracking

### How to Report Issues

1. **Check This Document First** - Verify if the issue is already known
2. **Gather Debug Information** - Use the [Debugging Guide](debugging-guide.md)
3. **Create GitHub Issue** - Include logs, configuration, and reproduction steps

### Contributing Fixes

1. **Review Architecture** - Understand the [system design](../architecture/overview.md)
2. **Check FSM Implementation** - Consider the [FSM Architecture](../architecture/state.md) for state management fixes
3. **Add Tests** - Include unit and integration tests with fixes
4. **Update Documentation** - Update this document when issues are resolved

## Related Documentation

- [Debugging Guide](debugging-guide.md) - How to troubleshoot issues
- [Architecture Overview](../architecture/overview.md) - System design and components
- [FSM Architecture](../architecture/state.md) - Planned state management improvements
- [Development Setup](../development/setup.md) - Setting up development environment