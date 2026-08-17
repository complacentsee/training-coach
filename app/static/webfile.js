/* Files already on this computer: a Zwift export, a download from somewhere,
   a recording rescued off a card. No device, no protocol — just bytes the
   athlete already has and wants in the archive.

   This is the only transport that needs nothing of the browser. A file input
   is not WebUSB and not the File System Access API, and it does not need a
   secure context, so Safari and Firefox get a real way in rather than being
   told to use the zip download.

   The server needs nothing either: POST /api/activity already checks the
   name, checks the container's own framing and CRC, refuses the same bytes
   under a second name, and stores at most once. Everything below is about
   choosing files and naming them honestly.

   NAMING. The archive's rule is that a stored file keeps its own name — the
   device's, when a device wrote it. A file from a download has a name too,
   but it may be one the store will not take: at most 100 characters of
   [A-Za-z0-9._-] ending .fit, never leading with a dot, never containing "..".
   So the name that will be archived is COMPUTED and SHOWN, before anything is
   uploaded, beside the name on disk. Nothing is renamed invisibly. That is the
   same discipline the FIT workout names already follow — transliterated,
   capped, and visible in the row before it ships. */

(function () {
  "use strict";

  window.rcTransports = window.rcTransports || [];

  var MAX_NAME = 100; // validActivityName's limit, and the reason for the cap

  // archiveName computes the name the store will hold a chosen file under, or
  // null when no honest name can be made of it. It mirrors the server's rule
  // rather than guessing at it: anything outside [A-Za-z0-9._-] becomes a
  // dash, runs collapse, a leading non-alphanumeric is dropped (the store
  // refuses a dotfile, and so does the deploy pipeline's junk scan), repeated
  // dots collapse because ".." can traverse, and the .fit extension is kept.
  //
  // The cross-language check in tools/webfile_replay.js feeds this function's
  // output to the Go rule itself, so the two cannot drift silently.
  function archiveName(local) {
    var s = String(local || "");
    // Take the basename: a browser gives just the name, but a drag from some
    // sources carries a path.
    s = s.replace(/^.*[\\/]/, "");
    if (!/\.fit$/i.test(s)) return null; // not a FIT file by name
    var ext = s.slice(-4);               // keep .fit or .FIT as chosen
    var base = s.slice(0, -4);
    base = base.replace(/[^A-Za-z0-9._-]/g, "-");
    base = base.replace(/-{2,}/g, "-");
    base = base.replace(/\.{2,}/g, ".");
    base = base.replace(/^[^A-Za-z0-9]+/, "");
    // Trailing dots would put ".." across the extension boundary.
    base = base.replace(/\.+$/, "");
    if (!base) return null;
    if (base.length + 4 > MAX_NAME) base = base.slice(0, MAX_NAME - 4);
    base = base.replace(/[.\-]+$/, "");
    if (!base) return null;
    return base + ext;
  }

  function fileTransport() {
    var input = null, chosen = [];

    var self = {
      id: "file",
      label: "Upload files",
      soloLabel: "Upload files",
      // What the page calls the place recordings came from, so its sentences
      // read true for something that is not a watch.
      source: "in the chosen files",
      after: "",
      pullLabel: "Upload selected",
      onLost: null,
      available: fileTransport.available,
      canSend: false,

      connect: async function () {
        // FIRST statement, before any await: clicking a file input needs the
        // click's activation exactly as a picker does.
        input = document.createElement("input");
        input.type = "file";
        input.multiple = true;
        input.accept = ".fit,.FIT";
        // Kept out of the layout rather than display:none — a detached input
        // still opens its dialog, and nothing here should shift the page.
        input.style.position = "fixed";
        input.style.left = "-9999px";
        document.body.appendChild(input);
        var picked = new Promise(function (resolve, reject) {
          input.addEventListener("change", function () {
            resolve(Array.prototype.slice.call(input.files || []));
          });
          // Browsers fire cancel when the dialog is dismissed (Chrome 113,
          // Firefox 91, Safari 16.4). An older one leaves this pending and the
          // button waiting until the page is reloaded; there is no reliable
          // signal to synthesise, and inventing one that misfires would be
          // worse than waiting.
          input.addEventListener("cancel", function () {
            reject(new Error("no files chosen"));
          });
        });
        input.click();

        chosen = await picked;
        if (!chosen.length) throw new Error("no files chosen");
        var named = 0, refused = 0;
        chosen.forEach(function (f) { archiveName(f.name) ? named++ : refused++; });
        return {
          deviceId: "local files",
          title: chosen.length + " file" + (chosen.length === 1 ? "" : "s") + " chosen" +
            (refused ? " — " + refused + " cannot be named for the archive" : "") +
            ". Nothing is uploaded until you say so.",
        };
      },

      listActivities: async function () {
        var out = [];
        chosen.forEach(function (f, i) {
          var name = archiveName(f.name);
          if (!name) return; // shown as refused by the count in the title
          var row = { id: i, name: name, size: f.size };
          // from is set only when the two differ, and the page renders it as
          // "on disk → archived as" so the rename is never invisible.
          if (name !== f.name) row.from = f.name;
          out.push(row);
        });
        return out;
      },

      readActivity: async function (entry) {
        var f = chosen[entry.id];
        if (!f) throw new Error("that file is no longer selected");
        var buf = await f.arrayBuffer();
        return new Uint8Array(buf);
      },

      // A download's name says nothing reliable about which training day it
      // belongs to, so this claims nothing and the server's settle window
      // decides when a day is complete.
      dayOf: function () { return null; },

      sendWorkout: async function () {
        throw new Error("this is an upload path; send workouts to a watch over USB");
      },
      newFilesCount: async function () { return null; },
      disconnect: async function () {
        if (input && input.parentNode) input.parentNode.removeChild(input);
        input = null;
        chosen = [];
      },
    };
    return self;
  }

  // Always available. A file input is not gated on a secure context or on any
  // API a browser might not ship, which is the point of having this at all.
  fileTransport.available = function () { return { ok: true, why: "" }; };
  fileTransport.label = "Upload files";
  // What the page's single button is called when this is the only way in —
  // which is every browser that has neither WebUSB nor a directory picker.
  fileTransport.soloLabel = "Upload files";
  // Exported for the cross-language name check; nothing in the page calls it.
  fileTransport.archiveName = archiveName;

  window.rcTransports.push(fileTransport);
})();
