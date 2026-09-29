// Keeps the dashboard current over the panel's WebSocket, runs the clock
// of the overview and switches between cards and list.
// The panel sends formatted strings, so this only writes text into the
// elements that are already on the page.
(function () {
  "use strict";

  var overview = document.querySelector("[data-overview]");
  if (!overview) {
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

  // fmtDate writes a date in the panel's format, a Go layout such as
  // "02/01/2006".
  function fmtDate(layout, year, month, day) {
    return (layout || "2006-01-02").replace(/2006|01|02/g, function (token) {
      return token === "2006" ? year : token === "01" ? month : day;
    });
  }

  // The clock shows the panel's time zone, whatever the browser's is.
  function startClock() {
    var layout = overview.getAttribute("data-date-format");
    var timeEl = overview.querySelector("[data-clock-time]");
    var dateEl = overview.querySelector("[data-clock-date]");
    var format;
    try {
      format = new Intl.DateTimeFormat("en-US", {
        timeZone: overview.getAttribute("data-time-zone") || "UTC",
        hourCycle: "h23",
        year: "numeric",
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
      });
    } catch (e) {
      return;
    }
    function tick() {
      var parts = {};
      format.formatToParts(new Date()).forEach(function (p) {
        parts[p.type] = p.value;
      });
      if (timeEl) {
        timeEl.textContent = parts.hour + ":" + parts.minute + ":" + parts.second;
      }
      if (dateEl) {
        var weekday = new Date(Date.UTC(+parts.year, +parts.month - 1, +parts.day)).getUTCDay();
        dateEl.textContent = t("js.weekday." + weekday) + " " + fmtDate(layout, parts.year, parts.month, parts.day);
      }
    }
    tick();
    window.setInterval(tick, 1000);
  }

  // Classes that come from the panel replace the ones it sent before,
  // which the element remembers in data-classes.
  function swapClasses(el, value) {
    if (typeof value !== "string") {
      return;
    }
    (el.getAttribute("data-classes") || "").split(" ").forEach(function (c) {
      if (c) {
        el.classList.remove(c);
      }
    });
    value.split(" ").forEach(function (c) {
      if (c) {
        el.classList.add(c);
      }
    });
    el.setAttribute("data-classes", value);
  }

  // The machine's size is only in a push that has a sample; without one
  // the page keeps what the panel rendered.
  var KEEP_WHEN_EMPTY = { cores: true, mem_total: true, disk_total: true };

  function applyTo(el, host) {
    Array.prototype.forEach.call(el.querySelectorAll("[data-field]"), function (field) {
      var name = field.getAttribute("data-field");
      var value = host[name];
      if (value && typeof value === "object") {
        value = value.text;
      }
      if (typeof value !== "string" || (value === "" && KEEP_WHEN_EMPTY[name])) {
        return;
      }
      field.textContent = value;
    });
    // The tag and the list's line on a host that is not reporting show
    // only with something to say, and carry the full text as a tooltip.
    [["[data-tag]", "tag", "tag_title"], ["[data-detail]", "detail_time", "detail"]].forEach(function (spec) {
      Array.prototype.forEach.call(el.querySelectorAll(spec[0]), function (box) {
        if (typeof host[spec[1]] === "string") {
          box.hidden = host[spec[1]] === "";
        }
        if (typeof host[spec[2]] === "string") {
          box.setAttribute("title", host[spec[2]]);
        }
      });
    });
    Array.prototype.forEach.call(el.querySelectorAll("[data-title-field]"), function (field) {
      var title = host[field.getAttribute("data-title-field")];
      if (typeof title === "string") {
        field.setAttribute("title", title);
      }
    });
    Array.prototype.forEach.call(el.querySelectorAll("[data-icon-field]"), function (icon) {
      icon.toggleAttribute("hidden", host[icon.getAttribute("data-icon-field")] !== icon.getAttribute("data-icon"));
    });
    Array.prototype.forEach.call(el.querySelectorAll("[data-class-field]"), function (field) {
      swapClasses(field, host[field.getAttribute("data-class-field")]);
    });
    Array.prototype.forEach.call(el.querySelectorAll("[data-meter]"), function (holder) {
      var m = host[holder.getAttribute("data-meter")];
      var bar = holder.querySelector("progress");
      if (!bar || !m || typeof m !== "object") {
        return;
      }
      bar.value = typeof m.value === "number" ? m.value : 0;
      bar.setAttribute("data-level", typeof m.level === "string" ? m.level : "");
    });
  }

  function applyOverview(data) {
    if (!data || typeof data !== "object") {
      return;
    }
    Array.prototype.forEach.call(overview.querySelectorAll("[data-overview-field]"), function (field) {
      var value = data[field.getAttribute("data-overview-field")];
      if (typeof value === "string") {
        field.textContent = value;
      }
    });
    Array.prototype.forEach.call(overview.querySelectorAll("[data-overview-alert]"), function (tile) {
      tile.toggleAttribute("data-alert", data[tile.getAttribute("data-overview-alert") + "_alert"] === true);
    });
  }

  var reloading = false;
  function update(payload) {
    if (!payload || !Array.isArray(payload.hosts)) {
      return;
    }
    // A host added or deleted elsewhere changes the set of cards, which
    // only a fresh page can render.
    var known = document.querySelectorAll('[data-view="cards"] [data-host-id]').length;
    if (payload.hosts.length !== known && !reloading) {
      reloading = true;
      window.location.reload();
      return;
    }
    payload.hosts.forEach(function (host) {
      var id = String(host.id);
      Array.prototype.forEach.call(document.querySelectorAll("[data-host-id]"), function (el) {
        if (el.getAttribute("data-host-id") === id) {
          applyTo(el, host);
        }
      });
    });
    applyOverview(payload.overview);
  }

  var indicator = document.querySelector("[data-live-status]");
  function note(key) {
    if (indicator) {
      indicator.textContent = key ? t(key) : "";
    }
  }

  var retry = 1000;
  function connect() {
    var url = (window.location.protocol === "https:" ? "wss://" : "ws://") + window.location.host + "/api/live";
    var socket;
    try {
      socket = new WebSocket(url);
    } catch (e) {
      window.setTimeout(connect, retry);
      return;
    }
    socket.addEventListener("open", function () {
      retry = 1000;
      note("");
    });
    socket.addEventListener("message", function (event) {
      var payload;
      try {
        payload = JSON.parse(event.data);
      } catch (e) {
        return;
      }
      update(payload);
    });
    socket.addEventListener("close", function () {
      note("js.live.reconnecting");
      window.setTimeout(connect, retry);
      retry = Math.min(retry * 2, 30000);
    });
    socket.addEventListener("error", function () {
      socket.close();
    });
  }

  // The view is stored by the panel, so the page switches only once it
  // is saved and the next visit shows the same one.
  var buttons = document.querySelectorAll("[data-view-button]");
  var busy = false;
  // Which view shows is up to the stylesheet: the list only on a screen
  // at least 980 pixels wide, the cards otherwise.
  var views = document.querySelector("[data-views]");
  function showView(view) {
    if (views) {
      views.setAttribute("data-views", view);
    }
    Array.prototype.forEach.call(buttons, function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-view-button") === view ? "true" : "false");
    });
  }
  Array.prototype.forEach.call(buttons, function (button) {
    button.addEventListener("click", function () {
      if (busy || button.getAttribute("aria-pressed") === "true") {
        return;
      }
      var view = button.getAttribute("data-view-button");
      busy = true;
      fetch("/api/settings/view", {
        method: "POST",
        credentials: "same-origin",
        headers: { "X-CSRF-Token": token, "Content-Type": "application/x-www-form-urlencoded" },
        body: "view=" + encodeURIComponent(view),
      })
        .then(function (response) {
          if (response.ok) {
            showView(view);
          }
        })
        .catch(function () {})
        .then(function () {
          busy = false;
        });
    });
  });

  startClock();
  connect();
})();
