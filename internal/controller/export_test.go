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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// SchemeForTest builds a scheme of the caller's own.
//
// The fake client used to be handed the package-global scheme.Scheme, in tests
// that run in parallel. Building a fake client reads the scheme's type map to
// construct a RESTMapper, and this package's envtest suite writes that map
// while those tests run -- so `go test ./...` failed intermittently with
// "concurrent map iteration and map write", from inside apimachinery, in a
// different test each time and never reproducibly.
//
// A scheme per test removes the sharing rather than narrowing the window. The
// registrations are the ones TestMain makes on the global scheme, so a fake
// client built from this knows exactly what one built from that did.
func SchemeForTest(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		gentianov1alpha1.AddToScheme,
		networkingv1.AddToScheme,
		gatewayv1.Install,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build test scheme: %v", err)
		}
	}
	return s
}

// DovecotDeployedForTest exposes the Dovecot gate to the external test package.
// The predicate decides whether an entire provisioning path runs, and it fails
// silently when wrong, so it is worth asserting directly.
func (r *TenantReconciler) DovecotDeployedForTest(ctx context.Context) bool {
	return r.dovecotDeployed(ctx)
}

// DefaultTenantMailModeForTest exposes the mail-mode default to the external
// test package. Like the Dovecot gate it decides a whole provisioning path from
// cluster state a tenant never states, and getting it wrong is silent.
func (r *TenantReconciler) DefaultTenantMailModeForTest(ctx context.Context) gentianov1alpha1.MailMode {
	return r.defaultTenantMailMode(ctx)
}

// SyncTenantMailDNSForTest exposes the tenant mail-DNS writer. It publishes into
// a public zone and, on a tunnel cluster, its records collide with the tenant's
// own web CNAME -- so whether it runs at all is worth asserting directly rather
// than inferring from a reconcile.
func (r *TenantReconciler) SyncTenantMailDNSForTest(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	return r.syncTenantMailDNS(ctx, tenant)
}

// EnsureMailForTest exposes the mail dispatch. What it decides is whether a
// tenant is registered in a mail stack this cluster runs, and the failure mode
// this test guards is silent: mail that is never delivered.
func (r *TenantReconciler) EnsureMailForTest(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	_, err := r.ensureMail(ctx, tenant)
	return err
}

// WriteDirectorRealmSecretForTest exposes the hand-over write. Its merge rule
// decides whether a realm that could not be reached on one pass keeps a
// working credential, and getting that wrong takes a screen down for as long
// as the outage lasts.
func WriteDirectorRealmSecretForTest(ctx context.Context, c client.Client, data map[string][]byte, complete bool) error {
	return writeDirectorRealmSecret(ctx, c, data, complete)
}

// ServicesNamespaceForTest is where the hand-over Secret lands.
func ServicesNamespaceForTest() string { return servicesNamespace }
