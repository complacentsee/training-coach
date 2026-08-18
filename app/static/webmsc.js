/* The second watch: a Forerunner that mounts as mass storage instead of
   speaking MTP.

   There is no protocol here. The device is a FAT volume the browser hands over
   as a FileSystemDirectoryHandle, and the work is entirely in knowing which
   directory holds what — which the device itself answers, in
   GARMIN/GarminDevice.xml.

   Two promises this file keeps, both of them the same ones the MTP path keeps:
   it reads exactly the manifest's OutputFromUnit activity directory, and it
   never deletes or renames anything.

   And one promise only this file needs. GarminDevice.xml on this device
   carries StoreKey and Modulus — the keys the device's own software uses. It
   is parsed HERE, in the browser, and its text never reaches the server, the
   transfer log, or any error message. The server has a log; the manifest must
   not end up in it.

   Platform truth: window.showDirectoryPicker is the File System Access API,
   Chromium-only and secure-context-only, the same two constraints WebUSB
   already imposes. Chromium's blocklist of un-pickable directories was read at
   branch-heads/7922 and contains no occurrence of "Volumes", so /Volumes/GARMIN
   is pickable. */

(function () {
  "use strict";

  // The transport registry the page reads. Order is load order, and both
  // scripts are `defer`, so this is deterministic; the page renders one
  // connect button per AVAILABLE transport and stays single-button when only
  // one is.
  window.rcTransports = window.rcTransports || [];

  // The manifest names its directories relative to the volume root. Everything
  // else here is derived from it rather than assumed — except these fallbacks,
  // for a device whose manifest cannot be read at all.
  var FALLBACK_ACTIVITY = "GARMIN/ACTIVITY";
  var FALLBACK_INBOX = "GARMIN/NEWFILES";

  // The watch stores at most 25 workouts and silently deletes the overflow
  // from its inbox — measured 12 Aug 2026, when 63 files slot-filled to
  // exactly 25. Refusing to write past the cap here turns a silent deletion
  // into a visible refusal.
  var WORKOUT_CAP = 25;
  var MANIFEST = "GarminDevice.xml";

  // FIT_TYPE_4 is recorded activities, FIT_TYPE_5 is workouts. The names are
  // the manifest's own; the paths are not, and are read from it.
  var TYPE_ACTIVITY = "FIT_TYPE_4";

  function fatal(msg) {
    var e = new Error(msg);
    e.fatal = true;
    return e;
  }

  function trace(line) {
    var el = document.getElementById("wtrace");
    if (!el) return;
    var stamp = new Date().toISOString().slice(11, 23);
    el.textContent = (el.textContent === "(nothing yet — connect first)" ? "" : el.textContent + "\n") +
      stamp + "  " + line;
  }

  /* ── reading the volume ────────────────────────────────────────────── */

  // dirNamed / fileNamed find a child by name, CASE-INSENSITIVELY — a FAT
  // volume stores "Garmin" or "GARMIN" as the mood took the firmware (the
  // Edge 520 Plus writes "Garmin/Activities", the watches "GARMIN/ACTIVITY"),
  // and the File System Access API matches names as stored. An exact
  // getDirectoryHandle would find one device's layout and miss another's, so
  // the directory is scanned and matched without regard to case. The exact
  // handle is tried first, since it is one call when it hits.
  async function dirNamed(dir, name) {
    try { return await dir.getDirectoryHandle(name); } catch (e) { /* fall through */ }
    var lc = name.toLowerCase();
    for await (var entry of dir.values()) {
      if (entry.kind === "directory" && entry.name.toLowerCase() === lc) return entry;
    }
    return null;
  }
  async function fileNamed(dir, name) {
    try { return await (await dir.getFileHandle(name)).getFile(); } catch (e) { /* fall through */ }
    var lc = name.toLowerCase();
    for await (var entry of dir.values()) {
      if (entry.kind === "file" && entry.name.toLowerCase() === lc) return await entry.getFile();
    }
    return null;
  }

  // childDir walks a slash-separated path, each segment matched
  // case-insensitively, returning null when a segment is missing.
  async function childDir(dir, path) {
    var parts = path.split("/").filter(Boolean);
    var cur = dir;
    for (var i = 0; i < parts.length; i++) {
      cur = await dirNamed(cur, parts[i]);
      if (!cur) return null;
    }
    return cur;
  }

  async function childFile(dir, name) {
    return fileNamed(dir, name);
  }

  // stillMounted asks the cheapest question that distinguishes "that file is
  // gone" from "the volume is gone": can the directory still be read at all.
  // An empty directory iterates to done without throwing, so an empty watch
  // is mounted, not missing. The manifest is deliberately NOT re-read — it
  // carries keys, and this is a liveness check, not a re-identification.
  async function stillMounted(dir) {
    try {
      await dir.values().next();
      return true;
    } catch (e) {
      return false;
    }
  }

  // findManifest accepts either the volume root or the GARMIN folder itself,
  // because the picker returns whatever the athlete chose and both are
  // reasonable choices. It reports which, because that decides what the
  // manifest's paths are relative to.
  async function findManifest(picked) {
    var garmin = await childDir(picked, "GARMIN");
    if (garmin) {
      var f = await childFile(garmin, MANIFEST);
      if (f) return { insideGarmin: false, text: await f.text() };
    }
    var here = await childFile(picked, MANIFEST);
    if (here) return { insideGarmin: true, text: await here.text() };
    return null;
  }

  // resolve turns a manifest path into a directory handle. The manifest names
  // its paths from the VOLUME ROOT ("GARMIN/ACTIVITY"), so when the athlete
  // picked the GARMIN folder itself there is no parent to walk up to — a
  // directory handle only goes downward — and the leading segment has to come
  // off instead. Both spellings are tried either way: the path this device
  // writes was not measured, and guessing wrong would refuse a working watch.
  async function resolve(picked, found, path) {
    var candidates = [path];
    var first = path.split("/").filter(Boolean)[0];
    if (found.insideGarmin && first) {
      candidates.unshift(path.split("/").slice(1).join("/"));
    } else if (first && first.toUpperCase() !== "GARMIN") {
      candidates.push("GARMIN/" + path);
    }
    for (var i = 0; i < candidates.length; i++) {
      if (!candidates[i]) continue;
      var d = await childDir(picked, candidates[i]);
      if (d) return d;
    }
    return null;
  }

  // parseManifest pulls out only what this page uses: the device's description,
  // and the directory each data type lives in. Everything else in the file —
  // including StoreKey and Modulus — is read and dropped on the floor.
  //
  // The shape, from the device XSD:
  //   <Device><Model><Description>…</Description></Model>
  //     <MassStorageMode>
  //       <DataType><Name>FIT_TYPE_4</Name>
  //         <File><TransferDirection>OutputFromUnit</TransferDirection>
  //           <Location><Path>GARMIN/ACTIVITY</Path>
  //                     <FileExtension>FIT</FileExtension></Location>
  //         </File>
  //       </DataType>
  //     …
  function parseManifest(text) {
    var doc = new DOMParser().parseFromString(text, "application/xml");
    if (doc.getElementsByTagName("parsererror").length) {
      throw new Error("the device's manifest is not valid XML");
    }
    var textOf = function (el, tag) {
      var n = el.getElementsByTagName(tag);
      return n.length ? (n[0].textContent || "").trim() : "";
    };

    var out = { description: "", types: {} };
    var models = doc.getElementsByTagName("Model");
    if (models.length) out.description = textOf(models[0], "Description");

    var types = doc.getElementsByTagName("DataType");
    for (var i = 0; i < types.length; i++) {
      var name = textOf(types[i], "Name");
      if (!name) continue;
      var files = types[i].getElementsByTagName("File");
      for (var j = 0; j < files.length; j++) {
        var dirn = textOf(files[j], "TransferDirection");
        var locs = files[j].getElementsByTagName("Location");
        if (!locs.length) continue;
        var entry = {
          path: textOf(locs[0], "Path"),
          ext: (textOf(locs[0], "FileExtension") || "FIT").toUpperCase(),
        };
        if (!entry.path) continue;
        out.types[name] = out.types[name] || {};
        // A data type can list both directions; keep them apart, because
        // reading from the inbox and writing to the outbox are both wrong.
        out.types[name][dirn === "InputToUnit" ? "in" : "out"] = entry;
      }
    }
    return out;
  }

  /* ── the transport ─────────────────────────────────────────────────── */

  // countFIT is the inbox census: real .fit files, junk ignored — a FAT
  // volume that has met macOS carries ._name side-files and .DS_Store, and
  // neither is a workout the watch will import.
  async function countFIT(dir) {
    var n = 0;
    for await (var entry of dir.values()) {
      if (entry.kind !== "file") continue;
      if (entry.name.charAt(0) === ".") continue;
      if (!/\.fit$/i.test(entry.name)) continue;
      n++;
    }
    return n;
  }

  // scanFIT walks a volume for .fit files, bounded in depth and count, for a
  // device that ships no GarminDevice.xml manifest — a non-Garmin computer,
  // or a bare card. Garmin's own paths are read from the manifest and never
  // reach here; this is the fallback that makes the drive brand-agnostic.
  // Junk (dot-dirs, AppleDouble) is skipped, and known system folders are not
  // descended. cb(name, fileHandle, size) per hit.
  var SKIP_DIRS = { "system volume information": 1, ".spotlight-v100": 1,
    ".fseventsd": 1, ".trashes": 1, "system": 1 };
  async function scanFIT(dir, depth, cap, count, cb) {
    if (depth < 0 || count.n >= cap) return;
    for await (var entry of dir.values()) {
      if (count.n >= cap) return;
      if (entry.name.charAt(0) === ".") continue;
      if (entry.kind === "directory") {
        if (SKIP_DIRS[entry.name.toLowerCase()]) continue;
        await scanFIT(entry, depth - 1, cap, count, cb);
      } else if (/\.fit$/i.test(entry.name)) {
        var f = await entry.getFile();
        count.n++;
        cb(entry.name, entry, f.size);
      }
    }
  }

  // mscTransport is a PURE constructor: it takes a directory handle and asks
  // nothing of the user. That is what makes it testable — a handle can come
  // from a picker, from OPFS, or from a shim, and the transport cannot tell.
  // fromPicker below is the only thing in this file that prompts.
  // dirHandle may be a FileSystemDirectoryHandle or a promise for one. That
  // one line of tolerance is what lets fromPicker call the picker as its very
  // first statement and still hand back a fully-built transport: the promise
  // is awaited later, inside connect, where an await costs nothing.
  function mscTransport(dirHandle) {
    var root = null, activityDir = null, manifest = null, label = "";
    var foundAt = null, inboxDir = null, inboxCount = -1;
    var generic = false, genericFiles = null;
    var MAX_SCAN = 500; // a volume with more than this is not one to enumerate

    var self = {
      id: "msc",
      label: "Watch as a drive",
      source: "on the watch",
      after: "Eject the watch before unplugging it.",
      pullLabel: "Pull selected",
      onLost: null,
      available: mscTransport.available,

      // No picker here: the handle was supplied. fromPicker owns the gesture.
      connect: async function () {
        root = await dirHandle;
        if (!root) throw new Error("no directory was chosen");

        var found = await findManifest(root);
        foundAt = found;
        if (found) {
          // A Garmin device: paths come from its manifest, exactly. StoreKey
          // and Modulus in the same file are read past and never held.
          manifest = parseManifest(found.text);
          found.text = null;
          var t4 = (manifest.types[TYPE_ACTIVITY] || {}).out;
          var path = t4 ? t4.path : FALLBACK_ACTIVITY;
          if (!t4) trace("manifest names no " + TYPE_ACTIVITY + " output — falling back to " + FALLBACK_ACTIVITY);
          activityDir = await resolve(root, found, path);
          if (!activityDir) throw new Error("the manifest names " + path + ", which is not on this volume");
          // Send only where the manifest names an inbox.
          self.canSend = !!((manifest.types["FIT_TYPE_5"] || {})["in"]);
          label = manifest.description || "Garmin device";
          trace("mounted " + label + ", activities in " + path);
          return {
            deviceId: label,
            title: label + " — " + path + " found. " +
              (self.canSend ? "Pull activities, or send workouts into its inbox." : "Pull activities."),
          };
        }
        // No manifest: any device that stores FIT on a drive — a Wahoo, a
        // card, another brand. Find the files wherever they are; read-only,
        // because without a manifest there is no known inbox to write to.
        generic = true;
        self.canSend = false;
        var c = { n: 0 };
        await scanFIT(root, 3, 25, c, function () {}); // a shallow count for the note
        if (!c.n) {
          throw new Error("no FIT files here — choose the device's drive (or a folder holding its activities)");
        }
        label = "the chosen drive";
        trace("no manifest — treating as a generic FIT drive (best-effort, unverified layout)");
        return {
          deviceId: "drive",
          title: "a drive with FIT activities (" + c.n + "+ found) — best-effort read for a device without a " +
            MANIFEST + " manifest, not tested against this hardware. Pull below; sending is off.",
        };
      },

      listActivities: async function () {
        if (generic) {
          genericFiles = {};
          var g = [];
          var c = { n: 0 };
          await scanFIT(root, 3, MAX_SCAN, c, function (name, fh, size) {
            // Names can repeat across folders; disambiguate the id, but show
            // the plain name. The server keys identity on the archived name.
            var id = genericFiles[name] ? name + "#" + c.n : name;
            genericFiles[id] = fh;
            g.push({ id: id, name: name, size: size });
          });
          g.sort(function (a, b) { return a.name < b.name ? 1 : a.name > b.name ? -1 : 0; });
          return g;
        }
        var want = "." + ((manifest.types[TYPE_ACTIVITY] || {}).out || { ext: "FIT" }).ext.toLowerCase();
        var out = [];
        for await (var entry of activityDir.values()) {
          if (entry.kind !== "file") continue;
          var lower = entry.name.toLowerCase();
          if (lower.slice(-want.length) !== want) continue;
          // A FAT volume that has met macOS carries ._name side-files and
          // .DS_Store. They are not recordings, and the server would refuse
          // them anyway — its name rule was written for this junk.
          if (entry.name.charAt(0) === ".") continue;
          var f = await entry.getFile();
          out.push({ id: entry.name, name: entry.name, size: f.size });
        }
        // Newest first, the same order the MTP path lists in. These names are
        // not timestamps, so this is alphabetical and nothing more; the page
        // shows what the device holds, in a stable order.
        out.sort(function (a, b) { return a.name < b.name ? 1 : a.name > b.name ? -1 : 0; });
        return out;
      },

      readActivity: async function (entry) {
        if (generic) {
          var gh = genericFiles && genericFiles[entry.id];
          if (!gh) throw new Error("that file is no longer listed — reconnect");
          return new Uint8Array(await (await gh.getFile()).arrayBuffer());
        }
        var f = await childFile(activityDir, entry.id);
        if (!f) {
          // One missing file and a vanished volume look identical from here —
          // both are "cannot open that" — and they want opposite answers. A
          // pulled cable would otherwise print the same error on every
          // remaining row instead of stopping and saying what happened.
          if (!(await stillMounted(activityDir))) {
            throw fatal("the watch was unmounted — plug it back in, reconnect, then pull the rest");
          }
          throw new Error("gone from the volume since it was listed");
        }
        var buf = await f.arrayBuffer();
        return new Uint8Array(buf);
      },

      // Which training day a recording belongs to. A device that names files
      // by the clock says so in the name; this one uses opaque 8.3 codes
      // (invented example: 4T7B0091.FIT),
      // which says nothing. Answering null is not a gap — it tells the page to
      // let the server's settle window decide when the day is complete, which
      // is exactly what that window exists for. Decoding the FIT here to do
      // better would put a second implementation of "what day is this" in the
      // browser, and this repo has one register on purpose.
      dayOf: function () { return null; },

      canSend: true,

      // prepareSend runs as the send click's FIRST statement, because asking
      // to write needs the click's transient activation and the batch loop's
      // first write sits behind a fetch. The picker asked for read only —
      // pulls stay read-only — so the same handle is upgraded here, once.
      prepareSend: function () {
        if (!root || !root.requestPermission) return Promise.resolve();
        return root.queryPermission({ mode: "readwrite" }).then(function (st) {
          if (st === "granted") return;
          return root.requestPermission({ mode: "readwrite" }).then(function (st2) {
            if (st2 !== "granted") {
              throw fatal("writing to the watch was not allowed — allow it when the browser asks, then send again");
            }
          });
        });
      },

      // sendWorkout writes into the manifest's InputToUnit inbox. The row
      // counts as sent only when this resolves, and this resolves only after
      // close() — Chromium's commit: it writes <name>.crswap beside the
      // target and renames on close, so an unclosed write is a file that
      // never existed, MTP's rollback by a different mechanism.
      sendWorkout: async function (name, bytes) {
        if (!inboxDir) {
          var t5 = (manifest.types["FIT_TYPE_5"] || {})["in"];
          var path = t5 ? t5.path : FALLBACK_INBOX;
          if (!t5) trace("manifest names no FIT_TYPE_5 inbox — falling back to " + FALLBACK_INBOX);
          inboxDir = await resolve(root, foundAt, path);
          if (!inboxDir) throw fatal("the manifest names " + path + ", which is not on this volume");
          inboxCount = await countFIT(inboxDir);
          trace("inbox " + path + " holds " + inboxCount + " file(s)");
        }
        // The cap: a same-named write replaces and costs no slot; a new name
        // past 25 would be silently deleted by the watch, so refuse it here
        // where the refusal can be seen.
        var replacing = !!(await childFile(inboxDir, name));
        if (!replacing && inboxCount >= WORKOUT_CAP) {
          throw new Error("the watch stores at most " + WORKOUT_CAP + " workouts and its inbox already holds " +
            inboxCount + " — it would silently delete this one");
        }
        var fh = await inboxDir.getFileHandle(name, { create: true });
        var w = await fh.createWritable();
        await w.write(bytes);
        await w.close();
        // Read back what landed: close() resolving is Chromium's promise,
        // and this is the check that the promise was kept.
        var f = await fh.getFile();
        if (f.size !== bytes.byteLength) {
          throw new Error("wrote " + bytes.byteLength + " bytes but the volume holds " + f.size + " — do not trust this file");
        }
        if (!replacing) inboxCount++;
      },

      // What the inbox holds, counted from the volume. Unlike MTP this is
      // not the device acknowledging anything — it is files sitting in a
      // folder the watch has not looked at yet, which is why the closing
      // sentence says eject rather than claiming success.
      newFilesCount: async function () {
        return inboxDir ? countFIT(inboxDir) : null;
      },

      // The half MTP cannot mislead about here: there is no session and no
      // observable import. Eject is the athlete's promise, said plainly.
      sendClose: function (listed) {
        return (listed === null ? " — written." : " — " + listed + " file(s) now in the watch's inbox.") +
          " EJECT the watch before unplugging; it imports on its own and the files appear under Training → Workouts.";
      },

      disconnect: async function () {
        root = activityDir = manifest = genericFiles = null;
        generic = false;
      },
    };
    return self;
  }

  // fromPicker is the ONLY picker call in this file, and the picker is its
  // first statement. Transient activation is about 4.9 seconds and is lost
  // across an await, so anything awaited before this line breaks the picker
  // with a SecurityError that reads exactly like a permissions problem.
  //
  // mode "read" is what this page currently does. Writing workouts will ask to
  // upgrade the same handle rather than pick again.
  mscTransport.fromPicker = function () {
    return mscTransport(window.showDirectoryPicker({ id: "garmin-watch", mode: "read" }));
  };
  mscTransport.available = function () {
    // Secure context first: a browser hides the whole API on an insecure
    // origin, so asking about the API first misdiagnoses the address as the
    // browser.
    if (!window.isSecureContext) {
      return { ok: false, why: "This page has to be reached over HTTPS to talk to a watch. The zip download works here." };
    }
    if (!window.showDirectoryPicker) {
      return { ok: false, why: "Reading the watch as a drive needs the File System Access API, which only Chrome and Edge implement." };
    }
    return { ok: true, why: "" };
  };

  // The registered factory IS fromPicker, so the page must be able to read a
  // transport's name and availability WITHOUT calling it — otherwise drawing
  // the chooser would open a directory picker at page load. Assigned down
  // here, after available exists: doing it beside fromPicker set it to
  // undefined, because a function declared with `=` is not hoisted.
  mscTransport.fromPicker.available = mscTransport.available;
  mscTransport.fromPicker.label = "Connect as a drive";

  window.rcTransports.push(mscTransport.fromPicker);

  // Exposed for the GPS-mode bridge (webgdi.js): after it switches a watch
  // from proprietary to mass storage, it builds a drive transport around the
  // now-mounted volume and delegates every operation to it. One
  // implementation of the drive, two ways in.
  window.rcBuildDrive = function (handleOrPromise) { return mscTransport(handleOrPromise); };
  window.rcDrivePicker = mscTransport.fromPicker;
})();
