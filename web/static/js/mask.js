// The navigation bar's switch that shows or masks every host address on
// the page. The panel stores the choice, so the next
// page renders the same way; this only saves it and updates the page in
// place. Values are written as text, never markup.
(function () {
  "use strict";

  var button = document.querySelector("[data-reveal-toggle]");
  if (!button) {
    return;
  }

  var text = {};
  var block = document.getElementById("i18n");
  if (block) {
    try {
      text = JSON.parse(block.textContent) || {};
    } catch (e) {
      text = {};
    }
  }
  function t(key) {
    return typeof text[key] === "string" ? text[key] : key;
  }

  var HIDDEN = "***"; // matches what the server renders
  var meta = document.querySelector('meta[name="csrf-token"]');
  var token = meta ? meta.getAttribute("content") : "";

  function apply(shown) {
    Array.prototype.forEach.call(document.querySelectorAll("[data-mask]"), function (el) {
      el.textContent = shown ? el.getAttribute("data-value") || "" : HIDDEN;
    });
    var label = t(shown ? "js.reveal.hide" : "js.reveal.show");
    button.setAttribute("aria-pressed", shown ? "true" : "false");
    button.setAttribute("aria-label", label);
    button.setAttribute("title", label);
    Array.prototype.forEach.call(button.querySelectorAll("[data-reveal-icon]"), function (icon) {
      var wanted = shown ? "shown" : "masked";
      icon.classList.toggle("hidden", icon.getAttribute("data-reveal-icon") !== wanted);
    });
  }

  // The page changes only once the panel has stored the choice, so what
  // is on screen never disagrees with what the next page will show.
  var busy = false;
  button.addEventListener("click", function () {
    if (busy) {
      return;
    }
    var next = button.getAttribute("aria-pressed") !== "true";
    busy = true;
    fetch("/api/settings/reveal", {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-CSRF-Token": token, "Content-Type": "application/x-www-form-urlencoded" },
      body: "reveal=" + (next ? "1" : "0"),
    })
      .then(function (response) {
        if (response.ok) {
          apply(next);
        }
      })
      .catch(function () {})
      .then(function () {
        busy = false;
      });
  });
})();
