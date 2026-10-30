/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package apprender renders what one tenant component's chart is installed
// with: its release, the Secrets and requirements its values read, its
// registration at the tenant's realm, its post-install job and its
// extensions. It reads nothing and writes nothing; the component reconciler
// gives it what it read and applies what comes back.
//
// What it renders is what the platform's app Composition of earlier releases
// rendered for the same inputs, object for object and name for name, so that
// an app that Composition installed is taken over by the operator without a
// change at the realm or a new Helm revision. The objects the Composition wrapped in a
// provider-kubernetes Object are rendered as themselves.
package apprender

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Input is everything one render reads.
type Input struct {
	// App is the component's name, and Namespace the tenant's namespace it
	// runs in. The tenant's name is the namespace's without "tenant-".
	App       string
	Namespace string
	// Realm is the tenant's Keycloak realm (keycloak.RealmName); empty means
	// the realm of the tenant's own name.
	Realm string
	// Domain is the tenant's effective domain, substituted for
	// ${TENANT_DOMAIN} and used in its client's addresses.
	Domain string

	// ReleaseObject is the name of the provider-helm Release object, and
	// HelmRelease the name of the Helm release it installs. They differ for
	// an app first installed by the platform's former app Composition, whose
	// release is <app>-release, and are the same for one the operator
	// installed.
	ReleaseObject string
	HelmRelease   string
	// KeepVolumes names the ConfigMap the release reads its keep rule from.
	KeepVolumes string
	// Composite names what the Composition named after the app's composite
	// resource -- its OIDC clients' secrets in the provisioning namespace,
	// which the clients read by that name, and its extensions' release
	// objects. For an app the Composition installed it is that composite's
	// name, and what is taken over keeps the names it had; for one the
	// operator installed, a name of the operator's, unique across tenants.
	Composite string

	// ProfileName is the profile's name, which is the app's identity across
	// every build of it, and Profile the spec it is rendered from: the
	// profile's own, or the verified build's the tenant runs.
	ProfileName string
	Profile     *gentianov1alpha1.ComponentProfileSpec
	// ProfileAnnotations are the profile's own annotations, which stay the
	// profile's when the spec is a build's: how the values are written
	// (gentianos.io/standard-image-pull-secrets), its entitlement group's
	// attributes (gentianos.io/keycloak-group-attributes) and its
	// extensions' default redirect addresses
	// (gentianos.io/oidc-default-redirect-uris).
	ProfileAnnotations map[string]string

	// Consumer is what a supplied credential's requirement names as
	// consuming it: the XApp for an app the Composition installed, the
	// Component for one the operator installed.
	Consumer gentianov1alpha1.CredentialConsumer

	// Placed says the platform placed the component. Its database is in the
	// platform's own schema and its way to the model gateway is the
	// operator's: both reach the chart through ExtraValues, and nothing of
	// either is read from the tenant's store.
	Placed bool

	// What the component asks of its install beyond its profile.
	PullSecrets        PullSecrets
	MailboxTokenSignIn bool
	// SignInSidecar is the sign-in sidecar the operator runs beside the app,
	// as the reconciler describes it; nil when the profile declares none.
	SignInSidecar map[string]interface{}
	// Addons are the addons selected into this app, by the names the app
	// knows them by.
	Addons      []string
	Replicas    *int64
	ExtraValues map[string]interface{}

	// The cluster's facts: gentian-cluster-config and its namespace, and
	// gentian-kernel-services.
	ClusterConfig          map[string]string
	ClusterConfigNamespace string
	KernelServices         map[string]string

	// Packs is the profile's OIDC pack catalog, nil when it has none.
	Packs *gentianov1alpha1.OIDCPackCatalogSpec
	// RedirectPolicy is the tenant realm's client policy that narrows a
	// request-bound client's addresses, nil while there is none.
	RedirectPolicy *unstructured.Unstructured

	// Observed are the objects rendered for this app before, as they are
	// now, by name. A few renders depend on them: an id the realm assigned,
	// or an object that goes on being rendered once it exists.
	Observed map[string]*unstructured.Unstructured
}

// PullSecrets are the tenant's pull Secrets this app's chart and images are
// pulled with, by name, in the app's namespace.
type PullSecrets struct {
	Images []string
	Charts []ChartPullSecret
}

// ChartPullSecret is the Secret a chart repository is pulled with.
type ChartPullSecret struct {
	Repository string
	SecretName string
}

// Tenant is the tenant's name, read off its namespace.
func (in *Input) Tenant() string {
	const prefix = "tenant-"
	if len(in.Namespace) > len(prefix) && in.Namespace[:len(prefix)] == prefix {
		return in.Namespace[len(prefix):]
	}
	return in.Namespace
}

// RealmName is the realm the app's client is registered in.
func (in *Input) RealmName() string {
	if in.Realm != "" {
		return in.Realm
	}
	return in.Tenant()
}

// TenantDomain is the domain substituted for ${TENANT_DOMAIN}.
func (in *Input) TenantDomain() string {
	if in.Domain != "" {
		return in.Domain
	}
	return in.Tenant() + ".example.org"
}

// extensionReleaseObject names the Release object of one of the app's
// extensions.
func (in *Input) extensionReleaseObject(extension string) string {
	return in.Composite + "-" + extension + "-release"
}

// observed returns the object rendered before under this name, or nil.
func (in *Input) observed(name string) *unstructured.Unstructured {
	return in.Observed[name]
}
