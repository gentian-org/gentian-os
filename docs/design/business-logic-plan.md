# Gentian Business Logic Plan

**Status:** Design plan  
**Companion to:** [app-catalogue.md](app-catalogue.md),
[app-profile-versioning.md](app-profile-versioning.md),
[architecture.md](../architecture.md)

Gentian OS and **`gentian-apps`** are **open source**. Premium **`AppProfile`s**
live in **`gentian-premium`**. **Commerce** (customers, orders, invoices,
entitlements) is handled by a **CRM/ERP** (e.g. **Odoo**).

The **app store** lists **`AppProfile`** entries from the cluster (`AppCatalogue`).
Premium profiles appear after the CRM confirms entitlement and triggers sync/install.

---

## 1. Repository layout

```text
gentian-os/                 # Platform (operator, CRDs, AppCatalogue controller)
gentian-apps/
└── profiles/               # OSS AppProfile CRs

gentian-premium/            # Premium AppProfile CRs
└── profiles/
    └── openproject-performant.yaml

gentian-deployments/        # Tenant desired state (spec.apps[].profile)
```

| Repo | Contents | Sync |
|---|---|---|
| **`gentian-apps`** | OSS `AppProfile` YAML | ArgoCD `gentian-appprofiles` → all clusters |
| **`gentian-premium`** | Premium `AppProfile` YAML | ArgoCD per customer/cluster **after entitlement** |
| **CRM (Odoo)** | Customers, products, orders, invoices, entitlements | API / webhooks → fulfillment |

Public vs premium is implied by **repo** and **`license`** on the profile (see §2).

---

## 2. AppProfile catalogue metadata (platform)

All discoverable apps are **`AppProfile`** CRs:

| Field | Purpose |
|---|---|
| `family` | Logical app id across revisions |
| `catalogueVersion` | Semver of this catalogue entry |
| `edition` | Feature variant (`minimal`, `full`, `performant`, …) |
| `trustTier` | Platform certification (`platform`, `certified`, `experimental`) |
| `license` | SPDX id (`Apache-2.0`, `proprietary`, …) |

Example — OSS (`gentian-apps`):

```yaml
spec:
  family: openproject
  catalogueVersion: "1.0.0"
  edition: full
  trustTier: certified
  license: Apache-2.0
```

Example — premium (`gentian-premium`):

```yaml
spec:
  family: openproject
  catalogueVersion: "2.0.0"
  edition: performant
  trustTier: certified
  license: proprietary
```

**Install path:** `Tenant.spec.apps[].profile` → operator → Crossplane → Helm.  
**Store path:** `AppCatalogue.status.apps[]` indexes every synced profile; UI/CLI
apply entitlement rules for premium (`license: proprietary`) profiles.

Details: [app-profile-versioning.md](app-profile-versioning.md).

---

## 3. End-to-end flow

```mermaid
flowchart LR
    subgraph oss ["OSS"]
        GA["gentian-apps/profiles"]
    end
    subgraph premium ["Premium"]
        GP["gentian-premium/profiles"]
    end
    subgraph crm ["CRM / ERP (Odoo)"]
        CUST["Customer"]
        PROD["Product / price"]
        SO["Sales order"]
        INV["Invoice"]
        ENT["Entitlement"]
    end
    subgraph cluster ["Cluster"]
        AP["AppProfile"]
        TEN["Tenant"]
        CAT["AppCatalogue"]
    end

    GA --> AP
    ENT -->|unlock sync| GP
    GP --> AP
    AP --> CAT
    CUST --> SO
    PROD --> SO
    SO --> ENT
    SO --> INV
    ENT -->|fulfill| TEN
    TEN -->|profile name| AP
```

1. **Browse:** store reads `AppCatalogue` — OSS profiles from `gentian-apps`; premium when entitled.
2. **Order:** customer buys in Odoo (or portal → Odoo).
3. **Entitlement:** CRM records which **customer** may use which **profile** (by CR name or identity tuple) on which **tenant**.
4. **Fulfill:** workflow enables `gentian-premium` sync (or Git deploy key) and appends `profile:` to `Tenant`.
5. **Invoice:** Odoo generates recurring **invoices** from subscription lines.

---

## 4. What Odoo (CRM/ERP) should track

Map Gentian concepts to standard Odoo apps (**Sales**, **Subscriptions**, **Accounting**,
**Contacts**). Exact module names vary by Odoo edition; the **data** is what matters.

### 4.1 Master data

| Gentian concept | Odoo model (typical) | Notes |
|---|---|---|
| **Customer** | `res.partner` | Company, billing address, VAT, `billing_email` |
| **Sellable app** | `product.product` / `product.template` | One product per billable **AppProfile** identity or family+edition SKU |
| **Tenant / workspace fee** (optional) | `product.product` | Platform subscription per tenant if billed separately |
| **Price** | `product.pricelist` / `list_price` + currency | Monthly recurring list price |
| **AppProfile reference** | Custom field on product, e.g. `x_appprofile_name` or `x_profile_identity` | Links commerce → technical install target |

**Product SKU code example:** `openproject--2.0.0--performant` → matches `AppProfile.metadata.name` or identity tuple.

### 4.2 Sales and entitlement

| Gentian concept | Odoo model (typical) | Notes |
|---|---|---|
| **Quote / order** | `sale.order` + `sale.order.line` | Lines reference `product.product`; quantity = tenants or seats |
| **Subscription** | `sale.subscription` (or recurring SO) | Monthly billing for active entitlements |
| **Entitlement** | Custom `gentian.entitlement` **or** confirmed SO line + analytic account | Fields: `partner_id`, `tenant_name`, `appprofile_name`, `state`, `start`, `end` |
| **Fulfillment status** | SO line / entitlement `state` | `draft` → `paid` → `fulfilled` → `active` → `cancelled` |

**Entitlement row (minimal):**

| Field | Example |
|---|---|
| Customer | Acme GmbH (`res.partner`) |
| Tenant | `demo` |
| AppProfile | `openproject-performant` |
| Valid from / to | subscription period |
| External id | Odoo SO line id |

### 4.3 Invoicing

| Gentian concept | Odoo model (typical) | Notes |
|---|---|---|
| **Monthly invoice** | `account.move` (`out_invoice`) | Generated from subscription or recurring SO |
| **Invoice lines** | `account.move.line` | One line per entitled app × tenant (or bundled line) |
| **Payment** | `account.payment` | Marks invoice paid; triggers or confirms fulfillment |
| **Revenue period** | invoice date / subscription period | Standard accounting |

Use Odoo **Subscriptions** or **recurring sales orders** for monthly billing.

### 4.4 Fulfillment integration

Use a **fulfillment service** (or n8n / Odoo automation) between Odoo and the cluster:

| Trigger | Action |
|---|---|
| SO confirmed + paid (OSS app) | Append `profile:` to tenant in `gentian-deployments` **or** call install API |
| SO confirmed + paid (premium app) | Enable ArgoCD app for `gentian-premium` path + append `profile:` |
| Subscription cancelled | Remove `profile:` from tenant; disable premium sync if no other entitlements |
| Entitlement expiry | Same as cancel; optional grace period in CRM only |

Odoo holds **commercial truth**; the cluster holds **desired infra state**.

---

## 5. Store behaviour

| Source | Typical `license` | Store listing | Install |
|---|---|---|---|
| **`gentian-apps`** | OSS SPDX (e.g. `Apache-2.0`) | Visible to all | GitOps / CLI |
| **`gentian-premium`** | `proprietary` | When entitled | After CRM grants entitlement |

**Pricing** lives on Odoo **`product.product`** (`list_price`, currency, recurring plan).
Map each sellable profile (or support plan) to a product. OSS profiles may use a €0
product for tracking.

**Support plans** (same `AppProfile`, paid support) use a separate Odoo service product;
install targets the same profile CR.

---

## 6. Related documents

| Topic | Document |
|---|---|
| Profile fields & versioning | [app-profile-versioning.md](app-profile-versioning.md) |
| Catalogue & install flow | [app-catalogue.md](app-catalogue.md) |
| Profile authoring | [gentian-apps/app-profile-guide.md](../../../gentian-apps/app-profile-guide.md) |
| IAM / roles | [iam.md](iam.md) |
