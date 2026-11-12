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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/bouncer"
)

func tokenEntryProfile() *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{}
	p.Name = "agent-door"
	p.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform
	p.Spec.Launch = gentianov1alpha1.ComponentLaunchNone
	p.Spec.Expose = []gentianov1alpha1.ExposureSpec{
		{Name: "web", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC,
			Backend: gentianov1alpha1.BackendRef{Service: "door", Port: 8080}},
		{Name: "mcp", Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeBearer, SubDomain: "mcp",
			Backend: gentianov1alpha1.BackendRef{Service: "door", Port: 8765}},
	}
	return p
}

func tokenEntryComponent(approvals ...gentianov1alpha1.ExposureEnablement) *gentianov1alpha1.Component {
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "agent-door", "tenant-acme", "uid-1"
	comp.Spec.ProfileRef.Name = "agent-door"
	comp.Spec.Exposures = approvals
	return comp
}

func tokenApproval(kind gentianov1alpha1.ExposureKind, expires *metav1.Time) gentianov1alpha1.ExposureEnablement {
	return gentianov1alpha1.ExposureEnablement{ExposureName: "mcp", Kind: kind, Owner: "tom", ExpiresAt: expires}
}

// Only a profile of platform trust has a token entry, and it is never one of
// the entries routed behind the session.
func TestATokenEntryIsAPlatformProfilesAndIsNotRoutedBehindTheSession(t *testing.T) {
	comp, profile := tokenEntryComponent(), tokenEntryProfile()
	if e := tokenEntryOf(comp, profile); e == nil || e.Name != "mcp" {
		t.Fatalf("token entry = %+v", e)
	}
	routable, refused := routableExposures(comp, profile)
	if len(routable) != 1 || routable[0].Name != "web" || len(refused) != 0 {
		t.Fatalf("routable %v, refused %v", routable, refused)
	}
	// The same entry of a profile that is not of platform trust is refused
	// as any gateway entry with no session was.
	profile.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	if tokenEntryOf(comp, profile) != nil {
		t.Fatal("a certified profile has a token entry")
	}
	if routable, refused = routableExposures(comp, profile); len(routable) != 1 || len(refused) != 1 || refused[0].Name != "mcp" {
		t.Fatalf("routable %v, refused %v", routable, refused)
	}
	// An entry that routes into another component is that component's.
	profile = tokenEntryProfile()
	profile.Spec.Expose[1].Backend.Component = "other"
	if tokenEntryOf(comp, profile) != nil {
		t.Fatal("an entry into another component is this one's token entry")
	}
}

// It is served only while an approval of that kind, not expired, is on the
// Component, and never on a host an entry behind sign-in answers on.
func TestATokenEntryIsServedOnlyApprovedAsOneAndOnAHostOfItsOwn(t *testing.T) {
	now := time.Now()
	past, future := metav1.NewTime(now.Add(-time.Hour)), metav1.NewTime(now.Add(time.Hour))
	const host = "mcp.acme.k.example"
	for name, tc := range map[string]struct {
		approvals []gentianov1alpha1.ExposureEnablement
		host      string
		taken     map[string]bool
		served    bool
		reason    string
	}{
		"not approved":                      {nil, host, nil, false, "AwaitingApproval"},
		"approved as a token entry":         {[]gentianov1alpha1.ExposureEnablement{tokenApproval(gentianov1alpha1.ExposureKindTokenEntry, nil)}, host, nil, true, "Approved"},
		"approved for a while":              {[]gentianov1alpha1.ExposureEnablement{tokenApproval(gentianov1alpha1.ExposureKindTokenEntry, &future)}, host, nil, true, "Approved"},
		"the approval expired":              {[]gentianov1alpha1.ExposureEnablement{tokenApproval(gentianov1alpha1.ExposureKindTokenEntry, &past)}, host, nil, false, "AwaitingApproval"},
		"approved with no kind":             {[]gentianov1alpha1.ExposureEnablement{tokenApproval("", nil)}, host, nil, false, "AwaitingApproval"},
		"approved as a public address":      {[]gentianov1alpha1.ExposureEnablement{tokenApproval(gentianov1alpha1.ExposureKindPublic, nil)}, host, nil, false, "AwaitingApproval"},
		"approved to keep the app's header": {[]gentianov1alpha1.ExposureEnablement{tokenApproval(gentianov1alpha1.ExposureKindSignInAppAuthorization, nil)}, host, nil, false, "AwaitingApproval"},
		"approved, on a session's host":     {[]gentianov1alpha1.ExposureEnablement{tokenApproval(gentianov1alpha1.ExposureKindTokenEntry, nil)}, host, map[string]bool{host: true}, false, "HostShared"},
		"approved, with no host":            {[]gentianov1alpha1.ExposureEnablement{tokenApproval(gentianov1alpha1.ExposureKindTokenEntry, nil)}, "", nil, false, "NoHost"},
	} {
		comp, profile := tokenEntryComponent(tc.approvals...), tokenEntryProfile()
		served := servedTokenEntry(comp, profile, tokenEntryOf(comp, profile), tc.host, tc.taken, now)
		cond := apimeta.FindStatusCondition(comp.Status.Conditions, ConditionTokenEntry)
		if served != tc.served || cond == nil || cond.Reason != tc.reason || (cond.Status == metav1.ConditionTrue) != tc.served {
			t.Errorf("%s: served %v, condition %+v", name, served, cond)
		}
	}
	// A component that declares none carries no such condition.
	comp := tokenEntryComponent()
	comp.Status.Conditions = []metav1.Condition{{Type: ConditionTokenEntry, Status: metav1.ConditionTrue, Reason: "Approved"}}
	if servedTokenEntry(comp, tokenEntryProfile(), nil, "", nil, now) || apimeta.FindStatusCondition(comp.Status.Conditions, ConditionTokenEntry) != nil {
		t.Fatalf("no entry: %+v", comp.Status.Conditions)
	}
}

// The policy in front of a token entry is the bouncer and nothing else: no
// sign-in filter, which would redirect a program and replace its token, and
// nothing that lets a request through when the bouncer cannot be asked.
func TestATokenEntrysPolicyIsTheBouncerAlone(t *testing.T) {
	spec := tokenEntryPolicySpec("agent-door-mcp", servicesNamespace, "b")
	for _, never := range []string{"oidc", "jwt", "basicAuth", "apiKeyAuth", "authorization", "cors"} {
		if _, there := spec[never]; there {
			t.Errorf("the policy carries %s", never)
		}
	}
	if len(spec) != 2 {
		t.Fatalf("the policy carries more than its target and the bouncer: %v", spec)
	}
	ext, _ := spec["extAuth"].(map[string]interface{})
	if ext["failOpen"] != false {
		t.Fatalf("extAuth = %v", ext)
	}
	backend, _, _ := unstructured.NestedMap(spec, "extAuth", "grpc", "backendRef")
	if backend["name"] != "b" || backend["namespace"] != servicesNamespace || backend["port"] != int64(bouncerPort) {
		t.Fatalf("backend = %v", backend)
	}
	targets, _ := spec["targetRefs"].([]interface{})
	if len(targets) != 1 || targets[0].(map[string]interface{})["name"] != "agent-door-mcp" {
		t.Fatalf("targets = %v", targets)
	}
}

// Served, the entry has a route the bouncer's table holds a token against
// and a policy that names it. No longer served, both are gone, and the
// policy of the component's session is not touched either way.
func TestATokenEntrysRouteAndPolicyComeAndGoWithItsApproval(t *testing.T) {
	ctx := context.Background()
	scheme := perimeterScheme()
	comp, profile, tenant := tokenEntryComponent(), tokenEntryProfile(), acmeTenantFixture()
	session := &unstructured.Unstructured{}
	session.SetGroupVersionKind(securityPolicyGVK)
	session.SetName("sp-" + comp.Name)
	session.SetNamespace(comp.Namespace)
	session.SetLabels(componentLabels(comp, tenant.Name))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(session).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi", Cluster: "c1"}
	zone := r.zoneOf(tenant)
	entry := tokenEntryOf(comp, profile)
	host := exposureHost(zone, comp, entry)

	if err := r.ensureTokenEntry(ctx, comp, tenant, profile, zone, entry, true, false); err != nil {
		t.Fatal(err)
	}
	route := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: comp.Namespace, Name: "agent-door-mcp"}, route); err != nil {
		t.Fatal(err)
	}
	ann := route.Annotations
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != host || !strings.HasPrefix(host, "mcp.") {
		t.Fatalf("hostnames = %v", route.Spec.Hostnames)
	}
	if ann[bouncerAuthModeAnnotation] != "bearer" || ann[bouncerTokenAudienceAnnotation] != "https://"+host ||
		ann[bouncerTokenRealmAnnotation] != zone.realm || zone.realm == "" || ann[bouncerForwardAnnotation] != "false" ||
		ann[bouncerRelationAnnotation] == "" || ann[bouncerObjectAnnotation] == "" {
		t.Fatalf("annotations = %v", ann)
	}
	// Nothing of a session: written empty where the reconciler always writes
	// the key, absent otherwise.
	for _, never := range []string{bouncerSessionCookiesAnnotation, bouncerExchangeScopeAnnotation, bouncerIDTokenAudienceAnnotation, bouncerClientAuthorizationAnnotation} {
		if ann[never] != "" {
			t.Errorf("a token route says %s: %q", never, ann[never])
		}
	}
	// No rule for the sign-in filter's own paths: there is no such filter.
	for _, rule := range route.Spec.Rules {
		for _, m := range rule.Matches {
			if m.Path != nil && m.Path.Value != nil && strings.HasPrefix(*m.Path.Value, edgeOAuth2Prefix) {
				t.Errorf("a token route serves %s", *m.Path.Value)
			}
		}
	}
	// What the bouncer is told about it loads, and is a token route.
	rows, err := componentRouteTableEntries(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	table, err := bouncerRouteTable(nil, rows)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := bouncer.ParseTable([]byte(table))
	if err != nil {
		t.Fatal(err)
	}
	if row := parsed.Match(host); row == nil || row.AuthMode != bouncer.AuthModeBearer || row.Audience != "https://"+host || row.Realm != zone.realm {
		t.Fatalf("table row = %+v\n%s", row, table)
	}

	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(securityPolicyGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: comp.Namespace, Name: tokenEntryPolicyName(comp.Name)}, policy); err != nil {
		t.Fatal(err)
	}
	if _, has, _ := unstructured.NestedMap(policy.Object, "spec", "oidc"); has {
		t.Fatal("the token entry's policy carries a session")
	}
	targets, _, _ := unstructured.NestedSlice(policy.Object, "spec", "targetRefs")
	if len(targets) != 1 || targets[0].(map[string]interface{})["name"] != route.Name {
		t.Fatalf("the policy names %v, the route is %s", targets, route.Name)
	}

	// The approval ends.
	if err := r.ensureTokenEntry(ctx, comp, tenant, profile, zone, entry, false, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: comp.Namespace, Name: "agent-door-mcp"}, route); !apierrors.IsNotFound(err) {
		t.Fatalf("the route is still there: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: comp.Namespace, Name: tokenEntryPolicyName(comp.Name)}, policy); !apierrors.IsNotFound(err) {
		t.Fatalf("the policy is still there: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: comp.Namespace, Name: session.GetName()}, session); err != nil {
		t.Fatalf("the session's policy went with it: %v", err)
	}
	// And a component with no token entry at all is asked the same, and
	// nothing fails.
	if err := r.ensureTokenEntry(ctx, comp, tenant, profile, zone, nil, false, false); err != nil {
		t.Fatal(err)
	}
}
