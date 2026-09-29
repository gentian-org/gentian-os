# Recovery playbook addition: writing configuration when the director is down

To be merged into [../../recovery-playbook.md](../../recovery-playbook.md)
once the director/operator split (AD-2) is live. Until then the lifecycle
API and direct git pushes exist and this section does not apply.

## When this applies

The director is the only identity that may push `gentian-deployments`
(branch protection) and the only signer Argo CD accepts
(`AppProject.spec.sourceIntegrity`). If the director cannot serve — its
pods are down, its issuer is unreachable, its git credential is revoked —
no configuration change can reach the cluster through the normal path.
Most incidents do not need one: a cluster keeps running on the last
synced state. This procedure is for the case where a configuration change
is itself the fix.

## What the recovery kit holds for it

Sealed with the kit's `age` key, alongside the OpenBao recovery material:

- **`break-glass-push`** — a git credential for `gentian-deployments` with
  push rights on `main`, held by no running process. Branch protection
  lists exactly two identities that may push: the director and this one.
- **`break-glass-signing.key`** — a GnuPG key whose id is listed in the
  `sourceIntegrity` policy next to the director's. Argo CD accepts a commit
  signed with either; it refuses everything else.

Both are rotated after every use (step 6).

## Procedure

1. **Declare.** Add yourself to `gentian:platform:break-glass` in the
   kernel realm. The group has no standing members, so the membership
   change is the incident's first audit line, in the Keycloak event log,
   before anything is written.
2. **Open the kit** and extract the two credentials to a workstation that
   will be wiped afterwards. Never onto a cluster node.
3. **Make the change in a clone, signed.** Commit as yourself with the
   break-glass key, and put the incident in the trailer:

       Gentian-Break-Glass: incident=<id> reason=<one line>

   Push to `main`. The director's absence is why this is allowed; the
   trailer is what lets the auditor find it later.
4. **Argo CD syncs** the commit like any other: the signature verifies
   against the break-glass key id. If Argo refuses — the key is not in the
   policy, or the policy was itself the casualty — edit
   `AppProject/gentian` directly with `kubectl` (a kernel object; this is
   what break-glass `kubectl` on `kernel-*` is for) and restore the policy
   as part of the same incident.
5. **Bring the director back.** On start it reconciles the OpenFGA store
   from git (the store is a projection, rule R9 of the authorization
   model), so a change to roles, grants or entitlements made in step 3 is
   reflected without a second manual step.
6. **Rotate and close.** Revoke `break-glass-push` at the git host and issue
   a new one; retire the signing key id from the `sourceIntegrity` policy
   and add the replacement; re-seal both into the kit; remove yourself from
   the break-glass group. The incident is closed when the auditor can
   see, for one incident id: the group change (issuer log), the commit
   (change log), and the rotation.

## What this does not cover

- The **git host** itself being down: nothing can be synced; wait, or
  restore the host from its own backup. Argo CD keeps applying the last
  fetched state.
- **OpenBao** sealed or lost: that is the existing playbook. The director
  needs OpenBao only to hand a pull credential to the credential manager;
  configuration writes do not.
- A **compromised director**: rotate its push credential and signing key
  first (step 6, before step 3), then proceed. The commits it made are in
  the change log with its key id; the auditor decides which to revert.

## Test

Quarterly, on the test cluster: take the director to zero replicas, run
steps 1–6 for a harmless change (a tenant `displayName`), and confirm the
three audit lines join on the incident id. The test is the proof that the
kit's credentials are current.
