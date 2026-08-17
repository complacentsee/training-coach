/* Drive static/webmtp.js against a FAKE watch and print every byte it puts on
   the wire.

   The page is 900 lines of measured MTP truth that no test in this repo
   exercises, because exercising it has always meant plugging in a watch. That
   makes any refactor of it unfalsifiable: a diff can look right and still
   change the bytes. This replaces the watch with a responder that speaks the
   same container protocol, so the proof of a refactor becomes a byte-for-byte
   diff of two traces:

     node tools/webmtp_replay.js path/to/old/webmtp.js > old.txt
     node tools/webmtp_replay.js path/to/new/webmtp.js > new.txt
     diff old.txt new.txt

   It is NOT a substitute for the watch. A fake responder proves that the code
   still sends what it used to send; only the Epix proves that what it sends is
   what the Epix accepts. Both are needed, and this one is the cheap half.

   Determinism is the whole point, so Date is frozen: the trace stamps every
   line with a clock, and SendObjectInfo writes a modification date into the
   bytes it sends. Left alone, two runs of the SAME file would differ.

   Node stdlib only — no packages, same reason the FIT encoder has no library.
*/

"use strict";

const fs = require("fs");
const vm = require("vm");

const SRC = process.argv[2];
if (!SRC) {
  console.error("usage: node tools/webmtp_replay.js <path-to-webmtp.js> [--split]");
  process.exit(2);
}
// --split makes the responder use MTP 1.1 Appendix H framing (a lone 12-byte
// data-phase header, payload separately), which the Epix does. Both framings
// are worth replaying: the page detects the split from the read side and
// mirrors it on writes, so they produce genuinely different wire bytes.
const SPLIT = process.argv.indexOf("--split") >= 0;
// --desync=pull|send makes the responder answer with the wrong transaction id
// once the named phase starts, which is the condition the page calls a
// desynced session. It is the cheap half of the fatal-error path: the other
// trigger is a 15-second silence, which is the same flag by a slower route.
// This exercises what used to be decided by searching an error message for
// the words "desynced" and "no answer".
const DESYNC = (process.argv.find((a) => a.indexOf("--desync=") === 0) || "").split("=")[1] || "";

/* ── the fake watch ──────────────────────────────────────────────────── */

const STORAGE = 0x10001;
const FMT_FOLDER = 0x3001, FMT_UNDEFINED = 0x3000;

const CRC_TABLE = [0x0000, 0xCC01, 0xD801, 0x1400, 0xF001, 0x3C00, 0x2800, 0xE401,
  0xA001, 0x6C00, 0x7800, 0xB401, 0x5000, 0x9C01, 0x8801, 0x4400];
function crc16(buf) {
  let crc = 0;
  for (const byte of buf) {
    let tmp = CRC_TABLE[crc & 0x0f];
    crc = (crc >> 4) & 0x0fff;
    crc = crc ^ tmp ^ CRC_TABLE[byte & 0x0f];
    tmp = CRC_TABLE[crc & 0x0f];
    crc = (crc >> 4) & 0x0fff;
    crc = crc ^ tmp ^ CRC_TABLE[(byte >> 4) & 0x0f];
  }
  return crc & 0xffff;
}

// A tiny object tree, shaped exactly like the watch's: GARMIN with NewFiles
// and Activity under it, and two recordings to pull.
const objects = {
  1: { name: "GARMIN", format: FMT_FOLDER, parent: 0xffffffff, bytes: null },
  2: { name: "NewFiles", format: FMT_FOLDER, parent: 1, bytes: null },
  3: { name: "Activity", format: FMT_FOLDER, parent: 1, bytes: null },
  4: { name: "2026-08-15-06-30-00.fit", format: FMT_UNDEFINED, parent: 3, bytes: fitFile(0x11, 96) },
  5: { name: "2026-08-15-17-05-00.fit", format: FMT_UNDEFINED, parent: 3, bytes: fitFile(0x22, 64) },
};
let nextHandle = 6;

// fitFile builds a well-framed FIT container — the same arithmetic the
// server's ingest gate checks — so what this fake hands over is the shape of
// a real recording rather than arbitrary bytes.
function fitFile(seed, dataLen) {
  const b = Buffer.alloc(14 + dataLen + 2);
  b[0] = 14; b[1] = 0x10;
  b.writeUInt16LE(2115, 2);
  b.writeUInt32LE(dataLen, 4);
  b.write(".FIT", 8, "latin1");
  b.writeUInt16LE(crc16(b.subarray(0, 12)), 12);
  for (let i = 0; i < dataLen; i++) b[14 + i] = (seed + i * 7) & 0xff;
  b.writeUInt16LE(crc16(b.subarray(14, 14 + dataLen)), 14 + dataLen);
  return new Uint8Array(b);
}

function ptpString(s) {
  if (!s) return Buffer.from([0]);
  const b = Buffer.alloc(1 + (s.length + 1) * 2);
  b[0] = s.length + 1;
  for (let i = 0; i < s.length; i++) b.writeUInt16LE(s.charCodeAt(i), 1 + i * 2);
  return b;
}

function deviceInfo() {
  const ops = [0x1001, 0x1002, 0x1003, 0x1004, 0x1007, 0x1008, 0x1009, 0x100c, 0x100d];
  const head = Buffer.alloc(8);
  head.writeUInt16LE(100, 0);      // standard version
  head.writeUInt32LE(6, 2);        // vendor extension id (Microsoft)
  head.writeUInt16LE(100, 6);      // extension version
  const ext = ptpString("microsoft.com: 1.0;");
  const mode = Buffer.alloc(2);    // functional mode
  const opsBuf = Buffer.alloc(4 + ops.length * 2);
  opsBuf.writeUInt32LE(ops.length, 0);
  ops.forEach((o, i) => opsBuf.writeUInt16LE(o, 4 + i * 2));
  const empty = Buffer.alloc(4);   // an empty counted array
  return Buffer.concat([head, ext, mode, opsBuf, empty, empty, empty, empty,
    ptpString("Garmin"), ptpString("Epix Pro"),
    ptpString("2.10"), ptpString("0000000000")]);
}

function objectInfoFor(h) {
  const o = objects[h];
  const fixed = Buffer.alloc(52);
  fixed.writeUInt32LE(STORAGE, 0);
  fixed.writeUInt16LE(o.format, 4);
  fixed.writeUInt32LE(o.bytes ? o.bytes.length : 0, 8);
  fixed.writeUInt32LE(o.parent, 38);
  return Buffer.concat([fixed, ptpString(o.name), ptpString(""), ptpString(""), ptpString("")]);
}

function handlesUnder(parent) {
  return Object.keys(objects).map(Number).filter((h) => objects[h].parent === parent);
}

function u32Array(list) {
  const b = Buffer.alloc(4 + list.length * 4);
  b.writeUInt32LE(list.length, 0);
  list.forEach((v, i) => b.writeUInt32LE(v, 4 + i * 4));
  return b;
}

function container(type, code, tid, params, payload) {
  const p = params || [];
  const len = 12 + p.length * 4 + (payload ? payload.length : 0);
  const b = Buffer.alloc(len);
  b.writeUInt32LE(len, 0);
  b.writeUInt16LE(type, 4);
  b.writeUInt16LE(code, 6);
  b.writeUInt32LE(tid, 8);
  p.forEach((v, i) => b.writeUInt32LE(v >>> 0, 12 + i * 4));
  if (payload) Buffer.from(payload).copy(b, 12 + p.length * 4);
  return new Uint8Array(b);
}

// The responder: a queue of replies the page's transferIn drains in order.
const replies = [];
const written = [];   // every object the page created, for the report
let pending = null;   // a command awaiting its host data phase

function queueData(code, tid, payload) {
  const full = container(2, code, tid, [], payload);
  if (SPLIT) {
    replies.push(full.subarray(0, 12));
    replies.push(full.subarray(12));
  } else {
    replies.push(full);
  }
}

function handleCommand(code, tid, params) {
  switch (code) {
    case 0x1002: replies.push(container(3, 0x2001, tid, [])); return;           // OpenSession
    case 0x1003: replies.push(container(3, 0x2001, tid, [])); return;           // CloseSession
    case 0x1001: queueData(code, tid, deviceInfo()); break;                      // GetDeviceInfo
    case 0x1004: queueData(code, tid, u32Array([STORAGE])); break;               // GetStorageIDs
    case 0x1007: queueData(code, tid, u32Array(handlesUnder(params[2]))); break; // GetObjectHandles
    case 0x1008: queueData(code, tid, objectInfoFor(params[0])); break;          // GetObjectInfo
    case 0x1009:                                                                 // GetObject
      if (DESYNC === "pull") return desyncedReplies(tid);
      queueData(code, tid, Buffer.from(objects[params[0]].bytes));
      break;
    case 0x100c:                                                                 // SendObjectInfo
      if (DESYNC === "send") { pending = { code, tid, params, desync: true }; return; }
      pending = { code, tid, params };
      return;
    case 0x100d: pending = { code, tid, params }; return;                        // SendObject
    default:
      replies.push(container(3, 0x2005, tid, [])); // operation not supported
      return;
  }
  replies.push(container(3, 0x2001, tid, []));
}

// desyncedReplies answers four containers carrying a transaction id that is
// not the one asked about — a late answer to a transaction that already timed
// out, which is exactly the ghost the page discards up to three of before
// conceding the session is lost.
function desyncedReplies(tid) {
  for (let i = 0; i < 4; i++) replies.push(container(3, 0x2001, tid + 1000 + i, []));
}

function handleHostData(payload) {
  const p = pending;
  pending = null;
  if (p.desync) return desyncedReplies(p.tid);
  if (p.code === 0x100c) {
    // The dataset names the file; keep the handle for the SendObject that
    // follows, exactly as a responder does.
    const dv = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
    const n = dv.getUint8(52);
    let name = "";
    for (let i = 0; i < n - 1; i++) name += String.fromCharCode(dv.getUint16(53 + i * 2, true));
    const h = nextHandle++;
    objects[h] = { name, format: FMT_UNDEFINED, parent: p.params[1], bytes: new Uint8Array(0) };
    written.push({ handle: h, name, bytes: null });
    replies.push(container(3, 0x2001, p.tid, [p.params[0], p.params[1], h]));
  } else {
    const last = written[written.length - 1];
    last.bytes = new Uint8Array(payload);
    objects[last.handle].bytes = last.bytes;
    replies.push(container(3, 0x2001, p.tid, []));
  }
}

const fakeDevice = {
  vendorId: 0x091e,
  configuration: {
    interfaces: [{
      interfaceNumber: 0,
      alternates: [{
        interfaceClass: 0xff,
        endpoints: [
          { type: "bulk", direction: "in", endpointNumber: 1, packetSize: 512 },
          { type: "bulk", direction: "out", endpointNumber: 2, packetSize: 512 },
        ],
      }],
    }],
  },
  open: async () => {},
  close: async () => {},
  selectConfiguration: async () => {},
  claimInterface: async () => {},
  releaseInterface: async () => {},
  transferOut: async (ep, bytes) => {
    const u8 = new Uint8Array(bytes);
    if (u8.length === 0) return { status: "ok" }; // the ZLP
    const dv = new DataView(u8.buffer, u8.byteOffset, u8.byteLength);
    if (pending && pending.awaitingPayload) {
      pending.awaitingPayload = false;
      handleHostData(u8);
      return { status: "ok" };
    }
    const type = dv.getUint16(4, true), code = dv.getUint16(6, true), tid = dv.getUint32(8, true);
    if (type === 1) {
      const params = [];
      for (let i = 12; i + 4 <= u8.length; i += 4) params.push(dv.getUint32(i, true));
      handleCommand(code, tid, params);
    } else if (type === 2) {
      // A data phase from the host. Under split framing the 12-byte header
      // arrives alone and the payload follows in its own transfer.
      if (u8.length === 12) pending.awaitingPayload = true;
      else handleHostData(u8.subarray(12));
    }
    return { status: "ok" };
  },
  transferIn: async (ep, len) => {
    const next = replies.shift();
    if (!next) throw new Error("fake watch: the page read with nothing queued");
    const slice = next.subarray(0, Math.min(len, next.length));
    if (slice.length < next.length) replies.unshift(next.subarray(slice.length));
    return { status: "ok", data: new DataView(slice.buffer, slice.byteOffset, slice.byteLength) };
  },
};

/* ── the DOM the page expects ────────────────────────────────────────── */

function El(tag) {
  return {
    tagName: tag, textContent: "", className: "", id: "", hidden: false,
    disabled: false, checked: false, indeterminate: false, tabIndex: 0,
    type: "", dataset: {}, children: [], attrs: {}, listeners: {},
    classList: { toggle() {}, add() {}, remove() {} },
    get firstChild() { return this.children[0] || null; },
    setAttribute(k, v) { this.attrs[k] = v; },
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); },
    dispatch(ev, arg) { (this.listeners[ev] || []).forEach((f) => f(arg || {})); },
    appendChild(c) { this.children.push(c); c.parent = this; return c; },
    removeChild(c) { this.children = this.children.filter((x) => x !== c); return c; },
    querySelector(sel) {
      const want = sel.replace(/^\./, "");
      const hit = (n) => (n.className || "").split(" ").indexOf(want) >= 0;
      const walk = (n) => {
        for (const c of n.children) { if (hit(c)) return c; const d = walk(c); if (d) return d; }
        return null;
      };
      return walk(this);
    },
    focus() {},
  };
}

const byId = {};
for (const id of ["wstatus", "wconnect", "wsend", "wpull", "wpullinfo", "wpullrows",
  "wpall", "wnewwrap", "wsaved", "wsavedsum", "wsavedrows", "wtab-send", "wtab-pull",
  "wpane-send", "wpane-pull", "wall", "wtrace"]) {
  byId[id] = El("div");
  byId[id].id = id;
}
// The initial states watch.html actually renders. Getting these wrong makes
// the driver race the page: a send button that starts enabled means "wait
// until connect finishes" is already true before connect has begun.
byId.wsend.disabled = true;
byId.wpull.disabled = true;
byId.wpall.disabled = true;
byId.wnewwrap.hidden = true;
byId.wsaved.hidden = true;
byId.wstatus.textContent = "Not connected.";
byId.wpullinfo.textContent = "Connect to list the watch's recorded activities.";
byId.wconnect.textContent = "Connect watch";

// Two workout rows, as the server renders them for a fortnight.
const workoutRows = [
  { slug: "W02 Tu Intervals.fit", url: "/fit/2026-08-18" },
  { slug: "W02 Sa Long run.fit", url: "/fit/2026-08-22" },
].map((w) => {
  const tr = El("tr");
  tr.dataset = { url: w.url, slug: w.slug };
  const box = El("input");
  box.type = "checkbox";
  box.checked = true;
  const st = El("span");
  st.className = "wstate";
  tr.appendChild(box);
  tr.appendChild(st);
  tr.querySelector = (sel) => (sel === "input" ? box : sel === ".wstate" ? st : null);
  return tr;
});

const document_ = {
  listeners: {},
  addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); },
  getElementById(id) { return byId[id] || null; },
  querySelector(sel) { return byId[sel.replace(/^#/, "")] || null; },
  querySelectorAll(sel) { return sel === "tr[data-url]" ? workoutRows : []; },
  createElement(tag) { return El(tag); },
};

const posts = [];
// The server's archive, statefully: the second recording is already saved, so
// the page must lock that row and pull only the first — the diff logic is
// exercised rather than bypassed — and an upload joins the listing, so the
// page's post-batch verify pass has something true to find.
const saved = { [objects[5].name]: objects[5].bytes.length };

async function fakeFetch(url, opts) {
  if (url === "/api/activities") {
    return {
      ok: true, status: 200,
      json: async () => Object.keys(saved).map((n) => ({ name: n, size: saved[n] })),
    };
  }
  if (url.indexOf("/api/activity?name=") === 0) {
    const bytes = new Uint8Array(opts.body);
    const name = decodeURIComponent(url.slice("/api/activity?name=".length).split("&")[0]);
    posts.push({ url, name, bytes });
    saved[name] = bytes.length;
    return { ok: true, status: 204 };
  }
  if (url.indexOf("/fit/") === 0) {
    return { ok: true, status: 200, arrayBuffer: async () => fitFile(0x33, 48).buffer };
  }
  throw new Error("fake fetch: unexpected " + url);
}

// Frozen clock: the trace stamps every line, and SendObjectInfo writes a
// modification date into the bytes. Without this, one file replayed twice
// would not match itself.
const FIXED = new Date("2026-08-16T12:00:00Z").getTime();
class FrozenDate extends Date {
  constructor(...args) { super(...(args.length ? args : [FIXED])); }
  static now() { return FIXED; }
}

const sandbox = {
  document: document_,
  navigator: { usb: { requestDevice: async () => fakeDevice, addEventListener() {}, removeEventListener() {} } },
  location: { hash: "" },
  fetch: fakeFetch,
  setTimeout, clearTimeout, console,
  Date: FrozenDate,
};
sandbox.window = sandbox;
sandbox.window.isSecureContext = true;
sandbox.window.addEventListener = () => {};
sandbox.window.history = { replaceState() {} };

vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(SRC, "utf8"), sandbox, { filename: SRC });

/* ── the replay ──────────────────────────────────────────────────────── */

const settle = () => new Promise((r) => setTimeout(r, 5));

// waitFor drains the microtask/timer queue until a condition holds. Every
// step of this page is a chain of awaits with no completion event to listen
// for, so the driver watches the same button states the athlete does.
async function waitFor(what, cond) {
  for (let i = 0; i < 400; i++) {
    if (cond()) return;
    await settle();
  }
  throw new Error("timed out waiting for " + what + " (status: " + byId.wstatus.textContent + ")");
}

(async () => {
  document_.listeners.DOMContentLoaded.forEach((f) => f());
  await settle();
  const out = [];
  const status = () => byId.wstatus.textContent;

  byId.wconnect.dispatch("click");
  await waitFor("connect to finish", () => !byId.wsend.disabled);
  out.push("after connect: " + status());
  out.push("  pull info:   " + byId.wpullinfo.textContent);
  out.push("  pull button: " + (byId.wpull.disabled ? "off" : "armed"));

  const stopped = () => status().indexOf("Stopped:") === 0;
  const rowStates = (tbody) => tbody.children
    .map((tr) => (tr.querySelector(".wstate") || { textContent: "?" }).textContent)
    .join(" | ");

  byId.wpull.dispatch("click");
  await waitFor("the pull to finish", () => stopped() || (posts.length > 0 && !byId.wsend.disabled));
  out.push("after pull:    " + status());
  out.push("  pull rows:   " + rowStates(byId.wpullrows));
  posts.forEach((p) => out.push("  POSTed " + p.url + "  " + p.bytes.length + " bytes  crc-ok=" +
    (crc16(Buffer.from(p.bytes.subarray(14, p.bytes.length - 2))) ===
      new DataView(p.bytes.buffer, p.bytes.byteOffset).getUint16(p.bytes.length - 2, true))));

  if (stopped()) {
    out.push("  (the batch stopped, so the send is not attempted — same as the athlete would see)");
  } else {
    byId.wsend.dispatch("click");
    await waitFor("the send to finish", () => stopped() ||
      (written.length === workoutRows.length && written[written.length - 1].bytes !== null &&
        byId.wconnect.textContent === "Connect watch"));
    out.push("after send:    " + status());
    out.push("  send rows:   " + workoutRows.map((tr) => tr.querySelector(".wstate").textContent).join(" | "));
  }
  written.forEach((w) => out.push("  wrote " + JSON.stringify(w.name) + "  " +
    (w.bytes ? w.bytes.length : 0) + " bytes"));

  // The trace is the wire: every transfer, hex-dumped, in order. Timestamps
  // are frozen, so this is the artefact two versions are compared on.
  console.log("=== framing: " + (SPLIT ? "split (Appendix H)" : "single container") + " ===");
  console.log(out.join("\n"));
  console.log("=== wire ===");
  console.log(byId.wtrace.textContent);
})().catch((e) => { console.error("replay failed: " + e.stack); process.exit(1); });
