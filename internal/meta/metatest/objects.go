/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package metatest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// Objects is a transcript of built objects: for each, by name, the digest of
// the whole object and every security context it holds, in the order they
// stand in its JSON.
func Objects(t *testing.T, objs map[string]any) string {
	t.Helper()
	var out []string
	for name, obj := range objs {
		raw, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		var tree any
		if err := json.Unmarshal(raw, &tree); err != nil {
			t.Fatal(err)
		}
		var contexts []string
		securityContexts(tree, "", &contexts)
		line := name + ": " + hex.EncodeToString(sum[:8])
		for _, c := range contexts {
			line += "\n  " + c
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return strings.Join(out, "\n") + "\n"
}

func securityContexts(node any, path string, out *[]string) {
	switch n := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "securityContext" {
				raw, _ := json.Marshal(n[k])
				*out = append(*out, path+"."+k+" "+string(raw))
				continue
			}
			securityContexts(n[k], path+"."+k, out)
		}
	case []any:
		for i, v := range n {
			securityContexts(v, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// Hold compares got with the transcript at path, or writes it there when
// UPDATE_TRANSCRIPTS is set.
func Hold(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("UPDATE_TRANSCRIPTS") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // A test fixture.
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // A test fixture.
	if err != nil {
		t.Fatalf("%v (run with UPDATE_TRANSCRIPTS=1 to record)", err)
	}
	if string(want) != got {
		t.Fatalf("the transcript differs from %s\n--- got\n%s--- want\n%s", path, got, want)
	}
}
