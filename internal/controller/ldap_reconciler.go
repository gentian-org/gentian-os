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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	conditionLDAPReady  = "LDAPReady"
	udmProvisionerImage = "curlimages/curl:8.7.1"
	udmAdminSecret      = "udm-admin"
	ldapRequeueAfter    = 30 * time.Second
)

// ensureLDAP provisions per-tenant LDAP organisational units, default groups,
// and per-app bind accounts via the UDM REST API. Jobs run in the kernel
// namespace and are idempotent (check-before-create). Returns a non-zero
// RequeueAfter while Jobs are still running.
func (r *TenantReconciler) ensureLDAP(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	ouDN := tenantOUDN(tenant)

	// Step 1 — tenant OU + default groups (always required for Nubus user management)
	ouDone, err := r.ensureOUJob(ctx, tenant, ouDN)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure LDAP OU Job: %w", err)
	}
	if !ouDone {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionFalse,
			"ProvisioningOU", "Waiting for UDM OU Job to complete")
		return ctrl.Result{RequeueAfter: ldapRequeueAfter}, nil
	}

	// Step 2 — tenant-scoped delegated admin policy (always required for Nubus user management)
	adminPolicyDone, err := r.ensureAdminPolicyJob(ctx, tenant, ouDN)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure LDAP admin policy Job: %w", err)
	}
	if !adminPolicyDone {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionFalse,
			"ProvisioningAdminPolicy", "Waiting for UDM admin policy Job to complete")
		return ctrl.Result{RequeueAfter: ldapRequeueAfter}, nil
	}

	// Step 2.5 — tenant admin UDM user (so the admin can log into the Nubus portal)
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

	// Step 3 — per-app bind accounts (only for apps that require LDAP)
	ldapApps, err := r.collectLDAPApps(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

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

	if !allDone {
		r.setCondition(tenant, conditionLDAPReady, metav1.ConditionFalse,
			"ProvisioningBindAccounts", "Waiting for UDM bind account Jobs to complete")
		return ctrl.Result{RequeueAfter: ldapRequeueAfter}, nil
	}

	r.setCondition(tenant, conditionLDAPReady, metav1.ConditionTrue,
		"Provisioned", "LDAP OU, groups, bind accounts, and admin policy are ready")
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

// ensureAdminUserJob creates the UDM users/user Job for the tenant admin if absent.
// The admin is added to admins_<tenant> so the UMC delegated admin policy takes effect.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureAdminUserJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, ouDN string, creds secrets.TenantAdminCreds) (bool, error) {
	jobName := adminUserJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
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
				BindDN: fmt.Sprintf("uid=app-%s,%s", appName, ouDN),
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
// DeletionPolicy=Delete creates a UDM Job that removes the tenant OU
// (cascading all child entries). DeletionPolicy=Retain is a no-op.
func (r *TenantReconciler) deleteLDAP(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	ouDN := tenantOUDN(tenant)
	jobName := ouDeleteJobName(tenant.Name)
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, makeOUDeleteJob(tenant, ouDN))
}

// --- Job constructors --------------------------------------------------------

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
	c := udmContainer("provision-bind-account", buildBindAccountScript(ouDN, appName))
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
					Containers: []corev1.Container{
						udmContainer("provision-admin-policy", buildAdminPolicyScript(ouDN, tenant.Name)),
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
else
  echo "OU %s already exists (HTTP ${STATUS})"
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
else
  echo "group users_%s already exists"
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
else
  echo "group admins_%s already exists"
fi`,
		ouDN, tenantName, tenantName, tenantName, tenantName,
		tenantName, tenantName, tenantName, tenantName,
		tenantName, tenantName, tenantName, tenantName)
}

// buildBindAccountScript creates a service-account user that apps use as the LDAP bind DN.
// Uses users/ldap object type which only requires username and password.
func buildBindAccountScript(ouDN, appName string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
# OU_POS and BIND_DN: ${UDM_LDAP_BASE} expands at runtime via shell.
OU_POS="%s"
BIND_DN="uid=app-%s,${OU_POS}"
BIND_DN_ENC=$(urlencode "${BIND_DN}")

STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
  -H "Accept: application/json" \
	"${BASE_URL}/users/ldap/${BIND_DN_ENC}")
if [ "${STATUS}" = "404" ]; then
  if [ -z "${BIND_PW:-}" ]; then
    BIND_PW=$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 20)
  fi
  curl -s -o /dev/null -X POST ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "${BASE_URL}/users/ldap/" \
    -d "{\"properties\":{\"username\":\"app-%s\",\"password\":\"${BIND_PW}\"},\"position\":\"${OU_POS}\"}"
  echo "bind account app-%s created in ${OU_POS}"
else
  echo "bind account app-%s already exists (HTTP ${STATUS})"
fi`, ouDN, appName, appName, appName, appName)
}

// buildAdminUserScript creates the tenant admin as a users/user in the tenant
// OU and adds them to admins_<tenant>. ADMIN_USERNAME and ADMIN_PASSWORD are
// injected as environment variables by the Job constructor. The call is
// idempotent: it ensures the mail domain exists, creates the user if missing,
// and then ensures group membership.
func buildAdminUserScript(ouDN, tenantName, adminEmail string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
OU_POS="%s"
ADMIN_EMAIL="%s"
MAIL_DOMAIN="${ADMIN_EMAIL##*@}"
MAIL_DOMAIN_CONTAINER="cn=domain,cn=mail,${UDM_LDAP_BASE}"
ADMIN_DN="uid=${ADMIN_USERNAME},${OU_POS}"
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
		-d "{\"properties\":{\"username\":\"${ADMIN_USERNAME}\",\"password\":\"${ADMIN_PASSWORD}\",\"firstname\":\"Tenant\",\"lastname\":\"Admin\",\"mailPrimaryAddress\":\"${ADMIN_EMAIL}\",\"pwdChangeNextLogin\":false},\"position\":\"${OU_POS}\"}"
  echo "UDM user ${ADMIN_USERNAME} created in ${OU_POS}"
else
  echo "UDM user ${ADMIN_USERNAME} already exists (HTTP ${STATUS})"
fi

# Ensure the admin user does not require a forced password change on next login.
curl -sf --max-time 30 -X PATCH ${CREDS} \
	-H "Content-Type: application/json" \
	-H "Accept: application/json" \
	"${BASE_URL}/users/user/${ADMIN_DN_ENC}" \
	-d "{\"properties\":{\"pwdChangeNextLogin\":false}}"
echo "pwdChangeNextLogin cleared for ${ADMIN_USERNAME}"

# Ensure the admin user is in the admins_<tenant> group (idempotent PATCH).
ADMINS_BODY=$(curl -s --max-time 30 ${CREDS} \
  -H "Accept: application/json" \
	"${BASE_URL}/groups/group/${ADMINS_GRP_ENC}")
if echo "${ADMINS_BODY}" | grep -q "\"${ADMIN_DN}\""; then
  echo "user ${ADMIN_USERNAME} already in admins group"
else
  curl -sf --max-time 30 -X PATCH ${CREDS} \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
	"${BASE_URL}/groups/group/${ADMINS_GRP_ENC}" \
    -d "{\"properties\":{\"users\":[\"${ADMIN_DN}\"]}}"
  echo "user ${ADMIN_USERNAME} added to admins group"
fi`, ouDN, adminEmail, tenantName)
}

// buildAdminPolicyScript configures UMC/UDM delegated admin policy for one
// tenant OU. It binds admins_<tenant> to UDM user/group management scoped to
// the tenant subtree only.
func buildAdminPolicyScript(ouDN, tenantName string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"

OU_POS="%s"
ADMINS_GRP_DN="cn=admins_%s,${OU_POS}"
POLICY_DN="cn=tenant-admins-%s,cn=UMC,cn=policies,${UDM_LDAP_BASE}"
MODULE_DN="cn=tenant-%s,cn=udm,cn=settings,${UDM_LDAP_BASE}"

POLICY_ENC=$(urlencode "${POLICY_DN}")
MODULE_ENC=$(urlencode "${MODULE_DN}")
ADMINS_GRP_ENC=$(urlencode "${ADMINS_GRP_DN}")

# Ensure tenant admins group exists.
STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/groups/group/${ADMINS_GRP_ENC}")
if [ "${STATUS}" = "404" ]; then
	echo "admins group ${ADMINS_GRP_DN} is missing"
	exit 1
fi

# Ensure UMC policy exists and is linked to admins_<tenant>.
STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/policies/umc/${POLICY_ENC}")
if [ "${STATUS}" = "404" ]; then
	curl -s -o /dev/null -X POST ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${BASE_URL}/policies/umc/" \
		-d "{\"properties\":{\"name\":\"tenant-admins-%s\",\"requiredObjectClasses\":[\"top\"]},\"position\":\"cn=UMC,cn=policies,${UDM_LDAP_BASE}\",\"policies\":{\"groups/group\":[\"${ADMINS_GRP_DN}\"]}}"
	echo "UMC policy tenant-admins-%s created"
else
	echo "UMC policy tenant-admins-%s already exists"
fi

# Ensure UDM module scope entry exists for tenant OU.
STATUS=$(curl -s -o /dev/null -w "%%{http_code}" ${CREDS} \
	-H "Accept: application/json" \
	"${BASE_URL}/settings/udm_module/${MODULE_ENC}")
if [ "${STATUS}" = "404" ]; then
	curl -s -o /dev/null -X POST ${CREDS} \
		-H "Content-Type: application/json" \
		-H "Accept: application/json" \
		"${BASE_URL}/settings/udm_module/" \
		-d "{\"properties\":{\"name\":\"tenant-%s\",\"module\":\"users/user\",\"subtree\":\"${OU_POS}\"},\"position\":\"cn=udm,cn=settings,${UDM_LDAP_BASE}\"}"
	echo "UDM module scope tenant-%s created"
else
	echo "UDM module scope tenant-%s already exists"
fi`,
		ouDN, tenantName, tenantName, tenantName,
		tenantName, tenantName, tenantName,
		tenantName, tenantName, tenantName)
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
echo "OU %s deletion requested (HTTP ${HTTP})"`, ouDN, ouDN)
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

func ouDeleteJobName(tenantName string) string {
	return fmt.Sprintf("ldap-ou-delete-%s", tenantName)
}
