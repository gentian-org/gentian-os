// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const portalBFFClientID = "gentian-portal-bff"
const portalBFFClientVersion = "2"

func tenantPortalBFFClientJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-portal-bff-%s", tenantName)
}

// buildPortalBFFClientScript ensures a confidential ROPC client exists for the
// Gentian portal BFF (password login without browser redirect to Keycloak).
func buildPortalBFFClientScript(realmExpr string) string {
	return keycloakShellJSONIDExtractor() + fmt.Sprintf(`
set -eu
REALM=%s
CLIENT_ID=%q

if [ -z "${PORTAL_BFF_CLIENT_SECRET:-}" ]; then
  echo "ERROR: PORTAL_BFF_CLIENT_SECRET unset" >&2
  exit 1
fi

TOKEN=$(curl -sf --max-time 30 \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

%s

EXISTING=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}" || echo "[]")
BODY="{\"clientId\":\"${CLIENT_ID}\",\"name\":\"Gentian Portal BFF\",\"protocol\":\"openid-connect\",\"publicClient\":false,\"standardFlowEnabled\":false,\"directAccessGrantsEnabled\":true,\"serviceAccountsEnabled\":false,\"fullScopeAllowed\":true,\"secret\":\"${PORTAL_BFF_CLIENT_SECRET}\"}"
if echo "${EXISTING}" | grep -q "\"clientId\":\"${CLIENT_ID}\""; then
  %s
  curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_KC_ID}" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "${BODY}" >/dev/null
  echo "portal BFF client updated in realm ${REALM}"
else
  curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/admin/realms/${REALM}/clients" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "${BODY}"
  EXISTING=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}" || echo "[]")
  %s
  echo "portal BFF client created in realm ${REALM}"
fi

SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes" || echo "[]")
GROUPS_SCOPE_ID=$(printf '%%s' "${SCOPE_LIST}" | jq -r '.[] | select(.name=="groups") | .id' | head -1)
if [ -n "${GROUPS_SCOPE_ID}" ] && [ "${GROUPS_SCOPE_ID}" != "null" ]; then
  curl -sf --max-time 30 -X PUT \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_KC_ID}/default-client-scopes/${GROUPS_SCOPE_ID}" \
    -H "${AUTH_HEADER}" >/dev/null 2>&1 || true
  echo "groups scope attached to ${CLIENT_ID} in realm ${REALM}"
fi
`, realmExpr, portalBFFClientID,
		keycloakShellWaitForRealm(realmExpr),
		keycloakShellRequireID("CLIENT_KC_ID", "${EXISTING}", "clientId", portalBFFClientID),
		keycloakShellRequireID("CLIENT_KC_ID", "${EXISTING}", "clientId", portalBFFClientID))
}

func makePortalBFFClientJob(tenantName, realmName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	c := keycloakContainer("portal-bff-client", buildPortalBFFClientScript(fmt.Sprintf("%q", realmName)))
	c.Env = append(c.Env,
		corev1.EnvVar{
			Name: "PORTAL_BFF_CLIENT_SECRET",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "gentian-portal-bff"},
					Key:                  "client_secret",
				},
			},
		},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantPortalBFFClientJobName(tenantName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:                               tenantName,
				managedByLabel:                            managedByValue,
				"gentianos.io/keycloak-portal-bff-client": portalBFFClientVersion,
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

func (r *TenantReconciler) ensurePortalBFFClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	if r.KernelRealm == "" || r.KernelDomain == "" {
		return true, nil
	}
	realmDone, err := r.waitForProvisioningJob(ctx, tenant.Name, realmJobName(tenant.Name))
	if err != nil || !realmDone {
		return false, err
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, tenantPortalBFFClientJobName(tenant.Name))
}
