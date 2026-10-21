/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"reflect"
	"testing"
)

func TestTheClaimsRoleFieldsBecomeTheModelsRelations(t *testing.T) {
	roles, unknown := RolesFromClaim(map[string]string{
		"admin":           "gentian:platform:admin",
		"securityOfficer": "gentian:platform:security",
		"serviceAdmin":    "gentian:platform:service",
		"sharedAppsAdmin": "gentian:platform:shared-apps",
		"breakGlass":      "gentian:platform:break-glass",
		"auditor":         "", // set to nothing grants nothing
	})
	want := map[string]string{
		"admin":             "gentian:platform:admin",
		"security_officer":  "gentian:platform:security",
		"service_admin":     "gentian:platform:service",
		"shared_apps_admin": "gentian:platform:shared-apps",
		"break_glass":       "gentian:platform:break-glass",
	}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %v, want none", unknown)
	}

	// Every translated field is a relation ReconcileClusterRoles accepts, and
	// every relation it accepts has a field: the two lists cannot drift apart.
	for field, rel := range ClaimRoles {
		if !knownClusterRole(rel) {
			t.Errorf("ClaimRoles[%q] = %q, which is not a cluster relation", field, rel)
		}
	}
	for _, rel := range ClusterRoles {
		found := false
		for _, r := range ClaimRoles {
			found = found || r == rel
		}
		if !found {
			t.Errorf("relation %q has no field in the claim", rel)
		}
	}
}

func TestAFieldTheClaimShouldNotHaveIsReportedNotTranslated(t *testing.T) {
	roles, unknown := RolesFromClaim(map[string]string{
		"securityOfficer":  "gentian:platform:security",
		"security_officer": "gentian:platform:security", // the relation's spelling is not a field
		"auditer":          "gentian:platform:auditor",  // a typo
	})
	if want := map[string]string{"security_officer": "gentian:platform:security"}; !reflect.DeepEqual(roles, want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	if want := []string{"auditer", "security_officer"}; !reflect.DeepEqual(unknown, want) {
		t.Errorf("unknown = %v, want %v", unknown, want)
	}
}
