# gentian-app-template

Scaffold for Gentian OS first-party apps: **FastAPI** backend, **React** frontend,
**Helm** chart, and **AppProfile** skeleton.

Derived from [full-stack-fastapi-template](https://github.com/fastapi/full-stack-fastapi-template)
with Gentian-specific packaging (OIDC, kernel Postgres, Pattern A secrets).

## Quick start

```bash
docker compose -f docker-compose.dev.yaml up --build
```

- API: http://localhost:8000/docs
- UI: http://localhost:5173

## Create a new app

1. Copy this repo to `gentian-apps/apps/<name>/` (or use Cookiecutter).
2. Rename chart, images, and `profile/appprofile.yaml.tmpl`.
3. Add `gentian-apps/profiles/<name>.yaml`.
4. See [custom-app-guide.md](https://github.com/gentian-org/gentian-apps/blob/main/custom-app-guide.md).

## Layout

```
backend/     FastAPI (Python 3.12+)
frontend/    React SPA (Vite + TypeScript + Tailwind)
chart/       Helm chart
profile/     AppProfile template
docs/        AGENTS.md
```
