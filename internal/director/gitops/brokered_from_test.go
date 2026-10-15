/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"errors"
	"testing"

	"sigs.k8s.io/yaml"
)

// An entry is taken as the CRD would take it, and its domain is the one its
// issuer is published under: an outside realm cannot claim the addresses of
// a domain it does not serve its sign-in from.
func TestAnOutsideRealmClaimsOnlyItsOwnDomain(t *testing.T) {
	entry := BrokeredFrom{
		Name: "abc", DisplayName: "ABC Zürich AG", Issuer: "https://id.abc.example/auth/realms/main",
		ClientID: "broker-to-aluvian", Domain: "abc.example",
	}
	if err := ValidBrokeredFrom(entry); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(b *BrokeredFrom){
		"name not a label":      func(b *BrokeredFrom) { b.Name = "Not_A_Label" },
		"empty display name":    func(b *BrokeredFrom) { b.DisplayName = "" },
		"display name quoted":   func(b *BrokeredFrom) { b.DisplayName = `ABC "AG"` },
		"display name two line": func(b *BrokeredFrom) { b.DisplayName = "ABC\nAG" },
		"client id":             func(b *BrokeredFrom) { b.ClientID = "broker to" },
		"issuer http":           func(b *BrokeredFrom) { b.Issuer = "http://id.abc.example/auth/realms/main" },
		"issuer with slash":     func(b *BrokeredFrom) { b.Issuer = "https://id.abc.example/auth/realms/main/" },
		"issuer with port":      func(b *BrokeredFrom) { b.Issuer = "https://id.abc.example:8443/auth/realms/main" },
		"issuer with query":     func(b *BrokeredFrom) { b.Issuer = "https://id.abc.example/auth/realms/main?x=1" },
		"issuer no path":        func(b *BrokeredFrom) { b.Issuer = "https://id.abc.example" },
		"domain not a domain":   func(b *BrokeredFrom) { b.Domain = "abc" },
		"another's domain":      func(b *BrokeredFrom) { b.Domain = "gmail.com" },
		"a parent domain":       func(b *BrokeredFrom) { b.Domain = "example" },
		"the issuer's own host": func(b *BrokeredFrom) { b.Domain = "id.abc.example" },
	}
	for name, change := range bad {
		b := entry
		change(&b)
		if err := ValidBrokeredFrom(b); !errors.Is(err, ErrInvalidBrokeredFrom) {
			t.Errorf("%s: accepted %+v (%v)", name, b, err)
		}
	}
}

// The patch is a Tenant naming the entries in the order of their names, and
// reads back as what was written.
func TestTheOutsideRealmsPatchReadsBack(t *testing.T) {
	list := []BrokeredFrom{
		{Name: "zed", DisplayName: "Zed GmbH", Issuer: "https://id.zed.example/auth/realms/z", ClientID: "broker-to-aluvian", Domain: "zed.example"},
		{Name: "abc", DisplayName: "ABC Zürich AG", Issuer: "https://id.abc.example/auth/realms/main", ClientID: "broker-to-aluvian", Domain: "abc.example"},
	}
	text := renderBrokeredFrom("customers", list)
	var doc struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			BrokeredFrom []BrokeredFrom `json:"brokeredFrom"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("the patch is not YAML: %v\n%s", err, text)
	}
	if doc.Kind != "Tenant" || doc.Metadata.Name != "customers" || len(doc.Spec.BrokeredFrom) != 2 ||
		doc.Spec.BrokeredFrom[0] != list[1] || doc.Spec.BrokeredFrom[1] != list[0] {
		t.Fatalf("the patch says %+v\n%s", doc, text)
	}
}
