// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

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
