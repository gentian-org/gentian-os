#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-openbao-snapshot.sh
# =============================================================================
# The vault's snapshot and its two procedures, against real OpenBao servers
# on this host, with no cluster (docs/guides/recovery.md §4 and §5):
#
#   the move     a vault on file storage, with a secret in it, is moved to
#                Raft as §5 does it -- bao operator migrate into a staging
#                directory, the result copied beside the file data -- and
#                starts on Raft with the secret; started on file storage
#                again, the file data is still there (the rollback);
#   the job      charts/gentian-os/files/openbao-snapshot.sh, the script the
#                CronJob runs, logs in with a token for its audience, reads a
#                snapshot, encrypts it to the backup recipient and writes it
#                beside earlier ones, keeping the newest KEEP; on a vault on
#                file storage it says so and writes nothing; without a
#                recipient, or a store, it refuses;
#   the restore  the snapshot, decrypted with the identity, restored into a
#                new vault with snapshot-force (§4), unsealed with the old
#                vault's key, holds the secret;
#   the seal     a transit seal on Raft, set up as
#                scripts/bootstrap/init-openbao-transit.sh does, and a vault
#                it unseals: the same job snapshots the seal with the seal's
#                own role, under its own prefix; both volumes lost, the seal
#                is restored from its snapshot and unsealed with the one key
#                the recovery kit carries (TRANSIT_UNSEAL_KEY), and the vault
#                then unseals itself through it.
#
# The cluster's pieces are stood in for: the Kubernetes auth mount by a JWT
# mount (the login body is the same, {role, jwt}), the object store by an
# rclone on PATH that writes to a directory. The policies are the ones the
# Cluster composition renders and kernel/openbao/transit-snapshot-policy.hcl.
#
# Needs bao, age, age-keygen, openssl, curl, jq, python3 with PyYAML. Skips
# (exit 0) when one is missing.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
ROOT="$(pwd)"

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"
check() {
    if [[ "$2" == 1 ]]; then ok "$1"; else bad "$1"; fi
}

# names <dir> — the names in a directory, one a line, dot files too; nothing
# for a directory that is not there.
names() {
    local f
    for f in "$1"/* "$1"/.[!.]*; do
        if [[ -e "${f}" ]]; then printf '%s\n' "${f##*/}"; fi
    done
}

for cmd in bao age age-keygen openssl curl jq python3; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
        echo "${YELLOW}SKIP${NC} — ${cmd} not found."
        exit 0
    fi
done
python3 -c 'import yaml' 2>/dev/null || { echo "${YELLOW}SKIP${NC} — python3 has no PyYAML."; exit 0; }

SCRIPT="${ROOT}/charts/gentian-os/files/openbao-snapshot.sh"
GOLDEN="${ROOT}/crossplane/tests/unit/render/cluster-bootstrap/expected.yaml"

W="$(mktemp -d)"
PID=""
stop_server() {
    if [[ -n "${PID}" ]]; then
        kill "${PID}" 2>/dev/null
        wait "${PID}" 2>/dev/null
        PID=""
    fi
}
# shellcheck disable=SC2317,SC2329 # called by the EXIT trap
cleanup() { stop_server; rm -rf "${W}"; }
trap cleanup EXIT INT TERM

# A port pair nobody is on: the API and the cluster port beside it.
PORT=18300
while [[ ${PORT} -lt 18380 ]]; do
    if ! (exec 3<>"/dev/tcp/127.0.0.1/${PORT}") 2>/dev/null &&
        ! (exec 3<>"/dev/tcp/127.0.0.1/$((PORT + 1))") 2>/dev/null; then
        break
    fi
    PORT=$((PORT + 2))
done
[[ ${PORT} -lt 18380 ]] || { echo "${RED}FAIL${NC} — no free port in 18300-18379."; exit 1; }

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=127.0.0.1" \
    -addext "subjectAltName=IP:127.0.0.1" -keyout "${W}/tls.key" -out "${W}/tls.crt" >/dev/null 2>&1
export BAO_ADDR="https://127.0.0.1:${PORT}" BAO_CACERT="${W}/tls.crt"
unset BAO_TOKEN VAULT_TOKEN

# start_server <storage stanza> — one server on ${W}/data, the vault's volume.
start_server() {
    stop_server
    cat > "${W}/server.hcl" <<EOF
listener "tcp" {
  address         = "127.0.0.1:${PORT}"
  cluster_address = "127.0.0.1:$((PORT + 1))"
  tls_cert_file   = "${W}/tls.crt"
  tls_key_file    = "${W}/tls.key"
}
api_addr     = "https://127.0.0.1:${PORT}"
cluster_addr = "https://127.0.0.1:$((PORT + 1))"
$1
EOF
    bao server -config="${W}/server.hcl" > "${W}/server.log" 2>&1 &
    PID=$!
    local _
    for _ in $(seq 1 100); do
        curl -s --cacert "${W}/tls.crt" "${BAO_ADDR}/v1/sys/seal-status" >/dev/null 2>&1 && return 0
        sleep 0.1
    done
    echo "${RED}the server did not start:${NC}"; tail -5 "${W}/server.log"
    return 1
}
FILE_STORAGE="storage \"file\" { path = \"${W}/data\" }"
RAFT_STORAGE="storage \"raft\" {
  path    = \"${W}/data\"
  node_id = \"openbao-0\"
}"
storage_type() { curl -s --cacert "${W}/tls.crt" "${BAO_ADDR}/v1/sys/seal-status" | jq -r '.storage_type'; }
sealed() { curl -s --cacert "${W}/tls.crt" "${BAO_ADDR}/v1/sys/seal-status" | jq -r '.sealed'; }

echo ""
echo "The vault's storage: the move to Raft (recovery.md §5)"
echo ""

mkdir -p "${W}/data"
start_server "${FILE_STORAGE}" || exit 1
bao operator init -key-shares=1 -key-threshold=1 -format=json > "${W}/init.json" 2>/dev/null
UNSEAL="$(jq -r '.unseal_keys_b64[0] // .unseal_keys_base64[0]' "${W}/init.json")"
ROOT_TOKEN="$(jq -r '.root_token' "${W}/init.json")"
bao operator unseal "${UNSEAL}" >/dev/null
export BAO_TOKEN="${ROOT_TOKEN}"
bao secrets enable -path=secret kv-v2 >/dev/null 2>&1
bao kv put -mount=secret gentian-os/kernel/probe value=before-the-move >/dev/null 2>&1
check "a vault on file storage holds a secret" \
    "$([[ "$(storage_type)" == file && "$(bao kv get -mount=secret -field=value gentian-os/kernel/probe 2>/dev/null)" == before-the-move ]] && echo 1 || echo 0)"

# The job on a vault that is not on Raft: says so, writes nothing.
mkdir -p "${W}/bin" "${W}/s3/b/openbao"
cat > "${W}/bin/rclone" <<'EOF'
#!/bin/sh
# An object store in a directory: snap:<bucket>/<key> is ${FAKE_S3}/<bucket>/<key>.
set -eu
p() { printf '%s/%s' "${FAKE_S3}" "${1#snap:}"; }
case "$1" in
    copyto) mkdir -p "$(dirname "$(p "$3")")"; cp "$2" "$(p "$3")" ;;
    lsf) ls -1 "$(p "$3")" ;;
    deletefile) rm "$(p "$2")" ;;
    *) echo "fake rclone: $1 is not stood in for" >&2; exit 2 ;;
esac
EOF
chmod +x "${W}/bin/rclone"
age-keygen -o "${W}/identity.txt" 2>/dev/null
RECIPIENT="$(age-keygen -y "${W}/identity.txt")"

run_job() {
    env -i PATH="${W}/bin:${PATH}" HOME="${W}" FAKE_S3="${W}/s3" \
        BAO_ADDR="${BAO_ADDR}" BAO_CACERT="${W}/tls.crt" BAO_AUTH_PATH=jwt BAO_ROLE=openbao-snapshot \
        JWT_FILE="${W}/jwt" KEEP=14 SNAPSHOT_NAME=openbao SNAPSHOT_PREFIX=openbao WORK_DIR="${W}/work" \
        S3_ENDPOINT=http://s3.invalid:9000 S3_BUCKET=b S3_ACCESS_KEY=ak S3_SECRET_KEY=sk S3_REGION=us-east-1 \
        "$@" sh "${SCRIPT}" > "${W}/job.log" 2>&1
}
run_job BACKUP_AGE_RECIPIENTS="${RECIPIENT}"
rc=$?
check "the job on a vault on file storage: exits 0, says NOT ON RAFT and points at §5, writes no snapshot" \
    "$([[ ${rc} -eq 0 ]] && grep -q 'NOT ON RAFT' "${W}/job.log" && grep -q 'recovery.md §5' "${W}/job.log" &&
        [[ "$(names "${W}/s3/b/openbao")" == storage.file ]] && echo 1 || echo 0)"
check "  and leaves the marker storage.file the operator reads" \
    "$([[ -f "${W}/s3/b/openbao/storage.file" && ! -s "${W}/s3/b/openbao/storage.file" ]] && echo 1 || echo 0)"

# The move, as §5 does it: the vault stopped, migrate into staging, the
# result copied beside the file data.
stop_server
mkdir -p "${W}/staging"
cat > "${W}/migrate.hcl" <<EOF
storage_source "file" {
  path = "${W}/data"
}
storage_destination "raft" {
  path    = "${W}/staging"
  node_id = "openbao-0"
}
cluster_addr = "https://127.0.0.1:$((PORT + 1))"
EOF
bao operator migrate -config="${W}/migrate.hcl" > "${W}/migrate.log" 2>&1
rc=$?
check "bao operator migrate copies file storage into a staging Raft directory" \
    "$([[ ${rc} -eq 0 && -f "${W}/staging/vault.db" ]] && echo 1 || echo 0)"
cp -a "${W}/staging/." "${W}/data/"

start_server "${RAFT_STORAGE}" || exit 1
bao operator unseal "${UNSEAL}" >/dev/null
check "started on Raft on the same volume, it is the same vault: unsealed by its key, the secret is there" \
    "$([[ "$(storage_type)" == raft && "$(sealed)" == false &&
        "$(bao kv get -mount=secret -field=value gentian-os/kernel/probe 2>/dev/null)" == before-the-move ]] && echo 1 || echo 0)"
bao kv put -mount=secret gentian-os/kernel/probe value=on-raft >/dev/null 2>&1

# The rollback: the file data was left where it was.
start_server "${FILE_STORAGE}" || exit 1
bao operator unseal "${UNSEAL}" >/dev/null
check "rollback: started on file storage again, the file data is as it was before the move" \
    "$([[ "$(storage_type)" == file &&
        "$(bao kv get -mount=secret -field=value gentian-os/kernel/probe 2>/dev/null)" == before-the-move ]] && echo 1 || echo 0)"
start_server "${RAFT_STORAGE}" || exit 1
bao operator unseal "${UNSEAL}" >/dev/null

echo ""
echo "The daily snapshot (recovery.md §4)"
echo ""

# The role the composition makes, on a JWT mount standing in for Kubernetes.
python3 - "${GOLDEN}" > "${W}/openbao-snapshot.hcl" <<'PY'
import sys, yaml
for doc in yaml.safe_load_all(open(sys.argv[1])):
    if doc and doc.get("kind") == "Policy" and doc["spec"]["forProvider"]["name"] == "openbao-snapshot":
        print(doc["spec"]["forProvider"]["policy"])
PY
bao policy write openbao-snapshot "${W}/openbao-snapshot.hcl" >/dev/null
openssl genrsa -out "${W}/jwt.key" 2048 2>/dev/null
openssl rsa -in "${W}/jwt.key" -pubout -out "${W}/jwt.pub" 2>/dev/null
bao auth enable jwt >/dev/null 2>&1
bao write auth/jwt/config jwt_validation_pubkeys=@"${W}/jwt.pub" >/dev/null
bao write auth/jwt/role/openbao-snapshot role_type=jwt bound_audiences=vault://openbao-snapshot \
    user_claim=sub token_policies=openbao-snapshot token_no_default_policy=true \
    token_ttl=300 token_max_ttl=300 >/dev/null
b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
now="$(date +%s)"
hdr="$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)"
payload="$(printf '{"sub":"system:serviceaccount:kernel-secrets:openbao-snapshot","aud":"vault://openbao-snapshot","iat":%d,"nbf":%d,"exp":%d}' \
    "$((now - 60))" "$((now - 60))" "$((now + 600))" | b64url)"
sig="$(printf '%s.%s' "${hdr}" "${payload}" | openssl dgst -sha256 -sign "${W}/jwt.key" -binary | b64url)"
printf '%s.%s.%s' "${hdr}" "${payload}" "${sig}" > "${W}/jwt"
unset BAO_TOKEN

# Fifteen earlier snapshots and something that is none.
for d in $(seq -w 1 15); do
    printf 'old' > "${W}/s3/b/openbao/openbao-202610${d}T021700Z.snap.age"
done
printf 'x' > "${W}/s3/b/openbao/notes.txt"

run_job BACKUP_AGE_RECIPIENTS="${RECIPIENT}"
rc=$?
newest="$(names "${W}/s3/b/openbao" | grep -E '^openbao-[0-9]{8}T[0-9]{6}Z\.snap\.age$' | sort | tail -1)"
check "the job on Raft: exits 0 and writes a new snapshot" \
    "$([[ ${rc} -eq 0 && "${newest}" != openbao-202610* && -s "${W}/s3/b/openbao/${newest}" ]] && echo 1 || echo 0)"
[[ ${rc} -eq 0 ]] || sed 's/^/      /' "${W}/job.log"
check "the marker now says raft, and storage.file is gone" \
    "$([[ -f "${W}/s3/b/openbao/storage.raft" && ! -f "${W}/s3/b/openbao/storage.file" ]] && echo 1 || echo 0)"
check "it keeps the newest 14 and deletes the older, and leaves what is not a snapshot" \
    "$([[ "$(names "${W}/s3/b/openbao" | grep -c '\.snap\.age$')" -eq 14 && -f "${W}/s3/b/openbao/notes.txt" &&
        ! -f "${W}/s3/b/openbao/openbao-20261001T021700Z.snap.age" ]] && echo 1 || echo 0)"
check "the object is age ciphertext, and opens with the backup identity alone" \
    "$(head -c 21 "${W}/s3/b/openbao/${newest}" | grep -q 'age-encryption.org/v1' &&
        age -d -i "${W}/identity.txt" -o "${W}/restore.snap" "${W}/s3/b/openbao/${newest}" 2>/dev/null &&
        gzip -t "${W}/restore.snap" 2>/dev/null && echo 1 || echo 0)"
check "no plaintext snapshot and no token are left in the work directory" \
    "$(names "${W}/work" | grep -qv '^recipients\.txt$' && echo 0 || echo 1)"
check "the log names neither the token nor the key" \
    "$(grep -qE 's\.[A-Za-z0-9]{20,}|hvs\.|AGE-SECRET-KEY' "${W}/job.log" && echo 0 || echo 1)"

# Refusals.
run_job
rc=$?
check "with no recipient it refuses, and says so" \
    "$([[ ${rc} -ne 0 ]] && grep -q 'no backup recipient' "${W}/job.log" && echo 1 || echo 0)"
run_job RECIPIENTS_FILE="${W}/recipients-from-the-vault"
rc=$?
check "a recipient file that is not there is no recipient" \
    "$([[ ${rc} -ne 0 ]] && grep -q 'no backup recipient' "${W}/job.log" && echo 1 || echo 0)"
printf '%s' "${RECIPIENT}" > "${W}/recipients-from-the-vault"
# A second apart, so the snapshot is a new object rather than the same name.
sleep 1
run_job RECIPIENTS_FILE="${W}/recipients-from-the-vault"
rc=$?
check "the recipient the recovery kit wrote to the vault is used when the values pin none" \
    "$([[ ${rc} -eq 0 ]] && echo 1 || echo 0)"
run_job BACKUP_AGE_RECIPIENTS="${RECIPIENT}" S3_BUCKET=''
rc=$?
check "before the operator made the store it refuses, and says so" \
    "$([[ ${rc} -ne 0 ]] && grep -q 'has not made the snapshot store' "${W}/job.log" && echo 1 || echo 0)"
run_job BACKUP_AGE_RECIPIENTS="not-a-key"
rc=$?
check "a recipient that is not an age key is refused" \
    "$([[ ${rc} -ne 0 ]] && grep -q 'not an age recipient' "${W}/job.log" && echo 1 || echo 0)"

# The snapshot read fails before curl writes a byte (the connection refused):
# the job fails at once and logs curl's error, rather than wait for a writer
# until the Job's deadline. A curl on PATH that fails on the snapshot alone.
mkdir -p "${W}/failing-curl"
cat > "${W}/failing-curl/curl" <<EOF
#!/bin/sh
for a in "\$@"; do
    case "\${a}" in
        */sys/storage/raft/snapshot)
            echo 'curl: (7) Failed to connect to 127.0.0.1 port ${PORT}: Connection refused' >&2
            exit 7 ;;
    esac
done
exec "$(command -v curl)" "\$@"
EOF
chmod +x "${W}/failing-curl/curl"
before="$(names "${W}/s3/b/openbao" | grep -c '\.snap\.age$')"
started="$(date +%s)"
# Under a watchdog of the shell's own (macOS has no timeout(1)): a job that
# hung would otherwise hang the test with it. A killed job's status is 124.
( PATH="${W}/failing-curl:${PATH}"; run_job BACKUP_AGE_RECIPIENTS="${RECIPIENT}" ) &
job_pid=$!
( sleep 60; pkill -P "${job_pid}" 2>/dev/null; kill "${job_pid}" 2>/dev/null; touch "${W}/job-killed" ) &
watchdog=$!
rc=0
wait "${job_pid}" || rc=$?
kill "${watchdog}" 2>/dev/null; wait "${watchdog}" 2>/dev/null
if [[ -e "${W}/job-killed" ]]; then rc=124; fi
took=$(( $(date +%s) - started ))
check "the snapshot read fails before a byte: the job fails within seconds, logs curl's error, uploads nothing" \
    "$([[ ${rc} -ne 0 && ${rc} -ne 124 && ${took} -lt 30 ]] && grep -q 'Connection refused' "${W}/job.log" &&
        grep -q 'could not be read (curl 7)' "${W}/job.log" &&
        [[ "$(names "${W}/s3/b/openbao" | grep -c '\.snap\.age$')" -eq "${before}" ]] && echo 1 || echo 0)"
[[ ${rc} -ne 124 ]] || echo "      the job hung until the timeout"
check "  and leaves no staged ciphertext behind" \
    "$(names "${W}/work" | grep -qv '^recipients\.txt$' && echo 0 || echo 1)"

echo ""
echo "The restore (recovery.md §4)"
echo ""

# The vault is lost: a new one on an empty volume, initialized anew.
stop_server
rm -rf "${W}/data"; mkdir -p "${W}/data"
start_server "${RAFT_STORAGE}" || exit 1
bao operator init -key-shares=1 -key-threshold=1 -format=json > "${W}/init2.json" 2>/dev/null
bao operator unseal "$(jq -r '.unseal_keys_b64[0] // .unseal_keys_base64[0]' "${W}/init2.json")" >/dev/null
export BAO_TOKEN
BAO_TOKEN="$(jq -r '.root_token' "${W}/init2.json")"
bao operator raft snapshot restore -force "${W}/restore.snap" > "${W}/restore.log" 2>&1
rc=$?
check "snapshot-force restores the decrypted snapshot into the new vault" \
    "$([[ ${rc} -eq 0 ]] && echo 1 || echo 0)"
[[ "$(sealed)" == true ]] && bao operator unseal "${UNSEAL}" >/dev/null 2>&1
BAO_TOKEN="${ROOT_TOKEN}"
check "unsealed with the old vault's key, it answers with the old root token and holds the secret" \
    "$([[ "$(sealed)" == false && "$(bao kv get -mount=secret -field=value gentian-os/kernel/probe 2>/dev/null)" == on-raft ]] && echo 1 || echo 0)"
stop_server

echo ""
echo "The transit seal: its snapshot, and its restore before the vault's (recovery.md §4)"
echo ""

# The seal, as the platform runs it: plain HTTP, Raft, one key share,
# initialized through the API as scripts/bootstrap/init-openbao-transit.sh
# does -- so its unseal key is keys_base64[0], the value the recovery kit
# carries as TRANSIT_UNSEAL_KEY.
TPORT=$((PORT + 2))
while [[ ${TPORT} -lt 18400 ]]; do
    if ! (exec 3<>"/dev/tcp/127.0.0.1/${TPORT}") 2>/dev/null &&
        ! (exec 3<>"/dev/tcp/127.0.0.1/$((TPORT + 1))") 2>/dev/null; then
        break
    fi
    TPORT=$((TPORT + 2))
done
TADDR="http://127.0.0.1:${TPORT}"
TPID=""
stop_transit() {
    if [[ -n "${TPID}" ]]; then kill "${TPID}" 2>/dev/null; wait "${TPID}" 2>/dev/null; TPID=""; fi
}
trap 'stop_transit; cleanup' EXIT INT TERM
start_transit() {
    stop_transit
    mkdir -p "${W}/transit"
    cat > "${W}/transit.hcl" <<EOF
listener "tcp" {
  address         = "127.0.0.1:${TPORT}"
  cluster_address = "127.0.0.1:$((TPORT + 1))"
  tls_disable     = 1
}
api_addr     = "${TADDR}"
cluster_addr = "https://127.0.0.1:$((TPORT + 1))"
storage "raft" {
  path    = "${W}/transit"
  node_id = "openbao-transit-0"
}
EOF
    bao server -config="${W}/transit.hcl" > "${W}/transit.log" 2>&1 &
    TPID=$!
    local _
    for _ in $(seq 1 100); do
        curl -s "${TADDR}/v1/sys/seal-status" >/dev/null 2>&1 && return 0
        sleep 0.1
    done
    echo "${RED}the seal did not start:${NC}"; tail -5 "${W}/transit.log"
    return 1
}
t_api() { # <method> <path> [body] — the seal's API with T_ROOT
    if [[ $# -ge 3 ]]; then
        curl -sf -X "$1" -H "X-Vault-Token: ${T_ROOT}" -H "Content-Type: application/json" -d "$3" "${TADDR}/v1/$2"
    else
        curl -sf -X "$1" -H "X-Vault-Token: ${T_ROOT}" "${TADDR}/v1/$2"
    fi
}
t_sealed() { curl -s "${TADDR}/v1/sys/seal-status" | jq -r '.sealed'; }
# Raft elects its one node leader a moment after the unseal; until then the
# API answers nothing a test can use.
t_active() {
    local _
    for _ in $(seq 1 100); do
        [[ "$(curl -s -o /dev/null -w '%{http_code}' "${TADDR}/v1/sys/health")" == 200 ]] && return 0
        sleep 0.1
    done
    return 1
}

start_transit || exit 1
curl -sf -X PUT -d '{"secret_shares": 1, "secret_threshold": 1}' "${TADDR}/v1/sys/init" > "${W}/transit-init.json"
TRANSIT_UNSEAL_KEY="$(jq -r '.keys_base64[0]' "${W}/transit-init.json")"
T_ROOT="$(jq -r '.root_token' "${W}/transit-init.json")"
curl -sf -X PUT -d "{\"key\": \"${TRANSIT_UNSEAL_KEY}\"}" "${TADDR}/v1/sys/unseal" >/dev/null
t_active
t_api POST sys/mounts/transit '{"type": "transit"}' >/dev/null
t_api POST transit/keys/autounseal '{"type": "aes256-gcm96"}' >/dev/null
t_api PUT sys/policies/acl/transit-autounseal \
    "$(jq -n --arg p 'path "transit/encrypt/autounseal" { capabilities = ["update"] }
path "transit/decrypt/autounseal" { capabilities = ["update"] }' '{policy: $p}')" >/dev/null
AUTOUNSEAL_TOKEN="$(t_api POST auth/token/create-orphan '{"policies":["transit-autounseal"],"period":"8760h"}' | jq -r '.auth.client_token')"
# The snapshot role, with the policy the seal is given
# (kernel/openbao/transit-snapshot-policy.hcl), on a JWT mount standing in for
# Kubernetes.
t_api PUT sys/policies/acl/openbao-transit-snapshot \
    "$(jq -n --rawfile p "${ROOT}/kernel/openbao/transit-snapshot-policy.hcl" '{policy: $p}')" >/dev/null
t_api POST sys/auth/jwt '{"type": "jwt"}' >/dev/null
t_api POST auth/jwt/config "$(jq -n --rawfile k "${W}/jwt.pub" '{jwt_validation_pubkeys: [$k]}')" >/dev/null
t_api POST auth/jwt/role/openbao-transit-snapshot '{"role_type":"jwt","bound_audiences":["vault://openbao-transit-snapshot"],"user_claim":"sub","token_policies":["openbao-transit-snapshot"],"token_no_default_policy":true,"token_ttl":300,"token_max_ttl":300}' >/dev/null
check "the seal is on Raft, initialized with one share, and holds the auto-unseal key" \
    "$([[ "$(curl -s "${TADDR}/v1/sys/seal-status" | jq -r '.storage_type,.n,.t' | tr '\n' ' ')" == "raft 1 1 " &&
        -n "$(t_api GET transit/keys/autounseal | jq -r '.data.name // empty')" ]] && echo 1 || echo 0)"

# A vault unsealed by the seal, with a secret.
rm -rf "${W}/data"; mkdir -p "${W}/data"
SEALED_BY_TRANSIT="${RAFT_STORAGE}
seal \"transit\" {
  address    = \"${TADDR}\"
  token      = \"${AUTOUNSEAL_TOKEN}\"
  key_name   = \"autounseal\"
  mount_path = \"transit/\"
}"
start_server "${SEALED_BY_TRANSIT}" || exit 1
bao operator init -recovery-shares=1 -recovery-threshold=1 -format=json > "${W}/init3.json" 2>/dev/null
for _ in $(seq 1 100); do
    [[ "$(curl -s -o /dev/null -w '%{http_code}' --cacert "${W}/tls.crt" "${BAO_ADDR}/v1/sys/health")" == 200 ]] && break
    sleep 0.1
done
BAO_TOKEN="$(jq -r '.root_token' "${W}/init3.json")"
bao secrets enable -path=secret kv-v2 >/dev/null 2>&1
bao kv put -mount=secret gentian-os/kernel/probe value=sealed-by-transit >/dev/null 2>&1
check "a vault on Raft unsealed by the seal holds a secret" \
    "$([[ "$(sealed)" == false && "$(bao kv get -mount=secret -field=value gentian-os/kernel/probe 2>/dev/null)" == sealed-by-transit ]] && echo 1 || echo 0)"
unset BAO_TOKEN

# The seal's job: plain HTTP, its own role and audience, its own prefix.
payload="$(printf '{"sub":"system:serviceaccount:kernel-seal:openbao-transit-snapshot","aud":"vault://openbao-transit-snapshot","iat":%d,"nbf":%d,"exp":%d}' \
    "$((now - 60))" "$((now - 60))" "$((now + 600))" | b64url)"
sig="$(printf '%s.%s' "${hdr}" "${payload}" | openssl dgst -sha256 -sign "${W}/jwt.key" -binary | b64url)"
printf '%s.%s.%s' "${hdr}" "${payload}" "${sig}" > "${W}/jwt-transit"
run_job BACKUP_AGE_RECIPIENTS="${RECIPIENT}" BAO_ADDR="${TADDR}" BAO_CACERT= BAO_ROLE=openbao-transit-snapshot \
    JWT_FILE="${W}/jwt-transit" SNAPSHOT_NAME=openbao-transit SNAPSHOT_PREFIX=openbao-transit
rc=$?
tnewest="$(names "${W}/s3/b/openbao-transit" | grep -E '^openbao-transit-[0-9]{8}T[0-9]{6}Z\.snap\.age$' | sort | tail -1)"
check "the seal's job writes its snapshot under openbao-transit/, and the marker storage.raft" \
    "$([[ ${rc} -eq 0 && -n "${tnewest}" && -f "${W}/s3/b/openbao-transit/storage.raft" ]] && echo 1 || echo 0)"
[[ ${rc} -eq 0 ]] || sed 's/^/      /' "${W}/job.log"
check "  encrypted to the same recipient: the backup identity opens it" \
    "$(age -d -i "${W}/identity.txt" -o "${W}/transit.snap" "${W}/s3/b/openbao-transit/${tnewest}" 2>/dev/null &&
        gzip -t "${W}/transit.snap" 2>/dev/null && echo 1 || echo 0)"
run_job BACKUP_AGE_RECIPIENTS="${RECIPIENT}" BAO_ADDR="${TADDR}" BAO_CACERT= BAO_ROLE=openbao-snapshot \
    JWT_FILE="${W}/jwt" SNAPSHOT_NAME=openbao-transit SNAPSHOT_PREFIX=openbao-transit
rc=$?
check "the vault's token is not one the seal's role takes" \
    "$([[ ${rc} -ne 0 ]] && grep -q 'refused the login' "${W}/job.log" && echo 1 || echo 0)"

# Both volumes lost. The seal comes back first: a new one, initialized anew,
# which the vault cannot unseal through.
stop_server
stop_transit
rm -rf "${W}/transit"
start_transit || exit 1
curl -sf -X PUT -d '{"secret_shares": 1, "secret_threshold": 1}' "${TADDR}/v1/sys/init" > "${W}/transit-init2.json"
curl -sf -X PUT -d "{\"key\": \"$(jq -r '.keys_base64[0]' "${W}/transit-init2.json")\"}" "${TADDR}/v1/sys/unseal" >/dev/null
t_active
# (It may not start at all: the new seal knows neither its token nor its key.)
start_server "${SEALED_BY_TRANSIT}" >/dev/null 2>&1
sleep 1
check "a new seal does not unseal the vault: its key is not the old one" \
    "$([[ "$(sealed)" != false ]] && echo 1 || echo 0)"
stop_server

# The seal restored from its snapshot with the new seal's temporary root
# token, then unsealed with the kit's TRANSIT_UNSEAL_KEY.
BAO_ADDR="${TADDR}" BAO_CACERT='' BAO_TOKEN="$(jq -r '.root_token' "${W}/transit-init2.json")" \
    bao operator raft snapshot restore -force "${W}/transit.snap" > "${W}/transit-restore.log" 2>&1
rc=$?
check "snapshot-force restores the seal's snapshot into the new seal" "$([[ ${rc} -eq 0 ]] && echo 1 || echo 0)"
for _ in $(seq 1 50); do [[ "$(t_sealed)" == true ]] && break; sleep 0.1; done
check "  after the restore the seal is sealed, under the restored seal configuration (one share of one)" \
    "$([[ "$(curl -s "${TADDR}/v1/sys/seal-status" | jq -r '.sealed,.n,.t' | tr '\n' ' ')" == "true 1 1 " ]] && echo 1 || echo 0)"
curl -s -X PUT -d "{\"key\": \"$(jq -r '.keys_base64[0]' "${W}/transit-init2.json")\"}" "${TADDR}/v1/sys/unseal" >/dev/null
check "  the new seal's own key does not unseal it" "$([[ "$(t_sealed)" == true ]] && echo 1 || echo 0)"
curl -s -X PUT -d '{"reset": true}' "${TADDR}/v1/sys/unseal" >/dev/null
curl -sf -X PUT -d "{\"key\": \"${TRANSIT_UNSEAL_KEY}\"}" "${TADDR}/v1/sys/unseal" >/dev/null
t_active
check "  unsealed with TRANSIT_UNSEAL_KEY alone, it holds the auto-unseal key again" \
    "$([[ "$(t_sealed)" == false && -n "$(t_api GET transit/keys/autounseal | jq -r '.data.name // empty')" ]] && echo 1 || echo 0)"

# Then the vault, which unseals itself through the restored seal.
start_server "${SEALED_BY_TRANSIT}" || exit 1
for _ in $(seq 1 100); do
    [[ "$(curl -s -o /dev/null -w '%{http_code}' --cacert "${W}/tls.crt" "${BAO_ADDR}/v1/sys/health")" == 200 ]] && break
    sleep 0.1
done
BAO_TOKEN="$(jq -r '.root_token' "${W}/init3.json")" \
    bao kv get -mount=secret -field=value gentian-os/kernel/probe > "${W}/probe" 2>/dev/null
check "the vault unseals through the restored seal and holds its secret" \
    "$([[ "$(sealed)" == false && "$(cat "${W}/probe")" == sealed-by-transit ]] && echo 1 || echo 0)"
stop_server
stop_transit

finish
