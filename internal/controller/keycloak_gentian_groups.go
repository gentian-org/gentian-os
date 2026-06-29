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

func (r *TenantReconciler) ensureGentianGroupsJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, gentianGroupsJobName(tenant.Name))
}

func makeGentianGroupsJob(tenant *gentianov1alpha1.Tenant, realmName string, groupNames []string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	backoff := meta.ProvisioningJobBackoffLimit
	container := keycloakContainer("provision-gentian-groups", buildGentianGroupsScript(realmName))
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "GENTIAN_GROUP_NAMES", Value: shellWordList(groupNames)},
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
	return keycloakShellJSONIDExtractor() + fmt.Sprintf(`set -eu
REALM=%q
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"
%s

for GROUP_NAME in ${GENTIAN_GROUP_NAMES}; do
  GROUP_LIST=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/groups?max=1000")
  keycloak_json_id_by_attr "${GROUP_LIST}" "name" "${GROUP_NAME}"
  if [ -n "${_kj_id}" ]; then
    echo "group ${GROUP_NAME} already exists"
    continue
  fi
  curl -sf -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/groups" \
    -d "{\"name\":\"${GROUP_NAME}\"}"
  echo "group ${GROUP_NAME} created"
done

SCOPE_LIST=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")
keycloak_json_id_by_attr "${SCOPE_LIST}" "name" "groups"
GROUPS_SCOPE_ID="${_kj_id}"
if [ -z "${GROUPS_SCOPE_ID}" ]; then
  echo "WARNING: groups client scope missing in realm ${REALM}" >&2
else
  echo "groups client scope present (id=${GROUPS_SCOPE_ID})"
fi
`, realmName, keycloakShellWaitForRealm("${REALM}"))
}
