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
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
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
// vault, delivered here as an ExternalSecret. Every other tenant's databases
// live on the tenant postgres of the system tier, which a cluster composes
// with its data-plane functions; until it has, the requirement waits and
// says so.
func (r *ComponentReconciler) ensureDatabaseRequirement(ctx context.Context, comp *gentianov1alpha1.Component, tenant *gentianov1alpha1.Tenant) (ready bool, reason, message string, err error) {
	if !tenantAdoptsKernelRealm(tenant, r.KernelRealm) {
		return false, "DatabaseUnavailable",
			fmt.Sprintf("this cluster composes no tenant postgres yet (%s); the requirement waits", postgresNamespace), nil
	}
	name := comp.Name + componentDatabaseSecretSuffix
	host := fmt.Sprintf("kernel-postgres-rw.%s.svc.cluster.local", r.componentDatabaseNamespace(tenant))
	spec := map[string]interface{}{
		"refreshInterval": "1h",
		"secretStoreRef":  map[string]interface{}{"name": "openbao", "kind": "ClusterSecretStore"},
		"target": map[string]interface{}{
			"name":           name,
			"creationPolicy": "Owner",
			"template": map[string]interface{}{
				"engineVersion": "v2",
				"data": map[string]interface{}{
					"host":         host,
					"port":         "5432",
					"database":     "portal_shell",
					"username":     "portal_shell_user",
					"password":     "{{ .password }}",
					"DATABASE_URL": "postgresql+psycopg://portal_shell_user:{{ .password }}@" + host + ":5432/portal_shell",
				},
			},
		},
		"data": []interface{}{
			map[string]interface{}{
				"secretKey": "password",
				"remoteRef": map[string]interface{}{"key": "gentian-os/kernel/database/postgresql", "property": "portal_shell_user_password"},
			},
		},
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
	}
	return nil
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
