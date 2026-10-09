/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"
	"sort"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
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
func perimeterProxyConfig(e *gentianov1alpha1.ExposureSpec, upstreamHost string, upstreamPort int32, website bool) string {
	denied := perimeterDenied(e)
	if website {
		denied = append(denied, mainAddressReservedPrefixes...)
		sort.Strings(denied)
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
	b.WriteString("  # A shared link may be a large file in either direction.\n")
	b.WriteString("  client_max_body_size 0;\n")
	b.WriteString("  proxy_request_buffering off;\n")
	b.WriteString("  proxy_http_version 1.1;\n")
	b.WriteString("  server {\n")
	fmt.Fprintf(&b, "    listen %d;\n", perimeterProxyPort)
	b.WriteString("    server_name _;\n")
	b.WriteString("    # Nothing here is a browsing surface, and an index of what a\n")
	b.WriteString("    # tenant shares is not ours to publish.\n")
	b.WriteString("    autoindex off;\n")
	b.WriteString("    server_tokens off;\n")
	b.WriteString("\n")
	// A surface that declares "/" publishes the whole host -- a public site
	// is one -- and its own location below is the root. Declaring it is the
	// only way to have it: an entry with no paths still publishes nothing.
	wholeHost := false
	for _, prefix := range perimeterPrefixes(e) {
		wholeHost = wholeHost || prefix == "/"
	}
	if !wholeHost {
		b.WriteString("    # Anything not declared never reaches the tenant.\n")
		b.WriteString("    location / { return 404; }\n")
	}

	for _, prefix := range perimeterPrefixes(e) {
		b.WriteString("\n")
		fmt.Fprintf(&b, "    location %s {\n", prefix)
		for _, deny := range denied {
			fmt.Fprintf(&b, "      location %s { return 404; }\n", deny)
		}
		fmt.Fprintf(&b, "      proxy_pass http://%s:%d;\n", upstreamHost, upstreamPort)
		b.WriteString("      proxy_set_header Host $host;\n")
		b.WriteString("      proxy_set_header X-Forwarded-Proto https;\n")
		b.WriteString("      proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
		b.WriteString("\n")
		b.WriteString("      # Property 2, 3 and 4: nothing of the session model goes in.\n")
		b.WriteString("      # An empty value is what nginx takes for \"do not send this\".\n")
		b.WriteString("      proxy_set_header Cookie \"\";\n")
		b.WriteString("      proxy_set_header Authorization \"\";\n")
		b.WriteString("      proxy_set_header X-Forwarded-Access-Token \"\";\n")
		for _, h := range perimeterStrippedIdentityHeaders {
			fmt.Fprintf(&b, "      proxy_set_header %s \"\";\n", h)
		}
		b.WriteString("\n")
		b.WriteString("      # And nothing of the application's session comes back out:\n")
		b.WriteString("      # a Set-Cookie here would plant a cookie on the public host\n")
		b.WriteString("      # that the browser then carries to every other link on it.\n")
		b.WriteString("      proxy_hide_header Set-Cookie;\n")
		b.WriteString("      proxy_pass_header Server;\n")
		b.WriteString("      proxy_hide_header X-Powered-By;\n")
		if website {
			b.WriteString("      proxy_hide_header X-Content-Type-Options;\n")
			b.WriteString("      add_header X-Content-Type-Options nosniff always;\n")
		}
		b.WriteString("    }\n")
	}
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

// perimeterStrippedIdentityHeaders are what the gateway sets on an
// AUTHENTICATED route to tell a backend who the caller is. On the perimeter
// there is nobody, so an inbound one is somebody claiming to be somebody.
var perimeterStrippedIdentityHeaders = []string{
	"X-Auth-Request-User",
	"X-Auth-Request-Email",
	"X-Auth-Request-Preferred-Username",
	"X-Auth-Request-Groups",
	"X-Auth-Request-Subject",
	"X-Gentian-Tenant",
	"X-Gentian-Subject",
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
