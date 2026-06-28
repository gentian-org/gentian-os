#!/bin/bash
# shellcheck disable=SC2034
# Portal login bootstrap — Keycloak OIDC client, kernel user, gentian-ui Helm release.
# Sourced from install.sh Step 16 (Stage 1 login dogfood).

set -euo pipefail

_portal_derive_password() {
    echo -n "portal-bootstrap:user_password" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" | awk '{print $2}'
}

_portal_kc_admin_token() {
    local url user pass
    url=$(kubectl get secret keycloak-admin -n platform-kernel -o jsonpath='{.data.url}' | base64 -d)
    user=$(kubectl get secret keycloak-admin -n platform-kernel -o jsonpath='{.data.username}' | base64 -d)
    pass=$(kubectl get secret keycloak-admin -n platform-kernel -o jsonpath='{.data.password}' | base64 -d)
    curl -sf \
        -X POST "${url}/realms/master/protocol/openid-connect/token" \
        -H "Content-Type: application/x-www-form-urlencoded" \
        --data-urlencode "client_id=admin-cli" \
        --data-urlencode "username=${user}" \
        --data-urlencode "password=${pass}" \
        --data-urlencode "grant_type=password" \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])"
}

ensure_gentian_portal_oidc_client() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    local portal_origin="https://portal.${kernel_domain}"
    local id_origin="https://id.${kernel_domain}"

    info "Ensuring Keycloak OIDC client gentian-portal in realm ${kernel_realm}..."

    local token
    token=$(_portal_kc_admin_token)

    local kc_url
    kc_url=$(kubectl get secret keycloak-admin -n platform-kernel -o jsonpath='{.data.url}' | base64 -d)

    local clients_json
    clients_json=$(curl -sf -H "Authorization: Bearer ${token}" \
        "${kc_url}/admin/realms/${kernel_realm}/clients?clientId=gentian-portal")

    local client_uuid
    client_uuid=$(printf '%s' "${clients_json}" | python3 -c "
import sys, json
items = json.load(sys.stdin)
print(items[0]['id'] if items else '')
")

    local body
    body=$(python3 -c "
import json
portal = '${portal_origin}'
print(json.dumps({
    'clientId': 'gentian-portal',
    'name': 'Gentian Portal',
    'enabled': True,
    'publicClient': True,
    'standardFlowEnabled': True,
    'directAccessGrantsEnabled': False,
    'implicitFlowEnabled': False,
    'serviceAccountsEnabled': False,
    'protocol': 'openid-connect',
    'redirectUris': [
        portal + '/login',
        portal + '/login/*',
        portal + '/*',
    ],
    'webOrigins': [portal, '+'],
    'attributes': {
        'pkce.code.challenge.method': 'S256',
    },
    'rootUrl': portal,
    'baseUrl': portal,
}))
")

    if [[ -n "${client_uuid}" ]]; then
        curl -sf -X PUT -H "Authorization: Bearer ${token}" \
            -H "Content-Type: application/json" \
            "${kc_url}/admin/realms/${kernel_realm}/clients/${client_uuid}" \
            -d "${body}" >/dev/null
        success "Updated Keycloak client gentian-portal."
    else
        curl -sf -X POST -H "Authorization: Bearer ${token}" \
            -H "Content-Type: application/json" \
            "${kc_url}/admin/realms/${kernel_realm}/clients" \
            -d "${body}" >/dev/null
        success "Created Keycloak client gentian-portal."
    fi

    info "OIDC issuer for portal: ${id_origin}/realms/${kernel_realm}"
}

bootstrap_kernel_portal_user() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    local username="${PORTAL_BOOTSTRAP_USERNAME:-demo}"
    local email="demo@${kernel_domain}"
    local password
    password=$(_portal_derive_password)

    info "Ensuring kernel realm user ${username} (${email})..."

    local token kc_url
    token=$(_portal_kc_admin_token)
    kc_url=$(kubectl get secret keycloak-admin -n platform-kernel -o jsonpath='{.data.url}' | base64 -d)

    local users_json
    users_json=$(curl -sf -H "Authorization: Bearer ${token}" \
        "${kc_url}/admin/realms/${kernel_realm}/users?username=${username}&exact=true" || echo "[]")

    local user_uuid
    user_uuid=$(printf '%s' "${users_json}" | python3 -c "
import sys, json
items = json.load(sys.stdin)
print(items[0]['id'] if items else '')
")

    local user_body
    user_body=$(python3 -c "
import json
print(json.dumps({
    'username': ${username@Q},
    'email': ${email@Q},
    'enabled': True,
    'emailVerified': True,
}))
")

    if [[ -z "${user_uuid}" ]]; then
        curl -sf -X POST -H "Authorization: Bearer ${token}" \
            -H "Content-Type: application/json" \
            "${kc_url}/admin/realms/${kernel_realm}/users" \
            -d "${user_body}" >/dev/null
        users_json=$(curl -sf -H "Authorization: Bearer ${token}" \
            "${kc_url}/admin/realms/${kernel_realm}/users?username=${username}&exact=true")
        user_uuid=$(printf '%s' "${users_json}" | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['id'])")
        success "Created kernel user ${username}."
    else
        curl -sf -X PUT -H "Authorization: Bearer ${token}" \
            -H "Content-Type: application/json" \
            "${kc_url}/admin/realms/${kernel_realm}/users/${user_uuid}" \
            -d "${user_body}" >/dev/null
        success "Updated kernel user ${username}."
    fi

    local cred_body
    cred_body=$(python3 -c "import json; print(json.dumps({'type':'password','value':${password@Q},'temporary':False}))")
    curl -sf -X PUT -H "Authorization: Bearer ${token}" \
        -H "Content-Type: application/json" \
        "${kc_url}/admin/realms/${kernel_realm}/users/${user_uuid}/reset-password" \
        -d "${cred_body}" >/dev/null

    export PORTAL_LOGIN_USERNAME="${username}"
    export PORTAL_LOGIN_EMAIL="${email}"
    export PORTAL_LOGIN_PASSWORD="${password}"

    info "Portal login credentials:"
    info "  URL:      https://portal.${kernel_domain}/login"
    info "  Username: ${username}  (or ${email})"
    info "  Password: ${password}"
}

build_gentian_portal_images() {
    local ui_dir="${GENTIAN_UI_DIR:-${SCRIPT_DIR}/../gentian-ui}"
    local tag="${PORTAL_IMAGE_TAG:-feat-new-security}"
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    local portal_origin="https://portal.${kernel_domain}"
    local issuer="https://id.${kernel_domain}/realms/${kernel_realm}"

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
        --build-arg "VITE_OIDC_SCOPES=openid profile email" \
        --build-arg "VITE_AUTH_DISABLED=false" \
        -t "ghcr.io/gentian-org/gentian-portal-web:${tag}"

    docker build -f "${ui_dir}/backend/Dockerfile" "${ui_dir}/backend" \
        -t "ghcr.io/gentian-org/gentian-portal-api:${tag}"

    if [[ "${PORTAL_IMAGE_PUSH:-true}" == "true" ]]; then
        docker push "ghcr.io/gentian-org/gentian-portal-web:${tag}" \
            && docker push "ghcr.io/gentian-org/gentian-portal-api:${tag}" \
            || warn "Could not push portal images to ghcr.io — ensure docker login ghcr.io"
    fi

    export PORTAL_WEB_IMAGE="ghcr.io/gentian-org/gentian-portal-web:${tag}"
    export PORTAL_API_IMAGE="ghcr.io/gentian-org/gentian-portal-api:${tag}"
    success "Portal images ready: ${PORTAL_WEB_IMAGE}"
}

install_gentian_portal_chart() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    local ui_dir="${GENTIAN_UI_DIR:-${SCRIPT_DIR}/../gentian-ui}"
    local chart_dir="${ui_dir}/chart"
    local ns="platform-kernel"
    local tag="${PORTAL_IMAGE_TAG:-feat-new-security}"
    local web_image="${PORTAL_WEB_IMAGE:-ghcr.io/gentian-org/gentian-portal-web:${tag}}"
    local api_image="${PORTAL_API_IMAGE:-ghcr.io/gentian-org/gentian-portal-api:${tag}}"
    local values_file="${SCRIPT_DIR}/kernel/services/gentian-portal-web/values/dev.yaml"
    local issuer="https://id.${kernel_domain}/realms/${kernel_realm}"
    local portal_origin="https://portal.${kernel_domain}"

    if [[ ! -f "${chart_dir}/Chart.yaml" ]]; then
        error "gentian-ui chart not found at ${chart_dir}"
        return 1
    fi

    local store_id=""
    if kubectl get secret openfga-runtime -n platform-kernel >/dev/null 2>&1; then
        store_id=$(kubectl get secret openfga-runtime -n platform-kernel -o jsonpath='{.data.store_id}' | base64 -d 2>/dev/null || true)
    fi
    if [[ -z "${store_id}" ]]; then
        warn "openfga-runtime.store_id missing — PEP checks may fail until authz bridge syncs."
    fi

    kubectl create secret generic gentian-portal-secrets -n "${ns}" \
        --from-literal=OIDC_ISSUER="${issuer}" \
        --from-literal=OIDC_CLIENT_ID="gentian-portal" \
        --from-literal=OIDC_AUDIENCE="gentian-portal" \
        --dry-run=client -o yaml | kubectl apply -f -

    info "Installing gentian-portal Helm release..."
    helm upgrade --install gentian-portal "${chart_dir}" \
        --namespace "${ns}" \
        --values "${values_file}" \
        --set kernelDomain="${kernel_domain}" \
        --set "api.image.repository=ghcr.io/gentian-org/gentian-portal-api" \
        --set "api.image.tag=${tag}" \
        --set "web.image.repository=ghcr.io/gentian-org/gentian-portal-web" \
        --set "web.image.tag=${tag}" \
        --set "api.env.ENVIRONMENT=development" \
        --set "api.env.BACKEND_CORS_ORIGINS=${portal_origin}" \
        --set "openfga.apiUrl=http://gentian-openfga.platform-kernel.svc.cluster.local:8080" \
        --set "openfga.storeId=${store_id}" \
        --set "auth.disabled=false" \
        --wait --timeout 5m

    success "gentian-portal installed at ${portal_origin}/login"
}

install_stage1_portal() {
    banner "Step 16 — Gentian portal login (OIDC + shell)"

    if ! kubectl get secret keycloak-admin -n platform-kernel >/dev/null 2>&1; then
        warn "keycloak-admin Secret missing — run Steps 14–15 first."
        return 1
    fi

    local kc_manifest="${SCRIPT_DIR}/kernel/services/keycloak-config/manifests/dev/gentian-portal-client.yaml"
    if [[ -f "${kc_manifest}" ]]; then
        info "Applying Crossplane gentian-portal Client MR (optional drift-safe path)..."
        kubectl apply -f "${kc_manifest}" 2>/dev/null || warn "Could not apply ${kc_manifest} (provider-keycloak may not be ready)."
    fi

    ensure_gentian_portal_oidc_client
    bootstrap_kernel_portal_user

    if [[ "${PORTAL_SKIP_IMAGE_BUILD:-false}" != "true" ]]; then
        build_gentian_portal_images || info "Using pre-built portal images (tag=${PORTAL_IMAGE_TAG:-feat-new-security})."
    fi

    install_gentian_portal_chart

    info "Restarting authz bridge to sync portal user into OpenFGA..."
    kubectl rollout restart deployment/gentian-os -n gentian-system 2>/dev/null || true
    kubectl rollout status deployment/gentian-os -n gentian-system --timeout=180s 2>/dev/null || true

    info "Waiting for authz bridge sync (up to 3m)..."
    local deadline=$((SECONDS + 180))
    while (( SECONDS < deadline )); do
        if kubectl logs -n gentian-system deploy/gentian-os --tail=20 2>/dev/null | grep -q "authz bridge sync complete"; then
            break
        fi
        sleep 15
    done

    success "Stage 1 portal login ready."
    info "  https://portal.${KERNEL_DOMAIN}/login"
    info "  user: ${PORTAL_LOGIN_USERNAME:-demo}@${KERNEL_DOMAIN}  password: (OpenBao identity/portal-bootstrap-user or MASTER_PASSWORD-derived)"
}
