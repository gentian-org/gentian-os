# Evidence store — design

**Status:** v1, draft for review
**Scope:** the retained, tamper-evident record of security-relevant events a
cluster produces: who decided what, who changed what, who signed in and was
refused, what was backed up and deleted. Not the operations logs an engineer
reads to debug a pod, except where the two overlap.
**Companion docs:** [security.md](security.md) (§3.0 rows *Record of
administrative changes*, *Decision log*), [operations.md](operations.md) §6,
[data-lifecycle.md](data-lifecycle.md),
[../security-principles.md](../security-principles.md) §7,
[../plans/certification.md](../plans/certification.md) C8,
[../compliance/controls.md](../compliance/controls.md) K7, K11, K18, K19,
[../roadmap.md](../roadmap.md) §5.1, [../plans/security-gap-closing.md](../plans/security-gap-closing.md) G10.

---

## 1. Purpose

Principle 7 says audit is three logs joined by one request id: the issuer's
events, the decision log, the change log. Today the change log exists and is
signed; the registrar keeps a record of identity changes; Keycloak keeps its
events in its own database with no expiry and none of them on stdout; the
authorization decisions on the edge are not written anywhere; the gateway's
access log is Envoy's default on a pod's stdout. Two of the five sources die
with a pod restart, and none is in one place an auditor can be shown.

Both frameworks the platform aims at sample this record. ISO 27001:2022
A.8.15 and A.8.16 and SOC 2 CC7.2 and CC7.3 ask that events be produced,
protected against tampering and unauthorised reading, retained, and
reviewed. Swiss law goes further: for large-scale processing of sensitive
data, DSV Art. 4 prescribes what to log (storing, changing, reading,
disclosing, deleting, destroying, and access), what each record says (who,
what kind of processing, when, to whom), and how to keep it: at least one
year, separate from the processing system, readable only by those who check
data protection or safeguard integrity and traceability, and for no other
purpose. France's CNIL recommends six to twelve months for the same logs
and notes that they are personal data about the operators themselves. The
GDPR recognises logging for network and information security as a
legitimate interest (Recital 49) and asks for a stated retention (Art.
5(1)(e)); erasure does not reach records kept for a legal obligation or
legal claims (Art. 17(3)), but the design below does not lean on that.

The evidence store is the one place where these records land, as they
happen, in one shape, minimised before they are written, locked against
change for twelve months, and readable by the two roles the rights model
already names.

## 2. Principles

1. **Two streams.** *Evidence*: low volume, one schema, twelve months,
   immutable, two roles may read. *Operations*: pod and access logs, thirty
   days, searchable, for engineers. One pipeline carries both and routes
   them apart; mixing them is what makes retention and access undecidable.
2. **Write as it happens.** A record that waits in a pod's stdout for a
   nightly job is one restart from not existing. The collector streams to
   the locked bucket in short objects.
3. **Minimise before the lock.** A locked object cannot be fixed. People
   appear as the UUID the issuer gave them, never as a name or an address;
   what cannot be avoided is keyed-hashed at the collector; client
   addresses are truncated; secrets are redacted by pattern. The collector
   is the one place this happens, and it is configuration under review.
4. **The archive is authoritative, the search tier is convenience.** The
   locked objects plus a signed daily manifest are what an auditor is
   shown. The search tier may be rebuilt from them and holds nothing
   longer than they do.
5. **Separate from what it watches.** Its own namespace, its own bucket,
   its own credentials; no tenant or application identity reaches it, and
   the platform's services write to it without being able to read it back.
6. **Reuse the backup machinery.** Object Lock, a stated retention, a signed
   manifest, a key kept outside the cluster: `internal/backup` has all four.
   One mechanism to explain for backups and evidence.
7. **Alert on the store itself.** A source that goes quiet, a break-glass
   use, a burst of refusals: records nobody reads are not a control.

## 3. Architecture

```
  pods (kernel, system, tenant)         Keycloak        Envoy edge
  stdout JSON ──────────────────┐   events (JSON) ──┐  access log ──┐
                                ▼                   ▼               ▼
                     Vector DaemonSet  (kernel-observability)
                     parse · normalise · minimise · route
                          │                       │
            evidence ─────┤                       ├──── operations
                          ▼                       ▼
         s3://gentian-evidence/…            VictoriaLogs (30 d)
         Object Lock, 12 months                   │
                  │                          usher read API
         nightly manifest, signed      /v1/tenants/{t}/evidence (can_view)
         (backup signing key)          /v1/clusters/{c}/evidence (can_audit)
                  │                               │
         Prometheus + Alertmanager        a console relays it to a person
```

### 3.1 Collector: Vector

One DaemonSet in `kernel-observability` (function `observability`, which
`kernel/namespaces.yaml` already reserves for "metrics and logs"). It reads
`/var/log/pods` through a hostPath; the Kyverno baseline excludes the kernel
tier, so nothing refuses it, and the inventory in
`internal/kernel/kernelnet/inventory.yaml` names its ports. Vector is
Apache-2.0, small, and its transformation language has the keyed hash and
redaction functions §5 needs, so minimisation is declarative and reviewed in
one file.

Sources it reads and how it tells evidence from operations:

| Source | How it arrives | Evidence when |
| --- | --- | --- |
| bouncer | JSON on stdout, one line per decision (§3.6) | always |
| director | JSON on stdout: `authz decision`, and one line per commit (§3.6) | always |
| registrar | JSON on stdout: identity actions, as the record table | always |
| custodian, usher | JSON on stdout | lines carrying `request_id` and a decision or write |
| Keycloak | JSON on stdout, events at `info` (§3.5) | every event and admin event |
| Envoy edge | JSON access log (§3.5) | refusals (401, 403) and sign-in paths; the rest is operations |
| operator | JSON on stdout | backup, restore, purge, deletion, privilege and waiver reconciles |
| Kyverno | PolicyReport changes via the reports controller's log | violations and audit findings |
| everything else | stdout | never; operations only |

A pod opts into the evidence stream by namespace function and container
name, declared in Vector's configuration, not by a label a tenant could set.

### 3.2 Evidence archive: the locked bucket

Bucket `gentian-evidence` on the platform's object store (SeaweedFS), made
with Object Lock enabled, `COMPLIANCE` mode, default retention twelve months.
Object Lock is a property a bucket is created with, so the bucket is made by
the operator through the S3 API with the `minio-go` client (Apache-2.0),
not by `weed shell`, which has no lock options. Versioning is implied.
SeaweedFS 4.48 supports both modes, legal holds and bucket defaults.

Objects: `evidence/<cluster-id>/<YYYY>/<MM>/<DD>/<HH>/<source>-<minute>.ndjson.zst`,
one line per record, written by Vector every five minutes or 8 MiB,
whichever first. Each object is immutable the moment it lands; no process
in the cluster holds `s3:BypassGovernanceRetention`, and in `COMPLIANCE`
mode nobody can shorten the lock.

Two identities, both made by the operator the way app identities are made
(`objectStorageProvisionScript`) and both kept in OpenBao under
`gentian-os/kernel/evidence/`: `evidence-writer` (Write, List on this
bucket; no Read) for Vector, and `evidence-reader` (Read, List; no Write),
whose path only the policy bound to `gentian:platform:security` and
`gentian:platform:auditor` may read.

Retention of the objects themselves is the lock's expiry plus a lifecycle
rule of the same length: after twelve months an object is both unlocked and
deleted. The two numbers are one value, `spec.evidence.retentionMonths` on
the Cluster claim, default 12.

### 3.3 Daily manifest, signed

Each night the operator lists the previous day's objects, writes
`evidence/<cluster-id>/<YYYY>/<MM>/<DD>/manifest.json` with each object's
key, size and SHA-256, and signs it with the cluster's backup signing key
(`internal/backup/signature.go`: `SigningKeyFromVault`, `SignManifest`),
under a new context string `gentian-os evidence manifest v1`. The manifest
and `manifest.json.sig` land in the same locked bucket.

The lock stops deletion and change; the manifest proves completeness: an
auditor, or the drill in C10, lists a day, hashes what is there and checks
the signature. `scripts/tools/verify-evidence-day.sh` does exactly that from
outside the cluster with the reader identity and the cluster's public key,
which the recovery kit carries.

### 3.4 Search tier: VictoriaLogs

One StatefulSet in `kernel-observability`, a PVC, `-retentionPeriod=30d`.
Vector sends it both streams. It is for investigation and for answering a
data-subject request; it is not the record. VictoriaLogs is Apache-2.0, a
single binary, and indexes every field, so queries by `subject`,
`request_id` or `tenant` cost nothing special; it stores on local disk,
which is fine for thirty days of a cluster this size.

Nothing reaches its port but the usher (§3.8); the inventory says so. Its
own web UI is not exposed: the platform's promise is the record, its
integrity and the API to read it, and a screen on top of that is a
console's business (§7).

### 3.8 The read API

The usher answers for live state, as it does for backups, under the same
two scopes:

| Route | Permission | Answers |
| --- | --- | --- |
| `GET /v1/tenants/{t}/evidence` | `can_view` on the tenant | the tenant's records from the search tier: `from`, `to`, `kind`, `subject`, `request_id`, `outcome`, paged |
| `GET /v1/tenants/{t}/evidence/export` | `can_view` on the tenant | the same as NDJSON for a period, for a data-subject request or the tenant's own audit |
| `GET /v1/clusters/{c}/evidence` | `can_audit` on the cluster | every tenant's and the kernel's records, same filters |
| `GET /v1/clusters/{c}/evidence/days` | `can_audit` on the cluster | the archive's days with manifest state: signed, verified, missing |
| `GET /v1/clusters/{c}/evidence/days/{d}` | `can_audit` on the cluster | one day's manifest and the verification result |
| `GET /v1/clusters/{c}/evidence/health` | `can_audit` on the cluster | each source's last record, the sink's state, the last manifest |

A tenant's administrator sees the records *about* their tenant: decisions
on its objects, changes to its state, its people's identity events. That is
the controller's own duty under DSV Art. 4, and the console that serves the
tenant is where they read it. The kernel's records and other tenants' are
the cluster roles' alone. Writes — retention, the alert receiver — are
declared state and go through the director as a commit, like a backup
policy.

### 3.5 Sources made to speak

- **Keycloak.** Realm events and admin events are already on with
  `jboss-logging`, but success events log at `debug` and in text. The
  keycloak-idp values set `KC_LOG_CONSOLE_OUTPUT=json` and
  `--spi-events-listener-jboss-logging-success-level=info`, so every event
  is one JSON line Vector parses. `eventsExpiration` is set to thirty days
  on every realm (`tenant-default.yaml` and the kernel realm bootstrap): the
  database copy is a buffer, the archive is the record.
- **Envoy edge.** The `gentian-edge` EnvoyProxy gets an explicit JSON
  access-log format: time, request id, method, host, path (query dropped),
  response code, flags, duration, upstream, client address, and the
  `x-gentian-subject` header the bouncer sets, which is a UUID. No cookies,
  no authorization header, no query string.
- **Perimeter proxy.** Its nginx access log becomes JSON with the same
  fields, minus subject (it has none).

### 3.6 Code that writes a record

Two gaps the survey found, both small:

- **Bouncer edge path.** `Decide` logs nothing; only a cache miss reaches
  the store client's `authz decision` line. Add one line per decision in
  `internal/bouncer/decider.go`: `request_id`, `subject`, `tenant`,
  `relation`, `object`, `allowed`, `cached`, `reason` (no-session,
  unpublished, refused, allowed). Every refusal is logged; allows are
  logged too, since DSV asks for reads.
- **Director.** Commits are logged on some paths. Add one uniform line per
  commit in `internal/director/gitops/gitops.go` after the write:
  `request_id`, `subject`, `relation`, `object`, `commit`, `files`.

The registrar, custodian and usher already log in the shape §4 needs.

### 3.7 Metrics and alerting

`prometheus-operator` with one Prometheus and one Alertmanager in
`kernel-observability`, without Grafana. The operator chart already ships a
`ServiceMonitor` behind `metrics.serviceMonitor.enabled`, which turns on;
the vendored Redis and SeaweedFS charts have theirs; Keycloak and OpenFGA
metrics switch on; Vector exposes its own. Rules in §8. The receiver is a
Cluster claim field, `spec.evidence.alerts`, with a webhook URL and an
optional e-mail; empty means Alertmanager holds the alerts and the admin
console shows them.

## 4. The evidence record

Every source is normalised by Vector to one envelope before it is written
to the archive. The search tier gets the same record plus the original line
under `raw`; the archive does not get `raw`.

```json
{
  "ts": "2026-10-21T09:14:03.118Z",
  "source": "bouncer",
  "kind": "decision",
  "request_id": "f3a1…",
  "cluster": "6c0e…",
  "tenant": "acme",
  "subject": "2f2a266e-…",
  "object": "app:acme/nextcloud",
  "action": "can_use",
  "outcome": "refused",
  "detail": { "reason": "no-session", "route": "nextcloud.acme.example.org" }
}
```

| Field | Values | Notes |
| --- | --- | --- |
| `ts` | RFC 3339, UTC, milliseconds | from the source line, else the collector's clock |
| `source` | `bouncer`, `director`, `registrar`, `custodian`, `usher`, `operator`, `keycloak`, `gateway`, `perimeter`, `kyverno` | |
| `kind` | `decision`, `change`, `identity`, `session`, `access`, `backup`, `deletion`, `policy`, `break-glass` | |
| `request_id` | as the services mint it | empty for Keycloak until the listener reads `X-Gentian-Request-Id` (§11) |
| `cluster` | the cluster's id (`internal/backup` provenance) | |
| `tenant` | tenant name or empty | |
| `subject` | the issuer's `sub`, a UUID; `system:<service>` for a service; a pseudonym `h:<hex>` where only an address was known | never a name or an e-mail |
| `object`, `action` | as the source states them | the FGA object and relation where there is one |
| `outcome` | `allowed`, `refused`, `ok`, `failed` | |
| `detail` | per source, minimised in §5 | no free text copied from a request |

## 5. Minimisation and pseudonymisation

Rules Vector applies before either sink, in this order:

1. **Subjects are UUIDs.** Where a source names a person otherwise
   (Keycloak events carry `username` and `email`), the UUID is kept and the
   names are dropped.
2. **Addresses are keyed-hashed.** Where only an e-mail address identifies
   someone, as in a sign-up or an invitation before an account exists, it
   becomes `h:` plus HMAC-SHA256 under the key at
   `gentian-os/kernel/evidence/pseudonym` in OpenBao. The key is 32 random
   bytes, made at install, rotated yearly with the retention window;
   after rotation old pseudonyms cannot be reproduced, and the archive is
   anonymous for anyone without the retired key, which the recovery kit
   does not carry.
3. **Client addresses are truncated** to /24 and /48 in evidence. The
   operations stream keeps them whole for thirty days: an investigation
   needs them, an auditor does not.
4. **Patterns are redacted**: `Authorization` values, `Cookie`, bearer and
   JWT shapes, `password=`, `token=`, `code=`, and anything the services
   already mark `[REDACTED]`.
5. **No request bodies, no query strings, no free text.** `detail` holds
   named fields; a source line that is not JSON is kept only in the
   operations stream.

The mapping from UUID to person lives in Keycloak and the registrar and goes
when the person goes. The archive then holds records about a UUID nobody can
resolve, which is the GDPR-compatible way to keep an audit trail intact
through an erasure: nothing in a locked object changes. The processing
register (company side) gets one entry for the evidence store: purpose,
legal basis, retention, recipients.

## 6. Retention

| Store | Retention | Set by |
| --- | --- | --- |
| evidence archive | 12 months, Object Lock `COMPLIANCE` plus lifecycle | `spec.evidence.retentionMonths` |
| search tier | 30 days | `spec.evidence.searchRetentionDays` |
| Keycloak events | 30 days | realm `eventsExpiration` |
| registrar record | 400 days, as today | `REGISTRAR_RECORD_RETENTION`; longer than the archive on purpose: it is the source of an access review, not a copy |
| director change log | the deployments repository's history | unchanged |

Twelve months is where DSV's "at least one year", CNIL's upper bound and
the longest SOC 2 observation period meet. A cluster that needs longer
sets the number; nothing shorter than six months is accepted.

## 7. Access

- `evidence-writer`: Vector only, Write and List, no Read. The lock makes
  Delete impossible for anyone.
- `evidence-reader`: in OpenBao under a path the policies of
  `gentian:platform:security` and `gentian:platform:auditor` may read, and
  no other. The custodian's token-exchange write path is how a person
  obtains it; the read is recorded like any credential read.
- The read API (§3.8): `can_view` on a tenant for that tenant's records,
  `can_audit` on the cluster for all of them. A dedicated `can_read_evidence`
  relation follows if an administrator's reach has to be narrowed without
  touching `can_audit`.
- The screens are a console's. The Admin Console, which is free, shows the
  store's health and promotes the paid continuation, as it does for
  backups; browsing, filtering, the access-review export and the retention
  and alert settings are that continuation's, served from the same API.
  A cluster without any console still has the record, the verify tool and
  the API: nothing the platform promises depends on a screen.
- Separation from the processing system, as DSV asks: a namespace of its
  own, a bucket of its own, credentials no service holds, and the archive's
  replica outside the cluster (§11).

## 8. Alert rules

The first set, as `PrometheusRule` objects in `kernel-observability`:

| Alert | Condition | Why |
| --- | --- | --- |
| `EvidenceSourceSilent` | a source's `vector_component_received_events_total` flat for 30 min while the pod runs | a record that stopped is the first thing an auditor finds |
| `EvidenceArchiveWriteFailing` | Vector's S3 sink error rate above zero for 5 min | the archive is the control |
| `EvidenceManifestMissing` | no manifest object for yesterday by 02:00 UTC | completeness |
| `BreakGlassUsed` | any evidence record with `kind: break-glass` | roadmap 5.5 |
| `RefusalsSpiking` | refusals per tenant above 10× the 7-day baseline for 10 min | brute force, misconfiguration |
| `KeycloakAdminEvent` outside business hours | admin events with no matching registrar `request_id` | changes made around the platform |
| the usual | pod crash-looping, PVC above 85 %, certificate within 14 days of expiry, node not ready | availability, A1 |

## 9. Integrity

Three layers, each independent of the other two: the lock (nothing deletes
or rewrites an object inside twelve months, root included), the signed
manifest (what was written that day is exactly what is there), and transport
(Vector to the store over TLS from the internal CA, the store's credential
write-only). The drill in C10 includes verifying one random day per month
and writes the result as a record of its own.

## 10. Rollout, and the Cluster claim

```yaml
spec:
  evidence:
    enabled: true              # default true; a development cluster may say false
    retentionMonths: 12        # 6 or more
    searchRetentionDays: 30
    alerts:
      webhook: ""              # Alertmanager receiver; empty holds alerts for the console
      email: ""
```

`enabled` follows the pattern of `llm.enabled`: a claim field, a
`claim_setting` in the installer, a `--set-string` into the bootstrap chart,
a skip-by-name rule for `*-evidence.yaml` in the ApplicationSet wrapper. The
components are one raw ApplicationSet file, `kernel/appsets/raw/10-evidence.yaml`
(Vector, VictoriaLogs, prometheus-operator, Prometheus, Alertmanager), each
a chart under `kernel/services/<name>/manifests` with its `UPSTREAM.md`. The
bucket, its identities and the pseudonym key are made by an operator
reconciler the way app storage is. Nothing waits on them at install; a
cluster is installable with the evidence store still coming up.

Resource floor on a small cluster: Vector 100 m / 128 Mi per node,
VictoriaLogs 200 m / 512 Mi plus 20 GiB, Prometheus 500 m / 1 GiB plus
20 GiB, Alertmanager 50 m / 64 Mi, the operator 100 m / 128 Mi.

## 11. Not in this version

- The off-cluster replica of the archive (the backup destination pattern,
  age-encrypted to a dedicated evidence recipient).
- The screens: the Admin Console's health-and-promotion card and the paid
  console's evidence pages are built against §3.8 once it exists, in their
  own repositories.
- A `can_read_evidence` relation, if `can_audit` turns out too wide.
- The evidence exports roadmap 5.5 names: access review, configuration
  changes over a period, break-glass uses.
- Keycloak's event listener reading `X-Gentian-Request-Id`, so that an
  issuer event joins the registrar's record on the id; today the registrar
  sends it and nothing reads it.
- The Kubernetes API server's audit log, which on a managed control plane
  is the provider's to hand over.
- Grafana. VictoriaLogs' own UI and Alertmanager's suffice until somebody
  needs a dashboard.

## 12. Build order

1. Sources speak: Keycloak JSON events at `info` with a thirty-day expiry;
   the Envoy and perimeter access-log formats; the bouncer's per-decision
   line; the director's per-commit line. Each is a change the search tier
   does not need to exist for.
2. The claim field, the installer plumbing and the namespace's `holds`.
3. The operator reconciler: bucket with Object Lock, the two identities,
   the pseudonym key, their OpenBao paths and policies.
4. Vector: the chart, the pipeline configuration, the inventory entries,
   the tests that feed it a line of each source and check the record and
   the minimisation.
5. VictoriaLogs, reachable by the usher only.
6. The usher's read API (§3.8), with the tenant scope filtered server-side.
7. The nightly manifest and `verify-evidence-day.sh`.
8. prometheus-operator, Prometheus, Alertmanager, the rules, the
   ServiceMonitors switched on, the claim's receiver.
9. Documentation: operations.md §6 rewritten, security.md §3.0 rows,
   security-principles §7 *Today* note, controls K7, K11, K18, K19, roadmap
   5.1, certification C8.
