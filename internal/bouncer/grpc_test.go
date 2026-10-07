/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"

	"github.com/gentian-org/gentian-os/internal/director/authn"
)

func TestEnvoyGetsHeadersOnAllowAndAStatusOnDeny(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	root := &authn.Identity{Subject: "root", Realm: "kernel", SessionID: "s1"}
	d := New(Options{
		Verifier: fakeVerifier{
			tokens:   map[string]*authn.Identity{"root-token": root},
			idTokens: map[string]map[string]*authn.Identity{kernelClient: {"root-id-token": root}},
		},
		Store: store, Table: table(), CacheTTL: time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s := &Server{Decider: d}
	check := func(host, path string, headers map[string]string) *authv3.CheckResponse {
		h := map[string]string{":authority": host}
		for k, v := range headers {
			h[k] = v
		}
		resp, err := s.Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
				Id: "r1", Host: host, Path: path, Headers: h,
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// The client sent identity headers of its own. They are overridden.
	ok := check("argocd.k.example", "/", map[string]string{
		"authorization": "Bearer root-token", HeaderSubject: "somebody-else", HeaderEmail: "forged@example.com",
	})
	if ok.Status.Code != int32(codes.OK) {
		t.Fatalf("status = %v", ok.Status)
	}
	set := map[string]string{}
	for _, h := range ok.GetOkResponse().GetHeaders() {
		set[h.Header.Key] = h.Header.Value
		if h.AppendAction != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
			t.Fatalf("%s must override what the client sent", h.Header.Key)
		}
	}
	if set[HeaderSubject] != "root" {
		t.Fatalf("subject header = %q", set[HeaderSubject])
	}
	for _, h := range []string{HeaderSubject, HeaderRealm, HeaderSession, HeaderEmail, HeaderName} {
		if _, there := set[h]; !there {
			t.Fatalf("%s is not overridden; a client-sent one would survive", h)
		}
	}
	if got := ok.GetOkResponse().GetHeadersToRemove(); len(got) != 2 || got[0] != HeaderIDToken || got[1] != "authorization" {
		t.Fatalf("headers to remove = %v", got)
	}
	// The session's cookies are not this service's to read: a token in a
	// cookie, in the clear or not, is no token.
	for name, headers := range map[string]map[string]string{
		"no token":            {},
		"a token in a cookie": {"cookie": "gentian-kernel-access=root-token; at=root-token"},
		"a forged bearer":     {"authorization": "Bearer forged"},
	} {
		denied := check("argocd.k.example", "/", headers)
		if denied.Status.Code != int32(codes.Unauthenticated) || denied.GetDeniedResponse().GetStatus().GetCode() != typev3.StatusCode_Unauthorized {
			t.Fatalf("%s on a session route = %v", name, denied)
		}
	}
	// The ID token is read from its header on the route that calls for it.
	kept := check("id.k.example", "/auth/admin/", map[string]string{
		"authorization": "Bearer the-pages-own", HeaderIDToken: "root-id-token",
	})
	if kept.Status.Code != int32(codes.OK) {
		t.Fatalf("status = %v", kept.Status)
	}
	if got := kept.GetOkResponse().GetHeadersToRemove(); len(got) != 1 || got[0] != HeaderIDToken {
		t.Fatalf("headers to remove = %v", got)
	}
	// The old sign-out path is a redirect to the gateway's logout.
	out := check("argocd.k.example", SignOutPath, nil)
	var location string
	for _, h := range out.GetDeniedResponse().GetHeaders() {
		if h.Header.Key == "location" {
			location = h.Header.Value
		}
	}
	if out.GetDeniedResponse().GetStatus().GetCode() != typev3.StatusCode_Found || location != LogoutPath {
		t.Fatalf("sign-out = %v", out)
	}
	denied := check("api.k.example", "/", nil)
	if denied.Status.Code != int32(codes.Unauthenticated) || denied.GetDeniedResponse().GetStatus().GetCode() != typev3.StatusCode_Unauthorized {
		t.Fatalf("denied = %v", denied)
	}
}
