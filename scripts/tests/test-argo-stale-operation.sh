#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-argo-stale-operation.sh
# =============================================================================
# Argo CD copies an Application's sources into an automated sync operation and
# retries that copy. D-01 moved gentian-os to a new operator build while such
# an operation ran on the build before, which could not succeed: the step
# waited out every retry of the old build first.
#
# Held here, against a stand-in kubectl, for request_argo_sync_if_stalled:
#
#   - an automated operation Running on sources the Application no longer
#     has is stopped (multi-source and single-source);
#   - one Running on the sources the Application has is left alone, also when
#     Argo CD's copy carries the nulls a merge left behind;
#   - one a person asked for is left alone, however it differs;
#   - one whose copy names no sources is left alone.
#
# No cluster.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

command -v jq >/dev/null || { echo "jq required"; exit 1; }

echo ""
echo "argo stale operation: an automated sync on sources the Application no longer has is stopped"
echo ""

SB="$(mktemp -d "${TMPDIR:-/tmp}/gto.XXXXXX")"
trap 'rm -rf "${SB}"' EXIT
mkdir -p "${SB}/bin"

# get application <name> -o json prints ${STATE}/app.json; every call is logged.
cat >"${SB}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >> "${STATE}/calls.log"
[[ "${1:-}" == "get" ]] && cat "${STATE}/app.json"
exit 0
STUB
chmod +x "${SB}/bin/kubectl"

# The sources gentian-os had during the D-01 run: a chart at a revision with an
# image tag, and the deployments repo as a values ref.
src() { # <revision> <tag>
    jq -n --arg rev "$1" --arg tag "$2" '[
      {repoURL: "https://github.com/gentian-org/gentian-os", path: "charts/gentian-os",
       targetRevision: $rev,
       helm: {valueFiles: ["$deploy/profiles/_base.yaml"],
              valuesObject: {image: {repository: "ghcr.io/gentian-org/gentian-os", tag: $tag}}}},
      {repoURL: "https://github.com/aluvian-hq/aluvian-deployments", targetRevision: "main", ref: "deploy"}]'
}
# Argo CD's copy of a merged spec: the ref source remembers a removed value as null.
with_null() { jq '.[1].helm = {valuesObject: {image: null}}'; }

app() { # <spec sources> <operation sources|null> <initiatedBy>
    jq -n --argjson spec "$1" --argjson ran "$2" --argjson by "$3" '{
      spec: {sources: $spec},
      status: {sync: {status: "OutOfSync"},
               operationState: {phase: "Running",
                                operation: ({initiatedBy: $by, retry: {limit: 5}}
                                            + (if $ran == null then {sync: {}} else {sync: {sources: $ran}} end))}}}'
}

run_case() { # <label> <app json> <expect: stop|leave>
    local st="${SB}/$RANDOM"; mkdir -p "${st}"
    printf '%s' "$2" >"${st}/app.json"
    STATE="${st}" PATH="${SB}/bin:${PATH}" bash -c '
        info() { :; }; warn() { :; }; success() { :; }; error() { :; }
        source scripts/lib/argocd.sh
        request_argo_sync_if_stalled kernel-gitops gentian-os' >/dev/null 2>&1
    if grep -q '"Terminating"' "${st}/calls.log" 2>/dev/null; then got=stop; else got=leave; fi
    if [[ "${got}" == "$3" ]]; then ok "$1"; else bad "$1 (expected $3, got ${got})"; fi
}

new="$(src 0355c5708255d897a9717c945873d0ba6a7ec049 develop-0355c57)"
old="$(src develop develop-7248b59)"
auto='{"automated":true}'
person='{"username":"admin"}'

run_case "automated, older revision and build: stopped" \
    "$(app "${new}" "$(with_null <<<"${old}")" "${auto}")" stop
run_case "automated, same sources with a merge's nulls: left alone" \
    "$(app "${new}" "$(with_null <<<"${new}")" "${auto}")" leave
run_case "automated, same sources: left alone" \
    "$(app "${new}" "${new}" "${auto}")" leave
run_case "asked for by a person, older sources: left alone" \
    "$(app "${new}" "${old}" "${person}")" leave
run_case "automated, the copy names no sources: left alone" \
    "$(app "${new}" null "${auto}")" leave

single="$(jq -n '{spec: {source: {repoURL: "r", path: "p", targetRevision: "abc"}},
    status: {sync: {status: "OutOfSync"}, operationState: {phase: "Running",
      operation: {initiatedBy: {automated: true}, sync: {source: {repoURL: "r", path: "p", targetRevision: "develop"}}}}}}')"
run_case "single source, automated, older revision: stopped" "${single}" stop
run_case "single source, automated, same revision: left alone" \
    "$(jq '.status.operationState.operation.sync.source.targetRevision = "abc"' <<<"${single}")" leave

finish
