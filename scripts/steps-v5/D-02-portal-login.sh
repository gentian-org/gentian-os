#!/usr/bin/env bash
# step: D-02-portal-login
# phase: applications
# requires: D-01-operator
# provides: the kernel realm with its clients and the administrator user, Argo CD and Headlamp signing in against it, the platform tenant Ready and its desktop serving console.<kernel>
# mutates: Keycloak realm, clients, groups and users; Secrets in the edge and gitops namespaces; the argocd-cm, argocd-rbac-cm and argocd-tls-certs-cm ConfigMaps; the bootstrap Applications (Headlamp's OIDC on); the gentian-portal Application

# _v5_render is B-01's; a step file is a library of verbs and sourcing another
# one is how they are shared.
# shellcheck source=scripts/steps-v5/B-01-bootstrap-apps.sh
source "${SCRIPT_DIR}/scripts/steps-v5/B-01-bootstrap-apps.sh"

# The bootstrap that turns a running Keycloak into one this cluster can be
# signed in to: the kernel realm, the kernel zone's edge client, the
# confidential clients Argo CD and Headlamp authenticate with, the
# gentian:platform:admin group, and the administrator account itself --
# whose password is derived from the master password, like every other kernel
# credential. The desktop is not this step's to install: it is a component of
# the platform tenant, and this step waits for it.
#
# The work is portal-login-bootstrap.sh. What this step adds is where each
# piece lives: that library used to name one namespace for Keycloak, OpenFGA,
# the wildcard certificate and Argo CD alike, and under this layout those are
# different answers.

# The desktop chart's immutable version for the branch this cluster follows.
#
# The registry holds a moving version per branch and an immutable one per
# build, and the moving chart's appVersion names the immutable one it is a
# copy of. A Helm release under an unchanged version string is never
# upgraded, so what the profile pins is the immutable version; this reads it
# off the moving chart through the registry's OCI API, anonymously, the way
# the cluster pulls it. No answer is an error: a desktop that cannot be
# installed is not something to guess at.
_d02_desktop_chart_version() {
    local repo="gentian-org/charts/gentian-portal"
    local moving="0.1.0-${PORTAL_IMAGE_TAG:-develop}"
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
    # The kernel zone at the edge: the zone client's secret, which this step
    # writes, and the session policy the operator puts on the kernel UIs once
    # it exists. Without the first there is no session; without the second the
    # kernel UIs have no route (never an open one).
    kubectl get secret edge-kernel-oidc -n "${EDGE_NAMESPACE}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    [[ "$(kubectl get securitypolicy sp-kernel-argocd -n "${EDGE_NAMESPACE}" -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}' 2>/dev/null)" == "True" ]] || return "${CHECK_MISSING}"
    # The platform's desktop: a component of the platform tenant, the console
    # at console.<kernel> behind the kernel session. Ready means its chart is
    # deployed and its routes exist -- the chart the profile pins, not an
    # earlier one still deployed under a moving version.
    [[ "$(kubectl get component desktop -n tenant-platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]] || return "${CHECK_MISSING}"
    local pinned deployed
    pinned="$(kubectl get componentprofile desktop -o jsonpath='{.spec.package.chart.version}' 2>/dev/null)"
    deployed="$(kubectl get release tenant-platform-desktop -o jsonpath='{.spec.forProvider.chart.version}' 2>/dev/null)"
    [[ -n "${pinned}" && "${pinned}" == "${deployed}" ]] || return "${CHECK_MISSING}"
    # Headlamp signs in with the client secret its kubeconfig names, and this
    # step is what puts it there. A kubeconfig still carrying the chart's
    # placeholder means the step has not finished its job, whatever else is
    # Ready -- so say so here rather than let a re-run skip it.
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
    local desktop_chart_version
    desktop_chart_version="$(_d02_desktop_chart_version)" || return 1
    info "Desktop chart: ${desktop_chart_version}"
    DESKTOP_CHART_VERSION="${desktop_chart_version}" \
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
    # once the zone secret exists; Envoy Gateway accepts them once the shim's
    # Service and the secret resolve.
    info "Waiting for the kernel zone's session policy on argocd.${KERNEL_DOMAIN}..."
    deadline=$((SECONDS + 600))
    until [[ "$(kubectl get securitypolicy sp-kernel-argocd -n "${EDGE_NAMESPACE}" -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}' 2>/dev/null)" == "True" ]]; do
        if (( SECONDS > deadline )); then
            error "SecurityPolicy sp-kernel-argocd is not Accepted after 10 minutes."
            kubectl get securitypolicy sp-kernel-argocd -n "${EDGE_NAMESPACE}" -o jsonpath='{range .status.ancestors[0].conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || \
                error "  The policy does not exist: is the operator running, and does ${EDGE_NAMESPACE}/edge-kernel-oidc exist?"
            return 1
        fi
        sleep 10
    done
    success "The kernel UIs sit behind the kernel zone's session and the ext-auth shim."

    # The platform desktop: the component reconciler installs it from the
    # desktop profile once the zone exists and the database credential is
    # delivered; its chart comes from the registry the profile names.
    info "Waiting for the platform desktop (Component tenant-platform/desktop) on chart ${desktop_chart_version}..."
    deadline=$((SECONDS + 900))
    until [[ "$(kubectl get release tenant-platform-desktop -o jsonpath='{.spec.forProvider.chart.version}' 2>/dev/null)" == "${desktop_chart_version}" \
        && "$(kubectl get component desktop -n tenant-platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]]; do
        if (( SECONDS > deadline )); then
            error "The platform desktop is not Ready after 15 minutes."
            kubectl get component desktop -n tenant-platform -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || \
                error "  Component tenant-platform/desktop does not exist: is the operator running and the desktop profile installed?"
            return 1
        fi
        sleep 10
    done
    success "The platform desktop serves console.${KERNEL_DOMAIN} behind the kernel zone's session."
}

destroy() {
    _d02_export_layout
    kubectl delete secret keycloak-smtp-credentials -n "${IDENTITY_NAMESPACE}" \
        --ignore-not-found=true >/dev/null 2>&1 || true
}
