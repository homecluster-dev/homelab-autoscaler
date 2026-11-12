# Read The Pool At A Glance

Two commands answer the shape of a pool: what it can have, what it is paying for, and what it did
last. No logs, no port-forward. The reading is the model in [The NodeClaim
model](nodeclaim-model.md) rendered as columns; the fields behind them are in
[Node CRD](../api-reference/crds/node.md) and [Group CRD](../api-reference/crds/group.md).

## The pool

```bash
kubectl get groups -n homelab-autoscaler-system
```

```
NAME             SLOTS   ON   OFF   LASTSCALE   HEALTH   AGE
remote-workers   5       2    3     24m         healthy  41d
```

| Column | What it answers |
| --- | --- |
| `SLOTS` | how many `Node` CRs are labelled `group: <name>`: the machines the pool can have, i.e. its max size |
| `ON` | the slots reporting a machine the pool is paying for: `booting`, `ready`, `draining` |
| `OFF` | the rest: `off`, `pending`, and a slot that has never reported. `SLOTS == ON + OFF` always |
| `LASTSCALE` | when the pool last grew or shrank. A scale-up is dated by the instant the request was made; a scale-down by the instant the scale-down landed on the claim, because a `DeleteNodes` names a machine an older request booted and does not rewrite that request's time. The verb and the slot are in `kubectl describe group` under `lastScaleAction`: `up remote-worker-2`, `down remote-worker-4`, `ca:DeleteNodes remote-worker-3` |
| `HEALTH` | `offline` when no slot of the pool has ever reported a phase — no slots committed, or a pool nobody's reconciler is running on — and `healthy` once at least one has. A pool whose every machine is `off` is asleep, which is the whole idea, and not an outage: the column answers "is the control loop alive for this pool" |

`NAME` and `AGE` are the columns every Kubernetes table has, not ones this CRD declares.

The group controller computes the numbers in one pass over the slots it already has, and writes only
when what it computed differs from what it read. `LASTSCALE` is the interesting one: the same story is
a Kubernetes Event, Events are garbage-collected in about an hour, and "what did this pool do last
night" is asked the next morning — so it is a field on the pool, materialized from the
`status.lastClaim` digest each slot keeps after its claim is collected.

## The slots

```bash
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
```

```
NAME              POWERSTATE   PHASE     GROUP           ZONE   SINCE    ON24H
remote-worker-1   on           ready     remote-workers  fsn1   3h13m    3
remote-worker-2   on           booting   remote-workers  fsn1   4m       1
remote-worker-3   off          off       remote-workers  hel1   18h      2
remote-worker-4   on           draining remote-workers         30m      0
remote-worker-5   off          off       remote-workers         55m      0
```

| Column | What it answers |
| --- | --- |
| `POWERSTATE` | what the provider observes: `on` when the `corev1.Node` reports `NodeReady=True` |
| `PHASE` | the derived report of the slot: `off`, `pending`, `booting`, `ready`, `draining`. A report, not an input — [The NodeClaim model](nodeclaim-model.md#the-input-of-every-decision) |
| `GROUP` | the pool this slot belongs to (`metadata.labels.group`) |
| `ZONE` | the zone the slot declares, `spec.nodeLabels["topology.kubernetes.io/zone"]`. Empty for a pool that does not zone its slots |
| `SINCE` | when `status.phase` last changed value (`status.lastTransitionTime`) |
| `ON24H` | how many times this machine was booted inside the last 24 hours (`status.activationsLast24H`) |

`ON24H` is the churn column: a pool that reads `ON 2 / OFF 3` at 3 AM and whose slots each report 6
activations was woken six times by work that finished. It is the length of a ledger of the window's
boots (`status.recentActivations`), pruned on the pass the oldest one leaves the window, so the number
does not go stale while the pool sleeps. It is deliberately **not** counted from
`status.recentTransitions`: that ring is the display history, capped at 32 rows, and a slot that
churns 40 times a day writes 80 rows, so a count read through the ring goes flat at 16 exactly where
the question starts to matter. The ledger is capped at 512 boots a day — 21 an hour, nonstop — so a
slot reporting exactly `512` booted *at least* 512 times: a floor the number states by sitting at the
cap, not a silent flatline.

## The history of one slot

```bash
kubectl describe node.infra.homecluster.dev remote-worker-3 -n homelab-autoscaler-system
```

The `recentTransitions` block is the last 24 hours of that slot, newest last, capped at 32 rows.
"Newest last" is the order the writes landed, not a clock sort: the two writers of the report append
as they go, so two rows inside one second can be listed out of chronological order. The rows carry
their own instants, and the order of the list is a display detail of a churn:

```
Status:
  Phase:  off
  Power State:  off
  Previous Phase:  draining
  Last Transition Time:  2026-11-11T20:58:00Z
  Activations Total:  9
  Activations Last24 H:  1
  Recent Activations:
    2026-11-11T09:12:00Z
  Recent Transitions:
    At:  2026-11-11T09:12:00Z
    From:  off
    To:  booting
    Reason:  claim remote-worker-3-4b7e pending, observed off, startup job in flight
    At:  2026-11-11T09:19:00Z
    From:  booting
    To:  ready
    Reason:  claim remote-worker-3-4b7e ready, observed on, node usable
    At:  2026-11-11T20:52:00Z
    From:  ready
    To:  draining
    Reason:  claim remote-worker-3-4b7e marked for scale-down, observed on
    At:  2026-11-11T20:58:00Z
    From:  draining
    To:  off
    Reason:  claim remote-worker-3-4b7e marked for scale-down, observed off
```

The `Reason` of a row names the inputs the report was derived from — the live claim, the observed
power, the usability of the node object — so "why does this slot say `draining`" is answered without
opening the claim. `recentActivations` is the block under the ring, and the ledger the `ON24H` number
is the length of: this slot booted once in the day, at 09:12, and the ring's four rows are the phases
that boot went through. `previousPhase` is the other half of the sentence a phase cannot say on its
own: `off -> booting` is a machine coming up, `ready -> booting` is a machine the pool lost.

The requests themselves are the third command, and the only one that answers "who asked":

```bash
kubectl get nodeclaim -n homelab-autoscaler-system -l infra.homecluster.dev/node=remote-worker-3 -o wide
```

## Durations

There is no duration column: a printer column selects a value from the object, it does not subtract
one, and the API server prints what the object holds. What the object holds is the
instant (`SINCE`); the duration is arithmetic you do at read time, and `status.phase` tells you which
side of the power you are measuring. The command, its exact output and the fixture it is checked
against are in the runbook:
[How long has this machine been up?](../troubleshooting/common-issues.md#how-long-has-this-machine-been-up).

`SINCE` alone is kubectl's relative rendering of the same instant, rounded its own way (`18h`); the
runbook's command is exact, and it is what to run when the rounding is the question.

## Related Documentation

- [The NodeClaim model](nodeclaim-model.md) — what the columns are derived from
- [Node labels](node-labels.md) — the `ZONE` column, and what a pool can promise
- [Events](../reference/events.md) — the timeline of one attempt, while it is readable
- [Common Issues & Solutions](../troubleshooting/common-issues.md) — when a column says something is wrong
