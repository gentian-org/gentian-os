#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-signing-key-lookup.sh
# =============================================================================
# The installer signs what it writes to the deployments repository with the
# cluster's break-glass key. It found that key by its uid, and the uid has
# the kernel domain in it -- so anything that signed before the domain had
# been read from the claim looked for "...@cluster.invalid", found nothing,
# concluded this cluster had no key and generated a second one. The commit it
# then pushed was signed by a key nothing trusts.
#
# Held here, with a real gpg and a keyring in a temporary directory:
#
#   - the key the repository records (clusters/<id>/kernel/signing/keys.env)
#     is found whether or not the kernel domain is known, and no second key
#     is generated beside it;
#   - a stray key under the placeholder address -- what the accident left in
#     the keyring -- is never the answer, with the domain known or not;
#   - before keys.env exists the key is still found by the cluster's address,
#     and none is generated while the domain is unknown;
#   - under --dry-run nothing is generated and no keyring is created;
#   - a head commit signed by the stray key is covered by one signed with the
#     recorded key (gentian_sign_unsigned_head), which is what repairs a
#     repository the accident left that way.
#
# Skips when gpg is not installed. No cluster, no network.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }

echo ""
echo "The break-glass key is found by the id the repository records"
echo ""
if ! command -v gpg >/dev/null 2>&1; then
    echo "  ${YELLOW}skipped${NC}: gpg is not installed."
    exit 0
fi

# Short, because gpg-agent's socket lives under the keyring's directory and a
# socket path has a length limit.
SB="$(mktemp -d "${TMPDIR:-/tmp}/gsk.XXXXXX")"
GPGHOME="${SB}/g"
# The agent gpg starts for this keyring is stopped before the directory goes.
trap 'gpgconf --homedir "${GPGHOME}" --kill all >/dev/null 2>&1; rm -rf "${SB}"' EXIT
g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null 2>&1; }
raw_gpg() { gpg --homedir "${GPGHOME}" --batch --yes --quiet --pinentry-mode loopback --passphrase '' "$@"; }
fpr_of() { raw_gpg --list-keys --with-colons "$1" 2>/dev/null | awk -F: '$1=="fpr" {print $10; exit}'; }
key_count() { raw_gpg --list-keys --with-colons 2>/dev/null | grep -c '^pub:' || true; }

DOMAIN="cluster.example.test"
CLUSTER="sandbox"
CHECKOUT="${SB}/home/.gentian/gentian-deployments"
KERNEL="${CHECKOUT}/clusters/${CLUSTER}/kernel"
mkdir -p "${GPGHOME}" "${SB}/home/.gentian"; chmod 700 "${GPGHOME}"

# The cluster's own key, made the way the installer makes it, and the stray
# one: same role, same cluster, the placeholder address.
raw_gpg --quick-generate-key "Gentian break-glass (${CLUSTER}) <gentian-break-glass@${DOMAIN}>" ed25519 sign never >/dev/null 2>&1
raw_gpg --quick-generate-key "Gentian break-glass (${CLUSTER}) <gentian-break-glass@cluster.invalid>" ed25519 sign never >/dev/null 2>&1
OWN="$(fpr_of "gentian-break-glass@${DOMAIN}")"
STRAY="$(fpr_of "gentian-break-glass@cluster.invalid")"
if [[ -z "${OWN}" || -z "${STRAY}" || "${OWN}" == "${STRAY}" ]]; then
    echo "  ${YELLOW}skipped${NC}: this gpg could not generate the two test keys."
    exit 0
fi

g init --bare -b main "${SB}/origin.git"
g clone "${SB}/origin.git" "${CHECKOUT}"
g -C "${CHECKOUT}" checkout -b main
mkdir -p "${KERNEL}/signing"
record_keys() {   # record_keys <break-glass long id, or empty for no file>
    rm -f "${KERNEL}/signing/keys.env"
    [[ -n "${1:-}" ]] || return 0
    printf '# The keys Argo CD will accept commits from.\nGENTIAN_SIGNING_KEY_DIRECTOR=%s\nGENTIAN_SIGNING_KEY_BREAK_GLASS=%s\n' \
        "AAAAAAAAAAAAAAAA" "$1" > "${KERNEL}/signing/keys.env"
}

# run <shell> [VAR=value ...] -- library functions in a fresh shell, the way
# the installer loads them.
run() {
    local script="$1"; shift
    # shellcheck disable=SC2016 # the inner script is for the child shell to expand
    env -i HOME="${SB}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" GENTIAN_GPG_HOME="${GPGHOME}" \
        GENTIAN_DEPLOYMENTS_PATH="${CHECKOUT}" GENTIAN_DEPLOYMENTS_CLUSTER_ID="${CLUSTER}" SCRIPT="${script}" "$@" \
        bash -c 'source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1; trap - ERR; set +e; eval "${SCRIPT}"' 2>&1
}
is() {   # is <what> <got> <want>
    if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "    got:    $2"$'\n'"    wanted: $3"; fi
}

# --- a read-only lookup leaves the keyring as it is ---------------------------
# First, while gpg still owes its trust database an update for the two new
# keys: that is when an ordinary --list-keys rewrites trustdb.gpg, and the
# lookup a dry run makes must not. The agent is stopped so that its sockets
# are not part of the comparison.
keyring() {
    python3 - "${GPGHOME}" <<'PY'
import hashlib, os, stat, sys
for base, dirs, files in sorted(os.walk(sys.argv[1])):
    for name in sorted(dirs + files):
        p = os.path.join(base, name)
        st = os.lstat(p)
        if stat.S_ISREG(st.st_mode):
            with open(p, "rb") as f:
                print(p, oct(st.st_mode), st.st_mtime_ns, hashlib.sha256(f.read()).hexdigest())
        elif not stat.S_ISSOCK(st.st_mode):
            print(p, oct(st.st_mode))
PY
}
gpgconf --homedir "${GPGHOME}" --kill all >/dev/null 2>&1 || true
record_keys "${OWN: -16}"
before="$(keyring)"
found="$(run 'gentian_signing_key_id break-glass' GENTIAN_DRY_RUN=1)"
if [[ "${found}" == "${OWN}" && "$(keyring)" == "${before}" ]]; then
    ok "--dry-run finds the recorded key, and every file of the keyring is byte for byte what it was"
else
    bad "--dry-run finds the recorded key, and every file of the keyring is byte for byte what it was" \
        "    found: ${found}"$'\n'"$(diff <(printf '%s\n' "${before}") <(keyring) | sed 's/^/    /')"
fi

# --- the recorded key --------------------------------------------------------
is "recorded in keys.env, kernel domain not read yet: found" \
    "$(run 'gentian_signing_key_id break-glass')" "${OWN}"
is "recorded in keys.env, kernel domain known: the same key" \
    "$(run 'gentian_signing_key_id break-glass' KERNEL_DOMAIN="${DOMAIN}")" "${OWN}"
before="$(key_count)"
out="$(run 'gentian_ensure_signing_key break-glass; echo " rc=$?"')"
if [[ "${out}" == "${OWN} rc=0" && "$(key_count)" == "${before}" ]]; then
    ok "asked to make sure there is one, domain not read yet: answers with it and generates nothing"
else
    bad "asked to make sure there is one, domain not read yet: answers with it and generates nothing" "${out} (keys: ${before} -> $(key_count))"
fi
out="$(run 'gentian_git_sign_args break-glass')"
if [[ "${out}" == *"user.signingkey=${OWN}"* ]]; then ok "git is told to sign with it"; else bad "git is told to sign with it" "${out}"; fi
is "its long id, the form keys.env and Argo CD use" "$(run 'gentian_signing_key_long_id break-glass')" "${OWN: -16}"
if run 'gentian_export_public_key break-glass' | raw_gpg --show-keys --with-colons 2>/dev/null | grep -q "${OWN}"; then
    ok "the public half that is exported is that key's"
else
    bad "the public half that is exported is that key's"
fi

# --- the stray key -----------------------------------------------------------
record_keys ""
before="$(key_count)"
is "no keys.env, domain not read yet: nothing is found -- the stray key under the placeholder address is not an answer" \
    "$(run 'gentian_signing_key_id break-glass')" ""
out="$(run 'gentian_ensure_signing_key break-glass; echo " rc=$?"')"
if [[ "${out}" == *"rc=1"* && "$(key_count)" == "${before}" ]]; then
    ok "and none is generated while the domain is unknown"
else
    bad "and none is generated while the domain is unknown" "${out} (keys: ${before} -> $(key_count))"
fi
is "no keys.env, domain known: found by the cluster's address, as on a first install" \
    "$(run 'gentian_signing_key_id break-glass' KERNEL_DOMAIN="${DOMAIN}")" "${OWN}"
record_keys "${OWN: -16}"
is "keys.env records the cluster's key and the stray one is in the keyring: the recorded one, with the placeholder domain in force" \
    "$(run 'gentian_signing_key_id break-glass' KERNEL_DOMAIN="cluster.invalid")" "${OWN}"

# keys.env is read as a key id and nothing else.
printf 'GENTIAN_SIGNING_KEY_BREAK_GLASS=--export-secret-keys\n' > "${KERNEL}/signing/keys.env"
is "a keys.env entry that is not a key id is not handed to gpg" "$(run 'gentian_signing_recorded_id break-glass')" ""

# --- read-only runs ----------------------------------------------------------
record_keys ""
before="$(key_count)"
out="$(run 'gentian_ensure_signing_key break-glass; echo " rc=$?"' KERNEL_DOMAIN="other.example.test" GENTIAN_DRY_RUN=1)"
if [[ "${out}" == *"rc=1"* && "${out}" == *"Would generate"* && "$(key_count)" == "${before}" ]]; then
    ok "--dry-run, no key for this cluster: says one would be generated, and generates none"
else
    bad "--dry-run, no key for this cluster: says one would be generated, and generates none" "${out}"
fi
out="$(run 'gentian_signing_key_id break-glass; echo " rc=$?"' KERNEL_DOMAIN="${DOMAIN}" GENTIAN_DRY_RUN=1 GENTIAN_GPG_HOME="${SB}/absent")"
if [[ "${out}" == " rc=0" && ! -e "${SB}/absent" ]]; then
    ok "--dry-run, no keyring on this host: nothing found, and no keyring directory is made"
else
    bad "--dry-run, no keyring on this host: nothing found, and no keyring directory is made" "${out}"
fi
out="$(run 'gentian_signing_key_id break-glass; echo " rc=$?"' KERNEL_DOMAIN="${DOMAIN}" INSTALL_VALIDATE_ONLY=1 GENTIAN_GPG_HOME="${SB}/absent")"
if [[ "${out}" == " rc=0" && ! -e "${SB}/absent" ]]; then ok "--validate: the same"; else bad "--validate: the same" "${out}"; fi

# --- a head signed by the stray key is covered -------------------------------
record_keys "${OWN: -16}"
g -C "${CHECKOUT}" add -A; g -C "${CHECKOUT}" commit -m "the cluster"
g -C "${CHECKOUT}" push origin main
# What the accident pushed: a commit signed by the key nothing lists.
printf '#!/usr/bin/env bash\nexec gpg --homedir "%s" --batch --yes --pinentry-mode loopback --passphrase "" "$@"\n' "${GPGHOME}" > "${SB}/git-gpg"
chmod 700 "${SB}/git-gpg"
git -C "${CHECKOUT}" -c user.name=t -c user.email=t@t -c gpg.program="${SB}/git-gpg" -c user.signingkey="${STRAY}" \
    -c commit.gpgsign=true -c gpg.format=openpgp commit -q --allow-empty -m "signed by the stray key" >/dev/null 2>&1
g -C "${CHECKOUT}" push origin main
head_key() { git -C "${SB}/origin.git" -c gpg.program="${SB}/git-gpg" log -1 --format='%GK' main 2>/dev/null; }
is "(the head of the repository is signed by the stray key)" "$(head_key)" "${STRAY: -16}"

out="$(run 'gentian_sign_unsigned_head "'"${KERNEL}"'" "'"${CLUSTER}"'"; echo " rc=$?"' KERNEL_DOMAIN="${DOMAIN}" GENTIAN_DRY_RUN=1)"
is "--dry-run leaves that head as it is" "$(head_key)" "${STRAY: -16}"

out="$(run 'gentian_sign_unsigned_head "'"${KERNEL}"'" "'"${CLUSTER}"'"; echo " rc=$?"' KERNEL_DOMAIN="${DOMAIN}")"
if [[ "${out}" == *"rc=0"* && "$(head_key)" == "${OWN: -16}" ]]; then
    ok "an install puts a commit signed by the recorded key on top of it"
else
    bad "an install puts a commit signed by the recorded key on top of it" "${out}"$'\n'"    head is signed by: $(head_key)"
fi
if [[ "$(git -C "${SB}/origin.git" log -1 --format=%s main)" == *"sign the head"* \
      && "$(git -C "${SB}/origin.git" diff --stat main~1 main | wc -l | tr -d ' ')" == "0" ]]; then
    ok "and that commit changes no file"
else
    bad "and that commit changes no file"
fi
tip="$(git -C "${SB}/origin.git" rev-parse main)"
run 'gentian_sign_unsigned_head "'"${KERNEL}"'" "'"${CLUSTER}"'"' KERNEL_DOMAIN="${DOMAIN}" >/dev/null
is "a head that is signed by a listed key is left alone" "$(git -C "${SB}/origin.git" rev-parse main)" "${tip}"
is "the keyring still holds the two keys it started with" "$(key_count)" "2"

echo ""
if [[ ${fail} -eq 0 ]]; then
    echo "${GREEN}${pass} checks passed.${NC}"
    exit 0
fi
echo "${RED}${fail} failed${NC}, ${pass} passed."
exit 1
