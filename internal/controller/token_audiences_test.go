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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

func audiencesProfile(origin string, audiences ...gentianov1alpha1.TokenAudience) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "store", Annotations: map[string]string{}}}
	if origin != "" {
		p.Annotations[profilebundle.OriginAnnotation] = origin
	}
	p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: "store", Audiences: audiences}},
	}}
	return p
}

// A name in aud is given only to a profile from a catalogue of the whole
// cluster; one from a tenant's own catalogue, or of no known origin, is held.
func TestTokenAudiencesAreGivenOnlyFromAClusterCatalogue(t *testing.T) {
	for _, c := range []struct {
		name    string
		profile *gentianov1alpha1.ComponentProfile
		held    bool
	}{
		{"none declared, from a tenant's catalogue", audiencesProfile(profilebundle.TenantOrigin("acme", "own")), false},
		{"declared, from a cluster catalogue", audiencesProfile(profilebundle.ClusterOrigin("private"), "store"), false},
		{"declared, from a tenant's own catalogue", audiencesProfile(profilebundle.TenantOrigin("acme", "own"), "store"), true},
		{"declared, of no known origin", audiencesProfile("", "store"), true},
		{"declared, with an origin that cannot be read", audiencesProfile("somewhere", "store"), true},
	} {
		refusal := tokenAudiencesRefusal(c.profile)
		if held := refusal != ""; held != c.held {
			t.Errorf("%s: held = %v (%q), want %v", c.name, held, refusal, c.held)
		}
	}
}

// The Composition holds the same rule, read off the same annotation.
func TestTheCompositionGivesAudiencesByTheSameRule(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "compositions", "app-default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{
		`"` + profilebundle.OriginAnnotation + `"`,
		`hasPrefix "` + profilebundle.ClusterOrigin("") + `" $origin`,
		`not (hasPrefix "gentian" $aud)`,
	} {
		if !strings.Contains(string(raw), must) {
			t.Errorf("app-default.yaml does not hold %s", must)
		}
	}
}
