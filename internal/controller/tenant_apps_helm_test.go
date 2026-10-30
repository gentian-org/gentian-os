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
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/layout"
)

func tenantAppsHelmClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func tenantAppsNS(name, tier string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{layout.LabelTier: tier}}}
}

func reconcileTenantAppsHelm(t *testing.T, c client.Client, ns string) ctrl.Result {
	t.Helper()
	res, err := (&TenantAppsHelmReconciler{Client: c}).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ns}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func tenantAppsBinding(t *testing.T, c client.Client, ns string) *rbacv1.RoleBinding {
	t.Helper()
	b := &rbacv1.RoleBinding{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: tenantAppsHelmRole}, b)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func releaseInto(name, namespace string) *unstructured.Unstructured {
	r := &unstructured.Unstructured{}
	r.SetGroupVersionKind(helmReleaseGVK)
	r.SetName(name)
	_ = unstructured.SetNestedField(r.Object, namespace, "spec", "forProvider", "namespace")
	return r
}

// In a tenant namespace, and only there, the account tenant apps are installed
// as is bound to its role, and the binding is kept by a finalizer.
func TestTheTenantAppsAccountIsBoundInTenantNamespacesOnly(t *testing.T) {
	c := tenantAppsHelmClient(t,
		tenantAppsNS("tenant-acme", "tenant"),
		tenantAppsNS("kernel-secrets", "kernel"),
		tenantAppsNS("tenant-acme-dmz", "tenant-dmz"),
		// In the tenant tier and not named as a tenant's namespace: a Tenant
		// may name its own namespace, and it gets no binding.
		tenantAppsNS("kernel-control", "tenant"),
	)
	for _, ns := range []string{"tenant-acme", "kernel-secrets", "tenant-acme-dmz", "kernel-control", "tenant-gone"} {
		reconcileTenantAppsHelm(t, c, ns)
	}

	b := tenantAppsBinding(t, c, "tenant-acme")
	if b == nil {
		t.Fatal("no binding in the tenant namespace")
	}
	if b.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "gentian-tenant-apps-helm"}) {
		t.Errorf("bound to %+v", b.RoleRef)
	}
	wantSubject := rbacv1.Subject{Kind: "ServiceAccount", Name: "provider-helm-tenant-apps", Namespace: "kernel-provisioning"}
	if len(b.Subjects) != 1 || b.Subjects[0] != wantSubject {
		t.Errorf("binds %+v", b.Subjects)
	}
	if !strings.Contains(strings.Join(b.Finalizers, ","), tenantAppsHelmFinalizer) {
		t.Errorf("no finalizer: %v", b.Finalizers)
	}
	for _, ns := range []string{"kernel-secrets", "tenant-acme-dmz", "kernel-control"} {
		if tenantAppsBinding(t, c, ns) != nil {
			t.Errorf("a binding in %s", ns)
		}
	}
}

// A binding someone widened to another subject is put back.
func TestTheTenantAppsBindingNamesTheOneAccount(t *testing.T) {
	widened := desiredTenantAppsBinding("tenant-acme")
	widened.Subjects = append(widened.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "default", Namespace: "tenant-acme"})
	c := tenantAppsHelmClient(t, tenantAppsNS("tenant-acme", "tenant"), widened)
	reconcileTenantAppsHelm(t, c, "tenant-acme")
	if b := tenantAppsBinding(t, c, "tenant-acme"); len(b.Subjects) != 1 || b.Subjects[0].Name != tenantAppsHelmServiceAccount {
		t.Errorf("binds %+v", b.Subjects)
	}
}

// A namespace being deleted keeps the binding while a Release still installs
// into it, so that release can be uninstalled; then the binding lets go.
func TestTheTenantAppsBindingOutlivesTheReleasesOfItsNamespace(t *testing.T) {
	ns := tenantAppsNS("tenant-acme", "tenant")
	now := metav1.Now()
	ns.DeletionTimestamp = &now
	ns.Finalizers = []string{"kubernetes"}
	binding := desiredTenantAppsBinding("tenant-acme")
	binding.DeletionTimestamp = &now
	c := tenantAppsHelmClient(t, ns, binding, releaseInto("acme-odoo-release", "tenant-acme"), releaseInto("other", "tenant-other"))

	if res := reconcileTenantAppsHelm(t, c, "tenant-acme"); res.RequeueAfter == 0 {
		t.Error("a namespace with a Release left is not looked at again")
	}
	if b := tenantAppsBinding(t, c, "tenant-acme"); b == nil || len(b.Finalizers) == 0 {
		t.Fatal("the binding let go while a Release still installs into the namespace")
	}

	if err := c.Delete(context.Background(), releaseInto("acme-odoo-release", "tenant-acme")); err != nil {
		t.Fatal(err)
	}
	reconcileTenantAppsHelm(t, c, "tenant-acme")
	if b := tenantAppsBinding(t, c, "tenant-acme"); b != nil {
		t.Errorf("the binding is held with no Release left: %v", b.Finalizers)
	}
}

// The names the operator binds are the ones the provider manifests define, and
// the ProviderConfig a tenant's Releases name exists and acts as that account.
func TestTheTenantAppsNamesMatchTheProviderManifests(t *testing.T) {
	dir := filepath.Join("..", "..", "crossplane", "providers")
	read := func(file string) []map[string]any {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		var docs []map[string]any
		for _, doc := range strings.Split(string(raw), "\n---\n") {
			var m map[string]any
			if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
				t.Fatal(err)
			}
			if m != nil {
				docs = append(docs, m)
			}
		}
		return docs
	}
	find := func(docs []map[string]any, kind, name string) map[string]any {
		for _, d := range docs {
			meta, _ := d["metadata"].(map[string]any)
			if d["kind"] == kind && meta["name"] == name {
				return d
			}
		}
		t.Fatalf("no %s %s", kind, name)
		return nil
	}

	rbac := read("provider-rbac.yaml")
	sa := find(rbac, "ServiceAccount", tenantAppsHelmServiceAccount)
	if ns := sa["metadata"].(map[string]any)["namespace"]; ns != layout.Kernel(layout.Provisioning) {
		t.Errorf("the account is in %v", ns)
	}
	role := find(rbac, "ClusterRole", tenantAppsHelmRole)
	for _, rule := range role["rules"].([]any) {
		r := rule.(map[string]any)
		for _, res := range r["resources"].([]any) {
			switch res {
			case "networkpolicies", "resourcequotas", "limitranges", "namespaces", "clusterroles", "clusterrolebindings", "httproutes", "externalsecrets":
				t.Errorf("the tenant apps role reaches %v", res)
			}
		}
		for _, verb := range r["verbs"].([]any) {
			if verb == "*" || verb == "escalate" || verb == "bind" || verb == "impersonate" {
				t.Errorf("the tenant apps role holds %v", verb)
			}
		}
	}
	for _, d := range rbac {
		if d["kind"] == "ClusterRoleBinding" {
			ref := d["roleRef"].(map[string]any)
			if ref["name"] == tenantAppsHelmRole {
				t.Error("the tenant apps role is bound cluster-wide")
			}
		}
	}

	pc := find(read("provider-configs.yaml"), "ProviderConfig", TenantAppsProviderConfig)
	creds := pc["spec"].(map[string]any)["credentials"].(map[string]any)
	ref := creds["secretRef"].(map[string]any)
	if creds["source"] != "Secret" || ref["name"] != tenantAppsHelmServiceAccount || ref["namespace"] != layout.Kernel(layout.Provisioning) {
		t.Errorf("the ProviderConfig %s reads %v", TenantAppsProviderConfig, creds)
	}
	// B-05 writes that Secret, acting as the account.
	step, err := os.ReadFile(filepath.Join("..", "..", "scripts", "steps", "B-05-crossplane-providers.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(step), `_B05_TENANT_APPS="`+tenantAppsHelmServiceAccount+`"`) ||
		!strings.Contains(string(step), "as: system:serviceaccount:${ns}:${_B05_TENANT_APPS}") {
		t.Error("B-05 does not write the kubeconfig acting as the account")
	}
}

// Every Release a chart is rendered with names the tenant ProviderConfig
// and does not create its namespace: every render fixture, the main chart's
// release and every extension's.
func TestEveryRenderedReleaseInstallsAsTheTenantAppsAccount(t *testing.T) {
	releases := 0
	for _, fixture := range apprenderFixtureNames(t) {
		for i, release := range renderedOfKind(renderFixture(t, fixture), "Release") {
			releases++
			if name, _, _ := unstructured.NestedString(release.Object, "spec", "providerConfigRef", "name"); name != TenantAppsProviderConfig {
				t.Errorf("%s: Release %d names the ProviderConfig %q", fixture, i, name)
			}
			if skip, _, _ := unstructured.NestedBool(release.Object, "spec", "forProvider", "skipCreateNamespace"); !skip {
				t.Errorf("%s: Release %d creates its namespace", fixture, i)
			}
		}
	}
	if releases == 0 {
		t.Fatal("no fixture renders a Release")
	}
}
