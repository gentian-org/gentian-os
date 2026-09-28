# Open items on the way to M3

One list, kept current. An item leaves it when the thing works on a cluster,
not when the code exists.

M1 is *the platform administrator signs in and the installer leaves a cluster a
tenant can be provisioned on*. M2 is *the first functional tenant*. M3 is *the
first user invited by a tenant administrator*. The milestones and their steps
live in [implementation-plan.md](implementation-plan.md); this file is only
what is still open and why.

## The architectural decisions, against the code

Checked against the implementation rather than against the plans, because the
plans are what the code was supposed to become.

| AD | | State |
| --- | --- | --- |
| AD-1 | Nine security principles normative | ✅ |
| AD-2 | The director is the only writer of `gentian-deployments` | ◐ the director writes; **commit signing and `sourceIntegrity` are not implemented**. Deferred deliberately — a separate, confined piece of work |
| AD-3 | The store runs outside the cluster | ◐ contract and grant format exist; materialise-on-reference is not wired |
| AD-4 | One catalogue kind, `ComponentProfile` | ◐ steps 1–3 done; see below |
| AD-5 | Privileges are requests with one approval path | ✅ |
| AD-6 | `authMode` mandatory; perimeter enabled per tenant | ◐ `authMode` and `surface` are enforced; **no publishing proxy exists** — `layout.TenantDMZ()` is defined and called nowhere |
| AD-7 | Namespaces named by tier | ✅ |
| AD-8 | Kernel trust domains are separate namespaces | ✅ |
| AD-9 | System services have no public route | ✅ |
| AD-10 | The portal splits two ways; the platform is a tenant | ✅ |
| AD-11 | The target layout applies to fresh installs | ✅ |
| AD-12 | The authorization store is a projection; git holds the defaults | ◐ the projection works; **the bootstrap check that compares git's defaults to the store's does not exist** |
| AD-13 | The edge is the only session authority | ◐ **the AD and the code disagree.** The AD says back-channel logout makes the director write `session:<sid>#revoked` and the shim denies on it; `decider.go` removed that path on purpose, relying on short-lived tokens and a refresh that fails against an ended session. One of the two is wrong and it should be the text, but that is a decision |
| AD-14 | Catalogue sources on the Cluster claim | ☐ **nothing built.** `catalogue.sources[]` is not on the XRD, no tuples are written, and the director serves no index. The authorization model already has `can_install: entitled or open from source` |
| AD-15 | Multi-language is a core requirement | ☐ see below |

## AD-15 — multi-language

The market is German-speaking, so this is not polish.

| Surface | State |
| --- | --- |
| Tile labels on the desktop | ✅ `ExposureTile.displayNames`, projected, and `localisedLabel` picks by the viewer's locale |
| Everything else in the desktop | ☐ every string inline in English; no catalogue, no library |
| Admin console | ☐ the same, and nothing started |
| Keycloak login and account | ☐ Keycloak ships the translations; the realm never enables them, so it serves English. A realm field, and the cheapest of these by a wide margin |
| A component's other catalogue text | ☐ `description` and the store listing's text are single strings |
| Where the viewer's language comes from | ☐ the browser today. AD-15 says the account, and the browser until they have said |

## AD-4 — what is left

Steps 1–3 are done: tile localisation, the privilege wiring, and the app
composition reading `ComponentProfile`. The converter matches the API and every
profile it writes passes the CRD's schema and its CEL rules.

| | |
| --- | --- |
| 4 | The Go side: 38 non-test files still name `AppProfile`. Mostly one line each, because `kernelRequirements` and `requires.services` are the same type |
| 5 | `Tenant.spec.apps[].profile` resolves `AppProfile` only. Until a `ComponentProfile` can be named there, the catalogue cannot move |
| 6 | Run the conversion in `gentian-apps` and `gentian-pro` for real, and settle the 119 review items — 8 of them tiles on the placeholder |
| 7 | Re-point the three metadata readers (`license`, `family`, `categories`) |
| 8 | Delete `AppProfile`, `AppCatalogue` and the `App` claim |

Two implementation gaps the design already names:

- The component reconciler refuses any package that is not a chart. No addon
  path, no API path.
- `BackendRef.component` is declared and unread. An addon's exposure reuses the
  base's host and must therefore **not** create a second HTTPRoute on it; the
  tile projection reads the host from the base's route instead. Confirmed: no
  dedicated URL per addon.

## The installer, to M3

| | |
| --- | --- |
| ✅ | `claims/deployments-repository.yaml` is required on v5, and an uncommitted working copy is named. Without that claim the director has no push credential and every write answers 503 — no tenant, no invited user |
| ☐ | Deployment preparation is not DRY: `--prepare-deployment` writes the files and nothing commits them, so the claim reaches the cluster only if somebody remembers. One writer, folded into the normal run |
| ☐ | `GETTING-STARTED.md` still names `claims/infra-data.yaml`, which v5 does not have and which would compose the system-tier engines a second time |
| ☐ | S7A.4 — no write has ever succeeded against this cluster |
| ☐ | S7A.17's second half: the event listener recording admin events with the request id read back, and a durable home for the director's record of the authority. Against M3 |
| ☐ | S7A.7 and S7A.11 — built, never exercised in a browser |
| ☐ | S8 — purge and reinstall, which is what makes M1 reached rather than demonstrated |

## Known and deliberately not now

- Commit signing and `sourceIntegrity` (AD-2). Confined, and separable.
- The tenant-DMZ publishing proxy and its registry of shared URLs (AD-6).
  Wanted, and larger than the path to M3.
