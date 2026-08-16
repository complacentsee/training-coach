/* Move files between the watch and the server over WebUSB — a hand-rolled MTP
   initiator, the same philosophy as the FIT encoder: the protocol subset we
   need is small and well-specified, so it is written here rather than
   imported.

   MTP is PTP (ISO 15740) with extensions: 12-byte little-endian containers
   over USB bulk endpoints. A transaction is command → optional data phase →
   response. This file implements exactly what the page needs — open a
   session, write workout files into GARMIN/NewFiles, read recorded
   activities out of GARMIN/Activity — and nothing destructive: it never
   deletes or renames anything.

   Platform truth this depends on: the watch's MTP interface is vendor-class
   0xFF (measured), which WebUSB may claim; macOS has no MTP kernel driver to
   compete with; Chromium implements WebUSB, Firefox and Safari do not; and
   WebUSB exists only in secure contexts, so this page works via HTTPS. */

(function () {
  "use strict";

  /* ── PTP/MTP constants ─────────────────────────────────────────────── */

  var GARMIN_VENDOR = 0x091e;

  // The wire trace: every transfer, hex-dumped, rendered on the page — the
  // instrument that turns "it hangs" into bytes we can argue about.
  var traceLines = [];
  function hex(u8, max) {
    var n = Math.min(u8.length, max || 64);
    var out = [];
    for (var i = 0; i < n; i++) out.push((u8[i] < 16 ? "0" : "") + u8[i].toString(16));
    return out.join(" ") + (u8.length > n ? " … (" + u8.length + "B)" : "");
  }
  function trace(line) {
    traceLines.push(new Date().toISOString().slice(11, 23) + "  " + line);
    if (traceLines.length > 200) traceLines.shift();
    var el = document.getElementById("wtrace");
    if (el) el.textContent = traceLines.join("\n");
  }

  var TYPE_COMMAND = 1, TYPE_DATA = 2, TYPE_RESPONSE = 3;

  var OP = {
    GetDeviceInfo: 0x1001,
    OpenSession: 0x1002,
    CloseSession: 0x1003,
    GetStorageIDs: 0x1004,
    GetObjectHandles: 0x1007,
    GetObjectInfo: 0x1008,
    GetObject: 0x1009,
    SendObjectInfo: 0x100c,
    SendObject: 0x100d,
    SendObjectPropList: 0x9808, // MTP-native creation — what modern responders actually test
  };

  var RSP_OK = 0x2001;
  var RSP_SESSION_ALREADY_OPEN = 0x201e;
  var RSP_NAMES = {
    0x2001: "OK", 0x2002: "general error", 0x2003: "session not open",
    0x2004: "invalid transaction id", 0x2005: "operation not supported",
    0x2006: "parameter not supported", 0x2007: "incomplete transfer",
    0x2008: "invalid storage id", 0x2009: "invalid object handle",
    0x200b: "invalid object format", 0x200c: "store full",
    0x200d: "object write protected", 0x200e: "store read-only",
    0x2013: "device busy", 0x201d: "invalid parameter",
    0x201e: "session already open",
  };

  var FMT_FOLDER = 0x3001; // PTP association = a directory
  var FMT_UNDEFINED = 0x3000; // an opaque binary object — what a .fit is to MTP

  var ALL_HANDLES = 0xffffffff; // GetObjectHandles: root / any format

  /* ── container encode/decode ───────────────────────────────────────── */

  function u16(v) { return [v & 0xff, (v >> 8) & 0xff]; }
  function u32(v) { return [v & 0xff, (v >> 8) & 0xff, (v >> 16) & 0xff, (v >>> 24) & 0xff]; }

  // container builds a command/data packet: length, type, code, transaction
  // id, then u32 params or raw payload bytes.
  function container(type, code, tid, params, payload) {
    var body = [];
    (params || []).forEach(function (p) { body = body.concat(u32(p)); });
    var len = 12 + body.length + (payload ? payload.byteLength : 0);
    var head = u32(len).concat(u16(type), u16(code), u32(tid), body);
    var out = new Uint8Array(len);
    out.set(head, 0);
    if (payload) out.set(new Uint8Array(payload), head.length);
    return out;
  }

  // ptpString encodes PTP's counted UTF-16LE string: a character count that
  // includes the terminator, then the characters. An empty string is the
  // single byte 0.
  function ptpString(s) {
    if (!s) return [0];
    var out = [s.length + 1];
    for (var i = 0; i < s.length; i++) out = out.concat(u16(s.charCodeAt(i)));
    return out.concat(u16(0));
  }

  function readPtpString(dv, at) {
    var n = dv.getUint8(at);
    var chars = [];
    for (var i = 0; i < n; i++) {
      var c = dv.getUint16(at + 1 + i * 2, true);
      if (c !== 0) chars.push(c);
    }
    return { value: String.fromCharCode.apply(null, chars), next: at + 1 + n * 2 };
  }

  // objectInfo is the dataset SendObjectInfo carries, byte-faithful to the
  // stack that provably writes to this watch (go-mtpx over the go-mtpfs
  // fork): REAL StorageID and ParentObject in the dataset — not the zeroed
  // libmtp spelling — and a ModificationDate, with CaptureDate and Keywords
  // empty. Thumbnail and image fields are zero — a .fit is not a picture,
  // whatever the protocol's ancestry says.
  function objectInfo(name, size, storageID, parent) {
    var d = new Date();
    var p2 = function (n) { return (n < 10 ? "0" : "") + n; };
    var stamp = "" + d.getFullYear() + p2(d.getMonth() + 1) + p2(d.getDate()) +
      "T" + p2(d.getHours()) + p2(d.getMinutes()) + p2(d.getSeconds());
    var b = [];
    b = b.concat(u32(storageID));
    b = b.concat(u16(FMT_UNDEFINED));
    b = b.concat(u16(0)); // protection: none
    b = b.concat(u32(size));
    b = b.concat(u16(0), u32(0), u32(0), u32(0)); // thumb format/size/w/h
    b = b.concat(u32(0), u32(0), u32(0)); // image w/h/depth
    b = b.concat(u32(parent));
    b = b.concat(u16(0), u32(0)); // association type/desc
    b = b.concat(u32(0)); // sequence number
    b = b.concat(ptpString(name));
    b = b.concat(ptpString("")); // capture date
    b = b.concat(ptpString(stamp)); // modification date
    b = b.concat(ptpString("")); // keywords
    return new Uint8Array(b).buffer;
  }

  // propList is SendObjectPropList's data phase: one property, the filename.
  // Format and size travel in the command params, so the minimal list is a
  // single ObjectFileName (0xDC07) string element describing handle 0.
  function propList(name) {
    var b = [];
    b = b.concat(u32(1)); // one element
    b = b.concat(u32(0)); // the object being created
    b = b.concat(u16(0xdc07)); // ObjectFileName
    b = b.concat(u16(0xffff)); // datatype: string
    b = b.concat(ptpString(name));
    return new Uint8Array(b).buffer;
  }

  // withTimeout turns a hung USB transfer into a diagnosis. A stalled MTP
  // responder simply never answers, and an await with no deadline is a page
  // stuck on "sending…" forever — the error must name the phase that died.
  // ms is per transfer and defaults to 15 s; big reads pass more headroom.
  function withTimeout(promise, what, ms) {
    ms = ms || 15000;
    return new Promise(function (resolve, reject) {
      var t = setTimeout(function () {
        reject(new Error(what + ": no answer from the watch after " + Math.round(ms / 1000) + " s — unplug, replug, reconnect"));
      }, ms);
      promise.then(
        function (v) { clearTimeout(t); resolve(v); },
        function (e) { clearTimeout(t); reject(e); }
      );
    });
  }

  /* ── the USB transport ─────────────────────────────────────────────── */

  function Mtp(device) {
    this.device = device;
    this.tid = 0;
    this.epIn = null;
    this.epOut = null;
    this.packetSize = 512;
    this.ifaceNum = null;
    this.separateHeader = false; // MTP 1.1 Appendix H framing, learned from the reads
  }

  Mtp.prototype.connect = async function () {
    var d = this.device;
    await d.open();
    if (d.configuration === null) await d.selectConfiguration(1);

    // The MTP interface is the one with bulk in + bulk out endpoints. On the
    // Epix it is vendor-class 0xFF; class 0x06 (still image) is the other
    // spelling MTP devices use, and neither is on WebUSB's protected list.
    var iface = null, alt = null;
    d.configuration.interfaces.forEach(function (i) {
      i.alternates.forEach(function (a) {
        var bulkIn = a.endpoints.find(function (e) { return e.type === "bulk" && e.direction === "in"; });
        var bulkOut = a.endpoints.find(function (e) { return e.type === "bulk" && e.direction === "out"; });
        if (bulkIn && bulkOut && !iface && (a.interfaceClass === 0xff || a.interfaceClass === 0x06)) {
          iface = i; alt = a;
        }
      });
    });
    if (!iface) throw new Error("no MTP interface (bulk in+out, class 0xFF/0x06) on this device");
    this.ifaceNum = iface.interfaceNumber;
    await d.claimInterface(this.ifaceNum);

    var bulkIn = alt.endpoints.find(function (e) { return e.type === "bulk" && e.direction === "in"; });
    var bulkOut = alt.endpoints.find(function (e) { return e.type === "bulk" && e.direction === "out"; });
    this.epIn = bulkIn.endpointNumber;
    this.epOut = bulkOut.endpointNumber;
    this.packetSize = bulkOut.packetSize || 512;
  };

  Mtp.prototype.close = async function () {
    try { await this.device.releaseInterface(this.ifaceNum); } catch (e) { /* already gone */ }
    try { await this.device.close(); } catch (e) { /* already gone */ }
  };

  Mtp.prototype.send = async function (bytes, what) {
    trace("OUT " + (what || "") + "  " + hex(bytes));
    var r = await withTimeout(this.device.transferOut(this.epOut, bytes), what || "USB write");
    if (r.status !== "ok") throw new Error((what || "USB write") + ": transfer " + r.status);
  };

  // readContainer assembles one complete container, however many USB
  // transfers it arrives in. ms, when set, is the per-transfer timeout.
  Mtp.prototype.readContainer = async function (what, ms) {
    var first = await withTimeout(this.device.transferIn(this.epIn, 4096), what || "USB read", ms);
    if (first.status !== "ok") throw new Error((what || "USB read") + ": transfer " + first.status);
    var dv = first.data;
    var total = dv.getUint32(0, true);
    var got = new Uint8Array(total);
    var have = Math.min(dv.byteLength, total);
    got.set(new Uint8Array(dv.buffer, dv.byteOffset, have), 0);
    while (have < total) {
      var more = await withTimeout(this.device.transferIn(this.epIn, Math.min(1 << 20, total - have + 4096)), what || "USB read", ms);
      if (more.status !== "ok") throw new Error((what || "USB read") + ": transfer " + more.status);
      var chunk = new Uint8Array(more.data.buffer, more.data.byteOffset, more.data.byteLength);
      got.set(chunk.subarray(0, Math.min(chunk.length, total - have)), have);
      have += chunk.length;
    }
    // MTP 1.1 Appendix H: a device may send a data phase as a lone 12-byte
    // header packet with the payload following separately — and once it
    // does, BOTH parties must frame that way. Detect it from the read side
    // (exactly as the working stack does) and mirror it on writes.
    if (!this.separateHeader && dv.byteLength === 12 && total > 12) {
      this.separateHeader = true;
      trace("device splits data-phase headers (Appendix H) — mirroring on writes");
    }
    trace(" IN " + (what || "") + "  " + hex(got));
    var head = new DataView(got.buffer, 0, 12);
    return {
      type: head.getUint16(4, true),
      code: head.getUint16(6, true),
      tid: head.getUint32(8, true),
      payload: new DataView(got.buffer, 12),
    };
  };

  // transact runs one MTP transaction. dataOut sends a data phase; expectData
  // reads one. ms, when set, extends the per-transfer read timeout (only the
  // GetObject path passes it). Returns {code, params, data}.
  Mtp.prototype.transact = async function (op, params, dataOut, expectData, what, ms) {
    what = what || ("op 0x" + op.toString(16));
    var tid = this.tid++;
    await this.send(container(TYPE_COMMAND, op, tid, params || []), what + " (command)");
    if (dataOut) {
      var dc = container(TYPE_DATA, op, tid, [], dataOut);
      if (this.separateHeader) {
        // Split framing: the 12-byte header as its own transfer, then the
        // payload, no trailing ZLP — byte-for-byte what the working stack
        // sends to a device that splits.
        await this.send(dc.subarray(0, 12), what + " (data header)");
        await this.send(dc.subarray(12), what + " (data payload)");
      } else {
        await this.send(dc, what + " (data out)");
        // The working stack's remainder loop leaves a quirk: any data phase
        // that fits one max packet is followed by an unconditional ZLP. The
        // reference bytes include it, so ours do too.
        if (dc.byteLength <= this.packetSize) {
          await this.send(new Uint8Array(0), what + " (data ZLP)");
        }
      }
    }

    var data = null;
    // A timed-out transaction can leave its late response in the pipe; a
    // container with an older transaction id is that ghost, not our answer.
    // Discard up to three before conceding the session is desynced.
    var c, stale = 0;
    for (;;) {
      c = await this.readContainer(what + " (response)", ms);
      if (c.tid === tid) break;
      if (++stale > 3) throw new Error(what + ": session desynced (stale responses) — unplug, replug, reconnect");
    }
    if (c.type === TYPE_DATA) {
      data = c.payload;
      for (;;) {
        c = await this.readContainer(what + " (response)", ms);
        if (c.tid === tid) break;
        if (++stale > 3) throw new Error(what + ": session desynced (stale responses) — unplug, replug, reconnect");
      }
    } else if (expectData && c.type === TYPE_RESPONSE) {
      // A responder may legally skip the data phase on error; fall through.
    }
    if (c.type !== TYPE_RESPONSE) throw new Error("expected a response container, got type " + c.type);
    var rparams = [];
    for (var i = 0; i + 4 <= c.payload.byteLength; i += 4) {
      rparams.push(c.payload.getUint32(i, true));
    }
    return { code: c.code, params: rparams, data: data };
  };

  Mtp.prototype.ok = async function (what, op, params, dataOut, expectData, ms) {
    var r = await this.transact(op, params, dataOut, expectData, what, ms);
    if (r.code !== RSP_OK) {
      var name = RSP_NAMES[r.code] || ("0x" + r.code.toString(16));
      throw new Error(what + ": " + name);
    }
    return r;
  };

  /* ── the operations the page needs ─────────────────────────────────── */

  Mtp.prototype.openSession = async function () {
    var r = await this.transact(OP.OpenSession, [1]);
    if (r.code === RSP_SESSION_ALREADY_OPEN) {
      await this.transact(OP.CloseSession, []);
      this.tid = 0;
      r = await this.transact(OP.OpenSession, [1]);
    }
    if (r.code !== RSP_OK) throw new Error("OpenSession: " + (RSP_NAMES[r.code] || r.code));
  };

  Mtp.prototype.deviceInfo = async function () {
    var r = await this.ok("GetDeviceInfo", OP.GetDeviceInfo, [], null, true);
    // Walk the DeviceInfo dataset. The first counted array is
    // OperationsSupported — the watch's own statement of which write flow
    // it implements — and the rest can be skipped.
    var dv = r.data, at = 0;
    at += 2 + 4 + 2; // standard version, vendor extension id, extension version
    at = readPtpString(dv, at).next; // extension description
    at += 2; // functional mode
    var nOps = dv.getUint32(at, true);
    at += 4;
    this.ops = new Set();
    for (var i = 0; i < nOps; i++) this.ops.add(dv.getUint16(at + i * 2, true));
    at += nOps * 2;
    for (var a = 0; a < 4; a++) { // events, properties, capture fmts, image fmts
      var n = dv.getUint32(at, true);
      at += 4 + n * 2;
    }
    var man = readPtpString(dv, at);
    var model = readPtpString(dv, man.next);
    return { manufacturer: man.value, model: model.value };
  };

  Mtp.prototype.storageIDs = async function () {
    var r = await this.ok("GetStorageIDs", OP.GetStorageIDs, [], null, true);
    var n = r.data.getUint32(0, true);
    var out = [];
    for (var i = 0; i < n; i++) out.push(r.data.getUint32(4 + i * 4, true));
    return out;
  };

  Mtp.prototype.handlesUnder = async function (storage, parent) {
    var r = await this.ok("GetObjectHandles", OP.GetObjectHandles, [storage, 0, parent], null, true);
    var n = r.data.getUint32(0, true);
    var out = [];
    for (var i = 0; i < n; i++) out.push(r.data.getUint32(4 + i * 4, true));
    return out;
  };

  Mtp.prototype.objectNameFormat = async function (handle) {
    var r = await this.ok("GetObjectInfo", OP.GetObjectInfo, [handle], null, true);
    var dv = r.data;
    // Fixed-size head of the ObjectInfo dataset, then the filename string.
    var format = dv.getUint16(4, true);
    var size = dv.getUint32(8, true); // ObjectCompressedSize
    var name = readPtpString(dv, 52).value;
    return { name: name, format: format, size: size };
  };

  // getObject reads one object's bytes. size is the listed ObjectCompressedSize
  // and buys per-transfer timeout headroom (~100 KB/s, capped at 120 s).
  Mtp.prototype.getObject = async function (handle, size) {
    var ms = Math.min(120000, 15000 + Math.ceil((size || 0) / 100));
    var r = await this.ok("GetObject", OP.GetObject, [handle], null, true, ms);
    if (!r.data) throw new Error("GetObject: the watch sent no data phase");
    // r.data views past the container header, but its .buffer still holds the
    // 12 header bytes — slice by byteOffset+byteLength, never take .buffer whole.
    return new Uint8Array(r.data.buffer, r.data.byteOffset, r.data.byteLength);
  };

  // findFolder locates a child folder by name (case-insensitive) under a
  // parent, without touching anything else.
  Mtp.prototype.findFolder = async function (storage, parent, name) {
    var handles = await this.handlesUnder(storage, parent);
    for (var i = 0; i < handles.length; i++) {
      var info = await this.objectNameFormat(handles[i]);
      if (info.format === FMT_FOLDER && info.name.toLowerCase() === name.toLowerCase()) {
        return handles[i];
      }
    }
    return 0;
  };

  Mtp.prototype.sendFile = async function (storage, parent, name, bytes) {
    // The stack that provably writes to this watch uses plain
    // SendObjectInfo with the dataset carrying the real destination — so
    // that is the lead attempt, in its exact framing. Prop-list survives
    // only as a last resort for a hard rejection.
    var r = await this.transact(OP.SendObjectInfo, [storage, parent],
      objectInfo(name, bytes.byteLength, storage, parent), false, "SendObjectInfo");
    if (r.code !== RSP_OK) {
      trace("SendObjectInfo rejected: 0x" + r.code.toString(16) + " — trying SendObjectPropList");
      r = await this.transact(OP.SendObjectPropList,
        [storage, parent, FMT_UNDEFINED, bytes.byteLength, 0], propList(name),
        false, "SendObjectPropList");
    }
    if (r.code !== RSP_OK) {
      var n = RSP_NAMES[r.code] || ("0x" + r.code.toString(16));
      throw new Error("SendObjectInfo: " + n);
    }
    await this.ok("SendObject", OP.SendObject, [], bytes);
  };

  /* ── the page ──────────────────────────────────────────────────────── */

  var $ = function (sel) { return document.querySelector(sel); };
  var statusEl, connectBtn, sendBtn, pullBtn, pullInfo, pullBody, wpall, rows;
  var newWrap, savedBox, savedSum, savedBody;

  function say(msg, isErr) {
    statusEl.textContent = msg;
    statusEl.classList.toggle("err", !!isErr);
  }

  function rowState(row, cls, text) {
    var el = row.querySelector(".wstate");
    el.className = "wstate " + cls;
    el.textContent = text;
  }

  var mtp = null, storage = 0, newFiles = 0;

  // The one button is a toggle and names the action it will take next.
  function setConnectUI(connected) {
    connectBtn.textContent = connected ? "Disconnect" : "Connect watch";
  }

  // disconnect releases the watch deliberately: session closed, USB claim
  // freed — so other MTP tools can reach the device without this tab closing.
  async function disconnect() {
    connectBtn.disabled = true;
    sendBtn.disabled = true;
    pullBtn.disabled = true;
    try { await mtp.transact(OP.CloseSession, [], null, false, "CloseSession"); } catch (e) { /* a dead session has nothing left to close */ }
    await mtp.close();
    mtp = null;
    setConnectUI(false);
    connectBtn.disabled = false;
    say("Disconnected. Unplug when ready, or connect again to transfer.");
  }
  var pullRows = []; // {tr, box, act:{handle,name,size}} — rebuilt on every connect

  function humanSize(n) {
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return Math.round(n / 1024) + " KB";
    return (n / (1024 * 1024)).toFixed(1) + " MB";
  }

  async function fetchStored() {
    var resp = await fetch("/api/activities");
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    return resp.json();
  }

  // buildPullRows renders the watch's activity list. Everything device-named
  // goes through textContent — never markup.
  function buildPullRows(acts) {
    pullRows = [];
    while (pullBody.firstChild) pullBody.removeChild(pullBody.firstChild);
    while (savedBody.firstChild) savedBody.removeChild(savedBody.firstChild);
    savedBox.hidden = true;
    newWrap.hidden = acts.length === 0;
    acts.forEach(function (act) {
      var tr = document.createElement("tr");
      var tdBox = document.createElement("td");
      var box = document.createElement("input");
      box.type = "checkbox";
      box.checked = true; // refreshPull unchecks and locks the already-stored ones
      box.setAttribute("aria-label", "pull " + act.name);
      box.addEventListener("change", syncPall);
      tdBox.appendChild(box);
      var tdName = document.createElement("td");
      tdName.className = "wslug";
      tdName.textContent = act.name;
      var tdSize = document.createElement("td");
      tdSize.className = "wday";
      tdSize.textContent = humanSize(act.size);
      var tdState = document.createElement("td");
      var state = document.createElement("span");
      state.className = "wstate";
      tdState.appendChild(state);
      tr.appendChild(tdBox);
      tr.appendChild(tdName);
      tr.appendChild(tdSize);
      tr.appendChild(tdState);
      pullBody.appendChild(tr);
      pullRows.push({ tr: tr, box: box, act: act });
    });
    syncPall();
  }

  // syncPall mirrors the send list's master-checkbox logic over the rows that
  // can still be pulled.
  function syncPall() {
    if (!wpall) return;
    var live = pullRows.filter(function (pr) { return !pr.box.disabled; });
    var on = live.filter(function (pr) { return pr.box.checked; }).length;
    wpall.disabled = live.length === 0;
    wpall.checked = live.length > 0 && on === live.length;
    wpall.indeterminate = on > 0 && on < live.length;
  }

  // refreshPull re-marks every row against the saved list: saved rows lock,
  // new rows stay selectable, and the diff line + pull button follow. Rows
  // the pull loop just labelled ("saved", a failure) keep their words — only
  // untouched rows get the listing labels, so a fresh pull never re-reads
  // as "already saved".
  function refreshPull(stored) {
    var sizes = {};
    stored.forEach(function (a) { sizes[a.name] = a.size; });
    var fresh = 0;
    pullRows.forEach(function (pr) {
      var st = pr.tr.querySelector(".wstate");
      var untouched = st.textContent === "" || st.textContent === "new file";
      if (Object.prototype.hasOwnProperty.call(sizes, pr.act.name)) {
        pr.box.checked = false;
        pr.box.disabled = true;
        if (!untouched) return;
        if (sizes[pr.act.name] === pr.act.size) {
          rowState(pr.tr, "sent", "already saved");
          // Filed under the collapsed section — checkbox cell dropped, the
          // row can no longer be acted on.
          pr.tr.removeChild(pr.tr.firstChild);
          savedBody.appendChild(pr.tr);
        } else {
          rowState(pr.tr, "fail", "differs from the saved copy");
        }
      } else {
        fresh++;
        if (untouched) rowState(pr.tr, "", "new file");
      }
    });
    var savedN = savedBody.children.length;
    savedSum.textContent = savedN + " already saved";
    savedBox.hidden = savedN === 0;
    newWrap.hidden = pullBody.children.length === 0;
    pullInfo.textContent = fresh ? fresh + " new on the watch." : "Nothing new on the watch.";
    syncPall();
    pullBtn.disabled = !mtp || fresh === 0;
    return fresh;
  }

  async function connect() {
    connectBtn.disabled = true;
    try {
      var device = await navigator.usb.requestDevice({ filters: [{ vendorId: GARMIN_VENDOR }] });
      mtp = new Mtp(device);
      await mtp.connect();
      await mtp.openSession();
      var info = await mtp.deviceInfo();
      var stores = await mtp.storageIDs();
      if (!stores.length) throw new Error("the watch reports no storage");
      storage = stores[0];
      trace("ops supported: " + Array.from(mtp.ops || []).map(function (o) { return "0x" + o.toString(16); }).sort().join(" "));
      var garmin = await mtp.findFolder(storage, ALL_HANDLES, "GARMIN");
      if (!garmin) throw new Error("no GARMIN folder on the watch's storage");
      newFiles = await mtp.findFolder(storage, garmin, "NewFiles");
      if (!newFiles) throw new Error("no GARMIN/NewFiles folder — is this the right device mode?");
      var senderLine = "Connected: " + info.manufacturer + " " + info.model + " — GARMIN/NewFiles found. Writes via " +
        (mtp.ops && mtp.ops.has(OP.SendObjectPropList) ? "SendObjectPropList." : "SendObjectInfo.");
      say(senderLine);
      // Send stays dark until the pull listing is done: two live buttons
      // would interleave two transaction streams on one session.
      await setupPull(garmin, senderLine);
      sendBtn.disabled = false;
      setConnectUI(true);
      connectBtn.disabled = false;
    } catch (e) {
      say(e.message, true);
      if (mtp) { await mtp.close(); mtp = null; }
      setConnectUI(false);
      connectBtn.disabled = false;
      sendBtn.disabled = true;
      pullBtn.disabled = true;
    }
  }

  // setupPull lists GARMIN/Activity once per connect, diffs it against the
  // server, and arms the pull card. Read-only on the watch.
  async function setupPull(garmin, senderLine) {
    buildPullRows([]);
    pullBtn.disabled = true;
    var acts = [];
    try {
      var activity = await mtp.findFolder(storage, garmin, "Activity");
      if (!activity) {
        pullInfo.textContent = "No Activity folder on this watch.";
        return;
      }
      var handles = await mtp.handlesUnder(storage, activity);
      for (var i = 0; i < handles.length; i++) {
        var info = await mtp.objectNameFormat(handles[i]);
        if (info.format === FMT_FOLDER) continue;
        acts.push({ handle: handles[i], name: info.name, size: info.size });
      }
    } catch (e) {
      pullInfo.textContent = "Could not list GARMIN/Activity (" + e.message + ") — reconnect to pull.";
      return;
    }
    acts.sort(function (a, b) { return a.name < b.name ? 1 : a.name > b.name ? -1 : 0; }); // names are timestamps: newest first
    buildPullRows(acts);
    var stored;
    try {
      stored = await fetchStored();
    } catch (e) {
      pullRows.forEach(function (pr) { pr.box.checked = false; pr.box.disabled = true; });
      syncPall();
      pullInfo.textContent = "Could not read the list of saved activities (" + e.message + ") — pull stays off; reconnect to retry.";
      return;
    }
    var fresh = refreshPull(stored);
    say(senderLine + " " + (fresh ? fresh + " new activit" + (fresh === 1 ? "y" : "ies") + " on the watch." : "Nothing new on the watch."));
  }

  async function pullAll() {
    pullBtn.disabled = true;
    sendBtn.disabled = true;
    connectBtn.disabled = true; // no disconnecting mid-transfer
    var pulled = 0, failed = 0, acked = [];
    /* The server sees one upload at a time and cannot tell a finished
       training day from a pause between files, so it waits before grading.
       This page DOES know: it is sending a known list, in order. Mark the
       last file of each day and that day is graded the moment it lands. */
    var lastOfDay = {};
    for (var k = 0; k < pullRows.length; k++) {
      if (pullRows[k].box.disabled || !pullRows[k].box.checked) continue;
      lastOfDay[(pullRows[k].act.name || "").slice(0, 10)] = k;
    }
    for (var i = 0; i < pullRows.length; i++) {
      var pr = pullRows[i];
      if (pr.box.disabled || !pr.box.checked) continue;
      if (!mtp) {
        rowState(pr.tr, "fail", "watch disconnected");
        failed++;
        continue;
      }
      rowState(pr.tr, "busy", "pulling…");
      try {
        var bytes = await mtp.getObject(pr.act.handle, pr.act.size);
        if (bytes.length !== pr.act.size) {
          rowState(pr.tr, "fail", "size changed on the watch — reconnect");
          failed++;
          continue;
        }
        var complete = lastOfDay[(pr.act.name || "").slice(0, 10)] === i;
        var resp = await fetch("/api/activity?name=" + encodeURIComponent(pr.act.name) +
          (complete ? "&now=1" : ""), { method: "POST", body: bytes });
        if (resp.status === 204) {
          rowState(pr.tr, "sent", "saved");
          pulled++;
          acked.push({ pr: pr, counted: true });
        } else if (resp.status === 409) {
          rowState(pr.tr, "sent", "already saved");
          acked.push({ pr: pr, counted: false });
        } else {
          rowState(pr.tr, "fail", "save failed (" + resp.status + ")");
          failed++;
        }
      } catch (e) {
        rowState(pr.tr, "fail", e.message);
        failed++;
        if (e.message.indexOf("no answer") >= 0 || e.message.indexOf("desynced") >= 0) {
          say("Stopped: the watch stopped answering. Unplug, replug, reconnect, then pull the remaining files.", true);
          for (var j = i + 1; j < pullRows.length; j++) {
            if (!pullRows[j].box.disabled && pullRows[j].box.checked) rowState(pullRows[j].tr, "fail", "not attempted");
          }
          connectBtn.disabled = false; // Disconnect still frees the claim
          return;
        }
      }
    }
    // Reads roll back nothing, so the session stays open here — only writes
    // need CloseSession before unplug, and the send path owns that.
    if (mtp) { sendBtn.disabled = false; connectBtn.disabled = false; }
    var stored;
    try {
      stored = await fetchStored();
    } catch (e) {
      say(pulled + " acknowledged — but re-reading the saved list failed (" + e.message + "), so nothing is verified. Reconnect to retry.", true);
      return;
    }
    var names = {};
    stored.forEach(function (a) { names[a.name] = true; });
    acked.forEach(function (a) {
      if (!names[a.pr.act.name]) {
        rowState(a.pr.tr, "fail", "not saved — retry");
        if (a.counted) pulled--;
        failed++;
      }
    });
    refreshPull(stored);
    if (failed) {
      say(pulled + " pulled, " + failed + " failed — retry after reconnecting.", true);
    } else {
      say("Pulled " + pulled + " — " + stored.length + " activit" + (stored.length === 1 ? "y" : "ies") + " saved in total. Send workouts next, or unplug.");
    }
  }

  async function sendAll() {
    sendBtn.disabled = true;
    pullBtn.disabled = true; // one MTP transaction at a time; connect re-arms pull
    connectBtn.disabled = true; // no disconnecting mid-transfer
    var sent = 0, failed = 0;
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      if (!row.querySelector("input").checked) continue;
      // The disconnect listener nulls mtp the moment the watch goes away;
      // fail the remaining rows plainly instead of racing dead transfers.
      if (!mtp) {
        rowState(row, "fail", "watch disconnected");
        failed++;
        continue;
      }
      rowState(row, "busy", "sending…");
      try {
        var resp = await fetch(row.dataset.url);
        if (!resp.ok) throw new Error("download failed (" + resp.status + ")");
        var bytes = await resp.arrayBuffer();
        await mtp.sendFile(storage, newFiles, row.dataset.slug, bytes);
        rowState(row, "sent", "on the watch");
        sent++;
      } catch (e) {
        rowState(row, "fail", e.message);
        failed++;
        if (e.message.indexOf("no answer") >= 0 || e.message.indexOf("desynced") >= 0) {
          say("Stopped: the watch stopped answering. Unplug, replug, reconnect, then send the remaining files.", true);
          for (var j = i + 1; j < rows.length; j++) {
            if (rows[j].querySelector("input").checked) rowState(rows[j], "fail", "not attempted");
          }
          connectBtn.disabled = false; // Disconnect still frees the claim
          return;
        }
      }
    }
    if (mtp) {
      // The count shown must be the device's own, and the session must be
      // closed before the cable moves — a responder may roll back objects
      // from a session that dies uncleanly.
      try {
        var listed = (await mtp.handlesUnder(storage, newFiles)).length;
        await mtp.transact(OP.CloseSession, [], null, false, "CloseSession");
        await mtp.close();
        mtp = null;
        setConnectUI(false);
        connectBtn.disabled = false;
        say(sent + " sent" + (failed ? ", " + failed + " failed" : "") +
          " — the watch itself lists " + listed + " file(s) in NewFiles. Session closed: unplug now, " +
          "and the workouts appear under Training → Workouts.");
      } catch (e) {
        say("Sent " + sent + ", but the closing handshake failed: " + e.message, true);
        sendBtn.disabled = false;
        connectBtn.disabled = false;
      }
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    statusEl = $("#wstatus");
    connectBtn = $("#wconnect");
    sendBtn = $("#wsend");
    pullBtn = $("#wpull");
    pullInfo = $("#wpullinfo");
    pullBody = $("#wpullrows");
    wpall = document.getElementById("wpall");
    newWrap = $("#wnewwrap");
    savedBox = $("#wsaved");
    savedSum = $("#wsavedsum");
    savedBody = $("#wsavedrows");
    rows = Array.prototype.slice.call(document.querySelectorAll("tr[data-url]"));

    // The two panes share one connection — a tab switch only swaps
    // visibility, so it never drops the USB claim or the MTP session.
    // Wired before the capability checks: the tabs must work in any browser.
    var tabSend = $("#wtab-send"), tabPull = $("#wtab-pull");
    var paneSend = $("#wpane-send"), panePull = $("#wpane-pull");
    function showTab(pull) {
      tabSend.className = pull ? "wtab" : "wtab on";
      tabPull.className = pull ? "wtab on" : "wtab";
      tabSend.setAttribute("aria-selected", pull ? "false" : "true");
      tabPull.setAttribute("aria-selected", pull ? "true" : "false");
      // Roving tabindex: role=tab promises one tab stop and arrow keys.
      tabSend.tabIndex = pull ? -1 : 0;
      tabPull.tabIndex = pull ? 0 : -1;
      paneSend.hidden = pull;
      panePull.hidden = !pull;
      if (window.history && history.replaceState) {
        history.replaceState(null, "", pull ? "#pull" : "#send");
      }
    }
    function tabKeys(e) {
      if (e.key === "ArrowLeft" || e.key === "Home") { showTab(false); tabSend.focus(); e.preventDefault(); }
      else if (e.key === "ArrowRight" || e.key === "End") { showTab(true); tabPull.focus(); e.preventDefault(); }
    }
    tabSend.addEventListener("click", function () { showTab(false); });
    tabPull.addEventListener("click", function () { showTab(true); });
    tabSend.addEventListener("keydown", tabKeys);
    tabPull.addEventListener("keydown", tabKeys);
    if (location.hash === "#pull") showTab(true);

    // Order matters: browsers hide navigator.usb entirely on an insecure
    // origin, so the missing-API check would misdiagnose Chrome-over-HTTP
    // as the wrong browser instead of the wrong address.
    if (!window.isSecureContext) {
      say("WebUSB only runs over HTTPS. The zip download works here.", true);
      connectBtn.disabled = true;
      pullBtn.disabled = true;
      return;
    }
    if (!navigator.usb) {
      say("This needs WebUSB, which only Chrome and Edge implement. The zip download still works everywhere.", true);
      connectBtn.disabled = true;
      pullBtn.disabled = true;
      return;
    }
    connectBtn.addEventListener("click", function () { if (mtp) disconnect(); else connect(); });
    sendBtn.addEventListener("click", sendAll);
    pullBtn.addEventListener("click", pullAll);
    if (wpall) {
      wpall.addEventListener("change", function () {
        pullRows.forEach(function (pr) { if (!pr.box.disabled) pr.box.checked = wpall.checked; });
      });
    }

    // The master checkbox: toggles every row, and mirrors their mixed state
    // back (indeterminate when some are ticked), so a partial batch reads as
    // what it is.
    var wall = document.getElementById("wall");
    if (wall) {
      var syncWall = function () {
        var boxes = rows.map(function (r) { return r.querySelector("input").checked; });
        var on = boxes.filter(Boolean).length;
        wall.checked = on === boxes.length && on > 0;
        wall.indeterminate = on > 0 && on < boxes.length;
      };
      wall.addEventListener("change", function () {
        rows.forEach(function (row) { row.querySelector("input").checked = wall.checked; });
      });
      rows.forEach(function (row) {
        row.querySelector("input").addEventListener("change", syncWall);
      });
      syncWall(); // the server pre-selects a fortnight, so the master starts mixed
    }
    // Unplugging IS the workflow — the watch imports on disconnect — so the
    // page must come back ready to connect again, not dead until a reload.
    navigator.usb.addEventListener("disconnect", function (e) {
      if (mtp && e.device === mtp.device) {
        mtp = null;
        sendBtn.disabled = true;
        pullBtn.disabled = true;
        setConnectUI(false);
        connectBtn.disabled = false;
        say("Watch unplugged — it imports anything just sent. Check Training → Workouts; reconnect to send again.");
      }
    });
    window.addEventListener("beforeunload", function () { if (mtp) mtp.close(); });
  });
})();
