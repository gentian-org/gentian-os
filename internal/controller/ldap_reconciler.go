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
	"sort"
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
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	conditionLDAPReady  = "LDAPReady"
	udmProvisionerImage = "curlimages/curl:8.7.1"
	udmAdminSecret      = "udm-admin"

	// annotationProvisionedPortalTiles tracks which portal tile names have been
	// provisioned in LDAP for a tenant. Used to detect and clean up stale entries
	// when apps are removed from a tenant's profile.
	annotationProvisionedPortalTiles = "gentian.org/provisioned-portal-tiles"
	ldapRequeueAfter                 = 2 * time.Second
)

// ensureLDAP provisions per-tenant LDAP organisational units, default groups,
// delegated admin user/policy, and per-app bind accounts via the UDM REST API.
// Jobs run in the kernel namespace and are idempotent (check-before-create).
// Returns a non-zero RequeueAfter while Jobs are still running.
//
// Steps 1-3 (OU, admin user, admin policy, bind accounts) are handled
// non-blocking by ensureLDAPBase for tenants with no LDAP-requiring apps.
// When LDAP apps are present, this function handles all steps and blocks
// Phase=Ready until complete. Step 4 (per-app bind accounts) is app-gated.
//
// Admin user must run BEFORE admin policy: the policy job updates the portal
// entry allowedGroups, and the Nubus portal consumer's groups cache must
// already contain the admin user in admins_<tenant> at that point. If the
// policy job ran first (old order), the portal server would see the group in
// allowedGroups but find it empty in the cache, so admin tiles would not show
// until the user reloaded the portal after the subsequent user job completed.
func (r *TenantReconciler) ensureLDAP(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	ouDN := tenantOUDN(tenant)

	// Short-circuit when no app requires LDAP. ensureLDAPBase (non-blocking)
	// handles OU+admin provisioning for these tenants independently.
	ldapApps, err := r.collectLDAPApps(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	// B.1: Always provision a Keycloak LDAP bind account when LDAP federation
	// is configured (r.LDAPBase != ""). This account is used by the tenant realm's
	// LDAP User Storage Provider regardless of which apps the tenant has enabled.
	if r.LDAPBase != "" {
		ldapApps = append(ldapApps, "keycloak")
	}

	if len(ldapApps) == 0 {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionTrue,
			"NoLDAPRequired", "No apps require LDAP provisioning")
		return ctrl.Result{}, nil
	}

	// Step 1 — tenant OU + default groups
	ouDone, err := r.ensureOUJob(ctx, tenant, ouDN)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure LDAP OU Job: %w", err)
	}
	if !ouDone {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionFalse,
			"ProvisioningOU", "Waiting for UDM OU Job to complete")
		return ctrl.Result{RequeueAfter: ldapRequeueAfter}, nil
	}

	// Step 1b — tenant-scoped App User template with username@<tenant-domain> prefill.
	appUserTemplateDone, err := r.ensureAppUserTemplateJob(ctx, tenant, ouDN)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure LDAP App User template Job: %w", err)
	}
	if !appUserTemplateDone {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionFalse,
			"ProvisioningAppUserTemplate", "Waiting for UDM App User template Job to complete")
		return ctrl.Result{RequeueAfter: ldapRequeueAfter}, nil
	}

	// Step 2 — tenant admin UDM user.
	// Must run BEFORE the admin policy job: the policy job updates the portal
	// entry allowedGroups, and the Nubus portal consumer groups cache must
	// already contain the admin user so that portal tile visibility is correct
	// on first login (no page reload required).
	var adminCreds secrets.TenantAdminCreds
	if r.Seeder != nil {
		adminCreds, err = r.Seeder.SeedTenantAdmin(ctx, tenant.Name)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("seed tenant admin for UDM: %w", err)
		}
	} else {
		adminCreds = secrets.TenantAdminCreds{Username: "admin-" + tenant.Name, Password: "placeholder"}
	}
	adminUserDone, err := r.ensureAdminUserJob(ctx, tenant, ouDN, adminCreds)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure LDAP admin user Job: %w", err)
	}
	if !adminUserDone {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionFalse,
			"ProvisioningAdminUser", "Waiting for UDM admin user Job to complete")
		return ctrl.Result{RequeueAfter: ldapRequeueAfter}, nil
	}

	// Step 3 — tenant-scoped delegated admin policy + portal tile allowedGroups.
	// Runs after the admin user so the groups cache is already populated.
	adminPolicyDone, err := r.ensureAdminPolicyJob(ctx, tenant, ouDN)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure LDAP admin policy Job: %w", err)
	}
	if !adminPolicyDone {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionFalse,
			"ProvisioningAdminPolicy", "Waiting for UDM admin policy Job to complete")
		return ctrl.Result{RequeueAfter: ldapRequeueAfter}, nil
	}

	// Step 3 — per-app bind accounts (ldapApps already collected at top of function)
	allDone := true
	for _, appName := range ldapApps {
		done, err := r.ensureBindAccountJob(ctx, tenant, ouDN, appName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure LDAP bind account Job for app %s: %w", appName, err)
		}
		if !done {
			allDone = false
		}
	}

	// Step 4 — per-tenant portal entries for dedicated apps with central-navigation:nubus.
	// Creates a per-tenant UDM portal tile that points to the tenant-specific URL so
	// users land on the correct dedicated instance instead of the shared kernel tile.
	portalApps, err := r.collectDedicatedPortalApps(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("collect dedicated portal apps: %w", err)
	}
	expectedTiles := make(map[string]struct{}, len(portalApps))
	for _, pa := range portalApps {
		expectedTiles[pa.AppName] = struct{}{}
	}
	// Remove portal entries that were provisioned previously but are no longer expected.
	staleDone, err := r.deleteStalePortalEntriesForTenant(ctx, tenant, expectedTiles)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("delete stale portal entries: %w", err)
	}
	if !staleDone {
		allDone = false
	}
	for _, pa := range portalApps {
		done, err := r.ensurePortalEntryJob(ctx, tenant, ouDN, pa)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure portal entry Job for app %s: %w", pa.AppName, err)
		}
		if done {
			if trackErr := r.addProvisionedPortalTile(ctx, tenant, pa.AppName); trackErr != nil {
				return ctrl.Result{}, trackErr
			}
		} else {
			allDone = false
		}
	}

	// Step 4b — per-tenant portal contact deep links (swp.realtime_*_<tenant>).
	// Entries are scoped to each tenant LDAP OU so users receive meet/chat URLs
	// for their tenant zone (flat on single-tenancy clusters, prefixed in multi).
	meetURL, chatURL := r.portalRealtimeLinkTargets(tenant)
	if meetURL != "" || chatURL != "" {
		rtDone, err := r.ensurePortalRealtimeLinksJob(ctx, tenant, ouDN, meetURL, chatURL)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure portal realtime links Job: %w", err)
		}
		if !rtDone {
			allDone = false
		}
	}

	if !allDone {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionFalse,
			"ProvisioningBindAccounts", "Waiting for UDM bind account Jobs to complete")
		return ctrl.Result{RequeueAfter: ldapRequeueAfter}, nil
	}

	r.setCondition(tenant, conditionLDAPReady, metav1.ConditionTrue,
		"Provisioned", "LDAP OU, admin user/policy, bind accounts, and portal entries are ready")
	return ctrl.Result{}, nil
}

// collectLDAPApps returns the profile names of apps that declare
// kernelRequirements.identity.ldap (non-nil).
func (r *TenantReconciler) collectLDAPApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	var ldapApps []string
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.KernelRequirements != nil &&
			profile.Spec.KernelRequirements.Identity != nil &&
			profile.Spec.KernelRequirements.Identity.LDAP != nil {
			ldapApps = append(ldapApps, app.Profile)
		}
	}
	return ldapApps, nil
}

// dedicatedPortalApp holds the resolved parameters for a single portal tile
// that a dedicated-mode app contributes to the tenant's Nubus/gentian-ui portal.
// Each PortalTileSpec in an AppProfile produces one dedicatedPortalApp.
type dedicatedPortalApp struct {
	// AppName is the tile name (= portal entry CN suffix: swp.{AppName}_{tenant}).
	AppName string
	// ProfileName is the AppProfile name; used as the appLabel on the Job.
	ProfileName    string
	SubDomain      string
	LinkSuffix     string
	DisplayNameDE  string
	DisplayNameEN  string
	LinkTarget     string
	AllowedGroupCN string // LDAP CN resolved to full DN in the shell script
	Logo           string // base64-encoded SVG without the data URI prefix
}

// collectDedicatedPortalApps returns one dedicatedPortalApp per PortalTileSpec
// across all dedicated-mode apps in the tenant that declare portal tiles and
// have an ingress.subDomain (needed to form the tile base URL).
func (r *TenantReconciler) collectDedicatedPortalApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]dedicatedPortalApp, error) {
	var result []dedicatedPortalApp
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if len(profile.Spec.PortalTiles) == 0 {
			continue
		}
		if profile.Spec.Ingress == nil || profile.Spec.Ingress.SubDomain == "" {
			continue
		}
		for _, tile := range profile.Spec.PortalTiles {
			allowedGroupCN := tile.AllowedGroup
			if allowedGroupCN == "" {
				allowedGroupCN = "App Users"
			}
			linkTarget := string(tile.LinkTarget)
			if linkTarget == "" {
				linkTarget = "newwindow"
			}
			deDE := tile.DisplayName["de_DE"]
			enUS := tile.DisplayName["en_US"]
			if enUS == "" {
				enUS = deDE
			}
			if deDE == "" {
				deDE = enUS
			}
			tileLogo := tile.Logo
			if tileLogo == "" {
				tileLogo = profile.Spec.Logo
			}
			result = append(result, dedicatedPortalApp{
				AppName:        tile.Name,
				ProfileName:    app.Profile,
				SubDomain:      profile.Spec.Ingress.SubDomain,
				LinkSuffix:     tile.LinkSuffix,
				DisplayNameDE:  deDE,
				DisplayNameEN:  enUS,
				LinkTarget:     linkTarget,
				AllowedGroupCN: allowedGroupCN,
				Logo:           strings.TrimPrefix(tileLogo, "data:image/svg+xml;base64,"),
			})
		}
	}
	return result, nil
}

// ensurePortalEntryJob creates the per-tenant portal entry UDM Job for one tile.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensurePortalEntryJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, ouDN string, pa dedicatedPortalApp) (bool, error) {
	// First, clean up any stale delete job for this tile.
	deleteJobName := portalEntryDeleteJobName(tenant.Name, pa.AppName)
	deleteJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: deleteJobName, Namespace: kernelNamespace}, deleteJob); err == nil && deleteJob.DeletionTimestamp.IsZero() {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, deleteJob, &client.DeleteOptions{PropagationPolicy: &prop})
		// A delete job existed, which means the app was uninstalled recently.
		// Any existing create job is stale and must be recreated to ensure the portal entry is actually created.
		createJobName := portalEntryJobName(tenant.Name, pa.AppName)
		createJob := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: createJobName, Namespace: kernelNamespace}, createJob); err == nil && createJob.DeletionTimestamp.IsZero() {
			_ = r.Delete(ctx, createJob, &client.DeleteOptions{PropagationPolicy: &prop})
		}
		return false, nil
	}

	jobName := portalEntryJobName(tenant.Name, pa.AppName)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		tenantDomain := r.tenantEffectiveDomain(tenant)
		return false, r.Create(ctx, makePortalEntryJob(tenant, ouDN, pa, tenantDomain))
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

// ensureOUJob creates the UDM OU + groups Job if absent.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureOUJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, ouDN string) (bool, error) {
	jobName := ouJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, makeOUJob(tenant, ouDN))
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

// ensureAdminPolicyJob creates the delegated-admin policy Job if absent.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureAdminPolicyJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, ouDN string) (bool, error) {
	jobName := adminPolicyJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, makeAdminPolicyJob(tenant, ouDN))
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

// ensureAppUserTemplateJob creates the tenant-scoped App User UDM template if absent.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureAppUserTemplateJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, ouDN string) (bool, error) {
	jobName := appUserTemplateJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		mailDomain := tenantUserMailDomain(tenant, r.KernelDomain, r.TenancyMode)
		if mailDomain == "" {
			return false, fmt.Errorf("tenant %q has no effective domain for App User mail prefill", tenant.Name)
		}
		return false, r.Create(ctx, makeAppUserTemplateJob(tenant, ouDN, mailDomain))
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

// ensureAdminUserJob creates the UDM users/user Job for the tenant admin if absent.
// The admin is added to admins_<tenant> so the UMC delegated admin policy takes effect.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureAdminUserJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, ouDN string, creds secrets.TenantAdminCreds) (bool, error) {
	jobName := adminUserJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		// Delete any stale cleanup jobs so the next undeploy creates fresh ones.
		r.deleteProvisioningJobs(ctx,
			adminUserDeleteJobName(tenant.Name),
			ouDeleteJobName(tenant.Name),
			ldapLockJobName(tenant.Name),
		)
		return false, r.Create(ctx, makeAdminUserJob(tenant, ouDN, creds))
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

// ensureBindAccountJob creates the UDM bind account Job for one app if absent.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureBindAccountJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, ouDN, appName string) (bool, error) {
	jobName := bindAccountJobName(tenant.Name, appName)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		// Inc 21a: derive the per-app LDAP bind password and persist it under
		// the canonical OpenBao path before creating the UDM Job. The Job
		// receives the same value via BIND_PW so live LDAP and OpenBao stay
		// in lockstep. When Seeder is nil the Job falls back to a local random.
		bindPassword := ""
		if r.Seeder != nil {
			creds, seedErr := r.Seeder.SeedLDAP(ctx, tenant.Name, appName, secrets.LDAPCreds{
				BindDN: fmt.Sprintf("uid=app-%s-%s,%s", appName, tenant.Name, ouDN),
				BaseDN: ouDN,
			})
			if seedErr != nil {
				return false, fmt.Errorf("seed ldap: %w", seedErr)
			}
			bindPassword = creds.BindPassword
		}
		return false, r.Create(ctx, makeBindAccountJob(tenant, ouDN, appName, bindPassword))
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

// deleteLDAP handles LDAP cleanup on tenant deletion.
//
// DeletionPolicy=Delete: creates a UDM Job that removes the tenant OU with
// recursive=1, which cascades deletion of all child entries including the
// admin user.
//
// DeletionPolicy=Retain: preserves all tenant LDAP data including the admin
// user. The admin user must not be deleted on Retain undeploy because deletion
// causes the LDAP server to assign a new entryUUID on recreation. The
// entryUUID is used as the Nextcloud user ID (via the LDAP username attribute)
// and as the opendesk_useruuid OIDC claim (via Keycloak's entryUUID mapper).
// Deleting the admin user therefore breaks the Nextcloud LDAP→OIDC user chain
// across undeploy/redeploy cycles, causing HTTP 400 errors on OIDC code
// exchange. Instead, provisioning jobs are deleted so they re-run on the next
// deploy via the PATCH path, which resets any stale attributes (isOxUser,
// oxAccess) without changing the entryUUID.
func (r *TenantReconciler) deleteLDAP(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	ouDN := tenantOUDN(tenant)

	// Start portal entry delete Jobs for all dedicated portal apps regardless of
	// deletion policy — the app service will be unavailable after this undeploy.
	// UDM handles cascading removal from portal categories when an entry is deleted.
	// This is fire-and-forget; we do not block the main deletion flow on it.
	portalApps, _ := r.collectDedicatedPortalApps(ctx, tenant)
	var portalJobNames []string
	for _, pa := range portalApps {
		jobName := portalEntryDeleteJobName(tenant.Name, pa.AppName)
		existing := &batchv1.Job{}
		if getErr := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, existing); errors.IsNotFound(getErr) {
			_ = r.Create(ctx, makePortalEntryDeleteJob(tenant, pa.AppName))
		}
		portalJobNames = append(portalJobNames, portalEntryJobName(tenant.Name, pa.AppName), jobName)
	}

	if tenant.Spec.DeletionPolicy == gentianov1alpha1.DeletionPolicyDelete {
		// OU recursive delete cascades all children including the admin user.
		jobName := ouDeleteJobName(tenant.Name)
		existing := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, existing)
		if err == nil {
			if jobIsComplete(existing) {
				// Delete provisioning jobs so they are re-created on the next deploy.
				r.deleteProvisioningJobs(ctx,
					ouJobName(tenant.Name),
					appUserTemplateJobName(tenant.Name),
					adminUserJobName(tenant.Name),
					adminPolicyJobName(tenant.Name),
				)
				r.deleteProvisioningJobs(ctx, portalJobNames...)
				return nil
			}
			return errDeleteJobPending
		}
		if !errors.IsNotFound(err) {
			return err
		}
		if err := r.Create(ctx, makeOUDeleteJob(tenant, ouDN)); err != nil {
			return err
		}
		return errDeleteJobPending
	}

	// DeletionPolicy=Retain: lock all users in the tenant OU so they cannot log in.
	// Guard: only create the lock job if the admin user job is complete (users exist).
	// Preserves all LDAP data; does NOT delete the admin user (entryUUID must be stable).
	aj := &batchv1.Job{}
	switch ajErr := r.Get(ctx, types.NamespacedName{Name: adminUserJobName(tenant.Name), Namespace: kernelNamespace}, aj); {
	case errors.IsNotFound(ajErr), ajErr == nil && !jobIsComplete(aj):
		// Admin user was never fully provisioned; nothing to lock.
		r.deleteProvisioningJobs(ctx,
			ouJobName(tenant.Name),
			appUserTemplateJobName(tenant.Name),
			adminUserJobName(tenant.Name),
			adminPolicyJobName(tenant.Name),
		)
		r.deleteProvisioningJobs(ctx, portalJobNames...)
		return nil
	case ajErr != nil:
		return ajErr
	}

	lockJobName := ldapLockJobName(tenant.Name)
	lockJob := &batchv1.Job{}
	lockErr := r.Get(ctx, types.NamespacedName{Name: lockJobName, Namespace: kernelNamespace}, lockJob)
	if lockErr == nil {
		if jobIsComplete(lockJob) {
			// Also remove the OU provision job so a subsequent deploy
			// re-runs it (ensures the OU is recreated if it was removed).
			r.deleteProvisioningJobs(ctx,
				ouJobName(tenant.Name),
				appUserTemplateJobName(tenant.Name),
				adminUserJobName(tenant.Name),
				adminPolicyJobName(tenant.Name),
			)
			r.deleteProvisioningJobs(ctx, portalJobNames...)
			return nil
		}
		return errDeleteJobPending
	}
	if !errors.IsNotFound(lockErr) {
		return lockErr
	}
	if err := r.Create(ctx, makeLockOUJob(tenant, ouDN)); err != nil {
		return err
	}
	return errDeleteJobPending
}

// ensureLDAPBase provisions the LDAP OU, admin user, and delegated-admin
// policy for tenants that have no LDAP-requiring apps. This is a non-blocking
// best-effort step identical in purpose to ensureNextcloudGroup: it does not
// affect Phase=Ready. For tenants WITH LDAP apps, ensureLDAP already handles
// all steps so this function is a no-op to avoid duplicate Job creation.
// Sequence: OU → App User template → admin-user → admin-policy (each step waits for the previous).
// Admin user runs before policy for the same reason as in ensureLDAP: the
// portal groups cache must be populated before portal allowedGroups are set.
func (r *TenantReconciler) ensureLDAPBase(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	ldapApps, err := r.collectLDAPApps(ctx, tenant)
	if err != nil {
		return err
	}
	// ensureLDAP already handles these steps when LDAP apps are present.
	if len(ldapApps) > 0 {
		return nil
	}

	ouDN := tenantOUDN(tenant)

	ouDone, err := r.ensureOUJob(ctx, tenant, ouDN)
	if err != nil || !ouDone {
		return err
	}

	templateDone, err := r.ensureAppUserTemplateJob(ctx, tenant, ouDN)
	if err != nil || !templateDone {
		return err
	}

	var adminCreds secrets.TenantAdminCreds
	if r.Seeder != nil {
		adminCreds, err = r.Seeder.SeedTenantAdmin(ctx, tenant.Name)
		if err != nil {
			return fmt.Errorf("seed tenant admin for LDAP base: %w", err)
		}
	} else {
		adminCreds = secrets.TenantAdminCreds{Username: "admin-" + tenant.Name, Password: "placeholder"}
	}
	userDone, err := r.ensureAdminUserJob(ctx, tenant, ouDN, adminCreds)
	if err != nil || !userDone {
		return err
	}

	policyDone, err := r.ensureAdminPolicyJob(ctx, tenant, ouDN)
	if err != nil || !policyDone {
		return err
	}

	// Also provision per-tenant portal entries for dedicated apps with central-navigation:nubus.
	portalApps, err := r.collectDedicatedPortalApps(ctx, tenant)
	if err != nil {
		return fmt.Errorf("collect dedicated portal apps: %w", err)
	}
	expectedTiles := make(map[string]struct{}, len(portalApps))
	for _, pa := range portalApps {
		expectedTiles[pa.AppName] = struct{}{}
	}
	// Remove portal entries that were provisioned previously but are no longer expected.
	if _, err := r.deleteStalePortalEntriesForTenant(ctx, tenant, expectedTiles); err != nil {
		return fmt.Errorf("delete stale portal entries: %w", err)
	}
	for _, pa := range portalApps {
		done, err := r.ensurePortalEntryJob(ctx, tenant, ouDN, pa)
		if err != nil {
			return fmt.Errorf("ensure portal entry Job for app %s: %w", pa.AppName, err)
		}
		if done {
			if trackErr := r.addProvisionedPortalTile(ctx, tenant, pa.AppName); trackErr != nil {
				return trackErr
			}
		}
	}
	return nil
}

// --- Portal tile annotation helpers -----------------------------------------

// getProvisionedPortalTiles reads the comma-separated tile names from the
// gentian.org/provisioned-portal-tiles annotation.
func getProvisionedPortalTiles(tenant *gentianov1alpha1.Tenant) map[string]struct{} {
	result := make(map[string]struct{})
	if tenant.Annotations == nil {
		return result
	}
	for _, name := range strings.Split(tenant.Annotations[annotationProvisionedPortalTiles], ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			result[name] = struct{}{}
		}
	}
	return result
}

// patchProvisionedPortalTiles persists the updated tile set as a tenant annotation.
func (r *TenantReconciler) patchProvisionedPortalTiles(ctx context.Context, tenant *gentianov1alpha1.Tenant, tiles map[string]struct{}) error {
	patch := client.MergeFrom(tenant.DeepCopy())
	if tenant.Annotations == nil {
		tenant.Annotations = make(map[string]string)
	}
	names := make([]string, 0, len(tiles))
	for n := range tiles {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		delete(tenant.Annotations, annotationProvisionedPortalTiles)
	} else {
		tenant.Annotations[annotationProvisionedPortalTiles] = strings.Join(names, ",")
	}
	return client.IgnoreNotFound(r.Patch(ctx, tenant, patch))
}

// addProvisionedPortalTile marks a tile as provisioned in the tenant annotation.
// No-op if the tile is already tracked.
func (r *TenantReconciler) addProvisionedPortalTile(ctx context.Context, tenant *gentianov1alpha1.Tenant, tileName string) error {
	tiles := getProvisionedPortalTiles(tenant)
	if _, exists := tiles[tileName]; exists {
		return nil
	}
	tiles[tileName] = struct{}{}
	return r.patchProvisionedPortalTiles(ctx, tenant, tiles)
}

// deleteStalePortalEntriesForTenant creates UDM delete Jobs for portal tiles
// that are tracked in the tenant annotation but are no longer in expectedTiles.
// Returns true when all stale entries have been cleaned up (delete Jobs
// completed and the annotation updated). Fire-and-forget for new delete Jobs;
// returns false to trigger a re-queue until cleanup is complete.
func (r *TenantReconciler) deleteStalePortalEntriesForTenant(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	expectedTiles map[string]struct{},
) (bool, error) {
	provisioned := getProvisionedPortalTiles(tenant)
	allDone := true
	for tileName := range provisioned {
		if _, wanted := expectedTiles[tileName]; wanted {
			continue
		}
		deleteJobName := portalEntryDeleteJobName(tenant.Name, tileName)
		deleteJob := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: deleteJobName, Namespace: kernelNamespace}, deleteJob)
		if errors.IsNotFound(err) {
			_ = r.Create(ctx, makePortalEntryDeleteJob(tenant, tileName))
			allDone = false
			continue
		}
		if err != nil {
			return false, err
		}
		if jobIsFailed(deleteJob) {
			if deleteJob.DeletionTimestamp.IsZero() {
				prop := metav1.DeletePropagationBackground
				_ = r.Delete(ctx, deleteJob, &client.DeleteOptions{PropagationPolicy: &prop})
			}
			allDone = false
			continue
		}
		if !jobIsComplete(deleteJob) {
			allDone = false
			continue
		}
		// Delete job completed: remove the create Job so reinstalling the same app
		// triggers a fresh ensurePortalEntryJob run instead of treating the
		// completed create Job as proof that the tile is already provisioned.
		createJob := &batchv1.Job{}
		createJobName := portalEntryJobName(tenant.Name, tileName)
		if getErr := r.Get(ctx, types.NamespacedName{Name: createJobName, Namespace: kernelNamespace}, createJob); getErr == nil {
			if createJob.DeletionTimestamp.IsZero() {
				prop := metav1.DeletePropagationBackground
				_ = r.Delete(ctx, createJob, &client.DeleteOptions{PropagationPolicy: &prop})
			}
		}
		// Remove tile from annotation.
		tiles := getProvisionedPortalTiles(tenant)
		delete(tiles, tileName)
		if err := r.patchProvisionedPortalTiles(ctx, tenant, tiles); err != nil {
			return false, err
		}
		ctrl.LoggerFrom(ctx).Info("removed stale portal entry", "tile", tileName, "tenant", tenant.Name)
	}
	return allDone, nil
}

// --- Job constructors --------------------------------------------------------

func makeLockOUJob(tenant *gentianov1alpha1.Tenant, ouDN string) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ldapLockJobName(tenant.Name),
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
						udmContainer("lock-users", buildLockOUScript(ouDN)),
					},
				},
			},
		},
	}
}

func makeOUJob(tenant *gentianov1alpha1.Tenant, ouDN string) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ouJobName(tenant.Name),
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
						udmContainer("provision-ou", buildOUScript(ouDN, tenant.Name)),
					},
				},
			},
		},
	}
}

func makeBindAccountJob(tenant *gentianov1alpha1.Tenant, ouDN, appName, bindPassword string) *batchv1.Job {
	ttl := int32(3600)
	c := udmContainer("provision-bind-account", buildBindAccountScript(ouDN, appName, tenant.Name))
	if bindPassword != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: "BIND_PW", Value: bindPassword})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bindAccountJobName(tenant.Name, appName),
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

func makeAppUserTemplateJob(tenant *gentianov1alpha1.Tenant, ouDN, mailDomain string) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appUserTemplateJobName(tenant.Name),
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
						udmContainer("provision-app-user-template", buildAppUserTemplateScript(ouDN, tenant.Name, mailDomain)),
					},
				},
			},
		},
	}
}

func makeAdminUserJob(tenant *gentianov1alpha1.Tenant, ouDN string, creds secrets.TenantAdminCreds) *batchv1.Job {
	ttl := int32(3600)
	c := udmContainer("provision-admin-user", buildAdminUserScript(ouDN, tenant.Name, tenant.Spec.AdminEmail))
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "ADMIN_USERNAME", Value: creds.Username},
		corev1.EnvVar{Name: "ADMIN_PASSWORD", Value: creds.Password},
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminUserJobName(tenant.Name),
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

func makeAdminPolicyJob(tenant *gentianov1alpha1.Tenant, ouDN string) *batchv1.Job {
	ttl := int32(3600)
	c := udmContainer("provision-admin-policy", buildAdminPolicyScript(ouDN, tenant.Name))
	c.Env = append(c.Env, corev1.EnvVar{Name: "ADMIN_USERNAME", Value: "admin-" + tenant.Name})
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminPolicyJobName(tenant.Name),
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

func makePortalEntryJob(tenant *gentianov1alpha1.Tenant, ouDN string, pa dedicatedPortalApp, tenantDomain string) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      portalEntryJobName(tenant.Name, pa.AppName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       pa.ProfileName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						udmContainer("provision-portal-entry", buildPortalEntryScript(
							ouDN, tenant.Name, pa.AppName, pa.SubDomain, tenantDomain,
							pa.DisplayNameDE, pa.DisplayNameEN,
							pa.LinkSuffix, pa.LinkTarget, pa.AllowedGroupCN, pa.Logo,
						)),
					},
				},
			},
		},
	}
}

func makePortalEntryDeleteJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      portalEntryDeleteJobName(tenant.Name, appName),
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
					Containers: []corev1.Container{
						udmContainer("delete-portal-entry", buildPortalEntryDeleteScript(tenant.Name, appName)),
					},
				},
			},
		},
	}
}

func makeOUDeleteJob(tenant *gentianov1alpha1.Tenant, ouDN string) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ouDeleteJobName(tenant.Name),
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
						udmContainer("delete-ou", buildOUDeleteScript(ouDN)),
					},
				},
			},
		},
	}
}

// udmContainer returns a Container that executes a shell script using the curl
// image. Credentials are injected from the udm-admin Secret in the kernel namespace.
func udmContainer(name, script string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   udmProvisionerImage,
		Command: []string{"/bin/sh", "-c", script},
		Env: []corev1.EnvVar{
			{
				Name: "UDM_URL",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: udmAdminSecret},
						Key:                  "url",
					},
				},
			},
			{
				Name: "UDM_ADMIN_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: udmAdminSecret},
						Key:                  "password",
					},
				},
			},
			{
				Name: "UDM_LDAP_BASE",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: udmAdminSecret},
						Key:                  "ldapBase",
					},
				},
			},
		},
	}
}

// --- Shell scripts -----------------------------------------------------------

// buildOUScript creates the tenant OU, users group, and admins group.
// All UDM calls are idempotent (GET before POST).
func buildOUScript(ouDN, tenantName string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
# OU_POS is assigned here; shell expands ${UDM_LDAP_BASE} at runtime.
OU_POS="%s"
OU_ENC=$(urlencode "${OU_POS}")
# Create tenant OU if absent
STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
	"${BASE_URL}/container/ou/${OU_ENC}")
if [ "${STATUS}" = "404" ]; then
  curl -s -o /dev/null -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/container/ou/" \
    -d "{\"properties\":{\"name\":\"%s\",\"description\":\"Tenant %s\"},\"position\":\"${UDM_LDAP_BASE}\"}"
  echo "OU %s created"
elif [ "${STATUS}" = "200" ]; then
  echo "OU %s already exists (HTTP ${STATUS})"
else
  echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
  exit 1
fi

# Create users group if absent
USERS_GRP_ENC=$(urlencode "cn=users_%s,${OU_POS}")
STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
	"${BASE_URL}/groups/group/${USERS_GRP_ENC}")
if [ "${STATUS}" = "404" ]; then
  curl -s -o /dev/null -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/groups/group/" \
		-d "{\"properties\":{\"name\":\"users_%s\"},\"position\":\"${OU_POS}\"}"
  echo "group users_%s created"
elif [ "${STATUS}" = "200" ]; then
  echo "group users_%s already exists"
else
  echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
  exit 1
fi

# Create admins group if absent
ADMINS_GRP_ENC=$(urlencode "cn=admins_%s,${OU_POS}")
STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
	"${BASE_URL}/groups/group/${ADMINS_GRP_ENC}")
if [ "${STATUS}" = "404" ]; then
  curl -s -o /dev/null -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/groups/group/" \
		-d "{\"properties\":{\"name\":\"admins_%s\"},\"position\":\"${OU_POS}\"}"
  echo "group admins_%s created"
elif [ "${STATUS}" = "200" ]; then
  echo "group admins_%s already exists"
else
  echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
  exit 1
fi

# Create ou=users sub-container for LDAP federation scope.
# The Keycloak LDAP User Storage Provider's usersDn points here so that
# uid=admin-{tenant} at the OU root is NOT imported by federation — it must
# stay a Keycloak-local user (provisioned by the admin job) to avoid duplicate
# or conflicting account entries.
USERS_OU_ENC=$(urlencode "ou=users,${OU_POS}")
STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
  "${BASE_URL}/container/ou/${USERS_OU_ENC}")
if [ "${STATUS}" = "404" ]; then
  curl -s -o /dev/null -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/container/ou/" \
    -d "{\"properties\":{\"name\":\"users\",\"description\":\"Regular users\"},\"position\":\"${OU_POS}\"}"
  echo "ou=users sub-container created"
elif [ "${STATUS}" = "200" ]; then
  echo "ou=users sub-container already exists"
else
  echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
  exit 1
fi

# Create cn=templates container for tenant-scoped user templates.
TEMPLATES_CN_ENC=$(urlencode "cn=templates,${OU_POS}")
STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
  "${BASE_URL}/container/cn/${TEMPLATES_CN_ENC}")
if [ "${STATUS}" = "404" ]; then
  curl -s -o /dev/null -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/container/cn/" \
    -d "{\"properties\":{\"name\":\"templates\",\"description\":\"Tenant user templates\"},\"position\":\"${OU_POS}\"}"
  echo "cn=templates container created"
elif [ "${STATUS}" = "200" ]; then
  echo "cn=templates container already exists"
else
  echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
  exit 1
fi

# Create per-tenant managed-by-attribute-* groups inside the tenant OU.
# These replace the global cn=groups managed-by-attribute-* groups so that
# app access control stays scoped to the tenant (one OU = one realm = one tenant).
for MBA_GROUP in Groupware Fileshare FileshareAdmin Videoconference Livecollaboration LivecollaborationAdmin; do
  MBA_GRP_ENC=$(urlencode "cn=managed-by-attribute-${MBA_GROUP},${OU_POS}")
  STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
    -H "Accept: application/json" \
    "${BASE_URL}/groups/group/${MBA_GRP_ENC}")
  if [ "${STATUS}" = "404" ]; then
    POST_STATUS=$(curl -s -o /dev/null -w "%%{http_code}" -X POST ${CREDS} \
      -H "Content-Type: application/json" \
      -H "Accept: application/json" \
      "${BASE_URL}/groups/group/" \
      -d "{\"properties\":{\"name\":\"managed-by-attribute-${MBA_GROUP}\"},\"position\":\"${OU_POS}\"}")
    if [ "${POST_STATUS}" = "201" ] || [ "${POST_STATUS}" = "200" ]; then
      echo "group managed-by-attribute-${MBA_GROUP} created in ${OU_POS}"
    elif [ "${POST_STATUS}" = "422" ]; then
      echo "group managed-by-attribute-${MBA_GROUP} name conflict (global group exists); skipped"
    else
      echo "failed to create group managed-by-attribute-${MBA_GROUP} (HTTP ${POST_STATUS})" >&2
      exit 1
    fi
  elif [ "${STATUS}" = "200" ]; then
    echo "group managed-by-attribute-${MBA_GROUP} already exists in ${OU_POS}"
  else
    echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
    exit 1
  fi
done`,
		ouDN, tenantName, tenantName, tenantName, tenantName,
		tenantName, tenantName, tenantName, tenantName,
		tenantName, tenantName, tenantName, tenantName)
}

// buildBindAccountScript creates a service-account user that apps use as the LDAP bind DN.
// Uses users/ldap object type which only requires username and password.
// The username is scoped to the tenant (app-<appName>-<tenantName>) to avoid
// UDM's global username uniqueness constraint when multiple tenants host the same app.
func buildBindAccountScript(ouDN, appName, tenantName string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
# OU_POS and BIND_DN: ${UDM_LDAP_BASE} expands at runtime via shell.
OU_POS="%s"
BIND_DN="uid=app-%s-%s,${OU_POS}"
BIND_DN_ENC=$(urlencode "${BIND_DN}")

STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
	"${BASE_URL}/users/ldap/${BIND_DN_ENC}")
if [ "${STATUS}" = "404" ]; then
  if [ -z "${BIND_PW:-}" ]; then
    BIND_PW=$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 20)
  fi
  POST_STATUS=$(curl -s -o /dev/null -w "%%{http_code}" -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/users/ldap/" \
    -d "{\"properties\":{\"username\":\"app-%s-%s\",\"password\":\"${BIND_PW}\"},\"position\":\"${OU_POS}\"}")
  if [ "${POST_STATUS}" != "201" ]; then
    echo "failed to create bind account app-%s-%s (HTTP ${POST_STATUS})" >&2
    exit 1
  fi
  echo "bind account app-%s-%s created in ${OU_POS}"
elif [ "${STATUS}" = "200" ]; then
  echo "bind account app-%s-%s already exists (HTTP ${STATUS})"
else
  echo "unexpected UDM status (HTTP ${STATUS}); will retry" >&2
  exit 1
fi`, ouDN, appName, tenantName, appName, tenantName, appName, tenantName, appName, tenantName, appName, tenantName)
}

// buildAppUserTemplateScript provisions a tenant-scoped App User template that
// pre-fills mailPrimaryAddress as <username>@<tenant-domain> (openDesk-style
// "@domain" template syntax). Also ensures the tenant mail domain exists in UDM
// and removes upstream openDesk user templates from the template picker.
func buildAppUserTemplateScript(ouDN, tenantName, mailDomain string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
OU_POS="%s"
TEMPLATES_POS="cn=templates,${OU_POS}"
APP_USERS_DN="cn=App Users,cn=groups,${UDM_LDAP_BASE}"
MAIL_DOMAIN="%s"
MAIL_DOMAIN_CONTAINER="cn=domain,cn=mail,${UDM_LDAP_BASE}"
# UDM uses the template "name" property ("1 App User") as the LDAP RDN cn.
TEMPLATE_DN="cn=1 App User,${TEMPLATES_POS}"
TEMPLATE_ENC=$(urlencode "${TEMPLATE_DN}")

# Remove kernel App User template if present (app users are tenant-scoped only).
KERNEL_APP_TEMPLATE_DN="cn=App User,cn=templates,cn=univention,${UDM_LDAP_BASE}"
KERNEL_APP_ENC=$(urlencode "${KERNEL_APP_TEMPLATE_DN}")
KERNEL_APP_STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
  "${BASE_URL}/settings/usertemplate/${KERNEL_APP_ENC}")
if [ "${KERNEL_APP_STATUS}" = "200" ]; then
  curl -sf --max-time 30 -X DELETE ${CREDS} \
    -H "Accept: application/json" \
    "${BASE_URL}/settings/usertemplate/${KERNEL_APP_ENC}" || true
  echo "removed kernel App User template"
fi

# Hide upstream openDesk templates so tenant admins only see Gentian templates.
for LEGACY_NAME in "openDesk User" "openDesk Admin" "openDesk Administrator"; do
  LEGACY_DN="cn=${LEGACY_NAME},cn=templates,cn=univention,${UDM_LDAP_BASE}"
  LEGACY_ENC=$(urlencode "${LEGACY_DN}")
  LEGACY_STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
    -H "Accept: application/json" \
    "${BASE_URL}/settings/usertemplate/${LEGACY_ENC}")
  if [ "${LEGACY_STATUS}" = "200" ]; then
    curl -sf --max-time 30 -X DELETE ${CREDS} \
      -H "Accept: application/json" \
      "${BASE_URL}/settings/usertemplate/${LEGACY_ENC}" || true
    echo "removed legacy template ${LEGACY_NAME}"
  fi
done

# Ensure the tenant mail domain object exists for UMC validation and prefill.
MAIL_DOMAIN_SEARCH=$(curl -s --max-time 30 ${CREDS} \
  -H "Accept: application/json" \
  "${BASE_URL}/mail/domain/?filter=name%%3D${MAIL_DOMAIN}")
if ! echo "${MAIL_DOMAIN_SEARCH}" | grep -q '"dn"'; then
  curl -sf --max-time 30 -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/mail/domain/" \
    -d "{\"properties\":{\"name\":\"${MAIL_DOMAIN}\"},\"position\":\"${MAIL_DOMAIN_CONTAINER}\"}"
  echo "mail domain ${MAIL_DOMAIN} created"
else
  echo "mail domain ${MAIL_DOMAIN} already exists"
fi

# Ensure cn=templates exists (also created by the OU job; idempotent here).
TEMPLATES_CN_ENC=$(urlencode "${TEMPLATES_POS}")
STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
  "${BASE_URL}/container/cn/${TEMPLATES_CN_ENC}")
if [ "${STATUS}" = "404" ]; then
  curl -sf --max-time 30 -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/container/cn/" \
    -d "{\"properties\":{\"name\":\"templates\",\"description\":\"Tenant user templates\"},\"position\":\"${OU_POS}\"}"
  echo "cn=templates container created"
elif [ "${STATUS}" != "200" ]; then
  echo "UDM not ready for cn=templates (HTTP ${STATUS}); will retry" >&2
  exit 1
fi

TEMPLATE_BODY=$(cat <<EOF
{
  "properties": {
    "name": "1 App User",
    "description": "Standard user with access to apps; email prefill uses @${MAIL_DOMAIN}",
    "mailPrimaryAddress": "<username>@${MAIL_DOMAIN}",
    "opendeskFileshareEnabled": true,
    "opendeskLivecollaborationEnabled": true,
    "opendeskVideoconferenceEnabled": true,
    "groups": ["${APP_USERS_DN}"]
  },
  "position": "${TEMPLATES_POS}"
}
EOF
)

STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
  "${BASE_URL}/settings/usertemplate/${TEMPLATE_ENC}")
if [ "${STATUS}" = "404" ]; then
  HTTP=$(curl -s -o /tmp/udm-template-body -w "%%{http_code}" -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/settings/usertemplate/" \
    -d "${TEMPLATE_BODY}")
  if [ "${HTTP}" != "200" ] && [ "${HTTP}" != "201" ]; then
    # UDM may return 422 when the template RDN already exists (e.g. after a manual UMC edit).
    if [ "${HTTP}" = "422" ]; then
      STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
        -H "Accept: application/json" \
        "${BASE_URL}/settings/usertemplate/${TEMPLATE_ENC}")
      if [ "${STATUS}" = "200" ]; then
        echo "App User template already exists at ${TEMPLATE_DN}"
      else
        echo "failed to create App User template (HTTP ${HTTP})" >&2
        cat /tmp/udm-template-body >&2 2>/dev/null || true
        exit 1
      fi
    else
      echo "failed to create App User template (HTTP ${HTTP})" >&2
      cat /tmp/udm-template-body >&2 2>/dev/null || true
      exit 1
    fi
  else
    echo "App User template created for ${MAIL_DOMAIN}"
  fi
elif [ "${STATUS}" = "200" ]; then
  HTTP=$(curl -s -o /tmp/udm-template-body -w "%%{http_code}" -X PATCH ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/settings/usertemplate/${TEMPLATE_ENC}" \
    -d "${TEMPLATE_BODY}")
  case "${HTTP}" in
  200|204|400|422)
    echo "App User template reconciled for ${MAIL_DOMAIN} (HTTP ${HTTP})"
    ;;
  *)
    echo "failed to reconcile App User template (HTTP ${HTTP})" >&2
    cat /tmp/udm-template-body >&2 2>/dev/null || true
    exit 1
    ;;
  esac
else
  echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
  exit 1
fi`, ouDN, mailDomain)
}

// buildAdminUserScript creates the tenant admin as a users/user in the tenant
// OU and adds them to admins_<tenant>. ADMIN_USERNAME and ADMIN_PASSWORD are
// injected as environment variables by the Job constructor. The call is
// idempotent: it ensures the mail domain exists, creates the user if missing,
// and then ensures group membership.
//
// The admin user is placed inside ou=users,<tenantOU> so that the Keycloak
// tenant-realm LDAP federation (which targets ou=users,<tenantOU>) picks them
// up. Service accounts and groups remain at the tenant OU root and are
// therefore NOT imported by the federation.
func buildAdminUserScript(ouDN, tenantName, adminEmail string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
udm_patch_ok() {
	local url="$1"
	local data="$2"
	local label="$3"
	local http
	http=$(curl -s --max-time 30 -o /tmp/udm-patch-body -w "%%{http_code}" -X PATCH ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${url}" -d "${data}")
	case "${http}" in
	200|204)
		echo "${label} (HTTP ${http})"
		return 0
		;;
	400|422)
		echo "${label}: already satisfied (HTTP ${http})"
		return 0
		;;
	*)
		echo "${label}: PATCH failed (HTTP ${http})" >&2
		cat /tmp/udm-patch-body >&2 2>/dev/null || true
		return 1
		;;
	esac
}
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
OU_POS="%s"
USERS_OU_POS="ou=users,${OU_POS}"
ADMIN_EMAIL="%s"
MAIL_DOMAIN="${ADMIN_EMAIL##*@}"
MAIL_DOMAIN_CONTAINER="cn=domain,cn=mail,${UDM_LDAP_BASE}"
ADMIN_DN="uid=${ADMIN_USERNAME},${USERS_OU_POS}"
ADMIN_DN_ENC=$(urlencode "${ADMIN_DN}")
ADMINS_GRP_DN="cn=admins_%s,${OU_POS}"
ADMINS_GRP_ENC=$(urlencode "${ADMINS_GRP_DN}")

# Ensure the mail domain object exists for the admin's email address.
MAIL_DOMAIN_SEARCH=$(curl -s --max-time 30 ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/mail/domain/?filter=name%%3D${MAIL_DOMAIN}")
if ! echo "${MAIL_DOMAIN_SEARCH}" | grep -q '"dn"'; then
	curl -sf --max-time 30 -X POST ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${BASE_URL}/mail/domain/" \
		-d "{\"properties\":{\"name\":\"${MAIL_DOMAIN}\"},\"position\":\"${MAIL_DOMAIN_CONTAINER}\"}"
	echo "mail domain ${MAIL_DOMAIN} created"
else
	echo "mail domain ${MAIL_DOMAIN} already exists"
fi

# Create the tenant admin user if absent.
STATUS=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
	"${BASE_URL}/users/user/${ADMIN_DN_ENC}")
if [ "${STATUS}" = "404" ]; then
  curl -sf --max-time 30 -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/users/user/" \
		-d "{\"properties\":{\"username\":\"${ADMIN_USERNAME}\",\"password\":\"${ADMIN_PASSWORD}\",\"firstname\":\"Tenant\",\"lastname\":\"Admin\",\"mailPrimaryAddress\":\"${ADMIN_EMAIL}\",\"pwdChangeNextLogin\":false,\"isOxUser\":false,\"oxAccess\":\"none\"},\"position\":\"${USERS_OU_POS}\"}"
  echo "UDM user ${ADMIN_USERNAME} created in ${USERS_OU_POS}"
elif [ "${STATUS}" = "200" ]; then
  echo "UDM user ${ADMIN_USERNAME} already exists (HTTP ${STATUS})"
  udm_patch_ok "${BASE_URL}/users/user/${ADMIN_DN_ENC}" \
	"{\"properties\":{\"password\":\"${ADMIN_PASSWORD}\",\"disabled\":false,\"pwdChangeNextLogin\":false}}" \
	"user ${ADMIN_USERNAME} password synced" || exit 1
else
  echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
  exit 1
fi

# Ensure the admin user is enabled and does not require a forced password change.
# disabled:false explicitly marks the account as active, preventing Keycloak's
# univention-ldap-mapper from importing the account as disabled on first login.
udm_patch_ok "${BASE_URL}/users/user/${ADMIN_DN_ENC}" \
	"{\"properties\":{\"disabled\":false,\"pwdChangeNextLogin\":false,\"isOxUser\":false,\"oxAccess\":\"none\",\"opendeskFileshareEnabled\":false,\"opendeskLivecollaborationEnabled\":false}}" \
	"user ${ADMIN_USERNAME} enabled and pwdChangeNextLogin cleared" || exit 1

# Ensure the admin user is in the admins_<tenant> group (idempotent PATCH).
ADMINS_BODY=$(curl -s --max-time 30 ${CREDS} \
  -H "Accept: application/json" \
	"${BASE_URL}/groups/group/${ADMINS_GRP_ENC}" | tr -d '\n')
if printf '%%s' "${ADMINS_BODY}" | grep -qF "${ADMIN_DN}"; then
  echo "user ${ADMIN_USERNAME} already in admins group"
else
  udm_patch_ok "${BASE_URL}/groups/group/${ADMINS_GRP_ENC}" \
    "{\"properties\":{\"users\":[\"${ADMIN_DN}\"]}}" \
    "user ${ADMIN_USERNAME} added to admins group" || exit 1
fi

# Unlock all users in the tenant OU (idempotent; reverses Retain lock on redeploy).
OU_ENC_UNLOCK=$(urlencode "${USERS_OU_POS}")
USERS_JSON=$(curl -s --max-time 30 ${CREDS} \
  -H "Accept: application/json" \
  "${BASE_URL}/users/user/?position=${OU_ENC_UNLOCK}")
printf '%%s' "${USERS_JSON}" | grep -o '"dn": *"[^"]*"' | sed 's/"dn": *"//;s/"$//' | while IFS= read -r USER_DN; do
  if [ -n "${USER_DN}" ]; then
    USER_ENC=$(urlencode "${USER_DN}")
    curl -sf --max-time 30 -X PATCH ${CREDS} \
      -H "Content-Type: application/json" \
      -H "Accept: application/json" \
      "${BASE_URL}/users/user/${USER_ENC}" \
      -d '{"properties":{"disabled":false}}' || true
    echo "unlocked ${USER_DN}"
  fi
done
echo "unlock sweep complete for ${USERS_OU_POS}"`, ouDN, adminEmail, tenantName)
}

// buildAdminPolicyScript configures UMC/UDM delegated admin policy for one
// tenant OU. It gives admins_<tenant> access to all UMC modules via a
// tenant-scoped UMC policy, and restricts the admin's browsing/search scope
// to the tenant OU.
//
// UDM REST API invariants:
//   - settings/umc_operationset "operation" must be a JSON array of objects
//     with "command" and "option" keys, e.g. [{"command":"*","option":"*"}].
//     Sending plain strings results in broken LDAP storage.
//   - A policy is assigned via PATCHing the target object's
//     "policies.policies/umc" field. The policy is assigned to BOTH the admins
//     GROUP and the tenant OU. Assigning to the OU causes UMC to use that OU
//     as the management scope (browsing/search base) for the admin.
//   - ldapFilter on policies/umc restricts policy application to specific users.
//     Setting it to "(uid=admin-<tenant>)" ensures regular users in the OU do
//     not inherit admin-level UMC permissions from the OU policy reference.
//
// Note: UDM REST API (and UMC) bind to LDAP as cn=admin (the slapd rootdn),
// which bypasses all LDAP ACLs. Tenant isolation for user browsing is therefore
// enforced at the UMC application layer via the OU-scoped policy reference.
func buildAdminPolicyScript(ouDN, tenantName string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
# PATCH helper: succeed on 200/204 and on 400/422 when UDM reports an idempotent conflict.
udm_patch_ok() {
	local url="$1"
	local data="$2"
	local label="$3"
	local http
	http=$(curl -s --max-time 30 -o /tmp/udm-patch-body -w "%%{http_code}" -X PATCH ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${url}" -d "${data}")
	case "${http}" in
	200|204)
		echo "${label} (HTTP ${http})"
		return 0
		;;
	400|422)
		echo "${label}: already satisfied (HTTP ${http})"
		return 0
		;;
	*)
		echo "${label}: PATCH failed (HTTP ${http})" >&2
		cat /tmp/udm-patch-body >&2 2>/dev/null || true
		return 1
		;;
	esac
}
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"

OU_POS="%s"
USERS_OU_POS="ou=users,${OU_POS}"
ADMIN_DN="uid=${ADMIN_USERNAME},${USERS_OU_POS}"
ADMIN_DN_ENC=$(urlencode "${ADMIN_DN}")
ADMINS_GRP_DN="cn=admins_%s,${OU_POS}"
POLICY_DN="cn=tenant-admins-%s,cn=UMC,cn=policies,${UDM_LDAP_BASE}"
OPSET_DN="cn=tenant-%s-admin,${UDM_LDAP_BASE}"

POLICY_ENC=$(urlencode "${POLICY_DN}")
OPSET_ENC=$(urlencode "${OPSET_DN}")
ADMINS_GRP_ENC=$(urlencode "${ADMINS_GRP_DN}")

# Verify admins group exists (created by the OU job).
STATUS=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/groups/group/${ADMINS_GRP_ENC}")
if [ "${STATUS}" = "404" ]; then
	echo "admins group ${ADMINS_GRP_DN} is missing — run OU job first"
	exit 1
elif [ "${STATUS}" != "200" ]; then
	echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
	exit 1
fi

# Ensure UMC operation set exists.
# NOTE: operation must be an array of objects with "command" and "option" keys.
# Plain strings like ["*"] result in broken umcOperationSetCommand LDAP storage.
STATUS=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/settings/umc_operationset/${OPSET_ENC}")
if [ "${STATUS}" = "404" ]; then
	curl -sf --max-time 30 -X POST ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${BASE_URL}/settings/umc_operationset/" \
		-d "{\"properties\":{\"name\":\"tenant-%s-admin\",\"description\":\"Tenant delegated admin operation set\",\"operation\":[{\"command\":\"*\",\"option\":\"*\"}],\"hosts\":[\"*\"]},\"position\":\"${UDM_LDAP_BASE}\"}"
	echo "UMC operation set tenant-%s-admin created"
elif [ "${STATUS}" = "200" ]; then
	echo "UMC operation set tenant-%s-admin already exists"
else
	echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
	exit 1
fi

# Reconcile operation set (idempotent — ensures correct format on pre-existing objects).
curl -sf --max-time 30 -X PATCH ${CREDS} \
	-H "Content-Type: application/json" \
	-H "Accept: application/json" \
	"${BASE_URL}/settings/umc_operationset/${OPSET_ENC}" \
	-d "{\"properties\":{\"operation\":[{\"command\":\"*\",\"option\":\"*\"}],\"hosts\":[\"*\"]}}"
echo "UMC operation set tenant-%s-admin reconciled"

# Ensure UMC policy exists.
STATUS=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/policies/umc/${POLICY_ENC}")
if [ "${STATUS}" = "404" ]; then
	curl -sf --max-time 30 -X POST ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${BASE_URL}/policies/umc/" \
		-d "{\"properties\":{\"name\":\"tenant-admins-%s\",\"ldapFilter\":\"(uid=admin-%s)\"},\"position\":\"cn=UMC,cn=policies,${UDM_LDAP_BASE}\"}"
	echo "UMC policy tenant-admins-%s created"
elif [ "${STATUS}" = "200" ]; then
	echo "UMC policy tenant-admins-%s already exists"
else
	echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
	exit 1
fi

# Ensure policy allows the operation set (idempotent PATCH on the policy's allow list).
# Also set ldapFilter so this policy only applies to the tenant admin, not regular
# users in the OU (which would gain unintended admin-level UMC access).
curl -sf --max-time 30 -X PATCH ${CREDS} \
	-H "Content-Type: application/json" \
	-H "Accept: application/json" \
	"${BASE_URL}/policies/umc/${POLICY_ENC}" \
	-d "{\"properties\":{\"allow\":[\"${OPSET_DN}\"],\"ldapFilter\":\"(uid=admin-%s)\"}}"
echo "UMC policy tenant-admins-%s now allows ${OPSET_DN}"

# Assign the UMC policy to the admins group. UMC evaluates policies by walking
# the LDAP hierarchy from the user and collecting univentionPolicyReference from
# each group and OU. Assigning to both the group and the OU (see below) gives
# UMC two signals: (a) the admin has UMC permissions via the group, and (b) the
# OU is the management scope (browsing/search base) for the admin.
curl -sf --max-time 30 -X PATCH ${CREDS} \
	-H "Content-Type: application/json" \
	-H "Accept: application/json" \
	"${BASE_URL}/groups/group/${ADMINS_GRP_ENC}" \
	-d "{\"policies\":{\"policies/umc\":[\"${POLICY_DN}\"]}}"
echo "UMC policy tenant-admins-%s assigned to ${ADMINS_GRP_DN}"

# Also assign the UMC policy to the tenant OU so UMC uses it as the management
# scope (browsing/search base) for the tenant admin. UMC evaluates policies
# inherited from OUs in the user's DN path; when the policy is found on the OU,
# UMC restricts all user/group browsing to within that OU. The ldapFilter on
# the policy ensures only the tenant admin gets this scoped view — regular users
# in the same OU are not affected (their objects don't match the filter).
OU_ENC=$(urlencode "${OU_POS}")
curl -sf --max-time 30 -X PATCH ${CREDS} \
	-H "Content-Type: application/json" \
	-H "Accept: application/json" \
	"${BASE_URL}/container/ou/${OU_ENC}" \
	-d "{\"policies\":{\"policies/umc\":[\"${POLICY_DN}\"]}}"
echo "UMC policy assigned to OU ${OU_POS} (management scope restriction)"

# Add the tenant admins group to the portal management entries so admins see the
# UMC user/group tiles in the portal. The entries are globally shared; we append
# the tenant group idempotently by reading the current allowedGroups and PATCHing
# only if the group is not yet present. Pure-shell JSON parsing is used because
# the curl image does not ship python3 or jq; allowedGroups values are LDAP DNs
# that never contain ']' so the sed extraction is safe.
for ENTRY_CN in swp.admin_user swp.admin_group; do
	ENTRY_ENC=$(urlencode "cn=${ENTRY_CN},cn=entry,cn=portals,cn=univention,${UDM_LDAP_BASE}")
	BODY=$(curl -s --max-time 30 ${CREDS} \
		-H "Accept: application/json" \
		"${BASE_URL}/portals/entry/${ENTRY_ENC}" | tr -d '\n')
	CURRENT_ARR=$(printf '%%s' "${BODY}" | sed -n 's/.*"allowedGroups":[[:space:]]*\[\([^]]*\)\].*/\1/p')
	if printf '%%s' "${CURRENT_ARR}" | grep -qF "\"${ADMINS_GRP_DN}\""; then
		echo "portal entry ${ENTRY_CN}: ${ADMINS_GRP_DN} already in allowedGroups"
	else
		if [ -z "${CURRENT_ARR}" ]; then
			NEW_GROUPS="[\"${ADMINS_GRP_DN}\"]"
		else
			NEW_GROUPS="[${CURRENT_ARR},\"${ADMINS_GRP_DN}\"]"
		fi
		curl -sf --max-time 30 -X PATCH ${CREDS} \
			-H "Content-Type: application/json" \
			-H "Accept: application/json" \
			"${BASE_URL}/portals/entry/${ENTRY_ENC}" \
			-d "{\"properties\":{\"allowedGroups\":${NEW_GROUPS}}}"
		echo "portal entry ${ENTRY_CN}: added ${ADMINS_GRP_DN} to allowedGroups"
	fi
done

# Ensure the shared cn=Tenant Admins group exists. The group is used by the
# slapd.conf patch (92-gentian-tenant-acl.sh) to grant all tenant admins write
# access to cn=temporary,cn=univention. The patch adds a single ACL rule for
# this group; the controller makes each tenant's admins group a nested member.
TENANT_ADMINS_DN="cn=Tenant Admins,cn=groups,${UDM_LDAP_BASE}"
TENANT_ADMINS_ENC=$(urlencode "${TENANT_ADMINS_DN}")
STATUS=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/groups/group/${TENANT_ADMINS_ENC}")
if [ "${STATUS}" = "404" ]; then
	curl -sf --max-time 30 -X POST ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${BASE_URL}/groups/group/" \
		-d "{\"properties\":{\"name\":\"Tenant Admins\",\"description\":\"Nested-group umbrella granting LDAP cn=temporary write access to all tenant admin groups\"},\"position\":\"cn=groups,${UDM_LDAP_BASE}\"}"
	echo "group Tenant Admins created"
elif [ "${STATUS}" = "200" ]; then
	echo "group Tenant Admins already exists"
else
	echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
	exit 1
fi

# Verify the tenant admin user exists. After a Retain undeploy the user is preserved;
# after a Delete undeploy the admin-user Job recreates it before this Job runs.
ADMIN_STATUS=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/users/user/${ADMIN_DN_ENC}")
if [ "${ADMIN_STATUS}" = "404" ]; then
	echo "admin user ${ADMIN_DN} not found — run admin-user job first" >&2
	exit 1
elif [ "${ADMIN_STATUS}" != "200" ]; then
	echo "UDM not ready for admin user (HTTP ${ADMIN_STATUS}); will retry" >&2
	exit 1
fi

# Add the tenant admins group as a nested member of cn=Tenant Admins (idempotent).
# UDM uses 'nestedGroup' for nested groups, which writes to uniqueMember in LDAP.
# We ALSO add the admin user as a direct member (in 'users') because UMC portal
# evaluates allowedGroups without expanding nested groups. PATCH each property
# separately so a partial update never resends malformed JSON extracted from an
# existing UDM object graph.
TENANT_ADMINS_BODY=$(curl -s --max-time 30 ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/groups/group/${TENANT_ADMINS_ENC}" | tr -d '\n')

if printf '%%s' "${TENANT_ADMINS_BODY}" | grep -qF "${ADMINS_GRP_DN}"; then
	echo "Tenant Admins: ${ADMINS_GRP_DN} already a nested member"
else
	udm_patch_ok "${BASE_URL}/groups/group/${TENANT_ADMINS_ENC}" \
		"{\"properties\":{\"nestedGroup\":[\"${ADMINS_GRP_DN}\"]}}" \
		"Tenant Admins: nested member ${ADMINS_GRP_DN}" || exit 1
fi

if printf '%%s' "${TENANT_ADMINS_BODY}" | grep -qF "${ADMIN_DN}"; then
	echo "Tenant Admins: ${ADMIN_DN} already a direct member"
else
	udm_patch_ok "${BASE_URL}/groups/group/${TENANT_ADMINS_ENC}" \
		"{\"properties\":{\"users\":[\"${ADMIN_DN}\"]}}" \
		"Tenant Admins: direct member ${ADMIN_DN}" || exit 1
fi

# Add the tenant OU's users sub-container to the global settings/directory
# 'users' default-container list. This ensures that the "openDesk User" wizard
# places new users in ou=users,<tenantOU> (the Keycloak federation target) rather
# than the OU root. Users at the OU root would not be picked up by the tenant
# realm's LDAP federation and would be invisible to tenant-realm OIDC clients.
SETTINGS_DN="cn=default containers,cn=univention,${UDM_LDAP_BASE}"
SETTINGS_ENC=$(urlencode "${SETTINGS_DN}")
SETTINGS_BODY=$(curl -s --max-time 30 ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/settings/directory/${SETTINGS_ENC}" | tr -d '\n')
if printf '%%s' "${SETTINGS_BODY}" | grep -qF "${USERS_OU_POS}"; then
	echo "settings/directory: ${USERS_OU_POS} already in users default containers"
else
	udm_patch_ok "${BASE_URL}/settings/directory/${SETTINGS_ENC}" \
		"{\"properties\":{\"users\":[\"${USERS_OU_POS}\"]}}" \
		"settings/directory: users default container ${USERS_OU_POS}" || exit 1
fi

# Tenant-scoped App User template used by UMC when creating users. The global UCR
# default points at the kernel template; tenant admins cannot read it (LDAP ACL
# patch 11). UMC gateway patch 93 selects this template when it is the only
# visible entry in the template picker.
TENANT_TEMPLATE_DN="cn=1 App User,cn=templates,${OU_POS}"
TENANT_TEMPLATE_ENC=$(urlencode "${TENANT_TEMPLATE_DN}")
TEMPLATE_STATUS=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/settings/usertemplate/${TENANT_TEMPLATE_ENC}")
if [ "${TEMPLATE_STATUS}" != "200" ]; then
	echo "tenant App User template ${TENANT_TEMPLATE_DN} missing (HTTP ${TEMPLATE_STATUS})" >&2
	exit 1
fi
echo "tenant App User template ${TENANT_TEMPLATE_DN} is the UMC default for ${ADMIN_USERNAME}"

echo "admin policy provisioning complete for ${ADMIN_USERNAME}"`,
		ouDN, tenantName, tenantName, tenantName,
		tenantName, tenantName,
		tenantName, tenantName, tenantName,
		tenantName, tenantName,
		tenantName, tenantName,
		tenantName, tenantName)
}

// buildLockOUScript lists all users in the tenant OU via UDM and disables each
// (sets disabled:true). This blocks all login channels (LDAP federation,
// Kerberos, Samba) while preserving all user data for fast re-enable on redeploy.
// Users are searched under ou=users,<tenantOU> (the sub-container where all
// human users are placed by the new LDAP structure).
func buildLockOUScript(ouDN string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
OU_POS="%s"
USERS_OU_POS="ou=users,${OU_POS}"
OU_ENC=$(urlencode "${USERS_OU_POS}")
echo "locking all users in ${USERS_OU_POS}"
USERS_JSON=$(curl -s --max-time 30 ${CREDS} \
  -H "Accept: application/json" \
  "${BASE_URL}/users/user/?position=${OU_ENC}")
printf '%%s' "${USERS_JSON}" | grep -o '"dn": *"[^"]*"' | sed 's/"dn": *"//;s/"$//' | while IFS= read -r USER_DN; do
  if [ -n "${USER_DN}" ]; then
    USER_ENC=$(urlencode "${USER_DN}")
    curl -sf --max-time 30 -X PATCH ${CREDS} \
      -H "Content-Type: application/json" \
      -H "Accept: application/json" \
      "${BASE_URL}/users/user/${USER_ENC}" \
      -d '{"properties":{"disabled":true}}' || true
    echo "locked ${USER_DN}"
  fi
done
echo "lock sweep complete for ${USERS_OU_POS}"`, ouDN)
}

// buildOUDeleteScript removes the tenant OU and all child entries.
func buildOUDeleteScript(ouDN string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
# OU_POS: ${UDM_LDAP_BASE} expands at runtime.
OU_POS="%s"
OU_ENC=$(urlencode "${OU_POS}")

HTTP=$(curl -s -o /dev/null -w "%%{http_code}" -X DELETE ${CREDS} \
  -H "Accept: application/json" \
	"${BASE_URL}/container/ou/${OU_ENC}?cleanup=1&recursive=1")
echo "OU %s deletion requested (HTTP ${HTTP})"
case "${HTTP}" in
  200|204|404) ;;
  *) echo "ERROR: OU delete failed (HTTP ${HTTP})" >&2; exit 1 ;;
esac`, ouDN, ouDN)
}

// --- Name helpers ------------------------------------------------------------

// tenantOUDN returns the LDAP DN for a tenant's OU as a shell-interpolatable string.
// Uses spec.isolation.ldapOU if set; if that value is a bare RDN (no ',' separator)
// it appends ',${UDM_LDAP_BASE}' so the job's shell can expand it at runtime.
// Defaults to "ou={name},${UDM_LDAP_BASE}" when ldapOU is not set.
func tenantOUDN(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.LDAPOu != "" {
		ou := tenant.Spec.Isolation.LDAPOu
		// Append LDAP base when value is a relative DN (no comma = no parent components).
		if !strings.Contains(ou, ",") {
			return ou + ",${UDM_LDAP_BASE}"
		}
		return ou
	}
	return fmt.Sprintf("ou=%s,${UDM_LDAP_BASE}", tenant.Name)
}

// tenantConcreteOUDN returns the concrete LDAP DN for a tenant's OU by substituting
// ldapBase for the ${UDM_LDAP_BASE} shell-interpolation placeholder.
// Used where a real DN (not a shell expression) is needed — e.g. Keycloak job env vars.
func tenantConcreteOUDN(tenant *gentianov1alpha1.Tenant, ldapBase string) string {
	return strings.ReplaceAll(tenantOUDN(tenant), "${UDM_LDAP_BASE}", ldapBase)
}

// tenantUserMailDomain returns the mail domain used for tenant end-user accounts.
// App users get <username>@<tenant-domain> for cluster-wide LDAP uniqueness.
func tenantUserMailDomain(tenant *gentianov1alpha1.Tenant, kernelDomain, tenancyMode string) string {
	return tenant.EffectiveDomain(kernelDomain, tenancyMode)
}

func ouJobName(tenantName string) string {
	return fmt.Sprintf("ldap-ou-%s", tenantName)
}

func bindAccountJobName(tenantName, appName string) string {
	return fmt.Sprintf("ldap-bind-%s-%s", tenantName, appName)
}

func adminPolicyJobName(tenantName string) string {
	return fmt.Sprintf("ldap-admin-policy-%s", tenantName)
}

func adminUserJobName(tenantName string) string {
	return fmt.Sprintf("ldap-admin-user-%s", tenantName)
}

func appUserTemplateJobName(tenantName string) string {
	return fmt.Sprintf("ldap-app-user-template-%s", tenantName)
}

func ouDeleteJobName(tenantName string) string {
	return fmt.Sprintf("ldap-ou-delete-%s", tenantName)
}

func ldapLockJobName(tenantName string) string {
	return fmt.Sprintf("ldap-lock-%s", tenantName)
}

func adminUserDeleteJobName(tenantName string) string {
	return fmt.Sprintf("ldap-admin-user-delete-%s", tenantName)
}

func portalEntryJobName(tenantName, appName string) string {
	return fmt.Sprintf("ldap-portal-entry-%s-%s", tenantName, appName)
}

func portalEntryDeleteJobName(tenantName, appName string) string {
	return fmt.Sprintf("ldap-portal-entry-delete-%s-%s", tenantName, appName)
}

func portalRealtimeLinksJobName(tenantName string) string {
	return fmt.Sprintf("ldap-portal-realtime-links-%s", tenantName)
}

// portalRealtimeLinkTargets returns meet/chat base URLs for kernel portal contact
// actions when the tenant has Jitsi and/or Element installed.
func (r *TenantReconciler) portalRealtimeLinkTargets(tenant *gentianov1alpha1.Tenant) (meetURL, chatURL string) {
	effectiveDomain := r.tenantEffectiveDomain(tenant)
	if effectiveDomain == "" {
		return "", ""
	}
	for _, app := range tenant.Spec.Apps {
		switch app.Profile {
		case "jitsi":
			meetURL = fmt.Sprintf("https://meet.%s", effectiveDomain)
		case "element":
			chatURL = fmt.Sprintf("https://chat.%s", effectiveDomain)
		}
	}
	return meetURL, chatURL
}

func (r *TenantReconciler) ensurePortalRealtimeLinksJob(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	ouDN, meetURL, chatURL string,
) (bool, error) {
	jobName := portalRealtimeLinksJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	includeLegacy := gentianov1alpha1.NormalizeTenancyMode(r.TenancyMode) == gentianov1alpha1.TenancyModeSingle
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, makePortalRealtimeLinksJob(tenant, ouDN, meetURL, chatURL, includeLegacy))
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

func makePortalRealtimeLinksJob(tenant *gentianov1alpha1.Tenant, ouDN, meetURL, chatURL string, includeLegacy bool) *batchv1.Job {
	ttl := int32(3600)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      portalRealtimeLinksJobName(tenant.Name),
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
						udmContainer("patch-portal-realtime-links",
							buildPortalRealtimeLinksScript(tenant.Name, ouDN, meetURL, chatURL, includeLegacy)),
					},
				},
			},
		},
	}
}

// --- Portal entry scripts ----------------------------------------------------

// buildPortalEntryScript returns a shell script that idempotently creates (or
// reconciles) a per-tenant UDM portal entry and adds it to the od.applications
// portal category. Each AppProfile PortalTileSpec produces one entry.
//
// Parameters (in fmt.Sprintf order):
//  1. ouDN         — tenant OU DN (shell-interpolatable)
//  2. tenantName   — literal tenant name
//  3. tileName     — tile name; forms entry CN: swp.{tileName}_{tenantName}
//  4. subDomain    — ingress subdomain (e.g. "webmail")
//  5. tenantDomain — full tenant domain (e.g. "gtn-demo.desk.gentian.org")
//  6. displayNameDE — de_DE display label
//  7. displayNameEN — en_US display label
//  8. linkSuffix   — appended to base URL for deep-linking (e.g. "#app=io.ox/mail")
//  9. linkTarget   — UDM linkTarget value: newwindow|samewindow|embedded
//  10. allowedGroupCN — LDAP CN of the group (e.g. "Domain Users",
//     "managed-by-attribute-Groupware"); full DN resolved at runtime via UDM_LDAP_BASE
//  11. logo         — raw base64 string (data URI prefix stripped); may be empty
func buildPortalEntryScript(ouDN, tenantName, tileName, subDomain, tenantDomain, displayNameDE, displayNameEN, linkSuffix, linkTarget, allowedGroupCN, logo string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
OU_POS="%s"
TENANT_NAME="%s"
TILE_NAME="%s"
ENTRY_CN="swp.${TILE_NAME}_${TENANT_NAME}"
ENTRY_DN="cn=${ENTRY_CN},cn=entry,cn=portals,cn=univention,${UDM_LDAP_BASE}"
ENTRY_ENC=$(urlencode "${ENTRY_DN}")
LINK="https://%s.%s%s"
LINK_TARGET="%s"
USERS_GRP_CN="%s"
# For per-tenant app access groups (managed-by-attribute-*) prefer the tenant-scoped
# group (created by the ldap-ou job). If the tenant-scoped group does not exist
# (e.g. UDM rejected creation due to a pre-existing global group with the same CN),
# fall back to the global cn=groups group so the tile remains accessible.
if printf '%%s' "${USERS_GRP_CN}" | grep -q '^managed-by-attribute-'; then
  TENANT_GRP_DN="cn=${USERS_GRP_CN},${OU_POS}"
  GLOBAL_GRP_DN="cn=${USERS_GRP_CN},cn=groups,${UDM_LDAP_BASE}"
  TENANT_GRP_STATUS=$(curl -s --max-time 10 -o /dev/null -w "%%{http_code}" ${CREDS} \
    -H "Accept: application/json" \
    "${BASE_URL}/groups/group/$(urlencode "${TENANT_GRP_DN}")")
  if [ "${TENANT_GRP_STATUS}" = "200" ]; then
    USERS_GRP_DN="${TENANT_GRP_DN}"
  else
    USERS_GRP_DN="${GLOBAL_GRP_DN}"
  fi
else
  USERS_GRP_DN="cn=${USERS_GRP_CN},cn=groups,${UDM_LDAP_BASE}"
fi
LOGO="%s"
CAT_DN="cn=od.applications,cn=category,cn=portals,cn=univention,${UDM_LDAP_BASE}"
CAT_ENC=$(urlencode "${CAT_DN}")

# Create or reconcile the per-tenant portal entry.
STATUS=$(curl -s --max-time 30 -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/portals/entry/${ENTRY_ENC}")
if [ "${STATUS}" = "404" ]; then
	curl -sf --max-time 30 -X POST ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${BASE_URL}/portals/entry/" \
		-d "{\"properties\":{\"name\":\"${ENTRY_CN}\",\"displayName\":{\"de_DE\":\"%s\",\"en_US\":\"%s\"},\"description\":{\"de_DE\":\"\",\"en_US\":\"\"},\"link\":[[\"en_US\",\"${LINK}\"]],\"linkTarget\":\"${LINK_TARGET}\",\"allowedGroups\":[\"${USERS_GRP_DN}\"],\"activated\":true,\"anonymous\":false,\"icon\":\"${LOGO}\"},\"position\":\"cn=entry,cn=portals,cn=univention,${UDM_LDAP_BASE}\"}"
	echo "portal entry ${ENTRY_CN} created"
elif [ "${STATUS}" = "200" ]; then
	curl -sf --max-time 30 -X PATCH ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${BASE_URL}/portals/entry/${ENTRY_ENC}" \
		-d "{\"properties\":{\"link\":[[\"en_US\",\"${LINK}\"]],\"linkTarget\":\"${LINK_TARGET}\",\"allowedGroups\":[\"${USERS_GRP_DN}\"],\"icon\":\"${LOGO}\"}}"
	echo "portal entry ${ENTRY_CN} link, linkTarget and allowedGroups reconciled"
else
	echo "UDM not ready (HTTP ${STATUS}); will retry" >&2
	exit 1
fi

# Add the entry to the od.applications category (idempotent, with retry-and-verify
# to guard against concurrent jobs racing on the same category PATCH).
MAX_RETRIES=10
i=0
while [ $i -lt $MAX_RETRIES ]; do
	CAT_BODY=$(curl -s --max-time 30 ${CREDS} \
		-H "Accept: application/json" \
		"${BASE_URL}/portals/category/${CAT_ENC}" | tr -d '\n')
	CURRENT_ENTRIES=$(printf '%%s' "${CAT_BODY}" | sed -n 's/.*"entries":[[:space:]]*\[\([^]]*\)\].*/\1/p')
	if printf '%%s' "${CURRENT_ENTRIES}" | grep -qF "\"${ENTRY_DN}\""; then
		echo "category od.applications: ${ENTRY_DN} already in entries"
		break
	fi
	if [ -z "${CURRENT_ENTRIES}" ]; then
		NEW_ENTRIES="[\"${ENTRY_DN}\"]"
	else
		NEW_ENTRIES="[${CURRENT_ENTRIES},\"${ENTRY_DN}\"]"
	fi
	curl -sf --max-time 30 -X PATCH ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${BASE_URL}/portals/category/${CAT_ENC}" \
		-d "{\"properties\":{\"entries\":${NEW_ENTRIES}}}"
	# Verify our entry is now present (another concurrent job may have overwritten).
	sleep 2
	VERIFY=$(curl -s --max-time 30 ${CREDS} \
		-H "Accept: application/json" \
		"${BASE_URL}/portals/category/${CAT_ENC}" | tr -d '\n')
	if printf '%%s' "${VERIFY}" | grep -qF "\"${ENTRY_DN}\""; then
		echo "category od.applications: added ${ENTRY_DN}"
		break
	fi
	echo "category od.applications: PATCH race detected, retrying (attempt $((i+1)))..." >&2
	i=$((i+1))
	sleep $i
done
if [ $i -eq $MAX_RETRIES ]; then
	echo "category od.applications: failed to add ${ENTRY_DN} after ${MAX_RETRIES} retries" >&2
	exit 1
fi`,
		ouDN, tenantName, tileName, subDomain, tenantDomain, linkSuffix,
		linkTarget, allowedGroupCN, logo,
		displayNameDE, displayNameEN)
}

// buildPortalRealtimeLinksScript creates or updates UDM portal entries used when
// starting a video call or chat from the contacts UI. Each tenant gets
// swp.realtime_videoconference_<tenant> and swp.realtime_collaboration_<tenant>
// with allowedGroups scoped to that tenant's LDAP OU. In single-tenancy mode,
// legacy OpenDesk entry names (swp.realtime_*) are also maintained.
func buildPortalRealtimeLinksScript(tenantName, ouDN, meetURL, chatURL string, includeLegacy bool) string {
	var body strings.Builder
	body.WriteString(`set -eu
urlencode() { printf '%s' "$1" | sed 's/%/%25/g; s/ /%20/g; s/,/%2C/g; s/=/=%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
OU_POS="`)
	body.WriteString(ouDN)
	body.WriteString(`"
USERS_GRP_DN="cn=App Users,${OU_POS}"
ensure_realtime_entry() {
  ENTRY_CN="$1"
  LINK="$2"
  ENTRY_DN="cn=${ENTRY_CN},cn=entry,cn=portals,cn=univention,${UDM_LDAP_BASE}"
  ENTRY_ENC=$(urlencode "${ENTRY_DN}")
  STATUS=$(curl -s --max-time 30 -o /dev/null -w "%{http_code}" ${CREDS} \
    -H "Accept: application/json" \
    "${BASE_URL}/portals/entry/${ENTRY_ENC}")
  if [ "${STATUS}" = "404" ]; then
    curl -sf --max-time 30 -X POST ${CREDS} \
      -H "Content-Type: application/json" \
      -H "Accept: application/json" \
      "${BASE_URL}/portals/entry/" \
      -d "{\"properties\":{\"name\":\"${ENTRY_CN}\",\"displayName\":{\"de_DE\":\"\",\"en_US\":\"\"},\"description\":{\"de_DE\":\"\",\"en_US\":\"\"},\"link\":[[\"en_US\",\"${LINK}\"]],\"linkTarget\":\"newwindow\",\"allowedGroups\":[\"${USERS_GRP_DN}\"],\"activated\":true,\"anonymous\":false,\"icon\":\"\"},\"position\":\"cn=entry,cn=portals,cn=univention,${UDM_LDAP_BASE}\"}"
    echo "portal entry ${ENTRY_CN} created with link ${LINK}"
  elif [ "${STATUS}" = "200" ]; then
    curl -sf --max-time 30 -X PATCH ${CREDS} \
      -H "Content-Type: application/json" \
      -H "Accept: application/json" \
      "${BASE_URL}/portals/entry/${ENTRY_ENC}" \
      -d "{\"properties\":{\"link\":[[\"en_US\",\"${LINK}\"]],\"linkTarget\":\"newwindow\",\"allowedGroups\":[\"${USERS_GRP_DN}\"]}}"
    echo "portal entry ${ENTRY_CN} link set to ${LINK}"
  else
    echo "portal entry ${ENTRY_CN} lookup failed (HTTP ${STATUS})" >&2
    exit 1
  fi
}
`)
	if meetURL != "" {
		suffixed := fmt.Sprintf("swp.realtime_videoconference_%s", tenantName)
		fmt.Fprintf(&body, "ensure_realtime_entry %q %q\n", suffixed, meetURL)
		if includeLegacy {
			fmt.Fprintf(&body, "ensure_realtime_entry %q %q\n", "swp.realtime_videoconference", meetURL)
		}
	}
	if chatURL != "" {
		suffixed := fmt.Sprintf("swp.realtime_collaboration_%s", tenantName)
		fmt.Fprintf(&body, "ensure_realtime_entry %q %q\n", suffixed, chatURL)
		if includeLegacy {
			fmt.Fprintf(&body, "ensure_realtime_entry %q %q\n", "swp.realtime_collaboration", chatURL)
		}
	}
	return body.String()
}

// buildPortalEntryDeleteScript returns a shell script that removes a per-tenant
// UDM portal entry. UDM handles cascading removal from portal categories when
// an entry is deleted.
//
// Parameters (in fmt.Sprintf order):
//  1. tenantName — literal tenant name
//  2. appName    — literal app/profile name
func buildPortalEntryDeleteScript(tenantName, appName string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
ENTRY_CN="swp.%s_%s"
ENTRY_DN="cn=${ENTRY_CN},cn=entry,cn=portals,cn=univention,${UDM_LDAP_BASE}"
ENTRY_ENC=$(urlencode "${ENTRY_DN}")
HTTP=$(curl -s -o /dev/null -w "%%{http_code}" -X DELETE ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/portals/entry/${ENTRY_ENC}")
if [ "${HTTP}" = "204" ] || [ "${HTTP}" = "404" ] || [ "${HTTP}" = "200" ]; then
	echo "portal entry ${ENTRY_CN} deletion (HTTP ${HTTP})"
else
	echo "failed to delete portal entry ${ENTRY_CN} (HTTP ${HTTP})" >&2
	exit 1
fi`,
		appName, tenantName)
}
