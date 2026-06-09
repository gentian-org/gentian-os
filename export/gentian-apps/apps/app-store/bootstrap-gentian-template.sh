#!/usr/bin/env bash
# Populates gentian-app-template from a streamlined FastAPI + React layout.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
APP_NAME="${1:-my-app}"

mkdir -p "$ROOT"/{backend/app/{api/routes,core,services,db},frontend/src/{components,pages,api},chart/templates,profile,docs,.github/workflows}

# README
cat > "$ROOT/README.md" <<'EOF'
# gentian-app-template

Scaffold for Gentian OS first-party apps: **FastAPI** backend, **React** frontend,
**Helm** chart, and **AppProfile** skeleton.

## Quick start

```bash
docker compose -f docker-compose.dev.yaml up --build
```

Copy this repo into `gentian-apps/apps/<name>/` when creating a new catalogue app.
See [docs/AGENTS.md](docs/AGENTS.md) and the Gentian [custom-app-guide](https://github.com/gentian-org/gentian-apps/blob/main/custom-app-guide.md).

## Layout

```
backend/     FastAPI API (Python 3.12+)
frontend/    React SPA (Vite + TypeScript + Tailwind)
chart/       Helm chart (Pattern A existingSecret)
profile/     AppProfile YAML template
docs/        AGENTS.md for AI/human contributors
```
EOF

echo "Bootstrap complete for $APP_NAME at $ROOT"
