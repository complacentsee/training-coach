/* Run Coach — one IIFE on purpose.
   This file previously had two, and a handler in the second called post()
   and say() from the first. That threw ReferenceError on every subtask
   click, silently: the box ticked visually and nothing was ever saved. */
(function () {
  "use strict";

  /* ── shared ─────────────────────────────────────────────────────────── */

  var toast = document.createElement("div");
  toast.className = "toast";
  document.body.appendChild(toast);
  var toastTimer;

  function say(msg, bad) {
    toast.textContent = msg;
    toast.style.background = bad ? "var(--stop)" : "";
    toast.classList.add("on");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { toast.classList.remove("on"); }, 2200);
  }

  function post(entry) {
    return fetch("/api/entry", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(entry)
    }).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || r.status); });
    });
  }

  function esc(s) {
    return String(s).replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }

  /* emph mirrors emphasise() in main.go: *strong*, _em_, newline -> break.
     Escaping happens FIRST, so the only tags that can reach the page are the
     three this function writes -- guide copy is data, and data does not get to
     write markup. */
  function emph(s) {
    return esc(s)
      .replace(/\*([^*]+)\*/g, "<strong>$1</strong>")
      .replace(/_([^_]+)_/g, "<em>$1</em>")
      .replace(/\n/g, "<br>");
  }

  var dateEl = document.querySelector("[data-date]");
  var today = dateEl ? dateEl.dataset.date : "";

  /* ── parent task checkboxes ─────────────────────────────────────────── */

  Array.prototype.forEach.call(document.querySelectorAll("input[data-task]"), function (b) {
    b.addEventListener("change", function () {
      var want = b.checked;
      b.disabled = true;
      post({ kind: "task", date: b.dataset.date, key: b.dataset.task, val: want ? "done" : "undone" })
        .then(function () { say(want ? "Logged" : "Unchecked"); })
        .catch(function (e) { b.checked = !want; say("Failed: " + e.message, true); })
        .finally(function () { b.disabled = false; });
    });
  });

  /* ── issue ratings ──────────────────────────────────────────────────
     One scale per declared issue, so the page carries however many the
     athlete has rather than one hardcoded calf. Everything is scoped to the
     form the button lives in — the previous version reached for the first
     .card .last on the page, which was fine while there was exactly one. */

  /* repaint updates the whole card from the button that was tapped. The button
     carries what its value means, so the prescription changes with the rating
     instead of waiting for the next page load -- which mattered most when
     re-rating across a band, where the card used to sit showing "train as
     written" under a number that means stop. */
  function repaint(card, b, noteText) {
    var line = card.querySelector(".last");
    if (line) {
      line.classList.add("logged");
      var band = b.dataset.label
        ? ' · <b class="tone-' + esc(b.dataset.tone) + '">' + esc(b.dataset.label) + "</b>"
        : "";
      line.innerHTML = "Logged today: <strong>" + esc(b.dataset.val) + "</strong>" + band +
        (noteText ? " — " + esc(noteText) : "") +
        '<br><span class="sub">Tap another number to change it.</span>';
    }
    var act = card.querySelector(".issue-action");
    if (act) {
      act.className = "issue-action" + (b.dataset.tone ? " tone-" + b.dataset.tone : "");
      act.innerHTML = b.dataset.rx ? emph(b.dataset.rx) : "";
    }
  }

  /* A failed post must put the card back exactly, prescription included. */
  function snapshot(card) {
    var line = card.querySelector(".last"), act = card.querySelector(".issue-action");
    return {
      line: line ? line.innerHTML : null,
      lineCls: line ? line.className : null,
      act: act ? act.innerHTML : null,
      actCls: act ? act.className : null
    };
  }

  function restore(card, s) {
    var line = card.querySelector(".last"), act = card.querySelector(".issue-action");
    if (line && s.line !== null) { line.innerHTML = s.line; line.className = s.lineCls; }
    if (act && s.act !== null) { act.innerHTML = s.act; act.className = s.actCls; }
  }

  Array.prototype.forEach.call(document.querySelectorAll("form.rate[data-issue]"), function (form) {
    var card = form.closest(".card");
    var dots = Array.prototype.slice.call(form.querySelectorAll(".dot"));

    dots.forEach(function (b) {
      b.addEventListener("click", function () {
        var note = form.querySelector("input[name=note]");
        var prev = form.querySelector(".dot.sel");
        dots.forEach(function (o) {
          o.classList.remove("sel");
          o.removeAttribute("aria-pressed");
        });
        b.classList.add("sel");
        b.setAttribute("aria-pressed", "true");

        var before = card ? snapshot(card) : null;

        post({ kind: "issue", key: form.dataset.issue, date: form.dataset.date,
               val: b.dataset.val, note: note.value.trim() })
          .then(function () {
            var name = form.dataset.name || form.dataset.issue;
            say(name + " " + b.dataset.val + " recorded");
            if (card) repaint(card, b, note.value.trim());
          })
          .catch(function (e) {
            if (card && before) restore(card, before);
            b.classList.remove("sel");
            b.removeAttribute("aria-pressed");
            if (prev) { prev.classList.add("sel"); prev.setAttribute("aria-pressed", "true"); }
            say("Failed: " + e.message, true);
          });
      });
    });
  });

  /* ── feedback ───────────────────────────────────────────────────────── */

  var noteForm = document.getElementById("note");
  if (noteForm) {
    noteForm.addEventListener("submit", function (ev) {
      ev.preventDefault();
      var ta = noteForm.querySelector("textarea");
      var text = ta.value.trim();
      if (!text) return;
      post({ kind: "note", date: noteForm.dataset.date, note: text })
        .then(function () { ta.value = ""; say("Sent"); })
        .catch(function (e) { say("Failed: " + e.message, true); });
    });
  }

  /* ── guide popups ───────────────────────────────────────────────────── */

  /* ── skipping a session ──────────────────────────────────────────────
     Two taps. The first arms and says what the second will do, because a
     session marked not-done by a stray thumb is a lie in the record that
     nothing else would catch. Undoing needs only one: putting a day back
     the way it was is not the consequential direction. */
  Array.prototype.forEach.call(document.querySelectorAll("form.skip"), function (form) {
    var btn = form.querySelector(".skipbtn");
    var note = form.querySelector("input[name=note]");
    var state = form.querySelector(".skip-state");
    var card = form.closest(".card");
    var armTimer;

    function disarm() {
      clearTimeout(armTimer);
      form.classList.remove("armed");
      note.hidden = true;
      btn.textContent = form.classList.contains("on") ? "Undo" : "Couldn't do this";
    }

    btn.addEventListener("click", function () {
      var skipping = !form.classList.contains("on");

      if (skipping && !form.classList.contains("armed")) {
        form.classList.add("armed");
        note.hidden = false;
        btn.textContent = "Confirm";
        note.focus();
        /* An armed button left alone goes back to asking. */
        clearTimeout(armTimer);
        armTimer = setTimeout(disarm, 8000);
        return;
      }

      var reason = note.value.trim();
      btn.disabled = true;
      post({ kind: "skip", date: form.dataset.date,
             val: skipping ? "skipped" : "unskipped", note: reason })
        .then(function () {
          form.classList.toggle("on", skipping);
          if (card) card.classList.toggle("skipped", skipping);
          if (state) {
            state.textContent = skipping
              ? "Not done" + (reason ? " — " + reason : "")
              : "";
          }
          if (!skipping) note.value = "";
          disarm();
          say(skipping ? "Marked not done" : "Back on");
        })
        .catch(function (e) { say("Failed: " + e.message, true); })
        .finally(function () { btn.disabled = false; });
    });
  });

  var modal = document.getElementById("modal");
  if (!modal) return;
  var box = modal.querySelector(".modal-box");
  var titleEl = document.getElementById("modal-title");
  var bodyEl = document.getElementById("modal-body");
  box.setAttribute("tabindex", "-1");

  var guides = null, loading = null, lastFocus = null, stack = [], currentID = null;
  var tasks = {};   /* step key -> done, for today */

  /* Guides are the block's, so the popup has to ask for the block the page is
     showing. Without this an archived calendar opened its own guide titles
     with the current block's paces resolved inside them. */
  var blockQ = "";
  var blockParam = new URLSearchParams(location.search).get("block");
  if (blockParam) blockQ = "?block=" + encodeURIComponent(blockParam);

  function load() {
    if (guides) return Promise.resolve(guides);
    if (!loading) {
      loading = Promise.all([
        fetch("/api/guides" + blockQ).then(function (r) {
          if (!r.ok) throw new Error("HTTP " + r.status);
          return r.json();
        }),
        fetch("/api/tasks?date=" + encodeURIComponent(today))
          .then(function (r) { return r.ok ? r.json() : {}; })
          .catch(function () { return {}; })
      ]).then(function (res) {
        guides = res[0];
        tasks = res[1] || {};
        return guides;
      }).catch(function (e) { loading = null; throw e; });
    }
    return loading;
  }

  function render(g) {
    currentID = g.id;
    titleEl.textContent = g.title;
    var h = "";
    (g.sections || []).forEach(function (s) {
      h += '<div class="g-sec"><span class="g-lab">' + esc(s.label) + "</span><p>" + emph(s.text) + "</p></div>";
    });
    if (g.steps && g.steps.length) {
      h += '<ul class="g-steps">' + g.steps.map(function (t, i) {
        var txt = typeof t === "string" ? t : t.text;
        var gid = typeof t === "string" ? "" : t.guide;
        var key = typeof t === "string" ? "" : t.key;
        var id = "s-" + String(key || g.id + "-" + i).replace(/[^a-z0-9-]/gi, "_");
        var sets = (typeof t === "object" && t.sets > 1 && key) ? t.sets : 0;
        var pills = "";
        if (sets) {
          pills = '<div class="g-sets" role="group" aria-label="sets">';
          for (var n = 1; n <= sets; n++) {
            var sk = key + ":" + n, on = !!tasks[sk];
            pills += '<button type="button" class="setdot' + (on ? " on" : "") +
              '" data-set="' + esc(sk) + '" data-parent="' + esc(g.id) +
              '" aria-pressed="' + on + '" aria-label="Set ' + n + '">' + n + "</button>";
          }
          pills += "</div>";
        }
        return '<li class="g-step">' +
          (key ? '<input type="checkbox" id="' + id + '" data-step="' + esc(key) +
                 '" data-parent="' + esc(g.id) + '"' + (tasks[key] ? " checked" : "") + ">" : "") +
          '<div><label for="' + id + '">' + emph(txt) + "</label>" +
          (gid ? ' <button class="gbtn" data-guide="' + esc(gid) + '" aria-label="How to do this">?</button>' : "") +
          pills + "</div></li>";
      }).join("") + "</ul>";
    }
    if (g.why) h += '<p class="g-why">' + emph(g.why) + "</p>";
    if (g.video || g.article) {
      h += '<div class="g-links">';
      if (g.video) h += '<a href="' + esc(g.video) + '" target="_blank" rel="noopener">Watch ↗</a>';
      if (g.article) h += '<a class="rd" href="' + esc(g.article) + '" target="_blank" rel="noopener">Read ↗</a>';
      if (g.source) h += '<span class="g-src">' + esc(g.source) + "</span>";
      h += "</div>";
    }
    bodyEl.innerHTML = h;

    if (stack.length) {
      var back = document.createElement("button");
      back.className = "g-back";
      back.textContent = "← back";
      back.addEventListener("click", function () {
        var prev = stack.pop();
        open(prev, stack.length ? stack[stack.length - 1] : null);
      });
      bodyEl.insertBefore(back, bodyEl.firstChild);
    }
  }

  function open(id, push) {
    if (push) {
      if (stack[stack.length - 1] !== push) stack.push(push);
    } else {
      stack = [];
      lastFocus = document.activeElement;
    }
    titleEl.textContent = "Loading…";
    bodyEl.innerHTML = "";
    modal.hidden = false;
    document.body.style.overflow = "hidden";
    box.focus();
    load().then(function (all) {
      var g = all[id];
      if (!g) {
        titleEl.textContent = "Not found";
        bodyEl.innerHTML = "<p>No guide for “" + esc(id) + "”.</p>";
        return;
      }
      render(g);
    }).catch(function (e) {
      titleEl.textContent = "Unavailable";
      bodyEl.innerHTML = "<p>Could not load guides: " + esc(e.message) + "</p>";
    });
  }

  function close() {
    stack = [];
    modal.hidden = true;
    document.body.style.overflow = "";
    if (lastFocus) lastFocus.focus();
  }

  /* Closing a movement note opened from inside a session returns you to that
     session rather than dumping you out of it. Checking a cue mid-workout
     should not cost you your place in the set tracker. */
  function dismiss() {
    if (stack.length) {
      var prev = stack.pop();
      open(prev, stack.length ? stack[stack.length - 1] : null);
      return;
    }
    close();
  }

  /* A grade is a judgement, and the reasoning behind it is the useful half:
     what the session actually was, measured against what was asked, and how
     that arrived at the letter. It was reachable only by hovering, which is a
     tooltip on a desktop and nothing whatsoever on a phone — where he reads
     this. So the letter is a button, and it opens in the panel the guides
     already use rather than inventing a second kind of popup. */
  function openNote(el) {
    var note = el.getAttribute("data-note") || "";
    var g = el.getAttribute("data-grade") || "";
    var range = el.getAttribute("data-range") || "";
    var when = el.getAttribute("data-when") || "";
    stack = [];
    lastFocus = document.activeElement;
    titleEl.textContent = when || "Grade";
    var h = "";
    if (g) {
      h += '<p class="g-band"><b class="g g' + esc(g.toLowerCase()) + '">' + esc(g) + "</b>" +
        (range ? " <span>" + esc(range) + " under the cap</span>" : "") + "</p>";
    }
    h += "<p>" + (note ? emph(note) : "No note was recorded with this grade.") + "</p>";
    bodyEl.innerHTML = h;
    modal.hidden = false;
    document.body.style.overflow = "hidden";
    box.focus();
  }

  /* The units a guide is measured in: one per set where sets are declared,
     otherwise one per step. Mirrors ProgressUnits in library.go. */
  function progressUnits(g) {
    var out = [];
    (g.steps || []).forEach(function (s) {
      if (!s.key) return;
      if (s.sets > 1) { for (var n = 1; n <= s.sets; n++) out.push(s.key + ":" + n); }
      else out.push(s.key);
    });
    return out;
  }

  function refreshProgress(parent) {
    var g = (guides || {})[parent];
    if (!g) return;
    var units = progressUnits(g);
    var done = units.filter(function (k) { return tasks[k]; }).length;
    var el = document.querySelector('[data-prog="' + parent + '"]');
    if (el) el.textContent = done + "/" + units.length;

    /* Finishing every exercise completes the parent task. Driven by the
       exercise boxes, not the sets, so marking something done after two of
       three sets still counts -- which is the whole point of keeping the two
       independent. */
    var steps = (g.steps || []).filter(function (s) { return s.key; });
    if (steps.length && steps.every(function (s) { return tasks[s.key]; })) {
      var pk = parent.replace(/^task-/, "");
      var pbox = document.querySelector('input[data-task="' + pk + '"]');
      if (pbox && !pbox.checked) {
        post({ kind: "task", date: today, key: pk, val: "done" })
          .then(function () { pbox.checked = true; })
          .catch(function () { /* parent tick is a convenience; ignore */ });
      }
    }
  }

  /* step checkboxes inside a guide */
  document.addEventListener("change", function (e) {
    var b = e.target.closest("input[data-step]");
    if (!b) return;
    var key = b.dataset.step, parent = b.dataset.parent, want = b.checked;
    b.disabled = true;
    post({ kind: "task", date: today, key: key, val: want ? "done" : "undone" })
      .then(function () {
        tasks[key] = want;
        say(want ? "Logged" : "Unchecked");
        refreshProgress(parent);
      })
      .catch(function (err) { b.checked = !want; say("Failed: " + err.message, true); })
      .finally(function () { b.disabled = false; });
  });

  /* per-set pills inside a guide */
  document.addEventListener("click", function (e) {
    var d = e.target.closest("button[data-set]");
    if (!d) return;
    var key = d.dataset.set, parent = d.dataset.parent, want = !tasks[key];
    d.disabled = true;
    post({ kind: "task", date: today, key: key, val: want ? "done" : "undone" })
      .then(function () {
        tasks[key] = want;
        d.classList.toggle("on", want);
        d.setAttribute("aria-pressed", String(want));
        say(want ? "Set logged" : "Set cleared");
        /* Completing the last set ticks the exercise as a convenience. It
           never un-ticks: clearing a set does not undo a deliberate "done". */
        var step = ((guides || {})[parent] || {}).steps.find(function (s) {
          return s.key === key.split(":")[0];
        });
        if (step && step.sets > 1 && !tasks[step.key]) {
          var all = true;
          for (var n = 1; n <= step.sets; n++) { if (!tasks[step.key + ":" + n]) all = false; }
          if (all) {
            post({ kind: "task", date: today, key: step.key, val: "done" }).then(function () {
              tasks[step.key] = true;
              var box = document.querySelector('input[data-step="' + step.key + '"]');
              if (box) box.checked = true;
              refreshProgress(parent);
            }).catch(function () { /* convenience only */ });
          }
        }
        refreshProgress(parent);
      })
      .catch(function (err) { say("Failed: " + err.message, true); })
      .finally(function () { d.disabled = false; });
  });

  /* The modal shell, exposed for page features (trend.js) that fill it
     with their own content rather than a guide. */
  window.AppModal = {
    show: function (title) {
      stack = [];
      lastFocus = document.activeElement;
      titleEl.textContent = title || "Loading…";
      bodyEl.innerHTML = "";
      modal.hidden = false;
      document.body.style.overflow = "hidden";
      box.focus();
      return { title: titleEl, body: bodyEl };
    },
    esc: esc,
  };

  document.addEventListener("click", function (e) {
    /* A real link inside a guide-opening cell — the calendar's FIT download
       — must navigate, not open the popup. */
    if (e.target.closest("a[href]")) return;
    /* A grade letter sits inside a cell that opens a guide, so it is checked
       first: clicking the letter is asking about the grade, not the workout. */
    var gn = e.target.closest("[data-note]");
    if (gn) {
      e.preventDefault();
      openNote(gn);
      return;
    }
    var t = e.target.closest("[data-guide]");
    if (t && t.dataset.guide) {
      e.preventDefault();
      open(t.dataset.guide, e.target.closest("#modal") ? currentID : null);
      return;
    }
    /* The × steps back one level; tapping the backdrop is the deliberate
       "get me out of here" and closes the lot. */
    if (e.target.closest(".modal-x")) dismiss();
    else if (e.target === modal) close();
  });

  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && !modal.hidden) dismiss();
  });

  /* deep link: /?guide=m-sl-bent or #guide=m-sl-bent */
  var want = new URLSearchParams(location.search).get("guide") ||
             (location.hash.indexOf("#guide=") === 0 ? location.hash.slice(7) : "");
  if (want) open(want);
})();
