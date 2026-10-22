/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"net/http"
	"testing"
	"time"
)

// Who approves a public address.
//
// The fixture's tenants are the two cases there are. demo is operated by its
// cluster and tom, its administrator, is its perimeter approver; solo
// administers itself and paul is its perimeter approver.

const soloShares = "/v1/tenants/solo/exposures/nextcloud/shares"

func TestAPublicAddressIsApprovedByWhoeverHoldsTheRelationAndNobodyElse(t *testing.T) {
	for _, c := range []struct {
		who, realm, subject, path string
		want                      int
	}{
		// demo: operated, tom its perimeter approver.
		{"the cluster's administrator, in a tenant the cluster operates", "gentian", "alice", sharesPath, http.StatusAccepted},
		{"the tenant's perimeter approver", "tenant-demo", "tom", sharesPath, http.StatusAccepted},
		{"tom's subject from another realm", "tenant-solo", "tom", sharesPath, http.StatusForbidden},
		{"somebody nobody named", "tenant-demo", "pat", sharesPath, http.StatusForbidden},
		{"a member of the tenant", "tenant-demo", "mia", sharesPath, http.StatusForbidden},
		{"another tenant's administrator", "tenant-solo", "tina", sharesPath, http.StatusForbidden},
		// solo: administers itself, paul its perimeter approver.
		{"the cluster's administrator, in a tenant that withdrew the operator", "gentian", "alice", soloShares, http.StatusForbidden},
		{"the tenant's administrator, not named", "tenant-solo", "tina", soloShares, http.StatusForbidden},
		{"that tenant's perimeter approver", "tenant-solo", "paul", soloShares, http.StatusAccepted},
		{"another tenant's approver", "tenant-demo", "tom", soloShares, http.StatusForbidden},
	} {
		h := start(t)
		before := h.tip(t)
		code, body := h.do(t, "PUT", c.path, h.token(t, c.realm, c.subject), publishBody(time.Now().Add(24*time.Hour)))
		if code != c.want {
			t.Errorf("%s: %d %v, want %d", c.who, code, body, c.want)
		}
		if moved := h.tip(t) != before; moved != (c.want == http.StatusAccepted) {
			t.Errorf("%s: answered %d, and the repository moved = %v", c.who, code, moved)
		}
		// Withdrawing is the same relation.
		code, _ = h.do(t, "DELETE", c.path, h.token(t, c.realm, c.subject), "")
		if refused := code == http.StatusForbidden; refused != (c.want == http.StatusForbidden) {
			t.Errorf("%s: withdrawing answered %d", c.who, code)
		}
	}
}
