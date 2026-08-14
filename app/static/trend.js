/* The issue-rating trend popup. Fetches /api/issue-trend and draws the
   declared bands plus one point per rated day into the shared modal shell
   that app.js exposes as window.AppModal. Loaded only by pages that render
   a [data-trend] button; everything here is lazy, so load order against
   app.js does not matter. */
(function () {
  "use strict";

  function esc(s) { return window.AppModal.esc(s); }

  function shortDate(iso) {
    return new Date(iso + "T12:00:00").toLocaleDateString(undefined, { day: "numeric", month: "short" });
  }

  function bandOf(tr, v) {
    for (var i = 0; i < tr.bands.length; i++) {
      if (tr.bands[i].upto == null || v <= tr.bands[i].upto) return tr.bands[i];
    }
    return null;
  }

  function openTrend(key) {
    if (!window.AppModal) return;
    var m = window.AppModal.show("Loading…");
    fetch("/api/issue-trend?key=" + encodeURIComponent(key))
      .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); })
      .then(function (tr) { renderTrend(m, tr); })
      .catch(function (e) {
        m.title.textContent = "Unavailable";
        m.body.innerHTML = "<p>Could not load the trend: " + esc(e.message) + "</p>";
      });
  }

  function renderTrend(m, tr) {
    m.title.textContent = tr.name;
    var n = tr.points.length;
    if (!n) {
      m.body.innerHTML = "<p>No readings yet — rate it once and the trend starts here.</p>";
      return;
    }
    /* viewBox units ≈ CSS pixels at phone width, so the 10px labels stay
       legible on the smallest screen and only grow on desktop. */
    var W = 360, H = 200, L = 24, R = 22, T = 10, B = 24;
    var pw = W - L - R, ph = H - T - B;
    var lo = tr.min - 0.5, hi = tr.max + 0.5;
    /* A rating outside today's declared scale (an old log line) plots
       clamped to the edge; the tooltip and table still state the raw value. */
    var cv = function (v) { return Math.min(Math.max(v, tr.min), tr.max); };
    var y = function (v) { return T + ph * (1 - (cv(v) - lo) / (hi - lo)); };
    var t0 = Date.parse(tr.points[0].date), t1 = Date.parse(tr.points[n - 1].date);
    var span = Math.max(t1 - t0, 864e5);
    var x = n === 1
      ? function () { return L + pw / 2; }
      : function (d) { return L + pw * ((Date.parse(d) - t0) / span); };

    var svg = [];
    /* The zones come from the declared bands — the same declaration the
       rating buttons use — filled with the tone and labelled in text ink. */
    var from = tr.min;
    tr.bands.forEach(function (b) {
      var top = (b.upto == null ? tr.max : b.upto) + 0.5;
      var yT = y(top), yB = y(from - 0.5);
      svg.push('<rect x="' + L + '" y="' + yT + '" width="' + pw + '" height="' + (yB - yT) + '" fill="var(--' + b.tone + ')" opacity="0.13"/>');
      if (yB - yT >= 15) {
        svg.push('<text x="' + (L + pw - 6) + '" y="' + (yT + 12) + '" text-anchor="end" font-size="10" fill="var(--ink-2)">' + esc(b.label.toUpperCase()) + "</text>");
      }
      from = (b.upto == null ? tr.max : b.upto) + 1;
    });
    var step = tr.max - tr.min > 10 ? 2 : 1;
    for (var v = tr.min; v <= tr.max; v += step) {
      svg.push('<text x="' + (L - 8) + '" y="' + (y(v) + 3.5) + '" text-anchor="end" font-size="10" fill="var(--ink-2)">' + v + "</text>");
    }
    var want = Math.min(5, n);
    for (var i = 0; i < want; i++) {
      var p = tr.points[Math.round(i * (n - 1) / Math.max(want - 1, 1))];
      svg.push('<text x="' + x(p.date) + '" y="' + (H - 8) + '" text-anchor="middle" font-size="10" fill="var(--ink-2)">' + shortDate(p.date) + "</text>");
    }
    if (n > 1) {
      var pts = tr.points.map(function (p) { return x(p.date) + "," + y(p.val); }).join(" ");
      svg.push('<polyline points="' + pts + '" fill="none" stroke="var(--ink-2)" stroke-width="2" stroke-linejoin="round"/>');
    }
    tr.points.forEach(function (p) {
      svg.push('<circle cx="' + x(p.date) + '" cy="' + y(p.val) + '" r="4" fill="var(--ink)" stroke="var(--surface)" stroke-width="2"/>');
    });
    svg.push('<line id="trend-cross" y1="' + T + '" y2="' + (T + ph) + '" x1="0" x2="0" stroke="var(--rule)" stroke-width="1" visibility="hidden"/>');
    svg.push('<rect id="trend-hit" x="' + L + '" y="' + T + '" width="' + pw + '" height="' + ph + '" fill="transparent"/>');

    var range = tr.min + (tr.low ? " " + tr.low : "") + " – " + tr.max + (tr.high ? " " + tr.high : "");
    m.body.innerHTML =
      '<p class="hint">' + esc(tr.ask || "Daily rating") + " · " + esc(range) + "</p>" +
      '<div class="trendwrap"><svg class="trend-svg" viewBox="0 0 ' + W + " " + H + '" width="100%" role="img" aria-label="' + esc(tr.name) + ' daily rating trend"></svg>' +
      '<div class="trendtip" hidden></div></div>' +
      '<p class="hint">' + n + " reading" + (n === 1 ? "" : "s") + ", " + shortDate(tr.points[0].date) + " – " + shortDate(tr.points[n - 1].date) + "</p>" +
      '<details class="trendtable"><summary>Every reading</summary><table>' +
      tr.points.map(function (p) {
        var b = bandOf(tr, p.val);
        return "<tr><td>" + esc(p.date) + "</td><td><strong>" + p.val + "</strong></td><td>" +
          (b ? esc(b.label) : "") + "</td><td>" + esc(p.note || "") + "</td></tr>";
      }).join("") + "</table></details>";
    var svgEl = m.body.querySelector("svg");
    svgEl.innerHTML = svg.join("");

    var tip = m.body.querySelector(".trendtip");
    var cross = svgEl.querySelector("#trend-cross");
    var hit = svgEl.querySelector("#trend-hit");
    function showNearest(clientX) {
      var rect = svgEl.getBoundingClientRect();
      var vx = (clientX - rect.left) / rect.width * W;
      var best = tr.points[0], bd = Infinity;
      tr.points.forEach(function (p) {
        var d = Math.abs(x(p.date) - vx);
        if (d < bd) { bd = d; best = p; }
      });
      var b = bandOf(tr, best.val);
      tip.innerHTML = "<strong>" + shortDate(best.date) + " · " + best.val + "</strong>" +
        (b ? " — " + esc(b.label) : "") + (best.note ? "<br>" + esc(best.note) : "");
      tip.hidden = false;
      var px = x(best.date) / W * rect.width, py = y(best.val) / H * rect.height;
      tip.style.left = Math.max(0, Math.min(px + 10, rect.width - tip.offsetWidth - 4)) + "px";
      tip.style.top = Math.max(py - tip.offsetHeight - 10, 0) + "px";
      cross.setAttribute("x1", x(best.date));
      cross.setAttribute("x2", x(best.date));
      cross.setAttribute("visibility", "visible");
    }
    hit.addEventListener("mousemove", function (e) { showNearest(e.clientX); });
    hit.addEventListener("click", function (e) { showNearest(e.clientX); });
    hit.addEventListener("mouseleave", function () { tip.hidden = true; cross.setAttribute("visibility", "hidden"); });
  }

  document.addEventListener("click", function (e) {
    var tb = e.target.closest("[data-trend]");
    if (!tb) return;
    e.preventDefault();
    openTrend(tb.dataset.trend);
  });

  document.addEventListener("DOMContentLoaded", function () {
    if (location.hash.indexOf("#trend-") === 0) openTrend(location.hash.slice(7));
  });
})();
