#!/bin/bash
# shellcheck disable=SC2034
# Portal login bootstrap — Keycloak OIDC client, kernel user, gentian-portal ArgoCD app.
# Sourced from install.sh Step 14 (Stage 1 login dogfood).

set -euo pipefail

_portal_derive_password() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        local existing_pw
        existing_pw=$(bao kv get -mount=secret -field=user_password identity/portal-admin 2>/dev/null || true)
        if [[ -n "${existing_pw}" ]]; then
            echo "${existing_pw}"
        else
            local new_pw
            new_pw=$(openssl rand -hex 24)
            bao kv patch -mount=secret identity/portal-admin user_password="${new_pw}" >/dev/null 2>&1 || \
            bao kv put -mount=secret identity/portal-admin user_password="${new_pw}" >/dev/null 2>&1
            echo "${new_pw}"
        fi
    else
        echo -n "portal-bootstrap:user_password" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT:-}" | awk '{print $2}'
    fi
}

_platform_admin_derive_password() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        local existing_pw
        existing_pw=$(bao kv get -mount=secret -field=admin_password identity/portal-admin 2>/dev/null || true)
        if [[ -n "${existing_pw}" ]]; then
            echo "${existing_pw}"
        else
            local new_pw
            new_pw=$(openssl rand -hex 24)
            bao kv patch -mount=secret identity/portal-admin admin_password="${new_pw}" >/dev/null 2>&1 || \
            bao kv put -mount=secret identity/portal-admin admin_password="${new_pw}" >/dev/null 2>&1
            echo "${new_pw}"
        fi
    else
        echo -n "portal-bootstrap:administrator_password" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT:-}" | awk '{print $2}'
    fi
}

_portal_bff_derive_secret() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        local existing_sec
        existing_sec=$(bao kv get -mount=secret -field=bff_client_secret identity/portal-admin 2>/dev/null || true)
        if [[ -n "${existing_sec}" ]]; then
            echo "${existing_sec}"
        else
            local new_sec
            new_sec=$(openssl rand -hex 24)
            bao kv patch -mount=secret identity/portal-admin bff_client_secret="${new_sec}" >/dev/null 2>&1 || \
            bao kv put -mount=secret identity/portal-admin bff_client_secret="${new_sec}" >/dev/null 2>&1
            echo "${new_sec}"
        fi
    else
        echo -n "portal-bootstrap:bff_client_secret" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT:-}" | awk '{print $2}'
    fi
}

_argocd_oidc_derive_secret() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        local existing_sec
        existing_sec=$(bao kv get -mount=secret -field=argocd_client_secret identity/portal-admin 2>/dev/null || true)
        if [[ -n "${existing_sec}" ]]; then
            echo "${existing_sec}"
        else
            local new_sec
            new_sec=$(openssl rand -hex 24)
            bao kv patch -mount=secret identity/portal-admin argocd_client_secret="${new_sec}" >/dev/null 2>&1 || \
            bao kv put -mount=secret identity/portal-admin argocd_client_secret="${new_sec}" >/dev/null 2>&1
            echo "${new_sec}"
        fi
    else
        echo -n "portal-bootstrap:argocd_client_secret" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT:-}" | awk '{print $2}'
    fi
}

_litellm_sso_derive_secret() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        local existing_sec
        existing_sec=$(bao kv get -mount=secret -field=litellm_sso_client_secret identity/portal-admin 2>/dev/null || true)
        if [[ -n "${existing_sec}" ]]; then
            echo "${existing_sec}"
        else
            local new_sec
            new_sec=$(openssl rand -hex 24)
            bao kv patch -mount=secret identity/portal-admin litellm_sso_client_secret="${new_sec}" >/dev/null 2>&1 || \
            bao kv put -mount=secret identity/portal-admin litellm_sso_client_secret="${new_sec}" >/dev/null 2>&1
            echo "${new_sec}"
        fi
    else
        echo -n "portal-bootstrap:litellm_sso_client_secret" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT:-}" | awk '{print $2}'
    fi
}

ensure_portal_bff_secret() {
    local ns="platform-kernel"
    local secret
    secret="$(_portal_bff_derive_secret)"
    kubectl create secret generic gentian-portal-bff -n "${ns}" \
        --from-literal=client_id="gentian-portal-bff" \
        --from-literal=client_secret="${secret}" \
        --dry-run=client -o yaml | kubectl apply -f -
    echo "${secret}"
}

ensure_argocd_oidc_secret() {
    local ns="platform-kernel"
    local secret
    secret="$(_argocd_oidc_derive_secret)"
    kubectl create secret generic gentian-argocd -n "${ns}" \
        --from-literal=client_id="gentian-argocd" \
        --from-literal=client_secret="${secret}" \
        --dry-run=client -o yaml | kubectl apply -f -
    # Also patch the actual argocd-secret in the argocd namespace
    kubectl patch secret argocd-secret -n argocd --type merge \
        -p "{\"stringData\":{\"oidc.keycloak.clientSecret\":\"${secret}\"}}"
    echo "${secret}"
}

ensure_litellm_sso_secret() {
    local ns="platform-kernel"
    local secret
    secret="$(_litellm_sso_derive_secret)"
    kubectl create secret generic litellm-dashboard-sso -n "${ns}" \
        --from-literal=client_id="litellm-dashboard" \
        --from-literal=client_secret="${secret}" \
        --dry-run=client -o yaml | kubectl apply -f -
    echo "${secret}"
}

_keycloak_internal_service_url() {
    local ns="${1:-platform-kernel}"
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
    local ns="${1:-platform-kernel}"
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
    local ns="platform-kernel"
    local url current eso_manifest

    if ! url=$(wait_for_keycloak_http_service "${ns}" 120); then
        if declare -F ensure_suze_idp_workloads >/dev/null 2>&1; then
            warn "Keycloak Service not found — attempting Suze IdP heal..."
            ensure_suze_idp_workloads "" 600 || true
            url=$(wait_for_keycloak_http_service "${ns}" 180) || true
        fi
    fi

    if [[ -z "${url:-}" ]]; then
        error "No Keycloak HTTP service found in ${ns} (expected ${GENTIAN_IDP_KEYCLOAK_RELEASE:-gentian-idp-keycloak}-keycloakx-http)."
        return 1
    fi
    current=$(kubectl get secret keycloak-admin -n "${ns}" -o jsonpath='{.data.url}' 2>/dev/null | base64 -d || true)
    if [[ "${current}" == "${url}" ]]; then
        info "keycloak-admin URL: ${url}"
        return 0
    fi
    warn "Updating keycloak-admin URL (${current:-missing} -> ${url}) via ExternalSecret"
    eso_manifest="${SCRIPT_DIR}/kernel/services/postgresql/manifests/dev/externalsecrets.yaml"
    if [[ -f "${eso_manifest}" ]]; then
        kubectl apply -f "${eso_manifest}" >/dev/null
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
    fi
    error "keycloak-admin URL still ${current:-missing}; expected ${url}"
    return 1
}

ensure_portal_gateway_readiness() {
    local ns="platform-kernel"
    if ! kubectl get secret wildcard-tls -n "${ns}" >/dev/null 2>&1; then
        if kubectl get secret wildcard-kernel-tls -n cert-manager >/dev/null 2>&1; then
            info "Copying wildcard-kernel-tls → ${ns}/wildcard-tls for kernel-public-gateway..."
            kubectl get secret wildcard-kernel-tls -n cert-manager -o json | python3 -c "
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
    local mode="${MAIL_SERVICE_MODE:-external}"
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
            if ! declare -F _derive >/dev/null 2>&1; then
                return 1
            fi
            # platform-kernel, matching the operator's SERVICES_NAMESPACE and the
            # namespace 09-infra-helm deploys Postfix into — not gentian-<env>.
            KC_SMTP_HOST="postfix-${env}.${SERVICES_NAMESPACE:-platform-kernel}.svc.cluster.local"
            KC_SMTP_PORT="587"
            KC_SMTP_USER="gentian-system@${kernel_domain}"
            KC_SMTP_PASSWORD="$(_derive smtp password)"
            KC_SMTP_SSL="false"
            # In-cluster Postfix advertises STARTTLS with a self-signed cert that Keycloak
            # does not trust; submission stays on the cluster network without TLS upgrade.
            KC_SMTP_STARTTLS="false"
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
                REALM_JSON=$(curl -sf -H "${AUTH}" "${KEYCLOAK_BASE}/admin/realms/${REALM}")
                SMTP_JSON=$(jq -n \
                  --arg host "${SMTP_HOST}" \
                  --arg port "${SMTP_PORT}" \
                  --arg from "${SMTP_FROM}" \
                  --arg user "${SMTP_USER}" \
                  --arg pass "${SMTP_PASSWORD}" \
                  --arg ssl "${SMTP_SSL}" \
                  --arg starttls "${SMTP_STARTTLS}" \
                  '{
                    host: $host,
                    port: $port,
                    from: $from,
                    fromDisplayName: "Gentian",
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
_apply_keycloak_smtp_secret() {
    local ns="${1:-platform-kernel}"
    local mail_mode="${MAIL_SERVICE_MODE:-external}"

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

# Configure Keycloak kernel realm SMTP (standalone Job; used by update.sh --mail).
configure_keycloak_realm_smtp() {
    local ns="platform-kernel"
    local job_name="keycloak-smtp-configure"
    local kernel_realm="${KERNEL_REALM:-kernel}"

    if ! kubectl get secret keycloak-admin -n "${ns}" >/dev/null 2>&1; then
        warn "keycloak-admin Secret not present yet — cannot configure realm SMTP."
        return 1
    fi

    if ! _apply_keycloak_smtp_secret "${ns}"; then
        warn "SMTP credentials incomplete — set EXTERNAL_SMTP_* + SMTP_RELAY_*" \
             "(external) or MAIL_SERVICE_MODE=kernel with derived smtp password."
        return 1
    fi

    info "Configuring Keycloak realm SMTP (MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE:-external})..."

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
              KEYCLOAK_BASE=\$(resolve_keycloak_base) || {
                printf '\033[0;31m[ERROR]\033[0m %s\n' "could not resolve Keycloak OIDC base from KEYCLOAK_URL=\${KEYCLOAK_URL}" >&2
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

    if ! kubectl wait "job/${job_name}" -n "${ns}" --for=condition=complete --timeout=120s; then
        error "Keycloak SMTP configure Job failed."
        kubectl logs -n "${ns}" "job/${job_name}" --tail=40 2>/dev/null || true
        return 1
    fi

    kubectl logs -n "${ns}" "job/${job_name}" --tail=5 2>/dev/null || true
    success "Keycloak realm ${kernel_realm} SMTP configured (${MAIL_SERVICE_MODE:-external})."
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

# Keycloak Admin API calls run in-cluster (Job). The keycloak-admin Secret URL is
# an in-cluster Service DNS name and is not reachable from the install host.
run_keycloak_portal_bootstrap_job() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    local username="administrator"
    local email="administrator@${kernel_domain}"
    local password job_name="keycloak-portal-bootstrap"
    local ns="platform-kernel"
    local platform_superadmin_group="gentian:platform:superadmin"

    password=$(_platform_admin_derive_password)
    export PORTAL_LOGIN_USERNAME="${username}"
    export PORTAL_LOGIN_EMAIL="${email}"
    export PORTAL_LOGIN_PASSWORD="${password}"

    info "Bootstrapping Keycloak portal client + user via in-cluster Job..."

    ensure_portal_bff_secret >/dev/null
    local argocd_secret
    argocd_secret=$(ensure_argocd_oidc_secret)

    local llm_support="${LLM_SUPPORT:-false}"
    local litellm_sso_secret=""
    if [[ "${llm_support}" == "true" ]]; then
        litellm_sso_secret=$(ensure_litellm_sso_secret)
    fi

    local -a bootstrap_secret_args=(
        --from-literal=kernel_domain="${kernel_domain}"
        --from-literal=kernel_realm="${kernel_realm}"
        --from-literal=username="${username}"
        --from-literal=email="${email}"
        --from-literal=password="${password}"
        --from-literal=platform_superadmin_group="${platform_superadmin_group}"
        --from-literal=argocd_client_secret="${argocd_secret}"
        --from-literal=llm_support="${llm_support}"
        --from-literal=litellm_sso_client_secret="${litellm_sso_secret}"
    )
    if _keycloak_smtp_settings; then
        bootstrap_secret_args+=(
            --from-literal=smtp_configure=true
            --from-literal=mail_service_mode="${MAIL_SERVICE_MODE:-external}"
            --from-literal=smtp_host="${KC_SMTP_HOST}"
            --from-literal=smtp_port="${KC_SMTP_PORT}"
            --from-literal=smtp_user="${KC_SMTP_USER}"
            --from-literal=smtp_password="${KC_SMTP_PASSWORD}"
            --from-literal=smtp_ssl="${KC_SMTP_SSL}"
            --from-literal=smtp_starttls="${KC_SMTP_STARTTLS}"
            --from-literal=smtp_from="${KC_SMTP_FROM}"
        )
    else
        warn "SMTP credentials incomplete — Keycloak invite/reset emails will not send" \
             "until ./update.sh --mail (set EXTERNAL_SMTP_* or MAIL_SERVICE_MODE=kernel)."
        bootstrap_secret_args+=(
            --from-literal=smtp_configure=false
            --from-literal=mail_service_mode="${MAIL_SERVICE_MODE:-external}"
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
              KEYCLOAK_BASE=\$(resolve_keycloak_base) || {
                printf '\033[0;31m[ERROR]\033[0m %s\n' "could not resolve Keycloak OIDC base from KEYCLOAK_URL=\${KEYCLOAK_URL}" >&2
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
              PORTAL="https://portal.\${KERNEL_DOMAIN}"

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

              CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=gentian-portal" \\
                | jq -r '.[0].id // empty')
              BODY=\$(jq -n --arg portal "\${PORTAL}" '{
                clientId: "gentian-portal",
                name: "Gentian Portal",
                enabled: true,
                publicClient: true,
                standardFlowEnabled: true,
                directAccessGrantsEnabled: false,
                implicitFlowEnabled: false,
                serviceAccountsEnabled: false,
                protocol: "openid-connect",
                redirectUris: [(\$portal + "/login"), (\$portal + "/login/*"), (\$portal + "/*")],
                attributes: {
                  "pkce.code.challenge.method": "S256",
                  "post.logout.redirect.uris": ((\$portal + "/login") + "##" + (\$portal + "/*"))
                },
                webOrigins: [\$portal, "+"],
                rootUrl: \$portal,
                baseUrl: \$portal
              }')
              if [ -n "\${CLIENT_ID}" ]; then
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLIENT_ID}" -d "\${BODY}"
                echo "Updated client gentian-portal"
              else
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients" -d "\${BODY}"
                CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=gentian-portal" \\
                  | jq -r '.[0].id // empty')
                echo "Created client gentian-portal"
              fi

              GROUPS_SCOPE_ID=""
              if [ -n "\${CLIENT_ID}" ]; then
                SCOPE_LIST=\$(curl -sf -H "\${AUTH}" "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/client-scopes")
                GROUPS_SCOPE_ID=\$(printf '%s' "\${SCOPE_LIST}" | jq -r '.[] | select(.name=="groups") | .id' | head -1)

                # Create the scope when the realm has none.
                #
                # Keycloak ships a groups scope in some distributions and not others,
                # and this realm had none — so the attach below found nothing, did
                # nothing, and said nothing. The portal's tokens then carried no groups
                # claim, OpenBao refused every one, and it surfaced three layers away as
                # a 401 from the credential manager.
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
                  # The success line used to print whether or not the PUT worked,
                  # and the PUT swallowed its own failure. The portal then minted
                  # tokens with no groups claim, OpenBao refused every one, and
                  # the log said the scope was attached.
                  if curl -sf -X PUT -H "\${AUTH}" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLIENT_ID}/default-client-scopes/\${GROUPS_SCOPE_ID}" >/dev/null 2>&1; then
                    echo "gentian-portal default scope: groups"
                  else
                    printf '\033[0;31m[ERROR]\033[0m %s\n' "could not attach the groups scope to gentian-portal." >&2
                    echo "  Without it the portal's tokens carry no groups claim, and" >&2
                    echo "  OpenBao refuses them — which reads as a permissions problem." >&2
                    exit 1
                  fi

                  # The audience. OpenBao's roles bind bound_audiences to openbao, and a
                  # Keycloak ACCESS token does not carry the requesting client in aud —
                  # azp names the client, aud gets only what an audience mapper puts there.
                  # Nothing created one, so every exchange failed on the audience even with
                  # the right group, and the refusal was indistinguishable from a
                  # permissions problem.
                  AUD_BODY='{
                    "name": "openbao-audience",
                    "protocol": "openid-connect",
                    "protocolMapper": "oidc-audience-mapper",
                    "config": {
                      "included.client.audience": "openbao",
                      "id.token.claim": "false",
                      "access.token.claim": "true"
                    }
                  }'
                  AUD_ID=\$(curl -sf -H "\${AUTH}" \\
                    "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLIENT_ID}/protocol-mappers/models" \\
                    | jq -r '.[] | select(.name=="openbao-audience") | .id' | head -1)
                  if [ -n "\${AUD_ID}" ] && [ "\${AUD_ID}" != "null" ]; then
                    AUD_HTTP=\$(printf '%s' "\${AUD_BODY}" | jq --arg id "\${AUD_ID}" '. + {id: \$id}' \\
                      | curl -s -o /tmp/aud.err -w '%{http_code}' -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                        "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLIENT_ID}/protocol-mappers/models/\${AUD_ID}" -d @-)
                  else
                    AUD_HTTP=\$(curl -s -o /tmp/aud.err -w '%{http_code}' -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                      "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${CLIENT_ID}/protocol-mappers/models" -d "\${AUD_BODY}")
                  fi
                  case "\${AUD_HTTP}" in
                    2*) echo "gentian-portal audience mapper: aud += openbao" ;;
                    *)
                      printf '\033[0;31m[ERROR]\033[0m %s\n' "could not add the openbao audience mapper (HTTP \${AUD_HTTP})." >&2
                      if [ -s /tmp/aud.err ]; then head -c 300 /tmp/aud.err >&2; echo >&2; fi
                      echo "  Without it the portal's tokens carry no openbao audience and" >&2
                      echo "  OpenBao refuses them — which reads as a permissions problem." >&2
                      exit 1
                      ;;
                  esac

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
              fi

              USER_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users?username=\${PORTAL_USERNAME}&exact=true" \\
                | jq -r '.[0].id // empty')
              USER_BODY=\$(jq -n --arg u "\${PORTAL_USERNAME}" --arg e "\${PORTAL_EMAIL}" '{
                username: \$u, email: \$e, firstName: "Platform", lastName: "Administrator",
                enabled: true, emailVerified: true
              }')
              if [ -z "\${USER_ID}" ]; then
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users" -d "\${USER_BODY}"
                USER_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users?username=\${PORTAL_USERNAME}&exact=true" \\
                  | jq -r '.[0].id')
                echo "Created user \${PORTAL_USERNAME}"
              else
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/\${USER_ID}" -d "\${USER_BODY}"
                echo "Updated user \${PORTAL_USERNAME}"
              fi
              CRED=\$(jq -n --arg p "\${PORTAL_PASSWORD}" '{type:"password",value:\$p,temporary:false}')
              curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/\${USER_ID}/reset-password" -d "\${CRED}"

              SUPERADMIN_GROUP="\${PLATFORM_SUPERADMIN_GROUP}"
              GROUP_LIST=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/groups?search=\${SUPERADMIN_GROUP}&exact=true")
              GROUP_ID=\$(printf '%s' "\${GROUP_LIST}" | jq -r --arg n "\${SUPERADMIN_GROUP}" '.[] | select(.name==\$n) | .id' | head -1)
              if [ -z "\${GROUP_ID}" ] || [ "\${GROUP_ID}" = "null" ]; then
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/groups" \\
                  -d "{\"name\":\"\${SUPERADMIN_GROUP}\"}"
                GROUP_LIST=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/groups?search=\${SUPERADMIN_GROUP}&exact=true")
                GROUP_ID=\$(printf '%s' "\${GROUP_LIST}" | jq -r --arg n "\${SUPERADMIN_GROUP}" '.[] | select(.name==\$n) | .id' | head -1)
                echo "Created group \${SUPERADMIN_GROUP}"
              else
                echo "Group \${SUPERADMIN_GROUP} already exists"
              fi
              curl -sf -X PUT -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/users/\${USER_ID}/groups/\${GROUP_ID}" >/dev/null || true
              echo "User \${PORTAL_USERNAME} joined \${SUPERADMIN_GROUP}"

              BFF_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=gentian-portal-bff" \\
                | jq -r '.[0].id // empty')
              BFF_BODY=\$(jq -n --arg secret "\${PORTAL_BFF_CLIENT_SECRET}" '{
                clientId: "gentian-portal-bff",
                name: "Gentian Portal BFF",
                enabled: true,
                publicClient: false,
                standardFlowEnabled: false,
                directAccessGrantsEnabled: true,
                serviceAccountsEnabled: false,
                protocol: "openid-connect",
                secret: \$secret
              }')
              if [ -n "\${BFF_CLIENT_ID}" ]; then
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${BFF_CLIENT_ID}" -d "\${BFF_BODY}"
                echo "Updated client gentian-portal-bff"
              else
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients" -d "\${BFF_BODY}"
                BFF_CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients?clientId=gentian-portal-bff" \\
                  | jq -r '.[0].id')
                echo "Created client gentian-portal-bff"
              fi
              if [ -n "\${BFF_CLIENT_ID}" ] && [ -n "\${GROUPS_SCOPE_ID}" ] && [ "\${GROUPS_SCOPE_ID}" != "null" ]; then
                curl -sf -X PUT -H "\${AUTH}" \\
                  "\${KEYCLOAK_BASE}/admin/realms/\${REALM}/clients/\${BFF_CLIENT_ID}/default-client-scopes/\${GROUPS_SCOPE_ID}" >/dev/null 2>&1 || true
                echo "gentian-portal-bff default scope: groups"
              fi

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

              echo "Portal bootstrap complete for \${PORTAL_USERNAME}@\${KERNEL_DOMAIN}"
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
            - name: PORTAL_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: password
            - name: PLATFORM_SUPERADMIN_GROUP
              valueFrom:
                secretKeyRef:
                  name: portal-bootstrap-credentials
                  key: platform_superadmin_group
            - name: PORTAL_BFF_CLIENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: gentian-portal-bff
                  key: client_secret
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

    if ! kubectl wait "job/${job_name}" -n "${ns}" --for=condition=complete --timeout=180s; then
        error "Keycloak portal bootstrap Job failed."
        kubectl logs -n "${ns}" "job/${job_name}" --tail=80 2>/dev/null || true
        return 1
    fi

    kubectl logs -n "${ns}" "job/${job_name}" --tail=20 2>/dev/null || true
    success "Keycloak gentian-portal client and platform admin ${username} are ready."
    info "OIDC issuer: https://id.${kernel_domain}/auth/realms/${kernel_realm}"
    info "Portal login credentials:"
    info "  URL:      https://portal.${kernel_domain}/login"
    info "  Username: ${username}  (or ${email})"
    info "  Password: ${password}"
}

build_gentian_portal_images() {
    local ui_dir="${GENTIAN_UI_DIR:-${SCRIPT_DIR}/../gentian-ui}"
    local tag="${PORTAL_IMAGE_TAG:-develop}"
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    local portal_origin="https://portal.${kernel_domain}"
    local issuer="https://id.${kernel_domain}/auth/realms/${kernel_realm}"

    if [[ ! -d "${ui_dir}/frontend" || ! -d "${ui_dir}/backend" ]]; then
        warn "gentian-ui not found at ${ui_dir} — set GENTIAN_UI_DIR or build images manually."
        return 1
    fi

    if ! command -v docker >/dev/null 2>&1; then
        warn "docker not available — skipping portal image build."
        return 1
    fi

    info "Building gentian-portal images (tag=${tag})..."

    docker build -f "${ui_dir}/frontend/Dockerfile" "${ui_dir}/frontend" \
        --build-arg "VITE_OIDC_ISSUER=${issuer}" \
        --build-arg "VITE_OIDC_CLIENT_ID=gentian-portal" \
        --build-arg "VITE_OIDC_REDIRECT_URI=${portal_origin}/login" \
        --build-arg "VITE_OIDC_SCOPES=openid profile email groups" \
        --build-arg "VITE_AUTH_DISABLED=false" \
        -t "ghcr.io/gentian-org/gentian-portal-web:${tag}"

    docker build -f "${ui_dir}/backend/Dockerfile" "${ui_dir}/backend" \
        -t "ghcr.io/gentian-org/gentian-portal-api:${tag}"

    if [[ "${PORTAL_IMAGE_PUSH:-true}" == "true" ]]; then
        if ! docker push "ghcr.io/gentian-org/gentian-portal-web:${tag}" \
            || ! docker push "ghcr.io/gentian-org/gentian-portal-api:${tag}"; then
            warn "Could not push portal images to ghcr.io — ensure docker login ghcr.io"
        fi
    fi

    export PORTAL_WEB_IMAGE="ghcr.io/gentian-org/gentian-portal-web:${tag}"
    export PORTAL_API_IMAGE="ghcr.io/gentian-org/gentian-portal-api:${tag}"
    success "Portal images ready: ${PORTAL_WEB_IMAGE}"
}

# Returns the OpenFGA store id, or empty + non-zero when the authz bridge has
# not created the Secret yet.
#
# Callers MUST invoke this as `$(_openfga_runtime_store_id || true)`. Every one
# of them already handles an empty result — "portal API will start without
# OPENFGA_* until authz bridge syncs" — but these scripts run under `set -e`, so
# an unguarded command substitution aborts the install before that handling is
# ever reached. A missing Secret is the normal state on a fresh cluster.
_openfga_runtime_store_id() {
    if ! kubectl get secret openfga-runtime -n platform-kernel >/dev/null 2>&1; then
        return 1
    fi
    kubectl get secret openfga-runtime -n platform-kernel \
        -o jsonpath='{.data.store_id}' 2>/dev/null | base64 -d 2>/dev/null || true
}

wait_for_openfga_runtime_store_id() {
    local timeout_sec="${1:-180}"
    info "Waiting for openfga-runtime store_id (authz bridge bootstrap, up to ${timeout_sec}s)..."

    kubectl rollout restart deployment/gentian-os -n gentian-system 2>/dev/null || true
    kubectl rollout status deployment/gentian-os -n gentian-system --timeout=180s 2>/dev/null || true

    local deadline=$((SECONDS + timeout_sec))
    while (( SECONDS < deadline )); do
        local store_id
        store_id=$(_openfga_runtime_store_id || true)
        if [[ -n "${store_id}" ]]; then
            success "OpenFGA store_id ready."
            return 0
        fi
        if kubectl logs -n gentian-system deploy/gentian-os --tail=30 2>/dev/null | grep -q "authz bridge sync complete"; then
            store_id=$(_openfga_runtime_store_id || true)
            [[ -n "${store_id}" ]] && return 0
        fi
        sleep 10
    done
    warn "openfga-runtime.store_id not ready within ${timeout_sec}s."
    return 1
}

_openfga_api_token() {
    if ! kubectl get secret openfga-sensitive-values -n platform-kernel >/dev/null 2>&1; then
        return 0
    fi
    kubectl get secret openfga-sensitive-values -n platform-kernel \
        -o jsonpath='{.data.sensitive-values\.yaml}' 2>/dev/null | base64 -d 2>/dev/null \
        | grep -A1 'keys:' | tail -1 | sed 's/.*"\([^"]*\)".*/\1/' || true
}

install_gentian_portal_secrets() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    local ns="platform-kernel"
    local issuer="https://id.${kernel_domain}/auth/realms/${kernel_realm}"

    local store_id
    store_id=$(_openfga_runtime_store_id || true)
    if [[ -z "${store_id}" ]]; then
        info "OpenFGA store_id not ready — portal API will start without OPENFGA_* until authz bridge syncs."
    fi

    secret_args=(
        --from-literal=OIDC_ISSUER="${issuer}"
        --from-literal=OIDC_CLIENT_ID="gentian-portal"
        --from-literal=OIDC_AUDIENCE="gentian-portal"
    )
    if kubectl get secret gentian-portal-bff -n "${ns}" >/dev/null 2>&1; then
        local bff_secret
        bff_secret=$(kubectl get secret gentian-portal-bff -n "${ns}" -o jsonpath='{.data.client_secret}' | base64 -d)
        secret_args+=(
            --from-literal=PORTAL_BFF_CLIENT_ID="gentian-portal-bff"
            --from-literal=PORTAL_BFF_CLIENT_SECRET="${bff_secret}"
        )
    else
        ensure_portal_bff_secret >/dev/null
        local bff_secret
        bff_secret=$(kubectl get secret gentian-portal-bff -n "${ns}" -o jsonpath='{.data.client_secret}' | base64 -d)
        secret_args+=(
            --from-literal=PORTAL_BFF_CLIENT_ID="gentian-portal-bff"
            --from-literal=PORTAL_BFF_CLIENT_SECRET="${bff_secret}"
        )
    fi

    local kc_url kc_user kc_pass
    kc_url=$(kubectl get secret keycloak-admin -n platform-kernel -o jsonpath='{.data.url}' 2>/dev/null | base64 -d || true)
    kc_user=$(kubectl get secret keycloak-admin -n platform-kernel -o jsonpath='{.data.username}' 2>/dev/null | base64 -d || true)
    kc_pass=$(kubectl get secret keycloak-admin -n platform-kernel -o jsonpath='{.data.password}' 2>/dev/null | base64 -d || true)
    if [[ -n "${kc_url}" && -n "${kc_pass}" ]]; then
        secret_args+=(
            --from-literal=KEYCLOAK_ADMIN_URL="${kc_url}"
            --from-literal=KEYCLOAK_ADMIN_USERNAME="${kc_user:-admin}"
            --from-literal=KEYCLOAK_ADMIN_PASSWORD="${kc_pass}"
        )
    fi

    local openfga_token
    openfga_token=$(_openfga_api_token)
    if [[ -n "${openfga_token}" ]]; then
        secret_args+=(--from-literal=OPENFGA_API_TOKEN="${openfga_token}")
    fi
    if [[ -n "${store_id}" ]]; then
        secret_args+=(--from-literal=OPENFGA_STORE_ID="${store_id}")
    fi

    kubectl create secret generic gentian-portal-secrets -n "${ns}" \
        "${secret_args[@]}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

release_gentian_portal_helm_bootstrap() {
    local ns="platform-kernel"
    if kubectl get secret -n "$ns" -l "owner=helm,name=gentian-portal" --no-headers 2>/dev/null | grep -q .; then
        info "Removing bootstrap Helm release metadata (ArgoCD owns gentian-portal now)..."
        kubectl delete secret -n "$ns" -l "owner=helm,name=gentian-portal" --ignore-not-found
    fi
}

apply_gentian_portal_argocd_application() {
    local gentian_ui_branch portal_tag rendered tmpl
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-test}"
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"
    resolve_gentian_os_branch
    gentian_ui_branch="${GENTIAN_UI_BRANCH:-develop}"
    portal_tag="${PORTAL_IMAGE_TAG:-develop}"
    tmpl="${SCRIPT_DIR}/kernel/bootstrap/chart/templates/gentian-portal.yaml"
    rendered="$(mktemp)"

    if [[ ! -f "${tmpl}" ]]; then
        error "gentian-portal Application template not found at ${tmpl}"
        return 1
    fi

    helm template gentian-bootstrap "${SCRIPT_DIR}/kernel/bootstrap/chart" \
        -s templates/gentian-portal.yaml \
        --set-string "gentianOsBranch=${GENTIAN_OS_BRANCH}" \
        --set-string "uiBranch=${gentian_ui_branch}" \
        --set-string "portalImageTag=${portal_tag}" \
        --set-string "deploymentsRepo=${GENTIAN_DEPLOYMENTS_REPO}" \
        --set-string "deploymentsBranch=${GENTIAN_DEPLOYMENTS_BRANCH}" \
        --set-string "cluster=${cluster}" \
        --set-string "stage=${stage}" >"${rendered}"
    info "Registering gentian-portal ArgoCD Application (os=${GENTIAN_OS_BRANCH}, ui=${gentian_ui_branch}, tag=${portal_tag}, cluster=${cluster}, stage=${stage})..."
    kubectl apply -f "${rendered}"
    rm -f "${rendered}"
}

wait_for_gentian_portal_argocd() {
    local timeout_sec="${1:-300}"
    local ns="platform-kernel"
    info "Waiting for gentian-portal ArgoCD Application (up to ${timeout_sec}s)..."
    local elapsed=0
    local interval=10
    while (( elapsed < timeout_sec )); do
        if ! kubectl get application gentian-portal -n argocd >/dev/null 2>&1; then
            sleep 5
            elapsed=$((elapsed + 5))
            continue
        fi
        local health sync
        health=$(kubectl get application gentian-portal -n argocd -o jsonpath='{.status.health.status}' 2>/dev/null || true)
        sync=$(kubectl get application gentian-portal -n argocd -o jsonpath='{.status.sync.status}' 2>/dev/null || true)

        # Gateway API HTTPRoute + ServerSideApply adds default backendRef fields that
        # keep sync=OutOfSync even when the route is Accepted and serving traffic.
        local sync_ok=0
        [[ "${sync}" == "Synced" || "${sync}" == "OutOfSync" ]] && sync_ok=1

        if [[ "${health}" == "Healthy" && "${sync_ok}" -eq 1 ]] \
            && kubectl wait "deployment/gentian-portal-gentian-portal-api" -n "${ns}" \
                --for=condition=Available --timeout=10s >/dev/null 2>&1 \
            && kubectl wait "deployment/gentian-portal-gentian-portal-web" -n "${ns}" \
                --for=condition=Available --timeout=10s >/dev/null 2>&1; then
            if [[ "${sync}" == "OutOfSync" ]]; then
                success "gentian-portal ArgoCD Application is Healthy (HTTPRoute SSA drift ignored)."
            else
                success "gentian-portal ArgoCD Application is Synced and Healthy."
            fi
            return 0
        fi
        printf "  gentian-portal: sync=%s health=%s (%ds/%ds)\n" \
            "${sync:-unknown}" "${health:-unknown}" "${elapsed}" "${timeout_sec}"
        sleep "${interval}"
        elapsed=$((elapsed + interval))
    done
    warn "gentian-portal ArgoCD Application did not become Healthy within ${timeout_sec}s."
    kubectl get application gentian-portal -n argocd \
        -o custom-columns='NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status' 2>/dev/null || true
    kubectl get deploy -n "${ns}" -l app.kubernetes.io/instance=gentian-portal \
        -o custom-columns='NAME:.metadata.name,READY:.status.readyReplicas,AVAILABLE:.status.availableReplicas' 2>/dev/null || true
    return 1
}

refresh_gentian_portal_openfga() {
    local store_id="$1"
    [[ -n "${store_id}" ]] || return 0
    local ns="platform-kernel"
    info "Refreshing portal API with OpenFGA store_id..."
    install_gentian_portal_secrets
    kubectl patch secret gentian-portal-secrets -n "${ns}" --type merge \
        -p "{\"stringData\":{\"OPENFGA_STORE_ID\":\"${store_id}\"}}" 2>/dev/null || true
    kubectl rollout restart deployment/gentian-portal-gentian-portal-api -n "${ns}" 2>/dev/null || true
}

install_gentian_portal_chart() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local portal_origin="https://portal.${kernel_domain}"

    install_gentian_portal_secrets
    apply_gentian_portal_argocd_application
    release_gentian_portal_helm_bootstrap

    success "gentian-portal ArgoCD Application registered at ${portal_origin}/login"
}

install_portal_login() {
    banner "Gentian portal login (OIDC + shell)"

    # Waited for, not tested once. keycloak-admin is written by ESO once
    # Keycloak is up, so at this point in a fresh install it is usually seconds
    # away rather than absent: a single probe failed the step while the Secret
    # appeared 100 seconds later. Anything ESO produces is eventually consistent
    # with the step that needs it.
    local deadline=$(( SECONDS + 180 ))
    until kubectl get secret keycloak-admin -n platform-kernel >/dev/null 2>&1; do
        if (( SECONDS >= deadline )); then
            error "keycloak-admin Secret did not appear within 180s."
            error "  It is written by ESO from OpenBao once Keycloak is running. Check:"
            error "    kubectl get externalsecret -n platform-kernel"
            error "    kubectl get pods -n platform-kernel -l app.kubernetes.io/name=keycloakx"
            return 1
        fi
        sleep 5
    done

    ensure_keycloak_admin_secret_url || return 1
    ensure_portal_gateway_readiness

    # The gentian-portal Client MR is not applied here.
    #
    # It used to be, from manifests/dev/gentian-portal-client.yaml — a path that
    # stopped existing when be4947bc templated these services on stage. The
    # apply was guarded by [[ -f ]], so it has silently done nothing since:
    # a step that reads as delivering something and delivers nothing.
    #
    # The gentian-keycloak-provider ApplicationSet syncs
    # kernel/services/keycloak-config/manifests, so Argo CD owns the MR — which
    # is what the comment here used to call the "optional drift-safe path", and
    # is now the only path.

    run_keycloak_portal_bootstrap_job
    configure_argocd_oidc

    if [[ "${PORTAL_SKIP_IMAGE_BUILD:-true}" != "true" ]]; then
        if build_gentian_portal_images; then
            export PORTAL_IMAGE_PULL_POLICY="${PORTAL_IMAGE_PULL_POLICY:-IfNotPresent}"
        else
            info "Using CI portal images (tag=${PORTAL_IMAGE_TAG:-develop})."
            export PORTAL_IMAGE_PULL_POLICY="${PORTAL_IMAGE_PULL_POLICY:-Always}"
        fi
    else
        info "Using CI portal images (tag=${PORTAL_IMAGE_TAG:-develop})."
        export PORTAL_IMAGE_PULL_POLICY="${PORTAL_IMAGE_PULL_POLICY:-Always}"
    fi

    install_gentian_portal_chart

    if wait_for_openfga_runtime_store_id 180; then
        local store_id
        store_id=$(_openfga_runtime_store_id || true)
        if [[ -n "${store_id}" ]]; then
            refresh_gentian_portal_openfga "${store_id}"
        fi
    else
        warn "Authz bridge did not publish openfga-runtime — ensure operator uses CI image (GENTIAN_OS_IMAGE_TAG=develop)."
    fi

    wait_for_gentian_portal_argocd 300 || warn "gentian-portal ArgoCD Application not Healthy yet — check portal API /readyz and OPENFGA_STORE_ID."

    success "Stage 1 portal login ready."
    info "  https://portal.${KERNEL_DOMAIN}/login"
    info "  user: administrator@${KERNEL_DOMAIN}"
    info "  password: $(_platform_admin_derive_password)"
}

print_portal_login_summary() {
    local kernel_domain="${KERNEL_DOMAIN:-}"
    [[ -n "${kernel_domain}" ]] || return 0
    local password
    password=$(_platform_admin_derive_password 2>/dev/null || echo "")
    [[ -n "${password}" ]] || password="(set MASTER_PASSWORD in install.env)"
    echo ""
    echo -e "${GREEN}  Gentian portal (cluster admin):${NC}"
    echo -e "${GREEN}    URL      : https://portal.${kernel_domain}/login${NC}"
    echo -e "${GREEN}    User     : administrator@${kernel_domain}${NC}"
    echo -e "${GREEN}    Password : ${password}${NC}"
    echo -e "${GREEN}    OIDC     : https://id.${kernel_domain}/auth/realms/${KERNEL_REALM:-kernel}${NC}"
}
