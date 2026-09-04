#!/usr/bin/env bash
# =============================================================================
# scripts/lib/common.sh — Shared install helpers: logging, config, cluster prep, verification.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# ─── check() verdicts ────────────────────────────────────────────────────────
# The three answers a step's check() can give. Named because `return 2` at the
# bottom of a step file says nothing about what it means.
#
#   SATISFIED  the step's `provides:` already holds — skip it.
#   MISSING    it does not hold — apply() has work to do.
#   UNDEFINED  the question does not apply, or cannot be answered yet: the
#              feature is switched off, the step has no install-time artefact,
#              or the config that would decide it was never loaded.
#   ALWAYS     the step runs on every pass by design, so there is no state to
#              report. Its work is idempotent and cheap, and answering the
#              question properly would mean duplicating the work — B-10 would
#              have to probe every path kv_put_once already guards, E-02 would
#              have to do the reconcile it exists to perform, and B-04 exports a
#              per-run token that a skip would leave unset for every later step.
#              Applies like MISSING; reads as "always" rather than as a fault.
#
# UNDEFINED exists because a check that returns SATISFIED for "there was nothing
# to do" is indistinguishable from one that returns it for "I verified this is
# done", and only the second is a claim about the cluster. On the forward pass
# both skip; in --status only the second is green.
CHECK_SATISFIED=0
CHECK_MISSING=1
CHECK_UNDEFINED=2
CHECK_ALWAYS=3
# Exported because the readers are the step files in scripts/steps/, which are
# sourced rather than sourced-from-here — same reason install.sh exports the
# Crossplane settings the step bodies read.
export CHECK_SATISFIED CHECK_MISSING CHECK_UNDEFINED CHECK_ALWAYS

# ─── Colour helpers ──────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
BLUE='\033[0;34m'; BOLD='\033[1m'
# Exported for the same reason as the CHECK_ constants above: the reader is
# another file. The colours below this line are consumed by the info/success/
# warn/error helpers here, but BLUE and BOLD are used only by the step heading
# in scripts/lib/driver.sh, so within this file they look unused and shellcheck
# says so (SC2034). Exporting states the actual contract rather than silencing
# the warning.
export BLUE BOLD
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
# banner — a heading for a unit of work inside a step.
#
# One line, in the same rule-and-title language the driver uses for a phase, and
# indented under it. It used to be a three-line box of ═, which made the deepest
# thing on the screen the loudest: a phase announced itself with a light rule, a
# step with a bracketed id, and then the work inside that step drew a full-width
# double box. Two visual languages, and the hierarchy upside down.
# banner — a section heading, and only where a section actually begins.
#
# Silent inside a step. The driver already prints the phase rule and then the
# step's own title, so a third heading between them said what the line above it
# had just said, and put a rule across the middle of the step's output. There is
# one structure on screen now: phase, then step, then what the step does.
#
# Still printed outside a step, in the phase's own form, because Credentials and
# Pre-flight checks run before any phase begins and would otherwise have no
# heading at all. GENTIAN_CURRENT_STEP is what tells the two apart — the driver
# sets it around each step and unsets it after.
banner() {
    [[ -n "${GENTIAN_CURRENT_STEP:-}" ]] && return 0
    echo -e "\n${CYAN}── $* ───────────────────────────────────────────${NC}\n"
}

# Retry kubectl when the API server is temporarily unreachable (common on remote
# clusters or flaky client networks). Only connection-level failures are retried;
# resource errors (NotFound, wait timeouts, etc.) fail immediately.
# Override attempts/delay via KUBECTL_RETRY_ATTEMPTS / KUBECTL_RETRY_DELAY_SECS.
# gentian_dns_resolves <host> [zone] — is the name PUBLISHED, not "has my
# machine noticed".
#
# Asks the zone's own nameservers, because the local resolver answers a
# different question. A resolver that queried while the record was still absent
# caches that NXDOMAIN for the zone's negative TTL — the last SOA field, 1800s
# on Cloudflare — so it keeps saying "does not exist" for half an hour after the
# record is live.
#
# That is not hypothetical and not rare: the installer is the thing most likely
# to have asked too early, since it asks in a loop while waiting for the record
# to appear. Observed exactly so — external-dns had created id and portal, both
# authoritative nameservers answered, and getent kept returning nothing while
# D-03 timed out.
#
# The cluster's workloads and Let's Encrypt both resolve from the public DNS
# rather than from this machine, so "published" is the condition worth testing.
#
# Falls back to the local resolver when neither dig nor nslookup is present —
# pre-flight guarantees neither — and a stale negative answer there is worth a
# late wait rather than a wrong verdict, since the fallback can only be
# pessimistic.
gentian_dns_resolves() {
    local host="$1"
    local zone="${2:-}"
    local ns=""

    [[ -n "${host}" ]] || return 1
    # The zone defaults to the last two labels, which is right for the domains
    # this installer publishes and wrong only for multi-label public suffixes —
    # where the NS lookup simply returns nothing and this falls through.
    if [[ -z "${zone}" ]]; then
        zone="$(printf '%s' "${host}" | awk -F. '{ if (NF>=2) print $(NF-1)"."$NF; else print $0 }')"
    fi

    if command -v dig >/dev/null 2>&1; then
        ns="$(dig +short NS "${zone}" 2>/dev/null | head -1)"
        if [[ -n "${ns}" ]]; then
            [[ -n "$(dig +short A "${host}" "@${ns}" 2>/dev/null | head -1)" ]] && return 0
            [[ -n "$(dig +short AAAA "${host}" "@${ns}" 2>/dev/null | head -1)" ]] && return 0
            return 1
        fi
        [[ -n "$(dig +short A "${host}" 2>/dev/null | head -1)" ]] && return 0
        return 1
    fi

    if command -v nslookup >/dev/null 2>&1; then
        ns="$(nslookup -type=NS "${zone}" 2>/dev/null | awk '/nameserver =/{print $NF; exit}')"
        if [[ -n "${ns}" ]]; then
            nslookup "${host}" "${ns%.}" >/dev/null 2>&1 && return 0
            return 1
        fi
        nslookup "${host}" >/dev/null 2>&1 && return 0
        return 1
    fi

    # Local resolver, negative cache and all. Only ever too pessimistic.
    getent hosts "${host}" >/dev/null 2>&1
}

# _helm_retry — the same idea as _kubectl_retry, for helm.
#
# Every chart this installer applies is fetched over the network, from a chart
# repository or an OCI registry, and those have bad seconds. A single one aborts
# the whole run:
#
#   Error: failed to perform "Fetch" on source: Get "https://production.
#   cloudfront.docker.com/.../data?Expires=...": EOF
#
# That killed an install at A-07 with everything before it already applied. The
# steps are idempotent so a re-run resumes, but an operator watching a
# multi-hour install should not have to babysit a CDN.
#
# Matched on the error text, like the kubectl helper, so this only ever retries
# a transport failure. A chart that is genuinely wrong — a bad value, an
# immutable field, a failed hook — still fails on the first attempt, which is
# the behaviour worth keeping: retrying those just delays the diagnosis by
# however long the backoff lasts.
#
# Override with HELM_RETRY_ATTEMPTS / HELM_RETRY_DELAY_SECS.
_helm_retry() {
    local attempts="${HELM_RETRY_ATTEMPTS:-4}"
    local delay="${HELM_RETRY_DELAY_SECS:-10}"
    local n=1 rc=0 err=""
    local err_file
    err_file="$(mktemp)"
    # shellcheck disable=SC2064
    trap "rm -f '${err_file}'" RETURN

    while (( n <= attempts )); do
        if helm "$@" 2>"${err_file}"; then
            return 0
        else
            # Inside the else, not after the fi. After `if cmd; then ...; fi`
            # with no branch taken, $? is the status of the if construct — which
            # is 0 — so reading it there reported success for every failure the
            # regex declined to retry. Caught by a test that made helm fail with
            # a non-transient error and got exit 0 back.
            rc=$?
        fi
        err="$(<"${err_file}")"
        [[ -n "${err}" ]] && printf '%s\n' "${err}" >&2

        if ! [[ "${err}" =~ (EOF|connection[[:space:]]refused|connection[[:space:]]reset|TLS[[:space:]]handshake[[:space:]]timeout|i/o[[:space:]]timeout|dial[[:space:]]tcp|no[[:space:]]route[[:space:]]to[[:space:]]host|context[[:space:]]deadline[[:space:]]exceeded|failed[[:space:]]to[[:space:]]perform|temporary[[:space:]]failure[[:space:]]in[[:space:]]name[[:space:]]resolution|502[[:space:]]Bad[[:space:]]Gateway|503[[:space:]]Service[[:space:]]Unavailable|504[[:space:]]Gateway[[:space:]]Time) ]]; then
            return "${rc}"
        fi
        if (( n >= attempts )); then
            return "${rc}"
        fi
        warn "helm failed (attempt ${n}/${attempts}): transient network error"
        warn "  Retrying in ${delay}s..."
        sleep "${delay}"
        n=$((n + 1))
    done
}

_kubectl_retry() {
    local attempts="${KUBECTL_RETRY_ATTEMPTS:-12}"
    local delay="${KUBECTL_RETRY_DELAY_SECS:-5}"
    local n=1 rc=0 err=""
    local err_file
    err_file="$(mktemp)"
    # shellcheck disable=SC2064
    trap "rm -f '${err_file}'" RETURN

    while (( n <= attempts )); do
        if kubectl "$@" 2>"${err_file}"; then
            return 0
        else
            rc=$?
        fi
        err="$(<"${err_file}")"
        if [[ -n "$err" ]]; then
            printf '%s\n' "$err" >&2
        fi
        if ! [[ "$err" =~ (connection[[:space:]]refused|connection[[:space:]]reset|TLS[[:space:]]handshake[[:space:]]timeout|timeout[[:space:]]awaiting[[:space:]]response[[:space:]]headers|Unable[[:space:]]to[[:space:]]connect[[:space:]]to[[:space:]]the[[:space:]]server|no[[:space:]]route[[:space:]]to[[:space:]]host|i/o[[:space:]]timeout|dial[[:space:]]tcp) ]]; then
            return "$rc"
        fi
        if (( n >= attempts )); then
            return "$rc"
        fi
        warn "kubectl failed (attempt ${n}/${attempts}): transient API error"
        warn "  Retrying in ${delay}s..."
        sleep "$delay"
        n=$((n + 1))
    done
}

# Same idea as _kubectl_retry, for bao. The OpenBao Service resolved by
# gentian_service_addr answers a one-off reachability probe and then, on this
# host at least, is not reliably reachable call to call — seen live as a
# bootstrap that minted two of three tokens/policies fine (bao's own client
# retried under the timeout and recovered) and then hard-failed on the third
# with nothing to retry it. Override attempts/delay via BAO_RETRY_ATTEMPTS /
# BAO_RETRY_DELAY_SECS.
#
# Commands that read a body from stdin (bao policy write NAME -) cannot just
# inherit a heredoc at the call site: bash hands that heredoc to this
# function's stdin exactly once, so a retry after the first attempt consumes
# it would send bao an empty body instead of failing loudly. Set
# _BAO_RETRY_STDIN before calling to have this function re-feed the same
# content as a fresh here-string on every attempt.
_bao_retry() {
    local attempts="${BAO_RETRY_ATTEMPTS:-6}"
    local delay="${BAO_RETRY_DELAY_SECS:-5}"
    local n=1 rc=0 err=""
    local err_file
    err_file="$(mktemp)"
    # shellcheck disable=SC2064
    trap "rm -f '${err_file}'" RETURN

    while (( n <= attempts )); do
        # Not `if bao ...; then return 0; fi`: when neither branch of an
        # if/elif with no else executes, bash resets $? to 0 regardless of
        # what the untaken conditions returned — a real, nonzero bao failure
        # would read back as success. Capturing rc from the call directly
        # sidesteps that.
        if [[ -n "${_BAO_RETRY_STDIN+set}" ]]; then
            bao "$@" 2>"${err_file}" <<<"${_BAO_RETRY_STDIN}"
            rc=$?
        else
            bao "$@" 2>"${err_file}"
            rc=$?
        fi
        if (( rc == 0 )); then
            [[ -s "${err_file}" ]] && cat "${err_file}" >&2
            return 0
        fi
        err="$(<"${err_file}")"
        if [[ -n "$err" ]]; then
            printf '%s\n' "$err" >&2
        fi
        if ! [[ "$err" =~ (connection[[:space:]]refused|connection[[:space:]]reset|TLS[[:space:]]handshake[[:space:]]timeout|timeout[[:space:]]awaiting[[:space:]]response[[:space:]]headers|i/o[[:space:]]timeout|dial[[:space:]]tcp) ]]; then
            return "$rc"
        fi
        if (( n >= attempts )); then
            return "$rc"
        fi
        # >&2 explicitly: warn() writes to stdout in this codebase, which is
        # fine for a plain call but corrupts a caller like
        # cp_token=$(_bao_retry token create ... | jq ...) — these lines would
        # feed straight into jq as bogus input, jq would error out and close
        # the pipe, and this loop's next write would die to SIGPIPE after only
        # one or two of the configured attempts. Reproduced live: exactly that
        # jq parse error, and the retry count silently short by attempt 6.
        warn "bao failed (attempt ${n}/${attempts}): transient connection error" >&2
        warn "  Retrying in ${delay}s..." >&2
        sleep "$delay"
        n=$((n + 1))
    done
}

# =============================================================================
# wait_for_running_pod NS LABEL_SELECTOR FRIENDLY_NAME [TIMEOUT_SECS]
#
# Polls every POLL_INTERVAL seconds until at least one pod matching
# LABEL_SELECTOR in NS is in phase=Running. Every STATUS_INTERVAL seconds it
# prints a status line with the current pod status. If a pod sits in a
# non-Running, non-Pending phase (e.g. ContainerCreating, ImagePullBackOff)
# for STUCK_THRESHOLD seconds it dumps the most recent kubelet events.
#
# Auto-recovery: when a pod has been stuck in ContainerCreating with no IP
# assigned for STUCK_RECOVERY_THRESHOLD seconds (a microk8s/containerd CNI
# sandbox hang we've seen repeatedly), delete the pod so its StatefulSet /
# Deployment / DaemonSet recreates it cleanly. Up to MAX_RECOVERY_ATTEMPTS
# automatic kicks per call.
#
# Returns 0 on success, 1 on timeout.
# =============================================================================
wait_for_running_pod() {
    local ns="$1" selector="$2" friendly="$3" timeout="${4:-300}"
    local poll_interval=5 status_interval=30 stuck_threshold=60
    local stuck_recovery_threshold=120 max_recovery_attempts=2
    local elapsed=0 stuck_for=0 last_status="" recovery_attempts=0

    info "Waiting for ${friendly} pod in namespace '${ns}' to become Running (up to ${timeout}s)..."
    while (( elapsed < timeout )); do
        local line phase
        line=$(kubectl get pods -n "$ns" -l "$selector" \
                --no-headers 2>/dev/null | head -1 || true)
        if [[ -z "$line" ]]; then
            phase="NotScheduledYet"
        else
            phase=$(awk '{print $3}' <<<"$line")
        fi

        if [[ "$phase" == "Running" ]]; then
            # Confirm at least one container is Ready.
            local ready
            ready=$(kubectl get pods -n "$ns" -l "$selector" \
                    -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || echo "false")
            if [[ "$ready" == "true" ]]; then
                echo ""
                success "${friendly} pod is Running and Ready."
                return 0
            fi
        fi

        # Periodic status line.
        if (( elapsed % status_interval == 0 )); then
            echo "  [${elapsed}s] status: ${phase:-<none>} ${line:+($line)}"
        fi

        # Track stuck-in-non-progressing state.
        if [[ "$phase" != "Pending" && "$phase" != "Running" && "$phase" != "NotScheduledYet" ]] \
           || [[ "$phase" == "Pending" && -n "$line" ]]; then
            if [[ "$phase" == "$last_status" ]]; then
                stuck_for=$(( stuck_for + poll_interval ))
            else
                stuck_for=0
            fi
        else
            stuck_for=0
        fi
        last_status="$phase"

        if (( stuck_for == stuck_threshold )); then
            warn "${friendly} pod has been in '${phase}' for ${stuck_for}s. Recent events:"
            kubectl get events -n "$ns" --sort-by=.lastTimestamp 2>/dev/null \
                | tail -10 | sed 's/^/    /'
        fi

        # Auto-recovery for the silent CNI / containerd sandbox hang we've
        # seen on microk8s: pod sits in ContainerCreating with no assigned IP,
        # and kubelet has stopped emitting events. Deleting the pod forces the
        # owning controller to create a fresh one, which usually unsticks it.
        if (( stuck_for >= stuck_recovery_threshold )) \
           && (( recovery_attempts < max_recovery_attempts )) \
           && [[ "$phase" == "ContainerCreating" || "$phase" == "Init:0/"* ]]; then
            local pod_name pod_ip
            pod_name=$(kubectl get pods -n "$ns" -l "$selector" \
                    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
            pod_ip=$(kubectl get pods -n "$ns" -l "$selector" \
                    -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)
            if [[ -n "$pod_name" && -z "$pod_ip" ]]; then
                recovery_attempts=$(( recovery_attempts + 1 ))
                warn "Auto-recovery ${recovery_attempts}/${max_recovery_attempts}:" \
                     "deleting wedged pod ${pod_name} (no IP after ${stuck_for}s in ${phase})."
                kubectl delete pod "$pod_name" -n "$ns" --grace-period=0 --force \
                    >/dev/null 2>&1 || true
                # On the second attempt, also sweep stale CRI state — the
                # most common root cause of this wedge is leaked sandboxes
                # from prior install/uninstall cycles.
                if (( recovery_attempts >= 2 )); then
                    cri_cleanup
                    # If pod-delete + cri_cleanup both failed, the kubelet
                    # status manager itself is wedged (calico assigns an IP
                    # but PodReadyToStartContainers never flips True). The
                    # only known-reliable fix is a kubelite restart. Done
                    # under sudo, gated on microk8s being detected.
                    kubelite_restart
                fi
                stuck_for=0
                last_status=""
            fi
        fi

        sleep "$poll_interval"
        elapsed=$(( elapsed + poll_interval ))
    done

    echo ""
    error "${friendly} pod did not reach Running within ${timeout}s."
    warn  "Diagnostics:"
    kubectl get pods -n "$ns" -l "$selector" -o wide 2>&1 | sed 's/^/    /'
    kubectl get events -n "$ns" --sort-by=.lastTimestamp 2>/dev/null \
        | tail -15 | sed 's/^/    /'

    # Diagnose actual failure mode rather than printing generic boilerplate.
    local d_pod d_ip d_phase d_started d_pvc_bound d_pull_err d_ready_cond
    d_pod=$(kubectl get pods -n "$ns" -l "$selector" \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    d_ip=$(kubectl get pod -n "$ns" "$d_pod" \
            -o jsonpath='{.status.podIP}' 2>/dev/null || true)
    d_phase=$(kubectl get pod -n "$ns" "$d_pod" \
            -o jsonpath='{.status.phase}' 2>/dev/null || true)
    d_started=$(kubectl get pod -n "$ns" "$d_pod" \
            -o jsonpath='{.status.containerStatuses[0].started}' 2>/dev/null || true)
    d_ready_cond=$(kubectl get pod -n "$ns" "$d_pod" \
            -o jsonpath='{range .status.conditions[?(@.type=="PodReadyToStartContainers")]}{.status}{end}' 2>/dev/null || true)
    d_pvc_bound=$(kubectl get pvc -n "$ns" --no-headers 2>/dev/null \
            | awk '$2!="Bound"' | wc -l | tr -d ' ')
    d_pull_err=$(kubectl get events -n "$ns" --field-selector=reason=Failed \
            -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null \
            | grep -iE 'pull|image' | head -1 || true)
    # Detect missing-secret / missing-configmap (CreateContainerConfigError).
    local d_missing_ref
    d_missing_ref=$(kubectl get events -n "$ns" --field-selector=reason=Failed \
            -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null \
            | grep -iE '(secret|configmap) "[^"]+" not found' | head -1 || true)

    if (( d_pvc_bound > 0 )); then
        warn "Likely cause: PVC binding failure ($d_pvc_bound PVC(s) not Bound)."
    elif [[ -n "$d_missing_ref" ]]; then
        warn "Likely cause: missing referenced resource: ${d_missing_ref}"
        warn "Create the missing Secret/ConfigMap (often produced by a prior bootstrap step) and retry."
    elif [[ -n "$d_pull_err" ]]; then
        warn "Likely cause: image pull error: ${d_pull_err}"
    elif [[ "$d_started" == "true" && "$d_ready_cond" != "True" ]]; then
        warn "Likely cause: kubelet status-sync wedge (container started but"
        warn "PodReadyToStartContainers stays False)."
        warn "Recovery: fully restart your Kubernetes runtime, then re-run this script."
        warn "  microk8s : sudo microk8s stop && sudo microk8s start"
        warn "  k3s      : sudo systemctl restart k3s"
        warn "  kubeadm  : sudo systemctl restart kubelet containerd"
    elif [[ -n "$d_ip" && "$d_phase" == "Pending" ]]; then
        warn "Likely cause: kubelet wedge (IP assigned but phase still Pending)."
        warn "Recovery: fully restart your Kubernetes runtime, then re-run this script."
        warn "  microk8s : sudo microk8s stop && sudo microk8s start"
        warn "  k3s      : sudo systemctl restart k3s"
        warn "  kubeadm  : sudo systemctl restart kubelet containerd"
    else
        warn "Cause unclear. Check 'kubectl describe pod -n $ns $d_pod' for details."
    fi
    return 1
}

# cri_cleanup() and kubelite_restart() are defined in scripts/lib/lib-runtime.sh
# (sourced near the top of this file) so step destroy() paths can reuse them.

# ─── Paths ────────────────────────────────────────────────────────────────────
# SCRIPT_DIR is already set by the outer install.sh to the repo root before
# this file is sourced. Do not overwrite it. The ":-" default only applies
# when SCRIPT_DIR is unset or empty.
SCRIPT_DIR="${SCRIPT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

# ─── Runtime defaults ─────────────────────────────────────────────────────────
# Under ~/.gentian, not /tmp.
#
# This file holds the primary OpenBao's recovery key, and holds it alone: the
# key is issued once, at init, and exists nowhere else until a recovery kit is
# exported. In /tmp that meant the only copy of the one credential that opens
# OpenBao when Keycloak is broken lived somewhere a reboot empties — and the
# window between installing and exporting a kit is however long the operator
# takes to get to it. The documentation's answer was to warn them not to
# reboot, which is not an answer.
#
# ~/.gentian already holds the other things an install must not lose between
# runs (config, the bootstrap credential cache), is mode 700, and is removed by
# purge_local_state — which reads this variable, so the purge follows the file
# without needing to know where it went.
OPENBAO_INIT_FILE="${OPENBAO_INIT_FILE:-${HOME}/.gentian/openbao-init.json}"
INSTALL_CLUSTER_INFRA="${INSTALL_CLUSTER_INFRA:-1}"
# Operator-managed env files (config + secrets). These are optional, but when
# present they are sourced automatically before prompting so installs can be
# fully declarative and non-interactive.
INSTALL_CONFIG_FILE="${INSTALL_CONFIG_FILE:-${SCRIPT_DIR}/install.env}"
INSTALL_AUTO_LOAD_CONFIG="${INSTALL_AUTO_LOAD_CONFIG:-1}"
INSTALL_VALIDATE_ONLY="${INSTALL_VALIDATE_ONLY:-0}"
INSTALL_VERIFY_ONLY="${INSTALL_VERIFY_ONLY:-0}"

# Local on-disk cache of non-secret installer state (kernel domain, etc.) so
# re-runs do not re-prompt. Gitignored. Set INSTALL_STATE_FILE=/dev/null to
# disable persistence.
INSTALL_STATE_FILE="${INSTALL_STATE_FILE:-${SCRIPT_DIR}/.install-state.env}"
CERT_MANAGER_NAMESPACE="${CERT_MANAGER_NAMESPACE:-cert-manager}"

# Input precedence (highest -> lowest):
#   1) CLI flags / existing shell environment
#   2) installer config files (install.env, install.secrets.env)
#   3) local caches (.install-secrets.env, .install-state.env)
#   4) gentian-deployments cluster-settings.env (overrides 3 for cluster runtime)
#   5) OpenBao backfill for missing values
#   6) interactive prompts for missing required values
#
# Cluster runtime vars (KERNEL_DOMAIN, MAIL_SERVICE_MODE, …) belong in
# clusters/<cluster>/kernel/cluster-settings.env; .install-state.env keeps only
# installer-local state (see save_install_state).
INPUT_HIERARCHY_VARS=(
    MASTER_PASSWORD
    SMTP_RELAY_USERNAME
    SMTP_RELAY_PASSWORD
    MAIL_SERVICE_MODE
    EXTERNAL_SMTP_HOST
    EXTERNAL_SMTP_PORT
    EXTERNAL_SMTP_SSL
    EXTERNAL_SMTP_STARTTLS
    KERNEL_DOMAIN
    TENANCY_MODE
    NODE_IP
    NETWORK_MODE
    SKIP_TOOLS
    OPENBAO_INIT_FILE
    LETSENCRYPT_EMAIL
    ROUTING_MODE
    GENTIAN_APPS_REPO
    GENTIAN_APPS_BRANCH
    GENTIAN_DEPLOYMENTS_REPO
    GENTIAN_DEPLOYMENTS_BRANCH
    GENTIAN_DEPLOYMENTS_PATH
    GENTIAN_DEPLOYMENTS_CLUSTER_ID
    GENTIAN_DEPLOYMENTS_STAGE
    GENTIAN_DEPLOYMENTS_GIT_TOKEN
    GENTIAN_DEPLOYMENTS_GIT_USERNAME
    GENTIAN_NONINTERACTIVE
    INSTALL_CLUSTER_INFRA
    GENTIAN_MANAGED_CERT_MANAGER
    CF_API_TOKEN
    CF_ZONE_NAME
    SECRET_MODE
    INFRA_CHART_PRIVATE
    INFRA_CHART_REPO
    STORAGE_CLASS
)

# ─── Versions ────────────────────────────────────────────────────────────────
# Pinned in versions.yaml, read here. See scripts/lib/versions.sh for why the
# inventory lives in one file rather than beside each helm invocation.
ESO_CHART_VERSION="$(gentian_pin external-secrets chart)"
export ESO_CHART_VERSION
ENVOY_GATEWAY_CHART_VERSION="${ENVOY_GATEWAY_CHART_VERSION:-$(gentian_pin envoy-gateway chart)}"
ENVOY_GATEWAY_NAMESPACE="${ENVOY_GATEWAY_NAMESPACE:-envoy-gateway-system}"
GENTIAN_GATEWAY_CONTROLLER_NAME="${GENTIAN_GATEWAY_CONTROLLER_NAME:-gateway.envoyproxy.io/gentian-gatewayclass-controller}"

usage() {
    cat <<'EOF'
Usage: ./install.sh [options]

Options:
  --no-cluster-infra   Skip cluster infra installation (cert-manager, reloader, CNPG)
  --cluster-infra      Force cluster infra installation (default)
  --config-file PATH   Source non-secret installer config from PATH
  --secrets-file PATH  Source secret installer values from PATH
  --no-config-files    Disable auto-loading of install.env
    --verify-only        Skip install steps and only run ArgoCD health verification
  --validate, --check  Validate config and secrets; print a report and exit (no
                       cluster actions are taken)
  -h, --help           Show this help

Environment overrides:
  INSTALL_CLUSTER_INFRA=1|0
  INSTALL_CONFIG_FILE=/path/to/install.env
  INSTALL_VALIDATE_ONLY=1
EOF
}


load_env_file() {
    local file="$1"
    local label="$2"
    local var
    # Parallel arrays rather than an associative one: stock macOS bash is 3.2,
    # which has no `declare -A`. The two arrays are only ever appended together,
    # so index i of one always matches index i of the other.
    local before_keys=() before_vals=()

    [[ "${file}" == "/dev/null" ]] && return 0
    [[ -r "${file}" ]] || return 0

    # Do not let a lower-precedence source override values that are already
    # set by higher-precedence sources.
    for var in "${INPUT_HIERARCHY_VARS[@]}"; do
        if [[ -n "${!var+x}" ]]; then
            before_keys+=("${var}")
            before_vals+=("${!var}")
        fi
    done

    set -a
    # shellcheck disable=SC1090
    if ! source "${file}"; then
        set +a
        error "Failed to load ${label} from ${file}. Fix shell syntax in this file and retry."
        return 1
    fi
    set +a

    local i
    for i in "${!before_keys[@]}"; do
        declare -gx "${before_keys[$i]}=${before_vals[$i]}"
    done

    info "Loaded ${label} from ${file}."
}

# Source an env file and allow its values to override any already-set variables.
# Used for gentian-deployments cluster-settings.env (Git source of truth for
# cluster runtime behavior).
load_env_file_override() {
    local file="$1"
    local label="$2"

    [[ "${file}" == "/dev/null" ]] && return 0
    [[ -r "${file}" ]] || return 0

    set -a
    # shellcheck disable=SC1090
    if ! source "${file}"; then
        set +a
        error "Failed to load ${label} from ${file}. Fix shell syntax in this file and retry."
        return 1
    fi
    set +a

    info "Loaded ${label} from ${file} (overrides prior values)."
}

# validate_config checks that all required environment variables are set and
# that key values pass basic format validation. Exits 0 on success, 1 on
# failure. No cluster actions are taken.
validate_config() {
    local errors=0 warnings=0
    local deployments_root cluster
    local cluster_settings_file

    deployments_root="${GENTIAN_DEPLOYMENTS_PATH:-${HOME}/.gentian/gentian-deployments}"
    cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-default-cluster}"
    cluster_settings_file="${deployments_root}/clusters/${cluster}/kernel/cluster-settings.env"

    _file_header() {
        local file="$1" role="$2"
        echo ""
        echo "━━━ ${role} ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        if [[ -r "${file}" ]]; then
            echo "  [FILE]     ${file}"
        else
            echo "  [ABSENT]   ${file}"
        fi
    }

    _req_from() {
        local var="$1" hint="$2" source_file="$3"
        if [[ -z "${!var:-}" ]]; then
            echo "  [MISSING]  ${var}  — ${hint} (set in ${source_file})"
            (( errors++ )) || true
        else
            echo "  [OK]       ${var}"
        fi
    }

    _opt_from() {
        local var="$1" hint="$2" source_file="$3"
        if [[ -z "${!var:-}" ]]; then
            echo "  [WARN]     ${var}  — not set (${hint}; set in ${source_file})"
            (( warnings++ )) || true
        else
            echo "  [OK]       ${var}"
        fi
    }

    _file_header "the environment" "Secrets checks (environment, ~/.gentian cache, or OpenBao)"
    _req_from MASTER_PASSWORD          "HKDF master secret — used to derive all app secrets" "the environment"
    _opt_from CF_API_TOKEN       "Cloudflare token — needed for DNS-01 wildcard certificates" "the environment"
    if [[ -z "${CF_ZONE_NAME:-}" ]]; then
        echo "  [OK]       CF_ZONE_NAME  (optional; derived from KERNEL_DOMAIN when unset)"
    else
        echo "  [OK]       CF_ZONE_NAME"
    fi

    _file_header "${cluster_settings_file}" "Cluster checks (cluster-settings.env)"

    MAIL_SERVICE_MODE="$(gentian_mail_service_mode)"
    if [[ "${MAIL_SERVICE_MODE}" != "external" && "${MAIL_SERVICE_MODE}" != "kernel" ]]; then
        echo "  [INVALID]  MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE}  — must be 'external' or 'kernel' (set in ${cluster_settings_file})"
        (( errors++ )) || true
    else
        echo "  [OK]       MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE}  (install-time; invitation mail uses in-cluster Postfix when kernel)"
    fi
    if [[ "${MAIL_SERVICE_MODE}" == "external" ]]; then
        _opt_from EXTERNAL_SMTP_HOST "relay address — mail.host on the Cluster claim" "claims/cluster.yaml"
        _opt_from EXTERNAL_SMTP_PORT "relay port, default 587 — mail.port on the claim" "claims/cluster.yaml"
        echo "  [OK]       SMTP relay credentials  (runtime: supplied to the credential manager after install)"
    else
        echo "  [OK]       SMTP relay  (not used for MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE})"
    fi

    NETWORK_MODE="${NETWORK_MODE:-tunnel}"
    if ! mail_network_mode_compatible "${MAIL_SERVICE_MODE}" "${NETWORK_MODE}"; then
        echo "  [INVALID]  MAIL_SERVICE_MODE=kernel with NETWORK_MODE=tunnel"
        echo "             Kernel mail (Postfix/Dovecot) needs a reachable SMTP ingress; use MAIL_SERVICE_MODE=external with an SMTP relay on tunnel clusters."
        (( errors++ )) || true
    elif [[ "${MAIL_SERVICE_MODE}" == "kernel" ]]; then
        echo "  [OK]       MAIL_SERVICE_MODE=kernel with NETWORK_MODE=${NETWORK_MODE}"
    fi

    # KERNEL_DOMAIN has exactly one authored copy — the cluster's Crossplane
    # Claim (claims/cluster.yaml) — not cluster-settings.env. Resolve it the
    # same way the real install/update flow does before judging it missing.
    resolve_kernel_domain_from_claim
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            echo "  [MISSING]  KERNEL_DOMAIN  — platform-wide DNS suffix; not resolvable from claims/cluster.yaml (cluster not bootstrapped yet) and GENTIAN_NONINTERACTIVE=1 (export KERNEL_DOMAIN or set it in ${INSTALL_CONFIG_FILE})"
            (( errors++ )) || true
        else
            echo "  [PENDING]  KERNEL_DOMAIN  — not yet bootstrapped; no claims/cluster.yaml for this cluster yet. Will be prompted for interactively during install (see gentian-os/docs/deployment.md §1)."
        fi
    elif [[ ! "${KERNEL_DOMAIN}" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]; then
        echo "  [INVALID]  KERNEL_DOMAIN=${KERNEL_DOMAIN}  — must match ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?\$"
        (( errors++ )) || true
    else
        echo "  [OK]       KERNEL_DOMAIN=${KERNEL_DOMAIN}"
    fi
    TENANCY_MODE="${TENANCY_MODE:-multi}"
    if [[ "${TENANCY_MODE}" != "multi" && "${TENANCY_MODE}" != "single" ]]; then
        echo "  [INVALID]  TENANCY_MODE=${TENANCY_MODE}  — must be 'multi' or 'single' (set in ${cluster_settings_file})"
        (( errors++ )) || true
    else
        echo "  [OK]       TENANCY_MODE=${TENANCY_MODE}"
    fi

    _opt_from NETWORK_MODE  "networking mode: tunnel (default) or static-ip" "${cluster_settings_file}"
    if [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]]; then
        _req_from NODE_IP   "required in static-ip mode" "${cluster_settings_file}"
    else
        echo "  [OK]       NODE_IP  (not required for NETWORK_MODE=${NETWORK_MODE:-tunnel})"
    fi

    _file_header "${INSTALL_CONFIG_FILE}" "Installer config checks (install.env)"
    _opt_from LETSENCRYPT_EMAIL  "required for Let's Encrypt ACME; falls back to a dummy address" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_APPS_REPO       "defaults to https://git.example.domain/gentian-apps" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_APPS_BRANCH     "defaults to 'main'" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_DEPLOYMENTS_REPO    "defaults to https://git.example.domain/gentian-deployments" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_DEPLOYMENTS_BRANCH  "defaults to 'main'" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_DEPLOYMENTS_GIT_TOKEN "GitHub PAT for operator in-cluster git push" "the environment"
    _opt_from GENTIAN_DEPLOYMENTS_GIT_USERNAME "defaults to x-access-token for GitHub PATs" "the environment"

    LLM_SUPPORT="${LLM_SUPPORT:-false}"
    if [[ "${LLM_SUPPORT}" != "true" && "${LLM_SUPPORT}" != "false" ]]; then
        echo "  [INVALID]  LLM_SUPPORT=${LLM_SUPPORT}  — must be 'true' or 'false' (set in ${INSTALL_CONFIG_FILE} or ${cluster_settings_file})"
        (( errors++ )) || true
    else
        echo "  [OK]       LLM_SUPPORT=${LLM_SUPPORT}"
    fi

    GPU_ACCELERATION="${GPU_ACCELERATION:-false}"
    if [[ "${GPU_ACCELERATION}" != "true" && "${GPU_ACCELERATION}" != "false" ]]; then
        echo "  [INVALID]  GPU_ACCELERATION=${GPU_ACCELERATION}  — must be 'true' or 'false' (set in ${INSTALL_CONFIG_FILE} or ${cluster_settings_file})"
        (( errors++ )) || true
    else
        echo "  [OK]       GPU_ACCELERATION=${GPU_ACCELERATION}"
    fi

    if [[ "${GPU_ACCELERATION}" == "true" ]]; then
        if [[ "${LLM_SUPPORT}" != "true" ]]; then
            echo "  [INVALID]  GPU_ACCELERATION=true requires LLM_SUPPORT=true (set in ${INSTALL_CONFIG_FILE} or ${cluster_settings_file})"
            (( errors++ )) || true
        fi

        # Verify that the cluster actually has GPU resources available
        if kubectl cluster-info --request-timeout=5s >/dev/null 2>&1; then
            local gpus
            gpus=$(kubectl get nodes -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}' 2>/dev/null || true)
            local total_gpus=0
            for gpu in ${gpus}; do
                if [[ "${gpu}" =~ ^[0-9]+$ ]]; then
                    total_gpus=$((total_gpus + gpu))
                fi
            done
            if (( total_gpus > 0 )); then
                echo "  [OK]       GPU_ACCELERATION checks: cluster has ${total_gpus} allocatable GPU(s)"
            else
                echo "  [INVALID]  GPU_ACCELERATION=true but no nodes in the cluster report allocatable 'nvidia.com/gpu' resources."
                (( errors++ )) || true
            fi
        else
            echo "  [WARN]     GPU_ACCELERATION=true: cluster is unreachable, skipping cluster GPU resource check"
            (( warnings++ )) || true
        fi
    fi

    echo ""
    if (( errors > 0 )); then
        echo -e "${RED}Result: ${errors} error(s), ${warnings} warning(s) — config is NOT ready.${NC}"
        exit 1
    elif (( warnings > 0 )); then
        echo -e "${YELLOW}Result: 0 errors, ${warnings} warning(s) — config is ready (with caveats).${NC}"
        exit 0
    else
        echo -e "${GREEN}Result: all checks passed — config is ready.${NC}"
        exit 0
    fi
}

# load_operator_config sources declarative operator-provided env files before
# cache loading and interactive prompts. This enables non-interactive installs
# driven by pre-filled templates.
load_operator_config() {
    if [[ "${INSTALL_AUTO_LOAD_CONFIG}" != "1" ]]; then
        return 0
    fi
    load_env_file "${INSTALL_CONFIG_FILE}" "installer config"
    # install.secrets.env is NOT loaded. Credentials come from the environment,
    # the 0600 cache under ~/.gentian, or OpenBao — collect_bootstrap_credentials
    # tries all three and prompts for what is left. A plaintext file of secrets
    # beside the installer was a fourth source that nothing rotated and nothing
    # audited, and the one on this machine held a live Cloudflare token.
}


# =============================================================================
# Prompt for the gentian-apps and gentian-deployments repo URLs/branches.
# Defaults use example.domain placeholders — override via GENTIAN_*_REPO env vars.
# Persist results to ~/.gentian/config (bash-sourceable) so the kubectl-gentian
# plugin can locate the deployments repo when running `kubectl gentian apps install/uninstall`.
# =============================================================================
prompt_app_repos() {
    local default_apps_repo="https://git.example.domain/gentian-apps"
    local default_apps_branch="main"
    local default_deploy_repo="https://git.example.domain/gentian-deployments"
    local default_deploy_branch="main"
    local default_deploy_cluster="default-cluster"
    local default_deploy_stage="${ENV:-dev}"
    local v

    if [[ -z "${GENTIAN_APPS_REPO:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_APPS_REPO="${default_apps_repo}"
        else
            echo ""
            info "App catalogue and deployment repositories (missing values only):"
            read -rp "  gentian-apps repo URL [${default_apps_repo}]: " v
            GENTIAN_APPS_REPO="${v:-${default_apps_repo}}"
        fi
    fi

    if [[ -z "${GENTIAN_APPS_BRANCH:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_APPS_BRANCH="${default_apps_branch}"
        else
            read -rp "  gentian-apps branch [${default_apps_branch}]: " v
            GENTIAN_APPS_BRANCH="${v:-${default_apps_branch}}"
        fi
    fi

    if [[ -z "${GENTIAN_DEPLOYMENTS_REPO:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_DEPLOYMENTS_REPO="${default_deploy_repo}"
        else
            read -rp "  gentian-deployments repo URL [${default_deploy_repo}]: " v
            GENTIAN_DEPLOYMENTS_REPO="${v:-${default_deploy_repo}}"
        fi
    fi

    if [[ -z "${GENTIAN_DEPLOYMENTS_BRANCH:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_DEPLOYMENTS_BRANCH="${default_deploy_branch}"
        else
            read -rp "  gentian-deployments branch [${default_deploy_branch}]: " v
            GENTIAN_DEPLOYMENTS_BRANCH="${v:-${default_deploy_branch}}"
        fi
    fi

    if [[ -z "${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_DEPLOYMENTS_CLUSTER_ID="${default_deploy_cluster}"
        else
            read -rp "  deployment cluster ID [${default_deploy_cluster}]: " v
            GENTIAN_DEPLOYMENTS_CLUSTER_ID="${v:-${default_deploy_cluster}}"
        fi
    fi

    if [[ -z "${GENTIAN_DEPLOYMENTS_STAGE:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_DEPLOYMENTS_STAGE="${default_deploy_stage}"
        else
            read -rp "  gentian-deployments stage [${default_deploy_stage}]: " v
            GENTIAN_DEPLOYMENTS_STAGE="${v:-${default_deploy_stage}}"
        fi
    fi

    : "${GENTIAN_APPS_REPO:=${default_apps_repo}}"
    : "${GENTIAN_APPS_BRANCH:=${default_apps_branch}}"
    : "${GENTIAN_DEPLOYMENTS_REPO:=${default_deploy_repo}}"
    : "${GENTIAN_DEPLOYMENTS_BRANCH:=${default_deploy_branch}}"
        : "${GENTIAN_DEPLOYMENTS_CLUSTER_ID:=${default_deploy_cluster}}"
        : "${GENTIAN_DEPLOYMENTS_STAGE:=${default_deploy_stage}}"
    : "${GENTIAN_DEPLOYMENTS_PATH:=${HOME}/.gentian/gentian-deployments}"
    export GENTIAN_APPS_REPO GENTIAN_APPS_BRANCH \
            GENTIAN_DEPLOYMENTS_REPO GENTIAN_DEPLOYMENTS_BRANCH \
            GENTIAN_DEPLOYMENTS_CLUSTER_ID GENTIAN_DEPLOYMENTS_STAGE \
            GENTIAN_DEPLOYMENTS_PATH

    # ENV is the environment suffix behind namespaces, service hostnames and
    # per-stage manifest paths: gentian-${ENV}, gentian-infra-${ENV},
    # kernel/services/*/manifests/${ENV}, postfix-${ENV}, and ~30 more sites.
    #
    # It was never assigned anywhere on the install path, so every one of those
    # `${ENV:-dev}` expansions silently resolved to "dev". On a prod cluster that
    # meant SERVICES_NAMESPACE=gentian-dev and
    # MINIO_ENDPOINT=http://minio-dev.gentian-infra-dev... while the
    # ApplicationSets — which key off GENTIAN_DEPLOYMENTS_STAGE — deployed
    # *-prod Applications into gentian-infra-prod. The shell half and the GitOps
    # half disagreed about which environment the cluster was, and neither
    # complained.
    #
    # Derive it from the stage so there is a single source of truth. An
    # explicitly exported ENV still wins, for anyone who genuinely needs the two
    # to differ.
    ENV="${ENV:-${GENTIAN_DEPLOYMENTS_STAGE}}"
    export ENV
    info "Environment: ENV=${ENV} (from GENTIAN_DEPLOYMENTS_STAGE)"

    # Persist to ~/.gentian/config (bash-sourceable) so kubectl-gentian can read it.
    # The plugin sources this file directly; keep variable names aligned with the
    # plugin's expectations (GENTIAN_DEPLOYMENTS_PATH / GENTIAN_DEPLOYMENTS_REPO).
    local cfg_dir="${HOME}/.gentian"
    local cfg_file="${cfg_dir}/config"
    mkdir -p "$cfg_dir"
    cat >"$cfg_file" <<EOF
# Auto-generated by gentian-os/install.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ).
# Sourced by the kubectl-gentian plugin (~/.gentian/config).
GENTIAN_APPS_REPO="${GENTIAN_APPS_REPO}"
GENTIAN_APPS_BRANCH="${GENTIAN_APPS_BRANCH}"
GENTIAN_DEPLOYMENTS_REPO="${GENTIAN_DEPLOYMENTS_REPO}"
GENTIAN_DEPLOYMENTS_BRANCH="${GENTIAN_DEPLOYMENTS_BRANCH}"
GENTIAN_DEPLOYMENTS_CLUSTER_ID="${GENTIAN_DEPLOYMENTS_CLUSTER_ID}"
GENTIAN_DEPLOYMENTS_STAGE="${GENTIAN_DEPLOYMENTS_STAGE}"
GENTIAN_DEPLOYMENTS_PATH="${GENTIAN_DEPLOYMENTS_PATH}"
EOF
    chmod 0600 "$cfg_file"
    success "App repo configuration saved to ${cfg_file}"

    if [[ -z "${GENTIAN_DEPLOYMENTS_GIT_TOKEN:-}" ]]; then
        warn "GENTIAN_DEPLOYMENTS_GIT_TOKEN not set — in-cluster App Store installs cannot push to gentian-deployments."
        warn "  Export it, or let the installer prompt and cache it, when needed."
    fi
    : "${GENTIAN_DEPLOYMENTS_GIT_USERNAME:=x-access-token}"
    export GENTIAN_DEPLOYMENTS_GIT_USERNAME

}


# =============================================================================
# Load cluster-scoped non-secret settings from gentian-deployments checkout.
# File path convention:
#   ${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/cluster-settings.env
# =============================================================================
# The XRD's default for a spec field, read from the schema rather than restated.
#
# The Composition and the installer must resolve an omitted field to the same
# value. Writing `${NETWORK_MODE:-tunnel}` in shell creates a second default set
# that agrees until one of them moves — which is exactly how the mail namespace
# and the derived admin email came to differ from what the cluster actually used.
# There is one place a default may live, and this reads it.
xrd_default() {
    local field="$1"
    # A nested field needs a properties. between every segment, because that is
    # how OpenAPI nests: mail.serviceMode lives at
    # properties.mail.properties.serviceMode, not properties.mail.serviceMode.
    #
    # Without the translation every dotted field answered empty — all eight of
    # them, mail.*, llm.* and certificates.* — so the XRD default was silently
    # unreachable for exactly the settings that have one, and the ${VAR:-literal}
    # in shell became the real default. That is the second default set this
    # function exists to prevent, reappearing where the lint cannot see it.
    local path="${field//./.properties.}"
    yq_get ".spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.${path}.default" \
        "${SCRIPT_DIR}/crossplane/xrds/cluster.yaml" 2>/dev/null || true
}

# One cluster setting, resolved: the claim if it says, else the XRD's default.
#
# Absence with no XRD default is deliberately empty rather than guessed. Where
# empty is not a legal answer — nodeIp under networkMode=static-ip — the caller
# that knows that says so, because this function cannot.
claim_setting() {
    local var="$1" field="$2" claim_file="$3" value
    # An operator's environment still wins: an explicit export is an instruction.
    [[ -n "${!var:-}" ]] && return 0
    value="$(yq_get ".spec.${field}" "${claim_file}" 2>/dev/null || true)"
    [[ -z "${value}" ]] && value="$(xrd_default "${field}")"
    [[ -z "${value}" ]] && return 0
    printf -v "${var}" '%s' "${value}"
    export "${var?}"
}

# claim_map_setting VAR field claim_file — a map-valued claim field, flattened.
#
# claim_setting reads scalars. platformParams and certificates.dnsParams are
# maps whose keys differ per provider — Hetzner's location, Route 53's hosted
# zone — so there is nothing to enumerate here and nothing that should be.
#
# Flattened to the k=v,k=v shape LB_ANNOTATIONS already uses, because both end
# up as --set-string arguments and a second encoding would need a second parser.
claim_map_setting() {
    local var="$1" field="$2" claim_file="$3" value
    [[ -n "${!var:-}" ]] && return 0
    value="$(yq_get ".spec.${field} | to_entries | map(.key + \"=\" + (.value | tostring)) | join(\",\")" \
        "${claim_file}" 2>/dev/null || true)"
    [[ -z "${value}" || "${value}" == "null" ]] && return 0
    printf -v "${var}" '%s' "${value}"
    export "${var?}"
}

load_deployments_cluster_settings() {
    # Whether the operator named a path, recorded before the default fills it
    # in. A configured path that does not exist is a mistake to report, not an
    # invitation to pick a different repository: the installer writes claims
    # into this directory and reads the cluster's identity back out of it, so
    # substituting another checkout silently configures the wrong cluster.
    local _configured_path="${GENTIAN_DEPLOYMENTS_PATH:-}"
    : "${GENTIAN_DEPLOYMENTS_PATH:=${HOME}/.gentian/gentian-deployments}"

    # Local developer layout often checks out sibling repos under the same
    # parent directory (../gentian-deployments). Prefer that path when the
    # default cache location does not exist.
    if [[ -z "${_configured_path}" && ! -d "${GENTIAN_DEPLOYMENTS_PATH}" ]]; then
        local sibling_repo
        sibling_repo="$(cd "${SCRIPT_DIR}/.." && pwd)/gentian-deployments"
        if [[ -d "${sibling_repo}" ]]; then
            GENTIAN_DEPLOYMENTS_PATH="${sibling_repo}"
            export GENTIAN_DEPLOYMENTS_PATH
            info "Using sibling deployments repo at ${GENTIAN_DEPLOYMENTS_PATH}."
        fi
    fi

    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-default-cluster}"
    local settings_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel/cluster-settings.env"

    # Read when present, silent when not. Absent is the CORRECT state on a
    # current cluster: nothing writes cluster-settings.env any more -- everything
    # that described the cluster is a field on claims/cluster.yaml, and
    # --prepare-deployment deliberately stops emitting it (bootstrap.sh). This
    # branch survives only so a cluster that still has one keeps working while it
    # migrates.
    #
    # Announcing its absence on every run reported the normal case as a finding,
    # in the installer's first few lines, where an operator has the least context
    # to judge it -- and the old comment here said installing requires the file,
    # which stopped being true when the claim replaced it.
    if [[ -r "${settings_file}" ]]; then
        load_env_file_override "${settings_file}" "deployments cluster settings"
    fi

    # The claim, for everything the Cluster XRD already models.
    #
    # These are read from the FILE, not the cluster: the installer needs
    # networkMode and nodeIp at A-05 and A-07, and the Cluster XR is not created
    # until B-08. The claim is authored before either, so one document serves
    # both readers — yq here, Crossplane later — and there is no second surface
    # to keep in step.
    #
    # After the .env, so a value still in cluster-settings.env keeps working
    # while clusters migrate; each claim_setting is a no-op when the variable is
    # already set.
    local claim_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel/claims/cluster.yaml"

    # A cluster property in install.env beats the claim, silently.
    #
    # install.env is loaded first and claim_setting is a no-op for a variable
    # that already has a value, so setting one of these there means editing
    # claims/cluster.yaml has no effect and nothing says why. install.env is the
    # pointer file (§2); what the cluster IS belongs on the claim.
    #
    # Reported, not overridden: an operator who wrote it there meant something,
    # and silently reversing the precedence would be the same fault in the other
    # direction.
    local v
    for v in TENANCY_MODE NETWORK_MODE NODE_IP ROUTING_MODE SECRET_MODE \
             STORAGE_CLASS MAIL_SERVICE_MODE LB_PROVIDER LB_ANNOTATIONS \
             PLATFORM PLATFORM_PARAMS EDGE_ADDRESS_REF DNS_PROVIDER DNS_PARAMS \
             LLM_SUPPORT GPU_ACCELERATION GPU_TIME_SLICE_REPLICAS; do
        [[ -n "${!v:-}" ]] || continue
        [[ -r "${INSTALL_CONFIG_FILE:-}" ]] || continue
        grep -qE "^[[:space:]]*(export[[:space:]]+)?${v}=" "${INSTALL_CONFIG_FILE}" || continue
        warn "${v} is set in ${INSTALL_CONFIG_FILE} — it overrides claims/cluster.yaml."
        warn "  Move it to the claim: install.env is the pointer file, the claim is the cluster."
    done

    if [[ -r "${claim_file}" ]]; then
        claim_setting TENANCY_MODE      tenancyMode      "${claim_file}"
        claim_setting NETWORK_MODE      networkMode      "${claim_file}"
        claim_setting ROUTING_MODE      routingMode      "${claim_file}"
        claim_setting SECRET_MODE       secretMode       "${claim_file}"
        claim_setting NODE_IP           nodeIp           "${claim_file}"
        claim_setting STORAGE_CLASS     storageClass     "${claim_file}"
        claim_setting MAIL_SERVICE_MODE mail.serviceMode "${claim_file}"
        # The external relay, when mail.serviceMode is external. Same object,
        # same claim; EXTERNAL_SMTP_* were only ever the shell's names for them.
        claim_setting MAIL_EGRESS_HOST       mail.egressHost "${claim_file}"
        claim_setting EXTERNAL_SMTP_HOST     mail.host     "${claim_file}"
        claim_setting EXTERNAL_SMTP_PORT     mail.port     "${claim_file}"
        claim_setting EXTERNAL_SMTP_SSL      mail.ssl      "${claim_file}"
        claim_setting EXTERNAL_SMTP_STARTTLS mail.starttls "${claim_file}"
        # LLM serving. Hardware-dependent, and the XRD has said so all along.
        #
        # enabled and gpuAcceleration are the switches; without them the sizing
        # fields below came from the claim while the decision to use them did
        # not, so llm.enabled: true on the claim changed nothing and no step
        # said why.
        claim_setting LLM_SUPPORT             llm.enabled              "${claim_file}"
        claim_setting GPU_ACCELERATION        llm.gpuAcceleration      "${claim_file}"
        claim_setting GPU_TIME_SLICE_REPLICAS llm.gpuTimeSliceReplicas "${claim_file}"
        # Where this cluster runs, and who hosts its zone. Two dimensions, kept
        # apart on purpose: a Hetzner cluster on a Cloudflare zone is ordinary,
        # and one field could not describe it. Both name an entry in
        # kernel/platforms.yaml, which is where a new provider is added.
        #
        # PLATFORM is detected from the nodes' providerID when neither the claim
        # nor the environment says, so the claim is an override rather than a
        # requirement.
        claim_setting     PLATFORM        platform        "${claim_file}"
        claim_map_setting PLATFORM_PARAMS platformParams  "${claim_file}"
        # One specific platformParams key, read directly rather than parsed out
        # of PLATFORM_PARAMS' comma-joined string — gates whether the Kyverno
        # host-namespace exception MetalLB's speaker needs gets deployed at all
        # (kernel/appsets/raw/05b-metallb-exception.yaml).
        claim_setting     METALLB_EXCEPTION platformParams.metallb "${claim_file}"
        claim_setting     EDGE_ADDRESS_REF addressRef     "${claim_file}"
        claim_setting     DNS_PROVIDER    certificates.dnsProvider "${claim_file}"
        claim_map_setting DNS_PARAMS      certificates.dnsParams   "${claim_file}"
        claim_setting     EXTERNAL_DNS_ENABLED certificates.externalDns "${claim_file}"

        # lbProvider was PLATFORM's name while the edge load balancer was the
        # only thing it selected. Kept readable so a claim written before the
        # rename still installs; the claim is the thing to update.
        claim_setting LB_PROVIDER    lbProvider    "${claim_file}"
        claim_setting LB_ANNOTATIONS lbAnnotations "${claim_file}"
        if [[ -n "${LB_PROVIDER:-}" && -z "${PLATFORM:-}" ]]; then
            PLATFORM="${LB_PROVIDER}"
            export PLATFORM
            warn "claims/cluster.yaml sets lbProvider; it is now spec.platform."
            warn "  Reading it as platform=${PLATFORM}. Rename the field when convenient."
        fi
    fi
}

# =============================================================================
# Prompt for the cluster's kernel domain (the single platform-wide domain on
# which all kernel UIs — Keycloak, Gentian portal, and Argo CD — are served, and
# which provides the tenant app zone fallback when Tenant.spec.domain is unset
# (shape depends on TENANCY_MODE — see docs/design/multi-tenancy.md §3).
#
# Persisted via cluster-settings.env in gentian-deployments when set there;
# prompts only run when the value is still missing after load_deployments_cluster_settings.
prompt_kernel_domain() {
    if [[ -n "${KERNEL_DOMAIN:-}" ]]; then
        info "Using KERNEL_DOMAIN=${KERNEL_DOMAIN}"
        export KERNEL_DOMAIN
        return
    fi

    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
        error "KERNEL_DOMAIN is not set and GENTIAN_NONINTERACTIVE=1; aborting."
        error "Export KERNEL_DOMAIN=<your-domain> and re-run."
        exit 1
    fi

    echo ""
    info "Kernel domain (single platform-wide DNS suffix used for all kernel UIs"
    info "and as the default base for tenant apps without a vanity domain):"
    info "  examples: platform.example.com, apps.example.org"

    local v=""
    while [[ -z "$v" ]]; do
        read -rp "  KERNEL_DOMAIN: " v
        if [[ ! "$v" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]; then
            warn "Invalid domain: must match ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$"
            v=""
        fi
    done
    export KERNEL_DOMAIN="$v"
    save_install_state
}

# =============================================================================
# Prompt for the cluster networking mode.
#
#   tunnel    — cluster sits behind a reverse-proxy/tunnel (e.g. Cloudflare
#               Tunnel). DNS *.${KERNEL_DOMAIN} resolves to the tunnel
#               endpoint. NODE_IP is optional. DNS-01 ACME is preferred for
#               wildcard certificates.
#   static-ip — node has a public/reachable static IP. DNS *.${KERNEL_DOMAIN}
#               points directly to NODE_IP, which must be set and valid.
#               MetalLB (or equivalent) must be pre-configured. HTTP-01 ACME
#               challenges work without Cloudflare.
#
# Persisted to ${INSTALL_STATE_FILE} so re-runs do not re-prompt.
# =============================================================================
# =============================================================================
# prompt_issuer_mode — who issues this cluster's certificates.
#
# Asked rather than defaulted because getting it wrong is not a preference, it
# is a failed install: on a domain Let's Encrypt cannot resolve, every Gateway
# waits for a certificate that will never be issued, and the symptom names a
# missing Secret rather than an unreachable ACME endpoint.
# =============================================================================
prompt_issuer_mode() {
    local valid="acme-dns01 acme-http01 private-ca self-signed"
    if [[ -n "${CERT_ISSUER_MODE:-}" ]]; then
        case " ${valid} " in
            *" ${CERT_ISSUER_MODE} "*) ;;
            *) error "CERT_ISSUER_MODE=${CERT_ISSUER_MODE} is invalid. One of: ${valid}."; exit 1 ;;
        esac
        info "Using CERT_ISSUER_MODE=${CERT_ISSUER_MODE}"
        export CERT_ISSUER_MODE
        return
    fi

    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
        CERT_ISSUER_MODE="acme-dns01"
        info "CERT_ISSUER_MODE not set; defaulting to acme-dns01 (non-interactive)."
        export CERT_ISSUER_MODE
        return
    fi

    echo ""
    info "Certificate issuer — how this cluster gets TLS certificates:"
    info "  acme-dns01  : Let's Encrypt over DNS-01. Needs the Cloudflare API token."
    info "  acme-http01 : Let's Encrypt over HTTP-01. Needs port 80 reachable from the internet."
    info "  private-ca  : an existing CA you supply as a Secret."
    info "  self-signed : no public DNS and no ACME reachability. Browsers will warn."
    local v
    while true; do
        read -rp "  issuerMode [${valid// /|}] (default: acme-dns01): " v
        v="${v:-acme-dns01}"
        case " ${valid} " in *" ${v} "*) break ;; esac
        warn "Invalid value '${v}'."
    done
    export CERT_ISSUER_MODE="$v"
}

# =============================================================================
# prompt_mail_mode — where this cluster's mail goes.
#
# Asked because both answers need something else supplied with them: external
# needs a relay address here and a credential at install time, kernel needs
# static-ip. A silent default leaves a cluster that cannot send an invitation
# or a password reset, and nothing reports that until someone tries.
# =============================================================================
prompt_mail_mode() {
    if [[ -z "${MAIL_SERVICE_MODE:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            MAIL_SERVICE_MODE="external"
        else
            echo ""
            info "Mail — how this cluster sends mail:"
            info "  external : relay through an SMTP provider. Needs its address and credentials."
            info "  kernel   : in-cluster Postfix/Dovecot. Requires networkMode=static-ip."
            local v
            while true; do
                read -rp "  mail.serviceMode [external|kernel] (default: external): " v
                v="${v:-external}"
                [[ "$v" == "external" || "$v" == "kernel" ]] && break
                warn "Invalid value '${v}'."
            done
            MAIL_SERVICE_MODE="$v"
        fi
    fi
    export MAIL_SERVICE_MODE

    if [[ "${MAIL_SERVICE_MODE}" == "kernel" && "${NETWORK_MODE:-tunnel}" != "static-ip" ]]; then
        error "mail.serviceMode=kernel requires networkMode=static-ip; this cluster is ${NETWORK_MODE:-tunnel}."
        error "  Choose external, or re-run with NETWORK_MODE=static-ip."
        exit 1
    fi

    # Only external mode has a relay to name.
    #
    # The address is asked for and the credentials are not, which is worth
    # saying out loud: the address is configuration and belongs on the claim in
    # Git, while the username and password are a credential and belong in
    # OpenBao. Asking for all three together would put a password in a file the
    # operator is about to commit.
    if [[ "${MAIL_SERVICE_MODE}" == "external" && -z "${EXTERNAL_SMTP_HOST:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" != "1" ]]; then
            echo ""
            info "  The relay's ADDRESS only — its hostname, e.g. smtp.gmail.com."
            info "  The username and password are supplied after the install, through"
            info "  the credential manager. They are not asked for here and are not"
            info "  written to Git."
            while true; do
                read -rp "  mail.host [blank to set later]: " EXTERNAL_SMTP_HOST
                [[ -z "${EXTERNAL_SMTP_HOST}" ]] && break
                # An @ means an account was entered where a host belongs. The
                # claim would then carry a value that looks configured, and mail
                # would fail at send time against a hostname that never resolves.
                if [[ "${EXTERNAL_SMTP_HOST}" == *"@"* ]]; then
                    warn "That looks like an account, not a hostname."
                    warn "  The relay for user@gmail.com is smtp.gmail.com."
                    warn "  The username goes to the credential manager after the install."
                    continue
                fi
                if [[ "${EXTERNAL_SMTP_HOST}" != *.* ]]; then
                    warn "A relay hostname has a domain in it, e.g. smtp.gmail.com."
                    continue
                fi
                break
            done
        fi
        # An `if`, not `[[ ... ]] && export`, because this is the last statement
        # in the function and its status becomes the function's.
        #
        # Unset is the normal case: a non-interactive run has nobody to ask, and
        # a blank answer is explicitly allowed above ("blank to set later"). But
        # a false [[ ]] at the end of an && list returns 1, so prompt_mail_mode
        # returned 1, and under set -Eeuo pipefail the ERR trap turned "the
        # operator did not name a relay" into:
        #
        #   [ABORT] Install stopped: a command failed and was not handled.
        #     command   : [[ -n "${EXTERNAL_SMTP_HOST:-}" ]]
        #     call stack: ./install.sh:322 in prepare_deployment_run()
        #
        # which stopped --prepare-deployment before it wrote anything at all.
        if [[ -n "${EXTERNAL_SMTP_HOST:-}" ]]; then
            export EXTERNAL_SMTP_HOST
        fi
    fi
    return 0
}

# =============================================================================
# prompt_tenant_identity — the three things a tenant file cannot be written
# without: its name, what to call it, and who administers it.
#
# Everything else on a Tenant has a workable default or belongs to the cluster's
# shared defaults component, so asking for it here would be asking an operator
# to decide something before they have any reason to.
# =============================================================================
prompt_tenant_identity() {
    if [[ -z "${GENTIAN_TENANT_NAME:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            error "No tenant name given and this is a non-interactive run."
            error "  ./install.sh --prepare-tenant <name>"
            exit 1
        fi
        echo ""
        info "Tenant name — the Kubernetes object name, its namespace suffix and"
        info "its subdomain. Lower-case letters, digits and hyphens."
        local v
        while true; do
            read -rp "  Tenant name: " v
            # The same shape Kubernetes will insist on, checked here so the
            # failure is a re-prompt rather than a rejected push.
            if [[ "${v}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && ${#v} -le 40 ]]; then
                break
            fi
            warn "Not a valid name. Lower-case letters, digits and hyphens; must start and end with one."
        done
        GENTIAN_TENANT_NAME="${v}"
    fi
    export GENTIAN_TENANT_NAME

    if [[ -z "${GENTIAN_TENANT_DISPLAY_NAME:-}" && "${GENTIAN_NONINTERACTIVE:-0}" != "1" ]]; then
        local d
        read -rp "  Display name (default: ${GENTIAN_TENANT_NAME}): " d
        GENTIAN_TENANT_DISPLAY_NAME="${d:-${GENTIAN_TENANT_NAME}}"
    fi
    GENTIAN_TENANT_DISPLAY_NAME="${GENTIAN_TENANT_DISPLAY_NAME:-${GENTIAN_TENANT_NAME}}"
    export GENTIAN_TENANT_DISPLAY_NAME

    # No administrator address is asked for.
    #
    # It is derived — admin@<tenant>.<kernelDomain> — and it is the Keycloak
    # username as well, so there is nothing here for an operator to choose.
    # Asking would invite a contact address at some third party, which is an
    # account the tenant cannot receive password resets for.
}

prompt_network_mode() {
    if [[ -n "${NETWORK_MODE:-}" ]]; then
        if [[ "${NETWORK_MODE}" != "tunnel" && "${NETWORK_MODE}" != "static-ip" ]]; then
            error "NETWORK_MODE=${NETWORK_MODE} is invalid. Must be 'tunnel' or 'static-ip'."
            exit 1
        fi
        info "Using NETWORK_MODE=${NETWORK_MODE}"
        export NETWORK_MODE
        _prompt_node_ip
        return
    fi

    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
        NETWORK_MODE="tunnel"
        info "NETWORK_MODE not set; defaulting to tunnel (non-interactive)."
        export NETWORK_MODE
        save_install_state
        return
    fi

    echo ""
    info "Networking mode — how external traffic reaches this cluster:"
    info "  tunnel    : cluster is behind a tunnel/proxy (e.g. Cloudflare Tunnel)"
    info "  static-ip : cluster node has a public reachable static IP"
    local v
    while true; do
        read -rp "  NETWORK_MODE [tunnel|static-ip] (default: tunnel): " v
        v="${v:-tunnel}"
        if [[ "$v" == "tunnel" || "$v" == "static-ip" ]]; then
            break
        fi
        warn "Invalid value '${v}'. Enter 'tunnel' or 'static-ip'."
    done
    export NETWORK_MODE="$v"
    save_install_state
    _prompt_node_ip
}

# _is_ip_address <value> — dotted-quad or IPv6. NODE_IP becomes the Service's
# loadBalancerIP, which Kubernetes rejects unless it is an address, so a
# hostname typed here fails much later and somewhere else.
_is_ip_address() {
    local ip="$1" octet
    [[ "$ip" == *:* ]] && return 0
    [[ "$ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]] || return 1
    local IFS=.
    for octet in $ip; do
        # A leading zero is rejected by Go's IP parser, so Kubernetes would
        # refuse the address the Service is eventually given.
        [[ "$octet" =~ ^(0|[1-9][0-9]{0,2})$ ]] || return 1
        (( octet <= 255 )) || return 1
    done
    return 0
}

# _prompt_node_ip — ask for NODE_IP when static-ip mode needs one.
#
# Only auto-detection would fill this in otherwise, and that reads the first
# node's InternalIP from a running cluster: wrong for the public address DNS
# points at, and unavailable altogether to --prepare-deployment, which writes
# cluster-settings.env without contacting a cluster.
_prompt_node_ip() {
    [[ "${NETWORK_MODE}" == "static-ip" ]] || return 0

    if [[ -n "${NODE_IP:-}" ]]; then
        info "Using NODE_IP=${NODE_IP}"
        export NODE_IP
        return 0
    fi

    # Non-interactive keeps the cluster-side auto-detection it had; there is no
    # one to ask, and validate_config already reports an unset NODE_IP here.
    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
        return 0
    fi

    echo ""
    info "NETWORK_MODE=static-ip: DNS for this cluster points straight at one address."
    local v
    while true; do
        read -rp "  NODE_IP (the address DNS resolves to): " v
        if [[ -n "$v" ]] && _is_ip_address "$v"; then
            break
        fi
        warn "Enter an IP address, not a hostname — it becomes the edge Service's loadBalancerIP."
    done
    export NODE_IP="$v"
    save_install_state
}

# =============================================================================
# Prompt for kernel-level secrets that depend on KERNEL_DOMAIN. Currently:
#
#   - CF_API_TOKEN: Cloudflare API token with Zone:Read + DNS:Edit on the
#     KERNEL_DOMAIN's zone. Used to solve DNS-01 ACME challenges for the
#     kernel wildcard *.${KERNEL_DOMAIN} (platform UIs only). Tenant apps
#     use per-tenant DNS-01 wildcards via TENANT_DNS01_CLUSTER_ISSUER.
#     Optional — see docs/design/multi-tenancy.md §3.
#
# Declared in credentials.yaml as acme-dns-cloudflare; collected and validated
# by collect_bootstrap_credentials. Never written to local disk.
# =============================================================================
# RFC 5737 documentation addresses (TEST-NET-1/2/3).
_is_testnet_ip() {
    local ip="$1"
    [[ "$ip" =~ ^192\.0\.2\.[0-9]+$ || "$ip" =~ ^198\.51\.100\.[0-9]+$ || "$ip" =~ ^203\.0\.113\.[0-9]+$ ]]
}


# =============================================================================
# cleanup_orphaned_kyverno_webhooks — self-heal for a specific, cluster-breaking
# leftover state. Kyverno's MutatingWebhookConfiguration/ValidatingWebhookConfiguration
# objects are cluster-scoped and survive a `kubectl delete namespace kyverno`
# (or any teardown that doesn't go through Kyverno's own Helm uninstall hooks,
# e.g. a manual/partial teardown outside install.sh). Kyverno's
# webhooks fail-closed by default, so an orphaned one with no backing service
# blocks ALL matching resource creation cluster-wide — including Crossplane's
# own pods, before Kyverno is ever reinstalled later in the sequence.
#
# Safe to call unconditionally: only acts when the kyverno namespace is
# absent (i.e. Kyverno is not actually running) but its webhook
# registrations remain. A healthy, running Kyverno is left untouched.
# =============================================================================
cleanup_orphaned_kyverno_webhooks() {
    kubectl get namespace kyverno >/dev/null 2>&1 && return 0

    local hooks
    hooks=$(kubectl get mutatingwebhookconfiguration,validatingwebhookconfiguration \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
        | grep '^kyverno-' || true)
    [[ -z "${hooks}" ]] && return 0

    warn "Orphaned Kyverno webhook configuration(s) found with no kyverno namespace behind them:"
    warn "  $(printf '%s' "${hooks}" | tr '\n' ' ')"
    warn "  These fail closed by default and block ALL matching resource creation"
    warn "  cluster-wide (e.g. Crossplane's own pods). Removing them now —"
    warn "  Kyverno recreates them cleanly when reinstalled."
    while IFS= read -r hook; do
        [[ -z "${hook}" ]] && continue
        kubectl delete mutatingwebhookconfiguration "${hook}" --ignore-not-found=true 2>/dev/null || true
        kubectl delete validatingwebhookconfiguration "${hook}" --ignore-not-found=true 2>/dev/null || true
    done <<< "${hooks}"
    success "Orphaned Kyverno webhook configuration(s) removed."
}

# =============================================================================
# force_reconcile_failed_helm_releases — self-heal for Crossplane
# helm.crossplane.io Release CRs stuck in Helm's own "failed" state.
#
# provider-helm does not retry on its own: once a `helm upgrade` fails (e.g.
# two ConfigMaps colliding on the same values key — see 334fae7, which sat
# stuck for 2+ hours until manually annotated), the Release's Synced
# condition still reports ReconcileSuccess (Crossplane executed the
# reconcile; the reconcile just happened to conclude "failed") — so nothing
# about the object's own status signals "needs another look" or triggers
# another attempt on its own.
#
# Globbing committed kernel/services/*/manifests/${env}/release.yaml files
# (as the deleted update.sh --reconcile-releases did) misses most Release
# CRs in this cluster: they are Crossplane-composition-generated (owned by
# XApp/XInfraData/XSuze, e.g. Keycloak, OpenFGA, infra-{mariadb,minio,
# postgresql,redis}), which a file-globbing approach can't see at all.
# This checks live Release objects directly instead, regardless of how
# they were created, and force-reconciles (annotate + let Crossplane retry)
# any genuinely in Helm's "failed" state. Safe to call unconditionally —
# a no-op when everything is deployed/healthy.
# =============================================================================
# A Helm release that Helm itself marks `failed` does not recover by being
# poked, and this used to poke it and report success.
#
# It annotated each Release MR and printed "Requested re-reconcile for: …",
# which reads as an action that worked. It is not: Crossplane re-reads the
# object, provider-helm reports Synced=True ReconcileSuccess because the desired
# state has not changed since the last attempt, and no `helm upgrade` runs. The
# release stays at its last DEPLOYED revision — which is the one with whatever
# broke it — while every later revision sits `failed`.
#
# postfix-dev spent fourteen hours that way. The values that fixed it were
# corrected in Git, synced into the cluster, and never applied: five upgrades
# timed out waiting for a pod that could not start because it was still running
# the old spec. Every install run reported "Requested re-reconcile" and moved on.
#
# So this now reports the state honestly and names the one thing that clears it.
# Deleting the MR is left to a human by default: Crossplane uninstalls the Helm
# release on the way out, so it is a brief outage for that service, and a
# pre-flight check is not the place to decide that unprompted.
force_reconcile_failed_helm_releases() {
    local failed
    failed=$(kubectl get release.helm.crossplane.io \
        -o jsonpath='{range .items[?(@.status.atProvider.state=="failed")]}{.metadata.name}{"\n"}{end}' \
        2>/dev/null || true)
    [[ -z "${failed}" ]] && return 0

    warn "Crossplane Release(s) in Helm 'failed' state:"
    warn "  $(printf '%s' "${failed}" | tr '\n' ' ')"
    warn ""
    warn "  provider-helm does not retry these, and reports Synced because the"
    warn "  desired state has not changed since the attempt that failed. The"
    warn "  release therefore stays at its last DEPLOYED revision — so a fix"
    warn "  committed to Git and synced into the cluster is never applied."

    if [[ "${GENTIAN_RESET_FAILED_RELEASES:-0}" != "1" ]]; then
        warn ""
        warn "  To clear one, delete its Release; Argo CD recreates it and"
        warn "  provider-helm installs fresh from the current values:"
        while IFS= read -r name; do
            [[ -z "${name}" ]] && continue
            warn "    kubectl delete release.helm.crossplane.io/${name}"
        done <<< "${failed}"
        warn ""
        warn "  StatefulSet PVCs are retained, so persistent data survives — but"
        warn "  the service is down while it reinstalls. Re-run with"
        warn "  GENTIAN_RESET_FAILED_RELEASES=1 to have the installer do it."
        return 0
    fi

    warn "  GENTIAN_RESET_FAILED_RELEASES=1 — deleting so they reinstall..."
    while IFS= read -r name; do
        [[ -z "${name}" ]] && continue
        gentian_run kubectl delete release.helm.crossplane.io/"${name}" \
            --wait=true --timeout=180s || true
    done <<< "${failed}"
    success "Deleted: $(printf '%s' "${failed}" | tr '\n' ' ') — Argo CD will recreate them."
    info "  Watch with: kubectl get release.helm.crossplane.io"
}

# =============================================================================
# 0. Pre-flight checks
# =============================================================================
check_prereqs() {
    banner "Pre-flight checks"

    local missing=0

    # ── CLI tools ─────────────────────────────────────────────────────────────
    # age is required: E-03 generates the cluster's backup key with it, and
    # there is no fallback. Without it the install finishes with no key and
    # every nightly export fails.
    local base_tools=(kubectl helm jq yq openssl curl bao age age-keygen)
    # Crossplane-based installer also needs the crossplane CLI and python3.
    local extra_tools=()
    [[ "${CROSSPLANE_MODE:-0}" == "1" ]] && extra_tools=(crossplane python3)

    local missing_age=0
    for cmd in "${base_tools[@]}" "${extra_tools[@]}"; do
        if ! command -v "$cmd" &>/dev/null; then
            error "Required command not found: $cmd"
            missing=$((missing + 1))
            case "$cmd" in age|age-keygen) missing_age=1 ;; esac
        else
            success "$cmd found"
        fi
    done
    # Named once for the pair: "age not found" reads like a missing convenience.
    if [[ ${missing_age} -eq 1 ]]; then
        error ""
        error "  age and age-keygen ship together and both are required."
        error "  They generate this cluster's backup key, which every scheduled"
        error "  export encrypts to. Without one, the install would finish and"
        error "  every nightly backup would fail with \"no age recipients configured\"."
        error "    Debian/Ubuntu   sudo apt install age"
        error "    macOS           brew install age"
        error "    Alpine          apk add age"
        error "    or              https://age-encryption.org"
        error ""
    fi

    # ── Optional tools ────────────────────────────────────────────────────────
    # Reported here rather than where they are used, because by the time the
    # fallback announces itself it is too late to act on.
    if command -v qrencode &>/dev/null; then
        success "qrencode found (optional)"
    else
        warn "Optional command not found: qrencode"
        warn "  The recovery kit prints the backup key as text either way; with"
        warn "  qrencode it also prints a QR code to keep on paper."
    fi

    # ── Cluster connectivity ──────────────────────────────────────────────────
    if ! kubectl cluster-info --request-timeout=5s >/dev/null 2>&1; then
        error "No reachable Kubernetes cluster — set KUBECONFIG or connect to your cluster first."
        missing=$((missing + 1))
    else
        success "cluster reachable (context: $(kubectl config current-context 2>/dev/null || echo unknown))"
        # These two HEAL rather than check: they delete orphaned webhooks and
        # re-reconcile failed Releases. Preflight runs before the --dry-run gate,
        # so without this guard `--dry-run` would mutate the cluster — which is
        # exactly the command someone runs to find out whether it is safe to.
        if [[ "${GENTIAN_DRY_RUN:-0}" == "1" ]]; then
            info "Dry run: skipping the self-heal hooks (they would mutate the cluster)."
        else
            cleanup_orphaned_kyverno_webhooks
            force_reconcile_failed_helm_releases
        fi
    fi

    # ── MicroK8s kubelet max-pods ─────────────────────────────────────────────
    # Gentian OS runs many pods (100+). The microk8s default of --max-pods=110
    # is too low and causes silent scheduling failures once the limit is reached.
    # If this is a microk8s cluster and the limit is below 220, increase it now
    # and restart microk8s so the new limit takes effect before any workloads
    # are deployed. This is idempotent.
    local kubelet_args_file="/var/snap/microk8s/current/args/kubelet"
    # Also a mutation, and a privileged one: it edits a root-owned file and
    # restarts microk8s. Same reasoning as the heal hooks above.
    if [[ "${GENTIAN_DRY_RUN:-0}" == "1" ]] && [[ -f "${kubelet_args_file}" ]]; then
        info "Dry run: skipping the microk8s max-pods adjustment."
    elif [[ -f "${kubelet_args_file}" ]]; then
        local cur_max_pods
        cur_max_pods=$(grep -E '^--max-pods=' "${kubelet_args_file}" | cut -d= -f2 || true)
        cur_max_pods=${cur_max_pods:-110}
        local target_max_pods=220
        if (( cur_max_pods < target_max_pods )); then
            info "microk8s kubelet max-pods=${cur_max_pods} is below ${target_max_pods}; updating to ${target_max_pods}..."
            local _kubelet_args_filtered
            _kubelet_args_filtered="$(sed '/^--max-pods=/d' "${kubelet_args_file}")"
            printf '%s\n' "${_kubelet_args_filtered}" | sudo tee "${kubelet_args_file}" >/dev/null
            echo "--max-pods=${target_max_pods}" | sudo tee -a "${kubelet_args_file}" >/dev/null
            info "Restarting microk8s to apply new max-pods limit (this takes ~30 s)..."
            sudo microk8s stop
            sudo microk8s start
            if kubectl wait node --all --for=condition=Ready --timeout=120s; then
                success "microk8s max-pods updated to ${target_max_pods} and cluster is Ready."
            else
                warn "Cluster not Ready after 120 s — continuing anyway."
            fi
        else
            success "microk8s max-pods=${cur_max_pods} (≥ ${target_max_pods}, ok)"
        fi
    fi

    # ── Required environment variables ───────────────────────────────────────
    # A dry run collects no credentials, and neither does a teardown, so their
    # absence is the expected state rather than a missing prerequisite. Counting
    # it as one aborts over values the run was never going to use — and on a
    # teardown that means refusing to remove a cluster because the operator no
    # longer has the password to the thing being removed.
    MAIL_SERVICE_MODE="$(gentian_mail_service_mode)"
    export MAIL_SERVICE_MODE

    if [[ "${GENTIAN_DRY_RUN:-0}" == "1" ]]; then
        info "Dry run: credential variables not checked (none were collected)."
    elif [[ "${GENTIAN_DIRECTION:-forward}" == "reverse" ]]; then
        info "Teardown: credential variables not checked (none were collected)."
    else
        if [[ -z "${MASTER_PASSWORD:-}" ]]; then
            error "MASTER_PASSWORD is not set"
            missing=$((missing + 1))
        else
            success "MASTER_PASSWORD set"
        fi

        # The relay credential is NOT a prerequisite. credentials.yaml declares
        # smtp-relay as phase: runtime and optional, which means the credential
        # manager supplies it once the cluster is up — so requiring it here made
        # the installer a second, stricter opinion about the same credential, and
        # the catalogue is the one that decides (§4).
        #
        # It aborted every external-mail install, which is the default. The
        # values used to arrive from install.secrets.env; nothing has loaded that
        # file since it was removed, so the check could no longer be satisfied by
        # the means it was written for.
        #
        # A cluster with no relay credential comes up and cannot send mail. That
        # is a reported gap, not a failed install: `make check-credentials` names
        # it, and the claim that needs it says so itself.
        if [[ "${MAIL_SERVICE_MODE}" == "external" ]]; then
            if [[ -n "${SMTP_RELAY_USERNAME:-}" && -n "${SMTP_RELAY_PASSWORD:-}" ]]; then
                success "SMTP relay credentials present; they will be seeded with the rest"
            else
                info "SMTP relay credentials not supplied — mail will not send until they are."
                info "  They are a runtime credential: supply them after the install with"
                info "  the credential manager. 'make check-credentials' lists what is open."
            fi
        else
            info "MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE}: relay credentials not used (in-cluster Postfix)"
        fi
    fi

    if ! mail_network_mode_compatible "${MAIL_SERVICE_MODE}" "${NETWORK_MODE:-tunnel}"; then
        error "$(mail_network_mode_incompatibility_message)"
        missing=$((missing + 1))
    fi

    # ── Operator image ────────────────────────────────────────────────────────
    # The tag the cluster will actually run. ArgoCD reconciles the chart from
    # clusters/<id>/kernel/values.yaml continuously, so that file wins over the
    # installer's --set: checking GENTIAN_OS_IMAGE_TAG alone would pass while
    # the cluster pulled something else.
    local _os_repo _os_tag _os_values
    _os_repo="${GENTIAN_OS_IMAGE_REPOSITORY:-ghcr.io/gentian-org/gentian-os}"
    _os_values="${GENTIAN_DEPLOYMENTS_PATH:-}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}/kernel/values.yaml"
    # yq_get, not a bare `yq`: mikefarah/yq takes `eval` and kislyuk/yq takes a
    # jq filter, and both ship as `yq`. Calling one syntax directly fails
    # silently here, and the fallback below would then validate a tag the
    # cluster is not going to pull — passing the check for the wrong image.
    _os_tag=""
    if [[ -r "${_os_values}" ]]; then
        _os_tag="$(yq_get '.image.tag' "${_os_values}" 2>/dev/null || true)"
    fi
    _os_tag="${_os_tag:-${GENTIAN_OS_IMAGE_TAG:-develop}}"
    if validate_image_tag "${_os_repo}" "${_os_tag}"; then
        success "Operator image ${_os_repo}:${_os_tag} exists"
    else
        error "  Set image.tag in ${_os_values} to a tag that exists."
        missing=$((missing + 1))
    fi

    if [[ "$missing" -gt 0 ]]; then
        error "$missing prerequisite(s) missing. Aborting."
        exit 1
    fi

    if [[ -z "${NODE_IP:-}" ]]; then
        NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
        info "Auto-detected NODE_IP: $NODE_IP"
    fi
    export NODE_IP

    # In static-ip mode NODE_IP must be a real, non-documentation address.
    if [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]]; then
        if _is_testnet_ip "${NODE_IP}"; then
            error "NETWORK_MODE=static-ip requires a real NODE_IP, but NODE_IP=${NODE_IP} looks like a documentation/testnet address."
            error "Set NODE_IP to the cluster node's actual public or reachable IP and re-run."
            exit 1
        fi
        # Check that the edge LoadBalancer service has an external IP.
        local lb_ip lb_label
        if [[ "${ROUTING_MODE:-gateway}" == "gateway" ]]; then
            lb_label='app.kubernetes.io/name=gateway-helm'
            lb_ip=$(kubectl get svc -A -l "${lb_label}" \
                -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        fi
        if [[ -z "$lb_ip" ]]; then
            warn "NETWORK_MODE=static-ip: Envoy Gateway LoadBalancer has no external IP yet."
            warn "  Make sure MetalLB (or a cloud LB) is configured with ${NODE_IP} before traffic can reach the cluster."
        else
            info "Gateway LoadBalancer external IP: ${lb_ip}"
        fi
    fi

    if [[ "${ROUTING_MODE:-gateway}" != "gateway" ]]; then
        error "ROUTING_MODE=${ROUTING_MODE} is no longer supported; use ROUTING_MODE=gateway."
        exit 1
    fi

    resolve_storage_class || exit 1

    success "All pre-flight checks passed."
}

# =============================================================================
# resolve_storage_class — settle on ONE StorageClass name for the whole install
#
# cluster-settings.env documents STORAGE_CLASS as "leave unset to use the
# cluster's own default StorageClass". That promise only holds if something
# actually resolves "unset" into a concrete name: kernel components are Helm
# charts fed from Git, and a chart cannot read the operator's shell. So resolve
# it here, once, and export it — bootstrap Applications pass the result down as
# a Helm parameter (see kernel/bootstrap/chart/templates/gentian-{os,portal}.yaml).
#
# Resolution order:
#   1. STORAGE_CLASS from cluster-settings.env / environment (explicit wins)
#   2. the cluster's default StorageClass (is-default-class annotation)
#   3. hard error — a silent fallback here surfaces much later as a PVC that
#      sits Pending forever, which is a far worse failure mode.
# =============================================================================
resolve_storage_class() {
    if [[ -n "${STORAGE_CLASS:-}" ]]; then
        if ! kubectl get storageclass "${STORAGE_CLASS}" &>/dev/null; then
            error "STORAGE_CLASS=${STORAGE_CLASS} does not exist on this cluster."
            error "  Available: $(kubectl get storageclass -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null)"
            error "  Fix storageClass in claims/cluster.yaml, or leave it unset to use the cluster default."
            return 1
        fi
        info "StorageClass: ${STORAGE_CLASS} (explicit, from claims/cluster.yaml)"
        export STORAGE_CLASS
        return 0
    fi

    local default_sc
    default_sc=$(kubectl get storageclass \
        -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{"\n"}{end}' \
        2>/dev/null | head -1)

    if [[ -z "${default_sc}" ]]; then
        error "STORAGE_CLASS is unset and this cluster has no default StorageClass."
        error "  Available: $(kubectl get storageclass -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null)"
        error "  Set storageClass in claims/cluster.yaml, or annotate one class"
        error "  with storageclass.kubernetes.io/is-default-class=true, and re-run."
        return 1
    fi

    STORAGE_CLASS="${default_sc}"
    export STORAGE_CLASS
    info "StorageClass: ${STORAGE_CLASS} (cluster default; STORAGE_CLASS unset)"
    return 0
}

# =============================================================================
# gentian_report_abort — say something before `set -e` kills the run
#
# These scripts run under `set -euo pipefail`, so any unguarded command that
# exits non-zero terminates the install immediately. When that command also had
# its stderr redirected — the common `kubectl ... 2>/dev/null` idiom — the run
# ends with no output whatsoever: the last thing printed is whatever INFO line
# preceded it, and the operator is left staring at a shell prompt with no clue
# which step failed or why. That happened at Step 10b, where a missing
# tunnel-credentials Secret made kubectl exit 1 inside a pipeline.
#
# Registered as an ERR trap by load.sh (which also sets -E so it fires inside
# functions). Failures that are already handled — `|| true`, `if cmd; then`,
# `cmd && ...` — do not trigger ERR, so this only speaks up for genuinely
# unhandled ones.
# =============================================================================
gentian_report_abort() {
    local exit_code=$?
    local cmd="${BASH_COMMAND:-<unknown>}"

    # ERR fires again at every enclosing frame as the failure unwinds, so one
    # failed command printed the banner once per nesting level — three times for
    # a failure three functions deep, each with a shorter stack, which reads like
    # three separate faults. Report only the first (innermost) occurrence, whose
    # stack is the complete one.
    if [[ -n "${_GENTIAN_ABORT_REPORTED:-}" ]]; then
        return "${exit_code}"
    fi
    _GENTIAN_ABORT_REPORTED=1

    # A bare `exit N` is a deliberate stop: the step that called it has already
    # printed its own diagnosis (which chart, which resource, what to run next).
    # Appending an ABORT banner to that only buries the useful message under a
    # generic one — and the frame it would name is wherever bash happened to be
    # unwinding, not where the problem is. Stay quiet and let the real error
    # stand.
    case "${cmd}" in
        exit|exit\ *|return|return\ *)
            # Still say which step stopped the run, and what re-running does.
            #
            # That is the one thing the step's own message cannot know, and it
            # used to come from the driver, which printed it after apply()
            # returned. apply() is called bare now (so that an unchecked
            # failure inside it actually stops the step rather than being
            # skipped over), which means errexit ends the run here and the
            # driver never gets to speak. Two lines, not the banner: the
            # step's own diagnosis is the useful part and stays on top.
            if [[ -n "${GENTIAN_CURRENT_STEP:-}" ]]; then
                echo "" >&2
                echo -e "\033[0;31m[ERROR]\033[0m Step ${GENTIAN_CURRENT_STEP} stopped the install (exit ${exit_code})." >&2
                echo "  Nothing after it has run. Fix the cause and re-run — steps that" >&2
                echo "  already succeeded report satisfied and are skipped." >&2
            fi
            return "${exit_code}" ;;
    esac

    # Walk the actual call stack rather than guessing one frame. Frame 0 is this
    # function; start at 1. This is what makes the report trustworthy — the
    # single-frame version pointed at whichever library was on the stack instead
    # of the failing command's own file.
    echo "" >&2
    echo -e "\033[0;31m[ABORT]\033[0m Install stopped: a command failed and was not handled." >&2
    # The driver sets GENTIAN_CURRENT_STEP around each step, so the report names
    # the step file to open rather than only the library frame that failed.
    [[ -n "${GENTIAN_CURRENT_STEP:-}" ]] && \
        echo "  step      : ${GENTIAN_CURRENT_STEP} (scripts/steps/${GENTIAN_CURRENT_STEP}.sh)" >&2
    echo "  exit code : ${exit_code}" >&2
    echo "  command   : ${cmd}" >&2
    echo "  call stack:" >&2
    local i=1
    while [[ -n "${BASH_SOURCE[i]:-}" ]]; do
        echo "    ${BASH_SOURCE[i]}:${BASH_LINENO[i-1]:-?}  in ${FUNCNAME[i]:-main}()" >&2
        i=$((i + 1))
    done
    echo "" >&2
    echo "  If the command above ends in 2>/dev/null its error text was" >&2
    echo "  suppressed — re-run it by hand without that redirect to see why." >&2
    return "${exit_code}"
}

# =============================================================================
# gentian_services_namespace — where the kernel SERVICES live
#
# Kernel services (the public Gateway, Keycloak, OpenFGA, the portal) live in
# platform-kernel. That is the operator's servicesNamespace, whose chart default
# is platform-kernel (charts/gentian-os/values.yaml) and which it uses to place
# the Gateway, so it is the authoritative value.
#
# The shell half used to default to "gentian-<env>" instead, so the two halves
# disagreed about which namespace was "services". That is what made the wildcard
# certificate land somewhere the Gateway could not see it, leaving the cluster
# serving nothing. Both halves now resolve to the same place.
#
# The mail namespace resolves to the same place — see gentian_mail_namespace
# below. It used to be deliberately different, and this line used to say so.
# =============================================================================
gentian_services_namespace() {
    echo "${SERVICES_NAMESPACE:-platform-kernel}"
}

# =============================================================================
# gentian_mail_namespace — where kernel Postfix/Dovecot live
#
# The services namespace, resolved identically to gentian_services_namespace
# above: kernel Postfix and Dovecot are kernel services and are deployed
# alongside the others.
#
# The two must agree, and agreeing is not enough — they have to be one
# resolution. When this returned gentian-<env> while the mail charts deployed
# into platform-kernel, D-04 looked for its own ConfigMap in an empty namespace
# and reported a working mail stack missing. The operator wrote the map to the
# services namespace and was right; the lookup was wrong.
#
# _mail_kernel_namespace in mail-lib.sh delegates here for that reason. One
# definition, so a future move cannot leave half the callers behind.
# =============================================================================
gentian_mail_namespace() {
    echo "${KERNEL_NAMESPACE:-${SERVICES_NAMESPACE:-platform-kernel}}"
}

# =============================================================================
# gentian_mail_service_mode — the cluster's mail stack, resolved once.
#
# kernel runs Postfix and Dovecot here with local mailboxes; external relays
# through a smarthost and has no Dovecot at all. Eighteen sites decided that by
# writing $(gentian_mail_service_mode), which is a second default beside the
# one the Cluster XRD declares — they agree only until the XRD moves, and then
# half the callers are left behind.
#
# That is not a hypothetical here. The claim said kernel while the operator,
# reading its own Helm value, said external: Dovecot ran unprovisioned and the
# ApplicationSet that would have managed it was never rendered, with git showing
# the correct answer the whole time.
#
# So the fallback is the XRD's, read from the schema. seed-openbao.sh keeps its
# literal deliberately — it is standalone, sources nothing, and takes everything
# from the environment, which is the "legitimately local" case rather than a
# second opinion.
# =============================================================================
gentian_mail_service_mode() {
    if [[ -n "${MAIL_SERVICE_MODE:-}" ]]; then
        printf '%s' "${MAIL_SERVICE_MODE}"
        return 0
    fi
    xrd_default mail.serviceMode
}

# =============================================================================
# gentian_cluster_derivation_salt — the salt this cluster's secrets were
# derived with, asked of the cluster.
#
# Every kernel credential is HMAC(MASTER_PASSWORD+DERIVATION_SALT, …), so the
# salt is not a preference an installer run may choose: once a cluster exists,
# it has exactly one, and a run that derives with any other computes passwords
# nothing on the cluster accepts.
#
# Read from the Secret create_crossplane_secrets writes, which is where the
# Cluster claim's masterPasswordSecretRef points. Deliberately not OpenBao:
# reading OpenBao needs a root token, and E-04 revokes the installer's at
# handover — which is precisely when a re-run most needs this answer.
#
# Prints nothing and returns 1 when the cluster has no salt yet (a first
# install), which is the one case where generating one is correct.
# =============================================================================
gentian_cluster_derivation_salt() {
    local salt
    salt="$(kubectl get secret gentian-os-master-password \
        -n "${CROSSPLANE_NAMESPACE:-crossplane-system}" \
        -o jsonpath='{.data.salt}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    [[ -n "${salt}" ]] || return 1
    printf '%s' "${salt}"
}

# =============================================================================
# gentian_kernel_namespaces — the namespaces the installer owns, in one place.
#
# A-03 checks this list and create_namespaces creates it. They used to be two
# hand-kept lists and had drifted apart in both directions: the check demanded
# gentian-infra-<stage>, which nothing created, so the step reported unsatisfied
# on every run forever while cheerfully announcing that all nine namespaces
# already existed; and gentian-<stage> was created by nothing at all, so the
# mail step failed applying a ConfigMap into a namespace that did not exist.
#
# The stage-scoped pair is deliberately here and not on the Cluster XR, which
# composes only gentian-system and platform-kernel. Two owners for one namespace
# is worse than one owner in the wrong phase.
# =============================================================================
# =============================================================================
# load_status_context — the configuration a read-only pass needs, and no more.
#
# --status skips prepare_run, because prepare_run prompts, writes ~/.gentian and
# scaffolds a deployments tree. But every check() that resolves a stage-scoped
# name, a claim name or a cluster id reads that same configuration, so without
# it they answer against defaults: A-03 asks for gentian-infra-dev on a prod
# cluster, C-02 compares against an empty cluster id, B-08 cannot name the claim
# to look up. Each then reports missing on a cluster where the thing is present,
# which is worse than not reporting at all — it is a wrong answer that looks
# like a right one.
#
# Loads files and derives names. Prompts for nothing, writes nothing.
# =============================================================================
load_status_context() {
    load_operator_config
    load_deployments_cluster_settings

    # Same derivation as prompt_app_repos, without the prompting: ENV is the
    # stage, and a dozen namespace and hostname lookups are built from it.
    ENV="${ENV:-${GENTIAN_DEPLOYMENTS_STAGE:-dev}}"
    export ENV
}

gentian_kernel_namespaces() {
    local ns seen=""
    for ns in openbao external-secrets argocd gentian-system platform-kernel \
              "${INFRA_NAMESPACE:-gentian-infra-${ENV:-dev}}" \
              "$(gentian_services_namespace)" \
              "$(gentian_mail_namespace)"; do
        # SERVICES_NAMESPACE defaults to platform-kernel, so the list can name
        # the same namespace twice.
        case " ${seen} " in *" ${ns} "*) continue ;; esac
        seen="${seen} ${ns}"
    done
    echo "${seen# }"
}

# =============================================================================
# gentian_cluster_claim_name — the Cluster claim's metadata.name for THIS cluster
#
# The name used to be the literal "dev-cluster" everywhere: the scaffolder wrote
# it, and the installer looked the object up by that same literal, in both
# directions. That
# is plainly wrong on any cluster that is not the original dev one — a prod
# cluster ends up owning a claim called "dev-cluster" — but it also cannot simply
# be recomputed, because clusters provisioned under the old name have a live
# XCluster called dev-cluster. Recomputing would make install/uninstall look for
# an object that does not exist and silently orphan the real one.
#
# So read it from the claim the scaffolder wrote: new clusters get
# <cluster>-<stage>, existing ones keep whatever their checked-in claim says.
# =============================================================================
gentian_claim_name() {
    local claim_file="$1" fallback="$2"
    local path="${GENTIAN_DEPLOYMENTS_PATH:-}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}/kernel/claims/${claim_file}.yaml"
    local name=""
    name=$(yq_get '.metadata.name' "${path}" 2>/dev/null || true)
    if [[ -n "${name}" ]]; then
        echo "${name}"
        return 0
    fi
    # Fall back to the historical literal: a missing or unreadable claim then
    # behaves exactly as before rather than targeting some other object.
    echo "${fallback}"
}

gentian_cluster_claim_name()  { gentian_claim_name cluster    dev-cluster;    }
gentian_infradata_claim_name() { gentian_claim_name infra-data dev-infra-data; }
gentian_suze_claim_name()      { gentian_claim_name suze       dev-suze;       }

# =============================================================================
# resolve_gentian_os_branch — the git ref every in-cluster Application tracks
# back to this repo.
#
# Exports GENTIAN_OS_BRANCH for apply_bootstrap_application, which passes it to
# the bootstrap chart as gentianOsBranch. Set GENTIAN_OS_BRANCH in install.env to
# pin a cluster to a branch or release tag; otherwise it follows the checkout the
# installer is running from, which is what a dev cluster wants.
#
# Detached HEAD returns the literal "HEAD" from rev-parse, which is not a ref
# ArgoCD can track — every Application would sit Unknown pointing at a revision
# that does not resolve. Treat it as "no branch" and fall back, the same as a
# missing .git.
# =============================================================================
resolve_gentian_os_branch() {
    if [[ -n "${GENTIAN_OS_BRANCH:-}" ]]; then
        export GENTIAN_OS_BRANCH
        return 0
    fi
    local detected
    detected="$(git -C "${SCRIPT_DIR}" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
    if [[ -z "${detected}" || "${detected}" == "HEAD" ]]; then
        detected="develop"
    fi
    export GENTIAN_OS_BRANCH="${detected}"
}

# =============================================================================
# apply_bootstrap_application — render one bootstrap Application from
# kernel/bootstrap/chart and kubectl apply it.
#
# Bootstrap Applications are applied by install.sh from the local checkout
# rather than read from Git by ArgoCD, which is exactly why per-cluster values
# (STORAGE_CLASS) reach them at all: they are the objects that install the agent
# that will read everything else from Git.
# =============================================================================
apply_bootstrap_application() {
    local name="$1"
    local chart="${SCRIPT_DIR}/kernel/bootstrap/chart"

    # One chart, one template per Application. Helm is what makes the Argo CD
    # multi-source "$values" references in these manifests safe to carry: $values
    # is not Helm syntax, so it survives rendering untouched with no allowlist of
    # substitutable names to keep in step with the manifests.
    # A name with no template is a caller naming something that does not exist —
    # a typo, or a template renamed without its callers. This used to fall
    # through to kernel/bootstrap/${name}-application.yaml, the pre-chart layout,
    # which has not existed for months: kubectl reported "no such file", the
    # caller announced "Applied ${name}-application.yaml" regardless, and the
    # Application was never created. Renaming cnpg-cluster.yaml to
    # kernel-admin.yaml walked straight into it.
    if [[ ! -f "${chart}/templates/${name}.yaml" ]]; then
        error "No bootstrap template ${chart}/templates/${name}.yaml for '${name}'."
        error "  Every name passed here must match a template in that directory."
        exit 1
    fi

    if [[ -z "${STORAGE_CLASS:-}" ]]; then
        error "STORAGE_CLASS is empty while rendering ${name}."
        error "  resolve_storage_class() should have set it during pre-flight."
        exit 1
    fi
    if [[ -z "${GENTIAN_DEPLOYMENTS_STAGE:-}" ]]; then
        error "GENTIAN_DEPLOYMENTS_STAGE is empty while rendering ${name}."
        exit 1
    fi
    # --show-only filters the OUTPUT; Helm still evaluates every template in
    # the chart. So the `required` on root-applicationset.yaml's cluster id
    # fires here too, even though this call renders a different file — and
    # the whole render fails, leaving kubectl with nothing on stdin.
    if [[ -z "${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}" ]]; then
        error "GENTIAN_DEPLOYMENTS_CLUSTER_ID is empty while rendering ${name}."
        error "  Set it in install.env; every chart template is evaluated on"
        error "  each render, and one of them requires it."
        exit 1
    fi
    resolve_gentian_os_branch

    # Rendered to a file rather than piped: a pipeline reports the exit
    # status of kubectl, so a failed render reached it as empty input and
    # was announced as a successful apply.
    local rendered; rendered="$(mktemp)"
    if ! helm template gentian-bootstrap "${chart}" -s "templates/${name}.yaml" \
            -f "${SCRIPT_DIR}/kernel/platforms.yaml" \
            --set-string "gentianOsBranch=${GENTIAN_OS_BRANCH}" \
            --set-string "storageClass=${STORAGE_CLASS}" \
            --set-string "stage=${GENTIAN_DEPLOYMENTS_STAGE}" \
            --set-string "kernelDomain=${KERNEL_DOMAIN:-}" \
            --set-string "dnsProvider=${DNS_PROVIDER:-cloudflare}" \
            --set-string "cluster=${GENTIAN_DEPLOYMENTS_CLUSTER_ID}" >"${rendered}"; then
        rm -f "${rendered}"
        error "Rendering ${name} failed; nothing was applied."
        exit 1
    fi
    # An empty render is a decision for some templates and a failure for the
    # rest. external-dns emits nothing when the cluster has no DNS provider,
    # which is the correct shape — a controller with no provider would
    # authenticate-fail every interval and never write a record.
    if [[ ! -s "${rendered}" ]]; then
        rm -f "${rendered}"
        if [[ "${name}" == "external-dns" ]]; then
            info "No DNS provider for this cluster; external-dns not installed."
            return 0
        fi
        error "Rendering ${name} produced nothing; nothing was applied."
        exit 1
    fi
    kubectl apply -f "${rendered}" || {
        rm -f "${rendered}"
        error "Applying ${name} failed."
        exit 1
    }
    rm -f "${rendered}"
}

# =============================================================================
# yq_get — read a YAML field, tolerant of either yq flavor (mikefarah/yq's
# "eval" subcommand, or kislyuk/yq's jq-style filter) since both are seen in
# the wild as `yq`. Echoes the value and returns 0, or returns 1 (nothing
# echoed) if the field is absent/null or the file doesn't exist.
# =============================================================================
yq_get() {
    local filter="$1" file="$2"
    [[ -f "${file}" ]] || return 1
    local out
    if out=$(yq eval "${filter}" "${file}" 2>/dev/null) && [[ -n "${out}" && "${out}" != "null" ]]; then
        echo "${out}"
        return 0
    fi
    if out=$(yq -r "${filter}" "${file}" 2>/dev/null) && [[ -n "${out}" && "${out}" != "null" ]]; then
        echo "${out}"
        return 0
    fi
    return 1
}

# =============================================================================
# resolve_kernel_domain_from_claim — an already-bootstrapped cluster's
# KERNEL_DOMAIN lives solely in its Crossplane Cluster claim
# (gentian-deployments/clusters/<cluster>/kernel/claims/cluster.yaml), not in
# cluster-settings.env. Read it from there. No-op (KERNEL_DOMAIN stays unset)
# for a brand-new cluster that has no claim file yet — install.sh's
# prompt_kernel_domain/scaffold_cluster_deployment handle that case.
# =============================================================================
resolve_kernel_domain_from_claim() {
    # Both defaulted, because this runs under `set -u` and is documented to be
    # a no-op when there is no claim to read. Dereferenced bare, an unset
    # cluster id aborted the whole run with "GENTIAN_DEPLOYMENTS_CLUSTER_ID:
    # unbound variable" — which is what --validate did outside a configured
    # checkout, the one place the no-op was most obviously the intent.
    local claim_file="${GENTIAN_DEPLOYMENTS_PATH:-}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}/kernel/claims/cluster.yaml"
    [[ -n "${KERNEL_DOMAIN:-}" ]] && return 0

    local domain
    if domain=$(yq_get '.spec.kernelDomain' "${claim_file}"); then
        export KERNEL_DOMAIN="${domain}"
        info "KERNEL_DOMAIN=${KERNEL_DOMAIN} (read from ${claim_file})"
    fi
}


# =============================================================================
# Crossplane platform compositions (gentian-os only)
# =============================================================================
# Generic app-default and tenant/cluster compositions live in gentian-os.
# Profile-specific compositions are synced from gentian-apps via the
# catalogue-sync ApplicationSet the gentian-apps Repository claim composes
# (see install_catalogue_sync).

apply_crossplane_app_compositions() {
    local comp_dir="${SCRIPT_DIR}/crossplane/compositions"
    info "Applying Composition app-default..."
    kubectl apply -f "${comp_dir}/app-default.yaml"
}

apply_crossplane_platform_compositions() {
    info "Applying Composition (cluster-default)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/compositions/cluster-default.yaml"
    apply_crossplane_app_compositions
    info "Applying Composition (tenant-default)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/compositions/tenant-default.yaml"
}

apply_crossplane_platform_compositions_update() {
    local f
    shopt -s nullglob
    for f in "${SCRIPT_DIR}"/crossplane/compositions/*.yaml; do
        info "Applying Composition $(basename "${f}")..."
        kubectl apply -f "${f}"
    done
    shopt -u nullglob
}

# =============================================================================
# 1. Create namespaces (idempotent)
# =============================================================================
create_namespaces() {
    banner "Creating namespaces"

    # The same list A-03's check() verifies, so "all namespaces already exist"
    # and "not satisfied" can no longer both be true.
    local namespaces=()
    for ns in $(gentian_kernel_namespaces); do namespaces+=("$ns"); done
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        namespaces+=(stakater-system cnpg-system cert-manager)
        if [[ "${ROUTING_MODE:-gateway}" == "gateway" ]]; then
            namespaces+=("${ENVOY_GATEWAY_NAMESPACE}")
        fi
    fi

    for ns in "${namespaces[@]}"; do
        if kubectl get namespace "$ns" &>/dev/null; then
            success "Namespace $ns already exists."
        else
            _kubectl_retry create namespace "$ns"
            success "Namespace $ns created."
        fi
    done
}

# =============================================================================
# 1b. Pre-warm cluster (distro-agnostic PLEG/CRI-race mitigation)
# =============================================================================
# On a freshly-bootstrapped cluster, the very first workload pod often races
# against kubelet's Pod Lifecycle Event Generator (PLEG) and containerd's
# event-subscription init. Symptom: pod stuck in ContainerCreating with no IP
# even though containerd has the sandbox RUNNING and CNI assigned an address.
# This is well-known across containerd-based distros (kind, k3d, microk8s,
# kubeadm) and is what install.sh's auto-recovery was designed to clean up
# after the fact.
#
# Pre-warming with a throwaway pod forces one full successful pod-create cycle
# through kubelet+containerd+CNI BEFORE any real workload is deployed. By the
# time cert-manager / ESO / ArgoCD pods land, the kubelet<->containerd event
# pipe is fully wired and the race is gone.
#
# This is the same trick kind/k3d use internally and is purely kubectl-based,
# so it works on any conformant cluster.
prewarm_cluster() {
    if [[ "${SKIP_PREWARM:-0}" == "1" ]]; then
        warn "Pre-warm disabled via SKIP_PREWARM=1; skipping."
        return
    fi

    banner "Pre-warming cluster (PLEG/CRI race mitigation)"

    # We pre-warm TWO things, in order, because they are independent races:
    #
    #   (a) volumeless pod  → exercises kubelet ↔ containerd ↔ CNI event pipe
    #                         (fixes the classic PLEG/CRI race that kind/k3d
    #                         work around the same way).
    #   (b) pod + hostpath PVC → exercises kubelet's volume_manager + the
    #                         microk8s.io/hostpath provisioner. On microk8s
    #                         specifically, the FIRST pod to bind a hostpath
    #                         PVC after a fresh cluster / kubelite restart
    #                         frequently wedges in ContainerCreating with
    #                         the container `Started` but PodReadyToStart-
    #                         Containers stuck False (kubelet status-sync
    #                         hang on first volume attach). Burning that
    #                         race on a throwaway pod here means
    #                         openbao-transit-0 (the first real PVC
    #                         consumer) lands cleanly.
    #
    # Both pre-warm pods get auto-recovery if they wedge, so worst case we
    # spend ~60s on cleanup at install time instead of 240s of
    # ContainerCreating later.

    local stamp pod_no_vol pod_pvc pvc_name
    stamp="$(date +%s)"
    pod_no_vol="prewarm-${stamp}"
    pod_pvc="prewarm-pvc-${stamp}"
    pvc_name="prewarm-pvc-${stamp}"

    # ── (a) volumeless pre-warm ─────────────────────────────────────────────
    info "Applying throwaway pod kube-system/${pod_no_vol}..."
    kubectl apply -f - <<EOF >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_no_vol}
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: gentian-install
    app.kubernetes.io/component: prewarm
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: prewarm
      image: busybox:1.36
      imagePullPolicy: IfNotPresent
      command: ["sh","-c","exit 0"]
      resources:
        requests: {cpu: "10m", memory: "8Mi"}
        limits:   {cpu: "50m", memory: "32Mi"}
EOF
    _wait_prewarm_pod "${pod_no_vol}" 180 "volumeless"

    # ── (b) PVC-backed pre-warm ─────────────────────────────────────────────
    # The volume_manager race we're mitigating is specific to node-local
    # provisioners (microk8s.io/hostpath, rancher.io/local-path, etc.):
    # the FIRST pod to bind one of these PVCs on a freshly-started kubelet
    # wedges in ContainerCreating because kubelet's volume_manager lags
    # the CRI sandbox. Network provisioners like nfs.csi.k8s.io do NOT
    # exhibit this — they use a separate attach/mount path. So we do NOT
    # prewarm the *default* SC; we prewarm every hostpath/local-path SC
    # we can find (typically just one, microk8s-hostpath). On clusters
    # that have no such SC (pure NFS / cloud CSI), we skip silently.
    local racy_scs sc
    racy_scs=$(kubectl get sc -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.provisioner}{"\n"}{end}' 2>/dev/null \
        | awk -F= 'tolower($2) ~ /hostpath|local-path/ {print $1}')
    if [[ -z "${racy_scs}" ]]; then
        info "No hostpath/local-path StorageClass detected; skipping PVC pre-warm."
    else
        local idx=0
        while IFS= read -r sc; do
            [[ -z "${sc}" ]] && continue
            idx=$((idx + 1))
            local pod_n="${pod_pvc}-${idx}"
            local pvc_n="${pvc_name}-${idx}"
            info "Applying throwaway PVC + pod kube-system/${pod_n} (sc=${sc})..."
            kubectl apply -f - <<EOF >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${pvc_n}
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: gentian-install
    app.kubernetes.io/component: prewarm
spec:
  storageClassName: ${sc}
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 16Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_n}
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: gentian-install
    app.kubernetes.io/component: prewarm
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: prewarm
      image: busybox:1.36
      imagePullPolicy: IfNotPresent
      command: ["sh","-c","echo ok > /data/ok && exit 0"]
      volumeMounts:
        - name: data
          mountPath: /data
      resources:
        requests: {cpu: "10m", memory: "8Mi"}
        limits:   {cpu: "50m", memory: "32Mi"}
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${pvc_n}
EOF
            _wait_prewarm_pod "${pod_n}" 240 "PVC-backed (${sc})"
            kubectl delete pvc -n kube-system "${pvc_n}" --grace-period=1 --wait=false >/dev/null 2>&1 || true
        done <<<"${racy_scs}"
    fi
}

# Helper for prewarm_cluster: wait for ${1} pod in kube-system to reach a
# terminal phase (Succeeded/Failed). Auto-recovers via cri_cleanup +
# kubelite_restart if it wedges in ContainerCreating with no IP. Always
# best-effort: caller continues regardless.
_wait_prewarm_pod() {
    local pod="$1" timeout="${2:-180}" label="${3:-prewarm}"
    info "Waiting for ${label} pre-warm pod to reach a terminal phase (up to ${timeout}s)..."
    local deadline=$((SECONDS + timeout))
    local phase="" stuck_for=0 last_phase="" recovery_done=0
    while (( SECONDS < deadline )); do
        phase=$(kubectl get pod -n kube-system "${pod}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        case "${phase}" in
            Succeeded) success "${label} pre-warm pod completed."; break ;;
            Failed)    warn "${label} pre-warm pod failed (phase=Failed); continuing anyway."; break ;;
        esac
        # Track ContainerCreating wedge identical to wait_for_running_pod.
        local pod_ip
        pod_ip=$(kubectl get pod -n kube-system "${pod}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
        if [[ "${phase}" == "Pending" && -z "${pod_ip}" ]]; then
            if [[ "${phase}" == "${last_phase}" ]]; then
                stuck_for=$(( stuck_for + 3 ))
            else
                stuck_for=0
            fi
        else
            stuck_for=0
        fi
        last_phase="${phase}"
        if (( stuck_for >= 90 && recovery_done == 0 )); then
            warn "${label} pre-warm pod wedged in ContainerCreating with no IP after ${stuck_for}s — running recovery."
            kubectl delete pod -n kube-system "${pod}" --grace-period=0 --force >/dev/null 2>&1 || true
            cri_cleanup
            kubelite_restart
            recovery_done=1
            stuck_for=0
            # Re-apply the same pod so the wait loop has something to poll.
            # The owning controller (none — bare Pod) won't recreate it.
            # We bail out instead and rely on the next prewarm step / real
            # workload, which now lands on a freshly-restarted kubelite.
            warn "${label} pre-warm pod recovery issued; not re-creating bare pod. Continuing."
            return 0
        fi
        sleep 3
    done
    if [[ "${phase}" != "Succeeded" && "${phase}" != "Failed" ]]; then
        warn "${label} pre-warm pod did not finish within ${timeout}s (last phase: ${phase:-unknown})."
        kubectl describe pod -n kube-system "${pod}" 2>/dev/null | tail -20 || true
    fi
    kubectl delete pod -n kube-system "${pod}" --grace-period=1 --wait=false >/dev/null 2>&1 || true
}

save_install_state() {
    [[ "${INSTALL_STATE_FILE}" == "/dev/null" ]] && return 0
    local tmp
    local val
    tmp="$(mktemp)"
    {
        echo "# Auto-generated by install.sh — installer-local state only."
        echo "# Cluster runtime settings: gentian-deployments/clusters/<cluster>/kernel/claims/cluster.yaml"
        echo "# Delete this file to reset installer-local caches."
        val="${GENTIAN_MANAGED_CERT_MANAGER:-}"
        [[ -n "$val" ]] && printf 'export GENTIAN_MANAGED_CERT_MANAGER=%q\n' "$val"
        val="${INSTALL_START_EPOCH:-}"
        [[ -n "$val" ]] && printf 'export INSTALL_START_EPOCH=%q\n' "$val"
    } >"$tmp"
    install -m 0644 "$tmp" "${INSTALL_STATE_FILE}"
    rm -f "$tmp"
    info "Saved installer state to ${INSTALL_STATE_FILE}."
}
