# Open items, M1 to M4

**What this is.** The list of what is open between milestones M1 and M4 of
this repository: what departs from the plans, what is built and has never
run on a cluster, the known defects, the plan changes nobody confirmed, and
the decisions that wait for the owner. An item leaves the list when the
thing works on a cluster, not when the code exists.

**Last checked** on 2026-10-11 against commit `5a265734` of branch
`develop`. No cluster was used for the check. Where an entry rests on the
user-interface repository (`gentian-ui`) or the catalogue repository
(`gentian-apps`), it was read on their branch `develop` at `3c7130ac` and
`a04b9f6`, and the entry says so.

**How to read the markers.**

| Marker | Meaning |
| --- | --- |
| *confirmed in code* | The file named was opened at the commit above and says what the entry says. |
| *reasoned, not confirmed* | Follows from reading the code or the documents; no test and no cluster has shown it. |
| *needs a cluster* | Cannot be settled by reading. It has to be looked at on a running cluster. |
| **Decided** *date* | The owner's answer is on record for that date and nothing found since puts it in doubt. |
| **Open, awaiting the owner** | No answer of the owner covers it. What the code does today is stated, and it is not a decision. |
| **Built, not decided** | It is in the code. The owner was not asked, or was asked in a way that did not say what the answer would bring. |
| **Closed in the code** | The code was changed. It has not been seen working on a cluster, so the entry stays. |

Sections 1 to 6 are the list as it stood, corrected. Sections 7 to 12 were
added on 2026-10-11. Entries in sections 2, 5 and 6 that carry no marker
were carried over and were not checked again one by one on that day.

**Where to find what.**

| Question | Section |
| --- | --- |
| What departs from [networking.md](networking.md) §1? | 7 |
| What does each program hold beyond [operator-split-plan.md](operator-split-plan.md) §2? | 8 |
| What is not enforced on a default install? | 9 |
| Which plan changes were made after the code and are not confirmed? | 10 |
| Which decisions wait for the owner? | 4, summary at its top |
| What was built and never run on a cluster? | 2, and 11 for what a cluster has to answer |
| What was closed recently? | 12 |
| How do the architectural decisions stand against the code? | 1 |
| Which defects are known? | 3 |

M1 is *the platform administrator signs in and the installer leaves a cluster a
tenant can be provisioned on*; it was reached on 2026-10-03. M2 is *the first
functional tenant*, M3 *the first user invited by a tenant administrator*, M4
*the first app a user can actually work in*. The milestones and their steps
live in [implementation-plan.md](implementation-plan.md); this file is only
what is still open and why.

## What to watch on the first test install

Drawn from the entries below; each line names its entry.

1. **The kernel's network rules are off.** A default install applies none
   of the twelve policies of
   `kernel/security/network-policies/kernel-network-policies.yaml`. Section 9
   lists what then rests on credentials alone. They are turned on after a
   successful install with `KERNEL_NETWORK_POLICIES=true` and
   `./install.sh --only A-01,B-01` (§2, §4 decision 8).
2. **Keycloak 26.8.0 with provider-keycloak v2.19.0.** The provider that
   creates realms and clients is pinned at a version from before this
   Keycloak line. A tenant's Realm was created and read back in local
   containers on 2026-10-13 and every declared field arrived, the mail
   server with its password included; clients were not tried, and are the
   first thing to look at (§11.1).
3. **A realm's mail.** After an invitation, Keycloak's log is where a mail
   server without its password shows (`no password specified?`); the Realm
   resource says `Synced` either way. On a cluster with its own mail, look
   for `keycloak-smtp-credentials` in `kernel-authentication` and for a
   `keycloak-tenant-smtp-<tenant>` Job per tenant (defect 31).
4. **Crossplane with only 25 resource types activated** has not run on a
   cluster (§2, defect 13).
5. **A pod-security waiver takes effect only once it is granted on the
   install**, and that grant is approved under the cluster's `can_approve`,
   not by the tenant's administrator. Nextcloud's bundle asks for three
   waivers for Collabora. Until they are approved the tenant reports
   `WaiverNotGranted` (§4 decision 23, §11.2).
6. **Integrations between two apps start closed.** The operator writes no
   `AppGrant`. Each integration waits for the tenant's administrator
   (§4 decision 22).
7. **A cluster installed from a branch reads the development catalogue.**
   That catalogue is not republished at present (defect 18), and its XWiki
   profile cannot be installed (defect 19).
8. **A public path on an app's own host may take the signed-in app off
   that host.** No app in the catalogue declares one; the page on the bare
   domain is the only public entry a fresh install has (§7.2).
9. **The platform tenant's admin console is at
   `admin.platform.<kernel domain>`** and needs the wildcard certificate
   `*.platform.<kernel domain>`, which only a DNS-01 issuer can give
   (§4 decision 41).
10. **Whether Argo CD checks commit signatures** depends on the repository
   and the key ids being known when the bootstrap chart is rendered. Look at
   `AppProject` `gentian` for `sourceIntegrity` after the install (§8.9,
   §11.3).
11. **Three apps are signed in by the sign-in sidecar**: Activepieces,
    Docmost and OpenProject. In OpenProject the password form is off and the
    seeded administrator is locked (§4 decision 44).
12. **A tunnel install works with one Cloudflare token that carries both
    permissions**, entered at both questions. The separate tunnel token is
    stored and used by nothing (§4 decision 30).
13. **The smoke check of the cluster's own mail cannot pass** (defect 22).
14. **The installer refuses Kubernetes older than 1.33**
    (`scripts/lib/common.sh`, `GENTIAN_MIN_KUBERNETES_MINOR`; §4 decision
    52).
15. **If the cluster's Crossplane definitions are older than the software,
    the operator holds its work and names the remedy**:
    `./install.sh --only B-06` (§4 decision 40).
16. **A person whose recovery address differs from their workspace address,
    signing in to Docmost, Activepieces or OpenProject**: the app may be told
    the recovery address, or the sign-in may be refused (defect 29).
17. **The account name Nextcloud and XWiki give a person invited on the
    platform**: the claim they read it from may be empty (defect 30).

## 1. The architectural decisions, against the code

Checked against the implementation, not against the plans. "Holds" means the
code does what the decision says **as the decision reads today**; it does not
mean a cluster has shown it. Six entries were changed between 2026-10-06
and 2026-10-09 after the code (AD-1, AD-4, AD-6, AD-12, AD-13, AD-14;
section 10), so for those "holds" also means the decision was moved to
where the code was. The rows for AD-1, AD-2, AD-5, AD-6, AD-9, AD-10, AD-13
and AD-14 were corrected on 2026-10-11.

| AD | | State |
| --- | --- | --- |
| AD-1 | Nine security principles normative; every request passes one named enforcement point | **Holds as amended on 2026-10-09; the amendment is not confirmed as written (§10.4).** The publishing proxy is a named enforcement point that checks no caller: it filters and limits. On an entry of `authMode: app` it passes the caller's credential to the app, which checks it; the platform does not know that caller, and a removed person's app credential lives until the app revokes it (roadmap 1.34). **Departs:** a route with no policy is served with no check at all (§7.3), and the sign-in sidecar's `/sso/acs` passes no enforcement point (§7.4). The entry names five enforcement points; the registrar and the usher decide requests and are not among them (§8.13) |
| AD-2 | The director is the only writer of the deployment repository | **Holds for who writes git; three things stand beside it.** Commits are signed, the operator holds no git credential, and the director is the only program on the cluster that writes the repository. For a private repository the installer gives Argo CD the repository's credential directly until the vault's copy takes over (`A-06`, `C-05-repository-handoff`). Beside it: Argo CD's rule that only signed commits sync is rendered only when the repository and the key ids are known at install (§8.9); Argo CD and the director share one credential that can push (defect 16); and the director has the operator do things that leave no commit, some of which destroy data (decision 35) |
| AD-3 | The store's data is outside the cluster; its interface is an app on it | **Holds on the cluster's side.** The operator places the App Store app on every tenant but the platform's while the cluster reports its licenses and names a store; an install is fetched at its digest, checked, committed, and checked again before rollout. A store address that is the cluster's own App Store host is not taken as a store |
| AD-4 | One catalogue kind, `ComponentProfile` | **Holds.** The design documents name the kind as the code does since 2026-10-09 |
| AD-5 | Privileges are requests with one approval path | **Does not hold for network paths.** Egress a profile declares reaches the network policy without an approval. So do a path to the whole control namespace for a profile that names the director's, custodian's, registrar's or usher's address, and any namespace a profile's annotation names (decision 34). Holds for integrations, pod-security waivers and cluster roles since 2026-10-10 (decisions 22 and 23) |
| AD-6 | `authMode` mandatory; a perimeter surface is published per tenant by an approver | **Holds as amended on 2026-10-09, not yet shown on a cluster; parts of the amendment are not the owner's (decision 2, §10.5).** Publishing works as the entry now reads: a proxy in `tenant-<t>-dmz` only for an entry an approver published under `can_expose`, with a review date. The proxy checks no caller, and no entry says it does any more: `authMode: app` passes the caller's credential to the app, which checks it (the limit AD-1 states), and `basic`, `signature`, `jwt` and `bearer` are refused on a perimeter entry by the schema. Who approves is as the entry says: the tenant's and the cluster's perimeter approvers named on the Cluster claim (AD-16), and the cluster's administrator, who holds `can_expose` in every tenant its cluster operates (§2). **Departs:** the entry says nothing is published by default, and the install publishes the page on the bare domain itself, with no approver (§7.10) |
| AD-7 | Namespaces named by tier | **Holds** |
| AD-8 | Kernel trust domains are separate namespaces | **Holds** |
| AD-9 | System services have no public route | **Holds by default; one claim setting departs from it.** Mail now faces the internet only through a proxy in `system-mail-dmz` that holds nothing; Postfix and Dovecot have no load balancer of their own. The model gateway's console has a claim setting, `llm.console.enabled`, off by default: off, `llm.<kernel domain>` has no route and the edge is not admitted to the gateway, so the cluster is in line with the decision on this point. Switching it on routes the console behind the kernel realm's session and `can_configure`, and contradicts the entry, whose text still says "without exception"; whether a switch is what the owner asked for is decision 6 ([llms.md](../design/llms.md)). TURN does not exist |
| AD-10 | The portal splits two ways; the platform is a tenant | **Holds for the split; one part contradicts it.** The entry says the desktop holds no administrative code path. The desktop has administrator-only template routes that it authorises itself (decision 39, §8.7) |
| AD-11 | The target layout applies to fresh installs | **Holds** |
| AD-12 | The authorization store is a projection: the operator writes it, from git and from Keycloak's events; the director asks it | **Holds for who writes, with two things not built, two stated as aims and one credential too many.** The operator receives Keycloak's signed statements and projects memberships, and projects roles, tenants, apps and grants from what git declares; the director asks the store and writes nothing to it. **Not built:** nothing reconciles the stored memberships toward Keycloak, so a statement that never arrives is repaired only by the next one about the same person; and the check at start that the defaults git implies are the ones the store holds does not exist. **An aim since 2026-10-09, not something the platform does:** no code revokes a person's sessions when their groups change, and no realm setting disables offline tokens (an app's client is left the optional scope `offline_access`). **Against the decision:** the operator still holds Keycloak's master administrator credential (defect 15) |
| AD-13 | The edge is the only session authority | **Holds with the one exception the entry names, written into it after it was built (§10.7); and one thing contradicts it.** The exception: the sign-in sidecar (the App Store app's sign-in to a store is gone, AD-17). An approved entry that keeps the app's own `Authorization` header is no exception to it: the session is still the edge's and still required. **Contradicts:** the entry says no component downstream holds a client secret; the operator copies the edge's client secret into every tenant namespace that has a signed-in component (decision 33), and the bouncer holds a client secret per tenant realm for token exchange (decision 53) |
| AD-14 | Catalogue sources on the Cluster claim; a profile reaches a cluster only at a verified digest | **Holds, with the exception the entry names**: the installer places the Operations Console's profile at install, at the digest its catalogue's index lists or a pin, written as the director writes it. The operator compares a Component it places by default with the bundle recorded with its profile since `23612810` (2026-10-09); no second place states that digest (defect 11). The admin console's backend still has a route that relays an install, with its screen switched off (§8.11) |
| AD-15 | Multi-language is a core requirement | **Partly.** Desktop, console and sign-in pages are translated. A component's `description` and a store listing's text are single strings; the desktop has no check for a missing translation in CI |

## 2. Built, and never run on a cluster

Most of what was built since M1 is in this part. Each item has tests; none has
been seen working on a cluster. Until it has, it is not done.

### Signing in and the front door

- The front door in its present order: the Gateway signs a person in first and
  asks the bouncer second, on Envoy Gateway 1.9.2 and Keycloak 26.8.0.
- The session's cookies and tokens stopping at the edge.
- The sign-in sidecar, for an app that supports neither OIDC nor SAML, and the
  App Admin role it reads from the realm's signed answer. Three apps declare
  it.
- A mailbox opened with a sign-in token, for an app that declared it.
- A sign-out told to the apps inside the cluster, at an address the platform
  builds from the entry's own Service: Nextcloud (`nextcloud-base-ce`) and
  XWiki through their own client, Docmost and OpenProject through the sign-in
  sidecar. What it does not reach is defect 2.
- The bouncer's rights check for a component that holds a key for it.

### Addresses and publishing

- The two tenancy modes, and the user tenant of a single-tenancy cluster
  created by the installer after the handover (`E-04`).
- Address names an app may not take, refused by the operator and the director.
- A tenant bound to a domain of its own with the command line, and the
  director refusing that for any tenant of a single-tenancy cluster and for
  the platform tenant on every cluster.
- What a tenant's apps ask to publish as a read; approval, review and
  withdrawal on the command line and in the administration console.
- A website on the main address of a single-tenancy cluster, with the
  approver's acknowledgement.
- Who approves a public address and egress: perimeter approvers named on the
  Cluster claim reaching the rights store and leaving it again, on removal
  and at `expiresAt`, and the cluster's administrator approving in a tenant
  its cluster operates.
- The publishing proxy's limits and filters, and the limit on sign-in posts at
  the Gateway.
- A public entry that passes its callers' credential to the app (`authMode:
  app`), with its lower limit. Tested against the proxy's own image; no app
  in the catalogue declares one yet.
- An entry behind sign-in that keeps the app's own `Authorization` header
  (`clientAuthorization: app`) once approved. The policy is validated against
  the pinned Envoy Gateway definitions and the bouncer's decisions are
  tested; no browser has been through it on a cluster.

### Network

- The kernel namespaces' rules for incoming connections. They are off by
  default (`KERNEL_NETWORK_POLICIES`) for exactly this reason: turn them on
  after a successful install with `./install.sh --only A-01,B-01`.
- The rules on the shared stores, the kernel's PostgreSQL, the mail servers and
  the model gateway's namespace.
- A contract between two apps carrying traffic once it is granted.

### Mail

- The mail proxy in `system-mail-dmz` and the client's address passed on to the
  servers. Its test runs the rendered proxy and servers in local containers.
- Every mail object written into the namespace of the server that reads it,
  and a tenant's mail held as not ready (`PostfixMapMissing`) until Postfix's
  map names its domain.
- Mail to a removed person's address refused at `RCPT` while the record of
  the removal stands and nobody holds the address. Shown against the rendered
  Postfix in local containers.

### Catalogue and apps

- Catalogues added per cluster and per tenant, read when needed; nothing copied
  into a cluster ahead of an install.
- A profile bundle with its companions under one digest, and the operator's
  check of all of it before rollout.
- The same check for a Component the operator places by default, against the
  bundle recorded with its profile.
- What a newer build of an app left behind, listed and removed.
- The App Store app placed on tenants where the cluster offers a store.
- The model gateway as a requirement an app declares, with its image named by
  tag and digest.
- A store, a role and a policy in OpenBao per tenant, and the kernel's store
  refused from a tenant's namespace. The policies are run against a local
  OpenBao and the store each `ExternalSecret` names is held by a test of the
  rendered Compositions; that ESO refuses the kernel's store from a tenant's
  namespace is read from ESO's code at the pinned version, not seen.
- A key a profile declares under `secrets.derived`, made by the cluster's
  secret mode and kept in the vault.

### Data

- Backup bundles of schema version 3: the data of uninstalled apps that was
  kept, the platform desktop's database, mailboxes, and the rights that follow
  from nothing else.
- A restore into a new tenant (`TenantRestore.spec.intoNewTenant`), and an
  import that gives the tenant its own names.
- Backup bundles of schema version 4: each app's own secrets in the bundle,
  set again by a restore and by an import at the app's own paths, the app
  handed them and restarted; and an app's own secrets drawn at random under
  `secretMode: random`. The operator's part is proven in local containers,
  against the object store and OpenBao (SeaweedFS since 2026-10-15). That External Secrets fills the app's Secret
  again when told to, and that the Helm provider upgrades a release whose
  value changed, is played by the tests and has not been seen.
- A tenant's deletion removing its mailboxes, and failing loudly.
- A removed person's mailbox archived or deleted, as whoever removes the
  person chooses at that moment; archived mailboxes in backups, listed, and
  deletable. Proven in local containers, not on a cluster.

### Installer and director

- Argo CD given a private deployment repository's credential before the claim
  that would supply it.
- `--dry-run` and `--validate` changing nothing; the break-glass key found by
  its recorded id; the OpenBao command line fetched and checked.
- The default catalogue chosen by the ref the platform is installed from.
- Crossplane installed without its activate-everything policy, and only the
  25 provider resource types in use activated (`A-04`, `B-05`): the four
  providers then bring 42 CRDs in place of the 391 they ship (the 25, the ten
  configuration kinds of the vault and Keycloak providers, and all seven of
  provider-kubernetes and provider-helm, which cannot be narrowed). Not shown
  on a cluster: that the two providers come up healthy with only those types, that
  `B-05`'s wait for them ends, how a composed resource of a type that is not
  activated shows on its composite, and `CROSSPLANE_ACTIVATE_ALL=true` as the
  way back on a cluster that was installed without it.
- The default profile placed only at the digest its catalogue's index lists or
  a pin, written as the director writes it.
- The installer refusing when the break-glass key `keys.env` records is not on
  the machine, and `--rotate-break-glass-key` -- in particular that Argo CD
  accepts the repository again once `B-01` and `B-10` have run with the new
  id.
- The director's clean stop and its write retry bounded by time.
- The installer stopping when its settings, its deployments checkout and the
  cluster `kubectl` reaches describe different clusters
  ([install-reference.md §4](../install-reference.md), *One run, one
  cluster*). This is shown against stand-ins only (`test-one-cluster`).

- The Models tab of the administration console and `kubectl gentian models`:
  the director's `GET` and `PUT /v1/clusters/{c}/models`, the commit of the
  claim, the gateway restarting with the new list after the sync, and the
  flags on models that cannot answer.
- A provider's credential declared from the claim: the Cluster composition
  composing `llm-provider-<name>` and its probe, taking over the one the
  installer applied for a provider on a cluster installed before, and removing
  them with the provider. The gateway leaving out a provider that names
  another's token property.
- The gateway's console switched on and off from the Models tab.

### Added on 2026-10-11

Built between 2026-10-06 and 2026-10-10 and missing from this section
until now. Each is *confirmed in code* as existing; none has run on a
cluster.

- Keycloak 26.8.0 with provider-keycloak v2.19.0 creating realms and
  clients (§11.1).
- A token of the person that is valid at one app alone (`exchangeToken`),
  and the bouncer's client secret per tenant realm that it needs
  (decision 53).
- A component vouching for a person, on the registrar's routes that a
  component's own key opens (§8.4).
- A pod-security waiver held until it is granted on the install, and a
  cluster role bound from the platform's set, which is empty
  (decision 23).
- Integrations between two apps starting closed, and a binding becoming
  ready once granted (decision 22).
- The check that the cluster's definitions are as new as the software,
  and the director refusing a commit on its word (decision 40).
- The platform tenant's admin console at `admin.platform.<kernel domain>`
  with its own wildcard certificate (decision 41).
- Backup and restore units running beside what they read, in kernel
  namespaces (decision 45).
- A provider's credential requirement composed in `kernel-control` from
  the claim (decision 19).
- The director telling the operator of each push, and the operator
  asking Argo CD to read the deployments repository at once
  (`refresh-deployments`, operator-split-plan.md §4.2).
- The user tenant of a single-tenancy cluster on an install nobody
  attends: step `E-04` runs after the platform administrator has signed
  in once, so such an install ends before it and the step is run
  afterwards with `./install.sh --only E-04`. *Reasoned from the step's
  own description, not confirmed.*

Carried over from before, and still not shown:

- Whether Argo CD accepts the director's signatures on a cluster after a
  rebuild from the recovery kit.
- The Keycloak event listener recording administrative events with the request
  id, so that the registrar's record and Keycloak's join. It projects group
  membership and drops the rest.
- Opening an app without a second sign-in (M4.6), which changed since it last
  worked.

### Added on 2026-10-13

- The registrar keeping a tenant administrator's invitation until the
  tenant is ready and mailing it itself (iam.md §1.4).

### Added on 2026-10-15

- SeaweedFS 4.48 as the platform's object store, in place of MinIO: one pod
  (master, volume server, filer, S3 gateway) from its own chart, vendored in
  `charts/infra/seaweedfs` and served from the platform's chart index; the
  administrator's credential at `gentian-os/kernel/storage/s3`; `s3-admin`
  in place of `minio-admin`; the master's and the filer's ports admitted only
  to the Jobs that run `weed shell`. An app's bucket and the identity it
  reads it with (one per bucket, named after it, never Admin) made and
  destroyed with `weed shell`. Every Job that moves a tenant's objects on
  rclone, in the platform's storage tools image. New installs only; nothing
  migrates. Proven in local containers against the pinned server
  (`make test-object-storage-lab`): the bucket and destroy Jobs, a capture
  and a restore of a bucket and of a volume, the upload and its checksum,
  the manifest, a bundle's deletion, and that one app's key cannot read,
  write or list another app's bucket, cannot delete its own, and is refused
  once its bucket is purged. *Needs a cluster:* the Release under
  provider-helm, the pod under the Kyverno baseline, the NetworkPolicy under
  Calico (the master's address names a Service, the filer is reached at the
  pod), and an app writing to its bucket.
- Four follow-ups to it, decided by the owner and built on `develop`, never
  run on a cluster: (2) an app's S3 key pair reaches the Job that makes its
  identity in a Secret of the Job's name that the Job owns, never in a Job
  spec, the provisioning ConfigMap or a composed Object; (3) the object
  store's administrator is `s3-admin`, its key derived under `s3` (new
  installs); (4) export and restore pods leave the cluster only to the
  external destination of their own run, on its port, at its address when
  the endpoint names one, and not at all for the platform's storage; (5) an
  external destination's `addressingStyle` (`path`, the default, or
  `virtualHost`), recorded on each bundle and used by the Jobs (rclone's
  `force_path_style`), the operator's reads (minio-go's bucket lookup), the
  director, `kubectl gentian tenants import` and `recovery.sh`. Proven in
  local containers: (2) and (3) by `make test-object-storage-lab`; (5) the
  Jobs' remote against SeaweedFS with `-s3.domainName`, which serves both
  styles. *Needs a cluster:* the bucket Job's pod waiting for its Secret and
  starting once the operator has made it, the Secret collected with the Job,
  and the per-run NetworkPolicy under Calico.
- Two more, decided by the owner and built on `develop`, never run on a
  cluster. (6) The internal CA `gentian-ca` on every cluster, in every issuer
  mode (on `private-ca` it stays the person's CA), its key generated once in
  the vault's PKI engine (`pki-internal`) and never out of it, cert-manager
  asking the vault to sign through a role that admits the store's names
  only (and the kernel domain on `self-signed`); the object store's gRPC mutual TLS from it (one
  certificate for the pod, one client certificate for the `weed shell`
  Jobs, accepted names set per component); the volume server and the
  filer's HTTP port taking only signed tokens, the keys random at
  `gentian-os/kernel/storage/s3/jwt`. (7) An app's PostgreSQL, MariaDB and
  cache passwords, and the OIDC client secret the Keycloak client Jobs set,
  reach their provisioning Jobs in Secrets the Jobs own.
  Proven in local containers (`make test-object-storage-lab`): a weed client
  without a certificate, with one of another CA, or with one of the CA under
  another name, is refused by the master and the filer; the Jobs' certificate
  works; a volume write without a token, or with one of another key, is
  refused and the master's is taken; volume and filer reads without a token
  are refused; a replaced CA and certificates are taken up without a
  restart; every earlier case still passes. A-09, B-08 and B-01 by
  `make test-internal-ca` against stubs; the engine, the role's refusals,
  the sign-only policy, the import of a cert-manager-made CA and seed-once
  against a local OpenBao 2.5.0; the vault taking a CA-signed certificate on
  `SIGHUP` without sealing, likewise. *Needs a cluster:* cert-manager's
  Kubernetes login at the vault (token audience, the token-request Role),
  its NetworkPolicy path, gentian-ca Ready after B-08 and C-01 on each issuer
  mode, the store's certificates issued, a self-signed cluster's existing CA
  imported, and an existing cluster's vault moving to `openbao-ca` with its
  stores and the custodian following. *Resolved on `develop`:* the sidecar
  `tls-reload` signals the vault when `openbao-tls` renews; a vault pod
  started before it has none until its next restart (operations.md §8.2). *Not
  migrated:* a cluster that ran the KV-based CA (72c3807a, on no live
  cluster) keeps that KV path and gets a new CA from the engine.
  *Resolved on `develop`:* a Certificate or CertificateRequest naming
  `gentian-ca`, or an Ingress asking the ingress-shim for it, is refused
  outside the kernel and system namespaces (`gentian-restrict-internal-ca`),
  and the tenant AppProject no longer allows ClusterIssuers. Shown with the
  Kyverno CLI. *Needs a cluster:* the policy Ready, a tenant's Certificate
  for `gentian-ca` refused, the store's certificates still issued, and the
  live AppProject without ClusterIssuer after the claim reconciles.
  *Resolved on `develop`:* the tenant AppProject deploys into `tenant-*`
  only, no longer the Argo CD namespace; and a namespace's tier label is set
  only by the installer, provider-kubernetes, the operator and
  `system:masters` (`gentian-namespace-tier`, security.md §12.4). Shown
  against an API server. *Needs a cluster:* a fresh install and a re-run
  passing with the guard in place, tenants' and DMZ namespaces still
  composed, and the live AppProject without the Argo CD namespace.
  *Resolved on `develop`:* a tenant's chart is installed as
  `provider-helm-tenant-apps` through the ProviderConfig `tenant-apps`,
  with rights in tenant namespaces only (security.md §12.4). Shown against
  an API server. *Open:* a profile bundle's own Composition (Element's,
  Odoo's) still names the kernel's ProviderConfig, and nothing refuses it.
  *Needs a cluster:* provider-helm honouring the kubeconfig's impersonation,
  every catalogue app installing and upgrading under it, an existing
  release taken over without a reinstall, and a tenant deleted with its
  apps.
  *Resolved on `develop`:* a namespace's `gentianos.io/function` is set only
  by the installer, provider-kubernetes and `system:masters`, and its
  `mac-waiver.gentianos.io/*` labels only by the operator and
  `system:masters` (`gentian-namespace-tier`). Shown against an API server.
  *Needs a cluster:* a re-run of A-01, the system namespaces composed, and a
  waiver granted and withdrawn with the guard in place.

### Added on 2026-10-16

- The bundle signing key is read by the operator from the vault and denied
  to `eso-read`; the operator, provider-vault and the installer verify the
  vault against `openbao-ca`; the vault's pod reloads its certificate by
  itself (`tls-reload`). Shown by unit tests and stubs. *Needs a cluster:*
  provider-vault taking `VAULT_CACERT` as its CA (the Terraform provider's
  fallback when the ProviderConfig names no `ca_cert_file`), `bao` honouring
  `BAO_TLS_SERVER_NAME` over a ClusterIP and a port-forward, the operator's
  vault login against the mounted copy, and the sidecar's signal reaching
  `bao` in the shared process namespace.
- An existing cluster keeps the Secret `gentian-os-bundle-signing-key`
  after its ExternalSecret is gone (it was `deletionPolicy: Retain`); delete
  it by hand.
- `gentian_service_addr` still probes the vault's address with `curl -k`
  (reachability only, no token sent).
- The custodian still reads the vault's CA through the API from
  `openbao-tls` in the secrets namespace rather than from the
  `openbao-ca-bundle` copy in its own.
- `busybox:1.36` is pinned by digest for `tls-reload` only; the image is not
  in `lint-image-pins`' `DIGEST_REQUIRED`, since its other uses are by tag.
- The registrar may now `get` Argo CD Applications in the gitops namespace
  (to answer 424 for a tenant whose sync failed); the owner kept the grant on 2026-10-16.
  The invitation state it reports still says `pending` for such a tenant.

## 3. Known defects, open

Defects 1 to 28 are carried from the list as it stood on 2026-10-10.
Defects 6, 7, 14, 15, 21, 22 and 25 were opened in the code again on
2026-10-11 and stand as written. The others were not checked again that
day. What the two checks of 2026-10-11 found is in sections 7, 8 and 9
and is not repeated in this table. Defects 29 and 30 were added on 2026-10-11
from the repair of the catalogue's sign-in test and were read in the code
at `be3cbea3` of `develop`. Defect 31 was added on 2026-10-13 from a mail
fault on an installed cluster and reproduced in local containers. Defect 32
was added on 2026-10-15.

| | Defect | Where it is described |
| --- | --- | --- |
| 1 | **An app's own client and its token: closed in two parts, open in a third.** A browser page that sends the app's own token keeps it on an entry that declares `clientAuthorization: app`, once the tenant's perimeter approver approved it; without the declaration or the approval the Gateway still removes the header. A client with no browser session (sync client, mobile app, script, webhook) reaches the app through a public entry of `authMode: app` with a credential the app issued. **Still open**: a client that needs the app's cookies on a public address does not work, which is what Synapse's sign-in needs, so Element is not served yet; no bearer token is verified at the edge; and the catalogue's apps have to declare the entries before any of it helps them | [routing.md §4.1, §7](../design/routing.md) |
| 2 | **Signing out does not reach every app.** The session ends at the Gateway when its access token runs out, and the realm now tells an app inside the cluster where the app can be told (built 2026-10-09): Nextcloud (`nextcloud-base-ce`) and XWiki through their own client, Docmost and OpenProject through the sign-in sidecar. Still open: Activepieces 0.28.0 has nothing to end a session with (an hour at most); Open WebUI needs a switch and Redis (on the roadmap, item 2.27); Element needs Synapse's switch and its own Composition to register an address; `nextcloud-base-od` declares it and has not been run; Mathesar and Odoo cannot be told at all. The realm tells once and not when a session only runs out, so an app's own session lifetime remains the bound | AD-13, [iam.md §1.12](../design/iam.md), [security.md §2.12, §2.15](../design/security.md) |
| 3 | **One key opens the rights store.** Six programs present the same OpenFGA key, and it can write | [operator-split-plan.md §6](operator-split-plan.md) |
| 4 | **Closed in the code on 2026-10-09, not yet shown on a cluster: nobody could publish by default.** The cluster's administrator approves in every tenant its cluster operates, and names perimeter approvers on the Cluster claim (AD-16) | §1 and §2, AD-6, decision 2 |
| 5 | **The publishing proxy checks no caller.** No entry claims otherwise any more: the modes that named a check are refused by the schema, and `authMode: app` hands the credential to the app. What stays open is the consequence AD-1 now calls accepted (§10.4): the platform does not know the caller on such a path, and a removed person's app password or token works until the app revokes it | [security.md §2.14](../design/security.md), AD-1, [roadmap.md](../roadmap.md) 1.34 |
| 6 | **Identity headers are not signed.** An app and a sign-in sidecar believe them; network rules are what keeps another pod from sending its own | [operator-split-plan.md §6](operator-split-plan.md) |
| 7 | **No kernel namespace restricts outgoing connections** | [security.md §2.13](../design/security.md) |
| 8 | **An import carries the source's approvals.** The new tenant's manifest is written from the bundle, so the privileges granted and the entries approved in the exported tenant — public addresses, with their kind, and kept `Authorization` headers — arrive approved, and nobody on the importing cluster approved them | [data-lifecycle.md](../design/data-lifecycle.md) |
| 9 | **A deleted tenant leaves entries behind**: its rights and memberships in the rights store, which a later tenant of the same name would inherit, and its keys in the shared cache. Not now: on the roadmap, item 1.38 | [data-lifecycle.md](../design/data-lifecycle.md), [roadmap.md](../roadmap.md) |
| 10 | **Mail to a removed person's address is accepted again once the record of the removal is gone.** Closed in the code on 2026-10-09 for as long as the record stands, and shown against Postfix in local containers, not on a cluster: the address is refused at `RCPT`, archived or deleted. The record of a deleted mailbox is removed after 30 days, and under the recipient policy `catchall` (the default) the address is accepted from then like any other nobody owns. A person removed before this was built is not known. An IMAP session open at the removal is not ended | [mail.md §5c](../design/mail.md) |
| 11 | **A default Component's digest is stated only on its profile.** Closed in part on 2026-10-09: the operator now compares the profile, and what its bundle brings, with the bundle recorded with it before it rolls out a Component it placed by default, and holds the Component when they differ. Open: no second place states that digest, as the tenant's manifest does for an install, so a profile replaced together with its bundle passes; on the roadmap, item 1.39. The digest the installer uses comes from the catalogue's own unsigned index unless the person installing pins one | AD-14, [security.md §2.11](../design/security.md), [roadmap.md](../roadmap.md) |
| 12 | **The model gateway's console switch has not run on a cluster.** The console is off unless the claim says `llm.console.enabled: true`, so the defect as it stood -- no way to serve models without the console -- is closed in the code. Switched on, the route and the tile came up on a cluster and the sign-in was refused, because the zone's client did not admit `llm.<kernel>` as a callback; the tenant composition now lists it when the setting is on. Not yet seen: the console working behind the edge (its own sign-in and its `Authorization` header are held by tests of the rendered policy only), and an upgraded cluster losing the route and the edge's rule when switched off | §1, AD-9, decision 6; [llms.md](../design/llms.md) |
| 13 | **A cluster installed before 2026-10-09 keeps every Crossplane resource type** — the 391 CRDs of the four providers, and provider-http's until `B-05` removes that provider. A fresh install creates 42: the 25 types the platform uses and the 17 that cannot be left out (`crossplane/providers/activation.yaml`, held by `make lint-provider-activation`); Crossplane never deactivates a type, so nothing narrows an existing cluster. The narrowed list has not run on a cluster yet (§2) | [install-reference.md §4](../install-reference.md) |
| 14 | **Two images float.** `vllm/vllm-openai` falls back to `latest` when the claim names no tag, and the model gateway's cache runs `redis:alpine`. `lint-image-pins` lists the first as known and does not look at the second | `kernel/services/llm/` |
| 15 | **The operator holds Keycloak's master administrator credential.** It reads the `keycloak-admin` Secret and hands it to the Jobs that configure realms, clients and groups. The process that writes the rights store can therefore change any identity as well | §1, AD-12, §8.2; `identity_reconciler.go` |
| 16 | **Argo CD's repository credential can push** | [operator-split-plan.md §6](operator-split-plan.md) |
| 17 | **A change of a person's groups does not end their sessions**, and offline tokens are not disabled. AD-12 states both as an aim since 2026-10-09; no code does either. An app that read groups from its own token keeps them until that token or the app's session ends | §1, AD-12 |
| 18 | **The development catalogue is not republished until the released branch can be built.** The catalogue repository's publishing job builds the released and the development catalogue together, and fails while the released branch lacks the catalogue build script. A profile changed on the development branch therefore does not reach `…/gentian-apps/develop`, and a cluster installed from a branch keeps reading the last catalogue that was published | [custom-catalogues.md §3](../custom-catalogues.md) |
| 19 | **The XWiki profile on the development catalogue cannot be installed.** It names a chart (`xwiki-ce` in the upstream chart repository, which serves `xwiki`) and an image that do not exist. Sign-out reaching XWiki (defect 2, §2) is therefore held by tests of the profile only | the catalogue repository, `profiles/xwiki/xwiki-ce` |
| 20 | **`backchannelLogoutUrl` remains in profiles outside this repository.** The schema refuses the field (`OIDCClientSpec`, `api/v1alpha1/profile_parts.go`): a bundle that still carries it is not admitted, and the app is not installed or updated from it, until the profile declares `backchannelLogout` (exposure and path) instead. The catalogue's released branch and some extension profiles still carry the old field | [app-customization.md](../app-customization.md) §2, [iam.md §1.12](../design/iam.md) |
| 21 | **Removing a tenant's file reports "unchanged" and leaves the file when the tenant has no `kustomization.yaml`.** `writeTenantFileLocked` (`internal/director/gitops/backup.go`) decides that there is nothing to remove from the read error of the kustomization, not from that of the file. Unbinding a tenant's domain (`tenants domain <t> --remove`) in such a tenant directory answers `unchanged` and `domain.yaml` stays | `internal/director/gitops/backup.go` |
| 22 | **The smoke check of a cluster's own mail cannot pass.** `make verify-kernel-services` (and `e2e-p5-keycloak-dovecot`) run `crossplane/tests/e2e/scripts/e2e-verify-kernel-services.sh`, which calls `verify_keycloak_installation`; that function was deleted from `scripts/lib/verify-kernel-services.sh`, so the script always counts one error. The Dovecot check it also calls now looks in `system-mail`, and has not been run | `scripts/lib/verify-kernel-services.sh` |
| 23 | **A model the cluster serves itself is offered and not served.** Closed in the code on 2026-10-09 for what the claim declares, not yet shown on a cluster: the gateway's model list is its configuration file, written by its chart from `spec.llm.providers` and `spec.llm.instances` of the claim the LLM ApplicationSet hands it. The vLLM instance behind an entry of `instances` is run by the gateway's chart from the same claim (pinned image, admission baseline, `llm-vllm-ingress`); what stays open for it is the Hugging Face token for gated models and a cluster that has served one. For a provider's model the tab reads the custodian's list of credentials and flags a missing token; nothing on the cluster does, nothing probes the provider, and a wrong token still shows as a failing call. A provider's credential requirement is declared from the claim by the Cluster composition | [llms.md §5, §6](../design/llms.md), [roadmap.md §2.28](../roadmap.md) |
| 24 | **Closed in the code on 2026-10-10, not yet shown on a cluster: a granted cluster role created nothing.** A profile asks for a cluster role by name; the operator binds it where the platform's set has it, the allowlist permits it for the profile and it was granted on the install. The set is empty and the operator has no permission on RBAC objects, so the path that binds has run in tests only. The first role brings those permissions with it. [roles-and-authorizations.md](roles-and-authorizations.md) §2 still states that no workload's ServiceAccount is bound to any role, which the first role of the set ends | [security.md §3.4](../design/security.md) |
| 25 | **Closed in the code on 2026-10-11, not yet shown on a cluster: the password policy was not in git.** From `78039042` (2026-10-10) to `24123aef` (2026-10-11) the registrar's action was the one way to set it and the director committed no password block. That is reversed (`24123aef`, `5fd8b15f`, `eec19bad`): the director commits the password block again with sessions and lockout (`security-policy.yaml`, `can_set_policy`), the tenant Composition applies it, the console's Security screen writes through the director, and the registrar's action is gone. The Composition now declares the realm's `passwordPolicy` always, empty when the tenant states no password rules, so the provider has nothing to copy from the live realm and hold (the note at `smtpServer` in `tenant-default.yaml`); a policy taken out of git is taken out of the realm, and one set by hand at Keycloak is replaced by what git says. What is left: that an emptied policy clears the realm's has not been run on a cluster; the form states seven rules and a clause outside them cannot be declared; and the registrar's client is no longer granted `manage-realm`, but the grant only adds, so a realm made before keeps the role until it is taken away at Keycloak | [admin-console.md §4.5](../design/admin-console.md) |
| 26 | **Closed in the code on 2026-10-10, not yet shown on a cluster: the operator granted integrations by itself, and a waiver needed no grant.** The operator writes no `AppGrant` and cannot (its role has neither create nor patch); a binding reports what was granted. A pod-security waiver needs the allowlist and the grant on the install. Grants the operator wrote before stay in the cluster | [security.md §3.4](../design/security.md) |
| 27 | **Closed in the catalogue on 2026-10-10, not yet shown on a cluster: Element and Odoo could not be given their credentials.** Each brings a Composition of its own that wrote the app's `ExternalSecret` into the tenant's namespace naming the kernel's store, `openbao`, which ESO refuses from there. Both now name `openbao-tenant-<tenant>`, as `app-default` does, in the development catalogue (`element-ce`, `odoo-base-ce`), and the copies in `internal/profilebundle/testdata/bundles` are the published ones; every key they read is under the tenant's `apps/`. Held by a render of the two Compositions only. The released catalogue's bundles of the two carry the profile and no Composition. **Still open**: three extension profiles kept in another repository bring Compositions that still name `openbao` from the tenant's namespace (`od-element`, `od-openproject`, `od-ox-appsuite`), so those apps get no credentials Secret. Element's Composition also held a database Job that signed in to OpenBao as a role `app-init` that nothing creates, to read the database superuser's credential and the master password; it was never rendered on a cluster installed from this repository (it waited for `cnpg.host` in the cluster's configuration, which `cluster-default` does not write) and is removed from the bundle: Element's database and its credential are the operator's to make, as for any app, which has not been run for Element on a cluster | [security.md §5, §6.1](../design/security.md), the catalogue repository |
| 28 | **Closed in the code on 2026-10-10, not yet shown on a cluster: an `ExternalSecret` in the platform tenant's namespace could read any kernel path.** The kernel's store admitted `tenant-platform` by name for the desktop's database password. That password is now at `gentian-os/tenants/platform/apps/shell/database`, read through `openbao-tenant-platform`, and the kernel's store admits no tenant's namespace. The role was already the desktop's own, owning only its database. On a cluster installed before, the path appears with the installer's next run; until then both `ExternalSecret`s report an error and keep the Secret they made. Decision 27 | [security.md §5](../design/security.md) |
| 29 | **The sign-in sidecar names a person to its app by Keycloak's email field, which holds the recovery address.** The sidecar's SAML client is composed with `forceNameIdFormat: true` and `nameIdFormat: email` (`crossplane/compositions/app-default.yaml`), and the comment there says the address is the one thing the sidecar reads. The registrar creates a person with the workspace address as the username and, when the invitation gave a different address, that other address in the email field (`internal/registrar/identity/people.go`, `Invite`): the field is the private address a password reset is mailed to. For such a person the app beside a sidecar (Docmost, Activepieces, OpenProject) is told the recovery address as who they are, or the sidecar refuses the sign-in because the address differs from the person the front door admitted. A person whose two addresses are the same is not affected, and the sidecar's own test has only such people. The realm's shared `email` scope and the OIDC packs of Nextcloud, XWiki and Element were pointed at the username for this reason; the SAML path was not. *Confirmed in code* for the two settings; what an app then shows, or whether the sign-in is refused, *needs a cluster* | [iam.md §1.11](../design/iam.md), [security.md §2.12](../design/security.md) |
| 30 | **Nothing found sets the `uid` attribute that two packs read as the account name.** The OIDC packs of Nextcloud (both editions) and XWiki in the catalogue repository (`develop`, `03fcda7`) carry a mapper that writes the claim `gentian_username` (`opendesk_username` in one edition) from the user attribute `uid`. The registrar's invitation writes the username, the email field, the names, the groups and, where the addresses differ, the invitation address (`internal/registrar/identity/people.go`, `Invite`); it writes no `uid`. The tenant's Composition declares the attribute in the realm's user profile and writes no value; the only mappers in this repository that carry `uid` belong to the kernel identity provider in tenant realms, which is switched off. If nothing fills it, a person invited on the platform reaches these apps with an empty account-name claim: the app then falls back to another claim, or refuses the sign-in. *Confirmed in code* that the registrar does not set it; that nothing else does is *reasoned, not confirmed* (no writer was found); the effect in Nextcloud and XWiki *needs a cluster* | the catalogue repository's `profiles/nextcloud/base/*/oidc-catalog.yaml`, `profiles/xwiki/xwiki-ce/oidc-catalog.yaml` |
| 31 | **A realm on a cluster with its own mail had a mail server without its password. Closed in the code on 2026-10-13 and shown in local containers, not yet on a cluster.** Keycloak answered an invitation with `failed to connect, no password specified?`. Two causes, both reproduced against Keycloak 26.8.0 with provider-keycloak v2.19.0. (a) The mail mode was renamed from `kernel` to `system` and two comparisons kept the old word: the installer gave the kernel realm no mail server and wrote no `keycloak-smtp-credentials`, so the operator built no mail-server Job for any tenant, and the Job's script would have handed a tenant realm the kernel realm's login. (b) That left the tenant's Composition as the only writer, and it names the password by reference to a Secret the operator wrote only after the realm existed: the provider wrote authentication on, the login and no password, and reported `Synced`. It puts that right at its next poll only if it has not restarted since; after a restart it never does, because it cannot read a password back. Now: both comparisons say `system`, the Job runs again over existing realms (version 5), the login is minted before the realm is declared, and step `D-03` reports work to do on a `system` cluster without the Secret. **An installed cluster needs** the operator's new image, the definitions (`./install.sh --only B-06`) and, with its own mail, `./install.sh --only D-03`. **Closed in the code on 2026-10-13 beside it, shown with the provider in local containers:** with a relay the Composition declared no mail server, the provider copied the live one into the resource without a password reference and wrote it back without the password two seconds after the Job; the Composition now declares the block in both modes from `spec.realmMail`, which the operator states once the password's Secret exists (Job version 6). A tenant the cluster's own mail mints no login for is declared no mail server and gets no Job. `loginWithEmailAllowed` is declared, so a realm the realm Job creates first is no longer refused its updates. The operator's image and the definitions of step `B-06` go together: the new Composition under an operator that states no `realmMail` declares no mail server. **Not established:** what the kernel realm's mail server held on the cluster when it failed the same way; by the code it had none, which fails with another message, and this is accepted as unexplained for now. **Not built:** a realm that had a mail server and is no longer stated one keeps the block the provider copied. | [mail.md §5d](../design/mail.md), `scripts/lib/portal-login-bootstrap.sh`, `internal/controller/keycloak_tenant_smtp.go`, `internal/controller/mail_apppassword.go`, `make test-realm-smtp-lab` |
| 32 | **Every restore failed on its first step. Closed in the code on 2026-10-15, shown in local containers, not yet on a cluster.** Each restore Job's fetch step downloaded MinIO's client from `dl.min.io` when it ran; the address stopped answering when MinIO withdrew the client, and every restore of every kind -- database, bucket, volume, realm, mailboxes, desktop -- failed before it read a byte. Now the fetch, like every Job that moves a tenant's objects, runs rclone and age from the platform's storage tools image, at the operator's own tag, and downloads nothing. A bundle written before keeps restoring: the checksum beside an artefact has the format it had | `internal/backup/restore_jobs.go`, `kernel/images/storage-tools`, `make test-object-storage-lab` |
| 33 | **An app's entitlement group whose profile carries `gentianos.io/keycloak-group-attributes` was composed with list values, which provider-keycloak v2.19.0 refuses (a Group's `attributes` is a map of strings). Closed in the code on 2026-10-16, not yet shown on a cluster.** Nine Odoo add-on profiles of the catalogue carry the annotation. `app-default.yaml` now joins a list with `##`, which the provider splits into Keycloak's list values; the render fixtures show strings | `crossplane/compositions/app-default.yaml` |
| 34 | **`A-02`'s `destroy()` says it keeps cert-manager without `--cluster-infra`, but `A-01` then removes the edge namespace, and cert-manager with it.** The message "Keeping cert-manager (shared cluster infrastructure)" is not what happens on an uninstall or a purge; external-dns, which the same reasoning keeps, goes the same way. Found 2026-10-16 while fixing the purge that left kernel-edge's objects behind; not changed | `scripts/steps/A-02-cert-manager.sh`, `scripts/steps/A-01-namespaces.sh` |
| 35 | **The catalogue's `odoo-base-ce` Composition holds its addons Job in a provider-kubernetes Object with `ttlSecondsAfterFinished: 600`, so the Object recreates it after every TTL and the addon install runs every ten minutes, as the platform's own Jobs did until 2026-10-19.** Not changed: it is the catalogue repository's. Found while removing the TTL from the Jobs this repository composes | the catalogue repository's `profiles/odoo/base/base-ce/composition.yaml` |
| 36 | **A changed post-install script leaves the previous run's completed pod in the tenant's namespace.** Since 2026-10-19 a changed `spec.hooks.postInstall` is a new Job under a new Object, and Crossplane removes the old Object; deleting a Job that way orphans its pods (the purge sweeps such pods for the same reason, `internal/applifecycle/purge.go`). One finished pod per change, until the app is purged. *Reasoned, not confirmed* | `crossplane/compositions/app-default.yaml` |
| 37 | **A privilege approved again on a Tenant does not reach its Component.** The director replaces a grant whose terms differ, and stamps every approval with a new `approvedAt`, so a second press of the button rewrites the Tenant's entry; since 2026-10-22 the operator keeps the approval the Component already carries (its API refuses a changed approver or `approvedAt`), so a new reason or expiry takes effect only after the grant is revoked and given again. *Confirmed in code* | `internal/director/gitops/privileges.go`, `internal/controller/component_desktop.go` (`keepStoredGrants`) |


## 4. Decisions waiting for the owner

Numbers 1 to 32 are the ones this list had; other documents cite them, so
they keep their numbers. Numbers 33 to 54 were added on 2026-10-11.

**Summary.**

| State | Decisions |
| --- | --- |
| **Open, awaiting the owner** | 8, 9, 14, 15, 29, 30, 33, 34, 35, 36, 37, 46, 49 |
| **Open in part** (one part decided, one part not) | 23, 25, 32 |
| **Built, not decided** (the owner is asked to confirm or reject) | 6, 19, 24, 38, 39, 40, 41, 42, 43, 44, 45, 47, 48, 50, 51, 52, 53, 54 |
| **Answered, and the answer needs confirming** | 7, 22 |
| **Decided** | 1, 2, 3, 4, 5, 10, 11, 12, 13, 16, 17, 18, 20, 21, 26, 27, 28, 31 |

Every open entry is written the same way: what the options are, what is in
the code today and since which commit, why it was built so where a reason
is written down, and what each answer costs. Where no reason is written in
the code or its commit, the entry says so.

1. **Whether a default Component is pinned** (AD-14). **Decided
   2026-10-09** and built: the operator compares a Component it places by
   default with the digest of the bundle recorded with its profile, at
   rollout, as it does for an install. What is left is defect 11.
   **Decided 2026-10-09**: stating that digest in a second place, on the
   Cluster claim or the Component, is not built now; on the roadmap, item
   1.39.

2. **Who may publish by default** (AD-6). **Decided.** A public address
   and egress beyond the baseline are approved by the tenant's perimeter
   approver, a role holder on the Cluster claim (`perimeterApprover`, AD-6
   and AD-16): an administrator of the tenant, held while they are one, or a
   person of the platform tenant, for every tenant. The cluster's
   administrator approves through the model's rule
   (`can_expose: perimeter_approver or admin from operated_by`). A new
   tenant, the user tenant of a single-tenancy cluster too, starts with no
   perimeter approver of its own.

3. **Sessions and offline tokens when a person's groups change** (AD-12).
   **Decided 2026-10-09**: AD-12 states both as an aim, not as something the
   platform does. Neither is built (defect 17). The operator's hold on
   Keycloak's master administrator credential is defect 15.

4. **What `basic`, `signature` and `jwt` mean on a perimeter entry** (AD-6,
   AD-1). **Decided 2026-10-09**: the schema refuses them there, with
   `bearer`, until they can be enforced; what can be delivered now is
   `authMode: app`, where the app checks. The owner's answer was short and
   its reading was stated to the owner before the build. The amendments it
   led to in AD-1 and AD-6 are in §10.4 and §10.5.

5. **A route for a client that brings its own token** (defect 1). **Decided
   2026-10-09**: both, each as an entry type the tenant's perimeter approver
   approves — behind sign-in the page's own header is kept
   (`clientAuthorization: app`), and a client without a session goes through
   the publishing proxy and is checked by the app (`authMode: app`). The
   app's cookies on a public address are left for later, with Synapse. As
   with decision 4, the reading of the answer was stated before the build.

6. **The model gateway's console** (AD-9). **Built, not decided.** This
   list recorded "decided 2026-10-09: it gets a switch". The owner's answer
   was: turn off the public console, and if possible keep the console as a
   component for administrators. That can be read two ways.
   - *Option A, what is built:* a switch on the Cluster claim,
     `llm.console.enabled`, off by default (commit `c185f871`, 2026-10-09).
     Off, `llm.<kernel domain>` has no route. On, it is the same kernel
     route as before, behind the kernel realm's session and
     `can_configure` (`internal/controller/kernel_gateway_routes.go`).
     *Confirmed in code.* It can be set from the Models tab through the
     director (decision 21).
   - *Option B, the other reading:* no route on the kernel's address at
     all, and the console offered as an app for administrators, published
     the way any component is.
   - *Why A was built:* it was the smallest change that takes the route
     away by default. No other reason is written down.
   - *Cost.* A, switched on, contradicts AD-9 ("no public route, without
     exception"), whose text was not changed; if A is confirmed AD-9 needs
     the exception written into it. B keeps AD-9 whole and needs the
     console delivered as a component, which is not built.
   What is left to run on a cluster either way is defect 12.

7. **External IMAP and submission.** **Answered, and the answer needs
   confirming.** The record of 2026-10-09 has the owner answering "accept"
   to this question; this list went on showing it as open.
   - *What is built:* ports 25, 587 and 993 are open on the mail proxy in
     `system-mail-dmz` whenever the cluster runs its own mail (commit
     `68769ef9`, 2026-10-09;
     `kernel/services/mail-edge/manifests/templates/service.yaml`).
     *Confirmed in code.*
   - *What the plan had:* [networking.md](networking.md) had 587 and 993
     closed until a tenant's approver opened them.
   - *Options:* accept the ports as open, which is what the answer says; or
     give the mail proxy a setting per port.
   - *Cost:* accepted, every cluster with its own mail offers sign-in to
     mailboxes from the internet with no approval. A setting per port is a
     chart value and a claim field that do not exist.
   If "accept" stands, this entry closes and the note in
   [networking.md](networking.md) §5 and §7 becomes the plan.

8. **When the kernel's network rules become the default.** **Open,
   awaiting the owner.** The owner asked for basic network rules on the
   kernel's namespaces. They were built and shipped switched off.
   - *In the code:* twelve policies in
     `kernel/security/network-policies/kernel-network-policies.yaml`,
     applied only when `KERNEL_NETWORK_POLICIES=true` (commit `4fa056da`,
     2026-10-09; default `false` in `charts/gentian-os/values.yaml`,
     `kernel/bootstrap/chart/values.yaml` and `install.env.template`).
     *Confirmed in code.*
   - *Why off:* they have never run on a cluster, and a wrong rule in a
     kernel namespace stops an install.
   - *Option A:* on by default now. The first test install is then also the
     first run of the rules; a failure may be the rules or anything else.
   - *Option B:* on by default once a fresh install has passed with them
     switched on by hand. This is the intent this list recorded. Until then
     everything in section 9 is not enforced on a default install.
   - *Option C:* stay off, as a setting. The diagram of
     [networking.md](networking.md) §1 then describes no default install.

9. **Should the package union admit a `composition` beside a `chart`?**
   **Open, awaiting the owner.** Before 2026-10-09 this list carried it
   twice: as a question, and in the backlog as "simplify the package union
   back to one", under decisions already taken. The rewrite of 2026-10-09
   (`4e3d2137`) kept only the question.
   - *In the code:* a profile's package may name a chart and a Composition
     together, because some entries are both.
   - *Option A:* keep both. Apps that bring a Composition of their own go
     on being rendered by it; a Composition from a catalogue decides what
     is created on the cluster (decision 42).
   - *Option B:* return to exactly one. The sign-in sidecar removed one
     reason for an app to bring its own Composition; the others (Element,
     Odoo and what the extension profiles bring) would have to be served by
     the platform's Composition first.

10. **Mail to a removed person's address** (defect 10). **Decided
    2026-10-09** and built: it is refused, whether the mailbox was archived
    or deleted. **Decided 2026-10-09**: a deleted mailbox's address is
    refused for the 30 days its record is kept, and no longer. The list is
    built by listing a domain's people at each sync; a lookup per address is
    on the roadmap, item 2.29.

11. **Whether a perimeter approver who is not a tenant administrator is let
    into the admin console.** **Decided 2026-10-09: no.** The console stays
    the tenant administrators'.

12. **Open WebUI ending a session at a sign-out.** **Decided 2026-10-09**:
    not built now; on the roadmap, item 2.27.

13. **Credentials that stay derived under `secretMode: random`.** **Decided
    2026-10-10** and built for an app's own secrets: a tenant's bundle
    carries them, encrypted by the operator, a restore sets them, and they
    follow the mode ([security.md §6.4](../design/security.md)). Decided
    with it: a restore into another tenant writes them at that tenant's own
    paths for the app, under the names its profile declares, and nowhere
    else; a restore replaces the stored value, and the app is restarted
    with it; a bundle encrypted to a requester's own key or passphrase
    carries them too; a platform administrator who reads the escrowed backup
    key can obtain them through a bundle, which is accepted and stated; all
    of an app's declared secrets travel, in `derived` mode as well; and a
    bundle is not signed for now, with its check on the roadmap, item 1.41.
    Shown in tests and against an object store and a vault in local
    containers; not run on a cluster (§2). Two kernel credentials are still
    computed from the master password in both modes: the key Keycloak's
    event listener signs with, and the kernel realm's own mail login on a
    cluster that runs its own mail server. Each needs a place in OpenBao
    that nobody has chosen, and neither is part of a tenant's bundle.
    **Decided 2026-10-10** and built: a key a profile declares under
    `secrets.derived` travels in the same artefact and is set by a restore
    at `…/apps/<app>/derived/<key>` of the tenant restored into, under the
    rules of the generated secrets. Not said when it was decided: on a
    cluster installed before, such a key changes once, and what an app
    encrypted with the old one (Open WebUI's stored tokens) cannot be read
    afterwards. *Reasoned, not confirmed.*

14. **Whether a change of `secretMode` on an installed cluster is
    refused.** **Open, awaiting the owner.**
    - *In the code:* a change converts nothing and existing credentials
      stay, with one exception: back from `random` to `derived`, the
      installer writes derived keys over `gentian-os/kernel/llm`. The mode
      is read from the cluster's configuration
      (`internal/controller/cluster_config.go`, since `afb8ddb1`,
      2026-10-09).
    - *Option A:* refuse a change. It needs a record of the mode the
      cluster was installed with, which nothing keeps.
    - *Option B:* leave it as it is and document that a change converts
      nothing. A cluster can then hold credentials of both kinds, and the
      one overwrite above remains.

15. **How the master password's presence is known without ESO reading
    it.** **Open, awaiting the owner.**
    - *In the code:* the policy `eso-read` cannot deny
      `gentian-os/kernel/internal/master-password` (and with it the salt)
      the way it denies the backup key, because the probe
      `credreq-master-password` reads that path. Denied, the probe fails,
      and the custodian and `make check-credentials` report the master
      password as missing.
    - *Narrowed 2026-10-10*, as decided: the store that reads the path is
      usable from the kernel and system namespaces and no longer from a
      tenant's (`0175f322`).
    - *Option A:* a requirement with no probe, and something else the
      installer's catalogue step, the custodian and the check take as the
      answer. ESO in the kernel's namespaces then cannot read the master
      password. What that "something else" is has to be designed.
    - *Option B:* leave it. Whoever can write an `ExternalSecret` in a
      kernel or system namespace can then turn the master password into a
      Secret there (§8.3, §8.8).
    What else still reaches the master password is decisions 27 and 28 and
    security.md §6.1.

16. **Whether the platform snapshots OpenBao.** Under `secretMode: random`
    OpenBao holds the only copy of what was drawn at random, no bundle and no
    recovery kit carries it, and nothing takes a snapshot. **Decided
    2026-10-10**: it will, and `random` is to be put through recovery drills
    with it; on the roadmap, item 2.30.

17. **What a cluster configuration without a mode means.** **Decided
    2026-10-10**, as built: a `gentian-cluster-config` that is not there yet,
    or that was written before it carried `secretMode`, reads as `derived`,
    which is what every cluster did before the operator read the mode
    (`ClusterSecretMode` in `internal/controller/cluster_config.go`).

18. **What a cluster administrator may point a model provider at.**
    **Decided 2026-10-10**, and built: a public `https` address only, by the
    rules for a catalogue's address, and a provider reads its own token
    (`<name>_api_key`) and no other's. The director refuses anything else and
    the gateway's chart offers no model of a provider that names another's
    property ([llms.md §5](../design/llms.md)).

19. **How a provider that is new to the platform gets its credential.**
    **Built, not decided.** This list recorded "decided 2026-10-10, and
    built". The owner's answer was: declare a requirement. The question
    did not say that the way in place then was already a declared
    requirement, written by hand in git, nor that the alternative creates
    objects in a kernel namespace.
    - *Option A, what was there before:* adding a provider is two edits in
      git, a requirement in `kernel/credentials/credential-requirements.yaml`
      and an entry under `spec.llm.providers` of the Cluster claim.
    - *Option B, what is built:* the Cluster composition creates a
      `CredentialRequirement` `llm-provider-<name>` and its probe
      `ExternalSecret` in `kernel-control` for every provider the claim
      names (`crossplane/compositions/cluster-default.yaml`, commit
      `1147b063`, 2026-10-10). The static entry for one provider and the
      installer's case for it were removed. *Confirmed in code.*
    - *Why B was built:* so that a provider added on the Models tab needs
      no second edit. It stays declared in git, through the director.
    - *Cost.* B: a save on the Models tab makes objects in a kernel
      namespace, which is a choice the owner keeps for themselves. A: a
      provider cannot be added from the console alone; somebody edits the
      requirements file. Going back to A means restoring the static entry
      and removing the composed block.

20. **Where a model's real health comes from.** **Decided 2026-10-10**: the
    console keeps flagging from the declared fact; a real health status is on
    the roadmap, item 2.31.

21. **Whether the Models tab also carries the gateway's console switch and
    GPU time slicing.** **Decided 2026-10-10**: in the console and through
    the director, where reasonable. The console switch is on the tab and in
    the director's route, with a warning. GPU time slicing is not: the
    claim has no field for it.

22. **What becomes of the grants the operator wrote, and whether an install
    grants anything.** **Answered, and the answer needs confirming.** The
    owner's instruction was that an administrator's grant must outrank what
    a profile declares. What was built goes further: the operator writes no
    `AppGrant` at all, so every integration starts closed (commit
    `e43f03a4`, 2026-10-10). Told this afterwards, the owner accepted it
    for now and said it may be adjusted later.
    - *As built:* an install grants nothing; an integration waits for the
      tenant's administrator. Grants an earlier operator wrote stay in the
      cluster and nothing is done about them. It matches
      [security.md §3.4](../design/security.md) ("a declaration grants
      nothing").
    - *The other option:* the operator grants what a profile declares at
      install, and never overwrites a grant an administrator narrowed.
      That is what the instruction asked for and no more. It contradicts
      security.md §3.4 as it reads today.
    - *Cost of what is built:* on a fresh install, apps that worked
      together because of the automatic grants do not, until somebody
      grants each integration. Whether a binding then becomes ready has
      not been seen (§11.2).

23. **How a cluster role is granted.** **Open in part.** **Decided
    2026-10-10** and built: the platform defines a fixed, named set in this
    repository, a profile asks by name, and the operator binds on the set,
    the allowlist and the grant (defect 24; commit `1a9578a1`). The set is
    empty. Open with the first role: which roles are in it.
    **Decided 2026-10-10**: a pod-security waiver gates. It takes effect
    only with the allowlist and a grant on the install (commit `e8218fca`;
    `internal/controller/mac_waiver_reconciler.go`, reason
    `WaiverNotGranted`). Not said when it was decided: the grant is
    approved under the cluster's `can_approve`, not by the tenant's
    administrator (`internal/director/api/privileges.go`), and apps in the
    catalogue need waivers today; Nextcloud's bundle asks for three, for
    Collabora. *Confirmed in code.*
    **Open, awaiting the owner**: which ServiceAccount the role is bound
    to.
    - *As built:* the profile names it. It is held to the component's own
      namespace and may not be `default`. This was built so and was not
      decided by the owner.
    - *Why:* the operator knows no other way to learn which account a
      chart runs its pods under.
    - *The other option:* the operator creates the account and tells the
      chart to use it. A profile can then not point a granted role at an
      account of its choosing; every chart must accept a given account
      name, and not every chart does.

24. **Whether the Tenant keeps `spec.security.password`.** **Built, not
    decided.** This list recorded "decided 2026-10-10: it keeps it". The
    owner's answer to that question was that they did not understand it.
    The field is there because the director's path needs it: the director
    commits the password block and the tenant Composition applies it,
    restored on 2026-10-11 (`24123aef`, `eec19bad`; defect 25). Removing
    the field would remove that path. Nothing is asked here unless the
    owner wants the policy somewhere else than in git.

25. **Where a backup key is made.** **Open in part.** This list recorded
    "decided 2026-10-10: in the person's browser, in the Operations
    Console, or on the command line; no server makes one". Two answers of
    the owner are on record and what was built matches neither fully.
    The first: remove key generation from the administration console and
    make sure it has been moved to the Operations Console. The second: all
    three places — browser, custodian, command line — should be possible,
    with the hope that nothing the console could already do was removed.
    - *As built:* a key is made in the browser in the Operations Console,
      which is an add-on, or on the command line with `age-keygen`. The
      administration console of this repository's platform offers neither;
      its screen was removed in the user-interface repository. The
      Operations Console submits the public half through the director and,
      on request, hands the private half to the custodian
      (`PUT /v1/backup-identity`, under `can_write_credential` on the
      caller's tenant), which writes it to
      `gentian-os/tenants/<tenant>/backup/identity` and returns it on no
      route. The cluster's own backup key is made with the recovery kit as
      before.
    - *Open, a:* whether the platform without the add-on is meant to have
      no screen that makes a key. Before the removal the button was there
      and its backend answered "not implemented", so no working function
      was lost; a tenant without the add-on makes a key on the command
      line only.
    - *Open, b:* whether the custodian should be able to make a key, as
      "all three" says. As built no server makes one. A key made by a
      server exists in that server's memory once; a key made in the browser
      never reaches a server unless the person asks for the copy.
    - *Open, c:* the default of "keep a copy in the vault" in the
      Operations Console was changed from ticked to unticked without the
      owner. Unticked, a person who loses the file loses the backups.
      Ticked, the vault holds a key that opens the tenant's backups.

26. **What a purge does with the kernel's volumes.** **Decided 2026-10-10**:
    the behaviour as built stands, and nothing is to change. A purge does
    not drain the claims of any kernel namespace before the reverse pass:
    the vault and the kernel's database stay up through it, and their claims
    go with their namespaces. The volumes are deleted in the pass after it
    (`purge_delete_volumes`, `scripts/lib/teardown.sh`), which takes the
    volumes of the kernel and system namespaces by name and every tenant's
    by prefix.

27. **How the platform tenant's desktop gets its database password.**
    **Decided 2026-10-10** and built: the password has a path of its own
    under the platform tenant,
    `gentian-os/tenants/platform/apps/shell/database`, where that tenant's
    own store reads it and the platform tenant's administrators' policy
    does too. The kernel's store no longer admits `tenant-platform`
    (defect 28; commit `7deab97e`). The desktop's role on the kernel's
    PostgreSQL was already its own and owns one database; it is not the
    server's administrator and no other service's. Two consequences were
    not put to the owner with the question: the platform tenant's
    administrators' vault policy covers that password, and on an existing
    cluster under `secretMode: random` the password is drawn again after
    the handover. *Reasoned, not confirmed.*

28. **Whether admission refuses a secret store the platform did not make.**
    **Decided 2026-10-10**: yes, and it is built in report-only mode first,
    once a test cluster is up to try it on; it refuses nothing until what it
    reports there has been read. Not built yet.
    The stores' conditions hold only for the stores that exist. A
    `ClusterSecretStore` made by anything else can sign in as `eso` and
    carries no conditions, and a chart or a catalogue's Composition can make
    one. A rule would refuse `SecretStore`, `ClusterSecretStore`,
    `ClusterExternalSecret` and `PushSecret` from anybody but the
    platform, and in a tenant's namespace an `ExternalSecret` that names
    another store than the tenant's. Not built: who "the platform" is at
    admission is five identities (provider-kubernetes, the operator, ESO's
    own controller for the objects a `ClusterExternalSecret` makes, Argo
    CD, the installer), one of which carries a generated name, and a rule
    that gets one wrong stops every app's credentials. It needs a cluster
    to be tried on, in audit first. It does not stop a chart that reads the
    master password's Kubernetes Secret directly; that needs provider-helm
    without the rights of the whole cluster (roadmap 1.16).

29. **Whether the cluster's own registry credential may reach a tenant's
    namespace.** **Open, awaiting the owner.**
    - *In the code:* a `Repository` without a tenant delivers its pull
      Secret to the namespaces its `namespaceSelector` matches. The
      credential is a kernel path, so since the tenants' own stores
      (`0175f322`, 2026-10-10) that Secret is no longer made in a tenant's
      namespace. Nothing the platform renders selects one: the default
      label, `gentianos.io/pull-secrets`, is set by nothing.
    - *Option A:* it stays so, and the selector is documented as kernel and
      system namespaces only. A tenant's app then cannot pull from the
      cluster's own registry with the cluster's credential.
    - *Option B:* the operator copies the credential into the tenant's
      namespace, as it does `registry-credentials`. A tenant's namespace
      then holds a credential of the cluster, and any chart there can
      mount it.

30. **Which Cloudflare token writes a tunnel's routes.** **Open, awaiting
    the owner.**
    - *In the code:* the installer asks for a separate tunnel API token and
      stores it (`scripts/lib/credentials.sh`,
      `kernel/credentials/credential-requirements.yaml`). Nothing hands it
      to the operator, which writes the tunnel's routes with the DNS token.
      The cluster's owner uses one Cloudflare token that carries both the
      DNS and the tunnel permission, entered at both questions, so it works
      as installed (`GETTING-STARTED.md`).
    - *Option A:* hand the separate token to the operator and use it. The
      DNS token then needs no tunnel permission, and the two can be revoked
      apart. It is a change in the operator and the composition.
    - *Option B:* remove the question for the tunnel token. One token with
      both permissions becomes the documented way; a leak of it gives both.

31. **What a manual export reads once scheduled backups have left the
    platform.** **Decided 2026-10-10**: the platform keeps a small
    "destination and key" setting, with no schedule and no retention, and a
    manual export reads it
    ([scheduled-backups-out.md](scheduled-backups-out.md) §3, option 2).

32. **Whether v0.5 upgrades a cluster that already has backup schedules.**
    **Open in part.** The question put two things at once and the answer
    was one word, "no".
    **Decided 2026-10-10**, on the reading that the "no" answered the
    first: new installs only, no upgrade support; no conversion of
    `BackupPolicy` or `TenantExportSchedule` objects is written.
    **Open, awaiting the owner**: whether and when the move itself starts.
    - *The owner's statement:* in v0.5 scheduled backups "should be out",
      and v0.5 is what is being written.
    - *In the code:* nothing has been changed for it. `BackupPolicy` and
      `TenantExportSchedule` are kinds of this repository
      (`api/v1alpha1`), and a cluster built from this tree takes scheduled
      backups with no add-on installed. *Confirmed in code that the kinds
      exist.*
    - *Option A:* start the move now, for new installs only, by the plan in
      [scheduled-backups-out.md](scheduled-backups-out.md). Until an add-on
      that schedules is installed, a cluster has manual exports only.
    - *Option B:* not yet. Then
      [sovereignty-concept.md](sovereignty-concept.md) goes on stating
      "scheduling is not the OS's" while the code schedules; the same
      document says so further down.

### Added on 2026-10-11

33. **The edge's client secret in a tenant's namespace** (AD-13, §7.1).
    **Open, awaiting the owner.**
    - *In the code:* for every component with a signed-in entry the
      operator copies the zone's edge client secret from `kernel-edge`
      into the component's namespace (`ensureZoneSecret`,
      `internal/controller/component_reconciler.go`; since `07372d08`,
      2026-09-23). For the platform tenant the zone's client is the kernel
      realm's, so that secret lies in `tenant-platform`. *Confirmed in
      code.*
    - *Why:* the Envoy Gateway policy that signs people in sits beside the
      route, in the tenant's namespace, and reads the secret from its own
      namespace. The code's comment says the copy "crosses no trust
      boundary".
    - *What the plans say:* AD-13: "no platform component downstream runs a
      code flow or holds a client secret". [networking.md](networking.md)
      §4 names "a kernel-realm client secret inside a tenant namespace" as
      the thing to avoid. The code contradicts both.
    - *Option A, follows AD-13:* keep the secret in `kernel-edge` only. The
      policies then have to live there too, or attach from there, which
      changes how routes and policies are written for every app.
    - *Option B:* accept the copy and write it into AD-13 as a third
      exception. Every `tenant-<t>` then holds the edge's identity at its
      realm, and charts are installed with rights that let them mount it
      (roadmap 1.16).

34. **Network paths a profile opens into kernel namespaces without an
    approval** (AD-5, §7.5). **Open, awaiting the owner.**
    - *In the code, confirmed:* a profile that names `directorUrlKey`,
      `custodianUrlKey`, `registrarUrlKey` or `usherUrlKey` in its value
      mapping gets outgoing access to all of `kernel-control`, every port
      (`internal/controller/component_network_policy.go`; since `ca66f192`,
      2026-09-24). Only `storeUrlKey` requires `trustTier: platform`
      (`api/v1alpha1/componentprofile_types.go`). The annotation
      `gentianos.io/kernel-egress-namespaces` on a profile opens any
      namespace it names, whole (`internal/kernel/netpolicy/kernel.go`;
      since `06c1f1ea`, 2026-07-02). An app that declares OIDC or SAML gets
      all of `kernel-edge` and all of `kernel-authentication`.
    - *Why:* the code's comment says that following the mapping, not a
      name, lets the console and the desktop reach the director and "a
      component that never asked cannot". Asking is all it takes.
    - *Today:* only the desktop, the admin console and the App Store app
      name these keys. No app in the catalogue does. *The catalogue's part
      is reasoned, not confirmed here.*
    - *Option A, follows AD-5:* each of these becomes a privilege that is
      requested and approved, or requires `trustTier: platform` as
      `storeUrlKey` does, and the annotation is removed or held to an
      allowlist. Cost: schema rules and, for the annotation, a migration
      of whatever profile still uses it.
    - *Option B:* leave it. A catalogue profile can then open itself a
      path to the director, the custodian, the registrar and the usher,
      and with the kernel's rules off (§9) nothing else stands between.

35. **Commands of the operator that leave no commit and no record**
    (AD-2, §8.1). **Open, awaiting the owner.** The owner agreed on
    2026-10-06 to keep the operator's command listener and have it
    documented. One reason given for keeping it was that the director
    records every action with who and when. That was not true then and is
    not true now.
    - *In the code, confirmed:* the director calls the operator's listener
      for backup, backup deletion, restore, notices, purge of an app,
      provisioning of an app and removal of leftover catalogue pieces.
      `internal/director/lifecycle/lifecycle.go` says of these: "happens
      now, leaves no commit". For a purge of an app the only trace is a
      log line of the operator (`internal/applifecycle/apps.go`). The
      director has no database.
    - *Option A:* the director writes a lasting record of each command and
      who asked. It needs a place to write to; the director holds none.
    - *Option B:* the destructive commands become declared state: a commit
      the operator acts on. This is AD-2's direction ("git is the trusted,
      audited source"). A purge then takes as long as a sync, and a failed
      one has to be reported back another way.
    - *Option C:* leave it and state it. A purge, a deleted backup and a
      removed leftover then have no author anywhere once the pod's log is
      gone.

36. **How a person comes to hold a platform role.** **Open, awaiting the
    owner.** The owner agreed on 2026-10-06 that the registrar also
    protects the groups behind the other platform roles. Not said with the
    question: after that, nothing on the platform can put a person into
    such a group.
    - *In the code:* the registrar refuses any change to a group the
      Cluster claim names for a platform role
      (`internal/registrar/identity/guard.go`; commit `e8ac877d`).
      `docs/commands.md` documents the refusal. No route of the director,
      no installer step and no command adds a person to one. *The guard is
      confirmed in code; that no other path exists is from a search, and
      how the first platform administrator gets into the group was not
      traced.*
    - *What follows:* an auditor, a security officer or a service
      administrator is made by hand in Keycloak, with the master
      administrator's password.
    - *Option A:* a declared path, for example the holders named on the
      Cluster claim and applied by the composition. It is then in git and
      signed.
    - *Option B:* a narrow route at the registrar for the platform
      administrator, with a second approval. The registrar's client can
      already change those groups; its own guard is what stops it.
    - *Option C:* keep it by hand and say so in the documents.

37. **One profile per name for the whole cluster, and the digest check at
    rollout.** **Open, awaiting the owner.** The owner agreed on
    2026-10-06 that the operator verifies the digest at rollout. Not said
    with the question: a profile is one object per name for the whole
    cluster.
    - *In the documents, confirmed:*
      [store-contract.md](../design/store-contract.md) states that a
      tenant that installs a newer build replaces the profile under every
      other tenant, and that those are then held with `DigestMismatch`
      until they move their pin (commit `4c98327b`). *The code path was
      not traced.*
    - *Option A:* keep it. On a cluster of many tenants one tenant's
      update holds every other tenant's copy of that app until each moves.
    - *Option B:* a profile per build, or per tenant. Tenants then do not
      touch each other's rollout. It changes how profiles are named and
      found everywhere; it is not a small change.

38. **The usage statistics report: its signing key, and how people are counted.**
    **Built, not decided.** Both were proposed while the owner was away
    and built without an answer (`d4ef4369`, 2026-10-06).
    - *In the code, confirmed:* the report is signed with a key the
      install puts into the vault (`internal/usagestatistics/reporter.go`).
      The operator sends it, and counts people with Keycloak's master
      administrator credential: accounts per realm and members per app
      group (`internal/controller/usage_statistics_counts.go`). The owner
      had been told the counts could come from the rights store and would
      need no access to Keycloak.
      [operator-split-plan.md](operator-split-plan.md) describes the
      report as "counts and addresses of things, never of people"; the
      report carries head-counts, with no names.
    - *Also open:* the owner said that all apps count, not only installs
      from a store. Built: every entry of `spec.apps` that carries a
      digest (`internal/usagestatistics/report.go`). An app without a digest
      is not reported. Whether "all" meant that is not known. The chart's
      comment still says "the apps installed through the App Store"
      (`kernel/bootstrap/chart/values.yaml`).
    - *Options for the count:* keep it; or count from the rights store's
      memberships, which needs no Keycloak credential and is one more
      reason to remove defect 15; or a Keycloak client that can only
      count.
    - *Options for the key:* keep the signed report; or send it unsigned,
      which removes one secret and lets anybody forge a cluster's report.

39. **Administrator-only template routes in the desktop** (AD-10, §8.7).
    **Built, not decided.** The owner was asked whether template
    management should move from the desktop to the admin console and said
    no: notifications, templates and a person's other settings belong in
    the desktop. The question did not say that the routes in question are
    administrator-only.
    - *In the code (user-interface repository, `develop` at `3c7130ac`,
      confirmed):* `apps/desktop/backend/app/api/routes/prefs.py` has
      four routes behind the desktop's own `require_admin`: list, create,
      delete a template, and apply one to a person. The desktop decides
      itself who is an administrator. In this repository the registrar
      relays to them with the caller's token
      (`internal/registrar/people.go`, since `e6bfa8df`).
    - *What the plans say:* AD-10: the desktop "must hold no
      administrative code path". The code contradicts it.
    - *Option A, follows AD-10:* the routes that act on other people move
      to the admin console's side, behind a check at the rights store; the
      registrar's relay goes. A person's own settings stay in the desktop,
      as the owner said.
    - *Option B:* they stay, and AD-10 gets an exception for templates.
      The desktop then keeps deciding an administrator's right by itself.

40. **The check that the cluster's definitions are as new as the
    software.** **Built, not decided as to its reach.** The owner agreed
    on 2026-10-07 to a check at start "that refuses loudly". Not said: how
    much stops.
    - *In the code:* the operator reads a named list of definitions
      (`charts/gentian-os/templates/clusterrole.yaml`, commit `c81547d0`).
      The director asks the operator before a commit and refuses one that
      sets a field the cluster would drop
      (`internal/director/gitops/definitions.go`). The remedy the code
      names for stale Crossplane definitions is
      `./install.sh --only B-06`, run by hand
      (`internal/schemacheck/definitions.go`). *Confirmed in code.* That
      an outdated definition holds every reconciler, and that the
      director refuses when the operator cannot be reached, is *reasoned,
      not confirmed*.
    - *Option A:* keep it. A cluster upgraded without its definitions
      stops work until somebody runs an installer step by hand.
    - *Option B:* deliver the Crossplane definitions through Argo CD, so
      that they cannot fall behind.
      [operator-split-plan.md](operator-split-plan.md) lists this as not
      built. It removes the hand-run step.

41. **The platform tenant's admin console at
    `admin.platform.<kernel domain>`.** **Built, not decided as to its
    cost.** The owner agreed on 2026-10-07 that the platform desktop and
    its admin console move under `platform.<kernel domain>`. The example
    in the question became the design.
    - *In the code and documents, confirmed:* the console answers at a
      name two levels below the kernel domain. The cluster's certificate
      does not cover it; it needs a wildcard of its own,
      `*.platform.<kernel domain>`, which only a DNS-01 issuer can give
      ([multi-tenancy.md](../design/multi-tenancy.md); commit `3903ba44`).
    - *Option A:* keep the name. A cluster whose issuer is not DNS-01 has
      no certificate for the platform tenant's admin console.
    - *Option B:* a name one level down, covered by the kernel's wildcard.
      It needs one more reserved name and changes addresses already
      written into documents and tests.

42. **Objects a catalogue brings, applied into a kernel namespace.**
    **Built, not decided.** The owner agreed on 2026-10-07 that an app's
    extra files travel with its profile under one digest. The question
    called them "extra files". They are Crossplane Compositions and
    sign-in pack catalogues, which are cluster-wide and decide what gets
    created, and ConfigMaps and customization records.
    - *In the code, confirmed:* the Argo CD Application for the catalogue
      applies what the director wrote into the deployments repository into
      the provisioning kernel namespace, in project `gentian`
      (`kernel/appsets/raw/11b-catalogue.yaml`). Project `gentian` allows
      every kind (§8.9). What limits the kinds is the allow-list in the
      director and the operator.
    - *Option A:* keep it. One allow-list in code is the only gate between
      a catalogue and a kernel namespace.
    - *Option B:* a namespace of its own for the companions, and an Argo
      CD project for the catalogue that allows only the kinds a bundle may
      bring. A wrong entry in the allow-list is then still stopped by Argo
      CD.

43. **Who sees and removes what a newer build of an app left behind, and
    the operator's right to delete it.** **Built, not decided.** The owner
    had said that the tenant's administrator would know about leftovers
    and could remove them. A counter-proposal — show them to the platform
    administrator — was put inside a longer message, and the "go ahead"
    that followed was taken as agreement to it.
    - *In the code, confirmed:* the cluster's list is read under
      `can_audit` on the cluster (`internal/usher/state.go`). Removal for
      the cluster is under `can_configure`
      (`internal/director/api/api.go`). Since `a75877d7` (2026-10-08) a
      tenant's administrator sees the leftovers of the tenant's own apps,
      and may remove them only where the tenant is the one user tenant the
      cluster carries (`internal/director/api/residue.go`). For this the
      operator was given delete on Compositions, `ComponentProfile`s and
      sign-in pack catalogues
      (`charts/gentian-os/templates/clusterrole.yaml`).
    - *Option A:* confirm the arrangement of 2026-10-08. On a many-tenant
      cluster a tenant's administrator sees and cannot remove.
    - *Option B:* the tenant's administrator removes on every cluster, as
      first said. The pieces are cluster-wide, so one tenant could remove
      what another tenant's older install still uses.
    - *Option C:* only the cluster's administrator removes. Switching the
      tenant's route off is one condition in the director and the
      operator.
    - *The operator's delete right* stands under all three; without it
      nothing on the platform can remove a leftover Composition.

44. **The sign-in sidecar, and OpenProject signed in by it.** **Built,
    not decided.** The owner said "yes, build it" to a question that
    bundled four things, while sign-in was on hold and the ten decisions
    of [sign-in-procedure.md](sign-in-procedure.md) had not been taken.
    That plan still shows its decisions 3 and 7 as undecided, with the
    opposite recommendation: a second sign-in with accounts made by the
    platform, a helper per app only after review, and for OpenProject the
    second sign-in in the free profile.
    - *In the code, confirmed:* a profile can declare that the platform
      signs people in (`0943eeb5`, 2026-10-08;
      `internal/controller/signin_sidecar.go`). The sidecar's route
      `/sso/acs` is reached with no session (§7.4). In the catalogue
      repository (`develop` at `a04b9f6`), OpenProject's profile turns the
      password form off for everybody, locks the seeded administrator, and
      gives the sidecar the app's database and two of its secrets
      (`profiles/openproject/openproject-ce/profile.yaml`). Activepieces
      and Docmost declare the sidecar too. *That three things are written
      straight into OpenProject's database is reasoned from the profile's
      comments, not confirmed.*
    - *What was not said with the question:* the sidecar can become any
      user of its app and is handed the app's full database login; nobody
      administers a new OpenProject until somebody holds the platform's
      App Admin role; the handler reaches what the vendor sells as a
      feature of its paid edition, which is a license question.
    - *Option A:* confirm what is built, and bring sign-in-procedure.md's
      decisions 3 and 7 in line with it.
    - *Option B:* the plan's recommendation. OpenProject goes back to a
      second sign-in in its free profile; that means restoring its earlier
      profile and Composition in the catalogue. The sidecar stays for
      apps reviewed one by one.
    - *Option C:* the sidecar stays and OpenProject alone goes back.

45. **Backup and restore units that run in kernel namespaces with the
    object store's administrator credential.** **Built, not decided.**
    The owner was asked whether each part of a backup should run where its
    password is, did not answer that point, and gave a general
    instruction to move on. Then, on an instruction to close the gaps in
    backups, more was built.
    - *In the code, confirmed:* a backup's units run beside what they
      read, among others in the identity provider's namespace and the
      database namespaces, and the object store's administrator Secret is
      copied there for the run
      (`internal/controller/tenantexport_placement.go`, commit `693c7a40`,
      2026-10-08). No administrator credential of a database or of
      Keycloak is copied; the file says so. The operator may create
      volume claims (`charts/gentian-os/templates/clusterrole.yaml`,
      commit `fe3d387a`, 2026-10-09). The object store's network rule
      admits backup pods from those namespaces
      (`kernel/services/infra-seaweedfs/manifests/templates/networkpolicy.yaml`).
      *That the backup key is staged into `kernel-data` and `system-mail`
      is reasoned, not confirmed.*
    - *Option A:* confirm it.
    - *Option B:* stage a storage account that can only write bundles in
      place of the store's administrator. A pod in a kernel namespace
      that reads the staged Secret during a run then cannot read or delete
      other tenants' bundles. It needs such an account made per run or per
      tenant.

46. **Which tenant namespaces the kernel's PostgreSQL admits.** **Open,
    awaiting the owner.** The owner approved rules described as: the
    kernel's database accepts the programs that keep data there, the
    operator and the database manager.
    - *In the code, confirmed:* the policy admits every namespace labelled
      `gentianos.io/tier=tenant`
      (`kernel/data/kernel-postgres/templates/networkpolicy.yaml`, commit
      `754b250d`, 2026-10-08), and the whole identity and rights-store
      namespaces. The tenants' PostgreSQL admits every tenant namespace as
      well (`kernel/data/tenant-postgres/templates/networkpolicy.yaml`).
    - *Why, from the policy's own comment:* the one tenant whose desktop
      keeps its database there is decided by its realm, not its name, so
      no namespace can be named; what narrows it is the tenant's side,
      where outgoing connections are denied by default.
    - *Option A:* keep it. The server side is wide; a tenant namespace
      whose own outgoing rule is wrong or missing reaches the kernel's
      database port, and the password is what remains.
    - *Option B:* the operator writes the server-side rule and names the
      one tenant. The policy then changes when that tenant does.

47. **The desktop's own key at the model gateway** (§8.7). **Built, not
    decided.** The owner said yes to "fixing the AI widget". Not said with
    the question: it gives the desktop a credential of its own, every
    member of a tenant spends on one key with no limit per person, and the
    widget had by then become a button that opens Open WebUI, so the fix
    brought back a chat that had been taken out. A later "say if you
    prefer the launcher only" was not answered.
    - *In the code, confirmed:* a key per tenant's desktop, delivered as
      the Secret `llm-credentials-desktop`
      (`internal/controller/model_access_reconciler.go`, since `3c241470`
      and `d8934c89`, 2026-10-08;
      [security.md](../design/security.md) §2.9). The desktop's backend
      relays a person's chat with that key
      (`apps/desktop/backend/app/api/routes/llm.py` in the user-interface
      repository).
    - *What the plans say:* [operator-split-plan.md](operator-split-plan.md)
      §8: "The consoles hold no credential of their own." The code
      contradicts it for the desktop.
    - *Option A:* launcher only. The route and the key go; the plan's
      sentence is true again. People chat in Open WebUI.
    - *Option B:* keep the chat. The plan's sentence is changed, and cost
      per person stays unbounded until the gateway has budgets.

48. **The registrar's right to write `MailboxRemoval`** (§8.4, §10.6).
    **Built, not decided.** The owner's answer on what happens to a
    removed person's mailbox was: ask whether it should be archived or
    not. That was built with a new cluster-wide kind and the registrar's
    first write right in the cluster. The owner was not told of either.
    - *In the code, confirmed:* the registrar may get, list, watch,
      create, update and delete `mailboxremovals`
      (`charts/gentian-os/templates/registrar.yaml`, commit `6a7e2cad`,
      2026-10-09). The operator acts on such a record only if the address
      is in the tenant's mail domain, no person of the realm holds it and
      the mail server holds no password for it.
    - *What the plan said:* the registrar holds people at Keycloak and
      nothing in the cluster. The plan was changed afterwards to match
      (§10.6).
    - *Option A:* accept the one write. A taken-over registrar can then
      delete an account and have that person's mailbox deleted.
    - *Option B:* the choice is committed by the director, as other
      declared state is. The registrar writes nothing in the cluster
      again; the operator acts on the commit. A removal then takes a
      sync, and the director has to be told of it.

49. **Whether the check on per-tenant L3 customizations is applied.**
    **Open, awaiting the owner.**
    - *In the code, confirmed:* `ValidateNamespaceScoping` and
      `RequiresTenantNamespace` are in
      `internal/customization/policy.go` again (restored by `d7b25cd9`,
      2026-10-11, after `6287fe08` had deleted them on 2026-10-10).
      Nothing but their tests calls them.
    - *What it guards:* the recorded decision "per-tenant L3 on shared
      runtimes: forbidden" ([app-customization.md](../app-customization.md),
      decisions table). That document calls the risk a cross-tenant data
      leak.
    - *Option A:* wire the check into the customization controller, so a
      record that breaks the rule is refused. A record that exists and
      breaks it would then be reported.
    - *Option B:* leave it written and not applied. The decision is then
      enforced by nothing.

50. **The chart registry settings the installer lost.** **Built, not
    decided.** The owner said "fix the faults" to a list of fourteen
    installer faults of which five were described. Two of the others were
    fixed by deleting settings. Asked later whether the removed
    `infra-chart-registry` credential entry should stay removed, the
    owner said no.
    - *In the code, confirmed:* the entry is declared again, optional
      (`kernel/credentials/credential-requirements.yaml`, commit
      `afb15ac9`). The settings `INFRA_CHART_REPO`, `INFRA_CHART_PRIVATE`
      and `GENTIAN_OS_IMAGE_REPOSITORY` are gone from the installer
      (`5e75176b`, 2026-10-10); the installer neither asks for the
      credential, checks it nor stores it.
    - *Why it matters:* the roadmap had described this as the built half
      of redirecting third-party charts through a mirror of the cluster's
      own, which is what running without anybody else's registry needs.
    - *Option A:* "should not be removed" meant the entry only. Nothing
      more to do.
    - *Option B:* it meant the registry setting as a whole. Then the
      installer's part of `5e75176b` is put back for those settings.
    - *Under the same instruction, also changed and to be confirmed:* a
      variable exported in the shell now wins over `install.env`; the
      user-interface branch defaults to `develop`; a tunnel install stops
      when its Secret in `default` is missing. *These three are reasoned,
      not confirmed.*

51. **A mailbox opened with a sign-in token.** **Built, not decided.**
    The owner approved it on the statement that webmail apps use it.
    No app in the catalogue does.
    - *In the code, confirmed:* a profile field `tokenSignIn`
      (`api/v1alpha1/profile_parts.go`, commit `45ac9add`, 2026-10-08), a
      realm scope, an object per app in Keycloak and a Dovecot setting.
      No profile in the catalogue repository (`develop` at `a04b9f6`)
      declares it. Decided without the owner: people of the kernel realm
      can never open a mailbox with a token.
    - *Option A:* keep it for a later app.
    - *Option B:* remove it. Token sign-in to mail is then refused again,
      which was the state before, and one less path into the mail server
      exists.

52. **Smaller things built without the owner or beyond the answer.**
    **Built, not decided.** Each is confirmed in code unless marked; each
    is put for a yes or a no.
    - **a. Kubernetes 1.33 as the floor.** The installer refuses an older
      cluster (`scripts/lib/common.sh`). Why: the pinned Envoy Gateway is
      tested on 1.33 to 1.36. A no means installing on versions the edge
      is not tested on.
    - **b. A second kind of token at the bouncer**, for Keycloak's
      administration console (`internal/bouncer/decider.go`): the
      console's own bearer is kept on `/auth/admin/` (§7.6). A no means
      the console at `id.<kernel domain>` does not work behind the edge
      and is reached another way.
    - **c. A catalogue's address must be public `https` on port 443**,
      for the cluster's administrator too
      (`internal/director/catalogue/address.go`). **Answered by the owner
      2026-10-19: yes.** Addresses stay public-only — https, port 443,
      public hosts, no redirects, no proxy — for everybody. What is
      private reaches a cluster through a catalogue kept as an OCI
      artifact, read with the credential of a repository its owner
      declared, which a delegated tenant's administrator may add as well
      (AD-14, [custom-catalogues.md §4.5](../custom-catalogues.md)); never
      through a private network address.
    - **d. Reserved address names beyond those asked for.** The owner
      asked for the critical names including desktop and console. Also
      reserved: `auth`, `login`, `signin`, `sign-in`, `sso`, `account`,
      `accounts`, `store` (`internal/hostnames/hostnames.go`). Why, from
      the code: names a sign-in page would have. A no frees them for
      apps.
    - **e. Every cluster on a tunnel publishes `corp.<kernel domain>`**
      (`internal/controller/gateway_tunnel_ingress.go`), with no route in
      this repository. The question was put and not answered. A no
      removes the host from the tunnel's list.
    - **f. A cluster installed from `main` reads the released
      catalogue**, like one installed from a release tag; any other
      branch reads the development one (`gentian_catalogue_url`,
      `scripts/lib/bootstrap.sh`). Put and not answered.
    - **g. `./install.sh --rotate-break-glass-key`** (`install.sh`,
      commit `94908c79`). The owner had agreed that the installer
      refuses when the recorded signing key is not on the machine; the
      command that replaces the key came with it. A no removes the flag;
      a lost break-glass key then means a new deployments history.
    - **h. The profile field `backchannelLogoutUrl` is refused by the
      schema** (commit `21dd459c`; defect 20). The owner had agreed to
      the sign-out notice inside the cluster; withdrawing the free-form
      field was a separate question that was not put. A no means
      admitting the old field again beside `backchannelLogout`.
    - **i. Apps reach Keycloak at its address inside the cluster.** The
      owner agreed on 2026-10-08 that apps use the public sign-in address
      only. It was not carried out, and later work went the other way:
      the sign-in sidecar fetches the realm's descriptor from the
      internal address over plain HTTP
      (`internal/controller/signin_sidecar.go`), and four profiles of
      the catalogue name the internal service (both Nextcloud editions,
      Odoo, Open WebUI). The question is whether the answer of 2026-10-08
      still stands.
    - **j. The kernel realm as identity provider in tenant realms is
      switched off, with its code kept** (`7c999951`, `10aa0784`,
      2026-10-10). The owner had said "remove". The lookup of a tenant
      administrator in the kernel realm, switched off in the same change,
      was never named to the owner. *Reasoned, not confirmed.*
    - **k. Sending mail inside the cluster without signing in.** The
      owner's answer was "leave open". It can mean leave it as it is, or
      leave the question open. Nothing was built.
    - **l. A commit pushed to the deployments repository by an installer
      run with `--dry-run`**, during a check that was meant to change
      nothing, and a stray signing key made by it. Whether to revert it
      was asked and not answered. *Not confirmed here; it is in another
      repository.*

53. **The bouncer holds a Keycloak client secret per tenant realm**
    (§8.6). **Built, not decided.**
    - *In the code, confirmed:* the Secret `bouncer-exchange` is mounted
      into the bouncer (`charts/gentian-os/templates/bouncer.yaml`). It
      holds the secret of each tenant realm's client
      `gentian-edge-exchange`
      (`crossplane/compositions/tenant-default.yaml`), gathered by the
      operator. It came with `exchangeToken` (commit `82bf91c1`,
      2026-10-09), by which an app may ask for a token of the person
      that is valid at that app alone.
    - *What the plan says:* [operator-split-plan.md](operator-split-plan.md)
      §2 and §6 say the bouncer holds nothing of its own, and §8 lists
      token exchange as not built. The code contradicts all three.
    - *Option A:* accept it and correct the plan, with its cost: a
      taken-over bouncer can trade any session token it sees for a token
      valid at an app, in every realm that has the client.
    - *Option B:* the exchange moves to a program that does not see every
      session. The bouncer holds nothing again; the front door then calls
      one more service on such a route.

54. **The four of the fourteen clean-up observations that were answered
    from a one-line label.** **Built, not decided.** On 2026-10-09
    fourteen observations were put to the owner as single lines, with no
    word on what was in use, why, or what an answer would cost. The
    password policy was one (corrected, §12). Four others were answered
    and built on 2026-10-10 and are restated in this list in full:
    grants between apps (decision 22), pod-security waivers and cluster
    roles (decision 23), backup key generation (decision 25), and the
    kernel realm as identity provider in tenant realms (decision 52 j).
    Nothing further is asked here; the entry is the record that these
    four answers were given without the facts, and that each is open to
    being given again.

## 5. Known and deliberately not now

- **Moving a tenant to another cluster goes through export and import**, which
  re-mint what the platform issues. The alternative, in which the recovery kit
  is the cluster's identity and a new server that imports it becomes the old
  cluster, is not taken: it would bind a tenant's portability to its
  provider's kit.
- **The director reads the cluster's catalogue sources from the claim in
  git.** An edit made outside the director reaches it with the next commit it
  reads.
- **A Cluster-claim setting for the mail proxy's load balancer** -- the PROXY
  header from a load balancer that is itself a proxy, the addresses it may
  come from, and the pinned mail address. They are values of the mail
  proxy's chart only. On the roadmap, item 2.25
  ([roadmap.md](../roadmap.md)).
- **Removing a deleted tenant's rights, memberships and shared-cache keys at
  deletion** (defect 9). On the roadmap, item 1.38.
- **The check at start of the rights store's defaults** (AD-12): wanted, and
  it reports, it does not repair. A store that has diverged is a question,
  because rewriting it would erase the grants and revocations that are
  nobody's default.

## 6. Carried over from the work packages

The work packages were a list of what the architecture cleanup had to build,
and the list is gone. This is what was still open on it and is recorded
nowhere else in this file or on the [roadmap](../roadmap.md). Each line was
checked against the code on 2026-10-10 for whether the thing exists, not for
whether it works. Where a plan specifies the item, the plan is named.
Seven lines were looked up again on 2026-10-11 and stand: no DPoP, the
operator still linking `internal/applifecycle`, no `egressAllow` fields, no
`credential.appliesWhen`, `AppPackage` still a kind, no API-server audit
policy, and `TenantExportSchedule` and `BackupPolicy` still kinds.

### The director and its callers

- **Tokens bound to their holder for callers that are not a browser** (DPoP,
  RFC 9449): the command line, an install asked for by a store, an agent.
  Nothing implements it.
- **One account that is both an administrator and a member** is not refused
  anywhere ([roles-and-authorizations.md](roles-and-authorizations.md) §1).
- **The operator still carries the install paths the director replaced**:
  `cmd/main.go` links `internal/applifecycle`. Removing them was the last
  step of the split ([operator-split-plan.md](operator-split-plan.md)).
- **A read for every write** on the director's API has not been checked
  route by route.

### Rights

- **The comparison of the rights store with Keycloak** — at start, after a
  failed event, and as a pass over every realm within fifteen minutes — and
  the alert when none has completed
  ([authorization-model.md](authorization-model.md) §2). The membership
  listener applies events; nothing re-reads. A lost *removal* leaves access
  in place until the person's next event.

### Network and publishing

- **Outgoing connections declared on the claims**
  (`Cluster.spec.network.egressAllow`, `Tenant.spec.network.egressAllow`
  and `denyKernel`, a tenant's list bounded by the cluster's): no such
  fields exist.
- **A filter for request content on a public entry that checks no caller**
  (a web application firewall on `authMode: none`): not built
  ([networking.md](networking.md)).
- **The publishing contract an app implements** (`exposure-policy`, and the
  cluster switch that would require it before an entry of `authMode: none`
  is enabled): not built ([target-component-structure.md](target-component-structure.md) §5.2).
- **A job that compares listeners, DMZ routes, DNS records and certificates
  with the approved entries**, and **the view of a public entry's traffic**
  (summary, condensed log, complete log): not built
  ([networking.md](networking.md) §8).
- **Proxy access logs with tokens hashed, in a log store with a retention
  the security officer sets**: waits on the log store
  ([security-gap-closing.md](security-gap-closing.md) G10).

### Catalogue

- **Admission rules for who may create which class of component in which
  tier of namespace**: not checked.
- **`credential.appliesWhen` on a `Repository` claim**, from which the
  credential requirements and the installer's own test of whether one
  applies would both be generated: no such field.
- **`AppPackage` is still a kind of the cluster**
  (`api/v1alpha1/apppackage_types.go`); the plan retired it with the
  catalogue that lived in the cluster.

### Installer and cluster

- **API-server audit logging.** The installer ships no audit policy. Until
  it does, what somebody does with a kubeconfig — break-glass above all —
  is recorded nowhere: it raises no Keycloak event and passes no service of
  the platform.
- **Kubernetes identities for the platform's roles**, with break-glass bound
  to `cluster-admin` by group, and **a second sign-in step for the acts
  that matter most**
  ([artefacts/roadmap-additions.md](artefacts/roadmap-additions.md)).
- **Tests that try to break each control** after a fresh install, as
  scripts.
- **A place in the deployments repository for the cluster's security
  policy and its exceptions** (`clusters/<c>/kernel/security/`), synced by
  an Application of its own.
- **The threat model run again against the code.**

### Export, import and purge

Specified by [sovereignty-concept.md](sovereignty-concept.md).

- **Offboarding as one act**: export to the tenant's key, hand over, purge,
  with the signed record of the deletion (roadmap item 5.5).
- **Canonical forms** of an app's data in a bundle (`spec.backup.canonical`
  on a profile): no such field.
- **`TenantExportSchedule` and `BackupPolicy` are still kinds of this
  repository** (`api/v1alpha1`); the concept has scheduling outside it, and
  [scheduled-backups-out.md](scheduled-backups-out.md) is the plan for the
  move. Nothing in the code has been changed for it. Decided for it (§4,
  decisions 31 and 32): the platform keeps a "destination and key" setting
  that a manual export reads, and v0.5 does not upgrade a cluster that
  already has schedules. Whether and when the move starts is open (§4, 32).
- **Import storage clean-up**: a bundle in `gentian-imports` is also removed
  when the restore that read it began and failed (behaviour from before,
  kept), not only on success; and two uploads or fetches of one tenant made
  at the same moment are each checked against its 50 GiB total alone, so
  together they can pass it until the sweep. *reasoned, not confirmed*

### Not this repository's

The desktop and the administration console, and a store outside the
cluster, had work packages of their own. What was open there — the second
sign-in on the Cluster tile, the Identity tile behind
two hostnames, a store's confirmation of an install, the license shown per
entry — belongs with those programs and is not carried here.

## 7. Departures from networking.md §1

[networking.md](networking.md) §1 is the diagram of the two paths into a
cluster: the `authenticated` Gateway and the `perimeter` Gateway. Its text
has one changed line since 2026-10-05, a renamed label. What moved are the
sections and decisions it rests on (§10). This section holds §1 as it
reads against the code at the commit named at the top.

**In line** (*confirmed in code* for the files named): one Envoy fleet;
two Gateways in `kernel-edge`, written by the operator
(`internal/controller/gateway_platform_reconciler.go`); sign-in first and
the bouncer second on every route the operator writes for an app; the edge
token forwarded only where the entry sets `forwardToken`, which requires
`trustTier: platform`; a publishing proxy in `tenant-<t>-dmz` only for an
approved entry, with no API token, that removes identity headers and
cookies (`internal/controller/component_perimeter.go`,
`component_perimeter_config.go`); `basic`, `signature`, `jwt` and `bearer`
refused on a public entry by the schema; the master realm refused on
`id.<kernel domain>`; mail behind one load balancer in `system-mail-dmz`.

**Departures**, most serious first.

1. **The edge's sign-in client secret is copied into tenant namespaces.**
   *Confirmed in code.* `ensureZoneSecret`
   (`internal/controller/component_reconciler.go`) copies the zone's
   client secret from `kernel-edge` into the component's namespace; for
   the platform tenant that is the kernel realm's client, in
   `tenant-platform`. It contradicts AD-13 and §4 of the plan. It is in no
   other document. In the code since `07372d08` (2026-09-23). Decision 33.

2. **A public entry is built on the perimeter Gateway also when it is a
   path on an app's own host.** *The construction is confirmed in code;
   the effect is reasoned, not confirmed, and needs a cluster.* The plan:
   perimeter paths on an app's own host stay on the `authenticated`
   Gateway, and "an app-host perimeter path never touches" the perimeter
   Gateway. The code: every published entry gets a listener for its exact
   host on the perimeter Gateway (`perimeterTenantListeners`,
   `gateway_platform_reconciler.go`) and its route attaches there
   (`component_perimeter.go`). An entry without `subDomain` answers on
   the component's own host (`internal/addresses/zone.go`), while the
   app's signed-in route is attached to the wildcard listener. No check
   was found that refuses a public entry sharing a host with a signed-in
   one. By reading, approving a public path on an app's own host moves
   that whole host to the exact listener, where only the public paths
   have a route, and the signed-in app would answer 404 there. The
   plan's status table still lists "perimeter paths on an app's own
   host" as built. No app in the catalogue declares a public entry; the
   page on the bare domain is the only one a fresh install has.

3. **No listener refuses a route by its namespace, and a route without a
   policy is served without a session.** *Confirmed in code; what Envoy
   does in the gap needs a cluster.* The plan: routes are admitted by the
   tenant listener's namespace selector, and "nothing is routable without
   a class". The code: every listener of both Gateways admits routes from
   all namespaces (`withAllowedRoutes`, `gateway_platform_reconciler.go`;
   so since `c3029e35`, 2026-06-12). Sign-in policies attach to a route,
   never to a listener. A route is created before its policy
   (`component_reconciler.go`: the route, then `ensureZonePolicy`). No
   admission rule limits who may create an `HTTPRoute`, a
   `SecurityPolicy` or a `NetworkPolicy`: the Kyverno policies in
   `kernel/security/kyverno/policies/` match pods only. So anything that
   can create an `HTTPRoute` in any namespace can publish a backend
   without sign-in. The roadmap has the open listeners as item 1.5. To
   bring it in line: a namespace selector per listener, as the plan
   says.

4. **Anonymous traffic reaches a pod in `tenant-<t>`: the sign-in
   sidecar's `/sso/acs`.** *Confirmed in code.* The plan: "the DMZ
   namespace remains the only thing that receives anonymous traffic".
   The code: a route on the `authenticated` Gateway with no policy and no
   bouncer, exact path, POST only, to the sidecar's Service in the
   tenant's namespace (`internal/controller/signin_sidecar.go`, since
   `0943eeb5`, 2026-10-08). AD-13 names it as its second exception since
   2026-10-09, written after it was built (§10.7). §1 and its status row
   do not mention it. Three apps of the catalogue use it (decision 44).

5. **A profile can open a network path to the whole control namespace,
   and to other kernel namespaces, without an approval.** *Confirmed in
   code.* The diagram shows only the desktops calling the director.
   Decision 34 has the detail. Beyond it: every pod of a tenant may reach
   the Kubernetes API where its address is known to the operator
   (`internal/kernel/netpolicy/baseline.go`).

6. **Keycloak's administration console is reachable on the perimeter
   Gateway.** *Confirmed in code.* The diagram's label says "/admin,
   master realm, metrics internal". The code routes `/auth/admin/` on
   `id.<kernel domain>`, behind the kernel realm's session and
   `can_configure`, with the console's own bearer kept
   (`internal/controller/kernel_gateway_routes.go`, since `21fee0b4`,
   2026-09-23). The plan contradicts itself here: its §3 was changed on
   2026-09-24 (`edd801c7`) and the diagram was not; the code follows §3.
   Also: the public allowlist is all of `/auth/realms/` and
   `/auth/resources/`, the master realm refused by a rule of its own.
   That is wider than §3's list, so the account pages and the broker and
   client-registration endpoints of every other realm are reachable.

7. **The publishing proxy checks no caller and holds no credential.**
   *Confirmed in code.* The diagram says "one credential"; AD-9 says
   "with that tenant's credential". The proxy filters paths and limits a
   rate; on an approved entry of `authMode: app` it passes the caller's
   `Authorization` header to the app. Defect 5; the amendments of AD-1
   and AD-6 that state it are §10.4 and §10.5.

8. **The model gateway's console contradicts AD-9 when it is switched
   on.** *Confirmed in code.* Off by default. Decision 6, defect 12.

9. **Parts of the diagram that are not built.** *Confirmed in code by
   search.*
   - No address `api.<kernel domain>` and no route to the director for a
     client with a bearer token; no JWT check at the edge. An entry of
     the Gateway that is not `oidc` is not routed.
   - No TCP or UDP listener on the perimeter Gateway; no TURN; no port
     8448.
   - Mail enters through its own load balancer in `system-mail-dmz`, not
     through the Gateway (decision 7).
   - No filter for request content (a web application firewall).
   - `shared-<app>`: no code creates such a namespace or routes into
     one; `layout.Shared` is defined and used by nothing. The plan's
     status table does not say so.
   The first four are in the plan's status table of 2026-10-09 (§10.2).

10. **Smaller departures.**
    - *Confirmed in code:* the edge token is forwarded to three
      components, not to "the desktop" alone: the desktop, the admin
      console and the App Store app
      (`charts/gentian-os/templates/componentprofile-*.yaml`). All three
      are `trustTier: platform`.
    - *Confirmed in code:* the install publishes the page on the bare
      domain itself, with no approver (`scripts/lib/bootstrap.sh`). AD-6
      says "nothing is published by default".
    - *Confirmed in code:* on a tunnel cluster `corp.<kernel domain>` is
      published with no route in this repository (decision 52 e).
    - *Confirmed in the catalogue repository:* apps call Keycloak at its
      address inside the cluster, a path the diagram does not draw
      (decision 52 i).
    - *Reasoned, not confirmed:* calls between kernel programs that the
      diagram does not draw — the director and the usher to the
      operator's listener, Keycloak to the operator's membership
      listener, Keycloak to apps for sign-out, the registrar to a
      tenant's desktop.
    - *Confirmed in the user-interface repository:* the desktop's backend
      still has an optional `OIDC_CLIENT_SECRET` setting
      (`apps/desktop/backend/app/core/config.py`). The profiles of this
      repository do not set it. Which sign-in mode a deployed desktop
      runs in needs a cluster.

## 8. What each program holds beyond operator-split-plan.md §2

[operator-split-plan.md](operator-split-plan.md) §2 is the cast: which
program does what and holds what. Its rule is one standing credential per
program. The document was rewritten from the code on 2026-10-07 (§10.1),
so "beyond the plan" means two things here and each entry says which:
beyond what the plan says **today**, or beyond what it said on
**2026-10-05** (commit `abb39901`), before it was rewritten.

**In line with today's text** (*confirmed in code*): the director has no
right in the cluster and no API token
(`charts/gentian-os/templates/director.yaml`); it holds no Keycloak and no
vault credential. The usher holds nothing beyond its token for the
operator's listener and the OpenFGA key. The custodian's vault policy has
no read and no delete. The registrar's client holds four roles and none in
the master realm. The operator has no git credential.

1. **Director — commands into the operator that destroy data without a
   commit.** *Confirmed in code.* Beyond 2026-10-05, when the plan said
   the operator ends with no listener; documented since `50b11570`
   (2026-10-06). Decision 35. Also: the director asks the operator before
   a commit (decision 40), so it depends on the operator being up. Not
   built from the plan of 2026-10-05: a rate limiter, an OpenFGA identity
   of the director's own, a read-only repository token for Argo CD. The
   signing key is not the director's alone: it is in the vault under
   `gentian-os/`, which the operator and provider-vault can read, and it
   is made on the machine that installs (`scripts/lib/signing.sh`). *The
   last sentence is reasoned, not confirmed.*

2. **Operator — every kind of right at once.** *Confirmed in code.*
   Today's plan says so; the plan of 2026-10-05 did not.
   - Cluster-wide get, list, watch, create, update, patch and delete on
     Secrets, namespaces, ConfigMaps and Services; create and delete on
     pods and create on `pods/exec`; create and delete on volume claims;
     delete on Compositions, `ComponentProfile`s, sign-in pack catalogues
     and customization records
     (`charts/gentian-os/templates/clusterrole.yaml`). No right on RBAC
     kinds and none on `ClusterSecretStore`.
   - Since 2026-10-11: get, list and patch on Argo CD's `ApplicationSet`s,
     to ask for a refresh after the director pushed. Patch is enough to
     change what a set generates.
   - The vault policy `operator-write`: create, read, update, patch and
     delete on all of `gentian-os/*`
     (`crossplane/compositions/cluster-default.yaml`). That includes
     `gentian-os/kernel/backup/identity`, the escrowed backup key, which
     the policy for External Secrets denies by name. **The operator can
     read the escrowed backup key.**
   - Keycloak's master administrator, by password on `admin-cli`
     (`internal/authz/keycloak_client.go`,
     `internal/controller/identity_reconciler.go`). Defect 15. The same
     password is put into backup, restore and provisioning Jobs
     (`internal/backup/jobs.go`), and is used to count people for the
     usage statistics report (decision 38).
   - The only code that writes the rights store. On 2026-10-05 the plan
     gave that to the director (§10.1).
   To bring it in line with the plan's own rule: the store's write and
   the master credential in different programs, or clients per realm in
   place of the master credential; a vault policy that leaves out
   `kernel/backup/identity` and the signing key's path.

3. **Custodian — may rewrite any `ExternalSecret` in the cluster.**
   *Confirmed in code; the consequence is reasoned.* Beyond today's
   text, which says it can only ask a secret's consumers to refresh and
   "cannot change any object that decides configuration". Its ClusterRole
   has `update` on `externalsecrets` cluster-wide
   (`charts/gentian-os/templates/custodian.yaml`, since `887fd6e5`,
   2026-10-06). The code only sets an annotation
   (`internal/custodian/refresh.go`). With `update`, a taken-over
   custodian could point an `ExternalSecret` in a kernel or system
   namespace at any kernel path — the master password, the director's
   signing key — and have it written into a Secret there. It cannot read
   Secrets itself. Its vault policy also writes all of `gentian-os/*`,
   not only the credential paths. To bring it in line: an admission rule
   that admits only the annotation, or the operator doing the refresh.

4. **Registrar — one write right in the cluster, callers the plan does
   not name, and one sentence that is stale.** *Confirmed in code.*
   - Create, update and delete on `MailboxRemoval`. In today's text,
     dated; not in the plan before 2026-10-09. Decision 48, §10.6.
   - Vouching routes that a component opens with a key of its own, not
     with a person's token (`internal/registrar/server.go`,
     `vouching.go`; commit `6895b84a`, 2026-10-09). The plan names only
     callers that carry a person's token and does not mention vouching.
   - The plan's §4.4 still says the registrar's client may "manage the
     realm's settings". The code and the plan's §6 say it cannot: the
     client holds `view-users`, `query-users`, `query-groups` and
     `manage-users` (`internal/authz/keycloak_registrar_client.go`). The
     plan contradicts itself.
   - The grant of roles only adds. A realm made before `5fd8b15f`
     (2026-10-11) keeps `manage-realm` until it is taken away at
     Keycloak. Not relevant on a fresh install.

5. **Usher — in line.**

6. **Bouncer — holds a Keycloak client secret per tenant realm.**
   *Confirmed in code.* Beyond today's text. Decision 53.

7. **Desktop — holds a credential and decides an administrator's right
   itself.** *Confirmed in the user-interface repository, `develop` at
   `3c7130ac`.* Beyond today's §8, "the consoles hold no credential of
   their own", and against AD-10.
   - A key at the model gateway, one per tenant, used for every member
     (decision 47), and a database login.
   - Administrator-only template routes that it authorises itself and
     that change another person's settings under its own database login
     (decision 39).
   - A route that fetches an address it is given, `check-iframe`
     (`apps/desktop/backend/app/api/routes/prefs.py`). Its function asks for no signed-in
     caller; whether the router adds one was not checked.
   - With no issuer configured, the backend treats every caller as an
     administrator of a tenant named `demo`
     (`apps/desktop/backend/app/core/auth.py`). The profiles shipped here
     set the issuer; what a deployed desktop has needs a cluster.

8. **Programs outside the cast that hold as much as the operator or
   more.** *Confirmed in code.* None is in the plan's §2 or §6.
   - **provider-helm** is bound to `cluster-admin`
     (`crossplane/providers/provider-rbac.yaml`; the file says "and not a
     TODO"). Roadmap 1.16.
   - **provider-kubernetes**: full rights cluster-wide on Secrets,
     namespaces, `ExternalSecret`s, `ClusterSecretStore`s and Argo CD's
     projects and ApplicationSets (same file). It can rewrite the Argo CD
     project and the secret store.
   - **provider-vault**: a token renewed for a year, in the Secret
     `openbao-crossplane-token`, with a policy that may write any policy,
     any auth role and any mount, and all of `gentian-os/*`
     (`internal/vaultmigrate/crossplane-write.hcl`). It is the vault's
     administrator. The operator can read that Secret.
   - **provider-keycloak**: Keycloak's master `admin` on `admin-cli`
     (`kernel/services/keycloak-config/manifests/templates/provider/externalsecret.yaml`).
     So the master credential is held by two programs and by Jobs.
   - **External Secrets**: the policy `eso-read` reads `gentian-os/kernel/*`
     except the backup key. Whoever can write an `ExternalSecret` in a
     kernel or system namespace — the operator, provider-kubernetes, the
     custodian by `update` — can turn a kernel secret into a Secret
     (decision 15).
   - **The installer** makes both signing keys on the machine that
     installs and signs its own commits with the break-glass key
     (`scripts/lib/signing.sh`); it is a second writer of the deployments
     repository by design. No step was found that removes the director's
     private key from that machine's keyring. *The last sentence is from
     a search; whether the key remains needs the machine.*

9. **Argo CD — its project allows every kind, and it shares the push
   credential.** *Confirmed in code.* Project `gentian` allows every
   cluster-wide and every namespaced kind
   (`kernel/bootstrap/chart/templates/gentian-appproject.yaml`). The
   signature rule in the same file is rendered only when the repository
   and the signing keys are known; otherwise commits are synced unsigned.
   It shares the director's credential, which can push (defect 16).

10. **People: platform administrators are `cluster-admin` where sign-in
    to Kubernetes is on.** *Confirmed in code.* The group
    `/gentian:platform:admin` is bound to `cluster-admin`, and the
    proxy's ServiceAccount may impersonate
    (`kernel/bootstrap/chart/templates/kube-oidc-proxy.yaml`). It is not
    in the plan's table of what a break-in costs. A platform
    administrator who signs in to the vault gets the policy
    `cluster-admin` there, which reads `gentian-os/kernel/*`
    (`crossplane/compositions/cluster-default.yaml`).

11. **Consoles.** *Confirmed in the user-interface repository for the
    two points named.* The admin console holds no credential and relays
    the person's token. Its backend still has the install relay
    `POST /api/v1/catalogue/apps/{profile}`
    (`apps/admin-console/backend/app/api/routes/catalogue.py`) while its
    screen is switched off (`SHOW_CATALOGUES = false`). The owner had
    said not to remove the code and not to show it. AD-14 says no
    install action lives in an administrative screen; the director
    decides either way, since the call carries the person's token. The
    desktop is entry 7.

12. **One key opens the rights store for six programs** (defect 3). On
    2026-10-05 the plan had an identity per program.

13. **AD-1 names five enforcement points** — gateway, publishing proxy,
    director, custodian, MCP gateway. The registrar and the usher have
    decided requests since 2026-10-06 and are not in the list
    ([architectural-decisions.md](architectural-decisions.md)).
    *Confirmed in the document.*

14. **Routes of the director that write a repository's address ask
    `can_write_credential`** (`internal/director/api/api.go`), for what
    is now a commit of configuration. Reported to the owner on
    2026-10-06 and not answered. *Confirmed in code.*

## 9. Not enforced on a default install

`KERNEL_NETWORK_POLICIES` is off by default (decision 8). With it off none
of the twelve policies in
`kernel/security/network-policies/kernel-network-policies.yaml` exists.
That is *confirmed in code*. What the policies would admit was read in
the file for the identity provider, the rights store, the control
namespace and the edge, and is taken from the file's comments for the
rest. Whether the cluster's network enforces any `NetworkPolicy` at all
*needs a cluster*.

**Not enforced by a network rule while the switch is off:**

- The rights store reachable from the enforcement points only, as the
  diagram of [networking.md](networking.md) §1 has it. Its key is the
  only barrier, and six programs share that key (defect 3).
- The bouncer's authorization port admitting Envoy alone.
- OpenBao and its seal admitting only named clients.
- Keycloak's port 8080 held to named namespaces, and its management port
  closed.
- The director, the usher, the custodian and the registrar held to tenant
  namespaces; Headlamp held to Envoy; the Kubernetes sign-in proxy
  closed.
- A tenant's `tenant-isolation` policy admits the whole `kernel-edge`
  namespace, not Envoy's pods alone: `NarrowEdge` follows the same
  switch (`internal/controller/tenant_network_policy.go`,
  `internal/kernel/netpolicy/baseline.go`). cert-manager, external-dns
  and the bouncer share that namespace, and any of them could send an
  app identity headers, which are not signed (defect 6).
- A publishing proxy's own incoming rule is removed while the switch is
  off (`internal/controller/component_perimeter.go`). A pod that can
  reach a proxy can state its own client address and step round the
  rate limit. *The consequence is reasoned.*

**Never enforced, whatever the switch:**

- Outgoing connections from any kernel or system namespace (defect 7).
  The twelve policies restrict incoming connections only.
- Who may create an `HTTPRoute`, a `SecurityPolicy` or a `NetworkPolicy`
  (§7.3).
- That a secret store was made by the platform (decision 28).
- The rule on per-tenant L3 customizations (decision 49).
- That a default Component's digest is stated in a second place (defect
  11).
- That a person's sessions end when their groups change, and that
  offline tokens are off (defect 17).
- Any audit record of what is done with a kubeconfig (§6, API-server
  audit logging).

**Enforced by other rules, with the switch off or on:**

- A tenant's default deny, in and out
  (`internal/kernel/netpolicy/baseline.go`).
- A publishing proxy's outgoing rule to one Service and one port, and the
  tenant's rule that admits it.
- The rules of the shared stores, of the kernel's PostgreSQL (wide on the
  server's side, decision 46), of the model gateway, of the mail proxy
  and the mail servers.
- The operator's listener port
  (`charts/gentian-os/templates/networkpolicy.yaml`).

## 10. Plan changes made after the code

Where a plan was changed after the code to say what the code does. For
each: what it said before and the commit that holds that text, what it
says now, whether the change carries a date in the text, and whether the
owner confirmed it. "Not confirmed" means no answer of the owner to that
change was found; it does not mean the owner objects.

1. **[operator-split-plan.md](operator-split-plan.md): the whole
   document, and who writes the rights store.**
   - *Before* (`abb39901`, 2026-10-05): a target. The director is "the
     store's only writer"; the operator ends with no lifecycle HTTP
     listener; Argo CD has a read-only repository token of its own; the
     bouncer, the usher and the concierge hold nothing. There was no
     section "the cast", no table of what a break-in costs and no list
     of weaknesses.
   - *2026-10-06* (`eae8d14a`): "the director is the store's only
     writer" became "the operator writes the store", with the sentence
     that an earlier form of the plan "is not what was built". The same
     day `50b11570` wrote in that the operator keeps a listener for
     commands, and `be18263f` added the registrar with `manage-realm`.
   - *2026-10-07* (`9916b4f7`): the document was rewritten from the code
     and the charts, 1105 lines out and 1107 in. It now opens:
     "Everything here describes the code and the charts as they are."
     What the target had and the code has not is listed under "Not built
     yet".
   - *After that:* `a36583be`, `a75877d7` (2026-10-08, the operator's
     delete rights and the command that removes leftovers); `5e44b2d4`
     (2026-10-08, the desktop's key at the model gateway); `4e3d2137`
     (2026-10-09, the publishing proxy, the sign-in sidecar, the mail
     objects, the bouncer's second listener).
   - *Dated in the text:* no, except the two changes of 2026-10-09 named
     in entry 6 and AD-12.
   - *Confirmed by the owner:* the owner agreed on 2026-10-09 that AD-12
     names the operator as the writer of the rights store, and on
     2026-10-06 that the command listener is documented (on a reason that
     was not true, decision 35). The rewrite as a whole, and the
     additions after it, are **not confirmed**.
   - *What it means:* since 2026-10-07 this plan is a description of the
     code. It cannot be used to hold the code to anything. Section 8
     therefore also measures against the text of 2026-10-05.

2. **[networking.md](networking.md): a status table above §1, and the
   layers.**
   - *Before* (`649d14dd`, 2026-10-05): §1 with no qualification. §2,
     row L1′: `none`, `basic` and `signature` "are verified here and
     nowhere else", at the publishing proxy.
   - *Now:* a table "Status against the code, 2026-10-09" (`4e3d2137`)
     declares parts of §1 not built or built differently: TCP and UDP
     listeners, the director's `api.` address, the content filter, the
     mail box in the system DMZ. §1's text and diagram stand unchanged
     under it. Row L1′ says the modes "were to be verified here" and are
     withdrawn (`2d3d5c62`, `90663b8e`). New paragraphs describe the
     rules of the stores, the kernel database, mail and the model
     gateway as built, including that the kernel's rules are off by
     default and no kernel namespace denies outgoing connections
     (`23f8ab5f`, `754b250d`, `9273b577`, `34f4b87e`, `4fa056da`,
     `79f27463`). §5's rows for sync clients and webhooks went from
     verified at the proxy to "the app verifies". §4 gained
     `exchangeToken` (`82bf91c1`).
   - *Dated in the text:* the table's heading and the mail notes, yes.
     The rows and paragraphs, no.
   - *Confirmed by the owner:* **not confirmed**, beyond the decisions
     the changes point to (4, 5, 7).

3. **[networking.md](networking.md) §4: two rules became aims.**
   - *Before* (`7e0d77e3^`): "offline tokens are disabled, because they
     would survive it", and the rule that a membership change revokes
     sessions.
   - *Now* (`95794972`, `7e0d77e3`, 2026-10-09): "offline tokens are to
     be disabled … (an aim, not built: AD-12)", and "the aim stays".
     "Through the director" became "through the operator".
   - *Dated in the text:* yes.
   - *Confirmed by the owner:* yes for the aim (decision 3). Defect 17
     is what is not built.

4. **AD-1: an accepted limit.**
   - *Before* (`90663b8e^`): every request passes exactly one named
     enforcement point, with no limit stated.
   - *Now* (`90663b8e`, 2026-10-09): "One accepted limit": on
     `authMode: app` the publishing proxy does not check who calls, and
     a removed person's app credential works until the app revokes it.
   - *Dated in the text:* yes.
   - *Confirmed by the owner:* the entry type was the owner's answer
     (decision 5). The sentence that the limit is accepted was written
     with the build. **Not confirmed as written.**

5. **AD-6: who approves, and what the proxy checks.**
   - *Before* (`90663b8e^`): "the tenant's admins hold that relation by
     default"; a proxy "with that tenant's credential".
   - *Now* (five commits of 2026-10-09: `90663b8e`, `d6f8a82a`,
     `95e7ce06`, `b390b0f6`, `209834c9`): `authMode: app` added, four
     modes withdrawn, the proxy checks no caller, the tenant's admins
     hold the right only where a flag is on.
   - *Dated in the text:* yes.
   - *Confirmed by the owner:* in part. Decision 2 says which parts were
     the owner's answer and which were not.

6. **The registrar's right to write `MailboxRemoval`**
   ([operator-split-plan.md](operator-split-plan.md) §2, §4.4, §6 and a
   new weakness).
   - *Before* (`95794972^`): the registrar could write nothing in the
     cluster.
   - *Now* (`95794972`, 2026-10-09): it may create, update and delete
     `MailboxRemoval`. The right itself was built in `6a7e2cad` the same
     day.
   - *Dated in the text:* yes ("Changed 2026-10-09: until then it could
     write nothing in the cluster").
   - *Confirmed by the owner:* **not confirmed.** Decision 48.

7. **AD-13: two exceptions and one entry type.**
   - *Before* (`d045a521^`): no exception. "No platform component
     downstream runs a code flow or holds a client secret."
   - *Now:* the App Store app's own sign-in to a store outside the
     cluster (`d045a521`, 2026-10-07); the sign-in sidecar and its
     route without a session (`1ee03a8d`, 2026-10-09, which says the
     sidecar was built and declared by three apps before the entry said
     so); `clientAuthorization: app` (`90663b8e`, 2026-10-09).
   - *Dated in the text:* the first, no. The other two, yes.
   - *Confirmed by the owner:* the first was written after a conditional
     yes whose condition did not hold, without a second answer: **not
     confirmed**. For the second the owner asked for the text to be
     updated; whether the sidecar itself is wanted is decision 44. The
     third follows decision 5.
   - The copy of the edge's client secret into tenant namespaces is in
     no exception (decision 33).

8. **AD-12: the operator writes the rights store.**
   - *Before* (`0163d452^`): tuples "written by the director".
   - *Now* (`0163d452`, 2026-10-09): written by the operator; "the
     operator projects; the director asks"; and the admission that the
     operator also holds Keycloak's master administrator credential.
     [authorization-model.md](authorization-model.md) R7 was changed to
     match, dated.
   - *Dated in the text:* yes.
   - *Confirmed by the owner:* yes, on 2026-10-09. From 2026-10-06 to
     2026-10-09 the split plan and AD-12 contradicted each other.

9. **The clean-up commits `b88a061f` and `95794972` (2026-10-09).** The
   owner agreed to a pass that corrects old terms in the documents. What
   ran:
   - `b88a061f`: 28 files, 2393 lines added and 3025 removed. Design
     documents were rewritten, among them `architecture.md`,
     `admin-console.md`, `deployment.md`, `app-catalogue.md`,
     `agentic-ai.md`, `operations.md` and parts of `security.md`.
   - `95794972`: 15 plan files edited, among them
     `operator-split-plan.md`, `authorization-model.md`,
     `roles-and-authorizations.md` and the entries AD-4 and AD-14.
   - A side effect: the rewrite put statements into `admin-console.md`
     that were not there before — that the console must not make a
     backup key, and that scheduled backups are the Operations
     Console's — and the same pass then held the code against them
     (decision 25, decision 32).
   - *Confirmed by the owner:* **not confirmed.** The owner has not
     read these two diffs for statements that are new and not renamed.

10. **[authorization-model.md](authorization-model.md): the password
    policy.** Changed on 2026-10-10 (`3983be7b`) to say the registrar's
    action is the one way to set it, after the code. Corrected since;
    the text now records both the change and its reversal. §12.

## 11. What only a cluster can answer

Things the checks could not settle by reading. Each *needs a cluster*.

1. Whether provider-keycloak v2.19.0
   (`crossplane/providers/providers.yaml`) creates realms and clients
   against Keycloak 26.8.0
   (`kernel/services/keycloak-idp/manifests/values.yaml`).
2. Whether a binding between two apps becomes ready once it is granted,
   and how a waiver that is not granted shows on a tenant.
3. Whether the signature rule is on Argo CD's project after an install:
   it is rendered only when `keys.env` was there at step `B-01` (§8.9).
4. Whether Envoy Gateway serves a route openly in the gap before its
   policy exists, or when a policy is invalid (§7.3).
5. Whether a public path on an app's own host takes the signed-in app
   off that host (§7.2).
6. Whether the cluster's network enforces `NetworkPolicy` at all, and
   how it treats the rule for the Kubernetes API. The kernel's rules
   have never run.
7. The order of the filters, the cookies and the removal of headers in a
   running Envoy.
8. What the upstream charts of the apps create beyond what their
   profiles override.
9. On a tunnel cluster, what the tunnel publishes for a tenant's
   wildcard and for public hosts.
10. Which sign-in mode the deployed desktop and consoles run in, what
    the desktop's database Secret contains, and how far its key at the
    model gateway reaches.
11. What Argo CD's own controller may do: its ClusterRole comes from the
    upstream manifest, not from this repository.
12. Whether `manage-realm` is still granted on a realm made before
    `5fd8b15f`, and whether Crossplane's generated roles for its
    providers add anything.
13. How the first platform administrator gets into the administrators'
    group, and whether anything else adds members there.
14. Whether the director's private signing key remains on the machine
    that installed.
15. Whether a tenant namespace's default ServiceAccount mounts a token
    into the console pods that do not switch it off.
16. Whether an emptied password policy clears the realm's (defect 25).
17. Whether namespace names in some of the catalogue's hooks
    (`platform-kernel`, `gentian-infra-dev` in the Nextcloud profile)
    resolve to anything. *The names are confirmed in the catalogue
    repository.*

## 12. Closed recently

Closed in the code or in the documents. None has been seen on a cluster.

| Date | What | Commit |
| --- | --- | --- |
| 2026-10-19 | A private catalogue for a tenant's own administrator: a catalogue may be an OCI artifact read with the credential of a repository its owner declared, added under delegation like an address; the deployments-repository directory is deprecated (AD-14, decision 52 c) | `be746b35` |
| 2026-10-11 | The director commits a tenant's password policy again; the registrar's action for it is gone and its Keycloak client is no longer granted `manage-realm`; the tenant Composition always declares the realm's password policy. The documents say so (defect 25) | `24123aef`, `5fd8b15f`, `eec19bad`, `a84fadd7` |
| 2026-10-11 | The check that a per-tenant L3 customization runs in the tenant's own namespace is written again. It is still applied nowhere (decision 49) | `d7b25cd9` |
| 2026-10-11 | The store API documents, a test and a comment name no company and carry no commercial terms | `cbd3d530` |
| 2026-10-11 | Three entries of this list that were recorded as decided are recorded as open (decisions 23, 30, 32) | `5a265734` |
| 2026-10-10 | The `infra-chart-registry` credential is declared again. The installer's settings for it are not back (decision 50) | `afb15ac9` |
| 2026-10-10 | A tenant's bundle carries each app's own secrets and the keys under `secrets.derived` (decision 13). With the bundles of 2026-10-09 this closes what an export left out: mailboxes, rights, stored credentials | `fe3d387a`, `97760b13`, `3d10fc5a` |
| 2026-10-10 | The model gateway's console switch can be set through the director, and its network rule follows the claim (decision 21) | `20dc0c9f` |
| 2026-10-10 | A tenant's namespace reads the vault through a store of its own; the kernel's store is not usable from it (decisions 15, 27, 29) | `0175f322`, `7deab97e` |
| 2026-10-09 | AD-12 names the operator as the writer of the rights store; it no longer contradicts the split plan (§10.8) | `0163d452` |
| 2026-10-09 | A private catalogue is kept in the cluster's own deployments repository; nothing of a vendor's own catalogue has to be published (AD-14) | `8e1598e4` |
| 2026-10-08 | A tenant's administrator sees what newer builds of the tenant's apps left behind (decision 43 for what is still open) | `a75877d7` |
| 2026-10-08 | A cluster installed from a branch reads the development catalogue, where the bundles with their Compositions are; this ended a state in which a test cluster could not reach them | `47679acb` |
