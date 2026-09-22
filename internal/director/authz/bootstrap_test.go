/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package authz

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
)

// These run against a real OpenFGA: make test-director-contract.
func requireOpenFGA(t *testing.T) Options {
	t.Helper()
	base := os.Getenv("DIRECTOR_TEST_OPENFGA_URL")
	if base == "" {
		t.Skip("needs OpenFGA: make test-director-contract")
	}
	return Options{BaseURL: base, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestBootstrapIsWhereTheStoreAndTheModelComeFrom(t *testing.T) {
	o := requireOpenFGA(t)
	ctx := context.Background()

	store, model, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if store == "" || model == "" {
		t.Fatalf("store=%q model=%q", store, model)
	}

	// Again: the same store, the same model. A restart must not add a version.
	store2, model2, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if store2 != store || model2 != model {
		t.Fatalf("first: store=%q model=%q\nsecond: store=%q model=%q", store, model, store2, model2)
	}

	// And the model that is there is the one this director embeds.
	c, err := newClient(o)
	if err != nil {
		t.Fatal(err)
	}
	var latest struct {
		Models []map[string]any `json:"authorization_models"`
	}
	if err := c.get(ctx, "/stores/"+store+"/authorization-models?page_size=10", &latest); err != nil {
		t.Fatal(err)
	}
	if len(latest.Models) != 1 {
		t.Fatalf("%d models in the store, want 1", len(latest.Models))
	}
	var want map[string]any
	_ = json.Unmarshal(modelV1, &want)
	if !sameModel(latest.Models[0], want) {
		t.Fatal("the store's model is not the embedded one")
	}
}

// What bootstrap produced must answer the questions model v1 defines.
func TestTheBootstrappedModelAnswersAsV1(t *testing.T) {
	o := requireOpenFGA(t)
	ctx := context.Background()
	store, model, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	o.StoreID, o.ModelID = store, model
	c, err := NewOpenFGA(o)
	if err != nil {
		t.Fatal(err)
	}
	writes := []Tuple{
		{User: "user:tom", Relation: "member", Object: "group:gentian/tenant/demo/admins"},
		{User: "group:gentian/tenant/demo/admins#member", Relation: "admin", Object: "tenant:demo"},
	}
	if err := c.Write(ctx, writes, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Write(ctx, nil, writes) }()
	ok, err := c.Check(ctx, "test", "user:tom", "can_install_app", "tenant:demo")
	if err != nil || !ok {
		t.Fatalf("can_install_app = %v, %v", ok, err)
	}
	if ok, _ := c.Check(ctx, "test", "user:mia", "can_install_app", "tenant:demo"); ok {
		t.Fatal("a stranger may install")
	}
}

// The embedded model must be the file the repository tests and the lint check.
func TestTheEmbeddedModelIsTheRepositorysModel(t *testing.T) {
	onDisk, err := os.ReadFile("../../../authz/model/v1/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(onDisk, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(modelV1, &b); err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		t.Fatal("internal/director/authz/model.json differs from authz/model/v1/model.json (make gen-all copies it)")
	}
}
