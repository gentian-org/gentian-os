/* The address the sign-in router was given, filled in for the person.
 *
 * The router sends the browser to the workspace's console with the address on
 * it as ?login_hint=. The console is behind the edge, which starts the
 * sign-in and passes the address it was asked for to this page inside the
 * request's `state` -- so the hint is in this page's own URL, and reading it
 * needs no cookie and nothing stored anywhere.
 *
 * The edge's `state` is its own format and has changed between releases:
 * "url=<address>&nonce=..." or the same fields as base64url JSON. Both are
 * read. One this does not recognise fills nothing, which is the safe way to
 * be wrong. The hint only ever becomes the value of the username field, and
 * only when the person has not typed one.
 */
(function () {
  function original(state) {
    if (!state) return "";
    var plain = new URLSearchParams(state).get("url");
    if (plain) return plain;
    try {
      var padded = state.replace(/-/g, "+").replace(/_/g, "/");
      while (padded.length % 4) padded += "=";
      var parsed = JSON.parse(atob(padded));
      return typeof parsed.url === "string" ? parsed.url : "";
    } catch (e) {
      return "";
    }
  }
  function hint() {
    try {
      var url = original(new URLSearchParams(window.location.search).get("state"));
      if (!url) return "";
      var value = new URL(url).searchParams.get("login_hint") || "";
      // A username or an address, nothing longer or stranger.
      return /^[^\s<>"'\\]{1,254}$/.test(value) ? value : "";
    } catch (e) {
      return "";
    }
  }
  function fill() {
    var value = hint();
    var field = document.getElementById("username");
    if (!value || !field || field.value) return;
    field.value = value;
    var password = document.getElementById("password");
    if (password) password.focus();
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", fill);
  } else {
    fill();
  }
})();
