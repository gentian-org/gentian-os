#!/usr/bin/env bash
# =============================================================================
# scripts/lib/catalogue.sh — App catalogue sync, operator Helm bootstrap, and orchestrator handoff.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# =============================================================================
# 16. AppCatalogue CRD + kubectl-gentian plugin
# =============================================================================
install_app_catalogue() {
    banner "AppCatalogue CRD"

    # The CRD is not applied here.
    #
    # It used to be, from config/crd/ in this checkout — a file byte-identical to
    # charts/gentian-os/crds/gentianos.io_appcatalogues.yaml, which the
    # gentian-os Argo CD Application delivers with the operator chart at
    # sync-wave 0. Two writers, one of them whatever tree the installer happened
    # to run from.
    #
    # Nothing local populates the catalogue either: ComponentProfiles arrive from
    # gentian-apps through the gentian-catalogue ApplicationSet, and the
    # operator's appstore controller creates the AppCatalogue singleton and
    # rebuilds its status from them. So this step waits for what Argo brings.
    local deadline=$((SECONDS + 120))
    until kubectl get crd appcatalogues.gentianos.io >/dev/null 2>&1; do
        if (( SECONDS > deadline )); then
            warn "AppCatalogue CRD not present after 2m — check the gentian-os Application in Argo CD."
            return 0
        fi
        sleep 5
    done
    success "AppCatalogue CRD is present (delivered by the operator chart)."

    # The host CLI is NOT installed from here. Installing it meant a cluster
    # installer asking for root on the machine it was run from, and an uninstall
    # deleting the binary that drives every other cluster the operator manages.
    # It is a host operation, so it has a host command.
    report_gentian_cli_state
}

# report_gentian_cli_state — is the CLI present, and is it THIS checkout's?
#
# Presence alone was the whole check, and presence is the easy half. The plugin
# is a script copied into place, so a machine accumulates copies: one in
# ~/.local/bin from `make install-plugin`, an older root-owned one in
# /usr/local/bin from when the installer still put it there, and the checkout's
# own. PATH order decides which answers, nothing announces the others, and a
# `gtnctl tenants deploy` can therefore run a build that predates the cluster it
# is talking to — silently, because an out-of-date CLI does not fail, it just
# does something slightly different.
#
# Compared by content rather than by version string: two copies can declare the
# same version and differ, which is exactly what a copied script does between
# releases.
report_gentian_cli_state() {
    local repo="${SCRIPT_DIR}/scripts/kubectl-gentian"
    local installed
    installed="$(command -v kubectl-gentian 2>/dev/null || true)"

    if [[ -z "${installed}" ]]; then
        info "The gentian CLI is not on PATH. Install it with:"
        info "  make -C ${SCRIPT_DIR} install-plugin"
        return 0
    fi

    [[ -r "${repo}" ]] || return 0
    if cmp -s "${repo}" "${installed}"; then
        return 0
    fi

    warn "The gentian CLI on PATH is not this checkout's copy:"
    warn "    on PATH : ${installed}"
    warn "    checkout: ${repo}"
    warn "  Refresh it with:  make -C ${SCRIPT_DIR} install-plugin"

    # Every other copy, because the stale one is only a PATH change away from
    # being the one that runs.
    local other seen=0
    while IFS= read -r other; do
        [[ -n "${other}" && "${other}" != "${installed}" ]] || continue
        (( seen++ == 0 )) && warn "  Other copies on PATH, any of which could take over:"
        warn "    ${other}"
    done < <(type -aP kubectl-gentian 2>/dev/null || true)
}

# Every host path install_app_catalogue writes: the plugin and its gtnctl
# symlink, in /usr/local/bin and in the ~/.local/bin mirror.
#
# Both destinations are real. ~/.local/bin usually precedes /usr/local/bin in
# PATH, so removing only the system copy leaves `gtnctl` still resolving to a
# plugin for a cluster that no longer exists.

# =============================================================================
# 15. Repository claim for the default app catalogue (gentian-apps)
# =============================================================================
# The Repository claim itself is B-12-apps-repository.sh's job now (it needs
# to run in the secrets phase, before D, to carry the OpenBao/ESO credential
# for a private repo — this file's install_catalogue_sync used to apply a
# second, credential-less Repository/gentian-apps here, which duplicated it:
# any role: apps, type: git repository composes its own catalogue-sync
# ApplicationSet (crossplane/compositions/repository-default.yaml) named after
# the claim, so two claims for the same repo meant two ApplicationSets
# fighting over the same Applications. D-08-componentprofiles.sh just verifies
# B-12's claim exists now.
#
# Once synced, each profiles/<name>/ bundle becomes an Application
# (catalogue-<name>) that applies AppProfile, optional composition.yaml, and
# optional cluster assets.

# =============================================================================
# 15. Install gentian-os orchestrator (Helm chart + ArgoCD Application)
# =============================================================================
# The orchestrator chart at charts/gentian-os/ ships:
#   - CRDs: tenants, componentprofiles, integrationbindings
#   - Deployment + ServiceAccount + ClusterRole(Binding) for the operator
#   - ServiceMonitor + Grafana dashboard
#
# Two-step install:
#   Direct Helm bootstrap (fast):
#     CRDs and the operator Deployment are applied immediately so that
#     subsequent install steps can use them without waiting for ArgoCD.
#   ArgoCD Application handoff:
#     The gentian-os ArgoCD Application (rendered from
#     kernel/bootstrap/chart/templates/gentian-os.yaml) is applied.
#     ArgoCD takes ownership of the resources via ServerSideApply and from
#     this point drives all future chart upgrades.  Critically, Source 4 of
#     the Application deploys the ImageUpdater CR into the cluster, which
#     activates argocd-image-updater's automatic image rollout: whenever a
#     new image is pushed to GHCR for the tracked branch, argocd-image-updater
#     patches image.tag in the Application's Helm parameters and ArgoCD
#     triggers a Helm upgrade (rolling restart) automatically.
#
# Without the ArgoCD handoff, argocd-image-updater reports "no ImageUpdater CRs to
# process" and image updates require manual kubectl rollout restart.
# =============================================================================
release_gentian_os_helm_bootstrap() {
    local ns="${1:-gentian-system}"
    if kubectl get secret -n "$ns" -l "owner=helm,name=gentian-os" --no-headers 2>/dev/null | grep -q .; then
        info "Removing bootstrap Helm release metadata (ArgoCD owns gentian-os now)..."
        kubectl delete secret -n "$ns" -l "owner=helm,name=gentian-os" --ignore-not-found
    fi
}

# The operator's .git-credentials used to be created here with `kubectl create
# secret`, alongside the one the XRepository Composition emits through ESO —
# two writers of one credential, and the values file decided which the operator
# mounted. The composed one is the credential: it is backed by an
# ExternalSecret, so rotating the value in OpenBao reaches the pod, which the
# imperative Secret could never do.
#
# It survived this long because the Composition named it from the composite
# (deployments-m288c-git-credentials), and a chart value cannot be written
# against a generated suffix. The Composition now names it from the claim, so
# `deployments-git-credentials` is stable and referenceable.

# Adopt cluster-scoped chart resources left from a prior ArgoCD or manual install so
# helm upgrade --install gentian-os can proceed (missing meta.helm.sh/release-*).
# =============================================================================
# ensure_kernel_services_configmap — break the Step 12 / Step 13 deadlock
#
# The Keycloak pod created by the Suze XR (Step 12) reads KERNEL_DOMAIN from the
# gentian-kernel-services ConfigMap in platform-kernel. That ConfigMap is
# rendered by the gentian-os operator chart — Step 13. So on a first install the
# pod fails with
#
#   Error: configmap "gentian-kernel-services" not found
#
# and Step 12 waits out its full 1200s timeout for a pod that cannot start,
# while the thing it needs is scheduled to arrive one step later. Re-runs of an
# already-bootstrapped cluster hide this, because the ConfigMap is left over
# from the previous run — which is why it survived until the first prod install.
#
# Seed it before Step 12 with the keys that are unambiguous this early
# (KERNEL_DOMAIN, TENANCY_MODE — the only ones any Step 12 workload reads), and
# tag it so Helm adopts rather than collides at Step 13, using the same
# annotation/label pair as adopt_gentian_os_helm_preflight below. Step 13 then
# re-renders it with the full key set (SMTP_HOST, S3_ENDPOINT, MYSQL_HOST, …),
# which is deliberately NOT guessed here: those hostnames come from chart
# defaults that this function has no business duplicating.
# =============================================================================
ensure_kernel_services_configmap() {
    local ns="platform-kernel"

    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        warn "KERNEL_DOMAIN unset; skipping gentian-kernel-services pre-seed."
        return 0
    fi

    kubectl get namespace "${ns}" >/dev/null 2>&1 || kubectl create namespace "${ns}" >/dev/null

    if kubectl get configmap gentian-kernel-services -n "${ns}" >/dev/null 2>&1; then
        success "gentian-kernel-services already present in ${ns}."
        return 0
    fi

    info "Pre-seeding gentian-kernel-services in ${ns} (needed by Keycloak in Step 12)..."
    kubectl create configmap gentian-kernel-services -n "${ns}" \
        --from-literal=KERNEL_DOMAIN="${KERNEL_DOMAIN}" \
        --from-literal=TENANCY_MODE="${TENANCY_MODE:-multi}" >/dev/null

    # Same adoption contract as adopt_gentian_os_helm_preflight: without these,
    # Step 13's `helm upgrade --install` aborts with "invalid ownership metadata".
    kubectl annotate configmap gentian-kernel-services -n "${ns}" \
        "meta.helm.sh/release-name=gentian-os" \
        "meta.helm.sh/release-namespace=gentian-system" --overwrite >/dev/null
    kubectl label configmap gentian-kernel-services -n "${ns}" \
        "app.kubernetes.io/managed-by=Helm" \
        "gentianos.io/config-type=kernel-services" --overwrite >/dev/null

    success "gentian-kernel-services seeded (Helm will adopt it in Step 13)."
}

adopt_gentian_os_helm_preflight() {
    local ns="${1:-gentian-system}"
    local vwc chart_ns

    while IFS= read -r vwc; do
        [[ -z "$vwc" ]] && continue
        if kubectl get validatingwebhookconfiguration "$vwc" \
            -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null \
            | grep -q .; then
            continue
        fi
        kubectl annotate validatingwebhookconfiguration "$vwc" \
            "meta.helm.sh/release-name=gentian-os" \
            "meta.helm.sh/release-namespace=${ns}" \
            --overwrite
        kubectl label validatingwebhookconfiguration "$vwc" \
            "app.kubernetes.io/managed-by=Helm" \
            --overwrite
        info "Adopted pre-existing ValidatingWebhookConfiguration '${vwc}' into Helm release."
    done < <(kubectl get validatingwebhookconfigurations \
                 --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null \
             | grep "^gentian-os-" || true)

    # The cluster-scoped RBAC, for the same reason and by the same mechanism.
    #
    # Once Argo CD has reconciled a cluster, the gentian-os Application renders
    # THIS chart and applies its output directly. Argo's copies therefore carry
    # the chart's app.kubernetes.io/managed-by=Helm label — which looks like
    # ownership and is not — but none of the meta.helm.sh/release-*
    # annotations, which is what Helm actually checks. So a local
    # `helm upgrade --install` refuses them:
    #
    #   ClusterRole "gentian-os" in namespace "" exists and cannot be imported
    #   into the current release: invalid ownership metadata
    #
    # and D-01 stops on a cluster where nothing is wrong except which client
    # applied the object last. The webhook above had already been given this
    # treatment; the RBAC had not, so an install after any Argo sync hit it.
    #
    # Namespaced objects do not need this: they are recreated in a namespace
    # the chart owns. Cluster-scoped ones outlive it.
    #
    # Every cluster-scoped kind the chart can render, not a list of names.
    # Fixing these one at a time is a losing game — the RBAC was found first,
    # then OIDCPackCatalog on the next run, then PlatformSecurityPolicy would
    # have been after that. The core kinds are fixed; the gentianos.io ones are
    # discovered, so a cluster-scoped CR added to the chart later is covered
    # without touching this.
    #
    # Selected by app.kubernetes.io/instance=gentian-os, which the chart stamps
    # on what it renders. That is what keeps this from adopting objects it has
    # no business claiming: a Tenant is cluster-scoped too, and carries no such
    # label because nothing in this chart renders it.
    local kinds=(clusterrole clusterrolebinding validatingwebhookconfiguration)
    local crd
    while IFS= read -r crd; do
        [[ -n "${crd}" ]] && kinds+=("${crd}")
    done < <(kubectl get crd -o json 2>/dev/null | python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for c in doc.get("items", []):
    spec = c.get("spec", {})
    if spec.get("group") == "gentianos.io" and spec.get("scope") == "Cluster":
        print(spec["names"]["plural"] + ".gentianos.io")
' || true)

    local kind obj existing
    for kind in "${kinds[@]}"; do
        while IFS= read -r obj; do
            [[ -n "${obj}" ]] || continue
            existing="$(kubectl get "${obj}" \
                -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' \
                2>/dev/null || true)"
            [[ -n "${existing}" ]] && continue
            kubectl annotate "${obj}" \
                "meta.helm.sh/release-name=gentian-os" \
                "meta.helm.sh/release-namespace=${ns}" \
                --overwrite >/dev/null
            kubectl label "${obj}" \
                "app.kubernetes.io/managed-by=Helm" \
                --overwrite >/dev/null
            info "Adopted pre-existing ${obj} into Helm release."
        done < <(kubectl get "${kind}" \
                     -l app.kubernetes.io/instance=gentian-os \
                     -o name 2>/dev/null || true)
    done

    chart_ns="shared-apps"
    if kubectl get namespace "${chart_ns}" >/dev/null 2>&1; then
        if ! kubectl get namespace "${chart_ns}" \
            -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null \
            | grep -q .; then
            kubectl annotate namespace "${chart_ns}" \
                "meta.helm.sh/release-name=gentian-os" \
                "meta.helm.sh/release-namespace=${ns}" \
                --overwrite
            kubectl label namespace "${chart_ns}" \
                "app.kubernetes.io/managed-by=Helm" \
                --overwrite
            info "Adopted pre-existing namespace '${chart_ns}' into Helm release."
        fi
    fi
}

_gentian_os_deployments_kernel_dir() {
    : "${GENTIAN_DEPLOYMENTS_PATH:=${HOME}/.gentian/gentian-deployments}"
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-default-cluster}"
    echo "${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel"
}

_gentian_os_collect_operator_value_files() {
    local -n _files=$1
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-${ENV:-dev}}"
    local kernel_dir deploy_dir
    kernel_dir="$(_gentian_os_deployments_kernel_dir)"
    deploy_dir="${GENTIAN_DEPLOYMENTS_PATH:-${HOME}/.gentian/gentian-deployments}"
    _files=()
    # Mirrors the layered valueFiles ArgoCD uses once it takes over this
    # Application (kernel/bootstrap/chart/templates/gentian-os.yaml):
    # profiles/_base.yaml -> profiles/<stage>.yaml -> clusters/<cluster>/kernel/values.yaml
    if [[ -f "${deploy_dir}/profiles/_base.yaml" ]]; then
        _files+=(-f "${deploy_dir}/profiles/_base.yaml")
    else
        warn "Missing ${deploy_dir}/profiles/_base.yaml — operator shared defaults not applied."
    fi
    if [[ -f "${deploy_dir}/profiles/${stage}.yaml" ]]; then
        _files+=(-f "${deploy_dir}/profiles/${stage}.yaml")
        info "Layering operator Helm values from gentian-deployments (profiles/${stage}.yaml)."
    else
        warn "Missing ${deploy_dir}/profiles/${stage}.yaml — operator stage overlay not applied."
    fi
    if [[ -f "${kernel_dir}/values.yaml" ]]; then
        _files+=(-f "${kernel_dir}/values.yaml")
        info "Layering operator Helm values from gentian-deployments (clusters/.../kernel/values.yaml)."
    else
        warn "Missing ${kernel_dir}/values.yaml — operator Cloudflare/DNS settings may be incomplete."
    fi
}

_gentian_os_services_namespace() {
    local ns
    ns=$(kubectl get deploy gentian-os -n gentian-system \
        -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="SERVICES_NAMESPACE")].value}' 2>/dev/null || true)
    if [[ -n "$ns" ]]; then
        echo "$ns"
        return
    fi
    # Fallback must match the chart default, not the old gentian-<env> guess.
    gentian_services_namespace
}

wait_for_operator_cloudflare_token() {
    local ns="${1:-gentian-system}"
    local secret_name="${2:-cloudflare-api-token-gentian-system}"
    if ! kubectl get externalsecret "${secret_name}" -n "${ns}" >/dev/null 2>&1; then
        info "No operator Cloudflare ExternalSecret in ${ns}; skipping API token wait."
        return 0
    fi
    info "Waiting for operator Cloudflare API token Secret ${secret_name} (max 120s)..."
    local i
    for (( i=1; i<=60; i++ )); do
        if kubectl get secret "${secret_name}" -n "${ns}" >/dev/null 2>&1; then
            success "Operator Cloudflare API token ready after $((i * 2))s."
            return 0
        fi
        sleep 2
    done
    warn "Secret ${secret_name} did not materialize within 120s."
    warn "  kubectl describe externalsecret ${secret_name} -n ${ns}"
    warn "  Ensure CF_API_TOKEN was seeded (Step 10c) and OpenBao path gentian-os/kernel/dns/cloudflare exists."
    return 1
}
