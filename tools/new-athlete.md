# Prompt: interview an athlete and write their plan

Give this to Claude in a fresh session in this repo. It produces
`app/data/athlete.json` and one block under `app/data/blocks/`.

Read `AUTHORING.md` first — it is the schema, and this prompt does not repeat it.

---

You are setting up this app for a new athlete. Interview them, then write their
data files. Work in this order and do not skip ahead: **the block cannot be
written until the anchors exist**, because every prescription in it is expressed
against them.

## 1. Before you ask anything

Read `AUTHORING.md`, then `app/defaults/` — a complete, valid, deliberately
small example. You are going to produce something shaped like that.

Ask for a data export if one exists (Strava, Garmin, Coros). **Numbers you can
measure beat numbers they estimate**, and the gap between the two is often the
whole diagnosis. If they have 12+ months of history, read it before the
interview and bring findings to it rather than questions you could have answered
yourself.

## 2. The interview

Ask these in conversation, not as a form. Follow the interesting answers.

**Who and where.** Name, the city they train in, timezone, units. Units decide
how every quantity in the app renders; get it wrong and the whole thing reads
foreign.

**The goal.** What event, what date, what time. If they say "get faster", find
the event — a block without a date has no taper and no end. Then ask what they
have actually run recently, and over what distance. Two marks at different
distances are worth far more than one, because the *shape* of the curve is
diagnostic.

**Constraints, honestly.** How many days a week will they genuinely train — not
aspire to. What else is in the week (a bike, a gym, a commute). How long is the
longest session that fits. A plan that assumes six days from someone with four
fails in week three and takes the whole block with it.

**What hurts.** Anything currently sore, anything that recurs, anything they are
working back from. Get specific: which side, what it feels like, what makes it
worse, how long it has been there. This becomes an `issue` — and if there is one,
it also becomes the block's rehab phases. Ask what a bad day feels like versus a
good one; that is the band descriptions written for you.

**Anchors.** Max HR and how they know it. Threshold, FTP, reference paces —
measured or estimated, and say which. Bodyweight, if anything derives from it.
**Record what was measured and when.** A number nobody measured will be treated
as fact by everything downstream.

## 3. Write athlete.json

Anchors go in the open maps (`hr`, `power`, `paces`, `notes`) under whatever
names the block will read them by. There is no fixed set — `hr.easyCap` is a
field because someone wrote it there.

Declare an `issue` for anything they are managing. The `key` is a log key and
can never change. Bands must ascend and the last must catch the top of the
scale; write `when` from what they told you a bad day feels like, and `action`
as an instruction they could follow at 6am without thinking.

Quantities carry their own unit: `"77.1 kg"`, `"9:45/mi"`. Never a bare number.

## 4. Scaffold the block

```sh
cd app && make new-block ID=2027-spring-5k START=2027-03-01 WEEKS=12
```

It gets the Monday start right, and stubs whatever the shared guide library
demands of a block — the vars and issue phases it references. It then runs
`make validate`, so you start from something that loads.

Then edit it. **Do not retype numbers you can derive**: weekly mileage, dates
and week ranges all come from the sessions. If you are computing a percentage of
bodyweight or a share of FTP by hand, stop and use `pct` or `wkg` — a
hand-computed figure is wrong the moment the input moves, and that has already
happened twice in this repo.

Order of work inside the block:

1. The **weekly skeleton** — which days are which kind. Get this right first;
   everything else hangs off it.
2. **Mesocycles**, then **phases** if there is an issue. Each must cover every
   week exactly once, and phases are checked per issue.
3. **Vars** for anything that changes on its own boundaries — pace bands
   typically move on different weeks from the training blocks, which is the
   whole reason vars exist.
4. **Targets** per kind and per benchmark tag.
5. The **checklist**. Keys are log keys; author them, do not derive them from
   the labels.
6. **Sessions** — `label` is the short form a calendar cell fits, `detail` is
   the full prescription where there is one.

## 5. Check the library

Guides are shared across blocks. If this athlete needs a session kind the
library has no `s-<kind>` guide for, write it. If they need an exercise that is
not there, add it with a `group` and an `order`.

Anything you add that references `{{var "x"}}` or `{{phase "y"}}` becomes a
requirement on **every** block, including ones written later. Prefer putting
block-specific wording in the block.

## 6. Prove it

```sh
cd app
make validate        # shape, then meaning: refuses gaps, overlaps, bad refs
make test            # the suite, including the unknown-field check
make artifacts       # regenerate the published documents
make check-artifacts # and prove they agree with the data
make run             # look at it: /, /calendar, /week/1, /workouts
```

Read the actual pages before you say it is done. `make validate` proves the data
loads; it does not prove the plan makes sense.

## 7. What not to do

- **Do not touch `entries.jsonl`.** It is a health log, append-only, and it
  lives on the box. Corrections are appended, never edited.
- **Do not write the numbers twice.** If it appears in two places, one of them
  should be deriving it.
- **Do not invent anchors.** If they do not know their max HR, say so in the
  data and plan around not knowing it, rather than picking a plausible number
  that everything downstream then treats as measured.
- **Do not put design rationale in guide copy.** A guide instructs. Why the plan
  is arranged as it is belongs in `CLAUDE.md` or a commit message.
