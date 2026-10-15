#!/usr/bin/env bash
# =============================================================================
# scripts/lib/openbao-curl.sh — curl to the vault, verified the way bao is
# =============================================================================
# openbao_cli_trust (openbao.sh) points this shell's bao at the vault's CA
# openbao-ca: BAO_CACERT, and BAO_TLS_SERVER_NAME for the name the serving
# certificate carries, because the address the installer reaches the vault
# at is a ClusterIP or a port-forward on 127.0.0.1, neither of which is in
# the certificate. curl has no server-name override, so bao_curl asks for
# the certificate's name and connects it to the address instead.
#
# Without BAO_CACERT -- only before B-01 has made the CA, when nothing
# answers on the vault's address anyway -- it does not verify, as before.
#
# Its own file, with nothing but the environment to go on, so that
# scripts/bootstrap/seed-openbao.sh, which runs as a process of its own,
# can source it too.
# =============================================================================

# bao_curl <curl arguments...> — curl, with every https:// argument verified
# against BAO_CACERT under the name BAO_TLS_SERVER_NAME.
bao_curl() {
    # A step's check runs before anything in the shell has asked for the CA.
    if [[ -z "${BAO_CACERT:-}" ]] && declare -F openbao_cli_trust >/dev/null; then
        openbao_cli_trust 2>/dev/null
    fi
    if [[ -z "${BAO_CACERT:-}" || -z "${BAO_TLS_SERVER_NAME:-}" ]]; then
        curl -k "$@"
        return
    fi
    local a hostport port rest args=()
    for a in "$@"; do
        if [[ "${a}" == https://* ]]; then
            hostport="${a#https://}"
            rest="${hostport#"${hostport%%/*}"}"
            hostport="${hostport%%/*}"
            port="${hostport##*:}"
            [[ "${port}" == "${hostport}" || "${port}" == *"]"* ]] && port=443 && hostport="${hostport}:443"
            args+=(--connect-to "${BAO_TLS_SERVER_NAME}:${port}:${hostport}"
                "https://${BAO_TLS_SERVER_NAME}:${port}${rest}")
            continue
        fi
        args+=("${a}")
    done
    curl --cacert "${BAO_CACERT}" "${args[@]}"
}
