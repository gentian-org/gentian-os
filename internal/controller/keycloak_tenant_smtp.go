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
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const keycloakSMTPCredentialsSecret = "keycloak-smtp-credentials"
const tenantSMTPVersion = "2"

func tenantSMTPJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-tenant-smtp-%s", tenantName)
}

// buildTenantSMTPConfigureScript copies cluster Keycloak SMTP settings into a tenant realm.
func buildTenantSMTPConfigureScript(realmExpr string) string {
	return fmt.Sprintf(`
set -eu
REALM=%s

if [ "${SMTP_CONFIGURE:-}" != "true" ]; then
  echo "ERROR: SMTP_CONFIGURE is not true (cluster keycloak-smtp-credentials missing or incomplete)" >&2
  exit 1
fi

TOKEN=$(curl -sf --max-time 30 \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

%s

REALM_JSON=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}")
SMTP_JSON=$(jq -n \
  --arg host "${SMTP_HOST}" \
  --arg port "${SMTP_PORT}" \
  --arg from "${SMTP_FROM}" \
  --arg user "${SMTP_USER}" \
  --arg pass "${SMTP_PASSWORD}" \
  --arg ssl "${SMTP_SSL}" \
  --arg starttls "${SMTP_STARTTLS}" \
  '{
    host: $host,
    port: $port,
    from: $from,
    fromDisplayName: "Gentian",
    auth: "true",
    user: $user,
    password: $pass,
    ssl: $ssl,
    starttls: $starttls
  }')
UPDATED=$(printf '%%s' "${REALM_JSON}" | jq --argjson smtp "${SMTP_JSON}" '.smtpServer = $smtp')
curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/${REALM}" \
  -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  -d "${UPDATED}" >/dev/null
echo "tenant realm SMTP configured for ${REALM} (${SMTP_HOST}:${SMTP_PORT})"
`, realmExpr, keycloak.ShellWaitForRealm(realmExpr))
}

func makeTenantSMTPJob(tenantName, realmName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	c := keycloakContainer("tenant-smtp", buildTenantSMTPConfigureScript(fmt.Sprintf("%q", realmName)))
	c.Env = append(c.Env,
		corev1.EnvVar{
			Name: "SMTP_CONFIGURE",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakSMTPCredentialsSecret},
					Key:                  "smtp_configure",
				},
			},
		},
		corev1.EnvVar{
			Name: "SMTP_HOST",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakSMTPCredentialsSecret},
					Key:                  "smtp_host",
				},
			},
		},
		corev1.EnvVar{
			Name: "SMTP_PORT",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakSMTPCredentialsSecret},
					Key:                  "smtp_port",
				},
			},
		},
		corev1.EnvVar{
			Name: "SMTP_USER",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakSMTPCredentialsSecret},
					Key:                  "smtp_user",
				},
			},
		},
		corev1.EnvVar{
			Name: "SMTP_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakSMTPCredentialsSecret},
					Key:                  "smtp_password",
				},
			},
		},
		corev1.EnvVar{
			Name: "SMTP_SSL",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakSMTPCredentialsSecret},
					Key:                  "smtp_ssl",
				},
			},
		},
		corev1.EnvVar{
			Name: "SMTP_STARTTLS",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakSMTPCredentialsSecret},
					Key:                  "smtp_starttls",
				},
			},
		},
		corev1.EnvVar{
			Name: "SMTP_FROM",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakSMTPCredentialsSecret},
					Key:                  "smtp_from",
				},
			},
		},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantSMTPJobName(tenantName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:                        tenantName,
				managedByLabel:                     managedByValue,
				"gentianos.io/keycloak-tenant-smtp": tenantSMTPVersion,
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

func (r *TenantReconciler) clusterKeycloakSMTPCredentialsAvailable(ctx context.Context) bool {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: keycloakSMTPCredentialsSecret, Namespace: kernelNamespace}, secret)
	return err == nil
}

func (r *TenantReconciler) ensureTenantSMTPJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	if r.KernelRealm == "" || r.KernelDomain == "" {
		return true, nil
	}
	if !r.clusterKeycloakSMTPCredentialsAvailable(ctx) {
		return true, nil
	}
	realmDone, err := r.waitForProvisioningJob(ctx, tenant.Name, realmJobName(tenant.Name))
	if err != nil || !realmDone {
		return false, err
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, tenantSMTPJobName(tenant.Name))
}
