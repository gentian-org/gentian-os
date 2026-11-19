/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops_test

import (
	"context"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// A profile the schema refuses is taken out of the catalog directory with its
// carrier and its entries, as one commit by the platform; what the schema
// takes stays; a second pass finds nothing and commits nothing.
func TestWhatTheSchemaRefusesIsTakenOut(t *testing.T) {
	remote := dt.Remote(t, "demo")
	dt.Commit(t, remote, map[string]string{dt.CataloguePath("kustomization.yaml"): emptyKustomization})
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	for _, name := range []string{"old-adapter", "notes"} {
		body := []byte(dt.ProfileYAML(name))
		if _, err := g.MaterialiseProfile(ctx, name, profilebundle.Digest(body), body, profilebundle.ClusterOrigin("main"), meta("u-ada")); err != nil {
			t.Fatal(err)
		}
	}
	refuser := func(obj map[string]any) ([]string, error) {
		md, _ := obj["metadata"].(map[string]any)
		if name, _ := md["name"].(string); strings.HasPrefix(name, "old-adapter") {
			return []string{"spec.adapter: field not declared in schema"}, nil
		}
		return nil, nil
	}
	platform := gitops.Meta{Principal: "platform:catalogue", Decision: "this release's schema refuses them"}

	res, why, err := g.TakeOutRefused(ctx, refuser, platform)
	if err != nil || !res.Changed || len(why) != 2 || why["old-adapter.yaml"][0] != "spec.adapter: field not declared in schema" {
		t.Fatalf("take out = %+v %v, %v", res, why, err)
	}
	for _, file := range []string{"old-adapter.yaml", gitops.BundleFile("old-adapter")} {
		if inRemote(remote, dt.CataloguePath(file)) {
			t.Errorf("%s is still there", file)
		}
	}
	k := dt.RemoteFile(t, remote, dt.CataloguePath("kustomization.yaml"))
	if strings.Contains(k, "old-adapter") || !strings.Contains(k, "- notes.yaml") || !strings.Contains(k, "notes.bundle.yaml") {
		t.Fatalf("kustomization:\n%s", k)
	}
	subject := dt.Git(t, "", "--git-dir", remote, "log", "-1", "--format=%s", "main")
	if subject != "fix(catalog): take out what this platform's schema refuses" {
		t.Fatalf("commit = %q", subject)
	}

	tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")
	if res, why, err := g.TakeOutRefused(ctx, refuser, platform); err != nil || res.Changed || len(why) != 0 {
		t.Fatalf("again = %+v %v, %v", res, why, err)
	}
	if dt.Git(t, "", "--git-dir", remote, "rev-parse", "main") != tip {
		t.Fatal("a pass that found nothing committed something")
	}
}
