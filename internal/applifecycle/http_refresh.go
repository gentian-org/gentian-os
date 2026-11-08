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
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/internal/layout"
)

// Argo CD reads the deployments repository when it next polls it, which is
// minutes after the director pushed. The director says here that it pushed,
// and the operator asks Argo CD to read the repository now.
//
// That is all this does. Nothing is synced by it: each Application syncs
// under its own policy once Argo CD has compared it, and a commit Argo CD
// refuses -- an unsigned one, say -- is refused as before. Nor is it needed
// for anything to happen: the poll goes on, and a refresh that was never
// asked for costs a few minutes and nothing else.
//
// The director cannot do this itself. It holds no right in the cluster, and
// is not given one for this: it tells the operator, on the listener it
// already calls, and the operator uses rights it has.
//
// The request says nothing about what to refresh. The operator works the set
// out: in Argo CD's namespace, every ApplicationSet with a git generator over
// the deployments repository, which is what makes a tenant's Application
// exist; and every Application with a source in that repository. A caller
// cannot point this at anything else.
//
// The marks are Argo CD's own, documented ones, and its controllers remove
// each once they have acted on it. Read and patch are all that takes; of
// ApplicationSets the operator has no other right.
//
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;patch
// +kubebuilder:rbac:groups=argoproj.io,resources=applicationsets,verbs=get;list;patch

// RefreshDeploymentsPath is the director's command that Argo CD read the
// deployments repository now.
const RefreshDeploymentsPath = "/v1/actions/refresh-deployments"

const (
	// applicationRefresh asks Argo CD's application controller to compare
	// an Application against its sources as they are now. "normal" is the
	// ordinary refresh; a hard one would also throw away cached manifests,
	// which a new commit does not need.
	applicationRefresh       = "argocd.argoproj.io/refresh"
	applicationRefreshNormal = "normal"
	// applicationSetRefresh asks the ApplicationSet controller to run the
	// set's generators again now rather than at its next requeue.
	applicationSetRefresh = "argocd.argoproj.io/application-set-refresh"
)

// refreshEvery is the least time between two refreshes. Several commits in a
// row -- a purge is two, an import several -- make one refresh at once and
// one more when this has passed, never one each.
const refreshEvery = 5 * time.Second

// refreshBudget bounds a refresh that runs after its request was answered.
const refreshBudget = 30 * time.Second

var applicationSetGVK = applicationGVK.GroupVersion().WithKind("ApplicationSet")

// Whether a refresh was made for a request, or will be.
const (
	RefreshDone     = "refreshed"
	RefreshDeferred = "deferred"
)

// RefreshResult is what a request to refresh came to.
type RefreshResult struct {
	// Status is RefreshDone when Argo CD was asked now, and RefreshDeferred
	// when it was asked too recently: one more refresh is then made when
	// refreshEvery has passed, for every request that arrived meanwhile.
	Status string `json:"status"`
	// What was marked, by name. Empty when deferred.
	ApplicationSets []string `json:"applicationSets"`
	Applications    []string `json:"applications"`
}

// ErrNoDeploymentsRepository is an operator that was not told which
// repository the cluster is declared in, and so cannot tell what reads it.
var ErrNoDeploymentsRepository = errors.New("the operator was not told which repository this cluster is declared in " +
	"(appLifecycle.deployments.repo); nothing was refreshed, and Argo CD reads the repository at its next poll")

func (h *HTTPServer) registerRefreshRoutes(mux router) {
	mux.HandleFunc("POST "+RefreshDeploymentsPath, h.handleRefreshDeployments)
}

func (h *HTTPServer) handleRefreshDeployments(w http.ResponseWriter, r *http.Request) {
	// The body is not read: there is nothing a caller could choose.
	res, err := h.Service.RefreshDeployments(r.Context(), actorOf(r))
	switch {
	case errors.Is(err, ErrNoDeploymentsRepository):
		writeErr(w, http.StatusServiceUnavailable, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	case res.Status == RefreshDeferred:
		writeJSON(w, http.StatusAccepted, res)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// RefreshDeployments asks Argo CD to read the deployments repository now, at
// most once every refreshEvery.
//
// A request inside that time is not dropped. The commit it is about may be
// newer than the one Argo CD was just shown, so one refresh is owed, and is
// made when the time has passed -- one, however many requests asked for it.
func (s *Service) RefreshDeployments(ctx context.Context, actor string) (*RefreshResult, error) {
	if repositoryKey(s.opts.DeploymentsRepo) == "" {
		return nil, ErrNoDeploymentsRepository
	}
	every := s.opts.RefreshEvery
	if every <= 0 {
		every = refreshEvery
	}
	s.refreshMu.Lock()
	if since := time.Since(s.refreshLast); !s.refreshLast.IsZero() && since < every {
		if !s.refreshOwed {
			s.refreshOwed = true
			time.AfterFunc(every-since, func() {
				s.refreshMu.Lock()
				s.refreshOwed = false
				s.refreshLast = time.Now()
				s.refreshMu.Unlock()
				owed, cancel := context.WithTimeout(context.Background(), refreshBudget)
				defer cancel()
				// Nobody is waiting for this answer; the log is where it goes.
				_, _ = s.refreshNow(owed, actor)
			})
		}
		s.refreshMu.Unlock()
		return &RefreshResult{Status: RefreshDeferred, ApplicationSets: []string{}, Applications: []string{}}, nil
	}
	s.refreshLast = time.Now()
	s.refreshMu.Unlock()
	return s.refreshNow(ctx, actor)
}

// refreshNow marks what reads the deployments repository. One object that
// cannot be marked does not stop the others: each is a separate question to
// Argo CD, and the ones that were asked are answered.
func (s *Service) refreshNow(ctx context.Context, actor string) (*RefreshResult, error) {
	logger := log.FromContext(ctx).WithName("refresh-deployments")
	namespace := layout.Namespace(layout.GitOps)
	repo := repositoryKey(s.opts.DeploymentsRepo)
	res := &RefreshResult{Status: RefreshDone, ApplicationSets: []string{}, Applications: []string{}}
	var failed []error

	// The sets first: a tenant that is new has no Application until its set
	// has generated one, and an Application Argo CD has just created is
	// compared without being asked.
	sets := &unstructured.UnstructuredList{}
	sets.SetGroupVersionKind(applicationSetGVK.GroupVersion().WithKind("ApplicationSetList"))
	if err := s.live().List(ctx, sets, client.InNamespace(namespace)); err != nil {
		failed = append(failed, fmt.Errorf("the ApplicationSets in %s could not be listed: %w", namespace, err))
	}
	for i := range sets.Items {
		set := &sets.Items[i]
		generators, _, _ := unstructured.NestedSlice(set.Object, "spec", "generators")
		if !generatesFrom(generators, repo) {
			continue
		}
		if err := s.mark(ctx, set, applicationSetRefresh, "true"); err != nil {
			failed = append(failed, fmt.Errorf("the ApplicationSet %s: %w", set.GetName(), err))
			continue
		}
		res.ApplicationSets = append(res.ApplicationSets, set.GetName())
	}

	apps := &unstructured.UnstructuredList{}
	apps.SetGroupVersionKind(applicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err := s.live().List(ctx, apps, client.InNamespace(namespace)); err != nil {
		failed = append(failed, fmt.Errorf("the Applications in %s could not be listed: %w", namespace, err))
	}
	for i := range apps.Items {
		app := &apps.Items[i]
		if !readsFrom(app, repo) {
			continue
		}
		if err := s.mark(ctx, app, applicationRefresh, applicationRefreshNormal); err != nil {
			failed = append(failed, fmt.Errorf("the Application %s: %w", app.GetName(), err))
			continue
		}
		res.Applications = append(res.Applications, app.GetName())
	}
	sort.Strings(res.ApplicationSets)
	sort.Strings(res.Applications)

	if err := errors.Join(failed...); err != nil {
		logger.Error(err, "Argo CD could not be asked to read all of the deployments repository now; it reads it at its next poll",
			"actor", actor, "applicationSets", res.ApplicationSets, "applications", res.Applications)
		return nil, err
	}
	logger.Info("asked Argo CD to read the deployments repository now",
		"actor", actor, "applicationSets", res.ApplicationSets, "applications", res.Applications)
	return res, nil
}

// mark sets one of Argo CD's request annotations on an object, unless one is
// there already: a request Argo CD has not yet acted on is not replaced,
// which would turn somebody's hard refresh into an ordinary one.
func (s *Service) mark(ctx context.Context, obj *unstructured.Unstructured, annotation, value string) error {
	if _, pending := obj.GetAnnotations()[annotation]; pending {
		return nil
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annotation, value)
	return s.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(patch)))
}

// readsFrom reports whether an Application has a source in the repository.
func readsFrom(app *unstructured.Unstructured, repo string) bool {
	if url, _, _ := unstructured.NestedString(app.Object, "spec", "source", "repoURL"); repositoryKey(url) == repo {
		return true
	}
	sources, _, _ := unstructured.NestedSlice(app.Object, "spec", "sources")
	for _, raw := range sources {
		source, _ := raw.(map[string]any)
		if url, _ := source["repoURL"].(string); repositoryKey(url) == repo {
			return true
		}
	}
	return false
}

// generatesFrom reports whether any of an ApplicationSet's generators is a
// git generator over the repository. A matrix or a merge generator carries
// generators of its own, so the search goes down into them.
func generatesFrom(generators []any, repo string) bool {
	for _, raw := range generators {
		generator, _ := raw.(map[string]any)
		if git, ok := generator["git"].(map[string]any); ok {
			if url, _ := git["repoURL"].(string); repositoryKey(url) == repo {
				return true
			}
		}
		for _, combining := range []string{"matrix", "merge"} {
			inner, _, _ := unstructured.NestedSlice(generator, combining, "generators")
			if generatesFrom(inner, repo) {
				return true
			}
		}
	}
	return false
}

// repositoryKey is a repository's URL as two spellings of it compare equal:
// without the trailing slash and the ".git" a remote may be written with.
func repositoryKey(url string) string {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	return strings.TrimSuffix(url, ".git")
}
