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
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/provisioning/privilege"
)

const (
	conditionAppPrivilegesReady = "AppPrivilegesReady"
	appPrivilegeRequeueAfter    = 5 * time.Minute
	// Cadence while an app's sync Job is still running, as opposed to the idle
	// re-check above.
	appPrivilegeJobPollAfter = 10 * time.Second

	// appPrivilegeRequestedAtAnnotation is set by the Admin Console BFF when
	// gentian:tenant:<t>:app-admins membership changes. The operator clears
	// per-app sync fingerprints while requested != processed.
	appPrivilegeRequestedAtAnnotation = "gentianos.io/app-privilege-requested-at"
	appPrivilegeProcessedAtAnnotation = "gentianos.io/app-privilege-processed-at"
	appPrivilegeSyncAnnotationPrefix  = "gentianos.io/app-privilege-sync-"
)

// ensureAppPrivileges maps gentian:tenant:<t>:app-admins members into each
// installed app's declared privileged role (AppProfile.spec.provisioning).
func (r *TenantReconciler) ensureAppPrivileges(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	if len(tenant.Spec.Apps) == 0 {
		r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionTrue,
			"NoAppsConfigured", "No application privileged roles to reconcile")
		return ctrl.Result{}, nil
	}

	identityReady := tenantConditionTrue(tenant, conditionIdentityReady)
	appsReady := tenantConditionTrue(tenant, conditionAppsReady)
	if !identityReady || !appsReady {
		r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse,
			"WaitingForPrerequisites", "Waiting for identity and app deployment before privileged role sync")
		return ctrl.Result{RequeueAfter: appPrivilegeRequeueAfter}, nil
	}

	if err := r.applyAppPrivilegeReconcileRequest(ctx, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply app privilege reconcile request: %w", err)
	}

	kcURL, kcUser, kcPass, err := loadKeycloakAdmin(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("load keycloak admin: %w", err)
	}
	kc := authz.NewKeycloakAdminClient(kcURL, kcUser, kcPass)
	members, err := kc.ListGroupMembers(ctx, tenant.Name, gentianTenantAppAdminsGroup(tenant.Name))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list app-admins members: %w", err)
	}
	fingerprint := privilege.MemberFingerprint(members)

	var privilegedApps []string
	syncFailed := false
	syncPending := false
	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: profileName}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("get AppProfile %s: %w", profileName, err)
		}
		role := profilePrivilegedRole(profile)
		if role == nil {
			continue
		}
		privilegedApps = append(privilegedApps, profileName)

		ready, err := r.waitForAppClaimReady(ctx, tenant, profileName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse,
				"WaitingForApp", fmt.Sprintf("Waiting for %s before privileged role sync", profileName))
			return ctrl.Result{RequeueAfter: appPrivilegeRequeueAfter}, nil
		}

		if r.appPrivilegeSynced(tenant, profileName, fingerprint) {
			continue
		}

		done, err := r.syncAppPrivilegedRole(ctx, tenant, profileName, profile, role, members, fingerprint)
		if err != nil {
			syncFailed = true
			r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse,
				"SyncFailed", fmt.Sprintf("%s: %s", profileName, err.Error()))
			continue
		}
		if !done {
			// The Job is still running. Only a completed run may record the
			// fingerprint, or a crashed sync would be remembered as applied.
			syncPending = true
			r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse,
				"Syncing", fmt.Sprintf("Applying app administrators to %s", profileName))
			continue
		}
		if err := r.persistAppPrivilegeFingerprint(ctx, tenant, profileName, fingerprint); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Forget apps that are no longer installed. A fingerprint left behind by an
	// uninstalled app still matches unchanged membership, so reinstalling it
	// would look already-synced and silently come back with no administrators.
	// Done here rather than in the uninstall path so it self-heals however the
	// app left — CLI, App Store, or a hand-edited tenant manifest.
	if err := r.pruneAppPrivilegeFingerprints(ctx, tenant, privilegedApps); err != nil {
		return ctrl.Result{}, err
	}

	if len(privilegedApps) == 0 {
		if err := r.markAppPrivilegeRequestProcessed(ctx, tenant); err != nil {
			return ctrl.Result{}, err
		}
		r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionTrue,
			"NoPrivilegedRoles", "No installed apps declare a privileged role mapping")
		return ctrl.Result{}, nil
	}
	if syncFailed {
		return ctrl.Result{RequeueAfter: appPrivilegeRequeueAfter}, nil
	}
	if syncPending {
		// Come back sooner than the idle cadence: a Job that takes seconds
		// should not leave the tenant reporting "Syncing" for five minutes.
		return ctrl.Result{RequeueAfter: appPrivilegeJobPollAfter}, nil
	}
	if err := r.markAppPrivilegeRequestProcessed(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}
	r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionTrue,
		"Synced", "App administrator roles are synchronized")
	return ctrl.Result{RequeueAfter: appPrivilegeRequeueAfter}, nil
}

func (r *TenantReconciler) applyAppPrivilegeReconcileRequest(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Annotations == nil {
		return nil
	}
	requested := tenant.Annotations[appPrivilegeRequestedAtAnnotation]
	if requested == "" {
		return nil
	}
	if requested == tenant.Annotations[appPrivilegeProcessedAtAnnotation] {
		return nil
	}

	orig := tenant.DeepCopy()
	changed := false
	for key := range tenant.Annotations {
		if strings.HasPrefix(key, appPrivilegeSyncAnnotationPrefix) {
			delete(tenant.Annotations, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, tenant, client.MergeFrom(orig))
}

func (r *TenantReconciler) markAppPrivilegeRequestProcessed(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Annotations == nil {
		return nil
	}
	requested := tenant.Annotations[appPrivilegeRequestedAtAnnotation]
	if requested == "" {
		return nil
	}
	if requested == tenant.Annotations[appPrivilegeProcessedAtAnnotation] {
		return nil
	}
	orig := tenant.DeepCopy()
	if tenant.Annotations == nil {
		tenant.Annotations = map[string]string{}
	}
	tenant.Annotations[appPrivilegeProcessedAtAnnotation] = requested
	return r.Patch(ctx, tenant, client.MergeFrom(orig))
}

func profilePrivilegedRole(profile *gentianov1alpha1.AppProfile) *gentianov1alpha1.PrivilegedRoleSpec {
	if profile == nil || profile.Spec.Provisioning == nil {
		return nil
	}
	return profile.Spec.Provisioning.PrivilegedRole
}

// syncAppPrivilegedRole applies app-admins membership to one app by running
// the Job that app supplied. The operator resolves and publishes the
// membership; the script decides what that means for its own application.
//
// Nothing here may branch on a profile name, family or protocol: an app the
// kernel has to recognise by name is an app the kernel would have to be
// modified to support, which is precisely what the platform boundary in
// gentian-apps/docs/app-profile-guide.md forbids.
func (r *TenantReconciler) syncAppPrivilegedRole(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	profileName string,
	profile *gentianov1alpha1.AppProfile,
	role *gentianov1alpha1.PrivilegedRoleSpec,
	members []authz.KeycloakUser,
	fingerprint string,
) (done bool, err error) {
	switch role.Kind {
	case gentianov1alpha1.PrivilegedRoleKindGroup:
	default:
		return false, fmt.Errorf("unsupported privileged role kind %q", role.Kind)
	}
	jobSpec := profile.Spec.Provisioning.SyncJob
	if jobSpec == nil {
		return false, fmt.Errorf(
			"profile %q declares provisioning.privilegedRole but no provisioning.syncJob, "+
				"so the platform has no way to apply it", profileName)
	}

	ns := tenantNamespaceName(tenant)
	membersJSON, err := privilege.MembersJSON(members)
	if err != nil {
		return false, fmt.Errorf("encode app-admins members: %w", err)
	}

	// Script and member list travel in one Secret so they are always mounted as
	// a matched pair; a Job cannot end up running last reconcile's script
	// against this reconcile's membership.
	secret := privilege.MembersSecret(tenant.Name, profileName, ns, membersJSON)
	secret.StringData = map[string]string{"run.sh": jobSpec.Script}
	if err := r.applySecret(ctx, secret); err != nil {
		return false, fmt.Errorf("publish app-admins members: %w", err)
	}

	existing := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: privilege.JobName(profileName), Namespace: ns}, existing)
	switch {
	case errors.IsNotFound(err):
		job := privilege.SyncJob(tenant.Name, profileName, ns, fingerprint, role, jobSpec)
		if err := r.Create(ctx, job); err != nil {
			return false, fmt.Errorf("create privilege sync job: %w", err)
		}
		return false, nil
	case err != nil:
		return false, err
	}

	// A Job built for different membership is stale whatever its result: a
	// success only ever proves the membership it was given was applied.
	if existing.Annotations[privilege.FingerprintAnnotation] != fingerprint {
		policy := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !errors.IsNotFound(err) {
			return false, fmt.Errorf("replace stale privilege sync job: %w", err)
		}
		return false, nil
	}

	switch privilege.StateOf(existing) {
	case privilege.JobSucceeded:
		return true, nil
	case privilege.JobFailed:
		return false, fmt.Errorf("privilege sync job failed: %s", privilege.FailureMessage(existing))
	default:
		return false, nil
	}
}

// applySecret creates or updates a Secret the operator owns.
func (r *TenantReconciler) applySecret(ctx context.Context, desired *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.Type = desired.Type
	existing.Data = desired.Data
	existing.StringData = desired.StringData
	return r.Patch(ctx, existing, patch)
}

// pruneAppPrivilegeFingerprints drops the recorded fingerprint of every app
// that is no longer installed on this tenant.
func (r *TenantReconciler) pruneAppPrivilegeFingerprints(ctx context.Context, tenant *gentianov1alpha1.Tenant, installed []string) error {
	if len(tenant.Annotations) == 0 {
		return nil
	}
	keep := make(map[string]bool, len(installed))
	for _, name := range installed {
		keep[name] = true
	}
	orig := tenant.DeepCopy()
	changed := false
	for key := range tenant.Annotations {
		if !strings.HasPrefix(key, appPrivilegeSyncAnnotationPrefix) {
			continue
		}
		if keep[strings.TrimPrefix(key, appPrivilegeSyncAnnotationPrefix)] {
			continue
		}
		delete(tenant.Annotations, key)
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, tenant, client.MergeFrom(orig))
}

func (r *TenantReconciler) appPrivilegeSynced(tenant *gentianov1alpha1.Tenant, profileName, fingerprint string) bool {
	if tenant.Annotations == nil {
		return false
	}
	key := appPrivilegeAnnotationKey(profileName)
	return tenant.Annotations[key] == fingerprint
}

func (r *TenantReconciler) markAppPrivilegeSynced(tenant *gentianov1alpha1.Tenant, profileName, fingerprint string) {
	if tenant.Annotations == nil {
		tenant.Annotations = map[string]string{}
	}
	tenant.Annotations[appPrivilegeAnnotationKey(profileName)] = fingerprint
}

func (r *TenantReconciler) persistAppPrivilegeFingerprint(ctx context.Context, tenant *gentianov1alpha1.Tenant, profileName, fingerprint string) error {
	orig := tenant.DeepCopy()
	r.markAppPrivilegeSynced(tenant, profileName, fingerprint)
	return r.Patch(ctx, tenant, client.MergeFrom(orig))
}

func appPrivilegeAnnotationKey(profileName string) string {
	return appPrivilegeSyncAnnotationPrefix + profileName
}

func tenantConditionTrue(tenant *gentianov1alpha1.Tenant, condType string) bool {
	for _, cond := range tenant.Status.Conditions {
		if cond.Type == condType {
			return cond.Status == metav1.ConditionTrue
		}
	}
	return false
}
