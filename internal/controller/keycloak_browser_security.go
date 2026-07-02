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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// browserSecurityHeadersVersion bumps when the realm PUT payload changes so
// completed jobs are recreated on operator upgrade.
const browserSecurityHeadersVersion = "1"

// keycloakBrowserSecurityHeadersJSON is the Keycloak realm browserSecurityHeaders
// payload. xFrameOptions must be empty — ingress sets frame-ancestors and
// X-Frame-Options: SAMEORIGIN blocks broker /endpoint callbacks in app iframes.
const keycloakBrowserSecurityHeadersJSON = `{"contentSecurityPolicy":"","contentSecurityPolicyReportOnly":"","strictTransportSecurity":"max-age=31536000; includeSubDomains","xContentTypeOptions":"nosniff","xFrameOptions":"","xRobotsTag":"none","xXSSProtection":"1; mode=block","referrerPolicy":"no-referrer"}`

func buildRealmBrowserSecurityHeadersScript(realmName string) string {
	return fmt.Sprintf(`set -eu

TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')

HTTP=$(curl -s -o /dev/null -w "%%{http_code}" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -X PUT "${KEYCLOAK_URL}/admin/realms/%s" \
  -d '{"browserSecurityHeaders":%s}')
if [ "${HTTP}" -ge 200 ] 2>/dev/null && [ "${HTTP}" -lt 300 ] 2>/dev/null; then
  echo "browserSecurityHeaders updated for realm %s (HTTP ${HTTP})"
else
  echo "ERROR: browserSecurityHeaders update for realm %s failed (HTTP ${HTTP})" >&2
  exit 1
fi
`, realmName, keycloakBrowserSecurityHeadersJSON, realmName, realmName)
}

func kernelBrowserSecurityJobName() string {
	return "keycloak-browser-security-kernel"
}

func tenantBrowserSecurityJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-browser-security-%s", tenantName)
}

func makeBrowserSecurityHeadersJob(name, realmName string) *batchv1.Job {
	script := buildRealmBrowserSecurityHeadersScript(realmName)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kernelNamespace,
			Labels: map[string]string{
				managedByLabel:                           managedByValue,
				"gentianos.io/keycloak-browser-security": browserSecurityHeadersVersion,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						managedByLabel: managedByValue,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						keycloakContainer("browser-security-headers", script),
					},
				},
			},
		},
	}
}

func browserSecurityJobCurrent(job *batchv1.Job) bool {
	return job != nil && job.Labels["gentianos.io/keycloak-browser-security"] == browserSecurityHeadersVersion
}

func (r *TenantReconciler) ensureBrowserSecurityHeadersJob(ctx context.Context, jobName, realmName string) error {
	if realmName == "" {
		return nil
	}
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return r.Create(ctx, makeBrowserSecurityHeadersJob(jobName, realmName))
	}
	if err != nil {
		return fmt.Errorf("get browser security job %s: %w", jobName, err)
	}
	if !browserSecurityJobCurrent(job) {
		prop := metav1.DeletePropagationBackground
		if delErr := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); delErr != nil && !errors.IsNotFound(delErr) {
			return fmt.Errorf("delete stale browser security job %s: %w", jobName, delErr)
		}
		return r.Create(ctx, makeBrowserSecurityHeadersJob(jobName, realmName))
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return r.Create(ctx, makeBrowserSecurityHeadersJob(jobName, realmName))
	}
	if !jobIsComplete(job) {
		log.FromContext(ctx).Info("waiting for Keycloak browser security headers job", "job", jobName, "realm", realmName)
	}
	return nil
}

// ensureKeycloakBrowserSecurityHeaders disables X-Frame-Options on kernel and
// tenant realms so OIDC broker /endpoint callbacks work inside portal iframes.
func (r *TenantReconciler) ensureKeycloakBrowserSecurityHeaders(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	kernelRealm := r.KernelRealm
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}
	if err := r.ensureBrowserSecurityHeadersJob(ctx, kernelBrowserSecurityJobName(), kernelRealm); err != nil {
		return err
	}
	return r.ensureBrowserSecurityHeadersJob(ctx, tenantBrowserSecurityJobName(tenant.Name), keycloakRealmName(tenant))
}
