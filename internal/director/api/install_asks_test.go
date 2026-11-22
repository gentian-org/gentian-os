/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// askingEgress is a catalog's profile that asks for a way out by host.
func askingEgress(name, request, host string) string {
	return profileNamed(name, "one") + "  requires:\n    privileges:\n      egress:\n        - name: " + request +
		"\n          reason: reaches the service it connects\n          hosts: [" + host + "]\n"
}

// startAsking is a director whose catalog offers a recorder that asks for a
// way out and comes with a relay that asks for one too.
func startAsking(t *testing.T, seed ...func(remote string)) (*harness, string) {
	t.Helper()
	recorder := askingEgress("recorder", "recordings", "api.recorder.example") + "  comesWith: [relay]\n"
	src := catalogues(t, served{"gentian": {"recorder": recorder, "relay": askingEgress("relay", "upstream", "relay.example")}})
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath: claimWith(gitops.CatalogueSource{Name: "gentian", URL: src.URL + "/gentian"}),
		})
		for _, more := range seed {
			more(remote)
		}
	}, withFetcher(src))
	return h, sha(recorder)
}

func asksPath(digest string) string {
	return "/v1/tenants/demo/catalogues/gentian/entries/recorder/asks?digest=" + digest
}

func waitingAsks(t *testing.T, out map[string]any) []string {
	t.Helper()
	var got []string
	for _, raw := range out["egress"].([]any) {
		e := raw.(map[string]any)
		got = append(got, fmt.Sprintf("%v %v %v", e["install"], e["privilege"], e["hosts"]))
	}
	return got
}

// What a build asks for is said before it is installed: its own requests and
// those of the apps it comes with, and whether the caller may approve them.
// Reading it writes nothing.
func TestWhatABuildAsksForIsSaidBeforeItIsInstalled(t *testing.T) {
	h, digest := startAsking(t)
	before := h.tip(t)
	code, out := h.do(t, "GET", asksPath(digest), h.token(t, "tenant-demo", "tom"), "")
	if code != http.StatusOK {
		t.Fatalf("asks = %d %v", code, out)
	}
	want := "recorder egress/recordings [api.recorder.example]|relay egress/upstream [relay.example]"
	if got := strings.Join(waitingAsks(t, out), "|"); got != want {
		t.Fatalf("asks:\n%s\nwant:\n%s", got, want)
	}
	if out["mayApprove"] != true {
		t.Errorf("the tenant's perimeter approver is not told they may approve: %v", out["mayApprove"])
	}
	if with := out["egress"].([]any)[1].(map[string]any)["cameWith"]; with != "recorder" {
		t.Errorf("the relay's request does not say what it comes with: %v", with)
	}
	if code, out := h.do(t, "GET", asksPath(digest), h.token(t, "tenant-demo", "mia"), ""); code != http.StatusOK || out["mayApprove"] != false {
		t.Errorf("a member: %d, mayApprove %v", code, out["mayApprove"])
	}
	if h.tip(t) != before {
		t.Fatal("reading what a build asks for moved the repository")
	}
	if code, _ := h.do(t, "GET", asksPath("sha256:"+strings.Repeat("0", 64)), h.token(t, "tenant-demo", "tom"), ""); code < 400 {
		t.Errorf("a digest the source does not serve answered %d", code)
	}
}

// An install names the requests its caller approves, and each is recorded
// as that person's grant, by the authority an approval afterwards has. What
// was approved no longer waits.
func TestAnInstallApprovesTheRequestsItNames(t *testing.T) {
	h, digest := startAsking(t)
	tom := h.token(t, "tenant-demo", "tom")
	body := fmt.Sprintf(`{"coordinate":"gentian/recorder","digest":%q,"approve":[`+
		`{"install":"recorder","privilege":"egress/recordings"},{"install":"relay","privilege":"egress/upstream"}]}`, digest)
	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/recorder", tom, body)
	if code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	if approved, _ := out["approved"].([]any); len(approved) != 2 {
		t.Fatalf("approved = %v", out["approved"])
	}
	if entry := appEntryOf(t, h, "demo", "relay"); !strings.Contains(entry, "cameWith: recorder") {
		t.Fatalf("the relay did not come with the app:\n%q", entry)
	}
	_, list := h.do(t, "GET", "/v1/tenants/demo/privileges", tom, "")
	grants, _ := list["privileges"].([]any)
	if len(grants) != 2 {
		t.Fatalf("grants = %v", list["privileges"])
	}
	for _, raw := range grants {
		if g := raw.(map[string]any); g["approver"] != "tom" {
			t.Errorf("a grant's approver is %v, want the caller", g["approver"])
		}
	}
	if trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main"); !strings.Contains(trailer, "can_approve_privilege tenant:demo") {
		t.Errorf("the grant's commit does not name the authority:\n%s", trailer)
	}
	if _, out := h.do(t, "GET", asksPath(digest), tom, ""); len(waitingAsks(t, out)) != 0 {
		t.Errorf("what was approved still waits: %v", out["egress"])
	}
}

// Approving with an install is the perimeter approver's, and names what the
// build asks for. Anything else refuses the install: no app is installed
// and nothing is granted.
func TestAnInstallThatMayNotApproveInstallsNothing(t *testing.T) {
	h, digest := startAsking(t)
	before := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")) + dt.RemoteFile(t, h.remote, dt.TenantPath("solo"))
	install := func(tenant, token, approve string) (int, map[string]any) {
		return h.do(t, "POST", "/v1/tenants/"+tenant+"/apps/recorder", token,
			fmt.Sprintf(`{"coordinate":"gentian/recorder","digest":%q,"approve":[%s]}`, digest, approve))
	}
	own := `{"install":"recorder","privilege":"egress/recordings"}`
	// tina administers solo and is not its perimeter approver.
	tina := h.token(t, "tenant-solo", "tina")
	if code, out := install("solo", tina, own); code != http.StatusForbidden {
		t.Errorf("an install by somebody who may not approve: %d %v", code, out)
	}
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := install("demo", tom, `{"install":"recorder","privilege":"egress/something-else"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("an approval of a request the build does not state: %d %v", code, out)
	}
	if code, out := install("demo", tom, `{"install":"nextcloud","privilege":"egress/recordings"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("an approval for another app: %d %v", code, out)
	}
	if code, out := install("demo", tom, `{"install":"recorder","privilege":"podSecurity/run-as-root"}`); code != http.StatusBadRequest {
		t.Errorf("an approval of a pod-security waiver with an install: %d %v", code, out)
	}
	if after := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")) + dt.RemoteFile(t, h.remote, dt.TenantPath("solo")); after != before {
		t.Fatal("a refused install changed a tenant")
	}
	if _, list := h.do(t, "GET", "/v1/tenants/demo/privileges", tom, ""); len(list["privileges"].([]any)) != 0 {
		t.Fatalf("a refused install granted: %v", list["privileges"])
	}
	// Without approve the same person installs, and the requests wait.
	if code, out := h.do(t, "POST", "/v1/tenants/solo/apps/recorder", tina,
		fmt.Sprintf(`{"coordinate":"gentian/recorder","digest":%q}`, digest)); code != http.StatusAccepted {
		t.Fatalf("install without approve = %d %v", code, out)
	}
	code, list := h.do(t, "GET", "/v1/tenants/solo/privileges", tina, "")
	if granted, _ := list["privileges"].([]any); code != http.StatusOK || len(granted) != 0 {
		t.Errorf("an install without approve granted: %d %v", code, list)
	}
}

// A request the cluster's policy answers waits for nobody, and is not
// offered for approval: hosts in a domain it trusts.
func TestWhatPolicyAnswersIsNotOfferedForApproval(t *testing.T) {
	policy := func(spec string) func(string) {
		return func(remote string) {
			dt.Commit(t, remote, map[string]string{
				"clusters/" + dt.Cluster + "/kernel/claims/" + gitops.PlatformSecurityFile: "apiVersion: gentianos.io/v1alpha1\n" +
					"kind: PlatformSecurityPolicy\nmetadata:\n  name: default\nspec:\n" + spec,
			})
		}
	}
	h, digest := startAsking(t, policy("  trustedDomains: [recorder.example]\n"))
	_, out := h.do(t, "GET", asksPath(digest), h.token(t, "tenant-demo", "tom"), "")
	if got := strings.Join(waitingAsks(t, out), "|"); got != "relay egress/upstream [relay.example]" {
		t.Errorf("with one trusted domain: %s", got)
	}
}
