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
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// nextcloudApp is an app with a sign-in client and a pack, as a catalog
// brings them: the pack's mappers say which tokens a claim goes into for the
// app's own sign-in.
func nextcloudApp() (*gentianov1alpha1.ComponentProfile, *gentianov1alpha1.OIDCPackCatalog) {
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-base-ce"}}
	profile.Spec.Classes = []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "gentian-nextcloud-base-ce"}},
	}}
	catalog := &gentianov1alpha1.OIDCPackCatalog{ObjectMeta: metav1.ObjectMeta{
		Name: "nextcloud-base-ce-oidc", Labels: map[string]string{"gentianos.io/profile-name": "nextcloud-base-ce"},
	}}
	attribute := func(claim string) gentianov1alpha1.OIDCMapperTemplate {
		return gentianov1alpha1.OIDCMapperTemplate{ProtocolMapper: "oidc-usermodel-attribute-mapper", Config: map[string]string{
			"user.attribute": "username", "claim.name": claim, "jsonType.label": "String",
			"id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true", "introspection.token.claim": "true",
		}}
	}
	catalog.Spec.MapperTemplates = map[string]gentianov1alpha1.OIDCMapperTemplate{
		"gentian_username": attribute("gentian_username"),
		"email":            attribute("email"),
		// In the catalog and in no pack of this client: not the app's.
		"groups": {ProtocolMapper: "oidc-group-membership-mapper", Config: map[string]string{"claim.name": "groups"}},
		// Names no mapper: left out.
		"broken": {},
	}
	catalog.Spec.Packs = map[string]gentianov1alpha1.OIDCPackSpec{
		"gentian-nextcloud-base-ce": {Mappers: []string{"gentian_username", "email", "broken", "missing", "email"}},
		"another-client":            {Mappers: []string{"groups"}},
	}
	return profile, catalog
}

// The scope of a bridged app says what the app's own sign-in says, and
// nothing else: its sign-in client as the audience, and the claims of that
// client's pack, without the pack's word on which tokens they go into.
func TestABridgeScopeCarriesTheAppsOwnSignInAndNoMore(t *testing.T) {
	profile, catalog := nextcloudApp()
	scope, ok := bridgeScopeFor(profile, &catalog.Spec)
	if !ok {
		t.Fatal("no scope for an app with a sign-in client")
	}
	want := bridgeScope{
		Profile: "nextcloud-base-ce", Audience: "gentian-nextcloud-base-ce",
		Mappers: []bridgeScopeMapper{
			{Name: "email", Slug: "email", Type: "oidc-usermodel-attribute-mapper",
				Config: map[string]string{"user.attribute": "username", "claim.name": "email", "jsonType.label": "String"}},
			{Name: "gentian_username", Slug: "gentian-username", Type: "oidc-usermodel-attribute-mapper",
				Config: map[string]string{"user.attribute": "username", "claim.name": "gentian_username", "jsonType.label": "String"}},
		},
	}
	if !reflect.DeepEqual(scope, want) {
		t.Fatalf("scope\n%+v\nwant\n%+v", scope, want)
	}

	// With no pack the app is still the audience, and no claim is invented.
	scope, ok = bridgeScopeFor(profile, nil)
	if !ok || scope.Audience != "gentian-nextcloud-base-ce" || len(scope.Mappers) != 0 {
		t.Fatalf("without a pack: %+v %v", scope, ok)
	}
	// An app with no sign-in client has nothing to verify a token with.
	profile.Spec.Requires = nil
	if scope, ok = bridgeScopeFor(profile, &catalog.Spec); ok {
		t.Fatalf("a scope for an app with no sign-in client: %+v", scope)
	}
}

// The realm is told of a bridge's app only while the bridge is served, and
// is told nothing once it is not.
func TestTheRealmIsToldOfABridgedAppOnlyWhileItsBridgeIsServed(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := acmeTenantFixture()
	app, catalog := nextcloudApp()
	appComp := componentFor("nextcloud", tenant.NamespaceName())
	appComp.Spec.ProfileRef.Name = app.Name
	bridge := nextcloudBridge()
	bridgeComp := componentFor(bridge.Name, tenant.NamespaceName())
	bridgeComp.Spec.ProfileRef.Name = bridge.Name
	bridgeComp.Spec.Class = gentianov1alpha1.ComponentClassBridge

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&gentianov1alpha1.Component{}).
		WithObjects(tenant.DeepCopy(), app, catalog, appComp, bridge, bridgeComp).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	ctx := context.Background()
	projected := func() (string, bool) {
		t.Helper()
		if err := r.projectZoneHosts(ctx); err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Name: zoneHostsConfigMapName(tenant.Name), Namespace: layout.Namespace(layout.Control)}, cm); err != nil {
			t.Fatal(err)
		}
		doc, ok := cm.Data[zoneBridgeScopesKey]
		return doc, ok
	}
	serve := func(tokenEntry, connected metav1.ConditionStatus) {
		t.Helper()
		bridgeComp.Status.Conditions = []metav1.Condition{
			{Type: ConditionTokenEntry, Status: tokenEntry, Reason: "x", LastTransitionTime: metav1.Now()},
			{Type: ConditionBridge, Status: connected, Reason: "x", LastTransitionTime: metav1.Now()},
		}
		if err := c.Status().Update(ctx, bridgeComp); err != nil {
			t.Fatal(err)
		}
	}

	if doc, ok := projected(); ok {
		t.Fatalf("installed and not served: %s", doc)
	}
	serve(metav1.ConditionFalse, metav1.ConditionTrue)
	if doc, ok := projected(); ok {
		t.Fatalf("not approved: %s", doc)
	}
	serve(metav1.ConditionTrue, metav1.ConditionFalse)
	if doc, ok := projected(); ok {
		t.Fatalf("approved, its app not connected: %s", doc)
	}

	serve(metav1.ConditionTrue, metav1.ConditionTrue)
	raw, ok := projected()
	if !ok {
		t.Fatal("a served bridge's app is not projected")
	}
	var doc bridgeScopesDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if len(doc.Scopes) != 1 || doc.Scopes[0].Profile != "nextcloud-base-ce" || doc.Scopes[0].Audience != "gentian-nextcloud-base-ce" || len(doc.Scopes[0].Mappers) != 2 {
		t.Fatalf("%+v", doc)
	}
	// The same state is the same document: the Composition is not woken.
	if again, _ := projected(); again != raw {
		t.Fatalf("the document changed between two passes:\n%s\n%s", raw, again)
	}

	serve(metav1.ConditionFalse, metav1.ConditionTrue)
	if doc, ok := projected(); ok {
		t.Fatalf("the approval ended and the realm is still told: %s", doc)
	}
}

func TestBridgeSlug(t *testing.T) {
	for in, want := range map[string]string{
		"gentian_username": "gentian-username", "Full Name": "full-name", "email": "email", "__x__": "x", "é": "",
	} {
		if got := bridgeSlug(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
