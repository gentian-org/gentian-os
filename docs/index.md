---
audience: customer, operator, app-author, architect, auditor, contributor
type: overview
---

# Gentian OS documentation

Gentian OS turns a Kubernetes cluster into a sovereign cloud operating system:
tenants, identity, routing, storage, mail and an app catalog, installed and
upgraded as one. These documents describe the system as it is. The past is in
git, the future in the issues.

Start with the [README](../README.md) for scope and
[GETTING-STARTED.md](../GETTING-STARTED.md) to install a cluster. Agents read
[AGENTS.md](../AGENTS.md) first.

## Overview

For readers who know software but not cloud-native systems.

| Document | What it answers |
|---|---|
| [architecture.md](overview/architecture.md) | What Gentian OS is, the OS analogy, the architecture at a glance, the kinds, how it scales |
| [why-a-cloud-os.md](overview/why-a-cloud-os.md) | Why open-source business software needs an operating system around it |
| [faq.md](overview/faq.md) | Frequently asked questions |

## Guides

How to do one thing, for operators, tenant administrators and app authors.

| Document | What it covers |
|---|---|
| [deployment.md](guides/deployment.md) | Deployment environments, promotion and release numbering |
| [app-customization.md](guides/app-customization.md) | Customizing an installed app along the customization ladder |
| [custom-catalogs.md](guides/custom-catalogs.md) | Publishing a catalog of your own |
| [identity-brokering.md](guides/identity-brokering.md) | People of another realm signing in to a tenant |
| [tenant-backup.md](guides/tenant-backup.md) | Backing up and recovering a workspace, as a tenant administrator |
| [recovery.md](guides/recovery.md) | Recovering after a loss of a cluster, a tenant or a key |
| [node-pool-migration.md](guides/node-pool-migration.md) | Moving a cluster to a new node flavor |

## Reference

Complete and exact: every flag, command and field.

| Document | What it lists |
|---|---|
| [install.md](reference/install.md) | The installer: steps, flags, configuration surfaces, tenancy modes |
| [commands.md](reference/commands.md) | The `kubectl gentian` commands |
| [store-api/](reference/store-api/store-api.md) | The App Store API: contract, OpenAPI documents, schema |

## Concepts

How each part is designed and why, for architects, operators and app authors.

| Document | Topic |
|---|---|
| [kernel.md](concepts/kernel.md) | Kernel functions, the default install, namespaces, system engines |
| [control-plane.md](concepts/control-plane.md) | The programs of the control plane: what each holds and refuses |
| [multi-tenancy.md](concepts/multi-tenancy.md) | Tenants, isolation, domains, tenancy modes |
| [routing.md](concepts/routing.md) | The edge, routes, sign-in sessions, embedding |
| [networking.md](concepts/networking.md) | Network layers, policies, exposure, certificates, TURN |
| [iam.md](concepts/iam.md) | Identity and access management, realms, roles, sign-in |
| [authorization.md](concepts/authorization.md) | The authorization model: relations, roles, projections |
| [app-authorization.md](concepts/app-authorization.md) | Authorization stores for apps and the arbiter |
| [partners.md](concepts/partners.md) | Identity across clusters |
| [app-catalog.md](concepts/app-catalog.md) | Profiles, classes, integrations, tiers, the component model |
| [store-contract.md](concepts/store-contract.md) | What a cluster accepts from an App Store |
| [versioning.md](concepts/versioning.md) | Versions, floors, update modes, the guarded move |
| [admin-console.md](concepts/admin-console.md) | The administration console and the desktop |
| [branding.md](concepts/branding.md) | One brand on every page: tokens, identity, publishing |
| [resource-plans.md](concepts/resource-plans.md) | Resource plans, quotas and usage |
| [mail.md](concepts/mail.md) | The mail kernel extension |
| [operations.md](concepts/operations.md) | Backup, disaster recovery, observability, image updates |
| [data-lifecycle.md](concepts/data-lifecycle.md) | What each operation does with a tenant's and an app's data |
| [sovereignty.md](concepts/sovereignty.md) | What sovereignty means here and what delivers it |
| [agentic-ai.md](concepts/agentic-ai.md) | The agentic AI layer and MCP |
| [llms.md](concepts/llms.md) | LLM serving |

## Decisions

The architectural decisions, numbered AD-1 onward, each with its reason:
[decisions/index.md](decisions/index.md).

## Security

| Document | For |
|---|---|
| [principles.md](security/principles.md) | The normative rules every change is held to |
| [design.md](security/design.md) | Identity, authorization, isolation, secrets, TLS: the security design |
| [audit-events.md](security/audit-events.md) | Audit events, log forwarding and monitoring |
| [controls.md](security/controls.md) | The controls, as an auditor reads them |

Reporting a vulnerability: [SECURITY.md](../SECURITY.md).

## Releases

One page per release in [releases/](releases/), the one place that records
what changed.

## Contributing

| Document | What it covers |
|---|---|
| [CONTRIBUTING.md](../CONTRIBUTING.md) | How to contribute, the sign-off |
| [folder-structure.md](contributing/folder-structure.md) | What lives where in this repository |
| [documentation.md](contributing/documentation.md) | How these documents are written |
| [LICENSING.md](../LICENSING.md) | What license covers what |

## Other repositories

| Repository | Holds |
|---|---|
| [gentian-apps](https://github.com/gentian-org/gentian-apps) | The open-source catalog, the profile guide, the custom app guide |
| [gentian-ui](https://github.com/gentian-org/gentian-ui) | The desktop, the administration console, the App Store app, the sign-in page |
| [gentian-deployments](https://github.com/gentian-org/gentian-deployments) | Cluster-specific GitOps manifests |
| [gentian-sidecars](https://github.com/gentian-org/gentian-sidecars) | Sidecar templates |
