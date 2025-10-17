# K3D Node Example

This example demonstrates physical node configuration with simulated power operations for k3d testing.

```yaml
# Example Node configuration for k3d testing environment
# This demonstrates physical node configuration with simulated power operations

apiVersion: infra.homecluster.dev/v1alpha1
kind: Node
metadata:
  name: k3d-homelab-autoscaler-agent-0
  namespace: homelab-autoscaler-system
  labels:
    # Associate this node with the k3d-workers group
    group: k3d-workers
    node-type: worker
spec:
  # The Kubernetes node name this physical node represents
  kubernetesNodeName: k3d-homelab-autoscaler-agent-0
  
  # Current power state (on/off)
  powerState: "on"
  
  # Pricing information for cost calculations
  pricing:
    hourlyRate: "1.5"  # Cost per hour when running
    podRate: "0.0"     # Additional cost per pod (optional)
  
  # Job specification for starting up this node
  startupPodSpec:
    image: curlimages/curl:latest
    command:
      - /bin/sh
    args:
      - -c
      - curl http://host.k3d.internal:8080/start?vm=k3d-homelab-autoscaler-agent-0
    # Optional: Add resource limits
    # resources:
    #   limits:
    #     cpu: 100m
    #     memory: 128Mi
  
  # Job specification for shutting down this node
  shutdownPodSpec:
    image: curlimages/curl:latest
    command:
      - /bin/sh
    args:
      - -c
      - curl http://host.k3d.internal:8080/stop?vm=k3d-homelab-autoscaler-agent-0
    # Optional: Add resource limits
    # resources:
    #   limits:
    #     cpu: 100m
    #     memory: 128Mi
```

## Key Features

- **Simulated power operations**: Uses HTTP calls to simulate node startup/shutdown
- **Development pricing**: Low cost rates suitable for testing
- **Simple container jobs**: Uses curl for power state changes
- **k3d integration**: Designed to work with k3d's host networking