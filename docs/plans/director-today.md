# The director as it is today

Temporary. This records what the director *is*, not what it should be, so that
the decision about what to change is made against facts. Delete it once the
director's intended shape is written into `work-packages.md` and reached.

Source: `internal/director/{api,authn,authz,entitlement,gitops,membership,session,tiles}`
and `cmd/director/main.go`. Deployed by `charts/gentian-os/templates/director.yaml`
into the control namespace, one replica, no Kubernetes credential at all
(`automountServiceAccountToken: false`, no RBAC).

## 1. What it holds

| | |
| --- | --- |
| A git credential | push access to `gentian-deployments`, in a Secret |
| An OpenFGA credential | `OPENFGA_API_TOKEN`, full read **and write** on the store |
| No Kubernetes credential | none, by construction |
| No Keycloak credential | it only fetches public key sets |

## 2. Every endpoint

Registered in `routes()`, `internal/director/api/api.go`. A guarded route is
registered together with the relation it requires, so a handler cannot forget
to authorise.

### Guarded by an OpenFGA check on the caller

| Method and path | Relation | Object | What it does |
| --- | --- | --- | --- |
| `GET /v1/tenants/{t}/apps` | `can_view` | `tenant:{t}` | reads git, returns the tenant's installed apps |
| `GET /v1/tenants/{t}/apps/{p}/addons` | `can_view` | `tenant:{t}` | reads git, returns one app's addons |
| `GET /v1/tenants/{t}/entitlements` | `can_view` | `tenant:{t}` | reads `entitlements.yaml` from git |
| `GET /v1/tenants/{t}/me` | `can_enter` | `tenant:{t}` | runs ten OpenFGA checks and returns the caller's relations on the tenant |
| `GET /v1/clusters/{c}/tiles` | `can_audit` | `cluster:{c}` | returns the kernel console tiles the caller may open |
| `POST /v1/tenants/{t}/apps/{p}` | `can_install_app` | `tenant:{t}` | commits an app into the tenant manifest and pushes |
| `DELETE /v1/tenants/{t}/apps/{p}` | `can_install_app` | `tenant:{t}` | removes it and pushes |
| `PUT /v1/tenants/{t}/apps/{p}/addons` | `can_install_app` | `tenant:{t}` | rewrites the addons block and pushes |

On install, when entitlement enforcement is on, a second check asks whether
the *tenant* is entitled to the catalogue entry. A write answers 202 with the
commit id when something changed and 200 when the state already held.

### Not guarded by a check on a caller

| Method and path | What authenticates it | What it does |
| --- | --- | --- |
| `GET /healthz` | nothing | liveness and readiness |
| `POST /v1/events/keycloak` | an Ed25519 signature from the Keycloak event listener | writes group membership into OpenFGA |
| `POST /v1/logout/keycloak` | the issuer's signature on the OIDC logout token | writes a session revocation into OpenFGA |
| `POST /v1/tenants/{t}/entitlements` | the App Store's signature | records the fact in git and writes an entitlement tuple into OpenFGA |

The entitlement route is half guarded: a *grant* additionally requires a caller
holding `can_install_app` on the tenant, a *revocation* requires nobody, on the
stated grounds that a tenant administrator must not be able to decline a
revocation.

## 3. Every write

### To git, which is the intended job

All writes go through one path that clones or resets the checkout, edits files,
commits and pushes, retrying from the new remote state when the push is
rejected. The human is the git author, the director is the committer, and every
commit carries a trailer naming the request id, the subject, the relation and
the object that allowed it.

| What | Where in git |
| --- | --- |
| install an app | `spec.apps` of `clusters/<c>/tenants/<t>/tenant.yaml` |
| uninstall an app | the same list entry, removed |
| set addons | the `addons:` block of one app entry |
| record an entitlement | `entitlements.yaml` beside the tenant manifest |

### To OpenFGA, which is the departure

| What it writes | When |
| --- | --- |
| creates the store and writes the authorization model | at start, when no store or model id is pinned |
| cluster role tuples, declaratively, deleting ones the claim no longer names | at start, from `spec.platformRoles` in the Cluster claim |
| `cluster → tenant`, `operated_by`, and the three tenant role tuples | at start, from the tenant manifests in git |
| user-to-group membership tuples, as complete state | on every Keycloak membership event |
| `session:<sid>#revoked` | on every back-channel logout |
| deletion of expired revocation tuples | a background sweep every five minutes |
| `tenant → entitled → catalogue_entry` with an expiry condition | on every store statement |

### To Kubernetes

None.

## 4. Everything it reads

- **Git**: the tenant manifests, the Cluster claim's `platformRoles` and
  `kernelDomain`, and `entitlements.yaml`. Every read re-fetches and resets the
  checkout first.
- **OpenFGA**: `Check` for each guarded route and for tiles and relations,
  `Read` before each write so writes are idempotent, and the changelog for the
  revocation sweep.
- **Keycloak**: public key sets only, cached per realm for ten minutes.

## 5. Loops

1. The revocation sweep, immediately at start and then every five minutes.
2. Two one-shot reconciles at start: cluster roles, then tenants.
3. The OpenFGA store and model bootstrap at start, when not pinned.

Keycloak events are pushed, not polled.

## 6. Who calls it

| Caller | What it calls |
| --- | --- |
| the desktop's backend | `/v1/tenants/{t}/me` and `/v1/clusters/{c}/tiles`, relaying the person's own token |
| the Keycloak event listener | `/v1/events/keycloak`, signed |
| Keycloak, on logout | `/v1/logout/keycloak`, signed |
| the App Store | `/v1/tenants/{t}/entitlements`, signed |
| the kubelet | `/healthz` |

The operator never calls the director. It only tells the desktop where the
director is. The ext-auth shim never calls it either: it reads the same OpenFGA
store directly, through a read-only helper written for that purpose.

## 7. Configuration

Environment only, no flags. Required: the Keycloak issuer base URL, the
expected audience, the OpenFGA URL, and the deployments repository, its local
path and the cluster id. Optional settings pin the OpenFGA store and model,
supply the OpenFGA token, turn entitlement enforcement off, name the git
committer, list the event-listener and store public keys, name the platform
realm and tenant, and set the revocation retention.

Two of these switch endpoints off entirely rather than failing at request time:
with no listener key there is no events endpoint, and with no store key there is
no entitlements endpoint.

## 8. Where this departs from the intended architecture

The intended director reads OpenFGA to decide whether a caller may make an API
call, and writes only to git. Argo CD then syncs git, and the operator turns
that into cluster state. Under that rule, section 3's second table should be
empty, and it is not.

Four things currently make the director a writer of authorization state:

1. **Store and model bootstrap.** Somebody has to create the store and load the
   model. Today the director does it at start.
2. **Structure projected from git**: cluster roles from the claim, and the
   tenant, `operated_by` and tenant role tuples from the tenant manifests. This
   is a projection of git into OpenFGA, which is exactly the operator's kind of
   work.
3. **Membership projected from Keycloak.** The event listener posts signed
   membership state, and the director applies it to OpenFGA. Nothing about this
   needs the director's git credential.
4. **Session revocation.** Back-channel logout writes a tuple the shim reads.

Each of these needs a new home before the director can be reduced to "decide,
then write git". The open question is whether that home is the operator, which
already turns git into cluster state and already holds a cluster credential, or
a separate small projector. Only item 4 is genuinely a runtime signal rather
than a projection, and it could equally be the shim's own state.

Two smaller departures worth deciding at the same time:

- The director serves the **kernel tile catalogue** from a file baked into its
  image. The permission part of that is its job; holding the list of consoles
  is not, since the operator is what routes them.
- `POST /v1/tenants/{t}/entitlements` accepts a **revocation with no caller at
  all**. That is deliberate and documented, but it means the director acts on
  an outside signature without any relation being checked.
