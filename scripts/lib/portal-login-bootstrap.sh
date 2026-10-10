#!/bin/bash
# shellcheck disable=SC2034
# Kernel realm bootstrap — the realm, its clients, the administrator and the kernel UIs' sign-in.
# Sourced from install.sh Step 14 (Stage 1 login dogfood).

set -euo pipefail

# =============================================================================
# Where each thing lives.
#
# This file used to name one namespace for the portal, Keycloak, OpenFGA, the
# wildcard certificate and the bootstrap Job alike, about twenty times over.
# Each of those is a different function, and the layout gives each its own, so
# each is asked for separately.
#
# The step that runs this exports what the layout resolved. Answering from the
# layout directly would be the same thing said twice, and a default would be a
# third place for it to be wrong.
# =============================================================================
_pl_ns() {
    local var="$1" fn="$2"
    if [[ -n "${!var:-}" ]]; then
        echo "${!var}"
        return 0
    fi
    ns_kernel "${fn}"
}
_pl_identity_ns()      { _pl_ns IDENTITY_NAMESPACE authentication; }
_pl_gitops_ns()        { _pl_ns GITOPS_NAMESPACE gitops; }
_pl_edge_ns()          { _pl_ns EDGE_NAMESPACE edge; }
_pl_control_ns()       { _pl_ns GENTIAN_SYSTEM_NAMESPACE control; }
_pl_observability_ns() { _pl_ns OBSERVABILITY_NAMESPACE observability; }

# The platform administrator has no derived password any more. The account is
# created without one, and its holder sets one through a single-use, expiring
# link -- mailed to a recovery address or shown once at the handover (see
# issue_platform_admin_activation) -- the way every member and every tenant
# administrator is let in. A password recomputable from the master password is
# one more person than the holder who can sign in as them.

# The key the Keycloak event listener signs its statements with, and the public
# half the director believes them by.
#
# Derived like every other kernel credential, so a cluster rebuilt from the
# same master password arrives at the same pair and the two halves cannot drift
# apart. An Ed25519 private key in PKCS#8 is a fixed 16-byte prefix followed by
# a 32-byte seed, and an HMAC-SHA256 is exactly 32 bytes, so the seed is the
# derivation and openssl does the rest.
ensure_keycloak_listener_keypair() {
    local key_id="${KEYCLOAK_LISTENER_KEY_ID:-gentian-listener}"
    local seed der pem pub tmp
    seed=$(printf '%s' "portal-bootstrap:listener_signing_seed" \
        | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT:-}" | awk '{print $2}')
    tmp="$(mktemp -d)"
    printf '%s' "302e020100300506032b657004220420${seed}" | xxd -r -p > "${tmp}/key.der"
    if ! openssl pkey -inform DER -in "${tmp}/key.der" -out "${tmp}/listener.pem" 2>/dev/null; then
        rm -rf "${tmp}"
        error "Could not build the listener signing key; openssl has no Ed25519 support."
        return 1
    fi
    pub=$(openssl pkey -in "${tmp}/listener.pem" -pubout -outform DER 2>/dev/null | tail -c 32 | base64 -w0)

    # The private half goes where Keycloak runs, the public half where the
    # director runs. Neither namespace ever sees the other's.
    kubectl create secret generic keycloak-event-listener-key -n "$(_pl_identity_ns)" \
        --from-file=listener.pem="${tmp}/listener.pem" \
        --dry-run=client -o yaml | kubectl apply -f - >&2
    kubectl create secret generic director-listener-keys -n "$(_pl_control_ns)" \
        --from-literal=keys="${key_id}=${pub}" \
        --dry-run=client -o yaml | kubectl apply -f - >&2
    rm -rf "${tmp}"
    success "Keycloak event listener key ${key_id} in place."
}

# _portal_random_secret <field> <namespace> <secret> <key> — a client secret
# under secretMode random: drawn once, then the same on every later call.
#
# Nothing reproduces a random secret, so it is looked for in the two places
# that hold it before a new one is drawn: the vault, under <field>, and the
# Kubernetes Secret its reader mounts. The vault alone was not enough. This
# shell has a vault token only while it is the run that seeded the vault; a
# run resumed at this step has none, every read came back empty, and each call
# drew a new secret -- so Headlamp's kubeconfig, written by a later call than
# the one that configured Keycloak, named a secret Keycloak had never seen.
_portal_random_secret() {
    local field="$1" ns="$2" name="$3" key="$4" value stored
    stored=$(bao kv get -mount=secret -field="${field}" identity/portal-admin 2>/dev/null || true)
    value="${stored}"
    if [[ -z "${value}" ]]; then
        value=$(kubectl get secret "${name}" -n "${ns}" -o "jsonpath={.data.${key}}" 2>/dev/null \
            | base64 -d 2>/dev/null || true)
    fi
    if [[ -z "${value}" ]]; then
        value=$(openssl rand -hex 24)
    fi
    if [[ -z "${stored}" ]]; then
        bao kv patch -mount=secret identity/portal-admin "${field}=${value}" >/dev/null 2>&1 || \
            bao kv put -mount=secret identity/portal-admin "${field}=${value}" >/dev/null 2>&1 || true
    fi
    echo -n "${value}"
}

_headlamp_derive_secret() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        _portal_random_secret headlamp_client_secret "$(_pl_observability_ns)" headlamp-oidc OIDC_CLIENT_SECRET
        return 0
    fi
    echo -n "portal-bootstrap:headlamp_client_secret" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT:-}" | awk '{print $2}'
}

# The kernel zone's confidential client and its secret are no longer written
# here. A zone is produced by the tenant that owns it: the platform tenant
# adopts the kernel realm (AD-10), so the kernel zone's client, its redirect
# URIs, its audience mapper and the edge-kernel-oidc Secret the SecurityPolicies
# read all come from crossplane/compositions/tenant-default.yaml, the same way
# every other tenant's zone does. What this library still does for the zone is
# create the realm the client lives in, which has to exist first.

# The placeholder the chart writes where the client secret belongs. D-03's
# check() reads it too, before this library is sourced, so it carries the same
# literal as a default there.
HEADLAMP_KUBECONFIG_PLACEHOLDER="PLACEHOLDER_REPLACED_BY_THE_INSTALLER"

# Put the derived client secret into the kubeconfig Headlamp reads.
#
# The chart renders the Secret with a placeholder, because the value is
# derived from the master password and no chart can know it. This rewrites
# that one line in place and leaves the rest of the file exactly as the chart
# wrote it, so the two cannot drift.
#
# MUST RUN AFTER THE LAST RENDER OF THE BOOTSTRAP CHART. D-03 renders that
# chart a second time, to turn Headlamp's OIDC on now that the realm exists,
# and that render carries the placeholder. Filling the secret in before it
# means the render puts the placeholder straight back: the step reports
# success, the Secret looks written, and the person is told to sign in again
# for ever. The caller checks the result rather than trusting the write.
ensure_headlamp_kubeconfig() {
    local ns secret config
    ns="$(_pl_observability_ns)"
    secret="$(_headlamp_derive_secret)"
    if [[ -z "${secret}" ]]; then
        error "no Headlamp client secret could be derived; the kubeconfig would authenticate with nothing."
        return 1
    fi
    config="$(kubectl get secret headlamp-kubeconfig -n "${ns}" -o jsonpath='{.data.config}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    if [[ -z "${config}" ]]; then
        warn "headlamp-kubeconfig is not present; Headlamp will ask for a token until the chart renders it."
        return 0
    fi
    if ! grep -q "client-secret:" <<<"${config}"; then
        warn "headlamp-kubeconfig carries no client-secret line; is the chart up to date?"
        return 0
    fi
    # Rewrite the one line, leaving the indentation the chart wrote.
    local updated line out=""
    while IFS= read -r line; do
        case "${line}" in
        *client-secret:*) out+="${line%%client-secret:*}client-secret: ${secret}"$'\n' ;;
        *) out+="${line}"$'\n' ;;
        esac
    done <<<"${config}"
    updated="${out%$'\n'}"
    if [[ "${updated}" != "${config}" ]]; then
        kubectl create secret generic headlamp-kubeconfig -n "${ns}" \
            --from-literal=config="${updated}" \
            --dry-run=client -o yaml | kubectl apply -f - >&2
        # A mounted Secret is re-read from the API server, but only on the
        # kubelet's own schedule; a restart makes the new file immediate.
        kubectl rollout restart deployment/headlamp -n "${ns}" >/dev/null 2>&1 || true
    fi
    # Read it back. A write that another apply has already overwritten is the
    # failure this whole function exists to catch, and it is invisible unless
    # someone looks.
    local stored
    stored="$(kubectl get secret headlamp-kubeconfig -n "${ns}" -o jsonpath='{.data.config}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    if grep -q "${HEADLAMP_KUBECONFIG_PLACEHOLDER}" <<<"${stored}"; then
        error "headlamp-kubeconfig still carries the placeholder after being written."
        error "  Something re-applied the chart's copy over it. Headlamp would exchange"
        error "  its code with no credential and Keycloak would answer unauthorized_client."
        return 1
    fi
    success "Headlamp exchanges its code with the client secret its kubeconfig names."
}

# The Secret Headlamp reads, in the namespace Headlamp runs in. Keys are the
# environment variable names, because the chart loads it with envFrom.
ensure_headlamp_oidc_secret() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    local ns secret
    ns="$(_pl_observability_ns)"
    secret="$(_headlamp_derive_secret)"
    # The kubeconfig Headlamp builds its token exchange from is NOT written
    # here. It names the client AND its secret, because Headlamp exchanges the
    # code with what the kubeconfig's auth-provider says and not with its own
    # -oidc-client-secret flag. But the chart renders that file, and D-03
    # renders the chart again after this point, so writing it here is writing
    # it too early. ensure_headlamp_kubeconfig does it after the last render.
    kubectl create secret generic headlamp-oidc -n "${ns}" \
        --from-literal=OIDC_CLIENT_ID="headlamp" \
        --from-literal=OIDC_CLIENT_SECRET="${secret}" \
        --from-literal=OIDC_ISSUER_URL="https://id.${kernel_domain}/auth/realms/${kernel_realm}" \
        --from-literal=OIDC_SCOPES="openid,profile,email,groups" \
        --dry-run=client -o yaml | kubectl apply -f - >&2
    echo "${secret}"
}

_argocd_oidc_derive_secret() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        _portal_random_secret argocd_client_secret "$(_pl_edge_ns)" gentian-argocd client_secret
        echo
    else
        echo -n "portal-bootstrap:argocd_client_secret" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT:-}" | awk '{print $2}'
    fi
}

_litellm_sso_derive_secret() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        _portal_random_secret litellm_sso_client_secret "$(ns_system llm)" litellm-dashboard-sso client_secret
        echo
    else
        echo -n "portal-bootstrap:litellm_sso_client_secret" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT:-}" | awk '{print $2}'
    fi
}

ensure_argocd_oidc_secret() {
    local ns
    ns="$(_pl_edge_ns)"
    local secret
    secret="$(_argocd_oidc_derive_secret)"
    # >&2 on both, for the reason spelled out in ensure_litellm_sso_secret:
    # this function's stdout is its return value, and two kubectl commands were
    # printing into it. argocd_client_secret came out 138 characters long,
    # beginning "secret/gentian-argocd configured", and that is what the
    # Keycloak client was configured with -- so ArgoCD's OIDC login was broken
    # by the same bug, in the same way, at the same time.
    kubectl create secret generic gentian-argocd -n "${ns}" \
        --from-literal=client_id="gentian-argocd" \
        --from-literal=client_secret="${secret}" \
        --dry-run=client -o yaml | kubectl apply -f - >&2
    # Also patch the actual argocd-secret in the argocd namespace
    kubectl patch secret argocd-secret -n "$(_pl_gitops_ns)" --type merge \
        -p "{\"stringData\":{\"oidc.keycloak.clientSecret\":\"${secret}\"}}" >&2
    echo "${secret}"
}

ensure_litellm_sso_secret() {
    local ns
    # Where its one reader runs: the proxy in the llm system namespace mounts
    # it by name. v5 wrote it to the edge namespace, so on a fresh install the
    # proxy sat in CreateContainerConfigError on "secret not found".
    ns="$(ns_system llm)"
    local secret
    secret="$(_litellm_sso_derive_secret)"
    # No llm namespace means the cluster serves no models (llm.enabled=false):
    # nothing to mount it. The Keycloak client still gets the derived secret.
    if ! kubectl get namespace "${ns}" >/dev/null 2>&1; then
        echo "${secret}"
        return 0
    fi
    # >&2 on the apply, and this is not cosmetic. This function's stdout IS its
    # return value -- callers do secret="$(ensure_litellm_sso_secret)" -- so
    # kubectl's own "secret/litellm-dashboard-sso configured" line was captured
    # as part of the secret. The caller then handed Keycloak a 104-character
    # string beginning "secret/litellm-dashboard-sso configured\n" as the
    # client secret, while the Secret this function writes held the clean 64.
    # Every SSO login then failed the token exchange with
    # "(unauthorized_client) Invalid client or Invalid client credentials",
    # surfacing to the browser as an opaque 500.
    kubectl create secret generic litellm-dashboard-sso -n "${ns}" \
        --from-literal=client_id="litellm-dashboard" \
        --from-literal=client_secret="${secret}" \
        --dry-run=client -o yaml | kubectl apply -f - >&2
    echo "${secret}"
}

_keycloak_internal_service_url() {
    local ns
    ns="${1:-$(_pl_identity_ns)}"
    local release="${GENTIAN_IDP_KEYCLOAK_RELEASE:-gentian-idp-keycloak}"
    local svc port
    # keycloakx Helm chart publishes {release}-keycloakx-http (Suze default release name).
    for svc in "${release}-keycloakx-http" \
        $(kubectl get svc -n "${ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
            | grep -E 'keycloak.*http' | head -1) \
        $(kubectl get svc -n "${ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
            | grep keycloak | grep -v headless | head -1); do
        [[ -n "${svc}" ]] || continue
        if ! kubectl get svc -n "${ns}" "${svc}" >/dev/null 2>&1; then
            continue
        fi
        port=$(kubectl get svc -n "${ns}" "${svc}" -o jsonpath='{.spec.ports[?(@.port==8080)].port}' 2>/dev/null || true)
        port="${port:-8080}"
        echo "http://${svc}.${ns}.svc.cluster.local:${port}/auth"
        return 0
    done
    return 1
}

wait_for_keycloak_http_service() {
    local ns
    ns="${1:-$(_pl_identity_ns)}"
    local timeout_sec="${2:-300}"
    local deadline=$((SECONDS + timeout_sec))
    while (( SECONDS < deadline )); do
        if _keycloak_internal_service_url "${ns}" >/dev/null 2>&1; then
            _keycloak_internal_service_url "${ns}"
            return 0
        fi
        sleep 5
    done
    return 1
}

ensure_keycloak_admin_secret_url() {
    local ns
    ns="$(_pl_identity_ns)"
    local url current

    # The first wait is the long one, because on a fresh install this is a race
    # with Argo CD rather than a fault.
    #
    # Keycloak arrives through gentian-appsets -> gentian-suze -> keycloak-idp,
    # and that chain took eleven minutes on a real cluster. The old budget was
    # 120s here plus 180s after a heal — under half of it — so the step reported
    # "No Keycloak HTTP service found" and stopped an install whose Keycloak
    # appeared a couple of minutes later. Nothing was wrong; it was early.
    #
    # The heal is still worth attempting, but only once the wait has genuinely
    # expired: ensure_suze_idp_workloads cannot help while the Suze claim itself
    # has not been applied yet, which is exactly the state a too-short wait
    # catches.
    local kc_wait="${GENTIAN_KEYCLOAK_WAIT_SECS:-900}"
    info "Waiting up to $(( kc_wait / 60 ))m for Keycloak to appear (Argo CD is still rolling out on a fresh install)..."
    if ! url=$(wait_for_keycloak_http_service "${ns}" "${kc_wait}"); then
        if declare -F ensure_suze_idp_workloads >/dev/null 2>&1; then
            warn "Keycloak Service still absent after $(( kc_wait / 60 ))m — attempting Suze IdP heal..."
            ensure_suze_idp_workloads "" 600 || true
            url=$(wait_for_keycloak_http_service "${ns}" 300) || true
        fi
    fi

    if [[ -z "${url:-}" ]]; then
        error "No Keycloak HTTP service found in ${ns} (expected ${GENTIAN_IDP_KEYCLOAK_RELEASE:-gentian-idp-keycloak}-keycloakx-http)."
        return 1
    fi
    # The Service existing is not Keycloak answering. On a fresh cluster it
    # starts before CloudNativePG has created its database, exits with
    # 'FATAL: database "keycloak" does not exist' and restarts, so a bootstrap
    # Job created in that window spends its whole retry budget failing and only
    # the Job's own retry succeeds -- ten minutes, and a failed pod that reads
    # like a broken install.
    #
    # Non-fatal on timeout: the Job retries either way, so this removes a
    # predictable delay rather than becoming a new way to fail.
    if ! kubectl wait --for=condition=Ready pod \
            -l "app.kubernetes.io/name=keycloakx" -n "${ns}" \
            --timeout="${GENTIAN_KEYCLOAK_READY_WAIT_SECS:-600}s" >/dev/null 2>&1; then
        warn "Keycloak is not Ready yet; the bootstrap Job will retry until it is."
    fi

    current=$(kubectl get secret keycloak-admin -n "${ns}" -o jsonpath='{.data.url}' 2>/dev/null | base64 -d || true)
    if [[ "${current}" == "${url}" ]]; then
        info "keycloak-admin URL: ${url}"
        return 0
    fi
    warn "Updating keycloak-admin URL (${current:-missing} -> ${url}) via ExternalSecret"

    # Force a re-sync, and do not apply anything.
    #
    # The ExternalSecret is the Keycloak chart's own
    # (kernel/services/keycloak-idp/manifests/templates/externalsecret-admin.yaml)
    # and Argo CD keeps it as git has it, with selfHeal. The URL in it is
    # rendered from the chart's values, so applying it again from here would
    # write back the value already there: the annotate below is the whole
    # remedy.
    kubectl annotate externalsecret keycloak-admin -n "${ns}" \
        "force-sync=$(date +%s)" --overwrite >/dev/null
    local deadline=$((SECONDS + 30))
    while (( SECONDS < deadline )); do
        current=$(kubectl get secret keycloak-admin -n "${ns}" -o jsonpath='{.data.url}' 2>/dev/null | base64 -d || true)
        if [[ "${current}" == "${url}" ]]; then
            success "keycloak-admin Secret URL corrected."
            return 0
        fi
        sleep 2
    done

    # A mismatch that survives a re-sync is a mismatch with git, not a transient
    # one: the ExternalSecret templates the URL as a literal, so the fix is a
    # commit to that manifest, not another retry here.
    error "keycloak-admin URL is ${current:-missing}; this cluster's Keycloak serves ${url}"
    error "  The URL comes from keycloakRelease in kernel/services/keycloak-idp/manifests/values.yaml."
    error "  Correct it there and let Argo CD sync, or set GENTIAN_IDP_KEYCLOAK_RELEASE to match."
    return 1
}

ensure_edge_gateway_readiness() {
    local ns
    ns="$(_pl_edge_ns)"
    if ! kubectl get secret wildcard-tls -n "${ns}" >/dev/null 2>&1; then
        if kubectl get secret wildcard-kernel-tls -n "$(gentian_cert_manager_namespace)" >/dev/null 2>&1; then
            info "Copying wildcard-kernel-tls → ${ns}/wildcard-tls for the edge Gateways..."
            kubectl get secret wildcard-kernel-tls -n "$(gentian_cert_manager_namespace)" -o json | python3 -c "
import sys, json
s = json.load(sys.stdin)
s['metadata'] = {'name': 'wildcard-tls', 'namespace': '${ns}'}
for k in ('resourceVersion','uid','creationTimestamp','managedFields','ownerReferences'):
    s['metadata'].pop(k, None)
s.pop('status', None)
print(json.dumps(s))
" | kubectl apply -f -
        else
            warn "wildcard-tls missing in ${ns} — gateway HTTPS listeners may stay invalid."
        fi
    fi
}

# Resolve Keycloak realm SMTP settings from MAIL_SERVICE_MODE.
# Sets: KC_SMTP_HOST, KC_SMTP_PORT, KC_SMTP_USER, KC_SMTP_PASSWORD,
#       KC_SMTP_SSL, KC_SMTP_STARTTLS, KC_SMTP_FROM
# Returns 0 when settings are complete, 1 when SMTP cannot be configured.
_keycloak_smtp_settings() {
    local mode
    mode="$(gentian_mail_service_mode)"
    local env="${ENV:-dev}"
    local kernel_domain="${KERNEL_DOMAIN:-}"

    KC_SMTP_HOST=""
    KC_SMTP_PORT=""
    KC_SMTP_USER=""
    KC_SMTP_PASSWORD=""
    KC_SMTP_SSL="${EXTERNAL_SMTP_SSL:-false}"
    KC_SMTP_STARTTLS="${EXTERNAL_SMTP_STARTTLS:-true}"
    KC_SMTP_FROM=""

    [[ -n "${kernel_domain}" ]] || return 1

    case "${mode}" in
        external)
            if [[ -z "${EXTERNAL_SMTP_HOST:-}" || -z "${SMTP_RELAY_USERNAME:-}" \
                || -z "${SMTP_RELAY_PASSWORD:-}" ]]; then
                return 1
            fi
            KC_SMTP_HOST="${EXTERNAL_SMTP_HOST}"
            KC_SMTP_PORT="${EXTERNAL_SMTP_PORT:-587}"
            KC_SMTP_USER="${SMTP_RELAY_USERNAME}"
            KC_SMTP_PASSWORD="${SMTP_RELAY_PASSWORD}"
            KC_SMTP_FROM="noreply@${kernel_domain}"
            ;;
        kernel)
            if ! declare -F _derived >/dev/null 2>&1; then
                return 1
            fi
            # The PUBLIC name, with STARTTLS. Postfix offers AUTH only after
            # STARTTLS (smtpd_tls_auth_only), so a realm that does not upgrade
            # cannot present the credential below at all and relays only while
            # the cluster still trusts its address. Java then checks the
            # certificate against the name it dialled, and the certificate is the
            # public wildcard for the kernel domain, which covers mail.<domain> and
            # not postfix-<env>.<ns>.svc.cluster.local — the name this used before,
            # together with STARTTLS off, when Postfix presented a self-signed
            # certificate no client trusted.
            KC_SMTP_HOST="mail.${kernel_domain}"
            KC_SMTP_PORT="587"
            KC_SMTP_USER="gentian-system@${kernel_domain}"
            # Derived under secretMode random too: this is asked for twice
            # in one run, by the realm bootstrap and by the Secret the mail
            # server registers the login from, and both must hold one value.
            KC_SMTP_PASSWORD="$(_derived smtp password)"
            KC_SMTP_SSL="false"
            KC_SMTP_STARTTLS="true"
            KC_SMTP_FROM="noreply@${kernel_domain}"
            ;;
        *)
            return 1
            ;;
    esac
    return 0
}

# In-cluster shell fragment: configure realm smtpServer via Keycloak Admin API.
_keycloak_smtp_configure_shell() {
    cat <<'EOSMTP'
              if [ "${SMTP_CONFIGURE}" = "true" ]; then
                # The status first, so a failure says which one it was.
                #
                # This was a bare `curl -sf`, and when the realm did not exist it
                # exited 22 with -s swallowing the message: the pod died under
                # set -e having printed nothing, three times over, and the only
                # evidence was an exit code. A 404 means the realm is missing, a
                # 401 means the admin credentials are wrong, and those want
                # different fixes.
                REALM_CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "${AUTH}" \
                  "${KEYCLOAK_BASE}/admin/realms/${REALM}")
                if [ "${REALM_CODE}" != "200" ]; then
                  echo "ERROR: cannot read realm ${REALM} (HTTP ${REALM_CODE}) at ${KEYCLOAK_BASE}" >&2
                  if [ "${REALM_CODE}" = "404" ]; then
                    echo "  The realm does not exist yet — the portal bootstrap creates it," >&2
                    echo "  and in D-06 that now runs before this." >&2
                  fi
                  exit 1
                fi
                REALM_JSON=$(curl -sf -H "${AUTH}" "${KEYCLOAK_BASE}/admin/realms/${REALM}")
                SMTP_JSON=$(jq -n \
                  --arg host "${SMTP_HOST}" \
                  --arg port "${SMTP_PORT}" \
                  --arg from "${SMTP_FROM}" \
                  --arg user "${SMTP_USER}" \
                  --arg pass "${SMTP_PASSWORD}" \
                  --arg ssl "${SMTP_SSL}" \
                  --arg starttls "${SMTP_STARTTLS}" \
                  --arg fromName "${SMTP_FROM_NAME:-Gentian}" \
                  '{
                    host: $host,
                    port: $port,
                    from: $from,
                    fromDisplayName: $fromName,
                    auth: "true",
                    user: $user,
                    password: $pass,
                    ssl: $ssl,
                    starttls: $starttls
                  }')
                UPDATED=$(printf '%s' "${REALM_JSON}" | jq --argjson smtp "${SMTP_JSON}" '.smtpServer = $smtp')
                curl -sf -X PUT -H "${AUTH}" -H "Content-Type: application/json" \
                  "${KEYCLOAK_BASE}/admin/realms/${REALM}" -d "${UPDATED}"
                echo "Configured realm SMTP (MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE}) → ${SMTP_HOST}:${SMTP_PORT}"
              else
                echo "Skipping Keycloak SMTP configuration (credentials not available)"
              fi
EOSMTP
}

# In-cluster shell fragment: re-authenticate and replace ${TOKEN}/${AUTH}.
#
# Keycloak's master realm (where the admin-cli password grant used
# throughout this script authenticates against) has a default
# accessTokenLifespan of 60 seconds. Scripts that chain many sequential
# realm/client/group API calls (e.g. run_keycloak_portal_bootstrap_job)
# can exceed that before reaching their later, optional sections — and
# since every call here uses `curl -sf`, an expired-token 401 fails
# completely silently (curl just exits 22, no response body printed), so
# the job dies with no visible reason right after whatever was last
# echoed. Splice this in before any section that runs after ~40-50s of
# prior API calls to get a fresh token instead of gambling on the old
# one still being valid.
_keycloak_refresh_token_shell() {
    cat <<'EOREFRESH'
              TOKEN=$(curl -sf -X POST "${KEYCLOAK_BASE}/realms/master/protocol/openid-connect/token" \
                -H "Content-Type: application/x-www-form-urlencoded" \
                --data-urlencode "client_id=admin-cli" \
                --data-urlencode "username=${KEYCLOAK_ADMIN_USERNAME}" \
                --data-urlencode "password=${KEYCLOAK_ADMIN_PASSWORD}" \
                --data-urlencode "grant_type=password" | jq -r .access_token)
              if [ -z "${TOKEN}" ] || [ "${TOKEN}" = "null" ]; then
                printf '\033[0;31m[ERROR]\033[0m %s\n' "Keycloak admin token refresh failed at ${KEYCLOAK_BASE}" >&2
                exit 1
              fi
              AUTH="Authorization: Bearer ${TOKEN}"
EOREFRESH
}

# Apply keycloak-smtp-credentials Secret used by bootstrap / SMTP-only Jobs.
#
# Only where nothing else owns it. On external-mail clusters the Secret is built
# by an ExternalSecret (kernel/services/keycloak-idp), which sources the relay
# credential from OpenBao — so the credential reaches Keycloak whenever it is
# supplied, including long after this script could have run. Writing it here as
# well would be two writers on one object, each reverting the other on its own
# schedule: ESO owns the Secret and restores it on every refresh, and this
# function would win only until then.
#
# Deferring is not a downgrade. The reason to write it from here was that
# nothing else could, and that is exactly what the ExternalSecret changed.
# kernel mode keeps this path: its submission password is derived, not stored,
# so there is no OpenBao path for ESO to extract.
_apply_keycloak_smtp_secret() {
    local ns
    ns="${1:-$(_pl_identity_ns)}"
    local mail_mode
    mail_mode="$(gentian_mail_service_mode)"
    if kubectl get externalsecret keycloak-smtp-credentials -n "${ns}" >/dev/null 2>&1; then
        info "keycloak-smtp-credentials is owned by an ExternalSecret; leaving it alone."
        info "  The relay credential reaches Keycloak from OpenBao. If realm SMTP is"
        info "  unconfigured, supply smtp-relay — Admin Console → Credentials."
        # Ready only if ESO has actually resolved the credential, so the caller
        # does not run a configure Job that would exit on its own gate.
        [[ "$(kubectl get secret keycloak-smtp-credentials -n "${ns}" \
            -o jsonpath='{.data.smtp_configure}' 2>/dev/null | base64 -d 2>/dev/null)" == "true" ]]
        return $?
    fi

    if ! _keycloak_smtp_settings; then
        kubectl delete secret keycloak-smtp-credentials -n "${ns}" --ignore-not-found=true \
            >/dev/null 2>&1 || true
        return 1
    fi

    kubectl create secret generic keycloak-smtp-credentials -n "${ns}" \
        --from-literal=mail_service_mode="${mail_mode}" \
        --from-literal=smtp_configure="true" \
        --from-literal=smtp_host="${KC_SMTP_HOST}" \
        --from-literal=smtp_port="${KC_SMTP_PORT}" \
        --from-literal=smtp_user="${KC_SMTP_USER}" \
        --from-literal=smtp_password="${KC_SMTP_PASSWORD}" \
        --from-literal=smtp_ssl="${KC_SMTP_SSL}" \
        --from-literal=smtp_starttls="${KC_SMTP_STARTTLS}" \
        --from-literal=smtp_from="${KC_SMTP_FROM}" \
        --from-literal=kernel_realm="${KERNEL_REALM:-kernel}" \
        --dry-run=client -o yaml | kubectl apply -f -
    return 0
}

# Configure Keycloak kernel realm SMTP (standalone Job; D-03 runs it after the realm bootstrap).
configure_keycloak_realm_smtp() {
    local ns
    ns="$(_pl_identity_ns)"
    local job_name="keycloak-smtp-configure"
    local kernel_realm="${KERNEL_REALM:-kernel}"

    # Keycloak first, and before the Secret check, because this runs earlier in
    # the step than the portal-login wait and hits the same race.
    #
    # Without it the Job is created against a Keycloak that does not exist, its
    # pods fail with
    #
    #   could not resolve Keycloak OIDC base from KEYCLOAK_URL=...
    #
    # three times over, and `kubectl wait` times out on a condition that was
    # never going to arrive. That is several minutes spent proving something the
    # caller could have established in one query.
    if ! wait_for_keycloak_http_service "${ns}" "${GENTIAN_KEYCLOAK_WAIT_SECS:-900}" >/dev/null; then
        warn "Keycloak is not serving in ${ns} yet — realm SMTP not configured."
        warn "  Nothing is wrong with the SMTP settings; there is no realm to put"
        warn "  them in. Re-run once Keycloak is up: ./install.sh --only D-06"
        return 1
    fi

    if ! kubectl get secret keycloak-admin -n "${ns}" >/dev/null 2>&1; then
        warn "keycloak-admin Secret not present yet — cannot configure realm SMTP."
        return 1
    fi

    if ! _apply_keycloak_smtp_secret "${ns}"; then
        warn "SMTP credentials incomplete — set EXTERNAL_SMTP_* + SMTP_RELAY_*" \
             "(external) or MAIL_SERVICE_MODE=system with derived smtp password."
        return 1
    fi

    info "Configuring Keycloak realm SMTP (MAIL_SERVICE_MODE=$(gentian_mail_service_mode))..."

    kubectl delete job "${job_name}" -n "${ns}" --ignore-not-found=true 2>/dev/null || true

    local smtp_shell
    smtp_shell=$(_keycloak_smtp_configure_shell)

    kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${ns}
  labels:
    app.kubernetes.io/name: keycloak-smtp-configure
spec:
  ttlSecondsAfterFinished: 3600
  backoffLimit: 2
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: configure
          image: alpine:3.20
          command:
            - /bin/sh
            - -ec
            - |
              apk add --no-cache --quiet curl jq >/dev/null
              set -eu
              resolve_keycloak_base() {
                local base code
                for base in "\${KEYCLOAK_URL}" "\${KEYCLOAK_URL%/}/auth" "\${KEYCLOAK_URL%/auth}"; do
                  code=\$(curl -s -o /dev/null -w '%{http_code}' "\${base}/realms/master/.well-known/openid-configuration")
                  if [ "\${code}" = "200" ]; then
                    echo "\${base}"
                    return 0
                  fi
                done
                return 1
              }
              # Poll, don't probe once. On a fresh install Keycloak is
              # legitimately still booting when this Job starts: it restarts
              # a few times while the shared database's bootstrap is still
              # creating its role, then runs first-run schema migrations —
              # ~10 minutes on a real cluster. A single probe here burned
              # the Job's whole backoffLimit inside ~90s against a server
              # that was minutes from healthy, and took the install down.
              KEYCLOAK_BASE=""
              kc_tries=0
              while [ "\${kc_tries}" -lt 60 ]; do
                if KEYCLOAK_BASE=\$(resolve_keycloak_base); then
                  break
                fi
                kc_tries=\$((kc_tries + 1))
                echo "Keycloak not answering OIDC discovery yet (attempt \${kc_tries}/60); retrying in 10s..."
                sleep 10
              done
              [ -n "\${KEYCLOAK_BASE}" ] || {
                printf '\033[0;31m[ERROR]\033[0m %s\n' "could not resolve Keycloak OIDC base from KEYCLOAK_URL=\${KEYCLOAK_URL} after 10 minutes" >&2
                exit 1
              }
              TOKEN=\$(curl -sf -X POST "\${KEYCLOAK_BASE}/realms/master/protocol/openid-connect/token" \\
                -H "Content-Type: application/x-www-form-urlencoded" \\
                --data-urlencode "client_id=admin-cli" \\
                --data-urlencode "username=\${KEYCLOAK_ADMIN_USERNAME}" \\
                --data-urlencode "password=\${KEYCLOAK_ADMIN_PASSWORD}" \\
                --data-urlencode "grant_type=password" | jq -r .access_token)
              AUTH="Authorization: Bearer \${TOKEN}"
              REALM="\${KERNEL_REALM}"
              SMTP_CONFIGURE="\${SMTP_CONFIGURE}"
              MAIL_SERVICE_MODE="\${MAIL_SERVICE_MODE}"
              SMTP_HOST="\${SMTP_HOST}"
              SMTP_PORT="\${SMTP_PORT}"
              SMTP_USER="\${SMTP_USER}"
              SMTP_PASSWORD="\${SMTP_PASSWORD}"
              SMTP_SSL="\${SMTP_SSL}"
              SMTP_STARTTLS="\${SMTP_STARTTLS}"
              SMTP_FROM="\${SMTP_FROM}"
${smtp_shell}
          env:
            - name: KEYCLOAK_URL
              valueFrom:
                secretKeyRef:
                  name: keycloak-admin
                  key: url
            - name: KEYCLOAK_ADMIN_USERNAME
              valueFrom:
                secretKeyRef:
                  name: keycloak-admin
                  key: username
            - name: KEYCLOAK_ADMIN_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: keycloak-admin
                  key: password
            - name: KERNEL_REALM
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: kernel_realm
            - name: MAIL_SERVICE_MODE
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: mail_service_mode
            - name: SMTP_CONFIGURE
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: smtp_configure
            - name: SMTP_HOST
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: smtp_host
            - name: SMTP_PORT
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: smtp_port
            - name: SMTP_USER
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: smtp_user
            - name: SMTP_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: smtp_password
            - name: SMTP_SSL
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: smtp_ssl
            - name: SMTP_STARTTLS
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: smtp_starttls
            - name: SMTP_FROM
              valueFrom:
                secretKeyRef:
                  name: keycloak-smtp-credentials
                  key: smtp_from
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 200m
              memory: 128Mi
EOF

    # 720s, not 120s: the Job now polls Keycloak for up to 10 minutes
    # before touching anything (see the loop in its script), so the wait
    # here has to outlast that budget or it reports failure over a Job
    # that is still legitimately waiting.
    if ! kubectl wait "job/${job_name}" -n "${ns}" --for=condition=complete --timeout=720s; then
        error "Keycloak SMTP configure Job failed."
        gentian_job_logs "${ns}" "${job_name}" Failed 40
        return 1
    fi

    gentian_job_logs "${ns}" "${job_name}" Succeeded 5
    success "Keycloak realm ${kernel_realm} SMTP configured ($(gentian_mail_service_mode))."
}

# Tenant realm SMTP is the operator's. The shell function that used to live
# here wrote the same Job — keycloak-tenant-smtp-<tenant> — as the
# TenantReconciler, deleting whatever was there first, and it stamped no
# version label. The operator reads that label to decide whether a Job is
# outdated, so it saw "" != the current version, deleted the shell's Job and
# recreated its own; the next E-02 run deleted that one. Two writers, one
# object, each undoing the other on its own schedule.
#
# The reconciler is also the one that can do the whole job: it orders the SMTP
# Job after the realm Job, gates on the cluster SMTP credentials existing, and
# migrates realms already configured when the script changes. A step that runs
# only when an operator invokes it cannot do the last of those.
#
# The kernel realm is a different case and stays here: configure_keycloak_realm_smtp
# above configures the kernel realm, which has no Tenant CR to reconcile from.

# gentian_job_logs <ns> <job> <outcome> [tail]
#
# The logs of the pod that produced <outcome>, not whichever pod kubectl picks.
#
# `kubectl logs job/<name>` selects one pod from the job's label selector, and a
# Job with a retry has more than one. On a fresh cluster Keycloak starts before
# CloudNativePG has created its database, crash-loops, and the first bootstrap
# pod exhausts its retries before a second runs and succeeds in seconds. The Job
# then reports succeeded=1 failed=1, and the tail printed after the success was
# the FAILED pod's — sixty retry lines and an ERROR immediately before the OK,
# about work that had already completed.
#
# outcome is Succeeded or Failed. Falls back to the job selector when no pod
# matches, so a Job whose pods were garbage-collected still prints something.
gentian_job_logs() {
    local ns="$1" job="$2" outcome="$3" tail="${4:-20}" pod
    pod="$(kubectl get pods -n "${ns}" -l "job-name=${job}" \
        --field-selector="status.phase=${outcome}" \
        -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)"
    if [[ -n "${pod}" ]]; then
        kubectl logs -n "${ns}" "${pod}" --tail="${tail}" 2>/dev/null || true
        return 0
    fi
    kubectl logs -n "${ns}" "job/${job}" --tail="${tail}" 2>/dev/null || true
}

# Keycloak Admin API calls run in-cluster (Job). The keycloak-admin Secret URL is
# an in-cluster Service DNS name and is not reachable from the install host.
run_keycloak_portal_bootstrap_job() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    # A username is an address, like every tenant administrator's.
    #
    # The kernel realm's email claim is mapped to the USERNAME so that the
    # email field can hold the recovery address Keycloak mails a reset to.
    # That only works where the username is itself an address; the bare name
    # "administrator" made every token say email=administrator. Tenants are
    # named admin@<their domain> and the platform administrator follows the
    # same pattern on the kernel domain.
    local username="admin@${kernel_domain}"
    local email="admin@${kernel_domain}"
    # The account this replaces, removed below once the new one exists.
    local legacy_username="administrator"
    local job_name="keycloak-portal-bootstrap"
    # Where Keycloak's admin credential is, which is where this Job has to run:
    # a Secret is readable only in its own namespace, and copying an admin
    # password into another one to save a namespace is not a trade worth making.
    local ns
    ns="$(_pl_identity_ns)"
    # The group model v1 names for the platform administrator role; the
    # claim binds it to cluster#admin and every console derives from that.
    local platform_admin_group="gentian:platform:admin"

    export PORTAL_LOGIN_USERNAME="${username}"
    local require_mfa
    require_mfa="$(platform_admin_requires_mfa)"

    info "Bootstrapping the kernel realm, its clients and the administrator via in-cluster Job..."

    local argocd_secret headlamp_secret
    argocd_secret=$(ensure_argocd_oidc_secret)
    headlamp_secret=$(ensure_headlamp_oidc_secret)

    local llm_support="${LLM_SUPPORT:-false}"
    local litellm_sso_secret=""
    if [[ "${llm_support}" == "true" ]]; then
        litellm_sso_secret=$(ensure_litellm_sso_secret)
    fi

    # These two are about to be handed to Keycloak as OIDC client secrets, and
    # they came back over stdout from functions that also run kubectl. A single
    # unredirected kubectl line silently turns a secret into a paragraph, the
    # client is configured with the paragraph, and the only symptom is every SSO
    # login failing the token exchange with "Invalid client credentials" and a
    # 500 in the browser. It took a packet capture's worth of digging to find
    # once; it should announce itself from here on.
    #
    # A derived secret is one whitespace-free token. Anything else is a bug in
    # the producer, not a credential.
    local _name _val
    for _name in argocd_secret litellm_sso_secret; do
        _val="${!_name}"
        [[ -n "${_val}" ]] || continue
        if [[ "${_val}" != "${_val//[[:space:]]/}" ]]; then
            error "${_name} contains whitespace — it captured command output, not just a secret."
            error "  first line: ${_val%%$'\n'*}"
            error "  Whichever ensure_* function produced it is printing to stdout;"
            error "  its kubectl calls need >&2. Refusing to configure Keycloak with this."
            return 1
        fi
    done

    local -a bootstrap_secret_args=(
        --from-literal=kernel_domain="${kernel_domain}"
        --from-literal=kernel_realm="${kernel_realm}"
        --from-literal=username="${username}"
        --from-literal=email="${email}"
        --from-literal=require_mfa="${require_mfa}"
        --from-literal=platform_admin_group="${platform_admin_group}"
        --from-literal=legacy_username="${legacy_username}"
        --from-literal=argocd_client_secret="${argocd_secret}"
        --from-literal=headlamp_client_secret="${headlamp_secret}"
        --from-literal=llm_support="${llm_support}"
        --from-literal=litellm_sso_client_secret="${litellm_sso_secret}"
    )
    if _keycloak_smtp_settings; then
        bootstrap_secret_args+=(
            --from-literal=smtp_configure=true
            --from-literal=mail_service_mode="$(gentian_mail_service_mode)"
            --from-literal=smtp_host="${KC_SMTP_HOST}"
            --from-literal=smtp_port="${KC_SMTP_PORT}"
            --from-literal=smtp_user="${KC_SMTP_USER}"
            --from-literal=smtp_password="${KC_SMTP_PASSWORD}"
            --from-literal=smtp_ssl="${KC_SMTP_SSL}"
            --from-literal=smtp_starttls="${KC_SMTP_STARTTLS}"
            --from-literal=smtp_from="${KC_SMTP_FROM}"
        )
    else
        # Not a warning: this only says the installer holds no SMTP settings
        # of its own, which is the normal case. The relay credential lives in
        # OpenBao and reaches Keycloak through keycloak-smtp-credentials, and
        # configure_keycloak_realm_smtp -- the next thing D-03 does -- reads
        # that and configures the realm. Warning here printed "will not send"
        # on a cluster whose realm the same run then configured, and that
        # later step is the one that warns when SMTP really is incomplete.
        info "Realm SMTP is configured after the bootstrap, from keycloak-smtp-credentials."
        bootstrap_secret_args+=(
            --from-literal=smtp_configure=false
            --from-literal=mail_service_mode="$(gentian_mail_service_mode)"
            --from-literal=smtp_host=""
            --from-literal=smtp_port=""
            --from-literal=smtp_user=""
            --from-literal=smtp_password=""
            --from-literal=smtp_ssl="false"
            --from-literal=smtp_starttls="true"
            --from-literal=smtp_from=""
        )
    fi

    kubectl create secret generic portal-bootstrap-credentials -n "${ns}" \
        "${bootstrap_secret_args[@]}" \
        --dry-run=client -o yaml | kubectl apply -f -

    local smtp_shell refresh_shell
    smtp_shell=$(_keycloak_smtp_configure_shell)
    refresh_shell=$(_keycloak_refresh_token_shell)

    kubectl delete job "${job_name}" -n "${ns}" --ignore-not-found=true 2>/dev/null || true

    kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${ns}
  labels:
    app.kubernetes.io/name: keycloak-portal-bootstrap
spec:
  ttlSecondsAfterFinished: 3600
  backoffLimit: 2
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: bootstrap
          image: alpine:3.20
          command:
            - /bin/sh
            - -ec
            - |
              apk add --no-cache --quiet curl jq >/dev/null
              set -eu
              resolve_keycloak_base() {
                local base code
                for base in "\${KEYCLOAK_URL}" "\${KEYCLOAK_URL%/}/auth" "\${KEYCLOAK_URL%/auth}"; do
                  code=\$(curl -s -o /dev/null -w '%{http_code}' "\${base}/realms/master/.well-known/openid-configuration")
                  if [ "\${code}" = "200" ]; then
                    echo "\${base}"
                    return 0
                  fi
                done
                return 1
              }
              # Poll, don't probe once. On a fresh install Keycloak is
              # legitimately still booting when this Job starts: it restarts
              # a few times while the shared database's bootstrap is still
              # creating its role, then runs first-run schema migrations —
              # ~10 minutes on a real cluster. A single probe here burned
              # the Job's whole backoffLimit inside ~90s against a server
              # that was minutes from healthy, and took the install down.
              KEYCLOAK_BASE=""
              kc_tries=0
              while [ "\${kc_tries}" -lt 60 ]; do
                if KEYCLOAK_BASE=\$(resolve_keycloak_base); then
                  break
                fi
                kc_tries=\$((kc_tries + 1))
                echo "Keycloak not answering OIDC discovery yet (attempt \${kc_tries}/60); retrying in 10s..."
                sleep 10
              done
              [ -n "\${KEYCLOAK_BASE}" ] || {
                printf '\033[0;31m[ERROR]\033[0m %s\n' "could not resolve Keycloak OIDC base from KEYCLOAK_URL=\${KEYCLOAK_URL} after 10 minutes" >&2
                exit 1
              }
              echo "Using Keycloak base \${KEYCLOAK_BASE}"
              TOKEN=\$(curl -sf -X POST "\${KEYCLOAK_BASE}/realms/master/protocol/openid-connect/token" \\
                -H "Content-Type: application/x-www-form-urlencoded" \\
                --data-urlencode "client_id=admin-cli" \\
                --data-urlencode "username=\${KEYCLOAK_ADMIN_USERNAME}" \\
                --data-urlencode "password=\${KEYCLOAK_ADMIN_PASSWORD}" \\
                --data-urlencode "grant_type=password" | jq -r .access_token)
              if [ -z "\${TOKEN}" ] || [ "\${TOKEN}" = "null" ]; then
                printf '\033[0;31m[ERROR]\033[0m %s\n' "Keycloak admin token request failed at \${KEYCLOAK_BASE}" >&2
                exit 1
              fi
              AUTH="Authorization: Bearer \${TOKEN}"
              REALM="\${KERNEL_REALM}"

              realm_http=\$(curl -s -o /dev/null -w '%{http_code}' -H "\${AUTH}" "\${KEYCLOAK_BASE}/admin/realms/\${REALM}")
              if [ "\${realm_http}" = "404" ]; then
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms" \\
                  -d "{\"realm\":\"\${REALM}\",\"enabled\":true,\"displayName\":\"\${REALM}\"}"
                echo "Created realm \${REALM}"
              elif [ "\${realm_http}" = "200" ]; then
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}" -d '{"enabled":true}'
                echo "Realm \${REALM} enabled"
              else
                printf '\033[0;31m[ERROR]\033[0m %s\n' "realm \${REALM} check returned HTTP \${realm_http}" >&2
                exit 1
              fi

              # Every surface Keycloak draws wears the product's theme.
              #
              # The login screen already did. The account and administration
              # consoles did not, and the administration console is embedded
              # in the desktop as the Identity tile, so its appearance is the
              # product's appearance. A theme restyles what Keycloak draws and
              # does not redraw it: both consoles are compiled React on
              # PatternFly rendered from one template, and overriding that
              # template would mean reworking it on every upgrade.

              # The realm states its memberships to the director. Admin events
              # carry the changes an administrator makes; the user events carry
              # the ones nobody makes by hand -- a default group, a federation
              # mapper -- which is why both are on.
              REALM_EVENTS=\$(jq -n '{
                eventsEnabled: true,
                adminEventsEnabled: true,
                adminEventsDetailsEnabled: false,
                eventsListeners: ["jboss-logging", "gentian-director"]
              }')
              if curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/events/config" -d "\${REALM_EVENTS}" >/dev/null 2>&1; then
                echo "Realm \${REALM} states memberships to the director"
              else
                printf '\033[0;31m[ERROR]\033[0m %s\n' "could not enable the gentian-director event listener on \${REALM}." >&2
                echo "  Without it OpenFGA never learns who is in which group, so every" >&2
                echo "  permission that follows from membership is refused to everyone." >&2
                exit 1
              fi

              PROFILE=\$(curl -sf -H "\${AUTH}" "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/profile")
              if ! echo "\${PROFILE}" | jq -e '.attributes[] | select(.name=="gentian.inviteEmail")' >/dev/null 2>&1; then
                UPDATED=\$(echo "\${PROFILE}" | jq '.attributes += [{"name":"gentian.inviteEmail","displayName":"Recovery email","validations":{"email":{},"length":{"max":255}},"permissions":{"view":["admin"],"edit":["admin"]},"multivalued":false}]')
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/profile" -d "\${UPDATED}"
                echo "user profile gentian.inviteEmail ensured for realm \${REALM}"
              else
                echo "user profile gentian.inviteEmail already present for realm \${REALM}"
              fi

              for ACTION in VERIFY_PROFILE UPDATE_PROFILE; do
                RA=\$(curl -sf -H "\${AUTH}" "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/authentication/required-actions/\${ACTION}" 2>/dev/null || true)
                if [ -n "\${RA}" ]; then
                  UPDATED=\$(echo "\${RA}" | jq '.enabled = false')
                  curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/authentication/required-actions/\${ACTION}" -d "\${UPDATED}" >/dev/null
                  echo "required action \${ACTION} disabled for realm \${REALM}"
                fi
              done
              PROFILE=\$(curl -sf -H "\${AUTH}" "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/profile")
              RELAXED=\$(echo "\${PROFILE}" | jq '.attributes = [.attributes[] | if .name == "firstName" or .name == "lastName" then del(.required) else . end]')
              curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/profile" -d "\${RELAXED}" >/dev/null
              echo "user profile firstName/lastName optional for realm \${REALM}"

              # The groups scope: what Argo CD and Headlamp read a person's
              # platform role from. The edge client does not carry it (the
              # bouncer asks the store, never the token).
              SCOPE_LIST=\$(curl -sf -H "\${AUTH}" "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/client-scopes")
              GROUPS_SCOPE_ID=\$(printf '%s' "\${SCOPE_LIST}" | jq -r '.[] | select(.name=="groups") | .id' | head -1)

              # Create the scope when the realm has none.
              #
              # Keycloak ships a groups scope in some distributions and not others,
              # and this realm had none — so the attach below found nothing, did
              # nothing, and said nothing. The portal's tokens then carried no groups
              # claim, OpenBao refused every one, and it surfaced three layers away as
              # a 401 from the custodian.
              #
              # full.path is what OpenBao matches: its roles bind /group-name with a
              # leading slash, and the bare name matches nothing.
              if [ -z "\${GROUPS_SCOPE_ID}" ] || [ "\${GROUPS_SCOPE_ID}" = "null" ]; then
                echo "No groups client scope in realm \${REALM}; creating one."
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/client-scopes" \\
                  -d '{"name":"groups","protocol":"openid-connect","attributes":{"include.in.token.scope":"true","display.on.consent.screen":"false"}}' \\
                  >/dev/null || { printf '\033[0;31m[ERROR]\033[0m %s\n' "could not create the groups client scope." >&2; exit 1; }
                GROUPS_SCOPE_ID=\$(curl -sf -H "\${AUTH}" "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/client-scopes" \\
                  | jq -r '.[] | select(.name=="groups") | .id' | head -1)
                if [ -z "\${GROUPS_SCOPE_ID}" ] || [ "\${GROUPS_SCOPE_ID}" = "null" ]; then
                  printf '\033[0;31m[ERROR]\033[0m %s\n' "created the groups scope but cannot find it." >&2
                  exit 1
                fi
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/client-scopes/\${GROUPS_SCOPE_ID}/protocol-mappers/models" \\
                  -d '{"name":"groups","protocol":"openid-connect","protocolMapper":"oidc-group-membership-mapper","config":{"claim.name":"groups","full.path":"true","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true"}}' \\
                  >/dev/null || { printf '\033[0;31m[ERROR]\033[0m %s\n' "could not add the group-membership mapper." >&2; exit 1; }
                echo "Created groups client scope with full.path=true"
              fi
              if [ -n "\${GROUPS_SCOPE_ID}" ] && [ "\${GROUPS_SCOPE_ID}" != "null" ]; then
                # Emit the FULL path. OpenBao's roles bind /group-name with a
                # leading slash; the bare name matches nothing and fails as a
                # denied login rather than as a misconfigured claim.
                GM_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/client-scopes/\${GROUPS_SCOPE_ID}/protocol-mappers/models" \\
                  | jq -r '.[] | select(.protocolMapper=="oidc-group-membership-mapper") | .id' | head -1)
                if [ -n "\${GM_ID}" ] && [ "\${GM_ID}" != "null" ]; then
                  GM=\$(curl -sf -H "\${AUTH}" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/client-scopes/\${GROUPS_SCOPE_ID}/protocol-mappers/models/\${GM_ID}")
                  GM_NEW=\$(printf '%s' "\${GM}" | jq '.config["full.path"]="true" | .config["access.token.claim"]="true"')
                  if curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/client-scopes/\${GROUPS_SCOPE_ID}/protocol-mappers/models/\${GM_ID}" \\
                    -d "\${GM_NEW}" >/dev/null 2>&1; then
                    echo "groups mapper: full.path=true"
                  else
                    printf '\033[1;33m[WARN]\033[0m  %s\n' "could not set full.path on the groups mapper" >&2
                  fi
                fi
              fi

              USER_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users?username=\${PORTAL_USERNAME}&exact=true" \\
                | jq -r '.[0].id // empty')
              # No password: its holder sets one through an activation link
              # issued at the handover. Created with what activating takes as
              # required actions; an existing account's password, email (its
              # recovery address) and actions are never overwritten here.
              ACTIONS='["UPDATE_PASSWORD"]'
              [ "\${REQUIRE_MFA:-true}" = "true" ] && ACTIONS='["UPDATE_PASSWORD","CONFIGURE_TOTP"]'
              if [ -z "\${USER_ID}" ]; then
                USER_BODY=\$(jq -n --arg u "\${PORTAL_USERNAME}" --argjson a "\${ACTIONS}" '{
                  username: \$u, firstName: "Platform", lastName: "Administrator",
                  enabled: true, emailVerified: false, requiredActions: \$a
                }')
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users" -d "\${USER_BODY}"
                USER_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users?username=\${PORTAL_USERNAME}&exact=true" \\
                  | jq -r '.[0].id')
                echo "Created user \${PORTAL_USERNAME}"
              else
                # The second-factor requirement as declared; nothing else.
                CUR=\$(curl -sf -H "\${AUTH}" "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/\${USER_ID}")
                HAS_OTP=\$(curl -sf -H "\${AUTH}" "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/\${USER_ID}/credentials" \\
                  | jq '[.[] | select(.type=="otp")] | length > 0')
                UPD=\$(printf '%s' "\${CUR}" | jq --arg mfa "\${REQUIRE_MFA:-true}" --argjson hasotp "\${HAS_OTP}" '
                  .enabled = true
                  | .requiredActions = ((.requiredActions // []) - ["CONFIGURE_TOTP"])
                  | (if \$mfa == "true" and (\$hasotp | not) then .requiredActions += ["CONFIGURE_TOTP"] else . end)')
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/\${USER_ID}" -d "\${UPD}"
                echo "Updated user \${PORTAL_USERNAME} (second factor required: \${REQUIRE_MFA:-true})"
              fi

              ADMIN_GROUP="\${PLATFORM_ADMIN_GROUP}"
              GROUP_LIST=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/groups?search=\${ADMIN_GROUP}&exact=true")
              GROUP_ID=\$(printf '%s' "\${GROUP_LIST}" | jq -r --arg n "\${ADMIN_GROUP}" '.[] | select(.name==\$n) | .id' | head -1)
              if [ -z "\${GROUP_ID}" ] || [ "\${GROUP_ID}" = "null" ]; then
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/groups" \\
                  -d "{\"name\":\"\${ADMIN_GROUP}\"}"
                GROUP_LIST=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/groups?search=\${ADMIN_GROUP}&exact=true")
                GROUP_ID=\$(printf '%s' "\${GROUP_LIST}" | jq -r --arg n "\${ADMIN_GROUP}" '.[] | select(.name==\$n) | .id' | head -1)
                echo "Created group \${ADMIN_GROUP}"
              else
                echo "Group \${ADMIN_GROUP} already exists"
              fi
              curl -sf -X PUT -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/\${USER_ID}/groups/\${GROUP_ID}" >/dev/null || true
              echo "User \${PORTAL_USERNAME} joined \${ADMIN_GROUP}"

              # The account this one replaces. Removed rather than left
              # disabled: two administrators, one of whom cannot be reached by
              # a password reset because their username is not an address, is
              # worse than one. Only ever the exact legacy name, and never the
              # account just created.
              if [ -n "\${LEGACY_USERNAME:-}" ] && [ "\${LEGACY_USERNAME}" != "\${PORTAL_USERNAME}" ]; then
                LEGACY_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users?username=\${LEGACY_USERNAME}&exact=true" \\
                  | jq -r '.[0].id // empty')
                if [ -n "\${LEGACY_ID}" ] && [ "\${LEGACY_ID}" != "\${USER_ID}" ]; then
                  if curl -sf -X DELETE -H "\${AUTH}" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/\${LEGACY_ID}" >/dev/null 2>&1; then
                    echo "Removed the account \${LEGACY_USERNAME} replaced by \${PORTAL_USERNAME}"
                  else
                    printf '\033[1;33m[WARN]\033[0m  %s\n' "could not remove the legacy account \${LEGACY_USERNAME}" >&2
                  fi
                fi
              fi

              # The realm's own administration, granted to the group rather than
              # to the person: whoever the cluster's platform administrators are,
              # they are the ones who manage users, groups and clients here.
              #
              # Without it the identity console answers 403 to the very account
              # the install says to sign in as -- a refusal that reads like a
              # broken login rather than a missing role.
              RM_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=realm-management" \\
                | jq -r '.[0].id // empty')
              if [ -n "\${RM_CLIENT_ID}" ]; then
                RM_ROLE=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${RM_CLIENT_ID}/roles/realm-admin")
                if [ -n "\${RM_ROLE}" ]; then
                  if curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/groups/\${GROUP_ID}/role-mappings/clients/\${RM_CLIENT_ID}" \\
                    -d "[\${RM_ROLE}]" >/dev/null 2>&1; then
                    echo "Group \${ADMIN_GROUP} granted realm-management:realm-admin"
                  else
                    echo "Group \${ADMIN_GROUP} already holds realm-management:realm-admin"
                  fi
                fi
              fi


              # Keycloak's own administration console must be issued a token
              # the Admin REST API will accept.
              #
              # Keycloak 26 turns on lightweight access tokens for
              # security-admin-console. Such a token carries exp, iat, jti,
              # iss, typ, azp, sid and scope, and NOTHING else -- no sub, no
              # aud, no realm_access, no resource_access. The console then
              # calls /admin/serverinfo with it and Keycloak answers 401, so
              # the console never finishes loading and sits on its spinner.
              #
              # Proved on the cluster rather than guessed: the same person, in
              # the same realm, holding realm-admin through
              # gentian:platform:admin, is answered 200 by that same endpoint
              # when the token comes from admin-cli, which issues a full one.
              # The only difference is what the token carries.
              #
              # So this realm's console gets full tokens. It costs a slightly
              # larger token on requests that never leave the identity host,
              # and it is the difference between a console that works and one
              # that does not. Merged into whatever attributes the client
              # already has, so nothing else about it is disturbed.
              ADMIN_CONSOLE_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=security-admin-console" \\
                | jq -r '.[0].id // empty')
              if [ -n "\${ADMIN_CONSOLE_ID}" ]; then
                ADMIN_CONSOLE_BODY=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${ADMIN_CONSOLE_ID}" \\
                  | jq '.attributes["client.use.lightweight.access.token.enabled"] = "false"')
                if [ -n "\${ADMIN_CONSOLE_BODY}" ] && curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${ADMIN_CONSOLE_ID}" \\
                    -d "\${ADMIN_CONSOLE_BODY}" >/dev/null; then
                  echo "security-admin-console issues full access tokens"
                else
                  echo "WARNING: could not turn off lightweight tokens for security-admin-console; the console will 401" >&2
                fi
              fi

              # The kernel zone's edge client is not created here. It is a
              # zone, and a zone is produced by the tenant that owns it:
              # Tenant/platform adopts this realm (AD-10) and its composition
              # writes the client, its redirect and post-logout URIs, its
              # director audience mapper and the Secret the edge reads. This
              # Job creates the realm that client lives in, and stops there.

              ARGOCD_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=gentian-argocd" \\
                | jq -r '.[0].id // empty')
              ARGOCD_BODY=\$(jq -n --arg secret "\${ARGOCD_OIDC_CLIENT_SECRET}" --arg portal "https://argocd.\${KERNEL_DOMAIN}" '{
                clientId: "gentian-argocd",
                name: "ArgoCD",
                enabled: true,
                publicClient: false,
                standardFlowEnabled: true,
                directAccessGrantsEnabled: false,
                serviceAccountsEnabled: false,
                protocol: "openid-connect",
                redirectUris: [(\$portal + "/auth/callback")],
                rootUrl: \$portal,
                baseUrl: "/",
                secret: \$secret
              }')
              if [ -n "\${ARGOCD_CLIENT_ID}" ]; then
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${ARGOCD_CLIENT_ID}" -d "\${ARGOCD_BODY}"
                echo "Updated client gentian-argocd"
              else
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients" -d "\${ARGOCD_BODY}"
                ARGOCD_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=gentian-argocd" \\
                  | jq -r '.[0].id')
                echo "Created client gentian-argocd"
              fi
              if [ -n "\${ARGOCD_CLIENT_ID}" ] && [ -n "\${GROUPS_SCOPE_ID}" ] && [ "\${GROUPS_SCOPE_ID}" != "null" ]; then
                curl -sf -X PUT -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${ARGOCD_CLIENT_ID}/default-client-scopes/\${GROUPS_SCOPE_ID}" >/dev/null 2>&1 || true
                echo "gentian-argocd default scope: groups"
              fi

              # Headlamp. Confidential, because the token it receives is what
              # the impersonating proxy in front of the API server verifies --
              # a public client would let anyone mint one.
              HEADLAMP_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=headlamp" \\
                | jq -r '.[0].id // empty')
              HEADLAMP_BODY=\$(jq -n --arg secret "\${HEADLAMP_CLIENT_SECRET}" --arg base "https://headlamp.\${KERNEL_DOMAIN}" '{
                clientId: "headlamp",
                name: "Headlamp",
                enabled: true,
                publicClient: false,
                standardFlowEnabled: true,
                directAccessGrantsEnabled: false,
                serviceAccountsEnabled: false,
                protocol: "openid-connect",
                redirectUris: [(\$base + "/oidc-callback"), (\$base + "/*")],
                rootUrl: \$base,
                baseUrl: "/",
                webOrigins: [\$base],
                secret: \$secret
              }')
              if [ -n "\${HEADLAMP_CLIENT_ID}" ]; then
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${HEADLAMP_CLIENT_ID}" -d "\${HEADLAMP_BODY}"
                echo "Updated client headlamp"
              else
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients" -d "\${HEADLAMP_BODY}"
                HEADLAMP_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=headlamp" \\
                  | jq -r '.[0].id')
                echo "Created client headlamp"
              fi
              # Without the groups claim the proxy impersonates a person with
              # no groups, and every API call is refused by RBAC.
              if [ -n "\${HEADLAMP_CLIENT_ID}" ] && [ -n "\${GROUPS_SCOPE_ID}" ] && [ "\${GROUPS_SCOPE_ID}" != "null" ]; then
                curl -sf -X PUT -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${HEADLAMP_CLIENT_ID}/default-client-scopes/\${GROUPS_SCOPE_ID}" >/dev/null 2>&1 || true
                echo "headlamp default scope: groups"
              fi

              # The CLI. kubectl gentian is a client of the director, as the
              # console is: a person signs in with the device flow (a code shown
              # in the terminal, confirmed in a browser) and every command
              # carries their own token, which the director checks like any
              # other. Public, because a CLI on a workstation cannot keep a
              # secret; device grant only, so it cannot be used to collect a
              # password; the director's audience and the groups claim, so the
              # token is one the director accepts and decides on.
              CLI_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=gentian-cli" \\
                | jq -r '.[0].id // empty')
              CLI_BODY=\$(jq -n '{
                clientId: "gentian-cli",
                name: "Gentian CLI",
                enabled: true,
                publicClient: true,
                standardFlowEnabled: false,
                implicitFlowEnabled: false,
                directAccessGrantsEnabled: false,
                serviceAccountsEnabled: false,
                protocol: "openid-connect",
                attributes: {"oauth2.device.authorization.grant.enabled": "true"}
              }')
              if [ -n "\${CLI_CLIENT_ID}" ]; then
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLI_CLIENT_ID}" -d "\${CLI_BODY}"
                echo "Updated client gentian-cli"
              else
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients" -d "\${CLI_BODY}"
                CLI_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=gentian-cli" \\
                  | jq -r '.[0].id')
                echo "Created client gentian-cli"
              fi
              if [ -n "\${CLI_CLIENT_ID}" ]; then
                # The audience is named as a custom audience, not as a client:
                # no client called gentian-director exists in any realm, and
                # Keycloak (26.8 on) leaves an audience out of the token when it
                # names a client that is absent or disabled. A mapper written
                # the earlier way is rewritten, so a re-run repairs it.
                AUD_BODY='{"name":"director-audience","protocol":"openid-connect","protocolMapper":"oidc-audience-mapper","config":{"included.custom.audience":"gentian-director","id.token.claim":"false","access.token.claim":"true","introspection.token.claim":"true"}}'
                AUD_MAPPER=\$(curl -sf -H "\${AUTH}" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLI_CLIENT_ID}/protocol-mappers/models" \\
                    | jq -c '[.[] | select(.name=="director-audience")][0] // empty' 2>/dev/null || true)
                if [ -z "\${AUD_MAPPER}" ]; then
                  curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLI_CLIENT_ID}/protocol-mappers/models" \\
                    -d "\${AUD_BODY}" >/dev/null
                elif [ "\$(printf '%s' "\${AUD_MAPPER}" | jq -r '.config["included.custom.audience"] // empty')" != "gentian-director" ]; then
                  AUD_MAPPER_ID=\$(printf '%s' "\${AUD_MAPPER}" | jq -r '.id')
                  curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLI_CLIENT_ID}/protocol-mappers/models/\${AUD_MAPPER_ID}" \\
                    -d "\$(printf '%s' "\${AUD_BODY}" | jq -c --arg id "\${AUD_MAPPER_ID}" '. + {id: \$id}')" >/dev/null
                fi
                if [ -n "\${GROUPS_SCOPE_ID}" ] && [ "\${GROUPS_SCOPE_ID}" != "null" ]; then
                  curl -sf -X PUT -H "\${AUTH}" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLI_CLIENT_ID}/default-client-scopes/\${GROUPS_SCOPE_ID}" >/dev/null 2>&1 || true
                fi
                echo "gentian-cli: device grant, director audience, groups"
              fi

${refresh_shell}
              if [ "\${LLM_SUPPORT}" = "true" ]; then
                # Best-effort, deliberately. Everything in here serves an optional LLM
                # dashboard, while what follows the block configures realm SMTP — which
                # password resets and invitations depend on. Under set -e a 4xx from any
                # of these calls ends the Job, so an unreachable dashboard would take
                # mail delivery down with it. Containment has to be set +e: busybox
                # suppresses errexit inside a subshell used as a condition, so ( ... ) ||
                # warn does not actually catch anything.
                set +e
                # Only kernel-realm users can ever reach this client (tenant users
                # live in separate per-tenant realms), so a flat hardcoded
                # litellm_role=proxy_admin claim is safe for now — LLM admin
                # stays a platform-admin-only capability until per-tenant access
                # is designed (see docs/design/llms.md).
                LITELLM_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=litellm-dashboard" \\
                  | jq -r '.[0].id // empty')
                LITELLM_BODY=\$(jq -n --arg secret "\${LITELLM_SSO_CLIENT_SECRET}" --arg base "https://llm.\${KERNEL_DOMAIN}" '{
                  clientId: "litellm-dashboard",
                  name: "LiteLLM Admin Console",
                  enabled: true,
                  publicClient: false,
                  standardFlowEnabled: true,
                  directAccessGrantsEnabled: false,
                  serviceAccountsEnabled: false,
                  protocol: "openid-connect",
                  redirectUris: [(\$base + "/sso/callback")],
                  rootUrl: \$base,
                  baseUrl: "/",
                  secret: \$secret
                }')
                if [ -n "\${LITELLM_CLIENT_ID}" ]; then
                  curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${LITELLM_CLIENT_ID}" -d "\${LITELLM_BODY}"
                  echo "Updated client litellm-dashboard"
                else
                  curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients" -d "\${LITELLM_BODY}"
                  LITELLM_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=litellm-dashboard" \\
                    | jq -r '.[0].id')
                  echo "Created client litellm-dashboard"
                fi
                if [ -n "\${LITELLM_CLIENT_ID}" ]; then
                  MAPPER_ID=\$(curl -sf -H "\${AUTH}" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${LITELLM_CLIENT_ID}/protocol-mappers/models" \\
                    | jq -r '.[] | select(.name=="litellm-role") | .id' | head -1)
                  MAPPER_BODY='{
                    "name": "litellm-role",
                    "protocol": "openid-connect",
                    "protocolMapper": "oidc-hardcoded-claim-mapper",
                    "config": {
                      "claim.name": "litellm_role",
                      "claim.value": "proxy_admin",
                      "jsonType.label": "String",
                      "id.token.claim": "true",
                      "access.token.claim": "true",
                      "userinfo.token.claim": "true"
                    }
                  }'
                  # Reported, not fatal. This mapper adds a convenience claim to an LLM
                  # dashboard. Ending the portal bootstrap over it leaves the cluster with
                  # no working login at all, which is far worse — and it did exactly that:
                  # a failing POST exited 22 under set -e, so every later step, including
                  # the groups scope the login depends on, never ran.
                  MAPPER_RC=0
                  MAPPER_HTTP=""
                  if [ -n "\${MAPPER_ID}" ] && [ "\${MAPPER_ID}" != "null" ]; then
                    # Keycloak's update takes a full ProtocolMapperRepresentation and reads
                    # the target from the body's id, not from the path alone. Re-sending the
                    # create body unchanged is rejected, which is where the 4xx came from.
                    MAPPER_HTTP=\$(printf '%s' "\${MAPPER_BODY}" | jq --arg id "\${MAPPER_ID}" '. + {id: \$id}' \\
                      | curl -s -o /tmp/mapper.err -w '%{http_code}' -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                        "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${LITELLM_CLIENT_ID}/protocol-mappers/models/\${MAPPER_ID}" -d @-) || MAPPER_RC=\$?
                  else
                    MAPPER_HTTP=\$(curl -s -o /tmp/mapper.err -w '%{http_code}' -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                      "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${LITELLM_CLIENT_ID}/protocol-mappers/models" -d "\${MAPPER_BODY}") || MAPPER_RC=\$?
                  fi
                  case "\${MAPPER_HTTP}" in
                    2*) echo "litellm-dashboard role mapper: litellm_role=proxy_admin" ;;
                    *)
                      # The status and Keycloak's own message, not just a curl exit code. "exit
                      # 22" says a 4xx happened and nothing about which or why, which is a
                      # dead end for whoever reads the log next.
                      if [ "\${MAPPER_RC}" -ne 0 ]; then
                        # curl never got an answer, so there is no status to report.
                        # Distinguishing this from a 4xx matters: one is the network, the
                        # other is Keycloak rejecting what we sent.
                        printf '\033[1;33m[WARN]\033[0m  litellm-dashboard role mapper not applied (curl exit %s, no response).\n' "\${MAPPER_RC}" >&2
                      else
                        printf '\033[1;33m[WARN]\033[0m  litellm-dashboard role mapper not applied (HTTP %s).\n' "\${MAPPER_HTTP:-none}" >&2
                      fi
                      if [ -s /tmp/mapper.err ]; then
                        printf '\033[1;33m[WARN]\033[0m    Keycloak said: %s\n' "\$(head -c 300 /tmp/mapper.err)" >&2
                      fi
                      printf '\033[1;33m[WARN]\033[0m    The LLM dashboard will not see litellm_role; nothing else is affected.\n' >&2
                      ;;
                  esac
                else
                  # The client create or lookup above failed. Without this the whole LLM
                  # section just stops here without saying so, and the mapper warning that
                  # would have explained it never runs either.
                  printf '\033[1;33m[WARN]\033[0m  litellm-dashboard client could not be created or found.\n' >&2
                  printf '\033[1;33m[WARN]\033[0m    LLM dashboard SSO is not configured; the rest of the bootstrap continues.\n' >&2
                fi
                set -e
              fi

${refresh_shell}
${smtp_shell}

              # A workday session, a short-lived token.
              #
              # LAST, after the SMTP fragment above. That fragment reads the
              # whole realm, sets smtpServer on the copy and writes the copy
              # back, so anything written before it that the copy predates is
              # silently undone -- which is exactly what happened to this
              # block: the Job reported setting a five-minute token and the
              # realm still said twelve hours.
              #
              # These two numbers are what makes removing someone take effect.
              # The SSO session lasts a working day, so nobody is asked for a
              # password again mid-morning. The ACCESS token lasts five
              # minutes, and the edge refreshes it against Keycloak without
              # the person noticing. A refresh re-checks the session and
              # re-reads the account, so disabling someone, ending their
              # session or changing their groups stops them within one token
              # lifetime rather than at the end of the day.
              #
              # This is why there is no revocation list anywhere: it existed
              # to make that immediate while the access token lived twelve
              # hours, which is what this realm was set to.
              REALM_THEMES=\$(jq -n '{
                loginTheme:"gentian", adminTheme:"gentian", accountTheme:"gentian",
                accessTokenLifespan: 300,
                ssoSessionIdleTimeout: 43200,
                ssoSessionMaxLifespan: 43200
              }')
              if curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}" -d "\${REALM_THEMES}" >/dev/null 2>&1; then
                echo "Realm \${REALM}: gentian theme, 5-minute access tokens, 12-hour sessions"
              else
                printf '\033[1;33m[WARN]\033[0m  %s\n' "could not set the realm themes on \${REALM}" >&2
              fi

              echo "Portal bootstrap complete for \${PORTAL_USERNAME}"
          env:
            - name: KEYCLOAK_URL
              valueFrom:
                secretKeyRef:
                  name: keycloak-admin
                  key: url
            - name: KEYCLOAK_ADMIN_USERNAME
              valueFrom:
                secretKeyRef:
                  name: keycloak-admin
                  key: username
            - name: KEYCLOAK_ADMIN_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: keycloak-admin
                  key: password
            - name: KERNEL_DOMAIN
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: kernel_domain
            - name: KERNEL_REALM
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: kernel_realm
            - name: PORTAL_USERNAME
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: username
            - name: PORTAL_EMAIL
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: email
            - name: REQUIRE_MFA
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: require_mfa
            - name: PLATFORM_ADMIN_GROUP
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: platform_admin_group
            - name: LEGACY_USERNAME
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: legacy_username
            - name: HEADLAMP_CLIENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: headlamp_client_secret
            - name: ARGOCD_OIDC_CLIENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: argocd_client_secret
            - name: LLM_SUPPORT
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: llm_support
            - name: LITELLM_SSO_CLIENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: litellm_sso_client_secret
            - name: MAIL_SERVICE_MODE
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: mail_service_mode
            - name: SMTP_CONFIGURE
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: smtp_configure
            - name: SMTP_HOST
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: smtp_host
            - name: SMTP_PORT
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: smtp_port
            - name: SMTP_USER
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: smtp_user
            - name: SMTP_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: smtp_password
            - name: SMTP_SSL
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: smtp_ssl
            - name: SMTP_STARTTLS
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: smtp_starttls
            - name: SMTP_FROM
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: smtp_from
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 200m
              memory: 128Mi
EOF

    # 720s, not 180s: the Job now polls Keycloak for up to 10 minutes
    # before touching anything (see the loop in its script), so the wait
    # here has to outlast that budget or it reports failure over a Job
    # that is still legitimately waiting.
    if ! kubectl wait "job/${job_name}" -n "${ns}" --for=condition=complete --timeout=720s; then
        error "Keycloak portal bootstrap Job failed."
        gentian_job_logs "${ns}" "${job_name}" Failed 80
        return 1
    fi

    gentian_job_logs "${ns}" "${job_name}" Succeeded 20
    success "Kernel realm ${kernel_realm}: clients, group and platform admin ${username} are ready."
    info "OIDC issuer: https://id.${kernel_domain}/auth/realms/${kernel_realm}"
    info "  Username: ${username}"
    info "  No password: the handover issues the activation link that sets one."
}

install_portal_login() {
    banner "Kernel realm sign-in"

    # Waited for, not tested once. keycloak-admin is written by ESO once
    # Keycloak is up, so at this point in a fresh install it is usually seconds
    # away rather than absent: a single probe failed the step while the Secret
    # appeared 100 seconds later. Anything ESO produces is eventually consistent
    # with the step that needs it.
    local deadline=$(( SECONDS + 180 ))
    until kubectl get secret keycloak-admin -n "$(_pl_identity_ns)" >/dev/null 2>&1; do
        if (( SECONDS >= deadline )); then
            error "keycloak-admin Secret did not appear within 180s."
            error "  It is written by ESO from OpenBao once Keycloak is running. Check:"
            error "    kubectl get externalsecret -n $(_pl_identity_ns)"
            error "    kubectl get pods -n $(_pl_identity_ns) -l app.kubernetes.io/name=keycloakx"
            return 1
        fi
        sleep 5
    done

    ensure_keycloak_admin_secret_url || return 1
    ensure_edge_gateway_readiness

    run_keycloak_portal_bootstrap_job
    configure_argocd_oidc

    success "The kernel realm can be signed in to."
    info "  https://platform.${KERNEL_DOMAIN}/  (once the platform desktop is Ready)"
    info "  user: admin@${KERNEL_DOMAIN} — activated through the link the handover issues"
}

# tenant_admin_requires_mfa <tenant> — whether that tenant's administrator
# must enrol a second factor: its manifest's spec.admin.requireMFA, true
# unless it says false. The same setting, in the same place, for the platform
# tenant and for every other.
tenant_admin_requires_mfa() {
    local file="${GENTIAN_DEPLOYMENTS_PATH:-}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}/tenants/$1/tenant.yaml"
    local v=""
    [[ -f "${file}" ]] && v="$(yq eval '.spec.admin.requireMFA' "${file}" 2>/dev/null || true)"
    [[ "${v}" == "false" ]] && echo false || echo true
}

# platform_admin_requires_mfa — the platform administrator's.
platform_admin_requires_mfa() {
    tenant_admin_requires_mfa platform
}

# issue_admin_activation <realm> <username> <require-mfa> <recovery-email> <again>
# — hand one administrator account to its holder, once.
#
# The account of a realm's administrator: the cluster's, in the kernel realm,
# or a tenant's, in the tenant's own. One function for both, because the
# practice is one: an account that already has a password is left alone -- it
# has been activated, and its holder signs in with what they chose. Otherwise
# a single-use, expiring link is issued -- set a password, and enrol a second
# factor when <require-mfa> is true -- and mailed to <recovery-email> when
# there is one (or to the address asked for here), or shown here, once, when
# there is not, or when the realm cannot send mail yet (a relay credential is
# supplied after the administrator's sign-in on an external-mail cluster).
# The link comes straight from Keycloak to this terminal over a Service
# address or port-forward; it is never written to a pod's log.
#
# <again> is the command that issues a new link later, named where the
# account turns out to be activated already.
#
# Prints what to do; returns 0 when the account is usable or a link was handed
# over, 1 when nothing could be issued.
issue_admin_activation() {
    local realm="$1" username="$2" require_mfa="$3" email="${4:-}" again="${5:-}" ns
    ns="$(_pl_identity_ns)"
    local kc_user kc_pass base token uid creds has_pw
    kc_user="$(kubectl get secret keycloak-admin -n "${ns}" -o jsonpath='{.data.username}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    kc_pass="$(kubectl get secret keycloak-admin -n "${ns}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    local svc="${GENTIAN_IDP_KEYCLOAK_RELEASE:-gentian-idp-keycloak}-keycloakx-http"
    if [[ -z "${kc_user}" || -z "${kc_pass}" ]] || ! base="$(gentian_service_addr "${svc}" "${ns}" 8080 http)"; then
        warn "  Keycloak cannot be reached from here to issue the activation link."
        return 1
    fi
    base="${base}/auth"
    token="$(curl -sS --max-time 15 -d client_id=admin-cli -d grant_type=password \
        --data-urlencode "username=${kc_user}" --data-urlencode "password=${kc_pass}" \
        "${base}/realms/master/protocol/openid-connect/token" 2>/dev/null | jq -r '.access_token // empty')"
    [[ -n "${token}" ]] || { warn "  Keycloak refused the installer's admin credential."; return 1; }
    local auth="Authorization: Bearer ${token}"
    uid="$(curl -sS --max-time 15 -H "${auth}" \
        "${base}/admin/realms/${realm}/users?username=${username}&exact=true" | jq -r '.[0].id // empty')"
    [[ -n "${uid}" ]] || { warn "  ${username} does not exist in realm ${realm}."; return 1; }
    creds="$(curl -sS --max-time 15 -H "${auth}" "${base}/admin/realms/${realm}/users/${uid}/credentials")"
    has_pw="$(printf '%s' "${creds}" | jq '[.[] | select(.type=="password")] | length > 0' 2>/dev/null || echo false)"
    # Forced (./install.sh --activate-admin): a new link even for an account
    # that has a password, which is how a holder who lost it gets back in.
    if [[ "${has_pw}" == "true" && "${GENTIAN_ACTIVATE_FORCE:-0}" != "1" ]]; then
        info "  ${username} is activated: sign in with the password its holder chose."
        if [[ -n "${again}" ]]; then
            info "  Lost it? A new link: ${again}"
        fi
        return 0
    fi

    local actions='["UPDATE_PASSWORD"]'
    if [[ "${require_mfa}" == "true" ]]; then
        actions='["UPDATE_PASSWORD","CONFIGURE_TOTP"]'
    fi
    local can_mail=false
    # Whether this realm can send mail now. With an external relay the
    # credential arrives through the console, after this sign-in -- so a
    # tunnel cluster, or any cluster whose relay is not supplied yet, cannot
    # mail the link, and asking for an address to mail it to only sets up a
    # failure. Ask only when there is a mail server to send through.
    if curl -sS --max-time 15 -H "${auth}" "${base}/admin/realms/${realm}" 2>/dev/null \
        | jq -e '(.smtpServer.host // "") != "" and (.smtpServer.from // "") != ""' >/dev/null 2>&1; then
        can_mail=true
    fi
    if [[ "${can_mail}" != "true" ]]; then
        info "  Realm ${realm} has no mail server yet (the relay is supplied after the"
        info "  administrator's sign-in), so the link is shown here rather than mailed."
    fi
    if [[ "${can_mail}" == "true" && -z "${email}" && -t 0 && "${GENTIAN_NONINTERACTIVE:-0}" != "1" ]]; then
        printf '  Recovery email for %s (Enter to show the link here instead): ' "${username}"
        read -r email || email=""
    fi
    # Where the person lands afterwards: the zone client's own root, read off
    # the client the way the director's invitations read it. A redirect the
    # client does not list is refused, and its list is the OIDC callbacks.
    local client="gentian-edge-${realm}" redirect
    redirect="$(curl -sS --max-time 15 -H "${auth}" "${base}/admin/realms/${realm}/clients?clientId=${client}" \
        | jq -r '.[0].rootUrl // .[0].baseUrl // empty' 2>/dev/null)"
    [[ -n "${redirect}" ]] && redirect="${redirect%/}/"
    if [[ -n "${email}" && "${can_mail}" != "true" ]]; then
        # Given ahead of time (in install.env): kept as the recovery address
        # for later resets, not mailed to now.
        local cur0 upd0
        cur0="$(curl -sS --max-time 15 -H "${auth}" "${base}/admin/realms/${realm}/users/${uid}")"
        upd0="$(printf '%s' "${cur0}" | jq --arg e "${email}" '.email = $e | .attributes["gentian.inviteEmail"] = [$e]')"
        curl -sf --max-time 15 -X PUT -H "${auth}" -H "Content-Type: application/json" \
            "${base}/admin/realms/${realm}/users/${uid}" -d "${upd0}" >/dev/null &&
            info "  ${email} is kept as the recovery address for later resets."
        email=""
    fi
    if [[ -n "${email}" ]]; then
        local cur upd mailed_actions
        # VERIFY_EMAIL in the mailed link only: following a link that arrived
        # at the address is what proves the address, and Keycloak (26.8 on)
        # records that only when the link names the action. It adds no step.
        # The link shown below does not carry it: nothing reached the address.
        mailed_actions="$(jq -c '["VERIFY_EMAIL"] + .' <<<"${actions}")"
        cur="$(curl -sS --max-time 15 -H "${auth}" "${base}/admin/realms/${realm}/users/${uid}")"
        upd="$(printf '%s' "${cur}" | jq --arg e "${email}" '.email = $e | .attributes["gentian.inviteEmail"] = [$e]')"
        if curl -sf --max-time 15 -X PUT -H "${auth}" -H "Content-Type: application/json" \
                "${base}/admin/realms/${realm}/users/${uid}" -d "${upd}" >/dev/null &&
           curl -sf --max-time 30 -X PUT -H "${auth}" -H "Content-Type: application/json" \
                "${base}/admin/realms/${realm}/users/${uid}/execute-actions-email?client_id=${client}${redirect:+&redirect_uri=$(jq -rn --arg r "${redirect}" '$r|@uri')}" \
                -d "${mailed_actions}" >/dev/null; then
            success "  Activation link mailed to ${email}: it sets ${username}'s password$( [[ "${actions}" == *TOTP* ]] && echo ' and second factor')."
            return 0
        fi
        warn "  The activation mail could not be sent (is the realm's mail configured yet?)."
        warn "  ${email} is kept as the recovery address; showing the link here instead."
    fi
    local answer link expires
    answer="$(curl -sS --max-time 15 -w '\n%{http_code}' -X POST -H "${auth}" -H "Content-Type: application/json" \
        "${base}/realms/${realm}/gentian-activation/users/${uid}/link" \
        -d "$(jq -n --argjson a "${actions}" --arg c "${client}" --arg r "${redirect}" '{actions:$a, clientId:$c} + (if $r != "" then {redirectUri:$r} else {} end)')")"
    local status="${answer##*$'\n'}"
    answer="${answer%$'\n'*}"
    link="$(printf '%s' "${answer}" | jq -r '.link // empty' 2>/dev/null)"
    expires="$(printf '%s' "${answer}" | jq -r '.expiresAt // empty' 2>/dev/null)"
    if [[ -z "${link}" ]]; then
        warn "  Keycloak issued no activation link: HTTP ${status} ${answer:-(empty answer)}"
        return 1
    fi
    echo ""
    echo -e "${GREEN}  Activate ${username} — open this once, it works one time:${NC}"
    echo -e "${GREEN}    ${link}${NC}"
    [[ -n "${expires}" ]] && info "  Valid until $(date -d "@${expires}" 2>/dev/null || date -r "${expires}" 2>/dev/null || echo "${expires}")."
    info "  It sets the password$( [[ "${actions}" == *TOTP* ]] && echo ' and a second factor'); nobody else ever knows either."
    return 0
}

# issue_platform_admin_activation — the cluster administrator's account, in
# the kernel realm (CLUSTER_ADMIN_RECOVERY_EMAIL, or asked for).
issue_platform_admin_activation() {
    local kernel_domain="${KERNEL_DOMAIN:?}" realm="${KERNEL_REALM:-kernel}"
    issue_admin_activation "${realm}" "admin@${kernel_domain}" "$(platform_admin_requires_mfa)" \
        "${CLUSTER_ADMIN_RECOVERY_EMAIL:-}" "./install.sh --activate-admin"
}

# issue_tenant_admin_activation <tenant> [recovery-email] — a tenant
# administrator's account, in the tenant's own realm.
#
# For the user tenant of a single-tenancy cluster, which the install creates
# after the handover (E-04): for every other tenant, and for this one later,
# the registrar issues these (kubectl gentian tenants activate-admin), as the
# person who asked. The realm and the login are the operator's: a tenant's
# realm carries its name unless its manifest says otherwise, and its
# administrator's login is the address the operator reports on the tenant's
# status -- on the tenant's custom domain when it has one, and
# user-admin@<kernel-domain> for the tenant that lives on the cluster's own
# domain, where admin@ is the platform admin's. No login is guessed: a tenant
# whose status names none yet has no account to hand over.
issue_tenant_admin_activation() {
    local tenant="$1" email="${2:-}" realm username
    realm="$(kubectl get tenant "${tenant}" -o jsonpath='{.spec.isolation.keycloakRealm}' 2>/dev/null || true)"
    realm="${realm:-${tenant}}"
    username="$(kubectl get tenant "${tenant}" -o jsonpath='{.status.adminEmail}' 2>/dev/null || true)"
    case "${username}" in
        ""|*.invalid)
            warn "  Tenant/${tenant} does not report its administrator's login yet (status.adminEmail)."
            return 1 ;;
    esac
    info "  User: ${username}"
    issue_admin_activation "${realm}" "${username}" "$(tenant_admin_requires_mfa "${tenant}")" \
        "${email}" "kubectl gentian tenants activate-admin ${tenant}"
}

print_portal_login_summary() {
    local kernel_domain="${KERNEL_DOMAIN:-}"
    [[ -n "${kernel_domain}" ]] || return 0
    echo ""
    echo -e "${GREEN}  Platform desktop (the platform admin):${NC}"
    echo -e "${GREEN}    URL      : https://platform.${kernel_domain}/${NC}"
    echo -e "${GREEN}    User     : admin@${kernel_domain}${NC}"
    echo -e "${GREEN}    Password : set by its holder through the activation link — never stored or printed${NC}"
    echo -e "${GREEN}    OIDC     : https://id.${kernel_domain}/auth/realms/${KERNEL_REALM:-kernel}${NC}"
}
