# Authoring a plan

Everything the app serves lives in four kinds of JSON file. This is how to write
them, and what the loader will refuse.

The short version: edit `app/data/`, run `make validate`, then
`make push-data` for a data-only change or `make deploy` if you touched
code. Nothing here needs an image build.

```
app/data/
  athlete.json          who the plan is for, and what they are carrying
  blocks/<id>.json      one training block
  library/movements.json   exercise guides
  library/sessions.json    session and benchmark guides
  library/tasks.json       daily and strength guides
  library/index.json       the workouts page layout
  entries.jsonl         the log. NOT yours to write. Never rewrite it.
```

**A directory is wholly the volume's or wholly the defaults'.** Put one file in
`blocks/` and the embedded example blocks vanish; same for `library/`. Merging
them file-by-file let example guides leak in beside real ones and made which
definition won depend on the alphabet.

**Start by scaffolding**, which gets the rules right that are otherwise only
discoverable by tripping over them:

```sh
cd app
make new-block ID=2027-spring-5k START=2027-03-01 WEEKS=8
```

It refuses a start that is not a Monday and names the two nearest ones, numbers
the weeks, gives every day a kind and a label, stubs whatever the shared library
demands (below), and runs `make validate` so you begin from something that
loads. `WEEKS` defaults to 16; the block length is data, so 4 and 8 are fine.

To start from a whole worked plan instead, copy `app/defaults/` — a complete,
valid, deliberately *different* one: a two-week block, a metric athlete, an
Achilles rather than a calf. It is the better template precisely because it is
not the real athlete's, and copying it into an empty volume serves every page.

> **The library is shared, so guides impose requirements on every block.**
> A guide saying `{{var "easyBand"}}` obliges *every* block to declare
> `easyBand`; one saying `{{phase "calf"}}` obliges every block to declare calf
> phases covering every week. The failure surfaces as a template error deep
> inside the guide rather than in the block that is actually missing something,
> which is why the scaffold scans for these and stubs them. Prefer keeping
> block-specific wording in the block.

---

## athlete.json

```json
{
  "schema": "athlete/1",
  "name": "Example Runner",
  "place": "Minneapolis",
  "units": "imperial",
  "timezone": "America/Chicago",
  "weight": "77.1 kg",
  "hr":    { "easyCap": 155, "firstMin": 145 },
  "power": { "ftp": 214 },
  "paces": { "dec": "9:45/mi" },
  "notes": { "anything": "free text a template can reach" }
}
```

| field | | |
|---|---|---|
| `schema` | required | `athlete/1` |
| `name` | required | shown in the app |
| `place` | optional | kept from the retired plan document's masthead; unused by the app |
| `units` | required | `imperial` or `metric` — flips the whole app |
| `timezone` | required | IANA name. Decides what "today" is |
| `weight` | optional | needed by `wkg`; a string with its unit |
| `hr` `power` `paces` `notes` | optional | open maps, reachable from any template |

The maps are open on purpose. `hr.easyCap` is not a field on a struct — it is
whatever you put there, reached as `{{.Athlete.HR.easyCap}}`. Add
`hr.marathonCap` and it works immediately. **A typo is a startup failure**, not
a blank page in November, because every template is resolved at load time.

`app` (all optional, all defaulted) is the product identity — the name under the
home-screen icon, the colour behind the status bar:

```json
"app": { "name": "Run Coach", "short": "Run Coach",
         "description": "Training block, daily checklist and injury log",
         "theme": { "light": "#F2F2EF", "dark": "#12171B" } }
```

This is **not** the block's name. The header brand is the current block; the app
name outlives it. Theme colours must be six-digit hex.

### issues

An issue is an injury under active management: the thing rated daily, and the
thing the block is rehabbing.

```json
"issues": [{
  "key": "calf",
  "name": "Left calf",
  "short": "Calf",
  "ask": "How much does it hurt right now?",
  "hint": "rate once in the morning, standing, before coffee",
  "guide": "task-calf-check",
  "protocol": "Calf & Glute Protocol",
  "retest": "DEC",
  "scale": { "min": 0, "max": 10, "low": "nothing", "high": "worst imaginable" },
  "bands": [
    { "upto": 2, "tone": "go",      "label": "Go",
      "when": "Quiet, or mild awareness that disappears in the first mile.",
      "action": "*Train as written.* Full protocol, strides and hills included." },
    { "upto": 5, "tone": "caution", "label": "Caution", "when": "…", "action": "…" },
    {            "tone": "stop",    "label": "Stop",    "when": "…", "action": "…" }
  ]
}]
```

- **`key` is a log key.** Once anything is recorded against it, it can never
  change — the log is append-only and is a health record.
- **Bands must ascend, must not overlap, and the last must omit `upto`** so it
  catches the top of the scale. A rating that lands in no band is a number with
  no instruction attached, which is worse than no bands at all.
- **`tone` is a closed set** — `go`, `caution`, `stop`. The colours were
  CVD-validated once; a data file does not get to invent a fourth.
- `when` describes what the rating feels like; `action` says what to do. The
  app's card shows the action (you have already given the rating); `when` is
  the scale's written definition and keeps the bands meaningful.
- `retest` names a benchmark tag; retest weeks derive from the sessions
  carrying it.
- Scales are capped at 21 points, because a rating is a row of buttons on a
  phone and beyond that it stops being one.

---

## blocks/&lt;id&gt;.json

```json
{
  "schema": "block/1",
  "id": "2026-08-16-week-build",
  "name": "16-Week Build",
  "title": "The Sixteen\nWeek Build",
  "note": "4 run days · structured Zwift",
  "start": "2026-08-03",
  "goal": { "event": "5K", "date": "2026-11-21", "target": "21:20" },
  "intro": "…the calendar page's standfirst…",
  "weeks": [ … ]
}
```

`name` is the header brand and the block index. `title` and `note` are
editorial display forms (they fed the retired plan document's masthead) —
authored here beside the dates that have to agree with them. A newline in
`title` is a line break.

**`start` must be a Monday.** The loader refuses anything else, because every
date in the app is `start + 7·(week−1) + weekday`.

`archived: true` takes a block out of the running for "current" regardless of
its dates. That is for the block abandoned halfway rather than finished — dates
alone cannot express that.

### weeks and sessions

Weeks are numbered `1..n` with no gaps and **exactly seven days each**, Monday
first. Length is whatever you write: four weeks, eight, sixteen.

```json
{ "n": 11,
  "tags": ["peak"],
  "days": [
    { "kind": "bike_easy", "label": "Z2 endurance", "mins": 55 },
    { "kind": "quality", "label": "4×1200 @5:00 + 4×200 @0:48 — peak", "dist": "9.5 mi",
      "detail": "2 WU + *4×1200 @5:00* (4′ jog) + 4×200 @0:48 + 1.5 CD" },
    … five more …
  ] }
```

| field | |
|---|---|
| `kind` | required — see below |
| `label` | required — the short form a calendar cell has room for |
| `dist` | a string with its unit: `"9.5 mi"`, `"12 km"`, `"400 m"` |
| `mins` | whole minutes, for sessions where duration is the number |
| `tag` | benchmark id: `FTP` `LT` `DEC` `TT` `RACE` — yours to name |
| `detail` | the full prescription, when it says more than the label |

**Weekly mileage, week start dates and the calendar's date ranges are derived
from the sessions.** Never write them down; they cannot then disagree.

**Kinds** are `rest` `easy` `long` `recovery` `quality` `bike_easy` `bike_hard`.
Two things follow from a kind, so inventing one has consequences:

- `easy` `long` `recovery` `quality` count toward weekly volume. Nothing else does.
- A non-rest session needs a guide named `s-<kind>` in the library (a tagged
  session wants `t-<tag>` instead, which wins). The loader checks this.

**Week `tags`** — `down` `peak` `test` `taper` `race` — are authored: `peak` and `race`
could be derived, but nothing in a week's data separates a down week from a
taper, and guessing from falling volume tags the last three weeks of a build
wrongly. The loader does not check them; `TestWeekTagsAreAuthoredAndConsistent`
does, asserting `race` sits on the week holding the race and `peak` on a week at
the block's maximum.

### steps

A run or bike day may add `steps` — the same session as typed durations and
targets, served at `/fit/<date>` as a Garmin workout file (`/fit.zip`
bundles the whole block's). A bike day's file says `sport=cycling`, and its
power targets are what the watch drives a paired trainer to in ERG. Bike
days are also served as Zwift custom workouts at `/zwo/<date>` (`/zwo.zip`
for the block) — the same resolved steps in Zwift's dialect, whose targets
are *fractions of Zwift's own FTP setting*: every generated file says so,
and Zwift must be set to the plan's FTP or the percentages land wrong.

**Bike steps are timed and power-shaped**: no `dist`, no `pace`, and a
`power` band instead — `[lo, hi]` watts, numbers or templates, **encoded as
absolute watts** so the plan's measured FTP stays the source of truth
regardless of the watch's own FTP setting. `{{pct 62 .Athlete.Power.ftp}}`
is the idiom for %-of-FTP bands; a literal wattage from a label is
transcribed as written (and re-authored when FTP moves, like the DEC
bands). A bike day's timed total is reconciled against `mins` exactly as a
run day's metres are against `dist` — the watch ends a trainer workout when
its steps do. Power on a run step refuses to load.

```json
"steps": [
  { "role": "warmup", "dist": "2 mi",
    "hr": ["{{.Athlete.HR.easyLo}}", "{{.Athlete.HR.easyCap}}"] },
  { "repeat": 4, "steps": [
    { "role": "active", "time": "5:00", "pace": ["7:41/mi", "7:51/mi"] },
    { "role": "recovery", "time": "1:00" } ] },
  { "role": "cooldown", "dist": "1 mi" }
]
```

| field | |
|---|---|
| `role` | required, **literal** — `warmup` `active` `recovery` `cooldown` `rest`. FIT's intensity enum is the label the watch displays, so the role *is* the on-watch text |
| `dist` / `time` | **exactly one.** `dist` takes the usual grammar; `time` is a clock string — `"0:20"`, `"3:00"`, `"75:00"`, `"1:15:00"` — and **the colon is required**: a bare `"180"` would be read as seconds |
| `pace` / `hr` | **at most one.** `pace` is two strings, **fast first** (the `paceRange` convention — the FIT inversion is the encoder's job); `hr` is two bpm bounds, **low first**, bare numbers or templates |
| `note` | optional. The only prescription text the watch shows; ≤ 200 chars after emphasis-stripping |
| `repeat` + `steps` | a repeat instead of a leaf: `repeat ≥ 2` over a body of leaves. Repeats cannot nest |

**Every string except `role` is a template.** The pace-anchor idiom is
`{{pace .Athlete.Paces.threshold}}` — it renders `4:30/km`, which the parser
then reads. A raw `{{.Athlete.Paces.threshold}}` prints the canonical float
and fails at load. Anchor-derived paces round to whole seconds in the
athlete's *display* units, so flipping `units` legitimately moves the FIT
bytes (and the data rev, and every ETag) by a few mm/s.

A timed `active` of 2:00 or longer must carry a `pace` or `hr` target — the
watch has no time-remaining gauge for an untargeted rep. Strides under 2:00
are exempt, and distance steps always show distance remaining.

**The on-watch name derives from the day**: `W06 Tu ` + the label,
emphasis-stripped, transliterated to ASCII, capped at 24 characters at a word
boundary. The watch dedupes imports on the **first 15 characters**, so the
label's head is what the watch shows and what has to tell two sessions apart
— the loader refuses a block where two steps days collide.

Not supported, deliberately: open (press-lap) steps — race day encodes its 5K
as a distance step — and per-session name overrides. If either is ever
wanted, it gets an explicit field rather than a bent form.

### mesocycles, phases, vars

All three attach values to week ranges. **A range is `"1-4"`, `"7"`, or `"14+"`
for open-ended**; an en dash works too.

```json
"mesocycles": [ { "name": "Tissue Prep", "weeks": "1-4" }, … ],

"phases": [
  { "name": "Phase A · tolerance", "weeks": "1-3", "issue": "calf",
    "detail": "Daily, floor only, bodyweight. No step, no load.",
    "goal": "Daily. Build capacity to be loaded at all.",
    "points": ["*Floor only. No step.*", "Both knee positions"] } ],

"vars": {
  "easyBand": [ { "weeks": "1-4",  "value": "{{paceRange \"9:45/mi\" \"11:00/mi\"}}" },
                { "weeks": "5-9",  "value": "…" } ] }
```

**Each of these must cover every week exactly once.** A gap or an overlap is a
load failure — that is the class of bug that renders fine for fifteen weeks and
blank for the sixteenth. Phases are checked **per owner**: an issue's rehab and
the block's own phases are independent series, each total on its own.

A phase with `issue` set belongs to that issue's protocol. `detail` is the line
the app's card shows; `goal` and `points` are the fuller account the retired
protocol document rendered (the loader still accepts them). Guides reach them with `{{phase "calf"}}` and `{{phaseDetail "calf"}}`
— `.Phase` is the block's own, which may be empty.

Vars exist so a value that changes on its own boundaries does not have to share
anyone else's. The pace bands and the calf phases genuinely change on different
weeks; collapsing them into one series put phase B's start at week 5.

### targets, checklist, grading

`targets` are the guidance lines under a session. **Most specific wins: the
session's own `targets`, then its `tag`, then its `kind`** — and a session's
replace rather than add, because the lines they displace are not incomplete,
they are wrong for that session. Eight of this block's long runs finish at
marathon or threshold pace, and on those the governing rule inverts: pace is
the input and heart rate is meant to climb through it. Keyed off `long` alone,
the app told him to cap HR and walk when the alarm fired — mid-tempo.

```json
"targets": {
  "kinds": { "easy": ["HR cap {{.Athlete.HR.easyCap}}", "Pace is an output: {{var \"easyBand\"}}"] },
  "tags":  { "DEC":  ["{{dist \"6 mi\"}} at a FIXED {{pace .Athlete.Paces.dec}}"] } }
```

`checklist` is the day's task list. **`key` is a log key** — authored, not
derived from the label, so rewording a task does not orphan its history.
`when` is a template that must render `true`/`yes`/`1` for the row to appear:

```json
{ "key": "strength-a", "label": "Strength A", "guide": "task-strength-a",
  "when": "{{and .InBlock (eq .Kind \"quality\")}}" }
```

There is no condition language; `when` is just a template. Predicates must
survive a day with **no session at all** — outside the block `.Session` is nil,
which is what `.Kind`, `.Tag` and `.Resting` are for.

`grading` is the calendar legend: a `note`, `bands` of `{grade, range}`, and a
`footer`. Colours stay in the stylesheet.

---

## library/

Guides are what a popup shows. Keyed by id; the id prefix decides the role:

| prefix | |
|---|---|
| `m-` | a movement — the only prefix the *code* tests for. Give it a `group` and it joins the exercise library |
| `s-` | a session kind. A session with no tag looks for `s-<kind>` |
| `t-` | a benchmark. A tagged session looks for `t-<tag>`, which wins over the kind |
| `task-` | everything else — daily and strength guides, named by a checklist row or a step. Convention only |

```json
"m-iso": {
  "id": "m-iso",
  "title": "Bent-knee isometric hold, single leg",
  "summary": "one line for the workouts index",
  "sections": [ { "label": "Set up", "text": "…" }, { "label": "Do", "text": "…" } ],
  "why": "Isometrics load a tendon hard without moving it.",
  "video": "https://…", "videoTitle": "…",
  "article": "https://…", "articleTitle": "…",
  "source": "Nottingham Physio",
  "group": "Calf", "order": 1,
  "match": "bent-knee isometric"
}
```

`group` and `order` place it in the movement library. **Order is the taught
progression, not the alphabet** — calf runs isometric → double-leg → single-leg
→ loaded → plyometric, and sorting by title destroyed that once. `order` 0 sorts
last and falls back to the title, so a new exercise appears without editing
anything else. `videoTitle`, `articleTitle` and `match` were used only by the retired
protocol document; the loader still accepts them.

**Steps** are a guide's checklist:

```json
"steps": [ { "text": "5 × 45 s bent-knee isometric hold", "guide": "m-iso",
             "sets": 5, "key": "task-calf#iso" } ]
```

`key` is the log identity, defaulting to `<guide id>#<n>`. **Set it explicitly
on any guide whose step list varies by week**, or a movement's identity shifts
as the list around it grows. `sets` above one gives a box per set, so a session
cut short after two of three records what actually happened.

**Variants** let one guide differ by week. A variant field *replaces* the base's
— it never merges, because a merge rule nobody can predict is worse than a
little duplication:

```json
"variants": [ { "weeks": "1-3", "sections": [ … ], "steps": [ … ] } ]
```

### library/index.json

The workouts page layout. A group has `rows`, or a `movements` group name that fills itself:

```json
{ "schema": "index/1",
  "groups": [ { "title": "Sessions", "rows": [ { "guide": "s-easy", "when": "{{count \"easy\"}}×" } ] },
              { "title": "Calf", "movements": "Calf" } ] }
```

A `movements` group picks up every guide carrying that `group`, in `order`. Add
an exercise to the library and it appears in both places without touching this.

---

## The template language

**Every human-readable string in every data file is a Go template**, resolved
against `{.Athlete .Block .Week .Phase .Meso .Session}` plus `.InBlock`, and the
nil-safe `.Kind`, `.Tag`, `.Resting`.

| helper | |
|---|---|
| `dist` `pace` `wt` | render a quantity in the athlete's units |
| `paceRange lo hi` | one shared unit suffix: `9:45–11:00/mi` |
| `kg` | force kilograms, where the metric unit *is* the concept |
| `pct n of` | a percentage of a value: `{{pct 62 .Athlete.Power.ftp}}` |
| `wkg n` | W/kg → watts for this athlete's bodyweight |
| `var "name"` | a week-ranged block variable |
| `count "kind"` | how many times a kind appears in the block |
| `dates "TAG"` | the dates a benchmark falls on |
| `perkg n` | watts → W/kg for this athlete |
| `pctBW w` | a load as a share of bodyweight: `{{pctBW "50 lb"}}` → `29%` |
| `bwPlus w` | bodyweight carrying a load |
| `phase "issue"` `phaseDetail "issue"` | an issue's current rehab stage |
| `sessionGuide` | today's popup id |
| `lower` `upper` `trim` | |

Each takes either a literal carrying its unit (`{{pace "9:45/mi"}}`) or a
quantity reached through the context (`{{pace .Athlete.Paces.dec}}`), so you
never have to know which you are holding.

**Quantities are strings carrying their own unit** — `"7 mi"`, `"9:45/mi"`,
`"25 lb"`. Stored canonically (metres, seconds per metre, kilograms) and
converted on the way out, so flipping `units` re-renders the whole app.
Distances take `mi` `km` `m`; paces `M:SS/mi` or `M:SS/km`; weights `lb` `kg`.

**Never hand-compute a number that derives from the athlete.** `50 lb is 31% of
bodyweight` was written out once and was wrong in two places the moment the
scale moved. Use `pct`, `wkg`, or add a helper.

**Emphasis is data, not markup.** `*strong*`, `_em_` and a newline are the only
three things a data file may say about presentation. Anything else is escaped —
a data file cannot inject markup into the page.

---

## What the loader refuses

Startup validation is deliberately fatal: the container restart-loops rather
than serve a half-right plan, because silently serving someone else's is worse
than being visibly down.

- a `schema` this build does not understand
- a `start` that is not a Monday; a week with other than seven days; a week
  numbered out of order; a session with no `kind` or no `label`
- phases, mesocycles or vars that leave a week uncovered or covered twice
- two blocks sharing an `id`; two issues sharing a `key`
- a checklist row with no `key`
- a guide id that does not exist: a session's `s-`/`t-` guide, a checklist
  `guide`, a step's `guide`, an index row, an issue's `guide`
- a phase naming an issue nobody declared
- bands that do not ascend, overlap, use an unknown tone, or leave the top of
  the scale uncovered
- steps on a non-run kind; a step that mixes repeat and leaf fields; a repeat
  under 2 or with an empty body; a nested repeat; a `role` missing, templated
  or unknown; both `dist` and `time` or neither; `pace` and `hr` together; a
  band without exactly two ends
- a slow-first `pace` band; an `hr` band high-first or outside 30–250 bpm; a
  colonless or out-of-range `time`; an untargeted timed `active` of 2:00+; a
  `note` over 200 chars; any character the FIT transliteration cannot map
- more than 50 emitted FIT steps; steps measuring over 2% beyond the
  session's `dist`; two steps days whose on-watch names share their first 15
  characters
- **any template that fails to render, for any week and any day of the block**

That last one is the important one. Every string is resolved for all 7·n
day-contexts at load. A typo in a field name is a startup failure, not a 500 on
the day that week comes round.

### and what it cannot see

`encoding/json` **drops an unknown key silently**. `"lable"` is not an error; it
is a session whose label went in the bin on the way past, and then fails the
loader's own check for a reason two steps removed from the typo. So the shape is
checked separately, against the struct fields themselves:

```sh
make validate       # shape, then meaning — runs check-schema first
make check-schema   # shape only
make schema         # rewrite app/schema/*.json after changing a Go struct
```

`app/schema/*.json` are JSON Schema files for editor autocomplete. They are
**generated from the Go types**, and a test fails if they drift — a schema
maintained separately from the struct is one more pair of things that can
disagree. All of this lives in `_test.go` files, which `SRC_HASH` excludes:
none of it reaches the image, and no schema file can become something the
running container needs in order to start.

---

## Publishing

Retired 13 Aug 2026. The plan and strength-protocol documents were an export
of this data with generated regions; the app fully replaced them and the
generator went too (git history holds both). The rule that outlives them:
**never hand-copy a derived figure anywhere** — resolve it from the data or
don't state it.

---

## Shipping

```sh
make new-block ID=… START=… [WEEKS=16]   # scaffold; validates what it wrote
make validate      # always. shape (check-schema) then meaning
make test          # if code changed. validate is not test; run both
make push-data     # data only — live in 30 s, no rebuild, no restart
make deploy        # code — builds arm64, ships, restarts, pushes data, verifies
make verify        # confirm the live site serves this build AND this plan
```

`/healthz` reports two revisions, `build` and `data`. They move independently
and either can be stale, which is why `verify` checks both.

**`entries.jsonl` is not yours.** It is the athlete's health log, it exists
only on the server, it is append-only, and corrections are appended rather than edited. Read it
with `make fetch-log`.
