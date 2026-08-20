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

	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// TestTenantRoleBindsTheGroupThatIsAssigned pins the two ends of a claim match
// that nothing else checks.
//
// identity_reconciler puts a tenant administrator in keycloak.TenantAdminsGroup;
// the JWT role binds a groups claim. The first version of that role bound
// "/admins" — a value nothing ever assigns — so it could not have matched any
// token, and the failure would have read as a permissions problem.
//
// The leading slash matters too: the tenant realm's groups mapper is created
// with full.path=false, so the claim is the bare name. The kernel realm's uses
// full.path=true, which is why a value copied between realms silently matches
// nothing.
func TestTenantRoleBindsTheGroupThatIsAssigned(t *testing.T) {
	got := keycloak.TenantAdminsGroup("demo")

	if strings.HasPrefix(got, "/") {
		t.Fatalf("tenant realm mapper is full.path=false, so the bound value must "+
			"not be slash-prefixed; got %q", got)
	}
	if want := "gentian:tenant:demo:admins"; got != want {
		t.Fatalf("the role binds %q but the reconciler assigns %q", want, got)
	}
}
