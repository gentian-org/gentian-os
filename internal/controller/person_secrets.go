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
	"regexp"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A component that keeps secrets of people (requires.services.personSecrets)
// asks the confidant, which alone reaches the vault for them. It shows the
// key it holds as a component (rights-check-<component>); the confidant
// reads no Secret, so it is told which keys exist the way the registrar is:
// by their hashes, in a ConfigMap it mounts.
const (
	// confidantKeysConfigMap is the list the confidant mounts
	// (charts/gentian-os/templates/confidant.yaml: the two must agree).
	confidantKeysConfigMap = "confidant-keys"
	// confidantKeysKey is the file in it (cmd/confidant, CONFIDANT_KEYS).
	confidantKeysKey = "keys.json"
	// confidantURLKey is the confidant's address in a component's key
	// Secret.
	confidantURLKey = "CONFIDANT_URL"
	// confidantPort is the confidant's listener.
	confidantPort = netpolicy.ConfidantPort
)

// wantsPersonSecrets reports whether a profile keeps secrets of people. The
// schema holds the requirement to platform trust; it is asked again here,
// because the list is what opens the confidant and a profile that reached
// the cluster past the schema must not be in it.
func wantsPersonSecrets(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.PersonSecrets != nil &&
		profile.Spec.TrustTier == gentianov1alpha1.TrustTierPlatform
}

// confidantURL is where the confidant answers inside the cluster: the
// Service the operator's chart puts in the control namespace
// (templates/confidant.yaml).
func confidantURL() string {
	return fmt.Sprintf("http://gentian-os-confidant.%s.svc.cluster.local:%d", layout.Namespace(layout.Control), confidantPort)
}

// confidantEgress is the way to the confidant: the control namespace on its
// one port, and nothing else there.
func confidantEgress() networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(confidantPort)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": layout.Namespace(layout.Control)},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	}
}

// confidantKeys are the entries the confidant's list must hold: one per
// component that holds a key and whose profile keeps secrets of people now.
// An entry names the component, which is the part of the vault path its
// entries are under, the profile it runs, which is the audience of the
// token the front door hands its page for a person, and the realm its
// tenant's people sign in to.
func confidantKeys(ctx context.Context, c client.Reader) ([]componentkeys.PersonSecrets, error) {
	keys, err := componentKeys(ctx, c, rightsCheckKeys, wantsPersonSecrets)
	if err != nil {
		return nil, err
	}
	out := make([]componentkeys.PersonSecrets, 0, len(keys))
	for _, k := range keys {
		out = append(out, componentkeys.PersonSecrets{
			Key:     componentkeys.Key{Tenant: k.Tenant, Component: k.Component, KeyHash: k.KeyHash},
			Profile: k.Profile,
			Realm:   keycloak.RealmName(k.holder),
		})
	}
	return out, nil
}

// ensureConfidantKeys keeps the confidant's list equal to the components
// that keep secrets of people.
func (r *GatewayPlatformReconciler) ensureConfidantKeys(ctx context.Context) error {
	keys, err := confidantKeys(ctx, r.Client)
	if err != nil {
		return err
	}
	return ensureKeyList(ctx, r.Client, confidantKeysConfigMap, confidantKeysKey, keys)
}

// personSecretsVault is the vault as far as ending people's secrets asks it.
type personSecretsVault interface {
	DeleteTree(ctx context.Context, logicalPath string) error
}

// personID is a person's id in a realm, as it may stand in a vault path.
var personID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// PersonSecretsDepartures removes what components keep for a person when
// the realm says the person is gone (membership.Departures): everything
// under that person's part of the vault, in every tenant whose people are in
// that realm. The secrets of a person who no longer exists open something
// for nobody the platform can ask.
type PersonSecretsDepartures struct {
	Client client.Reader
	Vault  personSecretsVault
}

// PersonRemoved implements membership.Departures.
func (d *PersonSecretsDepartures) PersonRemoved(ctx context.Context, realm, sub string) error {
	if !personID.MatchString(sub) {
		// Not an id the confidant would have kept anything under.
		return nil
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := d.Client.List(ctx, tenants); err != nil {
		return err
	}
	for i := range tenants.Items {
		tenant := &tenants.Items[i]
		if keycloak.RealmName(tenant) != realm {
			continue
		}
		if err := d.Vault.DeleteTree(ctx, secrets.PersonPath(tenant.Name, sub)); err != nil {
			return fmt.Errorf("remove what is kept for a removed person of tenant %s: %w", tenant.Name, err)
		}
	}
	return nil
}
