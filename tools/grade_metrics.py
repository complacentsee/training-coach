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
- A sample whose interval spans a RECORDING GAP carries ZERO weight in every
  statistic (since 17 Aug 2026): one post-resume second is not evidence
  about the minutes the watch was stopped. The gap threshold is the
  register's own — max(10 s, 5x the median positive interval). Statistics
  are therefore MOVING-time statistics; `elapsed` keeps the wall clock and
  `moving` says how much of it was recorded work.
- Windowed statistics (drift halves, first-20-min, the bike after-warm-up
  window) select samples by predicate on the original stream, each keeping
  its own weight — never by rebuilding a sublist and re-weighting inside it.
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

def sample_weights(t):
    """w[i] = seconds sample i covers; zero across a recording gap or a
    non-positive interval. The gap threshold mirrors recordingGapS."""
    dts = sorted(dt for i in range(1, len(t)) if (dt := t[i] - t[i - 1]) > 0)
    gap_s = max(10, dts[len(dts) // 2] * 5) if dts else 10
    w = [0] * len(t)
    for i in range(1, len(t)):
        dt = t[i] - t[i - 1]
        if 0 < dt < gap_s:
            w[i] = dt
    return w

def weighted(w, vals, keep):
    """(total secs kept, mean) over paired samples, sample i covering w[i]."""
    tot = tot_v = 0.0
    for i in range(1, len(w)):
        if w[i] > 0 and keep(vals[i]):
            tot += w[i]
            tot_v += w[i] * vals[i]
    return tot, (tot_v / tot if tot else None)

def share(w, vals, pred, valid=lambda v: True):
    num = den = 0.0
    for i in range(1, len(w)):
        if w[i] > 0 and valid(vals[i]):
            den += w[i]
            if pred(vals[i]):
                num += w[i]
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
    w = sample_weights(t)
    moving = sum(w)
    out = {"elapsed": t[-1], "elapsed_hms": f"{t[-1]//60}:{t[-1]%60:02d}",
           "moving": moving}

    ok = lambda v: v >= 50  # dropout rule
    dropped = share(w, hr, lambda v: not ok(v))
    kept_secs, avg_hr = weighted(w, hr, ok)
    out["hr"] = {
        "avg": round(avg_hr, 1),
        "max": max(v for v in hr if ok(v)),
        "dropout_share": round(dropped, 4),
    }

    # Drift: predicate on the original stream, each sample keeping its own
    # weight — never a rebuilt sublist.
    half = t[-1] / 2
    w1 = s1 = w2 = s2 = 0.0
    for i in range(1, len(w)):
        if w[i] > 0 and ok(hr[i]):
            if t[i] <= half:
                w1 += w[i]; s1 += w[i] * hr[i]
            else:
                w2 += w[i]; s2 += w[i] * hr[i]
    if w1 and w2:
        out["hr"]["drift"] = round(s2 / w2 - s1 / w1, 1)

    watts = s.get("watts")
    if watts:
        _, avg_w = weighted(w, watts, lambda v: True)
        out["power"] = {"avg": round(avg_w, 1), "max": max(watts)}
        # best_60s is the highest time-weighted mean over any 60 s of
        # ELAPSED time — deliberately not the gap rule: it is max-seeking,
        # a window spanning a stop can only lose, and changing it buys
        # nothing. A ramp
        # test's result IS this number: FTP is 75% of it, and an average
        # over the whole ride describes a climb to failure as a steady ride.
        if args.kind == "bike":
            best, j = None, 0
            csum = [0.0] * len(t)
            cdur = [0.0] * len(t)
            for i in range(1, len(t)):
                dt = max(t[i] - t[i - 1], 0)
                csum[i] = csum[i - 1] + dt * watts[i]
                cdur[i] = cdur[i - 1] + dt
            for i in range(len(t)):
                while j < len(t) and t[j] - t[i] < 60:
                    j += 1
                if j >= len(t):
                    break
                d = cdur[j] - cdur[i]
                if d > 0:
                    m = (csum[j] - csum[i]) / d
                    best = m if best is None or m > best else best
            if best is not None:
                out["power"]["best_60s"] = round(best, 1)
        # FTP is a cycling anchor, so nothing derived from it is reported for
        # a run: a running-power estimate divided by cycling FTP is a ratio
        # with no meaning, and quoting it invites grading a run on it.
        if args.kind == "bike":
            out["power"]["wkg"] = round(avg_w / kg, 2)  # avg is moving-based
            ftp = a.get("power", {}).get("ftp")
            if ftp:
                out["power"]["pct_ftp"] = round(avg_w / ftp, 3)
                out["power"]["z2_band_w"] = [round(0.56 * ftp), round(0.75 * ftp)]

    def decoupling(output):
        """(first-half output/HR) / (second-half) - 1, in percent, over the
        whole file; the bike's first 600 s excluded. None when a half has
        nothing valid in it. Mirrors windowDecoupling over (0, elapsed]."""
        def half_eff(lo, hi):
            num = den = 0.0
            for i in range(1, len(w)):
                if w[i] > 0 and lo < t[i] <= hi and ok(hr[i]) and output[i] > 0:
                    num += w[i] * (output[i] / hr[i])
                    den += w[i]
            return num / den if den else None
        start = 600 if args.kind == "bike" else 0
        mid = start + (t[-1] - start) / 2
        e1, e2 = half_eff(start, mid), half_eff(mid, t[-1])
        return round((e1 / e2 - 1) * 100, 2) if e1 and e2 else None

    vel = s.get("velocity_smooth")
    output = watts or vel
    if output and w1 and w2:
        d = decoupling(output)
        if d is not None:
            out["decoupling_pct"] = d
        # The benchmark timeline's two decoupling figures, each over an
        # EXPLICIT output: Pa:HR from velocity, Pw:HR from watts. A run
        # with device power has both; decoupling_pct above is the watts one
        # on such a file, and a reader comparing the two must know which.
        if vel:
            d = decoupling(vel)
            if d is not None:
                out["decoupling_pa_pct"] = d
        if watts:
            d = decoupling(watts)
            if d is not None:
                out["decoupling_pw_pct"] = d

    # The final twenty minutes: mean valid HR and mean velocity over
    # samples with t > elapsed - 1200, each keeping its own weight. The LT
    # field test's number is this over its effort; over a whole file it is
    # what the gate pins.
    f_num = f_den = 0.0
    v_num = v_den = 0.0
    for i in range(1, len(w)):
        if w[i] > 0 and t[i] > t[-1] - 1200:
            if ok(hr[i]):
                f_num += w[i] * hr[i]; f_den += w[i]
            if vel:
                v_num += w[i] * vel[i]; v_den += w[i]
    final = {}
    if f_den:
        final["hr"] = round(f_num / f_den, 1)
    if v_den:
        final["vel"] = round(v_num / v_den, 3)
    if final:
        out["final_20min"] = final

    cad = s.get("cadence")
    if cad:
        _, avg_c = weighted(w, cad, lambda v: v > 0)
        if avg_c:
            out["cadence"] = round(avg_c * (2 if args.kind == "run" else 1), 1)

    if args.kind == "run":
        cap = anchors["gradeCap"]
        out["grade_input"] = {
            "under_grade_cap_share": round(share(w, hr, lambda v: v <= cap, ok), 4),
            "grade_cap": cap,
        }
        f_tot = f_sum = 0.0
        for i in range(1, len(w)):
            if w[i] > 0 and t[i] <= 1200 and ok(hr[i]):
                f_tot += w[i]; f_sum += w[i] * hr[i]
        if f_tot:
            out["first_20min"] = {"avg": round(f_sum / f_tot, 1), "cap": anchors["firstMin"]}
    else:
        lo, hi, cap = anchors["bikeLo"], anchors["bikeHi"], anchors["bikeCap"]
        aw_num = aw_den = 0.0
        for i in range(1, len(w)):
            if w[i] > 0 and t[i] > 600 and ok(hr[i]):
                aw_den += w[i]
                if lo <= hr[i] <= hi:
                    aw_num += w[i]
        out["grade_input"] = {
            "in_band_share_after_warmup": round(aw_num / aw_den, 4) if aw_den else None,
            "band": [lo, hi],
            "cap": cap,
            "secs_over_cap": round(share(w, hr, lambda v: v > cap, ok) * kept_secs),
        }

    json.dump(out, sys.stdout, indent=1)
    print()

if __name__ == "__main__":
    main()
