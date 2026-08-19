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
	"github.com/gentian-org/gentian-os/internal/meta"
)

const portalPublicClientID = "gentian-portal"
const portalPublicClientVersion = "2" // 2: openbao audience mapper

func tenantPortalPublicClientJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-portal-public-%s", tenantName)
}

// buildPortalPublicClientScript ensures the public OIDC client used by the portal
// shell and execute-actions-email invites exists in each tenant realm.
func buildPortalPublicClientScript(realmExpr string) string {
	return keycloak.ShellJSONIDExtractor() + fmt.Sprintf(`
set -eu
REALM=%s
CLIENT_ID=%q
PORTAL="${PORTAL_ORIGIN%%/}"

if [ -z "${PORTAL:-}" ]; then
  echo "ERROR: PORTAL_ORIGIN unset" >&2
  exit 1
fi

# The tenant's own entry, https://<realm>.<kernel-domain>, is where sign-out
# returns a tenant member: the apex would ask them for an email again, turning
# every sign-out into a two-stage sign-in. Keycloak validates
# post_logout_redirect_uri against the registered list, so an unregistered host
# does not merely redirect elsewhere — it fails the logout with "Invalid
# redirect uri". This job runs once per tenant realm, so REALM is the tenant.
KERNEL_DOMAIN="${PORTAL#https://portal.}"
TENANT_ORIGIN="https://${REALM}.${KERNEL_DOMAIN}"

TOKEN=$(curl -sf --max-time 30 \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

%s

EXISTING=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}" || echo "[]")
BODY=$(jq -n --arg portal "${PORTAL}" --arg tenant "${TENANT_ORIGIN}" --arg clientId "${CLIENT_ID}" '{
  clientId: $clientId,
  name: "Gentian Portal",
  enabled: true,
  publicClient: true,
  standardFlowEnabled: true,
  directAccessGrantsEnabled: false,
  implicitFlowEnabled: false,
  serviceAccountsEnabled: false,
  protocol: "openid-connect",
  redirectUris: [($portal + "/login"), ($portal + "/login/*"), ($portal + "/*"),
                 ($tenant + "/"), ($tenant + "/*")],
  attributes: {
    "pkce.code.challenge.method": "S256",
    "post.logout.redirect.uris": (($portal + "/login") + "##" + ($portal + "/*")
                                  + "##" + ($tenant + "/") + "##" + ($tenant + "/*"))
  },
  webOrigins: [$portal, $tenant, "+"],
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

# The audience OpenBao's role binds. A Keycloak ACCESS token does not carry the
# requesting client in aud — azp names the client, and aud holds only what an
# audience mapper puts there — so without this the tenant's auth mount refuses
# every exchange on the audience, no matter who the caller is.
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
AUD_ID=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_KC_ID}/protocol-mappers/models" \
  | jq -r '.[] | select(.name=="openbao-audience") | .id' | head -1)
if [ -n "${AUD_ID}" ] && [ "${AUD_ID}" != "null" ]; then
  # Keycloak resolves the target from the body's id, not the path alone.
  AUD_HTTP=$(printf '%%s' "${AUD_BODY}" | jq --arg id "${AUD_ID}" '. + {id: $id}' \
    | curl -s -o /tmp/aud.err -w '%%{http_code}' -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_KC_ID}/protocol-mappers/models/${AUD_ID}" -d @-)
else
  AUD_HTTP=$(curl -s -o /tmp/aud.err -w '%%{http_code}' -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${CLIENT_KC_ID}/protocol-mappers/models" -d "${AUD_BODY}")
fi
case "${AUD_HTTP}" in
  2*) echo "openbao audience mapper set on ${CLIENT_ID} in realm ${REALM}" ;;
  *)
    echo "ERROR: openbao audience mapper failed in realm ${REALM} (HTTP ${AUD_HTTP})" >&2
    [ -s /tmp/aud.err ] && head -c 300 /tmp/aud.err >&2 && echo >&2
    echo "  Tenant admins cannot use the Credentials view until this succeeds." >&2
    exit 1
    ;;
esac
`, realmExpr, portalPublicClientID,
		keycloak.ShellWaitForRealm(realmExpr),
		keycloak.ShellRequireID("CLIENT_KC_ID", "${EXISTING}", "clientId", portalPublicClientID),
		keycloak.ShellRequireID("CLIENT_KC_ID", "${EXISTING}", "clientId", portalPublicClientID))
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
				tenantLabel:    tenantName,
				managedByLabel: managedByValue,
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
	// The script gained the openbao audience mapper, and a completed Job is
	// never re-run — so without this every realm provisioned before that change
	// would keep a client whose tokens the tenant's auth mount refuses.
	if err := r.replaceOutdatedJob(ctx, tenantPortalPublicClientJobName(tenant.Name), tenant.Name,
		"gentianos.io/keycloak-portal-public-client", portalPublicClientVersion); err != nil {
		return false, err
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, tenantPortalPublicClientJobName(tenant.Name))
}
