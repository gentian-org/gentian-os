/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/kernel"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	conditionIdentityReady = "IdentityReady"
	keycloakAdminSecret    = "keycloak-admin"
	appLabel               = "gentianos.io/app"
	identityRequeueAfter   = 2 * time.Second
)

// kernelBrokerEnabled is off: nobody signs in to a tenant realm through the
// kernel realm, so the realm Job no longer makes the client a tenant realm
// would sign in to the kernel realm as (broker-<realm>). tenant-default has the
// same switch for the identity provider that used it. The code stays; true
// brings the client back.
const kernelBrokerEnabled = false

// realmBrokerParams holds SSO identity brokering parameters for the realm provisioning job.
// When nil, no identity brokering is configured for the realm.
// The broker registers the shared kernel realm as an OIDC Identity Provider in the
// tenant realm so users logged into the portal don't need a second login for tenant apps.
type realmBrokerParams struct {
	kernelRealm       string // Keycloak realm name for the shared SSO realm, e.g. "kernel"
	kernelExternalURL string // External base URL of Keycloak, e.g. "https://id.platform.example.com"
}

// ensureIdentity provisions a Keycloak realm and OIDC/SAML clients for the tenant.
// It waits for Crossplane-owned Jobs in the kernel namespace that call the
// Keycloak Admin REST API. Returns a non-zero RequeueAfter while Jobs are pending.
func (r *TenantReconciler) ensureIdentity(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	realmName := keycloakRealmName(tenant)
	if r.adoptsKernelRealm(tenant) {
		return r.ensureAdoptedRealmIdentity(ctx, tenant)
	}

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

	// Two Jobs used to run here and no longer exist; what they leave behind is
	// swept. Both had already lost the object they were named for — the browser
	// flow and the first-broker-login flow are the Composition's — and what
	// remained of each was a repair for a state that no longer occurs.
	r.deleteRetiredJobs(ctx,
		oidcBrowserFlowJobName(tenant.Name),
		brokerFirstLoginFlowJobName(tenant.Name))

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
		// No broker IdP Job to wait for. Its last two writes — the mappers that
		// carry gentian_username across the realm boundary — are tenant-default's
		// now, so the Job is gone and what it left behind is swept.
		r.deleteRetiredJobs(ctx, tenantBrokerIdPJobName(tenant.Name))

		kernelBrokerDone, err := r.ensureKernelTenantBrokerJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure kernel tenant broker Job: %w", err)
		}
		if !kernelBrokerDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningKernelTenantBroker", "Waiting for kernel tenant broker Job to complete")
			return r.requeueForPendingJob(ctx, tenant.Name, tenantKernelBrokerJobName(tenant.Name)), nil
		}

		// The portal BFF client is a Composition resource now; its readiness is
		// reported through CrossplaneReady rather than watched here.

		// The portal public client is not waited on here any more. It is a
		// Composition resource, so its readiness belongs to CrossplaneReady
		// rather than to a Job this loop watches.

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

	// The kernel realm is never a tenant's to disable or delete, however the
	// tenant was written. The platform is itself a tenant whose realm is the
	// kernel one (AD-10), and a tenant can also be pointed at it by hand
	// through isolation.keycloakRealm -- in either case deleting that tenant
	// would run a realm-disable Job against the realm every administrator
	// signs in through, and lock all of them out of the cluster at once.
	//
	// The realm is adopted, never created by the tenant that adopts it, so
	// there is nothing here for its deletion to undo.
	if r.adoptsKernelRealm(tenant) {
		ctrl.LoggerFrom(ctx).Info(
			"tenant adopts the kernel realm; leaving it alone on deletion",
			"tenant", tenant.Name, "realm", realmName)
		return nil
	}

	var jobName string
	var makeJob func() *batchv1.Job
	if tenant.Spec.DeletionPolicy == gentianov1alpha1.DeletionPolicyDelete {
		jobName = realmDeleteJobName(tenant.Name)
		makeJob = func() *batchv1.Job { return makeRealmDeleteJob(tenant, realmName, r.KernelRealm) }
	} else {
		// Retain path: only disable the realm if one was actually provisioned.
		// If the realm job is absent, no realm exists in Keycloak and there is nothing to disable.
		rj := &batchv1.Job{}
		switch err := r.Get(ctx, types.NamespacedName{Name: realmJobName(tenant.Name), Namespace: identityNamespace}, rj); {
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
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: identityNamespace}, existing)
	if err == nil {
		if jobIsComplete(existing) {
			// Delete provisioning jobs so they are re-created on the next deploy.
			provNames := []string{
				realmJobName(tenant.Name), gentianGroupsJobName(tenant.Name), adminJobName(tenant.Name),
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
			return r.deleteProvisioningJobs(ctx, provNames...)
		}
		if jobIsFailed(existing) {
			// The realm was not deleted, or not disabled. Said, and the
			// Job removed so the next pass runs it again; it used to count
			// as still running, and the deletion waited on it in silence.
			prop := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &prop}); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("remove the failed Job %s: %w", jobName, err)
			}
			return fmt.Errorf("%w: %s in %s; it is run again", provisioner.ErrDeleteJobFailed, jobName, identityNamespace)
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
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
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
			Namespace: identityNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
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

func makeClientJob(tenant *gentianov1alpha1.Tenant, realmName, appName, clientID string, redirectURIs []string, clientSecret string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
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
			Namespace: identityNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
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

func makeSAMLClientJob(tenant *gentianov1alpha1.Tenant, realmName, appName, entityID, acsURL string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	container := keycloakContainer("provision-saml-client", buildSAMLClientScript(realmName, entityID, acsURL))
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clientJobName(tenant.Name, appName),
			Namespace: identityNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
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

// makeAdminJob builds the tenant-admin provisioning Job.
//
// The account is created with no password and with what activating it takes
// as required actions: set a password, and enrol a second factor unless the
// tenant opts out. Its holder does both through a single-use, expiring link
// the director issues on request (activate-admin) -- mailed to a recovery
// address when one is given then, otherwise shown once to whoever asked. The
// platform never knows the password, the same as for every member it invites.
func makeAdminJob(tenant *gentianov1alpha1.Tenant, realmName, username string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	requireMFA := "false"
	if tenant.AdminRequiresMFA() {
		requireMFA = "true"
	}
	container := keycloakContainer("provision-tenant-admin", buildAdminScript(realmName))
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "TENANT_NAME", Value: tenant.Name},
		corev1.EnvVar{Name: "TENANT_ADMIN_USERNAME", Value: username},
		corev1.EnvVar{Name: "TENANT_ADMIN_REQUIRE_MFA", Value: requireMFA},
		corev1.EnvVar{Name: "TENANT_ADMINS_GROUP", Value: gentianTenantAdminsGroup(tenant.Name)},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminJobName(tenant.Name),
			Namespace: identityNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
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

func makeRealmDisableJob(tenant *gentianov1alpha1.Tenant, realmName, kernelRealm string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      realmDisableJobName(tenant.Name),
			Namespace: identityNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
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

func makeRealmDeleteJob(tenant *gentianov1alpha1.Tenant, realmName, kernelRealm string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      realmDeleteJobName(tenant.Name),
			Namespace: identityNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						keycloakContainer("delete-realm", buildRealmDeleteScript(realmName, kernelRealm)),
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

`+keycloak.ShellAdminToken()+`
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
  # The realm is NOT restated here. tenant-default composes a Realm that
  # declares enabled, displayName, registrationAllowed and the browser security
  # headers, and this Job writing them too made it a second writer of the realm
  # root — the shape that had the kernel IdP on the wrong login flow for two
  # minutes of every reconcile.
  #
  # The create above stays, as a bootstrap: the Composition needs the realm to
  # exist before it can adopt it, and everything else in the realm needs the
  # realm.
  echo "realm %s already exists (HTTP ${HTTP}); its settings are the Composition's"
fi

# ── SSO Identity Brokering: register kernel realm as Identity Provider ───────
if [ -n "${KERNEL_REALM:-}" ] && [ -n "${KERNEL_EXTERNAL_URL:-}" ]; then
  BROKER_CLIENT_ID="broker-${REALM_NAME}"
  BROKER_REDIRECT="${KERNEL_EXTERNAL_URL}/realms/${REALM_NAME}/broker/kernel/endpoint"

`+keycloak.ShellAdminToken()+`

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
  # The IdP is NOT written here. tenant-default composes it, fully managed, and
  # declares more than this script ever could: the sixteen provider defaults the
  # live object was missing, useJwksUrl and updateProfileFirstLoginMode through
  # extraConfig, and the client id and secret taken from the broker Client's
  # connection Secret rather than read back from the admin API.
  #
  # This was the last object with two writers. They agreed only because the
  # script had been taught to carry forward whichever first-broker-login alias it
  # observed instead of restating one — which is a truce, not a resolution, and
  # the same truce held right up until the two disagreed about that field.
  #
  # The broker client above stays, and has to. It is Observe-only in the
  # Composition by design: writeConnectionSecretToRef republishes its secret
  # without rotating it, which is what lets the IdP take credentials from a
  # Secret. Something has to create it first, and on a realm that does not exist
  # yet that something cannot be the Composition.

# No gentian_username mappers here. This script wrote both — the one that makes
# the kernel broker client emit the claim, and the one that imports it back into
# the tenant user's uid — and so did the broker-idp Job, of three writers for two
# objects. tenant-default composes them.
fi`, realmName, realmName, displayName, realmName, realmName)
	script = strings.ReplaceAll(script, realmScriptBrokerIDPlaceholder, brokerResolveID)
	// No user-profile writes. tenant-default composes a UserProfile that declares
	// all six attributes whole, where this appended one patch to add uid and
	// gentian.inviteEmail and a second to strip `required` off the name fields.
	return keycloak.ShellJSONIDExtractor() + script
}

func buildClientScript(realmName, clientID, redirectURI string) string {
	// The script is idempotent: it creates the client on first run, and on
	// subsequent runs it always updates redirectUris + secret so config stays
	// in sync with what the controller generates (redirect URI may change when
	// the app type determines a different callback pattern, e.g. Synapse).
	return fmt.Sprintf(`set -eu
`+keycloak.ShellAdminToken()+`
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
`+keycloak.ShellAdminToken()+`
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
	//   2. Creates the tenant admin if absent -- with NO password, and with
	//      UPDATE_PASSWORD (and CONFIGURE_TOTP unless the tenant opts out) as
	//      required actions, which its holder completes through an activation
	//      link. An existing account's password is never touched: it is its
	//      holder's, and re-running the Job must not reset it.
	//   3. Keeps the second-factor requirement as the tenant declares it.
	//   4. Grants the realm-management/realm-admin composite role so the user can
	//      manage users/groups/clients/sessions within this realm only.
	//
	// All steps are idempotent: users/roles are checked for existence before
	// POST so re-running the Job is safe. Nothing secret is printed.
	return keycloak.ShellJSONIDExtractor() + fmt.Sprintf(`set -eu
`+keycloak.ShellAdminToken()+`
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

ACTIONS='"UPDATE_PASSWORD"'
if [ "${TENANT_ADMIN_REQUIRE_MFA:-true}" = "true" ]; then
  ACTIONS='"UPDATE_PASSWORD","CONFIGURE_TOTP"'
fi

# --- 1. Create tenant admin user if absent, without a password ---
EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/%s/users?username=${TENANT_ADMIN_USERNAME}&exact=true")
if echo "${EXISTING}" | grep -q '"id"'; then
  UID=$(echo "${EXISTING}" | sed 's/.*"id":"\([^"]*\)".*/\1/')
  echo "tenant admin ${TENANT_ADMIN_USERNAME} already exists (id=${UID}) in realm %s"
else
  curl -sf -X POST -H "${AUTH_HEADER}" \
    -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/%s/users" \
    -d "{\"username\":\"${TENANT_ADMIN_USERNAME}\",\"enabled\":true,\"emailVerified\":false,\"firstName\":\"Tenant\",\"lastName\":\"Administrator\",\"requiredActions\":[${ACTIONS}]}"
  EXISTING=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/%s/users?username=${TENANT_ADMIN_USERNAME}&exact=true")
  UID=$(echo "${EXISTING}" | sed 's/.*"id":"\([^"]*\)".*/\1/')
  echo "tenant admin ${TENANT_ADMIN_USERNAME} created without a password (id=${UID}) in realm %s"
fi

# --- 2. The second-factor requirement, as declared ---
USER_JSON=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}")
CREDS=$(curl -sf -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}/credentials")
HAS_OTP=$(echo "${CREDS}" | jq '[.[] | select(.type=="otp")] | length > 0')
UPDATED=$(echo "${USER_JSON}" | jq \
  --arg mfa "${TENANT_ADMIN_REQUIRE_MFA:-true}" \
  --argjson hasotp "${HAS_OTP}" '
  .requiredActions = ((.requiredActions // []) - ["CONFIGURE_TOTP"])
  | (if $mfa == "true" and ($hasotp | not) then .requiredActions += ["CONFIGURE_TOTP"] else . end)')
curl -sf -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}" -d "${UPDATED}"
echo "tenant admin: second factor required=${TENANT_ADMIN_REQUIRE_MFA:-true}"

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

// buildRealmDeleteScript deletes the tenant's realm, and with it every
// client, group, membership and user in it.
//
// The answer is read, not printed and forgotten: the script used to report
// "deletion requested (HTTP 403)" and exit 0, so a realm the admin credential
// could not delete was recorded as deleted and the tenant's deletion went on
// without it. Deleted, or already not there, is success; anything else fails
// the Job, and the deletion comes back to it.
//
// And what was made for the tenant in the kernel realm, which is not the
// tenant's and does not go with its realm: the client the tenant's realm
// signs in to the kernel realm as (broker-<realm>, made by the realm Job),
// with the protocol mapper the tenant's Composition hangs on it. It used to
// stay for ever -- a confidential client, with a redirect to a realm that no
// longer exists -- one per tenant ever deleted. kernelRealm is empty on a
// cluster with no kernel realm, where there is no such client.
//
// The client is looked for by its exact id and removed by the id Keycloak
// answers with; "not there" is a 200 with an empty list, never inferred from
// a failed delete.
func buildRealmDeleteScript(realmName, kernelRealm string) string {
	script := fmt.Sprintf(`set -eu
`+keycloak.ShellAdminToken()+`
HTTP=$(curl -s -o /dev/null -w "%%{http_code}" \
  -X DELETE \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%[1]s")
case "${HTTP}" in
  204) echo "realm %[1]s deleted" ;;
  404) echo "realm %[1]s is not there" ;;
  *) echo "ERROR: deleting realm %[1]s answered HTTP ${HTTP}" >&2; exit 1 ;;
esac
`, realmName)
	if kernelRealm == "" || kernelRealm == realmName {
		return script
	}
	return script + fmt.Sprintf(`BROKER="broker-%[1]s"
FOUND=$(curl -sS --fail --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%[2]s/clients?clientId=${BROKER}")
BROKER_ID=$(printf '%%s' "${FOUND}" | jq -r --arg id "${BROKER}" '.[] | select(.clientId == $id) | .id' | head -n 1)
if [ -z "${BROKER_ID}" ]; then
  echo "client ${BROKER} is not in realm %[2]s"
else
  HTTP=$(curl -s -o /dev/null -w "%%{http_code}" \
    -X DELETE \
    -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/%[2]s/clients/${BROKER_ID}")
  case "${HTTP}" in
    204) echo "client ${BROKER} removed from realm %[2]s, with its mappers" ;;
    404) echo "client ${BROKER} is not in realm %[2]s" ;;
    *) echo "ERROR: removing client ${BROKER} from realm %[2]s answered HTTP ${HTTP}" >&2; exit 1 ;;
  esac
fi
`, realmName, kernelRealm)
}

// buildRealmDisableScript disables a Keycloak realm on Retain undeploy,
// invalidating all active sessions, and disables the tenant's administrator
// in the kernel realm when there is one to broker through.
//
// Nothing in it is allowed to fail quietly: a realm that could not be
// disabled is a tenant that was retired and can still sign in.
func buildRealmDisableScript(realmName, adminUsername, kernelRealm string) string {
	script := fmt.Sprintf(`set -eu
`+keycloak.ShellAdminToken()+`
HTTP=$(curl -s -o /dev/null -w "%%{http_code}" \
  -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%[1]s")
case "${HTTP}" in
  404) echo "realm %[1]s not found, nothing to disable" ;;
  200)
    curl -sf \
      -X PUT "${KEYCLOAK_URL}/admin/realms/%[1]s" \
      -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      -d '{"realm":"%[1]s","enabled":false}'
    echo "realm %[1]s disabled (sessions invalidated)" ;;
  *) echo "ERROR: reading realm %[1]s answered HTTP ${HTTP}" >&2; exit 1 ;;
esac
`, realmName)
	// Not while kernelBroker is off: nothing creates that user in the kernel realm.
	if kernelRealm == "" || !kernelBroker {
		return script
	}
	return script + fmt.Sprintf(`# Also disable the tenant admin in the kernel realm, which brokers its sign-in.
USER_RESP=$(curl -sf -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%[1]s/users?username=%[2]s&exact=true")
if echo "${USER_RESP}" | grep -q '"id"'; then
  UID=$(echo "${USER_RESP}" | sed 's/.*"id":"\([^"]*\)".*/\1/')
  curl -sf -X PUT -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/%[1]s/users/${UID}" \
    -d '{"enabled":false}'
  echo "user %[2]s disabled in %[1]s realm"
else
  echo "user %[2]s not found in %[1]s realm"
fi
`, kernelRealm, adminUsername)
}

// --- Name helpers ------------------------------------------------------------

// keycloakRealmName returns the Keycloak realm name for a tenant.
// Uses spec.isolation.keycloakRealm if set, otherwise defaults to the tenant name.
func keycloakRealmName(tenant *gentianov1alpha1.Tenant) string {
	return keycloak.RealmName(tenant)
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

// jobHasCondition reports whether the Job carries condType as True.
func jobHasCondition(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobIsComplete(job *batchv1.Job) bool {
	return jobHasCondition(job, batchv1.JobComplete)
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
	return jobHasCondition(job, batchv1.JobFailed)
}

// loadKeycloakAdmin reads Keycloak's admin endpoint and credential from the
// authentication namespace, for the Jobs and calls that configure a realm.
func loadKeycloakAdmin(ctx context.Context, c client.Reader) (url, user, pass string, err error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: keycloakAdminSecret, Namespace: identityNamespace}, secret); err != nil {
		return "", "", "", err
	}
	url = string(secret.Data["url"])
	user = string(secret.Data["username"])
	if user == "" {
		user = "admin"
	}
	pass = string(secret.Data["password"])
	if url == "" || pass == "" {
		return "", "", "", fmt.Errorf("keycloak-admin secret missing url or password")
	}
	return url, user, pass, nil
}

// adoptsKernelRealm reports whether a tenant's identity lives in the kernel
// realm rather than in one of its own. Tenant/platform is the case (AD-10):
// the realm every administrator signs in through is adopted, never created,
// configured, brokered or deleted by a tenant. Only what belongs to the
// tenant inside it -- its entitlement groups, its apps' clients -- is the
// tenant's to provision.
func (r *TenantReconciler) adoptsKernelRealm(tenant *gentianov1alpha1.Tenant) bool {
	return r.KernelRealm != "" && keycloakRealmName(tenant) == r.KernelRealm
}

// ensureAdoptedRealmIdentity is ensureIdentity for a tenant that adopts the
// kernel realm: no realm Job to wait for, no administrator account minted,
// no broker in either direction, no mail server and no auth mount of its own
// -- the realm has all of those already, from the identity bootstrap. What
// is waited for is the tenant's groups, and its apps' clients when it has any.
func (r *TenantReconciler) ensureAdoptedRealmIdentity(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	groupsDone, err := r.ensureGentianGroupsJob(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure Gentian groups Job: %w", err)
	}
	if !groupsDone {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningGroups", "Waiting for Gentian groups Job to complete")
		return r.requeueForPendingJob(ctx, tenant.Name, gentianGroupsJobName(tenant.Name)), nil
	}
	oidcConfigs, err := r.collectOIDCAppConfigs(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}
	var pending []string
	for _, cfg := range oidcConfigs {
		done, err := r.ensureOIDCClientJob(ctx, tenant, r.KernelRealm, cfg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Keycloak OIDC Job for app %s: %w", cfg.profileName, err)
		}
		if !done {
			pending = append(pending, clientJobName(tenant.Name, cfg.profileName))
		}
	}
	if len(pending) > 0 {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningClients", "Waiting for OIDC client Jobs to complete")
		return r.requeueForPendingJob(ctx, tenant.Name, pending...), nil
	}
	r.setCondition(tenant, conditionIdentityReady, metav1.ConditionTrue,
		"Adopted", "The kernel realm is adopted; the tenant's groups and clients are ready")
	return ctrl.Result{}, nil
}
