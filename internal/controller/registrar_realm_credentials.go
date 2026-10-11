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
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// The registrar's per-realm Keycloak credentials, provisioned and handed over.
//
// The registrar speaks for Keycloak on a caller's behalf and holds ONE
// credential per realm, so that a missed authorization check cannot reach a
// realm the caller has nothing to do with. Somebody has to create those
// credentials, and it is not the registrar: a service that had to hold an
// administrative credential in order to create the one it is allowed to use
// would have the privilege the split exists to remove. It is the operator's,
// which already holds the Keycloak admin credential and whose whole job is to
// make cluster state match what is declared.
//
// It is handed over rather than fetched. The registrar mounts the Secret; its
// ServiceAccount may read the tenants and the Cluster claim and no Secret at
// all. The operator holds the administrative credential, the registrar is
// given what it may use.
//
// The credential was the director's until the registrar took the people
// routes over (operator-split-plan.md §4.4): the same client under the name
// gentian-director-admin, in a Secret the director mounted. Nothing reads
// either any more, and a credential nothing reads is still a credential, so
// each pass removes what is left of them -- see retireDirectorRealmCredentials.

// registrarRealmSecretNamespace is where the registrar runs and mounts the
// Secret from: the control namespace. It was servicesNamespace, which the v5
// layout made the edge namespace -- the Secret was written there with the
// kernel realm's key in it, its reader mounted an absent Secret from its own
// namespace, and People answered 503 "holds no credential for the realm
// kernel" on a finished install.
func registrarRealmSecretNamespace() string { return layout.Namespace(layout.Control) }

// RegistrarRealmSecretName is the Secret the registrar mounts, one key per realm
// whose value is that realm's client secret. The client id is the same in
// every realm (authz.RegistrarClientID), so it is not repeated per key.
const RegistrarRealmSecretName = "gentian-registrar-realms"

// ensureRegistrarRealmCredentials makes the registrar's credential exist in
// every realm this cluster serves, and writes them where the registrar reads.
//
// Realms, not tenants: several tenants may adopt the kernel realm, and a
// credential is per realm. The set is deduplicated for exactly that reason.
func (r *KeycloakPlatformReconciler) ensureRegistrarRealmCredentials(ctx context.Context) error {
	kernelRealm := r.KernelRealm
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}

	// The kernel realm first and always. No XTenant exists for it, so no
	// Composition covers it, and it is the realm the platform's own
	// administrators sign in through -- the one the administration console
	// needs before any tenant exists.
	realms := map[string]bool{kernelRealm: true}

	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return fmt.Errorf("list tenants for registrar realm credentials: %w", err)
	}
	for i := range tenants.Items {
		tenant := &tenants.Items[i]
		// A tenant on its way out keeps nothing: the realm goes with it, and
		// the client goes with the realm.
		if tenant.DeletionTimestamp != nil {
			continue
		}
		if realm := keycloakRealmName(tenant); realm != "" {
			realms[realm] = true
		}
	}

	kcURL, kcUser, kcPass, err := loadKeycloakAdmin(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("load keycloak-admin for registrar realm credentials: %w", err)
	}
	kc := authz.NewKeycloakAdminClient(kcURL, kcUser, kcPass)

	names := make([]string, 0, len(realms))
	for realm := range realms {
		names = append(names, realm)
	}
	sort.Strings(names)

	// One realm's failure does not withhold the others.
	//
	// A realm that is still being composed answers 404 for a while, and
	// refusing to write the Secret until every realm is ready would keep the
	// registrar from speaking for the kernel realm because a tenant realm is
	// thirty seconds behind. The error is returned so the reconcile requeues
	// and the missing realm is picked up on the next pass.
	secrets := map[string][]byte{}
	var firstErr error
	for _, realm := range names {
		secret, err := kc.EnsureRegistrarRealmClient(ctx, realm)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("realm %s: %w", realm, err)
			}
			continue
		}
		secrets[realm] = []byte(secret)
		// The client the director used to hold, in the same realm. Only once
		// the registrar's exists there, so a realm is never left with neither.
		if err := kc.DeleteRetiredDirectorRealmClient(ctx, realm); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("realm %s: retire %s: %w", realm, authz.RetiredDirectorClientID, err)
		}
	}
	if len(secrets) == 0 {
		// Nothing to hand over and nothing to correct. Writing an empty
		// Secret here would take away whatever the registrar is using.
		if firstErr != nil {
			return firstErr
		}
		return nil
	}
	if err := writeRegistrarRealmSecret(ctx, r.Client, secrets, firstErr == nil); err != nil {
		return err
	}
	if err := retireDirectorRealmSecret(ctx, r.Client); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// retiredDirectorRealmSecretName is the Secret these credentials were handed
// over in while the director held them.
const retiredDirectorRealmSecretName = "gentian-director-realms"

// retireDirectorRealmSecret removes the Secret the director used to mount.
//
// The director mounts nothing of the kind any more, and the clients whose
// secrets it holds are deleted in the same pass, so what it contains is
// already worthless on a cluster that has caught up. It is removed anyway: a
// Secret named for a credential is something a person reading the namespace
// takes to be one. Absent is the ordinary case, on every cluster installed
// since.
func retireDirectorRealmSecret(ctx context.Context, c client.Client) error {
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: retiredDirectorRealmSecretName, Namespace: registrarRealmSecretNamespace()}}
	if err := c.Delete(ctx, old); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("retire %s/%s: %w", old.Namespace, old.Name, err)
	}
	return nil
}

// writeRegistrarRealmSecret puts the credentials where the registrar mounts them.
//
// complete says whether every realm was reached on this pass. When it is true
// the Secret is replaced, so a realm that is gone stops being reachable; when
// it is false the new values are merged into what is there, because a realm
// this pass could not read is not the same as a realm that no longer exists,
// and removing a working credential over a transient 404 would take a screen
// down for as long as the outage lasts.
func writeRegistrarRealmSecret(ctx context.Context, c client.Client, data map[string][]byte, complete bool) error {
	existing := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{
		Name: RegistrarRealmSecretName, Namespace: registrarRealmSecretNamespace()}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return c.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      RegistrarRealmSecretName,
				Namespace: registrarRealmSecretNamespace(),
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "gentian-operator",
					"app.kubernetes.io/part-of":    "gentian-os",
				},
				Annotations: map[string]string{
					"gentianos.io/description": "The registrar's Keycloak client secret per realm. " +
						"One key per realm; the client id is " + authz.RegistrarClientID + " in all of them.",
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		})
	case err != nil:
		return fmt.Errorf("read %s/%s: %w", registrarRealmSecretNamespace(), RegistrarRealmSecretName, err)
	}

	merged := data
	if !complete {
		merged = map[string][]byte{}
		for k, v := range existing.Data {
			merged[k] = v
		}
		for k, v := range data {
			merged[k] = v
		}
	}
	if secretDataEqual(existing.Data, merged) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = merged
	return c.Patch(ctx, existing, patch)
}

// secretDataEqual keeps an unchanged pass from writing. A Secret rewritten on
// every five-minute loop is a Secret whose resourceVersion changes constantly,
// which makes a real change impossible to see in an audit.
func secretDataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || string(av) != string(bv) {
			return false
		}
	}
	return true
}
