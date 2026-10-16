/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestATenantNamesTheOutsideRealmsItsPeopleSignInAt(t *testing.T) {
	entry := BrokeredTo{
		Name:        "aluvian",
		RedirectURI: "https://id.aluvian.example/auth/realms/customers/broker/acme/endpoint",
		JWKSURL:     "https://id.aluvian.example/auth/realms/customers/protocol/openid-connect/certs",
	}
	if err := ValidBrokeredTo(entry); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []BrokeredTo{
		{Name: "Not_A_Label", RedirectURI: entry.RedirectURI, JWKSURL: entry.JWKSURL},
		{Name: "x", RedirectURI: "http://id.example/endpoint", JWKSURL: entry.JWKSURL},
		{Name: "x", RedirectURI: entry.RedirectURI, JWKSURL: "https:///certs"},
	} {
		if ValidBrokeredTo(bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}

	plain := tenantManifest("acme", "Acme", true, false)
	if withBrokeredTo(plain, nil) != plain {
		t.Error("a tenant without one is written as before")
	}
	text := withBrokeredTo(plain, []BrokeredTo{entry})
	var doc struct {
		Spec struct {
			BrokeredTo []BrokeredTo `json:"brokeredTo"`
			Isolation  struct {
				Mode string `json:"mode"`
			} `json:"isolation"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("the manifest is not YAML: %v\n%s", err, text)
	}
	if len(doc.Spec.BrokeredTo) != 1 || doc.Spec.BrokeredTo[0] != entry || doc.Spec.Isolation.Mode != "namespace" {
		t.Fatalf("manifest says %+v\n%s", doc.Spec, text)
	}
	if !strings.Contains(text, "no secret is made or handed over") {
		t.Error("the manifest does not say what the block does")
	}
}
