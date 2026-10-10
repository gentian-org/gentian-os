# Backing Up and Recovering Your Workspace

**For:** tenant administrators
**You will need:** the administration console, and your cluster administrator
for two of the steps below

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

**If you want to use the platform's key, ask your cluster administrator: "is a
backup key configured for this cluster?"**

The platform refuses to write your data to storage unencrypted. A backup
protected with a key or a passphrase of your own (§2) needs nothing from the
cluster; one protected with the platform's key fails until the cluster has
one — which looks like a broken feature rather than a missing setting.

They can check, and set one up, with the following. *(This section is for them,
not you — forward it.)*

> ### For the cluster administrator
>
> Check whether the operator has a recipient:
>
> ```bash
> kubectl -n kernel-control get deploy gentian-os -o \
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

Open the administration console's **Export** screen, give the backup a name,
choose who can read it, and start it.

### Choosing the encryption

This is the one decision that matters, and it is not reversible after the fact.

| Choice | Who can read the bundle | Choose it when |
|---|---|---|
| **The platform's key** | you, and whoever holds the cluster's backup key — normally your provider | this is a routine backup and you would like help restoring it |
| **A key of your workspace** — a new one, the one it already has, or one you paste | whoever holds the private key | the backup must not be readable by the platform or its operators |
| **My passphrase** | only you | the same, and you would rather remember a passphrase than keep a key file |

With **the platform's key** your cluster administrator can restore the backup
for you, which matters on the day you need it, because that day is rarely one
where you feel like following a procedure. With a key of your own, see
[§8](#8-regular-backups) for how to make and keep one.

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

The Export screen shows which app is being captured and, once each is finished,
**how long it was paused**. That number is the honest cost of a backup, and it
is worth watching on your first run so you know what to expect.

> **Why pause at all?** An app's database refers to its files, and its files
> refer back. Copying them while the app is still writing produces a set of
> pieces that were never true at the same moment — a backup that looks fine and
> restores into a broken app. Pausing is what makes the copy trustworthy.

Only one backup runs at a time per workspace. If you start a second, it waits.

---

## 4. Checking that it worked

A backup is finished when the Export screen shows **Ready**. Until then it is
still working, however long that takes on a large workspace.

Worth checking on the entry:

- **Every app is listed and Ready.** An app that failed is named, with the
  reason.
- **No app is still shown as paused.** A finished backup leaves nothing paused.
- **The encryption line matches what you chose.** If you chose your own
  passphrase, it says so, and says that only you can open it.

If a backup shows **Failed**, the reason is on the entry. The most common one on
a first attempt is that no backup key is configured — see [§1](#1-before-your-first-backup).
A failed backup is safe to delete from the list; whatever partial data it
wrote is removed with it. Deleting a **Ready** backup removes its stored bundle
permanently — there is no undo, so treat it like shredding the only copy.

> **An untested backup is a hypothesis.** Before you rely on this, ask your
> cluster administrator to run a restore drill on a scratch workspace. It is the
> only thing that turns "we have backups" into "we can recover", and the
> difference between those two sentences is usually discovered at the worst
> possible time.

---

## 5. Where the bundle lives

In the platform's object storage. The Export screen shows its location, and
**Download** on a finished backup saves it to your computer as one
`.gentian` file.

Inside the bundle, one file — `bundle-info.json` — is deliberately left
unencrypted. It says whose backup this is, when it was taken, how it was
protected, and the exact command that decrypts the rest. Everything else,
including the index of what was captured, is encrypted.

---

## 6. Recovering your data

**Restoring is a cluster-administrator operation today.** There is no button in
the administration console. This is deliberate for now: a restore replaces live data with
what the backup recorded, and everything written since is lost.

### What to tell your cluster administrator

1. **Which backup** — its name on the Export screen.
2. **Which apps**, if you only want some of them restored.
3. **The passphrase**, if you chose your own for a manual backup, or **the
   private key** (`AGE-SECRET-KEY-…`) if your schedule encrypts to a key only
   you hold. Without it nobody can help you, including them.

### What to expect afterwards

**Your members will not be able to sign in until their passwords are reset.**
Backups do not contain passwords — they are not stored in a form that can be
copied — so accounts come back without them. After a restore, use the
administration console's **Members** screen to send each member a password
reset.

Plan for this. It is the part that catches people out: the data is all there,
and the workspace looks broken because nobody can get in.

**A restore says what it left out.** It puts back the apps the backup holds. If
one of them is not installed any more, or is installed in an older version than
the backup was taken with, that app is not restored and the result names it and
says why; everything else is. Apps you installed after the backup was taken are
not touched.

Everything written after the backup was taken is gone. If you are restoring
because of a mistake rather than a loss, consider asking for a fresh backup
first, so the current state is recoverable too.

---

## 7. What is and is not in a backup

**Captured:**

- Each app's database, and any further databases an app that lets you create
  them has made (on PostgreSQL)
- Files and objects your apps store
- The contents of app volumes
- The data of apps you uninstalled without purging them: it stays in every
  backup until you purge it
- Your member accounts, groups and their memberships
- What your members set up on the desktop, and your workspace's notices
- Access rights granted beyond the standard ones
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
- **Credentials you entered yourself** — the password of a private app
  repository, an outgoing mail relay, an API key. They are stored outside your
  apps' data and are not in the bundle; after a restore into a new workspace
  they have to be entered again.
- **Mailboxes, sometimes.** Where your platform runs its own mail server
  and your workspace has a mail domain of its own, your mailboxes are in the
  backup. Where your addresses are on the platform's own domain, or mail is
  hosted elsewhere, they are not, and the backup's result says so.
  The archived mailboxes of people you removed are in it too, and come back
  as archived ones.

---

## 8. Regular backups

The administration console takes one backup, now. Backups on a schedule, where
they are stored and how long they are kept are a backup policy
(`BackupPolicy`), which your cluster administrator sets for the cluster or for
your workspace ([commands.md](commands.md) §13–§14), or which the Operations
Console manages where that app is installed. Ask for one: a nightly backup you
never think about is worth considerably more than a manual one you take when
you remember.

### Which key a schedule uses

A schedule cannot use a passphrase — there is nobody to type one at three in the
morning — so the choice is which key it encrypts to.

| Choice | Who can read the bundles | Choose it when |
|---|---|---|
| **The platform's key** | you, and whoever holds the cluster's backup key — normally your provider | this is a routine backup and you would like help restoring it |
| **A key only you hold** | only you | the backups must not be readable by the platform or its operators |

Choosing your own key means what it says: every bundle from the next run onwards
is written in a form nobody at the platform can open, so nobody there can help
you restore one. Lose the private key and those bundles are gone.

Make the key pair yourself, on your own machine:

```bash
age-keygen -o backup-identity.txt
age-keygen -y backup-identity.txt        # the public key — paste this one
```

The private key never reaches the platform at all. The console does not make a
key for you: a key made on the server is only as private as the server is, and
the console is built to hold none.

Give the **public** key — the line starting `age1` — to whoever sets the
schedule, or paste it into the Export screen for a single backup. Keep the
private key, the line starting `AGE-SECRET-KEY-`, offline; a copy on the cluster
you would be restoring *from* is no copy at all. Losing the file loses the
backups, with nothing anyone can do.

Where a workspace already has a key in the platform's vault, the Export screen
offers it again as *The key this workspace already has*, so one key opens every
backup the workspace makes.

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
The administration console captures the whole workspace. A single-app restore is possible
— mention it when you ask.

**How long does it take?**
It depends on how much data you have. The first one tells you; the Export screen
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
