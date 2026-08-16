package main

// A recorded activity's FIT bytes, decoded into the streams the metrics
// register consumes. This file is the CONVERSION half of the measurement
// register — the authoritative statement of how bytes become streams; the
// measurement half (time-weighting, the dropout rule, drift, decoupling)
// lives on metrics.go. tools/fit_streams.py is the cross-validation mirror
// of this file and changes in step with it or not at all.
//
// Conversion conventions — change them here or not at all:
//
//   - time[i] is whole seconds from the first record's timestamp. Recording
//     gaps (auto-pause) are left in, deliberately; Strava compacts them out,
//     so a delta against Strava streams for the same activity is expected.
//   - heart_rate is bpm from the record, present only when some record
//     carries a valid sample; within the stream a missing or invalid sample
//     (255) becomes 0, which the dropout rule excludes and reports.
//   - watts comes from record power, present only when some record carries
//     power; a missing sample within a power stream becomes 0.
//   - velocity is enhanced_speed falling back to speed, m/s. The EXPLICIT
//     wire field always wins over anything component expansion produced.
//     Two measured mechanisms (14 Aug 2026, full archive) make that rule
//     load-bearing: muktihari SYNTHESIZES enhanced_speed from an explicit
//     speed field with a float truncation that loses an ulp (raw 2006 →
//     synthesized 2005; 328 pre-enhanced-speed files affected), and
//     compressed_speed_distance expansion REPLACES an explicit speed
//     field's value in place with the flag left unset. Both are recovered
//     by re-reading any file that carries an explicit speed field with
//     expansion disabled; expansion remains the fallback for records with
//     no explicit speed at all.
//   - cadence is exactly as recorded, never pre-doubled — run records are
//     per-leg and the presentation doubling belongs to the register's output
//     side.
//   - CRC is enforced, with one measured exception: Zwift writes the
//     trailing CRC over header+data while leaving the header's own CRC slot
//     zero (125 of 1,366 archive files and every future Zwift ride);
//     muktihari checks the data-only convention and refuses those. The
//     whole-file convention is verified here first, and only then is the
//     decoder's checksum skipped. A file failing BOTH conventions is a
//     refusal, exactly as fitdecode's CrcCheck.RAISE refuses it.
//   - Developer fields are ignored. A chained file (several FIT parts back
//     to back) is walked in full; session distances sum.
//
// The archive's FIT bytes are canonical and immutable; everything this file
// produces is derived and rebuildable.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/profile/basetype"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/untyped/fieldnum"
	"github.com/muktihari/fit/profile/untyped/mesgnum"
)

// activityStreams is one decoded activity, in the register's canonical
// units: seconds, bpm, watts, m/s, rpm-as-recorded.
type activityStreams struct {
	Time  []int
	HR    []int // 0 = missing; the dropout rule owns exclusion
	Watts []int
	Vel   []float64
	Cad   []int

	HaveHR, HaveWatts, HaveVel, HaveCad bool

	Sport string // "running", "cycling", or whatever the file declares
	// SubSport is how it was done — "virtual_activity", "indoor_cycling",
	// "treadmill". It decides whether the weather outside meant anything.
	SubSport string
	StartUTC time.Time
	DistM    *float64 // session total, else the last record's odometer

	// StartLat/StartLon are the first fixed position, in degrees. The
	// REGISTER's only use is asking what the weather was there: rounded to a
	// tenth of a degree, never stored at full precision, never served from
	// here. A trainer ride has none.
	//
	// The wider promise that once sat here — that this app never serves a
	// position — was retired on 15 Aug 2026, deliberately and by the
	// athlete's decision: detail.go serves the full track to the popover, and
	// the map draws it. What replaced the promise is the deployment: an
	// identity-aware tunnel in front, since the app itself authenticates
	// nobody and the README now says so plainly. The register's own handling
	// is unchanged — coarse, and only for the weather.
	StartLat, StartLon *float64
}

// semicirclesToDegrees converts FIT's position unit — 2^31 semicircles to
// 180 degrees.
func semicirclesToDegrees(v int32) float64 {
	return float64(v) * (180.0 / 2147483648.0)
}

// wholeFileFITCRCOK walks the chained FIT parts and accepts a part when its
// trailing CRC matches EITHER the data-only convention (what muktihari
// checks) or the whole-part convention including the header (what Zwift
// writes when the header's own CRC field is zero).
func wholeFileFITCRCOK(data []byte) bool {
	o := 0
	for o < len(data) {
		if len(data)-o < 14 {
			return false
		}
		hs := int(data[o])
		dsize := int(uint32(data[o+4]) | uint32(data[o+5])<<8 | uint32(data[o+6])<<16 | uint32(data[o+7])<<24)
		end := o + hs + dsize + 2
		if end > len(data) || end <= o {
			return false
		}
		trailing := uint16(data[end-2]) | uint16(data[end-1])<<8
		// fitCRC16 is the encoder's, in fit.go — one CRC implementation.
		if fitCRC16(0, data[o:end-2]) != trailing && fitCRC16(0, data[o+hs:end-2]) != trailing {
			return false
		}
		o = end
	}
	return true
}

// decodeActivity turns stored FIT bytes into streams. The CRC fallback and
// the explicit-speed precedence are the two measured warts documented above;
// everything else is a plain walk of the record messages.
func decodeActivity(data []byte) (*activityStreams, error) {
	s, needsExplicit, err := decodePass(data, true)
	if err != nil {
		return nil, err
	}
	if needsExplicit {
		// A second, expansion-free pass recovers the explicit speed values
		// that expansion shadowed (the synthesized enhanced_speed) or
		// overwrote in place (compressed_speed_distance). Only files
		// carrying an explicit speed field pay for it — the watch's own
		// recordings write enhanced_speed only and stay single-pass. The
		// record sequence is identical by construction, so the overrides
		// align by index.
		explicit, _, err := decodePass(data, false)
		if err != nil {
			return nil, fmt.Errorf("expansion-free pass: %w", err)
		}
		if len(explicit.Vel) != len(s.Vel) {
			return nil, fmt.Errorf("expansion-free pass decoded %d records, first pass %d",
				len(explicit.Vel), len(s.Vel))
		}
		for i, v := range explicit.Vel {
			if explicit.velExplicit[i] {
				s.Vel[i] = v
			}
		}
		if explicit.HaveVel {
			s.HaveVel = true
		}
	}
	return s.activityStreams(), nil
}

// decodedActivity carries the streams plus per-record bookkeeping only the
// two-pass merge needs.
type decodedActivity struct {
	activityStreamsData activityStreams
	velExplicit         []bool // this record carried an explicit (enhanced_)speed field
	Vel                 []float64
	HaveVel             bool
}

func (d *decodedActivity) activityStreams() *activityStreams {
	out := d.activityStreamsData
	out.Vel = d.Vel
	out.HaveVel = d.HaveVel
	if !out.HaveHR {
		out.HR = nil
	}
	if !out.HaveWatts {
		out.Watts = nil
	}
	if !out.HaveVel {
		out.Vel = nil
	}
	if !out.HaveCad {
		out.Cad = nil
	}
	return &out
}

// decodePass decodes once, reporting alongside the streams whether any
// record carried an explicit speed field that expansion could have shadowed
// — the signal that the expansion-free pass is needed.
func decodePass(data []byte, expand bool) (*decodedActivity, bool, error) {
	s, needsExplicit, err := decodePassOpt(data, expand, false)
	if err != nil && strings.Contains(err.Error(), "crc checksum mismatch") {
		// muktihari checks the data-only CRC convention; Zwift writes the
		// whole-file one. Verify that convention ourselves, then trust it.
		if wholeFileFITCRCOK(data) {
			return decodePassOpt(data, expand, true)
		}
	}
	return s, needsExplicit, err
}

func decodePassOpt(data []byte, expand, skipCRC bool) (*decodedActivity, bool, error) {
	opts := []decoder.Option{}
	if !expand {
		opts = append(opts, decoder.WithNoComponentExpansion())
	}
	if skipCRC {
		opts = append(opts, decoder.WithIgnoreChecksum())
	}
	dec := decoder.New(bytes.NewReader(data), opts...)

	type rec struct {
		r        *mesgdef.Record
		explicit bool // carried an explicit (non-expanded) speed field
	}
	var records []rec
	needsExplicit := false
	d := &decodedActivity{}
	d.activityStreamsData.Sport = "unknown"

	for dec.Next() {
		fit, err := dec.Decode()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, false, err
		}
		for i := range fit.Messages {
			m := &fit.Messages[i]
			switch m.Num {
			case mesgnum.Record:
				r := mesgdef.NewRecord(m)
				if r.Timestamp.IsZero() {
					continue
				}
				explicit := false
				for j := range m.Fields {
					f := &m.Fields[j]
					switch f.Num {
					case fieldnum.RecordCompressedSpeedDistance:
						// Raw compressed field: expansion may have replaced
						// an explicit speed's value in place, flag unset.
						if !f.IsExpandedField {
							needsExplicit = true
						}
					case fieldnum.RecordSpeed:
						// An explicit speed field is exactly what synthesis
						// shadows (and in-place replacement corrupts) — the
						// expansion-free pass must have the final word.
						if !f.IsExpandedField {
							needsExplicit = true
							explicit = true
						}
					case fieldnum.RecordEnhancedSpeed:
						if !f.IsExpandedField {
							explicit = true
						}
					}
				}
				records = append(records, rec{r, explicit})
			case mesgnum.Session:
				ses := mesgdef.NewSession(m)
				if d.activityStreamsData.Sport == "unknown" {
					d.activityStreamsData.Sport = ses.Sport.String()
				}
				if d.activityStreamsData.SubSport == "" {
					d.activityStreamsData.SubSport = ses.SubSport.String()
				}
				if ses.TotalDistance != basetype.Uint32Invalid {
					dist := float64(ses.TotalDistance) / 100
					if d.activityStreamsData.DistM == nil {
						d.activityStreamsData.DistM = &dist
					} else {
						*d.activityStreamsData.DistM += dist // chained files: sum sessions
					}
				}
			case mesgnum.Sport:
				sp := mesgdef.NewSport(m)
				if d.activityStreamsData.Sport == "unknown" {
					d.activityStreamsData.Sport = sp.Sport.String()
				}
				if d.activityStreamsData.SubSport == "" {
					d.activityStreamsData.SubSport = sp.SubSport.String()
				}
			}
		}
	}
	if len(records) == 0 {
		return nil, false, fmt.Errorf("no record messages with a timestamp")
	}

	ts0 := records[0].r.Timestamp
	d.activityStreamsData.StartUTC = ts0.UTC()
	for _, rr := range records {
		r := rr.r
		d.activityStreamsData.Time = append(d.activityStreamsData.Time,
			int(math.Round(r.Timestamp.Sub(ts0).Seconds())))

		h := 0
		if r.HeartRate != basetype.Uint8Invalid {
			h = int(r.HeartRate)
			d.activityStreamsData.HaveHR = true
		}
		d.activityStreamsData.HR = append(d.activityStreamsData.HR, h)

		w := 0
		if r.Power != basetype.Uint16Invalid {
			w = int(r.Power)
			d.activityStreamsData.HaveWatts = true
		}
		d.activityStreamsData.Watts = append(d.activityStreamsData.Watts, w)

		v := 0.0
		haveThis := false
		if r.EnhancedSpeed != basetype.Uint32Invalid {
			v = float64(r.EnhancedSpeed) / 1000
			haveThis = true
		} else if r.Speed != basetype.Uint16Invalid {
			v = float64(r.Speed) / 1000
			haveThis = true
		}
		if haveThis {
			d.HaveVel = true
		}
		d.Vel = append(d.Vel, v)
		d.velExplicit = append(d.velExplicit, rr.explicit && haveThis)

		c := 0
		if r.Cadence != basetype.Uint8Invalid {
			c = int(r.Cadence)
			d.activityStreamsData.HaveCad = true
		}
		d.activityStreamsData.Cad = append(d.activityStreamsData.Cad, c)
	}
	// Where it started, for the weather lookup and nothing else. A virtual
	// ride records a position too — Zwift writes its own world's, and
	// Watopia sits in the Solomon Islands — so indoors is checked before
	// anyone asks what the weather was there.
	for _, rr := range records {
		if rr.r.PositionLat != basetype.Sint32Invalid && rr.r.PositionLong != basetype.Sint32Invalid {
			lat := semicirclesToDegrees(rr.r.PositionLat)
			lon := semicirclesToDegrees(rr.r.PositionLong)
			d.activityStreamsData.StartLat, d.activityStreamsData.StartLon = &lat, &lon
			break
		}
	}
	if d.activityStreamsData.DistM == nil { // no session distance: last record's odometer
		for i := len(records) - 1; i >= 0; i-- {
			if records[i].r.Distance != basetype.Uint32Invalid {
				dist := float64(records[i].r.Distance) / 100
				d.activityStreamsData.DistM = &dist
				break
			}
		}
	}
	return d, needsExplicit, nil
}
