# ComponentProfile — folded into target-component-structure.md

This document has been folded into
[target-component-structure.md](target-component-structure.md), which is now
the one to read and to implement against. Nothing was dropped; the reasoning
here survives, and the vocabulary around it changed.

It stays as this redirect because eight documents and one code comment cite it
by section number. It is deleted, and those citations repointed, when the
target is implemented.

## What changed in the fold

Four things this document said are no longer the target. Each is argued where
it now lives.

| this said | the target says | where |
|---|---|---|
| `tenancy: system \| shared \| tenant` | `classes: service \| app \| shared-app` | §2, axis 1 |
| `requires.contracts`, absorbing `kernelRequirements` | `requires.services`, because *contract* is taken by `provides` | §4.2 |
| a system component has **no** exposure (AD-9) | a `service` exposes **gateway only**, never perimeter, and its tile asks on the cluster | §8.1 |
| `package.chart` is optional because an addon has no package | `addon` **is** a package type, alongside chart, composition and api | §3 |

One addition with no predecessor here: `spec.launch`, which says how a person
reaches a component — a tile, another component, or nothing — so an app nobody
can open is refused at admission rather than installed and lost. Target §8.2.

## Section map

| here | there |
|---|---|
| §1 Two levels, one word | §1 One kind, one instance, two levels; §2 axis 1 |
| §2 Spec | §9 The entry, end to end |
| §3 Requires and integrations are different | §4.1, §4.2 |
| §3.1 Privileges: asking, and being granted | §4.3 |
| §4 Three kinds of secret, two declared | §5 |
| §5 Exposure: `authMode` mandatory, perimeter explicit | §8 |
| §5.1 Enablement: the tenant's half | §8.3 |
| §5.2 The `exposure-policy` contract | §8.4 |
| §6 What tenancy derives | §6 What the class derives |
| §6.1 Shared instances: offered, then installed | §7 |
| §7 Where enforcement goes | §10 |
| §8 Why the model closes | §11 |
| §9 Open decisions | §14 |

The full text before the fold is in git history, at the commit that replaced
this file.
