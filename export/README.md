# Cross-repo exports (manual install)

Cloud agents may not have push access to every Gentian repo. These exports let you
seed or update sibling repositories from `gentian-os`.

| Export | Target repo | Contents |
|--------|-------------|----------|
| [gentian-app-template/](gentian-app-template/) | `gentian-org/gentian-app-template` | App scaffold (FastAPI + React + Helm) |
| [gentian-apps/](gentian-apps/) | `gentian-org/gentian-apps` | App catalogue + App Store implementation |

Each folder has a `MANUAL-INSTALL.md` with `tar` and git-bundle restore steps.
Tarballs: `gentian-app-template.tar.gz`, `gentian-apps.tar.gz`.

Git bundles (exact agent branches):

- `gentian-app-template-cursor-app-template-scaffold-6bce.bundle`
- `gentian-apps-cursor-app-template-and-store-6bce.bundle`
