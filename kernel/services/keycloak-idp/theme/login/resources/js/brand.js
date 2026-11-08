/* The brand on the sign-in card: its colours, its name and its logo.
 *
 * The operator renders the brand from the Branding and the concierge serves
 * it on the cluster's bare domain, which is this host without its first
 * label (id.<kernel>). The colours are a stylesheet of --brand-* custom
 * properties, linked here; the name and the logo are content, read from
 * brand.json. Without either the card keeps the platform's own.
 */
(function () {
  var host = location.hostname;
  if (location.protocol !== "https:" || host.indexOf("id.") !== 0) return;
  var BASE = "https://" + host.slice(3) + "/branding/";
  var link = document.createElement("link");
  link.rel = "stylesheet";
  link.href = BASE + "brand.css";
  document.head.appendChild(link);
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
        var url = new URL(icon.src, BASE).href;
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
