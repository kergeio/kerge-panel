// The settings page's "Send test" button. It posts the webhook address and
// request body as they are in the form, saved or not, and shows the
// webhook's reply: the status, then its body and headers under two tabs.
// Everything is written as text, never markup.
(function () {
  "use strict";

  var form = document.querySelector("[data-notify-form]");
  if (!form) {
    return;
  }
  var button = form.querySelector("[data-notify-test]");
  var status = form.querySelector("[data-notify-status]");
  var result = form.querySelector("[data-notify-result]");
  var tabs = form.querySelectorAll("[data-notify-tab]");
  var panels = form.querySelectorAll("[data-notify-panel]");
  if (!button || !status || !result) {
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

  var meta = document.querySelector('meta[name="csrf-token"]');
  var token = meta ? meta.getAttribute("content") : "";

  function panel(name) {
    return form.querySelector('[data-notify-panel="' + name + '"]');
  }

  function show(name) {
    Array.prototype.forEach.call(tabs, function (tab) {
      tab.setAttribute("aria-selected", tab.getAttribute("data-notify-tab") === name ? "true" : "false");
    });
    Array.prototype.forEach.call(panels, function (p) {
      p.hidden = p.getAttribute("data-notify-panel") !== name;
    });
  }

  // report shows the status line; ok is true, false, or null while sending.
  function report(ok, message) {
    status.textContent = message;
    if (ok === null) {
      status.removeAttribute("data-ok");
    } else {
      status.setAttribute("data-ok", ok ? "true" : "false");
    }
  }

  Array.prototype.forEach.call(tabs, function (tab) {
    tab.addEventListener("click", function () {
      show(tab.getAttribute("data-notify-tab"));
    });
  });

  button.addEventListener("click", function () {
    if (button.disabled) {
      return;
    }
    button.disabled = true;
    result.hidden = true;
    report(null, t("js.notify.sending"));

    var params = new URLSearchParams();
    params.set("url", form.elements.webhook_url.value);
    params.set("body", form.elements.webhook_body.value);
    fetch("/api/settings/notifications/test", {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-CSRF-Token": token, "Content-Type": "application/x-www-form-urlencoded" },
      body: params.toString(),
    })
      .then(function (response) {
        return response
          .json()
          .catch(function () {
            return null;
          })
          .then(function (data) {
            if (!data) {
              report(false, t("js.notify.failed"));
              return;
            }
            if (!response.ok || typeof data.status !== "string" || data.status === "") {
              // A form error from the panel, or no reply from the webhook.
              report(false, typeof data.error === "string" ? data.error : t("js.notify.failed"));
              return;
            }
            report(data.ok === true, data.status);
            panel("body").textContent = data.body ? data.body : t("js.notify.empty");
            panel("headers").textContent = data.headers ? data.headers : t("js.notify.empty");
            show("body");
            result.hidden = false;
          });
      })
      .catch(function () {
        report(false, t("js.notify.failed"));
      })
      .then(function () {
        button.disabled = false;
      });
  });
})();
