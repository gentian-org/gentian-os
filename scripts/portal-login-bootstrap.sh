#!/bin/bash
# shellcheck disable=SC2034
# Portal login bootstrap — Keycloak OIDC client, kernel user, gentian-ui Helm release.
# Sourced from install.sh Step 16 (Stage 1 login dogfood).

set -euo pipefail

_portal_derive_password() {
    echo -n "portal-bootstrap:user_password" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" | awk '{print $2}'
}

_keycloak_internal_service_url() {
    local ns="${1:-platform-kernel}"
    local svc port
    svc=$(kubectl get svc -n "${ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
        | grep -E 'keycloak.*http' | head -1 || true)
    if [[ -z "${svc}" ]]; then
        svc=$(kubectl get svc -n "${ns}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
            | grep keycloak | grep -v headless | head -1 || true)
    fi
    [[ -n "${svc}" ]] || return 1
    port=$(kubectl get svc -n "${ns}" "${svc}" -o jsonpath='{.spec.ports[?(@.port==8080)].port}' 2>/dev/null || true)
    port="${port:-8080}"
    echo "http://${svc}.${ns}.svc.cluster.local:${port}"
}

ensure_keycloak_admin_secret_url() {
    local ns="platform-kernel"
    local url current
    url=$(_keycloak_internal_service_url "${ns}") || {
        error "No Keycloak HTTP service found in ${ns}."
        return 1
    }
    current=$(kubectl get secret keycloak-admin -n "${ns}" -o jsonpath='{.data.url}' 2>/dev/null | base64 -d || true)
    if [[ "${current}" == "${url}" ]]; then
        info "keycloak-admin URL: ${url}"
        return 0
    fi
    warn "Updating keycloak-admin URL (${current:-missing} -> ${url})"
    kubectl patch secret keycloak-admin -n "${ns}" --type merge \
        -p "{\"stringData\":{\"url\":\"${url}\"}}"
    success "keycloak-admin Secret URL corrected."
}

# Keycloak Admin API calls run in-cluster (Job). The keycloak-admin Secret URL is
# an in-cluster Service DNS name and is not reachable from the install host.
run_keycloak_portal_bootstrap_job() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    local kernel_realm="${KERNEL_REALM:-kernel}"
    local username="${PORTAL_BOOTSTRAP_USERNAME:-demo}"
    local email="demo@${kernel_domain}"
    local password job_name="keycloak-portal-bootstrap"
    local ns="platform-kernel"

    password=$(_portal_derive_password)
    export PORTAL_LOGIN_USERNAME="${username}"
    export PORTAL_LOGIN_EMAIL="${email}"
    export PORTAL_LOGIN_PASSWORD="${password}"

    info "Bootstrapping Keycloak portal client + user via in-cluster Job..."

    kubectl create secret generic portal-bootstrap-credentials -n "${ns}" \
        --from-literal=kernel_domain="${kernel_domain}" \
        --from-literal=kernel_realm="${kernel_realm}" \
        --from-literal=username="${username}" \
        --from-literal=email="${email}" \
        --from-literal=password="${password}" \
        --dry-run=client -o yaml | kubectl apply -f -

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
              TOKEN=\$(curl -sf -X POST "\${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \\
                -H "Content-Type: application/x-www-form-urlencoded" \\
                --data-urlencode "client_id=admin-cli" \\
                --data-urlencode "username=\${KEYCLOAK_ADMIN_USERNAME}" \\
                --data-urlencode "password=\${KEYCLOAK_ADMIN_PASSWORD}" \\
                --data-urlencode "grant_type=password" | jq -r .access_token)
              if [ -z "\${TOKEN}" ] || [ "\${TOKEN}" = "null" ]; then
                echo "ERROR: Keycloak admin token request failed" >&2
                exit 1
              fi
              AUTH="Authorization: Bearer \${TOKEN}"
              REALM="\${KERNEL_REALM}"
              PORTAL="https://portal.\${KERNEL_DOMAIN}"

              realm_http=\$(curl -s -o /dev/null -w '%{http_code}' -H "\${AUTH}" "\${KEYCLOAK_URL}/admin/realms/\${REALM}")
              if [ "\${realm_http}" = "404" ]; then
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_URL}/admin/realms" \\
                  -d "{\"realm\":\"\${REALM}\",\"enabled\":true,\"displayName\":\"\${REALM}\"}"
                echo "Created realm \${REALM}"
              elif [ "\${realm_http}" = "200" ]; then
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_URL}/admin/realms/\${REALM}" -d '{"enabled":true}'
                echo "Realm \${REALM} enabled"
              else
                echo "ERROR: realm \${REALM} check returned HTTP \${realm_http}" >&2
                exit 1
              fi

              CLIENT_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_URL}/admin/realms/\${REALM}/clients?clientId=gentian-portal" \\
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
                webOrigins: [\$portal, "+"],
                attributes: {"pkce.code.challenge.method": "S256"},
                rootUrl: \$portal,
                baseUrl: \$portal
              }')
              if [ -n "\${CLIENT_ID}" ]; then
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_URL}/admin/realms/\${REALM}/clients/\${CLIENT_ID}" -d "\${BODY}"
                echo "Updated client gentian-portal"
              else
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_URL}/admin/realms/\${REALM}/clients" -d "\${BODY}"
                echo "Created client gentian-portal"
              fi

              USER_ID=\$(curl -sf -H "\${AUTH}" \\
                "\${KEYCLOAK_URL}/admin/realms/\${REALM}/users?username=\${PORTAL_USERNAME}&exact=true" \\
                | jq -r '.[0].id // empty')
              USER_BODY=\$(jq -n --arg u "\${PORTAL_USERNAME}" --arg e "\${PORTAL_EMAIL}" '{
                username: \$u, email: \$e, enabled: true, emailVerified: true
              }')
              if [ -z "\${USER_ID}" ]; then
                curl -sf -X POST -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_URL}/admin/realms/\${REALM}/users" -d "\${USER_BODY}"
                USER_ID=\$(curl -sf -H "\${AUTH}" \\
                  "\${KEYCLOAK_URL}/admin/realms/\${REALM}/users?username=\${PORTAL_USERNAME}&exact=true" \\
                  | jq -r '.[0].id')
                echo "Created user \${PORTAL_USERNAME}"
              else
                curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                  "\${KEYCLOAK_URL}/admin/realms/\${REALM}/users/\${USER_ID}" -d "\${USER_BODY}"
                echo "Updated user \${PORTAL_USERNAME}"
              fi
              CRED=\$(jq -n --arg p "\${PORTAL_PASSWORD}" '{type:"password",value:\$p,temporary:false}')
              curl -sf -X PUT -H "\${AUTH}" -H "Content-Type: application/json" \\
                "\${KEYCLOAK_URL}/admin/realms/\${REALM}/users/\${USER_ID}/reset-password" -d "\${CRED}"
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
    success "Keycloak gentian-portal client and kernel user ${username} are ready."
    info "OIDC issuer: https://id.${kernel_domain}/realms/${kernel_realm}"
    info "Portal login credentials:"
    info "  URL:      https://portal.${kernel_domain}/login"
    info "  Username: ${username}  (or ${email})"
    info "  Password: ${password}"
}

build_gentian_portal_images() {
    local ui_dir="${GENTIAN_UI_DIR:-${SCRIPT_DIR}/../gentian-ui}"
    local tag="${PORTAL_IMAGE_TAG:-feat-new-ui}"
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
    local tag="${PORTAL_IMAGE_TAG:-feat-new-ui}"
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

    ensure_keycloak_admin_secret_url || return 1

    local kc_manifest="${SCRIPT_DIR}/kernel/services/keycloak-config/manifests/dev/gentian-portal-client.yaml"
    if [[ -f "${kc_manifest}" ]]; then
        info "Applying Crossplane gentian-portal Client MR (optional drift-safe path)..."
        kubectl apply -f "${kc_manifest}" 2>/dev/null || warn "Could not apply ${kc_manifest} (provider-keycloak may not be ready)."
    fi

    run_keycloak_portal_bootstrap_job

    if [[ "${PORTAL_SKIP_IMAGE_BUILD:-false}" != "true" ]]; then
        build_gentian_portal_images || info "Using pre-built portal images (tag=${PORTAL_IMAGE_TAG:-feat-new-ui})."
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
