#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-mail-edge-lab.sh — the mail edge in front of the real
# Postfix and Dovecot, in local containers
# =============================================================================
# What no rendered manifest can show: that a mail server behind the proxy of
# the mail DMZ still sees who the client is. If it did not, every sender on
# the internet would arrive from the proxy's own address -- an address inside
# the pod range Postfix trusts -- and the cluster would relay for all of them.
#
# The three charts are rendered as the mail ApplicationSet has Argo CD render
# them, the upstream Postfix chart the Release installs is rendered with the
# values the Release hands it, and the containers run THOSE: the chart's
# haproxy.cfg and command, the chart's dovecot.conf, the upstream chart's
# environment and start scripts, the images each manifest names. What is
# supplied here is what the operator and cert-manager supply on a cluster --
# the maps, the passwd-files, a certificate.
#
# Two docker networks stand for the pod network and the internet. Postfix and
# Dovecot are on the first, the proxy on both, and there is a client on each:
#
#   172.31.0.50  a host on the internet            172.30.0.60  a pod
#        |                                               |
#   172.31.0.10  [ proxy ]  172.30.0.10 ---- 172.30.0.21 Postfix, .20 Dovecot
#
# Postfix's mynetworks is the pod network, as the operator derives it.
#
# Needs docker, helm and network access (images, the upstream chart, and DNS
# for the sender domains Postfix looks up). Not part of `make verify`.
#
# Usage:
#   scripts/tests/test-mail-edge-lab.sh          run every check, then clean up
#   scripts/tests/test-mail-edge-lab.sh --keep   leave the containers running
# =============================================================================
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

KEEP=0
[[ "${1:-}" == "--keep" ]] && KEEP=1
PODS_CIDR=172.30.0.0/24
INET_CIDR=172.31.0.0/24
EDGE_OUT=172.31.0.10   # the proxy, as the internet reaches it
EDGE_IN=172.30.0.10    # the proxy, as a pod reaches it
SVC=system-mail.svc.cluster.local
W="$(mktemp -d)"
fail=0

lab_down() {
    docker rm -f mlab-dovecot mlab-postfix mlab-edge mlab-out mlab-in >/dev/null 2>&1 || true
    docker network rm mlab-pods mlab-inet >/dev/null 2>&1 || true
}

cleanup() {
    if [[ ${KEEP} -eq 0 ]]; then
        lab_down
        rm -rf "${W}"
    else
        echo "left running; files in ${W}"
    fi
}
trap cleanup EXIT

# render [extra helm arguments for the edge...] — the charts, as deployed.
render() {
    local common=(--set env=dev --set kernelDomain=lab.test --set mailServiceMode=system
                  --set edge.namespace=system-mail-dmz --set mailNamespace=system-mail)
    mkdir -p "${W}/render"
    helm template postfix-dev kernel/services/postfix/manifests -n system-mail \
        --set servicesNamespace=system-mail "${common[@]}" > "${W}/render/postfix.yaml"
    helm template dovecot-dev kernel/services/dovecot/manifests -n system-mail \
        --set servicesNamespace=system-mail "${common[@]}" > "${W}/render/dovecot.yaml"
    helm template mail-edge-dev kernel/services/mail-edge/manifests -n system-mail-dmz \
        --set servicesNamespace=system-mail-dmz "${common[@]}" "$@" > "${W}/render/edge.yaml"
}

# The upstream chart at the version the Release pins, with the Release's values.
render_upstream() {
    local version repo
    version="$(sed -n 's/^ *version: "\(.*\)"$/\1/p' kernel/services/postfix/manifests/templates/release.yaml)"
    repo="$(sed -n 's/^ *repository: \(https.*\)$/\1/p' kernel/services/postfix/manifests/templates/release.yaml)"
    helm pull mail --repo "${repo}" --version "${version}" --untar --untardir "${W}/chart" >/dev/null
    python3 - "${W}/render" <<'PY'
import sys, yaml
d = sys.argv[1]
for doc in yaml.safe_load_all(open(d + "/postfix.yaml")):
    if doc and doc["kind"] == "ConfigMap" and doc["metadata"]["name"] in ("postfix-base-values", "postfix-dev-values"):
        open(d + "/" + doc["metadata"]["name"] + ".yaml", "w").write(doc["data"]["values.yaml"])
PY
    helm template postfix-dev "${W}/chart/mail" -n system-mail \
        -f "${W}/render/postfix-base-values.yaml" -f "${W}/render/postfix-dev-values.yaml" > "${W}/render/upstream.yaml"
}

lab_up() {
    lab_down
    PODS_CIDR="${PODS_CIDR}" python3 scripts/tests/mail_edge_lab_build.py "${W}/render" "${W}/work"
    # A certificate for the names clients dial, as the cluster's wildcard is.
    mkdir -p "${W}/work/tls"
    openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=*.lab.test" \
        -addext "subjectAltName=DNS:*.lab.test,DNS:lab.test" \
        -keyout "${W}/work/tls/tls.key" -out "${W}/work/tls/tls.crt" >/dev/null 2>&1
    chmod 644 "${W}/work/tls/tls.key"
    chmod -R a+rX "${W}"
    docker network create --subnet "${PODS_CIDR}" mlab-pods >/dev/null
    docker network create --subnet "${INET_CIDR}" mlab-inet >/dev/null

    local work="${W}/work" args
    read -r -a args < "${work}/dovecot.args"
    docker run -d --name mlab-dovecot --network mlab-pods --ip 172.30.0.20 \
        --network-alias "dovecot-dev.${SVC}" --network-alias "dovecot-dev-edge.${SVC}" \
        -v "${work}/dovecot/gentian:/etc/dovecot/gentian:ro" -v "${work}/dovecot/apppw:/etc/dovecot/apppw:ro" \
        -v "${work}/dovecot/realms:/etc/dovecot/realms:ro" -v "${work}/tls:/etc/dovecot/tls:ro" \
        --tmpfs /var/mail:uid=1000,gid=1000,mode=0770 \
        --entrypoint "" "$(cat "${work}/dovecot.image")" "${args[@]}" >/dev/null

    docker run -d --name mlab-postfix --network mlab-pods --ip 172.30.0.21 \
        --network-alias "postfix-dev.${SVC}" --network-alias "postfix-dev-edge.${SVC}" \
        --env-file "${work}/postfix.env" \
        -v "${work}/tls:/var/run/certs:ro" \
        -v "${work}/postfix/init/_enable_tls.sh:/docker-init.d/_enable_tls.sh:ro" \
        -v "${work}/postfix/init/gentian-edge-listeners.sh:/docker-init.d/gentian-edge-listeners.sh:ro" \
        -v "${work}/postfix/gentian:/etc/postfix/gentian:ro" -v "${work}/postfix/kernel-maps:/etc/postfix/kernel-maps:ro" \
        -v "${work}/postfix/tenant-keys:/etc/opendkim/tenant-keys:ro" --tmpfs /etc/opendkim/keys \
        "$(cat "${work}/postfix.image")" >/dev/null

    # The proxy as its pod runs: an ordinary user, no capability, a read-only
    # root filesystem, the chart's command and the chart's configuration.
    read -r -a args < "${work}/edge.args"
    docker create --name mlab-edge --network mlab-pods --ip "${EDGE_IN}" \
        --user 99:99 --read-only --cap-drop ALL --security-opt no-new-privileges \
        -v "${work}/edge:/etc/mail-edge:ro" \
        --entrypoint "" "$(cat "${work}/edge.image")" "${args[@]}" >/dev/null
    docker network connect --ip "${EDGE_OUT}" mlab-inet mlab-edge
    docker start mlab-edge >/dev/null

    local client="${PWD}/scripts/tests/mail_edge_lab_client.py"
    docker run -d --name mlab-out --network mlab-inet --ip 172.31.0.50 \
        -v "${client}:/client.py:ro" python:3.12-alpine sleep 3600 >/dev/null
    docker run -d --name mlab-in --network mlab-pods --ip 172.30.0.60 \
        -v "${client}:/client.py:ro" python:3.12-alpine sleep 3600 >/dev/null

    for _ in $(seq 1 90); do
        if docker exec mlab-postfix postfix status >/dev/null 2>&1; then break; fi
        sleep 1
    done
    sleep 5
    if [[ "$(docker inspect -f '{{.State.Running}}' mlab-edge)" != "true" ]]; then
        echo "the proxy did not start as an ordinary user on a read-only filesystem:"; docker logs mlab-edge 2>&1 | tail -5
        exit 1
    fi
}

out() { docker exec mlab-out python /client.py "$@" 2>&1 || true; }   # from the internet
pod() { docker exec mlab-in python /client.py "$@" 2>&1 || true; }    # from a pod
postfix_log() { docker logs mlab-postfix 2>&1; }
dovecot_log() { docker logs mlab-dovecot 2>&1; }
edge_log() { docker logs mlab-edge 2>&1; }

# check <what> <text> <pattern> — the text has a line matching the pattern.
check() {
    local what="$1" text="$2" pattern="$3"
    if grep -Eq -- "${pattern}" <<<"${text}"; then
        printf '  \033[0;32mok\033[0m    %s\n' "${what}"
    else
        printf '  \033[0;31mFAIL\033[0m  %s\n' "${what}"
        printf '        wanted: %s\n' "${pattern}"
        printf '%s\n' "${text}" | tail -6 | sed 's/^/        | /'
        fail=$((fail + 1))
    fi
}

# refuse <what> <text> <pattern> — no line of the text matches the pattern.
refuse() {
    local what="$1" text="$2" pattern="$3"
    if grep -Eq -- "${pattern}" <<<"${text}"; then
        printf '  \033[0;31mFAIL\033[0m  %s\n' "${what}"
        grep -E -- "${pattern}" <<<"${text}" | sed 's/^/        | /' | tail -4
        fail=$((fail + 1))
    else
        printf '  \033[0;32mok\033[0m    %s\n' "${what}"
    fi
}

echo ""
echo "The mail edge in front of Postfix and Dovecot, as rendered"
echo ""
render
render_upstream
lab_up

echo "(a) inbound mail on the proxy's port 25"
got="$(out smtp "${EDGE_OUT}" 2525 someone@gmail.com ada@tenant.lab.test)"
check "STARTTLS is negotiated through the proxy, with Postfix" "${got}" '^tls: TLSv1\.[23]$'
check "mail for a hosted domain is accepted" "${got}" '^accepted: someone@gmail.com -> ada@tenant.lab.test$'
got="$(out smtp "${EDGE_OUT}" 2525 someone@gmail.com victim@outlook.com)"
check "the same client is refused a relay: it is not taken for the proxy, whose address Postfix trusts" "${got}" 'Relay access denied'
sleep 3
log="$(postfix_log)"
check "Postfix logs the client's address on the edge listener" "${log}" 'postfix/edge-smtp/smtpd\[[0-9]+\]: connect from [^ ]*\[172\.31\.0\.50\]'
refuse "Postfix logs no session from the proxy's address" "${log}" 'smtpd.*(connect from|client=)[^ ]*\[172\.30\.0\.10\]'
check "the message reaches the mailbox store over LMTP" "${log}" 'postfix/lmtp.*to=<ada@tenant.lab.test>.*status=sent'

echo "(b) submission on the proxy's port 587"
got="$(out smtp "${EDGE_OUT}" 2587 ada@tenant.lab.test victim@outlook.com smtp-tenant smtp-secret)"
check "an authenticated client may relay" "${got}" '^accepted: ada@tenant.lab.test -> victim@outlook.com$'
check "it authenticated inside TLS" "${got}" '^auth: ok as smtp-tenant$'
got="$(out smtp "${EDGE_OUT}" 2587 ada@tenant.lab.test victim@outlook.com smtp-tenant wrong-password)"
check "a wrong password is refused" "${got}" 'SMTPAuthenticationError'
sleep 2
check "Postfix logs the client's address with the login" "$(postfix_log)" 'postfix/edge-submission/smtpd.*client=[^ ]*\[172\.31\.0\.50\], sasl_method=PLAIN, sasl_username=smtp-tenant'
check "and with the failed one" "$(postfix_log)" 'edge-submission/smtpd.*\[172\.31\.0\.50\]: SASL (PLAIN|LOGIN) authentication failed'

echo "(c) IMAPS on the proxy's port 993"
got="$(out imaps "${EDGE_OUT}" 2993 ada@tenant.lab.test imap-secret)"
check "an app password logs in, TLS ending at Dovecot" "${got}" '^login: OK$'
check "and finds the message delivered above" "${got}" '^inbox messages: 1$'
got="$(out imaps "${EDGE_OUT}" 2993 ada@tenant.lab.test nope)"
check "a wrong password is refused" "${got}" 'AUTHENTICATIONFAILED'
sleep 3
check "Dovecot logs the client's address with the login" "$(dovecot_log)" 'imap-login: Info: Login: user=<ada@tenant.lab.test>.*rip=172\.31\.0\.50,'
check "and counts the failed one against it" "$(dovecot_log)" 'auth failed.*rip=172\.31\.0\.50,'

echo "(d) the PROXY-protocol ports, reached without a PROXY header"
got="$(pod raw "postfix-dev-edge.${SVC}" 10025)"
refuse "Postfix's inbound edge port gives no SMTP greeting" "${got}" "b'220"
got="$(pod raw "postfix-dev-edge.${SVC}" 10587 'EHLO pod\r\n')"
refuse "Postfix's submission edge port gives none either, spoken to or not" "${got}" "b'2[25]0"
got="$(pod rawtls "dovecot-dev-edge.${SVC}" 10993)"
refuse "Dovecot's edge port completes no TLS handshake" "${got}" '^tls up'
sleep 6
check "Postfix says why" "$(postfix_log)" 'edge-(smtp|submission)/smtpd.*haproxy read: (timeout error|short protocol header)'
check "Dovecot says why" "$(dovecot_log)" 'haproxy: Client disconnected: Failed to read valid HAproxy data'

echo "(e) the listeners of the cluster's own pods, with no PROXY header"
got="$(pod smtp "postfix-dev.${SVC}" 587 ada@tenant.lab.test bob@tenant.lab.test smtp-tenant smtp-secret)"
check "a pod submits on 587 with its credential, as before" "${got}" '^accepted: ada@tenant.lab.test -> bob@tenant.lab.test$'
got="$(pod imaps "dovecot-dev.${SVC}" 993 ada@tenant.lab.test imap-secret)"
check "a pod reads mail on 993, as before" "${got}" '^login: OK$'
sleep 2
check "Postfix sees the pod's own address there" "$(postfix_log)" 'postfix/smtpd.*client=[^ ]*\[172\.30\.0\.60\], sasl_method=PLAIN'

echo "(f) the limits per client address"
sleep 45   # let the connections of (a) to (e) leave the one-minute window
got="$(out flood "${EDGE_OUT}" 2525 30)"
check "of 30 connections held at once from one address, 20 are served and 10 closed by the proxy" "${got}" '20 got a 220 banner, 0 were refused by the server with an SMTP answer, 10 were closed by the proxy'
got="$(out burst "${EDGE_OUT}" 2525 80)"
check "past 60 connections in a minute the proxy closes the rest" "${got}" ' [1-9][0-9]* were closed by the proxy without a byte'
check "before that, Postfix's own limit answers in SMTP -- counted against the client's address" "${got}" ' [1-9][0-9]* were refused by the server with an SMTP answer'
sleep 2
check "the proxy logs each refusal, with the client's address and no server" "$(edge_log)" '^172\.31\.0\.50:[0-9]+ .* smtp smtp/<NOSRV> .* PR '
check "Postfix logs its own against the client's address" "$(postfix_log)" 'Connection rate limit exceeded: [0-9]+ from [^ ]*\[172\.31\.0\.50\]'

echo "(g) behind a load balancer that names the client itself (loadBalancer.proxyProtocol)"
render --set loadBalancer.proxyProtocol.enabled=true --set "loadBalancer.proxyProtocol.from={172.31.0.50}"
python3 - "${W}" <<'PY'
import sys, yaml
w = sys.argv[1]
for doc in yaml.safe_load_all(open(w + "/render/edge.yaml")):
    if doc and doc["kind"] == "ConfigMap":
        open(w + "/work/edge/haproxy.cfg", "w").write(doc["data"]["haproxy.cfg"])
PY
docker restart mlab-edge >/dev/null
sleep 65   # a fresh proxy and a fresh minute for Postfix's counters
header='PROXY TCP4 203.0.113.9 172.31.0.10 40000 25\r\n'
got="$(out raw "${EDGE_OUT}" 2525 "${header}")"
check "the load balancer's address, with a PROXY header, is served" "${got}" "b'220 "
got="$(out raw "${EDGE_OUT}" 2525)"
refuse "the load balancer's address without one is not" "${got}" "b'220 "
sleep 3
log="$(postfix_log)"
check "Postfix logs the address the load balancer named" "${log}" 'edge-smtp/smtpd\[[0-9]+\]: connect from [^ ]*\[203\.0\.113\.9\]'
# The same header from a pod, which is not the load balancer.
pod raw "${EDGE_IN}" 2525 "${header}" >/dev/null
sleep 6
named="$(postfix_log | grep -Ec 'edge-smtp/smtpd\[[0-9]+\]: connect from [^ ]*\[203\.0\.113\.9\]' || true)"
check "a header from anybody else is not believed: no second connection is logged from the address it names" "named=${named}" '^named=1$'

echo ""
if [[ ${fail} -ne 0 ]]; then
    echo "${fail} check(s) failed"
    exit 1
fi
echo "all checks passed"
