# Cross-repo exports (manual install)

Cloud agents may not have push access to every Gentian repo. These exports let you
seed or update sibling repositories from `gentian-os`.

| Export | Target repo | Contents |
|--------|-------------|----------|
| [gentian-app-template/](gentian-app-template/) | `gentian-org/gentian-app-template` | App scaffold (FastAPI + React + Helm) |

The OSS catalogue lives in [gentian-apps](https://github.com/gentian-org/gentian-apps);
OpenDesk profiles live in [gentian-pro](https://github.com/gentian-org/gentian-pro).

Each export folder has a `MANUAL-INSTALL.md` with `tar` and git-bundle restore steps.
Tarball: `gentian-app-template.tar.gz`.

Git bundle (exact agent branch):

- `gentian-app-template-cursor-app-template-scaffold-6bce.bundle`
