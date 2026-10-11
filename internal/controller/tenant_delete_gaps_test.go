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
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func deleteGapsScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	_ = gatewayv1beta1.Install(scheme)
	return scheme
}

// A tenant's namespace holds every volume it has. The deletion reports it
// gone only when the API server no longer has it, not when it was told to go.
func TestNamespaceGoneMeansNotFound(t *testing.T) {
	scheme := deleteGapsScheme()
	now := metav1.Now()
	terminating := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant-demo", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(terminating).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	if gone, err := r.namespaceGone(context.Background(), "tenant-demo"); err != nil || gone {
		t.Fatalf("a Terminating namespace: gone = %v, err = %v; want not gone", gone, err)
	}
	if gone, err := r.namespaceGone(context.Background(), "tenant-other"); err != nil || !gone {
		t.Fatalf("a namespace that is not there: gone = %v, err = %v", gone, err)
	}

	// Not being able to look is not the namespace being gone.
	broken := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("the API server did not answer")
		},
	}).Build()
	r = &TenantReconciler{Client: broken, Scheme: scheme}
	if gone, err := r.namespaceGone(context.Background(), "tenant-demo"); err == nil || gone {
		t.Fatalf("an unreadable namespace: gone = %v, err = %v; want an error", gone, err)
	}
}

// Removing the provisioning Jobs after a realm is deleted is what lets a
// tenant of the same name be provisioned again. A Job that could not be
// removed used to be passed over in silence.
func TestDeleteProvisioningJobsReportsWhatItCouldNotDelete(t *testing.T) {
	scheme := deleteGapsScheme()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "keycloak-realm-demo", Namespace: identityNamespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	err := r.deleteProvisioningJobs(context.Background(), "keycloak-realm-demo", "never-existed")
	if err == nil || !strings.Contains(err.Error(), "keycloak-realm-demo") {
		t.Fatalf("err = %v, want the Job that could not be deleted named", err)
	}
	if strings.Contains(err.Error(), "never-existed") {
		t.Fatalf("a Job that is not there was reported: %v", err)
	}

	clean := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job.DeepCopy()).Build()
	r = &TenantReconciler{Client: clean, Scheme: scheme}
	if err := r.deleteProvisioningJobs(context.Background(), "keycloak-realm-demo", "never-existed"); err != nil {
		t.Fatalf("err = %v", err)
	}
}

type failingEdge struct{ tried []string }

func (f *failingEdge) EnsureRoute(context.Context, string, string) error { return nil }
func (f *failingEdge) DeleteRoute(_ context.Context, host string) error {
	f.tried = append(f.tried, host)
	return errors.New("the edge did not answer")
}
func (f *failingEdge) DNSAnnotations() map[string]string { return nil }

// A hostname still routed to this cluster's edge is not a deleted tenant.
// Both routes are tried, and a failure is the deletion's error.
func TestDeleteEdgeRoutingReportsARouteItCouldNotRemove(t *testing.T) {
	scheme := deleteGapsScheme()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	edge := &failingEdge{}
	r := &TenantReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme,
		KernelDomain: "k.example", Ingress: edge,
	}
	err := r.deleteEdgeRouting(context.Background(), tenant)
	if err == nil {
		t.Fatal("an edge route that could not be removed was not reported")
	}
	if len(edge.tried) != 2 {
		t.Fatalf("tried %v, want both the wildcard and the apex (err: %v)", edge.tried, err)
	}
	for _, host := range edge.tried {
		if !strings.Contains(err.Error(), host) {
			t.Errorf("err = %v, want %s named", err, host)
		}
	}
}

// The workloads go first, and the deletion does not move on while one is
// still being removed.
func TestDeleteAppDeploymentRemovesComponentsAndWaits(t *testing.T) {
	scheme := deleteGapsScheme()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{
		Name: "wiki", Namespace: "tenant-demo", Finalizers: []string{componentFinalizer},
	}}
	elsewhere := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-other"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, elsewhere).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()

	if err := r.deleteAppDeployment(ctx, tenant); !errors.Is(err, errDeleteJobPending) {
		t.Fatalf("err = %v, want pending while a component is there", err)
	}
	got := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(comp), got); err != nil || got.DeletionTimestamp == nil {
		t.Fatalf("the component was not deleted: %v", err)
	}
	// Still held by its finalizer: its release is being uninstalled.
	if err := r.deleteAppDeployment(ctx, tenant); !errors.Is(err, errDeleteJobPending) {
		t.Fatalf("err = %v, want pending while the component's release is going", err)
	}
	got.Finalizers = nil
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := r.deleteAppDeployment(ctx, tenant); err != nil {
		t.Fatalf("err = %v, want done once no component is left", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(elsewhere), &gentianov1alpha1.Component{}); err != nil {
		t.Fatalf("another tenant's component was touched: %v", err)
	}
}

// What was made for a tenant in the kernel realm does not go with the
// tenant's own realm, and has to be removed by name: the client its realm
// signed in to the kernel realm as. A tenant that adopts the kernel realm has
// no such client, and a cluster without a kernel realm has neither.
func TestTheRealmDeleteScriptRemovesTheTenantsClientInTheKernelRealm(t *testing.T) {
	script := buildRealmDeleteScript("acme", "kernel")
	for _, want := range []string{
		`BROKER="broker-acme"`,
		`/admin/realms/kernel/clients?clientId=${BROKER}`,
		`select(.clientId == $id)`,
		`-X DELETE`, `/admin/realms/kernel/clients/${BROKER_ID}`,
		`answered HTTP ${HTTP}" >&2; exit 1`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q:\n%s", want, script)
		}
	}
	// The kernel realm itself is never deleted by this: only the tenant's.
	if strings.Contains(script, `-X DELETE \`+"\n"+`  -H "Authorization: Bearer ${TOKEN}" \`+"\n"+`  "${KEYCLOAK_URL}/admin/realms/kernel"`) {
		t.Error("the script deletes the kernel realm")
	}
	for name, s := range map[string]string{
		"no kernel realm":         buildRealmDeleteScript("acme", ""),
		"the realm is the kernel": buildRealmDeleteScript("kernel", "kernel"),
	} {
		if strings.Contains(s, "broker-") {
			t.Errorf("%s: the script looks for a broker client", name)
		}
	}
}

// A deleted tenant's mail records leave the zone with it. The endpoint is in
// the mail perimeter's namespace and carries no tenant label, so nothing
// else takes it.
func TestDeleteMailRemovesTheTenantsMailRecords(t *testing.T) {
	scheme := deleteGapsScheme()
	endpoint := func(name string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(dnsEndpointGVK)
		obj.SetName(name)
		obj.SetNamespace(mailDMZNamespace)
		obj.SetLabels(map[string]string{managedByLabel: managedByValue})
		return obj
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(endpoint("mail-demo"), endpoint("mail-other")).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
	for _, policy := range []gentianov1alpha1.DeletionPolicy{gentianov1alpha1.DeletionPolicyRetain, gentianov1alpha1.DeletionPolicyDelete} {
		tenant := &gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "demo"},
			Spec:       gentianov1alpha1.TenantSpec{DeletionPolicy: policy},
		}
		if err := r.deleteMail(context.Background(), tenant); err != nil {
			t.Fatalf("%s: %v", policy, err)
		}
		gone := endpoint("mail-demo")
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(gone), gone); !apierrors.IsNotFound(err) {
			t.Errorf("%s: the tenant's mail records are still published: %v", policy, err)
		}
	}
	kept := endpoint("mail-other")
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(kept), kept); err != nil {
		t.Errorf("another tenant's mail records were removed: %v", err)
	}
}
