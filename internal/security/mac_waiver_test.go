package security_test

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

func TestApprovedMacWaivers_intersection(t *testing.T) {
	requests := []gentianov1alpha1.MacWaiverRequest{
		{Policy: "gentian-require-non-root", Scope: "sidecar-jitsi"},
		{Policy: "other-policy", Scope: "other-scope"},
	}
	allowed := []gentianov1alpha1.AllowedMacWaiver{
		{Profile: "element", Policy: "gentian-require-non-root", Scope: "sidecar-jitsi"},
	}
	got := security.ApprovedMacWaivers("element", requests, allowed)
	if len(got) != 1 {
		t.Fatalf("expected 1 approved waiver, got %d", len(got))
	}
	if got[0].Policy != "gentian-require-non-root" {
		t.Fatalf("unexpected policy %q", got[0].Policy)
	}
}
