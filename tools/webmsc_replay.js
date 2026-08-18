/* Drive the watch page against a FAKE mounted volume — the mass-storage half
   of what webmtp_replay.js does for MTP.

     node tools/webmsc_replay.js app/static/webmtp.js app/static/webmsc.js
     node tools/webmsc_replay.js … --case=garmin-picked
     node tools/webmsc_replay.js … --case=not-a-watch

   The transport is a PURE constructor over a FileSystemDirectoryHandle, which
   is the whole reason this is possible without a browser: a handle can come
   from the picker, from OPFS, or from the shim below, and the transport cannot
   tell. Nothing here opens a picker, and nothing here needs a gesture.

   WHAT THIS DOES NOT PROVE: that Chrome's File System Access API behaves the
   way this shim does. The shim is written to the spec, but a shim validating
   its own author's misreading is exactly the failure it cannot catch. That is
   what the volume itself is for.

   The fixtures are hand-written from the device XSD. The real
   GarminDevice.xml carries StoreKey and Modulus, and no copy of a real one
   belongs in this repo — so this one has neither, and the parser is shown
   walking past a placeholder for both.

   Node stdlib only.
*/

"use strict";

const H = require("./webwatch_harness.js");

const SRCS = process.argv.slice(2).filter((a) => a.indexOf("--") !== 0);
if (SRCS.length < 2) {
  console.error("usage: node tools/webmsc_replay.js <webmtp.js> <webmsc.js> [--case=…]");
  process.exit(2);
}
// Which volume the picker "returned". The three cases are the three things an
// athlete can actually hand this page.
const CASE = (process.argv.find((a) => a.indexOf("--case=") === 0) || "").split("=")[1] || "volume-root";
// --send drives the send half after connect: the page fetches each workout
// and the transport writes it into the manifest's InputToUnit inbox.
const SEND = process.argv.indexOf("--send") >= 0;
// --deny-write makes the readwrite upgrade fail, which must fail the batch
// plainly before anything is written.
const DENY = process.argv.indexOf("--deny-write") >= 0;
// --full-inbox preloads the inbox to the watch's 25-workout cap, so a new
// name must be refused where the watch would have silently deleted it.
const FULL = process.argv.indexOf("--full-inbox") >= 0;
// --gps drives the GPS-mode bridge: a fake 0x0003 device that accepts the
// mode-switch write, then the drive picker takes over. Proves switch → bridge
// → the same drive flow, headless.
const GPS = process.argv.indexOf("--gps") >= 0;
// --with-usb pretends this browser also has WebUSB, so the page offers TWO
// transports and has to draw a chooser instead of using the single button
// already in the markup. The USB side is rigged to throw: clicking the drive
// button must never reach it, and the chooser must never construct a transport
// just to read its name — doing that would open a directory picker at page
// load, because the registered mass-storage factory IS the picker call.
const WITH_USB = process.argv.indexOf("--with-usb") >= 0;

/* ── a directory tree, and the File System Access API over it ────────── */

// A synthetic manifest. The Path is written as the device writes it — from the
// volume root — and the two placeholder secrets are here to be walked past,
// not read: if either ever appears in the output, the promise that this file
// is parsed in the browser and never leaves has been broken.
const MANIFEST_XML = `<?xml version="1.0" encoding="UTF-8"?>
<Device xmlns="http://www.garmin.com/xmlschemas/GarminDevice/v2">
  <Model>
    <PartNumber>006-XXXXX-00</PartNumber>
    <SoftwareVersion>2100</SoftwareVersion>
    <Description>Forerunner (fixture)</Description>
  </Model>
  <Id>0000000000</Id>
  <Extensions>
    <StoreKey>PLACEHOLDER-STOREKEY-MUST-NOT-LEAVE-THE-BROWSER</StoreKey>
    <Modulus>PLACEHOLDER-MODULUS-MUST-NOT-LEAVE-THE-BROWSER</Modulus>
  </Extensions>
  <MassStorageMode>
    <DataType>
      <Name>FIT_TYPE_4</Name>
      <File>
        <TransferDirection>OutputFromUnit</TransferDirection>
        <Location>
          <FileExtension>FIT</FileExtension>
          <Path>GARMIN/ACTIVITY</Path>
        </Location>
      </File>
    </DataType>
    <DataType>
      <Name>FIT_TYPE_5</Name>
      <File>
        <TransferDirection>OutputFromUnit</TransferDirection>
        <Location><FileExtension>FIT</FileExtension><Path>GARMIN/WORKOUTS</Path></Location>
      </File>
      <File>
        <TransferDirection>InputToUnit</TransferDirection>
        <Location><FileExtension>FIT</FileExtension><Path>GARMIN/NEWFILES</Path></Location>
      </File>
    </DataType>
  </MassStorageMode>
</Device>
`;

// The volume, as a plain nested object: strings are files, objects are
// directories. INVENTED 8.3 names with no date in them, which is the point —
// no real device filename belongs in a public fixture.
const VOLUME = {
  ".fseventsd": { "fseventsd-uuid": "" },       // already on the volume, measured
  GARMIN: {
    "GarminDevice.xml": MANIFEST_XML,
    ACTIVITY: {
      "4T7B0091.FIT": H.fitFile(0x11, 96),
      "4T7B0092.FIT": H.fitFile(0x22, 64),
      "4T7B0093.FIT": H.fitFile(0x33, 80),
      "._4T7B0093.FIT": "AppleDouble junk",     // FAT + macOS, every time
      ".DS_Store": "junk",
      "NOTES.TXT": "not a recording",
    },
    WORKOUTS: { "87BC2939.FIT": H.fitFile(0x44, 32) },
    NEWFILES: {},
  },
};

// Junk the inbox census must ignore, and optionally a full inbox.
VOLUME.GARMIN.NEWFILES[".DS_Store"] = "junk";
VOLUME.GARMIN.NEWFILES["._W01.FIT"] = "junk";
if (FULL) {
  for (let i = 1; i <= 25; i++) VOLUME.GARMIN.NEWFILES[`OLD${String(i).padStart(2, "0")}.FIT`] = H.fitFile(i, 16);
}

const EDGE_MANIFEST = MANIFEST_XML
  .replace("Forerunner (fixture)", "Edge 520 Plus (fixture)")
  .replace("GARMIN/ACTIVITY", "Garmin/Activities")
  .replace(/GARMIN\/WORKOUTS/g, "Garmin/Workouts")
  .replace(/GARMIN\/NEWFILES/g, "Garmin/NewFiles");
const EDGE_VOLUME = {
  Garmin: {  // mixed case, exactly as the Edge writes it
    "GarminDevice.xml": EDGE_MANIFEST,
    Activities: {
      "2026-01-24-160153.fit": H.fitFile(0x71, 64),
      "2026-01-26-093012.fit": H.fitFile(0x72, 80),
    },
    Workouts: {},
    NewFiles: {},
  },
};

const NOT_A_WATCH = { Downloads: { "notes.txt": "not a recording" } };
// A non-Garmin drive: no GarminDevice.xml, FIT activities in a brand-specific
// folder (a Wahoo-style layout). The manifest-optional fallback must find
// them. Junk and a deep system folder are present to prove the scan skips them.
const GENERIC_DRIVE = {
  ".fseventsd": { "x": "" },
  "System Volume Information": { "deep": { "more": { "buried.fit": H.fitFile(0x60, 40) } } },
  Activities: {
    "2026-08-01-070000.fit": H.fitFile(0x61, 64),
    "2026-08-03-181500.fit": H.fitFile(0x62, 48),
    "._2026-08-03-181500.fit": "AppleDouble",
    "ride.gpx": "not fit",
  },
};

// A FileSystemDirectoryHandle over the object above, written to the spec:
// getDirectoryHandle/getFileHandle reject with NotFoundError, values() is an
// async iterator of handles, and a file handle yields a File with .size,
// .arrayBuffer() and .text().
function dirHandle(name, node) {
  return {
    kind: "directory",
    name,
    // The permission pair the readwrite upgrade calls. Granted unless the
    // run says otherwise; a real browser prompts here.
    async queryPermission() { return "prompt"; },
    async requestPermission(opts) {
      return DENY && opts && opts.mode === "readwrite" ? "denied" : "granted";
    },
    async getDirectoryHandle(child) {
      const v = node[child];
      if (!v || typeof v !== "object" || v instanceof Uint8Array) {
        const e = new Error("no such directory: " + child);
        e.name = "NotFoundError";
        throw e;
      }
      return dirHandle(child, v);
    },
    async getFileHandle(child, opts) {
      const v = node[child];
      if (v === undefined || (typeof v === "object" && !(v instanceof Uint8Array))) {
        if (opts && opts.create && v === undefined) {
          node[child] = new Uint8Array(0);
          return fileHandle(child, node[child], node);
        }
        const e = new Error("no such file: " + child);
        e.name = "NotFoundError";
        throw e;
      }
      return fileHandle(child, v, node);
    },
    values() {
      const names = Object.keys(node);
      let i = 0;
      const it = {
        next: async () => {
          if (i >= names.length) return { done: true, value: undefined };
          const n = names[i++];
          const v = node[n];
          const isDir = typeof v === "object" && !(v instanceof Uint8Array);
          return { done: false, value: isDir ? dirHandle(n, v) : fileHandle(n, v) };
        },
      };
      it[Symbol.asyncIterator] = () => it;
      return it;
    },
  };
}

function fileHandle(name, value, parent) {
  return {
    kind: "file",
    name,
    async getFile() {
      const v = parent ? parent[name] : value; // re-read: a write may have landed
      const bytes = v instanceof Uint8Array ? v : Buffer.from(String(v), "utf8");
      return {
        name,
        size: bytes.length,
        async arrayBuffer() {
          const copy = new Uint8Array(bytes.length);
          copy.set(bytes);
          return copy.buffer;
        },
        async text() { return Buffer.from(bytes).toString("utf8"); },
      };
    },
    // Chromium's crswap semantics, modelled: write() buffers beside the
    // target and only close() commits. A batch that skips close leaves the
    // volume exactly as it was — MTP's rollback by another mechanism, and
    // the reason the page counts a row sent only after close resolves.
    async createWritable() {
      let buf = new Uint8Array(0);
      return {
        async write(b) { buf = new Uint8Array(b); },
        async close() { if (parent) parent[name] = buf; },
      };
    },
  };
}

/* ── the replay ──────────────────────────────────────────────────────── */

// The picker's three plausible answers: the volume root, the GARMIN folder
// itself, and a folder that is not a watch at all.
const picked = {
  "volume-root": () => dirHandle("GARMIN", VOLUME),
  "garmin-picked": () => dirHandle("GARMIN", VOLUME.GARMIN),
  "not-a-watch": () => dirHandle("Home", NOT_A_WATCH),
  "generic-drive": () => dirHandle("WAHOO", GENERIC_DRIVE),
  "edge": () => dirHandle("GARMIN", EDGE_VOLUME),
}[CASE];
if (!picked) {
  console.error("unknown --case: " + CASE);
  process.exit(2);
}

// One recording already archived, so the page has a real diff to do rather
// than pulling everything blindly.
const page = H.newPage({ alreadySaved: { "4T7B0092.FIT": VOLUME.GARMIN.ACTIVITY["4T7B0092.FIT"].length } });
// A fake watch in GPS mode: vendor 0x091e, product 0x0003, one vendor
// interface with a bulk-OUT the mode-switch is written to. The write is
// recorded so the replay can assert the exact 13 bytes went out.
const gpsWrites = [];
const gpsDevice = {
  vendorId: 0x091e, productId: 0x0003, configuration: { interfaces: [{
    interfaceNumber: 0, alternates: [{ interfaceClass: 0xff, endpoints: [
      { type: "bulk", direction: "out", endpointNumber: 2, packetSize: 512 }] }] }] },
  open: async () => {}, close: async () => {}, selectConfiguration: async () => {},
  claimInterface: async () => {}, releaseInterface: async () => {},
  transferOut: async (ep, bytes) => { gpsWrites.push(Array.from(new Uint8Array(bytes))); return { status: "ok" }; },
};
H.load(page, {
  usb: GPS ? {
    requestDevice: async () => gpsDevice, addEventListener() {}, removeEventListener() {},
  } : WITH_USB ? {
    requestDevice: async () => { throw new Error("the USB transport was reached by mistake"); },
    addEventListener() {}, removeEventListener() {},
  } : undefined, // without it, one transport: the page keeps its single button
  windowExtras: { showDirectoryPicker: async () => picked() },
}, SRCS);

const byId = page.byId;

(async () => {
  page.document.listeners.DOMContentLoaded.forEach((f) => f());
  await H.settle();
  const out = [];
  const status = () => byId.wstatus.textContent;

  out.push("case:          " + CASE + (WITH_USB ? " (both transports offered)" : "") + (GPS ? " (GPS-mode bridge)" : ""));
  const buttons = page.btnHost.children;
  out.push("connect:       " + buttons.map((b) => JSON.stringify(b.textContent)).join(" "));

  // GPS-mode bridge: click its button, assert the switch packet, then the
  // button becomes the follow-up that opens the drive.
  if (GPS) {
    // The merged USB button detects the fake 0x0003 device and bridges.
    const gbtn = buttons.find((b) => b.textContent === "Connect over USB" || b.textContent === "Connect watch");
    if (!gbtn) throw new Error("no USB button to bridge through");
    gbtn.dispatch("click");
    await H.waitFor(page, "the switch", () => gpsWrites.length > 0 && gbtn.textContent === "Open the watch's drive");
    const pkt = gpsWrites[0] || [];
    const want = [0x14,0,0,0,0x2f,0x04,0,0,0x01,0,0,0,0];
    out.push("  mode-switch:  " + pkt.map((x) => x.toString(16).padStart(2,"0")).join(" ") +
      (JSON.stringify(pkt) === JSON.stringify(want) ? "  ✓ exact packet" : "  ✗ WRONG"));
    out.push("  follow-up:    " + JSON.stringify(gbtn.textContent));
    gbtn.dispatch("click"); // second gesture: open the drive
    await H.waitFor(page, "the drive", () => status().indexOf("Pulled") === 0 || byId.wpullrows.children.length > 0 || status().indexOf("new") > 0);
    out.push("after bridge:  " + status());
    if (!byId.wpull.disabled) {
      byId.wpull.dispatch("click");
      await H.waitFor(page, "the pull", () => status().indexOf("Pulled") === 0 || status().indexOf("failed") > 0);
      out.push("after pull:    " + status());
    }
    out.push("manifest leak: " + (byId.wtrace.textContent.indexOf("STOREKEY") >= 0 ? "FAILED" : "none"));
    console.log(out.join("\n"));
    return;
  }

  // Click the drive button, wherever the chooser put it.
  const driveBtn = buttons.length === 1 ? buttons[0]
    : buttons.find((b) => b.textContent === "Connect as a drive");
  if (!driveBtn) throw new Error("no drive button among " + buttons.length);
  driveBtn.dispatch("click");
  await H.waitFor(page, "connect to settle",
    () => status() !== "Not connected." && !driveBtn.disabled);
  if (WITH_USB) {
    out.push("  other button: " + buttons.filter((b) => b !== driveBtn)
      .map((b) => JSON.stringify(b.textContent) + (b.disabled ? " (dark)" : " (LIVE — two connections at once)")).join(" "));
  }
  out.push("after connect: " + status());
  out.push("  pull info:   " + byId.wpullinfo.textContent);
  out.push("  send button: " + (byId.wsend.disabled ? "off (this transport cannot send)" : "armed"));
  out.push("  pull button: " + (byId.wpull.disabled ? "off" : "armed"));

  if (!byId.wpull.disabled) {
    out.push("  listed:      " + byId.wpullrows.children
      .map((tr) => tr.querySelector(".wslug").textContent).join(" "));
    out.push("  filed saved: " + byId.wsavedrows.children
      .map((tr) => tr.querySelector(".wslug").textContent).join(" "));
    byId.wpull.dispatch("click");
    await H.waitFor(page, "the pull to finish", () => status().indexOf("Pulled") === 0 ||
      status().indexOf("Stopped:") === 0 || status().indexOf("failed") > 0);
    out.push("after pull:    " + status());
    page.posts.forEach((p) => out.push("  POSTed " + p.name + "  " + p.bytes.length +
      " bytes  well-framed=" + H.fitOK(p.bytes) + "  now=" + p.now));
  }

  if (SEND) {
    byId.wsend.dispatch("click");
    await H.waitFor(page, "the send to finish", () =>
      status().indexOf(" sent") > 0 || status().indexOf("not allowed") > 0 || status().indexOf("failed") > 0);
    out.push("after send:    " + status());
    out.push("  send rows:   " + page.workoutRows.map((tr) => tr.querySelector(".wstate").textContent).join(" | "));
    const inbox = VOLUME.GARMIN.NEWFILES;
    const files = Object.keys(inbox).filter((n) => n[0] !== "." && /\.fit$/i.test(n));
    out.push("  inbox now:   " + files.length + " file(s)" +
      (files.length <= 6 ? ": " + files.join(" ") : ""));
    for (const n of ["W02 Tu Intervals.fit", "W02 Sa Long run.fit"]) {
      if (inbox[n]) out.push("  " + JSON.stringify(n) + "  " + inbox[n].length + " bytes  well-framed=" + H.fitOK(inbox[n]));
    }
  }

  // The manifest carries keys. If any of it reached the page's transfer log,
  // it would reach a screenshot, and the whole promise would be worthless.
  const leak = ["PLACEHOLDER-STOREKEY", "PLACEHOLDER-MODULUS", "<Device"]
    .filter((s) => byId.wtrace.textContent.indexOf(s) >= 0 ||
      JSON.stringify(page.posts.map((p) => p.name)).indexOf(s) >= 0 ||
      status().indexOf(s) >= 0);
  out.push("manifest leak: " + (leak.length ? "FAILED — " + leak.join(", ") : "none, in the log or in what was sent"));

  console.log(out.join("\n"));
  console.log("=== wire ===");
  console.log(byId.wtrace.textContent);
  if (leak.length) process.exit(1);
})().catch((e) => { console.error("replay failed: " + e.stack); process.exit(1); });
