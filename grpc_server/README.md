# Mock gRPC Cloud Provider Server

This is a mock implementation of the CloudProvider gRPC server for testing and development purposes. It implements all 16 RPC methods defined in the externalgrpc.proto file.

## Features

- **Complete Implementation**: All 16 RPC methods are implemented
- **Thread-Safe**: Uses mutexes to ensure concurrent access safety
- **Realistic Mock Data**: Includes mock node groups, nodes, instances, GPU types, and pricing data
- **Proper Error Handling**: Returns appropriate gRPC status codes
- **Logging**: Comprehensive logging for debugging
- **Optional Methods**: Handles optional methods by returning Unimplemented errors as specified

## Architecture

The server maintains several in-memory data structures:

- **Node Groups**: Collection of node groups with min/max sizes
- **Nodes**: Individual node information with labels and annotations
- **Instances**: Cloud provider instances with status information
- **GPU Types**: Available GPU types with metadata
- **Pricing Data**: Mock pricing information

## RPC Methods Implemented

### Cloud Provider Methods
1. **NodeGroups** - Returns all configured node groups
2. **NodeGroupForNode** - Returns the node group for a given node
3. **PricingNodePrice** - Returns theoretical node price (Unimplemented)
4. **PricingPodPrice** - Returns theoretical pod price (Unimplemented)
5. **GPULabel** - Returns the GPU label for nodes
6. **GetAvailableGPUTypes** - Returns available GPU types
7. **Cleanup** - Cleans up resources
8. **Refresh** - Refreshes cloud provider state

### Node Group Methods
9. **NodeGroupTargetSize** - Returns current target size
10. **NodeGroupIncreaseSize** - Increases node group size
11. **NodeGroupDeleteNodes** - Deletes nodes from group
12. **NodeGroupDecreaseTargetSize** - Decreases target size
13. **NodeGroupNodes** - Returns nodes in the group
14. **NodeGroupTemplateNodeInfo** - Returns template node info (Unimplemented)
15. **NodeGroupGetOptions** - Returns autoscaling options (Unimplemented)

## Building and Running

### Prerequisites
- Go 1.24.5 or later
- gRPC dependencies

### Build
```bash
make build
```

### Run
```bash
make run
```

### Clean
```bash
make clean
```

## Testing

The server listens on port 50051 by default. You can test it using any gRPC client or tools like `grpcurl`.

### Example Test Commands

```bash
# List node groups
grpcurl -plaintext localhost:50051 clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider/NodeGroups

# Get GPU label
grpcurl -plaintext localhost:50051 clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider/GPULabel

# Get available GPU types
grpcurl -plaintext localhost:50051 clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider/GetAvailableGPUTypes
```

## Mock Data

The server comes pre-populated with realistic mock data:

### Node Groups
- **ng-1**: Standard compute (min: 1, max: 10, current: 3)
- **ng-2**: GPU enabled (min: 0, max: 5, current: 1)
- **ng-3**: High memory (min: 2, max: 20, current: 5)

### GPU Types
- nvidia-tesla-v100
- nvidia-tesla-t4

### Instance Types and Pricing
- standard-2: $0.10/hour
- standard-4: $0.20/hour
- gpu-v100: $2.50/hour
- high-mem-8: $0.80/hour

## Thread Safety

All operations are protected by read-write mutexes to ensure thread safety during concurrent access.

## Error Handling

The server returns appropriate gRPC status codes:
- `codes.NotFound` - When resources are not found
- `codes.InvalidArgument` - For invalid parameters
- `codes.Unimplemented` - For optional methods
- `codes.Internal` - For internal server errors

## Logging

All RPC calls are logged with relevant parameters for debugging purposes.