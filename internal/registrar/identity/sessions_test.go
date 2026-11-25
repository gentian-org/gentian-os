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
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A person's sessions are listed most recently used first, with the clients
// they signed in to; ending one ends only a session of that person.
func TestSessionsAreListedAndOnlyThePersonsOwnIsEnded(t *testing.T) {
	f, srv := newFake(t)
	f.users["demo"] = []userRep{{ID: "abc123", Username: "ada", Enabled: true}}
	f.sessions = map[string][]map[string]any{
		"abc123": {
			{"id": "s-old", "userId": "abc123", "ipAddress": "10.0.0.1", "start": 1794900000000, "lastAccess": 1794900100000,
				"clients": map[string]string{"u2": "gentian-edge-demo"}},
			{"id": "s-new", "userId": "abc123", "ipAddress": "10.0.0.2", "start": 1794930000000, "lastAccess": 1794930100000,
				"clients": map[string]string{"u2": "gentian-edge-demo", "u1": "app-a"}},
		},
		"tom": {{"id": "s-tom", "userId": "tom"}},
	}
	c := clientFor(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "a", ClientSecret: "s"}})
	r, _ := c.Realm("demo")
	ctx := context.Background()

	held, err := c.Sessions(ctx, r, "abc123")
	if err != nil || len(held) != 2 || held[0].ID != "s-new" || strings.Join(held[0].Clients, ",") != "app-a,gentian-edge-demo" ||
		held[0].IPAddress != "10.0.0.2" || held[1].Started.UnixMilli() != 1794900000000 {
		t.Fatalf("listed %+v, %v", held, err)
	}

	if err := c.EndSession(ctx, r, "abc123", "s-tom"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ending somebody else's session: %v", err)
	}
	if err := c.EndSession(ctx, r, "abc123", "../users/tom"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session id that is a path: %v", err)
	}
	if deletes := f.callsTo(http.MethodDelete, "/admin/realms/demo/sessions/s-tom"); len(deletes) != 0 {
		t.Fatalf("somebody else's session was ended: %+v", deletes)
	}
	if err := c.EndSession(ctx, r, "abc123", "s-old"); err != nil {
		t.Fatalf("ending one's own: %v", err)
	}
	if deletes := f.callsTo(http.MethodDelete, "/admin/realms/demo/sessions/s-old"); len(deletes) != 1 {
		t.Fatalf("the realm was asked %+v", f.recorded())
	}
}
