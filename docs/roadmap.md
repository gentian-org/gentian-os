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
* **Context**: Gateway API listeners currently accept routing configurations from any namespace (`From: All`) and utilize broad `ReferenceGrants`, creating a risk of route hijacking.
* **Proposed Solution**: Secure the ingress edge by scoping listener `allowedRoutes` to specific namespace label selectors. Narrow down target namespaces in `ReferenceGrants` to prevent wildcard access.
* **Backlog Items**:
  - `[ ]` Update `gateway_platform_reconciler.go` to enforce route namespace selectors on gateway listeners.
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
* **Context**: Third-party binaries, Helm charts, and remote manifests are retrieved during installation without validating cryptographic digests or pinned versions.
* **Proposed Solution**: Establish a secure supply chain by pinning all dependencies to exact versions and SHA-256 digests. Move remote manifests to a verified local repository or mirror.
* **Backlog Items**:
  - `[ ]` Audit and replace all remote mutable git/branch references with specific tags and commits in `install.sh` and `scripts/steps/`.
  - `[ ]` Pin all Helm chart dependencies to specific versions and SHA digests in ArgoCD files.
  - `[ ]` Implement local mirror targets for all third-party charts.

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
* **Context**: Developers can deploy unverified app profiles, bypass registry constraints, or inject unauthorized sidecars into compositions.
* **Proposed Solution**: Implement an Admission Webhook for `AppProfile` resources that validates the image registry, verifies the `compositionRef`, and gates sidecar configuration.
* **Backlog Items**:
  - `[ ]` Implement an Admission Webhook targeting `AppProfile` CRD requests.
  - `[ ]` Add validation checks for allowed registries and image digests.
  - `[ ]` Reject profiles specifying unauthorized sidecars or privileged configurations.

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
* **Proposed Solution**: Replace the blanket binding with an aggregated `ClusterRole` assembled from the resource kinds the Compositions actually compose (namespaces, ClusterIssuers, ClusterSecretStores, AppProjects, and the kernel/tenant kinds). Because an under-specified role reproduces the original silent failure — a missing rule surfaces only as "observe failed" on an individual composed resource, never on the XR the installer waits on — the scoped role needs a CI guard that derives the required kinds from the Composition set and fails when the role does not cover them. Consider separate roles per provider, since `provider-helm` and `provider-kubernetes` do not need the same verbs.
* **Backlog Items**:
  - `[ ]` Enumerate the resource kinds and verbs each Composition requires of `provider-kubernetes` and `provider-helm`.
  - `[ ]` Define aggregated ClusterRoles per provider and replace the `cluster-admin` bindings in `provider-rbac.yaml`.
  - `[ ]` Add a CI check that extracts composed resource kinds from `crossplane/` and fails when the scoped roles omit one.
  - `[ ]` Surface provider permission errors on the XR status so a missing rule fails fast instead of exhausting the install timeout.
  - `[ ]` Document the "add a Composition → extend the provider role" step in `docs/deployment.md`.

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
* **Context**: external-dns's Cloudflare provider does not preserve an MX record's preference when it reads the record back. Debug logging shows it holding a record Cloudflare serves as `10 mail.gtn.host` as `0 mail.gtn.host`, so a published preference of 10 never compares equal to what is observed and every reconcile plans a change. Cloudflare applies that as a delete followed by a create, so the name had no MX for a moment every minute — and a sender resolving in that window falls back to the tenant's A record, which is the portal, not Postfix. Every other record external-dns manages here converged and stayed put, which is what isolated it to MX. Worked around by publishing preference 0, which is what the provider reports whatever we write, so the two now agree. That is sound only while each domain has exactly one MX, which is the case: preference orders one MX against another and there is nothing to order.
* **Proposed Solution**: Fix the read upstream so the preference survives, then publish a meaningful preference again. Until then the workaround holds, and the constraint it depends on — one MX per domain — should be checked rather than assumed if a backup MX is ever added.
* **Backlog Items**:
  - `[x]` Capture what external-dns reads back for the MX and compare it to the desired endpoint. *(Debug logging: `gtn.host 1 IN MX  0 mail.gtn.host` against a zone serving `10`.)*
  - `[x]` Stop the churn. *(Publish preference 0, matching what the provider reads back.)*
  - `[ ]` Report the lost preference upstream against the Cloudflare provider.
  - `[ ]` Alert on sustained record churn, so a non-converging reconcile is noticed without reading logs.
  - `[ ]` Guard the one-MX-per-domain assumption if a backup MX is ever introduced.

---

### 1.22 Kernel Settings Reach the Cluster Through the Installer, Not the Claim (**)
* **Target Domain**: Platform Configuration
* **Context**: `spec.mail.egressHost` is declared on the Cluster XRD and no Composition reads it. The value reaches Postfix only as a Helm parameter the installer sets on the `gentian-appsets` Application, and reaches the operator through a different key — `mailEgressHost` — in the cluster's deployments values file. Two sources for one fact, and the claim, which is what an administrator would edit, is not either of them. The failure mode is drift that git cannot show: this cluster's claim had said `egressHost: out.gtn.host` all along while the live Application carried no such parameter, because it was applied before the parameter existed. Postfix therefore greeted as `mail.gtn.host` while its address reversed to `out.gtn.host`, and nothing pinned it to the node holding the floating IP — a reschedule would have moved mail to another address and invalidated SPF and the PTR together. Both are corrected on this cluster; the mechanism that let them diverge is not. The same mechanism hides a live example: the claim says `mail.serviceMode: kernel` while the cluster runs `external`, so the operator skips Dovecot provisioning entirely.
* **Proposed Solution**: Have the Cluster composition carry these settings through, so the claim is the single source and the installer parameters can go. Failing that, make the drift visible — a check that compares the claim against what the live Applications actually carry, so "git says X" and "the cluster does X" cannot quietly differ.
* **Backlog Items**:
  - `[x]` Set Postfix `myhostname` from the egress host, so the HELO name matches the PTR of the address it sends from. *(Now greets as `out.gtn.host`.)*
  - `[x]` Pin Postfix to the node carrying the floating IP, so a reschedule cannot silently change the sending address.
  - `[ ]` Consume `spec.mail.egressHost` in the Cluster composition rather than declaring it and stopping there.
  - `[ ]` Collapse the deployments-values `mailEgressHost` onto the same source, so SPF and HELO cannot disagree.
  - `[ ]` Reconcile `mail.serviceMode`: the claim says kernel, the cluster runs external, and the operator provisions no Dovecot as a result.
  - `[ ]` Report a claim setting that the live cluster does not actually carry, rather than leaving it to be discovered by a mail fault.

---

## 2. Platform, Infrastructure & Lifecycle

### 2.1 Keycloak Provider & Crossplane Consolidation (*)
* **Target Domain**: Platform Infrastructure
* **Context**: Keycloak realms and OIDC clients are currently managed through a mix of Crossplane resources and manifest-bridge bootstrap Jobs. This splits configuration state and makes it harder to manage drift.
* **Proposed Solution**: Consolidate client and realm management to use drift-safe `provider-keycloak` Managed Resources (MRs) once upstream provider versions support browser-flow tuning and broker integration.
* **Backlog Items**:
  - `[ ]` Replace bootstrap Jobs with native `provider-keycloak` Client/Realm MRs.
  - `[ ]` Port OIDC client default scopes and custom browser flow configurations into Crossplane templates.

### 2.2 Composition-Only IntegrationBinding Egress (***)
* **Target Domain**: Platform Infrastructure
* **Context**: The operator handles part of the `IntegrationBinding` logic (like network policies) programmatically, creating a hybrid lifecycle.
* **Proposed Solution**: Transition integration binding entirely to Crossplane Compositions. Gate deployment on the readiness of both consumer and provider, write connection credentials directly, and remove programmatic operator reconciliation loops.
* **Backlog Items**:
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
* **Context**: Posfix/Dovecot and Collabora integrations are deployed and managed by the operator via hardcoded installation scripts.
* **Proposed Solution**: Package the Mail and Office workloads into standard Crossplane Compositions and Helm Charts, removing the installation burden from the operator.
* **Backlog Items**:
  - `[ ]` Create Crossplane compositions for Dovecot/Postfix.
  - `[ ]` Package Collabora/Office settings into standard Helm deployment templates.
  - `[ ]` Remove hardcoded mail/office functions from the Go operator.

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
* **Context**: `internal/controller/app_reconciler.go` and `kernel_gateway_routes.go` hardcode one application: its Service name (`litellm-proxy`), base URL, master-key Secret, and direct calls to its `/key/generate` and `/key/info` HTTP API. This is the same class of boundary violation as the per-app privileged-role provisioner removed in §6h of the app-profile-guide — an application the kernel recognises by name is an application the kernel must be modified to support. LiteLLM is genuinely dual-natured, which is why this was never obviously wrong: it runs as a kernel singleton from `kernel/services/llm/`, and *also* ships a `litellm-me` catalogue profile. The kernel may know its own services; it must not know catalogue entries.
* **Proposed Solution**: Decide which LiteLLM is — kernel service or catalogue app — and make the code say so. If it is a kernel service, express it the way the other kernel services are (discovered configuration rather than compiled-in constants, in the same shape as SMTP/S3/Keycloak endpoints) so a cluster can run a different LLM gateway or none. If it is a catalogue app, the virtual-key provisioning belongs in its profile via the generic `spec.provisioning.syncJob` mechanism, and the kernel keeps only the generic credential plumbing.
* **Backlog Items**:
  - `[ ]` Classify LiteLLM as kernel service or catalogue app and record the decision in `architecture.md`.
  - `[ ]` Replace the `litellmProxy*`/`litellmMasterKey*` constants with values resolved from kernel service configuration (as `${S3_ENDPOINT}` and friends already are).
  - `[ ]` Move virtual-key issuance (`/key/generate`, `/key/info`) out of the operator — to the app's own profile if it is a catalogue app, or behind a named kernel-service interface if it is not.
  - `[ ]` Drop the app-catalogue `litellm` special-case in `kernel_gateway_routes.go`.
  - `[ ]` Add a CI check that fails when a catalogue app name appears in gentian-os source, so this class of drift is caught rather than reviewed for.

---

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
* **Context**: Shell portal tiles are currently shown to users regardless of whether they have permission to access the application.
* **Proposed Solution**: Gate portal tile rendering by querying OpenFGA `can_launch` relations before rendering the main shell interface.
* **Backlog Items**:
  - `[ ]` Implement `can_launch` relation rules inside the OpenFGA authorization store.
  - `[ ]` Refactor the portal UI frontend to filter tiles based on OpenFGA responses.

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
