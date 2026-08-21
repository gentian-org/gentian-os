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

	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/meta"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// brokerIdentityProviderVersion bumps when the script changes so completed jobs
// are recreated on operator upgrade. 9 dropped the kernel IdP write, which
// tenant-default's IdentityProvider now owns.
const brokerIdentityProviderVersion = "9"

// firstBrokerLoginFlowAlias is a tenant-realm authentication flow that auto-links
// kernel IdP logins to pre-provisioned users by email (no confirm/re-auth).
const firstBrokerLoginFlowAlias = "first-broker-login-gentian"

// brokerFirstLoginFlowJobVersion bumps when the auto-link flow script changes.
const brokerFirstLoginFlowJobVersion = "3"

func tenantBrokerIdPJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-broker-idp-%s", tenantName)
}

// buildBrokerIdentityProviderScript prepares the two things the kernel IdP needs
// and cannot supply for itself: the tenant-realm authentication flow that its
// firstBrokerLoginFlowAlias names, and the two gentian_username mappers.
//
// It no longer writes the IdP. tenant-default's IdentityProvider owns that
// object, and this Job writing it too is what let this script and the realm
// script disagree about which first-broker-login flow a tenant realm used —
// the realm script setting the built-in flow, this one setting the gentian
// flow, whichever ran last deciding how the next first-time login behaved.
//
// The flow is still created here, and must be: the alias is a reference, so
// Keycloak rejects an IdP naming a flow that does not exist. On a new tenant
// the Composition simply retries until this Job has made it.
func buildBrokerIdentityProviderScript() string {
	return keycloak.ShellJSONIDExtractor() + fmt.Sprintf(`
set -eu

if [ -z "${REALM_NAME:-}" ] || [ -z "${KERNEL_REALM:-}" ]; then
  echo "broker flow and mappers skipped (REALM_NAME or KERNEL_REALM unset)"
  exit 0
fi

BROKER_CLIENT_ID="broker-${REALM_NAME}"

TOKEN=$(curl -sf --max-time 30 \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')

# The broker client's Keycloak id, for the protocol mapper below. Its SECRET is
# no longer read: it was only ever needed to put back into the IdP body, and the
# Composition takes it from the broker Client's connection secret rather than
# from anything curling for it.
BROKER_RESP=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients?clientId=${BROKER_CLIENT_ID}")
%s

# The first-broker-login flow is NOT built here either. tenant-default composes
# it, and this Job building it too made a third writer of one object.

# Read, not written. The IdP belongs to the Composition, but the mapper below
# hangs off it, so its absence is worth saying plainly rather than failing on a
# POST to a path that does not exist.
IDP_HTTP=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel")
if [ "${IDP_HTTP}" != "200" ]; then
  echo "ERROR: kernel IdP not found in realm ${REALM_NAME} (HTTP ${IDP_HTTP}) — the Composition has not created it yet" >&2
  exit 1
fi
%s%s
`, brokerResolveIDShell,
		brokerKernelClientUsernameMapperShell, brokerIdPUsernameImporterShell)
}

const brokerKernelClientUsernameMapperShell = `
# Kernel broker client must emit gentian_username in tokens issued
# during tenant→kernel broker login; otherwise tenant scope mappers see empty uid.
BROKER_M=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients/${BROKER_KC_ID}/protocol-mappers/models" || echo "[]")
if echo "${BROKER_M}" | grep -q '"name":"gentian_username"'; then
  echo "broker client gentian_username mapper already on ${BROKER_CLIENT_ID}"
else
  curl -sf --max-time 30 -X POST \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients/${BROKER_KC_ID}/protocol-mappers/models" \
    -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
    -d '{"name":"gentian_username","protocol":"openid-connect","protocolMapper":"oidc-usermodel-attribute-mapper","config":{"user.attribute":"uid","claim.name":"gentian_username","jsonType.label":"String","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true","introspection.token.claim":"true","multivalued":"false"}}'
  echo "broker client gentian_username mapper added on ${BROKER_CLIENT_ID}"
fi
`

const brokerIdPUsernameImporterShell = `
# Import gentian_username from the kernel IdP into the tenant user's uid attribute
# so gentian-matrix-scope mappers can emit the claim for Synapse localpart mapping.
IDP_M=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel/mappers" || echo "[]")
if echo "${IDP_M}" | grep -q '"name":"gentian_username"'; then
  echo "IdP mapper gentian_username already in realm ${REALM_NAME}"
else
  curl -sf --max-time 30 -X POST \
    "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel/mappers" \
    -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
    -d '{"name":"gentian_username","identityProviderMapper":"oidc-user-attribute-idp-mapper","identityProviderAlias":"kernel","config":{"syncMode":"IMPORT","claim":"gentian_username","user.attribute":"uid"}}'
  echo "IdP mapper gentian_username registered in realm ${REALM_NAME}"
fi
`

const brokerResolveIDShell = `
keycloak_json_id_by_attr "${BROKER_RESP}" "clientId" "${BROKER_CLIENT_ID}"
BROKER_KC_ID="${_kj_id}"
if [ -z "${BROKER_KC_ID}" ]; then
  echo "ERROR: could not resolve broker client id (clientId=${BROKER_CLIENT_ID})" >&2
  exit 1
fi
`

// No kernelExternalURL parameter: it was only ever interpolated into the IdP
// body's browser-facing endpoints, and the Composition builds those from the
// kernel domain now.
func makeBrokerIdentityProviderJob(tenantName, realmName, kernelRealm string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	c := keycloakContainer("broker-idp", buildBrokerIdentityProviderScript())
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "REALM_NAME", Value: realmName},
		corev1.EnvVar{Name: "KERNEL_REALM", Value: kernelRealm},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantBrokerIdPJobName(tenantName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:                        tenantName,
				managedByLabel:                     managedByValue,
				"gentianos.io/keycloak-broker-idp": brokerIdentityProviderVersion,
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

// ensureBrokerIdentityProviderJob waits for the Crossplane-owned broker IdP refresh Job.
func (r *TenantReconciler) ensureBrokerIdentityProviderJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	if r.KernelRealm == "" || r.KernelDomain == "" {
		return true, nil
	}
	realmDone, err := r.waitForProvisioningJob(ctx, tenant.Name, realmJobName(tenant.Name))
	if err != nil || !realmDone {
		return false, err
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, tenantBrokerIdPJobName(tenant.Name))
}
