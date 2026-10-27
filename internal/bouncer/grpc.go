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
	"html"
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
// service serves no assets and must answer without reaching anything. Its
// colours are the cluster's brand when brandingBase names where that is
// published (the browser fetches it, not this service), and the platform's
// own otherwise.
func deniedPage(brandingBase string) string {
	brand := ""
	if strings.HasPrefix(brandingBase, "https://") {
		brand = `<link rel="stylesheet" href="` + html.EscapeString(brandingBase+"brand.css") + `">`
	}
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>Not available to you</title>` + brand + `<style>` +
		`body{font-family:var(--brand-font-family-sans,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif);` +
		`background:var(--brand-color-paper-0,#f4f1ea);color:var(--brand-color-ink-1,#14152e);` +
		`display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}` +
		`main{max-width:32rem;padding:2rem}h1{font-size:1.25rem;margin:0 0 .5rem}` +
		`p{margin:0 0 1rem;color:var(--brand-color-ink-3,#45486b);line-height:1.5}` +
		`a{display:inline-block;background:var(--brand-color-brand-500,#262696);color:#fff;text-decoration:none;` +
		`padding:.55rem 1rem;border-radius:var(--brand-radius-1,.5rem)}</style></head><body><main>` +
		`<h1>This is not available to you</h1>` +
		`<p>Your session does not carry the access this page needs. If you have ` +
		`just been given it, or you are signed in as someone else, sign out and ` +
		`sign in again.</p><a href="` + LogoutPath + `">Sign out</a></main></body></html>`
}

// BrandingBase is where the cluster's brand is published, from the issuer
// base this service is configured with: the cluster's bare domain, which is
// the issuer's host (id.<kernel>) without its first label, where the
// concierge serves it. Empty when that is not a public https address of that
// shape.
func BrandingBase(issuerBase string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(issuerBase, "/"), "/auth")
	host, ok := strings.CutPrefix(base, "https://id.")
	if !ok || host == "" || strings.ContainsAny(host, "/?#") {
		return ""
	}
	return "https://" + host + "/branding/"
}

// Server is the Envoy ext_authz gRPC service over a Decider.
type Server struct {
	authv3.UnimplementedAuthorizationServer
	Decider *Decider
	// BrandingBase is where the denied page loads the cluster's brand
	// from; empty keeps the platform's own colours.
	BrandingBase string
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
		IDToken:       headers[HeaderIDToken],
		Cookie:        headers[HeaderCookie],
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
		// A redirect is answered here and the request never reaches a
		// backend: the old sign-out path, sent on to the Gateway's own.
		if dec.Redirect != "" {
			denied.Body = ""
			denied.Headers = []*corev3.HeaderValueOption{{
				Header:       &corev3.HeaderValue{Key: "location", Value: dec.Redirect},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			}, {
				// Nothing about a sign-out should be reused.
				Header:       &corev3.HeaderValue{Key: "cache-control", Value: "no-store"},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			}}
			return &authv3.CheckResponse{
				Status:       &rpcstatus.Status{Code: int32(codes.PermissionDenied), Message: dec.Reason},
				HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: denied},
			}, nil
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
			denied.Body = deniedPage(s.BrandingBase)
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
