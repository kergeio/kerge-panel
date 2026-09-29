// The install command page: reveal the enrollment token and copy the
// command. The token has its own button, apart from
// the site-wide switch for host addresses, and revealing it is not
// remembered, so a reload hides it again.
(function () {
  "use strict";

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

  var command = document.querySelector("[data-command]");
  if (!command) {
    return;
  }
  var full = command.getAttribute("data-full") || "";
  var masked = command.getAttribute("data-masked") || "";
  var code = command.querySelector("[data-command-text]");
  var toggle = command.querySelector("[data-command-toggle]");
  var copy = command.querySelector("[data-command-copy]");
  var status = command.querySelector("[data-command-status]");

  if (code && toggle) {
    var revealed = false;
    toggle.addEventListener("click", function () {
      revealed = !revealed;
      code.textContent = revealed ? full : masked;
      toggle.textContent = revealed ? t("js.install.hide_token") : t("js.install.show_token");
    });
  }

  if (copy) {
    copy.addEventListener("click", function () {
      function done(key) {
        if (status) {
          status.textContent = t(key);
        }
      }
      if (!navigator.clipboard || !navigator.clipboard.writeText) {
        done("js.copy.failed");
        return;
      }
      navigator.clipboard.writeText(full).then(
        function () {
          done("js.copy.done");
        },
        function () {
          done("js.copy.failed");
        }
      );
    });
  }
})();
