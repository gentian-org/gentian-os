# Migrating a cluster to a new node flavour

**Audience:** cluster operators.
**Companion docs:** [design/operations.md](design/operations.md), [tenant-backup-guide.md](tenant-backup-guide.md),
[design/resource-plans.md](design/resource-plans.md).

Moving a running cluster onto different worker nodes — usually to a larger or
memory-heavier flavour. The workloads move by draining; the nodes are replaced by
adding one instance pool and deleting another.

This is a **maintenance window, not a rolling upgrade**. The kernel runs one replica
of most services, so each one restarts as its node is drained.

---

## 1. Why flavour matters more than count

The kubelet withholds a reserve that is **front-loaded onto the first 4 GiB** of a
node, so a small node pays the steepest tier across its whole capacity:

```
reserve = 25% of first 4 GiB
        + 20% of next 4 GiB
        + 10% of next 8 GiB
        +  6% beyond 16 GiB
        + 100 MiB eviction threshold
```

Measured on Infomaniak KaaS:

| Flavour | Purchased | Allocatable | Usable after DaemonSets | Fraction |
|---|---|---|---|---|
| `a2-ram4-disk50-perf1` | 3.82 GiB | 2.77 GiB | 2.44 GiB | **63.7%** |
| `a4-ram16-disk50-perf1` | 15.62 GiB | 12.96 GiB | 12.62 GiB | **80.8%** |

DaemonSets (Cilium, cilium-envoy, konnectivity, Cinder CSI, OpenStack CCM) cost a
roughly fixed ~340 MiB per node, so their share also falls as nodes grow.

**Size the target from usable memory, not purchased memory:**

```
nodes ≈ max( ⌈total_requests ÷ usable_per_node⌉ , 3 )
```

The floor of 3 is redundancy, not capacity. Past a certain node size a Gentian
cluster is sized by failure tolerance rather than by how much it runs — verify the
target survives losing one node, and pick that number.

Read current requests with:

```bash
kubectl get pods -A -o json | jq -r '
[.items[] | select(.status.phase=="Running" or .status.phase=="Pending") |
 ([.spec.containers[].resources.requests.memory // "0"] |
  map(if test("Mi$") then (.[:-2]|tonumber)
      elif test("Gi$") then (.[:-2]|tonumber*1024)
      elif test("Ki$") then (.[:-2]|tonumber/1024) else 0 end) | add)]
| add | "total memory requests: \(./1024*100|round/100) GiB"'
```

---

## 2. Before starting

**Take a tenant backup.** See [tenant-backup-guide.md](tenant-backup-guide.md). A
`TenantExport` pauses the tenant while it runs, so do it before the window, not
during.

**Record the starting state**, so "did we break that?" is answerable afterwards:

```bash
kubectl get nodes -o wide          > /tmp/pre-migration-nodes.txt
kubectl get pods -A -o wide        > /tmp/pre-migration-pods.txt
kubectl get pvc -A                 > /tmp/pre-migration-pvc.txt
kubectl get applications -n argocd > /tmp/pre-migration-apps.txt
```

Note which Argo applications are already `OutOfSync` and which ExternalSecrets are
already failing. Both are normal on a healthy cluster, and mistaking pre-existing
drift for migration damage costs more time than the migration.

### 2.1 Check the provider quotas first

Two quotas bound this procedure, and neither is about capacity:

| Quota | Why it bites |
|---|---|
| **Neutron ports** | Every instance needs one. At the ceiling, new nodes boot with no network: their kubelets never reach the API server, no CSR is ever created, and the console still reports them healthy. |
| **Volumes / storage** | Unchanged by the migration, but worth confirming before adding nodes. |

Raising the port quota before starting is the simplest path — ports cost nothing —
and it removes the need for §4's two-wave structure entirely.

### 2.2 Availability zone

**The new pool must be in the same AZ as the existing volumes.** Cinder volumes are
AZ-bound: a node in another zone cannot run any stateful workload, and the pods sit
in `ContainerCreating` indefinitely.

```bash
kubectl get pv -o json | jq -r '[.items[].spec.nodeAffinity.required.nodeSelectorTerms[]?.matchExpressions[]?|select(.key|test("zone"))|.values[]]|unique'
```

### 2.3 Argo CD self-heal

Applications are configured with `selfHeal: true` and `prune: true`. **Any live
`kubectl patch` or `kubectl scale` on an Argo-managed resource is reverted**, often
mid-procedure. Everything in this runbook either avoids Argo-managed state or goes
through git.

---

## 3. Create the new pool

A KaaS instance pool has a single `flavor_name`, so a new flavour means a **second
pool**, not an edit to the existing one.

```hcl
resource "infomaniak_kaas_instance_pool" "workers_new" {
  kaas              = infomaniak_kaas.cluster.id
  name              = "<cluster>-<suffix>"
  flavor_name       = "a4-ram16-disk50-perf1"
  availability_zone = "az-1"        # must match §2.2
  min_instances     = 3
  max_instances     = 4             # headroom during the drain
}
```

Nodes take several minutes to boot, join and become `Ready`. **The console reporting
them healthy is not the same as them joining the cluster** — the console reflects the
instance record. The authoritative check is:

```bash
kubectl get nodes -L node.kubernetes.io/instance-type
```

If nodes never appear, check for pending CSRs (`kubectl get csr`). No CSR at all means
no kubelet has reached the API server — look at §2.1's port quota before anything else.

Confirm the reserve on a new node matches the §1 estimate before relying on it:

```bash
kubectl get node <new-node> -o jsonpath='{.status.allocatable.memory}{"\n"}'
```

---

## 4. Two-wave migration, when ports are scarce

Skip this section if the port quota was raised. Otherwise the new pool cannot start
until the old one gives ports back, and the migration runs in two waves:

1. Cap the **old** pool's min and max at its current count minus the number of nodes
   being freed. **Do this before deleting anything** — otherwise the autoscaler
   reclaims the freed ports for the old flavour the moment a pod is unschedulable.
2. Cordon and drain that many old nodes, choosing ones carrying no kernel singleton.
3. Delete those instances explicitly, by UUID. Draining does not release a port —
   only deleting the instance does.

   ```bash
   kubectl get nodes -o custom-columns='NAME:.metadata.name,PROVIDER:.spec.providerID'
   ```

4. The new nodes now join. Continue with §5.

Sizing the first wave: the remaining old nodes must still hold everything. A pod
larger than the largest remaining free block stays `Pending` until the new nodes
arrive — acceptable for a tenant app, not for a kernel service.

---

## 5. Drain the old nodes

Cordon every old node first, so nothing evicted lands back on one:

```bash
kubectl cordon <each-old-node>
```

Then drain one at a time, verifying between each:

```bash
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data --timeout=420s
kubectl get pods -A --no-headers | grep -vE "Running|Completed"
```

### 5.1 Order

Three rules decide the order:

| Rule | Why |
|---|---|
| Least-loaded node first | Proves the mechanism before anything critical moves |
| Never drain both Envoy gateway replicas in succession | The second only starts after the first is `Ready`, and the edge is down in between |
| `openbao-transit` before `openbao-0` | The primary unseals through transit; transit must be up and unsealed first |
| CNPG Postgres last | It needs §5.3's manual step, and it is what everything else depends on |

Between the two Envoy drains, wait for the moved replica explicitly:

```bash
kubectl wait --for=condition=Ready pod -n envoy-gateway-system \
  -l gateway.envoyproxy.io/owning-gateway-name=kernel-public-gateway --timeout=300s
```

### 5.2 OpenBao unseals itself

No manual unseal step is needed. `openbao-transit` carries a `postStart` hook that
unseals it from the `openbao-transit-unseal` Secret, and `openbao-0` then unseals
through transit. Verify after each moves:

```bash
kubectl -n openbao exec openbao-transit-0 -- bao status | grep -E 'Sealed|Initialized'
kubectl -n openbao exec openbao-0 -- bao status -tls-skip-verify | grep -E 'Sealed|Initialized'
```

Both must report `Sealed false`. If the primary is sealed, transit was not ready when
it started; restarting the primary is enough.

### 5.3 CNPG Postgres needs a manual delete

The kernel Postgres cluster runs `instances: 1`, so its `postgres-primary`
PodDisruptionBudget permits **zero** disruptions and `kubectl drain` blocks
indefinitely. CNPG cannot help: it logs *"Primary is running on an unschedulable
node, will try switching over"* and then *"there are no valid candidates"*, because
there is no replica.

Let the drain move everything else, then in another terminal:

```bash
kubectl delete pod <postgres-pod> -n platform-kernel
```

A direct delete bypasses the PDB — PDBs guard the Eviction API, not deletion. CNPG
recreates the pod on an uncordoned node and reattaches the PVC.

**Expect several minutes.** `terminationGracePeriodSeconds` is 1800, and Postgres
completes a graceful shutdown — including archiving its final WAL segment — before
exiting. The kernel database is unavailable throughout, so dependent services log
connection errors; this is expected, not a fault. Do not force-delete unless the
grace deadline actually passes.

Confirm afterwards:

```bash
kubectl get cluster.postgresql.cnpg.io -n platform-kernel postgres \
  -o jsonpath='{.status.phase}{"  ready="}{.status.readyInstances}{"\n"}'
```

A zero-downtime alternative is to raise `instances` to 2 in git, let the replica sync,
drain (CNPG fails over), then revert. It buys little during a window in which every
other kernel singleton restarts anyway.

### 5.4 Deployments sharing one RWO volume

Where two Deployments mount the same `ReadWriteOnce` PVC — the Odoo app and its
addons sidecar are the current example — they can only run **on the same node**. A
drain evicts them independently, and they frequently land apart, deadlocking with:

```
Multi-Attach error for volume "pvc-..." Volume is already used by pod(s) ...
```

Force them back together by cordoning the node that must *not* receive the pod:

```bash
kubectl cordon <other-node>
kubectl delete pod -n <ns> <the-displaced-pod>
kubectl uncordon <other-node>
```

Then confirm both pods and the volume attachment agree on one node:

```bash
kubectl get volumeattachment -o json | jq -r \
  '.items[]|select(.spec.source.persistentVolumeName=="<pv>")|"node=\(.spec.nodeName)"'
```

The durable fix is to stop splitting them — run the sidecar as a container in the
app's own pod, as the Odoo git-sync and Activepieces git content already do.

### 5.5 The Cinder CSI controller restarts noisily

`openstack-cinder-csi-controllerplugin` is a single-replica Deployment that gets
evicted like anything else. Its containers restart and re-run leader election, which
surfaces briefly as `CrashLoopBackOff`. It settles on its own. Confirm before
continuing, because nothing else can attach a volume while it is down:

```bash
kubectl -n kube-system logs deploy/openstack-cinder-csi-controllerplugin -c csi-attacher --tail=5
```

Healthy output shows `became leader` followed by `Attached`/`Detached` lines.

---

## 6. Verify before deleting the old pool

The old pool is the rollback path. Keep it until the platform is proven.

```bash
kubectl get pods -A --no-headers | grep -vE "Running|Completed"   # expect empty
kubectl get pvc -A                                               # all Bound
kubectl get applications -n argocd | grep -v Synced               # compare to §2's baseline
kubectl get svc -A --field-selector spec.type=LoadBalancer        # external IPs unchanged
```

Then exercise the platform: log in to the portal (Keycloak), open an app that uses
both Postgres and MinIO, and send a test mail.

Confirm the old nodes are genuinely empty — only DaemonSets should remain:

```bash
kubectl get pods -A --field-selector spec.nodeName=<old-node> --no-headers \
  | grep -vE "Completed" | grep -v kube-system
```

### 6.1 Rollback

Until the old pool is deleted, rollback is complete and cheap:

```bash
kubectl uncordon <each-old-node>
```

Then delete pods you want rescheduled back. Volumes use `Retain`, so nothing is lost.

---

## 7. Finish

1. Delete the old instance pool. This frees its ports.
2. Raise the new pool to its target size, now that ports are available.
3. Drop `max_instances` back to the target.

```bash
kubectl get nodes -L node.kubernetes.io/instance-type
```

Until the final node joins, the cluster has no failure tolerance — the workload fits,
but losing a node leaves pods `Pending`. Close that gap promptly.

---

## 8. Node labels

`gentianos.io/mail-egress` looks like it pins mail egress to a node, and does not.
The Postfix chart emits a nodeSelector for it **only when `egressHost` is set**; with
`egressHost` empty and `mailServiceMode: external`, Postfix relays through a smarthost
and no pin exists. Nothing else in the cluster reads the label.

Before treating any node label as load-bearing, confirm something consumes it:

```bash
kubectl get all -A -o json | grep -c "<label>"
```
