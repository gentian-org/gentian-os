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
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
)

// The director's per-realm Keycloak credentials, provisioned and handed over.
//
// The director speaks for Keycloak on a caller's behalf and holds ONE
// credential per realm, so that a missed authorization check cannot reach a
// realm the caller has nothing to do with. Somebody has to create those
// credentials, and it is not the director: a service that had to hold an
// administrative credential in order to create the one it is allowed to use
// would have the privilege the split exists to remove. It is the operator's,
// which already holds the Keycloak admin credential and whose whole job is to
// make cluster state match what is declared.
//
// It is handed over rather than fetched. The director reads a mounted Secret
// and has no Kubernetes identity to go and look with, which is the same
// property as the tile catalogue: the operator holds the credential, the
// director is given what it needs.

// DirectorRealmSecretName is the Secret the director mounts, one key per realm
// whose value is that realm's client secret. The client id is the same in
// every realm (authz.DirectorClientID), so it is not repeated per key.
const DirectorRealmSecretName = "gentian-director-realms"

// ensureDirectorRealmCredentials makes the director's credential exist in
// every realm this cluster serves, and writes them where the director reads.
//
// Realms, not tenants: several tenants may adopt the kernel realm, and a
// credential is per realm. The set is deduplicated for exactly that reason.
func (r *KeycloakPlatformReconciler) ensureDirectorRealmCredentials(ctx context.Context) error {
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
		return fmt.Errorf("list tenants for director realm credentials: %w", err)
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
		return fmt.Errorf("load keycloak-admin for director realm credentials: %w", err)
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
	// director from speaking for the kernel realm because a tenant realm is
	// thirty seconds behind. The error is returned so the reconcile requeues
	// and the missing realm is picked up on the next pass.
	secrets := map[string][]byte{}
	var firstErr error
	for _, realm := range names {
		secret, err := kc.EnsureDirectorRealmClient(ctx, realm)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("realm %s: %w", realm, err)
			}
			continue
		}
		secrets[realm] = []byte(secret)
	}
	if len(secrets) == 0 {
		// Nothing to hand over and nothing to correct. Writing an empty
		// Secret here would take away whatever the director is using.
		if firstErr != nil {
			return firstErr
		}
		return nil
	}
	if err := writeDirectorRealmSecret(ctx, r.Client, secrets, firstErr == nil); err != nil {
		return err
	}
	return firstErr
}

// writeDirectorRealmSecret puts the credentials where the director mounts them.
//
// complete says whether every realm was reached on this pass. When it is true
// the Secret is replaced, so a realm that is gone stops being reachable; when
// it is false the new values are merged into what is there, because a realm
// this pass could not read is not the same as a realm that no longer exists,
// and removing a working credential over a transient 404 would take a screen
// down for as long as the outage lasts.
func writeDirectorRealmSecret(ctx context.Context, c client.Client, data map[string][]byte, complete bool) error {
	existing := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{
		Name: DirectorRealmSecretName, Namespace: servicesNamespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return c.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      DirectorRealmSecretName,
				Namespace: servicesNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "gentian-operator",
					"app.kubernetes.io/part-of":    "gentian-os",
				},
				Annotations: map[string]string{
					"gentianos.io/description": "The director's Keycloak client secret per realm. " +
						"One key per realm; the client id is " + authz.DirectorClientID + " in all of them.",
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		})
	case err != nil:
		return fmt.Errorf("read %s/%s: %w", servicesNamespace, DirectorRealmSecretName, err)
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
