// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package oidc

import "testing"

func TestLoadCatalogJitsiPack(t *testing.T) {
	packs, templates, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	pack, ok := packs["opendesk-jitsi"]
	if !ok {
		t.Fatal("expected opendesk-jitsi pack")
	}
	if pack.LDAPGroup != "managed-by-attribute-Videoconference" {
		t.Fatalf("ldapGroup: %q", pack.LDAPGroup)
	}
	if !pack.PublicClient {
		t.Fatal("jitsi client should be public")
	}
	if _, ok := templates["opendesk_useruuid"]; !ok {
		t.Fatal("missing opendesk_useruuid mapper template")
	}
}

func TestPackForClientUnknown(t *testing.T) {
	_, _, ok, err := PackForClient("unknown-client")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected unknown client to have no pack")
	}
}
