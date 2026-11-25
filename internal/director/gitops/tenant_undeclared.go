/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"
)

// Undeclared names the fields of an object the cluster's schema does not
// declare, each by its path (schemacheck.(*Set).Undeclared).
type Undeclared func(obj map[string]any) ([]string, error)

// TakeOutUndeclared takes out of every tenant's file the fields the
// platform's schema does not declare: what an earlier release wrote and this
// one no longer has. A tenant is applied server-side, and the API server
// refuses an applied object that names a field its schema does not know, so
// while such a field is in the file nothing of the tenant is applied.
//
// A field is taken out as text, with the lines of its value and the comment
// right above it, so the rest of the file stays as it was written. A field
// that cannot be taken out that way (one written in braces, or the first of a
// list entry) is left and named.
//
// It returns, by file, the fields taken out and the fields left; nothing is
// committed where nothing is taken out.
func (g *GitOps) TakeOutUndeclared(ctx context.Context, undeclared Undeclared, meta Meta) (res Result, taken, left map[string][]string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for started, attempt := time.Now(), 1; ; attempt++ {
		if err := g.ensureRepo(ctx); err != nil {
			return Result{}, nil, nil, err
		}
		files, _ := filepath.Glob(filepath.Join(g.path, "clusters", g.cluster, "tenants", "*", "tenant.yaml"))
		taken, left = map[string][]string{}, map[string][]string{}
		var rels []string
		for _, file := range files {
			raw, err := os.ReadFile(file) //nolint:gosec // A tenant's file of the deployments repository.
			if err != nil {
				return Result{}, nil, nil, err
			}
			var obj map[string]any
			if err := yaml.Unmarshal(raw, &obj); err != nil || obj["kind"] != "Tenant" {
				// What does not parse, or is no Tenant, is not this pass's to judge.
				continue
			}
			paths, err := undeclared(obj)
			if err != nil {
				return Result{}, nil, nil, err
			}
			if len(paths) == 0 {
				continue
			}
			rel, err := filepath.Rel(g.path, file)
			if err != nil {
				return Result{}, nil, nil, err
			}
			next, out, kept := takeOutFields(string(raw), paths)
			if len(kept) > 0 {
				left[rel] = kept
			}
			if len(out) == 0 {
				continue
			}
			if err := os.WriteFile(file, []byte(next), 0o644); err != nil { //nolint:gosec // As above.
				return Result{}, nil, nil, err
			}
			taken[rel] = out
			rels = append(rels, rel)
		}
		if len(rels) == 0 {
			return Result{Status: "unchanged"}, nil, left, nil
		}
		var lines []string
		for _, rel := range rels {
			lines = append(lines, fmt.Sprintf("%s: %s", rel, strings.Join(taken[rel], ", ")))
		}
		msg := "fix(tenants): take out the fields this platform's schema does not declare\n\n" +
			"A tenant naming one is refused when it is applied, and nothing of it changes.\n\n" +
			strings.Join(lines, "\n")
		err := g.commitPaths(ctx, rels, msg, meta)
		if err == nil {
			res, err := g.landed(ctx, "removed")
			return res, taken, left, err
		}
		if !errors.Is(err, errPushRejected) {
			return Result{}, nil, nil, err
		}
		if err := retryPush(ctx, started, attempt); err != nil {
			return Result{}, nil, nil, err
		}
	}
}

// takeOutFields removes from a YAML text the fields at paths, each with the
// lines of its value and the comment lines right above it. It returns the
// text, the paths it took out and the paths it left: one that is not there,
// one in a mapping written in braces, and one that opens a list entry.
func takeOutFields(text string, paths []string) (next string, taken, left []string) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(text), &doc); err != nil || len(doc.Content) == 0 {
		return text, nil, paths
	}
	lines := strings.SplitAfter(text, "\n")
	type span struct{ from, to int } // lines, from 1, both included
	var spans []span
	for _, path := range paths {
		key, value, parent := fieldAt(doc.Content[0], path)
		if key == nil || parent.Style&yamlv3.FlowStyle != 0 || key.Line < 1 || key.Line > len(lines) ||
			strings.HasPrefix(strings.TrimSpace(lines[key.Line-1]), "- ") {
			left = append(left, path)
			continue
		}
		from := key.Line
		for from > 1 && strings.HasPrefix(strings.TrimSpace(lines[from-2]), "#") &&
			indentOf(lines[from-2]) == indentOf(lines[key.Line-1]) {
			from--
		}
		spans = append(spans, span{from: from, to: min(lastLine(value), len(lines))})
		taken = append(taken, path)
	}
	if len(spans) == 0 {
		return text, nil, left
	}
	drop := map[int]bool{}
	for _, s := range spans {
		for n := s.from; n <= s.to; n++ {
			drop[n] = true
		}
	}
	var b strings.Builder
	for i, line := range lines {
		if !drop[i+1] {
			b.WriteString(line)
		}
	}
	sort.Strings(taken)
	return b.String(), taken, left
}

// fieldAt finds the key and the value at a path such as "spec.apps[2].name"
// below a mapping, and the mapping that holds them.
func fieldAt(node *yamlv3.Node, path string) (key, value, parent *yamlv3.Node) {
	steps := strings.Split(path, ".")
	for i, step := range steps {
		name, index, indexed := strings.Cut(step, "[")
		if node.Kind != yamlv3.MappingNode {
			return nil, nil, nil
		}
		key, value = nil, nil
		for n := 0; n+1 < len(node.Content); n += 2 {
			if node.Content[n].Value == name {
				key, value = node.Content[n], node.Content[n+1]
				break
			}
		}
		if key == nil {
			return nil, nil, nil
		}
		if i == len(steps)-1 && !indexed {
			return key, value, node
		}
		node = value
		for indexed {
			var rest string
			index, rest, _ = strings.Cut(index, "]")
			at, err := strconv.Atoi(index)
			if err != nil || node.Kind != yamlv3.SequenceNode || at < 0 || at >= len(node.Content) {
				return nil, nil, nil
			}
			node = node.Content[at]
			index, indexed = strings.CutPrefix(rest, "[")
		}
	}
	// The path ends at a list entry, which is not a field.
	return nil, nil, nil
}

// lastLine is the last line a node's text takes.
func lastLine(node *yamlv3.Node) int {
	last := node.Line
	if node.Kind == yamlv3.ScalarNode && node.Style&(yamlv3.LiteralStyle|yamlv3.FoldedStyle) != 0 {
		last += strings.Count(strings.TrimRight(node.Value, "\n"), "\n") + 1
	}
	for _, child := range node.Content {
		last = max(last, lastLine(child))
	}
	return last
}
