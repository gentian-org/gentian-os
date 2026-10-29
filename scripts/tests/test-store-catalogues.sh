#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-store-catalogues.sh
# =============================================================================
# Step 0 reads GET /v1/meta of the store the Cluster claim names and adds each
# catalog it lists that the claim has no source of that name for, marked
# addedBy: store (store API 1.5), so an app bought at the store installs
# without a `kubectl gentian catalogues add` first. Held here:
#
#   - a listed catalog the claim lacks is added, after the last source,
#     with its mark; nothing else of the claim moves; a second run adds
#     nothing;
#   - a source the claim has is never changed: the same name at another
#     address is kept and warned about with both addresses;
#   - a name a tenant uses for a catalog of its own is not added;
#   - http, or a name that is not one, is not added;
#   - a store that cannot be reached, answers 404, or lists no catalogs is a
#     warning, and the claim is as it was;
#   - one the store stops listing stays on the claim;
#   - `sources: []` and a section with no sources get the list; a list written
#     on one line is left and said;
#   - no store, or usage statistics off: the store is not asked;
#   - --dry-run and --validate say what would be added and write nothing.
#
# curl is a stand-in that serves a directory; kubectl answers nothing. No
# cluster, no network.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }
is() { # <what> <got> <want>
    if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "    got:  $2
    want: $3"; fi
}
has() { # <what> <text> <fixed string>
    if grep -qF -- "$3" <<< "$2"; then ok "$1"; else bad "$1" "    in: $2
    no: $3"; fi
}

SB="$(mktemp -d)"
trap 'rm -rf "${SB}"' EXIT
BIN="${SB}/bin"; mkdir -p "${BIN}" "${SB}/home"

# curl, as _catalogue_fetch uses it: https://store.example.test/<path> is
# ${STORE}/<path>. A file that is not there is a 404; a store with a file
# named .down does not answer at all. Every address asked is logged.
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
[[ -e "${STORE}/.down" ]] && exit 7
code=200
file="${STORE}/${url#https://store.example.test/}"
[[ -f "${file}" ]] || code=404
if [[ "${code}" == "200" ]]; then cat "${file}" > "${out}"; else printf 'no\n' > "${out}"; fi
[[ "${fmt}" == *http_code* ]] && printf '%s' "${code}"
exit 0
STUB
cat > "${BIN}/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "${KUBECTL_LOG}"
exit 1
STUB
chmod +x "${BIN}/curl" "${BIN}/kubectl"

GENTIAN_URL="https://gentian.example/apps"
PUB_URL="https://catalogue.publisher.example"

# new_world -- a claim naming the store, a store, an empty tenants tree.
new_world() {
    W="$(mktemp -d "${SB}/w.XXXXXX")"
    STORE="${W}/store"; CURL_LOG="${W}/curl.log"; KUBECTL_LOG="${W}/kubectl.log"
    CLAIM="${W}/clusters/c/kernel/claims/cluster.yaml"; TENANTS="${W}/clusters/c/tenants"
    mkdir -p "${STORE}/v1" "$(dirname "${CLAIM}")" "${TENANTS}"
    : > "${CURL_LOG}"; : > "${KUBECTL_LOG}"
    cat > "${CLAIM}" <<YAML
apiVersion: gentianos.io/v1alpha1
kind: Cluster
metadata:
  name: c
spec:
  kernelDomain: k.example

  # Catalogues this cluster may fetch profiles from.
  catalogue:
    # The base address of the App Store API.
    storeUrl: https://store.example.test
    sources:
      - name: gentian
        # The development catalogue.
        url: ${GENTIAN_URL}
      # - name: in-house
      #   url: https://git.example.com/profiles
  mail:
    serviceMode: internal
YAML
}
# meta <catalogues JSON> -- the store's GET /v1/meta; "none" leaves the key out.
meta() {
    if [[ "$1" == "none" ]]; then
        printf '{"apiVersion":"1.4","name":"S"}\n' > "${STORE}/v1/meta"
    else
        printf '{"apiVersion":"1.5","name":"S","catalogues":%s}\n' "$1" > "${STORE}/v1/meta"
    fi
}
# edit <sed script> -- the claim through sed, without -i (BSD sed differs).
edit() { sed "$1" "${CLAIM}" > "${CLAIM}.x" && mv "${CLAIM}.x" "${CLAIM}"; }
both() { meta '[{"name":"gentian","url":"'"${GENTIAN_URL}"'","displayName":"Gentian OS"},{"name":"publisher","url":"'"${PUB_URL}"'","displayName":"Publisher"}]'; }
# run <shell> [VAR=value ...] -- the library in a fresh shell, as the
# installer loads it.
run() {
    local script="$1"; shift
    # shellcheck disable=SC2016 # the inner script is for the child shell to expand
    env -i HOME="${SB}/home" PATH="${BIN}:${PATH}" SCRIPT_DIR="${REPO}" STORE="${STORE}" CURL_LOG="${CURL_LOG}" \
        KUBECTL_LOG="${KUBECTL_LOG}" KERNEL_DOMAIN=k.example GENTIAN_NO_USAGE_STATISTICS=0 SCRIPT="${script}" "$@" \
        bash -c 'set -u; source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1; trap - ERR; set +e; eval "${SCRIPT}"' 2>&1 < /dev/null
}
ensure() { run 'ensure_claim_store_catalogues "'"${CLAIM}"'" "'"${TENANTS}"'"; echo "rc=$?"' "$@"; }
sources() { run '_claim_sources_tsv "'"${CLAIM}"'"' | tr '\t' '|'; }

echo ""
echo "Step 0: the catalogs the store lists"
echo ""

new_world; both
orig="$(cat "${CLAIM}")"
said="$(ensure)"
has "the run goes on" "${said}" "rc=0"
is "the store's meta is what is asked" "$(cat "${CURL_LOG}")" "https://store.example.test/v1/meta"
is "the catalogue the claim lacks is added, marked; the one it has is as it was" "$(sources)" \
    "gentian|${GENTIAN_URL}||
publisher|${PUB_URL}||store"
is "it goes after the last source, above the comments that end the list" \
    "$(sed -n '16,19p' "${CLAIM}")" \
    "      - name: publisher
        # Listed by the store https://store.example.test (GET /v1/meta); added by the installer.
        url: ${PUB_URL}
        addedBy: store"
is "nothing else of the claim moves" "$(sed '16,19d' "${CLAIM}")" "${orig}"
has "and it says what it added" "${said}" "added the catalogues the store https://store.example.test lists: publisher (${PUB_URL})"
before="$(cat "${CLAIM}")"
said="$(ensure)"
is "a second run adds nothing" "$(cat "${CLAIM}")" "${before}"
has "and says the store's catalogues are all there" "${said}" "all on the claim (2)"

# Dropped by the store: stays.
meta '[{"name":"gentian","url":"'"${GENTIAN_URL}"'"}]'
ensure >/dev/null
is "a catalogue the store stops listing stays on the claim" "$(cat "${CLAIM}")" "${before}"

# The same name at another address: the claim's is kept, both are said.
new_world
meta '[{"name":"gentian","url":"https://elsewhere.example/apps"}]'
before="$(cat "${CLAIM}")"
said="$(ensure)"
is "a name the claim has at another address is not changed" "$(cat "${CLAIM}")" "${before}"
has "the warning names the store's address" "${said}" "lists the catalogue gentian at https://elsewhere.example/apps"
has "and the claim's" "${said}" "names gentian at ${GENTIAN_URL}. The claim's is kept."
is "it is said once" "$(grep -c 'lists the catalogue gentian' <<< "${said}")" "1"
has "the run goes on" "${said}" "rc=0"

# A trailing slash is the same address.
new_world
meta '[{"name":"gentian","url":"'"${GENTIAN_URL}"'/"}]'
before="$(cat "${CLAIM}")"
said="$(ensure)"
is "the same address with a trailing slash is the source the claim has" "$(cat "${CLAIM}")" "${before}"
is "and nothing is warned" "$(grep -c 'WARN\|kept' <<< "${said}")" "0"

# Not https, not a name.
new_world
meta '[{"name":"plain","url":"http://plain.example/apps"},{"name":"Bad_Name","url":"https://b.example"},{"name":"q","url":"https://q.example/a b"}]'
before="$(cat "${CLAIM}")"
said="$(ensure)"
is "http, a name that is none, and an address with a space are not added" "$(cat "${CLAIM}")" "${before}"
is "each is said" "$(grep -c 'cannot take' <<< "${said}")" "3"

# A tenant's own catalog of the name.
new_world; both
mkdir -p "${TENANTS}/team"
printf 'apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: team\nspec:\n  catalogue:\n    sources:\n      - name: publisher\n        oci:\n          repository: r\n          artifact: ghcr.io/x/c:1\n        addedBy: cluster\n' > "${TENANTS}/team/tenant.yaml"
before="$(cat "${CLAIM}")"
said="$(ensure)"
is "a name a tenant has a catalogue of is not added for every tenant" "$(cat "${CLAIM}")" "${before}"
has "and the tenant is named" "${said}" "tenant team has a catalogue of"

# The store does not answer, answers 404, or lists nothing.
for how in down 404 none notjson; do
    new_world
    case "${how}" in
        down) both; touch "${STORE}/.down" ;;
        404) : ;;
        none) meta none ;;
        notjson) printf 'not json\n' > "${STORE}/v1/meta" ;;
    esac
    before="$(cat "${CLAIM}")"
    said="$(ensure)"
    is "a store that is ${how}: the claim is as it was" "$(cat "${CLAIM}")" "${before}"
    has "a store that is ${how}: the run goes on" "${said}" "rc=0"
    case "${how}" in
        down) has "  and says it cannot be reached" "${said}" "cannot be reached" ;;
        404) has "  and says what it answered" "${said}" "answered 404 to GET /v1/meta" ;;
        *) has "  and says it lists no catalogues" "${said}" "lists no catalogues" ;;
    esac
done

# Other shapes of the section.
new_world; both
edit 's|^    sources:$|    sources: []|; /^      - name: gentian$/d; /^        # The development catalogue.$/d; /^        url: https:\/\/gentian.example\/apps$/d'
ensure >/dev/null
is "sources: [] becomes the list of what the store lists" "$(sources)" \
    "gentian|${GENTIAN_URL}||store
publisher|${PUB_URL}||store"
is "  and the comments after it stay" "$(grep -c '# - name: in-house' "${CLAIM}")" "1"

new_world; both
edit '/^    sources:$/,/^      #   url:/d'
ensure >/dev/null
is "a section with no sources gets them" "$(sources | cut -d'|' -f1 | tr '\n' ' ')" "gentian publisher "
is "  and the claim's other sections are as they were" "$(run 'yq_get .spec.mail.serviceMode "'"${CLAIM}"'"')" "internal"

new_world; both
awk '/^    sources:$/ { print "    sources: [{name: gentian, url: https://gentian.example/apps}]"; skip = 1; next }
     skip && /^      #   url:/ { skip = 0; next }
     !skip' "${CLAIM}" > "${CLAIM}.x" && mv "${CLAIM}.x" "${CLAIM}"
before="$(cat "${CLAIM}")"
said="$(ensure)"
is "a list written on one line is not rewritten" "$(cat "${CLAIM}")" "${before}"
has "  and that is said" "${said}" "not written as a block"

# No store; usage statistics off.
new_world; both
edit '/storeUrl:/d'
ensure >/dev/null
is "a claim that names no store asks none" "$(cat "${CURL_LOG}")" ""
new_world; both
before="$(cat "${CLAIM}")"
said="$(ensure GENTIAN_NO_USAGE_STATISTICS=1)"
is "with usage statistics off the store is not asked" "$(cat "${CURL_LOG}")" ""
is "  and the claim is as it was" "$(cat "${CLAIM}")" "${before}"

# --dry-run: what would be added, and nothing written.
new_world; both
before="$(cat "${CLAIM}")"
said="$(run 'preview_claim_store_catalogues "'"${CLAIM}"'" "'"${TENANTS}"'"' GENTIAN_DRY_RUN=1)"
is "a dry run writes nothing" "$(cat "${CLAIM}")" "${before}"
has "  and says what an install would add" "${said}" "Would add the store's catalogues to claims/cluster.yaml"
has "  naming it" "${said}" "publisher (${PUB_URL})"

# The schema takes the mark, and nothing but store.
is "the Cluster schema allows addedBy: store on a source" \
    "$(run 'yq_get ".spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.catalogue.properties.sources.items.properties.addedBy.enum[0]" "'"${REPO}"'/crossplane/xrds/cluster.yaml"')" "store"

is "kubectl was not asked anything" "$(cat "${KUBECTL_LOG}")" ""

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%s%d passed%s\n' "${GREEN}" "${pass}" "${NC}"
