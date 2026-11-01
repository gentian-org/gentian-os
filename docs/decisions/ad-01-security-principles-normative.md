---
audience: architect, auditor
type: decision
---

# AD-1 The security principles are normative

The nine rules in [security/principles.md](../security/principles.md) bind
every change; one that breaks a rule needs a written reason. Every request
passes exactly one named enforcement point (the Gateway, a publishing proxy,
the director, the custodian, the MCP gateway), and apps never decide platform
questions. One accepted limit: on a public entry of `authMode: app` the
publishing proxy enforces paths, stripped headers and rate, not who calls; the
credential is the app's, so a person removed from the tenant keeps access
through it until the app revokes it ([security/design.md §2.14](../security/design.md),
[iam.md §1.11](../concepts/iam.md)).
