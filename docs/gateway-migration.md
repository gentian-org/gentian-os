# Gateway API Migration Plan

## Migration status (2026-06-16)

Validated on the **test** cluster (`desk.gentian.org`, `ROUTING_MODE=gateway`,
`NETWORK_MODE=tunnel`). External checks: portal login/OIDC (`redirect_uri=https`),
tenant apex redirect, Element/chat, kernel services (portal, id, files, office,
argocd), and Cloudflare tunnel ingress.

| Phase | Status | Notes |
|-------|--------|-------|
| **A — Foundation** | **Complete** | Envoy Gateway + Gateway API CRDs; `gentian-envoy` GatewayClass; `kernel-public-gateway`; operator reconcilers wired; test cluster on `routingMode: gateway`. |
| **B — Tenant app routing** | **Complete** | HTTPRoutes + `BackendTrafficPolicy` per app; stale route cleanup; tenant wildcard TLS via DNS-01. |
| **C — Kernel and special cases** | **Complete** | Kernel HTTPRoutes (portal, UMC, idp, cryptpad, collabora, nextcloud, argocd, intercom, apex redirect); Keycloak broker CSP; CoreDNS hairpin; tunnel origin wiring; superseded kernel Ingress cleanup. |
| **D — API and catalogue cleanup** | **Complete (bridge)** | Operator maps `AppProfile.spec.ingress.annotations` (nginx keys) → Envoy `BackendTrafficPolicy` / response headers. Profiles still author nginx keys; typed `routePolicy` fields remain optional follow-up. |
| **E — Cleanup and hardening** | **Complete** | Ingress reconcilers removed; gateway-only defaults; legacy install path retired; docs/runbooks updated. Test cluster validated on gateway mode. |

### Remaining follow-ups (post Phase E)

- [ ] Optional: typed `AppProfile.spec.routePolicy` fields and profile migration.
- [ ] Optional: CI lint in `gentian-apps` rejecting deprecated nginx-only keys.
- [ ] Cluster hygiene: disable MicroK8s `ingress` addon on gateway-only clusters (outside Gentian install scope).

---

## 1. Goal

Implement Gentian OS edge routing on Gateway API with Envoy Gateway as the
only supported routing stack, while preserving tenant isolation, TLS behavior,
portal embedding, OIDC flows, and app catalogue ergonomics.

## 2. Scope

In scope:

- Tenant app routing resources
- Kernel shared routing resources
- Redirect, header, and CSP edge policy
- AppProfile schema and generated CRDs
- Installer, deployment values, and docs
- Test suites and operational runbooks

Out of scope:

- Replacing cert-manager
- Replacing tenant identity model
- Replacing app deployment model (Crossplane Releases remain unchanged)

## 3. Success Criteria

1. All kernel and tenant hosts are served via Gateway API resources.
2. No runtime dependency on ingress-nginx.
3. Portal iframe embedding works for standard apps and CryptPad.
4. Keycloak OIDC broker login inside app iframes works across tenants.
5. Tenant wildcard TLS issuance and renewal remain automatic.
6. Tenant add/remove and app install/uninstall flows stay single-resource driven.
7. Existing docs and day-2 commands reflect Gateway API operation.

## 4. Delivery Strategy

Use phased delivery with a cold-start model:

- **Phase A**: platform prerequisites and feature flags — **Complete**
- **Phase B**: tenant app route rendering — **Complete**
- **Phase C**: kernel/shared routes and special policies — **Complete**
- **Phase D**: schema cleanup and annotation retirement — **Complete (bridge)**
- **Phase E**: cleanup and hardening — **Complete**

## 5. Phased Implementation

### Phase A - Foundation ✅

Deliverables:

- Install Envoy Gateway and Gateway API CRDs.
- Introduce operator routing mode flag: `ROUTING_MODE=gateway`.
- Add shared GatewayClass and initial Gateways.
- Add controller wiring for Gateway API clients and reconcilers.

Primary files:

- `scripts/install-lib.sh`
- `install.env.template`
- `charts/gentian-os/templates/deployment.yaml`
- `gentian-deployments/clusters/*/kernel/values-*.yaml`
- `docs/getting-started.md`
- `docs/FAQ.md`

Acceptance checks:

- [x] Envoy Gateway and Gateway API CRDs healthy after install.
- [x] Kernel and tenant Gateway objects can be created and reach listener
  `Programmed` state (Gateway-level `Programmed=False` with
  `AddressNotAssigned` is expected on `NETWORK_MODE=tunnel` / ClusterIP).

### Phase B - Tenant App Routing ✅

Deliverables:

- Render HTTPRoutes for all tenant app ingress intents.
- Attach per-route policy profiles for headers/timeouts/body limits.

Primary files:

- `internal/controller/gateway_reconciler.go`
- `internal/controller/gateway_policy.go`
- `internal/controller/tenant_controller.go`
- `internal/controller/*_test.go`

Acceptance checks:

- [x] App install creates HTTPRoute resources with healthy conditions.
- [x] Traffic to tenant app hosts resolves correctly.
- [x] Uninstall removes stale HTTPRoutes cleanly.

### Phase C - Kernel and Special Cases ✅

Deliverables:

- Move kernel endpoint routing to Gateway API resources.
- Implement apex redirect with HTTPRoute filters.
- Implement Keycloak broker embedding policy on Gateway API resources.
- Implement CryptPad-specific CSP handling on Gateway API resources.

Primary files:

- `internal/controller/kernel_gateway_routes.go`
- `internal/controller/keycloak_gateway_reconciler.go`
- `internal/controller/gateway_platform_reconciler.go`
- `internal/controller/gateway_tunnel_ingress.go`
- `kernel/services/*/manifests/*/values/gateway.yaml`

Acceptance checks:

- [x] Portal redirect works for tenant apex domains.
- [x] OIDC login flow works (portal → Keycloak with `https` redirect_uri).
- [x] CryptPad main+sandbox embedding policy on Gateway routes.
- [x] Cloudflare tunnel + CoreDNS hairpin wired to Envoy Gateway.

### Phase D - API and Catalogue Cleanup ✅ (bridge)

Deliverables:

- Introduce structured, controller-agnostic route policy fields in AppProfile.
- Deprecate direct nginx annotation maps in profiles.
- Regenerate CRDs and update profile authoring guides.

**Delivered via bridge:** `gateway_policy.go` translates nginx annotation keys
to Envoy `BackendTrafficPolicy` and response header modifiers. Catalogue
profiles continue to use nginx keys for authoring ergonomics.

Primary files:

- `internal/controller/gateway_policy.go`
- `gentian-apps/profiles/*.yaml` (nginx keys → Envoy via bridge)
- `gentian-apps/app-profile-guide.md`

Acceptance checks:

- [x] Catalogue profiles validate and render equivalent Gateway policy from
  annotation bridge.
- [ ] Typed `routePolicy` fields in `AppProfile` API (optional follow-up).

### Phase E - Cleanup and Hardening ✅

Deliverables:

- Remove ingress-nginx installation and health checks.
- Remove legacy ingress reconcilers and annotation helpers.
- Update runbooks, troubleshooting, and architecture docs.

Primary files:

- `scripts/install-lib.sh`
- `docs/getting-started.md`
- `docs/FAQ.md`
- `docs/architecture.md`
- `docs/commands.md`
- `internal/controller/tenant_edge_tls.go` (TLS/tunnel helpers extracted from removed ingress reconciler)
- `charts/gentian-os/values.yaml` (default `routingMode: gateway`)

**Completed:**

- [x] Default Helm `routingMode` and operator fallback → `gateway`.
- [x] Install-lib rejects `ROUTING_MODE=ingress`; legacy kernel Ingress manifests removed.
- [x] Treat listener-programmed Gateways as ready when address is not assigned
  (tunnel / ClusterIP).
- [x] Delete superseded tenant Ingress objects when HTTPRoutes exist.
- [x] Gateway-first docs (`commands.md`, `FAQ.md`, `architecture.md`).
- [x] Removed `ingress_reconciler.go`, `keycloak_idp_ingress_reconciler.go`, and
  ingress-mode tenant portal redirect path.
- [x] Keycloak platform reconciler watches kernel IdP HTTPRoute (not Ingress).

Acceptance checks:

- [x] Fresh install works without ingress-nginx.
- [x] End-to-end test suite green in gateway mode.

## 6. Risks and Mitigations

1. **Header/CSP parity risk**
   - Risk: nginx annotation behavior does not map 1:1 to Gateway API.
   - Mitigation: define explicit Envoy policy templates; add route-policy golden
     tests for portal, OIDC, and CryptPad paths.

2. **OIDC iframe regression risk**
   - Risk: Keycloak broker pages fail in embedded flow.
   - Mitigation: add dedicated E2E checks that execute full login flow in
     iframe context for representative apps.

3. **TLS reference and namespace-boundary risk**
   - Risk: route/listener certificate references fail due to namespace policy.
   - Mitigation: standardize Gateway/certificate placement per namespace and add
     admission checks for invalid reference topologies.

4. **Cold-switch integration risk**
   - Risk: defects appear only when all edge paths run on Gateway API in one
     release.
   - Mitigation: require full matrix validation on ephemeral clusters and
     pre-production soak before release tagging.

5. **Catalogue drift risk**
   - Risk: profiles continue to add nginx keys after typed API exists.
   - Mitigation: lints and CI policy in `gentian-apps` that reject deprecated
     ingress annotation keys.

6. **Operational knowledge gap risk**
   - Risk: day-2 teams use old ingress troubleshooting commands.
   - Mitigation: publish Gateway API runbooks and update `docs/commands.md` with
     Gateway/HTTPRoute status and Envoy troubleshooting commands.

## 7. Validation Matrix

Required validation before release:

- Fresh install in dev/staging/prod profiles
- Tenant create/delete lifecycle
- App install/uninstall lifecycle
- Portal embedding (standard apps)
- CryptPad sandbox embedding
- Keycloak OIDC broker in iframe
- Wildcard certificate issuance and renewal
- DNS and external reachability in both static-ip and tunnel modes
- Load and resilience smoke tests for route reconciliation

**Test cluster (2026-06-16):** portal OIDC, apex redirect, chat Element host,
kernel portal/id/files/office — verified externally via Cloudflare tunnel.

## 8. Work Breakdown by Repository

### gentian-os

- Controller logic, API types, CRDs, install scripts, tests, and docs.

### gentian-apps

- Profile schema updates, policy profile fields, and authoring guide changes.

### gentian-deployments

- Cluster values and Gateway parameters.

### gentian-ui

- Validate shell assumptions and browser-proxy paths against new route/policy
  behavior (no API model change expected).

## 9. Rollout and Contingency

Rollout:

1. Build and validate on ephemeral clusters.
2. Run full validation matrix in a pre-production environment.
3. Release with Gateway API as the single routing implementation.

Contingency:

- Use release rollback (Git/Helm/image rollback) if blocking defects are found.
- Keep prior release artifacts available for rapid restore.
- Preserve failed-environment manifests and logs for post-incident diffing.

## 10. Ownership and Governance

- Platform team owns controller and API changes.
- App catalogue team owns profile migration and lint rules.
- Operations team owns rollout sequencing and production cutover gates.
- Security team signs off on CSP, header policy, and OIDC flow validation.
