# Built bundles

Profile bundles exactly as gentian-apps publishes them: the output of its
`scripts/build-catalogue-source.py` (`dist/catalogue/profiles/<name>.yaml`),
copied here unchanged so that these tests need no other repository.

Built from gentian-apps commit `a04b9f6` (branch `develop`), all nine.

They are fixtures for what the two repositories have to agree on: each must be
a bundle the director accepts and the operator verifies
(`companions_test.go`, and `internal/director/api/bundles_test.go`). When
gentian-apps changes the shape of a bundle's companions, rebuild there and
copy the affected files again; do not edit them here.

| Bundle | Beside the profile |
|---|---|
| `activepieces-me` | a ConfigMap (the sign-in handler), two Customizations |
| `docmost-ce` | a ConfigMap (the sign-in handler), a Customization; the profile has a post-install job |
| `element-ce` | a Composition, a ConfigMap, an OIDCPackCatalog |
| `nextcloud-base-ce` | a ConfigMap, an OIDCPackCatalog |
| `nextcloud-base-od` | an OIDCPackCatalog |
| `nextcloud-calendar-ce` | nothing: an add-on, a bundle of its own in the same format |
| `odoo-base-ce` | a Composition |
| `openproject-ce` | a ConfigMap (the sign-in handler), a Customization |
| `xwiki-ce` | an OIDCPackCatalog |
