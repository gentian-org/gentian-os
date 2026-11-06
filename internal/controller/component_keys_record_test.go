/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"encoding/json"
	"testing"

	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/componentkeys/keystest"
)

// written decodes raw into the list f writes, whatever its type, and
// returns what f writes of it.
func written[L any](t *testing.T, f func(L) (string, error), raw string) string {
	t.Helper()
	var in L
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			t.Fatal(err)
		}
	}
	out, err := f(in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// writtenLast is written for a function whose list is its last argument,
// called with nothing else.
func writtenLast[A, B, L any](t *testing.T, f func(A, B, L) (string, error), raw string) string {
	t.Helper()
	var a A
	var b B
	return written(t, func(l L) (string, error) { return f(a, b, l) }, raw)
}

// The lists the operator writes for the services that recognize a component
// by its key, byte for byte, held to testdata/component-keys.txt.
func TestComponentKeyDocumentsTranscript(t *testing.T) {
	h1, h2 := keystest.Hash("notary-key"), keystest.Hash("scribe-key")
	plain := `[{"tenant":"demo","component":"notary","keyHash":"` + h1 + `"},{"tenant":"solo","component":"scribe","keyHash":"` + h2 + `"}]`
	commission := `[{"tenant":"demo","component":"notary","keyHash":"` + h1 + `","tenants":["demo","solo"]}]`
	checkers := `[{"tenant":"demo","component":"notary","keyHash":"` + h1 + `"},` +
		`{"tenant":"platform","component":"evidence","keyHash":"` + h2 + `","audit":{"cluster":"main","realm":"kernel"}}]`
	stores := `[{"tenant":"demo","component":"notary","keyHash":"` + h1 + `","model":{"schema_version":"1.1"},"realm":"r","mirrorGroups":true,` +
		`"tuples":[{"user":"user:a","relation":"r","object":"o:1"}]},{"tenant":"solo","component":"scribe","keyHash":"` + h2 + `","model":{}}]`
	bouncer := func(raw string) string { return writtenLast(t, bouncerTable, raw) }
	arbiterTable := func(raw string) string {
		return written(t, func(s []arbiter.Entry) (string, error) {
			b, err := json.Marshal(arbiter.Table{Stores: s})
			return string(b), err
		}, raw)
	}
	keystest.Golden(t, "component-keys", []string{
		"registrar, none: " + written(t, registrarVouchingKeysDocument, ""),
		"registrar: " + written(t, registrarVouchingKeysDocument, plain),
		"director, none: " + written(t, directorCommissionKeysDocument, ""),
		"director: " + written(t, directorCommissionKeysDocument, commission),
		"bouncer, none:\n" + bouncer(""),
		"bouncer:\n" + bouncer(checkers),
		"arbiter: " + arbiterTable(stores),
	})
}
