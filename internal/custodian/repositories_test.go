/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func repoObj(name, tenant, url, role string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(repositoryGVK)
	u.SetNamespace(repositoryNamespace)
	u.SetName(name)
	spec := map[string]any{
		"type":      "git",
		"role":      role,
		"endpoints": map[string]any{"inCluster": url},
	}
	if tenant != "" {
		spec["tenant"] = tenant
	}
	u.Object["spec"] = spec
	return u
}

// TestTenantSeesOwnAndClusterRepositoriesOnly — a tenant needs the cluster's
// base repository in the list, because that is where most of their apps come
// from. Another tenant's is not theirs to know about.
func TestTenantSeesOwnAndClusterRepositoriesOnly(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		repoObj("base", "", "https://git.example/base", roleApps),
		repoObj("acme-private", "acme", "https://git.example/acme", roleApps),
		repoObj("globex-private", "globex", "https://git.example/globex", roleApps),
	)
	w := do(t, s, "GET", "/v1/repositories", "")
	body := w.Body.String()

	if strings.Contains(body, "globex-private") {
		t.Fatalf("another tenant's repository was listed:\n%s", body)
	}
	for _, want := range []string{"base", "acme-private"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in the listing:\n%s", want, body)
		}
	}

	var got struct{ Repositories []RepositoryView }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, r := range got.Repositories {
		switch r.Name {
		case "base":
			if r.Owned {
				t.Fatal("the cluster's repository is marked owned by a tenant")
			}
		case "acme-private":
			if !r.Owned {
				t.Fatal("a tenant's own repository is not marked owned")
			}
		}
	}
}

// The custodian keeps the keys and decides no configuration: it can neither
// declare a repository nor remove one. There is no route for either, whoever
// asks and whatever they hold, and nothing in the cluster changes when one is
// tried. A repository's address is a commit the director makes.
func TestTheCustodianNeitherDeclaresNorRemovesARepository(t *testing.T) {
	for who, start := range map[string]func(*testing.T, ...runtime.Object) (*Server, *httptest.Server){
		"a cluster administrator": newServer,
		"a tenant's administrator": func(t *testing.T, objs ...runtime.Object) (*Server, *httptest.Server) {
			return newServerAsTenant(t, "acme", objs...)
		},
	} {
		t.Run(who, func(t *testing.T) {
			s, _ := start(t,
				repoObj("base", "", "https://git.example/base", roleApps),
				repoObj("acme-private", "acme", "https://git.example/acme", roleApps),
			)
			for _, c := range []struct{ method, target, body string }{
				{"PUT", "/v1/repositories/new-one", `{"role":"apps","type":"git","url":"https://git.example/new"}`},
				{"PUT", "/v1/repositories/acme-private", `{"role":"apps","type":"git","url":"https://evil.example/x","confirm":"acme-private"}`},
				{"PUT", "/v1/repositories/base", `{"role":"apps","type":"git","url":"https://evil.example/x","confirm":"base"}`},
				{"POST", "/v1/repositories", `{"name":"new-one","role":"apps","type":"git","url":"https://git.example/new"}`},
				{"PATCH", "/v1/repositories/acme-private", `{"url":"https://evil.example/x"}`},
				{"DELETE", "/v1/repositories/acme-private?confirm=acme-private", ""},
				{"DELETE", "/v1/repositories/base?confirm=base", ""},
			} {
				w := do(t, s, c.method, c.target, c.body)
				if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s answered %d; the custodian has no such route", c.method, c.target, w.Code)
				}
			}

			var list unstructured.UnstructuredList
			list.SetGroupVersionKind(repositoryGVK.GroupVersion().WithKind(repositoryGVK.Kind + "List"))
			if err := s.Catalogue.Client.List(context.Background(), &list); err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for i := range list.Items {
				url, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "endpoints", "inCluster")
				got[list.Items[i].GetName()] = url
			}
			want := map[string]string{"base": "https://git.example/base", "acme-private": "https://git.example/acme"}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("the repositories in the cluster changed: %v", got)
			}
		})
	}
}

// What the custodian may do to a Repository in the cluster is read it. The
// routes being gone is half of that; the other half is that the credential it
// runs under could not do it either.
func TestTheCustodiansClusterRoleOnlyReadsRepositories(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "gentian-os", "templates", "custodian.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	rules := 0
	for i, line := range lines {
		if !strings.Contains(line, "resources:") || !strings.Contains(line, `"repositories"`) {
			continue
		}
		rules++
		if strings.TrimSpace(line) != `resources: ["repositories"]` {
			t.Fatalf("repositories share a rule with something else, so its verbs are theirs too: %s", line)
		}
		if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != `verbs: ["get", "list", "watch"]` {
			t.Fatalf("the custodian's role on repositories is more than reading: %q", lines[i+1])
		}
	}
	if rules != 1 {
		t.Fatalf("found %d rules naming repositories, want exactly one", rules)
	}
	if strings.Contains(string(raw), `resources: ["*"]`) || strings.Contains(string(raw), `verbs: ["*"]`) {
		t.Fatal("a wildcard rule would grant what the repositories rule withholds")
	}
}
