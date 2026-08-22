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
const kernelTenantBrokerVersion = "4"

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

if [ -z "${REALM_NAME:-}" ] || [ -z "${KERNEL_REALM:-}" ] || [ -z "${KERNEL_EXTERNAL_URL:-}" ] || [ -z "${TENANT_NAME:-}" ]; then
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

# The tenant-realm broker client is NOT written here. tenant-default composes it,
# observed, and republishes its secret so the IdP below can take credentials from
# a Secret rather than from a read like the one this used to do.

# 2. First-broker-login flow in the kernel realm (auto-link by email).
%s

# 3. The tenant IdP in the kernel realm is NOT written here either — that is
# tenant-default's, declared with every field including the ones the provider
# cannot read back. Its presence is still required for the mapper below, so it is
# checked rather than created.
IDP_HTTP=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/identity-provider/instances/${TENANT_REALM}")
if [ "${IDP_HTTP}" != "200" ]; then
  echo "ERROR: tenant IdP ${TENANT_REALM} not in kernel realm (HTTP ${IDP_HTTP}); the Composition has not created it yet" >&2
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

# 5. Stamp the tenant name onto every user brokered from this realm.
#
# The groups mapper above tells the kernel realm WHAT a user may do; this tells
# it WHICH TENANT they are doing it for. OpenBao maps the claim into alias
# metadata, its tenant-admin policy templates a path from it, and the credential
# manager filters the catalogue by it — so all three agree because all three read
# one value.
#
# Hardcoded rather than derived from a claim: the tenant is a property of the
# identity provider the user came through, not something their token asserts. A
# user cannot arrive through this IdP and belong to another tenant.
#
# Applied to every brokered user, not only admins. It grants nothing on its own —
# the groups mapper decides that — and a member without an admin group matches no
# OpenBao role at all.
if echo "${IDP_M}" | grep -q '"name":"tenant"'; then
  echo "IdP mapper tenant already on kernel tenant IdP ${TENANT_REALM}"
else
  curl -sf --max-time 30 -X POST \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/identity-provider/instances/${TENANT_REALM}/mappers" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d '{"name":"tenant","identityProviderMapper":"hardcoded-attribute-idp-mapper","identityProviderAlias":"'"${TENANT_REALM}"'","config":{"syncMode":"FORCE","attribute":"tenant","attribute.value":"'"${TENANT_NAME}"'"}}' \
    >/dev/null 2>&1 || echo "WARN: could not add tenant IdP mapper"
  echo "IdP mapper tenant=${TENANT_NAME} registered for kernel tenant IdP ${TENANT_REALM}"
fi
`, kernelPortalBrokerClientID,
		keycloak.ShellWaitForRealm("${TENANT_REALM}"),
		buildEnsureFirstBrokerLoginFlowShellWithAlias("${KERNEL_REALM}", kernelPortalFirstBrokerLoginFlowAlias))
}

func makeKernelTenantBrokerJob(tenantName, realmName, kernelRealm, kernelExternalURL string) *batchv1.Job {
	ttl := int32(3600)
	c := keycloakContainer("kernel-tenant-broker", buildKernelTenantBrokerScript())
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "REALM_NAME", Value: realmName},
		// The tenant NAME, not its realm. They differ when a Tenant overrides
		// spec.isolation.keycloakRealm, and the OpenBao paths are keyed by name.
		corev1.EnvVar{Name: "TENANT_NAME", Value: tenantName},
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
