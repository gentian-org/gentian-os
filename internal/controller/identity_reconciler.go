/*
Copyright 2026 The Gentian Authors.

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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	conditionIdentityReady   = "IdentityReady"
	keycloakProvisionerImage = "curlimages/curl:8.7.1"
	keycloakAdminSecret      = "keycloak-admin"
	appLabel                 = "gentianos.io/app"
	identityRequeueAfter     = 2 * time.Second
)

// realmLDAPParams holds LDAP federation parameters for the realm provisioning job.
// When nil, LDAP federation is not configured for the realm.
type realmLDAPParams struct {
	server  string // LDAP connection URL, e.g. ldap://host:389
	bindDN  string // bind account DN, e.g. uid=app-keycloak,ou=tenant,dc=...
	bindPW  string // bind account password (from OpenBao seeder)
	usersDN string // users search base, e.g. ou=users,ou=tenant,dc=...
}

// realmBrokerParams holds SSO identity brokering parameters for the realm provisioning job.
// When nil, no identity brokering is configured for the realm.
// The broker registers the shared kernel realm as an OIDC Identity Provider in the
// tenant realm so users logged into the portal don't need a second login for tenant apps.
type realmBrokerParams struct {
	kernelRealm       string // Keycloak realm name for the shared SSO realm, e.g. "kernel"
	kernelExternalURL string // External base URL of Keycloak, e.g. "https://id.desk.gentian.org"
}

// ensureIdentity provisions a Keycloak realm and OIDC clients for the tenant.
// It creates idempotent Kubernetes Jobs in the kernel namespace that call the
// Keycloak Admin REST API. Returns a non-zero RequeueAfter while Jobs are pending.
func (r *TenantReconciler) ensureIdentity(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	realmName := keycloakRealmName(tenant)

	oidcApps, err := r.collectOIDCApps(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(oidcApps) == 0 {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionTrue,
			"NoIdentityRequired", "No apps require identity provisioning")
		return ctrl.Result{}, nil
	}

	realmDone, err := r.ensureRealmJob(ctx, tenant, realmName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure Keycloak realm Job: %w", err)
	}
	if !realmDone {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningRealm", "Waiting for Keycloak realm Job to complete")
		return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
	}

	// Ensure realm-admin user exists in the realm (Option A tenant admin).
	adminDone, err := r.ensureAdminJob(ctx, tenant, realmName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure Keycloak tenant admin Job: %w", err)
	}
	if !adminDone {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningAdmin", "Waiting for tenant admin Job to complete")
		return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
	}

	allDone := true
	for _, appName := range oidcApps {
		var done bool
		var err error
		if getAppIsolationMode(tenant, appName) == gentianov1alpha1.AppDeploymentModeShared {
			done, err = r.ensureSharedAppsJob(ctx, tenant, appName)
		} else {
			done, err = r.ensureClientJob(ctx, tenant, realmName, appName)
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Keycloak client Job for app %s: %w", appName, err)
		}
		if !done {
			allDone = false
		}
	}
	if !allDone {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningClients", "Waiting for OIDC client Jobs to complete")
		return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
	}

	// Gate the kernel realm user re-enable on LDAP admin-user job completion.
	// Both the realm job and the LDAP admin-user job start in the same reconcile
	// iteration. The realm job finishes quickly; the LDAP job may still be
	// running. If we re-enable the Keycloak user before UDM clears shadowExpire,
	// Keycloak's next LDAP federation import sees the still-locked user and
	// re-disables them, causing "Invalid username or password" on subsequent logins.
	// If the LDAP job doesn't exist yet (first deploy before LDAP reconciler runs,
	// or test environment) we proceed optimistically — the user was never disabled
	// in Keycloak so there is no stale state to race against.
	adminLDAPJob := &batchv1.Job{}
	switch ldapJobErr := r.Get(ctx, types.NamespacedName{Name: adminUserJobName(tenant.Name), Namespace: kernelNamespace}, adminLDAPJob); {
	case errors.IsNotFound(ldapJobErr):
		// LDAP admin-user job not yet created; proceed without waiting.
	case ldapJobErr != nil:
		return ctrl.Result{}, ldapJobErr
	case !jobIsComplete(adminLDAPJob):
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"WaitingLDAPAdminUnlock", "Waiting for LDAP admin-user job to complete before re-enabling kernel realm user")
		return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
	}

	opendeskEnableDone, err := r.ensureOpendeskAdminEnableJob(ctx, tenant, "admin-"+tenant.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure kernel admin re-enable Job: %w", err)
	}
	if !opendeskEnableDone {
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"ProvisioningOpendeskEnable", "Waiting for opendesk admin enable Job to complete")
		return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
	}

	r.setCondition(tenant, conditionIdentityReady, metav1.ConditionTrue,
		"Provisioned", "Keycloak realm and OIDC clients are ready")
	return ctrl.Result{}, nil
}

// collectOIDCApps returns the profile names of apps in tenant.spec.apps that
// have kernelRequirements.identity.oidc enabled.
func (r *TenantReconciler) collectOIDCApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	var oidcApps []string
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue // profile not yet installed; will retry on next reconcile
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.KernelRequirements != nil &&
			profile.Spec.KernelRequirements.Identity != nil &&
			profile.Spec.KernelRequirements.Identity.OIDC {
			oidcApps = append(oidcApps, app.Profile)
		}
	}
	return oidcApps, nil
}

// getAppIsolationMode returns the effective deployment mode for appName in the
// given tenant. Returns AppDeploymentModeShared only when explicitly set to
// "shared". All other cases (empty, "dedicated", or app not found) return
// AppDeploymentModeDedicated so the default code path is unchanged.
func getAppIsolationMode(tenant *gentianov1alpha1.Tenant, appName string) gentianov1alpha1.AppDeploymentMode {
	for _, app := range tenant.Spec.Apps {
		if app.Profile == appName {
			if app.IsolationMode == gentianov1alpha1.AppDeploymentModeShared {
				return gentianov1alpha1.AppDeploymentModeShared
			}
			return gentianov1alpha1.AppDeploymentModeDedicated
		}
	}
	return gentianov1alpha1.AppDeploymentModeDedicated
}

// ensureSharedAppsJob creates the shared-apps realm provisioning Job for one
// app if absent. Returns true when the Job has completed successfully.
// This job sets up the shared-apps Keycloak realm, creates the app OIDC client,
// and registers the tenant realm as an IdP broker with auto-redirect.
func (r *TenantReconciler) ensureSharedAppsJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) (bool, error) {
	jobName := sharedAppsJobName(tenant.Name, appName)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		clientSecret := ""
		if r.Seeder != nil {
			// Seed OIDC credentials for the app in the shared-apps realm.
			// The issuer is always shared-apps (stable across all tenants using this app).
			// The clientID is the app name (not tenant-prefixed) because the client
			// is shared across tenants. All tenants using the same app in shared mode
			// will have the same OIDC client ID in the shared-apps realm.
			issuer := fmt.Sprintf("https://id.%s/realms/shared-apps", r.KernelDomain)
			creds, seedErr := r.Seeder.SeedOIDC(ctx, tenant.Name, appName, issuer, appName)
			if seedErr != nil {
				return false, fmt.Errorf("seed shared-apps oidc: %w", seedErr)
			}
			clientSecret = creds.ClientSecret
		}
		return false, r.Create(ctx, makeSharedAppsJob(tenant, appName, clientSecret, r.KernelDomain))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
}

// ensureRealmJob creates the Keycloak realm Job if absent.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureRealmJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) (bool, error) {
	jobName := realmJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		// Delete any stale cleanup jobs so the next undeploy creates a fresh one.
		r.deleteProvisioningJobs(ctx, realmDeleteJobName(tenant.Name), realmDisableJobName(tenant.Name))

		// B.3: Seed the keycloak LDAP bind password and pass it to the realm job
		// so the realm script can register the LDAP User Storage Provider.
		// Seeder is deterministic — calling SeedLDAP here and in ensureLDAP yields
		// the same password, keeping LDAP and Keycloak in sync.
		var ldap *realmLDAPParams
		if r.LDAPBase != "" && r.LDAPServer != "" && r.Seeder != nil {
			ouDN := tenantConcreteOUDN(tenant, r.LDAPBase)
			bindDN := fmt.Sprintf("uid=app-keycloak-%s,%s", tenant.Name, ouDN)
			creds, seedErr := r.Seeder.SeedLDAP(ctx, tenant.Name, "keycloak", secrets.LDAPCreds{
				BindDN: bindDN,
				BaseDN: ouDN,
			})
			if seedErr != nil {
				return false, fmt.Errorf("seed keycloak ldap: %w", seedErr)
			}
			ldap = &realmLDAPParams{
				server:  r.LDAPServer,
				bindDN:  bindDN,
				bindPW:  creds.BindPassword,
				usersDN: "ou=users," + ouDN,
			}
		}
		var broker *realmBrokerParams
		if r.KernelRealm != "" && r.KernelDomain != "" {
			broker = &realmBrokerParams{
				kernelRealm:       r.KernelRealm,
				kernelExternalURL: fmt.Sprintf("https://id.%s", r.KernelDomain),
			}
		}
		return false, r.Create(ctx, makeRealmJob(tenant, realmName, ldap, broker))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil // recreated on next reconcile
	}
	return jobIsComplete(job), nil
}

// ensureAdminJob creates (or re-creates on failure) the Job that provisions a
// tenant-scoped realm-admin user in the tenant's Keycloak realm. The user is
// assigned the built-in realm-management/realm-admin composite role so they
// can manage users, groups, clients, and sessions within their realm only —
// zero visibility into other realms.
//
// The admin password is derived from the master via Seeder.SeedTenantAdmin and
// stored write-once at gentian-os/tenants/<tenant>/admin in OpenBao. On first
// login Keycloak will prompt the tenant admin to set a new password
// (UPDATE_PASSWORD required action).
//
// When Seeder is nil (envtest / staged rollout) a placeholder password is
// used so the Job is still created and the test flow can proceed.
func (r *TenantReconciler) ensureAdminJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) (bool, error) {
	jobName := adminJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		var creds secrets.TenantAdminCreds
		if r.Seeder != nil {
			creds, err = r.Seeder.SeedTenantAdmin(ctx, tenant.Name)
			if err != nil {
				return false, fmt.Errorf("seed tenant admin: %w", err)
			}
			log.FromContext(ctx).Info(
				"Initial tenant admin credentials (printed once)",
				"tenant", tenant.Name,
				"realm", realmName,
				"username", creds.Username,
				"password", creds.Password,
				"retrieveCommand", fmt.Sprintf("bao kv get -mount=secret -field=password gentian-os/tenants/%s/admin", tenant.Name),
			)
		} else {
			creds = secrets.TenantAdminCreds{Username: "admin", Password: "placeholder"}
		}
		return false, r.Create(ctx, makeAdminJob(tenant, realmName, creds))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
}

// ensureClientJob creates the OIDC client Job for one app if absent.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName, appName string) (bool, error) {
	jobName := clientJobName(tenant.Name, appName)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		// Inc 21a: seed the per-app OIDC secret into OpenBao *before* creating
		// the provisioning Job so the Job can apply the derived client secret
		// to Keycloak (POST on create, PUT on update). When the Seeder is nil
		// (envtest / staged rollout) we fall back to the legacy behaviour of
		// letting Keycloak auto-generate the secret.
		clientSecret := ""
		if r.Seeder != nil {
			// OIDC issuer URL stays on the kernel domain so it is stable
			// across vanity-domain changes (see docs/architecture.md §2.5).
			// Falls back to tenant.Spec.Domain when KERNEL_DOMAIN is unset
			// (envtest / staged rollout).
			issuerHost := tenant.Spec.Domain
			if r.KernelDomain != "" {
				issuerHost = r.KernelDomain
			}
			issuer := fmt.Sprintf("https://id.%s/realms/%s", issuerHost, realmName)
			creds, seedErr := r.Seeder.SeedOIDC(ctx, tenant.Name, appName, issuer, oidcClientID(tenant.Name, appName))
			if seedErr != nil {
				return false, fmt.Errorf("seed oidc: %w", seedErr)
			}
			clientSecret = creds.ClientSecret
		}
		return false, r.Create(ctx, makeClientJob(tenant, realmName, appName, clientSecret, r.KernelDomain))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
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
			provNames := []string{realmJobName(tenant.Name), adminJobName(tenant.Name), kernelAdminEnableJobName(tenant.Name)}
			for _, app := range tenant.Spec.Apps {
				provNames = append(provNames, clientJobName(tenant.Name, app.Profile))
				provNames = append(provNames, sharedAppsJobName(tenant.Name, app.Profile))
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

func makeRealmJob(tenant *gentianov1alpha1.Tenant, realmName string, ldap *realmLDAPParams, broker *realmBrokerParams) *batchv1.Job {
	ttl := int32(3600)
	c := keycloakContainer("provision-realm", buildRealmScript(realmName, tenant.Spec.DisplayName))
	// Inject realm name as a shell variable so the IdP brokering section can
	// reference it without additional fmt.Sprintf substitutions.
	c.Env = append(c.Env, corev1.EnvVar{Name: "REALM_NAME", Value: realmName})
	if ldap != nil {
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "LDAP_SERVER", Value: ldap.server},
			corev1.EnvVar{Name: "LDAP_BIND_DN", Value: ldap.bindDN},
			corev1.EnvVar{Name: "LDAP_BIND_PASSWORD", Value: ldap.bindPW},
			corev1.EnvVar{Name: "LDAP_USERS_DN", Value: ldap.usersDN},
		)
	}
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

func makeClientJob(tenant *gentianov1alpha1.Tenant, realmName, appName, clientSecret, kernelDomain string) *batchv1.Job {
	ttl := int32(3600)
	clientID := oidcClientID(tenant.Name, appName)
	// Redirect URI lives on the tenant's app plane (vanity domain when set,
	// otherwise <tenant>.<kernel_domain> fallback). See architecture §2.5.
	redirectHost := tenant.EffectiveDomain(kernelDomain)
	if redirectHost == "" {
		redirectHost = tenant.Spec.Domain
	}
	var redirectURI string
	if appName == "element" {
		// Synapse handles OIDC on behalf of Element: the callback goes to the
		// Matrix homeserver endpoint, not the frontend app path.
		redirectURI = fmt.Sprintf("https://matrix.%s/_synapse/client/oidc/callback", redirectHost)
	} else {
		redirectURI = fmt.Sprintf("https://%s/%s/*", redirectHost, appName)
	}
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

// makeSharedAppsJob creates a Job that provisions the shared-apps Keycloak realm,
// creates the app OIDC client in it, and wires the tenant realm as an IdP broker
// with auto-redirect for transparent SSO.
func makeSharedAppsJob(tenant *gentianov1alpha1.Tenant, appName, clientSecret, kernelDomain string) *batchv1.Job {
	ttl := int32(3600)

	// Redirect URI for the shared deployment. Before C.4 (which moves the
	// deployment to a shared namespace), the Synapse is still in the tenant
	// namespace so we use the tenant's effective domain as the redirect host.
	redirectHost := tenant.EffectiveDomain(kernelDomain)
	if redirectHost == "" {
		redirectHost = tenant.Spec.Domain
	}
	var redirectURI string
	if appName == "element" {
		redirectURI = fmt.Sprintf("https://matrix.%s/_synapse/client/oidc/callback", redirectHost)
	} else {
		redirectURI = fmt.Sprintf("https://%s/%s/*", redirectHost, appName)
	}

	kernelExternalURL := fmt.Sprintf("https://id.%s", kernelDomain)
	tenantRealm := keycloakRealmName(tenant)

	c := keycloakContainer("provision-shared-apps", buildSharedAppsScript())
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "KERNEL_EXTERNAL_URL", Value: kernelExternalURL},
		corev1.EnvVar{Name: "TENANT_REALM", Value: tenantRealm},
		corev1.EnvVar{Name: "APP_NAME", Value: appName},
		corev1.EnvVar{Name: "APP_REDIRECT_URI", Value: redirectURI},
	)
	if clientSecret != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: "OIDC_CLIENT_SECRET", Value: clientSecret})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sharedAppsJobName(tenant.Name, appName),
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
					Containers:    []corev1.Container{c},
				},
			},
		},
	}
}

func makeAdminJob(tenant *gentianov1alpha1.Tenant, realmName string, creds secrets.TenantAdminCreds) *batchv1.Job {
	ttl := int32(3600)
	container := keycloakContainer("provision-tenant-admin", buildAdminScript(realmName))
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "TENANT_NAME", Value: tenant.Name},
		corev1.EnvVar{Name: "TENANT_ADMIN_USERNAME", Value: creds.Username},
		corev1.EnvVar{Name: "TENANT_ADMIN_PASSWORD", Value: creds.Password},
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
	ttl := int32(3600)
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
	ttl := int32(3600)
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

func makeOpendeskAdminEnableJob(tenant *gentianov1alpha1.Tenant, adminUsername, kernelRealm string) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kernelAdminEnableJobName(tenant.Name),
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
						keycloakContainer("re-enable-kernel-admin", buildOpendeskAdminEnableScript(adminUsername, kernelRealm)),
					},
				},
			},
		},
	}
}

// ensureOpendeskAdminEnableJob creates the job that re-enables the tenant admin
// in the shared kernel Keycloak realm. It is called only after the LDAP
// admin-user job has completed so shadowExpire is already cleared in LDAP,
// making the Keycloak re-enable durable against subsequent LDAP federation
// imports.
func (r *TenantReconciler) ensureOpendeskAdminEnableJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, adminUsername string) (bool, error) {
	jobName := kernelAdminEnableJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, makeOpendeskAdminEnableJob(tenant, adminUsername, r.KernelRealm))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
}

// keycloakContainer returns a Container spec that runs a shell script via the
// curl-based Keycloak provisioner image. Credentials are injected from the
// well-known keycloak-admin Secret in the kernel namespace.
func keycloakContainer(name, script string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   keycloakProvisionerImage,
		Command: []string{"/bin/sh", "-c", script},
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
	}
}

// --- Shell scripts -----------------------------------------------------------

// buildRealmScript creates or updates a Keycloak realm and ensures it is enabled.
// It does NOT re-enable the tenant admin user in the kernel realm here because
// this job runs concurrently with the LDAP admin-user unlock job. Re-enabling
// Keycloak before LDAP clears shadowExpire triggers a re-import that re-disables
// the user. The dedicated ensureOpendeskAdminEnableJob handles the re-enable
// after the LDAP admin-user job is confirmed complete.
func buildRealmScript(realmName, displayName string) string {
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
  curl -sf \
    -X POST "${KEYCLOAK_URL}/admin/realms" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"realm":"%s","enabled":true,"displayName":"%s","registrationAllowed":false}'
  echo "realm %s created"
else
  curl -sf \
    -X PUT "${KEYCLOAK_URL}/admin/realms/%s" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"realm":"%s","enabled":true}'
  echo "realm %s already exists, ensured enabled=true (was HTTP ${HTTP})"
fi

# Register LDAP User Storage Provider for per-tenant user federation.
# Only runs when LDAP_SERVER env var is set (injected by makeRealmJob when
# r.LDAPBase and r.LDAPServer are configured). Idempotent: checks for an
# existing provider named "ldap" before creating a new one.
if [ -n "${LDAP_SERVER:-}" ]; then
  LDAP_COMPONENTS=$(curl -sf \
    -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/%s/components?type=org.keycloak.storage.UserStorageProvider")
  if echo "${LDAP_COMPONENTS}" | grep -q '"name":"ldap"'; then
    echo "LDAP federation provider already registered in realm %s"
  else
    curl -sf \
      -X POST "${KEYCLOAK_URL}/admin/realms/%s/components" \
      -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      -d "{
        \"name\":\"ldap\",
        \"providerId\":\"ldap\",
        \"providerType\":\"org.keycloak.storage.UserStorageProvider\",
        \"config\":{
          \"connectionUrl\":[\"${LDAP_SERVER}\"],
          \"bindDn\":[\"${LDAP_BIND_DN}\"],
          \"bindCredential\":[\"${LDAP_BIND_PASSWORD}\"],
          \"usersDn\":[\"${LDAP_USERS_DN}\"],
          \"searchScope\":[\"1\"],
          \"authType\":[\"simple\"],
          \"vendor\":[\"other\"],
          \"usernameLDAPAttribute\":[\"uid\"],
          \"rdnLDAPAttribute\":[\"uid\"],
          \"uuidLDAPAttribute\":[\"entryUUID\"],
          \"userObjectClasses\":[\"person\"],
          \"customUserSearchFilter\":[\"(uid=*)\"],
          \"importEnabled\":[\"true\"],
          \"editMode\":[\"READ_ONLY\"],
          \"syncRegistrations\":[\"false\"],
          \"fullSyncPeriod\":[\"-1\"],
          \"changedSyncPeriod\":[\"-1\"],
          \"pagination\":[\"false\"],
          \"connectionPooling\":[\"true\"],
          \"batchSizeForSync\":[\"1000\"],
          \"cachePolicy\":[\"MAX_LIFESPAN\"],
          \"maxLifespan\":[\"300000\"],
          \"enabled\":[\"true\"]
        }
      }"
    echo "LDAP federation provider registered in realm %s"
  fi
fi

# ── SSO Identity Brokering: register kernel realm as Identity Provider ───────
# Runs only when KERNEL_REALM and KERNEL_EXTERNAL_URL are injected by makeRealmJob
# (i.e. when r.KernelRealm and r.KernelDomain are configured in the operator).
# Configures the tenant realm to delegate authentication to the shared kernel realm
# so users already logged into the portal are not prompted to log in again for
# tenant-specific apps (e.g. Element/Matrix chat).
# All steps are idempotent: existing resources are updated rather than recreated.
if [ -n "${KERNEL_REALM:-}" ] && [ -n "${KERNEL_EXTERNAL_URL:-}" ]; then
  BROKER_CLIENT_ID="broker-${REALM_NAME}"
  BROKER_REDIRECT="${KERNEL_EXTERNAL_URL}/realms/${REALM_NAME}/broker/kernel/endpoint"

  # Refresh the admin token here because the realm + LDAP steps above may have
  # consumed more than the default token lifetime (60 s).
  TOKEN=$(curl -sf --max-time 30 \
    -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
    | sed 's/.*"access_token":"\([^"]*\)".*/\1/')

  # 1. Ensure broker client exists in the kernel realm.
  #    This client is used by the tenant realm's IdP to authenticate TO kernel.
  BROKER_RESP=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients?clientId=${BROKER_CLIENT_ID}")
  if echo "${BROKER_RESP}" | grep -q '"id"'; then
    # Extract the first "id" value (not a nested mapper id) using grep -o
    BROKER_KC_ID=$(echo "${BROKER_RESP}" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
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
    BROKER_KC_ID=$(echo "${BROKER_RESP}" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
    echo "broker client ${BROKER_CLIENT_ID} created in ${KERNEL_REALM} realm"
  fi
  BROKER_SECRET=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/${KERNEL_REALM}/clients/${BROKER_KC_ID}/client-secret" \
    | sed 's/.*"value":"\([^"]*\)".*/\1/')

  # 2. Register kernel as an OIDC Identity Provider in the tenant realm (idempotent).
  #    hideOnLoginPage:true prevents showing the "Login with Gentian SSO" button
  #    redundantly; the defaultProvider setting (step 3) handles the auto-redirect.
  IDP_HTTP=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel")
  IDP_BODY="{\"alias\":\"kernel\",\"displayName\":\"Gentian SSO\",\"providerId\":\"oidc\",\"enabled\":true,\"trustEmail\":true,\"firstBrokerLoginFlowAlias\":\"first broker login\",\"config\":{\"issuer\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}\",\"authorizationUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/auth\",\"tokenUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/token\",\"jwksUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/certs\",\"userInfoUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/userinfo\",\"clientId\":\"${BROKER_CLIENT_ID}\",\"clientSecret\":\"${BROKER_SECRET}\",\"syncMode\":\"IMPORT\",\"useJwksUrl\":\"true\",\"validateSignature\":\"true\",\"defaultScope\":\"openid profile email\",\"hideOnLoginPage\":\"true\"}}"
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

  # 3. Configure the Identity Provider Redirector execution in the browser flow
  #    to automatically redirect to the kernel IdP. The realm-level
  #    defaultProvider attribute alone is not honoured by all Keycloak versions;
  #    setting it directly on the execution config is the reliable approach.
  EXEC_ID=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/authentication/flows/browser/executions" \
    | grep -o '"id":"[^"]*"[^}]*"providerId":"identity-provider-redirector"' \
    | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
  if [ -n "${EXEC_ID}" ]; then
    # Check if a config already exists on this execution
    EXISTING_CFG=$(curl -sf --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
      "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/authentication/executions/${EXEC_ID}/config" 2>/dev/null || echo "")
    if echo "${EXISTING_CFG}" | grep -q '"alias"'; then
      CFG_ID=$(echo "${EXISTING_CFG}" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
      curl -sf --max-time 30 -X PUT \
        "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/authentication/config/${CFG_ID}" \
        -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
        -d "{\"alias\":\"kernel-redirector\",\"config\":{\"defaultProvider\":\"kernel\"}}" >/dev/null
      echo "identity-provider-redirector config updated in realm ${REALM_NAME}"
    else
      curl -sf --max-time 30 -X POST \
        "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/authentication/executions/${EXEC_ID}/config" \
        -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
        -d "{\"alias\":\"kernel-redirector\",\"config\":{\"defaultProvider\":\"kernel\"}}" >/dev/null
      echo "identity-provider-redirector config created in realm ${REALM_NAME}"
    fi
  fi
  # Also set the realm-level defaultProvider attribute as a belt-and-suspenders fallback.
  curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"attributes\":{\"defaultProvider\":\"kernel\"}}" >/dev/null
  echo "default provider set to kernel in realm ${REALM_NAME}"
fi`, realmName, realmName, displayName, realmName, realmName, realmName, realmName,
		realmName, realmName, realmName, realmName)
}

// buildOpendeskAdminEnableScript re-enables the tenant admin user in the shared
// kernel Keycloak realm. This job is intentionally separate from
// buildRealmScript so it only runs after the LDAP admin-user job has cleared
// shadowExpire, preventing Keycloak's next LDAP import from overriding the
// re-enable with the previously-locked LDAP state.
func buildOpendeskAdminEnableScript(adminUsername, kernelRealm string) string {
	return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
USER_RESP=$(curl -sf -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s/users?username=%s&exact=true" || echo "")
if echo "${USER_RESP}" | grep -q '"id"'; then
  UID=$(echo "${USER_RESP}" | sed 's/.*"id":"\([^"]*\)".*/\1/')
  curl -sf -X PUT -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}" \
    -d '{"enabled":true}'
  echo "admin %s re-enabled in %s realm (LDAP shadowExpire already cleared)"
else
  echo "admin %s not found in %s realm (first deploy, no action needed)"
fi`, kernelRealm, adminUsername, kernelRealm, adminUsername, kernelRealm, adminUsername, kernelRealm)
}

// buildSharedAppsScript returns the shell script that idempotently provisions:
//  1. The shared-apps Keycloak realm (created once for the whole platform).
//  2. The app's OIDC client in shared-apps (one client for all tenants sharing
//     this app — e.g. "element" for Matrix/Synapse).
//  3. A broker client "broker-shared-apps" in the tenant realm so shared-apps
//     can authenticate users via the tenant's IdP.
//  4. The tenant realm registered as an OIDC IdP in shared-apps.
//  5. The identity-provider-redirector execution config in shared-apps set to
//     auto-redirect to the tenant realm IdP (silent SSO, no login prompt).
//
// All variable values are injected as environment variables by makeSharedAppsJob:
//
//	KERNEL_EXTERNAL_URL  external Keycloak base URL, e.g. https://id.desk.gentian.org
//	TENANT_REALM         tenant's Keycloak realm name, e.g. gtn-demo
//	APP_NAME             application type name, e.g. element
//	APP_REDIRECT_URI     OIDC callback URL for this app's deployment
//	OIDC_CLIENT_SECRET   (optional) pre-seeded client secret for the app client
//
// NOTE: the identity-provider-redirector is set to redirect to TENANT_REALM.
// When multiple tenants use the same app in shared mode this step becomes a
// no-op for subsequent tenants (last-write-wins). Multi-tenant shared-mode
// home-realm discovery is a future concern (requires org-domain hints or a
// custom authenticator).
func buildSharedAppsScript() string {
	return `set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

# 1. Ensure the shared-apps realm exists and is enabled.
HTTP=$(curl -s -o /dev/null -w "%{http_code}" \
  -H "${AUTH_HEADER}" "${KEYCLOAK_URL}/admin/realms/shared-apps")
if [ "${HTTP}" = "404" ]; then
  curl -sf -X POST "${KEYCLOAK_URL}/admin/realms" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d '{"realm":"shared-apps","enabled":true,"displayName":"Gentian Shared Apps","registrationAllowed":false}'
  echo "shared-apps realm created"
else
  curl -sf -X PUT "${KEYCLOAK_URL}/admin/realms/shared-apps" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d '{"realm":"shared-apps","enabled":true}' >/dev/null
  echo "shared-apps realm already exists (HTTP ${HTTP}), ensured enabled=true"
fi

# Refresh token — realm creation can consume the initial token's lifetime.
TOKEN=$(curl -sf --max-time 30 \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"

# 2. Create/update the app OIDC client in shared-apps.
#    This is the client the app server uses to obtain tokens from shared-apps.
#    When the client already exists we intentionally do NOT update the secret:
#    multiple tenants may share one app client (e.g. element in shared mode),
#    each seeding their own oidc_client_secret.  Overwriting the secret on every
#    run causes a last-writer-wins race — the app server (e.g. Synapse) was
#    deployed by the first tenant and always uses the first tenant's secret.
EXISTING_APP=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/shared-apps/clients?clientId=${APP_NAME}")
if echo "${EXISTING_APP}" | grep -q '"id"'; then
  APP_CID=$(echo "${EXISTING_APP}" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
  curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/shared-apps/clients/${APP_CID}" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "{\"clientId\":\"${APP_NAME}\",\"redirectUris\":[\"${APP_REDIRECT_URI}\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"serviceAccountsEnabled\":true,\"publicClient\":false}" >/dev/null
  echo "client ${APP_NAME} updated in shared-apps realm (secret unchanged)"
else
  SECRET_FIELD=""
  if [ -n "${OIDC_CLIENT_SECRET:-}" ]; then
    SECRET_FIELD=",\"secret\":\"${OIDC_CLIENT_SECRET}\""
  fi
  curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/admin/realms/shared-apps/clients" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "{\"clientId\":\"${APP_NAME}\",\"redirectUris\":[\"${APP_REDIRECT_URI}\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"serviceAccountsEnabled\":true,\"publicClient\":false${SECRET_FIELD}}"
  echo "client ${APP_NAME} created in shared-apps realm"
fi

# 3. Create/update the broker client in the tenant realm.
#    shared-apps uses this confidential client to authenticate users via the tenant IdP.
BROKER_CLIENT_ID="broker-shared-apps"
BROKER_REDIRECT="${KERNEL_EXTERNAL_URL}/realms/shared-apps/broker/${TENANT_REALM}/endpoint"
BROKER_RESP=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients?clientId=${BROKER_CLIENT_ID}")
if echo "${BROKER_RESP}" | grep -q '"id"'; then
  BROKER_KC_ID=$(echo "${BROKER_RESP}" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
  curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients/${BROKER_KC_ID}" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "{\"clientId\":\"${BROKER_CLIENT_ID}\",\"redirectUris\":[\"${BROKER_REDIRECT}\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":false}" >/dev/null
  echo "broker client ${BROKER_CLIENT_ID} updated in ${TENANT_REALM} realm"
else
  curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "{\"clientId\":\"${BROKER_CLIENT_ID}\",\"redirectUris\":[\"${BROKER_REDIRECT}\"],\"protocol\":\"openid-connect\",\"standardFlowEnabled\":true,\"publicClient\":false}"
  BROKER_RESP=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients?clientId=${BROKER_CLIENT_ID}")
  BROKER_KC_ID=$(echo "${BROKER_RESP}" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
  echo "broker client ${BROKER_CLIENT_ID} created in ${TENANT_REALM} realm"
fi
BROKER_SECRET=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${TENANT_REALM}/clients/${BROKER_KC_ID}/client-secret" \
  | sed 's/.*"value":"\([^"]*\)".*/\1/')

# 4. Register the tenant realm as an OIDC IdP in shared-apps.
#    hideOnLoginPage:true suppresses the IdP button; the redirector execution
#    (step 5) handles the auto-redirect so users see no login prompt.
IDP_ALIAS="${TENANT_REALM}"
IDP_BODY="{\"alias\":\"${IDP_ALIAS}\",\"displayName\":\"${IDP_ALIAS}\",\"providerId\":\"oidc\",\"enabled\":true,\"trustEmail\":true,\"firstBrokerLoginFlowAlias\":\"first broker login\",\"config\":{\"issuer\":\"${KERNEL_EXTERNAL_URL}/realms/${TENANT_REALM}\",\"authorizationUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${TENANT_REALM}/protocol/openid-connect/auth\",\"tokenUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${TENANT_REALM}/protocol/openid-connect/token\",\"jwksUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${TENANT_REALM}/protocol/openid-connect/certs\",\"userInfoUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${TENANT_REALM}/protocol/openid-connect/userinfo\",\"clientId\":\"${BROKER_CLIENT_ID}\",\"clientSecret\":\"${BROKER_SECRET}\",\"syncMode\":\"IMPORT\",\"useJwksUrl\":\"true\",\"validateSignature\":\"true\",\"defaultScope\":\"openid profile email\",\"hideOnLoginPage\":\"true\"}}"
IDP_HTTP=$(curl -s --max-time 30 -o /dev/null -w "%{http_code}" -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/shared-apps/identity-provider/instances/${IDP_ALIAS}")
if [ "${IDP_HTTP}" = "200" ]; then
  curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/shared-apps/identity-provider/instances/${IDP_ALIAS}" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" -d "${IDP_BODY}" >/dev/null
  echo "IdP ${IDP_ALIAS} updated in shared-apps realm"
else
  curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/admin/realms/shared-apps/identity-provider/instances" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" -d "${IDP_BODY}"
  echo "IdP ${IDP_ALIAS} registered in shared-apps realm"
fi

# 5. Configure identity-provider-redirector in shared-apps browser flow to
#    auto-redirect to the tenant realm IdP.
EXEC_ID=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/shared-apps/authentication/flows/browser/executions" \
  | grep -o '"id":"[^"]*"[^}]*"providerId":"identity-provider-redirector"' \
  | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
if [ -n "${EXEC_ID}" ]; then
  EXISTING_CFG=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/shared-apps/authentication/executions/${EXEC_ID}/config" 2>/dev/null || echo "")
  if echo "${EXISTING_CFG}" | grep -q '"alias"'; then
    CFG_ID=$(echo "${EXISTING_CFG}" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
    curl -sf --max-time 30 -X PUT \
      "${KEYCLOAK_URL}/admin/realms/shared-apps/authentication/config/${CFG_ID}" \
      -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      -d "{\"alias\":\"${TENANT_REALM}-redirector\",\"config\":{\"defaultProvider\":\"${IDP_ALIAS}\"}}" >/dev/null
    echo "identity-provider-redirector config updated in shared-apps"
  else
    curl -sf --max-time 30 -X POST \
      "${KEYCLOAK_URL}/admin/realms/shared-apps/authentication/executions/${EXEC_ID}/config" \
      -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      -d "{\"alias\":\"${TENANT_REALM}-redirector\",\"config\":{\"defaultProvider\":\"${IDP_ALIAS}\"}}" >/dev/null
    echo "identity-provider-redirector config created in shared-apps"
  fi
fi
# Belt-and-suspenders: also set the realm-level defaultProvider attribute.
curl -sf --max-time 30 -X PUT "${KEYCLOAK_URL}/admin/realms/shared-apps" \
  -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
  -d "{\"attributes\":{\"defaultProvider\":\"${IDP_ALIAS}\"}}" >/dev/null
echo "default provider set to ${IDP_ALIAS} in shared-apps realm"`
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

func buildAdminScript(realmName string) string {
	// The script:
	//   1. Authenticates to the master realm (cluster-admin creds from keycloakAdminSecret).
	//   2. Creates the tenant admin user in the tenant realm if absent.
	//   3. Sets the password and marks it temporary only when the tenant admin user
	//      is newly created (UPDATE_PASSWORD required action on first login).
	//   4. Grants the realm-management/realm-admin composite role so the user can
	//      manage users/groups/clients/sessions within this realm only.
	//
	// All steps are idempotent: users/roles are checked for existence before
	// POST so re-running the Job is safe.
	return fmt.Sprintf(`set -eu
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

# --- 2. Set password only for a newly created admin user ---
if [ "${CREATED}" = "1" ]; then
	curl -sf -X PUT -H "${AUTH_HEADER}" \
		-H "Content-Type: application/json" \
		"${KEYCLOAK_URL}/admin/realms/%s/users/${UID}/reset-password" \
		-d "{\"type\":\"password\",\"value\":\"${TENANT_ADMIN_PASSWORD}\",\"temporary\":false}"
	echo "password set (temporary=false)"
	echo "INITIAL_TENANT_ADMIN realm=%s username=${TENANT_ADMIN_USERNAME} password=${TENANT_ADMIN_PASSWORD}"
	echo "INITIAL_TENANT_ADMIN_RETRIEVE bao kv get -mount=secret -field=password gentian-os/tenants/${TENANT_NAME}/admin"
else
	echo "password reset skipped for existing tenant admin user"
fi

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
fi`,
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
// invalidating all active sessions. It also explicitly sets enabled:false on
// the tenant admin user in the shared kernel realm. This is necessary because
// Keycloak caches LDAP state (MAX_LIFESPAN policy) and would otherwise continue
// to authenticate the user even after UDM sets shadowExpire.
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
# Also disable tenant admin in kernel realm; the LDAP federation cache
# means Keycloak must be told directly rather than relying on shadowExpire.
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

func sharedAppsJobName(tenantName, appName string) string {
	return fmt.Sprintf("keycloak-shared-apps-%s-%s", tenantName, appName)
}

func realmDeleteJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-realm-delete-%s", tenantName)
}

func realmDisableJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-realm-disable-%s", tenantName)
}

// kernelAdminEnableJobName returns the name of the post-LDAP Keycloak
// re-enable job. This job runs after the LDAP admin-user job clears
// shadowExpire so the re-enable is durable against Keycloak LDAP re-imports.
func kernelAdminEnableJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-kernel-enable-%s", tenantName)
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

func jobIsFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
