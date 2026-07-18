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
	"strings"
	"testing"
)

func TestKeycloakOIDCAncestorOrigins(t *testing.T) {
	t.Parallel()
	origins := keycloakOIDCAncestorOrigins(
		"desk.gentian.org",
		[]string{"demo.desk.gentian.org"},
		map[string][]string{"demo": {"chat", "cloud"}},
		[]string{"demo"},
	)
	for _, want := range []string{
		"https://portal.desk.gentian.org",
		"https://id.desk.gentian.org",
		"https://*.desk.gentian.org",
		"https://*.demo.desk.gentian.org",
		"https://chat.demo.desk.gentian.org",
		"https://cloud.demo.desk.gentian.org",
	} {
		if !strings.Contains(origins, want) {
			t.Fatalf("origins %q missing %q", origins, want)
		}
	}
}
