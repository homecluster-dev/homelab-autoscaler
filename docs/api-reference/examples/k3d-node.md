# K3D Node Example

The Node the k3d environment runs: a slot whose power operation is an HTTP call to a server on your
laptop, which is the k3d stand-in for a BMC.

The Job's pod reaches that server through a headless Service in the pool's namespace, whose Endpoints
point at the Docker bridge gateway — `examples/k3d/setup-vm-control-service.sh` creates it, and the
name it creates is what the curl below resolves. Run `examples/k3d/secrets.yaml` first: the ConfigMaps
and the Secret the volumes mount. Without them the mount is empty and the script fails at runtime,
not at apply.

```yaml
# Example Node configuration for a k3d testing environment
apiVersion: infra.homecluster.dev/v2alpha1
kind: Node
metadata:
  name: k3d-homelab-autoscaler-agent-0
  namespace: homelab-autoscaler-system
  labels:
    # The pool this slot belongs to: the label is the membership, and the pool's
    # ceiling is its count of slots.
    group: k3d-workers
spec:
  # The corev1.Node this machine registers as
  kubernetesNodeName: k3d-homelab-autoscaler-agent-0
  pricing:
    # Strings, so they are quoted
    hourlyRate: "1.5"
    podRate: "0.0"
  startupPodSpec:
    image: curlimages/curl:latest
    command:
      - /bin/sh
    args:
      - -c
      - |
        source /config/startup-config.sh
        curl http://vm-control-host.homelab-autoscaler-system.svc.cluster.local:9052/start?vm=k3d-homelab-autoscaler-agent-0
    serviceAccount: controller-manager
    volumes:
      - name: config
        configMapName: startup-config
        mountPath: /config
  shutdownPodSpec:
    image: curlimages/curl:latest
    command:
      - /bin/sh
    args:
      - -c
      - |
        source /config/shutdown-config.sh
        curl http://vm-control-host.homelab-autoscaler-system.svc.cluster.local:9052/stop?vm=k3d-homelab-autoscaler-agent-0
    serviceAccount: controller-manager
    volumes:
      - name: config
        configMapName: shutdown-config
        mountPath: /config
      - name: auth
        secretName: auth-secrets
        mountPath: /auth
```

## What This Shape Is Not

- **No `env`, no `resources`, no `volumeMounts`.** `startupPodSpec` is a `MinimalPodSpec`
  (`image`, `command`, `args`, `volumes`, `serviceAccount`, `priorityClassName`,
  `ttlSecondsAfterFinished`). The API server **prunes** a field it does not know, silently, so a pod
  spec written against Kubernetes' container shape loses its environment and its mounts and the script
  runs with no credentials. Pass configuration in through a mounted ConfigMap or Secret and read it in
  the script, as above
- **A volume is `{name, mountPath, secretName}` or `{name, mountPath, configMapName}`** — not
  Kubernetes' nested `secret: {secretName: …}`
- **Not `host.k3d.internal:8080`.** The k3d host alias resolves, and the VM control server of this
  repo's suite listens on **9052**; in-cluster jobs reach it through the headless Service above,
  which is the shape that keeps working when the Job pod lands on any node of the cluster
- **`serviceAccount: controller-manager`** is the chart's ServiceAccount in the pool's namespace.
  Name one that exists there, with the rights your script needs

## The Server On The Other End

`examples/k3d/vm_control_server.py` is the power API: an HTTP server on port 9052 that answers
`/start?vm=<name>` and `/stop?vm=<name>` with `k3d node start|stop`. Start it before you apply the
slots, and the pair of scripts wires the cluster to it:

```bash
cd examples/k3d
nohup python3 vm_control_server.py > /tmp/vm-control.log 2>&1 &
./setup-vm-control-service.sh            # the headless Service + Endpoints, port 9052
kubectl apply -f secrets.yaml            # startup-config, shutdown-config, auth-secrets
kubectl apply -f group1.yaml             # the Group of this pool
kubectl apply -f nodes1.yaml             # agent-0 and agent-1
```

## Related Documentation

- [Node CRD Reference](../crds/node.md) — every field of a slot
- [K3D Group Example](k3d-group.md)
- [Production Examples](production-examples.md) — the same shape against IPMI, WoL and a PDU
- [Quick Start](../../getting-started/quick-start.md)
