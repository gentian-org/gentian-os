// The file dialog's page. It is opened as a window of its own by an app's
// page and speaks to that page by messages:
//
//   dialog -> page   {gentianFiles: "ready"}
//   page -> dialog   {gentianFiles: "ask", use: "pick", id: <any>}
//   dialog -> page   {gentianFiles: "picked", id, file: {name, id, url}}
//   dialog -> page   {gentianFiles: "cancelled", id}
//
// Which app asks is the origin of the message that asks, as the browser
// states it. The answer is sent to that origin and no other.
(function () {
  "use strict";

  var words = {
    en: {
      title: "Choose a file", for: "for {app}", cancel: "Cancel", choose: "Choose",
      files: "Files", empty: "This folder is empty.", loading: "Loading …",
      alone: "This window is opened by an app that asks you for a file. You can close it.",
      unknown: "The app did not say what it asks for."
    },
    de: {
      title: "Datei auswählen", for: "für {app}", cancel: "Abbrechen", choose: "Auswählen",
      files: "Dateien", empty: "Dieser Ordner ist leer.", loading: "Wird geladen …",
      alone: "Dieses Fenster wird von einer App geöffnet, die Sie um eine Datei bittet. Sie können es schließen.",
      unknown: "Die App hat nicht gesagt, worum sie bittet."
    }
  };
  var t = words[(navigator.language || "en").slice(0, 2)] || words.en;

  var el = function (id) { return document.getElementById(id); };
  var asker = null;      // {origin, id}
  var path = [];         // folder names below the person's files
  var chosen = null;     // name of the chosen file

  el("title").textContent = t.title;
  el("cancel").textContent = t.cancel;
  el("choose").textContent = t.choose;
  document.title = t.files;

  function note(text) { el("note").textContent = text || ""; }

  function api(method, url, body) {
    return fetch(url, {
      method: method,
      headers: body ? {"Content-Type": "application/json"} : {},
      body: body ? JSON.stringify(body) : undefined
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) { throw new Error(j.error || ("" + r.status)); }
        return j;
      });
    });
  }

  function tell(message) {
    if (asker && window.opener) {
      message.id = asker.id;
      window.opener.postMessage(message, asker.origin);
    }
  }

  function size(n) {
    if (!n) { return ""; }
    var units = ["B", "kB", "MB", "GB"], i = 0;
    while (n >= 1000 && i < units.length - 1) { n /= 1000; i++; }
    return (i ? n.toFixed(1) : n) + " " + units[i];
  }

  function crumbs() {
    var nav = el("crumbs");
    nav.textContent = "";
    var add = function (label, depth) {
      var b = document.createElement("button");
      b.type = "button";
      b.textContent = label;
      b.addEventListener("click", function () { path = path.slice(0, depth); show(); });
      nav.appendChild(b);
    };
    add(t.files, 0);
    path.forEach(function (name, i) {
      var sep = document.createElement("span");
      sep.textContent = "/";
      nav.appendChild(sep);
      add(name, i + 1);
    });
  }

  function choose(li, name) {
    Array.prototype.forEach.call(el("entries").children, function (other) {
      other.setAttribute("aria-selected", "false");
    });
    li.setAttribute("aria-selected", "true");
    chosen = name;
    el("choose").disabled = false;
  }

  function show() {
    chosen = null;
    el("choose").disabled = true;
    crumbs();
    var list = el("entries");
    list.textContent = "";
    note(t.loading);
    api("GET", "/api/list?path=" + encodeURIComponent(path.join("/"))).then(function (answer) {
      note(answer.entries.length ? "" : t.empty);
      answer.entries.forEach(function (e) {
        var li = document.createElement("li");
        li.tabIndex = 0;
        li.setAttribute("role", "option");
        li.setAttribute("aria-selected", "false");
        var kind = document.createElement("span");
        kind.className = "kind";
        kind.textContent = e.folder ? "▸" : "·";
        var name = document.createElement("span");
        name.className = "name";
        name.textContent = e.name;
        var sz = document.createElement("span");
        sz.className = "size";
        sz.textContent = e.folder ? "" : size(e.size);
        li.appendChild(kind); li.appendChild(name); li.appendChild(sz);
        var open = function () {
          if (e.folder) { path.push(e.name); show(); } else { choose(li, e.name); done(); }
        };
        li.addEventListener("click", function () { if (e.folder) { open(); } else { choose(li, e.name); } });
        li.addEventListener("dblclick", open);
        li.addEventListener("keydown", function (ev) {
          if (ev.key === "Enter") { open(); }
          if (ev.key === " ") { ev.preventDefault(); if (!e.folder) { choose(li, e.name); } }
        });
        list.appendChild(li);
      });
    }).catch(function (err) { note(err.message); });
  }

  function done() {
    if (!chosen || !asker) { return; }
    el("choose").disabled = true;
    api("POST", "/api/pick", {origin: asker.origin, path: path.concat([chosen]).join("/")}).then(function (file) {
      tell({gentianFiles: "picked", file: file});
      asker = null;
      window.close();
    }).catch(function (err) { note(err.message); el("choose").disabled = false; });
  }

  el("choose").addEventListener("click", done);
  el("cancel").addEventListener("click", function () {
    tell({gentianFiles: "cancelled"});
    asker = null;
    window.close();
  });
  window.addEventListener("pagehide", function () { tell({gentianFiles: "cancelled"}); });

  window.addEventListener("message", function (ev) {
    if (asker || ev.source !== window.opener || !ev.data || ev.data.gentianFiles !== "ask") { return; }
    if (ev.data.use !== "pick") { note(t.unknown); return; }
    var origin = ev.origin;
    api("GET", "/api/ask?use=pick&origin=" + encodeURIComponent(origin)).then(function (answer) {
      asker = {origin: origin, id: ev.data.id};
      el("for").textContent = t.for.replace("{app}", new URL(origin).host);
      el("for").hidden = false;
      show();
    }).catch(function (err) { note(err.message); });
  });

  if (window.opener) {
    // The page that opened this window is told it can ask. Told to any
    // origin: the message says nothing, and who answers is read from the
    // answer.
    window.opener.postMessage({gentianFiles: "ready"}, "*");
  } else {
    note(t.alone);
    el("choose").hidden = true;
  }
})();
