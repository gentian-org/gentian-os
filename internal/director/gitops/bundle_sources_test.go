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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// A source is a BundleSource of its own in the cluster's claims, which the
// definition reads back as declared -- digits-only values included, which
// left bare would be numbers -- and removing it is removing that file.
func TestABundleSourceIsAFileOfTheClustersClaims(t *testing.T) {
	remote := dt.Remote(t, "demo")
	checkout := dt.Clone(t, remote)
	g := gitops.NewGitOps(checkout, remote, dt.Cluster, director)
	ctx := context.Background()
	decl := gitops.BundleSourceDeclaration{Endpoint: "https://sos-ch-gva-2.exo.io", Bucket: "old-cluster",
		Prefix: "2024", Region: "ch-gva-2", AddressingStyle: "virtualHost", Description: "Die alte Ablage: 0.4"}
	res, err := g.DeclareBundleSource(ctx, "old", decl, repoMeta())
	if err != nil || !res.Changed {
		t.Fatalf("declare: %+v, %v", res, err)
	}
	raw, err := os.ReadFile(filepath.Join(checkout, "clusters", dt.Cluster, "kernel", "claims", "bundle-source-old.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var obj gentianov1alpha1.BundleSource
	if err := yaml.UnmarshalStrict(raw, &obj); err != nil {
		t.Fatalf("the file is not a BundleSource: %v\n%s", err, raw)
	}
	if obj.Kind != "BundleSource" || obj.Name != "old" || obj.Spec.Prefix != "2024" || obj.Spec.Description != decl.Description ||
		obj.Spec.AddressingStyle != gentianov1alpha1.S3AddressingVirtualHost {
		t.Errorf("declared %+v", obj)
	}
	listed, err := g.BundleSources(ctx)
	if err != nil || len(listed) != 1 || listed[0].Name != "old" || listed[0].Bucket != "old-cluster" {
		t.Fatalf("listed %+v, %v", listed, err)
	}

	if _, err := g.RemoveBundleSource(ctx, "old", repoMeta()); err != nil {
		t.Fatal(err)
	}
	if listed, _ := g.BundleSources(ctx); len(listed) != 0 {
		t.Errorf("still listed after removal: %+v", listed)
	}
	if _, err := g.RemoveBundleSource(ctx, "old", repoMeta()); !errors.Is(err, gitops.ErrBundleSourceNotFound) {
		t.Errorf("removing it twice answered %v", err)
	}
}

// What the definition would refuse is refused before it is committed, and
// an address with a login in it is refused outright.
func TestABundleSourceTheDefinitionWouldRefuseIsNotCommitted(t *testing.T) {
	ok := gitops.BundleSourceDeclaration{Endpoint: "https://s3.example", Bucket: "bundles"}
	if err := gitops.CheckBundleSource("old", &ok); err != nil {
		t.Fatalf("a plain source was refused: %v", err)
	}
	for name, d := range map[string]gitops.BundleSourceDeclaration{
		"login":     {Endpoint: "https://key:secret@s3.example", Bucket: "bundles"},
		"scheme":    {Endpoint: "s3.example", Bucket: "bundles"},
		"bucket":    {Endpoint: "https://s3.example", Bucket: "B"},
		"prefix":    {Endpoint: "https://s3.example", Bucket: "bundles", Prefix: "../up"},
		"style":     {Endpoint: "https://s3.example", Bucket: "bundles", AddressingStyle: "dns"},
		"multiline": {Endpoint: "https://s3.example", Bucket: "bundles", Description: "a\nb"},
	} {
		if err := gitops.CheckBundleSource("old", &d); !errors.Is(err, gitops.ErrInvalidBundleSource) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := gitops.CheckBundleSource("Old_Store", &ok); err == nil {
		t.Error("a name that is not a DNS label was accepted")
	}
	if err := gitops.CheckBundleSource(strings.Repeat("a", 49), &ok); err == nil {
		t.Error("a name longer than the definition allows was accepted")
	}
}
