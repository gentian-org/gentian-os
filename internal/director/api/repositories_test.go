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
)

const (
	demoRepositories    = "/v1/tenants/demo/repositories/"
	clusterRepositories = "/v1/clusters/" + dt.Cluster + "/repositories/"
	demoDir             = "clusters/" + dt.Cluster + "/tenants/demo/"
	claimsDir           = "clusters/" + dt.Cluster + "/kernel/claims/"
)

// Where a tenant installs software from is a commit with the person's name on
// it: a Repository beside the tenant's manifest, listed in the kustomization
// Argo CD syncs, owned by the tenant the route named and pointing its
// credential inside that tenant's part of the vault.
func TestATenantsRepositoryIsACommitByThePersonWhoDeclaredIt(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	code, body := h.do(t, "PUT", demoRepositories+"team-apps", tom,
		`{"role":"apps","type":"oci","url":"oci://registry.example.com/demo/charts"}`)
	if code != http.StatusAccepted {
		t.Fatalf("declare: %d %v", code, body)
	}
	if body["created"] != true || body["tenant"] != "demo" || body["credentialName"] != "repository-team-apps" || body["commit"] == "" {
		t.Fatalf("answer = %v", body)
	}
	file := dt.RemoteFile(t, h.remote, demoDir+"repository-team-apps.yaml")
	for _, want := range []string{
		"kind: Repository", "name: team-apps", "role: apps", "type: oci",
		"inCluster: oci://registry.example.com/demo/charts",
		"tenant: demo",
		"vaultPath: gentian-os/tenants/demo/repositories/team-apps",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("the declaration is missing %q:\n%s", want, file)
		}
	}
	if kustomization := dt.RemoteFile(t, h.remote, demoDir+"kustomization.yaml"); !strings.Contains(kustomization, "- repository-team-apps.yaml") {
		t.Fatalf("Argo CD would not apply it; the kustomization does not list it:\n%s", kustomization)
	}
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an|%(trailers:key=Gentian-Authz,valueonly)", "main")
	if !strings.Contains(trailer, "Tom|") || !strings.Contains(trailer, "user:tom can_write_credential tenant:demo allowed") {
		t.Fatalf("commit = %q", trailer)
	}

	// Said again, nothing is committed.
	before := h.tip(t)
	code, body = h.do(t, "PUT", demoRepositories+"team-apps", tom,
		`{"role":"apps","type":"oci","url":"oci://registry.example.com/demo/charts"}`)
	if code != http.StatusOK || body["created"] != false || h.tip(t) != before {
		t.Fatalf("an unchanged declaration: %d %v", code, body)
	}
}

// A declaration carries an address and nothing secret. A body that tries to
// carry more is refused whole, and the owner is the route's to name.
func TestADeclarationCarriesNoSecretAndCannotNameItsOwner(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	for _, body := range []string{
		`{"role":"apps","type":"oci","url":"oci://git.example.com/a","password":"hunter2"}`,
		`{"role":"apps","type":"oci","url":"oci://git.example.com/a","tenant":"solo"}`,
		`{"role":"apps","type":"oci","url":"oci://git.example.com/a","credential":{"vaultPath":"gentian-os/kernel/repositories/deployments"}}`,
		`{"role":"apps","type":"svn","url":"https://git.example.com/a.git"}`,
		`{"role":"gentian-os","type":"git","url":"https://git.example.com/a.git"}`,
		`{"role":"deployments","type":"oci","url":"oci://registry.example.com/a","confirm":"a"}`,
		`{"role":"apps","type":"oci","url":" oci://registry.example.com/a"}`,
		`{"role":"apps","type":"oci","url":""}`,
	} {
		if code, _ := h.do(t, "PUT", demoRepositories+"a", tom, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	if code, _ := h.do(t, "PUT", demoRepositories+"Not_A_Name", tom, `{"role":"apps","type":"oci","url":"oci://x.example/a"}`); code != http.StatusBadRequest {
		t.Errorf("a name that is not a DNS label: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused declaration moved the repository")
	}
}

// The same people who could before, and nobody else: a tenant's
// administrators for the tenant's, the cluster's for the cluster's, and a
// platform administrator inside a tenant only where the tenant lets the
// platform operate it.
func TestWhoMayDeclareARepositoryIsTheStoresAnswer(t *testing.T) {
	h := start(t)
	body := `{"role":"apps","type":"oci","url":"oci://git.example.com/a"}`
	before := h.tip(t)

	for who, token := range map[string]string{
		"a member":                              h.token(t, "tenant-demo", "mia"),
		"another tenant's administrator":        h.token(t, "tenant-solo", "tina"),
		"an auditor":                            h.token(t, "gentian", "audrey"),
		"the security officer":                  h.token(t, "gentian", "sam"),
		"somebody who operates system services": h.token(t, "gentian", "serge"),
	} {
		if code, _ := h.do(t, "PUT", demoRepositories+"a", token, body); code != http.StatusForbidden {
			t.Errorf("%s declared a repository for tenant demo: %d", who, code)
		}
		if code, _ := h.do(t, "DELETE", demoRepositories+"a?confirm=a", token, ""); code != http.StatusForbidden {
			t.Errorf("%s removed a repository of tenant demo: %d", who, code)
		}
	}
	// The cluster's are not a tenant administrator's.
	for who, token := range map[string]string{
		"a tenant's administrator": h.token(t, "tenant-demo", "tom"),
		"an auditor":               h.token(t, "gentian", "audrey"),
	} {
		if code, _ := h.do(t, "PUT", clusterRepositories+"a", token, body); code != http.StatusForbidden {
			t.Errorf("%s declared a repository for the cluster: %d", who, code)
		}
		if code, _ := h.do(t, "DELETE", clusterRepositories+"a?confirm=a", token, ""); code != http.StatusForbidden {
			t.Errorf("%s removed a repository of the cluster: %d", who, code)
		}
	}
	// A tenant that administers itself is closed to the platform's
	// administrators, here as everywhere.
	alice := h.token(t, "gentian", "alice")
	if code, _ := h.do(t, "PUT", "/v1/tenants/solo/repositories/a", alice, body); code != http.StatusForbidden {
		t.Errorf("a platform administrator declared a repository in a tenant that administers itself: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}

	// And where they may: the platform's administrator, for the cluster and
	// for a tenant it operates.
	if code, body := h.do(t, "PUT", clusterRepositories+"mirror", alice, body); code != http.StatusAccepted || body["tenant"] != "" {
		t.Fatalf("the cluster's administrator: %d %v", code, body)
	}
	file := dt.RemoteFile(t, h.remote, claimsDir+"mirror-repository.yaml")
	if !strings.Contains(file, "vaultPath: gentian-os/kernel/repositories/mirror") || strings.Contains(file, "tenant:") {
		t.Fatalf("the cluster's repository:\n%s", file)
	}
	if code, body := h.do(t, "PUT", demoRepositories+"ops", alice, body); code != http.StatusAccepted || body["tenant"] != "demo" {
		t.Fatalf("a platform administrator in a tenant the platform operates: %d %v", code, body)
	}
}

// Anything that is not purely additive is refused until the name is repeated:
// a deployments repository even when new, a change of address, and every
// removal.
func TestWhatIsNotAdditiveNeedsTheNameRepeated(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	// A deployments repository, even a new one.
	code, body := h.do(t, "PUT", demoRepositories+"source", tom, `{"role":"deployments","type":"git","url":"https://git.example.com/d.git"}`)
	if code != http.StatusPreconditionRequired || body["confirmWith"] != "source" || body["confirmField"] != "confirm" || body["dangerous"] != true {
		t.Fatalf("a deployments repository without confirmation: %d %v", code, body)
	}
	if code, _ := h.do(t, "PUT", demoRepositories+"source", tom, `{"role":"deployments","type":"git","url":"https://git.example.com/d.git","confirm":"sauce"}`); code != http.StatusPreconditionRequired {
		t.Fatalf("the wrong name was taken as confirmation: %d", code)
	}
	if code, _ := h.do(t, "PUT", demoRepositories+"source", tom, `{"role":"deployments","type":"git","url":"https://git.example.com/d.git","confirm":"source"}`); code != http.StatusAccepted {
		t.Fatalf("confirmed: %d", code)
	}

	// A new apps repository needs nothing; moving it does.
	if code, _ := h.do(t, "PUT", demoRepositories+"apps", tom, `{"role":"apps","type":"oci","url":"oci://git.example.com/one"}`); code != http.StatusAccepted {
		t.Fatalf("a new apps repository: %d", code)
	}
	before := h.tip(t)
	code, body = h.do(t, "PUT", demoRepositories+"apps", tom, `{"role":"apps","type":"oci","url":"oci://git.example.com/two"}`)
	if code != http.StatusPreconditionRequired || !strings.Contains(body["error"].(string), "oci://git.example.com/one") {
		t.Fatalf("a change of address without confirmation: %d %v", code, body)
	}
	if h.tip(t) != before {
		t.Fatal("an unconfirmed change was committed")
	}
	if code, _ := h.do(t, "PUT", demoRepositories+"apps", tom, `{"role":"apps","type":"oci","url":"oci://git.example.com/two","confirm":"apps"}`); code != http.StatusAccepted {
		t.Fatalf("confirmed: %d", code)
	}
	if file := dt.RemoteFile(t, h.remote, demoDir+"repository-apps.yaml"); !strings.Contains(file, "example.com/two") || strings.Contains(file, "example.com/one") {
		t.Fatalf("the address did not move:\n%s", file)
	}

	// Removal, always.
	before = h.tip(t)
	if code, body := h.do(t, "DELETE", demoRepositories+"apps", tom, ""); code != http.StatusPreconditionRequired || body["confirmWith"] != "apps" {
		t.Fatalf("a removal without confirmation: %d %v", code, body)
	}
	if h.tip(t) != before {
		t.Fatal("an unconfirmed removal was committed")
	}
	if code, body := h.do(t, "DELETE", demoRepositories+"apps?confirm=apps", tom, ""); code != http.StatusAccepted || body["deleted"] != true {
		t.Fatalf("a confirmed removal: %d %v", code, body)
	}
	files := dt.Git(t, "", "--git-dir", h.remote, "ls-tree", "-r", "--name-only", "main")
	if strings.Contains(files, "repository-apps.yaml") {
		t.Fatalf("the declaration is still there:\n%s", files)
	}
	if kustomization := dt.RemoteFile(t, h.remote, demoDir+"kustomization.yaml"); strings.Contains(kustomization, "repository-apps.yaml") {
		t.Fatalf("the kustomization still lists a file that is gone:\n%s", kustomization)
	}
	if code, _ := h.do(t, "DELETE", demoRepositories+"apps?confirm=apps", tom, ""); code != http.StatusNotFound {
		t.Fatalf("removing what is not declared: %d", code)
	}
}

// A name is one object in the cluster whoever declares it. A name the cluster
// or another tenant holds does not exist for anybody else: it is not changed,
// not removed, and not confirmed to be taken.
func TestANameSomebodyElseDeclaredDoesNotExist(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	tina := h.token(t, "tenant-solo", "tina")
	alice := h.token(t, "gentian", "alice")
	body := `{"role":"apps","type":"oci","url":"oci://git.example.com/a"}`
	theirs := `{"role":"apps","type":"oci","url":"oci://evil.example.com/a","confirm":"shared"}`

	if code, _ := h.do(t, "PUT", demoRepositories+"shared", tom, body); code != http.StatusAccepted {
		t.Fatalf("demo's own: %d", code)
	}
	if code, _ := h.do(t, "PUT", clusterRepositories+"mirror", alice, body); code != http.StatusAccepted {
		t.Fatalf("the cluster's own: %d", code)
	}
	before := h.tip(t)

	// Another tenant, on a tenant's name and on the cluster's.
	for _, name := range []string{"shared", "mirror"} {
		taken := strings.ReplaceAll(theirs, `"confirm":"shared"`, `"confirm":"`+name+`"`)
		if code, _ := h.do(t, "PUT", "/v1/tenants/solo/repositories/"+name, tina, taken); code != http.StatusNotFound {
			t.Errorf("tenant solo redeclared %s: %d", name, code)
		}
		if code, _ := h.do(t, "DELETE", "/v1/tenants/solo/repositories/"+name+"?confirm="+name, tina, ""); code != http.StatusNotFound {
			t.Errorf("tenant solo removed %s: %d", name, code)
		}
	}
	// A tenant's own administrator, on the cluster's name.
	if code, _ := h.do(t, "PUT", demoRepositories+"mirror", tom, strings.ReplaceAll(theirs, "shared", "mirror")); code != http.StatusNotFound {
		t.Errorf("tenant demo redeclared the cluster's repository: %d", code)
	}
	// The cluster's administrator, through the cluster's route, on a
	// tenant's name: the tenant's is changed through the tenant.
	if code, _ := h.do(t, "PUT", clusterRepositories+"shared", alice, theirs); code != http.StatusNotFound {
		t.Errorf("the cluster's route redeclared a tenant's repository: %d", code)
	}
	if code, _ := h.do(t, "DELETE", clusterRepositories+"shared?confirm=shared", alice, ""); code != http.StatusNotFound {
		t.Errorf("the cluster's route removed a tenant's repository: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("somebody else's repository was changed")
	}
	if file := dt.RemoteFile(t, h.remote, demoDir+"repository-shared.yaml"); strings.Contains(file, "evil") {
		t.Fatalf("demo's repository now points elsewhere:\n%s", file)
	}
}

// Removing the cluster's own declaration says what did not happen: the
// directory it is in is synced without pruning, so the object stays applied.
func TestRemovingTheClustersRepositorySaysTheClusterKeepsIt(t *testing.T) {
	h := start(t)
	alice := h.token(t, "gentian", "alice")
	if code, _ := h.do(t, "PUT", clusterRepositories+"mirror", alice, `{"role":"apps","type":"oci","url":"oci://registry.example.com/charts"}`); code != http.StatusAccepted {
		t.Fatalf("declare: %d", code)
	}
	code, body := h.do(t, "DELETE", clusterRepositories+"mirror?confirm=mirror", alice, "")
	if code != http.StatusAccepted {
		t.Fatalf("remove: %d %v", code, body)
	}
	if message, _ := body["message"].(string); !strings.Contains(message, "without pruning") {
		t.Fatalf("the answer claims more than happened: %v", body)
	}
	if files := dt.Git(t, "", "--git-dir", h.remote, "ls-tree", "-r", "--name-only", "main"); strings.Contains(files, "mirror-repository.yaml") {
		t.Fatalf("the declaration is still there:\n%s", files)
	}
}

// A git repository with role apps was the declaration that copied every
// profile of the repository into the cluster. Nothing copies profiles any
// more, so declaring one is refused with what to do instead, for a tenant and
// for the cluster alike; a deployments repository and a registry are not
// affected.
func TestAGitAppsRepositoryIsRefusedWithWhatToDoInstead(t *testing.T) {
	h := start(t)
	before := h.tip(t)
	for path, token := range map[string]string{
		demoRepositories + "team-apps":   h.token(t, "tenant-demo", "tom"),
		clusterRepositories + "our-apps": h.token(t, "gentian", "alice"),
	} {
		code, body := h.do(t, "PUT", path, token, `{"role":"apps","type":"git","url":"https://git.example.com/demo/apps.git"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d %v, want 400", path, code, body)
		}
		said := fmt.Sprint(body["error"])
		for _, want := range []string{"no longer declared", "catalogues add", "type oci"} {
			if !strings.Contains(said, want) {
				t.Errorf("%s: the refusal does not say %q: %s", path, want, said)
			}
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused declaration moved the repository")
	}
}
