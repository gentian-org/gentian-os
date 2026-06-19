# AppProfile Versioning and Catalogue Metadata

**Status:** Implemented (API + App Store controller)  
**Companion to:** [app-catalogue.md](app-catalogue.md),
[business-logic-plan.md](business-logic-plan.md),
[app-catalogue-security.md](app-catalogue-security.md)

---

## 1. Model

**`AppProfile`** is the catalogue unit.

| Field | Purpose |
|---|---|
| `family` | Logical app id shared across revisions |
| `catalogueVersion` | Semver of this immutable catalogue entry |
| `edition` | Feature variant (`minimal`, `standard`, `full`, `performant`) |
| `trustTier` | Platform certification (`platform`, `certified`, `experimental`) |
| `license` | SPDX identifier (`Apache-2.0`, `proprietary`, …) |

Public vs premium is implied by **source repo** and **license**:

| Source | Typical license |
|---|---|
| **`gentian-apps/profiles/`** | OSS SPDX ids (e.g. `Apache-2.0`) |
| **`gentian-premium/profiles/`** | `proprietary` |

`spec.chart.version` is the **Helm chart pin** — distinct from `catalogueVersion`.

Commerce (price, customer, invoice) lives in **CRM/ERP** — see [business-logic-plan.md](business-logic-plan.md).

---

## 2. Identity tuple

```go
type ProfileIdentity struct {
    Family           string
    CatalogueVersion string   // semver
    Edition          Edition
}
```

Used by `Tenant.spec.apps[].profileRef` for dimensional installs.

**Index labels** (set by App Store controller):

| Label | Value |
|---|---|
| `gentianos.io/profile-name` | `metadata.name` |
| `gentianos.io/profile-family` | `spec.family` |
| `gentianos.io/profile-catalogue-version` | `spec.catalogueVersion` |
| `gentianos.io/profile-edition` | `spec.edition` |
| `gentianos.io/profile-trust-tier` | `spec.trustTier` |

---

## 3. Defaults

| Field | Default |
|---|---|
| `family` | `metadata.name` |
| `catalogueVersion` | `1.0.0` |
| `edition` | `full` |
| `trustTier` | `certified` |

---

## 4. Repositories

| Repo | Profiles |
|---|---|
| **`gentian-apps/profiles/`** | OSS catalogue |
| **`gentian-premium/profiles/`** | Premium catalogue (`license: proprietary`) |

Recommended CR name for new revisions:

```text
{family}--{catalogueVersion}--{edition}
```

---

## 5. AppCatalogue

Singleton `AppCatalogue/default` — `status.apps[]` lists every synced `AppProfile` with
catalogue metadata and install counts. Store UI and CLI consume this index.

---

## 6. Related documents

| Topic | Document |
|---|---|
| Business / Odoo | [business-logic-plan.md](business-logic-plan.md) |
| Catalogue flow | [app-catalogue.md](app-catalogue.md) |
| Authoring | [gentian-apps/app-profile-guide.md](../../../gentian-apps/app-profile-guide.md) |
