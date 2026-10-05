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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/locales"
)

// The desktop is the one component the OS ships itself (ui-restructure.md
// §1): every tenant gets one from the same profile, the platform tenant
// included, and its values are the platform's facts -- which realm, which
// director, which zone client -- that no profile author can know.
const (
	// DesktopProfileName is the ComponentProfile the operator chart ships.
	DesktopProfileName = "desktop"
	// DesktopComponentName is the Component each tenant gets from it.
	DesktopComponentName = "desktop"
)

// directorAudience is the audience the zone's edge token carries: the
// director's, because that token is minted for the director and relayed to it
// by whichever platform-trust component received it. A backend that verifies
// the forwarded token requires this audience, which is what stops a token
// minted for something else from being accepted as the zone's session.
const directorAudience = "gentian-director"

// platformValues is what a component is told about the cluster it runs in,
// placed where its profile's valueMapping.platform says its chart takes them.
//
// A profile that names no key receives nothing. This is the whole of what the
// desktop used to receive through a special case keyed on its annotation, and
// the reason that case had to go: a component built from the app template and
// put behind the edge needs exactly these facts, and the only way to give them
// to it was to teach the operator its name too. Now the profile says where it
// wants them and the operator does not know who is asking.
//
// The values are the operator's to know, not the profile's to choose: the
// issuer is the zone's realm on the identity provider, the client is the one
// the edge holds the zone's session with, the audience is what that token
// carries, and the director is where it is. Nothing here is configurable
// except where it lands.
func (r *ComponentReconciler) platformValues(profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant, zone edgeZone) map[string]interface{} {
	out := map[string]interface{}{}
	if profile.Spec.Package.ValueMapping == nil || profile.Spec.Package.ValueMapping.Platform == nil {
		return out
	}
	m := profile.Spec.Package.ValueMapping.Platform
	zoneKind := "tenant"
	if zone.kernel {
		zoneKind = "kernel"
	}
	for key, value := range map[string]string{
		m.IssuerKey:               fmt.Sprintf("https://id.%s/auth/realms/%s", r.KernelDomain, zone.realm),
		m.ZoneClientIDKey:         zone.clientID,
		m.AudienceKey:             directorAudience,
		m.DirectorURLKey:          r.directorURL(),
		m.CredentialManagerURLKey: r.credentialManagerURL(),
		m.ClusterKey:              r.Cluster,
		m.TenantKey:               tenant.Name,
		m.KernelDomainKey:         r.KernelDomain,
		m.RealmKey:                zone.realm,
		m.ZoneKindKey:             zoneKind,
		// The tenant's own language, for a desktop deciding what to render
		// before this person has chosen one and before a settings template
		// has chosen for them (AD-15). Normalise never returns an empty
		// slice, so the index is safe: a tenant declaring nothing gets the
		// platform's first language.
		m.DefaultLanguageKey: locales.Normalise(tenant.Spec.Locales)[0],
	} {
		if key != "" {
			setPath(out, key, value)
		}
	}
	return out
}

// wantsDirector reports whether a profile asked to be told where the director
// is, which is the one platform fact that also changes what the component may
// reach: its egress to the control namespace follows from it.
func wantsDirector(profile *gentianov1alpha1.ComponentProfile) bool {
	return platformMapping(profile) != nil && platformMapping(profile).DirectorURLKey != ""
}

// wantsCredentialManager reports the same for the credential manager, and has
// the same consequence: both live in the control namespace, and a component
// that relays to either is allowed to reach it.
func wantsCredentialManager(profile *gentianov1alpha1.ComponentProfile) bool {
	return platformMapping(profile) != nil && platformMapping(profile).CredentialManagerURLKey != ""
}

func platformMapping(profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.PlatformValueMapping {
	if profile.Spec.Package.ValueMapping == nil {
		return nil
	}
	return profile.Spec.Package.ValueMapping.Platform
}

// databaseValues maps the fulfilled database requirement onto the chart the
// profile's valueMapping names. The credential Secret carries every key
// (host, port, database, username, password, DATABASE_URL); a chart that
// reads them by key names the keys, a chart that takes a Secret takes the
// Secret -- the desktop does the latter through existingSecret.
func databaseValues(profile *gentianov1alpha1.ComponentProfile, secretName string) map[string]interface{} {
	out := map[string]interface{}{}
	if profile.Spec.Package.ValueMapping == nil || profile.Spec.Package.ValueMapping.Database == nil {
		return out
	}
	m := profile.Spec.Package.ValueMapping.Database
	if m.HostKey != "" {
		setPath(out, m.HostKey, map[string]interface{}{"valueFrom": secretName})
	}
	// The plain name, for a chart that consumes the Secret whole. Not the
	// structured reference above: a chart that wants existingSecret.name gets
	// a string there, or its envFrom names a map and nothing mounts.
	if m.SecretNameKey != "" {
		setPath(out, m.SecretNameKey, secretName)
	}
	return out
}

func setPath(dst map[string]interface{}, path string, v interface{}) {
	keys := splitDots(path)
	for i, k := range keys {
		if i == len(keys)-1 {
			dst[k] = v
			return
		}
		next, ok := dst[k].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			dst[k] = next
		}
		dst = next
	}
}

func splitDots(s string) []string {
	var out []string
	cur := ""
	for _, ch := range s {
		if ch == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(ch)
	}
	return append(out, cur)
}

// componentDatabaseNamespace is where a tenant's component databases live,
// and so where its pods must be allowed to go: the kernel's data plane for
// the platform tenant, the tenant postgres of the system tier for every
// other (namespace-cleanup.md §2).
func (r *ComponentReconciler) componentDatabaseNamespace(tenant *gentianov1alpha1.Tenant) string {
	if tenantAdoptsKernelRealm(tenant, r.KernelRealm) {
		return layout.Namespace(layout.Data)
	}
	return postgresNamespace
}

// ensureDatabaseRequirement fulfils a component's database requirement with
// a credential Secret in its namespace.
//
// The platform tenant's data plane is the kernel's (namespace-cleanup.md §2):
// its desktop's database is the portal_shell database kernel-data declares
// on kernel-postgres, and the credential is the one the kernel keeps in the
// vault. Every other tenant's desktop database lives on the tenant postgres
// of the system tier, provisioned here the way a tenant app's is: the
// credential seeded in the vault, the role and database made by a psql Job,
// the database declared as a CloudNativePG Database so a purge finds and
// drops it. The name is the one exports and restores already address.
//
// Either way the credential reaches the component as an ExternalSecret.
func (r *ComponentReconciler) ensureDatabaseRequirement(ctx context.Context, comp *gentianov1alpha1.Component, tenant *gentianov1alpha1.Tenant) (ready bool, reason, message string, err error) {
	var target map[string]interface{}
	var data []interface{}
	if tenantAdoptsKernelRealm(tenant, r.KernelRealm) {
		host := fmt.Sprintf("kernel-postgres-rw.%s.svc.cluster.local", r.componentDatabaseNamespace(tenant))
		target = databaseSecretTemplate(host, "5432", "portal_shell", "portal_shell_user", "{{ .password }}")
		data = []interface{}{
			map[string]interface{}{
				"secretKey": "password",
				"remoteRef": map[string]interface{}{"key": "gentian-os/kernel/database/postgresql", "property": "portal_shell_user_password"},
			},
		}
	} else {
		ready, reason, message, err := r.ensureTenantDatabase(ctx, tenant)
		if err != nil || !ready {
			return false, reason, message, err
		}
		target = databaseSecretTemplate("{{ .host }}", "{{ .port }}", "{{ .name }}", "{{ .user }}", "{{ .password }}")
		path := secrets.CategoryPath(tenant.Name, portalShellAppName, "database")
		for _, p := range []string{"host", "port", "name", "user", "password"} {
			data = append(data, map[string]interface{}{
				"secretKey": p,
				"remoteRef": map[string]interface{}{"key": path, "property": p},
			})
		}
	}
	return r.ensureDatabaseSecret(ctx, comp, target, data)
}

// databaseSecretTemplate is the credential Secret every database requirement
// is fulfilled with: each key by name, and the URL assembled from them.
func databaseSecretTemplate(host, port, database, username, password string) map[string]interface{} {
	return map[string]interface{}{
		"host":         host,
		"port":         port,
		"database":     database,
		"username":     username,
		"password":     password,
		"DATABASE_URL": "postgresql+psycopg://" + username + ":" + password + "@" + host + ":" + port + "/" + database,
	}
}

// ensureTenantDatabase makes the desktop's database on the tenant postgres:
// the vault record first, because the role Job sets the password it holds,
// then the role and database, then the Database resource that records them.
// Once that resource has been applied the Job is not run again: the role
// exists with the seeded password, and the record is write-once.
func (r *ComponentReconciler) ensureTenantDatabase(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ready bool, reason, message string, err error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: "Cluster"})
	if err := r.Get(ctx, types.NamespacedName{Name: cnpgClusterName, Namespace: postgresNamespace}, cluster); err != nil {
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, "DatabaseUnavailable",
				fmt.Sprintf("this cluster composes no tenant postgres (%s/%s); the requirement waits", postgresNamespace, cnpgClusterName), nil
		}
		return false, "", "", err
	}
	if r.Seeder == nil {
		return false, "DatabaseUnavailable", "the operator has no vault to seed the database credential in; the requirement waits", nil
	}
	dbName := databaseName(tenant, portalShellAppName)
	creds, err := r.Seeder.SeedDatabase(ctx, tenant.Name, portalShellAppName, secrets.DatabaseCreds{
		Host: fmt.Sprintf("%s-rw.%s.svc.cluster.local", cnpgClusterName, postgresNamespace),
		Port: "5432",
		Name: dbName,
		User: roleUserName(tenant.Name, portalShellAppName),
	})
	if err != nil {
		return false, "", "", fmt.Errorf("seed the desktop database credential: %w", err)
	}

	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: cnpgDatabaseKind})
	err = r.Get(ctx, types.NamespacedName{Name: databaseCRName(tenant.Name, portalShellAppName), Namespace: postgresNamespace}, db)
	if err == nil {
		if cnpgDatabaseIsReady(db) {
			return true, "", "", nil
		}
		return false, "DatabaseProvisioning", "the database is being created", nil
	}
	if !errors.IsNotFound(err) {
		return false, "", "", err
	}

	desired := makeRoleJob(tenant, tenantNamespaceName(tenant), dbName, portalShellAppName, creds.Password,
		gentianov1alpha1.SchemaPreferenceAppSchema, false)
	// What was asked for, as a hash on the Job. The live Job cannot be
	// compared with the desired one field by field: the API server fills in
	// defaults, so the two never match, and a comparison that never matches
	// deletes the Job on every pass before its pod has run.
	wanted := roleJobHash(desired)
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	desired.Annotations[roleJobHashAnnotation] = wanted
	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: roleJobName(tenant.Name, portalShellAppName), Namespace: postgresNamespace}, job)
	switch {
	case errors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
			return false, "", "", err
		}
		return false, "DatabaseProvisioning", "the database role is being created", nil
	case err != nil:
		return false, "", "", err
	case !jobIsComplete(job) && job.Annotations[roleJobHashAnnotation] != wanted:
		// A Job made by an earlier build of this. Its pod template cannot be
		// changed, and if that template is what kept it from running -- a pod
		// the namespace refuses is no pod at all -- waiting on it is waiting
		// for ever. Replaced, and made again on the next pass.
		prop := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil && !errors.IsNotFound(err) {
			return false, "", "", err
		}
		return false, "DatabaseProvisioning", "the database role Job is replaced by the current one", nil
	case jobIsFailed(job):
		prop := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil && !errors.IsNotFound(err) {
			return false, "", "", err
		}
		return false, "DatabaseProvisioning", "the database role Job failed and is retried", nil
	case !jobIsComplete(job):
		return false, "DatabaseProvisioning", "the database role is being created", nil
	}

	if err := r.Create(ctx, buildDatabaseCR(tenant, tenantNamespaceName(tenant), dbName, portalShellAppName)); err != nil && !errors.IsAlreadyExists(err) {
		return false, "", "", err
	}
	return false, "DatabaseProvisioning", "the database is being created", nil
}

// roleJobHashAnnotation records what a role Job was made from.
const roleJobHashAnnotation = "gentianos.io/role-job-hash"

// roleJobHash is a digest of the containers a role Job runs.
func roleJobHash(job *batchv1.Job) string {
	raw, _ := json.Marshal(job.Spec.Template.Spec.Containers)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// ensureDatabaseSecret delivers the credential into the component's
// namespace and reports whether it has arrived.
func (r *ComponentReconciler) ensureDatabaseSecret(ctx context.Context, comp *gentianov1alpha1.Component, template map[string]interface{}, data []interface{}) (ready bool, reason, message string, err error) {
	name := comp.Name + componentDatabaseSecretSuffix
	spec := map[string]interface{}{
		"refreshInterval": "1h",
		"secretStoreRef":  map[string]interface{}{"name": "openbao", "kind": "ClusterSecretStore"},
		"target": map[string]interface{}{
			"name":           name,
			"creationPolicy": "Owner",
			"template": map[string]interface{}{
				"engineVersion": "v2",
				"data":          template,
			},
		},
		"data": data,
	}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(externalSecretGVK)
	desired.SetName(name)
	desired.SetNamespace(comp.Namespace)
	desired.SetLabels(componentLabels(comp))
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return false, "", "", err
	}
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return false, "", "", err
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(externalSecretGVK)
	err = r.Get(ctx, types.NamespacedName{Name: name, Namespace: comp.Namespace}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return false, "", "", err
		}
		return false, "DatabaseProvisioning", "the database credential is being delivered", nil
	}
	if err != nil {
		return false, "", "", err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
			return false, "", "", err
		}
		if err := r.Patch(ctx, existing, patch); err != nil {
			return false, "", "", err
		}
	}
	if !crossplaneObjectReady(existing) {
		return false, "DatabaseProvisioning", "the database credential is being delivered", nil
	}
	return true, "", "", nil
}

func decodeJSONObject(raw []byte, out *map[string]interface{}) error {
	return json.Unmarshal(raw, out)
}

// componentOfRelease maps a Release back to the Component it was made for:
// its name is <namespace>-<component>, and the labels say which.
func componentOfRelease() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		labels := obj.GetLabels()
		name, ok := labels[componentLabel]
		if !ok || labels[managedByLabel] != managedByValue {
			return nil
		}
		tenant := labels[tenantLabel]
		if tenant == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name, Namespace: layout.Tenant(tenant)}}}
	})
}

// componentsOfProfile re-runs every Component of a profile when the profile
// changes: a new chart version pinned on the profile reaches the Release
// through the components, and nothing else would run them.
func componentsOfProfile(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return componentsReferencingProfile(ctx, c, obj.GetName())
	})
}

func componentsReferencingProfile(ctx context.Context, c client.Reader, profile string) []reconcile.Request {
	list := &gentianov1alpha1.ComponentList{}
	if err := c.List(ctx, list); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ProfileRef.Name == profile {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name, Namespace: list.Items[i].Namespace}})
		}
	}
	return out
}

// componentsOfZoneSecret re-runs every Component when a zone's edge client
// secret appears or changes in the edge namespace: until it exists nothing
// is routed, and once it does the components are what route.
func componentsOfZoneSecret(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj.GetNamespace() != servicesNamespace || !strings.HasPrefix(obj.GetName(), "edge-") || !strings.HasSuffix(obj.GetName(), "-oidc") {
			return nil
		}
		list := &gentianov1alpha1.ComponentList{}
		if err := c.List(ctx, list); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name, Namespace: list.Items[i].Namespace}})
		}
		return out
	})
}

// ensureDefaultComponents gives the tenant a Component of every profile that
// declares defaultForTenants, named after the profile.
//
// The desktop was the one component every tenant got, created here by name.
// The administration console is the second, and rather than teach this
// function a second name the profile says it: a component the platform ships
// to everyone declares that on itself, and this creates whatever declares it.
// Existing Components are left alone. Removing the declaration does not
// delete them, because a tenant's component going away is a tenant-level
// change and this is not where those are decided.
func (r *TenantReconciler) ensureDefaultComponents(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	profiles := &gentianov1alpha1.ComponentProfileList{}
	if err := r.List(ctx, profiles); err != nil {
		return err
	}
	for i := range profiles.Items {
		profile := &profiles.Items[i]
		if !profile.Spec.DefaultForTenants || !classIncludes(profile, gentianov1alpha1.ComponentClassApp) {
			continue
		}
		desired := &gentianov1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      profile.Name,
				Namespace: tenantNamespaceName(tenant),
				Labels: map[string]string{
					tenantLabel:    tenant.Name,
					managedByLabel: managedByValue,
				},
			},
			Spec: gentianov1alpha1.ComponentSpec{
				ProfileRef: gentianov1alpha1.ProfileRef{Name: profile.Name},
				Class:      gentianov1alpha1.ComponentClassApp,
				Privileges: tenantPrivilegeGrants(tenant, profile.Name),
				Exposures:  tenantExposures(tenant, profile.Name),
			},
		}
		if err := controllerutil.SetControllerReference(tenant, desired, r.Scheme); err != nil {
			return err
		}
		existing := &gentianov1alpha1.Component{}
		err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, desired); err != nil {
				return fmt.Errorf("create %s component: %w", profile.Name, err)
			}
			continue
		}
		if err != nil {
			return err
		}
		// A Component written before spec.tenancy became spec.class has
		// neither: the API server returns the stored object untouched, and
		// the rename left the field this reconciler reads empty. Such a
		// component reports ClassUnsupported and its release is never
		// reconciled again.
		//
		// It is REPAIRED, not replaced. Deleting it was the first attempt and
		// it deadlocked the cluster: the object has a finalizer, removing a
		// finalizer is an update of the whole object, and the whole object is
		// invalid while class is empty — so the delete never completed, the
		// create that followed it answered AlreadyExists, and both shipped
		// components sat terminating and un-finalizable with the reconciler
		// erroring once a minute. Setting the field is one small write and
		// the immutability rule admits it from empty for exactly this.
		if existing.Spec.Class == "" {
			patch := client.MergeFrom(existing.DeepCopy())
			existing.Spec.Class = gentianov1alpha1.ComponentClassApp
			if err := r.Patch(ctx, existing, patch); err != nil {
				return fmt.Errorf("set spec.class on the %s component written before the rename: %w", profile.Name, err)
			}
		}
		// Grants approved after the install reach the component here. This is
		// the whole of the approval path's second half: the director commits
		// to the Tenant, Argo applies it, and a component that was holding on
		// a pending privilege is reconciled again with the grant in hand. An
		// approval therefore takes effect without anybody touching the
		// Component, which is what keeps git the only writer.
		if want := tenantPrivilegeGrants(tenant, profile.Name); !equality.Semantic.DeepEqual(existing.Spec.Privileges, want) {
			patch := client.MergeFrom(existing.DeepCopy())
			existing.Spec.Privileges = want
			if err := r.Patch(ctx, existing, patch); err != nil {
				return fmt.Errorf("set the granted privileges on the %s component: %w", profile.Name, err)
			}
		}
		// And what a perimeter approver published, for the same reason: the
		// decision is a commit, and this is how it reaches the Component
		// whose reconcile stands the proxy up. Withdrawing one takes the
		// surface down by the same route.
		if want := tenantExposures(tenant, profile.Name); !equality.Semantic.DeepEqual(existing.Spec.Exposures, want) {
			patch := client.MergeFrom(existing.DeepCopy())
			existing.Spec.Exposures = want
			if err := r.Patch(ctx, existing, patch); err != nil {
				return fmt.Errorf("set the published exposures on the %s component: %w", profile.Name, err)
			}
		}
	}
	return nil
}

// tenantExposures are the surfaces a perimeter approver published from one
// component, in the order the Tenant lists them.
//
// An entry naming a component that is not installed is skipped rather than
// dropped: a decision to publish outlives a reinstall, and asking somebody to
// approve the same thing twice is how approvals become a formality.
func tenantExposures(tenant *gentianov1alpha1.Tenant, install string) []gentianov1alpha1.ExposureEnablement {
	var out []gentianov1alpha1.ExposureEnablement
	for i := range tenant.Spec.Exposures {
		if tenant.Spec.Exposures[i].Install == install {
			out = append(out, tenant.Spec.Exposures[i].Enablement())
		}
	}
	return out
}

// tenantPrivilegeGrants are the tenant's grants for one component, in the
// order the Tenant lists them so that a patch is written once rather than on
// every reconcile.
//
// Grants naming a component that is not installed are skipped rather than
// dropped from the Tenant: the record of an approval outlives the install it
// was given for, and reinstalling an app should not mean asking again.
func tenantPrivilegeGrants(tenant *gentianov1alpha1.Tenant, install string) []gentianov1alpha1.PrivilegeGrant {
	var out []gentianov1alpha1.PrivilegeGrant
	for i := range tenant.Spec.Privileges {
		if tenant.Spec.Privileges[i].Install == install {
			out = append(out, tenant.Spec.Privileges[i].Grant())
		}
	}
	return out
}

// classIncludes reports whether a profile is certified for a class.
func classIncludes(profile *gentianov1alpha1.ComponentProfile, c gentianov1alpha1.ComponentClass) bool {
	for _, have := range profile.Spec.Classes {
		if have == c {
			return true
		}
	}
	return false
}
