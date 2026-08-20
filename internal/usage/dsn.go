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
