// Draws the trends of one host with uPlot.
//
// Every chart draws one line; which column that line comes from is
// decided by the panel and carried in the frame's data attributes, so
// this script never has to know what a chart means. The panel sends
// readings as numbers, so units are formatted here; the unit symbols are
// international notation and are not translated, and the rules match
// internal/i18n/format.go. Everything written into the page goes through
// textContent or through uPlot itself, never as markup.
(function () {
  "use strict";

  var root = document.querySelector("[data-charts]");
  if (!root || typeof uPlot === "undefined") {
    return;
  }

  function readJSON(id) {
    var block = document.getElementById(id);
    if (!block) {
      return null;
    }
    try {
      return JSON.parse(block.textContent);
    } catch (e) {
      return null;
    }
  }

  var config = readJSON("chart-config");
  if (!config) {
    return;
  }
  var text = readJSON("i18n") || {};
  function t(key) {
    return typeof text[key] === "string" ? text[key] : key;
  }

  // Formatting, mirroring internal/i18n/format.go.

  var BYTE_UNITS = ["KiB", "MiB", "GiB", "TiB", "PiB", "EiB"];
  var RATE_UNITS = ["K/s", "M/s", "G/s", "T/s"];
  var MISSING = "--";

  function roundTenth(v) {
    return Math.round(v * 10) / 10;
  }

  function fmtPercent(v) {
    return v.toFixed(1) + "%";
  }

  // A load average keeps one digit more than the panel's other decimals:
  // on an idle host a single one turns the chart into a staircase.
  function fmtNumber(v) {
    return v.toFixed(2);
  }

  function fmtBytes(v) {
    if (v < 1024) {
      return Math.round(v) + " B";
    }
    var x = v / 1024;
    var i = 0;
    while (roundTenth(x) >= 1024 && i < BYTE_UNITS.length - 1) {
      x /= 1024;
      i++;
    }
    return x.toFixed(1) + " " + BYTE_UNITS[i];
  }

  function fmtRate(v) {
    var x = Math.max(v, 0);
    if (Math.round(x) < 1024) {
      return x.toFixed(0) + " B/s";
    }
    x /= 1024;
    var i = 0;
    while (roundTenth(x) >= 1024 && i < RATE_UNITS.length - 1) {
      x /= 1024;
      i++;
    }
    return x.toFixed(1) + " " + RATE_UNITS[i];
  }

  var FORMATS = { percent: fmtPercent, bytes: fmtBytes, rate: fmtRate, number: fmtNumber };

  // Axis labels are a ruler, not a reading: they carry a prefix and no
  // unit, which keeps every label short enough for one fixed axis width
  // and so keeps all the charts the same shape. The
  // unit itself is in the chart's title, and the hover readout gives the
  // exact value with its full unit.
  //
  // Byte and rate axes step by 1024 and write the plain prefix, the way
  // "ls -lh" and "df -h" do; the tooltip beside them says KiB or M/s for
  // the same number.
  var PREFIXES = ["", "K", "M", "G", "T", "P", "E"];

  // mantissa drops a trailing ".0", so a round number is one character
  // shorter than a fraction.
  function mantissa(v) {
    var s = v.toFixed(1);
    return s.slice(-2) === ".0" ? s.slice(0, -2) : s;
  }

  // scaled divides by step until the number is below 1000, which bounds
  // every label at four digits and a prefix.
  function scaled(v, step, prefixes) {
    var i = 0;
    while (Math.abs(v) >= 1000 && i < prefixes.length - 1) {
      v /= step;
      i++;
    }
    return mantissa(v) + prefixes[i];
  }

  function axisPercent(v) {
    return mantissa(v) + "%";
  }

  function axisBytes(v) {
    return scaled(v, 1024, PREFIXES);
  }

  // Rates are bytes per second, in the same binary steps as sizes.
  function axisRate(v) {
    return scaled(Math.max(v, 0), 1024, PREFIXES);
  }

  function axisNumber(v) {
    return Math.abs(v) >= 1000 ? scaled(v, 1000, PREFIXES) : String(v);
  }

  // A rate axis ticks at binary steps (1, 2, 4 ... 512 of B, K, M ...),
  // so its labels come out round, such as 16K or 2M, rather than the
  // decimal steps uPlot picks by default, which read 24.4K in binary.
  var RATE_INCRS = [];
  for (var p = 0; p < 5; p++) {
    for (var m = 1; m < 1024; m *= 2) {
      RATE_INCRS.push(m * Math.pow(1024, p));
    }
  }

  var AXIS_FORMATS = {
    percent: axisPercent,
    bytes: axisBytes,
    rate: axisRate,
    number: axisNumber,
  };

  function pad(n) {
    return (n < 10 ? "0" : "") + n;
  }

  // zoned is the moment ts in the panel's zone.
  function zoned(ts) {
    return config.time_zone ? uPlot.tzDate(new Date(ts * 1000), config.time_zone) : new Date(ts * 1000);
  }

  // fmtDate writes the date of d in a Go layout such as "02/01/2006",
  // the panel's date format with or without the year.
  function fmtDate(layout, d) {
    return layout.replace(/2006|01|02/g, function (token) {
      return token === "2006" ? String(d.getFullYear()) : token === "01" ? pad(d.getMonth() + 1) : pad(d.getDate());
    });
  }

  function fmtTime(d) {
    return pad(d.getHours()) + ":" + pad(d.getMinutes());
  }

  var DATE_LAYOUT = config.date_format || "2006-01-02";
  var SHORT_LAYOUT = config.short_date_format || "01-02";

  // stamp formats a point's time in the panel's zone, to the minute: a
  // point is a summarized bucket, never a single sample.
  function stamp(ts) {
    var d = zoned(ts);
    var s = fmtDate(DATE_LAYOUT, d) + " " + fmtTime(d);
    return config.time_zone ? s + " (" + config.time_zone + ")" : s;
  }

  // timeTicks labels the time axis on a 24-hour clock: a tick shows the
  // time, and under it the short date where the day changes; ticks a day
  // or more apart show the short date alone.
  function timeTicks(self, splits, axisIdx, space, incr) {
    var last = "";
    return splits.map(function (ts) {
      var d = zoned(ts);
      var day = fmtDate(SHORT_LAYOUT, d);
      if (incr >= 86400) {
        return day;
      }
      var label = fmtTime(d);
      if (day !== last) {
        label += "\n" + day;
        last = day;
      }
      return label;
    });
  }

  var LINE = "#2563eb";
  var PLOT_HEIGHT = 200; // matches the height reserved in the template
  // Room for the widest label a compact axis can produce ("1023.9G" is
  // 45 pixels) plus the tick and its gap. It is the same for every
  // chart, so the plots line up.
  var AXIS_WIDTH = 76;
  var TIP_OFFSET = 12;

  var resolution = document.querySelector("[data-chart-resolution]");
  var timeLabel = document.querySelector("[data-chart-time]");
  var status = document.querySelector("[data-chart-status]");
  var refresh = document.querySelector("[data-chart-refresh]");
  var statMenu = document.querySelector("[data-stat-menu]");
  var statLabel = document.querySelector("[data-stat-label]");
  var statItems = Array.prototype.slice.call(document.querySelectorAll("[data-chart-stat]"));

  function note(chart, key) {
    chart.note.textContent = key ? t(key) : "";
  }

  // The charts on the page. What each one draws comes from the panel.
  var charts = Array.prototype.map.call(root.querySelectorAll("[data-chart]"), function (box) {
    return {
      name: box.getAttribute("data-chart"),
      box: box,
      plot: box.querySelector("[data-chart-plot]"),
      note: box.querySelector("[data-chart-note]"),
      tip: box.querySelector("[data-chart-tip]"),
      tipTime: box.querySelector("[data-tip-time]"),
      tipMean: box.querySelector("[data-tip-mean]"),
      tipMax: box.querySelector("[data-tip-max]"),
      unit: box.getAttribute("data-unit"),
      format: FORMATS[box.getAttribute("data-unit")] || fmtNumber,
      axisFormat: AXIS_FORMATS[box.getAttribute("data-unit")] || axisNumber,
      mean: box.getAttribute("data-mean"),
      max: box.getAttribute("data-max"),
      capacity: box.getAttribute("data-capacity"),
      axisMax: parseFloat(box.getAttribute("data-axis-max")),
      u: null,
    };
  });

  // The window on screen and the statistic drawn from it.
  var shown = null;
  var stat = config.stat === "max" ? "max" : "mean";

  // column returns the column a chart draws for the current statistic,
  // falling back to the average when the window has no maxima.
  function column(chart, payload) {
    if (stat === "max" && chart.max && Array.isArray(payload[chart.max])) {
      return chart.max;
    }
    return chart.mean;
  }

  // ceiling is the top of a chart's axis: a fixed one, the host's
  // capacity, or nothing, in which case the data decides.
  function ceiling(chart, payload) {
    if (!isNaN(chart.axisMax)) {
      return chart.axisMax;
    }
    if (chart.capacity && Array.isArray(payload[chart.capacity])) {
      var top = 0;
      payload[chart.capacity].forEach(function (v) {
        if (typeof v === "number" && v > top) {
          top = v;
        }
      });
      return top;
    }
    return 0;
  }

  // Every reading is a rate, a share or a size, so no axis starts below
  // zero and a flat line does not fill the whole plot.
  function axis(top) {
    return function (self, min, max) {
      var high = max > 0 ? max : 1;
      return [0, top > high ? top : high];
    };
  }

  // Hovering.

  function hideTip(chart) {
    chart.tip.hidden = true;
  }

  function showTip(chart, idx) {
    if (!shown || idx == null || !chart.u) {
      hideTip(chart);
      return;
    }
    chart.tipTime.textContent = stamp(shown.t[idx]);

    var mean = shown[chart.mean] ? shown[chart.mean][idx] : null;
    var peak = chart.max && shown[chart.max] ? shown[chart.max][idx] : null;
    // A window of raw samples has one reading per point, so it is shown
    // without a label; a summarized one shows both statistics whichever
    // is drawn.
    if (peak == null) {
      chart.tipMean.textContent = typeof mean === "number" ? chart.format(mean) : MISSING;
      chart.tipMax.textContent = "";
    } else {
      chart.tipMean.textContent =
        t("js.chart.mean") + " " + (typeof mean === "number" ? chart.format(mean) : MISSING);
      chart.tipMax.textContent = t("js.chart.max") + " " + chart.format(peak);
    }

    chart.tip.hidden = false;
    var left = chart.u.over.offsetLeft + chart.u.valToPos(shown.t[idx], "x") + TIP_OFFSET;
    if (left + chart.tip.offsetWidth > chart.plot.clientWidth) {
      left = left - chart.tip.offsetWidth - 2 * TIP_OFFSET;
    }
    chart.tip.style.left = Math.max(left, 0) + "px";
    chart.tip.style.top = TIP_OFFSET + "px";
  }

  // The chart the pointer is actually over. The cursor is synchronized,
  // so every chart reacts to one hover, but only the hovered one speaks
  // for the time shown above the charts.
  var active = null;

  function showTime(idx) {
    if (!timeLabel || !shown || !shown.t.length) {
      return;
    }
    var i = idx == null ? shown.t.length - 1 : idx;
    timeLabel.textContent = stamp(shown.t[i]);
  }

  function onCursor(chart, idx) {
    showTip(chart, idx);
    if (active === null || active === chart) {
      showTime(idx);
    }
  }

  // hover resets every chart, for a fresh window or a pointer that left.
  function hover(idx) {
    charts.forEach(function (chart) {
      showTip(chart, idx);
    });
    showTime(idx);
  }

  function build(chart, payload) {
    var col = column(chart, payload);
    var values = payload[col];
    if (chart.u) {
      chart.u.destroy();
      chart.u = null;
    }
    hideTip(chart);
    // A chart the answer has no column for is one this host does not
    // report, such as the load average on a platform without one.
    if (!Array.isArray(values)) {
      chart.box.hidden = true;
      return;
    }
    chart.box.hidden = false;
    note(chart, "");
    chart.u = new uPlot(
      {
        width: Math.max(chart.plot.clientWidth, 120),
        height: PLOT_HEIGHT,
        tzDate: config.time_zone
          ? function (ts) {
              return uPlot.tzDate(new Date(ts * 1000), config.time_zone);
            }
          : undefined,
        legend: { show: false },
        cursor: { sync: { key: "kerge-host" } },
        // The time axis spans the whole window up to now, so a gap, such
        // as the time since a host went offline, shows as one.
        scales: {
          x: { time: true, range: [payload.from, payload.to] },
          y: { range: axis(ceiling(chart, payload)) },
        },
        axes: [
          { values: timeTicks },
          {
            size: AXIS_WIDTH,
            incrs: chart.unit === "rate" ? RATE_INCRS : undefined,
            values: function (self, ticks) {
              return ticks.map(chart.axisFormat);
            },
          },
        ],
        series: [{}, { stroke: LINE, width: 1.5 }],
        hooks: {
          setCursor: [
            function (u) {
              // The point uPlot drew for this series, which is what the
              // dot sits on; u.cursor.idx is the one nearest the
              // pointer and the two can differ.
              onCursor(chart, u.cursor.idxs[1]);
            },
          ],
        },
      },
      [payload.t, values],
      chart.plot
    );
  }

  function draw() {
    if (!shown) {
      return;
    }
    charts.forEach(function (chart) {
      build(chart, shown);
    });
    hover(null);
  }

  function render(payload) {
    if (resolution) {
      resolution.textContent = typeof payload.note === "string" ? payload.note : "";
    }
    if (!Array.isArray(payload.t) || payload.t.length === 0) {
      shown = null;
      if (timeLabel) {
        timeLabel.textContent = "";
      }
      charts.forEach(function (chart) {
        if (chart.u) {
          chart.u.destroy();
          chart.u = null;
        }
        hideTip(chart);
        chart.box.hidden = false;
        if (config.offline_since) {
          chart.note.textContent = t("js.chart.offline").replace("{since}", config.offline_since);
        } else {
          note(chart, "js.chart.empty");
        }
      });
      return;
    }
    shown = payload;
    draw();
    if (status) {
      status.textContent = payload.truncated ? t("js.chart.truncated") : "";
    }
  }

  var loading = false;
  function load() {
    if (loading) {
      return;
    }
    loading = true;
    if (refresh) {
      refresh.disabled = true;
    }
    charts.forEach(function (chart) {
      if (!chart.u) {
        note(chart, "js.chart.loading");
      }
    });
    var url =
      "/api/hosts/" +
      encodeURIComponent(String(config.host)) +
      "/metrics?range=" +
      encodeURIComponent(String(config.range));
    window
      .fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then(function (resp) {
        if (!resp.ok) {
          throw new Error(String(resp.status));
        }
        return resp.json();
      })
      .then(render)
      .catch(function () {
        charts.forEach(function (chart) {
          if (!chart.u) {
            note(chart, "js.chart.failed");
          }
        });
        if (status) {
          status.textContent = t("js.chart.failed");
        }
      })
      .then(function () {
        loading = false;
        if (refresh) {
          refresh.disabled = false;
        }
      });
  }

  // Switching the statistic redraws what is already loaded: both
  // statistics arrive in the same answer.
  // Each entry is also a link to the same span with its statistic, which
  // is what it does without this script.
  statItems.forEach(function (item) {
    item.addEventListener("click", function (event) {
      event.preventDefault();
      if (statMenu) {
        statMenu.open = false;
      }
      var chosen = item.getAttribute("data-chart-stat") === "max" ? "max" : "mean";
      if (chosen === stat) {
        return;
      }
      stat = chosen;
      statItems.forEach(function (other) {
        var selected = other === item;
        other.setAttribute("aria-current", selected ? "true" : "false");
        var check = other.querySelector("[data-stat-check]");
        if (check) {
          check.classList.toggle("invisible", !selected);
        }
      });
      var name = item.querySelector("[data-stat-name]");
      if (statLabel && name) {
        statLabel.textContent = name.textContent;
      }
      draw();
      // Keep the address bar in step, so the view can be shared and a
      // change of span keeps the statistic.
      if (window.history && window.history.replaceState) {
        var url = new URL(window.location.href);
        url.searchParams.set("stat", stat);
        window.history.replaceState(null, "", url.toString());
      }
      Array.prototype.forEach.call(document.querySelectorAll('a[href*="range="]:not([data-chart-stat])'), function (link) {
        var href = new URL(link.href, window.location.href);
        href.searchParams.set("stat", stat);
        link.href = href.toString();
      });
    });
  });

  charts.forEach(function (chart) {
    chart.plot.addEventListener("mouseenter", function () {
      active = chart;
    });
    chart.plot.addEventListener("mouseleave", function () {
      if (active === chart) {
        active = null;
      }
    });
  });

  if (refresh) {
    refresh.addEventListener("click", load);
  }

  var resizing = null;
  window.addEventListener("resize", function () {
    window.clearTimeout(resizing);
    resizing = window.setTimeout(function () {
      charts.forEach(function (chart) {
        if (chart.u) {
          chart.u.setSize({ width: Math.max(chart.plot.clientWidth, 120), height: PLOT_HEIGHT });
        }
      });
    }, 150);
  });

  load();
})();
