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

package authz

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
	d := New(Options{
		Verifier: fakeVerifier{tokens: map[string]*authn.Identity{"root-token": {Subject: "root", Realm: "kernel", SessionID: "s1"}}},
		Store:    store, Table: table(), CacheTTL: time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s := &Server{Decider: d}
	check := func(host, cookie string) *authv3.CheckResponse {
		resp, err := s.Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
				Id: "r1", Host: host, Path: "/", Headers: map[string]string{":authority": host, "cookie": cookie},
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	ok := check("argocd.k.example", "other=1; at=root-token")
	if ok.Status.Code != int32(codes.OK) {
		t.Fatalf("status = %v", ok.Status)
	}
	var subject string
	for _, h := range ok.GetOkResponse().GetHeaders() {
		if h.Header.Key == HeaderSubject {
			subject = h.Header.Value
			if h.AppendAction != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
				t.Fatal("identity headers must override what the client sent")
			}
		}
	}
	if subject != "root" {
		t.Fatalf("subject header = %q", subject)
	}
	if got := ok.GetOkResponse().GetHeadersToRemove(); len(got) != 1 || got[0] != "authorization" {
		t.Fatalf("headers to remove = %v", got)
	}
	// A forged session on an oidc route passes to the OIDC filter with
	// nothing: no identity header set, the bearer and every identity header
	// removed.
	passed := check("argocd.k.example", "at=forged")
	if passed.Status.Code != int32(codes.OK) || len(passed.GetOkResponse().GetHeaders()) != 0 {
		t.Fatalf("passed = %v", passed)
	}
	if got := passed.GetOkResponse().GetHeadersToRemove(); len(got) < 2 || got[0] != "authorization" {
		t.Fatalf("headers to remove = %v", got)
	}
	denied := check("api.k.example", "")
	if denied.Status.Code != int32(codes.Unauthenticated) || denied.GetDeniedResponse().GetStatus().GetCode() != typev3.StatusCode_Unauthorized {
		t.Fatalf("denied = %v", denied)
	}
}
