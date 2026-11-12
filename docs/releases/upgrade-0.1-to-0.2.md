---
sidebar_position: 2
---

# Upgrade 0.1 to 0.2

`0.2.0` moved the API to `v2alpha1` and removed the fields `v1alpha1` used to request a machine. It
is a breaking release, and the migration is a cut, not a rolling conversion: the pool goes to zero,
the objects are recreated, and the machines come back on demand.

| Fact | Consequence for you |
| --- | --- |
| `Node.spec.powerState` is removed | every `Node` manifest in Git is edited |
| `Node.status.progress` is removed | read it once, before you start; after the CRDs land it answers `<none>` |
| `v1alpha1` is out of the served set | a `0.1.x` operator cannot watch the cluster once the CRDs are applied |
| There is no conversion webhook | `spec.powerState: on` has no honest conversion: it is a request with no attribution, no deadline and no attempt |
| `NodeClaim` is new | the CRD set is three files, not two |

Plan a window in which the pool is empty. The downtime is **one boot**: from the operator step until
the first scale-up brings a machine back. Upgrade to the current chart (`0.2.3`) directly; the
migration has not changed shape since `0.2.0`.

## Prerequisites

- You can leave the pool at zero for the length of one boot. Do this on a pool that may be empty, not
  on the one that serves your family's Plex.
- `kubectl` and `helm` pointed at the cluster, and write access to the CRDs.
- The GitOps repo that declares the pool's `Node` manifests, editable in the same window.
- The chart's CRD values understood: `crd.enable: true` renders the three CRDs as **templates**, so
  `helm upgrade` replaces their schemas; `crd.keep: true` keeps them through `helm uninstall`. A chart
  that carries CRDs in a `crds/` directory never replaces them on upgrade and never drops a served
  version — if you installed that way, the CRDs on the cluster are yours to edit by hand.

## 1. Quiesce, and record what is in flight

```bash
kubectl scale deployment/homelab-autoscaler-cluster-autoscaler -n homelab-autoscaler-system --replicas=0

# The 0.1.22 fields. After step 2 they are gone from the schema, and the same read answers <none>.
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system -o custom-columns=\
SLOT:.metadata.name,CLAIM:.spec.powerState,OBSERVED:.status.powerState,PROGRESS:.status.progress
```

Every slot that reads `on` is a machine the autoscaler asked for. Note them: after step 5 the pool is
empty by design, and the requests that refill it come from workload demand.

## 2. CRDs

Check what the cluster stores before you replace anything:

```bash
kubectl get crd nodes.infra.homecluster.dev \
  -o jsonpath='{.spec.versions[*].name}{"\n"}{.status.storedVersions}{"\n"}'
kubectl get nodes.infra.homecluster.dev -A -o jsonpath=\
'{range .items[*]}{.apiVersion} {.metadata.namespace}/{.metadata.name}{"\n"}{end}'
```

The API server refuses a CRD update that drops the storage version while objects are still stored
under it. Two ways out, and both mean the same thing for your data, because the pool goes to zero
anyway:

- **Delete the objects first.** They are the slots, and step 4 recreates them from Git:
  `kubectl delete nodes.infra.homecluster.dev -n homelab-autoscaler-system --all` (same for `groups`
  if you re-apply them too).
- **Step through a merged CRD.** Apply a CRD that serves `v1alpha1` and `v2alpha1` with `v2alpha1` as
  the storage version, let the existing objects be read as `v2alpha1`, delete them, then apply the
  chart's CRD set. This is the path that keeps objects readable in the middle, and it is the slower
  one.

Then apply the `0.2.3` CRDs:

```bash
helm upgrade homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system --version 0.2.3 \
  --set controllerManager.replicas=0
```

`--set controllerManager.replicas=0` stages the release with no manager running: the same upgrade
stops the `0.1.x` pods and installs the `0.2.3` ones at zero replicas, so nothing is watching a schema
that is being replaced underneath it. Step 3 turns the manager on. Verify before you continue — the
storage version is the thing that has to be clean:

```bash
kubectl get crd nodes.infra.homecluster.dev -o jsonpath='{.status.storedVersions}{"\n"}'   # ["v2alpha1"]
kubectl get crd groups.infra.homecluster.dev nodes.infra.homecluster.dev nodeclaims.infra.homecluster.dev
```

**A stale `metadata.managedFields`.** An object whose managed-fields entries name
`infra.homecluster.dev/v1alpha1` can be refused by a write after that version is gone, with a
conversion error naming `v1alpha1`. The procedure recreates the slots (step 4), which clears it. If
you restore from a `kubectl get -o yaml` backup, strip `metadata.managedFields` (and
`metadata.creationTimestamp`, `metadata.uid`, `metadata.resourceVersion`) from the file you apply.

## 3. The operator

```bash
helm upgrade homelab-autoscaler homelab-autoscaler/homelab-autoscaler \
  --namespace homelab-autoscaler-system --version 0.2.3 \
  --values values.yaml

kubectl scale deployment/homelab-autoscaler-controller-manager -n homelab-autoscaler-system --replicas=1
kubectl rollout status deployment/homelab-autoscaler-controller-manager -n homelab-autoscaler-system
```

Scale to the replica count your values set — one, or two if `controllerManager.replicas: 2` and the
manager elects a leader, which makes the second a hot standby.

If you split the CRD apply and the operator start into two commands, do them inside one window. A
running `0.1.x` operator loses its watches the instant
`v1alpha1` stops being served: its informers fail with
`no matches for kind Node in version infra.homecluster.dev/v1alpha1` and the manager restarts. The
gRPC server runs in the same binary, so scale-up requests fail until this step lands. Nothing is
powered off in that window — a machine that is up keeps serving pods.

Expect the operator to power machines off on its first pass, and read it as correct: it reads the
existing slots through `v2alpha1`, finds no claims, and applies the law — **a machine observed up with
no claim is unwanted**. That is why step 1 exists.

## 4. The Node manifests

Re-apply every slot as `v2alpha1`, with no `spec.powerState`. Because `v1alpha1` is gone and there is
no conversion, the objects are **recreated, not patched**: a value written under a removed field
stays in etcd and reappears if the field is ever added back to a schema.

```yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: worker-01
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
spec:
  kubernetesNodeName: worker-01
  pricing:
    hourlyRate: "0.05"
    podRate: "0.001"
  nodeCapacity:
    cpu: 1950m
    memory: 3.2Gi
  startupPodSpec:
    image: gemtest/ipmitool:latest
    command: ["ipmitool"]
    args: ["-I", "lanplus", "-H", "worker-01-ipmi.homelab.local", "-U", "admin", "-P", "password", "power", "on"]
  shutdownPodSpec:
    image: gemtest/ipmitool:latest
    command: ["ipmitool"]
    args: ["-I", "lanplus", "-H", "worker-01-ipmi.homelab.local", "-U", "admin", "-P", "password", "power", "soft"]
```

```bash
kubectl delete nodes.infra.homecluster.dev -l group=worker-nodes -n homelab-autoscaler-system
kubectl apply -f apps/homelab-autoscaler/worker-nodes/nodes.yaml
```

Two things are load-bearing in the names you re-apply:

- `metadata.name` and `spec.kubernetesNodeName`. The provider looks a slot up by name, and
  `spec.providerID` is `homelab://<slot>`: a renamed slot matches no `corev1.Node`, and its machine
  becomes invisible to the pool.
- The `group` label. The pool's members are the Node CRs labelled `group: <name>`, and **the count of
  them is the pool's ceiling**. A partial re-apply lowers the ceiling of the pool.

With Argo CD, a sync policy that is not `Replace` with prune off keeps the pruned `v1` objects alive:
a slot that exists in Git and not in the cluster is capacity that will never be claimed.

## 5. The pool goes to zero, then scales on demand

With `v2alpha1` slots, no claims and no autoscaler, every slot is free and every machine is off — the
idle state of a homelab pool. Bring cluster-autoscaler back and watch the first request:

```bash
kubectl scale deployment/homelab-autoscaler-cluster-autoscaler -n homelab-autoscaler-system --replicas=1
kubectl get nodeclaim -n homelab-autoscaler-system -w
```

`pending` → `booting` → `ready` inside `spec.deadlineAt`, then the pending pod binds. If a claim
reaches `failed`, read `status.reason` and `Node.status.lastClaim` before you touch anything: that
pair is the record this release exists to produce.

```bash
kubectl get groups -n homelab-autoscaler-system
kubectl get nodes.infra.homecluster.dev -n homelab-autoscaler-system
kubectl describe nodes.infra.homecluster.dev worker-01 -n homelab-autoscaler-system
```

## Roll forward, not back

Rolling a cluster back to `0.1.x` means restoring the `v1alpha1` CRD by hand (Helm does not downgrade
CRDs), re-adding `spec.powerState` to every manifest, and living with the claims the operator left
behind. It is not a supported path. Do the upgrade on a pool that can be empty for ten minutes.

## Related Documentation

- [Changelog](changelog.md) — what each release changed
- [Installation](../getting-started/installation.md#upgrading) — the routine upgrade inside `0.2.x`
- [The NodeClaim model](../concepts/nodeclaim-model.md) — the model you are moving to
- [NodeClaim design](../development/nodeclaim.md#9-migration) — the reasoning behind the cut
