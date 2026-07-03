#!/usr/bin/env bash
# =============================================================================
# scripts/install-lib.sh — Legacy install entry + backward-compatible shim
# =============================================================================
# New callers should source scripts/lib/load.sh directly (install.sh,
# update.sh, uninstall.sh). This file remains so existing references keep
# working and so `./scripts/install-lib.sh` still runs the legacy bootstrap.
#
# Set GENTIAN_INSTALL_LIB_ONLY=1 before sourcing to load helpers without
# running main().
# =============================================================================

set -euo pipefail

__GENTIAN_INSTALL_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/load.sh
source "${__GENTIAN_INSTALL_LIB_DIR}/lib/load.sh"

# =============================================================================
# Main (legacy standalone bootstrap — prefer ./install.sh for Crossplane flow)
# =============================================================================
main() {
    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║     Gentian OS — Fresh Cluster Bootstrap                 ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
    echo ""

    parse_args "$@"
    if [[ "${INSTALL_VERIFY_ONLY}" == "1" ]]; then
        verify_argocd_apps || true
        print_summary
        return 0
    fi
    load_operator_config
    load_creds_cache
    [[ "${INSTALL_VALIDATE_ONLY}" == "1" ]] && { load_install_state; load_deployments_cluster_settings; try_load_creds_from_openbao; validate_config; }
    load_install_state
    load_deployments_cluster_settings
    try_load_creds_from_openbao
    prompt_credentials
    prompt_app_repos
    prompt_kernel_domain
    prompt_network_mode
    prompt_kernel_secrets
    check_prereqs
    install_tools
    create_namespaces
    prewarm_cluster
    install_cert_manager
    install_kernel_cert_resources
    install_envoy_gateway
    install_eso
    install_argocd
    setup_argocd_repos
    install_argocd_image_updater
    bootstrap_transit_app
    init_openbao_transit
    bootstrap_argocd_apps
    init_openbao
    bao_bootstrap
    seed_secrets
    install_kernel_wildcard
    bootstrap_root_appset
    install_app_catalogue
    install_appprofiles_sync
    install_orchestrator
    verify_argocd_apps || true
    print_summary
}

# Allow this file to be sourced as a function library without running main().
[[ "${GENTIAN_INSTALL_LIB_ONLY:-0}" == "1" ]] || main "$@"
