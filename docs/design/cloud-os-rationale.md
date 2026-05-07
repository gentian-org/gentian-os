# Why a Cloud OS

**Companion to:** [architecture-crossplane.md](../architecture-crossplane.md)

---

## 1. The Gap

Open-source business software is excellent — Nextcloud, OpenProject,
Element/Matrix, OX App Suite, XWiki, Jitsi each rival or surpass their
proprietary competitors in their own domain. But assembling them into a
**coherent workplace** that an organisation can actually adopt is, today,
a project measured in person-months:

- Each app brings its own identity model, its own database, its own
  storage, its own admin UI. SSO across them requires manual Keycloak
  configuration per app.
- Cross-app integration (open a Nextcloud file from inside OX App Suite,
  attach a file to an OpenProject task) requires hand-wired credentials,
  webhooks, and per-pair configuration.
- Operating ten apps means understanding ten upgrade cycles, ten
  backup/restore procedures, ten security postures.
- Onboarding a new tenant or a new app means repeating most of that
  work.

The proprietary alternatives (Microsoft 365, Google Workspace) hide all
of this behind a single "Add user" / "Install app" surface. **There is
no equivalent open-source experience.** The gap is not the apps — it is
the *integration substrate* underneath them.

## 2. The Closest Existing Analogy

A desktop operating system solves the same problem one layer down:
hundreds of independently developed applications coexist on one machine
because the kernel provides shared services (filesystem, network,
identity, IPC) through a stable API. Apps don't ship their own TCP
stack; they call `socket()`. Apps don't manage their own user
accounts; they ask PAM. Apps don't roll their own clipboard; they use
the windowing system's.

The result: installing a new desktop app is a one-click operation.
Removing it cleans up after itself. Apps interoperate (drag a file from
the file manager into the editor) without prior integration work.

A cloud-native equivalent would do the same for organisations:

> Click "install Nextcloud" — and the platform allocates a database,
> registers an OIDC client, mints S3 credentials, configures DKIM for
> the tenant's domain, wires Nextcloud into the SSO portal, and exposes
> its files to other installed apps via a standard contract.

This is what Gentian OS is.

## 3. Why "OS" Is Not a Metaphor

The word "OS" is doing real work, not marketing:

- A kernel is a **resource manager with stable API**. So is Gentian
  OS — the resources are databases, OIDC clients, buckets, mailboxes
  instead of memory pages and file descriptors, but the contract shape
  is the same.
- A kernel **mediates between independently developed apps**. So does
  Gentian OS — the apps are full SaaS-class products from independent
  upstream communities.
- A kernel **provides drivers for hardware diversity**. Gentian OS
  uses the same model (Crossplane providers) for cloud-API diversity.
- A kernel **enforces isolation between processes**. Gentian OS
  enforces isolation between tenants.

Every architectural decision in the platform — the kernel/userspace
split, the syscall-style declarative API, the driver model, the
reconcile loop, the default install — derives from taking the OS
analogy seriously rather than as a slogan. See
[kernel.md](kernel.md) for the full unpacking.

## 4. Why Now

Two enablers make this possible today that were not possible five
years ago:

- **Kubernetes operator ecosystem.** Identity (Keycloak), databases
  (CloudNativePG), object storage (MinIO), secrets (External Secrets),
  TLS (cert-manager) all have mature, production-grade operators. The
  "drivers" exist; the kernel only needs to compose them.
- **Crossplane.** A generic, declarative composition engine for
  Kubernetes resources. Without it, the kernel would be a custom Go
  controller; with it, the kernel is mostly YAML. This is the
  difference between writing your own ext4 implementation and using
  Linux's.

The third enabler — **agentic AI via MCP** — is what makes the OS
interesting beyond automation: once apps expose capabilities through a
standard discovery protocol, the assistant becomes the integration
layer that no manual pairwise wiring could ever match. See
[agentic-ai.md](agentic-ai.md).

## 5. Non-Goals

To be precise about what Gentian OS is *not*:

- **Not a single-product distribution.** It does not ship Nextcloud
  with a custom skin; it ships the platform underneath that lets
  Nextcloud (and others) be installed cleanly.
- **Not a Kubernetes distribution.** It runs on any conformant K8s.
- **Not a SaaS product.** It is the platform someone could build a
  SaaS on top of, or run for their own organisation.
- **Not a fork of any upstream app.** AppProfiles wrap upstream Helm
  charts as published; if patches are required, the goal is upstream
  contribution.

These boundaries exist because the value of the platform comes from
*not* fragmenting the upstream ecosystem.
