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
