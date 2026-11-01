# Gentian OS

Gentian OS is the open-source cloud-native operating system at the core of the Gentian
ecosystem: a Kubernetes operator and the services beside it (director, usher, custodian,
registrar, bouncer, arbiter), CRDs (`ComponentProfile`, `Component`, `Tenant`, `Cluster`, …), and
the automation that lets a cluster provision identity, mail, gateway routing, and third-party
apps for multiple tenants from declarative config.

## Purpose & scope

This repo owns the **kernel** — the operator and the director, CRDs, install/uninstall tooling, and the
kernel-level services (identity, mail, gateway, infra data stores) that every Gentian cluster
needs regardless of which apps run on it. It does **not** contain:

- App or sidecar implementations/catalogues — see [gentian-apps](https://github.com/gentian-org/gentian-apps)
  (open-source profiles) and [gentian-sidecars](https://github.com/gentian-org/gentian-sidecars)
  (sidecar templates).
- Proprietary profiles, or any private catalog: published by whoever maintains
  them, at an address a cluster or a tenant adds as a catalog
  ([docs/guides/custom-catalogs.md](docs/guides/custom-catalogs.md)).
- Any license gate. Editions (`ce · me · ie · pe · ee`) say who maintains and
  supports an app; the OS gates none of them. Whether a paid app arrives is
  decided at the repository it is pulled from, by the credential a tenant
  holds for it ([docs/concepts/store-contract.md](docs/concepts/store-contract.md) §2).
- Cluster-specific GitOps manifests (tenant instances, per-cluster config) — see
  [gentian-deployments](https://github.com/gentian-org/gentian-deployments).
- The user interfaces (the desktop, the administration console, the App Store app, the
  sign-in page) — see [gentian-ui](https://github.com/gentian-org/gentian-ui).

## Getting started

See [GETTING-STARTED.md](GETTING-STARTED.md) for prerequisites and step-by-step cluster
bootstrap via `install.sh`.

## Documentation

[docs/index.md](docs/index.md) lists every document by section and audience.

- [AGENTS.md](AGENTS.md) — repo rules for coding agents
- [docs/overview/architecture.md](docs/overview/architecture.md) — system architecture overview
- [docs/guides/](docs/guides/) — deployment, app customization, custom catalogs, identity brokering, backup and recovery, node-pool migration
- [docs/reference/](docs/reference/) — the installer, the `kubectl gentian` commands, the store API
- [docs/concepts/](docs/concepts/) — how each part is designed: kernel, control plane, multi-tenancy, routing, IAM, authorization, app catalog, versioning, mail, operations, ...
- [docs/decisions/](docs/decisions/index.md) — the architectural decisions, AD-1 onward
- [docs/security/](docs/security/) — the security principles, design, audit events and controls
- [docs/releases/](docs/releases/) — release notes

## License

MPL-2.0, with the API (`api/`, the CRDs and the store contract) under
Apache-2.0. [LICENSING.md](LICENSING.md) says what covers what and what each
asks of you; [CONTRIBUTING.md](CONTRIBUTING.md) says how to contribute.
