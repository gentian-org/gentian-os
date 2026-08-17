# AGENTS.md — Gentian app development conventions

This file helps AI coding agents and humans extend Gentian first-party apps.

## Directory map

| Path | Purpose |
|------|---------|
| `backend/app/main.py` | FastAPI entrypoint |
| `backend/app/core/config.py` | Settings from environment (ESO-injected in cluster) |
| `backend/app/core/auth.py` | OIDC JWT validation |
| `backend/app/api/routes/` | HTTP routers |
| `frontend/src/` | React UI |
| `chart/` | Helm chart (Pattern A `existingSecret`) |
| `profile/appprofile.yaml.tmpl` | AppProfile skeleton for `gentian-apps/profiles/` |

## Add an API endpoint

1. Create `backend/app/api/routes/<feature>.py` with an `APIRouter`.
2. Register it in `backend/app/main.py`.
3. Protect routes with `Depends(get_current_user)` when tenant-scoped.

## Add a React page

1. Add component under `frontend/src/pages/`.
2. Wire routing in `frontend/src/App.tsx` (or add a router).
3. Call backend via `/api/v1/...` (proxied by nginx in production).

## Kernel secrets (cluster)

Never commit secrets. The orchestrator injects via ExternalSecret:

- `DATABASE_URL`, `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`

Map keys in `profile/appprofile.yaml.tmpl` `valueMapping` must match Helm `values.yaml`.

## Publish a new app version

1. Bump `chart/Chart.yaml` version and image tags in `chart/values.yaml`.
2. CI builds and pushes images + OCI chart.
3. Update `gentian-apps/profiles/<app>.yaml` `spec.chart.version`.
4. AppProfile update reconciler rolls out to tenants.

## Local dev

```bash
docker compose -f docker-compose.dev.yaml up --build
```

`AUTH_DISABLED=true` skips OIDC locally.
