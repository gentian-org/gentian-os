# Licensing

Gentian OS is open source. Most of it is under the Mozilla Public License 2.0;
the part other programs are written against is under Apache 2.0. This file
says which is which, why, what each asks of you, and how a file tells you its
own.

## Which license covers what

| Path | License | Why |
|---|---|---|
| everything not listed below | **MPL-2.0** — [LICENSE](LICENSE) | the operator, the director, the installer, the system services' configuration, the documentation |
| `api/` | **Apache-2.0** — [api/LICENSE](api/LICENSE) | the resource types and the bundle format: what another program imports |
| `gentianos.io_*.yaml` in `config/crd/` and `charts/gentian-os/crds/` | **Apache-2.0** | generated from `api/`, and installed into clusters by anyone. The other CRDs beside them belong to the projects they come from |
| `charts/infra/` | each chart's own | third-party charts carried here unchanged or with noted changes; see the `UPSTREAM.md` in each |

A file's own notice wins over this table. Go files carry one at the top, ending
in an `SPDX-License-Identifier` line; `make lint-go` refuses a Go file without
the right one. Other files carry none, and for them this is the notice:

> This Source Code Form is subject to the terms of the Mozilla Public License,
> v. 2.0. If a copy of the MPL was not distributed with this file, You can
> obtain one at https://mozilla.org/MPL/2.0/.

"The Gentian OS Authors" in a copyright line means the people who wrote the
file, each of whom keeps the copyright in what they wrote. The history of the
repository says who they are.

## Why this allocation

Two goals shape it. A tenant's workloads and data run on any implementation
of the same resource types and formats, so a tenant can move wherever they
want. And a large organization can run the OS inside without a legal
exception process.

| Layer | License | Why |
|---|---|---|
| The resource types, the CRDs generated from them, the bundle format | **Apache-2.0** | The portability contract. Anyone implements, imports and integrates against it, in software under any license, with attribution and nothing else |
| The app template and the design system, in the project's other repositories | **Apache-2.0** | Made to be copied into other people's apps, open and closed alike |
| The operator, the director and the other services, the installer, the system services' configuration | **MPL-2.0** | Accepted by large organizations without an exception. Its copyleft follows the file, not the program: a change to a file of the core that is distributed is published, so distributed modifications stay visible and mergeable, while new files and programs that only use the APIs stay their authors' |
| The desktop and the administration console, in gentian-ui | **MPL-2.0** | The face of the core. They are customized through branding and extension points, which are not modifications; the design system and console kit they are built on are Apache-2.0 |
| Export, import, restore, retire and purge | **MPL-2.0** | Part of the core, so a tenant's mobility never depends on an add-on. The bundle they produce is the Apache-licensed format, readable with standard tools ([docs/design/sovereignty.md](docs/design/sovereignty.md)) |
| Catalog apps and add-ons | their own | Not part of the OS, installed by choice, never required by it |

**Leaving is in the core.** Taking all of a tenant's data out, bringing it in
elsewhere and destroying what is left are core functions under the core's
license. An add-on may build on them; a tenant can always leave without it.

## Licenses the project does not use

No code the project writes for the OS carries any of these:

| License | Why not |
|---|---|
| AGPLv3 | Banned by default in many large organizations' open-source policies, so a core under it could not be run there without an exception |
| EUPL | Banned in the same policies on the same grounds |
| SSPL | Not open source under the OSI definition |
| BSL, FSL | Source-available, not open source, and the licenses whose adoption has split communities into forks |

The list binds the project's own code. The system services a default install
brings are other projects' software under their own licenses, each a
separate service that no code of the project is derived from; the object
store is SeaweedFS (Apache-2.0). A service an administrator switches on may
carry a license on the list — the monitoring stack's dashboards (Grafana) are
AGPLv3 — and is stated where it is switched on. Catalog apps keep their
upstream licenses, several of which are AGPLv3, and are installed by choice.

## What the MPL asks, in practice

- **Running it asks nothing**, modified or not, at any size, for yourself or
  for customers.
- **Distributing it** — handing somebody a build, an image, an appliance —
  asks that the source of the MPL files in it is available to them, including
  your changes *to those files*.
- **Your own files stay yours.** The obligation follows the file, not the
  program: code you add in new files, and anything that only talks to Gentian
  OS through its APIs, can carry any license, open or not.
- **Patents:** every contributor licenses the patents their contribution
  needs, and that license ends for anyone who sues over the code.

**What the license does not reach.** The MPL obliges whoever *distributes* a
modified build to publish the files they changed. A provider that modifies
the core and only hosts it distributes nothing, and the license asks nothing
of them. The one part of a hosted deployment that is distributed is the
browser code of the desktop and the console, so modified files there are
published. Keeping hosted cores compatible with each other is therefore not
the license's work; it is the work of the specified types and formats, which
every implementation reads and writes the same way.

## What Apache 2.0 on the API means

You can import the types, generate clients from the CRDs, and read and write
bundles, in software under any license, with attribution and nothing else. That is deliberate: it is what
lets a tenant's data and a cluster's integrations outlive any one
implementation.

## Add-ons

A cluster may run components that are not part of this repository and carry
other licenses, including ones that are not open source. Gentian OS builds,
installs and runs without any of them, and nothing above depends on one. A
default install may propose an add-on; it never requires one
([docs/design/sovereignty.md](docs/design/sovereignty.md) §5).

## What a default install brings that is not ours

The system services are other projects' software under their own licenses.
The object store is SeaweedFS (Apache-2.0), installed from its own chart
(Apache-2.0, vendored unchanged with its attribution in
`charts/infra/seaweedfs`); the Jobs that move a tenant's objects run rclone
(MIT) in the platform's storage tools image. Catalog apps keep their
upstream licenses and are installed by choice.

## Contributing

Contributions are accepted under the license of the file they change —
inbound is outbound — with a Developer Certificate of Origin sign-off. See
[CONTRIBUTING.md](CONTRIBUTING.md).
