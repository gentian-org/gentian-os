---
audience: operator
type: guide
---

# The deployments repository on GitHub

Gentian OS keeps the definition of a cluster as files in a Git repository,
the **deployments repository**: its settings, its tenants, and the apps each
tenant has installed. Argo CD, which the installer puts on the cluster, reads
the repository and makes the cluster match it. The installer commits the
first version, and the director on the cluster commits every later change:
a new tenant, an installed app. Both need a token that is allowed to write
to the repository.

This guide creates the repository on GitHub and the token. It is written for
someone who knows basic IT and has not used Git hosting for deployments
before.

## 1. A GitHub account

Create one at [github.com/signup](https://github.com/signup) if you have
none. The repository can belong to your own account or to an organization
you are an owner of.

## 2. Create the repository

1. On GitHub, choose **+** (top right) → **New repository**.
2. **Owner:** your account or the organization. **Repository name:**
   `gentian-deployments`.
3. Choose **Private**. The repository describes your cluster in detail.
4. Add nothing to it: no README, no `.gitignore`, no license. The installer
   writes everything it needs.
5. Choose **Create repository**, and copy its HTTPS address,
   `https://github.com/<owner>/gentian-deployments`.

## 3. Create the token

GitHub calls it a fine-grained personal access token. It can write to this
one repository and nothing else.

1. Choose your picture (top right) → **Settings → Developer settings →
   Personal access tokens → Fine-grained tokens → Generate new token**.
2. **Token name:** for example `gentian-<cluster name>`.
3. **Resource owner:** the account or organization that owns the repository.
   An organization may first have to allow fine-grained tokens, or approve
   this one, under its own settings.
4. **Expiration:** the cluster keeps using the token for as long as it runs,
   and stops committing when the token expires. Pick a date your rules allow
   and note it.
5. **Repository access:** **Only select repositories**, and select
   `gentian-deployments`.
6. **Permissions → Repository permissions → Contents: Read and write.**
   GitHub adds **Metadata: Read-only** by itself. Nothing else is needed.
7. Choose **Generate token** and copy it. GitHub shows it once; keep it in a
   password manager.

## 4. Where the repository and the token go

- **Step 1 of [GETTING-STARTED](../../GETTING-STARTED.md#1-clone-the-deployments-repository)**
  clones the repository, by hand or, when the checkout is not there yet,
  during the install. Git asks for a user name and a password: give your
  GitHub user name, and the token as the password. Git says the repository
  is empty, which is expected.
- **Step 2** puts its address in `install.env`:

  ```bash
  GENTIAN_DEPLOYMENTS_REPO=https://github.com/<owner>/gentian-deployments
  GENTIAN_DEPLOYMENTS_BRANCH=main
  GENTIAN_DEPLOYMENTS_AUTH=basic
  ```

- **Step 3** asks for the credential `deployments-repository`: your GitHub
  user name and the token. The installer checks it against the repository
  before it touches the cluster.

When the token is about to expire, create a new one the same way and run
`./install.sh --only B-08` with it; the cluster picks it up within the hour
([reference/install.md](../reference/install.md)).

## 5. Next

You have everything step 1 needs. Go on with
[GETTING-STARTED](../../GETTING-STARTED.md#1-clone-the-deployments-repository).
