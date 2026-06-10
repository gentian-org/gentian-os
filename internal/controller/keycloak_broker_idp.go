// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// brokerIdentityProviderVersion bumps when the kernel IdP PUT payload changes so
// completed jobs are recreated on operator upgrade.
const brokerIdentityProviderVersion = "5"

// firstBrokerLoginFlowAlias is a tenant-realm authentication flow that auto-links
// kernel IdP logins to pre-provisioned LDAP users by email (no confirm/re-auth).
const firstBrokerLoginFlowAlias = "first-broker-login-gentian"

// brokerFirstLoginFlowJobVersion bumps when the auto-link flow script changes.
const brokerFirstLoginFlowJobVersion = "2"

func tenantBrokerIdPJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-broker-idp-%s", tenantName)
}

// buildBrokerIdentityProviderScript updates the kernel OIDC Identity Provider in
// a tenant realm. Server-side endpoints (token, JWKS, userinfo) use KEYCLOAK_URL
// so Keycloak does not hairpin through the public id.<kernel> URL during broker
// code exchange; browser-facing issuer and authorizationUrl stay external.
func buildBrokerIdentityProviderScript() string {
	script := keycloakShellJSONIDExtractor() + `
set -eu

if [ -z "${REALM_NAME:-}" ] || [ -z "${KERNEL_REALM:-}" ] || [ -z "${KERNEL_EXTERNAL_URL:-}" ]; then
  echo "broker IdP update skipped (REALM_NAME, KERNEL_REALM, or KERNEL_EXTERNAL_URL unset)"
  exit 0
fi

BROKER_CLIENT_ID="broker-${REALM_NAME}"

TOKEN=$(curl -sf --max-time 30 \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')

BROKER_RESP=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients?clientId=${BROKER_CLIENT_ID}")
` + brokerResolveIDShell + `
BROKER_SECRET=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients/${BROKER_KC_ID}/client-secret" \
  | sed 's/.*"value":"\([^"]*\)".*/\1/')

IDP_HTTP=$(curl -s --max-time 30 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel")
if [ "${IDP_HTTP}" != "200" ]; then
  echo "ERROR: kernel IdP not found in realm ${REALM_NAME} (HTTP ${IDP_HTTP})" >&2
  exit 1
fi

IDP_BODY="{\"alias\":\"kernel\",\"displayName\":\"Gentian SSO\",\"providerId\":\"oidc\",\"enabled\":true,\"trustEmail\":true,\"firstBrokerLoginFlowAlias\":\"first broker login\",\"config\":{\"issuer\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}\",\"authorizationUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/auth\",\"tokenUrl\":\"${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/token\",\"jwksUrl\":\"${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/certs\",\"userInfoUrl\":\"${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/userinfo\",\"clientId\":\"${BROKER_CLIENT_ID}\",\"clientSecret\":\"${BROKER_SECRET}\",\"syncMode\":\"IMPORT\",\"useJwksUrl\":\"true\",\"validateSignature\":\"true\",\"defaultScope\":\"openid profile email\",\"hideOnLoginPage\":\"true\"}}"

HTTP=$(curl -s --max-time 30 -o /dev/null -w "%{http_code}" -X PUT \
  "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel" \
  -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
  -d "${IDP_BODY}")
if [ "${HTTP}" -ge 200 ] 2>/dev/null && [ "${HTTP}" -lt 300 ] 2>/dev/null; then
  echo "kernel IdP updated in realm ${REALM_NAME} (HTTP ${HTTP})"
else
  echo "ERROR: kernel IdP update for realm ${REALM_NAME} failed (HTTP ${HTTP})" >&2
  exit 1
fi
` + ensureLDAPUIDAttributeMapperShell + `
ensure_ldap_uid_attribute_mapper "${KERNEL_REALM}" "ldap-provider"
` + brokerKernelClientUsernameMapperShell + brokerIdPUsernameImporterShell + `
`
	return strings.Replace(script, `\"firstBrokerLoginFlowAlias\":\"first broker login\"`,
		fmt.Sprintf(`\"firstBrokerLoginFlowAlias\":%q`, firstBrokerLoginFlowAlias), 1)
}

const brokerKernelClientUsernameMapperShell = `
# Kernel broker client must emit opendesk_username (LDAP uid) in tokens issued
# during tenant→kernel broker login; otherwise tenant scope mappers see empty uid.
BROKER_M=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients/${BROKER_KC_ID}/protocol-mappers/models" || echo "[]")
if echo "${BROKER_M}" | grep -q '"name":"opendesk_username"'; then
  echo "broker client opendesk_username mapper already on ${BROKER_CLIENT_ID}"
else
  curl -sf --max-time 30 -X POST \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients/${BROKER_KC_ID}/protocol-mappers/models" \
    -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
    -d '{"name":"opendesk_username","protocol":"openid-connect","protocolMapper":"oidc-usermodel-attribute-mapper","config":{"user.attribute":"uid","claim.name":"opendesk_username","jsonType.label":"String","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true","introspection.token.claim":"true","multivalued":"false"}}'
  echo "broker client opendesk_username mapper added on ${BROKER_CLIENT_ID}"
fi
`

const brokerIdPUsernameImporterShell = `
# Import opendesk_username from the kernel IdP into the tenant user's uid attribute
# so opendesk-matrix-scope mappers can emit the claim for Synapse localpart mapping.
IDP_M=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel/mappers" || echo "[]")
if echo "${IDP_M}" | grep -q '"name":"opendesk_username"'; then
  echo "IdP mapper opendesk_username already in realm ${REALM_NAME}"
else
  curl -sf --max-time 30 -X POST \
    "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel/mappers" \
    -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
    -d '{"name":"opendesk_username","identityProviderMapper":"oidc-user-attribute-idp-mapper","identityProviderAlias":"kernel","config":{"syncMode":"IMPORT","claim":"opendesk_username","user.attribute":"uid"}}'
  echo "IdP mapper opendesk_username registered in realm ${REALM_NAME}"
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

func makeBrokerIdentityProviderJob(tenantName, realmName, kernelRealm, kernelExternalURL string) *batchv1.Job {
	ttl := int32(3600)
	c := keycloakContainer("broker-idp", buildBrokerIdentityProviderScript())
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "REALM_NAME", Value: realmName},
		corev1.EnvVar{Name: "KERNEL_REALM", Value: kernelRealm},
		corev1.EnvVar{Name: "KERNEL_EXTERNAL_URL", Value: kernelExternalURL},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantBrokerIdPJobName(tenantName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:                          tenantName,
				managedByLabel:                       managedByValue,
				"gentianos.io/keycloak-broker-idp":   brokerIdentityProviderVersion,
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

func brokerIdPJobCurrent(job *batchv1.Job) bool {
	return job != nil && job.Labels["gentianos.io/keycloak-broker-idp"] == brokerIdentityProviderVersion
}

func (r *TenantReconciler) ensureBrokerIdentityProviderJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) error {
	if r.KernelRealm == "" || r.KernelDomain == "" {
		return nil
	}
	kernelExternalURL := fmt.Sprintf("https://id.%s", r.KernelDomain)
	jobName := tenantBrokerIdPJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return r.Create(ctx, makeBrokerIdentityProviderJob(tenant.Name, realmName, r.KernelRealm, kernelExternalURL))
	}
	if err != nil {
		return fmt.Errorf("get broker IdP job %s: %w", jobName, err)
	}
	if !brokerIdPJobCurrent(job) {
		prop := metav1.DeletePropagationBackground
		if delErr := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); delErr != nil && !errors.IsNotFound(delErr) {
			return fmt.Errorf("delete stale broker IdP job %s: %w", jobName, delErr)
		}
		return r.Create(ctx, makeBrokerIdentityProviderJob(tenant.Name, realmName, r.KernelRealm, kernelExternalURL))
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return r.Create(ctx, makeBrokerIdentityProviderJob(tenant.Name, realmName, r.KernelRealm, kernelExternalURL))
	}
	if !jobIsComplete(job) {
		log.FromContext(ctx).Info("waiting for Keycloak broker IdP job", "job", jobName, "realm", realmName)
	}
	return nil
}

func (r *KeycloakPlatformReconciler) ensureAllBrokerIdentityProviderJobs(ctx context.Context) (bool, error) {
	if r.KernelRealm == "" || r.KernelDomain == "" {
		return true, nil
	}
	tr := &TenantReconciler{
		Client:       r.Client,
		KernelRealm:  r.KernelRealm,
		KernelDomain: r.KernelDomain,
	}
	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		return false, fmt.Errorf("list tenants for broker IdP jobs: %w", err)
	}
	for i := range tenantList.Items {
		tenant := &tenantList.Items[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		realm := keycloakRealmName(tenant)
		if err := tr.ensureBrokerIdentityProviderJob(ctx, tenant, realm); err != nil {
			return false, err
		}
		job := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: tenantBrokerIdPJobName(tenant.Name), Namespace: kernelNamespace}, job); err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if !jobIsComplete(job) {
			return false, nil
		}
	}
	return true, nil
}
