---
audience: operator
type: guide
---

# Identity brokering: people of one realm signing in to another

People who already have an account in one realm — another tenant of this
cluster, or a tenant of another cluster — sign in to a second realm with that
account. Keycloak makes the account; what the person may do there is a
position a tenant's administrator gives them. This page is the configuration, done in Keycloak's
administration console, except between two tenants of one cluster, which is
[declared in git](#both-on-this-cluster-declared-in-git).

Two realms take part:

| | Here called | Example |
|---|---|---|
| where the people's accounts are | the **source** realm | `team` |
| where they are to sign in | the **target** realm | `customers` |

The target trusts the source completely about who a person is. Decide that
before you start: a source realm whose administrators you do not trust is an
account in the target for anybody they make.

## Before you start

- **Access.** Each realm has its own administration console:
  `https://id.<kernel-domain>/auth/admin/<realm>/console/`. Opening it takes
  two sign-ins: first the platform's (the page is behind the kernel session,
  for whoever may configure the cluster), then the realm's own
  administrator — the tenant's administrator account, `admin@<tenant>.<kernel-domain>`,
  which holds `realm-admin` in its realm and in no other. So step 1 is done
  as the source tenant's administrator, steps 2 and 3 as the target
  tenant's. The `master` realm's console is refused at the edge for
  everybody.
- **The platform's own brokers are not this.** A tenant realm may carry the
  identity providers `kernel` and `vouch-*`, which the platform composes and
  hides on the sign-in page. Leave them alone, and do not add the kernel
  realm as a source by hand.
- **Rights come from positions in the target.** A person who arrives through a
  broker gets an account in the target realm, and nothing else — like a
  person who signed up. What they may do is what the positions they are
  given hold (step 3). No group of a realm grants anything
  ([iam.md §1.2](../concepts/iam.md)). This holds even in a target that takes no sign-ups: brokering
  makes accounts there regardless.

## 1. In the source realm: a client for the target

*Clients* → *Create client*:

| Field | Value |
|---|---|
| Client type | OpenID Connect |
| Client ID | `broker-<target>`, e.g. `broker-customers` |
| Client authentication | On — without it the client is public and has no secret, and the *Credentials* tab does not appear |
| Authentication flow | Standard flow only |
| Valid redirect URIs | `https://id.<kernel-domain>/auth/realms/<target>/broker/<alias>/endpoint` — `<alias>` is the name step 2 gives the provider |

Save. A tab *Credentials* appears at the top: copy the *Client Secret*
there. (A client already made with *Client authentication* off: switch it on
under *Settings* → *Capability config*, and save.)

For a source realm on another cluster, its administrator does this step
there, with that cluster's domain in the URL above replaced by this one's.

## 2. In the target realm: the source as an identity provider

*Identity providers* → *Keycloak OpenID Connect*. The form shows the
provider's *Redirect URI* at the top: it is the one step 1 needs, once the
alias below is filled in.

| Field | Value |
|---|---|
| Alias | short and stable, e.g. `team`; it is part of the redirect URI of step 1 |
| Display name | what the sign-in page's button says, e.g. "Team account" |
| Discovery endpoint | `https://id.<source-domain>/auth/realms/<source>/.well-known/openid-configuration` — the page every realm publishes with its addresses; Keycloak fills in the rest from it |
| Client authentication | Client secret sent as post |
| Client ID / Client secret | from step 1 |
| Trust email | On — the source has confirmed it; otherwise the target asks again |
| Sync mode (under *Advanced settings*) | Force — name and e-mail follow the source at every sign-in |
| Hide on login page | Off — the button appears under the sign-in form |

A person whose e-mail address already has an account in the target is asked,
once, to confirm that it is theirs before the two are linked (Keycloak's
*first broker login* flow).

## 3. In the target tenant: give arrivals a position

A person who arrived holds nothing until they are given a position. They
appear in the target tenant's Admin Console under *Members* after their
first sign-in; give them the position User, which lets them enter the
tenant, and any position of the tenant's own.

Try it with a person of the source: the target's sign-in page shows the
button (or, to go straight to the source, add `&kc_idp_hint=<alias>` to a
sign-in URL of the target). After signing in at the source they are back in
the target, and refused until the position is given.

## Both on this cluster: declared in git

When source and target are tenants of the same cluster, steps 1 to 3 are not
done by hand at all. The target names the source and the positions its
people are given; the source consents:

```yaml
# the target, e.g. customers
spec:
  positions:
    - name: website-editor           # a position of the tenant's own
  signInFrom:
    - tenant: team
      positions: [website-editor, user]
---
# the source, e.g. team
spec:
  signInTo: [customers]
```

Written with the CLI, as the cluster's administrator (one commit of both
tenants' line):

```bash
kubectl gentian tenants sign-in customers --from team --positions website-editor,user
kubectl gentian tenants sign-in customers --remove team     # ends it
```

The platform then makes, in the source realm, the client `broker-to-<target>`
(authenticated by a JWT the target realm signs: no secret), and in the target
realm the identity provider `tenant-<source>`, shown on the login page under
the source tenant's name. It trusts the source's verified addresses, and
the operator gives each person who arrives the positions named. A position
may be `user` (who may enter the tenant) or one of `spec.positions`, never
an administrator's. Either side taking its line away ends
the sign-in.

An entry may give its positions only to some of the source's people: those
who hold one of the source's positions named in `holding`, given to them or
through a position that includes it.

```yaml
  signInFrom:
    - tenant: team
      positions: [website-editor]
      holding: [sales]               # positions of team
```

The realm says, with each arrival, who the person is in the source realm,
and the operator asks the rights store what they hold there. At every
arrival it gives the entry's positions to a person who holds one of
`holding`, and takes them from one who does not: who left sales loses
`website-editor` the next time they sign in, whoever gave it to them; a
person who should keep it without being in sales is given a position of the
target's own for it. A position that holds `admin` on the tenant or an app,
itself or through an inclusion, is never given or taken this way. An
arrival that does not say
who the person is in the source gives and takes nothing. An entry without
`holding` gives its positions to everybody from the source and takes
nothing.

The source's desktop may then frame the target's apps, so a tile on it opens
one of them in a window ([routing.md](../concepts/routing.md) §4.3). Only on
the same site: an app on another registrable domain than the source's desktop
gets no session in the frame and has to be opened in a tab
([routing.md](../concepts/routing.md) §4.3, the `SameSite=Lax` cookie).

## Another cluster: partners, declared

When the other realm is on another cluster, neither side is configured by
hand: each tenant declares the other as a **partner** (Tenant
`spec.partners`, written by the director as `partners.yaml` beside the
tenant; [partners.md](../concepts/partners.md), AD-17 in
[decisions/index.md](../decisions/index.md)). A person links their account
here with their account there once, by signing in there through this realm;
from then on apps on either side call each other on that person's behalf.
Only minimal information crosses: a **pairwise subject**, and the person's
role on the tenant (`gentian_roles`) when they are its administrator or the
cluster's.

```bash
# our people towards them (we issue)
kubectl gentian tenants partners acme add shop --issuer https://id.shop.example/auth/realms/customers --issue
# their people here (we accept): link | create | organization
kubectl gentian tenants partners customers add acme --issuer https://id.acme.example/auth/realms/acme --accept create
kubectl gentian tenants partners customers                 # lists them
kubectl gentian tenants partners customers remove acme     # ends it
```

- **Issue** makes, in this realm, the scope `partner-<name>` (the partner's
  issuer as the only audience, the pairwise subject) and the partner client,
  whose client ID is the partner's issuer: it sends a person back only to
  `<issuer>/broker/partner-<as>/endpoint`, checks the partner realm's
  published keys, asks the person's consent once and gives the subject only.
  `as` is the name the partner gives this realm; by default the tenant's
  `metadata.uid`.
- **Accept** makes the identity provider `partner-<name>`, hidden on the
  login page, authenticating by a JWT this realm signs, with the JWT
  authorization grant on. A first arrival is linked to an account that
  exists here (`link`), made (`create`), or a member of an organization of
  the realm with the domain given (`organization`). It gives no position and
  trusts the partner's address only with `organization`, where Keycloak
  requires a verified one for membership; positions are still given by hand
  (step 3). With
  `create`, the arrival is first asked what this realm's profile requires
  and the partner did not give, the e-mail address above all: the person
  tells this realm, the partner's realm never does; an address that has an
  account here already links the arrival to it, after a sign-in to it.
- **Bounds.** The Cluster claim's `spec.partners.allowedIssuers` lists the
  issuers a tenant may name (`*` matches any run of characters except
  `/`); the director refuses and the operator leaves out any other.
  `spec.partners.defaults` are partners every tenant issues towards; the
  installer declares the store's vendor there with the store's extra
  services.
- **Who writes.** The tenant's administrators (`can_administer`). A
  component that commissions partners — a service linking every customer
  cluster that registers with it — writes receiving entries with a key of
  its own: its profile declares `requires.services.partners.commission:
  true`, the tenant names it, by hand, in `spec.partnerCommissioners`
  (`tenant`, `profile`), and it calls `PUT`, `DELETE` and `GET` on
  `/v1/tenants/<tenant>/partners[/<name>]` at `DIRECTOR_URL` with the bearer
  `RIGHTS_CHECK_KEY`, both from its Secret `rights-check-<component>`.
- **Apps.** An app is on a partner's list by declaring it in its profile
  (`requires.services.partners.call: [<name>]`, or `accept: true` to take
  partners' assertions); see [app-customization.md](app-customization.md).
  How an app then calls across, and what a partner learns of a person:
  [partners.md §5 and §8](../concepts/partners.md).

## Removing it

Delete the identity provider in the target, and the client in the source.
The accounts it made stay in the target, with the positions they were
given, until you remove them (*Users*,
filter by the provider).

## What is not covered

The configuration lives in Keycloak's database, not in git: a realm made
again from git does not have it, and has to be given it again from this page.
