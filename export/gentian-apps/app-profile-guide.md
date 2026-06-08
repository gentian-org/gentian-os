# AppProfile Authoring Guide

This guide covers **catalogue entries for existing upstream Helm charts** (profile
YAML only). To **build a new Gentian-native app** (FastAPI + React + Helm), see
[custom-app-guide.md](custom-app-guide.md).

This guide captures the accumulated learnings and best practices from the existing
AppProfile implementations. Read it before writing a new profile — every section
corresponds to a class of bug that was caught in git history.

---

## 1. Mandatory top-level fields

```yaml
apiVersion: gentianos.io/v1alpha1
kind: AppProfile
metadata:
  name: <app-id>                        # lowercase, kebab-case
  labels:
    gentianos.io/profile-name: <app-id> # must match metadata.name
spec:
  deploymentMethod: crossplane          # ALWAYS crossplane — never tofu-controller
```

---

## 2. Placeholders (substituted at deploy time)

The **gentian-os operator** sets `App.spec.domain` from the tenant's
effective domain. **Crossplane app Compositions** replace placeholders in
`extraValues` and OIDC redirect URIs when rendering helm values and
`provider-keycloak` Client MRs. Use placeholders everywhere — never
hardcode cluster-specific addresses.

| Placeholder | Resolves to |
|---|---|
| `${TENANT_DOMAIN}` | Tenant effective domain (e.g. `demo.desk.gentian.org`) |
| `${TENANT_ID}` | Tenant name / Keycloak realm (e.g. `demo`) |
| `${TENANT_NAMESPACE}` | Kubernetes namespace (`tenant-{id}`) |
| `${KERNEL_DOMAIN}` | Cluster kernel DNS suffix (e.g. `desk.gentian.org`) |
| `${LDAP_HOST}` | UCS LDAP service hostname |
| `${LDAP_BASE_DN}` | LDAP base DN (e.g. `dc=swp-ldap,dc=internal`) |
| `${LDAP_BIND_DN}` | App-specific LDAP bind DN |
| `${SMTP_HOST}` | Postfix service address (injected by operator) |
| `${S3_ENDPOINT}` | MinIO API endpoint URL |
| `${MYSQL_HOST}` | MariaDB service (OX App Suite) |
| `${REDIS_HOST}` | Redis service (OX App Suite) |
| `${IMAP_HOST}` | Dovecot/IMAP service |
| `${NODE_IP}` | Node external IP (Jitsi TURN) |
| `${TURN_*}` | TURN credentials (Element/Jitsi, from kernel path) |

**Common mistake:** Using a hardcoded cluster-internal address like
`nubus-dev-ldap-server.gentian-dev.svc.cluster.local` instead of `${LDAP_HOST}`.
That value only works in one cluster and breaks on every other environment.

### `${TENANT_DOMAIN}` vs `${KERNEL_DOMAIN}` — where and why

| Placeholder | Use for | Why it matters |
|---|---|---|
| **`${TENANT_DOMAIN}`** | App-facing URLs on the tenant zone: `chat.${TENANT_DOMAIN}`, `meet.${TENANT_DOMAIN}`, OIDC `redirectUris`, Matrix `serverName`, public Jitsi URL, `mail.${TENANT_DOMAIN}` in OX | Browsers, Keycloak, and ingress must agree on the **same hostname** the user sees. A typo or hardcoded domain causes `redirect_uri` mismatch, broken cookies, or TLS on the wrong cert (`*.<effectiveDomain>`). |
| **`${KERNEL_DOMAIN}`** | Shared platform hosts: `portal.${KERNEL_DOMAIN}`, `id.${KERNEL_DOMAIN}`, post-logout redirect to portal | Kernel UIs and the Gentian Portal live on the **cluster** wildcard, not the per-tenant app wildcard. Mixing these (e.g. OIDC issuer on tenant domain when the app expects `id.<kernel>`) breaks SSO and iframe embedding from the portal. |

**Rule of thumb:** anything the tenant's users type in the address bar for an
**installed app** → `${TENANT_DOMAIN}` (plus `subDomain` in `AppProfile.spec.ingress`).
Anything on the **platform shell or IdP** → `${KERNEL_DOMAIN}`.

**Keycloak / `global.domain`:** charts build `https://{hosts.keycloak}.{global.domain}`
for OIDC. Set `global.domain: "${KERNEL_DOMAIN}"` and prefix tenant app host
labels with `${TENANT_ID}` (e.g. `jitsi: "meet.${TENANT_ID}"` →
`meet.demo.desk.gentian.org`). See `gentian-apps` commit `b1203d0`.

### Central IdP — required pattern (all profiles)

Gentian uses a **central Keycloak** on the kernel domain. Realms are **per tenant**
(`/realms/${TENANT_ID}`), but the IdP hostname is always **`id.${KERNEL_DOMAIN}`**
— never `id.${TENANT_DOMAIN}` or `id.${TENANT_ID}.…`.

| What | Placeholder / URL | Example (tenant `demo`, kernel `desk.gentian.org`) |
|---|---|---|
| IdP base | `https://id.${KERNEL_DOMAIN}` | `https://id.desk.gentian.org` |
| Realm | `/realms/${TENANT_ID}` | `/realms/demo` |
| Full issuer | `https://id.${KERNEL_DOMAIN}/realms/${TENANT_ID}` | `https://id.desk.gentian.org/realms/demo` |
| App login redirect | `https://{sub}.${TENANT_DOMAIN}/…` | `https://projects.demo.desk.gentian.org/…` |
| Portal / post-logout | `https://portal.${KERNEL_DOMAIN}` | `https://portal.desk.gentian.org` |

**Two configuration styles in existing profiles:**

1. **OpenDesk charts with `global.domain` + `global.hosts`** (Element, Jitsi,
   OpenProject, XWiki): set `global.domain: "${KERNEL_DOMAIN}"`,
   `global.hosts.keycloak: "id"`, and prefix **tenant app** host labels with
   `${TENANT_ID}` (`chat.${TENANT_ID}`, `projects.${TENANT_ID}`, …). OIDC
   redirect URIs and public app URLs still use `${TENANT_DOMAIN}`.
2. **Explicit OIDC property blocks** (OpenProject `openproject.oidc.*`, OX
   `com.openexchange.oidc.*`, XWiki `oidc.provider`): every endpoint and issuer
   must use `id.${KERNEL_DOMAIN}/realms/${TENANT_ID}`. Charts without
   `global.hosts.keycloak` (OX) may keep `global.domain: "${TENANT_DOMAIN}"`
   for the app hostname only — but must not derive IdP URLs from it.

The operator seeds OIDC issuer/client credentials in OpenBao as
`https://id.${KERNEL_DOMAIN}/realms/${TENANT_ID}` (stable across vanity
`spec.domain` overrides). Profiles that use `valueMapping.oidc.issuerKey`
(Element/Synapse) rely on that seed; do not hardcode a tenant-scoped IdP host.

**Common IdP mistakes:**

- `global.domain: "${TENANT_DOMAIN}"` with `hosts.keycloak: "id"` → resolves to
  `id.demo.desk.gentian.org` (404). Use `${KERNEL_DOMAIN}` instead.
- Hardcoded realm names (`opendesk`, `souvap`) instead of `${TENANT_ID}`.
- Redirect URIs on `${KERNEL_DOMAIN}` or portal host instead of `${TENANT_DOMAIN}`.

**ACME staging (dev):** when `tenantDNS01ClusterIssuer` contains `staging`,
compositions mount `gentian-staging-ca-tls` and (for Synapse) set
`use_insecure_ssl_client_just_for_testing_do_not_use` so in-cluster OIDC
discovery can reach `id.${KERNEL_DOMAIN}` — see
`gentian-os/docs/design/security.md` §9. `install.sh` and the
gentian-os operator bootstrap this secret in `gentian-dev` and replicate it
into each `tenant-*` namespace; run `./update.sh --acme-issuers` to refresh
the bundle after issuer or kernel cert changes.

---

## 3. Secrets and valueMapping

Secrets are **never** placed in `extraValues`. They travel from OpenBao →
Crossplane `ExternalSecret` → flat Kubernetes `Secret` → Helm `set[]` values.

`valueMapping` maps each secret category to the Helm value path the chart expects:

```yaml
valueMapping:
  oidc:
    clientSecretKey: "openproject.oidc.secret"   # path in Helm values tree
  database:
    # Map ALL FIVE fields — missing any one causes the chart to fall back to
    # a default (usually empty or wrong). This is the most common database bug.
    hostKey:     "postgresql.connection.host"
    portKey:     "postgresql.connection.port"
    nameKey:     "postgresql.auth.database"
    userKey:     "postgresql.auth.username"
    passwordKey: "postgresql.auth.password"
  s3:
    secretKeyKey: "s3.auth.secretAccessKey"       # only the secret key is secret
  smtp:
    passwordKey: "environment.OPENPROJECT_SMTP__PASSWORD"
  ldap:
    bindPasswordKey: "environment.OPENPROJECT_SEED_LDAP_..._BINDPASSWORD"
```

Non-secret fields (S3 endpoint, S3 bucket name, S3 access key ID, LDAP host, SMTP
host) belong in `extraValues` using placeholders, **not** in `valueMapping`.

---

## 4. App-level secrets (appSecrets)

Use `appSecrets` for secrets that belong to the app itself and are not provisioned
by a kernel reconciler (e.g. initial admin passwords):

```yaml
appSecrets:
  - name: admin_password
    valuePath: "openproject.admin_user.password"
```

The orchestrator seeds these into OpenBao under
`gentian-os/tenants/{tenant}/apps/{app}/internal/{name}` and injects them via
the Crossplane `ExternalSecret` → Helm `set[]` pipeline at deploy time.

---

## 5. Ingress, TLS and CORS

### 5a. How TLS is provisioned

Setting `spec.ingress` (with `tlsEnabled: true`, the default) tells the
gentian-os controller to create:

1. A Kubernetes `Ingress` at `{subDomain}.{effectiveDomain}` → `Service:{servicePort}`.
2. One **DNS-01 wildcard** cert-manager `Certificate` per tenant
   (`tenant-{tenant}-wildcard-tls`) covering `*.{effectiveDomain}`.

All app ingresses for that tenant reference the same TLS secret.
**Do not** set per-app HTTP-01 issuers in profiles — `spec.ingress.clusterIssuer`
is reserved for a possible future mode and is **ignored** by the operator today.
See [gentian-os/docs/architecture.md](../gentian-os/docs/architecture.md) §6.1.

```yaml
ingress:
  subDomain: "projects"        # → projects.demo.desk.gentian.org
  serviceName: "openproject"   # must match the Kubernetes Service name
  servicePort: 8080
  ingressClassName: "nginx"
  tlsEnabled: true             # default
```

**`subDomain` capitalization matters.** The field is validated; `subdomain`
(lowercase) is silently ignored, leaving the app with no ingress.

### 5b. Always disable chart-managed ingress

Every chart that ships its own `Ingress` resource must have it disabled,
otherwise two `Ingress` objects collide on the same host:

```yaml
extraValues:
  ingress:
    enabled: false
```

### 5c. Predictable Service name

The Crossplane composition generates a random Helm release name. If the chart
derives its Service name from the release name, the operator cannot predict it.
Set `fullnameOverride` to lock the Service name:

```yaml
extraValues:
  fullnameOverride: "openproject"   # must match spec.ingress.serviceName
```

### 5d. CORS — why most apps need nothing extra

Gentian OS avoids browser CORS issues by architecture:

- Apps load in **iframes** from the Gentian Portal on `portal.{kernelDomain}`;
  app UIs are on `{sub}.{tenantDomain}` (cross-origin). The operator injects
  `frame-ancestors` for the kernel portal origin.
- **OIDC** token exchange is server-side — no `fetch()` to a foreign origin.
- When the shell itself needs to call an app's API, declare a
  `spec.browserProxy` route (see §5e). The shell server proxies the call
  server-side; the browser sees a same-origin request.

If your app's own UI calls back to its own backend (normal REST/XHR to the same
host), no CORS configuration is needed.

### 5e. Shell proxy for app APIs (`spec.browserProxy`)

Declare a `browserProxy` route when the gentian shell (not the app's own UI)
needs to call the app's API from the browser. The shell exposes
`/api/apps/{appName}/{path}` and forwards requests to the cluster-internal
service, injecting the user's bearer token.

```yaml
browserProxy:
  - path: api
    target: "http://openproject.{namespace}.svc/api/v3/"
    authMode: forward-bearer   # default — forwards the user's Bearer token
    stripPrefix: true          # default — strips /api/apps/{name}/api before forwarding
```

**When you need it:** the shell calls the app's REST API to show a widget,
badge count, or AI context. **When you don't:** the app's own UI calls its own
backend (same host, no CORS).

---

## 6. Portal iframe embedding (CSP / `frame-ancestors`)

Gentian apps open inside the **kernel portal** (`https://portal.${KERNEL_DOMAIN}`)
in an iframe (gentian-ui window manager). The app UI is cross-origin
(`https://chat.${TENANT_DOMAIN}`, etc.). Browsers block embedding unless the app
response explicitly allows the portal origin in **`Content-Security-Policy:
frame-ancestors`**.

### 6a. Firefox “will not allow … if another site has embedded it”

That message means the app's CSP (or `X-Frame-Options`) does **not** include
`https://portal.${KERNEL_DOMAIN}` in `frame-ancestors`.

**Do not fix this in AppProfiles.** The gentian-os operator injects NGINX
`configuration-snippet` directives on every app `Ingress` it manages.

| Check | Action |
|---|---|
| Portal URL | Users must use `portal.${KERNEL_DOMAIN}` — **not** `portal.${TENANT_DOMAIN}` (404 on multi-tenant). |
| Profile `ingress.annotations` | **Never** add `X-Frame-Options`, `frame-ancestors`, or `Content-Security-Policy` — the operator owns this. Legacy per-tenant portal origins (`portal.${TENANT_DOMAIN}`) are stripped on reconcile. |
| `linkTarget` | `embedded` and `newwindow` both load inside gentian-ui; CSP must still allow the kernel portal. Default `newwindow` is fine. |
| Operator version | Reconcile the tenant after upgrading gentian-os so ingress snippets are updated. |

### 6b. Double CSP headers (Element and similar)

Some charts (notably **Element** / opendesk-element-web nginx) already send:

```http
Content-Security-Policy: frame-ancestors 'self'
```

If ingress **appends** a second header with the portal origin, the browser
enforces **both** policies — `'self'` still blocks embedding from
`portal.${KERNEL_DOMAIN}`. Symptom: portal tile opens but the iframe is blank
with the Firefox message above; `curl -sI` shows **two** `content-security-policy`
lines.

The operator **replaces** upstream CSP for standard AppProfile ingresses
(`chat`, `meet`, `projects`, `webmail`, …): it uses `proxy_hide_header` (stock
ingress-nginx; microk8s lacks `more_clear_headers`) to strip upstream
`X-Frame-Options` and `Content-Security-Policy`, then sets a single:

```http
Content-Security-Policy: frame-ancestors 'self' https://portal.<kernel-domain>
```

**Exception — CryptPad** (`pad` / `pad-sandbox` kernel ingresses only): the
operator **appends** a second CSP header so upstream `script-src` (no
`'unsafe-eval'`) stays intact. Do not copy CryptPad's append-only pattern into
AppProfiles.

### 6c. AppProfile checklist (all profiles)

These profiles rely on the operator and need **no** CSP annotations:

- `element`, `jitsi`, `openproject`, `ox-appsuite`, `xwiki`

Add only non-CSP ingress annotations your chart needs (proxy timeouts, body size):

```yaml
ingress:
  subDomain: chat
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "100M"
    # NO frame-ancestors / X-Frame-Options / Content-Security-Policy here
```

**No other app-level CORS setup is required** for normal same-origin app UIs.
TLS and `browserProxy` bearer forwarding are platform-managed (§5d–5e).

**Force ingress reconcile:** after an operator upgrade, bump the tenant to refresh
ingress annotations (the `gentianos.io/reconcile` timestamp annotation alone does
not change `spec` — patch any field or delete the app ingress and let the operator
recreate it):

```bash
kubectl delete ingress -n tenant-demo ingress-demo-element
# operator recreates on next tenant reconcile (~seconds)
```

### 6d. Element SSO — OIDC redirect URI host

Element Web is served at `chat.<tenant-domain>` but the Matrix homeserver (Synapse)
and OIDC callback live at **`matrix.<tenant-domain>`** (synapse-web ingress).
Keycloak `redirectUris` must target the homeserver host:

```yaml
kernelRequirements:
  identity:
    oidc:
      redirectUris:
        - "https://matrix.${TENANT_DOMAIN}/_synapse/client/oidc/callback"
```

Using `chat.${TENANT_DOMAIN}` causes OIDC to fail after Keycloak login; Element
shows **“Invalid username or password”** even though credentials are correct.
Reconcile the tenant / identity jobs after fixing the AppProfile so the Keycloak
client `opendesk-synapse` picks up the new redirect URI.

**Matrix localpart:** use `matrixIdLocalpart: "opendesk_username"` (LDAP `uid`) and
request scope `opendesk-matrix-scope`. Do not use `preferred_username` — kernel-broker
tokens may carry `mailPrimaryAddress` there, which is not a valid Matrix localpart.

**Loading screen / `net.nordeck.element_web.module.opendesk` error:** the
`opendesk-element-web` image bundles the Nordeck OpenDesk module; `additionalConfiguration`
must include its `banner` URLs (`portal_url`, `ics_*`, `portal_logo_svg_url`) and
`custom_css_variables` — see `profiles/element.yaml`.

**Wrong user after switching portal accounts:** portal login uses the **kernel** realm;
Element/Synapse OIDC uses the **tenant** realm (`demo`, …). A previous user's tenant-realm
SSO cookie or cached Matrix session in the browser can reopen Chat as the wrong person.
The Element AppProfile sets `logout_redirect_url`; gentian-ui app tiles pass `login_hint`
and `prompt=login` (and `#/logout` on `chat.*`) when opening SSO apps from the portal.

### 6e. IdP login inside a portal-embedded app (Keycloak `frame-ancestors`)

Portal tiles load tenant apps in an iframe (`portal.<kernel>` → `chat.<tenant>.<kernel>`).
OIDC SSO then loads **`id.<kernel>` inside the app iframe**. Firefox blocks with
*“id… will not allow … if another site has embedded it”* when Keycloak's CSP only
allows `https://portal.<kernel>` — the **immediate parent** is the app origin, not
the portal.

CSP `host-source` allows at most one `*.` label. `https://*.<kernel>` does **not**
match `chat.demo.<kernel>`; each tenant needs **`https://*.<tenant-effective-domain>`**
(e.g. `https://*.demo.desk.gentian.org`).

| Layer | Who sets CSP | Must allow |
|---|---|---|
| App ingress (`chat`, `meet`, …) | gentian-os operator | `https://portal.<kernel>` (§6a–6b) |
| IdP ingress (`id.<kernel>`) | nubus Helm values (`kernel/services/nubus/manifests/<env>/values/`) **and** gentian-os operator | `https://portal.<kernel>` **and** `https://*.<tenant-domain>` per Tenant CR |

Helm values provide the install baseline; the operator patches the Keycloak proxy
ingress on every tenant reconcile when tenants are added or removed. The operator
also clears Keycloak **`X-Frame-Options: SAMEORIGIN`** on kernel and tenant realms
(broker `/endpoint` callbacks fail in iframes even when `frame-ancestors` is correct).
After changing nubus values in Git, sync the nubus manifests app so Crossplane
reapplies the release. Verify:

```bash
curl -sI https://id.${KERNEL_DOMAIN}/ | grep -i content-security
# expect: frame-ancestors 'self' https://portal.<kernel> https://*.demo.<kernel> …
```

### 6b. Kernel diagram service (CryptPad)

Diagram editing from Nextcloud Files uses a **shared CryptPad kernel service**
(like Collabora in §9b of `gentian-os/docs/architecture.md`), not a per-tenant
AppProfile. One instance at `pad.<kernel_domain>` plus
`pad-sandbox.<kernel_domain>` for the crypto sandbox origin serves all tenants;
Nextcloud embeds it from `files.<kernel_domain>`.

There is **no portal tile** and **no tenant ingress** — manifests live under
`gentian-os/kernel/services/cryptpad/`. CSP `frame-ancestors` on the kernel
ingresses must allow Nextcloud and the portal; do not use `more_clear_headers`
(microk8s ingress-nginx lacks it) — append with `add_header … always` only.

### 6c. Kernel file share (Nextcloud Files)

Nextcloud Files is a **shared kernel service** at `files.<kernel_domain>` (not an
AppProfile). The gentian-os operator does **not** manage its Ingress — CSP lives in
`gentian-os/kernel/services/nextcloud/manifests/<env>/configmap.yaml` and is
re-applied by `./update.sh --nextcloud-office`.

Use the same **replace** pattern as Element (§6b): `proxy_hide_header` for upstream
`X-Frame-Options` and `Content-Security-Policy`, then a single
`add_header Content-Security-Policy "frame-ancestors 'self' https://portal.<kernel-domain>" always`.
Do **not** use `more_clear_headers` or `more_set_headers` — they are ignored on
microk8s and leave `X-Frame-Options: SAMEORIGIN`, which blocks the portal iframe.

---

## 7. Global domain and hosts

Many Bitnami-family and opendesk charts read `global.domain` and `global.hosts`
to build their internal URLs. If these are missing, charts fall back to
`example.com` defaults, which breaks inter-service communication.

When the chart lists `keycloak` under `global.hosts`, **`global.domain` must be
`${KERNEL_DOMAIN}`** so Keycloak resolves to the central IdP (see §2). Prefix
tenant app labels with `${TENANT_ID}`; keep user-facing redirect URIs on
`${TENANT_DOMAIN}`:

```yaml
extraValues:
  global:
    domain: "${KERNEL_DOMAIN}"
    hosts:
      openproject: "projects.${TENANT_ID}"  # → projects.demo.desk.gentian.org
      keycloak: "id"                        # → id.desk.gentian.org
      nubus: "portal"                       # → portal.desk.gentian.org
```

### 7b. Jitsi + Element (video in Matrix rooms)

OpenDesk keeps Jitsi and Element as **separate AppProfiles** in the same tenant.
To enable conference widgets:

1. Install both `element` and `jitsi` on the tenant (`spec.apps`).
2. Element's `optionalIntegrations` declares `videoconference` from provider `jitsi`
   (creates an `IntegrationBinding`; no extra Helm wiring in the binding itself).
3. The `app-element` composition deploys **Matrix User Verification Service**
   (UVS bootstrap Job + service). Jitsi Prosody must use `AUTH_TYPE=hybrid_matrix_token`,
   `JWT_APP_SECRET` (same value as `settings.jwtAppSecret` / keycloak adapter), and
   `MATRIX_UVS_URL=http://opendesk-matrix-user-verification-service.${TENANT_NAMESPACE}.svc.cluster.local`.
   The Element composition sets `fullnameOverride: opendesk-matrix-user-verification-service`
   on the UVS release so that DNS name resolves (without it, Prosody points at a non-existent Service).
   After OIDC login, the web client must receive `?jwt=…` (not only `?oidc=authorized`); otherwise users
   join as `@guest.meet.jitsi` and see “Waiting for a moderator”. Enable
   `enableUserRolesBasedOnToken: true` in `jitsi.web.extraConfig`.
   Gentian mounts persistent OIDC/JWT overlays from `gentian-os/overlays/jitsi/`
   (via `app-default` composition): top-level `window.top.location` for portal iframe
   SSO, JWT URL normalization after `oidc=authorized`, and Keycloak adapter
   `preferred_username` fallback when `opendesk_username` is absent.
4. Set `global.hosts.jitsi: "meet.${TENANT_ID}"` in **both** profiles (with
   `global.domain: "${KERNEL_DOMAIN}"`) so Element's bundled `jitsi.html`
   widget targets `https://meet.${TENANT_DOMAIN}`.
5. Configure shared TURN in `gentian-deployments` → `kernelServices.turn*` on the
   gentian-os Helm chart; compositions substitute `${TURN_*}` into Element Synapse
   and Jitsi Prosody env vars.
6. **UVS bootstrap** uses `opendesk-synapse-create-account`, which logs in as `@uvs`
   with a local Matrix password after `register_new_matrix_user`. Do **not** set
   `password_config.enabled: false` on Synapse — that breaks the bootstrap Job
   (`Password login has been disabled`) and leaves the Element XApp Not Ready.
   Human users still use OIDC: `app-element` sets `sso_redirect_options.immediate`
   on the Element web `config.json` only.
7. **Retry after a bootstrap failure:** the chart hook runs at post-install only.
   After Synapse password auth is restored, delete the failed Job and let Crossplane
   retry the bootstrap `Release` (or delete `opendesk-matrix-user-verification-service-bootstrap`
   and re-sync the Element `App` claim):

   ```bash
   kubectl delete job -n tenant-demo opendesk-matrix-user-verification-service-bootstrap --ignore-not-found
   ```

---

## 8. OIDC client spec

Use the full `OIDCClientSpec` struct. The old `oidc: true` shorthand is removed:

```yaml
kernelRequirements:
  identity:
    oidc:
      clientId: "opendesk-openproject"
      name: "OpenProject"
      accessType: CONFIDENTIAL   # or PUBLIC for browser-only apps (e.g. Jitsi)
      redirectUris:
        - "https://projects.${TENANT_DOMAIN}/auth/keycloak/callback"
      postLogoutRedirectUris:
        - "https://projects.${TENANT_DOMAIN}/"
      backchannelLogoutUrl: "https://projects.${TENANT_DOMAIN}/auth/keycloak/backchannel-logout"
```

**`PUBLIC` vs `CONFIDENTIAL`:**
- `CONFIDENTIAL` — server-side apps that can keep a client secret (OpenProject,
  OX App Suite, Element). Requires `valueMapping.oidc.clientSecretKey`.
- `PUBLIC` — browser-only or native apps where a secret cannot be protected
  (Jitsi). No `clientSecretKey` needed.

**Per-tenant realm:** All OIDC/LDAP URLs must use `${TENANT_ID}` as the Keycloak
realm name. Never hardcode `souvap`, `opendesk`, or any literal realm name.

**Central IdP host:** Issuer, authorization, token, JWKS, and logout endpoints
must use `https://id.${KERNEL_DOMAIN}/realms/${TENANT_ID}` (or path-only auth
URLs resolved against `openproject.oidc.host: "id.${KERNEL_DOMAIN}"`). See §2.

---

## 9. Secret rotation — Reloader annotation

Add the Stakater Reloader annotation so pods restart automatically when the
`ExternalSecret` refreshes (e.g. after credential rotation):

```yaml
extraValues:
  podAnnotations:
    reloader.stakater.com/auto: "true"
```

---

## 10. Composition reference

Only set `compositionRef` when using a non-default composition:

| Composition | When to use |
|---|---|
| *(omit)* | Standard apps — `app-default` is used automatically |
| `app-element` | Element (Matrix) — uses the element-specific composition |
| `app-ox` | OX App Suite — uses the ox-specific composition |

---

## 11. Image pull secrets

Charts pulling from private registries (e.g. `registry.opencode.de`) need:

```yaml
extraValues:
  global:
    imagePullSecrets:
      - name: registry-credentials
```

The `registry-credentials` secret is provisioned in every tenant namespace by
the namespace bootstrap step.

---

## 12. Checklist for a new AppProfile

Before opening a PR, verify:

- [ ] `deploymentMethod: crossplane`
- [ ] `subDomain` is camelCase
- [ ] No hardcoded cluster addresses — all use `${...}` placeholders
- [ ] All five database fields mapped in `valueMapping.database`
- [ ] Chart-managed ingress disabled (`ingress.enabled: false` in `extraValues`)
- [ ] `fullnameOverride` set to match `spec.ingress.serviceName`
- [ ] `global.domain` and `global.hosts` set in `extraValues`
- [ ] If `global.hosts.keycloak` is present: `global.domain` is `${KERNEL_DOMAIN}`, tenant app hosts use `${TENANT_ID}` prefix
- [ ] All IdP URLs use `id.${KERNEL_DOMAIN}/realms/${TENANT_ID}`; redirect URIs use `${TENANT_DOMAIN}`
- [ ] Element: OIDC redirect is `https://matrix.${TENANT_DOMAIN}/_synapse/client/oidc/callback` (not `chat.`)
- [ ] OIDC uses full `OIDCClientSpec`, realm is `${TENANT_ID}`
- [ ] Secrets only in `valueMapping` / `appSecrets`, never in `extraValues`
- [ ] `reloader.stakater.com/auto: "true"` in `podAnnotations`
- [ ] (automatic) Operator injects portal `frame-ancestors` on app Ingress — no CSP annotations in profile
- [ ] `ingress.annotations` contains no `frame-ancestors`, `X-Frame-Options`, or `Content-Security-Policy`
- [ ] (CryptPad / multi-host) `additionalIngresses` use flat subdomains; no per-host CSP in annotations — operator sets `pad-sandbox` policy
- [ ] `spec.browserProxy` declared if the shell calls this app's REST API
- [ ] `compositionRef` omitted unless using a non-default composition
- [ ] YAML passes `python3 -c "import yaml; yaml.safe_load(open('<file>'))"` locally
