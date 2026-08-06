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

package controller

import (
	"testing"
	"time"
)

// TestParseLadderDateAcceptsYAMLNormalizedForm guards against a real failure
// seen live: a bare "2027-08-06" in a Customization record's YAML is parsed by
// sigs.k8s.io/yaml as a timestamp and normalized to "2027-08-06T00:00:00Z"
// before it reaches the API server, which then fails a strict
// ^\d{4}-\d{2}-\d{2}$ pattern. The CRD pattern was widened to accept both
// forms; this locks the Go-side parser in step with it.
func TestParseLadderDateAcceptsYAMLNormalizedForm(t *testing.T) {
	want := time.Date(2027, 8, 6, 0, 0, 0, 0, time.UTC)

	bare, err := parseLadderDate("2027-08-06")
	if err != nil || !bare.Equal(want) {
		t.Fatalf("bare date: got %v, %v; want %v", bare, err, want)
	}

	normalized, err := parseLadderDate("2027-08-06T00:00:00Z")
	if err != nil || !normalized.Equal(want) {
		t.Fatalf("YAML-normalized date: got %v, %v; want %v", normalized, err, want)
	}
}

func TestParseLadderDateRejectsGarbage(t *testing.T) {
	if _, err := parseLadderDate("not-a-date"); err == nil {
		t.Fatal("expected an error for an unparseable date")
	}
}
