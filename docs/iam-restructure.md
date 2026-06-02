# IAM Restructure — Tenant-Realm-First Architecture

## Status: Phase A ✅ complete — Phase B ✅ complete — Phase C ✅ complete

See [design/ldap-restructuring.md](design/ldap-restructuring.md) for the full
LDAP audit, defect list, and step-by-step implementation plan.

---

## 1. Current state and its problem

**Phase A (realm rename opendesk→kernel) is done.** The current cluster has:

```
master          ← admin only
kernel          ← renamed from opendesk; LDAP federation: dc=swp-ldap,dc=internal SUBTREE ❌
                  clients: portal, opendesk-dovecot, opendesk-nextcloud, opendesk-oxappsuite
                  users: ALL users from ALL tenants imported here (federation bug)
gtn-demo        ← LDAP federation: ou=users,ou=gtn-demo (correct path, but EMPTY) ❌
                  clients: gtn-demo-element, gtn-demo-jitsi, gtn-demo-ox-appsuite
gtn-demo-2      ← LDAP federation: ou=users,ou=gtn-demo-2 (correct path, but EMPTY) ❌
                  clients: gtn-demo-2-element
```

Two critical bugs remain:
1. **kernel realm imports all tenant users** (LDAP scope is the full tree). UMC runs in the
   kernel realm, so tenant admins see every user from every tenant and can select any tenant's
   container in the user-creation wizard.
2. **Tenant realm federation is empty** — users are placed at `ou=<tenant>` root by the
   reconciler, but federation points to `ou=users,ou=<tenant>` which has no objects. SSO for
   all tenant-realm OIDC clients is therefore broken.

---

## 2. Philosophy (unchanged)

**User identity and application registrations belong in the same realm.**

Each tenant has exactly one Keycloak realm. That realm is both:
- the **identity namespace** (users, AI agents, service accounts)
- the **application namespace** (OIDC clients for every app the tenant has installed)

The kernel realm is scoped to kernel service accounts only — no human users belong there.

```
master          ← admin only (unchanged)
kernel          ← kernel services only
                  LDAP: cn=users,dc=swp-ldap,dc=internal (one-level, service accounts only)
                  clients: portal, opendesk-nextcloud, opendesk-dovecot, opendesk-oxappsuite
                  users: none (kernel service accounts are not imported as human users)
<tenant>        ← one per tenant (e.g. gtn-demo)
                  LDAP: ou=users,ou=<tenant>,dc=swp-ldap,dc=internal (one-level)
                  users: all tenant users (admin + regular)
                  OIDC clients: <tenant>-<app> for every installed app
                  UMC client registered here so tenant admins see only their own realm
```

---

## 3. Target architecture

### 3.1 LDAP structure (target — per [design/ldap-restructuring.md](design/ldap-restructuring.md))

The LDAP tree must follow the **one OU = one realm = one namespace** rule. All human users
for a tenant belong inside the `ou=users` sub-container of the tenant OU. Service accounts
and UDM groups stay at the tenant OU root.

```
dc=swp-ldap,dc=internal
├── cn=users                     ← kernel service accounts only
├── cn=groups                    ← kernel-level groups only (no cross-tenant user membership)
│
└── ou=<tenant>                  ← one per tenant
    ├── ou=users                 ← ALL human users (admin + regular users)
    │   ├── uid=admin-<tenant>   ← tenant admin (previously at OU root — must be moved)
    │   └── uid=<username>       ← regular users
    ├── uid=app-keycloak-<tenant>  ← Keycloak LDAP federation bind account
    ├── uid=app-<app>-<tenant>     ← per-app service bind accounts
    ├── cn=users_<tenant>          ← UDM group: all tenant users
    ├── cn=admins_<tenant>         ← UDM group: tenant admins
    └── cn=managed-by-attribute-*  ← per-tenant app access groups
```

Key changes from current state:
- `uid=admin-<tenant>` moves from `ou=<tenant>` root into `ou=users,ou=<tenant>`
- Regular users created via UMC now land in `ou=users,ou=<tenant>` (fixed via `settings/directory` default container)
- `cn=Domain Users` (cross-tenant) is removed; `cn=users_<tenant>` per tenant replaces it
- `managed-by-attribute-*` groups move inside `ou=<tenant>` (per-tenant isolation)

### 3.2 Keycloak realm per tenant — with LDAP federation

The realm provisioning job (`buildRealmScript`) currently creates only the realm record.
It must be extended to also register a Keycloak LDAP User Storage Provider scoped to the
tenant OU:

```
POST /admin/realms/{tenant}/components
{
  "name": "ldap",
  "providerId": "ldap",
  "providerType": "org.keycloak.storage.UserStorageProvider",
  "config": {
    "connectionUrl":  ["ldap://nubus-ldap.{kernel-ns}.svc.cluster.local:389"],
    "usersDn":        ["ou={tenant},{ldap-base}"],
    "bindDn":         ["uid=sys-keycloak-{tenant},{bind-base}"],
    "bindCredential": ["{ldap-bind-secret}"],
    "searchScope":    ["1"],          // one level — own OU only
    "importEnabled":  ["true"],
    "fullSyncPeriod": ["-1"]
  }
}
```

The LDAP bind account for Keycloak (`sys-keycloak-{tenant}`) is already provisioned by
`buildBindAccountScript` in `ldap_reconciler.go`; it just needs to be declared as a
`kernelRequirements.identity.ldap` entry on the "kernel-keycloak" internal consumer so
the reconciler creates the `users/ldap` object and seeds the password into OpenBao.

### 3.3 Kernel realm (renamed from `opendesk`)

The `opendesk` realm is renamed `kernel` everywhere:
- Nubus configmap: `KEYCLOAK_REALM: kernel`
- `identity_reconciler.go`: `buildOpendeskAdminEnableScript` / `buildRealmDisableScript`
  use the hardcoded string `"opendesk"` → replaced by `r.KernelRealm` (new field)
- `update.sh`: `_trigger_keycloak_ldap_sync` hardcodes `kc_realm="opendesk"` → `"kernel"`
- `install.sh`: comment references to `opendesk_standard` LDAP profile remain (those are
  UDM attribute names from Univention, not our realm name — no change needed)
- `install.env.template`: add optional `KERNEL_REALM` variable (default: `kernel`)

### 3.4 Tenant admin enable/disable flow

The `ensureOpendeskAdminEnableJob` currently re-enables the tenant admin user in the
`opendesk` realm after the LDAP shadowExpire race. With the rename, this job targets
the `kernel` realm (same logic, new realm name via `r.KernelRealm`).

Once per-tenant LDAP federation is in place, tenant admins live in the tenant realm, not
the kernel realm. The enable job will target the tenant realm directly and no longer
need to address the kernel realm at all. This is a follow-up simplification, not part
of this plan.

---

## 4. Implementation plan

### Phase A — Rename `opendesk` → `kernel` ✅ COMPLETE

Realm is named `kernel` in the live cluster. All code references have been updated.

---

### Phase B — Fix LDAP structure + per-tenant federation ✅ COMPLETE

See [design/ldap-restructuring.md](design/ldap-restructuring.md) for the full
step-by-step plan. Summary:

**B.1 — Fix user placement (ldap_reconciler.go)** ✅

- `buildAdminUserScript`: admin placed in `ou=users,ou=<tenant>` ✓
- `buildTenantOUScript`: `settings/directory` default container set to `ou=users,ou=<tenant>` ✓
- Stale `uid=app-keycloak` (no tenant suffix) creation removed ✓

**B.2 — Fix kernel realm LDAP scope (identity_reconciler.go)** ✅

- Kernel realm LDAP federation now uses `usersDn: cn=users,dc=swp-ldap,dc=internal`
  with `searchScope: 1` (one-level); tenant users no longer appear in the kernel realm ✓

**B.3 — Register UMC OIDC client in each tenant realm (identity_reconciler.go)** ✅

- `ensureRealmJob` seeds the UMC OIDC client secret and passes it to `makeRealmJob` ✓
- Realm provisioning script registers the UMC portal OIDC client per tenant ✓

**B.4 — Add per-tenant managed-by-attribute groups (ldap_reconciler.go)** ✅

- `buildTenantOUScript` creates `cn=managed-by-attribute-*` groups inside `ou=<tenant>`
  rather than the global `cn=groups` container ✓

**B.5 — Fix OX connector LDAP scope** ✅

- `ox-appsuite.yaml` kernelRequirements `ldap.sync` scoped to `ou=users,ou=<tenant>` ✓

**B.6 — Remove `ensureOpendeskAdminEnableJob`** ✅

- Tenant admins live in the tenant realm via LDAP federation; kernel-realm re-enable
  job removed ✓

---

## 5. Files changed — summary

| File | Phase | Change |
| --- | --- | --- |
| `internal/controller/identity_reconciler.go` | A | Add `KernelRealm` field; replace 4 hardcoded `"opendesk"` strings; rename job name function |
| `internal/controller/identity_reconciler_test.go` | A | Update 2 job name assertions |
| `cmd/main.go` | A | Wire `KERNEL_REALM` env var into reconciler |
| `charts/gentian-os/templates/deployment.yaml` | A | Add `KERNEL_REALM` env var |
| `charts/gentian-os/values.yaml` | A | Add `kernelRealm: ""` |
| `install.env.template` | A | Add commented `KERNEL_REALM=kernel` |
| `install.sh` | A | Pass `KERNEL_REALM` to envsubst; update banner |
| `update.sh` | A | Replace hardcoded `"opendesk"` in `_trigger_keycloak_ldap_sync` |
| Nubus bootstrap ConfigMap | A | `KEYCLOAK_REALM: opendesk` → `KEYCLOAK_REALM: kernel` |
| `docs/design/multi-tenancy.md` | A | Update realm name references |
| `docs/implementation-plan.md` | A | Update realm name references in identity sections |
| `internal/controller/identity_reconciler.go` | B | Extend `buildRealmScript` with LDAP federation call; wire new env vars into Job spec |
| `internal/controller/ldap_reconciler.go` | B | Add `keycloak` as bind account consumer; add `ou=users` sub-OU creation to `buildOUScript` |
| `api/v1alpha1/appprofile_types.go` | B | Add `PortalTiles` field for portal entry provisioning |
| `api/v1alpha1/tenant_types.go` | B | Extended tenant app config |

---

## 6. Migration path for existing tenants

Phase A (rename) requires a one-time manual action for any live cluster:

1. In Keycloak admin UI: rename realm `opendesk` → `kernel` (Settings → Realm name).
2. Update the Nubus `keycloak-bootstrap` ConfigMap: `KEYCLOAK_REALM: kernel`.
3. Restart the Nubus keycloak-bootstrap job to re-sync.
4. Delete and re-run the `keycloak-opendesk-enable-*` Jobs for all tenants (they will
   be recreated with the new name `keycloak-kernel-enable-*` by the next reconcile).

Phase B (LDAP federation) for existing tenants: the reconciler is idempotent. After
deploying the new code, trigger a reconcile (`kubectl gentian tenants deploy {tenant}`)
for each tenant. The realm job will detect the existing realm (HTTP 200) and only add
the LDAP federation component if absent.

---

## 7. Phase C — Dynamic app-rights provisioning (design)

### 7.1 Problem

Every infrastructure service that needs a per-app credential today requires a manual
entry in **four places before** the app can be installed on any tenant:

| What | Files requiring manual change |
| --- | --- |
| `ldapsearch_<app>` LDAP read user | `seed-openbao.sh`, `install.sh`, `crossplane/apps/nubus/externalsecrets.yaml` |
| MinIO/S3 user | `seed-openbao.sh` |
| PostgreSQL user | `seed-openbao.sh` |
| App-specific kernel secrets | `seed-openbao.sh` |

This breaks the "open app store" model: adding an app like Odoo requires a kernel change
and cluster re-provision before any tenant can install it. The credential set is decided
at install time, not at app-install time.

### 7.2 Root cause

All three patterns share the same structural problem: the credential is **generated once
at cluster install** (HMAC-derived from the master password) and **placed into a shared
secret store** (OpenBao) that nubus reads at Helm deploy time to create a static list of
LDAP users / MinIO buckets / Postgres roles. The list is closed.

### 7.3 Target: on-demand credential provisioning via init Jobs

The fix is to move credential creation from `install.sh` / nubus Helm values into a
**Crossplane Composition init Job** that runs the first time a tenant installs an app.
This is already the pattern used by `ox-connector-provisioning-subscription` — extend it
to credential bootstrap.

#### LDAP search users (most impactful)

**Today:** nubus Helm values include a static `ldapSearchUsers` list that creates all
`ldapsearch_*` LDAP accounts at nubus deploy time.

**Target:**

1. Remove all app-specific entries from the `ldapSearchUsers` list in
   `externalsecrets.yaml`. Keep only `ldapsearch_keycloak` (kernel-internal).
2. Each AppProfile that needs LDAP search declares `kernelRequirements.ldap.search: true`.
3. The `app-default` Composition (or a dedicated `app-ldapsearch-init` Job template)
   generates a random password, stores it at
   `gentian-os/tenants/<tenant>/apps/<app>/ldap-search` in OpenBao, then calls the UDM
   REST API to create `uid=ldapsearch_<tenant>-<app>,ou=<tenant>,dc=swp-ldap,dc=internal`
   with that password. The Job is idempotent (checks for existence first).
4. The ExternalSecret for the app's Helm release reads from
   `gentian-os/tenants/<tenant>/apps/<app>/ldap-search` — same path as all other
   per-tenant app secrets.

This makes LDAP search users tenant-scoped and app-scoped (not kernel-wide), which also
improves isolation: an app can only search its own tenant's `ou=users`.

#### MinIO users

**Today:** `seed-openbao.sh` derives and writes `minio_<app>_password` for a fixed set
of apps. Nubus provisions the MinIO users at cluster deploy time.

**Target:**

1. The `app-default` Composition already creates a per-tenant S3 bucket (when
   `kernelRequirements.storage.s3.bucketPerTenant: true`). Extend it to also create the
   MinIO service account credential on first deploy via an init Job calling the MinIO
   admin API, storing the result in `gentian-os/tenants/<tenant>/apps/<app>/s3`.
2. Remove per-app MinIO credential derivation from `seed-openbao.sh`.

This requires a MinIO admin credential available to the init Job — already present in
OpenBao at `gentian-os/kernel/minio`.

#### PostgreSQL users

**Today:** `seed-openbao.sh` derives `pg_<app>_password` for a fixed app list.

**Target:**

1. The `app-default` Composition already creates per-tenant databases. Extend it to
   create the Postgres role inline via an init Job (using `psql` or the CNPG managed
   role API), storing credentials in `gentian-os/tenants/<tenant>/apps/<app>/database`.
2. Remove per-app pg credential derivation from `seed-openbao.sh`.

### 7.4 What stays kernel-level

Some credentials legitimately belong in the kernel and cannot be made per-tenant:

| Credential | Reason stays kernel |
| --- | --- |
| `ldapsearch_keycloak` | Used by Keycloak itself (kernel service) to federate the tenant OU — provisioned before any tenant exists |
| `ldapsearch_postfix` / `ldapsearch_dovecot` | Mail infrastructure is a kernel service shared across all tenants |
| NATS passwords | nubus internal bus — kernel service |
| Keycloak admin password | kernel service admin |
| MariaDB / Postgres kernel passwords | kernel DB instances |

### 7.5 Migration path

The change is fully backward-compatible for existing apps:

1. For each existing app's `ldapsearch_<app>` user: write a one-off migration Job that
   reads the existing HMAC-derived credential from `gentian-os/kernel/nubus/<path>`,
   copies it to `gentian-os/tenants/<tenant>/apps/<app>/ldap-search`, and marks the old
   key deprecated.
2. Remove the app-specific entries from `externalsecrets.yaml` and `seed-openbao.sh`
   once all tenants have been migrated (the nubus Helm release will reconcile and delete
   the LDAP users on its next apply — tolerable since new ones are created by the init Jobs).
3. New apps (e.g. Odoo) never touch `install.sh` or `seed-openbao.sh` — they simply
   declare `kernelRequirements` in their AppProfile and let the Composition handle
   credential bootstrap.

### 7.6 Files to change (Phase C)

| File | Change |
| --- | --- |
| `gentian-os/scripts/seed-openbao.sh` | Remove per-app `ldapsearch_*`, `minio_*`, `pg_*` entries; keep kernel-only credentials ✅ |
| `gentian-os/install.sh` | Remove per-app `_derive` args from the nubus secret JSON block (see §7.7 below) ✅ |
| `gentian-os/crossplane/apps/nubus/externalsecrets.yaml` | Remove app-specific `ldapSearchUsers` entries; keep `ldapsearch_keycloak` and mail services ✅ |
| `gentian-os/crossplane/compositions/app-default.yaml` | Add `ldap-search-init`, `s3-init`, `database-init` Job templates gated on profile flags ✅ |
| `gentian-apps/profiles/*.yaml` | Replace `kernelRequirements.ldap.sync` with typed `ldap.search` / `ldap.sync` flags per profile ✅ |
| `gentian-os/install.env.template` | Add `SECRET_MODE=derived` with a comment explaining the options ✅ |

---

### 7.7 `install.sh` — what stays vs what leaves

**What must remain in `install.sh` after Phase C:**

| Block | Lines (approx.) | Reason |
| --- | --- | --- |
| Master password prompt / validation | top of file | Entry point for `secretMode=derived` derivation |
| `seed-openbao.sh` invocation | kernel bootstrap | Seeds all kernel-level credentials into OpenBao before any app is installed |
| Nubus Helm install / upgrade | main deploy | Nubus must be up before any app Composition init Job can call UDM REST API |
| `ldapsearch_keycloak` credential | via `seed-openbao.sh` | Keycloak binds LDAP before tenants exist — cannot be deferred |
| `ldapsearch_postfix` / `ldapsearch_dovecot` | via `seed-openbao.sh` | Mail infrastructure is a kernel service, not per-tenant |
| All kernel DB / MinIO / Redis / NATS / Keycloak passwords | via `seed-openbao.sh` | These are infrastructure services, not app-level |
| Applying the kernel Crossplane Compositions (`cluster-default`, `tenant-default`) | Composition apply block | These set up the tenant lifecycle, not individual apps |

**What must be removed from `install.sh` after Phase C:**

| Block | Lines (approx.) | What replaces it |
| --- | --- | --- |
| `app-ox.yaml` Composition `kubectl apply` | ~243–245 | App install is triggered by creating an `AppInstall` CR, not by `install.sh` |
| `gentian-cluster-config` ConfigMap upsert (LDAP server/baseDn/bindDn for OX) | ~613–635 | LDAP coordinates moved into the `app-ox` Composition's parameter block; Composition reads them from the Tenant CR |
| Per-app `ldapsearch_*` values in nubus secret JSON (`ldapsearch_ox`, `ldapsearch_nextcloud`, `ldapsearch_element`, `ldapsearch_openproject`, `ldapsearch_xwiki`) | ~499–524 | Each app's Composition init Job creates its own LDAP search user on first install |
| Per-app MinIO user derivations passed as nubus secrets (`minio_nextcloud_*`, `minio_openproject_*`, `minio_dovecot_*`) | `seed-openbao.sh` | MinIO init Job in each app Composition creates the bucket + service account on first install |
| Per-app Postgres user derivations (`pg_nextcloud_*`) | `seed-openbao.sh` | Database init Job in each app Composition creates the role on first install |

**Rule of thumb:** `install.sh` is the kernel bootstrap script. If a credential or resource belongs to a specific app, it has no place in `install.sh`. The test: *"Would the cluster function correctly without this line if no tenants had installed this app?"* If yes, remove it.

---

### 7.8 Secret generation mode (`secretMode`)

Dynamic init Jobs (§7.3) need to generate credentials at app-install time. Two modes are
supported, selectable via `SECRET_MODE` in `install.env`:

| Mode | Value | How credentials are generated | Recovery without backup |
| --- | --- | --- | --- |
| **Deterministic** (default) | `derived` | `HMAC-SHA256(master_password, "<tenant>:<app>:<purpose>")` | ✅ Re-run seeder with same master password |
| **Random** | `random` | `openssl rand -hex 32` at Job runtime | ❌ Requires OpenBao backup |

**`derived` mode:** init Jobs read the master password from
`gentian-os/kernel/master-password` in OpenBao (written there once by `seed-openbao.sh`
at cluster install) and call the same HMAC derivation function:

```bash
derive_password "${TENANT}:${APP}" "ldap-search"
```

This keeps the same disaster-recovery guarantee as kernel credentials: lose OpenBao
entirely, re-seed from master password, all credentials are restored.

**`random` mode:** init Jobs generate a fresh random credential and write it to OpenBao.
No master password is involved at app-install time. This is the correct choice for
deployments with a reliable OpenBao backup strategy or where SOC 2 / ISO 27001 compliance
is a goal, because credentials can be independently rotated per app without affecting others.

**Hash function note:** the current `seed-openbao.sh` implementation pipes
`HMAC-SHA256 | sha1sum`. The `sha1sum` step is unnecessary and weakens the output: an
attacker who obtains one derived credential can run an offline dictionary attack against
the master password at SHA-1 speed (very fast on GPU). The correct implementation is:

```bash
# Remove the sha1sum pipe — use raw HMAC-SHA256 hex directly
derive_password() {
  echo -n "${1}:${2}" \
    | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" \
    | awk '{print $2}'     # 64-char hex, no sha1sum
}
```

This change is backward-incompatible (all derived passwords change), so it must be done
together with the Phase C migration — the cluster re-seed is required anyway.

**Rotation:** `secretMode=derived` does not support independent per-app credential
rotation (changing one app's credential requires changing the master password, which
rotates everything). For regulated deployments requiring periodic rotation, use
`secretMode=random` and annotate the tenant CR:

```bash
kubectl annotate tenant gtn-demo gentian-os.io/rotate-credentials=<app-name>
```

The `random` + operator-triggered rotation path satisfies SOC 2 Type 1. Scheduled
automatic rotation (SOC 2 Type 2) is a future phase and requires a CronJob plus
Stakater Reloader coordination.
