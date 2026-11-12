---
title: Changelog
description: Release history and changes for Homelab Autoscaler
sidebar_position: 1
---

# Changelog

All notable changes to the Homelab Autoscaler are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/) and the project
uses [semantic versioning](https://semver.org/spec/v2.0.0.html). While the
project is `0.x`, a **minor** bump may break the API — those releases say so
under *Breaking*.

## [0.2.3] - 2026-11-12

The pool is readable at a glance.

### Added

- `kubectl get nodes.infra.homecluster.dev` now shows `POWERSTATE PHASE GROUP
  ZONE SINCE ON24H` — the group the slot belongs to, the
  `topology.kubernetes.io/zone` it declares, when its phase last changed, and
  how many times the machine was booted in the last 24 hours.
- `kubectl get groups.infra.homecluster.dev` now shows `SLOTS ON OFF LASTSCALE
  HEALTH`.
- `NodeStatus`: `lastTransitionTime`, `previousPhase`, `activationsTotal`,
  `activationsLast24H`, `recentTransitions` (a bounded, pruned ring that
  `kubectl describe` renders as a timeline) and `recentActivations` (the 24-hour
  ledger the count is derived from).
- `GroupStatus`: `slots`, `on`, `off`, `lastScaleAt`, `lastScaleAction`
  (`up <slot>`, `down <slot>`, `ca:DeleteNodes <slot>`), and a derived `health`.
- `NodeGroupDecreaseTargetSize` now records its attribution on the claim it
  cancels, so a scale-down is as auditable as a scale-up.

### Changed

- The 24-hour boot count is kept honest over time: the slot is requeued when the
  oldest entry leaves the window, so the number cannot go stale. The ledger
  holds up to 512 boots; past that the count is a stated floor.
- `Group.status` is written only when it changes, and `Loaded` no longer moves
  its `lastTransitionTime` on every pass.

### Notes

- These fields are observability only. Nothing in the state machine reads them —
  that is asserted by a test.

## [0.2.2] - 2026-11-12

Slots can describe the machine they want.

### Added

- `Node.spec.nodeLabels` and `Node.spec.nodeAnnotations`: labels and annotations
  written to the Kubernetes node when it joins, re-asserted when they drift, and
  removed when the declaration drops them. The keys the slot wrote are recorded
  on the node, so removal never touches a key the operator did not set.
- The declaration reaches cluster-autoscaler through the group's node template,
  so a pod selecting `topology.kubernetes.io/zone` (or any declared label) can
  trigger a scale-up and be matched against a slot that really has it.

### Changed

- The template carries a declared key only when **every** slot of the group
  agrees on it (a group whose slots differ cannot be simulated honestly); the
  operator logs which key and which slots disagreed.
- The Node webhook refuses a slot whose `spec.kubernetesNodeName` is already
  claimed by another slot — one machine belongs to one slot.
- Reserved namespaces are refused at admission and skipped at write time:
  `infra.homecluster.dev/*`, `node.kubernetes.io/*`, `kubernetes.io/*`,
  `node-role.kubernetes.io/*`, `homelab-autoscaler.dev/*`,
  `cluster-autoscaler.kubernetes.io/*`, and `topology.kubernetes.io/*` except
  `/zone` and `/region`.
- A slot that declares labels is not uncordoned until the labels are on the
  node; a failed write waits inside the readiness gate instead of logging and
  uncordoning.

## [0.2.1] - 2026-11-12

### Added

- Every claim carries its own event timeline: `ClaimCreated`, `ClaimBooting`,
  `ClaimReady`, `ClaimFailed`, `ClaimCancelled`, `ClaimCompleted`,
  `ClaimSuperseded` — one event per transition, emitted after the status write,
  readable with `kubectl describe nodeclaim`.

### Changed

- The lifecycle Job is owned by the claim that asked for it (the slot remains a
  secondary owner), so the Job appears under the attempt it belongs to and
  carries `infra.homecluster.dev/nodeclaim`.

## [0.2.0] - 2026-11-12

**Breaking.** The `NodeClaim` model, and the API moves to `v2alpha1`.

### Breaking

- `Node.spec.powerState` is **removed**. Desired power is derived: a machine is
  wanted when a live `NodeClaim` names the slot.
- `Node.status.progress` is **removed**, replaced by a derived `status.phase`
  (`off`, `pending`, `booting`, `ready`, `draining`) that is reported, never
  used to decide.
- `api/infra/v1alpha1` is removed from the served set. There is no conversion
  webhook and no deprecation period: this was a `0.x` breaking change.

### Added

- `NodeClaim` (`infra.homecluster.dev/v2alpha1`): one object per attempt, with
  `spec.slotRef`, `requestedAt`, `requestedBy` and `request` (the gRPC call, its
  delta, the group, the named nodes), `spec.deadlineAt`, and `status.phase`,
  `attempt`, `reason`, `finishedAt`, conditions.
- `Node.status.lastClaim`: a digest of the slot's latest attempt (name, phase,
  attempt, call, times), so `kubectl describe` still tells the story after the
  claim is collected.

### Changed

- cluster-autoscaler's RPCs map onto claims: `TargetSize` counts live claims
  (failed/overdue ones never count), `IncreaseSize` creates claims,
  `DecreaseTargetSize` cancels **only `pending`** claims (a boot that started is
  committed and always finishes), `DeleteNodes` records the request and drains
  the claim.
- A claim fails at `spec.deadlineAt` — the operator budget, floored by the
  group's `maxNodeProvisionTime`.
- `kind: Node` print columns report the derived phase; the `progress` column is
  gone.

### Removed

- The progress watchdog and the observed-power precedence rule (subsumed by
  overdue claims and the derived phase).
- The `ClaimRearmed` / `ShutdownAttempts` condition machinery and its reset
  heuristics (attempt state lives on the claim).

### Upgrade notes

Apply in this order: the new CRDs (delete the old `v1alpha1` objects first, or
step through a merged CRD — the API server refuses to drop a storage version
that still holds data), then the operator, then the GitOps manifests, then let
the pool reconcile to zero. The procedure, with the commands and the risks of
each step, is [Upgrade 0.1 to 0.2](upgrade-0.1-to-0.2.md).

## [0.1.22] - 2026-11-11

### Fixed

- A slot reported `powerState: on` while its progress said `shutdown` rested
  untouched for as long as nobody looked: observed provider power now outranks a
  progress record that outlived it, and the machine is shut down (or the report
  corrected to `ready`) on the next pass.
- A shutdown recorded as complete while the machine was still up: the completion
  is now checked against the observation (kubelet heartbeat against the Job's
  completion time).
- Shutdown attempts are bounded and backed off, mirroring the startup bound.

## [0.1.21] - 2026-11-11

### Fixed

- `spec.powerState` is optional with a default of `off`, so a GitOps manifest can
  stop declaring it. This is the root-cause fix for the churn: Argo CD's
  self-heal re-applied `powerState: off` every ~2.5 minutes and reverted every
  scale-up mid-boot.

## [0.1.20] - 2026-11-11

### Added

- `Group.spec.nodeReadinessTaints`: the taints a machine must be free of before
  the slot is called ready (defaults to Cilium's bootstrap taint).
- Autoscaler markers (`ToBeDeletedByClusterAutoscaler`,
  `DeletionCandidateOfClusterAutoscaler`) are recorded as events, so a cordon is
  attributable without raising cluster-autoscaler's verbosity.

### Fixed

- Slots whose progress outlived their machine are settled instead of resting
  forever; a claimed slot that never booted is re-armed, bounded to three startup
  attempts per boot episode and reset when the readiness gate completes.
- A slot is only declared ready once the node is Ready **and** free of the
  readiness taints, with its own four-minute deadline and lock renewal while
  waiting.

## [0.1.19] - 2026-11-11

### Fixed

- Claimed slots count towards the group's target size, and a slot can no longer
  be stranded in a transitional state by an out-of-band canceller.

## [0.1.18] - 2026-11-11

### Added

- `Node.spec.nodeCapacity`: a slot declares the allocatable capacity of the
  machine it will boot, so cluster-autoscaler can size a pending pod and the pool
  can scale from zero.

## [0.1.17] - 2026-11-11

### Added

- Node CRs can declare taints, applied to the machine when it joins.

### Fixed

- The node template is built from a slot that has a real `corev1.Node`, so
  cluster-autoscaler simulates the shape the pool actually produces.

## [0.1.16] - 2026-11-11

### Added

- `Group.spec.minSize`.

### Fixed

- A slot's progress is corrected when its power state did not change.

## [0.1.15] - 2026-11-11

### Added

- `priorityClassName` on the startup and shutdown pod specs.
- `PricingPodPrice` in the external gRPC provider, so `--expander=price` works.
- `nodeSelector` / `tolerations` / `affinity` for the controller manager in the
  chart.

### Fixed

- The chart templates `HOMELAB_AUTOSCALER_NAMESPACE` and the cluster-autoscaler
  gRPC endpoint from the release namespace.
- Lifecycle events moved to the `events.k8s.io` API.
- Chart RBAC and the validating webhook repaired for real installs.

## [0.1.14] - 2026-05-27

### Added

- Volumes and `serviceAccount` support on the startup and shutdown pod specs.

## [0.1.13] - 2026-04-20

### Changed

- External gRPC proto updated for Kubernetes 1.35; controller-runtime v0.23.
- The chart is released on every push to `main`, not only when the CRDs drift.

## [0.1.11] - 2025-11-03

### Added

- The documentation site (Docusaurus with Algolia search).

## [0.1.10] - 2025-10-17

### Changed

- Chart values and packaging fixes.

[0.2.3]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.2.3
[0.2.2]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.2.2
[0.2.1]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.2.1
[0.2.0]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.2.0
[0.1.22]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.22
[0.1.21]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.21
[0.1.20]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.20
[0.1.19]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.19
[0.1.18]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.18
[0.1.17]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.17
[0.1.16]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.16
[0.1.15]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.15
[0.1.14]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.14
[0.1.13]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.13
[0.1.11]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.11
[0.1.10]: https://github.com/homecluster-dev/homelab-autoscaler/releases/tag/homelab-autoscaler-0.1.10
