/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package directortest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// What decides, in a contract test.
//
// The director and the registrar are tested the same way: the whole service
// against a static-key issuer, with the authorization question answered
// either by a table that states what model v1 should say for the fixture, or
// -- when DIRECTOR_TEST_OPENFGA_URL is set, which is what
// `make test-director-contract` does -- by a real OpenFGA holding that model
// and that fixture. The second run is what shows the table is not wishful.

// Table answers from a list of facts, each "<user> <relation> <object>".
type Table map[string]bool

// Check implements authz.Checker.
func (tb Table) Check(_ context.Context, _, user, relation, object string) (bool, error) {
	return tb[user+" "+relation+" "+object], nil
}

// Checker returns what decides: a real OpenFGA holding model v1 when one is
// configured, and otherwise the table.
func Checker(t testing.TB, facts Table) authz.Checker {
	t.Helper()
	base := os.Getenv("DIRECTOR_TEST_OPENFGA_URL")
	if base == "" {
		return facts
	}
	storeID, modelID := loadOpenFGA(t, base)
	c, err := authz.NewOpenFGA(authz.Options{
		BaseURL: base, StoreID: storeID, ModelID: modelID,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// repoRoot is the repository this file is in, so the model and its fixture
// are found from whichever package's tests are running.
func repoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// loadOpenFGA creates a store holding model v1 and the shared fixture.
func loadOpenFGA(t testing.TB, base string) (storeID, modelID string) {
	t.Helper()
	post := func(path string, body any, out any) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("openfga %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("openfga %s: %d %s", path, resp.StatusCode, raw)
		}
		if out != nil {
			_ = json.Unmarshal(raw, out)
		}
	}
	var store struct {
		ID string `json:"id"`
	}
	// OpenFGA caps a store name at 64 characters, and this repository's test
	// names run longer than that. Keeping the tail keeps the part that
	// distinguishes one test from another.
	name := t.Name()
	if len(name) > 64 {
		name = name[len(name)-64:]
	}
	post("/stores", map[string]string{"name": name}, &store)

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "authz", "model", "v1", "model.json"))
	if err != nil {
		t.Fatal(err)
	}
	var model map[string]any
	if err := json.Unmarshal(raw, &model); err != nil {
		t.Fatal(err)
	}
	var written struct {
		ID string `json:"authorization_model_id"`
	}
	post("/stores/"+store.ID+"/authorization-models", model, &written)

	raw, err = os.ReadFile(filepath.Join(repoRoot(t), "authz", "model", "v1", "tests.fga.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Tuples []map[string]any `json:"tuples"`
	}
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	tuples := fixture.Tuples
	// The fixture's cluster is cluster:main; these tests serve Cluster.
	// Bind the same platform groups to it so cluster verbs can be checked.
	for _, role := range [][2]string{{"admin", "admin"}, {"auditor", "auditor"}, {"service-admin", "service_admin"}, {"security", "security_officer"}} {
		tuples = append(tuples, map[string]any{"user": "group:gentian/platform/" + role[0] + "#member", "relation": role[1], "object": "cluster:" + Cluster})
	}
	post("/stores/"+store.ID+"/write", map[string]any{
		"authorization_model_id": written.ID,
		"writes":                 map[string]any{"tuple_keys": tuples},
	}, nil)
	return store.ID, written.ID
}
