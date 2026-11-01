---
audience: contributor
type: contributing
---

# How the documentation is written

The rules every page under `docs/` follows. They hold for people and for
coding agents alike.

## 1. The present tense

A page describes the system as it is. It carries no dates of past work, no
"formerly", no "planned" and no "not yet". The past is in git, the future is
in the issues. Two structures are described as planned by decision: class
`shared-app` and class `service`, in
[app-catalog.md](../concepts/app-catalog.md). The release notes under
[releases/](../releases/) are the one place that records change.

## 2. Where a page goes

| Directory | Holds | Type |
|---|---|---|
| `overview/` | What the system is, for readers new to cloud-native systems | `overview` |
| `guides/` | How to do one thing | `guide` |
| `reference/` | Complete, exact lists: flags, commands, fields, APIs | `reference` |
| `concepts/` | How a part is designed and why | `concept` |
| `decisions/` | One architectural decision per file, numbered AD-n | `decision` |
| `security/` | The security principles, design, audit events and controls | `concept`, `reference` |
| `releases/` | One page per release | `release` |
| `contributing/` | Working in this repository | `contributing` |

[index.md](../index.md) lists every page; a new page is added there.

## 3. Front matter

Every page starts with its audience and type:

```yaml
---
audience: operator, architect
type: concept
---
```

Audiences: `customer` (a tenant administrator or a person deciding on Gentian),
`operator` (runs a cluster), `app-author` (writes profiles or apps),
`architect` (designs with or against the platform), `auditor` (reads the
controls), `store-vendor` (runs an App Store), `contributor` (works in this
repository).

## 4. Sections and citations

Sections are numbered: `## 1. Title`, `### 1.1 Title`. Code, charts and other
pages cite a page by path and section, as in `docs/concepts/mail.md §9c`, so
an existing section keeps its number; new content is appended as a new
section or placed inside the section it belongs to. A decision is cited as
"AD-n" with a link to its file under `decisions/`.

Links are relative. A link to code names the file, as in
`internal/controller/tenant_controller.go`, so a reader can open it.

## 5. Language

American English: catalog, organization, license, color. An identifier keeps
its spelling, as in `kubectl gentian catalogues`. Plain, factual sentences;
no marketing. People are "they". The platform components are named as the
code names them: the Gateway, the bouncer, the director, the operator, the
registrar, the custodian, the usher, the arbiter, the concierge.

## 6. What stays out

Business content, market and pricing, names of companies that build on
Gentian, and references to transient plans or trackers. Security gaps are
reported through a private security advisory, not written into a page.

## 7. Checks

`make lint-docs` runs `scripts/lint/lint-docs.py`: every relative link
resolves and every anchor exists, every page under `docs/` carries its front
matter and is listed in [index.md](../index.md), and prose outside code is
American English. `make lint-spelling` runs codespell over the pages with the
settings in `.codespellrc`. `make verify-gen` regenerates the reference pages
that `scripts/gen/gen-docs.py` writes and fails when a committed copy differs.
`scripts/tools/verify-authz-vocabulary.sh` checks that the `can_*` relations
the pages name and the ones the model defines are the same set. The render
fixtures under `crossplane/tests/unit/render/` are byte-identical copies of the
compositions, so a citation changed in a composition is changed in every copy.
