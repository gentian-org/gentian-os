# Upstream cleanup — removing `registry.opencode.de` dependencies

**Status:** Plan  
**Context:** Gentian OS was bootstrapped on the ZenDiS OpenDesk supply chain. Charts
and container images are pulled from the private OCI registry
`registry.opencode.de`. Without access to that registry, fresh installs and
upgrades will fail even though the Git repos are self-contained.

This document inventories every known dependency, records the intended cleanup
strategy per component (A–D), and tracks progress against the **kernel rebuild
roadmap** (§8). The goal is to rebuild the kernel from the inside with vanilla
upstream where possible — not to mirror OpenDesk charts into `gentian-os`.

---

## Cleanup strategy legend

Each component below has a checkbox (`[ ]` = not done, `[x]` = done) and a
**strategy** letter describing the intended end state:

| Strategy | Meaning | Target location |
|---|---|---|
| **A** | **Replaced with open charts / images** — genuinely free/open upstream (Apache-2.0, AGPL, vendor public registries with acceptable terms) | Public chart repos + `docker.io` / vendor registries; Gentian values only |
| **B** | **Commercial / Pro catalogue** — paid charts, images, and `AppProfile`s customers purchase | Private **`gentian-org/gentian-pro`** → private GHCR packages under **`ghcr.io/gentian-org`** (see [roadmap.md](roadmap.md)) |
| **C** | **Re-wrapped based on public image** — Gentian-owned Helm chart; public or rebuilt container image | `gentian-os/charts/upstream/` + `ghcr.io/gentian-org/mirror/` or public image refs |
| **D** | **Rebuilt** — replace with Gentian-native implementation (not a mirror of OpenDesk) | Gentian code + public deps (see Nubus replacement doc) |

**Progress summary:** check boxes in §4–§6 as each component completes its
**§8 roadmap step**.

---

## 1. Executive summary

| Question | Answer |
|---|---|
| Do we have local copies of the OpenDesk charts? | **Almost none in Git.** Phase 0 rescue artefacts live in `upstream-rescue/` (local, not committed). Only `gentian-apps/charts/odoo/` is vendored to `ghcr.io/gentian-org/charts`. |
| Is `gentian-apps/apps/...` the right place for migrated charts? | **Not for upstream Helm charts.** `apps/<name>/` is the first-party app scaffold. Use **`gentian-apps/charts/<name>/`** for **free/public** tenant charts. **Premium OpenDesk charts → `gentian-pro`.** Kernel infra → **`gentian-os/charts/upstream/`** (or public upstream). |
| What else breaks besides charts? | **Container images.** Many values files pin images under `registry.opencode.de/bmi/opendesk/...` with digest pins. Charts alone are not enough. |
| Where do paid OpenDesk artefacts go? | **`gentian-pro`** (private repo) — charts + images for premium catalogue apps (Element/Jitsi, OX). Kernel and free tier stay in public `gentian-org` repos. |
| Migration approach? | **Inside-out kernel rebuild** — infra first, strip OpenDesk wrappers, then IAM, then apps (see §8). |
| Rescue artefacts? | `upstream-rescue/` (local) — reference only; not the end-state delivery path. |
| Can we switch opencode → Univention registry? | **No.** Univention (`artifacts.software-univention.de`) is a **separate proprietary stack** (Terms of Use, subscription for commercial use). It is not “open upstream” — same class of problem as OpenDesk/opencode. Nubus, Intercom (ICS), and related Univention images must be **dropped (D)**, not repointed or replaced with another vendor stack. |

### 1.1 Proprietary supply chains (not migration targets)

Two overlapping proprietary stacks appear in today’s kernel. **Neither is an acceptable
end state** for Gentian OS:

| Source | What it provides | Why we cannot adopt it |
|---|---|---|
| **OpenDesk / `registry.opencode.de`** | Mirrored charts and images (Bitnami, Nextcloud wrappers, Nubus extensions, …) | Private ZenDiS supply chain; no entitlement for Gentian to depend on it long-term |
| **Univention / `artifacts.software-univention.de`** | Nubus umbrella, Intercom Service (ICS), LDAP/portal/UDM runtime (~40+ images) | Vendor Terms of Use; binaries not AGPL; commercial use requires subscription |

Rescue docs (`upstream-rescue/UNIVENTION-IMAGES.md`) describe what those artefacts
**are**, for comparison and decommission planning — not as a registry to migrate **to**.

**Intercom (ICS):** Univention’s proprietary OIDC/session bridge for OpenDesk (silent
login iframe, Nordeck Element banner, portal newsfeed credentials). It is **not** a
general notification service and **not** worth replacing — **drop it** in step 3 and
rewire portal + Element to use Keycloak (and Gentian UI) directly without `/silent` or
`navigation.json` from ICS.

---

## 2. Gentian-pro commercial catalogue (strategy **B**)

OpenDesk-derived charts and images that are part of the **commercial (Pro) catalogue**
are not published as public `gentian-apps` artefacts. They live in the private
**`gentian-org/gentian-pro`** repository. **Access is enforced by the Gentian
controller (entitlement), not by giving tenants a shared registry password.**

Terminology: Gentian uses **Community** (`gentian-apps`, OSS licences) vs **Pro**
(`gentian-pro`, `license: proprietary`). That matches common industry labels
(Community/Pro, OSS/Commercial). Use **`edition`** on `AppProfile` for variants
within a tier (e.g. `full` vs `performant`).

### 2.1 Repository and registry (near term)

| Item | Target |
|---|---|
| Community Git repo | **`gentian-org/gentian-apps`** (public) — OSS profiles + free charts |
| Pro Git repo | **`gentian-org/gentian-pro`** (private) — commercial profiles, charts, mirrored images |
| Community OCI | `oci://ghcr.io/gentian-org/charts/...` (public packages) |
| Pro OCI | `oci://ghcr.io/gentian-org/charts/...` with **private** package visibility (same GHCR org) |
| CI | Each repo publishes its own charts/images on release |
| Access control | **Entitlement in controller** (`ProfileRequiresEntitlement`, Tenant/AppCatalogue reconcilers); CRM (Odoo) is commercial source of truth |

**Future (not yet):** a separate GitHub org and GHCR org (`gentian-org-pro` /
`ghcr.io/gentian-org-pro`) for hard supply-chain isolation — see
[roadmap.md — Commercial layer](roadmap.md#commercial-layer).

### 2.2 What belongs in gentian-pro (strategy **B**)

| Component | Charts | Images | Profile source |
|---|---|---|---|
| Element (Matrix) | `opendesk-element`, `opendesk-synapse` | Element/Synapse mirrors | `gentian-pro/profiles/element/` (move from gentian-apps when ready) |
| Jitsi (Element sidecar) | `opendesk-jitsi` | Jitsi stack mirrors | composition in element profile |
| OX App Suite | `appsuite-public-sector` (+ subcharts) | OX public-sector image set | `gentian-pro/profiles/ox-appsuite/` |

Reference copies (if needed): `upstream-rescue/charts/` and `upstream-rescue/images/`.
Moved to gentian-pro in roadmap **step 5** — not before Nubus/Nextcloud kernel work.

### 2.3 What stays public (strategies **A**, **C**, **D**)

| Strategy | Repo | Examples |
|---|---|---|
| **A** | `gentian-os` / public upstream — no vendoring | Redis, MinIO, OpenProject |
| **C** | `gentian-os/charts/upstream/` → public `ghcr.io/gentian-org/charts` | Nextcloud, Postfix, Dovecot, Collabora, CryptPad, XWiki |
| **D** | `gentian-os` (code, not OpenDesk mirror) | Nubus decommission; Intercom (ICS) dropped; Gentian IAM |

### 2.4 Wiring Pro profiles (repo + entitlement)

1. **`AppProfile.spec.license: proprietary`** — `ProfileRequiresEntitlement()` in the
   operator ([`catalogue_helpers.go`](../api/v1alpha1/catalogue_helpers.go)).
2. **Catalogue sync** — `AppCatalogue` controller lists Pro profiles only when the
   cluster/tenant has an active entitlement (CRM → fulfillment → cluster state).
3. **Install gate** — Tenant controller rejects `spec.apps[].profile` for Pro profiles
   without entitlement; only then Crossplane/Helm provisions the app in that tenant
   namespace (other tenants on the same cluster never receive the release).
4. **`spec.chart.repository`** on Pro profiles points at private packages published
   from **`gentian-pro`** (e.g. `oci://ghcr.io/gentian-org/charts/element`).
5. **`gentian-os` kernel** does not depend on `gentian-pro`; only entitled tenant
   installs pull Pro charts/images. Optional: namespace-scoped `imagePullSecret`
   created by fulfillment when a Pro app is activated.

See **§8 step 5** and [business-logic-plan.md](design/business-logic-plan.md).

---

## 3. Dependency map

Two distinct pull paths exist today:

```mermaid
flowchart LR
  subgraph kernel ["gentian-os kernel"]
    AS["ArgoCD ApplicationSets<br/>10-infra, 20-iam"]
    PH["provider-helm Release CRs<br/>kernel/services/*/release.yaml"]
  end
  subgraph apps ["gentian-apps tenant catalogue"]
    AP["AppProfile spec.chart"]
    SC["sidecars / composition.yaml"]
  end
  subgraph supply ["Supply chain targets"]
    PUB["Public upstream<br/>Bitnami, vendors"]
    GHCR["ghcr.io/gentian-org<br/>public + private pkgs"]
    PRO["gentian-org/gentian-pro<br/>Git + private GHCR"]
  end
  REG["registry.opencode.de<br/>legacy"]

  AS --> REG
  PH --> REG
  AP --> REG
  AP --> GHCR
  AP --> PRO
  REG -.->|migrate| PUB
  REG -.->|migrate| GHCR
  REG -.->|migrate B| PRO
```

- **Kernel** (`gentian-os`): cluster-wide infra — databases, IAM, mail, Nextcloud, etc.
- **Catalogue** (`gentian-apps/profiles/*`): per-tenant installable apps.
- **Already migrated:** `app-store`, `odoo-free-base` → `oci://ghcr.io/gentian-org/charts`.

---

## 4. Chart inventory

### 4.1 Kernel charts (gentian-os)

Pulled via ArgoCD ApplicationSet or Crossplane `Release` CR.

| Done | Strategy | Component | Chart name | OCI repository (current) | Pinned version | Target | Referenced in |
|:---:|---|---|---|---|---|---|---|
| [x] | **A** | Redis | `redis` | `.../external/charts/bitnami-charts` | 18.6.1 | `oci://registry-1.docker.io/bitnamicharts/redis` | `kernel/appsets/10-infra.yaml` |
| [x] | **A** | MinIO | `minio` | `.../external/charts/bitnami-charts` | 16.0.10 | Public Bitnami chart + `bitnamilegacy/*` images | `kernel/appsets/10-infra.yaml` |
| [ ] | **D** | Intercom (ICS) | `intercom-service` | `.../supplier/univention/charts-mirror` | 2.19.5 | **Drop** — remove chart, Keycloak client, gateway route; rewire portal/Element | `kernel/appsets/20-iam.yaml` |
| [ ] | **D** | Nubus | `nubus` | `.../supplier/univention/charts-mirror` | 1.16.0 | Gentian IAM (Keycloak + directory + ReBAC); decommission Nubus | `kernel/services/nubus/manifests/dev/release.yaml` |
| [x] | **A** | PostgreSQL | `infra-postgresql` | Vendored `charts/infra/postgresql` | 2.1.2 | InfraData XR + packages repo (future: CNPG) | `kernel/services/infra-postgresql/`, `crossplane/compositions/infra-data.yaml` |
| [x] | **A** | MariaDB | `infra-mariadb` | Vendored `charts/infra/mariadb` | 3.0.3 | InfraData XR + packages repo (future: operator) | `kernel/services/infra-mariadb/`, `crossplane/compositions/infra-data.yaml` |
| [ ] | **C** | Nextcloud (umbrella) | `opendesk-nextcloud` | `.../platform-development/charts/opendesk-nextcloud` | 4.7.2 | Gentian chart + `library/nextcloud` or rebuilt image | `kernel/services/nextcloud/...` |
| [ ] | **C** | Nextcloud management | `opendesk-nextcloud-management` | same repo | 4.7.2 | Gentian init Job + public NC base | `kernel/services/nextcloud-management/...` |
| [ ] | **C** | Nextcloud notifypush | `opendesk-nextcloud-notifypush` | same repo | 4.7.2 | Gentian subchart + public notify_push | `kernel/services/nextcloud-notifypush/...` |
| [ ] | **C** | Postfix | `opendesk-postfix` | `.../platform-development/charts/opendesk-postfix` | 5.2.0 | Gentian chart + public/vendor postfix image | `kernel/services/postfix/...` |
| [ ] | **C** | Dovecot | `opendesk-dovecot` | `.../platform-development/charts/opendesk-dovecot` | 3.4.1 | Gentian chart + public dovecot image | `kernel/services/dovecot/...` |
| [ ] | **C** | Collabora | `collabora-online` | `.../supplier/collabora/charts-mirror` | 1.1.45 | Collabora upstream chart + CODE image | `kernel/services/collabora/...` |
| [ ] | **C** | CryptPad | `cryptpad` | `.../supplier/xwiki/charts-mirror` | 0.0.21 | xwiki-labs chart + `docker.io/cryptpad/cryptpad` | `kernel/services/cryptpad/...` |

Also referenced in install/ArgoCD plumbing but not in an active `release.yaml` in-tree:

| Done | Strategy | Chart | OCI repository (current) | Notes |
|:---:|---|---|---|---|
| [x] | — | `opendesk-keycloak-bootstrap` | `.../platform-development/charts/opendesk-keycloak-bootstrap` | Superseded by operator + `keycloak-config` manifests |

### 4.2 Tenant catalogue charts (gentian-apps)

| Done | Strategy | Profile | Chart(s) | OCI repository (current) | Version | Target | Local copy? |
|:---:|---|---|---|---|---|---|---|
| [ ] | **B** | `element` | `opendesk-element` | `.../platform-development/charts/opendesk-element` | 6.1.9 | `gentian-pro` | Rescue only |
| [ ] | **B** | `element` (sidecar) | `opendesk-jitsi` | `.../platform-development/charts/opendesk-jitsi` | 3.5.1 | `gentian-pro` | Rescue only |
| [ ] | **A** | `openproject` | `openproject` | `.../supplier/openproject/charts-mirror` | 10.1.0 | `charts.openproject.org` | Rescue only |
| [ ] | **B** | `ox-appsuite` | `open-xchange` / `appsuite-public-sector` | `.../supplier/open-xchange/charts-mirror` | 2.26.32 | `gentian-pro` | Rescue only |
| [ ] | **B** | `ox-appsuite` (composition) | `nubus` sub-chart ref | `.../supplier/univention/charts-mirror` | — | Drop when OX uses Gentian IAM (**D**) | — |
| [ ] | **C** | `xwiki` | `xwiki` | `.../supplier/xwiki/charts-mirror` | 1.4.4 | Gentian chart + public XWiki image | Rescue only |
| [x] | — | `odoo-free-base` | `odoo` | `oci://ghcr.io/gentian-org/charts` | 0.1.0 | **Done** — `gentian-apps/charts/odoo/` |
| [x] | — | `app-store` | `app-store` | `oci://ghcr.io/gentian-org/charts` | 0.2.5 | **Done** — first-party |

Stale duplicate profiles under `gentian-os/export/gentian-apps/` should be deleted once migration is tracked in `gentian-apps` only.

---

## 5. Container image inventory (not just charts)

Even after charts are migrated, pods will fail if images still point at opencode.
**Treat chart migration and image migration as one work item per component.**

### 5.1 Kernel image groups

| Done | Strategy | Service / group | Example image path | File |
|:---:|---|---|---|---|
| [ ] | **D** | Nubus — OpenDesk extensions | `opendesk-nubus`, `opendesk-nubus-a2g-mapper` | `kernel/services/nubus/.../values/_base.yaml` |
| [ ] | **D** | Nubus — Univention extensions | `ox-extension`, `portal-extension` | same — decommission with Nubus; no Univention registry target |
| [ ] | **D** | Nubus — runtime (~36 images) | `nubus/images/*` | Chart defaults — decommission with step 3 |
| [x] | **A** | MinIO | `images-mirror/minio`, `os-shell` | `kernel/services/minio/values/_base.yaml` |
| [x] | **A** | Redis | `images-mirror/redis` | `kernel/services/redis/values/_base.yaml` |
| [ ] | **D** | Intercom (ICS) | `images-mirror/intercom-service` | `kernel/services/intercom-service/` — delete service tree after consumers rewired |
| [ ] | **C** | Nextcloud AIO + mgmt + exporter | `opendesk-nextcloud*` | `nextcloud*`, `nextcloud-management/` |
| [ ] | **C** | Dovecot | `dovecot-public-sector` | `kernel/services/dovecot/values/_base.yaml` |
| [ ] | **C** | CryptPad | `images-mirror/cryptpad` | `kernel/services/cryptpad/...` |
| [ ] | **C** | Collabora | Collabora CODE image via chart | `kernel/services/collabora/...` |
| [ ] | **C** | Postfix | OpenDesk postfix image | `kernel/services/postfix/...` |

### 5.2 Tenant app image groups

| Done | Strategy | Profile | Example | File |
|:---:|---|---|---|---|
| [ ] | **B** | Element | `opendesk-element-web` | `profiles/element/profile.yaml` |
| [ ] | **B** | Element | `matrixdotorg/synapse` or opendesk mirror | `profiles/element/profile.yaml` |
| [ ] | **B** | Element composition | `matrix-user-verification-service`, Jitsi images | `profiles/element/composition.yaml` |
| [ ] | **A** | OpenProject | `open_desk` → public `openproject/openproject` image | `profiles/openproject/profile.yaml` |
| [ ] | **B** | OX App Suite | OX public-sector image set | `profiles/ox-appsuite/profile.yaml` |
| [ ] | **C** | XWiki | `xwiki` public image | `profiles/xwiki/profile.yaml` |

---

## 6. Non-chart opencode plumbing to remove

After charts and images are self-hosted, delete or replace:

| Done | Area | Files / symbols |
|:---:|---|---|
| [ ] | Install credentials | `OD_PRIVATE_REGISTRY_USERNAME`, `OD_PRIVATE_REGISTRY_PASSWORD` in `install.sh`, `scripts/install-lib.sh`, `getting-started.md` |
| [ ] | Docker pull secret bootstrap | `install.sh` (`registry.opencode.de` docker secret), `kernel/services/_globals/secrets/dev/registry-credentials.yaml` |
| [ ] | ArgoCD OCI repo secrets | `scripts/create-argocd-oci-secrets.sh` |
| [ ] | ArgoCD project allowlist | `kernel/argocd/projects/gentian.yaml` (`registry.opencode.de/*` entries) |
| [ ] | Crossplane cluster bootstrap | `crossplane/compositions/cluster-default.yaml` (ArgoCD source allowlist) |
| [ ] | ESO comment / dev values | `kernel/values/env/dev.yaml` |
| [ ] | gentian-pro entitlement gate | Tenant + AppCatalogue controllers enforce Pro install (see roadmap) |

OpenDesk **semantic** dependencies (OIDC pack client IDs, LDAP attribute names, Keycloak
scope YAML copied from upstream) can stay initially — they describe behaviour, not registry
access. Longer-term rename is optional (see §9).

---

## 7. Target repo layout

After the rebuild, artefacts land by **role** — not by “mirror everything to
`gentian-os/charts/upstream`”.

```
gentian-os/                             # Kernel — rebuilt from inside
├── crossplane/                         # Vanilla infra + app XRs (step 2+)
├── kernel/services/                    # Values + Release CRs → public charts or Gentian code
├── charts/gentian-os/                  # Operator
└── charts/upstream/                    # Optional thin wrappers only (e.g. Nextcloud step 4)

gentian-pro/                            # PRIVATE — step 5 (strategy B)
├── profiles/                           # Commercial AppProfiles (license: proprietary)
├── charts/                             # OX, Element, Jitsi
└── mirror/                             # Commercial container images (optional layout)

gentian-apps/                           # PUBLIC community catalogue
├── charts/                             # Free apps (Odoo, OpenProject, …)
├── apps/                               # First-party scaffold (app-store, …)
└── profiles/                           # Community AppProfiles (OSS licences)
```

**Publishing targets:**

| Tier | Helm OCI | When (§8) |
|---|---|---|
| Public upstream direct (**A**) | Bitnami, CNPG, Docker Hub, … | Steps 1–4, 6–7 |
| `ghcr.io/gentian-org` (public pkgs) | Community charts from `gentian-apps` | Steps 2, 4, 8 |
| `ghcr.io/gentian-org` (private pkgs) + **`gentian-pro`** (**B**) | Commercial tenant apps | Step 5 |
| `ghcr.io/gentian-org-pro` (separate org) | Optional future hard isolation | [roadmap.md](roadmap.md) |

---

## 8. Kernel rebuild roadmap

Rebuild the kernel **from the inside out**. Each step removes OpenDesk/opencode
coupling before the next layer depends on it. Check boxes when the step is done
in dev (and inventory rows in §4–§6 are updated).

This is **not** a “mirror everything to GHCR” plan. Prefer **A** (public upstream
direct) and **D** (Gentian-native rebuild) over vendoring OpenDesk charts into
`gentian-os`.

```mermaid
flowchart TD
  S1[1 Infra cleanup]
  S2[2 Vanilla Crossplane]
  S3[3 IAM rebuild drop ICS]
  S4[4 Vanilla Nextcloud]
  S5[5 gentian-pro apps]
  S6[6 Vanilla mail + OX]
  S7[7 Vanilla Element / Jitsi]
  S8[8 Final cleanup]

  S1 --> S2 --> S3 --> S4 --> S5 --> S6 --> S7 --> S8
```

### Step 1 — Clean up infra (strategy **A**)

Replace OpenDesk-mirrored **data plane** with public upstream. No Gentian chart
forks unless Crossplane needs a thin values wrapper.

```
[x] PostgreSQL — vendored chart; opencode OCI pull removed (step 2: CNPG / vanilla bootstrap)
[x] MariaDB — vendored chart; opencode OCI pull removed (step 2: public chart / operator)
[x] Redis — public Bitnami chart + bitnamilegacy image
[x] MinIO — public Bitnami chart + bitnamilegacy image
```

**Inventory:** §4.1 Redis, MinIO, PostgreSQL, MariaDB; §5.1 Redis, MinIO.

Intercom (ICS) and Nubus stay deployed only until **step 3** rewires consumers — there
is no acceptable “switch registry” shortcut for either (see §1.1).

### Step 2 — Strip wrappers; vanilla Crossplane provisioning

Remove OpenDesk-specific Helm glue, install.sh special cases, and provider-helm
complexity around infra that step 1 already made vanilla.

```
[x] Delete or replace opendesk-postgresql / opendesk-mariadb Release CRs and service trees
[x] Simplify kernel/appsets/10-infra.yaml to public chart repos only
[x] Infra provisioning via Crossplane compositions / XRs — no OpenDesk bootstrap Jobs
[x] Remove opendesk image overrides from redis/minio/postgres values
[x] Tenant DB provisioning stays operator-driven; no LDAP/UDM dependency for infra wave
```

**Step 2 done:** Shared PostgreSQL/MariaDB move from Argo-synced `Release` CRs
(`kernel/services/opendesk-*`) to the **InfraData** Crossplane XR
(`crossplane/xrds/infra-data.yaml`, `crossplane/compositions/infra-data.yaml`).
ESO secrets and plain values ConfigMaps sync via `kernel/appsets/08-infra-data.yaml`.
Helm release names stay `opendesk-postgresql-dev` / `opendesk-mariadb-dev` for DNS
compatibility until consumers are renamed in a later step.

**Principle:** kernel infra = Crossplane + public charts + Gentian values. Nothing
from `registry.opencode.de` at this layer.

### Step 3 — Rebuild IAM; drop Nubus and Intercom (strategy **D**)

Replace the **OpenDesk + Univention IAM stack** (Nubus, UDM/LDAP as system-of-record,
OpenDesk extensions) with Gentian IAM: **Keycloak** as IdP, **Gentian directory +
ReBAC**, optional LDAP/SCIM bridges later.

**Drop Intercom (ICS)** — do not replace it with another service. ICS is an OpenDesk-only
integration layer (silent OIDC iframe, Nordeck Element banner URLs, portal newsfeed
credentials). Remove it and point consumers at Keycloak + Gentian portal/Element config
directly:

| Consumer today | ICS dependency | After drop |
|---|---|---|
| Gentian portal newsfeed | hidden iframe → `ics…/silent` | Keycloak session / portal-native feed auth |
| Element (Nordeck module) | `ics_navigation_json_url`, `ics_silent_url` | Remove Nordeck OpenDesk module or vanilla Element config |
| Keycloak | `opendesk-intercom` client, token exchange | Delete client; apps use their own OIDC clients |
| Kernel gateway | `ics.<kernel>` → intercom-service | Remove HTTPRoute |
| Synapse (planned) | `intercom_as_token` app-service | Drop; wire bridges only if needed with public Synapse patterns |

Do **not** vend the Nubus chart to GHCR and do **not** repoint pulls to
`artifacts.software-univention.de`.

```
[ ] IAM replacement architecture signed off (Nubus decommission + ICS drop)
[ ] Interim: keep Nubus/ICS only as long as needed for dev parity (both proprietary)
[ ] Remove opendesk-nubus / a2g-mapper extensions from values (Gentian bootstrap)
[ ] Delete intercom-service from 20-iam.yaml; remove kernel/services/intercom-service/
[ ] Remove opendesk-intercom Keycloak client and identity/intercom OpenBao paths
[ ] Rewire gentian-ui portal newsfeed (no icsSilentLoginUrl)
[ ] Rewire Element profile (no net.nordeck.element_web.module.opendesk ICS URLs)
[ ] Remove verify_intercom_ics / gateway overlays from install.sh
[ ] Migrate operator off UDM REST; cut portal-server dependency on Nubus where possible
[ ] Decommission Nubus chart when replacement IAM path is green
```

**Inventory:** §4.1 Nubus, Intercom (ICS); §5.1 Nubus and Intercom rows; §9 LDAP/OIDC items.

### Step 4 — Vanilla Nextcloud in kernel (strategy **C** → public image)

Integrate **`docker.io/library/nextcloud`** (or equivalent public image) — not
`opendesk-nextcloud*`. Gentian-owned Helm/Crossplane release; init via operator Job
or `occ`, Keycloak `user_oidc`.

```
[ ] Drop opendesk-nextcloud, -management, -notifypush Release CRs
[ ] New kernel service: public Nextcloud + Postgres + Redis + ingress
[ ] WebDAV/CardDAV + .well-known redirects on gateway
[ ] OIDC via kernel IdP; drop opendesk LDAP object classes for Files
[ ] Collabora/CryptPad: defer or minimal public chart if still kernel apps
```

**Inventory:** §4.1 Nextcloud (×3); §5.1 Nextcloud images.

### Step 5 — Move commercial apps to gentian-pro (strategy **B**)

**After** kernel infra and IAM are on Gentian rails, move OpenDesk-derived **tenant**
charts/images and **`AppProfile`s** into **`gentian-org/gentian-pro`**. Kernel must not
depend on gentian-pro. **Install access = private repo + controller entitlement**
(CRM confirms payment → entitlement → Tenant may reference Pro profile).

```
[ ] Create gentian-org/gentian-pro (private): profiles/, charts/, CI publish to private ghcr.io/gentian-org packages
[ ] Import rescued OX / Element / Jitsi charts + images (reference: upstream-rescue/)
[ ] Move commercial profiles from gentian-apps; set license: proprietary
[ ] AppCatalogue + Tenant controllers: enforce entitlement before Pro list/install
[ ] CRM fulfillment webhook → entitlement record → Tenant.spec.apps (entitled tenants only)
[ ] Kernel install succeeds with zero gentian-pro references
```

**Inventory:** §4.2 Element, Jitsi, OX; §5.2 **B** rows; §2 gentian-pro wiring.

### Step 6 — Vanilla Postfix + Dovecot; trial with vanilla OX

Implement **public/vendor** mail stack in kernel (**C**). Experiment whether
**vanilla OX** (from gentian-pro or public OX paths) works without OpenDesk
mail integration.

```
[ ] Drop opendesk-postfix / opendesk-dovecot charts
[ ] Vanilla Postfix + Dovecot (Gentian Helm or Crossplane)
[ ] LDAP/OIDC integration against step 3 IAM (not Nubus UDM)
[ ] Spike: OX from gentian-pro + vanilla mail — document blockers
```

**Inventory:** §4.1 Postfix, Dovecot; §5.1 mail images.

### Step 7 — Vanilla Element / Jitsi spike

Validate **public** Element Web + Synapse + Jitsi charts/images (not OpenDesk
wrappers). If vanilla works, gentian-pro ships thin Gentian values only; if not,
keep mirrored OpenDesk charts in gentian-pro temporarily.

```
[ ] Spike: vectorim/element-web + matrixdotorg/synapse + jitsi/jitsi-helm-chart
[ ] Keycloak OIDC packs updated for vanilla client IDs
[ ] Decision: gentian-pro mirrors vs public-only profiles in gentian-apps
```

**Inventory:** §4.2 Element/Jitsi; §5.2 Element rows.

### Step 8 — Final cleanup

Remove all remaining opencode plumbing and OpenDesk semantic debt where safe.

```
[ ] Drop OD_PRIVATE_* from install.sh / install-lib.sh
[ ] Remove registry.opencode.de from ArgoCD allowlists and pull secrets
[ ] CI gate: fail on registry.opencode.de in gentian-os / gentian-apps
[ ] Delete gentian-os/export/gentian-apps/ duplicate tree
[ ] OpenProject / XWiki / CryptPad: public upstream (A/C) as catalogue work allows
[ ] Update getting-started.md, deployment.md, app-profile-guide.md
```

**Inventory:** §6 plumbing table; §9 optional decoupling.

### Reference artefacts (not a roadmap step)

`upstream-rescue/` holds charts/images pulled from opencode for **comparison and
gentian-pro import** — it is not the target architecture. See
[UPSTREAM-COMPARISON.md](../../upstream-rescue/UPSTREAM-COMPARISON.md). If access
is lost before step 5, recovery options are in §12.

---

## 9. Longer-term decoupling (optional)

These are **not** registry blockers but reduce OpenDesk coupling:

| Topic | Current state | Direction |
|---|---|---|
| OIDC client IDs (`opendesk-synapse`, etc.) | Required by upstream chart templates | Rename when **B** charts forked in gentian-pro |
| `opendesk-oidc-catalog` profile | Ships OpenDesk scope pack | Gentian-native `OIDCPackCatalog` |
| Nubus LDAP extensions | OpenDesk-specific schema objects | **D** — Gentian directory + ReBAC |
| Intercom (ICS) | Univention OIDC bridge for OpenDesk portal/Element | **D** — drop; rewire consumers to Keycloak / Gentian UI |
| Keycloak scopes YAML | Copied from upstream helmfile | Gentian scope definitions |
| Service aliases (`ums-*`) | `kernel/services/nubus/manifests/dev/service-aliases.yaml` | Remove with **D** |

---

## 10. Per-component checklist template

Copy for each chart/image group; set **Strategy** to A/B/C/D:

```
Strategy: __   Roadmap step: __
[ ] Public upstream or Gentian-native path chosen (prefer A/D over OpenDesk vendoring)
[ ] OpenDesk wrapper / Release CR removed from kernel where applicable
[ ] Crossplane or operator provisioning updated (vanilla path)
[ ] Chart/image refs off registry.opencode.de
[ ] Dev cluster sync green for this component
[ ] Inventory row(s) in §4–§6 checked off
```

For **step 5 (gentian-pro)** only: `[ ] Artefact copied from upstream-rescue/ into gentian-pro`

---

## 11. Verification commands

```bash
# Find remaining opencode references
rg -n 'registry\.opencode\.de' gentian-os gentian-apps

# Validate gentian-apps profiles after edits
cd gentian-apps && python3 scripts/validate-profile-tiles.py

# Dry-run helm template against new local chart
helm template test gentian-apps/charts/odoo -f profiles/odoo-free-base/profile.yaml
```

---

## 12. Risk register

| Risk | Mitigation |
|---|---|
| Charts lost with no rescue pull | Needed only for **step 5** gentian-pro import; kernel path uses public upstream + **D** rebuild |
| OpenDesk charts embed opencode image defaults | Remove chart entirely (steps 1–4, 6–7); do not vend to gentian-os |
| Univention / opencode treated as “the other proprietary registry” | **Neither is a target.** Step 3 drops Nubus + ICS; do not repoint to `artifacts.software-univention.de` |
| Premium images in public GHCR by mistake | **gentian-pro** only for **B**; CI policy on org separation |
| Licence / redistribution | **B** = proprietary catalogue; **A/C** record provenance in `UPSTREAM.md` |
| gentian-pro access without payment | Controller entitlement gate on Tenant + AppCatalogue; CRM as source of truth |
| Large fork maintenance | Avoid — **A** direct public; **D** rebuild; **B** isolated in gentian-pro only |

---

## 13. Related docs

- [upstream-rescue/UPSTREAM-COMPARISON.md](../../upstream-rescue/UPSTREAM-COMPARISON.md) — chart/image diff vs public upstream
- [upstream-rescue/UNIVENTION-IMAGES.md](../../upstream-rescue/UNIVENTION-IMAGES.md) — Univention artefact inventory (licence context; **not** a migration target)
- [architecture.md](architecture.md) — three-repo model (`gentian-os`, `gentian-apps`, `gentian-deployments`); add **gentian-pro** as fourth private repo
- [deployment.md](deployment.md) — environment promotion; entitlement / pull secrets
- [design/app-catalogue-security.md](design/app-catalogue-security.md) — `license: proprietary`, trust tiers
- Nubus replacement doc (draft) — strategy **D**
- Odoo precedent: `gentian-apps/charts/odoo/` + `profiles/odoo-free-base/profile.yaml`
