// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"strings"
	"testing"
)

func TestBuildRoleScript_CreatesPostgreSQLSchema(t *testing.T) {
	t.Parallel()
	script := buildRoleScript("demo_xwiki", "demo_xwiki")
	for _, want := range []string{
		`CREATE SCHEMA IF NOT EXISTS \"demo_xwiki\" AUTHORIZATION \"demo_xwiki\"`,
		`GRANT ALL ON SCHEMA \"demo_xwiki\" TO \"demo_xwiki\"`,
		`ALTER ROLE \"demo_xwiki\" SET search_path TO \"demo_xwiki\", public`,
		`schema demo_xwiki ensured`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("buildRoleScript() missing %q\nscript:\n%s", want, script)
		}
	}
}
