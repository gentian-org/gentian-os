// The browser's half of the proof-of-work check on the registration page
// (gentian-pow-captcha): find a number whose SHA-256, together with the salt
// the page carries, begins with as many zero bits as the page asks for, and
// put it in the form. About a second of work; the submit button waits for it.
//
// Nothing is loaded from anywhere and nothing is sent anywhere: the digest is
// the browser's own crypto.subtle, in a worker so the page stays responsive,
// on the main thread where there is no worker.
(function () {
  "use strict";
  var box = document.getElementById("gentian-pow");
  if (!box) { return; }
  var salt = box.getAttribute("data-salt") || "";
  var bits = parseInt(box.getAttribute("data-bits") || "0", 10);
  var answer = document.getElementById("gentian-pow-answer");
  var status = document.getElementById("gentian-pow-status");
  var form = document.getElementById("kc-register-form");
  var submit = form ? form.querySelector("input[type=submit], button[type=submit]") : null;
  if (submit) { submit.disabled = true; }

  // The same arithmetic as PowChallenge.java, written once here and run in
  // either place.
  var work = function (salt, bits, done) {
    var enc = new TextEncoder();
    var leading = function (buf) {
      var bytes = new Uint8Array(buf), n = 0;
      for (var i = 0; i < bytes.length; i++) {
        if (bytes[i] === 0) { n += 8; continue; }
        n += Math.clz32(bytes[i]) - 24;
        break;
      }
      return n;
    };
    var nonce = 0;
    var step = function () {
      var batch = 256;
      var next = function () {
        if (batch-- === 0) { setTimeout(step, 0); return; }
        crypto.subtle.digest("SHA-256", enc.encode(salt + ":" + nonce)).then(function (d) {
          if (leading(d) >= bits) { done(String(nonce)); return; }
          nonce++;
          next();
        });
      };
      next();
    };
    step();
  };

  var solved = function (nonce) {
    answer.value = nonce;
    if (status) { status.textContent = status.getAttribute("data-done") || ""; status.classList.add("gentian-pow-status--done"); }
    if (submit) { submit.disabled = false; }
  };

  var source = "(" + work.toString() + ")(" + JSON.stringify(salt) + "," + bits + ",function(n){postMessage(n)})";
  var worker = null;
  try {
    if (window.Worker && window.Blob && window.URL) {
      worker = new Worker(URL.createObjectURL(new Blob([source], { type: "text/javascript" })));
      worker.onmessage = function (e) { solved(e.data); worker.terminate(); };
      worker.onerror = function () { worker.terminate(); work(salt, bits, solved); };
    }
  } catch (e) { worker = null; }
  if (!worker) { work(salt, bits, solved); }
})();
