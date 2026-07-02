# App profile portal tiles

App-menu and portal tile icons for dedicated-mode apps.

## Spec (`AppProfile.spec.tile`)

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

## Catalogue

Built from `gentian-ui/design-system/tiles/` (Lucide glyphs + Gentian frame).
Embedded in the operator at `internal/tiles/catalogue.json`.

Rebuild and sync:

```bash
gentian-ui/design-system/tiles/scripts/sync-consumers.sh
```

## CRD

`TileSpec` on `AppProfile` and `PortalTileSpec` — see `api/v1alpha1/appprofile_types.go`.
