/* Drive static/webmtp.js against a FAKE watch and print every byte it puts on
   the wire.

   The page is a thousand lines of measured MTP truth that no test in this repo
   exercises, because exercising it has always meant plugging in a watch. That
   makes any refactor of it unfalsifiable: a diff can look right and still
   change the bytes. This replaces the watch with a responder that speaks the
   same container protocol, so the proof of a refactor becomes a byte-for-byte
   diff of two traces:

     node tools/webmtp_replay.js path/to/old/webmtp.js > old.txt
     node tools/webmtp_replay.js path/to/new/webmtp.js > new.txt
     diff old.txt new.txt

   or, against a git ref, `make watch-replay`.

   It is NOT a substitute for the watch. A fake responder proves that the code
   still sends what it used to send; only the Epix proves that what it sends is
   what the Epix accepts. Both are needed, and this one is the cheap half.

   The browser it runs in — the DOM, the server it POSTs to, the frozen clock —
   is webwatch_harness.js, shared with the mass-storage replay.

   Node stdlib only — no packages, same reason the FIT encoder has no library.
*/

"use strict";

const H = require("./webwatch_harness.js");

const SRC = process.argv[2];
if (!SRC) {
  console.error("usage: node tools/webmtp_replay.js <path-to-webmtp.js> [--split] [--desync=pull|send]");
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
const DESYNC = (process.argv.find((a) => a.indexOf("--desync=") === 0) || "").split("=")[1] || "";

/* ── the fake watch ──────────────────────────────────────────────────── */

const STORAGE = 0x10001;
const FMT_FOLDER = 0x3001, FMT_UNDEFINED = 0x3000;

// A tiny object tree, shaped exactly like the watch's: GARMIN with NewFiles
// and Activity under it, and two recordings to pull.
const objects = {
  1: { name: "GARMIN", format: FMT_FOLDER, parent: 0xffffffff, bytes: null },
  2: { name: "NewFiles", format: FMT_FOLDER, parent: 1, bytes: null },
  3: { name: "Activity", format: FMT_FOLDER, parent: 1, bytes: null },
  4: { name: "2026-08-15-06-30-00.fit", format: FMT_UNDEFINED, parent: 3, bytes: H.fitFile(0x11, 96) },
  5: { name: "2026-08-15-17-05-00.fit", format: FMT_UNDEFINED, parent: 3, bytes: H.fitFile(0x22, 64) },
};
let nextHandle = 6;

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

// desyncedReplies answers four containers carrying a transaction id that is
// not the one asked about — a late answer to a transaction that already timed
// out, which is exactly the ghost the page discards up to three of before
// conceding the session is lost.
function desyncedReplies(tid) {
  for (let i = 0; i < 4; i++) replies.push(container(3, 0x2001, tid + 1000 + i, []));
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

/* ── the replay ──────────────────────────────────────────────────────── */

// One recording is already on the server, so the page has a real diff to do.
const page = H.newPage({ alreadySaved: { [objects[5].name]: objects[5].bytes.length } });
H.load(page, { usb: { requestDevice: async () => fakeDevice, addEventListener() {}, removeEventListener() {} } }, [SRC]);

const byId = page.byId;

(async () => {
  page.document.listeners.DOMContentLoaded.forEach((f) => f());
  await H.settle();
  const out = [];
  const status = () => byId.wstatus.textContent;
  const stopped = () => status().indexOf("Stopped:") === 0;
  const rowStates = (tbody) => tbody.children
    .map((tr) => (tr.querySelector(".wstate") || { textContent: "?" }).textContent)
    .join(" | ");

  byId.wconnect.dispatch("click");
  await H.waitFor(page, "connect to finish", () => !byId.wsend.disabled);
  out.push("after connect: " + status());
  out.push("  pull info:   " + byId.wpullinfo.textContent);
  out.push("  pull button: " + (byId.wpull.disabled ? "off" : "armed"));

  byId.wpull.dispatch("click");
  await H.waitFor(page, "the pull to finish", () => stopped() || (page.posts.length > 0 && !byId.wsend.disabled));
  out.push("after pull:    " + status());
  out.push("  pull rows:   " + rowStates(byId.wpullrows));
  page.posts.forEach((p) => out.push("  POSTed " + p.url + "  " + p.bytes.length + " bytes  crc-ok=" + H.fitOK(p.bytes)));

  if (stopped()) {
    out.push("  (the batch stopped, so the send is not attempted — same as the athlete would see)");
  } else {
    byId.wsend.dispatch("click");
    await H.waitFor(page, "the send to finish", () => stopped() ||
      (written.length === page.workoutRows.length && written[written.length - 1].bytes !== null &&
        byId.wconnect.textContent === "Connect watch"));
    out.push("after send:    " + status());
    out.push("  send rows:   " + page.workoutRows.map((tr) => tr.querySelector(".wstate").textContent).join(" | "));
    written.forEach((w) => out.push("  wrote " + JSON.stringify(w.name) + "  " +
      (w.bytes ? w.bytes.length : 0) + " bytes"));
  }

  // The trace is the wire: every transfer, hex-dumped, in order. Timestamps
  // are frozen, so this is the artefact two versions are compared on.
  console.log("=== framing: " + (SPLIT ? "split (Appendix H)" : "single container") + " ===");
  console.log(out.join("\n"));
  console.log("=== wire ===");
  console.log(byId.wtrace.textContent);
})().catch((e) => { console.error("replay failed: " + e.stack); process.exit(1); });
