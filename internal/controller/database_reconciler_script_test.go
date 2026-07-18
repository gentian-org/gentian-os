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
