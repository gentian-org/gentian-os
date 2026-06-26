// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"strings"
	"testing"
)

func TestBuildRealmScriptEnsuresOpenDeskLDAPMappers(t *testing.T) {
	t.Parallel()
	script := buildRealmScript("demo", "Demo")
	for _, want := range []string{
		"ensure_ldap_oxcontext_attribute_mapper",
		`ensure_ldap_oxcontext_attribute_mapper "demo" "ldap"`,
		"ensure_ldap_entryuuid_attribute_mapper",
		`ensure_ldap_entryuuid_attribute_mapper "demo" "ldap"`,
		"oxContextIDNum",
		"entryUUID",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("realm script missing %q", want)
		}
	}
}

func TestBuildKCLDAPSyncScriptEnsuresOpenDeskLDAPMappers(t *testing.T) {
	t.Parallel()
	script := buildKCLDAPSyncScript("demo")
	for _, want := range []string{
		"ensure_ldap_oxcontext_attribute_mapper",
		`ensure_ldap_oxcontext_attribute_mapper "${REALM}" "ldap"`,
		"ensure_ldap_entryuuid_attribute_mapper",
		`ensure_ldap_entryuuid_attribute_mapper "${REALM}" "ldap"`,
		"oxContextIDNum",
		"triggerFullSync",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("LDAP sync script missing %q", want)
		}
	}
}
