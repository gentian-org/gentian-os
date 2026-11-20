---
audience: operator, architect, auditor
type: concept
---

# Secrets and keys: what is kept where

Every secret and key of a cluster has one place. This page states the eight
rules that decide the place, lists the places with who writes and who reads
each, and says for every kind of secret where it goes. The paths, policies
and generation modes are in [design.md §4 to §12](design.md#4-secrets-topology).

Words used here: a **secret** opens something (a password, a client secret,
an API token). A **key** proves or protects something (a signing key, an
encryption key, a TLS key). A **digest** is a one-way value that can check a
secret and cannot give it back.

## 1. The rules

They refine principles 1, 5, 7 and 8 of the [security principles](principles.md)
for secrets, and follow accepted practice: least privilege, separation of
duties, one key for one purpose (NIST SP 800-57), and the OWASP guidance on
secrets management and password storage.

| # | Rule | What it prevents |
|---|---|---|
| S1 | **One home, one writer.** A secret is kept in one place and written by one party. Whoever writes it need not be able to read it. | Two copies that drift apart; a service that takes in secrets and can also give them out |
| S2 | **A workload gets a copy, never the store.** A workload receives the secrets it needs as a Kubernetes Secret in its own namespace. It holds no vault identity. | A break-in at one app reaching every secret the vault holds |
| S3 | **Keep a digest or a public half wherever the value is never needed back.** A party that only checks a secret holds its digest. A party that only verifies a signature holds the public key. | A checking service that leaks what it checks |
| S4 | **A short-lived token before a standing secret.** Where a caller can prove itself with a token that names it, its audience and an expiry, no secret is made. | Secrets that stay valid long after they leaked |
| S5 | **A secret reaches as far as its owner and no further.** Owners are the cluster, a tenant, an app and a person. A place of one owner is not readable from another owner's namespace. | One tenant, or one app, reading another's secrets |
| S6 | **One key, one purpose, kept apart from what it protects.** The key that opens the vault is not in the vault. The key that opens the backups is not beside the backups. A signing key signs one kind of thing. | One theft that yields both the lock and the key |
| S7 | **Never in git, in a custom resource, in a ConfigMap or in a log.** Git holds public halves and names of secrets only. A record of who wrote or read a secret carries no value. | Secrets in history that cannot be taken back |
| S8 | **A secret ends with its owner.** Purging an app removes its secrets, deleting a tenant removes the tenant's, and a person's credentials end with the person. | Credentials that outlive what they were for |

## 2. The places

| # | Place | Holds | Written by | Read by | Why it is a place of its own |
|---|---|---|---|---|---|
| P1 | **The vault, kernel part**<br>`gentian-os/kernel/…` | The platform's own credentials: databases, cache, object storage, Keycloak's bootstrap administrator, the rights store's key, mail, the DNS provider's token, repository credentials, model provider keys | The installer and the `Cluster` Composition, once each; the custodian, for a value a person supplies | External Secrets, through the store `openbao`, for namespaces of tier `kernel` or `system` only; the operator; the director, repository credentials only | No tenant is in the path, and no tenant's store can name it (S5) |
| P2 | **The vault, a tenant's part**<br>`gentian-os/tenants/<tenant>/…` | Each app's credentials (sign-in client, database, object storage, cache, mail, model gateway), the secrets a profile asks to have generated, repository credentials, the shared credential of a contract, the backup destination | The operator, once per path, for what the platform generates; the custodian, for a value a person supplies | External Secrets, through the tenant's own store `openbao-tenant-<tenant>`, for that tenant's namespaces only | One store, one role and one policy per tenant: an `ExternalSecret` in a tenant's namespace reaches that tenant's paths, whatever it names (S5) |
| P3 | **The vault, paths no workload receives**<br>`kernel/backup/identity`, `kernel/backup/signing`, `tenants/<tenant>/backup/identity` | The key that opens backup bundles, where the cluster keeps it in escrow; the key that signs bundles | The installer, once; the custodian, for a tenant's own backup identity | The operator, from the vault itself. Denied to External Secrets, so it never becomes a Kubernetes Secret | A key that opens every bundle must not be one `ExternalSecret` away from an ordinary Secret (S6) |
| P4 | **The vault's certificate authority**<br>`pki-internal` | The private key of the internal CA | The vault, which generates it | Nobody. cert-manager asks the vault to sign; the key does not leave | A CA key that is never exported cannot be copied (S3) |
| P5 | **The seal**<br>a second OpenBao in `kernel-seal` | The key that unseals the vault | The installer | The vault, with a token for the seal only | The key that opens the vault is not kept in the vault ([AD-8](../decisions/ad-08-kernel-trust-domains.md), S6) |
| P6 | **The recovery kit**<br>an encrypted file, off the cluster | The master password and its salt, the vault's recovery key, the seal's unseal key, the break-glass signing key, the backup keys | The installer | The person who runs the cluster, with the kit's passphrase | What rebuilds a cluster must survive the loss of the cluster (S6) |
| P7 | **Copies in a namespace**<br>Kubernetes Secrets that External Secrets writes | What a workload needs from P1 or P2 | External Secrets, refreshed from the vault | The workloads of that namespace | The copy, not the store, is what a workload can lose (S2) |
| P8 | **Secrets made in the cluster for one pair of parties** | A realm client's secret for the Gateway and the bouncer; a component key; a contract key; the Keycloak event listener's signing key; TLS private keys; DKIM keys | A generator in the cluster (External Secrets' password generator, the operator, cert-manager), once | The one party that presents or uses it, in its own namespace. The party that checks it holds a digest or the public half (P9) | Nobody types or restores these: they can be made again, so they need no home in the vault (S1, S3) |
| P9 | **Digest lists and public keys** | SHA-256 digests of component keys and contract keys; the public half of the event listener's key; the public halves of the commit-signing keys, in the deployments repository | The operator; the installer | The checking service: the registrar, the director, the bouncer, the arbiter, a contract's provider; Argo CD for commit signatures | A checker that holds no secret has none to leak (S3) |
| P10 | **The realm**<br>Keycloak's database | A person's password and second factor; device passwords, as digests; a realm's token-signing keys | The person, through Keycloak and the registrar; Keycloak | Keycloak only. Everyone else sees tokens it signed and its public keys | Who a person is, is answered in one place ([principle 2](principles.md)); a credential there ends when the person's password changes or the person is disabled (S8) |
| P11 | **Nowhere: tokens** | A workload's ServiceAccount token for one audience, ten minutes; a person's sign-in token; a token exchanged for one app | The Kubernetes API server; Keycloak | The one audience named in the token | Nothing is stored, so nothing is there to steal later (S4) |
| P12 | **The install host**<br>`~/.gentian/` | The keyring with the break-glass signing key; the vault's first root token and recovery key until handover, when the token is revoked and the file deleted | The installer | The person who runs the installation | Installation is the one act that happens before the cluster can keep a secret |

## 3. Which secret goes where

| Kind of secret or key | Place | Rule that decides |
|---|---|---|
| A person's password, second factor | P10, the realm | S1 |
| A device password, for a calendar or file program | P10, as a digest; shown to the person once | S3 |
| A credential the platform generates for an app (database, sign-in client, storage) | P2, copied to the app's namespace (P7) | S2, S5 |
| A credential a person supplies (registry login, backup destination, DNS token, model provider key) | P1 or P2, written through the custodian, which cannot read it back | S1 |
| A secret a profile asks to have generated for its app | P2, under the app's own path | S5 |
| The shared credential of a contract between two apps | P2 for a password both sides use; P8 and P9 for a key the provider only checks | S3 |
| The key a component proves itself with to a kernel service | P8 for the component, its digest in P9 for the service | S3 |
| A realm client's secret for the edge | P8, in the edge namespace | S1 |
| A workload proving itself to a kernel service or to the vault | P11, a token for that audience | S4 |
| A TLS private key | P8, held by cert-manager; the internal CA's key in P4 | S3, S6 |
| The director's commit-signing key | P1, copied to the director alone; the public half in P9 | S6 |
| The break-glass commit-signing key | P6 and P12; the public half in P9 | S6 |
| An image signature | No key: CI signs with its workflow identity | S4 |
| The key that opens backups; the key that signs them | P3 and P6 | S6 |
| The key that unseals the vault | P5 and P6 | S6 |
| The master password and its salt | P1 and P6, and one Secret of the installer in `kernel-provisioning` | S6 |

## 4. Placing a new secret

Ask in this order, and stop at the first yes.

1. **Can a token do it?** A caller that can present a token naming it and its
   audience needs no secret (S4, P11).
2. **Does anyone need the value back?** If one side only checks it, generate
   it in the cluster, give that side the digest or the public half, and keep
   the value with the one party that presents it (S3, P8 and P9).
3. **Is it a person's proof of who they are?** It belongs to the realm (P10).
4. **Does it open the backups, the vault, or sign for the cluster?** It gets a
   path no workload receives, and a copy in the recovery kit (S6, P3 to P6).
5. **Everything else** goes to the vault under its owner's path, written once
   by the operator or through the custodian, and reaches its workload as a
   copy (S1, S2, S5; P1, P2, P7).

Then name the owner: the secret is removed when that owner is (S8).
