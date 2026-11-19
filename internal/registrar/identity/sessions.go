/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package identity

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// A person's sign-in sessions in the realm: where they are signed in, and
// ending one. The admin API is used, as for everything else here: the realm's
// own account API takes only a token made out to its account client, which no
// token of the edge is.

// Session is one of a person's sign-in sessions.
type Session struct {
	ID         string    `json:"id"`
	IPAddress  string    `json:"ipAddress,omitempty"`
	Started    time.Time `json:"started"`
	LastAccess time.Time `json:"lastAccess"`
	// Clients are the clientIds the session signed in to, sorted.
	Clients []string `json:"clients"`
}

type sessionRep struct {
	ID         string            `json:"id"`
	UserID     string            `json:"userId"`
	IPAddress  string            `json:"ipAddress"`
	Start      int64             `json:"start"`
	LastAccess int64             `json:"lastAccess"`
	Clients    map[string]string `json:"clients"`
}

// Sessions lists a person's sign-in sessions, the most recently used first.
func (c *Client) Sessions(ctx context.Context, r Realm, userID string) ([]Session, error) {
	reps, err := c.sessionReps(ctx, r, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(reps))
	for _, s := range reps {
		clients := make([]string, 0, len(s.Clients))
		for _, id := range s.Clients {
			clients = append(clients, id)
		}
		sort.Strings(clients)
		out = append(out, Session{
			ID:         s.ID,
			IPAddress:  s.IPAddress,
			Started:    time.UnixMilli(s.Start).UTC(),
			LastAccess: time.UnixMilli(s.LastAccess).UTC(),
			Clients:    clients,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastAccess.After(out[j].LastAccess) })
	return out, nil
}

// EndSession ends one of a person's sessions. A session that is not the
// person's is not one this ends: it answers ErrNotFound, as for one that
// does not exist, so the answer does not tell whether somebody else holds it.
func (c *Client) EndSession(ctx context.Context, r Realm, userID, sessionID string) error {
	if !plainID(sessionID) {
		return fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
	}
	reps, err := c.sessionReps(ctx, r, userID)
	if err != nil {
		return err
	}
	for _, s := range reps {
		if s.ID == sessionID {
			return c.call(ctx, r, http.MethodDelete, "/sessions/"+url.PathEscape(sessionID), nil, nil, nil)
		}
	}
	return fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
}

func (c *Client) sessionReps(ctx context.Context, r Realm, userID string) ([]sessionRep, error) {
	if !plainID(userID) {
		return nil, fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	var reps []sessionRep
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(userID)+"/sessions", nil, nil, &reps); err != nil {
		return nil, err
	}
	return reps, nil
}
