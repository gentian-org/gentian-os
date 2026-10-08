#!/usr/bin/env bash
# step: D-03-portal-login
# phase: applications
# requires: D-02-dns-wait
# provides: the kernel realm with its clients and the administrator user, Argo CD and Headlamp signing in against it, the platform tenant Ready and its desktop serving platform.<kernel>
# mutates: Keycloak realm, clients, groups and users; Secrets in the edge and gitops namespaces; the argocd-cm, argocd-rbac-cm and argocd-tls-certs-cm ConfigMaps (Argo CD's local admin off); the argocd-initial-admin-secret (deleted); the bootstrap Applications (Headlamp's OIDC on); the gentian-portal Application

# _v5_render is B-01's; a step file is a library of verbs and sourcing another
# one is how they are shared.
# shellcheck source=scripts/steps/B-01-bootstrap-apps.sh
source "${SCRIPT_DIR}/scripts/steps/B-01-bootstrap-apps.sh"

# The bootstrap that turns a running Keycloak into one this cluster can be
# signed in to: the kernel realm, the confidential clients Argo CD and Headlamp
# authenticate with, the gentian:platform:admin group, and the administrator
# account itself -- whose password is derived from the master password, like
# every other kernel credential. Neither the desktop nor the kernel zone's edge
# client is this step's to create: both belong to the platform tenant, which
# adopts this realm (AD-10), and this step waits for them.
#
# The work is portal-login-bootstrap.sh. What this step adds is where each
# piece lives: that library used to name one namespace for Keycloak, OpenFGA,
# the wildcard certificate and Argo CD alike, and under this layout those are
# different answers.

# A chart's immutable version for the branch this cluster follows.
#
# The registry holds a moving version per branch and an immutable one per
# build, and the moving chart's appVersion names the immutable one it is a
# copy of. A Helm release under an unchanged version string is never
# upgraded, so what a profile pins is the immutable version; this reads it
# off the moving chart through the registry's OCI API, anonymously, the way
# the cluster pulls it. No answer is an error: a component that cannot be
# installed is not something to guess at. The chart and the branch are
# arguments rather than the desktop's by name; the administration console is
# not resolved this way, because gentian-apps publishes its chart at an exact
# version the operator chart pins directly.
_d02_component_chart_version() {
    local repo="$1" moving="$2"
    local token manifest config_digest version
    token="$(curl -sf --max-time 20 "https://ghcr.io/token?scope=repository:${repo}:pull&service=ghcr.io" | jq -r '.token // empty')"
    [[ -n "${token}" ]] || { error "could not get a pull token for ${repo} from ghcr.io"; return 1; }
    manifest="$(curl -sf --max-time 20 -H "Authorization: Bearer ${token}" \
        -H "Accept: application/vnd.oci.image.manifest.v1+json" \
        "https://ghcr.io/v2/${repo}/manifests/${moving}")" \
        || { error "chart ${repo}:${moving} is not published; merge to the branch this cluster follows first"; return 1; }
    config_digest="$(jq -r '.config.digest // empty' <<<"${manifest}")"
    [[ -n "${config_digest}" ]] || { error "chart ${repo}:${moving} has no config blob"; return 1; }
    version="$(curl -sfL --max-time 20 -H "Authorization: Bearer ${token}" \
        "https://ghcr.io/v2/${repo}/blobs/${config_digest}" | jq -r '.appVersion // empty')"
    case "${version}" in
        "${moving}".*) echo "${version}" ;;
        *) error "chart ${repo}:${moving} names no immutable version (appVersion=${version:-none})"; return 1 ;;
    esac
}

_d02_export_layout() {
    export IDENTITY_NAMESPACE AUTHZ_NAMESPACE GITOPS_NAMESPACE \
        EDGE_NAMESPACE GENTIAN_SYSTEM_NAMESPACE CROSSPLANE_NAMESPACE OBSERVABILITY_NAMESPACE
    IDENTITY_NAMESPACE="$(ns_kernel authentication)"
    AUTHZ_NAMESPACE="$(ns_kernel authorization)"
    GITOPS_NAMESPACE="$(ns_kernel gitops)"
    EDGE_NAMESPACE="$(ns_kernel edge)"
    GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)"
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
    OBSERVABILITY_NAMESPACE="$(ns_kernel observability)"
}

check() {
    _d02_export_layout
    # The platform is a tenant whose realm is this one (AD-10). It is
    # scaffolded with the cluster and synced by the tenants ApplicationSet,
    # and it is Ready only once the operator has adopted the realm and its
    # groups exist in it -- which is what this step is for. The operator
    # says so in status.phase; the conditions are the per-function verdicts
    # it derives that from, and none of them is named Ready.
    [[ "$(kubectl get tenant platform -o jsonpath='{.status.phase}' 2>/dev/null)" == "Ready" ]] || return "${CHECK_MISSING}"
    # The kernel zone at the edge: the zone client's secret, which the platform
    # tenant's composition writes and this step waits for, and the session
    # policy the operator puts on the kernel UIs once it exists. Without the
    # first there is no session; without the second the kernel UIs have no
    # route (never an open one).
    kubectl get secret edge-kernel-oidc -n "${EDGE_NAMESPACE}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    [[ "$(kubectl get securitypolicy sp-kernel-argocd -n "${EDGE_NAMESPACE}" -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}' 2>/dev/null)" == "True" ]] || return "${CHECK_MISSING}"
    # The platform's desktop: a component of the platform tenant, at
    # platform.<kernel> behind the kernel session. Ready means its chart is
    # deployed and its routes exist -- the chart the profile pins, not an
    # earlier one still deployed under a moving version.
    [[ "$(kubectl get component desktop -n tenant-platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]] || return "${CHECK_MISSING}"
    local pinned deployed
    pinned="$(kubectl get componentprofile desktop -o jsonpath='{.spec.package.chart.version}' 2>/dev/null)"
    deployed="$(kubectl get release tenant-platform-desktop -o jsonpath='{.spec.forProvider.chart.version}' 2>/dev/null)"
    [[ -n "${pinned}" && "${pinned}" == "${deployed}" ]] || return "${CHECK_MISSING}"
    # And the administration console beside it, on the same terms.
    [[ "$(kubectl get component admin-console -n tenant-platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]] || return "${CHECK_MISSING}"
    pinned="$(kubectl get componentprofile admin-console -o jsonpath='{.spec.package.chart.version}' 2>/dev/null)"
    deployed="$(kubectl get release tenant-platform-admin-console -o jsonpath='{.spec.forProvider.chart.version}' 2>/dev/null)"
    [[ -n "${pinned}" && "${pinned}" == "${deployed}" ]] || return "${CHECK_MISSING}"
    # Headlamp signs in with the client secret its kubeconfig names, and this
    # step is what puts it there. A kubeconfig still carrying the chart's
    # placeholder means the step has not finished its job, whatever else is
    # Ready -- so say so here rather than let a re-run skip it.
    # Argo CD's local admin is off and its generated password gone: the
    # kernel's UIs are signed in to through the realm, with its MFA, and an
    # install from before this rule re-runs the step to get there.
    [[ "$(kubectl get configmap argocd-cm -n "${GITOPS_NAMESPACE}" -o jsonpath='{.data.admin\.enabled}' 2>/dev/null)" == "false" ]] || return "${CHECK_MISSING}"
    ! kubectl get secret argocd-initial-admin-secret -n "${GITOPS_NAMESPACE}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    local kubeconfig
    kubeconfig="$(kubectl get secret headlamp-kubeconfig -n "${OBSERVABILITY_NAMESPACE}" \
        -o jsonpath='{.data.config}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    case "${kubeconfig}" in
    *"${HEADLAMP_KUBECONFIG_PLACEHOLDER:-PLACEHOLDER_REPLACED_BY_THE_INSTALLER}"*)
        return "${CHECK_MISSING}" ;;
    esac
    return 0
}

apply() {
    banner "Kernel realm sign-in"
    _d02_export_layout

    # shellcheck source=scripts/lib/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"

    # The realm has to exist before anything configures its SMTP.
    install_portal_login
    configure_keycloak_realm_smtp || warn "Keycloak realm SMTP configuration skipped."

    # The bootstrap Applications again, with Headlamp's OIDC on: the realm,
    # the headlamp client and its Secret exist now, and the proxy that
    # verifies the person's token comes with the render. Before this step
    # there is no realm to sign in against, which is why a fresh cluster
    # starts on token login. The same render pins the desktop chart.
    local desktop_chart_version admin_console_chart_version concierge_chart_version
    desktop_chart_version="$(_d02_component_chart_version gentian-org/charts/gentian-portal "0.1.0-${PORTAL_IMAGE_TAG:-develop}")" || return 1
    info "Desktop chart: ${desktop_chart_version}"
    # Both from gentian-ui, so both follow its branch.
    admin_console_chart_version="$(_d02_component_chart_version gentian-org/charts/admin-console "0.1.1-${PORTAL_IMAGE_TAG:-develop}")" || return 1
    info "Administration console chart: ${admin_console_chart_version}"
    concierge_chart_version="$(_d02_component_chart_version gentian-org/charts/concierge "0.1.0-${PORTAL_IMAGE_TAG:-develop}")" || return 1
    info "Concierge chart: ${concierge_chart_version}"
    # The App Store app's chart, resolved the same way and pinned by the same
    # render. Unlike the three above it is not something an install fails
    # for: a tenant has the app only where the cluster offers an App Store,
    # and a cluster that does not is a complete one. Unresolved, the profile
    # keeps the version it has (_v5_keep_chart_version), or the branch's
    # moving one on a cluster that never had it.
    local app_store_chart_version=""
    if app_store_chart_version="$(_d02_component_chart_version gentian-org/charts/app-store "0.1.0-${PORTAL_IMAGE_TAG:-develop}" 2>/dev/null)"; then
        info "App Store chart: ${app_store_chart_version}"
    else
        app_store_chart_version=""
        warn "App Store chart 0.1.0-${PORTAL_IMAGE_TAG:-develop} names no immutable version on ghcr.io; its profile keeps the version it has. The install continues."
    fi
    DESKTOP_CHART_VERSION="${desktop_chart_version}" \
        ADMIN_CONSOLE_CHART_VERSION="${admin_console_chart_version}" \
        CONCIERGE_CHART_VERSION="${concierge_chart_version}" \
        APP_STORE_CHART_VERSION="${app_store_chart_version}" \
        V5_APPSETS=true V5_OPERATOR=true V5_HEADLAMP_OIDC=true \
        _v5_render | kubectl apply -f - >/dev/null

    # Only now: that render carries the chart's placeholder where Headlamp's
    # client secret belongs, so anything written before it is overwritten
    # here. This is the last apply of the bootstrap chart in the install, and
    # the fill reads its own work back rather than assuming it stuck.
    ensure_headlamp_kubeconfig || return 1

    # Tenant/platform arrives from git through the tenants ApplicationSet and
    # is provisioned by the operator against the realm just created. Nothing
    # here applies it; what is waited for is the operator's verdict.
    info "Waiting for Tenant/platform to be Ready..."
    local deadline=$((SECONDS + 900))
    until [[ "$(kubectl get tenant platform -o jsonpath='{.status.phase}' 2>/dev/null)" == "Ready" ]]; do
        if (( SECONDS > deadline )); then
            error "Tenant/platform is not Ready after 15 minutes."
            kubectl get tenant platform -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || \
                error "  Tenant/platform does not exist: is clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/tenants/platform pushed to gentian-deployments?"
            return 1
        fi
        request_argo_sync_if_stalled "${GITOPS_NAMESPACE}" "tenant-platform" 2>/dev/null || true
        sleep 10
    done
    success "Tenant/platform is Ready: the platform tenant adopts realm ${KERNEL_REALM:-kernel}."

    # The zone's session on the kernel UIs. The operator writes the policies
    # once the zone secret exists; Envoy Gateway accepts them once the bouncer's
    # Service and the secret resolve.
    info "Waiting for the kernel zone's session policy on argocd.${KERNEL_DOMAIN}..."
    deadline=$((SECONDS + 600))
    until [[ "$(kubectl get securitypolicy sp-kernel-argocd -n "${EDGE_NAMESPACE}" -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}' 2>/dev/null)" == "True" ]]; do
        if (( SECONDS > deadline )); then
            error "SecurityPolicy sp-kernel-argocd is not Accepted after 10 minutes."
            kubectl get securitypolicy sp-kernel-argocd -n "${EDGE_NAMESPACE}" -o jsonpath='{range .status.ancestors[0].conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || \
                error "  The policy does not exist: is the operator running, and did Tenant/platform compose ${EDGE_NAMESPACE}/edge-kernel-oidc?"
            return 1
        fi
        sleep 10
    done
    success "The kernel UIs sit behind the kernel zone's session and the ext-auth bouncer."

    # The platform's two UIs: the component reconciler installs each from its
    # profile once the zone exists and its credentials are delivered; the
    # chart comes from the registry the profile names. Both, because waiting
    # for the desktop alone let the install finish while the console's
    # release had been refused, and the first sign was a 500 at the admin console's address.
    _d03_wait_component desktop "${desktop_chart_version}" "platform.${KERNEL_DOMAIN}" || return 1
    _d03_wait_component admin-console "${admin_console_chart_version}" "admin.platform.${KERNEL_DOMAIN}" || return 1

    # Said, and not waited for: the App Store app is a tenant's, never the
    # platform tenant's, and only where the cluster offers a store.
    _d03_report_app_store
}

# _d03_report_app_store — one line on whether tenants get the App Store app.
#
# The verdict is the operator's, read where it writes it for the usher: beside
# the tiles (appStore in the gentian-tiles ConfigMap). The operator places the
# app by that same verdict, so this says what the cluster does rather than
# what the installer expects of it. Never a failure and never waited for: a
# cluster with licence reporting off, or whose claim names no store, has no
# App Store app and is complete without one.
_d03_report_app_store() {
    local verdict
    verdict="$(kubectl get configmap gentian-tiles -n "${GENTIAN_SYSTEM_NAMESPACE}" \
        -o jsonpath='{.data.tiles\.yaml}' 2>/dev/null | grep -A 2 '^appStore:' || true)"
    case "${verdict}" in
        *"offered: true"*)
            info "App Store app: placed on every tenant but the platform's, at store.<the tenant's domain>, for the people who may install apps." ;;
        *"reason: licence-report-disabled"*)
            info "App Store app: not placed -- licence reporting is off on this cluster." ;;
        *"reason: no-store-configured"*)
            info "App Store app: not placed -- claims/cluster.yaml names no App Store (spec.catalogue.storeUrl)." ;;
        *)
            info "App Store app: the operator has not said yet whether this cluster offers an App Store; it is placed once it does." ;;
    esac
    return 0
}

# _d03_wait_component <component> <chart-version> <host> — a platform-tenant
# component Ready on the chart the profile pins.
_d03_wait_component() {
    local name="$1" want="$2" host="$3" deadline have_version
    info "Waiting for Component tenant-platform/${name} on chart ${want}..."
    deadline=$((SECONDS + 900))
    until [[ "$(kubectl get release "tenant-platform-${name}" -o jsonpath='{.spec.forProvider.chart.version}' 2>/dev/null)" == "${want}" \
        && "$(kubectl get component "${name}" -n tenant-platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]]; do
        if (( SECONDS > deadline )); then
            error "Component tenant-platform/${name} is not Ready after 15 minutes."
            # Both halves of the condition, because reporting one of two is
            # how this came to print "not Ready" directly above a line
            # reading Ready=True: the Component was Ready and the Release was
            # still on the moving chart version, and only the Component was
            # shown.
            have_version="$(kubectl get release "tenant-platform-${name}" -o jsonpath='{.spec.forProvider.chart.version}' 2>/dev/null)"
            if [[ "${have_version}" != "${want}" ]]; then
                error "  chart: release is on ${have_version:-<no release>}, waiting for ${want}"
                kubectl get release "tenant-platform-${name}" -o jsonpath='{range .status.conditions[*]}  release {.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || true
            fi
            kubectl get component "${name}" -n tenant-platform -o jsonpath='{range .status.conditions[*]}  component {.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || \
                error "  Component tenant-platform/${name} does not exist: is the operator running and the ${name} profile installed?"
            # A Component stuck terminating cannot be repaired or removed by
            # anything the installer does, and it is invisible in the
            # conditions above.
            if [[ -n "$(kubectl get component "${name}" -n tenant-platform -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null)" ]]; then
                error "  the Component is terminating and its finalizer is not clearing; the operator cannot reconcile it"
            fi
            return 1
        fi
        sleep 10
    done
    success "${name} serves ${host} behind the kernel zone's session."
}

destroy() {
    _d02_export_layout
    kubectl delete secret keycloak-smtp-credentials -n "${IDENTITY_NAMESPACE}" \
        --ignore-not-found=true >/dev/null 2>&1 || true
}
