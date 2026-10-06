/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"strings"
	"testing"
)

func TestKeycloakOIDCAncestorOrigins(t *testing.T) {
	t.Parallel()
	origins := keycloakOIDCAncestorOrigins(
		"platform.example.test",
		[]string{"demo.platform.example.test"},
		map[string][]string{"demo": {"chat", "cloud"}},
		[]string{"demo"},
	)
	for _, want := range []string{
		"https://console.platform.example.test",
		"https://id.platform.example.test",
		"https://*.platform.example.test",
		"https://*.demo.platform.example.test",
		"https://chat.demo.platform.example.test",
		"https://cloud.demo.platform.example.test",
	} {
		if !strings.Contains(origins, want) {
			t.Fatalf("origins %q missing %q", origins, want)
		}
	}
}
