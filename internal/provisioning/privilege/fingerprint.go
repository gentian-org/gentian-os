/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package privilege

import (
	"sort"
	"strings"

	"github.com/gentian-org/gentian-os/internal/authz"
)

// MemberFingerprint returns a stable hash input for app-admins membership. It
// covers each member's username as well as their id: the username is the
// address the sync script matches on, so a changed one must run the script again.
func MemberFingerprint(members []authz.KeycloakUser) string {
	entries := make([]string, 0, len(members))
	for _, member := range members {
		if member.ID != "" {
			entries = append(entries, member.ID+":"+member.Username)
		}
	}
	sort.Strings(entries)
	return strings.Join(entries, ",")
}
