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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const keycloakSMTPCredentialsSecret = "keycloak-smtp-credentials"

// Bumped whenever buildTenantSMTPConfigureScript changes in a way that must
// reach realms already configured; replaceOutdatedTenantSMTPJob acts on it.
const tenantSMTPVersion = "3"

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

`+keycloak.ShellAdminToken()+`
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

%s

REALM_JSON=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}")
# The kernel Postfix accepts relaying from inside the cluster by mynetworks and
# advertises no AUTH mechanism, so asking Keycloak to authenticate against it
# fails outright. Only the external relay expects credentials.
if [ "${MAIL_SERVICE_MODE:-external}" = "kernel" ]; then
  SMTP_AUTH="false"
else
  SMTP_AUTH="true"
fi
SMTP_JSON=$(jq -n \
  --arg host "${SMTP_HOST}" \
  --arg port "${SMTP_PORT}" \
  --arg from "${SMTP_FROM}" \
  --arg user "${SMTP_USER}" \
  --arg pass "${SMTP_PASSWORD}" \
  --arg ssl "${SMTP_SSL}" \
  --arg starttls "${SMTP_STARTTLS}" \
  --arg auth "${SMTP_AUTH}" \
  '{
    host: $host,
    port: $port,
    from: $from,
    fromDisplayName: "Gentian",
    auth: $auth,
    ssl: $ssl,
    starttls: $starttls
  }
  # Keycloak keeps a stored user/password even when auth is off and will try to
  # use them again if auth is ever flipped back on; omit them entirely instead.
  + (if $auth == "true" then {user: $user, password: $pass} else {} end)')
UPDATED=$(printf '%%s' "${REALM_JSON}" | jq --argjson smtp "${SMTP_JSON}" '.smtpServer = $smtp')
curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/${REALM}" \
  -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  -d "${UPDATED}" >/dev/null
echo "tenant realm SMTP configured for ${REALM} (${SMTP_HOST}:${SMTP_PORT})"
`, realmExpr, keycloak.ShellWaitForRealm(realmExpr))
}

func makeTenantSMTPJob(tenantName, realmName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	// Older installs predate the mail_service_mode key in the SMTP secret.
	optionalKey := true
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
		corev1.EnvVar{
			// Decides whether Keycloak authenticates to the SMTP host. The
			// kernel Postfix is reachable only inside the cluster and permits
			// relaying by mynetworks, so it runs with smtpSASLAuthEnable "no"
			// and advertises no AUTH at all. Keycloak was told auth=true
			// unconditionally and could not authenticate against a server that
			// offers no mechanism, so every invite failed with "Failed to send
			// execute actions email".
			Name: "MAIL_SERVICE_MODE",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: keycloakSMTPCredentialsSecret},
					Key:                  "mail_service_mode",
					// Older installs predate this key; default to the external
					// relay behaviour rather than failing the Job.
					Optional: &optionalKey,
				},
			},
		},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantSMTPJobName(tenantName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:                         tenantName,
				managedByLabel:                      managedByValue,
				"gentianos.io/keycloak-tenant-smtp": tenantSMTPVersion,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{c},
				},
			},
		},
	}
}

// clusterKeycloakSMTPCredentialsAvailable reports whether this cluster has SMTP
// settings a realm can actually send with.
//
// Existence of the Secret is not that question. It is now built by an
// ExternalSecret that renders the cluster's non-secret mail settings whether or
// not the relay credential has been supplied — so the Secret exists from the
// moment the kernel syncs, and gating on presence alone would send every tenant
// into a configure Job that exits 1 on its own SMTP_CONFIGURE check.
//
// smtp_configure is the field that carries the answer: the ExternalSecret sets
// it to true only when both halves of the credential are there. The Job tests
// the same field, so the gate and the work agree on what "configured" means.
// The decision is a pure function of the Secret's contents so it can be tested
// without a client: every test in this package shares one envtest manager, and
// standing up even a fake client alongside it perturbs the scheduling the
// Job-polling tests depend on.
func smtpCredentialsUsable(data map[string][]byte) bool {
	return string(data["smtp_configure"]) == "true"
}

func (r *TenantReconciler) clusterKeycloakSMTPCredentialsAvailable(ctx context.Context) bool {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: keycloakSMTPCredentialsSecret, Namespace: kernelNamespace}, secret); err != nil {
		return false
	}
	return smtpCredentialsUsable(secret.Data)
}

func (r *TenantReconciler) ensureTenantSMTPJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	if r.KernelRealm == "" || r.KernelDomain == "" {
		return true, nil
	}
	if !r.clusterKeycloakSMTPCredentialsAvailable(ctx) {
		// Said, but at debug level, because this runs on every pass of a
		// provisioning tenant's two-second requeue — an Info line here is one
		// per tenant every two seconds for as long as provisioning takes, and
		// in envtest (where Jobs never complete, so the loop never ends) it was
		// enough log volume to push unrelated tests past their timeouts.
		//
		// The durable signal is the Secret's own smtp_configure field, which is
		// false exactly when this returns early, and which GETTING-STARTED
		// names as the thing to check when the portal answers an invitation
		// with "503: tenant realm SMTP is not configured".
		log.FromContext(ctx).V(1).Info(
			"realm SMTP left unconfigured: the cluster has no usable SMTP credential",
			"tenant", tenant.Name,
			"secret", fmt.Sprintf("%s/%s", kernelNamespace, keycloakSMTPCredentialsSecret),
			"remedy", "supply the smtp-relay credential (Admin Console -> Credentials)")
		return true, nil
	}
	realmDone, err := r.waitForProvisioningJob(ctx, tenant.Name, realmJobName(tenant.Name))
	if err != nil || !realmDone {
		return false, err
	}
	if err := r.replaceOutdatedTenantSMTPJob(ctx, tenant.Name); err != nil {
		return false, err
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, tenantSMTPJobName(tenant.Name))
}

// replaceOutdatedTenantSMTPJob is replaceOutdatedJob for this Job's label. See
// that function for why a version label is load-bearing.
func (r *TenantReconciler) replaceOutdatedTenantSMTPJob(ctx context.Context, tenantName string) error {
	return r.replaceOutdatedJob(ctx, tenantSMTPJobName(tenantName), tenantName,
		"gentianos.io/keycloak-tenant-smtp", tenantSMTPVersion)
}

// replaceOutdatedJob deletes a completed provisioning Job whose script has since
// changed, so the current one runs.
//
// A Job's spec is immutable once created, and waitForProvisioningJob only
// recreates a Job that FAILED or is absent. So without this a script change
// reaches new tenants only, while every existing realm keeps whatever the first
// run wrote — and the version label that was supposed to prevent that is stamped
// and never read. That happened once already with the SMTP Job; this is the same
// mechanism, named so the next Job can use it instead of growing a third copy.
func (r *TenantReconciler) replaceOutdatedJob(ctx context.Context, jobName, tenantName, labelKey, want string) error {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if err != nil {
		// Absent is the normal path; waitForProvisioningJob creates it.
		return client.IgnoreNotFound(err)
	}
	if job.Labels[labelKey] == want {
		return nil
	}
	prop := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil {
		return client.IgnoreNotFound(err)
	}
	log.FromContext(ctx).Info("replaced outdated provisioning job",
		"job", jobName, "tenant", tenantName, "had", job.Labels[labelKey], "want", want)
	return nil
}
