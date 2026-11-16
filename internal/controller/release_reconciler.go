/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/internal/platformversion"
	"github.com/gentian-org/gentian-os/internal/releaseindex"
)

// ReleaseReconciler applies the Cluster claim's spec.release: the release a
// cluster runs, or the branch it follows (docs/concepts/versioning.md §2).
//
// The ref a cluster runs used to live in the installer's settings alone,
// rendered once into the Argo CD Applications of the bootstrap chart
// (kernel/bootstrap/chart) and never read again. The claim is where the
// cluster's state is declared, so the ref is declared there, and this is
// what turns the declaration into the Applications: every Application on
// the gentian-os repository that the installer applied follows the release's
// revision, the operator's own image moves to the release's build (Argo CD
// rolls it; the new build carries on), and the gentian-ui charts move to the
// ones built for the release. One declaration, one coherent set.
//
// A version is a tag the release index lists: the revision is the tag, the
// image its number, the UI charts the entry's. A track is a branch: the
// revision is the branch's head, the image the build CI published for that
// commit, and the cluster waits (Pending) while the build is not published
// yet, so a branch tip never puts a cluster onto an image that does not
// exist. The infra charts the Cluster composition reads follow by
// themselves: the composition synced at the new revision names its own.
//
// What was done is written on the claim's status (status.release), with
// what the index offers this cluster under its autoUpgrade mode: the
// director moves spec.release by that, and a person reads it in the
// administration console.
//
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;patch
type ReleaseReconciler struct {
	// Reader reads the claims and the Applications, uncached, on a timer.
	Reader client.Reader
	// Client patches the Applications and the composite's status.
	Client client.Client
	// Interval is how often the claim is applied; the default is five
	// minutes. A branch moves at that pace.
	Interval time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// IndexURL is where the release index is read from (the platform status
	// writer's feed); "" resolves no version.
	IndexURL string
	// Resolver answers what a branch and a registry hold; nil is the one
	// that asks GitHub and ghcr.io.
	Resolver ReleaseResolver
	// HTTP is the client the default resolver and the index reader use; nil
	// is one with a short timeout.
	HTTP *http.Client
}

// ReleaseResolver answers the questions a track needs: where a branch is,
// whether a build is published, and which build a moving chart names.
type ReleaseResolver interface {
	// BranchHead is the commit a branch of a git repository points to.
	BranchHead(ctx context.Context, repoURL, branch string) (string, error)
	// ImageExists says whether an image, "ghcr.io/org/name:tag", is published.
	ImageExists(ctx context.Context, image string) (bool, error)
	// ChartBuild is the immutable version a moving chart version names:
	// "0.1.0-develop" of "ghcr.io/org/charts/name" is "0.1.0-develop.abc1234".
	ChartBuild(ctx context.Context, repo, moving string) (string, error)
}

const (
	defaultReleaseInterval = 5 * time.Minute
	defaultGentianOSRepo   = "https://github.com/gentian-org/gentian-os"
	defaultOperatorImage   = "ghcr.io/gentian-org/gentian-os"
	defaultChartRegistry   = "ghcr.io/gentian-org/charts"

	// The states status.release.state takes.
	releaseApplied   = "Applied"
	releasePending   = "Pending"
	releaseRefused   = "Refused"
	releaseUnmanaged = "Unmanaged"

	// The Applications the bootstrap chart renders with values of their
	// own, beyond a source's targetRevision.
	appsetsApplication  = "gentian-appsets"
	operatorApplication = "gentian-os"
)

// uiComponents maps the operator chart's UI value keys to the gentian-ui
// chart each installs, and the base version the bootstrap chart pins them
// at when the Application names none.
var uiComponents = []struct{ key, chart, base string }{
	{"desktop", "gentian-portal", "0.1.0"},
	{"adminConsole", "admin-console", "0.1.1"},
	{"concierge", "concierge", "0.1.0"},
}

// NeedLeaderElection keeps one replica applying.
func (r *ReleaseReconciler) NeedLeaderElection() bool { return true }

// Start applies the claim at start and then on the interval.
func (r *ReleaseReconciler) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("release")
	interval := r.Interval
	if interval <= 0 {
		interval = defaultReleaseInterval
	}
	for {
		if err := r.Once(ctx); err != nil {
			logger.Error(err, "the Cluster claim's release could not be applied; it is retried")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// releaseSpec is spec.release as read from the claim.
type releaseSpec struct {
	Track, Version, AutoUpgrade string
}

// releasePlan is what a spec resolves to.
type releasePlan struct {
	Revision, Image string
	// UI is the chart version per component key; absent keys are left as
	// they are.
	UI map[string]string
}

// Once applies every Cluster claim's spec.release.
func (r *ReleaseReconciler) Once(ctx context.Context) error {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	idx, idxErr := r.index(ctx)
	return forEachClusterClaim(ctx, r.Reader, func(claim, xr *unstructured.Unstructured) error {
		spec := readReleaseSpec(claim)
		status := map[string]any{}
		current := platformversion.ChartVersion
		switch {
		case spec.Track == "" && spec.Version == "":
			status["state"] = releaseUnmanaged
			status["message"] = "spec.release is absent: the cluster stays on what the installer rendered"
		default:
			desired := spec.Version
			if desired == "" {
				desired = spec.Track
			}
			status["desired"] = desired
			plan, state, message := r.resolve(ctx, spec, idx, idxErr)
			if plan != nil {
				changed, err := r.apply(ctx, plan)
				if err != nil {
					return err
				}
				status["revision"] = plan.Revision
				status["image"] = plan.Image
				status["appliedAt"] = now().UTC().Format(time.RFC3339)
				if changed > 0 {
					log.FromContext(ctx).Info("the cluster's release was applied", "claim", claim.GetName(), "desired", desired, "revision", plan.Revision, "image", plan.Image, "applications", changed)
				}
			}
			status["state"] = state
			status["message"] = message
			if spec.Version != "" {
				current = spec.Version
			}
		}
		if idxErr == nil && spec.Track == "" {
			mode := releaseindex.Mode(spec.AutoUpgrade)
			if mode == "" {
				mode = releaseindex.ModeNone
			}
			take, pending := idx.Next(current, mode)
			if take != nil {
				status["available"] = take.Version
			}
			if pending != nil {
				status["pending"] = pending.Version
				if pending.Notes != "" {
					status["pendingNotes"] = pending.Notes
				}
			}
		}
		return r.writeStatus(ctx, xr, status)
	})
}

func readReleaseSpec(claim *unstructured.Unstructured) releaseSpec {
	track, _, _ := unstructured.NestedString(claim.Object, "spec", "release", "track")
	version, _, _ := unstructured.NestedString(claim.Object, "spec", "release", "version")
	mode, _, _ := unstructured.NestedString(claim.Object, "spec", "release", "autoUpgrade")
	return releaseSpec{Track: strings.TrimSpace(track), Version: strings.TrimSpace(version), AutoUpgrade: strings.TrimSpace(mode)}
}

func (r *ReleaseReconciler) index(ctx context.Context) (releaseindex.Index, error) {
	if r.IndexURL == "" {
		return releaseindex.Index{}, fmt.Errorf("no release index address is configured (GENTIAN_RELEASE_FEED_URL)")
	}
	return releaseindex.Fetch(ctx, r.httpClient(), r.IndexURL)
}

func (r *ReleaseReconciler) httpClient() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (r *ReleaseReconciler) resolver() ReleaseResolver {
	if r.Resolver != nil {
		return r.Resolver
	}
	return &httpReleaseResolver{HTTP: r.httpClient()}
}

// resolve turns the spec into a plan, or says why not.
func (r *ReleaseReconciler) resolve(ctx context.Context, spec releaseSpec, idx releaseindex.Index, idxErr error) (*releasePlan, string, string) {
	if spec.Version != "" {
		if idxErr != nil {
			return nil, releasePending, fmt.Sprintf("the release index could not be read: %v", idxErr)
		}
		entry, ok := idx.Find(spec.Version)
		if !ok {
			return nil, releaseRefused, fmt.Sprintf("the release index does not list %s", spec.Version)
		}
		plan := &releasePlan{Revision: entry.Version, Image: strings.TrimPrefix(entry.Version, "v"), UI: map[string]string{}}
		for _, c := range uiComponents {
			if v := entry.UI[c.key]; v != "" {
				plan.UI[c.key] = v
			}
		}
		return plan, releaseApplied, fmt.Sprintf("release %s", entry.Version)
	}
	repo, image, bases, err := r.currentPins(ctx)
	if err != nil {
		return nil, releasePending, err.Error()
	}
	res := r.resolver()
	head, err := res.BranchHead(ctx, repo, spec.Track)
	if err != nil {
		return nil, releasePending, fmt.Sprintf("branch %s of %s could not be read: %v", spec.Track, repo, err)
	}
	tag := branchImageTag(spec.Track, head)
	ok, err := res.ImageExists(ctx, image+":"+tag)
	if err != nil {
		return nil, releasePending, fmt.Sprintf("the registry could not say whether %s:%s is published: %v", image, tag, err)
	}
	if !ok {
		return nil, releasePending, fmt.Sprintf("branch %s is at %s, whose build %s:%s is not published yet", spec.Track, head[:7], image, tag)
	}
	plan := &releasePlan{Revision: head, Image: tag, UI: map[string]string{}}
	for _, c := range uiComponents {
		moving := bases[c.key] + "-" + spec.Track
		build, err := res.ChartBuild(ctx, defaultChartRegistry+"/"+c.chart, moving)
		if err != nil {
			return nil, releasePending, fmt.Sprintf("chart %s:%s names no build: %v", c.chart, moving, err)
		}
		plan.UI[c.key] = build
	}
	return plan, releaseApplied, fmt.Sprintf("branch %s at %s", spec.Track, head[:7])
}

// branchImageTag is the image tag CI publishes for a branch's commit:
// <branch>-<sha7>, the branch's slashes as dashes.
func branchImageTag(branch, sha string) string {
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	return strings.ReplaceAll(branch, "/", "-") + "-" + short
}

// currentPins reads the gentian-os Application for the repository it
// follows, the operator image it runs and the base of each UI chart's
// version, so a track resolves against what the cluster was installed with
// rather than against constants alone.
func (r *ReleaseReconciler) currentPins(ctx context.Context) (repo, image string, bases map[string]string, err error) {
	repo, image, bases = defaultGentianOSRepo, defaultOperatorImage, map[string]string{}
	for _, c := range uiComponents {
		bases[c.key] = c.base
	}
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argoApplicationGVK)
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: argocdNamespace, Name: operatorApplication}, app); err != nil {
		return "", "", nil, fmt.Errorf("the %s Application could not be read: %w", operatorApplication, err)
	}
	for _, src := range applicationSources(app) {
		if path, _, _ := unstructured.NestedString(src, "path"); path == "charts/gentian-os" {
			if u, _, _ := unstructured.NestedString(src, "repoURL"); u != "" {
				repo = u
			}
			if v, _, _ := unstructured.NestedString(src, "helm", "valuesObject", "image", "repository"); v != "" {
				image = v
			}
			for _, c := range uiComponents {
				if v, _, _ := unstructured.NestedString(src, "helm", "valuesObject", c.key, "chart", "version"); v != "" {
					if base, _, ok := strings.Cut(v, "-"); ok && base != "" {
						bases[c.key] = base
					}
				}
			}
		}
	}
	return repo, image, bases, nil
}

var (
	argoApplicationGVK    = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}
	argoApplicationSetKnd = "ApplicationSet"
)

// applicationSources is spec.sources, or spec.source as a list of one.
func applicationSources(app *unstructured.Unstructured) []map[string]any {
	if list, found, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); found {
		var out []map[string]any
		for _, s := range list {
			if m, ok := s.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	if m, found, _ := unstructured.NestedMap(app.Object, "spec", "source"); found {
		return []map[string]any{m}
	}
	return nil
}

func setApplicationSources(app *unstructured.Unstructured, sources []map[string]any) {
	if _, found, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); found {
		list := make([]any, len(sources))
		for i := range sources {
			list[i] = sources[i]
		}
		_ = unstructured.SetNestedSlice(app.Object, list, "spec", "sources")
		return
	}
	if len(sources) == 1 {
		_ = unstructured.SetNestedMap(app.Object, sources[0], "spec", "source")
	}
}

func sameRepo(a, b string) bool {
	norm := func(s string) string {
		return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(s), "/"), ".git")
	}
	return norm(a) == norm(b)
}

// apply moves every Application the installer applied on the gentian-os
// repository to the plan: those the bootstrap chart rendered, which is
// every Application in Argo CD's namespace with a source on that repository
// that no ApplicationSet owns (an ApplicationSet's children follow their
// set, which follows gentian-appsets). Returns how many changed.
func (r *ReleaseReconciler) apply(ctx context.Context, plan *releasePlan) (int, error) {
	repo := defaultGentianOSRepo
	if cur, _, _, err := r.currentPins(ctx); err == nil {
		repo = cur
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(argoApplicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err := r.Reader.List(ctx, list, client.InNamespace(argocdNamespace)); err != nil {
		return 0, fmt.Errorf("list the Applications in %s: %w", argocdNamespace, err)
	}
	changed := 0
	for i := range list.Items {
		app := &list.Items[i]
		if ownedByApplicationSet(app) {
			continue
		}
		before := app.DeepCopy()
		sources := applicationSources(app)
		touched := false
		for _, src := range sources {
			u, _, _ := unstructured.NestedString(src, "repoURL")
			if !sameRepo(u, repo) {
				continue
			}
			if rev, _, _ := unstructured.NestedString(src, "targetRevision"); rev != plan.Revision {
				_ = unstructured.SetNestedField(src, plan.Revision, "targetRevision")
				touched = true
			}
			switch app.GetName() {
			case appsetsApplication:
				touched = setIfDiffers(src, plan.Revision, "helm", "valuesObject", "targetRevision") || touched
				touched = setIfDiffers(src, plan.Image, "helm", "valuesObject", "imageTag") || touched
			case operatorApplication:
				touched = setIfDiffers(src, plan.Image, "helm", "valuesObject", "image", "tag") || touched
				for key, version := range plan.UI {
					touched = setIfDiffers(src, version, "helm", "valuesObject", key, "chart", "version") || touched
				}
			}
		}
		if !touched {
			continue
		}
		setApplicationSources(app, sources)
		if err := r.Client.Patch(ctx, app, client.MergeFrom(before)); err != nil {
			return changed, fmt.Errorf("move Application %s to %s: %w", app.GetName(), plan.Revision, err)
		}
		changed++
	}
	return changed, nil
}

func setIfDiffers(m map[string]any, value string, fields ...string) bool {
	if cur, _, _ := unstructured.NestedString(m, fields...); cur == value {
		return false
	}
	_ = unstructured.SetNestedField(m, value, fields...)
	return true
}

func ownedByApplicationSet(app *unstructured.Unstructured) bool {
	for _, ref := range app.GetOwnerReferences() {
		if ref.Kind == argoApplicationSetKnd {
			return true
		}
	}
	return false
}

func (r *ReleaseReconciler) writeStatus(ctx context.Context, xr *unstructured.Unstructured, status map[string]any) error {
	// Replace the block whole: a field that no longer applies (pending, once
	// taken) must go, and a merge patch keeps what it is not told about.
	patch, _ := json.Marshal(map[string]any{"status": map[string]any{"release": nil}})
	if err := r.Client.Status().Patch(ctx, xr, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("write the release on %s: %w", xr.GetName(), err)
	}
	patch, _ = json.Marshal(map[string]any{"status": map[string]any{"release": status}})
	if err := r.Client.Status().Patch(ctx, xr, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("write the release on %s: %w", xr.GetName(), err)
	}
	return nil
}
