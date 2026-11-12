# Production Examples

Configurations for physical machines: wake-on-LAN, IPMI, a smart PDU. Each one is the shape the CRD
accepts, so what you apply is what runs.

`spec.startupPodSpec` and `spec.shutdownPodSpec` are a `MinimalPodSpec`: `image`, `command`, `args`,
`volumes`, `serviceAccount`, `priorityClassName`, `ttlSecondsAfterFinished`. There is no `env`, no
`resources`, no `volumeMounts`, no `nodeSelector`. A structural schema **prunes** a field it does not
know, silently, at `kubectl apply`: a script written against a Kubernetes container spec loses its
`env` and its Secret and boots a machine with no credentials, and the only symptom is a claim that
fails at its deadline. Credentials come from a mounted Secret, read by your script.

## Production Group

Conservative windows, and the pool's ceiling is its slots.

```yaml
# group.yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: worker-nodes
  namespace: homelab-autoscaler-system
spec:
  # 0 lets the pool scale to nothing; 1 keeps one machine up without a keepalive pod.
  minSize: 0
  # Strings, so they are quoted. 0.5 unquoted is rejected by the API server, and "30" on
  # this ratio field is 3000%: a number cluster-autoscaler compares against a utilization
  # between 0 and 1.
  scaleDownUtilizationThreshold: "0.3"
  scaleDownGpuUtilizationThreshold: "0.3"
  scaleDownUnneededTime: "30m"
  scaleDownUnreadyTime: "20m"
  # The budget of one boot: cluster-autoscaler's own termination deadline for the scale-up,
  # and the floor of every claim's spec.deadlineAt in this pool.
  maxNodeProvisionTime: "15m"
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: true
```

There is no `maxSize`. The pool can have as many machines up as it has `Node` CRs labelled
`group: worker-nodes`: the ceiling is the slot count, and the pool grows and shrinks by committing
slots in Git. A Group that names no slots is a pool with `SLOTS 0`: it can never scale up, and
`kubectl get groups` says so in its first column.

## Wake-on-LAN Startup, SSH Shutdown

A magic packet needs no credentials; the SSH shutdown needs a key, so it comes from a Secret volume.

```yaml
# worker-01.yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: worker-01
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
spec:
  kubernetesNodeName: worker-01.homelab.local
  pricing:
    hourlyRate: "2.50"   # strings, so they are quoted
    podRate: "0.01"
  # What the kubelet reports for this machine, so the pool scales up from zero slots.
  nodeCapacity:
    cpu: 3780m
    memory: 15.6Gi
  nodeLabels:
    topology.kubernetes.io/zone: rack-1
  startupPodSpec:
    image: alpine/socat:latest
    command: ["/bin/sh"]
    args:
      - -c
      - |
        set -eu
        # the magic packet: six 0xFF bytes, then the MAC repeated sixteen times
        mac=$(cat /etc/wol/mac)
        ip=$(cat /etc/wol/ip)
        mac_hex=$(echo "$mac" | sed 's/:/\\x/g; s/^/\\x/')
        printf '\xff\xff\xff\xff\xff\xff' > /tmp/wol
        for i in $(seq 16); do printf "$mac_hex" >> /tmp/wol; done
        cat /tmp/wol | socat - UDP-DATAGRAM:192.168.1.255:9,broadcast
        # Exit non-zero when the machine did not wake: a Job that exits 0 is not evidence
        # that the power-on landed, and the claim's deadline is what fails the attempt.
        sleep 90
        ping -c 3 -W 2 "$ip"
    volumes:
      - name: wol
        secretName: worker-01-wol
        mountPath: /etc/wol
    serviceAccount: controller-manager
  shutdownPodSpec:
    image: alpine/ssh:latest
    command: ["ssh"]
    args:
      - -i
      - /etc/ssh-keys/id_ed25519
      - -o
      - StrictHostKeyChecking=no
      - -l
      - automation
      - worker-01.homelab.local
      - shutdown -h now
    volumes:
      - name: ssh-keys
        secretName: ssh-automation-keys
        mountPath: /etc/ssh-keys
    serviceAccount: controller-manager
```

The Secret in the same namespace as the slot:

```yaml
# worker-01-wol.yaml
apiVersion: v1
kind: Secret
metadata:
  name: worker-01-wol
  namespace: homelab-autoscaler-system
type: Opaque
stringData:
  mac: "00:11:22:33:44:55"
  ip: "192.168.1.24"
```

## IPMI

The BMC's user and password are Secret files; the script reads them off the mount. Put them in the
password file's mode (`defaultMode: 0400` on a Kubernetes Secret volume is not a field
`MinimalPodSpec` has, so set it on the Secret's consumers or keep the file readable only by the pod's
user).

```yaml
# worker-02.yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: worker-02
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
spec:
  kubernetesNodeName: worker-02.homelab.local
  pricing:
    hourlyRate: "3.00"
    podRate: "0.01"
  nodeCapacity:
    cpu: 7860m
    memory: 31.3Gi
  nodeLabels:
    topology.kubernetes.io/zone: rack-1
  startupPodSpec:
    image: gemtest/ipmitool:latest
    command: ["/bin/sh"]
    args:
      - -c
      - |
        ipmitool -I lanplus -H worker-02-ipmi.homelab.local \
          -U "$(cat /etc/ipmi/user)" -P "$(cat /etc/ipmi/password)" power on
        # Power on is not a boot: confirm the chassis, and fail the Job when it is not on.
        ipmitool -I lanplus -H worker-02-ipmi.homelab.local \
          -U "$(cat /etc/ipmi/user)" -P "$(cat /etc/ipmi/password)" power status | grep -q "Power: on"
    volumes:
      - name: ipmi-credentials
        secretName: ipmi-credentials
        mountPath: /etc/ipmi
    serviceAccount: controller-manager
  shutdownPodSpec:
    image: gemtest/ipmitool:latest
    command: ["/bin/sh"]
    args:
      - -c
      - |
        ipmitool -I lanplus -H worker-02-ipmi.homelab.local \
          -U "$(cat /etc/ipmi/user)" -P "$(cat /etc/ipmi/password)" power soft
        # A shutdown Job that exits 0 does not power the machine off: the controller reads the
        # machine, and a machine that stays up spends the scale-down's three attempts.
        sleep 60
        ! ipmitool -I lanplus -H worker-02-ipmi.homelab.local \
          -U "$(cat /etc/ipmi/user)" -P "$(cat /etc/ipmi/password)" power status | grep -q "Power: on"
    volumes:
      - name: ipmi-credentials
        secretName: ipmi-credentials
        mountPath: /etc/ipmi
    serviceAccount: controller-manager
```

```yaml
# ipmi-credentials.yaml
apiVersion: v1
kind: Secret
metadata:
  name: ipmi-credentials
  namespace: homelab-autoscaler-system
type: Opaque
stringData:
  user: "automation"
  password: "replace-me"
```

## GPU Worker On A Smart PDU

An outlet is power, and a hard cut is why this slot declares a floor: the pool must not reclaim the
machine while a training run holds it, so the workload, not only the utilization, decides
`spec.minSize` and the Group's windows.

```yaml
# gpu-worker-01.yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: gpu-worker-01
  namespace: homelab-autoscaler-system
  labels:
    group: gpu-workers
spec:
  kubernetesNodeName: gpu-worker-01.homelab.local
  pricing:
    hourlyRate: "5.00"
    podRate: "0.05"
  # The resources a GPU worker's pods ask for: declare every one of them, because a resource
  # you leave out is a resource the group's template has no size for.
  nodeCapacity:
    cpu: 15700m
    memory: 62.5Gi
    nvidia.com/gpu: "1"
  nodeLabels:
    pool.homecluster.dev/gpu: "rtx-4090"
  nodeTaints:
    - key: dedicated
      value: gpu
      effect: NoSchedule
  startupPodSpec:
    image: alpine/curl:latest
    command: ["/bin/sh"]
    args:
      - -c
      - |
        curl --fail --silent --show-error \
          --user "$(cat /etc/pdu/user):$(cat /etc/pdu/password)" \
          "http://pdu-01.homelab.local/outlet/3/on"
    volumes:
      - name: pdu-credentials
        secretName: pdu-credentials
        mountPath: /etc/pdu
    serviceAccount: controller-manager
    priorityClassName: system-cluster-critical
    ttlSecondsAfterFinished: 3600
  shutdownPodSpec:
    image: alpine/curl:latest
    command: ["/bin/sh"]
    args:
      - -c
      - |
        # let the node drain first: cluster-autoscaler evicts the pods before it calls DeleteNodes,
        # and the machine's own grace is the pod's terminationGracePeriodSeconds
        sleep 60
        curl --fail --silent --show-error \
          --user "$(cat /etc/pdu/user):$(cat /etc/pdu/password)" \
          "http://pdu-01.homelab.local/outlet/3/off"
    volumes:
      - name: pdu-credentials
        secretName: pdu-credentials
        mountPath: /etc/pdu
    serviceAccount: controller-manager
    priorityClassName: system-cluster-critical
    ttlSecondsAfterFinished: 3600
```

The GPU pool is a Group of its own, not a labelled subset of the CPU pool: a Group answers
cluster-autoscaler with **one** node template, and a label its slots disagree on is left out of it, so
slots the scheduler must tell apart belong in separate pools.

```yaml
# gpu-workers.yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: Group
metadata:
  name: gpu-workers
  namespace: homelab-autoscaler-system
spec:
  minSize: 0
  scaleDownUtilizationThreshold: "0.3"
  scaleDownGpuUtilizationThreshold: "0.3"
  scaleDownUnneededTime: "60m"
  scaleDownUnreadyTime: "30m"
  maxNodeProvisionTime: "15m"
  zeroOrMaxNodeScaling: false
  ignoreDaemonSetsUtilization: true
```

## Power One Machine By Hand

There is no field on a `Node` to patch. A request is a `NodeClaim`, and the cancellation of one is the
power-off. Stamp the budget: the provider computes `spec.deadlineAt` when it creates a claim, and
nothing fills it in for a claim you write — a hand-made claim with no `deadlineAt` never expires.

```yaml
# worker-01-manual.yaml
apiVersion: infra.homecluster.dev/v2alpha1
kind: NodeClaim
metadata:
  name: worker-01-manual
  namespace: homelab-autoscaler-system
  labels:
    group: worker-nodes
    infra.homecluster.dev/node: worker-01
spec:
  slotRef:
    name: worker-01
  requestedBy: filippo
  request:
    call: IncreaseSize
    group: worker-nodes
  requestedAt: "2026-03-04T09:38:15Z"   # now, RFC3339
  deadlineAt: "2026-03-04T09:50:25Z"    # requestedAt + the 12m10s of one attempt
```

```bash
kubectl apply -f worker-01-manual.yaml
kubectl get nodeclaim -n homelab-autoscaler-system -w

# Power it off: cancel the request. A pending claim is deleted outright; a booting/ready claim runs
# cordon, the shutdown Job and the power-off through its finalizer, and ends completed.
kubectl delete nodeclaim worker-01-manual -n homelab-autoscaler-system
```

## What To Check Before You Trust A Pool

- **The script's exit code is the contract.** A startup Job that exits 0 without the machine on is a
  boot that dies at `deadlineAt`; a shutdown Job that exits 0 without the machine off spends the
  scale-down's three attempts and then pauses for 30 minutes. Confirm the state you asked for, and
  exit non-zero when it is not there
- **The Secret is in the pool's namespace.** A `secretName` that resolves to nothing mounts nothing,
  and the script fails at runtime, not at apply
- **The ServiceAccount exists in the pool's namespace** and is allowed to do what the script does to
  the cluster (read nodes, patch a slot, call a power API). `controller-manager` is the chart's own and
  has the rights the operator needs
- **The network path exists from a pod.** The script runs in the cluster, not on your laptop: the BMC,
  PDU or SSH endpoint must be reachable from a pod on the node the Job lands on
- **`spec.pricing` is quoted, `spec.nodeCapacity` is what the kubelet reports**, not what the box was
  sold as: the autoscaler sizes its simulation from that number, and a `NodeCapacityMismatch` Warning
  on the slot is the drift telling you so

## Related Documentation

- [Node CRD Reference](../crds/node.md) - every field of a slot, and the pruning rules
- [Group CRD Reference](../crds/group.md) - the seven fields, and the slot count as the ceiling
- [NodeClaim CRD Reference](../crds/nodeclaim.md) - the request object
- [K3D Examples](k3d-group.md) - the same shapes on a laptop, with a power API you can run
- [Common Issues & Solutions](../../troubleshooting/common-issues.md) - the runbooks
