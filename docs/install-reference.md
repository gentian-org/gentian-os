# Install reference

Everything about installing that is not the first-install walkthrough. For that,
see [GETTING-STARTED.md](../GETTING-STARTED.md).

---

## 1. How the installer is put together

`install.sh` installs nothing itself. It is a driver over a directory of steps:

```text
scripts/steps/A-01-crossplane.sh
scripts/steps/A-08-eso.sh
scripts/steps/B-08-cluster-xr.sh
…
```

Each step is a self-contained file, readable top to bottom. It declares what it
requires and provides in a header and implements up to three verbs:

| Verb | What it does |
|---|---|
| `check()` | Read-only. Is this already done? |
| `apply()` | Do it |
| `destroy()` | Undo it |

`check()` is why re-running works: each step asks the *cluster* whether its work
is present, so there is no state file on your machine to fall out of sync.

One program therefore covers three directions:

```bash
./install.sh              # install, or continue where a previous run stopped
./install.sh --update     # the same thing; converging IS updating
./install.sh --uninstall  # the same steps in reverse, calling destroy()
```

### Step numbering and phases

Steps are numbered `<phase letter>-<NN>`. The number orders steps within a
phase, so a new step only ever affects its own phase.

| | Phase | What it does |
|---|---|---|
| **A** | `control-plane` | Crossplane, namespaces, cert-manager, ESO, ArgoCD |
| **B** | `secrets` | OpenBao, the Cluster claim, credential seeding |
| **C** | `platform` | Wildcard cert, root ApplicationSet, admission, catalogue |
| **D** | `applications` | Operator, mail, LLM serving, portal, app profiles |
| **E** | `handover` | Tenants, per-tenant reconcile, revoke the bootstrap token |

### Reading a `--status` verdict

| Verdict | Meaning |
|---|---|
| `satisfied` | The step's `provides:` is present on the cluster |
| `missing` | It is not; `apply()` has work to do |
| `undefined` | The step has nothing persistent to check, or does not apply to this cluster — a feature that is switched off, or no install-time artefact |

`undefined` is never a failure. `A-04-prewarm`, `C-03-provider-helm` and
`D-02-gateway-wait` only wait or warm a cache, so they always read that way.

---

## 2. Commands that change nothing

```bash
./install.sh --explain    # every step, what it provides and what it mutates
./install.sh --status     # run every check() against the cluster
./install.sh --dry-run    # run the checks, print the plan, apply nothing
```

None of the three collects a credential. `--dry-run` runs the same preflight as
an install except for that: it applies nothing, and no step's `check()` reads a
credential, so it has everything it needs to print the plan. The install
collects them; the preview does not.

`--explain` needs no cluster connection. It reads the step headers, so it cannot
drift from what will actually run.

---

## 3. Running part of the install

```bash
./install.sh --from B-03            # from there to the end
./install.sh --only A-07            # one step
./install.sh --only A-07,A-08       # a named subset
./install.sh --skip A-04            # everything but that
./install.sh --phase secrets        # one phase
```

A failure names the step and its file. Fix the cause and re-run — completed
steps are skipped.

```bash
less scripts/steps/B-08-cluster-xr.sh
```

---

## 4. Configuration surfaces

Everything you supply belongs to one of three places.

| Surface | Carrier | Answers |
|---|---|---|
| Repository pointer | `install.env` on the installing machine | Where does this cluster's configuration come from? |
| Declarative configuration | YAML in `gentian-deployments` and `gentian-apps` | What is this cluster, its tenants, and their apps? |
| Credentials | Prompted, written to OpenBao | What secrets does it need that cannot be derived? |

`install.env` is the only non-secret file the installer reads from local disk.
[docs/deployment.md](deployment.md) covers the layering inside
`gentian-deployments`.

### Which credentials the installer handles

`credentials.yaml` gives every requirement a `phase`, and the phase decides who
asks for it:

| `phase` | Asked by | Why |
|---|---|---|
| `bootstrap` | The installer, at the prompt | The cluster does not exist yet, so nothing on it can |
| `runtime` | The credential manager, once the cluster runs | It can validate, record who set it, and gate the claims that need it |

The bootstrap set is deliberately small and every member is validatable with
`curl` or `openssl` alone. A credential needing an SDK or a signing algorithm is
`runtime` by that fact, and the shell never sees it — which is what stops
credential logic accumulating in the installer.

The manager holds no OpenBao token of its own: it exchanges the caller's
Keycloak token for a short-lived one, so the write carries a human identity into
the audit device. It has no endpoint that returns a value.

### What the master password does

It depends on the cluster's `secretMode`, a field on the `Cluster` claim:

| `secretMode` | Kernel credentials are | Reproducible on a rebuild? |
|---|---|---|
| `derived` (default) | `HMAC-SHA256` of the master password **and** a per-cluster salt | Yes — with **both** |
| `random` | Generated once by `openssl rand`, then stored | **No.** The master password only guards the paths |

The salt is generated at first install and stored in OpenBao beside the password.

> Under `derived`, reproducing a cluster's credentials needs the master password
> **and** the salt. The salt lives only in OpenBao, so a disaster that loses
> OpenBao's storage also loses it, and the master password alone reproduces
> nothing. `./install.sh --export-recovery-kit` captures both, plus the unseal
> material and the cluster's identity, in one encrypted file — see step 9 of
> [GETTING-STARTED.md](../GETTING-STARTED.md).

Under `random` there is nothing to reproduce; recovery means restoring OpenBao.

### Where the backup key lives

Scheduled backups are encrypted to the cluster's age key. Encryption needs only
the public half, so that is all the operator ever holds — the private half is
what opens a bundle, and where it lives is a choice.

`./install.sh --export-recovery-kit` generates the pair on a cluster that has
none: the public half goes to OpenBao at `gentian-os/kernel/backup/recipients`,
and the private half into the kit. It never regenerates, because a second pair
would orphan every bundle written to the first.

`spec.backup.escrowIdentity` in the cluster claim decides whether the private
half is *also* stored in OpenBao, at `gentian-os/kernel/backup/identity`:

| | Restoring needs | The risk you are taking |
|---|---|---|
| `true` (default) | OpenBao credentials, or the kit | whoever reaches OpenBao as a cluster administrator gets the bundles *and* the key that opens them |
| `false` | the recovery kit | losing every copy of the kit loses every backup, with no recourse |

On by default, because the likelier disaster is a lost recovery kit rather than
a stolen cluster, and a backup nobody can open is not a backup.

Off is the stronger position against tampering and theft: nothing the cluster
holds can open a bundle, so an attacker who takes the cluster gets ciphertext
and a public key — which is what an attacker who already has the bucket has.
Choose it when the cluster is the likelier loss and you are certain of your kit
custody, because it makes the first kit irreplaceable.

Escrow is read by the `cluster-admin` policy and explicitly denied to `eso-read`,
so the key cannot be turned into a Kubernetes Secret by anything that can write
an `ExternalSecret`. It means "a cluster administrator can read it", not "the
cluster can read it"; `make test-policy-openbao` asserts both halves against a
real OpenBao.

It also has a second effect worth knowing: with escrow on, a later
`--export-recovery-kit` reads the identity back and the new kit carries it. With
escrow off, the first kit is irreplaceable, and every kit written afterwards is
missing the one value that cannot be regenerated.

Only the literal `false` turns escrow off. The XRD defaults the field, so a
current cluster always states it; an empty answer means the resource could not
be read at all, and the kit export says which way it defaulted rather than
leaving it to be inferred.

To restore from an escrowed key:

```bash
bao kv get -mount=secret -field=identity gentian-os/kernel/backup/identity > identity.txt
age -d -i identity.txt manifest.json.age > manifest.json
```

Turning escrow on for a cluster whose key predates it stores nothing by itself —
the key is only ever written at generation. Supply it once from the kit:

```bash
bao kv put -mount=secret gentian-os/kernel/backup/identity identity=@identity.txt
```

---

## 5. Installing on an internal domain

A domain that is not publicly resolvable cannot be reached by Let's Encrypt, and
no certificate is ever issued. The symptom is unhelpful: every Gateway sits at
`ResolvedRefs=False` complaining about a missing Secret, which says nothing
about DNS.

Use the cluster's own certificate authority instead. In
`clusters/<cluster>/kernel/claims/cluster.yaml`:

```yaml
spec:
  kernelDomain: platform.internal
  certificates:
    issuerMode: self-signed
```

Issuance is then offline and instant. The certificates are not publicly trusted,
so anything validating a kernel hostname from outside the cluster needs the root
CA in its trust store:

```bash
kubectl get secret gentian-root-ca-tls -n cert-manager \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > gentian-root-ca.crt
```

The other modes are `acme-dns01` (the default; public DNS, wildcards),
`acme-http01` (public DNS, no wildcards) and `private-ca` (you supply the CA).
Selecting a mode the cluster cannot satisfy fails with a message naming the
mode; it never falls back to a public issuer.

---

## 6. Installing from a mirror

For an air-gapped or forked install, point the platform at your own copies in
`install.env`:

```bash
GENTIAN_OS_REPO=https://git.internal/gentian-os
GENTIAN_OS_IMAGE_REPOSITORY=registry.internal/gentian-os
GENTIAN_DEPLOYMENTS_REPO=https://git.internal/gentian-deployments
```

Both the Git origin and the image registry are redirected, including for every
child ApplicationSet the platform creates.

`versions.yaml` is the inventory of everything else the install pulls —
Crossplane, cert-manager, External Secrets Operator, ArgoCD, Envoy Gateway and
the OpenBao CLI, each with its pinned version and source. That file is what to
mirror.

---

## 7. Day-2 credentials

On a running cluster, credentials are supplied through the on-cluster credential
manager rather than the installer. It validates a value against the system it is
for before storing it, and never displays a stored value — metadata only:
whether it exists, who set it, and when.

Lost credentials are rotated, not recovered.

The installer's bootstrap token is revoked by the last step once an OIDC write
path is configured. Until then that token is the only way to write a credential,
so the step refuses to revoke it and says so.

---

## 8. Uninstalling

Uninstall is the same steps in reverse:

```bash
./install.sh --uninstall --dry-run    # see the order first
./install.sh --uninstall
./install.sh --uninstall --skip E-01  # keep tenant workloads and their Git manifests
```

OpenBao KV data survives an uninstall, so reinstalling onto the same cluster
recovers the credentials rather than re-prompting.

---

## 9. When something is wrong

**Start here.** It names the step whose expectation is unmet:

```bash
./install.sh --status
```

**A resource is not becoming Ready.** Most of the cluster is reconciled by
Crossplane and ArgoCD after the installer finishes:

```bash
kubectl get managed
kubectl get application,applicationset -n argocd
kubectl describe cluster.gentianos.io -n crossplane-system
```

**Something depends on a credential that is not there.** A claim waiting on a
credential says which one:

```bash
kubectl get repository.gentianos.io -A -o custom-columns=\
NAME:.metadata.name,SATISFIED:.status.credentialSatisfied,WHY:.status.credentialMessage
```

**A credential looks present but its probe says otherwise.** Check the field
names, not just the path — a value stored under the right path with the wrong
key reads as absent:

```bash
bao kv get -mount=secret gentian-os/kernel/mail/postfix
grep -A8 'name: smtp-relay' credentials.yaml
```
