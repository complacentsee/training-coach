/* The coach page: the day's transcript from /api/chat, a composer that
   POSTs one message and then polls until the reply lands — the regrade
   pattern, nothing streams. Text renders through AppModal.emph so the
   model's *strong* and _em_ read the way a note's do and nothing else
   is markup. */
(function () {
  "use strict";
  var root = document.getElementById("coach");
  if (!root) return;
  var date = root.dataset.date;
  var isToday = !!root.dataset.today;
  var list = document.getElementById("chat");
  var state = document.getElementById("chat-state");
  var form = document.getElementById("say");
  var left = document.getElementById("say-left");
  var days = document.getElementById("coach-days");
  var pollTimer = null, tries = 0;

  function esc(s) { return window.AppModal ? window.AppModal.esc(s) : String(s); }
  function emph(s) { return window.AppModal ? window.AppModal.emph(s) : esc(s); }

  function render(d) {
    var h = "";
    d.messages.forEach(function (m) {
      var who = m.role === "user" ? "You" : m.role === "assistant" ? "Coach" : "";
      h += '<li class="msg ' + esc(m.role) + '">' +
        (who ? '<span class="who">' + who + "</span>" : "") +
        '<div class="text">' + emph(m.text) + "</div></li>";
    });
    if (d.busy) h += '<li class="msg wait"><span class="who">Coach</span><div class="text hint">Thinking — reading the plan and the log…</div></li>';
    list.innerHTML = h;
    if (!d.messages.length && !d.busy) {
      state.textContent = isToday ? "Nothing asked yet today." : "Nothing was asked that day.";
      state.hidden = false;
    } else {
      state.hidden = true;
    }
    if (left) left.textContent = (d.cap - d.turns) + " of " + d.cap + " messages left today";
    if (form) {
      var ta = form.querySelector("textarea"), btn = form.querySelector("button");
      ta.disabled = btn.disabled = d.busy || d.turns >= d.cap;
    }
    if (days && d.days && d.days.length) {
      var others = d.days.filter(function (x) { return x !== date; }).slice(0, 7);
      days.innerHTML = others.length ? "Earlier: " + others.map(function (x) {
        return '<a href="/coach?date=' + esc(x) + '">' + esc(x) + "</a>";
      }).join(" · ") : "";
    }
    if (d.busy) schedule(); else { tries = 0; if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; } }
    window.scrollTo(0, document.body.scrollHeight);
  }

  function load() {
    return fetch("/api/chat?date=" + encodeURIComponent(date), { cache: "no-store" })
      .then(function (r) { if (!r.ok) throw new Error(r.status + " " + r.statusText); return r.json(); })
      .then(render)
      .catch(function (e) { state.textContent = "Could not load the conversation: " + e.message; state.hidden = false; });
  }

  /* Poll while a turn is in flight: every 2 s, up to the turn's own
     two-minute bound plus a little. The server keeps going if the page
     gives up; the reply is there on the next open. */
  function schedule() {
    if (pollTimer) return;
    pollTimer = setTimeout(function () {
      pollTimer = null;
      if (++tries > 75) { state.textContent = "Still thinking. It will be here when you next open this."; state.hidden = false; return; }
      load();
    }, 2000);
  }

  if (form) {
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      var ta = form.querySelector("textarea"), btn = form.querySelector("button");
      var text = ta.value.trim();
      if (!text) return;
      ta.disabled = btn.disabled = true;
      fetch("/api/chat", { method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ date: date, text: text }) })
        .then(function (r) {
          if (r.status === 202) { ta.value = ""; return load(); }
          return r.text().then(function (t) { throw new Error(t || (r.status + " " + r.statusText)); });
        })
        .catch(function (err) {
          ta.disabled = btn.disabled = false;
          state.textContent = err.message; state.hidden = false;
        });
    });
  }
  /* app.js (deferred, after this script in the document) provides
     AppModal.emph, and installs it from its own ready handler, which runs
     after any this script registers. So the first render waits for the
     window's load event — by then every script has run — rather than
     racing the fetch: measured 21 Aug 2026, a same-host fetch beat app.js
     one render in three and the reply showed bare stars. */
  if (document.readyState === "complete") load();
  else window.addEventListener("load", load);
})();
