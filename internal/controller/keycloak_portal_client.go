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

const portalPublicClientID = "gentian-portal"
const portalPublicClientVersion = "1"

func tenantPortalPublicClientJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-portal-public-%s", tenantName)
}

// buildPortalPublicClientScript ensures the public OIDC client used by the portal
// shell and execute-actions-email invites exists in each tenant realm.
func buildPortalPublicClientScript(realmExpr string) string {
	return keycloakShellJSONIDExtractor() + fmt.Sprintf(`
set -eu
REALM=%s
CLIENT_ID=%q
PORTAL="${PORTAL_ORIGIN%/}"

if [ -z "${PORTAL:-}" ]; then
  echo "ERROR: PORTAL_ORIGIN unset" >&2
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
BODY=$(jq -n --arg portal "${PORTAL}" '{
  clientId: "%s",
  name: "Gentian Portal",
  enabled: true,
  publicClient: true,
  standardFlowEnabled: true,
  directAccessGrantsEnabled: false,
  implicitFlowEnabled: false,
  serviceAccountsEnabled: false,
  protocol: "openid-connect",
  redirectUris: [($portal + "/login"), ($portal + "/login/*"), ($portal + "/*")],
  attributes: {
    "pkce.code.challenge.method": "S256",
    "post.logout.redirect.uris": (($portal + "/login") + "##" + ($portal + "/*"))
  },
  webOrigins: [$portal, "+"],
  rootUrl: $portal,
  baseUrl: "/"
}')
if echo "${EXISTING}" | grep -q "\"clientId\":\"${CLIENT_ID}\""; then
  %s
  curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_KC_ID}" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "${BODY}" >/dev/null
  echo "portal public client updated in realm ${REALM}"
else
  curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/admin/realms/${REALM}/clients" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "${BODY}"
  EXISTING=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}" || echo "[]")
  %s
  echo "portal public client created in realm ${REALM}"
fi

SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes" || echo "[]")
GROUPS_SCOPE_ID=$(printf '%%s' "${SCOPE_LIST}" | jq -r '.[] | select(.name=="groups") | .id' | head -1)
if [ -n "${GROUPS_SCOPE_ID}" ] && [ "${GROUPS_SCOPE_ID}" != "null" ]; then
  curl -sf --max-time 30 -X PUT \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_KC_ID}/default-client-scopes/${GROUPS_SCOPE_ID}" \
    -H "${AUTH_HEADER}" >/dev/null 2>&1 || true
  echo "groups scope attached to ${CLIENT_ID} in realm ${REALM}"
fi
`, realmExpr, portalPublicClientID,
		keycloakShellWaitForRealm(realmExpr),
		keycloakShellRequireID("CLIENT_KC_ID", "${EXISTING}", "clientId", portalPublicClientID),
		keycloakShellRequireID("CLIENT_KC_ID", "${EXISTING}", "clientId", portalPublicClientID))
}

func makePortalPublicClientJob(tenantName, realmName, portalOrigin string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	c := keycloakContainer("portal-public-client", buildPortalPublicClientScript(fmt.Sprintf("%q", realmName)))
	c.Env = append(c.Env, corev1.EnvVar{Name: "PORTAL_ORIGIN", Value: portalOrigin})
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantPortalPublicClientJobName(tenantName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:                                  tenantName,
				managedByLabel:                               managedByValue,
				"gentianos.io/keycloak-portal-public-client": portalPublicClientVersion,
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

func (r *TenantReconciler) ensurePortalPublicClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	if r.KernelRealm == "" || r.KernelDomain == "" {
		return true, nil
	}
	realmDone, err := r.waitForProvisioningJob(ctx, tenant.Name, realmJobName(tenant.Name))
	if err != nil || !realmDone {
		return false, err
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, tenantPortalPublicClientJobName(tenant.Name))
}
