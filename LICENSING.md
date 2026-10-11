# Licensing

Gentian OS is open source. Most of it is under the Mozilla Public License 2.0;
the part other programs are written against is under Apache 2.0. This file
says which is which, what each asks of you, and how a file tells you its own.

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

## What Apache 2.0 on the API means

You can import the types, generate clients from the CRDs, and read and write
bundles, in software under any license, with attribution and nothing else. That is deliberate: it is what
lets a tenant's data and a cluster's integrations outlive any one
implementation.

## Add-ons

A cluster may run components that are not part of this repository and carry
other licenses, including ones that are not open source. Gentian OS builds,
installs and runs without any of them, and nothing above depends on one.

## What a default install brings that is not ours

The system services are other projects' software under their own licenses.
One is copyleft with a network clause: MinIO, the default object store, is
AGPL-3.0. It runs as a separate service behind the S3 API and no Gentian code
derives from it, but an organisation that excludes AGPL software will want to
know it is there. Catalogue apps keep their upstream licenses and are
installed by choice.

## Contributing

Contributions are accepted under the license of the file they change —
inbound is outbound — with a Developer Certificate of Origin sign-off. See
[CONTRIBUTING.md](CONTRIBUTING.md).
