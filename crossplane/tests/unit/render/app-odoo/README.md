# app-odoo render fixture

`composition.yaml` here is a **copy** of
`gentian-apps/profiles/odoo/base/base-ce/composition.yaml`, because that file lives in
another repository and git cannot symlink across repos.

That means it drifts silently: editing the composition in gentian-apps does not fail
anything here, and these tests will happily keep passing against the old copy. It did
drift — the copy still contained the per-addon install branch for a while after that
branch was deleted upstream. Refresh it with:

    cp ../../../../../../gentian-apps/profiles/odoo/base/base-ce/composition.yaml composition.yaml
    make test-unit-render-update

The companion `app-odoo-module` fixture was removed when addons stopped composing a
release of their own; an addon profile now renders no resources at all.
