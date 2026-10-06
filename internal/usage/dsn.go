/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usage

import "strings"

// normalizeDSN strips the SQLAlchemy dialect suffix from a database URL.
//
// portal-shell-<tenant> holds postgresql+psycopg://… because the portal is what
// normally reads it, and SQLAlchemy selects its driver from that suffix. pgx
// parses the scheme as a whole and rejects the compound one, so the operator —
// a second, later reader of a Secret written for someone else — has to remove
// what was added for them. Writing two URLs into the Secret instead would leave
// two passwords to rotate in step.
func normalizeDSN(dsn string) string {
	scheme, rest, found := strings.Cut(dsn, "://")
	if !found {
		return dsn
	}
	base, _, hasDialect := strings.Cut(scheme, "+")
	if !hasDialect {
		return dsn
	}
	return base + "://" + rest
}
