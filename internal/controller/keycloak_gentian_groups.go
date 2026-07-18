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

func (r *TenantReconciler) ensureGentianGroupsJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, gentianGroupsJobName(tenant.Name))
}

func makeGentianGroupsJob(tenant *gentianov1alpha1.Tenant, realmName string, groupsJSON string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	backoff := meta.ProvisioningJobBackoffLimit
	container := keycloakContainer("provision-gentian-groups", buildGentianGroupsScript(realmName))
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "GENTIAN_GROUPS_JSON", Value: groupsJSON},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gentianGroupsJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
}

// buildGentianGroupsScript creates Gentian entitlement groups in a tenant realm and
// ensures the built-in "groups" client scope is available for JWT group claims.
func buildGentianGroupsScript(realmName string) string {
	return keycloak.ShellJSONIDExtractor() + fmt.Sprintf(`set -eu
REALM=%q
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"
%s

echo "${GENTIAN_GROUPS_JSON}" | jq -c '.[]' | while read -r group; do
  GROUP_NAME=$(echo "${group}" | jq -r '.name')
  GROUP_ATTRS=$(echo "${group}" | jq -c '.attributes')

  GROUP_LIST=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/groups?max=1000")
  keycloak_json_id_by_attr "${GROUP_LIST}" "name" "${GROUP_NAME}"
  
  if [ -n "${_kj_id}" ]; then
    echo "group ${GROUP_NAME} already exists (id=${_kj_id}), updating attributes"
    curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/groups/${_kj_id}" \
      -d "{\"name\":\"${GROUP_NAME}\",\"attributes\":${GROUP_ATTRS}}"
    continue
  fi

  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/groups" \
    -d "{\"name\":\"${GROUP_NAME}\",\"attributes\":${GROUP_ATTRS}}"
  echo "group ${GROUP_NAME} created"
done

SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")
keycloak_json_id_by_attr "${SCOPE_LIST}" "name" "groups"
GROUPS_SCOPE_ID="${_kj_id}"
if [ -z "${GROUPS_SCOPE_ID}" ]; then
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes" \
    -d "{\"name\":\"groups\",\"protocol\":\"openid-connect\",\"attributes\":{\"include.in.token.scope\":\"true\",\"display.on.consent.screen\":\"true\"}}"
  SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")
  keycloak_json_id_by_attr "${SCOPE_LIST}" "name" "groups"
  GROUPS_SCOPE_ID="${_kj_id}"
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${GROUPS_SCOPE_ID}/protocol-mappers/models" \
    -d '{"name":"groups","protocol":"openid-connect","protocolMapper":"oidc-group-membership-mapper","consentRequired":false,"config":{"full.path":"false","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true","claim.name":"groups"}}'
  curl -sf -X PUT -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/default-default-client-scopes/${GROUPS_SCOPE_ID}"
  echo "groups client scope created and configured"
fi

echo "groups client scope present (id=${GROUPS_SCOPE_ID})"
MAPPERS=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${GROUPS_SCOPE_ID}/protocol-mappers/models" || echo "[]")
keycloak_json_id_by_attr "${MAPPERS}" "name" "gentianOdooGroupRoles"
MAPPER_ID="${_kj_id}"
if [ -n "${MAPPER_ID}" ]; then
  curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${GROUPS_SCOPE_ID}/protocol-mappers/models/${MAPPER_ID}" \
    -d '{"id":"'"${MAPPER_ID}"'","name":"gentianOdooGroupRoles","protocol":"openid-connect","protocolMapper":"oidc-usermodel-attribute-mapper","consentRequired":false,"config":{"user.attribute":"gentianOdooGroupRoles","claim.name":"gentianOdooGroupRoles","jsonType.label":"String","multivalued":"true","aggregate.attrs":"true","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true"}}'
  echo "mapper gentianOdooGroupRoles updated with aggregation"
else
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${GROUPS_SCOPE_ID}/protocol-mappers/models" \
    -d '{"name":"gentianOdooGroupRoles","protocol":"openid-connect","protocolMapper":"oidc-usermodel-attribute-mapper","consentRequired":false,"config":{"user.attribute":"gentianOdooGroupRoles","claim.name":"gentianOdooGroupRoles","jsonType.label":"String","multivalued":"true","aggregate.attrs":"true","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true"}}'
  echo "mapper gentianOdooGroupRoles added to groups client scope"
fi
`, realmName, keycloak.ShellWaitForRealm("${REALM}"))
}
