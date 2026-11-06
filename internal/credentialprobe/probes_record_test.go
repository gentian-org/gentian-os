/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package credentialprobe

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// recordedProbes are the probe statuses every reader of a probe is held to.
var recordedProbes = map[string]map[string]any{
	"no status":          nil,
	"no conditions":      {"conditions": []any{}},
	"other condition":    {"conditions": []any{map[string]any{"type": "Synced", "status": "True"}}},
	"ready":              {"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": "2026-01-02T03:04:05Z"}}},
	"not ready, message": {"conditions": []any{map[string]any{"type": "Ready", "status": "False", "message": "the key is missing"}}},
	"not ready, silent":  {"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}},
	"unknown":            {"conditions": []any{map[string]any{"type": "Ready", "status": "Unknown", "message": "pending"}}},
	"ready second":       {"conditions": []any{map[string]any{"type": "Synced", "status": "False"}, map[string]any{"type": "Ready", "status": "True"}}},
	"malformed":          {"conditions": "x"},
}

// What Satisfied says of each recorded probe, held to
// testdata/satisfied.txt.
func TestSatisfiedTranscript(t *testing.T) {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(secrets.ExternalSecretGVK, &unstructured.Unstructured{})
	var out []string
	for name, status := range recordedProbes {
		es := &unstructured.Unstructured{}
		es.SetGroupVersionKind(secrets.ExternalSecretGVK)
		es.SetNamespace(meta.OperatorNamespace)
		es.SetName("credreq-req")
		if status != nil {
			es.Object["status"] = status
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects([]client.Object{es}...).Build()
		out = append(out, fmt.Sprintf("%s: %v", name, Satisfied(context.Background(), c, "req")))
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	out = append(out, fmt.Sprintf("absent: %v", Satisfied(context.Background(), c, "req")))
	sort.Strings(out)
	got := strings.Join(out, "\n") + "\n"
	const path = "testdata/satisfied.txt"
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
