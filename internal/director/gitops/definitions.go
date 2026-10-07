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
	"fmt"
	"strings"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

// What this process commits is applied to the cluster by Argo CD, and a
// cluster whose resource definitions are older than this software drops, on
// apply, every field it does not know -- with the commit made, the sync green
// and nothing anywhere saying a field was lost. An addon pin dropped that way
// is an add-on installed unverified.
//
// So a commit is checked before it is made. Every write in this package ends
// in commitPaths, and that is where the check is: the staged manifests are
// compared with the ones they replace, and what the commit sets is put to
// what the operator found the cluster serves (schemacheck.Decide). There is
// no table of which route writes which field. The commit is the table.

// GuardDefinitions makes every commit conditional on the cluster serving the
// fields it sets. source answers what the cluster serves; nil is a director
// with nobody to ask, which then commits nothing that sets a field.
//
// A GitOps this was never called on checks nothing, which is what the tests
// of everything else in this package, and the local development director,
// are built on.
func (g *GitOps) GuardDefinitions(source schemacheck.Source) {
	g.definitions = &definitionsGuard{set: schemacheck.Embedded(), source: source}
}

type definitionsGuard struct {
	set    *schemacheck.Set
	source schemacheck.Source
}

// checkDefinitions refuses the staged commit when the cluster would drop part
// of it. It returns a *schemacheck.Refusal for that, and any other error when
// the staged files could not be read.
func (g *GitOps) checkDefinitions(ctx context.Context) error {
	if g.definitions == nil {
		return nil
	}
	// Added and modified files only: a deleted manifest sets nothing. No
	// rename detection, so a moved file is a deletion and an addition.
	names, cancel := g.gitCmd(ctx, "-C", g.path, "diff", "--cached", "--name-only", "-z", "--no-renames", "--diff-filter=AM")
	defer cancel()
	out, err := names.Output()
	if err != nil {
		return fmt.Errorf("git diff --cached: %w", err)
	}
	var changes []schemacheck.Change
	for _, rel := range strings.Split(string(out), "\x00") {
		if !strings.HasSuffix(rel, ".yaml") && !strings.HasSuffix(rel, ".yml") {
			continue
		}
		after, err := g.blob(ctx, ":"+rel)
		if err != nil {
			return err
		}
		// A file the commit adds has no earlier version, and asking for one
		// is an error from git that means exactly that.
		before, _ := g.blob(ctx, "HEAD:"+rel)
		found, err := g.definitions.set.Changes(before, after)
		if err != nil {
			// A manifest that does not parse is not this check's to refuse:
			// whatever wrote it has its own validation, and Argo CD will say
			// so loudly. Nothing can be read off it, so nothing is.
			continue
		}
		changes = append(changes, found...)
	}
	if refusal := schemacheck.Decide(g.definitions.set, g.definitions.source, merge(changes)); refusal != nil {
		return refusal
	}
	return nil
}

// blob reads one file from the index (":path") or a commit ("HEAD:path").
func (g *GitOps) blob(ctx context.Context, object string) ([]byte, error) {
	show, cancel := g.gitCmd(ctx, "-C", g.path, "show", object)
	defer cancel()
	var stdout bytes.Buffer
	show.Stdout = &stdout
	if err := show.Run(); err != nil {
		return nil, fmt.Errorf("git show %s: %w", object, err)
	}
	return stdout.Bytes(), nil
}

// merge folds the changes of several files into one per kind.
func merge(changes []schemacheck.Change) []schemacheck.Change {
	index := map[string]int{}
	var out []schemacheck.Change
	for _, c := range changes {
		i, ok := index[c.Kind]
		if !ok {
			index[c.Kind] = len(out)
			out = append(out, schemacheck.Change{Kind: c.Kind})
			i = len(out) - 1
		}
		seen := map[string]bool{}
		for _, f := range out[i].Fields {
			seen[f] = true
		}
		for _, f := range c.Fields {
			if !seen[f] {
				out[i].Fields = append(out[i].Fields, f)
			}
		}
	}
	return out
}
