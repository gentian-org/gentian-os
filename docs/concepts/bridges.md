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

A way from `device` to `app` in `webdav`, `caldav` or `carddav`, of a bridge
packaged as `api` with a `backend`, is served at the tenant's **door**. A
bridge with no such way is held with the reason `WayUnsupported`.

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
  package:
    api:
      backend: {service: nextcloud, port: 8080}
```

### 4.1 The door

The door is `sync.<the tenant's domain>`. The label `sync` is reserved: no app
takes it ([routing.md](routing.md)).

- **Sign-in.** A program signs the person in at the tenant's realm with the
  authorization code flow and PKCE, through the realm client
  `token-entry-<component>`, and is sent back to a loopback address on any
  port. Its token is made out to the door. The sign-in, the second factor and
  the session limits are the realm's.
- **One client, ways approved one by one.** A bridge has one route and one
  realm client for all its ways. The tenant's perimeter approver approves each
  way as a token entry (`kubectl gentian exposures requests`), and the route
  serves the paths of the ways approved and no other. The client exists while
  at least one way is approved.
- **Every request.** The bouncer verifies the token, asks whether the person
  may use the app (`can_use` on the app, the question the app's own address
  asks), and exchanges the token at the realm for one made out to the app
  alone (scope `bridge-<app profile>`). The app is handed that token, never the
  one the program presented. A request without a token is answered `401` with
  the address of the door's resource metadata (RFC 9728); a path no approved
  way serves is answered `404`.
- **No path rewriting.** A path is asked of the app as the program asked for
  it: DAV answers name paths of their own.
- **One owner per path.** A path at the door is the first bridge's that served
  it. Another bridge that asks for the same path, or one under or over it, is
  not served (condition `Bridge`, reason `PathTaken`).

The app's one obligation is to accept a bearer token of the tenant's realm made
out to its own sign-in client. Nextcloud does with `user_oidc` and
`check-bearer`.

### 4.2 Conditions

| Condition | Reason | Means |
|---|---|---|
| `Bridge` | `Connected` | the app is installed and the paths are the bridge's own |
| `Bridge` | `AppNotInstalled` | the app the bridge connects is not installed in the tenant |
| `Bridge` | `PathTaken` | another component serves one of the paths at the door |
| `TokenEntry` | `Approved`, `ApprovalWaived` | the ways named in the message are served |
| `TokenEntry` | `AwaitingApproval` | no way is approved |
| `Ready` | `WayUnsupported` | no way of the bridge is one the door serves |
