# Tenant Identity — Crossplane Manifest Bridge

Companion to [architecture.md](../architecture.md) §3.1, [iam.md](iam.md), and [admin-console.md](admin-console.md).

## Overview

A tenant's Keycloak realm is provisioned in two ways, both owned by the
`tenant-default` Composition (`crossplane/compositions/tenant-default.yaml`):

- **Managed resources of `provider-keycloak`**, declared in the Composition.
- **Batch Jobs** that call the Keycloak Admin API, for what is not declared
  yet. They reach the Composition through the **manifest bridge**:

1. The operator seeds credentials in OpenBao and builds the Job manifests in Go.
2. It writes them to the ConfigMap `tenant-{name}-provisioning-jobs`
   (`jobs.json`) in `kernel-provisioning`.
3. `tenant-default` emits one `kubernetes.crossplane.io/Object` per Job. The
   Jobs run in `kernel-authentication`, beside Keycloak.
4. The operator waits for each Job (`waitForProvisioningJob`) and reports on
   `Tenant.status.conditions` (`IdentityReady`).

A Job's pod template is immutable, so the operator stamps each Job with a hash
of its script and removes a finished Job whose script changed; the Composition
then creates it again.

The platform tenant adopts the kernel realm: it gets no realm Job, no
administrator Job and no mail-server Job, only its groups -- and the
clients the platform's administrators enter other tenants with
([iam.md §1.1c](iam.md)), which are declared, not Jobs.

## What is declared and what is a Job

| Declared in `tenant-default` | Still a Job |
|---|---|
| The zone's sign-in client (`gentian-edge-<zone>`), its scopes and audience mappers | `keycloak-realm-{tenant}`: creates the realm |
| The token-exchange client, the `groups` scope and mapper, the user profile, realm events | `keycloak-gentian-groups-{tenant}`: the tenant's groups, one per installed app included, and their attributes |
| The four fixed groups (`members`, `admins`, `app-admins`, `perimeter`) | `keycloak-admin-{tenant}`: the tenant administrator's account, without a password |
| The realm's required actions | OIDC client Jobs, for an app whose client has an OIDC pack ([iam.md §1.7](iam.md)); SAML client Jobs |
| `gentian-dovecot` and the `mailbox` scope, where the cluster has its own mail server | |
| The realm itself, adopted by name | The realm's mail-server settings, where the cluster has SMTP credentials |
| For a tenant people sign up to (`spec.signUp.enabled`): the realm's registration settings, its registration flow with the platform's proof-of-work check, and the binding of the realm to that flow | |
| For a tenant that lets the platform's administrators enter (`spec.kernelBroker`, from `Tenant.spec.platformAccess.adminsEnter`): the identity provider `kernel` and its mapper into the admins group | |
| For the platform tenant (`spec.kernelEnter`): per entering tenant the client `broker-to-<t>` in the kernel realm | |

An app without a pack gets its OIDC client rendered with its release by the
operator (`internal/apprender`), not from a Job; the tenant reconciler only
seeds the client's secret.

The kernel identity provider in a tenant realm is composed only where the
tenant switched it on, and it maps one thing: the platform's administrators
into the tenant's admins group ([iam.md §1.1c](iam.md)).

## Composition ordering

`tenant-default` runs `function-sequencer`: objects that need the realm are
held back until what they depend on is Ready.

Job order, as the operator waits for it: realm → groups → tenant
administrator → OIDC and SAML client Jobs → mail server.

## Deleting a tenant

The operator runs one Job (`identity_reconciler.go`): it deletes the realm
where the tenant's `spec.deletionPolicy` is `Delete`, and otherwise disables
it. The kernel realm is never touched. The composed Keycloak objects have
`deletionPolicy: Orphan`: they are not deleted one by one, they go with the realm.

## Scripts

The Jobs' shell scripts are built in Go (`internal/controller/identity_reconciler.go`,
`keycloak_*.go`, `oidc_pack_script.go`), with shared helpers in
`internal/keycloak/shell_helpers.go`.

## Testing

- Unit: `go test ./internal/controller/...`; rendered Compositions under `crossplane/tests/unit/render/`
- Manual: `kubectl get jobs -n kernel-authentication -l gentianos.io/tenant=<name>`
