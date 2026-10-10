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
3. **A per-app capability declaration** — `ComponentProfile.spec.customization` (§4) — so the ladder is
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
| P2 | **The app declares its own ladder.** Rung availability is a property of the app, published in its `ComponentProfile`. Generic advice is useless; "Odoo supports L3 via addons, Collabora does not" is actionable. | Eclipse *declared* extension points |
| P3 | **Upstream first.** Any rung ≥ L4 carries an obligation to attempt the change upstream and to record the outcome. Carrying a downstream delta is a debt with a due date, not a decision. | Fedora "Upstream First"; Debian DEP-3 `Forwarded:` |
| P4 | **Every customization is a tracked artifact in git.** No rung of this ladder terminates in a live cluster. The existing absolute prohibition on cluster hotfixes is Rung X (§2.8). | gentian-apps `app-profile-guide.md` |
| P5 | **Descend over time.** Rungs are not permanent homes. Each record names exit criteria and a review date; the platform reports on aging debt. | Debian patch series shrink as patches land upstream |
| P6 | **Configuration layers, it does not fork.** Drop-in precedence is fixed and documented (image → chart → profile → tenant), like `/usr` → `/run` → `/etc`. | systemd drop-in precedence |
| P7 | **Extension APIs are versioned contracts.** An app that offers L3 owes plugin authors a stability policy, a deprecation window, and a "proposed API" lane for the unstable parts. | VS Code proposed API; Eclipse API freeze |
| P8 | **Customization inherits the trust model.** A customization can never raise its target's `trustTier` or bypass Kyverno, MAC waivers, licensing, or tenant isolation. | existing catalogue tiers, MAC waivers |
| P9 | **An app declares needs, never endpoints.** A profile says *that* it sends mail, uses a database, needs object storage — never where those are or what credentials reach them. Hosts, ports, users and passwords arrive through `valueMapping`, as references the app does not resolve. | this section |

---

### 1.1 Declaring a need — P9 in practice

Two fields, and neither names the cluster:

```yaml
spec:
  requires:
    services:
      mail:
        smtp: {}                    # the need. It carries nothing.
  package:
    valueMapping:
      smtp:
        hostKey: mail.smtp.host     # what THIS chart calls these values
        portKey: mail.smtp.port
        userKey: mail.smtp.name
        passwordKey: mail.smtp.password
```

The requirement is empty on purpose. It once carried `auth` and `port`, which
asked the wrong party: the mechanism a server accepts and the port it listens on
belong to the platform, and an app asserting `587, plain` asserts something it
cannot verify and would be wrong about the moment the cluster changed.

`valueMapping` is not cluster knowledge either — it is the app describing its
own chart. Nextcloud calls it `nextcloud.mail.smtp.host`, OpenProject
`environment.OPENPROJECT_SMTP__ADDRESS`. Only the packager knows that, and it is
the whole of what they must supply. The values then reach the chart as
`secretKeyRef` entries into a Secret the app never names, so a cluster can move
from in-cluster Postfix to a relay and no profile changes.

The same shape covers `database`, `cache`, `s3`, `identity` and `imap`.

**Opening mailboxes with the sign-in token is said separately.** `imap: {}`
gives the app the mail server's address and nothing more. An app that signs
people in to their mailboxes with their own token (XOAUTH2) declares it:

```yaml
spec:
  requires:
    services:
      identity:
        oidc: {clientId: gentian-webmail}   # the client the grant is given to
      mail:
        imap:
          tokenSignIn: true
```

The app's client is given the optional scope `mailbox`. The app asks for it
when it signs a person in for mail (`scope=openid email mailbox`), and that
token -- no other -- is accepted by the mail server, for that person's mailbox
only. Without the declaration Keycloak refuses the scope. It is served where
the cluster runs its own mail server and the tenant has mailboxes on it;
elsewhere it is accepted and does nothing, so an app keeps a fallback (an app
password) for those clusters. Who checks what:
[security.md §2.16](design/security.md).

**Language models are a need like the others.** An app that calls models
declares the platform's model gateway, and only an app that declares it is
given a key there, the gateway's address and a network path to it:

```yaml
spec:
  requires:
    services:
      llm: {}                       # the need. It carries nothing.
  package:
    valueMapping:
      llm:
        baseUrlKey: openai.baseUrl  # what THIS chart calls these values
        apiKeyKey: openai.apiKey
```

The gateway speaks the OpenAI API; the address ends in `/v1`. The key is the
app's own, generated per tenant and app and kept in the vault. A chart that
reads its configuration from the environment can take the Secret
`llm-credentials-<app>` whole instead of mapping keys (`OPENAI_API_BASE`,
`OPENAI_API_BASE_URL`, `OPENAI_API_KEY`); a post-install job gets
`LLM_BASE_URL` and `LLM_API_KEY` for the keys the profile maps. The path
opened is the gateway's port and nothing else in its namespace, so a profile
that declares `llm` does not name `system-llm` in
`gentianos.io/kernel-egress-namespaces`. On a cluster that serves no models
the app is not installed: its Component waits and says the cluster has no
model gateway. An app that keeps the key somewhere of its own on first start
has to take it again when it changes -- the platform replaces a key at the
gateway, it cannot reach into an app's database.

A component the platform places on every tenant itself (`defaultForTenants`
and its siblings: the desktop) may declare the same need, at `trustTier:
platform` only. It gets a key of its own per tenant under the same names, and
its chart is told the Secret's *name*, never the key:

```yaml
spec:
  requires:
    services:
      llm:
        optional: true              # run without the gateway where there is none
  package:
    valueMapping:
      llm:
        availableKey: llm.available         # true once the key is delivered
        baseUrlKey: llm.baseUrl
        secretNameKey: llm.apiKeySecretName # llm-credentials-<component>; key OPENAI_API_KEY
```

`optional` is for such a component: while the cluster serves no models, or the
key is not delivered yet, it is released without credentials and without a
path to the gateway, `availableKey` receives `false`, and it is rendered again
when the key arrives. For an app a tenant installs the requirement holds the
release whether or not `optional` is set. Nobody uninstalls a placed
component, so its key goes when the tenant is deleted with its data, when the
profile stops declaring `llm`, or when the platform takes the component away.

**Never substitute an endpoint into `extraValues`.** `${SMTP_HOST}`,
`${S3_ENDPOINT}`, `${MYSQL_HOST}` and `${REDIS_HOST}` handed the app a literal
hostname and no credential — an app wired that way can only attempt
unauthenticated access, and it hard-codes an assumption about where the service
lives. They no longer exist: the compositions stopped substituting them and no
profile uses them, so a placeholder written today reaches the cluster verbatim.
The identity placeholders (`${TENANT_ID}`, `${TENANT_DOMAIN}`,
`${TENANT_NAMESPACE}`, `${KERNEL_DOMAIN}`, `${NODE_IP}`) remain — those describe
who the tenant is, not where a service lives.

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
| Tenant | the tenant's manifest in the deployments repository | `Tenant.spec.apps[].config.extraValues` (deep-merged over profile) |
| Profile | `gentian-apps/profiles/<n>/profile.yaml` | `spec.package.extraValues`, `spec.expose` (with its tiles), `spec.requires.services` |

The director's install route writes an app's profile, digest, catalogue, add-ons and default
grant, and nothing under `config`: a tenant-scoped value is an edit of the tenant's manifest that
no route of the director makes today.

**Obligations:** none beyond normal review. **Test:** chart renders; app starts.

**Making L0 first-class:** every Gentian-owned chart should ship a `values.schema.json`. Today
`extraValues` is `PreserveUnknownFields` on both `ComponentProfile` (`spec.package.extraValues`)
and `TenantAppConfig` — a typo is
silently accepted and only fails at Helm render time. A published schema makes L0 machine-checkable
in CI, and lets an agent *discover* whether the change it wants is already an L0 knob.

### 2.2 L1 — Drop-in

Add a **file** into a directory the app already treats as an extension point: a theme, a policy
file, a locale bundle, an `xml`/`yaml` snippet, a logo, a config fragment. The app's binary and
its own config files are untouched.

**Mechanism:** the app's profile declares its drop-in directories (§4), and the app's chart mounts
a ConfigMap or Secret at each declared path. The platform's app Composition generates no mount:
the declaration is what the platform checks tenant content against (§2.2.1), and the chart's
values name what is mounted.

**Precedence (fixed, systemd-style):**

```
image defaults          (lowest)
  → chart values.yaml
    → ComponentProfile.spec.package.extraValues
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
# the tenant's manifest — Tenant.spec.apps[]
- profile: odoo-cb-base
  config:
    extraValues: { }
    dropIns:
      - name: branding                # must match a declared spec.customization.dropIns[].name
        files:
          90-brand.css: |
            :root { --primary: #0b7285; }
```

**Delivery: a tenant's drop-in has no effect on the app yet.** The operator reconciles
`Tenant.spec.apps[].config.dropIns` into a ConfigMap `app-dropin-<profile>-<name>` in the tenant
namespace (`internal/controller/dropin_reconciler.go`) and removes it when the entry goes. That is
where it ends: the platform generates no mount, and no chart of the catalogue mounts that
ConfigMap, so the content is validated, stored, and read by nothing. For it to take effect a chart
has to mount the ConfigMap of that name at the declared path, after the profile's own, where
tenant files win by the `90-`–`99-` prefix ([roadmap.md](roadmap.md) §2.33).

**Guardrails** — checked by the operator when it reconciles the tenant. One entry that fails
holds every drop-in of the tenant back (the Tenant's `DropInsReady` condition is false, reason
`InvalidDropIn`); nothing is refused at admission:

| Rule | Why |
|---|---|
| `name` must match a declared `spec.customization.dropIns[].name` | tenants cannot invent mount paths — that would be L4 at tenant scope |
| Declared entry must set `tenantEditable: true` | not every drop-in dir is safe to expose (a policy file usually is not) |
| Filenames must match `^[9][0-9]-[a-zA-Z0-9._-]+$` | reserves the platform/profile ranges |
| Total size ≤ `maxBytes` (default 256Ki) | ConfigMap limits; DoS |
| Content must parse as the declared `format` | a malformed fragment must fail before it is mounted, not crash the app at boot |
| No secret material — values land in a ConfigMap, in etcd | secrets go through `valueMapping`, always |

**Set by a commit, and by nothing else.** A tenant's values (`config.extraValues`, §2.1), its
drop-ins (`config.dropIns`) and a customization record of tenant scope (§5) can be set today only
by a commit to the deployments repository. No route of the director writes any of them and the
admin console has no screen for them. A route that writes them as signed commits, and an editor
for the tenant's administrator — the declared tenant-editable drop-ins, validated, with the
resulting diff — are on the roadmap ([roadmap.md](roadmap.md) §2.32) and not built.

### 2.3 L2 — Companion (side-by-side)

Build **new code as a separate deployable** that talks to the target app only through its
*published* API. The target app is not modified in any way.

This is the SAP "side-by-side extensibility" rung and the Nextcloud **ExApp** rung, and in Gentian
it is already fully supported infrastructure: scaffold from `gentian-app-template`, publish a
profile, and wire it to the target with a **contract**.

```yaml
# gentian-apps/profiles/acme-approvals/profile.yaml
spec:
  integrations:
    - contract: erp-core            # the target lists it under spec.provides
      provider: odoo-cb-base
      capabilities: [read, write]
```

**Declaring a contract opens nothing.** Where both apps are installed in a tenant the operator
writes an `IntegrationBinding`, and that is a request. The tenant's administrator grants it for the
consumer (`PUT /v1/tenants/{t}/grants/{app}` at the director, `{"consume": [{"contract":
"erp-core", "granted": ["read", "write"]}]}`). Only then are the two network policies written: the
companion's pods may reach the target's, and the target's admit them. Withdrawing the grant takes
both away. The operator grants nothing by itself and never changes a grant, so a grant narrower
than the declaration is what holds; the binding's `Granted` condition names what was asked for and
not granted. That is all the platform does for a contract today: the path is to the target's pods as
a whole, the granted capabilities are recorded and not enforced, and the target is not told which
app is calling. What a consumer may do there is the target's to check.

**A companion that needs the Kubernetes API asks for a cluster role by name.** It cannot bring a
role of its own: `requires.privileges.clusterRoles[]` names one of the roles the platform defines
and the ServiceAccount the chart runs under, and the role is bound only where the cluster permits
it for the profile and the security officer granted it on the install. Rules written on the entry
are ignored. The platform's set is empty today, so no app gets such access
([security.md §3.4](design/security.md)).

**L2 vs L3 tie-breaker** — the one decision this ladder cannot make positionally:

| Choose **L2 Companion** when | Choose **L3 Extension** when |
|---|---|
| The function can stand alone (own URL, own tile on the desktop, own data) | The function must appear *inside* the app's own UI, menus, or workflow |
| It needs its own scaling, language, or release cadence | It must extend the app's data model / ORM / permission model |
| The app's plugin API is absent, unstable, or undocumented | The app has a documented, versioned plugin API (`customization.extension.apiStability: stable`) |
| You need the change to survive a *major* upstream upgrade | Round-tripping through HTTP would be absurd for the semantics |

**Obligations:** a `Contract` must exist (or be added to `gentian-apps/contracts/`); auth via
`oidc-token-exchange`, never shared static credentials; the companion must degrade gracefully if
the target app is not installed.

### 2.3a The sign-in sidecar — an app that can do neither OIDC nor SAML

Some apps have single sign-on only in a paid edition. The platform signs people in to such an app
itself: a person who is signed in at the platform opens the app and is in, with no second sign-in
and no password. The profile declares it and brings one file, the **handler**:

```yaml
spec:
  requires:
    services:
      identity:
        sidecar:                       # instead of identity.oidc or identity.saml
          exposure: web                # may be omitted when one gateway entry is behind a session
          entryPaths: ["/", "/login"]  # pages of the app that lead to the sign-in instead
          database: true               # the handler is given the app's own database
          secrets: [app_secret]        # and these of spec.secrets.generated
          appPort: 3000                # and may call the app's own pods on this port
  expose:
    - name: web
      surface: gateway
      authMode: oidc
      denyPaths: [/api/auth/login]     # the app's own ways in, refused at the front door
```

The handler is the ConfigMap `<profile>.sign-in-handler` of the profile's bundle (key
`handler.js`, label `gentianos.io/asset: sign-in-handler`), so the install's digest covers it.

**What the platform does for it**, per install, and removes with the app:

| | By | |
|---|---|---|
| Registers the sidecar at the tenant's realm | the app Composition | a SAML client named `https://<app host>/sso`, with one address its answer may be posted to, `https://<app host>/sso/acs`; the response and the assertion both signed; the person named by e-mail address |
| Has the realm say who administers the app | the app Composition | one role at that client, `gentian-app-admin`, granted to the tenant's group `gentian:tenant:<tenant>:app-admins`, and a mapper that lists a person's roles at that client in the signed assertion. No other role is in the client's scope, and no group is named |
| Runs the sidecar beside the app | the operator | one pod, with the handler from the bundle, told where the realm is. No service-account token, read-only, not root |
| Hands the handler what was declared | the operator | `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD`; `SECRET_<NAME>`; `APP_URL`. From this app's own vault paths; a profile names which, never where |
| Routes two paths of the app's host to it | the operator | `/sso/login` as a rule of the app's own route, behind the session and the bouncer; `/sso/acs` on a route of its own with no session ([routing.md §4.1](design/routing.md)) |
| Sends the entry paths to `/sso/login` | the operator | for a page load (GET) only |
| Has the realm tell the sidecar of a sign-out | the app Composition and the operator | the client's single-logout address is the sidecar's own Service, `http://<app>-sign-in.<namespace>.svc.cluster.local:8081/sso/logout`; the sidecar is told the same address and answers `/sso/logout` under that name only. Not routed: nothing outside the cluster reaches it |
| Opens the sidecar's network paths | the operator | the realm's certificate; the app's database server; the app's own pods on `appPort`. One port each, and nothing else: the sidecar is not one of the app's pods |

A profile states no address, no client name and no Secret. They follow from where the app answers
and what the app owns, so a profile cannot point a sign-in somewhere else or at somebody else's
data.

An entry path cannot also be under `denyPaths`. The bouncer refuses a denied path before the
Gateway redirects, and for every method, so the person would never reach the sign-in. Where an
app's own form posts to the address of its page -- OpenProject's `/login` -- the page is the entry
path, and what is posted there is the app's to refuse: its password sign-in is switched off in the
app. A profile that declares the sidecar is rendered by the platform's Composition, which is the
one that registers it at the realm; a bundle that brings a Composition of its own would have to
compose the client itself.

**Which handler runs.** A handler is handed the app's signing key and its database: it can become
anybody in the app. So it runs only where it is known to be the reviewed file:

- from a bundle of a catalogue of the **whole cluster**. A tenant's own catalogue publishes
  profiles alone and has no handler to bring; a profile of one that declares a sidecar is held
  (`SignInSidecarRefused`) rather than installed with no way in;
- for an install **pinned to a digest**, whose bundle brings the handler. The operator compares
  the cluster with the bundle before every rollout, gives the sidecar the bundle's own bytes, and
  tells it their sha256; the sidecar loads no other file;
- never in the kernel realm.

**Who may use the app** is not the sidecar's to decide and not the handler's. A sign-in begins on
`/sso/login`, which only a person the front door admits to this app reaches, and the answer the
sidecar accepts has to be about that same person.

**Who administers the app** is who holds the platform's **App Admin** role in that tenant, and
nobody else: not the tenant's administrator for being that, and not the first person to open the
app. The role is membership of the tenant's group `gentian:tenant:<tenant>:app-admins`. A tenant's
administrator appoints somebody in the admin console — *Groups*, under *Roles*, the group
`app-admins`: add the person; or the person's own page, under their groups — and withdraws it in
the same place. It
is one role per tenant, the one `provisioning.privilegedRole` already maps for other apps: who
holds it administers every app of the tenant that maps it.

The realm states it in the assertion it signs; the sidecar reads it there and nowhere else — no
header, no form field — and tells the handler (`person.appAdmin`). At every sign-in the handler
gives the app's own administrator role to a person who holds the role and takes it from a person
who does not, in the app's own way, before it makes the session. So an appointment takes effect
the next time the person opens the app, and a withdrawal at their next sign-in, which is within
the hour a session lasts.

**What a handler is given, what it answers and what it must not do** is in gentian-apps,
`images/gentian-sidecar-sso-saml/README.md`. In short: it is told who the person is, whether they
administer the app and how long the session may last (an hour at most), it answers where to go
and which cookies or browser storage carry the app's session, and the sidecar writes the
response. It gives nobody a password and touches no licence check.

**Signing out is part of the handler.** A handler may export a second function:

```js
module.exports = {
  async onLogin(person, ctx) { /* make the session; as before */ },

  // Optional. The person signed out at the platform.
  // person: { email }   ctx: { origin, log(event, fields) }
  async onLogout(person, ctx) {
    // end every session this person has in the app, in every browser
  },
};
```

When a person signs out, the realm posts a signed logout request to the sidecar inside the
cluster. The sidecar accepts it only signed by the realm, addressed to its own address, fresh and
not seen before, and then calls `onLogout` with the person it names. The handler ends that
person's sessions in the app — all of them: it is told who signed out, not which browser.
Docmost's and OpenProject's delete the person's session rows, which both apps look up on every
request.

A handler without `onLogout` is one for an app whose session cannot be ended from outside: the
sidecar answers the realm, logs `no-sign-out-handling`, and the app's session lasts what is left
of its hour. Say so in the app's customization record, as Activepieces' does. A handler written
before this, and a sidecar built before it, go on working with each other and with the new ones.

**Before reaching for it.** It is the last of three ways to sign people in ([iam.md §1.11](design/iam.md)),
and the only one in which a program beside the app holds the app's keys. Use it when the edition
installed offers nothing else, write the `Customization` record first (rung L2: the handler is a
companion that integrates through the app's interface or its database), and record there anything
the handler writes itself that the app's own interface would not have done.

### 2.4 L3 — Extension (in-app addon)

Use the app's **own extension system**. Odoo addons (`_inherit`, view `xpath`/`inherit_id`),
Nextcloud apps, XWiki extensions, Activepieces pieces, Keycloak SPIs, Collabora — none.

`spec.customization.extension.delivery` names how an addon arrives:

| Delivery | Mechanism | Use when |
|---|---|---|
| `git-sidecar` | `gentian-sidecar-git-modules` syncs a git repo into the app's addon path (`odoo` chart: `gentian.git.repo` → `gentian.modulesPath`) | addons iterate faster than the app image |
| `image-layer` | addons baked into a Gentian-built image layer at build time | reproducibility/airgap matters more than iteration speed |
| `addon-profile` | a thin `ComponentProfile` whose package is `spec.package.addon.{id,of}`; the tenant selects it into a base via `Tenant.spec.apps[].addons` | the addon is a *catalogue-visible product* |
| `app-store-api` | the app's own runtime API installs the extension (Nextcloud `occ app:install`) via `spec.hooks.postInstall` | the app owns its own registry |

#### How an `addon-profile` is activated

The tenant selects **profile names**; the operator resolves them to whatever the hosting app
calls the thing (an Odoo module, a Nextcloud app id) through each addon's own
`spec.package.addon` (`internal/customization/addons.go`). Nothing in gentian-os knows those app-native names — putting that
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
separate, explicit path: uninstalling an app keeps everything it stored, and only a purge of the
uninstalled app destroys it ([design/store-contract.md](design/store-contract.md) §8).

**Multi-tenancy is the sharp edge here.** An addon loaded into a shared runtime affects every
tenant on that runtime. The existing Odoo pattern is the right precedent — per-tenant addon sets
driven by group attributes (`gentianos.io/keycloak-group-attributes: {"gentianOdooModules":["crm"]}`)
rather than per-tenant addon *binaries*. The framework should make this explicit:

> **L3 rule (namespace test).** Per-tenant addons are allowed **only if the app instance runs in
> that tenant's own namespace** (`tenant-<name>`). If the instance lives in a shared namespace
> serving more than one tenant, addons are **profile-scoped only** — per-tenant behaviour must come
> from the addon reading tenant context at runtime, never from divergent addon sets.

The namespace, not the profile, is the test: "one instance per tenant" is an intention, but
"deployed into `tenant-acme`" is a fact that can be checked. The `odoo-cb-*` family passes (one
Odoo per tenant, `databasePerTenant: true`); a shared-runtime app would not.

Sharing a runtime across tenants and then loading tenant-specific code into it is the single
fastest way to turn a customization into a cross-tenant data leak. **Nothing checks this
today**: for a record with `rung: L3` and `scope: tenant` it is a review-time rule, and the
controller that judges records does not look at where the app runs.

**Obligations:** declare the addon repo + delivery in `spec.customization.extension`; pin the
addon version alongside `spec.package.chart.version`; a `Customization` record (§5) is **required** from
L2 upward; the addon must be tested against the pinned app version in CI.

### 2.5 L4 — Repackage

The upstream **source and image are unchanged**, but Gentian now owns the *packaging*: a
Gentian-authored Helm chart wrapping an upstream image, a `composition.yaml` with init containers
or bootstrap Jobs, an entrypoint wrapper, a `spec.hooks.postInstall` job that calls the app's admin API,
sidecar injection, or a Kustomize post-render over an upstream chart.

This is where most Gentian upstream apps already sit (`charts/odoo`, `charts/gentian-sidecar-*`,
per-profile `composition.yaml`).

A `composition.yaml` may compose only the Crossplane provider resource types a cluster creates:
those in gentian-os's `crossplane/providers/activation.yaml`. One of another type renders and
never appears. `make lint-provider-activation` in gentian-os reads the gentian-apps checkout beside
it and names the type to add ([install-reference.md §4](install-reference.md)).

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
| Commercial terms for paid customizations | the platform gates no edition (§4.2); terms are between a tenant and a supplier |
| Review SLAs and a delegated maintainer role | needs the governance model in §8.1 to be operating first |
| Automated upstreaming of external contributions | needs the debt report (§8.3) to have real data |

**What v0.4 must not do** is bake in the assumption that Gentian is the author. Two concrete
consequences: `Customization.spec.owner` is a free-form owner reference rather than a Gentian team
enum, and the §8.1 approval matrix names *roles* (catalogue maintainer, platform team) rather than
Gentian individuals, so an external maintainer can hold a role later without a schema change.

---

### 2.10 A public website surface, and the main address

A profile publishes pages without sign-in by declaring a perimeter entry:

```yaml
expose:
  - name: site
    surface: perimeter
    authMode: none        # written out: nobody signs in to read it
    paths: ["/"]          # only what is listed is published
    backend: {service: website, port: 8080}
```

Declaring it publishes nothing. Once the app is installed the entry is a
request: `kubectl gentian exposures requests --tenant <t>` lists it with the
address it would be published at, its paths and who can reach it (the admin
console shows the same under Apps → Details, and approves, reviews and
withdraws there). The tenant's perimeter approver
approves it with `kubectl gentian exposures approve <install> <name> --tenant
<t>` (`PUT /v1/tenants/{t}/exposures/{install}/{name}`). The approver is
recorded as its owner, with a review date a year on at the latest;
`--expires <date>` takes it down on a day, `--reason` is kept with it, and
approving an approved entry again is its review. The director refuses an approval of an app that is not installed
in the tenant, of an entry that asks for nothing an approver decides (a
plain entry behind sign-in), and one whose `apex` is not the entry's. The entry then answers at `<subDomain or component
name>.<tenant's domain>`, from a proxy that passes no cookies either way.
`exposures list` shows what a tenant has published, and `exposures
withdraw <install> <name> --tenant <t>` takes one down.

**The proxy checks no caller.** A perimeter entry declares one of two modes:

| `authMode` | What the platform does | Use it for |
| --- | --- | --- |
| `none` | Forwards the declared paths for anyone. Removes `Authorization` and `Cookie`: no credential reaches the app | Pages and links for anyone; a link that carries its own secret in the path or query; a webhook signed in a header of its own |
| `app` | The same, and passes the caller's `Authorization` header to the app as sent. The app alone checks it | Sync clients, mobile apps, API keys and webhooks that present a credential **the app itself issued** |

```yaml
expose:
  - name: dav
    surface: perimeter
    authMode: app         # the app checks each caller's own credential
    subDomain: dav
    paths: ["/remote.php/dav/"]
    backend: {service: files, port: 8080}
```

`authMode: app` is approved like any public entry, and the approver is
shown, in the director's words, what it means: the caller's credential is
passed to the app; the platform does not know or check who calls; an app
password or token of a person removed from the tenant keeps working until
the app itself revokes it; and the limit per client address, which is lower
for such an entry (5 requests a second, 50 more at once, 20 at a time).
Its limits: cookies pass in neither direction, so a client that needs the
app's cookie does not work on it, and `apex` entries cannot use it.
`basic`, `bearer`, `jwt` and `signature` are refused on a perimeter entry:
the platform verifies no password, token or signature there yet.

**Behind sign-in: the `Authorization` header as the app's own.** A gateway
entry may ask for one thing of the same approver:

```yaml
expose:
  - name: web
    surface: gateway
    authMode: oidc
    clientAuthorization: app   # the app's pages send the app's own token there
    backend: {service: flows, port: 8080}
```

Declare it only for an app whose own pages call its API with a token of the
app's in `Authorization`. It is listed with the tenant's requests as
"Behind sign-in: keeps the app's own Authorization header", is no public
address, and is approved, reviewed and withdrawn with the same commands.
Until it is approved the entry works like any other behind sign-in — the
header is removed, so those calls fail — and the Component's
`ClientAuthorization` condition says so. Once approved, sign-in and the right
to use the app are required exactly as before; the front door neither puts
its own token in the header nor removes the page's, and no platform token
reaches the app. It holds for every entry of the component on the same host,
excludes `forwardToken` and `exchangeToken` anywhere in the profile, and is not for a service.

An approval is for the kind of entry it was given for. A profile that later
changes an approved entry from `none` to `app`, or adds
`clientAuthorization`, is a request again until it is approved as that.

**Address names an app cannot take.** No entry of an app or an add-on, on
either surface, may use one of the platform's names as its `subDomain` (or as
the component's name, where it states none): `desktop`, `admin`, `store`,
`console`, `platform`, `id`, `auth`, `login`, `signin`, `sign-in`, `sso`,
`account`, `accounts`, or anything below one (`x.admin`). `desktop`, `admin`
and `store` are taken by the platform's own component for each and by nothing
else; the others by nothing at all. Where the tenant's domain is the cluster's
own -- the user tenant of a single-tenancy cluster -- the kernel's addresses
are refused as well: `argocd`, `corp`, `headlamp`, `imap`, `llm`, `mail`,
`mail-egress`, `www`. The install is refused by the director (`422`, naming
the entry and the label), and a Component that exists anyway is held by the
operator with `HostReserved`; stating `trustTier: platform` changes nothing
([design/routing.md §3.1](design/routing.md)).

"For the main address" is the same entry with `apex: true` (and no
`subDomain`). It asks for the cluster's bare domain and needs all of this:

- the cluster's tenancy mode is `single`, the tenant is its user tenant, and
  that tenant is not on a domain of its own;
- the approver sends `{"apex": true, "acknowledgeMainAddressRule": true}`
  with the request (the CLI: `--acknowledge-main-address-rule`, which `--yes`
  does not imply) -- an entry that says `apex` is not published at all
  without `apex`, and the director refuses `apex` without the acknowledgement
  (`400`, with the warning below);
- no other surface holds the main address;
- the entry declares no path inside `/branding/`, `/sign-in`,
  `/.well-known/acme-challenge/` or `/.well-known/pki-validation/`. Those stay
  the platform's.

**Before you approve a website for the main address.** A script in a page on
the main address can set cookies that browsers also send to the desktop, the
consoles and sign-in. It cannot read anybody's session. It can stop people
from signing in until they clear their cookies, and it can sign a person in
to an account the script's author chose, without the person noticing. The
platform cannot check what a website loads, so the rule is yours to keep:

- only a site whose scripts your organisation itself controls;
- no third-party scripts (analytics, embeds, widgets, anything loaded from
  another host);
- no pages uploaded by users.

`"acknowledgeMainAddressRule": true` says you were told this and the site
meets it. It is asked on the first publication and on every review, and your
name and the time are recorded with the entry in the exposure registry
(`apexAcknowledgedBy`, `apexAcknowledgedAt`). A website approved before this
was asked stays published and is asked at its next review
([design/security.md §2.10](design/security.md)).

**What every published entry is held to.** Whatever a profile declares, the
publishing proxy refuses a request whose path has more than one reading
(`//`, dot segments, encoded slashes, `;`), the methods `TRACE`, `TRACK` and
`CONNECT`, a body over 10 MB, and more than 20 requests a second from one
client address (200 more at once, then `429`) or 100 at a time; it waits 60
seconds for the app's answer. It matches
`paths` by whole segments and `denyPaths` without regard to case, and it
does not render a path with characters outside letters, digits and
`/ . _ ~ -`. It removes every identity header and `Cookie` on the way in, `Authorization`
too unless the entry is `authMode: app`, and `Set-Cookie` on the way out. A profile has no field to
change any of this; the numbers are the cluster administrator's
([design/security.md §2.14](design/security.md)). An app that needs larger
uploads or cookies on a public path cannot have them on a perimeter entry
today.

**What an entry behind sign-in is left with.** On a `surface: gateway` entry
with `authMode: oidc` the app is told who is asking in the front door's
headers, and nothing else of the session reaches it: the front door puts its
own token in `Authorization`, replacing whatever the page sent, and takes it
out again before the app, with the session's cookies. So an app whose pages
call its own API with a bearer token of their own loses that token on such an
entry, unless the entry declares `clientAuthorization: app` and was approved
(above). `forwardToken: true` passes the
front door's token on instead, and needs `trustTier: platform`.

**Telling the app that a person signed out.** Signing out ends the session
at the front door and at the realm. A session the app keeps itself is the
app's: it ends then only if the realm tells the app and the app acts on it.
An app with its own sign-in client says where it takes that notice:

```yaml
spec:
  requires:
    services:
      identity:
        oidc:
          clientId: gentian-example
          backchannelLogout:
            exposure: web                    # an entry under spec.expose
            path: /oauth/backchannel-logout  # the app's endpoint for it
  expose:
    - name: web
      surface: gateway
      authMode: oidc
      backend: {service: example, port: 8080}
```

A path and an entry, never an address. The platform builds the address from
the entry's own backend,
`http://<backend.service>.<namespace>.svc.cluster.local:<backend.port><path>`,
and registers it with the client at the realm. Keycloak then posts its logout
token there when a person signs out: inside the cluster, at the app's own
Service, not at the public address, where the front door would ask Keycloak
for a session it does not have.

- The entry must be this profile's and route to this component's own Service
  (no `backend.component`), and that Service's name must be a plain name.
- The path is segments of letters, digits, `_`, `~` and `-`, with single dots
  inside a segment: no query, no fragment, no `..`, no `//`.
- The older `backchannelLogoutUrl`, an address of the profile's own writing,
  is refused, and the message names this field. It let a catalogue entry make
  the identity provider post to any address, and every profile that used it
  named an address the notice could not reach.
- It is served for the component's own client, by the platform's Composition.
  An extension's client may not declare it, and a profile that brings a
  Composition of its own has to build the address the same way itself.

The app sees its Service's name in the `Host` header. One that refuses a
host it does not know needs that name
(`<service>.${TENANT_NAMESPACE}.svc.cluster.local`) among the hosts it
trusts; that lets it answer a caller that could already reach it and opens
nothing.

Declare it only for an app that **checks the token** before it ends a
session -- the signature against the realm's keys, the issuer, that the
audience is its own client, and the back-channel logout event -- and say in
the profile's customization record what it checks. Show it once against the
app's real image (gentian-apps, `e2e/oidc-sign-out`). Keycloak posts once and
does not try again, so the notice is the common case and the app's own
session lifetime is the bound. An app behind the sign-in sidecar declares
none of this: §2.3a. What it guards and what it leaves open:
[security.md §2.15](design/security.md).

The component's `MainAddress` condition says whether it is there and, if not,
why. A profile that should also work on a multi-tenancy cluster declares a
second entry without `apex`. A fixture profile is in
`internal/controller/testdata/main-address/website-profile.yaml`.

## 3. The second axis — scope (blast radius)

Rung and scope are **independent**. Minimise both.

| Scope | Affects | Authored in | Approved by |
|---|---|---|---|
| **S0 · Tenant** | one tenant's install | the tenant's manifest in the deployments repository (`Tenant.spec.apps[].config`) | cluster admin |
| **S1 · Profile** | every tenant that installs this profile | `gentian-apps` (`profiles/<n>/`, `apps/<n>/`, addon repo) | catalogue maintainer |
| **S2 · Platform** | every tenant, every app | `gentian-os` (kernel, operator, compositions, policy) | platform team |

**The S2 gate is the existing platform boundary rule and does not change:** a customization may
enter `gentian-os` *only* if it is generic across apps. App-specific behaviour at S2 scope is
forbidden regardless of rung — no `case "myapp"` in a reconciler, ever. If many apps need the same
thing, extend the `ComponentProfile` contract generically; if one app needs it, it belongs in
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

## 4. `ComponentProfile.spec.customization` — the per-app ladder declaration

This is what makes the framework *app-specific* rather than generic advice. It is a **generic**
block — it describes capabilities in app-neutral terms, so it does not violate the "no per-app
fields" rule. The types are in `api/v1alpha1/customization_surface_types.go`. The example shows
the block alone; a profile's required fields (`classes`, `launch`, `trustTier`, `version`,
`package`) are left out.

```yaml
apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: odoo-cb-base
spec:
  # classes, launch, trustTier, version, package: ...
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
        tenantEditable: true               # a tenant may supply files here (§2.2.1)
        maxBytes: 262144                   # the default

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
      chartOwnership: gentian-owned        # upstream | patched | gentian-owned | vendored
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

A base that takes addons also states how the selection is activated — `addonActivation`
(`valuesPath`, `script`) and `addonValues[]` (`whenAddon`, `values`), §2.4. `rubricScore` records
the score behind `grade` (§4.1). An addon states none of this: it is `spec.package.addon`, and
the schema refuses `spec.customization.addon`.

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


### 4.2 Bases, addons, editions and packages

The catalogue shape L3 is delivered through. Referenced by the `ComponentProfile`,
`AppPackage` and `Tenant` CRD field documentation.

```text
profiles/<family>/
  base/       <family>-base-<name>      # deployable
  addons/     <family>-<addon>-<name>   # activated inside a base, never installed alone
  packages/   <family>-<package>        # not deployable — a UI preset
```

An **addon** declares `spec.package.addon.{id,of}` and is selected into an
installed base, arriving in `Tenant.spec.apps[].addons`. It inherits the base's
ladder — same image, same drop-in dirs, same plugin API — so it never restates
`grade`, `rubricScore` or `supportedRungs`.

An addon can be **pinned to a build**, like an app. The pin is recorded beside
the list, in `Tenant.spec.apps[].addonPins` (`name`, `digest`, `catalogue`);
`addons` stays a list of names. An addon is pinned only inside an app that is
itself pinned: the director refuses a build stated for an addon of an app
whose entry carries no `digest`, and says to install the app at a stated build
first (`kubectl gentian apps install <app> --tenant <t>` pins it). A pinned
addon is activated only from a
profile shown to be that build, and its base is held as it runs until it is.
See [design/store-contract.md](design/store-contract.md) §3 and §4.

**Editions** are `ce · pe · me · ee`, and say *who maintains and supports the
entry*, not who publishes it:

| | |
| --- | --- |
| `ce` | community edition, as the upstream organisation publishes it |
| `pe` | private edition: somebody's own profile, in their own catalogue source, for their own tenants |
| `me` | maintained edition — `ce` plus active Gentian maintenance |
| `ee` | enterprise edition — commercially licensed and supported by its supplier |

`ee` is deliberately not "the upstream's enterprise build" — a third party's
proprietary distribution is equally an `ee`. A supplier's name is never an edition. Editions are technically
compatible with one another, and the OS gates none of them: an `ee` app or
addon is installed and activated exactly like a `ce` one. Supply is controlled
at the source — its chart and images arrive only where the tenant holds a
credential for the repository they are pulled from
([store-contract.md](design/store-contract.md) §2). What constrains
addon↔base compatibility is **version**. There is therefore one addon set per
family and no per-edition compatibility matrix.

The split that matters operationally is not free against paid but **where the
entry comes from**. `ce` and `pe` are entries a cluster can hold and install on
its own — `ce` because it is public, `pe` because it is the operator's own — so
a cluster browsing its own catalogue sources lists exactly those two. `me` and
`ee` exist because somebody maintains or licenses them, which is the App
Store's business: a cluster counts them and sends the person to the store,
because an entry whose whole value is a relationship with a supplier is not
something a cluster can describe usefully.

An **edition shares a family name and nothing else** — `nextcloud-base-ee` may
deploy a supplier's all-in-one chart from a credentialed registry where
`nextcloud-base-ce` deploys the community chart — so it inherits no ladder and must
be characterised on its own.

**Naming is a hint, not a contract.** Two profiles may both be edition `ee` from
different authors under unrelated names. A profile carries neither an edition nor an author
field: the edition is what the catalogue's index entry states for it
([custom-catalogues.md](custom-catalogues.md)), and who supplies an entry is the store's to say.
Never infer either from a profile name.

A **package** is an `AppPackage`: cluster-scoped, no status, no reconciler, no
workload. It names a family and a set of addons, and pre-selects them in the
install window. The user may still adjust the selection, which is why a curated
bundle stays a preset rather than becoming an artifact.

---

## 5. The `Customization` record

**Required for every customization at L2 and above.** Written **before** the code. Modelled on
DEP-3 patch headers, generalised to the whole ladder.

**`Customization` is a namespaced CRD** (decision §11.1). Records are authored in git, beside the
artifact they describe: a profile's in `gentian-apps/profiles/<n>/customizations/<name>.yaml`,
from where it reaches a cluster in the profile's bundle (below). Being a cluster object buys
three things a file cannot:

* the **admin console reads live records** (§8.3) instead of a CI-generated snapshot;
* **the operator judges every record against the §3 cost matrix** — an `L5` record at
  `scope: tenant` is marked `Invalid` (condition `Valid` false, with every violation listed).
  Nothing refuses a record at admission;
* `status` carries **derived state** — `reviewOverdue`, `upstreamStale`, `targetVersionDrift` — so
  the debt report is computed by the operator, not by a script guessing from YAML.

Profiles and Compositions are cluster-scoped — there is no per-profile namespace. Profile-scoped
records land in the **one namespace the cluster's catalogue is applied in** (the provisioning
namespace, `kernel-provisioning`), a cluster-wide constant that is not derived from the profile
name. Tenant-scoped records belong in `tenant-<name>`; no route of the director writes one, so
today such a record reaches a cluster only by a commit beside the tenant's manifest.

**A profile-scoped record arrives in its profile's bundle.** A profile reaches a cluster one at a
time, from a catalogue, when a tenant installs it, as one file under one digest
([custom-catalogues.md §2](custom-catalogues.md)). A record travels in that file as a companion:
named `<profile>.<record>`, `spec.target.profile` its own profile, `spec.scope: profile`, and only
from a catalogue of the whole cluster — a tenant's own catalogue brings profiles alone. The
ApplicationSet that used to sync a whole profile directory — profile, `customizations/`,
`composition.yaml` — from a `role: apps`, `type: git` repository is retired, and the director
refuses to declare such a repository.

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
   Read ComponentProfile(A).spec.customization.
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
| L0 | tenant | deployments repository | the tenant's manifest: `Tenant.spec.apps[].config.extraValues` |
| L0 | profile | `gentian-apps` | `profiles/<n>/profile.yaml` → `spec.package.extraValues` |
| L1 | profile | `gentian-apps` | `profiles/<n>/dropins/` + the chart's mount of it |
| L1 | tenant | deployments repository | the tenant's manifest: `Tenant.spec.apps[].config.dropIns` (§2.2.1) |
| L2 | profile | `gentian-apps` | `apps/<new>/` (from template) + `contracts/<c>.yaml` + `profiles/<new>/` |
| L3 | profile | addon repo (`odoo-modules`, …) | addon + pinned version in profile |
| L4 | profile | `gentian-apps` | `charts/<app>/`, `profiles/<n>/composition.yaml`, `spec.hooks.postInstall` |
| L5 | profile | build repo (`ocb`, …) | `patches/series` + DEP-3 headers + Dockerfile |
| L6 | profile | fork repo | vendored source, `UPSTREAM-COMPARISON.md` |
| any | platform | `gentian-os` | **only** if generic for all apps (§3) |

Note `profiles/<n>/` is a *bundle*, not a fixed depth: singletons sit at
`profiles/xwiki/`, members of a multi-profile family at
`profiles/odoo/odoo-cb-crm/`. Locate a bundle by its leaf directory name, which
CI requires to equal the `ComponentProfile`'s `metadata.name` — never by counting path
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
level, exactly as `spec.trustTier` is a field rather than a `certified/`–
`experimental/` split.
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
    ├── profile-block.yaml     # spec.customization block, ready to paste into the ComponentProfile
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
* Volumes need nothing from the chart to survive an uninstall. The platform installs an app as the
  Helm release `<profile>-release` in the tenant's namespace — the same name on every install — and
  marks every PersistentVolumeClaim the chart renders `helm.sh/resource-policy: keep`. Uninstalling
  leaves the claims in place and the next install of the app in that tenant takes them over; only a
  purge deletes them. So: do not set the annotation yourself to mean something else, do not delete
  claims from a hook, and keep a claim's immutable fields (storage class, access modes) stable
  across chart versions, because a reinstall updates the kept claim rather than creating one.

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
**admin-console** — is the highest-value first implementation step: it is
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

Additionally, the operator marks a record `Invalid` (it refuses none at admission) for:

| Marked `Invalid` | Rule |
|---|---|
| `rung: L5\|L6` with `scope: tenant` | §3 cost matrix |
| a rung the target does not list in `supportedRungs` | §4 |
| `rung >= L4` with `upstreamFirst.attempted: false` | P3 |
| a skipped rung with no `rungJustification` | §6 step 3 |
| a record whose `target.profile` does not resolve to a `ComponentProfile` | dangling debt |

A tenant drop-in naming an undeclared or non-`tenantEditable` entry is held on the Tenant
(§2.2.1). The §2.4 namespace test for `rung: L3` with `scope: tenant` is not applied yet.

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
| "Raise Collabora's document size limit" | Collabora (C) | **L0** | it is a documented value | `spec.package.extraValues` |
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
| 1 | `Customization` as CRD or plain YAML? | **CRD** (`gentianos.io/v1alpha1`, namespaced) | The admin console reads records live; the operator judges them against the §3 cost matrix; §5 records are cluster objects, not just files |
| 2 | Is `spec.customization` reference data or contract? | **CRD block on `ComponentProfile`** | Machine-readable for agents and the App Store; §6 step 2 is executable |
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
| 3 | `spec.customization` on the `ComponentProfile` CRD + populated for those apps | `gentian-os`, `gentian-apps` | **done** |
| 4 | §6 procedure added to all `AGENTS.md` files | all | **done** |
| 5 | `Customization` CRD + CI validator + debt report generator | `gentian-os`, `gentian-apps` | **done** |
| 6 | `customization/` scaffolding + extension loader + slots in the template | `gentian-app-template` | **done** |
| 7 | `values.schema.json` for Gentian-owned charts | `gentian-apps` | **done** |
| 8 | L5 discipline retrofitted to `ocb` (DEP-3 headers, `series`, CI bump gate) | `ocb` | **done** |
| 9 | Debt report surfaced in Admin Console | `gentian-ui` | **done** |
| 10 | Tenant drop-in reconciler (§2.2.1) | `gentian-os` | **done**; the editor for it in the admin console is not built |
| 11 | L3 unified on one addon model: `addon-profile` delivery, addon resolver, activation, selection window (§4.2) | `gentian-os`, `gentian-apps` | **done** |
| — | Automated grade rubric in CI | `gentian-apps` | roadmap 2.13 |
| — | Third-party delegation process (signing, review SLAs) | — | deferred, §2.9 |
