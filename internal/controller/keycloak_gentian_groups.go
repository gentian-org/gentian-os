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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	backoff := meta.ProvisioningJobBackoffLimit
	container := keycloakContainer("provision-gentian-groups", buildGentianGroupsScript(realmName))
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "GENTIAN_GROUPS_JSON", Value: groupsJSON},
		// One protocol mapper per attribute the installed profiles actually declare.
		corev1.EnvVar{Name: "GENTIAN_GROUP_ATTR_NAMES", Value: strings.Join(groupAttributeNames(groupsJSON), " ")},
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
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
}

// groupAttributeNames returns the distinct group-attribute names present in the
// groups JSON, sorted.
//
// Each one needs a protocol mapper or the attribute never reaches the token. The
// names come from profiles' gentianos.io/keycloak-group-attributes annotations,
// so the platform does not need to know that Odoo uses gentianOdooGroupRoles —
// a second app declaring its own attribute gets a mapper without a code change.
//
// A malformed value yields no names rather than an error: the groups themselves
// are provisioned from the same JSON by the script, and failing the whole realm
// job over a mapper would be a worse outcome than a missing claim.
func groupAttributeNames(groupsJSON string) []string {
	var groups []struct {
		Attributes map[string]any `json:"attributes"`
	}
	if err := json.Unmarshal([]byte(groupsJSON), &groups); err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var names []string
	for _, g := range groups {
		for name := range g.Attributes {
			// Keycloak config keys are interpolated into JSON below; refuse anything
			// that is not a plain identifier rather than escaping it.
			if !validAttrName(name) {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func validAttrName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// buildGentianGroupsScript creates Gentian entitlement groups in a tenant realm and
// ensures the built-in "groups" client scope is available for JWT group claims.
func buildGentianGroupsScript(realmName string) string {
	return keycloak.ShellJSONIDExtractor() + fmt.Sprintf(`set -eu
REALM=%q
`+keycloak.ShellAdminToken()+`
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
    -d '{"name":"groups","protocol":"openid-connect","protocolMapper":"oidc-group-membership-mapper","consentRequired":false,"config":{"full.path":"false","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true","claim.name":"groups","introspection.token.claim":"true","multivalued":"true"}}'
  curl -sf -X PUT -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/default-default-client-scopes/${GROUPS_SCOPE_ID}"
  echo "groups client scope created and configured"
fi

echo "groups client scope present (id=${GROUPS_SCOPE_ID})"
MAPPERS=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${GROUPS_SCOPE_ID}/protocol-mappers/models" || echo "[]")
# One aggregating mapper per declared group attribute. The names come from the
# profiles' keycloak-group-attributes annotations, so an app introducing a new
# attribute needs no change here.
#
# introspection.token.claim is named although Keycloak would default it, because
# this PUT replaces the config wholesale. Leaving it out strips a key Keycloak
# immediately writes back, and the Composition managing the same mapper then sees
# drift and updates to match its own spec — each writer undoing the other on
# every pass, at the cost of an admin password grant per attempt. Both sides name
# it now, so both agree with what Keycloak stores.
for ATTR_NAME in ${GENTIAN_GROUP_ATTR_NAMES:-}; do
  # Body without the surrounding braces, so the PUT can prepend an id without
  # having to splice a string that is already JSON.
  MAPPER_BODY="\"name\":\"${ATTR_NAME}\",\"protocol\":\"openid-connect\",\"protocolMapper\":\"oidc-usermodel-attribute-mapper\",\"consentRequired\":false,\"config\":{\"user.attribute\":\"${ATTR_NAME}\",\"claim.name\":\"${ATTR_NAME}\",\"jsonType.label\":\"String\",\"multivalued\":\"true\",\"aggregate.attrs\":\"true\",\"id.token.claim\":\"true\",\"access.token.claim\":\"true\",\"userinfo.token.claim\":\"true\",\"introspection.token.claim\":\"true\"}"
  keycloak_json_id_by_attr "${MAPPERS}" "name" "${ATTR_NAME}"
  MAPPER_ID="${_kj_id}"
  if [ -n "${MAPPER_ID}" ]; then
    curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${GROUPS_SCOPE_ID}/protocol-mappers/models/${MAPPER_ID}" \
      -d "{\"id\":\"${MAPPER_ID}\",${MAPPER_BODY}}"
    echo "mapper ${ATTR_NAME} updated with aggregation"
  else
    curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${GROUPS_SCOPE_ID}/protocol-mappers/models" \
      -d "{${MAPPER_BODY}}"
    echo "mapper ${ATTR_NAME} added to groups client scope"
  fi
done
`, realmName, keycloak.ShellWaitForRealm("${REALM}"))
}
