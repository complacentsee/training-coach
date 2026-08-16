package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
)

/*
fit.go writes Garmin FIT workout files with the standard library alone. The
container framing (definition/data records, CRC-16) was validated as a PoC by
two independent decoders — muktihari/fit with CRC checking, and python
fitdecode in CrcCheck.RAISE mode — before it moved here; muktihari/fit remains
a test-only dependency for round-trip decoding and never reaches the binary.

The error contract, drawn once: the loader owns plan semantics — roles,
budgets, bands, targets, note length, names. Encode guards only what the byte
format itself cannot carry: no steps, an empty or non-ASCII name or note, a
string over 254 bytes, a sentinel serial, a time before the FIT epoch. A
51-step workout encodes successfully — the 50-step budget is the watch's rule,
enforced by the loader, and duplicating it here would just be a second opinion
waiting to disagree.
*/

/* ── the container: records and CRC ────────────────────────────────────── */

// FIT base type codes (protocol spec, base type field).
const (
	fitBaseEnum   byte = 0x00
	fitBaseString byte = 0x07
	fitBaseUint16 byte = 0x84
	fitBaseUint32 byte = 0x86
	// uint32z: zero is the invalid sentinel, so valid values exclude it.
	fitBaseUint32z byte = 0x8C
)

// fitInvalidUint32 is the "field not populated" sentinel for uint32 fields
// that share a definition with messages that do populate them.
const fitInvalidUint32 uint32 = 0xFFFFFFFF

const (
	fitHeaderSize      = 14
	fitProtocolVersion = 0x10 // protocol 1.0: no developer fields used
	fitProfileVersion  = 2194 // profile 21.94
)

// fitCRCTable drives the FIT CRC-16 (the SDK's nibble-at-a-time algorithm).
var fitCRCTable = [16]uint16{
	0x0000, 0xCC01, 0xD801, 0x1400, 0xF001, 0x3C00, 0x2800, 0xE401,
	0xA001, 0x6C00, 0x7800, 0xB401, 0x5000, 0x9C01, 0x8801, 0x4400,
}

// fitCRC16 folds data into a running FIT CRC, low nibble then high nibble.
func fitCRC16(crc uint16, data []byte) uint16 {
	for _, b := range data {
		tmp := fitCRCTable[crc&0x0F]
		crc = (crc >> 4) & 0x0FFF
		crc = crc ^ tmp ^ fitCRCTable[b&0x0F]
		tmp = fitCRCTable[crc&0x0F]
		crc = (crc >> 4) & 0x0FFF
		crc = crc ^ tmp ^ fitCRCTable[(b>>4)&0x0F]
	}
	return crc
}

// A fitField is one field of a data message: profile field number, base type,
// and a concrete value (uint8, uint16, uint32 or string).
type fitField struct {
	Num   byte
	Type  byte
	Value any
}

// size is the encoded byte size; strings are null-terminated so their
// declared size is len+1.
func (f fitField) size() (int, error) {
	switch v := f.Value.(type) {
	case uint8:
		return 1, nil
	case uint16:
		return 2, nil
	case uint32:
		return 4, nil
	case string:
		if len(v)+1 > 255 {
			return 0, fmt.Errorf("field %d: string longer than 254 bytes", f.Num)
		}
		return len(v) + 1, nil
	default:
		return 0, fmt.Errorf("field %d: unsupported value type %T", f.Num, f.Value)
	}
}

// A fitMsg is one FIT data message plus enough to derive its definition.
type fitMsg struct {
	GlobalNum uint16
	Fields    []fitField
}

// signature identifies the definition a data message needs: the
// (number, size, base type) triple of every field, in order.
func (m fitMsg) signature() (string, error) {
	sig := make([]byte, 0, len(m.Fields)*3)
	for _, f := range m.Fields {
		n, err := f.size()
		if err != nil {
			return "", err
		}
		sig = append(sig, f.Num, byte(n), f.Type)
	}
	return string(sig), nil
}

// appendDefinition writes a definition record: header (0x40|local), reserved,
// architecture 0 (little-endian), global number, field count, then one
// (num, size, type) triple per field.
func (m fitMsg) appendDefinition(b []byte, local byte) []byte {
	b = append(b, 0x40|local, 0, 0)
	b = binary.LittleEndian.AppendUint16(b, m.GlobalNum)
	b = append(b, byte(len(m.Fields)))
	for _, f := range m.Fields {
		n, _ := f.size()
		b = append(b, f.Num, byte(n), f.Type)
	}
	return b
}

// appendData writes a data record: header (local message type), then every
// field value in definition order.
func (m fitMsg) appendData(b []byte, local byte) ([]byte, error) {
	b = append(b, local)
	for _, f := range m.Fields {
		switch v := f.Value.(type) {
		case uint8:
			b = append(b, v)
		case uint16:
			b = binary.LittleEndian.AppendUint16(b, v)
		case uint32:
			b = binary.LittleEndian.AppendUint32(b, v)
		case string:
			b = append(append(b, v...), 0)
		default:
			return nil, fmt.Errorf("field %d: unsupported value type %T", f.Num, f.Value)
		}
	}
	return b, nil
}

// fitEncode renders the messages as a complete FIT file: 14-byte header with
// header CRC over its first 12 bytes, the records, and a trailing CRC-16 over
// everything before it (header included). One definition message is emitted
// per distinct (global number, field layout) and reused while the layout
// holds.
func fitEncode(msgs []fitMsg) ([]byte, error) {
	var body []byte
	lastSig := make(map[uint16]string)
	locals := make(map[uint16]byte)

	for _, m := range msgs {
		sig, err := m.signature()
		if err != nil {
			return nil, err
		}
		local, ok := locals[m.GlobalNum]
		if !ok {
			if len(locals) == 16 {
				return nil, fmt.Errorf("more than 16 distinct global message numbers")
			}
			local = byte(len(locals))
			locals[m.GlobalNum] = local
		}
		if lastSig[m.GlobalNum] != sig {
			body = m.appendDefinition(body, local)
			lastSig[m.GlobalNum] = sig
		}
		if body, err = m.appendData(body, local); err != nil {
			return nil, err
		}
	}

	header := make([]byte, fitHeaderSize)
	header[0] = fitHeaderSize
	header[1] = fitProtocolVersion
	binary.LittleEndian.PutUint16(header[2:4], fitProfileVersion)
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(body)))
	copy(header[8:12], ".FIT")
	binary.LittleEndian.PutUint16(header[12:14], fitCRC16(0, header[:12]))

	out := append(header, body...)
	return binary.LittleEndian.AppendUint16(out, fitCRC16(0, out)), nil
}

/* ── the workout profile ───────────────────────────────────────────────── */

// Global message numbers (FIT profile).
const (
	fitMesgFileID      uint16 = 0
	fitMesgWorkout     uint16 = 26
	fitMesgWorkoutStep uint16 = 27
)

// file_id field numbers.
const (
	fitFileIDType         byte = 0
	fitFileIDManufacturer byte = 1
	fitFileIDProduct      byte = 2
	fitFileIDSerial       byte = 3
	fitFileIDTimeCreated  byte = 4
)

// workout field numbers.
const (
	fitWorkoutSport         byte = 4
	fitWorkoutNumValidSteps byte = 6
	fitWorkoutWktName       byte = 8
)

// workout_step field numbers.
const (
	fitStepMessageIndex  byte = 254
	fitStepDurationType  byte = 1
	fitStepDurationValue byte = 2
	fitStepTargetType    byte = 3
	fitStepTargetValue   byte = 4
	fitStepCustomLow     byte = 5
	fitStepCustomHigh    byte = 6
	fitStepIntensity     byte = 7
	fitStepNotes         byte = 8
)

// Profile enum values.
const (
	fitFileWorkout        uint8  = 5
	fitManufacturerGarmin uint16 = 1
	// What Garmin's own workout exports stamp; confirm empirically at
	// rollout — the fallback ladder walks manufacturer/product variants.
	fitProduct uint16 = 65534

	fitSportRunning uint8 = 1
	fitSportCycling uint8 = 2

	fitDurationTime     uint8 = 0
	fitDurationDistance uint8 = 1
	fitDurationOpen     uint8 = 5
	fitDurationRepeat   uint8 = 6 // repeat_until_steps_cmplt

	fitTargetSpeed uint8 = 0
	fitTargetHR    uint8 = 1
	fitTargetOpen  uint8 = 2
	fitTargetPower uint8 = 4
)

// fitSportFor is the one mapping from a session's kind to the file's sport —
// shared by dryRun and both routes, so validation and serving agree.
func fitSportFor(k Kind) uint8 {
	if k.IsBike() {
		return fitSportCycling
	}
	return fitSportRunning
}

// fitIntensities is the closed set of step roles and what each becomes on
// the wire. FIT's intensity enum is the label the watch displays on a running
// workout (wkt_step_name is ignored there), which is why the dialect's roles
// are exactly these five and why the loader checks membership here — a role
// added to the dialect necessarily picks its label.
var fitIntensities = map[string]uint8{
	"active":   0,
	"rest":     1,
	"warmup":   2,
	"cooldown": 3,
	"recovery": 4,
}

// fitEpoch is 1989-12-31T00:00:00Z as a Unix timestamp; FIT date_time counts
// seconds from there.
const fitEpoch = 631065600

// fitTimestamp converts t to a FIT date_time.
func fitTimestamp(t time.Time) uint32 { return uint32(t.Unix() - fitEpoch) }

// A FitStep is one workout_step, already in raw FIT units: DurationValue is
// centimetres (distance, scale 100), milliseconds (time, scale 1000), or the
// message_index of the first repeated step (repeat). CustomLow/High are mm/s
// for a speed target, bpm+100 for a heart-rate target, and fitInvalidUint32
// when there is no custom range. Notes is emitted as field 8 only when
// non-empty.
type FitStep struct {
	Intensity     uint8
	DurationType  uint8
	DurationValue uint32
	TargetType    uint8
	TargetValue   uint32
	CustomLow     uint32
	CustomHigh    uint32
	Notes         string
}

// A FitWorkout is a complete FIT workout file. Identity is an explicit input
// — time.Now() appears nowhere in this file — so identical inputs give
// identical bytes, which is what makes ETags stable and golden tests
// possible.
type FitWorkout struct {
	Name        string
	Sport       uint8
	Serial      uint32 // uint32z: 0 and 0xFFFFFFFF are sentinels — fitIdentity never mints them
	TimeCreated time.Time
	Steps       []FitStep
}

// Encode renders the workout: file_id, workout, one workout_step per step.
// The name is written verbatim — a mutating encoder would falsify the
// loader's view of the name the watch dedupes on.
func (w FitWorkout) Encode() ([]byte, error) {
	if len(w.Steps) == 0 {
		return nil, fmt.Errorf("workout %q has no steps", w.Name)
	}
	if err := fitASCIIField("name", w.Name); err != nil {
		return nil, err
	}
	if w.Serial == 0 || w.Serial == fitInvalidUint32 {
		return nil, fmt.Errorf("serial %#x is a uint32z sentinel", w.Serial)
	}
	if w.TimeCreated.Unix() < fitEpoch {
		return nil, fmt.Errorf("time_created %s predates the FIT epoch", w.TimeCreated.Format(time.RFC3339))
	}
	msgs := []fitMsg{
		{GlobalNum: fitMesgFileID, Fields: []fitField{
			{fitFileIDType, fitBaseEnum, fitFileWorkout},
			{fitFileIDManufacturer, fitBaseUint16, fitManufacturerGarmin},
			{fitFileIDProduct, fitBaseUint16, fitProduct},
			{fitFileIDSerial, fitBaseUint32z, w.Serial},
			{fitFileIDTimeCreated, fitBaseUint32, fitTimestamp(w.TimeCreated)},
		}},
		{GlobalNum: fitMesgWorkout, Fields: []fitField{
			{fitWorkoutWktName, fitBaseString, w.Name},
			{fitWorkoutSport, fitBaseEnum, w.Sport},
			{fitWorkoutNumValidSteps, fitBaseUint16, uint16(len(w.Steps))},
		}},
	}
	for i, s := range w.Steps {
		fields := []fitField{
			{fitStepMessageIndex, fitBaseUint16, uint16(i)},
			{fitStepDurationType, fitBaseEnum, s.DurationType},
			{fitStepDurationValue, fitBaseUint32, s.DurationValue},
			{fitStepTargetType, fitBaseEnum, s.TargetType},
			{fitStepTargetValue, fitBaseUint32, s.TargetValue},
			{fitStepCustomLow, fitBaseUint32, s.CustomLow},
			{fitStepCustomHigh, fitBaseUint32, s.CustomHigh},
			{fitStepIntensity, fitBaseEnum, s.Intensity},
		}
		if s.Notes != "" {
			if err := fitASCIIField(fmt.Sprintf("step %d notes", i), s.Notes); err != nil {
				return nil, err
			}
			fields = append(fields, fitField{fitStepNotes, fitBaseString, s.Notes})
		}
		msgs = append(msgs, fitMsg{GlobalNum: fitMesgWorkoutStep, Fields: fields})
	}
	return fitEncode(msgs)
}

// fitASCIIField refuses what the format cannot carry: FIT strings are
// null-terminated bytes with no declared encoding, and printable ASCII is the
// only range every decoder and the watch agree on. For loaded data this is
// unreachable — fitName and the note pipeline transliterate first.
func fitASCIIField(what, s string) error {
	if s == "" {
		return fmt.Errorf("%s is empty", what)
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7E {
			return fmt.Errorf("%s contains %q — FIT strings are printable ASCII", what, r)
		}
	}
	return nil
}

/* ── the bridge from resolved steps ────────────────────────────────────── */

// fitWorkoutFor is the pure conversion from the loader's resolved steps to a
// FitWorkout: m→cm, s→ms, pace→mm/s, hr→bpm+100, repeats flattened body-first
// with the trailing repeat step. It validates nothing — the loader has
// already refused anything wrong — and it is total: any resolvedStep the
// loader emits converts.
func fitWorkoutFor(name string, steps []resolvedStep, sport uint8, serial uint32, created time.Time) FitWorkout {
	w := FitWorkout{Name: name, Sport: sport, Serial: serial, TimeCreated: created}
	// flattenSteps owns the emitted order and therefore the step numbering a
	// recorded lap refers back to; this loop only converts units.
	for _, e := range flattenSteps(steps) {
		if e.IsRepeat {
			w.Steps = append(w.Steps, FitStep{
				Intensity:     fitIntensities["active"],
				DurationType:  fitDurationRepeat,
				DurationValue: uint32(e.First), // message_index of the first child
				TargetType:    fitTargetOpen,
				TargetValue:   uint32(e.Times),
				CustomLow:     fitInvalidUint32,
				CustomHigh:    fitInvalidUint32,
			})
			continue
		}
		w.Steps = append(w.Steps, fitLeafStep(e.Leaf))
	}
	return w
}

func fitLeafStep(s resolvedStep) FitStep {
	st := FitStep{
		Intensity:  fitIntensities[s.Role],
		TargetType: fitTargetOpen,
		CustomLow:  fitInvalidUint32,
		CustomHigh: fitInvalidUint32,
		Notes:      s.Note,
	}
	if s.DistM > 0 {
		st.DurationType = fitDurationDistance
		st.DurationValue = uint32(math.Round(s.DistM * 100)) // scale 100: centimetres
	} else {
		st.DurationType = fitDurationTime
		st.DurationValue = uint32(s.Secs) * 1000 // scale 1000: milliseconds
	}
	switch {
	case s.PaceFast > 0:
		// The data is fast-first; FIT wants speeds, low first. Convert each
		// end then order by the converted values — conversion, not
		// validation: the loader separately rejects transposed bands so the
		// authoring convention holds, but this function stays total.
		a := math.Round(1000 / float64(s.PaceFast)) // s/m → mm/s
		b := math.Round(1000 / float64(s.PaceSlow))
		st.TargetType = fitTargetSpeed
		st.TargetValue = 0
		st.CustomLow = uint32(math.Min(a, b))
		st.CustomHigh = uint32(math.Max(a, b))
	case s.HRLo > 0:
		// Custom HR range: value > 100 means bpm + 100.
		st.TargetType = fitTargetHR
		st.TargetValue = 0
		st.CustomLow = uint32(s.HRLo) + 100
		st.CustomHigh = uint32(s.HRHi) + 100
	case s.PowerLo > 0:
		// Custom power range: values ≤ 1000 mean %FTP, so absolute watts are
		// offset by 1000. Absolute deliberately — the plan's measured FTP is
		// the source of truth, not whatever the watch's own setting says.
		st.TargetType = fitTargetPower
		st.TargetValue = 0
		st.CustomLow = uint32(s.PowerLo) + 1000
		st.CustomHigh = uint32(s.PowerHi) + 1000
	}
	return st
}

/* ── naming ────────────────────────────────────────────────────────────── */

// fitNamePrefixLen is how much of a workout name the watch reads when
// deduping imports: two files sharing their first 15 characters are one
// workout to it, and the second silently vanishes.
const fitNamePrefixLen = 15

// fitNameMax keeps the whole name inside what the watch list renders.
const fitNameMax = 24

// fitDayAbbrev is two characters so the "W06 Tu " prefix spends 7 of the
// 15-char dedupe window and leaves the label's head to distinguish the rest —
// and since (week, day) is unique within a block, the prefix alone already
// is.
var fitDayAbbrev = [7]string{"Mo", "Tu", "We", "Th", "Fr", "Sa", "Su"}

// fitName derives the on-watch workout name from a session. It is the single
// naming implementation, used by dryRun (validation) and the route (serving),
// so the name the loader checked is byte-for-byte the name the watch sees.
func fitName(weekN, dayIdx int, label string) (string, error) {
	if dayIdx < 0 || dayIdx > 6 {
		return "", fmt.Errorf("day index %d is not a weekday", dayIdx)
	}
	head, err := fitTransliterate(stripEmph(label))
	if err != nil {
		return "", fmt.Errorf("label %q: %w", label, err)
	}
	prefix := fmt.Sprintf("W%02d %s ", weekN, fitDayAbbrev[dayIdx])
	name := prefix + strings.TrimSpace(head)
	if len(name) > fitNameMax {
		cut := name[:fitNameMax]
		if i := strings.LastIndex(cut, " "); i >= len(prefix) {
			cut = cut[:i]
		}
		name = cut
	}
	return strings.TrimRight(name, " "), nil
}

// fitTranslit maps the typography the plan's labels actually use onto ASCII.
// An unmapped rune is an error rather than a dropped character: silent
// dropping would invisibly change the name the dedupe key is computed from,
// and a mystery glyph on the watch face is worse than a load failure.
var fitTranslit = map[rune]string{
	'×': "x", '—': "-", '–': "-", '·': " ", '₂': "2", '′': "'",
	'≤': "<=", '≥': ">=", 'ø': "o", 'Ø': "O", '\n': " ",
}

func fitTransliterate(s string) (string, error) {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r <= 0x7E {
			b.WriteRune(r)
			continue
		}
		t, ok := fitTranslit[r]
		if !ok {
			return "", fmt.Errorf("cannot transliterate %q to ASCII — add it to fitTranslit or reword", r)
		}
		b.WriteString(t)
	}
	return b.String(), nil
}

// fitSlug is the download filename: the name with shell-hostile characters
// gone. Spaces hyphenate, colons underscore, anything outside [A-Za-z0-9._-]
// drops, and hyphen runs collapse: "W06 Tu 4x5:00 @ T" → "W06-Tu-4x5_00-T.fit".
func fitSlug(name string) string { return fitSlugBase(name) + ".fit" }

// fitSlugBase is the slug without an extension, for the zip bundle's own
// filename as well as the entries inside it.
func fitSlugBase(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range name {
		switch r {
		case ' ':
			r = '-'
		case ':':
			r = '_'
		}
		ok := r == '.' || r == '_' || r == '-' ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			continue
		}
		if r == '-' {
			if dash || b.Len() == 0 {
				continue
			}
			dash = true
		} else {
			dash = false
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), "-")
}

// fitNamed is one steps-bearing day's derived name, kept with where it came
// from so a collision report says which two days to reword.
type fitNamed struct {
	Day  string // "week 6 day 2" — the loader's usual coordinates
	Name string
}

// checkFitNameCollisions enforces the first-15 rule per block. The week+day
// prefix makes a real collision structurally impossible, so this is a
// backstop for a future name scheme change — which is exactly why it stays.
func checkFitNameCollisions(names []fitNamed) error {
	seen := map[string]fitNamed{}
	for _, n := range names {
		k := n.Name
		if len(k) > fitNamePrefixLen {
			k = k[:fitNamePrefixLen]
		}
		if prev, ok := seen[k]; ok {
			return fmt.Errorf("workout names %q (%s) and %q (%s) share their first %d characters — the watch would silently dedupe them",
				prev.Name, prev.Day, n.Name, n.Day, fitNamePrefixLen)
		}
		seen[k] = n
	}
	return nil
}

/* ── identity ──────────────────────────────────────────────────────────── */

// fitIdentity derives (serial_number, time_created) from the data revision,
// the block and the day. Deterministic per revision — stable ETags, golden
// tests, A/B diffs — and distinct per day and per revision, so a re-imported
// edit is a new identity and a batch of files never dedupes into one.
// Accepted trade-off, recorded once: ANY data-file edit rotates every
// identity, because Rev hashes every loaded file. Conservative in the safe
// direction.
func fitIdentity(rev, blockID string, day time.Time) (serial uint32, created time.Time) {
	h := sha256.Sum256([]byte(rev + "\x00" + blockID + "\x00" + day.Format("2006-01-02")))
	// Branchless landing inside [1, 0xFFFFFFFE]: never the 0 or 0xFFFFFFFF
	// uint32z sentinels.
	serial = 1 + binary.LittleEndian.Uint32(h[0:4])%0xFFFFFFFE
	offset := binary.LittleEndian.Uint32(h[4:8]) % 86400
	created = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC).
		Add(time.Duration(offset) * time.Second)
	return serial, created
}
