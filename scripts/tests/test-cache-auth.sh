#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-cache-auth.sh
# =============================================================================
# The kernel cache's start script (charts/infra/cache), run in the image the
# chart pins, against a data directory kept across starts:
#
#   - a client without the password is refused, and one with it is let in;
#   - a user saved with ACL SAVE is there again after a restart;
#   - started with another password, the old one is refused and the new one
#     is let in.
#
# The script is the one helm renders, not a copy. Needs helm, docker and
# python3 with PyYAML; skips (exit 0) without them, except under CI, where a
# skip would read as coverage.
# =============================================================================
# Every check is a condition string evaluated by check, so its expansions are
# meant to wait; the values they read are set only for it:
# shellcheck disable=SC2016,SC2034
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

for tool in helm docker python3; do
    if ! command -v "${tool}" >/dev/null; then
        if [ -n "${CI:-}" ]; then echo "FAIL: ${tool} is required under CI"; exit 1; fi
        echo "SKIP: ${tool} not found"; exit 0
    fi
done
if ! docker info >/dev/null 2>&1; then
    if [ -n "${CI:-}" ]; then echo "FAIL: docker is not usable under CI"; exit 1; fi
    echo "SKIP: docker is not usable"; exit 0
fi

SANDBOX="$(mktemp -d)"
NAME="gentian-cache-auth-$$"
VOLUME="${NAME}-data"
# shellcheck disable=SC2317,SC2329 # run by the EXIT trap
cleanup() {
    docker rm -f "${NAME}" >/dev/null 2>&1
    docker volume rm -f "${VOLUME}" >/dev/null 2>&1
    rm -rf "${SANDBOX}"
}
trap cleanup EXIT

helm template cache charts/infra/cache --set auth.existingSecret=cache-test \
    > "${SANDBOX}/rendered.yaml" || { echo "FAIL: helm template"; exit 1; }
python3 - "${SANDBOX}" <<'EOF' || { echo "FAIL: no StatefulSet container in the render"; exit 1; }
import sys, yaml
out = sys.argv[1]
for doc in yaml.safe_load_all(open(f"{out}/rendered.yaml")):
    if doc and doc.get("kind") == "StatefulSet":
        c = doc["spec"]["template"]["spec"]["containers"][0]
        open(f"{out}/start.sh", "w").write(c["args"][0])
        open(f"{out}/image", "w").write(c["image"])
        break
else:
    sys.exit(1)
EOF
IMAGE="$(cat "${SANDBOX}/image")"

# start <password> — the server, on the kept volume, as the chart starts it.
start() {
    docker rm -f "${NAME}" >/dev/null 2>&1
    docker run -d --name "${NAME}" -v "${VOLUME}:/data" -e "REDISCLI_AUTH=$1" \
        -v "${SANDBOX}/start.sh:/start.sh:ro" --entrypoint /bin/sh "${IMAGE}" /start.sh >/dev/null \
        || { echo "FAIL: the server did not start"; exit 1; }
    for _ in $(seq 1 50); do
        docker exec "${NAME}" redis-cli -a "$1" --no-auth-warning ping 2>/dev/null | grep -q PONG && return 0
        sleep 0.2
    done
    echo "FAIL: the server does not answer with its password"
    docker logs "${NAME}"
    exit 1
}

# cli [<password>] -- <command...> — a client with or without a password.
cli() {
    local pass="$1"; shift 2
    if [ -n "${pass}" ]; then
        docker exec "${NAME}" redis-cli -a "${pass}" --no-auth-warning "$@" 2>&1
    else
        docker exec -e REDISCLI_AUTH= "${NAME}" redis-cli "$@" 2>&1
    fi
}

start first-password
reply="$(cli '' -- ping)"
check "no password is refused" '[[ "${reply}" == *NOAUTH* ]]'
reply="$(cli '' -- get anything)"
check "no password reads nothing" '[[ "${reply}" == *NOAUTH* ]]'
reply="$(cli wrong -- ping)"
check "a wrong password is refused" '[[ "${reply}" == *WRONGPASS* || "${reply}" == *NOAUTH* ]]'
reply="$(cli first-password -- ping)"
check "the password is let in" '[ "${reply}" = PONG ]'

cli first-password -- ACL SETUSER app-one on '>app-secret' '~app-one:*' '+@all' >/dev/null
cli first-password -- ACL SAVE >/dev/null

start second-password
reply="$(cli first-password -- ping)"
check "the old password is refused after a restart with a new one" '[[ "${reply}" == *WRONGPASS* || "${reply}" == *NOAUTH* ]]'
reply="$(cli second-password -- ping)"
check "the new password is let in" '[ "${reply}" = PONG ]'
reply="$(cli '' -- ping)"
check "no password is still refused after a restart" '[[ "${reply}" == *NOAUTH* ]]'
reply="$(cli second-password -- ACL LIST)"
check "the saved user is there after a restart" '[[ "${reply}" == *"user app-one "* ]]'
default_lines="$(printf '%s\n' "${reply}" | grep -c '^user default ')"
check "the default user is listed once" '[ "${default_lines}" = 1 ]'
check "the default user has no empty password" '[[ "${reply}" != *"user default on nopass"* ]]'

finish
