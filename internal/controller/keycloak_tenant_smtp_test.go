/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestTenantSMTPJobName(t *testing.T) {
	got := tenantSMTPJobName("demo")
	want := "keycloak-tenant-smtp-demo"
	if got != want {
		t.Fatalf("tenantSMTPJobName = %q, want %q", got, want)
	}
}

// The gate reads smtp_configure, not the Secret's existence.
//
// The Secret is built by an ExternalSecret that renders the cluster's mail
// settings whether or not the relay credential has been supplied, so it exists
// on every external-mode cluster from the moment the kernel syncs. Gating on
// presence would send every tenant into a configure Job that exits 1 on its own
// SMTP_CONFIGURE check — and, before the ExternalSecret existed, gating on
// presence is what let a cluster with no credential skip the step in silence.
func TestSMTPCredentialsUsable(t *testing.T) {
	b := func(m map[string]string) map[string][]byte {
		out := map[string][]byte{}
		for k, v := range m {
			out[k] = []byte(v)
		}
		return out
	}

	cases := []struct {
		name string
		data map[string][]byte
		want bool
	}{
		{"nothing synced yet", nil, false},
		{"credential supplied", b(map[string]string{"smtp_configure": "true", "smtp_user": "relay@example.org"}), true},
		{"settings rendered, credential not supplied", b(map[string]string{"smtp_configure": "false", "smtp_host": "smtp.example.org"}), false},
		{"field absent", b(map[string]string{"smtp_host": "smtp.example.org"}), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := smtpCredentialsUsable(tc.data); got != tc.want {
				t.Fatalf("smtpCredentialsUsable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildTenantSMTPConfigureScriptRealmPlaceholderCount(t *testing.T) {
	script := buildTenantSMTPConfigureScript(`"demo"`)
	for _, want := range []string{`REALM="demo"`, "SMTP_HOST", "smtpServer = $smtp"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
}

// runTenantSMTPScript runs the Job's script against a stand-in curl that plays
// Keycloak, and returns the smtpServer block the script wrote to the realm.
func runTenantSMTPScript(t *testing.T, env map[string]string) map[string]any {
	t.Helper()
	for _, tool := range []string{"sh", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	put := filepath.Join(dir, "put.json")
	// The token, the realm as Keycloak returns it (a stored password comes
	// back as its mask), and the write, kept for the assertions.
	stub := `#!/bin/sh
body=""; method="GET"; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -X) method="$2"; shift ;;
    -d) body="$2"; shift ;;
    -H|--max-time) shift ;;
    http*) url="$1" ;;
  esac
  shift
done
case "${method} ${url}" in
  "POST "*"/protocol/openid-connect/token") printf '{"access_token":"t"}' ;;
  "GET "*"/admin/realms/demo") printf '{"realm":"demo","smtpServer":{"host":"old.example.org","auth":"true","user":"old","password":"**********"}}' ;;
  "PUT "*"/admin/realms/demo") printf '%s' "${body}" > "` + put + `" ;;
  *) echo "unexpected ${method} ${url}" >&2; exit 22 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", buildTenantSMTPConfigureScript(`"demo"`))
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"KEYCLOAK_URL=http://keycloak.example.org", "KEYCLOAK_ADMIN_USERNAME=admin", "KEYCLOAK_ADMIN_PASSWORD=x",
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "relay-secret") || strings.Contains(string(out), "tenant-secret") {
		t.Fatalf("the script printed a password:\n%s", out)
	}
	raw, err := os.ReadFile(put)
	if err != nil {
		t.Fatalf("the script wrote no realm: %v\n%s", err, out)
	}
	var realm struct {
		SMTPServer map[string]any `json:"smtpServer"`
	}
	if err := json.Unmarshal(raw, &realm); err != nil {
		t.Fatalf("realm written is not JSON: %v\n%s", err, raw)
	}
	return realm.SMTPServer
}

// What keycloak-smtp-credentials holds differs by mode: the relay's login with
// a relay, the kernel realm's own login where the cluster runs its own mail.
// A tenant realm takes the first and never the second -- its own login is the
// one minted for it, in mail-submission-<tenant>.
//
// The script compared the mode against "kernel", its name before, so "system"
// fell through to the relay branch and a tenant realm was given the kernel
// realm's login. The password is asserted too: Keycloak stores a realm written
// with authentication on and no password, answers 204, and fails every
// invitation with "no password specified".
func TestTenantSMTPScriptWritesTheRealmsOwnLogin(t *testing.T) {
	shared := map[string]string{
		"SMTP_CONFIGURE": "true", "SMTP_PORT": "587", "SMTP_SSL": "false", "SMTP_STARTTLS": "true",
		"SMTP_FROM": "noreply@example.org", "SMTP_FROM_NAME": "Example Cloud",
		"KERNEL_MAIL_HOST": "mail.example.org",
	}
	with := func(extra map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range shared {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	ownMail := map[string]string{
		"SMTP_HOST": "mail.example.org", "SMTP_USER": "gentian-system@example.org", "SMTP_PASSWORD": "kernel-secret",
		"KERNEL_SMTP_USER": "noreply@demo.example.org", "KERNEL_SMTP_PASSWORD": "tenant-secret",
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want map[string]any
	}{
		{"the cluster's own mail", with(mergeEnv(ownMail, "MAIL_SERVICE_MODE", "system")), map[string]any{
			"host": "mail.example.org", "port": "587", "auth": "true", "starttls": "true", "ssl": "false",
			"user": "noreply@demo.example.org", "password": "tenant-secret",
			"from": "noreply@example.org", "fromDisplayName": "Example Cloud",
		}},
		{"the same, in a Secret that still names the mode as it was", with(mergeEnv(ownMail, "MAIL_SERVICE_MODE", "kernel")), map[string]any{
			"host": "mail.example.org", "port": "587", "auth": "true", "starttls": "true", "ssl": "false",
			"user": "noreply@demo.example.org", "password": "tenant-secret",
			"from": "noreply@example.org", "fromDisplayName": "Example Cloud",
		}},
		{"a relay", with(map[string]string{
			"MAIL_SERVICE_MODE": "external",
			"SMTP_HOST":         "relay.example.org", "SMTP_USER": "relay-user", "SMTP_PASSWORD": "relay-secret",
		}), map[string]any{
			"host": "relay.example.org", "port": "587", "auth": "true", "starttls": "true", "ssl": "false",
			"user": "relay-user", "password": "relay-secret",
			"from": "noreply@example.org", "fromDisplayName": "Example Cloud",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runTenantSMTPScript(t, tc.env)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("smtpServer written:\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

func mergeEnv(base map[string]string, k, v string) map[string]string {
	out := map[string]string{k: v}
	for bk, bv := range base {
		out[bk] = bv
	}
	return out
}

// The label is what makes the operator run the Job again over realms already
// written. A script change that must reach them and leaves the version alone
// reaches new tenants only.
func TestTenantSMTPJobCarriesTheScriptVersion(t *testing.T) {
	job := makeTenantSMTPJob("demo", "demo", "mail.example.org", "Example Cloud")
	if got := job.Labels["gentianos.io/keycloak-tenant-smtp"]; got != "6" {
		t.Fatalf("version label = %q, want 6: the run that puts a relay's password back", got)
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if strings.Contains(e.Name, "PASSWORD") && e.Value != "" {
				t.Fatalf("%s carries a literal value in the pod spec", e.Name)
			}
		}
	}
}

// TestTenantSMTPScriptAgainstKeycloak runs the Job's script, as built, against
// a Keycloak somebody started: `make test-realm-smtp-lab` starts one of the
// chart's version beside a mail server that demands a login, passes the
// environment the Job's pod would have, and then asks Keycloak to send.
func TestTenantSMTPScriptAgainstKeycloak(t *testing.T) {
	realm := os.Getenv("REALM_SMTP_LAB_REALM")
	if realm == "" {
		t.Skip("run by make test-realm-smtp-lab")
	}
	cmd := exec.Command("sh", "-c", buildTenantSMTPConfigureScript(strconv.Quote(realm)))
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
}
