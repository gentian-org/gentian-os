# Backing Up and Recovering Your Workspace

**For:** tenant administrators
**You will need:** the Admin Console, and your cluster administrator for two of
the steps below

A backup captures your whole workspace — your apps' databases, the files they
store, and your member accounts — into a single encrypted bundle. This guide
covers taking one, checking it worked, and getting your data back.

Two things are worth knowing before you start, because they surprise people:

- **Taking a backup briefly pauses your apps**, one at a time. This is not a
  side effect to be engineered away; it is the only way the backup can be
  internally consistent. See [§3](#3-what-happens-while-it-runs).
- **Restoring is not yet self-service.** You ask your cluster administrator.
  See [§6](#6-recovering-your-data).

---

## 1. Before your first backup

**Ask your cluster administrator: "is a backup key configured for this
cluster?"**

Nothing can be backed up until one is. The platform refuses to write your data
to storage unencrypted, so with no key configured your first backup will simply
fail — which looks like a broken feature rather than a missing setting.

They can check, and set one up, with the following. *(This section is for them,
not you — forward it.)*

> ### For the cluster administrator
>
> Check whether the operator has a recipient:
>
> ```bash
> kubectl -n gentian-system get deploy gentian-os -o \
>   jsonpath='{range .spec.template.spec.containers[0].env[?(@.name=="BACKUP_AGE_RECIPIENTS")]}{.value}{"\n"}{end}'
> ```
>
> If that prints nothing, create a key pair with
> [age](https://age-encryption.org):
>
> ```bash
> age-keygen -o backup-identity.txt
> chmod 600 backup-identity.txt
> age-keygen -y backup-identity.txt        # the public key, safe to share
> ```
>
> Put the **public** key in the cluster's operator values in
> `gentian-deployments`, and commit — Argo CD applies it:
>
> ```yaml
> # clusters/<cluster>/kernel/values.yaml
> backupRecipients:
>   - age1...            # the public key printed above
> ```
>
> **Keep `backup-identity.txt` off the cluster.** A key stored in the cluster it
> protects is readable by whoever compromises that cluster, which is precisely
> the situation backups exist for. It belongs with the recovery kit and the
> master password, wherever your organisation keeps break-glass material.
>
> Losing it means every bundle encrypted to it is unreadable. There is no
> recovery path, and that is deliberate.
>
> More than one recipient can be listed; any of them can decrypt independently,
> which is how you give a second recovery key to a different holder.

---

## 2. Taking a backup

Open **Admin Console → Backup**, give it a name, choose how it should be
protected, and start it.

### Choosing the encryption

This is the one decision that matters, and it is not reversible after the fact.

| Choice | Who can read the bundle | Choose it when |
|---|---|---|
| **Platform key** | you, and whoever holds the cluster's backup key — normally your provider | this is a routine backup and you would like help restoring it |
| **My passphrase** | only you | the backup must not be readable by the platform or its operators |

**Platform key** is the sensible default. Your cluster administrator can restore
it for you, which matters on the day you need it, because that day is rarely one
where you feel like following a procedure.

**My passphrase** means exactly what it says. Nobody else can open the bundle —
not your provider, not support, not anyone who later gains access to the
cluster. If you lose the passphrase, the bundle is gone. Use a password manager,
not your memory.

Whichever you pick, the bundle ends up as a standard
[age](https://age-encryption.org) file, so you are never dependent on Gentian
tooling to open it.

---

## 3. What happens while it runs

The backup works through your apps **one at a time**. For each one it pauses
writes, copies that app's database, files and stored objects, then lets the app
resume before moving on.

Your other apps keep running throughout. Only the app being captured is
affected, and only for as long as its own copy takes.

What your users see depends on the app:

- Apps with a maintenance mode — Nextcloud, for instance — show a maintenance
  page and come back by themselves.
- Apps without one are stopped and restarted. To a user that looks like the app
  being briefly unavailable.

The Backup tab shows which app is being captured and, once each is finished,
**how long it was paused**. That number is the honest cost of a backup, and it
is worth watching on your first run so you know what to expect.

> **Why pause at all?** An app's database refers to its files, and its files
> refer back. Copying them while the app is still writing produces a set of
> pieces that were never true at the same moment — a backup that looks fine and
> restores into a broken app. Pausing is what makes the copy trustworthy.

Only one backup runs at a time per workspace. If you start a second, it waits.

---

## 4. Checking that it worked

A backup is finished when the Backup tab shows **Ready**. Until then it is
still working, however long that takes on a large workspace.

Worth checking on the entry:

- **Every app is listed and Ready.** An app that failed is named, with the
  reason.
- **No app is still shown as paused.** A finished backup leaves nothing paused.
- **The encryption line matches what you chose.** If you chose your own
  passphrase, it says so, and says that only you can open it.

If a backup shows **Failed**, the reason is on the entry. The most common one on
a first attempt is that no backup key is configured — see [§1](#1-before-your-first-backup).
A failed backup is safe to delete from the Backup tab; whatever partial data it
wrote is removed with it. Deleting a **Ready** backup removes its stored bundle
permanently — there is no undo, so treat it like shredding the only copy.

> **An untested backup is a hypothesis.** Before you rely on this, ask your
> cluster administrator to run a restore drill on a scratch workspace. It is the
> only thing that turns "we have backups" into "we can recover", and the
> difference between those two sentences is usually discovered at the worst
> possible time.

---

## 5. Where the bundle lives

In the platform's object storage, not on your computer. The Backup tab shows its
location.

There is no download button yet. A workspace-sized bundle is not really a
browser download, and serving one safely needs work that has not been done —
so for now, getting a copy is a request to your cluster administrator.

Inside the bundle, one file — `bundle-info.json` — is deliberately left
unencrypted. It says whose backup this is, when it was taken, how it was
protected, and the exact command that decrypts the rest. Everything else,
including the index of what was captured, is encrypted.

---

## 6. Recovering your data

**Restoring is a cluster-administrator operation today.** There is no button in
the Admin Console. This is deliberate for now: a restore replaces live data with
what the backup recorded, and everything written since is lost.

### What to tell your cluster administrator

1. **Which backup** — the name from the Backup tab.
2. **Which apps**, if you only want some of them restored.
3. **The passphrase**, if you chose your own for a manual backup, or **the
   private key** (`AGE-SECRET-KEY-…`) if your schedule encrypts to a key only
   you hold. Without it nobody can help you, including them.

### What to expect afterwards

**Your members will not be able to sign in until their passwords are reset.**
Backups do not contain passwords — they are not stored in a form that can be
copied — so accounts come back without them. After a restore, use
**Admin Console → Members** to send each member a password reset.

Plan for this. It is the part that catches people out: the data is all there,
and the workspace looks broken because nobody can get in.

Everything written after the backup was taken is gone. If you are restoring
because of a mistake rather than a loss, consider asking for a fresh backup
first, so the current state is recoverable too.

---

## 7. What is and is not in a backup

**Captured:**

- Each app's database
- Files and objects your apps store
- The contents of app volumes
- Your member accounts, groups and their memberships
- Your workspace's configuration, so it can be rebuilt elsewhere

**Not captured, on purpose:**

- **Passwords.** See above.
- **Caches.** Rebuilt automatically; copying them would waste space and restore
  nothing useful.
- **Derived files** an app can regenerate — image previews, search indexes. Each
  app decides what counts, so its backup stays proportionate to its real data.
- **Platform credentials.** Your apps' internal passwords are regenerated by the
  platform rather than stored in the bundle. This means a leaked bundle does not
  hand anyone a working login.

---

## 8. Regular backups

Backups on a schedule are set in **Admin Console → Backup settings**, per
workspace, by your workspace administrator — or by your cluster administrator on
your behalf. Set one up: a nightly backup you never think about is worth
considerably more than a manual one you take when you remember.

### Which key a schedule uses

A schedule cannot use a passphrase — there is nobody to type one at three in the
morning — so the choice is which key it encrypts to, in **Admin Console → Backup
settings → Who can read the backups**.

| Choice | Who can read the bundles | Choose it when |
|---|---|---|
| **The platform's key** | you, and whoever holds the cluster's backup key — normally your provider | this is a routine backup and you would like help restoring it |
| **A key only you hold** | only you | the backups must not be readable by the platform or its operators |

Choosing your own key means what it says: every bundle from the next run onwards
is written in a form nobody at the platform can open, so nobody there can help
you restore one. Lose the private key and those bundles are gone.

Get a key pair one of two ways:

- **Generate it yourself**, which is the stronger route:

  ```bash
  age-keygen -o backup-identity.txt
  age-keygen -y backup-identity.txt        # the public key — paste this one
  ```

  The private key never reaches the platform at all.

- **Ask the console to generate one**, with *Generate a key for me*. The private
  key is shown once and stored nowhere, but it was made on the server, so it is
  only as private as the server is.

Paste the **public** key — the line starting `age1` — into the form. Keep the
private key, the line starting `AGE-SECRET-KEY-`, offline; a copy on the cluster
you would be restoring *from* is no copy at all.

**Keep a copy in the vault** is ticked by default when the console generates a
key for you. It stores the private key in your workspace's own area of the
platform's vault, so a download you never got round to saving is not fatal: you
can read the key back and restore. It is readable by a workspace administrator
and by nobody else — not other workspaces, and not the platform, which is
refused it explicitly.

Untick it and the download is the only copy in existence. That is the stronger
position and the one to choose if the point of holding your own key is that no
copy should sit on the platform at all. It also means losing the file loses the
backups, with nothing anyone can do.

Either way, save the download. The vault copy lives on the same cluster as the
backups; a disaster that takes the cluster takes it too.

You can name more than one key, one per line. Every listed key opens the bundle
independently, so naming your provider's alongside your own is how you keep a key
of your own without giving up their help.

Restoring from a bundle encrypted to your own key means supplying the private key
at the time — see [§6](#6-recovering-your-data). Switching keys applies from the next run;
bundles already taken keep the key they were written with and are still
restorable.

Two things worth asking your administrator to confirm:

- **How many are kept**, and therefore how far back you can go.
- **That someone is alerted when a schedule stops succeeding.** A schedule that
  runs nightly and fails every time looks healthy by every other measure. This
  is the failure mode a backup regime cannot afford, and the platform records a
  last-success time precisely so it can be watched.

---

## Common questions

**Can I back up a single app?**
The Admin Console captures the whole workspace. A single-app restore is possible
— mention it when you ask.

**How long does it take?**
It depends on how much data you have. The first one tells you; the Backup tab
records how long each app was paused, which is the part your users notice.

**Can I take a backup during working hours?**
Yes, but your apps pause one at a time while it runs. Outside working hours is
kinder, which is an argument for a schedule.

**Does a backup slow my apps down?**
Only the app being captured, which is paused rather than slowed. The others are
unaffected.

**What if I lose my passphrase?**
The bundle cannot be opened. Not by you, not by your provider, not by anyone.
That is the guarantee you chose when you selected it.

---

## For cluster administrators

The operator-side procedures — the `TenantExport`, `TenantRestore` and
`TenantExportSchedule` resources, and a restore drill worth running before any
of this is relied on — are in [commands.md](commands.md) §11–§15.
The recovery procedures themselves are in
[recovery-playbook.md](recovery-playbook.md).
