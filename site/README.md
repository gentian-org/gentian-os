# The documentation site

[Docusaurus](https://docusaurus.io) builds docs.gentian.org from the pages in
the gentian-* repositories. Nothing here is a page: `prepare.py` copies the
pages into `content/` (not committed) and rewrites their links, and the site
is built from that copy. The pages themselves live in [docs/](../docs/) and
in the sibling repositories.

Build it locally:

```bash
cd site
npm ci
GENTIAN_APPS_DIR=../../gentian-apps GENTIAN_UI_DIR=../../gentian-ui npm run prepare-content
npm run build      # or: npm run start, for a live preview
```

`make docs-site` at the repository root does the same. The workflow
`.github/workflows/docs-site.yaml` builds on every pull request that touches
the pages and deploys the `develop` branch to GitHub Pages.
