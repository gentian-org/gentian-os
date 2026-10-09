/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
	"github.com/gentian-org/gentian-os/internal/bouncer"
)

// The publishing proxy's configuration (AD-6).
//
// This file is where the perimeter's security properties actually live, so it
// is worth saying what they are before the string building starts.
//
// A perimeter surface is the only way into a tenant that carries no session.
// Somebody following a CalDAV URL or a shared document link is not signed in
// and must not be made to be: the link IS the credential, and the application
// behind it is what checks it. The proxy's whole job is to make sure that is
// the only thing reaching the application, and that nothing of the tenant's
// session model leaks past it in either direction.
//
// Four properties, each of which is one mistake away from being false:
//
//  1. ONLY THE DECLARED PREFIXES. Everything else is 404, from the proxy,
//     without the request reaching the tenant. A perimeter surface that
//     forwarded / would publish the whole application, which is precisely
//     what a tenant approving "share calendars" did not agree to.
//
//  2. NO SESSION TRAVELS IN. The zone cookie is stripped from every request
//     before it leaves. A browser that happens to hold a session for the
//     tenant's own domain must not have it ride along to a surface that
//     grants access on a link — that is how a public URL quietly becomes an
//     authenticated one, and the difference never shows in a log.
//
//  3. NO IDENTITY IS ASSERTED. The gateway sets identity headers for
//     authenticated routes; here there is nobody to be. Any inbound header in
//     that family is cleared, so an application trusting them cannot be told
//     who to be by whoever made the request.
//
//  4. NO AMBIENT AUTHORITY. Authorization, Cookie and the forwarded-token
//     header are all dropped. Whatever the application needs to make this
//     decision is in the URL.

// perimeterProxyPort is what the proxy listens on inside its pod.
const perimeterProxyPort = 8080

// perimeterLimits are what every published entry is held to, whatever its
// profile says. One place, because they are the platform's and not an
// app's: a profile cannot raise them, and has no field to try with.
//
// The cluster's administrator may set each through the operator's
// environment (the variable beside it). A value the proxy would not start
// with is not taken: the default stands.
type perimeterLimits struct {
	// MaxBody is the largest request body, in nginx's notation.
	// PERIMETER_MAX_BODY.
	MaxBody string
	// RatePerSecond and Burst are the requests one client address may make:
	// that many a second, and that many more at once before the proxy
	// answers 429. PERIMETER_RATE_PER_SECOND, PERIMETER_RATE_BURST.
	RatePerSecond int
	Burst         int
	// Concurrent is how many requests of one client address the proxy
	// works on at a time. PERIMETER_CONCURRENT_PER_CLIENT.
	Concurrent int
	// ClientAddressHeader names the request header the caller's address is
	// read from, on a cluster whose edge is reached through something that
	// is not the caller: a tunnel. Empty reads the address the Gateway saw
	// the connection come from. Decided by edgeClientAddressHeader.
	ClientAddressHeader string
}

const (
	defaultPerimeterMaxBody       = "10m"
	defaultPerimeterRatePerSecond = 20
	defaultPerimeterBurst         = 200
	defaultPerimeterConcurrent    = 100
)

var (
	perimeterBodySize   = regexp.MustCompile(`^[1-9][0-9]{0,8}[kKmMgG]?$`)
	perimeterHeaderName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)
)

// perimeterLimitsFromEnv are the limits in force: the defaults, each replaced
// by the operator's environment where that names a usable value.
// clientAddressHeader is edgeClientAddressHeader's answer for this cluster.
func perimeterLimitsFromEnv(clientAddressHeader string) perimeterLimits {
	l := perimeterLimits{
		MaxBody:             defaultPerimeterMaxBody,
		RatePerSecond:       defaultPerimeterRatePerSecond,
		Burst:               defaultPerimeterBurst,
		Concurrent:          defaultPerimeterConcurrent,
		ClientAddressHeader: clientAddressHeader,
	}
	if v := strings.TrimSpace(os.Getenv("PERIMETER_MAX_BODY")); perimeterBodySize.MatchString(v) {
		l.MaxBody = v
	}
	positive := func(key string, into *int, max int) {
		if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && n > 0 && n <= max {
			*into = n
		}
	}
	positive("PERIMETER_RATE_PER_SECOND", &l.RatePerSecond, 100000)
	positive("PERIMETER_RATE_BURST", &l.Burst, 1000000)
	positive("PERIMETER_CONCURRENT_PER_CLIENT", &l.Concurrent, 60000)
	return l
}

// edgeClientAddressHeader is the header the edge is told the caller's
// address in, or empty where the edge sees the caller's own connection.
//
// A cluster behind the tunnel is reached by the tunnel's client alone, so
// every request arrives from that client's address; the tunnel writes the
// caller's into CF-Connecting-IP itself and replaces whatever a caller sent
// under that name. On a cluster with an address of its own, the Gateway sees
// the caller's connection, and that header is a caller's to write: reading
// it there would let anybody choose the address they are counted under.
//
// So the answer has to be true of this cluster, and it is taken from the
// first of these that says:
//
//   - PERIMETER_CLIENT_ADDRESS_HEADER, the administrator's word: a header
//     name, for a cluster whose load balancer or CDN hides the caller behind
//     its own address and names it in a header; or "none".
//   - EDGE_INGRESS and NETWORK_MODE, where the operator was told them
//     (cmd/main.go, buildEdgeIngress): cf-tunnel and tunnel are the tunnel,
//     static-ip and any other ingress are not.
//   - The Gateway's own Service. Without an address outside the cluster
//     (type ClusterIP) nothing but the tunnel's client and this cluster's
//     pods can open a connection to the Gateway, so the tunnel's header is
//     the tunnel's word; with one (LoadBalancer, NodePort) it is not read.
//   - Nothing says: the connection, which nobody can forge.
func edgeClientAddressHeader(ctx context.Context, c client.Client) string {
	if header, said := edgeClientAddressHeaderFromEnv(); said {
		return header
	}
	if c == nil {
		return ""
	}
	svc, err := findKernelEdgeService(ctx, c)
	if err != nil {
		return ""
	}
	if svc.Spec.Type == corev1.ServiceTypeClusterIP || svc.Spec.Type == "" {
		return cloudflareClientAddressHeader
	}
	return ""
}

func edgeClientAddressHeaderFromEnv() (header string, said bool) {
	if v := strings.TrimSpace(os.Getenv("PERIMETER_CLIENT_ADDRESS_HEADER")); v != "" {
		if strings.EqualFold(v, "none") {
			return "", true
		}
		if perimeterHeaderName.MatchString(v) {
			return v, true
		}
	}
	if name := os.Getenv("EDGE_INGRESS"); name != "" {
		if name == "cf-tunnel" {
			return cloudflareClientAddressHeader, true
		}
		return "", true
	}
	switch os.Getenv("NETWORK_MODE") {
	case "static-ip":
		return "", true
	case "tunnel":
		return cloudflareClientAddressHeader, true
	}
	return "", false
}

const cloudflareClientAddressHeader = "CF-Connecting-IP"

// perimeterPathPattern is what a declared path may consist of. The path goes
// into the proxy's configuration, where a brace, a quote, a semicolon or a
// space would be configuration and not a path; so a path with anything else
// in it is not rendered at all (perimeterSafePaths).
var perimeterPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)

// perimeterSafePaths are the paths that can be rendered, and whether all of
// them could be.
func perimeterSafePaths(paths []string) (safe []string, all bool) {
	all = true
	for _, p := range paths {
		if perimeterPathPattern.MatchString(p) && !strings.Contains(p, "//") {
			safe = append(safe, p)
		} else {
			all = false
		}
	}
	return safe, all
}

// perimeterPublishedPattern is the expression a normalised path matches when
// it is inside a declared prefix, by whole segments: /s publishes /s and
// /s/x and not /sx, as the Gateway's own prefix match does.
func perimeterPublishedPattern(prefix string) string {
	if prefix == "/" {
		return "^/"
	}
	if strings.HasSuffix(prefix, "/") {
		return "^" + regexp.QuoteMeta(prefix)
	}
	return "^" + regexp.QuoteMeta(prefix) + "(/|$)"
}

// perimeterProxyConfig renders the nginx configuration for one enabled
// exposure.
//
// It takes the paths from the PROFILE and the host from the ENABLEMENT,
// because those are two different people's decisions: the author said which
// prefixes this surface is, and the tenant's perimeter approver said it may be
// published and where. Neither can widen the other.
//
// website says this surface is a tenant's on the cluster's main address. The
// platform's paths there are then refused by the proxy as well as outranked
// at the Gateway, so the website cannot answer them whatever the routes look
// like for a moment; and every answer says nosniff, the one header the
// platform adds to a page it does not own.
//
// What a request has to pass, in the order the proxy asks:
//
//   - its method is not TRACE, TRACK or CONNECT (405);
//   - its path, as the caller wrote it, has one reading: no empty segment,
//     no dot segment plain or encoded, no encoded slash or backslash, no
//     path parameter (400). The proxy decides on the normalised path and
//     the application is sent the path as written, so a path the two could
//     read differently is how a declared prefix is left and a denied path
//     reached;
//   - its normalised path is inside a declared prefix (404);
//   - and is not a denied path, compared without regard to case (404);
//   - its caller's address is within its rate and its concurrent requests
//     (429);
//   - its headers and body are within the sizes below (400, 413, 431).
func perimeterProxyConfig(e *gentianov1alpha1.ExposureSpec, upstreamHost string, upstreamPort int32, website bool, limits perimeterLimits) string {
	denied := perimeterDenied(e)
	if website {
		denied = append(denied, mainAddressReservedPrefixes...)
		sort.Strings(denied)
	}
	prefixes, _ := perimeterSafePaths(perimeterPrefixes(e))
	denied, deniedAll := perimeterSafePaths(denied)
	if !deniedAll {
		// A denied path that cannot be written down cannot be refused, and
		// publishing the rest without it would publish what the author said
		// not to. The entry publishes nothing.
		prefixes = nil
	}

	var b strings.Builder
	b.WriteString("# Managed by gentian-os. The publishing proxy for one exposure (AD-6).\n")
	b.WriteString("#\n")
	b.WriteString("# Generated from the profile's declared paths and the enablement a\n")
	b.WriteString("# perimeter approver signed. Editing it here changes nothing: the\n")
	b.WriteString("# operator rewrites this ConfigMap from those two on every reconcile.\n")
	b.WriteString("worker_processes 1;\n")
	b.WriteString("error_log /dev/stderr warn;\n")
	b.WriteString("pid /tmp/nginx.pid;\n")
	b.WriteString("events { worker_connections 1024; }\n")
	b.WriteString("http {\n")
	b.WriteString("  access_log /dev/stdout;\n")
	// Writable paths under /tmp: the pod runs read-only, and nginx wants
	// somewhere for its temporary bodies whatever we think of that.
	b.WriteString("  client_body_temp_path /tmp/client_body;\n")
	b.WriteString("  proxy_temp_path /tmp/proxy;\n")
	b.WriteString("  fastcgi_temp_path /tmp/fastcgi;\n")
	b.WriteString("  uwsgi_temp_path /tmp/uwsgi;\n")
	b.WriteString("  scgi_temp_path /tmp/scgi;\n")
	b.WriteString("  # Sizes and times a request is held to. A request line or a header\n")
	b.WriteString("  # over 8k, or headers over 32k together, is refused; so is a body\n")
	b.WriteString("  # over the limit. A caller that sends nothing is dropped.\n")
	fmt.Fprintf(&b, "  client_max_body_size %s;\n", limits.MaxBody)
	b.WriteString("  client_header_buffer_size 1k;\n")
	b.WriteString("  large_client_header_buffers 4 8k;\n")
	b.WriteString("  client_header_timeout 15s;\n")
	b.WriteString("  client_body_timeout 30s;\n")
	b.WriteString("  send_timeout 30s;\n")
	b.WriteString("  keepalive_timeout 65s;\n")
	b.WriteString("  proxy_connect_timeout 5s;\n")
	b.WriteString("  proxy_send_timeout 60s;\n")
	b.WriteString("  proxy_read_timeout 60s;\n")
	b.WriteString("  proxy_request_buffering off;\n")
	b.WriteString("  proxy_http_version 1.1;\n")
	b.WriteString("  # A header name with an underscore is not passed on: an application\n")
	b.WriteString("  # that reads X_Gentian_Email as X-Gentian-Email would be told who to be.\n")
	b.WriteString("  underscores_in_headers off;\n")
	b.WriteString("  ignore_invalid_headers on;\n")
	b.WriteString("  server_tokens off;\n")
	b.WriteString("\n")
	b.WriteString("  # The path as the caller wrote it, up to the query. A path with more\n")
	b.WriteString("  # than one reading is refused: the proxy decides on the normalised\n")
	b.WriteString("  # path and the application is sent the written one.\n")
	b.WriteString("  map $request_uri $perimeter_ambiguous {\n")
	b.WriteString("    default 0;\n")
	// An empty segment; a dot segment, plain or encoded; an encoded slash,
	// backslash or NUL; a backslash or a path parameter, plain or encoded.
	b.WriteString(`    "~^[^?]*//" 1;` + "\n")
	b.WriteString(`    "~*^[^?]*/(\.|%2e)(\.|%2e)?(/|\?|$)" 1;` + "\n")
	b.WriteString(`    "~*^[^?]*%(2f|5c|00)" 1;` + "\n")
	b.WriteString(`    "~^[^?]*[\x5c;]" 1;` + "\n")
	b.WriteString(`    "~*^[^?]*%3b" 1;` + "\n")
	b.WriteString("  }\n")
	b.WriteString("  # Inside a declared prefix, by whole segments.\n")
	b.WriteString("  map $uri $perimeter_published {\n")
	b.WriteString("    default 0;\n")
	for _, prefix := range prefixes {
		fmt.Fprintf(&b, "    \"~%s\" 1;\n", perimeterPublishedPattern(prefix))
	}
	b.WriteString("  }\n")
	b.WriteString("  # Refused even where a prefix admits it, whatever its case.\n")
	b.WriteString("  map $uri $perimeter_denied {\n")
	b.WriteString("    default 0;\n")
	for _, deny := range denied {
		fmt.Fprintf(&b, "    \"~*^%s\" 1;\n", regexp.QuoteMeta(deny))
	}
	b.WriteString("  }\n")
	b.WriteString("  # Whose request this is. The Gateway appends the address its\n")
	b.WriteString("  # connection came from to X-Forwarded-For, so the last one is the\n")
	b.WriteString("  # Gateway's word and the ones before it are the caller's.\n")
	b.WriteString("  map $http_x_forwarded_for $perimeter_edge_peer {\n")
	b.WriteString("    default $remote_addr;\n")
	b.WriteString("    \"~(?:^|[ ,])(?<perimeter_last>[0-9A-Fa-f:.]+) *$\" $perimeter_last;\n")
	b.WriteString("  }\n")
	client := "$perimeter_edge_peer"
	if h := limits.ClientAddressHeader; h != "" {
		b.WriteString("  # This cluster's edge is reached through something that is not the\n")
		b.WriteString("  # caller, which names the caller in a header of its own.\n")
		fmt.Fprintf(&b, "  map $http_%s $perimeter_client {\n", strings.ToLower(strings.ReplaceAll(h, "-", "_")))
		b.WriteString("    default $perimeter_edge_peer;\n")
		b.WriteString("    \"~^(?<perimeter_named>[0-9A-Fa-f:.]+)$\" $perimeter_named;\n")
		b.WriteString("  }\n")
		client = "$perimeter_client"
	}
	fmt.Fprintf(&b, "  limit_req_zone %s zone=perimeter_rate:10m rate=%dr/s;\n", client, limits.RatePerSecond)
	fmt.Fprintf(&b, "  limit_conn_zone %s zone=perimeter_concurrent:10m;\n", client)
	b.WriteString("  limit_req_status 429;\n")
	b.WriteString("  limit_conn_status 429;\n")
	b.WriteString("\n")
	b.WriteString("  server {\n")
	fmt.Fprintf(&b, "    listen %d;\n", perimeterProxyPort)
	b.WriteString("    server_name _;\n")
	b.WriteString("    # Nothing here is a browsing surface, and an index of what a\n")
	b.WriteString("    # tenant shares is not ours to publish.\n")
	b.WriteString("    autoindex off;\n")
	b.WriteString("\n")
	if len(prefixes) == 0 {
		// An entry with no paths publishes nothing; declaring "/" is the
		// only way to publish the whole host.
		b.WriteString("    # Nothing is declared, so nothing reaches the tenant.\n")
		b.WriteString("    location / { return 404; }\n")
		b.WriteString("  }\n")
		b.WriteString("}\n")
		return b.String()
	}
	b.WriteString("    location / {\n")
	b.WriteString("      if ($request_method ~ \"^(TRACE|TRACK|CONNECT)$\") { return 405; }\n")
	b.WriteString("      if ($perimeter_ambiguous) { return 400; }\n")
	b.WriteString("      # Anything not declared never reaches the tenant.\n")
	b.WriteString("      if ($perimeter_published = 0) { return 404; }\n")
	b.WriteString("      if ($perimeter_denied) { return 404; }\n")
	fmt.Fprintf(&b, "      limit_req zone=perimeter_rate burst=%d nodelay;\n", limits.Burst)
	fmt.Fprintf(&b, "      limit_conn perimeter_concurrent %d;\n", limits.Concurrent)
	b.WriteString("\n")
	fmt.Fprintf(&b, "      proxy_pass http://%s:%d;\n", upstreamHost, upstreamPort)
	b.WriteString("      proxy_set_header Host $host;\n")
	b.WriteString("      # What the application is told of the connection is this proxy's\n")
	b.WriteString("      # word, not the caller's: one address, the scheme and the host.\n")
	b.WriteString("      proxy_set_header X-Forwarded-Proto https;\n")
	fmt.Fprintf(&b, "      proxy_set_header X-Forwarded-For %s;\n", client)
	fmt.Fprintf(&b, "      proxy_set_header X-Real-IP %s;\n", client)
	b.WriteString("      proxy_set_header X-Forwarded-Host $host;\n")
	for _, h := range perimeterStrippedForwardingHeaders {
		fmt.Fprintf(&b, "      proxy_set_header %s \"\";\n", h)
	}
	b.WriteString("\n")
	b.WriteString("      # Property 2, 3 and 4: nothing of the session model goes in.\n")
	b.WriteString("      # An empty value is what nginx takes for \"do not send this\".\n")
	b.WriteString("      proxy_set_header Cookie \"\";\n")
	b.WriteString("      proxy_set_header Authorization \"\";\n")
	b.WriteString("      proxy_set_header X-Forwarded-Access-Token \"\";\n")
	for _, h := range perimeterStrippedIdentityHeaders() {
		fmt.Fprintf(&b, "      proxy_set_header %s \"\";\n", h)
	}
	b.WriteString("\n")
	b.WriteString("      # And nothing of the application's session comes back out:\n")
	b.WriteString("      # a Set-Cookie here would plant a cookie on the public host\n")
	b.WriteString("      # that the browser then carries to every other link on it.\n")
	b.WriteString("      # Nor what the application runs on: the answer names the proxy.\n")
	b.WriteString("      proxy_hide_header Set-Cookie;\n")
	b.WriteString("      proxy_hide_header X-Powered-By;\n")
	if website {
		b.WriteString("      proxy_hide_header X-Content-Type-Options;\n")
		b.WriteString("      add_header X-Content-Type-Options nosniff always;\n")
	}
	b.WriteString("    }\n")
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

// perimeterStrippedForwardingHeaders are what a caller could otherwise tell
// the application about its own connection, or about which path it asked
// for. The proxy sets the three it vouches for (X-Forwarded-For, -Proto,
// -Host, and X-Real-IP) and sends none of these.
var perimeterStrippedForwardingHeaders = []string{
	"Forwarded",
	"X-Forwarded-Port",
	"X-Forwarded-Prefix",
	"X-Forwarded-Server",
	"X-Original-URL",
	"X-Rewrite-URL",
}

// perimeterStrippedIdentityHeaders are what the gateway sets on an
// AUTHENTICATED route to tell a backend who the caller is. On the perimeter
// there is nobody, so an inbound one is somebody claiming to be somebody.
//
// The front door's own are read from the service that sets them
// (bouncer.IdentityHeaders), so a header added there is removed here without
// anybody remembering to. The rest are names an earlier edge used and an
// application may still read.
func perimeterStrippedIdentityHeaders() []string {
	out := []string{
		"X-Auth-Request-User",
		"X-Auth-Request-Email",
		"X-Auth-Request-Preferred-Username",
		"X-Auth-Request-Groups",
		"X-Auth-Request-Subject",
		"X-Gentian-Tenant",
		"X-Gentian-Actor",
	}
	for _, h := range bouncer.IdentityHeaders() {
		out = append(out, http.CanonicalHeaderKey(h))
	}
	return out
}

// perimeterPrefixes are the path prefixes this exposure publishes: none for an
// entry that declares none (addresses.Prefixes).
func perimeterPrefixes(e *gentianov1alpha1.ExposureSpec) []string {
	return addresses.Prefixes(e)
}

// perimeterDenied are the paths refused even where a prefix admits them.
func perimeterDenied(e *gentianov1alpha1.ExposureSpec) []string {
	out := make([]string, 0, len(e.DenyPaths))
	for _, p := range e.DenyPaths {
		if p = strings.TrimSpace(p); p != "" && strings.HasPrefix(p, "/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
