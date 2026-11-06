/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// The schema a bundle is defaulted with, as its digest, held to
// testdata/profile-schema.txt.
func TestProfileSchemaTranscript(t *testing.T) {
	s, err := profileSchema()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:]) + "\n"
	const path = "testdata/profile-schema.txt"
	if os.Getenv("UPDATE_TRANSCRIPTS") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // A test fixture.
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // A test fixture.
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("the schema differs from the recorded one: %s, want %s", got, want)
	}
}
