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
	"net/http"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
)

// deniedPage is what a browser gets instead of the word "Forbidden": the
// refusal, and the one link that can change it. Inline and tiny, because this
// service serves no assets and must answer without reaching anything.
const deniedPage = `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
	`<meta name="viewport" content="width=device-width,initial-scale=1">` +
	`<title>Not available to you</title><style>` +
	`body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;background:#f4f1ea;color:#14152e;` +
	`display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}` +
	`main{max-width:32rem;padding:2rem}h1{font-size:1.25rem;margin:0 0 .5rem}` +
	`p{margin:0 0 1rem;color:#45486b;line-height:1.5}` +
	`a{display:inline-block;background:#262696;color:#fff;text-decoration:none;` +
	`padding:.55rem 1rem;border-radius:.5rem}</style></head><body><main>` +
	`<h1>This is not available to you</h1>` +
	`<p>Your session does not carry the access this page needs. If you have ` +
	`just been given it, or you are signed in as someone else, sign out and ` +
	`sign in again.</p><a href="/oauth2/logout">Sign out</a></main></body></html>`

// Server is the Envoy ext_authz gRPC service over a Decider.
type Server struct {
	authv3.UnimplementedAuthorizationServer
	Decider *Decider
}

// Check answers Envoy.
func (s *Server) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	httpReq := req.GetAttributes().GetRequest().GetHttp()
	headers := httpReq.GetHeaders()
	r := Request{
		ID:            httpReq.GetId(),
		Host:          headers[":authority"],
		Path:          httpReq.GetPath(),
		Authorization: headers["authorization"],
		Cookies:       parseCookies(headers["cookie"]),
	}
	if r.Host == "" {
		r.Host = httpReq.GetHost()
	}
	dec := s.Decider.Decide(ctx, r)
	if !dec.Allow {
		denied := &authv3.DeniedHttpResponse{
			Status: &typev3.HttpStatus{Code: typev3.StatusCode(dec.Status)},
			Body:   http.StatusText(dec.Status),
		}
		// A refusal a browser can act on.
		//
		// A session may name someone this cluster no longer knows: an account
		// deleted, or renamed, while a browser still holds a valid cookie for
		// it. Every relation is then denied and the answer is a bare 403 on
		// every page, including the desktop the person would have signed out
		// from. The way out is the edge's own logout path, so the refusal
		// names it. It grants nothing: signing out is available to anyone
		// holding a session, refused or not.
		if dec.Browser {
			denied.Body = deniedPage
			denied.Headers = []*corev3.HeaderValueOption{{
				Header:       &corev3.HeaderValue{Key: "content-type", Value: "text/html; charset=utf-8"},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			}}
		}
		return &authv3.CheckResponse{
			Status:       &rpcstatus.Status{Code: int32(grpcCode(dec.Status)), Message: dec.Reason},
			HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: denied},
		}, nil
	}
	ok := &authv3.OkHttpResponse{HeadersToRemove: dec.RemoveHeaders}
	for k, v := range dec.Headers {
		ok.Headers = append(ok.Headers, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: k, Value: v},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	return &authv3.CheckResponse{
		Status:       &rpcstatus.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: ok},
	}, nil
}

func grpcCode(status int) codes.Code {
	switch status {
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	default:
		return codes.PermissionDenied
	}
}

func parseCookies(h string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(h, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" {
			continue
		}
		out[name] = strings.Trim(value, "\"")
	}
	return out
}
