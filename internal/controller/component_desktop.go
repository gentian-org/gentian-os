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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/locales"
	"github.com/gentian-org/gentian-os/internal/placement"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// The desktop is the one component the OS ships itself (admin-console.md
// §1.2): every tenant gets one from the same profile, the platform tenant
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
		m.IssuerKey:       fmt.Sprintf("https://id.%s/auth/realms/%s", r.KernelDomain, zone.realm),
		m.ZoneClientIDKey: zone.clientID,
		m.AudienceKey:     directorAudience,
		m.DirectorURLKey:  r.directorURL(),
		m.UsherURLKey:     r.usherURL(),
		m.CustodianURLKey: r.custodianURL(),
		m.RegistrarURLKey: r.registrarURL(),
		m.ClusterKey:      r.Cluster,
		m.TenantKey:       tenant.Name,
		m.KernelDomainKey: r.KernelDomain,
		m.RealmKey:        zone.realm,
		m.ZoneKindKey:     zoneKind,
		// The tenant's own language, for a desktop deciding what to render
		// before this person has chosen one and before a settings template
		// has chosen for them (AD-15). Normalise never returns an empty
		// slice, so the index is safe: a tenant declaring nothing gets the
		// platform's first language.
		m.DefaultLanguageKey: locales.Normalise(tenant.Spec.Locales)[0],
		// The tenant's country, or "" while it names none.
		m.CountryKey: tenant.Spec.Country,
		// The tenant's unique id, which never changes.
		m.TenantUIDKey: string(tenant.UID),
	} {
		if key != "" {
			setPath(out, key, value)
		}
	}
	// Only a zone the tenant names: while it names none, the profile's own
	// default at that key stands rather than an empty string over it.
	if m.TimeZoneKey != "" && tenant.Spec.Organization != nil && tenant.Spec.Organization.TimeZone != "" {
		setPath(out, m.TimeZoneKey, tenant.Spec.Organization.TimeZone)
	}
	if m.OrganizationKey != "" {
		setPath(out, m.OrganizationKey, organizationValues(tenant))
	}
	return out
}

// organizationValues is who the tenant is, as organizationKey delivers it:
// every field present, "" where the tenant has not filled it in, so a chart
// tests a value rather than its presence.
func organizationValues(tenant *gentianov1alpha1.Tenant) map[string]interface{} {
	org := gentianov1alpha1.TenantOrganization{}
	if tenant.Spec.Organization != nil {
		org = *tenant.Spec.Organization
	}
	addr := gentianov1alpha1.TenantAddress{}
	if org.Address != nil {
		addr = *org.Address
	}
	month := org.FiscalYearStartMonth
	if month == 0 {
		month = 1
	}
	languages := []interface{}{}
	for _, lang := range locales.Normalise(tenant.Spec.Locales) {
		languages = append(languages, lang)
	}
	return map[string]interface{}{
		"complete":             len(tenant.Spec.MissingOrganization()) == 0,
		"displayName":          tenant.Spec.DisplayName,
		"legalName":            org.LegalName,
		"country":              tenant.Spec.Country,
		"languages":            languages,
		"currency":             org.Currency,
		"timeZone":             org.TimeZone,
		"fiscalYearStartMonth": int64(month),
		"address": map[string]interface{}{
			"street":     addr.Street,
			"postalCode": addr.PostalCode,
			"city":       addr.City,
			"region":     addr.Region,
		},
		"website":            org.Website,
		"email":              org.Email,
		"phone":              org.Phone,
		"taxId":              org.TaxID,
		"registrationNumber": org.RegistrationNumber,
	}
}

// wantsDirector reports whether a profile asked to be told where the director
// is, which is the one platform fact that also changes what the component may
// reach: its egress to the control namespace follows from it.
func wantsDirector(profile *gentianov1alpha1.ComponentProfile) bool {
	return platformMapping(profile) != nil && platformMapping(profile).DirectorURLKey != ""
}

// wantsUsher reports whether a profile asked to be told where the usher is.
// It runs in the edge namespace, so asking opens the way there.
func wantsUsher(profile *gentianov1alpha1.ComponentProfile) bool {
	return platformMapping(profile) != nil && platformMapping(profile).UsherURLKey != ""
}

// wantsCustodian reports the same for the custodian, and has
// the same consequence: both live in the control namespace, and a component
// that relays to either is allowed to reach it.
func wantsCustodian(profile *gentianov1alpha1.ComponentProfile) bool {
	return platformMapping(profile) != nil && platformMapping(profile).CustodianURLKey != ""
}

// wantsRegistrar reports the same for the registrar, which is in the control
// namespace too.
func wantsRegistrar(profile *gentianov1alpha1.ComponentProfile) bool {
	return platformMapping(profile) != nil && platformMapping(profile).RegistrarURLKey != ""
}

func platformMapping(profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.PlatformValueMapping {
	if profile.Spec.Package.ValueMapping == nil {
		return nil
	}
	return profile.Spec.Package.ValueMapping.Platform
}

// databaseValues maps the fulfilled database requirement onto the chart the
// profile's valueMapping names, for a chart that takes the credential Secret
// by its name -- the desktop does, through existingSecret. The Secret
// carries every key (host, port, database, username, password,
// DATABASE_URL).
func databaseValues(profile *gentianov1alpha1.ComponentProfile, secretName string) map[string]interface{} {
	out := map[string]interface{}{}
	if profile.Spec.Package.ValueMapping == nil || profile.Spec.Package.ValueMapping.Database == nil {
		return out
	}
	m := profile.Spec.Package.ValueMapping.Database
	// The plain name, for a chart that consumes the Secret whole. The keys
	// one by one (hostKey and the rest) are set entries reading the
	// component's sensitive Secret (component_integrations.go).
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

// componentDatabaseNamespace is where a component's database lives, and so
// where its pods must be allowed to go. An app the tenant installed has its
// database on the shared store of its engine, made by the tenant's stores.
// The desktop's is on the kernel's data plane for the platform tenant, and
// on the tenant postgres of the system tier for every other
// (kernel.md §6.1).
func (r *ComponentReconciler) componentDatabaseNamespace(tenant *gentianov1alpha1.Tenant, profile *gentianov1alpha1.ComponentProfile) string {
	if !operatorPlaced(profile) {
		if provisioner.DatabaseEngineOf(profile) == gentianov1alpha1.DatabaseEngineMariaDB {
			return mariadbNamespace
		}
		return postgresNamespace
	}
	if tenantAdoptsKernelRealm(tenant, r.KernelRealm) {
		return layout.Namespace(layout.Data)
	}
	return postgresNamespace
}

// ensureDatabaseRequirement fulfils a component's database requirement with
// a credential Secret in its namespace.
//
// The platform tenant's data plane is the kernel's (kernel.md §6.1):
// its desktop's database is the portal_shell database kernel-data declares
// on kernel-postgres, under a role of its own whose password the installer
// seeds at the tenant's own path. Every other tenant's desktop database
// lives on the tenant postgres of the system tier, provisioned here the way
// a tenant app's is: the
// credential seeded in the vault, the role and database made by a psql Job,
// the database declared as a CloudNativePG Database so a purge finds and
// drops it. The name is the one exports and restores already address.
//
// Either way the credential reaches the component as an ExternalSecret.
func (r *ComponentReconciler) ensureDatabaseRequirement(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) (ready bool, reason, message string, err error) {
	var target map[string]interface{}
	var data []interface{}
	// The tenant's own store and the tenant's own path, for every tenant:
	// nothing of the kernel's is read into a tenant's namespace.
	store := secrets.TenantStore(tenant.Name)
	path := secrets.CategoryPath(tenant.Name, portalShellAppName, "database")
	// An app the tenant installed has its database made by the tenant's
	// stores, under its own name (kernel_requirement.go), as an app the
	// Composition delivers has: the credential is read from its own path,
	// and the Secret arrives once the record is there. Only a component the
	// platform places itself -- the desktop -- has the one made here.
	if !operatorPlaced(profile) {
		own := secrets.CategoryPath(tenant.Name, comp.Name, "database")
		target = databaseSecretTemplate("{{ .host }}", "{{ .port }}", "{{ .name }}", "{{ .user }}", "{{ .password }}")
		if provisioner.DatabaseEngineOf(profile) == gentianov1alpha1.DatabaseEngineMariaDB {
			target["DATABASE_URL"] = "mysql://{{ .user }}:{{ .password }}@{{ .host }}:{{ .port }}/{{ .name }}"
		}
		for _, p := range []string{"host", "port", "name", "user", "password"} {
			data = append(data, map[string]interface{}{
				"secretKey": p,
				"remoteRef": map[string]interface{}{"key": own, "property": p},
			})
		}
		return r.ensureDatabaseSecret(ctx, comp, store, target, data)
	}
	if tenantAdoptsKernelRealm(tenant, r.KernelRealm) {
		// The names the kernel's chart declares, which a backup of this
		// tenant captures by too (backup/desktop.go). Only the password is
		// read: the role is the desktop's alone, and its password is seeded
		// at this path, which the kernel's chart sets the role to.
		target = databaseSecretTemplate(backup.KernelPostgresHost(), backup.KernelPostgresPort,
			backup.KernelDesktopDatabase, backup.KernelDesktopRole, "{{ .password }}")
		data = []interface{}{
			map[string]interface{}{
				"secretKey": "password",
				"remoteRef": map[string]interface{}{"key": path, "property": "password"},
			},
		}
	} else {
		ready, reason, message, err := r.ensureTenantDatabase(ctx, tenant)
		if err != nil || !ready {
			return false, reason, message, err
		}
		target = databaseSecretTemplate("{{ .host }}", "{{ .port }}", "{{ .name }}", "{{ .user }}", "{{ .password }}")
		for _, p := range []string{"host", "port", "name", "user", "password"} {
			data = append(data, map[string]interface{}{
				"secretKey": p,
				"remoteRef": map[string]interface{}{"key": path, "property": p},
			})
		}
	}
	return r.ensureDatabaseSecret(ctx, comp, store, target, data)
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
		"DATABASE_URL": "postgresql+psycopg://" + username + ":" + password + "@" + host + ":" + port + "/" + database + "?sslmode=require",
	}
}

// ensureTenantDatabase makes the desktop's database on the tenant postgres:
// the vault record first, because the role Job sets the password it holds,
// then the role and database, then the Database resource that records them.
// Once that resource has been applied the Job is not run again: the role
// exists with the seeded password, and the record is write-once.
func (r *ComponentReconciler) ensureTenantDatabase(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ready bool, reason, message string, err error) {
	return ensurePostgresStore(ctx, r.Client, r.Seeder, tenant, portalShellAppName, gentianov1alpha1.SchemaPreferenceAppSchema, false)
}

// ensurePostgresStore makes one database on the tenant postgres, with its
// role, its vault record and the Database resource that records it, for a
// name that is no installed app's -- an installed app's are made through the
// tenant's Composition (tenant_data_plane_manifests.go), with these same
// pieces. Two callers: the desktop's database, and the database of an app
// whose data a restore brings back as retained into a tenant that has none.
func ensurePostgresStore(ctx context.Context, c client.Client, seeder *secrets.Seeder, tenant *gentianov1alpha1.Tenant, appName string,
	schemaPref gentianov1alpha1.SchemaPreference, allowCreateDB bool) (ready bool, reason, message string, err error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: "Cluster"})
	if err := c.Get(ctx, types.NamespacedName{Name: cnpgClusterName, Namespace: postgresNamespace}, cluster); err != nil {
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, "DatabaseUnavailable",
				fmt.Sprintf("this cluster composes no tenant postgres (%s/%s); the requirement waits", postgresNamespace, cnpgClusterName), nil
		}
		return false, "", "", err
	}
	if seeder == nil {
		return false, "DatabaseUnavailable", "the operator has no vault to seed the database credential in; the requirement waits", nil
	}
	dbName := databaseName(tenant, appName)
	creds, err := seeder.SeedDatabase(ctx, tenant.Name, appName, secrets.DatabaseCreds{
		Host: fmt.Sprintf("%s-rw.%s.svc.cluster.local", cnpgClusterName, postgresNamespace),
		Port: "5432",
		Name: dbName,
		User: roleUserName(tenant.Name, appName),
	})
	if err != nil {
		return false, "", "", fmt.Errorf("seed the database credential of %s: %w", appName, err)
	}

	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: cnpgDatabaseKind})
	err = c.Get(ctx, types.NamespacedName{Name: databaseCRName(tenant.Name, appName), Namespace: postgresNamespace}, db)
	if err == nil {
		if cnpgDatabaseIsReady(db) {
			return true, "", "", nil
		}
		return false, "DatabaseProvisioning", "the database is being created", nil
	}
	if !errors.IsNotFound(err) {
		return false, "", "", err
	}

	// The role's password reaches the Job in a Secret the Job owns, handed
	// over once the Job is there.
	desired := makeRoleJob(tenant, tenantNamespaceName(tenant), dbName, appName, true,
		schemaPref, allowCreateDB)
	password := passwordSecretData(creds.Password)
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
	err = c.Get(ctx, types.NamespacedName{Name: roleJobName(tenant.Name, appName), Namespace: postgresNamespace}, job)
	switch {
	case errors.IsNotFound(err):
		if err := c.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
			return false, "", "", err
		}
		// The Job as made, with the UID its Secret is owned by; a Job made
		// by an earlier pass is handed its Secret below on the next.
		if desired.UID != "" {
			if err := ensureJobSecret(ctx, c, desired, jobSecret(desired, password)); err != nil {
				return false, "", "", err
			}
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
		if err := c.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil && !errors.IsNotFound(err) {
			return false, "", "", err
		}
		return false, "DatabaseProvisioning", "the database role Job is replaced by the current one", nil
	case jobIsFailed(job):
		prop := metav1.DeletePropagationBackground
		if err := c.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil && !errors.IsNotFound(err) {
			return false, "", "", err
		}
		return false, "DatabaseProvisioning", "the database role Job failed and is retried", nil
	case !jobIsComplete(job):
		if job.DeletionTimestamp == nil {
			if err := ensureJobSecret(ctx, c, job, jobSecret(job, password)); err != nil {
				return false, "", "", err
			}
		}
		return false, "DatabaseProvisioning", "the database role is being created", nil
	}

	if err := c.Create(ctx, buildDatabaseCR(tenant, tenantNamespaceName(tenant), dbName, appName)); err != nil && !errors.IsAlreadyExists(err) {
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
func (r *ComponentReconciler) ensureDatabaseSecret(ctx context.Context, comp *gentianov1alpha1.Component, store string, template map[string]interface{}, data []interface{}) (ready bool, reason, message string, err error) {
	name := comp.Name + componentDatabaseSecretSuffix
	spec := map[string]interface{}{
		"refreshInterval": "1h",
		"secretStoreRef":  secrets.StoreRef(store),
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
	// Only what is written here is compared: the API server fills in the
	// schema's defaults, which would read as drift on every pass.
	if !covers(existing.Object["spec"], desired.Object["spec"]) {
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
//
// And every Component that activates the profile as an addon: a base is held
// while a pinned addon's profile is not the pinned build, and it is the
// addon's profile changing that releases it.
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
		if list.Items[i].Spec.ProfileRef.Name == profile || slices.Contains(list.Items[i].Spec.Addons, profile) {
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

// componentOriginLabel marks a Component made from an entry of the tenant's
// spec.apps ("install"): the ones ensureAppComponents removes again when
// their entry goes.
//
// "default" is what an earlier release labeled the components it placed
// itself, with no line in git. Such a Component is left exactly as it is
// until the director's placement worker writes its entry; ensureComponent
// then relabels it, and from then on it is an install like any other. It
// is never removed for lacking an entry: removing one purged its data.
// The value is read for one release.
const (
	componentOriginLabel   = "gentianos.io/component-origin"
	componentOriginDefault = "default"
	componentOriginInstall = "install"
)

// Placements is what the platform places on this cluster now: which
// placeable profiles a user tenant has, and which the platform tenant has
// (internal/placement). The director's placement worker asks for it through
// the lifecycle API and writes it into each tenant's manifest.
func (r *TenantReconciler) Placements(ctx context.Context) (placement.Answer, error) {
	profiles := &gentianov1alpha1.ComponentProfileList{}
	if err := r.List(ctx, profiles); err != nil {
		return placement.Answer{}, err
	}
	facts := placement.Facts{Profiles: profiles.Items, UsageStatistics: r.UsageStatisticsEnabled}
	storeRead, goneRead, credentialsRead := false, false, false
	for i := range profiles.Items {
		profile := &profiles.Items[i]
		p := profile.PlacementOf()
		if p == nil {
			continue
		}
		if p.While == gentianov1alpha1.PlacementWhileStoreOffered && !storeRead {
			storeRead = true
			offer, err := readStoreOffer(ctx, r.Client, r.UsageStatisticsEnabled, r.clusterNames())
			if err != nil {
				log.FromContext(ctx).Info("whether the cluster offers an App Store could not be read; "+
					"the components placed where it does are left as they are", "error", err.Error())
			} else {
				offered := offer.offered()
				facts.StoreOffered = &offered
			}
		}
		if p.While == gentianov1alpha1.PlacementWhileCredentialSet && !credentialsRead {
			credentialsRead = true
			credentialed, err := r.platformCredentials(ctx)
			if err != nil {
				return placement.Answer{}, err
			}
			facts.Credentialed = credentialed
		}
		// A profile materialized from a catalog that has since left the
		// catalog directory -- retired, or replaced under another name --
		// is still an object (the Application never prunes) and still
		// declares itself placeable. It is placed no more.
		if strings.TrimSpace(profile.Annotations[profilebundle.Annotation]) != "" && !goneRead {
			goneRead = true
			gone, err := profilebundle.GoneFromCatalogue(ctx, r.Client, layout.Namespace(layout.GitOps))
			if err != nil {
				log.FromContext(ctx).V(1).Info("which profiles left the catalog directory could not be read; "+
					"every placeable profile is placed", "error", err.Error())
			}
			facts.Gone = gone
		}
	}
	return placement.Decide(facts), nil
}

// ensureAppComponents makes every app the tenant has installed a Component,
// and every addon activated inside one a Component of its own.
//
// spec.apps is what git declares: the list a tenant's administrator changes
// through the director. A Component is what the cluster runs from it, and it
// is the unit everything else hangs off -- the release, the routes and the
// question asked at them, the tile, the privileges it was granted and the
// surfaces it publishes. An app that was only an entry in a list had none of
// those as its own, which is why it had no tile.
//
// An addon gets one too. It deploys nothing: it is activation state inside
// its base, and the base's Component carries the list. Its own Component is
// what makes it a thing that can be named, entitled and given a tile.
//
// An addon pinned to a build carries the pin twice. On its own Component, as
// profileRef.digest, so that nothing of its own -- its tile's route -- is
// rendered from another build. And on the base's, beside the list, because
// the base's release is where an addon takes effect and the base is what has
// to be held for it.
//
// A Component whose install has left spec.apps is deleted here, and only
// those: the label says which ones are installs. Uninstalling is a commit
// that removes the entry, and this is where the cluster follows it.
func (r *TenantReconciler) ensureAppComponents(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	wanted := map[string]struct{}{}
	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantComponentProfile(ctx, r.Client, app)
		if err != nil {
			return err
		}
		wanted[profileName] = struct{}{}
		if err := r.ensureComponent(ctx, tenant, profileName, componentOriginInstall, app.Digest, app.Addons, activePins(app), app.Config,
			moveRecord{at: app.MovedAt, by: app.MovedBy}); err != nil {
			return err
		}
		for _, addon := range app.Addons {
			if addon == "" {
				continue
			}
			wanted[addon] = struct{}{}
			if err := r.ensureComponent(ctx, tenant, addon, componentOriginInstall, app.PinOf(addon), nil, nil, nil, moveRecord{}); err != nil {
				return err
			}
		}
	}

	installed := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, installed, client.InNamespace(tenantNamespaceName(tenant)),
		client.MatchingLabels{componentOriginLabel: componentOriginInstall}); err != nil {
		return err
	}
	for i := range installed.Items {
		comp := &installed.Items[i]
		if _, keep := wanted[comp.Name]; keep || comp.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, comp); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("remove the %s component, which is no longer installed: %w", comp.Name, err)
		}
	}
	return nil
}

// activePins is the pins of an app's entry that name an addon the entry
// activates. A pin for an addon that is not in the list pins nothing, and is
// not carried on.
func activePins(app gentianov1alpha1.TenantApp) []gentianov1alpha1.AddonPin {
	var out []gentianov1alpha1.AddonPin
	for _, pin := range app.AddonPins {
		if slices.Contains(app.Addons, pin.Name) {
			out = append(out, pin)
		}
	}
	return out
}

// componentClassAssumedAnnotation marks a Component whose class was assumed
// because its profile was not on the cluster when it was made. It is removed
// once the profile is read and agrees, and an instance that still carries it
// when the profile says otherwise is replaced (ensureComponent).
const componentClassAssumedAnnotation = "gentianos.io/class-assumed"

// installClass is the class a tenant's install runs under: adapter for a
// profile certified as one, app otherwise -- including while the profile is
// not on the cluster yet, which is how an install usually begins.
func (r *TenantReconciler) installClass(ctx context.Context, profileName string) (gentianov1alpha1.ComponentClass, bool, error) {
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: profileName}, profile); err != nil {
		if errors.IsNotFound(err) {
			return gentianov1alpha1.ComponentClassApp, false, nil
		}
		return "", false, err
	}
	if classIncludes(profile, gentianov1alpha1.ComponentClassAdapter) {
		return gentianov1alpha1.ComponentClassAdapter, true, nil
	}
	return gentianov1alpha1.ComponentClassApp, true, nil
}

// ensureComponent keeps one Component of the tenant: named after its profile,
// of the class its profile is certified for (installClass), carrying what the
// Tenant says about that install.
// moveRecord is who or what moved an install's pin, and when, as the
// tenant's manifest records it.
type moveRecord struct{ at, by string }

func (r *TenantReconciler) ensureComponent(
	ctx context.Context, tenant *gentianov1alpha1.Tenant, profileName, origin, digest string,
	addons []string, addonPins []gentianov1alpha1.AddonPin, config *gentianov1alpha1.TenantAppConfig,
	moved moveRecord,
) error {
	class, profileKnown, err := r.installClass(ctx, profileName)
	if err != nil {
		return err
	}
	var annotations map[string]string
	if !profileKnown {
		annotations = map[string]string{componentClassAssumedAnnotation: "true"}
	}
	desired := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name:        profileName,
			Namespace:   tenantNamespaceName(tenant),
			Annotations: annotations,
			Labels: map[string]string{
				tenantLabel:          tenant.Name,
				managedByLabel:       managedByValue,
				componentOriginLabel: origin,
			},
		},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: profileName, Digest: digest},
			Class:      class,
			Addons:     addons,
			AddonPins:  addonPins,
			Config:     config,
			Privileges: tenantPrivilegeGrants(tenant, profileName),
			Exposures:  tenantExposures(tenant, profileName),
			MovedAt:    moved.at,
			MovedBy:    moved.by,
		},
	}
	if err := controllerutil.SetControllerReference(tenant, desired, r.Scheme); err != nil {
		return err
	}
	existing := &gentianov1alpha1.Component{}
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create %s component: %w", profileName, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	// Made before its profile was on the cluster, the class was assumed
	// (componentClassAssumedAnnotation), and the profile now says another.
	// The class cannot be changed, and the instance has rolled out nothing:
	// the Component reconciler refuses a class its profile is not certified
	// for before rendering anything. So it is replaced -- removed here, made
	// again with the profile's class by the next pass. An instance whose
	// class was read from its profile is never replaced this way; if its
	// profile later changes class it is held (ClassMismatch) and says why,
	// because replacing it would be an uninstall nobody asked for.
	if existing.Annotations[componentClassAssumedAnnotation] == "true" && profileKnown && existing.DeletionTimestamp == nil {
		if existing.Spec.Class != class {
			if err := r.Delete(ctx, existing); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("replace the %s component, made as %s before its profile said %s: %w",
					profileName, existing.Spec.Class, class, err)
			}
			return nil
		}
	}
	patch := client.MergeFrom(existing.DeepCopy())
	changed := false
	// A Component written before spec.tenancy became spec.class has neither:
	// the API server returns the stored object untouched, and the rename left
	// the field this reconciler reads empty. It is REPAIRED, not replaced.
	// Deleting it deadlocked the cluster once: the object has a finalizer,
	// removing a finalizer is an update of the whole object, and the whole
	// object is invalid while class is empty. Setting the field is one small
	// write and the immutability rule admits it from empty for exactly this.
	if existing.Spec.Class == "" {
		existing.Spec.Class = gentianov1alpha1.ComponentClassApp
		changed = true
	}
	// What the Tenant says about this install reaches the Component here, and
	// that is the whole of how a commit takes effect: the director commits to
	// the Tenant, Argo applies it, and the Component is reconciled again with
	// the grant, the published surface, the addon list or the configuration
	// in hand. Nobody touches the Component, which keeps git the only writer.
	if existing.Spec.ProfileRef.Digest != digest {
		// The build the install is pinned to, as the Tenant records it.
		existing.Spec.ProfileRef.Digest = digest
		changed = true
	}
	if existing.Spec.MovedAt != moved.at || existing.Spec.MovedBy != moved.by {
		// With the pin: the request the Component's rollout answers.
		existing.Spec.MovedAt, existing.Spec.MovedBy = moved.at, moved.by
		changed = true
	}
	if grants := keepStoredGrants(existing.Spec.Privileges, desired.Spec.Privileges); !equality.Semantic.DeepEqual(existing.Spec.Privileges, grants) {
		existing.Spec.Privileges = grants
		changed = true
	}
	if !equality.Semantic.DeepEqual(existing.Spec.Exposures, desired.Spec.Exposures) {
		existing.Spec.Exposures = desired.Spec.Exposures
		changed = true
	}
	if !equality.Semantic.DeepEqual(existing.Spec.Addons, desired.Spec.Addons) {
		existing.Spec.Addons = desired.Spec.Addons
		changed = true
	}
	if !equality.Semantic.DeepEqual(existing.Spec.AddonPins, desired.Spec.AddonPins) {
		// With the list, never after it: a pin that arrived one write later
		// would leave a moment in which the addon is listed unpinned.
		existing.Spec.AddonPins = desired.Spec.AddonPins
		changed = true
	}
	if !equality.Semantic.DeepEqual(existing.Spec.Config, desired.Spec.Config) {
		existing.Spec.Config = desired.Spec.Config
		changed = true
	}
	// The class is known now and agrees: it is no longer an assumption.
	if profileKnown && existing.Annotations[componentClassAssumedAnnotation] != "" {
		delete(existing.Annotations, componentClassAssumedAnnotation)
		changed = true
	}
	if existing.Labels[componentOriginLabel] != origin {
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[componentOriginLabel] = origin
		changed = true
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("update the %s component: %w", profileName, err)
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

// keepStoredGrants is the Component's grant list for the Tenant's grants
// (desired): an approval the Component already carries for a privilege stays
// exactly as stored, a privilege it does not carry yet is added, and one the
// Tenant no longer grants is removed.
//
// An approval is never rewritten in place. The Component's API refuses a
// change of approver or approvedAt (a new approval is a new grant), and a
// Tenant that records the same privilege approved again -- a second press of
// the button stamps a new approvedAt -- would otherwise fail every update of
// the Component, and with it the tenant's whole app stage.
func keepStoredGrants(stored, desired []gentianov1alpha1.PrivilegeGrant) []gentianov1alpha1.PrivilegeGrant {
	if len(desired) == 0 {
		return nil
	}
	have := make(map[string]gentianov1alpha1.PrivilegeGrant, len(stored))
	for _, g := range stored {
		have[g.Privilege] = g
	}
	out := make([]gentianov1alpha1.PrivilegeGrant, 0, len(desired))
	for _, g := range desired {
		if kept, ok := have[g.Privilege]; ok {
			out = append(out, kept)
			continue
		}
		out = append(out, g)
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
