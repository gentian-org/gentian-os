/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What a tenant is meant to have is declared in git by the director. The
// routes that once changed it here -- install, uninstall, the addon selection
// and the resource plan -- must stay gone: each was a second way to change
// declared state, with no author and no review.
func TestTheListenerHasNoWriteOfDeclaredState(t *testing.T) {
	h := &HTTPServer{Token: "director"}
	mux := h.routes()
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/tenants/demo/apps/nextcloud", ""},
		{"DELETE", "/v1/tenants/demo/apps/nextcloud", ""},
		{"PUT", "/v1/tenants/demo/apps/nextcloud/addons", `{"addons":[]}`},
		{"PUT", "/v1/tenants/demo/resources", `{"plan":"small"}`},
		{"PUT", "/v1/tenants/demo/backup-policy", `{}`},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		r.Header.Set("Authorization", "Bearer director")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d; the route must not exist", c.method, c.path, w.Code)
		}
	}
}
