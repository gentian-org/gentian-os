# AppProfile Versioning and Flavors

**Status:** Implemented (API + App Store controller)  
**Companion to:** [app-catalogue.md](app-catalogue.md),
[app-product-create.md](app-product-create.md),
[app-catalogue-security.md](app-catalogue-security.md)

---

## 1. Problem

The catalogue needs **immutable, semver-versioned** technical packages (`AppProfile`)
and **sellable SKUs** (`AppProduct`) that can differ on:

| Axis | Field | Examples | Purpose |
|---|---|---|---|
| **Catalogue version** | `catalogueVersion` | `1.0.0`, `2.1.0` | Immutable publish revision (OCI/Helm-style) |
| **Edition** (feature set) | `edition` | `minimal`, `full`, `performant` | Footprint / feature variant |
| **Offering tier** (commercial) | `offeringTier` | `free`, `hardened`, `supported` | Pricing / SLA / hardening pack |
| **Trust tier** (certification) | `trustTier` (product only) | `platform`, `certified`, `experimental` | Security / review gate |

**Do not conflate** `offeringTier` (commercial) with `trustTier` (catalogue certification).

Upstream Helm chart version remains `spec.chart.version` — a separate pin from
`spec.catalogueVersion`.

---

## 2. Identity model (DRY)

Shared types live in `api/v1alpha1/catalogue_types.go`:

```go
type ProfileIdentity struct {
    Family           string        // logical app id, e.g. openproject
    CatalogueVersion string        // semver catalogue revision
    Edition          Edition       // minimal | standard | full | performant
    OfferingTier     OfferingTier  // free | hardened | supported
}

type ProfileReference struct {
    Name     string           // exact AppProfile metadata.name (pin)
    Identity *ProfileIdentity // dimensional selector when Name is empty
}
```

`ProfileReference` is reused by:

- `AppProduct.spec.profileRefs[]`
- `Tenant.spec.apps[].profileRef` (optional dimensional install pin)

Helpers in `catalogue_helpers.go` normalize defaults and resolve references.

---

## 3. AppProfile versioning (best practice)

### 3.1 Immutable revisions

Each published catalogue entry is **immutable**. To ship changes:

1. Bump `spec.catalogueVersion` (semver).
2. Create a **new** `AppProfile` CR (recommended name pattern below) or replace
   in Git with a reviewed migration plan.

Never rewrite a published `(family, catalogueVersion, edition, offeringTier)` tuple.

### 3.2 Naming conventions

| Field | Role |
|---|---|
| `metadata.name` | Unique cluster id used in `Tenant.spec.apps[].profile` and App claims |
| `spec.family` | Groups all revisions of one logical app (defaults to `metadata.name`) |
| `spec.catalogueVersion` | Semver of this catalogue entry (defaults to `1.0.0`) |

**Recommended name** for new revisions (optional but aids GitOps clarity):

```text
{family}--{catalogueVersion}--{edition}--{offeringTier}
# e.g. openproject--2.1.0--full--supported
```

Legacy profiles may keep short names (`openproject`) with explicit `family` +
`catalogueVersion` fields.

### 3.3 Index labels

The App Store controller sets:

| Label | Value |
|---|---|
| `gentianos.io/profile-name` | `metadata.name` |
| `gentianos.io/profile-family` | `spec.family` |
| `gentianos.io/profile-catalogue-version` | `spec.catalogueVersion` |
| `gentianos.io/profile-edition` | `spec.edition` |
| `gentianos.io/profile-offering-tier` | `spec.offeringTier` |

### 3.4 Defaults (backward compatible)

| Field | Default |
|---|---|
| `family` | `metadata.name` |
| `catalogueVersion` | `1.0.0` |
| `edition` | `full` |
| `offeringTier` | `free` |

---

## 4. AppProduct flavors

`AppProduct` carries store metadata and pins profile revisions:

```yaml
apiVersion: gentianos.io/v1alpha1
kind: AppProduct
metadata:
  name: openproject-supported
spec:
  displayName: "OpenProject (Supported)"
  catalogueVersion: "1.0.0"      # SKU listing version
  edition: full
  offeringTier: supported        # commercial tier
  trustTier: certified             # certification tier
  profileRefs:
    - identity:
        family: openproject
        catalogueVersion: "2.1.0"
        edition: full
        offeringTier: supported
  publisher:
    name: "Gentian Platform"
```

`profileRefs` may use `name:` (explicit pin) or `identity:` (dimensional pin).
Name takes precedence when both are set.

---

## 5. Install resolution

| Source | Resolves to |
|---|---|
| `Tenant.spec.apps[].profile` | Exact AppProfile name (primary path today) |
| `Tenant.spec.apps[].profileRef` | Unique match on `ProfileIdentity` |
| `AppProduct` checkout | Each `profileRefs[]` → append resolved profile name |

Operator resolution: `internal/catalogue.ResolveTenantAppProfile`.

---

## 6. AppCatalogue status

`AppCatalogue.status` exposes:

- `apps[]` — every `AppProfile` with family, catalogue version, edition, offering tier
- `products[]` — every `AppProduct` with resolved `profileRefs`, trust/offering tiers

`apps[]` remains for CLI compatibility; store UIs should prefer `products[]`.

---

## 7. Related documents

| Topic | Document |
|---|---|
| AppProduct hull | [app-product-create.md](app-product-create.md) |
| Trust / admission | [app-catalogue-security.md](app-catalogue-security.md) |
| Profile authoring | [gentian-apps/app-profile-guide.md](../../../gentian-apps/app-profile-guide.md) |
