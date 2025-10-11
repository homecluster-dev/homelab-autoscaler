# Known Issues

> ⚠️ **CRITICAL**: This system is NOT production-ready. Multiple critical bugs exist.

## Critical Bugs

### 1. ~~gRPC Server Logic Errors~~ ✅ **FIXED**

#### ~~NodeGroupTargetSize Logic Bug~~ ✅ **FIXED**
**File**: [`internal/grpcserver/server.go:306-311`](../../internal/grpcserver/server.go:306)

**Status**: ✅ **RESOLVED** - Logic bugs have been fixed with proper operator precedence and correct state handling.

#### ~~NodeGroupDecreaseTargetSize Logic Bug~~ ✅ **FIXED**
**File**: [`internal/grpcserver/server.go:461-473`](../../internal/grpcserver/server.go:461)

**Status**: ✅ **RESOLVED** - Node updates are now properly persisted with correct Client.Update() calls and status handling.

### 2. Controller Implementation Issues

#### Group Controller Incomplete
**File**: [`internal/controller/infra/group_controller.go`](../../internal/controller/infra/group_controller.go)

**Issues**:
- Only sets "Loaded" condition, no actual reconciliation logic
- **Purpose clarification**: The Group controller is designed for node mapping to the cluster autoscaler gRPC interface, not for implementing autoscaling logic itself
- **No integration with Node management**: The Group controller doesn't monitor or manage its child Node CRs, doesn't enforce group policies on nodes, doesn't track node health or count

**Impact**: Groups exist but don't function - they cannot manage their nodes or provide proper mapping interface.

#### ~~Node Controller Race Conditions~~ ✅ **FIXED**
**File**: [`internal/controller/infra/node_controller.go`](../../internal/controller/infra/node_controller.go)

**Status**: ✅ **RESOLVED** - Race conditions have been fixed with proper state validation and transition logic.

#### ~~Core Controller State Management~~ ✅ **FIXED**
**File**: [`internal/controller/core/node_controller.go`](../../internal/controller/core/node_controller.go)

**Status**: ✅ **RESOLVED** - State management issues have been addressed with proper error handling and validation.

### 3. Missing Critical Features

#### ~~Node Draining~~ ✅ **FIXED**
**Status**: ✅ **IMPLEMENTED** - Node draining has been implemented with proper pod eviction using Kubernetes eviction API.

**Implementation details**:
- Uses Kubernetes policy/v1beta1 Eviction API for graceful pod eviction
- Implements 5-minute timeout for drain operations
- Handles DaemonSet pods, static pods, and critical system pods appropriately
- Includes comprehensive logging and error handling
- Waits for pod termination before proceeding with shutdown

#### Error Handling
**Status**: Inadequate throughout

**Issues**:
- gRPC methods don't properly handle Kubernetes API errors
- Controllers don't implement proper error recovery
- No circuit breaker patterns for external dependencies

**Impact**: System becomes unstable under error conditions.

### 4. Architecture Issues

#### Namespace Hardcoding
**Throughout codebase**

**Issues**:
- `homelab-autoscaler-system` namespace assumed everywhere
- No configuration option for different namespaces
- Breaks multi-tenancy

**Impact**: Cannot deploy in custom namespaces.

## Severity Assessment

| Issue | Severity | Impact |
|-------|----------|---------|
| ~~gRPC Logic Bugs~~ ✅ **FIXED** | ~~**CRITICAL**~~ | ~~Autoscaling completely broken~~ |
| ~~Missing Node Draining~~ ✅ **FIXED** | ~~**CRITICAL**~~ | ~~Data loss risk~~ |
| ~~Controller Race Conditions~~ ✅ **FIXED** | ~~**HIGH**~~ | ~~State inconsistency~~ |
| Missing Error Handling | **HIGH** | System instability |
| Group Controller Incomplete | **MEDIUM** | No node mapping interface |
| Namespace Hardcoding | **LOW** | Deployment limitations |

## Workarounds

### For Development/Testing Only

1. **Manual Node Management**: Bypass autoscaler, manage Node CRDs directly
2. **Frequent Restarts**: Restart controller to clear inconsistent state

> ⚠️ **WARNING**: These workarounds are for development only. Do not use in any production-like environment.

## Next Steps

1. ~~Fix gRPC server logic bugs~~ ✅ **COMPLETED**
2. ~~Fix controller state management bugs~~ ✅ **COMPLETED**
3. ~~Implement proper node draining~~ ✅ **COMPLETED**
4. Add comprehensive error handling (highest priority)
5. Add proper state management and locking
6. Comprehensive testing of all components

## Related Documentation

- [Architecture Overview](../architecture/overview.md) - Understanding the intended design
- [Debugging Guide](debugging-guide.md) - How to investigate issues
- [Development Setup](../development/setup.md) - Setting up for fixes