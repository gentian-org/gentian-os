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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/platformversion"
)

func TestThePlatformsVersionIsWrittenOnTheClaimsComposite(t *testing.T) {
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gentianos.io/v1alpha1", "kind": "Cluster",
		"metadata": map[string]any{"name": "cluster", "namespace": clusterConfigNamespace},
		"spec": map[string]any{"resourceRef": map[string]any{
			"apiVersion": "gentianos.io/v1alpha1", "kind": "XCluster", "name": "cluster-abc12",
		}},
	}}
	xr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gentianos.io/v1alpha1", "kind": "XCluster",
		"metadata": map[string]any{"name": "cluster-abc12"},
		"status":   map[string]any{"kernelNamespaces": []any{"kernel-control"}},
	}}
	// A claim whose composite is not there yet is skipped, not an error.
	orphan := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gentianos.io/v1alpha1", "kind": "Cluster",
		"metadata": map[string]any{"name": "young", "namespace": clusterConfigNamespace},
		"spec":     map[string]any{},
	}}
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).
		WithRuntimeObjects(claim, xr, orphan).WithStatusSubresource(xr).Build()
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.6.0","html_url":"https://example.org/releases/v0.6.0"}`))
	}))
	defer feed.Close()
	w := &PlatformStatusWriter{Reader: c, Writer: c, Build: "abc1234", Now: func() time.Time { return at }, ReleaseFeed: feed.URL}
	if err := w.WriteOnce(context.Background()); err != nil {
		t.Fatalf("WriteOnce: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(xr.GroupVersionKind())
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster-abc12"}, got); err != nil {
		t.Fatal(err)
	}
	platform, _, _ := unstructured.NestedStringMap(got.Object, "status", "platform")
	if platform["versionLine"] != platformversion.Line || platform["build"] != "abc1234" || platform["reportedAt"] != "2026-10-19T12:00:00Z" {
		t.Fatalf("status.platform = %v", platform)
	}
	// And what the feed says is newest, beside it.
	if platform["latestRelease"] != "0.6.0" || platform["latestReleaseNotes"] != "https://example.org/releases/v0.6.0" {
		t.Fatalf("status.platform's latest release = %v", platform)
	}
	// What the composition wrote is still there: a merge, not a replacement.
	if ns, _, _ := unstructured.NestedStringSlice(got.Object, "status", "kernelNamespaces"); len(ns) != 1 {
		t.Fatalf("status.kernelNamespaces = %v", ns)
	}
}
