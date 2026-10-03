/* The cluster's name and logo on the sign-in card.
 *
 * The colours arrive by stylesheet (the @import of /branding/brand.css); the
 * name and the logo are content, so they are read from /branding/brand.json,
 * which the operator renders from the Branding. Without it the card keeps the
 * platform's own.
 */
(function () {
  var BASE = "/branding/";
  function apply(brand) {
    if (!brand || typeof brand.name !== "string" || !brand.name) return;
    var titles = document.querySelectorAll(".gentian-login__title");
    for (var i = 0; i < titles.length; i++) titles[i].textContent = brand.name;
    var logos = document.querySelectorAll(".gentian-login__logo");
    var icon = null;
    (brand.icons || []).forEach(function (ic) {
      if (!icon && ic && typeof ic.src === "string" && (!ic.purpose || ic.purpose.split(" ").indexOf("any") >= 0)) icon = ic;
    });
    for (var j = 0; j < logos.length; j++) {
      logos[j].setAttribute("aria-label", brand.name);
      if (icon) {
        var url = new URL(icon.src, location.origin + BASE).href;
        if (/^https:\/\//.test(url)) logos[j].style.backgroundImage = 'url("' + url.replace(/"/g, "") + '")';
      }
    }
    document.title = document.title.replace(/Gentian|Keycloak/g, brand.name);
  }
  function load() {
    fetch(BASE + "brand.json", { cache: "no-cache", credentials: "omit" })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(apply)
      .catch(function () {});
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", load);
  } else {
    load();
  }
})();
