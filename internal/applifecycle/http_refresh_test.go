/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/layout"
)

const (
	deploymentsRepo = "https://git.example/acme/deployments"
	softwareRepo    = "https://git.example/gentian/software"
)

func argo(kind, namespace, name string, spec map[string]any, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(applicationGVK.GroupVersion().WithKind(kind))
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetAnnotations(annotations)
	return obj
}

func application(namespace, name string, annotations map[string]string, repos ...string) *unstructured.Unstructured {
	if len(repos) == 1 {
		return argo("Application", namespace, name, map[string]any{"source": map[string]any{"repoURL": repos[0]}}, annotations)
	}
	sources := []any{}
	for _, repo := range repos {
		sources = append(sources, map[string]any{"repoURL": repo})
	}
	return argo("Application", namespace, name, map[string]any{"sources": sources}, annotations)
}

func applicationSet(name string, generators ...any) *unstructured.Unstructured {
	return argo("ApplicationSet", layout.Namespace(layout.GitOps), name, map[string]any{"generators": generators}, nil)
}

func gitGenerator(repo string) map[string]any {
	return map[string]any{"git": map[string]any{"repoURL": repo, "revision": "main"}}
}

var listGenerator = map[string]any{"list": map[string]any{"elements": []any{map[string]any{"env": "dev"}}}}

// argoWorld is Argo CD's namespace on a cluster declared in deploymentsRepo:
// what reads that repository, beside what does not and must be left alone.
// refreshes counts how often the operator went looking.
func argoWorld(t *testing.T, opts Options) (*Service, client.Client, *atomic.Int32) {
	t.Helper()
	gitops := layout.Namespace(layout.GitOps)
	refreshes := &atomic.Int32{}
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(
		// A tenant's Application exists because this set finds its
		// directory in the repository.
		applicationSet("gentian-tenants", gitGenerator(deploymentsRepo+".git")),
		// Generates from a list; its Application is what reads the repository.
		applicationSet("gentian-claims", listGenerator),
		// A git generator inside a matrix is still one.
		applicationSet("regions", map[string]any{"matrix": map[string]any{"generators": []any{listGenerator, gitGenerator(deploymentsRepo)}}}),
		// Over another repository: none of the director's business.
		applicationSet("gentian-data-plane", gitGenerator(softwareRepo)),

		application(gitops, "tenant-demo", nil, deploymentsRepo),
		application(gitops, "gentian-claims-dev", nil, deploymentsRepo+"/"),
		// The repository as the second of several sources.
		application(gitops, "gentian-os", nil, softwareRepo, deploymentsRepo),
		// Somebody asked for more than this does; their request stands.
		application(gitops, "tenant-other", map[string]string{applicationRefresh: "hard"}, deploymentsRepo),
		application(gitops, "kyverno", nil, softwareRepo),
		// The same repository, read from outside Argo CD's own namespace.
		application("tenant-demo", "elsewhere", nil, deploymentsRepo),
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, o ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "ApplicationSetList" {
				refreshes.Add(1)
			}
			return c.List(ctx, list, o...)
		},
	}).Build()
	return &Service{client: c, opts: opts}, c, refreshes
}

func annotationOf(t *testing.T, c client.Client, kind, namespace, name, annotation string) (string, bool) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(applicationGVK.GroupVersion().WithKind(kind))
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	value, ok := obj.GetAnnotations()[annotation]
	return value, ok
}

func post(mux http.Handler, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", RefreshDeploymentsPath, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Gentian-Actor", "ada@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// The refresh is a command, and so the director's alone. The usher reads
// state; an identity that is neither is not answered at all.
func TestOnlyTheDirectorMayAskForARefresh(t *testing.T) {
	svc, c, refreshes := argoWorld(t, Options{DeploymentsRepo: deploymentsRepo})
	_, auth := cluster()
	mux := (&HTTPServer{Auth: auth, Service: svc}).routes()

	for token, want := range map[string]int{
		"usher":                       http.StatusForbidden,
		"custodian":                   http.StatusForbidden,
		"impostor":                    http.StatusForbidden,
		"director-for-the-api-server": http.StatusUnauthorized,
		"":                            http.StatusUnauthorized,
	} {
		if got := post(mux, token, "").Code; got != want {
			t.Errorf("token %q: answered %d, want %d", token, got, want)
		}
	}
	if refreshes.Load() != 0 {
		t.Fatal("a refused caller made the operator look")
	}
	if _, marked := annotationOf(t, c, "Application", layout.Namespace(layout.GitOps), "tenant-demo", applicationRefresh); marked {
		t.Fatal("a refused caller had an Application refreshed")
	}
	if got := answer(mux, "GET", RefreshDeploymentsPath, "director"); got != http.StatusMethodNotAllowed {
		t.Errorf("the route is a command; GET answered %d", got)
	}
	if got := post(mux, "director", "").Code; got != http.StatusOK {
		t.Fatalf("the director was answered %d", got)
	}
}

// What is refreshed is what reads the deployments repository in Argo CD's
// namespace, as the operator finds it. Nothing in the request chooses, and
// nothing else is touched.
func TestARefreshMarksWhatReadsTheDeploymentsRepositoryAndNothingElse(t *testing.T) {
	svc, c, _ := argoWorld(t, Options{DeploymentsRepo: deploymentsRepo + "/"})
	_, auth := cluster()
	mux := (&HTTPServer{Auth: auth, Service: svc}).routes()
	gitops := layout.Namespace(layout.GitOps)

	// A body that tries to choose is not read.
	w := post(mux, "director", `{"applications":["kyverno"],"namespace":"tenant-demo","hard":true,"sync":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d %s", w.Code, w.Body)
	}
	var got RefreshResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := RefreshResult{
		Status:          RefreshDone,
		ApplicationSets: []string{"gentian-tenants", "regions"},
		Applications:    []string{"gentian-claims-dev", "gentian-os", "tenant-demo", "tenant-other"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("answered\n%+v\nwant\n%+v", got, want)
	}

	for _, set := range []struct {
		name   string
		marked bool
	}{{"gentian-tenants", true}, {"regions", true}, {"gentian-claims", false}, {"gentian-data-plane", false}} {
		value, marked := annotationOf(t, c, "ApplicationSet", gitops, set.name, applicationSetRefresh)
		if marked != set.marked || (marked && value != "true") {
			t.Errorf("ApplicationSet %s: marked=%v value=%q", set.name, marked, value)
		}
	}
	for _, app := range []struct{ namespace, name, want string }{
		{gitops, "tenant-demo", "normal"},
		{gitops, "gentian-claims-dev", "normal"},
		{gitops, "gentian-os", "normal"},
		// Not turned into the lesser request.
		{gitops, "tenant-other", "hard"},
		{gitops, "kyverno", ""},
		{"tenant-demo", "elsewhere", ""},
	} {
		if value, _ := annotationOf(t, c, "Application", app.namespace, app.name, applicationRefresh); value != app.want {
			t.Errorf("Application %s/%s: refresh=%q, want %q", app.namespace, app.name, value, app.want)
		}
	}

	// A refresh and never a sync: the one thing written is the annotation.
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: gitops, Name: "tenant-demo"}, app); err != nil {
		t.Fatal(err)
	}
	if _, syncing := app.Object["operation"]; syncing {
		t.Fatal("the refresh started a sync")
	}
	if !reflect.DeepEqual(app.Object["spec"], map[string]any{"source": map[string]any{"repoURL": deploymentsRepo}}) {
		t.Fatalf("the refresh changed the Application: %v", app.Object["spec"])
	}
}

// Commits come in bursts: a purge is two, an import several. They make one
// refresh at once and one more when the time has passed, however many asked.
func TestABurstOfRequestsMakesOneRefreshNowAndOneLater(t *testing.T) {
	const every = 300 * time.Millisecond
	svc, _, refreshes := argoWorld(t, Options{DeploymentsRepo: deploymentsRepo, RefreshEvery: every})
	_, auth := cluster()
	mux := (&HTTPServer{Auth: auth, Service: svc}).routes()

	if w := post(mux, "director", ""); w.Code != http.StatusOK {
		t.Fatalf("the first request answered %d %s", w.Code, w.Body)
	}
	for i := 0; i < 5; i++ {
		w := post(mux, "director", "")
		var got RefreshResult
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusAccepted || got.Status != RefreshDeferred {
			t.Fatalf("request %d inside the interval answered %d %+v", i+2, w.Code, got)
		}
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("six requests at once made %d refreshes, want 1", n)
	}

	// The commit behind the later requests is newer than what Argo CD was
	// shown, so one refresh is owed -- one.
	deadline := time.Now().Add(10 * every)
	for refreshes.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(2 * every)
	if n := refreshes.Load(); n != 2 {
		t.Fatalf("the burst came to %d refreshes, want 2", n)
	}

	// And a request after the interval is answered at once again.
	if w := post(mux, "director", ""); w.Code != http.StatusOK {
		t.Fatalf("a request after the interval answered %d %s", w.Code, w.Body)
	}
}

// An operator that does not know the repository cannot tell what reads it,
// and refreshes nothing rather than everything.
func TestAnOperatorThatDoesNotKnowTheRepositoryRefreshesNothing(t *testing.T) {
	svc, c, refreshes := argoWorld(t, Options{})
	_, auth := cluster()
	mux := (&HTTPServer{Auth: auth, Service: svc}).routes()

	if w := post(mux, "director", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("answered %d %s", w.Code, w.Body)
	}
	if refreshes.Load() != 0 {
		t.Fatal("the operator looked without knowing what for")
	}
	if _, marked := annotationOf(t, c, "Application", layout.Namespace(layout.GitOps), "tenant-demo", applicationRefresh); marked {
		t.Fatal("an Application was refreshed")
	}
}

// The two ends against each other: the director's client, with the token it
// already presents here, and the listener it calls.
func TestTheDirectorTellsTheOperatorOfAPushThroughItsOwnClient(t *testing.T) {
	if RefreshDeploymentsPath != lifecycle.RefreshDeploymentsPath {
		t.Fatalf("the operator serves %s and the director asks %s", RefreshDeploymentsPath, lifecycle.RefreshDeploymentsPath)
	}
	svc, c, _ := argoWorld(t, Options{DeploymentsRepo: deploymentsRepo, RefreshEvery: time.Hour})
	_, auth := cluster()
	srv := httptest.NewServer((&HTTPServer{Auth: auth, Service: svc}).routes())
	defer srv.Close()
	as := func(token string) func() string { return func() string { return token } }

	// The usher's identity, on the director's client, is still the usher's.
	err := lifecycle.New(srv.URL, as("usher")).RefreshDeployments(context.Background(), "ada")
	var upstream *lifecycle.UpstreamError
	if !errors.As(err, &upstream) || upstream.Status != http.StatusForbidden {
		t.Fatalf("the usher: %v", err)
	}

	director := lifecycle.New(srv.URL, as("director"))
	if err := director.RefreshDeployments(context.Background(), "ada"); err != nil {
		t.Fatal(err)
	}
	if value, _ := annotationOf(t, c, "ApplicationSet", layout.Namespace(layout.GitOps), "gentian-tenants", applicationSetRefresh); value != "true" {
		t.Fatal("the tenants' ApplicationSet was not asked to generate again")
	}
	// Asked again at once: deferred, which the director takes as done.
	if err := director.RefreshDeployments(context.Background(), "ada"); err != nil {
		t.Fatalf("a deferred refresh was reported as a failure: %v", err)
	}
}
