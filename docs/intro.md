---
slug: /
sidebar_position: 1
---

# Start Here

Homelab Autoscaler scales a Kubernetes cluster of physical machines. It powers real machines on when
cluster-autoscaler has work for them and off when it has none, using the interface the machine
already answers to: wake-on-LAN, IPMI/BMC, a smart PDU, or a script of your own.

It is two things in one deployment:

- a Kubernetes operator over three objects — `Group` (a pool's policy), `Node` (one machine slot),
  `NodeClaim` (one request for one machine)
- a gRPC cloud provider that cluster-autoscaler calls, so the autoscaler's standard scale-up and
  scale-down logic drives your hardware

The rule everything else follows from: **a machine is wanted exactly when a live `NodeClaim` names its
slot.** No field of a slot declares an intent, so a GitOps apply cannot revert a scale-up and a
scale-up cannot overwrite a declaration.
[The NodeClaim model](concepts/nodeclaim-model.md) is that rule, in five minutes.

## Where to start

| You are here to | Read |
| --- | --- |
| try it on a laptop, tonight | [Quick Start](getting-started/quick-start.md) — k3d, five minutes, power operations simulated |
| install it for real | [Installation](getting-started/installation.md) — the chart, and every value it takes |
| understand the model before you touch a pool | [The NodeClaim model](concepts/nodeclaim-model.md), then [Architecture](architecture/overview.md) |
| label a pool so a zoned pod schedules | [Node labels](concepts/node-labels.md) |
| read what a pool is doing right now | [Read the Pool at a Glance](concepts/pool-at-a-glance.md) |
| fix a pool that is misbehaving | [Common Issues & Solutions](troubleshooting/common-issues.md), then [Known Issues](troubleshooting/known-issues.md) |
| declare the fields in Git | [Group](api-reference/crds/group.md), [Node](api-reference/crds/node.md), [NodeClaim](api-reference/crds/nodeclaim.md) |
| come from `0.1.x` | [Upgrade 0.1 to 0.2](releases/upgrade-0.1-to-0.2.md) — a breaking cut, with the order of operations |

A first pool is three commands: install the chart with `clusterAutoscaler.enabled: true`, apply one
`Group`, apply one `Node` per machine with the `group` label and a `startupPodSpec` /
`shutdownPodSpec` that really powers that machine. Then a pending pod that fits the pool's node
template starts the loop, and `kubectl get nodeclaims` shows you the request.

## Limits worth knowing before you commit to it

- Bounds are constants, not knobs: the boot budget (12m10s, floored by the Group's
  `spec.maxNodeProvisionTime`), the attempt bound (3), the readiness gate (4m), the idle resync (5m).
  The knobs are the fields of the objects.
- There are no domain metrics. The pool's state is on the objects, and `prometheus.enable: true`
  scrapes the manager's controller-runtime metrics.
- One live claim per slot, one namespace per release, and the gRPC server serves plaintext — run it on
  a private network.

[Known Issues](troubleshooting/known-issues.md) is the full list, with the runbook each one links to.
