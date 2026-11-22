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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	dauthz "github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/placement"
	"github.com/gentian-org/gentian-os/internal/provisioning/privilege"
)

const (
	conditionAppPrivilegesReady = "AppPrivilegesReady"
	appPrivilegeRequeueAfter    = 5 * time.Minute
	// Cadence while an app's sync Job is still running, as opposed to the idle
	// re-check above.
	appPrivilegeJobPollAfter = 10 * time.Second

	// appPrivilegeRequestedAtAnnotation is set when what a person holds in
	// the tenant changed (RightsChanged). The operator clears per-app sync
	// fingerprints while requested != processed.
	appPrivilegeRequestedAtAnnotation = "gentianos.io/app-privilege-requested-at"
	appPrivilegeProcessedAtAnnotation = "gentianos.io/app-privilege-processed-at"
	appPrivilegeSyncAnnotationPrefix  = "gentianos.io/app-privilege-sync-"
)

// ensureAppPrivileges tells each installed app that cannot read its people's
// roles from a token who its administrators and its users are, as the rights
// store names them, through the
// sync Job its profile declares (ComponentProfile.spec.hooks.provisioning).
func (r *TenantReconciler) ensureAppPrivileges(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	// The installed apps: a component the platform places is not among
	// the apps whose privileged role app-admins are mapped into.
	installed, err := placement.Installed(ctx, r.Client, tenant.Spec.Apps)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(installed) == 0 {
		r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionTrue,
			"NoAppsConfigured", "No application privileged roles to reconcile")
		return ctrl.Result{}, nil
	}

	identityReady := apimeta.IsStatusConditionTrue(tenant.Status.Conditions, conditionIdentityReady)
	appsReady := apimeta.IsStatusConditionTrue(tenant.Status.Conditions, conditionAppsReady)
	if !identityReady || !appsReady {
		r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse,
			"WaitingForPrerequisites", "Waiting for identity and app deployment before privileged role sync")
		return ctrl.Result{RequeueAfter: appPrivilegeRequeueAfter}, nil
	}

	if err := r.applyAppPrivilegeReconcileRequest(ctx, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply app privilege reconcile request: %w", err)
	}

	kc, err := authz.LoadKeycloakAdminClient(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("load keycloak admin: %w", err)
	}
	// Who holds what is the rights store's to say; the realm says who the
	// accounts are. Without the store nobody can be told to an app, and an
	// empty list would take every administrator away.
	if r.Rights == nil {
		r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse,
			"NoRightsStore", "The rights store is not connected; app roles are not applied")
		return ctrl.Result{RequeueAfter: appPrivilegeRequeueAfter}, nil
	}
	accounts, err := kc.ListRealmUsers(ctx, keycloakRealmName(tenant))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list the realm's accounts: %w", err)
	}

	var privilegedApps []string
	syncFailed := false
	syncPending := false
	for _, app := range installed {
		profileName, err := catalogue.ResolveTenantComponentProfile(ctx, r.Client, app)
		if err != nil {
			return ctrl.Result{}, err
		}
		profile, err := getProfileFor(ctx, r.Client, profileName, app.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("get ComponentProfile %s: %w", profileName, err)
		}
		role := profilePrivilegedRole(profile)
		if role == nil {
			continue
		}
		privilegedApps = append(privilegedApps, profileName)

		ready, err := r.appComponentReady(ctx, tenant, profileName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse,
				"WaitingForApp", fmt.Sprintf("Waiting for %s before privileged role sync", profileName))
			return ctrl.Result{RequeueAfter: appPrivilegeRequeueAfter}, nil
		}

		// The app's administrators, and everyone who may use it: the script
		// gives the first its privileged role and may give the rest the
		// app's ordinary ones.
		adminSubjects, userSubjects, err := dauthz.AppHolders(ctx, r.Rights, tenant.Name, profileName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("read who holds %s: %w", profileName, err)
		}
		members, users := accountsOf(accounts, adminSubjects), accountsOf(accounts, userSubjects)
		appFingerprint := privilege.MemberFingerprint(members) + "@" + privilege.UsersFingerprint(users) +
			"@" + privilege.DeclarationFingerprint(profile.Provisioning())
		if r.appPrivilegeSynced(tenant, profileName, appFingerprint) {
			continue
		}

		done, err := r.syncAppPrivilegedRole(ctx, tenant, profileName, profile, role, members, users, appFingerprint)
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
		if err := r.persistAppPrivilegeFingerprint(ctx, tenant, profileName, appFingerprint); err != nil {
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

func profilePrivilegedRole(profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.PrivilegedRoleSpec {
	if profile == nil || profile.Provisioning() == nil {
		return nil
	}
	return profile.Provisioning().PrivilegedRole
}

// syncAppPrivilegedRole applies app-admins membership to one app by running
// the Job that app supplied. The operator resolves and publishes the
// membership; the script decides what that means for its own application.
//
// Nothing here may branch on a profile name, family or protocol: an app the
// kernel has to recognize by name is an app the kernel would have to be
// modified to support, which is precisely what the platform boundary in
// gentian-apps/docs/app-profile-guide.md forbids.
func (r *TenantReconciler) syncAppPrivilegedRole(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	profileName string,
	profile *gentianov1alpha1.ComponentProfile,
	role *gentianov1alpha1.PrivilegedRoleSpec,
	members, users []authz.KeycloakUser,
	fingerprint string,
) (done bool, err error) {
	switch role.Kind {
	case gentianov1alpha1.PrivilegedRoleKindGroup:
	default:
		return false, fmt.Errorf("unsupported privileged role kind %q", role.Kind)
	}
	jobSpec := profile.Provisioning().SyncJob
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
	usersJSON, err := privilege.MembersJSON(users)
	if err != nil {
		return false, fmt.Errorf("encode app users: %w", err)
	}

	// Script and member list travel in one Secret so they are always mounted as
	// a matched pair; a Job cannot end up running last reconcile's script
	// against this reconcile's membership.
	secret := privilege.MembersSecret(tenant.Name, profileName, ns, membersJSON, usersJSON)
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
	return ensureObject(ctx, r.Client, desired, func(existing *corev1.Secret) bool {
		existing.Labels, existing.Type = desired.Labels, desired.Type
		existing.Data, existing.StringData = desired.Data, desired.StringData
		return true
	})
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

// accountsOf is the realm's accounts among subjects, in the realm's order.
// A subject the realm does not know is left out: there is nobody to tell
// the app about.
func accountsOf(accounts []authz.KeycloakUser, subjects []string) []authz.KeycloakUser {
	wanted := make(map[string]bool, len(subjects))
	for _, s := range subjects {
		wanted[s] = true
	}
	out := []authz.KeycloakUser{}
	for _, a := range accounts {
		if wanted[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

// RightsChanged is what follows, outside the rights store, from a change of
// what a person holds in a tenant.
//
// The apps that are told their people (provisioning.syncJob) are told again
// now rather than at the next idle pass: the request annotation makes the
// tenant reconcile and forget what it last told them.
//
// When something was taken away, the person's sessions in the realm are
// ended. A token carries the roles held when it was written, and the realm
// cannot end one app's session alone; it tells every app that declared
// where it hears of a sign-out, and the next sign-in carries the roles as
// they are. A role that was given waits for the person's next sign-in.
func (r *TenantReconciler) RightsChanged(ctx context.Context, tenantName, subject string, taken bool) error {
	tenant := &gentianov1alpha1.Tenant{}
	if err := r.Get(ctx, types.NamespacedName{Name: tenantName}, tenant); err != nil {
		return err
	}
	orig := tenant.DeepCopy()
	if tenant.Annotations == nil {
		tenant.Annotations = map[string]string{}
	}
	tenant.Annotations[appPrivilegeRequestedAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := r.Patch(ctx, tenant, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("ask for the apps to be told: %w", err)
	}
	if !taken {
		return nil
	}
	kc, err := authz.LoadKeycloakAdminClient(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("load keycloak admin: %w", err)
	}
	if err := kc.EndUserSessions(ctx, keycloakRealmName(tenant), subject); err != nil {
		return fmt.Errorf("end the person's sessions: %w", err)
	}
	return nil
}
