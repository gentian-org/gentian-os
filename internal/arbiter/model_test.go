/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"encoding/json"
	"strings"
	"testing"
)

const companyModel = `model
  schema 1.1
type user
type company
  relations
    define admin: [user]
    define member: [user] or admin
`

func TestAModelCompilesToTheJSONOpenFGATakes(t *testing.T) {
	compiled, err := Compile(companyModel)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		SchemaVersion   string           `json:"schema_version"`
		TypeDefinitions []map[string]any `json:"type_definitions"`
	}
	if err := json.Unmarshal(compiled, &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != "1.1" || len(m.TypeDefinitions) != 2 {
		t.Fatalf("compiled = %s", compiled)
	}
	// The same model is the same bytes, and so the same digest.
	again, _ := Compile(companyModel)
	if ModelDigest(again) != ModelDigest(compiled) {
		t.Fatal("one model compiled twice has two digests")
	}
	other, _ := Compile(strings.Replace(companyModel, "or admin", "", 1))
	if ModelDigest(other) == ModelDigest(compiled) {
		t.Fatal("two models have one digest")
	}
}

func TestAModelThatDoesNotCompileIsRefused(t *testing.T) {
	for _, dsl := range []string{
		"",
		"model\n  schema 1.1\n",
		"model\n  schema 1.1\ntype company\n  relations\n    define admin [user]\n",
		"this is not a model",
	} {
		if _, err := Compile(dsl); err == nil {
			t.Errorf("compiled: %q", dsl)
		}
	}
}
