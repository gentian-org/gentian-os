/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func clientProfile(name string, oidc gentianov1alpha1.OIDCClientSpec) gentianov1alpha1.ComponentProfile {
	if oidc.ClientID == "" {
		oidc.ClientID = name
	}
	p := gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &oidc},
	}}
	return p
}

func audiences(names ...string) gentianov1alpha1.OIDCClientSpec {
	var a []gentianov1alpha1.TokenAudience
	for _, n := range names {
		a = append(a, gentianov1alpha1.TokenAudience(n))
	}
	return gentianov1alpha1.OIDCClientSpec{Audiences: a}
}

// An audience is the profile's own and nothing else's: its name, a dash and
// a label, from any catalog, and never what the platform, a realm's own
// clients or another profile are called.
func TestAnAudienceIsTheProfilesOwn(t *testing.T) {
	others := []gentianov1alpha1.ComponentProfile{clientProfile("store-web", gentianov1alpha1.OIDCClientSpec{})}
	for _, c := range []struct {
		name, profile string
		audiences     []string
		held          string
	}{
		{"none declared", "store", nil, ""},
		{"its own name and a label", "store", []string{"store-api"}, ""},
		{"two of them", "store", []string{"store-api", "store-billing"}, ""},
		{"another name", "store", []string{"billing-api"}, "the profile's own"},
		{"its name alone", "store", []string{"store"}, "the profile's own"},
		{"a name that only begins like its own", "store", []string{"storefront-api"}, "the profile's own"},
		{"another profile", "store", []string{"store-web"}, "another profile"},
		{"a platform service", "gentian", []string{"gentian-director"}, "a service of the platform's"},
		{"a realm's own client", "realm", []string{"realm-management"}, "a service of the platform's"},
		{"a kernel service", "keycloak", []string{"keycloak-idp"}, "a service of the platform's"},
	} {
		refusal := signInClientRefusal(ptr(clientProfile(c.profile, audiences(c.audiences...))), others, false)
		if (refusal == "") != (c.held == "") || !strings.Contains(refusal, c.held) {
			t.Errorf("%s: refusal %q, want one containing %q", c.name, refusal, c.held)
		}
	}
}

// A profile named what another profile names as an audience is held: that
// profile's tokens would be accepted as this app's, whichever came first.
func TestAProfileNamedAsAnotherOnesAudienceIsHeld(t *testing.T) {
	others := []gentianov1alpha1.ComponentProfile{clientProfile("store", audiences("store-web"))}
	refusal := signInClientRefusal(ptr(clientProfile("store-web", gentianov1alpha1.OIDCClientSpec{})), others, false)
	if !strings.Contains(refusal, "names as an audience") {
		t.Fatalf("refusal %q", refusal)
	}
	if r := signInClientRefusal(ptr(clientProfile("store", audiences("store-web"))), others, false); r != "" {
		t.Fatalf("the declaring profile itself was held: %q", r)
	}
}

func withExtensionClient(p gentianov1alpha1.ComponentProfile, extension string, oidc gentianov1alpha1.OIDCClientSpec) gentianov1alpha1.ComponentProfile {
	if oidc.ClientID == "" {
		oidc.ClientID = p.Name + "-" + extension
	}
	p.Spec.Extensions = append(p.Spec.Extensions, gentianov1alpha1.AppSidecarSpec{
		Name:                extension,
		ServiceRequirements: &gentianov1alpha1.ServiceRequirements{Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &oidc}},
	})
	return p
}

// An extension's client is composed with its audiences as the app's own is,
// so they are the profile's own as well, and another profile's extension
// naming this one is held as the other profile itself would be.
func TestAnExtensionsAudienceIsTheProfilesOwn(t *testing.T) {
	others := []gentianov1alpha1.ComponentProfile{clientProfile("store-web", gentianov1alpha1.OIDCClientSpec{})}
	for _, c := range []struct {
		name      string
		audiences []string
		held      string
	}{
		{"its profile's name and a label", []string{"store-till-api"}, ""},
		{"another name", []string{"billing-api"}, "extensions[till].kernelRequirements.identity.oidc.audiences"},
		{"another profile", []string{"store-web"}, "another profile"},
		{"a platform service", []string{"gentian-director"}, "the profile's own"},
	} {
		profile := withExtensionClient(clientProfile("store", gentianov1alpha1.OIDCClientSpec{}), "till", audiences(c.audiences...))
		refusal := signInClientRefusal(&profile, others, false)
		if (refusal == "") != (c.held == "") || !strings.Contains(refusal, c.held) {
			t.Errorf("%s: refusal %q, want one containing %q", c.name, refusal, c.held)
		}
	}

	naming := []gentianov1alpha1.ComponentProfile{withExtensionClient(clientProfile("store", gentianov1alpha1.OIDCClientSpec{}), "till", audiences("store-web"))}
	if refusal := signInClientRefusal(ptr(clientProfile("store-web", gentianov1alpha1.OIDCClientSpec{})), naming, false); !strings.Contains(refusal, "names as an audience") {
		t.Fatalf("a profile another's extension names as an audience: refusal %q", refusal)
	}
}

func boundParameter(mutate func(*gentianov1alpha1.OIDCRequestParameterSpec)) gentianov1alpha1.OIDCClientSpec {
	p := &gentianov1alpha1.OIDCRequestParameterSpec{
		Name:        "tenant_url",
		Pattern:     `https://(?<host>[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+)`,
		RedirectURI: "https://store.${host}/oauth/callback",
		Claim:       "tenant_url",
	}
	if mutate != nil {
		mutate(p)
	}
	return gentianov1alpha1.OIDCClientSpec{AccessType: "PUBLIC", RequestParameter: p}
}

// A request parameter is held to what the extension can bind: a parameter and
// a claim of the app's own, a pattern both Go and Java read, an address that
// names its groups and is plain https -- and a realm of the tenant's own.
func TestARequestParameterIsHeldToWhatTheRealmCanBind(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*gentianov1alpha1.OIDCRequestParameterSpec)
		kernel bool
		held   string
	}{
		{"the store's", nil, false, ""},
		{"no claim", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.Claim = "" }, false, ""},
		{"in the kernel realm", nil, true, "kernel realm"},
		{"a protocol parameter", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.Name = "redirect_uri" }, false, "the protocol's own"},
		{"a claim of JWT's", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.Claim = "sub" }, false, "already gives a meaning"},
		{"Go's spelling of a group", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.Pattern = `https://(?P<host>[a-z.]+)` }, false, "Java does not read"},
		{"not a pattern", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.Pattern = `https://(?<host>[a-z` }, false, "not one"},
		{"a back-reference", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.Pattern = `(?<host>a)\1` }, false, "not one"},
		{"a group the pattern lacks", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.RedirectURI = "https://store.${tenant}/cb" }, false, "no group of"},
		{"an address not computed from it", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.RedirectURI = "https://store.example/cb" }, false, "names no ${group}"},
		{"a stray dollar", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.RedirectURI = "https://store.${host}/$cb" }, false, "not a ${group}"},
		{"a user in the address", func(p *gentianov1alpha1.OIDCRequestParameterSpec) { p.RedirectURI = "https://me@store.${host}/cb" }, false, "not an https address"},
	} {
		refusal := signInClientRefusal(ptr(clientProfile("store", boundParameter(c.mutate))), nil, c.kernel)
		if (refusal == "") != (c.held == "") || !strings.Contains(refusal, c.held) {
			t.Errorf("%s: refusal %q, want one containing %q", c.name, refusal, c.held)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// The Composition holds the names the operator does, list for list, and
// every kernel service is among the reserved audiences.
func TestTheCompositionHoldsTheSameNames(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "compositions", "app-default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for variable, want := range map[string][]string{
		"reservedAudiences":  reservedAudiences,
		"protocolParameters": protocolParameters,
		"reservedClaims":     reservedClaims,
	} {
		line := regexp.MustCompile(`\$` + variable + ` := list ([^}]*)\}\}`).FindStringSubmatch(string(raw))
		if line == nil {
			t.Errorf("app-default.yaml has no $%s", variable)
			continue
		}
		var got []string
		for _, q := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(line[1], -1) {
			got = append(got, q[1])
		}
		if !slices.Equal(got, want) {
			t.Errorf("app-default.yaml $%s = %v, the operator's = %v", variable, got, want)
		}
	}
	entries, err := os.ReadDir(filepath.Join("..", "..", "kernel", "services"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "_") && !slices.Contains(reservedAudiences, e.Name()) {
			t.Errorf("kernel service %s is not among the reserved audiences", e.Name())
		}
	}
	for _, must := range []string{`not (hasPrefix "gentian" $aud)`, `(hasPrefix (printf "%s-" $profileName) $aud)`} {
		if !strings.Contains(string(raw), must) {
			t.Errorf("app-default.yaml does not hold %s", must)
		}
	}
}
