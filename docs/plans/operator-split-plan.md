# Director / operator split

Issue #185. Splits the gentian-os process into two binaries with one invariant
between them:

```
human intent ──► director ──writes git only──► Argo CD ──► operator ──writes cluster only──► Crossplane
```

The **director** is the only process that can push to `gentian-deployments`.
Every push it makes was authenticated by Keycloak and authorised by OpenFGA,
so the repository becomes an audit log that can be trusted as the single
source of truth for cluster configuration. The **operator** never holds a git
credential: it converges the cluster on what Argo CD hands it, and its trust
in that input comes from the chain in §3.4, not from a shared secret.

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
   controllers, the admission webhook, the credential manager and the
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
|---|---|---|---|
| Operator lifecycle API | [internal/applifecycle](../../internal/applifecycle/) — `Install`, `Uninstall`, `SetAddons`, `SetResourcePlan` commit and push | operator's push token, `Actor` from an `X-Gentian-Actor` header the caller sets | nobody — the header is trusted |
| `kubectl gentian` | [scripts/kubectl-gentian](../../scripts/kubectl-gentian) `git_commit_push`, 11 call sites: `tenants deploy/undeploy`, `apps install/uninstall`, deletionPolicy flips around purge | the human's own git credential on their workstation | git host permissions only |
| `kubectl gentian` fallback | `apply_tenant_manifest_from_git` → `kubectl apply -f` when no Argo app exists | the human's kubeconfig | Kubernetes RBAC only — bypasses git entirely |
| `install.sh --prepare-deployment` / `--prepare-tenant` | writes local files and **stops**; the human commits ([deployment.md §3](../deployment.md)) | the human | review before commit |
| Humans | direct commits | their git identity | git host permissions |

Not a writer: argocd-image-updater uses `write-back-method: argocd`
([gentian-os.yaml](../../kernel/bootstrap/chart/templates/gentian-os.yaml))
and patches Applications in-cluster.

### 2.2 Human-facing writes that bypass git

The admin console (gentian-ui BFF) writes these directly to the API server
with its own ServiceAccount:

| Object | Console module | Nature |
|---|---|---|
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
  header, ever; identity is the token's verdict.
- An OpenFGA client for `Check` (already in
  [openfga_client.go](../../internal/authz/openfga_client.go)) against the
  store the authz bridge publishes in the `openfga-runtime` Secret.
- A working checkout of `gentian-deployments` and the **only** push
  credential. Commits are authored as the human (`Name <email>` from the
  token), committed by the director, and **signed** with a key only the
  director holds. The commit message carries a trailer with the OpenFGA
  decision (`Gentian-Authz: user:<sub> can_install_app tenant:<t> allowed`).
- A **read-only** Kubernetes client for `Tenant`, `App`, `AppProfile` status
  and the `openfga-runtime` Secret — enough to answer status queries and
  validate requests. Plus exactly one narrow write grant, `appprofiles`
  create/update, for materialise-on-reference (§3.6). No `pods/exec`, no
  `secrets` beyond the one named Secret, no Argo objects.
- A rate limiter and a request log that records subject, tenant, operation,
  decision, commit SHA.

It does **not**: wait for readiness, purge, exec, touch Keycloak groups, or
annotate Argo. The commit is the completed action; every write returns `202`
with a status URL, and status is served from the read-only client.

### 3.2 Operator

Unchanged in role, smaller in surface. After decommissioning (§6 D) it has:
no git clone init container, no `.git-credentials` mount, no lifecycle HTTP
listener, no `GENTIAN_DEPLOYMENTS_*` env. It keeps `pods/exec` because purge
needs it — but purge becomes a reconcile of desired state (app absent from
`Tenant.spec.apps` and the App claim gone → teardown converges), which also
fixes the current failure mode where a request that dies mid-purge leaves
half-deleted state with nothing to resume it.

### 3.3 Argo CD

Stays exactly where it is in the chain: it reads git and applies. Two
changes:

- It gets its **own, read-only** repository token, distinct from the
  director's push token (§2.3 is dissolved: two vault paths, or one claim
  field `credential.push` alongside `credential`).
- The `gentian` AppProject gets `spec.signatureKeys` set to the director's
  signing key, so Argo refuses to sync a commit it did not sign. This is what
  lets the operator trust git without trusting the git host. Known caveat:
  Argo's signature verification applies to Applications, not to the
  ApplicationSet git generator's own fetch — the generated `Application`s are
  still verified, which is where the manifests are applied.

### 3.4 Trust chain

```
Keycloak token ──► director verifies JWKS/iss/aud
             ──► OpenFGA Check(user, relation, object)
             ──► signed commit, authored as the human, trailer with decision
             ──► git host branch protection: only the director's identity may push main
             ──► Argo AppProject signatureKeys: only director-signed commits sync
             ──► operator reconciles what Argo applied
```

Each link is independently enforced. Compromise of the git host cannot inject
config (signature check); compromise of the director's push token without its
signing key cannot either; a stolen token from realm A cannot act on tenant B
(FGA); and the operator SA holds nothing that writes git.

### 3.5 API

The existing contract is kept so callers switch by URL, not by rewrite:

```
GET    /v1/tenants                              (list, filtered by what the caller may see)
GET    /v1/tenants/{t}
POST   /v1/tenants/{t}                          deploy a definition (today: kubectl gentian tenants deploy)
DELETE /v1/tenants/{t}                          undeploy
GET    /v1/tenants/{t}/apps
POST   /v1/tenants/{t}/apps/{p}                 → 202 + Location
DELETE /v1/tenants/{t}/apps/{p}                 → 202
PUT    /v1/tenants/{t}/apps/{p}/addons
GET    /v1/tenants/{t}/resources | /plans | /usage | /report
PUT    /v1/tenants/{t}/resources
PUT    /v1/tenants/{t}/policies/{kind}/{name}   backup, security, grants, export schedules (§2.2)
POST   /v1/tenants/{t}/requests/{kind}          export / restore — creates the request CR, secrets via ESO reference only
GET    /v1/operations/{id}                      status of a 202
PUT    /v1/files/{path}                         break-glass raw edit; FGA relation `can_edit_raw`, always audited
```

`/v1/files` exists so "sole writer" survives real operations: a platform
operator who needs to hand-edit a claim does it through the director, and the
edit is signed and attributed like any other.

### 3.6 Authorisation model

Additions to [authz/model/v0/model.fga](../../authz/model/v0/model.fga), as
v1:

```
type cluster
  relations
    define operator: [user, group#member]        # gentian:platform:superadmin / :operator
    define break_glass: [user, group#member]     # gentian:platform:break-glass
    define can_deploy_tenant: operator
    define can_edit_raw: break_glass

type tenant
  relations
    define cluster: [cluster]
    define member: [user]
    define admin: [user]
    define can_install_app: admin or operator from cluster
    define can_set_plan: admin or operator from cluster
    define can_set_policy: admin or operator from cluster

type catalogue_entry
  relations
    define entitled: [tenant]
    define can_install: entitled            # checked with tenant as user: tenant:<t>
```

Two things to settle while touching the model. First, v0 defines
`admin: [user] or member`, which makes every member an admin; the director
must not inherit that. Second, the authz bridge syncs Keycloak groups into
tuples on a 5-minute requeue
([authz_bridge_reconciler.go](../../internal/controller/authz_bridge_reconciler.go));
a freshly granted admin waits up to that long. Acceptable, but the director
should surface "not yet synced" distinctly from "denied".

Each relation gets a case in `tests.fga.yaml` before the director calls it.

Materialise-on-reference: the director resolves the profile from the
catalogue, applies the `AppProfile` CR (label `gentianos.io/profile-name`,
digest annotation), **then** commits. Apply-then-commit is the only ordering
that fails safe: a failed commit leaves an inert, unreferenced CR; commit-then-
apply leaves git asserting a state the Tenant webhook rejects on every sync.

### 3.7 Concurrency

Today `lockApp` is an in-process mutex justified by the operator running a
single replica, while the lifecycle `Runnable` opts *out* of leader election
"so any replica can serve" — two comments that contradict each other, kept
true only by `replicaCount: 1`. The director uses git itself: push, and treat
a non-fast-forward rejection as the optimistic-concurrency signal — fetch,
rebase the one-file change, retry, bounded. Correct across N replicas and
across the side-by-side period of §6 B when two processes push to one repo.

## 4. Bootstrap: writing configuration before Keycloak exists

The question is real only for the writes that happen before the director can
authenticate anyone. Enumerating them shows there are fewer than expected:

| Moment | Write | Who | How it stays secure |
|---|---|---|---|
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
   the credential-manager handover already requires) — a no-op "handover"
   commit. That commit *is* the proof the write path works, recorded in the
   `gentian-handover` ConfigMap as `configWritePathProven`.
2. `AppProject/gentian.spec.signatureKeys` is set to the director's key.
   Because Argo verifies the commit at the sync revision, step 1 must precede
   this or every Application goes OutOfSync on the last human commit.
3. The git host is configured so only the director's identity may push
   `main`; the installer prints what it could not verify remotely.
4. The Argo credential is rotated to the read-only token (§3.3); the push
   token exists in exactly one Secret, mounted in exactly one Deployment.

Until step 1 succeeds, `E-01-tenants` stays gated, for the same reason the
credential handover gates it: recovery is cheap on an empty cluster and
catastrophic on one with tenants.

## 5. What moves

"Copy" during §6 B; the operator-side original is deleted in §6 D.

| Today (operator) | Fate | Destination |
|---|---|---|
| `applifecycle/gitops.go`, `gitops_addons.go`, `gitops_resources.go`, `names.go`, `types.go` | copy | `internal/director/gitops` |
| `applifecycle/http.go`, `http_resources.go`, `resources.go` (plan selection, entitlement ceiling) | copy, drop `Actor`, add auth middleware | `internal/director/api` |
| `applifecycle/service.go` — `validateProfile` | copy → `ensureProfile` (§3.6) | director |
| `applifecycle/service.go` — `Install`/`Uninstall` orchestration | rewrite as commit + 202 | director |
| `applifecycle/wait.go`, `reconcile.go` (Argo refresh) | delete; Argo syncs on host webhook + polling | — |
| `applifecycle/purge.go` | stays, moves out of the request path into a reconciler | operator |
| `applifecycle/service.go` — `provisionAppGroupUsers` | stays; becomes desired-state reconcile of `Tenant.spec.apps` → Keycloak group | operator (`app_privilege_reconciler` already exists) |
| `credentialmgr/` | later, optional: same class of human-identified write, holds no token of its own, needs no controller-runtime | director, after D |
| `kubectl-gentian` `git_commit_push` + `kubectl apply` fallback | replaced by director API calls with the user's token (`kubectl gentian login` via device flow) | CLI |
| Console direct writes (§2.2, desired-state rows) | replaced by director calls with the user's token, as `credential_manager.py` already forwards it | BFF |
| Console direct writes (§2.2, request rows) | director creates the request CR; secrets stay ESO/OpenBao references | director, narrow RBAC on those kinds |
| `chart: initContainers.git-clone-deployments`, `git-credentials` volume, `appLifecycle.*` values | delete | — |
| `Repository/deployments` composition | split read credential from push credential | Crossplane |

## 6. Cutover

The order you proposed is right. Refinements are marked.

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
2. Apps install/uninstall/addons — the BFF has no install path today, so this
   is the CLI plus whatever calls the operator API.
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

## 7. Open decisions

- **Signing key format.** GPG is what Argo verifies today; SSH signing is
  simpler to hold in OpenBao. Pick GPG unless Argo's SSH verification lands
  first.
- **Credential split shape.** Second `Repository` claim (`deployments-push`)
  vs. a `credential.push` field on the existing one. The field keeps one
  object per repository, which the XRD's own rationale prefers.
- **Where the request CRs (`TenantExport`, `TenantRestore`) are created.**
  Director with a narrow write grant, or the console keeps its SA for exactly
  those kinds. The former keeps one human-facing write API; the latter keeps
  the director's RBAC purer.
- **Whether `credentialmgr` moves in the same milestone.** Recommended no —
  it is correct today and the move is mechanical once the director exists.
