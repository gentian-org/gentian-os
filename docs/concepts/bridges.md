---
audience: architect, app-author
type: concept
---

# Bridges

**Companion to:** [app-catalog.md §13](app-catalog.md#13-the-component-model-kinds-classes-and-axes)

---

## 1. What a bridge is

A bridge connects, for a person or for the tenant, across the boundary of the
browser's session, in either direction. A file sync program on a laptop, a
calendar program, an agent that works while the person is away: none of them
holds the session a person has in the browser, and each needs a way to an app
of the tenant, or out of it, that the platform checks.

A bridge is a component of class `bridge`. It is a class of its own: nobody
opens it, it exposes, provides and integrates nothing of its own, it never
vouches for people, and it is never placed. A tenant's administrator installs
it beside the app it connects. What the platform opens for it is read from its
**ways**, and nothing else is opened for it.

## 2. Ways

A bridge declares one or more ways. A way is one direction of calls:

| Field | Values | Says |
|---|---|---|
| `caller` | `device`, `outside`, `agents`, `app` | who opens the calls: a program on a person's device, a service outside the platform, the tenant's agents, the app the bridge connects |
| `callee` | `app`, `agents`, `outside`, `partner` | who answers them: the app the bridge connects, the tenant's agents (started by the call), a service outside the platform, a partner's service |
| `protocol` | `mcp`, `webdav`, `caldav`, `carddav` | what the calls are made in; one protocol per way |
| `speaksAs` | `person`, `tenant` | whom the calls are made for; `tenant` only for what every member of the tenant may see |
| `paths` | path prefixes | where a caller from outside the platform reaches the app; present exactly for a way from `device` or `outside` to `app` |
| `root` | a path under `paths` | where a calendar or contact program is sent from the discovery address; only for a way in `caldav` or `carddav`, one per protocol in a bridge |

A bridge in both directions declares two ways. Each way is approved on its own.

`spec.bridge.app` names the app of the tenant a way with caller or callee `app`
means. It is present exactly when such a way is.

The fifth part of a way, its delivery, is the bridge's: `package.api` when the
app speaks the protocol itself and the platform only routes to it, a chart when
a translator runs ([app-catalog.md §13.2](app-catalog.md#132-three-axes)). All
ways of a bridge share it. A bridge that needs both is two profiles.

## 3. Names

| Name | Caller | Callee |
|---|---|---|
| Sync Bridge | `device` | `app` |
| Agent Bridge | `agents` | `app` |
| Service Bridge | `agents` | `outside` |
| Partner Bridge | `agents` | `partner` |
| Event Bridge | `outside` | `agents` |

The names are for prose and the user interface; the schema has the ways.

## 4. What the platform serves

Two kinds of way are served:

- a way from `device` to `app` in `webdav`, `caldav` or `carddav`, of a bridge
  packaged as `api` with a `backend`, at the tenant's **door** (§4.1);
- a way from `agents` in `mcp`, to the tenant's components that call tools for
  a person (§4.4).

A bridge with neither is held with the reason `WayUnsupported`.

```yaml
spec:
  classes: [bridge]
  launch: none
  bridge:
    app: {profile: nextcloud-base-ce}
    ways:
    - name: files
      caller: device
      callee: app
      protocol: webdav
      speaksAs: person
      paths: [/remote.php/dav/files/, /remote.php/dav/uploads/]
    - name: calendars
      caller: device
      callee: app
      protocol: caldav
      speaksAs: person
      paths: [/remote.php/dav/calendars/, /remote.php/dav/principals/]
      root: /remote.php/dav/principals/
  package:
    api:
      backend: {service: nextcloud, port: 8080}
```

### 4.1 The door

The door is `sync.<the tenant's domain>`. The label `sync` is reserved: no app
takes it ([routing.md](routing.md)).

- **Sign-in.** A program presents one of two credentials, both issued and
  ended by the tenant's realm:
  - a **token**, for a program that can sign the person in: the
    authorization code flow with PKCE through the realm client
    `token-entry-<component>`, back to a loopback address on any port. The
    token is made out to the door. The sign-in, the second factor and the
    session limits are the realm's.
  - a **device password** (§4.2), as HTTP Basic credentials, for a program
    that knows only a username and a password: most calendar and contact
    programs.
- **One client, ways approved one by one.** A bridge has one route and one
  realm client for all its ways. The tenant's perimeter approver approves each
  way as a token entry (`kubectl gentian exposures requests`), and the route
  serves the paths of the ways approved and no other. The client exists while
  at least one way is approved.
- **Every request.** The bouncer verifies the token, asks whether the person
  may use the app (`can_use` on the app, the question the app's own address
  asks), and exchanges the token at the realm for one made out to the app
  alone (scope `bridge-<app profile>`). The app is handed that token, never the
  one the program presented. A request without a credential is answered `401`
  with the address of the door's resource metadata (RFC 9728) and, where the
  realm takes device passwords, a second challenge for a username and a
  password; a path no approved way serves is answered `404`.
- **Discovery.** For a way that names a `root`, the door serves its
  protocol's discovery address (RFC 6764: `/.well-known/caldav`,
  `/.well-known/carddav`) and answers it with a `301` to the root, before
  anybody is asked who is calling. A program set up with the door's name
  alone finds the app from there.
- **No path rewriting.** A path is asked of the app as the program asked for
  it: DAV answers name paths of their own.
- **One owner per path.** A path at the door is the first bridge's that served
  it. Another bridge that asks for the same path, or one under or over it, is
  not served (condition `Bridge`, reason `PathTaken`).

The app's one obligation is to accept a bearer token of the tenant's realm made
out to its own sign-in client. Nextcloud does with `user_oidc` and
`check-bearer`.

### 4.2 Device passwords

A device password is a credential of the person in the tenant's realm, beside
their password and second factor. The person makes one in their own settings,
for one device and the ways they choose; the password is shown once.

| | |
|---|---|
| Made | by the person, through the registrar (`create-device-password`), within ten minutes of a sign-in with whatever second factor the realm asks; an older sign-in is answered with a step-up challenge (RFC 9470) |
| Form | five groups of five characters, generated; the first group names it among the person's own, the rest is a secret of about 99 bits, kept as a digest |
| Username | the person's username or email address in the realm |
| Limited to | the ways chosen when it was made; a path no such way serves is refused |
| Ends | when the person revokes it, changes or resets their password, or is signed out of every session by an administrator; when unused for longer than the realm's offline session idle timeout (30 days unless the realm says otherwise); a disabled person's is refused |
| Count | at most 25 per person |

The bouncer posts the username and the password to the realm as a password
grant of the realm client `gentian-door-devices`, whose direct grant runs one
step: the platform's device password check. That client cannot check a
person's password, and no other client can check a device password. The
realm answers with a token of the person made out to the door, carrying the
password's ways (`gentian_ways`), and the request is decided as one that
presented it. The bouncer keeps the token until shortly before it runs out,
so the realm is asked about once in five minutes per device. A wrong
password does not count towards the realm's lockout of the person; the door
counts failures per username and per source address instead and answers
`429` past its limit, without asking the realm.

### 4.3 Conditions

| Condition | Reason | Means |
|---|---|---|
| `Bridge` | `Connected` | the app is installed and the paths are the bridge's own |
| `Bridge` | `AppNotInstalled` | the app the bridge connects is not installed in the tenant |
| `Bridge` | `PathTaken` | another component serves one of the paths at the door |
| `TokenEntry` | `Approved`, `ApprovalWaived` | the ways named in the message are served |
| `TokenEntry` | `AwaitingApproval` | no way is approved |
| `Ready` | `WayUnsupported` | no way of the bridge is one the platform serves |

### 4.4 Ways from the agents

A bridge with a way from `agents` (an Agent, Service or Partner Bridge) runs a
tool server (MCP). Its callers are the tenant's components that check every
tool call made for a person: those that declare
`requires.services.vouching`. A bridge has at most one such way.

```yaml
spec:
  classes: [bridge]
  launch: none
  package: {chart: {repository: oci://example/charts, name: openproject-tools, version: "1.0.0"}}
  bridge:
    app: {profile: openproject, versions: ["15.4.0"]}
    ways:
    - name: tools
      caller: agents
      callee: app
      protocol: mcp
      speaksAs: person
      endpoint: {service: openproject-tools, port: 8765, path: /mcp}
    addonsValuesPath: activeModules
```

- **Network.** The bridge's pods are left out of the tenant's baseline and
  admit only the callers; the callers may reach every such bridge. A bridge
  reaches the app it connects and nothing else in the tenant, and that app
  admits it ([networking.md](networking.md)).
- **The list.** Each caller finds a ConfigMap `agent-bridges-<component>` in
  its namespace. Its one key, `AGENT_BRIDGES`, is a JSON object from a bridge's install
  name to its address inside the tenant and the app a call through it is for:
  `{"openproject-tools": {"url": "http://openproject-tools.tenant-acme.svc.cluster.local:8765/mcp", "app": "openproject"}}`.
  A bridge without an `endpoint` is installed and on no list.
- **Whom a call is for.** The app the bridge connects, or, for a bridge that
  connects none, the bridge itself: the rights check asks `can_use` on it, and
  the token the bridge is handed is for it (scope `app-<app>`).
- **Callee `outside`.** The bridge may be packaged as `api` with a `baseUrl`:
  a tool server outside the cluster, reached where it is. It runs nothing in
  the tenant; the egress granted to it is given to its callers: a granted
  host through the outgoing gateway ([edge.md §3.3](edge.md)), a rule by
  address in their policy.
  The platform does not ask such a server what its tools are. The way says
  it, under `outside`, and the callers are told of the bridge only when it
  does and the address is `https`:

  ```yaml
  package: {api: {baseUrl: https://tools.example/mcp}}
  bridge:
    ways:
    - name: tools
      caller: agents
      callee: outside
      protocol: mcp
      speaksAs: person
      outside:
        tools:
        - name: list-files          # the name on the platform
          tool: list_files          # the name at the server; absent, the same
          version: 1
          class: read               # read or change
          description: Lists the person's recordings.
          inputSchema: {type: object, properties: {limit: {type: integer}}}
        signIn:
          oauth: {issuer: https://tools.example, scopes: []}
  ```

  A tool the way does not name is not called. `version` is raised when a
  tool is reviewed again. `signIn.oauth` names the authorization server a
  person signs in at (its metadata is read at
  `/.well-known/oauth-authorization-server`, RFC 8414); the liaison, the
  component that keeps people's connections, obtains the credential a call
  carries. The bridge declares no egress for that host: the liaison reaches
  it once the bridge's own requests are answered ([edge.md §3.3](edge.md)). The list
  entry of such a bridge carries the address, the bridge itself as `app`,
  and the whole of `outside`.
- **Callee `partner`.** The bridge names the partner's realm in
  `requires.services.partners.call`, and its token is exchanged for an
  assertion towards the partner.
- **Add-ons.** With `addonsValuesPath`, the bridge is told at that chart value
  the IDs of the add-ons selected into the app it connects, and is rendered
  again when they change.
- **Actions.** An action names the bridges whose tools the calling component
  may call on a run's behalf in `spec.action.agentBridges`.
