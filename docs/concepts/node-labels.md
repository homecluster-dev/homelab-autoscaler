# Node Labels

A slot declares the labels its machine must carry in `spec.nodeLabels`, and the annotations in
`spec.nodeAnnotations`. Labels are the half that changes behaviour: scheduling is label matching, and
cluster-autoscaler picks the pool to grow by matching a pod's `nodeSelector` against the pool's node
template. This page is what you need to make a labelled pool work; the field reference is
[Node CRD](../api-reference/crds/node.md#node-labels-and-annotations).

## Declare it on the slot

```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: storage-01
  namespace: homelab-autoscaler-system
  labels:
    group: storage-nodes
spec:
  kubernetesNodeName: storage-01
  nodeLabels:
    topology.kubernetes.io/zone: fsn1
  nodeAnnotations:
    pool.homecluster.dev/serial: SN-0001
```

A pod with `nodeSelector: {topology.kubernetes.io/zone: fsn1}` is a candidate for this pool only when
the pool's template carries that label, and the template is built from what the slots declare. In a
homelab nothing writes a machine's zone: there is no cloud node controller to ask, so the slot's
declaration in Git is the only source of the fact.

## What the controller does with the declaration

- **On join, before the node is schedulable.** On a scale-up the declared keys are written to the
  `corev1.Node` in the readiness gate, **before** the node is uncordoned. A machine is never
  schedulable for a pod whose selector it does not yet satisfy. A write that fails keeps the machine
  cordoned and records `DeclaredNodeLabelsUnwritten` on the slot; the gate's 4-minute deadline still
  wins, and at that point the node is uncordoned and the write keeps being retried, because a
  cordoned machine is one cluster-autoscaler's scale-down never removes.
- **Re-asserted on drift.** Change the value with `kubectl label --overwrite`, delete a key, reboot a
  machine whose kubelet stamps its own set, let a re-registration recreate the node object: the next
  reconcile of the slot puts the declaration back.
- **Removed when the declaration drops the key.** Delete the key from `spec.nodeLabels` in Git and it
  comes off the node on the next reconcile. The controller knows the key is its own because it wrote
  it down: the keys a slot applied are recorded on the node object under
  `infra.homecluster.dev/applied-node-labels` (and `/applied-node-annotations`), and the controller
  owns exactly those. That record is what makes removal honest — deleting every node label that is
  not in the spec would delete the kubelet's own.
- **Never written for a key the slot does not declare.** The kubelet's `kubernetes.io/hostname`, a
  cloud integration's topology labels, anything another tool added, stay as they are.

One `Normal` event per write, reason `NodeLabelsApplied`, names what changed with `+` (added), `~`
(rewritten) and `-` (removed):

```bash
kubectl describe node.infra.homecluster.dev storage-01 -n homelab-autoscaler-system
# NodeLabelsApplied  node storage-01: labels: +topology.kubernetes.io/zone ~pool.homecluster.dev/rack -old
kubectl get node storage-01 -o jsonpath='{.metadata.annotations.infra\.homecluster\.dev/applied-node-labels}' ; echo
```

Fix Git, not the node. A hand edit is undone by the next pass; a settled slot is passed over on its
five-minute idle resync, so ask for one
([Resource State Mismatch](../troubleshooting/common-issues.md#resource-state-mismatch)).

## Reserved namespaces

Keys in these namespaces are refused at `kubectl apply`, with the reason in the message. Two writers
of one key never converge, and the owner wins the last write: the label the scheduler matches ends up
the one its owner stamped, and your declaration becomes the thing the autoscaler simulated.

| Refused for labels | Owner |
| --- | --- |
| `infra.homecluster.dev/*` | this operator: it stamps `group` and `node` on the node object and keeps its records there |
| `homelab-autoscaler.dev/*` | this project: the coordination locks a lifecycle operation takes on a slot |
| `cluster-autoscaler.kubernetes.io/*` | cluster-autoscaler: its per-machine controls are read off the node object |
| `node.kubernetes.io/*` | the kubelet and the node lifecycle controller |
| `kubernetes.io/*` | the kubelet and the cloud integrations |
| `node-role.kubernetes.io/*` | the tooling that joins a machine: kubeadm, k3s, RKE2 |
| `topology.kubernetes.io/*` | the topology machinery — **except** `topology.kubernetes.io/zone` and `topology.kubernetes.io/region`, which are yours |

| Refused for annotations | Owner |
| --- | --- |
| `infra.homecluster.dev/*`, `homelab-autoscaler.dev/*` | this operator's records, and this project's locks |
| `cluster-autoscaler.kubernetes.io/*` | cluster-autoscaler. A slot that stamps `scale-down-disabled` takes its own machine out of the pool's scale-down behind the pool's back |
| `argocd.argoproj.io/*` | Argo CD's tracking and sync annotations |
| `kubectl.kubernetes.io/*` | kubectl's client-side records, first of them `last-applied-configuration` |

The two lists differ on purpose: `node.kubernetes.io/`, `kubernetes.io/` and
`node-role.kubernetes.io/` are reserved for labels only, because nothing stamps an annotation there on
its own; `argocd.argoproj.io/` is reserved for annotations only, so a *label* under it
(`argocd.argoproj.io/team: platform`) is a label like any other — and the label a `nodeSelector`
matches. Declare your own keys in a namespace you control (`pool.example.com/rack`).

A slot object that predates the webhook rule gets the same protection at write time: the key is
skipped, the refusal is recorded on the node object under `infra.homecluster.dev/skipped-node-keys`,
and a `SkippedReservedNodeLabelKey` Warning fires when the set of refused keys changes.

## The consensus rule of the group template

A group answers cluster-autoscaler with **one** node template, and the scale-up that template
authorises buys whichever slot of the group is free. So a declared key reaches the template only when
**every slot of the group declares it with the same value**:

| The group's slots | The template carries |
| --- | --- |
| all declare `zone: fsn1` | `zone: fsn1` |
| one `zone: fsn1`, one `zone: hel1` | no `zone` at all |
| one `zone: fsn1`, one declares nothing | no `zone` at all |
| one slot, declaring `zone: fsn1` | `zone: fsn1` — one slot is the group |
| nobody declares anything | nothing declared |

The consequence is fail-closed, and it is the cheap direction: a pod selecting a disputed key is not
placed on that group at all. The estimator matches the selector against a template that does not carry
the label, plans nothing, and buys no machine. The alternative — answering the group with one slot's
label — is the expensive failure: the plan fits, the provider boots a slot whose machine will never
carry the label, the boot is paid for, and the pod is still `Pending` minutes later with the pool one
machine up.

A disagreement is not an error and does not fail the call: the pool keeps answering every pod that
does not select the disputed key, and a call that returned an error would make cluster-autoscaler
skip the group for every pod. It is reported in the provider's log, per group, when it appears and
when it changes:

```bash
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller-manager \
  | grep "the slots of a group disagree"
```

```
INFO grpc-server NodeGroupTemplateNodeInfo: the slots of a group disagree on their declared node
metadata, the keys named are left out of the group's template so cluster-autoscaler cannot simulate a
label the group does not guarantee {"nodeGroup": "storage-nodes", "disagreements": "labels
[topology.kubernetes.io/zone: \"fsn1\" on storage-01; \"hel1\" on storage-02]"}
```

**Slots the scheduler must tell apart belong in different groups.** A pool is the unit the estimator
sizes, so two zones in one pool is not a pool with two kinds of machine — it is a pool that promises
neither. Split them by `group` label, declare the key with the same value on every slot of each pool,
and the template is exact.

```bash
# the slots of a pool, and what each declares
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -l group=storage-nodes \
  -o custom-columns=NAME:.metadata.name,ZONE:.spec.nodeLabels.topology\.kubernetes\.io/zone
```

Both fields are capped at 64 keys, and admission checks what a CRD pattern cannot: valid label keys
and values, the 256 kB the API server allows on a node object for annotations, and the reserved
namespaces above. A slot that fails is refused at apply, naming the key.

## Related Documentation

- [Node CRD](../api-reference/crds/node.md#node-labels-and-annotations) — every rule in reference form
- [The NodeClaim model](nodeclaim-model.md) — what a slot is, and what a request is
- [A Pod Needs A Slot With A Specific Label](../troubleshooting/common-issues.md#a-pod-needs-a-slot-with-a-specific-label)
  — the runbook, step by step
- [Events](../reference/events.md) — `NodeLabelsApplied`, `SkippedReservedNodeLabelKey`,
  `DeclaredNodeLabelsUnwritten`
