# Running Coach

A self-hosted dashboard for running a structured training block. You write
the plan once as plain JSON — sessions, targets, heart-rate caps, an injury
protocol if you're managing one — and the app turns it into a daily
coaching surface: what to do today and why, the whole block on a calendar,
a guide for every workout, checkoffs, injury ratings, and per-session
grades. It also speaks Garmin: structured workouts export straight to a
watch over WebUSB from the browser, and recorded activities pull back off
it into a permanent archive on your own server.

**Use it if** you (or someone you coach) train from a written plan and want
it to live somewhere better than a spreadsheet or a paid platform: your
hardware, your data, plain files, no accounts.

## What it looks like

Today — the one page that answers "what am I doing and how should it feel":

![The today view: session card with targets, checklist, injury rating, feedback](docs/today.png)

The whole block, with weekly volume derived from the sessions (never
retyped) and grades filling in as you train:

![The calendar: every week and day of the block, FIT export pills on structured days](docs/calendar.png)

Every session and movement the plan prescribes, each opening into a guide:

![The workouts page: sessions, daily work, and movement guides](docs/workouts.png)

## How it works

- **The plan is data.** One athlete file (units, timezone, HR/power/pace
  anchors, an optional injury declaration) plus one JSON file per block.
  Every human-readable string is a template resolved against the athlete —
  flip `"units"` to metric or retest your FTP and everything derived
  re-renders. A data error is a startup failure, not a surprise in week
  eleven.
- **The log is append-only.** Checkoffs, injury ratings, feedback, and
  grades are events in a JSONL file; state is a replay. Nothing is ever
  rewritten.
- **Watch sync, no vendor software.** The `/watch` page is a hand-rolled
  MTP-over-WebUSB client (Chrome/Edge, HTTPS): it sends structured workout
  FIT files to the watch and pulls recorded activities off it. The server
  stores activity bytes untouched and never decodes them;
  `tools/fit_streams.py` turns a stored file into analysis-ready streams.
- **One binary.** Go standard library only, templates and assets compiled
  in, shipped as a `FROM scratch` image (~18 MB). The plan is *not* in the
  image — it lives on a mounted volume, so plan changes go live with a file
  copy and a reload, no rebuild.

## Kick it off

With Go installed:

```sh
cd app
make run     # http://localhost:8080 — serves the built-in example athlete
make test
```

The repo ships a complete example under `app/defaults/` — a generic athlete
with a two-week base block and an injury protocol — which is exactly what
the screenshots above show. The moment you put your own files in `app/data/`
(gitignored), the example steps aside.

## Build the Docker image

```sh
cd app
docker build --build-arg SRC_HASH=$(git rev-parse --short HEAD) -t running-coach .
docker run -d -p 8080:8080 -v "$PWD/data:/data" running-coach
```

The build refuses to produce an image whose binary links anything beyond
the standard library (`go version -m` must show no `dep` line — the one
module in go.mod is a test-only FIT decoder). An empty `/data` volume
serves the example athlete; a populated one serves your plan. `compose.yml`
is the running configuration: restart policy, port, timezone, and the
single bind mount that holds everything worth keeping — the plan, the log,
and the activity archive.

## Run your own plan

1. Read `AUTHORING.md` — every field, and everything the loader refuses
   (it validates shape *and* meaning at startup, deliberately fatally).
2. Or let an LLM interview you: `tools/new-athlete.md` is a prompt that
   ends in a validated athlete file and first block.
3. Put the files in `app/data/`, then `make validate` and `make run`.
4. To deploy to a server: put your host/ssh coordinates in `app/local.mk`
   (gitignored), then `make deploy` — it builds for arm64, ships the image
   over ssh without a registry, pushes the plan, restarts, and verifies
   that the live site serves exactly this build *and* this data.
