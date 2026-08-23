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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// kernelTenantBrokerVersion bumps when the script changes. 5 dropped the two IdP
// mappers, which tenant-default owns.
const kernelTenantBrokerVersion = "5"

// kernelPortalFirstBrokerLoginFlowAlias is the kernel-realm first-broker-login flow
// for users imported from tenant IdPs during portal login.
const kernelPortalFirstBrokerLoginFlowAlias = "first-broker-login-kernel-portal"

func kernelExternalURL(kernelDomain string) string {
	return fmt.Sprintf("https://id.%s/auth", kernelDomain)
}

func tenantKernelBrokerJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-kernel-tenant-broker-%s", tenantName)
}

// buildKernelTenantBrokerScript makes the kernel realm's first-broker-login flow.
//
// That is all it does now. It once registered the tenant realm as an OIDC
// Identity Provider in the kernel realm, wrote the broker client, and hung two
// mappers off the IdP — which tenant a brokered user came from, and the groups
// that carry their entitlements. tenant-default composes all of those.
//
// The flow stays because the kernel realm has no XTenant and so no Composition:
// it is bootstrapped at install, and its first-broker-login flow is auto-link by
// email, which is not the built-in behaviour. The IdP that names the flow is
// composed, and Keycloak rejects an IdP whose flow alias does not resolve, so
// this must exist before that reconciles.
//
// One flow, shared by every tenant — the Job is per-tenant only because that is
// where the reconcile loop that runs it lives, and the script is idempotent.
func buildKernelTenantBrokerScript() string {
	return fmt.Sprintf(`
set -eu

if [ -z "${KERNEL_REALM:-}" ]; then
  echo "kernel first-broker-login flow skipped (KERNEL_REALM unset)"
  exit 0
fi

`+keycloak.ShellAdminToken()+`
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

%s
`, buildEnsureFirstBrokerLoginFlowShellWithAlias("${KERNEL_REALM}", kernelPortalFirstBrokerLoginFlowAlias))
}

func makeKernelTenantBrokerJob(tenantName, realmName, kernelRealm string) *batchv1.Job {
	ttl := int32(3600)
	c := keycloakContainer("kernel-tenant-broker", buildKernelTenantBrokerScript())
	c.Env = append(c.Env,
		// REALM_NAME is passed but unread: it varies per tenant, and the script
		// hash derives from the pod spec, so a tenant that renames its realm
		// still re-runs this rather than trusting another tenant's Job.
		corev1.EnvVar{Name: "REALM_NAME", Value: realmName},
		corev1.EnvVar{Name: "KERNEL_REALM", Value: kernelRealm},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantKernelBrokerJobName(tenantName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenantName,
				managedByLabel: managedByValue,
				"gentianos.io/keycloak-kernel-tenant-broker": kernelTenantBrokerVersion,
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

func (r *TenantReconciler) ensureKernelTenantBrokerJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	if r.KernelRealm == "" || r.KernelDomain == "" {
		return true, nil
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, tenantKernelBrokerJobName(tenant.Name))
}
