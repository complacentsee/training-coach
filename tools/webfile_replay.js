/* Drive static/webfile.js: files already on the computer, uploaded through the
   page, with no device of any kind.

     node tools/webfile_replay.js <webmtp.js> <webfile.js> [--case=…] [--names=out.tsv]

   Cases are the four things that actually happen: names the archive takes as
   they are, names it will not take until they are transliterated, a dismissed
   dialog, and a file whose bytes are damaged.

   --names writes `local<TAB>archived` for every name this run computed, which
   app/app_test.go's TestUploadNamesAreAcceptable then feeds to the REAL Go
   rule. That is the point: the JS namer mirrors validActivityName, and a
   mirror nobody checks is a mirror that drifts.

   Node stdlib only.
*/

"use strict";

const fs = require("fs");
const H = require("./webwatch_harness.js");

const SRCS = process.argv.slice(2).filter((a) => a.indexOf("--") !== 0);
if (SRCS.length < 2) {
  console.error("usage: node tools/webfile_replay.js <webmtp.js> <webfile.js> [--case=…] [--names=f]");
  process.exit(2);
}
const CASE = (process.argv.find((a) => a.indexOf("--case=") === 0) || "").split("=")[1] || "clean";
const NAMES = (process.argv.find((a) => a.indexOf("--names=") === 0) || "").split("=")[1] || "";

// The files the dialog "returns". Names chosen to be the awkward ones: a
// Zwift-style export, spaces, a unicode dash, a dotfile, a traversal attempt,
// something far too long, and one that is not FIT by name at all.
const CASES = {
  clean: [
    ["2026-08-16-07-00-00.fit", H.fitFile(0x11, 96)],
    ["12345_ACTIVITY.fit", H.fitFile(0x22, 64)],
  ],
  messy: [
    ["Zwift Activity - Watopia.fit", H.fitFile(0x33, 80)],
    ["ride–with–dashes.fit", H.fitFile(0x44, 48)],
    [".hidden.fit", H.fitFile(0x55, 32)],
    ["../../etc/passwd.fit", H.fitFile(0x66, 32)],
    ["a".repeat(140) + ".fit", H.fitFile(0x77, 32)],
    ["notes.txt", H.fitFile(0x88, 32)],
    ["....fit", H.fitFile(0x99, 32)],
  ],
  cancel: [],
  torn: [
    // A download that stopped early: framing says more bytes than arrived.
    ["2026-08-16-08-00-00.fit", H.fitFile(0xaa, 96).slice(0, -20)],
  ],
};
const chosen = CASES[CASE];
if (!chosen) { console.error("unknown --case: " + CASE); process.exit(2); }

function fakeFile(name, bytes) {
  return {
    name, size: bytes.length,
    async arrayBuffer() {
      const c = new Uint8Array(bytes.length); c.set(bytes); return c.buffer;
    },
  };
}

const page = H.newPage({});

// A file input the transport can click. The real one opens a dialog and later
// fires change (or cancel); this one does the same, asynchronously, because a
// synchronous resolve would hide an ordering bug in the transport.
const realCreate = page.document.createElement.bind(page.document);
page.document.createElement = (tag) => {
  const el = realCreate(tag);
  if (tag !== "input") return el;
  el.files = [];
  el.style = {};
  el.click = () => {
    setTimeout(() => {
      if (!chosen.length) { el.dispatch("cancel"); return; }
      el.files = chosen.map(([n, b]) => fakeFile(n, b));
      el.dispatch("change");
    }, 1);
  };
  return el;
};

H.load(page, { usb: undefined }, SRCS); // no WebUSB: upload must stand alone

const byId = page.byId;

(async () => {
  page.document.listeners.DOMContentLoaded.forEach((f) => f());
  await H.settle();
  const out = [];
  const status = () => byId.wstatus.textContent;

  out.push("case:          " + CASE);
  const buttons = page.btnHost.children;
  out.push("connect:       " + buttons.map((b) => JSON.stringify(b.textContent)).join(" "));

  const btn = buttons.length === 1 ? buttons[0] : buttons.find((b) => b.textContent === "Upload files");
  if (!btn) throw new Error("no upload button among " + buttons.length);
  btn.dispatch("click");
  await H.waitFor(page, "the dialog to settle", () => status() !== "Not connected." && !btn.disabled);
  out.push("after connect: " + status());
  out.push("  pull info:   " + byId.wpullinfo.textContent);
  out.push("  send button: " + (byId.wsend.disabled ? "off (nothing to send to)" : "armed"));

  const rows = byId.wpullrows.children.map((tr) => tr.querySelector(".wslug").textContent);
  if (rows.length) out.push("  rows:\n" + rows.map((r) => "                 " + r).join("\n"));

  if (!byId.wpull.disabled) {
    byId.wpull.dispatch("click");
    await H.waitFor(page, "the upload to finish",
      () => status().indexOf("Pulled") === 0 || status().indexOf("failed") > 0 || status().indexOf("Stopped:") === 0);
    out.push("after upload:  " + status());
    page.posts.forEach((p) => out.push("  ARCHIVED " + p.name + "  " + p.bytes.length + " bytes  now=" + p.now));
    byId.wpullrows.children.forEach((tr) =>
      out.push("  left over: " + tr.querySelector(".wslug").textContent + " — " + tr.querySelector(".wstate").textContent));
  }

  // Every name this run computed, for the Go side to judge.
  if (NAMES) {
    const lines = chosen.map(([local]) => {
      const row = byId.wpullrows.children.concat(byId.wsavedrows.children)
        .map((tr) => tr.querySelector(".wslug").textContent)
        .find((t) => t === local || t.indexOf(local + " → ") === 0);
      const archived = !row ? "" : (row.indexOf(" → ") > 0 ? row.split(" → ")[1] : row);
      return local + "\t" + archived;
    });
    fs.writeFileSync(NAMES, lines.join("\n") + "\n");
    out.push("  names written to " + NAMES + " for the Go rule to check");
  }

  console.log(out.join("\n"));
})().catch((e) => { console.error("replay failed: " + e.stack); process.exit(1); });
