# Director / operator split

Issue #185. Splits the gentian-os process into two binaries with one invariant
between them:

```
human intent ──► director ──writes git only──► Argo CD ──► operator ──writes cluster only──► Crossplane
```

That is the **write** path, and it is the only one this document covered at
first. Three more roles stand around it, each with one job and none of the
director's authority (§3.10):

```
arriving   person ──► concierge ──► the right zone's sign-in          (before a session; decides nothing)
entering   request ──► edge session ──► bouncer ──► backend           (every request; enforces, holds no rule)
looking    person ──► usher ──► "what is here, what may I open"       (reads only; changes nothing)
changing   person ──► director ──► git ──► Argo CD ──► operator ──► resources
                                                          └──► projections ──► bouncer, usher, OpenFGA
```

The **director** is the only process that can push to `gentian-deployments`.
Every push it makes was authenticated by Keycloak and authorised by OpenFGA,
so the repository becomes an audit log that can be trusted as the single
source of truth for cluster configuration. The **operator** never holds a git
credential: it converges the cluster on what Argo CD hands it, and its trust
in that input comes from the chain in §3.4, not from a shared secret.

The director is the configuration-write PEP of
[security-principles.md](../security-principles.md) §3 and the change log of
§7. It closes G1, G2 (its half), G7 (writes) and G11 (its half) in
[security-gap-closing.md](security-gap-closing.md): cutover steps A–B are
that plan's wave 1, step C spans waves 1 and 3. It runs in `kernel-control`
per [architectural-decisions.md](architectural-decisions.md) AD-7; this document uses
today's namespace names when it describes today's code and the taxonomy's
when it describes the target.

## 1. Decision

Two binaries, one repository, shared `internal/` packages. Not one binary with
internal discipline, for three reasons that are about the process boundary
rather than code organisation:

1. **The authorisation check becomes a property of the topology.** If exactly
   one process holds the push credential and that process is the one doing the
   ReBAC check, "every change to desired state was authorised" is enforced by
   deployment shape. No handler added later can bypass it, because no other
   process can write.
2. **Blast radius.** Today one pod ([cmd/main.go](../../cmd/main.go)) runs the
   controllers, the admission webhook, the custodian and the
   lifecycle API, under one ServiceAccount whose ClusterRole grants
   `pods/exec` and cluster-wide `secrets` CRUD
   ([clusterrole.yaml](../../charts/gentian-os/templates/clusterrole.yaml)),
   while serving an **unauthenticated** HTTP API on `:8082`
   ([runnable.go](../../internal/applifecycle/runnable.go)). An RCE in a
   request handler currently reaches Postgres, OpenBao and every tenant's
   desired state. After the split the request-facing process holds a git
   credential and read-only cluster access.
3. **Admission availability.** The Tenant and AppProfile webhooks run in the
   same process with `failurePolicy: Fail`. A request-driven OOM currently
   takes down admission cluster-wide.

If a single binary were kept anyway, the separation would have to be: two
`main` entry points selected by flag, the git package importable only from the
director entry point (enforced with a `go list -deps` check in CI), separate
ServiceAccounts selected by the same flag, and the push credential mounted only
in the director Deployment. That reproduces most of the cost of two binaries
and none of the process-boundary benefit, so it is not recommended.

## 2. What writes today

The split has to account for every writer, not only the operator. Inventory:

### 2.1 Writers to `gentian-deployments`

| Writer | Path | Identity | Authorised by |
| --- | --- | --- | --- |
| Operator lifecycle API | [internal/applifecycle](../../internal/applifecycle/) — `Install`, `Uninstall`, `SetAddons`, `SetResourcePlan` commit and push | operator's push token, `Actor` from an `X-Gentian-Actor` header the caller sets | nobody — the header is trusted |
| `kubectl gentian` | [scripts/kubectl-gentian](../../scripts/kubectl-gentian) `git_commit_push`, 11 call sites: `tenants deploy/undeploy`, `apps install/uninstall`, deletionPolicy flips around purge | the human's own git credential on their workstation | git host permissions only |
| `kubectl gentian` fallback | `apply_tenant_manifest_from_git` → `kubectl apply -f` when no Argo app exists | the human's kubeconfig | Kubernetes RBAC only — bypasses git entirely |
| `install.sh --prepare-deployment` / `--prepare-tenant` | writes local files and **stops**; the human commits ([deployment.md §3](../deployment.md)) | the human | review before commit |
| App Store backend (`gentian-apps/apps/app-store`) | `services/gitops.py` — commit and push, behind `INSTALL_MODE=gitops`; dormant (no route calls it) but shipped with the repo credential config. Also a dormant direct `patch` of `Tenant.spec.apps` ([ui-restructure.md](ui-restructure.md) §3) | store pod's push token, `actor` from the request | nobody |
| Humans | direct commits | their git identity | git host permissions |

Not a writer: argocd-image-updater uses `write-back-method: argocd`
([gentian-os.yaml](../../kernel/bootstrap/chart/templates/gentian-os.yaml))
and patches Applications in-cluster.

### 2.2 Human-facing writes that bypass git

The admin console (gentian-ui BFF) writes these directly to the API server
with its own ServiceAccount:

| Object | Console module | Nature |
| --- | --- | --- |
| `BackupPolicy` (cluster), `TenantBackupPolicy` | `k8s_backup_policy.py` | desired state — belongs in git |
| `PlatformSecurityPolicy` | `k8s_authorization.py` | desired state — belongs in git |
| `AppGrant` | `k8s_authorization.py` | desired state — belongs in git |
| `TenantExportSchedule` | `k8s_backup_schedules.py` | desired state — belongs in git |
| `TenantExport`, `TenantRestore` + backup-key `Secret`s | `k8s_backup.py` | one-shot requests carrying secrets — must **not** go to git |
| Tenant annotation `app-privilege-requested` | `k8s_catalogue.py` | a reconcile kick — should not exist as a write |

And the lifecycle API itself performs cluster writes from inside the request
path that are not git edits: `purge.go` (26 KB — exec into the Postgres and
OpenBao pods, PVC/Pod/Secret/Job deletion), `reconcile.go` (Argo refresh
annotation), `provisionAppGroupUsers` (Keycloak group membership), and a
synchronous wait for Ready of up to 15 minutes.

### 2.3 Credential coupling

Argo CD's read credential and the operator's push credential are **the same
token**: both are composed from `spec.credential.vaultPath`
(`gentian-os/kernel/repositories/deployments`) in
[repository-default.yaml](../../crossplane/compositions/repository-default.yaml)
— once as the `argocd.argoproj.io/secret-type: repository` Secret, once as
`.git-credentials` when `writable: true`. Anything that can read the Argo
Secret can push.

## 3. Target architecture

### 3.1 Director

A plain Deployment (N replicas, no leader election, no controller-runtime
manager). Built from `cmd/director`. It has:

- An HTTPS API (§3.5) that accepts **only** bearer tokens issued by the
  Keycloak `kernel` realm or a tenant realm — the same JWKS verification the
  BFF does in `gentian-ui/backend/app/core/auth.py`. No `X-Gentian-Actor`
  header, ever; identity is the token's verdict (principle 1). Its callers
  are each tenant's desktop BFF (`tenant-<t>`, AD-10 — the platform's own in
  `tenant-platform`), the CLI, and the **external App Store** (AD-3) —
  which never holds authority of its own: it calls with the tenant admin's
  token, or with an RFC 8693 exchanged token carrying `act` (principle 5),
  and the FGA check is on the human either way. The route is on the kernel
  gateway, bearer only, behind the gateway `SecurityPolicy` of G3; the
  director verifies again rather than trusting the hop.
- An OpenFGA client (already in
  [openfga_client.go](../../internal/authz/openfga_client.go)). The director
  is the **store's only writer** (AD-12): it creates the store and the model
  on first start, writes every structure tuple in the same operation as the
  commit it reflects, and rebuilds the tuples from git on start — the store
  is a projection of the change log. The operator reads. It authenticates
  to OpenFGA with its projected ServiceAccount token (`authn.method: oidc`),
  so the push credential stays its only stored secret. The vocabulary is
  [authorization-model.md](authorization-model.md).
- A working checkout of `gentian-deployments` and the **only** push
  credential. Commits are authored as the human (`Name <email>` from the
  token), committed by the director, and **signed** with a key only the
  director holds. The commit message carries a trailer with the OpenFGA
  decision and the request id
  (`Gentian-Authz: req=<id> user:<sub> can_install_app tenant:<t> allowed`),
  so one id joins the Keycloak event, the decision log and the commit
  (principle 7, G10).
- A **read-only** Kubernetes client for `Tenant`, `App`, `AppProfile` status
  and the `openfga-runtime` Secret — enough to answer status queries and
  validate requests. Plus exactly one narrow write grant, `appprofiles`
  create/update, for materialise-on-reference (§3.6). No `pods/exec`, no
  `secrets` beyond the one named Secret, no Argo objects.
- A rate limiter and a decision log: every `Check` it makes is written as
  `(request id, subject, relation, object, decision)`, plus the commit SHA
  when one results. The request id arrives as a header from the gateway or
  is minted here, and is returned in every response.

It does **not**: wait for readiness, purge, exec, touch Keycloak groups, or
annotate Argo. The commit is the completed action; every write returns `202`
with a status URL, and status is served from the read-only client.

### 3.2 Operator

Unchanged in role, smaller in surface. After decommissioning (§6 D) it has:
no git clone init container, no `.git-credentials` mount, no
`GENTIAN_DEPLOYMENTS_*` env, and an HTTP listener that only the director may
call and that carries a short, closed list of commands (§3.11). It keeps `pods/exec` because purge
needs it — but purge becomes a reconcile of desired state (app absent from
`Tenant.spec.apps` and the App claim gone → teardown converges), which also
fixes the current failure mode where a request that dies mid-purge leaves
half-deleted state with nothing to resume it. After AD-9 its exec targets are
the `system-<engine>` namespaces, never `kernel-data`.

### 3.3 Argo CD

Stays exactly where it is in the chain: it reads git and applies. Two
changes:

- It gets its **own, read-only** repository token, distinct from the
  director's push token (§2.3 is dissolved: two vault paths, or one claim
  field `credential.push` alongside `credential`).
- The `gentian` AppProject gets a `spec.sourceIntegrity` policy for the
  deployments repository (GnuPG, mode `head`, keys: the director's and the
  break-glass key), so Argo refuses to sync a commit neither signed. This is
  what lets the operator trust git without trusting the git host. It lands
  with G11 in wave 3, after the director is the sole pusher; until then the
  Argo link of §3.4 is branch protection alone. `signatureKeys` is
  deprecated upstream and is not used. Verified limits: git sources only
  (the deployments repository is git); an ApplicationSet with a templated
  `project` is not verified (ours are literal); `argocd app sync --local`
  stops working; GnuPG only. `head` verifies the target commit, not its
  ancestry — `strict` would fail on the human bootstrap commits.

### 3.4 Trust chain

```
Keycloak token ──► director verifies JWKS/iss/aud
             ──► OpenFGA Check(user, relation, object)
             ──► signed commit, authored as the human, trailer with decision
             ──► git host branch protection: only the director's identity may push main
             ──► Argo AppProject sourceIntegrity (GnuPG, head): only director- or break-glass-signed commits sync
                 (the recovery kit carries the director's key material by decision — work-packages WP-10 —
                  so a kit holder can sign as the director; custody and audit carry that link, not the signature)
             ──► operator reconciles what Argo applied
```

Each link is independently enforced. Compromise of the git host cannot inject
config (signature check, once wave 3 lands); compromise of the director's push
token without its signing key cannot either; a stolen token from realm A cannot
act on tenant B (FGA); and the operator SA holds nothing that writes git. This
is principle 2 in one line: Keycloak answers *who*, OpenFGA answers *may*, and
admission plus the Tenant webhook answer *is this shape allowed* — the
director consumes all three verdicts and issues none.

### 3.5 API

The existing contract is kept so callers switch by URL, not by rewrite.

**Every write has a read — of what was declared.** The director is the only
writer, so it is the only place that knows what git says, and a UI that
offers a change has to show the declared state it is changing. It is *not*
the place that knows what exists: that is the cluster, and "what is here and
what may I open" is the usher's to answer (§3.10), from what the operator
projected. The reads below that describe declared state stay here; the ones
every signed-in person needs merely to use the platform — tiles, "what may I
do", status — move to the usher, so that loading a desktop no longer reaches
the process that holds the push credential. A UI that cannot read either has
to guess. The App Store must show which apps a tenant already has, at which
version, with which addons and integrations enabled; the console must show
which users, groups, policies and surfaces exist before it can offer a
sensible change. Both render from these reads, with the signed-in human's
token, filtered by that account's relations. No endpoint below exists as a
write without its matching read, and a new write verb ships with one.

Reads are authorised by `can_view` on a tenant and by `can_configure` or
`can_audit` on a cluster, never by the write relation: a tenant administrator
must be able to see the exposure inventory they cannot change.

```
Writes
POST   /v1/tenants/{t}                          deploy a definition (today: kubectl gentian tenants deploy)
DELETE /v1/tenants/{t}                          undeploy
POST   /v1/tenants/{t}/apps/{p}                 → 202 + Location
DELETE /v1/tenants/{t}/apps/{p}                 → 202
PUT    /v1/tenants/{t}/apps/{p}/addons
PUT    /v1/tenants/{t}/apps/{p}/config          per-install overrides
PUT    /v1/tenants/{t}/resources
PUT    /v1/tenants/{t}/policies/{kind}/{name}   backup, grants, export schedules (§2.2)
PUT    /v1/tenants/{t}/integrations/{contract}  AppGrant: integration consent; `can_grant`
(people, groups and the realm's settings are not here: they are the registrar's, §3.10)
PUT    /v1/tenants/{t}/exposure/{inst}/{name}   enable a perimeter surface; `can_expose`
DELETE /v1/tenants/{t}/exposure/{inst}/{name}   disable it; `can_expose`
PUT    /v1/tenants/{t}/privileges/{inst}/{name} grant a tenant-scope privilege; `can_approve_privilege`
DELETE /v1/tenants/{t}/privileges/{inst}/{name} revoke it
PUT    /v1/clusters/{c}/privileges/{t}/{inst}/{name}  grant a cluster-scope privilege; `can_approve`
POST   /v1/tenants/{t}/entitlements             a signed grant or revocation from the App Store (§3.8)
POST   /v1/tenants/{t}/requests/{kind}          export / restore — creates the request CR, secrets via ESO reference only
POST   /v1/clusters/{c}/shared-apps/{p}         install a `tenancy: shared` profile into `shared-<app>` (AD-4); `can_install_shared`
PUT    /v1/clusters/{c}/security/{kind}/{name}  PlatformSecurityPolicy, PolicyException, overlays (§7.1)
PUT    /v1/clusters/{c}/exposure                the cluster exposure ceiling; `can_approve`
PUT    /v1/clusters/{c}/network | /v1/tenants/{t}/network   egress intent (§7.2)
PUT    /v1/files/{path}                         break-glass raw edit; FGA relation `can_edit_raw`, always audited

Reads
GET    /v1/tenants                              list, filtered by what the caller may see
GET    /v1/tenants/{t}
GET    /v1/tenants/{t}/apps                     installed, with version, digest and health
GET    /v1/tenants/{t}/apps/{p}                 config, addons, granted requirements, integrations, surfaces
GET    /v1/tenants/{t}/apps/{p}/addons          what is enabled now — what the store renders as checked
GET    /v1/tenants/{t}/resources | /plans | /usage | /report
GET    /v1/tenants/{t}/policies/{kind}[/{name}]
GET    /v1/tenants/{t}/integrations             grants in force, and what each profile could consume
GET    /v1/tenants/{t}/users | /groups          for the console's user administration
GET    /v1/tenants/{t}/exposure                 surfaces declared, enabled, owner, expiry, review
GET    /v1/tenants/{t}/exposure/log | /objects  condensed proxy log; public objects via the contract
GET    /v1/tenants/{t}/network
GET    /v1/tenants/{t}/privileges               requested, granted, pending, with approver and expiry
GET    /v1/clusters/{c}/privileges               the security officer's queue across tenants
GET    /v1/tenants/{t}/entitlements             what this tenant may install, and until when
GET    /v1/tenants/{t}/requests/{kind}[/{id}]
GET    /v1/clusters/{c}/shared-apps
GET    /v1/clusters/{c}/security/{kind}[/{name}]
GET    /v1/clusters/{c}/exposure                the ceiling, for rendering what an approver may choose
GET    /v1/clusters/{c}/network
GET    /v1/operations/{id}                      status of a 202
```

The App Store is an ordinary caller of the reads. It holds no cluster state of
its own and no identity toward the cluster: it renders what the director
returns for the signed-in tenant administrator, which is what lets it show an
app as installed, an addon as enabled, or a surface as published, instead of
offering a choice the cluster has already made. That is a read direction AD-3
did not originally have; it does not weaken "may trigger, may not supply",
because reading state is not supplying an artefact.

`/v1/files` exists so "sole writer" survives real operations: a platform
operator who needs to hand-edit a claim does it through the director, and the
edit is signed and attributed like any other.

### 3.6 Authorisation model

The model is [authz/model/v1/model.fga](../../authz/model/v1/model.fga); the rules and the
per-PEP relation table are in
[authorization-model.md](authorization-model.md). The director checks
`can_*` relations only — on `tenant:<t>` for tenant verbs, on `cluster:<c>`
for cluster verbs, on `catalogue_entry:<cat>/<app>` with user `tenant:<t>`
for entitlement — and writes the structure tuples listed in
authorization-model.md §3. It replaces
[authz/model/v0/model.fga](../../authz/model/v0/model.fga), whose
`admin: [user] or member` makes every member an admin; nothing of v0 is
inherited. Memberships reach the store from Keycloak's event
stream through the director (AD-12), so a freshly granted role is effective
on the next check within milliseconds and the only "not yet synced" state
is the reconcile's, which it reports. The 5-minute bridge
([authz_bridge_reconciler.go](../../internal/controller/authz_bridge_reconciler.go))
is retired with the split; the director receives Keycloak's events, runs
the reconcile with a read-only client, and creates the store and model
itself.

Each relation gets a case in `tests.fga.yaml` before the director calls it.

Materialise-on-reference: the director fetches the profile bundle at the
requested digest from the catalogue source named by the store row — a public
repository with the cluster's read credential, or a private one with the
single-use fetch token that arrived with the entitlement grant, discarded
after the fetch; never from the App Store's own database, which is
reference data outside the cluster (AD-3) — hands any pull credential to the
custodian to be written as the caller (no secret enters git;
[ui-restructure.md](ui-restructure.md) §3), applies the `AppProfile` CR
(label `gentianos.io/profile-name`, digest annotation), **then** commits.
Apply-then-commit is the only ordering that fails safe: a failed commit
leaves an inert, unreferenced CR; commit-then-apply leaves git asserting a
state the Tenant webhook rejects on every sync. The `catalogue-<repo>`
ApplicationSet that syncs every profile today is retired with this: the
cluster holds only the profiles a tenant installed (namespace-cleanup §3.4).

### 3.7 Concurrency

Today `lockApp` is an in-process mutex justified by the operator running a
single replica, while the lifecycle `Runnable` opts *out* of leader election
"so any replica can serve" — two comments that contradict each other, kept
true only by `replicaCount: 1`. The director uses git itself: push, and treat
a non-fast-forward rejection as the optimistic-concurrency signal — reset to
the remote, apply the edit again to what is there now, retry, bounded and with
jitter. Every write is therefore a function of the manifest's current text,
never a commit to be rebased: two installs into one tenant insert at the same
line, which git cannot merge, and re-applying also keeps the answer honest
when the other writer made the same change. Correct across N replicas and
across the side-by-side period of §6 B when two processes push to one repo.

### 3.8 Entitlements: granted and revoked by the same path

An entitlement arrives as a signed fact from the App Store and becomes a
`catalogue_entry#entitled@tenant:<t>` tuple with `expires_at` as its condition.
Expiry alone is not enough: a refund, a downgrade or an abuse takedown has to
take effect when it happens, not when the grant would have lapsed.

So **revocation travels the same path as the grant**, and overrides it:

```
POST /v1/tenants/{t}/entitlements
  { coordinate, granted: false, reason, issued_at, key_id, signature }
```

The director verifies the signature against the store signing keys **pinned
on the Cluster claim** (`store.signingKeys`, changed only under
`can_configure`) — never a key fetched at verification time, or whoever
controls that fetch issues entitlements — commits
the revocation as a fact in `gentian-deployments`, and deletes the tuple in the
same operation as the commit. Because the record in git is what the store is
rebuilt from on start (AD-12), **a later commit wins over an earlier
`expires_at`**: a revocation committed today ends the entitlement today, and a
rebuild replays the revocation rather than the grant it supersedes.

What revocation does *not* do is stop a running app. The tuple governs
install and upgrade; the pull credential in OpenBao and its `ExternalSecret`
are removed with it, so the next pod start cannot pull, while running pods are
unaffected. That is deliberate — a billing event should not take a tenant's
data offline — and it is the behaviour to state rather than discover.

`revoked_at` in the store's schema is therefore a record of something
delivered, never something the cluster polls for: the cluster holds no
identity toward the store and never calls it.

### 3.9 How a caller gets a token

Every write above is authenticated by a Keycloak token. A browser gets one
from the edge session (AD-13). The two callers that are not browsers need
their own path, and neither has one today: the CLI sends
`X-Gentian-Actor` over a port-forward, which step 0 removes.

**The CLI: the device authorization grant (RFC 8628).** A public client
`gentian-cli` in the kernel realm, no secret, device grant enabled, audience
including the director.

```
kubectl gentian login --cluster <c>
  1. POST <issuer>/protocol/openid-connect/auth/device   client_id=gentian-cli
     → device_code, user_code, verification_uri_complete, interval
  2. print the URL and the short code; open a browser if there is one
  3. the human authenticates in the browser — password, MFA, whatever the
     realm requires; the CLI never sees a credential
  4. poll the token endpoint with device_code at `interval`, honouring
     authorization_pending and slow_down, until tokens or expiry
  5. store in ~/.config/gentian/credentials, mode 0600, keyed by cluster:
     refresh token, access token, expiry
```

The device grant rather than a loopback redirect because the common case is
an operator on a remote shell, where the browser is on a different machine
and a callback to `localhost` cannot arrive. The same flow works when there
is a local browser.

Afterwards each command sends `Authorization: Bearer`, refreshing when the
access token has expired and re-prompting only when the refresh fails.
`kubectl gentian logout` calls the revocation endpoint and deletes the file.

Two properties worth stating. The CLI holds a refresh token, so it is a
non-browser credential held over time — which is where sender-constrained
tokens earn their place (WP-1), and not on the browser path. And because the
token is the human's, a CLI write is attributed to the human in the commit
and in the decision log, exactly like a console write; the CLI is not an
identity.

**The App Store** authenticates the same way as any other client of the read
API and carries the signed-in administrator's token on an install, so it too
needs no identity of its own (§3.8, AD-3). What that does and does not buy
should be plain: a sender-constrained token bound to the *store's* key makes
a stolen token useless, but it does not stop the store itself from issuing a
request the human never made — the store holds the key. That is accepted:
the store is operated by Gentian Technologies, every install is still an
OpenFGA check on the human and a signed commit naming them, and an
entitlement it could not also forge is required. A store that must be
unable to forge needs the browser to sign the intent with a key the store
never sees; that is recorded as the stronger option, not built.

### 3.10 Concierge, bouncer, usher

Three roles the split did not name. Two of them existed under other names;
the third is new, and is what the director's read routes become.

| Role | One sentence | Runs in | Holds | Was |
| --- | --- | --- | --- | --- |
| **Director** | Turns an authorised intent into a signed commit. | `kernel-control` | the push credential, the signing key | — |
| **Concierge** | Sends a person who has no session yet to the sign-in of the zone their address belongs to. | the platform tenant's DMZ (`tenant-platform-dmz`), as a perimeter surface of a platform-tenant component | nothing | `sign-in`, beside Keycloak |
| **Bouncer** | Asks, for every request on the authenticated Gateway, whether this session may enter this host, and refuses when the answer is not yes. | `kernel-edge` | nothing; reads the route table and asks OpenFGA | `edge-authz`, "the shim" |
| **Usher** | Tells a signed-in person what is here and what they may open. | `kernel-edge` | nothing; reads the operator's projections and asks OpenFGA | the director's `tiles`, `me` and status reads |
| **Custodian** | Takes a credential from the person entitled to set it and puts it in the vault, without anyone being able to take one back out. | `kernel-control`, a Deployment and ServiceAccount of its own | an identity at the vault that can write a credential and cannot read one | `credential manager`, a part of the operator's process |
| **Registrar** | Keeps the list of people: invites them, puts them in groups, removes them — and cannot touch who administers the platform. | `kernel-control`, a Deployment and ServiceAccount of its own | a Keycloak client credential for each realm, handed over by the operator; the login of its own database | the director's people, group and realm-settings routes, and the Keycloak credential the director held for them |

The names are a cast on purpose: the concierge points you to the right
door, the bouncer checks you at it, the usher shows you round inside, the
custodian keeps the keys, the registrar keeps the list of who belongs to the
house, and the director decides what the house runs. Only the last three
change anything, and they change different things: the director changes what
git declares, the custodian what the vault holds, the registrar who is in a
realm and in which groups. A secret must never be a commit and people do not
belong in an append-only history, which is why each has a keeper of its own.
One standing credential per process is the rule the cast follows; the table
at the end of this section says where it holds and where it does not.

**What each must not do** — the contract, stated as the failure it rules out.

- *The concierge is not an authority.* It derives a zone from the part of an
  address after the `@` and redirects; it asks the cluster nothing about
  accounts, so it cannot say whether one exists. It may only send a browser to
  a console of this cluster — `console.<tenant>.<kernel>`, or a custom domain
  the operator published — never to an address taken from its input. It is
  the one page on the platform with no session in front of it, which is why
  it is a perimeter surface in a DMZ and not a path on the identity
  provider's route.
- *The bouncer decides nothing.* It verifies the session the edge
  established and asks the store one question per (session, host), named by
  the table the operator wrote. A rule that lived in the bouncer would be a
  second policy, edited by deploying a binary.
- *The usher decides nothing either,* and changes nothing. Every answer is a
  projection the operator wrote, filtered by a question put to the store. It
  holds no git credential, no signing key and no Kubernetes write.
- *The custodian hands nothing back and decides nothing.* It is the
  director's pattern with the vault where the director has git: it verifies
  the caller's token, asks the store, and then acts under its own name,
  recording whose act it was. Who may see that a credential is required and
  who may set it are the store's answers -- `can_read_credential` and
  `can_write_credential`, on the cluster for kernel and system credentials
  and on the tenant for a tenant's. No route returns a credential's value, to
  anybody, and its vault role has no capability to read one. The vault is
  never shown a caller's token and is not asked who they are: an earlier
  shape exchanged that token at the vault and took the vault's verdict, which
  made a group written into a token a second source of rights.
- *The registrar does not decide who may ask, and does not change who
  administers the platform.* It is the director's pattern with Keycloak where
  the director has git: it verifies the caller's token, asks the store the
  relation the route names -- `can_manage_users` on the tenant for people
  and groups, `can_set_policy` for the realm's password policy,
  `can_configure` on the cluster for handing a tenant's administrator account
  to its holder, `can_audit` on the cluster for the count of accounts -- and
  then acts under its own client in that tenant's realm. Whose act it was is
  recorded in a database of its own, and the request id goes with the call
  so that Keycloak's admin event can be joined to it. It holds one credential
  per realm and can name no realm it was not handed one for. One thing it
  refuses whoever asks and whatever the store answers: a change to the group
  the Cluster claim names as the platform's administrators
  (`spec.platformRoles.admin`) -- adding to it, removing from it, renaming,
  deleting or re-creating it -- and any change to a person who is in it,
  because changing an administrator's address and mailing a password link
  replaces them as surely as editing the group does. That includes the
  activation link for the cluster administrator's own account, which comes
  from the install host (`./install.sh --activate-admin`). The rule is applied
  at the one place every write leaves for Keycloak
  (`internal/registrar/identity/guard.go`), not route by route. It holds no
  git credential and no vault role, and in the cluster it may read the
  tenants and the Cluster claim and nothing else.
- *The director does not answer for the cluster, and not for people.* It
  keeps the reads of declared state, for the people who may change it. It
  holds no Keycloak credential and serves no route about a person.

**Why the usher is not part of the bouncer.** Both are read-only and ask the
same store, and they share their code: one image, the same verifier, the same
store client. They are two processes because the bouncer is in the path of
every request and must stay small, rarely released and reachable only by the
Gateway, while the usher takes requests from browsers' backends and will grow
a route per screen. The usher builds the tiles from the bouncer's own table,
so a tile is shown only for something the bouncer would admit.

**Where this is weaker than it should be** — recorded so that the roles are
not mistaken for the controls.

1. *The bouncer is opt-in per route.* It sees a request only where a
   `SecurityPolicy` attaches it. A route on the authenticated Gateway without
   one is served with nothing in front of it, and two were: the LiteLLM
   console, and any component entry whose `authMode` was not `oidc`. Both are
   closed, and a test now walks the kernel routes, but the shape is still
   "protected where somebody remembered". The Gateway should refuse by
   default — the bouncer attached to the Gateway itself, answering "no" for
   any host it has no line for — so that forgetting a policy closes a route
   instead of opening it (§8).
2. *The bouncer's table is assembled from annotations on routes.* The
   question a route is asked — which relation, on which object — is read
   from the route's own annotations. That is policy carried by the thing it
   governs: a route written by anything other than the operator could name a
   weaker question for itself, and a change to the question does not reach a
   route that already exists unless the writer remembers annotations (it did
   not, once). The table should be derived from the `Component` and its
   profile, which are what the question follows from (§8).
3. *The bouncer does not guard the perimeter.* Surfaces published from a DMZ
   have no session; what stands in front of them is the publishing proxy
   (networking.md, L1′). "The bouncer checks everything" is true of the
   authenticated Gateway only.
4. *A right withdrawn takes up to the bouncer's cache lifetime to bite* —
   five minutes — and a session ended at Keycloak takes up to the access
   token's lifetime, also five. The design names a changelog poll that would
   make the first a second or two; it is not built.
5. *The usher will be asked to become a backend for everything.* Its routes
   are lists, and lists are where one tenant's objects leak to another. Each
   route names its object and its relation and is registered through one
   guard, as the director's are; a route that filters in its handler instead
   is the mistake to refuse in review.
6. *The concierge puts an address in a URL.* The sign-in form is filled from
   `login_hint` on the console's address, carried to the identity provider in
   the edge's `state`. Nothing is stored, but the address is in browser
   history and in the edge's access logs. It also depends on the format of
   `state`, which is the edge's own; when that changes the form is simply
   not filled.
7. *Five processes are not five controls.* The concierge is a convenience
   and could be removed without weakening anything; the usher is a
   separation of privilege, not an enforcement point. The controls are the
   edge session, the bouncer, the publishing proxies, and the director's
   check before a commit.
8. *The custodian can write any credential.* That is what it is for, and
   it is the director's position exactly: one process whose own authority
   covers everything it is ever asked to do for somebody, with the store as
   the only thing between a request and that authority. A fault in its check
   is therefore a fault with the whole of its vault role behind it. What
   bounds that is that the role cannot read or delete, so the worst a
   compromised custodian does is overwrite credentials, which is loud; and
   that it is a process of its own, so nothing else runs under its identity.
9. *The registrar's rule about administrators is the registrar's code.* Its
   Keycloak client holds `manage-users` in the kernel realm, and Keycloak
   would add anybody to the administrators' group for it. The rule stops
   every caller the store allows, and a fault in a route; it does not stop
   whoever takes over the process. It also covers one group: the one behind
   the cluster's `admin` role. The groups behind the other platform roles
   (`securityOfficer`, `auditor`, `serviceAdmin`, `sharedAppsAdmin`,
   `breakGlass`) are managed like any other group of the kernel realm, by
   whoever holds `can_manage_users` on the platform tenant. Keycloak's
   fine-grained admin permissions could deny the client the group itself;
   that is not built.

**What moves, and in what order.**

| Step | Change |
| --- | --- |
| Rename ✅ | `edge-authz` → `bouncer` (binary, package, Deployment and Service, the route table and its annotations, values, environment); `sign-in` → `concierge` (the gentian-ui app and its image, the Deployment and Service). Done on an empty cluster: the bouncer's Service is named by every session policy, and renaming it under live policies refuses every request until the last one follows. The address a person sees (`/sign-in/`) is not renamed. |
| Usher, first route ✅ | `GET /v1/tenants/{t}/tiles`: the tiles of one tenant, open to whoever may enter it, each filtered by its own relation, built from the bouncer's table. The desktop's backend calls it in-cluster with the person's token; it has no public route. Tenant namespaces are let through to it in `kernel-edge`, as they are to the director. |
| Usher, the rest | `me` (the relations a person holds on a tenant and on the cluster), then status reads. Each leaves the director when its last caller has moved. The director's `GET /v1/clusters/{c}/tiles` — reachable only with a cluster relation, which is why a tenant's own people saw an empty desktop — goes with the first. |
| Launch rights ✅ | Tiles of installed catalogue apps need `can_launch` on `app:<t>/<p>`. The operator's projection writes `app#tenant` and `app#entitled` from `Tenant.spec.apps`, by the rule the portal applied: an app's own group entitles it, and a base with activated addons is entitled by the addons' groups. Every entry of `spec.apps`, and every addon activated inside one, is a `Component` the tenant reconciler creates and removes; the Component writes the App claim the app Composition answers, routes the app's exposures behind the zone's session, and asks `can_use` on the app at them. |
| Concierge to the DMZ ✅ | A component of the platform tenant with one perimeter surface, `authMode: none`, published from `tenant-platform-dmz` with an owner and a review date. The brand files it serves today move with the branding work, not with this. |
| Registrar ✅ | The director's people, group and realm-settings routes → `registrar` (package, executable, Deployment and ServiceAccount, Service, values `registrar.*`, the profile key `registrarUrlKey`). Paths, methods, bodies and the relation each route asks are unchanged; a caller changes the address. The per-realm Keycloak client is a new one, `gentian-registrar-admin`, with the roles the director's had and no more, handed over in the Secret `gentian-registrar-realms`; the operator removes the director's client from each realm and the Secret it was handed over in. The record of who was allowed to ask moved with the routes, to a database and a login of the registrar's own. A tenant's realm and login domain are read from the Tenant objects, where the director read them from git. The director holds no Keycloak credential, mounts none and answers 404 on every one of these paths. |
| Custodian ✅ | `credential manager` → `custodian` (package, executable, Deployment and ServiceAccount, Service, values, environment, the profile key `custodianUrlKey`, the consoles that call it). It left the operator's process for one of its own. It verifies the caller's token, asks the store `can_read_credential` or `can_write_credential` on the cluster or the tenant, and writes as itself with a vault role that cannot read a value. The exchange of a caller's token at the vault is gone, and with it what existed only for it: the vault's JWT role for cluster administrators, the per-tenant auth mounts and their group-bound roles, the director's audience on the vault's roles, and the `groups` scope on the zone clients. |

**Who enforces what, and what a compromise of each costs.** Read from the
code and the chart as they are, not from the names. "Enforces" is where a
request is refused; a process that only filters what it shows enforces
nothing.

| Process | Standing credentials | What it decides or enforces | What somebody who takes it over can do | What bounds that |
| --- | --- | --- | --- | --- |
| **Director** | The deployments repository's push credential; the key its commits are signed with; the token of the operator's command door, which is a shared string (§3.11); the OpenFGA key. No Kubernetes token is mounted, and it holds no Keycloak credential and no vault role. | Every change to declared state and every command of §3.11: it verifies the caller's token and asks the store the route's relation before it commits or relays. | Push a signed commit of anything, in anybody's name: tenants, apps, policies, and the Cluster claim -- which names the group that administers the platform and the catalogue sources software comes from. Issue every command of §3.11 for any tenant, naming anybody as the actor. Write the store directly (see below). | Nothing prevents it. Every commit stays in git, signed by its key, where it can be seen and reverted. It cannot read a secret from the vault or the cluster and cannot reach a realm. |
| **Operator** | A ServiceAccount whose ClusterRole reads and writes Secrets in every namespace, among much else; Keycloak's administrator credential; a vault role that reads and writes the whole platform path, the master password included; the OpenFGA key; the command door's token. The chart also mounts the deployments repository's credential into it -- the same one the director pushes with -- though not the signing key. | It writes the store: the cluster's roles from the claim, the tenants, the apps, and membership from Keycloak's signed statements. It admits or refuses a Tenant at admission. At the command door it checks the director's token and nothing about the person. | Everything in the cluster. Every Secret is readable to it, so the director's signing key and the registrar's realm credentials are too; with those and the repository credential it can also do what they can. | Nothing inside the cluster. It is the trusted base. What protects it is that no person's request reaches it except through the command door, Keycloak's signed statements and the admission webhook. |
| **Custodian** | A vault role bound to its ServiceAccount that can create and update a value under the platform path and read and write its metadata, and cannot read a value or delete one; the OpenFGA key. In the cluster: read the credential catalogue, the tenants and the Cluster claim; create, change and delete `Repository` objects; update `ExternalSecret` objects; create and update ConfigMaps in its own namespace; read the vault's CA certificate. | Who may see that a credential is required and who may set one: `can_read_credential` and `can_write_credential`, on the cluster or on a tenant. | Overwrite any credential the platform stores, without being able to read one. Change where the cluster or a tenant installs software from, by changing a `Repository`. Write the store directly. | It cannot read or delete a secret, so what it does is loud: things stop working. No git, no Keycloak. |
| **Registrar** | One Keycloak client secret per realm (`gentian-registrar-admin`: `view-users`, `query-users`, `query-groups`, `manage-users`, `manage-realm`); the login of its own database; the OpenFGA key. In the cluster: read the tenants and the Cluster claim. | Who may manage a tenant's people (`can_manage_users`), set a realm's password policy (`can_set_policy`), hand over a tenant's administrator account (`can_configure` on the cluster) and count accounts (`can_audit`). That a tenant's request reaches that tenant's realm only. That nobody changes the platform administrators' group or a person in it. | In every realm, the kernel realm included, whatever those roles allow: create, disable or delete any account, set any password, remove any second factor, change any group membership. That includes the platform administrators' group: the rule that forbids it is the registrar's code, which a takeover replaces, and Keycloak carries the change out for this client. So it can make an administrator of the platform. It can also change the realm settings `manage-realm` covers, rewrite its own record of who asked, and write the store directly. | No git, no vault, no write in the cluster. A client per realm and none in `master`; no `manage-clients`, no `manage-identity-providers`, no `impersonation`. Keycloak writes an admin event for each change under the client's service account, and a change with no partner in the registrar's record is what an audit looks for. |
| **Usher** | The OpenFGA key. No Kubernetes token; the table of tiles is a mounted file. | Nothing. It filters what it lists by the store's answers. | Show or hide tiles, read from the store who holds what, and write the store directly. | A tile is not access: the bouncer decides each request. It holds nothing else. |
| **Bouncer** | The OpenFGA key. No Kubernetes token; the route table is a mounted file. | Every request on a route of the authenticated Gateway that attaches it: it verifies the session's token and asks the store the question the table names for that host. | Admit any signed-in person to any host behind it, or refuse everybody. It is shown the token of every session that passes, and that token is the one the director, the registrar and the custodian accept -- so for a token's lifetime it can ask them for anything that person may ask. It can also write the store directly. | It holds no credential of its own beyond the key, and a token it sees is good only until it expires -- five minutes, by item 4 above. |
| **Concierge** | None. | Nothing. It redirects a browser to a console of this cluster. | Serve what it likes on the cluster's bare domain, a false sign-in page included, and alter the brand files other pages load from it. | It holds no credential and no session passes through it. |

Three things in that table are weaker than the cast suggests, and they are
the same for every row.

- *One key opens the store for everybody.* OpenFGA is configured with a
  single preshared key, and the director, the operator, the custodian, the
  registrar, the usher and the bouncer all present it. Only the operator is
  meant to write; nothing but the code of the other five keeps them from it.
  A write to the store can grant any relation, the cluster's `admin`
  included. Per-process identities at the store, with the right to write
  given to one, would make "reads the store" a property of the credential.
- *One audience serves every door.* The zone's token is minted for
  `gentian-director`, and the director, the custodian, the registrar, the
  usher and the bouncer all accept that audience. A token relayed to one of
  them is therefore valid at the others for as long as it lives. Anything
  that sees a person's token -- the bouncer, a console's backend -- can use
  it at all of them.
- *The director's word is taken at the command door.* The operator checks a
  shared string and believes the name the director passes with it (§3.11).

### 3.11 The operator's command door

The operator's input is git. That is the rule, and this section is the
exception to it, kept on purpose and kept small.

Some things a person asks for are not a state to declare but an act to
perform once: take a backup now, delete that backup, restore from this
bundle, send this notification, tear down what an uninstalled app left
behind. Writing each as a commit would fill the deployments repository with
entries that describe nothing the cluster should converge on, and a restore
or a backup carries or produces material that must not be in git at all. So
the operator keeps an HTTP listener for them, and the director is its only
caller.

**What goes through it.** The commands, all `POST …/actions/<verb>`:

| Command | The director asks the store | What the operator does |
| --- | --- | --- |
| `backup` | `can_administer` on the tenant | creates the export request |
| `delete-backup` | `can_administer` on the tenant | deletes one export and its bundle |
| `bundles`, `bundles/inspect` | `can_configure` on the cluster | takes an uploaded bundle, or says what one contains |
| `restore` | `can_configure` on the cluster | creates the restore request from an uploaded bundle |
| `notify` | `can_administer` on the tenant | publishes one notification to the tenant's people |
| `purge-app` | `can_install_app` on the tenant | removes the data of an app that is no longer installed |
| `provision-app` | `can_grant` on the tenant | puts the tenant's people in an app's group again |

and the reads of live state the director relays for the consoles (backups,
schedules, resource usage, integrations, notifications, app status), which
move to the usher (§3.10).

**The rules of the door.**

- *One caller.* The listener accepts the director's token and nothing else,
  compared in constant time, and refuses every request when no token is
  configured. It verifies no person and asks the store nothing: by the time
  a command arrives the director has verified the caller and asked the
  question in the table above. The name recorded against the act is the one
  the director passes, and is worth something only because nobody else can
  pass one.
- *A closed list.* A command is added here by a change to this table and
  to the director's route table together. Anything that can be expressed as
  desired state is a commit instead and does not belong here. Installing an
  app, removing one and setting its addons are the director's commits now;
  the three routes that did them here are still registered, called by
  nothing, and are to be deleted with the operator's git clone (§6 D).
- *Nothing that changes who may do what.* No command writes a relation, a
  group that carries a right, or a policy. Those are projections of what git
  declares.
- *An act, not a state.* Each command answers what was started. Whether it
  finished is read from the object it created.

**What this costs.** The director's token is a second standing credential
in the director's process, beside the push credential, and it is a shared
secret rather than an identity: whoever holds it can issue every command in
the table for any tenant and name anybody as the actor. The commands are
bounded — none reads a secret or grants a right — but `delete-backup` and
`purge-app` destroy data. Two things would narrow this and neither is built:
the operator verifying the director's ServiceAccount token instead of a
shared string, and the operator re-asking the store with the person's
subject rather than taking the director's word.

## 4. Bootstrap: writing configuration before Keycloak exists

The question is real only for the writes that happen before the director can
authenticate anyone. Enumerating them shows there are fewer than expected:

| Moment | Write | Who | How it stays secure |
| --- | --- | --- | --- |
| `--prepare-deployment` | `clusters/<id>/kernel/{claims,values.yaml}` | human, local files, then their own commit | already the design: install.sh never writes the repo; the human reviews and commits with their own git identity and branch-protection rights |
| `--prepare-tenant` | `definitions/<t>/tenant.yaml` | same | same |
| Install steps A–E | none to git | — | install.sh reads git; the only imperative writes are cluster objects (the tier-0 `Repository/deployments` claim in B-09, the bootstrap Applications) |
| First tenant | `tenants/<t>/tenant.yaml` | after install completes, through the director | Keycloak `kernel` realm (D-06) and OpenFGA store (authz bridge) exist by then |

So the director never needs a bootstrap identity: **nothing during install
asks it to write**. It comes up at D-01 alongside the operator, fails closed
(every request 401) until its OIDC issuer is reachable, and becomes usable
when the portal does.

The bootstrap *exception* is not in the director; it is that humans hold push
rights on `main` during install. It is closed the same way the OpenBao root
token exception is closed by
[E-04-revoke-bootstrap-token](../../scripts/steps/E-04-revoke-bootstrap-token.sh)
and proven by [internal/handover](../../internal/handover/handover.go):

**E-05-director-handover**

1. The director performs one real signed commit as the installing platform
   admin (`administrator@<KERNEL_DOMAIN>`, obtained by the same browser login
   the custodian handover already requires) — a no-op "handover"
   commit. That commit *is* the proof the write path works, recorded in the
   `gentian-handover` ConfigMap as `configWritePathProven`.
2. The git host is configured so only the director's identity may push
   `main`; the installer prints what it could not verify remotely.
3. The Argo credential is rotated to the read-only token (§3.3); the push
   token exists in exactly one Secret, mounted in exactly one Deployment.
4. *(wave 3, G11 — a later step, not part of the handover gate)*
   `AppProject/gentian.spec.sourceIntegrity` gets the GnuPG policy for the
   deployments repository (mode `head`, keys: the director's and the
   break-glass key from the recovery kit). Because `head` verifies the
   commit at the sync revision, step 1 must have happened or every
   Application goes OutOfSync on the last human commit.

Until step 1 succeeds, `E-01-tenants` stays gated, for the same reason the
credential handover gates it: recovery is cheap on an empty cluster and
catastrophic on one with tenants.

## 5. What moves

"Copy" during §6 B; the operator-side original is deleted in §6 D.

| Today (operator) | Fate | Destination |
| --- | --- | --- |
| `applifecycle/gitops.go`, `gitops_addons.go`, `gitops_resources.go`, `names.go`, `types.go` | copy | `internal/director/gitops` |
| `applifecycle/http.go`, `http_resources.go`, `resources.go` (plan selection, entitlement ceiling) | copy, drop `Actor`, add auth middleware | `internal/director/api` |
| `applifecycle/service.go` — `validateProfile` | copy → `ensureProfile` (§3.6) | director |
| `applifecycle/service.go` — `Install`/`Uninstall` orchestration | rewrite as commit + 202 | director |
| `applifecycle/wait.go`, `reconcile.go` (Argo refresh) | delete; Argo syncs on host webhook + polling | — |
| `applifecycle/purge.go` | stays, moves out of the request path into a reconciler | operator |
| `applifecycle/service.go` — `provisionAppGroupUsers` | stays; becomes desired-state reconcile of `Tenant.spec.apps` → Keycloak group | operator (`app_privilege_reconciler` already exists) |
| `custodian/` | later, optional: same class of human-identified write, holds no token of its own, needs no controller-runtime | director, after D |
| `kubectl-gentian` `git_commit_push` + `kubectl apply` fallback | replaced by director API calls with the user's token (`kubectl gentian login` via device flow) | CLI |
| Console direct writes (§2.2, desired-state rows) | replaced by director calls with the user's token, as `custodian.py` already forwards it; reads become director reads too — the console keeps no Kubernetes identity (G7, [ui-restructure.md](ui-restructure.md) §2) | tenant desktop BFF (`tenant-<t>`) and the platform tenant's in `tenant-platform`, AD-10 |
| Console direct writes (§2.2, request rows) | director creates the request CR; secrets stay ESO/OpenBao references — settled by G7: all writes move | director, narrow RBAC on those kinds |
| `catalogue-<repo>` ApplicationSet (every AppProfile synced to every cluster) | retired; the director materialises on reference (§3.6) | — |
| App Store (`app-store-me` profile, per tenant) | leaves the cluster (AD-3); the director is its ingestion endpoint | external |
| `chart: initContainers.git-clone-deployments`, `git-credentials` volume, `appLifecycle.*` values | delete | — |
| `Repository/deployments` composition | split read credential from push credential | Crossplane |

## 6. Cutover

The order you proposed is right. Refinements are marked.

### 0. Authenticate the existing endpoint now (wave 0, G1)

Before any of A–D: bearer verification on `internal/applifecycle/http.go`
against the kernel and tenant realms, the `X-Gentian-Actor` header ignored,
the BFF and CLI sending the user's token (the BFF already holds it for the
custodian). No FGA check yet — that waits for the director — but
"anyone on the network can push config" ends here, in one change to a live
endpoint, independent of everything below.

### A. Director standalone, no cluster

- `cmd/director`, `internal/director/{api,authn,authz,gitops}`.
- Contract tests against: a local bare repo as remote, a mock OIDC issuer
  (static JWKS), OpenFGA in a container with model v1 and the `tests.fga.yaml`
  tuples. Cover: unauthenticated → 401; wrong `iss`/`aud` → 401; tenant admin
  of A writing B → 403; allowed write → signed commit with the expected
  author/committer/trailer; concurrent writers → both land (§3.7).
- Existing `gitops_*_test.go` cases port unchanged — they already run against
  a fixture remote.
- **Refinement:** ship the chart and RBAC in this step too, rendered but not
  enabled, so B is a values flip and not a chart change.

### B. Side by side, one function at a time

Deploy the director next to the operator. Both can push (§3.7 handles it).
Switch callers by URL: `APP_LIFECYCLE_URL` in the BFF, a `DIRECTOR_URL` in
the CLI. Order, smallest and most API-shaped first:

1. Resources plan (`PUT …/resources`) — already API-only, one file patch.
2. Apps install/uninstall/addons — the console has no install path today, so
   this is the CLI plus the App Store, which becomes the external caller of
   AD-3 in the same step: the per-tenant `app-store-me` profile is replaced by
   the external service calling `POST /v1/tenants/{t}/apps/{p}` with the
   user's token.
3. Tenant deploy/undeploy — CLI-only today; gains authentication for the
   first time.
4. Console desired-state writes (§2.2).
5. Console request writes.

After each: flip the corresponding operator endpoint to `405` behind a
per-function flag, run the step's tests, keep the flag for one release.

**Refinement:** the operator's `kubectl apply` fallback in the CLI is deleted
in step 3, not D — it is the one path that bypasses git entirely.

### C. Bootstrap integration and security challenge

- `D-01-operator` installs both charts; `E-05-director-handover` as in §4.
- Split the repository credential (§3.3); `B-09` claims both.
- Challenge list, each a scripted test under `crossplane/tests/e2e` or
  `scripts/tools`:
  - push to `main` with the Argo token → rejected by host
  - push to `main` with a human token after handover → rejected
  - unsigned commit on `main` (force through as admin) → Argo refuses to sync
  - director pod's SA: `kubectl auth can-i` matrix shows no `pods/exec`,
    no `secrets` list, no `apps` create
  - operator pod: no git credential mounted, no route to the git host
    (NetworkPolicy)
  - replayed/expired token → 401; token from tenant realm A on tenant B → 403
  - OpenFGA unreachable → director fails closed, operator unaffected

### D. Decommission

Delete the operator-side copies from §5, the init container, the values, the
`X-Gentian-Actor` header everywhere, and the `NeedLeaderElection` special
case. Remove `pods/exec` from nothing — purge still needs it — but confirm the
ClusterRole no longer lists `argoproj.io` write verbs.

## 7. Adjacent scope the split pulls in

Three things that are not git writers today but become the director's
business once "intent goes through git, authenticated" is the rule.

### 7.1 Kyverno

**Today.** The baseline `ClusterPolicy` objects live in this repository
([kernel/security/kyverno/policies/](../../kernel/security/kyverno/policies/))
and Argo syncs them from `gentian-os` itself
([05-admission.yaml](../../kernel/appsets/raw/05-admission.yaml)). They are
release content: changing one is a PR to gentian-os, already authenticated by
the git host and reviewed. The per-cluster *decision* is the
`PlatformSecurityPolicy` MAC-waiver allowlist, which the console writes
directly to the API server; the operator turns an approval into namespace
labels that the policies' exclusions match
([mac_waiver_reconciler.go](../../internal/controller/mac_waiver_reconciler.go)).
Nothing writes `PolicyException`s dynamically.

**Target.** Two layers, each in the repository that owns it:

- Baseline policies stay in gentian-os. Their trust comes from the release
  pipeline, the same as the operator image.
- Per-cluster policy is a directory in gentian-deployments,
  `clusters/<c>/kernel/security/`, synced by one more Application in the
  claims ApplicationSet. It holds the `PlatformSecurityPolicy` (moved out of
  the console's direct write, §2.2) and any cluster-specific
  `PolicyException` or policy overlay an operator adds. Written only through
  the director: `PUT /v1/clusters/{c}/security/{kind}/{name}`. Two
  relations, following the role definitions: the `PlatformSecurityPolicy`
  allowlist approves what escapes the default posture, which is the
  **security officer's** (`cluster#can_approve` — the one who installs is not
  the one who approves exceptions, roles-and-authorizations §1); a
  `PolicyException` or overlay changes what `kernel-admission` enforces,
  which is break-glass only (`cluster#can_set_admission`).

The webhook that admits a `PlatformSecurityPolicy` does not change; it only
stops seeing writes from the console SA.

### 7.2 Network policies

**Today.** No NetworkPolicy is hand-authored anywhere — not in `kernel/`, the
compositions, or gentian-deployments. Every one is derived by
[internal/kernel/netpolicy](../../internal/kernel/netpolicy/) from
`AppProfile.kernelRequirements` and `security.egress`, the Tenant's apps, and
cluster config (namespaces, routing mode, API-server CIDR). The inputs are
already in git; the outputs are computed. There is no authored knob: no
`Tenant.spec.network`, no cluster egress allowlist.

**Target.** Keep the derivation — committing the derived objects would make a
second source of truth that drifts from the first and cannot be edited
meaningfully. What goes to git through the director is the **intent** that is
missing today:

- `Cluster.spec.network.egressAllow[]` — cluster-wide CIDRs/FQDNs every
  tenant may reach (a corporate proxy, a license server).
- `Tenant.spec.network.egressAllow[]` and `denyKernel[]` — per-tenant
  additions and withdrawals, bounded by the cluster list (a tenant cannot
  widen past what the cluster allows; the webhook enforces the subset).

The operator merges these into `BuildDesired` alongside the profile-derived
rules. Both fields land in the claims the director already edits, so no new
endpoint is needed beyond §3.5's `PUT /v1/files` semantics applied to a
schema'd field. If an auditor wants the effective policy set, it is a
read-only rendering (`GET /v1/tenants/{t}/network/effective`) served from the
cluster, not a commit.

### 7.3 Credential requirements

**Today.** [credentials.yaml](../../credentials.yaml) generates sixteen
`CredentialRequirement` CRs and
[C-06](../../scripts/steps/C-06-credential-catalogue.sh) applies every one
of them to every cluster. The applicability logic exists, but only on the
installer side and only in shell:
[`_requirement_applies()`](../../scripts/lib/credentials.sh) gates on
repository auth type, `INFRA_CHART_PRIVATE`, the DNS and edge-ingress
provider tables and `CERT_ISSUER_MODE`. The console reads the CRs and so
shows all sixteen. The `Repository` claims are the one place this is already
right: their requirement is composed only when the claim declares a
credential.

**Target.** The rule moves from shell into the catalogue and is evaluated
against the Cluster claim, which already carries every axis the shell reads:

```yaml
# credentials.yaml
- name: smtp-relay
  appliesWhen:
    cluster.mail.serviceMode: external
- name: acme-dns-cloudflare
  appliesWhen:
    cluster.certificates.issuerMode: acme-dns01
    cluster.certificates.dnsProvider: cloudflare
- name: infra-chart-registry
  appliesWhen:
    repository.infra.credential: present
```

Two steps, in this order:

1. **List-time filtering.** `appliesWhen` is generated into the CR spec; the
   custodian evaluates it against the Cluster claim when it lists,
   and the installer's `_requirement_applies()` is regenerated from the same
   field so the two carriers cannot disagree (`make verify-gen` already
   asserts this for the rest of the content). The console shows only what
   applies. No new controller; matches the "no controller" design of the
   catalogue.
2. **Emit only what applies.** C-06 stops applying the file. The Cluster
   composition reads the catalogue (shipped as a ConfigMap by the chart) and
   composes the applicable `CredentialRequirement`s, the way the Repository
   composition already does for its own. Requirements then appear and
   disappear with the claim, and a mode change in git retires the
   requirement it made obsolete.

The declaration of *which* credentials a cluster expects is therefore the
Cluster claim — in git, written through the director. The values never are.

### 7.4 Where these land in the cutover

| Item | Cutover step |
| --- | --- |
| `PlatformSecurityPolicy` via director; `clusters/<c>/kernel/security/` Application | B, step 4 (console desired-state writes) |
| `Cluster.spec.network`, `Tenant.spec.network`, merge in `BuildDesired` | after B — a CRD change, independent of the split, sequenced here so the new fields never get a console direct-write path |
| `appliesWhen`, list-time filtering | A — the custodian is the first consumer of the Cluster claim the director reads anyway |
| Composition-emitted requirements, C-06 retired | C, with the credential split (§3.3) |

## 8. Open decisions

- **Signing key format.** GPG is what Argo verifies today; SSH signing is
  simpler to hold in OpenBao. Pick GPG unless Argo's SSH verification lands
  first.
- **Credential split shape.** Second `Repository` claim (`deployments-push`)
  vs. a `credential.push` field on the existing one. The field keeps one
  object per repository, which the XRD's own rationale prefers.
- **How the external App Store presents the human.** The user's own token
  (the store is a pure client; the token's `aud` must include the director)
  or an exchanged token with `act` (the store is an agent in the principle 5
  chain, and its tuples must exist). The first is simpler and keeps the
  store out of the FGA model; the second is what an autonomous store action
  — a scheduled upgrade — will need. Start with the first.
- **Refuse by default at the Gateway.** Attach the bouncer to the
  authenticated Gateway itself, so a route with no line in its table is
  refused, and keep the per-route policy for the zone's session only.
  Recommended: it turns the two holes of §3.10 into something that cannot
  recur. To verify first: that Envoy Gateway applies a Gateway-level
  external authorization together with a route-level OIDC session in the
  order the bouncer needs, and what it does to the edge's own `/oauth2/`
  paths.
- **Where the bouncer's table comes from.** From the `Component` and its
  profile, written by the operator into the table directly, instead of from
  annotations on the routes. Recommended, with the default above.
- **Which reads the director keeps.** Declared state for those who may
  change it; everything a person needs merely to use the platform goes to
  the usher. The line to hold: a director route that a tenant member's
  desktop calls on every load is on the wrong side of it.
- **The concierge's address.** A host of its own in the kernel zone, or the
  kernel domain's apex itself. The apex is what people type; a perimeter
  surface on it means the apex is served from the DMZ rather than redirected
  from the authenticated Gateway.
- **Whether `custodian` moves in the same milestone.** Recommended no —
  it is correct today and the move is mechanical once the director exists.
