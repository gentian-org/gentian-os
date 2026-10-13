#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-realm-smtp-settings.sh
# =============================================================================
# The kernel realm is given a mail server in every mail mode the claim allows.
#
# _keycloak_smtp_settings turns the mode into the realm's mail settings, and
# D-03 writes keycloak-smtp-credentials from them -- the Secret the operator
# registers the kernel realm's mail login from, and waits for before it writes
# a tenant realm's mail server. The mode was renamed from "kernel" to "system"
# and the case label here was not: no mode matched, the function answered
# "cannot be configured", and a cluster running its own mail had a kernel
# realm with no mail server, no Secret, and tenant realms nothing but their
# Composition wrote. The operator's Job script compared against the old name
# too, and would have handed a tenant realm the kernel realm's login.
#
# Held here:
#   - each mode of the claim's schema yields complete settings, and the
#     cluster's own mail yields the kernel realm's own login on the public
#     name with STARTTLS;
#   - a mode the schema does not know yields none;
#   - the operator's Job script names the cluster's own mail by a mode the
#     schema allows.
#
# No cluster and no network.
# =============================================================================
# What the function under test reads is set here and never read here.
# shellcheck disable=SC2034
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; fail=$((fail + 1)); }

echo ""
echo "realm mail: every mail mode of the claim gives the kernel realm a mail server"
echo ""

# The modes, from the schema and nowhere else.
modes="$(python3 - <<'PY'
import yaml
xrd = yaml.safe_load(open("crossplane/xrds/cluster.yaml"))
spec = xrd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]
print(" ".join(spec["properties"]["mail"]["properties"]["serviceMode"]["enum"]))
PY
)"
if [[ -z "${modes}" ]]; then
    bad "the Cluster schema lists no mail.serviceMode values"
    exit 1
fi

MODE=""
gentian_mail_service_mode() { printf '%s' "${MODE}"; }
_derived() { printf 'derived-%s-%s' "$1" "$2"; }
eval "$(sed -n '/^_keycloak_smtp_settings() {/,/^}/p' scripts/lib/portal-login-bootstrap.sh)"
if ! declare -F _keycloak_smtp_settings >/dev/null; then
    bad "_keycloak_smtp_settings is not in scripts/lib/portal-login-bootstrap.sh"
    exit 1
fi

KERNEL_DOMAIN=example.org
EXTERNAL_SMTP_HOST=relay.example.org SMTP_RELAY_USERNAME=relay-user SMTP_RELAY_PASSWORD=relay-secret

for MODE in ${modes}; do
    if _keycloak_smtp_settings &&
        [[ -n "${KC_SMTP_HOST}" && -n "${KC_SMTP_PORT}" && -n "${KC_SMTP_USER}" && -n "${KC_SMTP_PASSWORD}" && -n "${KC_SMTP_FROM}" ]]; then
        ok "mail.serviceMode ${MODE}: host, port, login, password and sender"
    else
        bad "mail.serviceMode ${MODE}: no settings, so the kernel realm gets no mail server and keycloak-smtp-credentials is never written"
    fi
done

MODE=system
_keycloak_smtp_settings
if [[ "${KC_SMTP_HOST}" == "mail.example.org" && "${KC_SMTP_PORT}" == "587" && "${KC_SMTP_STARTTLS}" == "true" &&
      "${KC_SMTP_SSL}" == "false" && "${KC_SMTP_USER}" == "gentian-system@example.org" &&
      "${KC_SMTP_PASSWORD}" == "derived-smtp-password" ]]; then
    ok "the cluster's own mail: the realm's own login, on the public name, over STARTTLS"
else
    bad "the cluster's own mail: got ${KC_SMTP_USER}@${KC_SMTP_HOST}:${KC_SMTP_PORT} starttls=${KC_SMTP_STARTTLS} ssl=${KC_SMTP_SSL}"
fi

MODE=external
_keycloak_smtp_settings
if [[ "${KC_SMTP_HOST}" == "relay.example.org" && "${KC_SMTP_USER}" == "relay-user" && "${KC_SMTP_PASSWORD}" == "relay-secret" ]]; then
    ok "a relay: the relay's host and login"
else
    bad "a relay: got ${KC_SMTP_USER}@${KC_SMTP_HOST}"
fi

MODE=no-such-mode
if _keycloak_smtp_settings; then
    bad "a mode the schema does not know yields settings"
else
    ok "a mode the schema does not know yields none"
fi

# The operator's Job script decides the same thing from the same word, read
# from keycloak-smtp-credentials.
script=internal/controller/keycloak_tenant_smtp.go
own="$(sed -n 's/^  \([a-z|]*\)) OWN_MAIL="true" ;;$/\1/p' "${script}")"
if [[ -z "${own}" ]]; then
    bad "${script}: no case names the cluster's own mail"
elif [[ " ${modes} " == *" ${own%%|*} "* ]]; then
    ok "the operator's Job script takes the cluster's own mail by a mode of the schema (${own%%|*})"
else
    bad "the operator's Job script takes the cluster's own mail as '${own}', and the schema's modes are: ${modes}"
fi

echo ""
if [[ "${fail}" -gt 0 ]]; then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sall %d passed%s\n' "${GREEN}" "${pass}" "${NC}"
