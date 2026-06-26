// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"github.com/gentian-org/gentian-os/internal/meta"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	dovecotTenantOIDCClientVersion = "1"
	dovecotOIDCClientID            = "opendesk-dovecot"
)

func tenantDovecotOIDCClientJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-dovecot-oidc-%s", tenantName)
}

// buildDovecotTenantOIDCClientScript ensures opendesk-dovecot exists in the tenant
// Keycloak realm. Dovecot introspects IMAP XOAUTH2 tokens in the realm that issued
// them; kernel-only clients cannot validate tenant-realm tokens from OX App Suite.
func buildDovecotTenantOIDCClientScript() string {
	return keycloakShellJSONIDExtractor() + `
set -eu

if [ -z "${REALM_NAME:-}" ] || [ -z "${DOVECOT_CLIENT_SECRET:-}" ]; then
  echo "dovecot tenant OIDC client skipped (REALM_NAME or DOVECOT_CLIENT_SECRET unset)"
  exit 0
fi

TOKEN=$(curl -sf --max-time 30 \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

EXISTING=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/clients?clientId=opendesk-dovecot" || echo "[]")
keycloak_json_id_by_attr "${EXISTING}" "clientId" "opendesk-dovecot"
CLIENT_UUID="${_kj_id}"
if [ -z "${CLIENT_UUID}" ]; then
  curl -sf --max-time 30 -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/clients" \
    -d '{"clientId":"opendesk-dovecot","protocol":"openid-connect","publicClient":false,"standardFlowEnabled":false,"directAccessGrantsEnabled":false,"serviceAccountsEnabled":false,"fullScopeAllowed":false,"redirectUris":[],"webOrigins":[]}'
  EXISTING=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/clients?clientId=opendesk-dovecot")
  keycloak_json_id_by_attr "${EXISTING}" "clientId" "opendesk-dovecot"
  CLIENT_UUID="${_kj_id}"
  echo "client opendesk-dovecot created in realm ${REALM_NAME}"
else
  echo "client opendesk-dovecot already exists in realm ${REALM_NAME}"
fi

if [ -z "${CLIENT_UUID}" ]; then
  echo "ERROR: could not resolve opendesk-dovecot client id in realm ${REALM_NAME}" >&2
  exit 1
fi

curl -sf --max-time 30 -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/clients/${CLIENT_UUID}" \
  -d "{\"clientId\":\"opendesk-dovecot\",\"protocol\":\"openid-connect\",\"publicClient\":false,\"standardFlowEnabled\":false,\"directAccessGrantsEnabled\":false,\"serviceAccountsEnabled\":false,\"fullScopeAllowed\":false,\"redirectUris\":[],\"webOrigins\":[],\"secret\":\"${DOVECOT_CLIENT_SECRET}\"}"

SECRET_HTTP=$(curl -s --max-time 30 -o /dev/null -w "%{http_code}" -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/clients/${CLIENT_UUID}/client-secret")
if [ "${SECRET_HTTP}" = "404" ]; then
  curl -sf --max-time 30 -X POST -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/clients/${CLIENT_UUID}/client-secret" >/dev/null
fi

echo "client opendesk-dovecot configured in realm ${REALM_NAME}"
`
}

func makeDovecotTenantOIDCClientJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	c := keycloakContainer("dovecot-oidc-client", buildDovecotTenantOIDCClientScript())
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "REALM_NAME", Value: realmName},
		corev1.EnvVar{
			Name: "DOVECOT_CLIENT_SECRET",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "dovecot-admin"},
					Key:                  "oidc_client_secret",
				},
			},
		},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantDovecotOIDCClientJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				"gentianos.io/keycloak-dovecot-oidc-client": dovecotTenantOIDCClientVersion,
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

func (r *TenantReconciler) ensureDovecotTenantOIDCClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	realmName := keycloakRealmName(tenant)
	jobName := tenantDovecotOIDCClientJobName(tenant.Name)
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, makeDovecotTenantOIDCClientJob(tenant, realmName)); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, jobName)
}
