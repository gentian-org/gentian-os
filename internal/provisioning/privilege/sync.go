// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package privilege

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/provisioning/nextcloud"
)

// NextcloudTenantServiceURL returns the in-cluster Nextcloud base URL for a tenant app.
func NextcloudTenantServiceURL(tenantNamespace string) string {
	return fmt.Sprintf("http://nextcloud.%s.svc.cluster.local", tenantNamespace)
}

// NextcloudUID resolves the Nextcloud account id for a Keycloak user.
func NextcloudUID(user authz.KeycloakUser) string {
	if vals := user.Attributes["opendesk_username"]; len(vals) > 0 {
		if uid := strings.TrimSpace(vals[0]); uid != "" {
			return uid
		}
	}
	if user.Username != "" {
		return strings.Split(user.Username, "@")[0]
	}
	if user.Email != "" {
		return strings.Split(user.Email, "@")[0]
	}
	return ""
}

// SyncNextcloudGroup reconciles app-admins members into a Nextcloud group.
func SyncNextcloudGroup(
	ctx context.Context,
	members []authz.KeycloakUser,
	client *nextcloud.Client,
	groupName string,
	preserveUsers []string,
) error {
	desired := map[string]authz.KeycloakUser{}
	for _, member := range members {
		uid := NextcloudUID(member)
		if uid == "" {
			continue
		}
		desired[uid] = member
	}

	current, err := client.ListGroupUsers(ctx, groupName)
	if err != nil {
		return err
	}
	currentSet := map[string]struct{}{}
	for _, uid := range current {
		currentSet[uid] = struct{}{}
	}
	preserve := map[string]struct{}{}
	for _, uid := range preserveUsers {
		if uid != "" {
			preserve[uid] = struct{}{}
		}
	}

	for uid, member := range desired {
		display := strings.TrimSpace(strings.Join([]string{
			firstAttr(member.Attributes, "firstName"),
			firstAttr(member.Attributes, "lastName"),
		}, " "))
		if display == "" {
			display = uid
		}
		if err := client.EnsureUser(ctx, uid, display); err != nil {
			return fmt.Errorf("ensure nextcloud user %s: %w", uid, err)
		}
		if _, ok := currentSet[uid]; ok {
			continue
		}
		if err := client.AddUserToGroup(ctx, uid, groupName); err != nil {
			return fmt.Errorf("add nextcloud user %s to group %s: %w", uid, groupName, err)
		}
	}

	for uid := range currentSet {
		if _, keep := preserve[uid]; keep {
			continue
		}
		if _, want := desired[uid]; want {
			continue
		}
		if err := client.RemoveUserFromGroup(ctx, uid, groupName); err != nil {
			return fmt.Errorf("remove nextcloud user %s from group %s: %w", uid, groupName, err)
		}
	}
	return nil
}

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

func firstAttr(attrs map[string][]string, key string) string {
	if vals := attrs[key]; len(vals) > 0 {
		return strings.TrimSpace(vals[0])
	}
	return ""
}
