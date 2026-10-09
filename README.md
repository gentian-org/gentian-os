# Gentian OS

Gentian OS is the open-source cloud-native operating system at the core of the Gentian
ecosystem: a Kubernetes operator and the services beside it (director, usher, custodian,
registrar, bouncer), CRDs (`ComponentProfile`, `Component`, `Tenant`, `Cluster`, …), and
the automation that lets a cluster provision identity, mail, gateway routing, and third-party
apps for multiple tenants from declarative config.

## Purpose & scope

This repo owns the **kernel** — the operator and the director, CRDs, install/uninstall tooling, and the
kernel-level services (identity, mail, gateway, infra data stores) that every Gentian cluster
needs regardless of which apps run on it. It does **not** contain:

- App or sidecar implementations/catalogues — see [gentian-apps](https://github.com/gentian-org/gentian-apps)
  (open-source profiles) and [gentian-sidecars](https://github.com/gentian-org/gentian-sidecars)
  (sidecar templates).
- Proprietary profiles, or any private catalogue: published by whoever maintains
  them, at an address a cluster or a tenant adds as a catalogue
  ([docs/custom-catalogues.md](docs/custom-catalogues.md)).
- Any licence gate. Editions (`ce · pe · me · ee`) say who maintains and
  supports an app; the OS gates none of them. Whether a paid app arrives is
  decided at the repository it is pulled from, by the credential a tenant
  holds for it ([docs/design/store-contract.md](docs/design/store-contract.md) §2).
- Cluster-specific GitOps manifests (tenant instances, per-cluster config) — see
  [gentian-deployments](https://github.com/gentian-org/gentian-deployments).
- The user interfaces (the desktop, the administration console, the App Store app, the
  sign-in page) — see [gentian-ui](https://github.com/gentian-org/gentian-ui).

## Getting started

See [GETTING-STARTED.md](GETTING-STARTED.md) for prerequisites and step-by-step cluster
bootstrap via `install.sh`.

## Documentation

- [AGENTS.md](AGENTS.md) — repo rules for coding agents
- [docs/architecture.md](docs/architecture.md) — system architecture overview
- [docs/commands.md](docs/commands.md) — `kubectl gentian` commands
- [docs/install-reference.md](docs/install-reference.md) — installer steps, flags and configuration
- [docs/deployment.md](docs/deployment.md) — deployment model
- [docs/custom-catalogues.md](docs/custom-catalogues.md) — publishing your own apps
- [docs/app-customization.md](docs/app-customization.md) — customizing an installed app
- [docs/security-principles.md](docs/security-principles.md) — the security rules
- [docs/node-pool-migration.md](docs/node-pool-migration.md) — migrating a cluster to a new node flavour
- [docs/faq.md](docs/faq.md) — frequently asked questions
- [docs/roadmap.md](docs/roadmap.md) — roadmap
- [docs/design/](docs/design/) — architecture deep-dives (kernel, IAM, gateway, multi-tenancy, security, ...)
- [docs/research/](docs/research/) — exploratory research notes

## License

MPL-2.0, with the API (`api/`, the CRDs and the store contract) under
Apache-2.0. [LICENSING.md](LICENSING.md) says what covers what and what each
asks of you; [CONTRIBUTING.md](CONTRIBUTING.md) says how to contribute.
