# Gentian Cloud OS — Product & Security Roadmap

This document outlines the planned backlog of work for the Gentian Cloud OS. The items are structured hierarchically to allow development teams to easily ingest and refer to them.

For the current baseline design of the system, refer to [architecture.md](architecture.md).

---

## 1. Security & Hardening

### 1.1 Network Egress Hardening & Service API Boundaries (*)
* **Target Domain**: Platform Security & Networking
* **Context**: Default network policy configurations permit tenant workloads to access the entire service CIDR on port 443 (including the Kubernetes API server) and perform DNS lookups to the open internet. Furthermore, the network policy builder currently fails open (falls back to full capability sets) when an explicit AppGrant is missing.
* **Proposed Solution**: Upgrade the network layer to restrict tenant pod egress. Gate DNS queries to cluster CoreDNS IP addresses and restrict kube-API access to the active API server endpoint. Rebuild the policy logic to fail closed (deny egress) on missing AppGrants. Cilium will be evaluated to enforce FQDN/L7 egress filtering.
* **Backlog Items**:
  - `[ ]` Tightly restrict kube-API server egress (CIDR 443) on tenant namespaces to API server endpoint IPs.
  - `[ ]` Restrict external DNS egress to CoreDNS cluster IP addresses only.
  - `[ ]` Modify the network policy builder in `grants.go` and `integration.go` to fail closed (deny egress) when an AppGrant is missing.
  - `[ ]` Evaluate Cilium network policy configuration to enforce domain-scoped (FQDN) egress allow-lists.

### 1.2 Workload Identity & Service Mesh Integration (***)
* **Target Domain**: Platform Security & Service Mesh
* **Context**: Autonomous in-cluster agents and apps need a zero-trust communication channel. Standard edge gateways handle ingress TLS, but inside the cluster, workloads require dynamic, secret-less authentication and mTLS.
* **Proposed Solution**: Integrate SPIFFE/SPIRE to provide automated cryptographic identity issuance to pods. Couple SPIFFE ID generation with the platform operator's tenant namespace logic and enforce zero-trust traffic policies using a service mesh (e.g., Istio or Linkerd).
* **Backlog Items**:
  - `[ ]` Design the deployment model for SPIRE server and agent daemonsets.
  - `[ ]` Map `gentianos.io/app` and tenant labels to SPIFFE ID templates.
  - `[ ]` Wire service mesh traffic policy rules to platform integration bindings.
  - `[ ]` Enforce mutual TLS (mTLS) across all platform control plane and tenant communications.

### 1.3 Contract-Mediated Data Plane Access (***)
* **Target Domain**: Platform Security & Storage
* **Context**: Tenant workloads currently connect directly to underlying database and storage engines using standing credentials. This exposes credentials directly in the workload pods.
* **Proposed Solution**: Harden access by routing database and S3 requests through credential-less local sidecars or proxies that query a Policy Decision Point (PEP) to authenticate and validate access permissions on a per-query basis.
* **Backlog Items**:
  - `[ ]` Develop and configure a database proxy sidecar for PostgreSQL CNPG clusters.
  - `[ ]` Refactor catalogue apps to connect to database resources via localhost proxy paths.
  - `[ ]` Move database credential storage out of tenant namespaces into the operator's private domain.

### 1.4 Waiver Exclusion Verification Hardening (**)
* **Target Domain**: Platform Security & Admission Control
* **Context**: The exclusions keyed off a pod label (`mac-waiver.gentianos.io/<policy>: approved`) alone, which was not weak but a full bypass — any chart could exempt itself and the `PlatformSecurityPolicy` allowlist was never consulted by anything. The approval half was equally inert: approvals were recorded on the Tenant in an annotation nothing read, so a properly approved waiver was still denied.
* **Solution**: Each exclusion now requires the pod label **and** a matching label on the tenant namespace, which only the operator writes, from the allowlist intersection. Forging the pod label alone achieves nothing; revoking an approval removes the namespace grant.
* **Backlog Items**:
  - `[x]` Refactor Kyverno exclusion rules in `gentian-baseline.yaml` to evaluate namespace configuration (namespace labels, not service accounts — a `namespaceSelector` is native to Kyverno's exclusion matching and needs no API call per admission).
  - `[x]` Deprecate waiver checks relying on self-asserted pod labels — the pod label is now necessary but no longer sufficient.
  - `[ ]` Validate the updated PSP waiver checks against live workload deployments. Unit-covered; needs a cluster.
  - `[ ]` Have app pod templates carry the waiver label so approvals take effect. Until then the Tenant reports `AwaitingWorkloadOptIn` rather than a misleading `Approved`.

### 1.5 Gateway Routing & Listener Security (*)
* **Target Domain**: Platform Security & Gateways
* **Context**: Listeners are no longer unconditionally open: `withAllowedRoutes` in `gateway_platform_reconciler.go` sets `NamespacesFromSame` and widens to `NamespacesFromAll` only when `allowCrossNamespaceRoutes` is set. That is a switch, not a selector — when it is on, every namespace may attach again, and the `ReferenceGrants` are still broad. The hijacking risk is narrowed to the clusters that need cross-namespace routes, not removed.
* **Proposed Solution**: Secure the ingress edge by scoping listener `allowedRoutes` to specific namespace label selectors. Narrow down target namespaces in `ReferenceGrants` to prevent wildcard access.
* **Backlog Items**:
  - `[ ]` Replace the cross-namespace boolean with a label selector, so widening does not mean opening to all.
  - `[ ]` Replace wildcard namespaces in `ReferenceGrants` templates with specific named targets.
  - `[ ]` Verify that tenant routing configs cannot hijack administrative paths.

### 1.6 Keycloak Administrative Panel Isolation & Hostname Enforcement (*)
* **Target Domain**: Platform Security & Identity
* **Context**: The Keycloak master admin console is exposed externally at the cluster gateway edge, and the server runs with `hostname-strict=false`.
* **Proposed Solution**: Tightly lock down the `/admin` URL paths at the gateway layer, and configure Keycloak with strict hostname verification. Run administrative services on a private internal DNS zone.
* **Backlog Items**:
  - `[ ]` Restrict external edge access to `/admin` endpoints on the gateway configuration.
  - `[ ]` Set `hostname-strict=true` in Keycloak server configuration templates.
  - `[ ]` Run the Keycloak admin console on a dedicated internal-only hostname (e.g., `admin.kernel.local`).

### 1.7 Keycloak Refresh Token Rotation & Revocation (**)
* **Target Domain**: Platform Security & Identity
* **Context**: User access tokens are configured with long lifespans to prevent frequent login prompts. To balance usability with security, the refresh tokens must support rotation and explicit revocation to limit token theft risks.
* **Proposed Solution**: Enable refresh token rotation and revocation within the Keycloak realm defaults, and implement logout-triggered token revocation inside the portal BFF.
* **Backlog Items**:
  - `[ ]` Enable refresh token rotation (`revokeRefreshToken: true`) in default realm settings.
  - `[ ]` Implement active token revocation API calls in the portal shell BFF on user logout.

### 1.8 Cryptographic Entropy for App Secret Seeds (*)
* **Target Domain**: Platform Security & Secrets Management
* **Context**: App-internal seeds are generated deterministically as `sha256(xrName:app:secretName)`, which lacks sufficient entropy.
* **Proposed Solution**: Transition the secret generation mechanism to use HKDF-SHA256 (HMAC-based Key Derivation Function) or cryptographically secure random number generation (`crypto/rand`).
* **Backlog Items**:
  - `[ ]` Replace deterministic SHA-256 hash formatting with an HKDF-based key derivation function in Crossplane compositions.
  - `[ ]` Standardize all secret generation templates to consume keys from secure random generators.

### 1.9 Secure Dependency & Supply Chain Verification (**)
* **Target Domain**: Platform Security & Build Pipeline
* **Context**: Pinning is done. `versions.yaml` holds every external component the installer pulls, once, versioned with the platform rather than per cluster, and `validate_pins` fails when a step pins a component the file does not carry or the file carries one no step claims. Images are pinned by digest, and `make lint-image-digests` asks the registry whether each digest is a manifest list — a single-arch digest pins the supply chain and breaks the cluster on the next arm64 node, and the two are indistinguishable by inspection. What is not done is the mirror: every chart still comes from its upstream repository at install time, so an upstream that disappears or is tampered with is still a live dependency.
* **Proposed Solution**: Mirror the third-party charts the installer depends on, so the pins point at something we control.
* **Half-built already**: `credentials.yaml`'s `infra-chart-registry` entry and `install.env.template`'s `INFRA_CHART_REPO`/`INFRA_CHART_PRIVATE` are the credential, vault path, and `oci-registry` validator for exactly this — prompted for, validated, and seeded into OpenBao at bootstrap. Nothing reads `REGISTRY_USER`/`REGISTRY_PASSWORD` back out: no `helm install`/chart-pull call site in the A-phase or `charts/infra/` redirects through it yet. The mirror target is a real registry to point at; the credential to reach it already exists.
* **Backlog Items**:
  - `[x]` Audit and replace all remote mutable git/branch references with specific tags and commits. *(`versions.yaml` plus `validate_pins`.)*
  - `[x]` Pin chart and image dependencies to specific versions and digests. *(`make lint-image-digests` additionally rejects single-architecture digests.)*
  - `[ ]` Implement local mirror targets for all third-party charts. *(Redirect Crossplane, its providers, cert-manager, ArgoCD, and the Bitnami-derived `charts/infra/*` charts through `INFRA_CHART_REPO` when set — the credential side is already built, see above.)*

### 1.10 In-Cluster Secret Disclosure Prevention (*)
* **Target Domain**: Platform Security & Secrets Management
* **Context**: Administrative credentials and OpenBao root tokens are echoed to standard console logs during bootstrap, and the raw master key is persisted in plain text within cluster secrets.
* **Proposed Solution**: Sanitise installation scripts to prevent secrets from leaking into log aggregators. Leverage a KMS (Key Management Service) or HSM (Hardware Security Module) provider to securely wrap key material rather than storing the raw master key in-cluster.
* **Backlog Items**:
  - `[ ]` Refactor bootstrap helper scripts (`openbao.sh`, `install.sh`) to suppress secret logging.
  - `[ ]` Integrate external KMS providers (e.g., AWS KMS, HashiCorp Vault Transit) to wrap the cluster master key.
  - `[ ]` Remove the plain-text storage of unseal keys and master secrets from cluster-accessible namespaces.

### 1.11 Automated Annotation-Driven Secret Rotation (**)
* **Target Domain**: Platform Security & Secrets Management
* **Context**: There is currently no operator mechanism to handle credential rotation dynamically. Rotation must be done manually by modifying vault paths and restarting workloads.
* **Proposed Solution**: Extend the operator to watch for a rotation annotation on the `Tenant` CR (e.g., `gentian-os.io/rotate-credentials=all`). The controller will regenerate values in OpenBao and rely on ExternalSecrets (ESO) and Reloader to perform zero-downtime rolling updates.
* **Backlog Items**:
  - `[ ]` Implement the rotation annotation watcher inside the operator `Tenant` controller.
  - `[ ]` Implement the rotation engine that triggers secret regeneration in OpenBao.
  - `[ ]` Configure Reloader annotations on tenant apps to restart pods automatically upon secret changes.

### 1.12 SOC 2 Auditing & Compliance Instrumentation (**)
* **Target Domain**: Compliance & Audit
* **Context**: Platform audit logs, access reviews, and change management logs are not structured or aggregated to meet compliance framework requirements.
* **Proposed Solution**: Instrument audit log aggregation across the BFF, Keycloak administrative console events, and gateway access logs. Automate backup verification testing and establish formal audit trail storage.
* **Backlog Items**:
  - `[ ]` Setup a centralized audit log ingestion store.
  - `[ ]` Standardize logging format for administrative mutations across Keycloak and portal BFF.
  - `[ ]` Build automation to verify and test backup restores periodically.

### 1.13 App Catalogue Validating Webhook (**)
* **Target Domain**: Platform Security & Software Supply
* **Context**: The webhook exists — `internal/webhook/appprofile_validator.go`, `failurePolicy: fail`, on create and update — but it validates one thing: that every entry in `spec.categories` is in an allowed set. Registry, digest, `compositionRef` and sidecars pass unexamined. The admission point is built and wired; what it asks is close to nothing, which is worth stating plainly, because "an AppProfile webhook exists" reads as a control that is not there.
* **Proposed Solution**: Extend the existing validator rather than build a second one.
* **Backlog Items**:
  - `[x]` Implement an Admission Webhook targeting `AppProfile` CRD requests. *(Categories only — see Context.)*
  - `[ ]` Add validation checks for allowed registries and image digests.
  - `[ ]` Reject profiles specifying unauthorized sidecars or privileged configurations.
  - `[ ]` Verify `compositionRef` resolves to a Composition that exists.

### 1.14 Agent Identities & Token Delegation (RFC 8693) (***)
* **Target Domain**: Identity & Authorization
* **Context**: In-cluster autonomous agents need to perform actions on behalf of users. Currently, they lack a secure delegation model.
* **Proposed Solution**: Implement OAuth 2.0 Token Exchange (RFC 8693) in the Keycloak composite configuration to allow agents to obtain down-scoped, user-delegated access tokens.
* **Backlog Items**:
  - `[ ]` Configure token exchange policies within the Keycloak composite.
  - `[ ]` Implement down-scoped client scopes for agent authorization.
  - `[ ]` Wire the portal shell to delegate token exchange requests for registered agents.

### 1.15 Gateway External Authentication (AuthZEN PEP) (**)
* **Target Domain**: Gateway Security
* **Context**: External traffic routing relies on individual app authentication instead of a unified gateway policy enforcement point.
* **Proposed Solution**: Configure Envoy Gateway's `ext-auth` filters to query the AuthZEN PEP server, allowing access decisions to be made at the network edge before hitting application pods.
* **Backlog Items**:
  - `[ ]` Configure Envoy Gateway HTTPRoute filters to leverage external authentication.
  - `[ ]` Build a lightweight AuthZEN PEP helper that translates Gateway metadata to authz queries.

### 1.16 Crossplane Provider RBAC Scoping (**)
* **Target Domain**: Platform Security & Crossplane
* **Context**: `provider-kubernetes` and `provider-helm` authenticate as their own ServiceAccounts (`credentials.source: InjectedIdentity`) and are bound to `cluster-admin` by `crossplane/providers/provider-rbac.yaml`. The grant is currently necessary — Crossplane's generated per-provider role covers only the provider's own CRDs, so without it every composed `Object` fails to observe and the XCluster never reaches Ready — but it means any Composition, or a compromised provider pod, holds unrestricted control of the cluster. This is the widest standing privilege in the platform and sits outside the tenant isolation model that the rest of section 1 hardens.
* **Constraint — the two providers are not equally scopable.** `provider-helm` installs charts that ship their own `ClusterRole`s; Kubernetes refuses to create a role granting permissions the creator does not itself hold. Scoping it therefore requires either holding the union of everything every installed chart grants — which includes cluster-wide `secrets` read, from cert-manager among others — or holding `escalate` on `clusterroles`, which permits minting any role and is `cluster-admin` under another name. A scoped role derived from chart templates alone does not work: it breaks the charts that carry RBAC. The precondition for scoping `provider-helm` is that the platform owns the RBAC for the charts it installs (`rbac.create=false`, roles held in-repo and reviewed), which is a policy decision about third-party software rather than a provider change, and belongs with the kernel rights-management work. `provider-kubernetes` composes a small set of ordinary kinds — namespaces, config, secrets, jobs, routes, and the platform's own CRs — none of them RBAC, and can be scoped independently of any of that.
* **Constraint — no single source enumerates the kinds.** A running cluster shows only the paths it has exercised. The render fixtures show only the paths that have a fixture. A scan of the Composition templates shows fewer than either, because kinds sit behind conditionals. Each of the three omits kinds the others carry, so the list must be a union of them, and a CI guard must fail when a fixture carries a kind the role omits. A path that has neither a fixture nor a live object is reachable only by exercising it.
* **Proposed Solution**: Scope `provider-kubernetes` to an explicit `ClusterRole` generated from a committed kind list, with the CI guard above. Leave `provider-helm` on `cluster-admin`, and state the escalation-prevention reason in `provider-rbac.yaml` so the grant reads as a structural ceiling rather than as unfinished work. Revisit `provider-helm` only once chart RBAC is owned by the platform.
* **Note on the binding rename**: `roleRef` on a `ClusterRoleBinding` is immutable, confirmed with a server-side dry run against a real cluster. The scoped grant could not reuse the existing binding's name — it had to be a new object — so `provider-kubernetes`'s binding is `crossplane-provider-kubernetes-scoped` now, and the installer explicitly deletes the old `crossplane-provider-kubernetes-admin` before applying the new one. Reapplying the manifest alone would have left both bindings present, narrowing nothing.
* **Backlog Items**:
  - `[x]` Commit the `provider-kubernetes` kind list as data, unioned from the sources above — `crossplane/providers/provider-kubernetes-kinds.yaml`.
  - `[x]` Generate its `ClusterRole` from that list and replace its `cluster-admin` binding — `scripts/gen/gen-provider-rbac.py`, wired into `make gen-all` / `verify-gen`.
  - `[x]` Add a CI check failing when a render fixture carries a kind the list omits — `scripts/lint/lint-provider-rbac.py`, wired into `lint-shell`.
  - `[x]` State the escalation-prevention ceiling for `provider-helm` in `provider-rbac.yaml`, so its grant reads as a limit of chart-shipped RBAC rather than as work not yet done.
  - `[x]` Fail the install wait on a provider permission error instead of exhausting the timeout (`composed_permission_errors`, `scripts/lib/bootstrap.sh`). Surfacing the same on the XR's own status is still open.
  - `[ ]` Document the "add a Composition → extend the provider role" step in `docs/deployment.md`.
  - `[ ]` Own chart RBAC (`rbac.create=false`) as the precondition for scoping `provider-helm` — coordinate with the kernel rights-management overhaul rather than doing it here.

### 1.17 IMAP Transport Encryption with the Cluster CA (**)
* **Target Domain**: Kernel Mail Security
* **Context**: Dovecot serves IMAP with `ssl = no` and `disable_plaintext_auth = no`, so a mail password crosses the pod network in the clear on every login. The credential is password-equivalent and, for a mailbox, is the reset vector for every other account its owner holds — anything able to observe pod traffic (a sidecar, a CNI plugin, a node-level capture) sees a live one. The chart already implements the encrypted path behind `tls.enabled`, which issues a certificate for the in-cluster names, serves implicit TLS on 993 alongside STARTTLS on 143, and refuses plaintext auth outside TLS. It is off by default because turning it on without a certificate is worse than leaving it off: Dovecot exits when `ssl = yes` and the files are absent, and the same process serves LMTP, so a premature switch takes **inbound delivery** down rather than only retrieval.
* **Proposed Solution**: Apply the `gentian-ca` issuer chain from `kernel/manifests/cert-manager/cluster-issuers-selfsigned.yaml`, which is defined in the repo but not applied on any cluster. Let's Encrypt cannot serve this: the name clients dial is `dovecot-<env>.<ns>.svc.cluster.local`, which is not publicly resolvable, so the certificate has to come from the cluster's own CA. Every mail client must then trust that CA — a client that does not will fail to connect rather than fall back, because `disable_plaintext_auth` is set alongside. That trust distribution, not the Dovecot change, is the real work.
* **Backlog Items**:
  - `[ ]` Apply the `gentian-ca` ClusterIssuer chain and confirm the root CA Certificate reaches Ready.
  - `[ ]` Distribute the CA bundle to every mail client image, starting with the tenant Nextcloud pods.
  - `[ ]` Flip `tls.enabled` per cluster and verify LMTP delivery survives the restart before trusting retrieval.
  - `[ ]` Re-check the assumption that IMAP stays ClusterIP-only; exposing 993 through a gateway TCP listener needs a publicly-valid certificate instead.

### 1.18 Kubernetes Secret Encryption at Rest (**)
* **Target Domain**: Control Plane Security
* **Context**: The API server runs without `--encryption-provider-config`, so every Kubernetes Secret is stored base64-encoded in etcd rather than encrypted. Base64 is an encoding, not a protection: anyone with an etcd snapshot, a backup of one, or read access to the datastore holds every credential the cluster carries. This undercuts controls that are otherwise sound — ESO materialises OpenBao values into Secrets, so a credential protected by policy in OpenBao lands unprotected in etcd the moment it is consumed. It is the reason the mail passdb is specified to hold ARGON2ID hashes rather than passwords, and `lint-password-schemes` enforces that; but hashing is a mitigation for one credential class, not a substitute for encrypting the store.
* **Proposed Solution**: Add an `EncryptionConfiguration` with `aescbc` or a KMS provider ahead of the `identity` provider and restart the API server. Note the migration trap: enabling encryption does **not** rewrite existing Secrets, which stay readable in etcd until rewritten, so the change is incomplete without a `kubectl get secrets --all-namespaces -o json | kubectl replace -f -` sweep. Determine first whether the control plane is ours to configure — on a managed OpenStack control plane this may be the provider's, in which case the item becomes a procurement requirement rather than an engineering task.
* **Backlog Items**:
  - `[ ]` Establish whether the API server configuration is under our control or the provider's.
  - `[ ]` Define the `EncryptionConfiguration` and decide between `aescbc` and a KMS provider backed by OpenBao transit.
  - `[ ]` Rewrite all existing Secrets after enabling, and verify a fresh etcd read no longer returns plaintext.
  - `[ ]` Add the check to the install pre-flight so a cluster without encryption at rest is reported rather than assumed.

### 1.19 Replace Mail App Passwords with OIDC Token Authentication (**)
* **Target Domain**: Kernel Mail Security
* **Context**: Identities live in Keycloak and an OIDC login never yields a password, but IMAP clients expect one. The industry has settled this the other way: Google stopped accepting passwords for mail in May 2022 and Microsoft ends basic authentication for IMAP in December 2026, both in favour of OAuth tokens (XOAUTH2/OAUTHBEARER). App passwords are explicitly the fallback for clients that cannot do OAuth, not the multi-user default. Dovecot is already configured for the correct path — per-realm oauth2 passdbs pointed at Keycloak introspection — so the platform side is largely built. The blocker is the client: Nextcloud Mail's OAuth support covers hosted Google and Microsoft only.
* **Proposed Solution**: Track and, where useful, help land [nextcloud/mail#13317](https://github.com/nextcloud/mail/pull/13317), which implements generic OIDC/XOAUTH2 for any compliant provider and names Keycloak explicitly. It was ready for review on 2026-07-20 and reviewed by the Mail lead on 2026-07-21 (error handling, validation codes, test coverage); as of 2026-08-18 it is open, has no release milestone, and awaits code-owner approval — see [issue #12491](https://github.com/nextcloud/mail/issues/12491), which is assigned and marked in progress. Note [#12483](https://github.com/nextcloud/mail/issues/12483) is closed as *not planned* and is a duplicate; reading it alone gives the wrong impression. This cluster is an unusually good test bed for that PR — Keycloak plus Dovecot with introspection already configured — and validating it against a real third-party provider is plausibly the fastest route to a merge. Nextcloud's Community Conference is 2026-09-19/20 with Contributor Week immediately after, which is when stalled PRs tend to move.
* **Backlog Items**:
  - `[ ]` Run #13317 against this cluster's Keycloak and Dovecot and report results upstream.
  - `[ ]` Confirm behaviour on Dovecot 2.3.21 — 2.4 changed OAuth handling — and with Keycloak 26+ audience validation during introspection.
  - `[ ]` Retire the per-app password minting once token auth works for the webmail client.
  - `[ ]` Keep app passwords only for clients that genuinely cannot do OAuth (phones, Thunderbird), as Google and Fastmail do.

### 1.20 DKIM Key Rotation and Delivery Verification (**)
* **Target Domain**: Kernel Mail Security
* **Context**: Signing works. The operator owns an RSA-2048 key per tenant and one for the kernel domain, seeds them into Postfix ahead of the image, and publishes each public half from the same value that signs; `opendkim-testkey` reports `key OK` for every domain. What is missing is what happens afterwards. Keys are created once and never rotated, which is safe — a rotated key silently stops matching its published record — but leaves no answer to a compromised key. And `ALLOWED_SENDER_DOMAINS` is read once at Postfix start, so a new tenant receives mail immediately but signs only after a restart, which nothing currently triggers.
* **Proposed Solution**: A rotation that publishes the new public key under a second selector, waits for propagation, then switches signing to it — the standard two-selector rollover, which never leaves a signature without a matching record. For the restart gap, either have the operator roll the Postfix StatefulSet when the domain list changes, or move signing to a milter that re-reads its tables.
* **Backlog Items**:
  - `[x]` Emit KeyTable and SigningTable entries per tenant domain. *(The image builds both from the operator-supplied domain list; the operator owns the keys, the image owns the tables.)*
  - `[x]` Mount the tenant DKIM private keys into the Postfix Pod. *(Seeded from `postfix-dkim-tenants` into a persistent volume by an init container, ahead of the image's own generation.)*
  - `[ ]` Restart or reload Postfix when the tenant domain list changes, so a new tenant signs without waiting for an unrelated restart.
  - `[ ]` Surface the full DNS record — selector, `v=DKIM1` prefix and key — on tenant status rather than the bare key.
  - `[x]` Verify with a message to a major provider that the received headers report `dkim=pass` and `dmarc=pass`. *(Gmail, 2026-08-20: `dkim=pass header.i=@corp.gtn.host header.s=mail`, `spf=pass` for 37.156.43.16, `dmarc=pass (p=QUARANTINE dis=NONE)` — the quarantine policy evaluated and applied no disposition.)*
  - `[ ]` Decide a rotation story, using a second selector so signing and publishing never disagree.

---

### 1.21 external-dns Loses the MX Preference on Read (*)
* **Target Domain**: Kernel DNS
* **Context**: external-dns's Cloudflare provider does not preserve an MX record's preference when it reads the record back. Debug logging shows it holding a record Cloudflare serves as `10 mail.gtn.host` as `0 mail.gtn.host`, so a published preference of 10 never compares equal to what is observed and every reconcile plans a change. Cloudflare applies that as a delete followed by a create, so the name had no MX for a moment every minute — and a sender resolving in that window falls back to the tenant's A record, which is the portal, not Postfix. Every other record external-dns manages here converged and stayed put, which is what isolated it to MX. Worked around by asking for preference 0, which is what the provider reports whatever is actually stored, so desired and observed finally agree and it stops rewriting. Note what that does and does not do: the churn stops, but the published record keeps whatever preference it last had — 10 on this cluster — because the whole point is that external-dns no longer touches it. The record is valid either way; preference only orders one MX against another. That is sound only while each domain has exactly one MX, which is the case: preference orders one MX against another and there is nothing to order.
* **Proposed Solution**: Fix the read upstream so the preference survives, then publish a meaningful preference again. Until then the workaround holds, and the constraint it depends on — one MX per domain — should be checked rather than assumed if a backup MX is ever added.
* **Backlog Items**:
  - `[x]` Capture what external-dns reads back for the MX and compare it to the desired endpoint. *(Debug logging: `gtn.host 1 IN MX  0 mail.gtn.host` against a zone serving `10`.)*
  - `[x]` Stop the churn. *(Ask for preference 0, matching what the provider reads back; verified by four consecutive "all records are already up to date" cycles and zero changes over four minutes, from roughly three a minute indefinitely.)*
  - `[ ]` Report the lost preference upstream against the Cloudflare provider.
  - `[ ]` Alert on sustained record churn, so a non-converging reconcile is noticed without reading logs.
  - `[ ]` Guard the one-MX-per-domain assumption if a backup MX is ever introduced.

---

### 1.22 Settings That Still Reach the Cluster Without Passing Through the Claim (**)
* **Target Domain**: Platform Configuration
* **Context**: The claim is the source, read as a file before the cluster exists and as `gentian-cluster-config` afterwards. The mail settings now travel that way: the Cluster composition writes `mail.serviceMode` and `mail.egressHost` without restating a default — the XRD declares them and the API server materialises them onto the composite — and the operator reads both, demonstrated by making the two sources disagree deliberately. With the Helm value saying `external` and the ConfigMap saying `kernel`, the operator recreated a Job it creates only in kernel mode. Both cluster-level copies are deleted. What remains is structural rather than accidental: ApplicationSets are rendered by Argo CD before that ConfigMap exists, so their settings arrive as Helm parameters the installer writes onto the Application once and nothing re-applies. `make verify-claim-applied` reports when those disagree with the claim, which is the best available answer while the copy has to exist.
* **Proposed Solution**: For the remaining parameters, either give the ApplicationSets a source that can be read after the composition has run, or accept the copy and keep the check that makes its drift visible. The second is honest and cheap; the first removes the class.
* **Backlog Items**:
  - `[x]` Consume the mail settings in the Cluster composition rather than declaring them and stopping there.
  - `[x]` Collapse the deployments-values copies onto that source, so SPF and the mail mode cannot disagree.
  - `[x]` Set Postfix `myhostname` from the egress host, so HELO matches the PTR of the address it sends from.
  - `[x]` Pin Postfix to the node carrying the floating IP.
  - `[x]` Report a claim setting the live cluster does not carry. *(`make verify-claim-applied`.)*
  - `[x]` Report an operator image the cluster is not tracking. *(`make verify-image-updates`.)*
  - `[ ]` Do the same for the settings still passed only as installer-written Helm parameters — `tenancyMode`, `networkMode`, `platform` and the rest of the twenty.
  - `[ ]` Re-apply, or make Argo CD own, the Applications the installer writes once, so a parameter added after install is not missing forever.

  The cost of that last item is now measured rather than assumed. The
  `gentian-os` Application carried `image-list: gentianos=${GENTIAN_OS_IMAGE_REPOSITORY}`,
  a shell placeholder in a Helm template that nothing expands. The template was
  fixed, and the lint that catches that class passes — but the Application is
  written once by the installer, so the fix reached new installs and no existing
  cluster. argocd-image-updater looked for a registry by that literal name,
  found none, and *skipped* it: `images_considered=2 images_skipped=1
  images_updated=0 errors=0`, every two minutes, with a condition of *No errors*
  and the Application Healthy.

  ifk-w4h therefore ran a 17-hour-old operator through a day of merged fixes,
  including two that were verified against a binary that did not contain them.
  Nothing in the cluster said so; the only symptom was a retired Job that kept
  reappearing. One annotation patch fixed it, and the next cycle reported
  `images_considered=3 images_skipped=0 images_updated=1`.

  The lesson is the general one this item is about: a source-side lint cannot
  see an object the installer wrote once, and a reconciler that reports success
  for doing nothing will not tell you either.

---

### 1.23 A Condition Stays True While Its Reconcile Has Been Failing for Hours (**)
* **Target Domain**: Operator Observability
* **Context**: Tenant reconciliation runs its steps in order and returns on the first failure, so every condition after the failing one keeps whatever it last said. With the OpenBao auth step failing, `IdentityReady` went False and `MailReady` went on reporting `True` with a timestamp from the previous day — while the mail step had not run at all, and the DNS records, app passwords and signing tables it maintains were quietly unmaintained. There is no aggregate condition either, so nothing summarises "this tenant last reconciled successfully at T". The practical effect is that a reader checking whether mail is healthy is told yes by a value nothing has re-evaluated since it broke. Both bugs found on 2026-08-20 hid behind this: the symptom that surfaced was a DNS record not updating, several steps away from either cause.
* **Proposed Solution**: Distinguish "true as of the last successful evaluation" from "not evaluated this pass". Either stamp conditions with the reconcile generation and mark the untouched ones Unknown when a pass returns early, or carry a single Ready/LastReconcileSucceeded condition that goes False the moment any step does — so a stale True cannot read as a current one.
* **Backlog Items**:
  - `[ ]` Mark conditions not evaluated in a failed pass as Unknown, rather than leaving the previous value in place.
  - `[ ]` Add an aggregate condition naming the last successful full reconcile and the step that stopped the current one.
  - `[ ]` Alert on a tenant whose reconcile has been failing longer than one requeue interval, rather than waiting for a downstream symptom.


---

### 1.24 gentian-cluster-config Keeps Keys the Composition No Longer Writes (*)
* **Target Domain**: Platform Configuration
* **Context**: The ConfigMap is applied by provider-kubernetes, which patches rather than replaces, so a key the composition stops writing stays in the object indefinitely. On ifk-w4h 16 of its 26 keys are leftovers of that kind — `cnpg.*`, `network.*`, `secretMode`, `storageClass`, `tenant.initJob.*` — none read by anything today. The cost is not the storage. A stale key is indistinguishable from a live one by inspection, and reads as authoritative: `mail.serviceMode` sat there saying `kernel`, which is the correct answer, while nothing maintained it and the composition did not write it at all. That is exactly how it was mistaken for evidence that the mechanism was already working.
* **Proposed Solution**: Make the ConfigMap's contents a function of the composition and nothing else — replace rather than patch, or prune keys absent from the render — so its contents can be trusted as current. Failing that, the lint should compare the live object against the producer's key set and report leftovers, so they are at least named.
* **Backlog Items**:
  - `[ ]` Prune keys the composition no longer writes, or replace the object outright.
  - `[ ]` Report live keys the producer does not write, so a leftover cannot be read as current.
  - `[ ]` Decide whether the 16 present leftovers are dead or were readers that quietly regressed to a default.

---

### 1.25 Enforce Mail Rate Limits and Per-User Quotas (**)
* **Target Domain**: Kernel Mail Security
* **Context**: Neither exists. `Tenant.spec.mail.rateLimit` and `mail.quotaPerUser` were declared on the Tenant CRD and the XTenant XRD, described in mail.md's security section as enforced, and set to real values on two clusters — while nothing read either field. Postfix runs with `smtpd_client_message_rate_limit = 0`, its default of no limit, and Dovecot loads no quota plugin. The fields have been removed, because a setting that reads as configuration and does nothing is worse than an absent one: an operator reading either the schema or the documentation would conclude outbound abuse was capped. Note also that the mechanism the docs named would not have delivered what they promised — `smtpd_client_message_rate_limit` is per client IP, and every tenant reaches the same submission endpoint, so it cannot separate one tenant from another.
* **Proposed Solution**: For quotas, Dovecot's quota plugin with a per-user rule and the maildir backend, sized from the tenant's setting, plus `lmtp_rcpt_check_quota` so an over-quota delivery is refused at LMTP rather than accepted and lost. For rate limiting, a per-tenant measure that survives a shared submission endpoint — the authenticated SASL identity rather than the client address — which likely means a policy service rather than a stock `smtpd_*` parameter. Restore the claim fields only once something reads them.
* **Backlog Items**:
  - `[ ]` Load the Dovecot quota plugin and set a per-user rule from the tenant's setting.
  - `[ ]` Refuse over-quota deliveries at LMTP rather than accepting mail there is no room for.
  - `[ ]` Rate-limit per authenticated identity, not per client address, so tenants sharing the endpoint are actually separated.
  - `[ ]` Re-add the claim fields when a reader exists, and not before.
  - `[ ]` Report the quota a tenant is actually subject to on tenant status, so the answer does not have to be inferred from Dovecot's config.


---

### 1.26 Restore full management of the portal BFF client

The portal BFF client is adopted `Observe`-only, the one Keycloak client in
`tenant-default` that is not fully managed.

The live client has `standardFlowEnabled` and `implicitFlowEnabled` both false
while still carrying `redirectUris` and `webOrigins`. Keycloak stores that
combination; provider-keycloak refuses to write it:

    valid_redirect_uris cannot be set when standard or implicit flow is not enabled

Because Upjet plans from the observed object, the rejection does not depend on
what the Composition declares — any write re-validates the live object and
fails. Dropping the fields from the template and omitting `LateInitialize` from
`managementPolicies` were both necessary and neither was sufficient.

The fields are inert: with both redirect flows disabled Keycloak can never run a
redirect flow for this client, and `app/core/auth.py` uses it only as an
expected token audience for the ROPC grant. Clearing them on the live object is
therefore a no-op functionally, and it makes the object expressible.

To close: clear `redirectUris` and `webOrigins` on the `corp` realm client
`gentian-portal-bff`, then restore
`managementPolicies: ["Observe", "Create", "Update", "Delete"]` in
`crossplane/compositions/tenant-default.yaml`. The post-logout URIs need no
action — they are Keycloak's derived default, not stored (`attributes` is
empty).

### 1.27 Retire the realm script's kernel IdP write

`IdentityProvider kernel` is managed by `tenant-default`, and the broker-idp Job
no longer writes it. One writer remains: the realm script in
`identity_reconciler.go`.

It cannot simply be deleted. The broker-idp Job requires the IdP to already
exist and exits non-zero otherwise, and the Composition needs the tenant realm
before it can create anything in it — so something has to make the object first
on a brand new tenant. The realm script no longer restates the
first-broker-login alias, so the two no longer disagree; what is left is a
duplicated write, not a conflicting one.

To close: let the Composition create the IdP rather than adopt one, drop the
block from the realm script, and drop the broker-idp Job's existence check with
it. The ordering has to be shown to work on a tenant created from nothing, not
on `corp`, which has had the object since before any of this.

The composed `Client broker-<tenant>` stays `Observe`-only by design — see
docs/plans/tenant-composition-cleanup.md §8.

### 1.28 Tenant Separation Belongs to the API Server, Not the Console (***)
* **Target Domain**: Security & Isolation
* **Context**: Nothing in the Admin Console impersonates the signed-in
  administrator. Every call reaches the API server as
  `system:serviceaccount:platform-kernel:gentian-portal-gentian-portal` — each
  service builds its client with `load_incluster_config()` and no
  `Impersonate-User` header — and that account must be able to serve every
  tenant. RBAC therefore authorises *the console*, and cannot tell a tenant
  admin from a platform one: "demo's admin edits demo's policy" and "demo's
  admin edits the cluster policy" are the same request at the authorisation
  layer. What separates them is `resolve_admin_tenant`, `_require_platform_admin`
  and per-route filters on `spec.tenant` — application code. This is not
  specific to one resource; it is how every admin operation works today. The
  consequence worth stating plainly: **a bug in a route handler is a
  cross-tenant data bug, not a UI bug**, and no Kubernetes control would catch
  it. Scoping does not change this — a namespaced resource needs the same broad
  grant, because the console manages every tenant namespace.
* **Proposed Solution**: Give the API server the identity it is missing. The
  console derives `Impersonate-User` and `Impersonate-Group` from the OIDC token
  it has already validated, and the cluster carries per-tenant RBAC for the
  resources the console touches. Isolation then holds even when a handler
  forgets its filter, and the Kubernetes audit log names the person rather than
  the console — which is the same argument as §1.12's audit instrumentation,
  arriving through a different door.
* **What this costs, because none of it is free**:
  - The impersonation grant is itself powerful: a service account that may
    impersonate any user is a service account that may become a cluster admin.
    It has to be restricted by `resourceNames` to the tenant-admin groups, and
    that list has to stay correct as tenants come and go.
  - Per-tenant Roles and RoleBindings must exist for every tenant, created and
    removed with the tenant, which is new work in the provisioning path.
  - Cluster-scoped admin resources do not separate cleanly under RBAC:
    `resourceNames` restricts `get`, `update`, `patch` and `delete`, but not
    `create` (the name is in the body) or `list`/`watch` (there is no single
    name). `BackupPolicy` and `CredentialRequirement` both carry a `scope` field
    for exactly this reason, and both would need namespacing or a webhook to be
    enforceable rather than merely filtered.
  - Some console reads are legitimately cluster-wide — the app catalogue, the
    tenant list a platform admin sees — so impersonation cannot be applied
    uniformly, and deciding per call site is the bulk of the work.
* **Backlog Items**:
  - `[ ]` Decide which console operations are per-tenant and which are genuinely
    platform-wide; the split is the design, and the rest follows from it.
  - `[ ]` Add impersonation to the Kubernetes client layer, restricted to the
    tenant-admin groups by `resourceNames`.
  - `[ ]` Create per-tenant Roles and RoleBindings as part of tenant
    provisioning, so a new tenant is isolated without a manual step.
  - `[ ]` Namespace the admin resources that a tenant may edit, or gate them
    with a validating webhook — a cluster-scoped resource a tenant can `create`
    is not separable by RBAC alone.
  - `[ ]` Keep the application-layer checks after impersonation lands. Two
    independent controls is the point; removing one because the other exists
    returns to a single point of failure with extra steps.
  - `[ ]` Add a test that a tenant admin's token cannot read another tenant's
    resources with the route filters deliberately disabled — the assertion that
    the boundary has actually moved.

### 1.29 Retire the OIDC Pack Job's Bootstraps (*)
* **Target Domain**: Identity
* **Done**: the Job writes nothing the Composition owns. app-default composes the
  client, its default scopes, the client scope, one `ProtocolMapper` per entry in
  `pack.mappers`, the client `Role`, and the entitlement group's grant of that
  role as a group `Roles` with `exhaustive: false`. tenant-default composes the
  groups themselves. The Job's log is four lines, all "already exists".

  Every one of them adopted rather than being recreated — verified on corp, ids
  unchanged throughout: three mappers, one client role, five groups, and the
  role-mapping still on the group.
* **The rule that made it possible**: a Keycloak object whose id is a UUID cannot
  be adopted from `crossplane.io/external-name`, but it does not need to be. Given
  the *parent's* real id and no external-name at all, the provider resolves the
  existing object and records its id. Only the parent's id has to be right — and a
  Crossplane reference yields the parent's external-name, which is the id only
  when that parent is managed. The ClientScope is adopted by name and
  Observe-only, so its mappers take its observed id instead of a ref.
* **What is left**: the Job creates the client scope and the client when absent,
  and deletes mappers left corrupt by a much older failed run. The creates are
  bootstraps — the Job runs in the DataPlane stage while the App claim that
  composes them is created in AppsAndEdge, the stage after. Moving that ordering
  is what retires the Job; the cleanup has no declarative form and would move to
  a repair path or go.
* **Backlog Items**:
  - `[x]` Stop the Job configuring the client and attaching its default scopes.
  - `[x]` Compose the client scope, the mappers, the client role and the
    group-to-role mapping; stop the Job making any of them.
  - `[ ]` Create the client and its scope from the Composition, so the Job's
    bootstraps can go — which means the App claim existing before the identity
    stage waits on the Job.
  - `[ ]` Decide where the corrupt-mapper cleanup belongs.
### 1.30 Trim the Realm Script to What a Realm Cannot Express (*)
* **Target Domain**: Identity
* **Done**: `tenant-default` composes a managed `Realm` and it is the tenant
  realm's only writer — enabled, displayName, registrationAllowed, the eight
  browser security headers, and the twelve-hour access-token and session
  lifespans and gentian login theme that `UpdateRealmBrowserSecurityHeaders`
  used to apply. The realm Job restates none of it; the browser-security
  function runs only against the kernel realm, which no Composition covers.
* **The SMTP Job stays, and is not a gap.** It looked like one — publish the
  host, declare `smtpServer`, retire the Job. The host was never the blocker:
  `gentian-kernel-services` already publishes SMTP_HOST and it matches the realm
  exactly. The blocker is `auth`. The Job builds the whole block from a day-2
  credential and omits user and password entirely when auth is off, because
  Keycloak keeps a stored user and password even when auth is off and will use
  them again if it is ever flipped back on.

  Whether that credential exists is runtime state in a Secret, and a Composition
  can neither read a Secret nor render a block conditional on one. Under the
  boundary in docs/plans/tenant-composition-cleanup.md §6a that is discovery,
  which is the operator's — so the Job is correct rather than unfinished.
  `ensureTenantSMTPJob` already skips cleanly when no credential is supplied,
  which is why no such Job runs on ifk-w4h at all.
* **What is left** is smaller than it looked: the realm script's user profile
  attributes and its required-action toggles. Both are separate Keycloak APIs
  with no `Realm` field, so they need their own resource kinds or they stay.
* **Backlog Items**:
  - `[x]` Compose the realm, adopt it, promote it to managed.
  - `[x]` Declare `securityDefenses` and verify the headers through the Admin API.
  - `[x]` Remove the realm Job's restatement and the browser-security function's
    tenant-realm write.
  - `[x]` Establish whether SMTP can be declared. It cannot, and should not.
  - `[ ]` Decide whether the user profile attributes and required-action toggles
    have a declarative form worth using, or stay imperative like SMTP.
### 1.31 Bootstrap Validator Library: Missing `smtp` and No Automated Coverage (**)
* **Target Domain**: Platform Security & Credential Validation
* **Context**: `scripts/lib/validators.sh` covers the `phase: bootstrap` credential set (§10,
  `docs/plans/config-and-credential-cleanup.md`), and its own design table names `smtp` as one of
  the five bootstrap-phase types, probed by `openssl s_client -starttls smtp` then `AUTH LOGIN`.
  `run_validator`'s dispatch has no `smtp` case at all — an unimplemented type, not an untested one.
  It is silently unreachable today only because the sole `type: smtp` credential in
  `credentials.yaml` (`smtp-relay`) is `phase: runtime` and never reaches this dispatch; a future
  bootstrap-phase smtp credential would hard-fail every install needing it with "Unknown validator
  type". `internal/credentialmgr/validator.go`'s `smtpProbe` (the on-cluster, `phase: runtime`
  validator) already implements the real thing and its own comment claims to "mirror the shell
  validator" — which does not exist to mirror.

  Separately, none of the four implemented validators (`oci-registry`, `git-https`,
  `oidc-discovery`, `cloudflare-dns`) has an automated test. `oci-registry` and `smtp` have never
  been exercised at all; `git-https` and `oidc-discovery` were verified by hand against live
  endpoints once, in both directions, which is not repeatable and does not run in CI. There is no
  shell-test framework anywhere in this repository to build on — every existing shell-adjacent
  check in this class (`scripts/tools/verify-openbao-policies.sh`, the Go `fakeRelay` in
  `internal/credentialmgr/validator_smtp_test.go`) works by standing up a real throwaway service
  and asserting against it, not by mocking.
* **Proposed Solution**: Write `validate_smtp` for the shell validator library, and build a
  fake-server test harness reusable across all four validator types, following the pattern already
  established by `verify-openbao-policies.sh`.

  `validate_smtp` is the harder half. Driving an interactive STARTTLS-then-`AUTH LOGIN` exchange
  from bash via `openssl s_client` (coprocess, no Go stdlib to lean on) is materially more fragile
  than everything else in this file. The Go test suite hit the identical problem testing
  `smtpProbe` and deliberately stopped short of completing a handshake in its fake relay ("It never
  actually completes STARTTLS... which lets these tests cover the paths that matter without a
  certificate authority") — the shell tests should draw the same boundary: unreachable, a
  connection that does not speak SMTP, and a server offering no STARTTLS (which the validator must
  refuse to send credentials into, same as the Go version) are all real without needing a
  TLS-terminating fake relay in bash.
* **Backlog Items**:
  - `[ ]` Implement `validate_smtp` in `scripts/lib/validators.sh` and wire it into `run_validator`'s
    dispatch, matching the design table's probe (`openssl s_client -starttls smtp`, `AUTH LOGIN`)
    and the Go `smtpProbe`'s safety properties (refuse cleartext AUTH when STARTTLS is not offered,
    implicit TLS on port 465).
  - `[ ]` Build a small fake-HTTP-server test harness (a controllable-status-code Python responder,
    consistent with this repo's existing python3 tooling) and use it to test `validate_oci_registry`,
    `validate_git_https` and `validate_oidc_discovery` — pass, reject, unreachable, and (where
    applicable) not-found, both credentialed and credential-less.
  - `[ ]` Test `validate_smtp` up to the boundary above: unreachable, non-SMTP-speaking, and
    no-STARTTLS-offered.
  - `[ ]` Wire the new tests into a `make` target and CI, closing Phase 3 acceptance criterion 1
    ("none of the validators is automated").

## 2. Platform, Infrastructure & Lifecycle

### 2.1 Keycloak Provider & Crossplane Consolidation (*)
* **Target Domain**: Platform Infrastructure
* **Context**: Keycloak realms and OIDC clients were managed through a mix of Crossplane resources and manifest-bridge bootstrap Jobs, which splits configuration state and makes drift hard to see. **In progress** — the working record, including what each step verified and what it cost, is [plans/tenant-composition-cleanup.md](plans/tenant-composition-cleanup.md).
* **The stated precondition is met.** This item waited on "upstream provider versions supporting browser-flow tuning and broker integration". `provider-keycloak` v2.19.0 is installed and healthy and ships both: `flows`, `subflows`, `executions`, `executionconfigs` and `bindings` for authentication flows, and `identityproviders` plus `identityprovidermappers` for brokering. Adoption of existing objects also works — a `Client` or `Realm` carrying `crossplane.io/external-name` set to its natural key adopts what is already there rather than creating a duplicate, verified read-only against the live realm and both portal clients — so migrating an existing tenant needs no per-tenant import step.
* **Proposed Solution**: Move the tenant's Keycloak objects to `provider-keycloak` Managed Resources one Job at a time, adopting rather than recreating, and retire each Job only once its objects are seen to adopt without changing anything.
* **Backlog Items**:
  - `[~]` Replace bootstrap Jobs with native `provider-keycloak` Client/Realm MRs. *(2 of 9 retired: portal public client with its openbao-audience mapper, and portal BFF client with its client secret and default scopes. Remaining: dovecot OIDC client, gentian groups, kernel tenant broker, broker IdP, browser flow, realm admin, realm.)*
  - `[x]` Port OIDC client default scopes into Crossplane templates. *(All seven on the BFF client; the Job attached only `groups` because the rest were Keycloak's defaults, and the list is replaced wholesale — declaring the one the Job named would have stripped profile, email and role claims from every portal token.)*
  - `[ ]` Port the custom browser flow configuration. Note `keycloak-oidc-browser-*` is largely a one-time migration off a legacy flow; what remains ongoing is realm-level (`browserFlow`, `loginTheme`), so it belongs with the realm rather than before it.
  - `[ ]` Restore the assertion lost with `keycloak_portal_client_test.go` — that a tenant's own origin is a registered redirect *and* post-logout redirect URI. The render harness is golden-diff with no per-case assertion hook, so that property is currently only visible, not asserted.

### 2.2 Composition-Only IntegrationBinding Egress (***)
* **Target Domain**: Platform Infrastructure
* **Context**: The operator handles part of the `IntegrationBinding` logic (like network policies) programmatically, creating a hybrid lifecycle.
* **Proposed Solution**: Transition integration binding entirely to Crossplane Compositions. Gate deployment on the readiness of both consumer and provider, write connection credentials directly, and remove programmatic operator reconciliation loops.
* **Prerequisite, not listed when this was written**: "write connection credentials directly" assumes a mechanism that does not exist. `IntegrationBindingReconciler` writes the endpoint and credential into OpenBao at `secrets.ContractPath(...)` precisely because a Composition cannot mint a credential and store it. The same gap keeps `reconcileTenantApps` seeding app secrets (`seedAppPrerequisites`) — one missing capability blocking two items, so building it once settles both. The egress half is not blocked by it: `tenant_network_policy.go` derives its rules from `collectDesiredIntegrationBindings`, which is pure derivation and moves cleanly.
* **Backlog Items**:
  - `[ ]` Provide a declarative way to write a credential into OpenBao — a Managed Resource or a Composition pattern — since this item and app-secret seeding both wait on it.
  - `[ ]` Establish what creates the `IntegrationBinding` CRs today. `ensureIntegrationBindings` only waits for them and garbage-collects stale ones, and nothing in `crossplane/compositions/` names the kind.
  - `[ ]` Refactor `IntegrationBinding` logic to resolve exclusively within Crossplane compositions.
  - `[ ]` Remove the programmatically generated integration binding reconciliation code from the Go controller.

### 2.3 Event-Driven Messaging Bus (NATS & CloudEvents) (**)
* **Target Domain**: Platform Infrastructure
* **Context**: Applications and services currently communicate via point-to-point APIs. There is no messaging backbone for pub/sub operations.
* **Proposed Solution**: Deploy NATS JetStream as the cluster-wide messaging bus. Enforce per-tenant subject namespaces and mandate CloudEvents schemas for all asynchronous message payloads.
* **Backlog Items**:
  - `[ ]` Deploy NATS operator and configure JetStream.
  - `[ ]` Implement namespace-scoped subject isolation policies in NATS.
  - `[ ]` Integrate CloudEvents validation layers for inter-app communications.

### 2.4 OCI-Based App Catalogue Delivery (**)
* **Target Domain**: Software Supply
* **Context**: The app catalogue is delivered as an ArgoCD Application, which requires pulling raw YAML from Git repositories.
* **Proposed Solution**: Package and publish the `AppProfile` resources as OCI artifacts. Refactor the deployment logic to pull the catalogue directly from the registry using a custom controller or Crossplane `Cluster` XR.
* **Backlog Items**:
  - `[ ]` Build a packaging pipeline to compile `AppProfile` resources into OCI artifacts.
  - `[ ]` Refactor the App Catalogue setup to pull files directly from the registry.

### 2.5 Commercial App Entitlements & Licensing (***)
* **Target Domain**: Platform Billing & Business Logic
* **Context**: Pro apps can currently be listed by all cluster tenants, even if they have not been purchased or licensed.
* **Proposed Solution**: Extend the `AppCatalogue` and `Tenant` controllers to verify customer entitlements against CRM data, and reject installation requests for proprietary profiles unless valid licenses are found.
* **Backlog Items**:
  - `[ ]` Sync licensing/entitlement metadata to cluster ConfigMaps or CRs.
  - `[ ]` Extend the `Tenant` validating webhook to reject Pro app installations if a tenant lacks an active entitlement.
  - `[ ]` Restrict Pro images from being pulled unless namespace-scoped pull secrets are provisioned.

### 2.6 Office & Mail Composition Refactoring (**)
* **Target Domain**: Platform Infrastructure
* **Context**: ~~Postfix/Dovecot and Collabora are deployed and managed by the operator via hardcoded installation scripts.~~ **No longer true.** Postfix arrives through the `gentian-infra-helm` ApplicationSet and Dovecot through `kernel/appsets/raw/09b-dovecot.yaml`, generated only when `mail.serviceMode` is `kernel`; both are Helm charts synced by Argo CD. Collabora is a catalogue app, and the operator's only remaining knowledge of it is a routing default in `gateway_route_helpers.go`. `mail_reconciler.go` runs no `helm` and no `kubectl apply`: it registers tenants in a stack it does not deploy.
* **What is actually left**: per-tenant mail state — domains, app passwords, DKIM keys, realm SMTP — which the operator still owns. That is provisioning rather than installation, and it is not obviously misplaced: it is per-tenant, and it mints credentials, which is the same gap that blocks §2.2.
* **Backlog Items**:
  - `[x]` ~~Create Crossplane compositions for Dovecot/Postfix.~~ *(Solved differently: Argo CD ApplicationSets over Helm charts. A Composition buys nothing here — there is no claim to project into values, which is the one thing a Composition does that an ApplicationSet cannot.)*
  - `[x]` ~~Package Collabora/Office settings into standard Helm deployment templates.~~ *(Collabora is a catalogue app with its own profile in `gentian-apps`.)*
  - `[ ]` Decide whether per-tenant mail provisioning stays in the operator. If it moves, it moves for the same reason and by the same mechanism as §2.2.

### 2.7 Per-App HTTP-01 Certificate Issuance (**)
* **Target Domain**: Ingress & Networking
* **Context**: The gateway uses wildcard certificates generated via DNS-01 verification. Individual tenant apps cannot easily request HTTP-01 verified certificates.
* **Proposed Solution**: Extend the `AppProfile` spec to support HTTP-01 challenge definitions and dynamically provision Cert-Manager certs on demand.
* **Backlog Items**:
  - `[ ]` Add HTTP-01 challenge fields to the `AppProfile` API specification.
  - `[ ]` Update the ingress reconciler to map HTTP-01 challenge routes to Cert-Manager pods.

### 2.8 Database-Backed Marketplace Catalog (***)
* **Target Domain**: Software Supply & Catalog Management
* **Context**: In the short term, Git is the source of truth for the app catalog, requiring PR reviews for developer submissions to ensure quality control and audit logs. As the developer ecosystem scales, a Git-based workflow will become a bottleneck for updates.
* **Proposed Solution**: Migrate from a Git-based metadata store to a database-backed marketplace catalog managed by the commerce backend. Developers will upload and update their profiles via a developer dashboard portal, bypassing Git PRs entirely while keeping automated validation testbenches.
* **Backlog Items**:
  - `[ ]` Design the developer portal onboarding flow for app catalog submissions.
  - `[ ]` Define database schemas in Odoo/Postgres to store and version `AppProfile` manifests.
  - `[ ]` Implement automated quality check pipelines (testbenches) to validate submissions before publishing.
  - `[ ]` Update `app-store` and `gentian-os` cluster components to consume catalog APIs directly from the central database.

### 2.9 Third-Party App Developer Revenue Split (Stripe Connect) (***)
* **Target Domain**: Platform Billing & Business Logic
* **Context**: When external developers start publishing paid (Pro) applications on the Gentian Marketplace, a system is needed to automatically collect payments, deduct Gentian's commission, and distribute the remainder to the developer.
* **Proposed Solution**: Integrate Stripe Connect (Express/Custom) into the commerce backend's checkout pipeline. Allow developers to onboard as sub-merchants, and configure Stripe Checkout to dynamically split payments between Gentian (commission fee) and the developer (revenue cut).
* **Backlog Items**:
  - `[ ]` Integrate Stripe Connect Express onboarding flow for developers in the dashboard.
  - `[ ]` Implement split-payment execution in commerce-backend checkout sessions.
  - `[ ]` Update Odoo custom subscription billing modules to account for commission shares and developer payout records.

### 2.10 DNS TXT Record Verification for Custom Domains (**)
* **Target Domain**: Ingress & Domain Security
* **Context**: When a tenant registers an organization using a custom domain (e.g. `acme.com`), there is no check to ensure they actually own or control it. This can lead to domain conflicts, namespace hijacking, or incorrect routing.
* **Proposed Solution**: Introduce a DNS TXT verification challenge. Before a custom domain configuration is activated on the cluster ingress, the commerce backend issues a unique token (e.g., `gentian-verification=token`) that the customer must publish in their DNS records. The operator verifies the presence of the TXT record before routing HTTP traffic.
* **Backlog Items**:
  - `[ ]` Implement a DNS TXT challenge generator in the commerce backend API.
  - `[ ]` Update the cluster operator ingress controller to query and verify TXT challenge records before binding host ingresses.

### 2.11 Zero-Hurdle Demo Sandbox Launcher (***)
* **Target Domain**: Platform Trial & Engagement
* **Context**: The current demo flow requires email signup, email confirmation, and manual login credentials setup, which introduces friction for new evaluators.
* **Proposed Solution**: Enable instant "one-click" anonymous demo accounts. The SaaS portal will provision a sandboxed workspace instantly, generate a temporary login JWT, and redirect the user straight to the portal in-browser without requiring signup. If the user wishes to save their progress, they can register their email and convert it to a permanent tenant.
* **Backlog Items**:
  - `[ ]` Build anonymous ephemeral Keycloak session authorization handlers.
  - `[ ]` Implement the instant sandbox provisioning gateway in the demo engine.

### 2.12 Community Serverless Isolation (Auto-Sleep / Scale-to-Zero) (***)
* **Target Domain**: Resource Optimization & Tenant Isolation
* **Context**: Grouping community users in a single shared cluster namespace avoids resource overhead but compromises data isolation and privacy.
* **Proposed Solution**: Give community users their own individual isolated namespace, but configure the operator to automatically shut down or sleep (scale to zero) their workloads (Nextcloud, Element, Keycloak clients) after 15 minutes of inactivity. When a user requests their domain again, the ingress controller intercepts the request, wakes up the pods dynamically, and serves the page.
* **Backlog Items**:
  - `[ ]` Write an auto-sleep / scale-to-zero controller in the cluster operator.
  - `[ ]` Configure the ingress proxy to intercept requests for sleeping namespaces and trigger container wakeup.

### 2.13 Automated Customization Readiness Grading (**)
* **Target Domain**: App Catalogue & Customization Framework
* **Context**: The customization ladder (see [app-customization.md](app-customization.md)) grades each catalogue app **A–D** on how far up the ladder it can be customized, which determines whether a requested change lands at L1, L3, or L5. For v0.4 the grade is assigned by hand by the catalogue maintainer and recorded in `AppProfile.spec.customization.grade`; CI only checks that a grade is present and that it matches the recorded rubric score. Hand-assigned grades drift as upstreams evolve, and nothing currently detects that drift.
* **Proposed Solution**: Automate the mechanical subset of the §4.1 rubric in `gentian-apps` CI and re-score on every chart version bump. Criteria such as "declared drop-in directories exist in the image", "published API spec resolves", and "plugin API version is pinned in the test matrix" are machine-checkable; judgement criteria ("upstream accepts patches", "ABI survives minor releases") stay manual with a maintainer attestation and an expiry date. CI proposes a grade change as a PR comment rather than mutating the profile.
* **Backlog Items**:
  - `[ ]` Implement the mechanical rubric scorer over `spec.customization` and the resolved chart/image.
  - `[ ]` Add drift detection: re-score on chart version bumps and open a PR comment when the computed banding differs from the declared grade.
  - `[ ]` Add expiry to manual attestations so judgement criteria are revisited at a fixed cadence.
  - `[ ]` Feed grade history into the customization debt report (Admin Console).

### 2.14 Third-Party Customization Delegation (***)
* **Target Domain**: App Catalogue & Partner Ecosystem
* **Context**: The customization framework is authorship-neutral by design — `Customization.spec.origin` already records whether a customization was authored by Gentian, a tenant, a supplier, or a partner, and whether the backing repo is Gentian-owned or external. v0.4 ships the data model only; Gentian still owns every roadmap and repository on the ladder.
* **Proposed Solution**: Delegate repository and roadmap ownership to app owners and partners, and push the ladder's practices and definitions towards a published specification others can implement. Requires artifact signing and provenance, sandboxing for third-party L3 modules, an entitlement model for commercial customizations, and a delegated-maintainer role in the approval matrix.
* **Backlog Items**:
  - `[ ]` Define signing/provenance requirements for externally built customization artifacts.
  - `[ ]` Design sandboxing for third-party in-app (L3) modules.
  - `[ ]` Add a delegated-maintainer role to the customization approval matrix and RBAC.
  - `[ ]` Publish the ladder and record schema as an external specification.

### 2.15 Remove LiteLLM Specifics from the Kernel (**)
* **Target Domain**: Platform Infrastructure
* **Context**: `internal/controller/app_reconciler.go`, `kernel_gateway_routes.go` and now `litellm_team.go` hardcode one application: its Service name (`litellm-proxy`), base URL, master-key Secret, and direct calls to its `/key/generate`, `/key/info`, `/team/list` and `/team/new` HTTP APIs. The team endpoints moved *into* the operator deliberately, from a shell loop that converged per-tenant state only when someone re-ran the installer — a real improvement to correctness that also deepened exactly the coupling this item is about. Both things are true, and the fix for the second is this item, not a reversal of the first. This is the same class of boundary violation as the per-app privileged-role provisioner removed in §6h of the app-profile-guide — an application the kernel recognises by name is an application the kernel must be modified to support. LiteLLM is genuinely dual-natured, which is why this was never obviously wrong: it runs as a kernel singleton from `kernel/services/llm/`, and *also* ships a `litellm-me` catalogue profile. The kernel may know its own services; it must not know catalogue entries.
* **Proposed Solution**: Decide which LiteLLM is — kernel service or catalogue app — and make the code say so. If it is a kernel service, express it the way the other kernel services are (discovered configuration rather than compiled-in constants, in the same shape as SMTP/S3/Keycloak endpoints) so a cluster can run a different LLM gateway or none. If it is a catalogue app, the virtual-key provisioning belongs in its profile via the generic `spec.provisioning.syncJob` mechanism, and the kernel keeps only the generic credential plumbing.
* **Backlog Items**:
  - `[ ]` Classify LiteLLM as kernel service or catalogue app and record the decision in `architecture.md`.
  - `[ ]` Replace the `litellmProxy*`/`litellmMasterKey*` constants with values resolved from kernel service configuration — through the `valueMapping` contract an app uses to declare a need, not through endpoint substitution: `${S3_ENDPOINT}` and friends were removed for exactly the reason this item exists.
  - `[ ]` Move virtual-key issuance (`/key/generate`, `/key/info`) and team sync (`/team/list`, `/team/new`) out of the operator — to the app's own profile if it is a catalogue app, or behind a named kernel-service interface if it is not.
  - `[ ]` Drop the app-catalogue `litellm` special-case in `kernel_gateway_routes.go`.
  - `[ ]` Add a CI check that fails when a catalogue app name appears in gentian-os source, so this class of drift is caught rather than reviewed for.

### 2.16 A Bigger Plan Buys No More Users (**)
* **Target Domain**: Resource Optimization & Tenant Isolation
* **Context**: A tenant that grows from five users to five hundred runs byte-identical pods. Nothing in the tenant path autoscales — there is no HPA anywhere outside the vendored Redis chart, none exists on any cluster, every tenant Deployment is `replicas: 1`, and `AppProfile` has no replica or scaling field at all (its only `scale` references pause writes for a backup). What a plan sets is a **namespace** ResourceQuota, and what binds first under load is the **pod's own** limits: Nextcloud runs at `limits: 1 CPU / 1Gi` whether the workspace is on `base` or on `nodes-16`. So the plan gates how many apps may be installed, how much storage is available and the namespace ceiling — not how many people the workspace can serve. Buying more does not make Nextcloud faster or admit more concurrent sessions; what degrades is PHP-FPM worker saturation, database connections and Collabora document sessions, none of which the quota can see. The platform meanwhile already counts users: `MeteringWorker` reports `activeUserCount` per app from Keycloak group membership. User count is billed and capacity is billed, and nothing connects them. Storage is the one dimension that does track users, and a tenant will meet that limit honestly. See [resource-plans.md](design/resource-plans.md).
* **Proposed Solution**: Make the plan set what an app *gets*, not only what the namespace *permits*. Vertical sizing first: a per-app requests/limits figure that scales with the tenant's plan, so reserved capacity is actually consumed by the thing the tenant is paying for, and it applies uniformly — including to PostgreSQL and everything else that cannot be replicated. Horizontal scaling is worth having but is not a platform-wide switch: Nextcloud tolerates it with shared storage and Redis sessions, Collabora tolerates it, a single-writer database does not, so it belongs per-app behind a capability the profile declares. metrics-server now serves `metrics.k8s.io`, so HPA is available for the first time; the usage sampler already records committed and actual consumption per tenant, which is the evidence a sizing rule should be derived from rather than guessed. §3.6 resolving an app's effective requirements is the prerequisite for all of it — a per-plan multiplier needs a baseline to multiply.
* **Backlog Items**:
  - `[ ]` Decide and record whether a plan scales apps vertically, horizontally, or both, and say so in `resource-plans.md` — today it silently does neither, which is the part that misleads.
  - `[ ]` Add a per-app sizing field to `AppProfile` expressed relative to the plan, rather than absolute values that would have to be restated per tier.
  - `[ ]` Apply the resolved sizing through the same path that writes a tenant's quotas, so one change of plan moves the ceiling and the workloads together.
  - `[ ]` Raise the tenant `LimitRange` maximum (`4 CPU / 8Gi` per container) in step with the plan; it currently caps every container regardless of what was bought.
  - `[ ]` Declare horizontal scalability per app in `AppProfile` and attach an HPA only where the app claims it, so nothing replicates a single-writer database.
  - `[ ]` Derive the sizing rule from `tenant_resource_samples` rather than from a guess, once the series covers a period with real user growth in it.
  - `[ ]` Warn in the Resources tab when a plan change alters no workload, so a tenant is never sold headroom that changes nothing for them.

### 2.17 Deduplicated, Incremental Bundles (**)
* **Target Domain**: Backup & Disaster Recovery
* **Context**: Every export writes a full bundle. A tenant with a 483 MiB Nextcloud volume costs that much per night — about 14 GB a month, of which nearly all is byte-identical to the night before. The log-spaced tiers (`BackupPolicy.spec.retention`) reduce how many bundles are *kept*, not how much each one *costs to write*, so the transfer and the storage bill both scale with frequency rather than with change. This is the single largest inefficiency in the backup path and it gets worse as tenants grow, which is the opposite of how a backup regime should age.
* **The trade being made deliberately today**: a bundle is a plain `age` file beside an unencrypted `bundle-info.json` naming the exact command that opens it, so recovery needs no Gentian tooling and no Gentian process that still exists. A content-addressed repository (restic, kopia) is opaque by construction: recovery needs that tool and its key. That property was chosen once and should be re-chosen consciously, not lost as a side effect of wanting smaller backups.
* **Proposed Solution**: Adopt restic or kopia rather than building one — deduplicating, encrypting, content-addressed storage with verification is a decade of other people's bug fixes, and a bespoke implementation would be the least-tested component in the recovery path. Treat it as a second bundle *format* selected per policy, so the plain-`age` format stays available for tenants who want an archive they can open with a standard tool, and the deduplicated format is chosen where volume justifies opacity.
* **Interaction with Object Lock, which is not obvious**: full bundles suit WORM storage because no object is ever rewritten — only whole prefixes expire. `restic prune` and `kopia maintenance` both rewrite and delete pack files, which a compliance-mode lock forbids until expiry, so a deduplicated repository on locked storage must run append-only with maintenance performed elsewhere. Any tenant wanting both needs this settled before either is promised.
* **Backlog Items**:
  - `[ ]` Measure real change rates per app from the existing bundles before choosing, so the saving is known rather than assumed — a mostly-static volume may not justify the opacity at all.
  - `[ ]` Decide restic vs kopia against Object Lock and append-only support specifically, and record the reasoning where the backup design lives.
  - `[ ]` Express the format as a field on `BackupPolicy` so both can coexist, rather than migrating every tenant to one.
  - `[ ]` Keep `bundle-info.json` unencrypted and format-aware, so an operator holding only the bucket can still tell what a prefix is and which tool opens it.
  - `[ ]` Define what a restore drill means for a deduplicated repository — verifying a snapshot restores is not the same as verifying the repository is intact.

---

### 2.18 A Flaky envtest Wait Silently Withholds the Image (*)
* **Target Domain**: Build and Release
* **Context**: Four different envtest tests timed out on 2026-08-21, in three
  separate CI runs, each at the shared 3-minute `envtestWaitTimeout`, each
  passing locally and on re-run:
  `TestIdentity_DeleteDeletePolicy_CreatesCleanupJob`,
  `TestCache_DeleteDeletePolicy_CreatesDeleteJobsAndDeletesApplication`,
  `TestMariaDB_DeleteDeletePolicy_CreatesDeleteJob` and
  `TestDeletion_EndToEnd_WithApps`. Every one of them waits on a delete path.
  That is not the signature of a slow runner, which would strike waits at
  random; it points at the deletion path specifically.

  The cost is not the red X. `Docker build and push` is gated on the Go job, so
  a flake means no image is published for that commit and the cluster keeps
  running whatever it ran before. That is invisible unless someone reads the
  run: the branch looks merged, the Application is Healthy, and
  `make verify-image-updates` correctly reports the cluster tracks CI, because
  it does — there is simply nothing new to track. Three commits today were
  affected, and on two occasions work was verified against a binary that did
  not contain it.
* **What was ruled out**: not simply a loaded runner. The package runs in ~60s
  locally and ~195s in CI; forced to comparable slowness with `GOMAXPROCS=1` it
  took 225s — slower than CI — and passed, twice, with no wait coming close to
  its deadline. `markJobCompleteWhenReady`, the goroutine several of these tests
  rely on to unblock a sequential delete chain, carries its own 60s deadline
  against a 180s wait and gives up silently; that mismatch is real and worth
  fixing, but it was instrumented under load and never fired. So the cause is
  still open, and it is not "CI is slow".
* **Proposed Solution**: Reproduce it before fixing it. The delete path is
  strictly sequential — each cleanup Job must complete before the next is
  created — so a single Job whose completion is missed stalls the chain for the
  full timeout, and that shape fits every observed failure. Suspect the harness
  that completes Jobs, not the operator.
* **Backlog Items**:
  - `[ ]` Reproduce a delete-chain stall, and find which Job stopped being
    completed.
  - `[ ]` Give `markJobCompleteWhenReady` the same deadline as the wait it
    serves, and make it say so when it expires.
  - `[x]` Make a flake's cost visible: `make verify-image-updates` now reports
    when the branch is ahead of the running image, and what each intervening
    commit's CI concluded.
  - `[ ]` Decide whether `Docker build and push` should depend on the Go job.
### 2.19 Extend the RBAC Lint to Verbs (*)
* **Target Domain**: Build and Release
* **Context**: `make lint-rbac-coverage` asserts the operator may read every
  GroupVersionKind it constructs, which is the failure that has happened twice —
  a missing rule. It does not check verbs, and that gap is not theoretical
  either: the ClusterRole granted `pods` `get, list, create, delete` and not
  `watch`, so the manager's cache logged `pods is forbidden ... Failed to watch`
  on a loop and never synced, while a direct get kept working. The permission
  looks sufficient right up until something depends on the cache being current.
  Found by reading the operator's log after the lint itself was green.
* **Proposed Solution**: Decide which verbs a use implies and check those. A read
  through the manager's cache needs `list` and `watch`, not just `get` — that
  single rule would have caught this one. Going further means knowing which call
  each GVK reaches, which is a larger analysis than the current scan does.
* **Backlog Items**:
  - `[ ]` Require `list` and `watch` wherever a type is read through the cache.
  - `[ ]` Report a granted verb no call site uses, so the ClusterRole shrinks as
    the operator stops writing what Crossplane now owns.

## 3. User Management & Shell UI

### 3.1 SCIM & Provisioning Bus Integration (*)
* **Target Domain**: User Provisioning & Identity
* **Context**: User provisioning changes are updated directly in Keycloak but do not propagate to catalogue apps.
* **Proposed Solution**: Build a SCIM (System for Cross-domain Identity Management) listener that translates user/group events to CloudEvents, allowing tenant apps to sync their directories.
* **Backlog Items**:
  - `[ ]` Build a SCIM-compliant endpoint inside Keycloak or the operator.
  - `[ ]` Emit user and group changes as CloudEvents onto the NATS messaging bus.

### 3.2 Fine-Grained OpenFGA launch Authorization (*)
* **Target Domain**: UI/UX & Access Control
* **Context**: The relation is modelled and enforced, but not per tile. `can_launch` exists on the `shell_app` type in `internal/authz/data/model-v0.json`, resolving through tenant membership and tenant admin, and `gentian-ui/backend/app/core/authz.py` checks it — against the single object `shell_app:gentian-ui`, as an all-or-nothing gate on reaching the shell at all, and short-circuited for platform admins, tenant admins and the bootstrap admin. So the store answers "may this user open the portal", not "may this user see this tile". Every tile a tenant has installed is still rendered to every member.
* **Proposed Solution**: Write one `shell_app` object per installed app and query per tile, rather than once for the shell.
* **Backlog Items**:
  - `[x]` Implement `can_launch` relation rules inside the OpenFGA authorization store. *(Modelled on `shell_app`, with tenant member and admin inheritance.)*
  - `[ ]` Write a `shell_app` object per installed app, so the relation has per-app subjects to answer about.
  - `[ ]` Refactor the portal UI to filter tiles on the per-app answer.

### 3.3 Constrained Platform Admin Mode (**)
* **Target Domain**: UI/UX & Platform Access
* **Context**: Platform admins have complete administrative visibility over all tenant data.
* **Proposed Solution**: Configure a `platformAdminMode: constrained` mode where platform admins can only see system metadata and system logs, but cannot read tenant data or manage member records.
* **Backlog Items**:
  - `[ ]` Enforce tenant-scope path checks on the Admin Console BFF routes.
  - `[ ]` Split Keycloak administrative groups into operational roles (e.g., system-operator vs tenant-admin).
  - `[ ]` Deny cross-tenant administrative actions unless a break-glass role is explicitly active.

### 3.4 Tenant Administrator Invitations (**)
* **Target Domain**: Identity & Tenant Onboarding
* **Context**: Provisioning a tenant creates one account — `admin@<tenant>.<kernel-domain>` — whose password is derived from the cluster master. It is the only way into a new tenant, so it becomes the account the tenant's real administrators log in with day to day: a shared credential, attached to no person, that appears in the audit trail as itself no matter who acted. It also cannot be recovered by the tenant, since recovery runs through the cluster administrator by design.
* **Proposed Solution**: Make the provisioned account a bootstrap credential rather than a working one. A cluster administrator invites named people into the tenant's realm; each accepts, sets their own credentials, and holds tenant-admin through group membership. The provisioned account is then reduced to break-glass, and its use is an event rather than a routine.
* **Backlog Items**:
  - `[ ]` Add an invitation flow to the Admin Console: address in, Keycloak invitation out, membership granted on acceptance.
  - `[ ]` Grant tenant-admin through realm group membership so it survives the bootstrap account being disabled.
  - `[ ]` Report in the portal when a tenant still has no named administrator, so the state is visible rather than assumed.
  - `[ ]` Decide what happens to the bootstrap account once a named admin exists — disabled, or retained as break-glass with its use audited.

### 3.5 Shell Browser Egress Proxy (**)
* **Target Domain**: UI/UX & Shell
* **Context**: Embedded iframe applications make direct cross-origin API calls from the browser, exposing tokens to the user's browser context.
* **Proposed Solution**: Build a reverse proxy within the shell BFF that maps paths (e.g., `/api/apps/{name}/...`) and handles bearer tokens server-side.
* **Backlog Items**:
  - `[ ]` Build reverse-proxy routing within the shell BFF.
  - `[ ]` Implement server-side bearer token injection for outgoing app requests.

---

### 3.6 Effective Resource Requirements in the Catalogue (**)
* **Target Domain**: UI/UX & Shell
* **Context**: The app store shows a "Resource Profile" taken from the AppProfile's `extraValues.resources`. That field is the profile's *override* block, not the app's requirement, so an app content with its chart's defaults declares nothing and the store had nothing to show — Docmost reserves 1 CPU and 1Gi from `charts/docmost/values.yaml` and appeared to reserve nothing. Of seventeen profiles, fourteen declare no override; most legitimately (the nine Nextcloud add-ons install into an existing Nextcloud and start no pods, gentian-subscriptions is an ApiProfile with no workload), but docmost, mathesar, litellm and the app store itself all run pods on chart defaults. A tenant admin deciding what fits sees a blank where a whole core belongs. Mitigated for now by saying "not set by this profile — the chart's own defaults apply" instead of omitting the panel, so a blank is never read as free; the tenant resources panel shows the truth, but only once the app is installed, which is after the decision.
* **Proposed Solution**: Resolve the *effective* resources at catalogue-build time rather than reading the override block — the chart is already pulled, so its values are available — and serve requests and limits whether or not the profile overrides them. Copying the numbers into each profile is the alternative and the wrong one: it duplicates the chart's own values and drifts the first time a chart is bumped.
* **Backlog Items**:
  - `[ ]` Resolve requests and limits from the chart's values when the profile declares no override.
  - `[ ]` Cover the shapes charts actually use — per-component blocks (`api.resources`, `web.resources`), sub-charts, and sidecars — rather than a single top-level `resources` key.
  - `[ ]` Show the resolved figures in the same units as the tenant resources panel, so what an app *will* cost and what installed apps *do* cost read as one system.
  - `[ ]` Warn in the install dialog when an app's requirement exceeds the tenant's remaining headroom, which is the decision this data exists to inform.
  - `[ ]` Drop the "not set by this profile" fallback once the resolved figures are always available.

---

## 4. Agentic AI Layer

### 4.1 MCP Registry & Read-Scope Tooling (***)
* **Target Domain**: AI Agents
* **Context**: There is no platform-wide mechanism for LLMs or agents to query app APIs.
* **Proposed Solution**: Build an MCP (Model Context Protocol) server registry, add `mcp:` definitions to the `AppProfile` specification, and deliver 2-3 reference integrations (Nextcloud, OpenProject, Element) exposing read-scope API tools.
* **Backlog Items**:
  - `[ ]` Define the `mcp:` block schema in the `AppProfile` CRD.
  - `[ ]` Build a central MCP registry server within the platform kernel.
  - `[ ]` Implement read-only MCP connectors for Nextcloud, OpenProject, and Element.

### 4.2 Interactive Portal Assistant & Cross-App Aggregation (***)
* **Target Domain**: AI Agents & Portal
* **Context**: Users cannot interact with their system or perform cross-application query aggregations from a single chat window.
* **Proposed Solution**: Deploy a chat assistant inside the portal shell. Leverage OIDC token exchange to securely verify user identity and aggregate data across multiple tenant databases.
* **Backlog Items**:
  - `[ ]` Build the interactive portal chat UI component in the shell.
  - `[ ]` Configure OIDC token exchange client mappings for the chat assistant.
  - `[ ]` Build query routers to aggregate data from multiple tenant applications.

### 4.3 Automated Workflow Agents & Code Generation (***)
* **Target Domain**: AI Agents
* **Context**: Deployment configurations and AppProfiles must be created manually.
* **Proposed Solution**: Build event-driven agents that run workflows based on schedule or system triggers, and implement an AI assistant to auto-generate verified AppProfiles.
* **Backlog Items**:
  - `[ ]` Deploy an event listener that executes workflow scripts on NATS message triggers.
  - `[ ]` Implement the AppProfile code generation tool.
  - `[ ]` Integrate the agentic engine with the tenant provisioning API.

