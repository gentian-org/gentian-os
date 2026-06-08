# Manual install into gentian-apps

Copy everything in this folder (except this file) into your local clone root.

```bash
cd /path/to/gentian-apps
# tarball unpacks into the current directory (no --strip-components)
tar xzf /path/to/gentian-os/export/gentian-apps.tar.gz

git checkout -b cursor/app-template-and-store-6bce   # or your branch
git add -A
git commit -m "feat: add app template monorepo layout and App Store"
git push -u origin cursor/app-template-and-store-6bce
```

## Alternative: restore from git bundle

If you prefer preserving the exact commit from the cloud agent:

```bash
cd /path/to/gentian-apps
git fetch /path/to/gentian-os/export/gentian-apps-cursor-app-template-and-store-6bce.bundle \
  cursor/app-template-and-store-6bce:cursor/app-template-and-store-6bce
git checkout cursor/app-template-and-store-6bce
git push -u origin cursor/app-template-and-store-6bce
```

(Adjust the bundle path if you copied it from `gentian-os/export/`.)

## Layout

```text
profiles/              # AppProfile YAML — synced to cluster by ArgoCD
apps/
  _template/           # copy of gentian-app-template
  app-store/           # tenant admin App Store (FastAPI + React + Helm)
.github/workflows/     # CI: build images + OCI charts
icons/                 # shared SVG assets
```

## App Store local dev

```bash
cd apps/app-store
docker compose -f docker-compose.dev.yaml up --build
```

API: http://localhost:8000/docs  
UI: http://localhost:5173

## Related

- [custom-app-guide.md](custom-app-guide.md) — build new Gentian-native apps
- [app-profile-guide.md](app-profile-guide.md) — publish upstream Helm charts
- gentian-os PR with platform support: App Store install via GitOps + `CatalogueEntry.logo`
