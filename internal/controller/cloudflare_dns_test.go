package controller

import "testing"

func TestUpsertTunnelIngress(t *testing.T) {
	t.Parallel()
	rules := []cfTunnelIngressRule{
		{Hostname: "portal.example.com", Service: "https://envoy:443"},
		{Service: "http_status:404"},
	}
	got := upsertTunnelIngress(rules, "*.demo.example.com", "https://envoy:443")
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[1].Hostname != "*.demo.example.com" || got[1].Service != "https://envoy:443" {
		t.Fatalf("inserted rule = %+v", got[1])
	}
	if got[2].Service != "http_status:404" {
		t.Fatalf("catch-all moved: %+v", got[2])
	}

	got = upsertTunnelIngress(got, "*.demo.example.com", "https://envoy-new:443")
	if len(got) != 3 {
		t.Fatalf("update len = %d", len(got))
	}
	if got[1].Service != "https://envoy-new:443" {
		t.Fatalf("updated service = %q", got[1].Service)
	}
}

func TestParseTunnelID(t *testing.T) {
	t.Parallel()
	if got := parseTunnelID("e053a404-0dc1-4b56-bc8c-e0033dd4a016.cfargotunnel.com"); got != "e053a404-0dc1-4b56-bc8c-e0033dd4a016" {
		t.Fatalf("got %q", got)
	}
}
