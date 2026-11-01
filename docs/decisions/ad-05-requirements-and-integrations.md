---
audience: architect, auditor
type: decision
---

# AD-5 Requirements and integrations are separate fields

A requirement is owed by the platform, gates admission, and its absence is
the platform's fault; an integration is a peer relationship, bound
continuously, absent peers normal. Privileges (egress beyond the baseline,
pod-security waivers, elevated roles) are requirements with one approval
path, so a profile cannot grant itself any of them
([app-catalog.md §3.1](../concepts/app-catalog.md),
[security/design.md §3.4](../security/design.md)).
