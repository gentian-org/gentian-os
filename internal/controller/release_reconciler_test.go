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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testIndex = `releases:
  - version: v0.5.0
    line: "0.5"
    upgradesFrom: []
    ui: {desktop: 0.1.0-v0.5.0, adminConsole: 0.1.1-v0.5.0, concierge: 0.1.0-v0.5.0}
  - version: v0.5.1
    line: "0.5"
    notes: https://example.org/v0.5.1
    upgradesFrom: ["0.5"]
    ui: {desktop: 0.1.0-v0.5.1, adminConsole: 0.1.1-v0.5.1, concierge: 0.1.0-v0.5.1}
  - version: v0.6.0
    line: "0.6"
    notes: https://example.org/v0.6.0
    upgradesFrom: []
`

type fakeResolver struct {
	head   string
	images map[string]bool
	charts map[string]string
	err    error
}

func (f fakeResolver) BranchHead(context.Context, string, string) (string, error) {
	return f.head, f.err
}
func (f fakeResolver) ImageExists(_ context.Context, image string) (bool, error) {
	return f.images[image], nil
}
func (f fakeResolver) ChartBuild(_ context.Context, repo, moving string) (string, error) {
	if v, ok := f.charts[repo+":"+moving]; ok {
		return v, nil
	}
	return "", errors.New("not published")
}

func releaseFixtures(release map[string]any) []client.Object {
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gentianos.io/v1alpha1", "kind": "Cluster",
		"metadata": map[string]any{"name": "cluster", "namespace": clusterConfigNamespace},
		"spec": map[string]any{"resourceRef": map[string]any{
			"apiVersion": "gentianos.io/v1alpha1", "kind": "XCluster", "name": "cluster-abc12",
		}},
	}}
	if release != nil {
		_ = unstructured.SetNestedMap(claim.Object, release, "spec", "release")
	}
	xr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gentianos.io/v1alpha1", "kind": "XCluster",
		"metadata": map[string]any{"name": "cluster-abc12"},
		"status":   map[string]any{"kernelNamespaces": []any{"kernel-control"}},
	}}
	app := func(name string, spec map[string]any) *unstructured.Unstructured {
		a := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
			"metadata": map[string]any{"name": name, "namespace": argocdNamespace},
			"spec":     spec,
		}}
		return a
	}
	operator := app(operatorApplication, map[string]any{
		"sources": []any{
			map[string]any{
				"repoURL": "https://github.com/gentian-org/gentian-os", "targetRevision": "0123456789abcdef0123456789abcdef01234567", "path": "charts/gentian-os",
				"helm": map[string]any{"valuesObject": map[string]any{
					"image":        map[string]any{"repository": "ghcr.io/gentian-org/gentian-os", "tag": "develop-0123456"},
					"desktop":      map[string]any{"enabled": true, "chart": map[string]any{"version": "0.1.0-develop.0123456"}},
					"adminConsole": map[string]any{"enabled": true, "chart": map[string]any{"version": "0.1.1-develop.0123456"}},
					"concierge":    map[string]any{"enabled": true, "chart": map[string]any{"version": "0.1.0-develop.0123456"}},
				}},
			},
			map[string]any{"repoURL": "https://github.com/example/deployments", "targetRevision": "main", "ref": "deploy"},
		},
	})
	appsets := app(appsetsApplication, map[string]any{
		"source": map[string]any{
			"repoURL": "https://github.com/gentian-org/gentian-os.git", "targetRevision": "0123456789abcdef0123456789abcdef01234567", "path": "kernel/appsets",
			"helm": map[string]any{"valuesObject": map[string]any{"targetRevision": "0123456789abcdef0123456789abcdef01234567", "imageTag": "develop-0123456", "stage": "dev"}},
		},
	})
	cnpg := app("cnpg", map[string]any{
		"source": map[string]any{"repoURL": "https://github.com/gentian-org/gentian-os", "targetRevision": "0123456789abcdef0123456789abcdef01234567", "path": "charts/infra/cnpg"},
	})
	child := app("tenant-x", map[string]any{
		"source": map[string]any{"repoURL": "https://github.com/gentian-org/gentian-os", "targetRevision": "0123456789abcdef0123456789abcdef01234567", "path": "kernel/services/x"},
	})
	child.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "ApplicationSet", Name: "gentian-definitions", UID: "1"}})
	other := app("deployments-thing", map[string]any{
		"source": map[string]any{"repoURL": "https://github.com/example/deployments", "targetRevision": "main", "path": "clusters/x"},
	})
	return []client.Object{claim, xr, operator, appsets, cnpg, child, other}
}

// releaseClient is a fake cluster holding the fixtures, the composite with a
// status subresource.
func releaseClient(objs []client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(objs...).WithStatusSubresource(objs[1]).Build()
}

var xclusterGVK = schema.GroupVersionKind{Group: "gentianos.io", Version: "v1alpha1", Kind: "XCluster"}

func indexServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testIndex))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getApp(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	a := &unstructured.Unstructured{}
	a.SetGroupVersionKind(argoApplicationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: argocdNamespace, Name: name}, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func releaseStatus(t *testing.T, c client.Client) map[string]any {
	t.Helper()
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xclusterGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster-abc12"}, xr); err != nil {
		t.Fatal(err)
	}
	status, _, _ := unstructured.NestedMap(xr.Object, "status", "release")
	return status
}

func newReconciler(c client.Client, index string, res ReleaseResolver) *ReleaseReconciler {
	at := time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)
	return &ReleaseReconciler{Reader: c, Client: c, IndexURL: index, Resolver: res, Now: func() time.Time { return at }}
}

func TestAVersionMovesEveryInstallerApplicationToTheRelease(t *testing.T) {
	objs := releaseFixtures(map[string]any{"version": "v0.5.1", "autoUpgrade": "patch"})
	c := releaseClient(objs)
	r := newReconciler(c, indexServer(t).URL, fakeResolver{})
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	op := getApp(t, c, operatorApplication)
	src := applicationSources(op)
	if rev, _, _ := unstructured.NestedString(src[0], "targetRevision"); rev != "v0.5.1" {
		t.Fatalf("operator source revision = %q", rev)
	}
	if tag, _, _ := unstructured.NestedString(src[0], "helm", "valuesObject", "image", "tag"); tag != "0.5.1" {
		t.Fatalf("operator image tag = %q", tag)
	}
	if v, _, _ := unstructured.NestedString(src[0], "helm", "valuesObject", "adminConsole", "chart", "version"); v != "0.1.1-v0.5.1" {
		t.Fatalf("admin console chart = %q", v)
	}
	if enabled, _, _ := unstructured.NestedBool(src[0], "helm", "valuesObject", "desktop", "enabled"); !enabled {
		t.Fatal("the rest of the values must stay")
	}
	// The deployments source of the same Application is not the platform's.
	if rev, _, _ := unstructured.NestedString(src[1], "targetRevision"); rev != "main" {
		t.Fatalf("deployments source revision = %q", rev)
	}
	as := applicationSources(getApp(t, c, appsetsApplication))[0]
	if rev, _, _ := unstructured.NestedString(as, "targetRevision"); rev != "v0.5.1" {
		t.Fatalf("appsets revision = %q", rev)
	}
	if v, _, _ := unstructured.NestedString(as, "helm", "valuesObject", "imageTag"); v != "0.5.1" {
		t.Fatalf("appsets imageTag = %q", v)
	}
	if v, _, _ := unstructured.NestedString(as, "helm", "valuesObject", "stage"); v != "dev" {
		t.Fatal("appsets values beyond the pins must stay")
	}
	if rev, _, _ := unstructured.NestedString(applicationSources(getApp(t, c, "cnpg"))[0], "targetRevision"); rev != "v0.5.1" {
		t.Fatalf("cnpg revision = %q", rev)
	}
	// An ApplicationSet's child follows its set, and another repository's
	// Application is not ours.
	if rev, _, _ := unstructured.NestedString(applicationSources(getApp(t, c, "tenant-x"))[0], "targetRevision"); rev == "v0.5.1" {
		t.Fatal("an ApplicationSet's child must not be patched")
	}
	if rev, _, _ := unstructured.NestedString(applicationSources(getApp(t, c, "deployments-thing"))[0], "targetRevision"); rev != "main" {
		t.Fatal("another repository's Application must not be patched")
	}
	status := releaseStatus(t, c)
	if status["state"] != releaseApplied || status["desired"] != "v0.5.1" || status["revision"] != "v0.5.1" || status["image"] != "0.5.1" || status["appliedAt"] != "2026-11-09T03:00:00Z" {
		t.Fatalf("status.release = %v", status)
	}
	// patch mode: nothing newer on the line, v0.6.0 waits for a person.
	if _, ok := status["available"]; ok || status["pending"] != "v0.6.0" || status["pendingNotes"] != "https://example.org/v0.6.0" {
		t.Fatalf("status.release offers = %v", status)
	}
}

func TestAVersionTheIndexDoesNotListIsRefused(t *testing.T) {
	objs := releaseFixtures(map[string]any{"version": "v0.9.9"})
	c := releaseClient(objs)
	if err := newReconciler(c, indexServer(t).URL, fakeResolver{}).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rev, _, _ := unstructured.NestedString(applicationSources(getApp(t, c, "cnpg"))[0], "targetRevision"); rev == "v0.9.9" {
		t.Fatal("a refused version must move nothing")
	}
	if status := releaseStatus(t, c); status["state"] != releaseRefused {
		t.Fatalf("status.release = %v", status)
	}
}

func TestATrackFollowsTheBranchOnceItsBuildIsPublished(t *testing.T) {
	objs := releaseFixtures(map[string]any{"track": "develop"})
	c := releaseClient(objs)
	head := "89abcdef0123456789abcdef0123456789abcdef"
	res := fakeResolver{head: head, images: map[string]bool{}, charts: map[string]string{
		"ghcr.io/gentian-org/charts/gentian-portal:0.1.0-develop": "0.1.0-develop.89abcde",
		"ghcr.io/gentian-org/charts/admin-console:0.1.1-develop":  "0.1.1-develop.89abcde",
		"ghcr.io/gentian-org/charts/concierge:0.1.0-develop":      "0.1.0-develop.89abcde",
	}}
	r := newReconciler(c, indexServer(t).URL, res)
	// Not built yet: the cluster waits where it is.
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := releaseStatus(t, c); status["state"] != releasePending || status["desired"] != "develop" {
		t.Fatalf("status.release before the build = %v", status)
	}
	if rev, _, _ := unstructured.NestedString(applicationSources(getApp(t, c, "cnpg"))[0], "targetRevision"); rev == head {
		t.Fatal("an unpublished build must move nothing")
	}
	// Built: everything follows the commit.
	res.images["ghcr.io/gentian-org/gentian-os:develop-89abcde"] = true
	r.Resolver = res
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	src := applicationSources(getApp(t, c, operatorApplication))[0]
	if rev, _, _ := unstructured.NestedString(src, "targetRevision"); rev != head {
		t.Fatalf("operator revision = %q", rev)
	}
	if tag, _, _ := unstructured.NestedString(src, "helm", "valuesObject", "image", "tag"); tag != "develop-89abcde" {
		t.Fatalf("operator image tag = %q", tag)
	}
	if v, _, _ := unstructured.NestedString(src, "helm", "valuesObject", "desktop", "chart", "version"); v != "0.1.0-develop.89abcde" {
		t.Fatalf("desktop chart = %q", v)
	}
	status := releaseStatus(t, c)
	if status["state"] != releaseApplied || status["revision"] != head || status["image"] != "develop-89abcde" {
		t.Fatalf("status.release = %v", status)
	}
	if _, ok := status["pending"]; ok {
		t.Fatal("a track has no pending release")
	}
}

func TestAClaimWithoutAReleaseIsUnmanagedButToldWhatExists(t *testing.T) {
	objs := releaseFixtures(nil)
	c := releaseClient(objs)
	if err := newReconciler(c, indexServer(t).URL, fakeResolver{}).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := releaseStatus(t, c)
	if status["state"] != releaseUnmanaged || status["pending"] == "" {
		t.Fatalf("status.release = %v", status)
	}
	if rev, _, _ := unstructured.NestedString(applicationSources(getApp(t, c, "cnpg"))[0], "targetRevision"); rev != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatal("an unmanaged cluster must move nothing")
	}
}

func TestTheReleaseStatusIsReplacedNotMerged(t *testing.T) {
	objs := releaseFixtures(map[string]any{"version": "v0.5.1", "autoUpgrade": "patch"})
	c := releaseClient(objs)
	r := newReconciler(c, indexServer(t).URL, fakeResolver{})
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if releaseStatus(t, c)["pending"] != "v0.6.0" {
		t.Fatal("v0.6.0 should be pending")
	}
	// The index moves on: v0.6.0 is now in place from 0.5 and the mode is minor.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("releases:\n  - version: v0.5.1\n    line: \"0.5\"\n  - version: v0.6.0\n    line: \"0.6\"\n    upgradesFrom: [\"0.5\"]\n"))
	}))
	defer srv.Close()
	r.IndexURL = srv.URL
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(clusterClaimGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: clusterConfigNamespace, Name: "cluster"}, claim); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(claim.Object, "minor", "spec", "release", "autoUpgrade")
	if err := c.Update(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := releaseStatus(t, c)
	if status["available"] != "v0.6.0" {
		t.Fatalf("available = %v", status)
	}
	if _, ok := status["pending"]; ok {
		t.Fatalf("pending must be gone once the mode takes the release: %v", status)
	}
	// And what the composition wrote is still there.
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xclusterGVK)
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster-abc12"}, xr)
	if ns, _, _ := unstructured.NestedStringSlice(xr.Object, "status", "kernelNamespaces"); len(ns) != 1 {
		t.Fatalf("status.kernelNamespaces = %v", ns)
	}
}
