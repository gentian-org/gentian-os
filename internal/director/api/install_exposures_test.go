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

// keepingHeader is a catalog's profile whose web entry keeps the app's own
// Authorization header behind sign-in, and which also asks for a public
// address -- something an install never approves.
func keepingHeader(name string) string {
	return profileNamed(name, "one") +
		"  expose:\n" +
		"    - name: web\n      surface: gateway\n      authMode: oidc\n" + keepsHeader +
		"      backend:\n        service: " + name + "\n        port: 3456\n" +
		"    - name: shares\n      surface: perimeter\n      authMode: none\n      subDomain: share-" + name + "\n      paths: [\"/s/\"]\n" +
		"      backend:\n        service: " + name + "\n        port: 3456\n"
}

func startKeepingHeader(t *testing.T) (*harness, string) {
	t.Helper()
	tasks := keepingHeader("tasks")
	src := catalogues(t, served{"gentian": {"tasks": tasks}})
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath: claimWith(gitops.CatalogueSource{Name: "gentian", URL: src.URL + "/gentian"}),
		})
	}, withFetcher(src))
	return h, sha(tasks)
}

func tasksAsks(digest string) string {
	return "/v1/tenants/demo/catalogues/gentian/entries/tasks/asks?digest=" + digest
}

func askedExposures(out map[string]any) []string {
	var got []string
	raw, _ := out["exposures"].([]any)
	for _, r := range raw {
		e := r.(map[string]any)
		got = append(got, fmt.Sprintf("%v %v %v", e["install"], e["exposure"], e["kind"]))
	}
	return got
}

// An entry that keeps the app's own Authorization header is said before the
// install, with whether the caller may approve it; a public address of the
// same build is not offered, it is approved where it is listed.
func TestAKeptHeaderIsSaidBeforeTheInstall(t *testing.T) {
	h, digest := startKeepingHeader(t)
	code, out := h.do(t, "GET", tasksAsks(digest), h.token(t, "tenant-demo", "tom"), "")
	if code != http.StatusOK {
		t.Fatalf("asks = %d %v", code, out)
	}
	if got := strings.Join(askedExposures(out), "|"); got != "tasks web signInAppAuthorization" {
		t.Fatalf("exposures = %q", got)
	}
	if out["mayExpose"] != true {
		t.Errorf("the tenant's perimeter approver is not told they may approve: %v", out["mayExpose"])
	}
	if _, out := h.do(t, "GET", tasksAsks(digest), h.token(t, "tenant-demo", "mia"), ""); out["mayExpose"] != false {
		t.Errorf("a member is told they may approve: %v", out["mayExpose"])
	}
}

// An install names the entries its caller approves, and each is recorded as
// the perimeter approver's approval is, by that person. Approved, it no
// longer waits.
func TestAnInstallApprovesTheKeptHeaderItNames(t *testing.T) {
	h, digest := startKeepingHeader(t)
	tom := h.token(t, "tenant-demo", "tom")
	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/tasks", tom,
		fmt.Sprintf(`{"coordinate":"gentian/tasks","digest":%q,"expose":[{"install":"tasks","exposure":"web"}]}`, digest))
	if code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	if exposed, _ := out["exposed"].([]any); len(exposed) != 1 {
		t.Fatalf("exposed = %v", out["exposed"])
	}
	registry := registryOf(t, h, "demo")
	for _, want := range []string{"install: tasks", "exposureName: web", "kind: signInAppAuthorization", "owner: tom"} {
		if !strings.Contains(registry, want) {
			t.Fatalf("the approval lacks %q:\n%s", want, registry)
		}
	}
	if strings.Contains(registry, "exposureName: shares") {
		t.Fatalf("an install published a public address:\n%s", registry)
	}
	if trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main"); !strings.Contains(trailer, "can_expose tenant:demo") {
		t.Errorf("the approval's commit does not name the authority:\n%s", trailer)
	}
	if _, out := h.do(t, "GET", tasksAsks(digest), tom, ""); len(askedExposures(out)) != 0 {
		t.Errorf("what was approved still waits: %v", out["exposures"])
	}
}

// Approving with an install is the perimeter approver's, and names an entry
// the build asks to keep its header on. Anything else refuses the install.
func TestAnInstallThatMayNotApproveTheHeaderInstallsNothing(t *testing.T) {
	h, digest := startKeepingHeader(t)
	before := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")) + dt.RemoteFile(t, h.remote, dt.TenantPath("solo"))
	install := func(tenant, token, expose string) (int, map[string]any) {
		return h.do(t, "POST", "/v1/tenants/"+tenant+"/apps/tasks", token,
			fmt.Sprintf(`{"coordinate":"gentian/tasks","digest":%q,"expose":[%s]}`, digest, expose))
	}
	// tina administers solo and is not its perimeter approver.
	if code, out := install("solo", h.token(t, "tenant-solo", "tina"), `{"install":"tasks","exposure":"web"}`); code != http.StatusForbidden {
		t.Errorf("an install by somebody who may not approve: %d %v", code, out)
	}
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := install("demo", tom, `{"install":"tasks","exposure":"shares"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("a public address approved with an install: %d %v", code, out)
	}
	if code, out := install("demo", tom, `{"install":"tasks","exposure":"nothing"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("an entry the build does not declare: %d %v", code, out)
	}
	if code, out := install("demo", tom, `{"install":"Tasks!","exposure":"web"}`); code != http.StatusBadRequest {
		t.Errorf("an install that is not a name: %d %v", code, out)
	}
	if after := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")) + dt.RemoteFile(t, h.remote, dt.TenantPath("solo")); after != before {
		t.Fatal("a refused install changed a tenant")
	}
}
