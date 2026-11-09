/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestMembershipServerLimits: every timeout is set, and a request whose
// headers pass the size bound is refused before the handler sees it.
func TestMembershipServerLimits(t *testing.T) {
	var reached atomic.Bool
	srv := newMembershipServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	if srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 || srv.ReadHeaderTimeout == 0 {
		t.Fatalf("a timeout is unset: %+v", srv)
	}
	if srv.MaxHeaderBytes == 0 || srv.MaxHeaderBytes > 64<<10 {
		t.Fatalf("MaxHeaderBytes is %d, want a bound of at most 64 KiB", srv.MaxHeaderBytes)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v1/events/keycloak", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Signature", strings.Repeat("a", 256<<10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("a 256 KiB header answered %d, want 431", resp.StatusCode)
	}
	if reached.Load() {
		t.Fatal("the handler saw a request over the header bound")
	}
}
