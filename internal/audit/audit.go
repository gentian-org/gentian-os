/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package audit writes the platform's audit records: one JSON line per
// decision, change or identity event, in one envelope, on the service's own
// logger (docs/security/audit-events.md §3).
//
// The envelope is what a forwarder routes on and a destination keeps, so its
// field names are a contract. The line is an ordinary slog record: the JSON
// handler every service already runs supplies `time`, `level` and `msg`;
// this package adds `audit: true` and the envelope's fields beside them.
//
// People are named by the issuer's subject and nothing else. A name or an
// address is not a field here, and a Detail value that carries one is the
// caller's mistake.
package audit

import (
	"context"
	"log/slog"
	"strings"
)

// Message is the msg of every audit line, so a reader can find them without
// knowing the envelope.
const Message = "audit"

// Kinds of record.
const (
	KindDecision   = "decision"
	KindChange     = "change"
	KindIdentity   = "identity"
	KindSession    = "session"
	KindAccess     = "access"
	KindBackup     = "backup"
	KindDeletion   = "deletion"
	KindPolicy     = "policy"
	KindBreakGlass = "break-glass"
)

// Outcomes.
const (
	Allowed = "allowed"
	Refused = "refused"
	OK      = "ok"
	Failed  = "failed"
)

// Record is one audit event.
type Record struct {
	// Source is the service that wrote it: bouncer, director, registrar.
	Source string
	// Kind is one of the Kind constants.
	Kind string
	// RequestID joins this record to the others of the same request.
	RequestID string
	// Tenant the record is about; derived from Object when empty.
	Tenant string
	// Subject is the issuer's sub of the person, or system:<service>.
	Subject string
	// Object and Action are what was asked or done, as the rights store
	// names them where there is one: app:acme/nextcloud, can_use.
	Object string
	Action string
	// Outcome is one of the outcome constants.
	Outcome string
	// Detail holds named fields of the source's own. No free text copied
	// from a request, no names, no addresses, no secrets.
	Detail map[string]any
}

// Write logs r on log as one audit line. A nil logger writes nothing: a
// service built without one has nowhere to say it.
func Write(ctx context.Context, log *slog.Logger, r Record) {
	if log == nil {
		return
	}
	tenant := r.Tenant
	if tenant == "" {
		tenant = TenantOf(r.Object)
	}
	attrs := []slog.Attr{
		slog.Bool("audit", true),
		slog.String("source", r.Source),
		slog.String("kind", r.Kind),
		slog.String("outcome", r.Outcome),
	}
	for _, f := range []struct{ k, v string }{
		{"request_id", r.RequestID},
		{"tenant", tenant},
		{"subject", r.Subject},
		{"object", r.Object},
		{"action", r.Action},
	} {
		if f.v != "" {
			attrs = append(attrs, slog.String(f.k, f.v))
		}
	}
	if len(r.Detail) > 0 {
		var group []any
		for k, v := range r.Detail {
			group = append(group, k, v)
		}
		attrs = append(attrs, slog.Group("detail", group...))
	}
	log.LogAttrs(ctx, slog.LevelInfo, Message, attrs...)
}

// TenantOf is the tenant a rights-store object belongs to: tenant:<t> and
// app:<t>/<profile> name one, anything else none.
func TenantOf(object string) string {
	switch {
	case strings.HasPrefix(object, "tenant:"):
		return strings.TrimPrefix(object, "tenant:")
	case strings.HasPrefix(object, "app:"):
		rest := strings.TrimPrefix(object, "app:")
		if i := strings.IndexByte(rest, '/'); i > 0 {
			return rest[:i]
		}
	}
	return ""
}
