/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

// A sign-in that comes back to the edge after the edge has forgotten it -- a
// login page left open past csrfTokenTTL, or a callback in a browser that
// lost the flow's cookie -- is refused by the OIDC filter with a bare 401,
// "OAuth flow failed.". Retrying is always safe: a new flow has a new nonce
// and proves everything the old one would have. So instead of the bare text
// the edge serves a page that starts a new sign-in once, back to the page the
// person asked for, and offers a link after that.
//
// Only the filter's own refusals are rewritten: Envoy-generated (source
// Local) 401s in text/plain. The bouncer's refusals are HTML and keep their
// page. A background request the filter refuses (denyRedirect) gets the same
// page as its body, which no fetch renders; its status stays 401.
//
// The page cannot loop. It restarts on its own only on the callback path and
// only when it has not done so in the last two minutes in this tab; a
// browser that keeps nothing in sessionStorage gets the link and no restart.
// Where it goes is the address the filter kept in the flow's state, if that
// is on this host, and the host's root otherwise -- never another origin.
//
// The body contains no percent sign: Envoy reads one as a command operator.
const edgeSignInRestartPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sign in again</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">
<h1 style="font-size:1.25rem">Your sign-in did not finish</h1>
<p>The sign-in took longer than this site waits for it, so it has to start over.</p>
<p><a id="again" href="/">Sign in again</a></p>
<script>
(function () {
  var key = "gentian-sign-in-restart", target = "/";
  try {
    var state = new URLSearchParams(location.search).get("state");
    if (state) {
      var kept = JSON.parse(atob(state.replace(/-/g, "+").replace(/_/g, "/")));
      var url = new URL(kept.url, location.origin);
      if (url.origin === location.origin) { target = url.pathname + url.search + url.hash; }
    }
  } catch (e) {}
  document.getElementById("again").href = target;
  if (location.pathname.indexOf("/oauth2/callback") !== 0) { return; }
  var now = Date.now(), last = 0;
  try {
    last = Number(sessionStorage.getItem(key)) || 0;
    sessionStorage.setItem(key, String(now));
  } catch (e) { return; }
  if (now - last > 120000) { location.replace(target); }
})();
</script>
</body></html>
`

// edgeSignInRestart is the responseOverride of a BackendTrafficPolicy on
// routes behind a session: the authenticated Gateway's, and the routes on
// other Gateways that carry a session of their own.
func edgeSignInRestart() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"match": map[string]interface{}{
				"statusCodes": []interface{}{
					map[string]interface{}{"type": "Value", "value": int64(401)},
				},
				"responseHeaders": []interface{}{
					map[string]interface{}{
						"name":  "content-type",
						"value": map[string]interface{}{"type": "Prefix", "value": "text/plain"},
					},
				},
			},
			"source": "Local",
			"response": map[string]interface{}{
				"contentType": "text/html; charset=utf-8",
				"body":        map[string]interface{}{"type": "Inline", "inline": edgeSignInRestartPage},
			},
		},
	}
}
