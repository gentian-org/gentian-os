---
audience: customer, operator
type: overview
---

# Gentian OS in plain terms

For readers who know software and servers but not Kubernetes. The vocabulary
of the other pages is introduced here, one term at a time.

## 1. The problem it solves

The best open-source business applications are each excellent alone: files,
chat, projects, a wiki, mail, an ERP. Putting them together for one
organization is where the work begins: one sign-in for all of them, one place
to add a person and one to remove them, one address scheme, backups that
cover everything, updates that do not break what is installed, and the same
again for the next organization. Gentian OS does that assembly once, as
software, and runs it for many organizations on one cluster.

## 2. What you get

- **A workplace per organization.** Each organization is a *tenant*: it has
  its own sign-in, its own people and groups, its own addresses such as
  `files.acme.example`, and its own data, kept apart from every other tenant
  on the same machines.
- **Apps from a catalog.** An administrator picks an app and the platform
  installs it into the tenant, connected to the tenant's sign-in, storage and
  mail. An app is removed the same way. Which apps are offered comes from one
  or more *catalogs*; an optional *App Store* adds a shop front, reviews and
  paid editions on top.
- **One sign-in.** A person signs in once and reaches every app of the tenant
  they belong to. The platform decides at its front door who may reach what;
  apps do not run their own sign-in.
- **A desktop.** Every person lands on a desktop that shows the apps they
  may use. Administrators get a console beside it to run the tenant.
- **Your data stays yours.** A tenant can export everything it owns as one
  encrypted bundle, keep it anywhere, and import it into another cluster.

## 3. How a cluster is run

A cluster is described in text files kept in a git repository: which tenants
exist, which apps each has, which catalogs it reads, how mail and addresses
are set up. The platform reads that description and makes the cluster match
it. Nobody changes the running cluster by hand: a command or a click in the
console becomes a commit, and the commit becomes the change.

Three parts carry this out:

| Part | What it does |
|---|---|
| The **director** | The one program that writes the repository. It takes a command or a console action, checks who asked and whether they may, and commits. |
| **Argo CD** | Watches the repository and applies what it says to the cluster. |
| The **operator** | Turns each declared tenant and app into the running pieces: namespaces, databases, sign-in configuration, addresses, network rules. |

Behind them stand the services every tenant shares: **Keycloak** for
sign-in, **OpenFGA** for permissions, **OpenBao** for secrets, PostgreSQL and
object storage for data, and the **Gateway** at the edge that every request
enters through. These are the *kernel* of the OS; the pages under
[concepts/](../concepts/kernel.md) describe each.

## 4. What it runs on

Kubernetes, the system that schedules programs across a set of machines and
restarts them when they fail. Gentian OS is installed onto a Kubernetes
cluster with one script and needs a domain name whose records it may manage.
The cluster can be in a data center of your choosing or one you operate
yourself. Everything is open source; the editions of an app say who
maintains and supports it, and the OS puts no license gate in front of any
of them.

## 5. Where to go next

| You want to | Read |
|---|---|
| See the architecture with its parts named | [architecture.md](architecture.md) |
| Understand why an operating system, not a bundle of installers | [why-a-cloud-os.md](why-a-cloud-os.md) |
| Install a cluster | [GETTING-STARTED.md](../../GETTING-STARTED.md) |
| Run a tenant's backups as its administrator | [tenant-backup.md](../guides/tenant-backup.md) |
| Publish your own apps | [custom-catalogs.md](../guides/custom-catalogs.md) |
| Know what the platform promises about security | [principles.md](../security/principles.md) |

## 6. Glossary

| Term | Meaning |
|---|---|
| Cluster | A set of machines run as one by Kubernetes; one Gentian OS installation. |
| Tenant | One organization on a cluster: its people, apps, addresses and data. |
| Realm | A tenant's sign-in domain in Keycloak: its accounts, groups and sessions. |
| Namespace | A Kubernetes compartment; each tenant's apps run in their own. |
| App | An application installed into a tenant from a catalog, with its own address. |
| Profile | The catalog entry that says how an app is installed and what it needs. |
| Catalog | A published list of profiles a cluster or a tenant reads. |
| App Store | An optional service that lists catalogs with a shop front. |
| Edition | Who maintains and supports an app: `ce` community, `me` maintained, `ie` the operator's own, `pe` proprietary and free, `ee` paid. |
| Kernel | The shared services every tenant depends on: sign-in, permissions, secrets, data, the edge. |
| Director | The program that commits every change to the cluster's repository. |
| Operator | The program that makes the cluster match what the repository declares. |
| Gateway | The front door every request passes; it holds the one sign-in session. |
| Bundle | A tenant's complete export, encrypted to a key the tenant holds. |
