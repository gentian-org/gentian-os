/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func profileRequiring(services gentianov1alpha1.ServiceRequirements, annotations map[string]string) *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone,
			TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{Services: &services},
		},
	}
}

// opened reads a policy's egress back as "namespace:port" lines, sorted, with
// "namespace:*" for a namespace opened whole. A rule that is not exactly one
// namespace, or a port that is not TCP by number, fails the test: those are
// the shapes that would open more than they say.
func opened(t *testing.T, np *networkingv1.NetworkPolicy) []string {
	t.Helper()
	if np == nil {
		return nil
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("policy types = %v", np.Spec.PolicyTypes)
	}
	var out []string
	for _, rule := range np.Spec.Egress {
		if len(rule.To) != 1 || rule.To[0].NamespaceSelector == nil || rule.To[0].PodSelector != nil || rule.To[0].IPBlock != nil ||
			len(rule.To[0].NamespaceSelector.MatchLabels) != 1 || len(rule.To[0].NamespaceSelector.MatchExpressions) != 0 {
			t.Fatalf("a rule must name exactly one namespace: %+v", rule)
		}
		ns := rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
		if ns == "" {
			t.Fatalf("a rule must select its namespace by name: %+v", rule.To[0])
		}
		if len(rule.Ports) == 0 {
			out = append(out, ns+":*")
		}
		for _, p := range rule.Ports {
			if p.Protocol == nil || *p.Protocol != corev1.ProtocolTCP || p.Port == nil || p.Port.IntValue() == 0 || p.EndPort != nil {
				t.Fatalf("%s: a port must be one TCP port by number: %+v", ns, p)
			}
			out = append(out, fmt.Sprintf("%s:%d", ns, p.Port.IntValue()))
		}
	}
	sort.Strings(out)
	return out
}

// Each declared store opens the namespace of the server it was provisioned
// on and the port that server answers on, and nothing of any other store.
func TestADeclaredStoreOpensItsOwnServerAndNoOther(t *testing.T) {
	t.Parallel()
	pg, maria := layout.System("postgresql"), layout.System("mariadb")
	redis, s3 := layout.System("cache"), layout.System("s3")
	type services = gentianov1alpha1.ServiceRequirements
	for name, c := range map[string]struct {
		services services
		want     []string
	}{
		"postgresql": {
			services{Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEnginePostgreSQL}},
			[]string{pg + ":5432"}},
		"mariadb": {
			services{Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEngineMariaDB}},
			[]string{maria + ":3306"}},
		// The API's default for an engine left out.
		"a database with no engine named": {
			services{Database: &gentianov1alpha1.DatabaseRequirement{}},
			[]string{pg + ":5432"}},
		"redis": {
			services{Cache: &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis}},
			[]string{redis + ":6379"}},
		"a cache with no engine named": {
			services{Cache: &gentianov1alpha1.CacheRequirement{}},
			[]string{redis + ":6379"}},
		// Memcached is the tenant's own, in the tenant's namespace: the
		// tenant-cache policies open it, and the shared Redis is not its.
		"memcached": {
			services{Cache: &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineMemcached}},
			nil},
		"object storage": {
			services{Storage: &gentianov1alpha1.StorageRequirement{S3: &gentianov1alpha1.S3Requirement{}}},
			[]string{s3 + ":9000"}},
		// Files are another app's to serve, not the object store's.
		"storage that asks for files only": {
			services{Storage: &gentianov1alpha1.StorageRequirement{Files: &gentianov1alpha1.FilesRequirement{}}},
			nil},
		"mail": {
			services{Mail: &gentianov1alpha1.MailRequirement{SMTP: &gentianov1alpha1.SMTPRequirement{}}},
			[]string{layout.System("mail-dmz") + ":*", layout.System("mail") + ":*"}},
		"nothing": {services{}, nil},
		"mariadb beside redis and a bucket": {
			services{
				Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEngineMariaDB},
				Cache:    &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis},
				Storage:  &gentianov1alpha1.StorageRequirement{S3: &gentianov1alpha1.S3Requirement{}},
			},
			[]string{redis + ":6379", maria + ":3306", s3 + ":9000"}},
	} {
		np := netpolicy.KernelAccessNetworkPolicy("demo", "tenant-demo", "notes", profileRequiring(c.services, nil), netpolicy.DefaultConfig())
		got := opened(t, np)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: opens %v, want %v", name, got, want)
		}
		if np != nil && (np.Name != "kernel-access-notes" || np.Spec.PodSelector.MatchLabels["gentianos.io/app"] != "notes") {
			t.Errorf("%s: policy %q selects %v", name, np.Name, np.Spec.PodSelector.MatchLabels)
		}
	}
}

// The mistake this replaces: a database of either engine opened PostgreSQL,
// and MariaDB was never opened at all.
func TestADatabaseOfOneEngineOpensNothingOfTheOther(t *testing.T) {
	t.Parallel()
	for engine, other := range map[gentianov1alpha1.DatabaseEngine]string{
		gentianov1alpha1.DatabaseEngineMariaDB:    layout.System("postgresql"),
		gentianov1alpha1.DatabaseEnginePostgreSQL: layout.System("mariadb"),
	} {
		np := netpolicy.KernelAccessNetworkPolicy("demo", "tenant-demo", "notes",
			profileRequiring(gentianov1alpha1.ServiceRequirements{Database: &gentianov1alpha1.DatabaseRequirement{Engine: engine}}, nil),
			netpolicy.DefaultConfig())
		for _, line := range opened(t, np) {
			if strings.HasPrefix(line, other+":") {
				t.Errorf("a %s app is given a way to %s", engine, line)
			}
		}
	}
}

// The annotation is the profile's own widening and stays what it was: the
// namespaces it names, whole, beside what the stores open. Where it names a
// namespace a store opens too, the whole namespace is what is open.
func TestTheEgressAnnotationOpensWholeNamespacesBesideTheStores(t *testing.T) {
	t.Parallel()
	maria := layout.System("mariadb")
	database := gentianov1alpha1.ServiceRequirements{Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEngineMariaDB}}
	np := netpolicy.KernelAccessNetworkPolicy("demo", "tenant-demo", "notes", profileRequiring(database, map[string]string{
		gentianov1alpha1.AnnotationProfileKernelEgressNamespaces: "gentian-system",
	}), netpolicy.DefaultConfig())
	if got, want := opened(t, np), []string{"gentian-system:*", maria + ":3306"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("opens %v, want %v", got, want)
	}
	np = netpolicy.KernelAccessNetworkPolicy("demo", "tenant-demo", "notes", profileRequiring(database, map[string]string{
		gentianov1alpha1.AnnotationProfileKernelEgressNamespaces: maria,
	}), netpolicy.DefaultConfig())
	if got, want := opened(t, np), []string{maria + ":*"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("opens %v, want %v", got, want)
	}
}

// A policy the operator no longer builds is one it must stop keeping: an app
// whose profile opens nothing here any more does not go on holding what an
// earlier reading of it opened.
func TestAPolicyThatIsNoLongerBuiltIsNoLongerKept(t *testing.T) {
	t.Parallel()
	in := func(services gentianov1alpha1.ServiceRequirements) netpolicy.BuildInput {
		return netpolicy.BuildInput{
			TenantName: "demo", Namespace: "tenant-demo",
			Apps:     []gentianov1alpha1.TenantApp{{Profile: "notes"}},
			Profiles: map[string]*gentianov1alpha1.ComponentProfile{"notes": profileRequiring(services, nil)},
			Config:   netpolicy.DefaultConfig(),
		}
	}
	kept := func(names map[string]struct{}, name string) bool { _, ok := names[name]; return ok }

	memcached := netpolicy.ManagedPolicyNames(in(gentianov1alpha1.ServiceRequirements{
		Cache: &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineMemcached}}))
	if kept(memcached, "kernel-access-notes") {
		t.Error("a memcached app opens nothing in the system tier, and its old kernel-access policy would be kept")
	}
	if !kept(memcached, "tenant-cache-egress") || !kept(memcached, "tenant-cache-ingress") {
		t.Errorf("a memcached app's tenant-cache policies are kept: %v", memcached)
	}

	redis := netpolicy.ManagedPolicyNames(in(gentianov1alpha1.ServiceRequirements{
		Cache: &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis}}))
	if !kept(redis, "kernel-access-notes") {
		t.Error("a redis app keeps its kernel-access policy")
	}
	if kept(redis, "tenant-cache-egress") || kept(redis, "tenant-cache-ingress") {
		t.Error("a tenant with no memcached app would keep the policies an earlier one needed")
	}

	// A profile that cannot be read says nothing about what it opens, and
	// its app keeps what it had until it can be.
	unknown := in(gentianov1alpha1.ServiceRequirements{})
	unknown.Profiles = nil
	names := netpolicy.ManagedPolicyNames(unknown)
	for _, name := range []string{"kernel-access-notes", "tenant-cache-egress", "tenant-cache-ingress"} {
		if !kept(names, name) {
			t.Errorf("%s must be kept while the profile is unknown", name)
		}
	}
}

func TestKernelAccessNetworkPolicy_ProfileKernelEgressNamespaces(t *testing.T) {
	t.Parallel()
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				gentianov1alpha1.AnnotationProfileKernelEgressNamespaces: "gentian-system",
			},
		},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{},
			}},
		},
	}
	np := netpolicy.KernelAccessNetworkPolicy("demo", "tenant-demo", "notes", profile, netpolicy.DefaultConfig())
	if np == nil {
		t.Fatal("expected network policy")
	}
	if len(np.Spec.Egress) < 2 {
		t.Fatalf("expected infra + gentian-system egress, got %d rules", len(np.Spec.Egress))
	}
}

// An app that declares the model gateway may reach the gateway's port in the
// gateway's namespace, and nothing else there: its database, its cache and
// the model servers answer on other ports. An app that does not declare it
// has no path at all.
func TestTheModelGatewayIsOpenedForADeclaringAppOnItsPortAlone(t *testing.T) {
	t.Parallel()
	llm := layout.System("llm")

	declaring := profileRequiring(gentianov1alpha1.ServiceRequirements{LLM: &gentianov1alpha1.LLMRequirement{}}, nil)
	np := netpolicy.KernelAccessNetworkPolicy("acme", "tenant-acme", "chat", declaring, netpolicy.Config{})
	if got, want := opened(t, np), []string{fmt.Sprintf("%s:%d", llm, provisioner.ModelGatewayPort)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("opened = %v, want %v", got, want)
	}
	if np.Name != "kernel-access-chat" || np.Spec.PodSelector.MatchLabels["gentianos.io/app"] != "chat" {
		t.Errorf("policy %s selects %v", np.Name, np.Spec.PodSelector.MatchLabels)
	}

	// Beside a store: each its own namespace and port.
	both := profileRequiring(gentianov1alpha1.ServiceRequirements{
		LLM:   &gentianov1alpha1.LLMRequirement{},
		Cache: &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis},
	}, nil)
	got := opened(t, netpolicy.KernelAccessNetworkPolicy("acme", "tenant-acme", "chat", both, netpolicy.Config{}))
	want := []string{
		fmt.Sprintf("%s:%d", layout.System("cache"), provisioner.RedisPort),
		fmt.Sprintf("%s:%d", llm, provisioner.ModelGatewayPort),
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("opened = %v, want %v", got, want)
	}

	// Not declared: nothing towards the gateway, whatever else is declared.
	for name, services := range map[string]gentianov1alpha1.ServiceRequirements{
		"nothing":  {},
		"a cache":  {Cache: &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis}},
		"identity": {Identity: &gentianov1alpha1.IdentityRequirement{}},
	} {
		for _, line := range opened(t, netpolicy.KernelAccessNetworkPolicy("acme", "tenant-acme", "wiki", profileRequiring(services, nil), netpolicy.Config{ServicesNamespace: "kernel-edge"})) {
			if strings.HasPrefix(line, llm+":") {
				t.Errorf("an app that declared %s can reach the model gateway's namespace: %s", name, line)
			}
		}
	}

	// The annotation still opens a namespace whole, the gateway's included:
	// it is the wider of the two, and a profile that declares the gateway no
	// longer needs it.
	annotated := profileRequiring(gentianov1alpha1.ServiceRequirements{LLM: &gentianov1alpha1.LLMRequirement{}},
		map[string]string{gentianov1alpha1.AnnotationProfileKernelEgressNamespaces: llm})
	if got := opened(t, netpolicy.KernelAccessNetworkPolicy("acme", "tenant-acme", "chat", annotated, netpolicy.Config{})); !reflect.DeepEqual(got, []string{llm + ":*"}) {
		t.Errorf("opened = %v, want the namespace whole", got)
	}
}

// An app that signs people in itself reaches the identity provider and the
// edge. An app whose people are signed in by the platform's sidecar reaches
// neither: it never talks to the identity provider, and the sidecar that
// does has a policy of its own, for one port (app-default.yaml).
func TestAnAppBehindTheSignInSidecarDoesNotReachTheIdentityProvider(t *testing.T) {
	t.Parallel()
	cfg := netpolicy.Config{ServicesNamespace: "kernel-edge"}
	auth := layout.Namespace(layout.Authentication)

	for name, identity := range map[string]*gentianov1alpha1.IdentityRequirement{
		"an OIDC client":      {OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "wiki"}},
		"a SAML client":       {SAML: &gentianov1alpha1.SAMLClientSpec{EntityID: "wiki", ACSURL: "https://wiki/acs"}},
		"identity, unadorned": {},
	} {
		np := netpolicy.KernelAccessNetworkPolicy("acme", "tenant-acme", "wiki",
			profileRequiring(gentianov1alpha1.ServiceRequirements{Identity: identity}, nil), cfg)
		want := []string{auth + ":*", "kernel-edge:*"}
		sort.Strings(want)
		if got := opened(t, np); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: opened = %v, want %v", name, got, want)
		}
	}

	sidecar := gentianov1alpha1.ServiceRequirements{
		Identity: &gentianov1alpha1.IdentityRequirement{Sidecar: &gentianov1alpha1.SignInSidecarSpec{Database: true}},
		Database: &gentianov1alpha1.DatabaseRequirement{Engine: gentianov1alpha1.DatabaseEnginePostgreSQL},
	}
	np := netpolicy.KernelAccessNetworkPolicy("acme", "tenant-acme", "notes", profileRequiring(sidecar, nil), cfg)
	want := []string{fmt.Sprintf("%s:%d", layout.System("postgresql"), provisioner.PostgresPort)}
	if got := opened(t, np); !reflect.DeepEqual(got, want) {
		t.Errorf("an app behind the sidecar: opened = %v, want its database alone, %v", got, want)
	}
	if only := netpolicy.KernelAccessNetworkPolicy("acme", "tenant-acme", "notes", profileRequiring(gentianov1alpha1.ServiceRequirements{
		Identity: &gentianov1alpha1.IdentityRequirement{Sidecar: &gentianov1alpha1.SignInSidecarSpec{}},
	}, nil), cfg); only != nil {
		t.Errorf("an app that declares the sidecar and nothing else is opened to %v", opened(t, only))
	}
}
