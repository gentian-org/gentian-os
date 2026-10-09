#!/usr/bin/env bash
# =============================================================================
# scripts/lint/lint-image-pins.sh — no image the platform runs is `latest`,
# and the ones that must be exact are named by digest
# =============================================================================
# The model gateway ran ghcr.io/berriai/litellm:latest: whatever upstream
# published last, pulled again on every pod start. Nothing said so anywhere,
# because nothing looked -- lint-image-digests asks the registry about the
# digests that ARE pinned, and has nothing to say about an image with none.
#
# Two rules, both read from the repository alone:
#
#   1. No `image:` line under kernel/, charts/ or crossplane/ names `latest`,
#      as a literal tag or as a template's fallback.
#
#   2. Every image listed in DIGEST_REQUIRED is written tag@sha256:<digest>
#      wherever it is named, and is named at least once: a pin cannot be
#      loosened to a tag, and cannot disappear by the image being renamed
#      without this list being touched.
#
# And one that needs helm: the model gateway's chart is rendered, and every
# image in what comes out is held to both rules, so a template that stops
# reading the pinned value is caught as well as a value that stops being one.
#
# What lint-image-digests adds on top, with the network: that each digest is
# the multi-architecture one.
# =============================================================================
set -euo pipefail

# scripts/lint/ -> repo root
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
fail=0

# Images that are named by digest as well as by tag. One per line.
DIGEST_REQUIRED=(
    ghcr.io/berriai/litellm
)

# `latest` that is known and not yet removed, as file:pattern. The list is
# for shrinking: an entry is a floating image somebody still has to choose a
# release for, not a decision that floating is fine there.
#
#   vllm.yaml  the model server of a GPU cluster; its tag comes from the
#              claim's llm.instances[].imageTag and falls back to latest.
KNOWN_LATEST=(
    'kernel/services/llm/chart/templates/vllm.yaml:default "latest"'
)

_known_latest() {
    local file="$1" text="$2" entry
    for entry in "${KNOWN_LATEST[@]}"; do
        [[ "${file}" == "${entry%%:*}" && "${text}" == *"${entry#*:}"* ]] && return 0
    done
    return 1
}

echo ""
echo "Image pin lint — no latest, and a digest where one is required"
echo ""

files() {
    # Vendored charts' READMEs repeat their values as prose.
    git ls-files 'kernel/**/*.yaml' 'kernel/**/*.tpl' 'charts/**/*.yaml' 'charts/**/*.tpl' \
        'crossplane/compositions/*.yaml' 2>/dev/null
}

# --- 1. no latest -----------------------------------------------------------
while IFS= read -r hit; do
    [[ -n "${hit}" ]] || continue
    file="${hit%%:*}"; rest="${hit#*:}"; line="${rest%%:*}"; text="${rest#*:}"
    if _known_latest "${file}" "${text}"; then
        printf '  %s○%s %s:%s  known, still floating\n' "${YELLOW}" "${NC}" "${file}" "${line}"
        continue
    fi
    printf '  %s✗%s %s:%s\n      %s\n' "${RED}" "${NC}" "${file}" "${line}" "${text#"${text%%[![:space:]]*}"}"
    printf '      An image tagged latest is a different image on every pull. Name a release.\n'
    fail=1
done < <(files | xargs grep -HnE '^[[:space:]]*(-[[:space:]]+)?image:[[:space:]].*latest' 2>/dev/null |
         grep -vE ':[0-9]+:[[:space:]]*#' || true)

# --- 2. a digest where one is required --------------------------------------
for image in "${DIGEST_REQUIRED[@]}"; do
    named=0
    while IFS= read -r hit; do
        [[ -n "${hit}" ]] || continue
        named=$((named + 1))
        file="${hit%%:*}"; rest="${hit#*:}"; line="${rest%%:*}"; text="${rest#*:}"
        if grep -qE "${image//./\\.}:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}([^0-9a-f]|\$)" <<< "${text}" \
            && ! grep -qE "${image//./\\.}:latest@" <<< "${text}"; then
            printf '  %s✓%s %-28s %s:%s\n' "${GREEN}" "${NC}" "${image##*/}" "${file}" "${line}"
        else
            printf '  %s✗%s %s:%s\n      %s\n' "${RED}" "${NC}" "${file}" "${line}" "${text#"${text%%[![:space:]]*}"}"
            printf '      %s is named by release tag and digest: %s:<tag>@sha256:<digest>\n' "${image##*/}" "${image}"
            fail=1
        fi
    done < <(files | xargs grep -HnF "${image}" 2>/dev/null | grep -vE ':[0-9]+:[[:space:]]*#' || true)
    if [[ ${named} -eq 0 ]]; then
        printf '  %s✗%s %s is not named anywhere under kernel/, charts/ or crossplane/.\n' "${RED}" "${NC}" "${image}"
        printf '      If the image moved, move its entry in DIGEST_REQUIRED with it.\n'
        fail=1
    fi
done

# --- 3. what the model gateway's chart renders ------------------------------
if command -v helm >/dev/null 2>&1; then
    rendered="$(helm template lint kernel/services/llm/manifests 2>/dev/null |
        sed -n 's/^[[:space:]]*\(-[[:space:]]\{1,\}\)\{0,1\}image:[[:space:]]*//p' | tr -d "\"'" | sort -u)"
    if [[ -z "${rendered}" ]]; then
        printf '  %s✗%s kernel/services/llm/manifests rendered no image at all.\n' "${RED}" "${NC}"
        fail=1
    fi
    gateway=0
    while IFS= read -r ref; do
        [[ -n "${ref}" ]] || continue
        name="${ref%%@*}"
        # A tag is what follows the last colon, unless that colon is a
        # registry's port and a path follows it.
        tag=""
        [[ "${name##*/}" == *:* ]] && tag="${name##*:}"
        if [[ -z "${tag}" || "${tag}" == "latest" ]]; then
            printf '  %s✗%s the model gateway chart renders %s, which names no release.\n' "${RED}" "${NC}" "${ref}"
            fail=1
        fi
        for image in "${DIGEST_REQUIRED[@]}"; do
            [[ "${name%:*}" == "${image}" ]] || continue
            gateway=$((gateway + 1))
            if [[ ! "${ref}" =~ @sha256:[0-9a-f]{64}$ ]]; then
                printf '  %s✗%s the model gateway chart renders %s without a digest.\n' "${RED}" "${NC}" "${ref}"
                fail=1
            fi
        done
    done <<< "${rendered}"
    if [[ ${gateway} -ne 1 ]]; then
        printf '  %s✗%s the model gateway chart renders %d different LiteLLM images; its containers run one.\n' \
            "${RED}" "${NC}" "${gateway}"
        fail=1
    elif [[ ${fail} -eq 0 ]]; then
        printf '  %s✓%s %-28s one image, with its digest, in every container the chart renders\n' "${GREEN}" "${NC}" "litellm"
    fi
else
    printf '  %s○%s helm is not installed; the rendered chart was not checked.\n' "${YELLOW}" "${NC}"
fi

echo ""
if [[ ${fail} -ne 0 ]]; then
    echo "${RED}Unpinned image(s) found.${NC}"
    exit 1
fi
echo "${GREEN}No image is latest, and every image that must be exact carries its digest.${NC}"
