# ComponentProfile: Version, Edition and Tiles

**Companion to:** [app-catalogue.md](app-catalogue.md), [custom-catalogues.md](../custom-catalogues.md), [store-contract.md](store-contract.md)

This page covers three things about a profile: how it is identified, what its
edition means, and how it gets a tile. What a profile is and how one is
installed is [app-catalogue.md](app-catalogue.md); how to write one is
[custom-catalogues.md §4.1](../custom-catalogues.md) and the app profile guide
in `gentian-apps`. The field reference is the type
(`api/v1alpha1/componentprofile_types.go`, `profile_parts.go`).

---

## 1. Model

**`ComponentProfile`** is the catalogue unit: one profile, one name, one file
in a catalogue.

| Where | Field | Purpose |
|---|---|---|
| profile | `metadata.name` | The profile's name; the same app keeps it across builds |
| profile | `spec.version` | Version of the catalogue entry, not of the upstream project |
| profile | `spec.trustTier` | Review level (`platform`, `certified`, `experimental`); required |
| profile | `spec.package.chart.version` | The Helm chart pin, distinct from `spec.version` |
| catalogue index | `edition` | `ce`, `pe`, `me` or `ee`; absent means `ce` |
| catalogue index | `digest` | sha256 of the profile's bundle as served; what an install pins |

A profile has no `family`, `catalogueVersion`, `edition` or `license` field.
Descriptions, pictures, prices and licence terms are a store's listing, outside
the cluster ([store-contract.md](store-contract.md) §5).

**Editions.** `ce` is the upstream community edition; `pe` a private edition,
published by its own operator for their own tenants; `me` a maintained edition;
`ee` a commercially licensed one. The cluster gates none of them. It lists the
`ce` and `pe` entries of its catalogues to the command line; `me` and `ee`
entries are a store's to show. Whether something is paid for is decided where
it is handed over: a private chart or image arrives only where the tenant holds
a credential for its repository ([custom-catalogues.md §8](../custom-catalogues.md)).

---

## 2. Identity

An install names a profile and a build:

```yaml
# Tenant.spec.apps[]
- profile: openproject-ce
  catalogue: gentian          # first half of the coordinate <catalogue>/<profile>
  digest: sha256:…            # the bundle this install pinned
```

The digest is a field of the install, not part of the profile's name.
`profileRef` selects a profile by name only: resolving one by family, version
and edition was removed with the metadata it matched against, and such a
reference is refused.

---

## 3. Defaults

`spec.classes`, `spec.launch`, `spec.trustTier`, `spec.version` and
`spec.package` are required and have no default. In a catalogue's index an
entry without `edition` is `ce`.

---

## 4. Repositories

A profile reaches a cluster from a catalogue, at the digest an install names,
and in no other way ([custom-catalogues.md §1](../custom-catalogues.md)).
The default catalogue is built from `gentian-apps/profiles/`; anybody can
publish another.

---

## 5. Catalogue index

There is no catalogue object in the cluster. A catalogue's `index.yaml` lists
its entries (name, version, edition, digest, trust tier); the director fetches
it when asked and answers `kubectl gentian apps list --available`
([custom-catalogues.md §2](../custom-catalogues.md)).

---

## 6. Tiles

A tile is how an installed component appears on the desktop.

### 6.1 Spec

A tile belongs to an exposure (`spec.expose[].tile`), or to `spec.package.api`
for a component that is only a link to a service elsewhere.

```yaml
spec:
  launch: tile
  expose:
    - name: web
      surface: gateway
      authMode: oidc
      subDomain: projects
      backend: { service: openproject-ce, port: 8080 }
      tile:
        displayName: "Projects"
        displayNames: { de_DE: "Projekte" }
        description: "Plan and track work"
        image: assets/tile.svg                 # the author's copy, in git
        logo: data:image/svg+xml;base64,…      # what the cluster reads
        path: /                                # optional: where the tile leads
        relation: can_launch                   # what the person must hold
        object: app                            # app (default) | tenant | cluster
```

- `logo` is required and inline: every component brings its own SVG.
  `gentian-apps/scripts/sync-profile-tile.py` writes it from `image`.
- `relation` is required. A tile is shown to a person who holds that relation
  on the object: the component's own app object by default, the tenant for an
  administration tile (`can_administer`), the cluster for a service's console.
- `spec.launch` says how a person reaches the component: `tile`, `from`
  (another component opens it) or `none`. `tile` requires at least one tile.

### 6.2 How a tile reaches the desktop

The operator projects the tiles of every installed Component whose route
exists into one ConfigMap in `kernel-control`
(`internal/controller/tile_projection_reconciler.go`). The usher reads it and
answers, for the signed-in person, the tiles whose relation they hold. The
desktop shows that answer and opens what is clicked.

### 6.3 CRD

`ExposureTile` in `api/v1alpha1/componentprofile_types.go`.
