# Tenant Provisioning: What Could Move to Crossplane

**Status:** Inventory. No decision taken, nothing implemented.
**Scope:** the tenant reconcile path — `internal/controller/*.go`, `crossplane/compositions/tenant-default.yaml`, `crossplane/xrds/tenant.yaml`
**Question it answers:** is there a reason not to migrate tenant provisioning entirely to Crossplane?

---

## 1. Why this exists

`Tenant` is the only kind in the platform with both an operator CRD and a Crossplane composite.
Every other kind — `App`, `Cluster`, `InfraData`, `Repository`, `Suze` — is a claim Crossplane
owns outright, with no operator CRD in front of it. `XTenant` is also the only XRD with no claim
(`claimNames` is unset), so it is not a user-facing API at all: the operator writes it.

That arrangement began as a migration. `95a644f` — *"Phase 3 — XTenant XRD + tenant-default
Composition + ensureTenantXR bridge"* — describes the XRD as *"mirroring the existing Go Tenant
CRD schema"* and states the intent plainly:

> Imperative provisioners (identity, LDAP, databases, etc.) run in parallel — idempotent by
> design. **Phase 3b will migrate them to the Composition one-by-one.**

`docs/crossplane-migration-plan.md` tracked that work. It was deleted in `cd5c186`, a
documentation cleanup that renamed `architecture-crossplane.md` to `architecture.md` and removed
the migration and legacy docs together. Nothing recorded whether Phase 3b had finished, been
abandoned, or been superseded — so this inventory reconstructs the answer from the code.

## 2. Method

Every `ensure*` method on `TenantReconciler` (63) and every `sync*`/`seed*` helper (20) was
classified by what its body actually does, not by its name. The categories are the ones the code
exhibits, and the discriminator that matters is **declare versus compute**: a step that states
desired resources can move to a Composition, a step that enumerates or computes cannot.

## 3. The inventory

| Category | Count | Can it move? |
|---|---|---|
| **Waits on Crossplane-owned work** | 18 | Already moved — see §4 |
| **Writes Kubernetes objects** | 14 | Yes, `provider-kubernetes` |
| **Dispatch / thin wrappers** | 25 | N/A — no work of their own |
| **Direct external API call** | 1 | See §5 |
| **Computes key material** | 2 | No — §5 |
| **Enumerates external state** | 1 | No — §5 |

## 4. The most important correction

The tenant path is **not** 63 imperative steps beside Crossplane's 10 resources. Eighteen of them
do no provisioning at all: they call `waitForProvisioningJob` and block on a Job the *Composition*
created. `ensureRealmJob`, `ensureAdminJob`, `ensureClientJob` and `ensureSAMLClientJob` say so in
their own doc comments — *"waits for the Crossplane-owned tenant realm Job"*.

Identity provisioning has therefore already moved. The operator writes a manifest bridge, Crossplane
runs the Jobs, and the operator observes. Counting those as imperative work overstates what is left
by roughly a third, and any argument for the split that rests on that count is wrong.

## 5. What genuinely cannot move

Four things, and they are narrow.

**`syncMailAppPasswords` — enumerates external state.** It calls
`GET /admin/realms/{realm}/users?first=&max=100`, pages through Keycloak's *current* users, and
derives one credential per user. A Composition renders from the composite's spec;
`function-extra-resources` reads Kubernetes objects, not a remote API's contents. There is no way
to fan out over a list that must be discovered at reconcile time. This is an expressiveness gap,
not an effort gap.

**`ensureDKIMKeyPair` and the passwd-file hashing — compute rather than declare.**
`rsa.GenerateKey`, `hmac.New`, `argon2.IDKey`. Compositions template; they do not compute. Each
could be wrapped in a Job, which keeps the imperative code and loses the ability to debug it
directly.

**`ensureTenantOpenBaoAuth` — the provider cannot adopt what it did not create.** `provider-vault`
exists and is used elsewhere, but its jwt `AuthBackend` creates the mount and writes its config as
one resource and cannot adopt an existing mount. The failure is not recoverable: a mount left by a
partial create answers 400 *"path is already in use"* on every later reconcile, and nothing in
Crossplane can clear it. This is documented at length in `scripts/steps/B-07-openbao-oidc-mount.sh`,
which owns the kernel mount for exactly this reason.

**`ensureDovecotAuthReload` — a side effect keyed on change.** It restarts Dovecot when the realm
set changes. Declarative systems express desired state, not "when this changes, do that".

## 6. What could move but has not

The larger finding. `provider-keycloak` v2.19.0 is **installed and healthy**, and covers everything
the tenant path needs — realms, OIDC and SAML clients, users, groups, identity providers and
authentication flows — across 32 API groups.

It is *barely* used rather than unused: three live managed resources, all app-level. The
`app-default` Composition creates an OIDC client per app, and both are `Ready`/`Synced`
(`corp-app-store-me-keycloak-client`, `corp-nextcloud-base-ce-keycloak-client`). Meanwhile the
tenant-level objects — realm, admin user, portal and Dovecot clients, groups, broker identity
provider, browser flow — are still provisioned by Jobs.

So app-level identity has already made the move, and it works. The obstacle for the rest is not
capability, and no longer even an unproven path: it is that the work stopped when Phase 3b stopped.

**Adoption is possible, and verified.** The obvious objection is that these objects already exist,
so Crossplane would have to adopt rather than create them — the trap that keeps the OpenBao mount
imperative, where `provider-vault` cannot adopt a mount it did not create and a partial create
strands the path permanently. `provider-keycloak` does not have that problem. A `Realm` with
`crossplane.io/external-name: corp` and `managementPolicies: ["Observe"]` adopted the live realm on
first reconcile — `Ready: True`, `Synced: True`, 56 fields read back including
`displayName: Gentian Corp` — and removing the probe left the realm untouched. Existing clients
carry a Keycloak UUID as their external name, so the same import works for them given the id.

## 6a. The boundary

One test decides which side a step belongs on:

> **Can the answer be written down before it happens?**

If it can, it is a statement about what should exist, and Crossplane owns it. If it cannot — because
it must first be asked for, computed, or reacted to — the operator owns it.

**Crossplane owns everything that can be stated in advance.**

| | Provider |
|---|---|
| Namespace shell, limits, quota, network policy | `provider-kubernetes` |
| Realms, clients, users, groups, identity providers, authentication flows | `provider-keycloak` |
| Policies and roles | `provider-vault` |
| Charts | `provider-helm` |

An object already existing is not a reason to keep it imperative: adoption by
`crossplane.io/external-name` works and is verified above.

**The operator owns four things, and only these.**

1. **Discovery** — enumerate external state and act on what is found. Keycloak's *current* users are
   not in any spec, so the credential minted per user cannot be rendered from one. Narrower than it
   first reads: this is about state **nobody declared**. Reading a value a managed resource itself
   produced — a generated client secret, say — is what connection secrets are for, and stays
   declarative. See the broker group below.
2. **Computation** — produce a value rather than restate one: `rsa.GenerateKey`, `hmac.New`,
   `argon2.IDKey`. Compositions template; they do not compute.
3. **Adoption gaps** — where a provider cannot take over an object that already exists without
   risking it. `provider-vault`'s jwt `AuthBackend` is the case: it creates the mount and its config
   inseparably, cannot adopt, and a partial create strands the path permanently.
4. **Change-triggered action** — "restart when this changes" is a moment, not a thing.

**And one job that is not a category but is properly the operator's:** observing what Crossplane
provisioned and aggregating it into a single answer. `Tenant.status` carries fourteen conditions;
eighteen `ensure*` steps exist only to wait on Crossplane's own Jobs and report. That is the
operator being a controller, not the migration being unfinished.

**What the boundary is not.** It is not "Go is imperative, YAML is declarative". A level-triggered,
idempotent Go loop is declarative in the sense that matters: kill it anywhere, run it again, and it
converges. The boundary is about whether the answer can be *stated*, not about the language it is
stated in.

**Three of the four have known exits**, so the list is expected to shrink rather than hold:
computation can move into something whose job is generating — OpenBao, or the
`passwords.generators.external-secrets.io` already on the cluster; change-triggered restarts can
become a Reloader annotation, which this repo already uses for `keycloak-idp`; adoption gaps are
upstream bugs, not architecture. Discovery is the one that needs a different shape entirely —
events instead of polling, which is roadmap 3.1.

## 6b. Migration log

This is the working record for **roadmap 2.1, Keycloak Provider & Crossplane Consolidation**, which
already described this work and had been waiting on a precondition that is now met: `provider-keycloak`
v2.19.0 ships authentication flows and identity providers, and adopts existing objects by external
name. Progress belongs there; the detail, including what each step verified and what it cost, is here.


Recorded per step, because the procedure changed after the first one.

### 1. Portal public OIDC client — **done**

**Done.** `crossplane/compositions/tenant-default.yaml` declares the `gentian-portal`
`openidclient.keycloak.crossplane.io/Client`. It adopted the live client and changed nothing:
sixteen fields compared against a pre-migration capture, zero differences.

**What the step proved, for reuse:**

- `crossplane.io/external-name` may be the **clientId**. The provider resolves it, then rewrites the
  annotation to Keycloak's internal UUID. That rewrite is stable — checked over two minutes, the
  composition re-rendering `gentian-portal` does not reset it — so one template serves both a tenant
  whose client exists and a tenant whose does not, with no per-tenant import step.
- **Read the spec off the live object, not off the Job's script.** An adopted resource is reconciled
  *toward* the manifest, so a field omitted is reset, not left alone. The values here came from a
  read-only probe's `status.atProvider`.
- **`deletionPolicy: Orphan`** for anything that predates its manifest. Removing a manifest should
  not remove the login client.

**What it changed about the procedure.** The Job is still in place, and had to be put back after
being removed: `keycloak-portal-public-*` does not only create the client. It also maintains an
`openbao-audience` protocol mapper, without which — in the script's own words — *"the tenant's auth
mount refuses every exchange on the audience, no matter who the caller is"*. Deleting the Job after
migrating only the client would have left that mapper unowned on this cluster and absent on the next
tenant, and the failure would have surfaced as OpenBao refusing token exchange rather than as
anything to do with this change.

So the rule for the remaining Jobs is: **inventory everything a Job owns before retiring it**, not
just the object it is named after. A Job is retired when every object it touches has a manifest,
not when the first one does.

**Completed.** The `openbao-audience` `ProtocolMapper` is declared too, referencing the adopted
Client by `clientIdRef` rather than a per-tenant UUID. It adopted the existing mapper — same Keycloak
id, all five config keys unchanged — and only then were the Job, its wait, `keycloak_portal_client.go`
and the test helper's `portal-public` entry removed.

The mapper repeated the lesson in a new shape: its live config carried **five** keys where the Job
set three, because Keycloak adds `introspection.token.claim` and `userinfo.token.claim` on create.
The config map is replaced wholesale on reconcile, so declaring only the three that express the
intent would have stripped the two that Dovecot's XOAUTH2 and OpenBao read. **The intent and the
object are not the same thing; read the object.**

**One loss, recorded rather than dropped.** `keycloak_portal_client_test.go` asserted that a tenant's
own origin is registered as both a redirect and a post-logout redirect URI — without it a tenant
cannot log out back to itself. Its subject is deleted, and the render harness is golden-diff with no
per-case assertion hook, so that property is now only *visible* in the golden rather than asserted.
This repo treats a regenerable golden as insufficient elsewhere for exactly that reason. Restoring it
as a real assertion is outstanding.

### 2. Portal BFF client — **done**

Two objects, and reading the Job instead of the live client would have broken something both times.

**The client is CONFIDENTIAL, so it has a secret — and Keycloak does not own it.** The value lives
in the `gentian-portal-bff` Secret and is pushed in; the portal authenticates with it. The provider's
schema is explicit that an omitted secret *"will be generated by Keycloak"*, so leaving
`clientSecretSecretRef` out would have minted a new one and broken portal login until the Secret
caught up. It references the existing Secret.

**The default scopes were worse.** The Job attaches exactly one, `groups`, because the other six are
Keycloak's own defaults and were already present. The live client carries seven — `acr`, `basic`,
`email`, `groups`, `profile`, `roles`, `web-origins` — and the list is replaced wholesale on
reconcile. Declaring the one the Job names would have stripped profile, email and role claims from
every portal token.

Adoption changed nothing: same client id, thirteen fields compared with zero differences, all seven
scopes intact, and the login round-trip confirmed working before the Job was removed.

**A note on verification limits.** The client secret is the one field a diff cannot check — Keycloak
will not reveal it. It was verified functionally, by signing in, and while the Job was still present
it was re-pushing the correct value on every run. That safety net is what removing the Job gives up,
which is why the round-trip came first.

### 3. Dovecot OIDC client — **adopted; Job deliberately retained**

The client is declared, adopted and verified: same Keycloak id, nine fields compared with zero
differences, and IMAP confirmed working — mail fetched, messages visible.

The inventory was worth doing in the other direction this time. The Job routes through
`buildOIDCPackScript`, the shared builder that can create flows, client scopes, scope mappers, client
roles and default-scope attachments. Dovecot's pack is a `serviceClient`, which routes to a builder
that provisions **only a confidential client** — the catalogue validation enforces it. One object,
not six.

Two values came off the live client rather than the script: `fullScopeAllowed` is **false**, which
the script's `pack.FullScopeAllowed` would have had me guess the other way, and the secret comes from
`dovecot-admin`. Dovecot authenticates to the *introspection* endpoint with it, so an omitted
`clientSecretSecretRef` would have Keycloak mint a new one and IMAP would stop validating tokens
without Dovecot ever failing to start.

**The Job stays, and the tests are why.** Removing it made three unrelated tests fail —
`TestDB_SetsReadyWhenAllDone`, `TestCache_SetsReadyWhenRedisJobsDone`, `TestMariaDB_SetsReadyWhenJobsDone`
— with `DatabaseReady=nil`, meaning the reconcile never got that far. The step being removed did more
than wait for a Job: while it waited, it also held back steps 6 and 7, which write Dovecot's realm
auth config and reload it. That config carries the client secret and introspection URL, so writing it
before the client exists points Dovecot at a client that is not there yet.

On an existing tenant that window is empty, because the client already exists. On a **new** tenant it
is real. The gate was load-bearing and its purpose was not stated anywhere — the Job wait was
incidentally providing ordering.

**What the removal needs first, now established by trying it.** The operator should wait on the
Composition-managed client the way it waited on the Job — reading the `Client` MR's Ready condition
instead of the Job's completion. That much was written and works; what it needs is somewhere to be
tested.

envtest has no `provider-keycloak` CRD, so the wait has no object to read, and neither answer to
"what does a missing CRD mean" is usable there:

- **Treat it as ready** and steps 6 and 7 run anyway — which is exactly the ordering gap the wait
  exists to close, and four readiness tests fail with `DatabaseReady=nil` because the reconcile
  errors before reaching them.
- **Treat it as not ready** and mail waits forever — five tests time out at 180 seconds each.

Waiting is the right semantic for a real cluster: without `provider-keycloak` the Composition cannot
create this client at all, so carrying on would point Dovecot at a client that will never exist, and
a visible `MailReady: waiting` beats a mail stack that looks provisioned and authenticates nobody.

So the missing piece is a test fixture, not a design decision: a stub `clients.openidclient` CRD
registered with envtest, and something that marks the MR Ready — the same shape as the stub
`config/crd/gentianos.io_xtenants.yaml` that Phase 3 added so the reconciler could create XTenant
composites without a live Crossplane. Until that exists the Job stays, because the alternatives are
an ordering gap or a suite that hangs.

**Current state is consistent:** the client is adopted and serving IMAP, and the Job still runs and
agrees with it. Nothing is half-owned.

### 4-6. The broker group — one unit, not three Jobs

`keycloak-broker-idp-*`, `keycloak-kernel-tenant-broker-*` and
`keycloak-broker-first-login-*` are interdependent, so the one-Job-at-a-time rhythm that worked for
the three clients does not apply directly.

| Job | Owns |
|---|---|
| `broker-idp` | broker client in the **kernel** realm, with its secret and protocol mappers; identity provider `kernel` in the **tenant** realm, with mappers |
| `kernel-tenant-broker` | broker client in the **tenant** realm with its secret; identity provider `{tenant}` in the **kernel** realm, with mappers |
| `broker-first-login` | the `first-broker-login-gentian` authentication flow both identity providers point at |

Each of the first two spans **two realms**, and both reference the flow the third creates.

**The dependency is by alias, not by object reference.** The identity providers name
`firstBrokerLoginFlowAlias` as a string, so they can be declared while the flow remains Job-owned.
That is what makes this tractable: the identity providers and broker clients can be adopted now, and
the flow Job left alone, without leaving a dangling reference.

**The secret is declarable, which was not obvious.** The Job reads the broker client's generated
secret back out of Keycloak (`GET clients/{id}/client-secret`) and feeds it into the identity
provider's config. Crossplane has a first-class mechanism for exactly that shape:
`Client.spec.writeConnectionSecretToRef` publishes the secret, and
`IdentityProvider.spec.forProvider.clientSecretSecretRef` consumes it. No imperative read required.

**That corrects §6a.** "Discovery" was listed as inherently imperative, with Keycloak's user list as
the example. Reading a value a *managed resource produced* is not discovery in that sense — it is
what connection secrets are for. The line is narrower than stated: enumerating state **nobody
declared** is imperative; reading a managed resource's own output is not. The user-enumeration case
still stands, because those users are created by people, not by a resource anyone declared.

Adoption verified read-only against the live `kernel` identity provider in the corp realm: Ready,
31 fields observed, with both endpoint URLs.

**The probe also found a live defect, which is the strongest argument for declaring these.**
`firstBrokerLoginFlowAlias` on that identity provider is not stable. Sampled three times a minute
apart it read `first broker login` — Keycloak's built-in — twice, then `first-broker-login-gentian`.
The Job completion times explain the shape: `broker-first-login` finished at 05:02:30 and
`broker-idp` did not run until 05:03:12, so there is a window of roughly a minute in every reconcile
cycle where the tenant's brokered first login uses Keycloak's default flow rather than the Gentian
one that auto-links accounts.

Two Jobs own one field between them with no ordering, so which value is live depends on which ran
last. The exact mechanism is not pinned down — most likely the flow being recreated invalidates the
reference and Keycloak falls back — but the instability itself is measured, not inferred.

Declaring the identity provider fixes this by construction rather than by fixing a bug: one owner,
one value, and a reference that Crossplane reconciles back if anything moves it. It is worth doing
for that reason alone, ahead of the Job removals.

### Then: 7. Gentian groups

Finish by replacing the Job wait with an MR readiness wait, then retire the Job. After that,
`keycloak-gentian-groups-*`, which fans out over installed app profiles and so needs the composition
to read AppProfiles rather than a fixed list.

## 7. Honest answer to the question

There is no reason not to migrate **most** of it, and a clear reason not to migrate **all** of it.

The four items in §5 have to stay imperative, and they cluster in one place: per-user mail
credentials and DKIM. Everything else — identity, namespace shell, network policy, database and
cache setup — is expressible with providers already installed.

That means the end state is a hybrid either way. The question is only where the boundary sits:
today it sits roughly where Phase 3b stopped; after this work it would sit at *declare versus
compute*, which is a line someone can reason about without reading git history.

## 8. If this is taken up

- `[ ]` Decide whether the boundary is worth moving at all — the current split works, and churn in
  tenant provisioning is expensive to get wrong.
- `[ ]` Migrate the Keycloak Jobs to `provider-keycloak` resources, one kind at a time, keeping the
  `waitForProvisioningJob` observation until each is proven.
- `[ ]` Move the fourteen Kubernetes-object writers to the Composition, which is mechanical.
- `[ ]` Leave §5 in Go, and say so in `docs/architecture.md` so the boundary is documented rather
  than inferred.
- `[ ]` Generate or assert the seven fields `XTenant` mirrors from the `Tenant` CRD. Nothing checks
  that mirror today; it is how `mail.quotaPerUser` and `mail.rateLimit` came to exist in both
  schemas while nothing read either.

## 9. Measured

Taken from `develop` on 2026-08-20, against the `ifk-w4h` cluster.

```
ensure* steps on TenantReconciler        63
  waits on a Crossplane-owned Job        18
  writes Kubernetes objects              14
  dispatch / thin wrappers               25
  direct external API call                1
  computes key material                   2
  enumerates external state               1   (syncMailAppPasswords)

provider-keycloak                        installed, healthy, covers 32 API groups
  live managed resources                  3  (2 app OIDC clients + 1 default scope)
  adoption by external-name               verified on the live corp realm (Observe)
Keycloak Jobs for one tenant (corp)       9
resources tenant-default composes        ~10
```

## 7. Adoption edge: observable but not declarable

Adopting a live object assumes its state can be expressed as a spec. The portal
BFF client is the counter-example, and it is worth stating as a rule.

Keycloak accepts and stores redirect URIs on a client with both redirect flows
disabled. provider-keycloak, validating the same object, refuses to write that
combination. The state is therefore readable, real, and in production use, but
not declarable.

Two layers hide this. Upjet late-initialisation copies unset optional fields out
of the observed object into `spec.forProvider`, so a template that omits the
offending fields still ends up with them; omitting `LateInitialize` from
`managementPolicies` stops that. But the update still fails, because Upjet plans
from the observed object — so no spec on this side can make the write legal. The
live object must change first.

The practical consequences for the remaining migrations:

- Adoption is not verified by `Ready=True`. That only says the object exists.
  `Synced=True` is the condition that says the manifest is actually in force. A
  resource can sit `Ready=True, Synced=False` indefinitely, looking healthy while
  the manifest is inert.
- Check for provider validation rules the live object already violates before
  adopting, not after. Jobs write through the Admin API, which enforces less than
  the Terraform schema the provider validates against, so anything a Job created
  may carry a state the provider will not accept.
- When the live object cannot be made legal without a behaviour change, park it
  `Observe`-only with the reason recorded, rather than leaving a resource that
  retries a doomed update forever.

## 8. The broker group

Converted so far, and what each step established.

### What is declared

| Resource | Policy | Why |
|---|---|---|
| `Client broker-<tenant>` (kernel realm) | `Observe` | `writeConnectionSecretToRef` republishes the existing secret without rotating it. That is the whole point: it is what lets the IdP take credentials from a Secret instead of a Job curling the admin API for them. Managing it would put the secret under the provider's control, and a rotation would break brokered login for as long as the IdP still held the old value. |
| `IdentityProvider kernel` (tenant realm) | `Observe` | Adopted and verified against the live object. Not yet managed — see below. |

Both are gated on `identity.kernelRealm` and `identity.internalUrl` being
present, and stay unrendered rather than half-configured when they are not.

### What unblocked it

The IdP's endpoints need Keycloak's in-cluster URL, which lived only in the
`keycloak-admin` Secret. Compositions cannot read Secrets, so the broker could
not be declared at all. It is now published through the claim as
`identity.internalUrl`, deliberately distinct from `gentian-kernel-services`'
`KEYCLOAK_INTERNAL_URL`, which is documented as a base with **no** path. The two
live values already disagree on exactly that `/auth` suffix, so collapsing them
would hand one consumer the other's contract.

### Two writers, one field

`identity_reconciler.go`'s realm script and the broker-idp Job both write this
IdP, and they disagreed about `firstBrokerLoginFlowAlias`: the realm script set
the built-in `first broker login`, the Job sets the gentian flow. The Job runs
later, so the gentian flow is what is live — but a realm re-run would silently
put every tenant's first login back on a flow that stops to ask the user to
confirm the link, instead of matching them to the account already provisioned
for them by email.

The realm script now preserves whatever alias is already in place and uses the
built-in one only to bootstrap an IdP that does not exist yet. It cannot simply
stop writing the IdP: the broker-idp Job requires it to already exist and exits
non-zero otherwise.

### What blocks full management

The provider does not observe three keys the live object carries:
`useJwksUrl`, `defaultScope` and `updateProfileFirstLoginMode`. They are absent
from `status.atProvider` and from `extraConfig`, but present in the object read
straight from the Admin API.

That matters because §7's rule cuts both ways. An unobserved field is one the
provider may not preserve on write, and these three are not cosmetic:
`useJwksUrl` is what makes signature validation use the JWKS endpoint, and
`defaultScope` is `openid profile email` — dropping it to the provider's default
of `openid` costs every brokered login its name and email claims.

So promoting to management means declaring all three explicitly and confirming
against the Admin API that a write preserved them. It is a change to the live
login path of every tenant user, on a cluster with no staging equivalent, and it
is the one step here that cannot be verified before making it.
