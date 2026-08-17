/* The parts of a browser the watch page needs, faked well enough to run it.

   Shared by the two replays — webmtp_replay.js and webmsc_replay.js — because
   they differ only in which device they fake. The page, the DOM it talks to,
   the server it POSTs to and the clock are the same for both, and two copies
   of that would drift.

   Node stdlib only.
*/

"use strict";

const fs = require("fs");
const vm = require("vm");

/* ── FIT bytes ───────────────────────────────────────────────────────── */

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

// fitFile builds a well-framed FIT container — the same arithmetic the
// server's ingest gate checks — so what a fake device hands over is the shape
// of a real recording rather than arbitrary bytes.
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

// fitOK re-checks a pulled file the way the server would, so the report says
// whether what came off the device is what the archive would accept.
function fitOK(bytes) {
  if (bytes.length < 16 || Buffer.from(bytes.subarray(8, 12)).toString("latin1") !== ".FIT") return false;
  const hs = bytes[0];
  const dsize = Buffer.from(bytes.buffer, bytes.byteOffset).readUInt32LE(4);
  if (hs + dsize + 2 !== bytes.length) return false;
  const trailing = Buffer.from(bytes.buffer, bytes.byteOffset).readUInt16LE(bytes.length - 2);
  return crc16(Buffer.from(bytes.subarray(hs, bytes.length - 2))) === trailing ||
    crc16(Buffer.from(bytes.subarray(0, bytes.length - 2))) === trailing;
}

/* ── a very small XML parser ─────────────────────────────────────────── */

// Node has no DOMParser, and the manifest reader needs one. This implements
// exactly the two things that reader uses — getElementsByTagName and
// textContent — over a well-formed document, plus the parsererror element a
// browser inserts on malformed input, because the reader checks for it.
//
// It is deliberately not a general XML parser. Namespaces are ignored (the
// reader asks for local names), attributes are skipped, and entity handling
// covers only the five predefined ones. Anything it cannot parse becomes a
// parsererror, which is the same answer a browser gives.
function parseXML(text) {
  const root = { tagName: "#document", children: [], text: "" };
  const stack = [root];
  let i = 0, bad = "";

  const decode = (s) => s.replace(/&(lt|gt|amp|quot|apos|#\d+);/g, (m, e) =>
    ({ lt: "<", gt: ">", amp: "&", quot: '"', apos: "'" })[e] ||
    String.fromCharCode(parseInt(e.slice(1), 10)));

  while (i < text.length && !bad) {
    const lt = text.indexOf("<", i);
    if (lt < 0) break;
    if (lt > i) {
      const chunk = text.slice(i, lt);
      if (chunk.trim()) stack[stack.length - 1].text += decode(chunk);
    }
    if (text.startsWith("<!--", lt)) { i = text.indexOf("-->", lt) + 3; continue; }
    if (text.startsWith("<?", lt)) { i = text.indexOf("?>", lt) + 2; continue; }
    if (text.startsWith("<![CDATA[", lt)) {
      const end = text.indexOf("]]>", lt);
      stack[stack.length - 1].text += text.slice(lt + 9, end);
      i = end + 3;
      continue;
    }
    const gt = text.indexOf(">", lt);
    if (gt < 0) { bad = "unterminated tag"; break; }
    const raw = text.slice(lt + 1, gt).trim();
    i = gt + 1;
    if (raw.startsWith("/")) {
      const name = raw.slice(1).trim();
      const open = stack.pop();
      if (!open || open.tagName !== name) { bad = "mismatched </" + name + ">"; break; }
      continue;
    }
    const selfClosing = raw.endsWith("/");
    const name = raw.replace(/\/$/, "").split(/[\s]/)[0];
    if (!name) { bad = "empty tag"; break; }
    const node = { tagName: name, children: [], text: "" };
    stack[stack.length - 1].children.push(node);
    if (!selfClosing) stack.push(node);
  }
  if (!bad && stack.length !== 1) bad = "unclosed " + stack[stack.length - 1].tagName;

  const decorate = (n) => {
    Object.defineProperty(n, "textContent", {
      get() {
        return n.children.reduce((acc, c) => acc + c.textContent, n.text);
      },
    });
    n.getElementsByTagName = function (tag) {
      const out = [];
      const walk = (m) => m.children.forEach((c) => {
        // A browser matches on the LOCAL name for getElementsByTagName on an
        // XML document only when no namespace is in play; the manifest reader
        // asks for unprefixed names, so strip any prefix here.
        if (c.tagName === tag || c.tagName.split(":").pop() === tag) out.push(c);
        walk(c);
      });
      walk(n);
      return out;
    };
    n.children.forEach(decorate);
  };
  decorate(root);

  if (bad) {
    // What a browser does with malformed XML: a document whose content is a
    // parsererror element. The reader tests for exactly that.
    const err = { tagName: "parsererror", children: [], text: bad };
    const doc = { tagName: "#document", children: [err], text: "" };
    decorate(doc);
    return doc;
  }
  return root;
}

class DOMParser {
  parseFromString(text) { return parseXML(text); }
}

/* ── the DOM ─────────────────────────────────────────────────────────── */

function El(tag) {
  return {
    tagName: tag, textContent: "", className: "", id: "", hidden: false,
    disabled: false, checked: false, indeterminate: false, tabIndex: 0,
    type: "", dataset: {}, children: [], attrs: {}, listeners: {},
    classList: { toggle() {}, add() {}, remove() {} },
    get firstChild() { return this.children[0] || null; },
    get parentNode() { return this.parent || null; },
    setAttribute(k, v) { this.attrs[k] = v; },
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); },
    dispatch(ev, arg) { (this.listeners[ev] || []).forEach((f) => f(arg || {})); },
    // appendChild MOVES a node: the real DOM detaches it from wherever it was
    // first. Without that a row filed under "already saved" stayed in the new
    // table too, and `pullBody.children.length === 0` — which decides whether
    // the new-activities table is shown at all — could never become true.
    appendChild(c) {
      if (c.parent) c.parent.children = c.parent.children.filter((x) => x !== c);
      this.children.push(c);
      c.parent = this;
      return c;
    },
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

const PAGE_IDS = ["wstatus", "wconnect", "wsend", "wpull", "wpullinfo", "wpullrows",
  "wpall", "wnewwrap", "wsaved", "wsavedsum", "wsavedrows", "wtab-send", "wtab-pull",
  "wpane-send", "wpane-pull", "wall", "wtrace"];

// Frozen clock. The trace stamps every line with a time, and the MTP send
// writes a modification date into the bytes, so without this a single file
// replayed twice would not match itself.
const FIXED = new Date("2026-08-16T12:00:00Z").getTime();
class FrozenDate extends Date {
  constructor(...args) { super(...(args.length ? args : [FIXED])); }
  static now() { return FIXED; }
}

// newPage builds the DOM watch.html renders, in the state it renders it. The
// initial disabled flags matter: a send button that starts enabled means the
// driver's "wait until connect finishes" is already true before connect began.
function newPage(opts) {
  opts = opts || {};
  const byId = {};
  for (const id of PAGE_IDS) { byId[id] = El("div"); byId[id].id = id; }
  byId.wsend.disabled = true;
  byId.wpull.disabled = true;
  byId.wpall.disabled = true;
  byId.wnewwrap.hidden = true;
  byId.wsaved.hidden = true;
  byId.wstatus.textContent = "Not connected.";
  byId.wpullinfo.textContent = "Connect to list the watch's recorded activities.";
  byId.wtrace.textContent = "(nothing yet — connect first)";
  byId.wconnect.textContent = "Connect watch";

  // The connect button sits inside a <p class="wbtns">, which the chooser
  // replaces the contents of when there is more than one transport.
  const btnHost = El("p");
  btnHost.className = "wbtns";
  btnHost.appendChild(byId.wconnect);

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

  // The server, statefully. One recording is already saved, so the page must
  // lock that row and pull only the others — the diff logic is exercised
  // rather than bypassed — and an upload joins the listing, so the page's
  // post-batch verify pass has something true to find.
  const posts = [];
  const saved = Object.assign({}, opts.alreadySaved || {});
  async function fakeFetch(url, init) {
    if (url === "/api/activities") {
      return {
        ok: true, status: 200,
        json: async () => Object.keys(saved).map((n) => ({ name: n, size: saved[n] })),
      };
    }
    if (url.indexOf("/api/activity?name=") === 0) {
      const bytes = new Uint8Array(init.body);
      const name = decodeURIComponent(url.slice("/api/activity?name=".length).split("&")[0]);
      // The server would refuse a torn file; say so here rather than
      // pretending every upload lands.
      if (!fitOK(bytes)) return { ok: false, status: 400 };
      posts.push({ url, name, bytes, now: url.indexOf("now=1") > 0 });
      saved[name] = bytes.length;
      return { ok: true, status: 204 };
    }
    if (url.indexOf("/fit/") === 0) {
      return { ok: true, status: 200, arrayBuffer: async () => fitFile(0x33, 48).buffer };
    }
    throw new Error("fake fetch: unexpected " + url);
  }

  return { byId, btnHost, workoutRows, document: document_, posts, saved, fetch: fakeFetch };
}

// load runs the page's scripts in one context, with whatever device APIs the
// caller fakes. Script order is the template's: every file, then the page.
function load(page, navigatorExtras, sources) {
  const sandbox = {
    document: page.document,
    navigator: Object.assign({}, navigatorExtras),
    location: { hash: "" },
    fetch: page.fetch,
    setTimeout, clearTimeout, console,
    Date: FrozenDate,
    DOMParser,
  };
  sandbox.window = sandbox;
  sandbox.window.isSecureContext = true;
  sandbox.window.addEventListener = () => {};
  sandbox.window.history = { replaceState() {} };
  Object.assign(sandbox.window, navigatorExtras.windowExtras || {});
  vm.createContext(sandbox);
  for (const src of sources) {
    vm.runInContext(fs.readFileSync(src, "utf8"), sandbox, { filename: src });
  }
  return sandbox;
}

/* ── driving it ──────────────────────────────────────────────────────── */

const settle = () => new Promise((r) => setTimeout(r, 5));

// waitFor drains the timer queue until a condition holds. Every step of the
// page is a chain of awaits with no completion event to listen for, so the
// driver watches the same button states the athlete does.
async function waitFor(page, what, cond) {
  for (let i = 0; i < 400; i++) {
    if (cond()) return;
    await settle();
  }
  throw new Error("timed out waiting for " + what + " (status: " + page.byId.wstatus.textContent + ")");
}

module.exports = { crc16, fitFile, fitOK, El, newPage, load, settle, waitFor, FrozenDate, parseXML, DOMParser };
