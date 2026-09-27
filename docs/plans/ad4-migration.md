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

1. **Restore tile localisation, then close the gaps.** `ExposureTile` keeps
   `displayName` as the required fallback and gains an optional
   `displayNames` map of locale to string; `tilecatalogue.Tile` carries it and
   the desktop picks by the viewer's locale, falling back. Nothing else needs a
   new field.
2. **Wire privileges, because AD-4 lands on them.** `security.egress[]` becomes
   `requires.privileges.egress[]` and `security.macWaivers[]` becomes
   `requires.privileges.podSecurity[]`. Both are mechanical wraps — the egress
   `rule` is the same `networkingv1.NetworkPolicyEgressRule`, and a waiver keeps
   its `policy` and `scope` — but each entry gains a `name` and a `reason`, and
   both types say *"Name is what a PrivilegeGrant refers to"*. So the approval
   mechanism AD-5 describes and this migration's translation are one piece of
   work: `status.pendingPrivileges` populated, a granted counterpart, an
   approval action under `can_approve_privilege`, and an install held rather
   than silently unprivileged.
3. **Teach the app composition ComponentProfile.** It reads exactly eight
   fields of the profile spec — `appSecrets`, `chart`, `extraValues`, `ingress`,
   `kernelRequirements`, `postInstallJob`, `sidecars`, `valueMapping` — so this
   is eight renames, the `ExtraResources` kind, and the pipeline context key.
   The 1,827 lines are almost all rendering, not reading.
4. **The Go side: 35 non-test files.** Most are mechanical, because
   `AppProfile.spec.kernelRequirements` and
   `ComponentProfile.spec.requires.services` are the **same
   `*ServiceRequirements` type** — one line per site. The ones that are not
   mechanical are the egress and waiver readers from step 2:
   `netpolicy/build.go`, `netpolicy/internal.go`, `mac_waiver_reconciler.go`.
5. **`Tenant.spec.apps[].profile` names a ComponentProfile.** Admission
   already refuses a profile that is not in the catalogue, so the refusal moves
   with the kind.
6. **Convert the 38 documents.** Mechanical, from the table above.
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
