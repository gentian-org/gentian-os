---
audience: operator
type: guide
---

# Preparing a cluster: requirements, platform and DNS provider

**Audience:** cluster operators, before the first run of `./install.sh`.
**Companion docs:** [GETTING-STARTED.md](../../GETTING-STARTED.md),
[routing.md](../concepts/routing.md), [node-pool-migration.md](node-pool-migration.md).

What the Kubernetes cluster has to offer before the install, how big it has to
be, and how the platform learns where the cluster runs and who hosts its DNS.
Both of the last are entries in one file, `kernel/platforms.yaml`; §5 says how
to add one.

## 1. The cluster

- **Kubernetes 1.33 or newer, and you are an admin on it.** The installer does
  not create the cluster, and pre-flight refuses an older one: the edge it
  installs, Envoy Gateway 1.9, is tested on Kubernetes 1.33 to 1.36.
  `kubectl version` shows the server's.
- **Cluster DNS, RBAC and a default StorageClass.** On MicroK8s those are the
  add-ons `dns`, `rbac` and `hostpath-storage` (or a StorageClass of your own,
  named at the `storageClass` question in step 4 of GETTING-STARTED). The
  installer raises MicroK8s's limit of 110 pods per node to 220 itself, which
  restarts MicroK8s once and asks for `sudo`.
- **An older MicroK8s is upgraded one minor version at a time**, never across
  several — Kubernetes supports no other way. From 1.31, for example, wait for
  the node to be `Ready` after each of:

  ```bash
  sudo snap refresh microk8s --channel=1.32/stable && microk8s status --wait-ready
  sudo snap refresh microk8s --channel=1.33/stable && microk8s status --wait-ready
  kubectl get nodes        # VERSION reads v1.33.x
  ```

- **A `kubectl` no more than one minor version away from the cluster's.** An
  older one still works for most things and prints a warning about version
  skew on every call.
- **Secrets encrypted at rest.** Kubernetes stores every Secret in etcd, and
  without an encryption provider at the API server that is plain base64 for
  anyone who holds an etcd snapshot. On your own control plane, give the API
  server an `EncryptionConfiguration` before installing; on a managed one, ask
  the provider and record the answer when step 0 asks for
  `controlPlane.secretsAtRest`. Pre-flight warns until the claim says
  `provider` or `cluster`, or the API server's flags show it. Switched on
  after the fact, run `scripts/tools/rewrite-secrets.sh --apply` once: a
  Secret written before stays plain until it is written again.

## 2. Size

The kernel and the `platform` tenant run about 100 pods and reserve 4 to 6 CPU
and 9 to 16 GiB of memory before any app is installed. A tenant adds to that
with its apps: one with a handful of apps about 15 to 25 pods, 1 to 2 CPU and
2 to 4 GiB; one with about 25 apps about 45 pods, 4 CPU and 11 GiB. The
cluster's own system pods (network plugin, DNS, CSI driver, metrics) come on
top.

The limit met first is usually the number of pods per node: 110 unless the
node pool sets another. On a managed cluster it is a setting of the node pool
at the provider, which the installer cannot change. A single node of 4 CPU and
16 GiB does not hold the kernel; two such nodes hold it with one small tenant,
and three of 8 CPU and 32 GiB leave room for several.
[node-pool-migration.md](node-pool-migration.md) covers moving a running
cluster to bigger nodes.

## 3. The platform

`platform` in the Cluster claim says who runs the machines and answers for a
`LoadBalancer` Service. Its entry in `kernel/platforms.yaml` carries the
annotations the edge's Service gets, so that the cloud's load balancer is set
up the way the edge needs (for example a health monitor on OpenStack).

Left blank at the `platform` question of step 4, it is detected from the
first node's `spec.providerID` (`scripts/lib/certs.sh`):

| `providerID` starts with | Platform |
|---|---|
| `openstack://` | `openstack` |
| `aws://` | `aws` |
| `gce://` | `gcp` |
| `azure://` | `azure` |
| `hcloud://` | `hetzner` |

Anything else is detected as no platform, and the Service gets no annotations.
A managed Kubernetes built on one of these clouds is detected as that cloud,
which is the right answer for its load balancer; name the platform yourself
only when detection is wrong or when you want a provider's own name on the
claim (`infomaniak`).

| Entry | Load balancer | Needs |
|---|---|---|
| `self-hosted` | MetalLB on your own machines | — |
| `none` | none: k3s ServiceLB, a single node behind a tunnel, or an edge fronted from outside | — |
| `openstack` | Octavia; the address is kept when the Service is deleted | — |
| `infomaniak` | as `openstack` | — |
| `hetzner` | hcloud cloud controller | `platformParams.location` |
| `aws` | NLB through the aws-load-balancer-controller | — |
| `gcp` | GKE L4 external | — |
| `azure` | Azure standard load balancer | — |

An entry that pins an address reads it from the claim's `addressRef`; left
empty, the cloud allocates one.

## 4. The DNS provider

`certificates.dnsProvider` says who hosts the zone and who publishes the
cluster's names in it. It is independent of the platform: a Hetzner cluster on
a Cloudflare zone is ordinary. Its entry under `dnsProviders` in
`kernel/platforms.yaml` gives cert-manager its DNS-01 solver and external-dns
its provider, and names the one credential both read. An entry without a
solver answers no DNS-01: the installer offers those under `acme-http01` and
the others under `acme-dns01`.

| Entry | Needs, besides the credential |
|---|---|
| `cloudflare` (default) | — |
| `route53` | `region`, `hostedZoneID` |
| `clouddns` | `project` |
| `azuredns` | `subscriptionID`, `tenantID`, `resourceGroupName`, `clientID` |
| `rfc2136` (BIND, Knot, PowerDNS with TSIG) | `nameserver`, `tsigKeyName`, `zone` |
| `hetzner`, `infomaniak` | `webhookGroupName`, `webhookSolverName` of a cert-manager webhook |
| `gardener` | no credential: a Gardener shoot's DNS service publishes the names (§4.3); `acme-http01` only |
| `none` | no DNS-01 and no published records (§4.2); `acme-http01` only |

### 4.1 What the credential must be allowed to write

Whichever the provider, the credential writes these records into the kernel
domain's zone and into every zone named under `certificates.dnsZones`:

| Records | Names | Written by | Needed |
|---|---|---|---|
| TXT | `_acme-challenge.<name>` | cert-manager, for each certificate | under `acme-dns01` |
| A, CNAME | every name the cluster serves (`id.<domain>`, …), the domain itself included | external-dns | always |
| TXT | `_extdns-<type>.<name>` beside each of those: the record that marks it as this cluster's | external-dns | always |
| MX; TXT at the mail domain itself (SPF), at `_dmarc.<domain>` and at `<selector>._domainkey.<domain>` (DKIM); A for `mail.`, `imap.` and the egress name | the kernel domain and each tenant's mail domain | the operator, through external-dns | `mail.serviceMode: internal` |

Some hosted DNS APIs scope a token to certain record types or names, for
example TXT only under `_acme-challenge.`. Such a credential passes the
installer's checks and certificates are issued, but external-dns cannot write
its ownership records and so publishes none of the cluster's names. Ask the
provider for these rights before you install.

### 4.2 Records kept by hand

With `dnsProvider: none` nothing publishes the cluster's names, and you keep
them in the zone yourself. Wildcards replace the per-name A and CNAME records,
and with them external-dns's ownership TXT records, which exist only for
external-dns. Every name the cluster serves points at the same address, so two
records stand in for all of them:

| Name | Points at |
|---|---|
| `<domain>` | the edge's address |
| `*.<domain>` | the edge's address |

`*.<domain>` answers for names several levels down too
(`desktop.<tenant>.<domain>`), as long as nothing exists under
`<tenant>.<domain>` itself. Nothing does here: without DNS-01 no
`_acme-challenge` record is written. Anything else you put under a tenant's
name ends the wildcard there, and that tenant then needs its own
`*.<tenant>.<domain>`. A new app needs no record. With
`mail.serviceMode: internal`, the mail records of §4.1 are kept by hand too,
the DKIM key again whenever it changes.

### 4.3 Records published by a Gardener cluster

On a Gardener shoot whose DNS service (`shoot-dns-service`) manages the zone,
choose `dnsProvider: gardener` with `acme-http01`; where the cluster serves
`dns.gardener.cloud`, it is the interview's default. The edge's `LoadBalancer`
Service then carries Gardener's annotations for the two records of §4.2, and
Gardener publishes them for the Service's address, follows the address when
it changes, and removes them with the Service. Gardener writes the zone with
the shoot's own credential, so the platform holds none, and the scoped tokens
of §4.1 do not matter.

## 5. Adding an entry

A provider that none of the entries describes gets an entry of its own. Nothing
in a template branches on a provider's name: the annotations and the solver are
read from the entry, so the entry is most of the work.

A platform entry:

```yaml
platforms:
  <name>:
    displayName: <shown at the installer's question>
    supportsLoadBalancerIP: <true when the Service's own loadBalancerIP field pins the address>
    requiredParams: [<keys under platformParams the claim must set>]
    lbAnnotations:
      # Go templates rendered against the consuming chart's values
      # (.Values.addressRef, .Values.nodeIp, .Values.platformParams.<key>).
      # An annotation that renders empty is dropped.
      <annotation>: '<value>'
```

A DNS provider entry:

```yaml
dnsProviders:
  <name>:
    displayName: <shown at the installer's question>
    requiredParams: [<keys under certificates.dnsParams>]
    paramPatterns: {<key>: '<regular expression the value must match>'}
    credential:
      secretName: <name>-credentials
      vaultPath: gentian-os/kernel/dns/<name>
      displayName: <shown when the installer asks for it>
      description: <what to create at the provider, and with which rights (§4.1)>
      fields:
        - key: <key in the Secret>
          format: token
          secret: true
    solver:
      # Passed to cert-manager's ClusterIssuer verbatim, after rendering
      # against .Values.dnsParams.
      <solver kind>: {...}
    externalDNS:
      enabled: true
      provider: <external-dns provider name>
      secretKeys: {<key external-dns reads>: <key in the Secret>}
      env: {<variable>: <key external-dns reads>}
      args: ['<argument, rendered against the chart values>']
```

Then:

1. Add the name to the matching `enum` in `crossplane/xrds/cluster.yaml`
   (`platform`, or `certificates.dnsProvider`).
2. For a platform, add it to the list the installer offers in
   `scripts/lib/common.sh` (`prompt_cluster_settings`). A DNS provider is
   offered from the table itself: under `acme-dns01` if it has a `solver`,
   under `acme-http01` if not. A `detectCRD` makes it the default on a
   cluster that serves that definition, and `edgeAnnotations` are put on the
   edge's Service, rendered against `.Values.kernelDomain`.
3. For a platform the nodes identify, add its `providerID` prefix to
   `_detect_platform` in `scripts/lib/certs.sh`.
4. Run `make gen-all`: it renders the DNS credential's `CredentialRequirement`
   into `kernel/credentials/credential-requirements.yaml` and the embedded
   copy of the definition.
5. Add the entry to the tables in §3 or §4.
