# Manual install into gentian-app-template

Copy everything in this folder (except this file) into your local clone root.

```bash
cd /path/to/gentian-app-template   # or your /develop clone
# extract archive contents into repo root (not into a subfolder)
tar xzf gentian-app-template.tar.gz --strip-components=1

git add -A
git commit -m "feat: add Gentian app template (FastAPI + React + Helm)"
git push origin main
```

## Layout

```
backend/          FastAPI API
frontend/         React SPA (Vite + Tailwind)
chart/            Helm chart for Gentian deployment
profile/          AppProfile YAML template
docs/AGENTS.md    Conventions for humans and AI agents
docker-compose.dev.yaml
README.md
```

## Local dev

```bash
docker compose -f docker-compose.dev.yaml up --build
```

API: http://localhost:8000/docs  
UI: http://localhost:5173
