# Install reference

Everything about installing that is not the first-install walkthrough. For that,
see [GETTING-STARTED.md](../GETTING-STARTED.md).

---

## 1. How the installer is put together

`install.sh` installs nothing itself. It is a driver over a directory of steps:

```text
scripts/steps/A-01-crossplane.sh
scripts/steps/A-08-eso.sh
scripts/steps/B-07-cluster-xr.sh
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
less scripts/steps/B-07-cluster-xr.sh
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
> nothing. If reproducibility is part of your recovery plan, back up
> `gentian-os/kernel/internal/master-password` — both keys, not just the
> password you typed.

Under `random` there is nothing to reproduce; recovery means restoring OpenBao.

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
