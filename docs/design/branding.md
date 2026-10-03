# Branding

Every page the platform serves shows one brand: the sign-in router, the
identity provider's screens, the desktops, the consoles and the edge's
access-denied page. A provider running the cluster under its own name sets it
once; without it the pages show the platform's own.

## The brand

A cluster-scoped `Branding` named `default`, committed among the cluster's
declarations (`clusters/<cluster>/kernel/claims/branding.yaml`) by the director:
`GET` / `PUT /v1/clusters/{c}/branding`, `can_audit` / `can_configure`.

| Part | Format |
|---|---|
| `identity` | The Web App Manifest's own members: `name`, `shortName`, `description`, `icons` (https URLs, or images as data URLs) |
| `tokens` | A design-token document in the W3C Design Tokens Community Group format, 2025.10 — the format design tools export |
| `hideVendorPromotions` | Takes the platform vendor's offers off the consoles |

Pages read these tokens, and keep their own value for one that is absent:
`color.brand.50`…`800` (the primary scale), `color.ink.0`…`5` (text),
`color.paper.0`…`3` (surfaces), `color.status.{info,success,warning,danger}`
and their `-bg`, `font.family.{sans,display,mono}`, `radius.1`…`3`.

The director renders the brand before it commits and refuses one the pages
could not show: an unsupported `$type`, a value that would end its CSS
declaration, anything that would fetch (`url(`).

## Publishing

The operator renders the Branding — the platform's own tokens underneath, the
brand's over them path by path — into the `branding` ConfigMap beside Keycloak:

| File | What it is |
|---|---|
| `brand.css` | Every token as a `--brand-<group>-<token>` custom property; aliases become `var()` references |
| `brand.json` | The identity, with icons as files beside it |
| `brand.webmanifest` | A Web App Manifest: name, icons, `theme_color` from `color.brand.500` |
| `icon-<n>.<ext>` | An icon a data URL carried, decoded |

The `sign-in` Deployment serves them at `https://id.<kernel>/branding/`
(`brand.json` with CORS), and every page loads them from there. A brand whose
tokens cannot be rendered leaves the published files as they were, and its
status says why (`Rendered=False`).

The brand's name is also the sender of every realm's mail and the label of the
clients and sign-in options a person sees in the account console.
