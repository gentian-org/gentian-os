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
		"demo",
		"ou=demo,${UDM_LDAP_BASE}",
		"https://meet.demo.desk.gentian.org",
		"https://chat.demo.desk.gentian.org",
		false,
	)
	for _, want := range []string{
		"swp.realtime_videoconference_demo",
		"https://meet.demo.desk.gentian.org",
		"swp.realtime_collaboration_demo",
		"https://chat.demo.desk.gentian.org",
		"cn=users_demo,${OU_POS}",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
	if strings.Contains(script, `ensure_realtime_entry "swp.realtime_videoconference"`) {
		t.Fatal("multi mode must not create legacy swp.realtime_videoconference entry")
	}
	path := t.TempDir() + "/portal-realtime.sh"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("script must be valid POSIX sh: %v\n%s", err, out)
	}
}

func TestBuildPortalRealtimeLinksScriptSingleTenancyLegacy(t *testing.T) {
	script := buildPortalRealtimeLinksScript(
		"default",
		"ou=default,${UDM_LDAP_BASE}",
		"https://meet.desk.gentian.org",
		"https://chat.desk.gentian.org",
		true,
	)
	for _, want := range []string{
		"swp.realtime_videoconference_default",
		"swp.realtime_videoconference",
		"https://meet.desk.gentian.org",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
}

func TestPortalRealtimeLinkTargetsMulti(t *testing.T) {
	r := &TenantReconciler{KernelDomain: "desk.gentian.org", TenancyMode: gentianov1alpha1.TenancyModeMulti}
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

func TestPortalRealtimeLinkTargetsSingle(t *testing.T) {
	r := &TenantReconciler{KernelDomain: "desk.gentian.org", TenancyMode: gentianov1alpha1.TenancyModeSingle}
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "default"
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "jitsi"}}
	meet, _ := r.portalRealtimeLinkTargets(tenant)
	if meet != "https://meet.desk.gentian.org" {
		t.Fatalf("meet URL: %q", meet)
	}
}
