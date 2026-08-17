# Cross-repo seeds (manual install)

Cloud agents may not have push access to every Gentian repo. These seeds let you
seed or update sibling repositories from `gentian-os`.

Nothing here relates to backup: "export" in this repo means **tenant data
export** ([docs/plans/backup-plan.md](../docs/plans/backup-plan.md)), which is
why this directory is not called `export/`.

| Seed | Target repo | Contents |
|------|-------------|----------|
| [gentian-app-template/](gentian-app-template/) | `gentian-org/gentian-app-template` | App scaffold (FastAPI + React + Helm) |

The OSS catalogue lives in [gentian-apps](https://github.com/gentian-org/gentian-apps).
Further catalogues are supplied per deployment.

Each seed folder has a `MANUAL-INSTALL.md` with `tar` and git-bundle restore steps.
Tarball: `gentian-app-template.tar.gz`.

Git bundle (exact agent branch):

- `gentian-app-template-cursor-app-template-scaffold-6bce.bundle`
