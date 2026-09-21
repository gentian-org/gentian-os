# ComponentProfile: one catalogue kind for system services, apps and agents

Companion to [namespace-cleanup.md](namespace-cleanup.md) (which namespace each
tier lands in) and [operator-split-plan.md](operator-split-plan.md) (who writes
the CR); the decisions behind all three are in
[architectural-decisions.md](architectural-decisions.md). This document covers only the schema: what a catalogue entry declares,
and what the platform derives from it.

Kernel components are out of scope. They are installed by `install.sh` and then
reconciled by Argo CD, never from a profile and never by the operator, because
the operator depends on them existing first.

## 1. Two levels, one word

A single `tenancy` value on the profile would lose a
distinction worth keeping: whether an app *can* serve several tenants safely is
a claim the catalogue certifies, while *running* it shared is a decision the
platform admin makes. Collapsing them means a profile that could go either way
cannot say so, and the admin's decision is not recorded apart from the app's
claim about itself.

So: same word, two levels.

- `ComponentProfile.spec.tenancy` is a **list** of the modes this component may
  be deployed under. A certification claim, reviewed with the entry.
- `Component.spec.tenancy` is a **single value**, and must be a member of that
  list. The deployment decision.

| Mode | Responsible | Namespace | Serves |
| --- | --- | --- | --- |
| `system` | platform admin, via the Cluster claim | `system-<function>` | other components, over contracts |
| `shared` | platform admin, via the director | `shared-<app>` | humans of several tenants |
| `tenant` | tenant admin, via the director | `tenant-<t>` | humans of one tenant |

`system` is exclusive. A component that supplies contracts to other components
does not also serve humans; if it appears to, it is two components.

`trustTier` stays in the spec. AD-4 validates shared tenancy against it, and CRD
validation rules cannot read labels or annotations — only `name` and
`generateName` are exposed on metadata. Outside the spec, AD-4 could only be an
admission policy. Keeping it in the spec makes the rule a schema invariant that
fails at write time regardless of who is writing.

## 2. Spec

```go
type ComponentProfileSpec struct {
    // Tenancy lists the modes this component may be deployed under. A
    // certification claim, not a choice. "system" is exclusive.
    // +kubebuilder:validation:MinItems=1
    Tenancy []TenancyMode `json:"tenancy"`

    // TrustTier is the review level of this entry. AD-4 requires "platform"
    // before "shared" may appear in Tenancy.
    TrustTier TrustTier `json:"trustTier"`

    // Version is the catalogue entry version, not the upstream project's.
    Version string `json:"version"`

    // Package: chart reference, deployment method, and the mapping from
    // granted requirements onto chart values.
    Package PackageSpec `json:"package"`

    // Requires is what the platform is obliged to provide before this
    // component may run. Unmet means it does not start and the platform is at
    // fault. Resolved once, as a precondition.
    // +optional
    Requires *RequirementSpec `json:"requires,omitempty"`

    // Integrations are opportunistic relationships with peer components.
    // Absent peers are normal. Bound and unbound continuously, each binding
    // subject to the tenant's grant.
    // +optional
    Integrations []IntegrationRef `json:"integrations,omitempty"`

    // Provides lists contracts this component supplies. Tenancy decides the
    // audience: cluster-wide for system, within the tenant otherwise.
    // +optional
    Provides []ContractRef `json:"provides,omitempty"`

    // Secrets are values the platform generates and holds that have no
    // external counterparty. Credentials arriving with a granted requirement
    // are NOT declared here; they come with the grant.
    // +optional
    Secrets []SecretSpec `json:"secrets,omitempty"`

    // Expose declares entry points. Absent for system tenancy (AD-9), enforced
    // rather than assumed. See §5 — every entry carries a mandatory authMode.
    // +optional
    Expose []ExposureSpec `json:"expose,omitempty"`

    // SessionMaxAge caps the app's OWN session, for apps that establish one
    // instead of consuming the forwarded token. The edge bounds reachability;
    // it does not refresh the group model an app captured at its own login, so
    // this value — not the access-token lifetime — is the bound on what a user
    // may still do inside the app after their rights change (AD-13).
    // Required when the app runs its own login; meaningless otherwise.
    // +optional
    SessionMaxAge *metav1.Duration `json:"sessionMaxAge,omitempty"`

    // Extensions are containers shipped inside the component's own pod. The
    // only escape hatch, and deliberately one that cannot create a
    // cluster-scoped object.
    // +optional
    Extensions []ExtensionSpec `json:"extensions,omitempty"`

    // Hooks are lifecycle actions at install, upgrade and resync.
    // +optional
    Hooks *HookSpec `json:"hooks,omitempty"`
}
```

No presentation fields. They are reference data outside the cluster (AD-3), and
an optional field would invite partial population and two sources of truth for
one string.

## 3. Requires and integrations are different in five ways

| | `requires` | `integrations` |
| --- | --- | --- |
| Counterparty | the platform | another component |
| If unmet | does not start | runs normally |
| Whose fault | the platform's | nobody's |
| When resolved | once, before install | continuously |
| Consent | implicit in using the platform | explicit tenant grant |
| Signal | alert the platform admin | a status note, never an alert |

A boolean on a shared list hides all six. They are separate fields because they
are separate controller paths: requirements gate admission of the component,
integrations reconcile forever after.

An unmet requirement should surface as a condition naming the platform as
responsible, not as a component error a tenant admin cannot act on.

Keycloak's SMTP is the worked example already in the tree: the relay is
supplied after install, and a realm without it simply cannot send invitations.
That is an integration. Keycloak's database is a requirement.

`Requires` absorbs today's `kernelRequirements`, `optionalIntegrations` and
`security`:

```go
type RequirementSpec struct {
    // Contracts are platform capabilities: identity, database, object
    // storage, cache, mail, LLM, MCP.
    Contracts []ContractRequirement `json:"contracts,omitempty"`

    // Privileges escape the default posture: pod-security waivers, egress
    // beyond the baseline, elevated roles. Every one needs approval.
    Privileges *PrivilegeRequest `json:"privileges,omitempty"`
}
```

This closes the live asymmetry recorded as
[security-gap-closing.md](security-gap-closing.md) G27: MAC waivers are
intersected against the cluster `PlatformSecurityPolicy` allowlist, so an
administrator approves them, while `security.egress` is copied straight into a
NetworkPolicy with no equivalent check. A profile can currently grant itself
outbound network access but not a pod-security exception. One requirement block
with one approval path removes that by construction, and satisfies principle 8:
every permissive setting is a field admission can refuse and audit can grep.

## 4. Three kinds of secret, two declared

| Kind | Origin | Declared |
| --- | --- | --- |
| Granted credential | arrives with a requirement grant | No — restating it creates drift |
| Generated secret | random, created once, held in the vault | Yes, in `secrets` |
| Derived secret | deterministic from tenant and component name | Yes, but prefer generated |

Generated secrets have no counterparty: an admin bootstrap password, a session
signing key, a data-at-rest key. Nothing external can produce them.

Derivation deserves scrutiny rather than preservation. Its stability comes from
recomputation rather than storage, so the formula and its inputs are
load-bearing forever and a change to either silently rotates the value for
every existing tenant. That failure mode has already occurred on the derivation
salt. Generated and stored is the more robust default.

## 5. Exposure: `authMode` mandatory, perimeter explicit

Two things the schema has to get right, both carried by
[security-gap-closing.md](security-gap-closing.md) G3.

**`authMode` is required on every entry, with no default.** G3 and principle 6
both require that `none` be a word someone wrote and a reviewer can find. A
default would defeat exactly that — as today's `BrowserProxyRoute.authMode`
does, defaulting to `forward-bearer`.

**A gateway route and a perimeter surface are different objects.** The taxonomy
puts publishing proxies in `tenant-<t>-dmz` (AD-6) with one least-privilege
credential per surface, separate from routes on the authenticated gateway.
They differ in namespace, credential, policy and blast radius, so the schema
has to tell them apart:

```go
type ExposureSpec struct {
    Name     string       `json:"name"`
    // Surface selects where this entry is published.
    //   gateway   — the authenticated tenant gateway
    //   perimeter — a publishing proxy in tenant-<t>-dmz, its own credential
    // +kubebuilder:validation:Enum=gateway;perimeter
    Surface  SurfaceKind  `json:"surface"`
    // AuthMode is mandatory. "none" is explicit and greppable.
    // +kubebuilder:validation:Enum=oidc;jwt;bearer;basic;signature;none
    AuthMode AuthMode     `json:"authMode"`
    // Paths this entry serves. Empty means the whole host.
    Paths    []string     `json:"paths,omitempty"`
    // DenyPaths are refused even where Paths admits them. Deny wins over
    // allow regardless of specificity, so a broad allow with narrow denials
    // is readable rather than a precedence puzzle. Needed for apps whose
    // public surface is "the site except its admin", e.g. an Odoo website.
    // +optional
    DenyPaths []string    `json:"denyPaths,omitempty"`
    // Source restricts who may call this entry, before authMode is even
    // considered. The case it exists for is a callback that must come from
    // one known peer — Collabora's WOPI callbacks are `authMode: none` and
    // safe only because the caller is pinned. Without a field the restriction
    // lives in prose and nothing enforces it.
    // +optional
    Source   *SourceRestriction `json:"source,omitempty"`
    Backend  BackendRef   `json:"backend"`
}

// SourceRestriction pins the caller. Exactly one form; both are evaluated at
// the proxy, not the app.
type SourceRestriction struct {
    // CIDRs admitted, after the real client IP is resolved at the edge.
    // +optional
    CIDRs []string `json:"cidrs,omitempty"`
    // Component names another installed component whose pods may call this
    // entry; the operator resolves it to the peer's identity and, at L5, to a
    // NetworkPolicy. Preferred over CIDRs, which age badly.
    // +optional
    Component string `json:"component,omitempty"`
}
```

`surface: perimeter` is what the operator reads to build the DMZ namespace, and
it replaces a separate `publicSurfaces` list. One concept, one place.

### 5.1 Enablement: the tenant's half

The profile declares what *may* be published. What *is* published is the
tenant's decision, recorded on the instance, never on the profile
([networking.md](networking.md) §8.1). Gateway entries need no enablement:
they carry the session and are always on. Perimeter entries are off until a
perimeter approver enables them.

```go
// On the Component (the instance). Written only by the director.
type ExposureEnablement struct {
    // ExposureName names an ExposureSpec entry of the profile whose surface
    // is "perimeter". Not called `surface`: that is the enum on the profile's
    // entry, and one field name meaning two things in adjacent structs is how
    // a schema starts drifting. authMode is not repeated here either — the
    // profile's entry is the one source and the enablement cannot weaken it.
    ExposureName string    `json:"exposureName"`
    // Host is the public hostname. Empty means the entry's default host in
    // the tenant's zone, which the zone's DNS-01 wildcard already covers. A
    // vanity host is admitted only if the tenant's approved domains include
    // it, and its certificate is obtained by HTTP-01 — the platform holds no
    // credential to a customer's DNS zone and does not want one
    // (networking.md §7).
    // +optional
    Host      string       `json:"host,omitempty"`
    // Owner is the Keycloak subject that enabled the surface. Set by the
    // director from the caller's token; immutable.
    Owner     string       `json:"owner"`
    // ExpiresAt bounds the exposure. Always set: the cluster policy's
    // defaultLifetime is required, so the director fills this in when the
    // approver does not, and it is never later than maxLifetime. At expiry the
    // operator treats the enablement as absent when building desired state and
    // removes the proxy, route and listener — which also makes Argo CD's next
    // sync idempotent, since the enablement stays in git as history.
    ExpiresAt *metav1.Time `json:"expiresAt"`
    // ReviewAt is when the owner and the perimeter approver are asked to
    // renew or revoke. Renewal is a new commit.
    // +optional
    ReviewAt  *metav1.Time `json:"reviewAt,omitempty"`
}
```

Admission: `exposureName` must name a `perimeter` entry of the referenced
profile; `host` must be in the tenant's zone or in the tenant's approved
domains; `expiresAt` must respect the cluster policy. Who may write one is
`can_expose` on the tenant, checked by the director.

The cluster half lives on the Cluster claim and is written by the security
officer. It is a **ceiling, not a permission list**: no component is ever
published by default, at any trust tier, so this block has nothing to grant.
It bounds what an approver may choose.

An earlier draft gated `authMode: none` on `trustTier: platform`. That
conflated two unrelated risks — platform tier certifies that one instance can
safely serve several tenants, which says nothing about whether an anonymous
request may reach it — and it would have refused the first Nextcloud share
link, since those are ordinary tenant-tier apps.

```yaml
exposure:
  # Modes this cluster refuses outright, whatever a profile declares or an
  # approver chooses. Usually empty: the control is the enablement, not the mode.
  denyAuthModes: []
  # A `none` surface must provide the exposure-policy contract (§5.2), so the
  # platform can read and bound the app's public objects.
  requireExposurePolicyContract: true
  # Required, with no "absent" case: two optional fields whose joint default is
  # a permanent public surface is not a safe default (principle 8).
  defaultLifetime: 2160h         # 90 days
  maxLifetime: 8760h
  reviewInterval: 720h
```

### 5.2 The `exposure-policy` contract

A profile with any `authMode: none` entry should declare
`provides: [{name: exposure-policy}]`, and must wherever the cluster sets
`requireExposurePolicyContract` (the default). That is an admission check, not
a CRD validation rule: the requirement lives on the Cluster claim and CRD
rules cannot read another object. It is refused when the enablement is
written, which is also the only moment it matters.
The contract gives the platform, with the tenant's credential from the
binding, `policy.read`/`policy.write` — the app's public-sharing policy:
default and maximum object expiry, password required, groups allowed to
share publicly, anonymous upload — and `objects.list`/`objects.revoke` —
the app's public objects with owner, created, expiry and a hashed token.
The platform writes the tenant's policy into the app whenever the cluster
policy or the enablement changes. A `none` entry without the contract is
admitted but its surface is **opaque** in the inventory
([networking.md](networking.md) §8.3), bounded only by the enablement's
expiry.

## 6. What tenancy derives

| | system | shared | tenant |
| --- | --- | --- | --- |
| Namespace | `system-<function>` | `shared-<app>` | `tenant-<t>` (+ `-dmz`) |
| Public route | none (AD-9) | per granted tenant | tenant gateway |
| OIDC client | none | per granted tenant realm | tenant realm |
| Scope of `provides` | cluster-wide | cluster-wide | within the tenant |
| Tenant binding | none | grant per tenant | implicit |
| Delete blast radius | every consuming component | every granted tenant | one tenant |

Nothing here is a new field. Tenancy reinterprets the scope of declarations the
profile already makes. One schema, three readings.

## 7. Where enforcement goes

CRD validation rules for what the object may say:

```yaml
x-kubernetes-validations:
  - rule: "!('system' in self.tenancy) || self.tenancy.size() == 1"
    message: "system is exclusive: a component serving contracts does not also serve humans"
  - rule: "!('system' in self.tenancy) || !has(self.expose)"
    message: "system components have no exposure (AD-9)"
  - rule: "!('shared' in self.tenancy) || self.trustTier == 'platform'"
    message: "shared tenancy requires trustTier platform (AD-4)"
```

Admission policy for who may say it, because these depend on the writer or the
namespace, which CRD validation cannot see:

- a Component in a tenant namespace must have `tenancy: tenant`
- `tenancy` must be a member of the referenced profile's list
- only the platform admin may create Components in `system-*` or `shared-*`

CEL for what the object may say, admission policy for who may say it. Keeping
that boundary deliberate is worth more than putting every rule in one place.

## 8. Why the model closes

A tenant admin cannot create a system service, because the only tenancy
creatable in a tenant namespace is `tenant`. They never need to: what a
component requires arrives through contracts.

When something needs what no contract covers, it ships as an extension inside
the component's own pod. That stays in the tenant namespace, in tenant
ownership, and in the tenant's blast radius. The escape hatch never creates a
cluster-scoped object, which is why there is no hole.

## 9. Open decisions

1. **Fulfiller selection.** With system services as real instances, a
   `database` requirement must resolve to a specific one. Tenancy says a
   Postgres exists, not which. Needs a default per contract on the Cluster
   claim, or an explicit selector.
2. **The authorization vocabulary.** Principle 4 requires one OpenFGA type per
   CRD kind, shipped with its test cases. The model names things
   `installed_app`, `app_contract`, `capability`; the split plan adds
   `catalogue_entry`. Renaming AppProfile to ComponentProfile moves that
   vocabulary with it. `catalogue_entry` is arguably the better name for the
   entry and ComponentProfile for the kind, but only if chosen.
3. **Sequencing.** The split plan edits `kernelRequirements` and
   `security.egress` in `BuildDesired`, and AppProfile webhooks are in its
   critical path. §3 here merges those fields. Both touch the same code and
   only the split has a written cutover. Land the split first.
4. **May system components consume each other's contracts?** Today none do, by
   construction, which is why system services can be provisioned in one pass
   after the kernel converges. If that stops being true, provisioning needs a
   topological sort it does not need now. Worth stating as a rule rather than
   leaving it as an accident.
