package controller

import "testing"

func TestUpsertTunnelIngress(t *testing.T) {
	t.Parallel()
	rules := []cfTunnelIngressRule{
		{Hostname: "portal.example.com", Service: "https://envoy:443"},
		{Service: "http_status:404"},
	}
	got := upsertTunnelIngress(rules, tunnelIngressRuleForService("*.demo.example.com", "https://envoy:443"))
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[1].Hostname != "*.demo.example.com" || got[1].Service != "https://envoy:443" {
		t.Fatalf("inserted rule = %+v", got[1])
	}
	if got[1].OriginRequest == nil || !got[1].OriginRequest.MatchSNItoHost || !got[1].OriginRequest.NoTLSVerify {
		t.Fatalf("expected matchSNItoHost and noTLSVerify on HTTPS rule, got %+v", got[1].OriginRequest)
	}
	if got[2].Service != "http_status:404" {
		t.Fatalf("catch-all moved: %+v", got[2])
	}

	got = upsertTunnelIngress(got, tunnelIngressRuleForService("*.demo.example.com", "https://envoy-new:443"))
	if len(got) != 3 {
		t.Fatalf("update len = %d", len(got))
	}
	if got[1].Service != "https://envoy-new:443" {
		t.Fatalf("updated service = %q", got[1].Service)
	}
}

func TestTunnelIngressRuleForService(t *testing.T) {
	t.Parallel()
	httpRule := tunnelIngressRuleForService("app.example.com", "http://svc:8080")
	if httpRule.OriginRequest != nil {
		t.Fatalf("HTTP origin should not set originRequest: %+v", httpRule.OriginRequest)
	}
	httpsRule := tunnelIngressRuleForService("*.app.example.com", "https://envoy.svc:443")
	if httpsRule.OriginRequest == nil || !httpsRule.OriginRequest.MatchSNItoHost || !httpsRule.OriginRequest.NoTLSVerify {
		t.Fatalf("HTTPS origin should set matchSNItoHost and noTLSVerify: %+v", httpsRule.OriginRequest)
	}
}

func TestTunnelIngressRulesEqual(t *testing.T) {
	t.Parallel()
	base := tunnelIngressRuleForService("*.demo.example.com", "https://envoy:443")
	if !tunnelIngressRulesEqual(base, base) {
		t.Fatal("identical rules should be equal")
	}
	stale := cfTunnelIngressRule{Hostname: "*.demo.example.com", Service: "https://envoy:443"}
	if tunnelIngressRulesEqual(base, stale) {
		t.Fatal("missing originRequest should not equal desired rule")
	}
}

func TestParseTunnelID(t *testing.T) {
	t.Parallel()
	if got := parseTunnelID("e053a404-0dc1-4b56-bc8c-e0033dd4a016.cfargotunnel.com"); got != "e053a404-0dc1-4b56-bc8c-e0033dd4a016" {
		t.Fatalf("got %q", got)
	}
}
