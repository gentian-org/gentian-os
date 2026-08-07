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

func TestGroupAttributeNamesDerivesOdooMapper(t *testing.T) {
	t.Parallel()
	// What the Odoo profiles actually produce. Before this was derived, the
	// mapper name was hardcoded in the provisioning script.
	groups := `[
		{"name":"demo-members","attributes":{}},
		{"name":"App Users","attributes":{"gentianOdooGroupRoles":["base.group_user"]}},
		{"name":"App Admins","attributes":{"gentianOdooGroupRoles":["base.group_system"]}}
	]`
	got := groupAttributeNames(groups)
	if len(got) != 1 || got[0] != "gentianOdooGroupRoles" {
		t.Fatalf("got %v, want [gentianOdooGroupRoles]", got)
	}
}

func TestGroupAttributeNamesIsSortedAndDeduplicated(t *testing.T) {
	t.Parallel()
	groups := `[
		{"name":"a","attributes":{"zebra":["1"],"alpha":["2"]}},
		{"name":"b","attributes":{"alpha":["3"]}}
	]`
	got := groupAttributeNames(groups)
	if strings.Join(got, ",") != "alpha,zebra" {
		t.Fatalf("got %v, want [alpha zebra] — stable order avoids a churning Job spec", got)
	}
}

func TestGroupAttributeNamesRejectsUnsafeNames(t *testing.T) {
	t.Parallel()
	// The name is interpolated into a JSON body inside a shell script, so anything
	// that is not a plain identifier is dropped rather than escaped.
	groups := `[{"name":"a","attributes":{"ok_name":["1"],"bad\"quote":["2"],"has space":["3"],"$(id)":["4"]}}]`
	got := groupAttributeNames(groups)
	if len(got) != 1 || got[0] != "ok_name" {
		t.Fatalf("got %v, want only [ok_name]", got)
	}
}

func TestGroupAttributeNamesToleratesMalformedJSON(t *testing.T) {
	t.Parallel()
	// The groups themselves are provisioned from this same JSON by the script;
	// failing the whole realm job over the mapper list would be worse than a
	// missing claim.
	if got := groupAttributeNames("not json"); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	if got := groupAttributeNames(""); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}
