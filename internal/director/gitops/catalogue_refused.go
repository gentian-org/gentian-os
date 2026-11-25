/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Refuser says why the cluster's schema would not take an object, or nothing
// where it would (schemacheck.(*Set).Refused).
type Refuser func(obj map[string]any) ([]string, error)

// TakeOutRefused takes out of the catalog directory every profile and every
// build the platform's schema now refuses: what an earlier release wrote in a
// shape this one no longer has -- a class or a field it removed. Such a file
// can never be applied again, and while it is listed Argo CD's diff of the
// whole directory fails, so nothing else in it is applied either.
//
// Only git changes. The objects stay on the cluster, as the Application that
// applies the directory does not prune: an app a tenant still has installed
// keeps running from what it has, and a profile whose data a tenant retains
// stays where the purge of that data finds it. Removing them from the cluster
// is the unused-profile removal's, which asks the operator, as for any
// profile.
//
// It returns, by file, why each was taken out; nothing is committed where
// nothing is refused.
func (g *GitOps) TakeOutRefused(ctx context.Context, refused Refuser, meta Meta) (Result, map[string][]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for started, attempt := time.Now(), 1; ; attempt++ {
		if err := g.ensureRepo(ctx); err != nil {
			return Result{}, nil, err
		}
		dir := filepath.Join(g.path, "clusters", g.cluster, CatalogueDir)
		kustomization := filepath.Join(dir, "kustomization.yaml")
		listed, err := os.ReadFile(kustomization)
		if os.IsNotExist(err) {
			return Result{Status: "unchanged"}, nil, nil
		}
		if err != nil {
			return Result{}, nil, err
		}

		why := map[string][]string{}
		next := string(listed)
		var remove []string
		profiles, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
		builds, _ := filepath.Glob(filepath.Join(dir, BuildsDir, "*.yaml"))
		for _, file := range append(profiles, builds...) {
			base := filepath.Base(file)
			if base == "kustomization.yaml" || strings.HasSuffix(base, ".bundle.yaml") {
				continue
			}
			reasons, err := refusedIn(file, refused)
			if err != nil {
				return Result{}, nil, err
			}
			if len(reasons) == 0 {
				continue
			}
			rel, _ := filepath.Rel(dir, file)
			why[rel] = reasons
			remove = append(remove, file)
			next, _ = removeListed(next, filepath.ToSlash(rel), listResource)
			if filepath.Dir(file) == dir {
				// A profile's carrier goes with it.
				name := strings.TrimSuffix(base, ".yaml")
				next, _ = removeListed(next, BundleFile(name), listPatch)
				remove = append(remove, filepath.Join(dir, BundleFile(name)))
			}
		}
		if len(why) == 0 {
			return Result{Status: "unchanged"}, nil, nil
		}
		next = closeEmptyLists(next)

		var changed []string
		for _, file := range remove {
			err := os.Remove(file)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return Result{}, nil, err
			}
			changed = append(changed, file)
		}
		if next != string(listed) {
			if err := os.WriteFile(kustomization, []byte(next), 0o644); err != nil {
				return Result{}, nil, err
			}
			changed = append(changed, kustomization)
		}
		rels := make([]string, 0, len(changed))
		for _, file := range changed {
			rel, err := filepath.Rel(g.path, file)
			if err != nil {
				return Result{}, nil, err
			}
			rels = append(rels, rel)
		}
		names := make([]string, 0, len(why))
		for f := range why {
			names = append(names, f)
		}
		sort.Strings(names)
		msg := fmt.Sprintf("fix(catalog): take out what this platform's schema refuses\n\n%s can never be applied again; "+
			"while listed, Argo CD's diff of the directory fails. The objects stay on the cluster.", strings.Join(names, ", "))
		err = g.commitPaths(ctx, rels, msg, meta)
		if err == nil {
			res, err := g.landed(ctx, "removed")
			return res, why, err
		}
		if !errors.Is(err, errPushRejected) {
			return Result{}, nil, err
		}
		if err := retryPush(ctx, started, attempt); err != nil {
			return Result{}, nil, err
		}
	}
}

// refusedIn is why the schema refuses the file's ComponentProfile or
// ProfileBuild, or nothing for a file that holds neither or that it takes.
func refusedIn(file string, refused Refuser) ([]string, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // A file of the catalog directory.
	if err != nil {
		return nil, err
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []string
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			// What does not parse is not this pass's to judge.
			return nil, nil
		}
		kind, _ := obj["kind"].(string)
		if kind != "ComponentProfile" && kind != "ProfileBuild" {
			continue
		}
		reasons, err := refused(obj)
		if err != nil {
			return nil, err
		}
		out = append(out, reasons...)
	}
}
