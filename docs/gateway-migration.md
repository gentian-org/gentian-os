# Gateway API Migration Plan

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

- **Phase A**: platform prerequisites and feature flags
- **Phase B**: tenant app route rendering
- **Phase C**: kernel/shared routes and special policies
- **Phase D**: schema cleanup and annotation retirement
- **Phase E**: cleanup and hardening

## 5. Phased Implementation

### Phase A - Foundation

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

- Envoy Gateway and Gateway API CRDs healthy after install.
- Kernel and tenant Gateway objects can be created and reach `Programmed`
  state.

### Phase B - Tenant App Routing

Deliverables:

- Render HTTPRoutes for all tenant app ingress intents.
- Attach per-route policy profiles for headers/timeouts/body limits.

Primary files:

- `internal/controller/ingress_reconciler.go`
- `internal/controller/ingress_helpers.go`
- `internal/controller/tenant_controller.go`
- `internal/controller/*_test.go` (ingress and tenant tests)

Acceptance checks:

- App install creates HTTPRoute resources with healthy conditions.
- Traffic to tenant app hosts resolves correctly.
- Uninstall removes stale HTTPRoutes cleanly.

### Phase C - Kernel and Special Cases

Deliverables:

- Move kernel endpoint routing to Gateway API resources.
- Implement apex redirect with HTTPRoute filters.
- Implement Keycloak broker embedding policy on Gateway API resources.
- Implement CryptPad-specific CSP handling on Gateway API resources.

Primary files:

- `internal/controller/keycloak_platform_reconciler.go`
- `internal/controller/umc_reconciler.go`
- `kernel/services/*/manifests/*/ingress*.yaml`
- `kernel/services/gentian-portal/values/**/*.yaml`
- `crossplane/compositions/app-element.yaml`

Acceptance checks:

- Portal redirect works for tenant apex domains.
- OIDC login in iframe works for Element/OpenProject/OX flows.
- CryptPad main+sandbox embedding works.

### Phase D - API and Catalogue Cleanup

Deliverables:

- Introduce structured, controller-agnostic route policy fields in AppProfile.
- Deprecate direct nginx annotation maps in profiles.
- Regenerate CRDs and update profile authoring guides.

Primary files:

- `api/v1alpha1/appprofile_types.go`
- `config/crd/gentianos.io_appprofiles.yaml`
- `charts/gentian-os/crds/gentianos.io_appprofiles.yaml`
- `gentian-apps/profiles/*.yaml`
- `gentian-apps/app-profile-guide.md`

Acceptance checks:

- Catalogue profiles validate without nginx-specific keys.
- Operator renders equivalent Gateway policy from typed fields.

### Phase E - Cleanup and Hardening

Deliverables:

- Remove ingress-nginx installation and health checks.
- Remove legacy ingress reconcilers and annotation helpers.
- Update runbooks, troubleshooting, and architecture docs.

Primary files:

- `scripts/install-lib.sh`
- `docs/getting-started.md`
- `docs/FAQ.md`
- `docs/architecture.md`
- `internal/controller/ingress_*.go` (cleanup)

Acceptance checks:

- Fresh install works without ingress-nginx.
- End-to-end test suite green in gateway mode.

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
