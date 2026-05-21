# IAM Restructure — Tenant-Realm-First Architecture

## Status: Draft — awaiting review before implementation

---

## 1. Current state and its problem

Today Gentian OS has three Keycloak realms with an awkward responsibility split:

```
master          ← admin only (unchanged)
opendesk        ← all users (Nubus LDAP sync target); kernel apps + tenant admin users
<tenant>        ← OIDC clients for tenant apps, but zero users
```

The `opendesk` realm is a Nubus artefact: `KEYCLOAK_REALM=opendesk` is hardcoded in the
Nubus configmap, so Nubus's LDAP→Keycloak sync lands all users there regardless of which
tenant they belong to. Tenant realms are structurally empty (just an OIDC client) and have
no way to issue tokens for tenant users without a broker to `opendesk`.

This has two concrete consequences:
1. A user logged into the Nubus portal (`opendesk` session) gets a new login prompt when
   opening a tenant app, because the tenant app's realm is different.
2. The current design makes adding LDAP-federation-per-tenant realm necessary in order to
   fix this — but that is not implemented, so SSO is currently broken for all tenant apps.

---

## 2. New philosophy

**User identity and application registrations belong in the same realm.**

Each tenant has exactly one Keycloak realm. That realm is both:
- the **identity namespace** (users, AI agents, service accounts)
- the **application namespace** (OIDC clients for every app the tenant has installed)

The `opendesk` realm is renamed `kernel`. Its scope is narrowed to kernel services only:
Nubus portal, OX App Suite, Nextcloud, Dovecot, Intercom. It no longer houses tenant users
or tenant OIDC clients.

```
master          ← admin only (unchanged)
kernel          ← kernel services only (renamed from opendesk)
                  clients: portal, opendesk-nextcloud, opendesk-dovecot, opendesk-oxappsuite
                  users: kernel admins only
<tenant>        ← one per tenant (e.g. gtn-demo)
                  users: all tenant users (LDAP federation scoped to ou=<tenant>)
                  AI agents: LDAP service accounts under ou=agents,ou=<tenant>
                  OIDC clients: gtn-demo-element, gtn-demo-nextcloud, …
shared-apps     ← one realm for all shared app instances (optional, only when shared mode is used)
                  clients: element, nextcloud, … (one per app type)
```

### 2.1 Dedicated vs. shared deployment mode

Apps can be deployed in one of two isolation modes, declared **per tenant per app**:

| Mode | Description | IAM topology |
|---|---|---|
| `dedicated` (default) | One deployment per tenant in its own namespace | OIDC client lives in tenant realm; no broker needed |
| `shared` | One shared deployment serving all tenants | OIDC client lives in the `shared-apps` realm; tenant realm brokers into it |

This flag is declared on the AppInstance in the Tenant CR:

```yaml
spec:
  apps:
    - profile: element
      isolationMode: dedicated   # optional; overrides AppProfile default
    - profile: nextcloud
      isolationMode: shared      # free tier
```

AppProfile declares which modes are supported and what the default is:

```yaml
spec:
  isolation:
    modes: [shared, dedicated]
    default: dedicated
```

---

## 3. Target architecture

### 3.1 LDAP structure (minor extension)

The LDAP tree is already per-tenant-OU:

```
dc=swp-ldap,dc=internal
├── ou=gtn-demo                  ← tenant OU (provisioned by ldap_reconciler.go)
│   ├── cn=users_gtn-demo
│   ├── cn=admins_gtn-demo
│   ├── uid=admin-gtn-demo       ← tenant admin
│   ├── uid=gtn-demo-test        ← regular user (created via UDM)
│   └── ou=agents                ← new: AI agent service accounts
│       └── uid=agent-xyz
└── ou=gtn-demo-2
    └── …
```

AI agents are principals like any other. They belong in LDAP under an `ou=agents`
sub-container of the tenant OU, provisioned as `users/ldap` service account objects via
UDM (the same object class used for per-app bind accounts today). This keeps the LDAP
tree the single authoritative source of all principals for a tenant, and lets the
Keycloak LDAP federation import agents alongside human users.

The future nice-to-have `ou=tenants` parent container is a separate, longer-term concern
and is not part of this plan.

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

### Phase A — Rename `opendesk` → `kernel` (no behaviour change)

All changes in this phase are pure renames. Existing tenants continue to work.

**A.1 `identity_reconciler.go`**

- Add `KernelRealm string` field to `TenantReconciler` struct (alongside `KernelDomain`),
  defaulting to `"kernel"`.
- Replace the two hardcoded `"opendesk"` strings in `buildOpendeskAdminEnableScript`
  (lines 609, 614, 616, 618) with `%s` format args supplied by `r.KernelRealm`.
- Replace the two hardcoded `"opendesk"` strings in `buildRealmDisableScript`
  (lines 775, 780, 782, 784) with `%s` format args supplied by `r.KernelRealm`.
- Rename `opendeskAdminEnableJobName()` → `kernelAdminEnableJobName()` and update its
  return value from `"keycloak-opendesk-enable-%s"` → `"keycloak-kernel-enable-%s"`.
- Update all call sites and comments that reference `opendesk realm`.

**A.2 `cmd/main.go`**

- Add `KernelRealm: os.Getenv("KERNEL_REALM")` to the `TenantReconciler` initialiser,
  with a fallback to `"kernel"` when the env var is empty.

**A.3 `charts/gentian-os/templates/deployment.yaml`**

- Add env var `KERNEL_REALM` sourced from `{{ .Values.kernelRealm | default "kernel" | quote }}`.

**A.4 `charts/gentian-os/values.yaml`**

- Add `kernelRealm: ""` (empty = use default `"kernel"`).

**A.5 `install.env.template`**

- Add commented-out `# KERNEL_REALM=kernel` entry in the Core cluster/domain section.

**A.6 `install.sh`**

- In `apply_cluster_xr`: pass `KERNEL_REALM=${KERNEL_REALM:-kernel}` as an additional
  `envsubst` variable (alongside the existing domain/DN variables).
- Update the comment on line 1083–1088 (stale `opendesk_standard` profile) — these
  references are to UDM attribute names, not the realm, so add a clarifying comment
  only; do not change the logic or the LDAP path.
- Update the summary banner to print `Kernel realm  : ${KERNEL_REALM:-kernel}`.

**A.7 `update.sh`**

- In `_trigger_keycloak_ldap_sync`: change `local kc_realm="opendesk"` →
  `local kc_realm="${KERNEL_REALM:-kernel}"`.
- Update the `--keycloak-sync` help text and the `op_keycloak_sync` banner.

**A.8 Nubus configmap**

- In the `gentian-os` Helm chart (or the Crossplane composition that manages Nubus
  bootstrap config), update `KEYCLOAK_REALM: opendesk` → `KEYCLOAK_REALM: kernel`.
  Locate the exact resource: `nubus-dev-keycloak-bootstrap` ConfigMap in the kernel
  namespace. The configmap is either:
  - templated in `charts/gentian-os/templates/` → update the template value, or
  - generated by a Crossplane composition → update the composition.

**A.9 `identity_reconciler_test.go`**

- Update the two test job name assertions that expect `"keycloak-opendesk-enable-*"` →
  `"keycloak-kernel-enable-*"`.

**A.10 `docs/`**

- `docs/design/multi-tenancy.md`: update any realm name references.
- `docs/implementation-plan.md`: update "opendesk realm" references in identity sections.
- `docs/commands.md`: no realm references (existing `opendesk` references are to chart/OCI
  image names, not realm names — add a clarifying note only).

---

### Phase B — Per-tenant LDAP federation in tenant realm

This phase wires users into the tenant realm so SSO works without a broker.

**B.1 New LDAP bind account for Keycloak**

Add a `kernelRequirements.identity.ldap` consumer entry named `keycloak` to the internal
kernel manifest. This causes `ldap_reconciler.go` to call `buildBindAccountScript` and
create `uid=sys-keycloak-{tenant},…` in LDAP, seeding the password into OpenBao at
`gentian-os/tenants/{tenant}/apps/keycloak/ldap-bind-password`.

**B.2 Extend `buildRealmScript` with LDAP federation registration**

After the realm `POST`/`PUT`, append a curl call to register the LDAP User Storage
Provider in the tenant realm (see §3.2). The script must:
- Read the LDAP bind password from the env var `LDAP_BIND_PASSWORD` (injected from the
  OpenBao-backed ExternalSecret, same pattern as other job env vars).
- Set `usersDn` to the tenant OU DN (passed via env var `TENANT_OU_DN`, already
  available in the job via `tenantOUDN(tenant)`).
- Be idempotent: check if a provider named `ldap` already exists in the realm before
  creating a new one.

**B.3 Wire new env vars into `ensureRealmJob`**

The Job spec for the realm provisioning job must be extended with two additional env vars:
- `LDAP_BIND_PASSWORD` — from the ExternalSecret for the Keycloak bind account.
- `TENANT_OU_DN` — literal string from `tenantOUDN(tenant)` with `${UDM_LDAP_BASE}`
  replaced by the actual base DN (passed from `r.LDAPBase` or similar new field).

**B.4 Admin user duplicate risk — LDAP federation scope**

`buildAdminScript` creates `admin-{tenant}` as a **Keycloak-local user** directly in the
tenant realm via REST API and grants it `realm-management/realm-admin`. When LDAP
federation is added and Keycloak imports `ou=gtn-demo`, it will also find `uid=admin-gtn-demo`
in LDAP and attempt to import it, potentially creating a duplicate or conflict with the
existing local account.

This must be resolved by configuring the LDAP federation with a `usersDn` that points
to a sub-OU containing only regular users (e.g. `ou=users,ou=gtn-demo`) rather than
the tenant OU root. The admin LDAP entry and the `ou=agents` sub-OU would then be
outside the federation search base and not imported. `buildOUScript` must be extended
to create the `ou=users,ou={tenant}` sub-container, and UDM user provisioning must
place regular users under it.

**B.5 Remove `ensureOpendeskAdminEnableJob` (follow-up)**

Once tenant admins live in the tenant realm via LDAP federation, the shadowExpire race
no longer involves the kernel realm. The enable/disable jobs can be simplified to target
the tenant realm directly, and the separate kernel-realm enable job can be removed.
This is deferred to a follow-up PR after Phase 2 is verified in production.

---

### Phase C — Shared app instance support

Phase C adds the `isolationMode` field and proves out shared deployment with a real app.
Element/Synapse is the natural first candidate: Matrix natively supports multiple
homeservers and federation, so a shared Synapse with per-tenant namespacing is a
well-understood deployment pattern.

**C.1 `AppProfile` API extension**

Add `spec.isolation.modes []string` and `spec.isolation.default string` fields to
`AppProfile`. This is a purely additive CRD change — existing profiles gain the field
with a default of `[dedicated]` and are unaffected.

**C.2 `Tenant.spec.apps` API extension**

Add `isolationMode string` field to `AppInstance` (valid values: `dedicated`, `shared`).
Omitting the field inherits the `AppProfile` default.

**C.3 Identity reconciler — shared realm provisioning**

When `isolationMode == shared`, the reconciler:
1. Provisions the platform-level `shared-apps` realm if absent (idempotent, one realm
   for all shared app types).
2. Registers an OIDC client named after the app type (e.g. `element`) in `shared-apps`
   instead of in the tenant realm.
3. Registers an identity broker in the tenant realm pointing to `shared-apps`, so users
   authenticate via their tenant realm and the token is issued by `shared-apps`.

Client IDs within `shared-apps` are unique by construction (app type names are unique).
Adding a second shared app type adds a client to the same realm, not a new realm.

**C.4 Composition — conditional chart deployment**

The Crossplane composition for each app checks `isolationMode`:
- `dedicated`: deploys as today, one Helm release per tenant namespace.
- `shared`: deploys a single platform-level Helm release (idempotent across tenants).
  Per-tenant config (quota, display name) is injected via a separate values overlay
  rather than a new release.

**C.5 Shared Element trial**

Before wiring the full composition abstraction, validate the shared path manually:
1. Deploy a single `opendesk-synapse` release in the `platform-kernel` namespace.
2. Register an `element` OIDC client in the `shared-apps` realm.
3. Configure identity broker in `gtn-demo` realm → `shared-apps`.
4. Point `gentian-apps/profiles/element.yaml` at the shared issuer.
5. Verify that `gtn-demo` users can authenticate into the shared Element instance
   without a second login prompt.

This validates the IAM topology before investing in the composition scaffolding.

---

## 5. Files changed — summary

| File | Phase | Change |
|---|---|---|
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
| `api/v1alpha1/appprofile_types.go` | C | Add `Isolation` field |
| `api/v1alpha1/tenant_types.go` | C | Add `IsolationMode` to `AppInstance` |
| `internal/controller/identity_reconciler.go` | C | Shared realm (`shared-apps`) provisioning path |
| `crossplane/compositions/app-*.yaml` | C | Conditional dedicated/shared deployment |
| `gentian-apps/profiles/element.yaml` | C | Trial shared Element: point at `shared-apps` issuer |

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
