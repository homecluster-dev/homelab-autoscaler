# Integration Tests

This directory contains comprehensive integration tests for the homelab-autoscaler gRPC server implementation. These tests validate all gRPC endpoints with realistic test data and mock Kubernetes clients.

## Overview

The integration tests verify the complete functionality of the gRPC server that implements the CloudProvider interface for the cluster-autoscaler. All 13 gRPC endpoints are tested with both success and error scenarios.

## Test Structure

### Test Files

- [`integration_suite_test.go`](integration_suite_test.go) - Ginkgo test suite setup with envtest environment
- [`grpc_server_test.go`](grpc_server_test.go) - Main test file with all gRPC endpoint tests
- [`mocks/`](mocks/) - Mock Kubernetes client implementation
- [`testdata/`](testdata/) - Comprehensive test data in YAML format

### Test Data

The test data includes realistic homelab configurations:

- **8 Groups**: Various configurations (GPU, CPU, development, etc.)
- **12 Nodes**: Different power states, health statuses, and scenarios
- **12 Kubernetes Nodes**: Corresponding core/v1 Node objects

#### Test Scenarios

| Scenario | Description | Groups/Nodes |
|----------|-------------|--------------|
| `basic` | Standard healthy nodes | test-group (3 nodes) |
| `gpu-workload` | High-performance GPU nodes | gpu-group (2 nodes) |
| `cpu-workload` | CPU-focused compute nodes | cpu-group (2 nodes) |
| `offline-nodes` | Nodes with connectivity issues | offline-group (1 node) |
| `transitioning-nodes` | Nodes in startup/shutdown | unknown-group (1 node) |
| `scale-from-zero` | Empty group for scaling tests | empty-group (0 nodes) |
| `fast-scaling` | High-performance quick scaling | fast-scale-group (1 node) |
| `development` | Development environment | dev-group (1 node) |
| `scale-up-candidate` | Powered-off nodes for scale-up | gpu-group (gpu-node-2) |
| `shutting-down` | Nodes being shut down | cpu-group (cpu-node-2) |
| `orphaned-node` | Nodes without group assignment | orphaned-node-1 |

## Running Tests

### Prerequisites

```bash
# Install envtest binaries
make setup-envtest
```

### Execute Tests

```bash
# Run all integration tests
go test ./test/integration/... -v

# Run specific test patterns
go test ./test/integration/... -v -ginkgo.focus="NodeGroups"

# Run with coverage
go test ./test/integration/... -v -coverprofile=coverage.out
```

### Expected Output

```
Running Suite: Integration Suite
=================================
Random Seed: 1760434836

Will run 29 of 29 specs
•••••••••••••••••••••••••••••

Ran 29 of 29 Specs in 3.936 seconds
SUCCESS! -- 29 Passed | 0 Failed | 0 Pending | 0 Skipped
```

## Test Coverage

### gRPC Endpoints Tested

| Endpoint | Test Cases | Coverage |
|----------|------------|----------|
| `NodeGroups` | 2 | ✅ All groups, empty cluster |
| `NodeGroupForNode` | 3 | ✅ Found, orphaned, non-existent |
| `NodeGroupTargetSize` | 2 | ✅ Valid group, non-existent |
| `NodeGroupIncreaseSize` | 3 | ✅ Scale up, no nodes, invalid delta |
| `NodeGroupDeleteNodes` | 2 | ✅ Delete nodes, non-existent |
| `NodeGroupDecreaseTargetSize` | 2 | ✅ Scale down, invalid delta |
| `NodeGroupNodes` | 2 | ✅ List instances, non-existent group |
| `NodeGroupTemplateNodeInfo` | 2 | ✅ Template info, scale-from-zero |
| `NodeGroupGetOptions` | 2 | ✅ Autoscaling options, non-existent |
| `PricingNodePrice` | 3 | ✅ Valid pricing, non-existent, invalid |
| `GPULabel` | 1 | ✅ Returns GPU label |
| `GetAvailableGPUTypes` | 1 | ✅ Returns GPU types |
| `Cleanup` | 1 | ✅ Cleanup operation |
| `Refresh` | 1 | ✅ Refresh operation |

### Additional Test Scenarios

- **Context Cancellation**: Verifies proper handling of cancelled contexts
- **Error Handling**: Tests various error conditions and proper gRPC status codes
- **Mock Data Validation**: Ensures test data consistency and realistic scenarios

## Test Architecture

### Mock Client

The [`mocks/client.go`](mocks/client.go) provides a full implementation of the controller-runtime client interface:

- **Thread-safe**: Uses sync.RWMutex for concurrent access
- **CRUD Operations**: Complete Create, Read, Update, Delete support
- **Label Selectors**: Proper filtering by labels and namespaces
- **Status Updates**: Separate status writer implementation
- **Error Simulation**: Realistic Kubernetes API error responses

### Test Data Loader

The [`testdata/loader.go`](testdata/loader.go) provides flexible test data management:

- **Scenario-based Loading**: Load specific test scenarios
- **Group-based Loading**: Load all data for a specific group
- **Data Validation**: Ensures consistency between Node CRs and Kubernetes Nodes
- **Embedded YAML**: Test data embedded in binary for portability

### Environment Setup

- **envtest**: Real Kubernetes API server for CRD validation
- **Scheme Registration**: Proper registration of custom resources
- **Namespace Creation**: Automatic setup of required namespaces
- **Cleanup**: Proper teardown after test completion

## Key Features Validated

### Power Management
- Node power state transitions (on/off)
- Desired vs actual power state handling
- Progress tracking (ready, starting up, shutting down, shutdown)

### Scaling Operations
- Scale up: Finding powered-off nodes and powering them on
- Scale down: Graceful node shutdown and power-off
- Target size management and validation

### Group Management
- Group discovery and enumeration
- Node-to-group mapping via labels
- Group health status aggregation

### Pricing Integration
- Node hourly and pod pricing retrieval
- Invalid pricing data handling
- Cost calculation support

### GPU Support
- GPU node identification and labeling
- Available GPU types enumeration
- GPU-specific autoscaling options

## Troubleshooting

### Common Issues

1. **envtest Binary Missing**
   ```bash
   make setup-envtest
   ```

2. **Test Data Validation Errors**
   ```bash
   go test ./test/integration/testdata -v
   ```

3. **Mock Client Issues**
   - Ensure proper scheme registration
   - Check label selector syntax
   - Verify namespace consistency

### Debug Mode

Enable verbose logging for detailed test execution:

```bash
go test ./test/integration/... -v -ginkgo.v
```

## Contributing

When adding new tests:

1. **Follow Ginkgo Patterns**: Use Describe/Context/It structure
2. **Add Test Data**: Update YAML files for new scenarios
3. **Mock Validation**: Ensure mock client handles new operations
4. **Documentation**: Update this README with new test coverage

### Test Data Guidelines

- Use realistic homelab node configurations
- Ensure consistency between Node CRs and Kubernetes Nodes
- Include both success and error scenarios
- Follow the established labeling conventions

## Integration with CI/CD

These tests are designed to run in CI/CD pipelines:

- **No External Dependencies**: Uses embedded test data and mock clients
- **Fast Execution**: Typically completes in under 5 seconds
- **Deterministic**: Consistent results across environments
- **Comprehensive Coverage**: Validates all critical functionality

The integration tests complement the existing unit tests and e2e tests to provide complete validation of the homelab-autoscaler gRPC server implementation.