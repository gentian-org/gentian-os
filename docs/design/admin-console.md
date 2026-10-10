# Administration console — design

**Scope:** what the administration console is, what each of its screens does and which service answers it, and what is not built.

**Companion docs:** [architecture.md](../architecture.md), [iam.md](iam.md), [security.md](security.md), [multi-tenancy.md](multi-tenancy.md), [routing.md](routing.md), [store-contract.md](store-contract.md), [resource-plans.md](resource-plans.md).

---

## 1. Purpose

Every tenant needs a place to govern **who** belongs to it and **which apps**
each person may use. The administration console is that place. It is **an
app**: a component with its own profile (`admin-console`, shipped by the
operator chart with `defaultForTenants: true`), so every tenant has one at
`admin.<tenant domain>`, behind the tenant's session. It reaches a person as a
tile on the desktop, shown to whoever holds `can_administer` on the tenant and
to nobody else. The platform tenant has one too, at `admin.platform.<KERNEL_DOMAIN>`.

A console administers the tenant it is installed in and no other. A platform
administrator reaches another tenant through that tenant's own console.

The code is in [gentian-ui](https://github.com/gentian-org/gentian-ui),
`apps/admin-console`: a React frontend and a small backend. **The console
holds no credential, keeps no state and decides nothing.** Its backend checks
the token the edge forwards and relays each call, with that token, to one of
four services in `kernel-control`; what a person may see or change is that
service's answer from OpenFGA.

| Service | Asked for |
|---|---|
| **director** | Everything that is declared state: a write is a commit to the deployments repository, authored as the person. Also actions that happen once (take an export, publish a notice, purge an app's data) |
| **usher** | Reads of what the cluster holds: installed components and their state, resources, exports, notices |
| **registrar** | People and groups. It holds one Keycloak credential per realm; the console holds none |
| **custodian** | Credentials a person sets: repository credentials, the backup identity |

### 1.1 Screens

| Screen | What it does | Answered by |
|---|---|---|
| **Tenants** (platform administrator) | Create, retire and purge tenants, import one from a bundle, issue a tenant administrator's activation link | director, registrar |
| **Members** | List, invite, update, switch off, remove; password reset; require or remove an authenticator; mailboxes of removed people (§4.3a) | registrar |
| **Groups** | Custom groups and who is in each group, the app groups included | registrar |
| **Apps** | The tenant's installed apps. Per app: state, who has access and whether it is for everyone, integrations, what it asked of the platform and what was approved, public addresses and their approval, uninstall, purge. Uninstall keeps the app's data; purge destroys the data of an app that is no longer installed ([store-contract.md](store-contract.md) §8) | director, usher |
| **Resources** | Plan, ceiling, usage history (§4.8) | usher (read), director (write) |
| **Export** | Take one export now, list and download what exists | usher (read), director |
| **Security** | The realm's password, session and lockout policy (§4.5) | registrar (password), director (session, lockout) |
| **Integrations** | What the tenant's apps consume from each other, and the grants | director |
| **Credentials** | Repository credentials, backup identity | custodian, director |
| **Notifications** | Publish a notice to the tenant's people (§5) | director, usher (read) |
| **Audit** | Changes to declared state, from git (§4.7) | director |
| **Cluster settings**, **Platform security**, **Customization**, **Licence report** (platform administrator) | The Cluster claim's settings, permitted waivers, customization debt, the last licence report | director |
| **Models** (platform administrator) | The models the model gateway offers, which are the Cluster claim's: providers and their models, the models the cluster serves itself, and the two switches. Each model is flagged when it cannot answer. No token is entered here (§6a) | director; custodian (read, for the token's state) |
| **Catalogues** | Hidden. The cluster renders no catalogue of its own; the screen is kept and not linked |

**Nothing is installed from the console.** Apps come from the App Store app, a
component of its own beside the console ([store-contract.md](store-contract.md)
§6), or from `kubectl gentian apps install` where there is no store.

---

## 2. Placement in the security model

Tenant isolation rests on two independent layers (see [security.md](security.md)):

| Layer | Mechanism |
|---|---|
| **MAC backbone** | `tenant-<name>` namespaces, default-deny NetworkPolicy, Kyverno |
| **Identity domain** | One Keycloak realm per tenant |

Keycloak Organizations (one realm, logical tenants) are not used: they share
one user database. See [iam.md §1](iam.md#1-identity-topology-suze--keycloak-native).

Authority in the console is not read from a token's groups. Which screens a
person gets is decided from two answers of the director: the verbs they hold
on the tenant and on the cluster. Every relayed call is checked again by the
service that receives it.

---

## 3. Identity topology

Realms, sign-in, the group names and the member/administrator split are
described once, in [iam.md §1.1–§1.3](iam.md#1-identity-topology-suze--keycloak-native).
What the console relies on:

- People and groups of a tenant are in the tenant's own realm; the platform
  tenant's are in the `kernel` realm.
- The login name is the email address. `gentian.inviteEmail` is an optional
  second address used for the invitation, password reset and recovery.
- Administrator and member are separate accounts: `can_use` on an app excludes
  a tenant's administrators, so an administrator's desktop shows administration
  tiles and a member's shows apps.

---

## 4. What the screens do

### 4.1 One console, scoped by what the person holds

The same image runs for every tenant. The screens marked *platform
administrator* in §1.1 are shown only to a person who holds the cluster's
verbs; they are reachable from any console such a person can open.

### 4.2 First administrators

- **Platform administrator.** The installer creates `admin@<KERNEL_DOMAIN>` in
  the kernel realm with no password and issues a single-use activation link
  (`./install.sh --activate-admin` for a new one).
- **Tenant administrator.** Provisioning creates the realm, its groups and the
  administrator account (`status.adminEmail` on the Tenant), a member of
  `gentian:tenant:<t>:admins`, with no password. The registrar issues the
  activation link: `kubectl gentian tenants activate-admin <tenant>`, or the
  *Tenants* screen.

No password is derived, stored or printed for either ([iam.md §1.4](iam.md)).

**Inviting a member.** *Members* → *Invite*: address, name, optional recovery
address, the app groups to join (an app installed for everyone is pre-selected).
The registrar creates the account with no password and has Keycloak mail a
link with `VERIFY_EMAIL` and `UPDATE_PASSWORD`.

### 4.3 Password reset and recovery

| Action | Mechanism |
|---|---|
| Reset by an administrator | The registrar has Keycloak mail a link with `UPDATE_PASSWORD`, to `gentian.inviteEmail` if set, else the primary address |
| Self-service "forgot password" | Not built |

### 4.3a Removing a member

*Members* → open the member → *Remove member*. The account is deleted.

Where the member has a mailbox on the cluster's own mail server, a dialog
asks what becomes of it, with nothing selected:

- **Archive the mailbox.** The mail is kept, out of the live mailboxes.
  Nobody receives or signs in at the address; a new person given the address
  starts empty.
- **Delete the mailbox.** The mail is destroyed. It cannot be undone, and is
  confirmed a second time.

The button stays off until one is chosen. Switching a member off (*May sign
in*) asks nothing and keeps the mailbox.

*Mailboxes of removed people*, on the same screen, lists what was decided
for each and what became of it: being archived or deleted, archived (with
who chose, when, and its size), deleted, or failed with the reason. An
archived mailbox is deleted from that list, after a confirmation. Who may do
all of this: whoever may manage the tenant's people (`can_manage_users`).
[mail.md §5c](mail.md) says what happens on the mail server.

### 4.4 Second factor

Keycloak's built-in TOTP. An administrator requires an authenticator for one
person (at the next sign-in, or by a mailed link) and removes a person's
authenticators for a lost device. A realm-wide rule ("everybody must have
one") cannot be set from the console: Keycloak expresses it as an
authentication flow, not a realm setting. WebAuthn and passkeys are not
configured.

### 4.5 Security policies

| Policy area | Settings |
|---|---|
| **Password** | Minimum length, digits, lower and upper case, special characters, history, maximum age |
| **Session** | Idle time, maximum length, remember-me |
| **Lockout** | On or off, failures allowed, lockout duration |

Session and lockout are declared state: the director commits them as
`security-policy.yaml` beside the tenant's manifest (`can_set_policy`), and the
tenant Composition writes them into the realm. Nothing in this path holds a
Keycloak credential, and a realm rebuilt from scratch comes back with them.

The password policy is set in one place: the registrar's action
`set-password-policy` on the realm (`can_set_policy`), with the caller's own
token. It is in force at once and is not in git, so a realm rebuilt from
scratch comes back without it. The screen reads the realm's policy, shows the
parts it has a control for, and names the clauses it has none for; saving
keeps those, and keeps a clause the form did not change as the realm has it.
A caller who may not read the realm's policy is shown the rest of the screen
and cannot change the password part. The director takes no password block.

### 4.6 Sessions

The console has no Sessions screen. Listing a person's sessions and ending
one from the console is not built; it is a possible future feature
([roadmap.md](../roadmap.md) §3.7).

What ends a session today when an administrator switches a member off or
removes them is the realm's refusal to renew it, within five minutes; nobody
ends it by hand. The whole of it is in [iam.md §1.12](iam.md).

### 4.7 Audit

The screen shows **changes to declared state**: every such change is a commit
the director authored as the person, with the relation and object that
permitted it. It needs no store of its own.

Not in it: sign-ins, refused requests and reads of data, for which no store
exists, and changes to people. For those the registrar keeps a record of who
asked and what permitted it, with a retention period, beside Keycloak's own
admin events; the console does not show either yet. The screen says what it
leaves out. There is no export.

### 4.8 Resources

Maps to the OS question *"how much of this machine may this account use, and how
much is it using?"* — with the part a desktop OS has no answer for: **what that
came to over a month**.

Full design in [resource-plans.md](resource-plans.md); what matters to the
console is the shape.

| Capability | Tenant admin | Platform admin |
|---|---|---|
| **Current ceiling** | Own tenant: enforced limits paired with committed use, and live consumption where a metrics source exists | Any tenant, plus an all-tenants headroom table |
| **Plan catalogue** | Plans they may select, each blocked one carrying its reason | The whole catalogue, including plans withheld from self-service |
| **Change plan** | Self-service, held to the tenant's entitlement ceiling | Any plan; may **force** a shrink below current use |
| **Usage history** | Own tenant, per resource, over 7d–12m | Any tenant |
| **Billed intervals** | The stretches the window resolves to, with the SKU in effect over each | Same |

Three properties make this different from an editable quota field:

**The API takes a plan name, never a quantity.** A ceiling that can be any number
can be any number that was never sold, so every ceiling reachable through the
console is one the platform has priced — which is what makes a month resolve to
SKUs rather than to numbers somebody downstream has to interpret.

**The write is a commit, not a patch.** Selecting a plan is the director
committing `resource-plan.yaml` to the deployments repository as the caller, after
checking `can_set_plan` and validating the choice against the operator's answer
for that tenant — so the console and the GitOps repository cannot disagree
about a tenant's ceiling, and the console decides nothing itself. The reads
are the operator's, relayed by the usher; `kubectl gentian resources` reads
them the same way and writes through the director (`set`).

**A downgrade below current use is refused.** Kubernetes does not evict pods to
fit a shrunken quota — it refuses the *next* create — so shrinking a tenant too
far fails silently, hours later, at the next restart. The console names the
resource and both numbers instead.

A plan change is audited as the commit it is, authored by the person and
trailered with the decision that allowed it, and as the plan event the operator
records once the change has landed; a refusal is the director's decision-log
entry. The attempt is the interesting half when a tenant later asks why nothing
changed.

---

## 5. Notifications

An administrator publishes a notice (a title and a text) to the people of the
tenant. Publishing is an action at the director (`can_administer`), which
records who published it. Notices are stored in the tenant's own database,
in the table the desktop reads; the console keeps no copy and reads the
history through the usher.

Not built: an audience narrower than the tenant (the director's action takes
no group) and delivery by mail or chat.

---

## 6. App access

Who may use an installed app is membership of the app's Keycloak group,
`gentian:tenant:<t>:app:<profile>`, and nothing else. The console sets it: per
person on *Members*, per group on *Groups*, and on *Apps* whether the app is
for everyone. How membership becomes a decision at the front door and a tile
on the desktop is [iam.md §1.6](iam.md).

**Accounts inside apps.** The platform does not create or remove a person's
account in an app; an app makes one at the person's first sign-in. The one
thing synchronised is who administers an app: members of
`gentian:tenant:<t>:app-admins` are given the administrator role an app's
profile declares (`spec.hooks.provisioning`, [iam.md §1.8](iam.md)).

---

## 6a. Models

The model gateway serves the models the Cluster claim declares and no others
([llms.md](llms.md) §5). The **Models** tab is where the cluster's
administrator reads and changes them: `GET` and `PUT /v1/clusters/{c}/models`
of the director, under `can_audit` and `can_configure` on the cluster. A
tenant's administrator has no such tab and is refused by the director.

A save is one commit of the claim. What is not on the screen when it is
committed is removed from the gateway. The gateway restarts with the new list
once Argo CD has synced.

The tab flags a model that cannot answer, without asking the gateway: a model
the cluster would serve itself is "not served", because the platform starts no
server for it; a provider's model is "token missing" or "no credential" when
the custodian's list says so. A token that is there is shown as supplied, not
as working. A provider's token is entered on the Credentials tab, under
`llm-provider-<name>`; a provider the platform ships no credential requirement
for has nowhere to enter one, and the tab says so.

---

## 7. Platform roles

Which Keycloak group holds which platform role is stated on the Cluster claim
(`spec.platformRoles`; `admin` defaults to `gentian:platform:admin`, the other
roles have no holder unless a group is named). The operator projects it into
OpenFGA. A platform administrator administers a tenant the cluster operates
through that tenant's console.

A mode that withholds routine access to tenants' people from platform
administrators is not built ([roadmap.md](../roadmap.md) §3.3).

---

## 8. Not built

Designed at some point and absent from the code. None of it should be read as
a control that exists.

| Topic | State |
|---|---|
| Session list and revocation in the console | Not built; a possible future feature (§4.6, [roadmap.md](../roadmap.md) §3.7) |
| Sign-in and access audit, export, retention | Not built (§4.7; [roadmap.md](../roadmap.md) §1.12) |
| Realm-wide second-factor rule, WebAuthn | Not built (§4.4) |
| Creating and removing accounts inside apps (SCIM or events) | Not built (§6) |
| Group-scoped and mailed notices | Not built (§5) |
| A model's health as the gateway sees it; a credential for a provider the platform ships none for | Not built (§6a; [llms.md](llms.md) §5) |
| Making a backup key in the console | Not offered: the console must not hold a key. A person makes one with `age-keygen` and gives the console the public half ([tenant-backup-guide.md](../tenant-backup-guide.md)) |
| Agents and delegation, access requests, break-glass workflow | Not built |
| A tenant's own upstream identity provider, service-account registry, dynamic groups, guests with an end date, access certification | Not built |

---

## 9. Non-goals

These stay outside the console:

- Installing apps (the App Store app, or the command line)
- Scheduled backups, the backup policy and its destinations (the Operations Console)
- Editing NetworkPolicy, Kyverno policy or other MAC rules
- Authoring a `ComponentProfile` or a catalogue

---

## 10. References

| Topic | Location |
|---|---|
| Roles and sign-in | [iam.md](iam.md) |
| Hosts, the front door, sessions | [routing.md](routing.md) |
| MAC backbone | [security.md](security.md) |
| OpenFGA model | `authz/model/v1/model.fga` |
| Tenant identity objects | [tenant-identity-composition.md](tenant-identity-composition.md) |
| The App Store app | [store-contract.md](store-contract.md) |
| Resource plans and usage | [resource-plans.md](resource-plans.md) |
