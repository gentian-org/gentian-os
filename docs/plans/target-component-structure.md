# Target component structure

What a component declares, what the platform derives from it, and where each
rule is enforced.

This replaces `component-profile.md`, the schema design `ComponentProfile` was
built from, and `unified-app-crd-sketch.md`, a working note outside this
repository. Its reasoning is folded in here; four of its conclusions are not,
and are marked where they changed.

Companions: [namespace-cleanup.md](namespace-cleanup.md) for which namespace
each tier lands in, [operator-split-plan.md](operator-split-plan.md) for who
writes the CR, [architectural-decisions.md](architectural-decisions.md) for the
decisions behind all three.

**Out of scope: the kernel tier.** Argo CD, Keycloak, OpenBao, External
Secrets, Crossplane and Headlamp are installed by `install.sh` and reconciled
by Argo CD. They are never profiles and never the operator's, because the
operator depends on them existing first. `kernel/namespaces.yaml` names the
tiers: `kernel | system | system-dmz | shared | tenant | tenant-dmz`. This
document covers the last five.

A consequence for S7A.15: of the five hosts in the tenant Composition's
`$zoneHosts` list, `console` and `admin` are components and `argocd`,
`headlamp` and `id` are kernel tier. Deriving the list from components covers
two of the five. The other three need a separate answer.

**Status.** Nothing in §2, §3 or §4 is built. Two renames break every profile
in the catalogue, so they are a change to `convert-appprofile.py` rather than
an edit.

---

## 1. One kind, one instance, two levels

`ComponentProfile` is the cluster-scoped catalogue entry. `Component` is the
namespaced instance. Both exist and work.

Whether a component *can* serve several tenants safely is a claim the
catalogue certifies. *Running* it shared is a decision a named person makes.
Collapsing the two would leave a profile that could go either way unable to say
so, and the decision unrecorded apart from the claim. So the same word at two
levels:

- `ComponentProfile.spec.classes` is a **list** of the modes this component may
  be deployed under. A certification claim, reviewed with the entry.
- `Component.spec.class` is a **single value**, and must be a member of that
  list. The deployment decision.

`trustTier` stays in the spec rather than becoming a label. CRD validation
rules cannot read labels or annotations; only `name` and `generateName` are
exposed on metadata. In the spec, "shared requires platform tier" is a schema
invariant that fails at write time whoever is writing.

**No presentation fields beyond the tile.** A catalogue listing is reference
data outside the cluster (AD-3). The exception is the tile on an exposure,
because the portal must show what the cluster routes without asking a service
outside it (§8.1).

---

## 2. Three axes

### Axis 1 — class: who it serves

```yaml
spec:
  classes: [app]          # capability claim, on the profile
```

```yaml
spec:
  class: app              # deployment decision, on the Component
```

| value | serves | responsible | namespace | instances |
|---|---|---|---|---|
| `service` | other components over contracts, and operators at its own console | platform admin, via the Cluster claim | `system-<function>` | one |
| `app` | the people of one tenant | tenant admin, via the director | `tenant-<t>` (+ `-dmz`) | one per tenant |
| `shared-app` | the people of several tenants, from one backend | platform admin, via the director | `shared-<app>` | one |

`service` is exclusive: a component may not be both a service and an app.

**Changed from `tenancy: system | shared | tenant`.** Those values mix a
placement word, a bare adjective and a scope word for one question. `system`
names the namespace, not the role: its own doc comment defines it as *"serves
contracts to other components"*, which is a service. `shared` is an adjective
with no noun. Once `service` is a value, `tenancy` is no longer the question
being asked, so the field name moves with the values.

Precedent: `StorageClass`, `IngressClass` and `PriorityClass` use *class* for a
named variety that behaves differently. All three are objects rather than
enums, so a reader may look for a `ComponentClass` CRD that does not exist.

### Axis 2 — delivery: how the platform realises it

```yaml
spec:
  package:
    chart: {...}          # exactly one of chart | composition | api | addon
```

| delivery | package holds | meaning |
|---|---|---|
| `workload` | `chart` or `composition` | the platform runs it |
| `api` | `api` | the platform routes to something already running |
| `addon` | `addon` | the platform flips a switch inside another component |

**Derived from the package, not a field.** Two places already answer this and
can disagree:

- `package.deploymentMethod: api` alongside a `chart` is admissible on
  `ComponentProfile`. `AppProfile` has a CEL rule forbidding it; the new kind
  does not.
- `package.chart` and `package.apiIntegration` together are admissible. The
  one-of rule is an OR, not an exactly-one. The same file writes exactly-one
  correctly for `source.cidrs` against `source.component`. The app template
  already tells authors *"Exactly one of chart, compositionRef or
  apiIntegration must be present"*, so the documentation states the rule the
  schema fails to enforce.

So `deploymentMethod` is deleted and the package becomes an exactly-one union.
Delivery is then read from the package and cannot contradict it.

Visibility, which is what a field would have been for: a printcolumn, a label
the operator sets so `-l gentianos.io/delivery=api` selects, and a console view
beside Integrations listing every ingested API with its base URL, runtime and
tenant binding.

The word is `workload`, not `executable`: the codebase already uses it as the
antonym, in `ProfileDeploysWorkload` and in *"a component that is an API client
rather than a workload"*.

Delivery `api` does not mean external. `litellm-me` is delivery `api` and
points at `litellm-proxy.platform-kernel.svc.cluster.local`, inside the
cluster. The axis is whether the platform runs it. (That URL names a v4
namespace, a separate bug.)

### Axis 3 — trustTier: how far it was reviewed

Required, no default, load-bearing: `shared-app` requires `platform`, and so
does `forwardToken`. Unchanged.

---

## 3. `addon` is a package type

An addon is a catalogue entry that is not deployed. It flips a switch inside
another component's addon system: an Odoo module name, a Nextcloud app id, an
Activepieces piece name. That is the statement `api` makes — this entry is not
run here — so it belongs in the same field.

```yaml
spec:
  classes: [app]
  package:
    addon:
      id: mrp
      of: odoo-base-ce
```

**Changed from `spec.customization.addon`.** Five consequences of the current
placement:

1. The one-of rule reaches out of `package` to finish itself:
   `... || (has(self.customization) && has(self.customization.addon))`.
2. The kind is decided by an annotation. `EffectiveDeploymentRole` reads
   `gentianos.io/deployment-role`, and the addon resolver checks that. So *is
   this an addon* is answered by an annotation, *which addon and of what* by
   `customization.addon`, and *how is it delivered* by `package`.
3. The profiles contradict themselves. Of 20 addon profiles, 11 declare a
   `chart`. `odoo-mrp-ce` declares `deploymentMethod: crossplane`, a
   `compositionRef`, a chart version and its own CPU and memory requests
   alongside `customization.addon`. The `of` field's comment explains why: it
   replaced an annotation that auto-installed a base when an addon was
   installed standalone, *"the relationship the L3 cleanup inverts"*. Those
   fields are from the standalone era.
4. `ProfileDeploysWorkload` is `!ProfileIsAPI(p)`, so an addon reports that it
   deploys a workload.
5. Delivery derived from the package has no value for an addon: nothing is run
   and nothing is routed.

**What `customization` keeps.** Grade, rubric score, supported rungs,
drop-ins, `extension`, `publishes`, `repackage` — an assessment of how
changeable a deployable app is. `addon` is not an assessment. The block also
mixes sides today: `addon` is set on the addon, while `addonActivation` and
`addonValues` are *"set on the base profile, not on the addons"*.

Against the move: an addon exercises rung L3, the app's own addon system, so
grouping it with the ladder has a logic. But a rung is a property of the
relationship, not of the entry, and the one-of rule already treats `addon` as a
kind. The base's side of L3, `extension`, is a ladder statement and stays.

Undecided: whether `addonActivation` and `addonValues` follow into `package`.
They change the base's chart values, which is packaging, but they are tied to
the ladder.

---

## 4. Requires, integrations and provides

### 4.1 `requires` and `integrations` differ in six ways

| | `requires` | `integrations` |
|---|---|---|
| counterparty | the platform | another component |
| if unmet | does not start | runs normally |
| whose fault | the platform's | nobody's |
| when resolved | once, before install | continuously |
| consent | implicit in using the platform | explicit tenant grant |
| signal | alert the platform admin | a status note, never an alert |

A boolean on a shared list hides all six. They are separate fields because they
are separate controller paths: requirements gate admission, integrations
reconcile forever after. An unmet requirement surfaces as a condition naming
the platform as responsible, not as a component error a tenant administrator
cannot act on.

Keycloak is the worked example already in the tree. Its database is a
requirement. Its SMTP relay is an integration: supplied after install, and a
realm without it simply cannot send invitations.

### 4.2 `requires.services`

| | what the platform fulfils | what components offer each other |
|---|---|---|
| where | `spec.requires.services` | `spec.provides`, `spec.integrations` |
| shape | closed set, one typed struct per member | open set, a name and a protocol |
| members | identity, database, storage, cache, mail, MCP | `wiki`, `project-management`, `erp-core`, `central-navigation` |
| fulfilled by | a component of class `service`, or something outside the cluster | another component |

```yaml
spec:
  requires:
    services:                 # was kernelRequirements, then requires.contracts
      database: {...}
      identity: {...}
    privileges: {...}         # §4.3
  provides:
    - name: project-management
      protocol: http-json
  integrations:
    - contract: central-navigation
      provider: desktop
```

**Changed from `kernelRequirements`**, which names a fulfiller that is not the
fulfiller. A database comes from a component of class `service`. Mail may be a
relay outside the cluster. Object storage may be a bucket at a cloud provider.

**And changed from `requires.contracts`**, where it landed on
`ComponentProfile`: *contract* is already the word for the open named set in
the second column. That rename also only half happened — the Go type behind
`requires.contracts` is still `KernelRequirements`.

`services` pairs with class `service`, so the model reads consistently: a
component of class `service` fulfils `requires.services`. Go type
`ServiceRequirements`.

`resources` was the alternative, and is what Backstage, Radius and Score use.
Rejected because *resources* already means quota here: `ResourcePlan`,
`Tenant.spec.quotas`, the console's Resources screen.

The doc comment on the current field lists *"identity, database, object
storage, cache, mail, LLM, MCP"*. There is no LLM member in the struct. Add it
or stop promising it.

The scope of `provides` follows the class: cluster-wide for a `service` and a
`shared-app`, within the tenant for an `app`.

### 4.3 Privileges: asking, and being granted

A cluster's `PlatformSecurityPolicy` is a standing list of what may be **asked
for** at all. Granting is the separate act of saying this component, in this
tenant, may have this privilege, with a person, a time and a reason.

Only the first exists today. The operator intersects a profile's waivers with
the allowlist and applies whatever survives; egress is not filtered at all. An
intersection is a filter, so there is no approver, no timestamp and nothing in
a decision log.

Every request carries a `name`, so a grant can refer to it as `<kind>/<name>`,
and a `reason` the approver reads.

```yaml
spec:
  requires:
    privileges:
      podSecurity:
        - name: needs-write-root
          policy: require-read-only-root
          scope: odoo
          reason: "Odoo writes its filestore under /var/lib/odoo."
      egress:
        - name: smtp-relay
          rule: {...}
          reason: "Outbound mail to the tenant's relay."
      clusterRoles:
        - name: read-nodes
          rules: [...]
          reason: "The dashboard lists node capacity."
```

**Who approves follows from the kind, never from a field**, or a component
could ask for the cheaper approver.

| kind | scope | approver | relation |
|---|---|---|---|
| pod-security waiver | cluster: weakens an admission rule protecting the node | security officer | `cluster#can_approve` |
| cluster role | cluster: reaches the Kubernetes API | security officer | `cluster#can_approve` |
| egress beyond baseline | tenant: leaves the tenant's own namespace | tenant administrator | `tenant#can_approve_privilege` |

The grant sits on the Component, written only by the director from the caller's
token. `approver` and `approvedAt` are immutable: a new approval is a new
grant.

```yaml
spec:
  privileges:
    - privilege: egress/smtp-relay
      approver: <keycloak subject>
      approvedAt: 2026-09-25T09:00:00Z
      reason: "In the approver's words, not the profile's."
      expiresAt: 2027-09-25T09:00:00Z
```

An install with an ungranted privilege **waits**. It is neither rejected nor
run without it, and it is visible in the console as a pending request, which is
a state the tenant administrator can act on or escalate. That is why the
request has to be an object rather than a field the operator silently drops.

This closes [security-gap-closing.md](security-gap-closing.md) G27: MAC waivers
are intersected against the cluster allowlist, so an administrator approves
them, while `security.egress` is copied into a NetworkPolicy with no equivalent
check. One requirement block with one approval path removes the asymmetry by
construction.

---

## 5. Secrets: three kinds, two declared

| kind | origin | declared |
|---|---|---|
| granted credential | arrives with a fulfilled requirement | **no** — restating it creates drift |
| generated | random, created once, held in the vault | yes, under `secrets.generated` |
| derived | deterministic from tenant and component name | yes, but prefer generated |

Generated secrets have no counterparty: an admin bootstrap password, a session
signing key, a data-at-rest key.

`secrets` is `{generated, derived}` rather than one list, because the two have
different failure modes and a reviewer should see which is which.

Derivation's stability comes from recomputation rather than storage, so the
formula and its inputs are load-bearing forever and a change to either silently
rotates the value for every existing tenant. That has already happened here, on
the derivation salt. Generated and stored is the more robust default.

---

## 6. What the class derives

| | `service` | `shared-app` | `app` |
|---|---|---|---|
| namespace | `system-<function>` | `shared-<app>` | `tenant-<t>` (+ `-dmz`) |
| north-south route | its console, in the kernel zone, gateway only | per granted tenant | the tenant's gateway |
| OIDC client | one | per granted tenant realm | the tenant realm |
| scope of `provides` | cluster-wide | cluster-wide | within the tenant |
| tenant binding | none | a grant per tenant | implicit |
| `defaultForTenants` | meaningless | meaningless | allowed |
| delete blast radius | every consuming component | every granted tenant | one tenant |

Nothing here is a new field. The class reinterprets the scope of declarations
the profile already makes.

---

## 7. Shared instances: offered, then installed

A shared instance existing is not the same as a tenant having the app. Two
steps, two people:

1. The platform administrator installs the component with `class: shared-app`
   into `shared-<app>` and **offers** it, one
   `shared_instance:<p>#offered_to@tenant:<t>` tuple per tenant, written under
   `can_grant_shared`. Nothing is visible to anyone yet.
2. The tenant administrator installs the app. The tenant gets its own `app`
   object, per-app group, OIDC client in its own realm, route and tile. Only
   the backend is shared.

When a tenant names profile P:

```
is there a shared instance of P offered to this tenant?
  no  → install a dedicated release in tenant-<t>        (today's behaviour)
  yes → bind: create the tenant-side objects only, with the route's backend
        in shared-<app> and a NetworkPolicy allowing that one hop
```

Binding creates no Release. Everything a person meets is still per tenant, so
from inside the tenant a bound app and a dedicated one are indistinguishable.
That is what makes the choice operational rather than a product difference.

Availability decides by default, but never silently. Where the data lives is
something a tenant may know before installing and may refuse. The director's
read of an installable app says which fulfilment an install would get, and the
tenant may pin it:

```yaml
spec:
  fulfilment: auto        # auto (default) | dedicated
```

There is no `shared` value: a tenant cannot demand a backend the platform has
not offered. The instance records what it got, so a later offer does not move a
running app.

Withdrawing an offer uninstalls nothing; it stops new tenants binding. Removing
a bound tenant is an uninstall in that tenant, never a side effect of a tuple
delete.

---

## 8. Exposure

**`authMode` is required on every entry, with no default.** `none` has to be a
word somebody wrote and a reviewer can find. The old
`BrowserProxyRoute.authMode` defaulted to `forward-bearer`, which defeated
exactly that.

**`surface` is `gateway` or `perimeter`.** Publishing proxies live in
`tenant-<t>-dmz` (AD-6) with one least-privilege credential per surface,
separate from routes on the authenticated gateway. They differ in namespace,
credential, policy and blast radius. This replaces a separate `publicSurfaces`
list.

An entry also carries `paths`, `denyPaths`, `stripPrefix`, an optional `source`
restriction, `forwardToken`, a `backend` and an optional `tile`.

- **`denyPaths`** exists for a component whose public surface is "the site
  except its admin". Deny wins over allow regardless of specificity. It is in
  the schema and no controller reads it — S7A.13, build it or remove it.
- **`source`** pins the caller before `authMode` is considered. Collabora's
  WOPI callbacks are `authMode: none` and safe only because the caller is
  pinned. Exactly one of `cidrs` or `component`; `component` is preferred
  because CIDRs age badly.
- **`forwardToken`** passes the edge access token to the backend. Default
  false: a backend gets identity headers, not a bearer also valid at the
  director and at every sibling. CEL ties it to `trustTier: platform`. It is
  meaningless on a perimeter entry, which has no session.

### 8.1 A service may expose, and a backend may be another component's

**Changed from "a system component has no exposure".** That rule conflates a
service's contract surface, which is in-cluster and reached over a plain
Service, with having no north-south surface at all. A shared database with a
query console, a model gateway with an operator console and an object store
with a browser are each one component.

What replaces it:

1. **One instance, never one per tenant.** `defaultForTenants` is meaningless
   and the exposure is not multiplied.
2. **Gateway only, never perimeter.** A service console sits behind the kernel
   session; the perimeter has none. The instance-level rule already says this
   for perimeter enablements, and this lifts it to the profile.
3. **Its tile asks `object: cluster`.** The relations exist:
   `can_operate_system` for `service_admin`, plus `can_configure` and
   `can_audit`. `TileObject` offers `app` and `tenant` and needs `cluster`.

**An exposure's backend may name another component's Service.** Today
`BackendRef` is a Service in the component's own namespace, which cannot
express three cases the model already has:

| case | backend |
|---|---|
| an addon's tile | the base's Service, named by `package.addon.of` |
| a bound `shared-app` | the shared instance's Service in `shared-<app>` |
| an `app` or `service` | its own Service |

The second is already required and is currently implied by `fulfilment` rather
than declared. Naming the component makes all three the same mechanism:

```yaml
  backend:
    component: odoo-base-ce      # optional; default is this component
    service: odoo
    port: 80
```

The named component must be one this component is already bound to: its addon
base, or the shared instance it is bound to. Publishing is never a way to reach
a component you have no relationship with. Cross-namespace routing uses the
ReferenceGrant mechanism the reconciler already writes for the edge namespace.

**A tile lives on the exposure**, because a tile is a link to one host and one
path and the exposure decides those. The operator projects the catalogue from
the routes it composes, so a tile and what it points at cannot disagree. Each
tile carries a `relation` and an `object`, so nobody is shown a tile they may
not open.

The field is `object` and not `on`: YAML 1.1 reads a bare `on` as the boolean
`true`, so a hand-written profile carries a key named `true` and is refused by
the schema. The same goes for `off`, `yes` and `no`.

### 8.2 `launch`

An `app` is not required to have a tile. Two of the platform's own components:

| component | exposures | tiles |
|---|---|---|
| desktop | `api`, `web` | none |
| admin console | `api`, `web` | on `web` only |

The desktop is the surface tiles appear on. The admin console's API entry is
reachable and unadvertised, which is what an API entry should be. A blanket
"every app has a tile" rejects the first.

The schema cannot currently tell *deliberately unadvertised* from *omitted*, so
an app can be installed, run, and be unreachable. A person reaches a component
in exactly three ways, and `launch` names which:

```yaml
spec:
  launch: tile                  # at least one expose entry carries a tile
  # launch: {from: file-store}  # opened by whatever provides this contract
  # launch: none                # the launcher itself, or no human surface
```

`from` also records something the model cannot say today: which component opens
this one. Collabora is opened from Nextcloud and never from a launcher.

A `service` with a console sets `tile`; one without sets `none` and has no
`expose`.

### 8.3 Enablement: the tenant's half

The profile declares what **may** be published. What **is** published is
recorded on the instance ([networking.md](networking.md) §8.1). Gateway entries
need no enablement: they carry the session and are always on. Perimeter entries
are off until a perimeter approver enables them.

```yaml
spec:
  exposures:
    - exposureName: public-share     # must name a perimeter entry of the profile
      host: share.acme.example       # empty means the entry's default host
      owner: <keycloak subject>      # immutable
      expiresAt: 2026-12-24T00:00:00Z
      reviewAt: 2026-11-24T00:00:00Z
```

`authMode` is not repeated: the profile's entry is the one source and an
enablement cannot weaken it. The field is `exposureName` and not `surface`,
because `surface` is the enum on the profile's entry and one field name meaning
two things in adjacent structs is how a schema drifts.

`expiresAt` is always set. At expiry the operator treats the enablement as
absent and removes the proxy, route and listener, while the entry stays in git
as history, which keeps the next Argo CD sync idempotent.

A host in the tenant's own zone needs no certificate work: the zone's DNS-01
wildcard covers it. A vanity host is admitted only if the tenant's approved
domains include it, and its certificate comes by HTTP-01, because the platform
holds no credential to a customer's DNS zone.

Who may write one is `can_expose` on the tenant, checked by the director.

The cluster half is a **ceiling, not a permission list**. No component is
published by default at any trust tier, so it has nothing to grant.

```yaml
exposure:
  denyAuthModes: []              # modes this cluster refuses outright
  requireExposurePolicyContract: true
  defaultLifetime: 2160h         # 90 days
  maxLifetime: 8760h
  reviewInterval: 720h
```

The lifetimes are required with no absent case: two optional fields whose joint
default is a permanent public surface is not a safe default.

An earlier draft gated `authMode: none` on `trustTier: platform`. That
conflates two risks — platform tier certifies that one instance can serve
several tenants safely, which says nothing about anonymous access — and would
refuse the first Nextcloud share link.

### 8.4 The `exposure-policy` contract

A profile with any `authMode: none` entry should declare
`provides: [{name: exposure-policy}]`, and must wherever the cluster sets
`requireExposurePolicyContract`, the default.

That is an admission check, not a CRD rule: the requirement lives on the
Cluster claim and CRD rules cannot read another object. It is refused when the
enablement is written, the only moment it matters.

The contract gives the platform, with the tenant's credential from the binding,
`policy.read` and `policy.write` for the app's public-sharing policy — default
and maximum object expiry, password required, which groups may share publicly,
anonymous upload — and `objects.list` and `objects.revoke` for its public
objects with owner, created, expiry and a hashed token.

A `none` entry without the contract is admitted, but its surface is **opaque**
in the inventory ([networking.md](networking.md) §8.3), bounded only by the
enablement's expiry.

---

## 9. The entry, end to end

```yaml
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: openproject
spec:
  classes: [app]
  launch: tile
  trustTier: platform
  version: "1.4.2"

  package:                       # exactly one of chart | composition | api | addon
    chart:
      repository: oci://ghcr.io/gentian-org/charts
      name: openproject
      version: 16.1.0
    valueMapping: {...}
    extraValues: {...}

  requires:
    services:
      identity:
        oidc: {...}
      database:
        engine: postgres
      storage:
        s3: {...}
    privileges:
      egress:
        - name: smtp-relay
          rule: {...}
          reason: "Outbound mail to the tenant's relay."

  provides:
    - name: project-management
      protocol: http-json
  integrations:
    - contract: central-navigation
      provider: desktop

  secrets:
    generated: [...]
    derived: [...]

  expose:
    - name: web
      surface: gateway
      authMode: oidc
      subDomain: projects
      backend: {service: openproject, port: 80}
      tile:
        displayName: Projects
        description: Plans, tasks and timelines
        icon: project
        relation: can_use
        object: app

  sessionMaxAge: 8h
  defaultForTenants: false
```

`sessionMaxAge` caps the component's **own** session, for a component that
establishes one through its own client rather than relying on the gateway's
headers. The edge bounds reachability but does not refresh the group model a
component captured at its own login, so this value, not the access-token
lifetime, bounds what a person may still do inside it after their rights change
(AD-13). Meaningless for a component with no login of its own.

An API-delivered entry differs in one block:

```yaml
  package:
    api:                         # was apiIntegration
      runtime: portal-proxy
      baseUrl: https://corp.desk.gentian.org
      tenantBinding: tenant-domain
```

An addon, whose exposure names its base's Service:

```yaml
  package:
    addon: {id: mrp, of: odoo-base-ce}
  expose:
    - name: web
      surface: gateway
      authMode: oidc
      backend: {component: odoo-base-ce, service: odoo, port: 80}
      tile:
        displayName: Manufacturing
        icon: factory
        path: /odoo/action-mrp.mrp_production_action
        relation: can_use
        object: app
```

A service with a console:

```yaml
spec:
  classes: [service]
  launch: tile
  expose:
    - name: console
      surface: gateway           # a service is never perimeter
      authMode: oidc
      subDomain: models
      backend: {service: gateway-ui, port: 8080}
      tile:
        displayName: Models
        icon: model
        relation: can_operate_system
        object: cluster
```

The instance records answers, never requests:

```yaml
apiVersion: gentianos.io/v1alpha1
kind: Component
metadata: {name: openproject, namespace: tenant-acme}
spec:
  profileRef: {name: openproject, digest: "sha256:..."}
  class: app
  fulfilment: auto
  addons: []
  exposures: []                  # perimeter entries switched on
  privileges:
    - privilege: egress/smtp-relay
      approver: ...
      approvedAt: ...
```

---

## 10. Where enforcement goes

CEL for what the object may say; admission policy for who may say it. CEL
cannot see the writer or the namespace, and an admission policy cannot be
relied on at every write path.

### CEL, existing

- `service` is exclusive within `classes`.
- `shared-app` requires `trustTier: platform`.
- `forwardToken` requires `trustTier: platform`.
- `class` and `profileRef.name` are immutable on the instance.
- `fulfilment` applies to class `app` only.
- a perimeter entry cannot use `authMode: oidc`, and `forwardToken` is
  meaningless on one.
- exactly one of `source.cidrs` or `source.component`.

### CEL, new

- the package is **exactly one** of `chart`, `composition`, `api` or `addon`;
- `deploymentMethod` does not exist;
- a `service`'s exposures are all `surface: gateway`, and it switches on no
  perimeter enablement;
- a `service`'s tile asks `object: cluster`; an `app` or `shared-app` tile does
  not;
- `defaultForTenants` is false for a `service`;
- `launch: tile` requires at least one `expose[].tile`; `from` and `none`
  require none.

### Admission policy

- a Component in a tenant namespace must have `class: app`;
- `class` must be a member of the referenced profile's list;
- only the platform administrator may create Components in `system-*` or
  `shared-*`;
- `backend.component` must name the addon's base or the bound shared instance;
- a `none` surface must provide the `exposure-policy` contract wherever the
  cluster requires it.

### Promises the CRD makes and does not keep

- `expose[].denyPaths` — no controller reads it (S7A.13).
- `package.api.runtime: proxy` — no case in the route builder, so it falls
  through to the default service-backed rule and routes to a Service an
  API-delivered entry never creates. A silent 503 rather than a refusal.

---

## 11. Why the model closes

A tenant administrator cannot create a service, because the only class
creatable in a tenant namespace is `app`. They never need to: what a component
requires arrives through `requires.services`, fulfilled by somebody else.

When something needs what no contract covers, it ships as an extension inside
the component's own pod — in the tenant namespace, in tenant ownership, in the
tenant's blast radius. The escape hatch never creates a cluster-scoped object.

---

## 12. What changes

| today | target | breaks |
|---|---|---|
| `spec.tenancy: [system\|shared\|tenant]` | `spec.classes: [service\|app\|shared-app]` | every profile |
| `Component.spec.tenancy` | `Component.spec.class` | every install |
| Go `ComponentTenancy` | Go `ComponentClass` | nothing on the wire |
| `spec.kernelRequirements` | `spec.requires.services` | every profile |
| `spec.requires.contracts` | `spec.requires.services` | 2 profiles |
| Go `KernelRequirements` | Go `ServiceRequirements` | nothing on the wire |
| `package.apiIntegration` | `package.api` | 2 profiles |
| `package.deploymentMethod` | deleted | 2 profiles |
| `package.compositionRef` | `package.composition` | nothing, unused |
| `customization.addon` | `package.addon` | 20 profiles |
| annotation `deployment-role` | deleted, read from the package | 20 profiles |
| nothing | `spec.launch` | new field, default `tile` |
| `TileObject: app\|tenant` | `app\|tenant\|cluster` | nothing, additive |
| `BackendRef: {service, port}` | `{component?, service, port}` | nothing, additive |
| CEL "system has no expose" | "service is gateway-only" | nothing, no service profiles exist |

---

## 13. What has to happen first

1. `Tenant.spec.apps` resolves `AppProfile` only, at every site that reads it.
   Until a `ComponentProfile` can be installed into a tenant by naming it
   there, the catalogue cannot move.
2. The component reconciler refuses any package that is not a chart
   (*"only package.chart is reconciled yet"*). It has no addon path and no API
   path.
3. `BackendRef` gains `component`, with the ReferenceGrant and NetworkPolicy
   the cross-namespace hop needs. All 20 addon profiles carry a tile pointing
   into their base and cannot convert without it.
4. Run the conversion in `gentian-apps` and settle its review items. The
   converter exists and all profiles convert; nobody has run it for real.
5. Apply the renames in `convert-appprofile.py`, so the conversion and the
   rename are one migration.
6. Retire `AppProfile`, `AppCatalogue` and the `App` claim.

---

## 14. Open decisions

1. **Fulfiller selection.** A `database` requirement must resolve to a specific
   service. The class says a Postgres exists, not which. Needs a default per
   contract on the Cluster claim, or an explicit selector.
2. **The authorization vocabulary.** One OpenFGA type per CRD kind, with test
   cases. The model names things `app`, `contract`, `catalogue_entry`,
   `shared_instance`. Renaming the kind moves that vocabulary with it.
3. **May services consume each other's contracts?** None do today, which is why
   services can be provisioned in one pass after the kernel converges. If that
   changes, provisioning needs a topological sort. Worth stating as a rule
   rather than leaving as an accident.
4. **Do `addonActivation` and `addonValues` follow `addon` into the package?**
   (§3)
5. **Sequencing against the operator split.** The split plan edits
   `kernelRequirements` and `security.egress` in `BuildDesired`, and the
   `AppProfile` webhooks are in its critical path. §4 merges those fields. Only
   the split has a written cutover, so land it first.

---

## 15. What gets deleted when this lands

- `unified-app-crd-sketch.md`, the working note outside this repository.
- The guidance in `gentian-app-template` and `gentian-apps` that teaches
  `AppProfile` against `ComponentProfile` as a permanent choice. It is written
  as a design rather than a transition, so every new app entrenches the split.
- The term "ApiProfile", which was never a kind — only a nickname in comments
  for `deploymentMethod: api`.
