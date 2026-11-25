/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"encoding/json"
	"strings"
	"testing"
)

// An app released without the model gateway reads nothing of it: no entry of
// its ExternalSecret, no set entry of its release, no variable of its hook.
// Any of the three would name a record that is never written, and hold the
// release the optional requirement is there to let through. With the
// gateway, all three are there.
func TestAnAppReleasedWithoutTheModelGatewayReadsNothingOfIt(t *testing.T) {
	rendered := func(withheld bool) string {
		in := fixtureInput(t, "testdata/render/app-model-gateway")
		in.ModelAccessWithheld = withheld
		out, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(out.Objects)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	marks := []string{"llm-base-url", "llm-api-key", `"name":"LLM_BASE_URL"`, `"name":"LLM_API_KEY"`, `/llm"`}

	with := rendered(false)
	for _, mark := range marks {
		if !strings.Contains(with, mark) {
			t.Errorf("with the gateway, nothing renders %s", mark)
		}
	}
	without := rendered(true)
	for _, mark := range marks {
		if strings.Contains(without, mark) {
			t.Errorf("without the gateway, %s is still rendered", mark)
		}
	}
	// The release and the hook are rendered either way.
	for _, kind := range []string{`"kind":"Release"`, `"kind":"Job"`} {
		if !strings.Contains(without, kind) {
			t.Errorf("without the gateway no %s is rendered", kind)
		}
	}
}
