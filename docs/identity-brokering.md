# Identity brokering: people of one realm signing in to another

People who already have an account in one realm — another tenant of this
cluster, or a tenant of another cluster — sign in to a second realm with that
account, and get rights there according to where they come from. Keycloak
does all of it; this page is the configuration, done in Keycloak's
administration console. Nothing of it is declared in git.

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
- **Rights come from groups in the target.** A person who arrives through a
  broker gets an account in the target realm, and nothing else — like a
  person who signed up. What they may do is what the groups you map them into
  give (step 3). This holds even in a target that takes no sign-ups: brokering
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
| Display name | what the sign-in page's button says, e.g. "Aluvian team account" |
| Discovery endpoint | `https://id.<source-domain>/auth/realms/<source>/.well-known/openid-configuration` — the page every realm publishes with its addresses; Keycloak fills in the rest from it |
| Client authentication | Client secret sent as post |
| Client ID / Client secret | from step 1 |
| Trust email | On — the source has confirmed it; otherwise the target asks again |
| Sync mode (under *Advanced settings*) | Force — name, e-mail and the mapped groups (step 3) follow the source at every sign-in |
| Hide on login page | Off — the button appears under the sign-in form |

A person whose e-mail address already has an account in the target is asked,
once, to confirm that it is theirs before the two are linked (Keycloak's
*first broker login* flow).

## 3. In the target realm: map arrivals into groups

Create the groups first (*Groups* → *Create group*), with names of your own.
Do not use the platform's `gentian:tenant:<t>:*` groups for a whole source:
`app-admins`, for instance, administers every app of the tenant.

Then, on the identity provider, *Mappers* → *Add mapper*:

- **Everybody from this source gets a group:** *Hardcoded Group*, the
  group. One identity provider per origin, each with its own group, is
  rights by where people come from.
- **Only some of them:** *Advanced Claim to Group*. The source's tokens carry
  its people's groups in the `groups` claim, by name; match the source group
  and pick the target group. With sync mode *Force* the target group is taken
  away at the next sign-in once the person has left the source group.

## 4. Use the group

Whatever is to recognise these people is configured with the target group:
an app that edits for a group, a role in the tenant's Admin Console given to
the group. Try it with a person of the source: the target's sign-in page
shows the button (or, to go straight to the source, add
`&kc_idp_hint=<alias>` to a sign-in URL of the target); after signing in at the source, they are back in the
target, in the group.

## The source on another cluster, commissioned with the tenant

When the source realm is a tenant of *this* cluster and the target is on
another — a hosted tenant whose people sign in to their provider's
customer realm — step 1 need not be done by hand. The tenant is created with
the target named:

```bash
kubectl gentian tenants create acme \
  --brokered-to name=aluvian,redirect=https://id.aluvian.io/auth/realms/customers/broker/acme/endpoint,jwks=https://id.aluvian.io/auth/realms/customers/protocol/openid-connect/certs
```

The platform makes the client `broker-to-aluvian` in the tenant's realm. It
has no secret: the target authenticates with a JWT signed by its own realm
key, which the client checks against the keys the target publishes
(`jwks`). In the target, step 2 is then done with *Client authentication*
set to **JWT signed with private key** instead of a client secret, and
*Client ID* `broker-to-aluvian`. Nothing secret crosses between the clusters.

## Removing it

Delete the identity provider in the target, and the client in the source.
The accounts it made stay in the target — as accounts of nothing, without
the mapped groups after their next sign-in — until you delete them (*Users*,
filter by the provider).

## What is not covered

The configuration lives in Keycloak's database, not in git: a realm made
again from git does not have it, and has to be given it again from this page.
