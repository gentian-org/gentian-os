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
		return &authv3.CheckResponse{
			Status: &rpcstatus.Status{Code: int32(grpcCode(dec.Status)), Message: dec.Reason},
			HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
				Status: &typev3.HttpStatus{Code: typev3.StatusCode(dec.Status)},
				Body:   http.StatusText(dec.Status),
			}},
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
