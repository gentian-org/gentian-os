# Resource plans and usage — design

**Status:** v1  
**Scope:** How a tenant's resource ceiling is chosen, enforced, recorded and invoiced.

**Companion docs:** [admin-console.md](admin-console.md), [multi-tenancy.md](multi-tenancy.md), [app-catalogue.md](app-catalogue.md), [operations.md](operations.md), [commands.md](../commands.md).

---

## 1. Purpose

A tenant runs under a ceiling: how much CPU, memory and storage its apps may
claim between them. Before this, that ceiling existed — `Tenant.spec.quotas`
became a `tenant-quota` ResourceQuota in the tenant namespace — but three things
were missing, and each one cost something different:

| Missing | Cost |
|---|---|
| A way for a tenant to change it | Every resize was a cluster operator editing YAML |
| A record of what was in force, when | A month could not be invoiced from the platform's own state |
| Any observation of what was used underneath it | Nobody could tell an over-provisioned tenant from a tight one |

This design adds all three, and adds them the way the App Store adds an app: a
**named choice** from a **priced catalogue**, written to the deployments
repository through an **API shared by the CLI and the console**.

---

## 2. Why a catalogue, and not a number

`Tenant.spec.quotas` accepts any quantity. That is right for the file, and wrong
for the API: a ceiling that can be any number can be any number that was never
sold. So the write path accepts a **plan name and nothing else**, and every
ceiling reachable through the API or the console is one of a known, priced set.

That single constraint is what makes the usage report invoiceable. A month
resolves to *"one node from the 1st to the 17th, two from the 18th"* — plans
and SKUs, not quantities somebody downstream has to interpret and price.

Free-form quantities remain available to whoever edits the deployments
repository by hand, which is the cluster operator, which is the person entitled
to make an unpriced choice. The console reports such a tenant as **custom** and
says nothing prices it.

`ResourcePlan` is deliberately **not deployable** — nothing reconciles it, it
owns no workload. It is the same shape [`AppPackage`](../../api/v1alpha1/apppackage_types.go)
takes for addons: a curated preset that replaces a combinatorial choice with a
named one, without becoming an artifact of its own.

```yaml
apiVersion: gentianos.io/v1alpha1
kind: ResourcePlan
metadata:
  name: nodes-2
spec:
  displayName: 2 nodes
  tier: 10                       # ordering; quantities cannot imply "bigger"
  productSku: gentian-resources-nodes-2
  quotas:
    requestsCpu: "4"             # sold — reserved capacity, two node units
    requestsMemory: 8Gi
    cpu: "16"                    # burst ceiling — not sold, see §2.2
    memory: 16Gi
    storage: 100Gi
```

The default catalogue ships in the operator chart under `usage.plans.catalogue`;
a cluster selling something else replaces it wholesale in its own values.

### 2.2 What a plan sells: reserved capacity, not burst

A plan is a whole number of **node units** — 2 vCPU / 4 GB / 50 GB — so a plan
is a quantity the platform can go and buy.

That only works because the sold figure is **requests**. Requests are what the
scheduler reserves: two cores of requests is two cores of a node that nothing
else can schedule into, which is exactly what a node purchase provides. Limits
are a burst ceiling — what a container may spike to, with nothing set aside for
it — and they oversubscribe by design.

The gap is not small. Measured on a workspace running Nextcloud with Collabora,
the App Store and Open WebUI:

| | CPU | Memory |
|---|---|---|
| Reserved (`requests`) | 1.00 | 2.5 Gi |
| Burst ceiling (`limits`) | 5.95 | 5.5 Gi |
| Actually consumed | 0.014 | 2.0 Gi |

Sell the middle row and a one-node plan could not hold a single Nextcloud; sell
the top row and a node unit means what it says. So `ResourcePlan` carries both
pairs, and only the reserved pair is priced:

| Quota field | ResourceQuota key | Role |
|---|---|---|
| `requestsCpu`, `requestsMemory` | `requests.cpu`, `requests.memory` | **Sold.** Maps one-to-one onto purchased nodes |
| `cpu`, `memory` | `limits.cpu`, `limits.memory` | Blast radius for a runaway container |
| `storage` | `requests.storage` | Already request-shaped; PVCs reserve what they ask for |

The rule deciding what belongs in a plan is that a plan is a quantity of
capacity, so its fields are the ones that become ResourceQuota keys. `maxApps`
becomes none — the Tenant admission webhook enforces it against `spec.apps` —
so it is a policy limit set per cluster or per tenant, and no plan touches it in
either direction. Writing it would sell a policy as capacity; nulling it would
have a plan change quietly delete a cluster's app cap.

The catalogue's limits multiples — 4× CPU, 2× memory — come from that table
rather than from a round number. A tighter ceiling would refuse pods on a plan
whose *reserved* capacity is barely touched, which is the confusing failure this
whole design exists to avoid.

Imposing a requests quota on a namespace that already has pods is safe: the
tenant `LimitRange` sets `defaultRequest` (100m / 128Mi), so a container that
declares no request still has one and the quota cannot reject it.

### 2.3 The ladder

| Plan | Nodes | Reserved | Burst ceiling | Storage |
|---|---|---|---|---|
| `base` | 1 | 2 / 4Gi | 8 / 8Gi | 50Gi |
| `nodes-2` | 2 | 4 / 8Gi | 16 / 16Gi | 100Gi |
| `nodes-4` | 4 | 8 / 16Gi | 32 / 32Gi | 200Gi |
| `nodes-8` | 8 | 16 / 32Gi | 64 / 64Gi | 400Gi |
| `nodes-16` | 16 | 32 / 64Gi | 128 / 128Gi | 800Gi |

Doubling, so a tenant outgrowing a plan is always offered roughly twice what it
has — rather than steps that are enormous at the bottom and trivial at the top.
The measured workspace above fits `base` on every dimension, which is what a
one-node plan should mean.

At `nodes-16` the burst ceiling stops bounding much on a small cluster; that is
inherent to a multiplicative ceiling and is why the top plan says to arrange
anything larger with the platform operator.

**Tenants already running will read as `custom`** until a plan is applied,
because a cluster's existing `tenant-defaults` predates the catalogue and sets
no reserved capacity at all. That is the honest reading — nothing prices those
tenants — and applying a plan settles it. A cluster that wants new tenants to
start on `base` should set its `tenant-defaults` component to `base`'s
quantities.

### 2.1 Tier, not quantity

Plans are ordered by `spec.tier`, never by comparing their quantities.
Quantities can move in different directions between two plans — more CPU, less
storage — so "bigger" is not derivable from them, and an entitlement ceiling has
to compare something total. Tiers need not be contiguous; gaps let a plan be
inserted later without renumbering ones already sold.

---

## 3. The write path

Selecting a plan is a **commit to `gentian-deployments`**, exactly as installing
an app is, and it is made by the same writer: the **director**. It checks that
the caller holds `can_set_plan` on the tenant, reads the catalogue as it applies
to that tenant from the operator (§7), refuses a plan the tenant may not move
to, and commits the patch below as the person who asked. Argo CD syncs it, the
operator reconciles the ResourceQuota, records the plan change in the tenant's
usage history (§6.3), and
[`app_workload_health.go`](../../internal/controller/app_workload_health.go)'s
fingerprint nudge retries workloads the widened quota now admits.

The operator has no write endpoint for plans. It reads the plan back from git
like every other change to a tenant, so the only way onto a ceiling is a commit
that names who chose it and which decision allowed it.

### 3.1 Why not `tenant.yaml`

Quotas written into `tenant.yaml` **do not survive**. Every tenant kustomization
pulls in the shared `tenant-defaults` component, and a component's patches apply
*after* the resources they accompany:

```
resources:  tenant.yaml            cpu: 48   ← what the API wrote
components: tenant-defaults        cpu: 32   ← what wins
```

The edit would commit cleanly, push cleanly, sync cleanly and change nothing —
the exact failure `app_workload_health.go` was written to explain after fifteen
hours of it.

### 3.2 What is written instead

A per-tenant `resource-plan.yaml`, listed under the tenant kustomization's own
`patches:`, which kustomize applies **after** components:

```
clusters/<cluster>/tenants/<tenant>/
├── tenant.yaml            # apps, isolation, mail — hand-maintained
├── resource-plan.yaml     # the chosen plan, written by the director
└── kustomization.yaml     # resources: - tenant.yaml
                           # patches: - path: resource-plan.yaml
```

```yaml
# resource-plan.yaml — managed by the director
apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: corp
  annotations:
    gentianos.io/resource-plan: nodes-2
    gentianos.io/resource-plan-set-by: "ada@example.com"
spec:
  quotas:
    cpu: "40"
    memory: "48Gi"
    storage: "300Gi"
    maxPods: null          # ← deliberate
```

**Every quota key is emitted, and keys the plan does not set are emitted as
`null`** — which is how a strategic merge *removes* a key. Omitting them instead
would leave the `tenant-defaults` value in place, and the tenant would run on a
ceiling that is neither the default nor the plan but a silent mixture of both:
the plan's CPU with the default's storage, priced as the plan.

The patch file and the kustomization that lists it are committed **together**, in
one commit: a repository synced between the two would either apply a patch
nothing references or reference a patch that is not there, and Argo would fail
the whole tenant on the second. A tenant directory without a kustomization gets
the one every tenant starts with; the director writes it on create as well.

`resource-plan-set-by` names who chose the plan. It is in the patch and not only
in the commit because the operator reads the Tenant, not git, and it is what
lets the plan event (§6.3) carry an actor. Re-choosing the plan git already
records is not a commit, whoever asks: the state asked for already holds, and a
commit that only renamed the chooser would change the record and not the tenant.

---

## 4. The downgrade guard

Kubernetes does not evict pods to fit a shrunken ResourceQuota. It refuses the
**next** create. So a downgrade below current use is not rejected by the cluster
and does not fail loudly: everything keeps running until something restarts, and
then it silently does not come back.

A plan change is therefore refused when the plan is smaller than what the tenant
is using, naming the resource and both numbers:

```
409  limits.cpu: using 34, plan allows 32
```

The check runs against every key the plan sets, reserved and burst alike: a plan
that reserves less than the scheduler has already set aside for running pods is
refused for the same reason as one whose ceiling is too low. It reads
`ResourceQuota.status.used` through the one shared mapping in
[`tenantshell.ResourceListFromQuotas`](../../internal/kernel/tenantshell/resources.go).
Two copies of that mapping would let a plan pass a guard written against
`requests.cpu` and then be enforced against `limits.cpu`.

Only capacity is checked. An app cap is not part of a plan, so no move between
plans can put a tenant over one.

A platform operator may pass `--force` (CLI) or use **Force** (console) to shrink
a tenant anyway. Tenant administrators cannot: the guard protects their own
workspace, and asking for it is asking to break it without being told.

---

## 5. Entitlements

The ceiling on what a tenant may select is the `gentianos.io/max-resource-tier`
annotation on the **Tenant**, resolved by the API on every call.

It is not a request parameter. A ceiling a request supplies is a ceiling a
request can omit, and the console is not the only thing that can reach the
resources API. An unparseable value is treated as absent rather than as zero —
zero is the base tier and would silently pin a tenant to the smallest plan on
the cluster, which is a worse answer to a typo than no ceiling, and the operator
who typed it sees the tenant able to upgrade.

Absent means uncapped, matching the shape the App Store already uses: an
unreachable commerce backend leaves the catalogue usable rather than blocking it.

Set it from wherever a deployment's commerce integration lives — the platform
reads the annotation and does not care which service wrote it.

A plan may also carry `selfServiceDisabled: true`, which withholds it from
tenant administrators while leaving it selectable by a cluster operator and the
CLI. That serves a negotiated ceiling that should not appear in a tenant's own
list of upgrades.

---

## 6. Usage history

### 6.1 What is recorded, and where

A sampler in the operator records, per tenant on a ticker (15 minutes by
default):

| Field | Source | Used for |
|---|---|---|
| `hard` | `ResourceQuota.status.hard` | The ceiling in force |
| `used` | `ResourceQuota.status.used` | **Committed consumption — what is billed** |
| `actual` | `metrics.k8s.io`, when installed | Advisory: is the plan the right size? |
| `plan`, `productSku` | Resolved from the catalogue | The label a stretch is priced under |

Rows land in **that tenant's own `{tenant}_shell` database**, beside the portal's
audit events and notifications. A tenant's consumption is tenant data: it goes
where the rest of that tenant's data goes, it leaves with the tenant, and the
`TenantExport` that already captures the shell database captures it too. The cost
is that a cluster-wide roll-up opens one connection per tenant — the honest price
of the isolation, paid by a screen nobody loads in a loop.

`productSku` is denormalised onto every sample deliberately: a plan's SKU can be
re-pointed, and an invoice for March must not change because of an edit made in
June.

### 6.2 What is billed is the ceiling, not the burn

Invoicing is based on **what the tenant is committed to** — the enforced ceiling
and the requests the cluster has reserved for them — not on live consumption.
Both come from the API server, so the billing series needs no metrics stack at
all. Live consumption answers a different question: whether the plan a tenant
pays for is the plan they need.

That is why metrics-server is optional. See [§8](#8-metrics-server-is-optional-and-not-a-small-prometheus).

### 6.3 Plan events are the billing record

Samples imply a plan change, but only to the resolution of the sampling interval
and only while the samples are retained. A plan change is the billable event, so
it is recorded exactly, once, in `tenant_resource_plan_events` — and **never
pruned**. Samples are pruned at `usage.sampler.retention` (400 days by default);
plan events accrue at the rate a tenant changes plan, which is a handful of rows
a year.

The event is written by the **operator when the change lands**, not by the
director when it is asked for. The tenant reconciler compares the
`resource-plan` annotation with `status.resourcePlan`, the plan whose event was
last written, and when they differ records the move — from the recorded plan
(or, for a history that predates the status field, the store's last event) to
the annotated one, with the SKU from the catalogue and the actor from
`resource-plan-set-by` — then notes it on the status. So the record describes
the ceiling the cluster went on to enforce, which is the one a bill can be
defended with, and it still names who chose it, because the commit carried
that into the Tenant. A usage store that is not reachable yet leaves the status
alone and the event is written on a later reconcile; a plan the catalogue does
not know is recorded without a SKU rather than not at all.

### 6.4 The report

`GET /v1/tenants/{t}/resources/report?from=&to=` resolves a window into
`PlanInterval`s — the unit a bill is made of:

```json
{ "tenant": "corp", "intervals": [
  { "plan": "base",     "productSku": "sku-1node",  "from": "2026-01-01T00:00:00Z",
    "to": "2026-01-18T09:12:00Z", "seconds": 1473120, "partial": true },
  { "plan": "nodes-2",  "productSku": "sku-2node", "from": "2026-01-18T09:12:00Z",
    "to": "2026-02-01T00:00:00Z", "seconds": 1176480, "partial": true }
]}
```

`partial` marks an interval clipped by the window rather than ended by a plan
change, so a biller pro-rating a month knows which ends are real.

The plan in force when the window opened comes from the last event before it;
failing that, from the `fromPlan` of the first event inside it; failing both, the
report is marked `incomplete` rather than opening with a guess. A recovered
opening interval carries **no SKU** — the event names the SKU of the plan it
moved *to* — because a gap is honest and a wrong price is not.

---

## 7. Surfaces

One set of rules, one writer. The plan catalogue, the downgrade guard and the
entitlement ceiling live in the operator, which answers them for a tenant; the
director relays those answers to whoever may view the tenant, validates a choice
against them, and makes the one write. Nothing reimplements either half.

| Surface | How it reaches them |
|---|---|
| **Admin Console → Resources** | console API → director, as the caller |
| **`kubectl gentian resources`** | port-forward → the operator's reads; no write |
| **Anything else** | the director's endpoints, with a person's token |

The director, guarded by the tenant relation named:

```
GET  /v1/tenants/{t}/resources          can_view      plan, ceiling, committed, live
GET  /v1/tenants/{t}/resources/plans    can_view      catalogue, with blocked reasons
PUT  /v1/tenants/{t}/resources          can_set_plan  {"plan": "nodes-2", "force": false}
GET  /v1/tenants/{t}/resources/usage    can_view      thinned sample series
GET  /v1/tenants/{t}/resources/report   can_view      billable plan intervals
GET  /v1/clusters/{c}/resources         can_audit     every tenant's state, for the cluster's view
```

The operator's app-lifecycle API keeps the reads (`GET .../resources`,
`/plans`, `/usage`, `/report`), which is what the director relays. It has no
`PUT`. Each plan it lists that the tenant may not move to carries `blocked`, the
reason in words, and `blockedBy` — `fit`, `entitlement` or `self-service` — which
is what the director turns into a status:

| Code | Means |
|---|---|
| `202` | Committed; the answer names the commit. `200` means the tenant is on that plan already |
| `409` | The plan does not fit **today** (`fit`) — retry after freeing something, or `force` it |
| `402` | Above the tenant's entitlement, or arranged by hand (`entitlement`, `self-service`) — an entitlement problem, not a permission one |
| `404` | No such plan on this cluster |
| `403` | The caller does not hold `can_set_plan`, or asked to `force` without `can_configure` on the cluster |

Whether the caller chooses for themselves — a tenant administrator, held to
the self-service catalogue — or for the cluster is decided by the director from
`can_configure` on the cluster, and asserted by no screen. The console adds
nothing: it forwards the caller's own token and hands back what the director
answered, including a refusal.

### 7.1 Console

Tenant administrators see their plan, the ceiling paired with what is committed
under it, live consumption where available, the plan catalogue with a reason
beside anything they cannot pick, the usage history, and the billed intervals.

Platform administrators additionally get an **All tenants** table — plan,
headroom per resource, app count — and can manage any tenant from the same tab.

Charts are one panel per resource, never one panel with two y-axes: CPU cores and
gibibytes share no scale. The ceiling is a dashed reference line, not a third
series — it is the frame the other two are read against.

### 7.2 Audit

A plan change is a commit authored by the person, with the director's
`Gentian-Authz` trailer naming the request, the subject and the decision that
allowed it, and a plan event in the tenant's usage history naming the same
person (§6.3). A refusal is the director's decision-log entry: the attempt is
the interesting half when the tenant later asks why nothing changed.

---

## 8. metrics-server is optional, and not a small Prometheus

`metrics.k8s.io` serves the **latest value only** — in memory, no persistence, no
history, no query language. It exists for `kubectl top` and HPA. There is nothing
in it to upgrade *into* history.

Prometheus, conversely, does not serve `metrics.k8s.io`; HPA against it needs
prometheus-adapter. The two coexist rather than replace each other.

So the history is **Gentian's own**, sampled into each tenant's shell database,
and the *reading* is behind an interface:

```
usage.ActualSource
 ├── MetricsAPISource   (metrics.k8s.io — today, optional)
 └── PromQL             (later, if a cluster grows one)
```

Adopting Prometheus later is a change of source, not a restart of the record —
the series already collected stays, and neither the API nor the UI changes.

Install with `scripts/steps/A-11-metrics-server.sh` and set
`usage.metricsServer.enabled=true`. Without it the Resources tab shows ceilings
and committed usage, and says the live comparison is unavailable rather than
drawing an empty series that reads as "this tenant used nothing".

---

## 9. Configuration

Operator chart (`charts/gentian-os/values.yaml`):

| Value | Default | Effect |
|---|---|---|
| `usage.sampler.enabled` | `true` | Record ceilings and consumption |
| `usage.sampler.interval` | `15m` | How often every tenant is sampled |
| `usage.sampler.retention` | `9600h` (400d) | Sample retention; plan events are never pruned |
| `usage.metricsServer.enabled` | `false` | Add the live-consumption series |
| `usage.plans.enabled` | `true` | Ship the plan catalogue |
| `usage.plans.catalogue` | five node tiers | The priced plans themselves |

Director (the same chart):

| Value | Effect |
|---|---|
| `director.appLifecycleURL` | The operator's app-lifecycle API, which the director reads a tenant's resources from and validates a plan choice against. Empty derives it from the release's own lifecycle Service. Without it the director registers no resources routes, and the console's Resources screen says so rather than showing a cluster with no plans. |

---

## 10. Operations

```bash
# Catalogue, and what one tenant may pick
kubectl gentian resources plans
kubectl gentian resources plans --tenant corp

# A tenant's ceiling and what is under it
kubectl gentian resources show corp

# Move a tenant: through the director, as yourself. Refused if it does not fit.
curl -X PUT -H "Authorization: Bearer $TOKEN" -d '{"plan":"nodes-2"}' \
  https://<director>/v1/tenants/corp/resources
# (or the administration console's Resources screen, which does exactly this)

# What a month resolves to for invoicing
kubectl gentian resources report corp --from 2026-01-01T00:00:00Z --to 2026-02-01T00:00:00Z

# The ceiling on what a tenant may choose for itself
kubectl annotate tenant corp gentianos.io/max-resource-tier=20 --overwrite
```

**Sampler not recording:** it writes to each tenant's `{tenant}_shell` database
through the `portal-shell-<tenant>` Secret in `platform-kernel`. A tenant without
that Secret is skipped and logged; the others are unaffected.

**Plan shows as `custom`:** the tenant's quotas match no plan. Either the tenant
predates the catalogue and reserves nothing (see [§2.3](#23-the-ladder)), the
catalogue was re-priced under a tenant already on it, or `tenant.yaml` was
edited by hand. Applying a plan settles it.

**Plan shows as `drifted`:** the recorded plan and the enforced ceiling
disagree. The cluster enforces what is there; what is billed is what is
recorded. Re-applying a plan settles both.

---

## 11. Not in v1

| Deferred | Note |
|---|---|
| Per-app quotas inside a tenant | The ceiling is per namespace, which is where Kubernetes enforces it |
| Automatic right-sizing | The data to justify it is what this collects; acting on it is a later decision |
| Cost in currency | The platform reports plan intervals and SKUs; pricing is the commerce backend's |
| Scheduled or time-boxed plans | A plan is in force until changed |
| Cluster capacity planning | The overview shows per-tenant headroom, not node headroom |
