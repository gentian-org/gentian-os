// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"strings"
	"testing"
)

func TestBuildKCLDAPSyncScriptEnsuresEmailMapper(t *testing.T) {
	t.Parallel()
	script := buildKCLDAPSyncScript("demo")
	for _, want := range []string{
		"ensure_ldap_email_attribute_mapper",
		`ensure_ldap_email_attribute_mapper "${REALM}" "ldap"`,
		"mailPrimaryAddress",
		"triggerFullSync",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("LDAP sync script missing %q", want)
		}
	}
}
