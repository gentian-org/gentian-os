# Custom App Guide — build a Gentian-native application

This guide walks through creating a **new first-party app** (FastAPI + React) and
publishing it to the Gentian App Store. To wrap an **existing upstream Helm chart**
with an AppProfile only, see [app-profile-guide.md](app-profile-guide.md).

## Prerequisites

- A running Gentian OS cluster (see [gentian-os/getting-started.md](https://github.com/gentian-org/gentian-os/blob/main/getting-started.md))
- [gentian-app-template](https://github.com/gentian-org/gentian-app-template) cloned locally (e.g. `/develop`)
- Push access to `gentian-org/gentian-apps`

## Repository layout

First-party app **source** lives in this monorepo:

```text
gentian-apps/
├── apps/<app-id>/          # implementation (backend, frontend, chart)
└── profiles/<app-id>.yaml  # AppProfile catalogue entry
```

The cluster never mounts Git source. CI builds **pinned container images** and an
**OCI Helm chart**; the AppProfile references `spec.chart.version`.

## Step 1 — Scaffold from template

```bash
cp -a /develop gentian-apps/apps/my-app
cd gentian-apps/apps/my-app
```

Rename identifiers:

| File | Change |
|------|--------|
| `chart/Chart.yaml` | `name: my-app`, bump `version` |
| `chart/values.yaml` | image repositories `ghcr.io/gentian-org/my-app-api` etc. |
| `profile/appprofile.yaml.tmpl` | replace `APP_ID` |

## Step 2 — Implement features

### Backend (`backend/app/`)

- Add routes under `api/routes/`
- Read secrets from environment (injected by ExternalSecret / ESO)
- Use `Depends(get_current_user)` for tenant-scoped endpoints
- Expose `/healthz` and `/readyz`

### Frontend (`frontend/src/`)

- Build UI with React + Vite + Tailwind
- Call `/api/v1/...` (ingress routes `/api` to the API service in cluster)

### Local development

```bash
docker compose -f docker-compose.dev.yaml up --build
```

`AUTH_DISABLED=true` skips OIDC for local testing.

See [docs/AGENTS.md](apps/_template/docs/AGENTS.md) for agent-oriented conventions.

## Step 3 — Helm chart

Requirements for Gentian:

1. **Pattern A** — chart consumes `existingSecret` (see `chart/values.yaml`)
2. **Probes** on `/healthz` and `/readyz`
3. **ServiceAccount + RBAC** if the app talks to the Kubernetes API
4. Pin `image.tag` in `values.yaml` (semver for releases)

## Step 4 — AppProfile

Copy `profile/appprofile.yaml.tmpl` to `profiles/my-app.yaml` and set:

- `kernelRequirements` (typically `oidc` + `postgresql`)
- `valueMapping` keys aligned with chart values
- `ingress.subDomain` for `https://<sub>.<tenant-domain>`
- `portalTiles` for end-user apps (admin-only apps: omit tiles; see IAM docs)

Run validation:

```bash
python3 -c "import yaml; yaml.safe_load(open('profiles/my-app.yaml'))"
```

## Step 5 — CI and publish

`.github/workflows/apps-ci.yaml` builds changed apps under `apps/`.

On merge to `main`:

1. Docker images → `ghcr.io/gentian-org/<app>-api:<version>`
2. Helm chart → `oci://ghcr.io/gentian-org/charts/<app>:<version>`
3. Ensure `profiles/<app>.yaml` `spec.chart.version` matches

## Step 6 — Install for a tenant

**CLI (fallback):**

```bash
kubectl gentian apps install my-app --tenant gtn-demo
```

**App Store UI:** open `https://store.<tenant>.<kernel-domain>` (when `app-store` is installed).

## Admin vs user apps

- **User apps** — add `portalTiles` with `allowedGroup: App Users`
- **Admin apps** (e.g. App Store) — annotate `gentianos.io/platform-app: "true"`; expose via tenant-admin portal tile (see gentian-ui docs)

## Related

- [app-profile-guide.md](app-profile-guide.md) — catalogue-only / upstream charts
- [gentian-os/docs/design/app-catalogue.md](https://github.com/gentian-org/gentian-os/blob/main/docs/design/app-catalogue.md)
