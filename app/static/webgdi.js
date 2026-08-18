/* The third watch, and the third way in: a Forerunner sitting in its
   proprietary / "GPS mode" (USB 0x091E:0x0003, class 0xFF, no volume). It
   speaks a legacy real-time-GPS protocol, not activity transfer — Garmin's
   own macOS software confirmed it (Garmin Express talks MTP and mass storage,
   never the proprietary protocol; measured 17 Aug 2026 from its own logs). So
   this transport does not reverse-engineer that protocol. It does what
   Garmin's software does: switch the watch to mass storage and read the FIT.

   One bulk-OUT write on the vendor interface re-enumerates the device as a
   drive — measured on the real FR935, 0x0003 → 0x0A83, volume mounts, no
   unplug-replug-tap. After that the drive transport (webmsc.js) handles
   everything, so this file is a BRIDGE: switch, then delegate.

   Two gestures, unavoidably. The WebUSB picker spends the click's transient
   activation, and the volume takes a few seconds to appear across the
   re-enumeration, so the directory picker needs a fresh gesture. connect()
   does the switch and returns a `followup` the page runs on the next click. */

(function () {
  "use strict";

  window.rcTransports = window.rcTransports || [];

  var GARMIN_VENDOR = 0x091e;
  var GPS_MODE_PID = 0x0003;
  // Application layer 0x14, packet id 0x042F, size 1, payload 0x00 — the
  // measured mode-switch. Two independent implementations write it; the
  // device reads nothing back and re-enumerates.
  var SWITCH = new Uint8Array([0x14, 0, 0, 0, 0x2f, 0x04, 0, 0, 0x01, 0, 0, 0, 0]);

  function fatal(msg) { var e = new Error(msg); e.fatal = true; return e; }

  function gdiTransport() {
    var device = null, inner = null;

    var self = {
      id: "gdi",
      label: "Connect (GPS mode)",
      // Once bridged it IS a drive, so these mirror webmsc.js — but they are
      // read off the inner transport once it exists, so a single source of
      // truth stands. Defaults cover the pre-bridge window.
      source: "on the watch",
      after: "Eject the watch before unplugging it.",
      pullLabel: "Pull selected",
      canSend: true,
      onLost: null,
      available: gdiTransport.available,

      connect: async function () {
        // FIRST statement: the WebUSB picker needs the click's activation.
        device = await navigator.usb.requestDevice({ filters: [{ vendorId: GARMIN_VENDOR }] });
        if (device.productId !== GPS_MODE_PID) {
          // Already a drive, or an MTP watch — a different transport's job.
          throw new Error('that watch is not in GPS mode — use "Connect as a drive", or "Connect over USB" for the Epix');
        }
        await device.open();
        if (device.configuration === null) await device.selectConfiguration(1);
        var alt = device.configuration.interfaces[0].alternates[0];
        var epOut = alt.endpoints.find(function (e) { return e.type === "bulk" && e.direction === "out"; });
        if (!epOut) throw new Error("no bulk-OUT endpoint on the vendor interface — cannot switch this device");
        await device.claimInterface(0);
        await device.transferOut(epOut.endpointNumber, SWITCH);
        // The switch re-enumerates the device out from under us; let it go.
        try { await device.releaseInterface(0); } catch (e) { /* already gone */ }
        try { await device.close(); } catch (e) { /* already gone */ }
        device = null;

        return {
          deviceId: "Garmin (GPS mode)",
          title: "switching to drive mode — when the GARMIN drive appears (a few seconds), open it to continue.",
          followup: {
            label: "Open the watch's drive",
            run: async function () {
              // Fresh gesture: the directory picker for the now-mounted
              // volume. The drive transport takes over from here.
              inner = window.rcBuildDrive(window.showDirectoryPicker({ id: "garmin-watch", mode: "read" }));
              inner.onLost = self.onLost;
              var info = await inner.connect();
              // Adopt the drive's own vocabulary now that it is real.
              self.canSend = inner.canSend;
              self.source = inner.source;
              self.after = inner.after;
              self.pullLabel = inner.pullLabel;
              return info;
            },
          },
        };
      },

      // Every data operation forwards to the bridged drive.
      listActivities: function () { return inner.listActivities(); },
      readActivity: function (e) { return inner.readActivity(e); },
      prepareSend: function () { return inner && inner.prepareSend ? inner.prepareSend() : Promise.resolve(); },
      sendWorkout: function (n, b) { return inner.sendWorkout(n, b); },
      dayOf: function (e) { return inner ? inner.dayOf(e) : null; },
      newFilesCount: function () { return inner ? inner.newFilesCount() : Promise.resolve(null); },
      sendClose: function (l) { return inner && inner.sendClose ? inner.sendClose(l) : ""; },
      disconnect: async function () {
        if (inner) { try { await inner.disconnect(); } catch (e) { /* */ } inner = null; }
        if (device) { try { await device.close(); } catch (e) { /* */ } device = null; }
      },
    };
    return self;
  }

  // Both APIs are required: WebUSB to switch, File System Access to read the
  // mounted drive afterward. Secure context first, then each API — the same
  // ordering every transport here follows.
  gdiTransport.available = function () {
    if (!window.isSecureContext) {
      return { ok: false, why: "This page has to be reached over HTTPS to talk to a watch. The zip download works here." };
    }
    if (!navigator.usb) {
      return { ok: false, why: "Switching a watch out of GPS mode needs WebUSB, which only Chrome and Edge implement." };
    }
    if (!window.showDirectoryPicker) {
      return { ok: false, why: "Reading the watch as a drive needs the File System Access API, which only Chrome and Edge implement." };
    }
    return { ok: true, why: "" };
  };
  gdiTransport.label = "Connect (GPS mode)";

  window.rcTransports.push(gdiTransport);
})();
