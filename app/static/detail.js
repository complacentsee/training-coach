/* The recorded activity, in the shared modal. Reads /api/activity-detail
   for the shape of the recording and /api/activity-metrics for the measured
   numbers — the same two payloads a grade is written from, so the page and
   the grade cannot quote different figures. Loaded only by pages that render
   a [data-detail] trigger; everything here is lazy, so load order against
   app.js does not matter. */
(function () {
  "use strict";

  function esc(s) { return window.AppModal.esc(s); }
  function emph(s) { return window.AppModal.emph(s); }

  function getJSON(url) {
    return fetch(url).then(function (r) {
      if (!r.ok) {
        return r.text().then(function (t) {
          var e = new Error((t || "HTTP " + r.status).trim());
          e.status = r.status;
          throw e;
        });
      }
      return r.json();
    });
  }

  function longDate(iso) {
    return new Date(iso + "T12:00:00").toLocaleDateString(undefined,
      { weekday: "short", day: "numeric", month: "short" });
  }

  function clockOf(utc) {
    var d = new Date(utc);
    if (isNaN(d)) return "";
    return d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
  }

  /* mm:ss for a lap, h:mm:ss once an hour is involved — a mile split and a
     three-hour ride read in the units each is actually spoken in. */
  function dur(secs) {
    var s = Math.round(secs || 0);
    var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), r = s % 60;
    var two = function (n) { return (n < 10 ? "0" : "") + n; };
    return h ? h + ":" + two(m) + ":" + two(r) : m + ":" + two(r);
  }

  /* The recording that matches the day's session, when the day carries more
     than one — 121 archive dates do. Sport is the test the grader uses, so
     the page picks the same file the grade was written about. */
  function choose(list, sport) {
    for (var i = 0; i < list.length; i++) {
      if (sport && list[i].sport === sport) return list[i];
    }
    return list[0];
  }

  function paceLabel(basis) {
    if (basis === "elapsed") return "elapsed";
    if (basis === "moving") return "moving";
    if (basis === "running") return "running only";
    return basis;
  }

  function open(btn) {
    if (!window.AppModal) return;
    var st = {
      date: btn.getAttribute("data-detail") || "",
      sport: btn.getAttribute("data-sport") || "",
      block: btn.getAttribute("data-block") || "",
      grade: btn.getAttribute("data-grade") || "",
      note: btn.getAttribute("data-note") || "",
    };
    var m = window.AppModal.show(longDate(st.date));
    m.body.innerHTML = '<p class="hint">Reading the recording…</p>';
    getJSON("/api/activities?date=" + encodeURIComponent(st.date))
      .then(function (list) {
        st.list = list || [];
        if (!st.list.length) {
          m.body.innerHTML = "<p>Nothing was recorded on this day.</p>";
          return;
        }
        show(m, st, choose(st.list, st.sport).name);
      })
      .catch(function (e) { failed(m, e); });
  }

  function failed(m, e) {
    m.body.innerHTML = '<p class="hint">Could not read it: ' + esc(e.message) + "</p>";
  }

  function show(m, st, name) {
    st.name = name;
    m.body.innerHTML = '<p class="hint">Reading the recording…</p>';
    /* The measured numbers are a separate payload and a separate decode; a
       file whose import failed has none, and the shape is still worth
       reading, so its absence costs those rows and nothing else. */
    Promise.all([
      getJSON("/api/activity-detail?name=" + encodeURIComponent(name) +
        (st.block ? "&block=" + encodeURIComponent(st.block) : "")),
      getJSON("/api/activity-metrics?name=" + encodeURIComponent(name)).catch(function () { return null; }),
    ]).then(function (r) {
      draw(m, st, r[0], r[1]);
    }).catch(function (e) {
      /* An undecodable file still has a selector and a name: say what is
         wrong with THIS recording rather than emptying the popover. */
      m.body.innerHTML = picker(st) +
        "<p>This recording could not be read.</p>" +
        '<p class="hint">' + esc(e.message) + "</p>";
      bindPicker(m, st);
    });
  }

  function picker(st) {
    if (st.list.length < 2) return "";
    return '<div class="dpick">' + st.list.map(function (a) {
      var lab = (a.sport || "activity") + (a.size ? "" : "");
      return '<button type="button" class="dpill' + (a.name === st.name ? " on" : "") +
        '" data-pick="' + esc(a.name) + '">' + esc(lab) + "</button>";
    }).join("") + "</div>";
  }

  function bindPicker(m, st) {
    var pills = m.body.querySelectorAll("[data-pick]");
    for (var i = 0; i < pills.length; i++) {
      pills[i].addEventListener("click", function (e) {
        show(m, st, e.currentTarget.getAttribute("data-pick"));
      });
    }
  }

  function stat(label, value) {
    if (!value && value !== 0) return "";
    return '<div class="dstat"><span class="dlab">' + esc(label) + "</span>" +
      '<span class="dval">' + esc(String(value)) + "</span></div>";
  }

  function draw(m, st, d, mt) {
    var run = d.sport === "running";
    var h = picker(st);

    var chips = [];
    if (d.sport) chips.push(esc(d.sport));
    if (d.indoor) chips.push("indoor");
    var when = clockOf(d.start_utc);
    if (when) chips.push(esc(when));
    h += '<p class="dchips">' + chips.join(" · ") + "</p>";

    /* What the day asked for, above what the recording did. This is the join
       a general activity site cannot make: the block knows what step 1 was. */
    if (d.session && d.session.label) {
      var line = '<p class="dsess"><b>' + emph(d.session.label) + "</b>";
      if (d.session.reps_asked) {
        var done = d.session.reps_done || 0;
        line += ' <span class="' + (done >= d.session.reps_asked ? "v-in" : "v-under") + '">' +
          done + " of " + d.session.reps_asked + " reps</span>";
        if (d.session.reps_what) line += ' <span class="dwhat">' + esc(d.session.reps_what) + "</span>";
      }
      h += line + "</p>";
    }

    /* What it was. Distance and both clocks always; the rest only where the
       file and the register have something to say. */
    var cells = stat("Distance", d.dist) +
      stat("Moving", d.moving_hms) +
      stat("Elapsed", d.elapsed_hms) +
      stat("Climb", d.ascent);
    if (mt && mt.hr) {
      cells += stat("Avg HR", mt.hr.avg ? Math.round(mt.hr.avg) + " bpm" : "");
      cells += stat("Max HR", mt.hr.max ? mt.hr.max + " bpm" : "");
    }
    if (mt && mt.power && !run) {
      cells += stat("Avg power", Math.round(mt.power.avg) + " W");
      if (mt.power.best_60s) cells += stat("Best 60s", Math.round(mt.power.best_60s) + " W");
    }
    if (mt && mt.cadence) cells += stat("Cadence", Math.round(mt.cadence) + (run ? " spm" : " rpm"));
    h += '<div class="dstats">' + cells + "</div>";

    /* Three paces, each labelled, each from the file: elapsed and moving are
       the recording's own two clocks and running-only is the device's own
       split summary with the walk breaks taken out. */
    if (d.paces && d.paces.length) {
      h += '<ul class="dpaces">' + d.paces.map(function (p) {
        return "<li><b>" + esc(p.pace) + "</b> <span>" + esc(paceLabel(p.basis)) + "</span></li>";
      }).join("") + "</ul>";
    }

    /* Splits before the verdict: the numbers are what the popover is for,
       and a grade note runs to a paragraph that would push them off a
       phone screen. */
    h += splits(d, run);
    h += chartSVG(d, mt, run);

    if (st.grade) {
      h += '<div class="dgrade"><p class="g-band"><b class="g g' + esc(st.grade.toLowerCase()) +
        '">' + esc(st.grade) + "</b> <span>" +
        (st.grade.toUpperCase() === "DNF" ? "Not finished" : "Graded") + "</span></p>" +
        (st.note ? '<p class="gnote">' + emph(st.note) + "</p>" : "") + "</div>";
    }

    /* What the athlete said about the day, where anything was said. Nothing
       is rendered when nothing was — no heading, no empty box: a placeholder
       claims a note exists. */
    if (d.notes && d.notes.length) {
      h += '<div class="dnotes">' + d.notes.map(function (n) {
        return "<p>" + emph(n.note) + "</p>";
      }).join("") + "</div>";
    }

    h += '<p class="dsrc">' + esc(d.name) + "</p>";

    m.body.innerHTML = h;
    bindPicker(m, st);
  }

  /* Uniform means the auto-lap did the lapping: every full lap the same
     length, with at most a remainder at the end. */
  function uniformLaps(laps) {
    var full = laps.filter(function (l) { return l.trigger === "distance" && l.dist_m > 0; });
    if (full.length < 2) return false;
    var first = full[0].dist_m;
    return full.every(function (l) { return Math.abs(l.dist_m - first) / first < 0.01; });
  }

  /* The chart, in the trend.js idiom: an SVG built by string concatenation
     with CSS custom properties for every colour, so it is theme-aware for
     free and costs no library. Pace and heart rate on a run, watts and heart
     rate on a ride — the two currencies each kind of session is actually
     judged in.

     No linked hover: Dreeve gets lap-to-chart linkage cheaply because
     ECharts emits an index against a parallel array, and here it would be
     hand-rolled maths against hand-rolled SVG plus a tap gesture designed
     for a phone. Cut deliberately, per the plan.

     Colours are existing tokens (--accent for the effort, --hard for the
     heart), not a new ramp: the palette validator this repo used was retired
     with the documents, and inventing a pair without it is exactly what that
     rule forbids. */
  function pct(sorted, p) {
    if (!sorted.length) return 0;
    var i = Math.min(sorted.length - 1, Math.max(0, Math.round((sorted.length - 1) * p)));
    return sorted[i];
  }

  function rangeOf(vals) {
    var ok = vals.filter(function (v) { return v != null; }).sort(function (a, b) { return a - b; });
    if (ok.length < 2) return null;
    var lo = pct(ok, 0.05), hi = pct(ok, 0.95);
    if (hi <= lo) { lo = ok[0]; hi = ok[ok.length - 1]; }
    if (hi <= lo) return null;
    var pad = (hi - lo) * 0.08;
    return { lo: lo - pad, hi: hi + pad };
  }

  /* One polyline per unbroken run of samples: a null is where the recording
     stopped or the athlete did, and a line drawn through it would invent a
     value nobody recorded. */
  function segments(secs, vals, x, y) {
    var out = [], cur = [];
    for (var i = 0; i < vals.length; i++) {
      if (vals[i] == null) {
        if (cur.length > 1) out.push(cur.join(" "));
        cur = [];
        continue;
      }
      cur.push(x(secs[i]).toFixed(1) + "," + y(vals[i]).toFixed(1));
    }
    if (cur.length > 1) out.push(cur.join(" "));
    return out;
  }

  function mmss(secs) {
    var s = Math.round(secs);
    return Math.floor(s / 60) + ":" + (s % 60 < 10 ? "0" : "") + (s % 60);
  }

  function chartSVG(d, mt, run) {
    var c = d.chart;
    if (!c || !c.secs || c.secs.length < 2) return "";
    var prim = run ? c.pace : c.watts;
    var hr = c.hr;
    var pr = prim && prim.length ? rangeOf(prim) : null;
    var hrr = hr && hr.length ? rangeOf(hr) : null;
    if (!pr && !hrr) return "";

    /* Two panels stacked on one clock rather than two lines in one box. On
       a run they crossed constantly and read as a tangle — and the crossings
       are not noise, they are his walk breaks, which is exactly the thing
       worth being able to see. */
    var W = 360, L = 36, R = 26, T = 8, GAP = 14, BOT = 16;
    var pw = W - L - R;
    var panels = [];
    if (pr) panels.push({ vals: prim, range: pr, colour: "var(--accent)", invert: run,
                          label: run ? "pace" + (c.unit || "") : "watts", fmt: run ? mmss : Math.round });
    if (hrr) panels.push({ vals: hr, range: hrr, colour: "var(--hard)", invert: false,
                           label: "heart rate", fmt: Math.round, hr: true });
    var ph = panels.length > 1 ? 72 : 110;
    var H = T + panels.length * ph + (panels.length - 1) * GAP + BOT;

    var span = c.secs[c.secs.length - 1] || 1;
    var x = function (s) { return L + pw * (s / span); };

    /* The cap the day is judged against, drawn where the heart rate is read
       and taken from the metrics payload, which resolves it against the
       anchors as they stand now — the same number the grade quotes. */
    var cap = null;
    if (mt && mt.grade_input) cap = mt.grade_input.grade_cap_bpm || mt.grade_input.hr_cap_bpm || null;
    if (cap && hrr) { hrr.lo = Math.min(hrr.lo, cap - 2); hrr.hi = Math.max(hrr.hi, cap + 2); }

    var g = [];
    panels.forEach(function (p, i) {
      var top = T + i * (ph + GAP);
      var y = function (v) {
        var f = (v - p.range.lo) / (p.range.hi - p.range.lo);
        return top + ph * (p.invert ? f : 1 - f);
      };
      if (p.hr && cap) {
        /* Time above the cap is what costs the grade, so that is the region
           the eye should find first. */
        var yc = y(cap);
        g.push('<rect x="' + L + '" y="' + top + '" width="' + pw + '" height="' + Math.max(0, yc - top).toFixed(1) +
          '" fill="var(--hard)" opacity="0.10"/>');
        g.push('<line x1="' + L + '" x2="' + (L + pw) + '" y1="' + yc.toFixed(1) + '" y2="' + yc.toFixed(1) +
          '" stroke="var(--hard)" stroke-width="1" stroke-dasharray="3 3" opacity="0.75"/>');
        g.push('<text x="' + (L + 3) + '" y="' + (yc - 3).toFixed(1) + '" font-size="9" fill="var(--ink-3)">cap ' + cap + "</text>");
      }
      segments(c.secs, p.vals, x, y).forEach(function (pts) {
        g.push('<polyline points="' + pts + '" fill="none" stroke="' + p.colour +
          '" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/>');
      });
      [p.range.hi, p.range.lo].forEach(function (v) {
        g.push('<text x="' + (L - 4) + '" y="' + (y(v) + (v === p.range.hi ? 7 : 0)).toFixed(1) +
          '" text-anchor="end" font-size="9" fill="var(--ink-3)">' + p.fmt(v) + "</text>");
      });
      g.push('<text x="' + (L + pw) + '" y="' + (top - 1) + '" text-anchor="end" font-size="8.5" letter-spacing="0.08em" fill="' + p.colour +
        '" opacity="0.85">' + esc(p.label.toUpperCase()) + "</text>");
    });

    // The clock, in the minutes the prescription is written in.
    var step = span > 3600 ? 900 : span > 1800 ? 600 : 300;
    for (var s2 = 0; s2 <= span; s2 += step) {
      if (x(s2) > L + pw - 34) break; // the MINUTES label owns that corner
      g.push('<text x="' + x(s2).toFixed(1) + '" y="' + (H - 4) + '" text-anchor="middle" font-size="9" fill="var(--ink-3)">' +
        Math.round(s2 / 60) + "</text>");
    }
    g.push('<text x="' + (L + pw) + '" y="' + (H - 4) + '" text-anchor="end" font-size="8.5" fill="var(--ink-3)">MINUTES</text>');

    return '<div class="dchart"><svg viewBox="0 0 ' + W + " " + H + '" width="100%" role="img" aria-label="' +
      esc(panels.map(function (p) { return p.label; }).join(" and ") + " over the session") + '">' +
      g.join("") + "</svg></div>";
  }

  function splits(d, run) {
    var laps = d.laps || [];
    /* This is NOT the empty-notes case. Nothing said is about the athlete,
       and a heading over it claims input that does not exist; no laps is a
       fact about the recording, and saying so stops a reader hunting for
       splits the file never carried. */
    /* One session_end lap is the file saying it had no laps at all — four of
       twelve sampled recordings look like that, and the row would only
       repeat the summary above it. */
    if (!laps.length || (laps.length === 1 && laps[0].trigger === "session_end")) {
      return '<p class="hint">This recording carries no laps.</p>';
    }

    /* Columns follow the data: a column nothing fills is a column nobody
       reads, and at 390px there is room for five. */
    var anyClimb = false, anyPower = false, anyHR = false, mixed = false;
    laps.forEach(function (l) {
      if (l.ascent_m) anyClimb = true;
      if (l.avg_power) anyPower = true;
      if (l.avg_hr) anyHR = true;
      if (l.trigger && l.trigger !== "distance" && l.trigger !== "session_end") mixed = true;
    });
    /* On a run auto-lapped every mile, the pace column repeats the time
       column digit for digit — 1 mi in 10:07 is 10:07/mi. It earns its
       width only where the laps are not all the same length: reps, steps,
       a lap pressed by hand. */
    var uniform = uniformLaps(laps);
    var stepped = laps.some(function (l) { return l.prescribed; });
    var cols = [stepped ? "Step" : "#", "Dist", "Time"];
    if (run && !uniform) cols.push("Pace");
    if (!run && anyPower) cols.push("Power");
    if (anyHR) cols.push("HR");
    if (run && anyClimb) cols.push("Asc");

    var rows = laps.map(function (l) {
      var p = l.prescribed;
      var first = stepped
        ? (p ? esc(p.label) : "—")
        : l.n + (mixed && l.trigger !== "distance" ? '<i>' + esc(l.trigger.replace("session_end", "end")) + "</i>" : "");
      var td = ["<td>" + first + "</td>"];
      /* The verdict marks the metric the step actually targeted, and nothing
         else: watts where ERG held the watts, pace where a pace was asked. */
      var v = p && p.verdict ? ' class="v-' + esc(p.verdict) + '"' : "";
      var tip = p && p.target ? ' title="' + esc(p.dur + " @ " + p.target) + '"' : "";
      td.push("<td>" + esc(l.dist || "—") + "</td>");
      /* Where the timer and the clock disagree, both are shown: that gap is
         a stop inside the lap, and on 12 Aug it is a 296 s stop inside a
         three-minute interval. */
      var t = dur(l.timer_s);
      if (l.elapsed_s - l.timer_s > 2) t += " <i>" + dur(l.elapsed_s) + "</i>";
      td.push("<td>" + t + "</td>");
      if (run && !uniform) td.push("<td" + v + tip + ">" + esc(l.pace || "—") + "</td>");
      if (!run && anyPower) td.push("<td" + v + tip + ">" + (l.avg_power ? l.avg_power + " W" : "—") + "</td>");
      if (anyHR) td.push("<td>" + (l.avg_hr || "—") + "</td>");
      if (run && anyClimb) td.push("<td>" + (l.ascent_m ? esc(l.ascent || l.ascent_m + " m") : "—") + "</td>");
      return "<tr>" + td.join("") + "</tr>";
    }).join("");

    return '<div class="dtabwrap"><table class="dtab"><thead><tr>' +
      cols.map(function (c) { return "<th>" + esc(c) + "</th>"; }).join("") +
      "</tr></thead><tbody>" + rows + "</tbody></table></div>";
  }

  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-detail]");
    if (!b) return;
    e.preventDefault();
    open(b);
  });

  /* Deep link: #activity-2026-08-15 opens that day's recording. */
  document.addEventListener("DOMContentLoaded", function () {
    var h = location.hash;
    if (h.indexOf("#activity-") !== 0) return;
    var date = h.slice(10);
    var btn = document.querySelector('[data-detail="' + date + '"]');
    if (btn) open(btn);
  });
})();
