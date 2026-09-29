// The renewal reminder dialog. The server renders it
// into every signed-in page that has due reminders; this opens it and posts
// the operator's answers. Closing it answers "remind me later" for every
// host still listed.
(function () {
  "use strict";

  var dialog = document.querySelector("[data-reminders]");
  if (!dialog || typeof dialog.showModal !== "function") {
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
  var errorBox = dialog.querySelector("[data-reminder-error]");

  function post(hostID, action, keepalive) {
    return fetch("/api/hosts/" + encodeURIComponent(hostID) + "/reminder/" + action, {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-CSRF-Token": token },
      keepalive: keepalive,
    });
  }

  function showError(message) {
    if (errorBox) {
      errorBox.textContent = message;
      errorBox.hidden = false;
    }
  }

  // failure reads the translated message the API sends with an error.
  function failure(response) {
    return response
      .json()
      .then(function (body) {
        return body && typeof body.error === "string" ? body.error : t("js.reminder.failed");
      })
      .catch(function () {
        return t("js.reminder.failed");
      });
  }

  function remaining() {
    return dialog.querySelectorAll("[data-reminder-host]");
  }

  function answer(row, action) {
    var buttons = row.querySelectorAll("button");
    buttons.forEach(function (b) {
      b.disabled = true;
    });
    post(row.getAttribute("data-reminder-host"), action, false)
      .then(function (response) {
        if (!response.ok) {
          return failure(response).then(function (message) {
            throw new Error(message);
          });
        }
        row.remove();
        if (errorBox) {
          errorBox.hidden = true;
        }
        if (remaining().length === 0) {
          dialog.close();
        }
      })
      .catch(function (err) {
        showError(err && err.message ? err.message : t("js.reminder.failed"));
        buttons.forEach(function (b) {
          b.disabled = false;
        });
      });
  }

  dialog.addEventListener("click", function (event) {
    var button = event.target.closest("[data-reminder-action]");
    if (!button || button.disabled) {
      return;
    }
    var row = button.closest("[data-reminder-host]");
    if (row) {
      answer(row, button.getAttribute("data-reminder-action"));
    }
  });

  var closeButton = dialog.querySelector("[data-reminder-close]");
  if (closeButton) {
    closeButton.addEventListener("click", function () {
      dialog.close();
    });
  }

  // However the dialog closes (the close button, Escape), the hosts still
  // listed are snoozed. keepalive lets the requests finish if the page is
  // being left.
  dialog.addEventListener("close", function () {
    remaining().forEach(function (row) {
      post(row.getAttribute("data-reminder-host"), "snooze", true).catch(function () {});
      row.remove();
    });
  });

  dialog.showModal();
})();
