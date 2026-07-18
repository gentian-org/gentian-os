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

package security_test

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

func TestApprovedMacWaivers_intersection(t *testing.T) {
	requests := []gentianov1alpha1.MacWaiverRequest{
		{Policy: "gentian-require-non-root", Scope: "sidecar-meet"},
		{Policy: "other-policy", Scope: "other-scope"},
	}
	allowed := []gentianov1alpha1.AllowedMacWaiver{
		{Profile: "catalogue-test-app", Policy: "gentian-require-non-root", Scope: "sidecar-meet"},
	}
	got := security.ApprovedMacWaivers("catalogue-test-app", requests, allowed)
	if len(got) != 1 {
		t.Fatalf("expected 1 approved waiver, got %d", len(got))
	}
	if got[0].Policy != "gentian-require-non-root" {
		t.Fatalf("unexpected policy %q", got[0].Policy)
	}
}
