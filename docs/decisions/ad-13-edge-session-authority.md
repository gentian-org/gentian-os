---
audience: architect, auditor
type: decision
---

# AD-13 The edge is the only session authority

One confidential OIDC client per tenant zone, on the `authenticated` Gateway,
mints the one edge cookie. No platform component downstream runs a code flow
or holds a client secret, with one exception: the bouncer holds each realm's
`gentian-edge-exchange` client secret, to exchange a session's token for one
valid at a single app (`exchangeToken`). The Gateway forwards the edge token
only on a route whose entry sets `forwardToken`; every other backend receives
identity headers. An approved entry may keep the app's own `Authorization`
header (`clientAuthorization: app`); the session stays required. Ending a
session at Keycloak is what ends it: the access token's lifetime is the bound
on how long a signed-out person is still let through, so that lifetime is the
control, and no revocation list exists. The sign-in sidecar is the one
component beside an app that signs people in; the four conditions it is held
to and what it costs are in [security/design.md §2.12](../security/design.md) and
[iam.md §1.11](../concepts/iam.md). A call to a partner on a person's behalf is not an
exception (AD-17). The detail is [routing.md §4](../concepts/routing.md).
