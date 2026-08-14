# Running Coach

A self-hosted training dashboard: a training block as plain JSON, rendered as
a calendar with daily guidance, workout guides, injury tracking, and grades —
plus structured-workout export to a Garmin watch and an activity archive
pulled back off it, both over WebUSB straight from the browser.

One static Go binary (stdlib only, `FROM scratch` image, ~8 MB). Templates
and assets are compiled in; the plan is not — it lives on a mounted volume,
so a new block goes live with a file copy and a reload, no rebuild.

## What it does

- **The plan is data.** An athlete file (units, timezone, HR/power/pace
  anchors) plus one JSON file per training block. Every human-readable string
  is a template resolved against the athlete, so flipping units or retesting
  FTP re-renders everything derived from them. A data error is a startup
  failure, not a surprise in week eleven.
- **Calendar, today view, workout guides, checklists** — with an
  append-only JSONL event log for checkoffs, injury ratings, and grades.
- **FIT workout export**: structured sessions served as Garmin workout files
  (and Zwift `.zwo` for trainer rides), sent to the watch from the `/watch`
  page over WebUSB MTP — a hand-rolled initiator, no drivers, no Garmin
  software.
- **Activity archive**: the same page pulls recorded activities off the
  watch; the server stores and serves the raw bytes, never decodes them, and
  the archive is invisible to the data revision by construction.
  `tools/fit_streams.py` turns a stored FIT into analysis-ready streams.

## Quickstart

```sh
cd app
make run        # serves the embedded example athlete on :8080
make test
```

The repo ships a complete example under `app/defaults/` — a generic athlete
with a two-week block. Your real plan goes in `app/data/` (gitignored; the
same shape — see `AUTHORING.md`), and deployment coordinates go in
`app/local.mk`. `make` lists the targets; `make deploy` builds for arm64,
ships over ssh, and verifies both the build and the data revision landed.

## Writing a plan

`AUTHORING.md` documents every field and what the loader refuses.
`tools/new-athlete.md` is an interview prompt that ends in a validated
athlete file and block.
