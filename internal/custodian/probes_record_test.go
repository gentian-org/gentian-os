/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// What the custodian reads from each probe status, held to
// testdata/ready-condition.txt.
func TestReadyConditionTranscript(t *testing.T) {
	probes := map[string]map[string]any{
		"no status":          nil,
		"no conditions":      {"conditions": []any{}},
		"other condition":    {"conditions": []any{map[string]any{"type": "Synced", "status": "True"}}},
		"ready":              {"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": "2026-01-02T03:04:05Z"}}},
		"not ready, message": {"conditions": []any{map[string]any{"type": "Ready", "status": "False", "message": "the key is missing", "lastTransitionTime": "2026-01-02T03:04:05Z"}}},
		"not ready, silent":  {"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}},
		"unknown":            {"conditions": []any{map[string]any{"type": "Ready", "status": "Unknown", "message": "pending"}}},
		"ready second":       {"conditions": []any{map[string]any{"type": "Synced", "status": "False"}, map[string]any{"type": "Ready", "status": "True"}}},
		"malformed":          {"conditions": "x"},
		"bad time":           {"conditions": []any{map[string]any{"type": "Ready", "status": "False", "lastTransitionTime": "yesterday"}}},
	}
	var out []string
	for name, status := range probes {
		es := &unstructured.Unstructured{Object: map[string]any{}}
		if status != nil {
			es.Object["status"] = status
		}
		ok, since, reason := readyCondition(es)
		out = append(out, fmt.Sprintf("%s: ready=%v since=%s reason=%q", name, ok, since.UTC().Format("2006-01-02T15:04:05Z"), reason))
	}
	sort.Strings(out)
	got := strings.Join(out, "\n") + "\n"
	const path = "testdata/ready-condition.txt"
	if os.Getenv("UPDATE_TRANSCRIPTS") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // A test fixture.
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("the transcript differs from %s\n--- got\n%s--- want\n%s", path, got, want)
	}
}
