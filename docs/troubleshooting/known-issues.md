# Known Issues

What the operator does not do for you, what is bounded, and what is not configurable. Every symptom
has a runbook in [Common Issues & Solutions](common-issues.md).

## No Domain Metrics

The chart exposes controller-runtime metrics and nothing else: no gauge for "machines up", no counter
for "scale-ups this hour", no pool panel. The pool's state is on the objects:

```bash
kubectl get groups -n homelab-autoscaler-system                        # SLOTS ON OFF LASTSCALE HEALTH
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system   # POWERSTATE PHASE GROUP ZONE SINCE ON24H
kubectl get nodeclaim -n homelab-autoscaler-system -o wide             # PHASE ATTEMPT REASON DEADLINE
```

`prometheus.enable: true` renders a `ServiceMonitor` over the manager's metrics endpoint (needs
cert-manager, `metrics.enable: true`, and the Prometheus Operator CRDs). Alert on the objects: a
`NodeClaim` in `booting` past `spec.deadlineAt`, an `activationsLast24H` that says the pool is churning,
a `Group` whose `status.health` is `offline` while its `status.slots` is not 0.

## Error Recovery Is Bounded, Not Manual

Every failure path ends by itself. Nothing needs a restart, a re-arm, or a status field written back.

| Bound | Value | What happens when it is hit |
| --- | --- | --- |
| A claim's boot budget | 12m10s, floored by the Group's `spec.maxNodeProvisionTime` | `failed`, `reason: DeadlineExceeded`, and the machine is force-shut-down |
| Consecutive boot attempts, per slot | 3 | `failed`, `reason: AttemptLimitExceeded`: no machine is bought until an attempt lands |
| A stuck lifecycle transition | 15m (`fsm.StuckTransitionThreshold`) | `ForceCleanup`; the claim ends `failed`, `reason: StuckTransition` |
| A shutdown that will not land | 3 Jobs, then a 30m pause (`claim.ShutdownAttemptResetGrace`) | the controller stops firing the same script at the same machine, then fires it again |
| A ready claim whose machine is gone | one boot window with the machine observed off | `failed`, `reason: MachineGone`, freeing the slot |
| A terminal claim | `spec.ttlSecondsAfterFinished`, default 3600s | collected; the story survives in `Node.status.lastClaim` |

Manual action is what is left when the bound is not the fix: a `spec.shutdownPodSpec` whose script exits
0 without powering the machine off, a CNI that never clears the keys in the Group's
`spec.nodeReadinessTaints`, and two slots declaring the same `spec.kubernetesNodeName` (admission refuses
the second and names the holder).

## What Is Not Configurable

Constants, and the numbers the bounds above are built from: the idle resync of a settled slot (5m), the
lifecycle Job monitor poll (30s), the readiness gate (4m), the registration/uncordon window (90s), the
attempt bound (3), the shutdown streak (3 attempts, 30m reset grace), and the boot budget: 12m10s,
which is the startup Job's own timeout (5m) plus the Job monitor poll (30s) plus the registration wait
(90s) plus the readiness gate (4m) plus the uncordon retry ceiling (10s) plus a 1m margin. Raise the
budget only through the Group's `spec.maxNodeProvisionTime`, which lifts the floor of every claim's
`spec.deadlineAt`.

The knobs are the fields of the objects: the **Group's** seven spec fields (`scaleDownUtilizationThreshold`,
`scaleDownGpuUtilizationThreshold`, `scaleDownUnneededTime`, `scaleDownUnreadyTime`, `maxNodeProvisionTime`,
`zeroOrMaxNodeScaling`, `ignoreDaemonSetsUtilization`) plus `minSize` and `nodeReadinessTaints`; the
**slot's** (`startupPodSpec` / `shutdownPodSpec` with their `serviceAccount`, `priorityClassName` and
`ttlSecondsAfterFinished`, `nodeCapacity`, `nodeLabels`, `nodeAnnotations`, `nodeTaints`, `taintsToRemove`,
`kubernetesNodeName`, `pricing`); and the **claim's** (`requestedAt`, `deadlineAt`,
`ttlSecondsAfterFinished`, `requestedBy`, `request`). A scale-down window is a Group number: the pool is
what cluster-autoscaler sizes, not the machine.

## Symptoms To Runbooks

| Symptom | Runbook |
| --- | --- |
| A claim is stuck `pending` | [A Claim Is Stuck Pending](common-issues.md#a-claim-is-stuck-pending). A hand-made claim with no `spec.deadlineAt` never expires: stamp the budget |
| A claim is `failed` | [A Claim Failed](common-issues.md#a-claim-failed), and its reason table |
| `"one live claim per slot"` is rejected | [One Live Claim Per Slot Is Rejected](common-issues.md#one-live-claim-per-slot-is-rejected): the refusal is the invariant working |
| A pod that selects a label never schedules | [A Pod Needs A Slot With A Specific Label](common-issues.md#a-pod-needs-a-slot-with-a-specific-label): slots that disagree on a key leave it out of the group's template, fail-closed by design |
| A shutdown that never lands | [The Shutdown Job Exits 0 and the Machine Stays Up](common-issues.md#the-shutdown-job-exits-0-and-the-machine-stays-up): three Jobs, then a 30m pause, and the `shutdownAttempts` patch as the escape hatch |

[Debugging Guide](debugging-guide.md) is the component-by-component method,
[State Management](../development/state.md) the phase table and the deadlines,
[Events](../reference/events.md) every reason the claim and the slot publish,
[The NodeClaim model](../concepts/nodeclaim-model.md) and
[NodeClaim Design](../development/nodeclaim.md) why the bounds live on the request.
