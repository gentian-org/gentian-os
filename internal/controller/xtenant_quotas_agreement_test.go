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
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant's quotas cross three files on the way to the cluster: this package
// projects them onto the XTenant, crossplane/xrds/tenant.yaml decides which of
// them survive the write, and crossplane/compositions/tenant-default.yaml turns
// the survivors into ResourceQuota keys. A field missing from the middle one is
// pruned in silence — no event, no condition, and the Tenant still displays the
// quantity nobody is enforcing.
//
// requestsCpu and requestsMemory spent their first release exactly there: in the
// API type, in the Composition, in the Go mirror, and in neither the projection
// below nor the XRD. Tenants moved onto a ResourcePlan got the plan's limits and
// none of its reserved capacity, which is the half the plan is sold on.
//
// These tests are cheap and they are the only thing standing between a new quota
// field and the same silence.

// fullyPopulatedQuotas is a TenantQuotas with every field set, and the reason it
// is written out by hand rather than filled by reflection: adding a field to the
// struct must break a test that a human then has to look at.
func fullyPopulatedQuotas(t *testing.T) *gentianov1alpha1.TenantQuotas {
	t.Helper()
	q := &gentianov1alpha1.TenantQuotas{MaxApps: 10, MaxPods: 100}
	for target, value := range map[**resource.Quantity]string{
		&q.Storage:        "100Gi",
		&q.CPU:            "16",
		&q.Memory:         "16Gi",
		&q.RequestsCPU:    "4",
		&q.RequestsMemory: "8Gi",
	} {
		parsed := resource.MustParse(value)
		*target = &parsed
	}

	// Guard the fixture itself: a field added to TenantQuotas and not set here
	// would sail through both tests below without ever being projected.
	v := reflect.ValueOf(*q)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("TenantQuotas.%s is new and unset in this fixture — set it, "+
				"then make sure xtenantQuotas and the XRD both carry it",
				v.Type().Field(i).Name)
		}
	}
	return q
}

// jsonTagNames returns the wire names of every field of TenantQuotas, which are
// the names the XTenant spec and the XRD schema both have to use.
func jsonTagNames() []string {
	t := reflect.TypeOf(gentianov1alpha1.TenantQuotas{})
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if name := strings.Split(tag, ",")[0]; name != "" && name != "-" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// TestXTenantQuotasProjectsEveryField pins the first hop: a quantity set on the
// Tenant reaches the XTenant. It is the hop requestsCpu and requestsMemory did
// not survive.
func TestXTenantQuotasProjectsEveryField(t *testing.T) {
	projected := xtenantQuotas(fullyPopulatedQuotas(t))

	for _, name := range jsonTagNames() {
		if _, ok := projected[name]; !ok {
			t.Errorf("xtenantQuotas drops %q; the Tenant will keep showing it and "+
				"the XTenant will never receive it", name)
		}
	}

	if got := xtenantQuotas(nil); got != nil {
		t.Errorf("xtenantQuotas(nil) = %v, want nil so buildXTenant omits the key", got)
	}
	if got := xtenantQuotas(&gentianov1alpha1.TenantQuotas{}); len(got) != 0 {
		t.Errorf("xtenantQuotas(empty) = %v, want no keys", got)
	}
}

// TestXTenantQuotasMatchXRDSchema pins the second hop: what this package sends
// is what the XRD accepts. Anything else is pruned on write.
func TestXTenantQuotasMatchXRDSchema(t *testing.T) {
	declared := xrdQuotaProperties(t, "../../crossplane/xrds/tenant.yaml")

	for _, name := range jsonTagNames() {
		if _, ok := declared[name]; !ok {
			t.Errorf("crossplane/xrds/tenant.yaml does not declare quotas.%s; "+
				"the API server prunes it from the XTenant without saying so", name)
		}
	}
	for name := range declared {
		if _, ok := xtenantQuotas(fullyPopulatedQuotas(t))[name]; !ok {
			t.Errorf("the XRD declares quotas.%s but nothing projects it onto the "+
				"XTenant, so it can only ever be set by hand-editing the XR", name)
		}
	}
}

// TestRenderFixtureExercisesTheRealQuotas pins the render fixture to the same
// set. The fixture writes an XR directly, skipping both hops above — which is
// why it rendered requests.cpu correctly the whole time the real path dropped it.
// Holding it to the projection keeps it a test of the Composition rather than a
// test of itself.
func TestRenderFixtureExercisesTheRealQuotas(t *testing.T) {
	raw, err := os.ReadFile("../../crossplane/tests/unit/render/tenant-default/xr.yaml")
	if err != nil {
		t.Skipf("render fixture not readable from here: %v", err)
	}
	var fixture struct {
		Spec struct {
			Quotas map[string]interface{} `json:"quotas"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse render fixture: %v", err)
	}

	for _, name := range jsonTagNames() {
		if _, ok := fixture.Spec.Quotas[name]; !ok {
			t.Errorf("render fixture sets no quotas.%s, so the Composition's "+
				"mapping for it is never rendered and never compared", name)
		}
	}
}

// xrdQuotaProperties reads the quotas property names out of an XRD's structural
// schema.
func xrdQuotaProperties(t *testing.T, path string) map[string]struct{} {
	t.Helper()
	raw, err := os.ReadFile(path)
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
									Quotas struct {
										Properties map[string]interface{} `json:"properties"`
									} `json:"quotas"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &xrd); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(xrd.Spec.Versions) == 0 {
		t.Fatalf("%s declares no versions", path)
	}

	props := xrd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Quotas.Properties
	if len(props) == 0 {
		t.Fatalf("%s declares no quotas properties", path)
	}
	out := make(map[string]struct{}, len(props))
	for name := range props {
		out[name] = struct{}{}
	}
	return out
}
