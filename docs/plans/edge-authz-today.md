# The edge authorization service as it is today

Temporary, like `director-today.md`. What this process is, what it decides, and
what it would take to not have it. Delete it once its shape is settled in
`work-packages.md`.

Source: `cmd/edge-authz/main.go` and `internal/edge/authz/`, about 650 lines of
Go excluding tests. Built into the operator image, deployed by
`charts/gentian-os/templates/edge-authz.yaml` into the edge namespace, two
replicas.

## 1. On the name

"Shim" is ours, not the industry's, and it undersells the thing. The standard
vocabulary is:

| Piece | Standard name |
| --- | --- |
| Envoy, which admits or refuses the request | the **policy enforcement point** |
| this service, which Envoy asks | the **external authorization service**, or `ext_authz` service |
| OpenFGA, which holds the relations and answers | the **policy decision point** |

Envoy's own name for the protocol is `ext_authz`, and an external service
speaking it is what OPA, Open Policy Agent, is most often deployed as. The
binary is already called `edge-authz`. Calling it **the edge authorization
service** in prose, and `edge-authz` in code, would match both the binary and
the industry. Nothing in the code needs to change for that.

So: Envoy is the enforcement point for traffic. This service is what it asks,
and OpenFGA is what answers.

## 2. What it decides

One question per request, before the request reaches anything:

> may the person this token names do `<relation>` on `<object>`?

The relation and object are not in the request. They come from a **route
table** the operator writes, keyed by the request's host. A host the table does
not name is refused.

For each request the service:

1. Looks the host up in the table.
2. Verifies the bearer token against the realm's public keys, checking issuer
   and audience.
3. Refuses if the token's session id has been recorded as revoked.
4. Asks OpenFGA whether the subject holds the route's relation on its object.
5. On allow, strips the `Authorization` header unless the route forwards the
   token, and adds identity headers: subject, realm, session, email, name.
6. On deny, answers 403, and the request never reaches the backend.

Failure is closed. An unreachable OpenFGA is a denial, and the policy is
configured `failOpen: false` so an unreachable *service* is a denial too.

One deliberate exception, which is the awkward part: **Envoy runs ext_authz
before its OIDC filter**, in every version we have checked. On a route whose
mode is `oidc`, a request that arrives with no session therefore reaches this
service first, with nothing to verify. It is allowed through with no identity
headers and no bearer, so that the OIDC filter behind it can start the login.
Only a `bearer` route answers 401 to a session-less request.

## 3. Caching and revocation

A decision is cached per subject, session and host, for five minutes by
default. The cache is not the authority on how long access lasts: the service
polls the OpenFGA changelog every two seconds, and any change to a subject
evicts that subject's entries. A back-channel logout is therefore visible to
every replica within about a poll, without the replicas talking to each other
and without losing anything across a restart.

Cached allows survive a store outage until they expire, which is the one place
the design prefers availability. New decisions during an outage are denials.

## 4. The route table

A ConfigMap, `edge-authz-routes` in the edge namespace, written by the
operator's gateway reconciler and mounted into the service. One entry per host:

```yaml
- host: console.gentian-os.org
  authMode: oidc
  relation: can_enter
  object: tenant:platform
  accessTokenCookie: gentian-kernel-access
  forwardToken: true
```

Kernel entries come from the operator's own kernel route list. Component
entries come from every HTTPRoute labelled for it, carrying the relation and
object as annotations. Hosts are deduplicated, and a duplicate host makes the
whole table refuse rather than pick one.

## 5. Configuration

Environment only. The Keycloak issuer base URL and the expected audience, the
OpenFGA URL and token, the route table path, the listen address for gRPC and
one for health, the cache lifetime and the changelog poll interval. The store
and model ids are discovered read-only if not pinned: this process never
creates a store and never writes a tuple.

## 6. Could the gateway do this instead?

Not today, and the answer is worth writing down because it will be asked again.

What Envoy Gateway can do declaratively, in a `SecurityPolicy`:

- allow or deny on JWT claims, on the request's headers, and on client IP;
- require a JWT with a given issuer and audience;
- run the OIDC code flow and hold the session.

What it cannot do is ask a relationship question. Our decision is not "does the
token carry claim X" but "does `user:<sub>` hold `can_enter` on
`tenant:platform`", which only the store can answer, and which deliberately is
*not* in the token: the zone's client emits no groups scope precisely so that
authority is not carried in a bearer token a browser holds.

Three in-gateway alternatives and why none is better:

1. **Put the groups back in the token** and use claim-based rules. This undoes
   the decision that membership is projected into a store and revocable there.
   A token minted before a change would still carry the old answer.
2. **A Wasm or Lua extension** calling OpenFGA from inside Envoy. Envoy Gateway
   supports this. It means writing token verification, caching, changelog
   eviction and revocation in Wasm or Lua rather than in Go, with worse
   testing and no meaningful gain.
3. **Point ext_authz straight at OpenFGA.** OpenFGA does not speak the Envoy
   `ext_authz` protocol. It does have an experimental AuthZEN endpoint, and
   AuthZEN is the emerging standard for exactly this hop, but Envoy Gateway
   does not speak AuthZEN either. If both ends adopt it, this service could
   shrink to almost nothing, and that is the thing worth watching.

So the current shape, a small external authorization service between Envoy and
the store, is the ordinary industry pattern and not a workaround. What is worth
revisiting is only its name.

## 7. Where it stands relative to the director

The service and the director share the `authz` package and the same store, but
the service never calls the director and holds no write capability: it resolves
the store and model through a read-only helper written for it. If the director
stops writing authorization state, nothing here changes, except that the
revocation tuple it reads would be written by whatever takes that over.
