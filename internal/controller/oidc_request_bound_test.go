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
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What an app's OIDC client declares decides that app's client alone. A
// request-bound client declares no redirectUris, and is not refused for it;
// a client that declares nothing it could be redirected to is refused, and
// the tenant's other apps are provisioned all the same.

func requestBoundOIDC(clientID string) *gentianov1alpha1.OIDCClientSpec {
	return &gentianov1alpha1.OIDCClientSpec{
		ClientID:   clientID,
		AccessType: "PUBLIC",
		RequestParameter: &gentianov1alpha1.OIDCRequestParameterSpec{
			Name:        "tenant_url",
			Pattern:     `https://(?<host>[a-z0-9.-]+)`,
			RedirectURI: "https://store.${host}/oauth/callback",
			Claim:       "tenant_url",
		},
	}
}

func oidcProfile(name string, oidc *gentianov1alpha1.OIDCClientSpec, api bool) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{OIDC: oidc},
			}},
		},
	}
	if api {
		p.Spec.Package.API = &gentianov1alpha1.APIIntegration{}
	}
	return p
}

func oidcTestReconciler(t *testing.T, objs ...client.Object) *TenantReconciler {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{gentianov1alpha1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return &TenantReconciler{
		Client:       fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		Scheme:       s,
		KernelDomain: "example.test",
	}
}

func tenantWithApps(name string, apps ...string) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, a := range apps {
		t.Spec.Apps = append(t.Spec.Apps, gentianov1alpha1.TenantApp{Profile: a})
	}
	return t
}

func provisioningJobNames(jobs []batchv1.Job) map[string]bool {
	names := map[string]bool{}
	for _, j := range jobs {
		names[j.Name] = true
	}
	return names
}

// The app Composition registers https://* for a request-bound client; the
// profile names no address, and the tenant's provisioning goes on.
func TestARequestBoundClientNeedsNoRedirectURIs(t *testing.T) {
	r := oidcTestReconciler(t, oidcProfile("store", requestBoundOIDC("gentian-app-store"), false))
	tenant := tenantWithApps("customers", "store")

	configs, refused, err := r.collectOIDCAppConfigs(context.Background(), tenant)
	if err != nil || len(refused) > 0 {
		t.Fatalf("collectOIDCAppConfigs = %v, refused %v; want the client resolved", err, refused)
	}
	if len(configs) != 1 || configs[0].clientID != "gentian-app-store" || len(configs[0].redirectURIs) != 0 {
		t.Fatalf("configs = %+v, want gentian-app-store with no redirect URIs", configs)
	}
	jobs, _, err := r.buildTenantProvisioningJobs(context.Background(), tenant)
	if err != nil {
		t.Fatalf("buildTenantProvisioningJobs: %v", err)
	}
	if provisioningJobNames(jobs)[clientJobName("customers", "store")] {
		t.Fatal("a client Job was built for a client the app Composition composes")
	}
}

// One app's client that cannot be made as declared is that app's: it gets no
// Job and is named, and the next app's client is provisioned as before.
func TestOneAppsRefusedClientDoesNotStopTheOthers(t *testing.T) {
	bad := oidcProfile("broken", &gentianov1alpha1.OIDCClientSpec{ClientID: "broken"}, true)
	good := oidcProfile("wiki", &gentianov1alpha1.OIDCClientSpec{
		ClientID: "demo-wiki", RedirectURIs: []string{"https://wiki.${TENANT_DOMAIN}/*"},
	}, true)
	r := oidcTestReconciler(t, bad, good)
	tenant := tenantWithApps("demo", "broken", "wiki")

	configs, refused, err := r.collectOIDCAppConfigs(context.Background(), tenant)
	if err != nil {
		t.Fatalf("collectOIDCAppConfigs: %v", err)
	}
	if len(configs) != 1 || configs[0].profileName != "wiki" {
		t.Fatalf("configs = %+v, want wiki alone", configs)
	}
	if len(refused) != 1 || !strings.Contains(refused[0], "broken") {
		t.Fatalf("refused = %v, want broken named", refused)
	}

	jobs, _, err := r.buildTenantProvisioningJobs(context.Background(), tenant)
	if err != nil {
		t.Fatalf("buildTenantProvisioningJobs: %v; one app's client must not stop the tenant's", err)
	}
	names := provisioningJobNames(jobs)
	if !names[clientJobName("demo", "wiki")] {
		t.Errorf("no client Job for wiki among %v", names)
	}
	if names[clientJobName("demo", "broken")] {
		t.Errorf("a client Job was built for the refused client: %v", names)
	}
}

// A tenant whose identity is otherwise provisioned reports the refused client
// on IdentityReady, without asking to be run again: the other stages run.
// The tenant adopts the kernel realm here because that path waits on the
// fewest Jobs; it also must not wait for a Job for a client the app
// Composition composes, which is never built.
func TestARefusedClientIsReportedOnIdentityReady(t *testing.T) {
	groups := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: gentianGroupsJobName("platform"), Namespace: identityNamespace},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}},
	}
	composed := oidcProfile("notes", &gentianov1alpha1.OIDCClientSpec{
		ClientID: "notes", RedirectURIs: []string{"https://notes.${TENANT_DOMAIN}/*"},
	}, false)
	bad := oidcProfile("broken", &gentianov1alpha1.OIDCClientSpec{ClientID: "broken"}, false)
	r := oidcTestReconciler(t, groups, composed, bad)
	r.KernelRealm = "kernel"
	tenant := tenantWithApps("platform", "notes", "broken")
	tenant.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}

	res, err := r.ensureIdentity(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ensureIdentity: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("ensureIdentity asked to be run again in %s: it waits for something that is never built", res.RequeueAfter)
	}
	var cond *metav1.Condition
	for i := range tenant.Status.Conditions {
		if tenant.Status.Conditions[i].Type == conditionIdentityReady {
			cond = &tenant.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonOIDCClientRefused || !strings.Contains(cond.Message, "broken") {
		t.Fatalf("IdentityReady = %+v, want False/%s naming broken", cond, reasonOIDCClientRefused)
	}
}

// Only the app Composition holds the rule a request parameter needs, so no
// provisioning Job ever writes a request-bound client's redirect addresses:
// one whose client a Job would make is refused, not made without the rule.
func TestNoJobWritesARequestBoundClientsRedirects(t *testing.T) {
	api := oidcProfile("store-api", requestBoundOIDC("store-api"), true)
	sidecarParent := oidcProfile("shop", &gentianov1alpha1.OIDCClientSpec{
		ClientID: "shop", RedirectURIs: []string{"https://shop.${TENANT_DOMAIN}/*"},
	}, false)
	sidecarParent.Spec.Package.Composition = "app-default"
	sidecarParent.Spec.Extensions = []gentianov1alpha1.AppSidecarSpec{{
		Name: "till",
		ServiceRequirements: &gentianov1alpha1.ServiceRequirements{
			Identity: &gentianov1alpha1.IdentityRequirement{OIDC: requestBoundOIDC("shop-till")},
		},
	}}
	r := oidcTestReconciler(t, api, sidecarParent)
	tenant := tenantWithApps("demo", "store-api", "shop")

	_, refused, err := r.collectOIDCAppConfigs(context.Background(), tenant)
	if err != nil {
		t.Fatalf("collectOIDCAppConfigs: %v", err)
	}
	if len(refused) != 2 {
		t.Fatalf("refused = %v, want the API entry's and the extension's request-bound client", refused)
	}
	jobs, _, err := r.buildTenantProvisioningJobs(context.Background(), tenant)
	if err != nil {
		t.Fatalf("buildTenantProvisioningJobs: %v", err)
	}
	names := provisioningJobNames(jobs)
	for _, app := range []string{"store-api", gentianov1alpha1.SidecarAppName("shop", "till")} {
		if names[clientJobName("demo", app)] {
			t.Errorf("a client Job was built for the request-bound client of %s", app)
		}
	}

	// A pack's Job makes the client only when it is absent, and with no
	// address the profile did not declare: [] for a request-bound one, and
	// never a PUT of the client that would restate its redirects.
	withPack := oidcProfile("store", requestBoundOIDC("gentian-app-store"), false)
	catalog := &gentianov1alpha1.OIDCPackCatalog{
		ObjectMeta: metav1.ObjectMeta{Name: "store-oidc", Labels: map[string]string{gentianov1alpha1.LabelProfileName: "store"}},
		Spec: gentianov1alpha1.OIDCPackCatalogSpec{Packs: map[string]gentianov1alpha1.OIDCPackSpec{
			"gentian-app-store": {ScopeName: "store", ClientRole: "user", EntitlementGroup: "store-users", PublicClient: true},
		}},
	}
	r = oidcTestReconciler(t, withPack, catalog)
	tenant = tenantWithApps("customers", "store")
	jobs, _, err = r.buildTenantProvisioningJobs(context.Background(), tenant)
	if err != nil {
		t.Fatalf("buildTenantProvisioningJobs (pack): %v", err)
	}
	var script string
	for _, j := range jobs {
		if j.Name == clientJobName("customers", "store") {
			script = strings.Join(j.Spec.Template.Spec.Containers[0].Command, " ") + strings.Join(j.Spec.Template.Spec.Containers[0].Args, " ")
		}
	}
	if script == "" {
		t.Fatal("no pack Job for the store")
	}
	if !strings.Contains(script, "REDIRECT_URIS='[]'") {
		t.Errorf("the pack Job's redirect URIs are not []:\n%s", script)
	}
	lines := strings.Split(script, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.Contains(lines[i-1], "-X PUT") && strings.HasSuffix(strings.TrimSpace(lines[i]), `/clients/${CLIENT_UUID}" \`) {
			t.Errorf("the pack Job restates the client:\n%s\n%s", lines[i-1], lines[i])
		}
	}
}
