#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-openbao-cli-download.sh
# =============================================================================
# The installer fetches one binary itself: the OpenBao CLI. It built the
# archive's name from its own lower-cased, Go-style words
# (bao_<ver>_linux_amd64.tar.gz), which the release has never served -- the
# archives are named as uname -s prints the system and with x86_64 for 64-bit
# Intel -- so every host without bao stopped on a 404.
#
# Three things are held here:
#
#   - the name, for every host the installer supports (Linux and macOS, on
#     x86_64 and arm64), and a refusal rather than a guess for any other;
#   - that the archive is checked against the release's checksum list before
#     anything is unpacked, and that a mismatch installs nothing;
#   - with GENTIAN_TEST_ONLINE=1, that each address answers 200 for the
#     version versions.yaml pins and that the checksum list names the archive.
#     That part needs the network, so it is off in `make lint`; run it when
#     the pin moves.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }

# shellcheck source=scripts/lib/compat.sh
source scripts/lib/compat.sh
# shellcheck source=scripts/lib/versions.sh
source scripts/lib/versions.sh
VERSION="$(gentian_pin openbao cli)"

echo ""
echo "The OpenBao CLI download: its name, its checksum"
echo ""

name() {
    local what="$1" want="$2" got; shift 2
    got="$(openbao_cli_asset "$@" 2>/dev/null)" || got="<refused>"
    if [[ "${got}" == "${want}" ]]; then ok "${what}: ${want}"; else bad "${what}: wanted ${want}, got ${got}"; fi
}
name "Linux, x86_64"            "bao_2.5.0_Linux_x86_64.tar.gz"  v2.5.0 Linux  x86_64
name "Linux, arm64 (aarch64)"   "bao_2.5.0_Linux_arm64.tar.gz"   v2.5.0 Linux  aarch64
name "macOS, Apple silicon"     "bao_2.5.0_Darwin_arm64.tar.gz"  v2.5.0 Darwin arm64
name "macOS, Intel"             "bao_2.5.0_Darwin_x86_64.tar.gz" v2.5.0 Darwin x86_64
name "a version without its v"  "bao_2.5.0_Linux_x86_64.tar.gz"  2.5.0  Linux  x86_64
name "a system it is not tested on is refused"       "<refused>" v2.5.0 FreeBSD x86_64
name "an architecture it is not tested on is refused" "<refused>" v2.5.0 Linux  riscv64

sums() {
    if [[ "$(openbao_cli_checksums_asset "$1" 2>/dev/null)" == "$2" ]]; then ok "$1 checksums: $2"; else bad "$1 checksums: wanted $2"; fi
}
sums Linux  checksums-linux.txt
sums Darwin checksums-darwin.txt

# --- the download itself, against a release served from a directory ---------
#
# _ensure_bao is install.sh's, and install.sh runs when it is sourced, so the
# function is lifted out of it by its text. curl is a stand-in that serves the
# files of ${RELEASE}; uname says what each case tells it to.
SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
RELEASE="${SANDBOX}/release"; BIN="${SANDBOX}/bin"
mkdir -p "${RELEASE}" "${BIN}" "${SANDBOX}/payload"
printf '#!/bin/sh\necho "bao stand-in"\n' > "${SANDBOX}/payload/bao"
chmod +x "${SANDBOX}/payload/bao"
ASSET="bao_${VERSION#v}_Linux_x86_64.tar.gz"
tar -czf "${RELEASE}/${ASSET}" -C "${SANDBOX}/payload" bao
printf '%s  %s\n' "$(sha256_of "${RELEASE}/${ASSET}")" "${ASSET}" > "${RELEASE}/checksums-linux.txt"

cat > "${BIN}/curl" <<'STUB'
#!/usr/bin/env bash
# Serves ${RELEASE}/<last path segment of the URL> to the file after -o.
url=""; out=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o) out="$2"; shift ;;
        http*) url="$1" ;;
    esac
    shift
done
echo "${url}" >> "${CURL_LOG}"
[[ -f "${RELEASE}/${url##*/}" ]] || exit 22
cp "${RELEASE}/${url##*/}" "${out}"
STUB
# shellcheck disable=SC2016 # written for the stand-in to expand
printf '#!/bin/sh\ncase "$1" in -s) echo "${FAKE_OS}";; -m) echo "${FAKE_ARCH}";; esac\n' > "${BIN}/uname"
chmod +x "${BIN}/curl" "${BIN}/uname"

# A host that has bao already returns at once, so the child shell is told it
# has none until one is in the sandbox HOME.
ensure() {   # ensure <home> <os> <arch>
    # shellcheck disable=SC2016 # the inner script is for the child shell to expand
    env -i HOME="$1" PATH="${BIN}:${PATH}" RELEASE="${RELEASE}" CURL_LOG="${SANDBOX}/curl.log" \
        FAKE_OS="$2" FAKE_ARCH="$3" OPENBAO_CLI_VERSION="${VERSION}" REPO="${REPO}" \
        bash -c '
            set -uo pipefail
            source "${REPO}/scripts/lib/compat.sh"
            info() { echo "$*"; }; success() { echo "$*"; }; error() { echo "$*" >&2; }
            command() {
                if [[ "${2:-}" == bao ]]; then [[ -x "${HOME}/.local/bin/bao" ]]; return; fi
                builtin command "$@"
            }
            eval "$(sed -n "/^_ensure_bao() {/,/^}/p" "${REPO}/install.sh")"
            _ensure_bao; echo "rc=$?"
        ' 2>&1
}

: > "${SANDBOX}/curl.log"
out="$(ensure "${SANDBOX}/home1" Linux x86_64)"
if [[ "${out}" == *"rc=0"* && -x "${SANDBOX}/home1/.local/bin/bao" ]] \
    && grep -q "/releases/download/${VERSION}/${ASSET}\$" "${SANDBOX}/curl.log" \
    && grep -q "/releases/download/${VERSION}/checksums-linux.txt\$" "${SANDBOX}/curl.log"; then
    ok "a matching archive is installed, from the release's own addresses"
else
    bad "a matching archive is installed, from the release's own addresses" "${out}"
fi

# The same archive under a checksum list that says something else.
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" "${ASSET}" > "${RELEASE}/checksums-linux.txt"
out="$(ensure "${SANDBOX}/home2" Linux x86_64)"
if [[ "${out}" == *"rc=1"* && "${out}" == *"does not match"* && ! -e "${SANDBOX}/home2/.local/bin/bao" ]]; then
    ok "an archive that does not match its checksum installs nothing"
else
    bad "an archive that does not match its checksum installs nothing" "${out}"
fi

# A checksum list that does not name the archive at all.
printf '%s  %s\n' "$(sha256_of "${RELEASE}/${ASSET}")" "something-else.tar.gz" > "${RELEASE}/checksums-linux.txt"
out="$(ensure "${SANDBOX}/home3" Linux x86_64)"
if [[ "${out}" == *"rc=1"* && ! -e "${SANDBOX}/home3/.local/bin/bao" ]]; then
    ok "an archive the checksum list does not name installs nothing"
else
    bad "an archive the checksum list does not name installs nothing" "${out}"
fi

: > "${SANDBOX}/curl.log"
out="$(ensure "${SANDBOX}/home4" FreeBSD x86_64)"
if [[ "${out}" == *"rc=1"* && ! -s "${SANDBOX}/curl.log" ]]; then
    ok "an unsupported host is told so, and nothing is fetched"
else
    bad "an unsupported host is told so, and nothing is fetched" "${out}"
fi

# --- the real release, on request -------------------------------------------
if [[ "${GENTIAN_TEST_ONLINE:-0}" == "1" ]]; then
    base="https://github.com/openbao/openbao/releases/download/${VERSION}"
    for host in "Linux x86_64" "Linux aarch64" "Darwin arm64" "Darwin x86_64"; do
        read -r os arch <<< "${host}"
        asset="$(openbao_cli_asset "${VERSION}" "${os}" "${arch}")"
        sums="$(openbao_cli_checksums_asset "${os}")"
        code="$(curl -sIL -o /dev/null -w '%{http_code}' --max-time 60 "${base}/${asset}")"
        if [[ "${code}" == "200" ]]; then ok "${base}/${asset} answers 200"; else bad "${base}/${asset} answers ${code}"; fi
        if curl -fsSL --max-time 60 "${base}/${sums}" | awk -v f="${asset}" '$2 == f {found = 1} END {exit !found}'; then
            ok "${sums} lists ${asset}"
        else
            bad "${sums} does not list ${asset}"
        fi
    done
fi

echo ""
if [[ ${fail} -eq 0 ]]; then
    echo "${GREEN}${pass} checks passed.${NC}"
    exit 0
fi
echo "${RED}${fail} failed${NC}, ${pass} passed."
exit 1
