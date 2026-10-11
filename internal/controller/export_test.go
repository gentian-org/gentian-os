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
	"testing"

	batchv1 "k8s.io/api/batch/v1"
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

// WriteRegistrarRealmSecretForTest exposes the hand-over write. Its merge rule
// decides whether a realm that could not be reached on one pass keeps a
// working credential, and getting that wrong takes a screen down for as long
// as the outage lasts.
func WriteRegistrarRealmSecretForTest(ctx context.Context, c client.Client, data map[string][]byte, complete bool) error {
	return writeRegistrarRealmSecret(ctx, c, data, complete)
}

// RetireDirectorRealmSecretForTest exposes the removal of the Secret the
// director used to mount.
func RetireDirectorRealmSecretForTest(ctx context.Context, c client.Client) error {
	return retireDirectorRealmSecret(ctx, c)
}

// ServicesNamespaceForTest is where the hand-over Secret lands.
func ServicesNamespaceForTest() string { return servicesNamespace }

// RegistrarRealmSecretNamespaceForTest is where the registrar's realm credentials are written.
func RegistrarRealmSecretNamespaceForTest() string { return registrarRealmSecretNamespace() }

// SyncMailAppPasswordsForTest exposes the writer of a tenant's mail
// passwords: the hashes, the tenant's own copy, the seed and the realm's
// submission credential. Where each of them lands is what is asserted.
func (r *TenantReconciler) SyncMailAppPasswordsForTest(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	return r.syncMailAppPasswords(ctx, tenant)
}

// SyncKernelRealmSubmissionIdentityForTest exposes the registration of the
// kernel realm's submission credential, which is read from one namespace and
// written to another.
func (r *TenantReconciler) SyncKernelRealmSubmissionIdentityForTest(ctx context.Context) error {
	return r.syncKernelRealmSubmissionIdentity(ctx)
}

// RetireMisplacedMailObjectsForTest exposes the removal of the mail objects an
// earlier version wrote where nothing read them.
func (r *TenantReconciler) RetireMisplacedMailObjectsForTest(ctx context.Context) error {
	return r.retireMisplacedMailObjects(ctx)
}

// TenantSMTPJobForTest is the Job that configures a tenant realm's mail, for
// the namespace it runs in and the Secrets it reads there.
func TenantSMTPJobForTest(tenant string) *batchv1.Job {
	return makeTenantSMTPJob(tenant, tenant, "mail.example.test", "Example")
}

// KernelMailAddressesForTest are the addresses the operator publishes for
// mail.<kernelDomain> and imap.<kernelDomain>, read from the Services in
// front of the mail edge.
func (r *TenantReconciler) KernelMailAddressesForTest(ctx context.Context) (smtp, imap string) {
	return r.kernelMailAddress(ctx), r.kernelIMAPAddress(ctx)
}

// HoldUntilPostfixAcceptsDomainForTest exposes the check between a tenant's
// provisioning and its mail reporting ready.
func (r *TenantReconciler) HoldUntilPostfixAcceptsDomainForTest(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	return r.holdUntilPostfixAcceptsDomain(ctx, tenant)
}

// SyncPostfixDKIMTablesForTest exposes the writer of the signing tables
// Postfix mounts.
func (r *TenantReconciler) SyncPostfixDKIMTablesForTest(ctx context.Context) error {
	return r.syncPostfixDKIMTables(ctx)
}
