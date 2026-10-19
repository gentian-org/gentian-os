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
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/internal/platformversion"
)

// PlatformStatusWriter writes which platform this cluster runs onto the
// Cluster claim's status: the version line and the build of the operator
// that is running. From the operator's own binary, because nothing else on
// the cluster knows it -- the release lives in the installer's settings and
// in an Argo CD Application's tag, neither of which a person or a program
// on the cluster reads to answer "what version is this". The claim is what
// they read, and the director answers it from there.
//
// Written on the composite the claim names (spec.resourceRef), because a
// claim's status is Crossplane's copy of its composite's; written at start
// and then on a timer, because the composition writes the composite's status
// too and may one day replace it whole.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=xclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=xclusters/status,verbs=get;update;patch
type PlatformStatusWriter struct {
	// Reader reads the claim and its composite, uncached: one get each, on
	// a timer.
	Reader client.Reader
	// Writer patches the composite's status.
	Writer client.Client
	// Build is the commit the operator was built from, or its release.
	Build string
	// Interval is how often the status is re-asserted; the default is
	// ten minutes.
	Interval time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// ReleaseFeed is where the platform's releases are published, as the
	// GitHub releases API answers: the newest one is written on the status
	// beside the version this cluster runs, so a person sees that a newer
	// release exists without leaving the cluster. "" asks nothing.
	ReleaseFeed string
	// HTTP fetches the feed; nil is a client with a short timeout.
	HTTP *http.Client

	last latestRelease
}

// latestRelease is what the feed said last, kept so a feed that is down
// does not blank what a person already saw.
type latestRelease struct {
	Version, NotesURL string
}

const defaultPlatformStatusInterval = 10 * time.Minute

// NeedLeaderElection keeps one replica writing.
func (w *PlatformStatusWriter) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It returns only when ctx ends.
func (w *PlatformStatusWriter) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("platform-status")
	interval := w.Interval
	if interval <= 0 {
		interval = defaultPlatformStatusInterval
	}
	for {
		if err := w.WriteOnce(ctx); err != nil {
			logger.Error(err, "the platform's version could not be written on the Cluster claim; it is retried")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// WriteOnce writes the status on every Cluster claim's composite. A claim
// whose composite does not exist yet is skipped and written on the next
// pass.
func (w *PlatformStatusWriter) WriteOnce(ctx context.Context) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: clusterClaimGVK.Group, Version: clusterClaimGVK.Version, Kind: clusterClaimGVK.Kind + "List",
	})
	if err := w.Reader.List(ctx, list, client.InNamespace(clusterConfigNamespace)); err != nil {
		return fmt.Errorf("list Cluster claims: %w", err)
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	for i := range list.Items {
		claim := &list.Items[i]
		ref, found, err := unstructured.NestedStringMap(claim.Object, "spec", "resourceRef")
		if err != nil || !found || ref["name"] == "" || ref["kind"] == "" || ref["apiVersion"] == "" {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref["apiVersion"])
		if err != nil {
			continue
		}
		xr := &unstructured.Unstructured{}
		xr.SetGroupVersionKind(gv.WithKind(ref["kind"]))
		if err := w.Reader.Get(ctx, types.NamespacedName{Name: ref["name"]}, xr); err != nil {
			return fmt.Errorf("read the composite %s of Cluster claim %s: %w", ref["name"], claim.GetName(), err)
		}
		platform := map[string]any{
			"versionLine": platformversion.Line,
			"build":       w.Build,
			"reportedAt":  now().UTC().Format(time.RFC3339),
		}
		if latest := w.latest(ctx); latest.Version != "" {
			platform["latestRelease"] = latest.Version
			platform["latestReleaseNotes"] = latest.NotesURL
		}
		patch, _ := json.Marshal(map[string]any{"status": map[string]any{"platform": platform}})
		if err := w.Writer.Status().Patch(ctx, xr, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return fmt.Errorf("write the platform's version on %s: %w", ref["name"], err)
		}
	}
	return nil
}

// latest reads the newest release from the feed. A feed that cannot be read
// is logged and leaves the last answer standing.
func (w *PlatformStatusWriter) latest(ctx context.Context) latestRelease {
	if w.ReleaseFeed == "" {
		return latestRelease{}
	}
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.ReleaseFeed, nil)
	if err != nil {
		return w.last
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := client.Do(req)
	if err != nil {
		log.FromContext(ctx).V(1).Info("the release feed could not be read", "feed", w.ReleaseFeed, "error", err.Error())
		return w.last
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return w.last
	}
	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&release); err != nil || release.TagName == "" {
		return w.last
	}
	w.last = latestRelease{Version: strings.TrimPrefix(release.TagName, "v"), NotesURL: release.HTMLURL}
	return w.last
}
