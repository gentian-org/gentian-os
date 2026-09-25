# Target component structure

**What this is.** The shape the catalogue should end up in: what a component
declares, what the platform derives from it, and where each rule is enforced.
Written as a target rather than as a diff.

**What it replaces.** Two documents.

- [component-profile.md](component-profile.md), the schema design that
  `ComponentProfile` was built from. Its reasoning survives and is folded in
  here; its vocabulary does not. It is now a redirect carrying a section map,
  because eight documents and one code comment cite it by section number.
  **Delete it, and repoint those citations, when this lands.**
- `unified-app-crd-sketch.md`, a working note outside this repository, written
  before `ComponentProfile` existed. It proposes `AppProfile` with an
  `AppClass` enum and is silent on integrations. Nothing in it survives that is
  not here. **Delete it when this lands.**

Companions, still current: [namespace-cleanup.md](namespace-cleanup.md) for
which namespace each tier lands in, [operator-split-plan.md](operator-split-plan.md)
for who writes the CR, and [architectural-decisions.md](architectural-decisions.md)
for the decisions behind all three.

**Out of scope: the kernel tier.** Argo CD, Keycloak, OpenBao, External
Secrets, Crossplane and Headlamp are installed by `install.sh` and then
reconciled by Argo CD. They are never profiles and never the operator's,
because the operator depends on them existing first. `kernel/namespaces.yaml`
names the tiers: `kernel | system | system-dmz | shared | tenant | tenant-dmz`.
Everything below is about the last five.

**Status.** Nothing in §2, §3 or §4 is built. Two of the renames break every
profile in the catalogue, so they are a change to the conversion script and
not an edit.

---

## 1. One kind, one instance, two levels

`ComponentProfile` is the cluster-scoped catalogue entry. `Component` is the
namespaced instance. That much exists and works.

The two levels are the point, and collapsing them would lose something worth
keeping. Whether a component *can* serve several tenants safely is a claim the
catalogue certifies. *Running* it shared is a decision a named person makes. A
profile that could go either way has to be able to say so, and the decision has
to be recorded apart from the claim.

So the same word at two levels:

- `ComponentProfile.spec.classes` is a **list** of the modes this component may
  be deployed under. A certification claim, reviewed with the entry.
- `Component.spec.class` is a **single value**, and must be a member of that
  list. The deployment decision.

`trustTier` stays in the spec rather than becoming a label. CRD validation
rules cannot read labels or annotations; only `name` and `generateName` are
exposed on metadata. In the spec, "shared requires platform tier" is a schema
invariant that fails at write time whoever is writing. Outside it, it could
only ever be an admission policy.

**No presentation fields beyond the tile.** A catalogue listing is reference
data outside the cluster (AD-3). The one exception is the tile on an exposure,
because the portal has to show what the cluster routes without asking a service
outside it. §8.1.

---

## 2. Three axes

A catalogue entry answers three independent questions. Today it answers the
first under a misleading name, the second in two places that can disagree, and
the third correctly.

### Axis 1 — class: who it serves

```yaml
spec:
  classes: [app]          # capability claim, reviewed in the catalogue
```

```yaml
# on the Component
spec:
  class: app              # the one mode chosen, must be in the profile's list
```

| value | serves | responsible | namespace | instances |
|---|---|---|---|---|
| `service` | other components over contracts, and operators at its own console | platform admin, via the Cluster claim | `system-<function>` | one |
| `app` | the people of one tenant | tenant admin, via the director | `tenant-<t>` (+ `-dmz`) | one per tenant |
| `shared-app` | the people of several tenants, from one backend | platform admin, via the director | `shared-<app>` | one |

`service` is exclusive: a component may not be both a service and an app.

**Why this replaces `tenancy: system | shared | tenant`.** The three current
values mix a placement word, a bare adjective and a scope word for what is one
question. The code gives the game away: the doc comment on `system` reads
*"serves contracts to other components ... serves no human and has no
exposure"*, which is the definition of a service, while `system` names only the
namespace it lands in. `shared` is an adjective with no noun. And once
`service` is a value, `tenancy` is no longer the question, so the field name
moves with the values.

**Precedent.** `StorageClass`, `IngressClass` and `PriorityClass` all use
*class* for "a named variety that behaves differently". One caveat: all three
are objects you reference, not enums, so a reader may briefly look for a
`ComponentClass` CRD that does not exist.

**A service may expose**, and the current rule that it may not is wrong. §8.

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

**This is not a field.** It is read from the package, and the schema makes the
package a discriminated union so it cannot be ambiguous. Adding a field would
be a third statement of a fact that two places already state, and those two
already disagree in the schema as written.

**Why a field would be wrong, concretely.** On `ComponentProfile` today:

- `package.deploymentMethod: api` alongside a `chart` is admissible. The old
  kind has a CEL rule forbidding it; the new kind does not.
- `package.chart` and `package.apiIntegration` together are admissible. The
  one-of rule is written as an OR, not an exactly-one. The same file already
  writes exactly-one correctly, for egress, a few hundred lines away.

The second is not even a disagreement about intent. The app template tells
authors *"Exactly one of chart, compositionRef or apiIntegration must be
present"*, so the documentation already states the rule the schema fails to
enforce.

So: delete `deploymentMethod`, make the union exactly-one, and derive.

**Where the axis becomes visible**, which is the thing actually wanted:

- a printcolumn, so `kubectl get componentprofiles` shows a DELIVERY column;
- a well-known label the operator sets, so `-l gentianos.io/delivery=api`
  selects;
- a view in the console beside Integrations listing every ingested API with its
  base URL, runtime and tenant binding.

That view is the exposure register. Unlike a field, it cannot drift.

**On the word.** `workload`, not `executable`. The codebase already uses it as
the antonym: `ProfileDeploysWorkload`, and *"a component that is an API client
rather than a workload"*.

**And note what the catalogue shows.** `litellm-me` is delivery `api` and
points at `litellm-proxy.platform-kernel.svc.cluster.local`, inside the
cluster. The axis is whether the platform runs it, not where it lives. The word
`external` would be wrong. (That URL also names a v4 namespace, a separate
bug.)

### Axis 3 — trustTier: how far it was reviewed

Already present, already required with no default, already load-bearing:
`shared-app` requires `platform`, and so does forwarding the edge token.
Nothing to change. Named here only so it is clear the model has three
dimensions.

---

## 3. `addon` is a package type

An addon is a catalogue entry that is not deployed. It flips a switch inside
another component's own addon system: an Odoo module name, a Nextcloud app id,
an Activepieces piece name. Structurally that is the same statement `api`
makes, namely that this entry is not run here and here is where it really
lives, and it belongs in the same field.

```yaml
spec:
  classes: [app]
  package:
    addon:
      id: mrp
      of: odoo-base-ce
```

Today it is `spec.customization.addon`, and the schema strains in five places
because of it.

**1. The one-of rule reaches out of `package`.** It reads, verbatim:

```
has(package.chart) || (has(package.compositionRef) && ...) ||
has(package.apiIntegration) || (has(self.customization) && has(self.customization.addon))
```

A discriminator that has to reach into a sibling block to finish its own union
is in the wrong block.

**2. The kind is decided by an annotation, not by either field.**
`EffectiveDeploymentRole` reads `gentianos.io/deployment-role`, and that is
what the addon resolver checks. So *is this an addon* is answered by an
annotation, *which addon, and of what* by `customization.addon`, and *how is it
delivered* by `package`. Three places, one question.

**3. The profiles disagree with themselves.** Of the 20 addon profiles in the
catalogue, 11 declare a `chart`. `odoo-mrp-ce` declares
`deploymentMethod: crossplane`, a `compositionRef`, a chart version and its own
CPU and memory requests, alongside `customization.addon: {id: mrp, of:
odoo-base-ce}`. It describes itself as deploying something it does not deploy.
The `of` field's own comment explains why: it replaced an annotation that
existed to auto-install a base when an addon was installed standalone, *"the
relationship the L3 cleanup inverts"*. Those fields are from the standalone era
and nothing removed them.

**4. `ProfileDeploysWorkload` gets it wrong.** It is `!ProfileIsAPI(p)`, so an
addon reports that it deploys a workload.

**5. The derived delivery axis was incomplete**, which is how this surfaced. An
addon is neither `workload` nor `api`: nothing is run and nothing is routed.

### What `customization` keeps

Most of that block rates how changeable a deployable app is: grade, rubric
score, supported rungs, drop-ins, `extension`, `publishes`, `repackage`. That
is an assessment. `addon` is not an assessment — it says this entry is not an
app at all.

The block already mixes sides. `addon` is set on the addon, while
`addonActivation` and `addonValues` are documented as *"set on the base
profile, not on the addons"*. So one field is a kind discriminator and the
others are the base's machinery.

**The fair counter-argument**: an addon *is* rung L3, the app's own addon
system, so grouping it with the ladder has a logic. But a rung is a property of
the relationship, not of the entry, and the one-of rule already treats `addon`
as a kind by putting it in the union. The base's side of L3, `extension`, is
genuinely a ladder statement and stays where it is.

**Open, and not decided here:** whether `addonActivation` and `addonValues`
follow into `package`. They change the *base's* chart values, which is a
packaging concern, but they are tied to the ladder. Decide it when the addon
move is made.

### The conversion blocker this exposes

All 20 addon profiles carry a tile, and an addon's tile points into its base:
`linkSuffix: "/odoo/action-mrp..."`, `linkTarget: embedded`. In the new model a
tile hangs off an `expose` entry, and an exposure's `backend` is required and
documented as *"a Service in the component's own namespace"*. An addon has no
Service of its own.

So an addon with a tile cannot be expressed as a `ComponentProfile` today, and
that is all 20 of them. Either an exposure's backend may name another
component's Service, or an addon's tile is declared differently from an app's.
Answer this before the Odoo or Nextcloud families convert.

---

## 4. Requires, integrations and provides

Three relationships, and the current schema calls two of them by the same word.

### 4.1 `requires` and `integrations` differ in six ways

| | `requires` | `integrations` |
|---|---|---|
| Counterparty | the platform | another component |
| If unmet | does not start | runs normally |
| Whose fault | the platform's | nobody's |
| When resolved | once, before install | continuously |
| Consent | implicit in using the platform | explicit tenant grant |
| Signal | alert the platform admin | a status note, never an alert |

A boolean on a shared list hides all six. They are separate fields because they
are separate controller paths: requirements gate admission of the component,
integrations reconcile forever after. An unmet requirement should surface as a
condition naming the platform as responsible, not as a component error a tenant
administrator cannot act on.

Keycloak's SMTP is the worked example already in the tree: the relay is
supplied after install, and a realm without it simply cannot send invitations.
That is an integration. Keycloak's database is a requirement.

### 4.2 `requires.services`, not `kernelRequirements` and not `contracts`

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
    privileges:               # §4.3
      podSecurity: [...]
      egress: [...]
      clusterRoles: [...]
  provides:
    - name: project-management
      protocol: http-json
  integrations:
    - contract: central-navigation
      provider: desktop
```

**Why `kernelRequirements` has to go.** It names a fulfiller and picks the
wrong one. A database comes from a component of class `service`. Mail may be a
relay outside the cluster. Object storage may be a bucket at a cloud provider.
None of it comes from the kernel.

**Why not `requires.contracts`**, which is where it landed on
`ComponentProfile`: that word is already taken by the second column, and the
two sets are genuinely different things. The rename also only half happened
there — the Go type behind `requires.contracts` is still `KernelRequirements`.

**Why `services` and not `resources`.** Backstage, Radius and Score all use
*resource* for this, so the external convention favours it. Inside this product
*resources* already means quota: `ResourcePlan`, `Tenant.spec.quotas`, the
console's Resources screen. A collision one screen away beats a convention one
repository away. `services` also pairs with class `service`, so the model
explains itself: a component of class `service` is what fulfils
`requires.services`.

Go type: `ServiceRequirements`.

**One inaccuracy to fix while in there.** The doc comment on the current field
lists *"identity, database, object storage, cache, mail, LLM, MCP"*. There is
no LLM member in the struct. Either add it or stop promising it.

**The scope of `provides` follows the class.** Cluster-wide for a `service` and
a `shared-app`; within the tenant for an `app`.

### 4.3 Privileges: asking, and being granted

Two different things have been wearing one word. A cluster's
`PlatformSecurityPolicy` is a standing list of what may be **asked for** here
at all. Granting is the separate act of saying *this* component, in *this*
tenant, may have *this* privilege, with a person, a time and a reason.

Only the first exists today. The operator intersects a profile's waivers with
the allowlist and applies whatever survives; egress is not even filtered. An
intersection is a filter, so there is no approver, no timestamp and nothing in
a decision log. That is why "every one needs approval" was not true.

**What the profile asks for.** Every request carries a `name`, so a grant can
refer to it as `<kind>/<name>`, and a `reason` the approver reads.

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

**Who approves is the kind, not a field.** Scope is a property of what is being
asked for, never something the profile states, or a component could ask for the
cheaper approver.

| kind | scope | approver | relation |
|---|---|---|---|
| pod-security waiver | cluster: weakens an admission rule protecting the node | security officer | `cluster#can_approve` |
| cluster role | cluster: reaches the Kubernetes API | security officer | `cluster#can_approve` |
| egress beyond baseline | tenant: leaves the tenant's own namespace | tenant administrator | `tenant#can_approve_privilege` |

**What the grant is**, on the Component, written only by the director from the
caller's token. The same shape as an exposure enablement, for the same reason.
`approver` and `approvedAt` are immutable: a new approval is a new grant.

```yaml
# on the Component
spec:
  privileges:
    - privilege: egress/smtp-relay
      approver: <keycloak subject>
      approvedAt: 2026-09-25T09:00:00Z
      reason: "In the approver's words, not the profile's."
      expiresAt: 2027-09-25T09:00:00Z     # a waiver with no expiry is one nobody reviews
```

An install with an ungranted privilege does not proceed, and is not rejected
either. It **waits**, visible in the console as a pending request, which is a
state the tenant administrator can act on or escalate. That is the difference
between a queue and a failure, and it is why the request has to be an object
rather than a field the operator silently drops.

This closes the asymmetry recorded as
[security-gap-closing.md](security-gap-closing.md) G27: MAC waivers are
intersected against the cluster allowlist, so an administrator approves them,
while `security.egress` is copied straight into a NetworkPolicy with no
equivalent check. A profile can currently grant itself outbound network access
but not a pod-security exception. One requirement block with one approval path
removes that by construction.

---

## 5. Secrets: three kinds, two declared

| kind | origin | declared |
|---|---|---|
| granted credential | arrives with a fulfilled requirement | **no** — restating it creates drift |
| generated | random, created once, held in the vault | yes, under `secrets.generated` |
| derived | deterministic from tenant and component name | yes, but prefer generated |

Generated secrets have no counterparty: an admin bootstrap password, a session
signing key, a data-at-rest key. Nothing external can produce them.

`secrets` is `{generated, derived}` and not one list, because the two have
different failure modes and a reviewer should see which is which at a glance.

Derivation deserves scrutiny rather than preservation. Its stability comes from
recomputation rather than from storage, so the formula and its inputs are
load-bearing forever, and a change to either silently rotates the value for
every existing tenant. That failure mode has already occurred here, on the
derivation salt. Generated and stored is the more robust default.

---

## 6. What the class derives

| | `service` | `shared-app` | `app` |
|---|---|---|---|
| namespace | `system-<function>` | `shared-<app>` | `tenant-<t>` (+ `-dmz`) |
| north-south route | its own console, in the kernel zone, gateway only | per granted tenant | the tenant's gateway |
| OIDC client | one | per granted tenant realm | the tenant realm |
| scope of `provides` | cluster-wide | cluster-wide | within the tenant |
| tenant binding | none | a grant per tenant | implicit |
| `defaultForTenants` | meaningless | meaningless | allowed |
| delete blast radius | every consuming component | every granted tenant | one tenant |

Nothing here is a new field. The class reinterprets the scope of declarations
the profile already makes. One schema, three readings.

---

## 7. Shared instances: offered, then installed

A shared instance existing is not the same as a tenant having the app.
Installing one is the platform administrator's act; putting it in front of a
tenant's users is still the tenant administrator's. Two steps, two people:

1. The platform administrator installs the component with `class: shared-app`
   into `shared-<app>` and **offers** it to tenants, one
   `shared_instance:<p>#offered_to@tenant:<t>` tuple each, written under
   `can_grant_shared`. Nothing is visible to anyone yet.
2. The tenant administrator installs the app. The tenant gets its own `app`
   object, its own per-app group, its own OIDC client in its own realm, its own
   route and its own tile. Only the backend is shared.

**The reconciler chooses, and the default is unchanged.** When a tenant names
profile P:

```
is there a shared instance of P offered to this tenant?
  no  → install a dedicated release in tenant-<t>        (today's behaviour)
  yes → bind: create the tenant-side objects only, with the route's backend
        in shared-<app> and a NetworkPolicy allowing that one hop
```

Binding creates no Release. Everything a person meets is still per tenant, so
from inside the tenant a bound app and a dedicated one are indistinguishable.
That is what lets the choice be an operational one rather than a product one.

**Availability decides by default, but never silently.** Where the data lives
is something a tenant is entitled to know before installing, and to refuse. The
director's read of an installable app says which fulfilment an install would
get, the store and the console show it, and the tenant may pin it:

```yaml
spec:
  fulfilment: auto        # auto (default) | dedicated
```

`auto` keeps today's behaviour. `dedicated` is the tenant saying its data does
not go on a backend other tenants use, and the reconciler honours it even where
an offer exists. There is no `shared` value: a tenant cannot demand a backend
the platform has not offered.

The instance records what it got, so a later offer does not silently move a
running app: a bound instance stays bound, a dedicated one stays dedicated
until somebody reinstalls it.

Withdrawing an offer uninstalls nothing. It stops new tenants binding. Removing
a bound tenant is an uninstall in that tenant, which is somebody's action and
never a side effect of a tuple delete.

---

## 8. Exposure

Two things the schema has to get right, both carried by
[security-gap-closing.md](security-gap-closing.md) G3.

**`authMode` is required on every entry, with no default.** `none` has to be a
word somebody wrote and a reviewer can find. A default defeats exactly that, as
the old `BrowserProxyRoute.authMode` did by defaulting to `forward-bearer`.

**A gateway route and a perimeter surface are different objects.** Publishing
proxies live in `tenant-<t>-dmz` (AD-6) with one least-privilege credential per
surface, separate from routes on the authenticated gateway. They differ in
namespace, credential, policy and blast radius, so `surface: gateway |
perimeter` is what the operator reads to build the DMZ namespace. One concept,
one place, replacing a separate `publicSurfaces` list.

An entry also carries `paths`, `denyPaths`, `stripPrefix`, an optional `source`
restriction, `forwardToken`, a `backend` and an optional `tile`.

- **`denyPaths`** exists for a component whose public surface is "the site
  except its admin". Deny wins over allow regardless of specificity, so a broad
  allow with narrow denials stays readable rather than becoming a precedence
  puzzle. **It is in the schema and nothing applies it.** No controller reads
  the field. That is S7A.13: build it or take it out.
- **`source`** pins the caller before `authMode` is considered. The case it
  exists for is a callback that must come from one known peer: Collabora's WOPI
  callbacks are `authMode: none` and safe only because the caller is pinned.
  Exactly one of `cidrs` or `component`, and `component` is preferred because
  CIDRs age badly.
- **`forwardToken`** asks the gateway to pass the edge access token to the
  backend. Default false: a backend gets identity headers, not a bearer that is
  also valid at the director and at every sibling. Only a platform-tier
  component that calls the director on a person's behalf sets it, which is why
  CEL ties it to `trustTier: platform`. Meaningless on a perimeter entry, which
  has no session.

### 8.1 A service may expose, and tiles hang off exposures

**The rule that a service has no exposure is wrong.** Today CEL says a `system`
component has no `expose` at all, on the profile and on the instance, and the
stated reason is that *"a component serving contracts does not also serve
humans"*. That conflates two claims:

- **True, and it stays:** a service's *contract* surface is in-cluster. Another
  component reaches it over a plain Service. That is not an exposure and never
  was.
- **False:** that a service therefore has no north-south surface. A shared
  Postgres with a query console, a model gateway with an operator console, an
  object store with a browser — each is one component, not two.

**A correction worth recording.** An earlier draft of this claimed the rule was
what blocks S7A.15, because the tenant composition carries
`$zoneHosts := (list "console" "admin" "argocd" "headlamp" "id")` and three of
those are services with consoles. That was wrong. Argo CD, Headlamp and
Keycloak are **kernel** tier, installed by `install.sh`, and are not profiles at
all — so relaxing this rule does not make them components. S7A.15 covers two of
those five hosts today and the other three need a different answer. The rule
still has to be relaxed, for system-tier services, but it is not S7A.15's
blocker.

**What is actually invariant about a service**, and should be the rule instead:

1. **One instance, never one per tenant.** `defaultForTenants` is meaningless
   on it and its exposure is not multiplied.
2. **Gateway only, never perimeter.** A service console sits behind the kernel
   session. Publishing one on the perimeter, which by definition has no
   session, is never right. The instance-level rule already says this for
   perimeter enablements; this lifts it to the profile.
3. **Its tile asks on the cluster.** Not on a tenant and not on an app. The
   authorization model already has the relations: `can_operate_system` for
   `service_admin`, plus `can_configure` and `can_audit`, all on `cluster`.

Point 3 needs one schema change. `TileObject` offers `app` and `tenant` and
nothing else, so a service console has no object to ask a relation on. It needs
`cluster`.

**A tile lives on the exposure, not on the component**, because a tile is a
link to one host and one path and the exposure is what decides those. The
operator projects the catalogue from the routes it actually composes, so a tile
and the thing it points at cannot disagree. A tile carries a `relation` and an
`object`: nobody is shown a tile they may not open, and the portal is not where
that is decided.

The field is `object` and not `on`, because YAML 1.1 reads a bare `on` as the
boolean `true`, so a hand-written profile would carry a key named `true` and be
refused by the schema. The same goes for `off`, `yes` and `no`. This was found
the hard way.

### 8.2 `launch`: a person must be able to reach what they installed

An `app` is not required to have a tile, and must not be. Two of the platform's
own components prove it:

| component | exposures | tiles |
|---|---|---|
| desktop | `api`, `web` | none, on either |
| admin console | `api`, `web` | on `web` only |

The desktop is the surface tiles appear on, so it does not appear on itself.
The admin console's API entry is reachable and unadvertised, which is what an
API entry should be. A blanket "every app has a tile" rejects the first
outright.

**The gap** is that the schema cannot tell *deliberately unadvertised* from
*somebody forgot*. A tenant administrator installs an app, it runs, and there
is no way to open it. Nothing catches that.

The rule worth having is that a person must be able to reach every app they
installed, and that happens in exactly three ways:

1. **a tile**, on one of its exposures;
2. **another component opens it** — Collabora from Nextcloud, a viewer from a
   file manager. Common in a suite, and the reason the blanket rule is wrong;
3. **nothing opens it** — either because it *is* the launcher, which is the
   desktop and only the desktop, or because it has no human surface at all,
   which is a database.

Only the first is expressible today, so the second and third are
indistinguishable from an omission. One field fixes it, and says something the
model currently cannot say at all, which is *which* component opens this one:

```yaml
spec:
  launch: tile                  # at least one expose entry carries a tile
  # launch: {from: file-store}  # opened by whatever provides this contract
  # launch: none                # the launcher itself, or no human surface
```

A `service` with a console sets `launch: tile` like anything else; a service
with no console sets `none` and has no `expose`.

### 8.3 Enablement: the tenant's half

The profile declares what *may* be published. What *is* published is decided on
the instance, never on the profile ([networking.md](networking.md) §8.1).
Gateway entries need no enablement: they carry the session and are always on.
**Perimeter entries are off until a perimeter approver enables them.**

```yaml
# on the Component
spec:
  exposures:
    - exposureName: public-share     # must name a perimeter entry of the profile
      host: share.acme.example       # empty means the entry's default host
      owner: <keycloak subject>      # immutable: a renewal by someone else is a new enablement
      expiresAt: 2026-12-24T00:00:00Z
      reviewAt: 2026-11-24T00:00:00Z
```

`authMode` is deliberately not repeated here. The profile's entry is the one
source and an enablement cannot weaken it. The field is `exposureName` and not
`surface`, because `surface` is the enum on the profile's entry and one field
name meaning two things in adjacent structs is how a schema starts drifting.

`expiresAt` is always set. A public surface with no end is not something
anybody decided. At expiry the operator treats the enablement as absent and
removes the proxy, route and listener, while the entry stays in git as history
— which also keeps the next Argo CD sync idempotent.

A host in the tenant's own zone needs no certificate work: the zone's DNS-01
wildcard already covers it. A vanity host is admitted only if the tenant's
approved domains include it, and its certificate comes by HTTP-01, because the
platform holds no credential to a customer's DNS zone and does not want one.

Who may write one is `can_expose` on the tenant, checked by the director.

**The cluster half is a ceiling, not a permission list.** No component is ever
published by default at any trust tier, so this block has nothing to grant. It
bounds what an approver may choose.

```yaml
exposure:
  # Modes this cluster refuses outright, whatever a profile declares or an
  # approver chooses. Usually empty: the control is the enablement, not the mode.
  denyAuthModes: []
  requireExposurePolicyContract: true
  # Required, with no "absent" case: two optional fields whose joint default is
  # a permanent public surface is not a safe default.
  defaultLifetime: 2160h         # 90 days
  maxLifetime: 8760h
  reviewInterval: 720h
```

An earlier draft gated `authMode: none` on `trustTier: platform`. That
conflated two unrelated risks — platform tier certifies that one instance can
safely serve several tenants, which says nothing about whether an anonymous
request may reach it — and it would have refused the first Nextcloud share
link, since those are ordinary tenant-tier apps.

### 8.4 The `exposure-policy` contract

A profile with any `authMode: none` entry should declare
`provides: [{name: exposure-policy}]`, and must wherever the cluster sets
`requireExposurePolicyContract`, which is the default.

That is an admission check and not a CRD rule: the requirement lives on the
Cluster claim and CRD rules cannot read another object. It is refused when the
enablement is written, which is the only moment it matters.

The contract gives the platform, with the tenant's credential from the binding,
`policy.read` and `policy.write` for the app's public-sharing policy — default
and maximum object expiry, password required, which groups may share publicly,
anonymous upload — and `objects.list` and `objects.revoke` for the app's public
objects with owner, created, expiry and a hashed token. The platform writes the
tenant's policy into the app whenever the cluster policy or the enablement
changes.

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
  classes: [app]                 # was tenancy
  launch: tile                   # new: how a person gets to it
  trustTier: platform
  version: "1.4.2"

  package:                       # exactly one of chart | composition | api | addon
    chart:
      repository: oci://ghcr.io/gentian-org/charts
      name: openproject
      version: 16.1.0
    valueMapping: {...}
    extraValues: {...}

  requires:                      # was kernelRequirements
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

  sessionMaxAge: 8h              # only for a component that runs its own login
  defaultForTenants: false
```

`sessionMaxAge` caps the component's **own** session, for a component that
establishes one through its own client rather than relying on the gateway's
headers. The edge bounds reachability; it does not refresh the group model a
component captured at its own login. So this value, not the access-token
lifetime, is the bound on what a person may still do inside it after their
rights change (AD-13). Meaningless for a component with no login of its own.

An entry whose package is an API differs in one block and nothing else:

```yaml
  package:
    api:                         # was apiIntegration
      runtime: portal-proxy
      baseUrl: https://corp.desk.gentian.org
      tenantBinding: tenant-domain
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
        description: Which models are available, and what they cost
        icon: model
        relation: can_operate_system
        object: cluster          # new value
```

The instance records answers, never requests:

```yaml
apiVersion: gentianos.io/v1alpha1
kind: Component
metadata: {name: openproject, namespace: tenant-acme}
spec:
  profileRef: {name: openproject, digest: "sha256:..."}
  class: app                     # was tenancy
  fulfilment: auto
  addons: []
  exposures: []                  # perimeter entries switched on, with owner and expiry
  privileges:                    # what a named person granted
    - privilege: egress/smtp-relay
      approver: ...
      approvedAt: ...
```

---

## 10. Where enforcement goes

**CEL for what the object may say. Admission policy for who may say it.**
Keeping that boundary deliberate is worth more than putting every rule in one
place: CEL cannot see the writer or the namespace, and an admission policy
cannot be relied on to run at every write path.

### CEL, existing and correct, restated with the new words

- `service` is exclusive: `classes` may not contain it alongside another.
- `shared-app` requires `trustTier: platform`.
- `forwardToken` requires `trustTier: platform`.
- `class` and `profileRef.name` are immutable on the instance.
- `fulfilment` applies to class `app` only.
- a perimeter entry cannot use `authMode: oidc`, and `forwardToken` is
  meaningless on one.
- exactly one of `source.cidrs` or `source.component`.

### CEL, new

- **the package is exactly one of `chart`, `composition`, `api` or `addon`**,
  with no exception and nothing to reach for outside `package`;
- **`deploymentMethod` does not exist**, so nothing can contradict the package;
- **a `service`'s exposures are all `surface: gateway`**, never perimeter, and
  it switches on no perimeter enablement. This *replaces* the current rule that
  a service has no exposure at all;
- **a `service`'s tile asks `object: cluster`**, and an `app` or `shared-app`
  tile does not;
- **`defaultForTenants` is false for a `service`**, which has one instance;
- **`launch: tile` requires at least one `expose[].tile`**, and `launch: from`
  and `launch: none` require none, so an app nobody can open is refused at
  admission rather than installed and lost.

### Admission policy

- a Component in a tenant namespace must have `class: app`;
- `class` must be a member of the referenced profile's list;
- only the platform administrator may create Components in `system-*` or
  `shared-*`;
- a `none` surface must provide the `exposure-policy` contract wherever the
  cluster requires it (§8.4).

### Still a promise the CRD makes and does not keep

- `expose[].denyPaths` is in the schema and no controller reads it. S7A.13.
- `package.api.runtime: proxy` is in the enum and has no case in the route
  builder, so it falls through to the default service-backed rule and routes to
  a Service that an API-delivered entry never creates. A silent 503 rather than
  a refusal. Same decision: build it or take it out of the enum.

---

## 11. Why the model closes

A tenant administrator cannot create a service, because the only class
creatable in a tenant namespace is `app`. They never need to: what a component
requires arrives through `requires.services`, fulfilled by somebody else.

When something needs what no contract covers, it ships as an extension inside
the component's own pod. That stays in the tenant namespace, in tenant
ownership and in the tenant's blast radius. The escape hatch never creates a
cluster-scoped object, which is why there is no hole.

---

## 12. What changes, in one table

| today | target | breaks |
|---|---|---|
| `spec.tenancy: [system\|shared\|tenant]` | `spec.classes: [service\|app\|shared-app]` | every profile |
| `Component.spec.tenancy` | `Component.spec.class` | every install |
| Go `ComponentTenancy` | Go `ComponentClass` | nothing on the wire |
| `spec.kernelRequirements` (old kind) | `spec.requires.services` | every profile |
| `spec.requires.contracts` (new kind) | `spec.requires.services` | 2 profiles |
| Go `KernelRequirements` | Go `ServiceRequirements` | nothing on the wire |
| `package.apiIntegration` | `package.api` | 2 profiles |
| `package.deploymentMethod` | deleted | 2 profiles |
| `package.compositionRef` | `package.composition` | nothing yet, unused |
| `customization.addon` | `package.addon` | 20 profiles |
| annotation `deployment-role` | deleted, read from the package | 20 profiles |
| nothing | `spec.launch` | new field, default `tile` |
| `TileObject: app\|tenant` | `app\|tenant\|cluster` | nothing, additive |
| CEL "system has no expose" | "service is gateway-only" | nothing yet, no service profiles exist |

---

## 13. What has to happen first

In order. The first three are not naming work and block everything else.

1. **`Tenant.spec.apps` resolves `AppProfile` only**, at every site that reads
   it. Until a `ComponentProfile` can be installed into a tenant by naming it
   there, the catalogue cannot move and there is nothing to rename.
2. **The component reconciler refuses any package that is not a chart**
   (*"only package.chart is reconciled yet"*). Converting the two API-delivered
   profiles today would stop them working, and there is no addon path in it at
   all.
3. **An addon's tile has no backend it may name.** All 20 addon profiles carry
   a tile pointing into their base, and an exposure's `backend` is required and
   must be a Service in the component's own namespace. Answer §3's last
   question before converting anything in the Odoo or Nextcloud families.
4. Run the conversion in `gentian-apps` and settle its review items. The
   converter exists and all profiles convert; nobody has run it for real.
5. Apply the renames in `convert-appprofile.py`, so the conversion and the
   rename are one migration rather than two.
6. Retire `AppProfile`, `AppCatalogue` and the `App` claim.

---

## 14. Open decisions

1. **Fulfiller selection.** With services as real instances, a `database`
   requirement must resolve to a specific one. The class says a Postgres
   exists, not which. Needs a default per contract on the Cluster claim, or an
   explicit selector.
2. **The authorization vocabulary.** One OpenFGA type per CRD kind, shipped
   with its test cases. The model names things `app`, `contract`,
   `catalogue_entry`, `shared_instance`. Renaming the kind moves that
   vocabulary with it, and `catalogue_entry` is arguably the better name for
   the entry — but only if chosen deliberately.
3. **May services consume each other's contracts?** Today none do, by
   construction, which is why services can be provisioned in one pass after the
   kernel converges. If that stops being true, provisioning needs a topological
   sort it does not need now. Worth stating as a rule rather than leaving it as
   an accident.
4. **Do `addonActivation` and `addonValues` follow `addon` into the package?**
   §3.
5. **Sequencing against the operator split.** The split plan edits
   `kernelRequirements` and `security.egress` in `BuildDesired`, and the
   `AppProfile` webhooks are in its critical path. §4 merges those fields. Both
   touch the same code and only the split has a written cutover. Land the split
   first.

---

## 15. What gets deleted when this lands

- `component-profile.md`, this document's predecessor in this repository.
- `unified-app-crd-sketch.md`, a working note outside it.
- The guidance in `gentian-app-template` and `gentian-apps` that teaches
  `AppProfile` versus `ComponentProfile` as a permanent choice an author makes.
  It is written as a design, not a transition, and every new app written
  against it entrenches the split.
- The word "ApiProfile", which never was a kind — only a nickname in comments
  for `deploymentMethod: api`.
