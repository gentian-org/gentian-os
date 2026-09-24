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
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A realm policy passes through four places before it reaches Keycloak: the
// Tenant CRD, the projection onto the XTenant, the XRD's structural schema,
// and the Composition. A field missing from any one of them is dropped in
// silence — the console saves, the commit lands, Argo syncs, and the realm
// keeps its old value with nothing anywhere reporting a problem.
//
// This is the same guard the quotas have, for the same reason: that exact
// failure happened to requests.cpu, and it went unnoticed because every layer
// was individually correct.

func securityFieldNames(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for name, typ := range map[string]reflect.Type{
		"password":   reflect.TypeOf(gentianov1alpha1.PasswordPolicy{}),
		"session":    reflect.TypeOf(gentianov1alpha1.SessionPolicy{}),
		"bruteForce": reflect.TypeOf(gentianov1alpha1.BruteForcePolicy{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			tag := typ.Field(i).Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			out[name] = append(out[name], strings.Split(tag, ",")[0])
		}
	}
	return out
}

// TestTheProjectionCarriesEverySecurityField: a field the CRD declares and
// the projection drops can only ever be set by hand-editing the XTenant.
func TestTheProjectionCarriesEverySecurityField(t *testing.T) {
	// Everything on, so nothing is skipped for being a zero value.
	projected := xtenantSecurity(&gentianov1alpha1.TenantSecurity{
		Password: &gentianov1alpha1.PasswordPolicy{
			MinLength: 12, RequireDigits: true, RequireLowercase: true,
			RequireUppercase: true, RequireSpecialChars: true, HistoryCount: 3, MaxAgeDays: 90,
		},
		Session:    &gentianov1alpha1.SessionPolicy{IdleMinutes: 30, MaxHours: 12, RememberMe: true},
		BruteForce: &gentianov1alpha1.BruteForcePolicy{Enabled: true, MaxLoginFailures: 5, LockoutDurationSeconds: 900},
	})

	for block, fields := range securityFieldNames(t) {
		got, ok := projected[block].(map[string]interface{})
		if !ok {
			t.Errorf("the projection drops security.%s entirely", block)
			continue
		}
		for _, field := range fields {
			if _, ok := got[field]; !ok {
				t.Errorf("the CRD declares security.%s.%s but nothing projects it onto "+
					"the XTenant, so it can only ever be set by hand-editing the XR", block, field)
			}
		}
	}
}

// TestTheXRDModelsEverySecurityField: a field absent from the structural
// schema is pruned on the way in, whatever the projection did.
func TestTheXRDModelsEverySecurityField(t *testing.T) {
	raw, err := os.ReadFile("../../crossplane/xrds/tenant.yaml")
	if err != nil {
		t.Skipf("XRD not readable from here: %v", err)
	}
	var xrd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Security struct {
										Properties map[string]struct {
											Properties map[string]interface{} `json:"properties"`
										} `json:"properties"`
									} `json:"security"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &xrd); err != nil {
		t.Fatalf("parse XRD: %v", err)
	}
	if len(xrd.Spec.Versions) == 0 {
		t.Fatal("the XRD declares no versions")
	}
	schema := xrd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Security.Properties

	for block, fields := range securityFieldNames(t) {
		declared, ok := schema[block]
		if !ok {
			t.Errorf("the XRD models no security.%s, so it is pruned before the Composition sees it", block)
			continue
		}
		for _, field := range fields {
			if _, ok := declared.Properties[field]; !ok {
				t.Errorf("the XRD models no security.%s.%s, so it is pruned on the way in", block, field)
			}
		}
	}
}

// TestTheRenderFixtureExercisesEverySecurityField: a field no fixture sets is
// never rendered and never compared, so the Composition's mapping for it is
// untested — which is how a password policy could lose a clause silently.
func TestTheRenderFixtureExercisesEverySecurityField(t *testing.T) {
	raw, err := os.ReadFile("../../crossplane/tests/unit/render/tenant-default/xr.yaml")
	if err != nil {
		t.Skipf("render fixture not readable from here: %v", err)
	}
	var fixture struct {
		Spec struct {
			Security map[string]map[string]interface{} `json:"security"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse render fixture: %v", err)
	}

	for block, fields := range securityFieldNames(t) {
		set, ok := fixture.Spec.Security[block]
		if !ok {
			t.Errorf("the render fixture sets no security.%s", block)
			continue
		}
		for _, field := range fields {
			if _, ok := set[field]; !ok {
				t.Errorf("the render fixture sets no security.%s.%s, so the Composition's "+
					"mapping for it is never rendered and never compared", block, field)
			}
		}
	}
}
