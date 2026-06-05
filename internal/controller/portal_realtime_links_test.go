// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func TestBuildPortalRealtimeLinksScript(t *testing.T) {
	script := buildPortalRealtimeLinksScript(
		"https://meet.demo.desk.gentian.org",
		"https://chat.demo.desk.gentian.org",
	)
	for _, want := range []string{
		"swp.realtime_videoconference",
		"https://meet.demo.desk.gentian.org",
		"swp.realtime_collaboration",
		"https://chat.demo.desk.gentian.org",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
	path := t.TempDir() + "/portal-realtime.sh"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("script must be valid POSIX sh: %v\n%s", err, out)
	}
}

func TestPortalRealtimeLinkTargets(t *testing.T) {
	r := &TenantReconciler{KernelDomain: "desk.gentian.org"}
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{
		{Profile: "jitsi"},
		{Profile: "element"},
	}
	meet, chat := r.portalRealtimeLinkTargets(tenant)
	if meet != "https://meet.demo.desk.gentian.org" {
		t.Fatalf("meet URL: %q", meet)
	}
	if chat != "https://chat.demo.desk.gentian.org" {
		t.Fatalf("chat URL: %q", chat)
	}
}
