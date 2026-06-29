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
	"github.com/gentian-org/gentian-os/internal/meta"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/tiles"
)

const (
	conditionLDAPReady  = "LDAPReady"
	udmProvisionerImage = "curlimages/curl:8.7.1"
	udmAdminSecret      = "udm-admin"
)

// ensureLDAP provisions per-tenant LDAP organisational units, default groups,
// delegated admin user/policy, and per-app bind accounts via the UDM REST API.
// Jobs run in the kernel namespace and are idempotent (check-before-create).
// Returns a non-zero RequeueAfter while Jobs are still running.
//
// Steps 1-3 (OU, admin user, admin policy, bind accounts) are owned by the
// manifest bridge and waited on here when LDAP apps (or Keycloak federation) apply.
//
// Admin user must run BEFORE admin policy: the policy job updates the portal
// entry allowedGroups, and the Nubus portal consumer's groups cache must
// already contain the admin user in admins_<tenant> at that point. If the
// policy job ran first (old order), the portal server would see the group in
// allowedGroups but find it empty in the cache, so admin tiles would not show
// until the user reloaded the portal after the subsequent user job completed.
func (r *TenantReconciler) ensureLDAP(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	// LDAP / UDM provisioning is disabled — Keycloak is the identity authority (Suze path).
	// Legacy implementation remains in this file (makeOUJob, makeMBAGroupsJob, …) for migration reference.
	r.setCondition(tenant, conditionLDAPReady, metav1.ConditionTrue,
		"SkippedKeycloakNative", "LDAP provisioning disabled; Keycloak is authoritative")
	return ctrl.Result{}, nil
}

// ensureLDAP_legacy was the OpenDesk/Nubus LDAP provisioning path (removed from reconcile).
// See git history before Suze cutover if re-enabling legacy-ldap clusters.

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
			// Tenant Admins is a catalogue-level alias for per-tenant cn=admins_<tenant>.
			if allowedGroupCN == "Tenant Admins" {
				allowedGroupCN = "admins_" + tenant.Name
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
			resolvedLogo, err := tiles.ResolveLogo(
				profile.Spec.Tile,
				profile.Spec.Logo,
				tile.Tile,
				tile.Logo,
			)
			if err != nil {
				return nil, fmt.Errorf("resolve portal tile logo for %s/%s: %w", app.Profile, tile.Name, err)
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
				Logo:           tiles.LogoBase64(resolvedLogo),
			})
		}
	}
	return result, nil
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
		// If a Retain-path lock job ran due to a race (ArgoCD selfHeal), remove it
		// and proceed with destructive OU deletion.
		lockJobName := ldapLockJobName(tenant.Name)
		lockJob := &batchv1.Job{}
		if lockErr := r.Get(ctx, types.NamespacedName{Name: lockJobName, Namespace: kernelNamespace}, lockJob); lockErr == nil {
			prop := metav1.DeletePropagationBackground
			_ = r.Delete(ctx, lockJob, &client.DeleteOptions{PropagationPolicy: &prop})
		} else if lockErr != nil && !errors.IsNotFound(lockErr) {
			return lockErr
		}

		jobName := ouDeleteJobName(tenant.Name)
		existing := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, existing)
		if err == nil {
			if jobIsComplete(existing) {
				// Delete provisioning jobs so they are re-created on the next deploy.
				r.deleteProvisioningJobs(ctx,
					ouJobName(tenant.Name),
					mbaGroupsJobName(tenant.Name),
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
			mbaGroupsJobName(tenant.Name),
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
				mbaGroupsJobName(tenant.Name),
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

// --- Job constructors --------------------------------------------------------

func makeLockOUJob(tenant *gentianov1alpha1.Tenant, ouDN string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
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

func makePortalEntryDeleteJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
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
	ttl := meta.ProvisioningJobTTLSeconds
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
		Resources: meta.InitJobResources(),
	}
}

// --- Shell scripts -----------------------------------------------------------

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

// buildOUDeleteScript removes all users under the tenant OU (subtree search),
// then deletes the tenant OU recursively. A subtree user sweep is required because
// UDM can return 404 for the OU container while orphaned user entries remain
// (e.g. after Retain lock + partial cleanup).
func buildOUDeleteScript(ouDN string) string {
	return fmt.Sprintf(`set -eu
urlencode() { printf '%%s' "$1" | sed 's/%%/%%25/g; s/ /%%20/g; s/,/%%2C/g; s/=/%%3D/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
# OU_POS: ${UDM_LDAP_BASE} expands at runtime.
OU_POS="%s"
OU_ENC=$(urlencode "${OU_POS}")

echo "deleting users under ${OU_POS} (subtree search)"
USERS_JSON=$(curl -s --max-time 30 ${CREDS} \
  -H "Accept: application/json" \
  "${BASE_URL}/users/user/?position=${OU_ENC}&scope=sub")
printf '%%s' "${USERS_JSON}" | grep -o '"dn": *"[^"]*"' | sed 's/"dn": *"//;s/"$//' | while IFS= read -r USER_DN; do
  if [ -n "${USER_DN}" ]; then
    USER_ENC=$(urlencode "${USER_DN}")
    HTTP=$(curl -s -o /dev/null -w "%%{http_code}" -X DELETE ${CREDS} \
      -H "Accept: application/json" \
      "${BASE_URL}/users/user/${USER_ENC}")
    echo "deleted user ${USER_DN} (HTTP ${HTTP})"
    case "${HTTP}" in
      200|204|404) ;;
      *) echo "ERROR: user delete failed for ${USER_DN} (HTTP ${HTTP})" >&2; exit 1 ;;
    esac
  fi
done
echo "user subtree sweep complete for ${OU_POS}"

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

func ouJobName(tenantName string) string {
	return fmt.Sprintf("ldap-ou-%s", tenantName)
}

func mbaGroupsJobName(tenantName string) string {
	return fmt.Sprintf("ldap-mba-groups-%s", tenantName)
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

func portalEntryJobName(tenantName, appName string) string {
	return fmt.Sprintf("ldap-portal-entry-%s-%s", tenantName, appName)
}

func portalEntryDeleteJobName(tenantName, appName string) string {
	return fmt.Sprintf("ldap-portal-entry-delete-%s-%s", tenantName, appName)
}

// --- Portal entry scripts ----------------------------------------------------

// portalRealtimeLinkTargets returns meet/chat base URLs for kernel portal contact
// actions when the tenant has Element installed (Jitsi is bundled as a sidecar).
func (r *TenantReconciler) portalRealtimeLinkTargets(tenant *gentianov1alpha1.Tenant) (meetURL, chatURL string) {
	effectiveDomain := r.tenantEffectiveDomain(tenant)
	if effectiveDomain == "" {
		return "", ""
	}
	for _, app := range tenant.Spec.Apps {
		if app.Profile == "element" {
			chatURL = fmt.Sprintf("https://chat.%s", effectiveDomain)
			meetURL = fmt.Sprintf("https://meet.%s", effectiveDomain)
		}
	}
	return meetURL, chatURL
}

// buildPortalRealtimeLinksScript creates or updates UDM portal entries used when
// starting a video call or chat from the contacts UI. Each tenant gets
// swp.realtime_videoconference_<tenant> and swp.realtime_collaboration_<tenant>
// with allowedGroups scoped to that tenant's LDAP OU. In single-tenancy mode,
// legacy OpenDesk entry names (swp.realtime_*) are also maintained.
func buildPortalRealtimeLinksScript(tenantName, ouDN, meetURL, chatURL string, includeLegacy bool) string {
	var body strings.Builder
	body.WriteString(`set -eu
urlencode() { printf '%s' "$1" | sed 's/%/%25/g; s/ /%20/g; s/,/%2C/g'; }
CREDS="-u Administrator:${UDM_ADMIN_PASSWORD}"
BASE_URL="${UDM_URL}/udm"
OU_POS="`)
	body.WriteString(ouDN)
	fmt.Fprintf(&body, `"
USERS_GRP_DN="cn=users_%s,${OU_POS}"
ensure_realtime_entry() {`, tenantName)
	body.WriteString(`
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
