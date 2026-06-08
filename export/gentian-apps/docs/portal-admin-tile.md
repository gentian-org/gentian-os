# Portal admin tile (tenant admins)

Tenant admins see **UMC / admin tiles**, not end-user app tiles. Admin-only apps
such as the App Store register a `portalTiles` entry with:

```yaml
portalTiles:
  - name: app-store
    displayName:
      en_US: "App Store"
    allowedGroup: "Tenant Admins"
    linkTarget: embedded
```

The gentian-os LDAP reconciler resolves `Tenant Admins` to `cn=admins_<tenant>,ou=<tenant>,...`
per tenant when provisioning portal entries.

Until the tile appears, use the ingress URL: `https://store.<tenant>.<kernel-domain>`.
