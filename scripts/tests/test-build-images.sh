#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-build-images.sh
# =============================================================================
# Pre-flight checks every image built from the operator's commit at the tag a
# run pins, not only the operator's (check_build_images). CI publishes them in
# separate jobs; a run that checked the operator's alone moved Keycloak to a
# listener image that was never published, and nobody could sign in.
#
# Held here:
#   - gentian_os_build_images names exactly the images CI builds per commit
#     (the docker job's gentian-os, the images job's matrix);
#   - every image the bootstrap or the operator chart pins to the operator's
#     tag is one of them;
#   - the listener is asked for at the hash of its sources, which the identity
#     appset names, and once;
#   - a missing image is named and fails the check, and a tag given twice is
#     checked once.
#
# No cluster, no registry.
# =============================================================================
# The checks are strings handed to eval (harness.sh): they expand there.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

echo ""
echo "build images: pre-flight checks every image of the operator's commit"
echo ""

listed="$(SCRIPT_DIR="$(pwd)" bash -c 'source scripts/lib/load.sh >/dev/null 2>&1; gentian_os_build_images' | sort)"

# What CI builds: the docker job's image, and the images job's matrix.
ci="$( {
    grep -oE 'images: ghcr\.io/gentian-org/[a-z0-9-]+$' .github/workflows/ci.yaml | sed 's|.*/||'
    awk '/^  images:$/ {in_job=1; next} in_job && /^  [a-z-]+:$/ {in_job=0} in_job' .github/workflows/ci.yaml \
        | grep -oE '^ +image: [a-z0-9-]+$' | awk '{print $2}'
} | sort -u)"
if [[ "${listed}" == "${ci}" ]]; then
    ok "the list is what CI builds per commit"
else
    bad "the list is what CI builds per commit" "listed: ${listed//$'\n'/ }; CI: ${ci//$'\n'/ }"
fi

# What a run pins to the operator's tag: an appset's placeholder, and the
# operator chart's images printed with its $imageTag.
pinned="$( {
    grep -rhoE 'ghcr\.io/gentian-org/[a-z0-9-]+:gentian\.os\.revision\.placeholder' kernel | sed -E 's|.*/([a-z0-9-]+):.*|\1|'
    grep -rhoE 'ghcr\.io/gentian-org/[a-z0-9-]+:%s" \$imageTag' charts/gentian-os/templates | sed -E 's|.*/([a-z0-9-]+):.*|\1|'
} | sort -u)"
check "the bootstrap pins some image to the operator's tag" '[[ -n "${pinned}" ]]'
while IFS= read -r img; do
    [[ -n "${img}" ]] || continue
    check "${img}, pinned to the operator's tag, is checked" 'grep -qx "${img}" <<<"${listed}"' "listed: ${listed//$'\n'/ }"
done <<<"${pinned}"

# The listener is named by its sources, by the appset and by the check alike.
listener="$(bash scripts/tools/listener-tag.sh)"
check "the listener's tag is a source hash" '[[ "${listener}" =~ ^src-[0-9a-f]{12}$ ]]' "${listener}"
check "the identity appset names the listener by it" 'grep -q "keycloak-event-listener:${listener}'"'"'" kernel/appsets/raw/09-identity.yaml'

# A registry that has every image but the listener at its source tag.
run() {
    SCRIPT_DIR="$(pwd)" MISSING="${missing}" bash -c 'source scripts/lib/load.sh >/dev/null 2>&1; set +e
validate_image_tag() { echo "asked $1:$2"; [[ "$1:$2" != "${MISSING}" ]]; }
check_build_images values.yaml "$@"; echo "rc=$?"' _ "$@" 2>&1
}
missing="ghcr.io/gentian-org/keycloak-event-listener:${listener}"
out="$(run develop develop-abc1234 develop-abc1234)"
check "a missing listener image fails the check" '[[ "${out}" == *"rc=1"* ]]' "${out}"
check "the missing image is named" '[[ "${out}" == *"keycloak-event-listener:${listener} does not exist"* ]]' "${out}"
check "the listener is not asked for at the operator's tag" '[[ "${out}" != *"keycloak-event-listener:develop"* ]]' "${out}"
if [[ "$(grep -c "asked ghcr.io/gentian-org/keycloak-event-listener:${listener}" <<<"${out}")" == 1 ]]; then
    ok "the listener is asked for once, whatever the tags"
else
    bad "the listener is asked for once, whatever the tags" "${out}"
fi
check "the storage tools image is asked for too" '[[ "${out}" == *"asked ghcr.io/gentian-org/storage-tools:develop-abc1234"* ]]' "${out}"
if [[ "$(grep -c 'asked ghcr.io/gentian-org/gentian-os:develop-abc1234' <<<"${out}")" == 1 ]]; then
    ok "a tag given twice is checked once"
else
    bad "a tag given twice is checked once" "${out}"
fi
missing=""
out="$(run develop)"
check "every image present passes" '[[ "${out}" == *"rc=0"* ]]' "${out}"

finish
