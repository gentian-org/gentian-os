#!/usr/bin/env bash
# step: D-03-portal-login
# phase: applications
# requires: D-02-dns-wait
# provides: the kernel realm with its clients and the administrator user, Argo CD and Headlamp signing in against it, the platform tenant Ready and its desktop serving platform.<kernel>
# mutates: Keycloak realm, clients, groups and users; the realm's mail server and the keycloak-smtp-credentials Secret; Secrets in the edge and gitops namespaces; the argocd-cm, argocd-rbac-cm and argocd-tls-certs-cm ConfigMaps (Argo CD's local admin off); the argocd-initial-admin-secret (deleted); the bootstrap Applications (Headlamp's OIDC on); the gentian-portal Application

# _v5_render is B-01's; a step file is a library of verbs and sourcing another
# one is how they are shared.
# shellcheck source=scripts/steps/B-01-bootstrap-apps.sh
source "${SCRIPT_DIR}/scripts/steps/B-01-bootstrap-apps.sh"

# The bootstrap that turns a running Keycloak into one this cluster can be
# signed in to: the kernel realm, the confidential clients Argo CD and Headlamp
# authenticate with, the cluster-roles scope, and the administrator
# account itself -- whose password is derived from the master password, like
# every other kernel credential. Neither the desktop nor the kernel zone's edge
# client is this step's to create: both belong to the platform tenant, which
# adopts this realm (AD-10), and this step waits for them.
#
# The work is portal-login-bootstrap.sh. What this step adds is where each
# piece lives: that library used to name one namespace for Keycloak, OpenFGA,
# the wildcard certificate and Argo CD alike, and under this layout those are
# different answers.

# _d03_newest_chart_build <chart-repo> -- the newest build of a branch among
# a chart's versions, read off the registry's list of them, anonymously, the
# way the cluster pulls.
#
# A release's chart is X.Y.Z; a build of a branch is X.Y.Z-dev.<n>.g<commit>,
# a prerelease of the release it leads up to whose <n> grows with every
# commit (scripts/tools/chart-version.sh). So the highest is the newest, and
# no version ever names a different build than it did. The operator's
# release reconciler takes the same one (internal/controller,
# NewestBuild). No answer is an error: a component that cannot be installed
# is not something to guess at.
_d03_newest_chart_build() {
    local repo="$1"
    local token url headers body tags="" pages=0 newest
    token="$(curl -sf --max-time 20 "https://ghcr.io/token?scope=repository:${repo}:pull&service=ghcr.io" | jq -r '.token // empty')"
    [[ -n "${token}" ]] || { error "could not get a pull token for ${repo} from ghcr.io"; return 1; }
    headers="$(mktemp)"
    url="https://ghcr.io/v2/${repo}/tags/list?n=1000"
    while [[ -n "${url}" ]]; do
        pages=$((pages + 1))
        if (( pages > 200 )); then
            rm -f "${headers}"
            error "chart ${repo} lists more than 200 pages of versions"
            return 1
        fi
        if ! body="$(curl -sf --max-time 20 -D "${headers}" -H @<(printf 'Authorization: Bearer %s\n' "${token}") "${url}")"; then
            rm -f "${headers}"
            error "the versions of chart ${repo} could not be read from ghcr.io"
            return 1
        fi
        tags+="$(jq -r '.tags[]? // empty' <<<"${body}")"$'\n'
        # The next page, when the registry names one: Link: </v2/...>; rel="next".
        url="$(tr -d '\r' < "${headers}" | sed -nE 's/^[Ll]ink: *<([^>]+)> *; *rel="?next"?.*$/\1/p' | head -n 1)"
        [[ "${url}" != /* ]] || url="https://ghcr.io${url}"
    done
    rm -f "${headers}"
    newest="$(_d03_newest_build <<<"${tags}")"
    [[ -n "${newest}" ]] || { error "chart ${repo} has no build of a branch (X.Y.Z-dev.<n>.g<commit>)"; return 1; }
    printf '%s\n' "${newest}"
}

# _d03_newest_build -- of the versions on stdin, one a line, the highest
# build of a branch by semver: the release it leads up to, then its number.
_d03_newest_build() {
    sed -nE 's/^([0-9]+)\.([0-9]+)\.([0-9]+)-dev\.([0-9]+)\.g[0-9a-f]+$/\1 \2 \3 \4 &/p' \
        | sort -k1,1n -k2,2n -k3,3n -k4,4n | tail -n 1 | cut -d' ' -f5
}

# The release index the operator reads (internal/releaseindex DefaultFeed).
_D03_RELEASE_FEED_DEFAULT="https://raw.githubusercontent.com/gentian-org/gentian-os/main/docs/releases/index.yaml"

# _d03_release_index <file> -- the release index, written into <file>, when
# the claim names a release version; returns 0 with nothing written when it
# names a track or nothing.
_d03_release_index() {
    local file="$1" ref
    ref="$(claim_release_ref || true)"
    [[ "${ref}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 0
    curl -sfL --retry 3 --max-time 20 "${GENTIAN_RELEASE_FEED_URL:-${_D03_RELEASE_FEED_DEFAULT}}" -o "${file}" \
        || { error "could not read the release index ${GENTIAN_RELEASE_FEED_URL:-${_D03_RELEASE_FEED_DEFAULT}}"; return 1; }
}

# _d03_chart_version <chart-repo> <index-file> [index-key] -- the version of
# one of the platform's own charts this step renders.
#
# On a cluster whose claim names a release version, the operator's release
# reconciler sets each chart the release's index entry lists to that version
# (internal/controller/release_reconciler.go). Rendering anything else here
# is undone shortly after, and the wait below would then wait for a version
# that never comes back. So a chart the entry lists is taken from it, and
# one of gentian-ui's it does not list is the release's number, which every
# chart of a release is published under. The charts published with the
# operator's image (no index key) are left out on a release: the operator
# chart names them from image.tag. On a cluster that follows a branch each
# is the newest build, which is what the operator takes for a track too.
_d03_chart_version() {
    local repo="$1" index="$2" key="${3:-}" ref listed
    ref="$(gentian_os_ref)"
    if [[ -n "${key}" && -s "${index}" ]]; then
        listed="$(yq_get ".releases[] | select(.version == \"${ref}\") | .ui.${key}" "${index}" || true)"
        if [[ -n "${listed}" ]]; then
            printf '%s\n' "${listed}"
            return 0
        fi
    fi
    if [[ "${ref}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        [[ -z "${key}" ]] || printf '%s\n' "${ref#v}"
        return 0
    fi
    _d03_newest_chart_build "${repo}"
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
    # And the bouncer behind that policy ready, which it is only with the
    # realm's signing keys: the gentian-os Application Healthy. D-01 waits
    # for it Synced only, because before this step there is no realm.
    _v5_delivered "${GITOPS_NAMESPACE}" gentian-os || return "${CHECK_MISSING}"
    # The platform's desktop: a component of the platform tenant, at
    # platform.<kernel> behind the kernel session. Ready means its chart is
    # deployed and its routes exist -- the chart the profile pins, not an
    # earlier one still deployed.
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
    # The kernel realm's mail, where the cluster runs its own. This step
    # derives the realm's mail login and leaves it in keycloak-smtp-credentials,
    # which the operator registers the login from and waits for before it
    # writes a tenant realm's mail server. A cluster installed while this step
    # did not know the mode by its name has neither, and a re-run puts both
    # there. With a relay the Secret is an ExternalSecret's and arrives with
    # the credential, after the install: nothing to look for here.
    if [[ "$(gentian_mail_service_mode)" == "internal" ]]; then
        kubectl get secret keycloak-smtp-credentials -n "${IDENTITY_NAMESPACE}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    fi
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
    local release_index spec name repo key version
    local -A chart_version=()
    release_index="$(mktemp)"
    if ! _d03_release_index "${release_index}"; then
        rm -f "${release_index}"
        return 1
    fi
    # The three from gentian-ui, which a release's index entry lists, and the
    # three published from this repository with the operator's image.
    for spec in \
        "desktop gentian-portal desktop" "admin-console admin-console adminConsole" "concierge concierge concierge" \
        "homesync homesync" "porter porter" "liaison liaison"; do
        read -r name repo key <<<"${spec}"
        version="$(_d03_chart_version "gentian-org/charts/${repo}" "${release_index}" "${key}")" \
            || { rm -f "${release_index}"; return 1; }
        chart_version[${name}]="${version}"
        info "Chart ${repo}: ${version:-of the release, as the operator chart names it}"
    done
    rm -f "${release_index}"
    DESKTOP_CHART_VERSION="${chart_version[desktop]}" \
        ADMIN_CONSOLE_CHART_VERSION="${chart_version[admin-console]}" \
        CONCIERGE_CHART_VERSION="${chart_version[concierge]}" \
        HOMESYNC_CHART_VERSION="${chart_version[homesync]}" \
        PORTER_CHART_VERSION="${chart_version[porter]}" \
        LIAISON_CHART_VERSION="${chart_version[liaison]}" \
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

    # The bouncer answers for every request on those UIs, and it is ready only
    # once it holds the realm's signing keys -- this step made the realm, so
    # this is where the gentian-os Application can first be Healthy.
    info "Waiting for gentian-os to be Healthy (the bouncer, with the kernel realm's keys)..."
    deadline=$((SECONDS + 600))
    until _v5_delivered "${GITOPS_NAMESPACE}" gentian-os; do
        if (( SECONDS > deadline )); then
            error "gentian-os is not Synced and Healthy after 10 minutes:"
            kubectl get application gentian-os -n "${GITOPS_NAMESPACE}" -o jsonpath='{"  sync: "}{.status.sync.status}{"  health: "}{.status.health.status}{" "}{.status.health.message}{"\n"}' 2>/dev/null
            return 1
        fi
        request_argo_sync_if_stalled "${GITOPS_NAMESPACE}" gentian-os 2>/dev/null || true
        sleep 10
    done
    success "The kernel UIs sit behind the kernel zone's session and the ext-auth bouncer."

    # The platform's two UIs: the component reconciler installs each from its
    # profile once the zone exists and its credentials are delivered; the
    # chart comes from the registry the profile names. Both, because waiting
    # for the desktop alone let the install finish while the console's
    # release had been refused, and the first sign was a 500 at the admin console's address.
    _d03_wait_component desktop "${chart_version[desktop]}" "platform.${KERNEL_DOMAIN}" || return 1
    _d03_wait_component admin-console "${chart_version[admin-console]}" "admin.platform.${KERNEL_DOMAIN}" || return 1

    # Said, and not waited for: the App Store app is a tenant's, never the
    # platform tenant's, and only where the cluster offers a store.
    _d03_report_app_store
}

# _d03_report_app_store — one line on whether tenants get the App Store app,
# which comes from the store's catalog (GENTIAN_DEFAULT_PROFILES) and is
# placed while the cluster offers an App Store.
#
# The verdict is the operator's, read where it writes it for the usher: beside
# the tiles (appStore in the gentian-tiles ConfigMap). The operator places the
# app by that same verdict, so this says what the cluster does rather than
# what the installer expects of it. Never a failure and never waited for: a
# cluster with usage statistics off, or whose claim names no store or one
# that is the app's own address, has no App Store app and is complete without
# one.
_d03_report_app_store() {
    local verdict
    verdict="$(kubectl get configmap gentian-tiles -n "${GENTIAN_SYSTEM_NAMESPACE}" \
        -o jsonpath='{.data.tiles\.yaml}' 2>/dev/null | grep -A 2 '^appStore:' || true)"
    case "${verdict}" in
        *"offered: true"*)
            info "App Store app: offered -- every tenant but the platform's gets it from the catalog's app-store-pe profile, at store.<the tenant's domain>, for the people who may install apps." ;;
        *"reason: usage-statistics-disabled"*)
            info "App Store app: not placed -- usage statistics are off on this cluster." ;;
        *"reason: no-store-configured"*)
            info "App Store app: not placed -- claims/cluster.yaml names no App Store (spec.catalogue.storeUrl)." ;;
        *"reason: store-address-is-own-host"*)
            warn "App Store app: not placed -- spec.catalogue.storeUrl in claims/cluster.yaml names the App Store app's own address on this cluster (store.<a tenant's domain>), which is not a store's API." ;;
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
            # still on an earlier chart version, and only the Component was
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
