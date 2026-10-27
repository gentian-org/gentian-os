# Audit events, log forwarding and monitoring — design

**Status:** v1
**Scope:** what the platform records about who decided, changed, signed in
and was refused; how those records leave the nodes; and the platform's own
metrics and alerts. Not where the records are kept long term: that is a
destination the cluster names, such as the Aluvian Operations Console's
evidence store.
**Companion docs:** [security.md](security.md) (§3.0 rows *Record of
administrative changes*, *Decision log*), [operations.md](operations.md) §6,
[../security-principles.md](../security-principles.md) §7,
[../compliance/controls.md](../compliance/controls.md) K7, K11, K18, K19.

---

## 1. Purpose

Principle 7 says audit is three logs joined by one request id: the issuer's
events, the decision log, the change log. The platform produces them; it
does not have to keep them. A record that is written as structured JSON on
stdout, carries the request id and names people by their issuer UUID can be
collected, retained and reviewed by whatever the operator chooses, and an
auditor can follow one request from the edge to the commit.

Three parts, each independent of the others:

1. **Sources** (§3): every service whose decisions matter writes one JSON
   line per decision, change or identity event, in one envelope.
2. **Forwarder** (§4): an optional DaemonSet that collects those lines and
   the pods' other output and ships them to a destination the Cluster claim
   names. Off until a destination is set.
3. **Monitoring** (§5): an optional kube-prometheus-stack, switched on by
   one claim field, with the platform's own alert rules.

The platform installs and runs with none of 2 and 3 switched on. A cluster
without a destination still has the change log in git, the registrar's
record and the issuer's events, as today.

## 2. Principles

1. **Write as it happens, in one shape.** One JSON line per event, the
   envelope in §3.1, on stdout. No source keeps its own audit file.
2. **People are UUIDs.** The issuer's `sub`, never a name or an address. The
   mapping lives in Keycloak and the registrar and goes when the person
   goes.
3. **Secrets never leave a pod.** Tokens, cookies, passwords and query
   strings are not written, and the forwarder redacts the shapes again
   before anything leaves the node.
4. **The platform names no consumer.** The forwarder's destination is an
   address and a credential in the claim. What receives the records is the
   operator's choice.

## 3. Sources

### 3.1 The envelope

```json
{
  "time": "2026-10-27T09:14:03.118Z",
  "level": "INFO",
  "msg": "audit",
  "audit": true,
  "source": "bouncer",
  "kind": "decision",
  "request_id": "f3a1…",
  "tenant": "acme",
  "subject": "2f2a266e-…",
  "object": "app:acme/nextcloud",
  "action": "can_use",
  "outcome": "refused",
  "detail": { "reason": "no-session" }
}
```

`audit: true` is what tells an audit record from a service's ordinary log
line; the forwarder routes on it. `time`, `level` and `msg` are the JSON
handler's own; `internal/audit` adds the rest, and leaves out a field that
is empty rather than writing it empty. `kind` is one of `decision`, `change`,
`identity`, `session`, `access`, `backup`, `deletion`, `policy`,
`break-glass`. `outcome` is `allowed`, `refused`, `ok` or `failed`.
`detail` holds named fields only, never text copied from a request.

### 3.2 What writes it

| Source | Today | Change |
| --- | --- | --- |
| bouncer | only a cache miss reaches the store client's `authz decision` line; the edge path writes nothing | one envelope per decision in `internal/bouncer/decider.go`, allows and refusals, with the reason (no-session, unpublished, refused, allowed) and whether it was cached |
| director | `authz decision` per check; commits logged on some paths | one envelope per commit after the write in `internal/director/gitops`: request id, subject, relation, object, commit, files |
| registrar | identity actions in its record table and on stdout | the stdout line takes the envelope |
| custodian, usher | JSON with `request_id` | decisions and writes take the envelope |
| operator | reconcile logs | backup, restore, purge, deletion, privilege and waiver outcomes take the envelope |
| Keycloak | events in its database, success events at debug in text | `KC_LOG_CONSOLE_OUTPUT=json`, the jboss-logging listener's success level at `info`; `eventsExpiration` thirty days on every realm |
| Envoy edge | Envoy Gateway's default access log | a JSON format on the `gentian-edge` EnvoyProxy: time, request id, method, host, path without query, status, flags, duration, upstream, client address, and the subject header the bouncer sets |
| perimeter proxy | nginx default | the same JSON fields, without subject |

Keycloak's events carry the request id once the event listener reads the
`X-Gentian-Request-Id` header the registrar already sends.

## 4. Forwarder

Vector (Apache-2.0) as a DaemonSet in `kernel-observability`, the namespace
`kernel/namespaces.yaml` reserves for logs and metrics. It reads
`/var/log/pods` through a read-only hostPath, which the Kyverno baseline
permits in the kernel tier and nowhere else; that is why the forwarder is
the platform's and not an add-on's.

It does three things and no more:

1. **Parse** each pod's lines; JSON is kept as fields, anything else as a
   message.
2. **Redact** authorization values, cookies, bearer and JWT shapes,
   `password=`, `token=` and `code=` again, whatever the source promised.
3. **Ship** to the destination, two streams: lines with `audit: true`, and
   the rest. A destination may take either or both.

```yaml
spec:
  logForwarding:
    destination:
      endpoint: https://evidence.example.org/ingest   # unset: no forwarder
      format: ndjson            # ndjson over HTTPS, or otlp
      credentialRef: log-destination   # OpenBao-held, like a backup destination
      streams: [audit, operations]
```

Unset `destination` renders no DaemonSet. The credential is supplied once,
the way a backup destination's is, and never sits in the claim. The
inventory in `internal/kernel/kernelnet/inventory.yaml` names its egress.

Minimisation beyond redaction — pseudonymising addresses, truncating client
IPs, deciding what is evidence — is the destination's, because it depends
on what the destination keeps and for how long.

## 5. Monitoring

```yaml
spec:
  monitoring:
    enabled: false        # true installs kube-prometheus-stack
    alerts:
      webhook: ""         # Alertmanager receiver; empty holds alerts
```

`enabled: true` installs kube-prometheus-stack in `kernel-observability`:
Prometheus, Alertmanager, node-exporter, kube-state-metrics and Grafana.
Grafana answers at `monitoring.<kernel domain>` (a name for the function,
so a tenant's own Grafana keeps its name), behind the kernel Gateway's
session under `can_audit`, and asks for its own sign-in at the kernel realm
like Argo CD and Headlamp; the platform administrator is Grafana's Admin,
everyone else who reaches it a Viewer. Switched off on the claim, the
installer removes the stack. The operator's
`ServiceMonitor`, the vendored Redis and SeaweedFS monitors and the
Keycloak and OpenFGA metrics switch on with it. The platform ships these
rules:

| Alert | Condition |
| --- | --- |
| `AuditSourceSilent` | a source's audit lines stop for 30 minutes while its pod runs |
| `LogForwardingFailing` | the forwarder's sink errors for 5 minutes |
| `BreakGlassUsed` | a `break-glass` record |
| `RefusalsSpiking` | refusals per tenant above 10× the 7-day baseline |
| availability | pod crash-looping, volume above 85 %, certificate within 14 days, node not ready |

A destination may bring rules of its own as `PrometheusRule` objects.

## 6. What this leaves to a destination

Keeping the records for a period, locking them against change, signing
what was kept, searching them, exporting them for an auditor or a
data-subject request, and the screens for all of it. The Aluvian
Operations Console offers this as its evidence store, with an on-cluster or
external locked bucket; any other system that accepts NDJSON or OTLP over
HTTPS can take its place.
