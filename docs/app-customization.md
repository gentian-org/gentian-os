# App Customization Framework — the Gentian Customization Ladder

**Status:** **Live** — accepted 2026-08-06, delivering in **v0.4**. See §12 for per-step status.
**Companion to:** [design/app-catalogue.md](design/app-catalogue.md), [design/app-profiles.md](design/app-profiles.md),
[design/multi-tenancy.md](design/multi-tenancy.md), [gentian-apps/docs/app-profile-guide.md](https://github.com/gentian-org/gentian-apps/blob/main/docs/app-profile-guide.md)

---

## 0. Problem statement

Today Gentian has customization *mechanisms* — `extraValues`, `Tenant.spec.apps[].config`,
profile annotations, `composition.yaml`, the `gentian-sidecar-git-modules` addon sync, the
`ocb` Odoo fork — but no **ordering** over them. Nothing tells a human or an agent:

* Which mechanism is the *cheapest* one that can express this change?
* Which mechanisms does *this particular app* actually support?
* Who pays when the app is upgraded, and how do we ever get back down?

The result is the failure mode every ERP/ITSM platform hits: customizations land at whatever
rung the author happened to know, upgrade cost compounds silently, and the platform ossifies.
ServiceNow calls this the "customization conundrum"; SAP's answer is "Clean Core". We need the
same discipline, expressed in a Debian/Fedora idiom, and machine-readable enough that an agent
can follow it without judgement.

This document proposes:

1. **A seven-rung ladder** (§2) ordered by how much of the app's own artifact Gentian ends up owning.
2. **A second, independent axis — scope** (§3): tenant / profile / platform blast radius.
3. **A per-app capability declaration** — `AppProfile.spec.customization` (§4) — so the ladder is
   *app-specific*: which rungs are reachable at all, and by which mechanism.
4. **A customization record** — DEP-3-inspired manifest (§5) — so every rung ≥ L2 is tracked,
   owned, dated, and has exit criteria.
5. **A deterministic decision procedure for agents** (§6).
6. **Best practices, templated into `gentian-app-template`** (§7) so first-party apps are *born*
   customizable at L1–L3 instead of forcing everyone to L5.
7. **Governance, CI gates, and a customization-debt report** (§8).

Research that informed the design is in §10; open questions in §11.

---

## 1. Design principles

| # | Principle | Origin |
|---|---|---|
| P1 | **Lowest viable rung, narrowest viable scope.** Two independent minimisations, always both. | SAP Clean Core; ServiceNow OOTB-first |
| P2 | **The app declares its own ladder.** Rung availability is a property of the app, published in its `AppProfile`. Generic advice is useless; "Odoo supports L3 via addons, Collabora does not" is actionable. | Eclipse *declared* extension points |
| P3 | **Upstream first.** Any rung ≥ L4 carries an obligation to attempt the change upstream and to record the outcome. Carrying a downstream delta is a debt with a due date, not a decision. | Fedora "Upstream First"; Debian DEP-3 `Forwarded:` |
| P4 | **Every customization is a tracked artifact in git.** No rung of this ladder terminates in a live cluster. The existing absolute prohibition on cluster hotfixes is Rung X (§2.8). | gentian-apps `app-profile-guide.md` |
| P5 | **Descend over time.** Rungs are not permanent homes. Each record names exit criteria and a review date; the platform reports on aging debt. | Debian patch series shrink as patches land upstream |
| P6 | **Configuration layers, it does not fork.** Drop-in precedence is fixed and documented (image → chart → profile → tenant), like `/usr` → `/run` → `/etc`. | systemd drop-in precedence |
| P7 | **Extension APIs are versioned contracts.** An app that offers L3 owes plugin authors a stability policy, a deprecation window, and a "proposed API" lane for the unstable parts. | VS Code proposed API; Eclipse API freeze |
| P8 | **Customization inherits the trust model.** A customization can never raise its target's `trustTier` or bypass Kyverno, MAC waivers, licensing, or tenant isolation. | existing catalogue tiers, MAC waivers |

---

## 2. The ladder

Ordered by **how much of the app's own delivery artifact Gentian ends up owning** — which is the
thing that determines upgrade cost.

| Rung | Name | Debian/Linux analogue | Gentian owns | Survives upstream minor upgrade? |
|---|---|---|---|---|
| **L0** | **Configure** | edit a value in `/etc/foo.conf` | nothing (values only) | Yes |
| **L1** | **Drop-in** | `/etc/foo.conf.d/50-gentian.conf`, `systemctl edit` override | one config/asset file | Yes |
| **L2** | **Companion** | `apt install` a *new* program that talks to the old one | a separate deployable | Yes |
| **L3** | **Extension** | `apt install foo-plugin-bar` | an addon loaded by the app | Usually — bound to the app's plugin API |
| **L4** | **Repackage** | rebuild the *package* (build flags, conffiles, wrapper) — source untouched | the chart / composition / entrypoint | Often — bound to chart+image layout |
| **L5** | **Patch** | `debian/patches/series` over pristine upstream | a patch series + a rebuilt image | No — rebases every upstream release |
| **L6** | **Fork** | a derivative distribution with its own release train | the source tree | No — full maintenance, incl. CVE duty |
| **X** | **Hotfix** | *forbidden* | — | — |

> **Rule of thumb for the whole table:** the cost of a customization is not the cost of writing it.
> It is the cost of writing it *again* at every upstream release. L0–L3 you write once. L4 you
> re-check. L5 you rebase. L6 you own forever.

### 2.1 L0 — Configure

Change behaviour using knobs the app already exposes. No new files, no new code.

| Scope | Where | Mechanism |
|---|---|---|
| Tenant | `gentian-deployments` | `Tenant.spec.apps[].config.extraValues` (deep-merged over profile) |
| Profile | `gentian-apps/profiles/<n>/profile.yaml` | `spec.extraValues`, `spec.ingress`, `spec.portalTiles`, `spec.kernelRequirements` |

**Obligations:** none beyond normal review. **Test:** chart renders; app starts.

**Making L0 first-class:** every Gentian-owned chart should ship a `values.schema.json`. Today
`extraValues` is `PreserveUnknownFields` on both `AppProfile` and `TenantAppConfig` — a typo is
silently accepted and only fails at Helm render time. A published schema makes L0 machine-checkable
in CI, and lets an agent *discover* whether the change it wants is already an L0 knob.

### 2.2 L1 — Drop-in

Add a **file** into a directory the app already treats as an extension point: a theme, a policy
file, a locale bundle, an `xml`/`yaml` snippet, a logo, a config fragment. The app's binary and
its own config files are untouched.

**Mechanism:** the app's profile declares its drop-in directories (§4). The composition materialises
a ConfigMap/Secret and mounts it at that path; the chart mounts nothing app-specific.

**Precedence (fixed, systemd-style):**

```
image defaults          (lowest)
  → chart values.yaml
    → AppProfile.spec.extraValues
      → profile drop-ins (profiles/<n>/dropins/*)
        → Tenant.spec.apps[].config.extraValues
          → tenant drop-ins                       (highest)
```

Within a drop-in directory, files apply in lexicographic order — reserve `00-`–`49-` for platform,
`50-`–`89-` for profile, `90-`–`99-` for tenant.

**Obligations:** the drop-in path must be declared in `spec.customization.dropIns`. Undeclared
mounts into an upstream image are L4, not L1 — that distinction matters, because a declared
drop-in path is a contract the upstream project maintains and an undeclared one is not.

#### 2.2.1 Tenant-scoped drop-ins (S0 · L1)

L1 is the **highest rung a tenant admin may reach unaided**, and the only rung where self-service
makes sense: a tenant admin can supply a logo, a locale bundle, or a config fragment without a
catalogue PR, but cannot introduce code.

```yaml
# gentian-deployments — Tenant.spec.apps[]
- profile: odoo-cb-base
  config:
    extraValues: { }
    dropIns:
      - name: branding                # must match a declared spec.customization.dropIns[].name
        files:
          90-brand.css: |
            :root { --primary: #0b7285; }
```

**Delivery.** The operator reconciles `Tenant.spec.apps[].config.dropIns` into a ConfigMap
`app-<profile>-dropin-<name>` in the tenant namespace; the composition mounts it at the declared
path *after* the profile drop-in mount, so tenant files win by mount order and by the `90-`–`99-`
numeric prefix convention. No composition change is needed per app — the mount is generated from
`spec.customization.dropIns`, which keeps this generic (§3, platform boundary).

**Guardrails** — enforced by the operator and by admission:

| Rule | Why |
|---|---|
| `name` must match a declared `spec.customization.dropIns[].name` | tenants cannot invent mount paths — that would be L4 at tenant scope |
| Declared entry must set `tenantEditable: true` | not every drop-in dir is safe to expose (a policy file usually is not) |
| Filenames must match `^[9][0-9]-[a-zA-Z0-9._-]+$` | reserves the platform/profile ranges |
| Total size ≤ `maxBytes` (default 256Ki) | ConfigMap limits; DoS |
| `format` must match the declared format; content is parsed before mount | a malformed fragment must fail at admission, not crash the app at boot |
| No secret material — values land in a ConfigMap, in etcd, visible in the Admin Console | secrets go through `valueMapping`, always |

**Admin Console.** Surfaced as a per-app "Customization" tab for the tenant admin: declared
tenant-editable drop-ins, a validating editor, and the resulting diff. This is the self-service
front door that makes Rung X unattractive.

### 2.3 L2 — Companion (side-by-side)

Build **new code as a separate deployable** that talks to the target app only through its
*published* API. The target app is not modified in any way.

This is the SAP "side-by-side extensibility" rung and the Nextcloud **ExApp** rung, and in Gentian
it is already fully supported infrastructure: scaffold from `gentian-app-template`, publish an
`AppProfile`, and wire it to the target with a **contract + `IntegrationBinding`**.

```yaml
# gentian-apps/profiles/acme-approvals/profile.yaml
spec:
  optionalIntegrations:
    - contract: erp-core            # provided by odoo-cb-base
      provider: odoo-cb-base
      capabilities: [read, write]
```

**L2 vs L3 tie-breaker** — the one decision this ladder cannot make positionally:

| Choose **L2 Companion** when | Choose **L3 Extension** when |
|---|---|
| The function can stand alone (own URL, own portal tile, own data) | The function must appear *inside* the app's own UI, menus, or workflow |
| It needs its own scaling, language, or release cadence | It must extend the app's data model / ORM / permission model |
| The app's plugin API is absent, unstable, or undocumented | The app has a documented, versioned plugin API (`customization.extension.apiStability: stable`) |
| You need the change to survive a *major* upstream upgrade | Round-tripping through HTTP would be absurd for the semantics |

**Obligations:** a `Contract` must exist (or be added to `gentian-apps/contracts/`); auth via
`oidc-token-exchange`, never shared static credentials; the companion must degrade gracefully if
the target app is not installed.

### 2.4 L3 — Extension (in-app addon)

Use the app's **own extension system**. Odoo addons (`_inherit`, view `xpath`/`inherit_id`),
Nextcloud apps, XWiki extensions, Activepieces pieces, Keycloak SPIs, Collabora — none.

Gentian already has two delivery paths for this, and the framework should name them explicitly:

| Delivery | Mechanism | Use when |
|---|---|---|
| `git-sidecar` | `gentian-sidecar-git-modules` syncs a git repo into the app's addon path (`odoo` chart: `gentian.git.repo` → `gentian.modulesPath`) | addons iterate faster than the app image |
| `image-layer` | addons baked into a Gentian-built image layer at build time | reproducibility/airgap matters more than iteration speed |
| `addon-profile` | a thin `AppProfile` with `deployment-role: addon` declaring `spec.customization.addon.{id,of}`; the tenant selects it into a base via `Tenant.spec.apps[].addons` | the addon is a *catalogue-visible product* |
| `app-store-api` | the app's own runtime API installs the extension (Nextcloud `occ app:install`) via `spec.postInstallJob` | the app owns its own registry |

#### How an `addon-profile` is activated

The tenant selects **profile names**; the operator resolves them to whatever the hosting app
calls the thing (an Odoo module, a Nextcloud app id) through each addon's own
`spec.customization.addon`. Nothing in gentian-os knows those app-native names — putting that
knowledge in a reconciler would move an app fact into the platform, which is exactly the
boundary this framework exists to hold.

Activation itself has two shapes, and which one an app uses is a property of the app:

| Shape | Declared by | Used when |
|---|---|---|
| **Values** | `spec.customization.addonActivation` on the *base* profile: a Helm values path and a script, with `__GENTIAN_ADDON_IDS__` substituted | the chart exposes a hook that runs inside the app container (Nextcloud's `hooks.before-starting`). Preferred — no Job, and the script runs with the app's config, data volume and secrets already mounted |
| **Composition** | the app's own `composition.yaml` renders a Job | activation is not expressible as chart values. Odoo installs database-side via `odoo-bin -i`, so it needs a Job that reaches the database |

**Write the script to reconcile, not to add.** It re-runs on every pod start, which is what makes
a changed selection converge with no migration step — and it is the only way deselection can work
at all. A base that declares `addonActivation` has it rendered even when the selection is *empty*,
because "none" is a selection: skipping it there would drop the values key and leave the last
selection enabled forever.

Deselection is not uninstallation. Nextcloud's `occ app:disable` keeps the app's data, so
deselecting is reversible. Odoo's `-i` has no safe inverse — uninstalling a module drops its
tables — so an Odoo addon stops being activated but is not removed. Purging data is always a
separate, explicit path.

**Multi-tenancy is the sharp edge here.** An addon loaded into a shared runtime affects every
tenant on that runtime. The existing Odoo pattern is the right precedent — per-tenant addon sets
driven by group attributes (`gentianos.io/keycloak-group-attributes: {"gentianOdooModules":["crm"]}`)
rather than per-tenant addon *binaries*. The framework should make this explicit:

> **L3 rule (namespace test).** Per-tenant addons are allowed **only if the app instance runs in
> that tenant's own namespace** (`tenant-<name>`). If the instance lives in a shared namespace
> serving more than one tenant, addons are **profile-scoped only** — per-tenant behaviour must come
> from the addon reading tenant context at runtime, never from divergent addon sets.

The namespace, not the profile, is the test: "one instance per tenant" is an intention, but
"deployed into `tenant-acme`" is a fact the operator can check. Concretely, the operator rejects a
`Customization` with `rung: L3` and `scope: tenant` unless the target `App` claim for that tenant
resolves to a workload in the tenant's own namespace. The `odoo-cb-*` family passes (one Odoo per
tenant, `databasePerTenant: true`); a future shared-runtime app would not.

Sharing a runtime across tenants and then loading tenant-specific code into it is the single
fastest way to turn a customization into a cross-tenant data leak, which is why this is a hard
operator check and not a review-time convention.

**Obligations:** declare the addon repo + delivery in `spec.customization.extension`; pin the
addon version alongside `spec.chart.version`; a `Customization` record (§5) is **required** from
L2 upward; the addon must be tested against the pinned app version in CI.

### 2.5 L4 — Repackage

The upstream **source and image are unchanged**, but Gentian now owns the *packaging*: a
Gentian-authored Helm chart wrapping an upstream image, a `composition.yaml` with init containers
or bootstrap Jobs, an entrypoint wrapper, a `postInstallJob` that calls the app's admin API,
sidecar injection, or a Kustomize post-render over an upstream chart.

This is where most Gentian upstream apps already sit (`charts/odoo`, `charts/gentian-sidecar-*`,
per-profile `composition.yaml`).

**The L4 boundary test:** if you would have to change the file when upstream reorganises its chart
or its filesystem layout, it is L4. If upstream *promises* the path, it is L1.

**Prefer, in order:** (a) upstream chart + `extraValues`, (b) upstream chart + Kustomize
post-render patch, (c) Gentian wrapper chart with upstream as a dependency, (d) vendored chart
copy. (d) is a fork of the packaging and should be recorded as such (see `UPSTREAM.md` convention
already used for vendored charts).

**Obligations:** `Customization` record; a rendered-manifest golden test (`crossplane render` diff,
already in the catalogue CI plan); an `UPSTREAM.md` for any vendored chart; upstream-first check
recorded.

### 2.6 L5 — Patch

Gentian modifies **upstream source** and rebuilds the artifact. Modelled directly on Debian's
`3.0 (quilt)` source format: pristine upstream + an ordered, individually-documented patch series.

```
<build-repo>/
├── UPSTREAM              # upstream URL + exact pinned tag/commit
├── patches/
│   ├── series            # ordered list, applied top-down
│   ├── 0001-fix-oidc-logout.patch
│   └── 0002-add-tenant-header.patch
└── Dockerfile            # FROM pinned upstream; apply series; build
```

Every patch header **must** carry DEP-3 fields:

```
Description: Propagate X-Gentian-Tenant through the OIDC logout flow
Author: platform-iam@gentian.org
Origin: other, https://github.com/gentian-org/…
Bug-Upstream: https://github.com/<upstream>/issues/1234
Forwarded: https://github.com/<upstream>/pull/1235
Applied-Upstream: no
Last-Update: 2026-08-06
```

`Forwarded:` is not optional. `Forwarded: no` requires a written reason in the `Customization`
record; `Forwarded: not-needed` is only valid for Gentian-specific integration glue that upstream
would rightly refuse.

**Obligations:** platform `trustTier` only; two-person review; SBOM + signed image; the patch series
must be re-validated (rebased or dropped) at *every* upstream version bump, and CI must fail the
bump if `patches/series` does not apply cleanly; review date ≤ 6 months.

**Explicitly forbidden at L5 and L6** (restating the existing catalogue prohibition, because this
is the rung where the temptation lives): patches that bypass license-key validation, unlock
enterprise features, or crack terms of service. No `sed` over minified bundles, no SQL triggers
flipping `*_enabled` columns.

### 2.7 L6 — Fork

Gentian owns a source tree with its own release train — e.g. `gentian-org/ocb`. Justified when the
patch series has grown beyond rebaseability, when upstream is unmaintained, or when the divergence
is strategic rather than tactical.

**Obligations:** named owner and a bus-factor ≥ 2; documented rebase/merge cadence against upstream;
**own CVE monitoring and response** for the forked tree; an `UPSTREAM-COMPARISON.md` maintained
per release (the `upstream-rescue` repo already does this); explicit product sign-off, because a
fork is a product decision, not an engineering one; an exit strategy or an explicit "permanent
divergence" declaration.

### 2.8 Rung X — Cluster hotfix (forbidden)

`kubectl patch`, `kubectl exec` + edit, hand-created ConfigMaps shadowing image files, editing
Secrets to change behaviour. This is **not** the top of the ladder — it is off the ladder.
See the absolute prohibition in `gentian-apps/docs/app-profile-guide.md`. The framework's job is to
make Rung X unnecessary by ensuring there is always a *reachable* legitimate rung, and by making
L0/L1 fast enough that nobody is tempted.

### 2.9 Who may author a customization

The ladder is **authorship-neutral**: the rung is determined by what the change does, never by who
wrote it. A partner's Odoo addon and a Gentian addon are both L3 and carry identical obligations.

Today Gentian owns every roadmap and every repository on the ladder. The end state is that the
practices in this document become a published specification, and repo ownership is delegated to
whoever owns the app — suppliers maintaining their own addon registries, customers maintaining
their own tenant customizations. **v0.4 builds the model, not the process:** every record carries
its authorship and repo ownership from day one, so delegation later is a policy change rather than
a data migration.

```yaml
spec:
  origin:
    authorship: partner            # gentian | tenant | supplier | partner | community
    organisation: "Acme Integrators GmbH"
    contact: platform@acme.example
    repo: https://github.com/acme/gentian-odoo-modules
    repoOwnership: external        # gentian | external
    reviewedBy: platform-erp       # who at Gentian accepted it
    supportContract: none          # none | community | commercial
```

**What is deliberately deferred** — each is a design of its own, and none blocks v0.4:

| Deferred | Why it can wait |
|---|---|
| Signing and provenance for external artifacts | today every artifact is still built by Gentian CI |
| Sandboxing of third-party L3 addons | current addons run with the app's own privileges; changing that is an isolation project |
| Entitlement / commercial terms for paid customizations | interacts with the marketplace and revenue-split roadmap items |
| Review SLAs and a delegated maintainer role | needs the governance model in §8.1 to be operating first |
| Automated upstreaming of external contributions | needs the debt report (§8.3) to have real data |

**What v0.4 must not do** is bake in the assumption that Gentian is the author. Two concrete
consequences: `Customization.spec.owner` is a free-form owner reference rather than a Gentian team
enum, and the §8.1 approval matrix names *roles* (catalogue maintainer, platform team) rather than
Gentian individuals, so an external maintainer can hold a role later without a schema change.

---

## 3. The second axis — scope (blast radius)

Rung and scope are **independent**. Minimise both.

| Scope | Affects | Authored in | Approved by |
|---|---|---|---|
| **S0 · Tenant** | one tenant's install | `gentian-deployments` (`Tenant.spec.apps[].config`) | cluster admin |
| **S1 · Profile** | every tenant that installs this profile | `gentian-apps` (`profiles/<n>/`, `apps/<n>/`, addon repo) | catalogue maintainer |
| **S2 · Platform** | every tenant, every app | `gentian-os` (kernel, operator, compositions, policy) | platform team |

**The S2 gate is the existing platform boundary rule and does not change:** a customization may
enter `gentian-os` *only* if it is generic across apps. App-specific behaviour at S2 scope is
forbidden regardless of rung — no `case "myapp"` in a reconciler, ever. If many apps need the same
thing, extend the `AppProfile` contract generically; if one app needs it, it belongs in
`gentian-apps` at whatever rung fits.

**The cost matrix.** Cells are (rung × scope); the diagonal to the bottom-right is where platforms
die.

|  | S0 Tenant | S1 Profile | S2 Platform |
|---|---|---|---|
| **L0–L1** | routine | routine | needs generic justification |
| **L2–L3** | allowed if per-tenant runtime | **the target zone** | rarely correct |
| **L4** | discouraged — prefer S1 | recorded, reviewed | generic mechanisms only |
| **L5–L6** | **forbidden** — never fork for one tenant | product sign-off | product sign-off |

---

## 4. `AppProfile.spec.customization` — the per-app ladder declaration

This is the core new API surface, and it is what makes the framework *app-specific* rather than
generic advice. It is a **generic** block — it describes capabilities in app-neutral terms, so it
does not violate the "no per-app fields" rule.

```yaml
apiVersion: gentianos.io/v1alpha1
kind: AppProfile
metadata:
  name: odoo-cb-base
spec:
  customization:
    # Reachability grade — see §4.1. Derived, but pinned here for agents.
    grade: A
    # Rungs at which THIS app can be customized. L2 never appears here — see note below.
    supportedRungs: [L0, L1, L3, L4]
    ladderDocs: https://gentianos.io/docs/apps/odoo/customization

    configure:                             # L0
      valuesSchema: chart/values.schema.json
      hotReload: false                     # does a values change need a restart?

    dropIns:                               # L1
      - name: odoo-conf
        path: /etc/odoo/odoo.conf.d
        format: ini
        source: configMap
        upstreamDocumented: true           # false ⇒ this is really L4
      - name: branding
        path: /opt/odoo/web/static/branding
        format: files
        source: configMap

    extension:                             # L3
      mechanism: odoo-addon
      delivery: [git-sidecar, addon-profile]
      registry: https://github.com/gentian-org/odoo-modules
      addonPath: /opt/odoo/custom-addons
      apiStability: stable                 # stable | evolving | undocumented | none
      apiDocs: https://www.odoo.com/documentation/18.0/developer.html
      perTenantAddons: true               # one runtime per tenant ⇒ allowed
      testMatrix: ["18.0"]

    # NOT a rung of this app. This is the surface Odoo offers so that SOME OTHER app
    # can be built at L2 against it. Odoo itself is customized at L0/L1/L3/L4.
    publishes:
      apis:
        - contract: erp-core
          protocol: http-json
          spec: openapi
          path: /api/v2
          auth: oidc-token-exchange

    repackage:                             # L4
      chartOwnership: gentian-owned        # upstream | gentian-owned | vendored
      compositionRef: app-odoo

    patch:                                 # L5
      allowed: true
      buildRepo: https://github.com/gentian-org/ocb
      seriesPath: patches/series
      requiresApproval: platform-team

    fork:                                  # L6
      allowed: true
      repo: https://github.com/gentian-org/ocb
      upstream: https://github.com/odoo/odoo
      owner: platform-erp
      cveWatch: true
```

**Why L2 is never in `supportedRungs`.** Every other rung is a property of the app being
customized: Odoo either has a drop-in dir or it does not, an addon system or not, a patchable
build or not. **L2 is a property of the customization, not of the target.** A companion is a
*new* app; it is always buildable, because nothing stops you writing a service that talks to
Odoo's API — and if Odoo published no API at all, the companion could still be built against
its database or not integrate at all. So L2 is unconditionally available for every app at every
grade, which is exactly why grade C apps like Collabora fall through L1/L3 straight to L2 (§9).

What the target *does* contribute is how pleasant that companion will be to build, and that is
what `publishes.apis` records — it is descriptive metadata for whoever builds at L2, not a
declaration that Odoo is customized at L2. An agent reads `supportedRungs` to decide reachability
and reads `publishes` only after step 3 of §6 has already selected L2.

**Defaults when the block is absent:** `{grade: unknown, supportedRungs: [L0, L4]}` — i.e. an
uncharacterised app can only be configured or repackaged (L2 remains available per the above).
Agents must not infer more, and must raise a task to characterise the app.

### 4.1 Customization readiness grades

A Debian-package-style rating, computed by a CI rubric and shown in the App Store. This is the
answer to "the way to do it depends on the app".

| Grade | Meaning | Reachable rungs | Examples |
|---|---|---|---|
| **A** | Plugin API that is **documented and versioned**, plus declared drop-in dirs and a published API for companions | L0–L3 | Odoo, Nextcloud, XWiki, Keycloak, Activepieces |
| **B** | A plugin system exists, but it is **undocumented, unversioned, or ABI-unstable**. Config, drop-ins and a published API as well. | L0–L3, at the risk `extension.apiStability` records | Element/Synapse (`synapse-module`), OpenProject (`openproject-plugin`), LiteLLM (`litellm-callback`) |
| **C** | Config only; monolithic; **no** extension surface at all | L0, then L2 or L4 | Collabora, many appliance images |
| **D** | Anything beyond a value change requires touching source | L0, then L5/L6 | unmaintained or hostile upstreams |
| **?** | Not yet characterised | L0, L4 | new catalogue entries |

**Grading rubric** (each +1; **A** ≥ 7, **B** 5–6, **C** 3–4, **D** ≤ 2): documented config
reference · declared drop-in directories · documented plugin/addon API · plugin API versioned with
a deprecation policy · published HTTP API with a spec · upstream accepts patches (PR turnaround
< 90d) · plugin ABI survives minor releases · a test harness plugin authors can use.

**A and B differ in the quality of the plugin system, not its existence.** Three of the eight
criteria are about the plugin API — documented, versioned, ABI-stable — so an app with a real but
poorly-kept extension system loses those points and lands in B while still being extensible.
Reading B as "no plugin system" contradicts its own rubric, and would have forced Element,
OpenProject and LiteLLM to either be misgraded or to hide working extension mechanisms.

L3 therefore remains reachable at grade B. What changes is the warranty, and that is what
`extension.apiStability` is for: `stable` at grade A, `evolving` or `undocumented` at B. An agent
choosing L3 against an `undocumented` API is choosing to re-test it on every upstream bump, which
is a decision the record must justify — not something the grade should silently forbid.

Only at **C** is L3 genuinely unreachable, because there is nothing to extend.

**Assignment is manual for v0.4.** The catalogue maintainer scores the rubric by hand, records the
score in `customization.md`, and sets `spec.customization.grade`. Several criteria — "upstream
accepts patches", "ABI survives minor releases" — are judgements about a community, not facts a
script can read. Automating the mechanical subset is roadmap item **2.13**; until then CI only
checks that a grade is *present* and that the recorded score matches the banding.

Publishing the grade does two things: it sets expectations *before* a customization is requested,
and it creates pressure on the catalogue to prefer Grade A apps — the same pressure Debian applies
by making well-behaved upstreams cheap to package.

---

## 5. The `Customization` record

**Required for every customization at L2 and above.** Written **before** the code. Modelled on
DEP-3 patch headers, generalised to the whole ladder.

**`Customization` is a namespaced CRD** (decision §11.1). Records are authored in git — beside the
artifact they describe, in `gentian-apps/profiles/<n>/customizations/<name>.yaml` or
`gentian-deployments/tenants/<t>/customizations/<name>.yaml` — and synced to the cluster like any
other catalogue object. Being a cluster object buys three things a file cannot:

* the **Admin Console reads live records** (§8.3) instead of a CI-generated snapshot;
* **admission enforces the §3 cost matrix** — an `L5` record at `scope: tenant` is rejected, and the
  §2.4 namespace test runs against the real `App` claim;
* `status` carries **derived state** — `reviewOverdue`, `upstreamStale`, `targetVersionDrift` — so
  the debt report is computed by the operator, not by a script guessing from YAML.

`AppProfile` and `Composition` are cluster-scoped — there is no per-profile namespace. Profile-scoped
records therefore land in the **fixed system namespace the `gentian-catalogue` ApplicationSet syncs
every profile's namespaced objects into** (`gentian-system` on this deployment layout — check the
ApplicationSet's `template.spec.destination.namespace`, since it is a cluster-wide constant, not
derived from the profile name). Tenant-scoped records land in `tenant-<name>`.

```yaml
apiVersion: gentianos.io/v1alpha1
kind: Customization
metadata:
  name: acme-invoice-approval
  namespace: tenant-acme        # tenant-scoped example; profile-scoped records use the
                                 # catalogue's fixed system namespace instead (see above)
spec:
  # WHAT
  summary: "Two-stage approval on vendor invoices above 10k"
  target:
    family: odoo
    profile: odoo-cb-base
    appVersion: "18.0"
    chartVersion: "0.1.13"

  # WHERE ON THE LADDER
  rung: L3
  scope: profile             # tenant | profile | platform
  tenants: []                # required and non-empty iff scope == tenant

  # WHY NOT LOWER — mandatory, one line per rung skipped
  rungJustification:
    L0: "no configuration knob for approval thresholds"
    L1: "approval logic is behaviour, not config"
    L2: "must appear inside the Odoo purchase workflow and extend account.move"

  # UPSTREAM-FIRST (P3) — mandatory for rung >= L4, recommended below
  upstreamFirst:
    attempted: true
    forwarded: not-needed
    reason: "Gentian-specific tenant policy; upstream would rightly decline"

  # ARTIFACTS
  artifacts:
    - repo: gentian-org/odoo-modules
      path: gentian_invoice_approval
      version: "1.2.0"
  delivery: git-sidecar

  # AUTHORSHIP (§2.9) — carried from day one so delegation is a policy change, not a migration
  origin:
    authorship: gentian        # gentian | tenant | supplier | partner | community
    repoOwnership: gentian     # gentian | external
    supportContract: none

  # LIFECYCLE (P5)
  owner: platform-erp          # free-form owner ref, not a Gentian team enum (§2.9)
  created: 2026-08-06
  reviewBy: 2027-02-06
  exitCriteria: "drop when Odoo ships native multi-stage PO approval"
  upgradeRisk: medium
  testedAgainst: ["odoo 18.0"]
  tests:
    - gentian-apps/apps/../tests/test_invoice_approval.py

  # SAFETY (P8)
  security:
    macWaivers: []
    newEgress: []
    handlesPersonalData: true
  licensing:
    effect: none             # none | adds-dependency | changes-terms
```

**Why a record at all:** it is the only thing that makes P5 (descend over time) enforceable. A
patch series without `Forwarded:` is how Debian derivatives accumulate hundreds of unowned deltas;
a customization without `exitCriteria` is how a ServiceNow instance becomes unupgradeable.

---

## 6. Decision procedure (for agents and humans)

Deterministic. An agent asked to "add function F to app A" **must** execute this in order.

```
INPUT: capability request F, target app A, requesting scope S_req

1. RESTATE
   Express F as a capability ("users must approve invoices > 10k"),
   not as an implementation ("patch account_move.py").
   If F is actually two changes, split and run this procedure per change.

2. LOAD THE APP'S LADDER
   Read AppProfile(A).spec.customization.
   If absent → assume {grade: "?", supportedRungs: [L0, L4]}
              and emit a task "characterise customization surface of A".

3. WALK THE RUNGS  L0 → L1 → L2 → L3 → L4 → L5 → L6
   For each rung R:
     a. CAN R express F?              (semantics — see §2 rung definitions)
     b. Is R in supportedRungs?       (or L2, always available)
     c. Is R permitted at scope S_req? (§3 cost matrix)
   Stop at the FIRST R where all three hold. That is the answer.
   Record a one-line reason for every rung skipped → spec.rungJustification.

4. TIE-BREAK L2 vs L3
   If both are viable, apply the §2.3 table. Default to L2 when
   extension.apiStability is not "stable".

5. GATE
   If R >= L4:
     - search upstream for an existing feature/issue/PR; record findings
     - STOP and request human approval — do NOT proceed autonomously
   If R >= L5:
     - additionally require platform trustTier and named owner
   If R == X (cluster hotfix):
     - refuse; this is never a valid outcome

6. MINIMISE SCOPE
   Choose the narrowest scope that satisfies the request, independent of R.
   Never L5/L6 at tenant scope.

7. RECORD BEFORE CODE
   Write the Customization manifest (§5). It is the design review.

8. EMIT INTO THE OWNING REPO  (§6.1) — never into a live cluster.

9. TEST
   Add the regression test this rung requires (§8.2).

10. REPORT
   State: chosen rung, scope, rungs skipped and why, upgrade risk,
   review date, and what will break this at the next upstream release.
```

### 6.1 Rung → repository map

The single table an agent needs to know *where to type*.

| Rung | Scope | Repository | Artifact |
|---|---|---|---|
| L0 | tenant | `gentian-deployments` | `Tenant.spec.apps[].config.extraValues` |
| L0 | profile | `gentian-apps` | `profiles/<n>/profile.yaml` → `spec.extraValues` |
| L1 | profile | `gentian-apps` | `profiles/<n>/dropins/` + composition ConfigMap |
| L1 | tenant | `gentian-deployments` | `Tenant.spec.apps[].config.dropIns` (§2.2.1) — or the Admin Console customization tab |
| L2 | profile | `gentian-apps` | `apps/<new>/` (from template) + `contracts/<c>.yaml` + `profiles/<new>/` |
| L3 | profile | addon repo (`odoo-modules`, …) | addon + pinned version in profile |
| L4 | profile | `gentian-apps` | `charts/<app>/`, `profiles/<n>/composition.yaml`, `spec.postInstallJob` |
| L5 | profile | build repo (`ocb`, …) | `patches/series` + DEP-3 headers + Dockerfile |
| L6 | profile | fork repo | vendored source, `UPSTREAM-COMPARISON.md` |
| any | platform | `gentian-os` | **only** if generic for all apps (§3) |

Note `profiles/<n>/` is a *bundle*, not a fixed depth: singletons sit at
`profiles/xwiki/`, members of a multi-profile family at
`profiles/odoo/odoo-cb-crm/`. Locate a bundle by its leaf directory name, which
CI requires to equal the `AppProfile`'s `metadata.name` — never by counting path
segments.

### 6.2 Why the artifact for each rung lives where it does

The rung → repository map is not arbitrary; it falls out of `gentian-apps` being
a **distribution repo** rather than an application monorepo (the full argument is
in [gentian-apps/docs/app-profile-guide.md](https://github.com/gentian-org/gentian-apps/blob/main/docs/app-profile-guide.md) §0).
Three consequences bind this framework directly:

**Artifact types live in separate flat trees, so a rung maps to a tree.** A
profile references its chart by OCI coordinate + version, never by a path
relative to itself, and `charts/odoo` backs 10 different profiles. So L0/L1
(metadata and content) land in `profiles/`, while L4 (packaging) lands in
`charts/` — different rungs, different trees, because they are consumed by
different pipelines and versioned independently. Colocating a chart inside the
profile that happens to use it would imply a 1:1 ownership that mostly does not
exist.

**Placement never encodes a mutable fact.** A tempting alternative is to put an
artifact wherever its consumers' nearest common ancestor is — shared things high,
specific things low. That was rejected: it makes a directory's location depend on
*how many things currently use it*, so a second consumer forces a physical move.
The customization ladder has the same property and resolves it the same way:
`scope` (tenant/profile/platform) is a **field on the record**, not a directory
level, exactly as `spec.tier` is a field rather than a `free/`–`pro/` split.
Anything that changes over time belongs in a field, where it can be queried and
validated; only stable identity belongs in a path.

**The repo versions packaging, not build output.** This is why an L5 record
points at a `patches/series` (the delta) rather than a vendored copy of upstream,
and why a rung-L6 fork must still carry `UPSTREAM` pinning plus DEP-3 headers.
Debian commits `debian/patches/`, not `.deb` files; Gentoo commits an ebuild that
*fetches* source rather than embedding it. A vendored chart or a checked-in
`.tgz` is the same category error, which is why `charts/packages/` was deleted
and why `chartOwnership: vendored` is a signal to review rather than a normal
state.

---

## 7. Best practices, templated in `gentian-app-template`

Requirement (2): the practices should not be prose — they should be *scaffolding*, so that every
first-party Gentian app is born at **Grade A** and never forces a consumer to L5.

### 7.1 New directory: `customization/`

```
gentian-app-template/
└── customization/
    ├── README.md              # the ladder, filled in for THIS app; the doc consumers read
    ├── profile-block.yaml     # spec.customization block, ready to paste into the AppProfile
    ├── dropins/
    │   ├── README.md          # declared paths + precedence + numbering convention
    │   └── 50-example.yaml
    ├── extensions/
    │   ├── README.md          # how to write a plugin for this app
    │   └── example_plugin/    # a working, tested reference plugin
    ├── customizations/        # Customization records for deltas this app itself carries
    └── patches/
        ├── README.md          # DEP-3 header requirements; when this dir is legitimate
        └── series             # empty by default — a non-empty series is a debt signal
```

### 7.2 Backend — a real extension point

Ship a loader, not a promise. Python entry points give a plugin system that works with the existing
image-layer and sidecar delivery models:

```python
# backend/app/extensions/api.py  — the versioned public contract (P7)
EXTENSION_API_VERSION = "1.0"          # semver; N-2 support; deprecations announced one minor ahead

class GentianExtension(Protocol):
    api_version: str
    def register_routes(self, router: APIRouter) -> None: ...
    def register_settings(self) -> dict: ...
    def on_event(self, event: str, payload: dict) -> None: ...

# backend/app/extensions/loader.py
#   discovers entry_points(group="gentian.app.<app-id>.plugins"),
#   refuses plugins whose api_version is outside the supported range,
#   logs the loaded set at startup and exposes it at /api/v1/extensions
```

Plus a `/etc/gentian/<app>/conf.d/*.yaml` drop-in reader in `core/config.py` implementing the P6
precedence chain, so L1 works out of the box.

### 7.3 Frontend — extension slots

```tsx
// frontend/src/extensions/Slot.tsx
<ExtensionSlot name="dashboard.widgets" context={{ tenant }} />
```

Named slots + a manifest-driven dynamic import of `/extensions/*.js`, so a companion or addon can
contribute UI without a rebuild. Slot names are part of the public API and follow the same
versioning policy.

### 7.4 Chart

* `values.schema.json` — makes L0 checkable and discoverable.
* `extraObjects`, `extraEnv`, `extraVolumeMounts`, `podAnnotations` — the standard L4-lite hooks, so
  packaging changes rarely need a chart fork.
* A declared `conf.d` mount wired to a ConfigMap the composition can populate — L1 with no chart edit.

### 7.5 Docs the template must ship

* `customization/README.md` — the app's own ladder, generated from `profile-block.yaml`.
* An `AGENTS.md` section: "Before adding a feature to an *installed* app, run the §6 procedure."
* A **deprecation policy** statement for the extension API (N-2, one-minor notice, a `proposed/`
  namespace for unstable surface — VS Code's model).

### 7.6 Retrofit for upstream apps

Upstream apps cannot be given a `customization/` directory, but their **profile bundle** can:

```
gentian-apps/profiles/<n>/
├── profile.yaml            # + spec.customization
├── customization.md        # the app's ladder, written by the catalogue maintainer
├── dropins/
└── customizations/         # Customization records
```

Populating `spec.customization` for the existing catalogue — the families **odoo**, **nextcloud**,
**xwiki**, **element**, **openproject**, **activepieces**, **litellm**, plus the first-party
**app-store** and **gentian-subscriptions** — is the highest-value first implementation step: it is
pure documentation work that immediately makes the agent procedure executable. Grades are recorded
per family in `profiles/<n>/customization.md`; addon profiles (`odoo-cb-*`, `nextcloud-office*`)
inherit their base profile's declaration rather than repeating it.

---

## 8. Governance, CI, and debt

### 8.1 Approval matrix

| Rung | Scope | Reviewer | Extra gate |
|---|---|---|---|
| L0–L1 | S0/S1 | normal PR review | schema validation |
| L2 | S1 | catalogue maintainer | contract review; new profile |
| L3 | S1 | catalogue maintainer | addon CI against pinned app version |
| L4 | S1 | catalogue maintainer + platform | render-golden diff; `UPSTREAM.md` if vendored |
| L5 | S1 | **platform team, 2 reviewers** | DEP-3 complete; `Forwarded:` set; SBOM; signed image |
| L6 | S1 | **platform + product sign-off** | named owner, CVE watch, comparison doc |
| any | S2 | platform team | must be generic across apps |

### 8.2 CI obligations per rung

| Rung | CI must verify |
|---|---|
| L0 | values validate against `values.schema.json`; chart renders |
| L1 | drop-in path is declared in `spec.customization.dropIns`; precedence test |
| L2 | contract exists; companion tolerates target-app absence; token-exchange auth |
| L3 | addon builds against every version in `extension.testMatrix`; addon version pinned |
| L4 | `crossplane render` golden diff reviewed; no plaintext secrets in values |
| L5 | `patches/series` applies cleanly to the pinned upstream tag; **fails the version bump if not**; every patch has DEP-3 `Forwarded:` |
| L6 | fork builds; CVE scan; `UPSTREAM-COMPARISON.md` regenerated |
| all ≥L2 | a `Customization` record exists, parses, and has `reviewBy` in the future |

Additionally, admission rejects (not merely warns):

| Rejected | Rule |
|---|---|
| `rung: L5\|L6` with `scope: tenant` | §3 cost matrix |
| `rung: L3` + `scope: tenant` where the app does not run in that tenant's namespace | §2.4 namespace test |
| `rung >= L4` with `upstreamFirst.attempted: false` | P3 |
| tenant drop-in naming an undeclared or non-`tenantEditable` entry | §2.2.1 |
| a record whose `target.profile` does not resolve to an `AppProfile` | dangling debt |

### 8.3 The customization debt report

The operator computes `Customization.status` on every reconcile (`reviewOverdue`, `upstreamStale`,
`targetVersionDrift`, `rungAboveRecommended`); the Admin Console aggregates live records
alongside the existing platform/security views:

* count of records by rung × scope, trended over time — **the number that must go down**
* records past `reviewBy`
* records with `upstreamFirst.forwarded: no` and no reason
* patch series length per forked component
* apps whose grade is `?`
* **upgrade blast radius**: for a proposed `chart.version` bump, which records claim
  `testedAgainst` values that exclude the new version

This is the artifact that turns the ladder from advice into a managed liability — the thing
ServiceNow and SAP shops build after the damage is done, and that Gentian can build before.

---

## 9. Worked examples

| Request | Target | Chosen | Why not lower | Where |
|---|---|---|---|---|
| "Our brand colours in the ERP" | Odoo (A) | **L1** | no L0 knob for asset files | `profiles/odoo-cb-base/dropins/50-branding/` |
| "Approval workflow on invoices" | Odoo (A) | **L3** | must extend `account.move` and the purchase UI | `odoo-modules/gentian_invoice_approval` |
| "Dashboard combining ERP + project data" | Odoo + OpenProject | **L2** | stands alone; two targets; neither should own it | new `gentian-apps/apps/insights` + contracts |
| "Raise Collabora's document size limit" | Collabora (C) | **L0** | it is a documented value | `spec.extraValues` |
| "Custom Collabora save hook" | Collabora (C) | **L2** | grade C — no L1/L3 surface exists | companion service on the WOPI contract |
| "Tenant-specific SMTP sender name" | any | **L0/S0** | pure value, one tenant | `Tenant.spec.apps[].config.extraValues` |
| "Propagate tenant header through OIDC logout" | upstream app (D) | **L5** | no extension point; must change request handling | build repo `patches/`, DEP-3, forwarded upstream |
| "Odoo without the enterprise-addon nag" | Odoo | **L3** | already solved as an addon (`hide_enterprise_modules`) — **never** L5 | `odoo-modules/` |

---

## 10. Prior art consulted

| Source | What we took |
|---|---|
| **Debian `3.0 (quilt)` + quilt patch series** | L5 shape: pristine upstream + ordered, individually-documented series; series must apply cleanly or the bump fails |
| **Debian DEP-3 patch tagging** | The `Customization` record's `Origin` / `Bug-Upstream` / `Forwarded` / `Applied-Upstream` / `Last-Update` fields |
| **Fedora "Upstream First"** | P3, and the framing of a downstream delta as a *maintenance liability with a due date*, not a decision |
| **systemd drop-ins** (`/usr` → `/run` → `/etc`, lexicographic `*.conf.d`) | L1 precedence chain and the numeric prefix convention |
| **`dpkg` conffiles / `dpkg-divert` / `update-alternatives`** | The idea that "you now own this file" is a discrete, recorded event |
| **SAP Clean Core (levels A–D; key-user → side-by-side → developer extensibility)** | Two-axis thinking, the grade concept, and "exhaust the cheap tiers before the expensive ones" |
| **ServiceNow configuration-vs-customization + upgrade governance** | The debt report, review boards, and the empirical claim that customization-heavy instances take months to upgrade |
| **Odoo module inheritance** (`_inherit`, view `inherit_id` + `xpath`) | The canonical L3 model: patch behaviour without copying the original |
| **Nextcloud apps + AppAPI ExApps** | Direct evidence that L2 and L3 are *both* first-class and distinct — ExApps are containerised side-by-side extensions |
| **Kustomize overlays / Helm post-renderers** | L4 preference order; "customize without forking the chart" |
| **VS Code proposed API; Eclipse declared extension points & API freeze** | P7: extension APIs are versioned contracts with a deprecation window and an explicit unstable lane |
| **Kubernetes/OTel API stability & N-2 deprecation** | The concrete deprecation policy the template's extension API adopts |

Sources: [Debian maint-guide ch.3](https://www.debian.org/doc/maint-guide/modify.en.html) ·
[quilt for Debian maintainers](https://perl-team.pages.debian.net/howto/quilt.html) ·
[DebSrc3.0](https://wiki.debian.org/Projects/DebSrc3.0) ·
[Fedora Upstream First](https://docs.fedoraproject.org/hu/project/upstream-first/) ·
[Red Hat: what is an upstream](https://www.redhat.com/en/blog/what-open-source-upstream) ·
[systemd-system.conf(5)](https://www.man7.org/linux/man-pages/man5/systemd-system.conf.5.html) ·
[SAP Clean Core extensibility levels](https://community.sap.com/t5/technology-blog-posts-by-sap/clean-core-maturity-and-the-new-extensibility-levels/ba-p/14293974) ·
[SAP clean core best practices](https://learning.sap.com/courses/practicing-clean-core-extensibility-for-sap-s-4hana-cloud/explaining-extensibility-model-best-practices_e290f382-800e-40ef-a203-85a13115f487) ·
[ServiceNow configuration vs customization](https://www.servicenow.com/community/developer-articles/servicenow-configuration-vs-customization/ta-p/2415251) ·
[ServiceNow upgrade governance](https://www.servicenow.com/community/developer-blog/servicenow-upgrade-governance/ba-p/3506029) ·
[Odoo: fork or addon?](https://www.odoo.com/forum/help-1/is-it-best-to-fork-odoo-or-make-an-appmodule-96128) ·
[Nextcloud AppAPI](https://github.com/nextcloud/app_api) ·
[Nextcloud patching guide](https://docs.nextcloud.com/server/latest/admin_manual/issues/applying_patch.html) ·
[Helm post-renderer + Kustomize](https://gist.github.com/neoakris/edc0642a088be2cdc4f5ffe8d90ef5ca) ·
[Eclipse extensions & extension points](https://help.eclipse.org/latest/topic/org.eclipse.pde.doc.user/concepts/extension.htm) ·
[VS Code proposed API](https://code.visualstudio.com/api/advanced-topics/using-proposed-api) ·
[OpenTelemetry versioning & stability](https://opentelemetry.io/docs/specs/otel/versioning-and-stability/)

---

## 11. Resolved decisions

Decided 2026-08-06. Each decision is implemented in the step named in §12.

| # | Question | **Decision** | Consequence |
|---|---|---|---|
| 1 | `Customization` as CRD or plain YAML? | **CRD** (`gentianos.io/v1alpha1`, namespaced) | Admin Console reads records live; admission can enforce the §3 cost matrix; §5 records are cluster objects, not just files |
| 2 | Is `spec.customization` reference data or contract? | **CRD block on `AppProfile`** | Machine-readable for agents and the App Store; §6 step 2 is executable |
| 3 | Tenant-scoped L1 drop-ins | **Build them**, tenant-admin configurable | New `Tenant.spec.apps[].config.dropIns` + operator-rendered ConfigMap; see §2.2.1 |
| 4 | Per-tenant L3 on shared runtimes | **Forbidden**, and stated in namespace terms | Per-tenant addons require the app to run in the tenant's own namespace; see §2.4 |
| 5 | Third-party customization (tenants, suppliers, customers) | **In scope for the model, not yet for the process** | `Customization.spec.origin` carries authorship and repo ownership from day one; delegation processes deferred; see §2.9 |
| 6 | Grade computation | **Manual now, CI later** | Maintainer-assigned in `spec.customization.grade`; automated rubric is roadmap item 2.13 |
| 7 | Milestone | **v0.4** | Roadmap entries written against v0.4 |

---

## 12. Implementation order and status

| Step | Deliverable | Repo | Status |
|---|---|---|---|
| 1 | This document reviewed and agreed | `gentian-os` | **done** |
| 2 | `customization.md` + grades for the existing catalogue apps | `gentian-apps` | **done** |
| 3 | `spec.customization` on the `AppProfile` CRD + populated for those apps | `gentian-os`, `gentian-apps` | **done** |
| 4 | §6 procedure added to all `AGENTS.md` files | all | **done** |
| 5 | `Customization` CRD + CI validator + debt report generator | `gentian-os`, `gentian-apps` | **done** |
| 6 | `customization/` scaffolding + extension loader + slots in the template | `gentian-app-template` | **done** |
| 7 | `values.schema.json` for Gentian-owned charts | `gentian-apps` | **done** |
| 8 | L5 discipline retrofitted to `ocb` (DEP-3 headers, `series`, CI bump gate) | `ocb` | **done** |
| 9 | Debt report surfaced in Admin Console | `gentian-ui` | **done** |
| 10 | Tenant drop-in reconciler + Admin Console editor (§2.2.1) | `gentian-os`, `gentian-ui` | **done** |
| 11 | L3 unified on one addon model: `addon-profile` delivery, addon resolver, activation, selection window (`gentian-apps/docs/L3-cleanup.md`) | `gentian-os`, `gentian-apps` | **done** |
| — | Automated grade rubric in CI | `gentian-apps` | roadmap 2.13 |
| — | Third-party delegation process (signing, entitlement, review SLAs) | — | deferred, §2.9 |
