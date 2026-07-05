# AppProfile Versioning, Metadata, and Portal Tiles

**Status:** Implemented (API + App Store controller)  
**Companion to:** [app-catalogue.md](app-catalogue.md), [business-logic-plan.md](business-logic-plan.md)

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
| **`gentian-pro/profiles/`** | `proprietary` |

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
| **`gentian-pro/profiles/`** | Pro catalogue (`license: proprietary`) |

Recommended CR name for new revisions:

```text
{family}--{catalogueVersion}--{edition}
```

---

## 5. AppCatalogue

Singleton `AppCatalogue/default` — `status.apps[]` lists every synced `AppProfile` with
catalogue metadata and install counts. Store UI and CLI consume this index.

---

## 6. Portal Tiles

App-menu and portal tile icons for dedicated-mode apps.

### 6.1 Spec (`AppProfile.spec.tile`)

```yaml
spec:
  tile:
    icon: chat          # Path 2 — Gentian catalogue id
  portalTiles:
    - name: ox-mail
      tile:
        icon: mail      # optional per-tile override
```

Path 1 (custom SVG):

```yaml
spec:
  tile:
    image: assets/tile.svg   # git only — run gentian-apps/scripts/sync-profile-tile.py
    logo: data:image/svg+xml;base64,...  # committed after sync
```

Resolution order: portal tile `tile` → profile `tile` → catalogue default `app`.

### 6.2 Catalogue integration

Built from `gentian-ui/design-system/tiles/` (Lucide glyphs + Gentian frame).
Embedded in the operator at `internal/tiles/catalogue.json`.

Rebuild and sync:

```bash
gentian-ui/design-system/tiles/scripts/sync-consumers.sh
```

### 6.3 CRD

`TileSpec` on `AppProfile` and `PortalTileSpec` — see `api/v1alpha1/appprofile_types.go`.
