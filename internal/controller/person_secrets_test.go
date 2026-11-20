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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func personSecretsProfile(name string, tier gentianov1alpha1.TrustTier) *gentianov1alpha1.ComponentProfile {
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	profile.Spec.TrustTier = tier
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
		Services: &gentianov1alpha1.ServiceRequirements{PersonSecrets: &gentianov1alpha1.PersonSecretsRequirement{}},
	}
	return profile
}

// The confidant's list holds the components that keep secrets of people at
// the platform's trust, and no other: not one that holds a key for the
// rights check alone, and not one whose profile declares the requirement
// below that trust. An entry carries the hash of the key and never the key,
// the profile and the realm of the tenant's people. The component is given
// the confidant's address beside its key.
func TestConfidantListHoldsOnlyComponentsThatKeepSecretsOfPeople(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	liaison, checker, lesser := rightsComponent("acme", "liaison"), rightsComponent("acme", "checker"), rightsComponent("acme", "lesser")
	renamed := rightsTenant("globex")
	renamed.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "globex-people"}
	elsewhere := rightsComponent("globex", "liaison")
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"liaison": personSecretsProfile("liaison", gentianov1alpha1.TrustTierPlatform),
		"checker": rightsProfile("checker", true),
		"lesser":  personSecretsProfile("lesser", gentianov1alpha1.TrustTierCertified),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		rightsTenant("acme"), renamed, liaison, checker, lesser, elsewhere,
		profiles["liaison"], profiles["checker"], profiles["lesser"],
	).Build()
	components := &ComponentReconciler{Client: c, Scheme: scheme}
	for _, comp := range []*gentianov1alpha1.Component{liaison, checker, lesser, elsewhere} {
		if err := components.ensureRightsCheck(ctx, comp, defaultTenantName(comp), profiles[comp.Name]); err != nil {
			t.Fatal(err)
		}
	}
	held := rightsSecret(t, c, liaison)
	if got := string(held.Data[confidantURLKey]); !strings.HasSuffix(got, ":9446") || !strings.Contains(got, layout.Namespace(layout.Control)) {
		t.Fatalf("the component is told %q as the confidant's address", got)
	}
	if got := rightsSecret(t, c, checker).Data[confidantURLKey]; len(got) != 0 {
		t.Fatalf("a component that keeps no such secrets is told the confidant's address: %s", got)
	}

	gateway := &GatewayPlatformReconciler{Client: c}
	if err := gateway.ensureConfidantKeys(ctx); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: confidantKeysConfigMap, Namespace: layout.Namespace(layout.Control)}, cm); err != nil {
		t.Fatal(err)
	}
	raw := cm.Data[confidantKeysKey]
	keys, err := componentkeys.Parse[componentkeys.PersonSecrets]([]byte(raw), "the confidant's list")
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if len(keys) != 2 {
		t.Fatalf("the list holds %d entries, want the two liaisons: %s", len(keys), raw)
	}
	key := string(held.Data[rightsCheckKeyKey])
	if strings.Contains(raw, key) {
		t.Fatal("the list holds a key in the clear")
	}
	mine := componentkeys.Find(keys, key)
	if mine == nil || mine.Tenant != "acme" || mine.Component != "liaison" || mine.Profile != "liaison" || mine.Realm != "acme" {
		t.Fatalf("the entry of acme's liaison: %+v", mine)
	}
	theirs := componentkeys.Find(keys, string(rightsSecret(t, c, elsewhere).Data[rightsCheckKeyKey]))
	if theirs == nil || theirs.Tenant != "globex" || theirs.Realm != "globex-people" {
		t.Fatalf("the entry of globex's liaison names the realm its people are in: %+v", theirs)
	}
}

// The way to the confidant is its one port in the control namespace.
func TestPersonSecretsOpenOnePortOfTheControlNamespace(t *testing.T) {
	rule := confidantEgress()
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != layout.Namespace(layout.Control) {
		t.Fatalf("to %+v", rule.To)
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != netpolicy.ConfidantPort {
		t.Fatalf("ports %+v", rule.Ports)
	}
}

type deletedPaths []string

func (d *deletedPaths) DeleteTree(_ context.Context, path string) error {
	*d = append(*d, path)
	return nil
}

// A person the realm says is gone loses what components kept for them, in
// every tenant whose people are in that realm and in no other.
func TestAPersonWhoIsGoneLosesWhatWasKeptForThem(t *testing.T) {
	renamed := rightsTenant("globex")
	renamed.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "shared"}
	also := rightsTenant("initech")
	also.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "shared"}
	c := fake.NewClientBuilder().WithScheme(rightsScheme(t)).WithObjects(rightsTenant("acme"), renamed, also).Build()
	var deleted deletedPaths
	d := &PersonSecretsDepartures{Client: c, Vault: &deleted}
	if err := d.PersonRemoved(context.Background(), "acme", "u-ada"); err != nil {
		t.Fatal(err)
	}
	if err := d.PersonRemoved(context.Background(), "shared", "u-bob"); err != nil {
		t.Fatal(err)
	}
	// Nothing that could not be a person's part of the path is asked for.
	if err := d.PersonRemoved(context.Background(), "acme", "../apps"); err != nil {
		t.Fatal(err)
	}
	want := "gentian-os/tenants/acme/people/u-ada,gentian-os/tenants/globex/people/u-bob,gentian-os/tenants/initech/people/u-bob"
	if got := strings.Join(deleted, ","); got != want {
		t.Fatalf("deleted %s, want %s", got, want)
	}
}
