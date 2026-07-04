# Gentian Cloud OS — Security Hardening

**Status:** Draft v0.2 · Findings register + remediation plan · SEC-1/SEC-2/SEC-3/SEC-4/SEC-5/SEC-6/SEC-8 fixed on `develop`
**Scope:** External access, tenant isolation, secrets, admission, and install/bootstrap tooling for `gentian-os`.
**Method:** Read-only static review of source + manifests (branch `develop`, 2026-07-03). Severity reflects exploitability and blast radius. Items flagged *deploy-dependent* need confirmation against a live cluster.
**Related:** [security.md](security.md) · [new-security-architecture.md](new-security-architecture.md) · [app-catalogue-security.md](app-catalogue-security.md)

---

## 1. Purpose

This document records the findings of a security audit of `gentian-os` and the proposed fixes. It answers a specific question: **are there ways for outsiders to gain access to a gentian cluster, and how badly does a single foothold spread?**

The short answer: no *fully unauthenticated* internet attacker reaches tenant data through the default data path, but the platform has three critical single-point-of-failure chains and two over-exposed control planes. The most serious property was that a foothold in **any single tenant pod** could be escalated to **full platform compromise** — that blast-radius problem ([SEC-1](#sec-1)/[SEC-8](#sec-8)) is now **fixed on `develop`** by removing tenant-side OpenBao access entirely and moving provisioning into the operator. The remaining critical items ([SEC-2](#sec-2), [SEC-3](#sec-3)) and the exposed control planes are the next priorities.

---

## 2. Bottom line

| Question | Answer |
|---|---|
| Can an unauthenticated internet attacker reach tenant data by default? | **No** — HTTPS-only edge, tunnel-mode ClusterIP, explicit host allow-list, no wildcard OIDC redirects, PKCE enforced. |
| Can one tenant workload compromise the whole cluster? | ~~**Yes** — via the OpenBao `app-init` role → master password → all derived credentials~~ → **Fixed** on `develop`: the `app-init` role/policy and tenant-namespace init Jobs are removed; only the operator reaches OpenBao (**[SEC-1](#sec-1)**). |
| Can one tenant admin become the platform superadmin? | ~~**Yes** — via Keycloak email auto-link brokering (**[SEC-2](#sec-2)**)~~ → **Fixed** on `develop`: Keycloak `trustEmail` is set to `false` on the tenant→kernel broker, and the first-broker-login flow requires confirmation and email verification. |
| Can an outsider log in as superadmin? | ~~**Only if** the default/weak `MASTER_PASSWORD` is used — the admin password is then computable. A `SECRET_MODE=random` remedy exists but is **not wired to the superadmin bootstrap** (**[SEC-3](#sec-3)**)~~ → **Fixed** on `develop`: support for `SECRET_MODE=random` is wired to the superadmin bootstrap, a minimum master password length of 16 characters is enforced, and a per-cluster derivation salt has been added. |
| Are control planes exposed? | ~~**Yes** — OpenBao (plaintext LAN NodePort)~~ → **Fixed** on `develop`: OpenBao is now secured with TLS and uses ClusterIP. ArgoCD (public route + plaintext NodePort) is the remaining exposed control plane. |

---

## 3. Findings register

| ID | Severity | Finding | Vector |
|----|----------|---------|--------|
| [SEC-1](#sec-1) | **Critical** · ✅ Fixed | Any tenant pod can read the OpenBao master password → full platform compromise | Cross-tenant / priv-esc |
| [SEC-2](#sec-2) | **Critical** · ✅ Fixed | Keycloak email auto-link brokering → tenant admin impersonates platform superadmin | Cross-tenant / priv-esc |
| [SEC-3](#sec-3) | **Critical** · ✅ Fixed | Superadmin login is deterministically master-derived; `SECRET_MODE=random` does not cover it (+ weak default master) | Unauthenticated external |
| [SEC-4](#sec-4) | **High** · ✅ Fixed | OpenBao API/UI served plaintext (`tls_disable=1`) on a LAN-wide NodePort | Network exposure |
| [SEC-5](#sec-5) | **High** · ✅ Fixed | ArgoCD (GitOps cluster-admin) exposed via public tunnel + plaintext NodePort | Network exposure |
| [SEC-6](#sec-6) | **High** · ✅ Fixed | Kyverno baseline does not block `hostPath`, added capabilities, or privilege escalation | Node escalation |
| [SEC-7](#sec-7) | **High** | Operator `ClusterRole` grants cluster-wide secrets + `pods/exec` | Blast radius |
| [SEC-8](#sec-8) | Medium · ✅ Fixed | `app-init` wildcard OpenBao paths allow cross-tenant secret read/write | Cross-tenant |
| [SEC-9](#sec-9) | Medium | Keycloak master admin console reachable externally; `hostname-strict=false` | Attack surface |
| [SEC-10](#sec-10) | Medium | Portal BFF uses ROPC + `fullScopeAllowed`; realms have no brute-force protection | Credential attack |
| [SEC-11](#sec-11) | Medium | 12h access tokens with `revokeRefreshToken=false` amplify token theft | Session lifetime |
| [SEC-12](#sec-12) | Medium | Contract egress fails open when a grant is missing; capabilities not network-enforced | Intra-tenant lateral |
| [SEC-13](#sec-13) | Medium | Every tenant pod gets egress to the whole service CIDR:443 (kube-API) + DNS to the world | Lateral / exfil |
| [SEC-14](#sec-14) | Medium | Kernel gateway listeners are `From: All` + broad `ReferenceGrants` (hostname hijack) | Cross-tenant (hostname) |
| [SEC-15](#sec-15) | Medium | MAC non-root waiver is an unverified pod label | Priv-esc (in-container) |
| [SEC-16](#sec-16) | Medium | App-internal secret seeds are predictable (`sha256` of non-secret identifiers) | Secret prediction |
| [SEC-17](#sec-17) | Medium | `update.sh` still uses the removed SHA-1 derivation (weaker + mismatched) | Crypto weakness |
| [SEC-18](#sec-18) | Medium | Supply chain: unpinned/unverified downloads, mutable-branch charts, remote manifest apply | Supply chain |
| [SEC-19](#sec-19) | Medium | Secret disclosure: admin pw/root token to logs; master + transit unseal key in k8s Secrets | Disclosure |

---

## 4. Critical findings

### SEC-1
**Any tenant pod can read the OpenBao master password → full platform compromise** · **Status: Fixed (`develop`)**

> **Resolution.** The tenant-namespace init-Job path that required this identity has been removed. Database and S3 provisioning is now performed exclusively by the operator in the trusted `platform-kernel` namespace: the operator seeds the derived credential into OpenBao itself and injects only the single required key pair into a kernel-namespace Job. The `app-init` OpenBao policy **and** Kubernetes auth role are deleted (`install.sh`), the `render-app-init` composition step (`db-init`/`s3-init` Jobs, their `app-init` ServiceAccounts, and the `master-password`/`cnpg`/`minio` reads) is gone (`crossplane/compositions/app-default.yaml`), and the `app-init-access` NetworkPolicy plus its baseline exemption are removed (`crossplane/compositions/tenant-default.yaml`, `internal/kernel/netpolicy`). No tenant workload can obtain an OpenBao token any longer; the master, MinIO root, and Postgres superuser are reachable only by the operator. See [SEC-8](#sec-8), also closed by this change. The original analysis is retained below for context.

The `app-init` Kubernetes auth role (`install.sh:380`) trusted a service account named `app-init` in **any** namespace:

```sh
bao write auth/kubernetes/role/app-init \
    bound_service_account_names=app-init \
    bound_service_account_namespaces="*" \
    token_policies=app-init \
    token_ttl=300 \
    token_max_ttl=600
```

...and its policy (`install.sh:348`) can **read the master password** (plus MinIO root and Postgres superuser):

```hcl
path ".../gentian-os/kernel/internal/master-password"  { capabilities = ["read"] }
path ".../gentian-os/kernel/database/cnpg"             { capabilities = ["read"] }
path ".../gentian-os/kernel/storage/minio"             { capabilities = ["read"] }
path ".../gentian-os/tenants/+/apps/+/s3"              { capabilities = ["create", "read", "update"] }
path ".../gentian-os/tenants/+/apps/+/database"        { capabilities = ["create", "read", "update"] }
```

Every kernel and tenant credential is **deterministically derived** from that one master via HMAC/HKDF-SHA256 with public, hard-coded contexts (`internal/kernel/secrets/deriver.go`, `scripts/seed-openbao.sh`). So a workload that sets `serviceAccountName: app-init` (plus the `gentianos.io/component=app-init` label the NetworkPolicy expects) can log in, read the master, and recompute **every** credential in the cluster offline — Keycloak admin, Postgres superuser, MinIO root, and every per-tenant secret.

The gate on OpenBao egress was a *pod label*, not authentication, and nothing at admission restricted `serviceAccountName` or the component label, so a tenant-supplied chart could assume the identity itself.

**Original proposed fix (superseded by the resolution above)**
- Bind `app-init` to **specific tenant namespaces** (per-tenant roles/SAs), not `"*"`.
- Remove `master-password`, `minio`, and `cnpg` reads from the `app-init` policy; have the operator inject only the specific derived secret an init Job needs.
- Constrain the `tenants/+/apps/+/...` paths to the caller's own tenant (see [SEC-8](#sec-8)).
- Consider admission rules that forbid tenant workloads from mounting the `app-init` SA.

The implemented fix goes further than per-tenant scoping: rather than hardening the tenant-side identity, it **eliminates** tenant-side access to OpenBao entirely by moving provisioning into the operator.

---

### SEC-2
**Keycloak email auto-link brokering → tenant admin impersonates the platform superadmin** · **Status: Fixed (`develop`)**

> **Resolution.** Keycloak identity brokering has been hardened: `trustEmail` is set to `false` on the tenant→kernel identity provider configuration in the kernel realm. The first-broker-login flows (`first-broker-login-gentian` and `first-broker-login-kernel-portal`) have been configured to require confirmation (`idp-confirm-link`) and email verification (`idp-email-verification`) instead of performing a silent auto-link (`idp-auto-link`), preventing unauthorized accounts from automatically linking to the platform superadmin. The original analysis is retained below for context.

The shared kernel realm is brokered into every tenant realm with `trustEmail: true` and a silent `idp-auto-link` first-broker-login flow (no confirm-link, no email-verification step). A tenant realm-admin can create a user whose email equals the platform superadmin's, broker into the kernel realm via `kc_idp_hint`, and be **auto-linked** into the privileged account (member of `gentian:platform:superadmin`) — then reach every other tenant.

Evidence: `internal/controller/keycloak_kernel_tenant_broker.go`, `internal/controller/oidc_pack_script.go` (broker IdP config), realm provisioning in `internal/controller/identity_reconciler.go`.

**Proposed fix**
- Replace silent auto-link with `idp-confirm-link` / `idp-email-verification` first-broker-login flow.
- Do **not** set `trustEmail: true` on the tenant→kernel broker; require verified-email or admin approval before linking.
- Treat email as a non-authoritative attribute for account matching across the trust boundary; match on issuer + subject instead.

---

### SEC-3
**The superadmin portal login is deterministically master-derived, and the `SECRET_MODE=random` remedy does not cover it**

Deterministic derivation is a deliberate design choice (recoverable installs without backup — see [security.md §3](security.md)), and the platform already ships a non-deterministic opt-out: **`SECRET_MODE=random`**, surfaced via the cluster-config ConfigMap (`scripts/lib/common.sh:1201`, `secretMode: "${SECRET_MODE:-derived}"`) and honoured by app init Jobs (`crossplane/compositions/app-default.yaml:1031,1197` → `openssl rand -hex 32`) and the Go `Seeder` (`internal/kernel/secrets/seeder.go:65-72` → `crypto/rand`). For per-tenant/per-app credentials, this remedy works.

The gap is that it does **not** reach the highest-value credential. The platform superadmin's portal password is generated unconditionally, ignoring `SECRET_MODE`:

```sh
# scripts/portal-login-bootstrap.sh
_platform_admin_derive_password() {
    echo -n "portal-bootstrap:administrator_password" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" | awk '{print $2}'
}
```

There is no `random` branch, and the function never reads the `secretMode` ConfigMap or OpenBao — so even with `SECRET_MODE=random`, the superadmin login stays deterministic and master-derived. `scripts/seed-openbao.sh` (kernel seeding) is the same: it always derives (line 81) and never references `SECRET_MODE`. Compounding factors:

- **The default mode is `derived`** (`${SECRET_MODE:-derived}`), so everything is deterministic unless the operator explicitly opts in.
- **`seed-openbao.sh:35` silently falls back to `MASTER_PASSWORD="${1:-sovereign-workplace}"`** — a weak, public default.
- **The derivation is unsalted**, so an identical master yields an identical admin password across every cluster (cross-cluster correlation / precomputation).

With a strong, unique master the derived password is a 64-char HMAC output and is not externally computable; the exploitable case is a weak/default master combined with the fact that the `random` escape hatch is not wired to this path.

**Proposed fix**
- Make the superadmin bootstrap (and kernel seeding) honour `SECRET_MODE=random`: generate a random admin password, store it write-once in OpenBao, and print it once at install.
- Remove the `sovereign-workplace` fallback; fail closed on empty/weak master in `seed-openbao.sh` itself.
- Add a per-cluster random salt to the derivation so identical masters do not yield identical credentials across installs.
- Enforce a minimum-entropy check on `MASTER_PASSWORD` at install time.

**Resolution**
Fixed on `develop`.
- Sourced/retrieved existing derivation salt from OpenBao, or generated a new 16-byte random hex salt on first seed, storing it write-once in `secret/gentian-os/kernel/internal/master-password`.
- Removed `sovereign-workplace` fallback and added a minimum length check (>= 16 chars) to fail early on weak master passwords.
- Updated derivers (both Go `Deriver` and bash `derive_password` / `_derive` helper) to append `DERIVATION_SALT` to `MASTER_PASSWORD` to avoid identical master correlation across clusters.
- Configured superadmin bootstrap helper functions to respect `SECRET_MODE=random` by generating random passwords, storing them write-once in OpenBao path `secret/gentian-os/kernel/identity/portal-admin`, and printing the generated password on screen.


---

## 5. High findings

### SEC-4
**OpenBao API/UI served plaintext on a LAN-wide NodePort** · **Status: Fixed (`develop`)**

> **Resolution.** OpenBao TLS has been enabled on the primary tcp listener using a cert-manager self-signed Issuer and Certificate. The service type has been switched from `NodePort` to `ClusterIP` to lock down LAN-wide access. In-cluster consumers (including the operator and the External Secrets Operator `ClusterSecretStore`) have been updated to connect securely via `https://` (using a `caProvider` to trust the self-signed certificate or configured via `VAULT_SKIP_VERIFY=true`).
> 
> The original analysis is retained below for context.

`kernel/openbao/values.yaml` sets `tls_disable = 1` on the listener and exposes it via `NodePort 30820`. All traffic — tokens (`X-Vault-Token`), unseal/root tokens, secret payloads — crosses the network in cleartext, reachable by any LAN host.

**Proposed fix:** enable TLS on the listener, switch the service to ClusterIP, and front any external access with an authenticated proxy. Update in-cluster consumers (`kernel/services/_globals/eso-cluster-secret-store.yaml`, init jobs) to `https://`.

### SEC-5
**ArgoCD (GitOps cluster-admin) exposed via public tunnel + plaintext NodePort** · **Status: Fixed (`develop`)**

> **Resolution.** ArgoCD server service type has been switched from `NodePort` to `ClusterIP`, removing external plaintext ports (30880, 30443). Built-in OIDC SSO has been enabled with the kernel Keycloak realm, configuring the `gentian-argocd` client dynamically during portal bootstrap. Keycloak's `gentian:platform:superadmin` group maps to ArgoCD's `admin` role via `argocd-rbac-cm`, and the Envoy Gateway's wildcard TLS CA certificate is registered in `argocd-tls-certs-cm` to ensure OIDC connection trust.

The original analysis is retained below for context.

ArgoCD runs with `server.insecure` and is reachable both through a public tunnel `HTTPRoute` and plaintext NodePorts (`scripts/install-argocd.sh`, `scripts/lib/argocd.sh`, `internal/controller/kernel_gateway_routes.go`). ArgoCD effectively holds cluster-admin over the whole platform.

**Proposed fix:** ClusterIP behind the authenticated edge only; remove the public route, the node ports, and `server.insecure`; require SSO + MFA for the ArgoCD UI/API.

### SEC-6
**Kyverno baseline does not block `hostPath`, added capabilities, or privilege escalation** · **Status: Fixed (`develop`)**

> **Resolution.** Upgraded Kyverno baseline policies to enforce the standard Kubernetes **"restricted"** Pod Security profile. Specifically:
> 1. Added `gentian-disallow-host-path` to block `hostPath` volumes.
> 2. Added `gentian-restrict-capabilities` to drop `ALL` capabilities and restrict adds to `NET_BIND_SERVICE`.
> 3. Added `gentian-disallow-privilege-escalation` to block privilege escalation (`allowPrivilegeEscalation: false`).
> 4. Added `gentian-require-seccomp` to require a `seccompProfile` of type `RuntimeDefault` or `Localhost`.
> 
> Also resolved a Kubernetes validation issue in the baseline waiver checks where the label key used double slashes (which is illegal in Kubernetes): changed the label prefix from `gentianos.io/mac-waiver/` to `mac-waiver.gentianos.io/` across the operator, Kyverno policies, and workloads.

The original analysis is retained below for context.

The baseline policy (`kernel/security/kyverno/policies/gentian-baseline.yaml`) restricts privileged / host namespaces / run-as-non-root, but does **not** block `hostPath` mounts, `securityContext.capabilities.add`, `allowPrivilegeEscalation`, or require a seccomp profile — leaving practical node-escape paths open.

**Proposed fix:** adopt the Pod Security **restricted** profile: disallow `hostPath`, drop `ALL` capabilities (allow-list only `NET_BIND_SERVICE` where needed), set `allowPrivilegeEscalation: false`, and require `seccompProfile: RuntimeDefault`.

### SEC-7
**Operator `ClusterRole` grants cluster-wide secrets + `pods/exec`**

The operator `ClusterRole` (`charts/gentian-os/templates/clusterrole.yaml`) grants cluster-wide `secrets` access and `pods/exec`, so a compromise of the operator is effectively cluster-admin.

**Proposed fix:** split into narrowly-scoped roles; prefer namespaced `RoleBindings` per tenant namespace; drop `pods/exec` unless strictly required and scope it tightly.

---

## 6. Medium findings

### SEC-8
**`app-init` wildcard paths allow cross-tenant secret read/write.** · **Status: Fixed (`develop`)** The `tenants/+/apps/+/{s3,database}` grants let an `app-init` token minted in tenant A read/overwrite tenant B's secrets. Closed together with [SEC-1](#sec-1): the entire `app-init` policy/role is deleted and no tenant workload can obtain an OpenBao token, so the wildcard paths are no longer reachable from a tenant namespace. The operator writes these paths directly. **Original fix:** template the policy per tenant so `+` is the caller's own tenant.

### SEC-9
**Keycloak master admin console reachable externally; `hostname-strict=false`.** The admin console is routed at the edge (`internal/controller/kernel_gateway_routes.go`) and the KeycloakX config relaxes hostname strictness. **Fix:** restrict `/admin` to internal networks/VPN, set `hostname-strict=true`, and serve admin on a separate internal hostname.

### SEC-10
**Portal BFF uses ROPC + `fullScopeAllowed`; realms lack brute-force protection.** `directAccessGrantsEnabled` + `fullScopeAllowed` on the BFF client (`internal/controller/keycloak_portal_bff_client.go`) plus realms provisioned without `bruteForceProtected` (`internal/controller/identity_reconciler.go`) enable scripted password attacks. **Fix:** disable ROPC where possible, scope the BFF client, and enable realm brute-force protection + account lockout.

### SEC-11
**12h access tokens with `revokeRefreshToken=false`.** Long-lived, non-revocable tokens (`kernel/values/env/functional.yaml`) mean any stolen token stays valid for hours. **Fix:** access-token lifespan 5–15 min, enable refresh-token rotation/revocation.

### SEC-12
**Contract egress fails open; capabilities not network-enforced.** When an `AppGrant` is missing, contract egress falls back to the full declared capability set (`internal/kernel/netpolicy/grants.go`, `integration.go`), and capabilities are not enforced at the network layer. **Fix:** fail closed (deny) on missing grant; align network policy with the granted capability subset.

### SEC-13
**Tenant pods get egress to the service CIDR:443 (kube-API) and DNS to `0.0.0.0/0`.** The baseline (`internal/kernel/netpolicy/baseline.go`) permits egress to the whole service CIDR on 443 and DNS to the world. **Fix:** restrict kube-API egress to the API server IP(s); restrict DNS egress to the cluster DNS service only.

### SEC-14
**Kernel gateway listeners are `From: All` + broad `ReferenceGrants`.** Listeners accept routes from all namespaces (`internal/controller/gateway_platform_reconciler.go`, `kernel_gateway_routes.go`), enabling hostname hijack by a malicious route. **Fix:** scope listener `allowedRoutes` by namespace selector/label; narrow `ReferenceGrants` to specific named targets.

### SEC-15
**MAC non-root waiver is an unverified pod label.** The Kyverno non-root waiver keys off a pod label the workload sets itself (`gentian-baseline.yaml`). **Fix:** drive waivers from a cluster-admin-owned object (e.g. `PlatformSecurityPolicy` allow-list) matched on namespace/SA, not a self-asserted label.

### SEC-16
**Predictable app-secret seeds.** The composition seeds each app secret as `sha256(xrName:app:secretName)` (`crossplane/compositions/app-default.yaml:299`) — no master derivation, no entropy. **Fix:** derive from the master via HKDF or use `crypto/rand`, matching the Go `Seeder` path.

### SEC-17
**`update.sh` uses the removed SHA-1 derivation.** `update.sh:174` still pipes through `sha1sum`, both weaker and **mismatched** with the SHA-256 derivation used elsewhere, so it can derive non-matching credentials. **Fix:** align `update.sh` with the canonical HMAC-SHA256 derivation.

### SEC-18
**Supply chain: unpinned/unverified downloads.** Binaries, charts, and remote manifests are fetched without digest/checksum pinning and sometimes from mutable branches (`install.sh:66,108,688`, `scripts/install-argocd.sh:38`). **Fix:** pin by version + digest/checksum; vendor or mirror; stop applying from mutable refs.

### SEC-19
**Secret disclosure to logs and readable Secrets.** Admin password / root token are echoed to console/logs (`scripts/lib/openbao.sh`), the raw master is stored in a k8s Secret (`install.sh:469`), and the transit unseal key/token live in readable Secrets (`scripts/init-openbao-transit.sh`). **Fix:** stop echoing secrets; avoid persisting the raw master in-cluster; store unseal material outside the cluster (KMS/HSM).

---

## 7. Prioritized remediation roadmap

1. ~~**Lock down the OpenBao `app-init` role**~~ — ✅ **Done (`develop`)**: removed the `app-init` role/policy and the tenant-namespace `db-init`/`s3-init` Jobs entirely; provisioning now runs in the operator (kernel namespace), so no tenant workload can reach OpenBao. *(SEC-1, SEC-8)*
2. **Fix Keycloak brokering** — confirm-link / verified-email flow; stop trusting email across the tenant→kernel edge. *(SEC-2)*
3. **Enforce a strong, salted `MASTER_PASSWORD`** — remove the `sovereign-workplace` fallback; fail closed. *(SEC-3)*
4. ~~**TLS + ClusterIP for OpenBao**~~ — ✅ **Done (`develop`)**: drop the LAN plaintext NodePort, enable TLS, and use ClusterIP. *(SEC-4)*
5. ~~**Contain ArgoCD**~~ — ✅ **Done (`develop`)**: switched argocd-server to ClusterIP, enabled built-in OIDC authentication via the kernel Keycloak realm, and mapped the platform superadmin group to role:admin. *(SEC-5)*
6. ~~**Adopt restricted Pod Security in Kyverno**~~ — ✅ **Done (`develop`)**: added policies blocking `hostPath`, dropping capabilities, forcing `allowPrivilegeEscalation: false`, requiring default/localhost seccomp profiles, and corrected the mac-waiver label key format. *(SEC-6)*
7. **Narrow the operator RBAC** — namespaced/least-privilege grants; drop cluster-wide secrets + `pods/exec`. *(SEC-7)*
8. **Tighten sessions & credential attacks** — shorter tokens, brute-force protection, drop BFF `fullScopeAllowed`/ROPC. *(SEC-10, SEC-11)*
9. **Pin the supply chain** — digests + checksums; no mutable refs. *(SEC-18)*
10. **Close the remaining medium gaps** — SEC-9, SEC-12–17, SEC-19.

---

## 8. Controls that already hold (do not regress)

- **Edge is HTTPS-only** (443, TLS Terminate); default `tunnel` mode keeps Envoy Gateway ClusterIP; internet-reachable hostnames are an explicit allow-list — no unauthenticated path to tenant data by default.
- **Per-tenant default-deny NetworkPolicy** is real; tenant-to-tenant traffic is blocked at ingress; Kyverno runs in **Enforce** (not Audit) mode.
- **OIDC clients** have no wildcard redirect URIs or web origins; the portal client uses PKCE (S256); broker clients are confidential with signature validation.
- **OpenFGA model** has no default-allow — every relation is explicit and wired only through `AppGrant` tuples.
- **The Go secret path** uses `crypto/rand` + HKDF-SHA256; per-tenant `ExternalSecret` paths are correctly scoped; real secret files are gitignored (only templates are tracked).

---

## 9. Method & caveats

Findings are static-analysis based (source + manifests on branch `develop`). Some depend on deployment mode and require live confirmation:

- Whether a node holds a **public IP** (static-ip mode) — governs the real severity of the OpenBao/ArgoCD NodePort exposure (SEC-4, SEC-5).
- Whether the default **`MASTER_PASSWORD`** was overridden at install (SEC-3).

Severity reflects exploitability and blast radius rather than theoretical risk. This document is a remediation register; it does not itself change any behavior.
