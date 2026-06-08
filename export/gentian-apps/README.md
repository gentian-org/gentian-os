# gentian-apps

App Store catalogue for Gentian OS — `AppProfile` CRs plus first-party app implementations.

## Structure

```text
profiles/              # AppProfile YAML — synced to cluster by ArgoCD (gentian-appprofiles)
  openproject.yaml     # upstream chart (profile only)
  app-store.yaml       # first-party app (source in apps/app-store/)
apps/                  # first-party implementations (FastAPI + React + Helm)
  _template/           # copy of gentian-app-template
  app-store/           # tenant admin App Store UI
contracts/             # integration contract schemas
icons/                 # shared SVG assets
```

**Discovery:** catalogue = `profiles/` · implementation = `apps/<name>/`

## Guides

| Guide | Audience |
|-------|----------|
| [app-profile-guide.md](app-profile-guide.md) | Publish an **existing** Helm chart (profile YAML only) |
| [custom-app-guide.md](custom-app-guide.md) | Build a **new** Gentian-native app end-to-end |

## Adding an upstream app

Add `profiles/<app>.yaml` — see [app-profile-guide.md](app-profile-guide.md).

```bash
kubectl gentian apps list
```

## Adding a first-party app

1. Copy `apps/_template/` → `apps/<name>/`
2. Implement backend + frontend + chart
3. Add `profiles/<name>.yaml`
4. CI (`.github/workflows/apps-ci.yaml`) publishes images + OCI chart

See [custom-app-guide.md](custom-app-guide.md).

## Related repos

| Repo | Purpose |
|------|---------|
| [gentian-os](https://github.com/gentian-org/gentian-os) | Orchestrator, CRDs, kernel |
| [gentian-deployments](https://github.com/gentian-org/gentian-deployments) | Per-environment tenant state |
| [gentian-app-template](https://github.com/gentian-org/gentian-app-template) | Scaffold for new apps |
| [gentian-ui](https://github.com/gentian-org/gentian-ui) | Portal shell |
