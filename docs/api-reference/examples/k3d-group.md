# K3D Group Example

This example demonstrates a basic autoscaling group setup for development using k3d.

```yaml
# Example Group configuration for k3d testing environment
# This demonstrates a basic autoscaling group setup for development

apiVersion: infra.homecluster.dev/v1alpha1
kind: Group
metadata:
  name: k3d-workers
  namespace: homelab-autoscaler-system
spec:
  
  # Scale down when utilization is below 50%
  scaleDownUtilizationThreshold: 0.5
  
  # Wait 2 minutes before scaling down unneeded nodes
  scaleDownUnneededTime: "2m"
  
  # Wait 1 minute before scaling down unready nodes
  scaleDownUnreadyTime: "1m"
  
  # Maximum time to wait for a node to be provisioned
  maxNodeProvisionTime: "2m"
  
  # Ignore DaemonSet pods when calculating utilization
  ignoreDaemonSetsUtilization: true
  
  # Allow partial scaling (not just 0 or max)
  zeroOrMaxNodeScaling: false
  
  # GPU utilization threshold for scale down (if applicable)
  scaleDownGpuUtilizationThreshold: "30"
```

## Key Features

- **Development-friendly settings**: Short timeouts for quick testing
- **Moderate scaling thresholds**: 50% utilization threshold for responsive scaling
- **Partial scaling enabled**: Allows gradual scaling rather than all-or-nothing