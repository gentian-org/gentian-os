# Security principles

Nine rules that keep authentication, authorization and traffic one framework
as tenants, users, apps and agents multiply. They are normative: a change that
breaks one needs a written reason, not a workaround. The models behind them
(MAC / ReBAC / ABAC layering, the principal chain, the derived ceiling) are in
[design/security.md §2](design/security.md#2-security-principles); what each
one still lacks in code is tracked in [the roadmap](roadmap.md) §1.

The goal they serve: **safe by default, easy to control, easy to audit, without
limiting what can be done.** Every rule below is a way of getting the fourth
without giving up the first three.

## 1. Identity is a verified claim set from a known issuer

Never a header, never a name in a request body. Humans hold Keycloak OIDC
tokens; workloads hold audience-bound service-account tokens (SPIFFE when
needed); agents hold RFC 8693 exchanged tokens carrying `act`. A component
that accepts `X-Whoever: alice` has no identity, it has an opinion.

*Pattern:* the credential manager — forwards the caller's token, never parses
it, lets the issuer's verdict stand.

## 2. Three questions, three answerers, no overlap

**Who** → Keycloak. **May** → OpenFGA. **Is this shape allowed at all** → MAC:
admission policy, NetworkPolicy, CRD validation. A component that answers two
of these is where the framework forks.

## 3. Few, named policy-enforcement points; everything else consumes a verdict

The gateway (browser and API traffic), the director (configuration writes),
the credential manager (secret writes), the MCP gateway (agent tool calls).
A request that reaches a workload without passing a named PEP is a bug.
Apps never decide platform questions; they receive the decision as identity
headers or an exchanged token.

## 4. The authorization vocabulary grows with the API, not with use cases

One OpenFGA type per CRD kind (`cluster`, `tenant`, `app`, `catalogue_entry`,
`document`, …), one relation per verb a PEP exposes. A new kind ships with its
type and a case in `authz/model/*/tests.fga.yaml`, or it does not ship. RBAC
exists only as the Keycloak-group → tuple bridge — never as a second decision
path.

## 5. Authority is derived, never granted sideways

`agent ≤ human ≤ tenant ≤ cluster`, each expressed through the one above
(`acting_for`, `from parent`), with TTL as an OpenFGA condition. Revocation is
one tuple delete; the ceiling is an invariant of the model, not a setting.
This is the single rule that survives heterogeneity — anything new is placed
in the chain, not beside it.

## 6. Traffic is identity-aware and default-deny at every layer

L3/4: NetworkPolicy default-deny, derived from declared requirements. L7:
every route declares its `authMode`; `none` is a word someone wrote and a
reviewer can find, never an absence. East-west: mTLS once workloads have
identity. The same identity object is what all three layers reason about.

## 7. Audit is three logs joined by one request id

Issuer events (Keycloak), decision log (every OpenFGA check with its request
id), change log (git, written only by the director). A fact in none of these
did not happen. Everything else is telemetry.

## 8. Safe defaults by construction: every permissive setting is a diff

Schemas default to the strictest value (auth required, egress none, no
cross-app contracts, no waivers). A relaxation is a declared field — one that
admission can refuse and audit can `grep`. "What is open on this cluster?"
must be answerable by listing fields, never by reading code.

## 9. Control means declaring, not configuring

Who may do what — `PlatformSecurityPolicy`, `AppGrant`, egress intent,
entitlements — is data in git, written through the director, read by the
PEPs. Nobody logs into anything to change a permission.

---

**Applying them to a change.** Ask, in order: which issuer signed the caller
(1)? which single component decides (2, 3)? which FGA type and relation does
this add (4)? where in the chain does the new principal sit (5)? which layer
denies by default and what field opens it (6, 8)? which of the three logs
records it (7)? is the setting a commit (9)? A change with a blank answer is
not finished.
