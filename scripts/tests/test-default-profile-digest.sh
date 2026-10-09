#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-default-profile-digest.sh
# =============================================================================
# Step 0 places one profile before there is a director to fetch it: the
# Operations Console, from the store's catalogue (or what
# GENTIAN_DEFAULT_PROFILES lists). It used to write whatever the address
# served. It now writes a file only when it hashes to a digest somebody
# stated -- the catalogue's index, or a pin the person installing gave -- and
# writes it the way the director writes an install (AD-14).
#
# Held here against a stand-in catalogue (curl is a stand-in that serves a
# directory; no network):
#
#   - the index's digest matches: placed, with its bundle and origin recorded;
#   - the bytes do not hash to it: the step stops, and nothing was written;
#   - the catalogue cannot be reached: a warning, nothing written, and the
#     install goes on;
#   - a pin (@sha256:...) is what is required, whatever the index says;
#   - an index that does not list the entry: stops, unless pinned;
#   - a definition that already holds another build is kept and reported, and
#     nothing is fetched over it;
#   - a local file or an http address is refused, with what to do instead;
#   - a bundle with companions is written whole, and one that holds a kind no
#     bundle may hold is refused;
#   - --dry-run says what an install would do and asks no catalogue;
#   - the digest and where it came from are in the signed commit.
#
# That what is written is byte for byte what the director writes is held by
# internal/director/gitops/installer_profiles_test.go.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }

# Short, for the keyring the last check makes: gpg-agent's socket path has a
# length limit.
SB="$(mktemp -d "${TMPDIR:-/tmp}/gdp.XXXXXX")"
GPGHOME="${SB}/g"
trap 'gpgconf --homedir "${GPGHOME}" --kill all >/dev/null 2>&1; rm -rf "${SB}"' EXIT
BIN="${SB}/bin"; mkdir -p "${BIN}" "${SB}/home"

# curl, as far as the catalogue fetch uses it: https://cat.example.test/<path>
# is ${CATALOGUE}/<path>. A file that is not there is a 404; a catalogue with
# a file named .down does not answer at all; one named .broken answers 503.
# Every address asked is logged.
cat > "${BIN}/curl" <<'STUB'
#!/usr/bin/env bash
out=""; fmt=""; url=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o) out="$2"; shift ;;
        -w) fmt="$2"; shift ;;
        --proto|--max-time|--max-filesize) shift ;;
        -*) ;;
        *) url="$1" ;;
    esac
    shift
done
echo "${url}" >> "${CURL_LOG}"
[[ "${url}" == https://* ]] || exit 1
[[ -e "${CATALOGUE}/.down" ]] && exit 7
code=200
file="${CATALOGUE}/${url#https://cat.example.test/}"
if [[ -e "${CATALOGUE}/.broken" ]]; then code=503
elif [[ ! -f "${file}" ]]; then code=404
fi
if [[ "${code}" == "200" ]]; then cat "${file}" > "${out}"; else printf 'no\n' > "${out}"; fi
[[ "${fmt}" == *http_code* ]] && printf '%s' "${code}"
exit 0
STUB
chmod +x "${BIN}/curl"

CLUSTER="sandbox"
BASE="https://cat.example.test"
URL="${BASE}/profiles/operations-console.yaml"
EMPTY_KUSTOMIZATION=$'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []'

profile() {   # profile <name> <version>
    printf 'apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: %s\nspec:\n  classes: [app]\n  trustTier: platform\n  version: "%s"\n' "$1" "$2"
}
sha() { sha256sum "$1" | cut -d' ' -f1; }

# new_world -- an empty deployments checkout and an empty catalogue.
new_world() {
    WORLD="$(mktemp -d "${SB}/w.XXXXXX")"
    CATALOGUE="${WORLD}/catalogue"; CHECKOUT="${WORLD}/deployments"; CURL_LOG="${WORLD}/curl.log"
    DIR="${CHECKOUT}/clusters/${CLUSTER}/catalogue"
    mkdir -p "${CATALOGUE}/profiles" "${CHECKOUT}/clusters/${CLUSTER}/kernel/claims"
    : > "${CURL_LOG}"
}
# publish <name> <version> [digest the index states; default: the file's own]
publish() {
    profile "$1" "$2" > "${CATALOGUE}/profiles/$1.yaml"
    printf 'entries:\n- name: %s\n  version: "%s"\n  edition: pe\n  digest: sha256:%s\n' \
        "$1" "$2" "${3:-$(sha "${CATALOGUE}/profiles/$1.yaml")}" > "${CATALOGUE}/index.yaml"
}
# run <shell> [VAR=value ...] -- the library in a fresh shell, as the
# installer loads it and under the options it runs with.
run() {
    local script="$1"; shift
    # shellcheck disable=SC2016 # the inner script is for the child shell to expand
    env -i HOME="${SB}/home" PATH="${BIN}:${PATH}" SCRIPT_DIR="${REPO}" CATALOGUE="${CATALOGUE}" CURL_LOG="${CURL_LOG}" \
        GENTIAN_DEPLOYMENTS_PATH="${CHECKOUT}" GENTIAN_DEPLOYMENTS_CLUSTER_ID="${CLUSTER}" KERNEL_DOMAIN="k.example.test" \
        GENTIAN_STORE_CATALOGUE_URL="${BASE}" SCRIPT="${script}" "$@" \
        bash -c 'source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1; trap - ERR; set +e; eval "${SCRIPT}"' 2>&1 < /dev/null
}
# shellcheck disable=SC2016 # literal text, not for this shell to expand
scaffold() { run '_scaffold_default_profiles '"${CLUSTER}"'; echo "rc=$?"; printf "%s" "${_GENTIAN_DEFAULT_PROFILE_NOTES}"' "$@"; }
tree() { (cd "${CHECKOUT}" && find . -type f | sort | while IFS= read -r f; do printf '%s %s\n' "$(sha "${f}")" "${f}"; done); }
nothing_written() { [[ ! -e "${DIR}/operations-console.yaml" && ! -e "${DIR}/operations-console.bundle.yaml" && "$(cat "${DIR}/kustomization.yaml")" == "${EMPTY_KUSTOMIZATION}" ]]; }
fetched() { grep -qxF "$1" "${CURL_LOG}"; }
carried() { sed -n 's/^    gentianos.io\/profile-bundle: "\(.*\)"$/\1/p' "$1" | openssl base64 -d -A; }

echo ""
echo "The default profile is placed only at a digest somebody stated"
echo ""

# --- the index's digest matches ----------------------------------------------
new_world; publish operations-console 1.0.0
DIGEST="sha256:$(sha "${CATALOGUE}/profiles/operations-console.yaml")"
out="$(scaffold)"
if [[ "${out}" == *"rc=0"* ]] && cmp -s "${CATALOGUE}/profiles/operations-console.yaml" "${DIR}/operations-console.yaml"; then
    ok "the file hashes to the digest the index lists: placed, byte for byte as served"
else
    bad "the file hashes to the digest the index lists: placed, byte for byte as served" "${out}"
fi
if carried "${DIR}/operations-console.bundle.yaml" | cmp -s - "${CATALOGUE}/profiles/operations-console.yaml" \
    && grep -qx '    gentianos.io/catalogue-origin: cluster/cat-example-test' "${DIR}/operations-console.bundle.yaml"; then
    ok "beside it the bundle: the same bytes, and the catalogue it came from as its origin"
else
    bad "beside it the bundle: the same bytes, and the catalogue it came from as its origin" "$(cat "${DIR}/operations-console.bundle.yaml" 2>&1)"
fi
if [[ "$(cat "${DIR}/kustomization.yaml")" == $'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- operations-console.yaml\npatches:\n- path: operations-console.bundle.yaml' ]]; then
    ok "the kustomization lists the profile as a resource and its bundle as a patch"
else
    bad "the kustomization lists the profile as a resource and its bundle as a patch" "$(cat "${DIR}/kustomization.yaml")"
fi
if [[ "${out}" == *"Default profile operations-console: ${DIGEST}, listed by the index of ${BASE}."* && "${out}" == *"Fetched from ${URL}; recorded as cluster/cat-example-test."* ]]; then
    ok "what goes into the commit says the digest, that the index stated it, the address and the origin"
else
    bad "what goes into the commit says the digest, that the index stated it, the address and the origin" "${out}"
fi
if fetched "${BASE}/index.yaml" && fetched "${URL}" && [[ "$(wc -l < "${CURL_LOG}" | tr -d ' ')" == "2" ]]; then
    ok "two requests: the index, then the profile"
else
    bad "two requests: the index, then the profile" "$(cat "${CURL_LOG}")"
fi

# A second run.
before="$(tree)"; : > "${CURL_LOG}"
out="$(scaffold)"
if [[ "${out}" == *"rc=0"* && "${out}" == *"unchanged"* && "$(tree)" == "${before}" ]] && ! fetched "${URL}"; then
    ok "run again: nothing changes, and the profile is not fetched a second time"
else
    bad "run again: nothing changes, and the profile is not fetched a second time" "${out}"
fi

# --- the catalogue declared on the claim --------------------------------------
new_world; publish operations-console 1.0.0
printf 'spec:\n  catalogue:\n    sources:\n      - name: gentian\n        url: https://elsewhere.example.test\n      - name: aluvian\n        url: %s/\n' \
    "${BASE}" > "${CHECKOUT}/clusters/${CLUSTER}/kernel/claims/cluster.yaml"
out="$(scaffold)"
if grep -qx '    gentianos.io/catalogue-origin: cluster/aluvian' "${DIR}/operations-console.bundle.yaml" 2>/dev/null; then
    ok "a catalogue the Cluster claim declares at that address gives the origin its name"
else
    bad "a catalogue the Cluster claim declares at that address gives the origin its name" "${out}"
fi

# --- mismatch -------------------------------------------------------------------
new_world; publish operations-console 1.0.0
profile operations-console 6.6.6 > "${CATALOGUE}/profiles/operations-console.yaml"   # the index still states 1.0.0's digest
out="$(scaffold)"
if [[ "${out}" == *"rc=1"* ]] && nothing_written; then
    ok "the file does not hash to the digest the index lists: the step stops, and nothing was written"
else
    bad "the file does not hash to the digest the index lists: the step stops, and nothing was written" "${out}$(ls "${DIR}")"
fi
if [[ "${out}" == *"is not the build listed by the index of ${BASE}"* && "${out}" == *"nothing was written"* \
      && "${out}" == *"GENTIAN_DEFAULT_PROFILES= (empty)"* && "${out}" != *"$(sha "${CATALOGUE}/profiles/operations-console.yaml")"* ]]; then
    ok "it says which digest was wanted and how to go on -- and not the digest of what was served"
else
    bad "it says which digest was wanted and how to go on -- and not the digest of what was served" "${out}"
fi
out="$(run 'scaffold_ok() { _scaffold_default_profiles '"${CLUSTER}"' || return 1; echo COMMITTED; }; scaffold_ok; echo "rc=$?"')"
if [[ "${out}" == *"rc=1"* && "${out}" != *"COMMITTED"* ]]; then
    ok "the caller, written as step 0 calls it, does not go on to the commit"
else
    bad "the caller, written as step 0 calls it, does not go on to the commit" "${out}"
fi
# shellcheck disable=SC2016 # literal text, not for this shell to expand
if grep -q '_scaffold_default_profiles "${cluster}" || return 1' "${REPO}/scripts/lib/bootstrap.sh"; then
    ok "step 0 calls it that way"
else
    bad "step 0 calls it that way"
fi

# --- unreachable ----------------------------------------------------------------
new_world; publish operations-console 1.0.0; touch "${CATALOGUE}/.down"
out="$(scaffold)"
if [[ "${out}" == *"rc=0"* && "${out}" == *"cannot be reached"* && "${out}" == *"The install goes on without it"* ]] && nothing_written; then
    ok "the catalogue does not answer: a warning, nothing written, and the install goes on"
else
    bad "the catalogue does not answer: a warning, nothing written, and the install goes on" "${out}"
fi
rm "${CATALOGUE}/.down"; touch "${CATALOGUE}/.broken"
out="$(scaffold)"
if [[ "${out}" == *"rc=0"* && "${out}" == *"cannot be reached"* ]] && nothing_written; then
    ok "it answers 503: the same"
else
    bad "it answers 503: the same" "${out}"
fi

# --- pinned ---------------------------------------------------------------------
new_world; publish operations-console 1.0.0 "$(printf '0%.0s' {1..64})"   # an index that states another digest
ACTUAL="sha256:$(sha "${CATALOGUE}/profiles/operations-console.yaml")"
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${URL}@${ACTUAL}")"
if [[ "${out}" == *"rc=0"* && -f "${DIR}/operations-console.yaml" && "${out}" == *"The pin is what is required"* \
      && "${out}" == *"Default profile operations-console: ${ACTUAL}, pinned in GENTIAN_DEFAULT_PROFILES."* ]]; then
    ok "pinned, and the file hashes to the pin: placed, though the index states another digest, and recorded as pinned"
else
    bad "pinned, and the file hashes to the pin: placed, though the index states another digest, and recorded as pinned" "${out}"
fi
new_world; publish operations-console 1.0.0   # the index agrees with the file
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${URL}@sha256:$(printf '1%.0s' {1..64})")"
if [[ "${out}" == *"rc=1"* && "${out}" == *"is not the build pinned in GENTIAN_DEFAULT_PROFILES"* && "${out}" == *"move the pin"* ]] && nothing_written; then
    ok "pinned, and the file does not hash to the pin: stops, though the index would have accepted it"
else
    bad "pinned, and the file does not hash to the pin: stops, though the index would have accepted it" "${out}"
fi
: > "${CURL_LOG}"
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${URL}@sha256:abc")"
if [[ "${out}" == *"rc=1"* && "${out}" == *"is not sha256:<64 hex digits>"* ]] && nothing_written && [[ ! -s "${CURL_LOG}" ]]; then
    ok "a pin that is not a sha256 digest: stops before anything is asked of the catalogue"
else
    bad "a pin that is not a sha256 digest: stops before anything is asked of the catalogue" "${out}"
fi

# --- the index lacks the entry ---------------------------------------------------
new_world; publish something-else 1.0.0; profile operations-console 1.0.0 > "${CATALOGUE}/profiles/operations-console.yaml"
out="$(scaffold)"
if [[ "${out}" == *"rc=1"* && "${out}" == *"lists no entry named operations-console"* && "${out}" == *"@sha256:<digest>"* ]] && nothing_written && ! fetched "${URL}"; then
    ok "the index does not list the entry: stops, the file is not even fetched, and it says how to pin"
else
    bad "the index does not list the entry: stops, the file is not even fetched, and it says how to pin" "${out}"
fi
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${URL}@sha256:$(sha "${CATALOGUE}/profiles/operations-console.yaml")")"
if [[ "${out}" == *"rc=0"* && -f "${DIR}/operations-console.bundle.yaml" ]]; then
    ok "the same catalogue with a pin: placed at the pin"
else
    bad "the same catalogue with a pin: placed at the pin" "${out}"
fi
new_world; profile operations-console 1.0.0 > "${CATALOGUE}/profiles/operations-console.yaml"   # no index.yaml at all
out="$(scaffold)"
if [[ "${out}" == *"rc=1"* && "${out}" == *"answered 404"* ]] && nothing_written; then
    ok "a catalogue with no index: stops"
else
    bad "a catalogue with no index: stops" "${out}"
fi
new_world; publish operations-console 1.0.0; rm "${CATALOGUE}/profiles/operations-console.yaml"
out="$(scaffold)"
if [[ "${out}" == *"rc=1"* && "${out}" == *"does not serve the file"* ]] && nothing_written; then
    ok "the index lists it and the catalogue does not serve it: stops"
else
    bad "the index lists it and the catalogue does not serve it: stops" "${out}"
fi

# --- an existing definition -----------------------------------------------------
new_world; publish operations-console 1.0.0
scaffold >/dev/null
OLD="sha256:$(sha "${DIR}/operations-console.yaml")"
publish operations-console 2.0.0   # the catalogue moves on
NEW="sha256:$(sha "${CATALOGUE}/profiles/operations-console.yaml")"
before="$(tree)"; : > "${CURL_LOG}"
out="$(scaffold)"
if [[ "${out}" == *"rc=0"* && "$(tree)" == "${before}" ]] && ! fetched "${URL}"; then
    ok "the definition holds a build other than the index now lists: kept, byte for byte, and nothing is fetched over it"
else
    bad "the definition holds a build other than the index now lists: kept, byte for byte, and nothing is fetched over it" "${out}"
fi
if [[ "${out}" == *"recorded:  ${OLD} (cluster/cat-example-test)"* && "${out}" == *"wanted:    ${NEW}"* && "${out}" == *"kept as it is"* \
      && "${out}" == *"kubectl gentian apps install"* && "${out}" != *"Default profile operations-console: sha256"* ]]; then
    ok "it reports both digests, that it was kept, and how to move to the new build -- and records nothing for the commit"
else
    bad "it reports both digests, that it was kept, and how to move to the new build -- and records nothing for the commit" "${out}"
fi
touch "${CATALOGUE}/.down"; : > "${CURL_LOG}"
out="$(scaffold)"
if [[ "${out}" == *"rc=0"* && "${out}" == *"stays as the cluster's definition has it"* && "$(tree)" == "${before}" ]]; then
    ok "and with the catalogue unreachable it stays as it is"
else
    bad "and with the catalogue unreachable it stays as it is" "${out}"
fi

# A definition from an installer that recorded nothing.
new_world; publish operations-console 1.0.0
mkdir -p "${DIR}"
cp "${CATALOGUE}/profiles/operations-console.yaml" "${DIR}/operations-console.yaml"
printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- operations-console.yaml\n' > "${DIR}/kustomization.yaml"
out="$(scaffold)"
if [[ "${out}" == *"rc=0"* && -f "${DIR}/operations-console.bundle.yaml" ]] && cmp -s "${CATALOGUE}/profiles/operations-console.yaml" "${DIR}/operations-console.yaml" \
    && ! fetched "${URL}" && [[ "$(grep -c 'operations-console' "${DIR}/kustomization.yaml")" == "2" ]]; then
    ok "an earlier installer's copy with no record, the same bytes as the index lists: the record is added, the profile untouched"
else
    bad "an earlier installer's copy with no record, the same bytes as the index lists: the record is added, the profile untouched" "${out}"
fi
new_world; publish operations-console 2.0.0
mkdir -p "${DIR}"
profile operations-console 1.0.0 > "${DIR}/operations-console.yaml"
printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- operations-console.yaml\n' > "${DIR}/kustomization.yaml"
STALE="sha256:$(sha "${DIR}/operations-console.yaml")"
out="$(scaffold)"
if [[ "${out}" == *"rc=0"* && "${out}" == *"It replaces the copy an earlier install wrote with no digest (${STALE})"* ]] \
    && cmp -s "${CATALOGUE}/profiles/operations-console.yaml" "${DIR}/operations-console.yaml" && [[ -f "${DIR}/operations-console.bundle.yaml" ]]; then
    ok "an earlier installer's copy that is another build, with no record: replaced by the verified build, as it always was, and said"
else
    bad "an earlier installer's copy that is another build, with no record: replaced by the verified build, as it always was, and said" "${out}"
fi
# The same copy, and a catalogue whose file does not hash to its index.
new_world; publish operations-console 2.0.0
mkdir -p "${DIR}"
profile operations-console 1.0.0 > "${DIR}/operations-console.yaml"
printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- operations-console.yaml\n' > "${DIR}/kustomization.yaml"
profile operations-console 6.6.6 > "${CATALOGUE}/profiles/operations-console.yaml"
before="$(tree)"
out="$(scaffold)"
if [[ "${out}" == *"rc=1"* && "$(tree)" == "${before}" ]]; then
    ok "and it is not replaced by a file that does not hash to the digest: the step stops, the copy untouched"
else
    bad "and it is not replaced by a file that does not hash to the digest: the step stops, the copy untouched" "${out}"
fi

# --- under the installer's own shell options -------------------------------------
# Every check above runs the library with errexit off, to read a return code.
# The installer runs it under `set -euo pipefail`, where a command that merely
# answers "no" outside a condition ends the run without a word.
strict() {
    local script="$1"; shift
    # shellcheck disable=SC2016 # the inner script is for the child shell to expand
    env -i HOME="${SB}/home" PATH="${BIN}:${PATH}" SCRIPT_DIR="${REPO}" CATALOGUE="${CATALOGUE}" CURL_LOG="${CURL_LOG}" \
        GENTIAN_DEPLOYMENTS_PATH="${CHECKOUT}" GENTIAN_DEPLOYMENTS_CLUSTER_ID="${CLUSTER}" KERNEL_DOMAIN="k.example.test" \
        GENTIAN_STORE_CATALOGUE_URL="${BASE}" SCRIPT="${script}" "$@" \
        bash -c 'source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1; trap - ERR; set -euo pipefail; eval "${SCRIPT}"' 2>&1 < /dev/null
}
new_world; publish operations-console 1.0.0
out="$(strict '_scaffold_default_profiles '"${CLUSTER}"'; echo "reached the end"')"
if [[ "${out}" == *"reached the end"* && -f "${DIR}/operations-console.bundle.yaml" ]]; then
    ok "under set -euo pipefail: a profile is placed and the run goes on"
else
    bad "under set -euo pipefail: a profile is placed and the run goes on" "${out}"
fi
out="$(strict '_scaffold_default_profiles '"${CLUSTER}"'; echo "reached the end"')"
if [[ "${out}" == *"unchanged"* && "${out}" == *"reached the end"* ]]; then
    ok "under set -euo pipefail: a second run finds it unchanged and goes on"
else
    bad "under set -euo pipefail: a second run finds it unchanged and goes on" "${out}"
fi
publish operations-console 2.0.0
out="$(strict '_scaffold_default_profiles '"${CLUSTER}"'; echo "reached the end"')"
if [[ "${out}" == *"kept as it is"* && "${out}" == *"reached the end"* ]]; then
    ok "under set -euo pipefail: another build in the definition is reported and the run goes on"
else
    bad "under set -euo pipefail: another build in the definition is reported and the run goes on" "${out}"
fi
touch "${CATALOGUE}/.down"
out="$(strict '_scaffold_default_profiles '"${CLUSTER}"'; echo "reached the end"')"
if [[ "${out}" == *"cannot be reached"* && "${out}" == *"reached the end"* ]]; then
    ok "under set -euo pipefail: an unreachable catalogue is a warning and the run goes on"
else
    bad "under set -euo pipefail: an unreachable catalogue is a warning and the run goes on" "${out}"
fi
out="$(strict 'preview_default_profiles '"${CLUSTER}"'; echo "reached the end"' GENTIAN_DRY_RUN=1)"
if [[ "${out}" == *"Would read"* && "${out}" == *"reached the end"* ]]; then
    ok "under set -euo pipefail: the preview goes on"
else
    bad "under set -euo pipefail: the preview goes on" "${out}"
fi

# --- entries that are not a catalogue address -----------------------------------
new_world; publish operations-console 1.0.0
profile operations-console 1.0.0 > "${WORLD}/local.yaml"
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${WORLD}/local.yaml")"
if [[ "${out}" == *"rc=1"* && "${out}" == *"is not an https address"* && "${out}" == *"remove the line from install.env"* \
      && "${out}" == *"@sha256:<digest>"* ]] && nothing_written && [[ ! -s "${CURL_LOG}" ]]; then
    ok "a local file: refused, with the three things to do instead, and nothing is fetched or written"
else
    bad "a local file: refused, with the three things to do instead, and nothing is fetched or written" "${out}"
fi
out="$(scaffold GENTIAN_DEFAULT_PROFILES="http://cat.example.test/profiles/operations-console.yaml")"
if [[ "${out}" == *"rc=1"* && "${out}" == *"is not an https address"* ]] && nothing_written; then
    ok "an http address: refused"
else
    bad "an http address: refused" "${out}"
fi
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${BASE}/operations-console.yaml")"
if [[ "${out}" == *"rc=1"* && "${out}" == *"is not a profile in a catalogue"* ]] && nothing_written; then
    ok "an https address that is not <catalogue>/profiles/<name>.yaml: refused"
else
    bad "an https address that is not <catalogue>/profiles/<name>.yaml: refused" "${out}"
fi
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${BASE}/profiles/desktop.yaml")"
if [[ "${out}" == *"rc=1"* && "${out}" == *"which the platform ships itself"* ]] && nothing_written; then
    ok "the name of a profile the platform ships: refused"
else
    bad "the name of a profile the platform ships: refused" "${out}"
fi
out="$(scaffold GENTIAN_DEFAULT_PROFILES=)"
if [[ "${out}" == "rc=0" ]] && nothing_written && [[ ! -s "${CURL_LOG}" ]]; then
    ok "GENTIAN_DEFAULT_PROFILES set empty: none, and nothing is asked"
else
    bad "GENTIAN_DEFAULT_PROFILES set empty: none, and nothing is asked" "${out}"
fi
out="$(scaffold GENTIAN_DISABLE_API_EXTENSIONS=1)"
if [[ "${out}" == *"rc=0"* ]] && nothing_written && [[ ! -s "${CURL_LOG}" ]]; then
    ok "--disable-api-extensions: none, and nothing is asked"
else
    bad "--disable-api-extensions: none, and nothing is asked" "${out}"
fi

# --- a bundle with companions -------------------------------------------------
new_world
{
    profile notes 1.0.0
    printf -- '---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes.files\n  labels:\n    gentianos.io/profile-name: notes\n    gentianos.io/asset: files\ndata:\n  a.txt: "a"\n'
} > "${CATALOGUE}/profiles/notes.yaml"
printf 'entries:\n- name: notes\n  digest: sha256:%s\n' "$(sha "${CATALOGUE}/profiles/notes.yaml")" > "${CATALOGUE}/index.yaml"
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${BASE}/profiles/notes.yaml")"
if [[ "${out}" == *"rc=0"* ]] && cmp -s "${CATALOGUE}/profiles/notes.yaml" "${DIR}/notes.yaml" \
    && carried "${DIR}/notes.bundle.yaml" | cmp -s - "${CATALOGUE}/profiles/notes.yaml" \
    && [[ "$(grep -c '^- ' "${DIR}/kustomization.yaml")" == "2" ]]; then
    ok "a profile with a companion: the file is written whole, once, and the bundle carries all of it"
else
    bad "a profile with a companion: the file is written whole, once, and the bundle carries all of it" "${out}"
fi
new_world
{
    profile notes 1.0.0
    printf -- '---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: notes.all\nroleRef:\n  kind: ClusterRole\n  name: cluster-admin\n'
} > "${CATALOGUE}/profiles/notes.yaml"
printf 'entries:\n- name: notes\n  digest: sha256:%s\n' "$(sha "${CATALOGUE}/profiles/notes.yaml")" > "${CATALOGUE}/index.yaml"
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${BASE}/profiles/notes.yaml")"
if [[ "${out}" == *"rc=1"* && "${out}" == *"ClusterRoleBinding"* && "${out}" == *"not a kind a bundle may hold"* && ! -e "${DIR}/notes.yaml" ]]; then
    ok "a file at the right digest that holds a kind no bundle may hold: refused, nothing written"
else
    bad "a file at the right digest that holds a kind no bundle may hold: refused, nothing written" "${out}"
fi

# --- two entries ------------------------------------------------------------------
new_world; publish operations-console 1.0.0
profile notes 1.0.0 > "${CATALOGUE}/profiles/notes.yaml"
printf -- '- name: notes\n  digest: sha256:%s\n' "$(sha "${CATALOGUE}/profiles/notes.yaml")" >> "${CATALOGUE}/index.yaml"
out="$(scaffold GENTIAN_DEFAULT_PROFILES="${URL} , ${BASE}/profiles/notes.yaml")"
if [[ "${out}" == *"rc=0"* && -f "${DIR}/operations-console.bundle.yaml" && -f "${DIR}/notes.bundle.yaml" ]] \
    && [[ "$(cat "${DIR}/kustomization.yaml")" == $'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- operations-console.yaml\n- notes.yaml\npatches:\n- path: operations-console.bundle.yaml\n- path: notes.bundle.yaml' ]]; then
    ok "two entries, comma separated: both placed, each listed once"
else
    bad "two entries, comma separated: both placed, each listed once" "${out}$(cat "${DIR}/kustomization.yaml" 2>&1)"
fi

# --- --dry-run ------------------------------------------------------------------
new_world; publish operations-console 1.0.0
mkdir -p "${DIR}"; printf '%s\n' "${EMPTY_KUSTOMIZATION}" > "${DIR}/kustomization.yaml"
before="$(tree)"
out="$(run 'preview_default_profiles '"${CLUSTER}"'; echo "rc=$?"' GENTIAN_DRY_RUN=1)"
if [[ "${out}" == *"rc=0"* && "${out}" == *"Would read ${BASE}/index.yaml and place operations-console only if"*"hashes to the digest it lists"* \
      && "$(tree)" == "${before}" && ! -s "${CURL_LOG}" ]]; then
    ok "--dry-run: says what an install would do, asks no catalogue and writes nothing"
else
    bad "--dry-run: says what an install would do, asks no catalogue and writes nothing" "${out}"
fi
out="$(run 'preview_default_profiles '"${CLUSTER}"'; echo "rc=$?"' GENTIAN_DRY_RUN=1 GENTIAN_DEFAULT_PROFILES="${URL}@sha256:$(printf 'a%.0s' {1..64}),${WORLD}/local.yaml")"
if [[ "${out}" == *"rc=0"* && "${out}" == *"hashes to the pinned sha256:aaaa"* && "${out}" == *"is not an https address"* \
      && "${out}" == *"Would stop at step 0"* && "$(tree)" == "${before}" && ! -s "${CURL_LOG}" ]]; then
    ok "--dry-run: a pin is named, and an entry an install would stop on is said to be one"
else
    bad "--dry-run: a pin is named, and an entry an install would stop on is said to be one" "${out}"
fi
# shellcheck disable=SC2016 # literal text, not for this shell to expand
if grep -q 'preview_default_profiles "${cluster}"' "${REPO}/scripts/lib/bootstrap.sh"; then
    ok "the read-only path of step 0 calls the preview"
else
    bad "the read-only path of step 0 calls the preview"
fi

# --- the signed commit ---------------------------------------------------------
if command -v gpg >/dev/null 2>&1; then
    new_world; publish operations-console 1.0.0
    DIGEST="sha256:$(sha "${CATALOGUE}/profiles/operations-console.yaml")"
    g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null 2>&1; }
    rm -rf "${CHECKOUT}"
    g init --bare -b main "${WORLD}/origin.git"
    g clone "${WORLD}/origin.git" "${CHECKOUT}"
    g -C "${CHECKOUT}" checkout -b main
    mkdir -p "${CHECKOUT}/clusters/${CLUSTER}/kernel/claims" "${CHECKOUT}/clusters/${CLUSTER}/tenants/platform" "${GPGHOME}"
    chmod 700 "${GPGHOME}"
    : > "${CHECKOUT}/clusters/${CLUSTER}/tenants/platform/kustomization.yaml"
    gpg --homedir "${GPGHOME}" --batch --yes --quiet --pinentry-mode loopback --passphrase '' \
        --quick-generate-key "Gentian break-glass (${CLUSTER}) <gentian-break-glass@k.example.test>" ed25519 sign never >/dev/null 2>&1
    out="$(run '_scaffold_default_profiles '"${CLUSTER}"' && gentian_commit_cluster_deployment "'"${CHECKOUT}/clusters/${CLUSTER}/kernel"'" '"${CLUSTER}"'; echo "rc=$?"' \
        GENTIAN_GPG_HOME="${GPGHOME}" TENANCY_MODE=multi)"
    message="$(git --git-dir "${WORLD}/origin.git" log -1 --format=%B main 2>/dev/null)"
    signer="$(git --git-dir "${WORLD}/origin.git" log -1 --format=%GK main 2>/dev/null)"
    if [[ "${out}" == *"rc=0"* && "${message}" == *"Default profile operations-console: ${DIGEST}, listed by the index of ${BASE}."* \
          && "${message}" == *"Fetched from ${URL}; recorded as cluster/cat-example-test."* ]]; then
        ok "the commit that carries the profile says its digest, who stated it, the address and the origin"
    else
        bad "the commit that carries the profile says its digest, who stated it, the address and the origin" "${out}"$'\n'"${message}"
    fi
    if [[ -n "${signer}" ]] && git --git-dir "${WORLD}/origin.git" show --stat --format= main | grep -q 'catalogue/operations-console.bundle.yaml'; then
        ok "and it is signed, with the bundle in it"
    else
        bad "and it is signed, with the bundle in it" "signer: ${signer}"
    fi
else
    echo "  ${YELLOW}skipped${NC}: gpg is not installed; the signed commit is not exercised."
fi

echo ""
if [[ ${fail} -eq 0 ]]; then
    echo "${GREEN}${pass} checks passed.${NC}"
    exit 0
fi
echo "${RED}${fail} failed${NC}, ${pass} passed."
exit 1
