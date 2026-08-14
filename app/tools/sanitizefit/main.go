// Command sanitizefit rewrites a recorded activity into the smallest file
// that still measures the same, so a real session can be committed as a
// test fixture.
//
//	go run ./tools/sanitizefit -in recorded.fit -out testdata/cases/x.fit
//
// It keeps exactly what the measurement register reads — the record
// timestamps, heart rate, cadence, power and speed, plus the sport and
// total distance — and drops everything else on the floor. Gone: every
// position_lat/position_long in the file, device serial numbers and
// product ids, the user profile (weight, age, sex, zones), laps, events,
// and any developer fields. What survives says where the athlete's heart
// rate was, not where the athlete was.
//
// Timestamps are kept: a date is not a location, and the fixtures are only
// meaningful against the day they are graded as.
//
// Speed is written as enhanced_speed carrying the value the app's own
// decoder would have read, explicit field first — the same precedence
// decode.go applies — so the streams, and therefore every metric derived
// from them, come out identical to the original's.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/basetype"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/profile/untyped/fieldnum"
	"github.com/muktihari/fit/profile/untyped/mesgnum"
	"github.com/muktihari/fit/proto"
)

func main() {
	in := flag.String("in", "", "recorded activity to read")
	out := flag.String("out", "", "sanitized fixture to write")
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: sanitizefit -in recorded.fit -out fixture.fit")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		fail(err)
	}
	msgs, summary, err := sanitize(raw)
	if err != nil {
		fail(err)
	}
	f, err := os.Create(*out)
	if err != nil {
		fail(err)
	}
	if err := encoder.New(f).Encode(&proto.FIT{Messages: msgs}); err != nil {
		f.Close()
		fail(err)
	}
	if err := f.Close(); err != nil {
		fail(err)
	}
	fi, _ := os.Stat(*out)
	fmt.Printf("%s: %s (%d records, %d bytes)\n", *out, summary, len(msgs)-2, fi.Size())
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "sanitizefit:", err)
	os.Exit(1)
}

// sanitize decodes the activity and rebuilds it from the fields the
// register reads. Decoding accepts the Zwift whole-file CRC convention the
// same way the app does, by retrying with the check off — a fixture is only
// as useful as the awkward files it can be made from.
func sanitize(raw []byte) ([]proto.Message, string, error) {
	fits, err := decodeAll(raw, false)
	if err != nil && strings.Contains(err.Error(), "crc checksum mismatch") {
		fits, err = decodeAll(raw, true)
	}
	if err != nil {
		return nil, "", err
	}

	sport := typedef.SportGeneric
	var records []*mesgdef.Record
	var explicit []bool
	var totalDistCM uint32
	for _, fit := range fits {
		for i := range fit.Messages {
			m := &fit.Messages[i]
			switch m.Num {
			case mesgnum.Record:
				r := mesgdef.NewRecord(m)
				if r.Timestamp.IsZero() {
					continue
				}
				records = append(records, r)
				ex := false
				for j := range m.Fields {
					if m.Fields[j].Num == fieldnum.RecordSpeed && !m.Fields[j].IsExpandedField {
						ex = true
					}
				}
				explicit = append(explicit, ex)
			case mesgnum.Session:
				s := mesgdef.NewSession(m)
				if sport == typedef.SportGeneric {
					sport = s.Sport
				}
				if s.TotalDistance != basetype.Uint32Invalid {
					totalDistCM += s.TotalDistance
				}
			case mesgnum.Sport:
				if sport == typedef.SportGeneric {
					sport = mesgdef.NewSport(m).Sport
				}
			}
		}
	}
	if len(records) == 0 {
		return nil, "", errors.New("no record messages with a timestamp")
	}

	// The explicit-field precedence decode.go applies, so the sanitized
	// stream is the one the app would have measured.
	unexpanded, err := decodeAll(raw, true, decoder.WithNoComponentExpansion())
	if err != nil {
		return nil, "", fmt.Errorf("expansion-free pass: %w", err)
	}
	var plain []*mesgdef.Record
	for _, fit := range unexpanded {
		for i := range fit.Messages {
			if fit.Messages[i].Num != mesgnum.Record {
				continue
			}
			r := mesgdef.NewRecord(&fit.Messages[i])
			if !r.Timestamp.IsZero() {
				plain = append(plain, r)
			}
		}
	}
	if len(plain) != len(records) {
		return nil, "", fmt.Errorf("passes disagree: %d vs %d records", len(plain), len(records))
	}

	t0 := records[0].Timestamp
	msgs := []proto.Message{
		mesgdef.NewFileId(nil).
			SetType(typedef.FileActivity).
			SetManufacturer(typedef.ManufacturerDevelopment).
			SetTimeCreated(t0).ToMesg(nil),
	}
	kept := 0
	for i, r := range records {
		n := mesgdef.NewRecord(nil).SetTimestamp(r.Timestamp)
		if r.HeartRate != basetype.Uint8Invalid {
			n.SetHeartRate(r.HeartRate)
		}
		if r.Cadence != basetype.Uint8Invalid {
			n.SetCadence(r.Cadence)
		}
		if r.Power != basetype.Uint16Invalid {
			n.SetPower(r.Power)
		}
		if v, ok := speedOf(r, plain[i], explicit[i]); ok {
			n.SetEnhancedSpeed(v)
		}
		msgs = append(msgs, n.ToMesg(nil))
		kept++
	}
	ses := mesgdef.NewSession(nil).
		SetSport(sport).
		SetStartTime(t0).
		SetTimestamp(records[len(records)-1].Timestamp)
	if totalDistCM > 0 {
		ses.SetTotalDistance(totalDistCM)
	}
	msgs = append(msgs, ses.ToMesg(nil))

	return msgs, fmt.Sprintf("%s, %d records, %.2f km", sport.String(), kept,
		float64(totalDistCM)/100/1000), nil
}

// speedOf is decode.go's precedence in miniature: an explicit speed field
// beats anything component expansion synthesized for the same record.
func speedOf(expanded, plain *mesgdef.Record, hasExplicit bool) (uint32, bool) {
	if hasExplicit && plain.Speed != basetype.Uint16Invalid {
		return uint32(plain.Speed), true
	}
	if plain.EnhancedSpeed != basetype.Uint32Invalid {
		return plain.EnhancedSpeed, true
	}
	if expanded.EnhancedSpeed != basetype.Uint32Invalid {
		return expanded.EnhancedSpeed, true
	}
	if expanded.Speed != basetype.Uint16Invalid {
		return uint32(expanded.Speed), true
	}
	return 0, false
}

func decodeAll(raw []byte, skipCRC bool, extra ...decoder.Option) ([]*proto.FIT, error) {
	opts := extra
	if skipCRC {
		opts = append(opts, decoder.WithIgnoreChecksum())
	}
	dec := decoder.New(strings.NewReader(string(raw)), opts...)
	var out []*proto.FIT
	for dec.Next() {
		fit, err := dec.Decode()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		out = append(out, fit)
	}
	return out, nil
}
