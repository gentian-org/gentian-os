# AD-4: one catalogue kind

`ComponentProfile` replaces `AppProfile`. This file is the inventory and the
order of work, written before any of it, because the migration crosses four
repositories and the thing that makes it tractable is knowing that almost
nothing has to be invented.

## Why now

Two kinds means two requirement systems — `AppProfile.spec.kernelRequirements`
(`ServiceRequirements`) and `ComponentProfile.spec.requires` (`RequirementSpec`)
— and the reconcilers are wired to one of them. That is not an abstract debt: it
is why "are the requirement reconcilers up to date" had no single answer, and
why the system-tier engines needed their admin Secrets found one at a time.

## Inventory

**Documents carrying `kind: AppProfile`**

| Repository | Catalogue entries | Other |
|---|---|---|
| `gentian-apps` | 33 `profiles/*/*/profile.yaml` | 2 templates, 3 `composition.yaml` |
| `gentian-pro` | 5 `profiles/*/profile.yaml` | 3 `composition.yaml` |
| `gentian-corp` | 0 | mentions in prose only |
| `gentian-os` | — | the CRD, `app-default.yaml`, the AppProject, the catalogue-sync path list |

`gentian-ui` has 7 mentions, all prose or dead after S7A.6.

**Field usage across the 38 profiles, and where each lands**

| `AppProfile` field | profiles using it | `ComponentProfile` home |
|---|---|---|
| `deploymentMethod` | 39 | `package`, the exactly-one union |
| `portalTiles` | 38 | `expose[].tile` |
| `compositionRef` | 19 | `package.composition` |
| `kernelRequirements` | 16 | `requires` |
| `appSecrets` | 13 | `secrets` |
| `optionalIntegrations` | 12 | `integrations` |
| `browserProxy` | 6 | **nowhere — retires with the catalogue sync** |
| `security` | 5 | `requires.privileges` (`egress`, `podSecurity`) |
| `sidecars` | 4 | `extensions` |
| `provisioning` (incl. `syncJob`) | 3 | `provisioning` — the same type, already present |
| `privilegedRole` | 3 | `requires.privileges.clusterRoles` |
| `postInstallJob` | 3 | `hooks` |
| `additionalIngresses` | 3 | further `expose[]` entries |
| `apiIntegration` | 2 | `package.api` |
| `derivedSecretKeys` | 1 | `secrets.derived` |

**Catalogue metadata leaves the cluster** (AD-3, and the component template
already says so): `family`, `edition`, `catalogueVersion`, `license`, `author`,
`categories`, `keywords`, `description`, `displayName`, `tile`, `logo`. It
belongs to the bundle the store serves, not to a CR. Three have in-cluster
readers to re-point or drop: `license` (2), `family` (2), `categories` (1).

`browserProxy` is the one field with no home, and its only reference in
gentian-os is the jq path list of the catalogue-sync ApplicationSet — which
AD-3 retires. Nothing in `gentian-ui` or `gentian-apps` reads it.

## What the field check actually found

Three of the four mappings need no new field at all, because they reuse the
same element types: `secrets.generated`/`secrets.derived` are `[]AppSecret` and
`[]DerivedSecretKey`, and `hooks.postInstall`/`hooks.provisioning` are
`*AppPostInstallJob` and `*ProvisioningSpec`. `security` is
`requires.privileges.egress` and `.podSecurity`.

The fourth found something worth stopping on. **`portalTiles` has no consumer
at all any more** — not the operator, not the composition, not the desktop. The
tile a person sees comes from `tile_projection_reconciler.go`, which reads
`ComponentProfile.spec.expose[].tile`. So those 38 `portalTiles` blocks are
already dead, and with them **30 tiles' worth of German translations**:
`PortalTile.displayName` is a `map[string]string` and `ExposureTile.displayName`
is a plain string.

Measured, not assumed: 40 `en_US` and 38 `de_DE` entries, of which 30 differ
between the two — "Automatisierung", "Abonnements", "Dateien", "Wissen",
"Präsentation". Thirteen are identical in both and lose nothing.

That regression has already happened; the strings are simply unused today.
What makes it this migration's business is that rewriting all 38 profiles is
when they would be **deleted**, and a silent deletion during a migration is the
worst time for it. So step 1 restores the capability rather than dropping it.

## What has to be done, in order

1. **[done] Restore tile localisation, then close the gaps.** `ExposureTile` keeps
   `displayName` as the required fallback and gains an optional
   `displayNames` map of locale to string; `tilecatalogue.Tile` carries it and
   the desktop picks by the viewer's locale, falling back. Nothing else needs a
   new field.
2. **[done] Wire privileges, because AD-4 lands on them.** `security.egress[]` becomes
   `requires.privileges.egress[]` and `security.macWaivers[]` becomes
   `requires.privileges.podSecurity[]`. Both are mechanical wraps — the egress
   `rule` is the same `networkingv1.NetworkPolicyEgressRule`, and a waiver keeps
   its `policy` and `scope` — but each entry gains a `name` and a `reason`, and
   both types say *"Name is what a PrivilegeGrant refers to"*. So the approval
   mechanism AD-5 describes and this migration's translation are one piece of
   work: `status.pendingPrivileges` populated, a granted counterpart, an
   approval action under `can_approve_privilege`, and an install held rather
   than silently unprivileged.

   Built, and it found one thing worth recording. `requires.privileges.egress`
   had **no reader at all** on the ComponentProfile path, and the AppProfile
   path applied `security.egress` unconditionally — so an app's declared
   outbound access was granted to every tenant that installed it, with nobody
   asked. `macWaivers` at least passed the `PlatformSecurityPolicy` allowlist.
   The gate now holds the install before it writes anything, and
   `security.GrantedEgressRules` is what puts a rule in the policy.

   Who approves follows from the kind, and that is the part the route shape had
   to bend for: egress is `tenant#can_approve_privilege`, pod security and
   cluster roles are `cluster#can_approve`. Requiring both would mean a
   security officer had to be a tenant administrator to waive a rule; requiring
   only the tenant's would let a tenant administrator waive a rule that
   protects every tenant on the node. So the routes carry `can_view` as the
   floor and `mayApprove` decides per kind, recording the relation that
   actually authorised it in the commit trailer rather than the route's.
3. **[done] Teach the app composition ComponentProfile.** It reads exactly eight
   fields of the profile spec — `appSecrets`, `chart`, `extraValues`, `ingress`,
   `kernelRequirements`, `postInstallJob`, `sidecars`, `valueMapping` — so this
   is eight renames, the `ExtraResources` kind, and the pipeline context key.
   The 1,827 lines are almost all rendering, not reading.

   It was nine reads, not eight — `kernelRequirements` is read three times, and
   a `sort -u` on the field name hid two of them. Eight were `dig` renames.
   The ninth was not: `spec.ingress.serviceName`/`servicePort` became a list of
   exposures, so the stable service alias now takes the first `surface:
   gateway` entry's `backend.service`/`backend.port`.

   One thing that had to move in the same commit: the Crossplane
   extra-resources **RBAC grant** in `crossplane/xrds/app.yaml` named
   `appprofiles`. That file's own comment says what a missing half looks like —
   the informer cannot sync, the step times out, every XApp stays
   `Synced=False`, and nothing in the logs mentions RBAC.

   All five app render fixtures pass against **unchanged goldens**, which is
   the check worth having: the same values read from a different shape render
   byte-identical output.
## The addon tiles, and where they land

Measured before converting anything. Of 33 catalogue profiles carrying 35
tiles, **21 profiles carry 23 tiles and have no ingress of their own** — the
Nextcloud and Odoo addons, whose tiles deep-link into the *base* app:
`?app=calendar`, `?open=spreadsheet`,
`/odoo/action-crm.action_your_pipeline?gentian_embed=1`.

This looked at first like a gap in ComponentProfile. It is not.
`target-component-structure.md` already answers it, in a table — *"an addon's
tile | the base's Service, named by `package.addon.of`"* — and with a worked
example. An addon's exposure names the base through `backend.component`; the
tile hangs off that exposure like any other. Nothing new is needed in the
schema, and `ExposureTile.path` takes `/?app=calendar` because a bare query is
written as `/?…`.

What *was* wrong was the converter, which filed `portalTiles` under
presentation and sent every tile out of the cluster with the store listing. A
tile carries `relation`, the permission a person must hold to see it, and that
is an authorization question the operator has to be able to read. Fixed: tiles
become `expose[].tile`, and an addon's exposure keeps the base's subdomain so
`cloud.<tenant>/?app=calendar` still opens what it always did.

`tile_projection_reconciler.go` mentions addons nowhere, so v5 shows none of
these 23 tiles today. That is implementation, not design, and it is the second
of the three gaps below.

## What is left, from the design's own list

1. `Tenant.spec.apps` resolves `AppProfile` only. Until a `ComponentProfile`
   can be installed into a tenant by naming it there, the catalogue cannot
   move. This is step 5 below.
2. The component reconciler refuses any package that is not a chart (*"only
   package.chart is reconciled yet"*). It has no addon path and no API path.
3. `BackendRef.component` is declared and unread: `buildExposureRoute` uses
   `e.Backend.Service` in the component's own namespace. For an addon the base
   is in that same namespace, so the Service resolves — but an addon's exposure
   must **not** create a second HTTPRoute, because it reuses the base's host
   and two routes matching `/` on one hostname conflict. The tile projection
   then has to read the host from the base's route rather than requiring one of
   the addon's own. *(Confirm: an addon reusing the base's host is what v0.4
   did and what keeps the URLs; giving each addon its own subdomain is the
   alternative and changes every addon URL.)*

4. **The Go side: 35 non-test files.** Most are mechanical, because
   `AppProfile.spec.kernelRequirements` and
   `ComponentProfile.spec.requires.services` are the **same
   `*ServiceRequirements` type** — one line per site. The ones that are not
   mechanical are the egress and waiver readers from step 2:
   `netpolicy/build.go`, `netpolicy/internal.go`, `mac_waiver_reconciler.go`.
5. **`Tenant.spec.apps[].profile` names a ComponentProfile.** Admission
   already refuses a profile that is not in the catalogue, so the refusal moves
   with the kind.
6. **Convert the 38 documents.** The converter does it and all 33 in
   `gentian-apps` pass the CRD's schema and its CEL rules; 145 review items
   remain, 34 of them icons where the old profile carried an SVG and the new
   field wants a glyph name.
7. **Re-point or drop the three metadata readers** (`license`, `family`,
   `categories`).
8. **Delete `AppProfile`**: the Go types, the CRD, the composition's old path,
   and the references in the AppProject and the sync path list.

## How each step is proved

A conversion is right when the composition renders the same objects from the
new profile as from the old. So step 6 is a render-fixture exercise, not a
reading exercise: the goldens that already exist for `app-default` —
`app-default`, `app-addons`, `app-sidecar`, `app-post-install-job`,
`app-volume-mapping` — must not change. Where one does, the diff is the
question to answer before moving on.

The Go side is proved by the existing suite plus one new assertion per
non-mechanical reader: an egress request renders the NetworkPolicy rule it
carries, and a waiver reaches Kyverno only once it is granted.
