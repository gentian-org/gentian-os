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

// MemberFingerprint returns a stable hash input for app-admins membership.
func MemberFingerprint(members []authz.KeycloakUser) string {
	ids := make([]string, 0, len(members))
	for _, member := range members {
		if member.ID != "" {
			ids = append(ids, member.ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}
