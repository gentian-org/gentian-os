# Target component structure

**What this is.** The shape the catalogue should end up in, written as a
target rather than as a diff. It is the answer to three naming questions
raised on 2026-09-25 and to two schema holes found while answering them.

**What it replaces.** `unified-app-crd-sketch.md`, a working note that is not
in this repository. That file was written
before `ComponentProfile` existed and still proposes `AppProfile` plus an
`AppClass` enum; its three-way split is right and its names are not, and it is
silent on integrations, which turn out to be a second axis. **Delete it once
this is implemented.** Until then it is still the only place the shared-instance
grant model and the fulfiller-selection question are written down.

**Status.** Nothing here is built. Two of the renames break every profile in
the catalogue, so they are a conversion-script change and not an edit.

---

## 1. One kind, one instance

`ComponentProfile` is the cluster-scoped catalogue entry: what a component is,
what it needs, and what it may expose. `Component` is the namespaced instance:
what was answered, by whom, and until when.

That much exists and works. Everything below is what the entry should say.

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

| value | serves | who is responsible | exposure |
|---|---|---|---|
| `service` | other components, over contracts | the cluster administrator | none, by rule |
| `app` | the people of one tenant | the tenant administrator | its own, in the tenant's zone |
| `shared-app` | the people of several tenants, from one backend | the cluster administrator | its own, per tenant |

`service` is exclusive: a component that serves contracts does not also serve
humans. If it appears to, it is two components.

**Why this replaces `tenancy: system | shared | tenant`.** The three current
values mix a placement word, a bare adjective and a scope word for what is one
question. The code already gives the game away: the doc comment on `system`
reads *"serves contracts to other components ... serves no human and has no
exposure"*, which is the definition of a service, while `system` itself names
only the namespace it lands in. `shared` is an adjective with no noun. And
once `service` is a value, `tenancy` is no longer the question being asked, so
the field name moves with the values.

**Precedent.** `StorageClass`, `IngressClass` and `PriorityClass` all use
*class* for "a named variety that behaves differently". One caveat worth
knowing: all three are objects you reference, not enums, so a reader may
briefly look for a `ComponentClass` CRD that does not exist.

### Axis 2 — delivery: does the platform run it, or only route to it

```yaml
spec:
  package:
    chart: {...}          # exactly one of chart | composition | api
```

| delivery | package holds | meaning |
|---|---|---|
| `workload` | `chart` or `composition` | the platform runs it |
| `api` | `api` | the platform routes to something already running |

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

The second one is not even a disagreement about intent. The app template tells
authors *"Exactly one of chart, compositionRef or apiIntegration must be
present"*, so the documentation already states the rule the schema fails to
enforce.

So: delete `deploymentMethod`, make the union exactly-one, and derive.

**Where the axis becomes visible**, which is the thing actually wanted:

- a printcolumn, so `kubectl get componentprofiles` shows a DELIVERY column;
- a well-known label the operator sets, so `-l gentianos.io/delivery=api`
  selects;
- a view in the console beside Integrations listing every ingested API with
  its base URL, runtime and tenant binding.

That view is the exposure register. Unlike a field, it cannot drift from the
truth.

**On the word.** `workload`, not `executable`. The codebase already uses it as
the antonym: `ProfileDeploysWorkload`, and *"a component that is an API client
rather than a workload"*.

**And note what the catalogue shows.** `litellm-me` is delivery `api` and
points at `litellm-proxy.platform-kernel.svc.cluster.local`, which is inside
the cluster. The axis is whether the platform runs it, not where it lives.
The word `external` would be wrong. (That URL also names a v4 namespace, which
is a separate bug.)

### Axis 3 — trustTier: how far it was reviewed

Already present, already required with no default, already load-bearing:
`shared` requires `platform`, and so does forwarding the edge token. Nothing
to change. Named here only so it is clear the model has three dimensions.

---

## 3. Requirements and contracts are two vocabularies, not one

This is the second rename, and the obvious landing place is taken.

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
    privileges:               # unchanged
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
wrong one. A database comes from a component of class `service`. Mail may be
a relay outside the cluster. Object storage may be a bucket at a cloud
provider. None of it comes from the kernel.

**Why not `requires.contracts`**, which is where it landed on
`ComponentProfile`: that word is already taken by the second column, and the
two sets are genuinely different things. Note also that the rename only half
happened there, since the Go type behind `requires.contracts` is still
`KernelRequirements`.

**Why `services` and not `resources`.** Backstage, Radius and Score all use
*resource* for this, so the external convention favours it. Inside this
product *resources* already means quota: `ResourcePlan`, `Tenant.spec.quotas`,
the console's Resources screen. A collision one screen away beats a convention
one repository away. `services` also pairs with class `service`, so the model
explains itself: a component of class `service` is what fulfils
`requires.services`.

Go type: `ServiceRequirements`.

**One inaccuracy to fix while in there.** The doc comment on the current field
lists *"identity, database, object storage, cache, mail, LLM, MCP"*. There is
no LLM member in the struct. Either add it or stop promising it.

---

## 4. The entry, end to end

```yaml
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: openproject
spec:
  classes: [app]                 # was tenancy
  trustTier: platform
  version: "1.4.2"

  package:                       # exactly one of chart | composition | api
    chart:
      repository: ...
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
      backend: {name: openproject, port: 80}
      tile:
        displayName: Projects
        description: Plans, tasks and timelines
        icon: project
        relation: can_use
        object: component

  defaultForTenants: false
```

An entry whose package is an API differs in one block and nothing else:

```yaml
  package:
    api:                         # was apiIntegration
      runtime: portal-proxy
      baseUrl: https://corp.desk.gentian.org
      tenantBinding: tenant-domain
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

## 5. Rules the schema must enforce

Existing and correct, restated with the new words:

- `service` is exclusive: `classes` may not contain it alongside another.
- `service` components have no `expose`, on the profile and on the instance.
- `shared-app` requires `trustTier: platform`.
- `forwardToken` requires `trustTier: platform`.
- `class` and `profileRef.name` are immutable on the instance.
- `fulfilment` applies to class `app` only.
- a perimeter entry cannot use `authMode: oidc`, and `forwardToken` is
  meaningless on one.

New, and the reason this document exists:

- **the package is exactly one of `chart`, `composition` or `api`**, except
  for an addon, which rides on its base and has none;
- **`deploymentMethod` does not exist**, so nothing can contradict the package.

Still unbuilt and still a promise the CRD makes:

- `expose[].denyPaths` is in the schema and is not applied. This is S7A.13:
  build it or take it out.
- `package.api.runtime: proxy` is in the enum and has no case in the route
  builder, so it falls through to the default service-backed rule and routes
  to a Service that an API-delivered entry never creates. A silent 503 rather
  than a refusal. Same decision: build it or take it out of the enum.

---

## 6. What changes, in one table

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

---

## 7. What has to happen first

In order. The first two are not naming work and block everything else.

1. **`Tenant.spec.apps` resolves `AppProfile` only**, at every site that reads
   it. Until a `ComponentProfile` can be installed into a tenant by naming it
   there, the catalogue cannot move and there is nothing to rename.
2. **The component reconciler refuses any package that is not a chart**
   (*"only package.chart is reconciled yet"*). Converting the two
   API-delivered profiles today would stop them working.
3. Run the conversion in `gentian-apps` and settle its review items. The
   converter exists and all profiles convert; nobody has run it for real.
4. Apply the renames in `convert-appprofile.py`, so the conversion and the
   rename are one migration rather than two.
5. Retire `AppProfile`, `AppCatalogue` and the `App` claim.

## 8. What gets deleted when this lands

- `unified-app-crd-sketch.md`, this document's predecessor.
- The guidance in `gentian-app-template` and `gentian-apps` that teaches
  `AppProfile` versus `ComponentProfile` as a permanent choice an author
  makes. It is currently written as a design, not a transition, and every new
  app written against it entrenches the split.
- The word "ApiProfile", which never was a kind — only a nickname in comments
  for `deploymentMethod: api`.
