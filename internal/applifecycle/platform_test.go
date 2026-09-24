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

package applifecycle

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func platformService(t *testing.T, objects ...runtime.Object) *Service {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objects {
		builder = builder.WithRuntimeObjects(o)
	}
	return &Service{client: builder.Build()}
}

func demoTenant() *gentianov1alpha1.Tenant {
	return &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
}

// The interesting row of an integrations screen is the one where a binding
// asks for more than its grant permits: that binding will not do what its
// author expected, and nothing else reports it.
func TestABindingAskingMoreThanItsGrantIsReported(t *testing.T) {
	s := platformService(t,
		demoTenant(),
		&gentianov1alpha1.AppGrant{
			ObjectMeta: metav1.ObjectMeta{Name: "notes", Namespace: "tenant-demo"},
			Spec: gentianov1alpha1.AppGrantSpec{
				App:     "notes",
				Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "files", Granted: []string{"read"}}},
			},
			Status: gentianov1alpha1.AppGrantStatus{Phase: gentianov1alpha1.AppGrantPhaseReady},
		},
		&gentianov1alpha1.IntegrationBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "notes-files", Namespace: "tenant-demo"},
			Spec: gentianov1alpha1.IntegrationBindingSpec{
				Contract:     "files",
				Provider:     gentianov1alpha1.AppEndpoint{App: "drive"},
				Consumer:     gentianov1alpha1.AppEndpoint{App: "notes"},
				Capabilities: []string{"read", "write"},
			},
		},
	)

	out, err := s.Integrations(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary.BindingCount != 1 || out.Summary.GrantCount != 1 || out.Summary.GrantReadyCount != 1 {
		t.Fatalf("summary = %+v", out.Summary)
	}
	if len(out.EffectiveAccess) != 1 {
		t.Fatalf("effective access = %+v", out.EffectiveAccess)
	}
	row := out.EffectiveAccess[0]
	if len(row.Ungranted) != 1 || row.Ungranted[0] != "write" {
		t.Fatalf("a capability asked for and not granted is not reported: %+v", row)
	}
	if row.GrantPhase != "Ready" || row.Provider != "drive" {
		t.Fatalf("row = %+v", row)
	}
}

// A profile asking for something the cluster does not permit is refused at
// deploy time. Seeing which before installing is the screen's whole point.
func TestAWaiverTheClusterDoesNotPermitIsShownAsRefused(t *testing.T) {
	s := platformService(t,
		&gentianov1alpha1.PlatformSecurityPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: gentianov1alpha1.PlatformSecurityPolicySpec{
				AllowedMacWaivers: []gentianov1alpha1.AllowedMacWaiver{
					{Profile: "element", Policy: "gentian-require-non-root", Scope: "synapse"},
				},
			},
		},
		&gentianov1alpha1.AppProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "element"},
			Spec: gentianov1alpha1.AppProfileSpec{
				DisplayName: "Element",
				Security: &gentianov1alpha1.SecuritySpec{MacWaivers: []gentianov1alpha1.MacWaiverRequest{
					{Policy: "gentian-require-non-root", Scope: "synapse"},
					{Policy: "gentian-drop-capabilities", Scope: "synapse"},
				}},
			},
		},
		// A profile that asks for nothing does not appear at all.
		&gentianov1alpha1.AppProfile{ObjectMeta: metav1.ObjectMeta{Name: "quiet"}},
	)

	out, err := s.PlatformSecurity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.AllowedMacWaivers) != 1 {
		t.Fatalf("allowed = %+v", out.AllowedMacWaivers)
	}
	if len(out.CatalogueRequests) != 1 || out.CatalogueRequests[0].Name != "element" {
		t.Fatalf("catalogue = %+v", out.CatalogueRequests)
	}
	entry := out.CatalogueRequests[0]
	if len(entry.Allowed) != 1 || entry.Allowed[0].Policy != "gentian-require-non-root" {
		t.Fatalf("allowed asks = %+v", entry.Allowed)
	}
	if len(entry.Refused) != 1 || entry.Refused[0].Policy != "gentian-drop-capabilities" {
		t.Fatalf("refused asks = %+v", entry.Refused)
	}
}

// L0 is "we changed nothing", so it is not debt. Counting it would make
// every cluster look like it carries as much as it has records.
func TestOnlyRungsAboveL0CountAsCarried(t *testing.T) {
	s := platformService(t,
		&gentianov1alpha1.Customization{
			ObjectMeta: metav1.ObjectMeta{Name: "none", Namespace: "tenant-demo"},
			Spec:       gentianov1alpha1.CustomizationSpec{Summary: "stock", Rung: "L0"},
		},
		&gentianov1alpha1.Customization{
			ObjectMeta: metav1.ObjectMeta{Name: "patched", Namespace: "tenant-demo"},
			Spec:       gentianov1alpha1.CustomizationSpec{Summary: "a patch we carry", Rung: "L3", Owner: "tom"},
			Status:     gentianov1alpha1.CustomizationStatus{ReviewOverdue: true, UpstreamStale: true},
		},
	)

	out, err := s.CustomizationDebtReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.TotalRecords != 2 || out.CarriedDeltas != 1 {
		t.Fatalf("total=%d carried=%d", out.TotalRecords, out.CarriedDeltas)
	}
	if out.ByRung["L0"] != 1 || out.ByRung["L3"] != 1 {
		t.Fatalf("byRung = %v", out.ByRung)
	}
	// The lists are what somebody acts on, and a record can be in both.
	if len(out.ReviewOverdue) != 1 || len(out.UpstreamStale) != 1 {
		t.Fatalf("overdue=%d stale=%d", len(out.ReviewOverdue), len(out.UpstreamStale))
	}
	if out.ReviewOverdue[0].Name != "patched" {
		t.Fatalf("wrong record: %+v", out.ReviewOverdue[0])
	}
}
