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
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	conditionIdentityReady = "IdentityReady"
	keycloakAdminSecret    = "keycloak-admin"
	appLabel               = "gentianos.io/app"
	identityRequeueAfter   = 2 * time.Second
)

// realmBrokerParams holds SSO identity brokering parameters for the realm provisioning job.
// When nil, no identity brokering is configured for the realm.
// The broker registers the shared kernel realm as an OIDC Identity Provider in the
// tenant realm so users logged into the portal don't need a second login for tenant apps.
type realmBrokerParams struct {
	kernelRealm       string // Keycloak realm name for the shared SSO realm, e.g. "kernel"
	kernelExternalURL string // External base URL of Keycloak, e.g. "https://id.desk.gentian.org"
}

// ensureIdentity provisions a Keycloak realm and OIDC/SAML clients for the tenant.
// It waits for Crossplane-owned Jobs in the kernel namespace that call the
// Keycloak Admin REST API. Returns a non-zero RequeueAfter while Jobs are pending.
func (r *TenantReconciler) ensureIdentity(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	realmName := keycloakRealmName(tenant)

	oidcConfigs, err := r.collectOIDCAppConfigs(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	samlConfigs, err := r.collectSAMLAppConfigs(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.cleanupOrphanedClientJobs(ctx, tenant, oidcConfigs, samlConfigs); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup orphaned client Jobs: %w", err)
	}

	// We must always provision the tenant Keycloak realm for app OIDC and the
	// kernel IdP broker, even when no apps currently require OIDC clients.

	realmDone, err := r.ensureRealmJob(ctx, tenant, realmName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure Keycloak realm Job: %w", err)
	}
	if !realmDone {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningRealm", "Waiting for Keycloak realm Job to complete")
		return r.requeueForPendingJob(ctx, tenant.Name, realmJobName(tenant.Name)), nil
	}

	groupsDone, err := r.ensureGentianGroupsJob(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure Gentian groups Job: %w", err)
	}
	if !groupsDone {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningGroups", "Waiting for Gentian groups Job to complete")
		return r.requeueForPendingJob(ctx, tenant.Name, gentianGroupsJobName(tenant.Name)), nil
	}

	// Ensure realm-admin user exists in the realm (Option A tenant admin).
	adminDone, err := r.ensureAdminJob(ctx, tenant, realmName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure Keycloak tenant admin Job: %w", err)
	}
	if !adminDone {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningAdmin", "Waiting for tenant admin Job to complete")
		return r.requeueForPendingJob(ctx, tenant.Name, adminJobName(tenant.Name)), nil
	}

	if len(oidcConfigs) > 0 {
		browserDone, err := r.ensureOIDCBrowserFlowJob(ctx, tenant, realmName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure OIDC browser flow Job: %w", err)
		}
		if !browserDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningBrowserFlow", "Waiting for OIDC browser flow Job to complete")
			return r.requeueForPendingJob(ctx, tenant.Name, oidcBrowserFlowJobName(tenant.Name)), nil
		}
		firstLoginDone, err := r.ensureBrokerFirstLoginFlowJob(ctx, tenant, realmName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure broker first-login flow Job: %w", err)
		}
		if !firstLoginDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningBrokerFirstLogin", "Waiting for broker first-login flow Job to complete")
			return r.requeueForPendingJob(ctx, tenant.Name, brokerFirstLoginFlowJobName(tenant.Name)), nil
		}
	}

	// OIDC packs require Gentian entitlement groups (provisioned above).
	allDone := true
	var pendingClientJobs []string
	for _, cfg := range oidcConfigs {
		profile, err := r.getOIDCOwnerProfile(ctx, cfg)
		if err != nil {
			return ctrl.Result{}, err
		}
		if crossplaneOwnsOIDCClient(profile, cfg) {
			if err := r.seedOIDCSecrets(ctx, tenant, realmName, cfg); err != nil {
				return ctrl.Result{}, err
			}
			continue
		}
		done, err := r.ensureOIDCClientJob(ctx, tenant, realmName, cfg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Keycloak OIDC Job for app %s: %w", cfg.profileName, err)
		}
		if !done {
			allDone = false
			pendingClientJobs = append(pendingClientJobs, clientJobName(tenant.Name, cfg.profileName))
		}
	}

	for _, cfg := range samlConfigs {
		done, err := r.ensureSAMLClientJob(ctx, tenant, realmName, cfg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Keycloak SAML Job for app %s: %w", cfg.profileName, err)
		}
		if !done {
			allDone = false
			pendingClientJobs = append(pendingClientJobs, clientJobName(tenant.Name, cfg.profileName))
		}
	}

	if !allDone {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningClients", "Waiting for OIDC/SAML client Jobs to complete")
		return r.requeueForPendingJob(ctx, tenant.Name, pendingClientJobs...), nil
	}

	if r.KernelRealm != "" {
		brokerIdPDone, err := r.ensureBrokerIdentityProviderJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure broker IdP Job: %w", err)
		}
		if !brokerIdPDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningBrokerIdP", "Waiting for broker IdP Job to complete")
			return r.requeueForPendingJob(ctx, tenant.Name, tenantBrokerIdPJobName(tenant.Name)), nil
		}

		kernelBrokerDone, err := r.ensureKernelTenantBrokerJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure kernel tenant broker Job: %w", err)
		}
		if !kernelBrokerDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningKernelTenantBroker", "Waiting for kernel tenant broker Job to complete")
			return r.requeueForPendingJob(ctx, tenant.Name, tenantKernelBrokerJobName(tenant.Name)), nil
		}

		portalBFFDone, err := r.ensurePortalBFFClientJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure portal BFF client Job: %w", err)
		}
		if !portalBFFDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningPortalBFF", "Waiting for portal BFF client Job to complete")
			return r.requeueForPendingJob(ctx, tenant.Name, tenantPortalBFFClientJobName(tenant.Name)), nil
		}

		portalClientDone, err := r.ensurePortalPublicClientJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure portal public client Job: %w", err)
		}
		if !portalClientDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningPortalPublicClient", "Waiting for portal public OIDC client Job to complete")
			return r.requeueForPendingJob(ctx, tenant.Name, tenantPortalPublicClientJobName(tenant.Name)), nil
		}

		smtpDone, err := r.ensureTenantSMTPJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure tenant SMTP Job: %w", err)
		}
		if !smtpDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningTenantSMTP", "Waiting for tenant realm SMTP Job to complete")
			return r.requeueForPendingJob(ctx, tenant.Name, tenantSMTPJobName(tenant.Name)), nil
		}
	}

	r.setCondition(tenant, conditionIdentityReady, metav1.ConditionTrue,
		"Provisioned", "Keycloak realm and OIDC clients are ready")
	return ctrl.Result{}, nil
}

// ensureRealmJob waits for the Crossplane-owned Keycloak realm Job.
func (r *TenantReconciler) ensureRealmJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, realmJobName(tenant.Name))
}

// ensureAdminJob waits for the Crossplane-owned tenant realm-admin Job.
func (r *TenantReconciler) ensureAdminJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, adminJobName(tenant.Name))
}

// ensureClientJob waits for the Crossplane-owned OIDC client Job for one app.
func (r *TenantReconciler) ensureClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName, appName, clientID string, redirectURIs []string) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, clientJobName(tenant.Name, appName))
}

// ensureSAMLClientJob waits for the Keycloak SAML client Job for one app.
func (r *TenantReconciler) ensureSAMLClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string, cfg samlAppConfig) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, clientJobName(tenant.Name, cfg.profileName))
}

// deleteIdentity handles identity cleanup on tenant deletion.
// With DeletionPolicyDelete it creates a Job that permanently removes the Keycloak realm
// (cascading all clients and sessions). With DeletionPolicyRetain it creates a Job that
// disables the realm — users cannot log in but all configuration is preserved for fast
// redeploy (the realm provisioning job re-enables it on the next deploy).
// When no realm was provisioned (no OIDC apps) the function is a no-op in both cases.
func (r *TenantReconciler) deleteIdentity(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	realmName := keycloakRealmName(tenant)

	var jobName string
	var makeJob func() *batchv1.Job
	if tenant.Spec.DeletionPolicy == gentianov1alpha1.DeletionPolicyDelete {
		jobName = realmDeleteJobName(tenant.Name)
		makeJob = func() *batchv1.Job { return makeRealmDeleteJob(tenant, realmName) }
	} else {
		// Retain path: only disable the realm if one was actually provisioned.
		// If the realm job is absent, no realm exists in Keycloak and there is nothing to disable.
		rj := &batchv1.Job{}
		switch err := r.Get(ctx, types.NamespacedName{Name: realmJobName(tenant.Name), Namespace: kernelNamespace}, rj); {
		case err == nil:
			if !jobIsComplete(rj) {
				// Manifest exists but Crossplane never finished provisioning the realm.
				return nil
			}
			// Realm was provisioned; proceed to create the disable job.
		case errors.IsNotFound(err):
			return nil
		default:
			return err
		}
		jobName = realmDisableJobName(tenant.Name)
		makeJob = func() *batchv1.Job { return makeRealmDisableJob(tenant, realmName, r.KernelRealm) }
	}

	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, existing)
	if err == nil {
		if jobIsComplete(existing) {
			// Delete provisioning jobs so they are re-created on the next deploy.
			provNames := []string{
				realmJobName(tenant.Name), gentianGroupsJobName(tenant.Name), adminJobName(tenant.Name),
				oidcBrowserFlowJobName(tenant.Name),
			}
			for _, app := range tenant.Spec.Apps {
				provNames = append(provNames, clientJobName(tenant.Name, app.Profile))
			}
			if clientApps, err := r.listTenantAppsFromJobPrefix(ctx, tenant.Name, clientJobName(tenant.Name, "")); err != nil {
				return err
			} else {
				for _, app := range clientApps {
					provNames = appendUniqueStrings(provNames, clientJobName(tenant.Name, app))
				}
			}
			r.deleteProvisioningJobs(ctx, provNames...)
			return nil
		}
		return errDeleteJobPending
	}
	if !errors.IsNotFound(err) {
		return err
	}
	if err := r.Create(ctx, makeJob()); err != nil {
		return err
	}
	return errDeleteJobPending
}

// --- Job constructors --------------------------------------------------------

func makeRealmJob(tenant *gentianov1alpha1.Tenant, realmName, kernelDomain string, broker *realmBrokerParams) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	c := keycloakContainer("provision-realm", buildRealmScript(realmName, tenant.Spec.DisplayName))
	// Inject realm name as a shell variable so the IdP brokering section can
	// reference it without additional fmt.Sprintf substitutions.
	c.Env = append(c.Env, corev1.EnvVar{Name: "REALM_NAME", Value: realmName})
	if broker != nil {
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "KERNEL_REALM", Value: broker.kernelRealm},
			corev1.EnvVar{Name: "KERNEL_EXTERNAL_URL", Value: broker.kernelExternalURL},
		)
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      realmJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
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

func makeClientJob(tenant *gentianov1alpha1.Tenant, realmName, appName, clientID string, redirectURIs []string, clientSecret string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	redirectURI := redirectURIs[0]
	container := keycloakContainer("provision-client", buildClientScript(realmName, clientID, redirectURI))
	if clientSecret != "" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "OIDC_CLIENT_SECRET",
			Value: clientSecret,
		})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clientJobName(tenant.Name, appName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
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

func makeSAMLClientJob(tenant *gentianov1alpha1.Tenant, realmName, appName, entityID, acsURL string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	container := keycloakContainer("provision-saml-client", buildSAMLClientScript(realmName, entityID, acsURL))
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clientJobName(tenant.Name, appName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
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

// makeAdminJob builds the tenant-admin provisioning Job. effectiveDomain is the
// tenant's ingress domain, used only to synthesise a contact address when
// spec.adminEmail is empty — the CRD marks it required, so this is a defensive
// path. The address is derived from the cluster's own domain rather than any
// fixed operator domain: gentian-os runs on whatever domain the installer gave
// it and must not assume one.
func makeAdminJob(tenant *gentianov1alpha1.Tenant, realmName, effectiveDomain string, creds secrets.TenantAdminCreds) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	container := keycloakContainer("provision-tenant-admin", buildAdminScript(realmName))
	adminEmail := tenant.Spec.AdminEmail
	if adminEmail == "" {
		domain := effectiveDomain
		if domain == "" {
			// No domain configured at all: .invalid is reserved by RFC 2606 and
			// can never resolve, which is the honest representation of "unknown".
			domain = tenant.Name + ".invalid"
		}
		adminEmail = creds.Username + "@" + domain
	}
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "TENANT_NAME", Value: tenant.Name},
		corev1.EnvVar{Name: "TENANT_ADMIN_USERNAME", Value: creds.Username},
		corev1.EnvVar{Name: "TENANT_ADMIN_PASSWORD", Value: creds.Password},
		corev1.EnvVar{Name: "TENANT_ADMIN_EMAIL", Value: adminEmail},
		corev1.EnvVar{Name: "TENANT_ADMINS_GROUP", Value: gentianTenantAdminsGroup(tenant.Name)},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
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

func makeRealmDisableJob(tenant *gentianov1alpha1.Tenant, realmName, kernelRealm string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      realmDisableJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						keycloakContainer("disable-realm", buildRealmDisableScript(realmName, "admin-"+tenant.Name, kernelRealm)),
					},
				},
			},
		},
	}
}

func makeRealmDeleteJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      realmDeleteJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						keycloakContainer("delete-realm", buildRealmDeleteScript(realmName)),
					},
				},
			},
		},
	}
}

// keycloakContainer returns a Container spec that runs a shell script via the
// Alpine-based Keycloak provisioner image (wget + jq). Credentials are injected
// from the well-known keycloak-admin Secret in the kernel namespace.
func keycloakContainer(name, script string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c", keycloak.ProvisionerBootstrap + script},
		Env: []corev1.EnvVar{
			{
				Name: "KEYCLOAK_URL",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: keycloakAdminSecret},
						Key:                  "url",
					},
				},
			},
			{
				Name: "KEYCLOAK_ADMIN_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: keycloakAdminSecret},
						Key:                  "password",
					},
				},
			},
			{
				Name: "KEYCLOAK_ADMIN_USERNAME",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: keycloakAdminSecret},
						Key:                  "username",
					},
				},
			},
		},
		Resources: meta.InitJobResources(),
	}
}

// --- Shell scripts -----------------------------------------------------------

// buildRealmScript creates or updates a Keycloak realm and optionally registers
// kernel→tenant SSO brokering when KERNEL_REALM / KERNEL_EXTERNAL_URL are set.
const realmScriptBrokerIDPlaceholder = "__GENTIAN_BROKER_ID_BLOCK__"

func buildRealmScript(realmName, displayName string) string {
	brokerResolveID := `keycloak_json_id_by_attr "${BROKER_RESP}" "clientId" "${BROKER_CLIENT_ID}"
BROKER_KC_ID="${_kj_id}"
if [ -z "${BROKER_KC_ID}" ]; then
  echo "ERROR: could not resolve broker client id (clientId=${BROKER_CLIENT_ID})" >&2
  exit 1
fi`

	script := fmt.Sprintf(`set -eu

TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
HTTP=$(curl -s -o /dev/null -w "%%{http_code}" \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s")
if [ "${HTTP}" = "404" ]; then
  curl -sf \
    -X POST "${KEYCLOAK_URL}/admin/realms" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"realm":"%s","enabled":true,"displayName":"%s","registrationAllowed":false,"browserSecurityHeaders":`+authz.BrowserSecurityHeadersJSON()+`}'
  echo "realm %s created"
else
  curl -sf \
    -X PUT "${KEYCLOAK_URL}/admin/realms/%s" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"realm":"%s","enabled":true,"browserSecurityHeaders":`+authz.BrowserSecurityHeadersJSON()+`}'
  echo "realm %s already exists, ensured enabled=true and browserSecurityHeaders (was HTTP ${HTTP})"
fi

# ── SSO Identity Brokering: register kernel realm as Identity Provider ───────
if [ -n "${KERNEL_REALM:-}" ] && [ -n "${KERNEL_EXTERNAL_URL:-}" ]; then
  BROKER_CLIENT_ID="broker-${REALM_NAME}"
  BROKER_REDIRECT="${KERNEL_EXTERNAL_URL}/realms/${REALM_NAME}/broker/kernel/endpoint"

  TOKEN=$(curl -sf --max-time 30 \
    -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
    | sed 's/.*"access_token":"\([^"]*\)".*/\1/')

  BROKER_RESP=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients?clientId=${BROKER_CLIENT_ID}")
  if echo "${BROKER_RESP}" | grep -q "\"clientId\":\"${BROKER_CLIENT_ID}\""; then
`+realmScriptBrokerIDPlaceholder+`
    curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients/${BROKER_KC_ID}" \
      -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      -d "{\"clientId\":\"${BROKER_CLIENT_ID}\",\"redirectUris\":[\"${BROKER_REDIRECT}\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":false}" >/dev/null
    echo "broker client ${BROKER_CLIENT_ID} updated in ${KERNEL_REALM} realm"
  else
    curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients" \
      -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      -d "{\"clientId\":\"${BROKER_CLIENT_ID}\",\"redirectUris\":[\"${BROKER_REDIRECT}\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":false}"
    BROKER_RESP=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
      "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients?clientId=${BROKER_CLIENT_ID}")
`+realmScriptBrokerIDPlaceholder+`
    echo "broker client ${BROKER_CLIENT_ID} created in ${KERNEL_REALM} realm"
  fi
  BROKER_SECRET=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients/${BROKER_KC_ID}/client-secret" \
    | sed 's/.*"value":"\([^"]*\)".*/\1/')

  IDP_HTTP=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel")
  IDP_BODY="{\"alias\":\"kernel\",\"displayName\":\"Gentian SSO\",\"providerId\":\"oidc\",\"enabled\":true,\"trustEmail\":true,\"storeToken\":true,\"firstBrokerLoginFlowAlias\":\"first broker login\",\"config\":{\"issuer\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}\",\"authorizationUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/auth\",\"tokenUrl\":\"${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/token\",\"jwksUrl\":\"${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/certs\",\"userInfoUrl\":\"${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/userinfo\",\"logoutUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/logout\",\"backchannelSupported\":\"true\",\"clientId\":\"${BROKER_CLIENT_ID}\",\"clientSecret\":\"${BROKER_SECRET}\",\"syncMode\":\"IMPORT\",\"useJwksUrl\":\"true\",\"validateSignature\":\"true\",\"defaultScope\":\"openid profile email\",\"hideOnLoginPage\":\"true\"}}"
  if [ "${IDP_HTTP}" = "200" ]; then
    curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel" \
      -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
      -d "${IDP_BODY}" >/dev/null
    echo "IdP kernel updated in realm ${REALM_NAME}"
  else
    curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances" \
      -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
      -d "${IDP_BODY}"
    echo "IdP kernel registered in realm ${REALM_NAME}"
  fi
`+brokerKernelClientUsernameMapperShell+brokerIdPUsernameImporterShell+`
fi`, realmName, realmName, displayName, realmName, realmName, realmName, realmName)
	script = strings.ReplaceAll(script, realmScriptBrokerIDPlaceholder, brokerResolveID)
	return keycloak.ShellJSONIDExtractor() + script + keycloak.ShellEnsureInviteEmailUserProfile(realmName) + keycloak.ShellDisableProfilePromptRequiredActions(realmName)
}

func buildClientScript(realmName, clientID, redirectURI string) string {
	// The script is idempotent: it creates the client on first run, and on
	// subsequent runs it always updates redirectUris + secret so config stays
	// in sync with what the controller generates (redirect URI may change when
	// the app type determines a different callback pattern, e.g. Synapse).
	return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
SECRET_FIELD=""
if [ -n "${OIDC_CLIENT_SECRET:-}" ]; then
  SECRET_FIELD=",\"secret\":\"${OIDC_CLIENT_SECRET}\""
fi
EXISTING=$(curl -sf \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s/clients?clientId=%s")
if echo "${EXISTING}" | grep -q '"id"'; then
  CID=$(echo "${EXISTING}" | sed 's/.*"id":"\([^"]*\)".*/\1/')
  echo "client %s already exists (id=${CID}) in realm %s"
  curl -sf \
    -X PUT "${KEYCLOAK_URL}/admin/realms/%s/clients/${CID}" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"clientId\":\"%s\",\"redirectUris\":[\"%s\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"serviceAccountsEnabled\":true,\"publicClient\":false${SECRET_FIELD}}"
  echo "client %s updated (redirect URIs + secret)"
else
  curl -sf \
    -X POST "${KEYCLOAK_URL}/admin/realms/%s/clients" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"clientId\":\"%s\",\"redirectUris\":[\"%s\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"serviceAccountsEnabled\":true,\"publicClient\":false${SECRET_FIELD}}"
  echo "client %s created in realm %s"
fi`, realmName, clientID, clientID, realmName, realmName, clientID, redirectURI, clientID, realmName, clientID, redirectURI, clientID, realmName)
}

func buildSAMLClientScript(realmName, entityID, acsURL string) string {
	return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
EXISTING=$(curl -sf \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s/clients?clientId=%s")
if echo "${EXISTING}" | grep -q '"id"'; then
  CID=$(echo "${EXISTING}" | jq -r '.[0].id')
  echo "SAML client %s already exists (id=${CID}) in realm %s"
  curl -sf \
    -X PUT "${KEYCLOAK_URL}/admin/realms/%s/clients/${CID}" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"clientId\":\"%s\",\"redirectUris\":[\"%s\"],\"protocol\":\"saml\",\"standardFlowEnabled\":true,\"publicClient\":false,\"attributes\":{\"saml.client.signature\":\"false\"},\"protocolMappers\":[{\"name\":\"email\",\"protocol\":\"saml\",\"protocolMapper\":\"saml-user-property-mapper\",\"consentRequired\":false,\"config\":{\"attribute.name\":\"email\",\"attribute.nameformat\":\"Basic\",\"friendly.name\":\"email\",\"user.attribute\":\"email\"}},{\"name\":\"firstName\",\"protocol\":\"saml\",\"protocolMapper\":\"saml-user-property-mapper\",\"consentRequired\":false,\"config\":{\"attribute.name\":\"firstName\",\"attribute.nameformat\":\"Basic\",\"friendly.name\":\"firstName\",\"user.attribute\":\"firstName\"}},{\"name\":\"lastName\",\"protocol\":\"saml\",\"protocolMapper\":\"saml-user-property-mapper\",\"consentRequired\":false,\"config\":{\"attribute.name\":\"lastName\",\"attribute.nameformat\":\"Basic\",\"friendly.name\":\"lastName\",\"user.attribute\":\"lastName\"}}]}"
  echo "SAML client %s updated"
else
  curl -sf \
    -X POST "${KEYCLOAK_URL}/admin/realms/%s/clients" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"clientId\":\"%s\",\"redirectUris\":[\"%s\"],\"protocol\":\"saml\",\"standardFlowEnabled\":true,\"publicClient\":false,\"attributes\":{\"saml.client.signature\":\"false\"},\"protocolMappers\":[{\"name\":\"email\",\"protocol\":\"saml\",\"protocolMapper\":\"saml-user-property-mapper\",\"consentRequired\":false,\"config\":{\"attribute.name\":\"email\",\"attribute.nameformat\":\"Basic\",\"friendly.name\":\"email\",\"user.attribute\":\"email\"}},{\"name\":\"firstName\",\"protocol\":\"saml\",\"protocolMapper\":\"saml-user-property-mapper\",\"consentRequired\":false,\"config\":{\"attribute.name\":\"firstName\",\"attribute.nameformat\":\"Basic\",\"friendly.name\":\"firstName\",\"user.attribute\":\"firstName\"}},{\"name\":\"lastName\",\"protocol\":\"saml\",\"protocolMapper\":\"saml-user-property-mapper\",\"consentRequired\":false,\"config\":{\"attribute.name\":\"lastName\",\"attribute.nameformat\":\"Basic\",\"friendly.name\":\"lastName\",\"user.attribute\":\"lastName\"}}]}"
  echo "SAML client %s created in realm %s"
fi`, realmName, entityID, entityID, realmName, realmName, entityID, acsURL, entityID, realmName, entityID, acsURL, entityID, realmName)
}

func buildAdminScript(realmName string) string {
	// The script:
	//   1. Authenticates to the master realm (cluster-admin creds from keycloakAdminSecret).
	//   2. Creates the tenant admin user in the tenant realm if absent.
	//   3. Always syncs the OpenBao-canonical password and clears stale requiredActions.
	//   4. Grants the realm-management/realm-admin composite role so the user can
	//      manage users/groups/clients/sessions within this realm only.
	//
	// All steps are idempotent: users/roles are checked for existence before
	// POST so re-running the Job is safe.
	return keycloak.ShellJSONIDExtractor() + fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"
CREATED=0

# --- 1. Create tenant admin user if absent ---
EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/%s/users?username=${TENANT_ADMIN_USERNAME}&exact=true")
if echo "${EXISTING}" | grep -q '"id"'; then
  UID=$(echo "${EXISTING}" | sed 's/.*"id":"\([^"]*\)".*/\1/')
  echo "tenant admin ${TENANT_ADMIN_USERNAME} already exists (id=${UID}) in realm %s"
else
	curl -sf -X POST -H "${AUTH_HEADER}" \
    -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/%s/users" \
    -d "{\"username\":\"${TENANT_ADMIN_USERNAME}\",\"enabled\":true,\"requiredActions\":[\"UPDATE_PASSWORD\"]}"
	EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/%s/users?username=${TENANT_ADMIN_USERNAME}&exact=true")
  UID=$(echo "${EXISTING}" | sed 's/.*"id":"\([^"]*\)".*/\1/')
	CREATED=1
  echo "tenant admin ${TENANT_ADMIN_USERNAME} created (id=${UID}) in realm %s"
fi

# --- 2. Sync OpenBao-canonical password (idempotent; required after Retain redeploy) ---
HTTP=$(curl -s -o /tmp/kc-pw-body -w "%%{http_code}" -X PUT -H "${AUTH_HEADER}" \
  -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}/reset-password" \
  -d "{\"type\":\"password\",\"value\":\"${TENANT_ADMIN_PASSWORD}\",\"temporary\":false}")
case "${HTTP}" in
200|204)
  echo "password synced from OpenBao (temporary=false)"
  ;;
*)
  echo "Keycloak reset-password failed (HTTP ${HTTP})" >&2
  cat /tmp/kc-pw-body >&2 2>/dev/null || true
  exit 1
  ;;
esac
curl -sf -X PUT -H "${AUTH_HEADER}" \
  -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}" \
  -d "{\"enabled\":true,\"email\":\"${TENANT_ADMIN_EMAIL}\",\"emailVerified\":true,\"firstName\":\"Tenant\",\"lastName\":\"Administrator\",\"requiredActions\":[]}"
echo "tenant admin user enabled; requiredActions cleared; email=${TENANT_ADMIN_EMAIL}"
echo "INITIAL_TENANT_ADMIN realm=%s username=${TENANT_ADMIN_USERNAME} password=${TENANT_ADMIN_PASSWORD}"
echo "INITIAL_TENANT_ADMIN_RETRIEVE bao kv get -mount=secret -field=password gentian-os/tenants/${TENANT_NAME}/admin"

# --- 3. Grant realm-admin composite role via realm-management client ---
MGMT_CLIENT_ID=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/%s/clients?clientId=realm-management" \
  | sed 's/.*"id":"\([^"]*\)".*/\1/')
ROLE_ID=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/%s/clients/${MGMT_CLIENT_ID}/roles/realm-admin" \
  | sed 's/.*"id":"\([^"]*\)".*/\1/')
ROLE_NAME="realm-admin"
EXISTING_ROLES=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}/role-mappings/clients/${MGMT_CLIENT_ID}")
if echo "${EXISTING_ROLES}" | grep -q '"realm-admin"'; then
  echo "realm-admin role already assigned"
else
	curl -sf -X POST -H "${AUTH_HEADER}" \
    -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}/role-mappings/clients/${MGMT_CLIENT_ID}" \
    -d "[{\"id\":\"${ROLE_ID}\",\"name\":\"${ROLE_NAME}\"}]"
  echo "realm-admin role granted"
fi

# --- 4. Assign gentian:tenant:<t>:admins group membership ---
GROUP_LIST=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/%s/groups?search=${TENANT_ADMINS_GROUP}&exact=true")
keycloak_json_id_by_attr "${GROUP_LIST}" "name" "${TENANT_ADMINS_GROUP}"
ADMINS_GROUP_ID="${_kj_id}"
if [ -z "${ADMINS_GROUP_ID}" ]; then
  echo "ERROR: tenant admins group ${TENANT_ADMINS_GROUP} not found in realm %s" >&2
  exit 1
fi
MEMBER_GROUPS=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}/groups")
if echo "${MEMBER_GROUPS}" | grep -q "\"name\":\"${TENANT_ADMINS_GROUP}\""; then
  echo "tenant admin already in group ${TENANT_ADMINS_GROUP}"
else
  curl -sf -X PUT -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}/groups/${ADMINS_GROUP_ID}"
  echo "tenant admin joined group ${TENANT_ADMINS_GROUP}"
fi`,
		realmName, realmName, realmName, realmName, realmName,
		realmName, realmName, realmName, realmName, realmName,
		realmName, realmName, realmName, realmName, realmName, realmName)
}

func buildRealmDeleteScript(realmName string) string {
	return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
HTTP=$(curl -s -o /dev/null -w "%%{http_code}" \
  -X DELETE \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s")
echo "realm %s deletion requested (HTTP ${HTTP})"`, realmName, realmName)
}

// buildRealmDisableScript disables a Keycloak realm on Retain undeploy,
// invalidating all active sessions.
func buildRealmDisableScript(realmName, adminUsername, kernelRealm string) string {
	return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
HTTP=$(curl -s -o /dev/null -w "%%{http_code}" \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s")
if [ "${HTTP}" = "404" ]; then
  echo "realm %s not found, nothing to disable"
else
  curl -sf \
    -X PUT "${KEYCLOAK_URL}/admin/realms/%s" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"realm":"%s","enabled":false}'
  echo "realm %s disabled (sessions invalidated)"
fi
# Also disable tenant admin in kernel realm when brokering is configured.
USER_RESP=$(curl -sf -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s/users?username=%s&exact=true" || echo "")
if echo "${USER_RESP}" | grep -q '"id"'; then
  UID=$(echo "${USER_RESP}" | sed 's/.*"id":"\([^"]*\)".*/\1/')
  curl -sf -X PUT -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}" \
    -d '{"enabled":false}' || true
  echo "user %s disabled in %s realm"
else
  echo "user %s not found in %s realm"
fi`, realmName, realmName, realmName, realmName, realmName, kernelRealm, adminUsername, kernelRealm, adminUsername, kernelRealm, adminUsername, kernelRealm)
}

// --- Name helpers ------------------------------------------------------------

// keycloakRealmName returns the Keycloak realm name for a tenant.
// Uses spec.isolation.keycloakRealm if set, otherwise defaults to the tenant name.
func keycloakRealmName(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.KeycloakRealm != "" {
		return tenant.Spec.Isolation.KeycloakRealm
	}
	return tenant.Name
}

func adminJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-admin-%s", tenantName)
}

func realmJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-realm-%s", tenantName)
}

func clientJobName(tenantName, appName string) string {
	return fmt.Sprintf("keycloak-client-%s-%s", tenantName, appName)
}

func realmDeleteJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-realm-delete-%s", tenantName)
}

func realmDisableJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-realm-disable-%s", tenantName)
}

func oidcClientID(tenantName, appName string) string {
	return fmt.Sprintf("%s-%s", tenantName, appName)
}

// --- Job status helpers ------------------------------------------------------

func jobIsComplete(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobCompletionTime(job *batchv1.Job) *metav1.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return &c.LastTransitionTime
		}
	}
	return nil
}

func jobCompletedAfter(sync, source *batchv1.Job) bool {
	syncTime := jobCompletionTime(sync)
	sourceTime := jobCompletionTime(source)
	if syncTime == nil || sourceTime == nil {
		return false
	}
	return sourceTime.After(syncTime.Time)
}

func jobIsFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
