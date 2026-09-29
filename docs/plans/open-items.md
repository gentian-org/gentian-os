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
| AD-2 | The director is the only writer of `gentian-deployments` | ◐ the director writes; **commit signing and `sourceIntegrity` are not implemented**. On the backlog below |
| AD-3 | The store runs outside the cluster | ✅ an entry is fetched at the digest the store named, verified, and committed when a tenant installs it. A catalogue with no configured source still syncs wholesale |
| AD-4 | One catalogue kind, `ComponentProfile` | ◐ steps 1–7 done; `AppProfile` the Go type is unused but not yet deleted |
| AD-5 | Privileges are requests with one approval path | ✅ |
| AD-6 | `authMode` mandatory; perimeter enabled per tenant | ✅ a perimeter approver publishes a surface under `can_expose`, bounded by an expiry; the operator stands a proxy in `tenant-<t>-dmz` that forwards only the declared prefixes with no session and no identity. `exposures.yaml` is the registry |
| AD-7 | Namespaces named by tier | ✅ |
| AD-8 | Kernel trust domains are separate namespaces | ✅ |
| AD-9 | System services have no public route | ✅ |
| AD-10 | The portal splits two ways; the platform is a tenant | ✅ |
| AD-11 | The target layout applies to fresh installs | ✅ |
| AD-12 | The authorization store is a projection; git holds the defaults | ◐ the projection works; the bootstrap drift check is on the backlog below |
| AD-13 | The edge is the only session authority | ✅ the text now describes what the code does: ending the session at Keycloak ends it, bounded by the access token's lifetime, with no revocation list for the shim to consult |
| AD-14 | Catalogue sources on the Cluster claim | ◐ `catalogue.sources[]` is on the Cluster XRD and the installer scaffolds it; the director reads its sources from the claim in git instead of from an environment variable, and the operator projects each open source's tenants as `catalogue_source#open`, declaratively — a tenant the claim stops naming loses the access. What is left is the **index**: the director serves none, so a desktop store screen still needs the store to be reachable |
| AD-15 | Multi-language is a core requirement | ☐ see below |

## AD-15 — multi-language

The market is German-speaking, so this is not polish.

| Surface | State |
| --- | --- |
| Tile labels on the desktop | ✅ `ExposureTile.displayNames`, projected, and `localisedLabel` picks by the viewer's locale |
| The desktop's own strings | ✅ i18next with JSON catalogues in `src/locales`, English and German, discovered by a glob — adding a language is adding a file |
| Keycloak login and account | ✅ every realm enables internationalization; the kernel realm reads `GENTIAN_SUPPORTED_LOCALES`, a tenant realm reads `spec.locales` |
| A language chooser | ✅ in Settings. Clearing it falls back to the tenant's language, not the browser |
| Where a person's language comes from | ✅ their own choice (which a settings template also sets, because a template copies preferences and language is one), then the tenant's, then the browser |
| Admin console | ☐ nothing started. Same approach as the desktop; it is a separate app in `gentian-apps` |
| A component's other catalogue text | ☐ `description` and the store listing's text are single strings |
| The account's language | ✅ in the desktop's preferences database, one row per user per tenant, so it follows a person between machines. Browser storage is only a first-paint cache |
| A missing-translation check in CI | ☐ `src/locales/README.md` has the script; nothing runs it |

## AD-4 — what is left

Steps 1–3 are done: tile localisation, the privilege wiring, and the app
composition reading `ComponentProfile`. The converter matches the API and every
profile it writes passes the CRD's schema and its CEL rules.

| | |
| --- | --- |
| 4 | ✅ Every Go reader, 20 files plus 35 test files |
| 5 | ✅ `Tenant.spec.apps[].profile` resolves a `ComponentProfile`; `profileRef` by catalogue identity is retired with AD-3's metadata |
| 6 | ✅ 33 profiles in `gentian-apps` and 5 in `gentian-pro`, converted in place with their comments. 135 review items remain, 8 of them tiles on the placeholder |
| 7 | ✅ `license` became an annotation, `family` became the chart's name, `categories` went with the webhook that checked it |
| 8 | ◐ `AppCatalogue` and the profile webhook are gone. The `AppProfile` Go type is unused but still defined: `appprofile_types.go` is 1,559 lines and most of it is types `ComponentProfile` still uses, so deleting it is a split rather than a delete |

Two implementation gaps the design already names, and both are still open:

- The component reconciler refuses any package that is not a chart. No addon
  path, no API path. This is what stops an addon's tile appearing on the v5
  Component path; the v4 App path, which is what a tenant's apps still use,
  renders them through the Compositions as before.
- `BackendRef.component` is declared and unread. An addon's exposure reuses the
  base's host and must therefore **not** create a second HTTPRoute on it; the
  tile projection reads the host from the base's route instead. Confirmed: no
  dedicated URL per addon.

One question the migration raised and did not answer: **should the package
union admit a `composition` alongside a `chart`?** Three apps are delivered as
a chart *and* rendered by their own Composition, which emits a portal bridge, an
SSO sidecar or a stable alias beside the Release. `spec.compositionRef` said so
and the union's exactly-one rule cannot, so it survives as the annotation
`gentianos.io/composition`.

## The installer, to M3

| | |
| --- | --- |
| ✅ | `claims/deployments-repository.yaml` is required on v5, and an uncommitted working copy is named. Without that claim the director has no push credential and every write answers 503 — no tenant, no invited user |
| ☐ | Deployment preparation is not DRY: `--prepare-deployment` writes the files and nothing commits them, so the claim reaches the cluster only if somebody remembers. One writer, folded into the normal run |
| ☐ | `GETTING-STARTED.md` still names `claims/infra-data.yaml`, which v5 does not have and which would compose the system-tier engines a second time |
| ☐ | S7A.4 — no write has ever succeeded against this cluster |
| ✅ | S7A.17's durable record: the director's own database on `kernel-postgres` holds who was allowed to ask for each identity change, with a retention horizon it enforces. Optional — a cluster without it starts and warns |
| ☐ | S7A.17's other half: the event listener recording Keycloak **admin** events, carrying the request id so the two records join. It projects group membership today and drops the rest |
| ☐ | S7A.7 and S7A.11 — built, never exercised in a browser |
| ☐ | S8 — purge and reinstall, which is what makes M1 reached rather than demonstrated |

## Backlog — wanted, not now, and not forgotten

Each of these is a decision already taken. What is missing is the work, and
none of it blocks the purge.

| | Why it waits |
| --- | --- |
| **Deployment authorization** (AD-2): commit signing and Argo's `sourceIntegrity`, so the cluster syncs only commits the director or the break-glass key signed | Confined and separable. Until it lands, git is trusted because of who can push to it rather than because of what the commit carries |
| **The bootstrap drift check** (AD-12): at start the director recomputes the defaults git implies and compares them to what OpenFGA holds, then REPORTS the difference | No urgency, and the reporting-not-fixing part is the point: a store that has diverged is a question, because overwriting it would erase exactly the grants and revocations that are nobody's default |
| **Simplify the package union back to one** | The union now admits a chart and a composition together, because three entries genuinely are both. If those three ever render their extra objects some other way — a hook, a sidecar, the chart itself — the pair stops being needed and the rule can go back to exactly one, which is easier to answer without reading it twice |

## Known and deliberately not now

- AD-14's **index**. The director knows which catalogues this cluster may fetch
  from and which tenant each open one admits, but it serves no listing of what
  is in them. Until it does, the desktop's store screen needs the store itself
  to be reachable; AD-14 wants it to render from the cluster's own copy and let
  the store add listings when it is there.
- The director reads `catalogue.sources[]` once, at start. An edit to the claim
  reaches it when its Deployment next rolls, which an Argo sync of a changed
  claim produces anyway. The tuples that decide *which tenant* a source is open
  to are the operator's and are reconciled continuously, so the access half is
  never stale — only the list of URLs is.
