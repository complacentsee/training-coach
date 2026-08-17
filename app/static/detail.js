/* The recorded activity, in the shared modal. Reads /api/activity-detail
   for the shape of the recording and /api/activity-metrics for the measured
   numbers — the same two payloads a grade is written from, so the page and
   the grade cannot quote different figures. Loaded only by pages that render
   a [data-detail] trigger; everything here is lazy, so load order against
   app.js does not matter. */
(function () {
  "use strict";

  /* Leaflet is vendored and lazily loaded — a popover for an indoor ride
     never pays for it — and its URLs come from the script tag that loaded
     this file, because assets are content-hashed and only the template knows
     the hash. */
  var here = document.currentScript;
  var vendor = { js: here && here.dataset.leaflet, css: here && here.dataset.leafletCss };
  var leafletLoading = null;

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
      trigger: btn,
      draws: 0, // guards a slow map against a faster pill switch
      date: btn.getAttribute("data-detail") || "",
      sport: btn.getAttribute("data-sport") || "",
      block: btn.getAttribute("data-block") || "",
      grade: btn.getAttribute("data-grade") || "",
      note: btn.getAttribute("data-note") || "",
    };
    var m = window.AppModal.show("Activity overview");
    m.body.innerHTML = '<p class="hint">Reading the recording…</p>';
    getJSON("/api/activities?date=" + encodeURIComponent(st.date))
      .then(function (list) {
        list = list || [];
        /* The day's recordings, filtered the way the grader filters them: a
           walk on a bike day is not part of the ride, and its pill was noise
           beside the session's own files. The session's sport comes from the
           cell; a day that prescribes none, or whose recordings all mismatch
           (an unimported file has no sport to match), shows everything —
           hiding the only thing recorded would read as "nothing recorded". */
        var match = st.sport ? list.filter(function (a) { return a.sport === st.sport; }) : [];
        st.list = match.length ? match : list;
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
    /* Switching recordings must not rebuild the popover. The day around
       them does not change — same prescription, same grade, same notes, and
       possibly a re-grade running in it — so only the recording's own
       region is replaced. Blanking the body instead cost a flash, the
       scroll position, a fresh Leaflet instance every switch, and would
       have thrown away an in-flight "Grading…" along with the poll's handle
       on it. */
    var region = m.body.querySelector(".drec");
    if (region) {
      region.classList.add("wait");
    } else {
      m.body.innerHTML = '<p class="hint">Reading the recording…</p>';
    }
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
      var failed = '<p>This recording could not be read.</p>' +
        '<p class="hint">' + esc(e.message) + "</p>";
      var region = m.body.querySelector(".drec");
      if (region) {
        region.classList.remove("wait");
        region.innerHTML = failed;
        markPill(m, st);
        return;
      }
      m.body.innerHTML = picker(st) + failed;
      bindPicker(m, st);
    });
  }

  /* One button per recording the day holds. The label has to say WHICH: a
     split session is three files of the same sport, so the sport names
     nothing and the start time and the size name everything. Device names
     are local time, so the clock comes off the name itself. */
  function picker(st) {
    if (st.list.length < 2) return "";
    var chronological = st.list.slice().sort(function (a, b) {
      return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
    });
    return '<div class="dpick">' + chronological.map(function (a) {
      var bits = [];
      var t = clockOfName(a.name);
      if (t) bits.push(t);
      if (a.dist) bits.push(a.dist);
      else if (a.elapsed_hms) bits.push(a.elapsed_hms);
      if (!bits.length) bits.push(a.sport || "activity");
      return '<button type="button" class="dpill' + (a.name === st.name ? " on" : "") +
        '" data-pick="' + esc(a.name) + '">' + esc(bits.join(" · ")) + "</button>";
    }).join("") + "</div>";
  }

  /* A device filename is the athlete's local time: 2026-09-26-08-12-04.fit
     started at 8:12 am. No timezone maths, and correct for a session
     recorded away from home, which a UTC conversion would not be. */
  function clockOfName(name) {
    var m = /^\d{4}-\d{2}-\d{2}-(\d{2})-(\d{2})-\d{2}/.exec(name || "");
    if (!m) return "";
    var h = +m[1], suffix = h < 12 ? "a" : "p";
    return ((h % 12) || 12) + ":" + m[2] + suffix;
  }

  /* The re-grade control. A first click opens the form rather than firing:
     a grade costs money and supersedes a verdict, and the pause is also
     where the athlete gets to say what the numbers cannot — "the chain came
     off" is the difference between a DNF and an F, and only he knows it. */
  function bindRegrade(m, st) {
    var wrap = m.body.querySelector(".dact");
    if (!wrap) return;
    var btn = wrap.querySelector(".dregrade");
    var form = wrap.querySelector(".dsay");
    var box = wrap.querySelector("textarea");
    var go = wrap.querySelector(".dgo");
    var cancel = wrap.querySelector(".dcancel");

    btn.addEventListener("click", function () {
      btn.hidden = true;
      form.hidden = false;
      box.focus();
    });
    cancel.addEventListener("click", function () {
      form.hidden = true;
      btn.hidden = false;
    });

    go.addEventListener("click", function () {
      go.disabled = cancel.disabled = box.disabled = true;
      go.textContent = "Starting…";
      var before = st.gradedAt || "";
      fetch("/api/regrade?date=" + encodeURIComponent(st.date), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ note: box.value }),
      })
        .then(function (r) {
          if (r.ok) return;
          return r.text().then(function (t) { throw new Error(t.trim() || r.status); });
        })
        .then(function () {
          wrap.className = "dact dwait";
          wrap.innerHTML = '<p class="hint">Grading…</p>';
          awaitGrade(m, st, before, wrap);
        })
        .catch(function (e) {
          go.disabled = cancel.disabled = box.disabled = false;
          go.textContent = "Grade it again";
          var p = wrap.querySelector(".dfail") || document.createElement("p");
          p.className = "dfail";
          p.textContent = e.message;
          wrap.appendChild(p);
        });
    });
  }

  /* A grade takes seconds, so the popover waits for it rather than asking
     for a reload. It polls the detail payload because that is where the
     day's CURRENT verdict lives; graded_at is what makes the change
     detectable, since a re-grade may land on the same letter. */
  function awaitGrade(m, st, before, wrap) {
    var tries = 0;
    (function poll() {
      if (++tries > 60) {                       // two minutes, then say so
        wrap.innerHTML = '<p class="hint">Still grading. It will be here when you next open this.</p>';
        return;
      }
      setTimeout(function () {
        getJSON("/api/activity-detail?name=" + encodeURIComponent(st.name) +
          (st.block ? "&block=" + encodeURIComponent(st.block) : ""))
          .then(function (d) {
            if (!d.graded_at || d.graded_at === before) { poll(); return; }
            st.grade = d.grade || "";
            st.note = d.grade_note || "";
            st.gradedAt = d.graded_at;
            showGrade(m, st);
            publishGrade(st);
            wrap.innerHTML = "";
            wrap.className = "dact";
          })
          .catch(function () { poll(); });
      }, 2000);
    })();
  }

  /* Replace the verdict in place, inserting the block when the day had no
     grade at all before. */
  function showGrade(m, st) {
    var html = '<p class="g-band"><b class="g g' + esc(st.grade.toLowerCase()) + '">' +
      esc(st.grade) + "</b> <span>" +
      (st.grade.toUpperCase() === "DNF" ? "Not finished" : "Graded") + "</span></p>" +
      (st.note ? '<p class="gnote">' + emph(st.note) + "</p>" : "");
    var block = m.body.querySelector(".dgrade");
    if (!block) {
      block = document.createElement("div");
      block.className = "dgrade";
      var act = m.body.querySelector(".dact");
      act.parentNode.insertBefore(block, act);
    }
    block.innerHTML = html;
  }

  /* The page behind the popover was rendered before this grade existed.
     Update the cell it was opened from so closing the popover does not
     reveal the old verdict — the same markup calendar.html writes. */
  function publishGrade(st) {
    var btn = st.trigger;
    if (!btn) return;
    btn.setAttribute("data-grade", st.grade);
    btn.setAttribute("data-note", st.note);
    var cell = btn.closest ? btn.closest("td") : null;
    if (!cell) return;
    cell.classList.add("graded");
    if (st.note) cell.setAttribute("title", st.note);
    var chip = cell.querySelector("button.g");
    if (!chip) {
      chip = document.createElement("button");
      chip.type = "button";
      chip.setAttribute("data-when", longDate(st.date));
      cell.insertBefore(chip, cell.firstChild);
    }
    chip.className = "g g" + st.grade.toLowerCase();
    chip.textContent = st.grade;
    chip.setAttribute("data-grade", st.grade);
    chip.setAttribute("data-note", st.note);
    chip.setAttribute("aria-label", st.grade.toUpperCase() === "DNF"
      ? "Not finished — read why" : "Graded " + st.grade + " — read why");
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
    /* h accumulates the RECORDING's half; day accumulates the day's. What
       belongs to which is the question of what a pill switch may replace. */
    var day = "";
    /* The payload's grade is CURRENT; st.grade came from a calendar cell
       rendered whenever the page was loaded. A re-grade lands seconds later
       and must be visible without one. */
    if (d.graded_at || d.grade) {
      st.grade = d.grade || "";
      st.note = d.grade_note || "";
      st.gradedAt = d.graded_at || "";
    }
    var h = "";

    var chips = [];
    if (st.date) chips.push(esc(longDate(st.date)));
    if (d.sport) chips.push(esc(d.sport));
    if (d.indoor) chips.push("indoor");
    var when = clockOf(d.start_utc);
    if (when) chips.push(esc(when));
    h += '<p class="dchips">' + chips.join(" · ") + "</p>";

    /* Where the streams came from — the file's own connected-device list.
       One line, so a trainer ride and a power-meter ride stop reading
       identically the day both exist. */
    if (d.sources && (d.sources.hr || d.sources.power)) {
      var src = [];
      if (d.sources.hr) src.push("HR " + esc(d.sources.hr));
      if (d.sources.power) src.push("power " + esc(d.sources.power));
      h += '<p class="dsensors">' + src.join(" · ") + "</p>";
    }

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

    /* The route, first in the stack. An indoor session has none by
       construction — the gate is server-side — so this is simply absent
       there rather than an empty box. */
    var mapped = d.track && d.track.segments && d.track.segments.length;
    if (mapped) {
      h += '<div class="dmap"></div>';
    } else if (d.indoor) {
      /* A trainer session records a position and it is not a place — Zwift
         writes its own world's — so the route is gated server-side. Saying
         so is the "no laps" case: a fact about the recording, not a
         placeholder for something the athlete never wrote. */
      h += '<p class="hint">Indoors — no route.</p>';
    } else {
      h += '<p class="hint">This recording carries no positions.</p>';
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
      day += '<div class="dgrade"><p class="g-band"><b class="g g' + esc(st.grade.toLowerCase()) +
        '">' + esc(st.grade) + "</b> <span>" +
        (st.grade.toUpperCase() === "DNF" ? "Not finished" : "Graded") + "</span></p>" +
        (st.note ? '<p class="gnote">' + emph(st.note) + "</p>" : "") + "</div>";
    }

    /* Re-grade, for a verdict that has gone stale — the anchors moved, the
       prescription was edited, or the day was graded before the rest of it
       had been recorded. Offered only where the server would accept it. */
    if (d.can_regrade && st.date) {
      day += '<div class="dact"><button type="button" class="dregrade">' +
        (st.grade ? "Re-grade this day" : "Grade this day") + "</button>" +
        '<div class="dsay" hidden><label for="dsay">Anything the grader should know?' +
        ' <span>optional</span></label>' +
        '<textarea id="dsay" rows="3" maxlength="2000" placeholder="' +
        'e.g. the chain came off at mile 3 and the trainer would not re-engage"></textarea>' +
        '<p class="dsay-go"><button type="button" class="dgo">Grade it again</button>' +
        '<button type="button" class="dcancel">Cancel</button></p></div></div>';
    }

    /* What the athlete said about the day, where anything was said. Nothing
       is rendered when nothing was — no heading, no empty box: a placeholder
       claims a note exists. */
    if (d.notes && d.notes.length) {
      day += '<div class="dnotes">' + d.notes.map(function (n) {
        return "<p>" + emph(n.note) + "</p>";
      }).join("") + "</div>";
    }

    /* The skeleton is built once. .drec is the recording, .dday is the day,
       and only the first is replaced when the athlete switches pills. */
    var region = m.body.querySelector(".drec");
    if (!region) {
      m.body.innerHTML = picker(st) +
        '<div class="drec"></div><div class="dday"></div><p class="dsrc"></p>';
      bindPicker(m, st);
      region = m.body.querySelector(".drec");
      m.body.querySelector(".dday").innerHTML = day;
      bindRegrade(m, st);
    }
    /* A Leaflet instance holds document-level listeners, so the outgoing
       one is torn down rather than orphaned with its container. */
    if (st.map) {
      try { st.map.remove(); } catch (e) { /* already gone */ }
      st.map = null;
    }
    region.classList.remove("wait");
    region.innerHTML = h;
    m.body.querySelector(".dsrc").textContent = d.name;
    markPill(m, st);

    if (mapped) {
      /* Two fast switches must not leave the first one's map drawing into a
         container that is no longer on the page. */
      var mine = ++st.draws;
      var el = region.querySelector(".dmap");
      loadLeaflet()
        .then(function () {
          if (mine !== st.draws) return;
          st.map = drawMap(el, d.track);
        })
        .catch(function (e) {
          if (mine !== st.draws) return;
          el.className = "dmap-off";
          el.textContent = "The map did not load: " + e.message;
        });
    }
  }

  /* Which pill is lit. The picker itself is never re-rendered — its
     buttons keep the listeners bound on the first draw. */
  function markPill(m, st) {
    var pills = m.body.querySelectorAll("[data-pick]");
    for (var i = 0; i < pills.length; i++) {
      pills[i].classList.toggle("on", pills[i].getAttribute("data-pick") === st.name);
    }
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
    var hr = c.hr;
    var pr = run && c.pace && c.pace.length ? rangeOf(c.pace) : null;
    var hrr = hr && hr.length ? rangeOf(hr) : null;

    /* Two panels stacked on one clock rather than two lines in one box. On
       a run they crossed constantly and read as a tangle — and the crossings
       are not noise, they are his walk breaks, which is exactly the thing
       worth being able to see. */
    var W = 360, L = 36, R = 26, T = 8, GAP = 14, BOT = 16;
    var pw = W - L - R;
    var panels = [];
    if (run && pr) {
      panels.push({ vals: c.pace, range: pr, colour: "var(--accent)", invert: true,
                    label: "pace" + (c.unit || ""), fmt: mmss });
    }
    /* Watts on a run as well as a ride. On the bike ERG sets the level, so
       the trace is the trainer's shape; on a run it is the athlete's own
       output and worth trending on its own axis. Nothing is derived from a
       run's watts — no W/kg, no share of FTP — that stays the register's
       rule and this is a trace, not a number. */
    var wr = c.watts && c.watts.length ? rangeOf(c.watts) : null;
    if (wr) {
      /* The same ink as pace, deliberately. Each trace has its own labelled
         panel, so a third colour distinguishes nothing — and measured with
         tools/palette.py, --accent against --easy is dE2000 2.8 under
         deuteranopia, which is two colours that are one colour. What the
         chart actually asks a reader to tell apart is the effort trace from
         the heart-rate trace, and that pair measures 36.8. */
      panels.push({ vals: c.watts, range: wr, colour: "var(--accent)", invert: false,
                    label: "watts", fmt: Math.round, watts: true });
    }
    if (hrr) panels.push({ vals: hr, range: hrr, colour: "var(--hard)", invert: false,
                           label: "heart rate", fmt: Math.round, hr: true });
    if (!panels.length) return "";
    var ph = panels.length > 2 ? 58 : panels.length > 1 ? 72 : 110;
    var H = T + panels.length * ph + (panels.length - 1) * GAP + BOT;

    var span = c.secs[c.secs.length - 1] || 1;
    var x = function (s) { return L + pw * (s / span); };

    /* The cap the day is judged against, drawn where the heart rate is read
       and taken from the metrics payload, which resolves it against the
       anchors as they stand now — the same number the grade quotes. */
    var cap = null;
    if (mt && mt.grade_input) cap = mt.grade_input.grade_cap_bpm || mt.grade_input.hr_cap_bpm || null;
    if (cap && hrr) { hrr.lo = Math.min(hrr.lo, cap - 2); hrr.hi = Math.max(hrr.hi, cap + 2); }

    /* The band the grade is judged inside, from the same source as the cap:
       the grade_input the metrics payload resolves against the anchors as
       they stand now. Only bikes carry one — a run's grade is its share
       under the cap, so a run draws nothing new here. */
    var band = null;
    if (mt && mt.grade_input && mt.grade_input.hr_band_bpm) band = mt.grade_input.hr_band_bpm;
    if (band && hrr) { hrr.lo = Math.min(hrr.lo, band[0] - 2); hrr.hi = Math.max(hrr.hi, band[1] + 2); }

    /* The day's prescribed power band, resolved server-side from the same
       steps that pin the laps. Bikes with steps only; nothing else carries
       one, so a run's chart is untouched by construction. */
    var wband = c.power_band_w || null;
    var wpct = c.power_band_pct || null;
    if (wband && wr) { wr.lo = Math.min(wr.lo, wband[0] - 4); wr.hi = Math.max(wr.hi, wband[1] + 4); }

    var g = [];
    panels.forEach(function (p, i) {
      var top = T + i * (ph + GAP);
      /* CLAMPED to its own panel. Unclamped, his walk breaks — 13:41 against
         a 10:54 floor — drew straight down through the panel below and read
         as heart rate. The axis says where the floor is and marks it when
         something is pinned there. */
      var y = function (v) {
        var f = (v - p.range.lo) / (p.range.hi - p.range.lo);
        f = Math.max(0, Math.min(1, f));
        return top + ph * (p.invert ? f : 1 - f);
      };
      // A panel the eye can tell from its neighbour.
      g.push('<rect x="' + L + '" y="' + top + '" width="' + pw + '" height="' + ph +
        '" fill="var(--sunk)" opacity="0.5" rx="2"/>');
      if (p.watts && wband) {
        /* Same idiom as the HR band one panel down. The edges are dashed in
           the trace's own colour — the cap line's precedent: within a panel,
           the dash pattern is what separates a guide from the trace, which no
           colour vision is needed to see. The words carry the numbers, in
           watts and as a share of FTP. */
        var yWLo = y(wband[0]), yWHi = y(wband[1]);
        g.push('<rect x="' + L + '" y="' + yWHi.toFixed(1) + '" width="' + pw + '" height="' + Math.max(0, yWLo - yWHi).toFixed(1) +
          '" fill="var(--accent)" opacity="0.08"/>');
        [wband[0], wband[1]].forEach(function (bnd) {
          var yb = y(bnd).toFixed(1);
          g.push('<line x1="' + L + '" x2="' + (L + pw) + '" y1="' + yb + '" y2="' + yb +
            '" stroke="var(--accent)" stroke-width="1" stroke-dasharray="3 3" opacity="0.75"/>');
        });
        /* Above the upper edge when there is room; tucked inside the strip
           when there is not — a band riding the top of the range put this
           label on top of the panel's own WATTS caption. */
        var wLabelY = yWHi - 3 < top + 9 ? yWHi + 10 : yWHi - 3;
        g.push('<text x="' + (L + pw - 3) + '" y="' + wLabelY.toFixed(1) + '" text-anchor="end" font-size="9" fill="var(--ink-3)">target ' +
          wband[0] + "–" + wband[1] + " W" + (wpct ? " · " + wpct[0] + "–" + wpct[1] + "% FTP" : "") + "</text>");
      }
      if (p.hr && band) {
        /* The band the grade wants the trace inside, tinted so the in-band
           share — the number the bike grade IS — can be judged at a glance.
           Drawn before the cap so the cap's line and label sit on top.
           Accent against the trace's --hard measures dE2000 36.8 with
           tools/palette.py, and the words carry the meaning regardless. */
        var yLo = y(band[0]), yHi = y(band[1]);
        g.push('<rect x="' + L + '" y="' + yHi.toFixed(1) + '" width="' + pw + '" height="' + Math.max(0, yLo - yHi).toFixed(1) +
          '" fill="var(--accent)" opacity="0.08"/>');
        [band[0], band[1]].forEach(function (b) {
          var yb = y(b).toFixed(1);
          g.push('<line x1="' + L + '" x2="' + (L + pw) + '" y1="' + yb + '" y2="' + yb +
            '" stroke="var(--accent)" stroke-width="1" stroke-dasharray="3 3" opacity="0.75"/>');
        });
        /* Right-anchored where the cap's label is left-anchored: on a bike
           the cap sits a few beats above the band top, and two left labels
           would collide in a 58 px panel. */
        var bLabelY = yHi - 3 < top + 9 ? yHi + 10 : yHi - 3; // same caption-collision rule as the watts label
        g.push('<text x="' + (L + pw - 3) + '" y="' + bLabelY.toFixed(1) + '" text-anchor="end" font-size="9" fill="var(--ink-3)">band ' +
          band[0] + "–" + band[1] + "</text>");
      }
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
      /* Both ends of the scale, and no marker for the clamping: the range is
         the 5th to 95th percentile, so SOMETHING is always beyond both ends
         and a symbol that is always on says nothing. The box does the work —
         a line riding its edge is visibly at the edge. */
      [p.range.hi, p.range.lo].forEach(function (v) {
        var isHi = v === p.range.hi;
        g.push('<text x="' + (L - 4) + '" y="' + (y(v) + (isHi ? (p.invert ? 0 : 7) : (p.invert ? 7 : 0))).toFixed(1) +
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

  function loadLeaflet() {
    if (window.L) return Promise.resolve();
    if (leafletLoading) return leafletLoading;
    if (!vendor.js) return Promise.reject(new Error("no map library"));
    leafletLoading = new Promise(function (resolve, reject) {
      var pending = vendor.css ? 2 : 1;
      var fail = false;
      function one() { if (!--pending && !fail) resolve(); }
      /* BOTH halves, not just the script. Leaflet sizes its container from
         the stylesheet, so initialising while the CSS is still in flight
         gives a map with no height and no tiles — which looks exactly like
         a map that failed. */
      if (vendor.css) {
        var link = document.createElement("link");
        link.rel = "stylesheet";
        link.href = vendor.css;
        link.onload = one;
        link.onerror = function () { fail = true; reject(new Error("map styles did not load")); };
        document.head.appendChild(link);
      }
      var el = document.createElement("script");
      el.src = vendor.js;
      el.onload = one;
      el.onerror = function () { fail = true; reject(new Error("map library did not load")); };
      document.head.appendChild(el);
    });
    return leafletLoading;
  }

  /* Google's polyline algorithm, the decoding half. The encoder is in Go and
     a fixture test pins it against the published example, so this only has
     to be its inverse. */
  function decodePolyline(str) {
    var out = [], lat = 0, lon = 0, i = 0;
    function read() {
      var shift = 0, result = 0, b;
      do {
        b = str.charCodeAt(i++) - 63;
        result |= (b & 0x1f) << shift;
        shift += 5;
      } while (b >= 0x20 && i < str.length);
      return (result & 1) ? ~(result >> 1) : (result >> 1);
    }
    while (i < str.length) {
      lat += read();
      lon += read();
      out.push([lat / 1e5, lon / 1e5]);
    }
    return out;
  }

  function token(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return v || fallback;
  }

  /* Returns the map so the caller can tear it down: a Leaflet instance
     outlives its container and keeps document-level listeners. */
  function drawMap(el, track) {
    /* Leaflet writes the stroke as an SVG attribute, where a CSS var() does
       not resolve, so the tokens are read now and baked in. The popover is
       transient; a theme flipped underneath it is redrawn on the next open. */
    var colours = [token("--accent", "#3D4B96"), token("--easy", "#00819B")];
    var map = L.map(el, {
      zoomControl: true, scrollWheelZoom: false, attributionControl: true,
    });
    map.attributionControl.setPrefix("");
    L.tileLayer("/tiles/{z}/{x}/{y}", {
      minZoom: 3, maxZoom: 17,
      attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a>',
    }).addTo(map);

    var all = [];
    (track.segments || []).forEach(function (seg, i) {
      var pts = decodePolyline(seg.polyline);
      if (pts.length < 2) return;
      all = all.concat(pts);
      /* Alternating per lap, because his routes are out-and-backs and an
         undifferentiated stroke over a line that retraces itself says
         nothing about which mile is which. */
      L.polyline(pts, { color: colours[i % colours.length], weight: 3, opacity: 0.9 })
        .addTo(map)
        .bindTooltip(seg.lap ? "Lap " + seg.lap : "Before lap 1", { sticky: true });
    });
    if (all.length) map.fitBounds(all, { padding: [12, 12] });
    // The container is inside a popover that may still be settling; ask
    // twice rather than assume the first measurement was the real one.
    setTimeout(function () { if (map._container) map.invalidateSize(); }, 60);
    setTimeout(function () {
      if (!map._container) return; // torn down by a pill switch in between
      map.invalidateSize();
      if (all.length) map.fitBounds(all, { padding: [12, 12] });
    }, 400);
    return map;
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
