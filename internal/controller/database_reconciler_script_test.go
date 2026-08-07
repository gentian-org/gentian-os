/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestBuildRoleScript_CreatesPostgreSQLSchema(t *testing.T) {
	t.Parallel()
	script := buildRoleScript("demo_xwiki", "demo_xwiki", gentianov1alpha1.SchemaPreferenceAppSchema)
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

func TestBuildRoleScript_SchemaPreference(t *testing.T) {
	t.Parallel()
	// Default: the app's own schema resolves first.
	appFirst := buildRoleScript("demo_ap", "demo_ap", gentianov1alpha1.SchemaPreferenceAppSchema)
	if !strings.Contains(appFirst, `search_path TO \"demo_ap\", public`) {
		t.Fatalf("app-schema preference did not put the app schema first:\n%s", appFirst)
	}

	// Opting into public first is what Activepieces needs: its migrations create
	// tables in public and then look them up unqualified.
	publicFirst := buildRoleScript("demo_ap", "demo_ap", gentianov1alpha1.SchemaPreferencePublic)
	if !strings.Contains(publicFirst, `search_path TO public, \"demo_ap\"`) {
		t.Fatalf("public preference did not put public first:\n%s", publicFirst)
	}

	// An unset preference must behave exactly like the previous default, or every
	// existing app silently changes search_path on the next reconcile.
	unset := buildRoleScript("demo_ap", "demo_ap", "")
	if unset != appFirst {
		t.Fatal("unset preference must be identical to app-schema")
	}
}
