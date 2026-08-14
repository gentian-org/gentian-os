#!/usr/bin/env bash
# =============================================================================
# install.sh — Gentian OS bootstrap driver
# =============================================================================
#
# This script does not install anything itself. It discovers the step files in
# scripts/steps/, reports what each one found before changing anything, and runs
# them.
# Every step is a self-contained file you can read top to bottom:
#
#   scripts/steps/A-01-crossplane.sh   scripts/steps/B-07-cluster-xr.sh
#   scripts/steps/A-08-eso.sh          scripts/steps/C-02-root-appset.sh
#   scripts/steps/A-09-argocd.sh       ...
#
# Each declares a contract in its header and implements up to three verbs:
#
#   check()    read-only — is this already done?
#   apply()    make it so
#   destroy()  reverse it
#
# One driver, three directions. Update is not a separate program: converging a
# running cluster IS the update, so it is the same forward pass.
#
#   ./install.sh                    install or converge
#   ./install.sh --update           same thing, named for what you meant
#   ./install.sh --uninstall        reverse order, destroy() each step
#
# Read before you run:
#
#   ./install.sh --explain          the step table and what each one mutates
#   ./install.sh --dry-run          run every check(), print the plan, change nothing
#   ./install.sh --status           where did this install get to?
#
# Run part of it:
#
#   ./install.sh --step 16          just that one
#   ./install.sh --from 19          resume from there
#   ./install.sh --only 07,08       a named subset
#   ./install.sh --skip 03          everything but that
#   ./install.sh --phase secrets    one phase: control-plane, secrets, platform,
#                                   applications, handover
#
# There is no install-state file. State is read from the cluster by each step's
# check(), which is what makes a re-run converge instead of restart.
#
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=scripts/lib/versions.sh
source "${SCRIPT_DIR}/scripts/lib/versions.sh"
# shellcheck source=scripts/lib/load.sh
source "${SCRIPT_DIR}/scripts/lib/load.sh"
# shellcheck source=scripts/lib/driver.sh
source "${SCRIPT_DIR}/scripts/lib/driver.sh"

# ─── Crossplane ───────────────────────────────────────────────────────────────
# Exported because the step bodies that read them live in scripts/lib/, not here.
#
# Versions and chart repos come from versions.yaml, never from a config file: two
# clusters on the same gentian-os release must run the same Crossplane, and an
# operator-settable version selects an untested combination. The namespace is a
# constant — references to it are hardcoded across the repo, so presenting it as
# a knob would invite someone to turn it.
export CROSSPLANE_NAMESPACE=crossplane-system
CROSSPLANE_VERSION="$(gentian_pin crossplane chart)"
CROSSPLANE_HELM_REPO="$(gentian_pin crossplane repo)"
export CROSSPLANE_VERSION CROSSPLANE_HELM_REPO

# How long to wait, not what to install — a function of how slow the target is,
# so it stays an env override rather than becoming configuration.
export PROVIDER_WAIT_TIMEOUT="${PROVIDER_WAIT_TIMEOUT:-15m}"
export CLUSTER_XR_TIMEOUT="${CLUSTER_XR_TIMEOUT:-15m}"

# ─── OpenBao CLI — auto-install if missing ────────────────────────────────────
# Installed to ~/.local/bin so no sudo is required.
OPENBAO_CLI_VERSION="${OPENBAO_CLI_VERSION:-$(gentian_pin openbao cli)}"

GENTIAN_DIRECTION="forward"
GENTIAN_KIT_PATH=""
GENTIAN_RECOVER_FROM=""

driver_usage() {
    sed -n '3,42p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    cat <<'EOF'

Recovery:
  --export-recovery-kit [PATH]  write the bootstrap material (master password,
                                derivation salt, unseal keys, cluster identity)
                                to an encrypted kit; default gentian-recovery-kit.age
  --recover PATH                load a kit before installing, so derived
                                credentials reproduce their original values

Other options:
  --validate            validate config and step contracts, no cluster changes
  --verify-only         run post-install verification and print the summary
  --no-cluster-infra    skip cert-manager / CNPG / reloader
  --config-file PATH    override install.env
  --secrets-file PATH   override the secrets file
  -h, --help            this message
EOF
}

# =============================================================================
# Arguments
# =============================================================================
parse_driver_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --update)            GENTIAN_DIRECTION="forward" ;;
            --uninstall|--destroy) GENTIAN_DIRECTION="reverse" ;;
            --explain)           GENTIAN_EXPLAIN=1 ;;
            --status)            GENTIAN_DIRECTION="status" ;;
            --dry-run)           GENTIAN_DRY_RUN=1 ;;
            --step|--only)
                shift; [[ $# -gt 0 ]] || { error "$0: --only requires a value"; exit 1; }
                GENTIAN_ONLY="$1" ;;
            --from)
                shift; [[ $# -gt 0 ]] || { error "$0: --from requires a value"; exit 1; }
                GENTIAN_FROM="$1" ;;
            --until)
                shift; [[ $# -gt 0 ]] || { error "$0: --until requires a value"; exit 1; }
                GENTIAN_UNTIL="$1" ;;
            --skip)
                shift; [[ $# -gt 0 ]] || { error "$0: --skip requires a value"; exit 1; }
                GENTIAN_SKIP="$1" ;;
            --phase)
                shift; [[ $# -gt 0 ]] || { error "$0: --phase requires a value"; exit 1; }
                GENTIAN_PHASE="$1" ;;
            --export-recovery-kit)
                GENTIAN_DIRECTION="export-kit"
                if [[ -z "${2:-}" || "${2:-}" == -* ]]; then GENTIAN_KIT_PATH=""
                else shift; GENTIAN_KIT_PATH="$1"; fi ;;
            --recover)
                shift; [[ $# -gt 0 ]] || { error "$0: --recover requires a kit path"; exit 1; }
                GENTIAN_RECOVER_FROM="$1" ;;
            --no-cluster-infra)  INSTALL_CLUSTER_INFRA="0" ;;
            --cluster-infra)     INSTALL_CLUSTER_INFRA="1" ;;
            --config-file)
                shift; [[ $# -gt 0 ]] || { error "--config-file requires a value"; exit 1; }
                INSTALL_CONFIG_FILE="$1" ;;
            --secrets-file)
                shift; [[ $# -gt 0 ]] || { error "--secrets-file requires a value"; exit 1; }
                INSTALL_SECRETS_FILE="$1" ;;
            --no-config-files)   INSTALL_AUTO_LOAD_CONFIG="0" ;;
            --verify-only)       INSTALL_VERIFY_ONLY="1" ;;
            --validate|--check)  INSTALL_VALIDATE_ONLY="1" ;;
            -h|--help)           driver_usage; exit 0 ;;
            *)
                error "Unknown option: $1"
                driver_usage
                exit 1 ;;
        esac
        shift
    done
    export GENTIAN_DRY_RUN GENTIAN_ONLY GENTIAN_FROM GENTIAN_UNTIL GENTIAN_SKIP GENTIAN_PHASE
    export INSTALL_CLUSTER_INFRA INSTALL_CONFIG_FILE INSTALL_SECRETS_FILE
    export INSTALL_AUTO_LOAD_CONFIG INSTALL_VERIFY_ONLY INSTALL_VALIDATE_ONLY
}

# =============================================================================
# Configuration and credentials
#
# Everything before the first step runs: read config, collect credentials,
# validate. No cluster mutation happens here — aborting is free right up to the
# first apply().
# =============================================================================
prepare_run() {
    # Before config, so the kit supplies what install.env would otherwise have
    # to, and before the credential prompt, so nothing is asked for twice.
    if [[ -n "${GENTIAN_RECOVER_FROM}" ]]; then
        load_recovery_kit "${GENTIAN_RECOVER_FROM}" || exit 1
    fi

    load_operator_config
    load_deployments_cluster_settings
    try_load_creds_from_openbao

    [[ "${INSTALL_VALIDATE_ONLY:-0}" == "1" ]] && validate_config

    prompt_app_repos
    resolve_kernel_domain_from_claim   # already-bootstrapped cluster reads its Claim
    prompt_kernel_domain
    prompt_network_mode

    # One pass over credentials.yaml, replacing prompt_credentials and
    # prompt_kernel_secrets. Validation runs here, before the first apply(), so
    # a bad credential aborts with the cluster untouched.
    collect_bootstrap_credentials

    CROSSPLANE_MODE=1 check_prereqs
    _ensure_bao
    scaffold_cluster_deployment        # new cluster only — no-op if scaffolded
}

# _ensure_bao — install the OpenBao CLI to ~/.local/bin when absent.
_ensure_bao() {
    if command -v bao >/dev/null 2>&1; then
        return 0
    fi
    local _os _arch _archive _install_dir
    _os=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$(uname -m)" in
        x86_64|amd64) _arch=amd64 ;;
        aarch64|arm64) _arch=arm64 ;;
        *) error "Unsupported architecture for the OpenBao CLI: $(uname -m)"; return 1 ;;
    esac
    _install_dir="${HOME}/.local/bin"
    mkdir -p "${_install_dir}"
    _archive="$(mktemp -d)/bao.tar.gz"
    info "Installing the OpenBao CLI ${OPENBAO_CLI_VERSION} to ${_install_dir}..."
    curl -fsSL --connect-timeout 30 --max-time 300 \
        "https://github.com/openbao/openbao/releases/download/${OPENBAO_CLI_VERSION}/bao_${OPENBAO_CLI_VERSION#v}_${_os}_${_arch}.tar.gz" \
        -o "${_archive}"
    tar -xzf "${_archive}" -C "${_install_dir}" bao
    chmod +x "${_install_dir}/bao"
    rm -rf "$(dirname "${_archive}")"
    export PATH="${_install_dir}:${PATH}"
    command -v bao >/dev/null 2>&1 || { error "OpenBao CLI install failed."; return 1; }
    success "OpenBao CLI installed."
}

# =============================================================================
# Main
# =============================================================================
main() {
    parse_driver_args "$@"

    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║     Gentian OS — Install                                 ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"

    # --explain reads only the step headers, so it works with no kubeconfig and
    # answers "what will this do" before anything is loaded or prompted for.
    if [[ "${GENTIAN_EXPLAIN}" == "1" ]]; then
        drive_explain
        exit 0
    fi

    if [[ "${INSTALL_VERIFY_ONLY:-0}" == "1" ]]; then
        verify_argocd_apps || true
        print_summary_cp
        exit 0
    fi

    if [[ "${INSTALL_VALIDATE_ONLY:-0}" == "1" ]]; then
        validate_steps
        prepare_run
        success "Validation complete — no cluster changes were made."
        exit 0
    fi

    case "${GENTIAN_DIRECTION}" in
        export-kit)
            # Reads the cluster and OpenBao; changes neither.
            load_operator_config
            load_deployments_cluster_settings
            try_load_creds_from_openbao
            export_recovery_kit "${GENTIAN_KIT_PATH:-gentian-recovery-kit.age}"
            ;;
        status)
            drive_status
            ;;
        reverse)
            prepare_run
            warn "Uninstalling — this removes Gentian OS from the current cluster."
            drive_reverse
            success "Teardown complete."
            ;;
        forward)
            prepare_run
            drive_forward
            if [[ "${GENTIAN_DRY_RUN}" == "1" ]]; then
                success "Dry run complete — no cluster changes were made."
            else
                success "Bootstrap complete."
                print_summary_cp
            fi
            ;;
    esac
}

main "$@"
