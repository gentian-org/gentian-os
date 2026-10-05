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
| AD-2 | The director is the only writer of `gentian-deployments` | ✅ built end to end: two Ed25519 keys the installer generates, public halves and ids committed under `clusters/<id>/kernel/signing/`, `B-10` puts them in Argo CD's keyring and the director's private half in the vault, the AppProject renders `sourceIntegrity.git.policies[].gpg{mode: head}` scoped to the deployments repository alone, and the director signs what it commits. **Never exercised on a cluster** — whether Argo CD accepts the signatures is what S8 finds out |
| AD-3 | The store runs outside the cluster | ✅ an entry is fetched at the digest the store named, verified, and committed when a tenant installs it. A catalogue with no configured source still syncs wholesale |
| AD-4 | One catalogue kind, `ComponentProfile` | ✅ the type is gone, and deleting it found two live reads of a kind the catalogue stopped shipping — the integration-binding reconciler and `provisionAppGroupUsers`, both on the path to M4 — plus an installer step whose check tested for the deleted CRD |
| AD-5 | Privileges are requests with one approval path | ✅ |
| AD-6 | `authMode` mandatory; perimeter enabled per tenant | ✅ a perimeter approver publishes a surface under `can_expose`, bounded by an expiry; the operator stands a proxy in `tenant-<t>-dmz` that forwards only the declared prefixes with no session and no identity. `exposures.yaml` is the registry |
| AD-7 | Namespaces named by tier | ✅ |
| AD-8 | Kernel trust domains are separate namespaces | ✅ |
| AD-9 | System services have no public route | ✅ |
| AD-10 | The portal splits two ways; the platform is a tenant | ✅ |
| AD-11 | The target layout applies to fresh installs | ✅ |
| AD-12 | The authorization store is a projection; git holds the defaults | ◐ the projection works; the bootstrap drift check is on the backlog below |
| AD-13 | The edge is the only session authority | ✅ the text now describes what the code does: ending the session at Keycloak ends it, bounded by the access token's lifetime, with no revocation list for the bouncer to consult |
| AD-14 | Catalogue sources on the Cluster claim | ✅ `catalogue.sources[]` and `catalogue.storeUrl` are on the Cluster XRD and the installer scaffolds them; the director reads them from the claim in git, the operator projects each open source's tenants as `catalogue_source#open` declaratively, and the director serves each source's index at `GET /v1/tenants/{t}/catalogues[/{s}/entries]` — ce and pe only, no digest from an entitled source, and the rest counted and pointed at the store. The console renders it as a table, on purpose |
| AD-15 | Multi-language is a core requirement | ◐ desktop and console both translated; a component's `description` and the store listing's text are still single strings |

## AD-15 — multi-language

A person who cannot read the console cannot use it, so this is not polish.

| Surface | State |
| --- | --- |
| Tile labels on the desktop | ✅ `ExposureTile.displayNames`, projected, and `localisedLabel` picks by the viewer's locale |
| The desktop's own strings | ✅ i18next with JSON catalogues in `src/locales`, English and German, discovered by a glob — adding a language is adding a file |
| Keycloak login and account | ✅ every realm enables internationalization; the kernel realm reads `GENTIAN_SUPPORTED_LOCALES`, a tenant realm reads `spec.locales` |
| A language chooser | ✅ in Settings. Clearing it falls back to the tenant's language, not the browser |
| Where a person's language comes from | ✅ their own choice (which a settings template also sets, because a template copies preferences and language is one), then the tenant's, then the browser |
| Admin console | ✅ 542 strings in `en.json` and `de.json`, extracted with the TypeScript compiler; `npm run build` fails on an inline string, a missing key or a translation that dropped a `{{placeholder}}`. The German wants a native review before it is customer-facing |
| A component's other catalogue text | ☐ `description` and the store listing's text are single strings |
| The account's language | ✅ in the desktop's preferences database, one row per user per tenant, so it follows a person between machines. Browser storage is only a first-paint cache |
| A missing-translation check in CI | ◐ the console's build enforces it (`scripts/i18n.mjs --check`); the desktop still only documents the script |

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
| 8 | ✅ `AppCatalogue`, the profile webhook and now the `AppProfile` type itself. The file became `profile_parts.go` — the parts a `ComponentProfile` is made of, which is what it always held. The portal tile, the portal link target and the browser-proxy route went with the kind |

What deleting the type found, which is the argument for deleting a type
rather than leaving it defined and unused — a dead type keeps every reference
to it compiling, so code asking the API server for a kind nothing serves looks
exactly like code that works:

- `IntegrationBindingReconciler` fetched an `AppProfile` for the provider's
  Service. Since the catalogue converted, that `Get` has returned NotFound for
  every binding, so no contract credential has ever been seeded — M4.7.
- `applifecycle.provisionAppGroupUsers` did the same for one annotation, from
  four call sites.
- `D-08`'s `check()` tested `kubectl get crd appprofiles.gentianos.io`, which
  after the deletion never exists: the step would have reported MISSING on
  every run, before M1.

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
| ✅ | `--prepare-deployment` commits and pushes what it writes, signed with the break-glass key, and so does the install-time precondition. It used to say "commit and push them" and stop — and a `deployments-repository.yaml` left in the working copy is a director with no push credential, which surfaces as a 503 on the first write with nothing pointing back |
| ✅ | One layout. The v4 step set, kernel trees, `spec.layout`, the `InfraData` kind and `--layout` are gone, and the 50 library functions the v4 steps were the only caller of went with them. `make lint` now runs `lint-unreachable`, so a definition nothing reaches fails the build — the other half of `lint-resolvable` |
| ✅ | `GETTING-STARTED.md` names the claim set that exists, and says plainly that a leftover `claims/infra-data.yaml` must be deleted — the `InfraData` kind itself is gone, so nothing composes those engines twice |
| ☐ | S7A.4 — no write has ever succeeded against this cluster |
| ✅ | S7A.17's durable record: the director's own database on `kernel-postgres` holds who was allowed to ask for each identity change, with a retention horizon it enforces. Optional — a cluster without it starts and warns |
| ☐ | S7A.17's other half: the event listener recording Keycloak **admin** events, carrying the request id so the two records join. It projects group membership today and drops the rest |
| ☐ | S7A.7 and S7A.11 — built, never exercised in a browser |
| ✅ | S8 — purge and reinstall, which is what makes M1 reached rather than demonstrated (2026-10-03, beefy1) |

## Backlog — wanted, not now, and not forgotten

Each of these is a decision already taken. What is missing is the work, and
none of it blocks the purge.

| | Why it waits |
| --- | --- |
| **The bootstrap drift check** (AD-12): at start the director recomputes the defaults git implies and compares them to what OpenFGA holds, then REPORTS the difference | No urgency, and the reporting-not-fixing part is the point: a store that has diverged is a question, because overwriting it would erase exactly the grants and revocations that are nobody's default |
| **Simplify the package union back to one** | The union now admits a chart and a composition together, because three entries genuinely are both. If those three ever render their extra objects some other way — a hook, a sidecar, the chart itself — the pair stops being needed and the rule can go back to exactly one, which is easier to answer without reading it twice |

## Known and deliberately not now

- **Migrating a tenant to a different cluster is not a supported path.**
  Restore is same-cluster by construction: cluster-admin only, no console
  button, and it replaces live data. A kit + a backup rebuilds a tenant
  faithfully — definition from the repository, every derived credential
  identical from the master password and salt, data and member accounts from
  the bundle, everything but passwords, which are deliberately not in either.
  A DIFFERENT cluster has a different master password, so the restored app
  data would meet credentials it does not expect.

  Two shapes would work and they are a decision rather than a defect. Either
  the kit IS the cluster's identity — a new server that imports it becomes the
  old cluster, and migration reduces to restore, which is what the kit is
  already shaped for — or the restore re-keys every app credential on import,
  which is far more work and makes every app that caches a credential in its
  own database a special case. Nothing here implements either yet.


- A catalogue source has to publish `index.yaml` for a cluster to browse it.
  gentian-apps' CI builds the flat, https-served shape on every run
  (`scripts/build-catalogue-source.py`) and deploys it to GitHub Pages **from
  `main` only** — one repository has one Pages site, so publishing from
  develop as well would make the catalogue whichever branch ran last. Pages is
  enabled. The catalogue therefore follows releases: until the work merges to
  `main`, a cluster that wants the in-progress one declares no source and
  syncs it wholesale from git, as every cluster did before AD-3.
  `https://gentian-org.github.io/gentian-apps` is the URL for
  `spec.catalogue.sources[].url`.
- The cluster's catalogue view lists; it does not install. Installing stays the
  tenant's own act from their own screens, because a third place that installs
  apps — after the store and the desktop — is a third place to keep correct.
- The director reads `catalogue.sources[]` once, at start. An edit to the claim
  reaches it when its Deployment next rolls, which an Argo sync of a changed
  claim produces anyway. The tuples that decide *which tenant* a source is open
  to are the operator's and are reconciled continuously, so the access half is
  never stale — only the list of URLs is.
