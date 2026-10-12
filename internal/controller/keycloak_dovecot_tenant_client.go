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
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	dovecotOIDCClientID = "gentian-dovecot"

	// dovecotAdminSecretName carries doveadm_password and oidc_client_secret,
	// synced from gentian-os/kernel/mail/dovecot in OpenBao into the mail
	// namespace, beside Dovecot. The Compositions give its oidc_client_secret
	// to the gentian-dovecot Client by reference; the operator reads the same
	// value there to write Dovecot's introspection settings.
	//
	// Note the duplication: the Dovecot chart's own ExternalSecret
	// (dovecot-sensitive-values) pulls the same two properties from the same path
	// into the same namespace under a different name. Both work; they should
	// become one.
	dovecotAdminSecretName = "dovecot-admin"

	// kernelDovecotClientPurposeLabel marks the gentian-dovecot Client that
	// cluster-default composes in the kernel realm. A label and not a name:
	// the object is named after the Cluster XR, whose generated suffix the
	// operator has no way to know.
	kernelDovecotClientPurposeLabel = "gentianos.io/purpose"
	kernelDovecotClientPurpose      = "dovecot-oidc"
)

// Neither realm's gentian-dovecot is provisioned by a Job any more. A tenant
// realm's client is tenant-default's and the kernel realm's is cluster-default's;
// the operator waits for whichever one a tenant's mailboxes are introspected
// against.
//
// kernelDovecotOIDCClientJobName outlives the Job it named, for the sweep in
// keycloak_retired_jobs.go. That Job read the client secret from a Secret in the
// identity namespace, and the Secret is Dovecot's, in the mail namespace: once
// the two were different namespaces its Pod could not be created, so it never
// finished and its TTL never ran.
func kernelDovecotOIDCClientJobName() string {
	return "keycloak-dovecot-oidc-kernel"
}

// keycloakClientGVK is the provider-keycloak managed resource the Compositions
// compose for gentian-dovecot.
var keycloakClientGVK = schema.GroupVersionKind{
	Group:   "openidclient.keycloak.crossplane.io",
	Version: "v1alpha1",
	Kind:    "Client",
}

func tenantDovecotOIDCClientName(tenantName string) string {
	return fmt.Sprintf("%s-dovecot-oidc-client", tenantName)
}

// dovecotTenantClientReady reports whether gentian-dovecot is Ready in the realm
// the tenant's identity lives in.
//
// For a tenant with a realm of its own that is the Client tenant-default
// composes — declared with the secret from dovecot-admin, because an omitted
// clientSecretSecretRef would have Keycloak generate a new one and IMAP would
// stop validating tokens.
//
// A tenant that adopts the kernel realm has no such Client: tenant-default
// composes none of that realm's state, since the realm is not the tenant's. Its
// mailboxes are introspected against the kernel realm's client, so that is the
// one to wait for. Waiting for a Client named after the tenant held
// Tenant/platform at "Waiting for Dovecot tenant OIDC client" for good.
//
// The wait itself has to stay. Steps 6 and 7 add the realm to Dovecot's XOAUTH2
// configuration and reload it, and introspection against a client that does not
// exist yet fails; deleting the wait along with the Job broke three readiness
// tests for exactly that reason.
func (r *TenantReconciler) dovecotTenantClientReady(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	if r.adoptsKernelRealm(tenant) {
		return r.kernelDovecotClientReady(ctx)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(keycloakClientGVK)
	err := r.Get(ctx, types.NamespacedName{Name: tenantDovecotOIDCClientName(tenant.Name)}, obj)
	switch {
	case errors.IsNotFound(err):
		return false, nil
	case apimeta.IsNoMatchError(err):
		// provider-keycloak is not installed yet. A cluster still bootstrapping
		// should wait for it rather than fail the whole mail reconcile.
		log.FromContext(ctx).Info("waiting for the provider-keycloak Client CRD before configuring Dovecot",
			"client", dovecotOIDCClientID)
		return false, nil
	case err != nil:
		return false, err
	}
	return appClaimIsReady(obj), nil
}

// kernelDovecotClientReady reports whether gentian-dovecot is Ready in the
// KERNEL realm, where the cluster admin and every tenant that adopts the realm
// have their mailboxes.
//
// cluster-default composes it, on a cluster whose mail.serviceMode is system,
// with the secret read from dovecot-admin where Dovecot keeps it. It replaces a
// Job that created the same client and could not be given that secret outside
// Dovecot's namespace.
func (r *TenantReconciler) kernelDovecotClientReady(ctx context.Context) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(keycloakClientGVK)
	err := r.List(ctx, list, client.MatchingLabels{kernelDovecotClientPurposeLabel: kernelDovecotClientPurpose})
	switch {
	case apimeta.IsNoMatchError(err):
		log.FromContext(ctx).Info("waiting for the provider-keycloak Client CRD before configuring Dovecot",
			"client", dovecotOIDCClientID)
		return false, nil
	case err != nil:
		return false, err
	}
	for i := range list.Items {
		if appClaimIsReady(&list.Items[i]) {
			return true, nil
		}
	}
	return false, nil
}
