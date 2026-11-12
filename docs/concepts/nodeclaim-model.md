# The NodeClaim Model

One rule decides every power action in the pool:

```
a machine is wanted  ⇔  a live NodeClaim names its slot
```

Read it as an operator: nothing you declare on a slot asks for a machine, and nothing a machine is
doing asks for one either. A request is an object, and the object is what the controller acts on.
This page is the model in the words you need to run a pool; the field-by-field reference is
[NodeClaim CRD](../api-reference/crds/nodeclaim.md) and the running code is
[State Management](../development/state.md).

## The three objects, and who writes them

| Object | What it is | Who writes its spec | Who writes its status | How long it lives |
| --- | --- | --- | --- | --- |
| `Node` | a slot: the configuration of one machine | Git | the operator's reconcilers | as long as the machine is committed to the pool |
| `NodeClaim` | one request for one machine | the autoscaler's gRPC provider, or you, by hand | the claim controller | one attempt: minutes, then collected by `spec.ttlSecondsAfterFinished` |
| `Node.status` | the report: what the cluster observes about the machine | — | the observation reconciler and the transition path | with the slot |

`Node.spec` has no field for an intent in `v2alpha1`. Git is the only writer of a slot's spec, so an
apply cannot contradict a scale-up and a scale-up cannot overwrite a declaration. `Node.status` is
`powerState` (observed), `phase` (derived) and `lastClaim` (the digest of the request that ended).

## Five phases, and which ones are cancelable

| Phase | Meaning | Cancelable |
| --- | --- | --- |
| `pending` | the request exists and no lifecycle Job has been created for it | yes — deleting it is the cancel |
| `booting` | the startup Job exists and the power-on has been asked for | no — it is committed |
| `ready` | the machine is up, joined, and free of the pool's readiness taints, so it can take the pool's work | no — it owns a machine |
| `failed` | the attempt produced no usable machine and never will. Terminal | — |
| `completed` | the attempt ended with the machine it asked for available. Terminal | — |

`pending` is the only phase a caller cancels. A delete of a `booting` or `ready` claim is not an
interruption: the claim carries a finalizer from the moment its boot is committed, and the finalizer
runs the reversal to its end — cordon, shutdown Job, power off — and the claim ends `completed`.
A claim created and not yet stamped has no phase at all; the model reads an empty phase as `pending`,
so a request is capacity from the instant the API server accepts it.

`booting → ready` is written by the readiness gate, and it is the only writer. The same write clears
`spec.deadlineAt`.

## One live claim per slot

A slot answers for one request at a time. That is what makes "a live claim exists" mean "the machine
is wanted", what keeps the group's target size a count of claims, and what keeps a request
cancellable. A CRD cannot express uniqueness across objects, so the rule is enforced twice: at
admission, where you see the refusal, and in the claim controller, where admission cannot see
(concurrent providers, a hand-applied claim, a webhook that was down). The message names the slot and
the claim that owns it:

```
error: metadata: Invalid value: "worker-01": slot "worker-01" already has the live NodeClaim
"worker-01-7f4b9c" in phase "booting": one live claim per slot
```

If two live claims are ever found on one slot, the newest owns it and the extra one ends
`failed` with `reason: Superseded`. A terminal claim frees its slot, and a delete is never refused.

## The input of every decision

A reconcile pass reads three facts and nothing else:

```
(claim phase, observed power, node usability)
```

- **claim phase** — the phase of the slot's live claim, or none.
- **observed power** — `Node.status.powerState`: `on` when the `corev1.Node` reports
  `NodeReady=True`, `off` otherwise. It is an observation of the machine, not an intent.
- **node usability** — the `corev1.Node` exists, reports `Ready`, carries none of the pool's
  `spec.nodeReadinessTaints`, and is not cordoned.

The pass fires the lifecycle transition those three facts ask for. `Node.status.phase` — the
`PHASE` column of `kubectl get nodes.infra.homecluster.dev`, one of `off`, `pending`, `booting`,
`ready`, `draining` — is computed from the same triple and **decides nothing**: no reconciler, hook
or provider call reads it to choose an action. Write a value that contradicts the claim and the
observation and the next pass overwrites it from its inputs. The phase the model acts on is the
**claim's**. That is why a slot that reports `booting` for a machine that is serving is a display
bug, and why the fix is never "patch the phase".

| Live claim | Observed power | Usable | The slot reports | What runs |
| --- | --- | --- | --- | --- |
| none | off | — | `off` | nothing: a free slot |
| none | on | — | `draining` | the shutdown: an unwanted machine |
| `pending` | off | — | `pending` | the startup Job starts |
| `pending` | on | — | `booting` | the Job monitor of the boot owns it |
| `booting` | any | any | `booting` | the Job monitor owns it |
| `ready` | on | yes | `ready` | nothing |
| `ready` | on | no | `booting` | the readiness gate owns it |
| `ready` | off | — | `booting` | nothing fires; the claim ends past one boot window |
| live, marked for deletion | on | — | `draining` | the shutdown, then the finalizer releases |
| live, marked for deletion | off | — | `off` | the claim ends `completed` |
| `failed` / `completed` | on | — | `draining` | the shutdown: a terminal claim is not a want |
| `failed` / `completed` | off | — | `off` | nothing |

## `spec.deadlineAt`

Every claim is born with a budget, measured from `spec.requestedAt`:

```
deadlineAt = requestedAt + max(12m10s, group.spec.maxNodeProvisionTime)
```

12m10s is the sum of every deadline one attempt can consume, plus one sleep at its ceiling:

```
startup Job timeout 5m  + Job monitor poll 30s + registration wait 90s
                      + readiness gate 4m + uncordon retry ceiling 10s + 1m
```

The startup Job's own timeout is in the sum because the clock starts at `requestedAt`, before the Job
exists: one attempt can spend 5m of Job, 90s of registration and 4m of gate — 10m30s of deadlines in
series — and a shorter budget kills the claim inside the machine it is still buying. The
group's `spec.maxNodeProvisionTime` is a **floor**, never a ceiling below the operator's number:
cluster-autoscaler is told that same number as its own scale-up termination deadline, so a claim that
died before it is a slot the autoscaler is still waiting for, and its answer is another request.

What the deadline does when it passes:

- the claim is recorded `failed` with `reason: DeadlineExceeded`;
- the machine is force-shut-down, because a boot that overran may join a cluster that no longer
  wants it and start serving capacity the autoscaler has already given up on;
- the slot becomes free, and the next claim on it is attempt N+1.

The deadline bounds the **boot**, not the machine's life: it applies to `pending` (counting an empty
phase) and `booting`, and the `booting → ready` write clears it. A healthy machine is never
destroyed by a budget it already spent.

A claim you write by hand carries no computed deadline — the provider stamps `spec.requestedAt` and
`spec.deadlineAt` when it creates one, and nothing fills them in for you. **A hand-made claim with no
`spec.deadlineAt` never expires**: an attempt that never starts holds its slot open with nothing left
to time it out. Stamp the budget in the manifest:

```yaml
spec:
  requestedAt: "2026-03-04T09:38:15Z"
  deadlineAt: "2026-03-04T09:50:25Z"   # requestedAt + 12m10s
```

The two bounds that outlive the boot window are the ones you read about, not the ones you tune:

- a `ready` claim whose machine is observed off for longer than one boot window ends
  `failed` / `MachineGone`. The clock starts at the first observation
  (`MachineObservedOff`) and is cleared when the machine comes back, so a reboot heals and a
  controller restart does not restart it;
- a scale-down whose shutdown never lands is counted on `status.shutdownAttempts` — one per shutdown
  Job — and past the bound the controller stops firing the same script, for 30 minutes, then fires
  again. See [Common issues](../troubleshooting/common-issues.md#the-shutdown-job-exits-0-and-the-machine-stays-up).

## Attempts

Attempts are a property of the request, not of the slot.

- `status.attempt` is seeded once, on the claim's first pass, from `Node.status.lastClaim.attempt`:
  the next number while the slot keeps failing, and back to `1` after an episode that ended `ready`
  or `completed`.
- It is incremented by the `pending → booting` write, so it counts machines bought, not reconcile
  passes.
- The bound is 3. Past it, a claim in the boot window is failed with `reason: AttemptLimitExceeded`
  before a machine is bought, and nothing is created for the slot.
- What resets it: an attempt that lands — the streak restarts at 1 after an episode ends `ready` or
  `completed`. There is no counter on the slot to accumulate and no reset heuristic, because the
  object that carries the count is collected with the episode.
- A scale-down's attempts are a separate counter, `status.shutdownAttempts`, on the claim that asked
  for the scale-down. A shutdown script that never lands does not spend the boot attempts of the
  machine it is draining.

## Why this is shaped like this

In `v1alpha1` the request was a scalar, `Node.spec.powerState`, and one field carried the scale-up
request, the lock, the target-size arithmetic and the history at once, written by four controllers.
That is where the churn came from: a spec field is Git's, so Argo CD with self-heal re-applied the
value in Git over the autoscaler's write on every sync — about every two and a half minutes — the
machine went down under a running pod, the pending pod triggered the next scale-up, and the slot
looped forever. Mitigating it (default the field, stop declaring it) left the field in the API, so
any applier that wrote it won the last-write race. The same scalar made a request anonymous — nothing
recorded who asked, for which pool, or that this was attempt three — and put its retry counters on a
slot that never ends, which forced a reset heuristic per counter. `status.progress`, the field that
recorded "what the FSM last did" and "what the world looks like" at the same time, needed two more
fixes to disambiguate it. The request got an object, an owner, a deadline and an end, and the fields
that had to mean three things were removed.

## Related Documentation

- [Node CRD](../api-reference/crds/node.md), [NodeClaim CRD](../api-reference/crds/nodeclaim.md),
  [Group CRD](../api-reference/crds/group.md)
- [Node labels](node-labels.md) — what a slot declares, and what the pool's template promises
- [Read the Pool at a Glance](pool-at-a-glance.md) — the columns that show this model running
- [Events](../reference/events.md) — what the claim and the slot publish, and what lasts
- [State Management](../development/state.md) — the same rules as the running code
- [NodeClaim design](../development/nodeclaim.md) — the full design, and its trade-offs
