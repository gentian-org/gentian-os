# Gentian OS — Current State vs Architecture

**Date:** 2026-04-04
**Source:** `architecture.md` v2.0-draft vs `server/` repository (develop branch)

---

## Summary

The `server/` repository implements a functional single-tenant GitOps deployment of OpenDesk components using ArgoCD, OpenBao, ESO, and Tofu Controller. It covers most kernel services but lacks the multi-tenant orchestration layer, CRD abstractions, and contract system described in the architecture.

---

## 1. Kernel Functions

| Kernel Function | Architecture Target | Current State (`server/`) | Gap |
|---|---|---|---|
| **Identity & permissions** | OIDC provider + LDAP via Keycloak Operator CRs, per-tenant realms | Nubus (Keycloak + UCS LDAP) deployed via Tofu Controller Helm release; Keycloak OIDC clients provisioned per-app via Tofu `app/` module | No per-tenant realm isolation; Keycloak managed via Tofu not Keycloak Operator CRs; single-realm model |
| **Filesystem (WebDAV/S3)** | Nextcloud (WebDAV) + MinIO (S3) with per-tenant buckets | Nextcloud AIO deployed via Tofu Controller; MinIO deployed via ArgoCD ApplicationSet (Pattern A) | Single-tenant; no per-tenant bucket isolation; no WebDAV contract abstraction |
| **Networking** | NetworkPolicies per tenant namespace, cross-namespace deny | Per-namespace restrictions exist | No tenant-scoped dynamic NetworkPolicy generation |
| **Process execution** | Kubernetes + ArgoCD GitOps | ArgoCD v2.11.3 fully operational; ApplicationSets manage deployment pipeline | Functional; no gap for single-tenant |
| **Secrets & keyring** | OpenBao + ESO, tenant-scoped policies, secret path `gentian-os/tenants/{name}/apps/{app}/...` | OpenBao v2.5.0 (Raft, transit auto-unseal) + ESO v2.0; secrets at `gentian/{env}/...` | Secret path structure is flat per-env, not hierarchical per-tenant/per-app as architecture requires; no per-tenant OpenBao policies |
| **Database services** | CloudNativePG operator CRs, per-app-per-tenant databases | PostgreSQL + MariaDB deployed via Tofu Controller Helm releases | No CloudNativePG operator; databases not managed via CRs; no per-tenant isolation |
| **Cache subsystem** | Redis Operator + Memcached with per-app ACLs | Redis 18.6.1 deployed via ArgoCD (Pattern A) | No Redis Operator CRs; no per-app ACL isolation; no Memcached |
| **Mail (kernel extension)** | Per-tenant Postfix + Dovecot + Rspamd, 4 tenant modes | Postfix + Dovecot deployed via Tofu Controller | Single instance, not per-tenant; no Rspamd; no tenant mode selection |
| **Package manager** | AppProfile CRD + orchestrator pipeline | Manual ApplicationSets + per-app Helm values directories | No AppProfile CRD; no automated app onboarding; each app requires manual YAML |
| **App-to-app permissions** | IntegrationBinding CRD + OIDC token exchange | Tofu `app-trust/` module configures token exchange between apps | Manual Tofu config not CRD-driven; no health tracking or auto-wiring |
| **Window manager / Shell** | Univention Portal with unified navigation | Univention Portal deployed as part of Nubus | Functional; no contract-based navigation registration |
| **Notifications** | Cross-app notification gateway | Intercom Service deployed (v2.19.5) | Partial; unclear if cross-app aggregation is complete |
| **Init system / lifecycle** | Thin orchestrator (Go, controller-runtime) | Tofu Controller + ArgoCD ApplicationSets + shell scripts | No custom orchestrator; lifecycle managed via scripts + Tofu + ArgoCD |
| **Resource quotas** | Per-tenant ResourceQuotas + LimitRanges | Not found | No tenant-scoped quota enforcement |

## 2. Architecture Triangle

| Tool | Architecture Role | Current State | Gap |
|---|---|---|---|
| **Thin orchestrator (Go)** | Provisioning plane — reacts to Tenant CRs, creates operator CRs, wires secrets, manages IntegrationBindings | **Does not exist** | Entire orchestrator is unbuilt; its responsibilities are split across Tofu modules, shell scripts, and manual YAML |
| **OpenTofu (via Tofu Controller)** | Infrastructure plane — static kernel provisioning, secret seeding, external resources | Tofu Controller v0.16.1 deployed; modules for OpenBao path seeding, Keycloak config, and Helm releases (Pattern B) | Tofu currently handles app-level provisioning (Helm releases) which architecture assigns to ArgoCD; scope creep into orchestrator territory |
| **ArgoCD** | Deployment plane — Helm chart deployment, drift detection, rollback | ArgoCD v2.11.3 with ApplicationSets for Pattern A apps | Only handles "Pattern A" apps (those supporting `existingSecret`); Pattern B apps bypass ArgoCD and go through Tofu Controller |

## 3. CRD Abstraction Model

| CRD | Architecture Target | Current State | Gap |
|---|---|---|---|
| **AppProfile** | Cluster-scoped; defines app kernel requirements, chart ref, value mapping schema | **Does not exist** | Apps defined as per-directory Helm values + ApplicationSet entries; no declarative app catalogue |
| **Tenant** | Declares domain, isolation, mail mode, quotas, app list | **Does not exist** | Single implicit tenant; no tenant lifecycle management |
| **IntegrationBinding** | Auto-generated per contract; tracks credential health | **Does not exist** | Token exchange configured manually via Tofu `app-trust/` module |
| **ArgoCD Application** | Generated by orchestrator, one per app per tenant | Manually defined in ApplicationSets; generated by Tofu for Pattern B | Not orchestrator-generated; split across two delivery patterns |

## 4. Secret Management

| Aspect | Architecture Target | Current State | Gap |
|---|---|---|---|
| **Secret store** | OpenBao with `gentian-os/kernel/...` and `gentian-os/tenants/{name}/apps/{app}/...` paths | OpenBao with `gentian/{env}/databases/...`, `gentian/{env}/keycloak/...` flat paths | Path hierarchy does not match architecture; no tenant dimension |
| **Secret delivery** | ESO syncs all secrets; Helm charts use `existingSecret` | Two patterns: **Pattern A** (ESO for apps supporting `existingSecret`) and **Pattern B** (Tofu `set_sensitive` for apps that don't) | Pattern B is a workaround; architecture assumes all apps use `existingSecret` via ESO |
| **Credential rotation** | Passive via orchestrator annotation trigger → OpenBao → ESO → ArgoCD | Stakater Reloader restarts pods on secret changes; no automated rotation trigger | No rotation workflow; Reloader handles reload but not credential regeneration |
| **Secret seeding** | OpenTofu seeds kernel credentials | `seed-openbao.sh` + Tofu `openbao-paths/` module derives passwords from master password via HMAC-SHA256 | Functional but script-based; architecture envisions pure Tofu seeding |

## 5. Deployment Layers

| Layer | Architecture Target | Current State | Gap |
|---|---|---|---|
| **000 — Bootstrap** | One-time script installs ArgoCD + Tofu Controller | `scripts/install.sh` (11-step bootstrap) + separate scripts for ArgoCD, OpenBao init | Functional; matches architecture intent |
| **100 — Kernel** | OpenTofu provisions infra; ArgoCD deploys kernel workloads via sublayers (100–160) | ArgoCD bootstrap apps deploy OpenBao, Tofu Controller, Reloader; Tofu deploys PostgreSQL, MariaDB, Nubus | No explicit sublayer ordering (100/110/120/...); kernel services not decomposed into sequenced layers |
| **100e — Kernel Extensions** | Mail stack as optional per-tenant extension | Postfix + Dovecot deployed as standard apps | Not modelled as a kernel extension; always-on, not per-tenant |
| **200 — Apps** | Orchestrator creates per-tenant ArgoCD Applications | ApplicationSets create apps per-environment (dev/staging/prod) | Environment-centric not tenant-centric; no orchestrator |

## 6. Multi-Tenancy

| Aspect | Architecture Target | Current State | Gap |
|---|---|---|---|
| **Tenant isolation** | Namespace-per-tenant or vCluster-per-tenant | Single set of namespaces (`gentian-infra-dev`, `gentian-iam-dev`, etc.) | No multi-tenancy; environment-based namespacing not tenant-based |
| **Database isolation** | Per-app-per-tenant databases with dedicated users | Shared databases per app | No tenant-scoped database isolation |
| **Storage isolation** | Per-tenant S3 buckets and WebDAV namespaces | Shared MinIO and Nextcloud instances | No tenant-scoped storage |
| **Identity isolation** | Separate Keycloak realm per tenant | Single realm | No tenant realm isolation |
| **Deletion policy** | Retain (default) or Delete per tenant | N/A | No tenant lifecycle |

## 7. Repository Structure

| Repo | Architecture Target | Current State | Gap |
|---|---|---|---|
| **gentian-os** | OS definition: orchestrator code, CRDs, Tofu kernel modules, kernel ArgoCD Applications | Contains `docs/` only (architecture document) | Orchestrator, CRDs, kernel modules not yet implemented |
| **gentian-apps** | App catalogue: one AppProfile YAML per app, contract schemas | Contains `LICENSE` and `README.md` only | Empty; no app profiles or contracts defined |
| **gentian-deployments** | Cluster state: Tenant CRs, env-specific Tofu vars, app-of-apps | Contains `README.md` only | Empty; all deployment state currently lives in `server/` |
| **server/** (current) | Not in architecture | Full deployment repo with apps, appsets, ArgoCD, ESO, OpenBao, Tofu, scripts | Serves as a monolithic precursor; content needs to be decomposed into the three target repos |

## 8. Applications

| Application | Architecture Status | Current Deployment | Gap |
|---|---|---|---|
| Nubus (Keycloak + LDAP) | Kernel — Identity | ✅ Deployed (Tofu Controller) | Functional |
| PostgreSQL | Kernel — Database | ✅ Deployed (Tofu Controller) | Not via CloudNativePG operator CRs |
| MariaDB | Kernel — Database | ✅ Deployed (Tofu Controller) | Not via MariaDB Operator CRs |
| Redis | Kernel — Cache | ✅ Deployed (ArgoCD Pattern A) | No operator; no per-app ACLs |
| Memcached | Kernel — Cache | ❌ Not deployed | Missing |
| MinIO | Kernel — Storage | ✅ Deployed (ArgoCD Pattern A) | Not via MinIO Operator CRs |
| Nextcloud | Kernel — Filesystem | ✅ Deployed (Tofu Controller) | Functional |
| Univention Portal | Kernel — Shell | ✅ Deployed (part of Nubus) | Functional |
| OpenBao | Kernel — Secrets | ✅ Deployed (bootstrap) | Functional |
| ESO | Kernel — Secrets sync | ✅ Deployed (bootstrap) | Functional |
| Postfix | Kernel ext. — Mail | ✅ Deployed (Tofu Controller) | Not per-tenant; no Rspamd |
| Dovecot | Kernel ext. — Mail | ✅ Deployed (Tofu Controller) | Not per-tenant |
| OX App Suite | App — Groupware | ✅ Deployed (Tofu Controller) | Functional |
| Intercom Service | App — Notifications | ✅ Deployed (ArgoCD Pattern A) | Functional |
| Collabora | App — Collaboration | 🔄 In progress (Phase 3A) | Spec ready, not yet synced |
| Element (Matrix) | App — Chat | ❌ Not deployed | Phase 3B |
| Jitsi | App — Video | ❌ Not deployed | Phase 3B |
| XWiki | App — Wiki | ❌ Not deployed | Phase 3B |
| OpenProject | App — Projects | ❌ Not deployed | Phase 3B |
| Notification Gateway | Kernel — Notifications | ❌ Not deployed | Not built |
| Thin Orchestrator | Kernel — Lifecycle | ❌ Not deployed | Not built |

## 9. Security Model

| Aspect | Architecture Target | Current State | Gap |
|---|---|---|---|
| **Network boundaries** | Tenant-to-tenant deny; tenant-to-kernel allow; IntegrationBinding-scoped app-to-app | Per-namespace restrictions | No tenant-aware dynamic policy generation |
| **OIDC trust chain** | Per-tenant realms, token exchange via IntegrationBindings | Single realm, token exchange via Tofu `app-trust/` | Single realm; manual trust config |
| **Mail security** | DKIM keys in OpenBao, SPF/DMARC in Tenant status | Not documented | No DKIM/SPF/DMARC automation |
| **Database isolation** | Per-app-per-tenant databases with scoped grants | Shared databases | No tenant-level DB isolation |
| **Zero-trust secrets** | All secrets via OpenBao, never in Git | ✅ Implemented (Pattern A + B) | Functional |

## 10. Backup and Observability

| Aspect | Architecture Target | Current State | Gap |
|---|---|---|---|
| **Backup — PostgreSQL** | pgBackRest or CloudNativePG built-in | Not configured | No backup strategy deployed |
| **Backup — MinIO** | Replication or Restic | Not configured | Missing |
| **Backup — Keycloak** | Realm export to S3 | Not configured | Missing |
| **Backup — OpenBao** | Raft snapshots to S3 | Not configured | Missing |
| **Backup — Velero** | Per-namespace K8s resource backup | Not deployed | Missing |
| **Tenant-scoped restore** | RestoreTenant CR (future) | N/A | No multi-tenancy |
| **Prometheus metrics** | Orchestrator exports `gentianos_*` metrics | No custom metrics | Orchestrator not built |
| **CRD status** | `kubectl get tenants` shows health | N/A | No CRDs |

---

## Priority Gap Summary

| Priority | Gap | Effort | Blocks |
|---|---|---|---|
| **P0** | Thin orchestrator (Go) — the core of the architecture | Large | Everything tenant-related |
| **P0** | CRD definitions (AppProfile, Tenant, IntegrationBinding) | Medium | Orchestrator |
| **P1** | Decompose `server/` into 3-repo structure | Medium | Clean separation of concerns |
| **P1** | Migrate Pattern B apps to Pattern A (upstream `existingSecret` support or wrapper charts) | Medium | Unified deployment plane |
| **P1** | Restructure OpenBao paths to architecture schema | Medium | Multi-tenancy |
| **P2** | Deploy missing apps (Collabora, Element, Jitsi, XWiki, OpenProject) | Medium | Per-app effort |
| **P2** | CloudNativePG operator for database management via CRs | Medium | Replaces Tofu Helm releases for PostgreSQL |
| **P2** | Multi-tenant namespace model and isolation | Large | Orchestrator must exist first |
| **P3** | Backup strategy (pgBackRest, Velero, OpenBao snapshots) | Medium | Independent |
| **P3** | DKIM/SPF/DMARC automation for mail | Small | Mail extension redesign |
| **P3** | Notification gateway | Medium | Architecture design needed |
| **P3** | Observability (Prometheus metrics, CRD status) | Medium | Orchestrator must exist first |
