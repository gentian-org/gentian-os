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
	"strings"
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
	keycloakProvisionerImage = "alpine:3.20"
	keycloakAdminSecret      = "keycloak-admin"
	appLabel                 = "gentianos.io/app"
	identityRequeueAfter     = 2 * time.Second
)

// realmLDAPParams holds LDAP federation parameters for the realm provisioning job.
// When nil, LDAP federation is not configured for the realm.
type realmLDAPParams struct {
	server   string // LDAP connection URL, e.g. ldap://host:389
	bindDN   string // bind account DN, e.g. uid=app-keycloak,ou=tenant,dc=...
	bindPW   string // bind account password (from OpenBao seeder)
	usersDN  string // users search base, e.g. ou=users,ou=tenant,dc=...
	groupsDN string // tenant OU where managed-by-attribute-* groups live
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

	oidcConfigs, err := r.collectOIDCAppConfigs(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.cleanupOrphanedOIDCClientJobs(ctx, tenant, oidcConfigs); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup orphaned OIDC client Jobs: %w", err)
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

	if len(oidcConfigs) > 0 {
		browserDone, err := r.ensureOIDCBrowserFlowJob(ctx, tenant, realmName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure OIDC browser flow Job: %w", err)
		}
		if !browserDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningBrowserFlow", "Waiting for OIDC browser flow Job to complete")
			return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
		}
		firstLoginDone, err := r.ensureBrokerFirstLoginFlowJob(ctx, tenant, realmName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure broker first-login flow Job: %w", err)
		}
		if !firstLoginDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningBrokerFirstLogin", "Waiting for broker first-login flow Job to complete")
			return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
		}
	}

	// OpenDesk OIDC packs map managed-by-attribute-* LDAP groups to client roles.
	// Those groups must exist in LDAP (OU Job) and be imported into Keycloak
	// (group-ldap-mapper sync) before client Jobs run — see docs/design/iam.md §1.3.
	if len(oidcConfigs) > 0 && r.LDAPBase != "" && oidcPacksNeedLDAPGroups(oidcConfigs) {
		ouDone, err := r.ldapManagedGroupsReady(ctx, tenant.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ouDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"WaitingLDAPOU", "Waiting for tenant LDAP OU and managed-by-attribute groups")
			return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
		}
		groupSyncDone, err := r.ensureKCLDAPGroupSyncJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Keycloak LDAP group sync Job: %w", err)
		}
		if !groupSyncDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"SyncingKCLDAPGroups", "Waiting for Keycloak LDAP group sync before OIDC client Jobs")
			return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
		}
	}

	allDone := true
	for _, cfg := range oidcConfigs {
		done, err := r.ensureOIDCClientJob(ctx, tenant, realmName, cfg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Keycloak OIDC Job for app %s: %w", cfg.profileName, err)
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
		// If LDAP federation is enabled globally, we must wait for the LDAP reconciler
		// to create and complete the admin-user job. Otherwise we race against Keycloak
		// federation syncs.
		if r.LDAPBase != "" {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"WaitingLDAPAdminUnlock", "Waiting for LDAP admin-user job to be created")
			return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
		}
		// Proceed without waiting only if LDAP is completely disabled.
	case ldapJobErr != nil:
		return ctrl.Result{}, ldapJobErr
	case !jobIsComplete(adminLDAPJob):
		r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
			"WaitingLDAPAdminUnlock", "Waiting for LDAP admin-user job to complete before re-enabling kernel realm user")
		return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
	}

	// Re-enable the kernel realm user and trigger an LDAP sync only when a
	// kernel Keycloak realm is configured. When KernelRealm is empty (test
	// environments, or deployments without Keycloak) there is no kernel realm
	// user to re-enable and no LDAP provider to sync, so skip these steps.
	if r.KernelRealm != "" {
		adminEmail := tenant.Spec.AdminEmail
		if adminEmail == "" {
			adminEmail = fmt.Sprintf("admin-%s@gentian.org", tenant.Name)
		}
		opendeskEnableDone, err := r.ensureOpendeskAdminEnableJob(ctx, tenant, adminEmail)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure kernel admin re-enable Job: %w", err)
		}
		if !opendeskEnableDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"ProvisioningOpendeskEnable", "Waiting for opendesk admin enable Job to complete")
			return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
		}

		kernelLDAPSyncDone, err := r.ensureKernelLDAPSyncJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure kernel Keycloak LDAP sync Job: %w", err)
		}
		if !kernelLDAPSyncDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"SyncingKernelLDAP", "Waiting for kernel Keycloak LDAP sync before portal login")
			return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
		}

		// Trigger a full Keycloak LDAP sync after all LDAP provisioning is stable.
		// This re-imports all users with their current LDAP attributes, clearing any
		// cached enabled=false state caused by the brief UDM shadowExpire race during
		// user creation (the univention-ldap-mapper sets isEnabled()=false while
		// shadowExpire=1 is set, and the cached state persists until a sync refreshes it).
		kcLDAPSyncDone, err := r.ensureKCLDAPSyncJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Keycloak LDAP sync Job: %w", err)
		}
		if !kcLDAPSyncDone {
			r.setCondition(tenant, conditionIdentityReady, metav1.ConditionFalse,
				"SyncingKCLDAP", "Waiting for Keycloak LDAP sync Job to complete")
			return ctrl.Result{RequeueAfter: identityRequeueAfter}, nil
		}
	}

	r.setCondition(tenant, conditionIdentityReady, metav1.ConditionTrue,
		"Provisioned", "Keycloak realm and OIDC clients are ready")
	return ctrl.Result{}, nil
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
				server:   r.LDAPServer,
				bindDN:   bindDN,
				bindPW:   creds.BindPassword,
				usersDN:  "ou=users," + ouDN,
				groupsDN: ouDN,
			}
		}
		var broker *realmBrokerParams
		if r.KernelRealm != "" && r.KernelDomain != "" {
			broker = &realmBrokerParams{
				kernelRealm:       r.KernelRealm,
				kernelExternalURL: fmt.Sprintf("https://id.%s", r.KernelDomain),
			}
		}
		return false, r.Create(ctx, makeRealmJob(tenant, realmName, r.KernelDomain, ldap, broker))
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
// stored write-once at gentian-os/tenants/<tenant>/admin in OpenBao. The
// provision Job always syncs that canonical password into Keycloak (including
// redeploys after deletionPolicy=Retain) and emits INITIAL_TENANT_ADMIN once.
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
func (r *TenantReconciler) ensureClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName, appName, clientID string, redirectURIs []string) (bool, error) {
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
			creds, seedErr := r.Seeder.SeedOIDC(ctx, tenant.Name, appName, issuer, clientID)
			if seedErr != nil {
				return false, fmt.Errorf("seed oidc: %w", seedErr)
			}
			clientSecret = creds.ClientSecret
		}
		return false, r.Create(ctx, makeClientJob(tenant, realmName, appName, clientID, redirectURIs, clientSecret))
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
			provNames := []string{
				realmJobName(tenant.Name), adminJobName(tenant.Name),
				oidcBrowserFlowJobName(tenant.Name),
				kernelAdminEnableJobName(tenant.Name), kernelLDAPSyncJobName(tenant.Name),
				kcLDAPGroupSyncJobName(tenant.Name), kcLDAPSyncJobName(tenant.Name),
				mbaGroupsJobName(tenant.Name),
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

func makeRealmJob(tenant *gentianov1alpha1.Tenant, realmName, kernelDomain string, ldap *realmLDAPParams, broker *realmBrokerParams) *batchv1.Job {
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
			corev1.EnvVar{Name: "LDAP_GROUPS_DN", Value: ldap.groupsDN},
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

func makeClientJob(tenant *gentianov1alpha1.Tenant, realmName, appName, clientID string, redirectURIs []string, clientSecret string) *batchv1.Job {
	ttl := int32(3600)
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

func makeAdminJob(tenant *gentianov1alpha1.Tenant, realmName string, creds secrets.TenantAdminCreds) *batchv1.Job {
	ttl := int32(3600)
	container := keycloakContainer("provision-tenant-admin", buildAdminScript(realmName))
	adminEmail := tenant.Spec.AdminEmail
	if adminEmail == "" {
		adminEmail = creds.Username + "@gentian.org"
	}
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "TENANT_NAME", Value: tenant.Name},
		corev1.EnvVar{Name: "TENANT_ADMIN_USERNAME", Value: creds.Username},
		corev1.EnvVar{Name: "TENANT_ADMIN_PASSWORD", Value: creds.Password},
		corev1.EnvVar{Name: "TENANT_ADMIN_EMAIL", Value: adminEmail},
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

func makeOpendeskAdminEnableJob(tenant *gentianov1alpha1.Tenant, adminEmail, kernelRealm string) *batchv1.Job {
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
						keycloakContainer("re-enable-kernel-admin", buildOpendeskAdminEnableScript(adminEmail, kernelRealm)),
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
func (r *TenantReconciler) ensureOpendeskAdminEnableJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, adminEmail string) (bool, error) {
	jobName := kernelAdminEnableJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, makeOpendeskAdminEnableJob(tenant, adminEmail, r.KernelRealm))
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
// Alpine-based Keycloak provisioner image (wget + jq). Credentials are injected
// from the well-known keycloak-admin Secret in the kernel namespace.
func keycloakContainer(name, script string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   keycloakProvisionerImage,
		Command: []string{"/bin/sh", "-c", keycloakProvisionerBootstrap + script},
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
const (
	realmScriptLDAPIDPlaceholder   = "__GENTIAN_LDAP_ID_BLOCK__"
	realmScriptBrokerIDPlaceholder = "__GENTIAN_BROKER_ID_BLOCK__"
)

func buildRealmScript(realmName, displayName string) string {
	ldapIDBlock := keycloakShellRequireID("LDAP_ID", "${LDAP_COMPONENTS}", "name", "ldap")
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
    -d '{"realm":"%s","enabled":true,"displayName":"%s","registrationAllowed":false,"browserSecurityHeaders":`+keycloakBrowserSecurityHeadersJSON+`}'
  echo "realm %s created"
else
  curl -sf \
    -X PUT "${KEYCLOAK_URL}/admin/realms/%s" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"realm":"%s","enabled":true,"browserSecurityHeaders":`+keycloakBrowserSecurityHeadersJSON+`}'
  echo "realm %s already exists, ensured enabled=true and browserSecurityHeaders (was HTTP ${HTTP})"
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

  # Sync managed-by-attribute-* groups from the tenant OU for OIDC role mapping.
  if [ -n "${LDAP_GROUPS_DN:-}" ]; then
    LDAP_COMPONENTS=$(curl -sf \
      -H "Authorization: Bearer ${TOKEN}" \
      "${KEYCLOAK_URL}/admin/realms/%s/components?type=org.keycloak.storage.UserStorageProvider")
`+realmScriptLDAPIDPlaceholder+`
    GROUP_MAPPERS=$(curl -sf \
      -H "Authorization: Bearer ${TOKEN}" \
      "${KEYCLOAK_URL}/admin/realms/%s/components?parent=${LDAP_ID}&type=org.keycloak.storage.ldap.mappers.LDAPStorageMapper" || echo "[]")
    if echo "${GROUP_MAPPERS}" | grep -q '"name":"group-mapper"'; then
      echo "LDAP group-mapper already registered in realm %s"
    else
      curl -sf \
        -X POST "${KEYCLOAK_URL}/admin/realms/%s/components" \
        -H "Authorization: Bearer ${TOKEN}" \
        -H "Content-Type: application/json" \
        -d "{
          \"name\":\"group-mapper\",
          \"providerId\":\"group-ldap-mapper\",
          \"providerType\":\"org.keycloak.storage.ldap.mappers.LDAPStorageMapper\",
          \"parentId\":\"${LDAP_ID}\",
          \"config\":{
            \"groups.dn\":[\"${LDAP_GROUPS_DN}\"],
            \"group.name.ldap.attribute\":[\"cn\"],
            \"group.object.classes\":[\"univentionGroup\"],
            \"groups.ldap.filter\":[\"(&(cn=managed-by-attribute*)(objectClass=univentionGroup))\"],
            \"membership.attribute.type\":[\"DN\"],
            \"membership.ldap.attribute\":[\"uniqueMember\"],
            \"membership.user.ldap.attribute\":[\"uid\"],
            \"mode\":[\"LDAP_ONLY\"],
            \"ignore.missing.groups\":[\"true\"],
            \"drop.non.existing.groups.during.sync\":[\"false\"]
          }
        }"
      echo "LDAP group-mapper registered in realm %s (groupsDn=${LDAP_GROUPS_DN})"
      curl -sf -X POST -H "Authorization: Bearer ${TOKEN}" \
        "${KEYCLOAK_URL}/admin/realms/%s/user-storage/${LDAP_ID}/sync?action=triggerFullSync" >/dev/null 2>&1 || true
    fi
  fi

  ensure_ldap_uid_attribute_mapper "%s" "ldap"
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

  # 2. Register kernel as an OIDC Identity Provider in the tenant realm (idempotent).
  #    hideOnLoginPage:true prevents showing the "Login with Gentian SSO" button
  #    redundantly; the defaultProvider setting (step 3) handles the auto-redirect.
  IDP_HTTP=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" -H "Authorization: Bearer ${TOKEN}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM_NAME}/identity-provider/instances/kernel")
  # Server-side OIDC endpoints use KEYCLOAK_URL (cluster-internal) so broker code
  # exchange does not hairpin through the public id.<kernel> URL.
  IDP_BODY="{\"alias\":\"kernel\",\"displayName\":\"Gentian SSO\",\"providerId\":\"oidc\",\"enabled\":true,\"trustEmail\":true,\"firstBrokerLoginFlowAlias\":\"first broker login\",\"config\":{\"issuer\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}\",\"authorizationUrl\":\"${KERNEL_EXTERNAL_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/auth\",\"tokenUrl\":\"${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/token\",\"jwksUrl\":\"${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/certs\",\"userInfoUrl\":\"${KEYCLOAK_URL}/realms/${KERNEL_REALM}/protocol/openid-connect/userinfo\",\"clientId\":\"${BROKER_CLIENT_ID}\",\"clientSecret\":\"${BROKER_SECRET}\",\"syncMode\":\"IMPORT\",\"useJwksUrl\":\"true\",\"validateSignature\":\"true\",\"defaultScope\":\"openid profile email\",\"hideOnLoginPage\":\"true\"}}"
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
ensure_ldap_uid_attribute_mapper "${KERNEL_REALM}" "ldap-provider"
` + brokerKernelClientUsernameMapperShell + brokerIdPUsernameImporterShell + `
  # (No defaultProvider is set on the identity-provider-redirector execution.
  #  Tenant users sign in at the shared kernel portal (SUBTREE LDAP federation
  #  on mailPrimaryAddress). The kernel IdP registered above remains available
  #  for explicit kc_idp_hint=kernel flows when apps need a brokered session.)
fi`, realmName, realmName, displayName, realmName, realmName, realmName, realmName,
		realmName, realmName, realmName, realmName,
		realmName, realmName, realmName, realmName, realmName, realmName, realmName)
	script = strings.ReplaceAll(script, realmScriptLDAPIDPlaceholder, ldapIDBlock)
	script = strings.ReplaceAll(script, realmScriptBrokerIDPlaceholder, brokerResolveID)
	// Realm Job registers the kernel IdP with the built-in "first broker login" flow.
	// The custom first-broker-login-gentian flow is created later (dedicated Job or
	// broker-idp Job) before the IdP alias is switched — see docs/design/iam.md.
	return keycloakShellJSONIDExtractor() + ensureLDAPUIDAttributeMapperShell + script
}

// buildOpendeskAdminEnableScript re-enables the tenant admin user in the shared
// kernel Keycloak realm. This job is intentionally separate from
// buildRealmScript so it only runs after the LDAP admin-user job has cleared
// shadowExpire, preventing Keycloak's next LDAP import from overriding the
// re-enable with the previously-locked LDAP state.
//
// The kernel realm uses mailPrimaryAddress as the Keycloak username, so lookup
// is by email (portal login identifier), not uid=admin-<tenant>.
func buildOpendeskAdminEnableScript(adminEmail, kernelRealm string) string {
	return fmt.Sprintf(`set -eu
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
USER_RESP=$(curl -sf -H "Authorization: Bearer ${TOKEN}" \
  "${KEYCLOAK_URL}/admin/realms/%s/users?email=%s&exact=true" || echo "")
if echo "${USER_RESP}" | grep -q '"id"'; then
  UID=$(echo "${USER_RESP}" | sed 's/.*"id":"\([^"]*\)".*/\1/')
  curl -sf -X PUT -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}" \
    -d '{"enabled":true}'
  echo "admin %s re-enabled in %s realm (LDAP shadowExpire already cleared)"
else
  echo "admin %s not found in %s realm (first deploy, no action needed)"
fi`, kernelRealm, adminEmail, kernelRealm, adminEmail, kernelRealm, adminEmail, kernelRealm)
}

// buildKCLDAPSyncScript triggers a full Keycloak LDAP user import after admin
// unlock. See buildKCLDAPFederationSyncScript for the shared preamble.
func buildKCLDAPSyncScript(realmName string) string {
	return buildKCLDAPFederationSyncScript(realmName, true, false)
}

// buildKCLDAPGroupSyncScript imports managed-by-attribute-* groups from the
// tenant OU into Keycloak before OpenDesk OIDC pack Jobs map them to client roles.
func buildKCLDAPGroupSyncScript(realmName string) string {
	return buildKCLDAPFederationSyncScript(realmName, false, true)
}

func buildKCLDAPFederationSyncScript(realmName string, syncUsers, syncGroups bool) string {
	var steps string
	if syncGroups {
		steps += `
MAPPERS=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/components?parent=${PROVIDER_ID}&type=org.keycloak.storage.ldap.mappers.LDAPStorageMapper")
MAPPER_ID=$(printf '%s' "${MAPPERS}" | jq -r '.[] | select(.name=="group-mapper") | .id' 2>/dev/null | head -1)
if [ -z "${MAPPER_ID}" ] || [ "${MAPPER_ID}" = "null" ]; then
  echo "group-mapper not found in realm ${REALM}" >&2
  exit 1
fi
RESULT=$(curl -sf -X POST -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/user-storage/${PROVIDER_ID}/mappers/${MAPPER_ID}/sync?direction=fedToKeycloak")
echo "Keycloak LDAP group sync complete for realm ${REALM}: ${RESULT}"
`
	}
	if syncUsers {
		steps += `
RESULT=$(curl -sf -X POST -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/user-storage/${PROVIDER_ID}/sync?action=triggerFullSync")
echo "Keycloak LDAP user sync complete for realm ${REALM}: ${RESULT}"
`
	}
	return fmt.Sprintf(`set -eu
REALM=%q
TOKEN=$(curl -sf \
  -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=admin-cli&username=${KEYCLOAK_ADMIN_USERNAME}&password=${KEYCLOAK_ADMIN_PASSWORD}&grant_type=password" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
AUTH_HEADER="Authorization: Bearer ${TOKEN}"
PROVIDER_ID=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/components?type=org.keycloak.storage.UserStorageProvider" \
  | jq -r '.[0].id // empty' 2>/dev/null | head -1)
if [ -z "${PROVIDER_ID}" ]; then
  PROVIDER_ID=$(curl -sf -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components?type=org.keycloak.storage.UserStorageProvider" \
    | sed 's/.*"id":"\([^"]*\)".*/\1/')
fi
if [ -z "${PROVIDER_ID}" ]; then
  echo "LDAP provider not found in ${REALM} realm — skipping sync"
  exit 0
fi
%s`, realmName, steps)
}

// makeKCLDAPGroupSyncJob imports LDAP groups before OpenDesk OIDC pack Jobs.
func makeKCLDAPGroupSyncJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
	return makeKCLDAPFederationSyncJob(tenant, kcLDAPGroupSyncJobName(tenant.Name), "kc-ldap-group-sync",
		buildKCLDAPGroupSyncScript(realmName))
}

// makeKCLDAPSyncJob returns the Job that triggers a Keycloak LDAP user sync in
// the tenant realm after admin provisioning is stable.
func makeKCLDAPSyncJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
	return makeKCLDAPFederationSyncJob(tenant, kcLDAPSyncJobName(tenant.Name), "kc-ldap-sync",
		buildKCLDAPSyncScript(realmName))
}

// makeKernelLDAPSyncJob returns the Job that re-imports LDAP users into the
// shared kernel realm for portal login (iam.md §1.2).
func makeKernelLDAPSyncJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
	return makeKCLDAPFederationSyncJob(tenant, kernelLDAPSyncJobName(tenant.Name), "kernel-ldap-sync",
		buildKCLDAPSyncScript(realmName))
}

func makeKCLDAPFederationSyncJob(tenant *gentianov1alpha1.Tenant, jobName, containerName, script string) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
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
						keycloakContainer(containerName, script),
					},
				},
			},
		},
	}
}

// ensureKCLDAPGroupSyncJob creates the pre-OIDC LDAP group import job.
// If managed-by-attribute groups were backfilled after the last sync (e.g. tenant
// upgrade or new OpenDesk OIDC packs), the completed sync Job is deleted so LDAP
// groups are re-imported before OIDC client Jobs run.
func (r *TenantReconciler) ensureKCLDAPGroupSyncJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	jobName := kcLDAPGroupSyncJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if err == nil && jobIsComplete(job) {
		stale, staleErr := r.kcLDAPGroupSyncStaleAfterManagedGroups(ctx, tenant.Name, job)
		if staleErr != nil {
			return false, staleErr
		}
		if stale {
			prop := metav1.DeletePropagationBackground
			if delErr := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); delErr != nil {
				return false, delErr
			}
			return false, nil
		}
	}
	if err != nil && !errors.IsNotFound(err) {
		return false, err
	}
	return r.ensureKCLDAPFederationSyncJob(ctx, tenant, jobName,
		func() *batchv1.Job { return makeKCLDAPGroupSyncJob(tenant, keycloakRealmName(tenant)) })
}

// kcLDAPGroupSyncStaleAfterManagedGroups reports whether LDAP OU or managed-by-attribute
// group Jobs finished after the last Keycloak LDAP group sync.
func (r *TenantReconciler) kcLDAPGroupSyncStaleAfterManagedGroups(ctx context.Context, tenantName string, groupSync *batchv1.Job) (bool, error) {
	for _, sourceName := range []string{ouJobName(tenantName), mbaGroupsJobName(tenantName)} {
		source := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: sourceName, Namespace: kernelNamespace}, source)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if jobIsComplete(source) && jobCompletedAfter(groupSync, source) {
			return true, nil
		}
	}
	return false, nil
}

// ldapManagedGroupsReady reports whether the UDM OU Job and the managed-by-attribute
// group backfill Job have both completed successfully.
func (r *TenantReconciler) ldapManagedGroupsReady(ctx context.Context, tenantName string) (bool, error) {
	for _, jobName := range []string{ouJobName(tenantName), mbaGroupsJobName(tenantName)} {
		done, err := r.kernelJobSucceeded(ctx, jobName)
		if err != nil || !done {
			return false, err
		}
	}
	return true, nil
}

func (r *TenantReconciler) kernelJobSucceeded(ctx context.Context, jobName string) (bool, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		return false, nil
	}
	return jobIsComplete(job), nil
}

func (r *TenantReconciler) ensureKCLDAPFederationSyncJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, jobName string, makeJob func() *batchv1.Job) (bool, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, makeJob())
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

// ensureKCLDAPSyncJob creates the Keycloak LDAP full-sync job if absent and
// returns true when it has completed. Called after the admin-enable job so
// all LDAP changes (including shadowExpire clearance) are stable in LDAP
// before the sync re-imports users into Keycloak.
func (r *TenantReconciler) ensureKCLDAPSyncJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	return r.ensureKCLDAPFederationSyncJob(ctx, tenant, kcLDAPSyncJobName(tenant.Name),
		func() *batchv1.Job { return makeKCLDAPSyncJob(tenant, keycloakRealmName(tenant)) })
}

// ensureKernelLDAPSyncJob re-imports LDAP users into the shared kernel realm so
// portal login sees up-to-date enabled state and mailPrimaryAddress usernames.
func (r *TenantReconciler) ensureKernelLDAPSyncJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	return r.ensureKCLDAPFederationSyncJob(ctx, tenant, kernelLDAPSyncJobName(tenant.Name),
		func() *batchv1.Job { return makeKernelLDAPSyncJob(tenant, r.KernelRealm) })
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
	//   3. Always syncs the OpenBao-canonical password and clears stale requiredActions.
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

# --- 2. Sync OpenBao-canonical password (idempotent; required after Retain redeploy) ---
# LDAP-federated users cannot use reset-password; portal auth uses kernel LDAP + UDM.
USER_JSON=$(curl -sf -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}")
FEDERATED=0
if echo "${USER_JSON}" | grep -q '"federationLink"'; then
  FEDERATED=1
  echo "tenant admin ${TENANT_ADMIN_USERNAME} is LDAP-federated; password is managed in UDM (skip Keycloak reset-password)"
else
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
fi
if [ "${FEDERATED}" = "1" ]; then
  echo "tenant admin user attributes managed by LDAP federation (skip Keycloak user PUT)"
else
  curl -sf -X PUT -H "${AUTH_HEADER}" \
    -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/%s/users/${UID}" \
    -d "{\"enabled\":true,\"email\":\"${TENANT_ADMIN_EMAIL}\",\"requiredActions\":[]}"
  echo "tenant admin user enabled; requiredActions cleared; email=${TENANT_ADMIN_EMAIL}"
fi
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
fi`,
		realmName, realmName, realmName, realmName, realmName,
		realmName, realmName, realmName, realmName, realmName,
		realmName, realmName, realmName)
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

// kernelLDAPSyncJobName returns the kernel-realm LDAP user sync job used for
// shared-portal login (iam.md §1.2). Distinct from kcLDAPSyncJobName, which
// targets the tenant realm for per-app OIDC.
func kernelLDAPSyncJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-kernel-ldap-sync-%s", tenantName)
}

// kcLDAPGroupSyncJobName returns the Keycloak LDAP group import job run after
// the OU Job and before OpenDesk OIDC pack Jobs.
func kcLDAPGroupSyncJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-ldap-group-sync-%s", tenantName)
}

// kcLDAPSyncJobName returns the name of the Keycloak LDAP user sync job.
// This job runs after the admin-enable job so all LDAP users (not just the
// admin) are re-imported with their correct enabled state, clearing any cached
// disabled entries caused by the brief UDM shadowExpire race during provisioning.
func kcLDAPSyncJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-ldap-sync-%s", tenantName)
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
