"""CROSS-VALIDATION MIRROR of the measurement register in app/metrics.go —
the register's authoritative home since 14 Aug 2026, when grading moved into
the binary. This script exists to check that register against an independent
implementation (the acceptance gate in app/acceptgate_test.go runs both over
the whole archive); it is also still runnable by hand exactly as before.
A measurement convention changes in app/metrics.go and here in step, or not
at all.

    python3 tools/grade_metrics.py --streams s.json --athlete app/data/athlete.json --kind bike

--streams is the JSON returned by the Strava MCP get_activity_streams call,
saved verbatim (needs `time` + `heart_rate`; `watts`, `velocity_smooth` and
`cadence` are used when present). --kind is `bike` or `run`.

The pinned conventions, restated from the register:
- Every share is time-weighted: sample i covers time[i] - time[i-1] seconds.
  Never count samples; resampled streams only look uniform. A non-positive
  interval contributes nothing (clocks can step backwards at a chained
  file's seam).
- HR dropouts: samples under 50 bpm are excluded from all HR statistics and
  the excluded share is reported. Over 5% excluded, the note must say so.
- Runs are graded on the whole run, warm-up, strides and all — that is what
  the block legend's "share of minutes under the cap" has always meant.
- Bikes: the in-band share is measured after the first 600 s, because a ride
  should spend its warm-up below the band; the cap applies to the whole ride.
- Drift is second-half mean HR minus first-half mean HR, split at half the
  elapsed time. Decoupling is (first-half output/HR) / (second-half) - 1,
  using watts on a bike and velocity on a run; absent that stream it is
  omitted, not approximated. On a bike the first 600 s are excluded — the
  warm-up's rising HR reads as decoupling and swamps the signal. A run keeps
  its whole trace because the DEC protocol is a cold start by design.
- Run cadence is reported doubled (Strava streams are per-leg for runs).
- Anchors come only from athlete.json — never retyped, never from an older doc.
"""
import argparse, json, re, sys

def parse_weight_kg(s):
    m = re.match(r"([\d.]+)\s*(kg|lb)", s)
    if not m:
        sys.exit(f"unparseable weight: {s!r}")
    v = float(m.group(1))
    return v if m.group(2) == "kg" else v * 0.45359237

def weighted(t, vals, keep):
    """(total secs kept, mean, share of secs where keep) over paired samples."""
    tot = tot_v = 0.0
    for i in range(1, len(t)):
        dt = t[i] - t[i - 1]
        if dt <= 0:
            continue
        if keep(vals[i]):
            tot += dt
            tot_v += dt * vals[i]
    return tot, (tot_v / tot if tot else None)

def share(t, vals, pred, valid=lambda v: True):
    num = den = 0.0
    for i in range(1, len(t)):
        dt = t[i] - t[i - 1]
        if dt <= 0:
            continue
        if valid(vals[i]):
            den += dt
            if pred(vals[i]):
                num += dt
    return num / den if den else None

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--streams", required=True)
    ap.add_argument("--athlete", required=True)
    ap.add_argument("--kind", required=True, choices=["bike", "run"])
    args = ap.parse_args()

    s = json.load(open(args.streams))
    a = json.load(open(args.athlete))
    t, hr = s["time"], s.get("heart_rate")
    if not hr:
        sys.exit("no heart_rate stream")
    kg = parse_weight_kg(a["weight"])
    anchors = a["hr"]
    out = {"elapsed": t[-1], "elapsed_hms": f"{t[-1]//60}:{t[-1]%60:02d}"}

    ok = lambda v: v >= 50  # dropout rule
    dropped = share(t, hr, lambda v: not ok(v))
    kept_secs, avg_hr = weighted(t, hr, ok)
    out["hr"] = {
        "avg": round(avg_hr, 1),
        "max": max(v for v in hr if ok(v)),
        "dropout_share": round(dropped, 4),
    }

    half = t[-1] / 2
    firsts = [(ti, v) for ti, v in zip(t, hr) if ok(v) and ti <= half]
    seconds = [(ti, v) for ti, v in zip(t, hr) if ok(v) and ti > half]
    if firsts and seconds:
        m1 = weighted([x for x, _ in firsts], [v for _, v in firsts], ok)[1]
        m2 = weighted([x for x, _ in seconds], [v for _, v in seconds], ok)[1]
        out["hr"]["drift"] = round(m2 - m1, 1)

    w = s.get("watts")
    if w:
        _, avg_w = weighted(t, w, lambda v: True)
        out["power"] = {"avg": round(avg_w, 1)}
        # FTP is a cycling anchor, so nothing derived from it is reported for
        # a run: a running-power estimate divided by cycling FTP is a ratio
        # with no meaning, and quoting it invites grading a run on it.
        if args.kind == "bike":
            out["power"]["wkg"] = round(avg_w / kg, 2)
            ftp = a.get("power", {}).get("ftp")
            if ftp:
                out["power"]["pct_ftp"] = round(avg_w / ftp, 3)
                out["power"]["z2_band_w"] = [round(0.56 * ftp), round(0.75 * ftp)]

    output = w or s.get("velocity_smooth")
    if output and firsts and seconds:
        def half_eff(lo, hi):
            num = den = 0.0
            for i in range(1, len(t)):
                dt = t[i] - t[i - 1]
                if dt <= 0:
                    continue
                if lo < t[i] <= hi and ok(hr[i]) and output[i] > 0:
                    num += dt * (output[i] / hr[i])
                    den += dt
            return num / den if den else None
        start = 600 if args.kind == "bike" else 0
        mid = start + (t[-1] - start) / 2
        e1, e2 = half_eff(start, mid), half_eff(mid, t[-1])
        if e1 and e2:
            out["decoupling_pct"] = round((e1 / e2 - 1) * 100, 2)

    cad = s.get("cadence")
    if cad:
        _, avg_c = weighted(t, cad, lambda v: v > 0)
        if avg_c:
            out["cadence"] = round(avg_c * (2 if args.kind == "run" else 1), 1)

    if args.kind == "run":
        cap = anchors["gradeCap"]
        out["grade_input"] = {
            "under_grade_cap_share": round(share(t, hr, lambda v: v <= cap, ok), 4),
            "grade_cap": cap,
        }
        f20 = [(ti, v) for ti, v in zip(t, hr) if ok(v) and ti <= 1200]
        if f20:
            m = weighted([x for x, _ in f20], [v for _, v in f20], ok)[1]
            out["first_20min"] = {"avg": round(m, 1), "cap": anchors["firstMin"]}
    else:
        lo, hi, cap = anchors["bikeLo"], anchors["bikeHi"], anchors["bikeCap"]
        after_warmup = lambda: [
            (ti, v) for ti, v in zip(t, hr) if ti > 600 and ok(v)
        ]
        aw = after_warmup()
        tw, vw = [x for x, _ in aw], [v for _, v in aw]
        out["grade_input"] = {
            "in_band_share_after_warmup": round(share(tw, vw, lambda v: lo <= v <= hi), 4),
            "band": [lo, hi],
            "cap": cap,
            "secs_over_cap": round(share(t, hr, lambda v: v > cap, ok) * kept_secs),
        }

    json.dump(out, sys.stdout, indent=1)
    print()

if __name__ == "__main__":
    main()
