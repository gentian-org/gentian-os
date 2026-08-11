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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/provisioning/mathesar"
	"github.com/gentian-org/gentian-os/internal/provisioning/privilege"
)

const (
	conditionAppPrivilegesReady = "AppPrivilegesReady"
	appPrivilegeRequeueAfter    = 5 * time.Minute

	// appPrivilegeRequestedAtAnnotation is set by the Admin Console BFF when
	// gentian:tenant:<t>:app-admins membership changes. The operator clears
	// per-app sync fingerprints while requested != processed.
	appPrivilegeRequestedAtAnnotation  = "gentianos.io/app-privilege-requested-at"
	appPrivilegeProcessedAtAnnotation  = "gentianos.io/app-privilege-processed-at"
	appPrivilegeSyncAnnotationPrefix   = "gentianos.io/app-privilege-sync-"
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

	kcURL, kcUser, kcPass, err := r.loadKeycloakAdmin(ctx)
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

		if err := r.syncAppPrivilegedRole(ctx, tenant, profileName, profile, role, members); err != nil {
			syncFailed = true
			r.setCondition(tenant, conditionAppPrivilegesReady, metav1.ConditionFalse,
				"SyncFailed", fmt.Sprintf("%s: %s", profileName, err.Error()))
			continue
		}
		if err := r.persistAppPrivilegeFingerprint(ctx, tenant, profileName, fingerprint); err != nil {
			return ctrl.Result{}, err
		}
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

func (r *TenantReconciler) loadKeycloakAdmin(ctx context.Context) (url, user, pass string, err error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: keycloakAdminSecret, Namespace: kernelNamespace}, secret); err != nil {
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

func (r *TenantReconciler) syncAppPrivilegedRole(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	profileName string,
	profile *gentianov1alpha1.AppProfile,
	role *gentianov1alpha1.PrivilegedRoleSpec,
	members []authz.KeycloakUser,
) error {
	switch role.Kind {
	case gentianov1alpha1.PrivilegedRoleKindGroup:
	default:
		return fmt.Errorf("unsupported privileged role kind %q", role.Kind)
	}
	// Protocol is a wire protocol the operator knows how to speak, not an app
	// name — see the field doc on PrivilegedRoleSpec. Add a case here (and a
	// client package under internal/provisioning/) for each protocol a
	// profile declares; never branch on profileName/family here.
	switch role.Protocol {
	case "mathesar-rpc":
		return r.syncMathesarPrivilegedRole(ctx, tenant, profileName, profile, members)
	default:
		return fmt.Errorf("privileged role sync is not implemented for profile %q (protocol %q)", profileName, role.Protocol)
	}
}

const (
	// mathesarBootstrapUsername is the fixed, protocol-defined username of the
	// technical Mathesar superuser this sync authenticates as. It is not a
	// per-tenant human account (app-admins members get their own accounts,
	// provisioned below) and not a secret itself — only its password is. A
	// "mathesar-rpc" profile's spec.postInstallJob must create exactly this
	// account (see profiles/mathesar/mathesar-ce/profile.yaml in gentian-apps
	// for the reference script).
	mathesarBootstrapUsername = "gentian-bootstrap-admin"

	// mathesarBootstrapPasswordKey is the key inside
	// "<profileName>-sensitive-values" (the same per-app Secret every
	// AppProfile's valueMapping/appSecrets already populate — see the
	// app-default composition and docs/app-profile-guide.md §4) holding that
	// account's password. The profile must declare it via
	// spec.appSecrets[].name: bootstrap_admin_password.
	mathesarBootstrapPasswordKey = "internal-bootstrap_admin_password" //nolint:gosec // secret key name, not a credential.
)

// syncMathesarPrivilegedRole reconciles gentian:tenant:<t>:app-admins
// membership into Mathesar's is_superuser flag via its own /api/rpc/v0/
// JSON-RPC endpoint (see internal/provisioning/mathesar). There is no OIDC
// claim Mathesar reads for this — upstream always creates new SSO logins as
// regular users (mathesar/sso.py) — so a technical bootstrap superuser
// (created once by the profile's spec.postInstallJob) authenticates every
// call this makes.
func (r *TenantReconciler) syncMathesarPrivilegedRole(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	profileName string,
	profile *gentianov1alpha1.AppProfile,
	members []authz.KeycloakUser,
) error {
	if profile.Spec.Ingress == nil || profile.Spec.Ingress.ServiceName == "" || profile.Spec.Ingress.ServicePort == 0 {
		return fmt.Errorf("mathesar-rpc requires spec.ingress.serviceName/servicePort on profile %q", profileName)
	}
	ns := tenantNamespaceName(tenant)

	secret := &corev1.Secret{}
	secretName := profileName + "-sensitive-values"
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, secret); err != nil {
		return fmt.Errorf("load bootstrap credentials from %s/%s: %w", ns, secretName, err)
	}
	bootstrapPass := string(secret.Data[mathesarBootstrapPasswordKey])
	if bootstrapPass == "" {
		return fmt.Errorf("secret %s/%s is missing the bootstrap admin credential (key %q)",
			ns, secretName, mathesarBootstrapPasswordKey)
	}

	// Internal service DNS, never the public tenant hostname — this is a
	// kernel-component-to-tenant-app call (docs/app-profile-guide.md §2).
	baseURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d",
		profile.Spec.Ingress.ServiceName, ns, profile.Spec.Ingress.ServicePort)
	rpc := mathesar.NewClient(baseURL, mathesarBootstrapUsername, bootstrapPass)

	users, err := rpc.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list mathesar users: %w", err)
	}
	byEmail := make(map[string]mathesar.User, len(users))
	for _, u := range users {
		if u.Email != "" {
			byEmail[strings.ToLower(u.Email)] = u
		}
	}

	wantAdmin := make(map[string]bool, len(members))
	for _, m := range members {
		if m.Email == "" {
			// Mathesar links accounts by email; a Keycloak member without one
			// can never be matched to (or provisioned as) a Mathesar user.
			continue
		}
		email := strings.ToLower(m.Email)
		wantAdmin[email] = true

		existing, ok := byEmail[email]
		switch {
		case ok && !existing.IsSuperuser:
			if _, err := rpc.SetSuperuser(ctx, existing, true); err != nil {
				return fmt.Errorf("promote mathesar user %s: %w", email, err)
			}
		case !ok:
			username := m.Username
			if username == "" {
				username = email
			}
			// Pre-provisions the account so the eventual first SSO login
			// links to it (matched by email) instead of creating a fresh,
			// non-superuser one — see mathesar/sso.py's save_user upstream.
			if _, err := rpc.AddUser(ctx, username, email, true); err != nil {
				return fmt.Errorf("provision mathesar user %s: %w", email, err)
			}
		}
	}

	// Demote accounts that lost app-admins membership. The technical
	// bootstrap account is matched by username, not membership — it is what
	// authenticates this very sync and is never a human app-admins member.
	for email, u := range byEmail {
		if u.Username == mathesarBootstrapUsername || !u.IsSuperuser || wantAdmin[email] {
			continue
		}
		if _, err := rpc.SetSuperuser(ctx, u, false); err != nil {
			return fmt.Errorf("demote mathesar user %s: %w", email, err)
		}
	}
	return nil
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
