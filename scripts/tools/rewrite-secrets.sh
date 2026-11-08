#!/usr/bin/env bash
# =============================================================================
# scripts/tools/rewrite-secrets.sh — write every Secret again, so etcd holds it
# encrypted.
# =============================================================================
# Turning on an encryption provider at the API server encrypts what is written
# from then on. Every Secret written before stays in etcd exactly as it was,
# plain base64, until something writes it again -- and nothing does on its own.
# This tool is that something: it reads each Secret and replaces it with itself,
# and the API server encrypts it on the way in. Nothing about the Secret
# changes but its resourceVersion.
#
#   scripts/tools/rewrite-secrets.sh              lists what it would rewrite
#   scripts/tools/rewrite-secrets.sh --apply      rewrites
#   scripts/tools/rewrite-secrets.sh --namespace <ns> [--apply]
#
# Run it after the API server restarted with the provider, never before: a
# rewrite under a server that does not encrypt changes nothing. Whether the
# server does is what pre-flight reports (check_secrets_at_rest). Verifying the
# result means reading etcd, which only the control plane's operator can do:
#   etcdctl get /registry/secrets/<ns>/<name> | hexdump -C | head
# shows k8s:enc:aescbc:v1: (or the provider's prefix) and not the plain value.
#
# Service-account token Secrets are rewritten like any other; the controller
# that owns them accepts an unchanged object. A Secret that changes between
# the read and the write is reported and left; run again.
# =============================================================================
set -euo pipefail

apply=0
ns_opt=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --apply) apply=1 ;;
        --namespace|-n) ns_opt=(--namespace "$2"); shift ;;
        -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
    shift
done
[[ ${#ns_opt[@]} -eq 0 ]] && ns_opt=(--all-namespaces)

for tool in kubectl jq; do
    command -v "${tool}" >/dev/null 2>&1 || { echo "${tool} is required" >&2; exit 2; }
done

list="$(kubectl get secrets "${ns_opt[@]}" -o json | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name)"')"
total="$(printf '%s\n' "${list}" | grep -c . || true)"
if [[ ${apply} -eq 0 ]]; then
    printf '%s\n' "${list}"
    echo
    echo "${total} Secret(s) would be rewritten. Nothing was changed; add --apply to rewrite."
    exit 0
fi

done_n=0; skipped=0
while IFS=/ read -r ns name; do
    [[ -n "${ns}" && -n "${name}" ]] || continue
    # kubectl replace sends the object as read, resourceVersion included, so a
    # Secret written by somebody else in between is refused rather than
    # overwritten; it is listed and left for a second run.
    if kubectl get secret -n "${ns}" "${name}" -o json 2>/dev/null \
        | kubectl replace -f - >/dev/null 2>&1; then
        done_n=$((done_n + 1))
    else
        echo "skipped ${ns}/${name}: changed, or gone, between the read and the write"
        skipped=$((skipped + 1))
    fi
done <<< "${list}"
echo "${done_n} Secret(s) rewritten, ${skipped} skipped, of ${total}."
[[ ${skipped} -eq 0 ]]
