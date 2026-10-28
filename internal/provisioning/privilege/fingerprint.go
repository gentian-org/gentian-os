/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package privilege

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// UsersFingerprint returns a short hash of the people entitled to an app. The
// list can run to thousands, so it is recorded as a hash rather than spelled
// out as the administrators are.
func UsersFingerprint(members []authz.KeycloakUser) string {
	sum := sha256.Sum256([]byte(MemberFingerprint(members)))
	return hex.EncodeToString(sum[:8])
}

// DeclarationFingerprint returns a short hash of what an app declares for its
// privileged-role sync: the role and the Job. A profile that changes either
// has not had the change applied, whatever the membership, so a completed sync
// is recorded against it as well as against the members.
func DeclarationFingerprint(provisioning any) string {
	raw, err := json.Marshal(provisioning)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}
