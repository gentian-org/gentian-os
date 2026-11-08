---
audience: architect, auditor
type: decision
---

# AD-15 The OS is multi-lingual

Every word a person reads is translatable, and a language is added by adding
a message catalog, never by changing a component. The platform's own chrome
(desktop, console, Keycloak's pages) carries a message catalog per locale and
no inline user-visible string; a component's catalog text carries a required
fallback plus an optional map keyed by locale (`de_DE`, `en_US`). Matching is
forgiving within a language and never across one (`de-CH` is served `de_DE`);
a string with no translation renders its fallback, never a blank or a key.
The viewer's language comes from their account, and from the browser until
they have said.
