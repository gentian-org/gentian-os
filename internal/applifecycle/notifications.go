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

package applifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/usage"
)

// Notices an administrator publishes to the people of a tenant.
//
// WHERE THIS LIVES, AND WHY IT IS NOT THE CONSOLE'S
// --------------------------------------------------
// A notification is read on the desktop: it is the desktop that shows people
// what their administrator said, and the desktop already owns the table --
// `admin_notifications` in the tenant's own shell database, with its own
// dismissals table beside it. So the console must not keep a copy: two
// stores would mean a notice that exists in one place and not the other, and
// the console keeps no state by design.
//
// The operator writes into that table instead. It already resolves the same
// database for the usage history (usage.StoreForTenant reads the
// portal-shell-<tenant> Secret), so this adds a query and not a connection,
// and a tenant's notices leave with the tenant's data like everything else in
// there.
//
// The table is the DESKTOP'S. This does not create it and must not: the
// desktop's migrations own that schema, and an operator that created its own
// version would own half a table the desktop then migrates. A tenant whose
// desktop has not run yet is answered with "the desktop has not created it",
// which is true and actionable, rather than an empty list that reads as "no
// notices".

// Notification is one notice as the API presents it.
type Notification struct {
	ID          string               `json:"id"`
	PublishedAt int64                `json:"publishedAt"`
	Tenant      string               `json:"tenant"`
	Title       string               `json:"title"`
	Body        string               `json:"body"`
	Severity    string               `json:"severity"`
	Publisher   string               `json:"publisher"`
	Audience    NotificationAudience `json:"audience"`
	LinkURL     *string              `json:"linkUrl,omitempty"`
	LinkLabel   *string              `json:"linkLabel,omitempty"`
	ExpiresAt   *int64               `json:"expiresAt,omitempty"`
}

// NotificationAudience is who a notice is for.
type NotificationAudience struct {
	Scope  string   `json:"scope"`
	Tenant *string  `json:"tenant,omitempty"`
	Groups []string `json:"groups"`
}

// PublishNotificationRequest is one notice, as the caller asks for it.
type PublishNotificationRequest struct {
	Tenant    string
	Actor     string
	Title     string
	Body      string
	Severity  string
	Audience  *NotificationAudience
	LinkURL   string
	LinkLabel string
	ExpiresAt int64
}

// ErrNoNotificationStore is a tenant whose desktop has not created the table.
var ErrNoNotificationStore = fmt.Errorf("this tenant has no notification store yet: the desktop creates it on its first start")

// notificationColumns is the desktop's schema, named once. A column list
// written twice is a column list that disagrees with itself the day one side
// gains a field.
const notificationColumns = "id, published_at, tenant, title, body, severity, publisher, audience, link_url, link_label, expires_at"

// Notifications lists a tenant's notices, newest first.
func (s *Service) Notifications(ctx context.Context, tenantName string) ([]Notification, error) {
	conn, err := s.notificationConn(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx,
		"SELECT "+notificationColumns+" FROM admin_notifications WHERE tenant = $1 ORDER BY published_at DESC LIMIT 200",
		tenantName)
	if err != nil {
		return nil, notificationError(err)
	}
	defer rows.Close()

	out := []Notification{}
	for rows.Next() {
		var n Notification
		var audience []byte
		if err := rows.Scan(&n.ID, &n.PublishedAt, &n.Tenant, &n.Title, &n.Body, &n.Severity,
			&n.Publisher, &audience, &n.LinkURL, &n.LinkLabel, &n.ExpiresAt); err != nil {
			return nil, err
		}
		n.Audience = NotificationAudience{Groups: []string{}}
		if len(audience) > 0 {
			_ = json.Unmarshal(audience, &n.Audience)
		}
		if n.Audience.Groups == nil {
			n.Audience.Groups = []string{}
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// PublishNotification writes one notice for a tenant's people to read.
//
// An action: it happens once, it is not a statement about how the cluster
// should be, and nothing reconciles it. The publisher is recorded because a
// notice with no author is one nobody can ask about.
func (s *Service) PublishNotification(ctx context.Context, req PublishNotificationRequest) (*Notification, error) {
	if req.Title == "" || req.Body == "" {
		return nil, fmt.Errorf("a notification needs a title and a body")
	}
	severity := req.Severity
	switch severity {
	case "info", "warning", "critical":
	case "":
		severity = "info"
	default:
		return nil, fmt.Errorf("severity must be info, warning or critical, not %q", severity)
	}

	notice := Notification{
		ID:          uuid.NewString(),
		PublishedAt: time.Now().UTC().UnixMilli(),
		Tenant:      req.Tenant,
		Title:       req.Title,
		Body:        req.Body,
		Severity:    severity,
		Publisher:   req.Actor,
		Audience:    NotificationAudience{Scope: "tenant", Tenant: &req.Tenant, Groups: []string{}},
	}
	if req.Audience != nil {
		notice.Audience = *req.Audience
		if notice.Audience.Groups == nil {
			notice.Audience.Groups = []string{}
		}
	}
	if req.LinkURL != "" {
		notice.LinkURL = &req.LinkURL
	}
	if req.LinkLabel != "" {
		notice.LinkLabel = &req.LinkLabel
	}
	if req.ExpiresAt > 0 {
		notice.ExpiresAt = &req.ExpiresAt
	}

	audience, err := json.Marshal(notice.Audience)
	if err != nil {
		return nil, err
	}
	conn, err := s.notificationConn(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()

	_, err = conn.Exec(ctx,
		"INSERT INTO admin_notifications ("+notificationColumns+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)",
		notice.ID, notice.PublishedAt, notice.Tenant, notice.Title, notice.Body, notice.Severity,
		notice.Publisher, audience, notice.LinkURL, notice.LinkLabel, notice.ExpiresAt)
	if err != nil {
		return nil, notificationError(err)
	}
	return &notice, nil
}

// notificationConn opens the tenant's own database, the one the desktop and
// the usage history already use.
func (s *Service) notificationConn(ctx context.Context, tenantName string) (*pgx.Conn, error) {
	if _, err := s.getTenant(ctx, tenantName); err != nil {
		return nil, err
	}
	store, err := usage.StoreForTenant(ctx, s.client, layout.Tenant(tenantName), tenantName)
	if err != nil {
		return nil, err
	}
	return store.Connect(ctx)
}

// notificationError names the one failure worth distinguishing: the table is
// the desktop's, and a tenant whose desktop has not started yet does not have
// it. Reporting that as "no notices" would be a lie a person acts on.
func notificationError(err error) error {
	if err == nil {
		return nil
	}
	if msg := err.Error(); strings.Contains(msg, "admin_notifications") &&
		(strings.Contains(msg, "does not exist") || strings.Contains(msg, "undefined_table")) {
		return ErrNoNotificationStore
	}
	return err
}
