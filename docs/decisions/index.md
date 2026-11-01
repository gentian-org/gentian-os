---
audience: architect, auditor
type: decision
---

# Architectural decisions

The decisions that set the direction of the architecture. Each is stated once,
in its own file by number, with its reason and a link to the design document that holds
the detail. Other documents cite them as "AD-n". A decision changes by editing
its entry, not by contradicting it elsewhere.

| | Decision |
|---|---|
| AD-1 | [The security principles are normative](ad-01-security-principles-normative.md) |
| AD-2 | [The director is the only writer to the deployment repository](ad-02-director-only-writer.md) |
| AD-3 | [The store's data lives outside the cluster; its interface is an app on it](ad-03-store-outside-cluster.md) |
| AD-4 | [One catalog kind, `ComponentProfile`, with classes](ad-04-one-catalog-kind.md) |
| AD-5 | [Requirements and integrations are separate fields](ad-05-requirements-and-integrations.md) |
| AD-6 | [Exposure carries a mandatory `authMode` and a `surface`](ad-06-exposure-authmode-surface.md) |
| AD-7 | [Namespaces are named by tier](ad-07-namespaces-by-tier.md) |
| AD-8 | [Kernel trust domains are separate namespaces](ad-08-kernel-trust-domains.md) |
| AD-9 | [System services have no public route](ad-09-no-public-route-for-system-services.md) |
| AD-10 | [The desktop is the desktop, for everyone; the consoles are apps](ad-10-desktop-and-consoles.md) |
| AD-11 | [The layout applies to fresh installs](ad-11-layout-fresh-installs.md) |
| AD-12 | [The authorization store is a projection; Keycloak's events are the only source of membership](ad-12-authorization-store-projection.md) |
| AD-13 | [The edge is the only session authority](ad-13-edge-session-authority.md) |
| AD-14 | [Without a store, apps are installed by command only](ad-14-install-by-command-without-store.md) |
| AD-15 | [The OS is multi-lingual](ad-15-multi-lingual.md) |
| AD-16 | [Every cluster role but `admin` and `breakGlass` is held by a person named in git](ad-16-cluster-roles-named-in-git.md) |
| AD-17 | [Identity crosses clusters only as a partner link, by standards](ad-17-identity-crosses-as-partner-link.md) |
