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

package tiles

import "testing"

func TestEveryTileHasWhatTheConsoleNeeds(t *testing.T) {
	if len(All()) < 3 {
		t.Fatalf("expected the three kernel tiles, got %d", len(All()))
	}
	for _, tile := range All() {
		if tile.DisplayName == "" || tile.Icon == "" || tile.Description == "" {
			t.Errorf("%s: display name, icon and description are what the console shows", tile.Name)
		}
	}
	if got := All()[2].URL("k.example"); got != "https://id.k.example/admin/" {
		t.Fatalf("keycloak URL = %s", got)
	}
}
