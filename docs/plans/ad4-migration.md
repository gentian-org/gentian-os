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

## What has to be done, in order

1. **Close the two field gaps.** Verify `secrets` covers `appSecrets` +
   `derivedSecretKeys`, `hooks` covers `postInstallJob`, and `expose[]` covers
   `ingress` + `additionalIngresses` + `portalTiles`. Add only what is missing.
2. **Teach the app composition ComponentProfile.** `app-default.yaml` reads
   `AppProfile` today; the tenant app path is the one place the two kinds are
   not yet interchangeable.
3. **`Tenant.spec.apps[].profile` names a ComponentProfile.** Admission
   already refuses a profile that is not in the catalogue, so the refusal moves
   with the kind.
4. **Convert the 38 documents.** Mechanical, from the table above, and
   verifiable: every converted profile must render the same objects through the
   composition as it did before.
5. **Re-point or drop the three metadata readers.**
6. **Delete `AppProfile`**: the Go types, the CRD, the composition's old path,
   and the references in the AppProject and the sync path list.

## How each step is proved

A conversion is right when the composition renders the same objects from the
new profile as from the old. So step 4 is a render-fixture exercise, not a
reading exercise: one fixture per shape (chart, composition, api, addon,
sidecar, post-install job, privileged role), and the goldens are the ones that
already exist for `app-default`.
