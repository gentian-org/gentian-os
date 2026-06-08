# App Store

Tenant-admin web UI to browse the Gentian app catalogue and install/uninstall apps.

## API

- `GET /api/v1/catalogue/` — available apps (`AppCatalogue` + `AppProfile` metadata)
- `GET /api/v1/tenant/apps/installed`
- `POST /api/v1/tenant/apps/{profile}/install`
- `DELETE /api/v1/tenant/apps/{profile}`
- `GET /api/v1/tenant/apps/{profile}/status`

## Install modes

| `INSTALL_MODE` | Behaviour |
|----------------|-----------|
| `gitops` (default) | Patch `gentian-deployments` tenant YAML, commit+push |
| `k8s` | Create/delete namespace-scoped `App` claims directly |

## Local dev

```bash
docker compose -f docker-compose.dev.yaml up --build
```

Set `AUTH_DISABLED=true` and mock K8s/Git env vars for UI development.
