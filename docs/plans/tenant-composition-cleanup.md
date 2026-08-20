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
   not in any spec, so the credential minted per user cannot be rendered from one.
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
