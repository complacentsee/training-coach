"""A recorded activity .fit file, converted to the streams JSON that
tools/grade_metrics.py reads. This owns shape and unit conversion only —
every measurement convention (time-weighting, the dropout rule, run-cadence
doubling) lives in grade_metrics.py's docstring and is not restated here.
Decoding is fitdecode (python3 -m pip install fitdecode) with CRC checking
enforced: a corrupt file is a refusal, not a stream.

    python3 tools/fit_streams.py activity.fit > s.json
    python3 tools/fit_streams.py --describe --tz America/Chicago activity.fit

--describe summarises instead: sport, sub_sport, start_utc (and start_local
when --tz names an IANA zone), elapsed seconds, record count.

Conversion conventions — change them here or not at all:
- time[i] is whole seconds from the first record's timestamp. Recording gaps
  (auto-pause) are left in, deliberately; Strava compacts them out, so a
  delta against Strava's streams for the same activity is expected and is a
  known cross-check item.
- heart_rate is bpm from the record, emitted only when some record carries a
  valid sample (so a strapless activity gets grade_metrics.py's own "no
  heart_rate stream" refusal, not a wall of zeros); within the stream a
  missing or invalid sample (None, 255) becomes 0, which the dropout rule
  excludes and reports.
- watts comes from record `power`, emitted only when some record carries
  power; a missing sample within a power stream becomes 0.
- velocity_smooth is `enhanced_speed`, falling back to `speed`, m/s floats.
- cadence is exactly as recorded, integers, never pre-doubled — run records
  are per-leg and grade_metrics.py owns the doubling.
- Ints stay ints in the JSON. Developer fields are ignored.
"""
import argparse, json, sys, zoneinfo

try:
    import fitdecode
except ImportError:
    sys.exit("fitdecode is required: python3 -m pip install fitdecode")

def read_activity(path):
    """(records carrying a timestamp, {sport, sub_sport}) with CRC enforced."""
    records, sport = [], {}
    try:
        with fitdecode.FitReader(path, check_crc=fitdecode.CrcCheck.RAISE) as f:
            for frame in f:
                if not isinstance(frame, fitdecode.FitDataMessage):
                    continue
                if frame.name == "record":
                    if frame.get_value("timestamp", fallback=None) is not None:
                        records.append(frame)
                elif frame.name in ("session", "sport"):
                    for key in ("sport", "sub_sport"):
                        v = frame.get_value(key, fallback=None)
                        if v is not None:
                            sport[key] = v
    except (fitdecode.FitError, OSError) as e:
        sys.exit(f"could not decode {path}: {e}")
    if not records:
        sys.exit(f"no record messages with a timestamp in {path}")
    return records, sport

def streams(records):
    ts0 = records[0].get_value("timestamp")
    time, hr = [], []
    watts, vel, cad = [], [], []
    have_h = have_w = have_v = have_c = False
    for r in records:
        time.append(int(round((r.get_value("timestamp") - ts0).total_seconds())))
        h = r.get_value("heart_rate", fallback=None)
        have_h = have_h or (h is not None and h != 255)
        hr.append(0 if h is None or h == 255 else int(h))
        w = r.get_value("power", fallback=None)
        have_w = have_w or w is not None
        watts.append(0 if w is None else int(w))
        v = r.get_value("enhanced_speed", fallback=None)
        if v is None:
            v = r.get_value("speed", fallback=None)
        have_v = have_v or v is not None
        vel.append(0.0 if v is None else float(v))
        c = r.get_value("cadence", fallback=None)
        have_c = have_c or c is not None
        cad.append(0 if c is None else int(c))
    out = {"time": time}
    if have_h:
        out["heart_rate"] = hr
    if have_w:
        out["watts"] = watts
    if have_v:
        out["velocity_smooth"] = vel
    if have_c:
        out["cadence"] = cad
    return out

def describe(records, sport, tz):
    ts0 = records[0].get_value("timestamp")
    out = {
        "sport": sport.get("sport"),
        "sub_sport": sport.get("sub_sport"),
        "start_utc": ts0.isoformat(),
    }
    if tz:
        try:
            out["start_local"] = ts0.astimezone(zoneinfo.ZoneInfo(tz)).isoformat()
        except zoneinfo.ZoneInfoNotFoundError:
            sys.exit(f"unknown timezone: {tz}")
    ts1 = records[-1].get_value("timestamp")
    out["elapsed"] = int(round((ts1 - ts0).total_seconds()))
    out["records"] = len(records)
    return out

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("fit", help="recorded activity .fit file")
    ap.add_argument("--describe", action="store_true",
                    help="summarise the activity instead of emitting streams")
    ap.add_argument("--tz", help="IANA zone for --describe's start_local")
    args = ap.parse_args()

    records, sport = read_activity(args.fit)
    if args.describe:
        json.dump(describe(records, sport, args.tz), sys.stdout, indent=1)
    else:
        json.dump(streams(records), sys.stdout)
    print()

if __name__ == "__main__":
    main()
