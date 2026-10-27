/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

func auditRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("not JSON: %s", l)
		}
		if m["audit"] == true {
			out = append(out, m)
		}
	}
	return out
}

// Every commit that reached the remote is one audit record naming the
// person by subject, the request, the decision, the commit and its files.
// A write that changed nothing and a push the remote refused write none:
// neither is in the repository.
func TestEveryPushedCommitIsOneAuditRecord(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	var buf bytes.Buffer
	g.AuditTo(slog.New(slog.NewJSONHandler(&buf, nil)))
	ctx := context.Background()

	if _, err := g.Install(ctx, "demo", "element", "", meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Install(ctx, "demo", "element", "", meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	hook := remote + "/hooks/pre-receive"
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _ = g.Install(ctx, "demo", "jitsi", "", meta("u-ada"))

	recs := auditRecords(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("%d audit records, want 1: %v", len(recs), recs)
	}
	r := recs[0]
	m := meta("u-ada")
	if r["source"] != "director" || r["kind"] != "change" || r["outcome"] != "ok" {
		t.Errorf("envelope: %v", r)
	}
	if r["subject"] != m.Subject || r["request_id"] != m.RequestID {
		t.Errorf("subject %v request %v, want %q %q", r["subject"], r["request_id"], m.Subject, m.RequestID)
	}
	if rel, obj, _ := strings.Cut(m.Decision, " "); r["action"] != rel || r["object"] != obj {
		t.Errorf("action %v object %v, want from decision %q", r["action"], r["object"], m.Decision)
	}
	detail, _ := r["detail"].(map[string]any)
	if c, _ := detail["commit"].(string); len(c) != 40 {
		t.Errorf("commit = %v", detail["commit"])
	}
	if files, _ := detail["files"].([]any); len(files) == 0 {
		t.Errorf("files = %v", detail["files"])
	}
	for _, k := range []string{"email", "name", "author"} {
		if _, ok := r[k]; ok {
			t.Errorf("record carries %s", k)
		}
	}
}
