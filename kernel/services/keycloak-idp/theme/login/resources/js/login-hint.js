/* The address the sign-in router was given, filled in for the person.
 *
 * The router (id.<kernel>/sign-in/) asks for an e-mail address and sends the
 * browser to its workspace's console; the edge starts the sign-in there, and
 * nothing on that path can carry a login_hint to this form. So the router
 * leaves the address in a cookie only this host's realm pages receive, for
 * ten minutes, and this reads it once: a field the person already typed in
 * is left alone, and the cookie is gone either way.
 */
(function () {
  var NAME = "gentian_login_hint";
  var match = document.cookie.match(new RegExp("(?:^|; )" + NAME + "=([^;]*)"));
  if (!match) return;
  document.cookie = NAME + "=; Path=/auth/realms/; Max-Age=0; Secure; SameSite=Lax";
  var address;
  try {
    address = decodeURIComponent(match[1]);
  } catch (e) {
    return;
  }
  function fill() {
    var field = document.getElementById("username");
    if (!field || field.value) return;
    field.value = address;
    var password = document.getElementById("password");
    if (password) password.focus();
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", fill);
  } else {
    fill();
  }
})();
