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

  /* A proposal is a card: the rework candidate the model chose, with
     Apply (armed twice, like every change here) and Dismiss. Apply is the
     rework control's own POST to /api/amend; the decision is then recorded
     on the conversation. A decided card shows what happened to it. */
  var decided = {};
  function proposalCard(m) {
    var p = m.data || {};
    var state = decided[m.id];
    var h = '<li class="msg proposal' + (state ? " " + esc(state) : "") + '" data-id="' + esc(m.id) + '">' +
      '<span class="who">Proposed</span><div class="text"><b>' + esc(p.title || m.text) + "</b>" +
      (p.detail ? '<span class="pdetail">' + esc(p.detail) + "</span>" : "") +
      (p.reason ? '<span class="preason">' + emph(p.reason) + "</span>" : "") + "</div>";
    if (state) {
      h += '<p class="pstate">' + (state === "applied" ? "Applied — the week shows it" : "Dismissed") + "</p>";
    } else if (isToday) {
      h += '<div class="pact"><button type="button" class="skipbtn papply" data-date="' + esc(p.date) + '" data-op="' + esc(p.op) + '" data-arg="' + esc(p.arg || "") + '" data-note="' + esc(p.reason || "") + '">Apply</button>' +
        '<button type="button" class="skipbtn pdismiss">Dismiss</button></div>';
    }
    return h + "</li>";
  }

  function render(d) {
    var h = "";
    decided = {};
    d.messages.forEach(function (m) {
      if (m.role === "decision" && m.data) decided[m.data.proposal] = m.data.status;
    });
    d.messages.forEach(function (m) {
      if (m.role === "decision") return;
      if (m.role === "proposal") { h += proposalCard(m); return; }
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
    wireProposals();
    window.scrollTo(0, document.body.scrollHeight);
  }

  function decide(card, status) {
    return fetch("/api/chat/decide", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ date: date, proposal: card.dataset.id, status: status }) })
      .then(function (r) { if (!r.ok) return r.text().then(function (t) { throw new Error(t || r.status); }); });
  }

  function wireProposals() {
    Array.prototype.forEach.call(list.querySelectorAll(".msg.proposal"), function (card) {
      var apply = card.querySelector(".papply"), dismiss = card.querySelector(".pdismiss");
      if (!apply) return;
      var timer;
      apply.addEventListener("click", function () {
        if (!apply.classList.contains("armed")) {
          apply.classList.add("armed"); apply.textContent = "Apply — sure?";
          clearTimeout(timer);
          timer = setTimeout(function () { apply.classList.remove("armed"); apply.textContent = "Apply"; }, 8000);
          return;
        }
        apply.disabled = dismiss.disabled = true;
        fetch("/api/amend", { method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ date: apply.dataset.date, op: apply.dataset.op, arg: apply.dataset.arg, note: apply.dataset.note }) })
          .then(function (r) { if (!r.ok) return r.text().then(function (t) { throw new Error((t || ("HTTP " + r.status)).trim()); }); })
          .then(function () { return decide(card, "applied"); })
          .then(load)
          .catch(function (err) {
            apply.disabled = dismiss.disabled = false;
            apply.classList.remove("armed"); apply.textContent = "Apply";
            state.textContent = "Not applied: " + err.message; state.hidden = false;
          });
      });
      dismiss.addEventListener("click", function () {
        apply.disabled = dismiss.disabled = true;
        decide(card, "dismissed").then(load).catch(function (err) {
          apply.disabled = dismiss.disabled = false;
          state.textContent = err.message; state.hidden = false;
        });
      });
    });
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
