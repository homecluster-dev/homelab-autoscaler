# K3D Group Example

The Group the k3d environment runs: short windows, so a laptop run reaches a scale-down in minutes.

```yaml
# Example Group configuration for a k3d testing environment
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: k3d-workers
  namespace: homelab-autoscaler-system
spec:
  # 0 lets the pool scale to nothing. Set 1 to keep one machine up: cluster-autoscaler
  # will not take the pool below the floor, and no keepalive pod is needed.
  minSize: 0
  # Strings on a ratio field: "0.5" is 50% utilization. 0.5 unquoted is rejected by the
  # API server, and "30" on this field is 3000% - a number the autoscaler compares
  # against a utilization between 0 and 1.
  scaleDownUtilizationThreshold: "0.5"
  scaleDownGpuUtilizationThreshold: "0.3"
  # Short windows for a test run; production numbers are in the Production Examples.
  scaleDownUnneededTime: "2m"
  scaleDownUnreadyTime: "1m"
  # The budget of one boot: the group's number is the floor of every claim's
  # spec.deadlineAt here, and cluster-autoscaler's own termination deadline for the scale-up.
  # A k3d container answers inside it; a real machine needs more.
  maxNodeProvisionTime: "2m"
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: true
```

## What The Fields Do Here

- **No `maxSize`.** This pool's ceiling is the number of `Node` CRs labelled
  `group: k3d-workers` — the k3d examples declare two, so `kubectl get groups` reads `SLOTS 2` and
  the pool can never be asked for a third machine
- **Every field except `minSize` and `nodeReadinessTaints` is required**, so a Group that declares
  three of them is refused at apply with a list of what is missing
- **`maxNodeProvisionTime: "2m"`** is the number a laptop run wants; the claim's own budget is
  12m10s and the group's number only raises the floor, so on this pool a claim expires at 12m10s
- **The readiness gate** is Cilium's bootstrap taint by default. A k3d cluster runs flannel, which
  stamps no such taint, so the gate is satisfied by the `Ready` condition — the CRD default costs a
  look at a key that never appears, not a stuck slot

## Related Documentation

- [Group CRD Reference](../crds/group.md)
- [K3D Node Example](k3d-node.md)
- [Quick Start](../../getting-started/quick-start.md)
