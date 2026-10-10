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
#   scripts/steps/A-01-namespaces.sh   scripts/steps/C-01-cluster-claim.sh
#   scripts/steps/A-04-crossplane.sh   scripts/steps/C-02-appsets.sh
#   scripts/steps/A-06-argocd.sh       ...
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
#   ./install.sh --purge            the same, plus the data an uninstall keeps:
#                                   OpenBao and infra volumes, and local state
#   ./install.sh --purge --cluster-infra
#                                   the same, plus the shared operators this
#                                   installer brought up (CNPG, Reloader) and
#                                   their CRDs
#
# The last two are separate commands because "this installer created it" and
# "this cluster can lose it" are different questions. CNPG's CRDs define every
# Postgres on the machine, not only Gentian's, so a purge keeps them unless
# asked. --cluster-infra is that ask.
#
# A cluster is its claims and values in gentian-deployments, so those come
# first -- and an install writes them when they are absent rather than
# refusing with instructions. That is step 0 of the forward pass: if the
# definition is there, nothing is prepared; if it is not, every setting is
# asked with its default, the files are written, and they are committed and
# pushed signed (AD-2). There is no separate command for it: under AD-2 the
# operator does not edit and push these files by hand, so there is nothing
# to review between writing them and installing from them.
#
# Tenants are not the installer's: they are created through the director --
# the admin console, or `kubectl gentian tenants create`, its command-line
# client -- which commits them as the person who asked.
#
# Read before you run:
#
#   ./install.sh --explain          the step table and what each one mutates
#   ./install.sh --dry-run          run every check(), print the plan, change nothing:
#                                   not the cluster, not the deployments repository,
#                                   not a file or a key on this host
#   ./install.sh --status           where did this install get to?
#
# Run part of it:
#
#   ./install.sh --step A-07        just that one
#   ./install.sh --from B-03        resume from there
#   ./install.sh --only A-07,A-08   a named subset
#   ./install.sh --skip A-04        everything but that
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
# kernel/namespaces.yaml names it; the constant here is what the steps read
# before the layout is on the cluster to read it from.
export CROSSPLANE_NAMESPACE="${CROSSPLANE_NAMESPACE:-kernel-provisioning}"
# The control namespace: the operator, the director and the handover record
# live there, and E-02, E-03 and every summary that reports handover read it.
# E-02 used to set this for its own process only, so `--only E-03` and
# `--verify-only` looked in a namespace v5 never creates and reported a
# handover that had happened as not started.
export GENTIAN_SYSTEM_NAMESPACE="${GENTIAN_SYSTEM_NAMESPACE:-$(ns_kernel control)}"

# The rest of the layout, once, for every step and library that names a
# namespace through a variable.
#
# Each step used to export the few it needed itself -- B-04, B-08, C-01, E-02
# and E-03 all set OPENBAO_NAMESPACE -- and a step that did not fell through to
# the library's own default, which is still the old layout's name for the
# namespace. B-03 did not: on the first fresh v5 install it looked for the
# vault in namespace "openbao", which no longer exists, and stopped with
# "Neither the ClusterIP nor a kubectl port-forward responded" while the vault
# sat Ready in kernel-secrets. Set here, before any step runs, no step can
# forget. A value already in the environment is kept.
export OPENBAO_NAMESPACE="${OPENBAO_NAMESPACE:-$(ns_kernel secrets)}"
export TRANSIT_NAMESPACE="${TRANSIT_NAMESPACE:-$(ns_kernel seal)}"
export CERT_MANAGER_NAMESPACE="${CERT_MANAGER_NAMESPACE:-$(ns_kernel edge)}"
export CERT_MANAGER_NS="${CERT_MANAGER_NS:-${CERT_MANAGER_NAMESPACE}}"
export ENVOY_GATEWAY_NAMESPACE="${ENVOY_GATEWAY_NAMESPACE:-$(ns_kernel edge)}"
export SERVICES_NAMESPACE="${SERVICES_NAMESPACE:-$(ns_kernel edge)}"
export KERNEL_NAMESPACE="${KERNEL_NAMESPACE:-${SERVICES_NAMESPACE}}"
export GENTIAN_OPERATOR_NAMESPACE="${GENTIAN_OPERATOR_NAMESPACE:-$(ns_kernel control)}"
export GENTIAN_ARGOCD_NAMESPACE="${GENTIAN_ARGOCD_NAMESPACE:-$(ns_kernel gitops)}"
export GITOPS_NAMESPACE="${GITOPS_NAMESPACE:-$(ns_kernel gitops)}"
export EDGE_NAMESPACE="${EDGE_NAMESPACE:-$(ns_kernel edge)}"
export IDENTITY_NAMESPACE="${IDENTITY_NAMESPACE:-$(ns_kernel authentication)}"
export AUTHZ_NAMESPACE="${AUTHZ_NAMESPACE:-$(ns_kernel authorization)}"
export OBSERVABILITY_NAMESPACE="${OBSERVABILITY_NAMESPACE:-$(ns_kernel observability)}"
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
GENTIAN_PURGE=0
GENTIAN_PURGE_CLUSTER_INFRA=0
GENTIAN_KIT_PATH=""
GENTIAN_RECOVER_FROM=""
# Set by --rotate-break-glass-key and by nothing else: assigned here so that a
# value in the environment is not a flag nobody typed.
GENTIAN_ROTATE_BREAK_GLASS=""
GENTIAN_TENANT_NAME="${GENTIAN_TENANT_NAME:-}"

driver_usage() {
    # The header of this file, whole: from its title to the line that ends it.
    # A fixed line range cut it off in the middle of a sentence once the
    # header grew.
    sed -n '3,/^set -euo pipefail$/p' "${BASH_SOURCE[0]}" | sed -e '$d' -e 's/^# \{0,1\}//'
    cat <<'EOF'

Recovery:
  --export-recovery-kit [PATH]  write the bootstrap material (master password,
                                derivation salt, unseal keys, cluster identity)
                                to an encrypted kit; default gentian-recovery-kit.age
  --recover PATH                load a kit before installing, so derived
                                credentials reproduce their original values
  --rotate-break-glass-key      replace a break-glass signing key that is lost.
                                The installer refuses to run when the key the
                                deployments repository records for this cluster
                                is not in this host's keyring; --recover puts it
                                back. This is for when there is nothing to put
                                back: it generates a new key, after a person has
                                typed the old key's id at a terminal. Nothing
                                confirms it on their behalf

Looking before running:
  --explain             print what every step does, in order, and stop
  --status              report which steps this cluster has already satisfied
  --dry-run             run every check() and print the plan. Changes nothing:
                        no object in the cluster, no file in the deployments
                        checkout, no commit or push, no signing key, nothing
                        under ~/.gentian or ~/.local/bin. Where an install
                        would do one of those, it prints a line beginning
                        "Would". It reads: the cluster, and the deployments
                        repository's remote (to say whether the checkout is
                        behind it). With --uninstall or --purge it previews
                        the teardown the same way

Running part of it. A step is named by its number or its full id, so
--only B-08 and --only B-08-seed-secrets are the same thing:
  --only ID[,ID...]     run only these steps (--step is a synonym)
  --skip ID[,ID...]     run everything except these
  --from ID             start here and continue to the end
  --until ID            start at the beginning and stop after this one
  --phase NAME          one phase: control-plane, secrets, platform,
                        applications, handover — or its letter, A through E
  --force               apply even where check() says satisfied. The way past a
                        step whose check tests the wrong thing, without editing
                        code. A step needing this routinely has a check() that
                        is asking the wrong question

Other options:
  --validate, --check   check the step files, report which settings and
                        credentials this configuration has, then run the
                        pre-flight (tools, cluster reachable, Kubernetes
                        version, operator image). Asks no question, collects
                        no credential and runs no step's check(). Changes
                        nothing, on the same terms as --dry-run
  --verify-only         run post-install verification and print the summary
  --activate-admin      issue a new activation link for the cluster administrator
                        (admin@<kernel-domain>), mailed or shown once — for a lost
                        password when nobody can sign in to ask the director
  --disable-api-extensions
                        leave the Aluvian extensions -- the Operations
                        Console's service and the API-extension grant it needs
                        -- out of the scaffold. Everything of Gentian OS stays:
                        export, import, purge and the bundle need no extension
  --no-licence-report   turn the licence report off. By default the cluster
                        tells the report address once a day what it runs:
                        tenants, counts of accounts, and the apps installed
                        through the App Store. No personal data. Off, nothing
                        is sent and the App Store is not offered. The same as
                        GENTIAN_NO_LICENCE_REPORT=1 in install.env, which is
                        where to put it so a later run does not turn it back on
  --no-cluster-infra    do not issue the kernel wildcard certificate (C-03).
                        That is all it leaves out: cert-manager, CNPG and
                        Reloader are installed as always. The same as
                        INSTALL_CLUSTER_INFRA=0 in the environment
  --cluster-infra       with --purge, also remove them and their CRDs: CNPG,
                        Reloader, external-dns, cert-manager. They may serve
                        workloads that are not Gentian's, and this discards the
                        wildcard certificate, which Let's Encrypt rations to
                        five per week for one set of names.
                        It is also the ONLY thing that removes this cluster's
                        published DNS records: without it external-dns is
                        stopped before its sources go, so the records — and
                        the per-hostname edge certificates that depend on them
                        existing — survive a teardown
  --config-file PATH    override install.env
  --no-config-files     ignore install.env entirely; take everything from the
                        environment
  -h, --help            this message

Re-running is safe. Every step reads the cluster before it acts and skips what
is already done, which is why converging and updating are the same pass.
EOF
}

# =============================================================================
# Arguments
# =============================================================================
parse_driver_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --update)            GENTIAN_DIRECTION="forward" ;;
            --prepare-tenant)
                GENTIAN_DIRECTION="prepare-tenant"
                # The name is optional here and prompted for when absent, so a
                # bare --prepare-tenant is a question rather than an error.
                if [[ -z "${2:-}" || "${2:-}" == -* ]]; then GENTIAN_TENANT_NAME=""
                else shift; GENTIAN_TENANT_NAME="$1"; fi ;;
            --uninstall|--destroy) GENTIAN_DIRECTION="reverse" ;;
            # Purge is an uninstall that also removes what an uninstall keeps,
            # so it selects the same direction rather than a fourth one.
            --purge)             GENTIAN_DIRECTION="reverse"; GENTIAN_PURGE=1 ;;
            --explain)           GENTIAN_EXPLAIN=1 ;;
            --status)            GENTIAN_DIRECTION="status" ;;
            --dry-run)           GENTIAN_DRY_RUN=1 ;;
            --force)             GENTIAN_FORCE=1 ;;
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
            --layout)
                # There is one layout. The flag is still read so that a script
                # or a runbook carrying --layout v5 keeps working rather than
                # failing on an unknown option; anything else is refused,
                # because a caller asking for v4 wants something this installer
                # no longer builds and should hear so.
                shift; [[ $# -gt 0 ]] || { error "$0: --layout requires a value (v5)"; exit 1; }
                case "$1" in
                    v5)  warn "--layout v5 is the only layout and is now the default; the flag does nothing." ;;
                    *)   error "$0: --layout v5 is the only layout; '$1' is not built by this installer." ; exit 1 ;;
                esac ;;
            --export-recovery-kit)
                GENTIAN_DIRECTION="export-kit"
                if [[ -z "${2:-}" || "${2:-}" == -* ]]; then GENTIAN_KIT_PATH=""
                else shift; GENTIAN_KIT_PATH="$1"; fi ;;
            --recover)
                shift; [[ $# -gt 0 ]] || { error "$0: --recover requires a kit path"; exit 1; }
                GENTIAN_RECOVER_FROM="$1" ;;
            --rotate-break-glass-key) GENTIAN_ROTATE_BREAK_GLASS="rotate" ;;
            --disable-api-extensions) export GENTIAN_DISABLE_API_EXTENSIONS="1" ;;
            --no-licence-report) export GENTIAN_NO_LICENCE_REPORT="1" ;;
            --no-cluster-infra)  INSTALL_CLUSTER_INFRA="0" ;;
            --cluster-infra)     INSTALL_CLUSTER_INFRA="1"; GENTIAN_PURGE_CLUSTER_INFRA=1 ;;
            --config-file)
                shift; [[ $# -gt 0 ]] || { error "--config-file requires a value"; exit 1; }
                INSTALL_CONFIG_FILE="$1" ;;
            --secrets-file)
                shift; [[ $# -gt 0 ]] || { error "--secrets-file requires a value"; exit 1; }
                error "--secrets-file is no longer read."
                error "  Credentials come from the environment, the ~/.gentian cache, or OpenBao."
                error "  Export the variables, or let the installer prompt and cache them."
                exit 1 ;;
            --no-config-files)   INSTALL_AUTO_LOAD_CONFIG="0" ;;
            --verify-only)       INSTALL_VERIFY_ONLY="1" ;;
            --activate-admin)    GENTIAN_DIRECTION="activate-admin" ;;
            --validate|--check)  INSTALL_VALIDATE_ONLY="1" ;;
            -h|--help)           driver_usage; exit 0 ;;
            *)
                error "Unknown option: $1"
                driver_usage
                exit 1 ;;
        esac
        shift
    done
    # Refused rather than ignored, and refused here rather than mid-teardown: a
    # flag that silently does nothing is how somebody hands back a cluster still
    # carrying CNPG, having asked for it to be gone. Argument errors must not
    # depend on reaching a cluster — the earlier attempt at this sat after
    # credential collection and never ran.
    if [[ "${GENTIAN_PURGE_CLUSTER_INFRA}" == "1" && "${GENTIAN_DIRECTION}" == "reverse" \
          && "${GENTIAN_PURGE}" != "1" ]]; then
        error "--cluster-infra removes shared operators and their CRDs, which only --purge does."
        error "  Did you mean: ./install.sh --purge --cluster-infra"
        exit 1
    fi

    # The same for --rotate-break-glass-key: it belongs to an install, which is
    # the only run that asks whether the recorded key is here.
    if [[ -n "${GENTIAN_ROTATE_BREAK_GLASS}" ]] && \
       [[ "${GENTIAN_DIRECTION}" != "forward" || "${INSTALL_VERIFY_ONLY:-0}" == "1" || "${GENTIAN_EXPLAIN:-0}" == "1" ]]; then
        error "--rotate-break-glass-key replaces a lost signing key during an install or an update."
        error "  It does nothing with the command it was given beside; run it on its own:"
        error "    ./install.sh --rotate-break-glass-key"
        exit 1
    fi

    # --dry-run and --validate promise to change nothing, and these commands
    # do not know the promise: each goes its own way through main() and never
    # reaches the place a dry run is honoured. --verify-only restarts Argo
    # CD's controller when it finds it stuck, --activate-admin issues a link
    # and resets a password, --export-recovery-kit writes a file and may store
    # a backup key. Refused together rather than run as if the flag were not
    # there -- which is what happened, and is the worst of the three choices.
    if [[ "${GENTIAN_DRY_RUN:-0}" == "1" || "${INSTALL_VALIDATE_ONLY:-0}" == "1" ]]; then
        local _not_previewed=""
        [[ "${INSTALL_VERIFY_ONLY:-0}" == "1" ]] && _not_previewed="--verify-only"
        case "${GENTIAN_DIRECTION}" in
            activate-admin) _not_previewed="--activate-admin" ;;
            export-kit)     _not_previewed="--export-recovery-kit" ;;
        esac
        if [[ -n "${_not_previewed}" ]]; then
            error "${_not_previewed} has no preview: --dry-run and --validate apply to an install, an update, an uninstall or a purge."
            error "  Run ${_not_previewed} on its own, or drop it to preview the install."
            exit 1
        fi
    fi

    export GENTIAN_DRY_RUN GENTIAN_ONLY GENTIAN_FROM GENTIAN_UNTIL GENTIAN_SKIP GENTIAN_PHASE
    export GENTIAN_PURGE_CLUSTER_INFRA
    export INSTALL_CLUSTER_INFRA INSTALL_CONFIG_FILE GENTIAN_TENANT_NAME
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
    # Before the claim is read: a checkout behind its remote would answer
    # "this cluster has a definition" for one that was deleted on origin.
    if [[ "${INSTALL_VALIDATE_ONLY:-0}" == "1" || "${GENTIAN_DRY_RUN:-0}" == "1" ]]; then
        gentian_sync_deployments_checkout check || exit 1
    else
        gentian_sync_deployments_checkout || exit 1
    fi
    load_deployments_cluster_settings

    # The key this cluster's definition is signed with has to be on this
    # machine, if the repository records one. Asked here: the checkout is
    # current and the cluster is known, and nothing has been written yet --
    # no file in the checkout, no key, no object in a cluster. After
    # --recover, which is what puts a missing key back.
    if [[ "${GENTIAN_DIRECTION}" == "forward" ]]; then
        gentian_require_recorded_break_glass_key "${GENTIAN_ROTATE_BREAK_GLASS}" || exit 1
    fi

    try_load_creds_from_openbao

    # --validate: the report of the configuration, then the same pre-flight an
    # install runs, and nothing else. It asks no question and collects no
    # credential, so it is the one command that can be run before anything has
    # been answered: a cluster with no definition yet is told that an install
    # writes one, not refused for lacking it.
    if [[ "${INSTALL_VALIDATE_ONLY:-0}" == "1" ]]; then
        validate_config || exit 1
        GENTIAN_NONINTERACTIVE=1 prompt_app_repos
        local _undefined
        _undefined="$(cluster_deployment_missing | tr '\n' ' ')"
        if [[ -n "${_undefined}" ]]; then
            info "This cluster has no complete deployment definition yet (missing: ${_undefined% })."
            info "  An install writes it first, asking for each setting — step 0."
        else
            require_cluster_deployment
        fi
        _ensure_bao
        CROSSPLANE_MODE=1 check_prereqs
        return 0
    fi

    prompt_app_repos

    # Step 0. The claims and values this cluster is built from have to exist
    # before any credential is collected -- and when they do not, the install
    # makes them rather than refusing with instructions. When they do, the
    # same call commits and pushes any edit sitting in the checkout, signed,
    # so "change the claim, run ./install.sh" is the whole of a reconfigure.
    #
    # Not under --validate or --dry-run. Both promise to change nothing, and
    # writing files and pushing them to a remote is a change -- a smaller one
    # than touching a cluster, but not none. They report an incomplete
    # definition instead, which is the answer they are for.
    if [[ "${INSTALL_VALIDATE_ONLY:-0}" != "1" && "${GENTIAN_DRY_RUN:-0}" != "1" ]]; then
        if [[ -n "$(cluster_deployment_missing)" ]]; then
            echo ""
            info "This cluster has no deployment definition yet. Writing one first."
            info "  Nothing is applied and no cluster is contacted by this part."
        fi
        ensure_cluster_deployment || {
            error "Step 0 did not complete. Nothing was applied and no cluster was contacted."
            exit 1
        }
    fi

    # Now a genuine precondition rather than an instruction: it validates what
    # is there and commits an edit the operator made by hand -- or, under
    # --dry-run, says which edits an install would commit and leaves them.
    require_cluster_deployment

    resolve_kernel_domain_from_claim   # already-bootstrapped cluster reads its Claim
    prompt_kernel_domain
    prompt_network_mode

    # Driven by credentials.yaml. Validation runs here, before the first
    # apply(), so a bad credential aborts with the cluster untouched.
    #
    # Only a forward run needs them, and only a forward run is asked.
    #
    # A dry run applies nothing, and no step's check() reads a credential, so it
    # has everything it needs to print the plan without one. Asking anyway turns
    # "show me what you would do" into a credential hunt, and makes the preview
    # unavailable exactly when an operator most wants it — before they have
    # gathered the secrets.
    #
    # A teardown is the same argument, further along. No destroy() reads a
    # credential and neither does any helper in teardown.sh: removing an object
    # needs a kubeconfig, not a secret. Demanding one to uninstall is worse than
    # pointless — a cluster is most often torn down because something is wrong
    # with it, and the deployments token is exactly what an operator may no
    # longer have. Purge then refuses over a secret it will delete moments later.
    if [[ "${GENTIAN_DRY_RUN}" == "1" ]]; then
        info "Dry run: skipping credential collection — nothing is applied, so nothing is needed."
    elif [[ "${GENTIAN_DIRECTION}" == "reverse" ]]; then
        info "Teardown: skipping credential collection — destroy() reads none."
    else
        collect_bootstrap_credentials
    fi

    # bao first: check_prereqs lists it as required, and it is the one tool
    # the installer fetches itself, so checking before fetching aborted every
    # host that did not already have it.
    _ensure_bao
    CROSSPLANE_MODE=1 check_prereqs
}

# =============================================================================
# ensure_cluster_deployment — step 0 of every install.
#
# The definition this cluster is built from used to be a separate command the
# operator had to know to run first, and an install that met its absence
# refused with instructions. That is a question the installer can answer
# itself: if the files are there, there is nothing to prepare; if they are
# not, ask what they need and write them.
#
# Nothing is applied here and no cluster is contacted -- the questions and
# the files come first. Assumes load_operator_config and prompt_app_repos
# have run.
ensure_cluster_deployment() {
    resolve_kernel_domain_from_claim   # a re-run reads back what it wrote
    prompt_kernel_domain

    # A re-run is a scaffold of whatever is still missing, never a second
    # interview: the settings are in the claim already, and the claim is not
    # rewritten. Asking again produced answers nothing used and, without a
    # terminal, an install that stopped.
    if [[ -f "${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims/cluster.yaml" ]]; then
        info "clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims/cluster.yaml exists; its settings are read from it."
    else
        prompt_network_mode
        explain_network_mode
        prompt_issuer_mode
        explain_issuer_mode
        prompt_mail_mode
        prompt_cluster_settings
    fi
    scaffold_cluster_deployment
}

# =============================================================================
# prepare_tenant_run — retired: tenants are created through the director.
#
# This wrote a tenant's definition into the deployments checkout and left
# `kubectl gentian tenants deploy` to commit it: two ways round every check the
# director makes, each committing with whatever git credential this host held.
# A tenant is now created where every other change is -- the admin console's
# Tenants tab, or `kubectl gentian tenants create`, a client of the director --
# and the director commits it as the person who asked.
# =============================================================================
prepare_tenant_run() {
    error "--prepare-tenant is retired: tenants are created through the director."
    error "  Admin console → Tenants, or:"
    error "    kubectl gentian login"
    error "    kubectl gentian tenants create ${GENTIAN_TENANT_NAME:-<name>}"
    error "    kubectl gentian tenants activate-admin ${GENTIAN_TENANT_NAME:-<name>} [--recovery-email <address>]"
    return 1
}

# _ensure_bao — install the OpenBao CLI to ~/.local/bin when absent.
#
# The archive is checked against the release's own checksum list before
# anything is unpacked from it: this is a binary the installer goes on to hand
# the cluster's root token to.
_ensure_bao() {
    if command -v bao >/dev/null 2>&1; then
        return 0
    fi
    local _asset _sums _base _tmp _install_dir _want _have
    # Downloading a binary into ~/.local/bin is a change to this host.
    if gentian_read_only; then
        gentian_would "install the OpenBao CLI ${OPENBAO_CLI_VERSION} to ${HOME}/.local/bin, which this host does not have"
        return 0
    fi
    if ! _asset="$(openbao_cli_asset "${OPENBAO_CLI_VERSION}" "$(uname -s)" "$(uname -m)")" \
        || ! _sums="$(openbao_cli_checksums_asset "$(uname -s)")"; then
        error "The installer fetches the OpenBao CLI for Linux and macOS on x86_64 and arm64,"
        error "  and this host is $(uname -s) on $(uname -m). Install bao ${OPENBAO_CLI_VERSION} and run again."
        return 1
    fi
    _base="https://github.com/openbao/openbao/releases/download/${OPENBAO_CLI_VERSION}"
    _install_dir="${HOME}/.local/bin"
    _tmp="$(mktemp -d)"
    info "Installing the OpenBao CLI ${OPENBAO_CLI_VERSION} to ${_install_dir}..."
    if ! curl -fsSL --connect-timeout 30 --max-time 300 "${_base}/${_asset}" -o "${_tmp}/${_asset}" \
        || ! curl -fsSL --connect-timeout 30 --max-time 60 "${_base}/${_sums}" -o "${_tmp}/${_sums}"; then
        error "Could not download ${_base}/${_asset} and its checksums (${_sums})."
        rm -rf "${_tmp}"
        return 1
    fi
    _want="$(awk -v f="${_asset}" '$2 == f {print $1}' "${_tmp}/${_sums}")"
    _have="$(sha256_of "${_tmp}/${_asset}" || true)"
    if [[ -z "${_want}" || "${_want}" != "${_have}" ]]; then
        error "The OpenBao CLI archive does not match the release's checksum; nothing was installed."
        error "  ${_asset}: expected ${_want:-<not listed in ${_sums}>}, got ${_have:-<no checksum tool>}"
        rm -rf "${_tmp}"
        return 1
    fi
    mkdir -p "${_install_dir}"
    tar -xzf "${_tmp}/${_asset}" -C "${_install_dir}" bao
    chmod +x "${_install_dir}/bao"
    rm -rf "${_tmp}"
    export PATH="${_install_dir}:${PATH}"
    command -v bao >/dev/null 2>&1 || { error "OpenBao CLI install failed."; return 1; }
    success "OpenBao CLI installed (checksum verified)."
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
        prepare-tenant)
            prepare_tenant_run
            ;;
        activate-admin)
            # Break glass for the cluster administrator: a new single-use
            # activation link from the installer's own Keycloak credential,
            # for when nobody can sign in to ask the director for one.
            load_operator_config
            load_deployments_cluster_settings
            resolve_kernel_domain_from_claim
            # shellcheck source=scripts/lib/portal-login-bootstrap.sh
            source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"
            GENTIAN_ACTIVATE_FORCE=1 issue_platform_admin_activation
            exit $?
            ;;
        export-kit)
            # Reads the cluster and OpenBao; changes neither.
            load_operator_config
            load_deployments_cluster_settings
            # Get a token the way every other OpenBao caller does, rather than
            # requiring one to be exported already.
            #
            # try_load_creds_from_openbao below tries only BAO_TOKEN and the
            # init file's root token, and E-03 revokes that token and strips it
            # from the file at handover. So on any cluster past handover — which
            # is every cluster this command is for — both came up empty, the
            # lookup returned silently, and the export failed with "the master
            # password and derivation salt are both required. Set BAO_TOKEN and
            # retry", naming the one thing the installer already knows how to
            # obtain.
            #
            # _resolve_bao_token was written for exactly this: it prefers the
            # init file, then an OIDC sign-in as cluster-admin, and only then
            # prompts. The kit export was the one path that never called it.
            #
            # Failure here is not fatal under set -e: an unreachable OpenBao
            # still leaves the environment and the init files to gather from,
            # and export_recovery_kit says precisely what it could not find.
            # Aborting on an address lookup would replace that with a worse
            # message about a Service.
            resolve_openbao_access || true
            try_load_creds_from_openbao
            export_recovery_kit "${GENTIAN_KIT_PATH:-}"
            ;;
        status)
            drive_status
            ;;
        reverse)
            prepare_run
            if [[ "${GENTIAN_PURGE}" == "1" ]]; then
                # Confirm before anything is touched, not between the reverse
                # pass and the volume deletion — a teardown stopped half way is
                # the state this is least able to reason about.
                if [[ "${GENTIAN_DRY_RUN}" == "1" ]]; then
                    # Nothing is purged, so there is nothing to confirm -- and
                    # the confirmation takes the purge lock, which is a file.
                    info "Dry run: the purge is not confirmed, because it is not carried out."
                elif [[ -n "${GENTIAN_PURGE_CONFIRM:-}" ]]; then
                    [[ "${GENTIAN_PURGE_CONFIRM}" == "${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}" ]] || {
                        error "GENTIAN_PURGE_CONFIRM does not match GENTIAN_DEPLOYMENTS_CLUSTER_ID."
                        exit 1
                    }
                else
                    purge_confirm || exit 1
                fi
                warn "Purging — this removes Gentian OS and its data from the current cluster."
                if [[ "${GENTIAN_DRY_RUN}" == "1" ]]; then
                    info "Dry run: volumes and local state would be removed; nothing is."
                    # The most destructive option is the one a preview most has
                    # to name, so it reports the namespaces by the same
                    # derivation the real run uses rather than describing them.
                    if [[ "${GENTIAN_PURGE_CLUSTER_INFRA}" == "1" ]]; then
                        purge_cluster_infra
                    fi
                else
                    purge_release_volumes
                fi
            else
                warn "Uninstalling — this removes Gentian OS from the current cluster."
            fi
            # Before the reverse drive, not during it: the sources external-dns
            # publishes from are removed early on, and it prunes what it can no
            # longer see. --cluster-infra is the one case where the records
            # SHOULD go, and there it is left running to take them.
            if [[ "${GENTIAN_PURGE_CLUSTER_INFRA}" != "1" && "${GENTIAN_DRY_RUN}" != "1" ]]; then
                teardown_freeze_edge_dns
            fi
            drive_reverse
            if [[ "${GENTIAN_PURGE}" == "1" && "${GENTIAN_DRY_RUN}" != "1" ]]; then
                # The namespaces no step created, and so no step's destroy()
                # removes: a tenant's, and any shared or system namespace a
                # component landed in. Before the volume pass, which is what
                # reclaims what draining them releases.
                purge_tenant_namespaces
                purge_delete_volumes
                # After the volumes: the CRDs removed here define the objects
                # those volumes back, and taking the definitions first strands
                # the claims that release them.
                if [[ "${GENTIAN_PURGE_CLUSTER_INFRA}" == "1" ]]; then
                    purge_cluster_infra
                fi
                purge_local_state
                purge_report_remaining
                purge_report_cluster_residue
                success "Purge complete."
            else
                success "Teardown complete."
            fi
            ;;
        forward)
            prepare_run
            drive_forward
            if [[ "${GENTIAN_DRY_RUN}" == "1" ]]; then
                success "Dry run complete — no cluster changes were made."
            elif _forward_run_fully_satisfied; then
                # No verdict here — print_summary_cp gives it, once, having
                # checked whether handover actually completed. This line used
                # to say "Bootstrap complete" before the summary went on to
                # explain that it was not.
                print_summary_cp
            else
                # Deliberately not the completion banner and not the summary.
                # The summary prints portal credentials and a green "infra
                # bootstrap complete" line, which on an incomplete cluster
                # reads as a finished install — the exact misreport this
                # branch exists to prevent.
                warn "Steps still outstanding — this cluster is not fully installed:"
                local _missing_id
                for _missing_id in "${_MISSING_STEP_IDS[@]}"; do
                    warn "  ${_missing_id}"
                done
                echo ""
                info "Run ./install.sh to continue, or ./install.sh --status for the full picture."
            fi
            ;;
    esac
}

main "$@"
