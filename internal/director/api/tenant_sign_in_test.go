/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

func (h *harness) remoteFile(t *testing.T, tenant, name string) string {
	t.Helper()
	// A file that is not there is the empty answer, not a failure.
	out, err := exec.Command("git", "--git-dir", h.remote, "show",
		"main:clusters/"+dt.Cluster+"/tenants/"+tenant+"/"+name).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// A sign-in between two tenants is one commit of both tenants' line, with
// the positions its people are given; the cluster's administrator writes it,
// and taking it away takes both halves.
func TestASignInBetweenTenantsIsOneCommitOfBothSides(t *testing.T) {
	h := startWith(t, nil)
	alice := h.token(t, "gentian", "alice")
	base := "/v1/clusters/" + dt.Cluster + "/tenants/"

	before := h.tip(t)
	code, body := h.do(t, "PUT", base+"demo/sign-in/solo", alice, `{"positions": ["user"]}`)
	if code != http.StatusAccepted {
		t.Fatalf("sign-in: %d %v", code, body)
	}
	if n := len(strings.Fields(dt.Git(t, "", "--git-dir", h.remote, "rev-list", before+"..main"))); n != 1 {
		t.Fatalf("%d commits, want one", n)
	}
	target, source := h.remoteFile(t, "demo", "sign-in.yaml"), h.remoteFile(t, "solo", "sign-in.yaml")
	if !strings.Contains(target, "- tenant: solo") || !strings.Contains(target, "positions: [user]") {
		t.Fatalf("the target's half:\n%s", target)
	}
	if !strings.Contains(source, "signInTo: [demo]") {
		t.Fatalf("the source's half:\n%s", source)
	}
	code, body = h.do(t, "GET", base+"demo/sign-in", alice, "")
	if from, _ := body["signInFrom"].([]any); code != http.StatusOK || len(from) != 1 {
		t.Fatalf("read back: %d %v", code, body)
	}

	if code, body := h.do(t, "DELETE", base+"demo/sign-in/solo", alice, ""); code != http.StatusAccepted {
		t.Fatalf("remove: %d %v", code, body)
	}
	if h.remoteFile(t, "demo", "sign-in.yaml") != "" || h.remoteFile(t, "solo", "sign-in.yaml") != "" {
		t.Fatal("a half of the sign-in is left after it was taken away")
	}
}

// What a sign-in may not give, and who may not write one.
func TestASignInGivesOnlyPositionsTheTenantHasAndIsTheClustersAdministrators(t *testing.T) {
	h := startWith(t, nil)
	alice := h.token(t, "gentian", "alice")
	base := "/v1/clusters/" + dt.Cluster + "/tenants/"
	before := h.tip(t)
	for name, c := range map[string]struct{ path, body string }{
		"an undeclared position":      {"demo/sign-in/solo", `{"positions": ["website-editor"]}`},
		"the tenant's administrators": {"demo/sign-in/solo", `{"positions": ["tenant-administrator"]}`},
		"the apps' administrators":    {"demo/sign-in/solo", `{"positions": ["app-administrator"]}`},
		"the tenant itself":           {"demo/sign-in/demo", `{"positions": ["user"]}`},
	} {
		if code, out := h.do(t, "PUT", base+c.path, alice, c.body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, out := h.do(t, "PUT", base+"demo/sign-in/solo", h.token(t, "tenant-demo", "tom"), `{"positions": ["user"]}`); code != http.StatusForbidden {
		t.Errorf("the tenant's own administrator: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request committed")
	}
}
