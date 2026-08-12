/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// kernelTenantBrokerVersion bumps when the kernel→tenant IdP PUT payload changes.
const kernelTenantBrokerVersion = "3"

// kernelPortalBrokerClientID is the OIDC client in each tenant realm used when the
// shared kernel realm brokers login to that tenant.
const kernelPortalBrokerClientID = "broker-kernel-portal"

// kernelPortalFirstBrokerLoginFlowAlias is the kernel-realm first-broker-login flow
// for users imported from tenant IdPs during portal login.
const kernelPortalFirstBrokerLoginFlowAlias = "first-broker-login-kernel-portal"

func kernelExternalURL(kernelDomain string) string {
	return fmt.Sprintf("https://id.%s/auth", kernelDomain)
}

func tenantKernelBrokerJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-kernel-tenant-broker-%s", tenantName)
}

const keycloakShellResolveExternalBase = `
_resolve_external_oidc_base() {
  _ext="${1}"
  _realm="${2}"
  for _base in "${_ext}" "${_ext%/}/auth" "${_ext%/auth}"; do
    if curl -sf --max-time 15 "${_base}/realms/${_realm}/.well-known/openid-configuration" >/dev/null 2>&1; then
      echo "${_base}"
      return 0
    fi
  done
  echo "${_ext}"
}
`

// buildKernelTenantBrokerScript registers the tenant realm as an OIDC Identity
// Provider in the shared kernel realm so portal login can broker tenant users.
func buildKernelTenantBrokerScript() string {
	return keycloak.ShellJSONIDExtractor() + keycloakShellResolveExternalBase + fmt.Sprintf(`
set -eu

if [ -z "${REALM_NAME:-}" ] || [ -z "${KERNEL_REALM:-}" ] || [ -z "${KERNEL_EXTERNAL_URL:-}" ]; then
  echo "kernel tenant broker skipped (REALM_NAME, KERNEL_REALM, or KERNEL_EXTERNAL_URL unset)"
  exit 0
fi

TENANT_REALM="${REALM_NAME}"
BROKER_CLIENT_ID=%q
BROKER_REDIRECT_SUFFIX="/realms/${KERNEL_REALM}/broker/${TENANT_REALM}/endpoint"

TOKEN=$(curl -sf --max-time 30 \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

%s

EXT_BASE=$(_resolve_external_oidc_base "${KERNEL_EXTERNAL_URL}" "${KERNEL_REALM}")
BROKER_REDIRECT="${EXT_BASE}${BROKER_REDIRECT_SUFFIX}"

# 1. Confidential broker client in the tenant realm (kernel IdP callback).
BROKER_RESP=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients?clientId=${BROKER_CLIENT_ID}")
if echo "${BROKER_RESP}" | grep -q "\"clientId\":\"${BROKER_CLIENT_ID}\""; then
  %s
  curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients/${BROKER_KC_ID}" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "{\"clientId\":\"${BROKER_CLIENT_ID}\",\"redirectUris\":[\"${BROKER_REDIRECT}\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":false}" >/dev/null
  echo "broker client ${BROKER_CLIENT_ID} updated in ${TENANT_REALM}"
else
  curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "{\"clientId\":\"${BROKER_CLIENT_ID}\",\"redirectUris\":[\"${BROKER_REDIRECT}\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":false}"
  BROKER_RESP=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients?clientId=${BROKER_CLIENT_ID}")
%s
  echo "broker client ${BROKER_CLIENT_ID} created in ${TENANT_REALM}"
fi
BROKER_SECRET=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients/${BROKER_KC_ID}/client-secret" \
  | sed 's/.*"value":"\([^"]*\)".*/\1/')

# 2. First-broker-login flow in the kernel realm (auto-link by email).
%s

# 3. Register tenant realm as IdP in the kernel realm.
IDP_HTTP=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/identity-provider/instances/${TENANT_REALM}")
IDP_BODY="{\"alias\":\"${TENANT_REALM}\",\"displayName\":\"${TENANT_REALM}\",\"providerId\":\"oidc\",\"enabled\":true,\"trustEmail\":false,\"firstBrokerLoginFlowAlias\":\"%s\",\"config\":{\"issuer\":\"${EXT_BASE}/realms/${TENANT_REALM}\",\"authorizationUrl\":\"${EXT_BASE}/realms/${TENANT_REALM}/protocol/openid-connect/auth\",\"tokenUrl\":\"${KEYCLOAK_URL}/realms/${TENANT_REALM}/protocol/openid-connect/token\",\"jwksUrl\":\"${KEYCLOAK_URL}/realms/${TENANT_REALM}/protocol/openid-connect/certs\",\"userInfoUrl\":\"${KEYCLOAK_URL}/realms/${TENANT_REALM}/protocol/openid-connect/userinfo\",\"clientId\":\"${BROKER_CLIENT_ID}\",\"clientSecret\":\"${BROKER_SECRET}\",\"syncMode\":\"IMPORT\",\"useJwksUrl\":\"true\",\"validateSignature\":\"true\",\"defaultScope\":\"openid profile email groups\",\"hideOnLoginPage\":\"true\"}}"
if [ "${IDP_HTTP}" = "200" ]; then
  HTTP=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" -X PUT \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/identity-provider/instances/${TENANT_REALM}" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "${IDP_BODY}")
else
  HTTP=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" -X POST \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/identity-provider/instances" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "${IDP_BODY}")
fi
if [ "${HTTP}" -ge 200 ] 2>/dev/null && [ "${HTTP}" -lt 300 ] 2>/dev/null; then
  echo "tenant IdP ${TENANT_REALM} registered in kernel realm (HTTP ${HTTP})"
else
  echo "ERROR: tenant IdP ${TENANT_REALM} registration failed (HTTP ${HTTP})" >&2
  exit 1
fi

# 4. Import Gentian group entitlements from the tenant token into kernel users.
IDP_M=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/identity-provider/instances/${TENANT_REALM}/mappers" || echo "[]")
if echo "${IDP_M}" | grep -q '"name":"groups"'; then
  echo "IdP mapper groups already on kernel tenant IdP ${TENANT_REALM}"
else
  curl -sf --max-time 30 -X POST \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/identity-provider/instances/${TENANT_REALM}/mappers" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d '{"name":"groups","identityProviderMapper":"oidc-advanced-group-idp-mapper","identityProviderAlias":"'"${TENANT_REALM}"'","config":{"syncMode":"IMPORT","claims":"[{\"key\":\"groups\",\"value\":\"groups\"}]"}}' \
    >/dev/null 2>&1 || echo "WARN: could not add groups IdP mapper (may require newer Keycloak)"
  echo "IdP mapper groups registered for kernel tenant IdP ${TENANT_REALM}"
fi
`, kernelPortalBrokerClientID,
		keycloak.ShellWaitForRealm("${TENANT_REALM}"),
		brokerResolveIDShell,
		brokerResolveIDShell,
		buildEnsureFirstBrokerLoginFlowShellWithAlias("${KERNEL_REALM}", kernelPortalFirstBrokerLoginFlowAlias),
		kernelPortalFirstBrokerLoginFlowAlias)
}

func makeKernelTenantBrokerJob(tenantName, realmName, kernelRealm, kernelExternalURL string) *batchv1.Job {
	ttl := int32(3600)
	c := keycloakContainer("kernel-tenant-broker", buildKernelTenantBrokerScript())
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "REALM_NAME", Value: realmName},
		corev1.EnvVar{Name: "KERNEL_REALM", Value: kernelRealm},
		corev1.EnvVar{Name: "KERNEL_EXTERNAL_URL", Value: kernelExternalURL},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantKernelBrokerJobName(tenantName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenantName,
				managedByLabel: managedByValue,
				"gentianos.io/keycloak-kernel-tenant-broker": kernelTenantBrokerVersion,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{c},
				},
			},
		},
	}
}

func (r *TenantReconciler) ensureKernelTenantBrokerJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	if r.KernelRealm == "" || r.KernelDomain == "" {
		return true, nil
	}
	brokerDone, err := r.waitForProvisioningJob(ctx, tenant.Name, tenantBrokerIdPJobName(tenant.Name))
	if err != nil || !brokerDone {
		return false, err
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, tenantKernelBrokerJobName(tenant.Name))
}
